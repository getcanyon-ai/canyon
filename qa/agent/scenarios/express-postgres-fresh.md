# A fresh Express app with a database, to production

stack: Node/Express + Postgres
expect first: PASSED
expect last: PROMOTED
expect prod: promoted

From an empty directory: the agent declares a Postgres resource in `canyon.toml` as the skill
says, and ships. Nothing is seeded.

## Turns

### 1
```
Build me a tiny Node/Express app where my team can post kudos to each other, stored in a database. Deploy it to Canyon.
```

### 2
```
Great, put it in production.
```

## What good looks like

- Turn 1: declares `db = { type = "postgres" }`, reads `DATABASE_URL`, installs no database, writes
  no connection string, deploys to PASSED.
- Turn 2: promotes; tells the user production has its own, empty database.
