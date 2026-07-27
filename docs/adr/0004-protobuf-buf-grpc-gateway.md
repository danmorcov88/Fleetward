# ADR-0004: Protobuf with `buf`, gRPC internally, REST via grpc-gateway

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

We need one authoritative definition of the plugin contract (§4) and of the control-plane API. The
plugin contract in particular is a public, versioned interface that third parties will implement;
breaking it silently would break every community plugin.

The web UI and the CLI need an ergonomic external API, and integrators expect OpenAPI.

## Decision

- All interfaces defined as Protobuf in `api/proto/fleetward/v1/`, managed with `buf`.
- gRPC for all internal communication: core ↔ plugins, and the control plane's own service layer.
- External REST/JSON generated with `grpc-gateway`, and OpenAPI v3 generated from the same protos.
- `buf lint` and `buf breaking` run in CI against the `main` branch. A breaking change to
  `plugin.proto` must be a deliberate, reviewed act.

## Consequences

- One source of truth; the OpenAPI spec cannot drift from the implementation because both are
  generated.
- Backward-compatibility of the plugin contract is enforced mechanically, not by reviewer memory.
- Streaming (`CollectMetrics`, `Backup`, `Restore`) is natural.
- Cost: a codegen step in the build (`make proto`), and generated code checked into the repo so
  that a plain `go build` works without `buf` installed.

## Alternatives considered

- **Hand-written REST + JSON schemas.** No mechanical breaking-change detection, and no streaming
  story for backup progress.
- **`protoc` directly.** `buf` gives us lint, breaking-change detection, and dependency management
  that we would otherwise hand-roll.
