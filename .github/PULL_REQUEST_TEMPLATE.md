## What and why

<!-- What changes, and what problem it solves. Link the issue if there is one. -->

Closes #

## Type

<!-- The PR title must follow Conventional Commits, e.g. feat(plugin/postgres): add PITR window discovery -->

- [ ] `feat` — new capability
- [ ] `fix` — bug fix
- [ ] `docs` — documentation only
- [ ] `refactor` / `perf` / `test` / `chore` / `ci`
- [ ] Breaking change (`!` in the title, or a `BREAKING CHANGE:` footer)

## Checklist

- [ ] `make lint test` passes locally
- [ ] Conventional Commits title
- [ ] `README.md` updated if this changes what the project does, how it is run, or its architecture
- [ ] **No documentation added here describes behaviour that does not exist**, unless it is in a
      slice brief or marked `> Planned.`
- [ ] If this changed what a security researcher needs to know, `.github/SECURITY.md` is updated.
      *(Nothing can detect that a prose claim became false — this one is on you.)*
- [ ] If a slice completed: a journal entry exists at `docs/dev/journal/`, and `docs/dev/STATUS.md`
      has been rewritten to point at the next slice

### If this touches a plugin

- [ ] `make conformance` passes
- [ ] No capability flag is set ahead of the behaviour that implements it
- [ ] Metric names follow OpenTelemetry database semantic conventions

### If this touches the contract (`api/proto/`)

- [ ] `make proto` run and the generated code committed
- [ ] `buf breaking` passes, or an ADR explains why the break is necessary

### If this touches core (`internal/`) or the UI (`web/`)

- [ ] No branching on an engine name — behaviour is driven by `Capabilities`

### If this touches backup, restore, or verification

- [ ] Sandbox teardown is guaranteed on every path, including panic
- [ ] `FAILED` and `INCONCLUSIVE` remain distinct
- [ ] No credential can reach a log line, an error message, or disk

## Architecture decisions

- [ ] No decision here needs an ADR
- [ ] An ADR is included in `docs/adr/`
