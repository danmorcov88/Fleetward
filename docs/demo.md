# The demo

```bash
git clone https://github.com/danmorcov88/Fleetward.git
cd Fleetward
make demo
```

One command. It brings the development stack up, builds an estate with something to say, takes a
real backup, restores it into a throwaway container and checks it against the manifest — and then
overwrites a byte of that artifact where it actually lives and shows the same check going red.

> **A backup that has never been restored is a hypothesis.** That sentence is the product, and this
> is where it is said out loud.

It takes three to five minutes on a machine whose images are already pulled, and it needs Docker and
Go 1.25+. `make` is not required: `go run ./tools/demo` is the same thing.

| | |
|---|---|
| `make demo` | run it, narrated, and tear the stack down afterwards |
| `make demo-keep` | leave the stack up, so the estate view can still be clicked around |
| `make demo-check` | the same acts as an end-to-end test, without narration — what CI runs |
| `go run ./tools/demo -compose=false` | run against a stack that is already up |
| `go run ./tools/demo -cast x.cast` | record it, in asciinema's format — see [Recording it](#recording-it) |

---

## What is seeded, and what is live

This is the first section rather than an appendix, because a demo is the most dangerous document
this repository produces and the honest version has to arrive before the persuasive one.

**Seeded — a fixture, written directly into the metadata database:**

- **Twelve instances across three environments, and six weeks of backup history.** An estate view
  with three rows saying "never" demonstrates nothing. Every seeded backup is a row and nothing
  else: no object exists behind a seeded artifact.
- **The verdicts attached to that history** — which of those backups verification proved
  restorable, and which it did not.
- **Five expiries, on one instance, stamped into the past**, so the retention act has something to
  show without waiting a fortnight. Every other seeded backup has no expiry at all, which is what
  stops the hourly sweep quietly deleting six weeks of history mid-demo
  ([ADR-0031](adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md)).
- **Two tables and a few thousand rows in the monitored SQL Server**, so the manifest the live
  backup captures describes something. This is the one place the demo writes to a monitored
  instance, and Fleetward itself never does: here the demo is standing in for the application that
  would own that database.

**Live — the product doing the thing, against this stack:**

- The stack itself, and `/readyz` with every component named.
- The health of all twelve instances. Every one is genuinely probed. Ten point at the two real
  engines in the stack under different names; two point at a hostname under `.example.invalid`,
  which is reserved never to resolve, so the red cells are answers rather than decoration.
- The backup in act 3, taken by the engine's own `BACKUP DATABASE`.
- The verification in act 3: a real container of the matching engine, a real restore into it, real
  row counts compared against the manifest, and the container destroyed.
- The corruption in act 4: real bytes, changed in the real object store.
- The verification in act 4, and the `FAILED` it returns.
- The 403 in act 5, decided by the server, and every audit row shown.
- The retention answer in act 6, read through the same query the sweep runs.
- The alert in act 7: opened by an evaluation pass nobody asked for, and delivered to an HTTP
  listener the demo really runs on the host. The listener is the demo's; the POST that reaches it is
  the control plane's.

The line the demo says on screen, and the line to use in anything published from it:

> The history is seeded so the screen has something to say; everything from act 3 onward is live.

**Nothing is mocked. Nothing is staged. No screenshot shows a feature that exists only in a brief.**

---

## The acts

**Act 0 — the stack.** Pre-flight, then `docker compose up -d --wait`, then `/readyz` green with
every component named. The pre-flight is there because every failure at this point is boring —
Docker is not running, the socket group is wrong, an image is not pulled — and each of them, left
unchecked, surfaces forty seconds later as something that looks like a product defect.

**Act 1 — an estate you can believe.** Three environments and twelve instances, arranged to have
something to say:

| Instances | What they show |
|---|---|
| six healthy, adherent, verified | the ordinary case, so the exceptions read as exceptions |
| two observed-only | an estate that already backs itself up, reported on without changing anything ([ADR-0015](adr/0015-observed-and-managed-backups.md)) |
| one behind its window | the gap the product exists to surface |
| one succeeding and failing verification for a fortnight | the case invisible to every tool that only checks whether the job ran |
| two unreachable | pointed at a hostname that genuinely does not resolve |

