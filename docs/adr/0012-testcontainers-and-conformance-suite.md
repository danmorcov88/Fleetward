# ADR-0012: testcontainers-go integration tests and a shared plugin-conformance suite

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

A backup tool that has not been tested against a real database has not been tested. Mocking
`pg_basebackup` proves nothing about whether a restored cluster actually starts.

Separately, we intend to accept community-contributed engine plugins. Reviewers cannot be expected
to manually verify that a Cassandra plugin implements ten RPCs correctly.

## Decision

- Integration tests use `testcontainers-go` and spin real engines. **No test may require a
  pre-installed database engine** — a fresh clone plus Docker must be sufficient.
- A single shared **conformance suite** in `test/conformance` runs against every plugin, asserting
  the full contract: discover → metrics → backup → restore-to-sandbox → verify → principals.
- **Conformance passing is the merge gate for any plugin change.** Born in Stage 1 with the
  PostgreSQL reference plugin; every subsequent plugin runs the same suite unmodified.

## Consequences

- The suite is what makes community plugins trustworthy: "does it pass conformance?" replaces
  reviewer archaeology.
- Writing the suite against Postgres first, then running it against three more engines in Stage 3,
  surfaces contract leaks — places where the abstraction quietly assumed Postgres. Finding those in
  Stage 3, before UI work, is deliberate sequencing.
- Cost: CI needs Docker and integration runs are minutes, not seconds. Split fast unit tests from
  the integration tag so the inner dev loop stays quick.

## Alternatives considered

- **Mocked engine responses.** Fast and worthless for the one thing that must not break.
- **A separate hand-written test suite per plugin.** Guarantees divergence in rigor and defeats the
  purpose of a contract.
