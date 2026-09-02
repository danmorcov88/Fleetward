// Package sandbox provisions throwaway database instances used to verify that a backup is
// actually restorable, and guarantees they are destroyed.
//
// Everything the provider needs in order to stand an engine up comes from the plugin's
// SandboxTemplate: the image repository, how to derive a tag from the discovered version, the
// environment, the port, and how to tell when it is ready. Core holds no table of engines, because
// the moment it does, adding an engine stops being a plugin-only change (CLAUDE.md §4).
//
// The one thing core does own is identity. A sandbox gets a generated username, password, and
// database name; the template says where they belong by referencing them as {{ .Username }},
// {{ .Password }}, and {{ .Database }} in its environment and commands. See ADR-0020. A template
// may narrow that: an image whose administrative account cannot be renamed declares
// fixed_username, and an image that validates the password it is handed declares a password_policy.
// Neither tells core which engine it is talking to.
//
// A template may also ask for a directory the plugin can reach — shared_directory — for an engine
// that hands its artifact over as a file rather than as a stream (ADR-0026). Core creates it,
// mounts it, reports both of its names in the sandbox's credentials, and removes it with the
// sandbox. What goes in it is the plugin's business.
//
// Cleanup is defended three times over, because a leaked sandbox does not break anything — it
// quietly consumes a machine until someone notices:
//
//  1. Run destroys the sandbox on every return path, including a panic.
//  2. A lifetime reaper destroys any sandbox past its ceiling, catching a verification that hung
//     rather than failed.
//  3. Sweep, called at startup, destroys sandboxes labelled by a previous process — the leak that
//     happens when the control plane is killed mid-verification.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"strings"
	"text/template"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
)

// Errors a caller is expected to distinguish. Everything else is wrapped with context.
var (
	// ErrNoTemplate means the plugin declared no sandbox template, so its engine cannot be
	// verified. It is a capability gap, not a failure.
	ErrNoTemplate = errors.New("the plugin declares no sandbox template")

	// ErrInvalidTemplate means the template is present but unusable.
	ErrInvalidTemplate = errors.New("invalid sandbox template")

	// ErrNotReady means the container started but never became usable within the startup timeout.
	ErrNotReady = errors.New("sandbox did not become ready")

	// ErrClosed means the provider has been shut down.
	ErrClosed = errors.New("sandbox provider is closed")
)

// Spec is what core needs to stand up a sandbox. Every field describing the engine originates in
// the plugin's SandboxTemplate; core adds only identity and lifetime.
type Spec struct {
	// EngineType is carried for labelling and logging only. Nothing branches on it, and nothing
	// ever should.
	EngineType string
	// EngineVersion is the normalized version reported by Discover, used to resolve the image tag.
	// Empty falls back to the template's default_tag.
	EngineVersion string
	// Template comes from the plugin's Capabilities.
	Template *fwv1.SandboxTemplate
	// Username overrides the generated sandbox username. Empty uses the default.
	Username string
	// Database overrides the generated sandbox database name. Empty uses the default.
	Database string
	// Labels are attached to the container in addition to the ones the provider owns. They are for
	// tracing a sandbox back to the job that asked for it; provider labels win on collision.
	Labels map[string]string
	// Lifetime overrides the configured MaxLifetime for this one sandbox. Zero uses the default.
	Lifetime time.Duration
}

// Provider stands sandboxes up and tears them down. Implementations must be safe for concurrent
// use, and must not expose their runtime's types: Docker today, Kubernetes Jobs later.
type Provider interface {
	// Provision returns a sandbox that is ready to accept connections. On any failure it leaves
	// nothing behind.
	Provision(ctx context.Context, spec Spec) (Sandbox, error)

	// Sweep destroys sandboxes left behind by a previous process. Called at startup.
	Sweep(ctx context.Context) (int, error)

	// HealthCheck reports whether the container runtime is reachable.
	HealthCheck(ctx context.Context) error

	// Close stops background reaping and releases the runtime client. It does not destroy live
	// sandboxes: the caller's defer owns those, and the next startup's Sweep is the backstop.
	Close() error
}

