# B8 — Self-observability, or the slice that answers "how do I monitor the thing that monitors my backups"

Seven slices have built a product an operator is supposed to rely on being awake. B7 made that
literal: a webhook fires because Fleetward noticed something, which means the value of the whole
installation now depends on a process that is running, ticking, and evaluating. There is no way to
find out whether it is.

`internal/telemetry/otel.go` builds a tracer provider and a meter provider and installs them
globally. Nothing in the tree has ever called `otel.Tracer` or `otel.Meter`. Not one span is
started, not one instrument is obtained. There is no `/metrics`. A 403 emits no metric, a backup
that took forty minutes emits no metric, and "did the evaluation pass run last night" is answerable
only by grepping a log — which `STATUS.md` says under "Known broken" in as many words, and which
[ADR-0038](../../adr/0038-alert-evaluation-is-a-pass-over-the-estate.md) accepted as B7's cost.

VictoriaMetrics has been in the compose stack since the foundation slice, health-checked on every
start, holding nothing. Nothing has ever written a sample to it.

---

## Goal

**Fleetward exposes its own health as metrics on `/metrics` and as spans on four operations, so that
an operator can monitor the monitor with the tooling they already run.**

## Why now

**An operator asked to install this will ask how to monitor it, and the answer cannot be that they
cannot.** That question got sharper one slice ago. Before B7 an unnoticed control-plane stall meant
a stale dashboard; after B7 it means an alert that never fires, which is indistinguishable from an
estate with nothing wrong. Monitoring is the only thing that tells those two apart, and
[ADR-0024](../../adr/0024-production-readiness-is-a-slice-property.md) says a capability ships with
its limits rather than waiting for a readiness phase.

**It is the last slice before the release.** B9 tags `v0.1.0`, publishes an image and signs it.
Shipping a control plane that cannot be scraped and then telling people to install it is the shape
of promise this project has already decided not to make — `SECURITY.md` once claimed an
authorization layer that did not exist, and the whole of B6 plus ADR-0024 came out of that.

**The wiring is paid for and idle.** `telemetry.Setup` already builds resource attributes from
`version.Version`, installs W3C propagators whether or not telemetry is enabled, and returns a
composite shutdown. Its own comment already promises that "instrumentation code needs no
`if enabled` guards at any call site". That promise has never been tested, because there is no
instrumentation code.

**Three known-broken entries collapse into one metric each.** "Alert evaluation leaves no job row,
so `job list` cannot answer *did it run last night*" becomes a histogram's `_count`. "A notification
can be lost" becomes a `dropped` counter beside a `delivered` one. "Fleetward cannot be observed"
goes away.

**It closes the loop on a component that has run empty for eight slices.** The dev stack's
VictoriaMetrics gets its first sample in this slice, by scraping the endpoint this slice adds.

## Preconditions

All hold on `main` at `e8ec8dc` (the B7 merge). Every one was verified by reading the code.

### The telemetry package is built and unused

- **`telemetry.Setup(ctx, cfg, log) (ShutdownFunc, error)`** — `internal/telemetry/otel.go:31`. It
  installs `propagation.TraceContext` and `Baggage` unconditionally, then returns a no-op shutdown
  when `cfg.Enabled` is false, *before* constructing any provider.
- **The global providers are therefore OTel's no-op ones when telemetry is disabled**, which is why
  `otel.Meter(...)` and `otel.Tracer(...)` are safe to call from anywhere with no guard. There is no
  nil meter to check for; the failure mode the constraint warns about is a `nil` *instrument* a
  package stored during a failed init, not the meter itself. See the traps.
- **`grep -rn 'otel\.' --include='*.go' internal/ cmd/ plugins/` returns nothing outside
  `internal/telemetry/`.** Zero call sites, exactly as `STATUS.md` says.
- **`TelemetryConfig`** — `internal/config/config.go:186` — carries `Enabled`, `OTLPEndpoint`,
  `OTLPInsecure`, `ServiceName`, `SampleRatio`. `FLEETWARD_TELEMETRY_ENABLED` defaults to **false**
  (`config.go:373`). There is no validation for the telemetry block at all.
- **`internal/telemetry/logging.go` is what the twelve files importing this package actually use** —
  `NewLogger`, `WithRequestID`, `WithJobID`, `WithPrincipal`. None of them touches `otel.go`.

### The four operations each already have exactly one funnel

