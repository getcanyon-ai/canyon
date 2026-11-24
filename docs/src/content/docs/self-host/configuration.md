---
title: Configuration reference
description: Every environment variable the Canyon server, the Docker executor and the Kubernetes executor read, with its default, what it accepts, and what Canyon sets for each app.
---

Canyon runs as a server and an executor. The server holds Canyon's records, reviews code and serves
the dashboard. The executor builds, scans and runs apps: `canyon-executor` on a single Docker host,
`canyon-executor-kube` on Kubernetes. Each reads its settings from its environment when it starts.

Almost every value Canyon can't use stops it at start, with a message that names the setting, so a
mistyped value fails the new process instead of reaching a running one.

The [Kubernetes install guide](/self-host/kubernetes/)
says where each setting goes on a cluster: the executor's Deployment, the server's Deployment, or
the `canyon-server` Secret. [Sign-in, the AI model and keys](/org/set-up/) walks through the
sign-in and model settings. [Choices at a glance](/concepts/modes/) lists the settings that
change how Canyon behaves as a whole, with their defaults and what's beta.

## Server

`canyon-server`, on both Docker and Kubernetes.

### Where it listens and how it's reached

| Variable | What it does |
|---|---|
| `CANYON_ADDR` | The address the server listens on, as `host:port`. Default `127.0.0.1:7777`. The Kubernetes manifests set `:7777`. |
| `CANYON_PUBLIC_URL` | The address people open Canyon at, `http://host[:port]` or `https://host[:port]` and nothing else: no path, query or credential. A trailing slash is dropped. It is also the issuer of the identity token every app receives, and the scheme each app is told it is reached by (`X-Forwarded-Proto`), whatever scheme the request reached the server by. With sign-in on (`oidc`) it is required and `https`, unless its host is loopback: unset, or `http` on any other host, Canyon refuses to start. With `off` and `demo` the default is `http://localhost:7777`, and its host must be loopback. |
| `CANYON_RUNTIME` | Where apps run: `docker` (the default) or `kubernetes`. Any other value stops the server. Set `kubernetes` on a cluster. |
| `CANYON_EXECUTOR_URL` | Where the server reaches the executor. Default `http://executor:7778`. |
| `CANYON_TRUSTED_PROXIES` | Optional. The proxies in front of the server, as comma-separated CIDRs or addresses. Only a request that arrives from one of them is believed about its client through `X-Forwarded-For`, which the sign-in rate limit and the audit log's source use. Write IPv4 addresses as plain IPv4. Unset, the connection's own address is the client. Never list a range that holds other hosts: any address in it could claim to be anyone. |
| `CANYON_TLS_ASK_TOKEN` | Optional. A token that lets a TLS proxy with no fixed address ask the server which names may get a certificate (`GET /tls/ask?token=…&domain=…`), as the Helm chart's Caddy does: 32 to 512 characters of `A-Z`, `a-z`, `0-9`, `.`, `_`, `~` and `-`. The server refuses to start on anything else. Unset, only loopback and `CANYON_TRUSTED_PROXIES` may ask. The server never logs it. |
| `CANYON_PUBLISHED_ADDR` | Optional. The `host:port` your host publishes the server's port on, with a numeric port. Only the laptop demo's sign-in setting (`CANYON_AUTH=demo`) uses it: to tell the dashboard whether that Canyon is reachable beyond this machine, and to serve the Docker network's gateway the published port arrives from. It must be a loopback address, such as `127.0.0.1:7777`: any other, `0.0.0.0` included, stops Canyon from starting, because Docker would hand a remote client the gateway's address. Canyon doesn't take its word for it: it asks Docker, through the executor, where the server's port is published, and serves the gateway only once every binding of it is on loopback. A binding on any other address, or none at all, stops Canyon with a log line naming the binding. Until the executor answers, and when this is unset, the demo serves loopback connections alone. Keep it equal to the server's `ports:` line in `docker-compose.yml`. |
| `CANYON_APPS_JWKS_ADDR` | Docker. The `host:port` an app reaches the server at from its own network. Default `canyon-server:7780`. The server listens on that port on every interface and serves only the key set that signs identity tokens there; apps on Docker get its address as `CANYON_IDENTITY_JWKS_URL`. |
| `CANYON_APPS_SUBNET` | Docker. The pool of app networks, as one CIDR. It must match the executor's `CANYON_APPS_POOL`. The server closes every connection from inside it, except from the first address of each `/24`, which is that network's gateway, and every IPv6 connection but loopback. Unset, it refuses nothing, which is right on Kubernetes. |
| `CANYON_EGRESS_RECEIVER_ADDR` | Optional. The `ip:port` the server takes Canyon's Envoy's access logs on, over gRPC: what each app calls out to. Unset, there is no receiver. It must be the server's own IPv4 address on the `canyon-egress-ctl` network, as `10.214.3.2:7781`: the server refuses to start on `0.0.0.0`, loopback, a name, IPv6, an address inside `CANYON_APPS_SUBNET`, or with `CANYON_APPS_SUBNET` unset. It closes every connection from that pool, each `/24`'s gateway included. On Kubernetes it is `0.0.0.0:7781`, as `deploy/k8s/server.yaml` sets it, and `0.0.0.0` alone: no manifest knows the pod's address, `CANYON_APPS_SUBNET` is not needed, and the server's NetworkPolicy admits that port from the egress gateway's pods alone; any other address, a name or IPv6 stops the server. The server listens on IPv4 alone. |
| `CANYON_EGRESS_RECEIVER_TOKEN` | Required with `CANYON_EGRESS_RECEIVER_ADDR`. At least 32 random characters, as `openssl rand -hex 32` writes; give Envoy and the egress check the same. Envoy sends it as `authorization: Bearer <token>` on every stream, and a stream without it is refused. The bundled compose file sets the receiver on, with a local default token that only Envoy, the check and the server share: on any host but a laptop's demo, set your own (`export CANYON_EGRESS_RECEIVER_TOKEN=$(openssl rand -hex 32)` before `docker compose up`), and it reaches all three. Nothing else has a default: the server refuses to start without one. |
| `CANYON_EGRESS_POLICY_ADDR` | Optional. The `ip:port` the server serves the policy Canyon's egress check pulls on: each app env's mode and allowlist, and which app holds each address — never a credential. Unset on Docker with `CANYON_EGRESS_RECEIVER_ADDR` set, it is the receiver's address on port 7782, as the bundled compose file relies on (a receiver on 7782 itself then stops the server: set this). Unset otherwise, none is served, and a deploy waits on no egress check. The same address rules as `CANYON_EGRESS_RECEIVER_ADDR`, another port, as `10.214.3.2:7782`; it closes every connection from the apps' pool. On Kubernetes it is `0.0.0.0:7782`, as `deploy/k8s/server.yaml` sets it: there the server serves it over TLS under a certificate it mints at its first start, which the install step publishes (`canyon-server egress-policy-cert`), and takes only a token of the egress gateway's ServiceAccount, checked against the cluster's own key set, which it reads with its own mounted token and CA (`/var/run/secrets/kubernetes.io/serviceaccount`): without them it does not start. |
| `CANYON_EGRESS_PROXY` | Optional. Canyon's Envoy as apps reach it, `http://<host>:3128`; any other form stops the server. The bundled compose file sets `http://canyon-egress:3128`. On Docker, an app deployed while Settings sends apps' calls through Canyon runs on an internal network with `HTTPS_PROXY` and `HTTP_PROXY` set to it, and reaches nothing outside its network but Envoy. Settings turns it on for a Docker host whose server has this set, and lists each app whose network was made before: an admin's Recreate networks restarts that app on an internal network, and turning it off lists and recreates them open again. On Kubernetes `deploy/k8s/server.yaml` sets `http://canyon-egress.canyon.svc:3128`, the gateway's Service: there an app has no network, so each environment's next deploy or promotion runs through the gateway while Settings says so, and nothing needs recreating. |
| `CANYON_KUBE_GATEWAY_SIGNIN` | Optional, Kubernetes. The issuer, an `https` URL, of a sign-in provider published only through your cluster's own gateway or ingress, such as the Helm chart's Pocket ID on k3s (the chart sets it there) or a Dex, Keycloak or Zitadel behind your own gateway (set it yourself). Apps routed through Canyon's egress gateway can't reach such a provider. So while the front door signs in there, Settings refuses to route apps through Canyon, and saving such a front door while they are routed shows a warning. Anything but an `https` URL stops the server. |

