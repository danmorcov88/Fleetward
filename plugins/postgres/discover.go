package postgres

import (
	"context"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// queryer is the subset of pgx.Conn this package needs, so the query logic can be exercised without
// a live connection.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Discover reports topology, version, and databases for one instance.
func (p *Plugin) Discover(ctx context.Context, req *fwv1.DiscoverRequest) (*fwv1.DiscoverResponse, error) {
	conn, err := connect(ctx, req.GetCredentials())
	if err != nil {
		// Unlike HealthCheck, an unreachable instance here is a genuine RPC failure: the caller
		// asked for an inventory and there is none to give.
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	server, err := discoverServer(ctx, conn)
	if err != nil {
		return nil, err
	}

	resp := &fwv1.DiscoverResponse{Server: server}

	if !req.GetSkipDatabaseDetails() {
		databases, err := discoverDatabases(ctx, conn)
		if err != nil {
			return nil, err
		}
		resp.Databases = databases
	}

	topology, err := discoverTopology(ctx, conn, req.GetCredentials())
	if err != nil {
		return nil, err
	}
	resp.Topology = topology

	return resp, nil
}

// discoverServer collects instance-level facts.
func discoverServer(ctx context.Context, conn queryer) (*fwv1.ServerInfo, error) {
	var (
		versionRaw    string
		serverVersion string
		uptimeSeconds float64
		inRecovery    bool
	)

	err := conn.QueryRow(ctx, `
		SELECT version(),
		       current_setting('server_version'),
		       EXTRACT(EPOCH FROM (now() - pg_postmaster_start_time())),
		       pg_is_in_recovery()`).
		Scan(&versionRaw, &serverVersion, &uptimeSeconds, &inRecovery)
	if err != nil {
		return nil, sdk.ConnectionFailed("read server information").WithCause(err)
	}

	info := &fwv1.ServerInfo{
		EngineType:    EngineType,
		Version:       normalizeVersion(serverVersion),
		VersionString: versionRaw,
		Uptime:        durationpb.New(time.Duration(uptimeSeconds * float64(time.Second))),
		// A standby accepts no writes. Core needs this to know a backup taken here is of a replica,
		// and to keep the UI from offering actions that will fail.
		ReadOnly:   inRecovery,
		Attributes: map[string]string{},
	}

	// data_directory requires superuser or pg_read_all_settings. A monitoring account
	// deliberately holding neither is good practice, so its absence is not an error.
	var dataDirectory string
	if err := conn.QueryRow(ctx, `SELECT current_setting('data_directory')`).Scan(&dataDirectory); err == nil {
		info.DataDirectory = dataDirectory
	}

	var clusterName string
	if err := conn.QueryRow(ctx, `SELECT current_setting('cluster_name')`).Scan(&clusterName); err == nil {
		info.ClusterName = clusterName
	}

	// Settings worth surfacing on an instance detail screen without a separate GetConfig round trip.
	for _, setting := range []string{"max_connections", "shared_buffers", "wal_level", "archive_mode"} {
		var value string
		if err := conn.QueryRow(ctx, `SELECT current_setting($1)`, setting).Scan(&value); err == nil {
			info.Attributes[setting] = value
		}
	}

	return info, nil
}

// discoverDatabases enumerates the databases on the instance.
func discoverDatabases(ctx context.Context, conn queryer) ([]*fwv1.DatabaseInfo, error) {
	// datallowconn excludes template0, which cannot be connected to at all. pg_database_size is
	// wrapped in a CASE because it errors on a database the current role cannot access, and one
	// inaccessible database must not fail the whole inventory.
	rows, err := conn.Query(ctx, `
		SELECT d.datname,
		       CASE WHEN has_database_privilege(d.datname, 'CONNECT')
		            THEN pg_database_size(d.datname) ELSE 0 END AS size_bytes,
		       pg_catalog.pg_get_userbyid(d.datdba) AS owner,
		       pg_encoding_to_char(d.encoding) AS encoding,
		       d.datcollate,
		       d.datistemplate OR d.datname IN ('postgres', 'template0', 'template1') AS is_system
		FROM pg_database d
		WHERE d.datallowconn
		ORDER BY d.datname`)
	if err != nil {
		return nil, sdk.ConnectionFailed("list databases").WithCause(err)
	}
	defer rows.Close()

	var databases []*fwv1.DatabaseInfo
	for rows.Next() {
		var db fwv1.DatabaseInfo
		if err := rows.Scan(&db.Name, &db.SizeBytes, &db.Owner, &db.Encoding, &db.Collation, &db.IsSystem); err != nil {
			return nil, sdk.Internal("scan database row: %v", err)
		}
		databases = append(databases, &db)
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.ConnectionFailed("read database list").WithCause(err)
	}

	return databases, nil
}

// discoverTopology reports the replication layout as seen from this instance.
//
// PostgreSQL has no cluster-wide view: a primary knows its connected standbys, and a standby knows
// its upstream. What is reported is therefore what this node can see, which is why every node
// carries is_self and the caller stitches the estate together.
func discoverTopology(ctx context.Context, conn queryer, creds *fwv1.Credentials) (*fwv1.Topology, error) {
	var inRecovery bool
	if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return nil, sdk.ConnectionFailed("read recovery state").WithCause(err)
	}

	self := &fwv1.Node{
		Id:     creds.GetHost(),
		Host:   creds.GetHost(),
		Port:   portOrDefault(creds.GetPort()),
		IsSelf: true,
		Role:   fwv1.NodeRole_NODE_ROLE_PRIMARY,
		State:  "streaming",
	}
	if inRecovery {
		self.Role = fwv1.NodeRole_NODE_ROLE_REPLICA
		self.State = "in recovery"
		if lag := replayLagSeconds(ctx, conn); lag != nil {
			self.ReplicationLagSeconds = *lag
		}
	}

	topology := &fwv1.Topology{Nodes: []*fwv1.Node{self}}

	if inRecovery {
		// A standby cannot enumerate its siblings; reporting only itself is the honest answer.
		return topology, nil
	}

	// pg_stat_replication requires pg_read_all_stats or superuser. Without it we still return the
	// node itself rather than failing discovery.
	rows, err := conn.Query(ctx, `
		SELECT client_addr::text,
		       COALESCE(application_name, ''),
		       COALESCE(state, ''),
		       COALESCE(EXTRACT(EPOCH FROM replay_lag), 0)
		FROM pg_stat_replication`)
	if err != nil {
		topology.IsStandalone = true
		return topology, nil //nolint:nilerr // insufficient privilege is not a discovery failure
	}
	defer rows.Close()

	for rows.Next() {
		var clientAddr *string
		var applicationName, state string
		var lagSeconds float64
		if err := rows.Scan(&clientAddr, &applicationName, &state, &lagSeconds); err != nil {
			return nil, sdk.Internal("scan replication row: %v", err)
		}

		host := ""
		if clientAddr != nil {
			host = *clientAddr
		}
		id := host
		if applicationName != "" {
			id = applicationName
		}

		topology.Nodes = append(topology.Nodes, &fwv1.Node{
			Id:                    id,
			Host:                  host,
			Role:                  fwv1.NodeRole_NODE_ROLE_REPLICA,
			State:                 state,
			ReplicationLagSeconds: lagSeconds,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.ConnectionFailed("read replication state").WithCause(err)
	}

	topology.IsStandalone = len(topology.Nodes) == 1
	return topology, nil
}

// replayLagSeconds returns how far a standby is behind, or nil when it cannot be determined.
func replayLagSeconds(ctx context.Context, conn queryer) *float64 {
	var lag *float64
	err := conn.QueryRow(ctx, `
		SELECT CASE
		         WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
		         ELSE EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
		       END`).Scan(&lag)
	if err != nil {
		return nil
	}
	return lag
}

// versionPattern matches the leading numeric part of a PostgreSQL version string.
var versionPattern = regexp.MustCompile(`^(\d+)(?:\.(\d+))?`)

// normalizeVersion reduces a reported version to a comparable form.
//
// PostgreSQL reports things like "16.2", "16.2 (Debian 16.2-1.pgdg120+2)", and "17beta1". Core
// compares versions to choose a sandbox image tag, so it needs the numeric part and nothing else.
func normalizeVersion(raw string) string {
	match := versionPattern.FindStringSubmatch(raw)
	if match == nil {
		return raw
	}
	if match[2] == "" {
		return match[1]
	}
	return match[1] + "." + match[2]
}
