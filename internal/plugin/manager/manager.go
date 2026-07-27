// Package manager launches and supervises engine plugin processes (ADR-0003).
//
// Plugins are separate binaries. That isolation is the point: a plugin that leaks memory, hangs, or
// panics mid-backup cannot take the control plane with it. The cost is that something has to own
// the process lifecycle — launch, handshake, health, restart, shutdown — and that something is this
// package.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

// BinaryPrefix is the filename prefix every plugin binary must use. The engine type is whatever
// follows it, so `fleetward-plugin-postgresql` serves instances with engine_type "postgresql".
const BinaryPrefix = "fleetward-plugin-"

// ErrPluginNotFound is returned when no plugin serves a requested engine type.
var ErrPluginNotFound = errors.New("no plugin available for engine type")

// ErrPluginNotReady is returned when a plugin exists but is not currently usable.
var ErrPluginNotReady = errors.New("plugin is not ready")

// State is a plugin's lifecycle state.
type State int

const (
	// StateStarting means the process is launching or handshaking.
	StateStarting State = iota
	// StateReady means the plugin has handshaked and can serve requests.
	StateReady
	// StateUnhealthy means the plugin is being restarted after a failure.
	StateUnhealthy
	// StateFailed means restarts have been exhausted; the plugin will not be retried.
	StateFailed
	// StateStopped means the manager shut the plugin down deliberately.
	StateStopped
)

// String renders the state for logs and API responses.
func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateUnhealthy:
		return "unhealthy"
	case StateFailed:
		return "failed"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Proto converts the state to its wire representation.
func (s State) Proto() fwv1.PluginState {
	switch s {
	case StateStarting:
		return fwv1.PluginState_PLUGIN_STATE_STARTING
	case StateReady:
		return fwv1.PluginState_PLUGIN_STATE_READY
	case StateUnhealthy:
		return fwv1.PluginState_PLUGIN_STATE_UNHEALTHY
	case StateFailed:
		return fwv1.PluginState_PLUGIN_STATE_FAILED
	case StateStopped:
		return fwv1.PluginState_PLUGIN_STATE_STOPPED
	default:
		return fwv1.PluginState_PLUGIN_STATE_UNSPECIFIED
	}
}

// Info is a point-in-time snapshot of a plugin's status.
type Info struct {
	EngineType   string
	BinaryPath   string
	State        State
	Capabilities *fwv1.Capabilities
	Message      string
	MissingTools []string
	RestartCount int
	StartedAt    time.Time
}

// Manager owns every plugin process.
type Manager struct {
	cfg config.PluginsConfig
	log *slog.Logger

	mu      sync.RWMutex
	plugins map[string]*managed

	// stop cancels all supervisors. Guarded by once so Close is idempotent.
	stop     context.CancelFunc
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// managed is one supervised plugin process.
type managed struct {
	engineType string
	binaryPath string
	cfg        config.PluginsConfig
	log        *slog.Logger

	mu           sync.RWMutex
	client       *goplugin.Client
	engine       fwv1.EnginePluginClient
	caps         *fwv1.Capabilities
	state        State
	message      string
	missingTools []string
	restarts     int
	startedAt    time.Time
}

// New builds a manager. It does not launch anything; call Start.
func New(cfg config.PluginsConfig, log *slog.Logger) *Manager {
	return &Manager{
		cfg:     cfg,
		log:     log.With(slog.String("component", "plugin-manager")),
		plugins: make(map[string]*managed),
	}
}

// Start discovers plugin binaries and launches each one.
//
// A plugin that fails to launch is recorded as failed rather than aborting startup: an operator
// with a broken MongoDB plugin should still get a working control plane for their other engines.
func (m *Manager) Start(ctx context.Context) error {
	binaries, err := discoverBinaries(m.cfg.Dir)
	if err != nil {
		return err
	}

	if len(binaries) == 0 {
		m.log.Warn("no engine plugins found",
			slog.String("dir", m.cfg.Dir),
			slog.String("hint", "run `make build-plugins` to build them"))
	}

	supervisorCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.stop = cancel

	for engineType, path := range binaries {
		p := &managed{
			engineType: engineType,
			binaryPath: path,
			cfg:        m.cfg,
			log:        m.log.With(slog.String("engine_type", engineType)),
			state:      StateStarting,
		}

		m.mu.Lock()
		m.plugins[engineType] = p
		m.mu.Unlock()

		if err := p.launch(ctx); err != nil {
			p.setFailed(err)
			m.log.Error("plugin failed to start",
				slog.String("engine_type", engineType),
				slog.String("error", err.Error()))
		}

		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			p.supervise(supervisorCtx)
		}()
	}

	return nil
}

