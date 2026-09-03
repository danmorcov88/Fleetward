# Roadmap

This is the only roadmap. It used to exist in four places — `CLAUDE.md` §6, `docs/dev/STATUS.md`,
`README.md`, and `docs/dev/slices/README.md` — which had already drifted apart from one another.
Those now link here.

For where the work actually stands today, see [`dev/STATUS.md`](dev/STATUS.md). For why things are
built the way they are, see the [engineering journal](dev/journal/README.md).

## How the work is cut

Work is cut into **slices**, not stages. Each slice is independently demoable, carries its own
operational surface ([ADR-0024](adr/0024-production-readiness-is-a-slice-property.md)), and ends
with `STATUS.md` rewritten and a journal entry added. This matters more than speed: development is
sporadic, so a session must be able to start without reconstructing context and finish leaving the
tree green.

Slices are ordered by when they start paying back, not by architectural tidiness. Each has a
self-contained brief in [`dev/slices/`](dev/slices/), written when the slice starts.

## Phase A — prove the loop (PostgreSQL) ✅

The thinnest path through the entire product, using the simplest possible backup method. The point
was to prove the verification loop early, because it is simultaneously the differentiator and the
riskiest piece.

| Slice | Content | Journal |
|---|---|---|
| A1 | PostgreSQL plugin: real `HealthCheck` and `Discover` | [entry](dev/journal/A1-health-and-discover.md) |
| A2 | `inventory` service, credential storage, instance CLI | [entry](dev/journal/A2-inventory-and-cli.md) |
| A3 | `SandboxProvider` (Docker), teardown guaranteed on every path | [entry](dev/journal/A3-sandbox-provider.md) |
| A4 | Backup via `pg_dump` + `SourceManifest` + multipart upload | [entry](dev/journal/A4-backup-and-manifest.md) |
| A5 | `Restore` into a sandbox + `VerifyRestore` | [entry](dev/journal/A5-restore-and-verify.md) |
| A6 | A corrupted artifact returns `FAILED`; the conformance suite covers the path | [entry](dev/journal/A6-verification-fails-loudly.md) |

`pg_dump` before `pg_basebackup` was deliberate: a logical dump yields exact row counts trivially
and restores into any empty database, while a physical backup restores a whole cluster and needs a
version-exact image plus recovery configuration. Both will exist — the contract already supports
several methods per engine. Starting with the physical one would have meant debugging two hard
things at once.

## Phase B — from a proven loop to an installed tool

The goal of this phase is a single sentence: **Fleetward runs on a server inside a company, watches
an estate it was pointed at, and is trusted with the answer.** Every slice below is on the path to
that sentence; anything that is not has been moved behind it.

| Slice | Content | What it unblocks |
|---|---|---|
| B1 | Scheduler: cron over `schedules`, job claim by lease, heartbeat, recovery after a crash | Nothing runs automatically today. Without this there is nothing to install. |
| B2 | SQL Server plugin, passing the conformance suite unmodified | The architecture's central claim, until then untested |
| B3 | Observed backups: `ListBackupHistory`, `backups.origin`, schedule adherence, PostgreSQL and SQL Server | Reporting on backups taken by existing cron and scripts, changing nothing |
| B4 | Estate Overview screen + an API client generated from the OpenAPI document | Fifty servers at a glance; the CLI is a poor surface for that |
| B5 | Retention and expiry | Untracked artifact growth on a company bucket |
| B6 | Authorization spine: principal, RBAC over `role_grants`, tenancy, audit log | Every route is currently open to anyone who can reach the port |
| B7 | Alert rules and delivery (webhook, SMTP) | The difference between a dashboard and monitoring |
| B8 | Self-observability: `/metrics` and spans on four operations | "How do I monitor the thing that monitors my backups" |
| B9 | Production deployment artifact, signed release, `v0.1.0` | There is no tag and no published image today |

**Exit: a pilot installation.** After B9 the remaining work is engines and the identity provider.

| Slice | Content |
|---|---|
| B10 | OIDC, swapped in behind the authorization spine B6 built |
| B11 | MySQL / MariaDB plugin |
| B12 | Oracle plugin |
| B13 | MongoDB plugin |
| B14 | ClickHouse plugin |
| B15 | Cassandra plugin |
| B16 | Redis plugin — the manifest's stress test |

### Why SQL Server is the second engine, and why it is second

The claim that adding an engine never means modifying core appears in `CLAUDE.md`, in the README, in
[ADR-0003](adr/0003-hashicorp-go-plugin.md), and in the design of the whole conformance suite. Until
B2 it had never been tested; until a second engine passed the suite unmodified it was a design intention,
not a result.

Doing it now is also the cheapest it will ever be. `buf breaking` is enforced, there is no tag, no
release, and no external consumer, so a contract change today costs a regenerate. After `v0.1.0`
publishes the contract as a public interface, it costs a deprecation cycle.

