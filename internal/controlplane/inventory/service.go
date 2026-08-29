// Package inventory owns the estate: environments, instances, and the connections that reach them.
//
// This is the path from a user's command to a database and back — store an instance, store its
// credentials safely, resolve them, call the plugin, return the answer. Everything downstream
// (backup, verification, compliance) sits on it.
//
// Three rules run through every function here:
//
//   - Every query filters on tenant_id. The MVP runs single-tenant against the seeded default
//     tenant, but retrofitting tenancy later would mean auditing every query in the project
//     (ADR-0008).
//   - Credentials are write-only. They enter through CreateInstance, leave only as materialized
//     Credentials for the duration of a single plugin call, and no read path has a field that could
//     return them (ADR-0009).
//   - Nothing branches on an engine name. Engine type is a routing key handed to the plugin
//     manager; behaviour comes from the plugin's declared capabilities.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
)

// Sentinel errors. Callers classify with errors.Is; the gRPC layer maps them to status codes and is
// the only place that decides what a client sees.
var (
	// ErrNotFound reports that no such row exists in this tenant.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists reports a name collision within its uniqueness scope.
	ErrAlreadyExists = errors.New("already exists")
	// ErrInvalidArgument reports a malformed request.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrEngineUnavailable reports that no plugin currently serves an engine type.
	ErrEngineUnavailable = errors.New("engine unavailable")
	// ErrPluginFailed reports a failure returned by a plugin. Its message is safe to show a client:
	// the contract forbids plugins from putting credentials in one.
	ErrPluginFailed = errors.New("plugin call failed")
)

const (
	// defaultPageSize applies when a caller does not ask for one.
	defaultPageSize = 50
	// maxPageSize bounds what a caller may ask for, so one request cannot pull a whole estate.
	maxPageSize = 500
	// defaultProbeTimeout bounds a health probe. Monitoring an estate means probing unreachable
	// hosts routinely, and a probe that hangs is worse than one that fails.
	defaultProbeTimeout = 10 * time.Second
	// probeGrace is how much longer the RPC gets than the probe deadline handed to the plugin, so a
	// plugin that honours its timeout is always the one that decides the outcome.
	probeGrace = 5 * time.Second
	// discoverTimeout bounds discovery, which enumerates databases and is heavier than a probe.
	discoverTimeout = 60 * time.Second
	// secretNamePrefix namespaces a connection's credentials inside the SecretsProvider.
	secretNamePrefix = "connection/"
)

// Router is the slice of the plugin manager this service needs. Depending on an interface rather
// than on *manager.Manager is what lets the unit tests run without launching plugin processes.
type Router interface {
	// Client returns the gRPC client for an engine type together with its capabilities.
	Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error)
	// Capabilities returns a plugin's capability matrix even when it is not currently ready.
	Capabilities(engineType string) (*fwv1.Capabilities, error)
	// EngineTypes lists the engine types this control plane can serve.
	EngineTypes() []string
}

// Service is the inventory domain service.
type Service struct {
	pool     *pgxpool.Pool
	secrets  secrets.Provider
	plugins  Router
	log      *slog.Logger
	tenantID string
}

// New builds the service. The tenant is fixed for now; when OIDC lands in Phase F it comes from the
// authenticated principal instead, and every query below is already written to accept it.
func New(pool *pgxpool.Pool, provider secrets.Provider, plugins Router, log *slog.Logger) *Service {
	return &Service{
		pool:     pool,
		secrets:  provider,
		plugins:  plugins,
		log:      log.With(slog.String("component", "inventory")),
		tenantID: metadb.DefaultTenantID,
	}
}

// -----------------------------------------------------------------------------------------------
// Inputs
// -----------------------------------------------------------------------------------------------

// CreateEnvironmentInput describes a new environment.
type CreateEnvironmentInput struct {
	Name         string
	Description  string
	IsProduction bool
}

// CreateInstanceInput describes a new instance and the connection that reaches it.
type CreateInstanceInput struct {
	EnvironmentID string
	Name          string
	EngineType    string
	Host          string
	Port          int32
	Labels        map[string]string
	Connection    *fwv1.ConnectionSpec
}

