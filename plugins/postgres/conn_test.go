package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const testPassword = "sup3r-s3cret-p@ssw0rd"

func testCreds() *fwv1.Credentials {
	return &fwv1.Credentials{
		Host:     "db.example.internal",
		Port:     5432,
		Username: "fleetward",
		Password: testPassword,
		Database: "app",
	}
}

func TestConnConfig(t *testing.T) {
	tests := []struct {
		name    string
		creds   *fwv1.Credentials
		wantErr bool
		check   func(t *testing.T, cfg connCheck)
	}{
		{
			name:  "full credentials",
			creds: testCreds(),
			check: func(t *testing.T, cfg connCheck) {
				if cfg.host != "db.example.internal" {
					t.Errorf("host = %q", cfg.host)
				}
				if cfg.port != 5432 {
					t.Errorf("port = %d", cfg.port)
				}
				if cfg.database != "app" {
					t.Errorf("database = %q", cfg.database)
				}
			},
		},
		{
			name: "port defaults to 5432",
			creds: &fwv1.Credentials{
				Host: "h", Username: "u", Password: "p", Database: "d",
			},
			check: func(t *testing.T, cfg connCheck) {
				if cfg.port != 5432 {
					t.Errorf("port = %d, want the PostgreSQL default 5432", cfg.port)
				}
			},
		},
		{
			name: "database defaults to postgres",
			creds: &fwv1.Credentials{
				Host: "h", Username: "u", Password: "p",
			},
			check: func(t *testing.T, cfg connCheck) {
				// Discovery runs before anything is known about the instance, so it must be able
				// to connect without being told a database name.
				if cfg.database != "postgres" {
					t.Errorf("database = %q, want the always-present 'postgres'", cfg.database)
				}
			},
		},
		{"nil credentials", nil, true, nil},
		{"missing host", &fwv1.Credentials{Username: "u"}, true, nil},
		{"port too high", &fwv1.Credentials{Host: "h", Port: 70000}, true, nil},
		{"negative port", &fwv1.Credentials{Host: "h", Port: -1}, true, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := connConfig(tc.creds)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("connConfig: %v", err)
			}
			if tc.check != nil {
				tc.check(t, connCheck{
					host:     cfg.Host,
					port:     cfg.Port,
					database: cfg.Database,
				})
			}
		})
	}
}

type connCheck struct {
	host     string
	port     uint16
	database string
}

func TestConnConfigIdentifiesFleetward(t *testing.T) {
	cfg, err := connConfig(testCreds())
	if err != nil {
		t.Fatalf("connConfig: %v", err)
	}
	// A DBA reading pg_stat_activity on a monitored instance must be able to tell which
	// connections are ours. An unexplained connection is a support ticket.
	if got := cfg.RuntimeParams["application_name"]; got != "fleetward" {
		t.Errorf("application_name = %q, want %q", got, "fleetward")
	}
}

func TestConnConfigOptionsCannotOverrideIdentity(t *testing.T) {
	creds := testCreds()
	creds.Options = map[string]string{"application_name": "something-else"}

	cfg, err := connConfig(creds)
	if err != nil {
		t.Fatalf("connConfig: %v", err)
	}
	if got := cfg.RuntimeParams["application_name"]; got != "fleetward" {
		t.Errorf("application_name = %q; identifying our own connections is not overridable", got)
	}
}

func TestConnConfigPassesThroughOptions(t *testing.T) {
	creds := testCreds()
	creds.Options = map[string]string{"search_path": "public,app"}

	cfg, err := connConfig(creds)
	if err != nil {
		t.Fatalf("connConfig: %v", err)
	}
	if got := cfg.RuntimeParams["search_path"]; got != "public,app" {
		t.Errorf("search_path = %q, want %q", got, "public,app")
	}
}

func TestConnConfigRejectsBadConnectTimeout(t *testing.T) {
	for _, value := range []string{"0", "-5", "abc", ""} {
		creds := testCreds()
		creds.Options = map[string]string{"connect_timeout": value}
		if _, err := connConfig(creds); err == nil {
			t.Errorf("connect_timeout=%q: expected an error, got nil", value)
		}
	}
}

func TestBuildTLS(t *testing.T) {
	tests := []struct {
		name     string
		settings *fwv1.TLSSettings
		wantNil  bool
		wantErr  bool
	}{
		{"disabled yields no TLS", &fwv1.TLSSettings{Enabled: false}, true, false},
		{"nil yields no TLS", nil, true, false},
		{"enabled", &fwv1.TLSSettings{Enabled: true}, false, false},
		{
			name:     "garbage CA",
			settings: &fwv1.TLSSettings{Enabled: true, CaPem: []byte("not a certificate")},
			wantErr:  true,
		},
		{
			name:     "client cert without key",
			settings: &fwv1.TLSSettings{Enabled: true, ClientCertPem: []byte("cert")},
			wantErr:  true,
		},
		{
			name:     "client key without cert",
			settings: &fwv1.TLSSettings{Enabled: true, ClientKeyPem: []byte("key")},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := buildTLS(tc.settings, "db.example.internal")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildTLS: %v", err)
			}
			if tc.wantNil {
				if cfg != nil {
					t.Fatal("expected nil TLS config when TLS is disabled")
				}
				return
			}
			if cfg == nil {
				t.Fatal("expected a TLS config")
			}
			if cfg.MinVersion != 0x0303 { // tls.VersionTLS12
				t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
			}
			if cfg.ServerName != "db.example.internal" {
				t.Errorf("ServerName = %q, want the host when none is given", cfg.ServerName)
			}
		})
	}
}

