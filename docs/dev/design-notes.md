# Design notes

A selection from the [engineering journal](journal/README.md) — the decisions with the longest
reach, grouped by the idea behind them. Each links to the entry it came from, where the full
reasoning and the alternatives live.

This page exists because the journal is chronological and the useful pattern is not. Someone
deciding how a new slice should behave wants the rule, not the week it was discovered.

---

## Credentials travel no further than they must

**The connection config is built field by field, never as a DSN.** A connection string containing a
password ends up in error messages, logs, and stack traces, and the only reliable prevention is
never to construct one. `TestConnConfigDoesNotBuildADSN` and `TestConnectErrorsNeverLeakThePassword`
guard it. → [A1](journal/A1-health-and-discover.md)

**The CLI has no `--password` flag, on purpose.** The password comes from an environment variable or
`--password-stdin`. A password in `argv` is visible to every process on the host through `ps`, and
stays in the shell history of whoever typed it. → [A2](journal/A2-inventory-and-cli.md)

**Credentials are split, and the split is tested.** Non-secret connection fields live in
`connections`; the password and the client *private key* go to the `SecretsProvider` as one
document. The client certificate travels with its key, because a half-configured mutual-TLS
connection is a worse failure than a slightly over-protected certificate.
`TestStoredPasswordIsCiphertextEverywhere` greps the whole metadata store for the plaintext.
→ [A2](journal/A2-inventory-and-cli.md)

**Only `internal/controlplane/api` decides what a client sees.** Services return sentinel errors,
the gRPC layer maps them to status codes, and anything unclassified is logged in full and returned
as a bare internal error — because a pgx or secrets-provider failure can carry a connection string.
→ [A2](journal/A2-inventory-and-cli.md)

## A refusal is an answer, not a failure

**An unreachable instance is `HEALTH_STATE_DOWN`, not an RPC error.** "Down" is the most important
answer that RPC gives. Returning it as a failure would lose the distinction between *the database is
down* and *we could not ask*. → [A1](journal/A1-health-and-discover.md)

**A plugin without point-in-time recovery answers `available: false` with a reason, rather than
erroring.** The conformance suite asserts this for every plugin. An engine that cannot do something
and an engine that is broken must not look alike to core. → [A6](journal/A6-verification-fails-loudly.md)

**Authentication failure is deliberately not retryable.** The same wrong password stays wrong, and
retrying can trip account lockout on the monitored instance — turning a configuration mistake into
an outage. → [A1](journal/A1-health-and-discover.md)

**Missing privileges never fail discovery.** A monitoring account without `pg_read_all_settings` is
good practice, so the fields that need it are best-effort. Their absence must not turn a security
choice into a false outage. → [A1](journal/A1-health-and-discover.md)

## Say only what the evidence supports

**`FAILED` is reserved for evidence about the artifact; everything else is `INCONCLUSIVE`.** A
corrupt artifact or an engine's own tooling refusing to load it is evidence the backup is bad.
Docker being down, a sandbox that never answered, a transfer that broke — none of that says anything
about the backup, and reporting it as data loss trains people to ignore the alert that matters.
→ [A5](journal/A5-restore-and-verify.md), [ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md)

**The restore is lenient and the count comparison is strict, in that order.** A restore that emits
warnings still produces a database worth counting; the comparison is where the verdict is made.
Being strict at both steps would report a cosmetic warning as data loss.
→ [A5](journal/A5-restore-and-verify.md)

**A manifest-less backup is `INCONCLUSIVE` by construction and never reaches a sandbox.** Comparing
zero objects to zero objects succeeds trivially — a verification that cannot fail is worse than no
verification, because it produces a green checkmark. → [A5](journal/A5-restore-and-verify.md)

**A tag is never guessed, and there is no fallback to `latest`.** Verifying a backup against the
wrong engine version is worse than reporting that it could not be verified.
→ [A3](journal/A3-sandbox-provider.md)

## Core knows capabilities, never engine names

**Plugin capabilities start all false.** A capability is a promise core relies on when deciding what
to do to a production database, so each flag is turned on in the same change that implements the
behaviour behind it — never in advance. → [Foundation](journal/00-foundation.md)