### Sign-in

| Variable | What it does |
|---|---|
| `CANYON_AUTH` | How people sign in to Canyon. `oidc` (the default when unset): through your identity provider, for any Canyon other people reach. `off`: no sign-in and everyone is an admin, for one person on one machine, so Canyon refuses to start unless both `CANYON_ADDR` and `CANYON_PUBLIC_URL`'s host are loopback. The laptop demo's `docker-compose.yml` sets a third value, `demo`: `off` for a server inside a container, which has to listen on the container's network for Docker to forward the laptop's `127.0.0.1` port to it ([Try it on your laptop](/start/try-it-on-your-laptop/#sign-in-is-off)). It refuses to start unless `CANYON_PUBLIC_URL`'s host is loopback. Don't set it yourself. With `off` and `demo`, Canyon answers only a request addressed to `localhost`, a name under it or a loopback address, and any other with `421`, so a web page can't reach it by pointing a name of its own at your machine. They also serve only connections from the machine itself: loopback, or, behind the demo's port mapping (`CANYON_PUBLISHED_ADDR` a loopback address), the Docker network's gateway the published port arrives from. Any other connection is closed before Canyon reads it, so another machine that reaches a demo published beyond `127.0.0.1` with its own address gets an empty reply. Some Docker setups (rootless Docker, Podman, Docker Desktop, a daemon with `iptables: false`, an IPv6 client) forward another machine's connection from the gateway, so with `CANYON_PUBLISHED_ADDR` set, as the demo sets it, the demo stops if Docker publishes its port on anything but loopback: keep it on `127.0.0.1`. Not closed: anything on your machine that hands other machines' connections to that port with `Host: localhost`, such as nginx or Caddy rewriting `Host`, `ngrok --host-header=rewrite`, cloudflared's `httpHostHeader`, `ssh -g -L`, or a service mesh's sidecar (Istio's, from `127.0.0.6`), makes everyone who reaches it an admin. An app the demo runs never reaches it that way: on Docker Desktop, OrbStack and Colima, `host.docker.internal` leads to the Mac's own `127.0.0.1:7777`, and every app's floor refuses it with every other address of the host's (*What an app reaches*, below), unless `CANYON_APPS_ALLOW` opens a range that holds one. |
| `CANYON_OIDC_ISSUER` | With `oidc`, required. Your identity provider's issuer, an `https` URL. |
| `CANYON_OIDC_CLIENT_ID` | With `oidc`, required. Canyon's client at your provider, with the redirect URI `<CANYON_PUBLIC_URL>/auth/callback`. |
| `CANYON_OIDC_CLIENT_SECRET` | Optional. The client's secret, if it has one. Without it, Canyon signs in as a public client with PKCE. |
| `CANYON_ADMINS` | With `oidc`, required. Comma-separated email addresses of the first admins, the only people who can sign in until an admin says who else may. These are always admins, beside anyone made an admin in the dashboard. |
| `CANYON_OIDC_ALLOWED_DOMAINS` | Optional. Comma-separated domains: anyone with an email address at one may sign in. Set here, the list is fixed and the dashboard shows it read-only. Unset, an admin sets it in the dashboard. |
| `CANYON_OIDC_ALLOWED_EMAILS` | Optional. Comma-separated addresses that may sign in, fixed the same way. With both lists set, every `CANYON_ADMINS` address must be on one of them, or Canyon refuses to start. |
| `CANYON_ADMIN_CLAIM` | Optional, set together with `CANYON_ADMIN_VALUE` or not at all. The ID token claim that makes someone an admin. |
| `CANYON_ADMIN_VALUE` | The value of `CANYON_ADMIN_CLAIM` that means admin: the claim equals it, is a list that holds it, or is an object with it as a key. |
| `CANYON_RESPONDER_CLAIM` | Optional, set together with `CANYON_RESPONDER_VALUE` or not at all. The ID token claim that makes someone a security responder, who can stop any app's production and never ships a version. |
| `CANYON_RESPONDER_VALUE` | The value of `CANYON_RESPONDER_CLAIM` that means responder, matched as the admin claim is. |
| `CANYON_GROUPS_CLAIM` | Optional. The ID token claim that carries a person's groups, for sharing an app with a group: 1 to 256 characters of plain text. Set here, it overrides the dashboard's setting. |

Canyon only reads the sign-in variables when `CANYON_AUTH` is `oidc`.

### Storage and keys

| Variable | What it does |
|---|---|
| `CANYON_DATABASE_URL` | Required. Canyon's own Postgres. The server will not start without it, and never falls back to `PG*` variables. Its `sslmode`: `verify-full`, with `sslrootcert` naming the server's CA file, or `require` at the least. With no `sslmode`, or `prefer`, Canyon connects over TLS or not at all, never falling back to plaintext; `allow` refuses to start; `disable` connects unencrypted, and the server says so as it starts. |
| `CANYON_DATABASE_MAX_CONNS` | Optional. The most connections the server opens to its Postgres: a whole number from 4 to 500, default 20. It opens one more for its readiness check. |
| `CANYON_KEY_FILE` | The key every stored secret is encrypted under. Default `/var/lib/canyon/keys/kek`. Canyon creates it on first start. Keep it on a persistent volume and back it up: if Canyon has to create a new key while the database holds secrets encrypted under another, it refuses to start. `canyon-server rotate-kek` rotates it. |
| `CANYON_REPOS` | The directory where Canyon keeps each app's uploaded code. Default `/var/lib/canyon/repos`. Keep it on a persistent volume. |

### Builds and scans

| Variable | What it does |
|---|---|
| `CANYON_SCAN_CONCURRENCY` | Optional. How many scans run at once across the whole host: a whole number from 2 to 100. One of them is always kept for Grype. Unset, 2. The Kubernetes manifests set 10. Past it, a scan waits its turn and says so; it never fails for waiting. |
| `CANYON_BUILD_CONCURRENCY` | Optional. How many builds run at once: a whole number from 1 to 100. Unset, 1. The Kubernetes manifests set 4. |
| `CANYON_PATCHING` | Optional, **beta**: `on` or `off`. Unset, `off`. Whether Canyon patches its own platform under your apps. `on` takes effect only with sign-in on (`CANYON_AUTH` `oidc`); with sign-in off, no patch runs. Turning it on lets Canyon rebase an app's preview onto a newer run image and redeploy it on its own, then watch it for `CANYON_PATCH_WATCH`: if the patched app exits or restarts, Canyon puts back the image it replaced. Canyon patches one app's preview for each new run image first, and more previews in waves once a watch passes. It never patches production on its own: once a patched preview's watch passes, Canyon asks for production with a production approval, which a contributor or above on the app approves in the dashboard, and asks again each rescan while the patch still applies. Once approved, production is watched for `CANYON_PATCH_WATCH` too, and a crash there puts back the image production ran. With patching on, an admin can also turn on **Rebuilds** (Settings › App hosting › Patching): Canyon rebuilds a preview from the same code on newer build tools when they fix a vulnerability, and always asks before production. Off, Canyon still checks the packages it put in the images of every app where the rule *No known-vulnerable system libraries or build tools* is on against the newest vulnerability database, so the rule reads as it does with patching on, but no patch runs. Patching acts only on those apps, and the rule is off by default. The server refuses to start on any other value. |
| `CANYON_PATCH_CONCURRENCY` | Optional, **beta**, read only while `CANYON_PATCHING` is `on`. How many of Canyon's platform patches run at once across the whole host: a whole number from 1 to 100. Unset, 1. The Kubernetes manifests set 2. A patch always waits for a person's run of the same app, and never fails for waiting. A patch frees its slot once the patched app runs: its watch (`CANYON_PATCH_WATCH`) takes none. |
| `CANYON_PATCH_INTERVAL` | Optional, **beta**. How often Canyon checks the packages it put in every running image against the newest vulnerability database, while the rule *No known-vulnerable system libraries or build tools* is on. A duration that divides a minute, an hour or a day evenly, such as `6h` or `30m`. Unset, `6h`. Every server replica ticks on the same instants, and each tick runs once. Canyon keeps the schedule in its database, so with replicas set differently, the one that started last decides. The server refuses to start on any other value. |
| `CANYON_PATCH_WATCH` | Optional, **beta**, read only while `CANYON_PATCHING` is `on`. How long Canyon watches a preview it patched, and production once a person approves the patch: if the patched app exits or restarts in that time, Canyon puts back the image it replaced. A duration from `1m` to `24h`, such as `15m`. Unset, `15m`. A watch that can't tell at its end whether the app runs doesn't pass, and that patch goes no further. A watch takes no `CANYON_PATCH_CONCURRENCY` slot. The server refuses to start on any other value. |
| `CANYON_IMAGE_IDENTITY` | Kubernetes, for hand-off. The `canyon-identity` sidecar image, by digest. When [your platform runs the apps](/self-host/hand-off/), each release names this image for your pipeline to run beside the app. Set it to the same value as the executor's. Canyon refuses hand-off without it, and the server refuses to start when it isn't an image by digest. |

### Databases for apps

| Variable | What it does |
|---|---|
| `CANYON_APP_DATABASE_URL` | Optional. An admin login on the Postgres server where apps get their databases. Set, Canyon's built-in driver gives each app that declares Postgres its own database and login, in both environments. Unset, a deploy that declares Postgres fails and names this setting. See [Databases](/build/databases/). A login that is not a superuser, such as RDS's master user, also needs a cap on the temporary files a person's own login to an app's database may write: have a superuser run `GRANT SET ON PARAMETER temp_file_limit TO <this login>` (Postgres 15 or later), or set `temp_file_limit` to `1GB` or less for the whole server (on RDS, in the instance's parameter group, as RDS has no superuser to grant it). Without either, Canyon refuses those logins and names both. Name `sslmode=verify-full` and `sslrootcert=<the server's CA file>` in this URL, or `sslmode=require` at the least. With no `sslmode`, or `prefer`, Canyon's own connections and each person's database session use TLS without checking the server's certificate, and never fall back to plaintext; `sslmode=allow` refuses to start, since it could send a password in plaintext; `disable` connects unencrypted, and the server says so as it starts. |
| `CANYON_APP_DATABASE_CA_FILE` | Optional, Kubernetes only, and only with `CANYON_APP_DATABASE_URL`. A file of PEM certificates, at most 256 KiB, for the apps' database server. Each app's `DATABASE_URL` then verifies that server against it. A PEM block in it that isn't a certificate stops the server. |
| `CANYON_DB_MAX_CONNECTIONS` | Optional. The most connections to apps' databases this server opens for people at once, through `canyon db connect` and `canyon db query`: a whole number from 1 to 10000, 100 when unset. Anything else stops the server. Keep it below the apps' database server's `max_connections`, less what the apps' own pools take. A person has at most 10 at once. Database sessions themselves are off until an admin turns them on in Settings › Databases. |
| `CANYON_PROVISIONERS` | Optional. A JSON file that says which driver makes each resource type in each environment: resource type, then `preview` or `prod`, then `{"builtin": true}` (Postgres only, and it needs `CANYON_APP_DATABASE_URL`) or `{"url": ..., "token_file": ..., "ca_file": ...}` for your own driver. An unknown type, environment or field stops the server. A type and environment the file doesn't name has no driver. Unset, the built-in driver serves Postgres in both environments when `CANYON_APP_DATABASE_URL` is set. The driver contract is in [Contracts](/reference/contracts/). |

### The AI review

Leave all five of `CANYON_MODEL_PROVIDER`, `CANYON_MODEL`, `CANYON_MODEL_URL`, `ANTHROPIC_API_KEY`
and `CANYON_MODEL_TOKEN` unset to run with no model: every AI review then shows as skipped, and the
scanners still run. Setting any of them asks for a model, and a combination Canyon can't serve stops
it at start.

| Variable | What it does |
|---|---|
| `CANYON_MODEL_PROVIDER` | `anthropic` (the default) or `openai`. On `openai`, Canyon runs the AI review but not the setup chat. `bedrock` and `vertex` stop the server: put a gateway in front of them and use `CANYON_MODEL_URL`. |
| `CANYON_MODEL` | The model id, exactly as the provider names it. Default `claude-opus-5` on `anthropic`. Required on `openai`. |
| `CANYON_MODEL_URL` | Optional. A gateway, proxy or internal endpoint that speaks the provider's API: `http` or `https`, a host, no credential in it, at most 2048 bytes. Unset, the provider's public API. |
| `ANTHROPIC_API_KEY` | The Anthropic key, also sent to a gateway that wants one. Set this or `CANYON_MODEL_TOKEN`, not both; a gateway at `CANYON_MODEL_URL` that needs no credential takes neither. |
| `CANYON_MODEL_TOKEN` | A bearer token sent instead of a key, for a gateway at `CANYON_MODEL_URL` that holds the provider key itself, so this host holds none. |
| `OPENAI_API_KEY` | Required on `openai`, and read only there. |

Canyon ignores the SDKs' own variables, such as `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL`: the
model is only ever reached at the address these settings name.

### Other

| Variable | What it does |
|---|---|
| `CANYON_ORG` | Optional. Your organization's name, as the dashboard and every receipt show it: 1 to 128 characters of plain text. Set here, the dashboard shows it read-only. Unset, an admin sets it in the dashboard. |
| `CANYON_TELEMETRY` | Optional. `0`, or anything but `1`, `true`, `on` or `yes`, turns off the daily report of counts Canyon sends, as do `DO_NOT_TRACK` set to anything but `0` or `false`, and any `CI` value; an admin can't turn it back on while one is set. On by default: `canyon telemetry show` prints what is sent. What it carries, and never: [TELEMETRY.md](https://github.com/getcanyon-ai/canyon/blob/main/TELEMETRY.md). |
| `CANYON_LICENSE` | Optional. Your Canyon Enterprise licence: the licence file's text, or the path of a file holding it. Set here, it wins over one an admin pastes in **Settings › Licence**, and the dashboard shows it read-only. Canyon checks it on the server, with no network call. A missing or expired licence never stops an app or loosens a review: it only locks changes to Enterprise features. See [Canyon Enterprise](/org/licence/). |
| `CANYON_INSTALL_MODE` | Optional. How Canyon was installed, which the daily report says: `laptop`, `one_server`, `helm_one_node` or `kubernetes`. The demo sets `laptop`, a team's Docker server `one_server` and the Helm chart `helm_one_node`; on Kubernetes, set `kubernetes` yourself. Unset, the report leaves it out. The server refuses to start on any other value. |
| `CANYON_GITHUB_API` | Optional. The GitHub API address Canyon's GitHub App talks to. Default `https://api.github.com`. |
| `CANYON_DEMO_SEED` | Demo only. `1` fills an empty database with a sample fleet. Only the bundled demo sets it, and it does nothing on a database that already has apps. |

## Docker executor

`canyon-executor`, for a single Docker host.

| Variable | What it does |
|---|---|
| `CANYON_EXECUTOR_ADDR` | The address it listens on. Default `:7778`. |
| `CANYON_APPS_POOL` | The CIDR app networks are cut from, one `/24` per app network. Default `172.28.0.0/16`. Must match the server's `CANYON_APPS_SUBNET`. |
| `CANYON_EXECUTOR_JOIN` | Comma-separated containers joined to every app network. Default `canyon-server,app-postgres`: the server, so it can reach apps, and the apps' database. |
| `CANYON_EXECUTOR_EGRESS_JOIN` | Comma-separated containers joined to every app network whose calls out go through Canyon's Envoy, and to no other. Default `canyon-egress`, the container Envoy runs beside, which such an app reaches it by. A deploy of such an app fails while the app can't reach it; a down `canyon-egress` fails no other deploy. These networks need Docker 28.0 or later. |
| `CANYON_EXECUTOR_BUILDS` | Optional. Builds it takes at once, platform-patch rebases included: 1 to 100, default 2. Keep it at or above the server's `CANYON_BUILD_CONCURRENCY` plus `CANYON_PATCH_CONCURRENCY`. |
| `CANYON_EXECUTOR_SCANS` | Optional. Scans it takes at once: 1 to 100, default 5. Keep it at or above the server's `CANYON_SCAN_CONCURRENCY` plus `CANYON_PATCH_CONCURRENCY` plus two: one for the image scan after a deploy, one for the rescan. |
| `CANYON_BUILDER` | Optional. The buildpack builder image. Unset, Canyon builds its own builder image on first use. A builder on a different stack from Canyon's run image fails the build and names both stacks. |
| `CANYON_IMAGE_LIFECYCLE` | Optional. The buildpack lifecycle image, by digest. |
| `CANYON_IMAGE_RUN` | Optional. The run image every app builds onto and a platform patch rebases onto, by digest: your own image on the builder's stack. Unset, Canyon builds its own, `canyon-run`. A run image on a different stack from the builder's fails the build and names both. Move it only to a new digest of the same repository: setting it, unsetting it, or naming another repository leaves every app built before unpatchable until that app is deployed again. |
| `CANYON_RUN_APT_MIRROR` | Optional. The Ubuntu archive Canyon's run image takes its packages from, in place of Ubuntu's own archive and security pocket, for a host that can't reach Ubuntu's archive. An `http` or `https` URL — prefer `https` — with no credential, query or fragment, and none of `'`, `"`, `\`, `$`, a backtick or whitespace. It must serve `noble`, `noble-updates` and `noble-security` with `main`, `universe` and `multiverse`, signed by Ubuntu's archive key, and on arm64 mirror `ubuntu-ports`. The URL is built into the run image, so every app can read it: never put a secret in it. |
| `CANYON_IMAGE_BUSYBOX` | Optional. The image that checks an app answers on its port, by digest. |
| `CANYON_IMAGE_EGRESS_HOLDER` | Optional. The image of each build's holder, and of each app's, which keep the build and the app off private and link-local addresses (below), by digest, with or without a tag beside it (`<registry>/<repository>[:<tag>]@sha256:<hex>`): the same setting, and the same image, as the egress gateway's holder. |
| `CANYON_BUILD_ALLOW` | Optional. Addresses and ports builds may reach besides public 80 and 443, such as an internal package registry or mirror: comma-separated entries, each `<IPv4 prefix>:<TCP port>`, like `10.20.0.0/16:443,10.30.4.12/32:8081`. An entry that isn't of that shape, is wider than a `/8`, or overlaps `169.254.0.0/16`, `168.63.129.16`, `100.100.100.200`, `0.0.0.0/8`, `127.0.0.0/8`, `224.0.0.0/4`, `240.0.0.0/4`, `CANYON_APPS_POOL` or `CANYON_EGRESS_POOLS` stops the executor from starting, naming it. Never name a forward proxy here unless it refuses private and link-local addresses itself: builds reach everything it reaches, the cloud's metadata address included. On the [one-server install](/self-host/docker/), set it in `.env`, which the bundled compose file hands the executor. |
| `CANYON_APPS_ALLOW` | Optional. Private addresses and ports running apps may reach besides their own network, such as a company database or API on your internal network: comma-separated entries, each `<IPv4 prefix>:<TCP port>`, like `10.40.0.0/16:5432`. The same shape and the same refusals as `CANYON_BUILD_ALLOW`, so no entry can open link-local, the metadata addresses, loopback, Canyon's own networks or an app network's gateway, and none opens an address of the machine Canyon runs on. It covers apps whose outbound calls don't go through Canyon (Settings › Outbound off); an app routed through Canyon's egress gateway reaches what the gateway allows. Never name a range that holds the address of the machine Canyon runs on: under the laptop demo, a range like Colima's `192.168.5.0/24` or Docker Desktop's `192.168.65.0/24` hands every app the dashboard as an admin, through the host's own forward to it. On the [one-server install](/self-host/docker/), set it in `.env`, which the bundled compose file hands the executor. |
| `CANYON_EGRESS_POOLS` | Optional. Canyon's own networks, comma-separated IPv4 CIDRs, which every build's floor, and every app's, refuses before `CANYON_BUILD_ALLOW` or `CANYON_APPS_ALLOW`, as the egress gateway's does (its row below). Default `10.213.0.0/16,10.214.0.0/16`, the bundled compose file's, which hands both the same value. |
| `CANYON_IMAGE_TRIVY` · `CANYON_IMAGE_GRYPE` · `CANYON_IMAGE_CHECKOV` | Optional. Where each scanner's image comes from, by digest. |
| `CANYON_SCANDB_TRIVY` | Optional. The OCI repository and tag Trivy's vulnerability database downloads from, `<registry>/<repository>:<tag>`. Unset, Trivy's own public source. |
| `CANYON_SCANDB_GRYPE` | Optional. The `https` URL Grype's vulnerability database downloads from, with no credential in it. Unset, Grype's own default. |
| `CANYON_SCANDB_TRIVY_JAVA` | Optional. The OCI repository and tag Trivy's Java database downloads from, `<registry>/<repository>:<tag>`; an image scan reads it to name each jar. Unset, Trivy's own public source. |
| `CANYON_SCAN_IMAGE_MAX` | Optional. The largest image an image scan reads: a whole number of bytes, or of `Ki`, `Mi`, `Gi` or `Ti`. Default `8Gi`; a larger image reads as not checked. |

Every image setting takes an image by digest with its registry named,
`<registry>/<repository>@sha256:<hex>`. Unset, the image pinned in this Canyon release. On Docker,
the executor builds the Opengrep and ESLint scanner images itself.

**What a build reaches.** Each build runs every step, the app's own install and build scripts included, in
a network namespace of its own on the Docker network `canyon-builds`. From there it reaches DNS, through
Docker's own resolver, and TCP 80 and 443 at public addresses, where package registries are, plus what
`CANYON_BUILD_ALLOW` names. It never reaches the cloud's metadata address, Azure's WireServer, any
address of the host's own (a public one included), Canyon's networks or any other private address that `CANYON_BUILD_ALLOW` doesn't name. That holds under every Outbound
mode: Outbound covers running apps, not builds. A build that fetches from a registry, mirror or proxy at a
private address, or on another port (git over SSH), fails at that fetch until `CANYON_BUILD_ALLOW` names
it. If the network or its floor can't be set up, the build fails with `build network: …` and never runs
without it. A build value named `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`,
`SSL_CERT_DIR`, `GODEBUG`, `REGISTRY_AUTH_FILE`, `XDG_RUNTIME_DIR`, or anything starting `PACK_` or
`DOCKER_`, in any case, is refused: the build tool, which runs outside the build's network, would read
it too. A runtime value may name the proxies and `PACK_`, never a `DOCKER_` name: an app with such a
runtime secret doesn't start, so its deploy, promotion, rollback or platform patch fails naming it. A `project.toml` may name only the
builder's own buildpacks, by id, and inline scripts: Canyon refuses one that names a buildpack by
`uri`, a registry name, an image or a path, which the build tool would fetch outside the build's
network, and one with a key of the tables the build tool reads that Canyon doesn't read, or that
spells one of their names in another case. Other tools' tables are left alone. A `CANYON_BUILDER` whose image runs as root, by name or
uid, is refused.

**What an app reaches.** On Docker, an app whose outbound calls don't go through Canyon runs in a network
namespace kept by a small container of its own, `canyon-appfloor-<id>`, on the app's network. From there it
reaches its own network (Canyon's server and its database), DNS, every public address on any port, and what
`CANYON_APPS_ALLOW` names. It never reaches the machine Canyon runs on: not any of its addresses, a public
one included, as Docker sees them when the app is deployed, not its network's gateway, not
`host.docker.internal` (which Docker Desktop, OrbStack, Colima and Podman point at the host or the Mac), not
the cloud's metadata address, Azure's WireServer, Canyon's other networks, or any private, shared or
link-local address that `CANYON_APPS_ALLOW` doesn't name. That holds whatever the sign-in mode. An app that
calls a service on your internal network fails at that call until `CANYON_APPS_ALLOW` names it; one on the
host itself stays out of reach, as no entry opens the host. An app deployed before the release that brought
the floor runs without it until its next deploy: redeploy every app once after upgrading
([Back up, restore and upgrade](/self-host/backup-and-upgrade/#the-upgrade-that-keeps-apps-off-the-host-docker)).
If the floor can't be set up, the deploy fails with `app network: …` and the
app never starts without it. After a reboot or a restart of Docker, the executor starts each app again
behind its floor within 15 seconds of its own start; an app that had stopped by itself stays stopped. A
`dns` setting in the daemon's `daemon.json` at a private address stops apps resolving names, as it does
builds (below).

**Builds' DNS.** A build resolves names through Docker's own resolver, which Docker 28 and later
forward to the host's resolvers from the host itself, so a resolver at a private address, such as a
cloud network's, still answers. Docker before 28 isn't supported. A `dns` setting in the daemon's
`daemon.json` is the exception: Docker forwards to those servers from inside the build's network,
where a private address is refused, and builds then resolve nothing (`npm ERR! getaddrinfo EAI_AGAIN`,
for one). Remove the daemon's `dns` setting, so the host's own resolvers answer, or name public
resolvers in it.

On a Docker host, never prune the tagged `canyon/*` images (`docker image prune -a` removes them):
Canyon keeps some of them for a rollback, a hold or a patch revert, and a rollback to one that is gone
rebuilds it instead of running the image production ran.

## Egress gateway

```mermaid title="Canyon's egress gateway on Docker: three containers in one network namespace, and the server's two ports"
flowchart TD
  app["An app whose calls go through Canyon<br>HTTPS_PROXY and HTTP_PROXY set"]
  subgraph ns["canyon-egress: the network namespace"]
    envoy["canyon-egress-envoy<br>Envoy, port 3128"]:::canyon
    check["canyon-egress-control<br>the egress check, 127.0.0.1:7783"]:::canyon
  end
  server["canyon-server<br>receiver :7781 · policy :7782"]:::canyon
  net["Public addresses<br>TCP 80 and 443 only"]
  app --> envoy
  envoy -->|"may this call go?"| check
  check -->|"pulls the policy every 5 s"| server
  envoy -->|"a record of each call"| server
  envoy --> net
```

`canyon-egress`, the container that holds the network namespace Canyon's egress gateway runs in.
It refuses every new connection out of that namespace to a private, shared, link-local, loopback or
reserved address, to Canyon's own networks and to any address of the machine Canyon runs on, a public one
included, and every public one but TCP 80 and 443. It lets through
the server's receiver and its policy, each on its one port, and the egress check on its one loopback port. It needs a
kernel with IPv6 present (not booted with `ipv6.disable=1`), which it turns off in its namespace.

`canyon-egress-host`, which lists the machine's own addresses for it: a container in the host's network
namespace, with no capability and a read-only root, that replaces the list with `ip addr show` once as it starts,
marked with the machine's boot, then sleeps. `canyon-egress` reads that list as it starts and doesn't start without
one written since the machine last booted; an address the machine takes later
is refused once `canyon-egress` starts again. On Docker Desktop and Colima the machine is the Docker VM.

`canyon-egress-envoy`, the gateway: Envoy in that namespace, listening on port 3128, with no
capabilities and not as root. It forwards to port 443 by `CONNECT` or an `https://` request and
to port 80 by an `http://` request, and to no other port. Before each call it asks the egress check,
`canyon-egress-control`, beside it on loopback, which decides from Canyon's policy: a call it refuses is
answered `403` with a line saying why, and while it doesn't answer within 5 seconds, every call is `503`
*Canyon's egress check did not answer, so this call was not made.* Nothing is looked up or sent first. A tunnel or a request may take up to 15 minutes; one that moves no byte for 5 minutes is closed, and a client has 10 seconds to send its request's headers. A token, resolver or receiver address it can't use stops it from starting, naming the variable. It sends what it forwards to the server's
receiver and to nothing else: no access log file, and its admin interface is a Unix socket,
`/var/run/envoy-admin/admin.sock`, on a volume it shares with the check alone, which reads from it how many calls
never reached the server's records. It takes `CANYON_EGRESS_RECEIVER_ADDR`,
`CANYON_EGRESS_RECEIVER_TOKEN` and `CANYON_EGRESS_RESOLVERS`, the same values as the server and the holder, and
optionally `CANYON_EGRESS_CONTROL_ADDR`, the check's, `127.0.0.1:7783` unset. On Kubernetes the same Envoy runs as `canyon-egress`
(`deploy/k8s/egress.yaml`), its resolvers the cluster's DNS and its receiver the server's Service, both filled
in when it starts, and its token from the Secret `canyon-egress` (*Install on Kubernetes*); the check runs in
each of its pods, which are ready once the check holds a policy.

### Outbound modes (beta)

Each app environment's calls through the gateway are checked under one of two modes. **`observe`**, the
default, lets a call to a host name on port 80 or 443 through and records it. **`allowlist`** lets a call through only to a destination
on that environment's allowlist, or to a platform destination, and refuses every other. Both refuse IP addresses,
single-label names and other ports, and hold an app environment to a limit of new destinations a minute, 30 unless an
admin changes it, as the table below says. An admin sets the
org's mode (`PUT /api/v1/egress {"mode": "allowlist"}`), and an app's owner can set one environment stricter
(`PUT /api/v1/projects/<app>/egress/<env>/mode`).

An admin can also change three limits in Settings › Outbound, or with `PUT /api/v1/egress {"caps": {...}}`:

| Limit | Default | Range | What it holds |
|---|---|---|---|
| `new_names` | 30 | 1 to 600 | New destinations an app environment may call a minute. Canyon's egress checks share it: each lets through this many divided by `CANYON_EGRESS_REPLICAS`, rounded down, so a limit below the number of replicas lets no new destination through. |
| `destinations` | 500 | 50 to 5,000 | Destinations an app environment's records keep. Calls to a new one past it are still allowed or refused as usual, and counted without their names. Lowering it deletes nothing. |
| `pending` | 50 | 1 to 500 | Requests to allow a destination an app environment may have waiting. Past it a refusal files no new one. |

A value out of its range is refused, and nothing changes.

Outbound is beta. A destination can't be allowed from the CLI: the API answers
`403 dashboard_required` to a CLI session on a Canyon with sign-in on. It is allowed in the dashboard, with
**Allow** on the app's *Outbound* card or in Settings › Outbound ([Outbound](/org/outbound/)).

**Platform destinations** are the hosts Canyon's own sign-in sidecars call, allowed for every app and on no
allowlist. On Kubernetes they come from each front door in use: the one saved, and the one each running app was
deployed with, so changing the front door breaks no app before its next deploy. That is a standard OIDC gateway's `issuer` and
`jwks_url` hosts, or an ALB's key endpoint for its region (`public-keys.auth.elb.<region>.amazonaws.com`); and,
where Canyon signs people in, its `signin.issuer` host and the hosts of the `token_endpoint`, `userinfo_endpoint` and `jwks_uri` its
discovery document names (Google's are `oauth2.googleapis.com`, `openidconnect.googleapis.com` and
`www.googleapis.com`). An app deployed before Canyon recorded which front door it runs behind adds none of its
own: deploy it again.

Where Canyon signs people in, Canyon's server reads the provider's discovery document itself, when the front door is saved, so the
server needs to reach the provider's issuer on port 443: the server's NetworkPolicy (`deploy/k8s/server.yaml`)
admits public addresses on 443, and a provider at a private address needs a rule of its own there. A save never fails on it: it waits at most 10 seconds for the document, and if it can't be read, the front door is still saved, Settings › App
hosting says *Canyon couldn't read your sign-in provider's discovery document yet and keeps trying every minute;
until it does, apps whose outbound mode is allowlist can't sign people in.*, and the server tries again every
minute until it can. A provider whose issuer is an IP address or a single label, such as an in-cluster
`keycloak`, is never read: it is reached directly (`CANYON_KUBE_SIGNIN_DIRECT`), and none of its hosts is a
platform destination.

A provider's discovery hosts are allowed whole, for every app. Some are large shared API hosts — Google's
`www.googleapis.com`, Microsoft's `graph.microsoft.com` — that an app could also call for its own purposes:
under `allowlist` it can reach them without being allowed to.

A refused call is answered `403` with the header `Canyon-Egress: not_allowed` and one line of
`text/plain`: *Canyon refused this call: `<destination>` is not on `<app>` `<env>`'s outbound allowlist. A
person on the app's team can allow it in Canyon: `<url>`*. The link opens the app's *Outbound* card, where
the app's contributors, its owners and admins can click **Allow** beside the refused destination. Nothing is looked up or sent. The
first refusal of a destination files an egress request, which `GET /api/v1/projects/<app>/egress` lists under the
environment's `requests`. [Outbound](/org/outbound/) says how to set the mode in the dashboard.

With apps' calls sent through Canyon, a Node app's process logs `[UNDICI-EHPA] Warning:
EnvHttpProxyAgent is experimental` once, the first time it fetches: Node's own notice that it reads
the proxy from its environment (`NODE_USE_ENV_PROXY=1`, which Canyon sets). It changes nothing. Don't
set `NODE_NO_WARNINGS` to hide it: that hides every other warning Node prints too.

With apps' calls not sent through Canyon (Settings › Outbound, *Off*), an app on Docker calls out
directly, with nothing between it and the network: it reaches the cloud's metadata address
(`169.254.169.254`) and the host itself. Sending its calls through Canyon closes both. Builds are not
apps: whatever the mode, they reach public TCP 80 and 443 alone ([What a build reaches](#docker-executor)).

| Variable | What it does |
|---|---|
| `CANYON_EGRESS_RESOLVERS` | The DNS resolvers the gateway resolves through, comma-separated IPv4 addresses, each admitted on port 53 alone. Use this machine's own upstream resolvers. Default `8.8.8.8,8.8.4.4`, Google's public resolvers: set your own if your network blocks them or you'd rather not send them your apps' lookups. Docker's resolver (`127.0.0.11`) and any address on Canyon's networks are refused even when listed. |
| `CANYON_EGRESS_POOLS` | Canyon's networks, comma-separated CIDRs, refused whole but the server's receiver and policy: the apps' pool (`CANYON_APPS_SUBNET` and `CANYON_APPS_POOL`) and Canyon's own networks. Default `10.213.0.0/16,10.214.0.0/16`, the bundled compose file's. Change it with them. |
| `CANYON_EGRESS_RECEIVER_ADDR` | The server's receiver, `ip:port`, admitted on that port alone. The same value as the server's. |
| `CANYON_EGRESS_POLICY_ADDR` | Optional. The server's policy, `ip:port`, which the egress check pulls, admitted on that port alone: the same value as the server's. Default: the receiver's address, port 7782, the server's own default. |
| `CANYON_EGRESS_CONTROL_ADDR` | Optional. The egress check's loopback `ip:port`, which Envoy asks, admitted on that port alone. Default `127.0.0.1:7783`, as Envoy's and the check's. |
| `CANYON_EGRESS_PER_SOURCE` | The connections one address, an app's container, may hold open to the gateway's port 3128 at once; the next is refused with a reset, and another app still connects. Default `512`: half of the 1,024 files an app's process may hold, and a 32nd of the 16,384 connections the gateway holds in all, so no one app holds the rest out. A positive integer; anything else stops it from starting, naming the variable. It needs the host kernel's `xt_connlimit`: without it, the holder does not start. Docker alone: on Kubernetes there is no per-source cap. |
| `CANYON_IMAGE_EGRESS_HOLDER` | Optional. Its image, by digest; it needs `sh`, `sleep`, `iptables-nft` with its `connlimit` match, and `ip6tables-nft`. Unset, Kubernetes' `distroless-iptables`, pinned. |
| `CANYON_EGRESS_CONCURRENCY` | Optional. The gateway's workers, each with one stream of records to the server's receiver, which takes 16 at once. Default `8`; `deploy/k8s/egress.yaml` sets `4`, as two replicas run there. A positive integer up to 16; anything else stops it from starting, naming the variable. |
| `CANYON_IMAGE_EGRESS` | Optional. Envoy's image, by digest; it needs `sh` and `sed` beside `envoy`, and on Kubernetes `getent` and `paste`. Unset, the stock `envoyproxy/envoy`, pinned. |

`canyon-server egress-control`, the egress check: the server's image beside the gateway, which Envoy asks
before each call out whether it may go. The bundled compose file runs it as `canyon-egress-control`, in the
holder's namespace; on Kubernetes it runs in each of the gateway's pods, as the ServiceAccount `canyon-egress`. It pulls the policy the server serves on `CANYON_EGRESS_POLICY_ADDR`
every 5 seconds and decides from the last one it took. It forgets which app holds which address once its
policy is 30 seconds old, so with the server down for longer than that it refuses every call out until the
server answers again. It keeps nothing across a restart: it is ready while it holds a policy the server answered
within the last 30 seconds, which `canyon-server egress-control probe` tells an exec probe (exit 0 when ready), so
a check cut off from the server leaves the gateway's Service. A call from an address its policy names no app for
makes it ask the server again, every 250 ms for up to 4 seconds, as a new app's address takes a moment to be
known; an address still no one's is refused at once for 15 seconds after. A setting out of shape stops it
from starting, naming the variable. It needs no database. The one secret it holds is the receiver's token, below.

| Variable | What it does |
|---|---|
| `CANYON_EGRESS_POLICY_URL` | The server's policy, `http://<ip>:7782/egress/policy` on Docker (the server's address on `canyon-egress-ctl`, the receiver's), `https://canyon-server.canyon.svc:7782/egress/policy` on Kubernetes. |
| `CANYON_EGRESS_POLICY_TOKEN_FILE` | Kubernetes alone, with an `https://` URL: the file holding its projected token, audience `canyon-egress-policy`, read again at every ask. |
| `CANYON_EGRESS_POLICY_CERT_FILE` | Kubernetes alone, with an `https://` URL: the certificate the server minted (the ConfigMap `canyon-egress-policy-cert`'s `tls.crt`), the only one it trusts. |
| `CANYON_EGRESS_RECEIVER_TOKEN` | The same token the server and Envoy hold. The check passes it to Envoy, so that each call it adds a stored credential to reaches the server's records. If the token is unset or out of shape, the check still runs, but every call it would add a credential to is refused with a 503, so none goes unrecorded. The token is never logged. The bundled compose file and `deploy/k8s/egress.yaml` set it from the server's. |
| `CANYON_EGRESS_REPLICAS` | The most egress checks that run at once, 1 to 30: each lets an app's environment call 30 divided by this many new destinations a minute, so all of them together never let more than 30 through. `1` on Docker; `3` on Kubernetes, two replicas and one more during a rollout. |
| `CANYON_EGRESS_CONTROL_ADDR` | Optional. The loopback `ip:port` Envoy asks it on. Default `127.0.0.1:7783`. Any other address stops it from starting. |
| `CANYON_EGRESS_CONTROL_METRICS_ADDR` | Optional. The `ip:port` it serves Prometheus metrics on, `GET /metrics`: `canyon_egress_policy_age_seconds`, the age of the policy it holds (absent before it holds one), `canyon_egress_refusals_total` by `reason`, and `canyon_egress_refetches_total` by `result` (`known`, `unknown`, `failed`). Never an app, a destination or an address. Unset, none is served, as in the bundled compose file: there the listener would sit on every app network the gateway joins. `deploy/k8s/egress.yaml` sets `0.0.0.0:7784`, which the gateway's NetworkPolicy admits from your scraper's namespace alone. Anything but an IPv4 address and a port stops it from starting, and so does `0.0.0.0` on Docker (a policy URL that is not `https://`). |

## Kubernetes executor

`canyon-executor-kube`, which runs in the cluster with no Docker socket. It takes the same image
format as the Docker executor.

| Variable | What it does |
|---|---|
| `CANYON_KUBE_REGISTRY` | Required. The registry host every image lives under. An ECR host, `<account>.dkr.ecr.<region>.amazonaws.com`, turns on ECR, and the executor then refuses to start without its own AWS credential. |
| `CANYON_IMAGE_IDENTITY` | Required. The `canyon-identity` sidecar image, by digest. |
| `CANYON_IMAGE_OPENGREP` | Canyon's Opengrep image, by digest. Unset, an Opengrep scan can't run, and a review with Opengrep on blocks on it. |
| `CANYON_IMAGE_ESLINT` | Canyon's ESLint image, by digest. Unset, an ESLint scan can't run, and a review with ESLint on blocks on it. |
| `CANYON_IMAGE_OAUTH2_PROXY` · `CANYON_IMAGE_BUSYBOX` · `CANYON_IMAGE_BUSYBOX_STATIC` · `CANYON_IMAGE_TRIVY` · `CANYON_IMAGE_GRYPE` · `CANYON_IMAGE_CHECKOV` | Optional. A copy of each image in your registry, by digest. Unset, the image pinned in this release. |
| `CANYON_SCANDB_TRIVY` · `CANYON_SCANDB_GRYPE` · `CANYON_SCANDB_TRIVY_JAVA` | Optional. As on Docker. Trivy's Java database is kept in the 5 Gi volume `canyon-scandb-trivy-java`, beside Grype's. |
| `CANYON_SCAN_IMAGE_MAX` | Optional. As on Docker, counted in the compressed bytes the image's manifest names. |
| `CANYON_KUBE_DB_CIDRS` | Comma-separated CIDRs where the apps' database listens on port 5433. Empty, no app reaches a database. |
| `CANYON_KUBE_SCANDB_CIDRS` | Optional. Comma-separated CIDRs of a scanner database source that isn't a public address on 443. None may overlap `169.254.0.0/16`, the node's instance metadata. |
| `CANYON_KUBE_SIGNIN_DIRECT` | Optional. The sign-in hosts that apps routed through Canyon's egress gateway call directly, comma-separated: `<host>=<address>/32` for an identity provider outside the cluster, `<host>` for one inside it. Only the front door's sign-in hosts it lists skip the gateway, so a public provider keeps working. An empty entry, a host with a wildcard, a leading dot or a port, a range, an address without `/32`, IPv6 or link-local stops the executor from starting. Those calls aren't recorded. The Helm chart sets it from `canyon.egress.signin.hosts`. See [A private sign-in provider for apps routed through Canyon](/self-host/kubernetes/#a-private-sign-in-provider-for-apps-routed-through-canyon). |
| `CANYON_KUBE_APP_CPU_REQUEST` | Optional. The CPU each app container requests, a Kubernetes quantity above 0 and at most the app's limit of 1. Default `10m`. |
| `CANYON_KUBE_APP_MEMORY_REQUEST` | Optional. The memory each app container requests, above 0 and at most the app's limit of `1Gi`. Default `256Mi`. |
| `CANYON_EXECUTOR_BUILDS` · `CANYON_EXECUTOR_SCANS` | Optional. As on Docker, with defaults of 6 builds and 14 scans. |
| `CANYON_EXECUTOR_ADDR` | The address it listens on. Default `:7778`. |

CIDRs are written without host bits, `10.0.4.0/24`, not `10.0.4.7/24`.

## What Canyon sets for each app

You don't set these: Canyon does, and an app's own secrets can't be named `PORT` or start with
`CANYON_IDENTITY_`. They are listed so you recognise them in a running container.

| Variable | Where | What it is |
|---|---|---|
| `PORT` | The app, on both runtimes | `3000`, the port the app must listen on. |
| `CANYON_IDENTITY_ISSUER` | The app, on both runtimes, and the sidecar on Kubernetes | Canyon's public URL, the identity token's `iss`. |
| `CANYON_IDENTITY_AUDIENCE` | The app, on both runtimes, and the sidecar on Kubernetes | The app's id, the identity token's `aud`. |
| `CANYON_IDENTITY_JWKS_URL` | The app, on both runtimes | Where to fetch the key set that signs identity tokens: the server's (`CANYON_APPS_JWKS_ADDR`) on Docker, the sidecar in the app's own pod on Kubernetes. See [Sign-in inside your app](/build/sign-in-in-your-app/). |
| `CANYON_TARGET` | The sidecar, Kubernetes only | `preview` or `prod`. |
| `CANYON_FRONT_DOOR` | The sidecar, Kubernetes only | How people reach the app and who signs them in, as JSON, without any client secret. |
| `CANYON_IDENTITY_EGRESS_PROXY` | The sidecar, Kubernetes only, when the app's calls go through Canyon | Canyon's egress gateway. The sidecar passes each of the app's outbound calls on to it, with a token that proves which app is calling. The app's proxy variables point at the sidecar, `http://127.0.0.1:4184`. |
| `CANYON_IDENTITY_EGRESS_TOKEN` | The sidecar, Kubernetes only, when the app's calls go through Canyon | The file the sidecar reads that token from. Kubernetes renews it, and only the sidecar can read it, never the app. |
| `CANYON_APP_HOST` | The sidecar, Kubernetes only, when Canyon runs the app | The app's address, which the executor uses to ask what the app answered. A release your own platform runs doesn't carry it. |

On Kubernetes the sidecar is `canyon-identity`, which runs in every app's pod in front of the app.
When your own platform runs the apps, the release also gives the sidecar `CANYON_ACCESS_URL`,
`CANYON_ACCESS_GRACE` and `CANYON_ACCESS_TOKEN`: see [Your platform runs the apps](/self-host/hand-off/), and
for the grace, [When Canyon can't be reached](/self-host/hand-off/#when-canyon-cant-be-reached).
