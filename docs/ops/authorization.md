# Authentication, authorization, and the audit log

Who can reach Fleetward, what each of them may do, and where the record of it goes.

Every setting named here is documented in [`configuration.md`](configuration.md). The decisions
behind the design are [ADR-0008](../adr/0008-oidc-rbac-multitenancy.md),
[ADR-0033](../adr/0033-the-bootstrap-credential-is-configuration-and-never-a-row.md),
[ADR-0034](../adr/0034-grants-are-additive-and-the-highest-rank-wins.md),
[ADR-0035](../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md) and
[ADR-0036](../adr/0036-the-scheduler-is-an-actor-and-not-a-user.md).

---

## The shape of it

**Every route under `/api/v1/` requires a credential and a role, enforced on the server.** The UI
hides what you cannot do as a courtesy; hiding is never the control. `/healthz`, `/readyz`,
`GET /api/v1/version` and `POST /api/v1/sessions` are the exceptions, and each has to be: a
readiness probe that needed a credential would report a credential problem as an outage, and a
sign-in that needed one could never be reached.

Two kinds of credential exist, for two kinds of caller:

| Caller | Credential | How it gets one |
|---|---|---|
| A person in a browser | a session cookie, `HttpOnly` | pastes a token once at the sign-in screen |
| A script, a cron job, the CLI | an API token, `Authorization: Bearer` | issued by an administrator |

The browser never holds a token. It exchanges one for a cookie it cannot read, because anything a
script on the page can read is something an injected script can steal, and a Fleetward token can
restore a production database.

> **Planned.** Sign-in through your own identity provider (OIDC) arrives in a later slice. It plugs
> into the same session endpoint and changes nothing about roles, grants or the audit log.

## The four roles

Seeded in the database, ordered, and not editable:

| Role | Rank | What it is for |
|---|---|---|
| `viewer` | 10 | Read-only: inventory, health, backups, adherence. |
| `operator` | 20 | May test a connection and trigger discovery. **Cannot back up or restore.** |
| `dba` | 30 | May run backups, verifications and restores within its scope, and read the retention preview. |
| `admin` | 40 | Everything, including adding instances, issuing tokens and reading the audit log. |

Adding an instance is `admin` rather than `dba`, because it stores a credential for a production
database. That is administration rather than operation.

## Scope, and the one rule that decides everything

A grant covers **one instance**, or **one environment**, or **the whole tenant** — never two of
those. The database enforces it.

Two rules follow, and both are worth knowing before issuing a grant.

**Grants add up. The highest role you hold anywhere that covers the request wins.**

So `dba` on one instance raises what you may do there even if your environment grant is only
`viewer`. And the reverse does not work: a `viewer` grant on one instance does **not** reduce a `dba`
grant on the environment around it. There is no way to say "this person, but not on that one
server"; the way to achieve it is to grant instance by instance instead of granting the environment
([ADR-0034](../adr/0034-grants-are-additive-and-the-highest-rank-wins.md)).

**Scope comes from the request, and a request that names nothing is asking about the whole estate.**

`GET /api/v1/backups?instance_id=…` is a question about one instance, and an instance-scoped grant
answers it. `GET /api/v1/backups` with no filter is a question about the estate, and needs a grant
that covers the estate. The practical consequence:

> A scoped grant can read and act on what it covers, and **cannot enumerate the estate**. Somebody
> whose only grant is on three instances gets 403 from the estate view. Give them a tenant-wide
> `viewer` alongside their scoped `dba` if they need the overview — that is the intended shape, and
> it is why grants add up.

## Getting the first credential into a fresh installation

A chicken and egg: issuing a token needs a token. The way out is a **bootstrap credential**, which
is configuration and is never stored in the database.

```bash
# 1. Generate something long and random. Any 32+ random bytes will do.
openssl rand -hex 32 > /etc/fleetward/bootstrap-token

# 2. Point the control plane at it and start.
export FLEETWARD_AUTH_BOOTSTRAP_TOKEN_FILE=/etc/fleetward/bootstrap-token

# 3. Issue yourself a real administrator token.
export FLEETWARD_TOKEN_FILE=/etc/fleetward/bootstrap-token
fleetward token create --email you@example.com --role admin > /etc/fleetward/token
export FLEETWARD_TOKEN_FILE=/etc/fleetward/token

# 4. Remove the bootstrap setting and restart. This is the step that matters.
```

**Step 4 is not optional housekeeping.** While the setting is present, whoever holds that string is
a tenant-wide administrator. The control plane logs a warning naming it on *every* start for exactly
that reason.

What it is not: a seeded administrator row. There is nothing in the database to revoke, so removing
the setting removes the access and leaves nothing behind for somebody to find in a year
([ADR-0033](../adr/0033-the-bootstrap-credential-is-configuration-and-never-a-row.md)).

