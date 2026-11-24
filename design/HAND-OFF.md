# Hand-off — the org's own platform runs every environment

**Built** (§9's PRs, #477–#536; #2016). Mode C beside mode A (Canyon's router on Kubernetes, built) and mode B
(Gateway API routes, `design/GATEWAY-ROUTES.md`). Hand-off is for an org that already has a
platform: Canyon builds, scans, gates and publishes; **Canyon deploys nothing, previews included,
and provisions nothing: the org's platform makes every database and bucket an app declares** (#2432).
The org's pipeline — Argo CD, Flux, GitHub Actions, Jenkins — deploys what Canyon hands it and
reports back. An environment is live only when the org reports the approved digest running.

What stays exactly as today: the upload, the gate, approvals, promote-binds-a-run, the receipt,
sharing, the sidecar's token to the app. What changes: the step after the build.

## 1. The setting

**Where apps run** is one key on the preferences row, `run_by` ∈ `canyon | org`, `canyon` by
default — `PUT /api/v1/preferences {run_by}`, admin-only and audited like every key there
(design/AUTH.md §4.5 chose the same row for the same reasons). It is a Kubernetes host's alone:
on a `docker` host the PUT is `409 {error:"not_kubernetes"}` with D5's Docker sentence, because the
Docker executor's image is the daemon's, never pullable (§2). Setting `org` needs the platform
webhook (§3) already saved, else `409 {error:"webhook_required"}`, and a front door set, else
`409 {error:"front_door_required"}`.

