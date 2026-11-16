# The hand-off journey

Canyon under `run_by: org` (design/HAND-OFF.md), end to end on a real cluster that Canyon runs nothing
on. `journey_test.go` (build tag `handoff`) proves seven things; `repo_test.go`, below, the same through a deploy repo:

0. The admin sets hand-off up from the dashboard alone (#2439), clicked through in Chromium by
   `e2e/ui`'s `handoff` project in two parts. H1 saves the platform webhook, reads the secret and the host
   token from the dialog that shows them once, and saves the front door at choice 1. The journey starts
   the pipeline with those two values. H2 sends a test delivery, which the pipeline answers `202` once the
   signature verifies, then hands apps to the platform. The values pass through a file under the test's
   temporary directory; the project keeps no trace, video or screenshot.
1. `canyon deploy` ends `live at … · reported by your platform`.
2. `canyon share` reaches the app within a minute of the share.
2b. A pod of the release checks in (#2452): within three minutes the status read carries its
   `seen_at`, `canyon status` says `a pod checked in`, and the app page says it too (H3 in `e2e/ui`'s
   `handoff` project). The stale and never-seen lines take minutes of a pod's absence: the dashboard's
   and the CLI's own tests hold them.
3. The app's database is the platform's (#2432): the app declares `db = { type = "postgres" }`, the
   pipeline provides it on the org's own Postgres, Canyon makes no resource record or provisioned
   secret, the project read names the database's endpoint, and a note the app writes through the
   sidecar is in that database and is read back after a restart. Then, with Canyon's access route
   cut off from the sidecar, its container is stopped (`crictl stop` on the kind node) and started
   again by the kubelet in the same pod: it serves alice from the list it cached on its emptyDir,
   within the grace, and its log says so.
4. `canyon undeploy` ends with the environment removed, and the data kept. Before that, with the
   pipeline unreachable so the remove waits on its retries, the pod the platform has not taken down
   sends alice to no-access within 90 seconds: its sidecar's pull answers the deny-all file (#2450).
5. `canyon delete` sends a remove with `delete: true`; the pipeline drops the database and its role.

## What is real and what is not

| Piece | In the journey |
|---|---|
| `canyon-server` | Real. A subprocess on this machine, with its own Postgres, `CANYON_RUNTIME=kubernetes` and `CANYON_AUTH=oidc`: sharing needs sign-in. Its public URL is TLS on the kind network's gateway, where pods reach it. |
| The executor | A stub (`internal/executortest`). Its `/build` answers the app's image, `testdata/app` on busybox, which the test built and pushed to the cluster's registry. `make e2e` proves Canyon's build, not this journey. |
| The app | Real: `testdata/app`, a small Go server that keeps notes in the database its `DATABASE_URL` names. |
| The org's Postgres | Real: `postgres:16-alpine`, started by the test with Docker on the kind network's gateway address, where pods reach it. The pipeline provides each app's database and role on it; Canyon is never told of it and has no database driver of its own here. |
| `canyon` CLI | Real. Built from this tree, signed in with `canyon login` as the admin, the code approved over the API with the admin's browser session. |
| The dashboard | Real: the bundle `make e2e-handoff` builds into the server, driven by Playwright with the admin's session cookie, trusting the journey's CA by `ignoreHTTPSErrors`. |
| The org's pipeline | `./pipeline`, run on this machine. It verifies the webhook — a bad signature is `401`, a test delivery (`ping`) that verifies is `202` and nothing more — fetches the access token, provides the release's postgres resources on the org's Postgres (a database and role per app, environment and resource, `DATABASE_URL` in a Secret the app reads), runs the app with its sidecar in the `handoff-apps` namespace (the sidecar as the docs' reference pod spec runs it: no root, a read-only root file system, the access cache's emptyDir), and reports back with each database's endpoint. It answers the webhook `202` at once and does the work after. A remove keeps the data; a project delete's remove drops it. |
| `canyon-identity` | Real. The sidecar image `.github/kind/up.sh` built from this tree. It pulls its access file from Canyon. |
| Identity provider | The test's own, over TLS. Canyon signs the admin in through it. It also issues the ID tokens that front door choice 1 `oidc` puts in front of the app; the test sends alice's to the sidecar directly, as the org's gateway would. It checks nothing it is sent. |

One throwaway CA signs a certificate for the kind network's gateway address. Canyon's public URL, the
identity provider and the pipeline are served under that certificate. The server, the CLI, the pipeline
and the sidecar trust the CA.

## Run it

It needs Linux on amd64 with Docker, because `up.sh` fetches kind and kubectl for that platform, and
Node for the dashboard and Playwright's Chromium (`npx playwright install --with-deps chromium` in
`e2e/ui` once, for the browser's system libraries). CI runs it as a step of the kube workflow's Calico job.

```sh
.github/kind/up.sh calico
docker run -d --rm --name handoff-db -p 127.0.0.1:5432:5432 -e POSTGRES_PASSWORD=canyon postgres:16-alpine
HANDOFF_DATABASE_URL='postgres://postgres:canyon@127.0.0.1:5432/postgres?sslmode=disable' make e2e-handoff
```

| Variable | Default | What it is |
|---|---|---|
| `HANDOFF_DATABASE_URL` | none; the journey fails without it | Canyon's Postgres. Use an empty database. |
| `HANDOFF_HOST_IP` | `172.30.0.1` | Where a pod reaches this machine: the gateway of `up.sh`'s kind network |
| `HANDOFF_REGISTRY_PUSH` | `localhost:5001` | The cluster's registry, as this machine pushes to it |
| `HANDOFF_REGISTRY` | `172.30.0.200:5000` | The same registry, as the cluster pulls from it |
| `KUBECONFIG` | client-go's default | The cluster the test and the pipeline use |

## The journey through a deploy repo

`repo_test.go` (build tag `handoffrepo`) is the same hand-off under `delivery: repo` (design/HAND-OFF.md §3b),
with no pipeline: Canyon commits each release to a Gitea repository, Argo CD or Flux syncs it, External Secrets
fetches the access token, and CloudNativePG runs the app's database. `HANDOFF_GITOPS` picks the controller,
`argocd` (the default) or `flux`: the reference files applied, how the CA is trusted and what a failure prints
follow it, and every assertion is the same under both. The shared pieces are in `helpers_test.go`.

0. The test starts Gitea (`gitea/gitea` rootless, pinned) behind the journey's TLS, makes its user with a
   password made at run time, a write token for Canyon and a read token for the sync, and a repo `deploy` with
   `deploy/gitops/templates/.canyon` under `apps/.canyon` on `main` and `preview` branched from it, and a
   public repo `platform`, which Canyon's token can't write, holding every file of `deploy/gitops/namespaces`. In
   Chromium (`e2e/ui`'s `handoff` project, `repo.spec.ts`), R1 picks the deploy repo, saves it, stores the
   fetch-only token and the host token from the dialog, checks the repo and saves the front door. The test then
   applies `deploy/gitops` as it is in this tree, filled in (Argo CD's files, or Flux's with `${INTERVAL}` at 15s),
   and R2 hands apps to the platform.
1. `canyon deploy --create` commits to `preview`, and ends `live at … · a pod checked in`. Alice is shared in, and
   the app keeps a note in the database CloudNativePG runs. In its namespace, a server-side dry run of a plain
   ClusterIP Service is admitted, and one of a LoadBalancer, a NodePort and a ClusterIP with `externalIPs` is
   each refused by `deploy/gitops/admission/services.yaml` (#2476). The namespace, and production's after step 2,
   carries the namespaces chart's `canyon.dev/env` label, which the reference Gateway's listeners select on. kind
   runs no Gateway API implementation, so no route's status is read here; `.github/kind/gitops-check.sh`, run after
   the journey whatever its outcome, renders the chart's label for each environment, refuses any other, and validates the reference
   Gateway against Gateway API's CRDs. Under Flux it also renders the chart's Flux mode as helm-controller loads
   it, and checks the generator's account, the sync accounts and the admission policy as themselves on the cluster.
1b. The writer's probe (#2479), committed straight to `preview` through Gitea's API, never through Canyon, under a
   `canyon.json` naming another Canyon: a second app's ConfigMap is applied into its own namespace, made with no
   hand step; a kustomization file (`Kustomization`, which Argo CD's directory source skips) naming a remote base,
   served by a listener the test runs, is never fetched, while a pod proves the listener is reachable, and under
   Flux the app's sync applies that commit with the file left out; with `canyon.json` alone left, the ConfigMap is pruned; with it gone, so is the namespace. Under
   Flux, a namespace someone else made under an app's name is never taken over.
2. `canyon deploy --env prod` opens a pull request into `main`; merged in Gitea, it ends live the same way.
3. `canyon undeploy --env preview` commits preview's objects away, and the sync prunes the Deployment;
   the namespace, made from the platform repo, stays with the data.
4. `canyon delete` commits both directories away, production's through a pull request the test merges, and
   finishes; the sync removes production's Deployment, and each environment's last namespace goes with the data.

CI runs it in the kube workflow's `handoff-repo` job, on a pull request, once per controller: `up.sh calico`,
then `.github/kind/platform.sh argocd` or `flux`, which installs Gateway API's CRDs, CloudNativePG and External
Secrets (helm), then Argo CD's core install or Flux's install with the flags deploy/gitops needs, each at a
pinned release, checksum-verified.

```sh
.github/kind/up.sh calico && .github/kind/platform.sh flux
docker run -d --rm --name handoff-db -p 127.0.0.1:5432:5432 -e POSTGRES_PASSWORD=canyon postgres:16-alpine
HANDOFF_GITOPS=flux HANDOFF_DATABASE_URL='postgres://postgres:canyon@127.0.0.1:5432/postgres?sslmode=disable' make e2e-handoff-repo
```

## The pipeline on another cluster

The pipeline has nothing specific to kind. It loads its cluster the way client-go does by default, and
takes its namespace, listen address, apps domain, CA and the org's Postgres (`PIPELINE_DATABASE_URL`, as its admin; unset, it provides no databases) from `PIPELINE_*` variables (`pipeline/main.go`).
On the AWS test cluster, the server's NetworkPolicy (`deploy/k8s/server.yaml`) allows egress only to public
addresses on 443. A pipeline running inside the cluster is at a private address, so the server needs an
egress rule for it (`deploy/k8s/README.md`, *Where `canyon-server` may connect*). This journey does not
change the test cluster.
