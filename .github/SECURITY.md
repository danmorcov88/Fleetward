# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Use GitHub's [private vulnerability reporting](https://github.com/danmorcov88/fleetward/security/advisories/new)
for this repository, or email **security@fleetward.dev**.

Please include: affected version and component, reproduction steps or proof of concept, the impact
you believe it has, and any suggested mitigation.

We aim to acknowledge within 3 business days and to provide an assessment with a remediation
timeline within 10 business days. We will keep you updated as we work, and we will credit you in
the advisory unless you prefer otherwise.

## Scope

Fleetward holds credentials for production databases and can trigger restores. We consider the
following particularly severe and are especially interested in reports about them:

- Any path that exposes stored credentials — through an API, logs, error messages, metrics, or the
  UI.
- Authorization bypass: performing an action your role and scope should not permit, or reading data
  belonging to another tenant. (Not yet applicable — see the limits below.)
- Anything that allows a restore to be triggered against an unintended target.
- Sandbox escape during backup verification.
- Injection into the native tooling plugins invoke (`pg_basebackup`, `mysqldump`, `mongodump`, …).

## Security model and its current limits

Fleetward is pre-alpha. These are known, deliberate limitations of the MVP — stated plainly rather
than left for you to discover:

- **Secrets at rest.** The MVP `SecretsProvider` encrypts credentials with AES-GCM in PostgreSQL,
  using a master key supplied via environment variable or mounted file. **The security of every
  stored credential reduces to the protection of that key.** A Vault-backed provider is planned
  (see [ADR-0009](../docs/adr/0009-secrets-provider-interface.md)).
- **The dev stack is not production configuration.** `docker-compose.yml` ships development
  credentials for Postgres, MinIO, and Dex, and runs without TLS. It is for local evaluation only.
  Never expose it to a network you do not fully control.
- **Authorization is enforced server-side, on every route, and an endpoint that relies on the UI to
  restrict access is a vulnerability worth reporting.** Every route under `/api/v1/` requires a
  credential and a role within the scope it acts on; `/healthz`, `/readyz`, `GET /api/v1/version`
  and `POST /api/v1/sessions` are the deliberate exceptions. A test enumerates every method of every
  generated service interface by reflection and asserts each one refuses an unauthenticated caller,
  so the claim in this paragraph is checked by CI rather than written from the architecture — which
  is what a previous version of this file got wrong
  ([ADR-0024](../docs/adr/0024-production-readiness-is-a-slice-property.md)).
  The limits are documented in
  [`docs/ops/authorization.md`](../docs/ops/authorization.md) and the ones worth knowing here are:
  most listings need a tenant-wide grant or an explicit instance filter, since only the instance and
  adherence listings filter their rows per caller; a session cookie outlives the revocation of the
  token it was minted from until it expires; a revoked token keeps working for up to
  `AUTH_PRINCIPAL_CACHE_TTL` on a replica that did not revoke it; and there is no rate limiting.
- **Authentication is API tokens and sessions, not yet an identity provider.** OIDC is a later
  slice. Until it lands, an administrator issues bearer tokens and the UI exchanges one for an
  `HttpOnly` session cookie. `FLEETWARD_AUTH_BOOTSTRAP_TOKEN` is a break-glass credential that
  grants tenant-wide administrator; it is configuration and never a database row, and an
  installation is expected to remove it after issuing a real token
  ([ADR-0033](../docs/adr/0033-the-bootstrap-credential-is-configuration-and-never-a-row.md)).
- **`FLEETWARD_AUTH_ENABLED=false` disables authorization entirely** and makes every request a
  tenant-wide administrator. The control plane refuses to start with it in production and logs a
  warning everywhere else. It exists for development, and the development stack does not use it.
- **The control plane needs the Docker socket, and that is root-equivalent on the host.** Backup
  verification provisions a throwaway container of the matching engine, so `docker-compose.yml`
  mounts `/var/run/docker.sock` into the control plane. Anything able to reach the control plane's
  process can therefore reach the host's container runtime. Run it on a dedicated host, behind a
  socket proxy restricted to the endpoints it uses, or point `FLEETWARD_SANDBOX_DOCKER_HOST` at a
  separate sandbox host.

## Supported versions

Pre-1.0, only the latest release receives security fixes.
