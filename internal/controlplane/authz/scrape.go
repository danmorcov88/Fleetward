package authz

import (
	"context"
	"errors"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
)

// Scraping /metrics, which is the one authorized thing in this product that is not an RPC.
//
// It does not go through Policies, and that is deliberate rather than an oversight. The policy
// table is keyed on generated gRPC method names and the coverage test asserts in *reverse* that
// every entry names a method some generated service interface actually has — a fabricated
// "/fleetward.v1.Metrics/Scrape" row would break that check, which is exactly the check that keeps
// the table honest. So the decision lives here, built from the same primitives Check uses, and the
// rule it applies is one that already exists rather than a new one.
//
// **A scrape names no scope, and a request that names no scope is a question about the whole
// tenant** (ADR-0035). The response carries a series per instance — how many servers there are,
// which engines they run, which of them fail verification — so a caller granted three servers
// cannot be handed an answer about fifty. Tenant-wide `viewer` is therefore the floor, and it falls
// out of an existing decision rather than being invented for this endpoint (ADR-0042).

var (
	// ErrScrapeNoCredential reports that nobody presented one. The endpoint answers 401.
	ErrScrapeNoCredential = errors.New("scraping metrics requires a credential")
	// ErrScrapeForbidden reports a caller who is somebody, but not somebody who may ask about the
	// whole estate. The endpoint answers 403.
	ErrScrapeForbidden = errors.New("scraping metrics requires a tenant-wide viewer grant")
)

// AuthorizeScrape decides whether the caller on ctx may read /metrics.
//
// The order of the two checks below is load-bearing and is the same order Check uses. A system or
// bootstrap caller is allowed **by kind**, before any grant is examined, because neither holds a
// grant at all: authn.System constructs a principal in-process with no rows behind it, and
// BootstrapAuthenticator returns one with a nil Grants slice. A check written as
// `bestRank(p.Grants, tenantWide) >= viewer` would therefore refuse the break-glass credential —
// which is the credential the development stack's own scrape config presents, so the failure would
// show up as a metrics store that silently collects nothing.
func (g *Guard) AuthorizeScrape(ctx context.Context) error {
	p, ok := authn.From(ctx)
	if !ok || !p.Authenticated() {
		return ErrScrapeNoCredential
	}

	if p.Kind == authn.KindSystem || p.Kind == authn.KindBootstrap {
		return nil
	}

	required, ok := g.ranks[RoleViewer]
	if !ok {
		// The ranks come from the `roles` table and NewGuard fails without them, so this cannot
		// happen in a control plane that started. Refusing rather than allowing is the only safe
		// reading of "I do not know what viewer means".
		return ErrScrapeForbidden
	}

	if bestRank(p.Grants, func(gr authn.Grant) bool { return gr.TenantWide() }) >= required {
		return nil
	}
	return ErrScrapeForbidden
}
