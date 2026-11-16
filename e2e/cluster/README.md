# Cluster E2E — how it runs

`CASES.md` is the suite. This file is what it needs and how a run goes. The suite is
infra-agnostic: it names no cloud, no account, no cluster; everything it needs arrives as an
environment variable, and the one thing only the test cluster can do — flipping the gateway in front of the
apps — is one named hook. `make e2e-cluster` runs it: the Go journeys (`go test -tags cluster
./e2e/cluster/...`) and the Playwright journeys (`e2e/ui`, project `cluster`), against a Canyon
already installed. The suite never installs, upgrades or deletes Canyon. The order is
`e2e/cluster/suite_test.go`'s `TestSuite`: one Go process that calls the Go cases and runs the
Playwright cases by id (`--project=cluster --grep ' A[123] '`, unanchored) between them, so `make
e2e-cluster` is that one test and the end-of-run secret grep reads every line, the browser's
included. M1 is the Go sign-in whole: alice's device code is approved by
`e2e/ui/harness/confirm-device.ts`, which the Go harness runs. K1 runs on M5's state, and each
value it sets reaches the browser's K1 as `E2E_K1_VALUE` -- in the grep's list, never on argv. The
hook is switched back to `none` from `TestSuite`'s cleanup, pass or fail.

A Canyon that holds other people's apps never gets a fresh install, so `make e2e-cluster` is not for it. `make
e2e-cluster-egress` is what runs there: O1 alone (`TestOutbound`), one app of its own, which it deletes, and no
setting changed. It reads the app through `canyon logs` and the app's egress records, so it needs no redirect URI, no
kubeconfig and no `/probe`: of the variables below, `E2E_CANYON_URL`, `E2E_APPS_DOMAIN`, `E2E_OIDC_ISSUER`, and alice's and the admin's
addresses and passwords. It passes only where the host's via is `canyon` and its mode `observe`.

## Reuse

- **Go** (`e2e/`): `canyon(…)`, `cliJSON`, `api`, `read`, `waitFor`, `timeline`, `secretOf`,
  `fixture` and `forget` as they are; `host` becomes `$E2E_CANYON_URL`; every request carries a
  session (below). `compose`, `docker`, `psql` and `stub` have no cluster counterpart and are not
  called: the cluster's surface is `kubectl` through `$E2E_KUBECONFIG`, as `executor/kube/kind_test.go`
  already uses it.
- **Playwright** (`e2e/ui`): `journey()` and its per-step surface diff stay. A `cluster` project
  sets `baseURL` from `$E2E_CANYON_URL`; `surfaces.ts` gains a reader that lists pods (name, UID,
  restart counts, images) and ConfigMap annotations in `canyon-apps` instead of containers, and
  reads Canyon's database not at all (no access on the test cluster): a step declares `pods` and
  `audit_events` (through the API), not tables.
- **Sign-in**: one helper, `signIn(page, user)`, drives the provider's pages (`$E2E_*_EMAIL`,
  `…_PASSWORD`), then the API helpers reuse that browser context's Canyon session cookie; the CLI
  signs in once through M1's device flow and keeps its own config home. Nothing else knows how the
  provider looks.

## What the test cluster provides

| Variable | Holds |
|---|---|
| `E2E_CANYON_URL` | Canyon's public URL, `https://…`, no path |
| `E2E_APPS_DOMAIN` | the front door's `domain`; apps at `<app>.<preview\|prod>.<domain>` (form `app.env.domain`) |
| `E2E_OIDC_ISSUER` · `E2E_OIDC_CLIENT_ID` · `E2E_OIDC_CLIENT_SECRET` | what the admin types into the choice-2 front door form (A2) |
| `E2E_ALB_ARN` | what the admin types as `alb_arn` into the choice-1 form (A4); opaque to the suite |
| `E2E_ADMIN_EMAIL` · `E2E_ADMIN_PASSWORD` | the bootstrap admin (`CANYON_ADMINS` on the install) |
| `E2E_ALICE_EMAIL` · `E2E_ALICE_PASSWORD` · `E2E_BOB_EMAIL` · `E2E_BOB_PASSWORD` | the two makers |
| `E2E_KUBECONFIG` | a read-only kubeconfig (below) |
| `E2E_APPS_DB_HOST` · `E2E_CANYON_DB_HOST` | the apps database's address (5433) and Canyon's own (5432), for I2's probes |
| `E2E_SWITCH_FRONT_DOOR` | the hook: a script taking `alb` or `none` (below) |
| `E2E_DEPLOY_REPO_URL` · `E2E_DEPLOY_REPO_TOKEN` | the deploy repo's `https://` address and the token Canyon writes it with (H4 alone) |
| `E2E_DEPLOY_REPO_PREVIEW` · `E2E_DEPLOY_REPO_PROD` | its preview and production branches |
| `E2E_STORE_FETCH` · `E2E_DEPLOY_REPO_HOOK` | the hand-off hooks (below, *Hand-off through a deploy repo*) |

