package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// defaultConnectTimeout bounds a connection attempt when the caller supplies none. Monitoring an
// estate means probing unreachable hosts routinely, and a probe that hangs is worse than one that
// fails: it holds a slot in the collection loop.
const defaultConnectTimeout = 10 * time.Second

// connect opens a single connection from per-request credentials.
//
// The caller owns the returned connection and must close it. Credentials are used to build the
// configuration and are not retained anywhere afterwards (ADR-0009).
func connect(ctx context.Context, creds *fwv1.Credentials) (*pgx.Conn, error) {
	cfg, err := connConfig(creds)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, classifyConnectError(err, creds)
	}
	return conn, nil
}

// connConfig builds a pgx configuration without ever rendering a connection string.
//
// Building the config field by field rather than formatting a DSN is deliberate: a DSN containing a
// password tends to end up in an error message, a log line, or a stack trace, and the only reliable
// way to prevent that is never to construct one.
func connConfig(creds *fwv1.Credentials) (*pgx.ConnConfig, error) {
	if creds == nil {
		return nil, sdk.InvalidArgument("credentials are required")
	}
	if creds.GetHost() == "" {
		return nil, sdk.InvalidArgument("host is required")
	}

	port := creds.GetPort()
	if port == 0 {
		port = 5432
	}
	if port < 0 || port > 65535 {
		return nil, sdk.InvalidArgument("port %d is out of range", port)
	}

	database := creds.GetDatabase()
	if database == "" {
		// Every PostgreSQL cluster has this database, and connecting to it needs no prior
		// knowledge of the instance — which is exactly the situation during discovery.
		database = "postgres"
	}

	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, sdk.Internal("build connection config: %v", err)
	}

	cfg.Host = creds.GetHost()
	cfg.Port = uint16(port) //nolint:gosec // G115: range-checked above
	cfg.User = creds.GetUsername()
	cfg.Password = creds.GetPassword()
	cfg.Database = database
	cfg.ConnectTimeout = defaultConnectTimeout

	// Fleetward identifies itself so a DBA reading pg_stat_activity can tell which connections are
	// ours. On an estate being monitored continuously, an unexplained connection is a support
	// ticket.
	cfg.RuntimeParams["application_name"] = "fleetward"

	tlsCfg, err := buildTLS(creds.GetTls(), creds.GetHost())
	if err != nil {
		return nil, err
	}
	cfg.TLSConfig = tlsCfg

	for key, value := range creds.GetOptions() {
		switch key {
		case "application_name":
			// Ignored on purpose: identifying our connections is not the operator's to override.
		case optionBackupFilePattern:
			// Configures this plugin rather than the connection. Passing it on would make libpq
			// refuse a runtime parameter it has never heard of, and the whole instance would look
			// unreachable because somebody configured which files count as a backup.
		case "connect_timeout":
			seconds, convErr := strconv.Atoi(value)
			if convErr != nil || seconds <= 0 {
				return nil, sdk.InvalidArgument("connect_timeout must be a positive number of seconds, got %q", value)
			}
			cfg.ConnectTimeout = time.Duration(seconds) * time.Second
		default:
			cfg.RuntimeParams[key] = value
		}
	}

	return cfg, nil
}

// buildTLS translates the contract's TLS settings into a tls.Config.
func buildTLS(settings *fwv1.TLSSettings, host string) (*tls.Config, error) {
	if !settings.GetEnabled() {
		// nil disables TLS in pgx. The contract makes this an explicit operator choice rather than
		// a silent default.
		return nil, nil //nolint:nilnil // nil TLS config is pgx's documented "no TLS"
	}

	serverName := settings.GetServerName()
	if serverName == "" {
		serverName = host
	}

	cfg := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		//nolint:gosec // G402: operator-controlled, warned about by core, and never the default
		InsecureSkipVerify: settings.GetInsecureSkipVerify(),
	}

	if pem := settings.GetCaPem(); len(pem) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, sdk.InvalidArgument("tls: ca_pem contains no usable certificate")
		}
		cfg.RootCAs = pool
	}

	certPEM, keyPEM := settings.GetClientCertPem(), settings.GetClientKeyPem()
	switch {
	case len(certPEM) > 0 && len(keyPEM) > 0:
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			// The error from X509KeyPair describes the parse failure and contains no key material.
			return nil, sdk.InvalidArgument("tls: client certificate and key do not form a valid pair: %v", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case len(certPEM) > 0 || len(keyPEM) > 0:
		return nil, sdk.InvalidArgument("tls: client certificate and key must be supplied together")
	}

	return cfg, nil
}

// classifyConnectError turns a pgx failure into a typed plugin error core can act on.
//
// The distinction that matters most is authentication versus unreachable. A wrong password stays
// wrong, so retrying only burns attempts and can trip account lockout on the monitored instance; an
// unreachable host is the most common transient failure in an estate and is worth retrying.
//
// The underlying error is attached with WithCause, which keeps it local for logging. Only the
// message crosses the process boundary, because a pgx error can carry the connection string.
func classifyConnectError(err error, creds *fwv1.Credentials) error {
	target := fmt.Sprintf("%s:%d", creds.GetHost(), portOrDefault(creds.GetPort()))

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		// Class 28 — invalid authorization specification.
		case "28P01", "28000":
			return sdk.AuthenticationFailed("authentication failed for user %q on %s",
				creds.GetUsername(), target).WithCause(err)
		// Class 3D — invalid catalog name.
		case "3D000":
			return sdk.InvalidArgument("database %q does not exist on %s",
				creds.GetDatabase(), target).WithCause(err)
		// Class 53 — insufficient resources.
		case "53300":
			return sdk.ConnectionFailed("%s has too many connections", target).WithCause(err)
		case "42501":
			return sdk.PermissionDenied("insufficient privilege on %s", target).WithCause(err)
		}
		return sdk.ConnectionFailed("connect to %s: %s", target, pgErr.Code).WithCause(err)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return sdk.ConnectionFailed("connecting to %s timed out", target).WithCause(err)
	}
	if errors.Is(err, context.Canceled) {
		return sdk.NewError(fwv1.ErrorCode_ERROR_CODE_CANCELED, "connection to %s canceled", target).WithCause(err)
	}

	return sdk.ConnectionFailed("cannot reach %s", target).WithCause(err)
}

func portOrDefault(port int32) int32 {
	if port == 0 {
		return 5432
	}
	return port
}
