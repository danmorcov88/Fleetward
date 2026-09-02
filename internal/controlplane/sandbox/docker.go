package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"google.golang.org/protobuf/proto"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
)

// Tunables that are deliberately not configuration. Each one is a property of how Docker behaves
// rather than of a deployment, and an operator turning any of them into a knob would be tuning the
// wrong thing.
const (
	// imagePullTimeout bounds the image pull. It has its own budget rather than sharing the
	// startup timeout, because a first run on a clean machine pulls several hundred megabytes and
	// that must not look like a hung verification.
	imagePullTimeout = 15 * time.Minute

	// readinessPollInterval is how often the readiness probe is retried.
	readinessPollInterval = 500 * time.Millisecond

	// readinessSuccesses is how many consecutive successful probes are needed. A PostgreSQL
	// container runs a temporary server during initdb and then restarts it; one success is not
	// evidence that the server answering is the one that will still be there a second later.
	readinessSuccesses = 2

	// stopTimeout is how long a container gets to exit before it is killed. A sandbox holds no
	// data anybody wants, so this is short on purpose.
	stopTimeout = 5 * time.Second

	// reapInterval bounds how stale the lifetime reaper's view can be.
	reapInterval = time.Minute

	// defaultMaxLifetime applies when configuration leaves the ceiling unset. There is no "no
	// ceiling" option: the reaper is a cleanup defence, and disabling it disables the defence.
	defaultMaxLifetime = 2 * time.Hour
)

// nameUnsafe matches every character that is not legal in a Docker container name.
var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// labels are the label keys this provider owns, derived from the configured prefix. Every
// container is labelled at creation and never afterwards: a container that starts unlabelled is
// invisible to the sweep, which means it leaks forever.
type labels struct {
	// marker is present on every sandbox and is what the sweep filters on.
	marker    string
	id        string
	engine    string
	owner     string
	createdAt string
	expiresAt string
}

func newLabels(prefix string) labels {
	if prefix == "" {
		prefix = "fleetward"
	}
	base := prefix + ".sandbox"
	return labels{
		marker:    base,
		id:        base + ".id",
		engine:    base + ".engine",
		owner:     base + ".owner",
		createdAt: base + ".created-at",
		expiresAt: base + ".expires-at",
	}
}

// DockerProvider provisions sandboxes as Docker containers.
//
// It is exported so that main can name the concrete type in an error, but it should be constructed
// through New and used through Provider — nothing outside this package may depend on the fact that
// containers are involved at all.
type DockerProvider struct {
	cli    *client.Client
	cfg    config.SandboxConfig
	log    *slog.Logger
	labels labels

	// owner distinguishes this process's sandboxes from those of a process that died. It is what
	// lets Sweep tell an orphan from a live sandbox.
	owner string
	// namePrefix is the configured label prefix reduced to characters Docker accepts in a name.
	namePrefix string

	closeOnce  sync.Once
	stop       chan struct{}
	reaperGone chan struct{}
}

// NewDockerProvider builds a Docker-backed provider. It does not contact the daemon.
func NewDockerProvider(cfg config.SandboxConfig, log *slog.Logger) (*DockerProvider, error) {
	if log == nil {
		log = slog.Default()
	}

	// No option here contacts the daemon. API-version negotiation happens on the first request,
	// which is what keeps a control plane with no Docker socket able to start and report itself
	// degraded rather than refusing to boot.
	opts := []client.Opt{
		client.WithHostFromEnv(),
		client.WithTLSClientConfigFromEnv(),
	}
	if cfg.DockerHost != "" {
		opts = append(opts, client.WithHost(cfg.DockerHost))
	}

	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	owner, err := randomID()
	if err != nil {
		_ = cli.Close()
		return nil, err
	}

	p := &DockerProvider{
		cli:        cli,
		cfg:        cfg,
		log:        log.With(slog.String("component", "sandbox")),
		labels:     newLabels(cfg.LabelPrefix),
		owner:      owner,
		namePrefix: sanitizeName(cfg.LabelPrefix),
		stop:       make(chan struct{}),
		reaperGone: make(chan struct{}),
	}

	go p.reapLoop()
	return p, nil
}

