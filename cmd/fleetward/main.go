// Command fleetward is the Fleetward control plane.
//
// It owns the metadata store, the plugin manager, the scheduler, and the API surface. Configuration
// comes entirely from FLEETWARD_-prefixed environment variables; see internal/config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	// The time zone database, embedded in the binary. Schedules fire in the timezone a DBA wrote
	// them in, which means time.LoadLocation has to work — and Go reads the zone database from the
	// operating system, which the runtime image (debian:trixie-slim) does not ship. Without this
	// import every schedule with a real timezone works on a developer's machine, where a Go
	// installation supplies the database, and fails inside the container. It costs about 450 KB and
	// cannot drift from the image's package set.
	_ "time/tzdata"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/alerts"
	"github.com/danmorcov88/fleetward/internal/controlplane/api"
	"github.com/danmorcov88/fleetward/internal/controlplane/audit"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/controlplane/authz"
	"github.com/danmorcov88/fleetward/internal/controlplane/backup"
	"github.com/danmorcov88/fleetward/internal/controlplane/identity"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
	"github.com/danmorcov88/fleetward/internal/controlplane/sandbox"
	"github.com/danmorcov88/fleetward/internal/controlplane/scheduler"
	"github.com/danmorcov88/fleetward/internal/plugin/manager"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
	"github.com/danmorcov88/fleetward/internal/storage/tsdb"
	"github.com/danmorcov88/fleetward/internal/telemetry"
	"github.com/danmorcov88/fleetward/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("fleetward", version.Get())
		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so this path uses stderr directly.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log := telemetry.NewLogger(cfg.Log, os.Stdout)
	slog.SetDefault(log)

	log.Info("starting fleetward control plane",
		slog.String("version", version.Version),
		slog.String("commit", version.Commit),
		slog.Any("config", cfg))

	// Signals cancel this context, which unwinds every component below in reverse order.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, cfg.Telemetry, log)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			log.Warn("telemetry shutdown", slog.String("error", err.Error()))
		}
	}()

	// --- Metadata store -------------------------------------------------------------------------

	db, err := metadb.Open(ctx, cfg.MetaDB, log)
	if err != nil {
		return fmt.Errorf("metadata store: %w", err)
	}
	defer func() { _ = db.Close() }()

	if cfg.MetaDB.AutoMigrate {
		if err := db.Migrate(ctx); err != nil {
			return fmt.Errorf("metadata store: %w", err)
		}
	} else {
		schemaVersion, dirty, err := db.SchemaVersion(ctx)
		if err != nil {
			return fmt.Errorf("metadata store: %w", err)
		}
		if dirty {
			return fmt.Errorf("metadata store: schema version %d is dirty; resolve it before starting", schemaVersion)
		}
		log.Info("automatic migration disabled", slog.Uint64("schema_version", uint64(schemaVersion)))
	}

	// --- Secrets --------------------------------------------------------------------------------

	secretsProvider, err := buildSecretsProvider(cfg.Secrets, db)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	defer func() { _ = secretsProvider.Close() }()

	// --- Object storage -------------------------------------------------------------------------

	store, err := objstore.NewS3Store(cfg.ObjStore)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Creating the bucket is best-effort at startup: an operator whose object storage is briefly
	// down should get a control plane that starts and reports itself degraded, not one that
	// refuses to boot.
	bucketCtx, cancelBucket := context.WithTimeout(ctx, 15*time.Second)
	if err := store.EnsureBucket(bucketCtx); err != nil {
		log.Warn("could not ensure the backup bucket exists; backups will fail until this is resolved",
			slog.String("bucket", cfg.ObjStore.Bucket),
			slog.String("error", err.Error()))
	}
	cancelBucket()

	// --- Metrics store --------------------------------------------------------------------------

	metrics, err := tsdb.NewVictoriaMetrics(cfg.TSDB)
	if err != nil {
		return fmt.Errorf("metrics store: %w", err)
	}
	defer func() { _ = metrics.Close() }()

	// --- Plugins --------------------------------------------------------------------------------

	plugins := manager.New(cfg.Plugins, log)
	if err := plugins.Start(ctx); err != nil {
		return fmt.Errorf("plugin manager: %w", err)
	}
	defer func() { _ = plugins.Close() }()

	for _, info := range plugins.List() {
		log.Info("engine plugin",
			slog.String("engine_type", info.EngineType),
			slog.String("state", info.State.String()),
			slog.String("message", info.Message))
	}

	// --- Sandboxes ------------------------------------------------------------------------------

	sandboxes, err := sandbox.New(cfg.Sandbox, log)
	if err != nil {
		return fmt.Errorf("sandbox provider: %w", err)
	}
	defer func() { _ = sandboxes.Close() }()

	// The orphan sweep is the cleanup defence that covers the case the other two cannot: a control
	// plane killed mid-verification, leaving a container nobody will ever tear down. Startup is the
	// only moment we can be sure those containers are ours and abandoned.
	//
	// It is best-effort, for the same reason the bucket check above is. A missing Docker socket
	// means verification is unavailable, which readiness reports as degraded — it is not a reason
	// to refuse to serve the estate view.
	sweepCtx, cancelSweep := context.WithTimeout(ctx, 60*time.Second)
	if removed, err := sandboxes.Sweep(sweepCtx); err != nil {
		log.Warn("could not sweep orphaned verification sandboxes; check for leaked containers",
			slog.String("error", err.Error()))
	} else if removed > 0 {
		log.Warn("removed verification sandboxes left behind by a previous process",
			slog.Int("count", removed))
	}
	cancelSweep()

	// --- API ------------------------------------------------------------------------------------

	health := api.NewHealth(log, 5*time.Second)
	// The metadata store is the only truly critical dependency: without it the control plane
	// cannot answer any question at all. The rest degrade readiness instead of failing it, so that
	// a MinIO outage does not take the whole estate view offline.
	health.Register("metadb", true, db)
	health.Register("secrets", true, api.CheckerFunc(secretsProvider.HealthCheck))
	health.Register("objstore", false, store)
	health.Register("tsdb", false, metrics)
	health.Register("plugins", false, plugins)
	// Non-critical: without a container runtime a backup can still be taken and reported, it just
	// cannot be verified. Degrading readiness says exactly that, and says it before someone
	// discovers it at 3am.
	health.Register("sandbox", false, api.CheckerFunc(sandboxes.HealthCheck))

	// --- Who is calling, and what they may do ----------------------------------------------------
	//
	// Built before the services because every one of them is wrapped in the guard on the way out.

	tokens := authn.NewTokenStore(db.Pool(), cfg.Auth.PrincipalCacheTTL)

	// `last_used_at` is an operator's convenience — telling a live credential from one nobody has
	// touched since it was issued — and paying for a write on every authenticated request to keep it
	// current would be the wrong trade on a dashboard that polls every thirty seconds. So it is
	// batched, and flushed here. Losing a minute of it on a crash costs nothing.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := tokens.FlushLastUsed(ctx); err != nil {
					log.WarnContext(ctx, "could not record token use", slog.String("error", err.Error()))
				}
			}
		}
	}()

	sessionKey, configured, err := authn.LoadSessionKey(cfg.Auth.SessionKey, cfg.Auth.SessionKeyFile)
	if err != nil {
		return fmt.Errorf("session key: %w", err)
	}
	if !configured {
		log.Info("no session signing key configured; generating one for this process",
			slog.String("consequence", "restarting the control plane signs everybody out"),
			slog.String("remedy", "set FLEETWARD_AUTH_SESSION_KEY_FILE to keep sessions across restarts"))
	}
	sessions, err := authn.NewSessions(db.Pool(), sessionKey, cfg.Auth.SessionTTL, cfg.HTTP.TLSEnabled)
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}

	authenticator, err := buildAuthenticator(cfg.Auth, sessions, tokens, log)
	if err != nil {
		return err
	}

	guard, err := authz.NewGuard(ctx, db.Pool(), log)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	auditor := audit.NewWriter(db.Pool(), log)
	enforcer := authz.NewEnforcer(guard, auditor, log)

	server, err := api.NewServer(cfg.HTTP, log, health, authenticator)
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	server.RegisterSessions(sessions, tokens)

	// --- Being observable -------------------------------------------------------------------------
	//
	// Registered here, after the guard, because a scrape is authorized by the same thing every other
	// request is — and because /metrics is the one authorized route in this product that is not an
	// RPC, so it does not go through the policy table (ADR-0042). A nil handler registers nothing,
	// which is how a disabled endpoint answers 404 rather than an empty 200.
	var scrapeAuth api.ScrapeAuthorizer = guard
	if !cfg.Telemetry.PrometheusAuth {
		// Warned on every start, like disabled authentication and the bootstrap credential. The
		// endpoint discloses the shape of the estate — how many servers, which engines, which of
		// them fail verification — and an installation serving that to anyone who can reach the
		// port should never be a quiet fact.
		log.Warn("THE METRICS ENDPOINT IS UNAUTHENTICATED: anyone who can reach this port can read "+
			"the shape of your estate",
			slog.String("remedy", "set FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=true and give the "+
				"scraper a token with a tenant-wide viewer grant"))
		scrapeAuth = nil
	}
	server.RegisterMetrics(tel.MetricsHandler, scrapeAuth)

	// The REST API is served by grpc-gateway handlers mounted on the same mux as the health
	// endpoints. `GET /api/v1/version` stays registered on its own, more specific pattern, which
	// takes precedence over this subtree.
	//
	// Every service is registered wrapped in its guard. That wrapping is the only thing standing
	// between a stranger who can reach the port and an instance's stored credentials, so it is
	// deliberately impossible to read one of these registrations and not see it.
	gateway := api.NewGatewayMux()

	identitySvc := identity.New(db.Pool(), tokens, log)
	if err := fwv1.RegisterIdentityServiceHandlerServer(ctx, gateway,
		authz.GuardIdentity(enforcer, identity.NewGRPCServer(identitySvc, log))); err != nil {
		return fmt.Errorf("identity api: %w", err)
	}

	inventorySvc := inventory.New(db.Pool(), secretsProvider, plugins, log)
	if err := fwv1.RegisterInventoryServiceHandlerServer(ctx, gateway,
		authz.GuardInventory(enforcer, inventory.NewGRPCServer(inventorySvc, log))); err != nil {
		return fmt.Errorf("inventory api: %w", err)
	}

	// Backups outlive the request that asks for them, so the service owns goroutines. Closing it
	// before the HTTP server stops accepting would abandon a running backup; closing it after gives
	// each run the chance to record its outcome, which is the difference between a failed backup
	// and a row stuck in `running` with nothing to explain it.
	backupSvc := backup.New(db.Pool(), store, plugins, inventorySvc, sandboxes, backup.RetentionPolicy{
		Enabled:     cfg.Retention.Enabled,
		Interval:    cfg.Retention.Interval,
		MinKeep:     cfg.Retention.MinKeep,
		MaxPerSweep: cfg.Retention.MaxPerSweep,
	}, auditor, log)
	defer func() { _ = backupSvc.Close() }()

	if err := fwv1.RegisterBackupServiceHandlerServer(ctx, gateway,
		authz.GuardBackup(enforcer, backup.NewGRPCServer(backupSvc, log))); err != nil {
		return fmt.Errorf("backup api: %w", err)
	}

	// --- Alerts ---------------------------------------------------------------------------------
	//
	// The dispatcher is built before the service because the service hands it work, and it is closed
	// after the scheduler stops for the same reason the backup service is: a notification already
	// queued when shutdown began should still go out. Deferred calls unwind in reverse, so this one,
	// registered here, runs after the scheduler's Close below it.
	alertDispatch := alerts.NewDispatcher(db.Pool(), secretsProvider, alerts.DispatchConfig{
		Workers:   cfg.Alerts.DeliveryWorkers,
		QueueSize: cfg.Alerts.DeliveryQueueSize,
		Timeout:   cfg.Alerts.DeliveryTimeout,
		Attempts:  cfg.Alerts.DeliveryAttempts,
	}, log)
	defer func() { _ = alertDispatch.Close() }()
	alertDispatch.Start(ctx)

	// The adherence source is the backup service itself, behind a one-method interface. The
	// evaluator reads the same computation the estate view reads rather than a stored verdict, so an
	// alert and the screen can never disagree (ADR-0038).
	alertsSvc := alerts.New(db.Pool(), secretsProvider, backupAdherence{backupSvc}, alertDispatch, log)
	if err := fwv1.RegisterAlertServiceHandlerServer(ctx, gateway,
		authz.GuardAlerts(enforcer, alerts.NewGRPCServer(alertsSvc, log))); err != nil {
		return fmt.Errorf("alerts api: %w", err)
	}
	if !cfg.Alerts.Enabled {
		// Said out loud rather than left to be discovered. An installation whose evaluation pass is
		// off looks exactly like an estate with nothing wrong.
		log.Warn("ALERT EVALUATION IS DISABLED: no condition will become an alert and nothing will "+
			"be delivered anywhere",
			slog.String("remedy", "set FLEETWARD_ALERTS_ENABLED=true and restart"))
	}

	// --- Scheduler ------------------------------------------------------------------------------
	//
	// Constructed after the backup service, and this ordering is load-bearing rather than tidy.
	// Deferred calls unwind in reverse, so `defer sched.Close()` registered here runs *before*
	// `defer backupSvc.Close()` above. That is the only correct sequence: the scheduler stops
	// claiming work and drains its runners, and only then does the backup service wait for what is
	// left. In the other order the backup service would wait on runs the scheduler was still
	// starting, and shutdown would never complete.
	scheduleSvc := scheduler.NewService(db.Pool(), log)
	if err := fwv1.RegisterScheduleServiceHandlerServer(ctx, gateway,
		authz.GuardSchedule(enforcer, scheduler.NewGRPCServer(scheduleSvc, log))); err != nil {
		return fmt.Errorf("schedule api: %w", err)
	}

	sched := scheduler.New(db.Pool(), scheduler.NewJobRunner(backupSvc, inventorySvc, alertsSvc),
		cfg.Scheduler, cfg.Retention, cfg.Alerts, log)
	defer func() { _ = sched.Close() }()

	// Non-critical: a stalled tick loop means nothing runs automatically, which is worth degrading
	// readiness for and is not worth refusing to serve the estate view for. It is also the only way
	// this failure is visible at all — a loop that has quietly stopped looks exactly like an estate
	// with nothing scheduled.
	health.Register("scheduler", false, api.CheckerFunc(sched.HealthCheck))
	sched.Start(ctx)

	server.Mux().Handle("/api/v1/", gateway)

	if err := server.Start(ctx); err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	log.Info("fleetward control plane stopped")
	return nil
}