// discoverBinaries maps engine type to binary path for every plugin in dir.
func discoverBinaries(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing directory is a deployment that has not built its plugins yet, not a fatal
			// configuration error.
			return nil, nil
		}
		return nil, fmt.Errorf("plugin manager: read plugin dir %q: %w", dir, err)
	}

	found := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, BinaryPrefix) {
			continue
		}
		engineType := strings.TrimSuffix(strings.TrimPrefix(name, BinaryPrefix), ".exe")
		if engineType == "" {
			continue
		}

		path := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("plugin manager: stat %q: %w", path, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("plugin manager: %q is not executable", path)
		}
		found[engineType] = path
	}
	return found, nil
}

// launch starts the plugin process and completes the handshake.
func (p *managed) launch(ctx context.Context) error {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: sdk.Handshake,
		Plugins:         map[string]goplugin.Plugin{sdk.PluginName: &sdk.EnginePlugin{}},
		// exec.Command, not exec.CommandContext: this context belongs to the launch attempt, and
		// binding the process to it would kill a healthy plugin the moment startup finished.
		// go-plugin owns the process lifetime and the manager ends it through Client.Kill.
		Cmd: exec.Command(p.binaryPath), //nolint:gosec,noctx // path is from our own plugin directory; lifetime is owned by go-plugin
		AllowedProtocols: []goplugin.Protocol{
			goplugin.ProtocolGRPC,
		},
		// AutoMTLS makes the local socket mutually authenticated with a per-launch certificate, so
		// another process on the host cannot talk to a plugin and hand it credentials.
		AutoMTLS:     true,
		StartTimeout: p.cfg.HandshakeTimeout,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:       "plugin." + p.engineType,
			Level:      hclog.Info,
			Output:     os.Stderr,
			JSONFormat: true,
		}),
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return fmt.Errorf("handshake with %s plugin: %w", p.engineType, err)
	}

	raw, err := rpcClient.Dispense(sdk.PluginName)
	if err != nil {
		client.Kill()
		return fmt.Errorf("dispense %s plugin: %w", p.engineType, err)
	}

	engine, ok := raw.(fwv1.EnginePluginClient)
	if !ok {
		client.Kill()
		return fmt.Errorf("plugin %s returned an unexpected client type %T", p.engineType, raw)
	}

	capsCtx, cancel := context.WithTimeout(ctx, p.cfg.HandshakeTimeout)
	defer cancel()

	caps, err := engine.GetCapabilities(capsCtx, &fwv1.GetCapabilitiesRequest{})
	if err != nil {
		client.Kill()
		return fmt.Errorf("get capabilities from %s plugin: %w", p.engineType, err)
	}

	if err := validateHandshake(p.engineType, caps); err != nil {
		client.Kill()
		return err
	}

	missing := missingTools(caps)

	p.mu.Lock()
	p.client = client
	p.engine = engine
	p.caps = caps
	p.state = StateReady
	p.message = ""
	p.missingTools = missing
	p.startedAt = time.Now()
	p.mu.Unlock()

	p.log.Info("plugin ready",
		slog.String("plugin_version", caps.GetPluginVersion()),
		slog.String("engine_display_name", caps.GetEngineDisplayName()),
		slog.Int("backup_methods", len(caps.GetBackupMethods())),
		slog.Bool("supports_pitr", caps.GetSupportsPitr()),
		slog.Bool("supports_sandbox_restore", caps.GetSupportsSandboxRestore()))

	if len(missing) > 0 {
		// Surfaced now, loudly, rather than at 3am when a scheduled backup fails on a missing
		// binary.
		p.log.Warn("plugin is missing required tools; backups using the affected methods will fail",
			slog.Any("missing_tools", missing))
	}

	return nil
}