// sanitizeName reduces a label prefix to something Docker will accept as a name component.
func sanitizeName(prefix string) string {
	name := nameUnsafe.ReplaceAllString(prefix, "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		return "fleetward"
	}
	return name
}

// maxLifetime is the ceiling applied to a sandbox that does not ask for its own.
func (p *DockerProvider) maxLifetime() time.Duration {
	if p.cfg.MaxLifetime > 0 {
		return p.cfg.MaxLifetime
	}
	return defaultMaxLifetime
}

// startupTimeout bounds create-start-become-ready.
func (p *DockerProvider) startupTimeout() time.Duration {
	if p.cfg.StartupTimeout > 0 {
		return p.cfg.StartupTimeout
	}
	return 3 * time.Minute
}

// HealthCheck reports whether the Docker daemon answers.
func (p *DockerProvider) HealthCheck(ctx context.Context) error {
	if _, err := p.cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("docker daemon at %s: %w", p.cli.DaemonHost(), err)
	}
	return nil
}

// Close stops the reaper and releases the client.
func (p *DockerProvider) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.reaperGone
		err = p.cli.Close()
	})
	return err
}

// --- Provisioning --------------------------------------------------------------------------------

// Provision stands up a sandbox and waits until it accepts connections.
func (p *DockerProvider) Provision(ctx context.Context, spec Spec) (Sandbox, error) {
	select {
	case <-p.stop:
		return nil, ErrClosed
	default:
	}

	if err := spec.validate(); err != nil {
		return nil, err
	}

	image, err := ImageRef(spec.Template, spec.EngineVersion)
	if err != nil {
		return nil, err
	}

	id, err := newIdentity(spec)
	if err != nil {
		return nil, err
	}

	// The pull gets its own budget so a cold machine's several-hundred-megabyte download does not
	// eat the startup timeout and surface as "the sandbox never became ready".
	pullCtx, cancelPull := context.WithTimeout(ctx, imagePullTimeout)
	err = p.ensureImage(pullCtx, image)
	cancelPull()
	if err != nil {
		return nil, err
	}

	startCtx, cancelStart := context.WithTimeout(ctx, p.startupTimeout())
	defer cancelStart()

	box, err := p.create(startCtx, spec, id, image)
	if err != nil {
		return nil, err
	}

	// From here on the container exists, so every failure path must remove it. The destroy context
	// is detached from startCtx because the most likely reason we are here is that startCtx expired.
	defer func() {
		if err == nil {
			return
		}
		destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyTimeout)
		defer cancel()
		if derr := box.Destroy(destroyCtx); derr != nil {
			p.log.Error("could not remove a sandbox that failed to start; it may be leaking",
				slog.String("sandbox_id", box.ID()),
				slog.String("container_id", box.containerID),
				slog.String("error", derr.Error()))
		}
	}()

	if _, err = p.cli.ContainerStart(startCtx, box.containerID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start sandbox container: %w", err)
	}

	if err = p.resolveEndpoint(startCtx, box); err != nil {
		return nil, err
	}

	if err = p.waitReady(startCtx, box, spec.Template); err != nil {
		return nil, err
	}

	p.log.Info("sandbox ready",
		slog.String("sandbox_id", box.ID()),
		slog.String("engine_type", spec.EngineType),
		slog.String("image", image),
		slog.String("host", box.creds.GetHost()),
		slog.Int("port", int(box.creds.GetPort())))

	return box, nil
}

// ensureImage pulls the image unless the daemon already has it.
//
// Inspecting first matters more than it looks: an unconditional pull contacts the registry on
// every verification, which turns a registry outage or a rate limit into a verification failure
// even though the image is sitting on disk.
func (p *DockerProvider) ensureImage(ctx context.Context, image string) error {
	if _, err := p.cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect sandbox image %s: %w", image, err)
	}

	p.log.Info("pulling sandbox image", slog.String("image", image))

	resp, err := p.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull sandbox image %s: %w", image, err)
	}
	defer func() { _ = resp.Close() }()

	// The body must be drained for the pull to complete; the progress messages themselves are of no
	// use to a control plane log.
	if _, err := io.Copy(io.Discard, resp); err != nil {
		return fmt.Errorf("pull sandbox image %s: %w", image, err)
	}
	return nil
}

