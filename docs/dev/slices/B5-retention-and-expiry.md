# B5 — Retention and expiry, or the first slice that can destroy data

Everything Fleetward has built so far reads, reports, or adds. This slice removes. Every artifact
the product has ever written is still in the bucket, and B1's scheduler fills that bucket faster
than anyone was filling it by hand — so the storage bill of an installed Fleetward grows without
bound, forever, by design.

That is the problem. The far more important sentence is the other one:

> **Until this slice, the worst a bug could do was report something untrue. From this slice on, a
> bug deletes a backup.**

Every decision below is made in that light. Where a choice is between "simple" and "cannot destroy
anything by accident", this brief picks the second and says what the first would have cost.

---

## Goal

**A managed backup that has outlived the retention its schedule declared has its artifact deleted,
and nothing else ever is.**

## Why now

**The bucket is the only unbounded thing in the product.** B1 made backups automatic. A nightly
backup of fifty instances is eighteen thousand artifacts a year, none of which Fleetward has any
mechanism to remove. Every slice after this one adds more reasons to run backups; none of them adds
a reason to keep them forever.

**The schema has been waiting since the first migration, and half of it is a lie.**
`schedules.retention_days` is validated, stored on every schedule ever created, round-tripped
through the API and the CLI — and read by nothing. An operator who sets `--retention-days 7` today
is told nothing and gets nothing. A configuration option that is accepted and ignored is worse than
one that does not exist.

**It is cheapest before there is anything else to be careful about.** B6 adds authorization, which
means retention will have to answer "who may delete" as well as "what may be deleted". Settling
*what* first, while the only actor is the scheduler, keeps the two questions separable.

## Preconditions

All hold on `main` at `1ddc7a1`. Verified by reading the code rather than assumed.

- **`backups.expires_at` exists and is never written**
  (`internal/storage/metadb/migrations/000001_init.up.sql:284`). Every `ExpiresAt` elsewhere in
  `internal/` is a presigned-grant TTL and unrelated. No backup Fleetward has ever taken carries an
  expiry.
- **`schedules.retention_days` exists, defaults to 30, is `CHECK (retention_days > 0)`** (`:193`),
  is validated in `scheduler.CreateSchedule` (`internal/controlplane/scheduler/service.go:136`), and
  is consulted nowhere.
- **`idx_backups_expiring ON backups (expires_at) WHERE state = 'succeeded'`** already exists
  (`:294`). The sweep's first query has its index before the sweep exists.
- **`BACKUP_STATE_EXPIRED` is in the state CHECK and in the enum mapping**
  (`internal/controlplane/backup/service.go:1014`), and no code path produces it.
- **`objstore.Delete` exists on the interface and in the S3 implementation, and nothing calls it.**
  It is documented as idempotent — "Deleting an object that does not exist is not an error"
  (`internal/storage/objstore/objstore.go`). That sentence is load-bearing for decision 5.
- **`backups.origin` is `'managed' | 'observed'`** with a CHECK, since migration `000002`. An
  observed row has `bucket` and `object_key` empty by construction, because they mean "an object
  Fleetward owns".
- **The scheduler's tick already does non-job work.** `materialize`, then `reap`, then `dispatch`
  (`internal/controlplane/scheduler/scheduler.go:182`). `reap` is estate-wide, holds no lease, and
  runs in one transaction (`lease.go:182`).
- **`DockerProvider.Sweep` (`internal/controlplane/sandbox/docker.go:752`) is the existing pattern**
  for "destroy what this process does not own, idempotently, at a moment nobody asked for".
- **A verification's row exists in `pending`/`running` while it runs** (`verify.go:638`, `:660`),
  and a queued verify job carries its target as `jobs.payload->>'backup_id'`
  (`scheduler/service.go:508`). Both are queryable in SQL — which is what makes the concurrency
  guard in decision 5 possible.
- **`restores.backup_id` is `ON DELETE RESTRICT`** (`000001_init.up.sql:329`). The database already
  refuses to delete a `backups` row that was ever restored.