// Sandbox is one running throwaway instance.
type Sandbox interface {
	// ID is the provider-scoped identifier, stable for the sandbox's lifetime.
	ID() string

	// Credentials connect to the sandbox, ready to hand to a plugin's Restore.
	Credentials() *fwv1.Credentials

	// Destroy is idempotent and safe to call from a defer. A sandbox that is already gone is a
	// success: Destroy runs from the deferred path, and may race the lifetime reaper or a sweep.
	Destroy(ctx context.Context) error
}

// New builds the configured provider (ADR-0003 keeps engines in plugins; this keeps runtimes here).
//
// It does not contact the container runtime. A control plane whose Docker socket is missing must
// still start and report itself degraded, exactly as it does for object storage — an operator
// cannot fix a broken daemon from a control plane that refuses to boot.
func New(cfg config.SandboxConfig, log *slog.Logger) (Provider, error) {
	switch strings.ToLower(cfg.Provider) {
	case "docker":
		return NewDockerProvider(cfg, log)
	case "kubernetes":
		return nil, errors.New(`the "kubernetes" sandbox provider is not implemented yet; use "docker"`)
	default:
		return nil, fmt.Errorf("unknown sandbox provider %q; supported: docker", cfg.Provider)
	}
}

// Run provisions a sandbox, hands it to fn, and destroys it on every path out — including a panic.
//
// This is the first of the three cleanup defences, and the reason it is a function rather than a
// documented convention is that a convention is exactly what gets forgotten at the one call site
// that panics.
func Run(ctx context.Context, p Provider, spec Spec, fn func(context.Context, Sandbox) error) (err error) {
	box, err := p.Provision(ctx, spec)
	if err != nil {
		return err
	}

	defer func() {
		// The destroy context is deliberately not derived from ctx. Verification failing usually
		// means ctx was cancelled, and a cancelled context cannot be used to clean up.
		destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyTimeout)
		defer cancel()

		if derr := box.Destroy(destroyCtx); derr != nil && err == nil {
			err = fmt.Errorf("destroy sandbox: %w", derr)
		}
	}()

	return fn(ctx, box)
}

// destroyTimeout bounds a teardown. It is generous because failing to remove a container is the
// one outcome this package exists to prevent.
const destroyTimeout = 60 * time.Second

// Credentials core generates for a sandbox. The plugin's template decides where they land.
const (
	defaultSandboxUsername = "fleetward"
	defaultSandboxDatabase = "fleetward_sandbox"
	// generatedPasswordBytes is the entropy behind a sandbox password. The sandbox is short-lived
	// and bound to loopback, but it holds a full copy of a production database while it is up.
	generatedPasswordBytes = 24
)

// identity is core's contribution to a sandbox: who connects to it, and how.
type identity struct {
	Username string
	Password string
	Database string
	// Port is the port inside the container, which a readiness command usually needs.
	Port int32
}

// newIdentity generates the credentials for one sandbox.
//
// The username is core's to generate unless the image will not accept one: an engine whose
// administrative account is fixed says so in its template, and core uses that name rather than
// inventing one the image would ignore. The password is never the plugin's to choose.
func newIdentity(spec Spec) (identity, error) {
	password, err := generatePassword(spec.Template.GetPasswordPolicy())
	if err != nil {
		return identity{}, err
	}

	id := identity{
		Username: spec.Username,
		Password: password,
		Database: spec.Database,
		Port:     spec.Template.GetContainerPort(),
	}
	if fixed := spec.Template.GetFixedUsername(); fixed != "" {
		// The image creates this account and cannot be told to rename it, so a generated name
		// would produce a sandbox nobody can log in to. A caller's override loses to it for the
		// same reason.
		id.Username = fixed
	}
	if id.Username == "" {
		id.Username = defaultSandboxUsername
	}
	if id.Database == "" {
		id.Database = defaultSandboxDatabase
	}
	return id, nil
}

// Character classes a password policy can require. The order is the order a generated password
// draws its guaranteed characters in, so it is stable rather than incidental.
const (
	passwordUpper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	passwordLower   = "abcdefghijklmnopqrstuvwxyz"
	passwordDigits  = "0123456789"
	passwordSymbols = "-_"
	// passwordClasses is how many there are, and therefore the ceiling on min_character_classes.
	passwordClasses = 4
)

