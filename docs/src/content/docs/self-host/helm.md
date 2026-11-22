---
title: Try it on Kubernetes (Helm)
description: The demo Helm chart, deploy/helm/canyon, runs Canyon and everything it needs on one node. Not for production.
---

| | |
|---|---|
| **Time** | Under an hour on a fresh node, most of it waiting for images and kpack's builder |
| **You need** | One Kubernetes or k3s node, with ports 80 and 443 reachable from the internet: the bundled gateway gets its certificates over HTTP-01 · a domain whose `<domain>`, `*.preview.<domain>` and `*.prod.<domain>` point at the node · `helm` and `kubectl` · the digests from the release's `images.txt` ([What you must set](#what-you-must-set)) |
| **You get** | Canyon at `https://<domain>`, its own sign-in provider (Pocket ID) at `https://auth.<domain>`, and apps at `https://<app>.preview.<domain>` and `https://<app>.prod.<domain>`, with the registry, both Postgres servers, kpack and the gateway in the cluster |
| **Not covered** | Production: the bundled databases have no backups |

**Not for production.** This chart runs Canyon and everything it needs on a single node, so you can
try it. Its registry, sign-in and gateway are the simplest that work. To run Canyon for real, install
it with [`deploy/k8s`](/self-host/kubernetes/), on your own registry, Postgres, identity provider and
load balancer.

