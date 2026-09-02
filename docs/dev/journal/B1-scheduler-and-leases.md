# B1 — The scheduler and the job lease

- **Delivered:** 2026-09-02
- **Brief:** [B1-scheduler-and-leases.md](../slices/B1-scheduler-and-leases.md)

`internal/controlplane/scheduler` is the fifth package under `controlplane`, and the first thing in
the tree that does work nobody asked for. It polls `schedules` for what is due, turns each due
schedule into a `jobs` row, claims that row with one atomic statement, holds a lease while the work
runs, and closes the books on jobs whose runner disappeared. `ScheduleService` and the
`schedule`/`job` CLI groups are how a human declares the intent and reads back what happened.

Two other packages moved to make room, both narrowly. `backup.Service` gained `RunBackupSync` and
`RunVerificationSync` — the same work as the existing asynchronous entry points, but bound to the
caller's context and returning when the outcome has been recorded, because a runner that loses its
lease has to be able to stop what it started. `cmd/fleetward/main.go` gained the wiring, a
`scheduler` readiness component, and `import _ "time/tzdata"`.

## How it was verified

On Windows (amd64), 2026-09-02, against Go 1.25 and Docker 27.3.1.

`go test -short ./...` passes. The scheduler's own unit suite covers the lease identity, the
interval rendering, the sampling policy, the payload round trip, the readiness staleness window, and
the enum mapping in both directions.

`go test -tags=integration ./internal/controlplane/scheduler/...` passes in 29.8 seconds, against a
real `postgres:16-alpine` per test, with the real schema. Nine cases, and the ones worth naming:

- **`TestClaimIsRaceFreeUnderContention`** — sixteen goroutines claiming eight jobs across eight
  instances. Every job is claimed exactly once; no claimer receives a job twice.
- **`TestHeartbeatReportsALostLease`** — renewing a lease held by someone else, and renewing one the
  reaper has already closed, both return `errLeaseLost` rather than succeeding or erroring.
- **`TestRunnerAbandonsAJobWhoseLeaseWasTaken`** — the end-to-end version. A job is claimed, its
  work blocks, another process takes the lease and writes a verdict; the runner's own context is
  cancelled and the verdict it finds afterwards is still the other process's.
- **`TestReapClosesAbandonedWork`** — an expired lease fails the job *and* the `backups` row it
  orphaned, and the job is then **not** claimable again.
- **`TestMaterializeIsSafeWithTwoSchedulers`** — two schedulers ticking on one due schedule at the
  same instant produce one job, not two.

The daylight-saving behaviour is measured rather than assumed. `TestNextRunAcrossDaylightSaving`
pins four cases against Europe/Bucharest's real 2026 transitions, and the results are what
[`../../ops/scheduling.md`](../../ops/scheduling.md) now tells operators: at the spring forward an
hour that does not exist is skipped and the run lands the following day; at the fall back the
repeated hour fires once, on its first occurrence.

`go test -tags=conformance ./test/conformance/...` passes, and on this machine that means less than
it does in CI: every end-to-end case skips, because `pg_dump`, `pg_restore` and `psql` are not on
the Windows host's PATH and the suite refuses to pretend otherwise. Only the capability and health
subset ran locally. CI installs the PostgreSQL client and runs the whole thing, and the suite is
unchanged by this slice in any case — the scheduler is not part of the plugin contract.

`golangci-lint run`, `buf lint`, `buf format --diff --exit-code`, `buf breaking --against main`, and
`go run ./tools/docscheck` are all clean. They were checked in a worktree created with
`core.autocrlf=false`: a Windows checkout with the default `autocrlf=true` makes `gofmt` and `buf
format` report every file in the tree as unformatted, which is a line-ending artefact and not a
finding. That is now written into `STATUS.md` so the next session does not spend an hour on it.

One pre-existing failure, unrelated and reproduced on `main` at f0c604f before this branch was
touched: `plugins/postgres`'s `TestDiscoverOnUnreachableInstanceFails` dials 192.0.2.1 and expects
`CONNECTION_FAILED`, but this network answers with something pgx reads as
`AUTHENTICATION_FAILED`. It is an environment artefact of the host, not a regression.

And the walk the brief asked for, against `docker compose up --build` on the same machine: an
instance pointed at the stack's own PostgreSQL 16.14, then

```
fleetward-cli schedule create --instance prod-1 --cron "* * * * *" \
    --timezone Europe/Bucharest --verify always
  → next run 2026-09-02 10:31:00
```

