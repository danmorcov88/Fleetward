# ADR-0029: The OpenAPI document is generated to match the wire, and the web app's types come from it

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B4 — the Estate Overview
- **Relates to:** [ADR-0004](0004-protobuf-buf-grpc-gateway.md),
  [ADR-0010](0010-react-frontend-stack.md),
  [ADR-0019](0019-rest-api-without-a-grpc-listener.md)

## Context

[ADR-0010](0010-react-frontend-stack.md) promised that "TypeScript against the generated OpenAPI
spec means API drift is a compile error". Nothing delivered it. `web/src/lib/api.ts` hand-wrote
types for two endpoints, and its own header comment said generated ones were always the plan.

Slice B4 is the first slice that needs more than two endpoints, so it is the first that has to
actually do it. And doing it surfaced the reason nobody had: **the document that was being generated
did not describe the API the control plane serves.** It has been generated, committed, and diffed by
CI since the contract existed, which made it stable — and stable is not the same as correct.

Four mismatches, every one of which a generated client inherits silently:

| The document said | The server sends | Why |
|---|---|---|
| `instanceId`, `problemsOnly` | `instance_id`, `problems_only` | the gateway marshals with `UseProtoNames` ([ADR-0019](0019-rest-api-without-a-grpc-listener.md)) |
| `state: {type: integer, format: enum}` | `"ADHERENCE_STATE_MISSED"` | protojson renders an enum as its name |
| errors are `google.rpc.Status` | RFC 9457 problem details | `problemErrorHandler` in `internal/controlplane/api/gateway.go` |
| `/readyz` absent | `/readyz` exists | it is a hand-written HTTP handler, not a gRPC method |

The first two are the generator's defaults for a gRPC service, applied without knowing how this
gateway was configured. The third is the generator writing what a gRPC service's errors would look
like if nobody had replaced the error handler. The fourth is a genuine gap: not everything the
server serves is defined in the contract.

Generating a client from that document produces code that compiles and reads nothing — every field
`undefined`, every enum comparison false, and no error anywhere. It is the same failure mode
`tools/docsgen` had before slice B3 fixed it: a generated artifact nobody regenerates is a file that
is confidently wrong.

The document is also a release artifact (`CLAUDE.md` §7.7), so publishing it wrong is a promise made
to every future consumer.

## Decision

**The document is generated to describe the bytes the server actually sends, and the web app's types
are generated from it.**

Two generator options, in `buf.gen.yaml`:

```yaml
  - remote: buf.build/community/google-gnostic-openapi:v0.7.0
    out: api/openapi
    opt: naming=proto,enum_type=string
```

`naming=proto` makes every field and query parameter snake_case, matching `UseProtoNames`.
`enum_type=string` renders an enum as the union of its value names, matching protojson. Both were
verified against the installed toolchain before being written down, not assumed from documentation.

**The two things the contract does not describe stay hand-written, and say so.** `/readyz` and the
problem-details error shape live in `web/src/lib/api.ts` with a comment explaining why they are
there. `default_response=false` would delete the wrong error schema, but it would document nothing
in its place; an undocumented error is honest, and a hand-written type beside a comment is more
honest still.

**Types only, not a generated client.** `openapi-typescript` emits one module of types and no
runtime. `openapi-fetch`, `hey-api` and `orval` each ship a runtime that would still need overriding
for the error shape and would still not know about `/readyz` — a dependency that removes nothing.
The existing forty-line `request<T>` helper stays.

**The generated TypeScript is held to the rule the Go output is held to.** `make proto` regenerates
it; it is committed; CI regenerates and diffs it in the same step. A generated file that is never
regenerated is exactly the problem this record exists to fix, and exempting the new one would repeat
it.

## Consequences

- **API drift is a compile error, as ADR-0010 said it would be.** A field renamed in the `.proto`
  fails `npm run build`, which CI already runs.
- **It found a bug in the first minute.** `System.tsx` rendered `version.data.platform`, a field
  `GetVersionResponse` has never carried. It had rendered blank since the day it was written, and
  the hand-written type it was checked against was written from the same wrong assumption. That is
  one screen, one field, and the only surface that existed; the estate view has four columns over
  fifty rows.
- **The document's diff is large once.** Every field of every message is renamed and every enum
  changes shape. It is a correction, and it is the last one of its size.
- **The OpenAPI release artifact becomes true**, before `v0.1.0` publishes it to people who will
  write clients against it.
- **Cost: the web app is now part of the code-generation pipeline.** `make proto` needs `npm`
  installed, and the "Protobuf contract" CI job now sets up Node. That is the price of one contract
  with two consumers rather than two vocabularies.
- **Cost: `naming=proto` is not the generator's default**, so it looks like a mistake to anyone who
  does not know why. That is what this record is for, and the option carries a comment beside it.

## Alternatives considered

- **Leave the document as generated and hand-write the types.** What was already happening. It cost
  a blank field on the only screen that existed, and it would cost more on a screen whose entire
  claim is that its four columns are correct.
- **Leave the document wrong and post-process it.** A script that rewrites the generator's output
  is a second generator with no tests, and the release artifact would still be whichever of the two
  someone happened to publish.
- **Change the gateway to camelCase instead, so the document becomes right by accident.** This
  inverts the decision in [ADR-0019](0019-rest-api-without-a-grpc-listener.md) — that the contract
  is the documentation and the JSON uses the names in the `.proto` — to satisfy a generator's
  default. It would also break every existing client, including the CLI.
- **A full generated client (`openapi-fetch`, `hey-api`, `orval`).** Reasonable, and reconsiderable
  when there is a mutation surface to generate. Today the app makes four GETs, the error shape needs
  a hand-written override either way, and `/readyz` is outside the document.
- **Generate the TypeScript at build time instead of committing it.** Removes the diff check and
  with it the property that makes the whole thing trustworthy: that the committed artifact and the
  contract cannot disagree.
