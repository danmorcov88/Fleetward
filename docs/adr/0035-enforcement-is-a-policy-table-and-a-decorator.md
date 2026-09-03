# ADR-0035: Enforcement is a policy table and a decorator, scope comes from the request, and a refusal that names somebody is audited

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B6 — the authorization spine
- **Relates to:** [ADR-0008](0008-oidc-rbac-multitenancy.md),
  [ADR-0019](0019-rest-api-without-a-grpc-listener.md),
  [ADR-0024](0024-production-readiness-is-a-slice-property.md),
  [ADR-0034](0034-grants-are-additive-and-the-highest-rank-wins.md)

## Context

Requests reach a service as in-process grpc-gateway handlers with no gRPC listener
([ADR-0019](0019-rest-api-without-a-grpc-listener.md)). The generated registration function says so
in as many words: *"GRPC interceptors will not work for this type of registration."* So the one
enforcement point a normal gRPC server would have does not exist here.

Three shapes were available, and the choice is not really about paths:

- **HTTP middleware on the path.** One line, and blind. `POST /api/v1/instances` carries its
  `environment_id` in a body, and `POST /api/v1/backups/{backup_id}/verify` names a backup whose
  scope is the instance behind it. A middleware sees neither.
- **A check inside each service method.** Sees everything, and is 24 call sites that a 25th can
  forget — with no mechanism that notices.
- **A table keyed on the RPC, applied by a decorator.**

There is a second question underneath: scope is environment → instance, so *where does a request's
scope come from*, and what does a request that names no scope at all mean?

And a third: `audit_log` carries `succeeded`, so a refusal can be recorded. Whether it should be is
a real question, because a 403 is simultaneously the most interesting row in an audit log and the
row an attacker can generate a million of.

## Decision

### Enforcement

**A policy table keyed on the gRPC full method name, applied by one decorator per service.** The
table says, for each of the 24 served RPCs: the minimum role, where the scope comes from, whether
the call mutates anything, and what word the audit log records it under. Scope is read from the
request message by protobuf reflection, so there is one extraction function rather than 24.

Three things stop a route being added and left unguarded:

1. **The embedded `UnimplementedXServiceServer` is the fail-closed default.** A method the decorator
   does not override is answered with `codes.Unimplemented`; the real service behind it is never
   reached. The request fails, loudly, rather than being served without a check.
2. **A method with no entry in the table is denied to everybody**, administrators included, until
   somebody writes down what it needs.
3. **A coverage test enumerates every method of every generated service interface by reflection** —
   no list of route names to fall out of date — and calls each one with an anonymous caller,
   asserting `Unauthenticated`. Because an undecorated method answers `Unimplemented` instead, the
   same assertion that proves every route needs a credential also proves every route is wrapped.
   This is the test [ADR-0024](0024-production-readiness-is-a-slice-property.md) §4 asked for.

### Scope

**Scope comes from the request, and a request that names no scope is asking about the whole
tenant.**

`ListBackups` with an `instance_id` is a question about one instance, and an instance-scoped grant
answers it. `ListBackups` with nothing set is a question about the estate, and needs a grant that
covers the estate. One rule, no per-endpoint special cases, and no list endpoint can return a row
from outside the caller's scope.

A tenant-wide grant is checked first and is the only path that performs no query at all — which is
the estate view's path, sixty times a minute across fifty rows. An environment lookup happens only
when the caller actually holds an environment-scoped grant.

### What is audited

**A refusal is audited when it names a principal, and not otherwise.**

- **401 is not audited.** It names nobody, so the row would carry nothing an investigation could
  use, and it is exactly the row an attacker can generate a million of. It goes to the access log,
  which records the actor on every line.
- **403 is audited**, with `succeeded = false`, whatever the method. Somebody who *is* somebody
  reached for something they may not have. It is bounded by the number of issued credentials.
- **Every mutating action is audited on both outcomes.** An action that was allowed and then failed
  is a row with `succeeded = false`, like a refusal, because the log records attempts.
- **One read is audited**: `ListPrincipalsForInstance`. Reading who has access to a monitored
  database is a security-relevant act even though it changes nothing.

**`details` is assembled from an allow-list, and no function in the audit package takes a request
message.** That absence is the mechanism, not a convention. `CreateInstanceRequest` carries a
production database password; a convenient "log the request for context" would write it into a table
that by design cannot be edited or deleted, and there is no way back from that.

**A refusal tells the client one sentence, whichever way it was refused.** Not whether the resource
exists, not how close the caller's role was, not which check failed. All of that is in the server's
log and in the audit record; each of it is a probe an unauthorized caller could repeat.

## Consequences

**The whole authorization surface is one file a reviewer reads top to bottom.** That was the main
thing worth buying: "which routes need `dba`" is answerable by reading, not by grepping 24 service
methods.

**The compile-time barrier the slice brief wanted is not available, and the reason is worth
recording.** A decorator that omitted the `Unimplemented` embed would stop compiling the moment the
contract grew a method — the strongest possible version of this, and the shape of B5's `CHECK`
constraint. It needs `require_unimplemented_servers=false`, which is a global generator setting, and
turning it off globally would make every additive change to `plugin.proto` break every third-party
plugin at compile time. That is precisely the forward compatibility `CONTRIBUTING.md` promises plugin
authors, so the runtime default plus the coverage test is what replaces it.

**A scoped grant cannot enumerate the estate.** An instance-scoped `viewer` gets 200 on
`GetInstance` for their instance and 403 on `ListInstances` with no filter. That is restrictive
rather than permissive — the safe direction — and it is the honest consequence of not implementing
per-row filtering of list results, which is named in `STATUS.md` rather than left to be found.

**`audit_log` grows and nothing prunes it.** Roughly 150 rows a day on an estate of fifty — a
nightly backup, its verification and an observation poll each — so about 55,000 a year and tens of
megabytes. `DELETE` is refused by `audit_log_no_update`, which is the entire point of that trigger,
so pruning needs monthly range partitioning and `DROP PARTITION`. That is a schema change with its
own failure modes and its own decision, and adding a retention that can delete evidence in the same
slice that creates the evidence is the wrong order. Migration 000004 adds the index that makes the
age question cheap to ask.

**Authorization costs one query per request at worst**, and zero for a tenant-wide grant, because
the grant set is resolved once at authentication and cached with the principal.

## Alternatives considered

**A middleware on the URL path.** One line, and structurally unable to see a scope in a body or
resolve a `backup_id` to an instance. It would have produced a system where the routes that most
need scoping are the ones it cannot scope.

**A check at the top of every service method.** Sees everything the decorator does. Rejected because
nothing detects the fifty-first method that forgets, and because it puts authorization logic inside
services whose gRPC layer is otherwise translation-only.

**Generating the decorators from the contract.** The repository already generates its configuration
reference and its wiki from source, so a generator would fit the culture. Rejected on cost against benefit: 24
one-line methods that a test proves complete are cheaper than a generator, and the generator would
have to be maintained through B10's changes.

**Filtering list results by the caller's scope instead of requiring a tenant-wide grant.** The more
useful behaviour, and the more invasive: seven queries would each grow a visible-instance filter,
including the retention preview, which is the code that decides what gets destroyed. Deferred
deliberately, and recorded so that "lists are not filtered" is understood as a decision rather than
an oversight.

**Auditing 401s too.** Rejected on the arithmetic: the rows carry no principal, and an unauthenticated
scanner would fill an append-only table nothing can prune with entries naming nobody.
