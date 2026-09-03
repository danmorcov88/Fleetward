# ADR-0028: Observation is a schedule kind, and an expectation is declared rather than inferred

- **Status:** Accepted
- **Date:** 2026-09-02
- **Slice:** B3 — observed backups
- **Relates to:** [ADR-0013](0013-internal-scheduler-with-leases.md),
  [ADR-0015](0015-observed-and-managed-backups.md),
  [ADR-0027](0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md)

## Context

The product thesis is three words long: **declare, detect, show the gap.** Slice B3 builds the
detection half for backups Fleetward did not take. Two questions come with it, and they are the same
question asked from two directions.

**How does detection happen without anyone asking?** Reading an engine's backup history is recurring
work. Until this slice, `internal/controlplane/scheduler/service.go` refused every schedule kind but
`backup`, with a message naming the slice that would bring the others.

**Where does the declaration live?** Detection alone answers "here is what I found". The question a
DBA with fifty servers actually has is "which ones are behind", and nothing about the evidence can
answer that: a backup that stopped running six months ago leaves no trace at all, and an absence is
only a finding once somebody has said what should have been there.

There is a tempting alternative to declaring it, and it is worth naming because it is the wrong
shape rather than merely worse. Fleetward could infer the expectation from the observed rhythm —
"this instance has been backed up at 02:00 every night for a year, and last night it was not". That
answers *is this normal for you*. The question the product exists for is *is this what you asked
for*, and an estate that has silently been failing to back up one server since March would have that
failure normalised into its own baseline.

## Decision

**Observation runs as a schedule kind, and the expectation is declared on that schedule.**

`observe` joins `backup` as a kind the scheduler runs. It is materialized, leased, heartbeated and
released by exactly the machinery [ADR-0013](0013-internal-scheduler-with-leases.md) built: one more
`Runner` method and two widened `CHECK` constraints, and everything at-most-once about it comes for
free from being a job kind rather than a second timer.

The declaration is two columns on `schedules`:

```sql
ALTER TABLE schedules
    ADD COLUMN expected_cron TEXT NOT NULL DEFAULT '',
    ADD COLUMN expected_grace_minutes INTEGER NOT NULL DEFAULT 0 CHECK (expected_grace_minutes >= 0);
```

Two cron expressions on one row, answering different questions, and the row says so:

- `cron_expression` — how often Fleetward goes and looks.
- `expected_cron` — when a backup is supposed to have happened, plus how late is still acceptable.

They are genuinely independent. Polling every thirty minutes to check a nightly backup is a
reasonable thing to want, and so is polling once a day. Deriving one from the other would report
"we polled and saw nothing" as though it were "your backup did not run".

**Adherence is computed on read.** No verdict is stored anywhere. The endpoint walks the instances
carrying an expectation, computes the most recent occurrence whose grace period has already run out,
and asks whether a backup of either origin — managed or observed — satisfied it.

Three details of that computation are load-bearing:

- **The occurrence under judgement is the last one whose grace has expired**, not the last one that
  has passed. An instance expected to back up at 02:00 with two hours of grace must not be reported
  as behind at 02:30 while the backup is still running.
- **A window is satisfied by a backup of either origin.** This is the whole point of the slice: a
  nightly dump somebody's cron job has been writing since 2019 satisfies the window exactly as a
  backup Fleetward took does.
- **A window containing only evidence that cannot report an outcome is `UNPROVEN`, never
  `ADHERENT`.** A file that arrived is not a backup that worked, and the two must never render as the
  same green tick ([ADR-0015](0015-observed-and-managed-backups.md)).

An instance with nothing declared reports `NOT_DECLARED`. It is neither a problem nor an
achievement, and it is emphatically not omitted: on an estate of fifty servers, "nobody has said
what this one's backups should look like" is a finding.

## Consequences

**The scheduler runs two kinds of work with different risk profiles, and only one of them touches a
production database in a way anybody would notice.** A backup job holds a connection for hours and
writes an artifact; an observation job runs two `SELECT`s or lists a directory and returns. They
share the concurrency budget and the lease machinery, which is right — an observation that hangs
should lose its lease like anything else — and the difference is worth remembering when
`SCHEDULER_MAX_CONCURRENT_JOBS` is tuned.

**Nothing is alerted.** Adherence is served through the API and the CLI and delivered nowhere; a
missed window is visible to whoever looks. That is B7, and the computation this ADR describes is
what it will read, rather than a table it would have to trust.

**Two cron expressions on one row is a real cost.** It wants a clear comment in the schema, a clear
flag name at the CLI (`--cron` and `--expect-cron`), and it will want a clear label in the UI. The
alternative — a second table — buys nothing until there is a second consumer.

**A grace of zero is not taken literally.** It would demand a backup complete in the same instant it
was due, so every instance on the estate would report as missing one. A declaration that names a
schedule and no tolerance gets two hours, which is what a nightly window needs: it absorbs a run
that started on time and took longer than usual, and it absorbs the one hour an engine that records
local time without an offset can be wrong by across a daylight-saving change
([ADR-0027](0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md)).

**One expectation per instance.** More than one enabled schedule carrying an expectation is a
configuration nobody meant, and the newest wins. Per-database expectations are a real requirement
for a large estate and they are not this; they need the separate table this decision declined to
build yet.

## Alternatives considered

**Observation on demand only**, with the recurring part waiting for the estate view. Cheapest by a
wide margin, and it fails the slice's own headline: "did every server's backup run" cannot require a
human to type a command per server, on the estate of fifty this product exists for. It is kept
anyway, as `fleetward-cli backup observe`, because being able to ask now is what makes the first
five minutes with Fleetward work.

**Reuse the `backup` schedule kind, with a flag.** One row, one cadence, no migration. Rejected
because the two mean opposite things: a `backup` schedule says Fleetward takes the backup, and an
`observe` schedule says something else does and Fleetward watches. Conflating them puts the most
important distinction in the product behind a boolean.

**Infer the expectation from the observed rhythm.** Discussed above. It answers a different question,
and it launders a long-running failure into a baseline.

**Store an adherence verdict per instance, refreshed by the scheduler.** It would make the estate
view a single indexed read, which matters at a thousand instances and does not at fifty. The cost is
a cache with no invalidation story: it is stale after every schedule edit, wrong after a poll that
half succeeded, and needs a backfill whenever an expectation changes. Computing it costs two queries
for the whole estate, and a number that is always right is worth more than a number that is
occasionally faster.
