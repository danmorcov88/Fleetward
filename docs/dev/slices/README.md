# Slice briefs — how to run one development session

Fleetward is built in **slices**: units of work small enough for one or two sittings, each
independently demoable, each ending with the tree green and `STATUS.md` updated.

Development on this project is sporadic. That makes the expensive thing not writing code but
**reconstructing context** — remembering what was decided, what was deliberately left out, and why
something that looks wrong is actually correct. These briefs exist to make that cost near zero: a
session should be able to start cold, from the repository alone.

Each brief is self-contained. It does not assume you remember the conversation that produced it.

---

## Starting a session

Paste this into a fresh chat, changing only the slice identifier:

```
We are working on Fleetward. Read these in order, before writing any code:

1. CLAUDE.md                                  — project context, the rules that are not negotiable
2. docs/dev/STATUS.md                         — where we are right now
3. docs/dev/slices/README.md                  — the session protocol
4. docs/dev/slices/<slice>.md                 — the slice to execute

Execute the slice following the protocol: new branch, green tests, STATUS.md rewritten,
a journal entry added, push, pull request. Do not exceed the scope fence in the brief.
```

Replace `<slice>.md` with whichever slice `STATUS.md` says is next.

---

## The protocol

### 1. Orient

Read `CLAUDE.md` fully, then `STATUS.md`, then the slice brief. `STATUS.md` is the authority on
what is next; the briefs are the authority on how.

### 2. Branch

`main` is protected: no direct pushes, no force pushes, and all ten CI jobs must pass before a
pull request can merge.

```bash
git switch main && git pull --ff-only
git switch -c <type>/<short-description>
```

Branch names follow the Conventional Commit type: `feat/inventory-service`,
`fix/lease-renewal`, `docs/slice-briefs`.

### 3. Build

Work only inside the slice's scope. Every brief has a **Scope fence** section listing what is
explicitly *not* in this slice. That section exists because a fresh session reading a roadmap will
naturally try to build too much, and a half-finished slice is worse than a small complete one.

If you discover the slice is wrong — a design that does not survive contact with the code — stop
and say so rather than working around it. Record the finding in the brief and, if it changes a
decision, write an ADR.

### 4. Verify

Everything below must pass before the branch is pushed. These are the same checks CI runs.

```bash
make lint            # golangci-lint + buf lint + eslint
make test            # unit tests
make test-integration  # testcontainers; needs Docker
make conformance     # the shared plugin suite
```

If the slice touched `api/proto/`, also:

```bash
make proto           # regenerate, and commit the result
buf breaking --against '.git#branch=main'
```

### 5. Close out

Four things, always:

- **A journal entry** at `docs/dev/journal/<slice>-<slug>.md` — what shipped, how it was verified
  with the actual numbers, the decisions worth carrying forward, and what was deliberately left
  unbuilt. Append-only; never edited afterwards. See
  [`../journal/README.md`](../journal/README.md) for the shape.
- **`docs/dev/STATUS.md`** — **rewritten**, not appended to: current position pointing at the next
  slice, and the known-broken list adjusted. It stays short because everything with a longer
  lifetime goes in the journal. This is what the next session reads first; it is not bookkeeping.
- **`README.md` and the docs under `docs/`** — update if the change alters what Fleetward does, how
  it is run, or its architecture. Mermaid diagrams drift silently. Documentation describes what is;
  briefs describe what will be; the two never mix in one file.
- **An ADR** in `docs/adr/` for anything a future session might otherwise undo.

### 6. Ship

```bash
git push -u origin <branch>
gh pr create        # or open the compare URL in a browser
```

Conventional Commits for the title. The body explains *why*, not *what* — the diff already says
what. Every commit ends with:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Wait for CI. Merge when green.

---

## What a good slice brief contains

If you write a new one, follow this shape — it is what makes a cold start possible.

| Section | Why it is there |
|---|---|
| **Goal** | One sentence. If it needs two, the slice is too big. |
| **Why now** | What this unblocks. Prevents reordering work on a whim. |
| **Preconditions** | What must already exist. Catches a session started out of order. |
| **Design decisions already made** | So they are not relitigated. The expensive failure mode. |
| **Files** | Where the work goes, split into new and modified. |
| **Reuse, do not rewrite** | Existing helpers with paths. Prevents parallel implementations. |
| **Traps** | What will bite. Usually the reason an earlier attempt failed. |
| **Scope fence** | What is explicitly *not* in this slice. |
| **Done when** | Concrete commands with expected output, not adjectives. |

---

## Current slices

| Slice | Brief | Journal | State |
|---|---|---|---|
| A1 | — | [entry](../journal/A1-health-and-discover.md) | ✅ |
| A2 | [Inventory service and CLI](A2-inventory-and-cli.md) | [entry](../journal/A2-inventory-and-cli.md) | ✅ |
| A3 | [Sandbox provider](A3-sandbox-provider.md) | [entry](../journal/A3-sandbox-provider.md) | ✅ |
| A4 | [Backup with manifest](A4-backup-and-manifest.md) | [entry](../journal/A4-backup-and-manifest.md) | ✅ |
| A5 | [Restore and verification](A5-restore-and-verify.md) | [entry](../journal/A5-restore-and-verify.md) | ✅ |
| A6 | [Proving verification fails](A6-verification-fails-loudly.md) | [entry](../journal/A6-verification-fails-loudly.md) | ✅ |
| B1 | [Scheduler and leases](B1-scheduler-and-leases.md) | [entry](../journal/B1-scheduler-and-leases.md) | ✅ |
| B2 | [SQL Server plugin](B2-sqlserver-plugin.md) | [entry](../journal/B2-sqlserver-plugin.md) | ✅ |
| B3 | [Observed backups](B3-observed-backups.md) | [entry](../journal/B3-observed-backups.md) | ✅ |

Phase A is complete and Phase B has started. What comes next is in
[`../../roadmap.md`](../../roadmap.md). A brief is written when its slice starts — writing them all
now would be inventing detail that the preceding slice is likely to change, and a confidently wrong
brief is worse than none.

The Phase A briefs above use the roadmap vocabulary of the time and defer work to "Phase C" or
"Phase F". Those labels are retired — the plan is now a numbered slice sequence, and production
readiness is a property of every slice rather than a phase
([ADR-0024](../../adr/0024-production-readiness-is-a-slice-property.md)). The briefs are left as
written; read such a reference as "a later slice".
