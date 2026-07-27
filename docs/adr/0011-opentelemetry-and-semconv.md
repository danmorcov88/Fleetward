# ADR-0011: OpenTelemetry SDK and OTel database semantic conventions

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Two distinct telemetry concerns exist, and conflating them is a common mistake: (a) Fleetward's own
observability, and (b) the metrics Fleetward *collects about the databases it monitors*.

For (b), every engine reports differently — `pg_stat_database`, `SHOW GLOBAL STATUS`, `serverStatus`,
`INFO`. If each plugin invented its own metric names, core and the UI would need per-engine
knowledge, violating the capability-driven rule of §4.

## Decision

- The OpenTelemetry Go SDK for Fleetward's own traces, metrics, and logs.
- **Plugin-emitted database metrics follow OTel database semantic conventions** (`db.client.*`), with
  engine-specific extras namespaced clearly and declared through capabilities.

## Consequences

- Core and the UI can chart connection counts across Postgres, MySQL, MongoDB, and Redis without
  knowing which engine produced them — the semconv naming *is* the normalization layer.
- Fleetward's own telemetry exports to any OTLP-compatible backend the operator already runs.
- Conformance can assert semconv compliance mechanically, so a community plugin cannot quietly
  invent its own names.
- Cost: plugin authors must map their engine's native metric names to semconv, which is real work
  and must be documented in the plugin-authoring guide.

## Alternatives considered

- **Ad-hoc per-plugin metric names.** Pushes engine knowledge into core. Directly violates §4.
- **A Fleetward-proprietary naming scheme.** Same normalization benefit, but discards an existing
  standard and the tooling that understands it.
