//go:build conformance

package conformance

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// A Fixture supplies the one thing the plugin contract deliberately cannot express: how to put
// rows into a database, and how to take some out again.
//
// Fleetward never writes to a monitored instance — that is a §1 non-goal and the reason
// ListPrincipals is read-only — so there is no RPC the shared suite could call to create a table.
// Yet a backup of an empty database proves nothing: the manifest has no entries, the comparison
// runs over zero objects, and "VERIFIED" would mean only that a container started. The engine's
// own client fills the gap, in the test and nowhere else.
//
// This is an extension point, not a leak. Adding an engine means registering a fixture beside the
// plugin; it never means changing an assertion in this suite. A plugin with no fixture still gets
// the whole Stage 0 suite, and its Stage 1 cases skip with a message saying what is missing.
type Fixture interface {
	// Seed populates a freshly provisioned instance with objects and rows. It must create enough
	// that a manifest has several entries with different counts: a single table of one row would
	// let a plugin pass the count comparison by accident.
	Seed(ctx context.Context, creds *fwv1.Credentials) error

	// RemoveRows deletes rows from exactly one object and reports which one, by the same name the
	// manifest uses for it, and how many rows went. It is how the suite produces a manifest that
	// no longer describes its source without ever touching the manifest itself.
	RemoveRows(ctx context.Context, creds *fwv1.Credentials) (objectName string, removed int64, err error)
}

// fixtures is keyed by the engine type a plugin declares in its capability matrix, which is also
// what its binary is named after.
var fixtures = map[string]Fixture{
	"postgresql": postgresFixture{},
}
