# Seam 4 — the executor interface

The executor is a separate trust domain: it holds the Docker socket and runs untrusted app
code; it must never hold anything else. Eight calls, and two more on Kubernetes (`PUT /access`,
`POST /probe`). Reachable only from the control plane on `canyon-internal` — on Kubernetes, from the
server's pods alone (*On Kubernetes*, below).

## `POST /build`

```jsonc
// request: multipart, part "json" first, then part "tree" (a tar of the source) — the tree
// streams straight into the build, it is never buffered
{ "app": "claims-portal",
  "package": "pkg_7d3e8f",
  "build_values": { "VITE_API_URL": "" } }   // ONLY the values the user marked where: build — see invariant
// response: application/x-ndjson — one object per build-log line as it happens, then the result
{ "line": "===> DETECTING" }
{ "line": "===> BUILDING" }
{ "image": "canyon/claims-portal@sha256:…", "duration_ms": 51800,
  "buildpacks": [ { "id": "paketo-buildpacks/node-engine", "name": "Paketo Buildpack for Node Engine" }, … ] }   // or, on failure, the last line is
{ "error": "the build step exited 51" }                               // — headers are long gone, so failure is a line, never a 500
{ "error": "no buildpack group passed detection", "builds": ["Paketo Buildpack for Node.js", "…"], "errored": true }   // a failed detect, below
{ "error": "the cluster had no room to run the build", "no_room": "0/4 nodes are available: 4 Insufficient cpu." }   // no room, below
{ "error": "build network: network create canyon-builds: …", "own": true }   // the executor's own failure, below
```

**A failed build says which step and its code** (**amended 2026-10-05**, #1734: `pack: exit status 1`
named neither). When a step of the build exits non-zero the line is exactly `the <step> step exited
<code>`, the step's own exit code. On Kubernetes `<step>` is the build pod's step container that exited
(`detect`, `restore`, `build`, `export` …). On Docker pack reports one status for the whole lifecycle
and never which phase it came from, so `<step>` is always `build`, Canyon's build step, and the code is
the status pack's last line reports. The build's own lines stand before it as they were written. Any
other failure (pack itself, the daemon, the cluster API) is the error as the executor got it, such as
`pack: exit status 1`. Which of
the app, a buildpack or the build image a step failed on is never the executor's to say.

