# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice B5 is complete. Next is B6 — the authorization spine.**

Fleetward now deletes things, and that is a different kind of product from the one that shipped
yesterday. Every slice before this one read, reported, or added; from here on the worst a bug can do
is not report something untrue but destroy a backup.

A managed backup that has outlived the retention its schedule declared loses its artifact. Nothing
else does, and the "nothing else" is where the slice went:

| The thing | What stops it |
|---|---|
| deleting a backup Fleetward did not take | a `CHECK` constraint, not a `WHERE` clause — a query that forgets the filter raises 23514 |
| deleting an instance's last working backup | a floor of two rows: the newest successful one, and the newest proven restorable |
| deleting a year of history the first time somebody upgrades | expiries are stamped at backup time, so every backup that already exists carries none |
| leaving a row that claims an artifact which is gone | the row is expired and committed *first*; the object goes second; the row is never deleted |
| deleting an artifact something is restoring from | a backup with a verification or a restore in flight — or merely queued — is not eligible |

Three decisions were worth records rather than comments:
[ADR-0030](../adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md) — the sweep is
estate-wide, runs on the scheduler's tick beside the reaper, and holds **no lease**, because
retention is idempotent in a way a backup is not;
[ADR-0031](../adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md) — an expiry is stamped when
the backup is taken and never recomputed, so a report may change its mind and a deletion may not;
[ADR-0032](../adr/0032-retention-never-deletes-the-last-good-backup.md) — the floor, and why
verification decides it rather than deciding what is eligible.

The operational surface is `fleetward-cli backup retention`, which reads through the same SQL the
sweep does. It exists because there is no job row per sweep to inspect afterwards, and because this
is the one feature where seeing the answer before it is acted on matters.

Session protocol: [`slices/README.md`](slices/README.md). B6's brief is not written yet; briefs are
written when the slice starts.

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1–B5 done, B6 next |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **There is no authentication or authorization.** Every route under `/api/v1/` is open to anyone
  who can reach the port, including the ones that add an instance, create a schedule, trigger a
  backup, and poll an instance's backup history. So is the estate view, which is why it carries no
  login screen, no account menu and no "signed in as": a UI implying a protection that does not exist
  is worse than one that says nothing. `cfg.Auth` is parsed and validated and read by no file outside
  `internal/config`. The tenant is the constant `metadb.DefaultTenantID`. **B6.**
- **Five of the eight engines are still binaries that only handshake.** MySQL, MongoDB, and Redis
  declare no capabilities; Oracle, ClickHouse, and Cassandra have no binary at all. PostgreSQL and
  SQL Server are real. **B11–B16.**
- **There is no way to delete one named backup, and no way to reclaim what the retention floor
  pins.** The floor keeps between one and two artifacts per instance forever
  ([ADR-0032](../adr/0032-retention-never-deletes-the-last-good-backup.md)), and the only escape is
  removing the instance. A `DeleteBackup` action needs confirmation, an audit record and RBAC, so it
  belongs after **B6**.
- **Deleting an instance orphans its artifacts.** `backups.instance_id` is `ON DELETE CASCADE`, so
  the rows go and the objects stay, and `DeleteInstance(delete_artifacts=true)` is declared in the
  contract and unimplemented. Since B5 this is the only remaining way to orphan an object. Keys are
  `tenants/<t>/instances/<i>/backups/<b>/artifact`, so reconciling a bucket against the rows is
  straightforward whenever somebody wants it.
- **Lowering a schedule's `retention_days` does not shorten what is already stored.** Deliberate: an
  expiry is stamped when a backup is taken and never recomputed, which is what keeps an edit to a
  number from destroying artifacts on the next tick. Re-stamping existing backups on purpose is a
  separate action with its own confirmation surface, and it is not built.
- **A renamed backup file is a second backup.** PostgreSQL's observation reads a directory, which
  assigns no identity, so the plugin derives one from the file name and declares that it did. Core
  reports the caveat on every answer that rests on it rather than inventing a matching heuristic
  ([ADR-0027](../adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md)).