10:31 UTC is 13:31 in Bucharest, which is the arithmetic the timezone column exists to do. Nobody
typed anything after that:

```
ID       KIND    STATE      TRIGGER   ATTEMPTS  STARTED (UTC)        FINISHED (UTC)
82bdf45  verify  succeeded  schedule  1         2026-09-02 10:37:13  2026-09-02 10:37:17
d3716b7  backup  succeeded  schedule  1         2026-09-02 10:37:03  2026-09-02 10:37:03
8b162e7  backup  succeeded  schedule  1         2026-09-02 10:36:02  2026-09-02 10:36:02
```

Each backup was a 68,770-byte `pg_dump` covering 19 objects and 13 records, taken in about 190 ms,
and each verification restored it into a `postgres:16` sandbox and destroyed the container.
`/readyz` reports a `scheduler` component alongside the rest. This is the only place on this machine
where the scheduler drives a *real* backup, which is why it was worth doing: the integration tests
use a stub runner, and the conformance suite's end-to-end cases skip here for want of `pg_dump`.

### And then the control plane was killed mid-verification

The half of this slice that has no screen, walked for real rather than only in a test.

At **10:38:16** the container was `SIGKILL`ed while a `verify` job held a lease due to expire at
10:40:13. It came back at 10:38:34, and three things happened, in this order:

1. **The startup sweep removed the sandbox the dead process had left**, one `postgres:16` container
   created at 10:38:13 — `removed verification sandboxes left behind by a previous process count=1`.
2. **Backups kept running on schedule**, at 10:39:04 and 10:40:04. The stuck job did not block the
   instance, because `idx_jobs_one_active_per_instance_kind` is per kind and what was stuck was a
   verification.
3. At **10:40:14**, one lease TTL after the dead process's last heartbeat, the reaper closed the
   books:

```
WARN  closed jobs whose runner stopped reporting  count=1

1afd5ef0  verify  failed  schedule  1  2026-09-02 10:38:13  2026-09-02 10:40:14
          the runner holding this job's lease stopped reporting; the job was closed rather
          than re-run, and the next scheduled run will proceed normally
```

The `verifications` row it orphaned became **`inconclusive`**, not `failed`. That is
[ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md) holding under a real
crash rather than under a test: a control plane that was killed is evidence about the control plane,
and `FAILED` is the one alert this product cannot afford to cry wolf on.

`attempts` reads `1`. The job was started once, and the row says so.

## Two things the compose walk found that the tests did not

Both were invisible to a test suite and obvious within a minute of watching real output.

**Every scheduler line printed `job_id` twice.** The identifier was attached to the context — which
`internal/telemetry` promotes onto every record — *and* named again on the logger. The brief said to
use one mechanism and the implementation used both. Now only the context carries it, which also
means the backup service's own lines inherit it without knowing the scheduler exists.

**`attempts` counted every run twice.** `jobs.attempts` means "how many times has this been
started", and the closing `UPDATE`s in `recordFailure` and `recordVerification` incremented it on
the way *out*. That was correct when a job could only be created already-running, because then the
close was the only counter. With a scheduler the claim counts the start too, so a verification that
ran once reported `2` — which on `job list` reads as a retry that never happened, in a column an
operator would act on. The count now belongs to whichever event actually starts the job: the claim
for scheduled work, the insert for work a human triggered directly. Two integration assertions pin
it.

## Decisions worth carrying forward

**An expired lease fails its job; it is never claimed twice.** This reverses a clause of
[ADR-0013](../../adr/0013-internal-scheduler-with-leases.md) and is recorded as
[ADR-0025](../../adr/0025-an-expired-lease-fails-its-job.md), because a future session would
otherwise "fix" it back. The clause was written before there was anything to schedule. Once there
was, the case it describes turned out to be narrow and expensive: a control plane killed mid-backup
leaves no artifact — the multipart upload was aborted, so there is nothing to resume — and
re-running means an unattended `pg_dump` starting against a production server at the moment that
server has just come back. The reaper closes the job as failed instead, with a message an operator
can act on, and the schedule's next occurrence proceeds normally. A crash costs one run.

It also buys a stronger guarantee than the original design: because a job only ever moves `pending
→ running`, no job is executed twice by any path, and the lease's cancellation is left to do the
thing it is actually good at — stopping orphaned work.

**Zero rows affected is a result, not an error.** The heartbeat ends `WHERE id = $1 AND lease_owner
= $2 AND state = 'running'`. When that updates nothing, the lease is gone. Treating it as a database
failure — the obvious reading — would leave a ghost runner holding a connection to a production
database while another process writes the job's outcome. It returns a sentinel, and the runner
cancels its own context.

