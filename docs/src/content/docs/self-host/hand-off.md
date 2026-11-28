---
title: Your platform runs the apps
description: Hand Canyon's reviewed, built images to your own platform instead of Canyon running them on your cluster, through your deploy repo, which your Argo CD or Flux syncs, or through a webhook to your own pipeline.
---

This page is for platform teams who already run a deployment platform, such as Argo CD, Flux or
their own CI, and want it to run the apps Canyon reviews. After reading it, you can choose how
releases reach your platform, turn hand-off on, run each app with its sign-in sidecar, and tell when
a release is live.

By default Canyon runs every app it builds. With **hand-off**, your platform runs them instead.
Canyon still reviews the code, runs the scanners, applies your rules and approvals, builds the image
and pushes it to your registry. It creates no database or bucket: your platform provisions every one
the app declares. Then, in place of starting the app, it hands your platform a **release**: run this
image in this environment, with these resources, or remove what runs there. It hands it over one of
two ways: it commits the release's files to **your deploy repo**, which your Argo CD or Flux syncs, or
it posts the release to **your pipeline's webhook**. An environment is live only once Canyon knows the
exact image it reviewed runs.

This applies to previews and production alike. For how the rest of Canyon fits together, see
[How Canyon works](/concepts/how-canyon-works/).

The words this page uses:

- **Deploy repo:** a git repository your Argo CD or Flux syncs to your cluster. Canyon commits each
  release's files to it.
- **Platform webhook:** the `https` address on your side that receives releases.
- **Webhook secret:** the shared key Canyon signs each release with, so your platform can check it
  came from Canyon.
- **Host token:** a token Canyon gives your platform once. Your platform sends it to fetch each
  release's access token and to report back. It can do nothing else, and can't sign in to Canyon.
- **Fetch-only token:** a token Canyon gives your platform once, under the deploy repo. External
  Secrets sends it to fetch each release's access token, and it opens nothing else.
- **Sidecar:** `canyon-identity`, a small container Canyon provides that runs beside each app. It
  checks who is signed in and whether they may open the app, then passes the request on.
- **Check-in:** each time a sidecar pulls its app's access list from Canyon, which it does every
  minute. Canyon records when a pod of each release last checked in.
- **Front door:** how people reach apps on this Canyon, set in **Settings › App hosting**: the address
  apps answer at, and who signs people in before the app sees them.
- **Digest:** the `sha256:…` hash that names one exact image in a registry.

## How releases reach your platform: your repo (recommended) or a webhook

In **Settings › App hosting**, under **Your platform runs apps**, **How releases reach your platform**
offers two choices:

- **Canyon commits to your deploy repo (recommended):** "Canyon writes each release's files to the git
  repo your Argo CD or Flux syncs: previews are pushed, production is a pull request."
- **Canyon calls your webhook:** "Canyon posts each release to your own pipeline, which runs it and
  reports back."

