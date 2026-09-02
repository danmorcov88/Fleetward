# Scheduling

How Fleetward decides that a backup should run now, how it makes sure exactly one control plane runs
it, and what it does when the process running it disappears.

Every setting named here is documented in [`configuration.md`](configuration.md). The decision
behind the design is [ADR-0013](../adr/0013-internal-scheduler-with-leases.md), and the decision
about what an expired lease means is [ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md).

---

## Schedules and jobs are different things

A **schedule** is recurring intent: *back this instance up every night at two, in Bucharest time,
and prove the backup restorable afterwards*. It does not run.

A **job** is one attempt at that intent, with a row of its own. It records who leased it, when it
started, whether it succeeded, and what went wrong if it did not.

The separation is what makes the system answerable after the fact. A schedule tells you what should
have happened; the jobs tell you what did.

```bash
fleetward-cli schedule create --instance prod-1 --cron "0 2 * * *" \
    --timezone Europe/Bucharest --verify always
fleetward-cli schedule list
fleetward-cli job list --instance prod-1
```

## The clock is a column, not a timer

Each schedule carries `next_run_at`, stored in UTC. The control plane polls every
`FLEETWARD_SCHEDULER_POLL_INTERVAL` (10 seconds by default) and asks the database what is due.

There is no in-process timer holding "the next backup is at 02:00", and that is deliberate: a timer
does not survive a restart, and two control planes would each hold their own copy and both fire.

What this means in practice:

- The control plane restarting at 01:59 does not lose the 02:00 backup.
- A control plane that is down from 01:55 to 02:05 runs the backup at 02:05 — **late, and visible as
  late**, rather than silently skipped.
- A control plane that is down all night misses that night's run. The next occurrence runs normally.
  Nothing catches up on a backlog; one late backup is useful, six at once against one server is not.

## Timezones, and what daylight saving actually does

A cron expression is read in the schedule's own timezone, and the resulting instant is stored in
UTC. `0 2 * * *` with `--timezone Europe/Bucharest` means 02:00 in Bucharest all year — 00:00 UTC in
winter and 23:00 UTC the previous day in summer.

Computing in UTC instead would walk the backup window an hour into and out of the business day twice
a year, which is exactly the surprise a maintenance window exists to avoid.

Daylight-saving transitions are not tidy, so here is what Fleetward does rather than a claim that
nothing happens. Both behaviours are pinned by tests against real transitions.

| Situation | What happens |
|---|---|
| An hour that **does not exist** on the spring-forward day — 03:30 in a zone where 03:00 becomes 04:00 | That day's run is **skipped**. The next run is at 03:30 the following day. |
| An hour that **occurs twice** on the fall-back day — 03:30 where 04:00 becomes 03:00 | The run happens **once**, on the first occurrence. The repeat does not fire a second one, because `next_run_at` has already advanced past it. |
| Any hour outside the transition | Unaffected. |

If a schedule must never be skipped, put it at an hour your zone does not move — anything outside
roughly 01:00–04:00 local is safe in every zone Fleetward has been tested against. Or use `UTC`,
which has no transitions at all.

> The time-zone database is compiled into the `fleetward` binary rather than read from the operating
> system, so `Europe/Bucharest` resolves identically on a developer's laptop and inside the
> container image.

## One run at a time, per instance

Two simultaneous dumps of one production server is an operational incident, not a race to lose, so
it is prevented by a database constraint rather than by careful code: an instance may have at most
one active job of a given kind.

When a schedule falls due and the previous run of that same schedule has not finished, the tick is
**skipped**, and the control plane logs:

```
WARN  skipped a scheduled run because the previous one is still active
```

That line means something real and worth acting on: *the backup is taking longer than the interval
it was scheduled at.* Either the schedule is too frequent or the backup has got slower.

## Leases: which control plane runs the job

A job is claimed by one atomic `UPDATE`. The winner writes its identity into `lease_owner` —
`<hostname>/<pid>/<a random per-process suffix>` — and a `lease_expires_at`
`FLEETWARD_SCHEDULER_LEASE_TTL` in the future. Everyone else sees nothing claimable and moves on.

While the job runs, its runner renews the lease every
`FLEETWARD_SCHEDULER_LEASE_HEARTBEAT`. The heartbeat must be shorter than the TTL, and the control
plane refuses to start if it is not: a lease that expires before it is renewed would be a lease that
means nothing.

The TTL does **not** need to exceed the length of a backup. Long jobs renew; they do not hold.

`FLEETWARD_SCHEDULER_MAX_CONCURRENT_JOBS` bounds how many jobs one control plane runs at once. It is
the knob that matters on a large estate: each verification holds a sandbox container and a spooled
copy of the artifact, so fifty instances all verifying at 02:00 is a resource problem rather than a
busy night.

> This bound covers scheduled work. A human triggering many verifications by hand through the API is
> not currently bounded by it.

## When a control plane is killed mid-backup

The lease expires. On its next tick — any control plane's next tick, not necessarily the one that
died — a reaper closes the books:

- the **job** becomes `failed`, with a message saying the runner holding its lease stopped
  reporting, that the job was closed rather than re-run, and that the next scheduled run will
  proceed normally;
- the **backup** row it orphaned becomes `failed` too, because the artifact upload was aborted and
  there is nothing behind that row;
- an interrupted **verification** becomes `inconclusive`, never `failed`. A control plane that was
  killed is evidence about the control plane, not about the artifact
  ([ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md)).

**The job is not re-run.** Restarting a six-hour dump against a production server, unattended, at
the moment that server has just come back, is worse than the missed run — and the interrupted run
left nothing to salvage. The reasoning is recorded in
[ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md).

So a crash costs one run, and that run is visible as failed with a reason. It does not leave a row
reading `running` forever.

## Verification is its own job

When a scheduled backup succeeds, the verification is queued as a **separate job** rather than
chained onto the backup:

| `--verify` | What happens after a successful backup |
|---|---|
| `always` (default) | A verify job is queued every time. |
| `sampled` with `--verify-percent N` | A verify job is queued for roughly N% of runs, chosen at random per run. |
| `manual` | Nothing is queued. Verify by hand with `fleetward-cli backup verify`. |

Two consequences worth knowing. The policy is visible in the job table, so *"why was this backup
never verified"* is answered by looking rather than by guessing. And verifications compete for the
same concurrency budget as everything else, which is what keeps a nightly wave of them bounded.

## Pausing without losing

```bash
fleetward-cli schedule disable <schedule-id>   # during a migration window
fleetward-cli schedule enable  <schedule-id>
```

A disabled schedule has no next run and creates no jobs; runs already in flight finish normally.
Enabling recomputes the next run **from now**, so resuming a schedule that was paused for a week
does not immediately fire the runs it missed.

## Turning the scheduler off entirely

`FLEETWARD_SCHEDULER_ENABLED=false` starts a control plane that serves the API and runs nothing
automatically. It is a legitimate configuration — a second replica dedicated to serving traffic, or
an operator who wants the estate view before trusting the automation — and it is not reported as
unhealthy.

When the scheduler *is* enabled, `/readyz` carries a `scheduler` component that degrades if the tick
loop stops advancing. It is non-critical: a stalled scheduler must not take the estate view offline,
but it must not be silent either, because a loop that has quietly stopped looks exactly like an
estate with nothing scheduled.
