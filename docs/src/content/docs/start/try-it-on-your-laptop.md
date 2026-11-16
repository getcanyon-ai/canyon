---
title: Try it on your laptop
description: Run Canyon and its sample org on one Docker host, watch it block a SQL injection, then deploy an app of your own.
---

| | |
|---|---|
| **Time** | The first start: 20 to 30 minutes at 25 Mbit/s, longer on a slower line, while it downloads about 3.5 GB. A restart: 5 to 10 minutes |
| **You need** | The `canyon` CLI · Docker 28 or newer, with Compose v2 · 4 CPUs and 6 GB of memory for Docker · about 18 GB of free disk · port 7777 free. Details: [What you need](#what-you-need) |
| **You get** | Canyon at `http://localhost:7777`, with a sample org of seven apps, sign-in off, and apps at `http://<app>.preview.localhost:7777` |
| **Not covered** | Other people: it listens on your laptop alone. Keeping anything: it's a demo |

The demo is Canyon and a fictional company, Meridian, with seven apps in realistic disrepair. It runs
on your laptop with sign-in turned off, and needs no account and no AI model. This page starts it,
shows it blocking a SQL injection, deploys an app of your own, and cleans up.

## What you need

- Docker 28 or newer, with Compose v2.
- About 18 GB of free disk, most of it for Canyon's build image (3.3 GB), the scanners' databases
  (several GB) and the sample apps' builds. On Docker Desktop or Colima, that space is on the Docker
  VM's disk.
- 4 CPUs and 6 GB of memory for Docker. At rest the stack and its sample apps use about 300 MB; the
  builds and reviews of a first start need the rest.

## Start it

Install the CLI, then start the demo in a terminal. [Take the tour](/start/take-the-tour/) starts it
the same way:

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh
canyon demo up
```

`canyon demo up` checks this machine first. If something is missing it lists each gap with its fix,
and starts nothing; it never installs or changes anything itself. Then it downloads the demo into
`~/.canyon/demo` and starts it. The first start builds four of the sample apps (claims-portal,
endorsement-hub, claims-archive and premium-ledger) through Canyon itself, so the fleet shows real
running apps and a real blocked version. It takes 20 to 30 minutes at 25 Mbit/s, longer on a slower
line, because it downloads about 3.5 GB on the way, most of it Canyon's build image and the scanners'
databases. A restart: 5 to 10 minutes. After `canyon demo down --purge`, nearer the first: the
scanners' databases download again and the apps build again.

It names each stage and each sample app as it goes, signs the CLI in to the demo, and ends on:

```text
  ✓ Canyon is ready at http://localhost:7777

  Next, take the guided tour: canyon demo tour
```

Open `http://localhost:7777` in your browser. The demo binds to localhost only, and says on every
screen that sign-in is off. You can run `canyon demo up` again at any point: it keeps what it
downloaded, and on a demo that is already running it signs in and says the same lines.

If your CLI was signed in to another Canyon, `canyon demo up` says that the demo has replaced it, and
`canyon demo down` names the command that goes back.

**Without the CLI,** start the same demo with Docker Compose, from a folder inside your home folder
(Colima shares only your home folder with its Docker VM):

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-deploy.tar.gz | tar -xz && cd canyon
docker compose --profile demo-apps up -d
```

The sample apps are done when `docker compose logs -f demo-apps` says `The sample apps are running,
and claims-portal's next version is blocked at review.` Stop it from the same folder with
`docker compose --profile demo-apps down -v`. Port 7777 must be free: Compose doesn't check it, and
under Colima it starts anyway while your browser reaches the other program;
`lsof -nP -iTCP:7777 -sTCP:LISTEN` shows which program holds it.

## What's running

```mermaid title="The laptop demo: Canyon and its apps, all in Docker on your laptop"
flowchart TD
  you["Your browser and the canyon CLI"]
  subgraph laptop["Docker on your laptop"]
    server["server<br>canyon-server, on 127.0.0.1:7777"]:::canyon
    pg[("postgres<br>Canyon's records")]
    executor["executor<br>scans, builds with pack, runs apps"]:::canyon
    apps["Each app's container<br>on a Docker network of its own"]
    apppg[("app-postgres<br>the apps' databases")]
    egress["egress<br>Canyon's egress gateway, used once<br>apps' calls go through Canyon"]:::canyon
  end
  you -->|"localhost:7777, and each app at<br>app.preview.localhost:7777"| server
  server --> pg
  server -->|"scan, build, run"| executor
  executor -->|"starts"| apps
  server -->|"forwards each app's address"| apps
  apps --> apppg
  apps -.-> egress
```

- **`server`** is Canyon: the dashboard, the API and the review. It answers each app's address too,
  and forwards the request to the app.
- **`executor`** runs the scanners, builds each app with buildpacks and starts it. Each app gets a
  Docker network of its own, shared with the server, `app-postgres` and, while its calls go through
  Canyon, the egress gateway, so apps can't reach each other, Canyon's own database or its API.
- **`egress`** is idle until an admin sends apps' calls through Canyon
  ([Outbound](/concepts/modes/#outbound-beta), beta).

## Sign-in is off

The demo has no sign-in: anyone who can reach it is an admin, and every screen says so. Docker
publishes Canyon's port on your laptop's `127.0.0.1` alone, so nobody else on your network reaches
it. Open it at `localhost`: with sign-in off, Canyon answers a request addressed to any other name
with `421 Misdirected Request`, so a web page you visit can't act as you on it. It also closes,
before reading it, a connection that arrives from another machine's own address. Keep the port on
`127.0.0.1`: some Docker setups forward another machine's connection as if it came from yours, so if
Docker publishes the port anywhere else, Canyon stops and its log names the binding and the fix. Two things
it can't tell apart from you: a tool on your laptop that forwards other machines to the port with
`Host: localhost` (an nginx or Caddy rewriting `Host`, `ngrok --host-header=rewrite`, `ssh -g -L`), and, on
Docker Desktop, OrbStack and Colima, an app the demo runs calling `host.docker.internal:7777`. Don't run
the first, and deploy only apps you trust.

Its `docker-compose.yml` sets `CANYON_AUTH=demo`. On one machine you'd normally turn
sign-in off with `CANYON_AUTH=off`, but then Canyon refuses to start unless it listens on loopback.
In the demo the server runs inside a container, and has to listen on the container's network so
Docker can forward your laptop's port 7777 to it. That is the only difference, and it is why the
demo's value has its own name. Don't use it anywhere but this demo: a Canyon that other people
reach signs them in through your identity provider ([Sign-in, the AI model and keys](/org/set-up/)).

## Look at the fleet

The dashboard opens on the fleet. Here it is from the CLI, `canyon fleet`, once you have
[installed it](#deploy-an-app-of-your-own). The eighth app, `team-notes`, is the one deployed further
down this page:

```text
  8 apps

  ✗ 1 app with secrets in code
  ! 1 app never reviewed
  ! 2 apps with no owner
  ✗ 2 apps blocked

  APP              OWNER           STACK                  ENV      REVIEW            UPDATED
  commission-calc  Priya Raman     React + Vite           —        ✗ blocked         38d
  claims-portal    Dana Whitfield  Express · SQLite       preview  ✗ blocked         1m
  quote-engine     —               FastAPI · Postgres     —        · never reviewed  41d
  fnol-intake      Marcus Bell     Nuxt 3 · Postgres      —        ✓ passed          40d
  endorsement-hub  Priya Raman     Remix · Postgres       preview  ✓ passed          4m
  claims-archive   Tomas Neri      Remix · Postgres       prod     ✓ passed          2m
  premium-ledger   Dana Whitfield  Next.js 15 · Postgres  prod     ✓ passed          2m
  team-notes       —               —                      prod     ✓ passed          now

  canyon fleet --unowned        2 apps
  canyon fleet --never-reviewed 1 app
  Open in Canyon http://localhost:7777/#/fleet
```

`quote-engine` was never reviewed: Canyon has no opinion about it at all, which is worse than an
app with findings.

## Watch it stop something

The fleet's *Start here* line points at commission-calc, a seeded app whose last version is blocked
for a Stripe key in its code. This page follows claims-portal, whose blocked version the demo itself
just sent for review: it is trying to ship a claims search that builds its SQL from the request.
Open claims-portal in the fleet to see its review. `canyon check` prints the same review in a
terminal:

```text
  canyon 696e7ae · claims-portal · localhost:7777

  ✓ Package    4 files · 2.5 KB · → localhost:7777

  Review
    · opengrep: waiting for a scanner slot (1st in line)
    · trivy: waiting for a scanner slot (2nd in line)

  ✗ Review blocked · 1 blocking finding
    · AI review          off (no AI model set up on this Canyon)
    · design.org-system: no package.json in this app lists a UI framework, so there was no UI to check
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✗ opengrep           1 blocking finding
    ✓ trivy              clean

  ────────────────────────────────────────────────────────────────────────────

  critical  Improper neutralization of special elements used in an SQL command ('SQL Injection')
  server.js:20:27

    20 │   const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all();
       │                           ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

  Improper neutralization of special elements used in an SQL command ('SQL
  Injection'). A value from the request reaches the text of a SQL statement,
  so whoever sends the request can rewrite the statement: read other rows,
  change them or drop the table.

  fix   Pass the user's input to the query as a bound parameter instead of
        building the SQL text from it.
  why   Policy "No hand-built SQL from user input"

  ────────────────────────────────────────────────────────────────────────────

  1 blocking finding · 0 warnings

  Fix it, then check again without deploying:
    canyon check

  Only an admin or a security responder can approve a blocking finding, and never their own.

  Open in Canyon http://localhost:7777/#/apps/claims-portal

  Done in 36.1s · run gr_f1988011cb6fc806
```

Nothing was built, so this version never reached preview, and preview keeps running the one before
it. The review lists the checks that passed beside the one that didn't, and says the AI review was
off: with no AI model set up, nothing checked the rule that every route needs sign-in.

The fix is the one the finding says: pass the policy number as a bound parameter. `String(…)` makes
it one string, so a search with no policy number, or with two, finds nothing instead of failing.

```js
const rows = db.prepare("SELECT * FROM claims WHERE policy_number = ?").all(String(req.query.policy ?? ""));
```

The sample apps' source isn't in the download, but once you have
[installed the CLI](#start-it), `canyon pull --app claims-portal` fetches it into an
empty directory. Or let your coding agent show you the whole loop: [Take the tour](/start/take-the-tour/).

Here is `canyon check` on claims-portal with that line fixed:

```text
  canyon 696e7ae · claims-portal · localhost:7777

  ✓ Package    4 files · 2.5 KB · → localhost:7777

  Review
    · gitleaks: waiting for a scanner slot (1st in line)
    · opengrep: waiting for a scanner slot (2nd in line)
    · trivy: waiting for a scanner slot (3rd in line)

  ✓ Review found nothing blocking in what was checked · 0 blocking, 0 warnings
    · AI review          off (no AI model set up on this Canyon)
    · design.org-system: no package.json in this app lists a UI framework, so there was no UI to check
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✓ opengrep           clean
    ✓ trivy              clean

  Done in 36.1s · run gr_0b90f66d8a15a21d
```

## Deploy an app of your own

Then deploy from your app's directory. This is the first deploy of a two-file Node app in a
directory called `team-notes`:

```text
  No canyon.toml here — setting up this app.
  app: cheery-fox-gbz (named "team-notes" after this folder)

  ✓ Wrote canyon.toml — commit it so your team deploys the same app.

  canyon <version> · cheery-fox-gbz → preview · localhost:7777

  ✓ Package    3 files · 336 B · → localhost:7777

  ✓ Review found nothing blocking in what was checked · 0 blocking, 0 warnings
    · AI review          off (no AI model set up on this Canyon)
    · design.org-system: no package.json in this app lists a UI framework, so there was no UI to check
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✓ opengrep           clean
    ✓ trivy              clean

  ✓ Build      [25.0s]
  ✓ Deploy     preview

  Live   http://cheery-fox-gbz.preview.localhost:7777 · answered 200 OK at /
         Team notes are coming soon.
  Fleet  http://localhost:7777/#/apps/cheery-fox-gbz
  Sign-in is off on this Canyon, so anyone who can reach it can open every app.
  No description yet — add description = "…" to canyon.toml so colleagues see what it does.

  Next: canyon deploy --env prod

  Done in 52.5s · run gr_a50efbd65f2cf6ed
```

The first deploy wrote a `canyon.toml` with the app's id: commit it, so everyone deploys the same
app. Most Node, Python, Go and Ruby apps need nothing more. To put it in production, run
`canyon deploy --env prod`. It asks before it changes production; this run answered with `--yes`:

```text
  canyon <version> · cheery-fox-gbz preview → prod · localhost:7777

  Promoting 2b3d3b (passed review) from preview to production. Nothing runs in production yet.
  Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.

  Promote
  │ deploying to production
  │ live at http://cheery-fox-gbz.prod.localhost:7777

  ✓ Promote    prod

  Live   http://cheery-fox-gbz.prod.localhost:7777 · answered 200 OK at /
         Team notes are coming soon.
  Fleet  http://localhost:7777/#/apps/cheery-fox-gbz
  Access anyone who can reach this Canyon (sign-in is off)

  Done in 12.1s
```


## Add an AI model

Without a model the scanners still run, and the AI review reports itself as skipped. To have it
review your own code, give the demo's server a model. Compose doesn't pass your shell's variables to
it, so edit `~/.canyon/demo/canyon/docker-compose.yml`: under `server` → `environment`, uncomment
`- ANTHROPIC_API_KEY=` and put your Claude API key after the `=`. For an endpoint that speaks the
Anthropic Messages API instead, such as a gateway in front of Bedrock or Vertex, uncomment
`- CANYON_MODEL_URL=` and set it to that endpoint. Then start the server again with it:

```sh
cd ~/.canyon/demo/canyon && docker compose -p canyon-demo --profile demo-apps up -d
```

Only review requests and the dashboard's setup chat leave your machine, and only to that endpoint. The
next `canyon demo up` after a `canyon demo down` unpacks `docker-compose.yml` again, so put the line
back after one. (Started without the CLI, edit `canyon/docker-compose.yml` and run
`docker compose --profile demo-apps up -d` in `canyon/`.)

## Clean up

```sh
canyon demo down
```

This stops the demo and removes what it made on this Docker host: its containers and data, and the
containers, networks and build caches of its apps. Nothing another Canyon on the same Docker made is
touched. Images and the scanners' databases (several GB) stay, so the next start doesn't download or
build them again. To remove those too, and the demo's files in `~/.canyon/demo`:

```sh
canyon demo down --purge
```

Both end by saying the demo is gone, or by naming what Docker kept, if anything is left.

If you set up a coding agent with `canyon skill install`, as the tour does, its session hook still
looks for this Canyon at the start of every session. Take Canyon out of your coding agents:

```sh
canyon skill uninstall
```

Run it in the folder you ran `canyon skill install` in, so it removes Cursor's files there too.

**Next:** [Take the tour](/start/take-the-tour/): one prompt, and your coding agent walks you through
Canyon in three chapters, from a blocked deploy to production. Or [Use it from your coding agent](/start/use-it-from-your-coding-agent/):
teach Claude Code to deploy with Canyon, and ask it to ship an app.