// ListInstancesFilter narrows a listing. Both fields are display-level filters; core does not
// branch on either.
type ListInstancesFilter struct {
	EnvironmentID string
	EngineType    string
	PageSize      int32
	PageToken     string
}

// Page carries a listing's pagination metadata alongside the rows themselves.
type Page struct {
	// NextPageToken is empty on the last page.
	NextPageToken string
	// TotalSize counts every matching row, not just this page. Populated for instances, where the
	// estate grid shows a total; left zero where nothing asks for it.
	TotalSize int32
}

// -----------------------------------------------------------------------------------------------
// Environments
// -----------------------------------------------------------------------------------------------

// CreateEnvironment adds an environment.
func (s *Service) CreateEnvironment(ctx context.Context, in CreateEnvironmentInput) (*fwv1.Environment, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}

	env := &fwv1.Environment{
		TenantId:     s.tenantID,
		Name:         name,
		Description:  strings.TrimSpace(in.Description),
		IsProduction: in.IsProduction,
	}

	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO environments (tenant_id, name, description, is_production)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		s.tenantID, env.GetName(), env.GetDescription(), env.GetIsProduction()).
		Scan(&env.Id, &createdAt)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: environment %q", ErrAlreadyExists, name)
	}
	if err != nil {
		return nil, fmt.Errorf("inventory: create environment: %w", err)
	}
	env.CreatedAt = timestamppb.New(createdAt)

	s.log.InfoContext(ctx, "environment created",
		slog.String("environment_id", env.GetId()),
		slog.String("name", env.GetName()),
		slog.Bool("is_production", env.GetIsProduction()))
	return env, nil
}

