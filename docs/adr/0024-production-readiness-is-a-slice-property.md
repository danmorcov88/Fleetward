# ADR-0024: Production readiness is a property of every slice, not a phase

- **Status:** Accepted
- **Date:** 2026-09-02
- **Relates to:** [ADR-0008](0008-oidc-rbac-multitenancy.md),
  [ADR-0013](0013-internal-scheduler-with-leases.md)

## Context

The roadmap in `CLAUDE.md` §6 placed a "Phase F — Production readiness" after the engines: RBAC and
OIDC enforced on every route, a full audit log, backup retention and expiry, signed release
artifacts. Everything a deployment needs and a demo does not was collected there.

The arrangement has now produced its first concrete failure. `.github/SECURITY.md` stated
*"Authorization is enforced server-side, on every route"* and invited reports of any endpoint that
relied on the UI to restrict access. Nothing in the tree enforced anything: `cfg.Auth` is parsed and
validated but read by no file outside `internal/config`, no middleware exists, and the tenant is the
constant `metadb.DefaultTenantID`. The claim was not a lie anyone told; it was written from the
architecture as designed, in a document with no slice attached, and no merge gate could contradict
it because the behaviour it described was scheduled rather than absent.

The same shape appears elsewhere. `internal/config` validates that `AUTH_ENABLED` is true in
production and that `SCHEDULER_LEASE_HEARTBEAT` is shorter than `SCHEDULER_LEASE_TTL` — careful
validation of settings that no component consumes. The metadata schema carries `alert_rules`,
`alerts`, `notifiers`, `role_grants`, and the full lease and heartbeat columns on `jobs`, all
correct, all unread by any query. The design is consistently ahead of the code, which is a good
problem, but "Phase F" is what allows the documentation to describe the design as though it were
the code.

There is also a sequencing problem. Fleetward is meant to be installed on a server and left to
watch an estate. That is impossible without a scheduler, retention, alert delivery, and
authorization — which the roadmap distributes across Phase B and Phase F, so no prefix of the plan
produces something installable.

## Decision

**There is no Phase F.** Production readiness is not a phase; it is a property that each slice
either has or does not, and that the slice states explicitly.

Concretely:

1. **A slice that ships a capability ships its enforcement, its limits, and its operational
   story in the same slice.** Retention arrives with the scheduler that runs it, not after every
   engine exists.
2. **Documentation describes what is; slice briefs describe what will be; the two never mix in one
   file.** Forward-looking behaviour appearing in a reference document is marked `> Planned.` or it
   does not appear. `.github/PULL_REQUEST_TEMPLATE.md` carries this as a checklist item, because no
   tool can detect that a prose claim has become false.
3. **The work Phase F held is redistributed into the numbered slices** in
   [`../roadmap.md`](../roadmap.md), each placed where it first becomes load-bearing rather than
   where it is architecturally tidy.
4. **A security-relevant claim is provable or it is not made.** Where a claim can be enforced by a
   test, the test is written first. For authorization this means a reflection test that enumerates
   every method on the generated service interfaces and asserts each one performs an authorization
   check — which is what makes the SECURITY.md claim true by construction rather than by intention.

## Consequences

Access compliance and structural drift — the former Phases C and D — move behind the remaining
engines. They are good product ideas that are not on the path to a trusted installation, and saying
so in the roadmap is preferable to discovering it as slippage.

Slices get larger, because each now carries the operational surface that Phase F would have
deferred. That is the cost, and it is the intended one: a slice that is demoable but not
installable was overstating its own completeness.

The roadmap loses a convenient place to put anything not yet designed. Work that genuinely has no
home now has to be either scheduled into a slice or written down as explicitly deferred, which is
the behaviour this ADR is trying to produce.

Existing documents that describe Phase F are corrected rather than annotated, since a phase that no
longer exists should not be discoverable as though it did.

## Alternatives considered

**Keep Phase F and fix SECURITY.md.** The smallest change, and it addresses the symptom. Rejected
because the same mechanism would produce the next false claim: the roadmap would still describe
enforcement as a thing that exists somewhere in the future tense, and the next reference document
written from the architecture would make the same mistake. The failure was structural, not
editorial.

**Keep Phase F but forbid forward-looking claims in reference docs.** Rule 2 of the decision without
the resequencing. Rejected because it fixes the documentation and leaves the deployment problem: no
prefix of the roadmap yields something that can be installed and trusted, which is the actual goal.

**Move only authorization out of Phase F.** Tempting, because authorization is the acute problem.
Rejected because retention, alert delivery, and job recovery after a crash fail the same test — each
is invisible in a demo and mandatory on a server — and moving them one at a time would mean
relitigating this decision three more times.
