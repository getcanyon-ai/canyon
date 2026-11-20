# Cluster E2E — the cases

The journeys Canyon must pass on a real Kubernetes cluster: a real identity provider, real
buildpack builds pushed to a registry, a real model, two people sharing an app, both front-door
choices. `README.md` says how the suite runs and what the test cluster provides. Every case here names its
id, journey, preconditions, steps, exact assertions, cleanup and the surface that proves it:
**UI** (Playwright, `e2e/ui`), **CLI** and **API** (the Go harness, `e2e/`), **cluster** (the Go
harness through the read-only kubeconfig), **app** (a page of the fixture app, read in the browser).

Where a Docker journey (J1–J11, `make e2e`) already proves a step, the case says so and the
cluster run does not repeat it: the cluster proves what Docker cannot — sign-in, the sidecar, the
front door, the build on kpack, NetworkPolicy, the registry — and trusts `make e2e` for the rest.

## The run

One run is one fresh install (README, *Fresh install per run*), the cases in this order, apps named
fixed (README, *App names and redirect URIs*): the three phases are serial, the cases inside a phase
run as marked (`∥` may run beside its neighbours, `→` waits for the one before).

| Phase | Cases | Front door | About |
|---|---|---|---|
| 1 admin | A1 → A2 → A3 → A5 | none → choice 2, via `canyon` | no build |
| 2 maker, under choice 2 | M1 → M2 → M3 → M4 ∥ (G1 → G2) · M5 → K1 → S1 → S2 → S3 → S4 → I1 ∥ I2 ∥ I5 ∥ P1 · M6 → M7 | choice 2 | three builds |
| 3 choice 1 | A4 → M8 → M9 | choice 1 | one build |
| wave 2 | H1 → H2 → H3 | choice 1, `run_by: org` | one build |
| hand-off, deploy repo | H4 → H5 → H6 → H7 → H8, on its own (`make e2e-cluster-handoff-repo`) | choice 1, `run_by: org`, `delivery: repo` | one build |
| outbound, beside the suite | O1, on its own (`make e2e-cluster-egress`) | whatever the host has | one build, no fresh install |

