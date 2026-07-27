# ADR-0016: Structural drift detection via schema snapshots

- **Status:** Accepted
- **Date:** 2026-07-27

## Context

A DBA responsible for an estate needs to know when a database's structure changed without their
knowledge. An index dropped by someone debugging at 3am, a column type widened by an application
migration nobody reviewed, a constraint quietly removed — each can degrade performance or break
assumptions, and each is currently discovered only when something else breaks.

Existing tooling answers "what does the schema look like now". The question that matters
operationally is "what changed since last week, and did anyone intend it".

## Decision

Plugins can produce a **normalized structural fingerprint** of a database through an additive
`GetSchemaSnapshot` RPC. Fleetward stores snapshots over time and diffs consecutive ones.

The snapshot is deliberately *structural*, not a dump:

- Tables and columns with types, nullability, and defaults
- Indexes with their columns, uniqueness, and method
- Constraints: primary keys, foreign keys, checks, unique
- Views, routines, and triggers by name and definition hash

It captures **no data and no row counts**. Those belong to the backup manifest, which answers a
different question. Mixing them would make snapshots expensive on large databases and would put
business data into a structural record that is stored indefinitely.

Each snapshot carries a stable content hash, so "did anything change" is a single comparison rather
than a full diff, which matters when polling fifty servers.

Normalization is the plugin's responsibility. Every engine spells its catalogue differently, and
core must be able to diff two snapshots without knowing which engine produced them — the same rule
that governs metric naming (ADR-0011).

## Consequences

- Structural change becomes a timeline an operator can read, and an alert they can receive, rather
  than an archaeology exercise after an incident.
- Storing definition hashes rather than full definitions keeps the history cheap while still
  detecting change. The tradeoff is that a diff can say *that* a routine changed without saying
  *how*; retrieving the current definition is a separate, on-demand call.
- Cost: snapshotting is not free on a database with tens of thousands of objects. Frequency must be
  configurable per instance, and the capability must let a plugin declare that it cannot do this at
  all.
- Snapshots are structural metadata about production systems. They are not secret, but they are
  sensitive reconnaissance material, and they inherit the same tenant scoping as everything else.

## Alternatives considered

- **Diffing full schema dumps** (`pg_dump --schema-only` and equivalents). Simple to implement and
  engine-native, but the output is unstable in ordering and formatting, producing diffs full of
  noise, and it is not comparable across engines.
- **Relying on the engine's own DDL audit trail.** Where it exists it is more precise, but coverage
  varies wildly by engine and it is frequently disabled. Snapshots work everywhere with read access.
- **Watching migration tooling instead.** Only catches changes made through that tooling, which is
  precisely the set of changes that were already intended.