// create builds the container and returns the sandbox handle for it. The container is created but
// not started.
func (p *DockerProvider) create(ctx context.Context, spec Spec, id identity, image string) (*dockerSandbox, error) {
	sandboxID, err := randomID()
	if err != nil {
		return nil, err
	}

	env, err := renderEnv(spec.Template.GetEnv(), id)
	if err != nil {
		return nil, err
	}
	cmd, err := renderAll(spec.Template.GetCommand(), id)
	if err != nil {
		return nil, err
	}

	lifetime := spec.Lifetime
	if lifetime <= 0 {
		lifetime = p.maxLifetime()
	}
	now := time.Now().UTC()

	// Caller labels first, so a provider label can never be overwritten by one.
	containerLabels := make(map[string]string, len(spec.Labels)+6)
	maps.Copy(containerLabels, spec.Labels)
	containerLabels[p.labels.marker] = "true"
	containerLabels[p.labels.id] = sandboxID
	containerLabels[p.labels.engine] = spec.EngineType
	containerLabels[p.labels.owner] = p.owner
	containerLabels[p.labels.createdAt] = now.Format(time.RFC3339)
	containerLabels[p.labels.expiresAt] = now.Add(lifetime).Format(time.RFC3339)

	// Parsed from text rather than converted numerically: ParsePort owns the range check, so there
	// is one place that decides what a legal port is instead of two that can drift apart.
	port, err := network.ParsePort(strconv.Itoa(int(spec.Template.GetContainerPort())) + "/tcp")
	if err != nil {
		return nil, fmt.Errorf("%w: container_port: %w", ErrInvalidTemplate, err)
	}

	hostConfig := &container.HostConfig{
		// A sandbox that dies must stay dead and stay inspectable. Restarting it would hide a
		// verification failure, and auto-removing it would race the teardown path that reports one.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		AutoRemove:    false,
	}
	if p.cfg.Network != "" {
		// On a shared network the sandbox is reached by container name, so nothing is published.
		// A sandbox holds a full copy of a production database behind a password that lives for
		// minutes; putting it on a host port nobody reads from is exposure bought for nothing.
		hostConfig.NetworkMode = container.NetworkMode(p.cfg.Network)
	} else {
		// An empty HostPort asks Docker for an ephemeral one. Fixed ports collide the moment two
		// verifications run at once.
		hostConfig.PortBindings = network.PortMap{
			port: []network.PortBinding{{HostIP: p.bindAddress(), HostPort: ""}},
		}
	}

	// A plugin whose engine hands its artifact over as a file needs somewhere both of them can
	// reach (ADR-0026). Core makes that directory and mounts it; what goes in it is the plugin's
	// business, and core never learns which engine asked.
	share, err := p.prepareSharedDir(spec.Template.GetSharedDirectory(), sandboxID)
	if err != nil {
		return nil, err
	}
	if share != nil {
		hostConfig.Mounts = append(hostConfig.Mounts, share.mount)
	}

	name := p.namePrefix + "-sandbox-" + sandboxID

	created, err := p.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        image,
			Env:          env,
			Cmd:          cmd,
			Labels:       containerLabels,
			ExposedPorts: network.PortSet{port: struct{}{}},
		},
		HostConfig: hostConfig,
	})
	if err != nil {
		if share != nil {
			share.cleanup()
		}
		return nil, fmt.Errorf("create sandbox container: %w", err)
	}
	for _, warning := range created.Warnings {
		p.log.Warn("docker warning creating a sandbox",
			slog.String("sandbox_id", sandboxID), slog.String("warning", warning))
	}

	box := &dockerSandbox{
		provider:      p,
		id:            sandboxID,
		containerID:   created.ID,
		containerName: name,
		containerPort: port,
		creds: &fwv1.Credentials{
			Username: id.Username,
			Password: id.Password,
			Database: id.Database,
			Options:  map[string]string{},
		},
	}
	if share != nil {
		box.creds.SharedDirectory = &fwv1.SharedDirectory{
			EnginePath: share.enginePath,
			LocalPath:  share.localPath,
		}
		box.cleanupShare = share.cleanup
	}
	return box, nil
}

