# B6 — The authorization spine, or the slice where a stranger could

- **Delivered:** 2026-09-03
- **Brief:** [B6-authorization-spine.md](../slices/B6-authorization-spine.md)

B5 was the slice where a bug could destroy data. This was the slice where a stranger could.

Every route under `/api/v1/` was open to anyone who could reach the port: adding an instance,
storing its credentials, creating a schedule, triggering a backup, triggering a restore, and — since
B5 — the retention configuration that decides what gets deleted.

**The deliverable is that every request names a caller, every route decides on that caller's role
within the scope it is acting on, and every mutating action lands in an append-only record.** The
schema for all of it had existed since migration 000001 and had never held a row.

## How it was verified

On Windows (amd64), 2026-09-03, against Go 1.25.6, Node 22.16, buf 1.58.0, golangci-lint 2.12.2,
Docker 27.3.1, `postgres:16-alpine`, `minio/minio:RELEASE.2025-04-22T22-12-26Z` and
`mcr.microsoft.com/mssql/server:2022-latest`.

```
go build ./...                     ok
go vet ./...                       ok
go vet -tags=integration ./...     ok
go vet -tags=conformance ./...     ok
go test ./...                      ok, no failures
go test -tags=integration ./...    ok, bar the two known machine failures
go test -tags=conformance ./test/conformance/...   ok, 306.4s
go run ./tools/docscheck           81 markdown files, no problems
go mod tidy                        no drift
golangci-lint run                  0 issues   (LF worktree)
buf lint / format / breaking       clean — the contract change is additive
npm run lint / build / test        clean, 22 tests
grep -cF '\r\n' api/openapi/openapi.yaml   0
```

`make` is not installed on this machine, so those are the targets run directly rather than
`make lint test` reported as passing.

## What the contract gained, and what it deliberately did not

`IdentityService`, with five RPCs: who am I, mint a token, list them, revoke one, read the record.
Five rather than four because an audit log nobody can read is a table rather than a control, and
five rather than six because **session establishment is not in the contract at all**.

`POST /api/v1/sessions` and `DELETE /api/v1/sessions` are hand-written HTTP handlers. A cookie is a
transport concern Protobuf cannot express, and the alternative — smuggling `Set-Cookie` out through
gRPC response metadata and a header matcher — is more machinery than the thing it describes. It is
the same reasoning that has always kept `/readyz` outside the contract (ADR-0029).

**`buf lint` caught the first mistake before any code existed.** The message was going to be called
`Principal`, and `Principal` already exists in `plugin.proto` — it is *access compliance*'s
principal, the accounts on a monitored database (ADR-0017). The brief listed confusing those two as
a trap; the compiler enforced it. The wire type is `Caller`, and its comment says why.

## The four decisions

[ADR-0033](../../adr/0033-the-bootstrap-credential-is-configuration-and-never-a-row.md) — what a
principal is;
[ADR-0034](../../adr/0034-grants-are-additive-and-the-highest-rank-wins.md) — how a grant resolves;
[ADR-0035](../../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md) — where enforcement
lives and what is audited;
[ADR-0036](../../adr/0036-the-scheduler-is-an-actor-and-not-a-user.md) — what the scheduler is, and
the tenant.

Three of them are worth restating here, because what they cost was not what the brief expected.

### The bootstrap credential is configuration, and that is the whole point

The first credential on a fresh installation cannot come from a CLI command that writes a row: the
CLI never opens the metadata database, deliberately, so authorization is not duplicated in a second
place. So it comes from `FLEETWARD_AUTH_BOOTSTRAP_TOKEN`.

The tempting alternative was a seeded administrator row on first start. It would have been friendlier
and it ages badly: a row survives the operator removing every environment variable, is invisible in
a diff, and can only be revoked by somebody who knows it exists.

**Nothing is stored, so there is nothing to revoke.** Delete the setting, restart, and the access is
gone with nothing left behind to find in a year. Every action it takes is audited under
`actor = "bootstrap"`, which is visibly not a person, and the control plane logs a warning naming it
on *every* start rather than once — because a break-glass credential nobody is reminded about stays
configured for a year.

The cost is stated rather than hidden: while the setting is present, whoever holds that string is a
tenant-wide administrator, and losing every admin token means editing configuration to recover. A
permanent recovery path is a permanent way in.

### Grants add up, and the case that decides it is the unintuitive one

`role_grants_single_scope` means a grant covers one instance, one environment, or the whole tenant,
never two. So what happens when somebody holds `dba` on an environment and `viewer` on one instance
inside it?