Verified by reading each, because instrumenting two places that both claim to be "the backup" is how
a duration metric ends up double-counted.

- **An API request** — `(*Server).middleware` at `internal/controlplane/api/server.go:166` wraps the
  whole mux, including `/healthz`, `/readyz` and the grpc-gateway subtree. It already establishes the
  request id, the principal, and a `statusRecorder` that captures the status code, and its deferred
  closure already runs after the handler with the duration in hand.
- **A backup run** — `(*Service).execute` at `internal/controlplane/backup/service.go:414`. Both
  entry points funnel here: `RunBackup` (`:296`, asynchronous, from the API) and `RunBackupSync`
  (`:348`, synchronous, from the scheduler). It already takes `started := time.Now()`.
- **A verification** — `(*Service).verify` at `internal/controlplane/backup/verify.go:311`. Same
  shape: `RunVerification` (`:53`) and `RunVerificationSync` (`:124`) both reach it, and it already
  switches on `result.status` over the three terminal verdicts.
- **The alert evaluation pass** — `(*Service).Evaluate` at
  `internal/controlplane/alerts/evaluate.go:124`, reached from `maybeEvaluateAlerts`
  (`internal/controlplane/scheduler/scheduler.go:311`) through the `Runner` interface. It returns an
  `EvaluationResult` (`evaluate.go:96`) with `RulesConsidered`, `Firing`, `Opened` and `Resolved`.

### The authorization decision has exactly one funnel too

- **`guarded[Req, Resp]`** at `internal/controlplane/authz/enforce.go:53` is the single generic
  function every one of the 24 guarded RPCs calls. Its own comment says so: "there is exactly one
  copy of this logic and 24 one-line call sites".
- It holds a `Decision` carrying `Method`, `Rule.MinRole`, `EffectiveRole` and `Allowed` on **both**
  outcomes — `Guard.Check` returns a `Decision` on refusal precisely so the refusal can be recorded
  (`guard.go:90`). That is the 403 metric's whole input, already assembled.
- The method string is bounded: `authz.Policies` is a closed table, and
  `internal/controlplane/authz/coverage_test.go` fails if a generated service interface grows a
  method the table does not name. So `rpc.method` as a label is bounded by the *contract* rather than
  by traffic.

### The delivery path already counts what is lost

- **`(*Dispatcher).Enqueue`** — `internal/controlplane/alerts/deliver.go:167` — increments
  `d.dropped` and logs when the bounded queue is full.
- **`(*Dispatcher).deliver`** — `:212` — loops destinations, calls `attempt`, and logs delivered or
  failed per destination, with `dest.Kind` (`webhook | smtp`) in hand.
- So `delivered`, `failed` and `dropped` are three existing branches, not three new decisions.

### The HTTP surface can be extended without touching `routes()`

- **`(*Server).RegisterSessions`** — `internal/controlplane/api/sessions.go:28` — is the established
  idiom: `main` builds a dependency and hands it to a `Register…` method that mounts handlers on the
  mux. `/metrics` follows it exactly.
- **`http.ServeMux.Handler(r)` returns the matched pattern** without serving the request, which is
  the only way the middleware can label a request by route rather than by path. See the traps: the
  gateway is mounted on one pattern and raw paths carry backup UUIDs.
- **`api.WriteProblem`** — `server.go:261` — is exported and is the single error shape, so a refused
  scrape renders like every other refusal.

### The dev stack can prove it end to end

- **VictoriaMetrics is in `docker-compose.yml:73`** at `v1.109.1`, on `:8428`, health-checked, with a
  `vm-data` volume. It supports `--promscrape.config`, so it can scrape rather than only receive.
- **Nothing writes to it.** `tsdb.NewVictoriaMetrics` is constructed at `cmd/fleetward/main.go:145`
  and registered as a non-critical health check; `Write` has no caller anywhere in the tree.
- **The bootstrap credential is a known value in the compose file** —
  `FLEETWARD_AUTH_BOOTSTRAP_TOKEN: fwt_devbootstrap_0000000000000000` (`docker-compose.yml:210`) —
  and the comment above it explains why authorization is *on* in the development stack: enforcement
  nothing exercises is enforcement that was never built. A scrape config using that token exercises
  the authenticated path rather than bypassing it.

### The dependency is not present and has to be added

- **`go.sum` contains no `prometheus` entry at all.** `internal/storage/tsdb/prompb` is a *generated*
  copy of the remote-write wire format, kept precisely so the client library is not a dependency.
