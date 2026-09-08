# D1 — the demo, which is also the end-to-end test

**Delivered 2026-09-08.** Brief: [`../slices/D1-the-demo.md`](../slices/D1-the-demo.md).

Six slices had shipped and none of them had ever been shown to anybody. Each ended with a walk on a
real stack that only the person running it saw, and the evidence that any of it works lived in test
output and in journal entries like this one.

This slice turns that walk into `make demo` — one command, on a real stack, ending with a backup
that fails verification on purpose — and runs the same program in CI so it cannot quietly stop
working.

---

## What shipped

`tools/demo/acts` holds nine acts as ordinary Go functions taking a client and a narrator, plus the
seeder, the object-store corruption, and the pre-flight. `tools/demo` is a thin `main` over them.
`test/e2e/demo_test.go` calls the same `acts.Run` under the `e2e` build tag with the presentation
off. `test/e2e/doc.go`, which had described this slice in the roadmap vocabulary of six slices ago
and held no test, now describes what is in the package.

Three Makefile targets — `demo`, `demo-keep`, `demo-check` — and one CI job, `End-to-end demo`,
sequenced after the dev-stack smoke test because both need the runner's single Docker daemon.
`docs/demo.md` says what the demo shows and, first, what is seeded.

Nothing in `tools/demo` imports `internal/`. The demo drives the REST API, and the metadata database
for exactly one thing.

### The acts

| | |
|---|---|
| 0 | the stack: pre-flight, `docker compose up -d`, `/readyz` green with every component named |
| 1 | an estate you can believe: three environments, twelve instances, six weeks of history |
| 2 | declare, detect, gap: `backup adherence` over the seeded estate |
| 3 | the loop, live: a real backup, a real sandbox, a real restore, `VERIFIED` |
| 4 | break it on purpose: one byte overwritten in MinIO, and `FAILED` |
| 5 | who may do this: a 403, an allowed `dba`, and an audit log the database refuses to edit |
| 6 | what it refuses to delete: the retention floor, and why it is not "keep the newest" |
| 7 | the alert — a stub, because B7 has not happened |
| 8 | what was seeded and what was live, said again at the end |

---

## How it was verified

Windows (amd64), Docker Desktop 27.3.1, 2026-09-08. The stack is the one in `docker-compose.yml`,
with `FLEETWARD_POSTGRES_PORT=55432` in `.env` because this machine already runs a PostgreSQL.

**`go run ./tools/demo -keep`, twice in a row, both green.** On a warm stack, acts 0 to 5 completed
at **0:53** and the whole run at about **1:20**, the difference being the wait for a retention sweep.
From a cold `docker compose up --build`, act 0 alone took **3:27**.

The numbers the run reported, which are the fixture:

```
[seeded]  389 backups, 292 of them verified, 84 observed rather than taken by Fleetward
[seeded]  5 of those expiries stamped into the past on mssql-archive-prod
```

The live half, from the same run:

```
0:20  backup succeeded
0:20  artifact tenants/…/backups/20b5b03b-…/artifact (644.0 KiB), sha256 recorded with it
0:36  verification verified
      connectivity      — fleetward_demo is online on SQL Server 16.0.4265.3
      record_counts     — all 2 objects match the manifest exactly
      schema_presence   — all 2 objects are present
      integrity         — DBCC CHECKDB found no physical inconsistencies
      queryability      — 2 objects were read back successfully
0:36  overwriting one byte in the middle of fleetward-backups/tenants/…/artifact
0:36  644.0 KiB written back, same length, checksum on the row untouched
0:51  verification failed
```

and act 5, which is [ADR-0035](../../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md) and
§7.5 of `CLAUDE.md` demonstrated rather than asserted:

