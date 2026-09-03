# B6 — The authorization spine, or the slice where a stranger can

B5 was the slice where a *bug* could destroy data. This is the slice where a *stranger* can.

Every route under `/api/v1/` is open to anyone who can reach the port. That includes adding an
instance, storing its credentials, creating a schedule, triggering a backup, triggering a restore,
and — since B5 — the retention configuration that decides what gets deleted. There is no principal,
no role, no tenant that is not a constant, and no record of who did anything.

It is also the last thing standing between Fleetward and a pilot installation. B7, B8 and B9 are
delivery, observability and packaging; none of them is a reason not to install it. This is.

> **Until this slice, "who did this" had no answer because there was nobody. From this slice on,
> every mutating action names a principal, and a refusal is as much a record as a success.**

---

## Goal

**Every request to the control plane names a principal, every route decides on that principal's role
within the scope it is acting on, and every mutating action lands in the append-only audit log.**

## Why now

**Nothing else on the roadmap is a reason not to install Fleetward; this is.** B7 (alerts), B8
(self-observability) and B9 (the release artifact) each make an installation better. Only this one
makes an installation *defensible*. A tool that holds database credentials for fifty production
servers and serves them behind an open port is not a tool anybody may put on a company network.

**The schema has been waiting since the first migration and no Go file touches any of it.**
`users`, `roles` (seeded, with ranks), `role_grants` (scoped, constrained) and `audit_log`
(append-only by trigger) have all existed since `000001`. A grep for any of those four table names
across `internal/` and `cmd/` returns nothing outside the migration. Not one row is ever written or
read. Meanwhile `cfg.Auth` is fully parsed, fully validated, refuses to start in production with
`AUTH_ENABLED=false`, and is read by no file outside `internal/config`. The design has been ahead of
the code here longer than anywhere else in the tree, and ADR-0024 exists because that gap already
produced one false security claim.