// validateHandshake rejects a plugin whose identity or contract version does not line up.
func validateHandshake(engineType string, caps *fwv1.Capabilities) error {
	if err := sdk.ValidateCapabilities(caps); err != nil {
		return fmt.Errorf("plugin %s reported invalid capabilities: %w", engineType, err)
	}

	// The binary name and the declared engine type must agree, or routing an instance to a plugin
	// would silently send it to the wrong engine.
	if caps.GetEngineType() != engineType {
		return fmt.Errorf(
			"plugin binary %s%s declares engine_type %q; the binary name and declared engine must match",
			BinaryPrefix, engineType, caps.GetEngineType())
	}

	if got := caps.GetContractVersion(); got != version.ContractVersion {
		return fmt.Errorf(
			"plugin %s speaks contract %q but this control plane speaks %q",
			engineType, got, version.ContractVersion)
	}

	return nil
}

// missingTools reports which of a plugin's declared required tools are absent from PATH. Plugins
// run on the same host as the control plane, so looking them up here is a faithful check.
func missingTools(caps *fwv1.Capabilities) []string {
	required := make(map[string]bool)
	for _, tool := range caps.GetRequiredTools() {
		required[tool] = true
	}
	for _, method := range caps.GetBackupMethods() {
		for _, tool := range method.GetRequiredTools() {
			required[tool] = true
		}
	}

	var missing []string
	for tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	sort.Strings(missing)
	return missing
}

// supervise watches one plugin and restarts it when it dies.
func (p *managed) supervise(ctx context.Context) {
	interval := p.cfg.HealthInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p.isAlive() {
				continue
			}
			p.log.Warn("plugin process is not running; restarting")
			p.restart(ctx)
		}
	}
}

func (p *managed) isAlive() bool {
	p.mu.RLock()
	client, state := p.client, p.state
	p.mu.RUnlock()

	if state == StateStopped {
		return true // deliberately stopped; nothing to restart
	}
	return client != nil && !client.Exited()
}

// restart relaunches a dead plugin with exponential backoff.
func (p *managed) restart(ctx context.Context) {
	for {
		p.mu.Lock()
		if p.state == StateStopped {
			p.mu.Unlock()
			return
		}
		if p.client != nil {
			p.client.Kill()
			p.client = nil
			p.engine = nil
		}
		p.restarts++
		attempt := p.restarts
		p.state = StateStarting
		p.mu.Unlock()

		if p.cfg.MaxRestarts > 0 && attempt > p.cfg.MaxRestarts {
			p.setFailed(fmt.Errorf("gave up after %d restart attempts", attempt-1))
			p.log.Error("plugin permanently failed", slog.Int("attempts", attempt-1))
			return
		}

		delay := backoff(p.cfg.RestartBackoffMin, p.cfg.RestartBackoffMax, attempt)
		p.log.Info("restarting plugin",
			slog.Int("attempt", attempt),
			slog.Duration("delay", delay))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := p.launch(ctx); err != nil {
			p.setUnhealthy(err)
			p.log.Error("plugin restart failed", slog.String("error", err.Error()))
			continue
		}

		p.log.Info("plugin restarted successfully", slog.Int("attempt", attempt))
		return
	}
}

