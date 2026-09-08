# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice D1 is complete. Next is B7 — alert rules and delivery.**

Six slices had shipped and none of them had ever been shown to anybody. `make demo` now tells the
product's story on a real stack in one command, ending with a backup that fails verification on
purpose — and `make demo-check` runs the same program in CI on every pull request, so it cannot
quietly stop working. `test/e2e/` had been empty since the foundation, holding a package comment
that described this slice and no test; it holds the test now.

| The thing | What holds it |
|---|---|
| a demo that quietly stops matching the product | it *is* the end-to-end test; a renamed field fails `End-to-end demo` on the pull request that renamed it |
| a demo that shows something not built | act 7 prints that alerts do not exist and names B7 |
| a fixture passed off as live | every seeded sentence goes through `Narrator.Seeded` and comes out marked, on screen and in `docs/demo.md` |
| the retention sweep blanking the estate mid-demo | seeded backups carry no expiry; exactly five on one instance are stamped, deliberately, and acts 5 and 6 are built out of them |
| the demo writing to somebody's own database | the one place it writes to a monitored instance goes through `docker compose exec`, never a published port |
| a second run confused by the first | the seeder clears the previous demo estate before it seeds; run twice and the second is not confused |

One decision was worth a record:
[ADR-0037](../adr/0037-the-demo-and-the-end-to-end-test-are-one-program.md) — the demo and the
end-to-end test are one program, because whichever of a split pair CI does not run is the one that
lies.

The operational surface is `make demo`, `make demo-keep` and `make demo-check`, all three of which
are `go run ./tools/demo` or `go test -tags=e2e` underneath, so a machine without `make` loses
nothing. The page is [`../demo.md`](../demo.md), and its first section is what is seeded rather than
what is impressive.

## What comes next, and why that order

**B7 — alert rules and delivery.** `alert_rules`, `alerts` and `notifiers` have existed in the schema
since migration 000001 and no Go code touches them. Today a failed verification, a missed backup
window, a schedule that has silently stopped firing and a retention sweep whose object store has
been refusing all week are visible only by polling the API or reading the log — which is the
difference between a dashboard and monitoring.

It also fills act 7. The demo's most dramatic beat is an alert firing on the artifact act 4
corrupts, and the act list was written so that inserting it is an addition rather than a rewrite.

Session protocol: [`slices/README.md`](slices/README.md). B7's brief is not written yet; briefs are
written when the slice starts, and D1's was the one exception — written ahead, deliberately, so a
fresh session could start it cold.

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1–B6 done, B7 next |
| D1 — the demo, and the end-to-end test it is | ✅ [journal](journal/D1-the-demo.md) |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **Authentication is an API token, not your identity provider.** OIDC is **B10**, behind the seam
  B6 built: one more implementation of `authn.Authenticator` and one branch in the session handler.
  Until then an administrator issues bearer tokens and the UI exchanges one for an `HttpOnly`
  session cookie. Dex is in the compose stack and nothing talks to it.
- **Most listings need a tenant-wide grant or an explicit filter.** Scope comes from the request, so
  a request naming no instance and no environment is asking about the whole estate. Two listings are
  the exception and filter their own rows — `ListInstances` and `GetBackupAdherence`, which is what
  makes the CLI and the estate view work for somebody granted three servers. `ListBackups`,
  `ListSchedules` and `ListJobs` do not: a scoped caller passes `--instance` and gets an answer, or
  gets a 403 for asking about the estate
  ([ADR-0035](../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md)).
- **A session outlives the revocation of the token it was minted from**, until `AUTH_SESSION_TTL`
  expires it (12 hours by default). The session is a signed statement rather than a row, which is
  what keeps a table from growing and what makes this true. A revoked token also keeps working for
  up to `AUTH_PRINCIPAL_CACHE_TTL` — 15 seconds, refused above five minutes — on a replica that did
  not perform the revocation.
- **`audit_log` grows and nothing prunes it.** About 150 rows a day on an estate of fifty: roughly
  55,000 a year, tens of megabytes. `DELETE` is refused by trigger, which is the entire point of the
  trigger, so pruning needs monthly range partitioning and its own decision record. Migration 000004
  adds the index that makes the age question cheap, and nothing else about it is built.
- **There is no rate limiting and no lockout.** A token is 128 bits of entropy, so guessing is not
  the threat; a flood of 401s is a denial-of-service question and belongs in front of the control
  plane rather than inside it.
- **A restart signs everybody out** unless `AUTH_SESSION_KEY_FILE` is configured, because the
  session signing key is generated per process by default. Right for one node; a second replica has
  to configure it or watch users bounce between them.
- **Users, roles and tokens are CLI-only.** There is no administration screen. The UI acquired a
  sign-in and an identity in the sidebar, and nothing else.
- **Five of the eight engines are still binaries that only handshake.** MySQL, MongoDB, and Redis
  declare no capabilities; Oracle, ClickHouse, and Cassandra have no binary at all. PostgreSQL and
  SQL Server are real. **B11–B16.**
- **There is no way to delete one named backup, and no way to reclaim what the retention floor
  pins.** The floor keeps between one and two artifacts per instance forever
  ([ADR-0032](../adr/0032-retention-never-deletes-the-last-good-backup.md)), and the only escape is
  removing the instance. `DeleteBackup` was waiting on the RBAC and the audit record B6 built; it is
  still not written.
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
- **The demo asserts nothing through a browser.** `make demo` drives the REST API and the metadata
  database; the compose smoke test is what asserts the web UI is served, and the web unit tests are
  what assert the estate view renders the two-part status. If that screen stopped rendering a failed
  verification correctly, the demo would still pass. Deliberate — see the scope fence in
  [`slices/D1-the-demo.md`](slices/D1-the-demo.md) — and worth knowing before trusting the demo as
  proof of the UI.
