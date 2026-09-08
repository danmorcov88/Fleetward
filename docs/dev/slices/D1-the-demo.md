# D1 — The demo, which is also the end-to-end test

Fleetward has been demoable since A6 and has never been demonstrated. Six slices have shipped, each
ending with a walk on a real stack that nobody but the person running it ever saw, and the evidence
that any of it works lives in test output and in journal entries.

This slice turns that walk into something anybody can run in one command, and something CI runs on
every merge.

> **A backup that has never been restored is a hypothesis.** That sentence is the product, and this
> slice is the first time anything says it out loud to somebody who is not reading the source.

---

## Goal

**`make demo` tells the product's story on a real stack in one command, ending with a backup that
fails verification on purpose — and the same script runs in CI, so the demo cannot quietly stop
working.**

## Why now

**Because `test/e2e/` has been empty since the foundation, and its `doc.go` describes this slice.**
It says, in as many words: *"bring the stack up, add an instance, run a backup, watch verification
pass, and assert the two-part status the UI shows."* That package has carried the intent through six
slices and never been filled. It is the only directory in the tree that describes work nobody did.

**Because the product's central claim is demonstrated by nothing that a stranger can run.** The
conformance suite proves one plugin against the contract. The integration suites prove one service
against a database. Neither proves *the product against a user's workflow*, which is the one thing
somebody deciding whether to trust this actually wants to see.

**Because B6 completed the story that makes an installation defensible**, and B7, B8 and B9 add
delivery, observability and packaging on top of a loop that already works. This is the natural point
at which the thing is worth showing.

**Because a demo that CI does not run is a demo that rots.** This repository's whole discipline is
that a claim nothing checks becomes false — `docscheck` exists because a security policy once
described an authorization layer that had never been built
([ADR-0024](../../adr/0024-production-readiness-is-a-slice-property.md)). A demo script is a claim
about the product, in executable form, and it gets the same treatment as every other one.

### Where it sits on the roadmap

Between B6 and B7, and **outside the B-sequence on purpose**: it ships no product capability. It
ships evidence, and an end-to-end guard that every later slice inherits. The B-numbers stay as they
are, because the roadmap's numbering is referred to from journals and ADRs.

The honest cost of doing it now rather than after B7: **the demo cannot show an alert firing**, which
is the most dramatic beat this product will ever have. Act 7 is left as a stub for B7 to fill, and
the act list is designed so that inserting it later is an addition rather than a rewrite.

## Preconditions

All hold on `main` at `842f4c3`. Verified by reading, not assumed.

- **`test/e2e/` contains one file, `doc.go`, and no test.** Its package comment describes this
  slice almost line for line, and does it in the stage vocabulary the roadmap retired — which is
  worth noticing on its own: `docscheck` polices retired words in markdown and not in Go comments,
  so that one survived six slices. This slice rewrites the comment.
- **The dev stack runs with authorization on** since B6. `docker-compose.yml` sets
  `FLEETWARD_AUTH_ENABLED: "true"` and a known bootstrap token, so the seeder has a credential and
  the demo's authorization act has something real to refuse.
- **Two engines are real.** PostgreSQL and SQL Server both back up, restore into a sandbox and
  verify. The control plane image installs `postgresql-client` 16 from PGDG, so a PostgreSQL backup
  works *inside the container* even where it does not on this development machine.
- **Compose already carries a monitored SQL Server** (`sqlserver`, database `fleetward_demo`) with
  the shared directory mounted as `sandbox-share` at `/var/opt/mssql/fleetward`, which is what a
  file-based artifact needs ([ADR-0026](../../adr/0026-a-shared-directory-carries-a-file-based-artifact.md)).
  The compose `postgres` service is the *metadata* store, not a monitored instance.
- **A corrupted artifact already produces `FAILED` rather than `INCONCLUSIVE`**, proven by
  `test/conformance/corruption_test.go` and by
  `internal/controlplane/backup/verify_integration_test.go`. The demo does not need to invent this;
  it needs to show it. `test/conformance/conformance_test.go:701` mutates bytes **in the bucket,
  never through the plugin**, which is the technique to copy.
- **The estate view is one screen** with health and a two-part backup status, refetching every
  thirty seconds, and a failed verification is the loudest thing on it. Adding an instance, editing
  a schedule and everything about retention are CLI-only.
- **`fleetward-cli` covers the whole workflow**: `environment`, `instance`, `backup`, `schedule`,
  `job`, `token`, `audit`. Nothing the demo wants to narrate is missing a command.