- `go.opentelemetry.io/otel/exporters/prometheus@v0.68.0` is the release that pairs with
  `sdk/metric v1.46.0`, which is what `go.mod:25` already pins. It requires
  `github.com/prometheus/client_golang v1.24.1`. That is a decision rather than a detail — see
  design decision 6.

### Two documentation claims are already stale, and this slice's docs pass owns them

- **`docs/architecture.md:20`** still marks `ALERT["Alerting"]:::planned`, and the paragraph at `:65`
  still says "Today alerting does not exist". B7 shipped it. Fix both here; a diagram that describes
  the previous version of the project is worse than no diagram.
- **`README.md:646`** says "Fleetward emits no metrics about itself" under Project status. That is
  what this slice is for.

## Design decisions already made

Settled here so they are not relitigated mid-slice. Two of them become ADRs.

### 1. Four operations, and these four

The roadmap says "spans on four operations". They are, and the reason each earns its place:

| Operation | Span started at | Answers |
|---|---|---|
| An API request | `api.(*Server).middleware` | who is being refused, what is slow, what is erroring |
| A backup run | `backup.(*Service).execute` | why did that instance's backup take forty minutes |
| A verification | `backup.(*Service).verify` | how long the product's differentiator takes, and how it ended |
| The alert evaluation pass | `alerts.(*Service).Evaluate` | did it run last night |

**The authorization decision is not a fifth operation.** It happens inside an API request, so
`guarded` emits a counter and calls `trace.SpanFromContext(ctx).SetAttributes(…)` to put the RPC
method and the decision onto the span the middleware already started. The gateway runs in-process
([ADR-0019](../../adr/0019-rest-api-without-a-grpc-listener.md)), so the request context reaches the
decorator unbroken, and `SetAttributes` on a non-recording span is a no-op. One span per request with
the fine-grained name attached, rather than two spans somebody has to join.

### 2. Fleetward's own metrics are a namespace of their own — ADR-0041

Three namespaces, with a boundary a session can apply without asking:

- **`http.server.*`** — where OTel semantic conventions already name the thing being measured, use
  the semconv name unchanged: `http.server.request.duration`, with `http.request.method`,
  `http.route`, `http.response.status_code`. Inventing `fleetward.http.requests` would discard the
  dashboards and alert rules every operator already has for it.
- **`fleetward.*`** — where semconv has no name because the thing is a Fleetward concept: a backup, a
  verification, an alert pass, an authorization decision.
- **`db.client.*`** — metrics about a *monitored database*, which is
  [ADR-0011](../../adr/0011-opentelemetry-and-semconv.md), and which remains uncollected and
  deferred.

The third boundary is what makes `/metrics` safe to be a different thing from remote-write.
`/metrics` exposes the first two and never the third: estate metrics go to VictoriaMetrics through
`internal/storage/tsdb` if something ever collects them, and Fleetward's own go out through the pull
endpoint.

One reconciliation to record rather than discover later: `internal/storage/tsdb/tsdb.go:21` names its
remote-write labels `fw_tenant_id`, `fw_instance_id`, `fw_engine_type`, while this slice's OTel
attribute keys are dotted and render as `fleetward_instance_id`. Nothing has ever written a sample
through the `fw_*` path, so there is no data to migrate and no reason to touch it now; the ADR says
the two are reconciled when database metric collection lands, so that a third spelling is never
invented.

### 3. A label is bounded by the estate; a span attribute is not — ADR-0041

The rule, in one sentence, because "keep cardinality low" is advice and this is a test:

> **A metric label's value set must be bounded by the size of the estate. A value set that grows with
> time is forbidden.**

Permitted, with what bounds each: `instance.id` (the estate, about fifty), `engine.type` (eight),
`backup.method` (a handful per engine), `rpc.method` (the policy table, 24), `http.route` (the
registered mux patterns), `alert.kind` (the `CHECK` constraint, seven), `alert.severity` (three),
`notifier.kind` (two), and every `outcome` or `status` enum.

Forbidden, and named so nobody has to re-derive the list: `backup_id`, `verification_id`, `job_id`,
`alert_id`, `fingerprint`, `object_key`, and any error message. Every one of those grows every night
forever, and a histogram keyed on one turns VictoriaMetrics into the problem it was installed to
solve.

