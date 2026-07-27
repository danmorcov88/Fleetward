// Package config loads Fleetward's runtime configuration from the environment.
//
// Every setting is read from an environment variable prefixed FLEETWARD_, which keeps the twelve-
// factor deployment story simple and means no configuration file has to be mounted for the
// docker-compose quickstart to work.
//
// Two rules hold throughout this package:
//
//   - Secrets are never rendered by String, LogValue, or any error message (ADR-0009).
//   - Every listener accepts TLS configuration, even where development defaults leave it disabled.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const envPrefix = "FLEETWARD_"

// Environment distinguishes development conveniences from production defaults.
type Environment string

const (
	// EnvDevelopment permits conveniences such as disabled authentication and self-signed TLS.
	EnvDevelopment Environment = "development"
	// EnvProduction refuses to start with any of those conveniences enabled.
	EnvProduction Environment = "production"
)

// Config is the complete control-plane configuration.
type Config struct {
	Environment Environment
	Log         LogConfig
	HTTP        ServerConfig
	GRPC        ServerConfig
	MetaDB      MetaDBConfig
	TSDB        TSDBConfig
	ObjStore    ObjStoreConfig
	Secrets     SecretsConfig
	Plugins     PluginsConfig
	Auth        AuthConfig
	Telemetry   TelemetryConfig
	Scheduler   SchedulerConfig
	Sandbox     SandboxConfig
}

// LogConfig controls the slog handler (ADR-0014).
type LogConfig struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is "json" for production pipelines or "text" for a readable development console.
	Format string
	// AddSource includes file and line in every record. Useful in development, noisy in production.
	AddSource bool
}

// ServerConfig describes one listener. TLS fields are always present so that enabling transport
// security is a configuration change rather than a code change.
type ServerConfig struct {
	Addr            string
	TLSEnabled      bool
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// MetaDBConfig configures the PostgreSQL metadata store (ADR-0005).
type MetaDBConfig struct {
	// DSN is a libpq-style connection string. It contains a password and must never be logged.
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	ConnectTimeout  time.Duration
	// AutoMigrate runs pending migrations at startup. Convenient in development; in production most
	// operators prefer to run migrations as a deliberate, separate step.
	AutoMigrate bool
}

// TSDBConfig configures the VictoriaMetrics client (ADR-0006).
type TSDBConfig struct {
	// RemoteWriteURL receives Prometheus remote_write payloads.
	RemoteWriteURL string
	// QueryURL serves the Prometheus-compatible query API.
	QueryURL string
	Timeout  time.Duration
	// Username and Password authenticate to VictoriaMetrics when it sits behind basic auth.
	Username string
	Password string
}

// ObjStoreConfig configures S3-compatible storage for backup artifacts (ADR-0007).
type ObjStoreConfig struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// PresignTTL bounds how long a plugin's upload or download grant stays valid.
	PresignTTL time.Duration
	// PartSizeBytes is the multipart chunk size used for large artifacts.
	PartSizeBytes int64
}

// SecretsConfig selects and configures the SecretsProvider (ADR-0009).
type SecretsConfig struct {
	// Provider is "aesgcm" for the MVP provider or "vault" once that lands.
	Provider string
	// MasterKey is a base64-encoded 32-byte key for the aesgcm provider. Prefer MasterKeyFile:
	// an environment variable is visible to anything that can read /proc.
	MasterKey string
	// MasterKeyFile points at a mounted file containing the base64-encoded key.
	MasterKeyFile string
}

// PluginsConfig controls how the plugin manager finds and supervises engine plugins (ADR-0003).
type PluginsConfig struct {
	// Dir holds the plugin binaries, named fleetward-plugin-<engine>.
	Dir string
	// HandshakeTimeout bounds how long a plugin may take to become usable after launch.
	HandshakeTimeout time.Duration
	// RestartBackoffMin and RestartBackoffMax bound the exponential backoff between restarts of a
	// crashing plugin.
	RestartBackoffMin time.Duration
	RestartBackoffMax time.Duration
	// MaxRestarts caps consecutive restart attempts before the plugin is marked failed. Zero means
	// keep retrying at the maximum backoff forever.
	MaxRestarts int
	// HealthInterval is how often the manager probes a running plugin.
	HealthInterval time.Duration
}

// AuthConfig configures OIDC authentication (ADR-0008).
type AuthConfig struct {
	// Enabled may be false only in development. The server refuses to start in production with
	// authentication disabled.
	Enabled      bool
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// UsernameClaim and GroupsClaim map ID token claims onto Fleetward principals.
	UsernameClaim string
	GroupsClaim   string
	// SkipIssuerVerification tolerates a development IdP whose external and internal issuer URLs
	// differ, as Dex does in docker-compose.
	SkipIssuerVerification bool
	SessionTTL             time.Duration
}

