# ADR-0010: React 19 + TypeScript + Vite + Tailwind + shadcn/ui + TanStack

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

The UI must render an estate of potentially thousands of instances (virtualized grid), adapt its
tabs to each engine's declared capabilities, and present a backup/restore wizard where a
mis-click is expensive. Design tokens and mockups arrive later, from a separate workstream, so the
skeleton must be restyleable without rewriting.

## Decision

React 19 + TypeScript, built with Vite. Tailwind CSS with shadcn/ui components. TanStack Query for
server state, TanStack Table for the estate grid, TanStack Router for routing.

## Consequences

- shadcn/ui components live in our repo rather than in `node_modules`, so applying design tokens is
  editing our own code — exactly the restyle-later property we need.
- TanStack Table virtualizes the estate grid without us writing windowing logic.
- TanStack Query removes most hand-written cache, refetch, and loading-state code around a
  polling-heavy dashboard.
- TypeScript against the generated OpenAPI spec means API drift is a compile error.
- Cost: a JS toolchain in an otherwise Go repo, and Tailwind's verbose class strings.

## Alternatives considered

- **Server-rendered Go templates (htmx).** Fewer moving parts, but a poor fit for a virtualized grid
  and a multi-step restore wizard with live progress.
- **A component library with baked-in theming (MUI, Ant).** Fights custom design tokens instead of
  absorbing them.