Under "most specific wins" — the rule almost everybody expects, and how file permissions work — the
narrow grant *demotes* them there. That turns `role_grants` into a deny mechanism the schema has no
column to express, no ordering for, and no way to state explicitly. A deny that exists only as an
emergent property of a resolution rule is a security control that looks like it works, works by
accident, and stops working the day somebody adds a second grant at the same level.

So: **the maximum rank of every grant covering the request wins, and a grant only ever adds.** The
integration test asserts both pairings, and the one that would fail under the other rule says so in
its failure message.

The cost is real: there is no way to say "this person, but not on that one server". The answer is to
grant instance by instance instead of granting the environment, and that sentence is in
`docs/ops/authorization.md` rather than left to be discovered.

### The compile-time barrier the brief wanted does not exist, and the reason matters

The brief specified a decorator that omitted the generated `UnimplementedXServiceServer` embed, so
that `var _ fwv1.InventoryServiceServer = (*inventoryGuard)(nil)` would stop compiling the moment
the contract grew a method. That is the strongest possible version of this and it is the exact shape
of B5's `CHECK` constraint: the mistake refused by the toolchain rather than caught by a reviewer.

It is not available. `protoc-gen-go-grpc` generates its service interfaces with
`require_unimplemented_servers` on, so every implementation *must* embed it. Turning the option off
is global, and turning it off globally would make every additive change to `plugin.proto` break
every third-party plugin at compile time — which is precisely the forward compatibility
`CONTRIBUTING.md` promises plugin authors.

What replaces it is two things, and the second one is better than it looks:

- **The embed is fail-closed rather than a hole.** A method the decorator does not override is
  answered by the embedded Unimplemented with `codes.Unimplemented`. The request is refused and the
  real service is never reached.
- **The coverage test does double duty.** It enumerates every method of every generated service
  interface by reflection and calls each one with an anonymous caller, asserting `Unauthenticated`.
  An undecorated method answers `Unimplemented` instead — so the same assertion that proves every
  route needs a credential also proves every route is wrapped, and the failure message says which
  file to go and edit.

The brief was wrong about a mechanism and right about the property. Recorded here because the next
session to read ADR-0035 will otherwise wonder why the obvious thing was not done.

## The tenant stops being a constant

Four services held `metadb.DefaultTenantID` as a field and dereferenced it at 72 sites, almost all
of them in the code that deletes data. ADR-0008 put `tenant_id` on every table from the first
migration specifically so this would never be a migration, and the claim had never once been
exercised.

The substitution is mechanical and compiler-checked: `s.tenantID` became `authn.Tenant(ctx)`, and
**not one query changed** — B5's retention SQL is byte-identical. What moved is where the value
comes from.

The failure mode is what made it worth the risk. `authn.Tenant` returns the empty string when there
is no principal, every query filters `tenant_id = $1` against a UUID column, and Postgres rejects
the statement outright. A path that forgets a principal fails on its first query. The alternative
failure — a constant quietly serving one tenant's rows to another — is silent, and would have been
found by a customer.

**The place it nearly went wrong was the background half of a backup.** A backup outlives the
request that asked for it, so the work continues on a context the service owns, which had no
principal at all. The first version attached one carrying `DefaultTenantID` — reintroducing the
constant in the one place nobody would look for it. It now takes its *lifetime* from the service and
its *tenant* from the caller, and `background(ctx)` says so in a comment.

## What is audited, and the row that is not

A refusal is audited when it names a principal, and not otherwise.

- **401 writes nothing.** It names nobody, so the row carries nothing an investigation could use,
  and it is the row an attacker generates a million of. The access log records the actor on every
  line instead.
- **403 always writes a row**, whatever the method, with `succeeded = false`. Somebody who *is*
  somebody reached for something they may not have, and it is bounded by the number of issued
  credentials.

**The rule that `details` never holds a credential is enforced by an absence.** There is no function
in the audit package that takes a `proto.Message`. `CreateInstanceRequest` carries a production
database password; a convenient "log the request for context" would write it into a table that by
design cannot be edited or deleted, and there is no way back from that. The package comment says so,
and a unit test asserts that the only reader of a request message the audit path has cannot reach a
password field.

## The dev stack now runs with authorization on

It used to be off, so the quickstart needed no login round trip. The cost was that the enforcement
was never exercised by anything — not by a developer, not by CI — and never being exercised by CI is
the exact mechanism that once let `.github/SECURITY.md` claim an authorization layer that had never
been built (ADR-0024).

So `docker-compose.yml` sets `FLEETWARD_AUTH_ENABLED: "true"` and a known bootstrap token, and the
smoke test gained two steps: an unauthenticated `GET /api/v1/instances` must return **401**, and an
authenticated one must return an estate and a `highest_role` of `admin`.