// sharedDir is one prepared directory, in the two forms its two users need it in.
type sharedDir struct {
	mount      mount.Mount
	enginePath string
	localPath  string
	// cleanup removes what prepareSharedDir created. It never removes a configured volume, only
	// this sandbox's own subtree of it.
	cleanup func()
}

// prepareSharedDir creates the directory a template asked for and describes how to mount it.
//
// Two shapes, and which one applies is a property of where the control plane runs rather than of
// the engine. With a configured volume, the sandbox mounts that volume and core writes into the
// place the same volume is already mounted for itself — the only arrangement that works when the
// control plane is itself a container, because a bind mount's source is resolved by the daemon
// against its own filesystem. Without one, core binds a temporary directory of its own, which is
// what the conformance suite and any host-run control plane get.
//
// Either way the sandbox gets its own subdirectory, so two sandboxes sharing a volume cannot see
// each other's artifacts.
func (p *DockerProvider) prepareSharedDir(containerPath, sandboxID string) (*sharedDir, error) {
	if containerPath == "" {
		return nil, nil
	}

	if volume := p.cfg.SharedDirVolume; volume != "" {
		local := filepath.Join(p.cfg.SharedDirLocal, sandboxID)
		if err := os.MkdirAll(local, sharedDirMode); err != nil {
			return nil, fmt.Errorf("create the shared directory for a sandbox: %w", err)
		}
		return &sharedDir{
			mount:      mount.Mount{Type: mount.TypeVolume, Source: volume, Target: containerPath},
			enginePath: path.Join(containerPath, sandboxID),
			localPath:  local,
			cleanup:    func() { _ = os.RemoveAll(local) },
		}, nil
	}

	local, err := os.MkdirTemp("", "fleetward-share-")
	if err != nil {
		return nil, fmt.Errorf("create the shared directory for a sandbox: %w", err)
	}
	// MkdirTemp is 0700 by design, and the engine runs as its own user. It needs to traverse the
	// directory and write the file the plugin creates in it; it never needs to create one, which
	// is what keeps the artifact owned by the plugin.
	if err := os.Chmod(local, sharedDirMode); err != nil {
		_ = os.RemoveAll(local)
		return nil, fmt.Errorf("open the shared directory to the engine: %w", err)
	}
	return &sharedDir{
		mount:      mount.Mount{Type: mount.TypeBind, Source: local, Target: containerPath},
		enginePath: containerPath,
		localPath:  local,
		cleanup:    func() { _ = os.RemoveAll(local) },
	}, nil
}

// sharedDirMode lets the engine's own user reach the directory. The artifact inside it is created
// by the plugin and stays the plugin's; this permits traversal, not creation.
const sharedDirMode = 0o777

