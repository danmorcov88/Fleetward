# ADR-0019: The REST API is served by in-process grpc-gateway handlers, with no gRPC listener

- **Status:** Accepted
- **Date:** 2026-07-28

## Context

ADR-0004 fixed the shape of the API: services are defined as Protobuf, spoken internally as gRPC,
and exposed externally as REST through `grpc-gateway`. Slice A2 is the first slice that has to
actually serve one of those services, so it is the first that has to decide *how*.

The usual grpc-gateway deployment runs a gRPC server on one port and a gateway on another, with the
gateway dialling the gRPC server over loopback. `internal/config` already carries a `GRPC
ServerConfig` alongside `HTTP`, which reads as an intention to do exactly that.

There is a second option. Every generated service also has a
`RegisterXHandlerServer(ctx, mux, server)` function, which wires the HTTP handler directly to the
service implementation as an ordinary Go call. No socket, no listener, no dial.

## Decision

The control plane registers its services with `RegisterXHandlerServer` and mounts the resulting
`runtime.ServeMux` on the existing HTTP server's mux at `/api/v1/`. **No gRPC listener is opened.**

Two details of that mux are part of this decision:

- JSON uses `UseProtoNames` and `EmitDefaultValues`. Field names in responses therefore match the
  `.proto` — `environment_id`, not `environmentId` — because the contract is the documentation, and
  an empty list appears as `[]` rather than being omitted, so clients need no special case for a
  fresh installation.
- Unknown request fields are rejected rather than discarded, and errors are rendered in the same
  problem-details shape as the rest of the API (`CLAUDE.md` §8).

`config.GRPC` stays where it is. It becomes live the moment there is a reason for it.

## Consequences

- One listener, so one place to configure TLS and, later, one place to enforce authentication. A
  loopback gRPC port carrying every inventory and backup call is a real attack surface on a host
  that also runs the databases' monitoring account, and not opening it is cheaper than securing it.
- No serialization round trip per request, and a Go stack trace that runs from the HTTP handler into
  the service without a network hop in the middle.
- Cost: **server-streaming RPCs cannot be served this way.** `grpc-gateway`'s in-process handlers
  support unary calls only. `CollectMetrics`, `Backup`, and `Restore` are plugin-facing RPCs, not
  control-plane ones, so nothing in the contract needs streaming over REST today — but if a
  control-plane RPC ever does, this decision is what has to change first, and it is a small change:
  add the listener, swap `RegisterXHandlerServer` for `RegisterXHandlerFromEndpoint`.
- Cost: no gRPC surface for an external client that would prefer one. Nothing is asking for it.

## Alternatives considered

- **gRPC listener plus a gateway dialling loopback.** The conventional layout, and the right one
  once there is a second consumer of the gRPC surface or a streaming control-plane RPC. Today it
  buys nothing and costs a port that has to be secured.
- **Hand-written HTTP handlers.** Would drift from the Protobuf contract and from the generated
  OpenAPI document, which is exactly what ADR-0004 exists to prevent.
