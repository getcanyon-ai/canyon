---
title: Choices at a glance
description: The six choices a Canyon install makes, each with its default, who sets it, a picture and when to pick which, linked to the page that sets it up.
---

A Canyon install makes six choices. Every install decides where Canyon runs and, on Kubernetes, how
people reach apps. The rest have a default that's right to start with. Each section below is a
picture and when to pick which; the page it links sets the choice up.

| Choice | Options | Default | Who sets it, and where |
|---|---|---|---|
| [Where Canyon runs](#where-canyon-runs) | Try it: laptop demo · Helm chart. Run it: one server, plain Docker · one server, step by step (k3s) · your Kubernetes cluster | None | Whoever installs it |
| [How people sign in to Canyon](#how-people-sign-in-to-canyon) | Your identity provider (`oidc`) · no sign-in (`off`), only on one machine, with Canyon listening on loopback. The laptop demo has [a setting of its own](/start/try-it-on-your-laptop/#sign-in-is-off) | `oidc` | The server's `CANYON_AUTH` |
| [How people reach apps](#how-people-reach-apps), the front door | On Kubernetes: your OIDC gateway · your AWS load balancer · Canyon. On one Docker host: Canyon's server | None on Kubernetes: no app runs until it's set | An admin, **Settings › App hosting** |
| [Who runs the apps](#who-runs-the-apps) | Canyon · your own platform (hand-off) | Canyon | An admin: [Your platform runs the apps](/self-host/hand-off/) |
| [Outbound](#outbound-beta) (beta) | Off · through Canyon, under `observe` or `allowlist` | Off, and `observe` | An admin, [**Settings › Outbound**](/org/outbound/); an app's owner can make one environment stricter |
| [Patching](#patching-beta) (beta) | Off · on | Off | The server's `CANYON_PATCHING` |

## Where Canyon runs

```mermaid title="Where Canyon runs: two places to try it, three to run it"
flowchart TD
  want["What do you want from Canyon?"]
  subgraph try["Try it: nothing to keep"]
    demo["Your laptop<br>docker compose, sign-in off"]:::canyon
    helm["One Kubernetes or k3s node<br>the Helm chart, everything bundled"]:::canyon
  end
  subgraph run["Run it: your sign-in, your backups"]
    docker["One server, plain Docker<br>docker compose, sign-in on"]:::canyon
    k3s["One server, step by step<br>k3s, the install base"]:::canyon
    k8s["Your Kubernetes cluster<br>the install base and your overlay"]:::canyon
  end
  want -->|"see it work, today"| demo
  want -->|"try it on Kubernetes"| helm
  want -->|"a team, on one Docker server"| docker
  want -->|"a team, on one server, on Kubernetes"| k3s
  want -->|"production"| k8s
```

**To try it**, pick one of these. Nothing on them is worth keeping, and neither is for production.

**To run it**, pick one of these. People sign in through your identity provider, and you back up
what Canyon keeps ([Back up, restore and upgrade](/self-host/backup-and-upgrade/)). One server, on
Docker or k3s, is one point of failure. On Docker, Canyon's executor holds the server's Docker socket,
so the server runs Canyon and nothing else ([Security](/self-host/docker/#security)). Your Kubernetes
cluster is the production setup; EKS with ECR is tested.

If your platform already runs apps, Canyon can review and build each release, then hand it off to
your platform to run: [Your platform runs the apps](/self-host/hand-off/).

| | Laptop demo | Helm chart | One server (Docker) | One server (k3s) | Your cluster |
|---|---|---|---|---|---|
| For | Trying it | Trying it | A team, on one server ([Caveats](/self-host/docker/#caveats)) | A team, on one server ([Caveats](/self-host/k3s/#caveats)) | Production |
| People sign in to Canyon | Sign-in is off | The Pocket ID the chart brings, or your provider | Your identity provider | Your identity provider | Your identity provider |
| Registry, databases, gateway | Both databases bundled; no registry or gateway needed | Bundled, with no backups | Both databases bundled, with your backups; Caddy in the stack; no registry needed | In the cluster, from the guide | Yours |
| Runs apps with | `canyon-executor`, Docker | `canyon-executor-kube` | `canyon-executor`, Docker | `canyon-executor-kube` | `canyon-executor-kube` |
| Builds with | `pack` | kpack | `pack` | kpack | kpack |
| Guide | [Try it on your laptop](/start/try-it-on-your-laptop/) | [Try it on Kubernetes (Helm)](/self-host/helm/) | [One Docker server](/self-host/docker/) | [One k3s host](/self-host/k3s/) | [Install on Kubernetes](/self-host/kubernetes/) |

The server learns which runtime it's on from `CANYON_RUNTIME`: `docker` (the default) or
`kubernetes`, which the Kubernetes manifests and the Helm chart set.
[How Canyon works](/concepts/how-canyon-works/) draws both.

## How people sign in to Canyon

This is sign-in to Canyon itself: the dashboard and the CLI. Sign-in to the apps Canyon runs is
[the next choice](#how-people-reach-apps).

```mermaid title="Signing in to Canyon: through your identity provider, or not at all on one machine"
flowchart TD
  subgraph oidc["oidc, the default: anyone other people can reach"]
    person["A person, in a browser or the CLI"]
    server1["canyon-server"]:::canyon
    idp["Your identity provider<br>Entra, Okta, Google, any OIDC"]
    person --> server1
    server1 <-->|"sign-in"| idp
  end
  subgraph off["off: one person, on one machine"]
    you["You, on the same machine"]
    server2["canyon-server<br>listens on 127.0.0.1 only<br>everyone is an admin"]:::canyon
    you -->|"localhost"| server2
  end
```

- **Pick `oidc`** for every Canyon that anyone but you can reach. People sign in through your own
  identity provider, Canyon knows who did each thing, and the CLI signs in with a code a person
  approves in the browser. It needs an issuer, a client and the first admins:
  [Sign-in, the AI model and keys](/org/set-up/).
- **Pick `off`** only to run Canyon for yourself on one machine. There is no sign-in and everyone is
  an admin, so Canyon refuses to start unless it listens on loopback and `CANYON_PUBLIC_URL` is a
  loopback address too. With it, patching never runs and apps get no identity token.

Canyon has no passwords and no local accounts, not even one for an admin. Everyone signs in through
an identity provider. If nobody can sign in, an admin fixes the provider's settings or
`CANYON_ADMINS` and restarts the server. A standing admin password would be a credential that never
expires, kept only to save that five-minute restart. No identity provider yet? Any OIDC provider
works, including a small one you run yourself, such as the Pocket ID the Helm chart brings. To try
Canyon alone on your own machine, use `off`.

The laptop demo comes with sign-in off. [Try it on your laptop](/start/try-it-on-your-laptop/#sign-in-is-off)
says how its setting differs. On Kubernetes Canyon listens beyond loopback, so only `oidc` works there.
The Helm chart runs `oidc` only, and refuses `off` and the demo's value in `canyon.auth.mode`.

## How people reach apps

The **front door** is how people get to an app: the address it answers at, and who signs them in
before the app sees them. [How people reach apps](/self-host/front-door/) defines it, draws each
choice and says what your gateway does.

```mermaid title="The front door on Kubernetes: something signs the person in, then Canyon's sidecar checks the app's access list"
flowchart TD
  browser["Browser"]
  signin["Who signs people in<br>your gateway, your AWS load balancer,<br>or oauth2-proxy beside the app"]
  sidecar["canyon-identity, in the app's pod<br>checks the app's access list"]:::canyon
  app["Your app<br>reads Canyon's identity token"]
  browser --> signin
  signin --> sidecar
  sidecar --> app
```

- **On one Docker host there is nothing to choose:** Canyon's server is every app's front door.
- **On Kubernetes, an admin picks who signs people in** in **Settings › App hosting**, and no app
  runs until it's saved:
  - **Your gateway signs people in, as a standard OIDC gateway**, if an OIDC proxy already sits in
    front of your internal apps.
  - **Your gateway signs people in, as AWS load balancer sign-in**, if your apps sit behind an AWS
    Application Load Balancer that signs people in.
  - **Canyon signs people in** if your gateway only routes and ends TLS: oauth2-proxy runs beside
    each app, against an OIDC client you register.

## Who runs the apps

```mermaid title="Who runs the apps: Canyon, or your own platform after Canyon reviews and builds"
flowchart TD
  built["Canyon reviews the code<br>and builds the image"]:::canyon
  canyon["Canyon's executor runs the app<br>run_by: canyon, the default"]:::canyon
  release["Canyon sends a signed release<br>run_by: org"]:::canyon
  platform["Your platform runs the image<br>with canyon-identity beside it,<br>and reports back"]
  built --> canyon
  built --> release
  release --> platform
```

- **Leave it with Canyon** (`run_by: canyon`) unless you already have a deployment platform.
- **Hand off** (`run_by: org`) if Argo CD, Flux or your own CI should run the apps. Canyon still
  reviews, builds and pushes each image, and an environment is live only once your platform reports
  the exact image Canyon reviewed running. It needs Canyon on Kubernetes, the platform webhook saved,
  a front door set to **Your gateway signs people in** (not **Canyon signs people in**), and two
  settings on Canyon's server: `CANYON_IMAGE_IDENTITY`, the sidecar image every release names, and an
  `https` `CANYON_PUBLIC_URL`, which the sidecar pulls the app's access list from.

Under hand-off, Canyon keeps the review, the build and who may open each app. Your side runs the
app, signs people in and holds the secrets:

```mermaid title="Hand-off: Canyon reviews, builds and keeps the access list; your platform runs the app, and the sidecar pulls the list from Canyon"
flowchart TD
  server["canyon-server<br>review, approvals,<br>who may open each app"]:::canyon
  executor["canyon-executor<br>scans and builds"]:::canyon
  registry[("Your registry")]
  platform["Your platform<br>Argo CD, Flux or your CI"]
  store[("Your secret store")]
  users["People using the app"]
  gw["Your gateway<br>signs people in"]
  subgraph pod["The app's pod, which your platform runs"]
    sidecar["canyon-identity<br>port 4180"]:::canyon
    app["Your app, port 3000"]
  end
  net["Your outbound"]
  server -->|"builds"| executor
  executor -->|"pushes the image, by digest"| registry
  server -->|"the release, signed,<br>to your webhook"| platform
  platform -.->|"fetch the access token,<br>report, with the host token"| server
  store -->|"secret values and<br>CANYON_ACCESS_TOKEN"| platform
  registry -->|"pulled by digest"| platform
  platform -->|"runs"| sidecar
  users --> gw
  gw -->|"the person's token"| sidecar
  sidecar -.->|"pulls the access list, https,<br>at start and every minute"| server
  sidecar -->|"Canyon's identity token"| app
  app --> net
```

At runtime your cluster reaches Canyon for one thing: each sidecar pulls its app's access list from
`CANYON_PUBLIC_URL` over `https`, when it starts and every minute. Your pipeline reaches it for each
release, to fetch the access token and to report back. Nothing else calls Canyon. The sidecar signs the
identity token with a key it makes when it starts, and the app reads that key from the sidecar,
inside the pod. [Your platform runs the apps](/self-host/hand-off/#what-canyon-still-does) lists
what stays with Canyon and what becomes yours.

See [Your platform runs the apps](/self-host/hand-off/).

## Outbound (beta)

Outbound decides how apps call out to the internet. Through Canyon, every call goes through Canyon's
**egress gateway**, and the **egress check** beside it decides whether each call may go.

```mermaid title="Outbound through Canyon: the egress gateway asks the egress check before each call, and records it"
flowchart TD
  app["Your app<br>HTTPS_PROXY set to the gateway"]
  envoy["Canyon's egress gateway<br>Envoy, port 3128"]:::canyon
  check["Egress check<br>observe or allowlist,<br>from the server's policy"]:::canyon
  server["canyon-server<br>records each call"]:::canyon
  net["Public addresses<br>ports 80 and 443 only"]
  refused["403<br>Canyon-Egress: not_allowed"]
  app --> envoy
  envoy -->|"may this call go?"| check
  envoy -->|"allowed"| net
  envoy -->|"refused, under allowlist"| refused
  envoy -.-> server
```

| How apps call out | What it does |
|---|---|
| **Off** (`off`, the default) | Apps call out directly. On Docker, an app can then reach the cloud's metadata address and the host itself. |
| **Through Canyon** (`canyon`) | Apps call out through Canyon's egress gateway, which records each destination. Nothing else gets out, except on Kubernetes: there, DNS lookups and the sign-in hosts in `CANYON_KUBE_SIGNIN_DIRECT` also go out directly, unrecorded. |

| Mode, through Canyon | What it does |
|---|---|
| `observe` (the default) | Lets a call to a host name on port 80 or 443 through and records it. Like `allowlist`, it refuses IP addresses, single-label names and other ports, and holds an app environment to 30 new destinations a minute |
| `allowlist` | Lets a call through only to a destination on the environment's allowlist, or to a platform destination. Every other call gets `403` with the header `Canyon-Egress: not_allowed` |

- **Leave outbound off** while you start.
- **Send calls through Canyon, under `observe`**, to see every destination your apps call, recorded
  per app.
- **Switch to `allowlist`** once you know what your apps call, and have allowed what they need.

[Outbound](/org/outbound/) has the steps, who may change what, and each app's *Outbound* card.

## Patching (beta)

Under every app's own code is what Canyon puts there: the system libraries the app runs on, such
as OpenSSL, and the build tools: Node, Python or Java, and the launcher that starts the app. The rule
[`platform.known-vuln`](/rules/platform.known-vuln/), off by default, counts known-vulnerable
packages in all of them, at promotion. Patching fixes only the system libraries, by moving a preview
onto Canyon's newer ones, and acts only on apps where that rule is on. On Docker the newer system
libraries come with a new Canyon release, or your own `CANYON_IMAGE_RUN`; on Kubernetes they are the
run image of kpack's ClusterStack, which the Helm chart sets with `images.run`.

```mermaid title="Patching: Canyon checks the system libraries and build tools under apps with platform.known-vuln on; only with patching on does it fix a preview"
flowchart TD
  rule["platform.known-vuln is on for the app<br>off by default"]
  rematch["Every 6 hours, Canyon checks the packages it put<br>in the app's images against the newest<br>vulnerability database"]:::canyon
  off["Patching off, the default<br>nothing is patched; the rule reads<br>at promotion"]
  on["Patching on, with sign-in on<br>for a finding in the system libraries, Canyon moves<br>the app's preview onto its newer ones,<br>through the same review, and redeploys it"]:::canyon
  prod["Production<br>Canyon asks; the app's team approves<br>the patched image in the dashboard"]:::person
  rule --> rematch
  rematch --> off
  rematch --> on
  on --> prod
```

| `CANYON_PATCHING` | What it does |
|---|---|
| `off` (the default) | Canyon still checks the packages it put in the images of every app where `platform.known-vuln` is on against the newest vulnerability database, and no patch runs |
| `on` | With sign-in on (`CANYON_AUTH` `oidc`) only, Canyon moves an app's preview onto newer system libraries and redeploys it on its own, then watches it for `CANYON_PATCH_WATCH` (15 minutes by default): if the patched app exits or restarts, Canyon puts back the image it replaced. It patches one app's preview for each new fix first, and more previews in waves once a watch passes. Once a patched preview's watch passes, Canyon asks for production: the app's team approves the patched image in the dashboard, and Canyon watches production the same way |

- **Leave it off** unless you want to try it.
- **Turn it on** to see what a patch does. Patching never
  touches production on its own: Canyon files a production approval for the patched image, a
  contributor or above on the app approves it in the dashboard, and Canyon records the promotion as a
  patch. Production held is never asked, a denial stands until a newer fix is out, and an approval that
  expires is asked again while the patch still applies. Once approved, production is watched as
  preview was: if the patched app exits or restarts there, Canyon puts back the image production ran.
- **The org's `patching`**, `ask` by default, lets an admin stop patching for the whole org without a
  restart: `off` stops every patch before it deploys and asks nothing. It reads `off` while sign-in is
  off. Either way, an app is patched only while its own *No known-vulnerable system libraries or
  build tools* rule is on, an app's stricter addition included: once it turns off, the app's open patch
  approval can no longer be approved.

On Kubernetes, the server's own environment sets `CANYON_PATCHING` to `off`, so turn it on with your
overlay; with Helm, use `canyon.patching`, quoted. The
[configuration reference](/self-host/configuration/#builds-and-scans) has it and its three related
settings.

## Beta

A beta feature works, but it isn't complete or fully tested yet. The dashboard marks it with a `Beta`
chip, whose title reads *This works, but it isn't complete or fully tested yet. Tell us what you find.*
The CLI says `(beta)` on the feature's row.

In this release, three features are beta:

- [outbound](/org/outbound/)
- [`platform.known-vuln`](/rules/platform.known-vuln/)
- [patch runs](#patching-beta)

**Next:** [Try it on your laptop](/start/try-it-on-your-laptop/) to see Canyon work, or
[Install on Kubernetes](/self-host/kubernetes/) to run it for real.
