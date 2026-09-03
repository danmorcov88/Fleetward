package scheduler

import (
	"context"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

// testTenantCtx is the context every service call in this package's tests runs on.
//
// Since B6 the tenant comes from the principal on the context rather than from a constant, so a
// bare context.Background() reaches Postgres with an empty tenant and is rejected as an invalid
// UUID. That is the intended failure — loud, rather than silently reading the default tenant — and
// this is what a test uses instead.
//
// It carries no build tag on purpose: the unit tests and the integration tests both need it, and a
// helper that existed only under `-tags=integration` is how the two suites drift apart.
func testTenantCtx() context.Context {
	return authn.WithPrincipal(context.Background(),
		authn.System("test", metadb.DefaultTenantID))
}