| | Your deploy repo | A webhook |
|---|---|---|
| What Canyon sends | Each release's Kubernetes files, committed to `<path>/<app>/<env>/` in your git repo | A signed JSON release, posted to your pipeline |
| Preview | Pushed to the preview branch | Posted |
| Production | A pull request into the production branch, which your team merges | Posted |
| What your platform team builds | Nothing per release. Once, under Argo CD: three AppProjects, an ApplicationSet per branch, the namespaces' ApplicationSet and the chart it reads from your own platform repo, Argo CD's read-only credential for the repo, a ClusterSecretStore and the Secret that holds its fetch-only token, and templates for your route and each kind of database or bucket ([the full list](#what-your-platform-team-sets-up-once)). Flux's equivalent makes each app's namespace from the same chart | A receiver that checks the signature, fetches the access token, runs the app and the sidecar, provisions what the app declares and reports back |
| How the pod gets its access token | External Secrets fetches it with the fetch-only token | Your pipeline fetches it with the host token |
| Live when | The first check-in of a pod whose app answers, while the branch holds exactly the files Canyon committed | Your platform reports the image's digest running |
| Runs on | Kubernetes with Argo CD or Flux, and External Secrets | Any pipeline |

The deploy repo is the recommended way. Production changes are reviewed in git like any other change
to your platform, nothing on your side has to speak Canyon's API, and a deploy is proven by the app
itself running, not by a report. Choose the webhook when your platform isn't Argo CD or Flux on
Kubernetes, or when you already have a pipeline that should decide how apps run.

The choice is the `delivery` preference: `webhook` (the default) or `repo`. Choosing one in the
dashboard saves it. Only an admin can, and the change is in the [audit trail](/org/audit-trail/). It
applies from the next deploy or promotion: see
[Switching between a webhook and the repo](#switching-between-a-webhook-and-the-repo).

## What it needs

Hand-off is a setting on your organization's preferences, `run_by`: `canyon` (the default) or
`org`. Only an admin can change it, and Canyon refuses `org` unless all of these hold:

1. **Canyon runs on Kubernetes** (`CANYON_RUNTIME=kubernetes`, see the
   [configuration reference](/self-host/configuration/)). On Kubernetes, Canyon pushes every image
   to your registry by digest, which is what your platform pulls.
2. **The way releases reach your platform is saved**: under the deploy repo, **your deploy repo**;
   under a webhook, **the platform webhook**, the address releases are sent to.
3. **A front door is set** in **Settings › App hosting**.
4. **The front door is "Your sign-in proxy signs people in".** Your own gateway, such as an OIDC proxy or
   an AWS load balancer, signs people in, and the sidecar beside the app checks the token it passes
   on. "Canyon signs people in" isn't supported under hand-off.

It also refuses `run_by: org` while Canyon's server lacks one of two settings:

- **`CANYON_IMAGE_IDENTITY`**: the sidecar image every release puts in your pods. Set it to the
  same value the executor has: the `canyon-identity` image, by digest.
- **An `https` `CANYON_PUBLIC_URL`**: the sidecar pulls the app's access list from Canyon over
  `https` only, and won't start without it.

Each refusal is `409` with its own error: `not_kubernetes`, `identity_image_required`,
`https_required`, `webhook_required` (under a webhook), `repo_required` (under the deploy repo, with
the message "Save your deploy repo first."), `front_door_required` or `signin_unsupported`. The two
server settings' messages name the setting and say to restart the server with it. Until both are set,
**Settings › App hosting** tells admins "To hand apps off to your platform, Canyon's server still
needs:" and lists what is missing. While hand-off is on, changing `delivery` needs the other way saved
as well: `repo_required` or `webhook_required` otherwise, and nothing changes.

Saving the front door runs Canyon's check of its own cluster ([Saving it](/self-host/front-door/#saving-it)): whether the cluster enforces
NetworkPolicy, so an app's port can't be reached without signing in, and whether the app addresses
have a certificate. Your platform's cluster isn't the one checked, so under hand-off the check never
blocks a "Your sign-in proxy signs people in" front door, as long as the platform webhook or the deploy repo
is saved first. A front door that fails it is saved **not checked on this cluster**, and **Settings ›
App hosting** says so beside it, with what the check found. A check that can't finish at all refuses
the save and changes nothing while Canyon still runs the apps, so a passing failure never takes a
checked front door out of use. Your platform can run apps behind it. Canyon won't, and a
deploy Canyon would run behind it fails with "The front door hasn't passed Canyon's check of this
cluster, so Canyon won't run apps behind it."

**Before the first deploy, Canyon's own cluster must enforce NetworkPolicy.** Under hand-off Canyon
still builds every image, on its own cluster, and a build's network policy, which keeps it off private
and link-local addresses, only holds where the cluster enforces policy. Without it, an app's install
scripts could reach the node's cloud credentials, Canyon's database and the rest of your network. So
Canyon builds nothing on a cluster its check hasn't seen enforce NetworkPolicy, under hand-off as when
it runs the apps itself. The certificate doesn't matter for this: a front door saved *not checked on this
cluster* only for its certificate still deploys. Until the check sees enforcement, every deploy fails
before anything is built, with:

> Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.

It is the answer for a plugin that ignores policy, for one that lets a new pod out before its policy is
in force (the VPC CNI's default `standard` mode), and for a check that couldn't finish. On EKS, turn
enforcement on as [Strict mode on the AWS VPC CNI](/self-host/kubernetes/#strict-mode-on-the-aws-vpc-cni)
shows, then save the front door again: the save runs the check, and the next deploy builds. After
upgrading, every front door counts as unchecked until an admin saves it once.

Canyon doesn't watch the cluster after a save. If enforcement is turned off, the add-on goes back to
standard mode, or new nodes come up without the policy agent, builds go on until the front door is
saved again, and the check runs on one node. Give the nodes IMDSv2 with a hop limit of 1 as the
backstop, so a pod doesn't reach the node's role even where a policy is not in force.

If a server already in hand-off starts without either setting, it refuses to start, and its error
names each one. To have Canyon run the apps again instead, run
`UPDATE org_preferences SET run_by = 'canyon'` on Canyon's database and restart it, then save the
front door again: Canyon runs no app behind a front door that hasn't passed its check of this cluster.

## Turn it on

An admin sets hand-off up in **Settings › App hosting**, under **Where apps run**: choose **Your
platform runs apps**, which saves nothing yet. Under it, choose how releases reach your platform
(above): that choice is saved at once. A checklist of four steps opens, and its second step follows
the choice. Each change is in the [audit trail](/org/audit-trail/).

1. **Canyon's server settings.** Set `CANYON_IMAGE_IDENTITY` and an `https` `CANYON_PUBLIC_URL` on
   the server and restart it, then reload the page. Until both are set, this step lists what is
   missing.
2. **Your deploy repo**, when Canyon commits to your deploy repo. Fill in the form, its fields
   described in [The deploy repo's settings](#the-deploy-repos-settings): **Repository URL**, **User
   name**, **Token**, **Preview branch**, **Production branch**, **Production changes**, **Path in the
   repo**, **Pull requests API**, **API address**, **Secret store** and **CA for the sign-in sidecar
   and External Secrets (optional)**. Then choose **Save the deploy repo**.

   The first save opens a dialog, **Store these now**, that shows two values once. The **Fetch-only
   token**: "External Secrets fetches each release's access token with it, and it opens nothing else."
   And the **Host token**, the deploy repo's own, which your platform needs only to report a failure
   ([Reporting a failed sync](/self-host/hand-off-argo-cd/#reporting-a-failed-sync)). Copy each into
   your platform's secret store, tick the box, then choose **Done**. Canyon never shows either again.

   Then choose **Check the repo**. While it runs, the button reads **Checking…**. Canyon reads both
   branches, reads your templates and renders a sample app with them, pushes a throwaway test branch
   `canyon/check-<n>` and deletes it, and, when production goes through pull requests, lists the open
   ones. A pass reads "Canyon reached the repo, read both branches, pushed a test branch to it and
   rendered your templates.", then "Templates found: " and the files it found under `.canyon`. A check
   that finds a problem lists each one's sentence: see [Troubleshooting](#troubleshooting). Either way
   it says "This check can't prove your branch rules, that Canyon may open a pull request, or that Argo
   CD or Flux, External Secrets and your CA are set up: the first deploy does."

   The check pushes to a test branch, not to preview's own branch, so a branch rule on the preview
   branch that refuses Canyon's push shows at the first deploy, not here.

   The step is ticked once a check has passed, and says "Checked 2 minutes ago." (with the time since).
   Every save of the repo clears it, so check again after each save.

   Under the step, **For your platform team** opens with "Next, your platform team applies these once.
   The first deploy proves the set-up: it goes live when a pod checks in." It shows the files to apply,
   filled in for your Canyon (see [below](#what-your-platform-team-sets-up-once)).

   **Your platform's webhook**, when Canyon calls your webhook. Enter the `https` address your platform
   receives releases at and save it. A dialog shows the **webhook secret** and the **host token**, once:
   copy each into your platform's secret store, tick **I've stored both**, then choose **Done**. Canyon
   never shows either again. If one is lost, replace it (below).

   Once the webhook is saved, **Send a test delivery** is in this step. Canyon sends your platform a
   signed `ping` (see [The test delivery](#the-test-delivery)) and shows what it answered. "Your
   platform answered 202. Your platform is reachable." means the address and the secret are right. A
   `401` or `403` means your platform doesn't have the current webhook secret; a redirect means the
   address isn't the final one, since Canyon follows none. If nothing answers in 10 seconds, Canyon's
   server can't reach your platform: allow it, as [Install on Kubernetes](/self-host/kubernetes/)
   describes. The host token isn't used until the first release.
3. **Your sign-in proxy signs people in.** **Go to How people reach apps** takes you to the front door, further
   down the page. Set it to **Your sign-in proxy signs people in** and save it there: the step is ticked once
   it is saved and the check found NetworkPolicy enforced, whatever it found of the certificate. Until
   then the step stays open with the amber note saying why, and **Hand apps to your platform** waits for
   it. Save the webhook or the deploy repo first, so that a cluster that fails the check still saves it.
4. **Hand apps to your platform.** The button is on once the steps above are done. It asks first, then
   switches. If Canyon refuses, its reason shows under the button, and the steps are read again.

The next deploy or promotion is the first one Canyon hands to your platform. Under the deploy repo,
**Where apps run** then reads "Your platform runs apps. Canyon commits each release to" your repo's
address, and "Committed, waiting for the first check-in." until a release has gone live. Under a
webhook, it shows where releases go and the last release your platform answered. Either way it shows
**How long apps keep letting people in when they can't reach Canyon** (the access grace) and what your platform team needs. Every member can read it; only an admin can change
it.

Once a deploy repo is saved, every member also sees **For your platform team**, under the repo's step
and, once your platform runs apps, under **Where apps run**: "Canyon commits each release to
`<path>/<app>/<env>` in" your repo. Below it are this Canyon's ApplicationSets, the namespaces'
AppProject and ApplicationSet, the AppProjects, the ClusterSecretStore and the repository credential,
and the namespaces chart for your platform repo under "Into your platform repo, under `<path>`", all
filled in to copy, never with a token Canyon holds in them. Each file has a **Copy** button, and **Copy
all** copies every one.

Above the files are fields for what Canyon doesn't know: **Argo CD's read-only user**, **Your platform
repo**, **Its branch** and **Where it holds namespaces/**. "These fill in the files below, in this
browser alone: Canyon never receives them." Until a field is filled, each file shows the field's name
in its place. No field takes Argo CD's read-only token, so `argocd/repository.yaml` always shows
`<Argo CD's read-only token>`: under it, the card gives the one `kubectl` line that creates that
Secret and labels it, reading the token from its input, never from a file or the command line
([Argo CD's credential for the repo](/self-host/hand-off-argo-cd/#argo-cds-credential-for-the-repo)).

The files are the reference set-up this page and the [Argo CD](/self-host/hand-off-argo-cd/),
[Flux](/self-host/hand-off-flux/) and [External Secrets](/self-host/hand-off-external-secrets/) pages
explain.

### With the API

The same calls, for an admin who scripts the set-up, signed in as themselves. The examples below use
the admin's CLI sign-in: after `canyon login`, the CLI keeps its session in
`~/.config/canyon/config.json`, and Canyon reads it from the `__Host-canyon_session` cookie. Signing in
for the API, and which calls need the dashboard instead, is in [Calling Canyon's API](/reference/api/).

```sh
CANYON_URL=https://canyon.example.com
session=$(jq -r .session ~/.config/canyon/config.json)
```

1. Set `CANYON_IMAGE_IDENTITY` and an `https` `CANYON_PUBLIC_URL` on the server, and restart it.
2. Save how releases reach your platform.

   **Under the deploy repo**, choose it, save the repo and check it. The token is read from an
   environment variable, so it isn't in your shell's history:

   ```sh
   curl -sS -X PUT "$CANYON_URL/api/v1/preferences" -b "__Host-canyon_session=$session" \
     -H 'Content-Type: application/json' -d '{"delivery": "repo"}'

   jq -n --arg token "$REPO_TOKEN" '{url: "https://github.com/example/deploy", token: $token,
     preview: {branch: "preview"}, prod: {branch: "main", mode: "pull_request"}, root: "apps"}' |
   curl -sS -X PUT "$CANYON_URL/api/v1/platform-repo" -b "__Host-canyon_session=$session" \
     -H 'Content-Type: application/json' -d @-
   ```

   You should see `200` and the saved repo, never its token, with `fetch_token` and `token` (the deploy
   repo's host token) on this first save only. Store them in your platform's secret store now: Canyon
   never shows either again. Then check the repo:

   ```sh
   curl -sS -X POST "$CANYON_URL/api/v1/platform-repo/check" -b "__Host-canyon_session=$session"
   ```

   You should see `200` and `{"passed": true, "problems": [], "templates": [".canyon/resources/postgres.yaml", ".canyon/route.yaml"]}`.
   When `passed` is `false`, `problems` lists each `{code, message}`: see
   [Troubleshooting](#troubleshooting).

   **Under a webhook**, save the platform webhook:

   ```sh
   curl -sS -X PUT "$CANYON_URL/api/v1/platform-webhook" -b "__Host-canyon_session=$session" \
     -H 'Content-Type: application/json' -d '{"url": "https://deploy.example.com/canyon"}'
   ```

   You should see `200` and a JSON object with `url`, `set_by` and `set_at`, and, on this first
   save only, `secret` (the webhook secret) and `token` (the host token). Store both in your
   platform's secret store now: Canyon never shows either again.
3. In **Settings › App hosting**, set the front door to **Your sign-in proxy signs people in**.
4. Under a webhook, send a test delivery:

   ```sh
   curl -sS -X POST "$CANYON_URL/api/v1/platform-webhook/ping" -b "__Host-canyon_session=$session"
   ```

   You should see `200` and `{"delivery": "…", "sent_at": "…", "status": 202}`, `status` being what
   your platform answered, or `0` when nothing answered.
5. Turn hand-off on:

   ```sh
   curl -sS -X PUT "$CANYON_URL/api/v1/preferences" -b "__Host-canyon_session=$session" \
     -H 'Content-Type: application/json' -d '{"run_by": "org"}'
   ```

   You should see `200` and the org's preferences, with `"run_by": "org"`. If something is missing,
   you see `409` and `{"error": "…", "message": "…"}`, where `error` is one of the codes above,
   such as `repo_required`, and `message` says what to do. The next deploy or promotion is the
   first one Canyon hands to your platform. The API needs the repo saved, not checked: check it
   anyway, since the dashboard's checklist won't tick the step until a check passes.

The webhook's calls and the preferences:

| Call | What it does |
|---|---|
| `PUT /api/v1/platform-webhook` `{url, secret?}` | Saves where releases go. `url` must be `https://`. `secret` is 1 to 1024 printable ASCII characters, with no spaces; leave it out on the first save and Canyon makes one. The response carries a secret Canyon made, and on the first save the **host token**, once each: Canyon never shows either again. A later save without `secret` keeps the stored one. |
| `GET /api/v1/platform-webhook` | Any member: `url`, who saved it and when, and `last_delivery`, the newest attempt to deliver a release since it was saved, with the status your platform answered (`0` for none), or `null`. Never the secret or the token. `null` before a webhook is saved. |
| `POST /api/v1/platform-webhook/token` | Replaces the host token and answers the new one, once. The old one stops working at once. |
| `POST /api/v1/platform-webhook/secret` | Replaces the webhook secret with one Canyon makes and answers it, once. Every release after it is signed with the new one. The address stays; who saved the webhook and when move to this replace, so the last delivery starts again. |
| `POST /api/v1/platform-webhook/ping` | Sends a [test delivery](#the-test-delivery) to the saved address and answers `{delivery, sent_at, status}`. One at a time: a second while one is in flight answers `429`. It's not a release, isn't recorded and isn't the last delivery. |
| `PUT /api/v1/preferences` `{run_by, delivery, access_grace}` | Turns hand-off on (`"org"`) or off (`"canyon"`), chooses how releases reach your platform (`"repo"` or `"webhook"`), and sets `access_grace`, described [below](#when-canyon-cant-be-reached). |

The deploy repo's calls are in [Its calls](#its-calls). Every answer that carries a secret or a token
is sent with `Cache-Control: no-store`.

### Replacing the token or the secret

If the host token or the webhook secret is lost or leaked, replace it. In **Where apps run**, choose
**Replace the host token** or **Replace the webhook secret**; with the API, call `POST …/token` or
`POST …/secret`. Each asks first, then shows the new value once, as the first save does. The old one
stops working at once:

- With a new host token, your platform can't report or fetch access tokens until it has it.
- With a new webhook secret, your platform refuses every release until it checks them with it,
  including a release Canyon is retrying at that moment. Send a test delivery once your platform has
  the new secret.

The deploy repo keeps a host token of its own, apart from the webhook's: a Canyon that saved both has
two, and reports and access-token fetches take either. The repo's step has three replace buttons:

- **Replace the repo token** asks "Replace the repo token? Canyon commits with the new one from now
  on." and takes the new token in a field below the question (`POST /api/v1/platform-repo/token`).
  Canyon never shows it, before or after.
- **Replace the host token** asks "Replace the host token? The current one stops working at once: your
  platform reports with the new one from now on." and shows the new one once, in the same dialog as
  the first save (`POST /api/v1/platform-repo/host-token`). It replaces the deploy repo's host token
  alone; the webhook's is replaced under **Where apps run**, as above.
- **Replace the fetch-only token** asks "Replace the fetch-only token? The current one stops working
  at once: External Secrets can't fetch access tokens until it has the new one." and shows the new one
  once, in the same dialog as the first save (`POST /api/v1/platform-repo/fetch-token`). See
  [Rotating the fetch-only token](/self-host/hand-off-external-secrets/#rotating-the-fetch-only-token).

### Switching back

The setting applies from the next deploy or promotion. An environment keeps being run by whoever
deployed it, Canyon or your platform, until its next deploy replaces it or it is undeployed. Nothing
moves when you switch, data included: a database Canyon made for an app before the switch stays where
it is, and your platform provisions its own, which starts empty. Move the data yourself if the app
needs it. Until the app is deleted, the app page lists both, Canyon's and your platform's.

To have Canyon run apps again, choose **Canyon runs apps on this cluster** under **Where apps run**,
which asks first, or send `{"run_by": "canyon"}`. Canyon refuses it with `409 front_door_unchecked`
while the front door hasn't passed its check of this cluster. Save **How people reach apps** again
so Canyon checks it, then switch.

## Your deploy repo

Under the deploy repo, Canyon commits each release's files to a git repository your Argo CD or Flux
syncs. Preview is pushed to its branch; production is a pull request into its branch, which your team
merges. External Secrets brings each release's access token into its namespace. Once a pod of the
release checks in, its app answering and the branch holding exactly what Canyon committed, the
environment is live.

```diagram kind=flow title="A deploy through your deploy repo: Canyon commits, Argo CD or Flux syncs, the first check-in makes it live"
steps:
  - {who: You or your coding agent, icon: agent, step: 1, what: "`canyon deploy`"}
  - {who: Canyon, icon: canyon, tone: canyon, step: 2, what: "Reviews, builds, and pushes the image to your registry"}
  - {who: Canyon, icon: canyon, tone: canyon, step: 3, what: "Commits the release's files to your deploy repo", more: "Preview to its branch; production as a pull request into its branch, which your team merges."}
  - {who: Argo CD or Flux, step: 4, what: Syncs the branch}
  - {who: External Secrets, icon: key, step: 5, what: "Fetches the release's access token from Canyon", more: "With the fetch-only token."}
  - {who: Argo CD or Flux, step: 6, what: Runs the app and the sidecar}
  - {who: The sidecar beside the app, icon: canyon, tone: canyon, step: 7, what: "Pulls the access list from Canyon", more: "Its first check-in: the app answered."}
  - {who: Canyon, icon: canyon, tone: canyon, step: 8, what: "Checks the branch's tree is what it committed"}
  - {who: Live, icon: passed, tone: passed, step: 9, what: "Live at the route's address", more: "Canyon tells you or your agent: a pod checked in."}
```

### What your platform team sets up once

Everything but the templates goes outside the deploy repo: whoever can write the deploy repo must not
be able to change what may be deployed from it. The reference set-up is `deploy/gitops` in Canyon's
source, and **For your platform team** in **Settings › App hosting** shows it filled in for your
Canyon. Under Argo CD, it is all of this:

- **In the deploy repo:** a preview branch and a production branch, holding nothing but Canyon's path
  (`apps` unless you choose another), and **the templates**, committed to **both** branches under
  `<path>/.canyon/`: how each app is reached (`route.yaml`) and how each kind of database or bucket is
  made (`resources/<type>.yaml`). Canyon writes only `<path>/<app>/<env>/` and branches under
  `canyon/`, and reads the templates from the branch it writes. See [Templates](#templates).
- **In your own platform repo**, the one Canyon's token can't write: **the namespaces chart**, a copy
  of `deploy/gitops/namespaces`. It is the step a first set-up most often misses: without it, no app's
  namespace is made, and no app goes live. See [Namespaces](/self-host/hand-off-argo-cd/#namespaces).
- **In Argo CD's namespace** ([Argo CD](/self-host/hand-off-argo-cd/)):
  - three **AppProjects**: `canyon-preview` and `canyon-prod` for the apps, and `canyon-namespaces`
    for their namespaces;
  - the two apps' **ApplicationSets**, `canyon-preview` and `canyon-prod`, one per branch;
  - the namespaces' **ApplicationSet**, `canyon-namespaces`, which reads the chart from your platform
    repo;
  - **the repository credential**, the Secret `canyon-deploy-repo`: Argo CD's own read-only token for
    the deploy repo, never Canyon's.
- **Once per cluster:** the **admission policy on the apps' Services**, which keeps every Service in
  an app's namespace `ClusterIP`, never a load balancer, a node port or an external IP
  ([The apps' Services](/self-host/hand-off-argo-cd/#the-apps-services)). The cluster must run
  **Kubernetes 1.30 or later**, where ValidatingAdmissionPolicy, the API the policy uses, is generally
  available.
- **For External Secrets** ([External Secrets](/self-host/hand-off-external-secrets/)): the
  **ClusterSecretStore** each release's access token comes through, and the **Secret
  `canyon-fetch-token`** in `external-secrets`, key `token`, labelled `external-secrets.io/type:
  webhook`, holding the fetch-only token.
- **On your git host:** a machine user and its token for Canyon, with write access to that one
  repository ([A GitHub token for Canyon](#a-github-token-for-canyon)), and **branch rules** on
  production ([Protecting the branches](#protecting-the-branches)).
- **For each app:** its runtime values, in a Secret `<app>-<env>-vars` in its namespace (see
  [What Canyon commits](#what-canyon-commits)).

Under [Flux](/self-host/hand-off-flux/), its GitRepositories, an ArtifactGenerator and a HelmRelease
per environment, their accounts and the admission policy on the generator take the place of the
AppProjects and ApplicationSets. Each app's namespace still comes from the same chart in your platform
repo, with no hand step.

#### From zero, in order

1. **Install what the set-up runs on:** a cluster on Kubernetes 1.30 or later, Argo CD (or Flux),
   External Secrets, your database operator
   (CloudNativePG for the reference `postgres.yaml`) and whatever your `route.yaml` needs, such as a
   Gateway API gateway with one listener per environment
   ([The gateway](/self-host/hand-off-argo-cd/#the-gateway)). Canyon's own cluster, where it builds
   every image, must enforce NetworkPolicy, or no deploy builds
   ([What it needs](#what-it-needs)).
2. **Make the deploy repo**, with its two branches and the templates on both, and **Canyon's machine
   user and token**, with the branch rules on production.
3. **Save the repo in Canyon and check it**, under **Your deploy repo** in **Settings › App hosting**
   ([Turn it on](#turn-it-on)). The first save shows the fetch-only token and the host token, once:
   store them. **For your platform team** now shows the files below, filled in for your Canyon.
4. **Put the fetch-only token in the Secret `canyon-fetch-token`**, labelled
   `external-secrets.io/type: webhook` ([The fetch-only token](/self-host/hand-off-external-secrets/#the-fetch-only-token)).
5. **Copy the namespaces chart into your platform repo**, and push it
   ([Namespaces](/self-host/hand-off-argo-cd/#namespaces); under Flux,
   [The chart's Flux mode](/self-host/hand-off-flux/#the-charts-flux-mode)).
6. **Apply, in Argo CD's namespace,** the AppProjects, the ApplicationSets, the namespaces' AppProject
   and ApplicationSet and the repository credential, and **apply the admission policy and the
   ClusterSecretStore** ([Argo CD](/self-host/hand-off-argo-cd/),
   [The apps' Services](/self-host/hand-off-argo-cd/#the-apps-services),
   [External Secrets](/self-host/hand-off-external-secrets/#the-clustersecretstore)).
   Under Flux, apply its set-up instead of Argo CD's, with the flags it needs
   ([Flux](/self-host/hand-off-flux/#what-to-install)).
7. **Set the front door and hand apps to your platform**: steps 3 and 4 of [Turn it on](#turn-it-on).
8. **Deploy an app.** The first deploy proves the set-up: Canyon commits, Argo CD makes the namespace
   and syncs the app, External Secrets brings its access token, and the environment goes live when a
   pod checks in. An app with runtime values waits for its `<app>-<env>-vars` until you make it in the
   new namespace. If nothing has checked in after ten minutes, the deploy says so: follow
   [Troubleshooting](#troubleshooting).

### The deploy repo's settings

| Field | In the API | What it is |
|---|---|---|
| **Repository URL** | `url` | The repo's `https://` address, with no user name, password, query or fragment, and a lower-case host. With GitHub's pull requests API it is exactly `https://<host>/<owner>/<repo>`; with GitLab's merge requests, `https://<host>/<group>/<project>`, with any subgroups between; either with `.git` or without. Git runs over `https` alone. |
| **User name** | `username` | The user name sent with the token. `x-access-token` when left empty, which a GitHub token takes; on Gitea or Forgejo, the machine user's name. GitLab takes any name with a token, so `x-access-token` works there too. |
| **Token** | `token` | "A token for this one repository: on GitHub, a fine-grained token with Contents and Pull requests read and write. Canyon never shows it again." On Gitea or Forgejo, an access token with read and write on the repository; on GitLab, a project access token with the Developer role and the `api` and `write_repository` scopes ([On GitLab](#on-gitlab)). Once saved, the form says "A token is set. Canyon never shows it." |
| **Preview branch** | `preview.branch` | Where previews go. Canyon always pushes it. |
| **Production branch** | `prod.branch` | Where production goes. Neither branch may be under `canyon/`. |
| **Production changes** | `prod.mode` | **Through a pull request** (`pull_request`, the default) or **Pushed to the branch** (`push`). |
| **Path in the repo** | `root` | `apps` when left empty: lower-case segments. "Canyon writes each app under `<path>/<app>/<env>`, and reads your templates from `<path>/.canyon`." |
| **Pull requests API** | `api` | **GitHub's (GitHub, GitHub Enterprise, Gitea, Forgejo)** (`github`, the default), **GitLab's merge requests (GitLab.com, GitLab self-managed)** (`gitlab`: [On GitLab](#on-gitlab)) or **None** (`none`). With **None**, production is pushed, and the form says in amber, before you save: "With no pull requests API, Canyon pushes production to its branch with no review in git." |
| **API address** | `api_url` | Where that API answers: `https://api.github.com` when left empty; `https://<host>/api/v3` on GitHub Enterprise Server; `https://<host>/api/v1` on Gitea and Forgejo. On GitLab, `https://<host>/api/v4` on the repository's own host when left empty: always on that host and port, ending `/api/v4`, with any path before it the start of the repository URL's path (a GitLab served under `/gitlab`). |
| **Secret store** | `store` | The name of the ClusterSecretStore each release's access token comes through: `canyon` when left empty. |
| **Your private certificate authority, if one signed Canyon's address (optional)** | `ca` | "Only when a private CA signed Canyon's own address: the sign-in sidecar in each pod and External Secrets trust it. A git host on a private CA needs its CA in Canyon's server instead: A git host at a private address, under Your deploy repo in Canyon's docs." PEM, up to 64 KiB. Canyon then commits it in a ConfigMap beside each app, and **For your platform team** puts it in the ClusterSecretStore's `caBundle`. It is never the git host's CA: see [A git host at a private address](#a-git-host-at-a-private-address). |

### Its calls

| Call | What it does |
|---|---|
| `GET /api/v1/platform-repo` | Any member: the settings above but the token, who saved them and when, `checked_at` (the last passing check since the last save, or `null`) and `last_commit`, the newest repo release's `{release, env, state, at, commit?, pull_request?, problem?}`, or `null`. `null` before a repo is saved. |
| `PUT /api/v1/platform-repo` `{url, username?, token?, preview, prod, root?, api?, api_url?, store?, ca?}` | Admin only. Saves the repo and answers the read's shape. The first save answers, once, `fetch_token` and `token`, the deploy repo's own host token. `token` is required on the first save; later it is kept when left out. A save that moves `url` or `api_url` to another host, or changes `ca`, must carry `token` again, or it is refused `400 invalid_token`: the stored token never follows an address to a host, or a trust, it wasn't given for. Each save clears `checked_at`. |
| `POST /api/v1/platform-repo/check` | Admin only. Runs the check and answers `{passed, problems, templates}`. One at a time: a second while one runs answers `429 check_in_flight`, "A check is running already. Try again once it answers." |
| `POST /api/v1/platform-repo/token` `{token}` | Admin only. Replaces the repo token and answers who replaced it and when, never the token. |
| `POST /api/v1/platform-repo/fetch-token` | Admin only. Makes a new fetch-only token and answers it, once, as `fetch_token`. The old one stops working at once. |
| `POST /api/v1/platform-repo/host-token` | Admin only. Makes a new host token for the deploy repo and answers it, once, as `token`. The repo's old one stops working at once; the webhook's, if one is saved, is untouched. |

A field outside its shape is refused `400` with one of `invalid_repo_url`, `invalid_username`,
`invalid_token`, `invalid_branch`, `invalid_mode` (`push` is the only mode with no pull requests API),
`invalid_root`, `invalid_api` (an API other than the three, or GitLab's on another host than the repository's), `invalid_store` or `invalid_ca`, and nothing is saved. A member who isn't
an admin gets `403 admin_required`. Each write is in the audit trail, never with the token, the user
name or the CA's bytes.

### What Canyon commits

For each release, Canyon writes `<path>/<app>/<env>/` for the namespace `canyon-<app>-<env>`. The
`canyon-` prefix is fixed, and the whole name is at most 63 characters: a longer one refuses the
release before any git call, "`<namespace>` is longer than 63 characters, the most a namespace takes:
a shorter app name fits." So an app's id fits in 48 characters for preview and 51 for production.

| File | What it is |
|---|---|
| `deployment.yaml` | The Deployment `<app>-<env>`: the app, by its image's digest, and the sign-in sidecar as a native init container, by digest too, with the release's settings, the security contexts and resources Canyon gives apps it runs itself, and a readiness probe on the app's port, 3000. The pod is pinned to nodes of the image's architecture, the release's `platform` (`nodeSelector: {kubernetes.io/arch: arm64}`, say), so a cluster with nodes of both architectures runs it where it can start. The sidecar caches the access list on an `emptyDir` ([When Canyon can't be reached](#when-canyon-cant-be-reached)). |
| `service.yaml` | The Service `<app>-<env>`, port 4180, the sidecar's. |
| `networkpolicy.yaml` | The NetworkPolicy `<app>-<env>`: traffic in to port 4180 alone. It says nothing about traffic out: that is yours. |
| `externalsecret.yaml` | The ExternalSecret that makes the Secret `<app>-<env>-access-<hex>` holding `CANYON_ACCESS_TOKEN`, from the store you named. Your own `<path>/.canyon/externalsecret.yaml` replaces it ([External Secrets](/self-host/hand-off-external-secrets/#using-another-secret-mechanism)). |
| `route.yaml` | Your `route.yaml` template, rendered. |
| `resources/<name>.yaml` | Your template for each database or bucket the app declares, rendered. |
| `ca.yaml` | The ConfigMap `canyon-ca` with Canyon's CA, only when it is set. |
| `canyon.json` | `{canyon, app, env, release, gate_run, package, version}`: what Argo CD's files generator and Flux's namespaces generator find. It binds nothing: the namespace and the project come from the path. |

Every object carries `metadata.namespace` `canyon-<app>-<env>`, and every file is serialized from
parsed values, keys sorted. Canyon commits **no Namespace**, and neither does anything else in the
deploy repo: it comes from your platform repo, so a token that can write the deploy repo can never
choose a namespace's pod security. Under Argo CD a second ApplicationSet makes it
([Namespaces](/self-host/hand-off-argo-cd/#namespaces)); under Flux, a HelmRelease renders the same
chart for each app it finds ([Flux](/self-host/hand-off-flux/#the-namespaces-release)).

The pod reads three kinds of Secret by name, none of them optional, so a value that isn't there never
starts the app:

- **`<app>-<env>-access-<hex>`**, key `CANYON_ACCESS_TOKEN`: the release's access token, `<hex>` being
  the release id after `rel_`. A new name each release, so every deploy rolls the pod. External
  Secrets makes it.
- **`<app>-<env>-res-<name>`**: what the resource `<name>` gives the app, such as `DATABASE_URL`. Your
  resource template makes it.
- **`<app>-<env>-vars`**: the app's runtime values, one key per name, such as `STRIPE_SECRET_KEY`.
  Canyon never sends a value and no template may make this Secret: your platform makes it in the
  namespace from your own secret store. `deployment.yaml` names each key it reads.

Nothing secret is ever in git. This is what Canyon commits for `claims-portal`'s preview at `v1.4`,
with the reference templates:

```text
apps/claims-portal/preview/
├── canyon.json
├── deployment.yaml
├── externalsecret.yaml
├── networkpolicy.yaml
├── resources/
│   └── db.yaml
├── route.yaml
└── service.yaml
```

```json
{
  "canyon": "https://canyon.example.com",
  "app": "claims-portal",
  "env": "preview",
  "release": "rel_8f2a",
  "gate_run": "gr_8f2a",
  "package": "pkg_9e40",
  "version": "v1.4"
}
```

```yaml
# deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    canyon.app: claims-portal
    canyon.target: preview
  name: claims-portal-preview
  namespace: canyon-claims-portal-preview
spec:
  minReadySeconds: 10
  progressDeadlineSeconds: 300
  replicas: 1
  revisionHistoryLimit: 0
  selector:
    matchLabels:
      canyon.app: claims-portal
      canyon.target: preview
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      annotations:
        canyon.dev/gate-run: gr_8f2a
        canyon.dev/package: pkg_9e40
        canyon.dev/release: rel_8f2a
      labels:
        canyon.app: claims-portal
        canyon.target: preview
    spec:
      automountServiceAccountToken: false
      containers:
      - env:
        - name: PORT
          value: "3000"
        - name: CANYON_IDENTITY_ISSUER
          value: https://canyon.example.com
        - name: CANYON_IDENTITY_AUDIENCE
          value: claims-portal
        - name: CANYON_IDENTITY_JWKS_URL
          value: http://127.0.0.1:4182/.well-known/jwks.json
        - name: STRIPE_SECRET_KEY
          valueFrom:
            secretKeyRef:
              key: STRIPE_SECRET_KEY
              name: claims-portal-preview-vars
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              key: DATABASE_URL
              name: claims-portal-preview-res-db
        image: registry.example.com/canyon/claims-portal@sha256:<64 hex>
        name: app
        readinessProbe:
          periodSeconds: 1
          tcpSocket:
            port: 3000
        resources:
          limits:
            cpu: "1"
            memory: 1Gi
          requests:
            cpu: 10m
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
        terminationMessagePolicy: FallbackToLogsOnError
      enableServiceLinks: false
      initContainers:
      - env:
        - name: CANYON_ACCESS_GRACE
          value: 1h
        - name: CANYON_ACCESS_URL
          value: https://canyon.example.com/access/claims-portal/preview
        - name: CANYON_FRONT_DOOR
          value: '{"choice":1,"gateway":"oidc","issuer":"https://login.example.com","audience":"canyon-gateway","jwks_url":"https://login.example.com/keys","cookie":"_pomerium","domain":"canyon.example.com","form":"app.env.domain"}'
        - name: CANYON_IDENTITY_AUDIENCE
          value: claims-portal
        - name: CANYON_IDENTITY_ISSUER
          value: https://canyon.example.com
        - name: CANYON_TARGET
          value: preview
        - name: CANYON_ACCESS_TOKEN
          valueFrom:
            secretKeyRef:
              key: CANYON_ACCESS_TOKEN
              name: claims-portal-preview-access-8f2a
        image: registry.example.com/canyon-identity@sha256:<64 hex>
        name: canyon-identity
        resources:
          limits:
            cpu: 100m
            memory: 64Mi
          requests:
            cpu: 10m
            memory: 32Mi
        restartPolicy: Always
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
          runAsNonRoot: true
          seccompProfile:
            type: RuntimeDefault
        terminationMessagePolicy: FallbackToLogsOnError
        volumeMounts:
        - mountPath: /var/cache/canyon
          name: access-cache
      volumes:
      - emptyDir: {}
        name: access-cache
```

```yaml
# externalsecret.yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  labels:
    canyon.app: claims-portal
    canyon.target: preview
  name: claims-portal-preview-access-8f2a
  namespace: canyon-claims-portal-preview
spec:
  data:
  - remoteRef:
      key: rel_8f2a
    secretKey: CANYON_ACCESS_TOKEN
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: canyon
  target:
    creationPolicy: Owner
    name: claims-portal-preview-access-8f2a
```

The sidecar in each pod works as [Running the app](#running-the-app) describes: it checks the token
your gateway signed people in with and the app's access list, and passes the request on with Canyon's
identity token.

### Templates

Your templates say how an app is reached and how its databases and buckets are made, in your
platform's own kinds. They live in your repo, under `<path>/.canyon/`, and Canyon reads them from the
branch it is writing, so commit them to the preview branch and the production branch alike:

| Template | When |
|---|---|
| `route.yaml` | Always. One or more objects, exactly one of them annotated `canyon.dev/address`: the app's `https` address, which Canyon shows as where it is live. |
| `resources/<type>.yaml` | One for each type an app declares, `postgres` or `s3`. One or more objects, exactly one annotated `canyon.dev/endpoint`: where the resource is, in the shape [a report's endpoint](#reporting-where-they-are) takes. They make the Secret `${secret}`, holding each variable the resource gives. |
| `externalsecret.yaml` | Optional. Replaces Canyon's ExternalSecret, for a platform that brings Secrets another way. |

Canyon reads both annotations back from the bytes it commits. Whoever can edit the templates can
already commit any YAML to the repo, so templates grant nothing new, and each commit's
`Canyon-Templates` trailer names the tree of templates it used.

These variables are replaced inside scalar values, never in a key, each with a string:

| Variable | Its value |
|---|---|
| `${app}` | The app's id, such as `claims-portal` |
| `${env}` | `preview` or `prod` |
| `${namespace}` | `canyon-<app>-<env>` |
| `${name}` | The resource's name, such as `db`; the app's id in `route.yaml` and `externalsecret.yaml` |
| `${secret}` | The Secret the template must make: `<app>-<env>-res-<name>` for a resource, `<app>-<env>-access-<hex>` in `externalsecret.yaml`, `""` in `route.yaml` |
| `${release}` | The release's id, such as `rel_8f2a` |
| `${store}` | The **Secret store** you named |
| `${service}` | `<app>-<env>`: the Service, and the Deployment |

Any other `${…}` is refused. Canyon parses each template as YAML before it substitutes anything, so an
app's name never becomes YAML. A template is refused, and the release with it, for the first of these
it fails, and the deploy's last line says "`<file>`: `<reason>`. Ask your platform team." with the
reason in Canyon's words, never quoting the template:

- "it is over 64 KiB"
- "it doesn't parse as YAML"
- "it makes more than 10 objects" (documents counted, an empty one too)
- "it uses an anchor or an alias"
- "it uses a merge key"
- "it repeats a key"
- "it uses a tag outside YAML's core schema"
- "it puts ${…} in a key"
- "it uses a variable Canyon doesn't have"
- "an object has no apiVersion, kind or metadata.name"
- "it sets another namespace" (a template that sets none gets the release's)
- "it makes a cluster-scoped object", such as a Namespace, a ClusterRole or a ClusterSecretStore
- "it makes an object Canyon makes, or one another template makes" (or `<app>-<env>-vars`, or an
  `<app>-<env>-access-…` Secret outside `externalsecret.yaml`)
- "it has no canyon.dev/address, or more than one"
- "its canyon.dev/address isn't an https address"
- "it has no canyon.dev/endpoint, or more than one"
- "its canyon.dev/endpoint isn't a `<type>` address"

A type an app declares with no template, or no `route.yaml`, is "`<file>` is missing: Canyon needs it
to render this app. Ask your platform team." Canyon checks what a template says, not what your
cluster does with it: a resource template that never makes `<app>-<env>-res-<name>` leaves the pod
waiting for its Secret, and the environment shows "Your platform reported v1.4 deployed, but not
running", never live.

The reference templates, in `deploy/gitops/templates/.canyon/` in Canyon's source: a Gateway API
HTTPRoute on your gateway, and a CloudNativePG cluster whose password External Secrets generates once.
Change the domain, the gateway and the database to your own:

```yaml
# <path>/.canyon/route.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ${service}
  annotations:
    canyon.dev/address: https://${app}.${env}.apps.example.com
spec:
  parentRefs:
    - name: apps
      namespace: gateway
      sectionName: ${env}
  hostnames:
    - ${app}.${env}.apps.example.com
  rules:
    - backendRefs:
        - name: ${service}
          port: 4180
```

```yaml
# <path>/.canyon/resources/postgres.yaml
apiVersion: generators.external-secrets.io/v1alpha1
kind: Password
metadata:
  name: ${secret}
spec:
  length: 32
  digits: 8
  symbols: 0
  noUpper: false
  allowRepeat: true
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: ${secret}
spec:
  refreshPolicy: CreatedOnce
  target:
    name: ${secret}
    creationPolicy: Owner
    template:
      type: kubernetes.io/basic-auth
      data:
        username: app
        password: "{{ .password }}"
        DATABASE_URL: "postgres://app:{{ .password }}@${service}-${name}-rw.${namespace}.svc:5432/app?sslmode=require"
  dataFrom:
    - sourceRef:
        generatorRef:
          apiVersion: generators.external-secrets.io/v1alpha1
          kind: Password
          name: ${secret}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: ${service}-${name}
  annotations:
    canyon.dev/endpoint: postgres://${service}-${name}-rw.${namespace}.svc:5432/app
spec:
  instances: 1
  storage:
    size: 1Gi
  bootstrap:
    initdb:
      database: app
      owner: app
      secret:
        name: ${secret}
```

Rendered for `claims-portal`'s preview, the route reads:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  annotations:
    canyon.dev/address: https://claims-portal.preview.apps.example.com
  name: claims-portal-preview
  namespace: canyon-claims-portal-preview
spec:
  hostnames:
  - claims-portal.preview.apps.example.com
  parentRefs:
  - name: apps
    namespace: gateway
    sectionName: preview
  rules:
  - backendRefs:
    - name: claims-portal-preview
      port: 4180
```

A template that makes a kind of its own needs that kind in the AppProjects too
([Argo CD](/self-host/hand-off-argo-cd/#the-appprojects)).

### The commit

Canyon renders everything in memory before it calls git. The commit is Canyon's own, author and
committer `Canyon <canyon@<your Canyon's host>>`, dated when the release was made, with one parent, the
branch's head:

```text
v1.4 · claims-portal · preview

Canyon reviewed it: https://canyon.example.com/#/apps/claims-portal/runs/gr_8f2a

Canyon-Release: rel_8f2a
Canyon-Review: gr_8f2a
Canyon-Version: pkg_9e40
Canyon-Templates: <the tree id of <path>/.canyon, or none>
```

- **It is deterministic.** A retry rebuilds the same commit, and when the branch already holds the
  release's files at the path, Canyon pushes nothing.
- **It stays in its path.** Before any push, every path the commit changes must be under
  `<path>/<app>/<env>/`, or Canyon pushes nothing and the release is refused. Canyon never writes over
  a directory it didn't write. A directory whose `canyon.json` names another Canyon is "`<path>` was
  written by another Canyon, `<host>`: Canyon never writes over it." A directory already in the repo
  with no `canyon.json` at all is "`<path>` is already in the repo and Canyon didn't write it: Canyon
  never writes over it."
- **It is never forced.** A push that isn't a fast-forward, because someone else pushed first, is
  fetched, rendered onto the new head and tried again, up to five times.
- **The newest release wins.** Pushes to one repo take turns, and right before each one Canyon checks
  the release is still the environment's newest, still cleared and not removed since, and, for
  production, that production isn't held. So an old retry never writes over a newer release, and
  nothing is committed to production while it is held.
- **It is bounded in time.** One attempt holds the repo's turn for at most three minutes, git's fetch,
  commit and push together, and git gives up a transfer slower than 1 KiB a second for 30 seconds.
  Past either, the attempt is `unreachable`, and is retried as below.

A commit that can't land is retried as a webhook delivery is: a minute apart for five minutes, then
every 15 minutes, for an hour in all, each retry a line in the deploy, "your deploy repo didn't take
v1.4 (unreachable) · retrying in 1m". No connection, the git host's `5xx` and a pull requests API call
that failed are retried; the hour's end is "Canyon couldn't commit v1.4 to your deploy repo in an hour:
" and the last problem's sentence. A refused token, a certificate Canyon doesn't trust (no retry fixes
it), a missing branch, a branch rule, a refused template, a directory Canyon didn't write and a
namespace that is too long refuse the release at once, "Your deploy repo refused v1.4: " and the
problem's sentence, or, for a template, "Canyon couldn't render v1.4: " and its sentence.

### Production pull requests

With **Production changes** set to **Through a pull request**, Canyon pushes production's commit to a
branch `canyon/<app>/<env>/<hex>` and opens a pull request into the production branch, titled like the
commit, `v1.4 · claims-portal · prod`. A retry finds the same pull request again by its branch. Opening
one closes the environment's older Canyon pull requests, so only the newest waits for review.

A branch already there is reused only when it is exactly what Canyon would push: one commit, holding
Canyon's render at the app's directory and changing nothing outside it, whose parent is already in the
production branch's history. Anything else, such as a branch someone pushed to or one that starts from
another history, refuses the release with the branch rule's sentence (`protected_branch`).

Canyon lists the repo's open pull requests page after page until a page comes back empty, at most ten
pages, asking for 100 a page (GitHub's `per_page`, Gitea's and Forgejo's `limit`, which cap a page at
their own size). A repo whose open pull requests go on past ten pages is the pull requests API's problem
(`pulls_api_unavailable`), and the release is tried again. When Canyon finds its own pull request again,
it is the one from the deploy repo's own branch into the production branch, never one a fork opened under
the same branch name. Canyon keeps a pull request's link only when it is `https` on the repo's host or the
API's; any other link is the same problem.

The deploy says "opened a pull request for v1.4: `<url>` · production goes live once it is merged and a
pod checks in", and the environment reads "waiting on your merge since 10:00", with a link to it for
the app's team, **Pull request #`<n>`**. Merge it unedited; the merge method is yours. Canyon checks
that the pull request's head is still its own commit and that the production branch then holds exactly
the files it committed. A pull request closed without merging, or changed after Canyon opened it, puts
nothing live (see [Troubleshooting](#troubleshooting)).

After 30 minutes the deploy ends "The pull request for v1.4 is open: `<url>`. Production goes live once
it is merged and a pod checks in." The release stays open: once it is merged and a pod checks in, the
environment goes live.

### Deployed by check-in

Nothing on your side reports to Canyon. Each pod proves the release runs instead:

1. Before each pull of the access list, the sidecar connects to the app's port. When the app accepts
   the connection, the pull says so, with the header `Canyon-App: answered`.
2. At the first such check-in from a pod of the release, before it reads git, Canyon checks the
   release is still the environment's newest, not removed since and, for production, not held. A late
   check-in from an older release's pod is superseded, as its report would be, never read as a
   change to the branch.
3. Then Canyon reads the branch it committed to. The tree at `<path>/<app>/<env>` must be exactly the
   one it rendered. For a pull request, the pull request must be merged with Canyon's commit as its
   head; while it is open, nothing happens until a later check-in.

Then the environment is live, at the address your `route.yaml` annotated and with the endpoints your
resource templates annotated, and the deploy says "live at `<address>` · a pod checked in". The audit
trail records it as **release live by check-in**, by Canyon. When Canyon can't read the branch or the
pull request, nothing goes live, and it asks again at a later check-in.

A check-in proves the sidecar runs and the app accepts connections on its port, not that every page
works. Canyon still takes reports under the deploy repo: your platform can report a release
`failed` ([Reporting back](#reporting-back)), and the deploy ends with it. Argo CD can do that for you
([Reporting a failed sync](/self-host/hand-off-argo-cd/#reporting-a-failed-sync)).

While it waits, the deploy says:

- "committed v1.4 to preview · waiting for a pod to check in", or, for production, the pull request's
  line above;
- "still waiting for a pod to check in (5m)", every five minutes;
- "Canyon committed v1.4 10 minutes ago and no pod has checked in: look at your Argo CD or Flux sync",
  once, at ten minutes;
- after 30 minutes, "No pod has checked in on v1.4 in 30 minutes. The release stays open: when a pod
  checks in, the environment goes live."

The dashboard, the fleet and `canyon status` show the environment as "Your platform hasn't reported it
running since 10:00", "waiting on your merge since 10:00", "Being removed once you merge", "Canyon couldn't
commit the release" or "Your deploy repo refused the release". `canyon status` prints the pull
request's address under it.

### Removes and deletes

- **Undeploying**, **holding production** or **stopping it**: Canyon commits the environment's
  directory with everything removed but `canyon.json` and `resources/`, with the message
  `remove · <app> · <env>`. Argo CD or Flux then prunes the app. The namespace, the databases and
  buckets and their data stay: `canyon.json` keeps the app's sync and its namespace, under Argo CD
  its Application and its namespace's, under Flux its Kustomization and its namespace.
- **Deleting the app**: Canyon removes the whole directory, `delete · <app> · <env>`. Under Argo CD,
  the app's Application deletes everything it made, and the namespace's own Application deletes the
  namespace; under Flux, the namespaces HelmRelease deletes the app's Kustomization and its namespace.
  Either way the namespace goes with everything in it: the CloudNativePG Cluster and its storage
  claim, so **the data is gone**, unless your storage class keeps released volumes.

Either way, the pods let nobody in from the moment Canyon records the remove, whatever git says or
your platform does next ([When an environment is stopped](#when-an-environment-is-stopped)).

A pushed remove is done once it is pushed. A remove of production that goes through a pull request is
done once the pull request is merged, unedited, and refused when it is closed without merging. Canyon
asks once a minute for 30 minutes; after that the environment reads "Being removed once you merge",
and undeploying or deleting again asks again.

### Rolling back

Roll back through Canyon, never through git. `canyon deploy --env prod --version v1.3` puts an earlier
version back in production: a new release, with its own access token, committed (or proposed in a pull
request) as any other.

A `git revert` of Canyon's commit doesn't roll back: the branch no longer holds the files Canyon
committed for its newest release, so nothing goes live ("`<path>` on `<branch>` isn't what Canyon
committed for v1.4: nothing went live"). The pods the revert brings back are new pods of the older
release. Its access token can no longer be fetched, since Canyon answers `404` once a newer release
exists, or, if you supply it yourself, no longer works, so they never start
([When Canyon can't be reached](#when-canyon-cant-be-reached)).

### Protecting the branches

Canyon's token can push to the repo, so what limits a stolen one is your git host's rules. On GitHub,
with a ruleset (the repository's **Settings › Rules › Rulesets › New branch ruleset**):

1. Target the production branch.
2. Turn on **Restrict deletions** and **Block force pushes**.
3. Turn on **Require a pull request before merging**, with at least one approval, and under it
   **Dismiss stale pull request approvals when new commits are pushed**, **Require review from Code
   Owners** and **Require approval of the most recent reviewable push**.
4. Name your platform team in `CODEOWNERS` for Canyon's path, such as `/apps/ @example/platform`.
5. Leave the ruleset's bypass list without Canyon's machine user.

Give Canyon a **dedicated machine user** of its own, with the **Write** role on this one repository: never
a bypass actor, never a code owner, and no access to anything else. Then Canyon can open pull requests
into production, and only your team can merge them.

Protect the preview branch too if you need to, against deletion and force pushes, and allowing pushes
by Canyon's machine user alone: Canyon pushes preview, so a rule that refuses its push refuses every
preview, "The repo refused a push to preview: a branch rule. Production goes through a pull request,
and preview needs a branch Canyon may push to." Leave branches under `canyon/` unprotected: Canyon
pushes and deletes them.

With **Pull requests API** set to **None**, production is pushed with no review in git, whatever your
rules: protect it on your git host, and a push Canyon can't make is refused with the branch rule's
sentence. On GitLab, protect production as [On GitLab](#on-gitlab) says.

### What write access to the deploy repo gives

Whoever can write the deploy repo, Canyon's token or a person, controls the workloads in the apps'
`canyon-*` namespaces. Canyon checks a release when it goes live: the branch must hold exactly what
Canyon committed ([Deployed by check-in](#deployed-by-check-in)). After that, your git host's access
control and branch rules decide what runs: a later push to an app's directory is synced like any
other commit, and Canyon doesn't look at the branch again until that environment's next release.

What limits a writer is your cluster's set-up, never Canyon's:

- **the AppProjects**: the deploy repo alone, the `canyon-*-<env>` namespaces alone, the kinds they
  list and nothing cluster-scoped ([The AppProjects](/self-host/hand-off-argo-cd/#the-appprojects));
- **Pod Security `baseline`** on every app namespace, from your platform repo
  ([Namespaces](/self-host/hand-off-argo-cd/#namespaces));
- **the admission policy on the apps' Services**: an AppProject restricts kinds, not what is in
  them, so the policy keeps every Service `ClusterIP`, never a load balancer, a node port or an
  external IP ([The apps' Services](/self-host/hand-off-argo-cd/#the-apps-services)).
- **your gateway's listeners**: one per environment, each taking only its own environment's
  hostnames and admitting routes only from namespaces labelled with that environment, so a route
  pushed to preview can't claim a production hostname
  ([The gateway](/self-host/hand-off-argo-cd/#the-gateway)).

Under Flux, the sync accounts' `canyon-deploy` role, bound in each app's own namespace alone, takes the
AppProjects' place, and the sync leaves every kustomization file in the deploy repo out
([What the deploy repo's writer can do under Flux](/self-host/hand-off-flux/#what-the-deploy-repos-writer-can-do-under-flux)).

Keep write access to the deploy repo to the people you'd let run code in those namespaces, and
protect production as [Protecting the branches](#protecting-the-branches) says.

### A GitHub token for Canyon

Signed in to GitHub as Canyon's machine user:

1. Open **Settings › Developer settings › Personal access tokens › Fine-grained tokens**, and choose
   **Generate new token**.
2. Name it, such as `canyon deploy repo`, and choose an **Expiration**. Note the date.
3. Under **Resource owner**, choose the organization that owns the deploy repo. If your organization
   requires approval for fine-grained tokens, an owner approves it before it works.
4. Under **Repository access**, choose **Only select repositories** and pick the deploy repo alone.
5. Under **Repository permissions**, set **Contents** to **Read and write** and **Pull requests** to
   **Read and write**. **Metadata** is **Read-only**, which GitHub sets for you. Set nothing else.
6. Choose **Generate token**, and paste it into **Token** in Canyon's form. Don't keep another copy.

Before it expires, generate a new one the same way and choose **Replace the repo token**. A token that
has expired, or lost the repository, refuses every deploy at once: "Your deploy repo refused v1.4: The
repo refused the token. Check it is current and has this repository, and replace it here."

On Gitea or Forgejo, make an access token for the machine user with write access to repositories, and
put the machine user's name in **User name**. On GitLab, make the token as [On GitLab](#on-gitlab) says.

### On GitLab

Canyon opens production's merge requests on GitLab.com and on self-managed GitLab, through GitLab's
merge requests API. Canyon's own lines, its pages and `canyon status` call a merge request a pull
request, and its number there is the merge request's `!` number.

1. **Make Canyon's token.** In the deploy repo's project, open **Settings › Access tokens** and add a
   project access token. Name it, such as `canyon deploy repo`, and choose an expiry date; note it. Give
   it the **Developer** role and the scopes **`api`** and **`write_repository`**: `api` lets Canyon open,
   read and close merge requests, and `write_repository` lets it push, which a project token's `api`
   scope doesn't cover. On GitLab.com, project access tokens need Premium or Ultimate. On the Free tier,
   make a user for Canyon alone, give it the Developer role on this one project, and make a personal
   access token for it with the same two scopes.
2. **Choose GitLab in Canyon's form.** **Repository URL** is the project's address,
   `https://<host>/<group>/<project>` with any subgroups between. **User name** can stay `x-access-token`.
   Set **Pull requests API** to **GitLab's merge requests**, and leave **Production changes** at
   **Through a pull request**. Leave **API address** empty, and Canyon uses `https://<host>/api/v4` on the
   repository's own host. A GitLab served under a path of its own, such as `https://<host>/gitlab`, must
   set **API address** to `https://<host>/gitlab/api/v4` itself: left empty, it defaults to the host's
   root, `https://<host>/api/v4`, where Canyon's merge request calls fail, closed. Its **Repository URL**
   starts with that path too: `https://<host>/gitlab/<group>/<project>`. An address on another host or
   port, one that doesn't end `/api/v4`, or one whose path the repository URL doesn't start with, is
   refused, `400 invalid_api`: Canyon's token goes nowhere else, and reaches the project you named.
3. **Protect production**, under the project's **Settings › Repository › Protected branches**: set the
   production branch's **Allowed to merge** to your platform team's role, such as Maintainers, **Allowed
   to push and merge** to **No one**, and **Allowed to force push** off. Canyon's Developer token then
   can't push to production: it pushes its own branch `canyon/<app>/prod/<id>` and opens a merge request,
   and only your team merges it. Leave branches under `canyon/` unprotected: Canyon pushes and deletes
   them. On Premium or Ultimate, add an approval rule or code owners for Canyon's path, so a merge needs
   your platform team's approval.
4. **Keep the merge method at Merge commit**, under **Settings › Merge requests**. With **Merge commit
   with semi-linear history** or **Fast-forward merge**, GitLab asks for a rebase whenever production
   moved since Canyon opened the merge request. A rebase replaces Canyon's commit, and Canyon refuses
   the release at its first check-in: "The pull request for v1.4 was changed after Canyon opened it:
   nothing went live". Squashing on merge is fine: Canyon checks the branch's tree, not its commits.

Undeploying production, holding it and deleting the app go through a merge request the same way
([Removes and deletes](#removes-and-deletes)). Opening a merge request closes Canyon's older open ones
for the same app and environment: those from this project's own `canyon/<app>/<env>/` branches into
production's branch, never a fork's or one into another branch. Don't open merge requests from Canyon's
`canyon/` branches yourself: Canyon takes them as its own and closes them. A merge request merged into
another branch, from a fork, or after someone changed its commits, is never Canyon's: a deploy through
it doesn't go live, and a remove through it is refused.

**Moving from None to GitLab's merge requests.** If you set up Canyon on GitLab before it opened merge
requests, its token has `write_repository` alone. Make a new one with `api` and `write_repository`, as
in step 1, and paste it when you save the form with **GitLab's merge requests**: a save that changes
**Pull requests API** asks for the token again, `400 invalid_token` without it. Then protect production
as step 3 says, since Canyon no longer needs to push to it.

**Before rolling back** to a release from before Canyon opened GitLab merge requests, save the form
with **Pull requests API** set to **None** first ([Roll back a failed upgrade](/self-host/backup-and-upgrade/#roll-back-a-failed-upgrade)).
Switching delivery to the webhook doesn't do it: a remove follows the way the environment's last deploy
went.

**With no review in git.** Set **Pull requests API** to **None** instead, and Canyon pushes production
straight to its branch, as it pushes preview, on its own review alone. A save with **Through a pull
request** is then refused, `400 invalid_mode`. If production's branch rule doesn't let Canyon's token
push, each production release is refused at once with the branch rule's sentence, and production keeps
what it ran.

### A git host at a private address

Canyon's server runs git over `https` alone, and its NetworkPolicy lets it out to public addresses on
port 443. A git host at a private address, or one inside your cluster, needs a rule of its own, beside
Canyon's ([Install on Kubernetes](/self-host/kubernetes/#canyon-never-touches), where `canyon-server`
may connect):

```yaml
# Lets canyon-server reach a git host at a private address on 443.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: canyon-server-git
  namespace: canyon
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: canyon-server
  policyTypes: [Egress]
  egress:
    - to: [{ipBlock: {cidr: 10.20.30.40/32}}] # your git host's address
      ports: [{protocol: TCP, port: 443}]
```

Without it, the check answers "Canyon couldn't reach the repo. Canyon's server must be allowed to
connect to it: a git host at a private address needs a rule in Canyon's network policy." Canyon follows
no redirect, and sends the token only in one HTTP header, never in an address or a log.

If a private CA signed your git host's certificate, Canyon's server needs to trust it, or every check
and every commit is refused at once: "Canyon doesn't trust the repo's certificate. Give Canyon's server
the CA that signed it: A git host at a private address, under Your deploy repo in Canyon's docs." The
form's **Your private certificate authority, if one signed Canyon's address (optional)** doesn't do this: it is for
Canyon's own address, in your cluster.

Mount a bundle into `canyon-server` and point `SSL_CERT_FILE` at it. Git and the pull requests API
then trust what the bundle holds, and nothing else, so it must hold the public roots as well as your
CA: Canyon's server also reaches public addresses, such as GitHub's. Make the bundle and its ConfigMap:

```sh
cat /etc/ssl/certs/ca-certificates.crt your-git-ca.pem > git-ca-bundle.pem
kubectl -n canyon create configmap canyon-git-ca --from-file=ca.pem=git-ca-bundle.pem
```

You should see `configmap/canyon-git-ca created`. Then, in your overlay
([Install on Kubernetes](/self-host/kubernetes/)), patch the server:

```yaml
# A patch of canyon-server in your overlay: the server trusts the bundle.
apiVersion: apps/v1
kind: Deployment
metadata: {name: canyon-server, namespace: canyon}
spec:
  template:
    spec:
      containers:
        - name: server
          env:
            - {name: SSL_CERT_FILE, value: /etc/canyon/git-ca/ca.pem}
          volumeMounts:
            - {name: git-ca, mountPath: /etc/canyon/git-ca, readOnly: true}
      volumes:
        - {name: git-ca, configMap: {name: canyon-git-ca}}
```

Apply the overlay, wait for `kubectl -n canyon rollout status deploy/canyon-server`, and choose
**Check the repo** again. Argo CD needs the CA too
([Argo CD](/self-host/hand-off-argo-cd/#a-git-host-with-a-private-ca)).

### Switching between a webhook and the repo

Each release records how it was sent, and the setting applies to the next deploy or promotion. A
remove follows the environment's last deploy: an environment last deployed through the webhook is
removed through the webhook, even after you switch to the repo, and the other way round. So keep the
webhook saved, with your pipeline answering it, until each environment it deployed has been deployed
again through the repo, or undeployed.

To switch while hand-off is on, the other way must be saved first. In **Settings › App hosting**,
save the deploy repo, check it, then choose **Canyon commits to your deploy repo (recommended)**; or
send `{"delivery": "repo"}`. Without a repo saved, Canyon answers `409 repo_required`, "Save your deploy
repo first.", and nothing changes.

### Taking the set-up down

To stop using the deploy repo, change Canyon first, then your cluster:

1. **Switch Canyon away from the repo**: to your webhook (**Canyon calls your webhook**, or
   `{"delivery": "webhook"}`), or to Canyon running apps again ([Switching back](#switching-back)).
   From the next deploy or promotion, Canyon commits nothing more. A remove still follows the
   environment's last deploy, so undeploy or delete, while Argo CD or Flux still syncs, every
   environment you want taken down through the repo.
2. **Delete the ApplicationSets**, the two apps' and the namespaces', so they make no more
   Applications. **Mind what this deletes.** The reference ApplicationSets set
   `preserveResourcesOnDeletion: false` and the resources finalizer, so deleting one deletes each of
   its Applications and everything they made: the apps' sets take every app, its CloudNativePG Cluster
   and its storage claim; the namespaces' set takes every `canyon-<app>-<env>` namespace and whatever
   is left in it. The data is gone. To keep running apps and their data, delete each ApplicationSet with
   `kubectl -n argocd delete applicationset <name> --cascade=orphan`, which keeps its Applications,
   then delete each Application with `argocd app delete <name> --cascade=false`, which keeps what it
   made.
3. **Delete the AppProjects** `canyon-preview`, `canyon-prod` and `canyon-namespaces`, then the
   repository credential `canyon-deploy-repo`.
4. **Delete the ClusterSecretStore and the Secret `canyon-fetch-token`.** A pod still running keeps the
   access token it has, so its sidecar keeps pulling the access list. A new pod can't get one.

Under Flux, delete the two namespaces HelmReleases, then the GitRepositories and ArtifactGenerators,
instead of the ApplicationSets and AppProjects. **Mind what this deletes**: deleting a namespaces
HelmRelease uninstalls it, and so deletes every app namespace of its environment and everything in it,
the data included. To keep running apps and their data, set `spec.suspend: true` on each HelmRelease
first: helm-controller doesn't uninstall a suspended release when it is deleted. Then delete the apps'
Kustomizations in `canyon-sync`, which have `deletionPolicy: Orphan` and so keep what they applied, and
the Helm records left in `canyon-namespaces` ([Flux](/self-host/hand-off-flux/#when-it-fails)).

Pods you keep running keep letting in whoever Canyon's access list allows, as long as their release
stands. Once you undeploy the environment, hold production, delete the app, or Canyon deploys the
environment itself, they let nobody in ([When an environment is stopped](#when-an-environment-is-stopped)).
Data you keep is yours to move: Canyon never moves a database between your platform and its own.

The deploy repo and your platform repo stay yours, as they are: Canyon deletes neither, and the deploy
repo keeps every commit Canyon made. Canyon keeps the repo's settings. Replace the fetch-only token and
the host token in the repo's step so the ones your cluster held stop working, and revoke Canyon's token
on your git host.

### Troubleshooting

Each problem's sentence is what the check answers and what a refused or undelivered deploy's last line
ends with.

| What you see | Code | What to do |
|---|---|---|
| "Canyon couldn't reach the repo. Canyon's server must be allowed to connect to it: a git host at a private address needs a rule in Canyon's network policy." | `unreachable` | Check the address. For a git host at a private address, add the rule in [A git host at a private address](#a-git-host-at-a-private-address). It is also what an attempt that ran past three minutes, or a transfer that stalled, ends on ([The commit](#the-commit)): a slow or overloaded git host. |
| "Canyon doesn't trust the repo's certificate. Give Canyon's server the CA that signed it: A git host at a private address, under Your deploy repo in Canyon's docs." | `tls_untrusted` | Mount your CA into `canyon-server`, as [A git host at a private address](#a-git-host-at-a-private-address) shows. A release is refused at once, never retried: no retry fixes it. Deploy again once the server trusts the CA. |
| "The repo refused the token. Check it is current and has this repository, and replace it here." | `auth_refused` | The token expired, was revoked, or doesn't have the repository. Make a new one ([A GitHub token for Canyon](#a-github-token-for-canyon)) and choose **Replace the repo token**. |
| "The branch `<branch>` isn't in the repo. Create it, or name another." | `branch_missing` | Create the branch, with the templates on it, or correct the branch's name in the form. |
| "The token can read the repo but can't push to it. It needs Contents read and write." | `no_write` | Give the token **Contents: Read and write**, and the machine user the **Write** role. |
| "The repo refused a push to `<branch>`: a branch rule. Production goes through a pull request, and preview needs a branch Canyon may push to." | `protected_branch` | Let Canyon's machine user push to preview, and set **Production changes** to **Through a pull request** ([Protecting the branches](#protecting-the-branches)). On production, it is also a pull request branch `canyon/<app>/prod/<hex>` that isn't Canyon's one commit on production's history ([Production pull requests](#production-pull-requests)): delete that branch, and deploy or promote again. |
| "Canyon couldn't use the pull requests API at `<api_url>`. Check the address, and that the token has Pull requests read and write." | `pulls_api_unavailable` | Correct **API address**, and give the token **Pull requests: Read and write**; on GitLab, the `api` scope. A GitLab project renamed or moved since its URL was saved answers with a redirect, which Canyon doesn't follow: save the new URL, with the token. It is also a repo with more than 1,000 open pull requests, or a pull request whose link isn't `https` on the repo's or the API's host: close stale pull requests, and check the API address names your git host. |
| "`<file>` is missing: Canyon needs it to render this app. Ask your platform team." | `template_missing` | Commit the template to both branches: `route.yaml`, and one `resources/<type>.yaml` for each type your apps declare. |
| "`<file>`: `<reason>`. Ask your platform team." | `template_invalid` | Fix the template for the reason given ([Templates](#templates)). |
| "`<path>` was written by another Canyon, `<host>`: Canyon never writes over it." | `directory_taken` | Two Canyons share the repo and an app id. Give each Canyon its own **Path in the repo**, or its own repo. |
| "`<path>` is already in the repo and Canyon didn't write it: Canyon never writes over it." | `directory_taken` | Something else put files at the app's directory, with no `canyon.json`. Move them out of Canyon's path, or give Canyon another **Path in the repo**. |
| "`<namespace>` is longer than 63 characters, the most a namespace takes: a shorter app name fits." | `name_too_long` | Give the app a shorter id. |

A release can also be committed and still not go live:

- **"Canyon committed v1.4 10 minutes ago and no pod has checked in: look at your Argo CD or Flux
  sync."** Follow the release from git to the pod:
  1. Is the namespace there? `kubectl get namespace canyon-<app>-<env>`. Under Argo CD, the
     namespace's Application, `argocd app get canyon-<app>-<env>-ns`, says why it isn't: most often the
     namespaces chart isn't in your platform repo at the path the ApplicationSet names
     ([Namespaces](/self-host/hand-off-argo-cd/#namespaces)). Under Flux,
     `kubectl -n flux-system get helmreleases,artifactgenerators` says whether the environment's
     namespaces release is Ready, and `kubectl -n flux-system describe helmrelease canyon-namespaces-<env>`
     why not ([Flux](/self-host/hand-off-flux/#when-it-fails)).
  2. Is the Application synced and healthy? `argocd app get canyon-<app>-<env>`, or under Flux,
     `kubectl -n canyon-sync get kustomizations`. A sync refused by the AppProject, or under Flux by
     `canyon-deploy`, names the kind it refused: add it to both if your templates make it.
  3. Did External Secrets make the access token's Secret? `kubectl get externalsecret -n canyon-<app>-<env>`
     should show `SecretSynced`. If not, `kubectl describe externalsecret -n canyon-<app>-<env>` says
     why: often the fetch-only token, the store's address or its CA
     ([External Secrets](/self-host/hand-off-external-secrets/#when-the-token-doesnt-arrive)).
  4. Is the pod waiting for a Secret? `kubectl get events -n canyon-<app>-<env>` names a missing
     `<app>-<env>-vars`, `<app>-<env>-res-<name>` or access Secret, or an image it can't pull.
  5. Is the sidecar running? `kubectl logs -n canyon-<app>-<env> deploy/<app>-<env> -c canyon-identity`
     says when it can't reach Canyon, or doesn't trust its address (set **CA for the sign-in sidecar
     and External Secrets**).
  6. Does the app answer on port 3000? A sidecar that pulls while the app refuses connections checks in
     without `Canyon-App: answered`, and nothing goes live.
- **"`<path>` on `<branch>` isn't what Canyon committed for v1.4: nothing went live".** Something other
  than Canyon changed the app's files on the branch, such as a revert or a hand edit. Deploy again
  through Canyon, and leave Canyon's path to Canyon.
- **"The pull request for v1.4 was closed without merging: nothing went live".** Deploy or promote
  again to open a new one.
- **"The pull request for v1.4 was changed after Canyon opened it: nothing went live".** A commit was
  pushed to Canyon's pull request branch. Deploy or promote again, and merge Canyon's pull request as
  it is.

## With a webhook

Under a webhook, Canyon posts each release to your pipeline, signed, and your pipeline runs it and
reports back.

```diagram kind=flow title="A deploy under hand-off: Canyon reviews and builds, your platform runs and reports back"
steps:
  - {who: You or your coding agent, icon: agent, step: 1, what: "`canyon deploy`"}
  - {who: Canyon, icon: canyon, tone: canyon, step: 2, what: Reviews and builds}
  - {who: Canyon, icon: canyon, tone: canyon, step: 3, what: "Pushes the image to your registry, by digest"}
  - {who: Canyon, icon: canyon, tone: canyon, step: 4, what: "Sends the release, signed, to your platform's webhook", more: "Your platform answers 2xx: accepted."}
  - {who: Your platform, icon: key, step: 5, what: "Fetches the release's access token from Canyon", more: "With the host token."}
  - {who: Your platform, step: 6, what: Runs the app and the sidecar}
  - {who: The sidecar beside the app, icon: canyon, tone: canyon, step: 7, what: "Pulls the app's access list from Canyon", more: "Then again every minute."}
  - {who: Your platform, step: 8, what: "Reports deployed to Canyon", more: "With the image's digest."}
  - {who: Live, icon: passed, tone: passed, step: 9, what: "Live at the address your platform reported", more: "Canyon tells you or your agent: your platform reported it running."}
```

### The release Canyon sends

Each release is a `POST` to your webhook URL with a JSON body and these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Canyon-Event` | `deploy`, `remove` or `ping`. No signature covers it: dispatch on the body's `event`, never on this header. |
| `Canyon-Delivery` | A UUID for this release, the same on every retry |
| `Canyon-Signature-256` | `sha256=` and the lower-case hex HMAC-SHA256 of the raw body, keyed with the webhook secret |

A `deploy` body:

| Field | What it is |
|---|---|
| `event` | `deploy` |
| `delivery` | The same UUID as `Canyon-Delivery` |
| `release` | The release's id. Your report and access-token calls name it. |
| `app` | The app's id |
| `environment` | `preview` or `prod` |
| `package` | `{id, label}`: the version being shipped. `label` is there only when the version has one. |
| `gate_run` | The id of the review this version passed. For a promotion, the review preview was deployed from. |
| `gate_url` | That review in Canyon's dashboard |
| `image` | The image in your registry, by digest: `<registry>/…@sha256:<hex>` |
| `platform` | `{os, architecture}`: the platform the image is built for, read from its config, such as `{"os": "linux", "architecture": "arm64"}`. Each image is built for one architecture, so run the app on nodes of that architecture: on Kubernetes, with `nodeSelector: {kubernetes.io/arch: <architecture>}`. A release whose image's platform Canyon can't read is never sent, and the deploy fails. |
| `port` | `3000`, the port the app listens on |
| `variables` | The names of the app's runtime secrets for this environment, less every variable a resource gives. Never a value: the values come from your own secret store. |
| `resources` | The databases and buckets the app declared, for your platform to provision: `{name, type, gives}`, where `gives` names the variables that resource hands the app, such as `DATABASE_URL`. See [Databases and buckets](#databases-and-buckets). |
| `app_env` | Variables to set on the app container, exactly as given: `PORT` and the three `CANYON_IDENTITY_*` |
| `sidecar` | The sidecar to run beside the app: `{image, port, env, secret}`. `port` is `4180`, `env` is its variables, set exactly as given, and `secret` is `CANYON_ACCESS_TOKEN`, the one variable you add yourself (see [below](#how-the-sidecar-gets-access)). |
| `report_url` | Where to report how it went |
| `sent_at` | When this attempt was sent, RFC 3339 |

A `remove` body has only `event`, `delivery`, `release`, `app`, `environment`, `report_url` and
`sent_at`. Undeploying an environment your platform runs sends one, and it keeps the data. From
then on the sidecar lets nobody in, even before your platform takes the pods down (see
[When an environment is stopped](#when-an-environment-is-stopped)). Deleting
the app sends one with `"delete": true` as well, for each environment your platform has had a release
in while it runs apps, or reported resources for, so your platform deletes what it provisioned there
(see [Deleting](#deleting)). No other remove
carries `delete`.

#### Delivery and retries

- Any `2xx` answer accepts the release. Canyon doesn't need your platform to have deployed it yet.
  Answer within the 10 seconds, then deploy and provision afterwards: the whole provision, databases
  included, has to fit the 30 minutes Canyon waits for the report.
- A `4xx` refuses it. Canyon doesn't try again, and the deploy fails with
  "Your platform refused the release: HTTP 422" (with your status code).
- Anything else is retried: a `5xx`, a redirect (never followed), no connection, or no answer in
  10 seconds. Canyon tries again once a minute for the first five minutes, then every 15 minutes,
  and gives up within an hour of the first attempt.

Every attempt carries a new `sent_at`, so its signature differs, while `Canyon-Delivery` stays the
same. Your receiver can use that to drop a delivery it has already handled.

#### Checking the signature

Check the signature against the raw body bytes, before you parse them. An example in Python,
matching how Canyon signs:

```python
# Example: verify a Canyon release before acting on it.
import hashlib
import hmac

def is_from_canyon(secret: bytes, raw_body: bytes, signature_header: str) -> bool:
    expected = "sha256=" + hmac.new(secret, raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature_header)
```

This is the same scheme GitHub uses for its webhooks, so a library that verifies those verifies
these too, with the header name `Canyon-Signature-256`.

Answer a body whose signature doesn't verify with `401`, before anything else. Then the test
delivery catches a wrong secret too.

#### The test delivery

**Send a test delivery** in the dashboard, or `POST /api/v1/platform-webhook/ping`, sends one signed
`POST` to your saved webhook address, with `Canyon-Event: ping` and this body:

```json
{"event": "ping", "delivery": "0b6e7c1a-2d3f-4e5a-9b8c-7d6e5f4a3b2c", "sent_at": "2026-09-27T08:05:00Z"}
```

It's signed, timed out and refused redirects exactly as a release is, and it's never retried. It
names no release, app or report address, so there is nothing to act on. Your receiver should:

1. check the signature, and answer `401` if it doesn't verify;
2. check `sent_at` as for any delivery;
3. read the body's `event`, never the `Canyon-Event` header, and answer `2xx` to one it doesn't act on, such as `ping`; ignore an event it doesn't know, so a new one Canyon sends later changes nothing.

A `2xx` tells the admin that Canyon reaches your platform and that your platform has the current
secret. It says nothing about the host token, which the first release's access-token fetch and report
are the first to use.

### Running the app

Run the app and the sidecar side by side, so the sidecar reaches the app at `127.0.0.1:3000`, as
two containers in one Kubernetes pod do. Under the deploy repo, Canyon's `deployment.yaml` does all of
this for you ([What Canyon commits](#what-canyon-commits)); under a webhook, your platform does:

- the app container: the release's `image`, with `app_env`;
- the sidecar container: `sidecar.image`, with `sidecar.env` and `CANYON_ACCESS_TOKEN`;
- an `emptyDir` mounted at `/var/cache/canyon` in the sidecar container and in no other, where the
  sidecar caches the access list ([below](#when-canyon-cant-be-reached));
- your gateway sends the app's traffic to the sidecar on port `4180`.

The sidecar runs as a user other than root and writes nothing but its cache, so run it locked down.
This is the sidecar's part of a pod spec, the way Canyon runs it on its own clusters:

```yaml
containers:
  - name: canyon-identity
    image: <sidecar.image>
    env: [] # sidecar.env, each as given, and CANYON_ACCESS_TOKEN from your secret store
    ports:
      - containerPort: 4180
    volumeMounts:
      - name: canyon-access-cache
        mountPath: /var/cache/canyon
    securityContext:
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: [ALL]
      seccompProfile:
        type: RuntimeDefault
    resources:
      requests: { cpu: 10m, memory: 32Mi }
      limits: { cpu: 100m, memory: 64Mi }
volumes:
  - name: canyon-access-cache
    emptyDir:
      sizeLimit: 4Mi # the cache is the access list, 1 MiB at most, and a copy while it's replaced
```

Don't mount the cache in the app's container: the app could then change who may open it. Without
the cache the sidecar still runs, but it can't start again while Canyon is unreachable. What the
cache covers, and what it doesn't, is in [When Canyon can't be reached](#when-canyon-cant-be-reached).

The sidecar checks the token your gateway signed people in with, checks that the person may open
the app, and passes the request on with Canyon's identity token: a signed token that names the
person and their app roles. The app verifies that token as
[Sign-in inside your app](/build/sign-in-in-your-app/) describes. Its `CANYON_IDENTITY_JWKS_URL`
points at the sidecar, which serves the keys to verify it with, inside the pod.

```mermaid title="A person opens an app your platform runs: your gateway signs them in, the sidecar checks the list it pulled from Canyon"
sequenceDiagram
  participant B as Browser
  participant G as Gateway
  participant S as Sidecar
  participant A as Your app
  participant C as Canyon
  S->>C: pull the app's access list, at start and every minute
  B->>G: open the app's address
  G->>G: sign the person in, at your identity provider
  G->>S: the request, with the person's token
  S->>S: verify the token against your gateway's keys
  S->>S: check the access list
  S->>A: the request, with Canyon's identity token
  A->>S: fetch the key set, inside the pod
  A-->>B: the page, back the same way
```

The sidecar verifies your gateway's token with your gateway's key set, or, for AWS load balancer
sign-in, the load balancer's public key from AWS. It signs Canyon's identity token with a key it
makes when it starts, so a sidecar that restarts signs with a new key: the app fetches the key set
again when a token names a key it doesn't hold. Neither the sidecar nor the app asks Canyon for a
key: the one call to Canyon is the access list.

Before a request reaches the app, the sidecar removes every header a client could use to claim to
be someone, or to fake where a request came from or over which scheme: `Authorization` (replaced
with Canyon's identity token), the gateway's session cookie, `X-Forwarded-*` but the two it sets
itself (`X-Forwarded-For` and `X-Forwarded-Proto`, from your gateway), `X-Auth-Request-*`,
`X-Amzn-Oidc-*`, other gateways' identity headers (`X-Goog-Authenticated-User-*`, `X-Goog-Iap-Jwt-Assertion`,
`Cf-Access-*`, `X-Ms-Client-Principal*`, `X-Ms-Token-*`, `X-Pomerium-*`, `X-Authentik-*`, `Tailscale-User-*`,
`X-Vouch-*`, `Teleport-Jwt-Assertion`, `Remote-Email`, `Remote-Groups` and `Remote-Name`), `Remote-User`, `X-Remote-User`, `X-Webauth-User`, `X-Real-IP`, `Client-IP`,
`X-Client-IP`, `True-Client-IP`, `Cf-Connecting-IP`, `X-Cluster-Client-IP`, `Forwarded`,
`X-Scheme`, `X-Url-Scheme`, `Front-End-Https`, and any header whose name contains `_`. The identity
token is the one thing that says who the person is. The sidecar passes on `X-Forwarded-For` and
`X-Forwarded-Proto` as your gateway sent them, so your gateway must set both itself, replacing any a
client sent.

### Databases and buckets

When an app declares a database or a bucket in `canyon.toml` ([Databases for your
app](/build/databases/)), the release lists it in `resources`, and your platform provisions it.
Canyon creates nothing, calls no provisioner and holds no credential for it. Under the deploy repo,
your [resource templates](#templates) make it instead. Each entry is:

| Field | What it is |
|---|---|
| `name` | The name the app gave it in `canyon.toml`, such as `db` |
| `type` | A [Score](https://score.dev) resource type: `postgres` or `s3` |
| `gives` | The variables the app reads from it: `["DATABASE_URL"]` for `postgres`, `["STORAGE_URL"]` for `s3` |

Provision one per app, environment and name, so preview and production never share data, and put
each variable in the app container's environment, built from your own secret store. The values
aren't in the release; their formats are:

| Type | Variable | Format |
|---|---|---|
| `postgres` | `DATABASE_URL` | `postgres://user:password@host:5432/database?sslmode=require`. Name the `sslmode` your server needs: `verify-full`, with `sslrootcert`, verifies its certificate. node-postgres, psycopg and pgx read both from the URL. |
| `s3` | `STORAGE_URL` | `https://ACCESS_KEY_ID:SECRET_ACCESS_KEY@endpoint[:port]/bucket?region=REGION`: one bucket, path-style, with a credential scoped to it |

The variables a resource gives are never in `variables`: your platform provides them with the
resource.

#### An example: a release as a Score workload

A release with `"resources": [{"name": "db", "type": "postgres", "gives": ["DATABASE_URL"]}]`
translates directly into a Score workload, which a Score implementation such as
score-k8s provisions:

```yaml
# Example: the release for claims-portal in preview, as a Score workload.
apiVersion: score.dev/v1b1
metadata:
  name: claims-portal-preview
containers:
  app:
    image: <the release's image>
    variables:
      DATABASE_URL: "postgres://${resources.db.username}:${resources.db.password}@${resources.db.host}:${resources.db.port}/${resources.db.database}?sslmode=require"
resources:
  db:
    type: postgres
```

On a platform that keeps credentials in a secret store and syncs them with the External Secrets
Operator, an `ExternalSecret` template builds the variable without anyone writing it by hand:

```yaml
# Example: build DATABASE_URL from your secret store's entry for this app, environment and resource.
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: claims-portal-preview-db
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: platform-store
    kind: ClusterSecretStore
  target:
    name: claims-portal-preview-db
    template:
      data:
        DATABASE_URL: "postgres://{{ .username }}:{{ .password }}@{{ .host }}:5432/{{ .database }}?sslmode=require"
  dataFrom:
    - extract:
        key: databases/claims-portal/preview/db
```

The app container then reads `DATABASE_URL` from that Secret, beside the release's `app_env`.

#### Reporting where they are

Your `deployed` report says where each resource is, by its name, with `resources`: one entry for
each resource the release lists. Canyon keeps these endpoints and shows them to the app's team, on the app page and in `canyon status`, so they can
find the database without asking you. An endpoint names a place, never a credential:

```json
{"status": "deployed", "address": "https://claims.preview.apps.example", "digest": "sha256:<64 hex>",
 "resources": [{"name": "db", "endpoint": "postgres://pg.data.example:5432/claims_portal_preview_db"}]}
```

- `name` is one of the release's own resources, each at most once.
- `endpoint` uses the type's scheme: `postgres://` or `postgresql://` for `postgres`, `s3://` or
  `https://` for `s3`.
- Its host is in lower case: a domain name, a single-label name such as a cluster's service
  (`claims-db-rw`), a bucket name, or an IP address. A port is optional, with no leading zero.
- Its path is empty or one segment, never `.` or `..`: the database or the bucket.
- No user name or password, no query (not even `?sslmode=`), no fragment, no `%` escapes. At most
  255 characters of printable ASCII.

Anything else refuses the whole report with `400 invalid_report`, so check the shape before you
send. A report that leaves out a resource the release lists is refused with `409
resources_missing`, and nothing goes live:

```json
{"error": "resources_missing", "missing": ["db"],
 "message": "the report has no endpoint for db: send one for every resource the release lists"}
```

Report again with every one: the deploy keeps waiting for it. A release that lists no resource takes
a report without `resources`, or with `"resources": []`. Each `deployed` report Canyon records
replaces the endpoints the environment's earlier report named, so a report without `resources`
clears them, as `[]` does.

Under the deploy repo, a resource template's `canyon.dev/endpoint` annotation takes this shape too.

#### Deleting

Undeploying keeps the data, as it does when Canyon runs the app. Only deleting the app sends a
`remove` with `"delete": true`. Then delete what you provisioned for that app and environment, and
nothing else: key it by app, environment and resource name. Make it idempotent: a delete can send
the same remove again, and deleting what is already gone should still report `removed`. The app's
delete waits until every environment reports `removed`. It sends one to every environment you run
the app in, to every one where a release's package declared a database or bucket, and to every one
you reported resources for, even after switching back to Canyon running apps. If you report the app
`deployed` after its delete was done, Canyon sends one more such remove. An app whose production data is on your platform needs a
person to delete it, as one with production running does, and while no platform webhook is
saved its delete is refused `webhook_required` before anything changes.

### Reporting back

Your platform reports on a release at its `report_url`, with the host token:

```text
POST /releases/<release>/report
Authorization: Bearer <host token>
```

The body is exactly one of these, with no other fields:

| Body | When |
|---|---|
| `{"status": "deployed", "address": "https://claims.prod.apps.example", "digest": "sha256:<64 hex>"}` | The app is running. `address` is where people open it: `https://`, a host and an optional port, then any path segments (none `.` or `..`), with no user name, query or fragment. `digest` must be the release image's own digest. When the release lists resources, it adds `resources`, [where each one is](#reporting-where-they-are). |
| `{"status": "failed", "reason": "…"}` | It didn't work. `reason` is plain text, 1 to 1024 characters, and people see it. |
| `{"status": "removed"}` | What ran in that environment is gone. |

Canyon answers:

| Answer | Meaning |
|---|---|
| `200` | Recorded. Sending the same status again changes nothing and answers `200`. |
| `400 invalid_report` | The body isn't one of the three shapes, or an `address` or an endpoint isn't as described. Nothing is recorded. |
| `401 bad_token` | No host token, or not a current one. The webhook and the deploy repo each keep their own host token, and either is taken. |
| `404 not_found` | No such release. |
| `409 superseded` | A `deployed` report on a release that no longer stands: a newer release of that environment exists, Canyon itself has deployed there since, or the release was already reported removed. The answer names the release that stands. A `removed` or `failed` report on such a release is accepted. |
| `409 digest_mismatch` | The digest isn't the one Canyon reviewed. Nothing goes live, and the answer names the expected digest. |
| `409 resources_missing` | A `deployed` report whose `resources` leaves out a resource the release lists. Nothing goes live, `missing` names each one, and the deploy keeps waiting: report again with every one. Canyon checks this last, so a report that also fails one of the other checks gets that answer instead. |
| `409 held` | A `deployed` report on production while a security responder or an admin has stopped it ([Stopping production](/org/who-can-do-what/#stopping-production)). Canyon sends your platform a `remove` for what it started. |
| `429 rate_limited` | More than 60 reports a minute with this token. |

The host token works only for reports and for fetching access tokens. It can't sign in to Canyon.

### What people see

A deploy or promotion runs as usual until the build. Then its timeline says:

- "handed to your platform · waiting for it to report";
- "your platform answered 503 · retrying in 1m" for each retry;
- "live at `https://claims.prod.apps.example` · reported by your platform" once your platform reports
  `deployed`.

It fails with one of:

- "Your platform refused the release: HTTP 422";
- "Your platform did not accept the release in an hour";
- "Your platform reported: " and your `reason`;
- "Your platform removed the release before it went live".

A `deployed` report that leaves out a resource adds "Your platform reported v1.4 deployed without
saying where db is: nothing went live, Canyon is still waiting" to the timeline, once, and the deploy
keeps waiting for a report that names every one.

While Canyon waits for a report it says "still waiting on your platform (5m)", with the minutes so far, every five minutes. After 30
minutes the deploy ends with "Your platform has not reported on v1.4 in 30 minutes. The release
stays open: when it reports, the environment goes live." A report that comes later still makes the
environment live.

The dashboard and `canyon status` show an environment that waits on your platform as "Sent to
your platform at 10:00, not running yet", "Being removed by your platform", "Your platform refused the
release" or "Your platform did not accept the release", or, after a report that left a resource out,
"Your platform's report left out where db is". A live environment shows the address your
platform reported. The app's team sees each resource your platform reported under the environment,
with its endpoint, such as "postgres db · preview · postgres://pg.data.example:5432/claims_portal_preview_db".
Each report is in the audit trail as your organization's platform.

**Settings › App hosting** shows every member the last release Canyon sent since the webhook was
saved, such as "Last release: your platform answered 202 · 3 minutes ago", or "No release has been
sent yet."

## What Canyon still does

| | What Canyon still does | What becomes yours |
|---|---|---|
| Sign-in | Keeps who may open each app. The sidecar checks every request against that list. | Signing people in, at your gateway: a standard OIDC gateway or AWS load balancer sign-in. **Canyon signs people in** isn't supported. |
| The identity token | Provides the sidecar that signs it. | Running the sidecar beside the app, with the release's settings. Under the deploy repo, Canyon's `deployment.yaml` does it. |
| Databases and buckets | Names each one the app declares in the release, and shows the endpoints you report or annotate to the app's team. It creates none and holds no credential for them. | Provisioning each one, putting its variable in the app's environment, and deleting it when the app is deleted. Under the deploy repo, your resource templates do it. |
| Secrets | Keeps the values set with `canyon secret set`, sealed, and never sends one. The release names each runtime secret in `variables`, and `deployment.yaml` by key. Build secrets are still read while Canyon builds the image. | Every runtime value, from your own secret store, and `CANYON_ACCESS_TOKEN`. |
| Outbound | Nothing. The app's calls don't go through Canyon's egress gateway, so Canyon records none and no outbound mode applies: setting one answers `409 egress_external`. The app's *Outbound* card says `runs on your own platform`. | What the app may call. The sidecar needs `https` to `CANYON_PUBLIC_URL`, and to your gateway's key set or AWS's load balancer keys. |
| Patching | With `platform.known-vuln` on, checks the packages in the image it built, and the rule still reads at promotion. | Every fix. Canyon never patches or redeploys an app your platform runs: a fix ships as a new deploy. |

## How the sidecar gets access

The sidecar needs the app's access list: who may open it, who is an admin, which roles they hold.
It pulls that list from Canyon, at `CANYON_ACCESS_URL` (Canyon's `/access/<app>/<environment>`),
using an access token, over `https` only, which is why hand-off needs an `https`
`CANYON_PUBLIC_URL`.

Under a webhook, your pipeline fetches that token with the host token. Under the deploy repo, External
Secrets fetches it with the fetch-only token ([External Secrets](/self-host/hand-off-external-secrets/)):

```text
GET /releases/<release>/access-token
Authorization: Bearer <host token or fetch-only token>
```

The answer is `{"name": "CANYON_ACCESS_TOKEN", "value": "…"}`. Store the value in your own secret
store and give it to the sidecar as `CANYON_ACCESS_TOKEN`; it never travels in the release itself.
Each release has its own token, the same value on every fetch. Once a newer release of the
environment exists, this call answers `404`. Once a newer release reports `deployed`, the older
release's token stops working.

The sidecar pulls the list when it starts, trying for up to 10 seconds. After that it pulls again
every minute, so a change in who can open the app reaches it within about a minute. It follows no
redirect: the token goes to `CANYON_ACCESS_URL` and nowhere else.

### When Canyon can't be reached

Every case below follows from three things the sidecar keeps.

- **The last list it pulled**, in memory. A pull that fails never replaces it. A pull fails when
  Canyon doesn't answer within 10 seconds, answers anything but `200`, or sends a list the sidecar
  can't read. Canyon refusing the pod's token is a failed pull too.
- **The access grace**, **How long apps keep letting people in when they can't reach Canyon** in the dashboard: how long it keeps letting people in on that
  list once pulls fail. It is the
  `access_grace` preference: a duration from `0` to `24h`, written like `30m` or `1h30m`, default
  `1h`. It counts from the last pull that succeeded; a sidecar that keeps running starts counting at
  its first failed pull, at most about a minute later. Past the grace, the sidecar answers every
  signed-in request `503` `access_unreadable`, "this app's access list cannot be read", and keeps
  pulling every minute. The first pull that succeeds lets people in again. A release carries the
  grace in force when it was sent, so a change applies to releases sent after it.
- **Its cache.** After every pull that succeeds, the sidecar saves the list, and when it got it, in
  `/var/cache/canyon`. It reads the cache only when it starts, and only if its own pull fails for 10
  seconds. Then it serves the cached list for what is left of the grace, and logs until when. With
  no cache, or one older than the grace, it exits without serving anyone.

The cache lasts as long as the volume at `/var/cache/canyon`. In Canyon's `deployment.yaml`, and in
the pod spec under [Running the app](#running-the-app), that is an `emptyDir`: it survives a restart
of the sidecar's container, and is gone with the pod. A
Kubernetes pod never moves: a pod "moved" to another node is a new pod, with a new `emptyDir`. So:

| Case | What the sidecar does |
|---|---|
| Canyon can't be reached, and nothing restarts | Serves its last list until the grace runs out. |
| The sidecar's container restarts during the outage: killed for memory, or after a crash | Finds its cache and serves that list until the grace runs out, counted from the last pull that succeeded. Restarting again never adds time. |
| The app's container restarts | Nothing: the sidecar keeps running. |
| A new pod starts during the outage: a rollout, a scale-up, `kubectl rollout restart`, a drained node, or a pod evicted or rescheduled onto another node | Has no cache, so it exits after its 10 seconds. Kubernetes starts it again with its usual back-off, by default up to 5 minutes apart. No request reaches the app through that pod until a start reaches Canyon. |
| The grace runs out | Answers every signed-in request `503` `access_unreadable`. A sidecar that restarts now doesn't start. |
| Canyon is back | Its next pull, within a minute, replaces the list and ends the count. A pod that couldn't start starts at its next try. |
| Canyon answers, but the environment is stopped | Takes the deny-all list as it takes any list, with no grace, and saves it: [When an environment is stopped](#when-an-environment-is-stopped). |
| A pod of an older release, once a newer release of the environment has gone live | Canyon refuses its token, a failed pull: the last list until the grace runs out, then nobody. A new pod of that release never starts. |

#### Choosing the access grace

The grace trades two things:

- **How long your apps keep working while Canyon can't be reached**: an upgrade, a restore, or a
  network fault between your cluster and Canyon.
- **How long a removal takes to land while it can't.** While pods reach Canyon, removing someone, or
  stopping an environment, lands within about a minute. While they don't, everyone on the last list
  can still open the app until the grace runs out, including someone you just removed. Someone who
  cuts the pods off from Canyon on purpose gets the same time.

So set it longer than the longest outage of Canyon you plan for, such as an upgrade, and no longer
than you would let a removed person keep their access. At `0`, the first pull that fails refuses
everyone, so every outage of Canyon, however short, is an outage of every app your platform runs,
and a sidecar that restarts during one doesn't start.

No grace helps a new pod. While Canyon is down for a planned outage, don't roll out, scale up or drain
the nodes of the apps your platform runs. A new grace reaches a running app with its next release:
deploy the app again.

### When an environment is stopped

Canyon can't take your pods down, but it can stop them letting anyone in. From the moment someone
undeploys an environment your platform runs, a security responder or an admin holds production
([Stopping production](/org/who-can-do-what/#stopping-production)), or someone starts deleting the
app, Canyon answers that environment's pulls with an empty list. At its next pull, within about a
minute, the sidecar refuses everyone, even while your platform is still taking the pods down, or if it
never does. There is no grace: the pull succeeds, so the sidecar takes the empty list as it is, and
it saves that list too, so a restart doesn't bring the old one back. A new pod pulls the same empty
list, or, while Canyon can't be reached, doesn't start.

It starts when Canyon sends the `remove`, or commits it, not when your platform reports `removed` or
syncs it. A hold leaves preview alone. The pods of a release that was removed never get the list back:
when you deploy the environment again, or start production again after lifting a hold, the new release
has its own access token, and only its pods are let in. The same goes for your platform's pods after
you switch back to Canyon running apps and Canyon deploys the environment. So a fix you push after
undeploying a vulnerable version never lets the vulnerable pods back in, even if your platform never
took them down.

A pod that still pulls the empty list still checks in. While the environment waits on its removal, the
dashboard and `canyon status` say so in amber, for example "Still running: your platform last reported it 1m ago": your platform
hasn't taken that environment's pods down yet.

### Whether a pod checks in

Every pull of the access list is also how Canyon knows a pod of the release is up, whatever your
pipeline reported. Under the deploy repo, the first check-in of a pod whose app answers is what makes
a release live ([Deployed by check-in](#deployed-by-check-in)). Under a live environment your platform
runs, the dashboard and `canyon status` say:

- "Your platform reported it running 2m ago", while a pod pulled in the last 5 minutes;
- "Your platform hasn't reported v1.4 running since 15:04", in amber, once 5 minutes pass without one;
- "Your platform reported v1.4 deployed, but not running", in amber, when none has 10 minutes after
  your report. Before that nothing more is said, while the pods start;
- "Still running: your platform last reported it 1m ago", in amber, while the environment waits on a `remove`: your platform
  hasn't taken its pods down yet (see [When an environment is stopped](#when-an-environment-is-stopped)).

It is the sidecar checking in: a pod whose app crashes while its sidecar runs still checks in. Under a
webhook it never blocks a deploy or a promotion. An app with no pods, scaled to zero, or
one deployed without the sidecar shows "Your platform reported v1.4 deployed, but not running", as it
should. After Canyon has been
unreachable for more than 5 minutes every environment you run shows it too, and each clears within
a minute of Canyon coming back, at the sidecar's next pull. Whoever holds a release's access token
can pull, so treat the token as you treat the host token. Under the deploy repo, that is why only the
fetch-only token reaches your cluster, only Canyon's app namespaces, `canyon-<app>-preview` and
`canyon-<app>-prod`, may read through Canyon's ClusterSecretStore, and a check-in makes live only a
release whose files are on the branch, unedited, and still the newest.

What that leaves, and Canyon accepts: the fetch-only token isn't bound to one app. Any ExternalSecret
in an app namespace can fetch **any** app's newest access token through the store, and with it pull
that app's access list, its members' emails and roles, and check in for it. Two things limit what that
can do. A check-in settles only what git already holds: the files Canyon committed, on the branch,
unedited. And a stopped environment's list lets nobody in, whoever pulls it. What your platform team
can do is keep who may create an ExternalSecret in an app namespace to Canyon's deploy repo alone. The
reference AppProjects already do: only they deploy into `canyon-*-preview` and `canyon-*-prod`, and
only from the deploy repo. Don't give another AppProject, Flux Kustomization or person a way to create
ExternalSecrets in those namespaces.

## Related

- [Argo CD](/self-host/hand-off-argo-cd/), [Flux](/self-host/hand-off-flux/) and
  [External Secrets](/self-host/hand-off-external-secrets/): the deploy repo's reference set-up.
- [Configuration reference](/self-host/configuration/): the variables on the server and executors.
- [The release schema](/contracts/schema/release.schema.json):
  the exact shapes of a release, a report, the webhook and deploy repo settings, and `canyon.json`.
- [Install on Kubernetes](/self-host/kubernetes/): what
  Canyon needs on the cluster, including letting the server reach your webhook or your git host.

## Changed in this release

If your platform already receives Canyon's releases:

- A `deploy` release now carries `platform`, `{os, architecture}`: the architecture its image is built
  for. A receiver that checks bodies against its own copy of `release.schema.json` needs the new copy
  before you upgrade, or it refuses every release. Run the app on nodes of that architecture.
- Under the deploy repo, each app's Deployment gains `nodeSelector: {kubernetes.io/arch: …}` at its next
  release, so each app rolls out once. A release in flight across the upgrade still carries no
  `platform`, and its Deployment no `nodeSelector`, as before.
- **Upgrade Canyon's executor before its server, or with it.** The server asks the executor for each
  image's platform, and an older executor doesn't know the question: until it is upgraded, every deploy
  and promotion your platform runs fails before anything is sent.
- This release changes the work Canyon's servers run, so it rolls back only to v0.2.0 or a later one
  ([Roll back a failed upgrade](/self-host/backup-and-upgrade/#roll-back-a-failed-upgrade)).
- Canyon can now commit releases to your deploy repo instead of calling your webhook
  ([Your deploy repo](#your-deploy-repo)). Nothing changes until an admin chooses it: the
  webhook stays the default.
- On GitLab, production can now go through a merge request ([On GitLab](#on-gitlab)). Choosing it
  needs a token with the `api` scope, and the save asks for the token again. Save it only once every
  Canyon server runs this release. **Before rolling back** to an earlier release with GitLab's merge
  requests saved, save the deploy repo with **Pull requests API** set to **None**: the earlier release
  has no GitLab client, and its server stops at its first production commit through one. Switching
  delivery to the webhook doesn't protect it, since a remove follows the environment's last deploy
  ([Roll back a failed upgrade](/self-host/backup-and-upgrade/#roll-back-a-failed-upgrade)).
- **Before rolling back to a release from before the deploy repo (v0.2.x), switch delivery back to
  the webhook** (`{"delivery": "webhook"}`). Those releases know nothing of the deploy repo and can't
  commit to it. While your platform runs the apps, that needs a webhook saved (`409
  webhook_required`): save one first, or switch Canyon back to running apps, then choose the webhook.
  The deploy repo keeps its tokens on its own settings, never the webhook's, so a Canyon that saved
  only a deploy repo still starts on the older release
  ([Roll back a failed upgrade](/self-host/backup-and-upgrade/#roll-back-a-failed-upgrade)).
- **Canyon builds nothing on a cluster its check hasn't seen enforce NetworkPolicy, under hand-off too**
  ([What it needs](#what-it-needs)). If **Settings › App hosting** shows the front door *not checked on
  this cluster*, turn enforcement on, then save the front door once after upgrading: until then every
  deploy fails before the build. Every Kubernetes host saves it once, whoever runs the apps
  ([The upgrade that checks NetworkPolicy before a build](/self-host/backup-and-upgrade/#the-upgrade-that-checks-networkpolicy-before-a-build)).
- If you saved a platform webhook, an admin saves the front door once after upgrading, before Canyon
  runs apps again. Canyon can't tell whether the front door was saved while your platform ran the
  apps, when nothing was checked, so until then **Settings › App hosting** says it is not checked on
  this cluster, and a deploy Canyon would run fails and says so. Your platform's releases are not
  affected.
- Canyon can now send a `ping`, when an admin sends a test delivery. A receiver that answers an event
  it doesn't know with a `4xx` still works for releases, but the dashboard then shows that answer.
  Answer `2xx` to a verified `ping`, dispatch on the body's `event`, never the `Canyon-Event` header,
  and ignore an event you don't know.
- A release's `variables` no longer lists the names a resource gives, such as `DATABASE_URL`. They
  are in `resources[].gives`. A receiver that builds the app's environment from `variables` alone
  must read `gives` too, and provide those itself.
- A `deployed` report's `address` must be `https://`, a lower-case host and an optional port, then any
  path segments. A user name, a query, a fragment, a `%` escape, a backslash, an upper-case host, a
  `.` or `..` segment, an all-digit last host label and a port with a leading zero are refused with
  `400 invalid_report`.
- Under Flux, the reference set-up now makes each app's namespace from your platform repo, as Argo CD's
  does, and replaces `flux/kustomizations.yaml` with six files, a new admission policy among them, and
  the namespaces chart 1.2.0. If you made namespaces by hand under the earlier one, move as
  [Moving from the hand-made set-up](/self-host/hand-off-flux/#moving-from-the-hand-made-set-up) says,
  in that order: done in another, it can delete your apps' data.
