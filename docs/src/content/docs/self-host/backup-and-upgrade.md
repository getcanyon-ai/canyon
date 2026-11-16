---
title: Back up, restore and upgrade
description: The three things to back up so Canyon can come back, how to restore them, and how to move to a new release.
---

Canyon keeps its state in three places and backs up none of them. Back up all three, at the same
time, before every upgrade:

- **Canyon's Postgres** (`CANYON_DATABASE_URL`): apps, versions, reviews, settings, the audit trail,
  and every secret, encrypted.
- **The apps' Postgres** (`CANYON_APP_DATABASE_URL`): the database and login of each app that
  declares one ([Databases for your app](/build/databases/)).
- **The server's state directory**, `/var/lib/canyon`. It holds `keys/`, the key every secret is
  encrypted under, and `repos/`, the uploaded code of every version.

Back up all three at the same time. A copy of the database without the key holds secrets nothing
can open, and the server refuses to start on it.

The backup's `files.tar` holds the key, so keep the backup folder as private as the key itself. Better,
keep `files.tar` apart from the two dumps, or encrypt it: whoever holds the key and a dump holds every
secret in it.

## On Kubernetes

These commands were run against the manifests from [Install on Kubernetes](/self-host/kubernetes/),
on a kind cluster (Kubernetes running in Docker), with both databases as plain Postgres 16 servers.
They haven't been run on EKS or RDS.

Run them from a machine that reaches both databases, with `kubectl` for the cluster and the
Postgres 16 client tools. `CANYON_DATABASE_URL` and `CANYON_APP_DATABASE_URL` are the values in your
Secret `canyon-server`. `pg_dumpall` connects once per database and reads the password from
`PGPASSWORD` or `~/.pgpass`, not from the URL, so set `PGPASSWORD` to the apps' Postgres admin
password.

### Back up

```sh
mkdir -m 700 backup
pg_dump -Fc "$CANYON_DATABASE_URL" > backup/canyon.dump
pg_dumpall -d "$CANYON_APP_DATABASE_URL" --exclude-database=postgres > backup/apps.sql
kubectl -n canyon exec deploy/canyon-server -- tar -C /var/lib/canyon -c keys repos > backup/files.tar
```

Keep the values of the Secret `canyon-server` with the backup.

