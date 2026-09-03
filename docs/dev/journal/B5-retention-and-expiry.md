# B5 — Retention and expiry, or the first slice that can destroy data

- **Delivered:** 2026-09-03
- **Brief:** [B5-retention-and-expiry.md](../slices/B5-retention-and-expiry.md)

Fleetward has never deleted anything. Every artifact it has ever written was still in the bucket,
and B1's scheduler was filling that bucket faster than anyone had been filling it by hand — while
`schedules.retention_days` was stored on every schedule ever created and read by nothing.

**The deliverable is that a managed backup which has outlived the retention its schedule declared
loses its artifact, and that nothing else ever does.** The second half of that sentence is where
almost all of the work went.

Everything before this slice was read-only or additive. From here on a bug does not report the wrong
thing; it deletes a backup. The design decisions below are all downstream of that one fact.

## How it was verified

On Windows (amd64), 2026-09-03, against Go 1.25.6, Node 22.16, buf 1.58.0, golangci-lint 2.12.2,
Docker 27.3.1, `postgres:16-alpine` and `minio/minio:RELEASE.2025-04-22T22-12-26Z`.

```
go build ./...                     ok
go vet ./...                       ok
go vet -tags=integration ./...     ok
go vet -tags=conformance ./...     ok
go test ./...                      ok, no failures
go test -tags=integration ./...    ok, bar the two known machine failures
go test -tags=conformance ./test/conformance/...   ok, 164.4s
go run ./tools/docscheck           73 markdown files, no problems
go mod tidy                        no drift
golangci-lint run                  0 issues   (LF worktree)
buf lint / format / breaking       clean — the RPC is additive
npm run lint / build               clean
```

`make` is not installed on this machine, so those are the targets run directly rather than
`make lint test` reported as passing.

The two integration failures are `sandbox.TestSandboxLifecycle` and `plugins/postgres`'s
`TestDiscoverOnUnreachableInstanceFails`, both in `STATUS.md`'s environment notes and both
reproduced on `main` before B1.

**`go vet -tags=integration` earned its place again.** A fifth `Runner` method broke `stubRunner` in
`scheduler/integration_test.go` — a file this slice had no reason to open, in a package it did. B4
caught the same shape in eight seconds; B3 paid a CI cycle for it.

## The five decisions, and the two that were not obvious

Three ADRs, because each is something a future session could reasonably undo without knowing what it
cost: [ADR-0030](../../adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md) (the
mechanics), [ADR-0031](../../adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md) (where the
number comes from), [ADR-0032](../../adr/0032-retention-never-deletes-the-last-good-backup.md) (the
floor).

Two of them are worth restating here because they turned out to be load-bearing in ways the brief
only half anticipated.

### Stamping the expiry is what makes the upgrade safe

The brief argued for stamping `expires_at` at backup time on the strength of four cases the schema
does not answer — a deleted schedule, a manual backup, two schedules with different retention, an
edited `retention_days`. All four resolve, and the argument holds.

But the property that actually matters is the one that falls out of it for free: **the migration
backfills nothing.** Every backup taken before this slice has `expires_at IS NULL`, NULL is never
selected, and so the first sweep after an upgrade deletes exactly zero objects. An operator gets a
full retention period of warning during which `backup retention` fills up and can be read.

Computed-on-read is retroactive by construction. The same feature, built the consistent way, would
have deleted a year of history on the first tick after `docker compose up`.

The distinction against [ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md),
which chose computed-on-read for adherence, is worth carrying forward in one line: **a report may
change its mind; a deletion may not.**

### The observed-backup guarantee is a CHECK constraint, not a WHERE clause

`ADR-0015` says Fleetward must never delete a backup it did not take. The obvious implementation is
`WHERE origin = 'managed'` in the retention query. The obvious implementation is a line somebody
deletes while refactoring, and the consequence of deleting it is destroying a customer's own
`pg_dump` output.

So migration `000003` makes the state transition itself illegal:

```sql
CHECK (NOT (origin = 'observed' AND state = 'expired'))
```

