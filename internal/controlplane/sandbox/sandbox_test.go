package sandbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
)

// sandboxConfig mirrors the defaults in internal/config, so a test exercises what a developer
// running `docker compose up` actually gets. The integration tests share it.
func sandboxConfig(provider string) config.SandboxConfig {
	return config.SandboxConfig{
		Provider:       provider,
		StartupTimeout: 3 * time.Minute,
		MaxLifetime:    2 * time.Hour,
		LabelPrefix:    "fleetward",
	}
}

func TestSpecValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    Spec
		wantErr error
	}{
		{
			name: "a usable template",
			spec: Spec{Template: &fwv1.SandboxTemplate{ImageRepository: "postgres", ContainerPort: 5432}},
		},
		{
			// An engine whose plugin declares no template simply cannot be verified. That is a
			// capability gap the caller reports, not a failure it retries.
			name:    "no template",
			spec:    Spec{},
			wantErr: ErrNoTemplate,
		},
		{
			name:    "no image",
			spec:    Spec{Template: &fwv1.SandboxTemplate{ContainerPort: 5432}},
			wantErr: ErrInvalidTemplate,
		},
		{
			name:    "no port",
			spec:    Spec{Template: &fwv1.SandboxTemplate{ImageRepository: "postgres"}},
			wantErr: ErrInvalidTemplate,
		},
		{
			name:    "a port outside the range",
			spec:    Spec{Template: &fwv1.SandboxTemplate{ImageRepository: "postgres", ContainerPort: 70000}},
			wantErr: ErrInvalidTemplate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.spec.validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRenderEnvSubstitutesTheSandboxIdentity is the core of ADR-0020: the plugin says where the
// credentials belong, core says what they are, and core never learns that POSTGRES_PASSWORD means
// anything in particular.
func TestRenderEnvSubstitutesTheSandboxIdentity(t *testing.T) {
	t.Parallel()

	id := identity{Username: "fleetward", Password: "s3cret", Database: "sandbox", Port: 5432}
	env := map[string]string{
		"POSTGRES_USER":     "{{ .Username }}",
		"POSTGRES_PASSWORD": "{{ .Password }}",
		"POSTGRES_DB":       "{{ .Database }}",
		"PGPORT":            "{{ .Port }}",
		"LITERAL":           "unchanged",
	}

	got, err := renderEnv(env, id)
	if err != nil {
		t.Fatalf("renderEnv: %v", err)
	}

	// Sorted, so a container's configuration is reproducible and two sandboxes diff cleanly.
	want := []string{
		"LITERAL=unchanged",
		"PGPORT=5432",
		"POSTGRES_DB=sandbox",
		"POSTGRES_PASSWORD=s3cret",
		"POSTGRES_USER=fleetward",
	}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRenderAllPreservesArgumentBoundaries(t *testing.T) {
	t.Parallel()

	// A generated password is base64, so it holds no spaces — but a caller-supplied database name
	// might, and rendering a joined command would silently split the argument.
	id := identity{Username: "fleetward", Password: "a b", Database: "two words"}

	got, err := renderAll([]string{"pg_isready", "-U", "{{ .Username }}", "-d", "{{ .Database }}"}, id)
	if err != nil {
		t.Fatalf("renderAll: %v", err)
	}

	want := []string{"pg_isready", "-U", "fleetward", "-d", "two words"}
	if len(got) != len(want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRenderRejectsAnUnknownField(t *testing.T) {
	t.Parallel()

	// Starting a database with an empty password because a template had a typo would be worse than
	// not starting it at all.
	if _, err := render("{{ .Secret }}", identity{}); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("error = %v, want ErrInvalidTemplate", err)
	}
	if _, err := render("{{ .Username", identity{}); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("error = %v, want ErrInvalidTemplate", err)
	}
}

func TestNewIdentityGeneratesADistinctPasswordEveryTime(t *testing.T) {
	t.Parallel()

	spec := Spec{Template: &fwv1.SandboxTemplate{ImageRepository: "postgres", ContainerPort: 5432}}

	seen := make(map[string]bool, 16)
	for range 16 {
		id, err := newIdentity(spec)
		if err != nil {
			t.Fatalf("newIdentity: %v", err)
		}
		if id.Username != defaultSandboxUsername || id.Database != defaultSandboxDatabase {
			t.Fatalf("identity = %+v, want the defaults", id)
		}
		if len(id.Password) < 24 {
			t.Fatalf("password is only %d characters", len(id.Password))
		}
		if seen[id.Password] {
			t.Fatal("a sandbox password was generated twice")
		}
		seen[id.Password] = true
	}
}

func TestNewIdentityHonoursOverrides(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Template: &fwv1.SandboxTemplate{ImageRepository: "postgres", ContainerPort: 5432},
		Username: "restorer",
		Database: "orders",
	}

	id, err := newIdentity(spec)
	if err != nil {
		t.Fatalf("newIdentity: %v", err)
	}
	if id.Username != "restorer" || id.Database != "orders" || id.Port != 5432 {
		t.Fatalf("identity = %+v", id)
	}
}

func TestNewLabelsUsesTheConfiguredPrefix(t *testing.T) {
	t.Parallel()

	// The marker key is what an operator greps for with `docker ps --filter label=...`, so its
	// shape is part of the contract with them.
	if got := newLabels("fleetward").marker; got != "fleetward.sandbox" {
		t.Errorf("marker = %q, want %q", got, "fleetward.sandbox")
	}
	if got := newLabels("acme").id; got != "acme.sandbox.id" {
		t.Errorf("id label = %q, want %q", got, "acme.sandbox.id")
	}
	if got := newLabels("").marker; got != "fleetward.sandbox" {
		t.Errorf("marker with an empty prefix = %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"fleetward", "fleetward"},
		{"acme.io/fleetward", "acme.io-fleetward"},
		{"a b", "a-b"},
		{"...", "fleetward"},
		{"", "fleetward"},
	}

	for _, tc := range tests {
		if got := sanitizeName(tc.in); got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNewRejectsAnUnknownProvider keeps a typo in configuration from silently disabling
// verification.
func TestNewRejectsAnUnknownProvider(t *testing.T) {
	t.Parallel()

	if _, err := New(sandboxConfig("podman"), nil); err == nil ||
		!strings.Contains(err.Error(), "unknown sandbox provider") {
		t.Fatalf("error = %v, want an unknown-provider error", err)
	}
	if _, err := New(sandboxConfig("kubernetes"), nil); err == nil ||
		!strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error = %v, want a not-implemented error", err)
	}
}
