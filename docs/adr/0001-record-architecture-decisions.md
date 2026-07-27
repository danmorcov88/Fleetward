# ADR-0001: Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward is a multi-month project worked on across many sessions, by humans and AI assistants
alike. Decisions made in week 2 must survive to week 26 without being silently relitigated by
someone who lacks the original context. Chat history and code comments both decay: the first is
not durable, the second is not discoverable.

## Decision

We record every architecturally significant decision as an Architecture Decision Record in
`docs/adr/`, numbered sequentially, in the format used by this file: Context / Decision /
Consequences / Alternatives considered.

- All decisions from §2 of the implementation brief are recorded as ADR-0002 through ADR-0014.
- A decision is "architecturally significant" if reversing it would touch more than one package,
  change the plugin contract, or alter an external interface.
- ADRs are immutable once Accepted. To change a decision, write a new ADR that supersedes the old
  one and mark the old one `Superseded by ADR-XXXX`.
- `CLAUDE.md` indexes the ADRs so every new session inherits the context.

## Consequences

- Every session starts with the same understanding of what has already been settled.
- There is a small, permanent cost to making a decision: writing it down.
- Disagreement is channeled into "write a superseding ADR", not into ad-hoc code changes.

## Alternatives considered

- **Comments in code.** Not discoverable before you have already found the code.
- **A wiki.** Drifts from the repo and is not versioned with the change it justifies.