// backoff computes the delay before restart attempt n, doubling from min up to max with jitter.
//
// The jitter matters when several plugins share a cause of death — a machine that just ran out of
// memory, say. Without it they would all restart in lockstep and recreate the condition.
func backoff(minDelay, maxDelay time.Duration, attempt int) time.Duration {
	if minDelay <= 0 {
		minDelay = time.Second
	}
	if maxDelay <= 0 || maxDelay < minDelay {
		maxDelay = 5 * time.Minute
	}

	delay := minDelay
	for range attempt - 1 {
		delay *= 2
		if delay >= maxDelay {
			delay = maxDelay
			break
		}
	}

	// Up to ±20% jitter.
	// math/rand is correct here: this jitter spreads restart attempts, it does not protect
	// anything. A cryptographic source would cost entropy for no security benefit.
	jitter := time.Duration(rand.Int64N(int64(delay/5)*2+1)) - delay/5 //nolint:gosec // G404: scheduling jitter, not a security control
	delay += jitter
	if delay < minDelay {
		delay = minDelay
	}
	return delay
}

func (p *managed) setFailed(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = StateFailed
	p.message = err.Error()
}

func (p *managed) setUnhealthy(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = StateUnhealthy
	p.message = err.Error()
}

func (p *managed) info() Info {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Info{
		EngineType:   p.engineType,
		BinaryPath:   p.binaryPath,
		State:        p.state,
		Capabilities: p.caps,
		Message:      p.message,
		MissingTools: p.missingTools,
		RestartCount: p.restarts,
		StartedAt:    p.startedAt,
	}
}

// Client returns the gRPC client for an engine type, together with its capabilities.
//
// Callers must not cache the client across calls: a plugin can be restarted at any time, and the
// old client will fail every RPC after that.
func (m *Manager) Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error) {
	m.mu.RLock()
	p, ok := m.plugins[engineType]
	m.mu.RUnlock()

	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", ErrPluginNotFound, engineType)
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.state != StateReady || p.engine == nil {
		return nil, nil, fmt.Errorf("%w: %s is %s: %s", ErrPluginNotReady, engineType, p.state, p.message)
	}
	return p.engine, p.caps, nil
}

// Capabilities returns a plugin's capability matrix without requiring it to be ready, so the UI can
// still explain what a currently-unhealthy engine would support.
func (m *Manager) Capabilities(engineType string) (*fwv1.Capabilities, error) {
	m.mu.RLock()
	p, ok := m.plugins[engineType]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPluginNotFound, engineType)
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.caps == nil {
		return nil, fmt.Errorf("%w: %q has not completed handshake", ErrPluginNotReady, engineType)
	}
	return p.caps, nil
}

// List returns a snapshot of every known plugin, sorted by engine type for stable output.
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Info, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EngineType < out[j].EngineType })
	return out
}

// EngineTypes returns the engine types this control plane can serve.
func (m *Manager) EngineTypes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.plugins))
	for engineType := range m.plugins {
		out = append(out, engineType)
	}
	sort.Strings(out)
	return out
}

// HealthCheck reports an error if any plugin is not ready.
//
// This feeds readiness reporting rather than the liveness probe: a control plane with one broken
// plugin is degraded, not dead, and restarting it would not fix the plugin.
func (m *Manager) HealthCheck(context.Context) error {
	var problems []string
	for _, info := range m.List() {
		if info.State != StateReady {
			problems = append(problems, fmt.Sprintf("%s is %s", info.EngineType, info.State))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("plugins not ready: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Close stops every plugin process. It is safe to call more than once.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() {
		if m.stop != nil {
			m.stop()
		}

		m.mu.RLock()
		plugins := make([]*managed, 0, len(m.plugins))
		for _, p := range m.plugins {
			plugins = append(plugins, p)
		}
		m.mu.RUnlock()

		for _, p := range plugins {
			p.mu.Lock()
			// Set state before killing, so a supervisor that wakes up mid-shutdown does not treat
			// the exit as a crash and start a restart we are trying to prevent.
			p.state = StateStopped
			if p.client != nil {
				p.client.Kill()
				p.client = nil
				p.engine = nil
			}
			p.mu.Unlock()
		}

		m.wg.Wait()
		m.log.Info("all plugins stopped", slog.Int("count", len(plugins)))
	})
	return nil
}