- **Retention sweeps every hour by default and stamps nothing retroactively.** A backup with
  `expires_at IS NULL` is never eligible ([ADR-0031](../../adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md)).
  This is load-bearing for the seeder; see the traps.
- **`make` targets are run directly on this machine**, which has no `make`. The demo must work as a
  plain `go run` as well as behind the target.

---

## Design

### One implementation, two entry points

The acts live once, in `tools/demo`, as ordinary Go functions that take a client and a narrator.

| Entry point | What it is for | Narration | Assertions |
|---|---|---|---|
| `make demo` → `go run ./tools/demo` | presenting and recording | on, pauses between acts | on |
| `make demo-check` → `go test -tags=e2e ./test/e2e/...` | CI | off | on |

That split is the whole idea. **The demo and the end-to-end test are the same program**, so the
demo cannot drift from the product without a CI job going red, and the end-to-end test is readable
by somebody who has never seen the codebase.

`go run ./tools/demo` follows the existing shape of `go run ./tools/docsgen` and
`go run ./tools/docscheck`, so nothing new has to be learnt to run it.

### The acts

**Act 0 — the stack.** `docker compose up -d --wait`, then `/readyz` green with every component
named. Pre-flight checks first, because the failures here are boring and the demo should say so
rather than hang: Docker reachable, the socket group set, images present, ports free.

**Act 1 — an estate you can believe.** An estate view with three rows saying "never" demonstrates
nothing. The seeder creates **three environments and twelve instances** across both real engines,
and backfills six weeks of history so the screen looks like an estate with real problems rather
than a fresh install.

What is seeded through the product's own API, because it can be: environments, instances,
connections, schedules, tokens and grants. What is seeded by SQL, because there is no API for it
and there should not be: **backups that happened in the past**. You cannot ask Fleetward to have
taken a backup last Tuesday.

The estate is arranged to have something to say:

| Instances | What they show |
|---|---|
| six healthy, adherent, verified | the ordinary case, so the exceptions read as exceptions |
| two observed-only | an estate that already backs itself up, reported on without changing anything ([ADR-0015](../../adr/0015-observed-and-managed-backups.md)) |
| one behind its window | the gap the product exists to surface |
| one succeeding and failing verification for a fortnight | the case that is invisible to every tool that only checks whether the job ran |
| two unreachable | pointed at an address that genuinely does not answer, so the red health cell is true rather than staged |

**Act 2 — declare, detect, gap.** `fleetward-cli backup adherence` on the seeded estate. One
command, and it is the product thesis: what was declared, what was detected, and the difference.

**Act 3 — the loop, live.** A real backup of `fleetward_demo`, then a real verification: a throwaway
container of the matching engine starts, the artifact restores into it, row counts are compared
against the manifest captured at backup time, and the container is destroyed on every path out.
Green, and the green means something.

**Act 4 — break it on purpose.** The climax. The artifact's bytes are overwritten in MinIO and the
verification is run again. It comes back `FAILED`, and the estate view turns red — louder than "no
backup yet", which is the distinction `CLAUDE.md` §5 insists on.

Then the sentence that is the whole reason to watch: **the green result in Act 3 is only worth
anything because this one is red.** A verification that has only ever been shown to pass is
indistinguishable from one that always passes.

And the second distinction, which nothing else in this category draws: `FAILED` is reserved for
evidence about the artifact. Everything else — an unreachable sandbox, a plugin that died, a
timeout — is `INCONCLUSIVE`, because "we could not tell" and "we can tell, and it is bad" are
different answers ([ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md)).

**Act 5 — who may do this.** A `viewer` refused a restore with a real 403 that came from the server,
a `dba` allowed, and both attempts in an audit log that refuses `UPDATE` and `DELETE` at the
database level. Then `fleetward-cli audit --actor system:retention`, because "who deleted this
artifact" having an answer is the part people do not expect.

**Act 6 — what it refuses to delete.** `fleetward-cli backup retention`: the artifact past its
retention that is kept anyway, because it is the last one on that instance proven restorable
([ADR-0032](../../adr/0032-retention-never-deletes-the-last-good-backup.md)).

**Act 7 — reserved for B7.** An alert firing on the failed verification. Not built, and the act list
leaves the slot rather than pretending.

**Teardown.** `docker compose down --volumes`, unless `--keep` was passed, because after a recording
somebody always wants to click around.

### The honesty rules

