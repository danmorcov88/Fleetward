package scheduler

import (
	"context"

	"github.com/danmorcov88/fleetward/internal/controlplane/backup"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
)

// JobRunner adapts the services that do the work to Runner.
//
// It exists so that the scheduler depends on a small interface rather than on those services
// directly: claiming, heartbeating, losing a lease and reaping are all testable without an object
// store, a plugin process, or a container runtime being present. The adapter itself is the only
// place those packages meet, and it holds no logic — if a line of policy ever appears here, it
// belongs on one side or the other.
//
// It was called BackupRunner until it ran a second kind of job that is not a backup, and now a
// third. The name follows the work rather than the first thing the work happened to be.
type JobRunner struct {
	backups   *backup.Service
	inventory *inventory.Service
}

// NewJobRunner wraps the services the scheduler drives.
func NewJobRunner(backups *backup.Service, inv *inventory.Service) *JobRunner {
	return &JobRunner{backups: backups, inventory: inv}
}

var _ Runner = (*JobRunner)(nil)

// RunBackupJob runs a scheduled backup to completion.
func (r *JobRunner) RunBackupJob(ctx context.Context, in BackupJob) (string, error) {
	return r.backups.RunBackupSync(ctx, backup.RunBackupInput{
		InstanceID: in.InstanceID,
		MethodID:   in.MethodID,
		Options:    in.Options,
		JobID:      in.JobID,
		ScheduleID: in.ScheduleID,
		// The whole point of this slice: nobody asked for this one.
		TriggeredManually: false,
	})
}

// RunVerificationJob runs a scheduled verification to completion.
func (r *JobRunner) RunVerificationJob(ctx context.Context, in VerificationJob) error {
	return r.backups.RunVerificationSync(ctx, in.JobID, backup.RunVerificationInput{BackupID: in.BackupID})
}
