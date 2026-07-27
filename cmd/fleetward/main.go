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

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/api"
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

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.Telemetry, log)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
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

	server, err := api.NewServer(cfg.HTTP, log, health)
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	if err := server.Start(ctx); err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	log.Info("fleetward control plane stopped")
	return nil
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