**A span attribute is a different thing and may carry them.** A span *is* one event; per-event
identifiers are what make it useful, and `backup_id` on a backup span is the point. The credential
rule below applies to both, without exception.

### 4. Nothing that records a metric takes a struct — ADR-0041

The same rule `internal/controlplane/audit/audit.go`'s package comment states, enforced the same way,
for the same reason:

> There is no function in `internal/telemetry` that takes a request message, a `runRequest`, an
> `inventory.Connection`, or an `error`.

This is not caution about a hypothetical. `runRequest.connection` is an `*inventory.Connection`
(`internal/controlplane/inventory/service.go:689`) and its fourth field is `Credentials`. A
convenient `RecordBackup(ctx, req, err)` is one `%v` away from a production database password in a
label, on an exporter that ships to somebody else's collector — and a metric, unlike an audit row, is
re-emitted on every scrape forever.

So every recorder takes named, typed arguments, and an `error` becomes an `outcome` enum at the call
site rather than inside the recorder. The instruments live in one file,
`internal/telemetry/metrics.go`, which is the one place a reviewer reads to know every metric name
and every label key that exists — the same reason `internal/storage/tsdb/tsdb.go:19` keeps its label
names in one block.

### 5. `/metrics` is a scrape of the whole estate, so it needs a tenant-wide `viewer` — ADR-0042

`/metrics` is a new route on the control plane's mux. It is not a gRPC method, so it does not go
through the policy table — a fabricated entry there would break the coverage test, which asserts in
reverse that every policy names a method some generated interface actually has.

It still needs an answer to "who may scrape it", and the answer is **the same credential everything
else needs, at tenant-wide `viewer`**:

- The response carries a series per instance. That is the shape of somebody's database estate — how
  many servers, which engines, which of them fail verification — and B6 exists because "every route
  is open to anyone who can reach the port" was the wrong answer once already.
- Tenant-wide rather than "viewer somewhere" falls straight out of
  [ADR-0035](../../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md): a request that names
  no scope is asking about the whole tenant, and a scrape names no scope. A caller granted three
  servers cannot ask a question about fifty.
- Prometheus, VictoriaMetrics, Grafana Agent and the OTel collector all support `bearer_token` and
  `bearer_token_file` natively. It costs an operator one line.

**The escape hatch, and why it warns rather than refuses.**
`FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=false` serves it unauthenticated, for an installation where the
port is already reachable only from the monitoring network. It warns on every start, in the same
shape as `AUTH_ENABLED=false` and the bootstrap credential, and it is **not** refused in production
the way disabled authentication is: disabling authentication grants control of the estate, while an
open `/metrics` discloses its shape. Those are different sizes of mistake and the configuration
should not pretend otherwise.

### 6. The Prometheus exporter is a dependency, and it is worth it

`/metrics` needs the OTel Prometheus exporter and, transitively,
`github.com/prometheus/client_golang` — roughly seven modules the tree does not have today, in a
repository that deliberately generated its own `prompb` rather than take one of them.

Take it anyway. The exposition format is not "print some lines": counter `_total` suffixing, unit
suffixes, label-value escaping, `NaN` and `+Inf`, histogram bucket ordering and the `le` label,
`target_info` from resource attributes, and `Accept`-driven negotiation are each a place where a
hand-rolled writer is subtly wrong in a way no test written by its own author catches. `prompb` was
generated to avoid a dependency for *twelve lines of protobuf*; this is not that.

Free from it, with no additional decision: `target_info{service_name,service_version}` renders
`version.Version` into the scrape, so no `build_info` metric has to be written.

### 7. Default histogram boundaries are wrong for everything this slice measures

OTel's default explicit bucket boundaries end at 10 seconds. A backup takes minutes to hours, and a
verification pulls a container image first, so with the defaults **every backup and every
verification lands in the `+Inf` bucket** and the histogram answers nothing. Each duration instrument
gets an explicit `sdkmetric.View` with boundaries chosen for it — seconds for HTTP,
minutes-to-hours for backup and verification.

This is the single most likely way this slice ships something that looks instrumented and is not.

### 8. `/metrics` is on by default; OTLP export stays off by default

Two flags, because they are two different things:

