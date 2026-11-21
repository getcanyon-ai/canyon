# Canyon — CLI/Server Architecture

The picture of the system, drawn from the code and kept with it, is the public page
[How Canyon works](../docs/src/content/docs/concepts/how-canyon-works.md): what runs where, the
review, and the way to production. This document holds the reasoning behind it.

**The rule this document exists to enforce: the CLI is a courier, the server is the brain.**
The CLI moves code and renders what the server says. Every decision — what blocks, what passes,
what the review agents look for, what gets built, where it deploys — is made server-side, from
org configuration, because a gate the client runs is a gate the client can skip, patch, or lie
about. One brain serves N couriers — CLI, dashboard, CI job, harness skill — with identical
verdicts, and a new CLI release should almost never be required to change Canyon's behavior.

---

## 1. The split

```
┌─────────────────────────┐          ┌───────────────────────────────────────────┐
│  canyon CLI (thin)      │          │  Canyon Control Plane (the brain)          │
│                         │          │                                           │
│  · upload code (tar)    │  HTTPS   │  · orgs · projects · fleet inventory      │
│  · render progress/     │ ───────► │  · ingest: upload → materialized commit   │
│    findings/URLs        │ ◄─────── │  · REVIEW GATE: scanners + LLM agents,    │
│  · write skill files    │   poll   │    vs the landed commit, per org policy   │
│  · login (device flow)  │          │  · durable gate runs (fail-closed)        │
│                         │          │  · deploy · environments · promote        │
│                         │          │  · secrets vault · audit · event rows     │
│  · --format human|ai|   │          │  · auth: OIDC to the org's own IdP        │
│    json (render only)   │          │                                           │
│                         │          │  ┌─────────────────────────────────────┐  │
│  NO scanners            │          │  │ BUILD EXECUTOR — isolated            │  │
│  NO policy logic        │          │  │ source tree in → image + logs out.  │  │
│  NO build               │          │  │ No vault, no org config, no audit   │  │
│  NO git required        │          │  │ store, no other projects' sources.  │  │
└─────────────────────────┘          │  └─────────────────────────────────────┘  │
                                     └───────────────────────────────────────────┘
```

Enterprise config, model choice, and policy state never leave the server, and the org tunes
behavior — new scanner, stricter policy — in server config, effective on every builder's next
command. Canyon calls the Claude API, or OpenAI's for the AI review alone; a Bedrock or Vertex estate
puts a gateway that speaks the Claude API in front (`CANYON_MODEL_URL`), since Canyon signs no cloud
request and refuses to boot on `CANYON_MODEL_PROVIDER=bedrock` or `vertex` (`cmd/canyon-server/model.go`).

