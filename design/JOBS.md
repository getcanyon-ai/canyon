# Scheduled jobs — work an app runs on a clock

**Designed, not built.** Build on the owner's go. This file is the source of truth for jobs, and for
how a job run gets its egress and its patches. EGRESS.md and PATCHING.md point here.

Many vibe-coded apps need work done on a clock: the claims portal emails brokers whose policies
renew in 30 days, every weekday at six. That work needs **the app's own data**. So v1 is **scheduled
jobs on an app**, not app kinds. An app declares `[jobs.<name>]` in `canyon.toml`. Each run is a
one-off container on Docker, or a Job on Kubernetes, made from the environment's own image, with that
environment's secrets, resources and network. An app with no web process is the same table plus
`web = false`.

What stays exactly as today: the upload, the gate (D14), approvals, promotion binding a run and
shipping the reviewed digest (executor invariant 4), resources, secrets, sharing.

## How it works

The trace below follows `renewal-reminder`, the job on claims-portal, from upload to its first
production tick.

1. **Upload.** The server reads `[jobs.remind]` with go-toml, in `declarations` beside
   `declaredResources`. It checks the schedule with `cron.ParseStandard`, the parser Kubernetes' own
   CronJob controller uses, and refuses bad input (§2). The jobs are stored on the package row. The
   package hash covers `canyon.toml`, so the review and its cache key cover the jobs.
2. **Gate, then build.** Both run unchanged; every rule is judged, `auth.unauthenticated-route`
   included. `/build`'s result also lists the image's process types, read from
   `io.buildpacks.build.metadata`.
3. **Deploy to preview.** The deploy step reads `canyon.toml` again from the package's archive
   (`Pipeline.archive`) and refuses a package whose jobs differ from the stored ones (§9). It checks
   that each job's `process` is one of the build's process types. It starts the web process as today
   (`RunApp`). It then writes one `job_arms` row per job for preview, **unarmed**. Preview jobs run
   only when someone asks (§4).
4. **A run in preview.** Someone runs `canyon run remind`. The server writes a `job_runs` row
   (`starting`). Inside the step it opens preview's runtime values (`Pipeline.secrets`, never a step
   output) and calls `POST /job` with a run id. The executor starts a one-off container or Job of
   preview's image. The server marks the row `running`. The executor's own loop enforces the timeout
   from the container's start (§4). The server's one-minute sweep reads `GET /jobs` and closes the
   row: `succeeded` with exit 0, `failed` with its code, or `timed_out`. Each close emits `job.finished`.
5. **Promote.** The receipt evaluates `cron.preview-run` (§8). For each job it looks for a `job_runs`
   row that ran preview's **image digest** and exited 0. With none, the receipt **warns** and the
   promotion goes on — it never waits on it (decided 2026-10-11, the owner). The promote run starts production's web process as
   today and arms production's jobs. It **upserts** the DBOS schedule `job:<app>:prod:<job>` with
   `ApplySchedules`: the spec is replaced, and the schedule's id, status and last fire are kept. It
   writes `armed_at` when the spec changed. The promote audit row names the evidence run, or says
   a job had none.
6. **The first production tick.** DBOS fires `sched-job:…-<tick RFC 3339>` on the dedicated queue
   `canyon-job-ticks`. The tick workflow reads production's deploy row and job arm, and writes the
   tick's `job_runs` row. It then calls `POST /job` with the run id derived from the tick, or records
   why it did not (the state table below). Ticks never change or delete a schedule.
7. **What reads the truth.** A run's truth is its `job_runs` row: the server writes it before the
   start and closes it from the runtime. The status read, the fleet's ENV cell, the receipt and the
   promote audit read that table, never container labels or Kubernetes' history. `next_run` comes
   from the armed schedule's own spec, never from the declaration.

```
 person/agent        server (DBOS)                     executor            runtime
     │ canyon deploy      │                                 │                   │
     ├───────────────────►│ gate · build (+processes)       │                   │
     │                    │ arm preview jobs: UNARMED       │                   │
     │ canyon run remind  │                                 │                   │
     ├───────────────────►│ job_runs(starting)              │                   │
     │                    ├── POST /job {run, image, env} ─►│ container / Job ─►│ runs
     │                    │ job_runs(running)               │ deadline loop     │
     │                    │ sweep 1/min ── GET /jobs ──────►│◄── exit 0 ────────┤
     │                    │ job_runs(succeeded) · job.finished                  │
     │ promote            │ receipt: cron.preview-run ✓ (digest)                │
     ├───────────────────►│ ApplySchedules(job:app:prod:remind) · armed_at      │
     │                    │ ……… 06:00 Europe/Berlin ………      │                   │
     │                    │ tick sched-…-2026-10-13T06:00:00+02:00             │
     │                    │ job_runs(starting) ─ POST /job ►│ container / Job ─►│ runs
```

**The `job_runs` states.** There is one row per expected tick and one per manual start.

