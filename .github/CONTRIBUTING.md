# Contributing to Fleetward

Thanks for considering a contribution. This document covers the practical mechanics; the
architectural ground rules live in [`CLAUDE.md`](../CLAUDE.md) and [`docs/adr/`](../docs/adr/).

## Ground rules

1. **Core never learns engine names.** Core and the UI branch on a plugin's declared
   `Capabilities`, never on `"postgres"` or `"redis"`. A PR that adds an engine-name conditional to
   `internal/` or `web/` will be asked to extend the capability matrix instead.
2. **Conformance is the merge gate for plugins.** No plugin change merges until
   `make conformance` passes for that plugin.
3. **Scope discipline.** The non-goals in `CLAUDE.md` §1 are firm. If you believe one should
   change, open an issue proposing an ADR — not a PR implementing it.
4. **Backup and restore code gets extra scrutiny.** Correctness there is the product.

## Getting set up

```bash
git clone https://github.com/danmorcov88/fleetward.git
cd fleetward
make dev      # full stack via docker compose
make build    # binaries into ./bin
make test     # unit tests
```

You need Go 1.25+, Node 22+, and Docker. `buf` is required only if you touch `api/proto/`.

## Development workflow

`main` is protected: it cannot be pushed to directly, force-pushed, or deleted, and every CI job
must pass before a pull request can merge. The protection itself is versioned as
[`.github/rulesets/main-protection.json`](rulesets/main-protection.json) and applied with
`make ruleset-apply`, so a change to what guards `main` shows up in a diff rather than only in the
web UI.

```bash
git switch -c feat/my-change
# ... work ...
git push -u origin feat/my-change
gh pr create
```

Work on the roadmap is organised into **slices**, each with a self-contained brief in
[`docs/dev/slices/`](../docs/dev/slices/). If you are picking up planned work rather than proposing
your own, start there — `docs/dev/STATUS.md` says which slice is next.

- Branch from `main`. Keep PRs review-sized — one concern per PR.
- **Conventional Commits** for every commit and PR title:
  `feat(plugin/postgres): add PITR window discovery`,
  `fix(scheduler): renew lease during long backups`,
  `docs(adr): record sandbox provider decision`.
  Scopes follow the package layout. `feat!:` or a `BREAKING CHANGE:` footer for breaking changes —
  these drive the release notes and version bumps.
- Run `make lint test` before pushing. CI runs the same checks plus `buf breaking` and
  `govulncheck`.
- **Update `README.md` in the same PR** whenever your change alters what Fleetward does, how it is
  run, its architecture, or its stage. The architecture and flow diagrams are Mermaid blocks in the
  README itself — GitHub renders them, so keeping them accurate costs one edit, and letting them
  drift misleads every future reader.

## Tests

- Table-driven tests are the default.
- **Integration tests must not require a pre-installed database engine.** Use `testcontainers-go`
  so a fresh clone plus Docker is sufficient. Tag them `//go:build integration`.
- Target ≥70% coverage on `internal/`, and 100% of the conformance surface for a plugin.

## Changing the plugin contract

`api/proto/fleetward/v1/plugin.proto` is a public interface that third-party plugins implement.

- Additive changes (new fields, new RPCs, new capability flags) are usually fine.
- `buf breaking` runs in CI against `main` and will block anything else. If a breaking change is
  genuinely necessary, it needs an ADR explaining why and a migration note for plugin authors.
- Run `make proto` and commit the generated code.

## Writing an engine plugin

This is the highest-value contribution to Fleetward. Read
[`docs/dev/writing-an-engine-plugin.md`](../docs/dev/writing-an-engine-plugin.md). In short: implement
the harness in `internal/plugin/sdk`, declare your capabilities honestly, and make the conformance
suite pass.

Declaring a capability you do not actually support is the one thing that will get a plugin rejected
outright — core trusts that matrix to decide what it is safe to do to someone's production database.

## Documentation

One rule, and it is the one this project has actually got wrong before:

> **Documentation describes what is. Slice briefs describe what will be. The two never mix in one
> file.**

Forward-looking behaviour in a reference document is marked `> Planned.` or it is not written. This
sounds pedantic until you find a security policy claiming an authorization layer that was never
built, or a repository-layout tree listing four packages that do not exist — both of which happened
here, because the documentation was written from the architecture rather than from the code.

Where a claim can be checked mechanically it should be. `make docs-check` runs
[`tools/docscheck`](../tools/docscheck), which is a required CI job. It asserts that every path the
documentation names exists, that every relative link resolves, that every control-plane package
appears in a layout tree, that decision records carry their alternatives and that supersessions
agree at both ends, that the CI jobs the ruleset requires are the ones the workflow defines, and
that retired vocabulary stays retired.

A path that genuinely cannot exist yet goes in [`docs/.docscheck-allow`](../docs/.docscheck-allow),
scoped to the one file allowed to name it and carrying the reason. An allowance without a reason is
refused, and one nothing uses any more is reported.

Where a claim cannot be checked — and "this prose is still true" cannot — it is a line in the pull
request template, and answering it honestly is part of the work.

## Architecture decisions

Anything that would touch more than one package, change the plugin contract, or alter an external
interface needs an ADR in `docs/adr/`. Copy the format of an existing one: Context, Decision,
Consequences, Alternatives considered. Start from [the index](../docs/adr/README.md).

ADRs are not rewritten. A decision is changed by a new ADR that supersedes it — and the superseded
one gets a reciprocal note, so the relationship is visible from both ends. A detail that simply
turned out differently gets a dated correction note at the top of the record rather than a silent
edit.

## Reporting bugs

Include the Fleetward version, the engine and version involved, and what you expected versus what
happened. For anything security-related, **do not open an issue** — see [`SECURITY.md`](SECURITY.md).

## Code of conduct

Participation is governed by [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