// bindAddress decides which host interface a sandbox's published port binds to.
//
// Loopback is the right default by a wide margin: a sandbox holds a full copy of a production
// database behind a password that exists for minutes. It is only wrong when the daemon is on
// another host, where a loopback binding would be unreachable by definition.
func (p *DockerProvider) bindAddress() netip.Addr {
	if p.daemonIsLocal() {
		return netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	// The zero address means "every interface", which is what a remote daemon requires.
	return netip.Addr{}
}

// daemonHostname is the host to reach the daemon's published ports on.
func (p *DockerProvider) daemonHostname() string {
	if p.daemonIsLocal() {
		// 127.0.0.1 rather than localhost, deliberately. localhost resolves to ::1 first in some
		// images while a published port is often bound IPv4-only, which reads as an unreachable
		// sandbox.
		return "127.0.0.1"
	}
	u, err := url.Parse(p.cli.DaemonHost())
	if err != nil {
		return "127.0.0.1"
	}
	if host := u.Hostname(); host != "" {
		return host
	}
	return "127.0.0.1"
}

// daemonIsLocal reports whether the Docker daemon shares a network namespace with this process.
func (p *DockerProvider) daemonIsLocal() bool {
	u, err := url.Parse(p.cli.DaemonHost())
	if err != nil {
		return true
	}
	switch u.Scheme {
	case "unix", "npipe", "":
		return true
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	}
	return false
}

// resolveEndpoint fills in where the started container can be reached.
//
// When sandboxes are attached to a user-defined network the control plane is also on, the
// container is addressed by name on that network. Otherwise it is addressed through the published
// ephemeral port, which is what a control plane running on the host needs.
//
// The published port has to be polled for, not simply read. ContainerStart returning does not mean
// the port is mapped: Docker Desktop publishes through a proxy it sets up asynchronously, so an
// inspect issued immediately after start reports an empty binding on exactly the platform this
// project is developed on.
func (p *DockerProvider) resolveEndpoint(ctx context.Context, box *dockerSandbox) error {
	if p.cfg.Network != "" {
		box.creds.Host = box.containerName
		box.creds.Port = int32(box.containerPort.Num())
		return nil
	}

	for {
		hostPort, err := p.publishedPort(ctx, box)
		if err != nil {
			return err
		}
		if hostPort > 0 {
			box.creds.Host = p.daemonHostname()
			box.creds.Port = hostPort
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: container port %s was never published", ErrNotReady, box.containerPort)
		case <-time.After(readinessPollInterval):
		}
	}
}

// publishedPort returns the host port the container's port is mapped to, or zero if it is not
// mapped yet. A container that has already exited is reported as a failure rather than polled.
func (p *DockerProvider) publishedPort(ctx context.Context, box *dockerSandbox) (int32, error) {
	exited, err := p.containerExited(ctx, box.containerID)
	if err != nil {
		return 0, err
	}
	if exited != nil {
		return 0, fmt.Errorf("%w: the container exited with status %d before publishing its port",
			ErrNotReady, *exited)
	}

	inspected, err := p.cli.ContainerInspect(ctx, box.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return 0, fmt.Errorf("inspect sandbox container: %w", err)
	}
	settings := inspected.Container.NetworkSettings
	if settings == nil {
		return 0, nil
	}

	for _, binding := range settings.Ports[box.containerPort] {
		if binding.HostPort == "" {
			continue
		}
		hostPort, err := strconv.ParseUint(binding.HostPort, 10, 16)
		if err != nil {
			continue
		}
		return int32(hostPort), nil
	}
	return 0, nil
}

// --- Readiness -----------------------------------------------------------------------------------

// waitReady polls until the sandbox actually accepts connections.
//
// "The container is running" is not readiness. A database container is running long before its
// server is listening, and several engines start a throwaway server during initialization and then
// restart it — which is why success has to be observed more than once.
func (p *DockerProvider) waitReady(ctx context.Context, box *dockerSandbox, tmpl *fwv1.SandboxTemplate) error {
	if timeout := tmpl.GetReadinessTimeout().AsDuration(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	probeCmd, err := renderAll(tmpl.GetReadinessCommand(), box.identity())
	if err != nil {
		return err
	}

	var (
		consecutive int
		lastErr     = errors.New("no probe has run yet")
	)

	for {
		exited, exitErr := p.containerExited(ctx, box.containerID)
		if exitErr != nil {
			return exitErr
		}
		if exited != nil {
			// A container that has exited will never become ready, and waiting out the timeout
			// only delays a diagnosis that is already available.
			return fmt.Errorf("%w: the container exited with status %d", ErrNotReady, *exited)
		}

		if err := p.probe(ctx, box, probeCmd); err != nil {
			consecutive = 0
			lastErr = err
		} else {
			consecutive++
			if consecutive >= readinessSuccesses {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w within the startup timeout: %w", ErrNotReady, lastErr)
		case <-time.After(readinessPollInterval):
		}
	}
}

// containerExited returns the exit code if the container has stopped, and nil while it runs.
func (p *DockerProvider) containerExited(ctx context.Context, containerID string) (*int, error) {
	inspected, err := p.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: the container disappeared while starting", ErrNotReady)
		}
		return nil, fmt.Errorf("inspect sandbox container: %w", err)
	}

	state := inspected.Container.State
	if state == nil || state.Running || state.Restarting {
		return nil, nil
	}
	switch state.Status {
	case container.StateExited, container.StateDead:
		code := state.ExitCode
		return &code, nil
	default:
		// "created" or "paused": not running, but not finished either.
		return nil, nil
	}
}

// probe runs one readiness check.
func (p *DockerProvider) probe(ctx context.Context, box *dockerSandbox, cmd []string) error {
	if len(cmd) == 0 {
		// Without a readiness command from the plugin, a completed TCP handshake is the strongest
		// engine-agnostic signal available. It is weaker than the command — a listening socket is
		// not a server ready to answer — which is why plugins should declare one.
		return p.probeTCP(ctx, box)
	}
	return p.probeExec(ctx, box, cmd)
}

// probeTCP dials the sandbox's published port.
func (p *DockerProvider) probeTCP(ctx context.Context, box *dockerSandbox) error {
	dialer := net.Dialer{}
	address := net.JoinHostPort(box.creds.GetHost(), strconv.Itoa(int(box.creds.GetPort())))
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// probeExec runs the template's readiness command inside the container and checks its exit code.
func (p *DockerProvider) probeExec(ctx context.Context, box *dockerSandbox, cmd []string) error {
	created, err := p.cli.ExecCreate(ctx, box.containerID, client.ExecCreateOptions{Cmd: cmd})
	if err != nil {
		return fmt.Errorf("create readiness exec: %w", err)
	}

	// Detached, with no attached streams: the exit code is the whole answer, and hijacking a
	// connection every 500ms to discard its output would be pure overhead.
	if _, err := p.cli.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("start readiness exec: %w", err)
	}

	for {
		inspected, err := p.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect readiness exec: %w", err)
		}
		if !inspected.Running {
			if inspected.ExitCode != 0 {
				return fmt.Errorf("readiness command exited with status %d", inspected.ExitCode)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// --- Cleanup -------------------------------------------------------------------------------------

// Sweep destroys sandboxes this process does not own.
//
// Called at startup, that means every sandbox left behind by a control plane that was killed
// mid-verification — the case where a leak actually happens, and the one a deferred teardown can
// never cover.
//
// The owner label is what makes this safe to run while sandboxes of our own are live. It is not
// enough to make it safe for two control planes to share one Docker daemon: the second to start
// would destroy the first's sandboxes. That deployment needs a distinct label prefix per control
// plane, and it is worth a note here because the failure would look like a random verification
// failure rather than a configuration mistake.
func (p *DockerProvider) Sweep(ctx context.Context) (int, error) {
	found, err := p.list(ctx)
	if err != nil {
		return 0, err
	}

	removed := 0
	var errs []error
	for _, summary := range found {
		if summary.Labels[p.labels.owner] == p.owner {
			continue
		}
		p.log.Warn("removing an orphaned sandbox left by a previous process",
			slog.String("container_id", summary.ID),
			slog.String("sandbox_id", summary.Labels[p.labels.id]),
			slog.String("engine_type", summary.Labels[p.labels.engine]),
			slog.String("created_at", summary.Labels[p.labels.createdAt]))

		if err := p.remove(ctx, summary.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}

	return removed, errors.Join(errs...)
}

// reapLoop enforces MaxLifetime on this process's own sandboxes.
//
// It is the defence against a verification that hung rather than failed: the deferred teardown is
// still pending, waiting on something that will never return, so nothing else will ever remove the
// container.
func (p *DockerProvider) reapLoop() {
	defer close(p.reaperGone)

	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), destroyTimeout)
			if n, err := p.reapExpired(ctx); err != nil {
				p.log.Error("sandbox lifetime reaper failed", slog.String("error", err.Error()))
			} else if n > 0 {
				p.log.Warn("destroyed sandboxes that outlived their ceiling", slog.Int("count", n))
			}
			cancel()
		}
	}
}

// reapExpired destroys our own sandboxes whose expiry has passed.
func (p *DockerProvider) reapExpired(ctx context.Context) (int, error) {
	found, err := p.list(ctx)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	removed := 0
	var errs []error
	for _, summary := range found {
		if summary.Labels[p.labels.owner] != p.owner {
			// Another process's sandbox. Sweep owns that decision, and only at startup.
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, summary.Labels[p.labels.expiresAt])
		if err != nil {
			// An unreadable expiry is treated as expired. A sandbox whose ceiling cannot be read
			// is exactly the kind that leaks, and there is nothing in it worth keeping.
			p.log.Warn("sandbox has an unreadable expiry label; destroying it",
				slog.String("container_id", summary.ID))
		} else if expiresAt.After(now) {
			continue
		}

		p.log.Warn("destroying a sandbox that outlived its ceiling",
			slog.String("container_id", summary.ID),
			slog.String("sandbox_id", summary.Labels[p.labels.id]),
			slog.String("expires_at", summary.Labels[p.labels.expiresAt]))

		if err := p.remove(ctx, summary.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}

	return removed, errors.Join(errs...)
}

// list returns every container carrying the sandbox marker label, running or not.
func (p *DockerProvider) list(ctx context.Context) ([]container.Summary, error) {
	result, err := p.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", p.labels.marker),
	})
	if err != nil {
		return nil, fmt.Errorf("list sandbox containers: %w", err)
	}
	return result.Items, nil
}