// generatePassword returns a random password that satisfies the engine's policy.
//
// With no policy it is base64url of generatedPasswordBytes, unchanged from ADR-0020: URL-safe, so
// free of anything a shell-quoted readiness command or a connection parameter would have to
// escape.
//
// With a policy it is built from the same alphabet, but the required classes are placed first and
// the result shuffled — satisfied by construction rather than by generating and retrying. The
// difference matters: mcr.microsoft.com/mssql/server refuses to start unless three of the four
// classes appear, and base64url of 24 bytes misses two of them about once in eight hundred. A
// rejection-sampling generator would hide that behind a loop with no upper bound; this one cannot
// fail on a legal policy.
func generatePassword(policy *fwv1.PasswordPolicy) (string, error) {
	if policy == nil || (policy.GetMinLength() <= 0 && policy.GetMinCharacterClasses() <= 0) {
		buf := make([]byte, generatedPasswordBytes)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate sandbox password: %w", err)
		}
		return base64.RawURLEncoding.EncodeToString(buf), nil
	}

	symbols := policy.GetSymbolAlphabet()
	if symbols == "" {
		symbols = passwordSymbols
	}
	classes := []string{passwordUpper, passwordLower, passwordDigits, symbols}

	required := int(policy.GetMinCharacterClasses())
	if required > passwordClasses {
		return "", fmt.Errorf("%w: min_character_classes of %d exceeds the %d classes that exist",
			ErrInvalidTemplate, required, passwordClasses)
	}

	// The default length is what an unconstrained sandbox password already has, so declaring a
	// policy never shortens one.
	length := base64.RawURLEncoding.EncodedLen(generatedPasswordBytes)
	if min := int(policy.GetMinLength()); min > length {
		length = min
	}
	if required > length {
		return "", fmt.Errorf("%w: min_character_classes of %d cannot fit in %d characters",
			ErrInvalidTemplate, required, length)
	}

	pool := strings.Join(classes, "")
	out := make([]byte, 0, length)
	for i := range required {
		c, err := randomChar(classes[i])
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < length {
		c, err := randomChar(pool)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}

	if err := shuffle(out); err != nil {
		return "", err
	}
	return string(out), nil
}

// randomChar picks one character uniformly from an alphabet.
func randomChar(alphabet string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
	if err != nil {
		return 0, fmt.Errorf("generate sandbox password: %w", err)
	}
	return alphabet[n.Int64()], nil
}

// shuffle is Fisher-Yates over a cryptographic source, so the guaranteed characters do not sit at
// a predictable offset.
func shuffle(b []byte) error {
	for i := len(b) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fmt.Errorf("generate sandbox password: %w", err)
		}
		j := n.Int64()
		b[i], b[j] = b[j], b[i]
	}
	return nil
}

// randomID returns a short identifier used in container names and labels.
func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate sandbox id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// render substitutes the sandbox identity into one template string.
//
// A value with no action in it is returned unchanged, so a template that predates ADR-0020 keeps
// working. A value referencing a field that does not exist is an error rather than an empty
// string: silently starting a database with an empty password would be worse than not starting it.
func render(value string, id identity) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}

	tmpl, err := template.New("sandbox").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("%w: parse %q: %w", ErrInvalidTemplate, value, err)
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, id); err != nil {
		return "", fmt.Errorf("%w: render %q: %w", ErrInvalidTemplate, value, err)
	}
	return out.String(), nil
}

// renderAll renders every element of a command, preserving argument boundaries. Rendering the
// joined string instead would let a generated password containing a space split an argument.
func renderAll(values []string, id identity) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		rendered, err := render(v, id)
		if err != nil {
			return nil, err
		}
		out[i] = rendered
	}
	return out, nil
}

// renderEnv turns the template's environment into the KEY=VALUE form a container runtime wants,
// with the sandbox identity substituted. Keys are sorted so a container's configuration is
// reproducible and a diff between two sandboxes is readable.
func renderEnv(env map[string]string, id identity) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}

	keys := slices.Sorted(maps.Keys(env))

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		value, err := render(env[k], id)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", k, err)
		}
		out = append(out, k+"="+value)
	}
	return out, nil
}

// validate rejects a spec that cannot produce a container, before anything is created.
func (s Spec) validate() error {
	if s.Template == nil {
		return ErrNoTemplate
	}
	if s.Template.GetImageRepository() == "" {
		return fmt.Errorf("%w: image_repository is required", ErrInvalidTemplate)
	}
	port := s.Template.GetContainerPort()
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%w: container_port %d is out of range", ErrInvalidTemplate, port)
	}
	return nil
}