This repository has organised itself around not claiming what it has not built, so a demo is the
most dangerous document it could produce. Four rules, and they are not negotiable:

1. **The demo shows nothing the product does not do.** No mocked screens, no staged alerts, no
   screenshot of a feature that exists in a brief.
2. **Seeded history is labelled seeded** — on screen while it runs, in the demo's own README, and in
   anything published from it. The line to use: *the history is seeded so the screen has something
   to say; everything from Act 3 onward is live.*
3. **The live acts are live.** Real containers, real artifacts, real bytes corrupted in a real
   object store.
4. **CI runs it**, so all three of the above stay true.

### What it produces, beyond running

The slice is finished when there is something to show, not merely something to run. Three artifacts,
and all three come out of the same execution:

- **A terminal recording of the acts.** `asciinema` if it is installed, a plain transcript to a file
  if it is not. No new dependency is worth adding for this.
- **Two screenshots of the estate view**, taken by a human: the same rows before Act 4 and after it.
  The second one is the image the whole thing rests on.
- **A written summary** that says what was shown, what was seeded, and what is not built yet.

The framing to use, and the reason it is the honest one as well as the strong one:

> Most backup tooling tells you the job finished. This restores the artifact into a throwaway
> container, counts the rows against a manifest captured at backup time, and destroys the container.
> Then it corrupts the artifact on purpose and shows the same check going red — because a
> verification that has only ever been seen to pass is indistinguishable from one that always
> passes.

And the sentence that has to be in anything published from this, because the project has spent six
slices earning the right to say it rather than to imply it: **this is a work in progress at slice
six of sixteen — alerts, metrics and a release are not built, and five of the eight engines still
only handshake.** `docs/dev/STATUS.md` is the list, and it is kept accurate on purpose.

## Files

### New

| Path | Purpose |
|---|---|
| `tools/demo/` | the acts, the narrator, and a thin `main`; run with `go run ./tools/demo` |
| `test/e2e/demo_test.go` | the same acts under `-tags=e2e`, narration off, assertions on |
| `docs/demo.md` | what the demo shows, what is seeded, and how to record it |

### Modified

| Path | Change |
|---|---|
| `Makefile` | `demo`, `demo-check`, and `demo-keep` targets |
| `.github/workflows/ci.yml` | one job running `demo-check`, sequenced after the smoke test so it does not contend with conformance for Docker |
| `test/e2e/doc.go` | the package comment stops referring to "Stage 6" and starts describing what is now in the package |
| `README.md` | a `make demo` line in the quickstart |
| `docs/roadmap.md`, `docs/dev/STATUS.md`, `tools/wikigen/manifest.go` | where D1 sits, the position, the new page |

## Reuse, do not rewrite

- **`test/conformance/conformance_test.go:701`** already corrupts an artifact by copying it in the
  bucket with a mutated byte. Act 4 is that technique against the demo's own artifact.
- **`copyArtifact` and the mutation table in `test/conformance/corruption_test.go`** name the three
  shapes corruption actually takes — a truncated file, a flipped byte, a wrong header. Pick one and
  say which.
- **The B6 walk, in `docs/dev/journal/B6-authorization-spine.md`**, is Act 5 already written out:
  the exact commands, the exact 403, the audit rows both attempts produced.
- **The B5 walk** is Act 6, including the sentence the preview prints about why an expired artifact
  is still there.
- **`fleetward-cli` and the REST API are the only surfaces the demo touches.** Nothing in
  `tools/demo` may reach into `internal/`, because the moment it does it stops being a demo of the
  product and becomes a demo of the code.
- **The bootstrap credential in `docker-compose.yml`** is how the seeder authenticates. It is
  already a published value in a development stack, and `docs/ops/authorization.md` already says
  what that means.

## Traps

- **Seeded backups must carry no expiry.** The retention sweep runs hourly and would quietly expire
  six weeks of seeded history mid-demo, blanking the estate view in front of an audience. `NULL`
  means never expires and nothing recomputes it
  ([ADR-0031](../../adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md)). Seed `expires_at`
  NULL, and let Act 6 make its own point from one deliberately stamped row.
- **Seeded timestamps must be relative to now.** A demo seeded with fixed dates reads as stale a
  week later and as broken a year later.
- **Verification needs the Docker socket, and that is the act everything builds to.** If the
  sandbox provider cannot start a container, Acts 3 and 4 die and the demo has no climax. Pre-flight
  it in Act 0 and fail with a sentence naming `FLEETWARD_DOCKER_GID`, not with a stack trace forty
  seconds later.
