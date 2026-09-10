# B8 — Self-observability, or the slice that made the wiring run

**Delivered:** 2026-09-10 · [brief](../slices/B8-self-observability.md) ·
[ADR-0041](../../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md) ·
[ADR-0042](../../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md)

---

## What shipped

`GET /metrics` serves Fleetward's own health in the Prometheus exposition format, and four
operations carry a span.

`internal/telemetry` grew two files. `metrics.go` holds every instrument, every label key, every
outcome word and every recorder — one file, so that a reviewer asking "what can a Fleetward metric
carry" reads one place. `tracing.go` holds the span names, the per-event attribute keys a metric may
*not* carry, and the only call to `otel.Tracer` in the tree. `otel.go` was restructured: the meter
provider now takes one reader per enabled export path, so the pull endpoint and the OTLP push are
independent switches, and `Setup` returns a struct carrying the `http.Handler` rather than a bare
shutdown function.

The four instrumented funnels were each the only one of their kind, which is why this slice touched
five call sites and not fifteen: `api.(*Server).middleware`, `backup.(*Service).execute` (both the
API's asynchronous path and the scheduler's synchronous one reach it), `backup.(*Service).verify`
(likewise), and `alerts.(*Service).Evaluate`. The authorization decision is not a fifth operation —
`authz.guarded` emits a counter and puts the RPC method and the outcome onto the span the middleware
already opened, because the gateway runs in-process and the request context reaches the decorator
unbroken.

`internal/controlplane/authz/scrape.go` decides who may scrape, and
`internal/controlplane/api/metrics.go` mounts the route. The development stack's VictoriaMetrics —
in the compose file since the foundation slice, health-checked on every start, holding nothing for
eight slices — now scrapes that endpoint with a credential, and holds Fleetward's own metrics.

Eight metrics of Fleetward's own, plus the Go runtime and process collectors:

| | |
|---|---|
| `fleetward_alert_evaluation_duration_seconds` | the `_count` is "did the pass run last night" |
| `fleetward_alerts_opened_total`, `fleetward_alerts_resolved_total` | by rule kind and severity |
| `fleetward_notifications_total` | `delivered`, `failed`, `dropped` |
| `fleetward_backup_duration_seconds` | by instance, engine, method, outcome |
| `fleetward_verification_duration_seconds` | by instance, engine, and all three verdicts |
| `fleetward_authz_decisions_total` | by RPC method, outcome, and the role actually held |
| `http_server_request_duration_seconds` | the semantic-convention name, by route |

## How it was verified

Windows (amd64), 2026-09-10, Go 1.25.6, Docker Desktop. `make` is not installed on this machine, so
every target below was run directly and is reported as run rather than as `make lint test`.

**The unit suite is green**, `go test -short ./...` — without `-race`, which needs a C toolchain this
machine does not have. `golangci-lint run` reports **0 issues**. `gofmt -l .` is empty, in a worktree
created with `git -c core.autocrlf=false worktree add`, which is the only way that is true here.
`go run ./tools/docscheck` reports 95 markdown files and no problems; `go run ./tools/docsgen`
leaves no diff, so `docs/ops/configuration.md` documents the three telemetry settings from the struct
comments rather than by hand. `npm run lint` is clean and `npm test` passes 22 tests in 3 files.

**Integration tests pass on all three packages the instrumentation touched**, with real containers:
`alerts` 67.9s, `authz` 47.1s, `backup` 175.0s.

**The endpoint refuses correctly.** Against the running development stack: no credential → `401`;
the bootstrap credential → `200`; a tenant-wide `viewer` token → `200`. `internal/controlplane/authz`
covers the rest of the table against real principals, including the two kinds that hold no grants at
all.

**A 403 is now a series.** A `viewer` token asking for a backup:

```
fleetward_authz_decisions_total{fleetward_authz_effective_role="viewer",
  fleetward_authz_outcome="refused",
  fleetward_rpc_method="/fleetward.v1.BackupService/RunBackup"} 1
```

That is CLAUDE.md §7.5, which has been provable since B6 and countable since this morning.

**`go run ./tools/demo -build` passes unchanged**, through all eight acts, with telemetry at its
default of push-disabled. Run again with `-keep`, the endpoint it left behind carried 456 exposed
series, of which the two that matter are:

```
fleetward_verification_duration_seconds_count{...,fleetward_verification_status="VERIFICATION_STATUS_VERIFIED"} 1
fleetward_verification_duration_seconds_count{...,fleetward_verification_status="VERIFICATION_STATUS_FAILED"}   1
```

The demo's good backup and the artifact it deliberately corrupts, as two distinct series, with
`_sum` at 16.44s and 15.23s. Beside them, `fleetward_backup_duration_seconds_sum` of 1.06s over two
SQL Server backups, `fleetward_alerts_opened_total{fleetward_alert_kind="verification_failed",
fleetward_alert_severity="critical"} 14`, and
`fleetward_notifications_total{fleetward_notifier_kind="webhook",fleetward_outcome="delivered"} 1`.

**VictoriaMetrics holds all of it, scraped with a bearer token.** `up{job="fleetward"}` is `1` and
`fleetward_alert_evaluation_duration_seconds_count` advances between queries thirty seconds apart.
That is the first sample that store has ever held.

## Decisions worth carrying forward

**Three namespaces, and the boundary is not a judgement call.** Where OTel semantic conventions
already name the thing, the semconv name is used unchanged — an operator's existing
`http.server.request.duration` dashboards work against Fleetward with no translation. Where they do
not, the name is under `fleetward.`. And `db.client.*` belongs to the *monitored estate*, never
appears on this endpoint, and remains uncollected. Keeping the third apart is what lets `/metrics`
be a different thing from the remote-write path
([ADR-0041](../../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md)).

**A label is bounded by the estate; a span attribute is not.** "Keep cardinality low" is advice, and
what this slice wrote down instead is a rule with a test: a label's value set must be bounded by the
size of the estate, and a set that grows with time is forbidden. `instance_id` is fine on fifty
servers; `backup_id` is one time series per backup forever. A span *is* one event, so
`fleetward.backup.id` belongs there and does belong there — which is why `tracing.go` declares
several keys `metrics.go` refuses.

**The cardinality test reads what is emitted, not a list.** It drives every recorder through a real
SDK pipeline and pulls the attribute keys off the exported data. A list of label constants would not
notice `attribute.String("fleetward.backup.id", …)` being added to a recorder, which is exactly the
mistake the rule exists for. It was checked by making that mistake on purpose; the test failed,
naming the metric and the label.

**Nothing that records a metric takes a struct.** The audit package's rule, transplanted, and for a
sharper reason. `runRequest.connection` is an `*inventory.Connection` whose fourth field is
`Credentials`, so a convenient `RecordBackup(ctx, req, err)` is one `%v` from a production database
password in a label — and a metric, unlike an audit row, is re-emitted on every scrape forever. Every
recorder takes named scalars, an error becomes an outcome word at the call site, and a set of
compile-time function-type assertions makes a recorder that grew a struct parameter fail to build.

**A scrape is a question about the whole estate, so it needs a tenant-wide `viewer`.** Not a new
rule: ADR-0035 already says scope comes from the request and a request naming no scope asks about
the whole tenant. A scrape names no scope, and the response carries a series per instance. Serving it
openly stays available and warns on every start, and is deliberately *not* refused in production the
way disabled authentication is — disclosing the shape of an estate and granting control of one are
different sizes of mistake ([ADR-0042](../../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md)).

**The development stack scrapes with a credential rather than turning the requirement off.** The
tempting shortcut was `FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=false` in `docker-compose.yml`. That is
the same shortcut authorization itself used to take, and the comment above `FLEETWARD_AUTH_ENABLED`
records what it cost: enforcement nothing exercises is enforcement nobody notices is broken.

**The route label is the mux pattern, never the path.** `http.ServeMux.Handler(r)` resolves a
request to its registered pattern without serving it. `r.Pattern` is not an option — `ServeMux` sets
it on a shallow copy it passes downward, and the middleware wraps the mux from outside. Without
this, `/api/v1/backups/<uuid>/verifications` would have created one series per backup on the first
day. Coarse and bounded beats precise and unbounded; the RPC's own name is on the span, from the
decorator that knows it.

## Two things the slice found that were not in the brief

