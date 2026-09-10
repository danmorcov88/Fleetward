# Observing Fleetward

How to monitor the thing that monitors your backups: what it exposes, who may read it, what each
series means, and — the part worth reading before you rely on it — what it will not tell you.

Every setting named here is documented in [`configuration.md`](configuration.md). The decisions
behind the design are [ADR-0011](../adr/0011-opentelemetry-and-semconv.md),
[ADR-0041](../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md) and
[ADR-0042](../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md).

---

## The one thing to read before you rely on this

**Fleetward's metrics tell you whether Fleetward is working. They are not how you find out whether
your backups are good.**

That answer is the estate view, `fleetward-cli backup adherence`, and the alerts B7 delivers. It has
to be, because it is computed from rows rather than from counters: a backup that failed at 02:00 is
a row that says so at 09:00, whereas a counter that stopped increasing looks identical to an estate
where nothing needed doing.

What these metrics are for is the question the alerts cannot answer about themselves:

> **Is the control plane awake, and did it look?**

`fleetward_alert_evaluation_duration_seconds_count` is the series that answers it. An evaluation
pass writes no job row — deliberately, and for the reasons in
[ADR-0038](../adr/0038-alert-evaluation-is-a-pass-over-the-estate.md) — so before B8 the only record
that a pass had run was a log line. If that counter stops advancing, nothing is being detected and
no alert will fire, and the estate will look exactly as healthy as it did the moment evaluation
stopped.

**Alert on that counter first.** Everything else on this page is secondary.

---

## Turning it on

It is already on. `GET /metrics` serves the Prometheus exposition format by default, and requires a
credential.

| Setting | Default | What it does |
|---|---|---|
| `FLEETWARD_TELEMETRY_PROMETHEUS_ENABLED` | `true` | serves `GET /metrics` |
| `FLEETWARD_TELEMETRY_PROMETHEUS_AUTH` | `true` | requires a tenant-wide `viewer` to scrape |
| `FLEETWARD_TELEMETRY_ENABLED` | `false` | exports spans and metrics to an OTLP collector |

The first two are the pull path and the third is the push path, and they are independent. `/metrics`
needs nothing else to be running, which is why it is the default; OTLP export names a collector that
has to exist, which is why it is not.

Disabling both is permitted and warned about on every start, because an installation nobody can
observe should not be a quiet fact.

---

## Who may scrape it

**A credential granting tenant-wide `viewer`.**

This is not an extra rule invented for the endpoint. Scope comes from the request, and a request
that names no scope is a question about the whole tenant
([ADR-0035](../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md)) — a scrape names no
scope, and the response carries a series per instance. Somebody granted three servers gets 403, the
same as they would for `fleetward-cli backup list` with no `--instance`.

```bash
# A credential for your monitoring system, and nothing else.
fleetward token create --email prometheus@example.com --role viewer > /etc/fleetward/scrape-token
```

Then, in Prometheus or VictoriaMetrics:

```yaml
scrape_configs:
  - job_name: fleetward
    metrics_path: /metrics
    static_configs:
      - targets: ["fleetward.internal:8080"]
    bearer_token_file: /etc/prometheus/fleetward-scrape-token
```

The development stack does exactly this, with the bootstrap credential —
[`deploy/dev/victoriametrics/scrape.yml`](../../deploy/dev/victoriametrics/scrape.yml). It presents
a token rather than turning the requirement off, deliberately: enforcement that nothing exercises is
enforcement nobody notices is broken.

### Serving it openly

`FLEETWARD_TELEMETRY_PROMETHEUS_AUTH=false` serves `/metrics` to anyone who can reach the port. That
is a supported choice for a control plane listening only on a monitoring network, it warns on every
start, and it is **not** refused in production the way disabled authentication is — disclosing the
shape of an estate and granting control of one are different sizes of mistake
([ADR-0042](../adr/0042-scraping-metrics-is-a-question-about-the-whole-estate.md)).

What you are disclosing, if you do: how many database servers you run, which engines, which of them
are failing verification, and how long their backups take. No credential, no hostname, no database
name and no backup contents — a metric label may not carry any of those, by construction.

---

## What is exposed

Eight metrics of Fleetward's own, plus the Go runtime and process collectors.

Names follow OpenTelemetry semantic conventions where a convention exists, and live under
`fleetward.` where none does; the Prometheus exporter renders the dots as underscores and appends
`_total` to counters and `_seconds` to durations
([ADR-0041](../adr/0041-what-a-fleetward-metric-is-allowed-to-carry.md)).

### Is it awake, and did it look

| Series | What it tells you |
|---|---|
| `fleetward_alert_evaluation_duration_seconds_count` | how many evaluation passes have run. **The one to alert on.** |
| `fleetward_alert_evaluation_duration_seconds_bucket` | how long a pass takes. Rising past a few seconds on a small estate means a slow query. |
| `fleetward_alert_evaluation_duration_seconds_count{fleetward_outcome="failed"}` | passes that started and did not finish. Non-zero means conditions are going undetected. |

```promql
# Nothing has evaluated in ten minutes. The default interval is thirty seconds.
increase(fleetward_alert_evaluation_duration_seconds_count[10m]) == 0
```

### Is anybody being told

