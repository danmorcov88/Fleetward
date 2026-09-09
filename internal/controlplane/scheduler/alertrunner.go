package scheduler

import "context"

// EvaluateAlerts runs one estate-wide alert evaluation pass.
//
// The adapter carries no policy of its own, exactly like the retention one beside it: what counts
// as an alert is `alert_rules`, which is data an operator edits, and how a condition becomes a row
// is the alerts service's business. What the scheduler holds is only the pacing — whether to
// evaluate at all, and how often.
//
// The alerts service is optional here. A control plane started with alerting disabled still runs
// every other kind of work, and a nil check is a smaller thing than a second Runner interface.
func (r *JobRunner) EvaluateAlerts(ctx context.Context) (AlertOutcome, error) {
	if r.alerts == nil {
		return AlertOutcome{}, nil
	}
	result, err := r.alerts.Evaluate(ctx)
	return AlertOutcome{
		RulesConsidered: result.RulesConsidered,
		Firing:          result.Firing,
		Opened:          result.Opened,
		Resolved:        result.Resolved,
	}, err
}