| State | Written by | Means | Fleet / receipt |
|---|---|---|---|
| `starting` | tick or manual start, before `POST /job` | a start was asked | — |
| `running` | after `POST /job` answers | the container or Job exists | — |
| `succeeded` | sweep, from `GET /jobs` | exited 0 | evidence for `cron.preview-run` |
| `failed` | sweep | exited non-zero (`exit_code`, masked `said`) | ✗ last run failed |
| `timed_out` | sweep | stopped at `timeout` after its container started | ✗ last run failed |
| `skipped: running` | tick | an earlier run of this job is still starting or running | shown, not a failure |
| `skipped: no deploy` | tick | no deploy row or job arm names this job: the reconcile is behind | ✗ last run missed |
| `failed to start: <reason>` | tick or manual start | the executor refused, or answered Busy for the whole hour | ✗ last run failed |
| `missed` | sweep | the tick is more than an hour old and never ran (Canyon was down) | ✗ last run missed |
| `lost` | sweep | `GET /jobs` no longer knows a `running` row | ✗ last run failed |

Each row carries the project, environment, job, run id, tick (none for a manual run), `trigger` ∈
`schedule | manual`, actor (`canyon` for a tick, the person for a manual run), image digest, Docker
address, the start and end times, the exit code and the masked `said`.

## 1. What is true today

- **One process shape.** `/run` names one container `<app>-<target>` and is up only once it answers
  on port 3000 (contracts/executor.md, *POST /run*, *Port 3000*). Restarts follow the web's rules
  (*An app that exits is started again*). A script that exits after its work fails the start. Kubernetes
  pods run `canyon-identity` as a native sidecar, which requires Kubernetes 1.29 (*On Kubernetes*).
- **`canyon.toml` has no jobs.** The CLI's `readProject` refuses every section but `[resources]` and
  `[roles]`. The server's `declarations` reads three keys and ignores the rest. Both use go-toml v2.
- **Resources belong to one project** (`resourcesOf`). So a job that needs claims-portal's database
  must run inside claims-portal: that is decision 1.
- **The executor decodes its requests as an allow-list** (`DisallowUnknownFields` in both executors'
  `http.go`). An executor older than a new field refuses it.
- **DBOS v1.3.0** (go.mod) has database-backed schedules. `ApplySchedules` upserts by name and keeps
  the id, status and `last_fired_at` (`CreateSchedule` makes a new id). `ScheduleSpec` carries
  `CronTimezone` and `QueueName`. A live tick's workflow id is `sched-<name>-<tick RFC 3339>`
  (`buildDBScheduleFunc`), and `BackfillSchedule` enqueues missed ticks under that same id, skipping
  one that exists. `TriggerSchedule`'s id carries `now`, so it never dedupes: unused here. Neither
  `ApplySchedules` nor `BackfillSchedule` may be called inside a workflow. Its parser has a seconds
  field. robfig/cron/v3 (MIT) goes from indirect to direct.
- **The server runs one replica** (`deploy/k8s/server.yaml`); two only during a rolling update.
  Queues with a global concurrency limit exist (`registerQueues`).
