# Writing an engine plugin

Adding an engine to Fleetward means writing a plugin. It never means modifying core. If you find
yourself needing to change something under `internal/controlplane/` to make your engine work, stop
and open an issue — you have found a gap in the contract, and patching around it in core is how a
plugin system quietly stops being one.

This guide walks through building one from scratch.

---

## 1. What a plugin is

A separate binary that speaks the `EnginePlugin` gRPC service defined in
[`api/proto/fleetward/v1/plugin.proto`](../../api/proto/fleetward/v1/plugin.proto). The control
plane launches it, supervises it, and restarts it if it dies (ADR-0003).

The binary must be named `fleetward-plugin-<engine type>` and placed in the control plane's plugin
directory. The manager derives the engine type from that filename and checks it against what your
plugin declares; a mismatch is refused at handshake, because routing an instance to the wrong
engine is not a failure anyone should have to debug at runtime.

---

## 2. The two rules

**Rule one: core branches on capabilities, never on your engine's name.**

If core needed to know that an instance is Cassandra to behave correctly, the plugin system would
be decoration. Everything core needs must be expressible in `Capabilities`. When something is not,
that is a contract change — propose it, do not work around it.

**Rule two: declare capabilities honestly.**

Core trusts the capability matrix when deciding what it is safe to do to someone's production
database. Setting `supports_pitr` before point-in-time recovery works is the one defect that will
get a plugin rejected outright, because the failure it produces arrives during a recovery, which is
the worst possible moment to discover a lie.

Turn a flag on in the same change that implements the behavior behind it.

---

## 3. Scaffold

```
plugins/mynewengine/plugin.go      # the implementation
cmd/plugins/mynewengine/main.go    # three lines
```

`main.go`:

```go
package main

import (
    "github.com/danmorcov88/fleetward/internal/plugin/sdk"
    "github.com/danmorcov88/fleetward/plugins/mynewengine"
)

func main() {
    sdk.Serve(mynewengine.New())
}
```

Keep logic out of `main` so the conformance suite can exercise your engine without spawning a
process.

`plugin.go`:

```go
package mynewengine

const EngineType = "mynewengine"

type Plugin struct {
    sdk.Base // supplies "not supported" for everything you have not written yet
}

func New() *Plugin {
    return &Plugin{Base: sdk.Base{EngineType: EngineType}}
}

func (p *Plugin) Capabilities(context.Context) (*fwv1.Capabilities, error) {
    return &fwv1.Capabilities{
        EngineType:        EngineType,
        EngineDisplayName: "My New Engine",
        PluginVersion:     version.Version,
        ContractVersion:   version.ContractVersion,
    }, nil
}
```

That is a complete, valid plugin. It handshakes, reports its identity, and declines everything
else with a typed `ERROR_CODE_UNSUPPORTED`. Add `HealthCheck` and it passes the capabilities and
health subset of conformance.

Embedding `sdk.Base` is what makes incremental development possible: the plugin satisfies the
contract at every point in its construction, not only at the end.

---

## 4. Build order

Implement in this order. Each step is independently useful and independently testable.

1. **`Capabilities`** — identity only, everything false.
2. **`HealthCheck`** — connect, ping, report. Return `HEALTH_STATE_DOWN` for an unreachable
   instance rather than a gRPC error: being down is a valid answer, not an RPC failure.
3. **`Discover`** — version, topology, databases. Turn on `supports_schema_discovery` and
   `supports_replication` if they apply.
4. **`CollectMetrics`** — see §5.
5. **`GetConfig`** — normalized keys plus the raw form. Redact sensitive settings rather than
   omitting them, so the UI can show that a setting exists without exposing it.
6. **`ListPrincipals`** — read-only, always. Fleetward surfaces access; it does not administer it.
7. **`Backup`** — see §6.
8. **`Restore`** and **`VerifyRestore`** — see §7. This is the part that matters most.
9. **`ListPITRTargets`** — only if your engine genuinely supports it.

---

## 5. Metrics

Metric names follow **OpenTelemetry database semantic conventions** — `db.client.connection.count`,
not `myengine_conns` (ADR-0011).

This is not bureaucracy. It is the normalization layer: it is what lets the UI chart connection
counts across PostgreSQL, MySQL, MongoDB, and Redis on one axis without core knowing which engine
produced any given series. If you invent your own names, either core grows engine-specific
knowledge or your metrics are invisible.