// buildAuthenticator assembles the chain that decides who is calling.
//
// The order is fixed: session cookie, then the configured break-glass credential, then the token
// store. A browser presents a cookie and a script presents a bearer value, so nothing presents two
// — but the *bearer* links have to be ordered against each other, and only one order works.
//
// The bootstrap link compares the whole presented string against the one configured, in constant
// time, and reports "not mine" on anything else. The token store cannot do that: a bearer value it
// has never seen is an invalid credential rather than somebody else's, and a chain that kept going
// after an invalid credential would let a revoked token fall through to whatever comes next. So the
// store is terminal, and anything that has to see a bearer value goes in front of it.
func buildAuthenticator(cfg config.AuthConfig, sessions *authn.Sessions, tokens *authn.TokenStore, log *slog.Logger) (authn.Authenticator, error) {
	if !cfg.Enabled {
		// The escape hatch, shipped with its limits, which is what ADR-0024 asks of every slice.
		// config.Validate already refuses this in production; the log line is for everywhere else,
		// because an installation nobody has to authenticate to should never be a quiet fact.
		log.Warn("AUTHENTICATION IS DISABLED: every request is treated as a tenant-wide administrator",
			slog.String("audit_actor", authn.AuthDisabledActor),
			slog.String("remedy", "set FLEETWARD_AUTH_ENABLED=true and issue a token"))
		return authn.NewDisabledAuthenticator(metadb.DefaultTenantID), nil
	}

	chain := authn.Chain{sessions}

	bootstrapToken, err := authn.LoadBootstrapToken(cfg.BootstrapToken, cfg.BootstrapTokenFile)
	if err != nil {
		return nil, fmt.Errorf("bootstrap token: %w", err)
	}
	if bootstrap := authn.NewBootstrapAuthenticator(bootstrapToken, metadb.DefaultTenantID); bootstrap != nil {
		// Warned on every start, not once. A break-glass credential that nobody is reminded about
		// is a break-glass credential that stays configured for a year.
		log.Warn("a bootstrap credential is configured and grants tenant-wide administrator",
			slog.String("audit_actor", authn.BootstrapActor),
			slog.String("remedy", "issue a real token, then remove FLEETWARD_AUTH_BOOTSTRAP_TOKEN and restart"))
		chain = append(chain, bootstrap)
	} else {
		log.Info("no bootstrap credential is configured",
			slog.String("consequence", "only tokens already in the database can authenticate"))
	}

	// The token store goes last, and the ordering is not a preference. It is the only link that
	// answers "this is a credential and it is wrong" rather than "this is not mine" — a bearer
	// value it has never seen is invalid, full stop — so anything after it is unreachable. The
	// walk found that the hard way: with the store ahead of the bootstrap link, the break-glass
	// credential never worked at all, because it is by definition a bearer value the database has
	// never seen.
	chain = append(chain, authn.NewBearerAuthenticator(tokens))
	return chain, nil
}