**The port is required, and core has no per-engine default.** Acquiring one would be exactly the
engine knowledge the plugin contract exists to keep out of core. `Capabilities` has no
`default_port` field, and adding one is how this would change.
→ [A2](journal/A2-inventory-and-cli.md)

**Core generates the sandbox identity; the plugin's template places it.** Core renders `env`,
`command`, and `readiness_command` against `{{ .Username }}`, `{{ .Password }}`, `{{ .Database }}`,
and `{{ .Port }}`. Every sandbox gets a distinct password with a lifetime of minutes, and nothing is
compiled into a plugin binary. This was the one place engine-agnosticism was genuinely under
pressure. → [A3](journal/A3-sandbox-provider.md), [ADR-0020](../adr/0020-sandbox-credentials-from-template-placeholders.md)

## Let the database enforce what code would forget

**Two concurrent backups of one instance are prevented by a partial unique index, not by a lock in
code.** The insert raises a unique violation, which the service maps to "already running". A guard
that lives in the schema cannot be bypassed by a second code path.
→ [A4](journal/A4-backup-and-manifest.md)

**Listings use keyset pagination on `(created_at, id)`.** An estate is added to while it is being
read, and an offset would silently skip or repeat rows when that happens.
→ [A2](journal/A2-inventory-and-cli.md)

**An incomplete upload is invisible, which is stronger than deleting a bad object.** Aborting a
multipart upload leaves nothing behind; writing an object and deleting it on failure has a window in
which a truncated artifact looks like a backup.
→ [A4](journal/A4-backup-and-manifest.md), [ADR-0021](../adr/0021-plugins-upload-artifacts-as-multipart-parts.md)

## Cleanup is guaranteed, not attempted

**A sandbox is destroyed on every path, including panic — and three mechanisms overlap.** A deferred
teardown, a reaper enforcing a maximum lifetime, and a sweep at startup for orphans left by a
crashed process. The overlap is deliberate: each covers a failure the others cannot.
→ [A3](journal/A3-sandbox-provider.md)

**`TestMain` fails the run if a single labelled container survives it.** A leak that only shows up as
a slow disk fill three weeks later is the kind of bug a test suite should refuse to let merge.
→ [A6](journal/A6-verification-fails-loudly.md)

## Tests that prove rather than illustrate

**Corruption is injected in the bucket, never through the plugin.** Corruption introduced through the
code path under test proves less than it appears to, because it can only ever exercise the branch
someone remembered to write. → [A6](journal/A6-verification-fails-loudly.md)

**The flipped-byte case is the one that proves the checksum earns its cost.** A truncated artifact
fails on length alone; only a same-length artifact with one byte changed can distinguish a real
integrity check from a size comparison. → [A6](journal/A6-verification-fails-loudly.md)

**A tool that talks to databases and has only been tested against mocks has not been tested.**
Integration tests spin real engines with testcontainers, and CI installs the host tooling they need
— because a skipped merge gate is not a merge gate.
→ [ADR-0012](../adr/0012-testcontainers-and-conformance-suite.md), [A6](journal/A6-verification-fails-loudly.md)

## Things that cost an hour and would cost it again

**The published port of a container has to be polled for, not read once.** `ContainerStart`
returning does not mean the port is mapped: Docker Desktop sets its proxy up asynchronously, so an
inspect issued immediately after start reports an empty binding.
→ [A3](journal/A3-sandbox-provider.md)

**Readiness needs two consecutive successes.** Some engines accept a connection during
initialization and then restart, so a single successful probe is not evidence the server is up.
→ [A3](journal/A3-sandbox-provider.md)

**Container health probes use `127.0.0.1`, not `localhost`.** VictoriaMetrics listens IPv4-only while
`localhost` resolves to `::1` first in its image, which made a perfectly healthy server fail its
probe. → [Foundation](journal/00-foundation.md)

**On Windows, a plugin binary must carry `.exe`.** `os.Stat` reports `0666` for every regular file
including a compiled binary, so a POSIX executable-bit check rejects everything; and `exec.Command`
resolves through `PATHEXT`, so a file without the suffix cannot be launched at all. `go build -o`
never appends it. → [#53](https://github.com/danmorcov88/Fleetward/pull/53)
