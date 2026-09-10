# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice B8 is complete. Next is B9 — the production deployment artifact and `v0.1.0`.**

OpenTelemetry had been wired in `internal/telemetry/otel.go` since the foundation slice with zero
call sites: no span was started, no meter obtained, and there was no `/metrics`. `GET /metrics` now
serves Fleetward's own health in the Prometheus exposition format, four operations carry a span, and
the development stack's VictoriaMetrics — health-checked on every start for eight slices and holding
nothing — scrapes it with a credential.

**The metric that matters is `fleetward_alert_evaluation_duration_seconds_count`.** An evaluation
pass writes no job row ([ADR-0038](../adr/0038-alert-evaluation-is-a-pass-over-the-estate.md)), so
until now the only record that Fleetward had looked at the estate was a log line. If that counter
stops advancing, nothing is being detected and no alert will fire — and the estate looks exactly as
healthy as it did the moment evaluation stopped.

| The thing | What holds it |
|---|---|
| a label keyed on `backup_id`, which is one time series per backup forever | a rule with a test that reads what is *emitted* rather than a list of constants, so a new recorder cannot smuggle one in ([ADR-0041](../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md)) |
| a database password in a label, on an exporter that ships elsewhere | no function in `internal/telemetry` takes a request, a connection, or an error — the audit package's rule, and `runRequest.connection` carries `Credentials` |
| `/api/v1/backups/<uuid>/verifications` as an `http.route` value | the label is the mux *pattern*, from `mux.Handler(r)`; `r.Pattern` is empty in middleware that wraps the mux |
| every backup landing in the `+Inf` bucket | OTel's defaults stop at 10 seconds; explicit boundaries reach four hours, and two tests refuse a regression |
| a panic on the first backup of a production install | no instrument is ever stored `nil`; a creation failure yields a working no-op and a log line |
| a stranger reading the shape of an estate | a scrape names no scope, so it needs a tenant-wide `viewer` — the rule [ADR-0035](../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md) already had ([ADR-0042](../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md)) |
| the development stack quietly bypassing that rule | it presents the bootstrap token, for the reason `docker-compose.yml` already gives about authorization being on |
| telemetry changing behaviour when it is off | the global providers are OTel's no-ops, and a test calls every recorder with no provider installed |

Two ADRs: [ADR-0041](../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md) — what a Fleetward
metric is allowed to carry, covering the namespace, the cardinality rule and the no-struct rule; and
[ADR-0042](../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md) — scraping is a
question about the whole estate.

The slice also found that the resource merge in `telemetry.Setup` had never run and was broken —
`conflicting Schema URL` on the first start after the endpoint was turned on by default. Written in
the foundation slice, never executed, because `Setup` returned before reaching it whenever telemetry
was disabled. Which was always. See the [journal](journal/B8-self-observability.md).

The operational surface is `GET /metrics` and the page is
[`../ops/observability.md`](../ops/observability.md) — whose last section is what these metrics will
*not* tell you, and whose first is that they answer whether Fleetward is working rather than whether
your backups are good.

There is no demo act. `/metrics` is an operator's concern rather than a beat in the story the demo
tells a DBA.

## What comes next, and why that order

**B9 — the production deployment artifact, a signed release, `v0.1.0`.** Nothing has been released:
no tag, no published container image, no signed artifact — `release.yml` installs cosign and never
invokes it, and `docker-compose.yml` is a development configuration by its own declaration.

Every slice from B1 has been building something an operator could install, and none of them has
produced anything an operator can install. B8 was the last piece of the answer to "how do I run
this" that was missing; what is left is the artifact itself.

Session protocol: [`slices/README.md`](slices/README.md). B9's brief is not written yet; briefs are
written when the slice starts.

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1–B8 done, B9 next |
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
- **`go run ./tools/demo` does not rebuild the image; `-build` does.** Off by default locally and on
  in CI, which is the right default for a demo run repeatedly against an unchanged tree — and the
  wrong one immediately after changing the control plane, where it silently runs the previous binary
  and the act that exercises the change fails for no visible reason. Worth twenty minutes to anybody
  who forgets, so: after touching `cmd/` or `internal/`, run `go run ./tools/demo -build`.
- **Act 7's webhook needs the control-plane container to reach a listener on the host**, through the
  `host.docker.internal:host-gateway` entry `docker-compose.yml` gives the `fleetward` service. Where
  a firewall or an unusual daemon refuses that route, act 7 says so and shows the rest; the delivery
  path itself is asserted with no container networking at all in
  `internal/controlplane/alerts/alerts_integration_test.go`. The act distinguishes the two with a
  `TestNotifier` pre-flight before act 4, so "this machine cannot" never reads as "the product did
  not".
- **A notification can be lost, and the absence of one is not evidence that nothing is wrong.**
  Delivery is at-most-once: a bounded in-process queue, a small bounded retry, and then the
  notification is logged and dropped. There is no outbox table, no backoff schedule and no
  dead-letter queue ([ADR-0039](../adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)).
  The alert row survives all of it, and three things make the trade visible rather than hidden:
  `notifiers.last_attempt_at`, `last_success_at` and `last_error`; `alert notifier test`, which
  sends a real message; and `docs/ops/alerting.md` saying so in its own section. Since B8 there is a
  fourth: `fleetward_notifications_total{fleetward_outcome="dropped"}`. A control plane restarted
  with work in its queue loses it.