**A build the executor went away under is asked once more** (added 2026-11-04, #2671). A stream that ends
before its final row — the connection refused or reset, the body cut short or closed — is the executor
restarting, upgrading or drained, never the package. The control plane waits for `GET /builder` to answer
and asks the same `/build` again once, the same tree and the same `build_values`. That is safe with no key:
a build keeps nothing between calls, and an executor that starts again removes what a crash mid-build
left before it serves (every Build labelled `canyon.build` on Kubernetes, every build holder labelled
`canyon.build` on Docker), so the second call builds afresh. A second loss, or the run's own deadline, fails the run as Canyon's own error.

**No room is not a failed build.** On Kubernetes, a build whose pod is still unscheduled at the build's
deadline — the executor's own, 29 minutes from the call, so it answers before the server's 30 give up
on it; its `PodScheduled` condition `False` with reason `Unschedulable` — ends on exactly `the
cluster had no room to run the build`, with `no_room` the condition's own message as the scheduler wrote
it, quoted and never parsed: the cluster's capacity, an admin's to add, never the package's nor Canyon's
(amended 2026-10-05, #1637). Any other failure carries no `no_room`.

**The executor's own failure is not a failed build** (**amended 2026-10-27**, #2489, #2537; **amended 2026-10-28**, #2550). When the
executor's host fails a build, and not the package's own steps, the failure line is the error as the executor got
it, with `"own": true`: Canyon's own failure, never the package's nor its build's, and the control plane records
`failure_reason` `error` with the line as the last `build.log` row (events.md). These are, on Docker: the
build network (`build network: …`: `canyon-builds`, the holder or its floor not in place); a builder whose
image runs as root, or whose user's uid cannot be read; a builder whose labels cannot be read while the
package's `project.toml` names buildpacks, whose line starts `the builder's labels cannot be read, and
project.toml names buildpacks: `; the run image (`CANYON_IMAGE_RUN` not pulled or read, or `canyon-run` not
built or tagged); the builder (`CANYON_BUILDER` not pulled or inspected, or `canyon-builder` not built); a
builder and a run image on different stacks, or either's stack unread; the build's own working files (its
directory, Canyon's descriptor) not written; and the built image's ID not read once pack succeeded. On
Kubernetes: the executor not watching Builds; the ClusterBuilder `canyon` not read, or with no image yet;
the app's repository not made in `CANYON_KUBE_REGISTRY`; the source image's push refused by the registry; the
Build not created, but for a refusal of its build values, below; the Build deleted before it ended; and an answered image that is not the app's by digest.
A server from before `own` reads such a line as a failed build (`build`), still closed: nothing is built or
run. Any other failure carries no `own`, and stays the package's (`build`): the tree (`tree: …`, a tar that
does not read, an entry that escapes it or one past its bounds), a `project.toml` refusal, pack's lifecycle
exiting (`the build step exited <n>`, a failed detect, `pack: …`), a build past the executor's deadline, a
Build kpack failed with no step to name (`build <name> failed: …`), and on Kubernetes a Build whose create the cluster's
API refuses as `Invalid` or `RequestEntityTooLarge`, whose line starts `the cluster refused the build values: ` and goes
on with the API's own error: the build values are the one part of the Build that is the user's, so too many or too large
for the API's object limit, or a name it refuses, is the user's to change, never Canyon's (#2550).

**A failed detect is not a failed build.** When no buildpack group of the builder detects the tree —
the lifecycle's detect phase exits 20 or 21, the CNB platform's two codes for "no buildpack group
passed detection" — nothing was compiled, and the failure line is exactly `no buildpack group passed
detection` with `builds`: the buildpacks the builder's order names, in order, optional ones left out,
each by the name the builder's metadata gives it (`Paketo Buildpack for Node.js`), else by its id — and
only text matching `^[A-Za-z0-9][ -~]{0,127}$`, printable ASCII: a name is the builder's text and reaches
a terminal, so a name with a control or bidi character, or a run-on name, is named by its id instead, and
a buildpack whose id fails too is left out. Each entry once, at most 64 of them; `[]` when it names none
(amended 2026-10-04, #1561: they were ids). A detect that exited 21 — no group passed and at least one
buildpack errored while detecting — also carries `"errored": true`; a 20 never does. A buildpack may
error on any tree it does not recognise (Paketo's `node-start` exits 1 on a tree with no Node entry
point, so a Rust app on the Paketo builder reads 21) or on a malformed manifest of its own stack, so
`errored` says the second is possible, never that it happened. The Docker executor reads the code from pack's last line,
`ERROR: failed to build: executing lifecycle: failed with status code: <code>`, the order from the
builder image's `io.buildpacks.buildpack.order` label and the names from its `io.buildpacks.builder.metadata`
label (`buildpacks[]`, each `{id, name}`); on Kubernetes the code is the exit code of the
build pod's `detect` step container, the order the ClusterBuilder `canyon`'s `status.order`, and the
names the same metadata label of its `status.latestImage`, read from `CANYON_KUBE_REGISTRY` with the
executor's credential (the ClusterBuilder's status names none, and an image outside that registry, or whose
manifest or config does not name a size of 1 to 4 MiB, is not read). Only a builder whose metadata carries names has them — pack's builders do; a kpack ClusterBuilder's
image carries none, so on Kubernetes the buildpacks show by id (amended 2026-10-04, #1599). A builder whose
names cannot be read, or whose metadata has none, names its buildpacks by id, and the executor's log says why. Any
other failure carries no `builds`. The detector's own lines stream before it as every build line does.

**The buildpacks that ran.** A build's result names the buildpacks of the group that passed detection, in
the order they ran: each `{id, name?}`, `id` the buildpack's CNB id and `name` the name the builder's
metadata gives it, under the rules a failed detect's `builds` follows — printable ASCII matching
`^[A-Za-z0-9][ -~]{0,127}$`, else no `name` — each once. A group with an id that fails
`^[A-Za-z0-9][A-Za-z0-9./-]{0,127}$`, or of more than 64, is `[]`: a list missing what ran would be judged
as if it were all. The Docker executor reads the group from the built image's
`io.buildpacks.build.metadata` label (`buildpacks[]`, each `{id, version}`) and the names from the
builder's metadata label, as for a failed detect; on Kubernetes the group is the kpack Build's
`status.buildMetadata` (each `{id, version}`) and, the ClusterBuilder's image naming none, no buildpack
has a `name`. A group that cannot be read is `[]`, and the executor's log says why: a build that ran
is never failed for it (amended 2026-10-04, #636).

**What `image` means.** In v1 the value after `@sha256:` is the Docker daemon's image ID
(`docker image inspect --format {{.Id}}`): content-addressed on the one daemon the executor
holds, so re-running it is re-running the same bytes — and never pullable from a registry.
On Kubernetes the same field carries the registry's digest under the registry's host, and `/run`
passes the whole reference through (*On Kubernetes*). Never a tag, on either. Build configuration that belongs to the app
(which directory to build, which scripts to run) lives in the app's own `project.toml`, which
the buildpacks read; it is not a build value.

**Every app runs on Canyon's run image.** Builds use Paketo's Ubuntu Noble stack, and the image an
app runs on is `canyon-run`: Paketo's noble run image, pinned by digest, with `libatomic1`,
`libsqlite3-0` (**amended 2026-10-05**, #1708) and `libpq5`, which the build image carries too (*Every
app builds on Canyon's build image*, below; #1734), each noble's own when the image is built, at no
pinned version: noble-updates and noble-security stop serving a superseded version, so a pin there
fails every build at the library's next security update, while noble keeps each library's interface
(`libpq.so.5`, `libsqlite3.so.0`, `libatomic.so.1`) and only its patches move (**amended 2026-10-05**,
#1754); and nothing else changed; it ends as the stack's non-root user, `1002:1001`, as the CNB spec requires
(`executor/run.Dockerfile`, Paketo's documented way to add packages to a stack).
Node 25 and later link `libatomic.so.1`; Paketo's Node Engine offers Node 26 on every stack and gives
it to an app whose `engines` reads `>=20`, and no Paketo run image carries the library
(paketo-buildpacks/nodejs#1387), so the app built and then could not start. The build image carries it
already. The app's own Node choice is never overridden. Paketo's CPython for noble links its `sqlite3`
module against `libsqlite3.so.0`, which neither the stack's build image nor its run image carries, so every stock
Django project — SQLite is `startproject`'s database — and any Python app importing `sqlite3` built and
then could not start (#1708). Measured on 2026-10-05 with `ldd` on canyon-run over every file of the
noble Python, Ruby, PHP (with its httpd and nginx), Node.js and .NET runtimes the builder's buildpacks
install: the other libraries canyon-run lacks are linked only by CPython's `readline`, `dbm.gnu` and
`tkinter`, PHP's optional extensions, httpd's LDAP modules and .NET's LTTng tracing; none is added until
an app is seen to need one. PHP's `pgsql` and `pdo_pgsql` link `libpq.so.5`, and a PHP app on Canyon's
Postgres was seen to need it on 2026-10-05 (#1734): its build failed, Composer refusing `ext-pdo_pgsql`. The Docker executor builds `canyon-run` itself
from that file, as it builds opengrep's image, tags it `canyon-run:<12 hex of the file's sha256>` — the
file and its pinned base determine the image but for its packages' patch versions, noble's when that
host built it; a host keeps the image its tag names until the file changes or the image is removed, so
it takes a later security fix of those packages only then, and a change of the file changes the tag
and every host builds it again — and
names it `canyon-run:noble` too, a tag it moves onto each build, which pack is handed as `--run-image`
(**amended 2026-10-18**, #2089): the lifecycle records the name pack is handed in the image's
`runImage.image` and rebases only onto that name (`/rebase`), so a name that moved with the file would
leave every image it built unrebasable. The name follows the distribution, so a file that moves to
another Ubuntu release takes another name and a rebase across releases is refused, never forced. An
image built onto `canyon-run:<hex>`, before the name, is never rebased: it moves only by a rebuild.
It builds it through `CANYON_RUN_APT_MIRROR` when that is set
(*Images and database sources*), so a host that cannot reach Ubuntu's archive builds it at all, and
never on a schedule of its own: the file alone says when it is built (**amended 2026-10-18**, #2086;
design/PATCHING.md §4.1). **`CANYON_IMAGE_RUN`** names the operator's own run image in its place — a
golden image on the same stack, which the executor pulls when the daemon has none, as it pulls
`CANYON_BUILDER` — and pack is handed that reference instead; set, `canyon-run` is never built. Setting
it, unsetting it, or moving it to another repository leaves every app built before onto another name,
which no `/rebase` moves until the app is deployed again (`another_run_image`); a new digest in the same
repository is the same name. Its
builder is `canyon-builder` (below), and a
`CANYON_BUILDER` must build on the same stack: pack 0.40.3 only warns when the builder's stack is not
the run image's, so before pack runs the executor compares the two images' `io.buildpacks.stack.id`
and, when they differ, fails the build naming `CANYON_BUILDER` — a builder on another stack never
builds an app onto `canyon-run` — and `CANYON_IMAGE_RUN` too when that is set: a run image is checked
as a builder is. On Kubernetes the
ClusterStack `canyon`'s run image is `canyon-run`, which the deploy names by digest and the ClusterStack
reads with the builder's service account, `canyon-build` (*On Kubernetes*).

**Every app builds on Canyon's build image** (**amended 2026-10-05**, #1734). A buildpack's build runs
the app's own tools against the libraries its runtime links — Composer loads PHP's extensions to check
a `composer.json`'s `ext-*` — so a library the run image carries for a runtime, the build image carries
too. Only `libpq5` does today: `libatomic1` is in Paketo's build image already, and `libsqlite3-0` is
linked by CPython's `sqlite3`, which no build step loads. On Docker the builder is `canyon-builder`,
which the executor builds as it builds `canyon-run`, from `executor/builder.Dockerfile`:
`paketobuildpacks/ubuntu-noble-builder` pinned by digest — the builder no longer follows the tag, so
every host that builds one `canyon-builder:<12 hex of the file's sha256>` tag holds the same buildpacks,
and a bump of the pin is a new builder — with `libpq5`, as above, ending as the stack's build
user, `1001:1001`. A `CANYON_BUILDER` replaces it whole, and its libraries are the operator's. On
Kubernetes the ClusterStack `canyon`'s build image is `canyon-build`, from `executor/build.Dockerfile`:
`paketobuildpacks/ubuntu-noble-build` pinned by digest with `libpq5`, ending as `1001:1001`, which the
deploy names by digest as it names `canyon-run`.

**A Node app builds on Canyon's Node builder, on Docker** (**amended 2026-11-24**, #174). `CANYON_BUILDER_NODE`, in
the executor's environment, names `canyon-builder-node`, as `CANYON_BUILDER` names a builder. Each release publishes it
by digest beside its other images (`images.txt`), and the laptop demo's bundle names it so. It is made from `canyon-build` (above), Paketo's `nodejs`
buildpack at the digest `deploy/k8s/builds.yaml` pins, and the lifecycle the pinned `canyon-builder` carries. Its run
image is `canyon-run:noble`, and the executor's own two build variables (*Every build runs with*, below) are the
builder's own build environment. A build with no build values therefore hands pack neither `--run-image` nor
`--env`, so pack builds on the builder itself, never on an ephemeral copy it saves to the daemon. Saving that copy
takes about 85 s a build on Docker's containerd image store, and leaves about 1.3 GB that nothing frees.

- **Which builder.** `/build` uses it for a tree when both hold:
  - (a) the tree and its build environment (its build values, and the `project.toml` Canyon reads) carry none of the
    entries `executor/choose.go` lists;
  - and no build value names either of those two variables, which only `--env` can replace;
  - (b) the Node builder's detect passes, as pack's `detect` phase reports it. Its log is held until it has, so a
    tree it detects nothing in shows `canyon-builder`'s build alone. When this host doesn't have `canyon-builder` yet,
    that build's log says so and what it downloads, once.
- **Otherwise `canyon-builder`**, which then decides exactly as today, its failed-detect answer included.
- **(a)'s list.** It is read from the detect code of every buildpack group the full builder orders before `nodejs`,
  at the versions `deploy/k8s/builds.yaml` pins: entry names and globs in the tree, and build variables set at all.
  It never names the app's language. It is a superset of what could make an earlier group pass, so a tree it
  can't rule out goes to today's builder.
- **Keeping it true.** `choose.go` carries the pins it was read for, written so the Paketo bump never moves them. A
  test fails while any of them differs from what the tree pins, until the detect code is read again.
- **Unset, or with `CANYON_BUILDER` set:** every app builds on the one builder, as before.
- **On Kubernetes** the ClusterBuilder is unchanged.

**Every build runs with `NODE_ENV=production`**, the executor's own build variable, which a build value
of that name replaces. Paketo's npm-install installs devDependencies under `NODE_ENV=development` and
hands that value to the buildpacks after it; a build variable is applied over what an earlier buildpack
hands on, so the app's build script builds for production (`next build` under development fails on its
404 page) with its devDependencies installed.

**Every build runs with `NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/`** too, the executor's second build variable,
which a build value of that name replaces (#1240). Paketo's npm-install links the app's `node_modules`
to a layer outside the app's directory, and Next 16's default bundler, Turbopack, refuses a link that
leaves its root: a stock `create-next-app` fails at `next build` with `Symlink [project]/node_modules is
invalid`. Next reads this variable as the root when the app's own config sets none (`turbopack.root`,
`outputFileTracingRoot`), and the filesystem's root holds the layer. It is Next's private name, kept
until Paketo or Next handles the link upstream (#1461).

**How pack runs.** The builder is never trusted: pack would run a trusted builder's every phase,
the app's own build scripts included, in one container holding the Docker socket, and pack
0.40.3 trusts any builder named exactly as on its built-in list (the Paketo builders among
them) whatever `--trust-builder` says. So the executor names a Docker Hub builder with its
registry (`docker.io/paketobuildpacks/…`), which is the same image and matches no entry, and
pack runs the five-phase flow: detect and build run in the builder with no socket; analyze,
restore and export run in the lifecycle image, and the exporter still holds the socket to write
the image. That image is pinned by digest (`CANYON_IMAGE_LIFECYCLE`, *Images and database
sources*). Each build asks the image's registry for its manifest to pick the platform's image, so a
host that cannot reach docker.io points `CANYON_IMAGE_LIFECYCLE` at a registry it reaches (pack 0.40.3
asks for that manifest before looking in the daemon); `CANYON_BUILDER` names a builder there, and
otherwise `canyon-builder`'s and `canyon-run`'s first builds fetch their pinned bases from docker.io and
`libpq5`, `libatomic1` and `libsqlite3-0` from Ubuntu's archive (amended 2026-10-05, #1734, #1754).
The builder's build user is never root: a builder whose image user is empty, or whose uid is 0, is
refused before pack runs, naming it, since the app's own scripts run as that user (amended 2026-10-25,
#2461). A numeric user is read as Docker reads it (`00` is root); a name is resolved by the image's own
`id -u`, in a container with no network, and one that cannot be resolved is refused.

**A build reaches no private or link-local address** (**amended 2026-10-25**, #2461: builds ran on
pack's own bridge network, the cloud's metadata address included). A build reaches DNS, TCP 80 and 443
at public addresses, and what `CANYON_BUILD_ALLOW` names (Docker) or the operator's own policies add
(Kubernetes). That is the same under every Outbound mode: Outbound covers running apps, never builds,
and no build call is recorded.

- **On Docker every phase runs in a floored network namespace.**
  - **The network.** The executor keeps one network, `canyon-builds`: a bridge, IPv6 off, ICC off
    (`com.docker.network.bridge.enable_icc=false`), labelled `canyon.build`. It creates the network
    when the daemon has none. If a network of that name exists and is anything else, it is refused,
    never adopted or changed.
  - **The holder.** For each build it starts a holder, `canyon-build-<nonce>`, on that network. The
    holder runs `CANYON_IMAGE_EGRESS_HOLDER`'s image (*Images and database sources*): every
    capability dropped but `NET_ADMIN`, `no-new-privileges`, a read-only root, `--restart no` and
    the label `canyon.build`. Its entrypoint is `executor/buildfloor.sh`, which applies the floor in
    its own namespace and then sleeps. A holder therefore never runs without its floor: one that is
    started again comes back as a new namespace, and nothing joins it.
  - **The floor.** It is the chain `CANYON-BUILD`; the script's last rule inserts the jump into it at
    the head of `OUTPUT`. In order:
    1. the namespace's own loopback, where Docker's resolver (127.0.0.11) answers, returns;
    2. every address of the Docker host's own, a public one among them (read as an app's holder reads
       them, *An app reaches no private, link-local or host address*; **amended 2026-10-27**, #2503's
       security review: public 80 and 443 returned the host's own public address, where Canyon's server
       may be published), `169.254.0.0/16`, `168.63.129.16`, `100.100.100.200`, `0.0.0.0/8`, `127.0.0.0/8`,
       `224.0.0.0/4`, `240.0.0.0/4`, `CANYON_APPS_POOL`, Canyon's own networks
       (`CANYON_EGRESS_POOLS`, which egress's floor refuses too; unset, the bundled compose file's
       `10.213.0.0/16,10.214.0.0/16`) and `canyon-builds`' own subnet are rejected, established
       connections among them (amended 2026-10-27: before established connections return);
    3. established connections return;
    4. each `CANYON_BUILD_ALLOW` entry returns;
    5. `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `192.0.0.0/24` and
       `198.18.0.0/15` are rejected;
    6. TCP 80 and 443 return;
    7. everything else is rejected.

    `FORWARD` drops, and where the namespace has IPv6, `ip6tables` drops all of it but loopback, in
    a chain of the same name.
  - **Waiting for the floor.** The executor waits, for at most 30 s, until both jumps are in place.
  - **DNS.** A build resolves through Docker's resolver on its own loopback, which forwards from the
    daemon. Docker 28.0 and later forward to the host's own resolvers from the host's network
    namespace, so a resolver at a private address (a VPC's) answers builds. A daemon `dns` setting
    is forwarded from the build's namespace instead, where the floor refuses a private address, so
    builds there resolve nothing: leave the daemon's `dns` unset, or name public resolvers in it.
    Docker before 28.0 forwards to any resolver that is not on loopback from the build's namespace,
    and Canyon needs 28.0 or later already (`--gw-priority`).
  - **pack.** pack is handed `--network container:canyon-build-<nonce>`, which it gives every phase.
    Every container of the build shares the holder's namespace and none holds `NET_ADMIN`.
  - **Cleanup.** The holder is removed when the build answers, and at the executor's start every
    container labelled `canyon.build` is removed, as `canyon.scan`'s are.
  - **Fail closed.** If the network cannot be had as above, or the host's addresses are not read, or
    the holder does not start, or its floor is not in place within the bound, the build fails before pack runs, its line starting
    `build network: `. No build runs on pack's own network or the default bridge.
  - **Build values pack would read.** pack runs in the executor, outside the floor, and a build value
    travels in pack's own environment to reach the buildpacks. A build value named, in any case,
    `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `GODEBUG`,
    `REGISTRY_AUTH_FILE`, `XDG_RUNTIME_DIR` or anything starting `PACK_`, in any case, refuses the
    build before pack runs, naming it: pack would connect where the app says, trust what it says,
    pick the credentials it says, or read its own configuration from it. They are one list with the
    names pack and docker need to find the daemon, which no call's env may name; and no call's env,
    a build's or a `/run`'s, may name anything starting `DOCKER_`, in any case, which docker's client
    reads (on Docker: a `/run` that does, an app's runtime secret named so, is refused, so the app's
    deploy, promote, rollback or platform patch fails naming it).
    On Docker alone: on Kubernetes a build value reaches kpack's build pod, never a process outside it.
- **`CANYON_BUILD_ALLOW` (Docker).** It names the addresses and ports a build may reach besides, such
  as an internal registry or mirror: comma-separated entries, each `<IPv4 prefix>:<TCP port>` with one
  port.
  - An entry that is malformed, has a prefix shorter than `/8`, or overlaps `169.254.0.0/16`,
    `168.63.129.16/32`, `100.100.100.200/32`, `0.0.0.0/8`, `127.0.0.0/8`, `224.0.0.0/4`,
    `240.0.0.0/4`, `CANYON_APPS_POOL` or `CANYON_EGRESS_POOLS` stops the executor from starting,
    naming it.
  - An allowed forward proxy gives builds everything that proxy reaches.
- **pack fetches nothing an app names, and never reads the app's `project.toml`.** pack decodes TOML
  with another library than Canyon's, and the two match keys differently (case, Unicode folding, two
  keys that differ in case), so a file Canyon read one way pack could read with a buildpack's `uri`.
  So the executor reads the tree's `project.toml`, if it has one, and writes a descriptor of its own,
  schema 0.2, in a directory outside the tree, which pack is handed with `--descriptor`: pack reads
  only bytes the executor wrote. pack resolves a group buildpack's relative path against that
  directory, and a `pre` or `post` one against its own working directory; every buildpack the
  executor writes is one the builder carries, or an inline script, so neither is ever read.
  - **What it reads.** Schemas 0.1 (`schema-version` `0.1`, or none), 0.2 and 0.3 as pack knows them.
    Inside the tables pack reads (`_`, `io.buildpacks`; 0.1's `project` and `build`), every key byte
    for byte: one it does not know refuses the build. Elsewhere, a table of another tool's
    (`[com.heroku.…]`, `[io.<tool>]`, 0.2's `[metadata]`) is left alone, unless its key matches a name
    pack reads only by case or Unicode folding (`IO`, `Buildpacks`, `buildpackſ`), which refuses the
    build: pack's decoder matches keys so, and Canyon reads none such. A value of another type, an
    unknown `schema-version`, or a file that does not parse refuses the build too.
  - **What it writes.** The build env (`build.env`, 0.2's older `env.build`), `include`, `exclude`,
    and every buildpack of the group, `pre` and `post`. The project's labels (`id`, `name`,
    `version`, `authors`, `licenses`, `documentation-url`, `source-url`, `metadata`) and `builder`,
    which pack ignores under `--builder`, it leaves out.
  - **Which buildpacks.** Each must be one of two: an inline script with an `id`; or a buildpack the
    builder carries, named by its `id` (and its `version`, if given), with no script. Anything with
    a `uri`, a buildpack-registry name, an image reference, a path, or a version the builder does
    not carry, pack would fetch from its own process or the daemon, outside the floor.

  Every refusal is a line starting `project.toml: `, saying what is refused. A builder whose labels cannot be
  read while it names buildpacks is no refusal: that is the executor's own failure (*The executor's own failure*).
- **On Kubernetes,** where the server sends no `/build` unless the front door's probe found NetworkPolicy
  enforced (contracts/routes.md, the front-door row; #2507), every pod of `canyon-builds` is held
  by the deploy's NetworkPolicy `canyon-builds` (*On Kubernetes*). It allows DNS, and TCP 80 and 443
  everywhere outside the private addresses an app's own policy leaves out (`10.0.0.0/8`,
  `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`, `0.0.0.0/8`, `192.0.0.0/24`,
  `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4` and `168.63.129.16/32`). Other policies may add to it
  (on k3s, the bundled registry), but no peer of any of them holds `169.254.0.0/16` or
  `168.63.129.16`.

## `POST /run`

```jsonc
{ "app": "claims-portal",
  "target": "preview",                        // preview | prod — which container this is
  "image": "canyon/claims-portal@sha256:…",   // this app's digest, never a tag
  "env": { "DATABASE_URL": "…", "STRIPE_SECRET_KEY": "…" },  // exactly this app, this call; PORT and CANYON_IDENTITY_* are refused; on Docker also what the executor's children need (PATH, HOME, TMPDIR, PACK_HOME, XDG_CONFIG_HOME, XDG_CACHE_HOME, PACK_VOLUME_KEY) and any name starting DOCKER_, in any case, which the docker client that starts the app reads; Kubernetes takes them (the env becomes the app's Secret, read by no process of the executor); values are bytes without NUL, ≤ 64 KiB each, multi-line allowed
  "network": "canyon-app-claims-portal",      // canyon-app-<app>, this app's own network; anything else is refused
  "identity": { "issuer": "http://localhost:7777", "audience": "claims-portal",
                "jwks_url": "http://canyon-server:7780/.well-known/jwks.json" },   // → the three CANYON_IDENTITY_* variables
  "keep": ["sha256:…"],                       // optional: this app's images the tag cleanup below spares, by digest — never a tag; at most 16
  "run": "gr_6f1c0b9a7e2a9d3c",               // optional: the server's run id, the label canyon.gate_run — Egress, below
  "egress": { "proxy": "http://canyon-egress:3128", "no_proxy": ["app-postgres"] } }  // optional: the app calls out through Canyon's Envoy — Egress, below
// response
{ "container": "claims-portal-preview", "addr": "claims-portal-preview:3000" }
// on Kubernetes also "answered": { "status": 400, "said": "<h1>Bad Request (400)</h1>" } or { "error": "no answer in 5 s" } — below
```

The container is named `<app>-<target>` and **replaces** any container of that name once it is
**up**: running after a settle window (10 s), not merely started, and answering on port 3000. Until
then the previous container is kept aside, and a start that fails, or an app that exits or restarts
inside the window, or runs with nothing answering on port 3000, hands it back and answers `502` with
the reason: a failed redeploy leaves the version that ran running. A `/run` answers `200` only once the previous
container is removed, so no instance of the earlier version serves past it (contracts/routes.md, *Interception*,
Trust). For an app that exited, restarted
or answers nothing on port 3000 the answer is `502 {error, output}`: `output` is the app's own last 20
lines of stdout and stderr — on Kubernetes the running instance of the `app` container when it answers
nothing, and for one that stopped its termination message (*On Kubernetes*), never a sidecar's — with each secret value replaced by its name as `<NAME>`, the longest first, then cut to
its last 8 KiB; `""` when it wrote none. The secret values are the request's `env` and nothing else:
the app's runtime secrets and the values its resources give, each non-empty one and each line of a
multi-line one, as it is and with its trailing carriage return trimmed — and, for one that parses as a URL whose userinfo carries a password, that password
on its own, decoded, as `<NAME password>`: `DATABASE_URL` is a container for its password, which an
app that fails to connect logs alone (the gate reads a URL's userinfo password as its own secret
too, `gate/review-gitleaks.toml`). What the executor sets itself — `PORT`, `CANYON_IDENTITY_*` — is
never masked, so a `3000` in a line stays `3000`. A secret the app itself encodes or cuts up is not a
value it was handed, and is not masked. A short value masks poorly: a secret set to `1` turns
`port 3001` into `port 300<FOO>`, which garbles the line and gives the value away, so a masked
`output` is not proof that no secret can be read from it. Its container is gone once the answer is
sent, so these lines are the only ones left of it. A sidecar that restarts, or an app not up by the
deadline for any other reason, is answered with the reason alone — `canyon-identity`'s ending in its
last words, quoted, which the server logs and never shows the app's builder (*On Kubernetes*). Preview and prod of one app therefore coexist, which is
what promote needs (invariant 4). The executor sets `PORT=3000` in every container and `addr` is
always `<container>:3000`; an app that listens elsewhere fails loudly rather than answering an address
that is never live.

**An image that is not there is `404`** (#1244), `{error}`, and nothing else is: on Docker, `docker image
ls` of the repository `canyon/<app>` exits 0 and lists no image of its ID — the tag cleanup below, or
`/forget`, removed it; what docker writes on stderr never decides it, and a daemon that does not answer is
`502`; on
Kubernetes, the registry answers `404` for its digest when the `/run` reads the image's architecture, before
anything is written (#2440), or the new pod's `app` container waits to pull it (`ErrImagePull` or
`ImagePullBackOff`) and the registry answers `404` for its digest, asked once per `/run`, and what the `/run`
wrote is undone as after any failed `/run`. A pull that fails for another reason waits out the deadline as before. The server reads
the `404` as the image gone: a deploy that reused an image builds it again (routes.md, the deploys row).
An executor from before this answer says `502`, which ends such a run as Canyon's error, so a host
deploys the executor before the server.

**An app that exits is started again.** On Docker the container runs under `--restart on-failure:10`:
an app that exits non-zero is started again, docker's delay doubling from 100 ms to at most a minute,
ten times in the container's life. Every `/run` makes a new container, and the executor starts one
again only to bring it back behind its floor (*An app reaches no private, link-local or host address*,
Reconcile), so each deploy gives the app ten: enough for an app that crashes now and then to keep
serving between deploys, though it reads `restarting` from its first restart and is never promoted from
(*Restarting*), and a bound on one that crashes at once, which then stays stopped instead of
flapping for ever. Past the ten, or after an exit 0, the container stays stopped, and `/running`
says so. Up is the *first* start staying up: after the
settle window the executor reads the container's restart count with its state (`docker container
inspect`, `{{.RestartCount}}`), and a count above 0 is an app that exited inside the window, reason
exactly `the app did not stay up: its container restarted`, answered with its last lines like any
exited start. On Kubernetes the pod's `app` container restarts as every Deployment's does
(`restartPolicy: Always`, the kubelet's backoff up to five minutes, no bound), and a restart before
the pod is up fails the `/run` (*On Kubernetes*).

**Port 3000.** An app is up only once something answers on port 3000 of its container, at the address
the server uses. On Docker, after the settle window, the executor tries it once from a helper that
shares the app container's network namespace and nothing else of it, so what the app's image holds
does not matter: `docker run --rm --network container:<container> <busybox> nc -z -w 2 <container> 3000`,
busybox by digest (`CANYON_IMAGE_BUSYBOX`), as user 65534 with every capability dropped and no
privilege escalation. `<container>` resolves there as it does for the server, to the app's address on
its one network, so an app listening on 127.0.0.1 alone answers nothing. On an open network the helper
joins the app's holder and tries the holder's own name, the address only this call's app answers on (*An
app reaches no private, link-local or host address*). On Kubernetes the `app`
container's readiness probe is a TCP connect to 3000 (below).
A start whose app is running with nothing answering there is a failed start, reason exactly
`the app is running and nothing answers on port 3000: it must listen on $PORT`, answered with its last
lines like any exited start. An app has the settle window to answer on Docker, and the 300 s a pod has
to become available on Kubernetes. A helper docker cannot start — its image not pulled, the daemon not
answering — is the executor's failure, not the app's: the start fails with the reason alone, as when
the status cannot be read.

**The app's identity.** The executor sets `CANYON_IDENTITY_ISSUER`, `CANYON_IDENTITY_AUDIENCE` and
`CANYON_IDENTITY_JWKS_URL` from `identity`, in every container — the audience is the app id: how the app verifies the signed-in
person's token (design/PERMISSIONS.md), the same three names on every host. `env` naming any
`CANYON_IDENTITY_` variable is `400`, as `PORT` is.

**The app's network.** An app's containers join `canyon-app-<app>` and no other network, so two
apps' containers never share one (invariant 11); a `network` other than the request's app's is
`400`. The executor creates the network when a `/run` finds none, under one lock — one /24 of
`CANYON_APPS_POOL` (default `172.28.0.0/16`), a /24 docker refuses as overlapping skipped, its
gateway the /24's first address (`cmd/canyon-server/appnet.go`'s rule), a pool with no free /24 a
`502` naming it — and connects each container named in `CANYON_EXECUTOR_JOIN` to it: container
names, comma-separated (default `canyon-server,app-postgres`), which the compose file pins. The
server proxies inward, and the app reaches its database as its own login alone. The server's
provisioning login reaches app-postgres over a network outside the pool from one pinned address,
the only one pg_hba admits it from; no app network carries it. From its start and every 15 s, the
executor connects each joined container to every `canyon-app-*` network it is not on: a recreated
container — a compose recreate of the server or app-postgres — comes up on its own networks alone,
and reaches the running apps again within 15 s, not at each app's next `/run` (**amended
2026-10-04**, #1104). `/stop` of the app's last container
disconnects the joined containers and removes the network; `/forget` disconnects the joined
containers from, and removes, one a crash left. The control plane's `CANYON_APPS_SUBNET` takes the
pool: its API listener's accept guard refuses every address of each /24 but its gateway, where host
traffic arrives from, and every IPv6 peer but loopback (#1109), and its JWKS listener (`identity.jwks_url`) serves the pool.

**What a `/run` creates is labelled, on Docker** (**amended 2026-10-09**, #1891). The container
`<app>-<target>` carries `canyon.app=<app>` and `canyon.target=<target>`, which it keeps once set
aside as `<app>-<target>-previous`; the network `canyon-app-<app>`, when the executor creates it,
carries `canyon.app=<app>` — the Kubernetes labels' names (*On Kubernetes*); the floor holder an app on an
open network runs in carries `canyon.app` and `canyon.target` too (**amended 2026-10-26**, #2503). The
executor never reads them but to find an app's floor holders (*An app reaches no private, link-local or host
address*): which app a container or network belongs to stays its name. They let a person list
what the executor made, and only that, with `docker ps -a --filter label=canyon.app` and
`docker network ls --filter label=canyon.app` — which is how the demo's `make demo-down` removes it,
since compose did not create it and cannot.

**What an app's things are labelled with: the executor's own compose project** (**amended 2026-11-25**, #178). At
start the executor reads the `com.docker.compose.project` label of its own container, found by its hostname, which
Docker sets to the container's ID; outside a container, in one compose didn't start, or with Docker not answering,
it has none. With one, everything it makes for an app also carries `canyon.project=<that project>`: the container
`<app>-<target>`, its floor holder, the network `canyon-app-<app>`, and the two build cache volumes, which it now
creates with `canyon.app=<app>` (and `canyon.project`) before pack, which would make them unlabelled. pack can't
label the image it builds, so the executor gives each image a build or a `/rebase` makes, tagged
`canyon/<app>:<package>-<nonce>`, a second name on the same ID, `<project>/<app>:<package>-<nonce>`. Every name an
app is read by stays `canyon/<app>`, and the executor's own cleanup of an app's images (after a `/run`, and
`/forget`) removes both names. `canyon demo down` finds what the demo's executor made by
`canyon.project=canyon-demo` and its images by `canyon-demo/*`, never by an app's id, which another Canyon on the
same Docker can share (cli.md §6.14).

**Egress** (**amended 2026-10-11**, #2085; design/EGRESS.md §2.2–2.3). `run` is the server's id of
the run that asked for this `/run`, `gr_` and hex (contracts/events.md, `run`), and the executor writes
it as the label `canyon.gate_run` on what it starts — the container on Docker, the pod on Kubernetes —
so a call out names the run that started its caller (`POST /callers`, below). `egress` is present when
the run's recorded egress spec says `via` `canyon`, absent when it says `off` (design/EGRESS.md §2.5):
the server decides it, never the repository. With it the executor sets, in the `app` container and on
Kubernetes in `canyon-identity` and `oauth2-proxy` too, `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy` and
`http_proxy` to `proxy`; `NO_PROXY` and `no_proxy` to `no_proxy`, then the host of `identity.jwks_url`,
`localhost` and `127.0.0.1`, comma-separated, each once, so the app's token check and its database never
reach the proxy; and `NODE_USE_ENV_PROXY=1`; and appends `-Dhttps.proxyHost`, `-Dhttps.proxyPort`,
`-Dhttp.proxyHost`, `-Dhttp.proxyPort` and `-Dhttp.nonProxyHosts` of the same (its hosts `|`-separated,
as Java reads them) to `JDK_JAVA_OPTIONS`, after any value `env` gives it — never `JAVA_TOOL_OPTIONS`, where apps put `-Xmx`. With `egress`, `env`
naming any of the variables it sets but `JDK_JAVA_OPTIONS` is `400`, as `PORT` is. `proxy` is
`http://<host>:3128`; `no_proxy` is the host of `env`'s `DATABASE_URL` when it names one — the app's
database (contracts/provisioner.md, `postgres`), inside the network — at most 32, each a host name or
an IPv4 address; a `run` or an `egress` out of these shapes is `400`. Without `egress` the executor sets
none of these, and an app reaches out directly, behind its own floor (*An app reaches no private,
link-local or host address*, below; **amended 2026-10-26**, #2503). The Docker executor routes an app through Envoy (**amended
2026-10-15**, #2095, below), and so does the Kubernetes executor (**amended 2026-10-15**, #2097; *On
Kubernetes*, `/run`). The server sends `egress` where the run's spec says `canyon` — and onto an app whose network is internal on every
`/run`, whatever spec was recorded (design/EGRESS.md §2.5; **amended 2026-10-15**, #2254, the owner's decision), and
on Kubernetes on every `/run` while the host's via is `canyon`, whatever spec was recorded (§2.5; **amended
2026-10-17**, #2286) — and sends `run` only beside `egress`.

**`egress.ca`** (**added 2026-11-07**, #2135; design/EGRESS.md §4.4; both executors read it from #2143 on,
refusing it until then as any key they do not know, and the server sends it from then). The roots an app trusts
for a destination Canyon adds a credential to (contracts/routes.md, *Interception*). `ca` is one to 8 roots, each
`{name, pem}`: `pem` one CA certificate, PEM, at most 8 KiB; `name` its OpenSSL subject-hash file name, `<8 hex>.<n>`,
`n` from 0 and each name once — the only names Paketo's JVM certificate loader reads. The executor puts each root
under its `name`, all of them in the order given in `bundle.pem`, and in `system-and-canyon.pem` the system roots of
the executor's own image, its `/etc/ssl/certs/ca-certificates.crt` read at each `/run`, then the same roots, all
read-only at `/etc/canyon-egress/` in the `app` container. It sets there
`SSL_CERT_DIR=/etc/ssl/certs:/etc/canyon-egress` and `NODE_EXTRA_CA_CERTS=/etc/canyon-egress/bundle.pem`, which add
to the trust a runtime already has, and `REQUESTS_CA_BUNDLE=/etc/canyon-egress/system-and-canyon.pem`, which Python's
`requests` reads in place of its own roots, so it holds both and a call to a destination with no route still checks
out. When `env` names any of the three, the executor sets none of the three, and the app's own values stand: the
server records that run as carrying no root (below), so nothing there is intercepted. The executor never refuses a
`/run` over these. A system roots file missing, or with
no certificate it parses, fails the start, never a `system-and-canyon.pem` of Canyon's roots alone. A JVM takes the roots at launch through that
loader, which reads `SSL_CERT_DIR`, so `JDK_JAVA_OPTIONS` gains no trust property. On Docker the executor copies them
into the container before it starts; on Kubernetes they are the ConfigMap `canyon-egress-ca` (*On Kubernetes*). A
`ca` out of these shapes, or a `pem` that does not parse as one CA certificate, is `400`, and a root that cannot be
put in place fails the start: an app never starts without the trust its `/run` named. While the host's
`credential_routes` is on (contracts/routes.md, the egress setting row; the owner's decision, 2026-11-09, #2143), the
server sends the host's current roots on every `/run` that carries `egress`, so a route works as soon as the policy
names it, with no redeploy; while it is off it sends none, and an app trusts Canyon's root no longer than its next
deploy after it was turned off. `REQUESTS_CA_BUNDLE`'s file holding the system roots and Canyon's, in place of the
roots Python's `requests` carries, is the owner's decision too. Each `/run` carries the roots it sends, by fingerprint, or none when `env` names one of the three, and the
server records them on the env's deploy row, `egress_roots`, which decide whether egress-control intercepts the env's
routes (contracts/routes.md, *Interception*, Trust).

On Docker, a network the executor creates for a `/run` that carries `egress` is `--internal`, its
gateway `-o com.docker.network.bridge.gateway_mode_ipv4=isolated` — no address of the host's on it, so a
service the host runs on every address is out of the app's reach (Docker 28.0 or later, as `--gw-priority`
already needs) — with `--ip-range` the /24's upper /25, so docker hands out addresses there alone, on the
pool's first /24 no network on the daemon holds (with `--ip-range` docker refuses no overlap, so the
executor reads every network's subnet first); on it the container starts at a fixed address in the lower
/25 (`--ip`): preview `.10` or `.12`, prod `.11` or `.13`, whichever its `-previous` does not hold while a
redeploy sets it aside, so a redeploy alternates between the two (**amended 2026-10-15**, #2095). The
network created for a `/run` without `egress` is as before, and the container takes the address docker
gives. Docker cannot make an existing network internal: a `/run` carrying `egress` on a network that is
not is `409 {error}`, naming the network — never an app started past the floor — until `POST
/networks/recreate` (below) recreates it. A `/run` without `egress` on a network that is internal is `409
{error}`, naming the network: the app would reach nothing outside it, so it is refused where a person
sees it, never started cut off. A `/run` never changes a network: only `POST /networks/recreate` moves an
app across the floor, either way. `canyon-egress`, the container Envoy runs beside, joins every internal app
network — the containers `CANYON_EXECUTOR_EGRESS_JOIN` names, comma-separated, default `canyon-egress` —
beside `CANYON_EXECUTOR_JOIN`'s, and never a network that is not internal, so a holder that is down fails
only the `/run`s routed through it. **The door must answer** (**amended 2026-10-15**,
#2095): with `egress`, up also means Canyon's egress gateway answers at `proxy`'s host on 3128 from the
app's own network namespace, tried once after the port check by the same helper (`nc -z -w 2 <host>
3128`). Nothing answering there — `canyon-egress` not joined, Envoy down — is the executor's failure, not
the app's: the start fails with the reason alone, `nothing answers at <host>:3128, Canyon's egress
gateway, from the app's network`, `502`, and the container that ran is handed back, as after any failed
start, so a deploy never leaves an app running whose calls out have no door.

**An app reaches no private, link-local or host address** (**amended 2026-10-26**, #2503: an app on an
open network reached anything its host reached, the host itself included, so under `demo` a connection
through the host's own forward to its loopback publish — `host.docker.internal` on Docker Desktop, OrbStack
and Colima — was served as this machine, an admin). On Docker an app on an internal network (`egress`,
above) reaches nothing but its own network and Envoy. Envoy's own floor (deploy/egress/floor.sh) refuses
Canyon's networks, loopback, `0.0.0.0/8`, every private, shared, link-local and reserved range, the metadata
addresses and every public port but 80 and 443 (amended 2026-10-27: `0.0.0.0/8`, where OrbStack's
`host.docker.internal` is), and every address of the Docker host's own, a public one among them, before 80 and
443 return (amended 2026-10-27, #2524). Compose's `egress-host`, a container in the host's network namespace
and nothing else of it, with every capability dropped, lists them with `ip addr show` as the executor's own
reading does (*The host's addresses*, below), with the kernel's `boot_id` on the first line and `end` alone on
the last, into a volume Envoy's holder reads at its start; it removes any listing first. A listing that is
missing, cannot be read, was written under another boot than the holder's (a cloud host's public address may
move across a reboot), is cut short (its last line not `end`, a line without its newline read, never skipped), or
names no address but loopback stops the floor before its first rule, so Envoy never starts without the host's own. The
listing is read only as the holder starts, never again as the host's addresses change: an address the host takes
later is out of the floor until egress starts again, the residual an app's holder states too (*The host's
addresses*). One on an open network, a `/run` without `egress`, runs in the network namespace of a
holder of its own, whatever the server's sign-in mode:

- **The holder.** Before anything of the app is set aside or started, under the network lock, the
  executor starts `canyon-appfloor-<nonce>` (8 hex) on `canyon-app-<app>`, with `--network-alias
  <app>-<target>`, so the server reaches the app at `<app>-<target>:3000` as before. It runs
  `CANYON_IMAGE_EGRESS_HOLDER`'s image, as a build's holder does: every capability dropped but
  `NET_ADMIN`, `no-new-privileges`, a read-only root, `--memory 32m`, `--pids-limit 32` (the floor's
  script and the executor's checks run in its cgroup beside its `sleep`), `--restart no`,
  and the labels `canyon.app=<app>`, `canyon.target=<target>` and `canyon.holder`. Its entrypoint is
  `executor/appfloor.sh`, which applies the floor in its own namespace and then sleeps. The app's
  container is started with `--network container:<holder>` and the label `canyon.floor=<holder>`, and
  has no network endpoint of its own; it holds no capability, so nothing in the namespace but the
  holder's own script may change it. Every app container carries `canyon.floor` as `/run` sets it —
  empty on an internal network — so a `LABEL` its image carries never stands: a run's own label
  overrides the image's. While a redeploy runs, its new holder and the one set aside both carry the alias,
  so `<app>-<target>` answers with either until the one set aside goes with its container.
- **The host's addresses.** Before the holder starts, the executor reads every IPv4 address of the Docker
  host's own — on Docker Desktop and Colima the VM's — with `ip addr show`, run by the port check's
  busybox (`CANYON_IMAGE_BUSYBOX`) in the host's network namespace and nothing else of it, as user 65534
  with every capability dropped, and hands them to the floor, each a /32, a public one among them. They
  are the host's as the holder is created; a deploy reads them again. They are read before the network
  lock is taken. A listing that fails, or names no address but loopback, fails the `/run` (*Fail
  closed*). Under rootless Docker `--network host` is RootlessKit's namespace, not the host's: the floor
  then refuses that namespace's addresses, and the host's own are what RootlessKit's network lets through.
- **The floor.** The chain `CANYON-APP`; the script's last rule inserts the jump into it at the head of
  `OUTPUT`. In order:
  1. the namespace's own loopback, where Docker's resolver (127.0.0.11) answers, returns;
  2. the network's gateway (the host's address on it), every address of the host's own (above),
     `169.254.0.0/16`, `168.63.129.16`, `100.100.100.200`, `0.0.0.0/8`, `127.0.0.0/8`, `224.0.0.0/4` and
     `240.0.0.0/4` are rejected, established connections among them (amended 2026-10-27: these come
     before established connections return, so none to the host outlives the floor);
  3. established connections return;
  4. the network's own /24 (the server, app-postgres) returns;
  5. `CANYON_APPS_POOL` and `CANYON_EGRESS_POOLS` are rejected;
  6. each `CANYON_APPS_ALLOW` entry returns;
  7. `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `192.0.0.0/24` and
     `198.18.0.0/15` are rejected;
  8. everything else — every public address, on any port, as before — returns.

  `FORWARD` drops, and where the namespace has IPv6, `ip6tables` drops all of it but loopback, in a chain
  of the same name. So `host.docker.internal` — host-gateway: Docker's bridge on Linux, `192.168.65.254`
  on Docker Desktop, `0.250.250.254` on OrbStack, `192.168.5.2` on Colima, a link-local or slirp address
  on Podman — is refused with every other address of the host's on a private, shared or link-local range,
  and a public address on the host's own interfaces is refused by name.
- **Waiting for the floor.** The executor waits, for at most 30 s, until both jumps are in place, and only
  then sets the previous container aside and starts the app.
- **`CANYON_APPS_ALLOW`.** The private addresses and ports apps on an open network reach besides their
  own, such as a company database: comma-separated entries, each `<IPv4 prefix>:<TCP port>`, refused at
  the executor's start exactly as `CANYON_BUILD_ALLOW`'s are (*A build reaches no private or link-local
  address*), so no entry opens link-local, the metadata addresses, loopback, Canyon's networks or any app
  network's gateway, which is in `CANYON_APPS_POOL`; nor, rejected before every entry, any address of the
  Docker host's own that its holder read. An entry that overlaps `192.168.65.0/24`, `192.168.5.0/24` or
  `10.0.2.2` — where `host.docker.internal` is on Docker Desktop (`.254`, `.2` before it), Colima and
  Podman — stops the executor's start too, naming it (amended 2026-10-27). An entry whose range holds another address of the host's that no
  listing shows — the Mac's own — opens it to every app, and under `demo` the dashboard as an admin: the
  configuration reference says never to name one.
- **Fail closed.** A network whose gateway is not in its /24, the host's addresses not read, a holder that
  does not start, or a floor not in place within the bound fails the `/run`, `502`, its reason starting `app network: `, before the
  previous container is set aside, and the holder is removed. No app this release starts runs on an open
  network but in a floored holder's namespace.
- **Apps deployed before the floor** (amended 2026-10-27). An app an executor from before this release
  started runs on its open network directly, unfloored, until its next deploy: **redeploy every app once
  after upgrading**. The executor does not stop it. It logs each `<app>-<target>` on an open `canyon-app-*`
  network that runs in no holder once a process, on the first reconcile pass that finds it (its start's
  among them), `<container> runs on <network> with no floor, deployed before the floor: redeploy it`. A redeploy whose start fails never names such a
  container back: where a `/run` on an open network would hand the one set aside back, one that runs in no
  holder is removed instead, and the app is left down, the start's own failure answered as before.
- **Cleanup.** A holder of the app's that none of its containers — `<app>-<target>` and the `-previous`
  of each — names in `canyon.floor` is removed: after a `/run` removes the one set aside, after a failed
  start hands it back, and at `/stop` and `/forget` before the network goes; `POST /networks/recreate`
  removes every holder of the app's after its stop. A holder is a container labelled `canyon.holder` and
  the app's `canyon.app` *and* named `canyon-appfloor-<8 hex>`, a name no image sets, so the app's own
  container, whatever its image's labels, is never one. The executor reads `canyon.holder`, `canyon.app`,
  `canyon.target` and `canyon.floor` for these and for the reconcile, and no other label of these.
- **Reconcile.** Docker restarts no holder, and starts no app whose holder is not running: an app joins
  `container:<holder>` at each start, and docker refuses it with no holder running. At the executor's
  start and every 15 s after, with the rejoin (*The app's network*), the executor lists the containers
  labelled `canyon.app` once and inspects them once (and, once, any holder they name outside that list);
  it takes the network lock only to act, and inspects the pair again under it before acting. For each
  `<app>-<target>` that names a holder that exists (never a `-previous`, which a `/run` holds), and only
  once that holder is proved the app's own — named `canyon-appfloor-<8 hex>`, labelled `canyon.holder`
  with the app's `canyon.app` and `canyon.target`, and the very container docker joined the app to (the
  app's `HostConfig.NetworkMode` is `container:<the holder's ID>`, which docker resolves at create and no
  image sets); any other is refused and logged, and nothing is started or stopped. In each case below the
  app is stopped first (a no-op on a stopped one, which cancels any restart docker has pending), the holder
  started where it is not running, the floor waited for as above, and the app started last:
  - the holder not running, and the app running, in the namespace the holder's stop emptied;
  - the holder not running, and the app stopped with the error of a refused start (`State.Error`: docker's
    own start under its restart policy, refused for want of the holder, after a daemon's restart or the
    host's reboot);
  - the holder running, and the app stopped with the error of a start (amended 2026-10-27);
  - the holder started after the app (`State.StartedAt`), a namespace the app no longer shares.

  Anything else is left as it is: an app that stopped by itself — past its restarts, or with exit 0 —
  stays stopped, as docker leaves it today.

  The same pass removes, under the lock, each holder that no container of its app names (*Cleanup*), so
  an orphan never keeps the app's alias, and logs each app deployed before the floor (above).
  Once the reconcile has stopped the app (amended 2026-10-27), any later failure — the holder's start, or
  its floor not in place in time — leaves the holder stopped and asks docker to start the app, which
  docker refuses for want of the holder and records as the app's `State.Error`; the next pass reads that
  as a refused start, and starts both. The app is never left stopped with no error, which no pass would
  start again, and never started while its holder runs without its floor: that start is asked only once the
  holder's stop succeeded and an inspect reads it stopped; a stop that fails, or an inspect that fails or
  finds no holder, leaves the app stopped, logged. A final start of the app that fails goes the same way
  (amended 2026-10-27, #2523), and each action under the lock runs on a deadline of its own, never on what
  the pass's listing and inspects left of theirs, so a pass that ran long never cuts one short between the
  app's stop and its start. Every name the executor hands
  docker after reading it from a label follows `--`, so a label an image set is never read as a flag.
  So after a daemon's restart or a reboot each app docker would have started again is back, floored,
  within one pass of the executor's start, and none is ever started outside its holder's namespace.

**Kubernetes only** (the Docker executor refuses each with `400`): `/run` also takes `access`, the
app's access file (`PUT /access`, below), and `front_door`, how people reach the app — both required
there, else `400` — and `database_ca`, optional: the apps database's CA bundle (*On Kubernetes*, the
apps database). What it writes from them: *On Kubernetes*, below.

After a `/run` has answered `200`, the executor removes the tags of repository `canyon/<app>`
(exactly — never `canyon/<app>-other`) except those of an image that one of `<app>-preview`, `<app>-prod`
or their `-previous` runs, or that `keep` names, at most 50 removed per call — a spared tag never
counts — with a plain `docker image rm`: a tag docker refuses is kept. It lists those tags once,
before it removes any. It is logged, never answered: it neither delays nor fails the `/run`.

**What `keep` spares** (**amended 2026-10-11**, #2044). This file owns its shape; design/OPERATIONS.md
§1.5 owns which images the server sends. Each entry is the value after `@` in an `image` of this app —
on Docker the image ID, on Kubernetes the registry digest — exactly `sha256:` and 64 lowercase hex
digits, and the cleanup after this `/run`, on Docker or in the registry, removes none of them. A tag
moves, so a kept tag would spare whatever it named at cleanup, not the image the server meant. A `keep`
that is not a list of at most 16 such digests — a sanity bound, not a budget — is `400 {error}`,
answered before anything else is done. A digest twice is the same as once, `[]` the same as no `keep`,
and `keep` may name the call's own image. It only narrows the cleanup, which never leaves repository
`canyon/<app>`, so the executor need not check that a digest is this app's: one naming no image there
spares nothing and is no error, and an image already gone does not fail the `/run` — a later `/run` of
it is `404` (*An image that is not there*, above). `keep` is this call's alone: the executor remembers none of it, so a kept image
is held until a `/run` of the app omits it, whose cleanup may remove it. `/forget` takes no `keep` and
ignores one sent, as any field it does not know: a deleted app keeps nothing. An executor from before
`keep` ignores the field on both hosts — `/run` on Kubernetes is the Docker executor's handler, which
reads no field it does not know (`executor/http.go`, `decode`) — and removes those images; the server
reads them as gone at that `404`, so a host deploys the executor before the server.

## `PUT /access` — Kubernetes only

```jsonc
{ "app": "claims-portal", "target": "preview",
  "access": { "updated_at": "2026-09-25T10:00:00Z", "groups_claim": "groups",
              "subject_issuer": "https://login.meridian.example",
              "members": { "domains": ["meridian.example"], "emails": [] },
              "admins": { "emails": ["dana@meridian.example"], "claim": "groups", "value": "canyon-admins" },
              "roles": { "approver": "Approves claims over 10,000 EUR" },
              "entries": [{ "kind": "email", "grantee": "dana@meridian.example", "subject": "…", "level": "owner", "roles": ["approver"] }] } }
// response: 204, no body · 404 when nothing runs as <app>-<target> · 409 {error:"stale"} when the
// ConfigMap already holds a newer updated_at, and nothing changes · not routed by the Docker executor
```

Rewrites the ConfigMap `canyon-access-<app>-<target>` and nothing else: the identity sidecar reads
the file on every request, so a share or an unshare reaches the app without a restart. The server
serialises its re-PUTs per app, so a second-granular `updated_at` orders them. The file is
the app's grants (routes.md, *App teams*) with the org's membership lists, its admins and the roles
its current package declares: `entries[]` are `{kind, grantee, subject, level, roles[]}`, an
`everyone` entry `grantee` `""` at `viewer` only. `subject_issuer` is the issuer of Canyon's own
sign-in, which recorded every entry's `subject` — `""` with sign-in off, where none is ever recorded (**amended 2026-10-16**, #2220): the sidecar matches an entry by its
`subject` only on a token from that sign-in (*On Kubernetes*, the front door). A grant bound under
another issuer is read as not yet bound (routes.md, *App teams*), so its entry's `subject` is `""` and
it matches by address (**amended 2026-10-17**, #2306). An executor from before
the field refuses a file carrying it, so a host deploys the executor before the server.

## `POST /probe` — Kubernetes only

```jsonc
{ "domain": "canyon.meridian.example", "form": "app.env.domain" }   // as /run's front_door carries them
// response 200 — the probe ran; its two halves run side by side, and past 60 s it is 502
{ "network_policy": "enforced", "certificate": "ok", "fix": "", "warning": "" }
// 400 {error} on a domain or form front_door refuses · 502 {error} when the probe could not tell (below)
// · not routed by the Docker executor
```

Whether this cluster keeps an app's port behind its sign-in, and whether people can reach the
addresses `domain` and `form` give. One probe runs at a time (a second waits); every object it makes
carries the label `canyon.probe` and is deleted before it answers, and any a crash left is deleted when
the executor starts.

- **`network_policy`.** In `canyon-apps`, three one-pod Jobs (`restartPolicy: Never`, `backoffLimit: 0`)
  of the probe image (busybox by digest, `CANYON_IMAGE_BUSYBOX`) — a pod no controller owns is not reliably
  under NetworkPolicy on every plugin: a target serving HTTP on 4180 and 8080, under a NetworkPolicy
  admitting 4180 exactly as an app's admits it (*On Kubernetes*) and 8080 from the prober alone; a
  canary serving HTTP on 8080 under no NetworkPolicy of its own, started alongside the target; and a
  prober, without the front-door label, started once both run, under a NetworkPolicy letting it out to
  the target on those two ports and nowhere else, which writes each attempt to its log for the executor
  to read. The prober first tries the canary (below), then opens TCP to the target's pod IP on 8080
  until it connects, for up to 30 s (else `502`: a probe that cannot reach its own target proves
  nothing), then tries pairs every 3 s for 15 s — 8080, then 4180, 3 s each — and the last pair
  decides, then tries its node (below). **`enforced`**: its 4180 got no answer while its 8080
  connected, and the canary did not answer — the cluster drops what the policy does not admit.
  **`not_enforced`**: its 4180 connected — the plugin ignores NetworkPolicy, and any pod could reach
  any app around its sign-in — or the canary did. Anything else, a refused connection or a
  failed 8080 among them, is `502`.
- **A new pod's first moments.** From its first instant, before it tries its target, the prober tries
  the canary's 8080 back to back for 5 s, 1 s timeout each. Any TCP connect counts, and nothing is
  sent. Its policy does not admit the canary, so **any connection there is `not_enforced`**, whatever
  the pairs say: the plugin lets a new pod out before its policy is in force, so an app's first
  moments reach anything (VPC CNI's default mode, k3s's own controller). A plugin that applies a pod's
  policy before its first packet answers none of them.
- **The node.** After the pairs, the prober opens TCP once to its own node's address (the Downward
  API's `status.hostIP`) on each of 10250 (the kubelet, which every node runs) and 22 (sshd), 3 s at
  most each, which its policy does not admit. Any TCP connect counts, whatever would follow, and
  nothing is sent. **A connection there is a `warning`, never `not_enforced`** (the owner, 2026-10-03):
  the plugin does not govern a pod's traffic to its own host (the AWS VPC CNI, even in strict mode,
  #1435), so an app reaches the services on the server it runs on, and closing that is a host
  firewall rule, the operator's to add (deploy/k8s/README.md, *Host firewall*). `warning` is `""`
  when neither port answered, else
  `Apps on this cluster can reach the server they run on (ports <ports>). Your cluster admin can close this with a host firewall rule.`
  — `<ports>` the ports that connected, in the order tried, joined by `, `. It rides beside any answer
  and never changes `network_policy` or `fix`.
- **`certificate`.** From the executor, a TLS handshake on 443 with each address the form gives an app
  named `canyon-probe` — `canyon-probe.preview.<domain>` and `canyon-probe.prod.<domain>`, or
  `canyon-probe-preview.<domain>` and `canyon-probe-prod.<domain>` — verified against the system roots
  for that name, 5 s each; nothing is sent after the handshake. **`ok`**: both verified. **`missing`**:
  either did not resolve, did not answer on 443, answered but completed no TLS handshake in its 5 s,
  or presented a certificate that does not cover it — people would meet a certificate error, or
  nothing, at every app.
- **`fix`** is `""` exactly when the answer is `enforced` and `ok`. Otherwise it is the sentence for
  each failing half, in this order, joined by one space, which the server shows verbatim. For
  `not_enforced` it is one sentence, the first whose check connected — 4180, then the canary:
  `This cluster does not enforce NetworkPolicy, so an app's port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
  · `New pods on this cluster get out before their NetworkPolicy is in force, so an app could reach anything in its first moments. Have the network plugin apply a pod's policy before the pod starts (on EKS: NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
  For `missing`:
  · `<name> does not resolve or does not answer on 443. Point <names> at the load balancer in front of the apps and save again.`
  · `<name> answers on 443 but completed no TLS handshake within 5 seconds. A gateway that issues certificates on first use may still be issuing this one: wait a minute and save again. Otherwise have the gateway serve a certificate for <names>.`
  · `The certificate at <name> does not cover it. Add <names> to the certificate and save again.` —
  the first when no connection to 443 could be made, the second when one was made and the handshake
  failed or did not finish for any reason but the certificate's (#1446), the third when the
  certificate did not verify for the name; `<name>` the first address that failed, `<names>` the
  wildcards the form needs:
  `*.preview.<domain> and *.prod.<domain>` for `app.env.domain`, `*.<domain>` for `app-env.domain`.

## `POST /sidecars` — Kubernetes only

```jsonc
// request: no body
// response: 200
{ "image": "ghcr.io/getcanyon-ai/canyon-identity@sha256:…",       // the executor's CANYON_IMAGE_IDENTITY
  "rolled": [ { "app": "claims-portal", "target": "preview", "roll": "4f1c0b9a" } ], // on it by a roll, rollout done
  "failed": 1 }                     // left alone or not moved: not /run's, unread or unwritten, or stuck
// 502 when the executor cannot list the Deployments · not routed by the Docker executor
```

Rolls every app's sign-in sidecar onto the executor's own image after an upgrade (**amended
2026-10-17**, #2305): a `/run` sets `canyon-identity`'s image, so without this an app would keep the
sidecar it was deployed with until its next deploy. Choice 2's oauth2-proxy is not rolled: it keeps its
image until the app's next deploy. A downgraded executor rolls every sidecar back to its older image.

- **What a roll changes.** For a Deployment of `canyon-apps` labelled `canyon.app` and `canyon.target`
  whose `canyon-identity` runs another image than `CANYON_IMAGE_IDENTITY`, the executor sets that image and
  two annotations on the Deployment, in one update: `canyon.sidecar-rolled-to`, the image, and
  `canyon.sidecar-roll`, the roll's name (8 hex characters, new each roll). Nothing else changes: the app's
  image, its environment, oauth2-proxy, the labels — `canyon.run` among them — and every other field stay
  what its `/run` wrote. The Deployment replaces its pod as a `/run`'s does, `maxUnavailable` 0. A
  Deployment on that image already is not written, so an environment is rolled once per image. One a `/run`
  wrote since the list is read again, and left alone when it is being deleted. A call rolls at most 10
  Deployments, each a pod surged at once, and tries at most 30, so Deployments the cluster refuses to
  write (an admission webhook's, say) never hold up the rest; the rest roll on the calls after.
- **What it answers.** `rolled` is state, not news: each environment whose Deployment carries both
  annotations, `canyon.sidecar-rolled-to` being its `canyon-identity`'s image and `CANYON_IMAGE_IDENTITY`,
  and whose rollout is done — the Deployment has seen its template, and its replicas are all updated and
  available — once, with its `roll`. It is answered on every call while those annotations stand: until
  the environment's next `/run`, which writes the Deployment without them, as does a failed `/run`'s
  put-back of its template. So an answer the server lost, or one it could not record, is answered again
  on the next call, unless a `/run` came first: such a roll is never recorded. That window is one call, a
  minute, unless the server cannot write to its database. A roll whose rollout is under way is not
  answered yet.
- **What it leaves alone.** The executor rolls only a Deployment a `/run` made: named `<app>-<target>` by
  its labels, its template labelled `canyon.run`, and its `canyon-identity` mounting the volume `access`,
  the ConfigMap `canyon-access-<app>-<target>`. One being deleted is passed over. Any other, one it cannot
  read or write, and a rolled one whose rollout missed its progress deadline
  (`ProgressDeadlineExceeded`), it logs, naming the Deployment, and counts in `failed` (fail closed). A
  stuck rollout is never undone: with `maxUnavailable` 0 the previous pod serves on.
- **The server.** It calls this when it starts and every minute after, so an executor upgraded after the
  server is caught within a minute. It takes only an answer of this shape — nothing else, nothing after
  it, the image by digest, each environment once with its roll — and records nothing from any other. For
  each `rolled` entry it writes one `update sidecar` row on the app (routes.md, *Audit categories*) the
  first time an answer names that roll of that environment, and never again. A roll is no deploy: no
  review, no version, no run and no deploy row, and `canyon status` and the project read say nothing new.
  An executor from before this call answers `404`, and nothing is rolled.

## `GET /platform?image=` — Kubernetes only

```jsonc
// request: GET /platform?image=123456789012.dkr.ecr.eu-central-1.amazonaws.com/canyon/claims-portal@sha256:4f1c…
// response: 200
{ "os": "linux", "architecture": "arm64" }   // the image config's own, the architecture a /run pins its pods to
// 400 for an image that is not <CANYON_KUBE_REGISTRY>/canyon/<app>@sha256:<64 hex> · 404 {error, reason: "image_gone"}
// for one the registry does not have · 502 for any other image whose platform is not read · not routed by the Docker
// executor
```

The platform an image runs on, for a hand-off release (**amended 2026-10-27**, #2528): the server asks it of each
release it builds — a deploy's, a promotion's, a rollback's — and names it in the release (release.schema.json's
`deploy.platform`), so the org's platform schedules the app's pods on nodes of that architecture, as a `/run` pins
them (*On Kubernetes*, the Deployment). It is read exactly as a `/run` reads it, with the same bounds, before
anything else: the image must be `<CANYON_KUBE_REGISTRY>/canyon/<app>@sha256:<64 hex>`, refused `400` before any
request otherwise, so the executor's registry credential goes to its own registry alone; then its manifest and its
config, by that digest, each read only once it names a size of 1 to 4 MiB. An image index (a multi-arch image), a
config whose `os` is not `linux` or whose `architecture` is empty, or one that cannot be read, answers `502` with
`the image <image>'s architecture could not be read, so Canyon runs it nowhere: <why>`, and the server sends no
release: it fails closed, as `/run` does. Only the architecture is named, never its variant (arm `v6`, `v7`). Like
`/run`, the call is executor-internal: the server's alone, reached from its pods only (*On Kubernetes*), with no
caller of its own and nothing of any app's permissions in it. Hand-off is a Kubernetes host's alone
(design/HAND-OFF.md §1), so the Docker executor does not route it, and an executor from before this call answers
`404` with no `reason` (`no such call: …`): either way no release is sent, so the executor is upgraded before the
server, or with it. Only `reason: "image_gone"` says the image is gone, and no release of it is sent: a rollback's
kept image the registry no longer holds is no longer kept, its last line saying so, as on Canyon's own cluster, and
any other release of it fails `error`. Under `run_by: org` a preview deploy always builds and never reuses an image,
so no deploy builds again here as one does on `/run`'s `404`. Any other `404` is an error, never an image gone.

## `POST /stop`

```jsonc
{ "app": "claims-portal", "target": "preview" }   // preview | prod — which container, as /run names it
// response: 204, no body · 400 on an app or target the naming pattern refuses · 502 when docker fails
```

Removes the container `<app>-<target>`, and `<app>-<target>-previous` when a redeploy cut short left
one set aside, the floor holders no remaining container of the app names (*An app reaches no private,
link-local or host address*), and — when no container of the app is left — the app's network; nothing
else: no image, no volume, no app data. A
container that is already gone is a `204` too (invariant 6). Undeploy and the project delete
are the callers (routes.md).

## `POST /forget`

```jsonc
{ "app": "claims-portal" }
// response: 204, no body · 400 on an app the naming pattern refuses · 409 while any of <app>-preview,
// <app>-prod, <app>-preview-previous, <app>-prod-previous exists · 502 when docker fails
```

Removes every tag of repository `canyon/<app>`, the two build cache volumes
`canyon-cache-<app>-build` and `canyon-cache-<app>-launch`, and the network `canyon-app-<app>` if a
crash left it, with the app's floor holders on it — those names exactly, and nothing else. In the volumes' names
`<app>` is written as pack writes a volume name, so the name the build passes is the one pack creates: each
`aux`, `com`, `con`, `lpt`, `nul` and `prn` in it spelt `a_u_x`, `c_o_m`, `c_o_n`, `l_p_t`, `n_u_l` and `p_r_n`
(`smooth-falcon-lei`'s are `canyon-cache-smooth-falc_o_n-lei-build` and `-launch`; amended 2026-11-15, #2961). Idempotent: what is already gone is a `204`. Deleting the project is the caller
(routes.md, `DELETE /api/v1/projects/:id`), after both `/stop`s; and each server start asks again for
every deleted project, retrying a forget that fails with backoff (one second, doubling to five minutes)
until it succeeds, so an executor rolled with the server, not up yet when the server starts, is not
missed (#1858).

## `POST /callers`

```jsonc
{ "addresses": ["172.28.3.11", "10.0.4.7"] }   // IPv4 source addresses of calls out, each once, 1 to 1,024
// response: 200
{ "callers": [ { "address": "172.28.3.11", "app": "claims-portal", "target": "prod", "kind": "web", "run": "gr_6f1c0b9a7e2a9d3c",
                "since": "2026-10-11T09:14:02.418Z" } ] }
// 400 on a body that is not 1 to 1,024 distinct IPv4 addresses · 502 when the executor cannot tell
```

Who holds each address now (**amended 2026-10-11**, #2085; design/EGRESS.md §2.3): the server asks it
once per batch of Envoy's access-log entries and keeps nothing between batches. The answer names each
address asked that exactly one of Canyon's app containers or pods holds, once, in the order asked; an
address that none holds, that more than one claims, or that a terminating pod holds is left out —
unknown, and its calls are recorded under no app. `kind` is `web` (a scheduled job's run, design/JOBS.md,
is `job` once `/job` exists); `run` is the label `canyon.gate_run`, or `null` on what was started without
one. `since` is a time no earlier than when the holder took the address, RFC 3339 in UTC (**amended
2026-11-04**, #2265): on Docker the container's `State.StartedAt`, docker giving it its address as it
starts — never its `Created`, which is before; on Kubernetes the second past the earliest
`state.running.startedAt` of the pod's containers, each started once its sandbox had the address, which
the API keeps to the second — never the pod's creation or `status.startTime`, which are before. A holder
whose time the executor cannot read, a container docker never started or a pod none of whose containers
runs, is left out: unknown. Late, `since` costs a new holder's first calls, no one's; early, it would name
the holder for a call its address's last holder made. The server's receiver names a holder for a call only
when the call started at or after `since` (design/EGRESS.md §2.3), so a call an app made before it
stopped, its entry read once another app holds its address, is no one's. On Docker the address names a container on a `canyon-app-<app>` network: its fixed address there
(`/run`, Egress, above) names the target, and any other address — in the upper /25, or on a network that
is not internal: what docker handed out, an address no container of the app is pinned to — is unknown; the
container is `<app>-<target>` or its `-previous`, which a redeploy sets aside, by name. On Kubernetes the address is a pod's
`status.podIP` in `canyon-apps`, whose `canyon.app` and `canyon.target` labels name the app and the target;
a pod with a `deletionTimestamp` is terminating, and only a `Pending` or `Running` one is named, while every
pod that lists the address — one that ended among them — claims it (**amended 2026-10-16**, #2097). Read-only: the executor lists pods it already lists for
`/running`, and holds no answer past the call. An executor from before this call answers `404`: the server
records every call of the batch under no app.

## `GET /published` — Docker only

```jsonc
// response: 200
{ "id": "3f1c0b9a7e2a…",                                                   // the container's Id, 64 hex
  "created": [ { "port": "7777/tcp", "host_ip": "127.0.0.1", "host_port": "7777" } ],   // HostConfig.PortBindings
  "held":    [ { "port": "7777/tcp", "host_ip": "127.0.0.1", "host_port": "7777" } ] }  // NetworkSettings.Ports
// 502 when the executor cannot tell: the daemon did not answer, no container is named canyon-server, it is not
// running, or its inspect is not read
```

Where Docker publishes the server (**added 2026-10-26**, #2491; design/AUTH.md, *A demo published beyond
loopback*): the running container `canyon-server`, the name the compose file gives the server and
`CANYON_EXECUTOR_JOIN`'s default joins, as `docker container inspect` reads it. `id` is its `Id`; `created`
the port bindings it was created with (`HostConfig.PortBindings`), and `held` those it holds now
(`NetworkSettings.Ports`), each list's bindings once, sorted by `port`, `host_ip` and `host_port`. `port` is
the container's port and protocol, Docker's key (`7777/tcp`); `host_ip` the host address it is published on,
as Docker spells it: `""` is every address, which a held port lists as `0.0.0.0` and `::`; `host_port` the
host's port, `""` when Docker picks one. A port exposed but published nowhere is in neither list, and a
container that publishes nothing answers two `[]`. A container that is not running is a `502`, never its
bindings: a stopped `canyon-server` left from another demo holds nothing and vouches for nothing (**amended
2026-10-26**, #2501's security review). The facts, never a verdict, as `/rebase`'s: the server judges them.
Read-only: nothing is held past the call. The Kubernetes executor routes no `/published`, and answers the
mux's `404`, as an executor from before the call does.

The server asks it under `CANYON_AUTH=demo` with `CANYON_PUBLISHED_ADDR` set, the one configuration that
serves a Docker network's gateway, each ask bounded at 10 seconds, and serves that gateway only when:

- **the answer is its own container's**: its host name, which Docker sets to the first 12 hex digits of the
  container's `Id` (the compose file sets no `hostname:`), begins `id`. An answer about another container, or a
  host name it cannot read, keeps the gateway closed, and the server asks again;
- **`held` holds at least one binding of the port the server listens on**, so the port is published now; and
- **every binding of that port, in `created` and in `held`, is on a loopback address**, `127.0.0.0/8` or
  `::1`, read as an IP. Any other — `""`, `0.0.0.0`, `::`, a LAN address, anything that is not an IP — stops
  the server, naming the binding, as does a port `held` does not publish at all.

Until an answer comes in this shape the gateway stays closed and the server asks again: a `404`, a `502`, an
answer out of shape, an ask past its 10 seconds or no answer is never read as loopback. A container's bindings
are fixed when Docker creates it, so one answer holds for the process's life.

## `POST /networks/recreate` — Docker only

```jsonc
{ "app": "claims-portal", "internal": true }   // internal: the host's via is canyon (below)
// response: 204, no body · 400 on an app the naming pattern refuses · 502 when docker fails
```

Makes an app's network what a `/run` under the host's via would make it now, either way (**amended
2026-10-15**, #2254, the owner's decision; design/EGRESS.md §2.2): it stops the app's containers —
`<app>-preview`, `<app>-prod` and the `-previous` of each, those the daemon lists — and leaves them stopped;
removes every floor holder of the app's, which a later `/run` brings again (*An app reaches no private,
link-local or host address*; **amended 2026-10-26**, #2503); disconnects every container still on
`canyon-app-<app>`; removes it and creates it again on the same /24 —
`--internal`, its gateway isolated and `--ip-range` the upper /25 when `internal` is true, as a `/run`
without `egress` makes it when false (Egress, above), on the pool's first free /24 when the app has no
network or its /24 is taken — and connects the joined containers again, and on an internal network the
door. **It starts nothing:** a container started without the proxy variables would reach nothing on an
internal network, and one started with them would reach past Envoy on an open one. The server sends
`internal` true when the host's via is `canyon`, false when it is `off`, records the network, then `/run`s
each env Canyon runs, prod first, from its deploy row — its image, today's runtime values, `egress` as `internal` says — and records that
spec on the row. The app is down for those seconds, which an admin's *Recreate networks* says before it
calls. Anything that fails after the stop leaves the app stopped — `/running` says so — never running on
its old network again; a second call repeats the whole of it, a network a failed call removed included.
The Kubernetes executor refuses it with `400`: its NetworkPolicy is written at every `/run`. An executor
from before the call answers `404`, which the server reads as recreation not done.

## `POST /rebase`

**Added 2026-10-18** (#2086; design/PATCHING.md §4.1, §4.3): Canyon's fix of what it put in the image —
a newer run image under the app's unchanged layers. A rebase is never a build: no tree, no build value
and no buildpack goes in, and nothing above the run image's layers changes.

```jsonc
{ "app": "claims-portal",
  "package": "pkg_7d3e8f",                       // the package the image was built from: the fresh tag's prefix, as /build's
  "image": "canyon/claims-portal@sha256:…",      // the image an environment runs, by digest — never a tag; /run's rule
  "run_image": "sha256:…" }                      // GET /builder's run_image, exactly: the one the server's plan named
// response: 200
{ "image": "canyon/claims-portal@sha256:…",      // the rebased image: a new digest, under a fresh tag of its own
  "before": { "diff_ids": ["sha256:…"],          // the image's layers, bottom first, from its config's rootfs
              "top_layer": "sha256:…",           // io.buildpacks.lifecycle.metadata's runImage.topLayer
              "run_image": "sha256:…",           // the run image it is on: GET /builder's run_image's form (below)
              "stack_id": "io.buildpacks.stacks.noble",   // the io.buildpacks.stack.id label
              "build_metadata": "sha256:…",      // the sha256 of the io.buildpacks.build.metadata label's bytes
              "config": "sha256:…" },            // the sha256 of the config's Entrypoint, Cmd, Env, WorkingDir and User
  "after": { … },                                // the same facts of the rebased image
  "duration_ms": 2140 }
// 400 on a request out of shape · 404 the image is not there (/run's rule) ·
// 409 {error: "not_rebasable" | "another_run_image" | "another_stack" | "run_image_moved", message} — never forced ·
// 502 {error, no_room?} the rebase failed, the error as the executor got it · 503 {error: "busy"} with Retry-After (*Busy*)
```

- **`run_image`, before and after** (**amended 2026-10-18**, #2089), is the run image each image is on, in
  `GET /builder`'s form for that host, so the server compares `after.run_image` with the `run_image` it
  planned byte for byte and strips nothing: on Docker the image ID, `sha256:<64 hex>` — the lifecycle
  records the daemon's ID in `runImage.reference` as the 64 hex alone, and the executor answers it with its
  prefix, and any other `runImage.reference` is a fact it cannot read (`502`); on Kubernetes
  `<repository>@sha256:<64 hex>`, any tag stripped (**amended 2026-10-19**, #2091): kpack copies the
  ClusterStack's run image to `<the ClusterBuilder's spec.tag>-run-image` and its `status.stack.runImage`
  names that copy by tag and digest, while the rebaser records the repository and digest alone, so the
  executor answers both in that one form, and any `runImage.reference` but one by digest is a fact it
  cannot read (`502`).
- **A fresh tag.** The rebased image is tagged `canyon/<app>:<package>-<nonce>` — on Kubernetes under
  `CANYON_KUBE_REGISTRY` — as a `/build`'s is, and the image the request names keeps its own tag: the
  server can `/run` either, and the tag cleanup after a `/run` treats the new tag as any other (*What
  `keep` spares*).
- **The facts, never a verdict.** `before` and `after` are what the server compares to prove that only
  the run image changed (design/PATCHING.md §4.3, the proof): the diff IDs above `top_layer`, in order,
  `build_metadata` and `config` the same, on the same `stack_id`. The executor answers the facts and
  judges none of them. `build_metadata` and `config` are digests — a sha256 over the label's bytes, and
  over the JSON object of the five config fields with its keys sorted — so no value of the image's
  environment leaves the executor; the server compares them, and nothing reads them back.
- **Refused, never forced** (`409`, nothing written): an image whose `io.buildpacks.rebasable` label is
  anything but `true` or absent (`not_rebasable`; **amended 2026-10-18**, #2089: the lifecycle refuses
  `false` alone, and Canyon fails closed on any other value); a run image whose name is not the one the image's lifecycle metadata
  names, `runImage.image`, else `stack.runImage.image`, which platform APIs before 0.12 write — kpack
  0.18's builds, at 0.9 (**amended 2026-10-19**, #2091) — (`another_run_image`; neither is a fact it
  cannot read, `502`): repository and tag compared with any digest stripped and
  a missing tag read as `latest`, as the lifecycle's rebaser compares them (buildpacks/lifecycle
  `internal/name.ParseMaybe`; **amended 2026-10-18**, #2089: this compared the repository alone, which
  pack then refused) — a new run image keeps its name, one by digest its repository; a run image whose `io.buildpacks.stack.id` is
  not the image's (`another_stack`); and a `run_image` that is no longer the one `GET /builder` answers
  (`run_image_moved`): the server planned onto another, and plans again. `message` names the label,
  run image or stack.
- **On Docker** the image is tagged with the fresh tag, then `pack rebase <fresh tag> --run-image <the
  run image> --pull-policy never` runs, the run image named as a build names it — `CANYON_IMAGE_RUN`'s
  reference, else `canyon-run:noble`, never its ID — the run image is the daemon's already, as `GET
  /builder` made it — and `image` is the new image ID, read as `/build` reads it. The facts are `docker
  image inspect`'s of each image. The rebased image's `run_image` must be the `run_image` the request
  named, else the rebase failed (`502`): a name that moved under pack is never answered as the plan's run
  image. Every failure after the fresh tag is written — pack's, the new image's ID or facts unread, that
  check — removes the fresh tag before it answers, whether or not the caller is still waiting.
- **On Kubernetes** it is kpack's own rebase: a Build `<app>-<nonce>` in `canyon-builds`, its nonce 16
  hex (64 bits, so no other Build takes its name; **amended 2026-10-19**, #2091), labelled
  `canyon.build`, whose `spec.lastBuild` names `image` and the stack id of its facts and whose one
  reason is `STACK` (the annotation `image.kpack.io/reason`), tagged as above; it lives as a `/build`'s
  Build lives — deleted once the rebase answers, swept when the executor starts — and `image` is its
  `status.latestImage`. **Amended 2026-10-19** (#2091):
  - Its `spec.runImage` pins the `run_image` the request named, so kpack never rebases onto what the
    ClusterBuilder names by then. kpack asks every Build for a `spec.source`, which its rebase never
    reads, so it names `image` too, and the sweep never deletes a rebase's source.
  - **The fresh tag is written before the Build is created** (**amended 2026-10-29**, #2576): kpack
    0.18's rebase opens `spec.tags[0]` and fails `could not access previous image` when the registry
    does not answer a HEAD of it (kpack `cmd/rebase`, imgutil's `Found`). It names a placeholder until
    kpack writes the rebased image over it: `image`'s manifest with the one annotation
    `canyon.rebase: <nonce>`, its config and layers `image`'s. The placeholder's digest is the rebase's
    alone, so the fresh tag is removed by it as below, and `image`'s own manifest is never tagged:
    `registry:2` removes a tag only with its digest, which would take `image` and its own tag with it. A
    fresh tag already there is never written over: the rebase fails (`502`) and leaves it. The
    placeholder is deleted by its digest once the rebase answers, whatever it answers, unless its Build
    was not seen gone (below); a crash after kpack's write leaves it untagged.
  - The answer is read from the registry, never from the Build's status alone: `status.stepsCompleted`
    must hold `rebase` and no step but `rebase` and `completion`, the container kpack 0.18 runs after
    every Build's steps and counts among them — kpack builds from `spec.source` a Build it does not take
    for a rebase, and its steps are a build's — and
    `status.latestImage` the fresh tag's image as the registry answers it; the facts of both images are
    read from the registry with the executor's credential, as an image scan reads an image (`/scan`,
    *`target: "image"`*), and the rebased image's `run_image` must be the `run_image` the request named.
    Otherwise the rebase failed (`502`), a rebased image the registry no longer holds included, never
    `404`, which is the request's image's.
  - Every failure once the fresh tag is written removes it, as Docker's does, a write or a create that
    fails included (**amended 2026-10-29**, #2576): the Build is
    deleted in the foreground — the deletion registered before the create, whose answer may be lost
    while the Build is made, and a Build never made nothing to delete — and the tag removed once it and
    its pod are seen gone, so a pod still running never pushes it after, and the caller's cancellation
    cuts none of it short. A deletion refused, or a Build not seen gone within two minutes, leaves the
    tag, and the executor's log says so, rather than race a pod that could push it again. The tag is removed by
    its digest, which a registry deletes with every tag of it, so only while no other tag names it and
    no Deployment of the app runs it: a rebase is deterministic, and a retry's image may be the one a
    deployed tag names. The sweep at start removes the fresh tag of each rebase Build a crash left,
    under the same rule, and only the one it created — the Build's one tag,
    `<CANYON_KUBE_REGISTRY>/canyon/<app>:<package>-<nonce>`, the app its `canyon.app` label names and
    the nonce its name `<app>-<nonce>` carries — never a tag of any other Build, nor a rebase's source.
  - The name `another_run_image` compares is kpack's copy's, `<spec.tag>-run-image` with its tag — by
    default `<registry>/canyon-builder:clusterbuilder-canyon-run-image` — which every app image the
    ClusterBuilder built records. So a ClusterStack's run image moved to another repository, a mirror
    included, keeps that name and is rebased onto; a ClusterBuilder renamed or given another
    `spec.tag` gives its run image another name, onto which no earlier image is rebased.
  - A pod still unscheduled at the deadline answers `502` with `the cluster had no room to run the
    rebase` and `no_room`, as `/build`'s.
  - A Build that failed answers `502` with `the <step> step exited <n>`, then `: ` and the last line
    that step wrote that is not blank, cut to 512 bytes (**amended 2026-10-29**, #2576), and the
    executor's log says the same. kpack's rebase writes its error there and nowhere else
    (`ERROR: could not access previous image: <image>`): its step writes no termination message, so the
    Build's condition holds none, and a rebase streams no log. The line is kpack's own words, image
    references among them; kpack names a registry credential by its Secret and host, never by value.
  - kpack 0.18 does rebase a one-shot Build so: it rebases a Build whose `spec.lastBuild` is set, whose
    reason is `STACK` and whose `lastBuild.stackId` is the builder image's stack id
    (`Build.rebasable`), so the pinned lifecycle image's `rebaser` as a Job is not taken and the
    executor's Role is unchanged. kpack's rebaser runs the lifecycle at platform API 0.9, which skips
    the lifecycle's run image name check (`stack.runImage.image` at 0.9); the executor's own
    `another_run_image`, before the Build is created, is that check. A ClusterBuilder with no
    `status.stack.id` is a fact the executor cannot read (`502`), never `another_stack`, which is
    permanent for the pair.
  - CI's kind runs no kpack controller, so kind proves the Build's shape, its fresh tag in the
    registry while it is there, and every refusal through the deployed executor, and the AWS test cluster the
    rebase itself (design/PATCHING.md §4.8).
- **Busy.** A rebase counts against `CANYON_EXECUTOR_BUILDS`, beside `/build`, and past it is `503
  {error:"busy"}` with `Retry-After`, as `/build` is (`/scan`, *Busy*).
- **It writes no environment.** A rebased image runs only through a `/run`, and production takes it
  only as a promotion re-`/run`s the digest that served preview (invariant 4).

## `GET /builder`

```jsonc
// response: 200 { "builder": "sha256:…",      // the identity of the builder the next /build builds with
//                 "run_image": "sha256:…",    // the run image it builds onto, and a /rebase rebases onto
//                 "waited_ms": 278000 }       // when this answer waited for Canyon's own images to be built first
// 502 when the executor cannot tell (its daemon or the cluster API did not answer)
```

Which builder a `/build` now would use (#1244). With `CANYON_BUILDER_NODE` set (**amended 2026-11-24**, #174), the
SHA-256 of the Node builder's image ID, a newline, and `canyon-builder`'s name, the tag its file's hash makes, as
`sha256:<64 hex>`. The Node builder is pulled first when the daemon has none. A new Node builder, or a change to
`executor/builder.Dockerfile`, a pin bump among them, is a new identity.
- **The full builder's part is its Dockerfile's hash, not its image ID.** So a `canyon-builder` built again from
  the same file, with newer packages from apt, is not a new identity, where without a Node builder it is.
- **Why:** the image ID exists only once the full builder is built, which means downloading it. Asking for it here
  would download the full builder at the first deploy's reuse check, when a Node app never needs it.
Otherwise, on Docker the builder image's ID (`docker image inspect
--format {{.Id}}` of `CANYON_BUILDER`, pulled first when the daemon has none, as a build would pull it,
else of `canyon-builder`, built first when the daemon has none, as a build would build it); on Kubernetes the ClusterBuilder `canyon`'s
`status.latestImage`, a reference by digest, and `502` while it has none. A builder patched or replaced —
a new Paketo release pulled through `CANYON_BUILDER`, kpack's rebuilt ClusterBuilder — is a new identity. The server asks before
every `/build` and before it reuses an image, and the build digest covers the answer (routes.md, the
deploys row): an image built under another builder is never reused. It changes nothing but, on Docker,
which image the tag `canyon-run:noble` names (below). An executor from before
this call answers `404`: the server then reuses nothing and records no digest, so it builds as it always
did; a host deploys the executor before the server.

**`run_image`** (**amended 2026-10-18**, #2086; design/PATCHING.md §4.1) is the run image the next
`/build` builds onto and a `/rebase` now rebases onto: on Docker the image ID of `CANYON_IMAGE_RUN`,
pulled first when the daemon has none, else of `canyon-run`, built first when the daemon has none, as
a build would; on Kubernetes the ClusterBuilder `canyon`'s `status.stack.runImage`, kpack's copy of the
ClusterStack's run image, answered as `<repository>@sha256:<64 hex>` with its tag stripped (**amended
2026-10-19**, #2091: `/rebase`), and `502` while it names none by digest — the Role's `get`
on the ClusterBuilder reads it, and nothing more is granted. It is for the server's plan alone and never
part of the build digest: an image built onto an older run image is reused as before, and a rebase
moves it. An executor from before it answers `builder` alone, and the server plans no rebase there.

**`waited_ms`** (**added 2026-11-08**, #2711) is how long this answer waited for Canyon's own images,
`canyon-builder` and `canyon-run`, to be built first, by this call or by one it waited behind: on Docker,
the first build on a host, and again when a Canyon release changes either image, `canyon-builder` up to a
1.4 GB download. It runs from the call's start to the moment the later of the two was built during it.
Absent when neither was built while it waited: both on the daemon already, `CANYON_BUILDER` and
`CANYON_IMAGE_RUN` (pulled, not built), Kubernetes, and an executor from before it. With `CANYON_BUILDER_NODE`
set it counts the Node builder's pull in place of `canyon-builder`'s build, which only a tree that needs it starts,
inside its `/build`, whose log says what it downloads. The server turns it
into the run's `build.image` row (events.md), which the CLI adds to the build's time.

## `GET /scandb`

```jsonc
// response: 200 { "databases": [ { "tool": "trivy", "built_at": "2026-11-05T01:00:58Z", "stale_at": "2026-11-10T01:00:58Z" },
//                                { "tool": "grype" } ] }   // one the executor has not read, or found none
// 502 when the executor cannot tell
```

**How old each vulnerability database is** (added 2026-11-09, #2743; design/ARCHITECTURE.md, *Scanner data*):
trivy's, grype's and trivy's Java database (`trivy-java`), in tool order. `built_at` is when the database was
built, read from the database itself, never from when a refresh ended; `stale_at` is `built_at` plus 5 days, when
a scan on it answers `db_stale` (*Vulnerability databases*), so the server compares no clocks of its own. Both are
absent while there is none. On Docker, and for a claim on Kubernetes, the answer is the age as the executor last
read it, after each refresh, failed or not, and at each scan's check, so the call never waits on a refresh's lock
and starts no container; a read that fails keeps the one before. On Kubernetes trivy's is its server's `GET
/version` now, absent while the server has no ready pod. The call reads only: it refreshes nothing. An executor
from before it answers `404`, and the server says nothing of the databases' age.

## `GET /running?app=&target=`

```jsonc
// app, target as /run
// response: 200 { "image": "canyon/claims-portal@sha256:…" } — the image <app>-<target> was started from, while it runs
// 400 on an app or target the naming pattern refuses; 404 when there is no such container, or it is not running;
// 502 when the executor cannot tell (its daemon or the cluster API did not answer) — never 404, which a caller reads as nothing running
```

Read-only: a caller that recorded nothing after a `/run` (a crash between the two) asks what runs
before it starts the same image again. The promote run is that caller: only a retry of its prod
step asks, and records a start this image already made instead of starting it twice; a first
attempt always starts the container, so prod's current values reach it.

## `POST /running`

```jsonc
// request: the environments one read shows, 1 to 400 of them, app and target as /run; why: true asks
// why those that have exited stopped (below), and only the status read asks it
{ "containers": [ { "app": "claims-portal", "target": "preview" }, { "app": "claims-archive", "target": "prod" } ], "why": true }
// response: 200 — those asked whose app runs, each once, in the order asked; those asked whose
// container has been started again and is up or waiting to start (below); and those asked
// whose container is there and has exited, with its last exit code and its last line, raw, when why was
// asked, and [] when it was not (below)
{ "running": [ { "app": "claims-portal", "target": "preview" } ],
  "restarting": [ { "app": "case-dies-later", "target": "preview", "restarts": 7 } ],
  "stopped": [ { "app": "claims-archive", "target": "prod", "exit_code": 1, "said": "Error: connect ECONNREFUSED 10.0.4.7:5432" } ] }
// 400 on an app or target the naming pattern refuses, on none or more than 400;
// 502 when the executor cannot tell — never a 200 naming fewer, which a caller reads as stopped
```

Whether the app runs, for a read that shows many environments at once — the status read and the
fleet (routes.md, `envs.<env>.state`): one call for the read, never one per app — and for one app's
environment, a patch's watch (routes.md, *Patch runs*, step 8; amended 2026-10-29, #2100), which asks of
its one container every 30 s for whether it exited or restarted since its `/run`. Read-only, and as
cheap as one: Docker lists, `docker container ls --all` filtered to every `<app>-<target>` asked, each
name matched whole, and inspects the running ones once together for their restart counts (*Restarting*),
and answers those running from an image by digest, as
`GET /running` answers 200 for them — a name the daemon does not list is not running, and a daemon that
does not answer is `502`, never an error's words read as a container gone (#1244). Kubernetes lists the Deployments and the pods of the apps
asked once each (label selector `canyon.app in (…)`), and answers a Deployment with any available
pod, whatever its template: during a redeploy or a promotion the previous pod serves until the new
one is available, and the app runs all along, unless the new pod has restarted (*Restarting*)
(`GET /running` keeps its template rule, which the promote retry needs).

**Restarting** (**amended 2026-10-05**, #1722). Each asked whose container has been started again and
is up or waiting to start is in `restarting` and nowhere else — **never in `running`, even while it is
up**: an app that keeps exiting is not running in any sense a promotion may read, and an answer read in
one of its up-windows must agree with one read between two restarts (the test cluster's `case-dies-later` served
40 s in every 5½ min and read as running in those 40 s). `restarts` is how many times the container or
pod now counted was started again, at least 1, read from the runtime's own count, never timed by the
executor. A `/run` that succeeds starts that count at 0 (*An app that exits is started again*, *On
Kubernetes*), so it counts the restarts since the deploy. On Docker it is a container the daemon lists
as `restarting`, or as `running` with a `RestartCount` above 0, one `docker container inspect` of the
running ones; past its ten restarts, or after an exit 0, the container stays exited and is `stopped`,
as before. On Kubernetes it is a Deployment whose newest pod of its current template has an `app`
container with `restartCount` above 0, ready or not, read from the one list of pods above, so no call is
added; the kubelet restarts it without bound, an exit 0 included, and a pod the cluster replaces
(evicted, drained) starts its count again. **One restart is enough**: an app started again once,
however long ago, reads `restarting` until its next `/run`, which clears it — a promotion is never made
from an app that has exited since it was deployed. An executor older than this answers no
`restarting`, which the server reads as none.

**Why it stopped** (#1597). Asked `why`, `stopped` names each asked whose container is there and has
exited, each once, in the order asked, with `exit_code`, its last exit code, and `said`, its last line of output —
`""` when it wrote none. Only a container that does not run carries one, so a read of running apps
costs nothing more; not asked `why` — the fleet and every read but the status read — `stopped` is
`[]` and nothing more is asked of the daemon or the cluster. **A container that is `restarting` (above) is
in `stopped` too when its last exit can be told** (**amended 2026-10-10**, #1597: the test cluster's `case-dies-later`
restarts for ever on Kubernetes, so it never reads stopped and its reason was the CLI's alone): on
Kubernetes always, from the same pod's `lastState`; on Docker only while the daemon lists it as
`restarting`, down and waiting out the delay — once it is up again its last line is the new start's and
its exit code is gone, so it is left out. A server reads such an entry as the restarting one's last
exit, never as stopped (routes.md, `envs.<env>.restarting`); one older than this ignores it. On Docker it
is a container the daemon lists as `exited`, `dead` or `restarting`:
`said` the last line of `docker logs --tail 1` of each, stdout and stderr, then `exit_code` its
`State.ExitCode`, one `docker container inspect` for all of them that keeps only those still `exited`,
`dead` or `restarting` — one started again between the reads is left out. On Kubernetes it is the newest
pod (a restarting one's: of its current template, whose restarts are counted) of
the Deployment's `app` container whose current state or `lastState` is `terminated`, read from the one
list of pods above: `exit_code` is that state's `exitCode` and `said` the last non-empty line of its
termination message (#1549), so no call is added. `said` is at most 4 KiB: a longer line is dropped,
`said` `""`, never cut, since a cut line can hold the start of a value the server would not recognise. A container whose exit or line cannot be read is left out, the reason logged, and the
answer stands: it is still not running. **`said` is the app's own words, unmasked**: the executor
holds no app's values between calls (invariant 2), so it answers the line as the app wrote it, to
the server alone, which masks it before anything stores or shows it — each runtime value of that
app and environment, the rows `/run` hands on, by `/run`'s rule above (`<NAME>`, the longest first,
a URL's userinfo password as `<NAME password>`; `PORT` and `CANYON_IDENTITY_*` never), then cut to
its first 200 characters (routes.md, `envs.<env>.stopped`). It never masks with values other than
the ones the app was handed: when one of that environment's secrets was set or removed after the
start of the run that started what runs there, the server drops the line and shows the exit code alone.

**Rollout.** The server calls `POST /running`, so a host deploys the executor before the server. A
server that meets an older executor's `404` reads every environment as `unknown`, never `running`;
an answer without `stopped` says no environment why.

## `GET /logs?app=&target=&tail=&follow=`

```
// app, target as /run — preview | prod, which container's log this is; tail — backlog lines
// before the stream follows, an integer 0..10000, omitted = 200; follow — true | false, omitted = true
// response: 200 text/plain; charset=utf-8, chunked — the container's stdout+stderr, interleaved
// exactly as docker wrote them: the last `tail` lines, then live, until the client ends the stream;
// under follow=false the last `tail` lines and the end of the body (amended 2026-10-07)
// 400 on an app or target the naming pattern refuses, a tail outside 0..10000, or a follow other than true or false; 404 when there is no container <app>-<target>
// 502 when the executor cannot tell (its daemon or the cluster API did not answer) — never 404, which a caller reads as nothing there
// after the 200, a read that fails ends the stream with one line of the executor's own:
// canyon: this log could not be read: <reason>
```

The container named is `<app>-<target>`, exactly as `/run` names it above; `GET /logs` answers
that container's log and no other's, and it neither receives nor returns anything from the
vault — a stream of bytes the container already wrote, nothing more but that one line. The `200`
leaves before the log is opened, because on Kubernetes the API answers a follow request only with
the app's first byte: a quiet app still answers at once, and a read that fails — the log not yet
there, or cut off — ends on its reason instead of silently. Both executors write the line; on Docker,
`docker logs` also writes its own error into the stream before it (**amended 2026-09-30**,
`canyonv2-qlsd`).

**A container that isn't running** (**amended 2026-11-24**, #184: a crash's lines are what a person
needs, and they are the run that ended). On Docker an exited container is read as a running one is:
`docker logs` answers every run since `/run` made it, its restarts' included, and a `follow` ends with
the body. On Kubernetes, when the newest pod's `app` container isn't running, the executor reads its
last run: that run's log (`previous`) while the container waits to start again, its own log once it has
ended for good, and `follow` as `false`, since nothing runs to follow. Where the kubelet no longer holds
that log, the executor writes the run's termination message, the tail of its log the pod's status keeps
(`FallbackToLogsOnError`, *On Kubernetes*): its last `tail` lines, from a whole line, as `/run` reads
it (a first line at the kubelet's 2 KiB bound begins cut and is left out), then one line of its own:

```
canyon: the cluster kept only the end of the app's last run
```

The kubelet no longer holds it when the read is refused, or when it answers `200` with exactly its own
sentence `unable to retrieve container logs for <containerID>`, naming that run's container, as it can
once the dead container is gone from the node, before its status sees it gone (amended 2026-11-24, #185's
review: kubelet v1.33.0, `kuberuntime_container.go`; it refuses after that, as kind's v1.34.11 did); that
sentence is never passed on as the app's. A container that is still running can have its log file missing
on the node: the kubelet then answers `200` with the same sentence naming that container, and the executor
passes it on as written, since no run has ended and there is no kept end to fall back to. It is the
kubelet's line, not the app's. A run the kubelet lost track
of (`reason` `ContainerStatusUnknown`) carries the kubelet's message, not the app's, and a run whose
status kept nothing has none: for either the executor writes only

```
canyon: the cluster kept nothing of the app's last run
```

A container that isn't running and never ended — waiting for its image, say — is read as today: its log
is empty, or the read fails on its reason.

## `POST /scan`

```jsonc
// request: multipart like /build — part "json" first, then part "tree" (a tar of the package),
// then, for opengrep only, part "rules" (a tar of rule files, at most 50 MiB)
{ "app": "claims-portal",
  "package": "pkg_7d3e8f",
  "tool": "trivy",                                   // trivy | opengrep | eslint | grype | checkov
  "settings": { "scanners": ["vuln", "secret"], "skip_paths": ["docs"] } }   // this tool's allow-list only
// response 200 — exactly one of:
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "db_built_at": "2026-09-24T06:12:40Z", "sarif": { … } }
{ "tool": "opengrep", "image": "…", "sarif": { … }, "unloaded": ["org-bad-regex"], "unparsed": ["src/app/api/claims/route.ts"] }
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "timeout" }
{ "tool": "trivy", "not_finished": "no_image" }     // no image ran, so none is named
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "no_room", "reason": "0/4 nodes are available: 4 Insufficient cpu." }

// target "image" (PATCHING.md §3.1): the json part alone
{ "tool": "trivy", "target": "image", "app": "claims-portal",
  "image": "canyon/claims-portal@sha256:…" }         // as /build answered it; on Kubernetes <registry>/canyon/<app>@sha256:…
// response 200 — exactly one of:
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "sbom": { "bomFormat": "CycloneDX", … },
  "diff_ids": ["sha256:…", "sha256:…"],                                // the image's layers, bottom first
  "lifecycle_metadata": "{\"buildpacks\":[…],\"runImage\":{…},…}",   // absent when the image has no such label
  "referrer": "sha256:…" }                    // on Kubernetes, an app's image: the OCI referrer that publishes the SBOM beside it; absent when none was
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "db_missing", "reason": "java" }
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "over_cap", "reason": "image" }
// the run image (PATCHING.md §0.4): no app, and image exactly GET /builder's run_image
{ "tool": "trivy", "target": "image", "image": "sha256:…" }   // on Kubernetes <repository>@sha256:…
// response 200 as an app image's, with no lifecycle_metadata when the run image carries no such label;
// 409 { "error": "run_image_moved", "message": "the run image is now …, not …: plan again" }

// target "sbom" (PATCHING.md §3.2): the json part, then part "sbom", one CycloneDX JSON document
{ "tool": "trivy", "target": "sbom" }
// response 200 — exactly one of:
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "db_built_at": "2026-09-24T06:12:40Z",
  "report": { "SchemaVersion": 2, "ArtifactType": "cyclonedx", "Results": [ … ] } }   // trivy's JSON report
// each vulnerability: { "VulnerabilityID": "CVE-…", "PkgIdentifier": { "PURL": "…", "BOMRef": "…" }, "Layer": { "DiffID": "sha256:…" }, … }
// each result's Packages: every package trivy read, { "Identifier": { "PURL": "…", "BOMRef": "…" }, … }
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "not_sarif", "reason": "unread" }   // a component trivy did not read
// beside a report: "unscanned": ["pkg:conda/…"]   // the bom-refs of components trivy lists but never matches
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "db_stale" }

// target "packages" (finding.md, A custom rule checked by code): the json part, then part "tree", as a tree scan's
{ "tool": "trivy", "target": "packages", "app": "claims-portal", "package": "pkg_7d3e8f" }
// response 200 — exactly one of:
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…",
  "packages": { "SchemaVersion": 2, "ArtifactType": "filesystem", "Results": [ … ] } }   // trivy's JSON report, no vulnerabilities
// each result: { "Target": "package-lock.json", "Class": "lang-pkgs", "Type": "npm", "Packages": [
//   { "Name": "express", "Version": "4.21.2", "Identifier": { "PURL": "pkg:npm/express@4.21.2" }, "Relationship": "direct", … } ] }
{ "tool": "trivy", "image": "ghcr.io/aquasecurity/trivy@sha256:…", "not_finished": "timeout" }
```

**Runs one of the five tools Canyon runs through its executor on one package** and answers the tool's SARIF,
which the control plane attaches as a report (finding.md, *Canyon-run tools*).

- **The image is the executor's.** Each tool has one image pinned by digest per Canyon release,
  compiled into the executor — opengrep's on Kubernetes the operator's build of
  `executor/opengrep.Dockerfile` (below) — which the tool's setting may point at another registry's copy
  (*Images and database sources*); the request never names the tool's image, and `image` in the answer
  is the one that ran. A tool this executor has no image for answers `not_finished: "no_image"`.
  Opengrep publishes no image, so its image is built by the executor from inputs pinned the same
  way — a base image by digest and the release binary by sha256, checked before it is used — and
  `image` names the built image by its content id (`canyon-scan/opengrep@sha256:<id>`); a build
  that fails answers `no_image`. Its settings reach it as flags, and `skip_paths` are removed
  from the copied tree, not passed; it reads no `/scan/opengrep.yaml`. ESLint publishes no image
  either: its image is built the same way from `executor/eslint.Dockerfile` — a Node base image by
  digest, ESLint 9.39.5, eslint-plugin-security 4.2.0, `@typescript-eslint/parser` 8.71.0,
  TypeScript 5.9.3 and `@microsoft/eslint-formatter-sarif` 3.1.0 by exact version, every package they
  pull in resolved as the registry stood at 2026-10-02 (`npm install --before`), install scripts off —
  and named `canyon-scan/eslint@sha256:<id>`. Its rules are fixed in the image, four and no other:
  `security/detect-child-process`, `security/detect-non-literal-fs-filename`, `no-eval` and
  `no-new-func`; it runs with `--no-config-lookup` on the image's own config and
  `--no-inline-config`, so nothing in the tree turns a rule off or another on, and with
  `--no-error-on-unmatched-pattern`, so a package with no JavaScript or TypeScript is a scan with no
  result, never a failed one. Its `skip_paths` are
  removed from the copied tree; each uri it writes, `file:///scan/tree/<path>`, comes back as the
  package's path.
- **Settings are an allow-list per tool**, the same shape as the tool's policy rule
  (routes.md, the policies row): trivy `{scanners?, skip_paths?}`, `scanners` a subset of
  `vuln | misconfig | secret`, unset `vuln` and `misconfig` — never `secret`, since gitleaks is the
  one secrets detector (finding.md, *Canyon-run tools*); checkov `{frameworks?, skip_checks?, skip_paths?}`; opengrep,
  eslint and grype `{skip_paths?}`, a path of the package. Opengrep's rules repo is the server's to fetch:
  the executor gets rule files only as the `rules` tar. Any other key, a `rules` part for another
  tool, a part over its cap, or a tar entry other than a regular file or a directory (a symlink,
  a hard link, a device, a fifo) is `400 {error:"invalid_request", message}`.
- **How it runs.** `docker create --network none --read-only --user 65534:65534 --cap-drop ALL
  --security-opt no-new-privileges --memory 2g --pids-limit 512 --cpus 2 --tmpfs /tmp:mode=1777 --label
  canyon.scan`, with an anonymous volume at `/scan` (docker cp cannot write a read-only root fs)
  and, for trivy and grype, the tool's database volume (below) mounted read-only at `/scan/db`. The
  tmpfs is mode 1777 because a tmpfs otherwise copies the image's `/tmp` mode (0755 root in
  grype's), and grype, unable to write there, silently skips the archives it unpacks (a jar). `docker cp` streams
  the tree into `/scan/tree`, the rules into `/scan/rules`, and the config the executor writes — the settings, every
  inline suppression off — into `/scan/<tool>.yaml`, which it passes to the tool by explicit path
  (invariant 9); then `docker start -a`, its stdout streamed into what the executor keeps (below);
  then `docker rm -f -v`. The tool's exit status is not read — but for Opengrep's, below — its stdout is.
  Opengrep runs under `/bin/sh -c 'exec 2>/dev/null; "$@"; c=$?; printf '\0'; cat /tmp/opengrep.json; exit $c' sh`, its arguments
  carrying `--json-output=/tmp/opengrep.json`: its stdout is its SARIF, a NUL, then its JSON output —
  whose `errors[].path` are the answer's `unparsed` — and its exit status its own; no NUL, nothing
  after it, or JSON without `errors` is `not_sarif`. A result the
  tool reports suppressed anyway comes back with its `suppressions` removed, so the attach drops
  none for that reason. A tool that writes result uris relative to its working directory
  (checkov: `scan/tree/main.tf`) comes back with that exact leading `scan/tree/` (or `/scan/tree/`)
  removed, and a uri grype writes from the tree's root after a `/` (`/package-lock.json`) comes back
  with that `/` removed, so each uri is the package path the attach looks up — a result's, and a
  notification's in `invocations` (ESLint's `file:///scan/tree/` uri of a file it could not parse);
  a uri without its tool's prefix is left as the tool wrote it. **What the executor keeps** of a
  stdout, as it streams: each result's `region.snippet` and `contextRegion` dropped — the attach builds
  every snippet from the package's own tree (finding.md) — and, of Opengrep's JSON output, its `errors`
  alone. At most 10 MiB is kept, and at most 10 MiB held unread at once, so one result longer than that
  is `over_cap` too; the stdout itself is bounded by the deadline alone. Only one whole SARIF document
  is kept — bytes after it, or a JSON output cut short or followed by more, are `not_sarif` — and a read
  that fails is the executor's own error (**amended
  2026-10-05**, #1684: every stock `dotnet new webapp`'s Opengrep stdout was 11.8 MB, a 91 KB minified
  jQuery line quoted in each result's snippet and again in the JSON output; 1.5 MB kept). Nothing else
  in the SARIF is changed. The whole scan has a deadline per tool, fixed per release beside the
  digest and at most 10 minutes; at it the container is removed.
- **`not_finished`** is the one answer without SARIF: `timeout` (the deadline), `killed` (the
  container died: out of memory or a signal, which is checked first), `not_sarif` (stdout is not one run of SARIF 2.1.0
  JSON; for an image or sbom target, not the one document that target answers, below), `over_cap` (what the executor keeps past its 10 MiB, still a blocking could-not-check; or an image over its cap, below), `no_room` (on
  Kubernetes, the scan's pod still unscheduled when its Job's deadline ends it: its `PodScheduled`
  condition `False` with reason `Unschedulable` as last seen, never a pod placed since; the answer carries `reason`, that condition's own message as the
  scheduler wrote it, quoted and never parsed — amended 2026-10-05, #1637: such a scan answered `502`
  and read as Canyon's own failure), `db_missing` / `db_stale` (below), `no_image`
  (above), and `rules_error`: Opengrep, not `killed`, exited with a status other than 0 or 1 (128+n
  is `killed`, and so is an engine the out-of-memory killer ended though Opengrep itself exits 2), its own word that it could not load every rules file it was handed — one file that is not a rules file drops the whole
  config, the vendored rules with it, and the SARIF it still writes holds no result — so its SARIF
  is not the answer. One exit is a scan all the same: status 2, the container not `killed`, whose SARIF's `error`
  notifications are one or more, each a `Rule parse error` — a rule Opengrep dropped (an invalid regex) while it ran the rest —
  answers that SARIF, and the attach names each dropped rule (finding.md, *In a run*). `db_built_at` is on trivy's and grype's SARIF answer, and on no other: when the
  database the scan read was built. `unloaded` is on opengrep's SARIF answer, and on no other, when
  not empty: the `rule_id` of each entry of the `errors` its JSON output lists whose `type` is `Rule
  parse error` (`""` for one without), distinct, in name order. `unparsed` is on opengrep's SARIF answer, and on no other, when
  not empty: the `path` of each entry of the `errors` its JSON output (`--json-output`, written in the
  same run as the SARIF) lists that carries one — a file it read none of or only part of, which its
  SARIF names only in a message quoting the file — distinct, in path order. The control plane names
  those of them the package holds (finding.md, *In a run*).
- **Vulnerability databases** (trivy, grype, and trivy's Java database, `canyon-scandb-trivy-java`, below) are the executor's, downloaded on first need: a
  refresh runs that tool's image with network, downloading from the tool's source (*Images and
  database sources*), into the volume `canyon-scandb-<tool>`, and a
  tool no scan has asked for has no volume and is never downloaded. Each scan mounts that volume read-only at `/scan/db`, the tool's cache
  directory: trivy 0.74.0 keeps its scan cache in memory and grype 0.119.0 reads its SQLite
  database without writing, so neither writes there, and the executor's e2e test checks the volume
  unchanged by a scan. A write to `/scan/db` fails: trivy made to keep its scan cache on disk exits
  without SARIF, `not_sarif`. Grype makes its database's directory readable by its own user alone, so
  its refresh runs as the scan's user (65534), who is first made the owner of the volume's root. Grype downloads
  into its cache directory, and a refresh cut short leaves that download, `grype-db-download<N>`, in the volume:
  on Docker each grype refresh first removes `/db/grype-db-download*`, from the executor's busybox
  (`CANYON_IMAGE_BUSYBOX`) as that user with no network, and never the database (amended 2026-10-27, #2504), and
  on Kubernetes its refresh Job does the same (*On Kubernetes*, #2527); a sweep that fails stops the refresh, the
  database as it was. A refresh and a scan of the same tool never overlap — one
  lock per tool covers both. **The first scan waits for the download**: a scan that finds no
  database starts the tool's refresh, one at a time — a scan while it runs starts no other — and
  answers busy (below), and a scan once the database exists runs on it; the caller asks for as
  long as it answers so (finding.md, *Canyon-run tools*). When the executor starts, and every 6
  hours, it refreshes each tool whose volume exists, and no other (on Kubernetes trivy has none: its server refreshes its own, *On Kubernetes*). `db_missing` is the refresh
  failing: a scan that finds no database after the refresh a scan started failed answers it, once,
  and the scan after that starts a refresh again. A database built more than 5 days before the
  scan starts is `db_stale`. Neither runs the tool.
- **Busy.** `/build` and `/scan` each have their own limit on work in flight, `CANYON_EXECUTOR_BUILDS`
  and `CANYON_EXECUTOR_SCANS` (defaults: 6 and 14 on Kubernetes, 2 and 5 on Docker, so the defaults keep
  this rule: 4 builds and 2 rebases, 10 scans, 2 patch scans, the deploys' image scans' one and the
  rescan's one; on Docker 1 and 1, and 2, 1, 1 and 1; amended 2026-10-18, #2086, and raised with
  PATCHING.md's `patches` queue, #2096), set at or above the server's queues (`CANYON_BUILD_CONCURRENCY`
  plus `CANYON_PATCH_CONCURRENCY`, since patch runs' rebases share the build limit; for `/scan`, the sum
  of `CANYON_SCAN_CONCURRENCY`, the image scans and re-matches the `patches` queue's patch runs
  (`CANYON_PATCH_CONCURRENCY`), the `image-sbom` queue's image scans after a deploy (one) and the
  rescan queue (one) send; finding.md, *Canyon-run tools*, and routes.md, the deploys row), so the server
  never sends more than the executor runs and
  busy means real overload — another server, or limits set below the queues'. Past its limit either
  is `503 {error:"busy"}` with `Retry-After` in seconds, and the caller waits and asks again for as
  long as the executor answers so, never failing the run for it (amended 2026-10-04, #1652: a run asked
  180 times, then failed). A scan of
  a tool whose database a refresh is writing, or has yet to write, is the same answer with
  `reason:"database"` beside the error, `503 {error:"busy", reason:"database"}`, so the run can say
  what it waits on (events.md, `scan.waiting`); a busy answer for the limit carries no `reason`, and nor
  does a second grype scan on Kubernetes while one runs (*On Kubernetes*), or a second image scan on
  either host (*`target: "image"`*). A scan both would turn
  away while a refresh waits or writes is answered with `reason:"database"`.
- **`target`** is `tree` — unset is `tree`, everything above — `image`, `sbom` or `packages` (PATCHING.md §3.1,
  §3.2; `packages` below), and only trivy takes `image`, `sbom` or `packages`. An image or sbom target takes no `package` and no
  `settings`, and an image target names `image`, and `app` unless the image is the run image (below), an sbom target neither; any other key, another
  tool, or a part the target does not take is `400 {error:"invalid_request", message}`. Each runs as a
  tree scan runs — the container above, no network, a read-only root fs, user 65534 — and counts
  against `CANYON_EXECUTOR_SCANS` (*Busy*), with these arguments, trivy 0.74.0's own: it takes no
  `--skip-check-update` on `image` or `sbom`, so it is not passed, and `--list-all-pkgs` is for its JSON
  format alone, so only the sbom target passes it (the image target's CycloneDX lists every package
  anyway); `--ignorefile /dev/null` is passed, as on a tree scan. On these targets `over_cap` always
  carries `reason`: `image` when the image is past a bound below, `report` when what trivy wrote is past
  the 10 MiB kept.
- **`target: "image"`** reads the app's image — the only time Canyon reads an app's image — or the run
  image (below), and answers its CycloneDX SBOM. `image` is the app's image by digest as `/build` answered it and `/run` takes it, never a tag;
  an image this host does not hold is `404`, as `/run`'s. Image scans run one at a time on each host —
  a second while one runs answers busy with no `reason` — each holding its Java database (below). The
  image reaches trivy readable by user 65534, and the two executors hand it over differently
  (**amended 2026-10-30**, #2585). **On Docker** the image goes to `/scan/image`, an OCI image layout
  directory. The executor runs `docker save` into its own private directory, outside what it copies in,
  checks the tarball against the cap below, and unpacks only `oci-layout`, `index.json` and
  `blobs/sha256/<64 hex>` (regular files, each once) into `/scan/image`. It drops `docker save`'s legacy
  `manifest.json` and `repositories`, and the two directory entries `blobs/` and `blobs/sha256/`. It
  refuses anything else: another name, a link or a device, a second entry with one name, or an archive
  without `oci-layout` or `index.json`. A refused archive answers `not_sarif`, never an SBOM, and nothing
  else of the image is read. Trivy reads the layout by `index.json`, which names each layer's media type.
  Docker 29's containerd image store saves an image whose layers mix uncompressed and gzip blobs. Trivy
  reads the legacy `manifest.json` as if every layer had the first one's compression, so it fails on such
  an image ("archive/tar: invalid tar header") and answers no SBOM. **On Kubernetes** it goes to
  `/scan/image.tar`, the tarball the executor writes. The executor reads it from `CANYON_KUBE_REGISTRY` with its own credential, once its lock
  is held and its deadline runs, and streams it into the feed, the pod's `/scan` sized, and its
  ephemeral storage requested, at twice its tarball's size from the manifest — the image, and trivy's
  copies of the files it reads (jars, manifests) under `TMPDIR` — plus 64 MiB; a pod past it is evicted,
  and the scan answers no SBOM. **Its bounds**, each `over_cap` with
  `reason: "image"` and no container or Job made: larger than `CANYON_SCAN_IMAGE_MAX` — on Docker the
  uncompressed size `docker image inspect` answers, checked again on the saved tarball before it is unpacked; on
  Kubernetes the compressed bytes its manifest names, so one cap reads larger images there — a
  manifest or config whose descriptor names no size above zero or one of more than 4 MiB (each is read
  only once its descriptor says it is within that; on Kubernetes a layer of no known size is past the
  cap too, and a manifest HEAD without `Content-Length` fails the call before the body is asked for;
  the manifest's body is read to 4 MiB and one byte more, whatever its HEAD named, and one not the size
  its HEAD named fails the call), more than 128 layers, or an `io.buildpacks.lifecycle.metadata` label of more than 1 MiB. Trivy runs
  `image --input <in> --format cyclonedx --cache-dir /scan/db --cache-backend memory
  --ignorefile /dev/null --skip-db-update --skip-java-db-update --offline-scan --disable-telemetry
  --skip-version-check --no-progress --skip-files /layers/sbom/**/sbom.cdx.json --skip-files
  /layers/sbom/**/sbom.spdx.json --skip-files /layers/sbom/**/sbom.syft.json --skip-files
  /layers/sbom/**/sbom.legacy.json`, `<in>` being `/scan/image` on Docker and `/scan/image.tar` on
  Kubernetes, with `TMPDIR=/scan/tmp`, the scan's disk rather than its memory:
  `--format cyclonedx` turns its vulnerability scanning off, so it lists and never matches, and the memory
  backend keeps its layer cache off the read-only `/scan/db`. Those four are the names of the SBOM
  documents the lifecycle writes in its SBOM layer, `/layers/sbom`: trivy would list every package they
  name — the app's dependencies and the launcher's modules, each listed again in its own layer — under
  that layer (PATCHING.md §2, #2216), so no package a document there names alone is in the SBOM. Every
  other file there is read, as anywhere in the image: an app's own buildpack can write one. Each component carries trivy's
  `aquasecurity:trivy:LayerDiffID`, and the image's own component its diff IDs — sorted, as trivy
  0.74.0 sorts every property list, so their order is the answer's `diff_ids`: the image config's
  `rootfs.diff_ids`, bottom layer first (on Docker `docker image inspect`'s `RootFS.Layers`). Its stdout
  is kept as a SARIF one is, and is the answer's `sbom` when it is one CycloneDX JSON document
  (`bomFormat: "CycloneDX"`); else `not_sarif`. `lifecycle_metadata` is the image config's
  `io.buildpacks.lifecycle.metadata` label as the image carries it, absent when it has none: whose each
  layer is. The image's environment and files are read for packages alone: no value of its `ENV` is in
  the SBOM.
- **On Kubernetes the SBOM is also published** (**added 2026-11-02**, #2077; design/PATCHING.md §3.1), for
  the org's own tools: once an app's image — never the run image — answers its SBOM, the executor pushes it
  with its own credential to the image's own repository in `CANYON_KUBE_REGISTRY` as an OCI 1.1 artifact
  whose `subject` is the image's manifest: `artifactType` and its one layer `application/vnd.cyclonedx+json`,
  the layer the answer's `sbom` byte for byte, its config OCI's empty descriptor
  (`application/vnd.oci.empty.v1+json`). The registry then lists it among the image's referrers (OCI 1.1's
  Referrers API), where a tool finds it by the image's digest. `referrer` is its digest, answered only once
  the registry lists it there. An image keeps one: when the registry lists a referrer of it already — a scan of
  the same image again, whose document differs by trivy's serial number and timestamp alone — that one is
  answered and none is pushed. A registry without the Referrers API gets none: never the referrers tag
  schema's `sha256-<hex>` tag, which the tag cleanup would take for an image, and the manifest pushed is
  removed again. A push that fails is logged and the answer is the SBOM all the same: the referrer is never
  the scan's failure, and the server reads nothing of it. When the tag cleanup removes an image (*What
  `keep` spares*), it removes every referrer of it first: none outlives what it describes. Docker publishes
  none.
- **The run image** (**added 2026-10-19**, #2375; design/PATCHING.md §0.4) is read by the same target
  with no `app`: `image` is exactly the `run_image` `GET /builder` answers now — on Docker the image ID
  (`sha256:<64 hex>`), on Kubernetes kpack's copy (`<repository>@sha256:<64 hex>`), read with the
  executor's own credential as an app's image is, and so only from `CANYON_KUBE_REGISTRY`: a run image the
  ClusterBuilder names in another registry is never read, and the scan fails (`502`) before any request
  reaches it. Any other image, an older run image
  this host still holds included, is `409 {error:"run_image_moved", message}`, as `/rebase`'s refusal is:
  the server's plan named another, and it plans again. It is bounded, saved, scanned and answered as an
  app's image is, one at a time with them, its `lifecycle_metadata` absent when it carries no such
  label. The server reads it, and matches its layers, at each rescan tick while an image runs in a
  project whose `platform.known-vuln` is on, so its plan can tell a run image that carries a fix from one
  that does not: it alone reads the answer, and no finding is made of it.
- **The Java database is read by the image target alone.** Trivy's jar analyser identifies each jar
  from it — by the jar's SHA-1, or by its file name's artifact id when it carries no `pom.properties` —
  and without it `trivy image` of an image holding any jar ends without an SBOM (0.74.0: `Unable to
  initialize the Java DB`); a re-match never reads it. It is a database of its own, as trivy's and
  grype's are (*Vulnerability databases*): the volume `canyon-scandb-trivy-java`, downloaded from
  `CANYON_SCANDB_TRIVY_JAVA` on the first image scan by `image --cache-dir /db
  --download-java-db-only`, its download (near 1 GB; the database is 1.4 GB) staged in the volume's
  `tmp/` (`TMPDIR=/db/tmp`), which each refresh empties first, rather than in `/tmp`; refreshed, locked
  and aged as theirs, and mounted read-only at `/scan/db`, where trivy reads `java-db/`. It is there
  when its `metadata.json` reads and its `trivy-java.db` begins with SQLite's header, and every image
  scan needs it, jars or none — whether an image holds a jar is the scan's to find. An image scan
  without it answers `db_missing`, or `db_stale` for one built more than 5 days before the scan starts,
  each with `reason: "java"`; no SBOM is taken without the Java database. The 5 days hold: upstream
  builds it daily at 00:00 UTC (aquasecurity/trivy-java-db, `cron.yml`), its metadata names the next
  build 3 days on (measured 2026-10-12: `"UpdatedAt":"2026-10-12T01:00:58Z"`,
  `"NextUpdate":"2026-10-15T01:00:58Z"`), and trivy downloads only once that has passed, so a copy
  refreshed every 6 hours is at most about 3¼ days old. A database that passes that check and still
  fails trivy ends the run without an SBOM: `not_sarif`, never a clean answer. An image answer carries
  no `db_built_at`: nothing on it was matched.
- **`target: "sbom"`** re-matches what the server keeps of an image's SBOM — one layer's components and
  the image's operating-system component — with no image read. Part `sbom` is one CycloneDX JSON
  document of at most 10 MiB, written to `/scan/sbom.cdx.json`, and **it must have the shape of a slice
  of trivy's own image SBOM** (`trivy image --format cyclonedx`, which is all the server sends): the
  executor never predicts what trivy's parser skips, renames or nests; it refuses anything trivy 0.74.0
  does not write, `400` with the message naming the component and why. **One parse, bounded as it
  streams**: the part is read once, token by token, and written again as it is read — and those bytes,
  never the part as sent, are what is checked below, written to `/scan/sbom.cdx.json` and read by trivy,
  so no two readers see two documents. Refused while it streams, before anything is built past it: a
  byte-order mark, bytes that are not valid UTF-8, more than one JSON document, more than 1,000,000
  tokens, nesting deeper than 16 anywhere, any array — components, properties, licenses, hashes,
  dependencies — longer than 50,000, any token — a key, a string, a number — of more than 64 KiB, cut
  off while it is read (the decoder never holds more of one unread: a string is never materialised past
  its bound), a string holding U+FFFD (what a lone surrogate decodes to), and an object with a key twice
  (Go keeps the last of two, another reader the first). Past the part's own 10 MiB, which the body cap
  holds before any of this, what is allocated is bounded by these caps, and the bytes written again are
  never more than twice the part's (strings are written without HTML escaping). **Then the shape**,
  exactly:
  - `components` at the top level alone, none of them nesting `components`;
  - `metadata.component`, if present, is trivy's container: `type: "container"`, no purl, no
    `components`;
  - each component is of type `library`, `application` or `operating-system`, and a purl is on a
    `library` alone;
  - each component has a `bom-ref` of at most 2 KiB, unique in the document (`metadata.component`'s
    counted); its form is trivy's — the purl, a UUID, or a ref an SBOM embedded in the image gave it (a
    jar's own `log4j-core`) — and is not checked: the read check joins it with the purl;
  - every purl parses with packageurl-go v0.1.6, trivy 0.74.0's own parser, and its library component's
    `group`, `name` and `version` are what trivy writes beside it (`pkg/sbom/io/encode.go`, `component`;
    `pkg/purl/purl.go`, `New`), because trivy's decoder matches by the component's name, not the purl's:
    the `version` is the purl's, with `<epoch>:` before it when its `epoch` qualifier is not `0`; for
    `npm` and `maven` the `group` and `name` are the purl's namespace and name; for any other type there
    is no `group`, and the `name` is the one from which trivy derives the purl's namespace, name and
    subpath — `deb`, `rpm` and `apk` the purl's name (the namespace is the distribution's); `golang`,
    `composer`, `swift` and `julia` `<namespace>/<name>`; `cocoapods` `<name>/<subpath>`; any other the
    name — each compared after packageurl-go normalises both, so `zope_interface` is `pkg:pypi/zope-
    interface` and `github.com/Azure/go-autorest` is `pkg:golang/github.com/azure/go-autorest`;
  - a part holding any operating-system package — a `deb`, `rpm` or `apk` purl — has exactly one
    `operating-system` component, which trivy needs to match them.
  **After the run, every component carrying a purl must be among the packages trivy lists as read**
  (`--list-all-pkgs`: `Results[].Packages[].Identifier`), by the pair of its `bom-ref` and its purl, both
  purls as packageurl-go normalises them — as trivy does, so trivy's spelling (an npm scope's `@` as
  `%40`, a PyPI underscore as a dash) refuses nothing; one that is not — a purl of a type trivy does not
  match, which it skips without a word, or a `bom-ref` trivy read for another purl — makes the answer
  `not_finished: "not_sarif"` with `reason: "unread"`, never a clean report. **Listed is not scanned**:
  trivy lists packages its vulnerability drivers never match (a `conda` package, an operating-system
  package of a distribution it has no driver for), so the report answer carries `unscanned`, the
  `bom-ref` of every component whose purl trivy 0.74.0 does not scan — scanned are the purl types
  `npm`, `pypi`, `maven`, `golang`, `gem`, `cargo`, `composer`, `nuget`, `pub`, `hex`, `conan`, `swift`,
  `cocoapods` and `bitnami` (`pkg/detector/library/driver.go`, `NewDriver`, by trivy's purl type
  mapping), and `deb`, `rpm` and `apk` when the part's operating-system component names a distribution
  trivy has a driver for (`pkg/detector/ospkg/detect.go`, `drivers`) whose `Detect` matches — a driver
  that answers nothing, as CoreOS's does unconditionally (`pkg/detector/ospkg/coreos/coreos.go`), is not
  a scan; `conda`, `julia`, `k8s` and every other type are not. The executor's table cites each driver's
  `Detect` by file and line, so a trivy upgrade re-checks each. `unscanned` covers three things, each from trivy's source: a purl type or
  distribution trivy does not scan, as above; **a component without a version, whatever its type**
  (`pkg/detector/library/detect.go`, and the operating-system drivers that skip an empty version); and
  **trivy's named skips**, pinned in one table beside the scanned types — `gpg-pubkey` on every
  distribution (`pkg/detector/ospkg/detect.go`) and an AlmaLinux modular package without its
  `modularitylabel` (`pkg/detector/ospkg/alma/alma.go`). Trivy's drop of third-party-repository packages
  reads a field no SBOM carries, so it skips nothing here. The server counts each `unscanned` component
  as not checked (a note, never clean).
  A package without a purl (a `library` component; an `application` or `operating-system` component holds
  packages and is none) is not matched by the re-match; the server counts every such package of the layer
  as not checked (a note, never clean), the same rule (finding.md, *System libraries and build tools*). The properties trivy reads beside a purl (an operating-system package's source
  name and version, `aquasecurity:trivy:SrcName`) are its own and are not checked against the purl:
  the part is trivy's output, kept by the server.
  Trivy runs `sbom --format json --list-all-pkgs --scanners vuln --cache-dir /scan/db --ignorefile /dev/null
  --skip-db-update --skip-java-db-update --offline-scan --disable-telemetry --skip-version-check
  --no-progress /scan/sbom.cdx.json` on trivy's database as a tree scan reads it — on Kubernetes a
  client of `canyon-trivy` — and reads no Java database: a jar component is matched by its purl against
  the main one. Its stdout, kept as above, is the answer's `report` when the executor's decoder reads it
  as one trivy JSON report (`SchemaVersion` 2, `Results` a list) each of whose vulnerabilities carries
  trivy's `Layer.DiffID` — the `aquasecurity:trivy:LayerDiffID` of the component it matched — and its
  `PkgIdentifier.BOMRef`, that component's `bom-ref`, so no match is answered without its layer or its
  component; else `not_sarif`. A vulnerability is joined to its component by `PkgIdentifier.BOMRef`
  (its `PURL` beside it), never by name and version, which two components can share. `db_built_at`,
  `db_missing` and `db_stale` are a tree scan's.
- **`target: "packages"`** lists the packages a package's files name, for the custom rules checked by
  code (finding.md, *A custom rule checked by code*; added 2026-11-04, #2644). It takes `app`,
  `package` and the part `tree`, exactly as a tree scan does, and no `settings`, no `image` and no other
  part; anything else is `400 {error:"invalid_request", message}`. It runs as a tree scan runs — the
  container above, no network, a read-only root fs, user 65534, counted against
  `CANYON_EXECUTOR_SCANS` — with no database, so it never waits on one, is never busy for one, and never
  answers `db_missing` or `db_stale`, nor `db_built_at`; on Kubernetes it is no client of
  `canyon-trivy`, its pod labelled `canyon.scan` alone, so it reaches nothing. Trivy runs `fs --format json
  --list-all-pkgs --scanners license --include-dev-deps --cache-dir /tmp/trivy --cache-backend memory
  --ignorefile /dev/null --skip-db-update --skip-java-db-update --offline-scan --disable-telemetry
  --skip-version-check --no-progress /scan/tree`: the license scanner is the one that lists packages and reads no
  vulnerability database, a lockfile's development packages are listed with the rest, and
  `--offline-scan` keeps trivy from fetching anything — a Maven parent or
  BOM it would fetch is not, and a package whose version only that would give is listed with none. Its
  stdout, kept as a tree scan's (10 MiB), is the answer's `packages` when the executor's decoder reads it
  as one trivy JSON report (`SchemaVersion` 2, `Results` a list, or absent: trivy leaves it out when it
  lists nothing, a tree with no packages); else `not_sarif`. Past the 10 MiB
  kept it is `over_cap` with `reason: "report"`. The tree is never changed for it: no skip path applies.
  A review asks for it from its own step, once, while a custom rule checked by code is on and
  `sarif.trivy` is on with `run` — never a task of the scan queue, so the queues' sum (*Busy*) leaves it
  out: past the limit it is answered busy and asked again, and the review never fails for waiting.

## On Kubernetes

The same calls, served by `cmd/canyon-executor-kube`; where this section says nothing, a call is as
above. It needs Kubernetes 1.29 or newer: an app's pod runs `canyon-identity` and, for choice 2,
oauth2-proxy as native sidecars — init containers with `restartPolicy: Always` — which versions before
1.29 silently drop, and the app never starts. There is no Docker socket: the service account `canyon-executor` holds namespaced Roles in the
three namespaces below and `get` on the kpack ClusterBuilder `canyon`, and nothing else — no Role in
`canyon`, where it runs beside the server, so the server's Secrets are never its to mount. Its own
NetworkPolicy admits the server's pods alone. Every label below that names no value carries `"true"`.

| Namespace | Holds |
|---|---|
| `canyon` | the server, the executor, Canyon's egress gateway (Envoy, pods labelled `app.kubernetes.io/name: canyon-egress`, below), and the router (nginx, pods labelled `canyon.front-door`): one wildcard Ingress sends every app address to it, and it proxies `<app>.<target>.<domain>` and `<app>-<target>.<domain>` — both forms, always — to `<app>-<target>.canyon-apps.svc:4180` |
| `canyon-apps` | per app and target: the Deployment, Service and NetworkPolicy `<app>-<target>`, the ConfigMap `canyon-access-<app>-<target>`, the Secret `canyon-env-<app>-<target>` and, for choice 2, the Secret `canyon-signin-<app>-<target>` — each, and the Deployment's pod template, labelled `canyon.app: <app>` and `canyon.target: <target>`; and one ConfigMap `canyon-db-ca` every app shares, once a `/run` carries `database_ca`, and one ConfigMap `canyon-egress-ca` every app shares, once a `/run` carries `egress.ca` |
| `canyon-builds` | kpack Builds, labelled `canyon.build`, and the registry credential `canyon-registry` |
| `canyon-scans` | scan Jobs (`canyon.scan`), grype's and trivy's Java database's refresh Jobs (`canyon.scandb`) and their volumes `canyon-scandb-grype` and `canyon-scandb-trivy-java`, and trivy's server `canyon-trivy` (`canyon.trivy`) |

**Secrets are written, never read.** The Roles grant `create` and `delete` on Secrets, and never
`get`, `list`, `watch`, `patch` or `update`: a patch answers the whole object. The answers to those three calls are the only
reads the Roles allow, and are discarded unread; a Secret is replaced by deleting it and creating it
again. No Role grants `pods/exec` in `canyon-apps`, which would read an app's environment. Kubernetes
lets whatever creates pods in a namespace mount that namespace's Secrets, so this is how the executor
keeps no copy and reads none back (invariant 2), not a wall against a compromised executor — which
receives every app's values on each `/run` anyway.

**The registry.** `CANYON_KUBE_REGISTRY` is the registry host every image lives under (on ECR,
`<account>.dkr.ecr.<region>.amazonaws.com`, the repository `canyon/<app>` created at the app's first
build). `image` is `<registry>/canyon/<app>@sha256:<digest>`, the digest of the registry's manifest —
pullable, never a tag — and `/run` refuses an image outside its registry's `canyon/<app>` with `400`.
On ECR the executor signs with its own IAM role (IRSA), never the node's, and that role is the bound
on `DeleteRepository`'s `force` that the code can't move. It holds exactly these:

| Actions | Resource |
|---|---|
| `ecr:GetAuthorizationToken` | `*`, as ECR scopes it to nothing narrower |
| `ecr:CreateRepository`, `ecr:DeleteRepository`, `ecr:ListImages`, `ecr:BatchDeleteImage`, push and pull | `arn:aws:ecr:<region>:<account>:repository/canyon/*` |
| push and pull | `arn:aws:ecr:<region>:<account>:repository/canyon-builder` |
| pull | `arn:aws:ecr:<region>:<account>:repository/canyon-run`, `arn:aws:ecr:<region>:<account>:repository/canyon-build` |

Pull is `ecr:BatchCheckLayerAvailability`, `ecr:GetDownloadUrlForLayer` and `ecr:BatchGetImage`.
Push is those three plus `ecr:PutImage`, `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart` and
`ecr:CompleteLayerUpload`. kpack pushes the builder and pulls the build and run images with
`canyon-registry`, this role's token.

**`/build`.** The tree becomes a single-layer source image `<registry>/canyon/<app>:src-<nonce>`; a
kpack Build `<app>-<nonce>` in `canyon-builds` builds it, with the builder image the ClusterBuilder
`canyon` resolved — its stack's run image `canyon-run`, Canyon's own, named by the deploy by digest — to `<registry>/canyon/<app>:<package>-<nonce>`, its cache the image
`<registry>/canyon/<app>:cache`; the answer's lines are the build pod's step containers' logs in step
order, and its `image` the Build's `status.latestImage`. **The architecture an app is built for** (amended
2026-10-27, #2440): kpack makes the ClusterBuilder's image for the architecture of the node its controller runs
on, pins each build pod to its builder's architecture itself (kpack v0.18's `BuildPodBuilderConfig`; its webhook
refuses a Build whose `spec.nodeSelector` names `kubernetes.io/arch`), and a build makes an image of that
architecture. The Build carries no `nodeSelector`, nor does a `/rebase`'s, whose pod only rewrites manifests;
each `/run` pins the app to its own image's architecture (Deployment, below). **Build values**, and the executor's own
build variables every build runs with, are literal `spec.env` entries of that Build, one per value, which kpack copies into
its build pod's containers' environment:
the Build and its pod hold them, and no other object. They are not in a Secret: kpack since v0.17.2
refuses `spec.env[].valueFrom.secretKeyRef` (kpack #1945 — whoever could create a Build read any Secret
of its namespace through it). The Build
lives only as long as the build: once the build answers, image or error, the executor deletes it, its pod
with it, and the source tag, and when it starts it deletes every Build labelled `canyon.build`, and each swept
Build's source tag, that a crash left. The executor's Role is the only one the deploy writes that grants
`get`, `list` or `watch` on Builds or pods in `canyon-builds`, so nothing else of Canyon's reads the values
back. Outside a Secret, three of a Secret's protections don't reach them, and **the operator restores
each**:
- **Who reads them.** In `canyon-builds`, nobody but the executor and kpack's controller (beyond the
  cluster's admins and Kubernetes' own controllers) holds `get`, `list`, `watch`, `patch` or `update`
  on `builds.kpack.io` or `pods`, `get` or `create` on `pods/exec`, or `patch` or `update` on
  `pods/ephemeralcontainers`. That includes ClusterRoleBindings: the built-in `view`, `edit` and
  `admin` reach pods, and so do agents that list every pod. `get` on `nodes/proxy` reads every pod of
  a node through its kubelet.
- **At rest.** Add `builds.kpack.io` and `pods` to the API server's `EncryptionConfiguration` beside
  `secrets`. On a managed control plane, use what it offers for data beyond Secrets.
- **Audit logs.** Record `builds.kpack.io` with its subresources (`builds/*`) and `pods` in
  `canyon-builds` at the audit level `Metadata`. At `Request` or `RequestResponse` the log holds a
  Build's spec, which kpack's status updates send whole, and its pod's, values included, in plaintext.
  Where you can't set the audit policy, restrict who reads the log.

**The app's own build scripts never hold a registry credential**: the Build's service account `canyon-build`
pushes with the docker-config Secret `canyon-registry`, which kpack mounts in its prepare and export
steps alone and the executor writes afresh every 6 hours (an ECR token lasts 12) — never a web identity
the pod's every container would be handed. Under strict enforcement every pod needs a policy for what
it reaches: the deploy writes one for every pod of `canyon-builds` (DNS, and TCP 80 and 443 at public
addresses, where the registry is, outside every private address an app's own policy leaves out: *A
build reaches no private or link-local address*), and the router's, the server's and the executor's.

**`/run`.** As above, and: `network` is checked as above and names nothing more — an app's isolation is
its NetworkPolicy; `identity.jwks_url` is exactly `http://127.0.0.1:4182/.well-known/jwks.json`, the
key set `canyon-identity` makes when it starts, else `400`; `identity.issuer` is Canyon's public URL,
which the sidecar's tokens carry and under which it finds the no-access screen. In `canyon-apps` it
writes, in this order, so that no pod of the app exists before its policy:

- the NetworkPolicy `<app>-<target>`, selecting the pod, for ingress and egress. **In**: TCP 4180 from
  pods labelled `canyon.front-door` in `canyon`, TCP 4183 from pods labelled
  `app.kubernetes.io/name: canyon-executor` in `canyon` (one peer, both selectors; *What the app
  answered*), and nothing else. **Out**: DNS (UDP and TCP 53 to the
  `kube-dns` pods of `kube-system`), TCP 5433 to `CANYON_KUBE_DB_CIDRS` (comma-separated private CIDRs:
  the apps database, below) and, without `egress`, TCP 80 and 443 to every address outside `10.0.0.0/8`,
  `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`, `0.0.0.0/8`, `192.0.0.0/24`,
  `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4` and `168.63.129.16/32` — the ranges Docker's floor refuses
  (`deploy/egress/floor.sh`; **amended 2026-10-16**, #2097), which assume the cluster's pod and Service
  addresses are among them — **with `egress`, TCP 3128 to the pods labelled `app.kubernetes.io/name:
  canyon-egress` in `canyon` instead, Canyon's egress gateway, and no public address at all; DNS through
  kube-dns stays open until its own later change** (**amended 2026-10-15**, #2097; design/EGRESS.md §2.2:
  the floor, §3.6: DNS), and with `egress` and `CANYON_KUBE_SIGNIN_DIRECT`, TCP 443 to each address
  it names, one rule (**amended 2026-10-17**, #2316; *Sign-in past the gateway*, below) — and nothing else: no other app, no control plane, no instance metadata. Every
  `/run` writes the policy its own `egress` says, so a `/run` that changes it changes it for the pods the
  rollout replaces too: they reach neither the old way out nor the new one until the rollout ends, never
  past the floor. A `/run` that fails puts the policy it read back with the template, at once when that
  policy is the narrower; when it is the wider — the `/run` floored an open app — only once nothing of the
  failed rollout can run floored under it: the Deployment has seen its put-back template, every
  ReplicaSet of the `/run` is scaled to nothing and has seen that, and none of its pods is left — a pod
  list alone is empty before the ReplicaSet makes its first pod — waited for 60 s at most (the executor's
  Role lists the ReplicaSets of `canyon-apps` for that alone); short of it, or if the put-back fails, the
  narrower policy stays and the `/run`'s failure says so (**amended 2026-10-16**, #2097). On EKS this holds from a pod's first packet only with
  the vpc-cni add-on's `NETWORK_POLICY_ENFORCING_MODE` `strict`; in its default mode a new pod
  accepts every connection until its policy reaches it.
- the Service `<app>-<target>`: port 4180 to the pod's 4180 — never 3000 (invariant 10), and the same
  number on both sides, which VPC CNI's policies need.
- the Secret `canyon-env-<app>-<target>`, one key per `env` variable, replaced whole by every `/run`
  and deleted by `/stop`: it lives exactly as long as the Deployment, which Kubernetes restarts pods
  from, and has one version — a pod of the previous template that restarts after a failed `/run`
  starts with that `/run`'s values. For choice 2, the Secret `canyon-signin-<app>-<target>` likewise:
  `signin.client_secret` and a cookie secret made fresh by each `/run` (so each deploy signs the app's
  visitors in again), read by oauth2-proxy alone.
- the ConfigMap `canyon-access-<app>-<target>`: `access` as `access.json`, its `updated_at` in the
  annotation `canyon.updated_at`. A ConfigMap already holding a newer one is kept, as `PUT /access`
  keeps it: a share made during the build is not undone by the deploy.
- the ConfigMap `canyon-egress-ca`, when the `/run` carries `egress.ca` (Egress, above): each root under its `name`,
  `bundle.pem` and `system-and-canyon.pem`, written before the Deployment and mounted read-only at `/etc/canyon-egress` in `app` alone.
  It carries the annotations `canyon.ca_newest`, the newest root's `notBefore` (RFC 3339, UTC), and `canyon.ca_roots`,
  how many roots it holds. A `/run` whose set is older keeps the ConfigMap as it is: its newest root's `notBefore`
  earlier, or the same with more roots, as a set read before a root's end holds that root too. So a `/run` that read
  the roots before a rotation or a root's end never puts back a set a later `/run` replaced (#2143's review). A
  ConfigMap with neither annotation, written before them, is replaced.
- the ConfigMap `canyon-db-ca`, when the `/run` carries `database_ca`: it as `ca.pem`, written when the
  ConfigMap holds anything else and never deleted — the host has one apps database, and every app
  mounts the same bundle.
- the Deployment `<app>-<target>`: one replica, `maxSurge` 1, `maxUnavailable` 0, `minReadySeconds`
  10, `progressDeadlineSeconds` 300, `revisionHistoryLimit` 0 (no earlier ReplicaSet is left for
  `kubectl rollout undo` to start past the gate, design/OPERATIONS.md §3.4; a failed `/run` still puts
  back the template it read before its update), no service-account token, no service links, no shared process
  namespace, no host network and no host port. Its pod carries `nodeSelector: {kubernetes.io/arch: <the
  image's architecture>}` (#2440), so it is never scheduled on a node of another architecture, where its image
  would not start. Before anything is written, the executor reads the image's config from
  `CANYON_KUBE_REGISTRY` with its own credential, by the digest the `/run` names (an image on another host is
  refused, above, before any request), its manifest and its config each read only once they name a size of 1 to
  4 MiB. An image index (a multi-arch image), a config whose `os` is not `linux` or whose `architecture` is empty,
  or one that cannot be read, runs nowhere: the `/run` fails with `the image <image>'s
  architecture could not be read, so Canyon runs it nowhere: <why>`, and writes nothing; an image the registry
  answers `404` for is `404` (An image that is not there, above). Only the architecture is compared, never its variant (arm `v6`, `v7`):
  kpack builds `amd64` and `arm64` images. A promotion, a rollback and a patch are each a `/run`, so each image
  runs on nodes of its own architecture, on a cluster of either or both; an existing Deployment takes the
  selector at its next `/run`, which rolls the app out once. Its pod runs `canyon-identity` and, for choice 2,
  oauth2-proxy as native sidecars (init containers with `restartPolicy: Always`), each pinned by
  digest as the scan images are (`CANYON_IMAGE_IDENTITY`, `CANYON_IMAGE_OAUTH2_PROXY`), then the container `app`: the image, `PORT=3000`,
  the three `CANYON_IDENTITY_*` and the env Secret — with `egress`, the proxy variables (Egress, above) as
  literal values of the template, none of them a secret, so a put-back template carries its own: the
  Secret keeps `env` alone, and `JDK_JAVA_OPTIONS` is `$(JDK_JAVA_OPTIONS)` — `env`'s own value, which
  Kubernetes reads from the Secret — then the proxy properties when `env` gives one, the properties alone
  when not; `canyon-identity` and oauth2-proxy carry the same variables but `JDK_JAVA_OPTIONS`, their
  `NO_PROXY` and `no_proxy` followed by the front door's sign-in hosts `CANYON_KUBE_SIGNIN_DIRECT` lists
  (*Sign-in past the gateway*, below); and the pod
  template is labelled `canyon.floor`, which any other policy of `canyon-apps` leaves out, but the
  operator's own to a sign-in provider inside the cluster (*Sign-in past the gateway*) — the pods a
  flooring rollout replaces carry no such label, so a policy like the k3s chart's `canyon-apps-gateway`
  still lets them out until the rollout has replaced them (the executor patches no pod) (**amended
  2026-10-16**, #2097) —
  and nothing else in its environment, ready once a
  TCP connect to its port 3000 succeeds, tried every second — and, when the
  `/run` carries `database_ca`, `canyon-db-ca` mounted read-only at `/etc/canyon-db`; as on Docker,
  every capability dropped and no privilege escalation; the sidecars, Canyon's images, also run as no
  root (`runAsNonRoot`), on a read-only root file system and under the runtime's seccomp profile
  (`RuntimeDefault`), which the app, any image an agent built, does not (**amended 2026-10-23**, #2451). Limits: the app 1 CPU and 1 Gi, each sidecar
  100m and 64 Mi; processes, the node's `podPidsLimit` (Kubernetes has no per-pod setting). The app
  requests `CANYON_KUBE_APP_CPU_REQUEST` CPU and `CANYON_KUBE_APP_MEMORY_REQUEST` memory, each a
  Kubernetes quantity at or below its limit (an executor whose settings are not does not start): 10m
  and 256 Mi unset, measured on the AWS test cluster's 53 running apps at 1.2m and 136 Mi at their 99th
  percentile, where 100m each filled the nodes and no scan could be scheduled (amended 2026-10-05,
  #1637).

**Canyon's egress gateway** (**amended 2026-10-15**, #2097; design/EGRESS.md §2.1–2.2). `deploy/k8s/egress.yaml`
runs Envoy, `deploy/egress/envoy.yaml` and `envoy.sh` byte for byte, as the Deployment `canyon-egress` in
`canyon` behind the Service `canyon-egress` on 3128 — so a `/run`'s `egress.proxy` there is exactly
`http://canyon-egress.canyon.svc:3128`, any other `400`, as a shorter name would resolve in the executor's
namespace and not the app's — its resolvers the pod's own (kube-dns), its receiver the server's.
Its replicas, a rollout's surge among them, times its `CANYON_EGRESS_CONCURRENCY`, times two (each worker holds
`:3128`'s access-log stream and the interception listener's) stays under the receiver's 32 streams. Its NetworkPolicy is the floor (design/EGRESS.md §2.2): in, TCP 3128 from the pods of
`canyon-apps` labelled `canyon.app` and from the executor's; out, DNS to kube-dns, the server's pods on the
receiver's port, and TCP 80 and 443 to every address outside the ranges above, and nothing else. **The
gateway must answer**: with `egress`, before it writes anything the executor opens TCP once to `proxy`'s
host on 3128, 2 s at most, from its own pod — its NetworkPolicy admits that — and nothing answering is the
executor's failure, `502`, `nothing answers at <host>:3128, Canyon's egress gateway`, the running version
untouched. That proves Envoy answers, not the app's own path to it, which the app's NetworkPolicy above is
and the kind suite proves (`TestKindAppReachesOnlyEnvoy`).

**The pod's token** (**added 2026-11-09**, #2142; design/EGRESS.md §4.3). With `egress`, the pod proves which app
env it is to the gateway with a token, never its address:

- **The ServiceAccount.** `/run` creates `canyon-app-<app>-<target>` in `canyon-apps`, bound to nothing,
  `automountServiceAccountToken: false`, when it is not there. The executor only creates it, and never reads
  or changes it. `/stop`, and a first `/run` that fails, delete it with the rest. The pod runs as it, its own
  `automountServiceAccountToken` still `false`.
- **The token.** One projected `serviceAccountToken` volume, audience `canyon-egress`, `expirationSeconds` 600,
  path `token`, is mounted read-only at `/var/run/canyon-egress` in `canyon-identity` alone. The app and
  `oauth2-proxy` mount nothing of it, and the pod shares no process namespace.
- **The forwarder.** `canyon-identity` gets `CANYON_IDENTITY_EGRESS_PROXY`, `proxy`, and
  `CANYON_IDENTITY_EGRESS_TOKEN`, `/var/run/canyon-egress/token`, and listens on `127.0.0.1:4184` before its
  first fetch.
  - It takes `CONNECT` and absolute-form requests, and refuses any other `400`.
  - It relays each to `proxy`'s host and port alone, never to an address a request names.
  - It sends `Proxy-Authorization: Bearer <token>` with the token read again for every call, in place of any
    `Proxy-Authorization` the caller sent.
  - With no token to read it relays nothing, `502`. The gateway's own refusals reach the caller as the gateway
    gave them.
- **The variables.** The proxy variables above, in the `app` container, `canyon-identity` and `oauth2-proxy`
  alike, name `http://127.0.0.1:4184`, the pod's forwarder, and not `proxy`. The JVM's properties name
  `127.0.0.1` and `4184`.
- **The answer.** `/run`'s `200` carries `token_callers: true`. Without `egress` there is no ServiceAccount, no
  token and no forwarder, and the answer carries no `token_callers`. Docker's answer never carries it, and
  neither does an executor's from before #2142. The server marks an env `token_only` only on an answer with
  `token_callers` and `previous_gone` both `true` (contracts/routes.md, *The check*), so a `/run` an older
  executor answered never marks an env whose pods carry no forwarder.

A job pod on Kubernetes, once jobs run there, carries no forwarder until its own sidecar exists
(design/EGRESS.md §4.3). Until then it is named by its address and never intercepted.

**Sign-in past the gateway** (**amended 2026-10-17**, #2316, the owner's decisions; design/EGRESS.md §2.2).
The gateway refuses every private address, so without more a floored app whose front door signs people
in at an internal identity provider never starts: its sidecars fetch the provider's keys or discovery
through the gateway. `CANYON_KUBE_SIGNIN_DIRECT` lists the sign-in hosts its sidecars reach directly
instead, comma-separated, each `<host>` or `<host>=<address>/32`: `<host>` a lower-case DNS name or an
IPv4 address — no wildcard, no leading dot, no port — named once; `<address>` one IPv4 address outside
`169.254.0.0/16`. An empty entry, a host out of that shape or named twice, an address without `/32`, a
range, an IPv6 address or a link-local one, and the executor does not start (*A bad value refuses the
start*, below). With it, every `/run` with `egress`:

- adds to the NetworkPolicy one egress rule after the gateway's, TCP 443 to each address the setting
  names, each once — none when it names none;
- sets `canyon-identity`'s and oauth2-proxy's `NO_PROXY` and `no_proxy` to the variables above, then each
  of the front door's sign-in hosts the setting lists, lower-cased, each once: for choice 2
  `signin.issuer`'s host; for choice 1 `oidc` `issuer`'s, then `jwks_url`'s; for `alb` none. A host it
  does not list is reached through the gateway as before, so a public provider keeps working with the
  setting on. Any of the door's URLs on a listed host naming a port other than 443 is `400`: the rule
  admits 443 alone. The app container's are unchanged. As every proxy client reads `NO_PROXY`, a listed
  host's subdomains go direct too — still to the listed addresses on 443 alone, as the policy drops
  anything else.

A host listed with an address is a provider outside the cluster, which the rule admits. A host listed
without one is a provider inside the cluster: its Service's address is rewritten to a pod's before any
policy matches, so no `/32` reaches it, and the operator's own policy admits floored pods to the
provider's pods (the docs' *Kubernetes* guide, *Install for production*, gives one). A host listed without
an address is only for a provider whose sign-in host resolves, inside the cluster, to its own Service —
never one published through an ingress, gateway or load balancer, whose pods serve everything else too. Every endpoint the
sidecars call — the discovery document's `token_endpoint` and `jwks_uri` among them — must be on a listed
host, or it is called through the gateway.

Without the setting, or without `egress`, neither is written. The costs, accepted: the whole pod, `app`
included, reaches the listed addresses on 443, and those calls pass no gateway, so none is recorded.
The kind suite proves the rule and that an unlisted provider stays behind the gateway
(`TestKindSigninPastTheGateway`). The fixture's `kube_signin_policy` is the NetworkPolicy written for
`kube_run_request` with `kube_egress` and `CANYON_KUBE_SIGNIN_DIRECT` `login.meridian.example=10.30.0.7/32`.
Each `/run` writes both, so a change to the setting reaches a running app only at its next `/run`:
removing or narrowing it leaves a running app's rule and variables in place until then, and
`POST /sidecars` changes `canyon-identity`'s image alone.

The fixture's `kube_objects` are the Service and NetworkPolicy written for its `kube_run_request`, as
Kubernetes spells them, and `kube_egress_policy` the NetworkPolicy written for it with `kube_egress`; the
executor's test renders that request and expects them. **Up** is as on
Docker, ready through the settle window: `/run` answers once the new pod has been ready for 10 s and, as on Docker
the previous container is removed by then, once no pod of an earlier template remains, terminating ones included,
and no ReplicaSet of one is left with a pod — an earlier template's being one labelled with another `canyon.run`,
each read from the cluster, never inferred from time (**clarified 2026-11-09**, #2142):
it waits for them at most their termination grace period (`terminationGracePeriodSeconds`, Kubernetes' 30 s unless the template sets one) and 10 s
more, then answers `previous_gone`, `true` when none remained and `false` when the bound passed first — the new
version is up either way, and nothing is put back (**added 2026-11-07**, #2135; answered from #2143 on). The server
moves an env's trusted roots to the carried set only on `previous_gone` `true` (contracts/routes.md, *Interception*, Trust): `false`,
or an answer without it, leaves the intersection. A pod
not available within 300 s, or whose container restarts first, fails the `/run` with `502` naming the
pod's reason — and, when the `app` container restarted, its `output` (above), read before anything is
put back. The `app` container runs with `terminationMessagePolicy: FallbackToLogsOnError`, as
`canyon-identity` does, so a crash keeps its last words in the pod's status, the kubelet's tail of its log (at most 80 lines, 2 KiB):
`output` is that `lastState.terminated.message`, `""` when it is empty or the status records no
termination: the kubelet falls back to the log only when the app exited non-zero and wrote nothing to
its termination-message path, so an app that exits 0 having written none leaves no message, and the
reason names its exit 0. The stopped container's log is never read: it can already be gone, and the
kubelet then answers the read `200` with a sentence of its own that no status field marks, which an
app's line would otherwise carry (**amended 2026-10-04**, #1549, #1655). The kubelet cuts the message at a byte bound, the log's last 2 KiB
or the first 4 KiB of what the app wrote to `/dev/termination-log`; a line a bound fell inside is
dropped whole before the naming, as a log's is. Only `output` is named: the pod's status holds the tail
as the app wrote it, values included, for as long as the pod lives, and it reaches whoever reads pods,
the audit log and etcd, where a log never goes. `canyon-identity`'s message ends the reason as `, its
last words: "<message>"` (quoted as Go quotes a string, so it stays one log line), and is absent when
the status holds none: an issuer the sidecar cannot reach is then in the server's log, though the
failed `/run` deletes the pod. It is not masked: Canyon's own binary, handed no secret — no value of
`env`, no client secret — and logging no token or header value (below). oauth2-proxy keeps the
default policy, so its restart is the reason alone: it holds choice 2's client and cookie secrets,
and what it writes as it ends is not Canyon's to vouch for (**amended 2026-10-08**, #1790). **The operator restores** what the build values'
three controls (above) restore, in `canyon-apps` too: who may `get`, `list` or `watch` its `pods`; `pods`
in the `EncryptionConfiguration`; and `pods` with `pods/status` there at the audit level `Metadata`. A pod whose `app` container is running and not ready at 300 s fails it with the port reason
(above) and the running instance's `output`; the executor puts the previous ReplicaSet's template back, as `kubectl rollout undo`
does, and the previous pod, never stopped, serves on. A first `/run` that fails deletes what it wrote,
the Deployment first. The answer is `{container: "<app>-<target>", addr:
"<app>-<target>.canyon-apps.svc:4180"}`; after it, the executor deletes the tags of
`<registry>/canyon/<app>` but `cache`, the `src-` tags, those of an image a Deployment of the app
runs and those of a digest in `keep` (*What `keep` spares*, above), at most 50 removed per call,
logged and never answered. It lists those tags once, before it deletes, so a
`/build` or `/run` of the same app meanwhile could lose its image: the executor relies on the server never
sending one — every `/build` and `/run` is a step of a run, and the server runs one run of a project at a
time (routes.md, the gate-runs row) — and takes no lock of its own.

**What the app answered** (**amended 2026-10-05**, #1740: a stock Django answering `400` to every
host read as a clean Live, and a visitor's request meets the front door's sign-in, never the app).
Once the pod of the new ReplicaSet `/run` waited on is up, the executor asks that pod's `canyon-identity`
once: `GET http://<its pod IP>:4183/answered`, 10 s, no body and no parameter, reading at most 8 KiB of
the answer. Its own NetworkPolicy (`deploy/k8s/executor.yaml`) gains egress on TCP 4183 to the pods of
`canyon-apps`, and each app's policy admits it there and on no other port (above). The sidecar listens
on 4183 of the pod's address, under both choices, for that one path alone (any other is `404`, any
other method `405`) and makes exactly one `GET http://127.0.0.1:3000/`: `Host` the app's address,
which `/run` sets as its `CANYON_APP_HOST` — the host the front door's `domain` and `form` give — and
never anything of the request it was asked with; no cookie, no credential, no redirect followed, 5 s.
It answers `200 {host, status, said}`: `host` its `CANYON_APP_HOST`, the app's HTTP status, and — when
the answer's `Content-Type` is `text/plain` or `application/json`, else `""` — the first non-empty line
of its body, trimmed, each Cc, Cf, Zl and Zp character removed, at most 1 KiB of
it, read from at most the body's first 64 KiB; or `200 {host, error}` when the app answered nothing, `error` exactly one of `no answer in 5 s`,
`refused`, `unreachable` and `not an HTTP answer` (contracts/routes.md, `envs.<env>.answered`), never a
library's message. The executor keeps the answer only when
its `host` is the address this `/run` set, so a pod IP another app's pod has taken since is never read
as this app's; it masks `said` with the request's `env` as it masks `output` (above), cuts it to 200 characters and answers
`{status, said}` or `{error}` as `/run`'s `answered`. A secret the 1 KiB cut fell inside is masked only
in part, as with `output`. When it could not ask — the sidecar did not answer, answered over 8 KiB, named
another host or answered anything else — `/run` answers without `answered` and the reason is in its
log, never a failed `/run`: what the app
answers is a fact the server records, never a reason to put the previous version back. The Docker
executor answers no `answered`: there the server, which reaches the app's network, asks it itself
(contracts/routes.md, `envs.<env>.answered`).

**The front door.** `front_door` is the host's one front door (contracts/routes.md,
`/api/v1/front-door`), sent whole on every `/run`. There is no choice 3 here: the server proxies no app
on Kubernetes.

```jsonc
{ "choice": 2,                          // 1: the org's gateway signs people in · 2: oauth2-proxy in the app's pod does
  "signin": { "issuer": "https://login.meridian.example", "client_id": "canyon-apps", "client_secret": "…" },  // choice 2 only
  // choice 1 instead: "gateway": "oidc", "issuer": "https://…", "audience": "…", "jwks_url": "https://…", "cookie": "_pomerium"
  //               or: "gateway": "alb", "alb_arn": "arn:aws:elasticloadbalancing:<region>:<account>:loadbalancer/app/<name>/<id>", "cookie"?
  "domain": "canyon.meridian.example",  // a lower-case DNS name whose first label is neither preview nor prod
  "form": "app.env.domain" }            // app.env.domain: <app>.<target>.<domain> · app-env.domain: <app>-<target>.<domain>
```

The router serves both forms without knowing the domain: a host whose second label is `preview` or
`prod` is `<app>.<target>.<domain>`, any other `<app>-<target>.<domain>` — which is why `domain`'s
first label is never `preview` or `prod`. **When it cannot reach the app, the router says why, in a
small HTML page:** a host whose `<app>-<target>` Service does not resolve — nothing is deployed there,
as after an undeploy or a delete — is `404` with *No app is running at this address. It may have been
deleted, or never deployed here.*; a Service that resolves and does not answer is `502` with *This
app isn't answering right now. It may be starting, or it may have stopped.* What an app answers
itself, an error included, passes through untouched. Whether an app is deployed at an address is not
a secret, so these pages disclose nothing: under choice 2 the router answers before anyone has signed
in, and a running app's oauth2-proxy sends a visitor with no session straight to sign-in (below),
which already shows the app is there. A page added to the router must not assume a signed-in visitor
either. The container facing
the router listens on 4180 — `canyon-identity` for choice 1, oauth2-proxy for choice 2 with
`canyon-identity` behind it on `127.0.0.1:4181` — and `canyon-identity` serves its key set on
`127.0.0.1:4182` alone, and `/answered` on 4183 of the pod's address (*What the app answered*). It believes one thing per mode, and nothing else, not a header the router sets:

- **choice 1, `oidc`**: an ID token in `Authorization: Bearer` — the gateway passes it there and in no
  other identity header — verified against `jwks_url` (fetched
  again on an unknown `kid`, at most once a minute; a key set with no keys never replaces the keys held, #2451): signed RS256 or ES256, `iss` the `issuer`, `aud`
  holding `audience` — the gateway's client — and not expired.
- **choice 1, `alb`**: the AWS ALB's `x-amzn-oidc-data`, an ES256 JWT. Its `kid` — one path segment of
  letters, digits and dashes, else refused before any fetch — names the PEM key at
  `https://public-keys.auth.elb.<region>.amazonaws.com/<kid>`, `<region>` the ARN's (partition `aws`
  alone), cached per `kid`; the header's `signer` equals `alb_arn` and its `exp` is after now. The ALB
  pads the JWT's segments with `=`; the padding is removed before decoding and nothing else is
  relaxed. Its claims are the identity provider's user-info answer, not the ID token's, so groups
  count only where that answer carries them (Entra's does not).
- **choice 2**: oauth2-proxy (the official image) signs people in with `signin` — `--provider=oidc`,
  scopes `openid email profile offline_access`, `--email-domain=*` (membership is the sidecar's
  check), its cookie `_oauth2_proxy` on the app's own host alone, `--cookie-refresh=4m` so the ID token
  it passes on is renewed before any provider's expires, `--reverse-proxy`,
  `--pass-authorization-header`, `--skip-provider-button=true` so a visitor with no session is sent
  to the provider at once, never shown oauth2-proxy's own sign-in page, redirect
  `https://<the app's address>/oauth2/callback` — and passes the
  ID token on as `Authorization: Bearer`, which the sidecar verifies as for `oidc`: `iss`
  `signin.issuer`, its keys from that issuer's discovery, `aud` holding `client_id`. A token that has
  only expired is `302` to `/oauth2/start?rd=<the request's path and query>`, a fresh sign-in, not a
  dead end — `rd` a path on the app's own host alone, never another host.

Then, in every mode and on every request: no token, or one that fails, is `401` and nothing reaches the
app — to a browser's navigation (an `Accept` naming `text/html` above `q=0`) a small HTML page, *You're
not signed in to this app. Whoever shared the link with you can help.*, to anything else
`{"error": "unauthenticated", "message": "sign in to open this app"}`. No detail is echoed to the
visitor; the reason goes to the sidecar's log with what to check, *check that the gateway's sign-in
rule covers this app's address*, and never a token or a header's value. The sidecar reads `/etc/canyon/access.json` — the ConfigMap, mounted whole and never through
`subPath`, so a rewrite reaches it — and a file it cannot read or parse refuses every request with
`503`. **Pull mode** (design/HAND-OFF.md §5): with `CANYON_ACCESS_URL` set it reads no file and fetches the same object from that URL instead, `Authorization: Bearer $CANYON_ACCESS_TOKEN`, at start and every minute, taking each good fetch's file as it is, an older `updated_at` included, so a file Canyon answers, a deny-all among them, is never set aside (**amended 2026-10-24**, #2451: the ConfigMap's reader alone keeps the newer); a redirect is a failed fetch, so the token goes to no other address; when a fetch fails it serves the last file for `CANYON_ACCESS_GRACE` (a duration, `0` to `24h`; a value outside that never starts), counted from the first failure, then `503`. After every good fetch it writes `/var/cache/canyon/access.json`, `{"list": <the file held>, "last_success": <that fetch's time, RFC 3339>}`, whole or not at all (written to `access.json.tmp` beside it, truncated first, then renamed over it, so a crash leaves at most that one file), on an emptyDir the org's pod spec mounts in the sidecar's container alone (**amended 2026-10-23**, #2451). The start fetch is tried again for up to 10 s, as the key set's is (#2370); when it still fails, the sidecar serves the cached list while now less `last_success` is under the grace — counted from that good fetch, so a restart never extends it — and logs that it does, until when; past it, with no cache, or one it cannot read or parse, whose list has no `updated_at`, or whose `last_success` is missing or after now — a list's `updated_at` is Canyon's clock and `last_success` the node's, so the two are never compared, it never starts, as a key set that cannot be fetched does not. A new pod has an empty cache: only a container restarted in the same pod rides out an outage. Without that emptyDir the write fails, is logged once, and the sidecar runs; a start while Canyon is unreachable never starts. Under `run_by: org` (contracts/routes.md, the preferences row) the server never calls `/run` — so the tag cleanup that follows one never runs — and the app's and the sidecar's environment in a release come from the one function `/run` builds them with, so nothing here is spelled twice. Before each fetch it dials the app's port, `127.0.0.1:3000`, for at most a second, and when the app accepts the connection the fetch carries `Canyon-App: answered` (#2470: under `delivery: repo` a check-in with it settles the deploy, contracts/routes.md, the access row); the header carries nothing else, and a dial that fails changes nothing but its absence. Under `delivery: repo` the pod itself is Canyon's too (contracts/routes.md, *The deploy repo*): `executor/kube`'s hand-off renderer builds its Deployment, Service and NetworkPolicy beside `/run`'s, from the same security contexts, resources, readiness probe and environment producers, a test holding the shared fields equal. It takes the person's email (`email`, else `preferred_username`, address-shaped and lower-cased;
an `email_verified` present and not `true` refuses, design/AUTH.md) and their groups through the file's
`groups_claim`, read as the server reads it (routes.md, *App teams*: an array's strings or an object's keys),
and matches the file's entries by those groups, `everyone`, and a person's entry by its `subject` when
it has one, else by that email (design/PERMISSIONS.md, *Grants*; **amended 2026-10-16**, #2220). A
subject names a person only under the issuer that issued it, so an entry's `subject` is compared with
the token's `sub` only when the token's issuer is the file's `subject_issuer`, not `""`. A bearer
token's issuer is the one its verifier checked, the front door's own, which takes Google's `iss`
`accounts.google.com` for `https://accounts.google.com`; an `alb`'s is the `iss` in the JWT's header,
which the load balancer signs, compared exactly: a `/` at its end is another issuer.
On any other token, or a file without it, an entry with a `subject` matches nobody, and the sidecar
logs the first token from each such issuer, the two issuers and nothing of the person. A provider
whose subjects differ per client (Entra ID) needs the front door on Canyon's own client. Level `none`, which is everyone off the membership lists, is `302` to
`<identity.issuer>/#/no-access/<app>`; an unsafe method whose `Sec-Fetch-Site` or `Origin` is not the
request's own origin is `403 cross_origin`, as on a single host. Otherwise, on the request it passes
on and after it has read what it verifies, it removes the front door's
own credentials — `Authorization`, every `X-Amzn-Oidc-*` header, and every cookie whose name begins
with the front door's session cookie: `cookie` for choice 1 (`AWSELBAuthSessionCookie` when an `alb`
names none), `_oauth2_proxy` for choice 2 — and every header a client could name a person, an
address or a scheme with to an app that trusts a proxy in front of it (**amended 2026-10-23**, #2451): every
`X-Forwarded-*` and `X-Auth-Request-*`, other gateways' word on the person (**amended 2026-10-27**, #2460) — Google
IAP's `X-Goog-Authenticated-User-*` and `X-Goog-Iap-Jwt-Assertion`, Cloudflare Access's `Cf-Access-*`, Azure App
Service's `X-Ms-Client-Principal*` and `X-Ms-Token-*`, Pomerium's `X-Pomerium-*`, authentik's `X-Authentik-*`,
Tailscale's `Tailscale-User-*`, Vouch Proxy's `X-Vouch-*` and Teleport's `Teleport-Jwt-Assertion` (#2542), and
`Remote-Email`, `Remote-Groups` and `Remote-Name` (Authelia, authentik, Organizr) — `Remote-User`, `X-Remote-User`, `X-Webauth-User`, `X-Real-IP`,
`Client-IP`, `X-Client-IP`, `True-Client-IP`, `Cf-Connecting-IP`, `X-Cluster-Client-IP`, `Forwarded`,
`X-Scheme`, `X-Url-Scheme` and `Front-End-Https`, and every header whose name holds an `_`, which CGI-style stacks
read as the dashed one — each by its name however it is spelled; then it sets `X-Forwarded-For` and
`X-Forwarded-Proto` to the front door's, sets `Authorization: Bearer` to Canyon's token
(design/PERMISSIONS.md; minted with its own key, `aud` the app, `env` the target, the person's app
roles) and proxies to `127.0.0.1:3000`. So no app's code is handed a credential named here that it
could replay to another app through the front door. The key sets and the issuer are reached over the pod's egress, so
they are public addresses.

**The apps database.** Postgres is the provisioner's (contracts/provisioner.md), unchanged: the
built-in driver's `CANYON_APP_DATABASE_URL` names a database server of its own listening on **5433**,
whose admin login is in the server's Secret in `canyon` and in nothing of `canyon-apps`. An app reaches
a database only on TCP 5433 inside `CANYON_KUBE_DB_CIDRS`, so an adapter serving a Kubernetes host
answers URLs there too, and the control plane's own Postgres, on 5432, is out of every app's reach.
Nothing else in those ranges listens on 5433: the deploy keeps that true. A server that speaks TLS
alone (RDS) signs with a CA the common runtimes do not trust: the host names that CA's bundle
(`CANYON_APP_DATABASE_CA_FILE`, the server's), every `/run` then carries it as `database_ca` — PEM
`CERTIFICATE` blocks of lines of at most 64 characters and nothing else, at most 256 KiB, else `400` —
and each app finds it at `/etc/canyon-db/ca.pem`, the file its `DATABASE_URL` names
(contracts/provisioner.md).

**The other calls.** `/running` answers the image in the template of the Deployment `<app>-<target>`
while a pod of that template is available, `404` when there is no such Deployment or no pod of it is available, and `502` when the cluster API did not answer. `POST /running` answers each asked
from one list of the asked apps' Deployments and one of their pods (above): a Deployment whose current
pod's `app` container has restarted is `restarting`, ready or not (*Restarting*); otherwise one with an
available pod runs. `/stop` deletes the Deployment first and,
once its pods are gone, the Service, NetworkPolicy, ConfigMap and Secrets of `<app>-<target>`, by name;
one already gone is a success. `/forget` is `409` while a Deployment `<app>-preview` or `<app>-prod`
exists, and otherwise deletes every image of `<registry>/canyon/<app>`, the cache among them — on ECR,
the repository. `/logs` streams the container `app` of the Deployment's newest pod and never a
sidecar's: the last `tail` lines, then live, until that pod or the client ends; `404` while the
Deployment has no pod.

**`/scan`** is a Job in `canyon-scans`, it and its pod labelled `canyon.scan`, under a NetworkPolicy
selecting those pods that allows nothing in or out. The pod is the container above as Kubernetes spells
it — user and group 65534, a read-only root filesystem, every capability dropped, no privilege
escalation, 2 CPU and 2 Gi, no service-account token, `/tmp` a memory `emptyDir` and `/scan` an
`emptyDir`, processes bounded by the node's `podPidsLimit`. Its init container, a static busybox pinned by
digest (`CANYON_IMAGE_BUSYBOX_STATIC`), waits for its feed: the executor streams one tar over `pods/exec` — the tree, the rules, the
`/scan/<tool>.yaml` it writes — into `/scan`, and the feed copies its busybox to `/scan/busybox`. Then the
tool's container runs. A container's log is its stdout and stderr, merged by the kubelet, and trivy,
opengrep and checkov write progress to stderr, which would make their SARIF `not_sarif`; so the tool's
command is `/scan/busybox sh -c 'exec "$@" 2>/dev/null' sh` followed by its image's entrypoint and the
tool's arguments — Opengrep's, the Docker executor's script above, under `/scan/busybox sh` — its stderr is
discarded, as the Docker executor discards it, and its log is its stdout alone, followed from the container's start — the kubelet rotates a running
container's log past its size limit, 10 Mi by default — into what the executor keeps (above), its exit status unread but for Opengrep's (`rules_error`, above). `killed` is a
container ended by the out-of-memory killer or a signal; the deadline is the Job's
`activeDeadlineSeconds`, and at it the Job goes: `timeout`. The Job is deleted when the scan answers,
and every one labelled `canyon.scan` when the executor starts. **trivy runs as client and server**
(amended 2026-10-04, #1652): its database is held by one `trivy server`, the Deployment `canyon-trivy`
in `canyon-scans` — one replica, labelled `canyon.trivy`, `CANYON_IMAGE_TRIVY`, `trivy server --listen
0.0.0.0:4954 --cache-dir /db` and `--db-repository` from `CANYON_SCANDB_TRIVY` when set, user 65534, a
read-only root filesystem, `/db` and `/tmp` each an `emptyDir`, requests 10m CPU and 128 Mi (measured:
idle, 0% and 76 Mi; ten scans at once, 160 ms of CPU in all; amended 2026-10-05, #1685), limits 1
CPU and 1 Gi — with its Service `canyon-trivy` (TCP 4954) and a NetworkPolicy letting in only pods
labelled `canyon.scan-trivy` on 4954 and letting out only what a refresh reaches (below). The executor
makes them on trivy's first scan and updates the Deployment whenever its image or database source
differs from this executor's, so the server is never another trivy than its clients; its Role in
`canyon-scans` gets and creates Services and gets, creates and updates Deployments. The server downloads
the database when it starts and checks for a newer one every hour, holding scans while it swaps it in
(trivy 0.74.0's own server). Every trivy scan is a client of it — `trivy fs --server
http://canyon-trivy.canyon-scans:4954`, the same offline flags, secret and misconfig scanning in the
client with trivy's embedded checks — on any node, side by side; it mounts no volume, and its pod, also
labelled `canyon.scan-trivy`, reaches DNS (UDP and TCP 53 to the `kube-dns` pods of `kube-system`) and
TCP 4954 to the server's pods, and nothing else. The server's pods take TCP 4954 from those pods and from the
executor's (`app.kubernetes.io/name: canyon-executor` in `canyon`), whose own policy lets it out to them on 4954. While the server has no ready pod a trivy scan answers
busy with `reason:"database"`, and one whose call to the server fails mid-scan (a restart) answers the
same, never a scan that did not finish; a server with no ready pod 15 minutes after the executor made or
updated it, or restarting since, is `db_missing` — a tool that cannot check, not a wait. `db_built_at`
and `db_stale` read the server's own `GET /version`, its `VulnerabilityDB.UpdatedAt`, which trivy 0.74.0 reads
from its `metadata.json` (`pkg/version`); one that names no `VulnerabilityDB` is no database. `pods/exec` streams a
scan its feed and reads nothing (amended 2026-11-09, #2743: it read the timestamp). grype's database is the volume
`canyon-scandb-grype`, a 5 Gi `ReadWriteOnce` claim of the cluster's default class that grype's first
refresh makes, mounted read-only at `/scan/db`; a refresh is a Job labelled `canyon.scandb` mounting it
writable, whose init container `sweep` first removes `/db/grype-db-download*`, what a refresh cut short left,
from it — never the database — on the scans' busybox (`CANYON_IMAGE_BUSYBOX_STATIC`) as 65534, making no network
calls though it shares the pod's egress, and fails the Job before grype runs if it fails, the database as it was
(amended 2026-10-27, #2527); the Job's egress DNS (UDP and TCP 53 to the `kube-dns` pods of `kube-system`), TCP 443 to public
addresses and every TCP port at `CANYON_KUBE_SCANDB_CIDRS` (below), and nothing else. A volume attaches
to one node at a time and grype has no server, so grype's lock serialises its scans: a second grype
scan while one runs answers busy, `503 {error:"busy"}` with no `reason`, and is asked again, never
waiting inside its call (#1643) — it waits its turn, and never fails for waiting (finding.md). Trivy's
Java database is the volume `canyon-scandb-trivy-java`, made, refreshed, mounted and locked exactly as
grype's is — a 5 Gi `ReadWriteOnce` claim (the database is 1.4 GB, its download near 1 GB more), a refresh Job labelled `canyon.scandb` with grype's
egress, its reader a Job — so image scans are serialised by its lock: a second image scan while one runs
answers busy with no `reason`. An image scan is not a client of `canyon-trivy` (it matches nothing): its
pod, labelled `canyon.scan` alone, reaches nothing, and the feed streams the image into a `/scan` sized
from its manifest, its ephemeral storage requested the same. A sbom scan is a trivy client like a tree
scan, its part fed as `/scan/sbom.cdx.json`. A tool
with no database takes no lock: its scans run side by side, each its own Job. Opengrep's image is Canyon's own here,
built by the operator from `executor/opengrep.Dockerfile`, the file the Docker executor builds from, and
named by digest in `CANYON_IMAGE_OPENGREP` (unset: `no_image`), and ESLint's likewise from
`executor/eslint.Dockerfile` in `CANYON_IMAGE_ESLINT`: the executor builds no scan image.

## Images and database sources

Every image an executor runs from a registry, and each scanner database it downloads, has a setting
in the executor's environment, so an operator can take each from a registry mirror or an air-gapped
copy. Each but `CANYON_IMAGE_IDENTITY` is optional, and unset is what the release pins.

| Setting | The image of | Executor | Unset |
|---|---|---|---|
| `CANYON_IMAGE_TRIVY` | trivy's scans, refreshes and database reads; on Kubernetes its server `canyon-trivy` and its clients | both | the release's pin (executor/scan.go) |
| `CANYON_IMAGE_GRYPE` | grype's scans, refreshes and database reads | both | the release's pin (executor/scan.go) |
| `CANYON_IMAGE_CHECKOV` | checkov's scans | both | the release's pin (executor/scan.go) |
| `CANYON_IMAGE_LIFECYCLE` | pack's analyze, restore and export (*How pack runs*) | Docker | the release's pin (executor/executor.go) |
| `CANYON_IMAGE_IDENTITY` | the `canyon-identity` sidecar | Kubernetes | none: Canyon's own image, which the deploy names; the executor does not start without it |
| `CANYON_IMAGE_OAUTH2_PROXY` | choice 2's oauth2-proxy sidecar | Kubernetes | the release's pin (executor/kube/objects.go) |
| `CANYON_IMAGE_OPENGREP` | opengrep's scans | Kubernetes | none: opengrep answers `no_image` (the Docker executor builds its own) |
| `CANYON_IMAGE_ESLINT` | eslint's scans | Kubernetes | none: eslint answers `no_image` (the Docker executor builds its own) |
| `CANYON_IMAGE_BUSYBOX` | `/probe`'s two Jobs on Kubernetes; `/run`'s port check, the listing of the host's addresses an app's floor refuses, and grype's refresh's sweep on Docker | both | the release's pin (executor/kube/probe.go, executor/executor.go) |
| `CANYON_IMAGE_BUSYBOX_STATIC` | a scan's feed, whose statically linked busybox each tool's command runs under, and grype's refresh's sweep | Kubernetes | the release's pin (executor/kube/scan.go) |
| `CANYON_IMAGE_EGRESS_HOLDER` | each build's holder, and each holder an app on an open network runs in, which apply their floors (*A build reaches no private or link-local address*; *An app reaches no private, link-local or host address*); the same setting names Canyon's egress gateway's holder. It may carry a tag beside its digest, as compose's pin does; the digest is what runs | Docker | the release's pin (executor/buildnet.go), which docker-compose.yml's `canyon-egress` pins too (deploy/egress's `TestTheBuildsHolderIsEgress` holds them equal) |
| `CANYON_IMAGE_RUN` | the run image every app builds onto and a `/rebase` rebases onto (*Every app runs on Canyon's run image*): the operator's golden image on the builder's stack | Docker | `canyon-run`, which the executor builds |

An image setting is a reference by digest with its registry named, `<registry>/<repository>@sha256:<hex>`,
so a registry cannot change what runs. It replaces the pin everywhere the pin runs, `/scan`'s `image`
included, and nothing else about how the image runs changes: the value is a copy of the pinned image,
or one that runs as it does — the operator's to ensure, which the executor does not verify. The router's and the builder's images, and on Kubernetes `canyon-run` and `canyon-build`, are
the deploy's (deploy/k8s) — on Kubernetes the operator moves the run image in the ClusterStack, keeping
the ClusterBuilder's name and `spec.tag`, which name kpack's copy of it (`/rebase`); the Docker executor's builder is
`CANYON_BUILDER` or the `canyon-builder` it builds, a setting of its own, and its run image
`CANYON_IMAGE_RUN` or the `canyon-run` it builds (*Every app runs on Canyon's run image*).
`CANYON_RUN_APT_MIRROR`, on Docker, is the Ubuntu archive `canyon-run` is built through: an `http` or
`https` URL — `https` is advised, since nothing bounds how old a replayed archive may be — with no
credential, query or fragment and none of `'`, `"`, `\`, `$`, a backtick or whitespace, which the
Dockerfile's quoting cannot carry. It is handed to `executor/run.Dockerfile`'s package install as its one
deb822 source in place of Ubuntu's own archive and security pocket: it serves `noble`, `noble-updates`
and `noble-security` with `main`, `universe` and `multiverse`, signed by Ubuntu's archive key (the base
image's `/usr/share/keyrings/ubuntu-archive-keyring.gpg`, which apt checks), and on arm64 it mirrors
`ubuntu-ports`. The URL is part of `canyon-run` — its history and its apt source — and so readable by
every app built onto it: it is never a secret. Unset, Ubuntu's own.

| Setting | Where the tool's database downloads from | Reaches the refresh as | Unset |
|---|---|---|---|
| `CANYON_SCANDB_TRIVY` | one OCI repository and tag, `<registry>/<repository>:<tag>` | trivy 0.74.0's `--db-repository <value>`, on Kubernetes the server's | trivy's own: `mirror.gcr.io/aquasec/trivy-db:2`, then `ghcr.io/aquasecurity/trivy-db:2` |
| `CANYON_SCANDB_GRYPE` | an `https` URL, under which grype reads `v6/latest.json` and the archive it lists | grype 0.119.0's `GRYPE_DB_UPDATE_URL`, in the refresh's environment | grype's own: `https://grype.anchore.io/databases` |
| `CANYON_SCANDB_TRIVY_JAVA` | one OCI repository and tag, `<registry>/<repository>:<tag>` | trivy 0.74.0's `--java-db-repository <value>`, in the Java database's refresh | trivy's own: `mirror.gcr.io/aquasec/trivy-java-db:1`, then `ghcr.io/aquasecurity/trivy-java-db:1` |

No source carries a credential: a grype URL with one is refused, since the value is written into
the refresh pod's spec and a failed refresh's output into the executor's log, and an OCI reference has
no place for one.

`CANYON_SCAN_IMAGE_MAX`, on both executors, is the largest image an image scan reads (*`target:
"image"`*): a whole number of bytes, or of `Ki`, `Mi`, `Gi` or `Ti`. Unset, it is `8Gi`.

On Kubernetes a refresh Job reaches public addresses on TCP 443 alone (*On Kubernetes*, `/scan`); a
source outside public TCP 443 is named in `CANYON_KUBE_SCANDB_CIDRS`, comma-separated CIDRs as
`CANYON_KUBE_DB_CIDRS` takes them, where a refresh Job reaches every TCP port. Unset, there are none.
A CIDR that overlaps `169.254.0.0/16` or `fe80::/10` is refused: every port there includes the node's
instance metadata.

**A bad value refuses the start.** An image setting that is a tag, lacks its registry or its digest,
or does not parse; a `CANYON_RUN_APT_MIRROR` that is not an `http` or `https` URL, or carries a
credential, a query, a fragment or a character above; a trivy or trivy Java source that is not one repository and tag; a
`CANYON_SCAN_IMAGE_MAX` that is not a size above zero; a grype source that is not an
`https` URL; a CIDR that does not parse or has host bits set; a `CANYON_KUBE_SIGNIN_DIRECT` entry that is
empty, names a host out of shape or twice, or an address without `/32`, a range, an IPv6 or a link-local
address; or `CANYON_IMAGE_IDENTITY` unset on
Kubernetes — the executor exits before it serves, and its error names the setting. A value is never
corrected or used in part. The tests, in `executor/` for the Docker executor and the three shared
scanners and in `cmd/canyon-executor-kube/` and `executor/kube/` for Kubernetes: an unset setting is
the pin; a digest is the image in every place the pin ran; a tag or a malformed value refuses the
start, naming the setting; each source is in its refresh's arguments or environment, and the CIDRs
in the refresh Jobs' NetworkPolicy.

## The invariants (each gets a test, per AGENTS rule 2)

1. **A `/build` payload contains only the values the user marked `where: build`** (routes.md,
   the secrets rows); a runtime secret never enters a build. The control plane enforces before
   sending; the executor rejects anything else as defense in depth is NOT added — one
   enforcement point, tested.
2. **`/run` env is held for the duration of the call only** — no cache, no reuse across
   calls, no enumeration endpoint, no vault key anywhere in the executor. On Kubernetes the values
   outlive the call in the Secret `canyon-env-<app>-<target>`, for exactly the Deployment's life,
   and the executor reads no Secret back (*On Kubernetes*).
3. The executor has **no access** to org config, the audit store, other projects' sources,
   or the control-plane database.
4. Promote never calls `/build` — it re-`/run`s the **same digest** that served preview,
   with `target: prod` and production's own runtime values ("Production runs the same image as
   preview, with production's own secrets. Nothing is rebuilt."; amended 2026-10-05, #1698: the old
   "gets exactly what preview runs" read as if preview's values went too).
5. `/logs` answers the log of `<app>-<target>` and **never another container's** — on Kubernetes
   the `app` container's, never a sidecar's — and carries no env and no vault value in either
   direction.
6. **`/stop` is idempotent**: stopping `<app>-<target>` when no such container exists is the
   same `204` as stopping a running one, so a retry after a lost answer is safe.
7. **A scan reaches no network, holds no secret and writes nothing but stdout**: no network,
   no env beyond the image's (grype's database reader adds two fixed settings, `GRYPE_DB_CACHE_DIR`
   and `GRYPE_DB_VALIDATE_AGE`, and trivy's image target one, `TMPDIR=/scan/tmp`), a read-only root fs, and nothing it wrote outlives its container.
8. **A scan leaves nothing**: its container and volume are removed when it answers, at its
   deadline, and — for any a crash left — when the executor starts, by label `canyon.scan`, with the
   Docker executor's own staging directories (`canyon-scan-*` in its temporary directory: an image
   scan's `image.tar` among them) (on
   Kubernetes its Job, under a NetworkPolicy allowing nothing in or out).
9. **The tree never steers its scan: a suppression or config file it carries changes nothing** —
   `.trivyignore`, `trivy.yaml`, `.semgrepignore`, `nosemgrep` comments, `.checkov.yaml`,
   `checkov:skip` comments, `.grype.yaml`, `.grype/config.yaml`, an OpenVEX document. The tool reads only `/scan/<tool>.yaml`; the executor's e2e test
   plants each of these over a finding and expects the finding still reported.
10. **The app port is reachable only through the identity sidecar** (Kubernetes): the Service
    targets, and the NetworkPolicy admits from the router's pods alone, 4180 — `canyon-identity` for
    choice 1, oauth2-proxy in front of it for choice 2 — never 3000. The one other port the policy
    admits is the sidecar's 4183, from the executor's pods alone, whose one path asks the app one fixed
    `GET /` and takes nothing from its caller (*What the app answered*; amended 2026-10-05, #1740); the
    executor reaches it by the pod's IP, so the Service names no port but 4180, and its Role gains no
    verb: it reads pods already.
11. **Two apps' containers cannot reach each other**: each app has its own network, shared only
    with the containers `CANYON_EXECUTOR_JOIN` names. From one app's network, the other app's
    container, the server's API and app-postgres's admin login are all unreachable; the server
    reaches both apps. **On Kubernetes**: from an app's pod, another app's pod on any port, the
    server, the executor and every private address but TCP 5433 in `CANYON_KUBE_DB_CIDRS` are
    unreachable; the router reaches every app. The apps database's admin login is held by the
    server alone — an app reaches the port, and never the credential, with its own login only.
12. **A build reaches no private or link-local address** (*A build reaches no private or link-local
    address*; amended 2026-10-25, #2461): from a build's own step, the cloud's metadata address, a
    link-local address that answers, the Azure WireServer, a private address that answers and a public
    address of the host's own do not connect, and a public registry on 443 does; pack fetches nothing a
    `project.toml` names; and a
    build whose floor is not in place never starts. **On Kubernetes** the server builds nothing where
    the front door's check has not seen NetworkPolicy enforced (contracts/routes.md, the front-door row;
    #2507).
13. **An app reaches no private, link-local or host address** (*An app reaches no private, link-local
    or host address*; amended 2026-10-26, #2503): from an app's own namespace on an open network, a
    service on the host's every address through its gateway and through host-gateway, the host's private
    address, a public address on the host's own interface, a link-local address that answers and the
    metadata address do not connect, while a public address elsewhere, a public name's
    lookup, the app's own name and a `CANYON_APPS_ALLOW` entry do; an app whose floor is not in place never
    starts; a holder down under its app, or stopped with it by a daemon's restart, comes back with the
    app, floored, and no app is ever running unfloored in between; the reconcile starts or stops no
    container an image's label names but the app's own holder; and a failed redeploy never names an
    unfloored container back. **On Kubernetes** an app pod's policy refuses every private address already
    (invariant 11); its own node is the operator's host firewall (`POST /probe`, *The node*).

Where each invariant's test lives: 1 and 4 in the control plane, the only caller; 2, 3 and 5–9
in the executor (`executor/`; 6–9 against the real daemon in the e2e test); 11 in `make demo-check`, from app A's
namespace, its holder's: app B's container,
the server's API and app-postgres's admin login unreachable, the server's own addresses on `canyon-state`,
`canyon-internal`, `canyon-provisioning` and `canyon-egress-ctl` not even connecting — Docker's isolation
between its networks, and app A's floor,
which keep an app's connection from arriving there from those networks' gateways, which a demo serves (#2491) —
and the server reaching both apps. 12 in `executor/buildnet_test.go` (the floor's order, the holder, its
fail-closed cases) and the e2e test's `TestEndToEndABuildReachesNoPrivateNorLinkLocalAddress`. 13 in
`executor/appfloor_test.go` (the floor's order, the holder and its fail-closed cases, the host's addresses,
the reconcile, its refusals and Rejoin's call of it, the reap, the restore of an unfloored container), the
e2e test's `TestAppE2EReachesNoAddressOfTheHostsAndComesBackFloored` (the refusals above against stand-ins
that answer an unfloored network first, a real restart of the daemon, a holder started again) and
`make demo-check`: from app A's namespace, a forward to the demo's loopback publish on host-gateway's
address, on its own gateway and on a public address of the host's does not connect (#2503).
On Kubernetes (`executor/kube/`; the `kind` tests run by `.github/workflows/kube.yml` on kind with
Calico): 1 in `TestBuildValuesLiveOnlyAsLongAsTheBuild` — no Build of the app is left once `/build`
answers, image or error — `TestKindOnlyTheExecutorReadsBuilds` — `kubectl auth can-i` `get`, `list` and
`watch` on Builds and on pods in `canyon-builds` as every service account the deploy writes, `no` for all
but `canyon-executor`; 2 and 3 in `TestKindExecutorReadsNoSecret` —
`kubectl auth can-i` as `canyon-executor`: `get`, `list`, `watch`, `patch` and `update` on Secrets in
each of the four namespaces, and `create` on pods in `canyon`, every answer `no`; 5 in `TestLogsReadTheAppContainerAlone`; 7 and 8 in
`TestKindScanReachesNothingAndLeavesNothing`; 10 in `validate.py` (a Service naming any port but 4180,
or a NetworkPolicy admitting anything but 4180 from the router's pods and 4183 from the executor's,
fails), `TestRunWritesTheContractObjects` (the fixture's
`kube_objects`) and `TestKindAppPortOnlyThroughTheSidecar` — from a pod of `canyon` without the
front-door label and from another app's pod, the app pod's 3000, 4180, 4181 and 4183 do not connect,
the executor in the cluster hears what the app answered (its `/run`'s `answered`), from a
router pod the app answers through the sidecar, a pod's first connection included, and `/probe` answers `enforced` there and
`not_enforced` on kind's default network plugin; 11 in `TestKindAppsReachNoOtherAppNorTheControlPlane` —
from app A's pod, app B's pod, the server's and the executor's Services and TCP 5432 in the database
ranges do not connect, TCP 5433 there does, and the admin credential is in no object of
`canyon-apps`; 12 in `TestTheBuildsReachNoPrivateAddress` (deploy/k8s: every NetworkPolicy selecting
pods of `canyon-builds`, together, allows DNS, TCP 80 and 443 outside the eleven ranges, and nothing
else), `deploy/helm/check-rendered.py` (the same of the chart's) and `TestKindBuildsReachNoPrivateNorLinkLocalAddress`
— from a pod of `canyon-builds`, the metadata address, a link-local address the node answers on 80 and
a pod's 80 and 443 do not connect while a pod of `canyon` labelled as nothing reaches each, and a
public 443 does, and `TestTheBuildsRefuseTheOneList` (the builds' policy's list is `private`, an app's).
On Docker, 12 is `TestEndToEndABuildReachesNoPrivateNorLinkLocalAddress` (a build
step probing each address, against the real daemon, the stand-ins on 80 shown to answer from a plain
bridge first, each refused by the floor's REJECT), `TestBuildRunsInsideItsFloor`,
`TestBuildFailsClosedWithoutItsFloor`, `TestTheBuildFloorRejectsWhatNoEntryOpens`,
`TestTheBuildFloorStopsAtARefusedRule`, `TestBuildRefusesARootBuilder`,
`TestBuildAllowRefusesWhatNoBuildMayReach`, `TestBuildRefusesAValuePackWouldRead`,
`TestBuildRefusesAProjectTomlPackWouldFetch`, `TestBuildRefusesAProjectTomlPackReadsOtherwise`,
`TestPackReadsCanyonsDescriptorAlone`, and the sweep tests. The same `kind` tests of 10, 11 and 12
(12 without its link-local stand-in, which needs kind's node) run once against the cluster Canyon is
deployed to
(`make test-kube KUBECONFIG=…`), whose network plugin is not kind's. Reachability is network-only — `canyon-internal`, no token; on Kubernetes the
executor's NetworkPolicy — and that is the single enforcement point; nothing adds a bearer token later
for depth.