`FLEETWARD_AUTH_ENABLED=false` still works and still makes everybody an administrator. It is an
escape hatch shipped with its limits, which is what ADR-0024 asks of every slice, and it now records
its own use: those rows read `actor = "auth-disabled"`, a different word from `bootstrap` because
"somebody used the break-glass credential" and "this installation was not asking anybody who they
were" are different facts.

## The UI got an identity and not a login

A login form implies local passwords, which ADR-0008 forbids outright. Leaving the UI
credential-less behind a flag would have meant the product's only screen worked only where
authorization was turned off.

So it asks for an API token once and posts it to `POST /api/v1/sessions` in exchange for an
`HttpOnly` cookie. **The browser never holds a token in JavaScript**, because anything a script on
the page can read is something an injected script can steal, and a Fleetward token can restore a
production database.

The test that matters asserts the negative: after a successful exchange, neither `localStorage` nor
`sessionStorage` holds anything, under any key, and the input is cleared. A future change that
"helpfully" remembered the token would break nothing visible, which is exactly the regression a test
has to catch.

The screen says in plain words that this is an interim credential and that sign-in through an
identity provider is next. B4's rule was that a UI implying a protection it does not have is worse
than one saying nothing; the converse applies now that the protection is real.

## The five defects, and where each was caught

Two suites and one walk, each finding what the others structurally could not.

### The unit tests could not see it: an audit row naming a person who does not exist

The first run of the integration suite against a real Postgres failed on one assertion:

```
details.effective_role = "", want viewer
```

A caller holding a tenant-wide `viewer` grant, refused a backup, was recorded in the append-only
audit log as **holding no role at all**.

The tenant-wide rank was computed, compared against what the method required, and then *discarded*
when it was not enough. Everything after that looked only at instance- and environment-scoped
grants, found none, and reported a caller who does not exist.

Every authorization outcome was correct — a tenant-wide grant lower than the requirement cannot
change a decision — so nothing failed. The only symptom was a row in a table that cannot be edited
afterwards, describing the wrong person, in the exact record an investigation would rely on. The fix
is three lines, and it is also the additive rule stated properly: every grant covering the request
counts, whether or not it is enough.

### The walk found the rest, and the first of them was total

**The bootstrap credential did not work. At all. On any installation, with any value.**

The chain was session → bearer token → bootstrap, on the reasoning — written into an ADR — that a
revoked token must not fall through to whatever comes next. Both halves of that reasoning are right.
The conclusion was wrong, because of an asymmetry nobody had noticed:

> A link that compares against one known string can say *"not mine"*. **The token store cannot.** A
> bearer value it has never seen is an *invalid credential*, not somebody else's.

And a bootstrap token is, by definition, a bearer value the database has never seen. The store
declined it, the chain stopped as designed, and the credential whose only job is to get a fresh
installation started could never be used. `docker compose up` produced an installation nobody could
log into.

The fix is one line of ordering. `authn_test.go` now pins both directions, and ADR-0033's paragraph
about the ordering — which argued confidently for the wrong one — is corrected in place.

### `token list` understated what a credential could do

It showed the *last* grant in the list. A person holding `dba` on an environment and `viewer` on one
instance inside it was listed as a **viewer**.

The same family as the first defect and the more dangerous direction: an administrator reviewing
credentials would have read less access than the credential actually had. It shows the strongest
grant now, with a count of the others, and `whoami` prints them all — which is what somebody reads
after a 403.

### The audit log said `instance` beside a backup's id

`resource_id` was "the first id-shaped field on the message", and a create's response overrode it.
For `backup.run` that produced rows reading `resource_type = instance`, `resource_id = <a backup>`.

An investigator filtering on an instance would have missed every backup ever run against it, and
every row would have looked perfectly plausible while they did. The mapping from resource type to
request field is now explicit and validated at startup; what a call *created* goes in
`details.created`, and what it *acted on* stays in `resource_id`.

The same pass caught `details.scope` reading `tenant` for a request that plainly named one instance —
true of the *grant* that authorized it, misleading as written. It is `authorized_by` now, and says
what it means.

### The one that mattered most: a scoped grant could not be used

The rule "a request naming no scope is asking about the whole estate" is sound, and applied without
exception it did not restrict a scoped caller — it locked them out.

The CLI resolves an instance *name* by listing instances. The estate view **is** a listing. So a
person granted `dba` on the three servers they operate could not see them, could not name them, and
could not do the one thing they had been granted.

