package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danmorcov88/fleetward/internal/controlplane/authz"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// ScrapeAuthorizer decides who may read /metrics.
//
// An interface rather than *authz.Guard so that this package's tests can exercise the refusal
// without a database behind them, and so that the one place the decision is made stays in the
// authorization package where every other decision lives.
type ScrapeAuthorizer interface {
	AuthorizeScrape(ctx context.Context) error
}

// RegisterMetrics mounts the Prometheus endpoint.
//
// A nil handler registers nothing, so a control plane with the endpoint disabled answers 404 rather
// than an empty 200 — "there is no endpoint here" and "there is an endpoint and it knows nothing"
// are different answers and a monitoring system should be told which one it got.
//
// A nil authorizer serves it to anyone who can reach the port. That is a supported configuration,
// for an installation where the listener is already reachable only from the monitoring network, and
// the control plane warns about it on every start (ADR-0042). It is not the default.
func (s *Server) RegisterMetrics(handler http.Handler, auth ScrapeAuthorizer) {
	if handler == nil {
		return
	}
	s.mux.Handle("GET /metrics", s.guardScrape(handler, auth))
}

// guardScrape refuses a scrape that may not have the estate's shape.
func (s *Server) guardScrape(next http.Handler, auth ScrapeAuthorizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth == nil {
			next.ServeHTTP(w, r)
			return
		}

		err := auth.AuthorizeScrape(r.Context())
		switch {
		case err == nil:
			next.ServeHTTP(w, r)

		case errors.Is(err, authz.ErrScrapeNoCredential):
			// Deliberately not audited, for the same reason an unauthenticated RPC is not: the row
			// would name no principal, and it is exactly the row an attacker can generate a million
			// of (ADR-0035). The access log records it.
			writeProblem(w, http.StatusUnauthorized, "Unauthorized",
				"Scraping metrics requires a credential: send Authorization: Bearer <token>.",
				telemetry.RequestIDFrom(r.Context()))

		default:
			s.log.WarnContext(r.Context(), "refused a metrics scrape",
				slog.String("remote_addr", clientIP(r)))
			writeProblem(w, http.StatusForbidden, "Forbidden",
				"Scraping metrics is a question about the whole estate and requires a "+
					"tenant-wide viewer grant.",
				telemetry.RequestIDFrom(r.Context()))
		}
	})
}
