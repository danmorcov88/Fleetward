package scheduler

import "context"

// SweepRetention runs one estate-wide retention pass.
//
// The adapter carries no policy of its own: what a sweep may delete is configured once, on the
// backup service, so that the sweep and the preview an operator reads beforehand cannot disagree.
// What the scheduler holds is only the pacing — whether to sweep at all, and how often.
func (r *JobRunner) SweepRetention(ctx context.Context) (RetentionOutcome, error) {
	result, err := r.backups.SweepRetention(ctx)
	return RetentionOutcome{
		Expired:          result.Expired,
		ArtifactsDeleted: result.ArtifactsDeleted,
		BytesReclaimed:   result.BytesReclaimed,
		Unreachable:      result.Unreachable,
	}, err
}
