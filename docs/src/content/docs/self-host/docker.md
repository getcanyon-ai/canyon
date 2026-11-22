---
title: One Docker server, step by step
description: Run Canyon for a team on one Linux server with plain Docker, signing people in through your identity provider, behind Caddy over https.
---

| | |
|---|---|
| **Time** | About an hour on a fresh server, most of it waiting for images |
| **You need** | Canyon v0.3.0 or later · a Linux server you use for nothing else, with a public IPv4 address, 4 vCPU, 8 GB and about 80 GB of disk, and no cloud instance identity · Docker 28 or newer, with Compose v2 · a domain with DNS you control · an OIDC client at your identity provider · `curl` and `openssl`. Details: [What you need](#what-you-need) |
| **You get** | Canyon at `https://<domain>`, signing people in through your identity provider, and apps at `https://<app>.preview.<domain>` and `https://<app>.prod.<domain>`, each behind that sign-in. Both Postgres servers run on the same server, and you back them up |
| **Not covered** | A second server: while this one is down, so are Canyon and every app ([Caveats](#caveats)) |

This is the laptop demo's stack, set up for a team. It runs the same containers from the same
release, with four differences:

- people sign in through your identity provider;
- Caddy ends TLS for your own domain;
- the server is reached through Caddy alone;
- the passwords and tokens are yours.

One file in the release, `deploy/docker/team.yml`, makes those changes, and a `.env` file beside it
holds your values. The release must be v0.3.0 or later: earlier ones don't ship `team.yml`.

Read [Security](#security) before you pick the server. Canyon's executor holds the server's Docker
socket, and that decides what else may run there: nothing.

```mermaid title="One Docker server: Caddy ends TLS, Canyon's server is the front door of Canyon and of every app"
flowchart TD
  people["People and coding agents<br>your domain and its app wildcards<br>point at the server"]
  idp["Your identity provider<br>an OIDC client"]
  subgraph host["One Linux server, Docker"]
    caddy["Caddy, on ports 80 and 443<br>ends TLS"]
    server["canyon-server<br>publishes no port"]:::canyon
    executor["canyon-executor<br>holds the Docker socket"]:::canyon
    apps["Each app's container<br>on a Docker network of its own"]
    pg[("Canyon's Postgres")]
    apg[("The apps' Postgres")]
  end
  people -->|"https"| caddy
  caddy -->|"a network the two share<br>with nothing else"| server
  server -->|"sign-in"| idp
  server --> pg
  server -->|"scan, build, run"| executor
  executor --> apps
  server -->|"each app's address,<br>after sign-in"| apps
  apps --> apg
```

Canyon's server answers Canyon's own address and every app's. For an app, it checks the person's
sign-in and the app's access list, then forwards the request with Canyon's identity token
([How people reach apps](/self-host/front-door/#on-one-docker-host)). There is no front door to
choose on Docker.

## What you need

| | |
|---|---|
| A Linux server with root and a public IPv4 address | A virtual machine of its own: nothing else runs on it, and it has no cloud instance identity ([Security](#security)). This guide was written for Ubuntu 24.04. |
| 4 vCPU and 8 GB RAM, and about 80 GB of disk | Each build runs the buildpack builder, whose image alone is 3.3 GB. Each app container may use 1 GB of memory and has no CPU limit, and runs beside a small container that keeps its network floor, so two per app, preview and production (about 5–10 MB each with its runtime shim, 32 MB at most; one more while a redeploy runs); each scan, 2 CPU and 2 GB. Images, build caches and both databases share the disk. |
| Docker 28 or newer, with Compose v2 | Install Docker Engine as [Docker's own guide](https://docs.docker.com/engine/install/) says for your distribution. |
| A domain with DNS you control | An A record for `<domain>`, and A records for `*.preview.<domain>` and `*.prod.<domain>`, all pointing at the server. A domain used only for Canyon is best ([TLS](#tls-and-certificates)). |
| An OIDC client | Its redirect URI is `https://<domain>/auth/callback`. You need its issuer, client ID and client secret, if it has one. |

Run every step as root (`sudo -i`). Set these once, in the shell you run every step from. `RELEASE`
is the Canyon release you install, v0.3.0 or a later one of
[Canyon's releases](https://github.com/getcanyon-ai/canyon-releases/releases).

```sh
export DOMAIN=canyon.example.com   # Canyon's host; apps live at <app>.preview.$DOMAIN and <app>.prod.$DOMAIN
export RELEASE=v0.10.0             # the release's tag, such as v0.10.0 (v0.3.0 or later)
mkdir -p /opt && cd /opt
```

## 1. DNS and the firewall

Point `$DOMAIN`, `*.preview.$DOMAIN` and `*.prod.$DOMAIN` at the server's public address, then
check that each one resolves to it:

```sh
for name in $DOMAIN check.preview.$DOMAIN check.prod.$DOMAIN; do getent ahostsv4 $name | head -1; done
```

Add A records alone, no AAAA, unless you turn on IPv6 in Docker: without it, Docker hands Caddy every
IPv6 client from one address of its own, and they all share one sign-in rate limit.

Open ports 80 and 443 to the internet in your cloud's firewall or security group, and SSH to the
addresses you manage the server from. Close everything else there.

Docker publishes Caddy's 80 and 443 itself, around a host firewall such as `ufw`, and so it does any
`ports:` line you add to a compose file. Make the cloud firewall the one that decides.

## 2. The OIDC client

Register Canyon at your identity provider as a web application. Entra, Okta, Google Workspace and any
other OIDC provider take the same values:

- **Redirect URI:** `https://<domain>/auth/callback`. It is the only one: apps sign in through
  Canyon's own address, so they need none of their own.
- **Scopes:** `openid profile email`.
- **Client secret:** optional. Without one, Canyon signs in as a public client, with PKCE.

Note the issuer (such as `https://login.microsoftonline.com/<tenant>/v2.0`), the client ID and the
secret. [Sign-in, the AI model and keys](/org/set-up/#sign-in) says what each one does.

## 3. Download Canyon

Download the release and its checksums, and unpack the download only if it matches them:

```sh title="Download Canyon"
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/canyon-deploy.tar.gz
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS && tar -xzf canyon-deploy.tar.gz && cd canyon
```

`sha256sum` prints `canyon-deploy.tar.gz: OK` before anything is unpacked. If it prints `FAILED`,
nothing is unpacked: download both files again.

`canyon/` holds the release's `docker-compose.yml`, `docker-compose.override.yml` (the release's
images) and `deploy/docker/team.yml`. Every later step runs in `canyon/`.

If this server ever ran the laptop demo from a directory named `canyon`, its volumes have the same
names as this setup's, and they hold the demo's sample org and its passwords. Remove them first,
from that directory: `docker compose --profile demo-apps down -v`.

## 4. The `.env` file

Compose reads `.env` from `canyon/`. Its first line, `COMPOSE_FILE`, adds `deploy/docker/team.yml` to
the two compose files, so every `docker compose` command in `canyon/` runs the team setup. Replace the
issuer, the client ID and `you@example.com` with yours, and add the client secret if there is one,
then run this, **once**:

```sh title="Write .env"
umask 077
cat > .env <<EOF
COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:deploy/docker/team.yml
CANYON_DOMAIN=$DOMAIN
CANYON_OIDC_ISSUER=https://login.example.com/v2.0
CANYON_OIDC_CLIENT_ID=canyon
CANYON_OIDC_CLIENT_SECRET=
CANYON_ADMINS=you@example.com
CANYON_POSTGRES_PASSWORD=$(openssl rand -hex 24)
CANYON_APP_POSTGRES_PASSWORD=$(openssl rand -hex 24)
CANYON_EGRESS_RECEIVER_TOKEN=$(openssl rand -hex 32)
EOF
docker compose config --quiet && echo ".env is complete"
```

- **`CANYON_ADMINS`** are the first admins, comma-separated, and the only people who can sign in
  until one of them finishes setup.
- **The two Postgres passwords** are read once, when each database's volume is first created.
  Running the block again draws new ones, which the existing databases don't know, and the server
  can't log in. To change a value later, edit that line of `.env` instead. To start over from
  nothing, with every app and all data gone, run `docker compose down -v` first.
- **`CANYON_EGRESS_RECEIVER_TOKEN`** is what Canyon's egress gateway sends the server
  ([configuration](/self-host/configuration/)).
- **An AI model:** add `ANTHROPIC_API_KEY=<your key>` to `.env`. Without a model the scanners still
  run, and the AI review shows as skipped.

`.env` holds the passwords, so `umask 077` makes it readable by root alone. A value left out stops
compose before anything starts, naming the variable. For any other setting in the
[configuration reference](/self-host/configuration/), write a compose file of your own beside
`.env`, such as `local.yml`, with the setting under `services` → `server` → `environment`, and append
`:local.yml` to `COMPOSE_FILE`. `CANYON_BUILD_ALLOW`, `CANYON_APPS_ALLOW` and `CANYON_IMAGE_EGRESS_HOLDER`
are the exceptions: they go in `.env` itself, which the bundled compose file hands the executor. A new release's download replaces `docker-compose.yml` and
`deploy/docker/team.yml`, and never touches `.env` or your own file.

## 5. The Caddyfile

Caddy runs as part of the stack. It ends TLS on the server's 80 and 443, and forwards everything to
Canyon's server on a Docker network the two share with nothing else. It keeps the `Host` header,
which Canyon routes apps by, and sets `X-Forwarded-For` to the address it saw. It gets a certificate
for `$DOMAIN` at its first start, and one for each app's address on its first visit, once Canyon says
that app exists.

```sh title="Write the Caddyfile"
cat > Caddyfile <<EOF
{
	admin off
	servers {
		protocols h1 h2
	}
	on_demand_tls {
		ask http://canyon-server:7777/tls/ask
	}
}
$DOMAIN {
	header Strict-Transport-Security "max-age=31536000"
	reverse_proxy canyon-server:7777
}
*.preview.$DOMAIN, *.prod.$DOMAIN {
	tls {
		on_demand
	}
	header Strict-Transport-Security "max-age=31536000"
	reverse_proxy canyon-server:7777
}
EOF
```

After editing the Caddyfile, run `docker compose restart caddy`.

### TLS and certificates

Before each app certificate, Caddy asks Canyon whether the name is one it serves, and Canyon says yes
only for its own host and the address of an app that exists. Canyon answers that question for Caddy
alone: nobody outside can ask Canyon directly. But a TLS handshake for a guessed name still shows
whether that app exists, and a guess that hits puts the name in the certificate logs. Two things
follow from getting each certificate as an app is first opened:

- **Let's Encrypt limits certificates per registered domain**: `example.com` as a whole, every
  subdomain of it included. Canyon's apps share that weekly limit with everything else your company
  runs under it.
- **Every certificate is public.** Certificate Transparency logs list each one, so every app's name,
  `<app>.preview.<domain>`, becomes public the first time someone opens it.

So give Canyon a domain of its own, used for nothing else. Or use one wildcard certificate per
environment instead, which Caddy gets over DNS-01 with your DNS provider's API. That needs a Caddy
built with your provider's DNS module: name its image in `.env` as `CANYON_IMAGE_CADDY`, and put a
`tls` block with `dns` in both sites, as
[Caddy's documentation](https://caddyserver.com/docs/automatic-https#dns-challenge) shows.

## 6. Start Canyon

```sh
docker compose up -d --wait
docker compose logs server | grep -E 'auth oidc|oidc discovery|Canyon is up'
docker compose exec caddy wget -qO- http://canyon-server:7777/ready   # {"status":"ready"}
```

The log names your issuer, says discovery from it is complete, and ends with `Canyon is up: open
https://<domain>`. The server isn't ready until it has read your issuer's discovery document; if
`/ready` says it is waiting on `oidc`, `docker compose logs server` says why. Every container restarts
with Docker, so Canyon comes back after a reboot.

Caddy needs a minute or so for Canyon's certificate. `docker compose logs caddy` shows it: a line with
`certificate obtained successfully` and your domain. Then, from anywhere:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://$DOMAIN/ready   # 200, over a Let's Encrypt certificate
```

Only Caddy is published, on 80 and 443. The server, the databases and the executor are on Docker
networks the internet can't reach. Canyon trusts the `X-Forwarded-For` of Caddy's one address
(`CANYON_TRUSTED_PROXIES`, which `team.yml` sets), so each person has a sign-in rate limit of their
own, and the device page's "Requested from" and the audit trail's `source` show their own address.

## 7. First sign-in and the first deploy

1. **Open `https://<domain>`** and sign in as one of `CANYON_ADMINS`.
2. **Finish setup.** The setup screen asks who else may sign in: a domain, a list of addresses, or
   both. Until it's done, only `CANYON_ADMINS` can sign in
   ([First boot and sign-in](/org/first-boot/#the-setup-screen)).
3. **Deploy a first app** from any machine, with the CLI of the same release
   ([Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/) is the page to send your
   developers):

   ```sh
   curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/install.sh -o install.sh && sh install.sh
   canyon login https://$DOMAIN    # prints a code; approve it in the browser, signed in
   cd ~/my-app && canyon deploy    # reviews and builds it, then runs it at https://my-app.preview.$DOMAIN
   canyon share sam@example.com    # an app is private to its team until it is shared
   ```

A teammate who opens the app's address signs in through your identity provider. Until it is shared
with them, Canyon shows them a page that says whom to ask, with an **Ask for access** button. Once
it is shared, the app answers them with Canyon's identity token, which says who they are
([Sign-in inside your app](/build/sign-in-in-your-app/)).

## 8. Connect your agents

Your developers connect their coding agents in one of two ways ([Connect your agent](/start/connect-your-agent/)):

- **On a laptop:** `canyon login https://$DOMAIN`, then `canyon skill install`. It sets up Canyon's skill, its
  session hook and its MCP server for each agent it finds.
- **In the cloud:** Claude Code on the web and Cursor's cloud agents connect to `https://$DOMAIN/mcp`, and each
  person allows it once in the browser. That needs Canyon reachable from the internet, Canyon's GitHub
  App on your repositories ([Connect GitHub](/build/github/)), and each person's GitHub account linked.

## Back up, upgrade and uninstall

**Back up** Canyon's Postgres, the apps' Postgres and the server's key and code, all three at the same
time, before every upgrade: [Back up, restore and upgrade](/self-host/backup-and-upgrade/#on-docker).
Run its commands in `canyon/`, where `.env` makes them the team setup's. Keep `.env` with the
database dumps, since it holds their passwords. Keep the key's archive apart from both, or encrypt it.

**Upgrade** by downloading the new release, v0.3.0 or later, over the old one, from `/opt`, then
starting it again:

```sh
cd /opt && curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/<new release>/canyon-deploy.tar.gz
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/<new release>/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS && tar -xzf canyon-deploy.tar.gz && cd canyon && docker compose up -d --wait
```

The download replaces the release's files and leaves `.env`, your own compose file and the
`Caddyfile` as they are. The server migrates its database when it starts, and apps keep running.
[Upgrade](/self-host/backup-and-upgrade/#upgrade) says what carries over, and how to roll back.

**Uninstall** removes Canyon, Caddy and its certificates, every app Canyon ran and all their data,
with no way back:

```sh
docker compose down -v
c=$(docker ps -aq --filter label=canyon.app); [ -z "$c" ] || docker rm -f $c
n=$(docker network ls -q --filter label=canyon.app); [ -z "$n" ] || docker network rm $n
c=$(docker ps -aq --filter label=canyon.build; docker ps -aq --filter label=canyon.scan); [ -z "$c" ] || docker rm -f $c
n=$(docker network ls -q --filter label=canyon.build); [ -z "$n" ] || docker network rm $n
v=$(docker volume ls -q --filter dangling=true --filter name=^canyon-cache- --filter name=^canyon-scandb-); [ -z "$v" ] || docker volume rm $v
```

## Security

**Canyon's executor holds the server's Docker socket, which is root on the host.** The executor
scans, builds and runs every app, so it needs the socket; Canyon never mounts it into anything that
holds the encryption key or the database. Each app runs in a container of its own, on a Docker
network of its own, with every Linux capability dropped, and each build runs the app's own build
scripts in buildpack containers. But containers share the host's kernel: a container that escapes,
or anything that takes over the executor or the server, has the whole host, and with it every app,
every secret and both databases. So:

- **Give Canyon a server of its own.** A virtual machine that runs Canyon and nothing else, with no
  other users and no other services.
- **Builds reach public registries and nothing private.** An app's build runs the app's own scripts
  (an npm `postinstall`, a `setup.py`, a Gradle plugin), each in a network namespace of its own on the
  Docker network `canyon-builds`, whatever Outbound says. From there it reaches DNS and TCP 80 and 443
  at public addresses, and never the cloud's metadata address (`169.254.169.254`), Azure's WireServer,
  this host's private addresses or Canyon's own networks. A registry or mirror at a private address
  is reached only once `CANYON_BUILD_ALLOW` in `.env` names it
  ([configuration](/self-host/configuration/#docker-executor)). Nor does it reach any address of this
  host's own, a public one on its network interface included, where Canyon itself is published.
- **Running apps reach the internet and nothing private.** With apps' calls going straight out
  (Outbound *Off*, the default), each app runs in a network namespace of its own, kept by a container
  of its own on the app's network. From there it reaches its own network (Canyon's server and its
  database), DNS and public addresses on any port, and never this host, its network's gateway, the
  metadata address, Azure's WireServer, Canyon's other networks or any private address. A service on
  your internal network is reached only once `CANYON_APPS_ALLOW` in `.env` names it
  ([configuration](/self-host/configuration/#docker-executor)); never name this host's own range there.
  An app deployed before the release that brought this keeps running without it until its next deploy:
  **redeploy every app once after upgrading** ([Back up, restore and upgrade](/self-host/backup-and-upgrade/#the-upgrade-that-keeps-apps-off-the-host-docker)).
- **Give the VM no cloud identity anyway.** Everything on the host that isn't a build or an app
  still reaches the metadata address. Give the VM no instance role, service account or managed
  identity it doesn't need, and on AWS require IMDSv2 with a hop limit of 1, which a container's
  request doesn't survive.
- **Send running apps' calls through Canyon.** Turn on **Settings › Outbound › Through Canyon**
  ([Outbound](/org/outbound/), beta): an app then reaches nothing outside its network but Canyon's
  egress gateway, which records each call and can limit public destinations too. Outbound covers
  running apps; builds have their own fixed limit, above.
- **Only `oidc` on a server.** `CANYON_AUTH` `off` and the laptop demo's `demo` make every caller an
  admin, so they are for one person on one machine and never for a server. `deploy/docker/team.yml`
  sets `oidc`, and Canyon refuses to start under `oidc` without an `https` `CANYON_PUBLIC_URL`, and
  under `demo` on a public one.
- **Keep `.env` and the backups private.** `.env` holds both database passwords, and the backup holds
  the key every secret is encrypted under.

## Caveats

- **One server is one point of failure.** While it's down, so are Canyon and every app. Nothing here
  moves to a second server; for that, [install on Kubernetes](/self-host/kubernetes/).
- **The Docker socket is root on the host**, as [Security](#security) says: give Canyon a VM of its
  own, with no cloud identity it doesn't need.
- **Builds reach public TCP 80 and 443 alone**, plus what `CANYON_BUILD_ALLOW` in `.env` names: a build
  that fetches over git+ssh, or from a registry on another port or at a private address not named
  there, fails at that fetch. Builds resolve names through the host's own resolvers, so leave
  `daemon.json`'s `dns` unset ([configuration](/self-host/configuration/#docker-executor)).
- **Running apps reach no private address**, plus what `CANYON_APPS_ALLOW` in `.env` names: an app
  that calls a database, an API or anything else on your internal network, or on this host, fails at
  that call until it is named there.
- **Every app's name is public** once it has a certificate, and app certificates count against your
  registered domain's limit, unless you use wildcard certificates ([TLS](#tls-and-certificates)).
- **Docker's published ports go around `ufw`**: Caddy's 80 and 443, and any `ports:` line in a compose
  file of your own. The cloud firewall is the one that decides ([step 1](#1-dns-and-the-firewall)).
- **Apps have no CPU limit.** One busy app can slow Canyon and the other apps on the server.
- **A laptop demo doesn't become a team server.** Start from empty volumes (step 3).
