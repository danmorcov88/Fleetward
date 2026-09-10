# ADR-0041: What a Fleetward metric is allowed to carry

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

[ADR-0011](0011-opentelemetry-and-semconv.md) settled the SDK and settled naming for one of the two
telemetry concerns it was careful to separate: metrics Fleetward *collects about monitored
databases* follow OTel database semantic conventions, `db.client.*`. It said nothing about the other
concern — Fleetward's own observability — beyond "the OpenTelemetry Go SDK", because at the time
there were no call sites to have an opinion about. For eight slices there still were none.

B8 adds them, and three questions have to be answered before the first one is written, because each
is expensive to change afterwards. A metric name that reaches a dashboard is a name somebody
depends on. A label that reaches a time-series store creates series that persist for the retention
period whether or not anybody wanted them. And a label that carries a credential cannot be recalled
from a collector that has already shipped it elsewhere.

The third is not hypothetical here. `runRequest` — the struct that describes one backup in flight —
holds an `*inventory.Connection`, whose fourth field is `Credentials`. A recorder taking that struct
"for context" is one `%v` away from putting a production database password into a label. The audit
package hit the same shape in B6 and solved it by construction: its package comment records that
there is no function taking a request message, because `CreateInstanceRequest` carries a password
and `audit_log` cannot be edited or deleted. A metric is worse than an audit row in one specific
way — it is re-emitted on every scrape, forever, to wherever the collector sends it.

## Decision

### Three namespaces, with a boundary that needs no judgement

- **Where OTel semantic conventions already name the thing, use the semconv name unchanged.**
  Fleetward's HTTP surface emits `http.server.request.duration` with `http.request.method`,
  `http.route` and `http.response.status_code`.
- **Where they do not, because the thing is a Fleetward concept, the name lives under
  `fleetward.`.** A backup, a verification, an alert evaluation pass, an authorization decision.
- **`db.client.*` is the estate's, not Fleetward's.** It never appears on `/metrics`. Those metrics
  reach the store through `internal/storage/tsdb`'s remote-write path when something eventually
  collects them, which is deferred deliberately.

### A label is bounded by the estate; a span attribute is not

> **A metric label's value set must be bounded by the size of the estate. A value set that grows
> with time is forbidden.**

Permitted, with what bounds each: `instance.id` (the estate — about fifty), `engine.type` (eight),
`backup.method` (a handful per engine), `rpc.method` (`authz.Policies`, which the coverage test
keeps closed), `http.route` (the registered mux patterns), `alert.kind` (a `CHECK` constraint of
seven), `alert.severity` (three), `notifier.kind` (two), and every outcome word.

Forbidden: `backup_id`, `verification_id`, `job_id`, `alert_id`, `schedule_id`, `fingerprint`,
`object_key`, `request_id`, and any error message, summary or URL.

**A span attribute may carry every one of those, and several do.** A span *is* one event, so
`backup.id` on a backup span is the point rather than a cost. The distinction is the whole reason
both exist.

### Nothing that records a metric takes a struct

There is no function in `internal/telemetry` that takes a request message, a `runRequest`, an
`inventory.Connection`, or an `error`. Every recorder takes named scalars, and an error becomes an
outcome word at the call site. The credential rule applies identically to span attributes, which
leave the process the same way.

### It is enforced by tests that read what is emitted

`internal/telemetry/metrics_test.go` drives every recorder through a real SDK pipeline and reads the
attribute keys back off the exported data, rather than checking a list of constants somebody would
have to remember to update. A list would not notice
`attribute.String("fleetward.backup.id", …)` being added to a recorder, which is precisely the
mistake the rule is about. A second test asserts the namespace rule the same way, and a set of
compile-time function-type assertions makes a recorder that grew a struct parameter fail to build.

## Consequences

- An operator's existing `http.server.request.duration` dashboards work against Fleetward with no
  translation, and everything Fleetward-specific is one prefix away from being found.
- `engine.type` is a label *value* and never a branch. Nothing decides what to record by reading it,
  which is CLAUDE.md §4.1 applied to telemetry: an engine's name as data is fine, an engine's name
  as a condition is not.
- The bill is paid at every call site: an outcome has to be named rather than derived from an error,
  and a per-event identifier has to go on the span rather than the metric. That is the point.
- One reconciliation is deferred and recorded here so a third spelling is never invented.
  `internal/storage/tsdb` names its remote-write labels `fw_tenant_id`, `fw_instance_id`,
  `fw_engine_type`, while these render as `fleetward_instance_id`. Nothing has ever written a sample
  through the `fw_*` path, so there is no data to migrate; when database metric collection lands,
  that path adopts this spelling.
- Histogram bucket boundaries are part of what a metric carries and are declared beside it, as
  instrument advisories rather than SDK views. OTel's defaults stop at ten seconds, which would put
  every backup and every verification in the `+Inf` bucket — an instrument that looks like it works
  and answers nothing. A test asserts the boundaries reach past thirty minutes and a second asserts
  the SDK honours them.

## Alternatives considered

- **A single `fleetward.` namespace for everything, including HTTP.** Consistent, and it throws away
  every dashboard, alerting rule and exporter convention that already understands the semconv name.
  Consistency inside one repository is worth less than compatibility with the tooling an operator
  already runs.
- **`fw.` as the attribute prefix, matching `internal/storage/tsdb`.** It would unify the two paths
  today at the cost of a prefix nobody can read. Since no data exists on the `fw_*` path, the
  reconciliation is free in the other direction and is recorded above.
- **"Keep cardinality low" as guidance rather than a rule with a test.** That is what every project
  has when it discovers a metrics bill. The failure mode is one label added in a hurry, and the only
  thing that catches it is a check that runs on the pull request.
- **A convenience recorder taking the request, with a redaction list.** A denylist of field names is
  a promise that has to be maintained against a growing contract; "the function does not exist" is a
  property of the code. This is the audit package's reasoning, unchanged.
