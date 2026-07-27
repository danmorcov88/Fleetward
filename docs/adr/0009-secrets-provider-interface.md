# ADR-0009: `SecretsProvider` interface with AES-GCM at rest for MVP

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward stores connection credentials for every monitored instance — some of the most sensitive
data in an organization. Serious deployments will demand Vault or a cloud KMS. Requiring Vault for
a `docker compose up` quickstart would kill adoption.

## Decision

A `SecretsProvider` interface in `internal/storage/secrets` with two implementations over time:

- **MVP:** AES-GCM envelope encryption at rest in Postgres, with the master key supplied by
  environment variable or a mounted file — never committed, never defaulted in production config.
- **Later:** a Vault-backed implementation, added without touching call sites.

Rules that hold for both implementations:

- Plugins receive credentials per-request via a `ConnectionRef` resolved by core. Plugins never
  persist credentials to disk or memory beyond the call.
- Secrets are never logged, never included in error messages, and never returned by any read API.

## Consequences

- The quickstart needs no external secret manager, while serious deployments have a documented path.
- AES-GCM gives authenticated encryption, so tampering with ciphertext in the database is detected
  rather than silently decrypting to garbage.
- Cost: MVP security depends on protecting one master key. This is documented plainly in
  `SECURITY.md` rather than papered over.

## Alternatives considered

- **Plaintext credentials in Postgres.** Unacceptable at any stage, including MVP.
- **Vault as a hard requirement from day one.** Correct for production, fatal for adoption.
- **Delegating entirely to the OS keychain.** Does not survive containerized deployment.