| Setting | Default | What it controls |
|---|---|---|
| `FLEETWARD_TELEMETRY_ENABLED` | `false` | **push** to an OTLP collector: spans and periodic metric export |
| `FLEETWARD_TELEMETRY_PROMETHEUS_ENABLED` | `true` | **pull**: whether `/metrics` is served |
| `FLEETWARD_TELEMETRY_PROMETHEUS_AUTH` | `true` | whether a scrape needs a tenant-wide `viewer` |

`/metrics` is the default way to monitor a Go service and needs no collector to exist, so it is on.
OTLP export names an endpoint that must be running, so it stays off. `TelemetryConfig.Enabled`'s doc
comment is narrowed to say it controls OTLP export only — `docs/ops/configuration.md` is generated
from that comment by `tools/docsgen`, so the reference explains the distinction for free.

Consequence for `Setup`: the meter provider is installed when **either** flag is on, with zero, one
or two readers. Tracing is installed only for OTLP, because a span has no pull endpoint. `Setup`
therefore returns a small struct rather than a bare `ShutdownFunc`, so `main` can reach the
`http.Handler`. One call site.

### 9. Instrumentation changes no behaviour, and the tests say so

Every recorder is a call on a global-provider instrument, so with both flags off the providers are
OTel's no-ops and every call is a nil-check and a return. Three rules make that true:

- **No instrument is ever stored in a package-level variable populated by an init that can fail.** A
  `nil` `metric.Float64Histogram` panics on `Record`, and that is the panic the constraint is about.
  Instruments are created once, in `internal/telemetry/metrics.go`, and a creation error yields a
  working no-op instrument plus a log line — never a `nil` stored for later.
- **No recorder returns an error**, so no call site can grow a branch on telemetry.
- **No recorder performs a query, a read, or an allocation the operation would not otherwise do.**
  `EvaluationResult` already carries its counts; `execute` already computes its duration.

### 10. The dev stack scrapes itself, and that is the demonstration

VictoriaMetrics gains `--promscrape.config` pointed at `deploy/dev/victoriametrics/scrape.yml`, which
scrapes `fleetward:8080/metrics` with the compose file's bootstrap token. Ten lines. It gives the
slice a "Done when" that is a real query against a real store rather than a `curl` of a text
endpoint, and it puts the first sample VictoriaMetrics has ever held into it.

## Files

### New

| Path | What |
|---|---|
| `internal/telemetry/metrics.go` | every instrument, every label key, and the recorders — the one file that says what a metric may carry |
| `internal/telemetry/metrics_test.go` | recorders are no-ops with no provider; boundaries are the chosen ones; no label key is on the forbidden list |
| `internal/telemetry/tracing.go` | `Tracer()` and the span helpers, so `otel.Tracer` has one call site |
| `internal/controlplane/api/metrics.go` | `RegisterMetrics`, the scrape authorization check, the handler |
| `internal/controlplane/api/metrics_test.go` | 401 with no credential, 403 for a scoped `viewer`, 200 for tenant-wide `viewer` and for the bootstrap credential |
| `deploy/dev/victoriametrics/scrape.yml` | the dev stack's scrape config |
| `docs/ops/observability.md` | the operator page: what is exposed, what each metric means, who may scrape, and what it will not tell you |
| `docs/adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md` | decisions 2, 3 and 4 |
| `docs/adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md` | decision 5 |
| `docs/dev/journal/B8-self-observability.md` | the close-out entry |

### Modified

| Path | What |
|---|---|
| `internal/telemetry/otel.go` | readers rather than one exporter; views for the duration histograms; return a struct carrying the handler |
| `internal/config/config.go` | `PrometheusEnabled`, `PrometheusAuth`; `Enabled`'s comment narrowed to OTLP |
| `cmd/fleetward/main.go` | `server.RegisterMetrics(...)` beside `RegisterSessions` |
| `internal/controlplane/api/server.go` | span and `http.server.request.duration` in `middleware`; route from `mux.Handler(r)`; `/metrics` demoted to debug in the access log |
| `internal/controlplane/authz/enforce.go` | the decision counter and the span attributes in `guarded` |
| `internal/controlplane/backup/service.go` | span and duration in `execute` |
| `internal/controlplane/backup/verify.go` | span and duration in `verify` |
| `internal/controlplane/alerts/evaluate.go` | span and duration in `Evaluate`; the opened and resolved counters |
| `internal/controlplane/alerts/deliver.go` | the notification counter in `Enqueue` and `deliver` |
| `docker-compose.yml` | the scrape config mount and flag on `victoriametrics` |
| `docs/architecture.md` | alerting is no longer dashed; the stale sentence at `:65`; `/metrics` on the diagram |
| `README.md` | the Project status paragraph; a "Where to go next" row |
| `docs/dev/STATUS.md` | rewritten, B9 next |
| `tools/wikigen/manifest.go` | the observability page, `audience: "run"` |
| `docs/.docscheck-allow` | the allowances this brief needs, removed as each file appears |

