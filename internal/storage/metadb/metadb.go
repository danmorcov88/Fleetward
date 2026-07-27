// Package metadb is the PostgreSQL metadata store (ADR-0005).
//
// It owns the connection pool, schema migrations, and the low-level access used by the control
// plane's services. Queries are parameterized without exception; nothing in this package builds SQL
// by string concatenation from caller input.
package metadb

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
)

// DefaultTenantID is the tenant seeded by the first migration. The MVP runs single-tenant, but
// every row still carries a real tenant so the multi-tenant query paths are exercised from day one.
const DefaultTenantID = "00000000-0000-0000-0000-000000000001"

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB is the metadata store handle.
type DB struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// connConfig is retained so Migrate can open a database/sql handle, which is what
	// golang-migrate's driver requires, without re-parsing (and therefore re-handling) the DSN.
	connConfig *pgx.ConnConfig
}

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, cfg config.MetaDBConfig, log *slog.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// ParseConfig errors can echo the DSN, which contains the password.
		return nil, errors.New("metadb: invalid connection string (check FLEETWARD_METADB_DSN)")
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("metadb: create pool: %w", err)
	}

	db := &DB{pool: pool, log: log, connConfig: poolCfg.ConnConfig}

	pingCtx, cancel := context.WithTimeout(ctx, max(cfg.ConnectTimeout, 5*time.Second))
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("metadb: ping: %w", err)
	}

	return db, nil
}

// Pool exposes the underlying connection pool for the service layer and for sqlc-generated queries.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Migrate applies all pending migrations.
//
// golang-migrate takes an advisory lock for the duration, so several control-plane replicas
// starting at once is safe: one migrates and the rest wait, then find nothing to do.
func (db *DB) Migrate(ctx context.Context) error {
	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("metadb: open migration source: %w", err)
	}

	// golang-migrate's driver is database/sql based, so migrations run over a dedicated stdlib
	// handle rather than borrowing a connection from the pgx pool. It is short-lived and closed
	// before any application traffic starts.
	sqlDB := stdlib.OpenDB(*db.connConfig)
	defer func() { _ = sqlDB.Close() }()

	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("metadb: connect for migration: %w", err)
	}

	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("metadb: create migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("metadb: create migrator: %w", err)
	}

	before, _, _ := m.Version()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("metadb: apply migrations: %w", err)
	}
	after, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("metadb: read schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("metadb: schema version %d is dirty; a previous migration failed part-way "+
			"and must be resolved manually before the control plane can start", after)
	}

	if before == after {
		db.log.Info("metadb schema up to date", slog.Uint64("version", uint64(after)))
	} else {
		db.log.Info("metadb schema migrated",
			slog.Uint64("from", uint64(before)),
			slog.Uint64("to", uint64(after)))
	}
	return nil
}

// SchemaVersion reports the currently applied migration version.
func (db *DB) SchemaVersion(ctx context.Context) (uint, bool, error) {
	var version uint
	var dirty bool
	err := db.pool.QueryRow(ctx,
		`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("metadb: read schema version: %w", err)
	}
	return version, dirty, nil
}

// HealthCheck verifies the database answers a trivial query.
func (db *DB) HealthCheck(ctx context.Context) error {
	var one int
	if err := db.pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("metadb: unhealthy: %w", err)
	}
	return nil
}

// Close shuts the pool down.
func (db *DB) Close() error {
	db.pool.Close()
	return nil
}

// -----------------------------------------------------------------------------------------------
// secrets.Store
// -----------------------------------------------------------------------------------------------

var _ secrets.Store = (*DB)(nil)

// PutSecret stores ciphertext, replacing any existing value at the same reference.
func (db *DB) PutSecret(ctx context.Context, ref secrets.Ref, ciphertext []byte, keyVersion int32) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO secrets (tenant_id, name, ciphertext, key_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, name)
		DO UPDATE SET ciphertext = EXCLUDED.ciphertext,
		              key_version = EXCLUDED.key_version`,
		ref.TenantID, ref.Name, ciphertext, keyVersion)
	if err != nil {
		// The reference is safe to include; the ciphertext and plaintext are not, and neither
		// appears here.
		return fmt.Errorf("metadb: put secret %s: %w", ref, err)
	}
	return nil
}

// GetSecret loads ciphertext, returning secrets.ErrNotFound when the reference is unknown.
func (db *DB) GetSecret(ctx context.Context, ref secrets.Ref) ([]byte, int32, error) {
	var ciphertext []byte
	var keyVersion int32
	err := db.pool.QueryRow(ctx, `
		SELECT ciphertext, key_version
		FROM secrets
		WHERE tenant_id = $1 AND name = $2`,
		ref.TenantID, ref.Name).Scan(&ciphertext, &keyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, secrets.ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("metadb: get secret %s: %w", ref, err)
	}
	return ciphertext, keyVersion, nil
}

// DeleteSecret removes a secret. Deleting one that does not exist is not an error.
func (db *DB) DeleteSecret(ctx context.Context, ref secrets.Ref) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM secrets WHERE tenant_id = $1 AND name = $2`,
		ref.TenantID, ref.Name)
	if err != nil {
		return fmt.Errorf("metadb: delete secret %s: %w", ref, err)
	}
	return nil
}

// PingSecrets reports whether the secrets table is reachable.
func (db *DB) PingSecrets(ctx context.Context) error {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'secrets')`).
		Scan(&exists)
	if err != nil {
		return fmt.Errorf("metadb: secrets table unreachable: %w", err)
	}
	if !exists {
		return errors.New("metadb: secrets table is missing; run migrations")
	}
	return nil
}
