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
Lucrăm la Fleetward. Citește în ordine, înainte să scrii cod:

1. CLAUDE.md                                  — contextul proiectului, regulile care nu se negociază
2. docs/dev/STATUS.md                         — unde suntem acum
3. docs/dev/slices/README.md                  — protocolul de sesiune
4. docs/dev/slices/A5-restore-and-verify.md   — slice-ul de executat

Execută slice-ul respectând protocolul: branch nou, teste verzi, STATUS.md actualizat,
push și pull request. Nu depăși scope fence-ul din brief.
```

Replace `A5-restore-and-verify.md` with whichever slice `STATUS.md` says is next.

---

## The protocol

### 1. Orient

Read `CLAUDE.md` fully, then `STATUS.md`, then the slice brief. `STATUS.md` is the authority on
what is next; the briefs are the authority on how.

### 2. Branch

`main` is protected: no direct pushes, no force pushes, and all seven CI jobs must pass before a
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

Three things, always:

- **`docs/dev/STATUS.md`** — mark the slice done, point "Current position" at the next one, and add
  a short *delivered* section recording decisions worth carrying forward. This is what the next
  session reads first; it is not bookkeeping.
- **`README.md`** — update if the change alters what Fleetward does, how it is run, its
  architecture, or its stage. The Mermaid diagrams live there and drift silently.
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

| Slice | Brief | State |
|---|---|---|
| A1 | — (delivered; notes in `STATUS.md`) | ✅ |
| A2 | [Inventory service and CLI](A2-inventory-and-cli.md) | ✅ |
| A3 | [Sandbox provider](A3-sandbox-provider.md) | ✅ |
| A4 | [Backup with manifest](A4-backup-and-manifest.md) | ✅ |
| A5 | [Restore and verification](A5-restore-and-verify.md) | next |
| A6 | [Proving verification fails](A6-verification-fails-loudly.md) | ⬜ |

Phases B through G are described in `CLAUDE.md` §6 and `STATUS.md`. Their briefs are written when
the phase starts — writing them now would be inventing detail that the preceding phase is likely to
change, and a confidently wrong brief is worse than none.