// TelemetryConfig configures Fleetward's own observability (ADR-0011).
type TelemetryConfig struct {
	Enabled      bool
	OTLPEndpoint string
	OTLPInsecure bool
	ServiceName  string
	SampleRatio  float64
}

// SchedulerConfig tunes the job scheduler (ADR-0013).
type SchedulerConfig struct {
	Enabled bool
	// LeaseTTL is how long a claimed job stays claimed without a heartbeat. It must comfortably
	// exceed the heartbeat interval, never the job duration: long jobs renew rather than hold.
	LeaseTTL time.Duration
	// LeaseHeartbeat is how often a running job renews its lease.
	LeaseHeartbeat time.Duration
	// PollInterval is how often a runner looks for claimable work.
	PollInterval time.Duration
	// MaxConcurrentJobs caps jobs running on this control-plane instance.
	MaxConcurrentJobs int
}

// SandboxConfig configures the ephemeral containers used for backup verification.
type SandboxConfig struct {
	// Provider is "docker" in the MVP; "kubernetes" is the planned second implementation.
	Provider   string
	DockerHost string
	// StartupTimeout bounds how long a sandbox may take to become ready before verification is
	// reported inconclusive.
	StartupTimeout time.Duration
	// MaxLifetime is a hard ceiling after which a sandbox is destroyed regardless of state. It is
	// the backstop behind the normal deferred cleanup, not a replacement for it.
	MaxLifetime time.Duration
	// Network attaches sandboxes to a specific Docker network. Empty uses the default bridge.
	Network string
	// LabelPrefix marks containers Fleetward owns, so an orphan sweep can find them after a crash.
	LabelPrefix string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		Environment: Environment(env("ENV", string(EnvDevelopment))),
		Log: LogConfig{
			Level:     env("LOG_LEVEL", "info"),
			Format:    env("LOG_FORMAT", "text"),
			AddSource: envBool("LOG_ADD_SOURCE", false),
		},
		HTTP: ServerConfig{
			Addr:            env("HTTP_ADDR", ":8080"),
			TLSEnabled:      envBool("HTTP_TLS_ENABLED", false),
			TLSCertFile:     env("HTTP_TLS_CERT_FILE", ""),
			TLSKeyFile:      env("HTTP_TLS_KEY_FILE", ""),
			TLSClientCAFile: env("HTTP_TLS_CLIENT_CA_FILE", ""),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 60*time.Second),
			IdleTimeout:     envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		},
		GRPC: ServerConfig{
			Addr:            env("GRPC_ADDR", ":9090"),
			TLSEnabled:      envBool("GRPC_TLS_ENABLED", false),
			TLSCertFile:     env("GRPC_TLS_CERT_FILE", ""),
			TLSKeyFile:      env("GRPC_TLS_KEY_FILE", ""),
			TLSClientCAFile: env("GRPC_TLS_CLIENT_CA_FILE", ""),
			ShutdownTimeout: envDuration("GRPC_SHUTDOWN_TIMEOUT", 20*time.Second),
		},
		MetaDB: MetaDBConfig{
			DSN:             env("METADB_DSN", "postgres://fleetward:fleetward@localhost:5432/fleetward?sslmode=disable"),
			MaxConns:        envInt32("METADB_MAX_CONNS", 20),
			MinConns:        envInt32("METADB_MIN_CONNS", 2),
			MaxConnLifetime: envDuration("METADB_MAX_CONN_LIFETIME", time.Hour),
			ConnectTimeout:  envDuration("METADB_CONNECT_TIMEOUT", 10*time.Second),
			AutoMigrate:     envBool("METADB_AUTO_MIGRATE", true),
		},
		TSDB: TSDBConfig{
			RemoteWriteURL: env("TSDB_REMOTE_WRITE_URL", "http://localhost:8428/api/v1/write"),
			QueryURL:       env("TSDB_QUERY_URL", "http://localhost:8428"),
			Timeout:        envDuration("TSDB_TIMEOUT", 30*time.Second),
			Username:       env("TSDB_USERNAME", ""),
			Password:       env("TSDB_PASSWORD", ""),
		},
		ObjStore: ObjStoreConfig{
			Endpoint:      env("OBJSTORE_ENDPOINT", "localhost:9000"),
			Region:        env("OBJSTORE_REGION", "us-east-1"),
			Bucket:        env("OBJSTORE_BUCKET", "fleetward-backups"),
			AccessKey:     env("OBJSTORE_ACCESS_KEY", ""),
			SecretKey:     env("OBJSTORE_SECRET_KEY", ""),
			UseSSL:        envBool("OBJSTORE_USE_SSL", false),
			PresignTTL:    envDuration("OBJSTORE_PRESIGN_TTL", 6*time.Hour),
			PartSizeBytes: int64(envInt("OBJSTORE_PART_SIZE_BYTES", 64<<20)),
		},
		Secrets: SecretsConfig{
			Provider:      env("SECRETS_PROVIDER", "aesgcm"),
			MasterKey:     env("SECRETS_MASTER_KEY", ""),
			MasterKeyFile: env("SECRETS_MASTER_KEY_FILE", ""),
		},
		Plugins: PluginsConfig{
			Dir:               env("PLUGINS_DIR", "./bin/plugins"),
			HandshakeTimeout:  envDuration("PLUGINS_HANDSHAKE_TIMEOUT", 30*time.Second),
			RestartBackoffMin: envDuration("PLUGINS_RESTART_BACKOFF_MIN", time.Second),
			RestartBackoffMax: envDuration("PLUGINS_RESTART_BACKOFF_MAX", 5*time.Minute),
			MaxRestarts:       envInt("PLUGINS_MAX_RESTARTS", 0),
			HealthInterval:    envDuration("PLUGINS_HEALTH_INTERVAL", 30*time.Second),
		},
		Auth: AuthConfig{
			Enabled:                envBool("AUTH_ENABLED", false),
			IssuerURL:              env("AUTH_ISSUER_URL", ""),
			ClientID:               env("AUTH_CLIENT_ID", "fleetward"),
			ClientSecret:           env("AUTH_CLIENT_SECRET", ""),
			RedirectURL:            env("AUTH_REDIRECT_URL", "http://localhost:8080/api/v1/auth/callback"),
			Scopes:                 envList("AUTH_SCOPES", []string{"openid", "profile", "email", "groups"}),
			UsernameClaim:          env("AUTH_USERNAME_CLAIM", "email"),
			GroupsClaim:            env("AUTH_GROUPS_CLAIM", "groups"),
			SkipIssuerVerification: envBool("AUTH_SKIP_ISSUER_VERIFICATION", false),
			SessionTTL:             envDuration("AUTH_SESSION_TTL", 12*time.Hour),
		},
		Telemetry: TelemetryConfig{
			Enabled:      envBool("TELEMETRY_ENABLED", false),
			OTLPEndpoint: env("TELEMETRY_OTLP_ENDPOINT", "localhost:4317"),
			OTLPInsecure: envBool("TELEMETRY_OTLP_INSECURE", true),
			ServiceName:  env("TELEMETRY_SERVICE_NAME", "fleetward"),
			SampleRatio:  envFloat("TELEMETRY_SAMPLE_RATIO", 1.0),
		},
		Scheduler: SchedulerConfig{
			Enabled:           envBool("SCHEDULER_ENABLED", true),
			LeaseTTL:          envDuration("SCHEDULER_LEASE_TTL", 2*time.Minute),
			LeaseHeartbeat:    envDuration("SCHEDULER_LEASE_HEARTBEAT", 30*time.Second),
			PollInterval:      envDuration("SCHEDULER_POLL_INTERVAL", 10*time.Second),
			MaxConcurrentJobs: envInt("SCHEDULER_MAX_CONCURRENT_JOBS", 4),
		},
		Sandbox: SandboxConfig{
			Provider:       env("SANDBOX_PROVIDER", "docker"),
			DockerHost:     env("SANDBOX_DOCKER_HOST", ""),
			StartupTimeout: envDuration("SANDBOX_STARTUP_TIMEOUT", 3*time.Minute),
			MaxLifetime:    envDuration("SANDBOX_MAX_LIFETIME", 2*time.Hour),
			Network:        env("SANDBOX_NETWORK", ""),
			LabelPrefix:    env("SANDBOX_LABEL_PREFIX", "fleetward"),
		},
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate reports every configuration problem at once, so an operator fixing a misconfigured
// deployment does not have to restart repeatedly to discover them one at a time.
func (c *Config) Validate() error {
	var errs []error

	switch c.Environment {
	case EnvDevelopment, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("%sENV: must be %q or %q, got %q",
			envPrefix, EnvDevelopment, EnvProduction, c.Environment))
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("%sLOG_LEVEL: must be debug, info, warn or error, got %q",
			envPrefix, c.Log.Level))
	}

	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("%sLOG_FORMAT: must be json or text, got %q", envPrefix, c.Log.Format))
	}

	if c.MetaDB.DSN == "" {
		errs = append(errs, fmt.Errorf("%sMETADB_DSN: required", envPrefix))
	}

	errs = append(errs, validateTLS("HTTP", c.HTTP)...)
	errs = append(errs, validateTLS("GRPC", c.GRPC)...)

	if c.Secrets.Provider == "aesgcm" && c.Secrets.MasterKey == "" && c.Secrets.MasterKeyFile == "" {
		errs = append(errs, fmt.Errorf(
			"%sSECRETS_MASTER_KEY or %sSECRETS_MASTER_KEY_FILE: required for the aesgcm secrets provider",
			envPrefix, envPrefix))
	}

	if c.Scheduler.Enabled && c.Scheduler.LeaseHeartbeat >= c.Scheduler.LeaseTTL {
		errs = append(errs, fmt.Errorf(
			"%sSCHEDULER_LEASE_HEARTBEAT (%s) must be shorter than %sSCHEDULER_LEASE_TTL (%s), "+
				"otherwise a running job's lease expires before it is renewed and the job can be claimed twice",
			envPrefix, c.Scheduler.LeaseHeartbeat, envPrefix, c.Scheduler.LeaseTTL))
	}

	// Production must not run with the development shortcuts that make the quickstart pleasant.
	if c.Environment == EnvProduction {
		if !c.Auth.Enabled {
			errs = append(errs, fmt.Errorf(
				"%sAUTH_ENABLED: authentication cannot be disabled in production", envPrefix))
		}
		if c.Auth.SkipIssuerVerification {
			errs = append(errs, fmt.Errorf(
				"%sAUTH_SKIP_ISSUER_VERIFICATION: cannot be enabled in production", envPrefix))
		}
	}

	if c.Auth.Enabled && c.Auth.IssuerURL == "" {
		errs = append(errs, fmt.Errorf("%sAUTH_ISSUER_URL: required when authentication is enabled", envPrefix))
	}

	return errors.Join(errs...)
}

