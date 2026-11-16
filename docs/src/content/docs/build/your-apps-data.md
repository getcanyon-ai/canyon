---
title: Your app's data, as you
description: Query an app's preview database as yourself with canyon db query, or connect your own client with canyon db connect. Read-only unless you ask, and no connection string ever leaves Canyon.
---

When Canyon creates your app's database, you can read it, and change it when you need to, as yourself.
Canyon signs you in to the database as your own role for a short time and records what you open, so
nobody needs the app's connection string and nobody pastes a database password into a chat.

You need to be a contributor or an owner of the app. Viewers see nothing of its data.

This works on the app's preview database. Production data isn't available in this release.
Database sessions are off until an admin turns them on in **Settings › Databases**.

## Run one statement

```sh
canyon db query "select status, count(*) from claims group by 1 order by 1"
```

```
  claims-portal · preview · read-only · as dana@meridian.example

  status    count
  approved  118
  denied    9
  open      41

  3 rows · 12 ms · this database session is recorded as you
```

- **One statement per call.** Values come back as Postgres prints them, with `NULL` as `null`.
- **Read-only unless you add `--write`.** A read-only session can't change a row. With `--write`, the
  statement runs in its own transaction. If it fails, nothing changes.
- **At most 500 rows by default.** `--limit` takes up to 10,000. A cut answer says so and suggests
  narrowing the query.
- **30 seconds by default.** `--timeout` takes 1s to 5m.
- **Formats.** The table above is the default. `--format csv` gives a header and the rows, and a cell a
  spreadsheet would run as a formula gets a leading `'`. `--format json` gives Canyon's answer as it
  came. `--format ai` gives a block for your coding agent.
- **Other flags.** `--app` names an app other than the one in `canyon.toml`. `--resource` picks the
  database when the app declares more than one.

## Connect your own client

`canyon db connect` opens the same session and listens on your machine, so `psql`, DBeaver, an ORM or
a migration tool can connect to it:

```
$ canyon db connect
  claims-portal · preview · read-only · as dana@meridian.example

✓ Listening on 127.0.0.1:54329 · until 18:02, or 30 min idle
  postgres://dana:<local token>@127.0.0.1:54329/claims-portal?sslmode=require&require_auth=scram-sha-256

  This database session is recorded as you. Ctrl-C stops listening.
```

- **Each connection goes through Canyon, as you.** Your client never learns the database's address or
  the app's password. The URL's password is a local token that works only with this command.
- **The URL carries that token.** Paste it into your database client and nowhere else: not a chat, a
  ticket or a file you commit.
- **The token and the port stay the same between runs,** so a saved connection keeps working. If the port
  is taken, Canyon listens on the next one and says so.
- **Your client proves it knows the token without sending it** (SCRAM-SHA-256), over TLS made for this
  run. Use `sslmode=require`. `verify-ca` and `verify-full` fail, because nothing can vouch for a
  certificate made on your machine.
- **`require_auth=scram-sha-256`** stops your client from sending the token to anything else that
  listens on that port while `canyon db connect` isn't running. Use a client that honours it: libpq 16
  or newer (`psql` 16 and later, and the tools built on it), or pgx 5.10 or newer. An older client could
  send the token to another program listening on that port. Older libpq refuses the setting: you can
  drop it from the URL, but the client will then hand its token to anything that asks.
- **The session ends after 30 minutes idle,** when it reaches its time limit, when
  your sign-in expires, or when an owner or an admin ends it. Canyon prints why and stops listening, with
  or without a client connected: it checks every 30 seconds, and ends on the dot at the time limit.
- **When one connection drops, Canyon keeps listening** and says why, so you can reconnect your client:
  - `Canyon is restarting — still listening; reconnect your client.`
  - `A connection failed: Canyon couldn't check the database session. Reconnect your client.`
  - `The connection to Canyon dropped: reconnect your client.`, when the connection stopped answering.
  - `A message was over 1 MiB, Canyon's limit: the connection closed.`
- **Ctrl-C stops listening,** and ends the session if nothing else is using it.

## See and end sessions

```sh
canyon db sessions        # the app's open database sessions: who, where, how, since when
canyon db sessions -v     # with each one's database role
canyon db end dana@meridian.example   # end every open session of one person on the app
canyon db end dbs_0cf24d382accf9ea    # end one session
```

You can end your own sessions. Owners and admins can end anyone's.

## With a coding agent

The Canyon skill tells your agent to run `canyon db query` to look at an app's data. It never reads the
app's `DATABASE_URL` or asks you for a connection string, and adds `--write` only for a statement
you've confirmed. If a client must connect, it knows `canyon db connect`'s URL carries a token, and
pastes it nowhere but that client.