On a managed Postgres such as RDS, `pg_dumpall` can't read the logins' passwords: the admin isn't a
superuser there, so add `--no-role-passwords`. The restored logins then have no password, and every
app fails to connect until you run `reissue-db-passwords` after the restore
([below](#a-restore-without-the-logins-passwords)). On a Postgres where you are a superuser, leave
the flag off and the logins keep their passwords.

### Restore

1. Install the release the backup was taken on, as in [Install on Kubernetes](/self-host/kubernetes/),
   with the same Secret values. Upgrade only after the restore. Both databases start empty, at the
   same addresses as before, with the admin passwords in that Secret: each app's `DATABASE_URL`
   names the apps' Postgres by its address.
2. When `canyon-server` is running, restore the keys and the code into its volume, stop it, restore
   both databases, and start it again:

   ```sh
   kubectl -n canyon exec -i deploy/canyon-server -- tar -x -C /var/lib/canyon < backup/files.tar
   kubectl -n canyon scale deploy/canyon-server --replicas=0
   kubectl -n canyon wait --for=delete pod -l app.kubernetes.io/name=canyon-server
   pg_restore --clean --if-exists -d "$CANYON_DATABASE_URL" backup/canyon.dump
   psql -q -o /dev/null -d "$CANYON_APP_DATABASE_URL" -f backup/apps.sql
   kubectl -n canyon scale deploy/canyon-server --replicas=1
   kubectl -n canyon rollout status deploy/canyon-server
   ```

`psql` reports what the new server already has. On the test cluster that was the admin login and its
database:

```text
psql:backup/apps.sql:16: ERROR:  role "canyon_admin" already exists
psql:backup/apps.sql:98: ERROR:  database "canyon_admin" already exists
```

After the restore, the server started, `canyon secret list` showed the app's secret, `canyon pull`
returned the app's code, and a login made in the apps' Postgres before the backup read its rows
with its original password.

### A restore without the logins' passwords

If `apps.sql` was dumped with `--no-role-passwords`, as it must be on RDS, give every app's login a
new password once the server runs again:

```sh
kubectl -n canyon exec deploy/canyon-server -- /canyon-server reissue-db-passwords
```

It sets a fresh password on each app's login, stores the app's new `DATABASE_URL`, and lists every
app still running on the old password, with what brings it back. Each app reads its new
`DATABASE_URL` the next time it is deployed. Its data isn't touched. On the laptop demo, after an app
in preview and production lost its password this way, the command printed:

```text wrap
reissue-db-passwords: re-issued 2 app database passwords
These still run on the old password, until each is deployed again:
  restore-check preview runs 87bee7: Deploy again on http://localhost:7777/#/apps/restore-check, or run canyon deploy from its code
  restore-check prod runs 87bee7: once preview runs 87bee7 again, run canyon deploy --env prod from its code
```

`canyon deploy` then reused the app's image, `canyon deploy --env prod` promoted the same version,
and both read the rows written before the backup. This was run on the laptop demo, and hasn't been
run on a cluster or on RDS.

## On one k3s host

[One k3s host](/self-host/k3s/) runs both Postgres servers inside the cluster, in `canyon-infra`, where
nothing outside the cluster reaches them. These commands reach them through `kubectl exec`, so you
need no Postgres client of your own: each database is dumped and restored by its own server's tools,
over its local socket. Run them on the host, or on any machine with its kubeconfig.

:::note[Where these commands run]
Canyon's CI runs the three blocks below exactly as they're written here, on a kind cluster with Calico,
with both Postgres servers made as [step 3](/self-host/k3s/#3-registry-and-postgres) makes them. It
backs up, deletes the server's volume, both databases and the four Secrets, puts the Secrets back,
restores, and checks that the server starts on the restored key, that it can open what it sealed before
the backup, and that an app's secret and code are back. They haven't been run on a k3s host itself.
:::

:::caution[The restore needs the key]
Every secret Canyon keeps is encrypted under the key in `/var/lib/canyon/keys`, which `files.tar`
holds. Restore the databases without it and nothing Canyon sealed can be opened, and nothing recovers
it. Keep `files.tar` with the dumps, and keep the backup as private as the key itself.
:::

### Back up

```sh title="Back up one k3s host"
mkdir -m 700 backup
kubectl -n canyon-infra exec deploy/canyon-postgres -- pg_dump -U canyon -Fc canyon > backup/canyon.dump
kubectl -n canyon-infra exec deploy/app-postgres -- pg_dumpall -p 5433 -U canyon_admin --exclude-database=postgres > backup/apps.sql
kubectl -n canyon exec deploy/canyon-server -c server -- tar -C /var/lib/canyon -c keys repos > backup/files.tar
for s in canyon-infra/canyon-postgres canyon-infra/app-postgres canyon/canyon-server canyon/canyon-egress; do
  kubectl -n "${s%/*}" get secret "${s#*/}" -o go-template='{{range $k, $v := .data}}{{$k}}={{$v | base64decode}}{{"\n"}}{{end}}' > "backup/${s#*/}.env"
done
```

The backup holds:

- `canyon.dump` and `apps.sql`: Canyon's Postgres, and the apps' Postgres with every app's login and
  its password.
- `files.tar`: the key every secret is encrypted under, and the uploaded code of every version.
- One `.env` file per Secret: both databases' passwords, the server's settings with your sign-in
  client, and the egress gateway's token. They're the values of the Secrets, in plain text.

The registry isn't in the backup, and neither are the apps' pods: on a new host, no app runs until it's
deployed again.

### Restore

Restore onto a host you set up with [the k3s guide](/self-host/k3s/), from the release the backup was
taken on, with the backup folder in the directory you run the guide from. Upgrade only after the
restore. Two of the guide's steps change, so that the new install starts with the backup's Secrets.

**In step 3**, in place of its **Make canyon-infra** block, which makes the `canyon-infra` namespace, labels it and
makes the two databases' passwords, run this, then its **Run the registry and both databases** block as written:

```sh title="Recreate the Secrets"
kubectl create namespace canyon-infra
kubectl label namespace canyon-infra pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted
kubectl apply -f $CANYON/deploy/k8s/namespaces.yaml
for s in canyon-infra/canyon-postgres canyon-infra/app-postgres canyon/canyon-server canyon/canyon-egress; do
  kubectl -n "${s%/*}" create secret generic "${s#*/}" --from-env-file="backup/${s#*/}.env"
done
```

Both databases then start with the passwords the backup was taken under, which `apps.sql` sets the
apps' admin login back to.

**In step 5**, leave out the two `kubectl -n canyon create secret generic` commands: those Secrets
exist already. Make `canyon-registry` as the step does.

When the guide's step 8 passes, restore the key and the code into the server's volume, stop the
server, restore both databases, and start it again. Then publish the certificate the restored server
serves the egress gateway's check under, and restart the gateway so it pins it:

```sh title="Restore one k3s host"
kubectl -n canyon exec -i deploy/canyon-server -c server -- tar -x -C /var/lib/canyon < backup/files.tar
kubectl -n canyon scale deploy/canyon-server --replicas=0
kubectl -n canyon wait --for=delete pod -l app.kubernetes.io/name=canyon-server --timeout=5m
kubectl -n canyon-infra exec -i deploy/canyon-postgres -- pg_restore --clean --if-exists -U canyon -d canyon < backup/canyon.dump
kubectl -n canyon-infra exec -i deploy/app-postgres -- psql -q -o /dev/null -p 5433 -U canyon_admin -d postgres < backup/apps.sql
kubectl -n canyon scale deploy/canyon-server --replicas=1
kubectl -n canyon rollout status deploy/canyon-server --timeout=5m
cert=$(kubectl -n canyon exec deploy/canyon-server -c server -- /canyon-server egress-policy-cert)
kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n canyon rollout restart deploy/canyon-egress
kubectl -n canyon rollout status deploy/canyon-egress --timeout=5m
```

`psql` reports the one thing the new install already has, the apps' admin login:

```text
ERROR:  role "canyon_admin" already exists
```

If `files.tar` held no key, the restored server can't open what it sealed. It stops at its first sealed
key, and `kubectl -n canyon rollout status deploy/canyon-server` times out. Its log says why:

```text wrap
open identity/key/QDzPsrDDRZ6oiFCv1p0vay8WvetpxXzdlaSY-M5ac0k: the data key: cipher: message authentication failed
```

## On Docker

These run on the laptop demo and on [one Docker server](/self-host/docker/) alike, from the `canyon`
directory Canyon runs from. On a server, the `.env` there makes every `docker compose` command the
team setup's.

### Back up

```sh
mkdir -m 700 backup
docker compose exec -T postgres pg_dump -U canyon -Fc canyon > backup/canyon.dump
docker compose exec -T app-postgres pg_dumpall -U canyon --exclude-database=canyon > backup/apps.sql
docker compose exec -T server tar -C /var/lib/canyon -c keys repos > backup/files.tar
```

On a server, copy `.env` into the backup too (`cp .env backup/`): it holds both database passwords,
and the backup of the apps' Postgres sets its admin login's password back to the one it had.

### Restore

Restore into a Canyon of the same release with empty volumes, before the server's first start. On a
server, put the backup's `.env` in `canyon/` first, so both databases start with the passwords the
backup was taken under:

```sh
docker compose up -d --wait postgres app-postgres
docker compose exec -T postgres pg_restore -U canyon -d canyon < backup/canyon.dump
docker compose exec -T app-postgres psql -q -o /dev/null -U canyon -d postgres < backup/apps.sql
docker compose run --rm --no-deps -T --entrypoint tar server -x -C /var/lib/canyon < backup/files.tar
docker compose up -d --wait
```

The `psql` line prints one error, which is expected: the new install already made the `canyon` login.

```text
ERROR:  role "canyon" already exists
```

If `apps.sql` was dumped with `--no-role-passwords`, run
`docker compose exec server /canyon-server reissue-db-passwords` once Canyon is up, as
[on Kubernetes](#a-restore-without-the-logins-passwords).

Canyon now knows every app again, but on a new Docker host their containers are gone, so
`canyon status` shows each preview as stopped:

```text
  preview      1deae2   ✗ stopped   canyon logs --env preview says why
```

Each app's owner runs `canyon deploy` again. The app reads the same secrets and the same database
as before the backup. In this run the app prints its `GREETING` secret and the visits it counted
before the backup, and the CLI hides the secret's value:

```text wrap
  Live   http://steady-salmon-3t6.preview.localhost:7777 · answered 200 OK at /
         <GREETING>: 2 visits
```

If the server starts without the key, it makes a new one, finds secrets encrypted under another,
and stops:

```text wrap
CANYON_KEY_FILE was generated at this boot but 7 row(s) are sealed under another key: the key volume is missing
```

Start again from empty volumes, and restore `files.tar` before the server's first start.

## Upgrade

This is the one place the upgrade is written: the install guides link here. Back up first.

While Canyon is down for an upgrade or a restore, apps your platform runs keep letting people in on
their last access list for the access grace, and a new pod of theirs waits for Canyon
([When Canyon can't be reached](/self-host/hand-off/#when-canyon-cant-be-reached)).

### On Docker

Download the newest release from the directory that holds `canyon/`, and start it again. On the laptop
demo:

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-deploy.tar.gz | tar -xz && cd canyon
docker compose --profile demo-apps up -d --wait
```

On [one Docker server](/self-host/docker/), with no sample apps, to v0.3.0 or a later release, unpacking
the download only if it matches the release's checksums:

```sh
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-deploy.tar.gz
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS && tar -xzf canyon-deploy.tar.gz && cd canyon && docker compose up -d --wait
```

For a particular release, put `download/<version>` (such as `download/v0.2.3`) in place of
`latest/download`.

The download replaces `canyon/docker-compose.yml`, `docker-compose.override.yml` and
`deploy/docker/team.yml`. If you added a model key or any other setting to `docker-compose.yml`
itself (under `server` → `environment`), put it back before you start. A server's `.env`, and any
compose file of your own it names, stay as they are.

### On Kubernetes, with your overlay

1. **Point your overlay at the new release.** Download its `canyon-deploy.tar.gz` beside your overlay,
   so `resources` names its `deploy/k8s`, copy its images into your registry (its `images.txt`, the
   scanner images too, as [Install on Kubernetes](/self-host/kubernetes/#canyon-ships) shows), and
   replace their digests in the overlay.
2. **Check that the Secret of Canyon's egress gateway exists.** The egress gateway, `canyon-egress`,
   is the proxy apps' outbound traffic goes through:

   ```sh
   kubectl -n canyon get secret canyon-egress || \
     kubectl -n canyon create secret generic canyon-egress --from-literal=token="$(openssl rand -hex 32)"
   ```

   Without the Secret the gateway does not start, and from the release that routes apps through it on
   Kubernetes neither does the server: it is one replica, replaced by `Recreate`, and reads the token as
   a required Secret, so the old server stops and the new one waits in `CreateContainerConfigError`.
   Canyon is down until the Secret exists, and comes back up once you create it.
3. **Check that your overlay patches the server's `egress[5]`**, the rule in the `canyon-server`
   NetworkPolicy that lets it reach the Kubernetes API server. It needs both the API server's endpoint
   addresses (`kubectl get endpoints kubernetes`) and the `kubernetes` Service's cluster IP
   (`kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}'`), as the install guide's overlay has.
   Without them the server can't check the gateway's token, and the gateway never becomes ready.
4. **Apply the overlay.** Once the new server is up, publish the certificate it made for the gateway's
   check, then wait on the gateway. Its rollout never finishes without it:

   ```sh
   kubectl apply -k <your overlay>
   kubectl -n canyon rollout status deploy/canyon-executor
   kubectl -n canyon rollout status deploy/canyon-server
   cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) &&
     kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
   kubectl -n canyon rollout status deploy/canyon-egress
   ```

The server migrates its schema when it starts. Once the new executor is up, every app Canyon runs
moves to the new `canyon-identity` sidecar, ten environments a minute. Only that container's image
changes, and the app's pod is replaced the way a deploy replaces it. Each app's Activity records it.
It is not a deploy: no review and no new version. Where Canyon signs people in, the app's
oauth2-proxy keeps its image until the app's next deploy, so redeploy those apps to take the
release's oauth2-proxy. An app your platform runs takes the new sidecar with its next release.
Downgrading the executor moves every sidecar back to the older image.

### With the Helm chart

```sh
helm upgrade canyon https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-chart.tgz -n default -f values.yaml -f values.secret.yaml --wait --timeout 15m
```

Upgrading from a release whose gateway ran no check (no `egress-control` in its pods), publish the
certificate first, as the chart's [first install](/self-host/helm/) does: without it the gateway's
rollout never finishes, and `--wait` fails.

### What carries over

The server updates its database when it starts. Apps keep running through the upgrade. In our tests,
an upgrade across six database changes kept an app answering throughout, with its secret and its rows.

Runs carry over too: the reviews, deploys and promotions under way. While servers of both releases
run side by side, each finishes the runs it started, and a kind of work the new release adds waits
for one of its servers. About 30 seconds after the last server of the old release stops, the new one
takes over whatever the old one left queued.

### The upgrade that brings in who approves a blocking finding

v0.1.13 changed [who approves a blocking finding](/org/who-can-do-what/#approving-a-blocking-finding).
The upgrade doesn't carry the old approvals across:

- **Every approval of a blocking finding made before the upgrade stops counting.** Those findings
  read unapproved again on the app's page, in the go-live checks, on the GitHub check and on the
  next deploy. Approvals of findings that don't block are kept while they stay advisory. A deploy
  that was under way when the server stopped reads its approvals again before it deploys, and stops
  if they no longer count.
- **Each one is approved again**, in the dashboard, by someone that rule allows.
- **On a Canyon with one admin, add a security responder first** (Settings › People & sign-in), so
  the admin's own deploys have someone to approve them.
- **An incident rollback** puts an earlier version back in production. If that version's review had
  approvals, they need approving again, in the dashboard, before
  `canyon deploy --env prod --version` promotes it.
- Approvals made while sign-in was off stop counting once it is on.

The server says how many approvals stopped counting when it starts, in one log line beginning
`canyon-server: <n> unexpired approval(s) cover no blocking finding`.

### The upgrade that records each grant's issuer

From v0.2.0, Canyon records which sign-in issuer each person's access was granted under. The issuer
is your identity provider's address, `CANYON_OIDC_ISSUER`; it changes when you move Canyon to
another identity provider or tenant. Access granted before the upgrade counts as your current
issuer's. Canyon records that the first time someone signs in through that issuer, so nobody loses
access.

- **Don't change `CANYON_OIDC_ISSUER` in the same rollout as this upgrade.** Wait until the rollout has
  finished and no server of the old release is running: until then, an old server can still bind a
  grant without its issuer. Then restart the server once, sign in to the dashboard once, and look in
  the log of that restarted server for
  `canyon-server: <n> grant and request subject(s) recorded under <issuer>`. Only then change the
  issuer. If you upgrade and switch in one step, Canyon records the old grants as the new issuer's.
- **After a switch, everyone signs in again.** A Canyon session from the old issuer is refused. A grant
  bound under the old issuer opens the app by its address until that person signs in through the new
  one, which binds it again.

### The upgrade that checks NetworkPolicy before a build

From this release, on Kubernetes, Canyon builds nothing on a cluster its front-door check hasn't seen
enforce NetworkPolicy, whoever runs the apps. A build's network policy keeps it off the node's
credentials and your private network only where the cluster enforces it.

- **What stops:** every deploy, before the build, with "Canyon hasn't seen this cluster enforce
  NetworkPolicy, so it builds nothing here". Promotions and rollbacks build nothing and are not
  affected by this check.
- **Who is affected:** every Canyon on Kubernetes, once. Nothing records which release checked a
  front door saved before the upgrade, so none counts as checked: after the upgrade, **Settings › App
  hosting** shows the front door *not checked on this cluster*, and every deploy fails before the build
  until an admin saves it. Promotions, rollbacks, patches and the apps already running are not affected.
- **What to do:** save the front door once. If the cluster doesn't enforce NetworkPolicy, turn
  enforcement on first ([Strict mode on the AWS VPC CNI](/self-host/kubernetes/#strict-mode-on-the-aws-vpc-cni)),
  or the save refuses under Canyon running the apps, and the note says so under hand-off. With the Helm chart, a
  plain `helm upgrade` saves it again: its bootstrap saves any front door that isn't seen enforcing, and on a
  cluster that doesn't enforce NetworkPolicy it logs Canyon's note and the upgrade still completes.
- **Rolling back:** the release before doesn't check, so a rollback lets builds run again on a cluster
  that doesn't enforce NetworkPolicy. A front door saved while rolled back counts as unchecked once you
  upgrade again: save it once more then.

### The upgrade that keeps apps off the host (Docker)

From this release, on Docker, an app whose calls go straight out (Outbound *Off*) runs behind a network
floor of its own, which keeps it off the machine Canyon runs on and every private address
([What an app reaches](/self-host/configuration/#docker-executor)).

- **What to do: redeploy every app once after upgrading.** The floor comes with an app's next deploy,
  promotion, rollback or patch. Until then, an app deployed before the upgrade keeps running as it did,
  without the floor, and can still reach the host. Canyon doesn't stop it for you. The executor's log names
  each such app once each time it starts: `<app>-<env> runs on canyon-app-<app> with no floor,
  deployed before the floor: redeploy it`.
- **What may stop working:** an app that calls a database or service on your internal network, or on the
  host itself. Name what it needs in `CANYON_APPS_ALLOW` in `.env` before you redeploy.
- **A redeploy that fails** leaves that app down rather than putting the old, unfloored version back.
  Fix it and deploy again.

### The upgrade that keeps database connections encrypted

From v0.9.0, Canyon never lets a connection to Postgres fall back to plaintext. The `sslmode` in each
database URL decides, and before, a URL without one quietly allowed it:

- **No `sslmode`, or `prefer`:** TLS only. Canyon used to try TLS and, if the handshake failed, connect
  without it. Now it stops there. Someone between Canyon and the database could otherwise make it send a
  password in the clear.
- **`allow`** (plaintext first): refused. A person's database session ([Your app's data](/build/your-apps-data/))
  answers that Canyon couldn't reach the database, and Canyon's own connections refuse to start. Each
  names the setting, never the URL.
- **`disable`:** plaintext, as you asked. The Docker install and the Helm chart's bundled Postgres use it,
  each on a network of its own, and need nothing. The server's log says the connection is unencrypted.
- **`require`, `verify-ca`, `verify-full`:** as before.

This covers `CANYON_DATABASE_URL`, Canyon's own database, and `CANYON_APP_DATABASE_URL`, where apps get
their databases and people's sessions log in.

- **What to do before you upgrade:** name `sslmode=verify-full` with `sslrootcert=<the server's CA file>`
  in each URL, or `sslmode=require` at the least. A Postgres without TLS needs `sslmode=disable` written
  out.
- **What may stop working:** a URL with no `sslmode` to a Postgres that doesn't offer TLS. Canyon used to
  connect to it in plaintext without saying so. Now the connection fails until the URL says
  `sslmode=disable`, or the server offers TLS.

### The upgrade that runs the chart's bundled parts as no root (Helm chart)

From this release, the Helm chart runs its bundled parts as no root, under Pod Security `restricted`: the
registry and Caddy as user 65532, both Postgres servers as 70 (their image's own `postgres` user) and Pocket ID as
1000 (the user its image already dropped to).

- **What to do: just before `helm upgrade`, hand the registry's and Caddy's volumes to their new user**, while
  the release before still runs them as root, then upgrade straight away: a push or a new certificate in between
  writes root's files again.

  ```sh
  kubectl -n canyon-infra exec deploy/registry -- chown -R 65532:65532 /var/lib/registry
  kubectl -n canyon-gateway exec deploy/caddy -- chown -R 65532:65532 /data
  helm upgrade …
  ```

  Both Postgres volumes and Pocket ID's need nothing: their images already wrote them as the new users.
- **If its user can't write a volume:** the registry and Caddy don't start. Their check, the pod's `volume-owned`
  initContainer, refuses, so `kubectl get pods` shows `Init:CrashLoopBackOff`, and
  `kubectl -n canyon-infra logs deploy/registry -c volume-owned` says `canyon: user 65532 can't write
  /var/lib/registry (…)`, naming up to three entries, or `/data` for Caddy. It happens when a volume holds root's
  files at the upgrade, or when a fresh volume's driver neither applies `fsGroup` nor makes it writable. Where the
  driver applies `fsGroup` and the files took its write bits, they start. Otherwise allow root in that namespace for
  one pod, chown with the part's own image, so it works without reaching a registry, and restore the level:

  ```sh
  image=$(kubectl -n canyon-infra get deploy registry -o jsonpath='{.spec.template.spec.containers[0].image}')
  kubectl -n canyon-infra scale deploy registry --replicas=0
  kubectl -n canyon-infra wait --for=delete pod -l app=registry --timeout=3m
  kubectl label ns canyon-infra pod-security.kubernetes.io/enforce=baseline --overwrite
  kubectl -n canyon-infra run chown --rm -i --restart=Never --image="$image" --overrides='{"spec":{"containers":[{"name":"chown","image":"'"$image"'","command":["chown","-R","65532:65532","/v"],"volumeMounts":[{"name":"v","mountPath":"/v"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"registry"}}]}}'
  kubectl label ns canyon-infra pod-security.kubernetes.io/enforce=restricted --overwrite
  kubectl -n canyon-infra scale deploy registry --replicas=1
  ```

  For Caddy, the same in `canyon-gateway` with the Deployment `caddy` and the claim `caddy-data`.
- **Rolling back:** the release before runs them as root again, which reads volumes owned by any user, and
  writes root's files into them. **Before you upgrade again, chown again**, as above.
- **On one k3s host set up with [the k3s guide](/self-host/k3s/)**, whose step 3 makes its own registry and
  Postgres servers: the guide now runs them as no root too, the registry as 65532 with the same check and Postgres
  as 70, so `canyon-infra` is `restricted`. Hand the registry's volume to its user and raise the namespace's level,
  then straight away run the new guide's **Run the registry and both databases** block from step 3, and only that
  one:

  ```sh
  kubectl -n canyon-infra exec deploy/registry -- chown -R 65532:65532 /var/lib/registry
  kubectl label namespace canyon-infra --overwrite pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted
  ```

  Never run step 3's **Make canyon-infra** block again on an existing install: its creates fail, and making its
  Secrets again by any other means changes both databases' passwords under them, which the databases don't take.
  The label stops nothing running; the new pods start as no root. To roll back to the guide's manifests from
  before, label the namespace `baseline` first, or Kubernetes refuses their root pods, and chown again before you
  upgrade again.
- **The k3s guide's Caddy, in its step 7,** runs as no root too, as 65532 with the same check, so `canyon-gateway` is
  `restricted` as well. The new guide's **Run the gateway** block brings it there: it labels the namespace, and
  every object it applies is the same as before or takes the change. It reads `$DOMAIN`, `$CADDY_IP` and `$WORK`,
  which a new shell doesn't have, and an empty one would leave Caddy serving no site or reserving no address. So:

  1. Save the new guide's **Run the gateway** block as `run-the-gateway.sh`, with this as its first line, so it
     stops before anything is applied unless all three are set:

     ```sh
     : "${DOMAIN:?}" "${CADDY_IP:?}" "${WORK:?}"
     ```

  2. Read the three back from your install, then, only if they are all set, hand Caddy's volume to its user and run
     the block straight away:

     ```sh
     CADDY_IP=$(kubectl get ipreservations.crd.projectcalico.org canyon-gateway-caddy -o jsonpath='{.spec.reservedCIDRs[0]}')
     DOMAIN=$(kubectl -n canyon-gateway get configmap caddy -o jsonpath='{.data.Caddyfile}' | awk '/^[^ \t{*][^ \t]* \{$/ {print $1; exit}')
     export CADDY_IP=${CADDY_IP%/32} DOMAIN WORK=$(mktemp -d)
     : "${DOMAIN:?}" "${CADDY_IP:?}" "${WORK:?}" &&
       kubectl -n canyon-gateway exec deploy/caddy -- chown -R 65532:65532 /data &&
       bash -eu run-the-gateway.sh
     ```

  It applies the guide's own Caddyfile again: if you changed yours, apply your changes again after it. Rolling back
  is as for `canyon-infra` above.

## Roll back a failed upgrade

A release only adds to the database: it never removes a table or column the release before it
reads. So the previous release runs on the database a new one has updated, and going back one
release is starting the previous one again. Nothing is restored, and nothing written since is lost.

That includes runs. About 30 seconds after the newer servers have stopped, the previous release takes
over the runs they left queued or under way. Work of a kind only the newer release has stays queued
until you upgrade again. v0.2.0 is the first release that hands runs over this way, so it doesn't roll
back to v0.1.13 or earlier: they never take over the runs v0.2.0 left, which would wait until you
upgrade again. A later release that changes which kinds of work Canyon runs rolls back only to v0.2.0
or a later one, and its release notes say so; after rolling one back, a
changed `CANYON_SCAN_CONCURRENCY`, `CANYON_BUILD_CONCURRENCY` or `CANYON_PATCH_CONCURRENCY` takes effect only once you upgrade again.

If your platform runs the apps through a deploy repo
([Your deploy repo](/self-host/hand-off/#your-deploy-repo)), switch delivery back to the webhook
before rolling back to a release from before the deploy repo (v0.2.x): in **Settings › App hosting**,
choose **Canyon calls your webhook**, or send `{"delivery": "webhook"}`. Those releases know nothing of
the deploy repo and can't commit to it. While your platform runs the apps, Canyon takes the webhook only
once one is saved (`409 webhook_required`): save your pipeline's webhook first, or switch Canyon back to
running apps and then choose the webhook. A Canyon that saved only a deploy repo, and never a webhook,
still starts on the older release: the deploy repo keeps its tokens on its own settings.

If your deploy repo is on GitLab with **Pull requests API** set to **GitLab's merge requests**, roll
back only after saving it with **Pull requests API** set to **None** (production **Pushed to the
branch**). A release before Canyon opened GitLab merge requests has no client for them: its server
stops at its first production commit through one, and again each time it restarts. Switching delivery
to the webhook doesn't help, because a remove follows the way the environment's last deploy went.

- Before you save **None**, let production's commits settle: no production release still retrying
  "your deploy repo didn't take". Once **None** is saved, one still being committed is pushed straight
  to production.
- If the new release can't serve, so you can't save the form, change the setting in Canyon's database
  before starting the older release:

  ```sql
  UPDATE platform_repo SET api = 'none', prod_mode = 'push' WHERE api = 'gitlab';
  ```

- A production release whose merge request was still open when you rolled back never goes live on the
  older release. After you upgrade again, deploy it again.
- When you upgrade again, save **GitLab's merge requests** only once every Canyon server runs the new
  release.

On Docker, download the release you upgraded from, from the directory that holds `canyon/`, and start
it again. Put back any setting you added to `docker-compose.yml` first, as for an upgrade. On the
laptop demo:

```sh
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/<that release>/canyon-deploy.tar.gz
curl -fsSLO https://github.com/getcanyon-ai/canyon-releases/releases/download/<that release>/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS && tar -xzf canyon-deploy.tar.gz && cd canyon
docker compose --profile demo-apps up -d --wait
```

On one Docker server, start it with `docker compose up -d --wait` instead, as for an upgrade. Releases
before v0.3.0 list no checksum for `canyon-deploy.tar.gz`, so `sha256sum` has nothing to compare it
with: for one of those, check the download some other way before you unpack it. A server runs v0.3.0 or
later anyway.

The server starts on the database the newer release updated, and apps keep answering, with their rows.

On Kubernetes, apply your overlay as it was before the upgrade: its `resources` pointing at the
previous release, and that release's image digests. Keep the overlay in version control, and the
previous one is a checkout away.

```sh
kubectl apply -k <your overlay, at the release you upgraded from>
kubectl -n canyon rollout status deploy/canyon-server
```

The executor and the server roll back, and the server starts on the updated database.

**When to restore the backup instead.** Going back more than one release isn't covered: a release
may drop a table once the release before it no longer reads it, so a release two back can miss
something it needs. And if the failed upgrade damaged data, rolling back keeps the damage. In both
cases, restore the backup you took before the upgrade, on the release it was taken on, as in
[Restore](#restore).
