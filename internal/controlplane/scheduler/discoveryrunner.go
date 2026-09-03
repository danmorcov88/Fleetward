package scheduler

import (
	"context"
	"fmt"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// RunDiscoveryJob probes one instance and records what it found.
//
// This is the first of the three components of the estate view's promise that the answer is never
// more than about thirty seconds stale. Without it, `instances.health` is whatever the last human
// who ran `test-connection` left behind — which on an estate of fifty means a screen showing a
// healthy server that died three weeks ago. The alternative, a browser triggering fifty live
// probes on every refresh, is fifty connections to production every thirty seconds from a page
// anyone can open.
//
// The distinction that decides the job's outcome is worth stating, because it is easy to get
// backwards. An instance that is **down** is a successful probe: the plugin answered, the answer
// was DOWN, and inventory recorded it. What fails this job is not being able to ask at all — no
// plugin for the engine, or the plugin process itself unreachable — because that is Fleetward's
// problem rather than the estate's, and a job that quietly succeeds while learning nothing would
// let the screen keep showing a stale answer with a fresh timestamp on it.
func (r *JobRunner) RunDiscoveryJob(ctx context.Context, in DiscoveryJob) error {
	if _, err := r.inventory.TestConnection(ctx, &fwv1.TestConnectionRequest{
		InstanceId: in.InstanceID,
	}); err != nil {
		return fmt.Errorf("scheduler: probe instance %s: %w", in.InstanceID, err)
	}
	return nil
}
