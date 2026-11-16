---
title: Troubleshooting
description: The messages Canyon prints when something stops you, what each means, and what to do, for people who deploy and for whoever runs Canyon.
---

Each entry starts with what Canyon prints, word for word, then says why and what to do. A message
that names a policy is a review's finding instead: [Fix or approve](/build/fix-or-approve/) and the
rule's own page under [Rules](/rules/) cover those.

## Deploying

### The host can't build this kind of app

```text
  ✗ This host can't build this kind of app.
  It builds:
    …
  No buildpack recognised the package, so nothing was compiled: not an error in the code, not a policy block.
```

No buildpack on this Canyon recognised the code, so nothing was built. When the language is one Canyon
doesn't build yet, the last line is a link to ask for it ([Ask for a feature](/reference/ask-for-a-feature/)).
Most Node, Python, Go and
Ruby apps are recognised by their manifest (`package.json`, `requirements.txt`, `go.mod`, `Gemfile`)
at the app's root, so check you deployed from the app's root directory. When the CLI knows what's
missing, it says which file to add and prints what goes in it: a static site, for example, needs a
`project.toml` at its root that has nginx serve its files. When the output also says "A buildpack
also errored while checking", the manifest may be malformed: check it is valid, such as a
`package.json` that parses.

### Build failed

```text
  ✗ Build failed: <the build's last line>
  The review was clean, so this is not a policy block: the build's own lines above say what failed.
  Then run: canyon deploy
```

The review passed and the build itself failed: a dependency that wouldn't install, a compile error,
a build script that exited. The lines above it are the build's own. After approvals, the second line
starts "The review passed under approvals" instead; when the build wrote nothing, the first line is
"✗ Build failed" alone and the second ends "The build wrote no lines." When the output also says
"Not packed, in case the build needs one:", those are files Canyon left out of the upload, such as
`.env` files: a value the build needs goes in a build secret
(`canyon secret set NAME --where build`, [Secrets](/build/secrets/)), not in a file.

### project.toml is refused

```text
  ✗ Build failed: project.toml: buildpack "https://example.com/bp.tgz" has a uri, which pack would fetch outside the build's network: name a buildpack of the builder by its id, or an inline script
```

Canyon read the app's `project.toml` before building and refused it: the build itself never started.
It names a buildpack the host's builder doesn't carry (by `uri`, a buildpack-registry name such as
`acme/thing@1.0.0`, an image, or a path), which the build tool would fetch from outside the build's
network; or it has a key Canyon doesn't read, such as `URI` or `Group` for `uri` or `group`; or it
doesn't parse. Name the builder's own buildpacks by id (the "It builds:" list of a failed detect
names them), use an inline script (`[io.buildpacks.group.script]`), or leave buildpacks out and let
Canyon detect them. Spell every key as the schema does, in lower case.

### A secret's name is refused

```text
env "DOCKER_USERNAME": names are [A-Za-z0-9_-] not starting with a dash, values carry no NUL, and none is reserved: …
```

On a Canyon that runs on one Docker server, Canyon's own tools run beside the app and read some names
from the environment a secret travels in, so a secret named for one of them is refused, and the
line names it:

- **Any secret, runtime or build,** named anything starting `DOCKER_`, in any case (`DOCKER_USERNAME`,
  `docker_password`). The app doesn't start: its deploy, promotion, rollback or Canyon's fix of its system libraries fails.
  An app that had one before Canyon refused them fails at its next one.
- **A build secret** named `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`,
  `SSL_CERT_DIR`, `GODEBUG`, `REGISTRY_AUTH_FILE`, `XDG_RUNTIME_DIR` or anything starting `PACK_`, in
  any case. The build tool runs outside the build's network and would read it too, so the build is
  refused. A runtime secret may take these names.

Rename the secret (`canyon secret list` shows them; `canyon secret set` it under the new name, then `canyon secret rm` the old one)
and have the app or its build read the new name (`npm config set proxy "$MY_PROXY"`, for one). For a
registry at a private address, ask whoever runs Canyon to name it in `CANYON_BUILD_ALLOW`. On
Kubernetes all these names are fine.

