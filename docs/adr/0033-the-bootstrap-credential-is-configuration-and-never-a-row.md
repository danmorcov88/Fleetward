# ADR-0033: A principal is an API token, a session is minted from one, and the first credential is configuration rather than a row

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B6 — the authorization spine
- **Relates to:** [ADR-0008](0008-oidc-rbac-multitenancy.md),
  [ADR-0009](0009-secrets-provider-interface.md),
  [ADR-0024](0024-production-readiness-is-a-slice-property.md),
  [ADR-0029](0029-the-openapi-document-is-generated-to-match-the-wire.md)

## Context

[ADR-0008](0008-oidc-rbac-multitenancy.md) settled that authentication is OIDC and that Fleetward
stores no passwords. The roadmap then put OIDC in B10 and the authorization spine in B6, on the
reasoning that the spine is what makes an installation defensible and the identity provider is a
component that plugs into it.

That ordering leaves a gap this slice has to fill: for the length of four slices, something other
than an identity provider has to say who is calling. It has to work for three quite different
callers — a browser, a script, and an operator on a machine that has just come up for the first
time — and it has to be replaceable by OIDC without the replacement being a rewrite.

There is also a chicken-and-egg problem with no clever solution. `fleetward token create` needs a
credential to authenticate with. The CLI deliberately never opens the metadata database — that rule
is written into `cmd/fleetward-cli/client.go` and exists so authorization is not duplicated in a
second place and so the database password is not on every operator's laptop — so the first
credential cannot come from a CLI command that writes a row.

## Decision

Three mechanisms, which are not alternatives to one another but the three shapes the same identity
has to take.

**1. An API token is the credential.** `fwt_<token_id>_<secret>`, presented as
`Authorization: Bearer`. The id half is stored in the clear and indexed; the secret half is stored
only as its SHA-256 and compared in constant time.

SHA-256 rather than bcrypt or argon2. The secret is 128 bits from `crypto/rand`, so there is no
dictionary for a work factor to slow down, and a KDF would put tens of milliseconds on the hot path
of a screen that refetches every thirty seconds across fifty rows. A password KDF protects a
password; there is no password here.

A verified credential is cached in memory, keyed by the SHA-256 of what was presented, for
`AUTH_PRINCIPAL_CACHE_TTL` — 15 seconds by default, refused above five minutes. That TTL is the
window in which a revoked credential keeps working on a replica that did not perform the
revocation, which is why it is short rather than convenient.

**2. A session is minted from a token, and the browser never holds a token.**
`POST /api/v1/sessions` exchanges one for an `HttpOnly; SameSite=Strict` cookie carrying a signed
statement of user id and expiry. Anything a script on the page can read is something an injected
script can steal, and a Fleetward token can restore a production database.

The session is a signature, not a row: no server-side session table to grow, to clean up, or to fail
to clean up. Revoking a session before it expires therefore means revoking the token behind it,
which is stated in `docs/ops/authorization.md` rather than left to be discovered.

Session establishment is two hand-written HTTP handlers rather than RPCs. A cookie is a transport
concern Protobuf cannot express, and the same reasoning has always kept `/readyz` outside the
contract ([ADR-0029](0029-the-openapi-document-is-generated-to-match-the-wire.md)).

**3. The first credential is configuration and is never a database row.**
`FLEETWARD_AUTH_BOOTSTRAP_TOKEN`, or the `_FILE` form that is preferred for the reason
`fleetward keygen` already gives about the secrets master key. It maps to a synthetic caller named
`bootstrap` with tenant-wide `admin`.

**The seam is one interface**, `authn.Authenticator`, and a chain of implementations tried in order:
session cookie, the configured bootstrap credential, then the token store. B10 adds one
implementation and one branch inside the session handler — exchange an authorization code instead of
a token — and changes nothing about roles, grants, scope resolution, the audit log, or the UI.

## Consequences

**The bootstrap credential cannot outlive its configuration.** This is the property the whole third
decision exists for. A seeded administrator row would survive the operator removing every
environment variable, would be invisible in a diff, and could only be revoked by somebody who knew
it existed. There is nothing to revoke here because there is nothing stored: delete the setting,
restart, and the access is gone with nothing left behind to find later.

**Its use is always visible.** Every action it takes is audited under `actor = "bootstrap"`, which
is visibly not a person, in a table that cannot be edited or deleted. The control plane logs a
warning naming it on *every* start while it is set — not once — because a break-glass credential
nobody is reminded about is one that stays configured for a year.

**A leaked bootstrap token is tenant-wide admin until somebody removes the setting.** That is the
cost, it is real, and it is why the recommended sequence in `docs/ops/authorization.md` is: start,
issue a proper admin token, remove the setting, restart.

**The token store is last in the chain, and that ordering is load-bearing.** The chain stops on a
credential that is presented and rejected, so a revoked token cannot fall through to whatever comes
next — and the store is the only link that produces that verdict, because a bearer value it has
never seen is *invalid* rather than somebody else's. Every link that has to see a bearer value must
therefore sit in front of it.

The B6 walk found this the hard way, with the store ahead of the bootstrap link: a bootstrap
credential is by definition a bearer value the database has never seen, so the store called it
invalid and the chain stopped. The break-glass credential did not work on any installation, with any
value, and every test passed. `internal/controlplane/authn/authn_test.go` now pins both directions
of the ordering.

**Recovering from losing every admin token means editing configuration**, not clicking anything.
Set a bootstrap token, restart, issue a new token. That is a deliberate trade against the
alternative, which is a permanent recovery path that is also a permanent way in.

**A restart signs everybody out** unless `AUTH_SESSION_KEY_FILE` is configured, because the signing
key is generated per process by default. That is the right default for a single node — it removes a
mandatory secret from the quickstart and costs one sign-in — and an installation running more than
one replica configures it or watches its users bounce between them.

**API tokens survive B10.** OIDC is a browser flow; a cron job on a laptop still needs a bearer
credential. So `api_tokens` is not scaffolding to be thrown away, which is part of why it was worth
building properly rather than fudging.

## Alternatives considered

**A trusted header from a reverse proxy.** Zero code. Rejected because the security of the entire
product would become "did the operator configure their proxy correctly", and a direct connection to
port 8080 defeats it with no signal that anything is wrong.

**A local bootstrap admin created on first start.** Solves the first credential and creates the
permanent back door this decision is mostly about avoiding. It is the option that looks friendliest
and ages worst.

**Local user accounts with passwords.** Explicitly forbidden by
[ADR-0008](0008-oidc-rbac-multitenancy.md), and it would make Fleetward responsible for credential
storage, reset flows, and lockout policy — a liability the product was designed not to have.

**Sessions in a database table.** Revocable before expiry, which signed statements are not. Rejected
for now: it is a table that grows, that needs sweeping, and whose sweep is another thing that can
fail silently, in exchange for closing a window already bounded by the session TTL. Worth revisiting
when somebody needs to end a specific session rather than a credential.

**Storing the token secret with bcrypt.** The reflex answer, and wrong for a high-entropy secret.
See the reasoning above; the cost would have been paid on every request of the product's main
screen.
