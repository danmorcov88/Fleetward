//go:build conformance

package conformance

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"path"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// sqlserverFixture is the SQL Server implementation of Fixture.
//
// It is the second engine-specific file in this suite, it is the same shape and roughly the same
// size as the first, and none of the assertions depend on it — everything it produces reaches the
// tests through the plugin's own manifest. That is the whole of what adding an engine costs here.
type sqlserverFixture struct{}

// sqlserverSeed builds two tables with different row counts and a foreign key between them, so a
// restore has to reproduce an order of operations rather than a single flat table.
var sqlserverSeed = []string{
	`CREATE TABLE customers (id int IDENTITY(1,1) PRIMARY KEY, name nvarchar(100) NOT NULL)`,
	`CREATE TABLE orders (
		id int IDENTITY(1,1) PRIMARY KEY,
		customer_id int NOT NULL REFERENCES customers(id),
		total decimal(12,2) NOT NULL)`,
	// A numbers table built from the catalog rather than from a loop: forty and a hundred and
	// twenty rows, deterministically, in two statements.
	`INSERT INTO customers (name)
	 SELECT TOP (40) CONCAT('customer-', ROW_NUMBER() OVER (ORDER BY object_id))
	 FROM sys.all_objects`,
	`INSERT INTO orders (customer_id, total)
	 SELECT TOP (120) ((ROW_NUMBER() OVER (ORDER BY object_id) - 1) % 40) + 1,
	                  ROW_NUMBER() OVER (ORDER BY object_id) * 1.5
	 FROM sys.all_objects`,
}

func (sqlserverFixture) Seed(ctx context.Context, creds *fwv1.Credentials) error {
	db, err := connectSQLServer(creds)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	for _, stmt := range sqlserverSeed {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("seed %q: %w", stmt, err)
		}
	}
	return nil
}

// RemoveRows deletes a quarter of the orders. The object name is schema-qualified because that is
// how the plugin's manifest names it, and a discrepancy the suite cannot match to an entry would
// make the assertion vacuous.
func (sqlserverFixture) RemoveRows(ctx context.Context, creds *fwv1.Credentials) (string, int64, error) {
	db, err := connectSQLServer(creds)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = db.Close() }()

	result, err := db.ExecContext(ctx, `DELETE FROM orders WHERE id % 4 = 0`)
	if err != nil {
		return "", 0, fmt.Errorf("delete rows: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return "", 0, fmt.Errorf("count the deleted rows: %w", err)
	}
	if removed == 0 {
		return "", 0, fmt.Errorf("no rows were deleted, so the source still matches its manifest")
	}
	return "dbo.orders", removed, nil
}

// connectSQLServer builds the configuration field by field rather than formatting a connection
// string, for the same reason the plugin does: a DSN carrying a password ends up in error messages
// and logs, and the only reliable prevention is never to construct one.
func connectSQLServer(creds *fwv1.Credentials) (*sql.DB, error) {
	cfg := msdsn.Config{
		Host:        creds.GetHost(),
		Port:        uint64(creds.GetPort()), //nolint:gosec // G115: a sandbox port comes from Docker and is always a valid port
		User:        creds.GetUsername(),
		Password:    creds.GetPassword(),
		Database:    creds.GetDatabase(),
		Protocols:   []string{"tcp"},
		AppName:     "fleetward-conformance",
		Encryption:  msdsn.EncryptionOff,
		DialTimeout: 15 * time.Second,
		// A sandbox lives for minutes on the loopback interface of the machine running the suite,
		// behind a certificate it generated for itself, so there is nothing here to verify against.
		// SQL Server encrypts the login packet whatever the mode, so the TLS configuration has to be
		// supplied even when encryption is off — the flag alone is not enough when the configuration
		// is built by hand rather than parsed from a connection string.
		TrustServerCertificate: true,
		TLSConfig:              &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // G402: a throwaway container's self-signed certificate, on loopback
	}
	return sql.OpenDB(mssql.NewConnectorConfig(cfg)), nil
}

// TakeExternalBackup runs a native backup the way a maintenance plan or a scheduled T-SQL job on
// this instance would, with Fleetward not involved in it at all.
//
// The engine records it in its own history exactly as it records everyone else's, which is what
// makes this instance indistinguishable from a production one whose backups predate Fleetward. The
// artifact is written into the directory the sandbox has mounted; nothing ever reads it back.
func (sqlserverFixture) TakeExternalBackup(ctx context.Context, creds *fwv1.Credentials) error {
	dir := creds.GetSharedDirectory().GetEnginePath()
	if dir == "" {
		return fmt.Errorf("this instance has no directory the engine can write a backup to")
	}

	db, err := connectSQLServer(creds)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	database := creds.GetDatabase()
	if database == "" {
		return fmt.Errorf("no database to back up")
	}
	// path.Join rather than filepath.Join: the path is interpreted by the database server, which is
	// not necessarily running the operating system this test is.
	target := path.Join(dir, fmt.Sprintf("external-%d.bak", time.Now().UnixNano()))
	stmt := fmt.Sprintf("BACKUP DATABASE [%s] TO DISK = N'%s' WITH FORMAT, INIT",
		strings.ReplaceAll(database, "]", "]]"), strings.ReplaceAll(target, "'", "''"))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("take a backup outside Fleetward: %w", err)
	}
	return nil
}