- **`make demo-check` must not run beside `make conformance` or `make test-integration`.** All three
  start containers and contend for one Docker daemon, and the result is a screenful of failures that
  are not real. CI sequences the `End-to-end demo` job after `Dev stack smoke test` for the same
  reason; locally it is a habit rather than a guard.
- **A demo run interrupted between act 4's corruption and its verification leaves a corrupted
  artifact** in the bucket of a stack started with `--keep`. The next run clears the estate, and the
  object goes with `docker compose down --volumes`. It is a development stack and the artifact is one
  the demo took itself.
- **Nothing is delivered anywhere.** `alert_rules`, `alerts`, and `notifiers` exist in the schema
  and no Go code touches them. A failed verification, a missed backup window, a schedule that has
  silently stopped firing, and a retention sweep whose object store has been refusing all week are
  all visible only by polling the API or reading the log. **B7.**
- **Fleetward cannot be observed.** OpenTelemetry is wired in `internal/telemetry/otel.go` with
  zero call sites: no span is started and no meter obtained. There is no `/metrics`, and a 403 emits
  no metric either. **B8.**
- **Nothing has been released.** No tag, no published container image, no signed artifact —
  `release.yml` installs cosign and never invokes it. `docker-compose.yml` is a development
  configuration by its own declaration. **B9.**
- **The web UI is one screen, a sign-in, and a status page.** The Estate Overview reads the estate
  and reports on it; nothing in it changes anything. Adding or editing an instance, managing
  schedules and jobs, any view of an individual backup's verification report, anything at all about
  retention, and every user or token operation are CLI-only.
- **A verification carried on the estate view omits its per-check detail.** `GetBackupAdherence`
  attaches the verdict and when it was reached, in one batched query; the checks and discrepancies
  behind that verdict come from `GetBackup`, one backup at a time. Deliberate — no column renders
  them — and written down because the `Verification` on that response is therefore partial.
- **A manually triggered verification is not bounded.** `SCHEDULER_MAX_CONCURRENT_JOBS` bounds
  scheduled work, which is the case that matters on an estate of fifty. A `dba` calling the verify
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
  is that `job list` cannot answer "did retention run last night"; `backup retention`, the audit log
  under `actor = system:retention`, and the log line can.

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
  already pulled — 164 seconds at B5, 306 seconds at B6 on a cold image cache.
- **Git Bash rewrites a Unix-looking argument into a Windows path.** `--backup-dir-local /app/share`
  reaches the CLI as `C:/Program Files/Git/app/share`, and the failure surfaces much later as a
  plugin that cannot write its backup file. Prefix the command with `MSYS_NO_PATHCONV=1`. It is a
  shell artefact and not a product defect, and it costs twenty minutes to diagnose from the far
  end.
- **The `web` image occasionally fails to build here** with `failed to prepare extraction snapshot …
  parent snapshot does not exist`. It is a Docker Desktop containerd-snapshotter fault, not a
  Dockerfile one; `docker compose up --build <the other services>` works, and a `docker builder
  prune` clears it. Recorded because it is easy to mistake for a broken web build.
- **The `web` container exits 133 when the Docker VM is under pressure**, with `Fatal process out of
  memory: Failed to reserve virtual memory for CodeRange` from Node. It starts normally once the VM
  has room — see the note below. The demo reports any container that did not start, by name, and
  carries on, because nothing it asserts goes through a browser; the cost is that the estate view is
  unreachable and act 4's screen going red is the beat that run cannot show.
- **This machine already runs a PostgreSQL on 5432 and a SQL Server on 1433**, and on Windows both a
  local server and Docker's port proxy can bind the same port at once, so a connection reaches
  whichever answers. `.env` therefore needs `FLEETWARD_POSTGRES_PORT=55432`; the demo reads `.env`
  the way compose does, and refuses with that instruction if the database on the port is not
  Fleetward's. It is also why every host-side connection in the demo says `127.0.0.1` rather than
  `localhost`, which resolves to `::1` first and finds the machine's own PostgreSQL there.
- **Docker Desktop degrades under a full VM and does not say so usefully.** With the VM out of space
  every container start failed with `read init-p: connection reset by peer` or
  `no space left on device: /var/run/desktop-containerd/…` — including the verification sandbox,
  which the product correctly reported as `INCONCLUSIVE` rather than `FAILED`.
  `docker system prune -af --volumes` and then `wsl --shutdown` cleared it. Recorded because the
  first symptom looks like a sandbox provider defect.
- **Two integration tests fail on this machine for reasons that are the machine's.** Both were
  reproduced on `origin/main` at f0c604f before being blamed on anything.
  `sandbox.TestSandboxLifecycle` connects to its sandbox as `localhost`, which resolves to both
  `127.0.0.1` and `::1`; the sandbox refuses TLS on its mapped port, and the fallback attempt reaches
  the PostgreSQL this machine has installed on `::1:5432` and fails authentication against it.
  `plugins/postgres`'s `TestDiscoverOnUnreachableInstanceFails` dials 192.0.2.1 expecting
  `CONNECTION_FAILED` and gets something pgx reads as `AUTHENTICATION_FAILED` on this network.
  Neither reproduces in CI.