**The build executor is a separate trust domain**, because buildpack builds execute the app's own
tooling (`postinstall`, `setup.py`, Makefiles). Two of its endpoints carry that trust: `POST /build` takes a source tree
plus the values marked `where: build` and returns an image + logs; `POST /run` takes an image ref
plus **exactly the one app's runtime env for the duration of the call — never a vault key, no
enumeration, no cache, no reuse across calls.** `POST /rebase` runs no app code: it moves an image
onto a newer run image under a fresh tag and answers the facts the server compares (design/PATCHING.md
§4.3). No org config, no audit store, no other projects'
sources; the control plane never executes app code in-process. A build reaches DNS and public TCP 80
and 443, never a private or link-local address — the cloud's metadata address among them — whatever the
org's Outbound: on Docker every phase runs in a floored holder's network namespace on `canyon-builds`,
which `CANYON_BUILD_ALLOW` may open to named addresses and ports (never Canyon's own networks), and pack
reads a descriptor the executor writes, never the app's `project.toml`; on Kubernetes under the
`canyon-builds` NetworkPolicy, which on k3s adds the bundled registry (contracts/executor.md, *A build
reaches no private or link-local address*; #2461). A cluster that doesn't enforce NetworkPolicy builds
nothing, whatever `run_by` is: a deploy fails before the executor is called unless the save of the stored
front door found it enforced (contracts/routes.md, the front-door row; #2507).
In the demo compose stack the Docker socket is never mounted into anything that also holds the
vault, and **each app's containers live on the app's own network** (`canyon-app-<app>`, a /24 the
executor cuts from `CANYON_APPS_POOL` and shares with the server, `app-postgres` and, while the app's
calls go through Canyon, the egress gateway `canyon-egress` alone — `CANYON_EXECUTOR_EGRESS_JOIN`,
design/EGRESS.md §2.2): two apps
never reach each other, and an app reaches nothing on the control-plane network — not Postgres, and
not the API: the server is joined to each app's network to proxy inward, and its listener binds every
interface, so it refuses at accept every address of the pool (`CANYON_APPS_SUBNET`) but each /24's
gateway, where host traffic arrives from (`cmd/canyon-server/appnet.go`), before any HTTP is read. Of the
server an app reaches one thing: a second listener (`CANYON_APPS_JWKS_ADDR`), the identity key set. The
server's admin login reaches `app-postgres` over `canyon-provisioning`, outside the pool, from the one address
pg_hba admits it from (contracts/executor.md, invariant 11; `make demo-check` proves it). **No app reaches
the host** (#2503): on an open network, while its calls go straight out, each app container runs in the
network namespace of a floored holder of its own (`canyon-appfloor-<nonce>`, on the app's network under the
app's name), which refuses its network's gateway, every address of the host's own, link-local, the metadata addresses, Canyon's other networks
and every private range but what `CANYON_APPS_ALLOW` opens. On an internal network the host has no address,
and the egress gateway's floor refuses Canyon's networks, loopback, `0.0.0.0/8`, every private, shared,
link-local and reserved range, every address of the host's own (egress-host's listing, #2524) and every public
port but 80 and 443. So a connection that loops through the host's own forward to the demo's loopback publish —
`host.docker.internal` on Docker Desktop, OrbStack and Colima — never leaves an app a release with the floor
deployed, whatever the sign-in mode (contracts/executor.md, *An app reaches no private, link-local or host
address*). An app deployed before that runs unfloored until its next deploy: redeploy every app once after
upgrading; the executor logs each one until then.
The same compose stack serves a team on one Docker server (decided 2026-10-23, #2458): `deploy/docker/team.yml`,
which the operator's `.env` adds, signs people in through the org's identity provider (`oidc`) at an `https`
public URL, on the operator's own Postgres passwords and egress token. Caddy, a service there, is the one way
in: it ends TLS on the host's 80 and 443, reaches the server on `canyon-edge` (10.214.4.0/24, the two alone),
from the one address `CANYON_TRUSTED_PROXIES` names, and gets a certificate only for a name `GET /tls/ask`
says Canyon serves (contracts/routes.md); the server publishes no port. `e2e/oidc` runs it, and the guide is
docs/src/content/docs/self-host/docker.md. There the executor's Docker socket is root on a host other people
reach, so the guide makes the host Canyon's alone: a container escape, or a takeover of the executor or the
server, is the host and every app on it. A build runs the app's own scripts on Docker's default bridge with
open egress, whatever the app's outbound mode (#2461), so the guide also forbids the VM a cloud identity.

**An app's database is never the control plane's Postgres** (*Databases* below);
`GET /api/v1/component-types` answers a type `available` only on a host with a driver for it
(`contracts/routes.md`).

### On Kubernetes (decided 2026-09-26)

The same server, run with `CANYON_RUNTIME=kubernetes`, which the front-door route reads
(contracts/routes.md), and a second executor binary (`cmd/canyon-executor-kube`) serving the same
calls (contracts/executor.md, *On Kubernetes*). Four namespaces: `canyon` — the server (one replica, no cluster credential but a projected token that reads the cluster's service-account key set, design/EGRESS.md §3.2),
the executor, Canyon's egress gateway (Envoy, design/EGRESS.md), and the router every app address reaches through one wildcard Ingress; `canyon-apps` — per app and target a Deployment, Service, NetworkPolicy, access ConfigMap and
env Secret; `canyon-builds` — kpack Builds; `canyon-scans` — scan Jobs that reach nothing.

- **The front door is one host setting**, choice 1 or 2; choice 3, Canyon's own proxy, is the single
  host's alone. Choice 1: the org's gateway signs people in and `canyon-identity`, a sidecar in the
  app's pod, verifies what it says — a standard OIDC ID token against the gateway's key set, or the
  AWS ALB's `x-amzn-oidc-data` signed by that ALB. Choice 2: Canyon's oauth2-proxy in the pod signs
  them in against the org's identity provider and the sidecar verifies the token it passes on. Either
  way the sidecar checks the app's access file and hands the app Canyon's identity token
  (`design/PERMISSIONS.md`).
- **The app's port answers the sidecar alone.** The Service targets the sidecar, and the NetworkPolicy
  admits it from the router's pods and nothing else; an app's pod reaches DNS, the apps database and
  public 80/443 — under via `canyon`, Canyon's egress gateway, Envoy on 3128, instead of any public
  address, and the internal identity provider's addresses on 443 when the operator names them
  (`CANYON_KUBE_SIGNIN_DIRECT`, #2316): the floor (design/EGRESS.md §2.2) — never another app, the control
  plane or instance metadata. A cluster that ignores
  NetworkPolicy makes all of that a document — an EKS cluster whose vpc-cni add-on has
  `enableNetworkPolicy` off does, and in its default mode a new pod is open until its policy arrives,
  so it runs in strict mode — and every front-door save runs the executor's probe first and refuses on
  an unenforced cluster, but for one door: under hand-off, with a platform webhook saved, a choice 1 the
  probe finds unenforced or uncovered is stored *not probed* (design/HAND-OFF.md §1), and Canyon runs
  no app behind a door that has not passed the probe — every `/run` refuses one (#2438).
- **Addresses** are `<app>.<env>.<domain>` or `<app>-<env>.<domain>`, the admin's choice, `domain`
  Canyon's public host by default; the router serves both forms, and the probe checks that the
  certificate covers the chosen one.
- **Postgres** goes through the same drivers: the built-in one points at a database server of its own
  listening on 5433, the only private port an app may reach, so the control plane's Postgres on 5432
  is out of reach, and its admin login stays with the server.
- **The executor writes Secrets and reads none** (no `get`, `list`, `watch`, `patch` or `update`); an
  app's values live in its Secret as long as its Deployment, build values literally in the kpack Build and,
  as kpack copies them, its build pod — kpack refuses a Build that reads a Secret — which only the
  executor reads and which go when the build answers, and the app's own build scripts never hold a registry credential.
- Not in v1 on Kubernetes: choice 3, per-app namespaces, more than one server replica, disruption budgets. Its runs
  no longer hold the server to one — servers on one database each drive their own (*Durable gate state*)
  — but its volume does, the key every secret is sealed under and the package store, one `ReadWriteOnce`
  mount, and its rate limits are each process's own, so a second replica would double them.

## 2. Code transport: upload-primary

**The CLI uploads a directory; the server materializes an immutable commit.**

```
POST /projects/:id/packages   (size-capped tar stream)
  → untar to temp dir → temp index → commit-tree → update-ref
```

This preserves what the gate needs — landed, immutable, re-runnable, auditable, diffable between
packages (`build a3f9c21 → 7b1e4d8` still renders) — and deletes push credentials, a git endpoint
on the server, and any client-side git dependency. It dedupes by tree hash. A Lovable/Bolt/v0
export unzipped to a directory is one `canyon deploy` away from the fleet; a monorepo subdirectory
is just a directory; "Requirements: Docker" stays literally true.

- **Exclusions are Canyon's list, published by the server, versioned with the policy snapshot**
  (`.env*` but `.env.example`, `node_modules`, `.git`, the root's `dist`, `build` and `.next`, virtualenvs —
  contracts/snapshot.md), applied by the CLI at package
  time and re-enforced at ingest. `.gitignore` is **not a gate input** — a repo-controlled,
  unaudited exclusion channel. The trust string gains a destination:
  `✓ Package  241 files · 1.4 MB · .env and node_modules excluded → localhost:7777`
- **Canyon never ingests history** — a single tree per package, so a secret committed to the app's
  git history three weeks ago never reaches Canyon. Packages have a stated retention window — 30
  days after the newer package that superseded them, the running and the promoted package never, nor
  production's last five versions (the status read's `earlier[]`, design/OPERATIONS.md §1.4) —
  because they can contain leaked credentials. Retention drops a package's tree and keeps its row, so a
  host still knows every package id it made, since a package id is its tree: guard (b) is answered
  `200` for one it removed and `404 unknown_package` only for one it never made (contracts/routes.md, design/OPERATIONS.md §2.3). The window is stated here and enforced by nothing
  yet; the worker that deletes on that schedule, and the setting that names the days, are day two
  and land together (canyonv2-3nq.4 closed on this sentence, 2026-09-22).
- **The GitHub App courier ships in v1** (decided 2026-08-29): per-instance App via the manifest
  flow, webhook → tarball fetch → the **same ingest** (`kind: repo`), verdicts as Checks
  annotations, required-check enforcement, scoped to PRs + the default branch. The **demo path
  stays upload** — webhooks cannot reach loopback, so the App is exercised against deployed
  instances. Same filter, same single tree: the repo is a source, never a store.
- If a verified need appears (400 MB repos in a fix loop), add content-addressed chunk skipping
  *to upload* then.

## 3. The CLI surface (unchanged verbs, thinner insides)

| Command | Client does | Server does |
|---|---|---|
| `deploy` | package (server-published exclusions) → upload, poll, render | ingest → gate → build → deploy preview |
| `check` | same package path, upload, poll, render | same gate run, stops before build |
| `deploy --env prod` | POST intent bound to a gate-run id, render receipt | policy evaluation, promote the reviewed image |
| `status` / `fleet` | GET, render | inventory, gate state, environments |
| `logs` | stream, render | tail from runtime |
| `secret set` | prompt hidden, POST | vault; `--from-gate` adoption |
| `skill` | write the shipped skill file per harness | — (v1; see §4) |
| `login` | device authorization grant (RFC 8628) against the host's own device endpoints | mints the session; the person confirms the code in a browser |

**Authentication is mandatory and on by default** (`design/AUTH.md`). Canyon speaks OIDC directly
to the customer's identity provider, with no broker: one host serves one org, so Authorization Code
plus PKCE for the dashboard and Canyon's own device flow for the CLI are the whole protocol
surface. A request under `/api/v1` without a valid session is `401`, and nothing is minted for it.
`CANYON_AUTH=off` turns that off for a laptop and requires a loopback bind **and** a loopback
public URL, so a reverse proxy cannot put it on the internet;
`CANYON_AUTH=demo` is the shipped compose demo and the only configuration that mints a
session for any caller on this machine: off and demo close every other connection at accept
(design/AUTH.md, #2449). The honesty banner is **derived** from the listener + peer address — behind
the compose demo's port mapping, from the address the host publishes it on — and is shown only in
those two modes; an authenticated host shows the signed-in person instead. **A
person's session is that person**: whatever runs under it, a terminal or an agent in a harness,
acts and is recorded as them, so `prod.human-approval` names somebody. One org preference,
*Production is approved in the dashboard* (`design/AUTH.md` §4.5, decided 2026-10-07, #1820), is for orgs
that want that act in the dashboard: on, the CLI files a production approval and prints its link,
and a person signed in to the dashboard approves it. **Who may sign in is the union of two lists**, the email
domains that may sign in and the individual addresses that may, with at least one non-empty, so a
team with no domain of its own is a normal customer rather than an edge case; membership is checked
at the callback and again on every request, so a removal takes effect at once. Those values and the
admin list are environment in M2b and move into the product in M2c (`design/ONBOARDING.md`), where
a setup state and four screens replace editing a values file; infrastructure stays in the
environment either way, because an operator sets it and a client secret must not live in the
database.

`owner` is **server state**, the app's grants at `owner` (design/PERMISSIONS.md), so accountability
survives someone leaving: the creator is written as owner when the app is created, and nobody becomes
one by deploying; `Claim this app` adds the signed-in person under a display name — anyone's to click
on an app with no owner, no secret and nothing deployed, otherwise its owners' and admins' — and on a
demo host, where there is no address, the grant is held under the subject `anonymous`. Every stored actor is the
typed triple `{kind: person|agent|canyon, subject, display}`, which is why identity landed with no
backfill: the subject became the issuer's `sub` claim and no shape changed.
`canyon.toml` names the app and the `[resources]` it declares, never an owner — accountability is not a text edit in the repo under review.

**Every request runs the pipeline** `recovery → structured logging → session (OIDC; with sign-in off,
`CANYON_AUTH` `off` or `demo`, a caller with no session is the anonymous admin, and no session row or
cookie is made) → handler` (no CORS stage: the dashboard is served by the same binary on the same
origin, and in development the Vite proxy keeps `/api` same-origin), and the audit row is written **inside the
handler's transaction** (synchronous, fail-closed, three-way actor: the state change and its
audit row commit together or neither does; **a row for a state change that took place**, naming
what the action produced; reads, the once-a-second polls included, leave no row and raise no
`audit.appended`). **Audit is not logging** (decided 2026-09-11): the audit trail records what
took place and who did it; a refused request (a 404, a 409, a 429), a validation error and a 500
are the logging stage's business and leave no audit row — but for a `403` refusal and a refused
browser sign-in, which the trail keeps (contracts/routes.md, the audit row; amended 2026-10-05). Rate limits are enforced per project and
per actor together, which identity makes possible: an authenticated host mints a session only at
sign-in, so the per-request session row that made a per-actor limit meaningless is gone. Outside
the session, audit, and rate-limit stages (recovery and logging still run), the **Open** route
group: `GET /health`, `GET /ready` (fail closed — a host whose Postgres or durable-run store is
unreachable is not ready; `waiting_on` is a closed set, never error text; the model backend is
never a readiness input), the dashboard bundle, the CLI download, and the auth
group: `GET /auth/config`, `GET /auth/start`, `GET /auth/callback`, `POST /auth/device`,
`POST /auth/device/token` (`design/AUTH.md`), and `GET /auth/app`, which signs a browser into an app (design/PERMISSIONS.md). No other API route is reachable without a session — except `POST /webhooks/github`,
authenticated by its signature (M8); the host-routed `<app>.preview.<host>` and
`<app>.prod.<host>` surfaces proxy to the app (M4; `contracts/routes.md`, App surfaces).
Rendering (`--format`, harness detection, glyphs, spinner discipline) is the CLI's whole craft:
presentation of server-sent structure, never computation.

## 3b. The design-system capability (v1, decided 2026-08-29)

**A registry of design systems, as rows** — the component menu applied to design:
`{name, namespace, source_type, registry_url, tokens_url?}`, org default + per-project override,
resolved in the same order as component backends. **Ships pre-registered: stock shadcn + Canyon's
own** (the DTCG tokens/components published in shadcn-registry format); **custom org systems are
added as rows.** Canyon never hosts *org* design assets and never holds the org's registry
token — the builder's environment expands the credential. Canyon's **own** shipped registry is
the one exception: its assets live in-repo (`designsystem/canyon/`) and its serving route lands
with M9.1 as a routes-contract entry. The skill section renders the *active* system's
instructions. No MCP tool carries the design system: the skill and the session hook hand the agent
the active system (the read-only design-tools endpoint planned here was never built; Canyon's MCP
server, §3c, carries the deploy path). One Advisory gate rule: `design.org-system`.

## 3c. Canyon over MCP (built 2026-11, design/specs/mcp.md)

**One set of tools, two ways in.** Each tool is the CLI verb's own handler, run against `/api/v1`
as the person, with that route's level, refusals and audit rows; MCP adds no permission of its own.

- **On a laptop: `canyon mcp`**, a stdio server inside the CLI, signed in as the CLI is
  (`canyon login`). `canyon skill install` writes it into each agent beside the skill and the
  session hook. It packages the working directory, so it deploys unpushed code.
- **In the cloud: `POST /mcp`** on the server, streamable HTTP and stateless, so any replica
  answers. It is an OAuth 2.1 protected resource: Canyon is its own authorization server
  (design/AUTH.md §4.7), a person allows a client once in the browser with scopes `read` or `act`,
  and the token's session is a `flow: oauth` row the person disconnects under Settings › People &
  sign-in. With sign-in off it serves nothing. It has no local directory, so it reviews and deploys
  a pushed ref of a linked repository (`canyon_check_ref`, `canyon_deploy_ref`), packaged on request
  only when the caller's own GitHub account reads it.
- **One server per agent.** An org pushes the URL to cloud agents only; laptops keep `canyon mcp`.
- **No tool takes a secret's value.** A person sets it in the dashboard or with `canyon secret set`.

## 4. Skills: shipped in v1, org-authored later — with guardrails

**v1: the skill file ships with the binary.** `canyon skill install` writes it per harness
(`~/.claude/skills/canyon/SKILL.md`, `.cursor/rules/canyon.mdc`, `AGENTS.md` append),
marker-delimited, idempotent, and registers `canyon manifest` as the session hook beside the
first two (`~/.claude/settings.json`, `.cursor/hooks.json`). `--print` shows exactly what will be written, auditable against the
signed release. `canyon manifest` hands an agent only what is already data, read live at session
start and written to no file: the host URL, the org's stacks and design system as typed values
(`GET /skill/manifest`), the component types, and the org's active policy list. Under each Canyon
rule that blocks it prints one line on how to pass it, Canyon's own words, which the server holds
beside the rules and hands with them, so the dashboard shows an admin the same line (amended
2026-11-03, #2622). The one org-written text it hands is a custom rule's, for a
custom rule that blocks alone: the text Canyon's review already judges every app against, under the
heading `Your org's own rules (written by its admins):`. It is the exception to the day-2 rule below,
and meets its guardrails but one: only admins write it, every version is audited with its diff, it is
versioned and size-bounded (2,000 characters a rule, 20 rules), and it never holds a secret. The one
it cannot meet is install's diff, because nothing is installed: the text is read live and written to
no file.

**Day 2: org-authored instruction sections** ("all internal apps use `@meridian/ui`") are the real
product direction, but writing remote text into the config of an agent with shell access is an
instruction-injection channel *by design*. It ships only with all of:

- a named role that may edit the org section, with an audit event **including the diff**
- the org text visually delimited in the written file (`## From your platform team — org-authored`)
- `canyon skill install` **showing the diff and asking** — never auto-applying someone else's prompt to
  a builder's agent
- versioned content, size-bounded

## 5. One deploy, end to end

```
canyon deploy
  │ package per server-published exclusions → POST tar    [CLI]
  ▼
  server: materialize commit ──► GATE (org policy snapshot)
            gate run row written `pending` BEFORE work starts
            scanners (server-side, versioned)
            LLM review (host model config; may add findings,
                        never remove scanner findings)
            custom rules (the org's own, one more AI call)
            verdict: findings[] — SARIF results in Canyon's profile (seam 1)
  ▼                                       │ poll: row-by-row progress
  blocked? ──► CLI renders findings        ▼
  clean?   ──► build (isolated executor) ──► preview ──► URL
                                                │
  promote (human, binds THAT gate-run id) ──► the same reviewed image to prod
```

Every arrow emits an audit event with the three-way actor (person / agent / Canyon). The same
timeline rows the CLI polls are what the dashboard renders.

### Findings are SARIF (decided 2026-09-07)

A finding is a SARIF 2.1.0 `result` in a strict profile: plain SARIF the tools understand plus
a required `properties.canyon` bag (severity, blocks, title, problem, fix, policy_name,
secret_name?) that Canyon requires and tools ignore. One structure in and out — external
scanners' SARIF is accepted after severity normalisation (`canyonv2-e9d`), and the gate's own
output is a SARIF log any CI can upload to GitHub code scanning unchanged; the GitHub App reports
through Checks and uploads nothing to code scanning. The raw per-producer log is kept verbatim in
the run ledger; findings are derived from it. `contracts/finding.md` is canonical.

### Custom rules (built 2026-10-02)

An admin writes a rule in plain words — "flag code that writes a customer's email unencrypted" —
and Canyon checks every app against it on every review. Each rule is a policy, `custom.<slug>`,
on the same Off / Advisory / Blocking dial as the built-in ones: it installs Off, and its findings are
`high` whatever the model answers, so Blocking blocks. **Checked by code where the facts decide it**
(amended 2026-11-04, #2637, #2639): when a version is saved, the host's model is asked once whether the
facts gate/custom reads of an app — its languages, files, locked dependencies, `canyon.toml`'s resources
and roles, its secrets by name — decide the rule, and answers a CEL expression over them if they do.
Canyon keeps it only when it compiles, returns a bool, fits the facts' bounds and its cost estimate,
uses no function whose cost CEL cannot bound, and, run once on facts at their bounds, finishes in a
second within its estimate and allocation budget; the version is then checked by code: the scanners'
run evaluates it on every review, with no model call, and a missing fact or an evaluation it cannot
finish reads "could not check", a finding under the rule's own id that blocks wherever the rule does.
Anything else is checked by AI. Fixed per version, so a review never translates; an admin can switch a
rule to the AI. The measure, 100 rules over 17 fixture apps, three translations each, is
`internal/tools/ruleeval`. One `custom-rules` agent reviews every rule checked by AI that is on in one
call per review, after `canyon-review`, shown the same redacted files, and is never sent a rule
checked by code. Admins write, every member reads; each save is a new version with an
audit row carrying the before and after; 20 rules of 2,000 characters on a host. The rule's text never
goes into a builder's agent's config — that stays §4's day 2 — but it does reach builders' agents
indirectly, through the fix prose the model writes, and that is accepted because an admin can
already do everything on every app (`design/PERMISSIONS.md`). `contracts/snapshot.md` (*Custom rules*) is canonical.

### Scanner data: where it lives, how old it is, who refreshes it (design note, 2026-11-09, #2743)

A tool whose vulnerability database is more than 5 days old doesn't run: the review reads `could not check:
<tool>: its vulnerability database is over 5 days old` (finding.md), which blocks until a person approves it.
Nothing today warns before that happens. This note says what data each tool reads, how old it can get, and how
Canyon refreshes it.

**Data that ages.** Three databases, downloaded from their publishers and refreshed by the executor
(contracts/executor.md, `POST /scan`):

| Data | Docker | Kubernetes | Age read from |
|---|---|---|---|
| trivy's vulnerability database | volume `canyon-scandb-trivy` | trivy's own server, Deployment `canyon-trivy` in `canyon-scans`, which checks for a newer database every hour; the database is in the pod's `emptyDir`, so no other pod can mount it | `db/metadata.json`'s `UpdatedAt`: on Docker a reader container with the volume read-only; on Kubernetes the server's own `GET /version`, whose `VulnerabilityDB.UpdatedAt` trivy 0.74.0 reads from that file (`pkg/version`) |
| trivy's Java database (image scans) | volume `canyon-scandb-trivy-java` | its own claim, refreshed by a Job as grype's is | `java-db/metadata.json`, read the same way |
| grype's database | volume `canyon-scandb-grype` | claim `canyon-scandb-grype`, 5 Gi `ReadWriteOnce` | `grype db status -o json` as the scan's user, the volume read-only |

The executor refreshes each one when it starts, every 6 hours after that (`dbEvery`), and when a scan finds no
database at all. A refresh holds the tool's lock alone, so no scan reads a half-written database. Each refresh
has a 10-minute deadline and leaves the old database in place when it fails. A database is stale once it is more
than 5 days old (`dbMaxAge`), so it goes stale only after roughly 20 refreshes in a row have failed: the
publisher was unreachable, egress blocked it (`CANYON_KUBE_SCANDB_CIDRS` on Kubernetes), or the disk was full.
Each failure is logged by the executor. No person sees it until reviews fail.

**Data that doesn't age.** It changes only when Canyon is upgraded, so it has no age to report:
- opengrep's rules (GitLab's sast-rules, vendored as `gate/opengrep/sast-rules.tar` and embedded in the server);
- gitleaks' rules (in-process in the server, `gate/gitleaks.go`);
- eslint's and checkov's rules, carried in their pinned images.

An org's own opengrep rules repo is the one exception: it is fetched on each review, so a failed fetch is
already that review's `could not check` (finding.md).

**Reading the age.** Every trivy and grype scan already returns `db_built_at`, and stored reports keep it
(migration 0060). That only describes the last scan, though, and a quiet host has none to read. A small read
on the executor would answer each database's built time on demand, using the readers above. It reads only
(no network, the volumes read-only):

`GET /scandb` → `{databases: [{tool, built_at, stale_at}]}`, the age as the executor last read it (after each
refresh, failed or not, and at each scan's check), so the call never waits on a refresh's lock

`built_at` is absent when there is no database yet. `stale_at` is `built_at` plus 5 days, so the server
compares no clocks of its own. An executor from before this call answers `404`, and the server shows nothing.

On Kubernetes this read uses no `pods/exec`, which is too much power for reading a timestamp. trivy's server
answers `GET /version` with the database's metadata, so the executor asks it over HTTP. That needs one
NetworkPolicy ingress rule, from the executor's pod to the server's port 4954, and no new RBAC. The two claims
are read by a reader Job, as today. Every trivy scan's `db_built_at` moves to `/version` too, so there is one way to read the age, and
`pods/exec`, which the executor holds to stream a scan its files, reads nothing. Keeping the last `db_built_at` a
scan returned was ruled out, because a quiet host has no recent scan.

**Refreshing on demand.** This is safe on both runtimes because it adds no new code path: it runs the same
refresh the 6-hour schedule runs.
- **Who:** an admin. That is the level that owns the host's settings, and a refresh changes what every app's
  review reads (design/PERMISSIONS.md, Settings). Below admin it is `403`. Each refresh writes an audit row
  that names the person.
- **What runs:**
  - Docker: the tool's pinned image, the same container `refreshDB` runs (read-only root, no capabilities,
    2 GB memory, its 10-minute deadline).
  - Kubernetes: grype's refresh Job, and for trivy a rollout restart of `canyon-trivy`, which downloads when it
    starts.
  - The call answers `202` and runs in the background. A tool whose refresh is already running answers `409`.
- **How it fails closed:**
  - A refresh never makes an old database look newer. The age is always read back from the database itself,
    never from the time a refresh finished.
  - A failed refresh leaves the old database as it was, and `db_stale` still blocks once it is 5 days old.
  - A refresh cut short leaves partial files, which the next refresh sweeps away first (grype's
    `grype-db-download*`). If trivy's file is cut short, it reads as no database: `db_missing`, which also
    blocks.
  - While a refresh runs, scans of that tool wait busy, as they do during the scheduled one.
  - An approval of a `could not check` finding stays as it was. A refresh changes no verdict already given.
- **A refresh swaps the database in whole.** trivy 0.74.0's `--download-db-only` downloads to a temp directory,
  then unpacks straight into the cache directory with no rename (`pkg/oci/artifact.go`). A refresh cut short
  could leave the old metadata beside a half-written database. So a refresh, the scheduled one included, writes
  into a fresh cache directory on the same volume. Only once trivy has finished and the database opens does it
  rename that directory over the one scans read. A refresh cut short leaves the old database untouched, and the
  next refresh removes the partial directory first, as grype's `grype-db-download*` is swept today. This is the
  refresh PR's first piece, before any button runs a refresh.

**Smallest useful piece.** No refresh yet, just the warning:
1. The executor's `GET /scandb`, tested by `TestScannerDataAge`.
2. The server reads it for admins.
3. The dashboard's Settings, and `canyon status` for an admin, name a database that will go stale within a day:
   `trivy's vulnerability data goes stale <when>`. That gives a day to fix the egress or the disk before
   reviews fail.

The admin refresh builds on the same read once this is in.

### Durable gate state (fail-closed, restart-safe)

**Substrate: DBOS Transact** (decided 2026-08-29) — an MIT-licensed durable-execution library
embedded in the Go binary, journaling into our existing Postgres. Zero new containers; gate runs
are journaled workflows that **resume** after a crash; queues give per-project serialization and
per-actor rate limits; cron covers scheduled re-review later. **Decided 2026-09-10: the run
driver is DBOS Transact for Go v1.3.0**, embedded, journaling into the same Postgres under its
own schema `dbos`, an executor id of its own per server process (amended 2026-10-10, #1715),
and an application version that names one set of workflows (amended 2026-10-15, #2271): it stays the same
across a redeploy, so a redeploy never orphans a queued run, and a release that adds, removes or renames a
workflow bumps it, so a server still on the release before never claims a workflow it has no handler for.
DBOS owns execution machinery; the rows below remain the
governance record that promote FKs and audit reads.

Platform patches (design/PATCHING.md §4) ride the same machinery: a DBOS scheduled workflow,
`patch.rescan`, ticks every `CANYON_PATCH_INTERVAL` on a global queue of one, re-matches the platform
layers, then plans — only while the operator set `CANYON_PATCHING` `on` (beta), sign-in is on and the org's `patching`
reads `ask` — and asks production again for each patch whose watch passed, a production approval of kind `patch`
(#2102); each patch run is a gate run of kind `patch` on its own queue, `patches`, at most
`CANYON_PATCH_CONCURRENCY` at once, started only while no other run of the project is queued or running,
and ended by any person's start before its `/run` (#2096). Once its `/run` is recorded its gate run's workflow
ends, freeing its slot, and its watch runs on as `patch.watch`, a workflow of its own on the runs' queue, `gate`, which has no
limit (#2567). The image scan after a deploy, `image.sbom`,
has a queue of one of its own, `image-sbom`, so it never waits on a patch run.

- A missing or non-clean gate-run row blocks promote; a server restart can never lose or pass a
  run.
- DBOS owns ownership through its executor id, and each server process has one of its own, so a
  server starting beside another — a rolling update, a second replica — never runs what the other is
  driving (#1715). Each server keeps a row in `durable_executors` fresh every 5 seconds from before
  it launches, and deletes it once it has stopped. A live server recovers a stopped one's runs: the
  journaled workflows still `PENDING` under an executor id whose row is gone, or 30 seconds stale
  while the live server's own row stayed fresh all along (so a database outage that stalls every
  server condemns none), go back on their queues — DBOS's own recovery for those ids, its
  `ReenqueueForRecovery`, which v1.3.0 exports only through its admin server and its Conductor — and
  whichever server dequeues each resumes it from its last checkpointed step. A server stopped
  cleanly hands its runs over at once; a killed one about 30 seconds after the later of its last write and the live server's first. A server whose row was taken
  for dead writes it again and says so in its log. Terminal failure is `failed` with
  `failure_reason ∈ {interrupted, error, model, build, detect, start}` (`build`: the package did not build, and its own lines say what failed — Canyon names no culprit; `detect`: no buildpack on the host detected it, so nothing was compiled — the host does not build that kind of app; `start`: it built and the app did not stay up or answer on port 3000 — the user's fix). One-active-run-per-project is a partial unique
  index, not a mutex.
- **Versions hand work over, they never share it** (#2271). A server dequeues only work stamped with
  its own application version, and DBOS stamps every enqueue with the enqueuer's and every schedule tick
  with the latest. A version names one set of workflows (`workflowSets` in `runs/durable.go`, a recorded
  set never edited), and `runs.Launch` refuses a server whose registered workflows are not its version's
  set, so canyon-server fails closed at boot. The set promises names only: a release that changes a
  workflow's input or steps so a run the release before journaled would not resume bumps the version too.
  A stopped server's runs go back on their queues under the version they had. Each server records its
  version in `durable_executors`; a clean stop marks the row `stopped_at` instead of deleting it, so its
  runs move at once while its version counts as running until the row is stale (30 seconds) and goes.
  Every 5 seconds a server takes over the queued workflows it has a handler for among those stamped with
  a version no row names — a release rolled forward or back. A version still running drains its own, so
  two never pass a workflow back and forth, and a release rolled back never takes a newer one's work the
  moment it stops; a workflow no running server has a handler for stays queued, holding no slot of its
  queue, until one starts. Three consequences: after a rollback the older version is never DBOS's
  latest again, so its `RegisterQueue` no longer rewrites a queue's row (a changed capacity takes effect
  on the next newer release) and each schedule tick, stamped with the newer version, waits a beat for the
  hand-over; a recovered run counts a recovery attempt at each claim, so one passed through several
  retiring servers is dead-lettered at its fifth (`WithMaxRecoveryAttempts(3)`), and the next boot's
  reconcile ends it; and a resumed run whose steps no longer match its journal fails at DBOS's step check
  (`UnexpectedStep`), so it ends failed, never passed. v0.1.12 and v0.1.13 run `canyon-v1` without the
  hand-over, which no release before v0.2.0 has (#2285 merged after v0.1.13 was tagged). v0.2.0 brings it
  and also bumps the version, so it is its own rollback floor: v0.1.13 never claims the work it stamped,
  and v0.2.0 rolls back to no earlier release. A later release that bumps the version rolls back only to
  v0.2.0 or a later one — its release notes say so. Neither v0.1.12 nor v0.1.13 claims a workflow v0.2.0
  stamped, so an upgrade from either to v0.2.0 leaves no workflow with a server that cannot run it.
- **Promote binds to a gate-run id**, not "the app's latest gate" — a re-review landing
  mid-promote can't flip the answer under the operator's hand. `<version> · passed review` is a
  claim about one specific run, and so is the data model.
- **`--from-gate` values are encrypted at rest and survive restarts**; the 15-minute window is a
  timestamp, not a process lifetime. On expiry/miss, the failure names the recovery:
  `canyon secret set NAME` interactively. There is no sweeper: a candidate expires on read (the
  adopt query carries `expires_at > now()`), is deleted on adopt so it cannot be adopted twice, and
  an expired row is inert until the next adopt of anything clears it. Restart-safe because the
  window is a timestamp; nothing replays. Both live in the adopt route (canyonv2-f0e), written
  when that route is, since nothing writes a candidate today.
- A newer run supersedes a queued (`pending`) one; while a run is `running` the request answers
  `409 run_in_progress` (seam 3); `queue_position` is unused in v1. Parallel `Review all` is
  bounded.
- **The LLM verdict cache keys on the tree hash** (+ policy snapshot + model + agent); commit hashes
  include timestamps and are not stable for identical content. `check` → `deploy` on an unchanged
  tree replays the cached verdict.
- **A rule's state is composed per request from the org's policy edits** (`policy_edits`, one
  appended row per state a `PUT /policies/:rule` changes, never rewritten): the effective state is the newest
  edit whose `effective_at` has passed, the canonical default otherwise, laid over the boot
  snapshot. Per request and never cached because `grace_days` makes the answer a function of the
  clock — a cache would serve the old state past the instant a grace elapsed — and the cost is one
  indexed read of at most one row per rule and one hash. Newest by when it was set: an edit
  scheduled with a grace and then superseded by a later decision never wakes. A change reaches the
  skill manifest and the promote receipt on their next read, and the gate derives against the
  snapshot composed at run start — once per run, inside the start's ledger-locked transaction, and
  journaled with the run's input so a replay reuses it — while a stored verdict keeps the `blocks`
  it was derived with.
- **`check ≡ deploy`:** both commands produce the package through the same code path; `deploy` is
  `check` plus build-and-ship *of the same package id*. If `check` ever grows a "faster" ingest
  path, the guarantee is gone.

### Databases (the hero app needs one)

Most vibe-coded apps need Postgres; the seed shows 3 instances and claims-portal is
Next.js + Postgres. Built in `provisioners/`; `contracts/provisioner.md` is canonical.

- **Pluggable.** The platform, never the app, picks a driver per resource type and environment.
  A driver does two idempotent things for one key, `project/env/resource`: *ensure* (ready with
  its outputs, pending, or failed) and *delete* (deleting what is already gone succeeds). Canyon
  keeps one record per key and drops it only once the driver answers gone, so a failed delete
  never orphans real infrastructure.
- **Under hand-off the org's platform provisions, never Canyon** (design/HAND-OFF.md §3, #2432).
  One journaled read of `run_by` decides both where a run's app goes and whether Canyon provisions:
  under `org` no driver is called and no record or provisioned secret is written; the release names
  each resource (`{name, type, gives}`), the platform makes it and builds the variables from its own
  secret store, and its `deployed` report names where each one is (`platform_resources`, endpoints
  only, never a credential). Only a project delete's `remove` (`delete: true`) asks the platform to
  delete them — sent to each environment the org runs, or where a release's package declared
  resources, or with provided resources, or with a deploy release or a delete's remove still open,
  read once as the project is marked deleting — and the delete waits until each reports `removed`.
  Switching moves no data: a database Canyon made before the switch stays Canyon's, and the
  platform's starts empty.
- **Under hand-off with `delivery: repo` the server writes to git** (design/HAND-OFF.md §3b, #2470):
  the release step renders the release's files in memory (`gitops/`, the Deployment from
  `executor/kube`'s hand-off renderer, the company's templates from its repo) and commits them to one
  path of the company's deploy repo with `git` in the server's image (`internal/gitpush`, the token in
  one header), under one advisory lock per repo; production is a pull request through GitHub's pulls API
  (the subset Gitea and Forgejo serve too) or a merge request through GitLab's (#2480). The run then waits as it waits on a report, and the access
  pull's check-in settles the deploy through the report's own decision path (`settle`), after one fetch
  of the branch proves the tree is Canyon's. Canyon reads nothing else of the org's cluster.
- **Built-in driver, for the demo:** one shared `app-postgres` server, on `canyon-provisioning` and
  each app's network, a database and login per key — never a container per app, and never the
  control plane's Postgres, which stays on `canyon-state` where neither an app nor the executor can
  reach it. The server is the only member of both, so it is the only thing that can create a
  database on one and seal its URL in the vault on the other.
- **Delegating driver, for a platform team:** Canyon calls the team's own adapter over HTTPS, and
  the adapter drives their Crossplane, Terraform or internal API. **Canyon never holds cloud
  credentials** — they stay with the adapter — and Canyon always initiates, so outputs arrive in
  the answer to its own request and no inbound port exists.
- Outputs are sealed in the vault and **injected at runtime** (`DATABASE_URL` for `postgres`) —
  never in the source, and only the env names the type promises.
- **One name, one source:** an upload declaring two resources that give the same name is refused
  (`invalid_resources`), a deploy whose resource would give a name a person's secret holds fails
  naming `canyon secret rm`, and a provisioned value is never set or removed by hand (`409 provisioned`).
- **Deleting an app** stops both environments (an environment the org runs gets a remove release),
  has each record's own driver delete it, newest first, dropping the record only once the driver
  answers gone, then removes the app's images and build caches, and last its secrets and the
  project; a failure keeps the rest and the same delete resumes (`contracts/routes.md`, the delete row).
- **Second v1 type: `s3`** (decided 2026-08-29; named as Score names it, since type names are
  written into repositories), served by a delegating driver. A type is one entry: its name, the
  outputs it promises and the env names they become. No further types (queues, caches) until a
  real fleet needs one.

### The control plane's own schema (upgrade path, canyonv2-3nq.3)

The server migrates at boot under one advisory lock, each embedded SQL file in its own
transaction with its record, so any number of concurrent starters apply each file exactly once
and `/ready` fails closed while anything is pending. The binary being replaced keeps serving
while the new one migrates, so **every migration is expand-only and N-1 compatible**, enforced
by `TestEveryMigrationIsExpandOnly` (`db/expand_contract_test.go`): it applies the files one at
a time and diffs the catalogue after each — a table or column that existed before a migration
and is gone after it fails the build, and so does a column added to an existing table `NOT NULL`
without a default, because the previous binary's inserts do not name it. A column an older binary could omit — absent, nullable or defaulted — that a migration leaves NOT NULL with no default, whether added so or tightened by SET NOT NULL or DROP DEFAULT, fails the same check; a type change is not seen by it and stays a review question. Drops wait for the
release after the last reader is gone. The allowlist has one entry, `0003_sessions_audit.sql`
dropping `projects.owner`, which nothing ever read. KEK versioning is the vault's: rows carry the
`key_ref` they were wrapped under, and `canyon-server rotate-kek` re-wraps them under a new version.

### Progress delivery

- **No push transport in v1** (decided 2026-09-04, `canyonv2-kaq`): the CLI and dashboard poll
  `GET /gate-runs/:id/timeline` (rows, cursor-paged) and `GET /gate-runs/:id` (state); the rows are
  the ledger (seam 2), outlive any connection, and include each producer's raw SARIF log verbatim.
- Resuming a run is a re-poll, never a replay — no history to serve, nothing anonymous to guard.
- Queueing is rendered, never silent: `· queued behind 3 runs`.

### Who may see and do what (decided 2026-09-25)

Every app sits behind company sign-in and is private to its team until shared: owners,
contributors and read-only members, plus admins. One decision point per app — Canyon's proxy on a
single host, a Canyon sidecar on Kubernetes — hands the app a signed identity token with the
user's app roles. The model and its enforcement: `design/PERMISSIONS.md`, the one source.

## 6. Deliberately not in v1

- Org-authored skill text (day 2, with §4's guardrails)
- No local/offline gate mode. The consequence, stated in the docs: **if the control plane is
  unreachable, nobody deploys.** That is what a governance gate means.
- No plugin system in the CLI; new behavior arrives as server data.
- No client-side agent execution by Canyon (the builder's harness is the agent).
- No patch-PRs raised from findings: the GitHub App reads and annotates, it never writes code.
- **Log redaction** — `canyon logs` tails app logs that can contain secrets/PII; centralized log
  storage ships only with shipper-level redaction (day 2).

## 7. Implementation order (demo dependency order — the visitor's order, not the stack's)

1. **Findings contract + gate v0** against a fixture app. Everything renders this.
2. **Ingest** (upload → materialized commit) + **durable gate runs** + the event feed.
3. **Dashboard**: fleet → app detail → findings + always-visible receipt → blocked promote →
   the unlock. Minutes 2–9.
4. **One app that really runs**: isolated build + path-routed preview + promote-the-reviewed-image.
5. **Meridian seed** on the real server (real scanner runs; recorded, labeled AI rows).
6. **Thin CLI** (`deploy`, `check`, `fleet`, `status`) — minute 12 onward.
7. **Skill file shipped with the binary.**

Auth is a stub through M1 and M2 and becomes real in M2b (`design/AUTH.md`), before the dashboard
work of M3b. The demo's builder image is pinned and pulled during
`docker compose up`, and the first-build wait is stated in the README.

## 8. Canyon Enterprise: `ee/` beside the core

Decided 2026-11-14 (the owner, option A; #2913): Canyon's server is the AGPL-3.0 core plus Canyon Enterprise's code,
`ee/`, under the Elastic License 2.0. The cut: **the code that creates or changes an Enterprise feature's
configuration is in `ee/`; reading, enforcing and removing what is set stay in the core.** The licence's verification
is `ee/licence`, and its signing tool `ee/tools/licence`. Platform patching's is `ee/patching` (#2923): each tick's
rebase and rebuild plans and their waves, filing patch approvals, the fleet approval's decisions and writes, and
turning patching on; the patch run itself, its watch and revert, the breaker, the rescan and every read stay in
`runs`. `db/migrations`, `web/`, `cli/` and `contracts/` stay in the core whole: a schema is no feature, and the
dashboard and the CLI are the same for both builds.

- **No core file imports `ee/`.** `internal/enterprise`'s `TestCoreImportsNoEnterprise` parses every Go file outside
  `ee/`, tests included, and fails on one that imports `ee/` unless its own `//go:build` requires `!noee` whatever
  its other tags.
- **Hooks, satisfied structurally.** The server holds one `eeHooks`, a field per hook (the licence's, and each
  feature's as its move to `ee/` adds it with the code that calls it), each an interface the core declares and `ee/`
  satisfies without naming it; the types `ee/` must answer in (`Querier`, `Required`, `Licence`) are `internal/enterprise`'s. Each hook
  takes the caller's transaction, since the licence is read inside it. A nil hook is a server built without Canyon
  Enterprise, and answers as an unlicensed server does (contracts/routes.md, *Licence*).
- **Hooks beyond the server's.** Outbound keys (#2922) add two, set from the server's `eeHooks.outbound`: the
  pipeline's `runs.Pipeline.Inject`, nil a `/run` carrying no `egress.ca` and no placeholder, and egress-control's
  `egresscontrol.Injector`, nil no interception listener and a routed call refused `inject_unavailable`. Platform
  patching (#2923) adds one, set from `eeHooks.patching`: the pipeline's `runs.Pipeline.Patcher`, nil reading patching
  as off. runs' own tests set it from an external test package in a `//go:build !noee` file
  (`runs/ee_patching_test.go`), as `ee/` imports runs and runs never imports `ee/`.
- **Wiring, one file per feature.** `cmd/canyon-server/ee_<feature>.go`, each `//go:build !noee`, sets its own hook:
  `ee_licence.go` the licence, then each feature's from it. The default build is the released one, so no release
  path can leave a hook unset by forgetting a tag, and `ee_wired_test.go` fails a default build with one unset.
- **A server built without it** is `rm -rf ee && go build -tags noee ./...`; CI's `noee` jobs build, vet and run
  every test that way, `noee_test.go` among them, which holds what that server answers.
