# A1 — PostgreSQL health checks and discovery

- **Delivered:** 2026-07-27 ([#13](https://github.com/danmorcov88/Fleetward/pull/13))
- **Brief:** none — A1 predates the brief template ([why](../slices/README.md))

`HealthCheck` and `Discover` implemented against a real PostgreSQL, covered by unit tests and by
testcontainers integration tests that now run in CI.

Decisions worth carrying forward:

- **The connection config is built field by field, never as a DSN.** A connection string containing
  a password ends up in error messages, logs, and stack traces; the only reliable prevention is
  never to construct one. `TestConnConfigDoesNotBuildADSN` and
  `TestConnectErrorsNeverLeakThePassword` guard this.
- **An unreachable instance is `HEALTH_STATE_DOWN`, not an RPC error.** "Down" is the most important
  answer this RPC gives, and returning it as a failure would lose the distinction between "the
  database is down" and "we could not ask".
- **Authentication failure is deliberately not retryable.** The same wrong password stays wrong, and
  retrying can trip account lockout on the monitored instance.
- **Missing privileges never fail discovery.** A monitoring account without `pg_read_all_settings`
  or `pg_read_all_stats` is good practice, so `data_directory` and `pg_stat_replication` are
  best-effort; their absence must not turn a permissions choice into a false outage.
- Only three capabilities are declared — `supports_schema_discovery`, `supports_replication`,
  `supports_replication_lag` — and a test asserts the rest stay off until implemented.

