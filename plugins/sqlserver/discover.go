package sqlserver

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// discoverTimeout bounds one Discover call. Per-database sizes are a scan of catalog views rather
// than of data, so this is generous enough for a large estate and still short enough that a hung
// instance is reported rather than waited on.
const discoverTimeout = 60 * time.Second

// listDatabasesSQL enumerates the databases a backup could target.
//
// System databases are reported but flagged, because an operator wants to see that master and msdb
// exist without a backup schedule ever defaulting to them. tempdb is excluded outright: it cannot
// be backed up at all, so listing it as a candidate would be an offer SQL Server refuses.
const listDatabasesSQL = `
	SELECT d.name,
	       d.database_id,
	       ISNULL(SUSER_SNAME(d.owner_sid), ''),
	       d.collation_name,
	       d.state_desc,
	       d.recovery_model_desc,
	       ISNULL((SELECT SUM(CAST(mf.size AS bigint)) * 8192
	               FROM sys.master_files mf
	               WHERE mf.database_id = d.database_id), 0)
	FROM sys.databases d
	WHERE d.name <> 'tempdb'
	ORDER BY d.name`

// Discover reports the instance's identity and the databases on it.
func (p *Plugin) Discover(ctx context.Context, req *fwv1.DiscoverRequest) (*fwv1.DiscoverResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	db, err := open(req.GetCredentials(), masterDatabase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	if err := ping(ctx, db); err != nil {
		return nil, err
	}

	server, err := describeServer(ctx, db)
	if err != nil {
		return nil, err
	}

	databases, err := listDatabases(ctx, db, req.GetSkipDatabaseDetails())
	if err != nil {
		return nil, err
	}

	return &fwv1.DiscoverResponse{
		Server:    server,
		Databases: databases,
		// Availability groups and failover clusters are out of scope for this slice, and a plugin
		// that guessed at a topology it has not inspected would be declaring one it cannot support.
		Topology: &fwv1.Topology{
			IsStandalone: true,
			Nodes: []*fwv1.Node{{
				Host:   req.GetCredentials().GetHost(),
				Port:   req.GetCredentials().GetPort(),
				Role:   fwv1.NodeRole_NODE_ROLE_STANDALONE,
				State:  "online",
				IsSelf: true,
			}},
		},
	}, nil
}

// describeServer reads the instance's own identity.
func describeServer(ctx context.Context, db *sql.DB) (*fwv1.ServerInfo, error) {
	var (
		version     string
		versionText string
		edition     string
		serverName  string
		dataPath    sql.NullString
		startedAt   time.Time
		readOnly    string
	)
	err := db.QueryRowContext(ctx, `
		SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)),
		       @@VERSION,
		       CAST(SERVERPROPERTY('Edition') AS nvarchar(128)),
		       CAST(SERVERPROPERTY('ServerName') AS nvarchar(128)),
		       CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512)),
		       (SELECT sqlserver_start_time FROM sys.dm_os_sys_info),
		       CAST(DATABASEPROPERTYEX(DB_NAME(), 'Updateability') AS nvarchar(64))`).
		Scan(&version, &versionText, &edition, &serverName, &dataPath, &startedAt, &readOnly)
	if err != nil {
		return nil, sdk.Internal("read the instance's version").WithCause(classifyConnError(err))
	}

	return &fwv1.ServerInfo{
		EngineType: EngineType,
		// SERVERPROPERTY('ProductVersion') is already major.minor.build.revision, which is close
		// enough to semver for core's purposes and is what the sandbox tag is resolved against.
		Version:       version,
		VersionString: strings.Join(strings.Fields(versionText), " "),
		Uptime:        durationpb.New(time.Since(startedAt)),
		ClusterName:   serverName,
		DataDirectory: dataPath.String,
		ReadOnly:      !strings.EqualFold(readOnly, "READ_WRITE"),
		Attributes: map[string]string{
			"edition": edition,
		},
	}, nil
}

// listDatabases enumerates what is on the instance.
func listDatabases(ctx context.Context, db *sql.DB, skipDetails bool) ([]*fwv1.DatabaseInfo, error) {
	rows, err := db.QueryContext(ctx, listDatabasesSQL)
	if err != nil {
		return nil, sdk.Internal("list databases").WithCause(classifyConnError(err))
	}
	defer func() { _ = rows.Close() }()

	var databases []*fwv1.DatabaseInfo
	for rows.Next() {
		var (
			name      string
			id        int64
			owner     string
			collation sql.NullString
			state     string
			recovery  string
			sizeBytes int64
		)
		if err := rows.Scan(&name, &id, &owner, &collation, &state, &recovery, &sizeBytes); err != nil {
			return nil, sdk.Internal("read a database row").WithCause(err)
		}

		info := &fwv1.DatabaseInfo{
			Name:      name,
			SizeBytes: sizeBytes,
			Owner:     owner,
			Collation: collation.String,
			// database_id 1 to 4 are master, tempdb, model, and msdb. tempdb is already excluded;
			// the rest are shown and de-emphasized rather than hidden, because an operator wants to
			// know msdb exists and a schedule must never default to it.
			IsSystem: id <= 4,
		}
		databases = append(databases, info)
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.Internal("list databases").WithCause(err)
	}

	if !skipDetails {
		countObjects(ctx, db, databases)
	}
	return databases, nil
}

// countObjects fills in each database's table count.
//
// It is best effort by design: a database that is offline, restoring, or in single-user mode cannot
// be inspected, and that is a normal state rather than a failure of discovery. The database is
// still reported, with a count of zero, because knowing it exists and cannot be read is more useful
// than not knowing it exists.
func countObjects(ctx context.Context, db *sql.DB, databases []*fwv1.DatabaseInfo) {
	for _, info := range databases {
		if info.GetIsSystem() {
			continue
		}
		var count int64
		// The identifier comes from sys.databases rather than from the request, and is quoted the
		// way QUOTENAME quotes it, so it cannot carry an injection.
		query := "SELECT COUNT(*) FROM " + quoteIdentifier(info.GetName()) + ".sys.tables WHERE is_ms_shipped = 0"
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			continue
		}
		info.ObjectCount = count
	}
}
