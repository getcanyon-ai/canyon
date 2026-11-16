# Provisioner adapter — what a platform team implements

To provision through Crossplane, Terraform or an internal API, a platform team runs an **adapter**: an HTTPS service answering the
two calls below, and optionally a third, `/people` (*People*). It holds the cloud credentials; Canyon holds a bearer token and always initiates, so no route back is needed.

## The calls

`PUT {base}/{project}/{env}/{name}` with body `{"type": "postgres"}` (or `"s3"`) makes the resource exist; `DELETE
{base}/{project}/{env}/{name}` destroys it. The key is the project id, `preview` or `prod`, and the resource's name, each a
lowercase DNS label, checked before anything is sent. One key is always one resource. Both
calls are idempotent: a PUT of a resource that exists answers its outputs again; a DELETE of one already gone answers 204. Every
request carries `Authorization: Bearer <token>`.

| Answer | PUT | DELETE |
|---|---|---|
| `200 {"outputs": {...}, "message"?}` | ready | failed |
| `204` | failed | gone |
| `202 {"message"?}` | pending: the adapter progresses on its own; Canyon asks again later | pending |
| `3xx` | failed, never followed: it would carry the token elsewhere | failed, never followed |
| any other `4xx {"message"?}` | failed, permanently | failed, permanently |
| `5xx`, `408`, `429`, a timeout, a refused or dropped connection | transient: Canyon asks again until its own deadline | transient |

Each call has 30 seconds. An answer over 1 MiB, or a 200 or 202 that is not JSON, is failed. A
`message` is shown to people: control characters dropped, the first 1024 characters kept.

## Outputs

A ready answer carries the type's promised outputs, which become the app's environment. Canyon
keeps only those names and drops every other key, so an adapter cannot set `NODE_OPTIONS`,
`LD_PRELOAD` or anything else in the app. A promised output that is missing, empty or over 64 KiB
makes the answer failed.

| Type | Outputs | Served by |
|---|---|---|
| `postgres` | `DATABASE_URL`: a connection URL, `postgres://user:password@host:5432/database`; verify-full with `sslrootcert` on a host that names the database's CA (below) | the built-in driver or an adapter |
| `s3` | `STORAGE_URL`: the bucket and its credential in one URL, `https://ACCESS_KEY_ID:SECRET_ACCESS_KEY@endpoint[:port]/bucket?region=REGION` | an adapter only |

On Kubernetes an app reaches a database only on port 5433, inside the ranges its host allows,
so a `DATABASE_URL` answered there names port 5433. On a
host whose `CANYON_APP_DATABASE_CA_FILE` names the apps database's CA bundle, the built-in driver's
`DATABASE_URL` verifies the server: its query carries `sslmode=verify-full&sslrootcert=/etc/canyon-db/ca.pem`,
the bundle's path in every app container, in place of the admin URL's own two, and keeps the rest.
node-postgres, libpq (psycopg) and pgx read both from the URL. The URL's host is the admin URL's, so
it is the name on the server's certificate — for RDS, the instance's endpoint. The URL is made when
the resource is provisioned: one provisioned before the setting keeps the URL it was given.

`STORAGE_URL` addresses one bucket path-style: its scheme and host are the S3 endpoint (`http` for
a local store), the first path segment is the bucket, `region` is the signing region, and the
access key and secret are percent-encoded in the user part. The credential is scoped to that
bucket. One URL, not five variables, so a rotation replaces the key and the secret together. An
adapter answers, for example:

```json
{"outputs": {"STORAGE_URL": "https://app-key-id:app-secret@s3.eu-central-1.amazonaws.com/claims-portal-files?region=eu-central-1"}}
```

### After a restore: the built-in driver's passwords again

A database restored without its role passwords — `pg_dumpall --no-role-passwords`, and every dump of a
managed Postgres such as RDS, which cannot read them — leaves each app's login with none, while
Canyon keeps the `DATABASE_URL` it issued: a redeploy reuses it and the app fails to sign in. The way
back is one command, run beside the server with the server's environment, as `rotate-kek` is:

