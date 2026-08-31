# ADR-0023 — The conformance suite carries a per-engine fixture, and only a fixture

- **Status:** Accepted
- **Date:** 2026-08-31
- **Slice:** A6 — proving that verification actually verifies
- **Relates to:** [ADR-0012](0012-testcontainers-and-conformance-suite.md),
  [ADR-0020](0020-sandbox-credentials-from-template-placeholders.md),
  [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md)

## Context

ADR-0012 made one shared suite the merge gate for every plugin, and CLAUDE.md §6 states the test
that follows from it: *if an engine requires changing the suite, that is a contract leak, and the
fix belongs in the contract rather than in the test.*

Growing the suite from capabilities-and-health into the full backup → restore → verify path put
that rule under real pressure for the first time, and it survived everywhere except one place.

Most of what the path needs turned out to be expressible with no engine knowledge at all. The
source database is stood up through the sandbox provider from the plugin's own `SandboxTemplate`,
which is the mechanism ADR-0020 built for exactly this reason — core already has an engine-agnostic
way to run an engine. The artifact is written through presigned part grants and read back through a
presigned `GET`. Which tool loads it comes from the backup's own metadata, handed back verbatim.
The comparison is against the manifest the plugin itself produced.

The exception is seeding. Verifying a backup of an empty database is worse than useless: the
manifest has no entries, the comparison runs over zero objects, and the answer is `VERIFIED` for an
artifact nobody has checked — the exact failure ADR-0022 spends a section refusing. So the suite
needs rows in a database, and it needs to be able to take some out again to produce a manifest that
no longer describes its source.

There is no RPC that can do either, and there should not be. Fleetward never writes to a monitored
instance: it is a §1 non-goal, it is why `ListPrincipals` is read-only (ADR-0017), and it is why the
query editor is gated behind five conditions (ADR-0018). Adding a "run this statement" RPC to make
a test easier would be the largest blast-radius change in the project, made for the smallest reason.

Three options were considered.

**Back up an empty database.** Costs nothing and proves nothing — see above.

**Add a write RPC to the contract.** Rejected on the grounds above. The test is not a good enough
reason to give a plugin a general write path into production.

**Let each engine supply the seed, in the test and nowhere else.** Chosen.

## Decision

**`test/conformance` defines a two-method `Fixture` interface, and registers one implementation per
engine. Nothing else in the suite is engine-specific.**

```go
type Fixture interface {
    Seed(ctx context.Context, creds *fwv1.Credentials) error
    RemoveRows(ctx context.Context, creds *fwv1.Credentials) (objectName string, removed int64, err error)
}

var fixtures = map[string]Fixture{"postgresql": postgresFixture{}}
```

`Seed` populates a freshly provisioned instance. `RemoveRows` deletes rows from exactly one object
and reports which one — by the name the manifest uses for it — and how many went.

Four properties make this an extension point rather than the leak CLAUDE.md warns about.

**It is additive.** Adding an engine means registering a fixture. It never means changing an
assertion, a helper, or a skip condition. If a future engine cannot be exercised without editing
something else in the suite, that *is* the contract leak, and this ADR does not license it.

**It is capability-gated in the same way everything else is.** A plugin with no fixture still runs
the whole Stage 0 suite; its Stage 1 cases skip with a message naming what is missing. A suite that
failed a plugin for not yet having a fixture would stop being useful from a plugin's first commit,
which is the property ADR-0012 exists to protect.

**No assertion depends on it.** Everything a fixture produces reaches the tests through the
plugin's own manifest — including the row counts. The suite asks the fixture how many rows it
removed only to check that the plugin reported the same number back.

**It is confined to the test.** No fixture code is compiled into core, into a plugin, or into any
binary. The files carry the `conformance` build tag, so they do not exist outside `make
conformance`.

## Consequences

**Contributing an engine now has one more required artifact.** It is under fifty lines for
PostgreSQL, it is documented in `docs/dev/writing-an-engine-plugin.md` §10, and it is the smallest
of the several things a plugin author has to write. The alternative was contributing an engine whose
backups have never been proven restorable, which is not a trade worth making.

**A fixture can be wrong in a way the suite cannot detect.** A `Seed` that creates one table with
one row would let a plugin pass the count comparison by luck. The interface documents what a fixture
must produce — several objects with different counts — but nothing enforces it, and a reviewer
reading a new engine's fixture should check that first.

**`RemoveRows` names an object in the manifest's own vocabulary**, which is the one place the
fixture and the plugin have to agree on something. For PostgreSQL that is a schema-qualified table
name; for a document store it will be a collection. This is a genuine coupling, and it is the price
of an assertion that names the object rather than reporting a bare boolean.

**The suite now needs a container runtime and the plugin's native tooling on `PATH`.** Both missing
produce a skip rather than a failure, so a contributor without them still gets Stage 0. CI installs
both, because a skipped merge gate is not a merge gate.