// remove force-removes a container and its anonymous volumes. A container that is already gone is
// a success — remove races the reaper, the sweep, and the caller's defer by design.
func (p *DockerProvider) remove(ctx context.Context, containerID string) error {
	_, err := p.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err == nil || cerrdefs.IsNotFound(err) {
		return nil
	}
	if cerrdefs.IsConflict(err) {
		// A removal is already in flight. Give it a moment rather than reporting a leak.
		_, _ = p.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: stopTimeoutSeconds()})
		_, err = p.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		if err == nil || cerrdefs.IsNotFound(err) {
			return nil
		}
	}
	return fmt.Errorf("remove sandbox container %s: %w", containerID, err)
}

func stopTimeoutSeconds() *int {
	seconds := int(stopTimeout.Seconds())
	return &seconds
}

// --- The sandbox handle --------------------------------------------------------------------------

// dockerSandbox is one provisioned container.
type dockerSandbox struct {
	provider      *DockerProvider
	id            string
	containerID   string
	containerName string
	containerPort network.Port

	creds *fwv1.Credentials

	// cleanupShare removes the shared directory this sandbox was given, if it was given one. A
	// verified artifact is a full copy of a production database, so it does not outlive the
	// container it was restored into.
	cleanupShare func()

	destroyOnce sync.Once
	destroyErr  error
}