```
canyon-server reissue-db-passwords
```

It re-provisions, through the built-in driver, every resource whose record is `ready` and made by
`builtin`, in every app and environment: the login and database are kept, or made, the login gets
a fresh password, and the app's provisioned `DATABASE_URL` is replaced, its row's `version` moved on,
with one `reissue resource password` audit row per resource, actor Canyon's provisioner,
`detail {name, type, env}`. A record a delete
took while it ran is never made again nor sealed for: each is read again before it is re-provisioned and held
while its URL is sealed. It prints, however it ends, `reissue-db-passwords: re-issued <n> app
database password` — `passwords` for any count but one, `0 app database passwords` too — then every environment it re-issued for that runs now — still on the old
password until it is deployed again, as a secret change reaches an app — a line each with the version it runs and what brings it back: preview,
*Deploy again* on the app's page or `canyon deploy` from the app's code (the same code reuses its
image, nothing is rebuilt); production, `canyon deploy --env prod` once preview runs that version
again. Under hand-off Canyon provisions nothing and a release carries names, never a URL, so an
environment the org's platform runs has its database from the platform; a database Canyon made there before the
switch is re-issued like any, and reaches the app only as the platform passes it on. A `DATABASE_URL` set by hand stops it, naming it. It refuses to start, naming the setting,
without `CANYON_APP_DATABASE_URL`, with the key file missing, or while a migration this binary carries is
not applied; an adapter's resources are its platform's, never asked. The data is never touched: a
password is all a lost URL costs.

### Adding a resource type

A change to Canyon: one entry in its table of outputs, the type's Score name and the env names it
promises. Any adapter then serves it with no change to this contract; the built-in driver serves
postgres only. The component menu's `gives` is that entry, so the rest is prose: the type's
notes in the component menu, its name in the snapshot's `component_menu`, and this table.

## People