The metric set, agreed before it is written rather than accreted:

| Metric | Type | Unit | Labels |
|---|---|---|---|
| `http.server.request.duration` | histogram | s | `http.request.method`, `http.route`, `http.response.status_code` |
| `fleetward.authz.decisions` | counter | `{decision}` | `fleetward.rpc.method`, `fleetward.authz.outcome`, `fleetward.authz.effective_role` |
| `fleetward.backup.duration` | histogram | s | `fleetward.instance.id`, `fleetward.engine.type`, `fleetward.backup.method`, `fleetward.outcome` |
| `fleetward.verification.duration` | histogram | s | `fleetward.instance.id`, `fleetward.engine.type`, `fleetward.verification.status` |
| `fleetward.alert.evaluation.duration` | histogram | s | `fleetward.outcome` |
| `fleetward.alerts.opened` | counter | `{alert}` | `fleetward.alert.kind`, `fleetward.alert.severity` |
| `fleetward.alerts.resolved` | counter | `{alert}` | `fleetward.alert.kind` |
| `fleetward.notifications` | counter | `{notification}` | `fleetward.notifier.kind`, `fleetward.outcome` |

Eight metrics. `fleetward_alert_evaluation_duration_seconds_count` is the one that answers "did it
run last night", and no separate counter is added for it.

## Reuse, do not rewrite

- **`statusRecorder`** — `internal/controlplane/api/server.go:227` — already captures the status code
  for the access log. The metric reads the same field. Do not add a second wrapper.
- **`http.ServeMux.Handler(r)`** returns the matched pattern. Do not build a route table.
- **`Decision`** — `internal/controlplane/authz/guard.go:70` — already carries `Method`,
  `EffectiveRole` and the rule, on both outcomes. The counter reads it and derives nothing.
- **`EvaluationResult`** — `internal/controlplane/alerts/evaluate.go:96` — already counts `Opened`
  and `Resolved`. The counters add those deltas; they do not re-count rows.
- **`d.dropped`** — `internal/controlplane/alerts/deliver.go:174` — already exists as an
  `atomic.Int64`. The `dropped` counter increments beside it, at the same branch.
- **`outcome.status`** — `internal/controlplane/backup/verify.go:301` — is already the three-way
  verdict, and `fwv1.VerificationStatus.String()` is the label value. Do not invent a second
  spelling.
- **`api.WriteProblem`** — `server.go:261` — is the refusal shape for a rejected scrape.
- **`authz.Guard`'s primitives** — `bestRank`, `roleAtLeast`, `RoleViewer` — are what the scrape
  check is built from. Do not write a second role comparison.
- **`RegisterSessions`** — `internal/controlplane/api/sessions.go:28` — is the mounting idiom.
- **`tools/docsgen`** generates `docs/ops/configuration.md` from the struct comments and the `Load()`
  call. Write the doc comment; never edit the reference by hand.
- **`internal/storage/tsdb`** is the *other* metrics path and this slice does not touch it. Nothing in
  B8 writes a sample through remote-write.

## Traps

Each of these has a specific way of producing something that looks finished and is not.

1. **`http.route` from `r.URL.Path` is unbounded.** The gateway is mounted at `/api/v1/` on a single
   pattern, so a request to `/api/v1/backups/9f1c…/verifications` has a UUID in its path. Labelling
   on the path creates one series per backup, forever — the exact failure decision 3 forbids, one
   line into the slice. Use `mux.Handler(r)`, which returns `/api/v1/` for the whole gateway subtree
   and `GET /healthz` for the rest. Coarse and bounded beats precise and unbounded; the fine-grained
   name is on the span, from `guarded`.

2. **Default histogram boundaries make the backup metric useless.** Decision 7. If the views are
   forgotten, `fleetward_backup_duration_seconds_bucket{le="10"}` and `le="+Inf"` will be the only
   two that ever differ, and nothing about the slice will look broken.