**It is a precondition for three things that are already queued behind it.** `DeleteBackup` — the
only way to reclaim what B5's retention floor pins — needs confirmation, an audit record and RBAC
(B5's journal says so). ADR-0017's approval workflow for access-compliance remediation names RBAC
and the audit log as its two missing prerequisites. ADR-0018 gates the query editor on server-side
RBAC and an audit record per execution. All three are waiting on this slice specifically.

**B10 gets cheaper the later it happens, and only if the seam exists.** The roadmap is explicit that
B6 builds the spine and B10 swaps OIDC in behind it. Building the spine without a seam means B10 is
a rewrite; building the seam now costs one interface.

## Preconditions

All hold on `main` at `9082280`. Verified by reading the code, not assumed.

- **`tenants` is seeded with one row** (`000001_init.up.sql:512`) whose id is
  `metadb.DefaultTenantID` (`internal/storage/metadb/metadb.go:29`). That constant is held as a
  field by exactly four services: `backup` (`service.go:145`), `inventory` (`service.go:102`),
  `scheduler` (`scheduler.go:168`) and the schedule service (`service.go:58`). It is dereferenced as
  `s.tenantID` at **72 sites** across `internal/controlplane/`, all of them as a query parameter.
- **`users` exists** (`:30`) with `UNIQUE (tenant_id, subject)` — the OIDC subject, unique per
  tenant, not globally — plus `email`, `display_name`, `is_active` and `last_login_at`. It has never
  held a row.
- **`roles` is seeded, not empty** (`:141`, insert at `:147`): `viewer` 10, `operator` 20, `dba` 30,
  `admin` 40, each with a description saying what it may do. The four roles and their ranks are
  facts in the database, not constants to invent in Go.
- **`role_grants` exists** (`:155`) binding a user to a role in a scope, with
  `CONSTRAINT role_grants_single_scope CHECK (environment_id IS NULL OR instance_id IS NULL)`
  (`:166`) and both NULL meaning tenant-wide. `role_name` is `ON DELETE RESTRICT` against `roles`.
  Two indexes already exist: `idx_role_grants_user` and `idx_role_grants_scope`.
- **`audit_log` exists** (`:446`) carrying `actor` as text *beside* a nullable `user_id`, so the
  record survives the user being deleted, plus `source_ip INET`, `user_agent`, `request_id`,
  `succeeded`, `details JSONB` commented "Never contains credentials, only which fields changed",
  and three indexes. **`CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE`** (`:474`)
  raises on both.
- **`jobs.triggered_by` (`:233`), `backups.triggered_by` (`:287`), `verifications.triggered_by`
  (`:314`), `restores.triggered_by` (`:343`) and `role_grants.granted_by` (`:162`)** all exist, are
  all nullable references to `users(id)`, and are all always NULL.
- **There is one middleware and it is the only seam of its kind.** `Server.middleware`
  (`internal/controlplane/api/server.go:150`) does request id, access logging and panic recovery,
  and wraps the whole router — including `/healthz` and `/readyz`, which are registered in
  `routes()` (`:60`). `telemetry.WithRequestID` already puts the request id in the context, which is
  the pattern a principal follows.
- **Requests reach a service as in-process grpc-gateway handlers; there is no gRPC listener**
  (ADR-0019). The generated `RegisterXHandlerServer` carries a comment saying in as many words that
  **gRPC interceptors do not work for this registration**. It does call
  `runtime.AnnotateIncomingContext`, and it derives its context from `req.Context()` — so anything
  the HTTP middleware puts in the request context arrives at the service method unchanged.
- **Three of the four services are registered; `SystemService` is not.** `main.go:210` builds the
  gateway and registers `InventoryService`, `BackupService` and `ScheduleService`. The 27 RPCs in
  `controlplane.proto` are therefore **24 served RPCs**; `SystemService`'s three have no
  implementation anywhere in the tree, and `GET /api/v1/version` is a hand-written handler.
- **`Problem` and `WriteProblem` already exist** (`server.go`), and `problemErrorHandler`
  (`gateway.go`) renders any gRPC status through it. `codes.PermissionDenied` already maps to 403
  and `codes.Unauthenticated` to 401 via `runtime.HTTPStatusFromCode`. No new error shape is needed.
- **The CLI sends no credential.** `cmd/fleetward-cli/client.go` builds a plain `http.Client` with a
  base URL and a timeout; every command goes through it. Its doc comment already says the CLI never
  opens the metadata store, so that authorization is not duplicated in a second place — which is why
  bootstrapping cannot be a CLI-writes-to-Postgres command.
- **The web app sends no credential either**, and deliberately shows no login, no account menu and
  no "signed in as" (B4's journal). `web/vite.config.ts` proxies `/api`, `/readyz` and `/healthz` to
  the control plane, so the browser is on one origin and cookies are same-origin by construction.
- **CI's `Dev stack smoke test`** (`.github/workflows/ci.yml:300`) runs
  `docker compose up -d --build --wait`, curls `/readyz` and the web UI. `docker-compose.yml:193`
  sets `FLEETWARD_AUTH_ENABLED: "false"` beside a full set of Dex settings.
- **`fleetward-cli keygen`** (`cmd/fleetward-cli/main.go:150`) is the existing precedent for a
  credential generated outside the system, delivered on stdout, and stored in a file rather than an
  environment variable — with the reasoning written in its help text.

---

## The six open decisions

These are the decisions this brief exists to settle. **Nothing below is implemented until they are
approved.** Each carries its options, its consequences, and a recommendation.

### 1. What a principal is, when there is no identity provider yet

B10 brings OIDC. Until then something has to say who is calling, and it must be a thing a `curl`, a
cron job and a browser can all present.

| Option | Consequence |
|---|---|
| Hashed API tokens in a table | A real credential store. Works for humans, scripts and CI. Survives B10, because OIDC is a browser flow and a cron job still needs a token. |
| A trusted header from a reverse proxy | Zero code, but the security of the whole product becomes "did the operator configure their proxy correctly", and a direct connection to port 8080 defeats it silently. |
| A local bootstrap admin created on first start | Solves only the first credential, not the second one. |
| A signed local session (cookie) | What a browser needs, and useless to a script. |

**Recommendation: hashed bearer tokens, plus a signed session cookie minted from one, plus a
configuration-only bootstrap token.** The three are not alternatives; they are the three shapes the
same identity has to take.

- **Tokens.** A new `api_tokens` table. Format `fwt_<16 hex id>_<32 hex secret>`, presented as
  `Authorization: Bearer …`. Stored as SHA-256 of the secret, looked up by the id half so the query
  is a single indexed row, compared in constant time. Not bcrypt: the secret is 128 bits of
  entropy, not a password, and a KDF on the hot path of a screen that refetches every thirty
  seconds across fifty rows is a cost this product cannot pay for no gain.
- **Sessions.** `POST /api/v1/sessions` exchanges a bearer token for an `HttpOnly; Secure;
  SameSite=Strict` cookie whose lifetime is `cfg.Auth.SessionTTL` — a setting that has existed and
  meant nothing since ADR-0008. The browser then never holds a token in JavaScript, so an XSS
  cannot exfiltrate one.
- **The seam, which is the point.** One interface:

  ```go
  // internal/controlplane/authn
  type Authenticator interface {
      Authenticate(ctx context.Context, r *http.Request) (Principal, error)
  }
  ```

  A chain tries session cookie → bearer token → bootstrap token → anonymous. **B10 adds one
  implementation and one branch inside `POST /api/v1/sessions`** — exchange an OIDC code instead of
  a token — and changes nothing about roles, grants, scope resolution or auditing. That is a swap.
- **The first credential on a fresh `docker compose up`:** `FLEETWARD_AUTH_BOOTSTRAP_TOKEN` /
  `_FILE`, following the `SECRETS_MASTER_KEY_FILE` precedent exactly, generated by
  `fleetward-cli token generate` the way `keygen` already generates the master key. It maps to a
  synthetic principal named `bootstrap` with a tenant-wide `admin` role.
- **What stops it being a permanent back door:** it is *configuration and never a database row*.
  There is nothing to revoke because there is nothing stored; deleting the environment variable
  deletes the access, and it cannot outlive the config the way a seeded admin row would. Every
  action it takes is audited with `actor = "bootstrap"`, which is visibly not a person, in a table
  that cannot be edited. The server logs a warning naming it on every start while it is set, and
  `config.Validate` refuses to start in production with the bootstrap token set *and*
  `AUTH_ENABLED=false` together.

### 2. What the scheduler is

A scheduled backup has no human behind it, and `triggered_by` is nullable for exactly that reason.
But §7.5 says *every* mutating action lands in `audit_log`, and a backup is mutating.

| Option | Consequence |
|---|---|
| A user row with a role | An identity that can be granted things, and therefore an identity that can be impersonated if it ever acquires a credential. |
| An actor string with no user row | `user_id` stays NULL, `actor` reads `system:scheduler`. Nothing to authenticate as. |
| Exempt | Two thirds of the rows in the audit log never exist, and "who deleted this artifact" has no answer. |

**Recommendation: an actor string, no user row, and not exempt.** `audit_log.user_id` is NULL and
`actor` reads `system:scheduler`, `system:retention`, `system:reaper`. The context carries a
`Principal{Kind: System, Name: "scheduler"}` so the audit writer takes one uniform input, and
`jobs.triggered_by` / `backups.triggered_by` stay NULL for scheduled work and carry the user id for
human-triggered work — which is what nullable was always for.

The property that makes this safe is worth stating: **a system principal has no credential, so
nothing can ever present one at the HTTP surface.** It is produced in-process by the scheduler and
by the retention sweep, and by nothing that parses a request. It also never passes an authorization
check, because it does not go through the API — it calls the services directly.

And "who deleted this artifact" now answers `system:retention`, which is the honest answer and the
one an operator staring at a missing backup actually needs.

### 3. Whether the tenant stops being a constant

**Recommendation: yes.** The four `tenantID` fields are removed and the tenant comes from the
principal in the request context.

The cost is real and should be stated plainly: **72 call sites**, most of them in
`internal/controlplane/backup/` — which is the code that deletes things. A mechanical change there
is exactly where a slice goes wrong.

Three things make it the right call anyway:

- **The queries do not change.** `s.tenantID` becomes `tenantOf(ctx)` and nothing else moves; B5's
  retention SQL stays byte-identical. It is a compiler-checked substitution, not a rewrite.
- **The failure mode is loud by construction.** `tenantOf(ctx)` returns an error when there is no
  principal in the context, and every caller already returns an error. A path that forgot to attach
  a principal fails with an internal error on its first request; it never silently reads the
  default tenant. That is the B5 lesson — make the wrong state impossible rather than unlikely.
- **There is no later slice for which this is cheaper.** B10 puts an identity provider on top of
  whatever B6 leaves; if the tenant is still a constant then, it stays one. ADR-0008 put `tenant_id`
  on every table from the first migration specifically so that this would never be a migration —
  and the claim has never once been exercised.

The constant survives in exactly one place: the seed, and the system principals that carry it.

### 4. Where enforcement lives

Scope is environment → instance, so this is not a question about URL paths. An instance id arrives
in a path variable on some routes (`/instances/{instance_id}/backups`) and in a request body on
others, and a backup id has to be resolved to its instance before its scope is even known.

| Option | Consequence |
|---|---|
| HTTP middleware on the path | One line, and it cannot see an instance id in a body or resolve a backup id to an instance. |
| A check inside each service method | Sees everything; is 24 call sites that a 25th can forget. |
| A policy table keyed on the RPC, applied by a decorator | Sees everything, and the forgetting is detectable. |

**Recommendation: a policy table plus a per-service decorator, with three independent things that
make a forgotten route fail loudly.**

- **The table** (`internal/controlplane/authz/policy.go`) maps each of the 24 served RPC names to a
  minimum role and to *where its scope comes from*: tenant-wide, an `instance_id` on the request, an
  `environment_id` on the request, or a `backup_id` / `schedule_id` / `job_id` that resolves to an
  instance. Scope extraction reads the request message through protobuf reflection, so it is one
  function rather than 24.
- **The decorator** wraps each generated `XServiceServer` and calls
  `guard.Check(ctx, method, req)` before delegating.
- **Failure one — compile time.** The decorator does **not** embed `UnimplementedXServiceServer`.
  `var _ fwv1.InventoryServiceServer = (*authzInventory)(nil)` then fails to build the moment the
  contract grows a method. This is the equivalent of B5's `CHECK` constraint: the mistake is
  refused by the toolchain, not caught by a reviewer.
- **Failure two — a fail-closed table.** `Check` on a method with no policy entry returns
  `PermissionDenied`. A new RPC is denied to everyone until somebody writes down what it needs.
- **Failure three — the test ADR-0024 §4 asked for.** A table test enumerates every method on every
  generated service interface by reflection, asserts each has a policy entry, and drives each one
  with an anonymous principal asserting 401 and with a `viewer` on a mutating method asserting 403.
  That is what makes SECURITY.md's claim true by construction rather than by intention.

**Grant resolution, which is the sharp edge.** `role_grants_single_scope` means a grant is
tenant-wide, environment-wide or instance-wide and never two. So what happens when a user holds
`viewer` on an environment and `dba` on one instance inside it — and, the case that matters, `dba`
on the environment and `viewer` on one instance inside it?

**Recommendation: the effective role for a scope is the maximum rank of every grant that covers
it.** Grants are additive; a grant only ever adds permission and never removes it. So `dba` on one
instance elevates within a `viewer` environment, and `viewer` on one instance does *not* demote a
`dba` environment grant. "Most specific wins" reads like the obvious rule and would quietly turn
`role_grants` into a deny mechanism the schema has no way to express — a security control that looks
like it works and does not. If a deny is ever wanted, it needs a column and an ADR. This one gets an
ADR, because it is exactly the kind of thing a future session would reasonably "fix".

**The hot path.** The estate view refetches every thirty seconds across fifty rows. Resolution is
one query per request at worst — the whole grant set for a principal is a handful of rows — and the
authenticated principal, grants included, is cached in memory keyed by token hash for a short TTL.
A revoked token therefore stays live for up to that TTL, which is stated in the docs and is why the
TTL is small rather than convenient.

### 5. What is audited, and whether a refusal is

**Recommendation: a refusal is audited when it names a principal, and not otherwise.**

- **401 — no credential, or a credential that is not recognised — is not audited.** It names no
  principal, so the row would carry nothing an investigator could use, and it is precisely the row
  an attacker can generate a million of. It goes to the access log and, in B8, to a counter.
- **403 — a real, authenticated principal refused by role or scope — is audited**, with
  `succeeded = false`. It is bounded by the number of issued tokens, and it is the single most
  interesting record in an audit log: somebody who *is* somebody tried to do something they may not.
- **Every mutating action is audited on both outcomes**, success and failure. Reads are not, with
  one exception argued below.

**What `details` may contain.** The rule "never holds credentials" is enforced by construction, not
by care: `details` is assembled from a fixed allow-list per action — the changed field *names*, the
resource type, the required role and the effective role — and **the request message is never
marshalled into it**. That last clause is the whole point. `CreateInstance` and the credential RPCs
carry a database password in the request; a generic "log the request for context" would write a
production credential into a table that by design cannot be edited or deleted. There is no way back
from that.

**Reads.** One exception is worth making: `ListPrincipalsForInstance` reads who has access to a
monitored database, and reading that is itself a security-relevant act. It is audited. Nothing else
on a read path is.

**Retention of `audit_log`, which B5 makes an obligation rather than an afterthought.** The
arithmetic first: fifty instances with a nightly backup, a verification and an observation poll is
roughly 150 rows a day, about 55,000 a year, tens of megabytes. It is not the bucket.

But "not the bucket" is not "bounded", and the honest answer is that **pruning an append-only table
is a decision of its own and B6 should not make it.** `DELETE` is refused by the trigger, which is
the entire point of the trigger. The mechanism that does not fight it is monthly range partitioning
and `DROP PARTITION`, which is a schema change with its own failure modes and its own ADR. B6 ships
the numbers, an index that makes an age query cheap, and a line in `STATUS.md` saying the table
grows and by how much. A retention that can delete evidence is the thing the trigger exists to
prevent, and adding one in the same slice that creates the evidence is the wrong order.

### 6. What happens to the UI and the CLI on the day this lands

Three clients read `/api/v1/` with no credential today, and all three stop working the moment
enforcement is real.

**The CLI** gets `--token`, `FLEETWARD_TOKEN` and `FLEETWARD_TOKEN_FILE`, sent as
`Authorization: Bearer`, with the file form documented as preferred for the same reason `keygen`
already gives. A 401 renders as an error that says how to get a token rather than as "control plane
returned 401".

**CI's compose smoke test** keeps working because `/healthz` and `/readyz` stay unauthenticated —
they have to, `--wait` depends on them — and because compose gains a development bootstrap token
beside the Dex settings that are already there. **Recommendation: `docker-compose.yml` flips to
`FLEETWARD_AUTH_ENABLED: "true"`.** A quickstart with authorization off would mean the one thing
this slice builds is never exercised by CI, and never being exercised by CI is the exact mechanism
that produced the false SECURITY.md claim ADR-0024 was written about. The smoke test gains one step
that calls an authenticated route with the dev token and one that calls it without and expects 401.

**The UI: no login form, and not credential-less either.** A login form implies local passwords,
which ADR-0008 forbids outright. Credential-less behind a flag would mean the product's only screen
works only on installations with authorization turned off.

**Recommendation: the UI gets a token exchange and an identity, and nothing else.** An unauthorised
response sends the user to one screen that asks for an API token, posts it to
`POST /api/v1/sessions`, and thereafter holds nothing — the cookie is `HttpOnly` and JavaScript
cannot read it. The header shows the display name and the effective role from `GET /api/v1/me`, and
a sign-out that clears the session.

That screen says, in plain words, that this is an interim credential and that sign-in through an
identity provider arrives in B10. B4's rule was that a UI implying a protection it does not have is
worse than one that says nothing; the converse applies now that the protection is real, and saying
what kind of credential this is remains the honest version of it.

---

## Design decisions already made — do not relitigate

- **AuthN is OIDC, AuthZ is four ordered roles scoped environment → instance, enforcement is
  server-side on every route without exception, and `tenant_id` is on every table from day one**
  (ADR-0008). The UI hiding an action is a courtesy, never a control.
- **OIDC itself is B10, not B6.** This slice builds the spine; B10 swaps the identity provider in
  behind it.
- **Production readiness is a property of this slice** (ADR-0024). What this slice enforces ships
  with its limits and its escape hatches, here.
- **§7.5 of `CLAUDE.md` is the acceptance criterion and its numbering is load-bearing**: `viewer`
  cannot trigger backup/restore (403, server-side); `dba` can; every mutating action lands in
  `audit_log`.
- **No gRPC listener** (ADR-0019). Enforcement lives where the requests actually arrive.
- **The four roles and their ranks come from the database**, not from a Go constant block.
- **This is not access compliance** (ADR-0017). That is about principals *on the monitored
  databases*, it is read-only, it generates remediation SQL a human runs, and it is deferred behind
  the engines. Same word, unrelated feature, and the two must not acquire shared code.

## Files

### New

| Path | Purpose |
|---|---|
| `internal/storage/metadb/migrations/000004_api_tokens.up.sql` / `.down.sql` | `api_tokens`; an `audit_log` index supporting an age query |
| `internal/controlplane/authn/authn.go` | `Principal`, `Authenticator`, the chain, the context helpers |
| `internal/controlplane/authn/token.go` | Token minting, hashing, lookup, the short-TTL cache |
| `internal/controlplane/authn/session.go` | Signed session cookie, `SessionTTL`, sign-out |
| `internal/controlplane/authn/bootstrap.go` | The configuration-only bootstrap principal |
| `internal/controlplane/authz/policy.go` | The RPC → (minimum role, scope source) table |
| `internal/controlplane/authz/guard.go` | Grant resolution, maximum-rank rule, `Check` |
| `internal/controlplane/authz/scope.go` | Scope extraction from a request message by reflection |
| `internal/controlplane/authz/decorator_*.go` | One decorator per served service |
| `internal/controlplane/authz/coverage_test.go` | The ADR-0024 §4 reflection test |
| `internal/controlplane/audit/audit.go` | The append-only writer and its allow-listed `details` |
| `internal/controlplane/identity/` | Users, grants, tokens: create, list, revoke; `me` |
| `cmd/fleetward-cli/token.go`, `audit.go` | `token generate/create/list/revoke`, `audit list` |
| `web/src/routes/sign-in.tsx` (or equivalent) | Token exchange screen and the identity header |
| `docs/ops/authorization.md` | Roles, scopes, tokens, bootstrap, what is audited |
| `docs/adr/0033-…` … `0036-…` | See "Done when" |

### Modified

| Path | Change |
|---|---|
| `api/proto/fleetward/v1/controlplane.proto` | `IdentityService`: `CreateSession`, `GetMe`, `CreateToken`, `ListTokens`, `RevokeToken`, `ListAuditLog` |
| `internal/controlplane/api/server.go` | Authentication in the middleware; `/healthz` and `/readyz` exempt |
| `cmd/fleetward/main.go` | Build the authenticator and guard; wrap each service in its decorator |
| `internal/config/config.go` | `Auth.BootstrapToken`, `BootstrapTokenFile`; the production validations |
| `internal/controlplane/{backup,inventory,scheduler}/*.go` | `s.tenantID` → `tenantOf(ctx)`; audit calls; `triggered_by` written |
| `cmd/fleetward-cli/client.go`, `main.go` | Token flags, bearer header, a 401 that explains itself |
| `web/src/lib/api.ts` and the estate view | 401 handling, `credentials: "same-origin"`, the header |
| `docker-compose.yml`, `.env.example` | `AUTH_ENABLED: "true"` and a development bootstrap token |
| `.github/workflows/ci.yml` | Smoke test: an authenticated call and a 401 |
| `.github/SECURITY.md` | The "no authentication or authorization yet" paragraph, corrected |
| `README.md`, `docs/dev/STATUS.md`, `tools/wikigen/manifest.go` | The front door, the position, the new page |

## Reuse, do not rewrite

- **`telemetry.WithRequestID` / `RequestIDFrom`** (`internal/telemetry/`) is the exact pattern for
  carrying a principal on the context. Follow it; do not invent a second one.
- **`api.WriteProblem` and `problemErrorHandler`.** 401 and 403 already render correctly through
  `codes.Unauthenticated` and `codes.PermissionDenied`. No new error shape.
- **`clientIP(r)`** (`server.go`) already produces what `audit_log.source_ip` wants.
- **`secrets.GenerateMasterKey` and the `keygen` command** are the template for `token generate`:
  stdout alone so it can be redirected, the warning on stderr, the file-over-environment reasoning
  in the help text.
- **`config.LogValue`** already redacts every secret. Add the bootstrap token to it; do not build a
  second redactor.
- **`inventory.GRPCServer`'s translation-only shape** is what the decorators must not violate. No
  logic in a decorator beyond the check.
- **`metadb.DefaultTenantID`** stays, for the seed and for system principals. It stops being a
  service field.
- **B5's `RetentionPolicy` threading through `main.go`** shows how a cross-cutting policy object is
  passed without a global. The guard follows it.

## Traps

- **`audit_log` refuses `UPDATE` and `DELETE` by trigger.** A test that cleans up after itself fails,
  and so does a migration correcting a row. `TRUNCATE` is *not* blocked — the trigger is
  `FOR EACH ROW` — so a test harness resets with `TRUNCATE`. Discovering this in CI is the avoidable
  version.
- **A request message can contain a database password.** Never marshal one into `details`. The
  allow-list is the mechanism; a helper that takes `proto.Message` and produces JSON must not exist,
  because somebody will call it.
- **The credential must never reach a log.** `Server.middleware` logs method, path and remote
  address today. A token in a query string, or a header dumped on error, ends that property. Tokens
  are accepted from the `Authorization` header and the session cookie only — never from a query
  parameter, never from a body — precisely so no logging change can leak one.
- **`role_grants_single_scope` means a grant is never two scopes at once.** The resolver must handle
  a user with grants at all three levels, and the maximum-rank rule must be tested with the
  `dba`-on-environment / `viewer`-on-instance pairing specifically, because that is the one where
  "most specific wins" gives the opposite answer.
- **`roles` is seeded and `role_grants.role_name` is `ON DELETE RESTRICT`.** Read the ranks from the
  database. A Go constant that disagrees with the table is a bug that will not surface until
  somebody edits one of them.
- **Authorization on the hot path.** The estate view refetches every thirty seconds across fifty
  rows. Resolve grants once per authentication, cache the principal by token hash, and measure it.
- **`SystemService` is declared, unimplemented and unregistered.** The reflection test must decide
  deliberately what it does about a service with no implementation rather than crashing on it, and
  the decision must be a line of code with a comment, not an omission.
- **The scheduler and the retention sweep have no request context.** Every path that reaches a
  service from `scheduler.go` or `retention.go` must attach a system principal, or `tenantOf(ctx)`
  will fail them. That is 100% of the automatic work in the product; it is where the tenant
  refactor breaks if it breaks.
- **`/healthz` and `/readyz` must stay unauthenticated.** `docker compose --wait`, the CI smoke test
  and `fleetward-cli health` all depend on them, and a readiness probe that needs a credential is a
  readiness probe that reports a credential problem as an outage.
- **A cookie means CSRF.** `SameSite=Strict` is sufficient while the UI performs no mutations, which
  it does not in B6. The first mutating screen needs a double-submit token, and that sentence
  belongs in `docs/ops/authorization.md` rather than in a future session's incident.
- **Do not confuse this with access compliance** (ADR-0017).
- **`make` is not installed on this machine.** Run the targets directly and say so rather than
  reporting `make lint test` as passing.
- **On Windows, `gofmt -l`, `buf format --diff` and golangci-lint's `whitespace` linter report
  `core.autocrlf` artefacts.** Verify in a worktree created with
  `git -c core.autocrlf=false worktree add` before believing any of them. B5 found two *real* gofmt
  findings hiding in that noise. `go test -race` needs cgo, which a stock install does not have.
- **This slice touches `api/proto/`, so regenerate in an LF worktree.** The OpenAPI generator embeds
  `.proto` comments as YAML strings, so a CRLF checkout writes literal escaped newlines that no
  line-ending normalization removes and `git diff` does not show. The `grep -cF` check in "Done
  when" must print 0. `make proto` also regenerates `web/src/lib/api.gen.ts`; CI diffs both.
- **Run `go vet -tags=integration ./...` and `go vet -tags=conformance ./...` over the whole tree
  before pushing.** B3 lost a CI cycle to a test stub in an untouched package; B4 and B5 each caught
  the same thing in seconds. A change to a service constructor signature is exactly that shape.
- **Do not run `make conformance` and the integration suite at the same time.** Both start
  containers, they contend for Docker, and the result is a screenful of failures that are not real.
- **Two integration tests fail on this machine and neither is a regression.** Both are in
  `STATUS.md`'s environment notes and both reproduce on `main`.
- **The `web` image sometimes fails to build here** with `failed to prepare extraction snapshot …
  parent snapshot does not exist`. Docker Desktop's fault; `docker builder prune` clears it.
- **Run `go mod tidy` before pushing**, and `npm ci` rather than `npm install` when touching
  `package.json`.
- **`make docs` and `make docs-check`.** The configuration reference is generated from
  `internal/config`, so new settings must be regenerated and committed, and a new page under `docs/`
  fails CI until it is in `tools/wikigen/manifest.go`.

## Scope fence

In: the authorization spine and whatever decisions 1–6 settle.

Not in this slice:

- **OIDC itself** (B10), and any Dex integration beyond the settings already in compose.
- **Alert rules and delivery** (B7); **self-observability and metrics** (B8); **the release
  artifact** (B9). A 403 emits no metric here.
- **`DeleteBackup`**, or any way to reclaim what B5's retention floor pins.
- **Honouring `DeleteInstance(delete_artifacts=true)`**, and re-stamping retention on existing
  backups.
- **The query editor** (ADR-0018), whose gate this slice opens and does not walk through.
- **A user-and-role management UI**, or any new screen beyond what decision 6 requires to keep the
  estate view working. Users, grants and tokens are CLI-only.
- **Audit log pruning.** Decision 5 says why, and says what it would take.
- **Rate limiting, lockout, and token expiry policy** beyond a plain optional `expires_at`.
- **A second tenant.** The tenant stops being a constant; nothing creates a second one.
- **Sweeping a plugin's leftover file on a shared directory** (B2's journal, still open).
- **Widening the job reaper to catch a `running` job with no lease** (B3's journal, still open).
- **The `verify` job that reads `succeeded` after a `FAILED` verification** (B2's journal, carried
  through B4 and B5).
- **The remaining five engines** (B11–B16).

## Done when

Concrete, in the order they are run.

```
go build ./...
go vet ./...            && go vet -tags=integration ./... && go vet -tags=conformance ./...
go test ./...                                  # includes the authz coverage test
go test -tags=integration ./...                # bar the two known machine failures
go test -tags=conformance ./test/conformance/...
go run ./tools/docscheck
go mod tidy                                    # no drift
golangci-lint run                              # in an LF worktree
buf lint && buf format --diff && buf breaking --against '.git#branch=main'
npm run lint && npm run build && npm run test
grep -cF '\r\n' api/openapi/openapi.yaml       # prints 0
```

And, on a real stack — **the walk, which is not optional.** B3's found two defects, B4's two, B5's
one, all four in seams every other check passed. This one must show:

1. `docker compose up --build` with `AUTH_ENABLED=true`; `/readyz` green and unauthenticated.
2. An unauthenticated `GET /api/v1/instances` returns **401** in the problem-details shape.
3. A `viewer` token refused a backup with a real **403**, server-side, and the matching
   `audit_log` row with `succeeded = false`.
4. A `dba` token allowed the same backup, and its `audit_log` row with `succeeded = true` and a
   non-NULL `user_id` — and `backups.triggered_by` no longer NULL.
5. A `dba` grant on one instance that does **not** carry to its neighbour: allowed on one, 403 on
   the other, both audited.
6. The `dba`-on-environment / `viewer`-on-instance pairing resolving to `dba`, demonstrated.
7. A scheduled backup running with `actor = "system:scheduler"` and `user_id` NULL, and a retention
   sweep deleting an artifact under `actor = "system:retention"`.
8. `UPDATE audit_log SET succeeded = true` refused by the trigger, by name.
9. The CLI working end to end with `FLEETWARD_TOKEN_FILE`, and a 401 that explains itself.
10. The estate view working: token exchanged, cookie set, identity in the header, sign-out clearing
    it — and the browser's `document.cookie` **not** containing the session.
11. `docker compose logs fleetward` searched for the token, finding nothing.

Close-out, per the protocol: `STATUS.md` rewritten, a journal entry at
`docs/dev/journal/B6-authorization-spine.md`, `README.md` and `.github/SECURITY.md` corrected, and
ADRs for the four things a future session could reasonably undo — **the bootstrap credential is
configuration and never a row**, **grants are additive and the maximum rank wins**, **enforcement is
a policy table and a decorator that fails to compile when the contract grows**, and **the scheduler
is an actor string rather than a user**.
