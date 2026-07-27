# ADR-0014: `log/slog` for structured logging

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward's logs are operational evidence: which backup ran, on which instance, why verification
failed. They need to be machine-parseable in production and readable during development. They also
touch credentials constantly, so the logging layer is a security surface.

## Decision

The standard library's `log/slog`. JSON handler in production, a human-readable handler in
development, selected by configuration.

- Structured key/value fields, never formatted-string interpolation of important values.
- Context propagation carries request, tenant, and job identifiers into log records.
- **No secrets in logs, ever**: no passwords, no connection strings containing credentials, no
  presigned URLs with signatures, no artifact contents.

## Consequences

- No third-party logging dependency in any binary, including plugins — one less thing for a
  community plugin author to get wrong.
- JSON output ships directly to any log pipeline; structured fields make "every backup for instance
  X" a query rather than a grep.
- `slog.Handler` is an interface, so an OTel logs bridge (ADR-0011) plugs in without changing call
  sites.
- Cost: `slog` is less feature-rich than `zerolog` or `zap`, and marginally slower. Irrelevant at
  our log volume.

## Alternatives considered

- **`zerolog` / `zap`.** Faster and more featureful; not worth a dependency in every plugin binary
  for a workload that is nowhere near log-throughput-bound.
- **Standard `log`.** Unstructured. Fails the machine-parseable requirement.