// ID returns the sandbox identifier, which is also on the container's labels.
func (s *dockerSandbox) ID() string { return s.id }

// identity is what a readiness command is rendered against. The port is the one inside the
// container, because that is where a command running inside it has to look, and the password is
// the one the container was created with, so the probe can authenticate if the engine requires it.
func (s *dockerSandbox) identity() identity {
	return identity{
		Username: s.creds.GetUsername(),
		Password: s.creds.GetPassword(),
		Database: s.creds.GetDatabase(),
		Port:     int32(s.containerPort.Num()),
	}
}

// Credentials returns a copy, so that a caller stashing them cannot mutate what the sandbox
// believes about itself.
func (s *dockerSandbox) Credentials() *fwv1.Credentials {
	if s.creds == nil {
		return nil
	}
	return proto.CloneOf(s.creds)
}

// Destroy removes the container. It is idempotent: the second call returns the first call's result
// without touching Docker, because Destroy runs from a defer and may also be reached through the
// reaper or a sweep.
func (s *dockerSandbox) Destroy(ctx context.Context) error {
	s.destroyOnce.Do(func() {
		s.destroyErr = s.provider.remove(ctx, s.containerID)
		// The directory goes even when the container did not, because leaving a restored copy of a
		// production database on disk is the worse of the two leaks.
		if s.cleanupShare != nil {
			s.cleanupShare()
		}
		if s.destroyErr == nil {
			s.provider.log.Info("sandbox destroyed", slog.String("sandbox_id", s.id))
		}
	})
	return s.destroyErr
}
