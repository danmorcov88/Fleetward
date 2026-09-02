package sqlserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// dialTimeout bounds establishing one connection. It is short because every caller of this
	// package treats "did not answer" as an answer, and a slow one is indistinguishable from a
	// hung verification.
	dialTimeout = 15 * time.Second

	// masterDatabase is where a connection lands when the caller named no database. RESTORE and
	// DBCC run from here, because a session connected to a database cannot restore over it.
	masterDatabase = "master"
)

// open builds a connection to one instance.
//
// The configuration is assembled field by field rather than formatted into a connection string, for
// one reason that matters more than tidiness: a DSN carrying a password ends up in error messages,
// in logs, and in stack traces, and the only reliable prevention is never to construct one
// (ADR-0009).
//
// database overrides the credentials' own, which RESTORE needs: it has to run from a session that
// is not connected to the database being replaced.
func open(creds *fwv1.Credentials, database string) (*sql.DB, error) {
	if creds.GetHost() == "" {
		return nil, sdk.InvalidArgument("credentials carry no host")
	}
	if port := creds.GetPort(); port <= 0 || port > 65535 {
		return nil, sdk.InvalidArgument("credentials carry no usable port")
	}

	cfg := msdsn.Config{
		Host:     creds.GetHost(),
		Port:     uint64(creds.GetPort()), //nolint:gosec // G115: range-checked immediately above
		User:     creds.GetUsername(),
		Password: creds.GetPassword(),
		Database: database,
		// TCP only. The driver would otherwise also try named pipes on Windows, which cannot reach
		// a container and turns a clean refusal into a slow one.
		Protocols:   []string{"tcp"},
		DialTimeout: dialTimeout,
		AppName:     "fleetward",
		// database/sql retries a query that started on a connection the pool had already lost.
		// For BACKUP and RESTORE that would mean running a statement twice against a production
		// server, which is never what anyone wants.
		DisableRetry: true,
	}
	if cfg.Database == "" {
		cfg.Database = creds.GetDatabase()
	}

	tlsConfig, encryption, err := tlsSettings(creds.GetTls(), creds.GetHost())
	if err != nil {
		return nil, err
	}
	cfg.TLSConfig = tlsConfig
	cfg.Encryption = encryption
	cfg.TrustServerCertificate = tlsConfig != nil && tlsConfig.InsecureSkipVerify

	db := sql.OpenDB(mssql.NewConnectorConfig(cfg))
	// A plugin call is one operation against one instance. A pool that outlives it would hold
	// credentials past the call that carried them, which the contract forbids.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// tlsSettings turns the contract's TLS block into what the driver wants.
//
// A sandbox generates its own certificate for a hostname that does not exist, so verification there
// is not merely inconvenient — there is nothing that could ever satisfy it. That is why a sandbox's
// credentials arrive with no TLS block at all and land in the first branch.
func tlsSettings(settings *fwv1.TLSSettings, host string) (*tls.Config, msdsn.Encryption, error) {
	if !settings.GetEnabled() {
		// The login packet is still encrypted — SQL Server insists on that regardless — and the
		// certificate behind it is the instance's own self-signed one.
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, msdsn.EncryptionOff, nil //nolint:gosec // G402: the caller declined TLS; this covers only the login packet SQL Server always encrypts
	}

	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         settings.GetServerName(),
		InsecureSkipVerify: settings.GetInsecureSkipVerify(), //nolint:gosec // G402: explicitly requested, and core warns when it is set
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	if pem := settings.GetCaPem(); len(pem) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, 0, sdk.InvalidArgument("the supplied CA bundle contains no usable certificate")
		}
		cfg.RootCAs = pool
	}
	if cert := settings.GetClientCertPem(); len(cert) > 0 {
		pair, err := tls.X509KeyPair(cert, settings.GetClientKeyPem())
		if err != nil {
			// The key is a credential; only the fact that it did not load may be reported.
			return nil, 0, sdk.InvalidArgument("the supplied client certificate and key do not form a pair")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, msdsn.EncryptionRequired, nil
}

// ping opens a connection and confirms the server answers.
func ping(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return classifyConnError(err)
	}
	return nil
}