3. **A `nil` instrument panics, and only in production.** If instrument creation is wrapped in an
   `init()` that stores `nil` on error, the panic happens on the first backup — a path no unit test
   runs. Create instruments once, log a creation error, and store a working no-op.

4. **`middleware` wraps the mux from outside, so `r.Pattern` is empty there.** Go's `ServeMux` sets
   `Pattern` on a shallow copy it passes down; the outer request never sees it. This is why trap 1's
   fix is `mux.Handler(r)` and not `r.Pattern`.

5. **The bootstrap credential holds no grants.** `BootstrapAuthenticator.Authenticate`
   (`internal/controlplane/authn/bootstrap.go:64`) returns a principal with `Kind: KindBootstrap` and
   a nil `Grants` slice; `Guard.Check` allows it *by kind*, at `guard.go:119`. A scrape check written
   as `bestRank(p.Grants, tenantWide) >= viewer` therefore refuses the bootstrap credential — which
   is the credential the dev stack's scrape config uses, so the dev stack would silently stop
   collecting and the failure would surface as an empty query in "Done when" item 6. Handle
   `KindSystem` and `KindBootstrap` first, exactly as `Guard.Check` does.

6. **`/metrics` at a 15-second scrape interval writes an info line every 15 seconds.** The access log
   already demotes `/healthz` and `/readyz` to debug at `server.go:204`. Add `/metrics` to that
   condition, or the dev stack's log becomes unreadable and the demo's narration is buried.

7. **A metric recorded on the way out of a run happens after the context is cancelled.** Recording is
   synchronous and context-free in the OTel metric API, so this is safe today — but if a recorder is
   ever given a `ctx` parameter it must be the detached one the recording path already uses
   (`context.WithoutCancel`, as `verify` does at `verify.go:324`), or an outcome recorded during
   shutdown is dropped.

8. **`buf` is not involved and neither is `api/proto/`.** Nothing in this slice touches the contract,
   so `make proto` is not run and the CRLF hazard in
   [ADR-0029](../../adr/0029-the-openapi-document-is-generated-to-match-the-wire.md) does not apply.
   A session that finds itself editing a `.proto` file has left the scope fence.

9. **Adding `client_golang` changes `go.sum` and CI's vulnerability job.** `govulncheck` reports
   stdlib findings on this machine that CI does not have — see `STATUS.md`'s environment notes — so
   do not read those as caused by the new dependency. Compare against a run on `origin/main` before
   blaming the exporter.

10. **`gofmt` and `golangci-lint` report the whole tree as unformatted here.** Run them in a worktree
    created with `git -c core.autocrlf=false worktree add`, and set `core.autocrlf=false` in that
    worktree's own config, or a later checkout inside it reintroduces CRLF and the findings come
    back.

11. **`go run ./tools/demo` does not rebuild the image.** This slice changes `cmd/` and `internal/`,
    so every demo run must pass `-build` or it exercises the previous binary and `/metrics` is absent
    for no visible reason.

## Scope fence

Explicitly **not** in this slice. A session reading the roadmap will want most of these.

- **No database metric collection.** `CollectMetrics` is in the plugin contract and stays uncalled;
  `db.client.*` stays empty. It is deferred deliberately in the roadmap, and conflating it with
  Fleetward's own metrics is the exact mistake
  [ADR-0011](../../adr/0011-opentelemetry-and-semconv.md) opens by naming.
- **No writes through `internal/storage/tsdb`.** The remote-write path stays unused. `/metrics` is a
  pull endpoint and the two do not meet in this slice.
- **No fifth operation.** Not the scheduler tick — readiness already reports a stalled tick loop,
  which is the failure that matters. Not the retention sweep, not a plugin RPC, not the object store,
  not the sandbox provider. Four is the number, and each additional one is a label set and a bucket
  choice to get wrong.
- **No sub-spans inside a backup.** "Was it the dump or the upload" is a real question and it is a
  later slice's. The backup span carries bytes and part count as attributes and stops there.
- **No artifact-size and no queue-depth metric.** Retention already reports bytes, and the
  dispatcher's queue depth wants an observable-gauge callback; both are additive later.
- **No `fleetward.build.info` metric.** `target_info` from the resource attributes already carries
  the version and the service name, for free.
- **No alert rule over Fleetward's own metrics.** `custom_promql` is refused at creation because
  nothing collects, and this slice does not change that. Self-alerting on a metric Fleetward itself
  emits has a bootstrapping problem worth its own thinking.