Passwords and the client secret are read once into memory, typed into the browser, and never
logged. **Playwright** records nothing: the `cluster` project sets `trace: "off"`, no video, no HAR
— a trace keeps every `fill` value and the provider's DOM, and Playwright has no redaction before
writing; the per-step screenshot is taken after each step, never on the provider's pages or the
front-door form. A fourth recorder is easy to miss: Playwright 1.55 writes the failed page's
accessibility tree — including typed field values — to `error-context.md` unless
`PLAYWRIGHT_NO_COPY_PROMPT=1`; `playwright.config.ts` sets it whenever `$E2E_CANYON_URL` is set, so
the cluster project never writes one either. **Go** never `t.Logf`s a value from the environment, and that is enforced, not
promised: the run's whole output (the `go test -v` stream and the Playwright report directory) is
grepped at the end for the value of every variable `secretVars` lists (`e2e/cluster/main_test.go`: the
client secret, the three passwords, K1's value, the deploy repo's token -- an explicit list a unit test keeps complete), and one hit fails
the run naming the file. K1 does the same for the app's own secrets. The test cluster puts the values in the
runner's environment from its secret store; nothing in this repo holds one.

**The identity provider** has three users — `admin`, `alice`, `bob` — whose addresses are the three
above, one OIDC client for Canyon (`$E2E_CANYON_URL/auth/callback`) and one for the apps (choice
2's `signin`, plus whatever the gateway of choice 1 uses), accepting these redirect URIs exactly:

| For | Redirect URIs |
|---|---|
| Canyon | `$E2E_CANYON_URL/auth/callback` |
| choice 2 (oauth2-proxy), each app and env | `https://e2e-app.preview.<domain>/oauth2/callback` · `https://e2e-app.prod.<domain>/oauth2/callback` · `https://e2e-peer.preview.<domain>/oauth2/callback` · `https://e2e-peer.prod.<domain>/oauth2/callback` |
| choice 1 (the gateway), each app and env | the same four hosts with `/oauth2/idpresponse` |
| wave 2 | `https://e2e-org.preview.<domain>/oauth2/idpresponse` · `…prod…` |
| hand-off through a deploy repo | `https://e2e-repo.preview.<domain>/oauth2/idpresponse` · `…prod…` |

The list is finite because the app names are fixed (next section). `e2e-vuln` and `e2e-cp` are
never deployed and need none.

**The read-only kubeconfig** is a ServiceAccount with `get`, `list`, `watch` on pods, pods/log,
deployments, replicasets, services, networkpolicies, configmaps, events, jobs and
persistentvolumeclaims in `canyon`, `canyon-apps`, `canyon-builds` and `canyon-scans`, `get`/`list`
on kpack `builds` in `canyon-builds`, and `create` on `selfsubjectaccessreviews` and
`subjectaccessreviews` (I4's `auth can-i --as`). For H4–H8 it also may `get`, by name alone, `e2e-repo`'s two namespaces
`canyon-e2e-repo-<preview|prod>`, their Deployments `e2e-repo-<env>` and their database claims `e2e-repo-<env>-db-1`,
which Argo CD makes and removes during a run. **No `secrets`, no `pods/exec`, no `create` on
anything else.** The suite asserts a Secret's *name* and its absence, never its content.

**The hook.** `$E2E_SWITCH_FRONT_DOOR alb` puts the test cluster's OIDC-authenticating listener rule in
front of the app addresses; `$E2E_SWITCH_FRONT_DOOR none` removes it, so the apps' 443 reaches the
router directly (choice 2's proxy signs people in). It exits 0 once the rule is live, else non-zero
with a line saying why; the suite calls it in A4 and at the end, and fails the phase on a non-zero
exit. It is the only thing the suite runs that it did not build.

**The egress gateway** is `deploy/k8s/egress.yaml`, in the base every overlay applies. A fresh install's server mints
a new policy certificate, so the runner publishes it and restarts the gateway after each one (`deploy/k8s/README.md`):
until then the gateway is not ready, and A5's apps have no way out.

**Fresh install per run.** The runner installs Canyon fresh — empty database, empty `canyon-apps`,
no `canyon-scandb-*` volume, no `canyon-trivy` server, no repository under `canyon/` in the registry — before every run, and
leaves it after. That is what makes A1 (onboarding), G3 (the first download) and fixed app names
(a deleted id is never free again) possible, and it is the recovery for a half-failed run: there is
nothing to clean by hand, the next run starts from nothing. Inside a run each phase's cleanup
(M7, M9) still runs so a failure in phase 2 does not leave apps for phase 3 to trip on: `t.Cleanup`
calls `forget` on every fixed name, and the hook's `none` runs from a deferred call.

## Hand-off through a deploy repo

H4–H8 (CASES.md) need the platform's side, which the test cluster sets up once and the suite never changes: Argo CD,
External Secrets and CloudNativePG; Canyon's reference set-up (`deploy/gitops`) applied from a platform repo
Canyon's token can't write; and the deploy repo's two branches, holding nothing but `apps/.canyon/route.yaml` and
`apps/.canyon/resources/postgres.yaml`, the test cluster's own route behind its gateway and its database. Two hooks are the
test cluster's, as `E2E_SWITCH_FRONT_DOOR` is:

- **`$E2E_STORE_FETCH`** reads the fetch-only token on stdin and puts it where External Secrets reads it (the
  Secret `canyon-fetch-token` in `external-secrets`). It never prints it, and exits 0 once it is stored.
- **`$E2E_DEPLOY_REPO_HOOK merge <url> <commit>`** merges, as a person and never with Canyon's token, the one pull
  request at `<url>`, and only while it is Canyon's (head `canyon/…`, opened by Canyon's machine user) into the
  production branch with `<commit>` still its head: the suite takes both from the app's audit trail, the `commit
  release` row Canyon wrote when it pushed. It prints `<url> <head> -> <base>`, and refuses anything else, and to
  run as Canyon's machine user. **`… remove <app>`** commits the removal of whatever is left under `apps/<app>` on
  each branch, never a force-push, and exits 0 when nothing is.

`run_by` is the org's, so H4 switches the whole Canyon to hand-off and the cleanup switches it back: the run
refuses to start unless `run_by` is `canyon` and the front door is choice 1 checked on this cluster, and a deploy
anyone else makes during the run is handed off too. The cleanup restores under contexts of its own, retrying, so
a case that ran out of time still restores. `go test` runs no cleanup when its own `-timeout` fires, so the run
starts no case unless that case's longest wait and the cleanup's 20 minutes both fit before it (the target's
`-timeout` is 120 minutes): it stops early and cleans up instead. A signal also skips the cleanup, so the test cluster's
runner restores `run_by` and `delivery` itself, whatever ends the run. The cleanup ends by replacing the fetch-only
token, so the one H4 stored opens nothing once the run is over.

## App names and redirect URIs

| App | Fixture | Deployed | Why the name is fixed |
|---|---|---|---|
| `e2e-app` | `f-cluster` | preview + prod, phase 2, deleted in M7 | its addresses are redirect URIs |
| `e2e-peer` | `f-cluster` | preview, phase 2 (I1's app B) and again under choice 1 in M8, deleted in M9 | same |
| `e2e-org` | `f-cluster` | wave 2 | same |
| `$E2E_HANDOFF_APP`, `e2e-repo` when unset | `f-cluster` | preview + prod, H4–H8, deleted in H8 | same, and the test cluster's target groups behind the gateway; Canyon never reuses a deleted name, so each run names one the test cluster has not used yet |
| `e2e-vuln` · `e2e-cp` | `f-vuln` · `f-cp` | never | checked only; forgotten at the end |
| `e2e-outbound-<random>` | `f-outbound` | preview, O1, deleted at its end | not fixed: nobody signs in to it, so it needs no redirect URI |

## Waits

Every timed assertion polls with a deadline through `waitFor`, extended to carry the last
observation: `waited 60s for bob to reach e2e-app: last answer 302 /#/no-access/e2e-app at
10:04:59`. A share reaches the app when the kubelet has synced the mounted ConfigMap — about once
a minute, plus its watch delay — and the sidecar re-read it, so the deadline is 120 s, not 60. The
deadlines: 120 s for a share or unshare to reach the app, 90 s for the front-door probe, 5 min for a
promotion, 15 min for a build, 30 min for the first scan (its database download).

## Order, parallelism, duration

Phases are serial; inside phase 2 the two deploys (`e2e-app`, `e2e-peer`) and the checks (G1 then
G2) run in parallel, so the executor's shared build/scan limit is the ceiling. Four builds in all
(M3, M6, M8, and `e2e-peer`'s first), every one cold: a fresh install has no cache image, and M7 and
M9 delete the app's repository with it. Wave 1 took 33 min on the AWS test cluster (EKS, two m7g.large
nodes, 2026-10-03): about 5 min for each build, 2½ min for M2's first scan with its database
download, 1 min for each share to reach the app. The Playwright journeys run one at a time, as on
Docker.

## Build

Each PR off `main`, ≤500 lines, in this order; each one's cases run green on the test cluster before the next.

| PR | Holds | Lines |
|---|---|---|
| 1 fixture | `f-cluster` in `gen.sh` (`[roles]`, `/whoami`, `/probe`); and the last loop of `gen.sh` tars `f-cp f-core f-clean f-vite f-unread f-mask` only — `f-vuln` and `f-sql` get no tarball, which this PR fixes so the GitHub journeys can use them too | ~150 |
| 2 Go harness | `$E2E_CANYON_URL` in place of the host, the session on every request, the device-flow sign-in, the kubectl surface (pods, ConfigMaps, Jobs, PVCs, SubjectAccessReviews), `waitFor` carrying the last observation, the end-of-run secret grep | ~450 |
| 3 Playwright harness | the `cluster` project (`baseURL`, `trace: "off"`), `signIn`, the pod surface reader in `surfaces.ts`, declared `pods` changes | ~350 |
| 4 admin | A1–A4 | ~350 |
| 5 maker | M1–M9 | ~450 |
| 6 sharing | S1–S4 | ~300 |
| 7 secrets and scanners | K1, G1, G2 | ~250 |
| 8 isolation and images | I1–I4, P1 (after #475) | ~300 |
| 9 run | `make e2e-cluster` (`TestSuite`, the order), `confirm-device.ts`, this file kept true; the workflow the test cluster's runner calls is the test cluster's | ~120 |
| 10 wave 2 | H1–H3, after design/HAND-OFF.md's PRs 1–8 | ~300 |

## Open questions, and what today's code cannot do

- **No fixture echoes the identity token.** `e2e/fixtures/gen.sh` makes `f-core` with `/db`,
  `/secret` and `/version` only; M4, S1–S3 and I1 need `/whoami` and `/probe`. PR 1.
- **The scan's busy wait is silent.** The gate re-asks a busy executor without a timeline line, so
  M2 proves the download by trivy's server, `canyon-trivy`, coming ready, not by what the person sees.
  Filed as a product gap, separately.
- **No planted-misconfiguration fixture.** `gate/` has must-fail trees for `sql`, `auth`, `secrets`
  and `deps` only; G2 uses `f-cp` until a checkov-shaped fixture exists.
- **Both harnesses are Docker-bound and unauthenticated.** `e2e/stack_test.go` hard-codes the host
  and sends no session; `e2e/ui/harness/seed.ts` and `surfaces.ts` read compose and `docker`;
  `e2e/ui/playwright.config.ts` fixes `baseURL`. PRs 2 and 3 make the host, the session and the
  surface reader environment-driven, with the Docker path unchanged.
- **`journey()` cannot run on the test cluster yet.** Its per-step snapshot reads the audit trail with no
  session (`e2e/ui/harness/surfaces.ts` `auditTrail`), which an authenticated host refuses, so the
  cluster cases (`e2e/ui/cluster/`) are plain tests with explicit sessions and no surface diff until
  the factory takes a session for the trail. They are picked by case id (`--grep ' A[123] '`: the pattern is matched against project, file and title joined by spaces, so an anchor at the start selects nothing); `make
  e2e-ui` runs the Docker projects alone, so a Docker run never reaches them.
- **M4's header check is dropped.** The fixture's `/whoami` prints no request headers but the front
  door's own credentials (`door:`, M8's check; `e2e/fixtures/gen.sh`), and the sidecar hands the app its identity token *as*
  `Authorization` by design (design/PERMISSIONS.md), so "no `Authorization` header the app could
  see" was false as written. CASES M4 now asserts what the page proves: the verified token.
- **Wave 2 waits for design/HAND-OFF.md's PRs 1–8.** H1–H3 are written against its stub org
  pipeline and are not runnable until it is merged.