// buildSecretsProvider selects the configured SecretsProvider implementation (ADR-0009).
func buildSecretsProvider(cfg config.SecretsConfig, store secrets.Store) (secrets.Provider, error) {
	switch cfg.Provider {
	case "aesgcm":
		key, err := secrets.LoadMasterKey(cfg.MasterKey, cfg.MasterKeyFile)
		if err != nil {
			return nil, err
		}
		return secrets.NewAESGCM(store, key, 1)
	case "vault":
		return nil, errors.New(`the "vault" secrets provider is not implemented yet; use "aesgcm"`)
	default:
		return nil, fmt.Errorf("unknown secrets provider %q; supported: aesgcm", cfg.Provider)
	}
}

// backupAdherence adapts the backup service to the one method the alert evaluator needs.
//
// A named type here rather than an anonymous closure so that the coupling is visible at the wiring
// site: the `backup_missing` evaluator reads the estate view's own computation, and this is the
// three lines that say so. `ProblemsOnly` because an instance that is adherent produces no
// condition, and asking for the whole estate to filter it in Go would be work done twice.
type backupAdherence struct{ svc *backup.Service }

func (a backupAdherence) InstanceAdherence(ctx context.Context) ([]*fwv1.InstanceAdherence, error) {
	return a.svc.GetBackupAdherence(ctx, backup.GetAdherenceInput{ProblemsOnly: true})
}