### Build failed with build network

```text
  ✗ Build failed: build network: …
```

Canyon couldn't put the build behind its network floor, so it didn't run it: the rest of the line
says why, and it is the host's to fix, not the app's. Common reasons: a Docker network named
`canyon-builds` that Canyon didn't create (remove it, and Canyon makes its own), the holder image not
reachable (`CANYON_IMAGE_EGRESS_HOLDER`), or a host kernel without `nf_tables`. Whoever runs Canyon
reads the executor's log, then deploys again.

### A deploy fails with app network

On Docker, an app whose calls go straight out (Outbound *Off*) runs behind a network floor of its own,
which keeps it off the host and every private address. A deploy whose reason starts `app network: `
couldn't put the app behind it, so the app wasn't started and the version that ran keeps running. It is
the host's to fix, not the app's: the holder image not reachable (`CANYON_IMAGE_EGRESS_HOLDER`), or a
host kernel without `nf_tables`. Whoever runs Canyon reads the executor's log, then deploys again.

### An app can't reach a service on your network

On Docker, an app whose calls go straight out reaches public addresses and its own database, and never
the host Canyon runs on (`host.docker.internal` included), the cloud's metadata address or a private
address. A call to a database or an API on your internal network is refused until whoever runs Canyon
names its address and port in `CANYON_APPS_ALLOW`
([configuration](/self-host/configuration/#docker-executor)). A service on the host itself stays out of
reach: run it somewhere else, or give it an address the host's own ranges don't hold.

### The app did not start

```text
  ✗ The app did not start
```

The build passed and the app stopped as it started: its own error, not a policy block. The app's
last lines are above it. The usual causes:

- **A value it reads is missing.** Set it with `canyon secret set NAME`, then `canyon deploy`.
- **It doesn't listen on `PORT`.** Canyon sets `PORT` to `3000`, and the app must listen there.
- **The build gave it no start command.** When Canyon can tell, the CLI names the file to add and
  what goes in it: a `Procfile` for a Python web app, a `project.toml` for a single-page app whose
  build writes static files.

### An app that keeps restarting

The app's page says:

```text
<version> restarted <n> times since it was deployed to <env>
Deploy again once it's fixed: the build is reused. canyon logs --env <env> has why.
```

The app started, then exited and was started again. `canyon logs --env preview` (or `--env prod`)
shows its own lines. Fix the code or the missing value, and deploy again. A version whose preview
keeps restarting can't be promoted: `canyon deploy --env prod` says
"<version> restarted <n> times since it was deployed; deploy again once it's fixed (the build is
reused)."

### A Node 22 app's log starts with an UNDICI-EHPA warning

```text
(node:1) [UNDICI-EHPA] Warning: EnvHttpProxyAgent is experimental, expect them to change at any time.
(Use `node --trace-warnings ...` to show where the warning was created)
```

When an app's calls go through Canyon (Settings › Outbound), Canyon has Node send them through its
outbound proxy. Node 22 prints this line each time the app starts, because the part of Node that does
this is still marked experimental there. It's harmless: the app's calls work. Node 24, which Canyon
builds a Node app on unless the app asks for another version, doesn't print it.

To silence it, set `NODE_OPTIONS` in each environment, then deploy again:

```sh
printf %s '--disable-warning=UNDICI-EHPA' | canyon secret set NODE_OPTIONS
printf %s '--disable-warning=UNDICI-EHPA' | canyon secret set NODE_OPTIONS --env prod
```

Preview takes it at the next `canyon deploy`, production at its next promotion. If the app already sets
`NODE_OPTIONS`, add the flag to its value after a space. Node 20.11 and later accept the flag; Node 18
refuses it and won't start.

### Canyon could not check this package

The review's row for the tool:

```text
    ✗ opengrep           could not check: <reason>
```

Its finding is titled "Canyon could not check this package with opengrep", and its problem reads
`could not check: <tool>: <reason>`. One of the tools Canyon runs itself (Trivy, Opengrep, ESLint, Checkov or Grype) didn't finish, and
that is a blocking finding: a review that didn't happen never passes. The reason says why, such as
"did not finish in time", "this Canyon host has no image for it" or "its vulnerability database is
over 5 days old". Run `canyon check` again once the tool can run. Whoever runs Canyon fixes a
missing image or a database that can't download ([below](#every-deploy-is-blocked-by-a-tool-with-no-image)).
If it can't be fixed in time, [have the finding approved](/build/fix-or-approve/#approve-it-instead).

### The cluster had no room to run the checks

```text
  ✗ The cluster had no room to run Canyon's checks
  Nothing was reviewed, so there is nothing to fix or approve: it's the cluster's capacity, not the app's code or a policy.
  Run it again in a few minutes: canyon deploy
```

The scans couldn't get a node in time. Run it again in a few minutes. If it keeps happening, an
admin adds nodes or lowers what each app requests (`CANYON_KUBE_APP_CPU_REQUEST`,
`CANYON_KUBE_APP_MEMORY_REQUEST`).

### The cluster had no room to build it

```text
  ✗ The cluster had no room to build it
  That's the cluster's capacity, not the app's code or Canyon: an admin adds nodes, or lowers what apps request (CANYON_KUBE_APP_CPU_REQUEST).
  Then run: canyon deploy
```

The review passed, and the build couldn't get a node; the scheduler's message is the line above it.
Deploy again once an admin has added room.

### No front door is set

```text
No front door is set, so no app can run on this Canyon yet. An admin sets it in Settings › App hosting.
```

On Kubernetes, no app runs until an admin sets [the front door](/self-host/front-door/).

### Canyon hasn't seen this cluster enforce NetworkPolicy

```text
Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.
```

On Kubernetes a build is held off private and link-local addresses by a NetworkPolicy, which only
holds where the cluster's network plugin enforces it. So Canyon builds nothing until the stored front
door's save found it enforced, whoever runs the apps, hand-off included. The check also answers this for a
plugin that lets a new pod out before its policy is in force, and when it couldn't finish. Every front
door saved before Canyon recorded this counts as unchecked: save it once. Canyon doesn't watch the cluster after a save, so turning enforcement off later
doesn't stop builds until the front door is saved again. On
EKS, set both values as [Strict mode on the AWS VPC CNI](/self-host/kubernetes/#strict-mode-on-the-aws-vpc-cni)
shows, then save the front door again and deploy. Until then, **Settings › App hosting** shows this
beside the front door, followed by why:

```text
Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced, so no deploy goes through, whoever runs the apps.
```

### The app isn't on this Canyon

```text
<app> isn't on <host>. If it should be, run: canyon deploy --create
```

The `canyon.toml` here names an app the Canyon you're signed in to doesn't have: the code was
deployed to another Canyon, or the app was deleted. If you're signed in to the wrong one,
`canyon login <the right address>`. Run `canyon deploy --create` only if the app really is new on
this Canyon.

### DATABASE_URL is set by hand

```text
DATABASE_URL is set by hand; remove it with canyon secret rm DATABASE_URL so Canyon can provide postgres db
```

The app declares a database in `canyon.toml` (here one named `db`), and Canyon gives it
`DATABASE_URL` itself. Remove the one you set, then deploy again
([Databases for your app](/build/databases/)).

### Production is held

```text
Production was stopped by <who> on <date>: <reason>. It stays stopped until a responder or an admin lets it run again.
```

A security responder or an admin stopped production. Nothing changes production until one of them
lifts the hold in the dashboard; you can still deploy your fix to preview
([Stopping production](/org/who-can-do-what/#stopping-production)).

### Nobody here can approve the finding

```text
Only an admin or a security responder can approve a blocking finding, and never their own.
```

Fix the code, or ask an admin or a security responder who didn't start the deploy
([Approve it instead](/build/fix-or-approve/#approve-it-instead)). On a push from GitHub, the app's
page may say instead "<org> asks whoever approves a push's blocking findings to link their GitHub
account first.": the approver links theirs with **Link GitHub account**
([GitHub](/build/github/#link-your-github-account)).

## Signing in

### canyon login waits, then the code expires

```text
Canyon is still being set up here, so nobody can approve a sign-in yet — ask whoever installed it.
```

Until the first admin finishes setup, nobody can approve a CLI sign-in. The browser says the same
to anyone else who signs in: "Canyon isn't set up yet. Ask the person who installed it to finish
setup." One of the admins named when Canyon was installed (`CANYON_ADMINS`) signs in and finishes
setup ([First boot and sign-in](/org/first-boot/)).

### Your identity provider signs you in, but Canyon doesn't

Canyon turns away an address that is on neither sign-in list in Settings › People & sign-in. An admin
adds it, or its domain; the [audit trail](/org/audit-trail/#refused-sign-ins) records each refusal
with its reason.

## Running Canyon

### Port 7777 is already allocated

```text
port is already allocated
```

Another program listens on port 7777, which the laptop demo needs (Docker may say
`address already in use` instead). `lsof -nP -iTCP:7777 -sTCP:LISTEN` shows which one: stop it, and
start the demo again ([Try it on your laptop](/start/try-it-on-your-laptop/#start-it)).

### Every deploy is blocked by a tool with no image

```text
could not check: opengrep: this Canyon host has no image for it
```

On Kubernetes, Canyon's Opengrep and ESLint images come from the release's `images.txt`. Set them:
`CANYON_IMAGE_OPENGREP` and `CANYON_IMAGE_ESLINT` on the executor
([Install on Kubernetes](/self-host/kubernetes/#install)), or `images.opengrep` and `images.eslint`
with the [Helm chart](/self-host/helm/#what-you-must-set).

### The ClusterBuilder is never ready

```text
the ClusterBuilder canyon has no image yet
```

A build fails this way until kpack has built Canyon's builder, which takes minutes after an install.
`kubectl wait --for=condition=Ready clusterbuilder/canyon --timeout=15m` waits for it. If it never
becomes ready and `kubectl get clusterlifecycle default-lifecycle` shows a lookup error from before
the network worked, run `kubectl -n kpack rollout restart deploy/kpack-controller`: kpack doesn't
retry it by itself.

### Every lookup times out from a pod, on k3s

k3s hands CoreDNS the host's resolver, and one only the host can reach, such as Tailscale MagicDNS's
`100.100.100.100`, times out every lookup from a pod: the gateway gets no certificate and kpack
pulls nothing. Give k3s resolvers it can reach before installing
([One k3s host, step 1](/self-host/k3s/#1-k3s)).

### The egress gateway's rollout never finishes

The gateway's pods start only once the certificate the server made for the egress check is
published. Publish it, then wait again: [Install on Kubernetes](/self-host/kubernetes/#install), or,
on an upgrade, [Back up, restore and upgrade](/self-host/backup-and-upgrade/#upgrade). If the server
itself waits in `CreateContainerConfigError`, the Secret `canyon-egress` is missing: the upgrade
steps create it.

### The front door won't save

Saving checks the cluster and the certificate first, and a refusal shows the fix, word for word:
the switch your network plugin needs, or the names the certificate must cover
([How people reach apps](/self-host/front-door/#saving-it)). When it says "Canyon could not finish
checking this cluster. Try again in a minute; if it keeps failing, Canyon's log says why.", read
the server's log.

### The server won't start after a restore

```text
CANYON_KEY_FILE was generated at this boot but 7 row(s) are sealed under another key: the key volume is missing
```

The database holds secrets sealed under a key the server doesn't have. Start again from empty
volumes, and restore `files.tar`, which holds the key, before the server's first start
([Back up, restore and upgrade](/self-host/backup-and-upgrade/)).