A rule whose effect is that a granted permission cannot be exercised is not conservative; it is
broken. `ListInstances` and `GetBackupAdherence` now filter their rows — a caller holding the role
anywhere may call them, and gets back exactly what their grants cover. The other listings still take
a tenant-wide grant or an explicit `--instance`, which is usable.

The tests are on the rows and on the page total rather than on the flag that enables the filtering,
because a `total_size` of three beside one visible row would tell a scoped caller precisely how many
servers they are not allowed to see.

### And §7.5 was two thirds unmet

The guard in front of the API records every request. The scheduler and the retention sweep are not
requests: they reach the services directly, and **wrote nothing at all**.

So "every mutating action lands in `audit_log`" held for the third of the work a human asks for and
failed for the two thirds Fleetward does on its own — including the one question an operator asks
first about a missing backup. `auditAutomatic` records an action only when the caller is a system
principal, so a request is never recorded twice, and the record now sits in `createRows`, where both
the API path and the scheduler's path meet. The retention sweep records `backup.expire` beside the
bytes it deleted.

The walk demonstrated all of it against the running stack: `system:scheduler` rows appearing a minute
apart, and `system:retention` naming five artifacts with their object keys and sizes.

## What the tests are for

The unit suite proves the shape with no database: every route has a policy, every route refuses an
anonymous caller with `Unauthenticated` rather than `Unimplemented`, every route needing more than
`viewer` refuses one — and each refusal writes exactly one audit row while each unauthenticated
request writes none. `authn`'s own tests pin the chain's ordering in both directions, which is the
regression test for the defect that made the whole product unusable.

The integration suite proves the parts that are rows: that the ranks the policy table names are the
ranks migration 000001 actually seeded; that a `dba` grant on one instance does not reach its
neighbour; both directions of the environment/instance pairing, with the one that would fail under
"most specific wins" saying so in its failure message; that a filtered listing returns the caller's
rows and a page total that agrees with them; that an unknown resource is refused rather than
reported missing; that the retention sweep records what it deleted and a request is never recorded
twice; and that `audit_log` still refuses `UPDATE` and `DELETE` — with `TRUNCATE` asserted to still
work, because it is the only reason a test harness can reset the table at all.

The web suite asserts one negative that nothing else could: after a successful token exchange,
neither `localStorage` nor `sessionStorage` holds anything under any key.

## Still open

- **Most listings are not filtered per row.** `ListInstances` and `GetBackupAdherence` are, because
  without them a scoped grant was unusable; `ListBackups`, `ListSchedules` and `ListJobs` are not,
  and need a tenant-wide grant or an explicit `--instance`. The row-level filter lives in the
  services rather than the guard — a guard can only answer yes or no to a whole request — so each
  filtered listing is a place the filter could be forgotten. Named in `STATUS.md` and in
  `docs/ops/authorization.md`.
- **A session outlives the revocation of the token it was minted from**, until `AUTH_SESSION_TTL`
  expires it. The session is a signature rather than a row, which is what keeps a table from growing
  and what makes this true.
- **A revoked token keeps working for up to `AUTH_PRINCIPAL_CACHE_TTL`** on a replica that did not
  perform the revocation. Fifteen seconds by default, refused above five minutes.
- **`audit_log` grows and nothing prunes it** — about 55,000 rows a year on an estate of fifty.
  `DELETE` is refused by the trigger, which is the point of the trigger, so pruning needs monthly
  partitioning and its own decision. Migration 000004 adds the index that makes the age question
  cheap; nothing else about it is built.
- **There is no rate limiting and no lockout.** A token is 128 bits of entropy, so guessing is not
  the threat; a flood of 401s is a denial-of-service question that belongs in front of the control
  plane.
- **A restart signs everybody out** unless `AUTH_SESSION_KEY_FILE` is set. Right default for one
  node, and the reason a second replica has to configure it.
- **No user-and-role management UI.** Tokens, users and grants are CLI-only, and the fence said so.
- **OIDC is not here.** B10, behind the seam this slice built: one more `Authenticator` and one
  branch in the session handler.
- **A `verify` job whose verification returned `FAILED` still reads `succeeded` in `job list`.**
  Carried from B2 through B4 and B5, and not fixed here either.
- **A job left `running` with no lease is still invisible to the reaper.** Named in B3's journal.
- **A backup file left on a plugin's shared directory is still not swept.** Named in B2's journal.
- **`DeleteInstance(delete_artifacts=true)` is still unimplemented**, and `DeleteBackup` still does
  not exist — though the RBAC and audit record they were waiting on now do.
- **Two integration tests fail on this development machine and neither is a regression.**
