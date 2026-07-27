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

---

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

The suite in [`test/conformance`](../../test/conformance) spins your real engine with
testcontainers and drives the whole contract: discover → metrics → backup → restore-to-sandbox →
verify → principals. It reads your capability matrix and skips what you do not claim to support,
so it is useful from your very first commit.

**Conformance passing is the merge gate.** It is also the reason a reviewer can trust a plugin for
an engine they have never operated: "does it pass conformance?" replaces reading a thousand lines
of engine-specific code and hoping.

If the suite needs to change to accommodate your engine, that is a signal worth taking seriously.
Nine times out of ten it means the contract has leaked an assumption from an earlier engine, and
the fix belongs in the contract, not in the test.

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
- [ ] No credentials in logs, errors, or on disk
- [ ] Conformance passes
- [ ] An ADR for any decision a future maintainer might otherwise undo