- **No Grafana dashboard, no recording rules, no alerting-rules file.** `docs/ops/observability.md`
  documents the metrics; a dashboard is a separate artifact with a separate maintenance story.
- **No log export through OTel.** `log/slog` to stdout stays what
  [ADR-0014](../../adr/0014-slog-structured-logging.md) says it is. Traces and metrics only.
- **No `/metrics` on a separate admin listener.** One listener, one authorization rule. A second bind
  address is a deployment change and belongs with B9's packaging if anybody wants it.
- **No web UI for any of it.** There is no alerts screen either; the endpoint is the surface.
- **No demo act.** The demo tells the product's story to a DBA, and `/metrics` is an operator's
  concern rather than a beat in that story. The compose smoke test is where the endpoint is asserted
  on a real stack.

## Done when

Concrete commands and their expected output. `make` is absent on this machine; the direct
equivalents are given.

1. **The endpoint refuses correctly, and the tests are about the refusal rather than the format.**
   ```
   go test ./internal/controlplane/api/... -run Metrics -v
   ```
   shows a 401 with no credential, a 403 for a caller holding `viewer` on one instance, a 200 for a
   tenant-wide `viewer`, and a 200 for the bootstrap credential — trap 5.

2. **Instrumentation is a no-op with no provider installed.**
   ```
   go test ./internal/telemetry/... -v
   ```
   passes with `FLEETWARD_TELEMETRY_ENABLED` unset, including the test that calls every recorder with
   no meter provider and asserts no panic.

3. **The histogram boundaries are the ones chosen, not the defaults.**
   ```
   go test ./internal/telemetry/... -run Boundaries -v
   ```
   asserts that `fleetward.backup.duration` has a bucket above 1800 seconds — trap 2, caught by a
   test rather than by an operator six months from now.

4. **No label carries something unbounded.**
   ```
   go test ./internal/telemetry/... -run Cardinality -v
   ```
   enumerates the declared label keys and fails on any name in decision 3's forbidden list. The list
   lives in the test as data, so adding a metric with `backup_id` on it fails to merge.

5. **The whole tree is green.**
   ```
   golangci-lint run
   go run ./tools/docscheck
   go run ./tools/docsgen && git diff --exit-code docs/
   go test ./...
   cd web && npm run lint && npm test
   ```
   `docscheck` reports no unused allowance, which means every entry this brief added to
   `docs/.docscheck-allow` has been removed as its file appeared. `docsgen` leaves no diff, which
   means `docs/ops/configuration.md` already documents the three new settings.

6. **VictoriaMetrics holds Fleetward's own metrics, scraped with a credential.**
   ```
   docker compose up -d --build
   curl -s '127.0.0.1:8428/api/v1/query?query=up{job="fleetward"}'
   curl -s '127.0.0.1:8428/api/v1/query?query=fleetward_alert_evaluation_duration_seconds_count'
   ```
   The first returns `1`. The second returns a value that increases between two calls thirty seconds
   apart — which is this slice's whole point stated as a query: *did the evaluation pass run*.
   `.env` needs `FLEETWARD_POSTGRES_PORT=55432`; a local PostgreSQL holds 5432.

7. **A refusal is visible as a metric.**
   ```
   curl -s -H 'Authorization: Bearer fwt_devbootstrap_0000000000000000' 127.0.0.1:8080/metrics \
     | grep fleetward_authz_decisions_total
   ```
   after a `viewer` token has been refused a backup, shows a series with
   `fleetward_authz_outcome="refused"` and the RPC method — which is CLAUDE.md §7.5's 403, now
   countable.

8. **Nothing changed when telemetry is off.**
   ```
   go run ./tools/demo -build
   go test -tags=e2e ./test/e2e/...
   ```
   pass unchanged, with `FLEETWARD_TELEMETRY_ENABLED` at its default of false.

9. **The close-out is done.** `docs/dev/STATUS.md` rewritten with **B9** next, and the "Fleetward
   cannot be observed" entry replaced by what B8 knowingly leaves — no database metrics, four
   operations and not five, `/metrics` on the main listener. Journal entry at
   `docs/dev/journal/B8-self-observability.md` with the real numbers. `README.md` no longer says
   Fleetward emits no metrics about itself, and `docs/architecture.md` no longer says alerting does
   not exist. Two ADRs in `docs/adr/`, linked from CLAUDE.md §2.