The server has to be able to serve a release, too (#2010): its `CANYON_IMAGE_IDENTITY` set, the
sidecar's image every release names, else `409 {error:"identity_image_required"}`, and its
`CANYON_PUBLIC_URL` `https`, the access URL the sidecar refuses otherwise, else
`409 {error:"https_required"}` — both answered after `not_kubernetes` and before the webhook, each
naming the setting. A server whose row already says `org` refuses to start while either holds,
and Settings › App hosting names what is missing before anyone tries.

Beside it, `access_grace` — how long a handed-off app keeps serving its last access list when
Canyon is unreachable (§5): a duration, `0` to `24h`, default `1h`, refused outside that range
(`400 {error:"invalid_access_grace"}`). It applies to releases sent after the change.

**How releases reach the platform** (amended 2026-10-24, #2470) is a second key on the same row,
`delivery` ∈ `webhook | repo`, `webhook` by default, admin-only and audited as `run_by` is: `webhook`
posts each release to the platform's pipeline (§3), `repo` commits each release's files to the
company's deploy repo, which Argo CD or Flux syncs (§3b). The dashboard recommends `repo`. Setting
`run_by: org` needs the chosen one saved: under `repo`, a deploy repo, else
`409 {error:"repo_required"}`, in place of `webhook_required`; and a PUT of `delivery` while `run_by` is
`org` needs it too. Like `run_by`, it applies to the next deploy or promotion: each release records
the `via` it was made under, and a remove follows the environment's last deploy's `via`, never the
setting.

The Settings screen shows it above the front door as two choices, *Canyon runs apps on this
cluster* · *Your platform runs apps*, and under the second the webhook URL, the host token, the
grace and the last delivery. **Amended 2026-10-23** (#2439): under the second, a checklist an admin
follows from the dashboard alone — the server's settings, the webhook saved, a front door at choice 1
(after the webhook, so a door this cluster can't pass the probe for still saves), a test delivery
(§3), then the switch — and once it is on, a summary with the last delivery,
the grace and what the platform team needs. The webhook secret and the host token are shown once,
when they are made, in a dialog that stays until the admin says both are stored; from then on the
screen offers to replace each (`POST …/secret`, `POST …/token`), never to show it.

**The front door stays** (`/api/v1/front-door`): under `org` it is what the sidecar in the
org's pod believes, so choice 1 is required, and its probe never refuses the save — a webhook is
saved whenever `run_by` is `org`, and a choice 1 door the probe finds unenforced or uncovered while a
webhook or a deploy repo (§3b) is saved is stored *not probed* (`probed: false`). Under `canyon` a probe that could not finish
refuses the save and changes nothing, so a passing failure never demotes a probed door; under `org` it
stores the door not probed, since nothing is refused there. **Canyon never runs an app behind a door that has not
passed the probe** (amended 2026-10-23, #2438): a deploy or promotion under `canyon` behind one
fails before `/run`, and switching back to `canyon` is `409 {error:"front_door_unchecked"}` until
the door is saved again and passes. The same rule under `canyon` is how a cluster without a wildcard
certificate reaches hand-off at all: save the webhook, then the door, then switch. `domain` and `form`
are not read by a release; the address is the org's (§4). Choice 2 is refused under `org` (§6).

**A prerequisite before the first deploy: the cluster enforces NetworkPolicy** (the owner's decision,
2026-10-26, #2507). Canyon still builds the image under `org` (§2), and a build's `canyon-builds`
NetworkPolicy holds only where the cluster enforces it: without it an app's install scripts reach the
node's metadata credentials, Canyon's database and the VPC. So every save records whether the probe
found NetworkPolicy enforced, whatever the certificate, and a deploy under either `run_by` fails before
the executor is called unless the stored door's save did — the probe answered `not_enforced` (a plugin
that ignores policy, or one that lets a new pod out before its policy is in force), could not finish, or
the door was stored before this. Its last `build.log` line is D5's `Canyon hasn't seen
this cluster enforce NetworkPolicy…`, which names the switch: on EKS, `enableNetworkPolicy` `"true"` and
`NETWORK_POLICY_ENFORCING_MODE` `"strict"` in the vpc-cni add-on; then an admin saves the front door
again. The certificate plays no part in this: a door stored not probed for its certificate alone still
builds. Canyon doesn't watch the cluster after a save. Enforcement turned off, the vpc-cni add-on put
back in standard mode, or new nodes without the policy agent keep builds going until the door is saved
again, and the probe checks one node. Nodes with IMDSv2 and a hop limit of 1 are the backstop: a pod
then doesn't reach the node's role even where a policy is not in force.

**Switching.** Each deploy row records the `run_by` it was made under, as it records its front
door today, and the setting applies to the next deploy or promotion. An environment Canyon runs
keeps running until its next deploy replaces it under the new mode or an undeploy stops it; an
environment the org runs stays the org's the same way. Nothing is moved by the switch. The switch
back to `canyon` needs a front door that passed the probe (above).

## 2. Which executor

**The Kubernetes executor alone.** Its `/build` already pushes the image to `CANYON_KUBE_REGISTRY`
as `<registry>/canyon/<app>@sha256:<digest>` (contracts/executor.md, *The registry*), pullable by
digest; the scans run there too. In hand-off the release is that reference and `/run` is never
called, so today's tag cleanup, which runs after `/run`, does not run after a build under `org`;
delete still calls `POST /forget`, since the build artifacts are Canyon's. Pushing to a registry
other than ECR is canyonv2-ula5.12.34, not this design.

The Docker executor would need a push step and a registry credential it does not have; that is
the other registries issue, not a hand-off cost.

## 3. The release webhook

A **release** is one instruction to the org: run this image here, or remove what runs here. One
row, `rel_<id>`, per instruction; a release names one app and one environment.

**Target.** `PUT /api/v1/platform-webhook {url, secret?}`, admin-only: `url` must be `https://`
(as the front door's URLs are), `secret` is generated by Canyon when absent and **answered once**
in the PUT's response, then sealed in the vault as the front door's `client_secret` is — Canyon
signs every delivery with it, so it cannot be hashed — and never answered again; a PUT with the
same URL and no secret keeps the stored one, and `POST /api/v1/platform-webhook/secret` replaces it
with one Canyon makes, answered once (#2439). `GET` answers `{url, set_by, set_at, last_delivery}`,
`last_delivery` the newest attempt to deliver a release since the webhook was saved and the status
it got (`0` for none), from `releases.attempted_at` and `attempt_status`.
No address filtering: an admin names their own pipeline's endpoint, and the body carries no
secret (§10).

**Delivery.** `POST <url>`, `Content-Type: application/json`, headers
`Canyon-Event: deploy | remove | ping`, `Canyon-Delivery: <uuid>` (the same on every retry),
`Canyon-Signature-256: sha256=<hex>` — HMAC-SHA256 of the raw body under the secret, GitHub's
scheme byte for byte, so every receiver library already verifies it.

```jsonc
{ "event": "deploy",                          // deploy | remove
  "delivery": "<uuid>",                       // the header's, inside the signed body: the receiver dedupes on it
  "release": "rel_8f2a",
  "app": "claims-portal", "environment": "preview",   // preview | prod
  "package": { "id": "pkg_7d3e8f", "label": "v1.4" }, // label absent when none
  "gate_run": "gr_8f2a", "gate_url": "https://canyon.example/#/apps/claims-portal/runs/gr_8f2a",
  "image": "<registry>/canyon/claims-portal@sha256:…", "port": 3000,
  "platform": { "os": "linux", "architecture": "arm64" },  // the image's, by digest: run it on nodes of this architecture (#2528)
  "variables": ["STRIPE_SECRET_KEY"],         // names of this env's runtime secrets rows, less every name a resource gives; values are the org's
  "resources": [{ "name": "db", "type": "postgres", "gives": ["DATABASE_URL"] }],  // canyon.toml [resources]; the org provides them
  "app_env": { "PORT": "3000", "CANYON_IDENTITY_ISSUER": "https://canyon.example",
               "CANYON_IDENTITY_AUDIENCE": "claims-portal",
               "CANYON_IDENTITY_JWKS_URL": "http://127.0.0.1:4182/.well-known/jwks.json" },
  "sidecar": { "image": "<registry>/canyon-identity@sha256:…", "port": 4180,
               "env": { "CANYON_FRONT_DOOR": "{…}", "CANYON_IDENTITY_ISSUER": "https://canyon.example",
                        "CANYON_IDENTITY_AUDIENCE": "claims-portal", "CANYON_TARGET": "preview",
                        "CANYON_ACCESS_URL": "https://canyon.example/access/claims-portal/preview",
                        "CANYON_ACCESS_GRACE": "1h" },   // §1's access_grace at the time of sending
               "secret": "CANYON_ACCESS_TOKEN" },   // the org fetches it (§5) and mounts it under this name
  "report_url": "https://canyon.example/releases/rel_8f2a/report",
  "sent_at": "2026-09-26T10:00:00Z" }
```

**The test delivery** (#2439). `POST /api/v1/platform-webhook/ping`, admin only, sends one
`{"event": "ping", "delivery": "<uuid>", "sent_at": "…"}` to the saved URL, never one the request
names, with `Canyon-Event: ping`, signed, timed out and refused redirects as a release is, outside
any transaction, and answers `{delivery, sent_at, status}` — the HTTP status, `0` for none, never
why. It is not retried, not a release, not audited and never `last_delivery`; one is in flight per
server (`429`). A receiver verifies the signature first and answers a bad one `401`, checks
`sent_at` as for any delivery, and answers `2xx` to an event it does not act on: so a `2xx` says the
address and the secret are right. It says nothing of the host token, which the first release's
report and access-token fetch are the first to use. **A receiver dispatches on the signed body's
`event`, never the header**, which the signature does not cover; a ping names no release, app or
report address, so a receiver that confused one has nothing to act on.

**Resources** (#2432, decided 2026-10-23). The platform provisions what `resources` names; Canyon
creates nothing under `org`. Platform teams already take resources this way — Score, platform orchestrators,
Crossplane, Service Binding and External Secrets each receive a type, a name and the variables the
app reads, provision, and build the variable from their own secret store — and Canyon's types are
Score's names. So a resource is `{name, type, gives}` and nothing more: no class (every menu row is
Score's `default`), no value and no format on the wire. The formats are documented
(docs, *Build → Databases*): `postgres` gives `DATABASE_URL`, a `postgres://` URL naming its `sslmode`;
`s3` gives `STORAGE_URL`. `variables` leaves out every name a resource gives, since the platform
provides it with the resource. One journaled read of `run_by` decides both where the run's app goes
and whether Canyon provisions, so a switch between the two never provisions for one and ships to the
other; the deploy still marks itself begun under the ledger lock before the release is sent, as
before a `/run`. A platform answers the webhook `2xx` within its 10 seconds and provisions after: the
whole provision fits the report's 30-minute wait.

A `remove` carries `event`, `delivery`, `release`, `app`, `environment`, `report_url`, `sent_at`
and nothing else, but on a project delete's: that one adds `delete: true`, and the platform then also
deletes what it provisioned for that app and environment — only that, keyed by app, environment and
resource name, idempotently. An undeploy's, a hold's and the brake's removes never carry it: they keep
the data, as an undeploy on Canyon's own cluster does. The org's pipeline runs the app and the sidecar in one pod, the app's port
answering the sidecar alone, exactly as Canyon's executor lays them out (contracts/executor.md,
*On Kubernetes*). **One producer:** `sidecar.env` and `app_env` come from the function
`executor/kube/objects.go`'s `deployment` uses to build the pod's environment today, which the
server imports — `CANYON_FRONT_DOOR` is composed in one place for both modes, so the sidecar
and the app cannot tell who started them, and a route mode (design/GATEWAY-ROUTES.md) changes
that one function, never a release.

**Retries.** A `2xx` is accepted. A `4xx` is refused and not retried: the run fails, its last
`build.log` line `Your platform refused the release: HTTP <status>`. A `5xx`, a timeout (10 s)
or no connection is retried: once a minute for five minutes, then every fifteen minutes, for one
hour in all, each attempt a `build.log` row — `your platform answered 503 · retrying in 15m`, or
`no connection` in place of the code; then the release is `undelivered` and the run fails, `Your
platform did not accept the release in an hour` — never silent. Every retry is the same delivery, re-stamped and re-signed: `delivery` stays, `sent_at` is the
attempt's. **A receiver must refuse a delivery whose `sent_at` is more than 30 minutes old, and
treat a `delivery` it has seen in the last 30 minutes as done** — freshness bounds the dedupe
state it keeps, and Canyon's hour of retries stays valid because each attempt is fresh.

**While it waits** the run stays `running` and writes `build.log` rows: `handed to your platform
· waiting for it to report` as the release is first sent — before any retry's row, so a platform
that reports inside its webhook handler never has its report land ahead of it — then every five minutes `still waiting on your platform (<elapsed>)`,
so a poller sees time pass. The wait is bounded at **30 minutes** — the bound a pending resource
has today — because a project runs one run at a time and a stuck wait would block every later
deploy. Past it the run ends `failed`/`error`, last line `Your platform has not reported on
<version> in 30 minutes. The release stays open: when it reports, the environment goes live.` A
report that arrives later still lands (§4).

## 3b. The deploy repo (`delivery: repo`)

**Option A, the deploy repo** (decided 2026-10-24, #2470). Canyon commits each hand-off release's
files to the company's deploy repo with a repo-scoped token over HTTPS; preview is pushed, production is
a pull request; Argo CD or Flux syncs it; External Secrets brings the access token through a fetch-only
token; a pod's first check-in, after the app answers and the branch tree matches Canyon's render,
settles the deploy. Everything before the step after the build stays as it is, and so does everything in
§4–§7 that is not said again here.

**The repo.** `PUT /api/v1/platform-repo`, admin-only: an `https://` URL with no userinfo, query or
fragment, a user name (`x-access-token` by default) and a token scoped to that one repository — on
GitHub a fine-grained token with Contents and Pull requests read and write, on Gitea or Forgejo an
access token, on GitLab a project access token with the Developer role and the `api` and `write_repository`
scopes — sealed in the vault as the webhook secret is, never answered,
replaced by `POST …/token`; preview's branch, always pushed; production's branch and how it changes,
`pull_request` or `push`; the path root (`apps` by default); the pull requests API, `github` (GitHub,
GitHub Enterprise Server, Gitea and Forgejo serve the same calls) or `gitlab` (GitLab.com and self-managed
GitLab: its merge requests, which Canyon reads, and names, as pull requests; #2480) at `api_url` — under
`gitlab` on the repo's own host and GitLab's own root, so the token goes nowhere else — or `none` (then production is `push`, and
the dashboard says before the save that production is then pushed with no review in git); the ClusterSecretStore's name (`canyon`); and, optionally, Canyon's CA, the public
certificates the sidecar needs to trust Canyon's address when a private CA signed it. Git runs over
HTTPS alone — the server's NetworkPolicy lets it out to public addresses on 443, so a private git host
needs a rule of its own (deploy/k8s, *Where `canyon-server` may connect*) — with the token in one HTTP
header, never a URL or an argument, redirects refused, and every failure kept as a problem code, never
the remote's words. The first save also answers, once, the **fetch-only token** (§5) and a host token
(§4), both kept on the repo's own row and replaced there (`POST …/fetch-token`, `POST …/host-token`) —
never on a platform-webhook row, so a release from before the deploy repo, which reads only that row,
boots and serves on a host that saved only a deploy repo (decision 26, amended 2026-10-24). That release
knows nothing of `delivery: repo`, so before rolling back to one, leave repo mode: save a platform webhook
and switch delivery to `webhook` (a host with none answers that switch `webhook_required` under `run_by:
org`), or switch to Canyon running the apps. A release before #2480 has no GitLab client, and a remove follows
the environment's last deploy's way whatever delivery says: before rolling back to one, save the repo with the pull
requests API `none` and production `push` (decided 2026-10-25). Reports
and the access-token fetch take either row's host token. A save that names another host for `url` or
`api_url`, changes the CA or changes the pull requests API must carry the token again.

**Check the repo** (`POST …/check`, admin, one at a time) reads both branches, reads the templates,
renders a sample release against them, pushes and deletes a throwaway branch `canyon/check-<n>` to prove
the token writes (a test branch: a rule on preview's own branch shows at the first deploy), and lists open pull requests through the API when production is `pull_request`. It
answers problem codes — `unreachable`, `tls_untrusted`, `auth_refused`, `branch_missing`, `no_write`,
`protected_branch`, `pulls_api_unavailable`, `template_missing`, `template_invalid` — each with D5's
sentence, and the templates it found. It says what it cannot prove: the branch rules, that a pull
request can be opened, and that Argo CD or Flux, External Secrets and the CA are set up — the first deploy
proves those, and until a pod checks in the checklist says so.

**What Canyon commits**, at `<root>/<app>/<env>/`, for namespace `canyon-<app>-<env>` — a fixed prefix
no setting removes, the whole at most 63 characters, else the release is refused, and the app and the
environment DNS labels already: `deployment.yaml` (the app and the sign-in sidecar by digest, as §3's
deploy names them, from the function the executor's pod uses — `executor/kube`'s hand-off renderer
shares its security contexts, resources and environment producers), `service.yaml` (4180, the sidecar),
`networkpolicy.yaml` (ingress to 4180 alone; egress is the org's), `externalsecret.yaml` (the access
token, below), `route.yaml` and `resources/<name>.yaml` from the company's templates, `ca.yaml` when
Canyon's CA is set, and `canyon.json` `{canyon, app, env, release, gate_run, package, version}`, which
Argo CD's files generator and Flux's namespaces generator read. Canyon renders **no Namespace object**, and nothing synced from the
deploy repo makes one: the reference set-up's namespaces ApplicationSet (Argo CD) or HelmRelease (Flux) makes it from the platform's own
repo (deploy/gitops/namespaces), with Pod Security `baseline` and Canyon's labels, `canyon.dev/env: <env>` among
them (the generator's own environment, which the gateway's listeners select on, §10), only its name taken from
the deploy repo's path, so a token that can write the deploy repo never chooses a namespace's pod security. Every object is serialized from a typed or
parsed value, never text, and carries `metadata.namespace` pinned to the release's; the access token's
Secret is `<app>-<env>-access-<id>`, the release id's hex part, a new name per release so every deploy
rolls the pod; a resource's Secret is `<app>-<env>-res-<name>` holding what it `gives`; the variables
are read from `<app>-<env>-vars`, names alone and none optional, so a value the platform has not set
never starts the app. Nothing secret is ever in git.

**Templates are the company's**, in its repo, read from the branch being written: `<root>/.canyon/route.yaml`
(required; under Flux it is also the reference namespaces generator's sentinel: while an environment's branch
lacks it, that generator makes and removes no namespace, so a root set wrong on the platform side never reads as
every app deleted), `<root>/.canyon/resources/<type>.yaml` (one per type an app declares) and
`<root>/.canyon/externalsecret.yaml` (optional: Canyon's External Secrets object when absent, so a
platform on Vault Agent or the CSI driver makes the same Secret its own way). Whoever can edit them can
already commit any YAML there, so Canyon grants nothing new; each commit's `Canyon-Templates` trailer
names the tree it used. A template is YAML parsed as nodes, and `${app}`, `${env}`, `${namespace}`,
`${name}`, `${secret}`, `${release}` and `${store}` are substituted only inside scalar values, each an
explicit string; it is refused — the release fails, nothing is committed, and the run's last line names
the file and why and says *ask your platform team*, never echoing its contents — when it is missing for
a declared type, does not parse, holds an anchor, an alias, a merge key, a duplicate key, a tag outside
YAML's core schema, `${…}` in a key or an unknown variable, is over 64 KiB or 10 objects (documents
counted), sets another namespace, makes a cluster-scoped kind, collides with one of Canyon's objects, or
fails its annotation: the route annotates `canyon.dev/address`, the app's https address, and each
resource annotates `canyon.dev/endpoint`, where it is (§4's shapes), both read back from the committed
bytes. Canyon checks no CRD's behaviour: a template that never makes `<app>-<env>-res-<name>` shows as
*your platform reported v1.4 deployed, but not running*, never as live.

**The commit.** Everything is rendered in memory before any git call. The commit is deterministic —
its date the release's, its message `<version> · <app> · <env>` and the trailers `Canyon-Release`,
`Canyon-Review`, `Canyon-Version` and `Canyon-Templates` — so a retry rebuilds the same commit; when the
branch's tree at the path already is the render, nothing is pushed. Before any push every changed path
must be under `<root>/<app>/<env>/`, else Canyon pushes nothing; a directory with no `canyon.json`, or
whose `canyon.json` names another Canyon, is never written over. Pushes to one repo take turns under one
lock, and under it, right before each push, Canyon reads again whether the release is still the
environment's newest, not removed since, still cleared and, on production, not held, so an old retry
never overwrites a newer directory. An attempt holds the lock at most three minutes — git's fetch,
commit and push bounded together, and a transfer slower than 1 KiB/s for 30 seconds given up — past
which it is `unreachable`. Never a force push. A push that is not a fast-forward is fetched, rendered onto the new head and tried again, up to 5
times. **Production** under `pull_request` is a branch `canyon/<app>/<env>/<id>` and a pull request into
production's branch, found again by its branch when it already exists — reused only when it is one
commit, Canyon's render at the directory and nothing else, on a parent already in production's history,
else `protected_branch`, and only a pull request of this repo's own branch, never a fork's of the same
name, looked for among the open ones before one is created, a create's `422` or `409` looked for again — and
opening one closes the environment's older Canyon pull requests into production's branch, of this repo's own
branches alone (a fork's is never Canyon's, nor its to close), read page after page until one
is empty, at most ten, so none is missed; more is an error and the release is tried again. A pull
request's address is kept only when it is https on the repo's or the API's host. Outcomes map onto §3's: pushed or the pull request open is
`accepted`; the token refused, a protected branch, a missing branch, a refused template or another
Canyon's directory is `refused`, its own `build.log` line, and so is a certificate Canyon does not trust
(`tls_untrusted`: no retry fixes it); no connection and the host's 5xx retry for the hour, then
`undelivered`.

**Remove.** An undeploy's, a hold's and the brake's remove commits the directory without everything but
`canyon.json` and `resources/`, so the data stays; a project delete's removes the directory. A pushed
remove is `removed` at its push; one that needs a pull request is `removed` when it is merged, unedited,
into production's branch from this repo's own branch, and `refused` when it is closed unmerged, the release-remove workflow asking once a minute for 30
minutes, after which the environment reads *being removed once you merge* and an undeploy or a delete
asked again asks again. The pods admit nobody from the remove's commit in Canyon (§5, *A stopped release*),
whatever git says.

**Deployed.** Under `repo` an environment is live when a pod of the release checks in (§5) and the app
answers, and the branch agrees with Canyon. The first such check-in first reads, before any git or API
call, whether the release is still the environment's newest, not removed since and, on production, not
held: an older release's late check-in is `superseded`, or held, as its report would be — never a tamper
alarm. Then, for a push and a pull request alike, it fetches the branch and needs
`<root>/<app>/<env>`'s tree to be the one Canyon rendered, else the release is `refused` with an audit
row; for a pull request it also needs the pull request merged with its head Canyon's commit, into production's
branch from this repo's own — open is not yet, closed unmerged, edited, retargeted or a fork's is `refused`. Then it goes through the same decision path as a report (`settle`), with the
digest, `address` and endpoints Canyon rendered, under the actor `{kind: canyon, subject: check-in}` and
its own audit row, `release live by check-in`. An API error means not live, asked again at a later
check-in, backing off per release. Repo releases still take reports, so a platform's `failed` lands.

**While it waits** the run's lines are D5's: committed or the pull request opened, then *still waiting for
a pod to check in* every five minutes, and at ten minutes *Canyon committed <version> 10 minutes ago and no
pod has checked in: look at your Argo CD or Flux sync*. The 30-minute bound stands, ending as §3's does,
in the deploy repo's words, the pull request's link when production waits on a merge (`pending.pull_request`).

## 4. The report

`POST /releases/:id/report` in the Open group, like `POST /webhooks/github`: no session, authenticated
by `Authorization: Bearer <host token>` — **one token per host**, created with the webhook (its
`PUT` answers `{url, token}` once), stored as its SHA-256, replaced by `POST /api/v1/platform-webhook/token`
(answers the new one once; the old one dies at that instant). One token because one pipeline
reports for the org; a per-app token would be a tier with one example.

```jsonc
{ "status": "deployed", "address": "https://claims.apps.meridian.example", "digest": "sha256:…",
  "resources": [{ "name": "db", "endpoint": "postgres://pg.data.meridian.example:5432/claims_portal_preview_db" }] }  // one per resource the release lists
{ "status": "failed",   "reason": "ImagePullBackOff: …" }     // plain text, ≤ 1024 chars
{ "status": "removed" }
```

- `200 {}` records it: `deployed` writes the environment — `url` the address, `image_digest`,
  `package`, `gate_run`, `deployed_by` the run's actor, `promoted_by` on prod — and the
  `deploy.completed` row, in one transaction with the audit row; `failed` and `removed` write
  theirs. The reporter's actor is `{kind: "agent", subject: "platform", display: "<org>'s platform"}`.
- **The digest check.** `deployed` whose `digest` is not the release's image digest is
  `409 {error:"digest_mismatch", expected}`: nothing goes live, one audit row `platform reported
  another digest`, and the waiting run says so and keeps waiting (contracts/events.md): the release
  stays open for the reviewed digest. Only the reviewed image is ever live.
- `address` must be `https://` (the front door's shape), parsed with `net/url`: a lower-case host
  and an optional port, a path of any segments of unreserved characters (none `.` or `..`), with or
  without a trailing slash, no userinfo, query, fragment or percent-escape; `status` outside the
  three, or a body outside the shape, `400 {error:"invalid_report"}`.
- **Endpoints** (#2432; required since #2452): `resources` names every resource the release lists, one
  entry each, as the release names it; a release that lists none may leave the key out or send `[]`. An endpoint is where the resource is and never how to sign in to
  it — a `postgres://` or `postgresql://` URL for `postgres`, `s3://` or `https://` for `s3` — parsed
  with `net/url`, no userinfo, query, fragment or percent-escape, its path at most one segment (the
  database or the bucket); anything else is `400 invalid_report`. Canyon keeps them per app,
  environment and name (`platform_resources`), written with the `deployed` record and never on a
  `409` or a duplicate, replaced whole by the next `deployed` that is written — absent clears: a
  `deployed` without `resources`, as one with `[]`, on a release that lists none, clears them — and
  shows them to the app's team: on
  the project read as `state: provided`, the app page, `canyon status`, and the delete's listing. A
  record Canyon made of the same name and environment (made before the switch to `org`) is shown beside
  them: switching moves no data, so the platform's database starts empty, and a delete destroys both.
- **Every resource, or nothing goes live** (#2452). A `deployed` that leaves one out — the key absent,
  `[]`, or a list short of a name — is `409 {error:"resources_missing", missing, message}`, after every
  other refusal and never over `409 held`: nothing goes live, the waiting run says which and keeps
  waiting (contracts/events.md), the environment's pending line says *Your platform's report left out
  where db is*, and one audit row `platform reported deployed without resources`. The platform
  provisioned them, so it knows where they are; a live app whose database Canyon cannot name is one a
  delete would leave behind and the team could not find.
- **Late** — after the run's 30 minutes: accepted and recorded the same; the run's state stays.
- **Duplicate** — the same status again: `200 {}`, nothing written. A `deployed` after `removed`,
  or on a release a newer release of the same environment superseded: `409 {error:"superseded",
  release}` — the org's pipeline is behind, and nothing older ever overwrites newer.
  An undeploy's `remove` supersedes the open `deploy` release a run waits on: the run ends
  `interrupted` at once, and a `deployed` on that release later has Canyon send another `remove`
  (design/OPERATIONS.md §3.3; contracts/routes.md, *The brake*).
- `401 {error:"bad_token"}` — a rotated token's too: the org retries under the new one and the
  report lands late · `404 {error:"not_found"}` on an unknown release · bounded per token, 60 a
  minute.

Release states: `sent → accepted → deployed | failed | removed`, and `undelivered`, `refused`.
The project read's `envs.<env>` gains `run_by` and, while a release is open, `pending:
{release, event, sent_at, state}` so a screen can say *sent to your platform at 10:00, not running yet*; it
stays after `undelivered` or `refused` until the environment's next release, so the failure is
never silently gone.

**No word.** Three silences, each named: the webhook not accepted (§3, an hour), accepted and not
reported (§3, 30 minutes, the run fails and the release stays open), and a `remove` not reported —
undeploy answers `202 {release}` at once and the environment shows `being removed by your
platform`; it leaves `envs` on `removed`. There is no timer on a remove: nothing runs one, and
the state says exactly what is known.

## 5. The sidecar in a release

The image is `CANYON_IMAGE_IDENTITY`, the executor's, digest-pinned per release; the settings
are `/run`'s today plus `CANYON_ACCESS_URL`, `CANYON_ACCESS_GRACE` and a token.

**Pull mode.** When `CANYON_ACCESS_URL` is set the sidecar fetches the access file from it at
start and every minute, `Authorization: Bearer $CANYON_ACCESS_TOKEN`, instead of reading
`/etc/canyon/access.json`; it takes each good fetch's file as it is, an older `updated_at` too, so a
file Canyon answers, a deny-all among them, is never set aside; only the file reader keeps the newer
(amended 2026-10-24, #2451).
A sidecar that cannot fetch at start never starts, as one that cannot fetch its key set does not —
unless it cached a list within the grace (below).

**The route.** `GET /access/:app/:env`, Open group, `200` the access file (`PUT /access`'s `access`
object, the same producer), or the deny-all file for a stopped environment (below), `401` on a bad token. **The token** is minted per release, for that
app and environment, and the org's pipeline fetches it with the host token:
`GET /releases/:id/access-token` → `200 {name: "CANYON_ACCESS_TOKEN", value}`, answered every
time — a pipeline that re-runs must not be stranded — and put in the org's secret store by the
pipeline, never by Canyon and never in the webhook. It grants that one read on that one
app-environment and nothing else: a pod that leaks it exposes one team list, not a report route.
It dies when a newer release of the environment reports `deployed` (§4's superseded rule), so
every deploy rotates it. Under `delivery: repo` (§3b) External Secrets fetches it, through a
ClusterSecretStore the platform team applies once, with the **fetch-only token**: a token of its own,
stored as its hash on the platform-repo row, made by the repo's first save or
`POST /api/v1/platform-repo/fetch-token` and answered once, that opens `GET /releases/:id/access-token`
and nothing else — the report-capable host token stays out of the cluster unless the platform sends
reports (an Argo CD notification's `failed`). Kubernetes projected service-account tokens were the alternative and
lose on two counts: Canyon would have to reach the org's cluster issuer to verify them, and
hand-off would be Kubernetes-only, which nothing else here requires.

**A stopped release admits nobody** (amended 2026-10-24, #2450). Canyon deploys nothing under
hand-off, so an undeploy, a hold or a delete stops nothing itself: it sends a `remove`, and pods the
platform is slow to take down, or never does, would go on pulling the list and letting people in — and
the hold is the emergency stop. So the route answers a token that passes its check `200` with the
**deny-all file** — the access file's shape naming nobody: no members, no admins, no grants, `updated_at`
now (contracts/routes.md, the access row) — while its environment is stopped: production held, or the
project deleting or deleted; or while its own release is: a `remove` itself, a `remove` of its
environment recorded at or after it, or a deploy Canyon ran there since (a switch back to `run_by:
canyon`, after which the org's pods are no longer the environment's), which is recorded on the release
when Canyon's deploy is written while the org's setting is `run_by: canyon`, so an undeploy of Canyon's
own deploy never gives the release's pods the list back, and a write of Canyon's under `run_by: org` —
a patch, a deploy that started before the switch — never stops the org's release (#2469). Every sidecar of it refuses
everyone at its next pull, within a minute, with no grace, and nothing in the sidecar changes: pull mode
takes each good fetch's file as it is and caches it for a restart.

It starts when Canyon decides, never when the platform confirms: for a hold, at the commit that writes
it, before its `remove` is recorded; for an undeploy or the brake, at the commit that records the
`remove`, whatever the platform answers or reports on it later — accepted, removed, refused or
undelivered alike, since Canyon still means those pods off; for a delete, at `deleting_since`, before any
of its removes. A stop still pending on a run (*The brake*) records its `remove` when that run carries it
out, at its deploy step's end, so the list goes then.

It is read per token, so it never ends for a release a `remove` followed: a release recorded after the
`remove` — a fix pushed, a promotion, a lift's `start=true` — has its own token, and only that
release's pods pull the list. Ending it per environment would hand the old pods the list again the
moment a newer deploy was merely recorded, and for good if that deploy then failed: undeploy a
vulnerable version, the platform ignores the `remove`, push the fix, and the vulnerable pods admit people
again. The hold and the delete are the environment's, read per environment: a lifted hold has sent its
`remove`, so production's earlier pods stay stopped. Preview's pull is unchanged by a hold, as the hold
leaves preview alone. Clearing the token instead was the alternative and loses: the sidecar reads a
`401` as Canyon unreachable and serves its last list for the grace, up to `24h`, and a distinct `410`
would need a sidecar that tells the two apart; the deny-all needs neither.

**When Canyon is unreachable** the sidecar serves the last file it fetched for `CANYON_ACCESS_GRACE`
— `0` to `24h`, a bad value refuses the start — then answers `503 access_unreadable` as an
unreadable file does today. A minute is how fast an unshare reaches the app when Canyon is up;
the grace bounds how long a removed person can still open the app during a Canyon outage; `0`
turns every Canyon outage into an outage of every handed-off app, which is the admin's trade to
make, not Canyon's, so the default is `1h`.

**A restart inside the grace** (amended 2026-10-23, #2451). After every good fetch the sidecar
caches the list and when it fetched it on an emptyDir only its container mounts. A sidecar that
starts while Canyon is unreachable — its start fetch tried for 10 s — serves that cache while the
grace, counted from that good fetch, lasts: an OOM kill or a crash does not take the app down, and
no restart or crash loop extends the grace. Past it, or with no cache or a corrupt one, it never
starts. A new pod starts with an empty cache, so a reschedule during an outage still waits for
Canyon; surviving that would need a list Canyon signs and storage the org owns. There is no "serve
the last list until Canyon is back": a revocation must land within the grace, whoever can cut the
sidecar off from Canyon.

Canyon's re-PUT of access files (`reput`) skips environments the org runs: the pull replaces it.

**A pod's check-in** (#2452). Each pull that answers `200` records `seen_at` on the release the token
was minted for, after the token check and apart from it, at most once per 30 seconds per release,
best effort: a write that fails or would wait is skipped and the pull answers as before, so the
check-in can never gate sign-in. The environment's `seen_at` is its live release's; every surface
says *your platform reported it running 2m ago*, or, in amber, *your platform hasn't reported v1.4 running
since 15:04* after 5 minutes unseen, or *your platform reported v1.4 deployed, but not running* once 10 minutes have passed since the
report with none (contracts/routes.md, `envs.<env>.seen_at`). While the environment is stopped — its
`pending` a `remove`, which an undeploy, a hold and a delete each record at once — a fresh check-in is
amber, *still running: your platform last reported it 1m ago*: the pod pulls the deny-all, so the platform has not taken it
down yet (#2450). It is Canyon's only view into the org's
pods, independent of the pipeline's word, and it says less than `running`: the sidecar is a container
of its own, so a check-in proves a pod is scheduled and reaches Canyon, not that the app answers.
Under `delivery: webhook` it is shown, never a gate: a pod behind an egress rule that blocks Canyon would
otherwise block promotions with nothing to fix in Canyon. **Under `delivery: repo` it settles the
deploy** (amended 2026-10-24, #2470; §3b): there is no pipeline to report, so a release goes live at the
first check-in whose pull carries `Canyon-App: answered` — the sidecar dials the app's port before each
pull and sends it when the app accepts the connection, since the sidecar is a native init container that
pulls before the app starts — and whose branch agrees with Canyon. The access token is then what a
deploy is proven with, which is acceptable only because the fetch-only token (above) is the one the
cluster holds, the `canyon` store serves only `canyon-` namespaces (deploy/gitops), and the branch's tree
must be Canyon's render (§10).

## 6. Sign-in choices under hand-off

**Choice 1 only.** The org's gateway signs people in and the sidecar verifies its token: nothing
secret leaves Canyon. Choice 2 puts oauth2-proxy in the pod with the OIDC client secret and a
cookie secret, which cannot travel in the webhook; the org would fetch two more secrets and mount
them, as with the access token. It is not designed here: setting `run_by: org` while the front
door is choice 2 is `409 {error:"signin_unsupported", message:"Your platform signs people in:
set the front door to choice 1 first."}`.

A grant to a person who has signed in to Canyon opens the app only on a token from Canyon's own
issuer (design/PERMISSIONS.md, *Grants*). An org's gateway usually signs people in on another
issuer, and there such a grant matches nobody: people open the app through group and `everyone`
grants, and a grant to someone who hasn't signed in yet still matches their address.

## 7. What the person sees

`canyon deploy` and `canyon deploy --env prod` render the timeline as today; the new lines are:

```
✓ Built      v1.4 · sha256:ab12…  → published to your registry
→ Handed to your platform · waiting for it to report
✓ Live       https://claims.apps.meridian.example · reported by your platform
```

or `✗ Your platform reported: ImagePullBackOff: …` · `✗ Your platform removed the release before it went live` · `✗ Your platform has not reported on v1.4 in
30 minutes. The release stays open: when it reports, the environment goes live.` · `✗ Your
platform refused the release: HTTP 422`. Previews are the same lines under `preview`. Under
`delivery: repo` (§3b) the lines are the deploy repo's, D5's exactly: `committed v1.4 to preview · waiting
for a pod to check in`, or for production `opened a pull request for v1.4: <url> · production goes live
once it is merged and a pod checks in`, then `live at <address> · a pod checked in`; and the environment
reads `your platform hasn't reported it running since 10:00`, or `waiting on your merge since 10:00` with the pull
request's link to its team.

`canyon status`, the fleet and the app page show the environment's state from `envs.<env>`:
`live · reported 2m ago`, `sent to your platform at 10:00, not running yet`, `being removed by your
platform`, `failed: <reason>`, and under a live one its pods' check-in (§5): `your platform reported it
running 2m ago`, `your platform hasn't reported v1.4 running since 15:04`, `your platform reported v1.4
deployed, but not running`. The receipt's `prod.clean-gate` reads *nothing is running in
preview* until the org reported preview live: a promotion binds to what runs, and what runs is
what was reported.

**Undeploy** sends `remove` and keeps the data, as today, and from that moment the environment's
sidecars admit nobody (§5, *A stopped release*). **Delete** sends `remove` with `delete:
true` to each environment the org runs, or where a release's package declared resources, or the
platform reported provisioning, or a deploy release is still open (the delete's remove supersedes it),
or a delete's remove is still open — undeployed already or not, and
whatever `run_by` says now, since a release that never went live reported no endpoint and only the
release says what the platform may hold. It reads that set once, as it marks the project deleting, and answers `202 {pending[]}` until each reports `removed`, the shape
delete already has for a driver still deleting; the same request again resumes, and an open remove
without `delete` is never taken for one. Its listing before the confirm names what the platform
provisioned beside Canyon's records, as the platform's. Production, when it is such an environment, is live
for the rule that a person deletes it, as a deployed one does; and with one and no webhook saved the
delete is refused `webhook_required` before anything changes, so it never waits on a remove it cannot
send. Once the project is marked deleting, no `deployed` report writes anything: each is `409
project_deleted` and has Canyon send one more remove with `delete: true` for its environment, none while
one is open there, so a late report on any release — superseded, undelivered, failed — never leaves an app
running on the platform. A delete's remove that failed keeps its environment a target, and the delete
finishes, under the ledger lock, only once no such remove is open.

`canyon status` and the app page list what the platform provided under the environments, each with
its endpoint as text: `postgres db · preview · postgres://pg.data.meridian.example:5432/claims_portal_preview_db`.
`GET /api/v1/component-types` reads every menu type `available` under `org`: the platform provides it.

## 8. Contract deltas

- `contracts/routes.md`: `run_by` and `access_grace` on preferences with their refusals; the platform-webhook rows
  (`GET`/`PUT`, the token route); `POST /releases/:id/report` and `GET /access/:app/:env` and
  `GET /releases/:id/access-token` in the Open group; `envs.<env>.run_by` and `pending` on the
  project read; undeploy's `202` and delete's `pending[]` under `org`; the front door's probe
  never refusing a choice 1 under `org`, its `probed` and `probe_note`, `front_door_unchecked`, and
  choice 2 refused under `org` (amended 2026-10-23, #2438); `POST …/secret`, `POST …/ping` and the
  real `last_delivery` (#2439).
- `contracts/events.md`: the new `build.log` lines; `deploy.completed` written from a report.
- `contracts/executor.md`: nothing — the executor is unchanged; the sidecar's pull mode is
  documented under *The front door* as a second source of the same file.
- `contracts/schema`: `release.schema.json` (the webhook body, both events) and the report body;
  `app-page.schema.json` for `run_by` and `pending`; `front-door.schema.json`'s `probed` and
  `probe_note` (#2438); `release.schema.json`'s `ping`, `webhook_ping` and `webhook_secret`, and
  `Canyon-Event` an enum of its own (#2439); its `access_deny_all`, the access pull of a stopped
  environment (#2450); its `repo_*`, `fetch_token`, `canyon_json` and `pending`'s `via` and
  `pull_request` (#2470), with `contracts/testdata/release/repo/`, meridian's release as Canyon commits
  it, byte for byte.
- `contracts/testdata`: `release/meridian.json`, one release told end to end — the webhook, a
  deploy, its reports, a remove, the access pull, the environment while it waits. `validate.py`
  checks it as it checks the rest.

## 9. The build, in PRs

Each off `main`, ≤500 lines, contract first, failing test first (rule 2), merged before its
consumer.

1. **contracts: hand-off** — §8 whole; `validate.py` red on a body missing `report_url`, then green.
2. **platform webhook + host token** — the rows, the sealed secret, the hashed token, rotation;
   test: a member is `403`, the secret is answered once and never on `GET`, a delivery signed
   under the stored secret verifies, a rotated token is `401`.
3. **releases: send** — the row, the deploy step under `org` (build → publish → release → wait),
   the shared env builder, retries, the 30-minute bound, the `build.log` lines; test: a stub
   receiver in CI that answers `500` twice then `200` sees three deliveries under one `delivery` id, each with its own `sent_at`, and three
   `retrying in` rows; a receiver that verifies the signature refuses a tampered
   body; `4xx` fails the run with the status.
4. **releases: report** — the route, digest check, late, duplicate, superseded, the env write and
   audit rows; test: a wrong digest goes nowhere; a late report lands; `envs.preview` matches the
   reported address.
5. **preferences: run_by + access_grace** — after the release path exists, so `org` can never be
   set before it; test: `org` on a docker host, without a webhook, without a front door and on
   choice 2 is each its `409`; `25h` is `400`; a release sent after a grace change carries the
   new one and one sent before does not; the audit row carries before/after.
6a. **sidecar: pull mode** — against a stub Canyon; test: fetches at start, serves a share within a
   minute, serves the last list through an outage and refuses after `CANYON_ACCESS_GRACE`, at
   `0` refuses at once, a bad value never starts.
6b. **access route + per-release token** — test: a token for app A is `401` on app B; a
   superseded release's token is `401` once the newer one reports `deployed`.
7. **undeploy, delete, promote under org** — `remove`, the `202`s, the receipt reading reported
   state; test: promote with preview unreported is `gate_not_clean`.
8. **e2e** — the kind stack plus a stub "org pipeline" (a 60-line Go server in `e2e/`): it
   receives the webhook, verifies the signature, fetches the access token, starts the pod, reports
   the digest; `canyon deploy` ends `Live`; `canyon share` reaches the app within a minute;
   `canyon undeploy` ends `removed`. The proof for the whole mode.

## 10. Threats — the ones that exist here

- **Webhook forgery** (someone posts a fake release to the org's pipeline): the HMAC over the raw
  body under a secret only Canyon and the pipeline hold; the pipeline verifies before it acts.
- **Report forgery** (someone tells Canyon an environment is live): the host token, hashed at
  rest, rotatable, bounded per token; a forged `deployed` still cannot pass the digest check with
  a digest it does not have, and a matching one only claims what the org would have deployed.
- **Digest substitution** (the pipeline runs another image): the report must name the release's
  digest or nothing is live; a pipeline that runs a different image and reports the right digest
  is the org lying to itself — outside Canyon's model, as a pipeline that deploys nothing is.
- **Access-list exposure**: `GET /access/:app/:env` answers one app-environment's team under a
  token that reads nothing else; the list holds emails and grants, never a secret.
- **A pod the platform has not taken down** (#2450): after an undeploy, a hold or a delete, Canyon cannot
  stop the org's pods, only stop them admitting anyone. The deny-all file (§5) does that within a minute
  of Canyon's decision, whatever the platform does. What it cannot reach: a sidecar cut off from Canyon
  serves its last list for the grace, as in any outage — the grace is the admin's bound on that — and a
  pod deployed without the sidecar admits whoever its own front lets in. A token holder learns from the
  deny-all only that its environment is stopped; the check-in it records (§5) tells the team a pod still
  runs while it waits on the platform.
- **A faked check-in** (#2452): whoever holds a release's access token can pull, so a leaked token
  makes an environment read *your platform reported it running* while no pod runs. Under `delivery: webhook` it is shown,
  never a gate, and the next release's report kills the token. **Under `delivery: repo` it is a gate
  threat** (amended 2026-10-24, #2470): a faked check-in could settle a deploy. It is bounded three ways:
  the token reaches the cluster only through the fetch-only token, which opens nothing but the token
  fetch; the reference set-up lets only `canyon-` namespaces read from the `canyon` store and keeps the
  deploy repo free of anything but Canyon's root; and a check-in settles only a release whose committed
  tree is on the branch, unedited, and still the environment's newest — so a faked check-in at most claims
  live what git already says runs. **Accepted residual risk** (2026-10-24): the fetch-only token is not
  bound to an app, so any ExternalSecret in a `canyon-` namespace can fetch any app's newest access token,
  and with it pull that app's member list and check in for it. The tree check bounds what such a check-in
  settles to what git already holds, and the deny-all ends every stopped environment's list; the reference
  set-up's store admits only namespaces matching `^canyon-.+-(preview|prod)$`, which `canyon-apps` and
  `canyon-egress`, Canyon's own, never match. The check-in's early read of superseded, removed and held
  only spares a git read: the decision path decides again inside its transaction, under the ledger lock a
  hold, a remove and a delete take too, so one that lands while git is read still keeps the release from
  going live. **After a release is live**, the branch is Argo CD's or Flux's: a later push to the
  directory — someone's, with write to the deploy repo — is synced as any commit is, and Canyon does not
  verify the branch again until its next release of the environment. What bounds that push is the
  platform's (the deploy repo's bullet below), never Canyon's check-in.
- **The deploy repo** (§3b): Canyon holds one token for one repository and writes only
  `<root>/<app>/<env>/` and branches `canyon/*`, never forced; the path check and the template rules
  catch Canyon's own mistakes, not a stolen token. What bounds a stolen token is the platform's: branch
  rulesets on production that dismiss stale approvals, require approval of the most recent push and a code
  owner's review, a machine user with the Write role that is never a bypass actor or a code owner, one
  AppProject per environment whitelisting exactly the kinds Canyon and the templates make and no
  cluster-scoped one, Pod Security `baseline` on namespaces made from the platform's own repo, the
  admission policy on the apps' Services (the next bullet), and the ApplicationSet that binds each branch to its environment from the path, never from
  `canyon.json` (deploy/gitops); under Flux, the generator that makes each app's Kustomization on its own
  environment's branch alone, held by the admission policy on its identity (the next bullet). Every template value is a scalar substituted after the YAML is parsed, so
  an app's name never becomes YAML. The reverse is a signal: a pod deployed without the sidecar
  never checks in, and its environment reads *your platform reported <version> deployed, but not running*, which tells the team the app
  is one nobody signs in to through Canyon.
- **Write access to the deploy repo** (#2476): whoever can write it, Canyon's token or a person, controls
  the workloads in the `canyon-*` namespaces. Canyon's tree check verifies a release at go-live (§3b,
  *Deployed*); after that, the deploy repo's own access control and branch rules decide what runs. What
  bounds a writer is the platform's: the AppProjects (the deploy repo alone, `canyon-*-<env>` namespaces
  alone, the kinds they list, nothing cluster-scoped), Pod Security `baseline`, the admission policy
  `canyon-app-services` (deploy/gitops/admission), and the Gateway's listeners. **Under Flux** the
  AppProjects' part is deploy/gitops/flux's: the sync accounts hold `canyon-deploy`, exactly the AppProjects'
  kinds with no subresource, by RoleBinding in the generator's namespaces alone, never cluster-wide; each app's
  Kustomization, made by the generator from the platform repo, applies its own directory into its own namespace
  (`targetNamespace`) from its own environment's branch; the sync's GitRepositories leave every kustomization
  file out, so kustomize-controller builds the plain manifests as Argo CD's directory source does. That is
  the bound: `--no-remote-bases` refuses a remote git base, but kustomize-controller 1.9.6 still fetches and
  applies an `http(s)` file a kustomization's `resources:` names (#2479's CI shows it), so the flag alone would
  let a reviewed commit pull unreviewed content at sync time. The writer keeps the per-object annotations
  `kustomize.toolkit.fluxcd.io/prune: disabled` and `kustomize.toolkit.fluxcd.io/ssa: Ignore`, which keep an
  object through an undeploy as Argo CD's `Prune=false` does; the deny-all (§5) bounds such a pod. The
  generator's identity, `canyon-namespaces`, can by RBAC alone bind `canyon-deploy` in any namespace and change
  or delete any namespace, since RBAC scopes no create by name; the admission policy `canyon-namespaces`
  (deploy/gitops/flux/admission.yaml, `failurePolicy: Fail`, `Deny`) holds it to what Argo CD's
  `canyon-namespaces` AppProject holds that generator to and more: Namespaces named `canyon-<app>-<preview|prod>`
  with exactly Canyon's labels, changed or deleted only when already Canyon's; RoleBindings named
  `canyon-deploy`, to `canyon-deploy`, for the matching environment's sync account, in those namespaces alone;
  Kustomizations in `canyon-sync` alone, every reference and account pinned and no other field admitted. The generator refuses
  to render without the policy and its binding, so a cluster without them makes nothing. A writer who deletes
  `<root>/.canyon/route.yaml` from a branch stops that environment's namespaces (none made, none removed, no
  label restored) until it is back: an outage of namespace management, never an escalation. An AppProject restricts kinds, never fields, so a
  Service would otherwise reach past the namespace: `type: LoadBalancer` makes a cloud load balancer (on
  EKS the AWS Load Balancer Controller's NLB, internet-facing when annotated, with node security-group
  rules), `NodePort` opens a port on every node, and `spec.externalIPs` intercepts other tenants'
  traffic (CVE-2020-8554). The policy admits, in those namespaces, a Service of type `ClusterIP` alone,
  with no `externalIPs` and no `loadBalancerClass`, on create and update, whoever makes it (an operator's
  Service for a database too); it fails closed (`failurePolicy: Fail`, `Deny`) and needs Kubernetes
  1.30, where ValidatingAdmissionPolicy is GA. It is scoped by the request's namespace, matching
  `^canyon-.*-(preview|prod)$` — every namespace the AppProjects' `canyon-*-<env>` glob admits: a
  namespaced object is in its request's namespace, and a namespace's name never changes, so no label,
  annotation or field of the Service moves it out of scope. It does not match `services/status`: a sync
  writes the Service, never its status subresource (the API server drops `status` from a create or an
  update of the Service itself), and writing the subresource takes RBAC on `services/status`, which
  Argo CD's sync never uses and Flux's `canyon-deploy` (deploy/gitops/flux/rbac.yaml) does not grant.
  **The routes** are bounded by the Gateway's listeners. Both environments' routes attach to one Gateway,
  and preview takes unreviewed pushes, so a preview HTTPRoute that claimed `b.prod.<domain>` with a more
  specific match (`/login`) would win under Gateway API's precedence: unreviewed code served under a
  production hostname, with that host's cookies, around production's review. The reference set-up closes it
  at the Gateway: one listener per environment, `*.preview.<domain>` and `*.prod.<domain>`, each with
  `allowedRoutes.namespaces.from: Selector` on the namespace label `canyon.dev/env`, which the namespaces
  chart sets from its generator's environment (preview's generator reads the preview branch alone) and
  nothing synced from the deploy repo can set, since no AppProject of the deploy repo makes a Namespace and
  Flux's `canyon-deploy` grants none. A
  preview route claiming a production hostname is then accepted by no listener: the production listener
  admits no preview namespace, and the preview listener's hostname does not cover it. The reference route
  names its environment's listener (`sectionName: ${env}`). The backend is not pinned: the Deployment is
  the writer's too, so a rule holding an HTTPRoute's backend to the sidecar's 4180 bounds nothing the
  writer cannot change beside it, and an HTTPRoute reaches no other namespace's Service without a
  ReferenceGrant, a kind the AppProjects don't list.
- **Token leakage**: the host token is answered once, the access token on demand to the host token
  and the fetch-only token alone, neither in a webhook, a commit, a log, a finding or an event; the deploy
  repo's token is sealed, sent only in git's one HTTP header and the pull or merge requests API's, and never in a log, an
  error, an audit row or an answer; the host token is replaced in one
  call — with both a webhook and a deploy repo saved, each row's host token can report, so rotate both — the access token by the next release. The webhook secret is answered once too, on the first
  save or `POST …/secret`, and every answer carrying either is `Cache-Control: no-store`; the
  dashboard holds them in the dialog that shows them and nowhere else.
- **The test delivery as a probe of the network**: it posts only to the saved URL, which an admin
  chose, with a release's transport — no proxy, no redirect, ten seconds — and answers only the
  status, never the error, which would name addresses and certificates behind the URL. The status
  and its timing remain an admin's view of that one URL, as a release's `build.log` line already is;
  the operator's NetworkPolicy on Canyon's server is the control, as for releases.
- **Replay** of a delivery: `delivery` and `sent_at` are inside the signed body; the receiver refuses a
  `sent_at` more than 30 minutes old and dedupes `delivery` for that window, so a captured
  delivery replays for at most 30 minutes and then only as a duplicate; a replayed `deploy` of
  a superseded release is refused at report time. Replay of a report: the same status is a no-op.
