package scheduler

import (
	"context"

	"github.com/danmorcov88/fleetward/internal/controlplane/backup"
)

// RunObservationJob reads one instance's backup history on a schedule.
//
// It is the same adapter shape as the others, and it stays that shape deliberately: observation is
// bounded, read-only work that has to happen without anyone asking, which is exactly what the
// lease machinery already does. Everything at-most-once about it — the claim, the heartbeat, the
// lease that can cancel the work — comes for free from being a job kind rather than a second timer.
func (r *JobRunner) RunObservationJob(ctx context.Context, in ObservationJob) error {
	_, err := r.backups.ObserveBackupHistory(ctx, backup.ObserveInput{InstanceID: in.InstanceID})
	return err
}