- **A real restore takes tens of seconds.** The narrator must show progress. A demo that looks hung
  is a demo somebody stops watching.
- **`mcr.microsoft.com/mssql/server:2022-latest` is 625 MB** and is slow on a cold cache. Pull in
  Act 0 with a line saying what is happening, or require it warm and say so.
- **The `web` image sometimes fails to build here** with `failed to prepare extraction snapshot …
  parent snapshot does not exist`. Docker Desktop's fault; `docker builder prune` clears it. It cost
  a rebuild in B6 and it is in `STATUS.md`'s environment notes.
- **Git Bash rewrites Unix-looking arguments into Windows paths.** `--backup-dir-local /app/share`
  reaches the CLI as `C:/Program Files/Git/app/share`, and it surfaces much later as a plugin that
  cannot write its file. `MSYS_NO_PATHCONV=1`. This cost twenty minutes in B6.
- **`make demo-check` must not run beside conformance or the integration suite.** All three start
  containers, they contend for Docker, and the result is a screenful of failures that are not real.
- **The demo must be idempotent, or say it is not.** Running it twice against a stack that is
  already seeded should either reset cleanly or refuse with a clear sentence. Half-seeded is the
  worst of the three.
- **Two integration tests fail on this development machine and neither is a regression.** Both are
  in `STATUS.md`'s environment notes.
- **`make` is not installed here.** Run the targets directly, and say so rather than reporting
  `make demo` as passing.
- **On Windows, `gofmt -l`, `buf format --diff` and golangci-lint's `whitespace` linter report
  `core.autocrlf` artefacts.** Verify in a worktree created with
  `git -c core.autocrlf=false worktree add` before believing any of them. B5 found two *real* gofmt
  findings hiding in that noise, and B6 found six real lint findings the same way.
- **Run `go vet` under every build tag before pushing** — default, `integration`, `conformance`, and
  now `e2e`. B3 lost a CI cycle to a test stub in an untouched package.
- **`go run ./tools/docsgen` and `go run ./tools/docscheck` after touching docs.** A new page under
  `docs/` fails CI until it is in `tools/wikigen/manifest.go`.

## Scope fence

In: the demo, the end-to-end test they share, the seeder, and the documentation for both.

Not in this slice:

- **Alerts** (B7). Act 7 is a stub, not an implementation, and not a mock.
- **`/metrics` or spans** (B8); **a release, a tag or a published image** (B9).
- **Any new UI screen.** The demo shows the estate view that exists. If a beat needs a screen that
  does not exist, the beat is cut rather than the screen built.
- **Playwright, or any browser automation.** Terminal output is captured by the demo; browser
  screenshots are taken by a human. Adding a browser-driver dependency to produce three images is
  not a trade worth making.
- **A hosted demo, a public instance, or anything with a URL.**
- **New engines**, and any change to the plugin contract.
- **Making the seeded estate configurable.** One good fixture beats a knob nobody turns.
- The three defects carried since B2 and B3 — the `verify` job that reads `succeeded` after a
  `FAILED` verification, the reaper's blind spot, the plugin's leftover file on a shared directory.

## Done when

```
go build ./...
go vet ./... && go vet -tags=integration ./... && go vet -tags=conformance ./... && go vet -tags=e2e ./...
go test ./...
go run ./tools/docscheck
golangci-lint run                      # in an LF worktree
gofmt -l .                             # in an LF worktree, prints nothing
```

And, the point of the slice:

1. **`make demo` on a clean machine**, from `git clone` to the red screen, in under five minutes,
   with no step that needs a human to know anything.
2. **Act 4 turns the estate view red** on a screen somebody is looking at, from a real artifact
   whose real bytes were really changed.
3. **`make demo-check` passes in CI**, on a runner, without narration, asserting every act.
4. **The demo is run twice in a row** and the second run is not confused by the first.
5. **A recording exists** — the terminal acts captured, and the estate view before and after Act 4.
6. **`docs/demo.md` says what is seeded**, in the same words the demo says it on screen.

Close-out, per the protocol: `STATUS.md` rewritten, a journal entry at
`docs/dev/journal/D1-the-demo.md`, `README.md` updated, and an ADR for anything a future session
might undo — at minimum **why the demo and the end-to-end test are one program**, because the
obvious future "simplification" is to split them, and splitting them is how the demo starts lying.
