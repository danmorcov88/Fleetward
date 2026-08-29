# Slice A3 — Sandbox provider

> **Delivered.** Two things in this brief did not survive contact with the code; both are recorded
> under [Findings](#findings) at the end, and the delivered decisions are in
> [`docs/dev/STATUS.md`](../STATUS.md).

## Goal

Provision a throwaway database container from a plugin's declared `SandboxTemplate`, and guarantee
it is destroyed.

## Why now

Verification restores a backup into an isolated instance. Nothing else in the product needs
containers, so this is the one place the capability lives — and it is worth building on its own,
before backup and restore, because the failure mode is silent. A sandbox that leaks does not break
a test; it consumes a machine slowly until someone notices.

Building it standalone means it can be tested for the thing that actually matters — that nothing
survives — rather than incidentally, as part of a larger flow.

## Preconditions

- A1 delivered. `Capabilities.sandbox_template` exists in the contract but no plugin populates it
  yet, and none needs to for this slice.
- `config.SandboxConfig` already exists with `Provider`, `DockerHost`, `StartupTimeout`,
  `MaxLifetime`, `Network`, and `LabelPrefix`.

## Design decisions already made

**Core provisions from what the plugin declares.** The image repository, tag template, environment,
port, and readiness command all come from `SandboxTemplate` inside `Capabilities`. Core must never
contain a lookup table of engines — that is the rule the whole plugin architecture rests on.

**Docker now, Kubernetes later.** The interface is `SandboxProvider`; the Docker implementation is
one satisfier. Nothing outside the package may reference Docker types.

**Use the Docker SDK directly, not testcontainers.** Testcontainers is already in the module graph
and does this well, but its cleanup guarantee comes from Ryuk, a reaper tied to a *test session*
lifecycle. A long-lived control plane is a different shape. Keep testcontainers for tests; use
`github.com/docker/docker/client` for production.

**Cleanup is defended three times over**, because one mechanism is not enough for something that
leaks silently:

1. `defer` on the normal path, including on panic.
2. `MaxLifetime` — a container older than the ceiling is destroyed regardless of state, catching a
   verification that hung rather than failed.
3. An **orphan sweep at startup**, finding containers by label. This is the one that saves you after
   the control plane is killed mid-verification, which is exactly when a leak happens.

`SandboxConfig.LabelPrefix` exists for the third.

## Files

**New**

- `internal/controlplane/sandbox/sandbox.go` — the `Provider` and `Sandbox` interfaces, the spec
  type, and errors.
- `internal/controlplane/sandbox/docker.go` — the Docker implementation.
- `internal/controlplane/sandbox/docker_test.go` — `//go:build integration`; a real daemon.
- `internal/controlplane/sandbox/tag.go` — resolving `tag_template` against a discovered version.
- `internal/controlplane/sandbox/tag_test.go` — unit tests for tag resolution.

**Modified**

- `cmd/fleetward/main.go` — construct the provider, run the orphan sweep at startup, register it as
  a non-critical readiness component.

Note that `internal/controlplane/sandbox/` is a package the original layout did not name. That is
fine and worth stating in the commit: it is core's concern, not a plugin's, and it does not belong
under `backup/`.

## Suggested shape

```go
// Spec is what core needs to stand up a sandbox. Every field originates in the plugin's
// SandboxTemplate; core adds only identity and lifetime.
type Spec struct {
    EngineType    string
    EngineVersion string        // from Discover, used to resolve the image tag
    Template      *fwv1.SandboxTemplate
    Labels        map[string]string
    Lifetime      time.Duration
}

type Provider interface {
    Provision(ctx context.Context, spec Spec) (Sandbox, error)
    // Sweep destroys sandboxes left behind by a previous process. Called at startup.
    Sweep(ctx context.Context) (int, error)
    HealthCheck(ctx context.Context) error
    Close() error
}

type Sandbox interface {
    ID() string
    // Credentials for connecting to the sandbox, ready to hand to a plugin's Restore.
    Credentials() *fwv1.Credentials
    // Destroy is idempotent and safe to call from a defer.
    Destroy(ctx context.Context) error
}
```

## Reuse, do not rewrite

| What | Where |
|---|---|
| Configuration | `config.SandboxConfig` — already carries every knob this needs |
| Docker client | `github.com/docker/docker/client` — already in `go.sum` via testcontainers |
| Readiness registration | `api.Health.Register(name, critical, checker)` in `internal/controlplane/api/health.go` |
| Structured errors | `sdk.Error` constructors, if the failure will cross to a plugin |

## Traps

**`Destroy` must be callable twice.** It runs from a `defer` and possibly again from the sweep or
the lifetime reaper. Treat "container not found" as success.

**Do not derive the destroy context from the request context.** Verification failing usually means
the context was cancelled — and a cancelled context cannot be used to clean up. Use
`context.WithoutCancel` plus a fresh timeout, the same pattern already used in
`api.Server.Shutdown`.

**Pull the image before starting, with its own timeout.** A first run on a clean machine pulls
several hundred megabytes. Rolling that into the startup timeout makes the first verification look
like a hang.

**Readiness is not "the container is running".** A PostgreSQL container is running long before it
accepts connections, and during initdb it restarts. Use the template's `readiness_command` with
polling, and remember the same double-start problem the A1 integration tests hit.

**Bind to an ephemeral host port, or none at all.** Fixed ports collide the moment two verifications
run at once. Ask Docker for a mapped port and read it back.

**Label every container, always.** A container created without the label is invisible to the sweep,
which means it leaks forever. Set labels at creation, never afterwards.

## Scope fence

Not in this slice:

- Restoring anything into the sandbox. That is A5.
- Any plugin declaring `supports_sandbox_restore` or populating `sandbox_template`. That is A5 too.
- A Kubernetes implementation. The interface must permit it; nothing more.
- Resource limits, quotas, or scheduling policy across concurrent sandboxes. Note them if you find
  them missing, but do not build them.

## Done when

```bash
make test-integration   # includes the Docker-backed sandbox tests
```

The integration test must prove, not assume, that nothing survives:

- a sandbox is provisioned and becomes ready
- `Credentials()` connects successfully
- `Destroy` removes it, and a second `Destroy` returns nil
- a panic mid-use still leaves nothing behind
- `Sweep` finds and removes a container created with the label and then abandoned

```bash
docker ps -a --filter "label=fleetward.sandbox" --format '{{.Names}}'
# empty, after the full test run
```

This last check is not left to a human: `TestMain` in `docker_test.go` runs it and fails the suite
if anything survived.

---

## Findings

Two things in the brief above were wrong, and one question it did not ask turned out to be the
important one.

**The Docker SDK import path.** The brief said `github.com/docker/docker/client`, "already in
`go.sum` via testcontainers". That stopped being true at testcontainers-go v0.43, which moved to
the split-out `github.com/moby/moby/client` and `github.com/moby/moby/api` modules;
`github.com/docker/docker` remains in `go.sum` only as a test dependency of `golang-migrate`, so
importing it would have pulled a large module into the build for nothing. The implementation uses
`moby/moby/client`, which added no module — only two promotions from indirect to direct. The
decision the brief was actually making — the Docker SDK directly rather than testcontainers,
because Ryuk's cleanup is tied to a test-session lifecycle and a long-lived control plane is not
one — is unchanged and correct.

**The suggested `Spec`/`Provider` shape omitted the hard part.** `Sandbox.Credentials()` has to
return a username and a password, and `SandboxTemplate` has no field for either — the knowledge
that `POSTGRES_PASSWORD` means something lives in the plugin, which is exactly where core is
forbidden from looking. Resolving this needed a decision rather than an implementation, and it is
recorded as [ADR-0020](../../adr/0020-sandbox-credentials-from-template-placeholders.md): core
generates the identity, the template places it via `{{ .Username }}`, `{{ .Password }}`,
`{{ .Database }}`, and `{{ .Port }}`. `Spec` gained `Username` and `Database` overrides, which A5
will need so a restore target can match its source.

**One trap the brief did not name, which cost the most time.** `ContainerStart` returning does not
mean the container's port is published. Docker Desktop sets its port proxy up asynchronously, so an
inspect issued immediately after start reports an empty binding — on macOS, which is this project's
development platform. The mapped port has to be polled for, exactly like readiness.