- **Labels are never read** (#1891, *What a /run creates is labelled*), but for scans, swept by
  `canyon.scan`: the precedent for jobs (§5).

## 2. `canyon.toml`

```toml
app         = "claims-portal"
description = "Claims intake and review for brokers"

[jobs.remind]
schedule = "0 6 * * 1-5"      # five fields or @hourly @daily @weekly @monthly @yearly
timezone = "Europe/Berlin"    # IANA; UTC when absent
timeout  = "15m"              # from the container's start; default min(1h, shortest gap − 1m)
process  = "remind"           # a process type of the build: the Procfile's `remind: node remind.js`

[resources]
db = { type = "postgres" }
```

A jobs-only app adds `web = false` at the top. `process` may then be left out, and the image's
default process runs.

**Refused at upload** with `400 {error:"invalid_jobs", message}` (`refuseDeclaration`). Every message
below is proposed.

| Declared | Message |
|---|---|
| a job name not matching `^[a-z][a-z0-9-]{0,29}$`, or more than 10 jobs | `job "<j>": a job name is lowercase letters, digits and dashes, at most 30; an app has at most 10 jobs` |
| a key under a job other than the four | `job "<j>" has <key>: a job has schedule, timezone, timeout and process` |
| `schedule` missing, or refused by `cron.ParseStandard` | `job "<j>": schedule "<s>" is not a cron schedule: <parser's reason>` |
| `@every …`, which parses to `cron.ConstantDelaySchedule` | `job "<j>": "@every" is an interval: write it as minutes, as "*/15 * * * *"` |
| a `TZ=` or `CRON_TZ=` prefix | `job "<j>": the time zone goes in timezone, not in the schedule` |
| `Next` from now answers the zero time | `job "<j>": schedule "<s>" never runs` |
| a shortest gap under 5 minutes (computed below) | `job "<j>" runs every <gap>: a job runs at most every 5 minutes` |
| `timezone` refused by `time.LoadLocation` (the binary embeds `time/tzdata`) | `job "<j>": timezone "<z>" is not an IANA time zone, as Europe/Berlin` |
| `timeout` refused by `time.ParseDuration`, under 1m, or at least the shortest gap | `job "<j>": timeout must be at least 1m and shorter than the <gap> between its runs` |
| no `process` on a web app | `job "<j>" needs process = "<type>": a web app's default process is its web server` |
| `web = false` with no jobs | `web = false needs at least one [jobs.<name>]: otherwise nothing runs` |

**The shortest gap** is the smallest interval between consecutive `Next` times over the next 366
days, in the declared zone. That catches a daylight-saving week. It takes at most about 105,000
iterations at the 5-minute floor.

**The CLI** accepts `web` and `[jobs]` (`readProject`) and leaves checking them to the server. An
older CLI refuses `[jobs]`, which fails closed. The package row records the jobs as JSON and `web`.
There is one new migration, numbered when it lands, which also adds `job_arms` and `job_runs`.

## 3. The experience

### 3.1 `canyon deploy`

```
  ✓ Deploy     preview

  Live   http://claims-portal.preview.localhost:7777 · answered 200 OK at /
  Jobs   remind   0 6 * * 1-5 · Europe/Berlin   runs in preview only when you ask: canyon run remind
  …
  Next: canyon run remind, then canyon deploy --env prod
```

A jobs-only app prints no `Live` line and writes no access line: D5's `Private:` line is about
opening the app, and there is nothing to open. The deploy says what it did not check: `Nothing has run
yet: canyon run <job> shows whether it starts.` A bad image is green at deploy, and `cron.preview-run`
stops it before production. On `--env prod` the line reads
`Jobs   remind   armed · next Mon 13 Oct 06:00 Europe/Berlin`. All of these lines are proposed.

### 3.2 `canyon run <job>` (new verb, the eighteenth command)

```
$ canyon run remind

  claims-portal · preview · remind · run jr_4f1c9a2e

  found 14 policies renewing in 30 days
  DRY RUN (CANYON_ENV=preview): would send 14 reminders

  ✓ Finished · exit 0 · 12s
```

The verb starts a run of what the environment runs and follows its log. It exits 0 when the run
exits 0, and 1 otherwise: `✗ Exited 1 after 12s · canyon logs --run jr_4f1c9a2e` · `✗ Stopped at its
15m timeout · canyon logs --run <id>`. Ctrl-C stops the following, never the run: `The run goes on;
canyon status shows how it ends.` `--env prod` needs owner level (§7).

Its refusals print the server's message: `remind is already running in <env> (started <time>). One
run at a time.` · `<app> has no job "<j>" in <env>: canyon status lists them.` · `Your platform runs
<app>: jobs aren't handed off yet.` All strings in this section are proposed.

### 3.3 `canyon status`, `logs`, `fleet`, `--format ai`

```
  production   v1.1   https://claims-portal.prod.…   2d ago
    remind     armed · next Mon 13 Oct 06:00 · last Fri 10 Oct 06:00 ✗ exit 1   canyon logs --env prod --run jr_9b…
  preview      v1.2   http://claims-portal.preview.…   1h ago
    remind     runs when asked · last 10:42 ✓ exit 0 (12s)
```

- **`canyon logs --run <id>`** prints one run's log, readable while the run is among its job's
  newest 5 and at most 24 hours (§4).
- **`canyon fleet`'s ENV column** adds `✗ last run failed` or `✗ last run missed` to an environment.
  Both come from `job_runs`, with no executor call.
- **`--format ai`** carries a `jobs:` block. Its steps (proposed) are:
  1. `Run each job once in preview: canyon run <job>. The skill's rule: a run in preview must be dry
     (read CANYON_ENV).`
  2. `Promoting warns until each job ran clean in preview.`

  The skill adds two rules. *Make a job's side effects conditional on `CANYON_ENV == "prod"`.* And:
  *never run `canyon run --env prod` unless the user asks for a run in production now.*

### 3.4 Dashboard

- **The app page** gets a *Jobs* card per environment. Each job shows its schedule verbatim (never
  rewritten into words by a guesser), its zone, `Armed · next <time>` or `Runs only when asked`, and
  its last 10 `job_runs` rows (state, trigger, who, duration, exit code, `said`). **Run now** shows to contributors in preview and to owners in production, and is disabled while
  this job runs. In production while `prod_approval` is on, it shows only in a browser.
- **The receipt** shows the `cron.preview-run` row with its evidence run, or its warning.
- **The fleet's ENV cell** says `✗ last run failed` or `✗ last run missed`. `app.updated` carries it.
- **Read-only members** see nothing of jobs. Jobs are technical detail (PERMISSIONS.md).

## 4. One semantics, both executors

**One scheduler.** The server schedules on both executors through DBOS. The executors only start and
report runs. That gives one place for the semantics and one test suite.

**The trade-off, stated honestly:** while Canyon is down, no tick fires. A web app keeps serving
through a Canyon outage on Kubernetes. A job does not. The one-hour catch-up below covers restarts and
short outages. A longer outage writes `missed` rows that the fleet shows.

| Semantic | Rule (both executors) |
|---|---|
| Grammar | `cron.ParseStandard` at upload. DBOS is handed `"0 " + schedule`, or a descriptor unchanged. The two parsers' `Next` times are equal: a test proves it (§10) |
| Time zone | IANA, UTC by default. DBOS `CronTimezone` |
| DST | the library's rule. A wall time that does not exist that day does not run (Europe/Berlin 2026-03-29 02:30). One that exists twice runs once (2026-10-25 02:30) |
| Concurrency | **one run per job** at a time: a tick while it is starting or running is `skipped: running`. **One run per environment**: another job's tick waits durably while the environment's run goes on (§6's fixed address). Manual starts follow the same rules (`409 running`) |
| Missed | a tick not fired live runs once its catch-up sweeps it, within an hour. Older ticks are `missed` |
| Deadline | `timeout` counts from the **app container's start**, never from the Job's start: a cold image pull on EKS does not count. Starting is bounded separately at 10 minutes, then `failed to start: not started in 10m` |
| Retries | none. A non-zero exit is `failed`, and the next tick runs again. Most jobs send, write or charge |
| Values | the environment's runtime values **at each run**, opened by the server inside the start step and carried on `POST /job`. A secret rotated after a leak reaches the next run on both executors |
| `CANYON_ENV` | `preview` or `prod`, set by the executor in every container and refused in `env`, as `PORT` is |
| `PORT` | **not set** in a job: nothing listens |
| Identity | no `CANYON_IDENTITY_*`: a job receives no requests |
| History | the newest 5 finished runs of each job, kept at most 24 hours, for logs. `job_runs` keeps the record for 90 days |

**Arming is owned by deploy, promote, undeploy and delete.** Each upserts or deletes the job's DBOS
schedule `job:<app>:<env>:<job>` with `ApplySchedules` or `DeleteSchedule`. Preview is never armed.
Production is armed by a promotion, which upserts the schedule. An image-only promotion keeps
`armed_at`. A changed spec or zone sets `armed_at` to now, so no tick of the old spec is ever caught
up under the new one. A job the new package drops is deleted. Neither call may run inside a workflow
(§1). So the deploy step writes `job_arms` in its own transaction, and the server's DBOS Client
applies the schedules after that transaction commits. **At boot**, the server reconciles every
schedule against `job_arms`, which are written beside the deploy rows. It upserts a missing or
different schedule, deletes one no arm names, and logs each change. PR 1 proves the order.

**The tick workflow never touches a schedule.** It runs on the queue `canyon-job-ticks`, whose global
concurrency is `CANYON_JOB_STARTS` (default 8). It:
1. reads production's deploy row and job arm. With none it records `skipped: no deploy`, and a database
   error retries the step, never deleting anything;
2. writes the `starting` row under the run id derived from the tick;
3. calls `POST /job`. A `503 busy` waits durably, 30 seconds per try, until the tick plus one hour,
   then records `failed to start: the host was running <n> jobs for an hour`;
4. marks the row `running`, and ends.

The workflow only starts runs, so the queue's limit bounds starts, never run lengths.

**The one-minute sweep** is a static DBOS schedule of its own. For each armed schedule it:
- calls `BackfillSchedule(name, max(armed_at, now − 1h), now − 1m)`. The missed ticks get the live
  ticks' ids, so a tick already run, or run by another replica during a rolling update, is skipped by
  the workflow table's primary key;
- writes a `missed` row for each tick between `armed_at` and `now − 1h` that has no row, at most 100
  per job per sweep;
- reads `GET /jobs` once per environment that has `running` rows, closes the ended ones, and emits
  `job.finished`.

The sweep also catches the tick DBOS's 30-second reconciler can miss right after arming.

**Run ids.** A tick's run id is `jr_` plus the first 16 hex characters of
sha256(`app/env/job/tick`), so it is deterministic. A manual run's id is random and written in its row
before `POST /job`. Both executors name the container or Job `canyon-job-<16 hex>`. **`POST /job` is
idempotent on the run id:** an id that exists answers that job and starts nothing. A replayed step, a
second replica or a backfill can never start a run twice.

## 5. The executors

Both executors get two calls, and `/build` gains one field.

- `POST /job {app, target, job, run, image, env, process?, timeout_s, keep[]}` answers
  `200 {run, started_at?, address?}`, the same answer for an existing `run` · `400` on a malformed
  body, `PORT`, `CANYON_ENV` or `CANYON_IDENTITY_*` in `env`, or a `process` the image does not list ·
  `404` on an image that is gone · `409 {error:"running", run}` while another run of this environment
  runs · `503 {error:"busy"}` at `CANYON_EXECUTOR_JOBS` running jobs (default 20 on Docker, 50 on
  Kubernetes) · `502` when the executor cannot tell.
- `GET /jobs?app=&target=` answers `{jobs: [{run, job, image, started_at?, ended_at?, state:
  starting | running | succeeded | failed | timed_out, exit_code?, said?}]}`. It answers `502` when it
  cannot tell, never `[]`.
- `GET /logs?…&run=` answers that run's app container log. Invariant 5 is amended: `/logs` answers the
  named run's container and no other's.
- `/build`'s result adds `processes[]` from `io.buildpacks.build.metadata`.
  - Docker reads it with `docker image inspect`, as `ran` already does.
  - Kubernetes reads it from the image config in the registry, which it already asks about digests.
  - A reused image keeps its recorded list.
- **Cleanup.** After `/run` and `/job` the image cleanup keeps every image in `keep`. That is every
  image a deploy row of the app names (§8), as well as what runs. A jobs-only app never calls `/run`,
  so this matters.
- **Labels.** The #1891 amendment gains one exception: job containers and Jobs are found by their
  labels `canyon.run`, `canyon.app` and `canyon.target`. The scan precedent applies, and parsing the
  name is ambiguous, since app ids contain dashes.
- **Invariant 12:** a job run is stopped within 15 seconds of its deadline while the executor runs.
  On Kubernetes the deadline holds once written, whether or not the executor runs. On Docker, a run
  past its deadline when the executor starts is stopped at once.
- **The header** becomes *Ten calls, and two more on Kubernetes*. `contracts/schema/executor.schema.json`
  gains `job` and `jobs`.

**Docker.** `POST /job` first creates the app's network if it is missing (`ensureNetwork`): a
jobs-only app has no `/run` to create it. It then runs
`docker run -d --name canyon-job-<hex> --network canyon-app-<app> --ip <§6 address> --restart no`
with:
- the web start's limits (`--memory 1g --pids-limit 512 --cap-drop ALL --security-opt
  no-new-privileges`);
- `--log-opt max-size=10m --log-opt max-file=1`;
- `--entrypoint /cnb/process/<process>` when `process` is named;
- the labels above plus `canyon.deadline_s`.

The **deadline loop** is its own goroutine every 15 seconds, without `e.nets` (`Rejoin` can hold that
lock for up to two minutes). It kills a job container whose `State.StartedAt` plus timeout has passed.
It runs once at the executor's start too. A run is `timed_out` when it ended at or after that instant,
read from `State.FinishedAt`, never from the exit code.

The same loop removes finished job containers beyond the newest 5 per job, or older than 24 hours.
`dropNetwork`'s in-use check counts job containers.

**Kubernetes.** `POST /job` creates a bare Job `canyon-job-<hex>` in `canyon-apps`:
- **The pod spec** comes from the function that builds the Deployment's (`Executor.deployment` in
  `objects.go`), with the job switches: no Service, no readiness probe, `restartPolicy: Never`,
  `backoffLimit: 0`, no 4180, no access ConfigMap, `CANYON_ENV`, no `PORT`, and the command
  `/cnb/process/<process>` when named.
- **The NetworkPolicy** `<app>-<target>` already selects the pod by its labels: ingress nothing new,
  egress as the app's.
- **The order of writes.** The Job is created with `suspend: true`. Then the Secret
  `canyon-job-<hex>` is created with the run's values, owned by the Job. Then the Job is patched to
  `suspend: false`. The Role grants `create` and `delete` on Secrets and never `patch`, so ownership
  must be set at creation.
- **The deadline.** The Job has `ttlSecondsAfterFinished: 86400` and `activeDeadlineSeconds` set to
  timeout plus 600, the start allowance. Once the `app` container's `state.running.startedAt` is known,
  the executor's 15-second loop patches `activeDeadlineSeconds` to
  `(startedAt − status.startTime) + timeout`. From then the Job controller enforces it as
  `DeadlineExceeded`, even while the executor is down.
- **Retention.** The executor deletes Jobs beyond the newest 5 finished per job.
- **The Role** gains `jobs` in `batch` (create, delete, get, list, watch, patch) in `canyon-apps`
  only.

## 6. Egress by kind

A job run must call out exactly as its web process does: through the same door, as the same
`spiffe://…/canyon/<app>/<env>`.

- **Kubernetes.** A job pod keeps `canyon-identity` as a **native sidecar in forwarder-only mode**
  (`CANYON_IDENTITY_MODE=forward`). It runs no 4180 listener, no 4183 and no JWKS. It holds the
  environment's projected token, audience `canyon-egress`, and the `127.0.0.1:4184` forwarder
  (EGRESS.md §4.3). The pod runs under the environment's ServiceAccount
  `canyon-app-<app>-<target>`, so the gateway's token check names the same `(app, env)`. A native
  sidecar ends when the main container exits (Kubernetes ≥ 1.29, already required), so the Job
  completes. Until EGRESS layer 3 ships, a job pod carries no sidecar. In layers 1–2 the egress
  gateway attributes it by pod IP through the executor's `POST /callers`, and the record's `run` is
  the job run's `canyon.run` label. The forwarder-only sidecar, the projected token and the
  ServiceAccount arrive with EGRESS layer 3 (its PR 24).
- **Docker.** Job containers take a **fixed per-target address** that the executor reserves: `.14`
  for preview and `.15` for production, in the lower /25 of the app's /24. The web containers keep
  `.10–.13`, and EGRESS's `--ip-range` keeps dynamic addresses in the upper /25. The address is
  deterministic, so the access-log receiver (layer 1) and the policy (layer 2) know it from the
  plan; nothing is written when jobs are armed.
  `POST /job` answers it, and a mismatch fails the run and stops the container. One address per
  target is why only one job runs per environment at a time (§4).
  - On a network made before EGRESS's `--ip-range`, a dynamic container may already hold the address.
    The start then fails with `failed to start: <address> is in use`; it never takes another address.
  - PR 5's red test proves that an exited container frees its `--ip`. If Docker holds it, the
    executor disconnects the exited container from the network, which keeps its logs.
- `POST /job` gains `egress {proxy, no_proxy[], ca?}`, the same block as `/run` (EGRESS.md §9); a
  patch's job runs carry the egress spec recorded on the deploy row (EGRESS.md §2.5).
- EGRESS.md follows this section: §2.2 *Every kind*, §2.3's slot table and §4.3's identity table.

## 7. Who may do what (PERMISSIONS.md first, its own PR)

| Route | Minimum | Refusals |
|---|---|---|
| `POST /api/v1/projects/:id/envs/preview/jobs/:job/runs` | `contributor` (*Change it*) | `403 team_required` |
| `POST /api/v1/projects/:id/envs/prod/jobs/:job/runs` | **`owner`**. While `prod_approval` is on, a browser session only | `403 owner_required` · `403 {error:"dashboard_required", message:"While production is approved in the dashboard, a production run is started there."}` (proposed) |
| `GET /api/v1/projects/:id/envs/:env/jobs` and `…/jobs/:job/runs?cursor=` | `contributor` (*technical detail*) | `403 team_required` |
| `GET /api/v1/projects/:id/logs?env=&run=` | `contributor`, as logs | `403 team_required` |

- **Every start route** answers `202 {run: {id, state:"starting"}}`, or `404` on an unknown job or
  nothing deployed, `409 {error:"running", run}`, `409 {error:"run_by_org"}`.
- **Below contributor** the project read carries no jobs. The fleet's `✗ last run …` cell is
  contributor-only, as the run state is.
- **Admins** pass everything. On a host with auth off, every caller is the admin.
- **The QA plan** covers owner, contributor, read-only, no access and admin, in each environment.

**Audit.** A manual start writes `POST /api/v1/projects/{id}/envs/{env}/jobs/{job}/runs` with
`{env, job, run, image_digest}`. Deploy and promote rows gain the `jobs` they armed (name, schedule,
zone); the promote row gains `evidence: [{job, run, image_digest}]`, with `run: null` for a job
that had none. Scheduled runs write no audit row: their record is `job_runs`, and who armed them is on the
promote row. Refusals write today's `denied` row.

## 8. Promotion evidence and patching by kind

**`cron.preview-run` — *Each job ran clean in preview*** (proposed). It is a new D2 rule, placed only
Before production, **Advisory** — it warns and never blocks (decided 2026-10-11, the owner) — and
editable like the go-live rules.

- **Evaluation.** The receipt evaluates it for every job of the package being promoted. A job passes
  when a `job_runs` row of preview ran **the image digest being promoted** and `succeeded`. Its `why`
  names that run: `remind ran clean in preview: jr_4f1c… (10:42)`.
- **No run.** A job without such a run is an advisory finding, shown on the receipt and the
  promotion's confirm: `<job> hasn't run <version> in preview: canyon run <job>`, or `…'s last run
  of <version> exited <code>` (proposed). Promotion goes on; nothing is approved, and a team whose
  credentials exist only in production promotes without faking a preview run.
- **Scope.** Evidence covers exactly the image it ran, so a rebuilt or patched image shows the
  warning again until it runs in preview.
- **A promotion that removes production's web process** (a `web = false` package over a running web
  app) is refused: `Production serves a web app and <version> has none: undeploy production first.`
  (proposed). In preview, the deploy stops the web container after arming. The receipt never hides a
  downtime.

**Patching by kind.** PATCHING.md §5.4–5.5 references these four rules.
1. **A patch never makes a job run.** Preview's watch reads only the web process's state: restarts,
   `answered`. A jobs-only app has nothing to watch, and that is not a pass.
2. **The production patch of an app with jobs is always asked**, whatever `patching` says, `auto`
   included. The patch approval reads `<job> has not run since patched` (proposed) for each job whose
   last preview run was of another digest.
3. **Evidence follows the digest.** The patched image shows the `cron.preview-run` warning until it
   runs in preview.
4. **Images are kept while a deploy row names them.** Tag cleanup keeps every image a deploy row of
   the app names (`keep`, §5), so production's armed image is never removed under its schedule.

## 9. Fail closed, and rolling updates

- **Bad declarations** are refused at upload (§2), and nothing is stored.
- **Rolling update.** An old server ignores `[jobs]`, so a package it uploaded has no jobs stored. The
  new server reads `canyon.toml` again from the package at deploy and refuses a mismatch:
  `This version was read by an older Canyon: deploy it again to upload it afresh.` (proposed).
- **Old executor.** It refuses `POST /job` with `404` and `/build`'s new field with `400`. The executor
  and the server upgrade together, executor first, as every executor change says.
- **Half a feature is never live.** Until the scheduler PR lands, the deploy step refuses a package
  with jobs: `This Canyon cannot run jobs yet.` (proposed). PR 4 adds the refusal and PR 7 removes it.
  So main never accepts jobs it would not run.
- **Unreadable state is never "fine".** A tick that cannot read its deploy row retries; it never
  records a pass and never deletes. `GET /jobs` that cannot tell leaves rows open, `lost` after the
  deadline plus 10 minutes. The receipt reads D5's existing `Canyon couldn't check whether <version>
  runs in preview.` when `job_runs` cannot be read.
- **Hand-off** (`run_by: org`) refuses a package with jobs at deploy:
  `Your platform runs <app>: jobs aren't handed off yet.` (proposed). Setting `run_by: org` leaves
  jobs Canyon armed running until the next deploy, which that refusal stops.
- **A process the build lacks** is refused before anything is armed: `The build has no process "<p>":
  it has <list>. A Procfile at the app's root names them.` (proposed).

**Scale.** `CANYON_JOB_STARTS` bounds how many runs start at once and the executor's cap how many
run; DBOS's jitter (≤ 10 s) spreads a fleet of `0 6 * * *`; `--log-opt` bounds logs. On Docker each
app takes one /24 of `CANYON_APPS_POOL`: a /16 holds **at most 256 apps per host**, a ceiling
ARCHITECTURE.md states (§13).

## 10. Testing

Every test is shown failing before it passes (rule 2).

- **Unit, red first.**
  - `ParseStandard` and DBOS's seconds parser give the same `Next` times over a year. Europe/Berlin
    2026-03-29: a `30 2 * * *` job does not run that day. 2026-10-25: it runs once.
  - The shortest-gap computation, and every refusal in §2's table.
  - A backfilled tick's id equals the live tick's, so a backfill after a live fire enqueues nothing.
  - `ApplySchedules` on an existing name keeps `last_fired_at` and the id.
  - The run-id derivation.
- **Scheduler conformance with a fake clock** (`runs/`, an injected clock against a fake executor).
  §4's table drives every case: missed by 30 minutes runs once, by 2 hours writes `missed`; a spec
  change catches up none of the old spec's ticks; a start during a run is `skipped: running`; Busy
  waits, then `failed to start`; a replayed tick step answers the existing run; two replicas
  backfilling one window start one run; a deploy-row read error retries and deletes nothing; the boot
  reconcile restores a deleted schedule.
- **Executor conformance, one suite against both executors** (the real daemon in the executor's e2e
  test; kind in `kube.yml`): `POST /job` twice with one run id starts one container; exit 3 is
  `failed/3`; a `sleep 600` with a 5-second timeout is `timed_out` measured from container start (on
  kind the cold pull does not count); a past-deadline run is stopped at an executor restart (Docker);
  the cap answers `busy`; no `PORT`, `CANYON_ENV` set; the address is `.14`/`.15` and an exited
  container frees it (Docker); 5-run and 24-hour retention; `dropNetwork` waits on a running job; on
  kind, a job pod admits no connection from a router pod.
- **Docker E2E** (`make e2e`):
  - **Fixtures.** `gate/testdata/apps/renewal-reminder/`, a web app with a job that reads its own
    Postgres. `gate/testdata/apps/nightly-export/`, with `web = false`.
  - **The test schedules the job** for the next minute at least 3 minutes ahead, rewriting the
    fixture's schedule.
  - **The journey:** deploy; `canyon run remind` in preview, dry, exit 0; promote, asserting the
    receipt's evidence run and the audit row; the scheduled production run's `job_runs` row and log;
    the server container stopped across a tick and started again, then exactly one run with the
    tick's id; undeploy, and the schedule is gone.
- **AWS EKS test cluster** (`e2e/cluster`, CASES.md gains J-JOBS). These cases are start-early, check-late:
  `TestSuite` deploys and promotes the job fixture **first**, with a schedule due near the suite's end,
  and asserts its run in the last step. They assert objects, not time: preview has no armed schedule;
  the Job's pod has the forwarder-only sidecar (once egress ships), no `PORT`, `CANYON_ENV=prod`, and
  its Secret owned by the Job; `kubectl wait --for=condition=complete job/canyon-job-<hex>` (read-only
  kubeconfig: wait is a watch); a deadline fixture job `kubectl wait --for=condition=failed`, reason
  `DeadlineExceeded`, its `started_at` to `ended_at` at least its timeout; `job_runs` matches
  `GET /jobs`.

  Catch-up across an outage is proven on Docker, where the suite may stop the server. The test cluster's
  kubeconfig cannot scale it.

## 11. Deliberately left out

- **Workers.** A long-running process with no port and no address is out. The trigger to design it is
  the first app that needs one: a socket-mode Slack bot, or a queue consumer once Canyon offers a queue
  resource. It needs stop-then-start rollouts, because two copies must never drain at once.
- **Gate exemptions.** `auth.unauthenticated-route` and every other rule stay judged on every package.
  No `PORT` is set for jobs.
- **Hand-off of jobs.** Later, a `last_run` report field would let the org's platform give evidence.
- **Retries and catch-up policy.** No retries, no concurrency other than forbid, no per-job catch-up
  window, no schedules more often than every 5 minutes.
- **A gate warning for in-process schedulers** (node-cron, APScheduler). It would read intent from a
  dependency name. An org can write a custom rule for it.
- **Alerts** beyond the fleet cell and `job.finished`: no email, no webhook.
- **The fleet tile** for failed jobs.

## 12. Implementation plan

Each PR goes off `main`, adds at most ~700 lines, and is red first. Seams merge before their
consumers, and the executors before the server.

| # | PR | Red-first test | Size |
|---|---|---|---|
| 0 | `docs(permissions): job runs — preview contributor, production owner` | (doc) | ~40 |
| 1 | `test(runs): prove DBOS v1.3.0 schedules` | upsert keeps `last_fired_at`; backfill id = live id; queue routing; neither call works inside a workflow, and both work from the Client after commit; the parser equality and DST cases | ~300 |
| 2 | `docs(contracts): jobs — executor seam` (`/job`, `/jobs`, `/build` processes, `CANYON_ENV`, `keep`, labels exception, invariants 5 and 12, header, `executor.schema.json`, fixture) | `validate.py` red on a job fixture whose Job carries a `PORT` or lacks `backoffLimit: 0` | ~350 |
| 3 | `docs(contracts): jobs — routes, events, snapshot` (the routes, `job_runs` shapes, `job.finished`, `cron.preview-run`, receipt fingerprints) | `validate.py` red on a `job.finished` missing `state` | ~450 |
| 4 | `feat(projects): canyon.toml declares jobs` (declarations, CLI keys, migration, the deploy refusal) | each row of §2's table uploads `201` on main and `400` after; a package with jobs is refused at deploy | ~550 |
| 5 | `feat(executor): jobs on Docker` | Docker half of the executor conformance suite | ~700 |
| 6 | `feat(executor-kube): jobs on Kubernetes` | kind half of the same suite | ~700 |
| 7 | `feat(runs): the job scheduler` (arming, boot reconcile, ticks, sweep, `job_runs`, `job.finished`, hand-off refusal; removes the deploy refusal) | the fake-clock conformance suite | ~700 |
| 8 | `feat(api): start and read job runs; promotion evidence` (routes, project and status reads, fleet cell, `cron.preview-run`, approvals, audit, patching rule 2) | a contributor's prod start `403`; under `prod_approval` a device session's `403 dashboard_required`; promote with no run warns and goes on | ~650 |
| 9 | `feat(cli): canyon run; jobs in deploy, status, logs, fleet, ai, skill` | golden transcripts of §3; `canyon run` exits 1 on a run exiting 1 | ~650 |
| 10 | `feat(dashboard): the Jobs card, Run now, the fleet cell` | `Run now` absent for a viewer, owner-only in prod, disabled while running | ~550 |
| 11 | `test(e2e): jobs on Docker and the EKS test cluster` (fixtures, journeys, CASES.md J-JOBS) | §10's E2E cases | ~600 |

That is about 6,200 lines. The order is 0 → 1 → 2 → 3 → 4, then 5 ∥ 6, then 7, then 8, then 9 ∥ 10,
then 11. The forwarder-only sidecar (§6) lands with EGRESS PR 24. PATCHING's §5.5 change lands with
PR 8.

## 13. Edits other docs need (listed, not made)

- **PATCHING.md** — §5.4 and §5.5 point to §8's four rules. `keep` names every image a deploy row
  names.
- **EXPERIENCE.md** — D2 gains `cron.preview-run`. D7: eighteen commands with `run`, and cli.md §1
  says *Sixteen*, so reconcile the two. D5's `Private:` row: written on an app with a web process.
  D11's *cron app renders no preview link* is now a jobs-only app's rule. D4: renewal-reminder becomes
  claims-portal's job when canyonv2-blg reconciles the roster.
- **design/specs/cli.md** — §1 the `run` row and *`canyon.toml`* (`[jobs]`, `web`); §3, §5.3, §6.1,
  §6.2, §6.4 per §3; §7.2 the skill's two rules.
- **design/specs/screens.md** — the Jobs card, Run now, the fleet cell.
- **PERMISSIONS.md** — PR 0.
- **ARCHITECTURE.md** — §1: the server schedules jobs. §5: a deploy arms jobs. The per-host app
  ceiling (256 on Docker).
- **HAND-OFF.md** — jobs refused under `org` in v1.
- **e2e/cluster/CASES.md** — J-JOBS (PR 11).