The three fixture apps: **`f-cluster`** — the claims portal (`f-core`, Docker's J4–J7 app: `/db`
counts visits, `/secret` shows a value's digest, `/version`) plus `canyon.toml` `[roles]
approver = "Approves claims"` and two pages the cluster needs: `/whoami`, which verifies the
identity token the sidecar hands it (`CANYON_IDENTITY_ISSUER`, `_AUDIENCE`, `_JWKS_URL`,
design/PERMISSIONS.md) and prints `email`, `roles` and `aud`; and `/probe?to=<host:port>`, which
dials `to` from inside the pod for 3 s and prints `connected`, `refused`, `timeout`, `unresolved`
(the name did not resolve) or `invalid` (`to` missing or not `host:port`) — never `timeout` for any
of the other four, so a DNS miss or a typo can never read as a passing isolation case. **`f-vuln`**
and **`f-cp`** as Docker's. Every browser step runs at desktop width; the phone project is Docker's
job. A deleted project's id is never free again (contracts/routes.md), which the fresh install absorbs.

## A — Admin first run

**A1 · sign in through the identity provider, then onboard.** UI. *Given* a fresh install in
setup state (`onboarded_at` null), `CANYON_ADMINS` holds the admin's address. *Steps*: open
`$E2E_CANYON_URL`; the sign-in screen's one button reads `Sign in` and never names the provider;
sign in on the provider's pages as admin; the dashboard renders onboarding, not the fleet; fill
step 1 (organisation name `E2E Org`), step 2 (sign-in: the three addresses, no domain), step 3
(admins: the admin alone), step 4 shows `Ready 4 of 4` and the identity row read-only; click
`Finish setup`. *Assert*: the fleet loads with no apps; `GET /api/v1/preferences` answers
`onboarded_at` set; the audit trail's newest row renders `finished setting Canyon up`; before the
finish, alice at `$E2E_CANYON_URL` lands on `/#/setup-required` reading `Canyon is being set up.
Ask whoever installed it.` and `GET /api/v1/audit` holds no `sign in` row for her. *Cleanup*: none.

**A2 · front door, choice 2: the probe passes.** UI + API + cluster. *Given* A1. *Steps*: Settings
→ App hosting shows `No front door is set, so no app can run on this Canyon yet.`;
choose `Canyon signs people in`; the note `A sign-in proxy runs beside each app, against your
identity provider.` and the callbacks note are visible; enter issuer `$E2E_OIDC_ISSUER`, client id
`$E2E_OIDC_CLIENT_ID`, secret `$E2E_OIDC_CLIENT_SECRET`, domain `$E2E_APPS_DOMAIN`, form
`app.env.domain`; save. *Assert*: while saving, `Checking this cluster and the certificate…`; then
`Saved. Apps change at their next deploy.` within 90 s; `GET /api/v1/front-door` answers `choice: 2`,
`signin: {issuer, client_id}` and **no `client_secret` key**; the audit row `set front door` has
`secret_replaced: true` and its `detail` contains no secret value (the suite greps the whole
JSON for the value); cluster: no object labelled `canyon.probe` remains in `canyon-apps` within
30 s. *Cleanup*: none. Docker cannot run this: `frontDoorDocker`.

**A3 · the front door, outbound and playbooks refuse a member.** UI + API. *Given* A2. As alice: the front door reads
`Only an admin can change the front door.` with no form; `PUT /api/v1/front-door` as alice is
`403 {error:"admin_required"}` and writes no audit row. Settings › Outbound reads `Only an admin can
change how apps call out.` above how apps call out, read-only, with no choice and no button; `PUT
/api/v1/egress {mode}` as alice is `403 {error:"admin_required"}` (#2422). Guardrails › Playbooks is the admins-only page,
`Only an admin can open Guardrails.`; `GET /api/v1/playbooks` as alice is `200`, and `PUT /api/v1/playbooks/:slug`
as alice is `403 {error:"admin_required"}` and saves nothing (design/specs/mcp.md §3.2).

**A5 · apps call out through Canyon.** UI + API. *Given* A3, before any deploy: every app of the run is
floored from its first pod, so choice 2's and choice 1's pods starting at all is what proves they start on the
floor (choice 1 `oidc`, which the test cluster has no gateway for, is `TestKindChoice1OIDCPodStartsUnderEmptyAllowlist`'s). *Steps*: as admin, Settings › Outbound, `Through Canyon`, `Save how
apps call out`. *Assert*: `GET /api/v1/egress` is `via: canyon`, `mode: observe`, `receiver: true`, `canyon_refused:
null`; the newest audit row is `egress.set`. *Cleanup*: none: the next fresh install starts off.

**A4 · front door, choice 1: the gateway signs people in.** UI + hook. *Given* phase 2 done, the
test cluster's runner switched the gateway (`$E2E_SWITCH_FRONT_DOOR alb`, README). *Steps*: as admin, choose
`Your gateway signs people in`, gateway `alb`, `alb_arn` `$E2E_ALB_ARN`, save. *Assert*: `Saved.
Apps change at their next deploy.`; `GET /api/v1/front-door` answers `choice: 1, gateway: "alb"`;
`e2e-peer` (still running from phase 2) still answers with choice 2's oauth2-proxy until M8
redeploys it: cluster, its pod's init containers still include the oauth2-proxy image. *Cleanup*:
at the run's end the runner switches back (`$E2E_SWITCH_FRONT_DOOR none`) and, as the admin,
saves A2's choice 2 again, so Canyon's saved door matches the test cluster's Ingress.

## M — Maker alice

**M1 · `canyon login` through the device flow.** CLI + UI. *Steps*: the Go harness runs `canyon
login $E2E_CANYON_URL` with a config home of its own; the CLI prints `Your code: <code>` and
`Open <verification_uri_complete> and approve it.` and the code's ten minutes on **stderr**, and
opens nothing (`cli/cmd_login.go` `deviceFlow`; design/AUTH.md §4.2's `--no-browser` does not
exist); the harness parses stderr; Playwright opens `verification_uri` as alice (signing in on
the provider's pages), types the code, the screen names where the request came from, alice confirms.
*Assert*: the CLI prints `✓ Signed in to <host> as <alice's display>` and exits 0 within 60 s of
the confirmation; the config file is mode `0600`; the audit trail's newest row is alice's `sign in`
with `flow: device`; a second poll of the same device code is `400 {error:"expired_token"}` (single
use). *Cleanup*: `canyon logout`. Docker: no device flow (`/auth/config` `device: false`).

**M2 · upload the fixture and the real gate review.** CLI + API. *Steps*: `canyon --format json
check` in `f-cluster` named `e2e-app`. *Assert*: exit 0 or 2, never 1; the run's notes carry
**neither** `gate.Skipped` nor `gate.Reused` — the model ran; `GET /api/v1/gate-runs/:id` shows the
`ai review` producer among the run's producers with `failure_reason` empty; the same `check` again
answers the note `gate.Reused` and no second model call (the timeline shows no second `ai review`
step); `GET /api/v1/projects/e2e-app/access` (as alice) answers `roles: [{name: "approver", …}]`.
*Given* a fresh install: no PVC `canyon-scandb-trivy` in `canyon-scans`. Every tool runs by default,
so this first check is the first scan: during it a Job labelled `canyon.scandb` ran and the PVC
appeared, and the check still ends within 30 min (the gate re-asks a busy executor silently; no
timeline line marks the wait). Approve every blocking finding as Docker's J4 does, reason `e2e: the fixture's findings are planted` — as the admin, signed in through a browser: alice started the run, and a blocking finding is approved only by an admin or a responder who did not, from a browser session (#2204).
Docker's J2/J3 cover findings, fingerprints, approvals, refusals: not repeated.

**M3 · a real buildpack build, the preview live.** CLI + API + cluster. *Steps*: `canyon --format
json deploy`. *Assert*: exit 0 within 15 min; `deployment.url` is exactly
`https://e2e-app.preview.$E2E_APPS_DOMAIN`; the timeline holds `postgres db: creating`, `postgres db:
ready`, and its `deploy.completed` row's `url` is that address (the `live at …` build.log line is
prod's alone, written by the promote path); `GET /api/v1/projects/e2e-app` `envs.preview.
image_digest` matches `^sha256:[0-9a-f]{64}$`; cluster, namespace `canyon-apps`: Deployment, Service,
NetworkPolicy `e2e-app-preview`, ConfigMap `canyon-access-e2e-app-preview` exist; the Service's one port
is 4180 on both sides; the pod's `app` container image is `<registry>/canyon/e2e-app@<that digest>`;
`canyon-builds` holds no Build labelled `canyon.build` for the app 60 s after the deploy ends; the
CLI's live timeline showed `postgres db: creating` before `postgres db: ready`, and the project read's
`resources` lists `db · postgres · preview · ready` (`canyon resources` is the host's catalogue of
component types, design/specs/cli.md §6.5, not the app's); the `app` container takes its whole
environment from the Secret `canyon-env-e2e-app-preview` (`envFrom`) and names no `DATABASE_URL` of
its own, never a literal. Docker's J4 covers the provisioned
`DATABASE_URL` row and its refusals: not repeated.

**M4 · alice opens the preview and sees herself.** UI + app. *Steps*: as alice, open
`https://e2e-app.preview.$E2E_APPS_DOMAIN/whoami`; the proxy redirects to the provider; alice is
already signed in there, so she comes back without typing. *Assert*: the page shows `email:
<alice's address>`, `roles: -` (the exact string for no app role: an owner's grant carries none by
default), `aud: e2e-app`, `verified: true` — the token the sidecar hands the app verified against
Canyon's key set, which is what proves alice reached the app as herself; with the claims portal's
own `cp_session` cookie set on the app's origin (its middleware's sign-in, as Docker's `appPage`
sets it; `/whoami`, `/probe` and `/fetch` alone are exempt), `/db` counts `1`, then `2`.

