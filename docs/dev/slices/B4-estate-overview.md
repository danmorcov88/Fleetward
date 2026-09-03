# B4 — The Estate Overview, and an API client generated from the contract

Everything Fleetward knows is reachable today only by typing a command per server. On fifty servers
that is not a surface, it is a chore, and it is the opposite of the claim the product is built on.

---

## Goal

**One screen that answers, for every instance at once and without a click: is this server healthy,
did its backup run when it was supposed to, and is that backup known to be restorable.**

## Why now

Three things make this the slice that pays back next rather than later.

**B3 finished the answer and left it unreachable.** `GET /api/v1/backup-adherence` already returns
one row per active instance with the verdict, the declaration, the backup that satisfied the window,
the most recent backup of any origin, and the caveats that weaken the answer. The computation the
product exists for is done. It is behind `fleetward-cli backup adherence`, which is a fine surface
for one server and a poor one for fifty.

**The roadmap's thirty-second promise has three components and B4 owns two of them.** Scheduled
health probes and a dashboard that refetches on an interval both land here; alert delivery is B7.
Until the first exists, `instances.health` is whatever the last human-triggered `test-connection`
left behind — which on an estate of fifty means a green dot for a server that died three weeks ago.

**The API client is cheapest to fix before there is a second consumer.** `web/src/lib/api.ts` speaks
two endpoints with hand-written types and its own header comment says generated ones were always the
plan. This slice is the first that needs more than two, and generating them now costs one build step
instead of a rewrite later.

## Preconditions

All hold on `main` at `c651b7c`. Verified rather than assumed.

- **`GET /api/v1/backup-adherence` returns every active instance**, including those with nothing
  declared, ordered by name, with an optional `problems_only` filter
  (`internal/controlplane/backup/adherence.go:220`).
- **`GET /api/v1/instances` returns health and `last_seen_at`** for every instance
  (`internal/controlplane/inventory/service.go:446`).
- **`schedules.kind` and `jobs.kind` already permit `discovery`** — migration `000001_init.up.sql`
  lines 182 and 210. Unlocking it needs **no migration**, unlike B3's, which needed two widened
  CHECK constraints.
- **`inventory.Service.TestConnection` already does the work a discovery job would drive**, and
  already writes `health`, `health_message`, `engine_version` and `last_seen_at` through
  `recordHealth` (`service.go:834`). A `DOWN` probe deliberately does not move `last_seen_at`.
- **`scheduler.Runner` is a three-method interface with one adapter** (`observerunner.go` is
  fourteen lines). A fourth kind is that shape again.
- **The web app builds and lints in CI** (`ci.yml`, job `Web`: `npm ci`, `npm run lint`,
  `npm run build`). React 19, Vite, Tailwind 4, TanStack Query/Router/Table are installed.
- **`api/openapi/openapi.yaml` is generated, committed, and diffed by CI** (`ci.yml`, job
  `Protobuf contract`, step "Verify generated code is current").

## Design decisions already made

Recorded so they are not relitigated. The first six come from outside this slice; the four numbered
below are this slice's own, and each says what it costs.

- **The stack is fixed** — React 19 + TypeScript + Vite + Tailwind + TanStack
  ([ADR-0010](../../adr/0010-react-frontend-stack.md)).
- **No streaming.** Fifty rows on a thirty-second cadence is a polling problem, and
  [ADR-0019](../../adr/0019-rest-api-without-a-grpc-listener.md) means a server-streaming RPC cannot
  be served over the gateway at all. TanStack Query's `refetchInterval` is the whole mechanism.
- **A backup that succeeded and failed verification is louder than no backup at all**
  (`CLAUDE.md` §5). This is the screen's acceptance criterion, not a styling preference.
- **Managed and observed are never the same green tick**
  ([ADR-0015](../../adr/0015-observed-and-managed-backups.md)). "Not verified" and "cannot be
  verified" are different facts, and the CLI already renders the second as `n/a — not ours`
  (`cmd/fleetward-cli/observed.go:200`). The screen uses the same vocabulary.
- **No authentication.** Every route is open to anyone who can reach the port. The UI must not imply
  otherwise: no login screen, no user menu, no "signed in as". **B6.**
- **Design tokens arrive from a separate workstream.** `web/src/index.css` isolates them in one
  `@theme` block for exactly this reason. Build the skeleton; the restyle is an edit to that block.

### Decision 1 — the OpenAPI document is generated wrong today, and that is fixed first

