# ADR-0037: The demo and the end-to-end test are one program

- **Status:** Accepted
- **Date:** 2026-09-08
- **Slice:** D1 — the demo
- **Relates to:** [ADR-0012](0012-testcontainers-and-conformance-suite.md),
  [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md),
  [ADR-0024](0024-production-readiness-is-a-slice-property.md)

## Context

Six slices shipped before anything was ever shown to anybody. Each ended with a walk on a real
stack that only the person running it saw, and the evidence that any of it works lived in test
output and in journal entries.

The obvious way to fix that is a demo script: a shell file, or a written runbook, that walks the
product for a camera. Every project has one, and every project's has rotted. It rots for a reason
that is structural rather than careless — it is a description of the product that nothing executes,
so it stays exactly as true as it was on the day it was written while the product moves.

This repository has already met that failure once, and answered it once. `docscheck` exists because
`SECURITY.md` described an authorization layer that had never been built
([ADR-0024](0024-production-readiness-is-a-slice-property.md)). The lesson was not "write better
documentation"; it was **a claim is trustworthy because a merge gate enforces it**.

A demo is a claim about the product, in the most persuasive form the project will ever produce. It
is therefore the most dangerous thing in the repository, and it needs the same treatment as the
protobuf contract, the plugin conformance suite and the documentation: something has to run it and
go red.

Separately, `test/e2e/` had been empty since the foundation, holding one `doc.go` whose package
comment described this exact walk — bring the stack up, add an instance, run a backup, watch
verification pass — and no test. Two things needed writing, and they were the same thing.

## Decision

**The demo and the end-to-end test are one program, with two entry points.**

The acts live once, in `tools/demo/acts`, as ordinary Go functions that take a client and a
narrator. `go run ./tools/demo` runs them with the presentation on — an act banner and a pause
between acts. `go test -tags=e2e ./test/e2e/...` runs the same functions with the presentation off.

**Every act asserts, in both modes.** There is no mode in which the demo prints a result without
checking it. The two entry points differ only in whether a person is watching.

**The narration stays on in CI**, without the pauses. A failed end-to-end run in a CI log reads as
the story it was telling when it broke, which is worth more than a bare comparison to somebody who
has never seen the codebase.

**Nothing in `tools/demo` imports `internal/`.** The demo drives the REST API and, for one thing
only, the metadata database. The moment it reached into the code it would stop being a demo of the
product and become a demo of the code.

**The one exception is seeded history.** Backups that happened in the past are inserted as rows,
because there is no endpoint for that and there should not be — an API that let you assert a backup
had happened last Tuesday would be a way to forge evidence about an estate. Everything the seeder
can do through the product's own API, it does: environments, instances, connections, schedules,
tokens and grants.

**Seeded is labelled seeded**, on screen while it runs, in `docs/demo.md`, and in anything published
from a recording.

## Consequences

**The demo cannot drift from the product without a CI job going red.** A renamed field, a changed
enum, an endpoint that stops accepting a body it used to — any of them fails `End-to-end demo` on
the pull request that introduced it, rather than in front of an audience six weeks later.

**The end-to-end test reads as a user's workflow rather than as a list of API calls.** Its failures
name an act and say what that act expected, in the same sentences the demo says out loud.

**The demo is slow, and it is a required check.** It brings up eight containers, takes two real
backups and runs three real verifications, each of which starts a further container. Fifteen to
twenty-five minutes on a runner, and it is sequenced after the compose smoke test because both need
the runner's single Docker daemon, as does the conformance suite. That cost is the price of the
guarantee, and it is the same trade [ADR-0012](0012-testcontainers-and-conformance-suite.md) already
made for the conformance suite.

**A failed demo is ambiguous between "the product broke" and "the demo broke", and that ambiguity is
deliberate.** Under the split design the second failure mode is silent, which is strictly worse: it
is how a demo comes to show a version of the product that no longer exists.

**Nothing here may be mocked, and act 7 is the proof.** An alert firing on the failed verification
in act 4 is the most dramatic beat this product will ever have, and it is not built. The act list
leaves the slot empty and says so, because a demo that shows a feature which exists only in a brief
makes every other claim in it worthless.

## Alternatives considered

**A shell script, plus a separate lean end-to-end test.** The obvious shape, and the one this
decision exists to refuse. The script would be readable and would need no Go, and it would be
unrunnable on Windows, would duplicate every assertion, and — the fatal part — the two would drift.
Whichever of them CI did not run would be the one that lied. This is the "simplification" a future
session is most likely to attempt, which is why it is written down here rather than in a comment.

**A demo with narration and no assertions, alongside a test with assertions and no narration.** The
same drift, one directory later. It also gives up the thing that makes the demo trustworthy: that
the sentence on the screen and the check behind it are the same line of code.

**Browser automation, so the demo could show the estate view going red by itself.** Rejected in the
slice's own scope fence. Adding a browser driver to produce three screenshots is a dependency, a CI
service, and a class of flakiness, in exchange for images a human can take in ten seconds. The
terminal acts are captured by the program; the screenshots are taken by a person.

**Seeding the whole estate through the API, with no SQL at all.** This would have required an
endpoint that backdates a backup. That endpoint is the problem: adherence, retention and the estate
view all rest on `completed_at` being what actually happened, and a route that writes it would be
the one route in the product that can manufacture evidence. Going around the API for the fixture,
loudly and in one file, is the smaller cost.

**Making the fixture configurable.** One good fixture beats a knob nobody turns, and a configurable
estate is a second thing to keep working.