- **An `inconclusive` verification produces no alert.** Deliberate, and the strongest form of
  [ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md): a sandbox that never
  started is not evidence that a backup is bad, and routing it through the same alert as a
  proven-bad artifact is how the alert that matters gets muted
  ([ADR-0040](../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md)).
  The cost is real: a sandbox provider broken all week means verification is not happening at all
  and nothing pages anybody. The estate view shows the verdicts and readiness reports the provider
  as degraded; neither is a page. The fix is a separate rule kind at a lower severity, never a
  widened predicate, and three tests refuse the widening.
- **Three rule kinds are declared and not evaluated.** `storage_threshold`, `replication_lag` and
  `custom_promql` are in `alert_rules.kind`'s CHECK because they are where alerting is going, and
  all three want database metrics that nothing collects. Creating a rule of one of them is refused
  rather than stored: a rule accepted and never evaluated is worse than one refused.
- **Alert evaluation leaves no job row, so `job list` cannot answer "did it run last night".** The
  same consequence retention has and for the same reason
  ([ADR-0038](../adr/0038-alert-evaluation-is-a-pass-over-the-estate.md)). Since B8,
  `fleetward_alert_evaluation_duration_seconds_count` answers it, and it is the series worth
  alerting on. The retention sweep still has no equivalent.
- **A webhook URL that embeds its own credential is readable by an administrator.** Slack and Teams
  build the token into the path, and that path lives in `notifiers.settings`, which `ListNotifiers`
  returns. The notifier's *own* secret never appears there and a credential-shaped settings key is
  refused; the vendor URL is the case that check cannot catch. Notifier management is `admin`-only
  partly for this reason, and `docs/ops/alerting.md` states it.
- **Silencing is disabling a rule, and nothing else.** No maintenance windows, no per-alert snoozes,
  no grouping, no inhibition. Disabling resolves the rule's open alerts on the next pass, which is
  the honest version of "stop telling me".
- **There is no alerts screen.** `web/src/components/AppShell.tsx` keeps `enabled: false` on
  `/alerts`. The API and the CLI are the whole surface.
- **Fleetward's own metrics do not tell you whether your backups are good.** They tell you whether
  Fleetward is working. The estate view, `backup adherence` and the alerts are the other answer, and
  they are computed from rows rather than counters for a reason: a backup that failed at 02:00 is a
  row that still says so at 09:00, while a counter that stopped increasing looks identical to an
  estate with nothing to do.
- **Nothing collects performance metrics from the databases Fleetward watches.** `CollectMetrics` is
  in the plugin contract and nothing calls it; `db.client.*` is empty and `/metrics` never carries
  it. Deferred deliberately rather than merely unbuilt — performance monitoring was never the pain
  this product exists to solve — and it is what the three unevaluated alert rule kinds are waiting
  on.
- **Four operations carry a span, and not five.** An API request, a backup run, a verification and
  the alert evaluation pass. Not the scheduler tick — `/readyz` degrades with a reason when the loop
  stalls, which is the signal to alert on — and not the retention sweep, a plugin RPC, the object
  store or the sandbox provider. There are also no sub-spans inside a backup, so "was it the dump or
  the upload" is not answerable.
- **`/metrics` is on the main listener and there is no shipped dashboard.** No separate admin port,
  no Grafana dashboard, no recording rules and no alerting-rules file; the PromQL in
  [`../ops/observability.md`](../ops/observability.md) is what there is. A scrape needs a tenant-wide
  `viewer` ([ADR-0042](../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md)), or
  `FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=false`, which is warned about on every start.
- **A metric is per control plane, and the notification queue's depth is invisible.** Two replicas
  produce two sets of series, so estate-wide queries need a `sum`; and you can see what the delivery
  queue dropped, never how close it came to dropping.
- **`internal/storage/tsdb` labels samples `fw_instance_id` while `/metrics` renders
  `fleetward_instance_id`.** Nothing has ever written through the `fw_*` path, so there is no data to
  migrate; ADR-0041 records that it adopts the longer spelling when database metric collection
  lands, so nobody invents a third.
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
- **Any image occasionally fails to build here** with `failed to prepare extraction snapshot … parent
  snapshot does not exist`, at the *export* step, after every layer has been built successfully. It
  is a Docker Desktop containerd-snapshotter fault rather than a Dockerfile one. `docker builder
  prune -af` clears it; on the `web` image, `docker compose up --build <the other services>` is
  another way round. It was first seen on `web` and this entry used to name only that one — B7 hit
  it on `fleetward` mid-demo, so it is not image-specific. Easy to mistake for a broken build of
  whatever it lands on.
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
- **`make vuln` reports stdlib vulnerabilities here that CI does not have.** The findings are real
  and they are about the *toolchain*, not the code: this machine's Go is 1.25.6, CI resolves
  `go-version: 1.25` to the newest patch, and every one of the twenty-three findings names a fix in
  1.25.7 or later. Upgrade Go before believing a stdlib finding that CI is not also reporting.
  `govulncheck` itself is pinned to v1.7.0 in both `ci.yml` and the Makefile — v1.8.0 raised its own
  minimum to Go 1.26 and turned this job red on a commit that changed nothing.
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