**M5 · promote with approvals, then the prod address.** UI + API + cluster. *Steps*: as alice on the
app page, `Promote to production` → the dialog reads `Production runs the same image as preview,
with production's own secrets. Nothing is rebuilt.` → `Promote`. *Assert*: the Production region shows
`e2e-app.prod.$E2E_APPS_DOMAIN` within 5 min; `envs.prod.image_digest == envs.preview.image_digest`;
no Build was created (cluster, `canyon-builds` watched during the promotion sees none); the pod
`e2e-app-prod` runs the same image; alice at `https://e2e-app.prod.…/whoami` sees herself; `/db` in
prod counts `1` (a database of its own); audit rows `POST /api/v1/projects/{id}/promotions`,
`ensure resource`, `deploy prod` in that order. Docker's J5 covers the hand-set `DATABASE_URL`
refusal, the server kill and the crash-on-boot rollback: not repeated.

**M6 · undeploy keeps the data; redeploy reuses it.** CLI + API + cluster. *Steps*: `canyon
undeploy --env preview`; then `canyon --format json deploy --force` (`--force`: the case redeploys
from a fresh copy, which records no base, onto an app with a current package, design/specs/cli.md
§3.6). *Assert*: after the undeploy the
project read has no `envs.preview`; cluster: Deployment, Service, NetworkPolicy, ConfigMap and the
Secret `canyon-env-e2e-app-preview` are gone within 60 s (the harness lists names, never reads a
Secret); the `DATABASE_URL` preview row stays `provisioned`; after the redeploy `/db` counts one
more than before the undeploy. One build.

**M7 · delete.** CLI + API + cluster. *Steps*: `DELETE /api/v1/projects/e2e-app` without confirm,
then with `?confirm=e2e-app`. *Assert*: `409 confirm_required` listing two databases, then `204`;
`GET /api/v1/projects/e2e-app` is `404`; the audit trail is still readable and ends `DELETE
/api/v1/projects/{id}`; cluster: nothing named `e2e-app-*` in `canyon-apps` within 60 s. The
executor's `/forget` has its own test (`executor/forget_test.go`) and Docker's J6 covers the pending
and refusing adapter: neither repeated.

