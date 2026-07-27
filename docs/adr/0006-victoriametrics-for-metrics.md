# ADR-0006: VictoriaMetrics as the metrics store

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward collects time-series metrics from every monitored instance on a ~30s loop. Building our
own time-series store is an explicit non-goal. We need something that ingests at fleet scale, is
cheap to run single-node in the dev compose stack, and speaks a query language DBAs and existing
tooling already know.

## Decision

VictoriaMetrics, single-node. Ingest via the Prometheus `remote_write` protocol. Query via its
Prometheus-compatible HTTP API, behind our `internal/storage/tsdb` interface.

## Consequences

- Far lower memory and disk footprint than Prometheus at the same cardinality, which matters when
  the dev stack must run comfortably on a laptop.
- `remote_write` means our collection loop is a well-understood, widely implemented protocol rather
  than a bespoke ingest path.
- PromQL compatibility means operators can point Grafana at it directly and reuse existing
  dashboards.
- Wrapping it behind our own interface keeps a future swap (Mimir, Thanos, plain Prometheus) cheap.
- Cost: a second datastore in the deployment alongside Postgres.

## Alternatives considered

- **Metrics in Postgres / TimescaleDB.** One less service, but poor fit for high-cardinality
  time-series at fleet scale and no PromQL.
- **Prometheus.** Pull-based by design, which fights our push-from-plugins collection model, and
  heavier at the same retention.
