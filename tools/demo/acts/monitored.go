package acts

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The monitored database's own content.
//
// `fleetward_demo` is created empty by the SQL Server image, and a backup of an empty database has
// an empty manifest — which would make act 3 say "all 0 objects match" and act 4's row counts prove
// nothing beyond the checksum. So the demo gives the monitored server something to lose.
//
// This is the one place the demo writes to a monitored instance, and it is deliberately not
// something Fleetward does: Fleetward reads databases and never writes to them. Here the demo is
// standing in for the application that would own this database.
//
// It goes through `docker compose exec` rather than through the published port, and that is a
// safety property rather than a convenience. A developer machine often already runs a SQL Server on
// 1433, and on Windows both it and Docker's port proxy can bind it at once — so a host connection
// is not proof of which server answered. Everything else the demo does through a published port
// only reads, or is guarded by an identity check; this drops and creates tables, and it must be
// impossible for it to do that to somebody's own server.
const (
	demoCustomers = 1200
	demoOrders    = 9400

	// The client the SQL Server image ships with, and the path its own health check already uses.
	sqlcmdPath = "/opt/mssql-tools18/bin/sqlcmd"
)

// monitoredSchema is what the demo's application would have created. Each statement is sent on its
// own, because a common table expression has to begin its batch.
var monitoredSchema = []string{
	`IF OBJECT_ID('dbo.Orders', 'U') IS NOT NULL DROP TABLE dbo.Orders`,
	`IF OBJECT_ID('dbo.Customers', 'U') IS NOT NULL DROP TABLE dbo.Customers`,
	`CREATE TABLE dbo.Customers (
	     id INT NOT NULL PRIMARY KEY,
	     name NVARCHAR(120) NOT NULL,
	     country CHAR(2) NOT NULL,
	     created_at DATETIME2 NOT NULL DEFAULT SYSUTCDATETIME())`,
	`CREATE TABLE dbo.Orders (
	     id INT NOT NULL PRIMARY KEY,
	     customer_id INT NOT NULL REFERENCES dbo.Customers (id),
	     total_cents BIGINT NOT NULL,
	     placed_at DATETIME2 NOT NULL)`,
	// A numbers relation from a cross join of system rows: a few thousand inserts in one statement
	// rather than a few thousand round trips.
	fmt.Sprintf(`WITH n AS (
	     SELECT TOP (%d) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
	     FROM sys.all_objects a CROSS JOIN sys.all_objects b)
	 INSERT INTO dbo.Customers (id, name, country)
	 SELECT i, CONCAT('Customer ', i),
	        CASE i %% 4 WHEN 0 THEN 'RO' WHEN 1 THEN 'GB' WHEN 2 THEN 'DE' ELSE 'NL' END
	 FROM n`, demoCustomers),
	fmt.Sprintf(`WITH n AS (
	     SELECT TOP (%d) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
	     FROM sys.all_objects a CROSS JOIN sys.all_objects b)
	 INSERT INTO dbo.Orders (id, customer_id, total_cents, placed_at)
	 SELECT i, (i %% %d) + 1, (i * 137) %% 500000, DATEADD(minute, -i, SYSUTCDATETIME())
	 FROM n`, demoOrders, demoCustomers),
}

// seedMonitoredDatabase puts two tables and a few thousand rows into the database act 3 backs up.
func seedMonitoredDatabase(ctx context.Context, cfg Config, n *Narrator) error {
	// The image creates the database in a setup pass that runs after the server is already
	// listening, so the container being up is not the same as `fleetward_demo` existing.
	if err := waitForMonitored(ctx, cfg); err != nil {
		return err
	}
	for _, stmt := range monitoredSchema {
		if out, err := sqlcmd(ctx, cfg, stmt); err != nil {
			return fmt.Errorf("prepare the monitored database: %w\n%s", err, strings.TrimSpace(out))
		}
	}

	n.Step("the monitored database holds %d customers and %d orders, so the manifest act 3 captures "+
		"describes something", demoCustomers, demoOrders)
	return nil
}

// sqlcmd runs one statement inside the monitored container.
func sqlcmd(ctx context.Context, cfg Config, statement string) (string, error) {
	return run(ctx, cfg.Root, "docker", "compose", "exec", "-T", "sqlserver",
		sqlcmdPath, "-C", "-b", "-S", "127.0.0.1", "-U", sqlUser, "-P", sqlPass, "-d", sqlDB,
		"-Q", statement)
}

func waitForMonitored(ctx context.Context, cfg Config) error {
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for {
		out, err := sqlcmd(ctx, cfg, `SELECT 1`)
		if err == nil {
			return nil
		}
		last = strings.TrimSpace(out)
		if time.Now().After(deadline) {
			return fmt.Errorf("the monitored SQL Server never accepted a connection to %s: %s", sqlDB, last)
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}
