---
title: CLI reference
description: Every canyon command and flag, as the CLI's own --help prints them.
---

This page is the CLI's own help, generated from it. CI fails when it differs from what the CLI
prints, so it always matches the CLI built from the same commit.

## canyon

```text
Canyon: review, ship and govern the apps your teams build with AI.

USAGE
  canyon <command> [flags]

SHIP
  check       Review this app's code without deploying it
  deploy      Review, build and run this app in preview (--env prod to promote)
  status      Show what this app runs and what it needs to reach production
  logs        Stream this app's logs from preview or production
  secret      Set the keys, passwords and settings this app reads when it runs

THIS APP
  share       Who can open this app
  rename      Give this app another name
  pull        Download the code an app is running
  db          Query this app's database, and list or end database sessions
  undeploy    Take preview or production offline (data is kept)
  delete      Delete this app and destroy its data

YOUR ORG
  fleet       List every app in your org and what needs attention
  resources   List the databases and services this Canyon can create for apps
  manifest    Show the instructions this Canyon gives coding agents

SETUP
  login       Sign this machine in to a Canyon host
  logout      Revoke this machine's session on a Canyon host
  skill       Teach your coding agent to use Canyon
  demo        Start, stop or take a tour of Canyon's demo on this machine
  version     Show the CLI version and which Canyon it talks to
  feedback    Ask for a feature or report a problem
  telemetry   See or turn off the daily report this Canyon sends
  licence     Show this Canyon's Enterprise licence

FLAGS
  --format <fmt>    auto | human | ai | json | sarif | csv    (default: auto)
  --host <url>      Which Canyon to use (default: $CANYON_HOST, then your saved host)
  --version, -v     Print the version; given alone, before any command

EXIT CODES
  0  success        1  error        2  blocked by the review or by policy
  3  this command doesn't know how a run in Canyon ends or ended: run canyon status

Install   curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh
```

## canyon check

```text
USAGE
  canyon check [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --sarif           Attach a tool's SARIF report to this review; repeatable
```

## canyon deploy

```text
USAGE
  canyon deploy [flags]

FLAGS
  --app             The app id a first deploy writes into canyon.toml
  --create          Make the app canyon.toml names on a host that doesn't have it
  --env             preview (default) or prod. prod promotes what preview runs, with no rebuild
  --force           Upload even if someone deployed a newer version since you last pulled
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --reason          With --version: why, an incident or change reference the audit trail records
  --sarif           Attach a tool's SARIF report to this review; repeatable
  --version         With --env prod: put an earlier version back in production (canyon status lists them)
  --wait            Where a person approves production, wait for their answer
  --yes             Promote without asking (for CI)
```

## canyon status

```text
USAGE
  canyon status [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon logs

```text
USAGE
  canyon logs [flags]

FLAGS
  --app             The app to read, when it is not the one canyon.toml names
  --env             preview | prod
  --follow          Keep printing new lines until Ctrl-C (the default on a terminal)
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --lines           How many of the last lines to print first
```

## canyon secret

```text
USAGE
  canyon secret list          The names this app reads, never the values
  canyon secret set NAME      Store one value, read from a prompt or a pipe
  canyon secret rm NAME       Remove one name from one environment

  Any setting the app reads goes here, not only keys and passwords.
