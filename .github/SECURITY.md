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
  belonging to another tenant.
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
- **Authorization is enforced server-side**, on every route. The UI hides actions a user cannot
  perform, but that is a convenience, never a control. If you find an endpoint that relies on the
  UI to restrict access, that is a vulnerability — please report it.

## Supported versions

Pre-1.0, only the latest release receives security fixes.
