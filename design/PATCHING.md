# Patching — Canyon fixes the platform under every app

**Partly built.** Phase 1 is built, and phase 2: the patch run to preview with its watch and revert, `patching`
and the patch approvals, the watch and revert of production after an approved patch, the fleet approval, the
identities read and resume, and what people see of it (§8), beta and off by default behind `CANYON_PATCHING`
(#2379, #2382, #2100, #2102, #2570, #2571, #2105); phase 3's CI bumps, as an issue a person opens the PR from (#2114), and its rebuild patches, opt-in and always
asked (§5; #2111, #2117). Not built yet: phase 4, the app page's *System libraries*
card and B6's *Patched by Canyon*. Phase 2's plan, its waves, filing patch approvals, the fleet approval and
turning patching on are Canyon Enterprise's (`ee/patching`; design/ARCHITECTURE.md §8, #2923): a server built without
it reads patching as off (contracts/routes.md, *Licence*). Phase 1 and the patch run itself are the core's.

**The rule this document exists to enforce: Canyon fixes what Canyon put in the image, never what
the builder put there; every image it changes passes the same gate as a person's deploy; and
production changes only when a person says so.** An app's image is three owners' work: Canyon's run
image (the operating system), what a runtime buildpack installs, and the app with its dependencies. A
builder can fix only the third, and today Canyon cannot see the first two at all.

Four phases, each useful alone, each shipped and tested end to end on Docker and on the AWS EKS test cluster
before the next starts:

1. **See** (§3) — an SBOM of every built image, re-matched offline against each new vulnerability
   database; a rule, judged before production, that counts what Canyon could fix.
2. **Fix OS packages by rebase** (§4) — a newer run image under the app's unchanged layers, through
   the same gate, to preview on its own, to production when a person approves.
3. **Runtimes** (§5) — CI moves Canyon's pinned Paketo images; rebuild patches, always asked.
4. **The patch buildpack** (§6) — Canyon fixes what Paketo has not shipped yet. **Spike-gated**: it
   ships only if a spike passes the criteria written here.

Deferred, each with its trigger (§12): `auto` production patches, production patches of a package
preview no longer runs, patching under hand-off.

---

## 0. How it works

### 0.1 The trace — a CVE lands, production runs the patched image

1. **At every deploy**, after `/run` answers and off the deploy's path, Canyon asks the executor for
   the image's CycloneDX SBOM (§3.1). Each component keeps the layer it came from. Canyon stores the
   components per layer and classifies each layer: `os`, `runtime`, `lifecycle`, `app` or `unknown`.
2. **Every `CANYON_PATCH_INTERVAL` (6 h)**, one scheduled workflow takes the newest vulnerability
   database and re-matches each distinct platform layer's components offline (§3.2). No registry is
   read. A new advisory for `libssl3 3.0.13-0ubuntu3.4` becomes a finding on the `os` layer of every
   image built on that run image, at once.
3. **The rule** `platform.known-vuln` now reads, for the digest preview runs: one finding Canyon can
   fix (a fix is published, the class is Canyon's). The receipt shows it; whether it blocks
   promotion is the org's state for the rule (§3.3).
4. **The plan** (§4.2): the finding is `high` or above, its class is `os`, and the run image Canyon
   names now — a Canyon release's, or the operator's `CANYON_IMAGE_RUN` — carries the fixed version.
   Outcome: **rebase**. Had the run image not had it yet, the outcome would be **wait**.
5. **Waves**: the rebase onto that run image (one **platform identity**) starts with one canary app's
   preview, then waves of 5, 25 and the rest, each after the last wave's watches ended with no revert.
6. **The patch run** (§4.3), on its own low-priority queue: the same gate on the same package; then
   rebase to a fresh tag; the new image's SBOM; the proof that only the run image changed; the
   regression check. Any failure ends it with nothing deployed.
7. **Preview**: `/run` of the patched image, the previous digest kept (OPERATIONS.md's `keep`).
8. **The watch** (§4.4), `CANYON_PATCH_WATCH` (15 min): an exit or restart of the patched container
   reverts to the kept digest. A person's request, a stop or an undeploy ends the watch clean.
9. **Production, asked** (§4.5): production runs the image preview ran before the patch, so Canyon
   files a patch approval for preview's patched digest. The app's team, or an admin for the whole
   fleet, approves it in the dashboard.
10. **The promotion** re-`/run`s the digest that served preview (executor invariant 4), and production
    is watched the same way.
11. **The record**: `Canyon patched claims-portal prod: libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5,
    app unchanged` (proposed) on the audit trail, the org timeline and the app page.

### 0.2 Sequence

```
 trivy DB tick      server (DBOS)                    executor                  preview / prod
     │                   │                               │                           │
     │──new DB──────────▶│ rescan workflow (one per tick)│                           │
     │                   │──POST /scan target=sbom──────▶│ trivy sbom, per layer     │
     │                   │◀─findings per layer───────────│                           │
     │                   │ join layers ↔ digests; plan   │                           │
     │                   │ (rebase · wait · nothing)     │                           │
     │                   │── patch run (queue "patches") │                           │
     │                   │   gate (same as deploy)       │                           │
     │                   │──POST /rebase────────────────▶│ kpack / pack rebase       │
     │                   │◀─new digest + layer facts─────│ to a fresh tag            │
     │                   │──POST /scan target=image─────▶│ SBOM of the new image     │
     │                   │ proof · regression · config   │                           │
     │                   │──POST /run keep=[old]────────▶│──────────────────────────▶│ preview
     │                   │ watch until watch_until       │◀─exits / restarts─────────│
     │                   │ (revert: /run old digest)     │                           │
     │                   │ file patch approval ──▶ person approves in dashboard      │
     │                   │──promote: /run same digest───▶│──────────────────────────▶│ prod
     │                   │ watch prod; record; audit     │                           │
```

### 0.3 An environment's patch state

`envs.<env>.patch.state` on the project read, answered to contributors and above. The states are
spelled as every contract enum is, in snake_case (decided 2026-10-18 with the seam, #2086).

| State | Means | Leaves it when |
|---|---|---|
| `none` | nothing Canyon could fix at or above the threshold, or patching is off | a rescan finds one → `planned` or `wait` |
| `planned` | a patch run is queued, in a wave, or running | it deploys → `patched_preview` · it fails → `held` · a person's request cancels it → `none` |
| `wait` | Ubuntu has a fix, and the run image Canyon names does not have it yet | the run image moves → `planned` |
| `patched_preview` | preview runs the patched digest; its watch passed or is running | production filed → `asked` · a revert → `reverted` |
| `asked` | a patch approval for production is open | approved and live → `patched` · expired or outdated → re-filed next cycle while the plan stands · denied → `denied`, never re-filed for that identity |
| `patched` | the environment runs a patched digest, its watch passed | the next rescan plans again, or a deploy replaces it → `none` |
| `reverted` | the patched container exited or restarted in its watch; the old digest runs | the platform identity changes → `planned`; admins are told |
| `held` | the gate blocked, the proof failed, the config changed, production is held, or the identity is paused | the reason clears (a fix, an approval, a deploy, a resume) |
| `denied` | production runs the unpatched image: its team declined the patch for this run image, and Canyon won't ask again for that identity (added 2026-10-31, #2601) | a deploy replaces production → `none` · a newer identity's patch is asked → `asked` |
| `could_not_rescan` | the database is stale, or the SBOM or re-match did not finish | the next rescan answers |

### 0.4 Plan outcome → action

| Outcome | When | Action |
|---|---|---|
| `nothing` | no counted finding at or above `high` | none; the receipt shows the notes |
| `rebase` | `os` class, and the named run image carries a fixed version (its own SBOM, matched) | patch run, in its identity's wave (Phase 2) |
| `wait` | `os` class, fix published, not in the named run image | none; admins see "run image behind" |
| `bump` | `runtime` or `lifecycle` class | none at run time; the CI bump picks it up (Phase 3) |
| `rebuild` | `runtime`/`lifecycle`, the org's `rebuilds` reads `ask`, no rebase is planned, and the builder Canyon names now differs from the one that built the image | a rebuild patch, its production always asked (Phase 3, §5) |
| `catalog` | `runtime`, and the patch buildpack has an entry | rebuild with `canyon/patch` (Phase 4, only if the spike passed) |
| `unknown` | class `unknown`, or no fix published | none; a note on the receipt and the app page |
| `paused` | the identity's breaker tripped, or `patching` reads `off` | none; admins see why |
| `held` | production held or stopped, the gate blocked, the config fingerprint changed | none; the app page says which |

## 1. What is true today

Verified against `main` at 48f8b11e; cited by name.

- **The gate never builds** (EXPERIENCE D9.2), and `check ≡ deploy` rests on it. Trivy scans the
  package's tree with `trivy fs` (executor/scan.go, `scanTools`). It runs offline with
  `--skip-java-db-update`. **Nothing scans the image an app runs.**
- **The run image is Canyon's.** `canyon-run` is executor/run.Dockerfile: Paketo's noble run image by
  digest, plus three libraries. It is tagged by the file's hash (executor/executor.go, `runImage`),
  so an Ubuntu fix reaches an app only through a Canyon release. On Kubernetes the operator builds it
  and the ClusterStack `canyon` names it (deploy/k8s/builds.yaml). No operator setting names another
  run image (contracts/executor.md, *Images and database sources*).
- **The builder is pinned** (executor/builder.Dockerfile; deploy/k8s/builds.yaml's ClusterStore). Each
  Kubernetes build is a one-shot kpack `Build` (executor/kube/build.go, `Build`). The build digest
  covers the builder's identity (contracts/executor.md, `GET /builder`; routes.md, the deploys row).
- **`/run` keeps the previous container until the new one is up** (contracts/executor.md, `POST
  /run`). After a `/run`, the executor deletes tags of images no environment runs
  (executor/executor.go, `collect`; on Kubernetes the same in *On Kubernetes*). **Invariant 4**:
  promote never calls `/build`; it re-`/run`s the digest that served preview.
- **Runs.** There is one DBOS queue, `gate`, with no limits (runs/start.go, `Queue`). `StartTx`
  supersedes a `pending` run, answers `409 run_in_progress` while one is `running`, and never lets a
  promotion supersede or be superseded. The fleet's `gate_state` is the newest run of any package
  (cmd/canyon-server/fleet.go, `fleetDerivation`). A GitHub push may deploy to preview while the
  app's `auto_deploy` is on (routes.md, `POST /webhooks/github`).
- **Production approvals** (routes.md, *Production approvals*): one pending per project, and the
  project's next request deletes it. Each names the run, package and image preview ran, and turns
  `outdated` when preview runs another. Only a person in a browser decides one.
- **`envs.<env>.answered`** is the one `GET /` Canyon makes after a deploy. It is never asked again,
  never polled, and never a judgement (routes.md). There is no live health signal but the
  container's state and restart count (`GET /running`, `envs.<env>.state`).
- **A dependency's finding carries trivy's fields** (contracts/finding.md, *A dependency's name and
  version*). `fixed_in` exists only for a plain `X.Y.Z` (internal/trivy, `FixedIn`).
- **OPERATIONS.md** (designed) owns the hold on production (§3.4) and the executor's `keep` (its build
  plan, the `keep` seam). JOBS.md owns jobs.

## 2. Who fixes what

A component's class is **f(layer, package type)**. The layer comes from trivy's
`aquasecurity:trivy:LayerDiffID` component property. The image's `io.buildpacks.lifecycle.metadata`
label says whose layer it is.

| Class | Which components | Who fixes it | How |
|---|---|---|---|
| `os` | dpkg packages; only the run image's layers hold them (buildpacks run unprivileged), canyon-run's own apt layer among them | Canyon | rebase onto a newer run image (Phase 2) |
| `runtime` | what a buildpack of Canyon's builder ships: everything in a **runtime provider**'s layers — node-engine's (npm and its dependencies, beside Node), CPython's (pip and setuptools, each with the packages it vendors), bellsoft-liberica's (the JRE and its helper), go-dist's, ca-certificates' (its helper) and spring-boot's (its helper and the `spring-cloud-bindings` jar) — and an installer's own Go binary (`gobinary`) in its layer, as npm-install's in `launch-modules`: a newer build of the buildpack fixes each | Canyon, via Paketo | a newer builder, then rebuild (Phase 3), or the patch buildpack (Phase 4) |
| `lifecycle` | the launcher's layer: the lifecycle's Go binary, its modules and its `stdlib` (§3.5). The config and process-types layers hold nothing trivy lists; the SBOM layer is the app's (below) | Canyon | a newer lifecycle image, then rebuild (Phase 3) |
| `app` | an **allowlist**: components in an installer buildpack's layers (`npm-install`, `yarn-install`, `pnpm-install`, `pip-install`, `pipenv-install`, `poetry-install`, `conda-env-update`, `bundle-install`, `composer-install`, `go-mod-vendor`, `maven`, `gradle`) but the installer's own Go binary, and `/workspace` (the app's layers): Maven and Gradle leave no layer in the image, and a Java app's dependencies are its jars under `/workspace` | the team, as any dependency of the app | not a platform finding: `deps.known-vuln` reads them at review, and before production counts each one its review didn't report: a build step's download, or a Maven app's inherited versions the review could not resolve (§3.6) |
| `unknown` | everything else: another buildpack's layer — one the app names in its `project.toml` among them — a layer the label does not name, an operator's `CANYON_BUILDER` buildpacks | the team, as any dependency of the app (decided 2026-11-01, #2355): Canyon can't | a note under `platform.known-vuln`, never planned; counted by `deps.known-vuln` before production (§3.6). No build is refused for its buildpack |

**A Go binary's `stdlib` is its binary's layer's class** (decided 2026-10-18, #2216), because a newer
build of that binary is what fixes it: the launcher's is `lifecycle` (§3.5), a buildpack's own helper's
is `runtime` with its modules, and go-dist's is `runtime` only inside an app's own Go build, whose
standard library go-dist's Go put there. Every buildpack helper carries its own (Go 1.27.1 or 1.26.5 on
today's pins), so `stdlib` is never classed by its name alone.

**The SBOM documents are never read** (decided 2026-10-18, #2216). `/layers/sbom` holds the lifecycle's
and the buildpacks' SBOM documents, and trivy lists every package a document names, under that layer: on
today's pins each app dependency and launcher module twice more, where it would read as the
lifecycle's. Each is listed again in its own layer (the launcher's under another spelling:
`buildpacks/lifecycle@v0.21.20-…+dirty` for `0.21.20`, `stdlib@1.26.7` for `v1.26.7`), so the image scan
skips the four document names the lifecycle writes there (`sbom.cdx.json`, `sbom.spdx.json`,
`sbom.syft.json`, `sbom.legacy.json`; contracts/executor.md, `target: "image"`) and loses nothing. It
skips nothing else: an app's own buildpack (project.toml) can write any file under `/layers/sbom`, so
anything else there is listed, and the SBOM layer is owned `app`, what the build put there
(contracts/finding.md, System libraries and build tools).

**Coverage, stated honestly.** Trivy matches package metadata: dpkg's database, `package.json`,
`METADATA`, jar manifests, Go build info. **It does not identify the Node, CPython or JDK binaries
themselves**, so a Node security release is not a finding. The JRE ships one jar, `jrt-fs.jar`, which
carries no `pom.properties` and is not in the Java database: trivy lists nothing for the JRE. That is why
the runtime class reads "what a buildpack ships", and why Phase 3 moves Paketo pins on a schedule rather
than on findings.

**Java.** Today's scans skip the Java database. The image SBOM (§3.1) reads it: trivy's jar analyser
identifies each jar from it, and `trivy image` of an image holding a jar ends without an SBOM when it
is missing. The re-match (§3.2) never reads it: `trivy sbom` matches a jar component's purl against the
main database alone. It is a database of its own, `canyon-scandb-trivy-java` (`CANYON_SCANDB_TRIVY_JAVA`,
default trivy's own), on Kubernetes a `ReadWriteOnce` volume beside grype's that serialises image scans
as grype's serialises its scans. If it is missing, unreadable or stale, the image scan fails closed —
`not_finished` with `reason: "java"` (contracts/executor.md) — and the image reads not checked; there is
never an SBOM silently missing its jars. On Paketo's Spring Boot sample, 16 of its 32 jars carry no
`pom.properties` (Spring Framework, Spring Boot, Tomcat embed, Micrometer) and trivy names each from the
Java database; the one it cannot name is the JRE's `jrt-fs.jar` (#2216).

This table is checked against what trivy lists for Paketo's Node, Python and Java samples on today's
pins by `TestEndToEndPaketoImagesHoldWhatPATCHINGsClassTableSays` (executor/e2e_test.go, #2216): a
builder or trivy change that moves a package out of the class this table gives it fails it.

## 3. Phase 1 — See

### 3.1 The SBOM, taken after `/run`

A deploy's run ends at `Live` as today. Its last step enqueues **`image.sbom`**, a workflow on a queue of
its own, `image-sbom`, one at a time across the replicas and never behind a patch run (§4.3; #2379's
review, 2026-10-19), while `platform.known-vuln` is on (§3.3) or `deps.known-vuln` is placed before production (§3.6).
**A deploy's goes first** (amended 2026-11-27, #264): the queue takes a deploy's `image.sbom` before every
one a rescan tick (§3.2) or an apply (§3.6) asks of the fleet, so a deploy waits for the one image scan in
flight, never for the fleet's (a tick after an upgrade asked of about 76 images, and a deploy waited
hours). Its id is the run's project, the image
digest and the run: each deploy asks, and a project's image whose scan finished is never read again,
while one that did not finish — `timeout`, `no_room`, an executor's error, `over_cap` before an operator
raised the cap, `busy` — is read again by the next deploy or rescan tick. Once its scan is stored, it matches the
image's layers at once (§3.2), so production waits the scan's length, never an interval. It calls
`POST /scan` with
`target: "image"`. Docker `docker save`s outside the scan container and unpacks only the OCI layout into
its `/scan/image`, which trivy reads by `index.json`: Docker 29's containerd store saves layers of mixed
compression, which trivy misreads through the legacy `manifest.json` (#2585); on Kubernetes
the executor feeds the image from `CANYON_KUBE_REGISTRY` with its own credential, as it feeds a tree,
the pod's ephemeral storage requested from the manifest's size up to `CANYON_SCAN_IMAGE_MAX` (8 GiB;
over it, `over_cap`). Trivy writes `--format cyclonedx` (which lists every package; `--list-all-pkgs`
is its JSON format's) under the flags a scan has today that `trivy image` takes — no
`--skip-check-update`, and `--cache-backend memory` since its cache would write to the read-only
database — and `--skip-files` for the lifecycle's SBOM documents (§2), with no network, read-only, as 65534, the `docker save` tar readable by that user. This is
the only time Canyon reads the image.

- **What is stored.** Each image gets its ordered diff IDs — from the image's config, the answer's
  `diff_ids`, since trivy's SBOM sorts them — and its layer → owner map from the label.
  Each scan is the **project's own**: stored under the run's project and the image digest, never under a
  digest alone — two apps may run one digest, and one never reads or writes the other's scan or label.
  Each image's components are stored **per layer, as that image's own**, deduplicated **by content**:
  a layer's set is the slice the re-match sends — that layer's components and the image's OS
  component, which trivy needs to match dpkg packages — stored once under its sha256, and each image
  names, per diff ID, the slice its own SBOM gave it. Never by diff ID alone: what trivy lists in a
  layer depends on the whole image — an upper layer can rewrite or white out the dpkg status file or
  `os-release` that trivy reads the layer's packages and the OS from (pkg/fanal/applier,
  `ApplyLayers`) — so one app's image must never change what another app's shared layer reads (#2075).
  Every app on one `canyon-run` still shares its `os` layers' slices, and every app on one node-engine
  version that layer's, since their slices are byte for byte the same.
- **The SBOM is also published** on Kubernetes as an OCI referrer of the image
  (`application/vnd.cyclonedx+json`), for the org's own tools.
- **Until the SBOM lands**, the rule reads `no image scan recorded` (proposed) and does not pass. The
  wait is the scan's length: production waits on it, preview never does.

### 3.2 Rescans: per layer, offline

One **DBOS scheduled workflow, `patch.rescan`**, runs every `CANYON_PATCH_INTERVAL`. Its workflow id
is DBOS's own for a scheduled workflow — the schedule's name and the tick's scheduled time — so across
server replicas each tick runs once, and a tick that finds the database no newer than the last one's
stores nothing new. It runs on a **global concurrency-1 queue**. For each distinct slice of a layer of a platform class
(`os`, `runtime`, `lifecycle`, `unknown`) that a running digest references — preview and prod, and the
images kept for a rollback (routes.md, `earlier[]`), so a rollback mid-incident never meets a stale
database — of a project whose rule is on, and of a layer holding an `app` package of a project that places
`deps.known-vuln` before production (§3.6), it sends that layer's components to `POST /scan`
with `target: "sbom"`. That is `trivy sbom`: seconds, offline, no image read, the main database
alone; the part is at most 10 MiB, and is refused unless it has the exact shape of a slice of trivy's
own image SBOM — bounded as it streams, then component by component, each name what trivy writes beside
its purl — and answers no clean report of a component trivy did not list as read (contracts/executor.md). A package without a purl (a `library` component) is not matched by the re-match; the
server counts every such package of the layer as not checked (a note, never clean). It answers trivy's JSON report, each vulnerability carrying its component's layer
(`Layer.DiffID`). Findings are stored per (slice, database build), the slice by its sha256: a match is
a function of the slice's bytes and the database alone, so every image naming that slice reads it. Only
each slice's newest is kept: a newer match replaces the older in its transaction, since the read reads
the newest alone. An image's new matches are stored together or not at all. Each tick also reads the
running images of such a project that have no finished scan — deployed before the rule was turned on,
say — on the `image-sbom` queue behind any deploy's (§3.1) — and drops the scans of images nothing runs or keeps, a deleted project's, and the slices no scan
names. A tick that starts an interval late does nothing; each scan waits on a busy executor for 15
minutes at most, and an image read that still finds it busy stores `busy` in place of nothing (amended
2026-11-27, #264): its read says `could not check: Canyon's scanner was busy with other apps, and checks again on its next round` until the
next deploy or tick reads it, and it never passes; and the scans matched longest ago go first, so no project waits behind another's.

**The named run image is matched too** (#2375, 2026-10-19), so the plan (§4.2) can tell a run image
that carries a fix from one that does not (§0.4). Each tick asks `GET /builder` for `run_image`, has the
executor read that image (contracts/executor.md, *The run image*) while no finished scan of its digest
is stored, keeps the scan under that digest — no project's — and drops any other run image's. Its layers'
slices are stored by content beside the apps' and matched each tick as theirs are: a run image's `os`
slice is the same bytes as the matching slice of each app on it. An executor that names no run image
drops every run image's scan, and one missing a layer is read again. No finding is made of it; the plan
alone reads it, only for the digest `GET /builder` answers now, and plans no `rebase` while it is
missing, did not finish, or its newest match failed or is past the 5 days.

**An environment's findings are a join in Postgres**: its digest's layers against each layer's
newest findings. **Cost is O(distinct platform layers)**: a fleet on two run images and four Node
versions re-matches about a dozen layer sets per tick, whatever the number of apps. A project that
places `deps.known-vuln` before production adds its images' own app layers, each matched every tick as a
platform layer is, since a match over 5 days old is never read: O(distinct platform layers + those apps'
images), the price of counting what only the built app holds (#2355). Another project's app layers are
not matched.

**Stale or failed.** A re-match answering anything but its report — any `not_finished` (`db_stale`
over 5 days, `db_missing`, `timeout`, `killed`, `no_image`, `not_sarif`, `over_cap`), or any status
but 200 — stores nothing, and marks each image it covers: its read says `could not check: its
last rescan did not finish` until a later match of all its layers, never what they matched before.
Every environment it covers reads `could_not_rescan`, and the
rule reads `could not check` (§3.3). After two failed ticks it is in front of admins on Settings ›
App hosting. The app layers a project matches for `deps.known-vuln` alone (§3.6), those holding no package of
a platform class, are matched apart: one that does not match marks only that rule's read of the built app,
never this rule's read, an environment's `could_not_rescan` or the patch plan. A patch run judges its two
images on one database over the platform layers it matched alone.

### 3.3 The rule: `platform.known-vuln`

**"No known-vulnerable system libraries or build tools"** (renamed 2026-11-01, #2079: it said *Canyon's layers*), placed **before production only**,
like the go-live rules (snapshot.md), because `check` never builds.

- **At promote time it reads the newest stored match for the digest preview runs.** That match's
  database must be within the 5-day staleness limit. Otherwise the row reads `could not check:
  trivy: <reason>` (EXPERIENCE D5) and blocks wherever the rule blocks. A rescan therefore moves the
  receipt without writing a verdict, and a days-old scan can never pass a version.
- **It counts only findings Canyon can fix**: class `os`, `runtime` or `lifecycle`, with a fixed
  version published. An `unknown` finding, or one with no fix yet, is a note under the row
  (`3 notes: no fix published, or Canyon can't tell what put it there`, proposed). A rule that blocks
  on what nobody can fix would block every promotion at once, and the way out would be N teams
  approving a CVE they cannot judge.
- **Its findings** follow seam 1's profile with one addition. `artifactLocation` carries `uriBaseId:
  "IMAGE"` and the path inside the image. The region spans line 1 from column 1 over the snippet, `<name>
  <version>`, composed by Canyon. The fingerprint hashes the rule, vulnerability, package, installed
  version and class, so an approval covers that CVE in that version across rebuilds. `dependency`
  gains `layer`. `fix` is the class's (proposed): `Canyon fixes this: it updates <name> to <fixed>
  under the app. The app is not rebuilt.` · `Canyon fixes this in its next build tools update, with a newer
  <buildpack>.`
- **The rule runs while it is on and `sarif.trivy` has `run`.** An app is patched (§4.5) only while
  its own effective rule is on, an owner's addition included: nothing is scanned for it to act on
  otherwise, and its patch approval reads `outdated` once it turns off (amended 2026-10-29, the security
  review of #2572). The org's `patching` is its switch alone.

### 3.4 Enterprise sources

Trivy is the default. An org that already scans images (Wiz, Prisma Cloud, Snyk, Amazon Inspector)
attaches that tool's SARIF **per image digest** through the report-attach machinery packages use
today: `PUT /api/v1/projects/:id/images/:digest/reports`, with the same body, limits and `sarif.<tool>`
registration. An admin then adds it as a source: `platform.known-vuln`'s `settings.source:
"<tool>"`. **An added source: Canyon's own scan always counts** (amended 2026-11-03, #2630's review: it said "names it
the rule's source", and as the rule's only input an empty report hid what Canyon found).

Its results are the rule's input beside trivy's matches, never in their place:

- A result counts when the tool names a fixed version.
- Its class is `unknown` unless the tool's result names a layer digest Canyon can place.
- Patching (Phase 2) plans only from trivy's classified matches, because a rebase needs to know the
  layer.

**Built (#2077, 2026-11-03).** A result names its package, fixed version, layer and purl under
`properties.canyon.package` (contracts/finding.md, *An imported image result*), which an org's export maps its tool's
fields to; Canyon reads no vendor's prose. The rule counts the union of Canyon's matches and the newest report of each of
the tool's categories, each finding once, and passes only when neither counts anything; it could not check when either
cannot — the matches as ever, or no report of the image, or any category's newest over 5 days old. A result in a layer
Canyon can't place is a note (`unplaced`), never clean. The read's age is the older of its database and its reports.
Canyon's own image scan still places every layer. A per-vendor adapter waits for an org to ask.

### 3.5 Testing — Phase 1

- **Red-first unit tests**: plant `npm-install` as a runtime provider and the class test fails; a
  launcher layer's Go `stdlib` CVE reads `lifecycle`, never `app`; a match on a 6-day-old database
  reads `could not check` and blocks; a newer match on the same digest moves the receipt; an
  unfixable finding is a note; a `trivy image` → `trivy sbom` round trip keeps `LayerDiffID`.
- **#2216** (PR 2's deferred listing) builds Paketo's Node, Python and Java samples on the amd64 e2e
  job, pastes each SBOM by layer into its PR with the Java database's behaviour, and keeps §2's table
  checked: each sample's packages are asserted in the class the table gives them.
- **Docker E2E** (`make e2e-up`, `make e2e`): claims-portal built with `CANYON_IMAGE_RUN` naming a
  canyon-run made from an **older pinned Paketo noble digest** — fixed USNs stay in the database
  forever, so the real database through the `CANYON_SCANDB_TRIVY` mirror yields a deterministic
  `libssl3` finding. Assert the platform row names it, class `os`; a stale database image makes it
  `could not check`; `CANYON_PATCH_INTERVAL=1m` re-matches with no `docker save` among the events of the daemon
  the executor drives (the executor logs no command it runs; amended 2026-11-01, #2081).
- **AWS EKS test cluster** (`make e2e-cluster`, a case in e2e/cluster/CASES.md): the ClusterStack on the same
  older run image; the same assertions, plus the SBOM referrer in ECR and no image read by a rescan;
  the Playwright cluster project sees the card as a contributor and nothing as read-only.

### 3.6 What only the built app holds: `deps.known-vuln` before production

`deps.known-vuln` reads the package's manifests and lockfiles at review, and the review never builds. Three
things put a dependency in the image that no lockfile names (#2355): a build step that downloads it (an
npm postinstall, a pip build, a Maven plugin fetching a jar into `/workspace`), a Maven app whose parent or
BOM is remote, whose inherited and transitive versions the offline review scan cannot resolve, and a
buildpack outside Canyon's builder, as the app's own `project.toml` can name. The first two class `app`,
the third `unknown` (§2). **Each is the team's to fix, like any dependency, and `deps.known-vuln` counts
it** (decided 2026-11-01, the owner). No build is refused for its buildpack.

- **Where it is judged.** Only the image holds them, and the image exists after the build, so they are
  judged where `platform.known-vuln` is: on the promote receipt, when it is read, on `deps.known-vuln`'s
  row while the rule is placed before production (snapshot.md, *Two gates*). It reads the image the
  receipt is for, as the platform row does. A rule placed only before preview reads no image: `check`
  never builds.
- **What it counts.** Each package of class `app` or `unknown` in the image with a vulnerability at
  `high` or above — a severity Canyon cannot read is `high` — whatever its fix, as the review counts a
  dependency. A package the run's own `deps.known-vuln` findings name already is not counted again: the
  same package type, name and version (contracts/finding.md, *The built app*). Canyon can tell only what
  the review reported, never what a lockfile names, so its words say "its review didn't see it", and a
  package a lockfile names that had no advisory at its review and has one now is counted too, on the row
  before production, until the app is reviewed again: the next review reports it, and it is a dependency
  finding as any other (decided 2026-11-01, #2613's review). A package trivy names no layer for is never
  matched, so it is not checked: named in the row's `why`, never counted.
- **Fail closed.** The image's scan and the newest match of each layer holding such a package are read
  as the platform row reads them: no scan, a scan that did not finish or does not read, a layer not yet
  matched, a match over 5 days old, a rescan that did not finish, the platform layers' or the app layers'
  own — each reads `<version>'s built app
  could not be checked: <reason>` and the row does not pass. An image that could not be checked is never
  a pass for this rule either.
- **What it costs.** The image is scanned while `platform.known-vuln` is on or `deps.known-vuln` is
  placed before production (§3.1), and the app layers of such a project are matched each tick (§3.2).
  An org that places only `deps.known-vuln` waits the image scan's length before its first promotion.
  Placing it asks at once: an apply that places it before production with no grace — the org's policy or
  an app's addition — asks `image.sbom` of every image a project it places it for runs or keeps, as a
  deploy asks but behind any deploy's (§3.1), and an image whose scan finished has its unmatched layers matched. An edit with a grace asks
  nothing when it takes effect, as no edit does: its images are read and matched by the next rescan tick, up
  to `CANYON_PATCH_INTERVAL` (6 hours by default) after the grace ends, or by the app's next deploy.
- **Approved as a blocking finding is** (decided 2026-11-02, #2610). None of them is in a run's findings,
  so each advisory of one has a fingerprint of no path, layer or digest (contracts/finding.md, *The built
  app*), and the dependency's approve files it under `deps.known-vuln` for the run the receipt is read for:
  an admin or a responder who didn't start that review, from a browser (design/PERMISSIONS.md). The row
  counts a package until approvals cover every fingerprint of it, then names it as approved.
- **The platform rule is unchanged**: an `unknown` package is still its note, never counted there, and
  an `app` package is still neither its finding nor its note.

## 4. Phase 2 — Fix OS packages by rebase

Phase 2 runs only once the operator turns it on: `CANYON_PATCHING` `on`, unset `off`, beta (#2098). Off, the
rescan still reads and matches every running image, and nothing is planned or patched.

### 4.1 The run image is named, never brewed

- **One model on both executors.** The run image is the release's (`canyon-run` from
  executor/run.Dockerfile, its Paketo base moved by Phase 3's CI) or the operator's: **
  `CANYON_IMAGE_RUN`**, a golden image on the stack (`io.buildpacks.stack.id` checked as for
  `CANYON_BUILDER`). On Kubernetes it is the ClusterStack's run image.
- **No daily apt rebuild.** Docker builds `canyon-run` from the file only, through
  `CANYON_RUN_APT_MIRROR` when set, so an air-gapped host builds it at all.
- **A new run image keeps its name.** The lifecycle's rebaser checks the name the app image's
  metadata gives, `runImage.image`, else `stack.runImage.image` as kpack 0.18's builds (platform API
  0.9) write it — repository and tag, any digest stripped — and Canyon never forces
  it. deploy/k8s/README.md says so. On Docker pack is handed `canyon-run:noble`, a stable name the
  executor moves onto each build of the file, beside the `canyon-run:<hex>` tag that says the file is
  built; a file moved to another Ubuntu release takes another name, so a rebase across releases is
  refused. Images built onto `canyon-run:<hex>` before the name are never rebased: they move only by a
  rebuild (#2089, 2026-10-18). On Kubernetes the name is kpack's copy of the ClusterStack's run image,
  `<the ClusterBuilder's spec.tag>-run-image`: the ClusterStack's run image may move to any
  repository, and a ClusterBuilder renamed or retagged strands every image it built (#2091,
  2026-10-19).
- **The run image stays out of the build digest.** A reused image on an older run image is reused as
  today; if it carries a counted `os` finding, the next tick plans its rebase. `GET /builder` answers
  `{builder, run_image}`. `run_image` is for planning, never the digest.

### 4.2 The plan, and the fleet's brakes

- **Threshold.** The plan considers only counted findings at or above `high`, the severity at which
  the rule can block. Ubuntu's low fixes never restart an app.
- **One platform identity per target run image digest.** Each identity rolls out in waves: a canary
  (one app's preview), then 5, 25, then the rest. A wave starts when every watch of the last one has
  run to its end with no revert: a watch a person's request ended early proves nothing. When that is
  the canary's, the next tick names a **replacement canary**, the next app in canary order with a
  patch planned, and the wave waits on its watch; the first app files no patch approval for that
  patch (its watch proved nothing) and is never the canary of that identity again (review of #2340,
  2026-10-18). In a later wave such an app is left out of the count, and the wave goes on. One revert short of the breaker **stalls** the identity until an admin
  resumes it, which leaves that app out. Apps are ordered by a hash of their id, so the canary is stable
  across ticks (review of #2340, 2026-10-18).
- **Concurrency.** At most `CANYON_PATCH_CONCURRENCY` patch runs at once: 1 on Docker, 2 on
  Kubernetes. **A watch holds no slot** (#2567): a patch run's slot frees once its `/run` is recorded, and its
  watch, which neither rebases nor scans, runs on as a workflow of its own on a queue with no limit. Image scans and re-matches count against the executor's `CANYON_EXECUTOR_SCANS`, which
  stays at or above `CANYON_SCAN_CONCURRENCY` plus `CANYON_PATCH_CONCURRENCY` plus the `image-sbom`
  queue's one and the rescan queue's one (contracts/executor.md, *Busy*), and rebases count against `CANYON_EXECUTOR_BUILDS` beside
  `CANYON_BUILD_CONCURRENCY`: the executor's defaults rise with PR 13 so they keep both sums (review of #2340, 2026-10-18).
- **Circuit breaker.** Two reverts or failed starts within one identity **pause it fleet-wide**.
  Admins see `Canyon paused patching with <image>: 2 apps reverted after the patch.` (D5),
  and an admin resumes it in the browser, `POST /api/v1/patching/identities/:id/resume`. Admins read
  every identity — its run image, its wave, whether it is paused or approved for the fleet — with
  `GET /api/v1/patching/identities`, which Settings › App hosting draws (§8). The org-wide pause is
  `patching: off`. (Both routes decided 2026-10-18 with the seam, #2086.) Resume clears a stalled
  identity too, and a pause, like `patching: off`, turns the identity's pending patch approvals
  `outdated` (review of #2340, 2026-10-18). A resume makes the identity's unexpired pending patch approvals current
  again, and under a fleet approval the next tick decides them (#2571).
- **Production held or stopped** (OPERATIONS.md §3.4) is never planned. Neither is a stopped preview.
- **A rebase refused for what the images are is never planned again** (#2089, 2026-10-18). A `409
  another_run_image` or `another_stack` is permanent for that pair — the image's digest and the run
  image the plan named — so the plan skips the pair until the environment runs another image or the
  run image moves; such an app is left out of its wave's count, as a stalled one is. Every image built
  onto `canyon-run:<hex>`, or before `CANYON_IMAGE_RUN` was set, unset or moved to another repository,
  answers so, and the next deploy is what patches it. `not_rebasable` is the same. `run_image_moved`
  is not: the plan is simply stale, and the next tick plans again.
- **Each tick says what it planned** (#2575): one server log line naming the run image, the apps whose
  patch runs started, and every other app it read under one cause each: the rule off, no counted
  finding at `high` or above, none the run image fixes, a refusal for good, a revert or failed start
  not retried, its wave, a pause or a stall, a person's run first. Before it reads an app the line says
  why it read none: patching off on the server or for the org, no app with `platform.known-vuln` on, or no preview. A plan that fails says
  why in its own line. Names and counts only, never an image's contents; an app given no cause reads
  `for a reason the plan does not name`, never nothing to do. The newest tick's causes are kept, and
  Settings › Patching says them beside the identity it planned onto (#2588; contracts/routes.md, the
  identities row's `plan`): a tick that fails, in its plan or before it, keeps none, so no cause outlives the plan that gave it.

### 4.3 The patch run

A run of kind `patch`, actor `{kind: "canyon", subject: "patch", display: "Canyon"}`, on the package
the environment runs, on **its own DBOS queue `patches`**. It starts only while no person's run of
the project is queued, so **a person's run always goes first**.

**A person's or the team's request** — a deploy (a GitHub push under `auto_deploy` included), a
promotion, an undeploy, a stop — **cancels a patch run at any step before its `/run`**. This is a
`StartTx` change: an active `patch` run is ended `interrupted` the way a pending run is today. During
the `/run` itself (the executor's settle window) the request answers `409 run_in_progress`. After it,
the request ends the watch clean (§4.4), and goes ahead — a promote run shipping a patch is treated
the same while it watches production. An undeploy or a hold ends only a patch run of the environment
it takes offline (review of #2340, 2026-10-18).

**Its author is the author of the run whose image it rebases**, for every rule that never lets an
author approve their own finding (PERMISSIONS.md, *Approve findings*): `started_by` says Canyon
started it; the author it records is preview's run's, so no one approves, through a patch run, a
blocking finding of code they shipped. PR 14 records it, and the approve routes and what an approval covers read it (review of #2340, 2026-10-18;
#2098).

**A patch run never sets the fleet's `gate_state`**: `fleetDerivation` skips every run of kind
`patch`. It is not the receipt's run unless preview runs the image it deployed (`promote_run` skips
kind `patch` otherwise), and the receipt's newest run of the package — `prod.clean-gate`'s, and a
rollback receipt's newer review — skips it the same way (review of #2340, 2026-10-18). An app with a
held patch is not shown as `failed` on the fleet. (Decided
2026-10-18 with the seam, #2086: this said `fleetDerivation` skipped only a patch run that had not
deployed, against the sentence before it.)

**Steps**, each ending the run with nothing deployed when it fails:

1. **The same gate** on that package, exactly as a deploy runs it: today's snapshot, every tool, the
   AI verdict cache, the project's unexpired approvals. `blocked` ends it `blocked`, state `held`.
2. **Rebase to a fresh tag**, `POST /rebase`. On Docker the image is retagged and `pack rebase` runs
   with `--run-image <named> --pull-policy never`, so the pre-patch image keeps its own tag. On
   Kubernetes it is **kpack's own rebase**: a `Build` with `spec.lastBuild` and the `STACK` reason,
   its `spec.runImage` the planned run image. kpack 0.18 rebases a one-shot `Build` so (#2091, read
   from its source: CI's kind runs no kpack controller), so the lifecycle's `rebaser` as a Job is not
   taken; kind proves the Build's shape and the refusals, the AWS test cluster the rebase. kpack's rebase opens
   the fresh tag before it writes it, so the executor writes it first, onto a placeholder of the
   pre-patch image's manifest that only the fresh tag names (#2576).
   `io.buildpacks.rebasable=false` is refused, never forced.
3. **The new image's SBOM** (§3.1), its layers and the replaced image's matched at the step on the
   executor's database, and both read on that one database (#2098): the
   `progress` step `image-sbom`, beside `rebase` and `watch` (decided 2026-10-18 with the seam, #2086).
   A match that did not finish, or a stale database, fails the run: it is never read as no
   findings (review of #2340, 2026-10-18).
4. **The proof that only the run image changed** — `proof: "layers"`: the diff IDs above
   `runImage.topLayer`, in order, `io.buildpacks.build.metadata`, and the config's `Entrypoint, Cmd,
   Env, WorkingDir, User` are identical, on the same stack id, and the new run image is the one the
   plan named — its digest the identity's; another fails the run, nothing deployed (review of #2340, 2026-10-18). The executor
   reads the facts; the server compares.
5. **What it would run with is unchanged.** Each deploy row records a **run-config fingerprint**: a
   sha256 over the row's recorded egress spec, `via` (EGRESS.md §2.5: the mode is live policy, never
   pending), `run_by`, the front door its `/run` carried on Kubernetes (review of #2340, 2026-10-18),
   that environment's runtime secret names, versions and set times, and its resources' names, which the row records
   beside it so a mismatch names what changed (#2094, 2026-10-19). The fingerprint at the patch's `/run`
   must equal the row's.
   Otherwise the run fails `not_platform_only` with `config_changed`, and the page says: `<what>
   changed since <version> was deployed: deploy again to apply it, then Canyon patches.` (proposed).
   A patch never carries a pending configuration change into a running app.
6. **Regression.** The new image has no counted finding at or above `high` that the old one lacked, the
   two read at step 3 on one database — a database update since the plan raises no false regression —
   and no longer has at least one finding the plan named. Otherwise it fails with
   `patch_regressed` or `patch_ineffective`.
7. **Deploy** (§4.4).

New `failure_reason`s: `not_platform_only` (with `config_changed` or `layers_changed`, carried in
the run's field `not_platform_only`), `patch_regressed`, `patch_ineffective`, `reverted`. Each step's
result is a `build.log` line, the proof's among them, before the patched image's `/run`
(contracts/events.md).

### 4.4 Preview, the watch, the revert

`/run` of the patched digest with `keep: [<previous digest>]` (contracts/executor.md, *What `keep`
spares*; OPERATIONS.md §1.5 says what the server keeps). `/run`'s own check is the first health check: up after 10 s on port 3000, else the old
container is handed back.

**The watch** lasts `CANYON_PATCH_WATCH`, with `watch_until` journaled so a resumed run waits for
the same instant. It reads the container's state every 30 s, idempotently, with `POST /running` of that
one container: `GET /running` answers no restart (contracts/routes.md, *Patch runs*, amended 2026-10-29,
#2100). **Its only signal is an exit or a restart of the patched container** — `answered` is one-shot by contract, an app behind sign-in
rightly answers `302` or `401` at `/`, and a health probe is a seam of its own (§12).

**Revert**: the run moves to `reverting` by compare-and-set under `FOR UPDATE`, so a person's request
and a revert never both `/run`; one transaction re-reads the hold and checks the deploy row is still
the patch's, then `/run`s the kept digest; audit `revert patch <env>`; the run ends
`failed`/`reverted`. It holds the undeploy lock across its checks and its `/run`, as *The brake*'s
writers do, so a hold landing meanwhile waits and then stops what it put back. The row returns to
what the patch replaced, image, gate run and fingerprint; a row a person's deploy replaced first is
left alone and the run ends `clean`; a kept image that is gone, or a `/run` that fails, leaves the
patched app running and ends the run `failed`/`error`, each with its own line (contracts/routes.md,
*Patch runs*) (review of #2340, 2026-10-18).

It is a **revert**, not a rollback (OPERATIONS.md §1): it puts back the digest that served 15
minutes ago, after a failure Canyon caused, and never reaches an older version.

**A watch ends clean** — no revert, `keep` released — on a person's request, a stop, an undeploy, a
hold, or its end. A watch that cannot read the state does not pass: no production step follows, and
nothing is reverted blind: a read that fails is read again at the next tick, as a restart is counted from the
`/run` and an exit stays, and only the read at `watch_until` must answer. A recreation of the app's network
starts no run, so while a watch runs it is `409 run_in_progress`, as during the `/run` (#2100). A patch run
whose watch ended clean ends `clean`, as a deploy run does, its
last `build.log` line saying what ended the watch; `reverting` is the run row's own mark, never a
run state (decided 2026-10-18 with the seam, #2086).

The deploy row keeps `package`, `label`, `deployed_by` and its buildpacks, and takes the new
`image_digest`, `gate_run` (the patch run, which says Canyon patched it) and the fingerprint; its
`build_digest` becomes none, as no build made the image (a build digest covers the image, so a redeploy
builds or reuses the image the build made). Its `values_hmac`, `values_key` and the builder that built the
image stay, since a rebase changes no build value (amended 2026-11-02, #2117: they became none, which left a
rebased app no rebuild patch, §5). The provenance stays D5's `<version> ·
passed review`; beside it, proposed: `patched by Canyon <date>`.

### 4.5 Production — asked

**`patching` ∈ `off | ask`**, default `ask`, on the preferences row. It is admin-only, set from a
browser session (as `prod_approval` is), and audited before and after. With sign-in off nobody can
decide a patch approval, so `patching` reads `off` there and a `PUT` of `ask` is `409
signin_required`, as `prod_approval`'s is (decided 2026-10-18 with the seam, #2086). Under `ask`:

- **Production is patched only with preview's patched digest**, when production runs the image
  preview ran before the patch. Invariant 4 holds, and D5's `Production runs the same image as
  preview, with production's own secrets. Nothing is rebuilt.` stays true.
- When preview is ahead (production v1.3, preview v1.4), production is not patched. Its state reads
  `held` with `promote v1.4 to take the fix to production` (D5). The alternative is deferred
  (§12).
- **The patch approval is a production approval with `kind: "patch"`** — a label, the fixes, and the
  proof text. It has the same code, routes, browser-only decision, contributor-and-above level,
  24 hours and `outdated` rule. Approving starts the promote run for the patch run, which re-`/run`s
  preview's digest and **watches production** as §4.4 does. It records the production image it
  replaces, and turns `outdated` once production runs anything else or nothing — a rollback, an
  undeploy — so a patch never ships over a person's rollback (review of #2340, 2026-10-18). Only the
  promote run a patch approval started watches, and only after Canyon's own `/run`: a person's promotion
  of the patched preview is a person's, and production the org's platform runs is §12's. Its revert is
  the app's outcome `reverted` in the identity's waves, as a preview revert is (§0.3, §4.2): admins are
  told, and it counts toward the breaker. Before its `/run` it reads production's row again, and one
  that no longer runs `replaces_image` is never shipped over: the run ends, nothing started (#2570).
- **One open patch approval per environment.** A newer patch's approval supersedes it. A patch
  approval **never supersedes a person's**: none is filed while a person's is pending, and a
  person's request deletes a patch approval, as today. Each tick re-files one while the plan stands
  and none is open, so a 24-hour expiry is never the end. A team's denial is: nothing is re-filed
  for that app and identity, and the fleet approval never decides for it (review of #2340, 2026-10-18).
  So is a production watch that saw the patched app exit or restart, reverted or not: its outcome
  `reverted` leaves `passed`, so nothing is re-filed for that app and identity, and a newer identity
  may ask (decided 2026-10-29, #2570).
- **Held production is never patched**: no approval is filed, and approving one is `409 held`
  (OPERATIONS.md §3.4).
- **Its digest is kept** until it is decided, expires or turns outdated.
- **Fleet approval** — platform-class only, and a change to PERMISSIONS.md first, the first commit of
  the PR that writes its route (PR 9, rule 5). An admin, in the browser, approves one platform
  identity for production across the fleet: `POST /api/v1/patching/identities/:id/approve {reason}`.
  Each app whose preview patch passed its watch is then promoted by Canyon in that identity's waves,
  with `approved_by` the admin. The receipt's `prod.human-approval` reads that person. Teams can still
  decide their own app first. It never expires and covers every later wave of that identity; it is
  not withdrawn in v1 — an admin stops it with `patching: off`. It decides each app's pending patch
  approval as it comes due, `decided_by` the admin, and skips a held production (decided 2026-10-18
  with the seam, #2086; a product choice flagged to the owner). Each decision runs the approve route's
  checks under its lock with no `promote_anyway`, so a preview answering an error leaves it pending for
  the team, and only while that admin is still an admin: one demoted or gone leaves each due approval
  pending for the team (#2571); its audit row is `approve promotion`, actor the admin, via `canyon`. A
  stall holds back only the next wave: the fleet approval still decides the apps already due (#2571). An app with a
  scheduled job keeps its own decision (§4.6) (review of #2340, 2026-10-18).

### 4.6 Jobs

A scheduled job on an app (JOBS.md §"Patching by kind") differs in three ways: its production
patch is **always asked**, even under a fleet approval; its evidence is keyed on the image digest (no
live container to watch between runs); the tag cleanup keeps every image a deploy row names.

### 4.7 What it leaves behind

- **Audit rows** (actor Canyon unless said): `patch preview` and `revert patch preview` (outcome);
  `patch prod`, `revert patch prod`, `request patch approval`, `pause patching`, and an admin's
  `resume patching` and `approve patch identity` (governance). Detail `{package, run, replaces,
  image, proof, fixed[]}`: names and versions, never content.
- **Change management**: the org's event stream is the hook. `GET /api/v1/timeline` carries
  `run.transition` (with `kind: patch`) and `audit.appended`, which an ITSM bridge polls with its own
  session. Canyon sends nothing outbound.
- **Who hears it**: the app page, `canyon status`, the next `canyon deploy`, the fleet's `needs_you`
  (a pending patch approval), the audit trail, and Settings for admins.

### 4.8 Testing — Phase 2

- **Red-first unit tests**: a changed `Entrypoint` fails `layers_changed`; a secret version bumped
  since the deploy fails `config_changed`; a new `high` fails `patch_regressed`; a person's deploy
  cancels a patch run at the rebase step and `gate_state` is untouched; a request racing a revert
  yields one `/run`; a resumed run watches until the journaled instant; two reverts pause the
  identity; no patch approval is filed over a person's or for a held production.
- **Docker E2E** (`CANYON_PATCH_INTERVAL=1m`, `CANYON_PATCH_WATCH=1m`): from an older digest of one
  `CANYON_IMAGE_RUN` repository, move it to a newer digest of the same repository (another repository,
  or setting or unsetting it, is another name: `another_run_image`, §4.1); assert preview's digest changed, `patched[]` shows the
  `libssl3` bump and the diff IDs above `topLayer` are equal; approve through the Playwright
  production-approval journey (`make e2e-ui`); production's digest equals preview's. **Revert**: an
  e2e-only run image carrying a marker file makes a fixture app exit ~45 s after start (after
  `/run`'s settle) — assert `reverted`, the old digest running, `revert patch preview` on the trail;
  a second such app trips the breaker.
- **AWS EKS test cluster**: the same, the run image swapped by `kubectl patch` of the ClusterStack (the
  ClusterBuilder unchanged), waiting for the ClusterBuilder; assert a kpack-built app is not refused (its metadata
  names the run image at `stack.runImage.image`) and that kpack's rebase landed on the planned run image
  with the layers above it identical; a
  responder's stop mid-watch ends it clean and nothing restarts.

## 5. Phase 3 — Runtimes

- **Platform bumps by CI.** A scheduled workflow resolves Paketo's newest digests for the builder,
  the build and run bases, and the ClusterStore sources, each at the version the newest builder
  carries. It rebuilds, takes the SBOMs of the fixture apps' images on old and new, and **opens an
  issue with the scan diff and a branch a person opens the PR from** (`libssl3 …→…`,
  `npm 10.8.2 → 10.9.x`): Actions cannot open pull requests in this repository by design, and a
  person's PR runs its checks as any other (decided 2026-11-02, #2114). An open issue for the same pins is updated, never
  a second. This is how Node and Python security releases, which trivy cannot see (§2), still reach
  Canyon on a schedule, except for the Python of an app built with uv (below). An operator moves faster with
  `CANYON_BUILDER` and `CANYON_IMAGE_RUN`.
- **A uv app's Python is the one uv downloads** (#160; decided 2026-11-27, #210: uv's download stays). uv chooses and
  downloads CPython itself during the build, from outside the pinned images: the newest its own version knows that the
  app's `requires-python` or `.python-version` allows (3.15.2, where a pipenv app on the same builder got the builder's
  3.11.18). The pins install no CPython for a uv app to use instead: Paketo's uv group takes the `cpython` buildpack
  only for an app with a `vendor` directory, and the build image has no Python of its own, so with uv's download off
  uv would have nothing to build on. So a bump does not move that Python, and a rebuild brings its patches: a rebuild,
  of unchanged code too, gets the newest release uv knows within what the app allows.
- **Which .NET line an app builds on is the app's own** (#159): the `TargetFramework` its project file names. Paketo's
  default line (`8.*`) is used only where nothing names one, so for an app built from a project file it decides
  nothing. Canyon builds every line its pins carry, one past its vendor's support date too (.NET 8 and 9 since
  2026-11-10; by the pinned buildpacks' dates, also Node 18 and 20, Python 3.10 and PHP 8.3), and a bump brings such a
  line nothing more. Canyon's skill tells coding agents to start a new app on a version its vendor still supports,
  .NET 10 for .NET; a finding for an app on a line past its date is #211.
- **Rebuild patches** — opt-in, **always asked**, never called platform-only (built 2026-11-02, #2111, #2117;
  contracts/routes.md, *Patch runs*, *A rebuild patch*). The org's **`rebuilds` ∈ `off | ask`**, `off` by default,
  is set as `patching` is: an admin's, from a browser session, audited, and read `off` while `patching` is. Each
  rescan tick, after the rebase plan, plans one for an app with no rebase planned whose preview's image has a counted
  `runtime` or `lifecycle` finding at or above `high`, when the builder Canyon names now (`GET /builder`) differs from
  the one that built the image. Every build records its builder beside its buildpacks, and the builder travels with
  the image as they do, kept by a rebase; an image whose build recorded none, or no values-only HMAC, is never
  planned. Its identity is the builder's digest, `kind: "rebuild"`, in waves, with the breaker, the watch and the
  revert of §4.2 and §4.4; at most one rebuild patch runs on a host at once, and its build waits for a build slot as a
  deploy's does. The run is §4.3's with its step 2 a **build** of the package preview runs, with preview's build
  values and no reuse, and its step 4 the proof **`same_inputs`**, the first difference ending it
  `not_same_inputs`: the same package (tree hash) — the package's lockfiles are in its tree, so the tree hash is the
  lockfile's digest and no digest of its own is stored; the same **values-only HMAC** under the same key version —
  stored with its key id on every build from Phase 1 on, since the build digest folds in the image and builder and
  keeps only the digest — one made under another key version is never compared (`key_rotated`); the same buildpack
  ids in order; and the two SBOMs' own packages identical — every library but Canyon's classes (`os`, `runtime`,
  `lifecycle`), so class `app` and `unknown` both — which is what refuses an unpinned `requirements.txt` that resolved
  anew. A rebuild that fixes none of its findings deploys nothing
  (`patch_ineffective`). Preview's row takes the new build's digest, values-only HMAC, buildpacks and builder.
  Production is asked of the app's team, every time: the fleet approval never decides a rebuild identity (`409
  always_asked`). The approval says: `Rebuilt from the same code and the same dependencies with a newer Node.js. A
  rebuild is a new program, not a copy of what production runs, so a person on your team decides.` — the runtime
  named from the layers of the packages it fixed (Node.js, Python, Java, Go) when they are one runtime's, else
  `newer build tools`, never a guess, since Canyon cannot read Node's or Python's own version (§2). `rebuilds: ask`
  is refused while `patching` is `off`, and turning `patching` off turns `rebuilds` off with it.
- **Testing**: red-first, an unpinned `requirements.txt` that resolves anew fails `same_inputs` and a
  rotated key id refuses to compare; a CI dry run of the bump against an old pin; on the EKS test cluster a
  rebuild patch is asked, approved by Playwright, and production gets preview's digest.

## 6. Phase 4 — The patch buildpack, spike-gated

**What it would do.** `canyon/patch` is a CNB buildpack Canyon writes, appended `optional` last in
every order group. It reads the installed version of a bundled package from that package's own
manifest with a real parser. When the version is in a catalog entry's affected semver range, it
replaces that tree with the fixed release (for example npm's own dependencies, staying within npm's
major). It fixes runtime-class findings Paketo has not shipped yet, on Canyon's timeline.

**The spike** is a branch and an issue report, never merged. It passes only if **every** criterion
holds, demonstrated on Docker and kind:

1. **Cache-safe.** Cached and uncached builds of the same package produce identical patched results.
   This includes a runtime layer restored from cache, and one that is launch-only with `cache=false`.
   The same holds through pack's per-app volumes and kpack's `:cache` tag.
2. **Cold on change.** The builder and patch-buildpack identity is in the cache's name or tag, so a
   catalog change means a cold cache, and reverting an entry takes effect at the next build.
3. **SBOM truthful.** The image's SBOM, Paketo's layer SBOM included, is rewritten to match what was
   patched. Trivy on the result shows the new version and not the old.
4. **Reachable.** The artifact is fetched by pinned integrity from a configurable mirror
   (`CANYON_PATCH_MIRROR`, Artifactory or Nexus), with a build-egress exception the way
   `CANYON_KUBE_DB_CIDRS` makes one. Air-gapped hosts work through the mirror, or the entry reads
   `not applied: no mirror` (never silent).
5. **Built locally.** The buildpack is built from sources embedded in the executor, as `canyon-run`
   is: no GHCR pull while images are private.
6. **Fixtures.** Must-fail and must-pass fixture apps per entry, and the must-fail one shows the
   finding gone on a cached rebuild.
7. **The CNB question answered.** Writing inside another buildpack's layer works on today's pinned
   lifecycle on both executors; the report says whether the spec allows it.

**Ships only if it passes**: the buildpack, its catalog and `make patch-buildpack`, reproducible,
with CI comparing the digest. Then wiring into both executors' order, with the identity in
`GET /builder` and the cache name. Then the `catalog` plan outcome, always asked (it is a rebuild).
If the spike fails, Phase 3's bumps are the answer and §12 records why.

**Testing, if built**: the spike's criteria become the PRs' red-first tests, plus an EKS case — a
Node fixture on a pinned old node-engine whose npm bundles a vulnerable package, patched, asked, and
approved.

## 7. The builder's own dependencies — out of scope

Canyon never changes an app's code or lockfile. A `deps.known-vuln` finding carries trivy's fields
and `fixed_in`, and `--format ai` hands the agent the bump. One constant line is added to that
advisory in the AI format (decided 2026-11-01, #2079): `This is in the app's own dependencies. Canyon fixes the
system libraries under the app, never these.`

The GitHub App now asks for `contents: write` for orgs whose code Canyon keeps
(github-app/manifest.go). So a bump PR is no longer a permission question; it is a product question.
ARCHITECTURE.md §6 and EXPERIENCE D9.1 cut patch PRs deliberately. If wanted, it is its own design.

## 8. The experience

**`canyon status`**, for an owner (lines marked `+` are proposed; the rest is cli.md §6.1):

```
  claims-portal · Meridian

  preview      v1.4   http://claims-portal.preview.localhost:7777   1d ago   patched by Canyon 2h ago
  production   v1.4   http://claims-portal.prod.localhost:7777      3d ago
+ platform     ! a fix is ready for production: libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5, app unchanged
+              approve it in Canyon: http://localhost:7777/#/approve?code=K7QF-2M9X
  access       shared with everyone at Meridian · you are an owner
  review       ✓ passed   0 blocking, 1 warning   2h ago

+ ! No known-vulnerable system libraries or build tools
+     libssl3 3.0.13-0ubuntu3.4 (CVE-2024-…) · Canyon is patching it · 3 notes
```

The `platform` line in each state (D5):

`none` `✓ checked: the system libraries and build tools Canyon adds · vulnerability data from 3h ago` ·
`wait` `· Ubuntu has a fix; Canyon hasn't picked it up yet` · `reverted` `✗ Canyon reverted preview at 10:41: the
patched app restarted` · `couldn't rescan` `! couldn't rescan: its vulnerability database is 6 days
old` · `held` `! held: v1.4 is blocked by deps.known-vuln` · `denied` `! Your team turned down Canyon's fix for production. Production
still has libssl3 3.0.13-0ubuntu3.4. Canyon will ask again when a newer fix is out.` (#2601, #2605). Below contributor there is no line.

**`canyon deploy` is unchanged**: the SBOM is taken after `Live`.

**`--format ai`**:

- A dependency advisory carries §7's line.
- When the platform rule blocks production, the step ends `…; Canyon is patching it. Do not change
  the app for it. Tell the user before they ask for production.` (proposed).

**Dashboard.**

- **App page**: a *System libraries* card for contributors and above (named 2026-11-01, #2079: it was *Canyon's
  layers*) — per environment, when it was matched and against which database; counted findings as dependency
  cards that say where each is, `system libraries` or `build tools`, never the class's own name; the
  notes; `Fixed by Canyon · <name> <version> → <now> · <vulnerability> · <place>`, one per package (`patched[]`, on the image read,
  `GET …/envs/:env/image`, decided 2026-10-18 with the seam, #2086); the §0.3
  state, with **Review the fix** while `asked`.
- **The approval page**, `kind: patch`: what changes, the proof (`The app's own files are byte for byte
  the ones in production.`, proposed), preview's watch result, then D5's confirm — a rebuild's its own, D5's
  `Production gets the image preview ran and Canyon watched, with production's own secrets. Approving doesn't
  build it again.`, as the promote confirm's `Nothing is rebuilt.` would contradict its proof (decided 2026-11-03,
  #2631).
- **Fleet: no new column.** The Preview and Production cells gain a second line for contributors and
  above: `patched by Canyon · 10 Oct` or amber `fix ready for production`. `needs_you` counts an open
  patch approval.
- **Settings › App hosting**, for admins: `patching` as `Canyon doesn't patch apps` · `Canyon patches
  previews and asks before production`; the run image and the apps behind it, by name; paused
  identities with **Resume**; **Approve for every app** on each identity with apps planned, patched in
  preview or asked, the only ones it can decide; each run image by 12 hex; identities with nothing left
  folded (#2582); and what Canyon's last check said of each app, in the card of the identity it checked
  against, or why it checked none (#2588).

**The known-vulnerable row in words (#2579).** "Canyon's layers" said nowhere where, and "Canyon can fix" beside a
patch read as the patch failing. The row's `why` now names where (`… with a published fix, in the operating system
and build tools Canyon adds`, contracts/routes.md); to a contributor and above each package carries its `layers`
and `go_stdlib`, so the checklist groups what it counts by what fixes it — a newer run image (`os`) or Canyon's
next build tools update (`runtime`, `lifecycle`) — names Go's standard library and each place, and lists each
package once with its versions (design/specs/screens.md). The patch approval says what remains after the patch and
whether production already runs it, by production's image read, and can't tell when that read fails. Neither names
the binary that carries Go's standard library: trivy writes no `FilePath` for a `gobinary` package and Canyon keeps
no dependency graph, so the layer's owner is the most the store says. `canyon status`'s `platform` row dates its
check by `vulnerability data from <age>`, the database's age.

**Built (#2105).** The approval page, the fleet cells, `needs_you`, Settings › App hosting and `canyon status`'s
`platform` row are as above, each state's line as design/specs/cli.md §6.1 and design/specs/screens.md say.
`wait` and `could_not_rescan` need the environment's image read and the named run image's match, so the status
read alone reads them (contracts/routes.md, `envs.<env>.patch`): the fleet cell and the app's project read never
show them, and `canyon status`, which reads status, does. The run image Canyon names is the one the newest rescan
tick read; one whose match does not read is never taken as nothing behind. `patched[]` is found by the image's
digest, so a rollback to an image a patch made names that patch, its `at` when the image last reached the
environment (the decision of #2556).

**Built (#2079).** The app page's *System libraries* card, for contributors and above, reads each environment's image
read: its `why` beside its mark (`✓` checked with nothing counted, `✗` `could not check`, else `·`), when the packages
were listed and the vulnerability data's age, each counted package a card naming where it is, its vulnerabilities and
Canyon's fix, the notes folded, and `patched[]`'s `Fixed by Canyon` lines. An app nothing was scanned for shows no card,
so an app whose rule is off reads as before. The §0.3 state is the environment cards' (*Canyon's patches in the cells*),
and **Review the fix** stays with Phase 2. `canyon status`'s `platform` row reads `✗ <the read's why>` when the read
could not check, and `--format ai`'s dependency advisories carry §7's line (design/specs/cli.md §5.3, §6.1).

## 9. Permissions (PERMISSIONS.md)

| What | Minimum | Below it |
|---|---|---|
| The *System libraries* card, `GET …/envs/:env/image`, `patched[]`, `envs.<env>.patch`, the receipt's known-vulnerable packages' `layers` and `go_stdlib` (#2579) | contributor (technical detail) | `403 team_required` naming the owners; the reduced row has none; a responder's receipt names each package without them |
| Attach an image report | contributor, as package reports | `403 team_required` |
| Decide an app's patch approval | contributor, a person, a browser session | `403 team_required` · `403 dashboard_required` · an agent never decides |
| `patching`, `rebuilds`, an identity's resume, a fleet approval, the identities read | admin; each change from a browser session | `403 admin_required` · `403 dashboard_required` with one `denied` row |
| `platform.known-vuln`'s state and source | admin; an owner may only make it stricter (D14) | as the policies rows |

Read-only members keep `Open preview` and `Open production`; a patch changes nothing they see. QA
covers owner, contributor, read-only, no access and admin for each row. The fleet approval is the
one model change, made in PERMISSIONS.md as PR 9's first commit, before PR 16.

## 10. Contract deltas — each in the contract PR before its first consumer

- **PR 2 (Phase 1)**, its first commit — moved out of PR 1 under the rule that a contract change and
  its consumer share one PR (2026-10-11):
  - executor.md: `POST /scan` `target: "image" | "sbom"`, `CANYON_SCAN_IMAGE_MAX`,
    `CANYON_SCANDB_TRIVY_JAVA`, executor.schema.json and its golden.
- **PR 1 (Phase 1)**, split by first consumer (2026-10-15):
  - **The first commit of PR 3:**
    - finding.md: the `IMAGE` base, owners and classes, region, snippet, fingerprint, `fix` texts, notes.
    - snapshot.md: `platform.known-vuln` as a go-live rule. It installs `off`, as a new rule does,
      until its default is decided.
    - routes.md: the receipt row (each layer's newest match, staleness),
      `GET /api/v1/projects/:id/envs/:env/image`, `dependency.layer`, the deploy row's values-only HMAC.
    - Schemas and a golden.
  - **With PR 4:** events.md's `progress` step `image-sbom` — PR 4 did not add it (the SBOM it takes is
    off the deploy's path); PR 9 does, for the patch run's step 3.
  - **With PR 5:** `PUT …/images/:digest/reports` and `settings.source`.
- **OPERATIONS.md's `keep` PR**: `/run keep[]` by digest. Phase 2 references it; it does not redefine
  it.
- **PR 8**: PERMISSIONS.md, the fleet approval — PR 9's first commit since 2026-10-18 (rule 5).
- **PR 9 (Phase 2)**:
  - executor.md: `POST /rebase` (fresh tag; kpack's rebase Build, the Job not taken and the Role
    unchanged, settled by #2091; the facts it answers), `GET /builder`'s `run_image`,
    `CANYON_IMAGE_RUN`, `CANYON_RUN_APT_MIRROR`; invariant 4 untouched.
  - routes.md: runs `kind: patch`, the `StartTx` cancel rule, `gate_state` skipping every patch run
    and `promote_run` each whose image preview does not run, `envs.<env>.patch`, `patched[]` (on the
    image read), the run-config fingerprint, `patching`, approvals `kind: patch`, one per
    environment, re-filing, the fleet-approval, resume and identities routes, audit categories.
  - events.md: `progress` steps `rebase`, `image-sbom` and `watch`, the new `failure_reason`s and
    `not_platform_only`, the `build.log` lines.
  - Not here: the SBOM of a named run image the plan matches (§0.4), which #2375, split out of PR 13,
    adds in its own seam commit: executor.md's run-image form of `target: "image"`.
  - The golden `patch/claims-portal.json`.
- **PR 19 (Phase 3)**: rebuild patches and `same_inputs` — with PR 21 in one PR, its first commit (#2111, #2117):
  routes.md's *A rebuild patch*, `rebuilds`, the identities' `kind` and `builder`, `always_asked`, the rebuild
  approval's `proof`; events.md's `not_same_inputs` and the rebuild's `build` step; the golden's `rebuild` case.

## 11. Fail closed

| What went wrong | What happens — never "pass" |
|---|---|
| SBOM not taken, over the cap, or not yet landed | the rule reads `could not check` / `no image scan recorded`; production waits |
| Re-match failed or database stale | `could_not_rescan`; the rule reads `could not check` and blocks where it blocks; nothing planned |
| A component Canyon can't place | class `unknown`: a note, never planned, never silently dropped |
| The built app could not be checked for `deps.known-vuln` (§3.6) | its row reads `<version>'s built app could not be checked: <reason>` and does not pass; production waits where it blocks |
| Java database missing, unreadable or stale | the image scan answers `not_finished`, `reason: "java"`; the rule reads `could not check` |
| Gate blocked or unreadable | patch run `blocked`/`failed`; `held`; nothing deployed |
| Rebase refused (`rebasable=false`, another run image name) or failed | `failed`; never forced |
| Rebase refused for what the images are (`not_rebasable`, `another_run_image`, `another_stack`) | that image and run image are never planned again; the app moves by its next deploy (§4.2) |
| Proof fails, or its facts are missing | `not_platform_only`; nothing deployed |
| A rebuild's inputs differ, can't be compared (another key version), or were never recorded | `not_same_inputs`, or never planned; nothing deployed |
| Config fingerprint differs | `config_changed`; nothing deployed; the team is told to deploy |
| New `high`, or the planned finding still there | `patch_regressed` / `patch_ineffective` |
| `/run` of the patch fails | the old container is handed back (today's `/run`) |
| Exit or restart in the watch | revert by compare-and-set, with the hold and the row re-checked in one transaction |
| Watch can't read state | no production step; no blind revert |
| Two reverts in one identity | identity paused fleet-wide; admins told |
| Approval expired, outdated, denied, or production held | nothing changes in production; re-filed next tick while the plan stands and production is not held |

## 12. Deliberately left out — and what would bring each back

| Left out | Brought back when |
|---|---|
| **`auto` production patches** | `ask` has run a quarter on real fleets with its revert and approval-latency data; then rebase-only, never rebuilds |
| **Production patches of a package preview no longer runs** | invariant 4 is amended in its own seam PR, with a reason stronger than "promote v1.4" |
| **Patching under hand-off** | an org asks; its release would carry `patch{}` and the org's platform would own the watch |
| **A health-probe seam** (per-app readiness path) | the first incident a container-exit watch missed |
| **Re-matching app-class layers for a project that does not place `deps.known-vuln` before production** | it places it (§3.6); until then the next review re-reads its lockfiles |
| **Counting a review's package inventory**, so a package its lockfiles name is never counted as only the built app's. The inventory itself is built (2026-11-04, #2644): `/scan` `target: "packages"` lists a tree's packages offline, and a review asks for it while a custom rule checked by code is on (contracts/executor.md). Left out: asking for it on every review, storing it with the run, and counting the built app's packages against it | a team reports a lockfile package blocked after its review; until then it is counted until the next review reports it (§3.6) |
| **Listing each package only the built app holds on the image read and the app page** | a team asks to see more than the first: the *System libraries* card is Canyon's packages alone, and the row names the first (§3.6) |
| **Pull-request previews** | they outlive a day; today the next push rebuilds |
| **Maintenance windows and freezes** | an org asks; under `ask` a person already picks production's moment |
| **Patch PRs** (§7) | its own design |
| **Outbound notifications** | an outbound channel is designed; the event stream is the hook meanwhile |
| **Grype for images; runtime-binary detection** | trivy, or another tool, identifies runtime binaries |
| **A bot-opened bump PR** (§5: the bump opens an issue and a branch) | the owner adds a bot/App token |
| **The bump moving the lifecycle image** (`lifecycleImage`, executor/executor.go, pinned apart from the builder's own) | a `lifecycle` finding has its fix only in a newer lifecycle; the pin moves by hand meanwhile, and §2's `lifecycle` class waits on it |

## 13. Implementation plan

Each PR is off `main`, at most about 800 added lines, with the failing test shown red first (rule 2)
and contracts merged before consumers (rule 5). Migrations take the next free number when written;
none is named here.

**Phase 1 — See**

| # | PR | Red-first | Size |
|---|---|---|---|
| 1 | `docs(contracts): image SBOM, Canyon's layers, image reports` — all but the executor's deltas, which moved to PR 2; lands as PR 3's first commit | `validate.py` refuses an `IMAGE` finding under another rule, and a receipt passing on a 6-day-old match | ~400 |
| 2 | `feat(executor): image SBOM and offline re-match` — Docker and Kubernetes, the Java database; its first commit the executor's contract deltas | the `LayerDiffID` round trip; over the cap is `over_cap`; the Paketo listings for Node, Python and Java, deferred to #2216 (amd64 builds) | ~1,400 |
| 3 | `feat(server): per-layer storage, classes, the rule, values-only HMAC` | §3.5's class and rule tests | ~750 |
| 4 | `feat(runs): the rescan workflow` — scheduled, a deterministic id, a global queue | two replicas run one tick once; a stale database plans nothing | ~450 |
| 5 | `feat(server): SBOM referrer and imported image reports as the rule's source` | a member attaching is `403`; an imported result with no fix version is a note | ~500 |
| 6 | `feat(dashboard,cli): System libraries` | read-only sees no card; the ai advisory carries §7's line; `could not check` reads ✗ | ~650 |
| 7 | `test(e2e): platform scan on Docker and the EKS test cluster` | §3.5's E2E | ~350 |

**Phase 2 — Rebase** (after OPERATIONS.md's `keep` contract and executor PRs)

| # | PR | Red-first | Size |
|---|---|---|---|
| 8 | `docs(permissions): an admin approves a platform patch for the fleet` | — (model change) | ~60 |
| 9 | `docs(contracts): rebase, patch runs, approvals, revert` | `validate.py` refuses a patch golden that deploys before its proof row | ~600 |
| 10 | `feat(executor): rebase on Docker, to a fresh tag` | layers above the run image identical and below changed; `rebasable=false` refused; the old tag survives | ~400 |
| 11 | `feat(executor): rebase on Kubernetes` — kpack's rebase Build (#2091) | kind: the Build's shape and every refusal; a mirror under another name refused | ~450 |
| 12 | `feat(server): named run images and the run-config fingerprint` | `CANYON_IMAGE_RUN` on another stack refused; the fingerprint is stable over unchanged rows | ~400 |
| 13 | `feat(runs): plan, queue, waves, breaker` | a low finding plans nothing; two reverts pause the identity; a person's run starts ahead of patch runs | ~650 |
| 14 | `feat(runs): the patch run to preview` | §4.8's proof, config, regression and cancel tests; `gate_state` untouched | ~750 |
| 15 | `feat(runs): watch and revert` | racing request and revert; resume at `watch_until`; a stop mid-watch ends it clean | ~500 |
| 16 | `feat(server): production asked; fleet approval` | one approval per environment; never over a person's; held production files none; re-filed after expiry | ~750 |
| 17 | `feat(dashboard,cli): patches people see` | the approval page's proof text; `needs_you`; the state lines | ~700 |
| 18 | `test(e2e): a rebase patch end to end, Docker and EKS, with a revert` | §4.8's E2E | ~500 |

**Phase 3 — Runtimes**

| # | PR | Red-first | Size |
|---|---|---|---|
| 19 | `docs(contracts): rebuild patches, same_inputs` | golden refused without a values-HMAC key id | ~250 |
| 20 | `ci: Paketo pin bumps with the scan diff` | a dry run against an old pin prints an issue body naming the diff; an open issue for the same pins is updated, never a second | ~400 |
| 21 | `feat(runs): rebuild patches, asked` | §5's tests | ~700 |
| 22 | `test(e2e): a rebuild patch on the EKS test cluster` | §5's E2E | ~250 |

**Phase 4 — Patch buildpack**: 23, the spike (a branch and an issue, never merged). Only if it
passes: 24 `feat(executor): canyon/patch and its first entry` (~700), 25 `feat(executor): every build
runs canyon/patch` (~450), 26 `test(e2e)` (~300).

## 14. Edits other docs need

Listed, not made; each lands in the PR that makes it true.

- **EXPERIENCE.md**: D2 `platform.known-vuln`; D5 the accepted proposed strings — done: the strings #2578, #2580,
  #2583 and #2584 proposed were accepted on 2026-10-30 with copy edits and are in D5 as edited (#2591); D9.2 the image is
  read after the deploy and judged before production only; D14 five go-live rules, and "Canyon
  patches a running app's platform through a run".
  D5 also takes `deps.known-vuln`'s built-app lines (§3.6, #2355).
- **ARCHITECTURE.md**: §1, `/rebase` and the SBOM; §5, the `patches` queue and DBOS's scheduled
  workflow; §6, patch PRs stay out, with §7's reason.
- **PERMISSIONS.md**: the fleet approval (PR 8).
- **OPERATIONS.md**: §3.4's cancel list becomes "any run that would write this environment, patch
  runs and their watches included"; §1 and §6 say a revert is not a rollback (§4.4); its `keep`
  serves both docs.
- **JOBS.md**: §"Patching by kind" matches §4.6.
- **EGRESS.md**: §2.5's recorded `via` is in the run-config fingerprint; the mode is live policy (#2094).
- **HAND-OFF.md**: no change until hand-off patching returns.
- **design/specs/cli.md**: §6.1's `platform` line; §5.2–5.3's dependency line and the production
  step's tail.
- **design/specs/screens.md**: A2 the card; the production approval screen's `patch` variant; A1
  the cells; Settings, patching; B6 the incident view gains *Patched by Canyon*.
- **deploy/k8s/README.md**: *Run image* — how an operator moves it, keeping the ClusterBuilder's name and tag.
- **AGENTS.md**: `make patch-buildpack` and the per-entry fixture rule, only if Phase 4 ships.
- **e2e/cluster/CASES.md**: the platform-scan, rebase-patch and rebuild-patch cases.