Map your engine's native names to semconv, and declare everything you emit in
`Capabilities.metrics` so core can register them without a round trip.

---

## 6. Backup

**Orchestrate native tooling. Do not implement a backup format.**

Your engine's maintainers have spent years on `pg_basebackup`, `xtrabackup`, and `mongodump`.
Fleetward's value is in scheduling, verifying, and reporting on those tools, not in replacing them.
Declare what you shell out to in `required_tools`, and the manager will report a missing binary as
a plugin problem at startup rather than letting a scheduled backup discover it at 3am.

Two things every backup implementation must get right:

**Terminal messages.** The last message you emit must carry either a `BackupResult` with phase
`JOB_PHASE_COMPLETED` or a `PluginError` with `JOB_PHASE_FAILED`. Returning without a terminal
message leaves core unable to tell success from a crashed stream. Conformance checks this.

**The manifest.** Capture a `SourceManifest` at backup time: databases, objects, and record counts.
This is what verification compares the restored instance against. Without it, "verification" can
only establish that a server started, which is not verification — and shipping that under the same
green checkmark as a real check is worse than shipping no check at all.

---

## 7. Restore and verification

This is the product. Correctness here outranks everything else in your plugin.

`Restore` targets either a sandbox (a throwaway container core provisioned, always safe to
overwrite) or a real instance (destructive; core handles authorization and confirmation). Read
`RestoreTarget.kind` and never assume.

`VerifyRestore` runs against a freshly restored instance and reports per-check results:

- `CONNECTIVITY` — it accepts connections and reports a sane version.
- `RECORD_COUNTS` — counts match the manifest. Report each mismatch as a `Discrepancy`, with
  expected and actual, rather than collapsing everything into one boolean.
- `SCHEMA_PRESENCE` — everything named in the manifest exists.
- `INTEGRITY` — engine-specific: `amcheck`, `CHECK TABLE`, `dbHash`.
- `QUERYABILITY` — representative reads succeed.

Use `VERIFICATION_STATUS_INCONCLUSIVE` when verification could not run — a sandbox that never
became ready, a tool that was missing. Do not report it as `FAILED`. The distinction is the
difference between "your backup is bad" and "our infrastructure hiccuped", and blurring it trains
operators to ignore the alert that matters most.

To support sandbox verification, declare a `SandboxTemplate`: image repository, tag template,
environment, port, readiness command. Core provisions containers from what you declare, which is
precisely why it does not need a lookup table of engines.

Four fields exist for images that will not simply take what core generates. Each of them was added
by an engine that needed it, and each is a declaration rather than an exception:

| Field | Declare it when |
|---|---|
| `fixed_username` | The image creates one administrative account and cannot be told to rename it. Core uses that name and still generates the password. |
| `password_policy` | The image validates the password it is handed. Core then satisfies the policy by construction, rather than generating one the image will reject at startup — which is a failure that arrives rarely and at the worst moment. |
| `shared_directory` | Your engine reads and writes backup files on its own filesystem. Core mounts a directory there that your plugin can also reach, and reports both of its names in the sandbox's credentials. |
| `min_disk_bytes` | The image needs more room than a default container gets. |

### If your engine hands over a file rather than a stream

`BACKUP DATABASE … TO DISK`, RMAN, and `nodetool snapshot` all write a file on the database server
rather than to stdout, where your plugin has no access to it. The contract's answer is a directory
both of them can see (ADR-0026):

- declare `requires_shared_directory` on the method, so core refuses to schedule against an instance
  that has none and says what is missing;
- read `Credentials.shared_directory`, which carries `engine_path` — the path to put in the
  statements you send — and `local_path`, the same directory as your own process sees it;
- **create the artifact file yourself before asking the engine to write it.** Several engines create
  their backup file owned by their own user and unreadable by anyone else, and ignore the umask.
  Opening an existing file preserves that file's owner and mode, so a file you created stays yours;
- delete it on every path out, including failure. An artifact left on a share is a full copy of a
  database that nothing will come back for.

The restore path is the same protocol in reverse: write the artifact into `local_path`, hash it in
full before you touch the target, then restore from `engine_path`.

---

## 7b. Backups your plugin did not take

Most estates already back themselves up, by cron or by scripts that predate Fleetward, and reporting
on those without changing anything is what makes Fleetward adoptable at all
([ADR-0015](../adr/0015-observed-and-managed-backups.md)). `ListBackupHistory` is how a plugin
supplies that, and it is strictly read-only: read whatever evidence the engine keeps, report it, and
touch nothing.

