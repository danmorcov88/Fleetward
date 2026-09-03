//go:build integration

// Integration test for the migrations themselves, against a real PostgreSQL.
//
// It exists because of a specific failure that only appears the first time a project acquires a
// second migration, and appears then in the direction nobody exercises. Every start-up runs Up, so
// Up is tested continuously by everything else in this repository. Down is run by a human, once, on
// the worst day — and narrowing a CHECK constraint fails against exactly the rows the Up migration
// made possible, so a down migration that looks like the mirror image of its up is not one.
//
// Run with: go test -tags=integration ./internal/storage/metadb/...
package metadb

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danmorcov88/fleetward/internal/config"
)

const (
	migrationTestImage   = "postgres:16-alpine"
	migrationTestTimeout = 3 * time.Minute
)

// TestMigrationsRunBothWays applies every migration, fills the tables the newest one made possible,
// then rolls all the way back and forward again.
//
// The rows are the point. Rolling back an empty schema proves nothing: PostgreSQL validates a
// narrowed CHECK against the rows already in the table, so the down migration is only correct if it
// deals with the rows its own up migration allowed.
func TestMigrationsRunBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), migrationTestTimeout)
	defer cancel()

	db := openTestDB(t, ctx)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("apply the migrations: %v", err)
	}

	seedRowsOnlyTheNewestMigrationAllows(t, ctx, db)

	m, sqlDB := migrator(t, db)
	defer func() { _ = sqlDB.Close() }()

	if err := m.Down(); err != nil {
		t.Fatalf("roll every migration back: %v", err)
	}
	if _, _, err := m.Version(); err != migrate.ErrNilVersion {
		t.Fatalf("version after a full rollback = %v, want no version at all", err)
	}

	// And forward again, because a rollback that leaves the database unable to migrate up is a
	// rollback nobody can recover from.
	if err := m.Up(); err != nil {
		t.Fatalf("re-apply the migrations after a rollback: %v", err)
	}
}

// seedRowsOnlyTheNewestMigrationAllows writes an observed backup and an observation schedule: the
// two things migration 000002 widened a CHECK constraint to permit, and therefore the two things
// its down migration has to deal with before it can narrow the constraint again.
func seedRowsOnlyTheNewestMigrationAllows(t *testing.T, ctx context.Context, db *DB) {
	t.Helper()

	var environmentID string
	if err := db.Pool().QueryRow(ctx, `
		INSERT INTO environments (tenant_id, name) VALUES ($1, 'production') RETURNING id`,
		DefaultTenantID).Scan(&environmentID); err != nil {
		t.Fatalf("seed environment: %v", err)
	}

	var instanceID string
	if err := db.Pool().QueryRow(ctx, `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, 'prod-1', 'testengine', 'db.example.internal', 5432) RETURNING id`,
		DefaultTenantID, environmentID).Scan(&instanceID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO schedules (tenant_id, instance_id, kind, cron_expression, expected_cron,
		                       expected_grace_minutes)
		VALUES ($1, $2, 'observe', '*/30 * * * *', '0 2 * * *', 120)`,
		DefaultTenantID, instanceID); err != nil {
		t.Fatalf("seed an observation schedule: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO jobs (tenant_id, instance_id, kind, state)
		VALUES ($1, $2, 'observe', 'succeeded')`, DefaultTenantID, instanceID); err != nil {
		t.Fatalf("seed an observation job: %v", err)
	}

	// state = 'unknown' is the value only an observed backup ever takes, and the one the down
	// migration's narrowed constraint would refuse.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO backups (tenant_id, instance_id, method_id, state, origin, external_id,
		                     external_location, completed_at)
		VALUES ($1, $2, 'file', 'unknown', 'observed', 'file:abc', '/srv/backups/x.dump', now())`,
		DefaultTenantID, instanceID); err != nil {
		t.Fatalf("seed an observed backup: %v", err)
	}

	// The identity index is what makes a poll idempotent, so it is worth proving it exists and
	// bites rather than assuming the CREATE ran.
	_, err := db.Pool().Exec(ctx, `
		INSERT INTO backups (tenant_id, instance_id, method_id, state, origin, external_id)
		VALUES ($1, $2, 'file', 'unknown', 'observed', 'file:abc')`, DefaultTenantID, instanceID)
	if err == nil {
		t.Error("the same backup was recorded twice; the identity index is not enforcing anything")
	} else if !IsUniqueViolation(err) {
		t.Errorf("recording the same backup twice failed with %v, want a unique violation", err)
	}
}

func migrator(t *testing.T, db *DB) (*migrate.Migrate, *sql.DB) {
	t.Helper()

	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("open the migration source: %v", err)
	}
	sqlDB := stdlib.OpenDB(*db.connConfig)
	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		t.Fatalf("create the migration driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		t.Fatalf("create the migrator: %v", err)
	}
	return m, sqlDB
}

func openTestDB(t *testing.T, ctx context.Context) *DB {
	t.Helper()

	container, err := postgres.Run(ctx, migrationTestImage,
		postgres.WithDatabase("fleetward"),
		postgres.WithUsername("fleetward"),
		postgres.WithPassword("fleetward"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(migrationTestTimeout)),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := container.Terminate(stopCtx); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := Open(ctx, config.MetaDBConfig{DSN: dsn, ConnectTimeout: 30 * time.Second},
		slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	if err != nil {
		t.Fatalf("open the metadata store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { return len(p), nil }