- **`schedules.kind` permits `backup`, `discovery`, `metrics`, `observe`; `jobs.kind` adds `verify`
  and `restore`.** Unlike B4's `discovery`, neither list contains anything a retention job could
  use. This slice cannot be free of a migration the way B4 was.
- **There is no `DeleteBackup` RPC.** The only `delete:` bindings in the contract are
  `/api/v1/instances/{id}` and `/api/v1/schedules/{id}`.

## Design decisions already made

Not open. Relitigating any of these is the expensive failure mode this section exists to prevent.

- **An observed backup is somebody else's file and Fleetward must never delete it.**
  [ADR-0015](../../adr/0015-observed-and-managed-backups.md), and `STATUS.md` says it in as many
  words. This is not configurable and there is no flag for it. The entire adoption story is that
  pointing Fleetward at an estate changes nothing on it; a tool that deleted a DBA's own `pg_dump`
  output because a retention policy said so has broken that promise permanently — and permanently is
  the right word, because it is the kind of thing a company uninstalls over.
- **Recurring work is a schedule kind with a lease**
  ([ADR-0013](../../adr/0013-internal-scheduler-with-leases.md)), materialized and reaped by B1's
  machinery. Decision 1 argues this one is the exception and says why; the machinery itself is not
  in question.
- **An expired lease fails its job and never re-runs it**
  ([ADR-0025](../../adr/0025-an-expired-lease-fails-its-job.md)).
- **`FAILED` is evidence about the artifact; everything else is `INCONCLUSIVE`**
  ([ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md)). Decision 4 is about
  whether that verdict may authorise a deletion, not about what the verdict means.
- **Production readiness is a property of this slice**
  ([ADR-0024](../../adr/0024-production-readiness-is-a-slice-property.md)). Whatever this slice
  deletes ships with its enforcement and its limits, here, not later.
- **Core never learns an engine name.** Retention counts days and deletes objects; it asks no plugin
  anything.

---

## The five decisions this slice had to make, and what was chosen

**All five are settled** — decided by the product owner on 2026-09-03, before any code was written.
Each is recorded below with the options that were rejected and what they would have cost, because a
decision whose alternatives are lost is a decision the next session will make again. The three ADRs
in the Files table are where these live permanently; this section is why they say what they say.

### Decision 1 — How retention runs

`retention_days` lives on a schedule, but retention is an estate-wide property far more than a
per-instance one, and every schedule kind so far has been per-instance.

| Option | Migration | At-most-once | Two control planes |
|---|---|---|---|
| **A** — a `retention` schedule kind, one per instance | widen both `kind` CHECKs | lease, as today | one wins the tick |
| **B** — an estate-wide sweep on the tick, beside `reap` | none for the kind | not needed — see below | both sweep; both are correct |
| **C** — a job kind with no `schedules` row behind it | widen `jobs.kind` **and** make `jobs.instance_id` nullable | lease, as today | one wins the claim |

**A** is the tidy answer and the wrong one. It puts a per-instance schedule in front of an
estate-wide property, which means fifty schedules to create by hand, and an instance whose retention
schedule nobody created grows forever *silently*. The failure mode of that design is exactly the
failure this slice exists to remove.

**C** costs the most and buys the least. `jobs.instance_id` is `NOT NULL`, and
`idx_jobs_one_active_per_instance_kind` is keyed on it — an estate-wide job does not fit the table
without weakening a constraint whose whole job is to stop two backups running against one server.
Paying that in order to schedule a `DELETE` is a bad trade.

**B** is recommended, and the reason is a property retention has that a backup does not: **it is
idempotent.** Two control planes sweeping at the same instant is not a race to lose. The state
transition is the guard — `UPDATE … WHERE state = 'succeeded' AND …` matches a given row for exactly
one of them, and the other sees nothing. Object deletion is idempotent by the interface's own
contract. So the lease machinery would be protecting against a collision that cannot happen, which is
precisely why it is not needed here and is needed for a `pg_dump`.