// ListEnvironments returns environments in creation order.
func (s *Service) ListEnvironments(ctx context.Context, pageSize int32, pageToken string) ([]*fwv1.Environment, Page, error) {
	limit, err := clampPageSize(pageSize)
	if err != nil {
		return nil, Page{}, err
	}
	cursor, err := decodeCursor(pageToken)
	if err != nil {
		return nil, Page{}, err
	}

	// Keyset pagination on (created_at, id): an offset would silently skip or repeat rows when an
	// environment is added between two pages.
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, description, is_production, created_at
		FROM environments
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) > ($2::timestamptz, $3::uuid))
		ORDER BY created_at, id
		LIMIT $4`,
		s.tenantID, cursor.after(), cursor.afterID(), limit+1)
	if err != nil {
		return nil, Page{}, fmt.Errorf("inventory: list environments: %w", err)
	}
	defer rows.Close()

	out := make([]*fwv1.Environment, 0, limit)
	for rows.Next() {
		env := &fwv1.Environment{TenantId: s.tenantID}
		var createdAt time.Time
		if err := rows.Scan(&env.Id, &env.Name, &env.Description, &env.IsProduction, &createdAt); err != nil {
			return nil, Page{}, fmt.Errorf("inventory: scan environment: %w", err)
		}
		env.CreatedAt = timestamppb.New(createdAt)
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, Page{}, fmt.Errorf("inventory: list environments: %w", err)
	}

	page := Page{}
	if len(out) > int(limit) {
		last := out[limit-1]
		out = out[:limit]
		page.NextPageToken = encodeCursor(last.GetCreatedAt().AsTime(), last.GetId())
	}
	return out, page, nil
}

// -----------------------------------------------------------------------------------------------
// Instances
// -----------------------------------------------------------------------------------------------

// CreateInstance stores an instance, its connection, and its credentials.
//
// It deliberately does not probe the instance. A slow or unreachable server would make adding it
// hang or fail, and an instance that cannot be reached is exactly the kind a user most needs to
// have in their inventory. TestConnection exists for checking before committing.
func (s *Service) CreateInstance(ctx context.Context, in CreateInstanceInput) (*fwv1.Instance, error) {
	inst, spec, err := s.validateCreateInstance(in)
	if err != nil {
		return nil, err
	}

	optionsJSON, err := marshalConnectionOptions(spec)
	if err != nil {
		return nil, err
	}
	payload, err := marshalCredentialSecret(spec)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("inventory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		createdAt time.Time
		// Read back rather than assumed: the column's default is the single source of truth for what
		// an unprobed instance's health is.
		health string
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, health`,
		s.tenantID, inst.GetEnvironmentId(), inst.GetName(), inst.GetEngineType(),
		inst.GetHost(), inst.GetPort(), labelsOrEmpty(inst.GetLabels())).
		Scan(&inst.Id, &createdAt, &health)
	switch {
	case isUniqueViolation(err):
		return nil, fmt.Errorf("%w: instance %q in this environment", ErrAlreadyExists, inst.GetName())
	case isForeignKeyViolation(err):
		return nil, fmt.Errorf("%w: environment %q", ErrNotFound, inst.GetEnvironmentId())
	case err != nil:
		return nil, fmt.Errorf("inventory: create instance: %w", err)
	}
	inst.CreatedAt = timestamppb.New(createdAt)
	inst.Health = parseHealthState(health)

	// The first connection is the default one. The partial unique index
	// idx_connections_one_default enforces at most one per instance; a second default would fail
	// here rather than leaving core to guess which credentials to use.
	var connectionID string
	err = tx.QueryRow(ctx, `
		INSERT INTO connections (tenant_id, instance_id, username, database, secret_name,
		                         tls_enabled, options, is_default)
		VALUES ($1, $2, $3, $4, '', $5, $6, TRUE)
		RETURNING id`,
		s.tenantID, inst.GetId(), spec.GetUsername(), spec.GetDatabase(),
		spec.GetTls().GetEnabled(), optionsJSON).
		Scan(&connectionID)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: instance %q already has a default connection", ErrAlreadyExists, inst.GetName())
	}
	if err != nil {
		return nil, fmt.Errorf("inventory: create connection: %w", err)
	}

	// The secret name is derived from the connection identifier, which only exists after the insert.
	secretName := secretNamePrefix + connectionID
	if _, err := tx.Exec(ctx,
		`UPDATE connections SET secret_name = $1 WHERE id = $2 AND tenant_id = $3`,
		secretName, connectionID, s.tenantID); err != nil {
		return nil, fmt.Errorf("inventory: link connection secret: %w", err)
	}

	// The SecretsProvider is an interface that may not be backed by this database at all (Vault is
	// the next implementation), so it cannot join the transaction. Writing before the commit means
	// a failed commit leaves an unreferenced secret rather than an instance whose credentials are
	// missing; the compensating delete below closes that window in the normal case.
	ref := secrets.Ref{TenantID: s.tenantID, Name: secretName}
	if err := s.secrets.Put(ctx, ref, payload); err != nil {
		return nil, fmt.Errorf("inventory: store credentials: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		if delErr := s.secrets.Delete(context.WithoutCancel(ctx), ref); delErr != nil {
			s.log.ErrorContext(ctx, "orphaned credential after a failed commit; delete it by hand",
				slog.String("secret_ref", ref.String()),
				slog.String("error", delErr.Error()))
		}
		return nil, fmt.Errorf("inventory: commit instance: %w", err)
	}

	s.log.InfoContext(ctx, "instance created",
		slog.String("instance_id", inst.GetId()),
		slog.String("name", inst.GetName()),
		slog.String("engine_type", inst.GetEngineType()),
		slog.String("host", inst.GetHost()),
		slog.Int("port", int(inst.GetPort())))
	return inst, nil
}