A person reaches an app's postgres database as themselves through a **database session**: Canyon
runs one statement for them (`canyon db query`) or relays their client's connection (`canyon db
connect`), logged in as their own role. The relay, the query and the database session's lifecycle
consume one boundary of the driver that made the database, and nothing else:

```go
// provisioners: one login for one person on one key, for one connection.
Issue(ctx, key Key, person Person, mode Mode, ttl time.Duration, connect func(context.Context, Login) error) error
Revoke(ctx, key Key, person Person) error // no new login; every backend of the person ended
Activity(ctx, key Key, person Person) (Activity, error) // the person's backends: live, and running a statement now
Cancel(ctx, key Key, person Person, pid int) error     // the statement of one backend of the person's cancelled
Terminate(ctx, key Key, person Person, pid int) error  // one backend of the person's ended, and verified gone
People(ctx, key Key) ([]string, error)                 // the subjects of the key's person roles, found by their comments
Forget(ctx, key Key, person Person) error              // the person's role on the key dropped, its backends ended first
ReadOnly(ctx, login Login) error                       // a read login's role can change no data: *ReadOnlyUnsafe naming what it can
NoSecondConnection(ctx, login Login) error             // a read login's role can open no second connection from inside the database: *ReadOnlyUnsafe naming how
type Login struct { Host string; Port int; Database, User string; Secret Secret; TLS TLSMode; Expires time.Time }
type Activity struct { Live, Active int }
type NotAsMade struct { Found string } // what Canyon found, a sentence a person reads
```

**ReadOnly** is the relay's, called inside a read connection's `connect`, before it dials (**added 2026-11-14**):
the client controls a relayed connection's transactions, so only grants enforce read there. It refuses, as
`*ReadOnlyUnsafe{Found, More}`, while the login's role could change data in its database by any path Postgres grants
one — its own, `PUBLIC`'s, or a role's it inherits: a write (`INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `TRIGGER`) on a
table, a view or a foreign table, a column's included; on Postgres 17 or later, `MAINTAIN` on a materialized view, whose
`REFRESH` rewrites its rows; `UPDATE` or `USAGE` on a sequence; `CREATE` on a schema; `CREATE` on the database;
`EXECUTE` on a `SECURITY DEFINER` function, which runs as its owner; `UPDATE` on an existing large object (Postgres 18's
`has_largeobject_privilege`, before it the object's owner and `lomacl`, every one while `lo_compat_privileges` is on);
each outside the system schemas, and any version branch read from `server_version_num`. `Found` names at most five,
sorted (relations, sequences, schemas, the database, functions, large objects, then by schema and name), as
`INSERT on public.ledger`, `MAINTAIN on public.totals`, `USAGE on sequence public.invoice_no`,
`CREATE on schema public`, `CREATE on database c_1a2b`, `EXECUTE on SECURITY DEFINER function public.bump()`,
`UPDATE on large object 4242`; `More` counts the rest. Creating a large object or a temporary table is `PUBLIC`'s by
default and changes nothing that was there, so neither is judged. Any error reading them is the connection's refusal: it fails closed. A query needs no such
check: it runs in a `READ ONLY` transaction Canyon begins and ends around one statement.

**NoSecondConnection** is a read connection's and a read query's, inside `connect` before it dials (**added
2026-11-15**): the role's grants and a `READ ONLY` transaction hold only the connection they are on, and a second one
opened from inside the database server — `dblink`'s, `postgres_fdw`'s — is held by neither, so a reader who knows any
writing role's password could write through it. It refuses, as `*ReadOnlyUnsafe{Found, More}`, while the login's role
can `EXECUTE` any function of the `dblink` extension, its own or `PUBLIC`'s (the default) or a role's it inherits,
named `EXECUTE on dblink function <schema>.<name>(<args>)`; can `EXECUTE` any `SECURITY DEFINER` function outside the
system schemas, whoever owns it and whatever it calls — it runs as its owner, and what that owner may reach, a
`dblink` function or another definer, is not followed — named `EXECUTE on SECURITY DEFINER function
<schema>.<name>(<args>)`, as `ReadOnly` names one, so an app that wants read queries revokes `EXECUTE` from `PUBLIC`
on its definers; or holds `USAGE` on any
foreign-data wrapper, named `USAGE on foreign-data wrapper <name>`; at most five named, sorted, `More` counting the
rest. An error reading them, in a database that answered, is `ErrUnchecked`, as is `ReadOnly`'s: a check Canyon
couldn't judge, which refuses (`503 check_failed`), never a database that didn't answer.

The last five before it are what a database session's checks, its end and the sweep ask of the person's role.
**Activity** is the idle check's and `DELETE …?if_idle=true`'s: `Live` the role's backends in
`pg_stat_activity`, `Active` those whose `state` is `active` — a long report is not idle. **Cancel** is
the relay's stateless cancel: `pg_cancel_backend` of the backend `pid`, only where `pg_stat_activity`
names it the person's role, so a pid of anyone else's cancels nothing and is no error, and no secret key
is needed. **Terminate** ends one connection whose own sign-in failed a check: `pg_terminate_backend` of
the backend `pid`, only where it is the person's role's, then verified gone from `pg_stat_activity` or an
error. **People** and **Forget** are the retired-role sweep: People names the subjects of the key's
person roles, each found by its comment, and Forget drops one person's role, found the same way, its
backends ended first — the server forgets a person only once it has judged, under the person role's
lock, that they have had no database session on the key for 30 days. Terminate and Revoke run on a
context of their own, so a cancelled request still ends the access. The built-in driver answers all five
on its admin login; an adapter's `/people` has no counterpart to them yet.

`Person` is `{subject, email}`, the identity provider's `sub` and the address; `Mode` is `read | write`;
`TLSMode` is the admin URL's mode and CA, or the adapter's, and **never weaker than the app's own
connections**: on a host whose `CANYON_APP_DATABASE_CA_FILE` names the apps database's CA, the built-in
driver's logins verify the server, `verify-full` against that bundle, as the app's `DATABASE_URL` does
(*Outputs*). `Issue` hands the `Login` to `connect`, under the person role's lock, with a context done
10 seconds on; the caller makes its one connection there, and the login opens nothing once `connect`
returns, so no `Login` outlives the call. An error after `connect` made its connection means the caller
closes it. **An open**, which makes no connection, is an `Issue` whose `connect` does nothing: it judges
everything a connection would, `unsafe_schema` and `limits_unsupported` included. `Issue`'s other refusals, each of a database it
reached, are named as well (**added 2026-11-14**): `*NotAsMade{Found}` when a role or a setting is not as
Canyon made it — the person role a member of another role by a grant Canyon can't revoke, the person role holding
an attribute or granted to a role Canyon never gave it, a group role with a login, an attribute or a membership
Canyon never gave it, or the database setting the role of every session that connects to it (`ALTER DATABASE … SET
role`) — `Found` saying which, in the words a refused open, query or connection shows; and `ErrNoDatabase` when the key's
database does not exist. Any other error is a database that did not answer. `Secret` prints and
marshals as `[secret]`, nested or not. **Never the app's `DATABASE_URL`**: nothing falls back to the app's login.

**The built-in driver** issues on Postgres 14 or later; a non-superuser admin sets `temp_file_limit` on 15 or later once granted it, and on any version is served by a limit in effect (below). Per app database it keeps two group roles,
`c_<h>_read` and `c_<h>_write`, `NOLOGIN`, `c_<h>_write` a member of `c_<h>_read`, both holding
`CONNECT`, made by `Ensure` and by the first `Issue` for a key that lacks them (a kept resource never
calls `Ensure`): `read` is `USAGE` on `public` and every schema the app login owns and `SELECT` on their
tables, views and sequences, never `USAGE` on a sequence; `write` adds `INSERT`, `UPDATE` and `DELETE` on
tables and `USAGE` on sequences — rows, never the schema. `ALTER DEFAULT PRIVILEGES FOR ROLE c_<h>` grants
the same on what the app login creates later. Every open verifies `PUBLIC` cannot `CREATE` in `public`,
and refuses `409 unsafe_schema` otherwise. Per person and app database it keeps one person role,
`p_` + the first 12 bytes, in hex, of the SHA-256 of `project/env/name/subject` (as the app login's `c_`
is of `project/env/name`), `NOLOGIN`, `CONNECTION LIMIT` the per-session cap, owning
nothing, commented `canyon person <email> <sub>` — how a retired role is found, never by its name, and
how a pgaudit line names its person. Opening is one `GRANT c_<h>_<mode> TO p_<h>`, made only once
Canyon has judged under the role's lock that no other database session of the person's holds it. Each
connection first sets the role's group membership to its database session's mode — that group granted,
the other revoked — and its limits by `ALTER ROLE … SET`: `statement_timeout`, `lock_timeout`,
`idle_in_transaction_session_timeout` and `temp_file_limit`; a database that refuses any of them refuses
the open or the connection, `409 limits_unsupported`, never a login without them. `temp_file_limit` is a superuser's parameter.
Canyon sets it on the role where the database allows it: a superuser admin, or, on Postgres 15 or later, one a
superuser has run `GRANT SET ON PARAMETER temp_file_limit TO <admin>` for. Where it does not — RDS has no
true superuser to grant it — Canyon reads the limit in effect for the role, `SHOW temp_file_limit` in a
session of its own as the role once its password is set, which an operator sets for the whole server (on
RDS, the instance's parameter group), and accepts it at 1 GB or less, never `-1`. Neither is `409
limits_unsupported`, its message naming both remedies: the grant, or the server-wide setting. Then one password, `ALTER ROLE p_<h> LOGIN PASSWORD '<verifier>' VALID UNTIL '<the
database's now() + 60 s>'`, and once `connect` returns, still under the lock, `ALTER ROLE p_<h>
NOLOGIN`, on a context of its own so a cancelled request still runs it: the role logs in only between an
issue and its login, so a password a person sets on their own role opens nothing, and a NOLOGIN that
fails is logged and fails the issue, the role then logging in only until `VALID UNTIL`;
every issue also checks both groups are as Canyon made them, and refuses a person role with an attribute, a
membership but its mode's group by any grantor, or granted to any role but the admin;
revoking is the groups revoked, every backend of the role terminated, `NOLOGIN`, any grant of the role to
another role revoked — each step under a short `lock_timeout`, run though one before it failed, and run
again while one fails, a grant the admin's `REVOKE` leaves revoked `GRANTED BY` its grantor — then the
backends verified gone and the role verified a member of nothing and granted to none but the admin, by
any grantor, or the revoke fails; on a context of its own so a cancelled request still revokes. A
person role with no database session for 30 days is dropped; `Delete` drops the key's person and group
roles after the database.

**An adapter** issues by the optional `/people` call:

`POST {base}/{project}/{env}/{name}/people` with body `{"person": {"subject", "email"}, "mode":
"read" | "write", "ttl_seconds"}` answers `200 {"login": {"host", "port", "database", "user", "secret",
"sslmode", "expires_at"}}`, a login for that one person, for one connection; `DELETE
{base}/{project}/{env}/{name}/people/{subject}`, the subject path-escaped, answers `204`: no new login for that person, and every
backend of theirs ended. It is how an enterprise's own broker mints the login — RDS IAM authentication
tokens, Vault's database secrets engine, Teleport or Boundary — while Canyon supplies the authorization,
the person's identity and the audit. Canyon refuses an answer whose `expires_at` is past the asked TTL,
whose `sslmode` is not `verify-full`, whose `host` is anything but a host name or an IP address, whose
`database` or `user` is not an identifier, whose `user` is the app's own login (its `DATABASE_URL`'s), or whose
address is outside what the server may reach (on Kubernetes, inside `CANYON_KUBE_DB_CIDRS` on 5433, the
rule a `DATABASE_URL` already follows). The calls
carry the token, the 30 seconds and the answer classes of *The calls*. **Until an adapter implements
`/people`, a database it made is refused `409 driver_unsupported`, and never falls back to the app's
credential.** An adapter that answers `/people` `404`, `405` or `501` does not implement it: `409
driver_unsupported`. Every other answer is read by the classes above, and a failed or transient one, or
an answer Canyon refuses, is `502 database_unreachable` to the person, with one log line naming the
refusal and the field — never the answer's body, which holds the secret.

## Trust

The adapter URL is `https` (plain `http` only on `localhost`, `127.0.0.1` or `::1`, for tests), with no credentials in it; the
token is read from a file. The adapter's certificate is verified against its own CA file when one is set, the system roots
otherwise.

## Choosing the driver

`CANYON_PROVISIONERS` names a JSON file mapping type → environment → driver; the app never chooses.
`ca_file` is optional:

```json
{"postgres": {"preview": {"builtin": true},
              "prod": {"url": "https://provision.internal/canyon",
                       "token_file": "/run/canyon/adapter-token", "ca_file": "/run/canyon/adapter-ca.pem"}}}
```

Unset, the built-in driver serves postgres in both environments; set, a type and environment the
file does not name has no driver.
A deploy of a package declaring a type with no driver in its environment fails before any
resource is touched: `this host does not provision s3 in preview`. `GET /api/v1/component-types` shows a type
`available` only where it has a driver in preview.
A resource's record keeps the name of the driver that made it — `builtin`, or the adapter's URL —
and its delete goes to that driver, whatever the file says about the type today.
The whole file is refused on an unknown field, type or environment, an entry that is both or
neither built-in and adapter, a built-in entry for a type other than postgres, a non-https URL, or an unreadable token file.
