# Foundation — the contract, the control plane, and the dev stack

- **Delivered:** 2026-07-27
- **Brief:** none — the foundation predates the slice protocol ([why](../slices/README.md))


| Deliverable | Status |
|---|---|
| Repository layout per `CLAUDE.md` §3 | done |
| `CLAUDE.md` + ADR-0001..0014 | done |
| buf setup, contract compiled, generated code committed | done |
| `EnginePlugin` contract (10 RPCs, capability matrix) | done |
| Plugin SDK: `Engine`, `Base`, `Serve`, typed errors, capability validation | done |
| Plugin manager: discovery, launch, mTLS handshake, supervision, restart with jittered backoff | done |
| Four plugin binaries handshaking (no engine logic yet) | done |
| Metadata schema v1: 18 tables, every tenant-scoped one carrying `tenant_id` | done |
| Storage layer: metadb, objstore, tsdb, secrets | done |
| AES-GCM secrets provider with envelope encryption | done |
| Control plane: `/healthz`, `/readyz`, `/api/v1/version`, graceful shutdown | done |
| `fleetward-cli`: `version`, `health`, `keygen` | done |
| Web UI shell: app shell, Estate placeholder, live System status | done |
| `docker-compose.yml`: postgres, victoriametrics, minio, dex, control plane, web | done |
| CI: buf lint/breaking/codegen-drift, golangci-lint, tests, build, govulncheck, web, compose smoke | done |
| `Makefile` | done |

**Exit criteria:** `docker compose up` yields a healthy control plane with a green `/readyz`, an
empty UI shell at `localhost:3000`, and green CI. ✅

**Verified on macOS (Apple Silicon), 2026-07-26:** all six services reach healthy, `/readyz`
reports `healthy` across metadb, secrets, objstore, tsdb, and plugins, all four plugin binaries
handshake and reach ready, migrations apply cleanly to version 1, the `audit_log` append-only
trigger rejects both UPDATE and DELETE, and the MinIO bucket is created on first start.

## Notable foundation decisions not in the original brief

- **Go 1.25, not 1.23.** The brief specified Go 1.23+. Current `pgx/v5`, `minio-go/v7`,
  `grpc-gateway/v2`, and the OpenTelemetry SDK all declare `go 1.25`, so 1.25 is the real floor.
  Holding 1.23 would mean pinning older releases of all four, which is a worse trade than raising
  the toolchain. ADR-0002, the Dockerfile, and CI were updated to match.
- **`internal/config`** was added to the layout. Configuration is shared by the server and the CLI,
  so it does not belong under `internal/controlplane/`.
- **Migrations live at `internal/storage/metadb/migrations/`**, not at the repository root, so they
  can be `go:embed`-ed and the control plane can migrate itself with no external files.
- **Prometheus remote-write types are vendored** as a four-message proto under
  `internal/storage/tsdb/prompb/`, rather than depending on `github.com/prometheus/prometheus`,
  whose module graph is enormous relative to what we use. It is excluded from the buf module
  because it is an external wire format we conform to, not part of our published contract.
- **`buf lint` excludes `RPC_RESPONSE_STANDARD_NAME` and `SERVICE_SUFFIX`**, so the contract can use
  the names in the brief (`Capabilities`, `HealthStatus`, `PITRWindow`, `EnginePlugin`). Both
  exclusions are documented inline in `buf.yaml`.
- **Every published port in `docker-compose.yml` is overridable** via a `.env` file (see
  `.env.example`). Developer machines routinely already run a Postgres or a MinIO, and a port
  collision should not be the first thing a new contributor debugs.
- **Container health probes use `127.0.0.1`, not `localhost`.** VictoriaMetrics listens IPv4-only
  while `localhost` resolves to `::1` first in its image, which made a perfectly healthy server
  fail its probe.
- **Plugin capabilities start all false.** Capabilities are a promise core relies on when
  deciding what to do to a production database, so each flag is turned on in the same change that
  implements the behavior behind it — never in advance.

---