// validateCreateInstance checks the request and returns the instance skeleton it describes.
func (s *Service) validateCreateInstance(in CreateInstanceInput) (*fwv1.Instance, *fwv1.ConnectionSpec, error) {
	name := strings.TrimSpace(in.Name)
	host := strings.TrimSpace(in.Host)
	engineType := strings.ToLower(strings.TrimSpace(in.EngineType))

	// Environments are required rather than created on demand. An instance's environment decides
	// whether destructive operations need production confirmation, and silently defaulting a
	// production server into a non-production environment would turn a missing field into a safety
	// regression.
	environmentID, err := requireUUID("environment_id", in.EnvironmentID)
	if err != nil {
		return nil, nil, err
	}

	switch {
	case name == "":
		return nil, nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	case engineType == "":
		return nil, nil, fmt.Errorf("%w: engine_type is required", ErrInvalidArgument)
	case host == "":
		return nil, nil, fmt.Errorf("%w: host is required", ErrInvalidArgument)
	case in.Port <= 0 || in.Port > 65535:
		// Core has no per-engine default port and must not acquire one: that would be exactly the
		// engine knowledge the plugin contract exists to keep out of core.
		return nil, nil, fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidArgument)
	}

	if _, err := s.plugins.Capabilities(engineType); err != nil {
		return nil, nil, fmt.Errorf("%w: no plugin serves engine_type %q; available: %s",
			ErrInvalidArgument, engineType, strings.Join(s.plugins.EngineTypes(), ", "))
	}

	spec := in.Connection
	if spec == nil {
		return nil, nil, fmt.Errorf("%w: connection is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(spec.GetUsername()) == "" {
		return nil, nil, fmt.Errorf("%w: connection.username is required", ErrInvalidArgument)
	}

	return &fwv1.Instance{
		TenantId:      s.tenantID,
		EnvironmentId: environmentID,
		Name:          name,
		EngineType:    engineType,
		Host:          host,
		Port:          in.Port,
		Labels:        in.Labels,
		Health:        fwv1.HealthState_HEALTH_STATE_UNKNOWN,
	}, spec, nil
}

// GetInstance returns one instance with the capabilities of the plugin serving it and whatever the
// last discovery found. The UI renders its tabs from the capabilities, which is how a
// capability-adaptive detail view stays engine-agnostic.
func (s *Service) GetInstance(ctx context.Context, instanceID string) (*fwv1.GetInstanceResponse, error) {
	inst, discovery, err := s.loadInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	resp := &fwv1.GetInstanceResponse{Instance: inst}
	// A plugin that is down must not make the instance unreadable; capabilities are best-effort.
	if caps, err := s.plugins.Capabilities(inst.GetEngineType()); err == nil {
		resp.Capabilities = caps
	}
	if discovery != nil {
		resp.Server = discovery.GetServer()
		resp.Databases = discovery.GetDatabases()
		resp.Topology = discovery.GetTopology()
	}
	return resp, nil
}

// ListInstances returns instances matching the filter, newest last.
func (s *Service) ListInstances(ctx context.Context, filter ListInstancesFilter) ([]*fwv1.Instance, Page, error) {
	limit, err := clampPageSize(filter.PageSize)
	if err != nil {
		return nil, Page{}, err
	}
	cursor, err := decodeCursor(filter.PageToken)
	if err != nil {
		return nil, Page{}, err
	}

	environmentID, err := nullableUUID("environment_id", filter.EnvironmentID)
	if err != nil {
		return nil, Page{}, err
	}
	engineType := nullableText(strings.ToLower(strings.TrimSpace(filter.EngineType)))

	var total int32
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*)::int
		FROM instances
		WHERE tenant_id = $1
		  AND ($2::uuid IS NULL OR environment_id = $2::uuid)
		  AND ($3::text IS NULL OR engine_type = $3::text)`,
		s.tenantID, environmentID, engineType).Scan(&total); err != nil {
		return nil, Page{}, fmt.Errorf("inventory: count instances: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, environment_id, name, engine_type, engine_version, host, port,
		       labels, health, last_seen_at, created_at
		FROM instances
		WHERE tenant_id = $1
		  AND ($2::uuid IS NULL OR environment_id = $2::uuid)
		  AND ($3::text IS NULL OR engine_type = $3::text)
		  AND ($4::timestamptz IS NULL OR (created_at, id) > ($4::timestamptz, $5::uuid))
		ORDER BY created_at, id
		LIMIT $6`,
		s.tenantID, environmentID, engineType, cursor.after(), cursor.afterID(), limit+1)
	if err != nil {
		return nil, Page{}, fmt.Errorf("inventory: list instances: %w", err)
	}
	defer rows.Close()

	out := make([]*fwv1.Instance, 0, limit)
	for rows.Next() {
		inst, err := scanInstance(rows, s.tenantID)
		if err != nil {
			return nil, Page{}, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, Page{}, fmt.Errorf("inventory: list instances: %w", err)
	}

	page := Page{TotalSize: total}
	if len(out) > int(limit) {
		last := out[limit-1]
		out = out[:limit]
		page.NextPageToken = encodeCursor(last.GetCreatedAt().AsTime(), last.GetId())
	}
	return out, page, nil
}

