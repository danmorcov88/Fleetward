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
- **There is no authentication or authorization yet.** Every route under `/api/v1/` is open to
  anyone who can reach the port, including the routes that add an instance and trigger a backup.
  `FLEETWARD_AUTH_*` is parsed and validated but nothing consumes it, and the tenant is a fixed
  constant, so setting `FLEETWARD_ENV=production` demands `AUTH_ENABLED=true` while enforcing
  nothing. **Do not expose Fleetward to any network you do not fully control.** Server-side
  enforcement — a principal per request, role and scope checked at every method, and an audit
  record per mutation — is the next security slice on the [roadmap](../docs/dev/STATUS.md); when it
  lands, an endpoint that relies on the UI to restrict access becomes a vulnerability worth
  reporting.
- **The control plane needs the Docker socket, and that is root-equivalent on the host.** Backup
  verification provisions a throwaway container of the matching engine, so `docker-compose.yml`
  mounts `/var/run/docker.sock` into the control plane. Anything able to reach the control plane's
  process can therefore reach the host's container runtime. Run it on a dedicated host, behind a
  socket proxy restricted to the endpoints it uses, or point `FLEETWARD_SANDBOX_DOCKER_HOST` at a
  separate sandbox host.

## Supported versions

Pre-1.0, only the latest release receives security fixes.