// engineVersion reads the instance's product version, or an empty string if it will not say.
//
// It is best effort on purpose. The version is recorded alongside a backup so a verification can
// stand up a matching container; failing a backup that has already been written because the server
// declined one extra query would be the wrong trade by a wide margin.
func engineVersion(ctx context.Context, db *sql.DB) string {
	var version string
	if err := db.QueryRowContext(ctx,
		`SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))`).Scan(&version); err != nil {
		return ""
	}
	return version
}

// classifyConnError turns a driver error into the typed code core classifies on.
//
// The distinction is load-bearing rather than cosmetic. Core reads a failed connection as an
// infrastructure problem and a failed login as a configuration one; it retries the first and
// deliberately does not retry the second, because the same wrong password stays wrong and retrying
// it can lock the account on the monitored instance (ADR-0022, and §8 of the plugin guide).
func classifyConnError(err error) error {
	if err == nil {
		return nil
	}

	var engineErr mssqlError
	if asEngineError(err, &engineErr) &&
		engineErr.hasNumber(18456, 18452, 4060, 916, 18470) {
		// Login failed, login from an untrusted domain, cannot open the requested database, no
		// access to it, and a disabled login. All five are answers about the credentials.
		return sdk.AuthenticationFailed("the instance refused the login").WithCause(err)
	}

	// The cause is kept local: a driver error can carry the whole connection configuration, and
	// only the message crosses the process boundary.
	return sdk.ConnectionFailed("connect to the instance").WithCause(err)
}

// quoteIdentifier renders a name as a bracket-delimited T-SQL identifier.
//
// A table or database name cannot be a query parameter — it is not a value — so it has to be
// interpolated, and interpolation is where injection lives. Doubling the closing bracket is exactly
// what QUOTENAME does, and it is the whole of the escaping rule for this delimiter.
func quoteIdentifier(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// quoteLiteral renders a string as a T-SQL literal, for the file paths that BACKUP and RESTORE take
// as strings rather than as identifiers.
func quoteLiteral(value string) string {
	return "N'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// qualified renders a schema and table as one quoted, addressable name.
func qualified(schema, table string) string {
	return quoteIdentifier(schema) + "." + quoteIdentifier(table)
}

// objectName is how a table appears in a manifest: schema-qualified and unquoted, so the same
// string is produced from the source and from the restored copy regardless of quoting.
func objectName(schema, table string) string {
	return schema + "." + table
}

// splitObjectName reverses objectName. A name with no dot is treated as being in the default
// schema, which is what a manifest written by hand or by an older version would carry.
func splitObjectName(name string) (schema, table string) {
	if idx := strings.Index(name, "."); idx >= 0 {
		return name[:idx], name[idx+1:]
	}
	return "dbo", name
}

// resolveDatabase decides which database an operation acts on.
//
// One database per artifact. An instance-wide backup would need either several artifacts or several
// backup sets inside one media set, and both are decisions this slice deliberately does not make.
func resolveDatabase(creds *fwv1.Credentials, requested []string) (string, error) {
	switch len(requested) {
	case 0:
		database := strings.TrimSpace(creds.GetDatabase())
		if database == "" || strings.EqualFold(database, masterDatabase) {
			return "", sdk.InvalidArgument(
				"no database to back up: name one in the request, or set the connection's database " +
					"to something other than master")
		}
		return database, nil
	case 1:
		if strings.TrimSpace(requested[0]) == "" {
			return "", sdk.InvalidArgument("the requested database name is empty")
		}
		return strings.TrimSpace(requested[0]), nil
	default:
		return "", sdk.Unsupported("the %s method backs up one database per artifact; %d were requested",
			MethodFull, len(requested))
	}
}