SQL Server specifically, rather than the easier MySQL, because it asks the contract a question
PostgreSQL never did. `pg_dump` writes to stdout, which the plugin streams into presigned part
grants. `BACKUP DATABASE … TO DISK` writes a file on the *database server's* filesystem, where the
plugin has no access. That was a genuine architectural decision and it was made deliberately, as
[ADR-0026](adr/0026-a-shared-directory-carries-a-file-based-artifact.md): a directory the engine and
the plugin can both see, chosen over `BACKUP TO URL` — which would have put
[ADR-0007](adr/0007-s3-object-storage-for-artifacts.md)'s rule that plugins never receive storage
credentials under real pressure — and over a co-located plugin, which is the same design with both
paths equal and is what a future agent will use.

The smaller thing that was already visible turned out to be real too: the
`mcr.microsoft.com/mssql/server` image refuses to start unless `MSSQL_SA_PASSWORD` satisfies a
complexity policy, and core's generated password misses it about once in eight hundred. The
capability matrix surfaced it, as intended, and it is now a declared `password_policy` rather than a
special case in core.

The bill was four additive contract fields and one session. What kept it bounded was the scope fence
in the brief: one backup method, one transport, no `xp_cmdshell`.

**B2 before B3** for a reason beyond ordering. SQL Server records every backup taken on an instance
in `msdb.dbo.backupset` — by far the richest observable backup history of any engine, and what a DBA
actually reads. PostgreSQL has the poorest: `pg_stat_archiver` describes WAL, not files someone
wrote to a directory. Designing `ListBackupHistory` with both the richest and the poorest source in
hand is how it avoids being a PostgreSQL-shaped RPC wearing a generic name.

### What "real-time monitoring" means here

Stated plainly so that expectations are set: **the answer to "does anything need my attention right
now" is never more than about thirty seconds stale, and finding out does not require looking at the
screen.** It does not mean streaming telemetry.

Three components deliver that: scheduled health probes (a `discovery` job kind, which
`schedules.kind` already permits, driving the existing `TestConnection`); a dashboard that refetches
on an interval; and alert delivery, which is the part that makes it monitoring rather than a
dashboard.

Deliberately not SSE or WebSockets. Fifty rows on a thirty-second cadence is a polling problem, and
a streaming transport would also collide with
[ADR-0019](adr/0019-rest-api-without-a-grpc-listener.md), under which server-streaming RPCs cannot
be served over the gateway.

## Engines

Eight engines, in the order the estate needs them. The list, what each one is planned to use, and
what is implemented today are in [`engines.md`](engines.md) — that page is the single source, and
the slice numbers above are the schedule for it.

Each engine runs the same conformance suite, unmodified. If an engine requires changing the suite,
that is a contract leak and the fix belongs in the contract rather than in the test.

Redis is placed last on purpose, and the placement is a claim rather than a convenience. Redis has
no tables, so "per-table row counts" as a manifest concept has to generalize into "named objects
with counts" — which is precisely the abstraction the architecture says it has. Adding three more
relational engines proves less than adding one key-value store and finding the abstraction held.

## Deferred, deliberately

Named here rather than left to be discovered as slippage.

- **Access compliance** — who has access, whose account expires, who is non-compliant. The contract
  already carries most of what it needs: `Principal` has `password_expires_at`, `last_login_at`,
  `is_superuser`, `can_login`, and `privileges`; only `created_at` is missing. Remediation SQL is
  generated and never executed ([ADR-0017](adr/0017-access-compliance-read-only.md)). Deferred
  behind the engines: a good product idea that is not on the path to a trusted installation.
- **Structural drift** — a normalized structural fingerprint per instance, diffed over time
  ([ADR-0016](adr/0016-schema-drift-snapshots.md)). Same reasoning.
- **A query editor** — moved from non-goal to eventual work by
  [ADR-0018](adr/0018-query-editor-on-the-roadmap.md), gated on server-side RBAC, an audit record
  per execution, a read/write distinction, and typed confirmation against production. A tool holding
  credentials for fifty production databases *and* executing arbitrary SQL has a materially larger
  blast radius than a monitoring tool.
- **Database performance metric collection.** The plugin contract carries `CollectMetrics` and
  VictoriaMetrics is wired and health-checked, but nothing collects yet. Performance monitoring was
  never the pain this product exists to solve, and that need is already met by existing tooling.
  Fleetward's own `/metrics` (B8) is a different thing and is not deferred: an operator asked to
  install this will ask how to monitor it, and the answer cannot be that they cannot.
- **Kubernetes** — the `SandboxProvider` interface anticipates a Jobs-based implementation, and
  `internal/controlplane/sandbox` refuses `kubernetes` with "not implemented yet". A Helm chart
  waits on that provider, because a chart that deploys a control plane unable to verify a backup
  would be a half-truth.