**What B costs, stated plainly.** There is no `jobs` row per sweep, so "did retention run last night,
and what did it remove" is not answerable from `job list`. The answer instead lives on the rows
themselves — a backup carries the state `expired` and, under decision 5, the moment its artifact went
— plus a log line per sweep. That is a weaker operational surface than a job row, and it is the price
of not bending the job table into a shape it was not built for. The CLI preview under decision 5 is
what compensates.

> **Chosen: B.** Estate-wide, on the tick, beside `reap`, paced by
> `FLEETWARD_RETENTION_INTERVAL` so it does not run every ten seconds. It still needs **a
> migration** — for decisions 2 and 5 — just not for a schedule kind.

### Decision 2 — What "retention" is counted against

`retention_days` is on a schedule; an expiry is a property of a backup. Four cases sit between them,
and one choice answers all four:

| Case | **Stamped at backup time** | **Computed on read from the schedule** |
|---|---|---|
| the schedule was deleted (`schedule_id` is now NULL) | unaffected — the value is already written | falls back to *something*; deleting a schedule silently changes what may be destroyed |
| a manually triggered backup, no schedule ever | gets no expiry — kept until a human says otherwise | no policy exists; needs an invented default |
| two enabled backup schedules, different retention | each backup carries the retention of the schedule that made it | must pick one, and the pick is arbitrary |
| `retention_days` edited after backups were taken | old backups keep the old expiry; the edit applies from the next backup onward | **editing a number retroactively destroys artifacts on the next tick** |

B3 chose computed-on-read for adherence and wrote down why
([ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md)). That
reasoning does **not** transfer, and the difference is worth stating precisely: adherence computes a
*report*, which must reflect the declaration in force now and is harmless when it changes. Retention
computes an *authorisation to destroy*, which must reflect the declaration in force when the backup
was taken, must be stable, and must be auditable after the fact. A report may change its mind; a
deletion may not.

There is a second benefit, and it is the one that decides this slice's blast radius. **A stamped
expiry means the migration backfills nothing.** Every backup taken before B5 has `expires_at IS
NULL`, NULL means "never expires", and the first sweep after an upgrade therefore deletes exactly
zero objects. An operator who upgrades gets a full retention period of warning before Fleetward
removes anything, and can watch the preview fill up before it does. Computed-on-read has the opposite
property: it is retroactive by construction, and the first sweep after an upgrade would delete a year
of history.

> **Chosen: stamp `expires_at` when the backup succeeds**, from the `retention_days` the
> schedule carried at the moment the job was materialized — snapshotted into `jobPayload` exactly the
> way `method_id`, `options` and `verify_policy` already are (`scheduler/runner.go`,
> `scheduler/service.go:507`). A manual backup with no schedule gets NULL. **NULL is never deleted,
> ever, by anything in this slice.**
>
> The consequence to accept: lowering a schedule's retention does not shrink what is already there.
> The deliberate path to that behaviour is a human re-stamping existing backups on purpose, which is
> **not built here** — see the scope fence.

### Decision 3 — What protects the last good backup

A correct, purely time-based implementation of "delete anything older than 30 days" will, on a server
whose backups have been failing for five weeks, delete the last backup that worked. That is the
single most damaging thing this product could do, and it is not a bug — it is the feature doing what
it was asked.

Options: keep the last N; never delete the newest; never delete the only one; no floor at all.

> **Chosen: a floor of two rows per instance, and it cannot be configured to zero.**
>
> Retention never removes:
> 1. the **most recent `succeeded` managed backup** of an instance, whatever its expiry says; and
> 2. the **most recent managed backup whose latest verification is `VERIFIED`** — often the same row,
>    and on a sick instance the row that matters most.
>
> `FLEETWARD_RETENTION_MIN_KEEP` widens rule 1 to N; its default is 1 and **0 is refused at startup**,
> the way a lease heartbeat longer than its TTL is refused today.

