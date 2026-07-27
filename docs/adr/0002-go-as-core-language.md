# ADR-0002: Go as the core language

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward ships a control plane, a CLI, and a fleet of engine plugins that must all run as
self-contained binaries on macOS and Linux, on both arm64 and amd64. Plugins shell out to native
database tooling and stream large artifacts. Operators will install this on servers where a
runtime dependency is a liability.

## Decision

Go 1.25 or newer for the control plane, the CLI, and all engine plugins.

## Consequences

- Static binaries, trivial cross-compilation, no runtime to install — this directly enables the
  `goreleaser` matrix in Stage 6 and the "download one binary" plugin distribution story.
- Excellent process and streaming primitives, which is most of what a plugin does.
- First-class support in `hashicorp/go-plugin`, `testcontainers-go`, `pgx`, and the OpenTelemetry
  SDK — every other decision in §2 has a mature Go implementation.
- Cost: more verbose than the alternatives, and generics are still comparatively limited. We accept
  verbosity in exchange for operational simplicity.

## Alternatives considered

- **Rust.** Better correctness guarantees, materially slower to build a broad surface area, and a
  much smaller pool of contributors for an OSS project that depends on community plugins.
- **JVM / .NET.** Strong DB ecosystems, but a runtime dependency on operator machines and a heavier
  plugin distribution story.
