# Operations — putting a version back, a checkout's host, and stopping any app

**Designed; rollback's server side, CLI and dashboard are built** (#2057, #2058, #2059, #2060)**, and the responder, the hold's server side and its CLI** (#2061–#2064, #2067). Three things an org needs in its first week on Canyon. Production needs the
version before this one (§1). A builder signed in to two Canyons sends one app to the wrong one (§2).
A security team needs to take any app offline without being able to ship one (§3). Each is the
smallest change that keeps today's invariants: every ship passes the gate, the receipt and the
production approval decide production, and the trail names who did what.

Strings marked *proposed* are new product surface and join `design/EXPERIENCE.md` D5 in the PR that
first renders them. Every D5 string quoted here is byte for byte. Code is cited by file and symbol;
migrations are "the next migration", never a number.

## 0. Words, and what goes straight to production

| Word | Means here | Not |
|---|---|---|
| **version** | a package, named by its label, else its id without `pkg_` (routes.md, *Naming a version*) | a git tag |
| **image** | one build's bytes, named by digest `sha256:…` | a package: one package can have several images (a rebuild, a patch) |
| **rollback** | a *person* puts an earlier version back in production (§1) | Canyon's own **revert** of its patch (design/PATCHING.md) |
| **earlier** | the last five versions production ran before the current one (the status read's `earlier[]`) | every package |
| **kept image** | an image the executor's tag cleanup is told to spare (`keep`, §1.5) | an image that happens to still exist |
| **undeploy** | the team takes an environment offline; anyone on the team can put it back | a hold |
| **hold** | production stopped by a responder or an admin, refusing every production write until lifted (§3.4) | the CLI's stdout buffering, renamed `park()` (§3.8) |
| **brake** | taking production offline at once, whatever else the app's runs are doing (§3.3) | rollback, which is the repair |
| **release** | hand-off's instruction to the org's platform, `rel_…` (design/HAND-OFF.md §3) | a version |

**Decision record: what goes straight to production.** Production changes in only two ways:

1. **A person's promotion**, through the receipt and, when the org turns it on, a production
   approval: preview's image (today), an image production ran before (§1, rollback), and the held
   image a responder or admin starts again on lifting a hold (§3.6). That last one is the same bytes
   already promoted, started by a person in the dashboard. A patch's way to production is a person's
   promotion too: its patch approval, decided in the dashboard by the app's team, or by an admin's
   fleet approval of its platform identity (design/PATCHING.md §4.5; design/PERMISSIONS.md, *Platform
   patches*; added 2026-10-18, #2086), whose promotion Canyon starts with that admin as its actor.
2. **Canyon's revert of its own patch**: production goes back to the image Canyon's patch replaced.
   PATCHING.md defines it; it is called "revert", never "rollback".

Any other path into production is a change to this record first.

## 1. Rollback — putting an earlier version back

### 1.1 How it works

1. `canyon status` lists production's earlier versions and ends with the exact command:
   `canyon deploy --env prod --version v1.3`.
2. The CLI resolves `v1.3` through the status read's `earlier[]` to a package id and the image digest
   production ran. The bare id `3f9e1c2a` resolves the same way. Anything not in the list is refused
   before a request is sent.
3. The CLI asks for **a review of that package under today's policy**:
   `POST /gate-runs {package, rollback_to: "<digest>"}`. Preview is untouched, and the run counts only
   against the hourly limit of rollback reviews, never an ordinary review's (amended 2026-10-11).
4. The run reviews. If the review passes, or every blocking finding is approved, the run records the
   image to ship: the **kept** digest the executor was told to keep (§1.5). When that image is gone,
   the run ships nothing and ends `failed`, saying why; a rollback never rebuilds (decided
   2026-10-11, the owner).
5. The CLI reads the receipt for that run (`GET /status?run=<id>`). The receipt includes `ships
   {version, image_digest}`. The CLI prints what ships and what it replaces, and asks once.
6. `POST /promotions {gate_run}`. This is the existing route, the existing receipt checks and the
   existing production approval when the org has one. A promote run starts the recorded image in
   production with `keep: [<the image it replaces>]`, so the rollback can itself be undone, under the
   egress spec production's history recorded for that image — a recorded spec, or the host's current
   one where none was recorded, never via off by omission (design/EGRESS.md §2.5).
7. Live: the deploy row, a `deploy_history` row and the audit row `deploy prod`
   `{…, replaces, rollback: true, reason?}` are written in one transaction by the shared production
   writer (§3.4).

```
CLI                          Canyon server                                  executor
 │ GET status ──────────────▶│ earlier[] (deploy_history)                    │
 │ POST gate-runs            │                                               │
 │  {package, rollback_to} ─▶│ gate run under today's snapshot               │
 │                           │ ├─ blocked ─▶ run ends blocked; nothing ships │
 │                           │ ├─ kept ────▶ record digest                   │
 │                           │ └─ gone ────▶ run ends failed; nothing built  │
 │◀── run clean ─────────────│                                               │
 │ GET status?run= ─────────▶│ receipt: rules, held?, ships                  │
 │ [y/N]                     │                                               │
 │ POST promotions ─────────▶│ promote run ─ /run image, keep[old] ─────────▶│ swaps once up
 │◀── live ──────────────────│ deploys + deploy_history + audit, one tx      │
```

| Review run ends | Image recorded | What the person sees before confirming |
|---|---|---|
| `clean` / approved, digest kept | the digest production ran | `Production gets the image it ran on 2 Oct.` *(proposed)* |
| `clean` / approved, image gone | none — refused, never rebuilt | `v1.3's image is no longer on this Canyon, so it can't be put back as it ran. Deploy v1.3 to preview and promote it if you want it rebuilt.` *(proposed)*; exit 1; production unchanged |
| `blocked` | none | the verdict; exit 2; production unchanged |
| `failed` (build, `no_room`, …) | none | the reason in words (routes.md); production unchanged |

### 1.2 What is true today

Built (#2057, #2058): the server's half below; the CLI's `--version` (#2059, design/specs/cli.md
§6.3); the dashboard's *Earlier versions* (#2060, design/specs/screens.md).

- **Production has a history**: `deploy_history`, appended by `runs.WriteProdTx` alone — the promote
  run's and the hand-off report's one way into production — and backfilled with what production ran
  when it landed. The status read answers `earlier[]` from it.
- **Every `/run` keeps** what §1.5 lists — a patch's `/run` the image it replaces, which preview's row
  names until the patch is recorded, and while its watch runs, so a revert can put it back (PATCHING.md §4.4, #2100); under
  `run_by: org` Canyon calls no `/run`, so the org's images stay kept (§1.6).
- **A rollback review** is `POST /gate-runs {package, rollback_to, reason?}`: limited in a bucket of its
  own (§1.1, step 3), recording `ships` or ending `failed`/`image_gone`; a promotion of it ships `ships`
  (contracts/routes.md, the gate-runs and promotions rows).
- **Before it**, production only took preview's image, its history was only `deploy.completed` rows, an
  image production ran was gone after the next `/run`'s tag cleanup, and 30 gate runs per project per
  hour held a rollback back during an incident.
- **Most versions have no label** — the CLI sends none — so a version shows as its id without `pkg_`,
  and `routes_gate.go` `packageNamed` matches an id or a label: `3f9e1c2a` from the status read is a `404`.

### 1.3 Why this shape

- **No preview leg.** Preview has its own resources and database (contracts/provisioner.md), so it is
  no smoke test of production's migrated data; and it is contended — the team's fix, GitHub pushes, a
  patch — which a chained rollback would race and overwrite. `/run` already keeps the previous
  production container until the new one answers on 3000 (contracts/executor.md, `/run`).
- **The exact bytes.** A rebuild against today's dependency resolution is a different program, and in
  a supply-chain incident the wrong one; when the digest is gone the rollback refuses before anything
  is asked, and the ordinary deploy-and-promote path is the way to a rebuilt version.
- **Today's review, no override.** "Every ship passes the gate" has no exception for code that shipped
  before: a version blocked today has the one way through — an admin or a security responder who did not
  start the review approves each blocking finding with a reason (routes.md, the approve rows, one dependency's findings together).
  An approval within 10 minutes of its expiry no longer clears the rollback's promotion, which judges it
  again as it starts production's container (`runs.ShipMargin`, design/PERMISSIONS.md); the page still
  shows it until it expires. The review cache (D9) makes it fast when nothing changed.
- **One run at a time still holds** (`runs/start.go` `StartTx`, `runs_one_active_per_project`): a
  rollback asked while another run is `running` is `409 run_in_progress`, naming it. The brake (§3.3)
  is never refused that way.

### 1.4 Which versions: the status read's `earlier[]`

- **Source**: a new table `deploy_history (project_id, package_id, image, gate_run, run, deployed_at,
  promoted_by_*, patched)`, index `(project_id, deployed_at DESC)`. Exactly one function appends to
  it: the shared production writer (§3.4). That writer is used by the promote run, the hand-off
  report and the lift's start. The table was chosen over `audit_events`, whose `detail` is untyped
  and is written by every path in its own shape.
- **The read**: the existing status read gains `earlier[]`, on the read itself so it is there after an undeploy too: `{package, version,
  image_digest, deployed_at, promoted_by, patched?, kept}`. Rows are collapsed by `(package, image)`
  and taken newest first. The read excludes what runs now and stops after five. A patched row (an
  image Canyon's patch produced, PATCHING.md) is marked `patched: true` and reads `v1.4 patched
  (4 Oct)`, so the same version twice never reads `v1.4 · v1.4`. The mark is the image's: a rollback
  to an image a patch made is marked patched too, so the mark never flips back once production moves on
  (amended 2026-10-28, #2390). The minimum level is
  **contributor**, as for the rest of the status read.
- **Retention**: the 30-day package sweep (design/ARCHITECTURE.md, *Canyon never ingests history*)
  exempts the packages of `earlier[]` and the current one, and nothing else. A version older than the
  five cannot be named.

### 1.5 The kept image — the `keep` seam

`POST /run` gains `keep: ["sha256:…"]`, digests of this app's images, at most 16. The tag cleanup
after the `/run` removes none of them, on Docker or in the registry. `/forget` takes no `keep`: a
deleted app keeps nothing. contracts/executor.md (*What `keep` spares*) owns its shape; **this doc owns
what the server keeps.** design/PATCHING.md and design/JOBS.md send their images through it and do
not redefine it.

**What the server keeps** is server policy, not seam. Every `/run` of an app sends, built as a set (one
image in two roles is sent once):

- production's previous image, the one the newest `deploy_history` row replaced;
- each image a deploy row of the app names, preview's and production's — a held production's image
  (§3.4) among them, and a jobs-only app's armed image, which no container runs (JOBS.md §5, §8);
- a patch's pre-patch digest while its watch runs, of preview or of production (PATCHING.md §4.4, §4.5);
- the digest a pending production patch approval names, until it is decided, expires or turns
  outdated (PATCHING.md §4.5);
- a rollback review's recorded `ships` image while the run waits for its promotion (§1.1, step 4): the
  newest such review's, until a promotion of the app starts.

`earlier[].kept` is true only for a digest in that set on every `/run` since it stopped running; images from before this seam are never
claimed kept, but for what production ran as it landed (the history's first row). A "kept" image whose `/run` still answers `404` fails the promote run — last line
*(proposed)* `v1.3's kept image is gone, so it can't be put back as it ran.` —
`deploy_history` marks it gone, and production does not change. The next rollback to it is reviewed
as §1.1 step 4 says and ends `failed`/`image_gone`, nothing built, before anything is asked
(design/specs/cli.md §6.3.3); the dashboard's *Put back* is disabled up front on a row whose image is
not `kept` (§1.7). (Corrected 2026-10-12, #2207: this said the CLI's rollback refuses at once.)

### 1.6 "Production is approved in the dashboard", hand-off, and what is not put back

- **The setting covers rollback**, which is a promotion: the CLI files a production approval naming
  the review run, its package and image, and prints the link (design/AUTH.md §4.5); it is `outdated`
  once production no longer runs the image it replaces (PATCHING.md's rule for patch approvals). The
  emergency act is the brake, which the setting has never covered.
- **Hand-off**: the same review; the promotion sends a `deploy` release for `prod` naming the recorded
  image (HAND-OFF §3–4). Under `org` no tag cleanup runs, so the image is in the org's registry and
  `keep` does not apply.
- **Not put back**: *Canyon puts back the code: not the data, the secrets or the outbound rules.* The
  app migrates its own schema at start, so v1.3 starts against the database as v1.4 left it, with
  today's production secrets and today's egress routes (design/EGRESS.md, routes keyed by app,
  environment and destination). The confirm says so (*proposed*): `Canyon puts back the code, not the
  data, the secrets or the outbound rules: v1.3 runs against production as v1.4 left it.` Its receipt and
  `canyon status` list what production refuses today of the destinations a person dismissed; the go-live
  rule on outbound reads no preview on a rollback, so it never holds one (design/EGRESS.md §2.5, §3.4).

### 1.7 The experience

```
$ canyon deploy --env prod --version v1.3 --reason "INC-2231: v1.4 corrupts exports"

  canyon 0.6.0 · claims-portal v1.3 → prod · canyon.meridian.example

  Reviewing v1.3 again under today's rules. Preview is not touched.                  (proposed)

  ✓ Review passed · 0 blocking, 1 warning

  Putting v1.3 back in production. It replaces v1.4.                                  (proposed)
  Production gets the image it ran on 2 Oct.                                          (proposed)
  Canyon puts back the code, not the data, the secrets or the outbound rules: v1.3 runs against production as v1.4 left it.

  Put claims-portal v1.3 back in production, replacing v1.4? [y/N] y

  Promote
  │ deploying to production
  │ live at https://claims-portal.prod.apps.meridian.example

  ✓ Promote    prod
```

- **Blocked today**: the existing verdict lines, `Nothing was built and nothing was deployed.` (D5),
  then *(proposed)* `v1.3 went to production on 2 Oct; today's review blocks it. A person on the team
  can approve each blocking finding with a reason in Canyon's dashboard; then run this again.` Exit
  `2`. **Not in `earlier[]`** *(proposed)*: `v1.1 isn't one of production's last five versions: canyon
  status lists them.` Exit `1`, nothing sent.
- **Command names**: `canyon rollback …` and `canyon revert …` are usage lines like today's `promote`
  redirect (`cli/root.go` `app.main`): `canyon rollback is: canyon deploy --env prod --version
  <version>. canyon status lists them.` No new command; D7's count is unchanged. **The global
  `--version`**: `canyon --version` before a verb still prints the CLI's version; `canyon deploy
  --version` with no value, or without `--env prod`, prints *(proposed)* `--version puts an earlier
  version back in production: canyon deploy --env prod --version <version>` and exits `1`.
- **`--format ai`** (blocked), after the existing `CANYON REVIEW — RESULT: BLOCKED` block
  *(proposed)*:
  ```
  rollback: v1.3 → prod; production still runs v1.4
  NEXT STEPS:
  1. Tell the user today's review blocks v1.3: each blocking line above, in its own words.
  2. Do not change v1.3's code to get it through: a fix is a new version, shipped with canyon deploy.
  3. An admin or a security responder who didn't start this review may approve each blocking finding in Canyon's dashboard; neither you nor the user can. Then run this command again.
  ```
- **`canyon status`** *(proposed)*: `Earlier in production  v1.3 (2 Oct) · v1.4 patched (4 Oct) ·
  v1.2 (28 Sep)`, then `Put one back: canyon deploy --env prod --version v1.3`. Under `ai`, the line is
  `earlier in production: v1.3, v1.4 patched, v1.2` plus that command (design/specs/cli.md §6.1.2).
- **The skill gains one line**: *Put an earlier version back only when the user asks: run the
  command `canyon status` prints under Earlier in production.*
- **Dashboard**: the production card gains *Earlier versions*, the five rows each with *Put back*,
  shown to contributors, owners and admins; on a row whose image is not `kept`, *Put back* is disabled
  with the image-gone line as its reason, and no review is asked for. *Put back* starts the review and shows the run. When the
  run passes, the card offers `Promote v1.3` with the confirm's lines; an image that is gone shows the refusal
  line and no Promote. A browser session promotes directly, as today.

### 1.8 Contracts, routes, audit

- **executor.md** (PR 1): `/run` `keep[]`. Its fixture is in `contracts/testdata/executor/`.
- **routes.md** (PR 6): the gate-runs row gains `rollback_to` — a digest of that package in
  `earlier[]`, else `409 {error:"not_earlier"}`; such a run counts in a limit of rollback reviews
  of its own and records `ships`. The status read gains `?run=` (the receipt for that run),
  `earlier[]` and the receipt's `ships`. The promotions row ships a run's `ships` instead of
  preview's image — `409 nothing_to_ship` when it recorded none, `409 outdated` once production no
  longer runs what the review started against — its `prod.clean-gate` reading that run with no preview
  check, only while it is its package's newest review and until it has shipped, and `waiting` not
  evaluated; its approval's `outdated` rule. `deploy prod` detail gains `replaces`, `rollback` and
  `reason?` (1–1024 characters, an incident or change reference); the trail reads *(proposed)* `put
  <version> back in production, replacing <version>`, or `… back in production` when nothing ran there.
- **Schemas**: `app-page.schema.json` (`earlier`, `ships`) and the matching fixtures.
- **Minimum level for every act**: contributor, as deploy and promote. Read-only and no-access members
  get `403 team_required` and see no *Earlier versions* card.

### 1.9 Testing

- **Unit (red first)**: `TestEarlierCollapsesPatchedAndRepeated` (v1.3, v1.4, v1.4-patched, v1.3 →
  three rows, the patched one marked) · `TestEarlierUsesItsIndex` (`EXPLAIN`: an index scan of
  `deploy_history`) · `TestRollbackShipsTheDigestProductionRan` · `TestRollbackRefusesWhenTheImageIsGone` (no `/build` call)
  · `TestRollbackReviewIsNotRateLimited` (30 gate runs, then a rollback review answers 202) ·
  `TestRollbackBlockedTodayShipsNothing` · `TestCollectSparesKept` (Docker and Kubernetes fakes) · CLI
  `TestVersionRoundTripUnlabelled` (status → `--version 3f9e1c2a` posts `pkg_3f9e1c2a`) · `TestRollbackRedirect`.
- **Docker E2E** (`make e2e-up`, `e2e/deploy_test.go`, journey J12): `f-core` v1, then v2 adding an
  advisory `code.injection` finding at high (an `eval` of a request's query), each promoted; `--version v1` reuses the kept digest (equal to the
  history row's); an admin turns `code.injection` blocking → the rollback to v2 exits 2, production's digest
  unchanged; promote v2 three times so v1 is not kept, and the rollback refuses with the image-gone line, no
  build holder created; `replaces` on the audit row; wall-clock printed with and without a kept image
  (amended 2026-11-09, #2083: v2 promoted three times in place of a v3 promoted twice, as a /run keeps the image the newest history row replaced, and no migration, which no step read).
- **EKS test cluster** (`e2e/cluster/CASES.md`, new **R** cases, phase 2 after M7): **R1** kept image — the
  `<app>-prod` Deployment's digest equals the history digest (the read-only kubeconfig reads the
  Deployment, never ECR) · **R2** an image no longer kept: the rollback refuses, no kpack Build is created · **R3** under `prod_approval`
  the CLI prints the link and Playwright approves · **R4** (wave 2, `run_by: org`) the release names
  the recorded digest.

## 2. A checkout's host

### 2.1 How it works

Two guards need no new state, and per-host sessions remove the reason to switch.

1. **(a) The app must already be on that host.** A verb whose `canyon.toml` names an app the host
   does not have refuses, instead of creating it. `--create` creates it on purpose. The one exception
   is the setup flow — a directory with no `canyon.toml`, where `canyon deploy` writes the file — which
   creates as today.
2. **(b) The checkout's base must belong to that host.** When `.canyon/base` names a package, the CLI
   asks the host for it (`GET …/packages/:pkg`) before any verb that changes the app. A package id is
   its tree, so a host this tree was never sent to has no such id: a `404 unknown_package` means this
   checkout was last sent to another Canyon, or this one no longer has the package: refuse, and name
   the hosts this machine is signed in to.
3. **Sessions per host.** `canyon login` adds a host and does not forget the others, so moving between
   staging and production never logs anyone out, and `--host` carries that host's own session.

```
canyon deploy (canyon.toml: claims-portal, .canyon/base: pkg_9e40)
  │ GET /api/v1/projects/claims-portal ──▶ 404 ──▶ ✗ refuse (a)      [--create to make it]
  │ GET …/packages/pkg_9e40 ──────────────▶ 404 unknown_package ──▶ ✗ refuse (b), naming hosts
  │ upload …                                  (today's path from here)
```

### 2.2 What is true today

- **One host and one session per machine.** `cli/config.go` `config` stores `{host, public, session}`.
  `cli/cmd_login.go` `runLogin` rewrites the file whole, so signing in to staging signs production's
  session out. `cli/root.go` `app.client` offers the stored session to the stored host only.
- **Deploy and check create the app on whatever host answers** (`cli/cmd_check.go` `ensureProject`,
  called from `prologue`). A production checkout run after `canyon login https://canyon-staging…`
  creates the app on staging and uploads its code there.
- **The base is a hint.** With no current package the upload accepts any base (#1678, a reinstalled
  host). With one, a base that does not match is `409 stale_base` "pull, or force"
  (`routes_packages.go` `uploadPackage`). An agent forces it. Amended 2026-10-11 (#2078): the base is
  now also asked of the host before any verb that changes the app, and one the host never had
  refuses (§2.3, (b)).

### 2.3 The guards, precisely

- **(a)**: `ensureProject` first reads `GET /api/v1/projects/:app` — `secret set` too, before it
  asks for the value, and `undeploy` before production's question, so a value never reaches a host
  without the app. A `200` must be that app's row. A `404` refuses with exit 1 and nothing sent
  *(proposed)*; `secret list`, `status`, `share` (no `--app`) and `delete` refuse the same from their
  own read's `404` (`delete --confirm` from the delete's, nothing deleted). `logs`, `pull` and `share --remove`, whose route's `404` can be something else,
  read the app on that `404`, and refuse with exit 1 and nothing changed only when that read's own
  answer is `404`; any other answer keeps the verb's own line (#2195). The refusal:
  `claims-portal isn't on https://canyon-staging.meridian.example. If it should be, run: canyon deploy
  --create`. Under `ai` the refusal's block says to run `canyon deploy --create` only when the user
  says this app is new on this Canyon. `--create` is `deploy`'s alone; with `--env prod` it is
  refused, and prod's refusal names no `--create`. The full transcripts are cli.md §3.2.
- **(b)**: run before `check`, `deploy` (all forms), `undeploy`, `delete`, `secret set|rm` and a
  `share` grant whenever `.canyon/base` names a package. It uses the new read
  `GET /api/v1/projects/:id/packages/:pkg` → `200 {package}` (the list's row) · `404
  {error:"unknown_package"}`, at minimum **contributor**. Only that `404` refuses:
  `pkg_9e40 isn't on https://canyon-staging.meridian.example. This checkout was last sent or pulled with
  it, so it went to another Canyon, or this one no longer has it.`, then one `canyon <verb path>
  --host <host>` line for each other host this machine holds a session for, sorted, and `rm
  .canyon/base` for this one. Any other `404` (a host older than the route) skips (b), as today.
  Under `ai` the hosts are listed with no runnable `--host`, and the steps say to ask the user which
  Canyon this app belongs on. The full transcripts are cli.md §6.6.
- **The reinstalled host** still works. Its project is gone, so (a) asks for `--create`, and with
  `--create` an unknown base is accepted, as #1678 accepts it.
- **Why no pin file.** (a) and (b) cover every upload, and every app-changing verb run from a checkout
  that has a base. The hole they leave is a checkout with no base (a fresh clone, a git worktree, CI)
  of an app that exists on both hosts. An upload from it meets `stale_base` today; a `secret set` from
  it reaches the wrong host. A package id is its tree's hash cut to the shortest prefix the app's
  repository doesn't already hold, so (b) also passes the same tree sent to both hosts, on both, and
  a base whose id another tree here happens to share. A pin file would not close that hole: a fresh checkout has no pin either.
  The trust line `.env and node_modules excluded → <host>` (D5) names the host of every upload, and
  every verb that changes an app or its secrets names it on its first line (decided 2026-10-11, the owner; built in #2154;
  cli.md §3.1). The routes past the guards stay open on purpose and are listed: deleting `.canyon/base`, deleting the `app` line (the setup flow makes a new app),
  `--app <id>` with no `app` line, and `--create` added unasked; `secret rm`, `share` and a preview
  `undeploy` ask nothing, and `--yes` answers production's question (cli.md §3.2, §6.6).

### 2.4 Sessions per host

**`config.json` stays byte-identical** and authoritative for the host it names: older CLIs read and
write it as always, so its session is the current one for that host. Every *other* host gets
`~/.config/canyon/hosts/<scheme>_<host>[_<port>].json` = `{host, public, session}` (the host part
`[a-z0-9.-]`, e.g. `https_canyon.meridian.example.json`; a host outside it, or a name over 200 bytes,
gets no file), 0600 in a 0700 directory; the file's `host` is authoritative and a name that doesn't
re-derive from it is refused. Older CLIs never touch `hosts/`, so they cannot clobber another host's
session. `login <B>` with `config.json` naming A moves A to `hosts/` and writes B to `config.json`,
so B is the default, as login has always made it, and an older CLI meets B with B's session. A
`hosts/` file for `config.json`'s host is stale and ignored, and any `login` that signs in, or
`logout`, revokes and removes it. A rotated session is written to whichever file holds the host. `canyon logout` signs
out of the default host as today, ignoring `CANYON_HOST`; `canyon logout <url>` out of that one,
removing a `hosts/` file even when its host can't be reached, and saying so; each distinct session
removed is revoked; `--host` stays refused (`cli/cmd_logout.go` `runLogout`). **One canonical form**
for file names and every comparison, **from the URL alone**, never from what a host says about
itself: parsed as `net/url` parses it, scheme and host lower-cased, the default port and a trailing
`/` dropped, a path, query, fragment or username refused; `config.json`'s host is canonicalised on
read. A host is offered a session only when that form is the file's `host`, and no redirect is
followed; `public` only names the cookie, as today. A VPN name and a public name are two hosts,
each signed in to on its own, and no request is needed to pick a session. `--host` and
`CANYON_HOST` now carry the named host's session for every verb but `login`; guards (a) and (b) are
what stand between them and a wrong-host write. cli.md §8.2 and §8.4 are the detail.

### 2.5 Contracts and testing

- **Contracts**: routes.md gains `GET /api/v1/projects/:id/packages/:pkg`. cli.md changes are listed
  in §8.
- **Unit (red first)**: `TestDeployRefusesAnAppTheHostLacks` (the stub saw no `POST /projects`) ·
  `TestCreateMakesIt` · `TestUnknownBaseRefusesNamingBothHosts` (no upload seen) ·
  `TestSecretSetChecksTheBase` · `TestLoginToASecondHostKeepsTheFirst` (`--host A` carries A's
  session after logging in to B) · `TestConfigJSONIsByteIdentical` · `TestCanonicalHost` (trailing
  `/`, `:443`, case, and a file name that re-derives) · `TestPublicNeverPicksASession` (a host
  answering another's `public` is offered none).
- **Docker E2E** (J14): `compose.e2e.yml` adds a second server, `canyon-b`, sign-in off. `f-core`
  deployed to A, then with `CANYON_HOST=B`: `deploy` refuses under (a) and B's audit shows no
  row; once B has an app of that name, (b) refuses and B holds no package. On the team setup
  (`e2e/oidc`), where A's session is real: logged in to A, then B, an old release's CLI logs in to B
  while the new one holds A in `hosts/` — A's session survives byte for byte and still signs the new
  CLI in to A, and `config.json` round-trips (amended 2026-11-09, #2083: a host with sign-in off keeps
  no session, so the two hosts' parts are swapped).
- **EKS test cluster** (**T1**): a checkout sent to the EKS install, then pointed at a Docker `canyon-b` on
  the runner, is refused under (b).

## 3. The responder, and the brake

### 3.1 How it works

1. **Assignment.** An admin lists responders on Settings › People & sign-in by address, or the
   identity provider's `responder_claim`/`responder_value` names them, as it names admins.
2. **What a responder reads.** The fleet whole: every app's fleet row — name, owners, summary,
   production's address and state, and its hold — and, of any app, what approving a blocking finding
   needs: its runs, a run's findings and SARIF — snippets included, a line or a few of the code each —
   and its status read trimmed to the receipt, its runs, production's address and state, and the hold
   (amended 2026-10-13, the owner; #2204; design/PERMISSIONS.md wins). Nothing else of any app: no logs,
   preview, earlier versions or who deployed.
3. **Stopping.** A responder stops an app's production with a reason, from the CLI (`canyon undeploy
   --app <app> --env prod --hold --reason "…"`) or from the fleet's *Stop production* (in a team row's side pane, or on a responder's reduced row, its phone card too; otherwise on a phone, from the app page). In one
   transaction under the ledger lock, the hold is written; every pending or running run that would
   write production is cancelled, PATCHING's watch included; open production approvals turn
   `outdated`. Then the container stops (Canyon) or a `remove` release goes out (org).
4. **While held**, every production write is refused structurally: `runs.ErrHeld` from `StartTx`, the
   check in `cancelled(…, deploy=true)`, and the shared production writer. The receipt carries `held`,
   so the CLI, status and the disabled Promote button say why with no per-route code. Preview,
   checks and secrets go on: the team ships its fix to preview and rotates what leaked.
5. **Lifting.** A responder or an admin lifts the hold, in the dashboard only. They choose *Lift the hold, keep it
   stopped* or *Lift the hold and start v1.4*: a `/run` of the held image's exact bytes, which were
   kept while held.

```
responder CLI           Canyon server                                  executor / org
 │ POST envs/prod/hold ─▶│ tx: hold row · cancel prod-writing runs ·     │
 │  {reason}             │     approvals → outdated · audit when done    │
 │                       │ promote past deploy_started_at, unrecorded?   │
 │                       │  ├─ yes: stop recorded pending; the run stops │
 │                       │  │       its container (retried step) ───────▶│ /stop
 │◀── 202 {stopping} ────│  └─ no:  POST /stop ─────────────────────────▶│ (or remove release)
 │◀── 204 ───────────────│ deploy row gone; audit `stop app`             │
 ⋮                       ⋮ every prod writer: ErrHeld                    ⋮
 dashboard: Lift ───────▶│ hold gone; optional promote run of held digest▶│ /run exact bytes
```

| Production | Writes allowed | Leaves it |
|---|---|---|
| running | promotion, patch, undeploy, hold | undeploy → empty; hold → *stopping* or *held* |
| **stopping** (a promote was mid-switch) | none | the run stops its container → *held* |
| **held** | none (`409 held`, `ErrHeld`) | lift → empty, or lift + start → running |
| **held · removing** (`run_by: org`) | none | the org reports `removed` → *held* |

### 3.2 What is true today

- **Org roles** were `admin` and `member` (now `admin`, `responder` and `member`, §3.5), per request (`cmd/canyon-server/session.go` `app.role`);
  admins are `CANYON_ADMINS` ∪ the row's `admin_emails` ∪ the admin claim (`sessions.claimed_admin`).
- **Undeploy was no brake** until the brake (§3.3, #2053): `409 run_in_progress` while *any* run of the app was pending or running
  (`routes_envs.go` `runInProgress`) — a preview deploy, an agent's check, a push; `403
  person_required` for a non-person while `prod.human-approval` blocks (`personRequired`); and any
  contributor's next promotion undoes it.
- **A cancel is too late once a run's deploy has begun** (`routes_cancel.go` `cancelRun`;
  `Pipeline.cancelled` sets `deploy_started_at` under the ledger lock).
- **`kubectl rollout undo` brought back a previous ReplicaSet** until #2051: `executor/kube/objects.go`
  `deployment` set no `RevisionHistoryLimit`.

### 3.3 The brake — undeploy conflicts only with runs that write the same environment

A routes seam PR, second in the plan, and before the responder:

| Taking offline | Conflicts with | What happens to that run |
|---|---|---|
| `prod` | a promote run, a production patch run, PATCHING's production watch | pending → ended `interrupted`; running before `deploy_started_at` → cancel asked; past it but no `deploy:prod` row yet → the stop is recorded **pending** on the run (`stop_requested_at`, `_by`), and the run, under the ledger lock in its deploy-row transaction, writes no `deploy.completed` row, no `deploy prod` audit row and no `live at` line — its env row says what runs there until the stop lands, so a stop that fails leaves no container unrecorded — and stops the container as a retried DBOS step |
| `preview` | a deploy run, a preview patch run and its watch | the same, on preview |
| either | check runs, the other environment's runs | never block, never cancelled |

Answers: `204` stopped · `202 {stopping: {run}}` when the stop is pending on a run, the CLI saying
*(proposed)* `Production is stopping: a promotion was already switching it, and Canyon stops it the
moment it lands.` · `202 {release}` under `org`. The audit row (`undeploy`, or `stop app` for a hold)
is written when the container has actually stopped — for a pending stop by the run, naming the
requester. **`person_required` is unchanged and stated**: a non-person actor taking production down
while `prod.human-approval` blocks is refused; a hold is a person's act whatever the policy.

The seam (contracts/routes.md, *The brake*; contracts/events.md, *the brake's lines*) fixes the rest:

- **Order.** The undeploy lock is held throughout and the ledger lock never across an executor
  call: (1) under the ledger lock the run is ended, asked to end, or given its pending stop, with a
  `request undeploy {env, run}` audit row, and committed; (2) `/stop`, or the `remove`; (3) under the
  ledger lock the row is deleted and the audit row `{env, package, run?}` written. A `500` after (1)
  keeps what (1) committed; a retry resumes.
- **A pending stop is honoured on every end of the run**: its deploy-row transaction, a failure, a
  replay, the reconcile at server start, and under `org` before its release is sent. A stop failing
  past its retries writes the env row truthfully and ends `failed`/`error`; the next undeploy stops
  it. Asking again while it is pending answers the same `202` and writes nothing.
- **No env row yet** (a first deploy or promotion): a pending or early run is ended or asked and the
  answer is `204`, the audit row's `package` `null`; past its deploy step, `202 {stopping}`.
- **Under `org`** the `remove` supersedes the open `deploy` release, and the run waiting on it ends
  `interrupted` at once; a late `deployed` on it gets another `remove`, as for a hold (§3.4). An org's
  first deploy, with an open release and no row, gets a `remove` naming that release.
- **A preview undeploy during a promotion** leaves it alone: it ships the image it read at its
  start, and production's row carries no build digest.
- **No read carries a pending stop**: the `202`, the run's timeline and the trail say it. An
  environment Canyon runs leaves `envs` (the project read, the status read, the fleet, the receipt's
  preview) once the run carrying the stop out has begun ending, or, with no run, while the
  undeploy's or the hold's own `/stop` is under way (#2202, #2262), never reading `stopped` while it is taken offline; one
  the org's platform runs stays, with its pending remove. Its row stays until the
  stop lands, a stop that fails shows it again once the run has ended, and a run stuck while ending
  keeps it left out until it ends or the reconcile ends it (#2193). A run the brake ended reads
  `interrupted`.
- **Patch runs and watches** joined the conflict sets in PATCHING's own seam PR (#2086;
  contracts/routes.md, *The brake* and *Patch runs*); its rule that an undeploy ends a watch clean
  stands.

### 3.4 Stop and hold

`POST /api/v1/projects/:id/envs/prod/hold {reason}` (1–1024 characters) — minimum **responder or
admin**, a person:

- **The hold row** (one per app, the next migration): `prod_holds (project_id, package_id, image,
  gate_run, reason, held_by_*, held_at)`. It records what ran, so the image is kept and can be started
  again. Production with nothing running still takes a hold, with no image.
- **Enforced in four places, and nowhere else per route:**
  1. **`runs.ErrHeld` from `runs/start.go` `StartTx`** for any run that writes production — a
     promotion, a rollback's promote run, a production patch — as `ErrUndeploying` is today. Every
     route that starts one answers `409 {error:"held", message, hold:{by, at, reason}, admins[]}`, and
     approving a production approval goes through the promotions route.
  2. **`Pipeline.cancelled(…, deploy=true)`** treats a held production as a cancel, under the ledger
     lock. A run that started before the hold stops before its deploy.
  3. **The shared production writer**, `runs.WriteProdTx` (new). Today's two writers,
     `Pipeline.deploy`'s production branch and the hand-off report `reportRelease`, both move to it.
     It checks the hold and a pending stop, upserts `deploys`, appends `deploy_history`, and writes
     `deploy.completed`. DBOS recovery replays through it, so a resumed run is checked too.
  4. **The receipt**: `evaluate` sets `held` and `blocked_by` (`prod.held`); `waiting` is not
     evaluated. The CLI, status, `--format ai` and the disabled Promote button (D6) all render that one
     field.
- **`DELETE /api/v1/projects/:id`** checks the hold itself (`409 held`): the app is evidence until the
  hold is lifted.
- **Hand-off**: the stop sends `remove` for production (`runs.SendRemoveTx`), superseding any open
  production `deploy` release. A `deployed` report arriving while held — late or raced — goes through
  the shared writer, is answered `409 {error:"held"}`, writes nothing, and Canyon sends another
  `remove`, so the org's platform takes down what it just started. The hold reads *being removed by your
  platform* until the org reports `removed`.
- **Kubernetes**: app Deployments get `revisionHistoryLimit: 0` (an executor change, PR 4), so no
  earlier ReplicaSet is left to `kubectl rollout undo` past the hold and the gate. A Deployment created
  before this executor keeps its old ReplicaSets until that target's next `/run`.
- **High availability**: the hold is a row read inside the transactions above; every replica enforces
  it, and none holds state of its own. During a rolling upgrade a replica still on the binary before
  the hold enforces none of it, matches a grant by its address or its subject, records an unverified
  person's subject on an access request and rebinds a grant on a claim: hold, and bind, once every
  replica runs it.

### 3.5 The role

| A responder | |
|---|---|
| Reads | every app's fleet row as a read-only member's reads (name, owners, summary, `envs` address and state) plus `hold`; the fleet's **Held** filter (`GET /fleet?held=true`); their own trail rows. Nothing else: no findings, runs, deploys, logs, packages (no `canyon pull`), secret names or audit rows of any app |
| Does | holds any app's production (§3.4); lifts a hold, optionally starting the held image again (§3.6) |
| Never | upload, check, deploy, promote, approve, set or delete a secret, share, delete an app, change a policy or setting, open an app it is not shared with, take preview down |

**Assignment**: the preferences row gains `responder_emails` (addresses, admin-set, audited before and
after like every key there) and `responder_claim`/`responder_value`, mirroring `admin_claim`/
`admin_value` — set from the environment or the row, matched at sign-in, stored as
`sessions.claimed_responder` (the next migration). **The role** is `admin` if admin, else `responder`
if listed or claimed, else `member`; the list is read live, a claim lasts its session, and an urgent
demotion is `DELETE /api/v1/sessions?subject=…`, as for admins (AUTH.md §3). `role` becomes `member |
responder | admin` on the fleet's and preferences' `viewer` and on audit rows. Responders are not
grants: one who also owns an app holds both. Settings copy: `Security responders can stop any app's
production and approve its blocking findings. They never deploy, promote or change secrets.` (amended
2026-10-14, #2068: it said they never approve, which #2204 made untrue; design/PERMISSIONS.md wins).

### 3.6 Getting it back

`DELETE /api/v1/projects/:id/envs/prod/hold?held_at=<the hold read>[&start=true]` (`held_at` amended 2026-10-14: a lift from a stale read lifts nothing, `409 hold_changed`):

a responder or an admin, from a **browser session only** (any other session `403
{error:"dashboard_required"}`, as production approvals; on a host with sign-in off any session), from the fleet's *Lift the hold* (a team row's side pane, a reduced row, Needs attention) — a
responder cannot open an app not shared with them. Anyone on the app's team — an owner or
contributor grant, directly or through a group, now or as the hold recorded it when production was
held (design/PERMISSIONS.md) — and a responder too, is `403 {error:"responder_required", admins[]}`
unless an admin: the stop exists for when the team's judgement is the one in question, and the team
never lifts its own app's hold. The role is checked before the session, so an owner lifting from the CLI is
`responder_required`. Without `start`, production stays empty until the team promotes. With
`start=true`, a promote run of the held image and gate run — the one production start a responder or
an admin makes: the same bytes, promoted once already, not reviewed again because nothing new ships.
Audit: `lift hold` `{held_by, held_at, reason, started, package?}`, the actor the lifter, `package` the version started; the run's
`deploy prod` carries `restart: true`.

### 3.7 What people see, and who learns of it

- **The app's users**: on Docker, Canyon's proxy answers `502 app_unreachable` (`surfaces.go`), as for
  any undeployed app. On Kubernetes, users see whatever the org's gateway answers for a missing backend.
  The reason is never shown to users.
- **The team**: a banner above production on the app page *(proposed)*: `Production was stopped by
  <who> on <date>: <reason>. It stays stopped until a responder or an admin lets it run again.` The
  same line appears in `canyon status`, after every deploy's access lines (a `build.log` row), on the
  fleet row as `Held`, and on the trail.
- **Nothing outbound**: Canyon has no outbound channel (§6).

### 3.8 The experience

```
$ canyon undeploy --app claims-portal --env prod --hold --reason "Exports claimant SSNs unauthenticated (INC-2231)"

  canyon 0.6.0 · claims-portal prod → held · canyon.meridian.example

  Stopping claims-portal in production and holding it: nobody can promote it until you or an admin let it run again.   (proposed)
  Its team can still deploy to preview.                                                                               (proposed)

  Stop and hold claims-portal in production? [y/N] y

  ✓ Held    prod · stopped                                                                                           (proposed)
```

- **Flags**: `--hold` needs `--reason`, `--reason` without `--hold` is a usage error, and so is
  `--hold` on preview: only production is held. A plain
  `undeploy` by a responder answers `403 team_required`, as for any non-member. `--app` is new on
  `undeploy`, as `logs` and `share` have it.
- **The CLI's stdout buffer** `hold()`/`release()` (`cli/hold.go`) was renamed `park()`/`unpark()`
  (`cli/park.go`) in the PR that built the hold's CLI (#2067), so "hold" means one thing in the code.
- **The owner's agent** meets the hold on `canyon deploy --env prod --format ai` *(proposed)*:
  `CANYON PROMOTE — RESULT: HELD`, then `held by:`, `held since:`, `reason:`, `ask:` (the admins), and
  `outcome: nothing was promoted. Production is stopped.` NEXT STEPS: 1. tell the user, with the
  reason; 2. do not try to put it in production another way; deploying a fix to preview is allowed.
  Exit `2`.
- **Dashboard**: the fleet's Held filter and, for responders and admins, *Stop production* on each
  row; the app page's banner and Promote disabled with its sentence (D6); for responders and admins,
  *Lift the hold* in the fleet (a team row's side pane, a reduced row), with `Lift the hold, keep it stopped` · `Lift the hold and start v1.4`, offered to
  no one holding a grant on the app but an admin (§3.6); Settings › People & sign-in, the responders
  list and the claim.

### 3.9 Routes, contracts, audit

| Route | Minimum | Refused |
|---|---|---|
| `DELETE /projects/:id/envs/:env` (undeploy) | contributor; `prod` a person while `prod.human-approval` blocks | `403 team_required` · `403 person_required` — never `run_in_progress` (§3.3) |
| `POST /projects/:id/envs/prod/hold` | responder or admin, a person | `403 {error:"responder_required", message:"only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.", admins[]}` *(proposed)* — owners too |
| `DELETE /projects/:id/envs/prod/hold` | responder or admin, browser | `403 responder_required` · `403 dashboard_required` · `404` no hold |
| `GET /fleet?held=true` | any member (a member sees their own rows, as today) | — |
| the runs list, a run's read and SARIF, the status read, the approve routes | contributor, or a responder with no grant (#2204) | `403 team_required` |
| every other `/projects/:id/` route | unchanged; a responder is not admitted | `403 team_required` |

- **Contracts (PR 11)**: routes.md — the hold routes; `409 held` on the promotions, approvals and
  project-delete rows; the report row's `409 held` plus `remove`; `hold?` on the project, fleet and
  status reads (contributor and above, responders, admins; the reduced row carries none); the
  receipt's `held`; `responder_*` on preferences; the `role` enum. events.md — the hold `build.log`
  line (the pending-stop lines land with the brake seam, PR 2). Schemas — `role` in `access`, `app-page`, `fleet`,
  `guardrails`; `hold` in `app-page` and `fleet`.
- **Audit** (both `governance`): `stop app` `{env, package?, image?, reason, cancelled[]}`, written
  when stopped; `lift hold` `{held_by, held_at, reason, started, package?}`. Trail words (#2244, amended
  by #145): `stopped and held production: <reason>`, or `held production: <reason>` where nothing ran
  (no `image_digest`) · `lifted the hold, leaving production stopped` · `lifted the hold and started
  <version>`.

### 3.10 Testing

- **Unit (red first)**: `TestUndeployProdWhileAPreviewRunRuns` (the brake seam's red) ·
  `TestUndeployProdCancelsAPendingPromotion` · `TestStopMidSwitchStopsAfterIt` (a hold planted between
  `deploy_started_at` and the row: the container stops, no row is written) ·
  `TestHeldRefusesEveryProdWriter` (promote, approve, a stub patch run through `StartTx`,
  `WriteProdTx`) · `TestOrgReportCannotWriteProdWhileHeld` (409, a `remove` queued) ·
  `TestResponderReadsOnlyTheFleetRow` (runs, logs, pull → 403) · `TestOwnerCannotHold` ·
  `TestLiftFromCLIIsDashboardRequired` · `TestStartAgainRunsTheHeldDigest` · `TestAppDeploymentKeepsNoRevisions`.
- **Docker E2E** (J13): `undeploy --env prod` succeeds while a preview deploy is in flight; the
  responder holds production → promote, approve and delete each `409 held`; a lift from the CLI is
  `403`; a Playwright lift leaves production empty, and *Start again* serves the same digest; `stop app`
  and `lift hold` are on the trail.
- **EKS test cluster** (new **X** cases; a fourth identity-provider user on no app's team,
  `E2E_RESPONDER_EMAIL`/`_PASSWORD`): **X1** the hold removes the Deployment, Service and
  NetworkPolicy; record what the ALB answers · **X2** the responder's `canyon pull`, logs and run reads
  are each `403` · **X3** no ReplicaSet is left to undo · **X4** (`e2e/handoff`, wave 2) a late
  `deployed` while held is refused and a `remove` is sent.

## 4. Fail closed — every path

- **Rollback**: an unreadable `earlier[]` refuses the command (`couldn't read production's earlier
  versions`, never an empty list); a digest not in `earlier[]` is `409 not_earlier`; a blocked review
  ships nothing; an image that is gone refuses the rollback, never rebuilds; a "kept" image that is
  gone by the promote fails the promote run with production unchanged.
- **Host**: `GET /projects/:app` or `GET …/packages/:pkg` answering anything but 200 or a 404 the
  guard reads refuses the verb (a `401` with §8.2's line); a `.canyon/base` that can't be used
  refuses the guarded verbs; a session file the CLI won't use, in `config.json` or `hosts/`, gets
  one line naming it and is then first contact for that host, which the host's own auth keeps
  closed.
- **Hold**: a role that cannot be computed is `member`; a hold that cannot be read fails the
  transaction, so nothing starts; a stop whose executor call fails keeps the hold and answers `500`,
  and a retry resumes; a pending stop the run cannot complete leaves the hold, and the run ends
  `failed`/`error` with `production was held while <version> was starting; Canyon could not stop it:
  an admin can look up run <id>` *(proposed)*.

## 5. QA viewpoints

| | Rollback | Host guards | Brake / hold |
|---|---|---|---|
| Owner | puts back via CLI and dashboard; blocked → approves → retries | (a), (b), `--create` | undeploy during a preview run works; held → `409 held`, banner, cannot lift |
| Contributor | as owner | as owner | as owner |
| Read-only | no *Earlier versions*; `?run=` 403 | n/a (no upload) | sees the app offline; no hold field |
| No access | `403 team_required` | n/a | the no-access screen |
| Admin | everything | as owner | holds, lifts, starts again; lift from the CLI → 403 |
| Responder | `403` on every app read but the fleet row and what approving needs (#2204) | n/a | holds from the CLI and the fleet; lifts and approves in the dashboard; deploy, promote, pull, logs → 403 |

## 6. Deliberately left out

- **A preview leg for rollback** (§1.3); a rollback command (redirects only); versions older than the
  last five; undoing data, secrets or outbound rules (§1.6).
- **A pin file** (§2.3) and **a host in `canyon.toml`**: the file is shared, older CLIs refuse unknown
  keys (`cli/project.go` `readProject`), and an agent would edit it.
- **Automation stopping an app** (SIEM/SOAR/PagerDuty with a stop-only token that cannot lift).
  *Trigger*: an org asks to stop from its tooling. That needs scoped API tokens, which Canyon does not
  have.
- **Outbound notice of a stop or lift** (email, chat, a webhook). *Trigger*: an org asks for notices
  outside Canyon. Until then its SIEM polls `GET /api/v1/audit`, where `stop app` and `lift hold` are
  `governance` rows.
- **Holding preview**, holds that expire, owners holding their own app, `group:` responder entries
  (the claim covers groups), responder reads beyond the fleet row, and a Docker "offline" page
  (`502 app_unreachable` exists).

## 7. Implementation plan

Each PR has its own issue, branches off `main`, and is ≤1,000 added lines (all here are well under).
Each PR is red first (rule 2). Seam and permission PRs merge before their consumers (rule 5). The
order is the `keep` seam, then the brake seam, then PERMISSIONS, then the rest. Sizes are added lines
including tests.

| # | PR | Red first | Lines |
|---|---|---|---|
| 1 | `docs(contracts): /run keeps images by digest` — executor.md, schema, fixture; PATCHING.md cites it | `validate.py` refuses `keep` holding a tag | ~80 |
| 2 | `docs(contracts): undeploy conflicts only with runs writing that environment` — §3.3, `202 stopping`, `person_required` stated; events.md's brake lines; HAND-OFF's superseding `remove` | prose seam; test lands in 5 | ~120 |
| 3 | `docs(permissions): the responder` — model, table, *Applying it* gains the responder viewpoint | doc | ~70 |
| 4 | `feat(executor): keep[] on Docker and Kubernetes; revisionHistoryLimit 0` | `TestCollectSparesKept` ×2, `TestAppDeploymentKeepsNoRevisions` | ~280 |
| 5 | `feat(server): the brake` — a runs column marking a deploy run (the next migration), conflict sets, cancel, pending stop on the run, audit when stopped | `TestUndeployProdWhileAPreviewRunRuns`, `TestStopMidSwitchStopsAfterIt` | ~450 |
| 6 | `docs(contracts): rollback` — §1.8 | `validate.py` on `earlier` without `kept` | ~200 |
| 7 | `feat(runs): WriteProdTx, deploy_history, earlier[], the keep policy on every /run` | `TestEarlierCollapsesPatchedAndRepeated`, `TestEarlierUsesItsIndex` | ~450 |
| 8 | `feat(runs): the rollback review` — `rollback_to`, kept digest or refuse (never build), limit exemption, promotions ship `ships` | `TestRollbackShipsTheDigestProductionRan`, `…RefusesWhenTheImageIsGone`, `…ReviewIsNotRateLimited` | ~550 |
| 9 | `feat(cli): deploy --env prod --version` — resolve via `earlier[]`, confirm lines, redirects, `--version` clash, status line, skill line | `TestVersionRoundTripUnlabelled`, `TestRollbackRedirect` | ~450 |
| 10 | `feat(dashboard): Earlier versions and Put back` | `PutBack.test.tsx`: a viewer sees no card; an image that is gone shows the refusal and no Promote | ~350 |
| 11 | `docs(contracts): responder, hold` — §3.9 | `validate.py` on `viewer.role: responder` before the enum | ~250 |
| 12 | `feat(server): the responder role` — list, claim, session column, fleet row and Held filter | `TestResponderReadsOnlyTheFleetRow`, `TestResponderClaimAtSignIn` | ~350 |
| 13 | `feat(runs): hold enforcement` — table, `ErrHeld` in `StartTx`, `cancelled`, `WriteProdTx`, receipt `held`, project delete | `TestHeldRefusesEveryProdWriter` | ~500 |
| 14 | `feat(server): hold and lift routes` — stop, outdated approvals, start again, audit | `TestOwnerCannotHold`, `TestLiftFromCLIIsDashboardRequired`, `TestStartAgainRunsTheHeldDigest` | ~450 |
| 15 | `feat(server): hold under hand-off` — report `409 held` + `remove`; stop supersedes open releases | `TestOrgReportCannotWriteProdWhileHeld` | ~250 |
| 16 | `feat(cli): undeploy --app --hold --reason; held from the receipt; park()` | `TestHoldNeedsReason`, held ai block golden | ~400 |
| 17 | `feat(dashboard): responders in Settings` | Settings test: claim pair both-or-neither; and, since §3.5's sentence is amended (responders approve, #2204), the Settings test and D5's registry red on the old sentence | ~250 |
| 18 | `feat(dashboard): Stop, the banner, Lift, Held filter` | a responder's fleet row shows Stop and Lift — and keeps its app link, which since #2204 opens the runs they approve (amended 2026-10-14; it read "no app link") | ~400 |
| 19 | `docs(contracts): one package by id` — `GET …/packages/:pkg` | `TestRoutesTableMatches` lists it | ~40 |
| 20 | `docs(cli): host guards and per-host sessions` — cli.md §§1, 3.2, 6.6, 8.2, 8.4 | doc | ~120 |
| 21 | `feat(server): GET packages/:pkg` | allowed contributor, refused viewer | ~120 |
| 22 | `feat(cli): guard (a) and --create` | `TestDeployRefusesAnAppTheHostLacks` | ~250 |
| 23 | `feat(cli): guard (b)` | `TestUnknownBaseRefusesNamingBothHosts`, `TestSecretSetChecksTheBase` | ~250 |
| 24 | `feat(cli): per-host session files, one canonical host` | `TestLoginToASecondHostKeepsTheFirst`, `TestConfigJSONIsByteIdentical`, `TestCanonicalHost` | ~350 |
| 25 | `test(e2e): J12 rollback, J13 brake and hold, J14 host (canyon-b)` | each journey fails on main first | ~600 |
| 26 | `test(cluster): R, X and T cases; the responder user` | R1 red before PR 8 is deployed | ~450 |

Pins on order: 4 after 1; 5 after 2; 12 after 3 and 11; 7 before 8 and 13 (`WriteProdTx`); 21 after
19.

## 8. Edits other docs need

- **design/PERMISSIONS.md**: the responder (PR 3).
- **design/PATCHING.md**: already cites `keep` from §1.5, names its undo **revert**, defers its hold
  handling to §3.4 and ends its watch on a hold. Its `auto` is deferred; if it returns, it either joins
  §0's decision record by decision or goes through a patch approval.
- **design/EXPERIENCE.md**: D5 gains every *proposed* string; D6 the held Promote button and the
  image-gone refusal line of a rollback.
- **design/AUTH.md**: §3 `responder_claim`/`responder_value` and the role; §4.5 a rollback is a
  promotion and the setting covers it, the brake and a hold are not.
- **design/specs/cli.md**: §1 (`deploy --env prod --version`, `undeploy --app --hold --reason`,
  `--create`, the `rollback`/`revert` redirects), §3.1 (the trust line's host), §3.2 (`--create`),
  §4.1 and §4.3 (check and secret list under guard (a)), §6.1 (Earlier in production, held), §6.3
  (rollback), §6.6 (pull and the base check), §7.2 (the skill's rollback, hold and `--create`
  lines), §8.2, §8.3 and §8.4 (per-host session files).
- **design/specs/screens.md**: Earlier versions, Put back, the hold banner, Lift, the responder's
  fleet row, Settings.
- **design/ARCHITECTURE.md**: the retention sentence exempts production's last five versions only.
- **contracts/**: PRs 1, 2, 6, 11 and 19.
- **e2e/cluster/CASES.md** and **README.md**: the R, X and T cases and `E2E_RESPONDER_*`.
- **design/EGRESS.md**: its §2 pointer (`design/OPERATIONS.md §2`) still holds.
