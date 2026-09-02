package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// robfig/cron is used here as an expression parser and nothing else: ParseStandard gives us a
// Schedule, and Schedule.Next answers "when does this fire after that instant". Its own in-process
// runner is never started, because the clock this scheduler obeys is schedules.next_run_at in
// PostgreSQL (ADR-0013). An in-process timer neither survives a restart nor coordinates two
// replicas, and both of those are the point.

// parseCron validates a standard five-field cron expression.
func parseCron(expr string) (cron.Schedule, error) {
	if expr == "" {
		return nil, fmt.Errorf("%w: a cron expression is required", ErrInvalidArgument)
	}
	spec, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a valid cron expression: %w", ErrInvalidArgument, expr, err)
	}
	return spec, nil
}

// loadLocation resolves a schedule's IANA timezone.
//
// The time zone database is embedded in the binary — see the time/tzdata import in the control
// plane's main — because the runtime container does not ship one, and a schedule that works on a
// developer's machine and fails in production is the worst possible way to find that out.
func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q: %w", ErrInvalidArgument, name, err)
	}
	return loc, nil
}

// nextRun computes when a schedule fires next, after `after`.
//
// The computation happens in the schedule's own location and the result is returned in UTC. That
// ordering is the whole point: a DBA writing "0 2 * * *" for a Bucharest server means 02:00 local,
// which is 00:00 UTC in winter and 23:00 UTC the previous day in summer. Computing in UTC instead
// would walk the backup window an hour into and out of the business day twice a year.
//
// What happens across a daylight-saving transition is not invented here — it is whatever
// robfig/cron's Next does when it walks local time, and cron_test.go pins that behaviour against
// real transitions so that a dependency upgrade cannot change it quietly. Measured, not assumed:
// at the spring forward an hour that does not exist is skipped and the run happens the following
// day; at the autumn fall back an hour that occurs twice fires once, on the first occurrence,
// because next_run_at has already advanced past the repeat. Both are written up for operators in
// docs/ops/scheduling.md.
func nextRun(expr, timezone string, after time.Time) (time.Time, error) {
	spec, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	return spec.Next(after.In(loc)).UTC(), nil
}