func validateTLS(name string, s ServerConfig) []error {
	if !s.TLSEnabled {
		return nil
	}
	var errs []error
	if s.TLSCertFile == "" {
		errs = append(errs, fmt.Errorf("%s%s_TLS_CERT_FILE: required when TLS is enabled", envPrefix, name))
	}
	if s.TLSKeyFile == "" {
		errs = append(errs, fmt.Errorf("%s%s_TLS_KEY_FILE: required when TLS is enabled", envPrefix, name))
	}
	return errs
}

// IsDevelopment reports whether development conveniences are permitted.
func (c *Config) IsDevelopment() bool { return c.Environment == EnvDevelopment }

// LogValue renders the configuration for logging with every secret redacted. Implementing
// slog.LogValuer means a stray `slog.Any("config", cfg)` cannot leak a password (ADR-0014).
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("environment", string(c.Environment)),
		slog.String("log_level", c.Log.Level),
		slog.String("log_format", c.Log.Format),
		slog.String("http_addr", c.HTTP.Addr),
		slog.Bool("http_tls", c.HTTP.TLSEnabled),
		slog.String("grpc_addr", c.GRPC.Addr),
		slog.Bool("grpc_tls", c.GRPC.TLSEnabled),
		slog.String("metadb_dsn", redactDSN(c.MetaDB.DSN)),
		slog.String("tsdb_remote_write_url", c.TSDB.RemoteWriteURL),
		slog.String("objstore_endpoint", c.ObjStore.Endpoint),
		slog.String("objstore_bucket", c.ObjStore.Bucket),
		slog.String("secrets_provider", c.Secrets.Provider),
		slog.String("plugins_dir", c.Plugins.Dir),
		slog.Bool("auth_enabled", c.Auth.Enabled),
		slog.String("auth_issuer_url", c.Auth.IssuerURL),
		slog.Bool("scheduler_enabled", c.Scheduler.Enabled),
		slog.String("sandbox_provider", c.Sandbox.Provider),
	)
}

// redactDSN strips the password from a URL-style DSN so a connection string can be logged for
// troubleshooting without exposing the credential inside it.
func redactDSN(dsn string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return "[redacted]"
	}
	userinfo, hostpart, ok := strings.Cut(rest, "@")
	if !ok {
		return dsn
	}
	user, _, hasPassword := strings.Cut(userinfo, ":")
	if !hasPassword {
		return dsn
	}
	return scheme + "://" + user + ":[redacted]@" + hostpart
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(envPrefix + key); ok && v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(envPrefix + key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(envPrefix + key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// envInt32 reads a bounded integer. Pool sizes are int32 in pgx, and a value outside that range
// almost certainly means a typo rather than an intent, so it falls back rather than wrapping.
func envInt32(key string, fallback int32) int32 {
	v := envInt(key, int(fallback))
	if v < 0 || v > math.MaxInt32 {
		return fallback
	}
	return int32(v)
}

func envFloat(key string, fallback float64) float64 {
	v, ok := os.LookupEnv(envPrefix + key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(envPrefix + key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(envPrefix + key)
	if !ok || v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