**The committed `api/openapi/openapi.yaml` does not describe the API the control plane serves.** It
is regenerated and diffed by CI, so it is stable, and it is published as a release artifact
(`CLAUDE.md` §7.7) — and it is wrong in four ways, every one of which a generated client would
inherit:

| The document says | The server sends | Because |
|---|---|---|
| `instanceId`, `problemsOnly` | `instance_id`, `problems_only` | `UseProtoNames: true` (ADR-0019) |
| `state: {type: integer, format: enum}` | `"ADHERENCE_STATE_MISSED"` | protojson renders enums as names |
| errors are `google.rpc.Status` | RFC 9457 problem details | `problemErrorHandler`, `gateway.go:42` |
| `/readyz` does not exist | it does, and the UI already uses it | a hand-written handler, not in the proto |

Generating a client from that document produces types that compile and are wrong on every field name
and every enum — the exact "stable and wrong" failure mode `tools/docsgen` had before B3.

**Decision: make the document true first, then generate types only from it.**

Two generator options fix the first two rows, and both were **verified against the installed
toolchain** (buf 1.58.0, `google-gnostic-openapi:v0.7.0`) before being written here:

```yaml
  - remote: buf.build/community/google-gnostic-openapi:v0.7.0
    out: api/openapi
    opt: naming=proto,enum_type=string
```

produces `instance_id:` for every field and query parameter, and

```yaml
                state:
                    enum: [ADHERENCE_STATE_UNSPECIFIED, ADHERENCE_STATE_ADHERENT, ...]
                    type: string
```

The third row is not fixed by an option. `default_response=false` deletes the wrong error schema but
documents nothing in its place, which is honest but unhelpful. **The error shape and `/readyz` stay
hand-written in `lib/api.ts`, with a comment saying why** — they are the two things the contract does
not describe, and pretending otherwise is how the document became wrong in the first place.

**Types only, not a full client.** `openapi-typescript` emits one module of types and no runtime. The
alternatives (`openapi-fetch`, `hey-api`, `orval`) each ship a runtime that would still need
overriding for the problem-details error shape and would still not know about `/readyz` — so they add
a dependency and remove nothing. The existing `request<T>` helper is forty lines, already handles a
503 carrying a good body, and stays.

**What it costs when the contract changes.** `make proto` regenerates the Go, the OpenAPI document
and `web/src/lib/api.gen.ts` together; all three are committed; CI's existing "Verify generated code
is current" step widens to cover the third. A renamed or removed field then fails `npm run build`,
which CI already runs — which is precisely what ADR-0010 promised and has never delivered.

This gets an ADR: a future session finding `naming=proto` will otherwise "fix" it back to the
generator's default.

### Decision 2 — what one row says, and what it deliberately does not

Five facts compete for a row: health, when the last backup was, who took it, whether it was verified,
and whether the instance is adherent. Five columns is not a glance.

**Four columns. Origin is not one of them.**

| Column | Carries | Why |
|---|---|---|
| **Instance** | name, engine, environment | identity, and the engine is how a DBA groups an estate |
| **Health** | tone, label, and the age of the answer | a health state without its age is a claim, not a fact |
| **Backup** | the adherence state, with the last backup's time beneath it | the verdict and its evidence are one thought |
| **Verified** | the three-state answer | the differentiator; never collapsed |

**Origin is folded into the Verified cell, and only there.** For a DBA scanning fifty rows, origin has
exactly one consequence: whether a verification is possible at all. A separate column lets someone
read the Verified column alone and misread a blank; folding it in means the cell itself reads
`n/a — not ours`, and the two facts cannot be read apart. That is the one collapse this screen is
allowed, and it is allowed because it makes the honest reading the only reading.

The Verified cell therefore has these answers and no others:

| Backup | Cell | Tone |
|---|---|---|
| observed | `n/a — not ours` | muted |
| managed, never verified | `never verified` | warn |
| managed, `VERIFIED` | `verified` | ok |
| managed, `FAILED` | `verification failed` | **critical — the loudest thing on the screen** |
| managed, `INCONCLUSIVE` | `inconclusive` | warn |
| no backup at all | `—` | muted |

**Adherence keeps all five of its states**, including `NOT_DECLARED`, which renders as "nothing
declared" rather than being hidden. On an estate of fifty, "nobody has said what this one's backups
should look like" is a finding
([ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md)).