**One statement, not a transaction.** The claim is a single `UPDATE ... WHERE id = (SELECT ... FOR
UPDATE SKIP LOCKED) RETURNING ...`. The subselect is not a second statement — PostgreSQL's `UPDATE`
has no `LIMIT`, so choosing the row has to happen inside the `WHERE` clause. What it buys is that
there is no window in which a job is claimed in the database but unrecorded in the process that
claimed it.

**The clock is a column.** `schedules.next_run_at`, advanced by a compare-and-swap on the value that
was read. Two replicas ticking on the same due schedule: one wins the swap and inserts the job, the
other updates nothing and moves on. No lock, no transaction, no leader election. The order —
advance first, then insert — is deliberate: dying between the two loses a tick, which is a missed
backup and visible as one, while the other order risks a duplicate concurrent dump.

**Verification is a row, not a callback.** A successful scheduled backup inserts a `pending` job of
kind `verify` rather than chaining in-process. Two things follow. `verify_policy` and
`verify_sample_percent` become answerable after the fact — "why was this backup never verified" is a
query rather than a reading of this package's source. And verifications compete for
`SCHEDULER_MAX_CONCURRENT_JOBS` along with everything else, which is what finally bounds the case
`STATUS.md` had been carrying since A3: fifty instances verifying at 02:00, each holding a container
and a spooled artifact. The manual path still chains, because a human who asked for both wants both.

**A job carries a snapshot of the schedule that made it.** Method, options, and verification policy
are copied into `jobs.payload` at creation, and the runner never re-reads the schedule. A schedule
edited at 02:15 must not change what the 02:00 run was asked to do.

**`23505` from the partial unique index means "skip", and it means something operationally.** A
second pending backup for one instance is refused by `idx_jobs_one_active_per_instance_kind` rather
than inserted. The scheduler logs it at warn and continues, because the line is worth reading: the
backup is taking longer than the interval it was scheduled at.

**The time-zone database is compiled into the binary.** `debian:trixie-slim`, the runtime stage of
the control-plane image, ships no `tzdata`, while a Go *installation* carries
`$GOROOT/lib/time/zoneinfo.zip`. Without `import _ "time/tzdata"` every schedule with a real
timezone would work on a developer's machine and fail in the container — the worst shape a bug can
have. Installing the Debian package instead would have fixed this image and quietly broken the next.

**Shutdown order is load-bearing, and now says so in a comment.** `main.go` builds the scheduler
after the backup service, so its `defer` runs first: the scheduler stops claiming and drains, then
the backup service drains. Reversed, the backup service would wait on runs the scheduler was still
starting. `Close` cancels in-flight work rather than waiting for it — a backup takes hours — but
does not return until every runner has unwound, so a cancelled run still records that it was
cancelled. Writing the integration test for this is what forced the distinction to be stated
precisely; the first version of the test asserted draining and was wrong about what shutdown should
promise.

## Not built, deliberately

- **Retries and backoff.** `jobs.max_attempts` is untouched. A failed job waits for its schedule's
  next occurrence. Retry deserves a policy with a window, not a counter that happens to exist.
- **`discovery` and `metrics` schedules.** `schedules.kind` permits both; creating one is refused
  with a message naming the slice that will bring them. **B4.**
- **Retention.** `retention_days` is stored on every schedule and read by nothing, and the scheduler
  now fills the bucket faster than before. **B5.**
- **Authorization.** Every new route is open, exactly like every existing one. **B6.**
- **Alerting on a missed window or a schedule that stopped firing.** The scheduler makes both
  detectable and delivers neither. **B7.**
- **Scheduler metrics and spans.** Log lines and one readiness component. **B8.**
- **A UI.** Schedules and jobs are CLI and REST only. **B4.**
- **Multi-replica deployment.** The lease protocol is built for it and two of the integration tests
  prove contention behaves, but shipping a replicated control plane needs a deployment artifact that
  does not exist. **B9.**

## Still open

- A **manually triggered** verification is not bounded by `SCHEDULER_MAX_CONCURRENT_JOBS`. The
  scheduled case — the one that matters on an estate of fifty — is.
- The scheduler's only visibility is `fleetward-cli job list` and the log. A schedule that stopped
  firing because its expression no longer parses is logged every tick and reaches nobody.
- `/readyz` degrades if the tick loop stalls, but nothing acts on that yet.
