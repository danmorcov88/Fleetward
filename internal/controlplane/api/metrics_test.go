package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/authz"
)

// These tests are about the *refusal*, not the exposition format.
//
// Whether a histogram renders its buckets correctly is the OTel exporter's problem and it has its
// own tests. Whether somebody granted three servers can read a series per instance across fifty is
// this product's problem, and it is the one ADR-0042 decides.
//
// The rule that produces the decision lives in internal/controlplane/authz and is tested there
// against real principals — including the two kinds that hold no grants at all. What is tested here
// is what the endpoint does with each answer.

// fixedDecision states an authorization outcome directly.
type fixedDecision struct{ err error }

func (d fixedDecision) AuthorizeScrape(context.Context) error { return d.err }

// newTestServer builds a server with /metrics mounted behind the given authorizer.
func newTestServer(t *testing.T, handler http.Handler, auth ScrapeAuthorizer) *httptest.Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewServer(config.ServerConfig{Addr: "127.0.0.1:0"}, log, NewHealth(log, 0), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.RegisterMetrics(handler, auth)

	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts
}

// exposition stands in for the Prometheus handler: what matters is whether it is reached.
func exposition() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# HELP fleetward_alert_evaluation_duration_seconds\n"))
	})
}

func TestScrapeWithNoCredentialIsRefused(t *testing.T) {
	ts := newTestServer(t, exposition(), fixedDecision{authz.ErrScrapeNoCredential})

	if got := get(t, ts.URL+"/metrics"); got != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 — a scrape with no credential must be refused", got)
	}
}

func TestScrapeByAScopedViewerIsRefused(t *testing.T) {
	// Somebody granted viewer on one instance. The endpoint carries a series per instance across
	// the whole estate, so answering would hand them fifty servers' worth of shape.
	ts := newTestServer(t, exposition(), fixedDecision{authz.ErrScrapeForbidden})

	if got := get(t, ts.URL+"/metrics"); got != http.StatusForbidden {
		t.Fatalf("got %d, want 403 — a scoped viewer may not ask about the whole estate", got)
	}
}

func TestScrapeByATenantWideViewerIsServed(t *testing.T) {
	ts := newTestServer(t, exposition(), fixedDecision{nil})

	if got := get(t, ts.URL+"/metrics"); got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
}

// TestScrapeIsNotRegisteredWhenDisabled is the difference between two answers a monitoring system
// should be able to tell apart: there is no endpoint here, and there is an endpoint that knows
// nothing. A nil handler must produce the first.
func TestScrapeIsNotRegisteredWhenDisabled(t *testing.T) {
	ts := newTestServer(t, nil, fixedDecision{nil})

	if got := get(t, ts.URL+"/metrics"); got != http.StatusNotFound {
		t.Fatalf("got %d, want 404 — a disabled endpoint must not answer an empty 200", got)
	}
}

// TestScrapeWithNoAuthorizerIsOpen covers the configuration ADR-0042 permits and warns about on
// every start: an installation whose listener is already reachable only from the monitoring network.
func TestScrapeWithNoAuthorizerIsOpen(t *testing.T) {
	ts := newTestServer(t, exposition(), nil)

	if got := get(t, ts.URL+"/metrics"); got != http.StatusOK {
		t.Fatalf("got %d, want 200 — a nil authorizer serves the endpoint to anyone", got)
	}
}

// TestARefusedScrapeRendersTheProblemShape keeps the endpoint's errors the same shape as every
// other error the API produces, which is what lets a client handle them generically.
func TestARefusedScrapeRendersTheProblemShape(t *testing.T) {
	ts := newTestServer(t, exposition(), fixedDecision{authz.ErrScrapeForbidden})

	resp, err := http.Get(ts.URL + "/metrics") //nolint:noctx // a local httptest server
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("got Content-Type %q, want application/problem+json", got)
	}
	body, _ := io.ReadAll(resp.Body)
	// The refusal explains the rule and names nothing about who was asking.
	if len(body) == 0 {
		t.Fatal("a refusal carried no problem document")
	}
}

// Compile-time assurance that the real guard satisfies the interface the server takes. Without it,
// a change to either side would be caught only by cmd/fleetward failing to build.
var _ ScrapeAuthorizer = (*authz.Guard)(nil)

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // a local httptest server
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