// DeleteInstance removes an instance, its connections, and its stored credentials.
func (s *Service) DeleteInstance(ctx context.Context, instanceID string, deleteArtifacts bool) error {
	id, err := requireUUID("instance_id", instanceID)
	if err != nil {
		return err
	}

	// Collect the secret names first: connections cascade with the instance, and once they are gone
	// there is nothing left pointing at the credentials.
	rows, err := s.pool.Query(ctx,
		`SELECT secret_name FROM connections WHERE instance_id = $1 AND tenant_id = $2`,
		id, s.tenantID)
	if err != nil {
		return fmt.Errorf("inventory: list connections: %w", err)
	}
	var secretNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("inventory: scan connection: %w", err)
		}
		secretNames = append(secretNames, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventory: list connections: %w", err)
	}

	tag, err := s.pool.Exec(ctx,
		`DELETE FROM instances WHERE id = $1 AND tenant_id = $2`, id, s.tenantID)
	if err != nil {
		return fmt.Errorf("inventory: delete instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: instance %s", ErrNotFound, instanceID)
	}

	// Secrets are keyed by (tenant_id, name) with no foreign key to connections — deliberately, so
	// the secret store stays independent of the schema around it. Nothing else will ever clean these
	// up, so they are deleted here explicitly. Rows are gone by now, so a failure here leaves an
	// unusable orphan rather than a live instance without credentials.
	for _, name := range secretNames {
		ref := secrets.Ref{TenantID: s.tenantID, Name: name}
		if err := s.secrets.Delete(ctx, ref); err != nil {
			s.log.ErrorContext(ctx, "credentials survived instance deletion; delete them by hand",
				slog.String("secret_ref", ref.String()),
				slog.String("error", err.Error()))
		}
	}

	// TODO(A4): honour delete_artifacts once backups exist. It defaults to false because removing
	// an instance from the inventory must not silently destroy its backups; there are no artifacts
	// to remove yet, so the intent is only recorded.
	s.log.InfoContext(ctx, "instance deleted",
		slog.String("instance_id", instanceID),
		slog.Int("secrets_removed", len(secretNames)),
		slog.Bool("delete_artifacts_requested", deleteArtifacts))
	return nil
}

// -----------------------------------------------------------------------------------------------
// Talking to an instance
// -----------------------------------------------------------------------------------------------

// TestConnection health-checks an instance and, for a stored one, records the result.
//
// It serves two callers. The add-instance wizard supplies engine type, host, port, and a connection
// spec to check credentials that have not been stored yet. Everything else supplies an instance
// identifier and the stored credentials are resolved.
func (s *Service) TestConnection(ctx context.Context, req *fwv1.TestConnectionRequest) (*fwv1.TestConnectionResponse, error) {
	target, err := s.resolveTarget(ctx, req)
	if err != nil {
		return nil, err
	}

	client, _, err := s.plugins.Client(target.engineType)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, target.engineType, err)
	}

	// The request carries the probe deadline the plugin is expected to honour, and the call context
	// carries a slightly longer one. The margin matters: the plugin's own timeout produces a
	// HEALTH_STATE_DOWN answer with a structured error, which is far more useful than a cancelled
	// RPC, so it should always be the deadline that fires first.
	callCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout+probeGrace)
	defer cancel()

	health, err := client.HealthCheck(callCtx, &fwv1.HealthCheckRequest{
		Connection:  target.ref,
		Credentials: target.credentials,
		Timeout:     durationpb.New(defaultProbeTimeout),
	})
	if err != nil {
		return nil, pluginError("health check", err)
	}

	if target.instanceID != "" {
		if err := s.recordHealth(ctx, target.instanceID, health); err != nil {
			// The probe succeeded; failing to cache it must not lose the answer the caller asked for.
			s.log.WarnContext(ctx, "could not record instance health",
				slog.String("instance_id", target.instanceID),
				slog.String("error", err.Error()))
		}
	}

	// DEGRADED counts as a success: the connection worked, and the warning belongs in the signals
	// rather than in a boolean that the wizard would read as "these credentials are wrong".
	up := health.GetState() == fwv1.HealthState_HEALTH_STATE_UP ||
		health.GetState() == fwv1.HealthState_HEALTH_STATE_DEGRADED
	return &fwv1.TestConnectionResponse{
		Success: up,
		Health:  health,
		Message: health.GetMessage(),
	}, nil
}

