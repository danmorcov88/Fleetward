# ADR-0042: Scraping metrics is a question about the whole estate

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

`/metrics` is the first route Fleetward has served that is not an RPC and not a health probe. Every
other authorized route goes through the policy table and the decorator that
[ADR-0035](0035-enforcement-is-a-policy-table-and-a-decorator.md) describes, keyed on a generated
gRPC method name. There is no method name for a scrape, and inventing one would break the coverage
test that asserts in reverse that every policy entry names a method some generated service interface
actually has — which is the check keeping that table honest.

So the endpoint needs its own answer to "who may read this", and
[ADR-0024](0024-production-readiness-is-a-slice-property.md) requires that the answer be written
down rather than left to whatever the code happens to do.

The convention in the wider ecosystem is that `/metrics` is unauthenticated. That convention comes
from services whose metrics describe themselves. Fleetward's describe an estate: a series per
instance, labelled with engine type, carrying which servers fail verification and which backups are
slow. That is a map of somebody's database estate, and B6 exists because "every route is open to
anyone who can reach the port" had already been the wrong answer once in this repository.

## Decision

**A scrape requires a credential granting tenant-wide `viewer`.**

The rule is not new. ADR-0035 already says that scope comes from the request and that **a request
naming no scope is asking about the whole tenant**. A scrape names no scope. A caller granted three
servers therefore cannot be handed an answer about fifty, for the same reason `ListBackups` with no
`instance_id` needs a tenant-wide grant.

The decision lives in `internal/controlplane/authz/scrape.go`, built from the same primitives
`Guard.Check` uses, so there is one place in the product where a role is compared against a scope.
System and bootstrap principals are allowed **by kind**, before any grant is examined, exactly as
`Check` allows them — neither holds a grant at all.

**`FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=false` serves it to anyone who can reach the port.** It warns
on every start, in the same shape as disabled authentication and a configured bootstrap credential.

**It is not refused in production**, and that asymmetry with `FLEETWARD_AUTH_ENABLED` is the
decision's second half. Disabling authentication grants control of the estate to a stranger.
Serving `/metrics` openly discloses the estate's shape. Those are different sizes of mistake, and a
configuration that treated them identically would be telling an operator something untrue about one
of them. An installation whose control plane listens only on a monitoring network has made a
legitimate choice, and refusing to start would push them towards a worse one.

**`/metrics` is on the main listener, and stays there.** A second bind address is a deployment
change — a new port to expose, a new TLS decision, a new firewall rule — and the authorization rule
above is what a separate admin port would otherwise be substituting for.

## Consequences

- A scraper needs a token. Prometheus, VictoriaMetrics, Grafana Agent and the OpenTelemetry
  collector all support `bearer_token` and `bearer_token_file`, so this costs an operator one line
  of configuration and `fleetward-cli token create` with a tenant-wide `viewer` grant.
- **The development stack scrapes with a credential rather than turning the requirement off.** That
  is deliberate and it is the same reasoning `docker-compose.yml` gives for having authorization on
  at all: enforcement nothing exercises is enforcement nobody notices is broken. It presents the
  bootstrap token, which is a known value in a public repository and fine for a stack somebody
  started on their own machine.
- A disabled endpoint answers **404**, never an empty 200. "There is no endpoint here" and "there is
  an endpoint and it knows nothing" are different answers, and a monitoring system should be told
  which it got.
- A refused scrape renders the same problem-details document as every other refusal, and is not
  written to the audit log when it presented no credential — the same rule ADR-0035 states, for the
  same reason.
- A scrape is logged at debug rather than info, alongside the health probes. Every fifteen seconds,
  forever, is not a thing an access log should be mostly made of.
- `/metrics` is not in `authz.Policies`, so the coverage test does not cover it. Its own tests do,
  including the bootstrap case, which is the one a plausible implementation gets wrong.

## Alternatives considered

- **Unauthenticated, following the convention.** Acceptable only with its reasoning written down,
  and the reasoning does not hold here: the convention assumes metrics that describe the service,
  not its customers' infrastructure. It remains available as a configured choice.
- **Any authenticated caller.** Simpler, and it makes an instance-scoped `viewer` a way to enumerate
  the estate they were deliberately not granted. The scope rule already exists; using it costs
  nothing and inventing an exception costs the rule's credibility.
- **A separate admin listener bound to localhost.** The usual answer, and a good one for a service
  with no authorization layer. Fleetward has one, and reaching for a network boundary instead of the
  layer it already built would be the weaker of two available answers.
- **A fabricated entry in the policy table.** It would put the decision beside every other decision,
  and it would break the reverse coverage check — trading a real guarantee for a cosmetic one.