**Caveats go behind the row, not into a column.** B3 attaches them per instance — an approximate
timestamp, an identity that will not survive a rename, evidence that cannot report an outcome. They
become a marker on the row that expands in place to show the caveat text, the declared cron and
grace, and the last verification's detail. Five facts become four columns and one expansion.

**Default order is severity, not name.** Verification failed, then missed, then unproven or failed
evidence, then not declared, then adherent. A screen sorted alphabetically makes the reader do the
scanning the screen exists to do. Column sorting is available; severity is what it opens on.

**A sub-question this forces, and the answer.** `Backup.verification` is populated **only** by
`GetBackup` — not by `ListBackups`, and not by adherence (`service.go:820` is the sole call site). So
no list endpoint can render the Verified column today. Three ways out, and one is right:

- Fifty `GetBackup` calls from the browser every thirty seconds. No.
- Populate `Instance.backup_summary`, declared at `controlplane.proto:245` with the comment
  "denormalized so the estate grid renders in one query" and **populated by nothing**. It would
  duplicate what adherence already says, less well.
- **Attach the latest verification to the backups adherence returns**, in one batched query beside
  the two that already run there. Chosen.

Which leaves `backup_summary` a field the contract declares and nothing fills. Removing it is
breaking; leaving it silent is a lie. It is marked `deprecated = true` with a comment pointing at
`GetBackupAdherence` — additive, `buf breaking` stays green, and the next session is not left to
discover it empty.

**The screen therefore makes two requests, not one:** `GET /api/v1/instances` for identity and
health, `GET /api/v1/backup-adherence` for the backup half, joined in the browser on `instance_id`.
Two, because health does not belong on a backup endpoint and adherence does not belong on an
inventory one. Both refetch on the same interval, so they cannot drift by more than one tick.

### Decision 3 — this slice unlocks scheduled health probes

**Yes.** The alternative is a screen that lies, and the cost is the smallest it will ever be.

Without it, the Health column shows `instances.health` from whenever a human last ran
`test-connection` — possibly never, since `AddInstance` writes a health of `unknown` and nothing else
touches it. The other way round, having the browser trigger fifty live probes on every refresh, means
fifty connections to production databases every thirty seconds from a page anyone can open. Neither
is acceptable on the screen whose entire claim is that it can be trusted at a glance.

The bill:

- **No migration.** Both CHECK constraints already permit `discovery`.
- One `Runner` method, `RunDiscoveryJob`, calling the existing `TestConnection`.
- One adapter file, the shape of `observerunner.go`.
- One widened condition in `scheduler.CreateSchedule` (`service.go:100`), and its message updated.
- `BackupRunner` gains an inventory dependency, so it is renamed `JobRunner` — it already runs
  observation, which is not a backup either. Internal, no contract impact.

**Scope guard, because the kind's name invites it.** `discovery` here means the health probe and
nothing more. It does **not** re-run `Discover` to refresh topology or database lists.
`TestConnection` already refreshes `engine_version` through `recordHealth`, and that is the whole of
it.

**And the screen still renders the age of the answer.** A scheduled probe every five minutes is not a
live one, and a Health cell reading `healthy · 4m ago` is honest in a way a bare green dot is not —
including when the scheduler itself has stopped.

### Decision 4 — the UI gets a test runner

**Yes: Vitest and React Testing Library, `npm run test`, one CI step beside lint and build.**

A dashboard whose entire claim is "it renders the right status", and whose CI checks only that it
compiles, is a gap rather than a detail. But a snapshot test of a placeholder proves nothing, so the
suite is scoped to the three things that would make this screen lie:

1. **The Verified cell, table-driven over every (origin × verification status) pair**, including the
   absent backup. The case that matters is the one that must never appear: an observed backup
   rendering as `never verified`, which sends a DBA looking for a verification that is never coming.
2. **That a verification-failed row is louder than a no-backup row**, asserted on the tone the row
   carries rather than on a colour — `CLAUDE.md` §5 as an executable statement.
3. **The default severity ordering**, given a fixture estate with one row of each state.

Not Playwright. An end-to-end runner needs the stack up, and that path is covered by the walk, which
is not optional here.

## Files

### New