```
viewer  → POST /api/v1/instances/{id}/backups  → 403
          permission denied: this action requires the dba role within its scope
viewer  → GET  /api/v1/audit                   → 403
dba     → POST /api/v1/instances/{id}/backups  → accepted, backup 4aa6903c-…

WHEN       ACTOR                      ACTION                    OUTCOME
13:51:50Z  demo-dba@fleetward.dev     backup.run                allowed
13:51:50Z  demo-viewer@fleetward.dev  backup.run                REFUSED
13:51:18Z  bootstrap                  backup.run                allowed

DELETE FROM audit_log — refused by the database itself
ERROR: audit_log is append-only: DELETE is not permitted (SQLSTATE P0001)

WHEN       ACTOR             ACTION         BACKUP
13:52:19Z  system:retention  backup.expire  650f2341-…
13:52:19Z  system:retention  backup.expire  dc3f7272-…
13:52:19Z  system:retention  backup.expire  0bc758a9-…
```

and act 6, which is [ADR-0032](../../adr/0032-retention-never-deletes-the-last-good-backup.md) in
three lines:

```
NEXT SWEEP  BACKUP    EXPIRY             WHY
KEPT        3187ec5c  passed 2 days ago  kept: it is this instance's most recent successful backup…
KEPT        b61d666f  passed 2 days ago  kept: it is the most recent backup of this instance proven restorable…
DELETES     9bef013c  passed 1 day ago   nothing protects it
```

**The second consecutive run produced the same shape with different identifiers**, which is the
idempotency criterion: the seeder clears the previous estate first, and the demo is not confused by
its own leftovers.

**`make demo-check` — the CI path — passed in 218.7 s**, from `docker compose up -d --build` through
every act to `docker compose down --volumes`, with the presentation off and the narration in the
test log. That is one `go test -tags=e2e`, and it is the same `acts.Run` the paragraphs above
describe.

Also green: `go build ./...`, `go vet` under the default, `integration`, `conformance` and `e2e`
tags, `go test ./...`, `go run ./tools/docscheck`, `golangci-lint run` and `gofmt -l` in an LF
worktree.

---

## Decisions worth carrying forward

### One program, two entry points — [ADR-0037](../../adr/0037-the-demo-and-the-end-to-end-test-are-one-program.md)

The obvious design is a shell script for the demo and a lean end-to-end test beside it. It is also
the design that fails, for a structural reason rather than a careless one: whichever of the two CI
does not run is the one that lies. Every project's demo script has rotted, and this repository has
already met that failure once — `docscheck` exists because `SECURITY.md` described an authorization
layer that had never been built.

So the acts live once, and the two entry points differ only in whether a person is watching. Every
act asserts in both modes. That is written down as an ADR rather than as a comment because splitting
them is the "simplification" a future session is most likely to reach for.

### The narration stays on in CI

Only the pauses and the banners go. A failed end-to-end run in a CI log reads as the story it was
telling when it broke, and the error names the act — `act 4, breaking it on purpose: verification of
a deliberately corrupted artifact came back INCONCLUSIVE, want FAILED`. That is worth more to
somebody who has never seen the codebase than any assertion diff.

### The fixture is written by SQL, and only the fixture

Everything the seeder can do through the product's own API, it does: environments, instances,
connections, schedules, tokens and grants. Backups that happened in the past cannot be, and the
missing endpoint is the point. Adherence, retention and the estate view all rest on `completed_at`
being what actually happened, so a route that wrote it would be the one route in the product capable
of manufacturing evidence about an estate.

### Seeded is labelled seeded, by a method rather than by a convention

`Narrator.Seeded` exists so that the label cannot be forgotten. Every sentence about history that
Fleetward did not observe goes through it and comes out marked, and the same sentence appears in
`docs/demo.md`: *the history is seeded so the screen has something to say; everything from act 3
onward is live.*

### Act 7 is a stub and stays one

An alert firing on act 4's failed verification is the most dramatic beat this product will ever
have, and it is not built. Staging it would have made every other claim in the demo worthless. The
act prints what does not exist, names B7, and ends with the sentence the whole thing has to carry:
*this is a work in progress at slice six of sixteen.*

### The declared expectation and Fleetward's own schedule are deliberately different

Every seeded instance declares `expected_cron` of `0 2 * * *` with two hours of grace — the thing
adherence holds it to — while its `cron_expression` is `0 3 1 1 *`, an occurrence the demo will not
reach. A dozen backup jobs firing partway through a recording would contend with the sandbox act 3
needs, and nothing in the demo depends on the scheduler firing. This is exactly the separation
[ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md)
introduced, used as designed.