// DiscoverInstance refreshes topology, version, and database inventory, and caches the result so
// GetInstance can answer without touching the database being monitored.
func (s *Service) DiscoverInstance(ctx context.Context, instanceID string) (*fwv1.DiscoverInstanceResponse, error) {
	target, err := s.resolveStored(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	client, _, err := s.plugins.Client(target.engineType)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, target.engineType, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	resp, err := client.Discover(callCtx, &fwv1.DiscoverRequest{
		Connection:  target.ref,
		Credentials: target.credentials,
	})
	if err != nil {
		return nil, pluginError("discover", err)
	}

	if err := s.recordDiscovery(ctx, target.instanceID, resp); err != nil {
		s.log.WarnContext(ctx, "could not cache discovery",
			slog.String("instance_id", target.instanceID),
			slog.String("error", err.Error()))
	}

	return &fwv1.DiscoverInstanceResponse{
		Server:    resp.GetServer(),
		Databases: resp.GetDatabases(),
		Topology:  resp.GetTopology(),
	}, nil
}

// pluginError turns a failed plugin RPC into a service error.
//
// The structured PluginError a plugin attaches to its status is preferred over the status string:
// the contract forbids credentials in it, so it is safe to show a client, and it carries the code
// core would use to decide whether a retry is worth attempting.
func pluginError(operation string, err error) error {
	if pe, ok := sdk.PluginErrorFrom(err); ok {
		return fmt.Errorf("%w: %s: %s", ErrPluginFailed, operation, pe.GetMessage())
	}
	return fmt.Errorf("%w: %s: %s", ErrPluginFailed, operation, status.Convert(err).Message())
}

// target is a resolved instance plus the credentials for one call. It never leaves this package and
// is never logged.
type target struct {
	instanceID  string
	engineType  string
	ref         *fwv1.ConnectionRef
	credentials *fwv1.Credentials
}

// Connection is a resolved instance and the credentials for exactly one plugin call.
//
// It is the one way credentials leave this package, and it exists because backup and verification
// need the same resolution the inventory already performs — a second implementation of it would be
// a second place for the credential-handling rules to be got wrong. The rules travel with it: the
// value must not be stored, logged, or held beyond the call that carries it (ADR-0009).
type Connection struct {
	InstanceID  string
	EngineType  string
	Ref         *fwv1.ConnectionRef
	Credentials *fwv1.Credentials
}

// ResolveConnection materializes an instance's stored credentials for a single plugin call.
func (s *Service) ResolveConnection(ctx context.Context, instanceID string) (*Connection, error) {
	resolved, err := s.resolveStored(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	return &Connection{
		InstanceID:  resolved.instanceID,
		EngineType:  resolved.engineType,
		Ref:         resolved.ref,
		Credentials: resolved.credentials,
	}, nil
}

// resolveTarget produces the target for TestConnection, preferring the request's own connection
// spec so the add-instance wizard can check credentials before anything is stored.
func (s *Service) resolveTarget(ctx context.Context, req *fwv1.TestConnectionRequest) (*target, error) {
	if req.GetConnection() == nil {
		return s.resolveStored(ctx, req.GetInstanceId())
	}

	engineType := strings.ToLower(strings.TrimSpace(req.GetEngineType()))
	host := strings.TrimSpace(req.GetHost())
	port := req.GetPort()
	instanceID := ""

	// A supplied spec may also name a stored instance — the "test these new credentials against the
	// server I already added" case — so the row fills whatever the request left out.
	//
	// The lookup is best-effort on purpose. The REST route carries instance_id in its path, so the
	// wizard has no way to leave it out while it is still deciding whether to add the instance at
	// all; an identifier that resolves to nothing therefore means "not stored yet" rather than a bad
	// request, as long as the request describes its target completely.
	if stored, err := s.resolveStored(ctx, req.GetInstanceId()); err == nil {
		instanceID = stored.instanceID
		if engineType == "" {
			engineType = stored.engineType
		}
		if host == "" {
			host = stored.credentials.GetHost()
		}
		if port <= 0 {
			port = stored.credentials.GetPort()
		}
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalidArgument) {
		return nil, err
	}

	switch {
	case engineType == "":
		return nil, fmt.Errorf("%w: engine_type is required when testing a connection that is not stored yet", ErrInvalidArgument)
	case host == "":
		return nil, fmt.Errorf("%w: host is required when testing a connection that is not stored yet", ErrInvalidArgument)
	case port <= 0 || port > 65535:
		return nil, fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidArgument)
	}

	spec := req.GetConnection()
	return &target{
		instanceID: instanceID,
		engineType: engineType,
		ref:        &fwv1.ConnectionRef{TenantId: s.tenantID, InstanceId: instanceID},
		credentials: &fwv1.Credentials{
			Host:     host,
			Port:     port,
			Username: spec.GetUsername(),
			Password: spec.GetPassword(),
			Database: spec.GetDatabase(),
			Tls:      spec.GetTls(),
			Options:  spec.GetOptions(),
		},
	}, nil
}

// resolveStored materializes the credentials of a stored instance's default connection.
//
// This is the only place a stored secret is ever decrypted, and the result lives no longer than the
// call that carries it (ADR-0009).
func (s *Service) resolveStored(ctx context.Context, instanceID string) (*target, error) {
	id, err := requireUUID("instance_id", instanceID)
	if err != nil {
		return nil, err
	}

	var (
		engineType  string
		host        string
		port        int32
		connID      string
		username    string
		database    string
		secretName  string
		tlsEnabled  bool
		optionsJSON []byte
	)
	err = s.pool.QueryRow(ctx, `
		SELECT i.engine_type, i.host, i.port,
		       c.id, c.username, c.database, c.secret_name, c.tls_enabled, c.options
		FROM instances i
		JOIN connections c ON c.instance_id = i.id AND c.tenant_id = i.tenant_id AND c.is_default
		WHERE i.id = $1 AND i.tenant_id = $2`,
		id, s.tenantID).
		Scan(&engineType, &host, &port, &connID, &username, &database, &secretName, &tlsEnabled, &optionsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		// A missing default connection is indistinguishable from a missing instance to a caller,
		// and both mean the same thing: there is nothing here to talk to.
		return nil, fmt.Errorf("%w: instance %s has no default connection", ErrNotFound, instanceID)
	}
	if err != nil {
		return nil, fmt.Errorf("inventory: load connection: %w", err)
	}

	opts, err := unmarshalConnectionOptions(optionsJSON)
	if err != nil {
		return nil, err
	}

	payload, err := s.loadCredentialSecret(ctx, secretName)
	if err != nil {
		return nil, err
	}

	return &target{
		instanceID: id,
		engineType: engineType,
		ref: &fwv1.ConnectionRef{
			ConnectionId: connID,
			TenantId:     s.tenantID,
			InstanceId:   id,
		},
		credentials: &fwv1.Credentials{
			Host:     host,
			Port:     port,
			Username: username,
			Password: payload.Password,
			Database: database,
			Tls:      opts.tlsSettings(tlsEnabled, payload),
			Options:  opts.Engine,
		},
	}, nil
}

func (s *Service) loadCredentialSecret(ctx context.Context, secretName string) (*credentialSecret, error) {
	plaintext, err := s.secrets.Get(ctx, secrets.Ref{TenantID: s.tenantID, Name: secretName})
	if errors.Is(err, secrets.ErrNotFound) {
		return nil, fmt.Errorf("%w: credentials for %s are missing from the secret store", ErrNotFound, secretName)
	}
	if err != nil {
		return nil, fmt.Errorf("inventory: resolve credentials: %w", err)
	}

	var payload credentialSecret
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		// The plaintext is a credential; only the reference may appear in the error.
		return nil, fmt.Errorf("inventory: stored credential for %s is not readable", secretName)
	}
	return &payload, nil
}

