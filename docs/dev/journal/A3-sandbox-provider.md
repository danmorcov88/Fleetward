# A3 — The Docker sandbox provider, with teardown guaranteed

- **Delivered:** 2026-08-29 ([#25](https://github.com/danmorcov88/Fleetward/pull/25))
- **Brief:** [A3-sandbox-provider.md](../slices/A3-sandbox-provider.md)

`internal/controlplane/sandbox` provisions a throwaway database container from a plugin's declared
`SandboxTemplate` and guarantees it is destroyed. `cmd/fleetward` constructs the provider, runs the
orphan sweep at startup, and registers it as a non-critical readiness component. No plugin declares
a template yet, and none needs to until A5 — the integration tests supply their own.

Decisions worth carrying forward:

- **Core generates the sandbox identity; the template places it** —
  see [ADR-0020](../../adr/0020-sandbox-credentials-from-template-placeholders.md). This was the one
  place the engine-agnosticism rule was genuinely under pressure: `Credentials()` has to carry a
  username and password, and `SandboxTemplate` has no field for either. Core renders `env`,
  `command`, and `readiness_command` as Go templates against `{{ .Username }}`, `{{ .Password }}`,
  `{{ .Database }}`, and `{{ .Port }}`. Every sandbox therefore gets a distinct password with a
  lifetime of minutes, and nothing is compiled into a plugin binary.
- **The Docker SDK is `github.com/moby/moby/client`, not `github.com/docker/docker/client`.** The
  brief named the latter as "already in `go.sum` via testcontainers"; that stopped being true at
  testcontainers-go v0.43, which moved to the split-out `moby/moby/client` and `moby/moby/api`
  modules. `docker/docker` survives in `go.sum` only as a test dependency of `golang-migrate`, so
  importing it would have added a large module for no reason. The brief's actual decision — the
  Docker SDK directly, not testcontainers, because Ryuk's cleanup is scoped to a test session and a
  control plane is not one — is unchanged. `go.mod` gained no new module, only two promotions from
  indirect to direct.
- **A tag is never guessed.** `ResolveTag` uses `tag_template` when it renders to something usable
  and `default_tag` otherwise, and errors when neither produces a tag. There is deliberately no
  fallback to `latest`: verifying a backup against the wrong engine version is worse than reporting
  that it could not be verified.
- **The image repository is validated, and may not carry its own tag.** `postgres:15` is rejected
  while `registry.internal:5000/db/postgres` is allowed. A repository that smuggles in a tag would
  let a plugin decide which version core believes it verified against.
- **The published port has to be polled for, not read once.** `ContainerStart` returning does not
  mean the port is mapped — Docker Desktop sets its proxy up asynchronously, so an inspect issued
  immediately after start reports an empty binding on the platform this project is developed on.
  This cost an hour; it is the single non-obvious thing in the Docker implementation.
- **Readiness needs two consecutive successes.** A PostgreSQL container runs a temporary server
  during initdb and then restarts it, so one success is not evidence the server answering will
  still be there a second later. The readiness command should also be scoped to TCP (`-h 127.0.0.1`)
  rather than the unix socket, or it will reach the temporary server.
- **Sandboxes bind to loopback when the daemon is local.** A sandbox holds a full copy of a
  production database behind a password that exists for minutes; publishing it on every interface
  by default is not a trade worth making. A remote daemon is the exception, because a loopback
  binding there is unreachable by definition.
- **Sweep is scoped by an owner label**, so a running control plane can sweep without destroying its
  own live sandboxes. That is not enough to make two control planes safe on one Docker daemon: the
  second to start would sweep the first's sandboxes. Such a deployment needs a distinct
  `FLEETWARD_SANDBOX_LABEL_PREFIX` per control plane, and the failure would otherwise look like a
  random verification failure rather than a configuration mistake.
- **The dev stack needed two changes to make verification actually possible in it**, and the
  compose smoke test found both by going `degraded`. The control-plane image runs unprivileged, and
  the Docker socket grants access by group — group 0 inside a Docker Desktop VM, the host's `docker`
  GID on Linux — so `group_add: ["${FLEETWARD_DOCKER_GID:-0}"]` defaults to the macOS answer and CI
  discovers the Linux one with `stat -c '%g'`. And because the control plane is itself a container
  there, a sandbox's published host port is unreachable from it: `FLEETWARD_SANDBOX_NETWORK` puts
  sandboxes on the stack's network to be addressed by container name. When a network is configured
  the provider publishes no host port at all — on a shared network nothing reads it, and a sandbox
  holding a copy of production data should not sit on a host port for free.
- **`TestMain` fails the integration run if a single labelled container survives.** The acceptance
  check in the brief was `docker ps -a --filter "label=fleetward.sandbox"` being empty; a suite that
  passes while leaking has tested the wrong thing, so CI enforces it rather than a human running it.
- **Not built, deliberately:** resource limits, quotas, and any scheduling policy across concurrent
  sandboxes. The scope fence excludes them, and nothing yet runs two verifications at once — but a
  fleet of fifty servers verifying on a schedule will, and `SandboxConfig` has no knob for it today.

