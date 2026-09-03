package authn

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// BootstrapActor is the name every action taken with the break-glass credential is recorded under.
// It is visibly not a person, in a table that cannot be edited.
const BootstrapActor = "bootstrap"

// AuthDisabledActor is what appears in the audit log when authentication is switched off entirely.
// It is a different word from BootstrapActor on purpose: "somebody used the break-glass credential"
// and "this installation was not asking anybody who they were" are different facts, and a log that
// spells them the same way cannot be used to tell them apart afterwards.
const AuthDisabledActor = "auth-disabled"

// BootstrapAuthenticator recognises the credential an operator configures to get the first real
// token out of a fresh installation.
//
// The design decision worth carrying forward (ADR-0033) is what this is *not*: it is not a seeded
// administrator row. A seeded row would outlive the configuration that created it — it would
// survive the operator removing every environment variable, it would be invisible in a diff, and
// revoking it would require knowing it existed. This is configuration and only configuration:
// delete the setting, restart, and the access is gone with nothing left behind to find later.
//
// What it costs is that it cannot be revoked *without* touching configuration, and that a leaked
// bootstrap token is tenant-wide admin until somebody notices. Both are why the control plane logs
// a warning naming it on every start while it is set, and why every action it takes is audited
// under an actor that is obviously not a person.
type BootstrapAuthenticator struct {
	hash     [32]byte
	tenantID string
}

// NewBootstrapAuthenticator builds one, or returns nil when no bootstrap token is configured — in
// which case the chain simply has one fewer link.
func NewBootstrapAuthenticator(token, tenantID string) *BootstrapAuthenticator {
	if token == "" {
		return nil
	}
	return &BootstrapAuthenticator{hash: sha256.Sum256([]byte(token)), tenantID: tenantID}
}

// Authenticate implements Authenticator.
func (a *BootstrapAuthenticator) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	presented, ok := BearerFrom(r)
	if !ok {
		return Principal{}, ErrNoCredential
	}
	got := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(a.hash[:], got[:]) != 1 {
		// Not ours, rather than not valid. This link can tell the difference — it compares against
		// one known string — which is exactly why it sits in front of the token store rather than
		// behind it: the store cannot, so the store has to be last.
		return Principal{}, ErrNoCredential
	}
	return Principal{
		Kind:        KindBootstrap,
		Actor:       BootstrapActor,
		DisplayName: "Bootstrap credential",
		TenantID:    a.tenantID,
	}, nil
}

// LoadBootstrapToken reads the configured break-glass credential.
//
// The file form is preferred and the inline form is accepted, matching what
// `fleetward-cli keygen` already says about the secrets master key: anything that can read the
// process environment can read an environment variable.
func LoadBootstrapToken(inline, file string) (string, error) {
	switch {
	case file != "":
		return readTrimmedFile(file)
	case inline != "":
		return strings.TrimSpace(inline), nil
	default:
		return "", nil
	}
}

// readTrimmedFile reads an operator-configured credential file.
func readTrimmedFile(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured credential file path
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// -----------------------------------------------------------------------------------------------
// Authentication switched off
// -----------------------------------------------------------------------------------------------

// DisabledAuthenticator is what runs when FLEETWARD_AUTH_ENABLED is false: everybody is a
// tenant-wide administrator, and the audit log says so on every row.
//
// This is an escape hatch shipped with its limits, which is what ADR-0024 asks of every slice.
// `config.Validate` already refuses to start in production with authentication disabled, so the
// only thing that can reach this is a development stack — and even the development stack no longer
// uses it, because a quickstart with authorization off would mean the whole of B6 was never
// exercised by anything.
type DisabledAuthenticator struct {
	tenantID string
}

// NewDisabledAuthenticator builds one.
func NewDisabledAuthenticator(tenantID string) *DisabledAuthenticator {
	return &DisabledAuthenticator{tenantID: tenantID}
}

// Authenticate implements Authenticator: everybody is admin, and nobody is asked for anything.
func (a *DisabledAuthenticator) Authenticate(_ context.Context, _ *http.Request) (Principal, error) {
	return Principal{
		Kind:        KindBootstrap,
		Actor:       AuthDisabledActor,
		DisplayName: "Authentication disabled",
		TenantID:    a.tenantID,
	}, nil
}