Rule 2 is what makes the floor worth having rather than decorative. Rule 1 alone, on a server whose
backups have been succeeding and failing verification for a month, keeps a backup known to be
unrestorable and deletes the last one proven good. Rule 2 keeps the proof.

**What this costs when somebody genuinely wants the storage back.** Between one and two artifacts per
instance become undeletable through Fleetward — on fifty instances, up to a hundred artifacts pinned
indefinitely — and rule 2 can pin an old artifact forever on an instance that never verifies again.
Today the only escape hatch is deleting the instance, and `DeleteInstance(delete_artifacts=true)` is
declared in the contract and not honoured (`inventory/service.go:534`). Whether that gap closes here
is a scope-fence question below.

### Decision 4 — Whether verification status participates

Three positions are defensible and they lead to different products:

- **An unverified backup is the first thing to delete.** It proves the least, so it is the cheapest
  to lose. Against it: "unverified" usually means *nobody checked*, not *it is bad*. Deleting on the
  basis of ignorance means an estate where verification is failing to run — a Docker daemon that went
  away, a `sampled` policy set to 5% — loses its backups faster precisely because it is less healthy.
- **An unverified backup is the last thing to delete.** Symmetrically wrong: it makes the unproven the
  most durable thing in the system.
- **Verification does not decide eligibility at all.**

> **Chosen: verification does not decide eligibility. Time does. Verification decides only the
> floor** (rule 2 above).
>
> Eligibility stays a subtraction an operator can do in their head: *taken more than N days ago*. That
> predictability is not a convenience — it is what lets somebody look at the preview and know it is
> right. The moment eligibility depends on a verdict, "why did that one go and this one stay" becomes
> a question only the source code answers.
>
> Explicitly: **a `FAILED` verification does not accelerate deletion.** It is tempting — the artifact
> is known bad, so why keep it — and it is wrong, because a failed verification is the loudest signal
> this product has and the artifact is the evidence behind it. Deleting the evidence early destroys
> the investigation. It expires on its ordinary schedule like everything else.

### Decision 5 — What is deleted, and in what order

A backup is a row and an object. There is no transaction across both, so one of them commits first and
the crash window belongs to whichever is second.

- **Object first, then the row** → a crash leaves a row saying `succeeded`, pointing at an artifact
  that is gone. The estate view and `backup show` render a backup that does not exist, and a verify
  job against it fails with a confusing error. **Actively misleading.**
- **Hard-delete the row first, then the object** → a crash leaves an object nothing points at,
  invisible and unrecoverable except by listing the bucket. It also does not work: `restores` is
  `ON DELETE RESTRICT`, so the database refuses this for any backup ever restored.

> **Chosen: neither. Three steps, and the row is never deleted.**
>
> 1. **`succeeded` → `expired`**, committed on its own. From that instant the row no longer claims a
>    restorable artifact, `loadVerificationTarget` refuses it (it requires `succeeded`), and the
>    estate view renders it honestly.
> 2. **Delete the object**, then set a new column `backups.artifact_deleted_at`.
> 3. **Never `DELETE FROM backups`.** The row is the audit record — the schema already says so on
>    `bucket`/`object_key`: *"Kept even after expiry so an audit can show what once existed."*
>
> A crash between 1 and 2 leaves a row that is `expired` with `artifact_deleted_at IS NULL` and an
> object still in the bucket. **That is self-reconciling by construction**: step 2 selects on exactly
> that predicate, so the next sweep — on any control plane, not necessarily the one that died —
> finishes the job, and `objstore.Delete` being idempotent makes the re-run free.

**And the guard decision 5 must carry: a backup being verified is not eligible.** A verify job
downloads the artifact at run time; retention deleting it underneath is a real race, and the lease
machinery does not cover it because the lease is on a job row and this is a different row. Step 1's
predicate therefore excludes any backup with a `verifications` row in `pending`/`running`, **and** any
backup named by a `pending`/`running` `verify` job's payload — the second is necessary because a
queued verification has a job before it has a verification row. The same shape covers `restores`.

