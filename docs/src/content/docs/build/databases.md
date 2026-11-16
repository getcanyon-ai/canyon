---
title: Databases for your app
description: Declare a Postgres database in canyon.toml, and Canyon creates it and hands your app DATABASE_URL.
---

Your app doesn't install a database or carry a connection string. It declares one in
`canyon.toml`, the file in the app's directory that names the app. Canyon creates the database on
the first deploy and gives the app its address, or, where your platform runs apps, your platform
does ([below](#when-your-platform-runs-apps)).

## Declare it

Put a `[resources]` section in `canyon.toml`, below the `description` line. This is an example:

```toml
description = "Log team expenses and send them to finance"

[resources]
db = { type = "postgres" }
```

Each line is `name = { type = "<type>" }` and nothing else. A name is lowercase letters, digits and
dashes, starting with a letter, at most 40 characters. Each type hands the app one environment
variable, so an app declares each type once: two `postgres` lines would both set `DATABASE_URL`, and
Canyon refuses the upload.

Canyon reads the section when the CLI uploads your code, before the review. A type this Canyon
doesn't offer, or offers but can't create yet, is refused there with the reason, not later in the
deploy.

## What this Canyon can create

`canyon resources` lists the types, what each gives your app, and whether this Canyon can create it
today. This is the demo on a laptop:

```text
  postgres
    class   default
    gives   DATABASE_URL
    status  ✓ available
    notes   An isolated database and role per app. Declare it and deploy; do
            not install a database or write a connection string.

  s3
    class   default
    gives   STORAGE_URL
    status  · planned
    notes   An S3-compatible bucket and a scoped credential per app. Declare
            it and deploy; do not create a bucket or write a credential.

  1 of 2 available
```

`planned` means this Canyon has nothing set up to create that type. Where your platform runs apps,
every type reads `available`: your platform provides it. Canyon itself creates only
Postgres. A storage bucket (`s3`) needs your platform team's own adapter, described
[below](#for-platform-teams-your-own-adapter). Until then, an app that declares `s3` is refused at
upload.

## Use DATABASE_URL

Canyon gives the app `DATABASE_URL` in its environment, a Postgres connection URL. Read it from the
environment. This is an example in Node.js:

```js
import pg from "pg";
const pool = new pg.Pool({ connectionString: process.env.DATABASE_URL });
```

Preview and production each get their own database. Promoting to production does not copy preview's
data: production starts with an empty database of its own.

A redeploy keeps the database, its data and the same `DATABASE_URL`.

`canyon secret list` shows `DATABASE_URL` once, with `each env` as its environment, set by Canyon.
You can't set or remove it by hand. If you already set a `DATABASE_URL` secret yourself, the
deploy fails and tells you to remove it with `canyon secret rm DATABASE_URL`.

## The built-in Postgres

The **operator** is whoever installs and runs Canyon. When the operator sets
`CANYON_APP_DATABASE_URL` to a Postgres server, Canyon creates each app's database itself, on that
server. Each app and environment gets a database and a login of its own. No other app's login can
connect to it.

On Kubernetes, when the operator also names the server's certificate authority
(`CANYON_APP_DATABASE_CA_FILE`), `DATABASE_URL` carries `sslmode=verify-full` and the certificate's
path. node-postgres, psycopg and pgx read both from the URL, so your code doesn't change.

The laptop demo runs its own Postgres container for this.

## Undeploy keeps the data, delete does not

`canyon undeploy --env preview` (or `--env prod`) takes one environment offline. Its database, its
data and its secrets are kept, and the next deploy finds them.

`canyon delete` destroys the app's secrets and every database, with their data, immediately. It
lists what goes, and asks you to type the app's name. In CI, pass `--confirm <app name>` instead.
Only the app's owners and admins can delete it.

The app goes offline first. If a database can't be deleted, Canyon keeps the app, its secrets and
that database's data, and running `canyon delete` again picks up where it stopped.

Taking the `[resources]` line out of `canyon.toml` deletes nothing.

## When your platform runs apps

On a Canyon whose apps your organisation's own platform runs ([Your platform runs the
apps](/self-host/hand-off/)), nothing changes in the app. It declares the same `[resources]` in
`canyon.toml` and reads the same `DATABASE_URL` or `STORAGE_URL`. What changes is who makes them:

- Your platform provisions each database and bucket, and builds the variable from its own secret
  store. Canyon creates none and holds no credential for them. An environment where Canyon made the
  database before the switch still lists Canyon's `DATABASE_URL` in `canyon secret list`, and keeps that
  database: the switch moves no data, and your platform's starts empty.
- The release Canyon sends names each resource, its type and the variable it gives. The values'
  formats are the ones above.
- When your platform reports the app running, it can say where each resource is. The app page and
  `canyon status` show that endpoint to the app's team, such as
  `postgres db · preview · postgres://pg.data.example:5432/claims_portal_preview_db`. It names a
  place, never a password.
- Undeploy keeps the data. `canyon delete` asks your platform to delete what it made for the app,
  lists those resources as your platform's before you confirm, and waits until your platform reports
  each environment removed.

If the organisation later switches back to Canyon running apps, a deploy of an app that declares a
type this Canyon can't create fails at its resources step, with the reason.

## For platform teams: your own adapter

To create databases or buckets in your own cloud, through Crossplane, Terraform or an internal API,
run a **provisioner adapter**: a small HTTPS service of your own that answers two calls, a `PUT`
that makes a resource exist and a `DELETE` that destroys it. The adapter holds the cloud
credentials. Canyon holds only a bearer token to call it, and always starts the call. The full
specification is [Provisioner adapter](/reference/contracts/provisioner/).

The operator points `CANYON_PROVISIONERS` at a JSON file that says who creates each type in each
environment: Canyon's built-in Postgres (`"builtin": true`) or an adapter (`url` and the file
holding its token). This is an example:

```json
{
  "postgres": { "preview": { "builtin": true }, "prod": { "url": "https://provisioner.example", "token_file": "/etc/canyon/provisioner-token" } },
  "s3": { "preview": { "url": "https://provisioner.example", "token_file": "/etc/canyon/provisioner-token" } }
}
```

- The built-in Postgres serves `postgres` only.
- An adapter's URL must be `https`, or `http` to the same machine (`localhost`, `127.0.0.1` or
  `::1`).
- A type and environment the file doesn't name can't be created there. With no driver for preview,
  an app that declares the type is refused at upload. With no driver for production only, the
  upload and the preview deploy go ahead, and the promotion to production fails.
- Your app never chooses who creates its database: it declares a type, and the environment it
  deploys to decides.
- The adapter's answer can set only the type's own variable, `DATABASE_URL` or `STORAGE_URL`. Canyon
  drops every other name.

## Look at the data yourself

To read or change your app's data as yourself, without its connection string, see
[Your app's data, as you](/build/your-apps-data/).