Every action taken with it is recorded under the actor `bootstrap`, which is visibly not a person.

**If you lose every administrator token**, set a bootstrap token again, restart, and issue a new one.
That is the recovery path, and it is deliberately a configuration change rather than a button:
a permanent recovery path is a permanent way in.

## Issuing credentials

```bash
# A DBA responsible for one environment.
fleetward token create --email dana@example.com --role dba --environment-id <uuid>

# A read-only account for the whole estate, expiring in 90 days.
fleetward token create --email audit@example.com --role viewer --ttl 2160h

# What a credential is and what it may do.
fleetward token whoami
fleetward token list

# Stop one working.
fleetward token revoke <token-id>
```

The secret is printed **once**, on stdout alone so it can be redirected into a file, and is stored
only as a SHA-256. Nobody — not an administrator, not a database superuser — can read it back.

Prefer `FLEETWARD_TOKEN_FILE` to `FLEETWARD_TOKEN`: anything that can read the process environment
can read a variable, and on a shared machine that is a longer list than it looks. It is the same
reasoning `fleetward keygen` gives about the secrets master key.

### How long a revocation takes to bite

Immediately on the control plane that performed it, and within `AUTH_PRINCIPAL_CACHE_TTL`
(15 seconds by default) on any other replica. That cache is what keeps authorization off the hot
path of a dashboard refetching every thirty seconds across fifty rows; it is capped at five minutes
and the control plane refuses to start above that.

**A session cookie already issued is not revoked by revoking its token.** The session is a signed
statement rather than a row, so it stays valid until `AUTH_SESSION_TTL` expires it — 12 hours by
default. Lower that setting if the gap matters to you.

## The audit log

Every mutating action lands in `audit_log`, on both outcomes, and so does every refusal of somebody
who was authenticated.

```bash
fleetward audit --limit 20
fleetward audit --failures              # where an investigation starts
fleetward audit --actor dana@example.com
fleetward audit --action backup.run --resource-id <instance-id>
```

**A request that carried no credential writes nothing.** It names nobody, so the row would say
nothing an investigation could use, and it is the row an attacker can generate a million of. Those
are in the control plane's log, which records the actor on every line.

**Actors that are not people:**

| Actor | What it is |
|---|---|
| `system:scheduler` | a scheduled job the scheduler started |
| `system:retention` | the retention sweep — this is what "who deleted this artifact" answers |
| `system:backup` | the part of a backup that continues after its request returned |
| `bootstrap` | the break-glass credential from configuration |
| `auth-disabled` | this installation was not asking anybody who they were |

None of them has a credential; they are constructed inside the control plane and cannot be presented
at the port ([ADR-0036](../adr/0036-the-scheduler-is-an-actor-and-not-a-user.md)).

**`details` never contains a credential.** It carries the method, the required and effective roles,
the scope, and the outcome — assembled field by field from an allow-list. There is deliberately no
code path that puts a request message into it, because `CreateInstance` carries a database password
and this table cannot be edited afterwards.

### The table is append-only, and nothing prunes it

`UPDATE` and `DELETE` both raise, by trigger, since the first migration. An audit log that can be
edited is not evidence.

The consequence is that it grows. On an estate of fifty with a nightly backup, its verification and
an observation poll each, that is roughly **150 rows a day — about 55,000 a year, tens of
megabytes**. It is not the object store, and it is not bounded either.

> **Planned.** Pruning needs monthly range partitioning and `DROP PARTITION`, because `DELETE` is
> refused by design. That is a schema change with its own decision record, and it is not built.

## Turning authorization off

`FLEETWARD_AUTH_ENABLED=false` makes every request a tenant-wide administrator, recorded under the
actor `auth-disabled`. The control plane logs a warning at startup and **refuses to start with it in
production**.

The development stack in `docker-compose.yml` does *not* use it: it runs with authorization on and a
known bootstrap token, so that the enforcement is exercised by every developer and by CI. A
quickstart with authorization off would mean nothing ever tested it, which is how a security claim
comes to be written from the architecture rather than from the code
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known limits

Stated here rather than left to be discovered.

- **A scoped grant cannot list the estate.** Restrictive rather than permissive, and the reasoning
  is under *Scope* above. List results are not filtered per row.
- **A session outlives the revocation of the token it was minted from**, until it expires.
- **A restart signs everybody out** unless `AUTH_SESSION_KEY_FILE` is set, because the signing key is
  generated per process by default.
- **The UI performs no mutations**, so `SameSite=Strict` is the whole CSRF defence. The first
  mutating screen needs a double-submit token as well.
- **There is no rate limiting and no lockout.** A token is 128 bits of entropy, so guessing is not
  the threat; a flood of 401s is a denial-of-service question and belongs with the rest of that
  question, in front of the control plane.
- **`audit_log` grows without bound**, as above.