---

## Files

### New

| Path | What |
|---|---|
| `internal/storage/metadb/migrations/000003_retention.up.sql` / `.down.sql` | `backups.artifact_deleted_at`; the CHECK that makes an observed backup unexpirable; the delete-queue index. **No backfill of `expires_at`** — that is the safety property, and the migration says so. |
| `internal/controlplane/backup/retention.go` | `Service.SweepRetention` and `Service.PreviewRetention`: the two statements, the floor, the guards, the object deletion. |
| `internal/controlplane/backup/retention_test.go` | Unit — the floor's arithmetic, the eligibility predicate, `min_keep` validation. |
| `internal/controlplane/backup/retention_integration_test.go` | Against a real Postgres and MinIO. The list is in **Done when**. |
| `internal/controlplane/scheduler/retentionrunner.go` | The adapter, in the shape of `observerunner.go`. |
| `docs/adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md` | Decisions 1 and 5, plus the observed-backup CHECK. |
| `docs/adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md` | Decision 2. |
| `docs/adr/0032-retention-never-deletes-the-last-good-backup.md` | Decisions 3 and 4. |
| `docs/ops/retention.md` | The reference page, in the register of `scheduling.md`. |
| `docs/dev/journal/B5-retention-and-expiry.md` | Close-out. |

### Modified

| Path | What |
|---|---|
| `internal/controlplane/backup/service.go` | `recordSuccess` stamps `expires_at` from the run's snapshotted retention. |
| `internal/controlplane/scheduler/{service,runner,scheduler}.go` | `retention_days` into `jobPayload`; a fifth `Runner` method; the sweep on the tick beside `reap`, on its own interval. |
| `internal/config/config.go`, `config_test.go` | `RetentionConfig`: `Enabled`, `Interval`, `MinKeep`, `MaxPerSweep`. `MinKeep = 0` refused at startup. |
| `api/proto/fleetward/v1/controlplane.proto` | One additive RPC, `PreviewRetention` → `GET /api/v1/backup-retention`. Additive, so `buf breaking` stays green. |
| `cmd/fleetward-cli/backup.go` | `fleetward-cli backup retention` — what the next sweep would remove, what the floor is holding, and why each protected row is protected. |
| `docs/dev/STATUS.md`, `docs/roadmap.md`, `README.md`, `docs/dev/data-model.md`, `docs/ops/configuration.md`, `docs/dev/slices/README.md` | Per the protocol. The last two are generated — `make docs`. |

## Reuse, do not rewrite

- **`reap` (`scheduler/lease.go:182`)** is the model for the sweep: estate-wide, no lease, one
  transaction, returns a count, logs at WARN when the count is non-zero. Follow its shape.
- **`DockerProvider.Sweep` (`sandbox/docker.go:752`)** is the model for idempotent destruction that is
  safe to re-run and safe to run concurrently.
- **`observerunner.go` / `discoveryrunner.go`** are the fourteen-line adapters a fifth `Runner` method
  follows.
- **`jobPayload` (`scheduler/service.go:500`)** already snapshots a schedule's parameters at
  materialize time so an edit mid-run changes nothing. `retention_days` is one more field, not a new
  mechanism.
- **`objstore.Delete`** exists, is idempotent, and needs no new interface method.
- **`GetBackupAdherence` (`backup/adherence.go:53`)** is the model for a read-only, estate-wide,
  batched query answering one question — which is what `PreviewRetention` is.
- **`backupColumns` / `scanBackup` (`backup/service.go:919`)** already read `expires_at`. It is on the
  wire (`Backup.expires_at`, field 12) and rendered by nothing.

## Traps