- **An observed backup's finish time is approximate on SQL Server 2019 and older.**
  `CURRENT_TIMEZONE_ID()` arrived in 2022, and it is the only function that returns something
  `AT TIME ZONE` accepts — `CURRENT_TIMEZONE()` returns a display name the engine then rejects. An
  older instance can offer only its current offset, which is wrong by one daylight-saving transition
  for a backup on the other side of one. Those records carry a flag, the compliance window is widened
  by an hour to match, and the answer says so.
- **The observation horizon and overlap are constants rather than configuration.** A first poll of an
  instance reads thirty days back; every poll re-reads six hours before its watermark. Both are named
  and reasoned about in `internal/controlplane/backup/observe.go`. Deliberate, and written down
  because it is a choice rather than an oversight.
- **A backup file left on a shared directory is not swept.** The plugin removes it on every path out
  of a backup or a restore, including failure, but a plugin killed between the two leaks an
  artifact-sized file on the share. A sandbox's own directory is removed with the sandbox; a real
  instance's is not. It has the shape slice A3 solved for containers with a startup sweep, and it
  does not have that sweep. Unrelated to retention, which only ever deletes objects in the bucket.
- **A SQL Server manifest is exact only on a quiescent database.** `BACKUP DATABASE` is consistent at
  the LSN it ends on, and a `COUNT(*)` cannot be tied to that LSN without writing to the monitored
  instance. The plugin brackets the counting pass and flags an object that changed underneath it, and
  a mismatch on a flagged object is `INCONCLUSIVE` rather than `FAILED`. So a busy database verifies
  more weakly than a quiet one, and says so in its report.
- **Nothing is delivered anywhere.** `alert_rules`, `alerts`, and `notifiers` exist in the schema
  and no Go code touches them. A failed verification, a missed backup window, a schedule that has
  silently stopped firing, and a retention sweep whose object store has been refusing all week are
  all visible only by polling the API or reading the log. **B7.**
- **Fleetward cannot be observed.** OpenTelemetry is wired in `internal/telemetry/otel.go` with
  zero call sites: no span is started and no meter obtained. There is no `/metrics`. The scheduler
  and the retention sweep emit log lines and a readiness component, and nothing else. **B8.**
- **Nothing has been released.** No tag, no published container image, no signed artifact —
  `release.yml` installs cosign and never invokes it. `docker-compose.yml` is a development
  configuration by its own declaration. **B9.**
- **The web UI is one screen and a status page.** The Estate Overview reads the estate and reports
  on it; nothing in it changes anything. Adding or editing an instance, managing schedules and jobs,
  any view of an individual backup's verification report, and anything at all about retention are
  CLI-only.
- **A verification carried on the estate view omits its per-check detail.** `GetBackupAdherence`
  attaches the verdict and when it was reached, in one batched query; the checks and discrepancies
  behind that verdict come from `GetBackup`, one backup at a time. Deliberate — no column renders
  them — and written down because the `Verification` on that response is therefore partial.
- **A manually triggered verification is not bounded.** `SCHEDULER_MAX_CONCURRENT_JOBS` bounds
  scheduled work, which is the case that matters on an estate of fifty. A human calling the verify
  endpoint in a loop can still start a sandbox per call.
- **A job left `running` with no lease cannot be reaped.** That state is by definition an orphan —
  nothing is working on it — and the reaper looks for an *expired lease*, which such a row does not
  have. Nothing produces one today: every job kind now writes its terminal state before releasing
  the lease, and B3's walk found and fixed the one path that did not. Recorded because the reaper's
  blind spot is still there.
- **Failed jobs are not retried.** `jobs.max_attempts` exists and nothing decrements against it; a
  failed run waits for its schedule's next occurrence. That is deliberate for now — see the
  alternatives in [ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md) — and a real retry
  policy needs backoff and a window rather than a counter.
- **`metrics` schedules do not run.** `backup`, `observe` and `discovery` do. `schedules.kind` also
  permits `metrics`, which is refused at creation: database performance metric collection is
  deferred deliberately rather than merely unbuilt — `CollectMetrics` is in the contract and nothing
  calls it, because performance monitoring was never the pain this product exists to solve.