// recordHealth caches a probe result so a listing can render the estate without probing fifty
// servers.
func (s *Service) recordHealth(ctx context.Context, instanceID string, health *fwv1.HealthStatus) error {
	// last_seen_at means "last time we successfully talked to it", so a DOWN probe must not move it.
	var lastSeen *time.Time
	if health.GetState() == fwv1.HealthState_HEALTH_STATE_UP ||
		health.GetState() == fwv1.HealthState_HEALTH_STATE_DEGRADED {
		now := time.Now().UTC()
		lastSeen = &now
	}

	_, err := s.pool.Exec(ctx, `
		UPDATE instances
		SET health         = $1,
		    health_message = $2,
		    last_seen_at   = COALESCE($3::timestamptz, last_seen_at),
		    engine_version = COALESCE(NULLIF($4, ''), engine_version),
		    updated_at     = now()
		WHERE id = $5 AND tenant_id = $6`,
		health.GetState().String(), health.GetMessage(), lastSeen, health.GetEngineVersion(),
		instanceID, s.tenantID)
	if err != nil {
		return fmt.Errorf("inventory: record health: %w", err)
	}
	return nil
}

// recordDiscovery caches the last Discover response on the instance row.
func (s *Service) recordDiscovery(ctx context.Context, instanceID string, resp *fwv1.DiscoverResponse) error {
	encoded, err := protojson.Marshal(resp)
	if err != nil {
		return fmt.Errorf("inventory: encode discovery: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE instances
		SET discovery      = $1,
		    engine_version = COALESCE(NULLIF($2, ''), engine_version),
		    updated_at     = now()
		WHERE id = $3 AND tenant_id = $4`,
		encoded, resp.GetServer().GetVersion(), instanceID, s.tenantID)
	if err != nil {
		return fmt.Errorf("inventory: record discovery: %w", err)
	}
	return nil
}

// loadInstance reads one instance row along with its cached discovery.
func (s *Service) loadInstance(ctx context.Context, instanceID string) (*fwv1.Instance, *fwv1.DiscoverResponse, error) {
	id, err := requireUUID("instance_id", instanceID)
	if err != nil {
		return nil, nil, err
	}

	row := s.pool.QueryRow(ctx, `
		SELECT id, environment_id, name, engine_type, engine_version, host, port,
		       labels, health, last_seen_at, created_at, discovery
		FROM instances
		WHERE id = $1 AND tenant_id = $2`, id, s.tenantID)

	var (
		inst         = &fwv1.Instance{TenantId: s.tenantID}
		health       string
		lastSeen     *time.Time
		createdAt    time.Time
		labels       map[string]string
		discoveryRaw []byte
	)
	err = row.Scan(&inst.Id, &inst.EnvironmentId, &inst.Name, &inst.EngineType, &inst.EngineVersion,
		&inst.Host, &inst.Port, &labels, &health, &lastSeen, &createdAt, &discoveryRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: instance %s", ErrNotFound, instanceID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inventory: load instance: %w", err)
	}

	inst.Labels = labels
	inst.Health = parseHealthState(health)
	inst.CreatedAt = timestamppb.New(createdAt)
	if lastSeen != nil {
		inst.LastSeenAt = timestamppb.New(*lastSeen)
	}

	var discovery *fwv1.DiscoverResponse
	if len(discoveryRaw) > 0 && string(discoveryRaw) != "{}" {
		discovery = &fwv1.DiscoverResponse{}
		// A cached snapshot written by an older contract must not make the instance unreadable.
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(discoveryRaw, discovery); err != nil {
			s.log.WarnContext(ctx, "cached discovery could not be decoded; re-run discovery",
				slog.String("instance_id", inst.GetId()),
				slog.String("error", err.Error()))
			discovery = nil
		}
	}

	return inst, discovery, nil
}