- **A `WHERE origin = 'managed'` is not a guarantee.** It is a line someone deletes in six months
  while refactoring, and the consequence is destroying a customer's own backups. Make the state
  transition itself impossible: a CHECK constraint `NOT (origin = 'observed' AND state = 'expired')`,
  so a query that forgets the filter raises `23514` and fails loudly instead of succeeding quietly.
  That, plus the empty `object_key` on every observed row, plus the query's own filter, is three
  independent barriers — and the database-level one is the only one that survives a future author who
  has not read ADR-0015. It forecloses ever using `expired` to mean "the DBA's own retention removed
  this file"; that is a different fact and deserves a different word.
- **`ON DELETE SET NULL` on `backups.schedule_id` means deleting a schedule changes which retention
  applies to its backups.** Under decision 2 this stops being true, because the expiry was already
  written — which is one of the strongest arguments for stamping. If decision 2 goes the other way,
  the trap is live and must be answered explicitly rather than discovered.
- **A backup mid-verification is a real race and the lease does not cover it.** See decision 5. Both
  halves of the guard are needed: the `verifications` row and the queued `verify` job.
- **The first sweep after an upgrade is the dangerous one.** Under the recommendations it deletes
  nothing, because nothing carries an expiry. Any design that backfills `expires_at` deletes a year of
  history on the first tick after `docker compose up`.
- **Bound the sweep.** `FLEETWARD_RETENTION_MAX_PER_SWEEP` exists so that a bug is bounded, not because
  the query is slow. A destructive loop with no ceiling is the wrong shape regardless of how correct
  it looks.
- **Deleting a base backup destroys every incremental that depends on it.** No engine here produces a
  chain today — `pg_dump` and `BACKUP DATABASE` both write standalone artifacts, and
  `backups.additional_artifacts` is declared and never populated. So it is not a bug in this slice; it
  is a design this slice must not make harder to add later. Do not write anything that assumes
  artifacts are independent of one another.
- **Deleting an instance still orphans its objects.** `backups.instance_id` is `ON DELETE CASCADE`, so
  the rows vanish and the objects do not. This predates B5, and B5 makes it the *only* remaining way to
  orphan an object. See the scope fence.
- **`make` is not installed on this machine.** Run the targets directly, and say so rather than
  reporting `make lint test` as passing.
- **On Windows, `gofmt -l`, `buf format --diff` and golangci-lint's `whitespace` linter report
  `core.autocrlf` artefacts.** Verify in a worktree created with
  `git -c core.autocrlf=false worktree add` before believing any of them. `go test -race` needs cgo,
  which a stock install does not have.
- **If `api/proto/` is touched, regenerate in an LF worktree.** `grep -cF '\r\n'
  api/openapi/openapi.yaml` must print 0. `make proto` also regenerates `web/src/lib/api.gen.ts`, and
  CI diffs both
  ([ADR-0029](../../adr/0029-the-openapi-document-is-generated-to-match-the-wire.md)).
- **Run `go vet -tags=integration ./...` and `go vet -tags=conformance ./...` over the whole tree
  before pushing.** A fifth `Runner` method breaks `stubRunner` in `scheduler/integration_test.go` — a
  file this slice has no reason to open. B4 caught exactly this in eight seconds; B3 paid a full CI
  cycle for it.
- **Do not run `make conformance` and the integration suite at the same time.** Both start containers,
  they contend for Docker, and the failures are not real.
- **Two integration tests fail on this machine and neither is a regression.** Both are in `STATUS.md`'s
  environment notes and both reproduce on `main`.
- **Run `go mod tidy` before pushing**; `npm ci` rather than `npm install` if `package.json` moves.

## Scope fence

**In:** expiry stamped on managed backups; the estate sweep and whatever decision 1 settles; the
floor; the artifact deletion and its reconciliation; the configuration and its refusals; a read-only
preview RPC and its CLI; migration `000003`; three ADRs; `docs/ops/retention.md`; `STATUS.md`; the
journal.

**Out, deliberately:**

- **Deleting anything observed.** Not a feature, not a flag, not a follow-up.
- **Authentication and authorization.** Retention is driven by the scheduler; "who may delete" is
  **B6**.