Everything the seeder can do through the product's own API, it does: environments, instances,
connections, schedules, tokens and grants. Only history is written directly, because there is no
endpoint for it and there should not be — you cannot ask Fleetward to have taken a backup last
Tuesday, and a route that let you would be the one route in the product that can manufacture
evidence.

**Act 2 — declare, detect, gap.** `fleetward-cli backup adherence` on the seeded estate. One
command, and it is the whole product thesis: what was declared, what was detected, and the
difference. Four answers, and the difference between them is the point — `ADHERENT`, `MISSED`,
`UNPROVEN`, and `NOT_DECLARED`.

**Act 3 — the loop, live.** A real backup, then a real verification: a throwaway container of the
matching engine starts, the artifact restores into it, row counts are compared against the manifest
captured at backup time, and the container is destroyed on every path out.

**Act 4 — break it on purpose.** One byte in the middle of the artifact is overwritten in MinIO. The
backup row is not touched: it still records the original size and checksum. The same verification is
run again and comes back `FAILED`, and the estate view turns red.

Then the sentence that is the reason to watch: **the green result in act 3 is only worth anything
because this one is red.** A verification that has only ever been shown to pass is
indistinguishable from one that always passes.

And the distinction nothing else in this category draws: `FAILED` is reserved for evidence about the
artifact. Everything else — an unreachable sandbox, a plugin that died, a timeout — is
`INCONCLUSIVE`, because "we could not tell" and "we can tell, and it is bad" are different answers
([ADR-0022](adr/0022-failed-and-inconclusive-are-different-answers.md)).

**Act 5 — who may do this.** A `viewer` refused with a real 403 that came from the server, a `dba`
allowed, and both attempts in an audit log the database itself refuses to let anybody edit — the
demo tries a direct `DELETE` against it and shows the refusal. Then the rows written by
`system:retention`, because "who deleted this artifact" having an answer is the part people do not
expect.

**Act 6 — what it refuses to delete.** The artifact past its retention that is kept anyway, because
it is the last backup of that instance anybody has proven restorable
([ADR-0032](adr/0032-retention-never-deletes-the-last-good-backup.md)).

**Act 7 — the alert.** The three rules a fresh installation is seeded with, then the `critical` alert
that fires on act 4's corrupted artifact, then the webhook that arrives on the host because of it —
with the notifier's credential in a header and nowhere in the body, and `GET /api/v1/notifiers`
returning the destination without it. A second evaluation pass finds the same condition and creates
no second row, which is what `alerts.fingerprint` has existed for since the first migration. The
alert is acknowledged, and the `alert.acknowledge` row appears in the audit log.

Two things about that act are worth knowing before it is published from.

**The notifier is created before act 4 runs, and the act says so on screen.** That is the product's
semantics rather than the demo's convenience: a notification goes out on the transition into
`firing`, so a destination configured *after* an alert has already opened is correctly told nothing
about it ([ADR-0039](adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)).

**Nothing fires for an `INCONCLUSIVE` verdict, and the act says that too** — as a property, without
staging one. A sandbox that would not start is not evidence that a backup is bad, and routing it
through the same alert as a proven-bad artifact is how the alert that matters gets muted
([ADR-0040](adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md)).

---

## Recording it

Three artifacts, all from one run:

**The terminal.** The demo records itself. `-cast` writes an
[asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/) file — the same format `asciinema`
produces — and `-transcript` writes plain text:

```bash
go run ./tools/demo -keep -pause 4s   -cast fleetward-demo.cast -transcript fleetward-demo.txt
```

It writes its own recording rather than being recorded, for a reason that is practical rather than
clever: asciinema's CLI is Unix-only and this project is developed on Windows, and what a recorder
produces is a JSON header plus one `[seconds, "o", text]` line per write — both halves of which the
demo already has, being the thing doing the writing. Every timestamp in the file is when that line
was actually printed. Nothing is re-enacted afterwards.

