//go:build conformance

package conformance

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// postgresFixture is the PostgreSQL implementation of Fixture.
//
// It is the only engine-specific code in this suite, it is under fifty lines, and none of the
// assertions depend on it — everything it produces reaches the tests through the plugin's own
// manifest. That is the shape a fixture for a new engine should keep.
type postgresFixture struct{}

// seedStatements build two tables with different row counts, plus a foreign key, so that a restore
// has to reproduce an order of operations rather than a single flat table.
var seedStatements = []string{
	`CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL)`,
	`CREATE TABLE orders (id serial PRIMARY KEY, customer_id int REFERENCES customers(id), total numeric)`,
	`INSERT INTO customers (name) SELECT 'customer-' || g FROM generate_series(1, 40) g`,
	`INSERT INTO orders (customer_id, total) SELECT (g % 40) + 1, g * 1.5 FROM generate_series(1, 120) g`,
}

func (postgresFixture) Seed(ctx context.Context, creds *fwv1.Credentials) error {
	conn, err := connect(ctx, creds)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, stmt := range seedStatements {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("seed %q: %w", stmt, err)
		}
	}
	return nil
}

// RemoveRows deletes a quarter of the orders. The object name is schema-qualified because that is
// how the plugin's manifest names it, and a discrepancy the suite cannot match to an entry would
// make the assertion vacuous.
func (postgresFixture) RemoveRows(ctx context.Context, creds *fwv1.Credentials) (string, int64, error) {
	conn, err := connect(ctx, creds)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = conn.Close(ctx) }()

	tag, err := conn.Exec(ctx, `DELETE FROM orders WHERE id % 4 = 0`)
	if err != nil {
		return "", 0, fmt.Errorf("delete rows: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", 0, fmt.Errorf("no rows were deleted, so the source still matches its manifest")
	}
	return "public.orders", tag.RowsAffected(), nil
}

// connect builds the configuration field by field rather than formatting a connection string, for
// the same reason the plugin does: a DSN carrying a password ends up in error messages and logs,
// and the only reliable prevention is never to construct one.
func connect(ctx context.Context, creds *fwv1.Credentials) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("build a connection config: %w", err)
	}
	cfg.Host = creds.GetHost()
	cfg.Port = uint16(creds.GetPort()) //nolint:gosec // G115: a sandbox port comes from Docker and is always a valid uint16
	cfg.User = creds.GetUsername()
	cfg.Password = creds.GetPassword()
	cfg.Database = creds.GetDatabase()
	// A sandbox lives for minutes on the loopback interface of the machine running the suite, and
	// no image ships a certificate the test could verify.
	cfg.TLSConfig = nil

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the %s instance: %w", creds.GetDatabase(), err)
	}
	return conn, nil
}
