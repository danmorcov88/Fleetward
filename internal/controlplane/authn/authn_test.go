package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The chain is three links deep and the ordering between them is a real decision, not a style
// choice. This file exists because getting it wrong produced a defect that every other test passed
// and that only appeared against a running stack: the bootstrap credential — the one thing that
// gets a fresh installation its first token — did not work at all.
//
// The cause is the asymmetry these tests pin down. A link that compares against one known string
// can say "not mine". The token store cannot: a bearer value it has never seen is an *invalid
// credential*, not somebody else's, and a chain that kept going after an invalid credential would
// let a revoked token fall through to whatever came next. So the store is terminal, and anything
// that has to see a bearer value goes in front of it.

// stubAuthenticator answers with whatever it was built to answer.
type stubAuthenticator struct {
	name string
	err  error
}

func (s *stubAuthenticator) Authenticate(context.Context, *http.Request) (Principal, error) {
	if s.err != nil {
		return Principal{}, s.err
	}
	return Principal{Kind: KindUser, Actor: s.name, TenantID: "t"}, nil
}

func request(t *testing.T, header string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/instances", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestTheChainSkipsLinksThatRecogniseNothing(t *testing.T) {
	chain := Chain{
		&stubAuthenticator{err: ErrNoCredential},
		&stubAuthenticator{err: ErrNoCredential},
		&stubAuthenticator{name: "third"},
	}
	p, err := chain.Authenticate(context.Background(), request(t, ""))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Actor != "third" {
		t.Fatalf("actor = %q, want the third link to have answered", p.Actor)
	}
}

// TestAnInvalidCredentialStopsTheChain is the property that makes the store terminal. Without it, a
// revoked token would be retried against every later link until something accepted it.
func TestAnInvalidCredentialStopsTheChain(t *testing.T) {
	after := &stubAuthenticator{name: "should never be reached"}
	chain := Chain{
		&stubAuthenticator{err: ErrInvalidCredential},
		after,
	}
	if _, err := chain.Authenticate(context.Background(), request(t, "")); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate = %v, want the chain to stop on an invalid credential", err)
	}
}

// TestTheBootstrapCredentialIsReachable is the regression test for the defect the B6 walk found.
//
// The bootstrap link has to sit in front of the token store, because a bootstrap token is by
// definition a bearer value the database has never seen. With the store first, the store called it
// invalid, the chain stopped, and the credential that exists to get a fresh installation started
// could never be used — on any installation, with any bootstrap value.
func TestTheBootstrapCredentialIsReachable(t *testing.T) {
	const configured = "fwt_devbootstrap_0000000000000000"
	bootstrap := NewBootstrapAuthenticator(configured, "tenant-1")

	// The store stands in for the real one: it has never seen this value, so it calls it invalid.
	store := &stubAuthenticator{err: ErrInvalidCredential}
	chain := Chain{bootstrap, store}

	p, err := chain.Authenticate(context.Background(), request(t, "Bearer "+configured))
	if err != nil {
		t.Fatalf("the configured bootstrap credential was refused: %v\n"+
			"the bootstrap link must come before the token store; see the B6 journal", err)
	}
	if p.Kind != KindBootstrap {
		t.Fatalf("kind = %v, want KindBootstrap", p.Kind)
	}
	if p.Actor != BootstrapActor {
		t.Fatalf("actor = %q, want %q so its use is visible in the audit log", p.Actor, BootstrapActor)
	}
	if p.TenantID != "tenant-1" {
		t.Fatalf("tenant = %q, want the configured tenant", p.TenantID)
	}
}