| Series | What it tells you |
|---|---|
| `fleetward_alerts_opened_total{fleetward_alert_kind, fleetward_alert_severity}` | alerts created, and therefore notifications sent |
| `fleetward_alerts_resolved_total{fleetward_alert_kind}` | alerts whose condition disappeared |
| `fleetward_notifications_total{fleetward_notifier_kind, fleetward_outcome}` | delivery attempts: `delivered`, `failed`, or `dropped` |

`dropped` is the one worth an alert of its own. Delivery is at-most-once and a full queue discards
rather than blocking ([ADR-0039](../adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)),
and until B8 the only trace of that was a log line. The alert row survives; the notification does
not.

### Are backups and verifications healthy

| Series | What it tells you |
|---|---|
| `fleetward_backup_duration_seconds{fleetward_instance_id, fleetward_engine_type, fleetward_backup_method, fleetward_outcome}` | how long a backup took, and whether it succeeded |
| `fleetward_verification_duration_seconds{fleetward_instance_id, fleetward_engine_type, fleetward_verification_status}` | how long a verification took, and which of the three verdicts it reached |

`fleetward_verification_status` carries `VERIFICATION_STATUS_VERIFIED`, `…_FAILED` and
`…_INCONCLUSIVE` separately, and they are not interchangeable
([ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md)). `FAILED` means an
artifact was restored and the data did not match. `INCONCLUSIVE` means we could not tell — an image
that would not pull, a sandbox that never started.

```promql
# The gap alerting deliberately leaves open: verification has not been happening all week, and
# nothing paged anybody, because an inconclusive verdict is not an alert about the artifact.
increase(fleetward_verification_duration_seconds_count{fleetward_verification_status=~".*INCONCLUSIVE"}[1h]) > 0
```

That query is worth setting up. `docs/ops/alerting.md` says in its own words that a sandbox provider
broken all week means verification is not happening and nothing pages anybody; this is how you close
that gap without widening the alert predicate, which is the fix
[ADR-0040](../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md) refuses.

### Who is being refused

| Series | What it tells you |
|---|---|
| `fleetward_authz_decisions_total{fleetward_rpc_method, fleetward_authz_outcome, fleetward_authz_effective_role}` | every authorization decision, allowed or refused |
| `http_server_request_duration_seconds{http_request_method, http_route, http_response_status_code}` | request rate, latency and status, by route |

```promql
# Somebody is repeatedly reaching for something they may not have.
sum by (fleetward_rpc_method, fleetward_authz_effective_role) (
  rate(fleetward_authz_decisions_total{fleetward_authz_outcome="refused"}[5m])
)
```

`http_route` is the mux **pattern**, not the path — the whole REST surface is `/api/v1/`, because a
path carrying a backup's identifier would create one time series per backup, forever. The RPC's own
name is on `fleetward_authz_decisions_total`, and on the request's span.

### The process itself

`go_goroutines`, `go_memstats_*`, `process_cpu_seconds_total`, `process_open_fds` and the rest, from
the standard Go and process collectors. `target_info` carries the version and the service name.

---

## Traces

`FLEETWARD_TELEMETRY_ENABLED=true` exports spans to an OTLP collector at
`FLEETWARD_TELEMETRY_OTLP_ENDPOINT`. Four operations carry one:

| Span | Covers |
|---|---|
| `fleetward.http.request` | one served request, with the authorization decision on it |
| `fleetward.backup.run` | one backup, manual or scheduled, from the first plugin call to the recorded outcome |
| `fleetward.verification.run` | one verification, including provisioning and tearing down the sandbox |
| `fleetward.alert.evaluation` | one pass over the estate |

A span may carry what a metric label may not — `fleetward.backup.id`, `fleetward.job.id`,
`fleetward.verification.id` — because a span is one event rather than a series. What neither may
carry is a credential, a connection string, or an artifact's contents.

`FLEETWARD_TELEMETRY_SAMPLE_RATIO` is `1.0` by default. Four operations on an estate of fifty is not
a volume that needs sampling; lower it if you are shipping to something that charges by the span.

---

## What this does not tell you

Stated plainly, because a monitoring page that lists only what it has is the kind of document this
project is trying not to write.

- **Nothing about your databases' performance.** `db.client.*` — connections, query latency, replica
  lag on the servers Fleetward watches — is in the plugin contract as `CollectMetrics` and nothing
  calls it. That is deferred deliberately: performance monitoring was never the pain this product
  exists to solve, and you almost certainly already have a tool for it.
- **Nothing about the scheduler's tick.** A tick loop that has stopped is reported by `/readyz`,
  which degrades with the reason, and that is the signal to alert on. There is no `ticks_total`.
- **Nothing about the retention sweep.** `fleetward-cli backup retention`, the audit log under
  `actor = system:retention`, and the log line are the account. A sweep writes no job row for the
  same reason an evaluation pass does not.
- **Nothing about the notification queue's depth.** You can see what was dropped, not how close it
  came.
- **No breakdown inside a backup.** "Was it the dump or the upload" is a fair question and the
  backup span does not answer it yet; it carries total duration and artifact size.
- **No shipped dashboard and no shipped alerting rules.** The PromQL on this page is what there is.
- **Logs are not exported through OpenTelemetry.** `log/slog` writes JSON to stdout
  ([ADR-0014](../adr/0014-slog-structured-logging.md)); collect it the way you collect any
  container's.
- **A metric is per control plane.** Two replicas produce two sets of series, distinguished by
  whatever your scrape configuration labels them with. That is correct — "did *a* pass run" and "did
  *this* process run one" are different questions — and it means estate-wide queries need a `sum`.