- **A `discovery` schedule probes health and nothing more.** It does not re-run `Discover` to refresh
  topology or database lists, despite the kind's name, which is older than the job. It refreshes
  `health`, `health_message`, `engine_version` and `last_seen_at` through the same `TestConnection` a
  human runs — and an instance that is *down* is a successful probe. What fails the job is not being
  able to ask at all.
- **Retention is not a schedule kind and never will be one.** Looking for a `retention` row in
  `schedules` is the mistake worth naming: the sweep is estate-wide, runs on the scheduler's tick
  beside the reaper, and has no schedule row, no job row and no lease
  ([ADR-0030](../adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md)). The consequence
  is that `job list` cannot answer "did retention run last night"; `backup retention` and the log
  line can.

## Environment notes

- Verified end to end on macOS (Apple Silicon) through slice A6, and on Windows (amd64) from
  2026-09-02.
- On Windows, plugin binaries must carry `.exe`: `os.Stat` reports no executable bit for any file
  and `exec.Command` resolves through `PATHEXT`. The Makefile appends `GOEXE` for this reason, and
  CI runs the unit suite on `windows-latest` to keep it working.
- `make` is not present on a stock Windows install. The targets can be run directly; a session on
  Windows without it should say so rather than report `make lint test` as passing.
- On Windows, `go test -race` requires cgo and a C toolchain, which a stock install has neither of.
  The unit and conformance suites run without `-race` there; CI runs them with `-race` on Linux.
- A Windows checkout with `core.autocrlf=true` makes `gofmt` and `buf format --diff` report every
  file in the tree as unformatted, and `golangci-lint`'s `whitespace` linter report a finding in
  `tools/wikigen/nav.go` that is not there. Line-ending artefacts, not findings — check a branch in a
  worktree created with `core.autocrlf=false` before believing any of them.
- **Regenerating `api/openapi/openapi.yaml` on Windows corrupts it, and the corruption is invisible
  to `git diff`.** The generator embeds `.proto` comments as YAML string literals, so a CRLF checkout
  produces literal `\r\n` escapes *inside* the strings, which no line-ending normalization touches.
  CI regenerates on Linux and rejects the diff. Regenerate in an LF worktree, or normalize the
  sources under `api/proto/` to LF first — `grep -cF '\r\n' api/openapi/openapi.yaml` must print 0.
- **Which conformance cases run here depends on what a case needs, not on which engine it is.** The
  SQL Server plugin shells out to nothing, so every case runs for it. PostgreSQL's backup and restore
  cases skip for want of `pg_dump`, `pg_restore` and `psql` on `PATH` — but its **backup-history case
  runs**, because observation shells out to nothing either and the tools gate belongs to the backup
  path alone. Read the skip reasons rather than the exit code.
- `mcr.microsoft.com/mssql/server:2022-latest` is 625 MB and becomes ready in about nine seconds
  warm. A full conformance run takes a little under three minutes on this machine with the image
  already pulled — 164 seconds at B5.
- **The `web` image occasionally fails to build here** with `failed to prepare extraction snapshot …
  parent snapshot does not exist`. It is a Docker Desktop containerd-snapshotter fault, not a
  Dockerfile one; `docker compose up --build <the other services>` works, and a `docker builder
  prune` clears it. Recorded because it is easy to mistake for a broken web build.
- **Two integration tests fail on this machine for reasons that are the machine's.** Both were
  reproduced on `origin/main` at f0c604f before being blamed on anything.
  `sandbox.TestSandboxLifecycle` connects to its sandbox as `localhost`, which resolves to both
  `127.0.0.1` and `::1`; the sandbox refuses TLS on its mapped port, and the fallback attempt reaches
  the PostgreSQL this machine has installed on `::1:5432` and fails authentication against it.
  `plugins/postgres`'s `TestDiscoverOnUnreachableInstanceFails` dials 192.0.2.1 expecting
  `CONNECTION_FAILED` and gets something pgx reads as `AUTHENTICATION_FAILED` on this network.
  Neither reproduces in CI.