// TestAnotherBearerValueFallsPastTheBootstrapLink is the other half: the bootstrap link must not
// swallow credentials that are not it, or no real token would ever reach the store.
func TestAnotherBearerValueFallsPastTheBootstrapLink(t *testing.T) {
	bootstrap := NewBootstrapAuthenticator("the-configured-one", "tenant-1")
	store := &stubAuthenticator{name: "a real token"}

	p, err := chainOf(bootstrap, store).Authenticate(context.Background(),
		request(t, "Bearer fwt_something_else"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Actor != "a real token" {
		t.Fatalf("actor = %q, want the token store to have answered", p.Actor)
	}
}

func TestNoBootstrapTokenMeansNoBootstrapLink(t *testing.T) {
	if a := NewBootstrapAuthenticator("", "tenant-1"); a != nil {
		t.Fatal("an unconfigured bootstrap credential produced a link; the chain must simply be shorter")
	}
}

func chainOf(bootstrap *BootstrapAuthenticator, rest ...Authenticator) Chain {
	c := Chain{bootstrap}
	return append(c, rest...)
}

// -------------------------------------------------------------------------------------------------
// Reading a credential off a request
// -------------------------------------------------------------------------------------------------

func TestBearerIsReadFromTheHeaderAndNowhereElse(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{name: "a bearer token", header: "Bearer fwt_a_b", want: "fwt_a_b", ok: true},
		{name: "the scheme is not case sensitive", header: "bearer fwt_a_b", want: "fwt_a_b", ok: true},
		{name: "surrounding space is trimmed", header: "Bearer   fwt_a_b  ", want: "fwt_a_b", ok: true},
		{name: "no header", header: ""},
		{name: "another scheme", header: "Basic dXNlcjpwYXNz"},
		{name: "a scheme and nothing else", header: "Bearer"},
		{name: "a scheme and only space", header: "Bearer    "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := BearerFrom(request(t, tt.header))
			if ok != tt.ok || got != tt.want {
				t.Fatalf("BearerFrom = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}

	// A token in a query string would be recorded by the access log, which prints the path of every
	// request. Nothing reads one from there, and this asserts it.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/instances?token=fwt_a_b", nil)
	if _, ok := BearerFrom(r); ok {
		t.Fatal("a credential was read from the query string; the access log prints that path")
	}
}

// -------------------------------------------------------------------------------------------------
// The tenant, and why an absent one is the empty string
// -------------------------------------------------------------------------------------------------

func TestTenantIsEmptyWithoutAPrincipal(t *testing.T) {
	// Deliberate: every query filters `tenant_id = $1` against a UUID column, so the empty string is
	// rejected by Postgres. A path that reaches the database without a principal fails loudly rather
	// than quietly reading the default tenant, which is what a hardcoded constant used to do.
	if got := Tenant(context.Background()); got != "" {
		t.Fatalf("Tenant on a bare context = %q, want the empty string", got)
	}
	if _, err := MustFrom(context.Background()); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("MustFrom on a bare context = %v, want ErrNoPrincipal", err)
	}

	ctx := WithPrincipal(context.Background(), System("scheduler", "tenant-1"))
	if got := Tenant(ctx); got != "tenant-1" {
		t.Fatalf("Tenant = %q, want the system principal's tenant", got)
	}
}

func TestASystemPrincipalIsNamedAndHoldsNothing(t *testing.T) {
	p := System("retention", "tenant-1")
	if p.Actor != "system:retention" {
		t.Fatalf(`actor = %q, want "system:retention"`, p.Actor)
	}
	if p.UserID != "" {
		t.Fatal("a system principal has a user id; it must have no row in users (ADR-0036)")
	}
	if len(p.Grants) != 0 {
		t.Fatal("a system principal holds grants; the guard allows it outright instead")
	}
	if !p.Authenticated() {
		t.Fatal("a system principal reports itself unauthenticated")
	}
}

// -------------------------------------------------------------------------------------------------
// The token format
// -------------------------------------------------------------------------------------------------

func TestATokenSplitsIntoAPublicHalfAndASecret(t *testing.T) {
	presented, tokenID, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	gotID, secret, err := splitToken(presented)
	if err != nil {
		t.Fatalf("splitToken(%q): %v", presented, err)
	}
	if gotID != tokenID {
		t.Fatalf("token id = %q, want %q", gotID, tokenID)
	}
	if hashSecret(secret) != hash {
		t.Fatal("the stored hash does not match the secret half")
	}
	// The secret must not be recoverable from what is stored.
	if hash == secret {
		t.Fatal("the stored value is the secret itself")
	}

	for _, malformed := range []string{"", "fwt", "fwt_only-two", "other_a_b", "fwt__b", "fwt_a_"} {
		if _, _, err := splitToken(malformed); err == nil {
			t.Errorf("splitToken(%q) accepted a malformed credential", malformed)
		}
	}
}

func TestTwoTokensAreNeverTheSame(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		presented, _, _, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[presented] {
			t.Fatal("NewToken returned a duplicate")
		}
		seen[presented] = true
	}
}