What counts as evidence differs enormously by engine, so **what your source can prove is a
declaration rather than an assumption**:

```go
BackupHistory: &fwv1.BackupHistoryCapabilities{
	Supported:                true,
	SourceDescription:        "myengine's own record of its backups",
	IdentityIsEngineAssigned: true,   // the engine names the backup; a moved file is still one backup
	ReportsOutcome:           true,   // the evidence distinguishes success from failure
	RequiresSharedDirectory:  false,  // observation reads a directory rather than the connection
},
```

Core acts on every one of those generically. Declaring `ReportsOutcome` for a source that cannot
prove one is the same defect as declaring a capability you have not implemented, and it is worse in
its consequence: it puts a green tick on somebody's estate that nothing stands behind.

Four rules, and each of them is a conformance assertion.

**Every record carries an `external_id` that is stable across polls and unique within the instance,
and comes from the engine rather than from the moment you observed it.** A poll runs every half hour
for years against unchanged evidence; core upserts on this identity, so an unstable one records the
same nightly backup forty-eight times a day
([ADR-0027](../adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md)). If your
source assigns nothing, derive one from what is stable about it and set
`IdentityIsEngineAssigned = false`.

**If your engine records its own backups, report the identity of the one you just took** in
`BackupResult.external_id`. Fleetward's backups appear in that record like everyone else's, and this
is what makes the next poll land on the row that already exists rather than inserting the same
backup a second time under an origin it does not have.

**Timestamps are UTC.** `google.protobuf.Timestamp` has no other meaning. An engine that records
local time with no offset is yours to convert — and where you cannot convert exactly, set
`FinishedAtIsApproximate` rather than guessing. Core widens the compliance window for a record that
carries it, and reports the caveat.

**Honour `Limit` and `Since`.** An engine's backup history on an instance that has been up for years
holds hundreds of thousands of rows, and a full scan on every poll is not acceptable against a
production server. Core supplies a watermark and expects a bounded page.

If your engine keeps no record at all — PostgreSQL does not — a configured directory of files is a
legitimate source, and its honest ceiling is `OBSERVED_OUTCOME_UNKNOWN`: a file arrived, and nothing
about it says the dump that wrote it finished. Declare `ReportsOutcome: false` and let Fleetward say
so. A plugin that can see nothing declares `Supported: false` and refuses the RPC with
`ERROR_CODE_UNSUPPORTED`; answering with an empty list would report an engine nobody is watching as
an engine with nothing to report.

## 8. Errors

Return `sdk.Error` values, not bare errors:

```go
return sdk.ConnectionFailed("connect to %s:%d", host, port).WithCause(err)
return sdk.ToolNotFound("pg_basebackup")
return sdk.Unsupported("point-in-time recovery requires WAL archiving to be enabled")
```

Core classifies failures from the structured code — is this retryable? is a tool missing? were the
credentials wrong? — without parsing message strings, which is exactly the coupling a plugin
contract exists to prevent.

`ConnectionFailed` is retryable; `AuthenticationFailed` deliberately is not, because the same wrong
password stays wrong and retrying can trigger account lockout on the monitored instance.

**Never put a credential in a message, a detail, or a log line.** A wrapped driver error can
contain a full connection string; `WithCause` keeps it local, and only the message crosses the
process boundary.

---

## 9. Credentials

Credentials arrive per-request in `Credentials`. Do not write them to disk, do not cache them, do
not log them, and do not retain them past the call that carried them. Core resolves them through
the `SecretsProvider` for exactly one RPC (ADR-0009).

For artifacts, you receive **presigned URLs**, never storage credentials. Treat a presigned URL as
a bearer credential in its own right: anyone holding it can read or overwrite that object until it
expires, so it must not reach a log line either.

---

## 10. Conformance

```bash
make conformance
```

The suite in [`test/conformance`](../../test/conformance) spins your real engine and drives the
contract against it. It reads your capability matrix and skips what you do not claim to support, so
it is useful from your very first commit.

**Conformance passing is the merge gate.** It is also the reason a reviewer can trust a plugin for
an engine they have never operated: "does it pass conformance?" replaces reading a thousand lines
of engine-specific code and hoping.

