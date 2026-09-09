# ADR-0040: An inconclusive verification is not an alert about the artifact

- **Status:** Accepted
- **Date:** 2026-09-09
- **Slice:** B7 — alert rules and delivery
- **Relates to:** [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md),
  [ADR-0038](0038-alert-evaluation-is-a-pass-over-the-estate.md)

## Context

[ADR-0022](0022-failed-and-inconclusive-are-different-answers.md) separated two verdicts that a
naive implementation would have collapsed into one:

- **`FAILED`** — the artifact was restored into a sandbox and the restored data did not match its
  manifest. Evidence about the backup.
- **`INCONCLUSIVE`** — a sandbox that never became ready, a plugin that could not be reached, a
  transfer that broke, a Docker daemon out of disk. Evidence about the machinery, and nothing at all
  about the backup.

That decision was written to protect a future that had not been built yet. Its own words: reporting
an infrastructure problem as data loss "would train operators to ignore the alert that matters".
Until B7 there was no alert, so the distinction cost nothing and protected nothing. B7 is the slice
where it starts doing work, and the slice where it can be undone in two words.

The undoing is not hypothetical, and it will not look like vandalism. It looks like this: somebody
notices that a week of inconclusive verifications produced no alert, reads that as a gap, and
changes

```sql
WHERE v.status = 'failed'
```

to

```sql
WHERE v.status IN ('failed', 'inconclusive')
```

in a commit whose message says "alert on verifications that did not succeed". It is a defensible
sentence. It is also the exact failure ADR-0022 exists to prevent, because on an estate of fifty
instances the inconclusive verdict is the common one — a busy Docker host produces them in
handfuls — and the `critical` page that means *this backup will not restore* would then arrive
beside a hundred that mean *the VM was full*, and be muted along with them.

## Decision

### 1. `verification_failed` fires on `status = 'failed'` and on nothing else

An `inconclusive` verdict produces no alert in B7. That is a knowing hole, and it is listed in
`docs/dev/STATUS.md` under what is knowingly absent rather than left to be discovered.

### 2. The predicate is a named constant, not text inside a query

```go
const verificationFailedPredicate = "v.status = 'failed'"
```

Buried in a query, widening it is a two-word edit in a fifteen-line string that a reviewer skims.
Named, it is one line in a diff, and the constant's own comment is the argument against changing it.

### 3. Two tests enforce it, from opposite directions

`TestInconclusiveVerificationDoesNotFireTheFailedAlert` states the intent: a table of verdicts and
whether each fires. `TestTheEvaluatorQueryNamesOnlyTheFailedVerdict` states the SQL: the constant's
value, with a failure message that names this ADR and ADR-0022 and explains what widening it costs.
A third, in the integration suite, seeds an inconclusive verification against a real database and
asserts that a pass opens nothing — and then seeds a failed one and asserts that a pass opens
exactly one, so the test cannot pass because nothing fires at all.

Three, because the intent and the SQL can be changed independently and only one of them is obviously
about alerting.

**A comment is advice. A test is a refusal.** This decision is written down *and* enforced, because
the previous decision was written down and would not have survived on its own.

### 4. The eventual fix is a separate rule kind, never a widened predicate

"Verification has been inconclusive on this instance for a week" is a real condition and worth
telling somebody about. It is a different sentence from "this backup will not restore", it deserves
a different severity, and it therefore deserves a different rule kind — one an operator can disable
without disabling the one that matters.

`alert_rules.kind` is a closed set widened by migration
([ADR-0038](0038-alert-evaluation-is-a-pass-over-the-estate.md)), so adding
`verification_inconclusive` later is one migration, one evaluator, and one entry in `evaluatedKinds`.
Nothing in this decision makes that harder; what it forbids is reaching the same place by making one
alert mean two things.

## Consequences

**Good.**

- A `critical` alert from Fleetward means one thing, and an operator can act on it without triage.
- The distinction ADR-0022 drew is now load-bearing rather than decorative, and is tested.
- The route to covering the inconclusive case is open and cheap.

**Costs, accepted.**

- **A sandbox that has been unable to start for a week produces no alert.** Verification is not
  happening at all, and nothing pages anybody about it. Two things soften it and neither closes it:
  the estate view shows the verdicts, and the control plane's readiness reports its sandbox provider
  as degraded. It is in `STATUS.md`, and it is what a later slice picks up.

## Alternatives considered

**Fire on `inconclusive` at `info` severity, under the same rule kind.** Attractive because it is one
line, and wrong because severity is a property of the rule an operator wrote, not of the row the
evaluator found. Somebody who set that rule to `critical` for the reason it exists would then be
paged for a full Docker host.

**Fire on `inconclusive` only after N consecutive occurrences.** A better idea than the one above and
still the wrong slice: it needs a window, a counter and a threshold, which is the machinery
`for_duration_s` exists for and which B7 deliberately does not build.

**Say nothing about it and let a future session decide.** What ADR-0022 effectively did, which is why
this record exists at all. A hole that is written down is a decision; a hole that is not is a defect
waiting to be reported.