- **Any alert about an expired or expiring backup**, and alert delivery generally. **B7.**
- **Metrics or spans on the sweep.** **B8.**
- **The release artifact.** **B9.**
- **A `DeleteBackup` RPC, or any delete action in the UI, or any new screen.** A human deleting one
  named backup is a different feature with a different safety story — confirmation, audit, RBAC — and
  it belongs after B6.
- **Re-stamping existing backups when a schedule's `retention_days` changes.** The deliberate path to
  "make my edit apply retroactively", and it needs its own confirmation surface.
- **Honouring `DeleteInstance(delete_artifacts=true)`** (`inventory/service.go:534`). Considered and
  fenced out: it is an inventory change, the flag defaults to false so nothing regresses, and the
  orphan it leaves is a known gap rather than a new one. Two consequences follow and both are the
  journal's to record rather than to fix quietly — deleting an instance stays the one remaining way
  to orphan an object in the bucket, and there is therefore **no way to reclaim the storage the floor
  in decision 3 pins**, short of removing the instance.
- **A bucket-versus-rows reconciliation tool.** Object keys are
  `tenants/<t>/instances/<i>/backups/<b>/artifact`, so it is straightforwardly possible later. Not now.
- **Sweeping a plugin's leftover file on a shared directory** (B2's journal, still open).
- **Widening the job reaper to catch a `running` job with no lease** (B3's journal, still open).
- **The remaining five engines.**

## Done when

Commands and their expected output, not adjectives. `make` is absent here; the direct equivalents are
what gets run and what gets reported.

```
go build ./...                              ok
go vet ./...                                ok
go vet -tags=integration ./...              ok
go vet -tags=conformance ./...              ok
go test ./...                               ok, no failures
go test -tags=integration ./internal/...    ok, bar the two known machine failures
go run ./tools/docscheck                    no problems
go mod tidy                                 no drift
golangci-lint run                           0 issues   (in an LF worktree)
buf lint / format / breaking                clean      (breaking green: the RPC is additive)
npm run lint / build                        clean      (only if api.gen.ts moves)
make conformance                            passes, unchanged — no plugin is touched
```

And these specific facts, each asserted by a test that fails if the behaviour goes away:

1. **An observed backup offered to the sweep survives**, and the same test asserts that forcing
   `state = 'expired'` onto an observed row raises a constraint violation. Both halves: the query
   declines it, and the database would refuse even if the query did not.
2. **A backup past its expiry has its object deleted and its row set to `expired`**, and the row is
   still readable afterwards with its `bucket`/`object_key` intact.
3. **A backup with `expires_at IS NULL` is never selected**, including one older than every configured
   retention.
4. **The floor holds.** An instance whose every backup is past expiry keeps its most recent
   `succeeded` one; an instance whose recent backups all failed verification also keeps the most
   recent `VERIFIED` one.
5. **A backup with a `running` verification is not expired**, and neither is one named by a `pending`
   verify job.
6. **A sweep interrupted between the state change and the object deletion is completed by the next
   sweep**, with no error, and a third sweep changes nothing.
7. **Two sweeps running concurrently delete each object once** and neither reports an error.
8. **`MinKeep = 0` is refused at startup** with a message saying why.

### And the walk, which is not optional

B3's walk found two defects and B4's found two more, all four in seams between components that every
other check passed. For this slice it must include:

- `docker compose up --build`, a real estate, a real MinIO, real artifacts.
- **A backup that is actually deleted** — the object confirmed present in MinIO before, confirmed
  absent after, and the estate view and `backup show` rendering the row honestly in between.
- **An observed backup offered to the same code path, which survives**, with its evidence intact.
- **A control plane restarted mid-sweep** — between the state change and the object deletion — and the
  next sweep finishing what it started, with nothing left behind and nothing deleted twice.
- **An instance whose backups all fail verification**, proving that the one artifact proven good weeks
  ago is still there.
- The preview run before each of the above, and its answer matching what actually happened.
