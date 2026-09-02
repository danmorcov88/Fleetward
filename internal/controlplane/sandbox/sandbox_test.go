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

// TestAPasswordPolicyIsSatisfiedByConstruction is the regression test for a bug that had not
// happened yet.
//
// mcr.microsoft.com/mssql/server refuses to start unless its password carries three of the four
// character classes, and base64url of 24 bytes misses two of them roughly once in eight hundred.
// One in eight hundred is the worst frequency a defect can have: too rare to reproduce, common
// enough to fire. Sixteen samples would not catch it either, so this asserts the property rather
// than sampling for the absence of the failure.
func TestAPasswordPolicyIsSatisfiedByConstruction(t *testing.T) {
	t.Parallel()

	spec := Spec{Template: &fwv1.SandboxTemplate{
		ImageRepository: "mcr.microsoft.com/mssql/server",
		ContainerPort:   1433,
		PasswordPolicy:  &fwv1.PasswordPolicy{MinLength: 32, MinCharacterClasses: 3},
	}}

	for range 512 {
		id, err := newIdentity(spec)
		if err != nil {
			t.Fatalf("newIdentity: %v", err)
		}
		if len(id.Password) < 32 {
			t.Fatalf("password %q is only %d characters, want at least 32", id.Password, len(id.Password))
		}
		if classes := characterClasses(id.Password); classes < 3 {
			t.Fatalf("password %q carries %d character classes, want at least 3", id.Password, classes)
		}
	}
}

// TestAPasswordWithoutAPolicyIsUnchanged pins ADR-0020's generator for every plugin that declares
// no policy, so adding the policy did not quietly change what PostgreSQL's sandboxes get.
func TestAPasswordWithoutAPolicyIsUnchanged(t *testing.T) {
	t.Parallel()

	spec := Spec{Template: &fwv1.SandboxTemplate{ImageRepository: "postgres", ContainerPort: 5432}}

	id, err := newIdentity(spec)
	if err != nil {
		t.Fatalf("newIdentity: %v", err)
	}
	if len(id.Password) != 32 {
		t.Fatalf("password is %d characters, want the 32 of base64url over 24 bytes", len(id.Password))
	}
	if strings.ContainsFunc(id.Password, func(r rune) bool {
		return !strings.ContainsRune(passwordUpper+passwordLower+passwordDigits+passwordSymbols, r)
	}) {
		t.Fatalf("password %q left the URL-safe alphabet", id.Password)
	}
}

// TestAnUnsatisfiablePolicyIsRefusedRatherThanApproximated. A policy core cannot meet has to fail
// loudly here, where the message names the template, rather than produce a password the engine
// will reject at startup with a diagnosis nobody reads.
func TestAnUnsatisfiablePolicyIsRefusedRatherThanApproximated(t *testing.T) {
	t.Parallel()

	spec := Spec{Template: &fwv1.SandboxTemplate{
		ImageRepository: "example",
		ContainerPort:   1234,
		PasswordPolicy:  &fwv1.PasswordPolicy{MinCharacterClasses: 5},
	}}

	if _, err := newIdentity(spec); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("error = %v, want ErrInvalidTemplate", err)
	}
}

// TestAFixedUsernameWins covers the engine whose administrative account cannot be renamed. A
// generated name would produce a sandbox nobody can log in to, so it loses even to a caller's
// explicit override.
func TestAFixedUsernameWins(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Template: &fwv1.SandboxTemplate{
			ImageRepository: "mcr.microsoft.com/mssql/server",
			ContainerPort:   1433,
			FixedUsername:   "sa",
		},
		Username: "restorer",
		Database: "orders",
	}

	id, err := newIdentity(spec)
	if err != nil {
		t.Fatalf("newIdentity: %v", err)
	}
	if id.Username != "sa" {
		t.Errorf("username = %q, want the template's fixed %q", id.Username, "sa")
	}
	if id.Database != "orders" {
		t.Errorf("database = %q; a fixed username must not disturb the database override", id.Database)
	}
}

// characterClasses counts how many of the four classes a password draws from.
func characterClasses(password string) int {
	var found int
	for _, class := range []string{passwordUpper, passwordLower, passwordDigits, passwordSymbols} {
		if strings.ContainsAny(password, class) {
			found++
		}
	}
	return found
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