If the suite needs to change to accommodate your engine, that is a signal worth taking seriously.
Nine times out of ten it means the contract has leaked an assumption from an earlier engine, and
the fix belongs in the contract, not in the test.

### What the suite runs

**Stage 0 — every plugin, always.** The binary launches and handshakes, the capability matrix is
coherent and stable across calls, `GetCapabilities` needs no connection, an unimplemented RPC is
refused with a typed error rather than a hang, and an engine without PITR answers with an
unavailable window instead of failing the call.

**Stage 1 — every plugin that declares `supports_sandbox_restore` and a backup method.** The whole
loop, driven the way the control plane drives it: your engine is stood up from your own
`SandboxTemplate`, seeded, backed up through your `Backup` into a real S3-compatible store via
presigned part grants, restored into a second container, and compared against the manifest your
backup captured.

Four of the Stage 1 cases are failures rather than successes, because a verification that has only
ever been shown to pass is indistinguishable from one that always passes:

| Case | Your plugin must answer |
|---|---|
| A healthy artifact | `VERIFIED`, with every check you declare actually run |
| A truncated artifact | a failed restore carrying `sdk.ArtifactCorrupt` |
| An artifact with a byte flipped mid-stream | the same — nothing but a checksum can catch this one |
| A restore compared against a manifest that no longer describes it | `FAILED`, with a `Discrepancy` naming the object and both counts |
| A restore target that never answers | anything **except** `sdk.ArtifactCorrupt` or `ERROR_CODE_TOOL_FAILED` |
| A missing or empty manifest | `INCONCLUSIVE`, never `VERIFIED` |

The artifacts are corrupted in the bucket, not through your code, because that is what bit rot and
a half-finished upload look like.

The last two rows are where plugins fail this suite. Core reads a tool failure as evidence that the
artifact could not be loaded, and reports it as a failed verification (ADR-0022) — so if your
restore tool's "connection refused" reaches core as `ERROR_CODE_TOOL_FAILED`, a sandbox that lost a
race fires this product's one critical alert. Check the target answers before you start the tool,
and classify a lost connection in its output as `sdk.ConnectionFailed`.

### Getting your engine into Stage 1

Stage 1 needs one thing the contract deliberately cannot give it: rows in a database. Fleetward
never writes to a monitored instance, so no RPC could create a table — and a backup of an empty
database proves nothing, because a comparison over zero objects succeeds trivially.

So register a `Fixture` for your engine in
[`test/conformance/fixtures_test.go`](../../test/conformance/fixtures_test.go), keyed by your
engine type. It has two methods: `Seed` puts several objects with different row counts into a fresh
instance, and `RemoveRows` deletes rows from exactly one of them and reports which — by the name
your manifest uses for it — and how many. Look at `fixture_postgresql_test.go`; it is small, and
fixtures are the only engine-specific code in the suite.

A plugin that declares `BackupHistory.Supported` also implements `ExternalBackuper` on its fixture,
which causes a backup by the engine's own means — the way a cron job would — so the suite has
something to observe that Fleetward did not take.

Everything else is shared. Adding an engine means adding a fixture beside your plugin; it never
means changing an assertion. Until you add one, the Stage 1 cases skip with a message saying so,
and Stage 0 still runs.

Two host requirements: a container runtime, and whatever native tools you declared in
`required_tools` on `PATH`. Both missing produce a skip rather than a failure — but a skipped merge
gate is not a merge gate, so CI installs them.

---

## 11. Checklist

- [ ] Binary named `fleetward-plugin-<engine type>`, matching the declared `engine_type`
- [ ] `Capabilities` honest — no flag set ahead of its implementation
- [ ] Metric names follow OTel semantic conventions
- [ ] `required_tools` declares every native binary you invoke
- [ ] Backup emits a terminal message on every path, including failure
- [ ] Backup captures a `SourceManifest`
- [ ] `VerifyRestore` distinguishes `FAILED` from `INCONCLUSIVE`
- [ ] Errors are `sdk.Error` values with the right codes
- [ ] A restore target that does not answer is never reported as a tool failure
- [ ] No credentials in logs, errors, or on disk
- [ ] A `Fixture` is registered so the suite can exercise Stage 1
- [ ] `BackupHistoryCapabilities` describes what your evidence can and cannot prove
- [ ] Observed records carry a stable `external_id` and UTC timestamps
- [ ] Conformance passes
- [ ] An ADR for any decision a future maintainer might otherwise undo