func TestBuildTLSServerNameOverride(t *testing.T) {
	cfg, err := buildTLS(&fwv1.TLSSettings{Enabled: true, ServerName: "cert-name"}, "connect-host")
	if err != nil {
		t.Fatalf("buildTLS: %v", err)
	}
	if cfg.ServerName != "cert-name" {
		t.Errorf("ServerName = %q, want the explicit override", cfg.ServerName)
	}
}

func TestClassifyConnectError(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCode      fwv1.ErrorCode
		wantRetryable bool
	}{
		{
			name:     "wrong password is not retryable",
			err:      &pgconn.PgError{Code: "28P01"},
			wantCode: fwv1.ErrorCode_ERROR_CODE_AUTHENTICATION_FAILED,
			// Deliberately not retryable: the same wrong password stays wrong, and repeated
			// attempts can trip account lockout on the monitored instance.
			wantRetryable: false,
		},
		{
			name:          "missing database",
			err:           &pgconn.PgError{Code: "3D000"},
			wantCode:      fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
			wantRetryable: false,
		},
		{
			name:     "too many connections is retryable",
			err:      &pgconn.PgError{Code: "53300"},
			wantCode: fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
			// Transient by nature — connections free up.
			wantRetryable: true,
		},
		{
			name:          "insufficient privilege",
			err:           &pgconn.PgError{Code: "42501"},
			wantCode:      fwv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
			wantRetryable: false,
		},
		{
			name:          "timeout is retryable",
			err:           context.DeadlineExceeded,
			wantCode:      fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
			wantRetryable: true,
		},
		{
			name:          "cancellation",
			err:           context.Canceled,
			wantCode:      fwv1.ErrorCode_ERROR_CODE_CANCELED,
			wantRetryable: false,
		},
		{
			name:          "unreachable host is retryable",
			err:           errors.New("dial tcp: connection refused"),
			wantCode:      fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
			wantRetryable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyConnectError(tc.err, testCreds())

			pe := sdk.AsPluginError(got)
			if pe.GetCode() != tc.wantCode {
				t.Errorf("code = %v, want %v", pe.GetCode(), tc.wantCode)
			}
			if pe.GetRetryable() != tc.wantRetryable {
				t.Errorf("retryable = %v, want %v", pe.GetRetryable(), tc.wantRetryable)
			}
		})
	}
}

// TestConnectErrorsNeverLeakThePassword is the test this file exists for.
//
// A pgx error can carry the connection string, and a plugin error crosses the process boundary into
// core's logs. Anything that reaches the wire must be free of credentials (ADR-0009).
func TestConnectErrorsNeverLeakThePassword(t *testing.T) {
	creds := testCreds()

	underlying := []error{
		&pgconn.PgError{Code: "28P01", Message: "password authentication failed for user \"fleetward\""},
		errors.New(`failed to connect to host=db.example.internal user=fleetward password=` + testPassword),
		context.DeadlineExceeded,
	}

	for _, cause := range underlying {
		err := classifyConnectError(cause, creds)

		// The wire representation is what core logs and what the UI may surface.
		wire := sdk.AsPluginError(err)
		if strings.Contains(wire.GetMessage(), testPassword) {
			t.Fatalf("password leaked into the wire message: %q", wire.GetMessage())
		}
		for k, v := range wire.GetDetails() {
			if strings.Contains(v, testPassword) {
				t.Fatalf("password leaked into detail %q: %q", k, v)
			}
		}
	}
}

func TestConnConfigDoesNotBuildADSN(t *testing.T) {
	// Building a config field by field rather than formatting a DSN is what keeps the password out
	// of error messages. If ConnString() ever becomes populated, that protection is gone.
	cfg, err := connConfig(testCreds())
	if err != nil {
		t.Fatalf("connConfig: %v", err)
	}
	if strings.Contains(cfg.ConnString(), testPassword) {
		t.Fatal("the connection config carries a DSN containing the password")
	}
}

func TestPortOrDefault(t *testing.T) {
	if got := portOrDefault(0); got != 5432 {
		t.Errorf("portOrDefault(0) = %d, want 5432", got)
	}
	if got := portOrDefault(6432); got != 6432 {
		t.Errorf("portOrDefault(6432) = %d, want 6432", got)
	}
}
