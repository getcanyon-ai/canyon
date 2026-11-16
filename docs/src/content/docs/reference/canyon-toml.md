---
title: canyon.toml
description: Every key and section of canyon.toml, the file at an app's root that names the app, describes it, and declares its resources and roles.
---

`canyon.toml` sits at the app's root and is committed with its code, so everyone who clones the code
deploys the same app. It holds three keys and two sections, and nothing else:

```toml
app = "amber-gecko-w4g"
description = "Log team expenses and send them to finance"

[resources]
db = { type = "postgres" }

[roles]
expense-approver = "Approves expenses over 1,000 EUR"
```

| | What it is | Who writes it |
|---|---|---|
| [`app`](#app) | The app's id on this Canyon | `canyon deploy`, on the first deploy, or `canyon secret set` before it |
| [`description`](#description) | One line saying what the app does | You, or your agent |
| [`owner`](#owner) | Accepted and ignored | Nobody needs to |
| [`[resources]`](#resources) | The databases and other resources the app needs | You |
| [`[roles]`](#roles) | What the app lets a person do | You |

How the app is built isn't in it: Canyon detects that with buildpacks. A `project.toml` beside it is
the buildpacks' own file, not Canyon's: a static site uses one to have nginx serve its files. Canyon
reads it before the build and hands the buildpacks what it read, never the file itself. In the tables
the buildpacks read (`[_]` and `[io.buildpacks]`, or schema 0.1's `[project]` and `[build]`), every key
must be one the schema has, spelled exactly; a table of another tool's, such as `[com.heroku.…]`, is
left alone, unless its name is one of those spelled in another case. Canyon refuses a file that names a
buildpack the host's builder doesn't carry (by `uri`, a registry name, an image or a path): name the
builder's own buildpacks by id, or write an inline script
([Troubleshooting](/reference/troubleshooting/#projecttoml-is-refused)). A `builder` key is accepted
and dropped: the host's builder builds every app.

## app

```toml
app = "amber-gecko-w4g"
```

The app's id. The first `canyon deploy` in a directory with no `canyon.toml`, or one with no `app`
line, makes the app on the Canyon you're signed in to and writes this line at the top of the file
(`canyon secret set` does the same, so a secret can be set before the first deploy):

```text
  No canyon.toml here — setting up this app.
  app: amber-gecko-w4g (named "team-notes" after this folder)

  ✓ Wrote canyon.toml — commit it so your team deploys the same app.
```

- Leave it to the CLI. The other verbs that act on an app, such as `check`, `status`, `share` and
  `logs`, need it, and say so when it's missing.
- An id is lowercase letters, digits and dashes, beginning with a letter, at most 55 characters.
- The id is the app's address and never changes. `canyon rename` changes the name people see, not
  the id.
- An `app` the Canyon you're signed in to doesn't have stops the CLI before it sends anything:
  `<app> isn't on <host>. If it should be, run: canyon deploy --create`.

## description

```toml
description = "Log team expenses and send them to finance"
```

One line of plain text, at most 200 characters, with no line breaks or control characters. It is
the app's summary on the fleet, the app's page and `canyon status`, where every signed-in member
reads it beside other teams' apps. Without one, those read "No description yet".

Canyon reads it each time the code is uploaded, from the CLI or a GitHub push.

## owner

Accepted and ignored, as long as it is a string: anything else stops the CLI with
`owner is a string, as owner = "<text>"`. An app's owners are set in Canyon: whoever makes the app owns it, and owners
add others with `canyon share <email> --level owner` ([Share an app](/build/share/)).

## [resources]

```toml
[resources]
db = { type = "postgres" }
```

Each line declares one resource the app needs, and Canyon, or your platform where it runs the apps,
makes it and hands the app its address ([Databases for your app](/build/databases/)).

- Each line is `name = { type = "<type>" }` and nothing else.
- A name is lowercase letters, digits and dashes, starting with a letter, at most 40 characters.
- The types are the ones `canyon resources` lists for this Canyon: `postgres`, which gives
  `DATABASE_URL`, and `s3`, which gives `STORAGE_URL`, where this Canyon can make it.
- Each type gives the app one variable, so an app declares each type once.
- Taking a line out deletes nothing.

Canyon reads the section when the code is uploaded, before the review, and refuses the upload with
the reason, such as `resource "<name>" has type "<type>", which this host does not offer: the types
are <types>`.

## [roles]

```toml
[roles]
expense-approver = "Approves expenses over 1,000 EUR"
```

Each line is a role the app defines for itself, and one line saying what it lets a person do. An
app's owners give roles to people when they share it, and the app reads a person's roles from the
token Canyon sends it ([Sign-in inside your app](/build/sign-in-in-your-app/#roles)).

- A role's name is lowercase letters, digits and dashes, starting with a letter, at most 40
  characters: `expense-approver`, never `expense_approver`.
- Its line is one line of plain text, at most 120 characters.
- An app has at most 16 roles.
- Canyon refuses to grant a role that no version of the app declares.

## What the CLI refuses

The CLI reads `canyon.toml` with the same TOML parser Canyon does, and stops before it sends
anything when the file isn't what Canyon reads:

- **It isn't TOML**: it names the line, `<directory>/canyon.toml line <n> is not TOML: …`.
- **It holds another key or section**: `<directory>/canyon.toml holds "<key>", which Canyon does not
  read: …`.
  Above its first section it holds `app`, `description` and `owner`; its sections are `[resources]`
  and `[roles]`.
- **It starts with a byte-order mark**: save it as UTF-8 without one.

Canyon also refuses a `canyon.toml` larger than 64 KiB.
