---
title: How Canyon works
description: The parts of Canyon, what each one may reach, how a review decides, and how a version gets to production.
---

Canyon has two halves that never swap jobs. The **server** decides: it holds your org's rules,
the review, sign-in, secrets and the audit trail, and runs no app code. The **executor** does: it
scans, builds and runs apps, and keeps no org settings and no secrets. Apps can't reach the server,
its database or each other directly; from outside, they are like any other visitor.

The same parts run in two shapes: as containers on **one Docker host**, the laptop demo's, or as pods
on **your Kubernetes cluster**. [Choices at a glance](/concepts/modes/#where-canyon-runs) says which to
pick.

```mermaid title="Canyon's parts: the server decides, the executor builds and runs, every app sits behind sign-in"
flowchart TD
  agent["Coding agent<br>with the canyon CLI"]
  team["Your team<br>in the dashboard"]
  users["People using the apps"]
  idp["Your identity provider"]
  model["Your AI model"]
  subgraph host["Canyon, on your Docker host or Kubernetes cluster"]
    server["canyon-server<br>API and dashboard · secrets vault<br>audit trail"]:::canyon
    pg[("Canyon's Postgres")]
    review["The review<br>gitleaks · Trivy · Opengrep · ESLint · Checkov<br>AI review · 21 rules you set"]:::canyon
    executor["Executor<br>scans · builds · runs apps"]:::canyon
    door["Sign-in in front of every app<br>when sign-in is on"]:::canyon
    app["Your app"]
    apppg[("Apps' Postgres")]
    egress["Egress gateway, beta<br>records each call out"]:::canyon
  end
  agent -->|"canyon deploy"| server
  team --> server
  idp -->|"signs people in"| server
  server --> pg
  server -->|"every deploy"| review
  review -->|"passed"| executor
  model -.->|"reads the code, when set"| review
  executor -->|"builds and starts"| app
  users --> door
  door --> app
  app --> apppg
  app -.->|"outbound through Canyon"| egress
```

**Every app sits behind your company's sign-in.** On Kubernetes a Canyon sidecar,
`canyon-identity`, runs in front of each app; on one Docker host the server checks sign-in itself.
Either way, the app learns who is calling from a token Canyon signs. The laptop demo is the
exception: it runs with sign-in off, so anyone who reaches it opens every app, and apps get no
token.

## What runs where, on one Docker host

```mermaid title="Canyon on one Docker host: the server is also every app's front door"
flowchart TD
  cli["Coding agent and CLI"]
  admins["People using Canyon"]
  users["People using the apps"]
  server["canyon-server<br>dashboard, API, review,<br>secrets, audit, and every<br>app's front door"]:::canyon
  pg[("Canyon's Postgres")]
  executor["canyon-executor<br>holds the Docker socket"]:::canyon
  app["An app's container<br>on a Docker network of its own"]
  apppg[("app-postgres<br>the apps' databases")]
  egress["canyon-egress<br>Envoy and the egress check"]:::canyon
  cli -->|"upload, deploy"| server
  admins -->|"the dashboard"| server
  users -->|"an app's address"| server
  server --> pg
  server -->|":7778"| executor
  executor -->|"scans, builds, starts"| app
  server -->|"the request, with the identity<br>token when sign-in is on"| app
  server -->|"each app's database"| apppg
  app --> apppg
  app -.->|"outbound through Canyon"| egress
  egress -->|"a record of each call"| server
```

- **The server answers every app's address itself.** With sign-in on, it checks the person's Canyon
  session and the app's access list, then forwards the request with Canyon's identity token. In the
  laptop demo, with sign-in off, it forwards every request with no check and no token. There is no
  router and no sidecar.
- **Each app gets a Docker network of its own**, shared with the server, `app-postgres` and, while
  its calls go through Canyon, the egress gateway, so an app can't reach another app or Canyon's
  Postgres. The server's API refuses connections from the
  app networks; an app reaches only the key set that signs its identity tokens.
- **The executor holds the Docker socket** and has no route to Canyon's Postgres, which sits on a
  network only the server joins.
- **Builds use `pack`**, with the same Paketo buildpacks as on Kubernetes.

## What runs where, on Kubernetes

```mermaid title="Canyon on a Kubernetes cluster: who talks to whom"
flowchart TD
  cli["Coding agent and CLI"]
  admins["People using Canyon"]
  gh["GitHub"]
  users["People using the apps"]
  gw["Your gateway or ingress<br>TLS"]
  server["canyon-server<br>dashboard, API, review,<br>secrets, audit"]:::canyon
  router["canyon-router"]:::canyon
  out["Your identity provider, GitHub,<br>the AI model if you set one"]
  pg[("Canyon's Postgres")]
  executor["canyon-executor<br>scans, builds, runs apps"]:::canyon
  scans["Scan jobs<br>no network but Trivy's database"]
  builds["kpack builds"]
  app["App pod<br>canyon-identity and the app"]
  apppg[("Apps' Postgres :5433")]
  egress["canyon-egress<br>Envoy and the egress check"]:::canyon
  cli -->|"upload, deploy"| server
  admins -->|"the dashboard"| server
  gh -->|"webhooks"| server
  users -->|"an app's address"| gw
  gw --> router
  server --> out
  server --> pg
  server -->|":7778"| executor
  executor --> scans
  executor --> builds
  executor -->|"creates"| app
  router -->|":4180"| app
  app --> apppg
  server -->|"each app's database"| apppg
  app -.->|"outbound through Canyon"| egress
```

| Part | Its job | What it may reach |
|---|---|---|
| `canyon` CLI | Packs the app's source and uploads it, prints the review, signs in with a device code. | Your Canyon's address. |
| `canyon-server` | The dashboard and the API. Signs people in through your identity provider. Runs the review: gitleaks and the AI review itself, the other scanners through the executor. Keeps approvals, the audit trail and app secrets, sealed under its own key. Receives GitHub's webhooks. | Its Postgres, the executor, the apps' Postgres to create each app's database and role, and public HTTPS: your identity provider, GitHub, the model, your org's rules repository, and your pipeline's webhook when handing off. The Kubernetes API on 443 or 6443, to read the cluster's service-account key set for the egress check's policy. Never an app. |
| Canyon's Postgres | Apps, runs, findings, approvals, the audit trail and sealed secrets. | Reached by the server alone. |
| `canyon-executor` | Runs each scan as a Job, each build through kpack, and each app as a Deployment with its own NetworkPolicy, all through the Kubernetes API. Writes an app's settings into a Secret it can create and delete, but never read. | The Kubernetes API, your registry, app pods on port 4183, and Canyon's egress gateway on 3128 for its check. Accepts connections from the server only. |
| Scan jobs | One per scanner, in `canyon-scans`, under a policy that lets nothing in or out. | Nothing, except that a Trivy scan asks Canyon's Trivy server, the one part here that downloads vulnerability databases from the internet. |
| kpack builds | Turn the reviewed source into an image with Paketo buildpacks, in `canyon-builds`. | DNS and public HTTP(S), to fetch dependencies. |
| `canyon-router` | Sends `<app>.preview.<domain>` and `<app>.prod.<domain>` (or `<app>-preview.<domain>` and `<app>-prod.<domain>`) to the right app. Your ingress and TLS certificate sit in front of it. | App pods on port 4180. |
| An app | Your code on port 3000, behind `canyon-identity`, a sidecar that checks who is signing in. When your own gateway doesn't sign people in, oauth2-proxy in the pod does, in front of the sidecar. | DNS, the apps' Postgres, and public addresses on 80 and 443. While outbound goes through Canyon, Canyon's egress gateway in their place, and the sign-in hosts in `CANYON_KUBE_SIGNIN_DIRECT`. Never the server, Canyon's Postgres or another app. |
| The apps' Postgres | One database and role per app that asks for one in `canyon.toml`. A separate server, on port 5433. | Nothing: the server and the apps reach it. |
| `canyon-egress` | Canyon's egress gateway, beta: Envoy with the egress check beside it. While an admin sends apps' calls through Canyon, it is an app's way out to the internet, records each call to the server, and under `allowlist` refuses what isn't allowed. DNS lookups and the sign-in hosts in `CANYON_KUBE_SIGNIN_DIRECT` still go out directly, unrecorded. | Public addresses on 80 and 443, and the server's receiver and policy. |

Who signs people in to an app on Kubernetes, your gateway or an oauth2-proxy beside each app, is the
front door an admin chooses: [How people reach apps](/self-host/front-door/) draws the request path
for each.

If your own platform already runs apps, Canyon can **hand off** instead: it still reviews and
builds, then sends a signed release to your pipeline rather than running the app.
[Choices at a glance](/concepts/modes/) lists this and every other choice, with its default.

## How a review decides

```mermaid title="The review: every way in goes through the same checks"
flowchart TD
  code["Your code<br>canyon deploy, canyon check,<br>a GitHub push or pull request"]
  snap["One snapshot<br>.env, node_modules and .git left out"]
  scan["Scanners, in parallel<br>gitleaks, Trivy, Opengrep,<br>ESLint, Checkov"]
  ai["AI review<br>skipped without a model"]
  reports["Your SARIF reports<br>--sarif, or GitHub code scanning"]
  f["Findings<br>each names a rule"]
  passed["Built and run<br>in preview"]:::passed
  blocked["Blocked<br>nothing is built"]:::blocked
  code --> snap
  snap --> scan
  snap --> ai
  scan --> f
  ai --> f
  reports --> f
  f -->|"nothing blocking"| passed
  f -->|"a blocking finding"| blocked
  blocked -->|"a person approves it, with a reason<br>and an expiry; then deploy again"| passed
```

- **Every way in is the same review.** An upload from the CLI and a push or pull request through
  the GitHub App become the same kind of snapshot and go through the same checks.
- **It fails closed.** A scanner that can't run, times out or has no database blocks the review as
  unchecked. It never passes in silence. With no model set, the AI review says it was skipped.
- **Your rules decide what blocks.** Every finding names a rule, and the org sets each rule to Off,
  Advisory or Blocking. A Blocking rule stops a deploy for its high and critical findings; Advisory
  findings are shown and don't stop anything.
- **A blocking finding can be approved** with a reason and an expiry date, on the app's page
  ([who can, and how](/build/fix-or-approve/#approve-it-instead)). The next deploy then builds.

## How a version gets to production

```mermaid title="From a passing review to production"
flowchart TD
  passed["A passing review"]
  pr["A pull request that passed"]
  build["Build<br>buildpacks"]
  preview["Preview<br>app.preview.your-domain"]:::passed
  checks["Production checks<br>running in preview, a passing review,<br>a person promotes"]
  waiting["Waiting for approval"]:::person
  prod["Production<br>the same image"]:::passed
  passed --> build
  build --> preview
  pr -->|"a person deploys it"| preview
  preview -->|"canyon deploy --env prod"| checks
  checks -->|"they pass"| prod
  checks -->|"where the org has a person<br>approve in the dashboard"| waiting
  waiting -->|"a person approves"| prod
```

- **Production runs the image preview ran**, with production's own secrets. Nothing is rebuilt.
- **The version must be running in preview**, and two rules stand in front of it, both blocking by
  default: its review passed, and a person promotes it, or an agent they ask with their own CLI
  sign-in.
- **An org can require a person's approval in the dashboard** for every promotion. Then
  `canyon deploy --env prod` ends `WAITING FOR APPROVAL` with a link, and a person on the app's
  team approves or denies it there.
- **A pull request's version can run in preview**, when a person deploys it from the app's
  page. It is never promoted: merge it, and deploy the default branch.