```
web/src/lib/api.gen.ts               generated from api/openapi/openapi.yaml; committed, CI-diffed
web/src/lib/format.ts                relative age, byte size, cron echo
web/src/estate/status.ts             (origin, verification, adherence) -> tone + label. Pure.
web/src/estate/status.test.ts        the table-driven cases above
web/src/estate/columns.tsx           the four columns and the cells they render
web/src/estate/EstateTable.tsx       TanStack Table wiring, severity order, row expansion
web/src/estate/EstateTable.test.tsx  loudness and ordering
web/src/components/Badge.tsx         a toned label, beside StatusDot
web/vitest.config.ts
internal/controlplane/scheduler/discoveryrunner.go
docs/adr/0029-the-openapi-document-is-generated-to-match-the-wire.md
docs/dev/journal/B4-estate-overview.md
```

### Modified

```
buf.gen.yaml                         naming=proto,enum_type=string
api/openapi/openapi.yaml             regenerated — a large, correct diff
Makefile                             proto also generates the TS types; a test-web target
.github/workflows/ci.yml             web job runs npm run test; generated-code diff covers api.gen.ts
web/package.json                     openapi-typescript, vitest, @testing-library/*, jsdom
web/src/lib/api.ts                   generated types; retired "Stage 1" comment; /readyz stays here
web/src/routes/Estate.tsx            the screen
web/src/components/AppShell.tsx      retired "Phase 1 screens" / "later stage" prose
internal/controlplane/scheduler/scheduler.go     Runner gains RunDiscoveryJob
internal/controlplane/scheduler/backuprunner.go  renamed JobRunner, takes inventory
internal/controlplane/scheduler/service.go       accepts kind 'discovery'
internal/controlplane/backup/adherence.go        attach the latest verification
api/proto/fleetward/v1/controlplane.proto        deprecate BackupSummary
cmd/fleetward/main.go                            wire the inventory service into the runner
docs/dev/STATUS.md  README.md  docs/roadmap.md   as the protocol requires
```

## Reuse, do not rewrite

- **`internal/controlplane/inventory/service.go:548` `TestConnection`** — the discovery job's whole
  body. It probes, records health, and deliberately does not move `last_seen_at` on a failure.
- **`internal/controlplane/scheduler/observerunner.go`** — copy its shape exactly. Fourteen lines.
- **`internal/controlplane/backup/adherence.go:263` `attachWindowBackups`** — the batched
  `VALUES`-join pattern for attaching per-instance rows without N+1. The verification attachment is
  that shape a third time.
- **`internal/controlplane/backup/service.go:820` `latestVerification`** — already exists; it needs a
  batched sibling, not a reimplementation.
- **`cmd/fleetward-cli/observed.go:200` `verifiedColumn`** — the three-state vocabulary is already
  decided and shipped. The screen says the same words. If the UI and the CLI disagree about what to
  call a state, one of them is wrong, and it is not the one that shipped first.
- **`web/src/components/StatusDot.tsx`** — never colour alone; the label carries the same
  information. Extend that tone vocabulary rather than inventing a second one.
- **`web/src/index.css`** — status colours are named for meaning, not hue. Use the tokens.

## Traps

- **`Estate.tsx`, `lib/api.ts` and `AppShell.tsx` carry retired vocabulary, and one claim is actively
  false.** `Estate.tsx` says "the add-instance flow arrives in Stage 1 along with the PostgreSQL
  plugin" — a stage name retired by ADR-0024, and a claim untrue since A1. `tools/docscheck` walks
  `.md` only (`main.go:162`), so nothing catches wrong prose inside a `.tsx` file. Fixing all three is
  part of this slice; consider whether the vocabulary check should read `web/src` too.
- **Regenerating the OpenAPI document on Windows produces CRLF inside YAML string literals.**
  Descriptions come out as `"...estate, which is the\r\n question..."`. CI regenerates on Linux and
  will reject the diff. Regenerate in a worktree created with
  `git -c core.autocrlf=false worktree add`, the same precaution `gofmt` and `buf format` need here.
- **A `verify` job whose verification returned `FAILED` still reads `succeeded` in `job list`.** The
  job ran to completion; the verdict lives on the verification row. Pre-existing, written up in B2's
  journal as B4's — and it is exactly the distinction this screen must not repeat. The screen reads
  the verification's status, never the job's.
- **Three verification states, not two, and five adherence states.** A UI that renders `not verified`
  for an observed backup is the specific failure ADR-0015 exists to prevent.
- **`@tanstack/react-table` resolves to 9.2.4**, which is built on `@tanstack/react-store` and is not
  the v8 `useReactTable` / `getCoreRowModel` API that almost everything written about the library
  describes. Read the installed types after `npm ci`; do not follow a blog post.
