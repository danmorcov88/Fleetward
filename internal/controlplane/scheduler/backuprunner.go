package scheduler

import (
	"context"

	"github.com/danmorcov88/fleetward/internal/controlplane/backup"
)

// BackupRunner adapts the backup service to Runner.
//
// It exists so that the scheduler depends on a two-method interface rather than on the backup
// service directly: claiming, heartbeating, losing a lease and reaping are all testable without an
// object store, a plugin process, or a container runtime being present. The adapter itself is the
// only place the two packages meet, and it holds no logic — if a line of policy ever appears here,
// it belongs on one side or the other.
type BackupRunner struct {
	svc *backup.Service
}

// NewBackupRunner wraps the backup service.
func NewBackupRunner(svc *backup.Service) *BackupRunner { return &BackupRunner{svc: svc} }

var _ Runner = (*BackupRunner)(nil)

// RunBackupJob runs a scheduled backup to completion.
func (r *BackupRunner) RunBackupJob(ctx context.Context, in BackupJob) (string, error) {
	return r.svc.RunBackupSync(ctx, backup.RunBackupInput{
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
func (r *BackupRunner) RunVerificationJob(ctx context.Context, in VerificationJob) error {
	return r.svc.RunVerificationSync(ctx, in.JobID, backup.RunVerificationInput{BackupID: in.BackupID})
}