```

## canyon secret list

```text
USAGE
  canyon secret list [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon secret set

```text
USAGE
  canyon secret set NAME [flags]

FLAGS
  --env             preview | prod
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --where           runtime | build
```

## canyon secret rm

```text
USAGE
  canyon secret rm NAME [flags]

FLAGS
  --env             preview | prod
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon share

```text
USAGE
  canyon share [flags]

FLAGS
  --app             The app to share, when it is not the one canyon.toml names
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --level           owner | contributor | read-only (default read-only)
  --remove          Remove a grant: an email, group:<name> or everyone
  --role            An app role to grant with it; repeatable
```

## canyon rename

```text
USAGE
  canyon rename NAME [flags]

FLAGS
  --app             The app to rename, when it is not the one canyon.toml names
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon pull

```text
USAGE
  canyon pull [flags]

FLAGS
  --app             The app to pull, when the directory holds no canyon.toml
  --force           Write over a directory that is not empty (nothing in it is deleted)
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon db

```text
USAGE
  canyon db query SQL           Run one statement against this app's database, as you
  canyon db connect             Listen on this machine for a Postgres client, connected as you
  canyon db sessions            The app's open database sessions
  canyon db end EMAIL|ID        End a person's database sessions, or one by its id
```

## canyon db query

```text
USAGE
  canyon db query SQL [flags]

FLAGS
  --app             The app to query, when it is not the one canyon.toml names
  --env             preview | prod
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --limit           The most rows to return, 1 to 10000
  --reason          Why you are opening production's data, in your words
  --replace         Close your open session of the other mode, and any client connected through it
  --resource        Which database, when the app declares more than one
  --timeout         How long the statement may run, 1s to 5m
  --write           Open a read-write session: the statement may change data
```

## canyon db sessions

```text
USAGE
  canyon db sessions [flags]

FLAGS
  --app             The app whose sessions to list, when it is not the one canyon.toml names
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --verbose         Also show each session's database login and the server relaying it
```

## canyon db end

```text
USAGE
  canyon db end EMAIL|ID [flags]

FLAGS
  --app             The app whose sessions to end, when it is not the one canyon.toml names
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon db connect

```text
USAGE
  canyon db connect [flags]

FLAGS
  --app             The app to connect to, when it is not the one canyon.toml names
  --env             preview | prod
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --new-token       Replace the URL's token with a new one: the old one stops working
  --reason          Why you are opening production's data, in your words
  --replace         Close your open session of the other mode and open this one
  --resource        Which database, when the app declares more than one
  --write           Open a read-write session: statements may change data
```

## canyon undeploy

```text
USAGE
  canyon undeploy [flags]

FLAGS
  --app             The app to take offline, when it is not the one canyon.toml names
  --env             preview | prod
  --format          auto | human | ai | json | sarif | csv
  --hold            For a security responder or an admin: stop production and hold it until one of them lets it run again, in Canyon's dashboard
  --host            Canyon host
  --reason          Why production is held, with --hold
  --yes             Take prod offline without asking, for CI, where nothing can answer
```

## canyon delete

```text
USAGE
  canyon delete [flags]

FLAGS
  --confirm         The app's name, for CI, where nothing can type it
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon fleet

```text
USAGE
  canyon fleet [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
  --never-reviewed  Only apps Canyon has never reviewed
  --unowned         Only the apps nobody owns
```

## canyon resources

```text
USAGE
  canyon resources [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon manifest

```text
USAGE
  canyon manifest [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --harness         claude-code | codex | cursor: print it the way that agent's session hook reads it
  --host            Canyon host
```

## canyon login

```text
USAGE
  canyon login [<url>] [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
```

## canyon logout

```text
USAGE
  canyon logout [<url>] [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
```

## canyon skill

```text
USAGE
  canyon skill install [--harness claude-code|codex|cursor|agents-md|all]      Write the skill and its session hook into the coding agents here
  canyon skill uninstall [--harness claude-code|codex|cursor|agents-md|all]    Remove the skill and its session hook from the coding agents here
  canyon skill --print [--harness claude-code|codex|cursor|agents-md]          The skill text, on stdout
```

## canyon skill install

```text
USAGE
  canyon skill install [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --harness         claude-code | codex | cursor | agents-md | all    (default: the coding agents found here)
  --host            Canyon host
```

## canyon skill uninstall

```text
USAGE
  canyon skill uninstall [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --harness         claude-code | codex | cursor | agents-md | all    (default: claude-code, codex and cursor)
  --host            Canyon host
```

## canyon demo

```text
USAGE
  canyon demo up              Check this machine, then start Canyon and its sample apps at http://localhost:7777
  canyon demo tour            Take the guided tour with your coding agent, a chapter at a time
  canyon demo down            Stop the demo and remove what it started
  canyon demo down --purge    Also remove the demo's images and its files in ~/.canyon/demo
```

## canyon demo tour

```text
USAGE
  canyon demo tour           The chapter you are on: what it shows and what to do
  canyon demo tour plant     Write this chapter's next files into ./canyon-tour
  canyon demo tour next      Go on to the next chapter, once this one is done
  canyon demo tour skip      Go on to the next chapter without doing this one
  canyon demo tour end       Skip to the end: the recap and where to go from here
  canyon demo tour status    Which chapter you are on
  canyon demo tour reset     Start the tour again from chapter 1
```

## canyon version

```text
USAGE
  canyon version [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon feedback

```text
USAGE
  canyon feedback [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```

## canyon telemetry

```text
USAGE
  canyon telemetry show         The report this Canyon sends next, exactly as it is sent
  canyon telemetry off          Stop sending it
  canyon telemetry on           Send it again
```

## canyon licence

```text
USAGE
  canyon licence [flags]

FLAGS
  --format          auto | human | ai | json | sarif | csv
  --host            Canyon host
```