### `docker compose up` without `--wait`

Compose's readiness is the union of every service's health check, and it answers "one of them will
never be healthy" by waiting for the whole timeout — seven minutes of a five-minute demo, spent on a
service the demo asserts nothing about. What the demo needs is answered better and faster by
`/readyz`, which names every dependency of the control plane, and by a real query against the
monitored database. Both poll, so both return the moment the answer is yes.

A container that did not start is still reported, by name, and if it is `web` the demo says plainly
that act 4's screen going red is the one beat that run cannot show.

### The monitored database is seeded through the container, not through the published port

This is the one place the demo writes to a monitored instance, and it drops and creates tables. A
developer machine often already runs a SQL Server on 1433, and on Windows both it and Docker's port
proxy can bind it at once — so a host connection is not proof of which server answered. Going
through `docker compose exec` makes it impossible for the demo to do that to somebody's own server.

The same class of problem, one layer down, is why the metadata connection asks for the tenant
migration 000001 seeds rather than settling for a successful `Ping`, and why failing that check
prints `echo FLEETWARD_POSTGRES_PORT=55432 >> .env` rather than a driver's error.

---

## What the walk found

**`localhost` is not `127.0.0.1`, and the difference cost the first three runs.** The demo's
metadata connection resolved to `::1:5432`, which on this machine is the PostgreSQL the operating
system installed, and the failure surfaced as `password authentication failed for user "fleetward"`
— which reads exactly like a broken stack. Compose's own health checks already say `127.0.0.1` for
this reason, in a comment written during an earlier slice. The demo now does too, throughout.

**The demo has to read `.env`, because compose does.** Having moved the published port to 55432 to
dodge the collision above, the next run went looking for the metadata database on 5432 — where it
found the machine's own again. A twenty-line reader for `KEY=VALUE`, consulted below the process
environment, which is compose's own precedence.

**A verification that could not start its sandbox came back `INCONCLUSIVE`, not `FAILED`.** Docker
Desktop on this machine had filled its VM and could not start any container at all; the product
reported *we could not tell* rather than *this backup is bad*, which is
[ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md) working unprompted and in
the one place it matters. The demo's own error message then said so, and named the act. Nothing was
changed as a result of this; it is recorded because it is the first time that distinction was
observed outside a test written to produce it.

**A backup of an empty database verifies, and proves nothing.** `fleetward_demo` is created empty by
the SQL Server image, so the manifest act 3 captures would have had no objects in it and act 4 would
have rested on the checksum alone. The demo now gives the monitored server 1,200 customers and 9,400
orders to lose, and act 3's `record_counts` check has something to count.

---

## Not built, deliberately

- **Alerts.** Act 7 is a stub. **B7.**
- **Any new UI screen.** The demo shows the estate view that exists. A beat needing a screen that
  does not exist was cut rather than the screen built.
- **Browser automation.** The terminal acts are captured by the program; the two screenshots of the
  estate view before and after act 4 are taken by a person. A driver dependency, a CI service and a
  class of flakiness, in exchange for two images somebody can take in ten seconds, is not a trade
  worth making.
- **A configurable estate.** One good fixture beats a knob nobody turns.
- **A hosted demo, a public instance, or anything with a URL.**

## Still open

- **The demo asserts nothing through a browser.** The compose smoke test asserts the web UI is
  served; the demo asserts the API. If the estate view stopped rendering the two-part status
  correctly, the web unit tests would catch it and the demo would not.
- **`make demo-check` must not run beside `make conformance` or `make test-integration`.** All three
  start containers and contend for one Docker daemon. CI sequences the demo after the compose smoke
  test for the same reason; locally it is a habit rather than a guard.
- **The retention act waits for a real sweep**, bounded at three minutes. The development stack
  sweeps every minute, so this is a wait of seconds — but it is the one act whose duration depends
  on a timer rather than on work finishing.
- **A run interrupted between the corruption and the second verification leaves a corrupted
  artifact** in the bucket of a stack started with `-keep`. It is a demo stack and the next run
  clears the estate, but the artifact stays until `docker compose down --volumes`.