**M8 · deploy under choice 1.** CLI + cluster + app. *Given* A4. *Steps*: as alice, `canyon deploy
--force` in `f-cluster` named `e2e-peer` (a redeploy: one build; `--force` because a fresh copy
redeploys, with no base, onto the app phase 2 deployed, design/specs/cli.md §3.6). *Assert*: the run's `deploy.completed` row's `url` is
`https://e2e-peer.preview.…`;
the new pod's init containers hold `canyon-identity` alone, no oauth2-proxy; the gateway signs
alice in (the provider's pages, then back); `/whoami` shows her email; the headers the app saw hold
no `x-amzn-oidc-data`, no `x-amzn-oidc-accesstoken`, no `AWSELBAuthSessionCookie`.

**M10 · a person approves production the CLI asked for** (design/AUTH.md §4.5, #1820). API + UI.
*Given* M5 (e2e-app live in preview), before M9. *Steps*: the admin turns *Production is approved in the
dashboard* on from the admin's browser session; alice gets a CLI session through the device flow;
its promotion of e2e-app is
answered `202 {approval}` and its approve of that code `403 dashboard_required`; alice opens the
approval's url in her browser and approves. *Assert*: the page names who asked from the CLI and who
is approving; production goes live; the trail has `request promotion` and `approve promotion`.
*Cleanup*: the admin turns the switch off. `e2e/ui/cluster/approval.spec.ts`. Docker: sign-in is
off, so the switch cannot be on.

**M9 · delete `e2e-peer`.** As M7 (the refusal lists one database, preview's: `f-cluster` declares a
postgres, and the peer is never promoted), the run's last case: the cluster is left as the install
made it.
M8 proves choice 1's verification; S1–S3 are not repeated under it.

## S — Sharing

**S1 · share with bob as viewer; bob opens the app within two minutes.** CLI + UI + app + cluster.
*Given* M5. *Steps*: note the pod `e2e-app-preview`'s UID and every container's `restartCount`;
alice runs `canyon share bob@… --level read-only`; from that moment Playwright as bob loads
`https://e2e-app.preview.…/whoami` every 5 s. *Assert*: bob sees `email: <bob's address>` within
**120 s** — the kubelet syncs a mounted ConfigMap about once a minute, plus its watch delay, before
the sidecar can re-read it (the failure message names the last answer: `302 to
/#/no-access/e2e-app` or a status);
the pod UID and restart counts are unchanged; the access read lists `{kind: "email", grantee:
bob, level: "viewer", roles: []}`; the audit row renders `shared the app with bob@… as read-only`;
bob's dashboard shows `e2e-app` as the reduced row: name, owner, summary, and no runs, secrets or
deploy buttons; `GET /api/v1/projects/e2e-app/secrets` as bob is `403 team_required` naming alice.

**S2 · roles from `[roles]` reach the app.** CLI + app. *Steps*: `canyon share bob@… --level
read-only --role approver`; bob reloads `/whoami`. *Assert*: `roles: approver` within 120 s;
`--role nosuch` is the server's `400 unknown_role`, which the CLI renders as exit **1** with the
refusal's `message` on stderr (`cli/client.go` `fail`), and the access read is unchanged; the audit
row's `roles.before` is `[]` and `after` `["approver"]`.

**S3 · unshare: bob is refused within two minutes, the pod not restarted.** CLI + UI + cluster.
*Steps*: `canyon share --remove bob@…`; bob reloads every 5 s. *Assert*: within **120 s** (as S1) bob lands on
`$E2E_CANYON_URL/#/no-access/e2e-app`, which names the app, its owner alice and `Ask for access`
— never the app, never a bare 403; the pod UID and restart counts are still the ones S1 noted;
cluster: the ConfigMap `canyon-access-e2e-app-preview`'s annotation `canyon.updated_at` advanced;
audit `removed bob@… from the app`.

**S4 · the reduced read and the access request.** UI + API. *Steps*: bob clicks `Ask for access`.
*Assert*: alice's access screen lists bob under requests; `POST …/access-requests` twice is one
request; bob's `PUT …/access/email/carol@…` is `403 owner_required`; alice's `DELETE
…/access/email/<alice>` (her own last owner grant) is `409 last_owner`. Alice dismisses the
request: bob's `no-access` screen still renders.

## K — Secrets

**K1 · set and rotate; the value appears nowhere else.** CLI + app + cluster. *Steps*: `canyon secret
set --env prod API_TOKEN` with a fresh random value `v1`; `canyon deploy --env prod --yes`; then
`v2` and ship again. *Assert*: prod `/secret` shows sha256(`v1`), then sha256(`v2`), each within 5 min; the
harness greps for `v1` and `v2` in: the pod spec of `e2e-app-prod` (JSON), the pod's `app` container
log (`pods/log`), every event in `canyon-apps`, every ConfigMap in `canyon-apps`, `GET
/api/v1/projects/e2e-app/secrets`, the run's timeline, the audit page — every hit fails, naming
where; `secrets` row `API_TOKEN · prod · user`. The Secret itself is never read (the kubeconfig
cannot). Docker's J7 covers `rotate-kek`: not repeated.

## G — Scanners

G1 then G2, serially: G1 is the install's first scan, G2 its second.

**G1 · a known-vulnerable dependency, on the downloaded database.** CLI + cluster. The admin sets
trivy to `vuln` alone (`PUT /api/v1/policies/sarif.trivy?apply=true {state: advisory, run: true,
settings: {scanners: [vuln]}}`; a fresh install runs every tool already, so M2's check made the first
download). `canyon --format json check` in `f-vuln` named `e2e-vuln`. *Assert*: exit 0, `passed`, at
least one non-blocking `deps.known-vuln` finding at `package-lock.json` for **each** planted package
(`lodash`, `xlsx`) — never a count or line numbers, a live database drifts. Docker's J2 asserts the
shape against OSV; here it is trivy's database.

**G2 · a planted misconfiguration, on the downloaded database.** CLI + cluster. The admin turns
checkov on (`sarif.checkov`, advisory, as G1 turned trivy on); `f-cp` named `e2e-cp`. *Assert*: `blocked`, and among the findings the ones J2 names (`secrets.client-bundle`,
`auth.unauthenticated-route`, …) with the same files and lines — never a count: the real model
adds advisory candidates of its own; during the check a Job labelled
`canyon.scan` existed and **no** `canyon.scandb` Job ran; 60 s after, no `canyon.scan` Job remains.
No checkov must-fail fixture exists in the repo today (README, *Open questions*).

## I — Isolation and security

**I1 · app A reaches nothing it should not.** app + cluster. *Given* M3 and the phase-2 deploy of
`e2e-peer` under choice 2 (∥ with M3; M8 redeploys it under choice 1). From `e2e-app`'s `/probe`: `e2e-peer-
preview.canyon-apps.svc:4180` → `timeout`; `…:3000` → `timeout`; the server's Service in `canyon`
(`canyon-server.canyon.svc:7777`, its real port, `deploy/k8s/server.yaml`) → `timeout`; the
executor's (`canyon-executor.canyon.svc:7778`, `deploy/k8s/executor.yaml`) →
`timeout`; `169.254.169.254:80` → `timeout`; `$E2E_CANYON_URL`'s public address on 443 → `timeout`
(a raw dial reaches no address outside: the floor, design/EGRESS.md §2.2; I5 is the way out). Each probe's failure
message names the target and the answer.

**I2 · the apps database only on its own port.** app. `/probe?to=<E2E_APPS_DB_HOST>:5433` →
`connected`; `:5432` → `timeout`; Canyon's own Postgres host on 5432 → `timeout`.

**I5 · a call out leaves through Canyon, and is a row.** app + API. *Given* A5 and M3. From `e2e-app`'s
`/fetch?url=$E2E_CANYON_URL/`, Node's own `fetch`: the page shows the target's HTTP status. Within 90 s `GET
/api/v1/projects/e2e-app/egress` holds, in `envs.preview`, a row of `$E2E_CANYON_URL`'s host on 443 with
`last_outcome: allowed` and `last_protocol: connect`, the env's `via` is `canyon` and `floor` is `enforced`.

**I3 · pods run as the contract says.** cluster. What contracts/executor.md *On Kubernetes*
promises, and only that: for every app pod in `canyon-apps` seen during the run, every container
has capabilities `drop: [ALL]` and `allowPrivilegeEscalation: false`, and the pod has no
service-account token, no host network, no host port, no shared process namespace; scan pods in
`canyon-scans` run as user and group 65534 with a read-only root filesystem; each app's
NetworkPolicy ingress admits TCP 4180 from `canyon.front-door` pods alone; scan Jobs' policy admits
nothing in or out. The kind tests of invariants 10 and 11 (`make test-kube`) run once on the test cluster
before this suite and are not repeated here.

**I4 · the executor holds no Secret read.** cluster. A `SubjectAccessReview` with `spec.user`
`system:serviceaccount:canyon:canyon-executor` for `get`, `list`, `watch`, `patch`, `update` on
`secrets` in each of the four namespaces: every `status.allowed` is `false` (as
`TestKindExecutorReadsNoSecret`, run here against the test cluster; `kubectl auth can-i --as` would need
`impersonate`, which the read-only kubeconfig does not have).

## O — Outbound, beside the suite

**O1 · one app's way out, on a Canyon that holds other people's.** CLI + API, `make e2e-cluster-egress`. No
fresh install and no sign-in to the app: it needs alice and the admin, changes no setting, and touches nothing but its
own app. *Given* the host's via is `canyon` and its mode `observe`; under via `off` it fails on the raw dial, which is
its red. *Steps*: as alice, make `e2e-outbound-<random>` from `f-outbound` (a deleted id is never free again), with
`target.txt` holding `$E2E_CANYON_URL/`; check, the admin approving any blocking finding; deploy to preview. The app
says, at its start and every 10 s, `outbound fetch <answer>` from Node's own `fetch` and `outbound dial <answer>` from
one raw TCP connect to the same host and port. *Assert*: within 3 min `canyon logs --env preview`'s latest
answers are an HTTP status and `timeout`; within 90 s `GET /api/v1/projects/<app>/egress` holds I5's row, `via` `canyon`
and `floor` `enforced`. *Cleanup*: the app is deleted, pass or fail.

## P — Per-image settings

**P1 · every image Canyon runs is the digest the install set.** cluster. **Needs #475 merged** (the
settings, contracts/executor.md *Images and database sources*). Read the executor Deployment's env
in `canyon`: `CANYON_IMAGE_IDENTITY` (required), and each of `CANYON_IMAGE_OAUTH2_PROXY`,
`CANYON_IMAGE_TRIVY`, `CANYON_IMAGE_GRYPE`, `CANYON_IMAGE_CHECKOV`, `CANYON_IMAGE_OPENGREP`,
`CANYON_IMAGE_BUSYBOX`, `CANYON_IMAGE_BUSYBOX_STATIC` that the install set — an unset one is the
release's pin (the contract's *Unset* column), which the pods must then run. *Assert*: the `canyon-identity` init container of every app pod runs
`CANYON_IMAGE_IDENTITY` exactly; under choice 2 oauth2-proxy runs `CANYON_IMAGE_OAUTH2_PROXY`; every
scan Job's tool container ran its tool's setting, its init container `CANYON_IMAGE_BUSYBOX_STATIC`;
the probe Jobs of A2 ran `CANYON_IMAGE_BUSYBOX`; every value is `<registry>/<repo>@sha256:<hex>`,
never a tag. The harness records each pod it sees during the run and checks at the end.

## R — Patching (design/PATCHING.md §3.5, §4.8, §5; #2081, #2108, #2120)

**R1 · a rebase patch to production, and a revert that pauses the identity.** CLI + API + cluster.
Docker's `patch-e2e` (e2e/oidc/patch_test.go) proves the plan, the waves, the watch, the revert and the
breaker; this case proves what only the test cluster can, kpack's rebase (CI's kind runs no kpack controller,
#2091). Not yet in `TestSuite`: run by hand, in this order. *Given* the server's `CANYON_PATCHING=on`,
`CANYON_PATCH_INTERVAL=1m` and `CANYON_PATCH_WATCH=1m`; `platform.known-vuln` advisory
(`PUT /api/v1/policies/platform.known-vuln?apply=true {state: advisory}`); `$RUN` the ClusterStack's run
image now (`kubectl get clusterstack canyon -o jsonpath='{.spec.runImage.image}'`), its repository `$REPO`;
two images pushed to `$REPO` and their digests read: `$AFTER`, `$RUN` with `apt-get -y upgrade` as root,
and `$EXITS`, `$AFTER` plus the file `/etc/canyon-e2e-exit` (Dockerfiles as `runImage` in
e2e/oidc/patch_test.go). Three apps from `patchSource` (e2e/oidc/patch_test.go): the first in canary
order (sha256 of the id) answers and never exits, the other two exit 45 s after they start on a run image
holding the marker; each deployed, the first promoted, and every preview's image read `checked` with an
`os` finding. *Steps*: `kubectl patch clusterstack canyon --type merge -p
'{"spec":{"runImage":{"image":"'$EXITS'"}}}'`; wait until `kubectl get clusterbuilder canyon -o
jsonpath='{.status.stack.runImage}'` names `$EXITS`'s digest. *Assert*: the canary's preview patch run
names a Build with `spec.lastBuild`, the `STACK` reason and `spec.runImage` `$EXITS`; its `after.run_image`
equals the ClusterBuilder's `status.stack.runImage` byte for byte, and the diff IDs above
`runImage.topLayer` before and after are equal in order, with `build_metadata` and `config` (the run's
`Only the system libraries Canyon adds changed` line); the Build and any failed fresh tag are gone after; its watch
passes, its patch approval approved in the browser, and production's `image_digest` equals preview's.
Each other app's preview reads `reverted`, runs the digest it ran before, with `Canyon reverted preview
at … the patched app exited` (or `restarted`, as Kubernetes reads a restart under the pod's restartPolicy) on its patch run and one `revert patch preview` audit row; the second pauses
the identity, `paused.projects` both. *Then*, on a fourth app patched onto `$AFTER` and promoted by its
approval, a responder's stop during production's watch (`POST /api/v1/projects/:id/envs/prod/hold
{reason}`): the promote run ends `clean` with `The watch ended early: <who> stopped production`, and no
production pod of it starts again. *Cleanup*: the
ClusterStack's run image back to `$RUN`, the apps deleted, the identity left paused (a fresh install
absorbs it).

**R2 · the platform scan: the image read once, its layers matched after.** CLI + API + cluster + UI
(design/PATCHING.md §3.5, #2081). Docker's patch journey (`patch-e2e`, #2608) proves the finding, the
stale database and a re-match with no `docker save`. This case proves the test cluster's half: the image read from
ECR, the SBOM published beside it, and a rescan that never reads the image again. It is not yet in
`TestSuite`, so run it by hand. *Given*:
- the server's `CANYON_PATCHING=on` and `CANYON_PATCH_INTERVAL=1m`;
- the ClusterStack's run image `$RUN`, an older canyon-run whose `libssl3t64` has a published fix. Ubuntu's
  fixed USNs stay in trivy's database, so the finding is deterministic. On the test cluster this is R1's `$RUN`;
- `platform.known-vuln` `off` for the org;
- one app, `$APP`: a Node app with no dependencies, alice its owner and bob a contributor. alice adds
  `PUT /api/v1/projects/$APP/policies/platform.known-vuln?apply=true {state: advisory}`, then runs
  `canyon deploy`.

*Assert*, once preview is `Live`:
- **The finding.** Within the image scan's length, `GET /api/v1/projects/$APP/envs/preview/image` reads
  `state: checked` with `scanned_at` set. Its `findings` include `libssl3t64` with `class: os` and a fixed
  version, whose `fix` reads `Canyon fixes this: it updates libssl3t64 to <fixed> under the app.` The status
  receipt's `platform.known-vuln` row names the package, with `layers` `[{owner: run-image, class: os}]`.
- **The SBOM referrer**: ECR's referrers of preview's digest in `canyon/$APP` include one
  whose `artifactType` is `application/vnd.cyclonedx+json`.
- **A rescan reads no image.** Watch from `scanned_at` through three rescan ticks:
  - Each tick's server log line names `$APP` (`the run image fixes none of its counted findings` while the
    ClusterStack is on `$RUN`) and reads every other app as `platform.known-vuln is off`.
  - The Jobs `canyon-scans` creates (its `SuccessfulCreate` events) are all `canyon-scan-trivy-<run>`,
    the `sbom` target matching a slice. None is a `canyon-scan-trivy-java-<run>`, the `image` target, which
    alone reads an image, as the deploy's own scan did just before `scanned_at`.
  - CloudTrail holds no `BatchGetImage` or `GetDownloadUrlForLayer` on `canyon/$APP` after the image scan's
    own (by the executor's role, seconds before `scanned_at`; a kubelet's pull is the node group's role).
    CloudTrail delivers these some minutes late. Page through every event with `--start-time` and
    `--end-time`: a single page of `lookup-events` holds only the newest.
  - `scanned_at` does not move.
- **Who sees it.** In the browser signed in as bob, the app page shows the *System libraries* card,
  listing `libssl3t64` under `system libraries`. alice then makes bob a viewer on `$APP` (`PUT
  …/access/email/<bob> {level: viewer}`): the page shows no card and names no package, and his `GET
  …/envs/preview/image` returns `403 {error:"team_required"}`.

*Cleanup*: remove alice's addition and delete the app.

**R3 · a rebuild patch: a newer builder rebuilds preview, a person approves production.** API + cluster + UI
(design/PATCHING.md §5, #2120). The unit and DB tests prove `same_inputs`, `key_rotated` and the fixes-nothing
guard. This case proves on the test cluster that kpack's newer builder rebuilds an app and that its production is always
asked. It is not yet in `TestSuite`, so run it by hand. *Given*:
- the server's `CANYON_PATCHING=on`, and its `CANYON_LICENSE` a licence naming `platform_patching` (#2879), issued for the
  test cluster with `ee/tools/licence -seed -` from the licence signing key, piped from Canyon's vault and never written to
  disk (the internal runbook has the command). Without it, turning `patching` and `rebuilds` to ask answers `409
  licence_required` and the case cannot start; never run it unlicensed or with patching set by hand;
- `$NEW`, the ClusterStore's sources at the pin (deploy/k8s/builds.yaml), and `$OLD`, an earlier pin's. Check the
  precondition before creating anything: a tiny Node app built on each builder must show a counted `runtime`
  finding at `high` or above under `$OLD` that `$NEW`'s node-engine fixes. Compare the two image reads by layer
  owner. If it fixes none, the case tests only `patch_ineffective`: stop;
- the ClusterStore moved to `$OLD` (`kubectl patch clusterstore canyon --type merge -p '{"spec":{"sources":…}}'`),
  waiting until the ClusterBuilder is `Ready` on a new `latestImage`, `$B_OLD`. Only a build since 0120 records its
  builder, so the app must be built after that;
- one app, `$APP`, a Node app with no dependencies: alice its owner, bob a contributor, alice's addition `PUT
  /api/v1/projects/$APP/policies/platform.known-vuln?apply=true {state: advisory}`. It is deployed and promoted,
  and its preview image read counts the `runtime` findings above;
- as admin, in the browser: `patching` set to Canyon patches previews and asks before production, then `rebuilds` set
  to Canyon rebuilds previews on newer Node.js, Python, Java and Go releases and asks before production.

*Steps*: patch the ClusterStore back to `$NEW` and wait until the ClusterBuilder is `Ready` on `$B_NEW`.
*Assert*:
- **The plan.** The next tick's server log line reads `the rebuild plan onto …@$B_NEW started 1 rebuild(s) ($APP)`.
  Any app built on `$B_NEW` reads `the builder that built its image is the one Canyon names now`, and preview's
  `envs.preview.patch` reads `planned`, its identity `$B_NEW`'s digest.
- **The rebuild.** Its patch run builds the package anew on `$B_NEW` (the build log names the newer buildpacks'
  versions) and records `Rebuilt with <newer>: <version>'s code, build settings and dependencies are the same`.
  Preview's image read counts fewer `runtime` findings, without the ones the precondition named. Preview reads
  `patched_preview`, and once its watch passes, production reads `asked` with an `approval` code.
- **Always asked.** The approval read carries `kind: "patch"`, `rebuild: true` and the proof `Rebuilt from the same
  code and the same dependencies with <newer>. A rebuild is a new program, not a copy of what production runs, so
  a person on your team decides.` (`<newer>` is `a newer Node.js` only when every fix is in node-engine's layers,
  else `newer build tools`). `GET /api/v1/patching/identities` reads the identity with `kind: "rebuild"`, and
  Settings › App hosting names it `build tools <12 hex>` with no **Approve for every app**.
- **A person approves.** bob, a contributor, approves it in the browser at `/#/approve?code=<code>`, which
  answers `202 {run}`. Production then reads `patched`, and its image read's `image_digest` equals preview's.

*Cleanup*: as admin in the browser, `rebuilds` and `patching` off; the ClusterStore on `$NEW` (the overlay's);
alice's addition removed; the app deleted.

A builder rebuild and a build on one node filled its disk (DiskPressure ~5 min, 43 preview pods evicted, all
recovered): on a small test cluster, run R3 when no other build is running.

## H — Wave 2: hand-off (after design/HAND-OFF.md lands, its PR 8)

**H1 · release to the org pipeline.** *Given* choice 1 saved, the stub "org pipeline" from HAND-OFF
PR 8 deployed on the test cluster as a Service the webhook reaches; the platform webhook and host token set.
Admin sets `run_by: org`. `canyon deploy` in `f-cluster` named `e2e-org`: the timeline reads
`→ Handed to your platform · waiting for it to report` then `✓ Live https://… · reported by your
platform`; the stub's pod runs the reported digest; alice sees herself at `/whoami`.

**H2 · share reaches a handed-off app within two minutes.** S1 and S3 on `e2e-org`: the sidecar in
pull mode fetches `GET /access/e2e-org/preview` with the release token; the stub's pod not restarted.

**H3 · undeploy and delete under org.** `canyon undeploy` ends `removed`; delete answers
`202 {pending[]}` until the stub reports, then `204`. The stub's request log shows one `remove` per
environment, each signed.

## H — Hand-off through a deploy repo

`TestHandOffRepo` (`handoff_repo_test.go`), run on its own by `make e2e-cluster-handoff-repo`, never by
`TestSuite`: `e2e/handoff/repo_test.go`'s journey (design/HAND-OFF.md §3b) against the test cluster's git host, Argo CD,
External Secrets and CloudNativePG, behind the test cluster's own gateway. App `$E2E_HANDOFF_APP`, `e2e-repo` when unset (README, *App names*), fixture
`f-cluster`; the cases below name it `e2e-repo`. The
platform's side is the test cluster's, set up once (README, *Hand-off through a deploy repo*). `run_by` is the org's, so
the run switches the whole Canyon to hand-off and back: a deploy anyone else makes meanwhile is handed off too.

**H4 · the admin hands apps to the platform through the deploy repo.** API + hook. *Given* `run_by: canyon`
and a front door at choice 1 that passed Canyon's check (`probed: true`); either missing refuses the run before
anything changes, since switching back is refused behind a door that never passed. *Steps*, as the admin (a
browser session): `PUT /api/v1/preferences {delivery: repo}`; `PUT /api/v1/platform-repo` with
`$E2E_DEPLOY_REPO_URL`, its token, preview `$E2E_DEPLOY_REPO_PREVIEW`, production `$E2E_DEPLOY_REPO_PROD` through
a pull request, root `apps`; `POST /api/v1/platform-repo/fetch-token`, its value to `$E2E_STORE_FETCH` on stdin;
`POST /api/v1/platform-repo/check`; `PUT /api/v1/preferences {run_by: org}`. *Assert*: each answers `200`; the
check `passed` with templates `.canyon/resources/postgres.yaml` and `.canyon/route.yaml`; the preferences read
`run_by: org`, `delivery: repo`. No response carrying a token is logged, and the fetch-only token is in the
end-of-run grep.

*Who may touch the deploy repo*, GitHub's pulls or GitLab's merge requests alike (design/PERMISSIONS.md, *The deploy
repo*; the route tests hold each row, `TestPlatformRepoIsAdminOnlyAndNeverAnswersItsToken` a GitLab repo's too, and
H4 runs the admin's on the test cluster's git host). The app roles change nothing here: it is a host setting.

| Who | `GET /api/v1/platform-repo` | `PUT` (GitHub or GitLab), `…/check`, `…/token`, `…/fetch-token`, `…/host-token` |
|---|---|---|
| admin | `200`, never the token | `200`; a save that changes `api` carries the token again, else `400 invalid_token` |
| owner, contributor, read-only of an app | `200`, never the token | `403 admin_required`, before the body is read; nothing written |
| no access to any app (a signed-in member) | `200`, never the token | `403 admin_required` |
| signed out | `401` | `401` |

**H5 · deploy: committed to preview, synced, a pod checks in.** CLI + API + cluster + app. *Steps*: as alice,
`canyon --format json check` in `f-cluster` named `e2e-repo`, every blocking finding approved as M2 does, then
`canyon deploy`. *Assert*: exit 0 and `a pod checked in`; the platform repo's `last_commit` is preview's, with a
commit and no pull request; `envs.preview` has `url` `https://e2e-repo.preview.$E2E_APPS_DOMAIN`, `run_by: org`,
a `seen_at` and a digest; cluster, namespace `canyon-e2e-repo-preview`: the Deployment `e2e-repo-preview`, whose pods'
init containers are `canyon-identity` alone, and the database's claim `e2e-repo-preview-db-1`;
app (`e2e/ui/cluster/handoff.spec.ts`): alice opens `/whoami` through the gateway and sees `email: <alice's
address>`, `roles: -`, `aud: e2e-repo`, `verified: true`, and `/db` counts `1`, then `2`.

**H6 · production is a pull request a person merges.** CLI + API + hook. *Steps*: `canyon deploy --env prod
--yes`; while it waits, the app's audit trail names Canyon's pull request and the commit it pushed (`commit
release`), and `$E2E_DEPLOY_REPO_HOOK merge <url> <commit>` merges it as a person, never with Canyon's token.
*Assert*: the hook merged exactly one, from `canyon/e2e-repo/prod/…` into `$E2E_DEPLOY_REPO_PROD`; the deploy exits 0 with `a pod checked in`; `envs.prod` runs preview's image digest,
`run_by: org`, a `seen_at`.

**H7 · undeploy keeps the data.** CLI + cluster. *Steps*: `canyon undeploy --env preview`. *Assert*: exit 0;
within 3 min the Deployment `e2e-repo-preview` is gone from `canyon-e2e-repo-preview`; the namespace and the
database's claim `e2e-repo-preview-db-1` stay (`canyon.json` and `resources/` stay in the repo).

**H8 · delete removes it.** CLI + hook + cluster. *Steps*: `canyon delete --confirm e2e-repo`, again until it
prints `Deleted e2e-repo`, the hook merging production's pull request between tries. *Assert*: within 5 min
both namespaces `canyon-e2e-repo-preview` and `canyon-e2e-repo-prod` are gone, and the data with them.

*Cleanup*, pass or fail, in this order: the app deleted through the repo while `run_by` is still `org` (the
hook merging production's pull request); `$E2E_DEPLOY_REPO_HOOK remove e2e-repo`, a deletion commit on each
branch for anything left under `apps/e2e-repo`, never a force-push; then `run_by` and `delivery` back to what
they were before H4; then the fetch-only token is replaced, so the one in the cluster opens nothing. No case starts
unless it and the cleanup can finish before `go test`'s `-timeout`, which runs no cleanup.