Retention's only means of action is setting `state = 'expired'`. A query that forgets the filter now
raises `23514` and rolls back. Three independent barriers, with the strongest at the bottom, and the
integration test asserts both ends: the sweep declines an observed backup seeded with an expiry that
ran out a year ago, *and* a hand-written `UPDATE backups SET state = 'expired'` with no filter at all
is refused by name.

The cost, stated so it is not rediscovered: `expired` can never mean "the DBA's own retention removed
this file". That is a different fact, and being forced to give it a different word is the point.

## What is deleted, and in what order

Three steps, and the third is that there is no third step:

1. `succeeded → expired`, committed on its own;
2. the object is deleted, and only then is `artifact_deleted_at` written;
3. **the row is never deleted** — `bucket` and `object_key` stay intact, because the schema has said
   since migration 000001 that they are "kept even after expiry so an audit can show what once
   existed".

Both of the obvious orders were wrong in a way worth recording. Object-first leaves a row saying
`succeeded` pointing at bytes that are gone — a backup the estate view renders as existing.
Row-first leaves an object nothing points at, and does not work anyway: `restores.backup_id` is
`ON DELETE RESTRICT`, so the database already refuses to delete the row of any backup ever restored.

A crash between 1 and 2 leaves a row that is `expired` with `artifact_deleted_at IS NULL`. **That
leftover is self-reconciling by construction**, because step 2 selects on exactly that predicate.

## No lease, and that is the interesting part

Every other recurring thing Fleetward does is a leased job. Retention is not, and the reason is a
property it has that a backup does not: **it is idempotent.** The state transition is its own guard —
`UPDATE … WHERE state = 'succeeded'` matches a row for exactly one of two concurrent sweeps — and
deleting an absent object is not an error. A lease would be protecting against a collision that
cannot happen.

It also does not fit the job table. `jobs.instance_id` is `NOT NULL` and
`idx_jobs_one_active_per_instance_kind` is keyed on it, so an estate-wide job would have meant making
that column nullable — weakening the constraint whose entire purpose is to stop two dumps running
against one production server. Paying that to schedule a `DELETE` is a bad trade.

**The cost is that there is no job row per sweep**, so `job list` will never answer "did retention
run last night". What replaces it is `GET /api/v1/backup-retention` and
`fleetward-cli backup retention` — read through the *same* SQL constant the sweep uses, because a
preview that answered a slightly different question would be worse than none: it would be believed.

## The floor, which is the part a single rule gets wrong

Retention that is purely time-based will, on a server whose backups have been failing for a month,
delete the last one that worked. Not a bug — the feature doing exactly what it was asked, on the
instance that needed it most.

The floor keeps two rows per instance: the most recent `succeeded` managed backup, and the most
recent one whose verification came back `VERIFIED`. Often the same row.

**Rule 2 is the one that earns its place.** Rule 1 alone, on an instance succeeding and failing
verification for weeks, keeps a backup *known to be unrestorable* and deletes the last one proven
good. The walk reproduced exactly that estate and the preview held:

```
PAST ITS RETENTION AND KEPT ANYWAY (2)
prod-2  267f9ee4…  2026-09-03 09:08:02  kept: it is this instance's most recent successful backup…
prod-2  df67baa9…  2026-09-03 09:06:07  kept: it is the most recent backup of this instance proven restorable…
```

`267f9ee4` had failed verification; `df67baa9` was the only one ever proven good. The three between
them went.

Verification decides the floor and never eligibility, and a `FAILED` verification does **not** delete
sooner: the artifact is the evidence behind the loudest alert this product raises, and an operator is
about to want to look at it.

`FLEETWARD_RETENTION_MIN_KEEP` widens rule 1 and **cannot be zero** — refused at startup by
`config.Validate`, and refused again in `SweepRetention` so a policy assembled in code cannot bypass
it either.

## The walk, which found the defect nothing else did

`docker compose up --build`, two PostgreSQL instances, real MinIO, real artifacts, a one-minute
retention interval so a sweep is observable.

Every claim in the slice was exercised against the running stack:

- **Manual backups carry no expiry; scheduled ones carry a stamped one.** Three `backup run`
  invocations produced `expires_at` NULL; the first scheduled run under `--retention-days 1` produced
  `2026-09-04 09:05:07`, exactly one day after `completed_at`.
- **The artifacts really went.** `mc ls --recursive` before and after: the two keys the preview named
  were present, then absent. `expired=2 artifacts_deleted=2 bytes_reclaimed=148094`.
- **The rows survived intact.** `state = expired`, `artifact_deleted_at` set, `bucket` and
  `object_key` unchanged.
- **The observed backup survived.** Seeded with an expiry 34 days in the past — a state it could
  never reach on its own — offered to every sweep, and still `succeeded` with its
  `external_location` intact at the end.
- **A control plane restarted mid-sweep finished what it started.** A row was put into the state a
  crash leaves (`expired`, artifact present), `docker compose restart fleetward` ran, and the *new*
  process — a different `lease_owner`, `10626efb381ae1e9` rather than `2e0602048051f09a` — closed it
  on its first tick: `expired=0 artifacts_deleted=1 bytes_reclaimed=76344`. The preview had shown it
  under *ALREADY EXPIRED, ARTIFACT STILL PRESENT* beforehand and showed nothing afterwards.

**And it found one defect that every test passed.** The first preview it printed read:

> kept: it is among this instance's **most recent successful backup**, which retention never deletes
> however old they are

A count spliced into a single sentence template, ungrammatical at the width that is the default and
therefore the width almost every operator will ever see. The floor's first rule is now two sentences
rather than one with a number in it, and the unit test asserts both widths. It is a small thing, and
it was in the only sentence explaining why an artifact an operator expected to disappear is still
there.

## What the tests are for

The integration suite runs against a real PostgreSQL and a real MinIO, because "the row says the
artifact is gone" is not the claim worth testing — "the bytes are gone, and *these other bytes* are
still there" is. Every one of them is an attempt to make retention delete something it must not:

an observed backup offered an ancient expiry; a five-year-old backup with no expiry surrounded by
newer ones so the floor is not what is saving it; an instance where every backup is past due; an
instance failing verification for weeks; a backup with a `running` verification *and* one named only
by a `pending` verify job, which is the window the obvious guard leaves open; a sweep interrupted
between its two steps; two sweeps racing; and a ceiling of two draining a backlog of five.

## Still open

- **The floor pins between one and two artifacts per instance, and there is no way to reclaim
  them** short of deleting the instance. `DeleteBackup` does not exist — a human deleting one named
  backup needs confirmation, an audit record and RBAC, so it belongs after **B6** — and
  `DeleteInstance(delete_artifacts=true)` is declared in the contract and still unimplemented
  (`inventory/service.go`). Fenced out deliberately rather than missed.
- **Deleting an instance still orphans its objects.** `backups.instance_id` is `ON DELETE CASCADE`,
  so the rows go and the bytes stay. It predates B5, and B5 makes it the *only* remaining way to
  orphan an object. Object keys are `tenants/<t>/instances/<i>/backups/<b>/artifact`, so a
  bucket-versus-rows reconciliation is straightforward whenever it is wanted.
- **Lowering a schedule's `retention_days` does not shrink what is already stored.** Deliberate
  (ADR-0031). The path to the other behaviour is a human re-stamping existing backups on purpose,
  with its own confirmation surface, and it is not built.
- **Nothing alerts on an expired backup, or on a sweep that has been failing all week.**
  `Unreachable` is counted and logged and delivered nowhere. **B7.**
- **The sweep emits no metric and no span.** **B8.**
- **The estate view says nothing about retention.** No column, no screen, and `backup retention` is
  CLI-only. The fence said so.
- **A `verify` job whose verification returned `FAILED` still reads `succeeded` in `job list`.**
  Carried from B2 through B4 and still not fixed here.
- **A job left `running` with no lease is still invisible to the reaper.** Named in B3's journal,
  fenced out again.
- **A backup file left on a plugin's shared directory is still not swept.** Named in B2's journal.
- **Two integration tests fail on this development machine and neither is a regression.**