**`telemetry.Setup`'s resource merge had never run, and it was broken.** The first
`docker compose up` after turning the endpoint on by default failed to start:
`conflicting Schema URL: .../1.43.0 and .../1.26.0`. `resource.Merge` refuses two resources
declaring different schema URLs, the SDK's own `Default()` declares whichever semconv version its
release was built against, and this file's import was pinned at 1.26.0. The code was written in the
foundation slice and had never executed, because `Setup` returned before reaching it whenever
telemetry was disabled — which was the default, and therefore always. The fix is
`resource.NewSchemaless`, which merges with any of them, so a future SDK upgrade cannot turn the
control plane into one that refuses to start. This is the clearest possible argument for the slice:
wiring that nothing runs is wiring that does not work.

**Two things arrived for free and are kept.** The Go runtime and process collectors are three lines
on a registry we already own, and no substitute exists the way `target_info` substitutes for a
build-info metric — `go_goroutines` and `process_open_fds` are half of what anybody means by "how do
I monitor it". And `http_client_request_duration_seconds` appeared unbidden: `otelhttp` is already an
indirect dependency and the sandbox provider's Docker client uses it, so calls to the Docker daemon
are instrumented with bounded labels and no credential. Both are named in
[`../../ops/observability.md`](../../ops/observability.md) rather than left to be discovered.

## Deviations from the brief

**Bucket boundaries are instrument advisories, not SDK views.** The brief specified
`sdkmetric.NewView`. The advisory achieves the same result — the SDK honours it when no view
overrides — and keeps every fact about a metric, including its resolution, in the one file that is
supposed to hold them. A test asserts the SDK actually honours it, because "the same result" is a
claim rather than an observation.

**`otelprom.WithoutScopeInfo()`.** The first real scrape carried `otel_scope_name`,
`otel_scope_version` and `otel_scope_schema_url` on every series, two of them empty. There is one
instrumentation scope in this binary, so all three are constant; dropping them also keeps a future
scope version from starting a fresh set of series on every release.

## Not built, deliberately

- **No database metric collection.** `CollectMetrics` is in the contract and nothing calls it;
  `db.client.*` stays empty. Deferred in the roadmap, and conflating it with Fleetward's own metrics
  is the mistake ADR-0011 opens by naming.
- **No writes through `internal/storage/tsdb`.** The remote-write path is still unused. `/metrics`
  is a pull endpoint and the two do not meet.
- **No fifth operation.** Not the scheduler tick — `/readyz` already degrades with a reason when the
  loop stalls, which is the signal to alert on. Not the retention sweep, not a plugin RPC, not the
  object store, not the sandbox provider.
- **No sub-spans inside a backup.** "Was it the dump or the upload" is a fair question; the backup
  span carries total duration and artifact size and stops there.
- **No artifact-size or queue-depth metric, and no `build_info`.** `target_info` covers the last one
  for free.
- **No Grafana dashboard, no recording rules, no alerting-rules file.** The PromQL in
  `docs/ops/observability.md` is what there is.
- **No log export through OpenTelemetry.** `slog` to stdout stays what ADR-0014 says it is.
- **No `/metrics` on a separate admin listener.** One listener, one authorization rule.
- **No demo act.** The demo tells the product's story to a DBA; `/metrics` is an operator's concern.
  The compose stack is where the endpoint was asserted end to end.

## Still open

- **A metric is per control plane.** Two replicas produce two sets of series. That is correct — "did
  *a* pass run" and "did *this* process run one" are different questions — and it means estate-wide
  queries need a `sum`.
- **The notification queue's depth is not observable.** You can see what was dropped, not how close
  it came to dropping.
- **The `fw_*` label prefix in `internal/storage/tsdb` and the `fleetward_*` prefix here disagree.**
  Nothing has ever written a sample through the `fw_*` path, so there is no data to migrate;
  ADR-0041 records that it adopts this spelling when database metric collection lands, so that a
  third spelling is never invented.
- **`FLEETWARD_TELEMETRY_ENABLED` still means OTLP export only**, which the name does not say. It
  was not renamed because nothing is released yet and a rename would be free later too; the doc
  comment says so, and `docs/ops/configuration.md` is generated from it.