One release installs Canyon and, each switchable, what [Install on Kubernetes](/self-host/kubernetes/#you-bring) says you bring: a
registry, both Postgres servers, kpack, an OIDC provider (Pocket ID), a TLS gateway (Caddy) and
Headlamp. Every setting is a value (`values.yaml`, each key commented). The objects are
`deploy/k8s`'s, with the same invariants.

```mermaid title="The demo Helm chart: Canyon and everything it needs, on one node, from one release"
flowchart TD
  people["People and coding agents"]
  subgraph node["One Kubernetes node"]
    caddy["Caddy, the gateway<br>canyon-gateway"]
    canyon["Canyon, deploy/k8s's objects<br>its four namespaces"]:::canyon
    pocket["Pocket ID, the sign-in provider<br>canyon-auth"]
    headlamp["Headlamp<br>a cluster dashboard"]
    kpack["kpack"]
    reg["Registry<br>canyon-infra"]
    pg[("Canyon's Postgres<br>canyon-infra")]
    apg[("The apps' Postgres<br>canyon-infra")]
  end
  people --> caddy
  caddy --> canyon
  caddy --> pocket
  caddy --> headlamp
  canyon -->|"sign-in"| pocket
  canyon --> pg
  canyon --> apg
  canyon --> kpack
  kpack --> reg
```

Start from the chart's own values, and keep secrets apart from them, in a file only you can read:

```sh
helm show values https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-chart.tgz > values.yaml
touch values.secret.yaml && chmod 600 values.secret.yaml
```

Set what *What you must set* lists in `values.yaml`, and any secret you choose yourself (*Secrets*) in
`values.secret.yaml`, which may stay empty.

The first install is two steps. Canyon's egress gateway starts only once the certificate the server mints at
its first start is published (*The egress check's policy*, below), so `--wait` on the first install would wait
on it for ever. Install without it, publish the certificate, then wait on the gateway:

```sh
helm install canyon https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-chart.tgz -n default -f values.yaml -f values.secret.yaml --timeout 15m
kubectl -n canyon rollout status deploy/canyon-server --timeout=15m
cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) &&
  kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n canyon rollout status deploy/canyon-egress --timeout=10m
```

The hooks (below) wait for what they need themselves, so the install finishes without `--wait`. Once
the bootstrap Job has run, `kubectl -n canyon-auth logs job/canyon-bootstrap` prints a one-time
sign-in link for the first user: open it to register a passkey, then sign in at `https://<domain>`.
To upgrade, see [Back up, restore and upgrade](/self-host/backup-and-upgrade/#with-the-helm-chart).

The chart creates its namespaces itself (`canyon`, `canyon-apps`, `canyon-builds`, `canyon-scans`,
`canyon-infra`, `canyon-auth`, `canyon-gateway`, `kpack`). The release lives in any other namespace.
The front door, its two ways of signing people in and the address form are defined in
[How people reach apps](/self-host/front-door/).

## What you must set

| Value | |
|---|---|
| `global.domain` | Apps are `<app>.preview.<domain>` / `<app>.prod.<domain>`. Canyon is `https://<domain>` unless `canyon.publicURL` says otherwise. |
| `images.server`, `images.executorKube`, `images.identity`, `images.run`, `images.build` | Canyon's five images, each `{repository: ghcr.io/getcanyon-ai/<name>, digest: "sha256:…"}` with its digest from the release's `images.txt`: `canyon-server`, `canyon-executor-kube`, `canyon-identity`, `canyon-run` (the run image every app runs on) and `canyon-build` (the build image every app builds on). They are public, and your nodes pull them without a login. Every image is rendered by digest, or the render fails. |
| `images.opengrep`, `images.eslint` | Canyon's scanner images, `ghcr.io/getcanyon-ai/canyon-opengrep@sha256:…` and `ghcr.io/getcanyon-ai/canyon-eslint@sha256:…`, by the digests in `images.txt`. The chart renders without them, but **every deploy is then blocked**: both scanners are on in the default Guardrails, and a scan with no image blocks the review (`gate.unreadable`). The install notes warn when either is empty. |
| `canyon.auth.admins` | The first admins (`CANYON_ADMINS`), the only people who can sign in until setup is finished, by an admin on the setup screen or by `bootstrap.completeSetup`. Who else may sign in is set on that screen, or fixed here with `allowedEmails` or `allowedDomains`. |
| `auth.pocketId.users` | With the bundled Pocket ID: the people who may sign in. The bootstrap signs in to Canyon as the first one. |

## Secrets

- **Passwords and keys left empty are generated once.** Each upgrade reads them back from the live
  Secret (`lookup`), so they never rotate by accident. This covers both Postgres passwords, Pocket
  ID's `ENCRYPTION_KEY` and `STATIC_API_KEY`, the two OIDC client secrets, the token Canyon's
  egress gateway sends the server's receiver (`canyon/canyon-egress`, key `token`), which both read,
  and the token the bundled gateway asks Canyon with before each certificate (`canyon/canyon-tls-ask`,
  copied to `canyon-gateway/caddy-tls-ask`). `lookup` reads the cluster only under `helm install` and
  `helm upgrade`, as Flux's helm-controller runs them. **Under Argo CD, or anything that renders with
  `helm template`, you must set `canyon.egress.token`**, a random value such as `openssl rand -base64 32`
  writes, **or bring the Secret with `canyon.egress.existingSecret`**, **and set `gateway.askToken`**
  (`openssl rand -hex 32`): left empty, every render generates new tokens, and the gateway and the
  server never agree on one for long. The same holds for every generated value above: set each in
  values there.
- **Or set them** in a `values.secret.yaml` you keep with mode 600.
- **Or bring a Secret:**
  - `canyon.existingSecret` replaces the server's whole environment.
  - `infra.*Postgres.existingSecret` holds key `password`.
  - `auth.pocketId.existingSecret` holds `ENCRYPTION_KEY` and `STATIC_API_KEY`.
  - `canyon.builder.registryCredential.existingSecret: true` means you made `canyon-builds/canyon-registry`.
  - `canyon.egress.existingSecret: true` means you made `canyon/canyon-egress`, key `token` (32 to 512 of
    `[A-Za-z0-9._~+/=-]`, as `openssl rand -hex 32` writes); set `canyon.egress.token` instead to keep it in values.
- **A new egress token from values rolls both pods that read it.** With `canyon.egress.token` set, the
  gateway's and the server's pod templates carry its sum (`checksum/egress-token`), so changing it restarts
  both. A generated token never changes, and a brought Secret is yours: neither carries a sum, so after
  changing a brought one restart both, `kubectl -n canyon rollout restart deploy/canyon-egress
  deploy/canyon-server`.
- **The gateway's ask token** only lets a caller ask Canyon which app names exist. Canyon never logs it,
  but Caddy can't send it in a header, so it rides in the ask URL, and Caddy's own error log can carry
  that URL when Canyon refuses an ask, such as a renewal for a deleted app. To rotate a generated one,
  delete `canyon/canyon-tls-ask`, run `helm upgrade`, which generates a new one into both Secrets, then
  `kubectl -n canyon rollout restart deploy/canyon-server` and `kubectl -n canyon-gateway rollout
  restart deploy/caddy`. A `gateway.askToken` from values rolls both itself when you change it.
- **The chart's `canyon-egress` Secret is kept** (`helm.sh/resource-policy: keep`): uninstalling, or
  switching to `canyon.egress.existingSecret`, leaves it in place, so the gateway and the server keep their
  token through the upgrade; delete it yourself once you no longer need it.
- **Kept on uninstall:** the namespaces, the volumes, and the Secrets a database was initialised with
  (`helm.sh/resource-policy: keep`). The server's volume holds the key every Canyon secret is sealed
  under. Back it up with the rest.

## The egress check's policy

Canyon's egress check pulls each app's allowlist from the server on port 7782, over TLS under a certificate
the server mints at its first start, with a token of the gateway's own ServiceAccount, which the server checks
against your cluster's key set. Two things are yours:

- **`canyon.kubeAPI.cidrs`**: the server reads that key set from the API server, on 443 or 6443, and its
  NetworkPolicy admits only these ranges there. List both your API server's endpoint addresses, `kubectl
  get endpoints kubernetes`, and the `kubernetes` Service's cluster IP as a `/32`, `kubectl get svc
  kubernetes -o jsonpath='{.spec.clusterIP}'`: some CNIs match a pod's packet before kube-proxy rewrites the
  cluster IP (the VPC CNI's network policy, which EKS uses), others after. On EKS the endpoints move when
  the control plane is upgraded, so list the cluster's subnets in their place. With `platform.k3s.enabled`
  it defaults to `platform.k3s.apiServer`; elsewhere, left empty, the chart refuses to render: the server
  would reach no API server, every pull of the policy would be refused, and the gateway would never be
  ready. Canyon v0.2.0's chart made it a list; before, it was `canyon.kubeAPI.cidr`, one range.
- **Publish the certificate** once the server is up, for the gateway to pin, before you wait on the gateway:
  the first install's steps above.

  It holds no secret, and running the step again is safe. It lasts ten years. To renew it, delete its row on Canyon's Postgres (`DELETE FROM egress_policy_cert;`), restart the server, which mints another, run the step again, and restart the gateway (`kubectl -n canyon rollout restart deploy/canyon-egress`) so it pins the new one.

## Hooks

- **`canyon-builder-apply`** (post-install/upgrade) applies kpack's objects with server-side apply:
  the ClusterStack, ClusterStore and ClusterBuilder `canyon`, and kpack's default ClusterLifecycle.
  It retries until kpack's webhook answers, which replaces the install guide's "apply twice". These objects
  update in place and are not deleted on uninstall.
- **`canyon-bootstrap`** (post-install/upgrade, `files/bootstrap.py`, needs the bundled Pocket ID):
  1. Sets up Pocket ID through its API: the users, the group `auth.pocketId.group`, and the clients
     `canyon` (`<publicURL>/auth/callback`) and `canyon-apps` (one wildcard callback per target, for
     the oauth2-proxy). Each client uses the ID and secret from values, and only the group may use it.
  2. Signs in to Canyon as the first user: a one-time Pocket ID token, then Canyon's own OIDC flow.
  3. `PUT /api/v1/preferences` from `preferences`, if set, and finishes setup when
     `bootstrap.completeSetup` is set.
  4. Once setup is finished, `PUT /api/v1/front-door` from `frontDoor` when it differs, when the stored one
     isn't seen enforcing NetworkPolicy, or every time with `frontDoor.forceUpdate`. Canyon probes it first
     and writes the audit row. A refused probe fails the Job with Canyon's message, except for an unchanged
     front door saved again because it isn't enforcing: when the probe refuses it, or can't run (the executor may
     still be rolling out), the Job logs Canyon's message and the upgrade completes, and nothing builds until a
     save passes. So `frontDoor.forceUpdate` is no longer needed after an upgrade, and a refusal of a door that
     isn't enforcing no longer fails the Job. With a platform webhook or a deploy repo saved, a `choice: 1` door that fails
     the probe is saved *not checked on this cluster* instead, and Canyon runs no app behind it
     ([Your platform runs the apps](/self-host/hand-off/)). While Canyon is still in setup, the Job logs that the front
     door waits: finish setup in the dashboard, then run `helm upgrade`. After the upgrade that checks
     NetworkPolicy before a build ([Upgrade](/self-host/backup-and-upgrade/#the-upgrade-that-checks-networkpolicy-before-a-build)),
     a plain `helm upgrade` saves the front door again.
  5. Prints a one-time sign-in link for the first user, so they can register a passkey.

  `kubectl -n canyon-auth logs job/canyon-bootstrap` shows what it did. Without the bundled Pocket ID,
  set the front door by hand under **Settings › App hosting**.

## Connect your agents

Your developers connect their coding agents in one of two ways ([Connect your agent](/start/connect-your-agent/)):

- **On a laptop:** `canyon login https://<domain>`, then `canyon skill install`. It sets up Canyon's skill, its
  session hook and its MCP server for each agent it finds.
- **In the cloud:** Claude Code on the web and Cursor's cloud agents connect to `https://<domain>/mcp`, and each
  person allows it once in the browser. That needs Canyon reachable from the internet, Canyon's GitHub
  App on your repositories ([Connect GitHub](/build/github/)), and each person's GitHub account linked.

## Platform notes

- **`canyon.patching`** is `CANYON_PATCHING`, **beta**, `"off"` by default, and `canyon.patchConcurrency`
  (`CANYON_PATCH_CONCURRENCY`, beta too) counts only while it is `"on"`: quote it, as YAML reads a bare
  `off` as false, which the server refuses. Turning it on lets Canyon rebase an app's preview onto a newer run image and redeploy it on its own, then watch it for `CANYON_PATCH_WATCH`: if the patched app exits or restarts, Canyon puts back the image it replaced. Canyon patches one app's preview for each new run image first, and more previews in waves once a watch passes. It never patches production on its own: once a patched preview's watch passes, Canyon asks for production with a production approval, which a contributor or above on the app approves in the dashboard, and asks again each rescan while the patch still applies. Once approved, production is watched for `CANYON_PATCH_WATCH` too, and a crash there puts back the image production ran.

- **`platform.k3s.enabled`** adds the egress rules k3s needs:
  - the executor to the API server at `platform.k3s.apiServer:6443`
  - the executor and builds to the in-cluster registry
  - the server and app pods to the gateway on 443, because the bundled hosts resolve to the node itself.
    Apps routed through Canyon's egress gateway are left out, and the chart opens no way for them to
    reach the gateway: kube-proxy turns the node's address into Caddy's pod address, which the egress
    gateway refuses. So with the bundled Pocket ID as the apps' sign-in, Canyon refuses to route apps
    through its egress gateway (**Settings › Outbound** says why), and saving that front door while they are
    routed shows a warning. Use your own sign-in provider for apps, or leave egress off. The chart tells
    the server through `CANYON_KUBE_GATEWAY_SIGNIN`, set on the server container itself, with
    `canyon.existingSecret` too.
- **An internal sign-in provider for routed apps**
  ([Kubernetes](/self-host/kubernetes/#a-private-sign-in-provider-for-apps-routed-through-canyon)) is `canyon.egress.signin`. `hosts` becomes the executor's
  `CANYON_KUBE_SIGNIN_DIRECT`. `inCluster: {namespace, podLabels, port}` renders the `canyon-apps-signin`
  NetworkPolicy, which admits routed pods to the provider's own pods on that port. Point it at those pods,
  never at an ingress's or a gateway's. The render fails on a namespace the chart writes or installs into
  (Canyon's own, Caddy's, Pocket ID's, kpack's, headlamp's) or `kube-system`, on a port that isn't a whole
  number from 1 to 65535, and on an empty host. The executor refuses to start on a host it doesn't take.
- **On k3s, containerd must pull from the bundled registry over plain http.** Configure that in
  `/etc/rancher/k3s/registries.yaml`, mirroring `<infra.registry.clusterIP>:5000`.
- **On k3s, pods need a resolver they can reach.** k3s hands CoreDNS the host's resolver, and one only
  the host can reach (Tailscale MagicDNS's `100.100.100.100`) times out every lookup. Set k3s's
  `resolv-conf` before installing, as [the k3s guide's step 1](/self-host/k3s/#1-k3s) shows.
- **If the ClusterBuilder never becomes ready** and `kubectl get clusterlifecycle default-lifecycle`
  shows a lookup error from before the network worked, run
  `kubectl -n kpack rollout restart deploy/kpack-controller`. kpack doesn't retry it by itself.
- **The bundled gateway asks Canyon before each certificate.** Caddy issues an app's certificate on
  the app's first visit, and first asks Canyon, which says yes only for Canyon's own host, an app that
  exists and the two addresses the front-door check opens. So nobody can make Caddy request
  certificates for made-up names and spend Let's Encrypt's weekly limit for your domain. Caddy proves
  it is the one asking with a token the chart generates and keeps across upgrades, in the Secrets
  `canyon/canyon-tls-ask` and `canyon-gateway/caddy-tls-ask`.
- **With the bundled gateway, all sign-ins share one rate limit, and every audit row's `source` is
  Caddy's address.** Caddy is a pod with no fixed address, so `CANYON_TRUSTED_PROXIES` stays unset and
  Canyon sees every request as coming from Caddy.
- **Canyon's own images come from a registry your nodes can pull from.** Point `images.*` at
  `ghcr.io/getcanyon-ai`, where they are public, or copy them into your own registry with `crane copy`,
  which keeps each digest, as [Install on Kubernetes](/self-host/kubernetes/#canyon-ships) shows.
