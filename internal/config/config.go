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
	Retention   RetentionConfig
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
	// SessionTTL bounds the cookie the UI holds after exchanging a token for one.
	SessionTTL time.Duration

	// BootstrapToken is the break-glass credential that gets the first real token out of a fresh
	// installation. It is configuration and never a database row, so removing the setting removes
	// the access and leaves nothing behind to find later (ADR-0033). The file form is preferred for
	// the reason `fleetward-cli keygen` already gives about the master key.
	BootstrapToken     string
	BootstrapTokenFile string

	// SessionKey signs the session cookie. When neither form is set the control plane generates one
	// at startup, which means a restart signs everybody out — the right default for a single node,
	// and the reason a multi-replica installation configures it.
	SessionKey     string
	SessionKeyFile string

	// PrincipalCacheTTL bounds how long a verified credential is reused without going back to the
	// database, and therefore how long a revoked one keeps working on a replica that did not
	// perform the revocation. Deliberately short rather than convenient: the alternative is a join
	// across three tables on every request of a dashboard that refetches every thirty seconds.
	PrincipalCacheTTL time.Duration
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

// RetentionConfig tunes the retention sweep (ADR-0030).
//
// This is the only configuration in Fleetward that governs destroying something, so every field is
// a limit rather than a feature, and two of them refuse values that would remove a limit entirely.
type RetentionConfig struct {
	// Enabled turns the sweep on. False leaves artifacts in place forever, which is what every
	// version before this one did, and is a legitimate configuration for an operator who wants to
	// watch `backup retention` for a while before trusting it.
	Enabled bool
	// Interval paces the sweep. It is deliberately far longer than the scheduler's poll interval:
	// the sweep is estate-wide and destructive, and there is no reason for an artifact to be removed
	// within seconds of its expiry rather than within the hour.
	Interval time.Duration
	// MinKeep is how many recent successful backups of an instance are kept whatever their expiry
	// says. Its floor is 1 and zero is refused: retention that can delete an instance's last
	// working backup is the most damaging thing this product could do, and it is the ordinary
	// consequence of a correct implementation of "delete anything older than N days" (ADR-0032).
	MinKeep int
	// MaxPerSweep bounds how many artifacts one sweep removes. It exists so that a bug is bounded,
	// not because the query is slow: a destructive loop with no ceiling is the wrong shape however
	// correct it looks. What it does not remove this hour it removes next hour.
	MaxPerSweep int
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
	// SharedDirVolume is the name of a Docker volume to mount into a sandbox whose plugin asks for
	// a shared directory (ADR-0026). It is what makes that directory work when the control plane
	// itself runs in a container: a bind mount's source path is resolved by the daemon against its
	// own filesystem rather than against this process's, so a path inside our container would
	// silently mount the wrong thing. Empty falls back to binding a temporary directory, which is
	// correct when the control plane runs directly on the daemon's host.
	SharedDirVolume string
	// SharedDirLocal is where that same volume is mounted in this process's own filesystem. The two
	// are required together: one without the other describes a directory only one side can reach,
	// which is the failure this whole mechanism exists to avoid.
	SharedDirLocal string
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
			BootstrapToken:         env("AUTH_BOOTSTRAP_TOKEN", ""),
			BootstrapTokenFile:     env("AUTH_BOOTSTRAP_TOKEN_FILE", ""),
			SessionKey:             env("AUTH_SESSION_KEY", ""),
			SessionKeyFile:         env("AUTH_SESSION_KEY_FILE", ""),
			PrincipalCacheTTL:      envDuration("AUTH_PRINCIPAL_CACHE_TTL", 15*time.Second),
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
		Retention: RetentionConfig{
			Enabled:     envBool("RETENTION_ENABLED", true),
			Interval:    envDuration("RETENTION_INTERVAL", time.Hour),
			MinKeep:     envInt("RETENTION_MIN_KEEP", 1),
			MaxPerSweep: envInt("RETENTION_MAX_PER_SWEEP", 500),
		},
		Sandbox: SandboxConfig{
			Provider:       env("SANDBOX_PROVIDER", "docker"),
			DockerHost:     env("SANDBOX_DOCKER_HOST", ""),
			StartupTimeout: envDuration("SANDBOX_STARTUP_TIMEOUT", 3*time.Minute),
			MaxLifetime:    envDuration("SANDBOX_MAX_LIFETIME", 2*time.Hour),
			Network:        env("SANDBOX_NETWORK", ""),
			LabelPrefix:    env("SANDBOX_LABEL_PREFIX", "fleetward"),

			SharedDirVolume: env("SANDBOX_SHARED_DIR_VOLUME", ""),
			SharedDirLocal:  env("SANDBOX_SHARED_DIR_LOCAL", ""),
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

	// The three refusals below are the reason retention is safe to leave on. Each removes a way to
	// configure the sweep into something unbounded, and they are checked even when the sweep is
	// disabled: a value that would be dangerous when someone turns it on should be rejected when
	// they write it, not months later when they flip the switch.
	if c.Retention.MinKeep < 1 {
		errs = append(errs, fmt.Errorf(
			"%sRETENTION_MIN_KEEP (%d): must be at least 1. Retention that may delete an instance's "+
				"last successful backup is the most damaging thing Fleetward can do, and it is the "+
				"ordinary result of a correct \"delete anything older than N days\"; the floor is not "+
				"optional",
			envPrefix, c.Retention.MinKeep))
	}
	if c.Retention.MaxPerSweep < 1 {
		errs = append(errs, fmt.Errorf(
			"%sRETENTION_MAX_PER_SWEEP (%d): must be at least 1; it bounds how many artifacts one "+
				"sweep may delete, and an unbounded destructive loop is the wrong shape however "+
				"correct it looks",
			envPrefix, c.Retention.MaxPerSweep))
	}
	if c.Retention.Interval <= 0 {
		errs = append(errs, fmt.Errorf(
			"%sRETENTION_INTERVAL (%s): must be positive; zero would run the sweep on every "+
				"scheduler tick",
			envPrefix, c.Retention.Interval))
	}

	if (c.Sandbox.SharedDirVolume == "") != (c.Sandbox.SharedDirLocal == "") {
		errs = append(errs, fmt.Errorf(
			"%sSANDBOX_SHARED_DIR_VOLUME and %sSANDBOX_SHARED_DIR_LOCAL must be set together: "+
				"one names the volume a sandbox mounts and the other says where this process sees "+
				"the same volume, and half of that describes a directory only one side can reach",
			envPrefix, envPrefix))
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

	// AUTH_ISSUER_URL is required only once there is an identity provider to talk to. Until B10
	// swaps OIDC in behind the spine, authentication is bearer tokens and sessions, and demanding an
	// issuer URL for those would be demanding configuration for a component that does not run.
	if c.Auth.Enabled && c.Auth.IssuerURL == "" && c.Environment == EnvProduction {
		errs = append(errs, fmt.Errorf(
			"%sAUTH_ISSUER_URL: required in production; until OIDC lands it may be a placeholder, "+
				"but an installation with no configured issuer has nothing to point at", envPrefix))
	}

	if c.Auth.PrincipalCacheTTL < 0 {
		errs = append(errs, fmt.Errorf("%sAUTH_PRINCIPAL_CACHE_TTL: cannot be negative", envPrefix))
	}
	// A cache longer than the session is a revoked credential that outlives the session it could
	// have been used to create, which is the wrong way round.
	if c.Auth.PrincipalCacheTTL > 5*time.Minute {
		errs = append(errs, fmt.Errorf(
			"%sAUTH_PRINCIPAL_CACHE_TTL: %s is longer than five minutes, which is how long a "+
				"revoked credential would keep working", envPrefix, c.Auth.PrincipalCacheTTL))
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
		// Whether a break-glass credential is configured, never what it is.
		slog.Bool("auth_bootstrap_token_set", c.Auth.BootstrapToken != "" || c.Auth.BootstrapTokenFile != ""),
		slog.Bool("scheduler_enabled", c.Scheduler.Enabled),
		slog.Bool("retention_enabled", c.Retention.Enabled),
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