- **ADR-0010 says Table "virtualizes the estate grid without us writing windowing logic". It does
  not** — virtualization is `@tanstack/react-virtual`, which is not installed. Fifty rows do not need
  it. Do not add it, and do not let the ADR's phrasing suggest otherwise.
- **`shadcn/ui` is named in ADR-0010 and is not installed.** Two hand-rolled Tailwind components are
  what exists. Installing the shadcn toolchain is not in this slice; a third small component beside
  the two is.
- **Run `go vet -tags=integration ./...` and `go vet -tags=conformance ./...` over the whole tree
  before pushing.** B3 cost a full CI cycle because a test stub in an untouched package implemented a
  client interface explicitly, and only CI compiled it. Widening `scheduler.Runner` will do this
  again if any fake implements it.
- **`make` is not installed on this machine.** Run the targets directly and say so, rather than
  reporting `make lint test` as passing.
- **On Windows `gofmt -l` and `buf format --diff` report every file.** A `core.autocrlf=true`
  artefact, not a finding. `go test -race` needs cgo, which a stock install does not have.
- **Run `go mod tidy` before pushing, and `npm ci` rather than `npm install`**, so the lockfile CI
  uses is the one that was tested.
- **Two integration tests fail on this machine and neither is a regression.** Both are in
  `STATUS.md`'s environment notes and both were reproduced on `main`. Do not chase them.
- **The walk is not optional.** B3's walk found two defects nothing else did, both in seams between
  components. Here it is `docker compose up --build` with more than one instance, at least one of them
  deliberately behind.

## Scope fence

**In:** the Estate Overview screen; a generated API client; scheduled health probes as a `discovery`
schedule kind; a UI test runner; the retired prose in `web/src`.

**Not in this slice, deliberately:**

- Authentication or a login flow — **B6**. No user menu, no "signed in as".
- Alert delivery or any notification — **B7**. The third component of the thirty-second promise.
- Adding, editing or deleting an instance from the UI. The CLI does it; the screen reports.
- A full schedules-and-jobs CRUD surface. A row links to what exists; it does not manage it.
- Retention or expiry — **B5**.
- Charts, sparklines or metric graphs. Deferred deliberately; nothing collects metrics yet.
- The remaining five engines — **B11–B16**.
- Virtualization, and a `shadcn/ui` installation.
- Widening the job reaper to catch a `running` job with no lease. B3 named it; it is a change to B1's
  at-most-once machinery and deserves its own consideration.

## Done when

Concrete, with the output expected. `make` is absent on this machine, so each target's command is
given directly.

```bash
# The document now describes the wire. The first must be zero.
grep -c "instanceId" api/openapi/openapi.yaml
grep -A2 "^                state:" api/openapi/openapi.yaml | head

# Generated TypeScript is current, the same way the Go is.
npm --prefix web run generate && git diff --exit-code -- web/src/lib/api.gen.ts

# The web app lints, type-checks, tests and builds.
npm --prefix web ci
npm --prefix web run lint && npm --prefix web run test && npm --prefix web run build

# Go, including under every build tag — this is the check B3 skipped.
go vet ./... && go vet -tags=integration ./... && go vet -tags=conformance ./...
go test ./...                                                  # ok, no new failures
go test -tags=integration -timeout 30m ./...                   # only the two known machine failures
go test -tags=conformance -timeout 60m ./test/conformance/...  # PASS, suite unchanged

go run ./tools/docscheck                                       # no findings
go mod tidy && git diff --exit-code -- go.mod go.sum

# A discovery schedule is accepted, fires, and moves the health the screen reads.
fleetward-cli schedule create --instance prod-1 --kind discovery --cron "*/5 * * * *"
fleetward-cli instance list          # HEALTH and LAST SEEN move without anyone asking

# And the walk, which is where this slice is actually judged.
docker compose up --build
```

The walk must show, on one screen at `localhost:3000`, without clicking:

- an instance whose backup is **missed**, and how long ago the last one was;
- an instance whose backup **succeeded and whose verification failed**, rendered louder than the
  missed one — if it is not the loudest thing on the screen, the slice is not done;
- an instance backed up only by **observed** evidence, reading `n/a — not ours` and never
  `not verified`;
- an instance with **nothing declared**, present rather than hidden;
- a Health cell carrying **the age of its answer**, and that age advancing on its own.

Then the protocol's close-out: `STATUS.md` rewritten, a journal entry carrying the actual numbers,
ADR-0029, `README.md` current, and a pull request whose body says why.