**The video.** [`agg`](https://docs.asciinema.org/manual/agg/) is a single binary from the asciinema
project — [releases here](https://github.com/asciinema/agg/releases), including
`agg-x86_64-pc-windows-msvc.exe` — and turns the cast into a GIF:

```bash
agg --theme asciinema --font-size 16 --cols 100 --rows 46     --idle-time-limit 3 --last-frame-duration 4 --fps-cap 12     fleetward-demo.cast fleetward-demo.gif
```

`--rows 46` matters: the demo emits up to forty lines in one burst, and a shorter terminal scrolls
the top of an act away before a frame is captured. `--idle-time-limit 3` is what makes it watchable
— a verification honestly takes fifteen seconds, and fifteen seconds of a still frame is dead air.
The gaps are shortened on playback and stay in the file, so the recording keeps the real timings.

For an MP4, ffmpeg — and `pip install imageio-ffmpeg` ships one, so nothing has to be installed
system-wide:

```bash
ffmpeg -i fleetward-demo.gif -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2,fps=12"        -c:v libx264 -crf 20 -pix_fmt yuv420p -movflags +faststart fleetward-demo.mp4
```

A run of the acts at `-pause 4s` gives about 46 seconds of video.

**Two screenshots of the estate view**, taken by a human, at <http://localhost:3000>: the same rows
before act 4 and after it. Run `make demo-keep` so the stack survives, and pause on act 4. The
second image is the one the whole thing rests on. Browser automation is deliberately not part of
this — adding a driver dependency to produce two images is not a trade worth making.

**A written summary** saying what was shown, what was seeded, and what is not built yet. The framing
to use:

> Most backup tooling tells you the job finished. This restores the artifact into a throwaway
> container, counts the rows against a manifest captured at backup time, and destroys the container.
> Then it corrupts the artifact on purpose and shows the same check going red — because a
> verification that has only ever been seen to pass is indistinguishable from one that always
> passes.

And the sentence that has to be in anything published from it:

> This is a work in progress at slice seven of sixteen — Fleetward emits no metrics about itself,
> nothing has been released, and five of the eight engines still only handshake.

[`docs/dev/STATUS.md`](dev/STATUS.md) is that list, and it is kept accurate on purpose.

---

## The demo is also the end-to-end test

`make demo` and `make demo-check` run the same functions from the same package. The demo narrates
and pauses; the check does neither, and asserts exactly the same things. Every act asserts in both
modes — there is no mode in which the demo prints a result without checking it.

That is deliberate and it is recorded as
[ADR-0037](adr/0037-the-demo-and-the-end-to-end-test-are-one-program.md), because the obvious future
simplification is to split them, and splitting them is how the demo starts lying. A demo nothing
runs is a claim about the product with no gate behind it, and this repository's whole quality
argument is that a claim is trustworthy because a merge gate enforces it.

CI runs `make demo-check` on every pull request, as the `End-to-end demo` job. It is sequenced after
the dev-stack smoke test because both need the runner's single Docker daemon — and for the same
reason, neither `make demo` nor `make demo-check` should be run beside `make conformance` or
`make test-integration` locally.

---

## Running it twice

The demo is idempotent. It clears the estate a previous run left — environments whose name starts
`demo-`, the instances inside them, and the credentials issued to the two demo users — and seeds
again from scratch. The two demo users themselves survive, because `audit_log` references them and
that table refuses `UPDATE` at the database level, which is the entire point of the trigger.

`docker compose up -d --wait` on a stack that is already running is a no-op, so the second run is
faster than the first.

## When it fails

Every act fails with a sentence naming what to change rather than with a stack trace.

- **`FLEETWARD_DOCKER_GID is not set`** — on Linux the Docker socket is owned by the docker group,
  and the control plane runs unprivileged. `echo "FLEETWARD_DOCKER_GID=$(stat -c '%g'
  /var/run/docker.sock)" >> .env`. Not needed on macOS or Windows, where the socket inside the
  Docker Desktop VM is root-owned.
- **`the sandbox provider is unhealthy`** — the same thing, caught in act 0 rather than in act 3.
- **`the web image failed to build`** — occasionally Docker Desktop reports `failed to prepare
  extraction snapshot`. It is a containerd-snapshotter fault rather than a Dockerfile one, and
  `docker builder prune` clears it.
- **A port is already taken** — every published port can be overridden from a `.env` file. See
  [`.env.example`](../.env.example); the demo reads the same variables.
