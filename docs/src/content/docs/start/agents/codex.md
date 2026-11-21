---
title: Codex
description: Set Canyon up for Codex with its own skill and session hook, see what it writes, and how Codex learns your org's rules.
---

Codex deploys with the same `canyon` CLI you use, set up once per machine. Install the CLI and sign
it in first, as [Try it on your laptop](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own) or
[Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in) shows.

## Set it up

1. **Start Codex once.** `canyon skill install` finds Codex by its own directory, `~/.codex`, and
   never creates it. Without it, `--harness codex` says so and writes nothing:

   ```text
     ✗ Codex is not set up here: ~/.codex does not exist, and Canyon does not create it
   ```

2. **Install the skill and the hook:**

   ```sh
   canyon skill install
   ```

   ```text
     Looking for coding agents in this environment…

     ✓ Codex          ~/.agents/skills/canyon/SKILL.md          written
     · Canyon's session hook, in ~/.codex/hooks.json under hooks.SessionStart:
         {"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"/Users/you/.local/bin/canyon manifest --harness codex"}]}
       It runs this canyon by its path: if you move it, run canyon skill install again from the new place.
       Codex asks you to trust it the next time it starts; /hooks in Codex shows it any time.
     ✓ Codex          ~/.codex/hooks.json                       written
     · Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md:
         canyon skill install --harness agents-md      appends to ./AGENTS.md

     Your agent can now deploy. Try asking it:
       "deploy this app"
   ```

   On a machine where Claude Code is set up too, the same command writes its files as well. On
   Windows it writes the skill and no hook, and says so: there the agent runs `canyon manifest`
   itself, as the skill asks.

3. **Trust the hook once.** Codex runs a hook only after you trust it, and the next time you start
   it, Codex asks:

   ```text
   Hooks need review
   1 hook is new or changed. Hooks can run outside the sandbox after you trust them.
   › 1. Review hooks
     2. Trust all and continue
     3. Continue without trusting (hooks won't run)
   ```

   Choose **Review hooks** to see it first, or **Trust all and continue**. `/hooks` in Codex shows
   it any time. Until you trust it, and in `codex exec`, which asks nothing, Codex skips the hook,
   and the skill's own instructions ask the agent to run `canyon manifest` itself, so the rules
   still reach it.

## What it writes

- **`~/.agents/skills/canyon/SKILL.md`**: Canyon's instructions as a Codex skill, the same text
  Claude Code gets. Its description says when to use it (deploy, ship, share a link, fix a blocked
  review):

  ```text
  ---
  name: canyon
  description: Ship and re-ship this app through Canyon. Use when the user asks to
    deploy, ship, publish, preview, share, or send someone a link to an app — and
    whenever a Canyon gate has blocked a deploy and needs fixing.
  ---

  <!-- canyon:skill v:1 hash:44d567a26175 -->
  # Canyon
  ```

  `canyon skill install` makes `~/.agents/skills` when it isn't there yet.

- **A `SessionStart` hook in `~/.codex/hooks.json`**, beside any hooks you already have:

  ```json
  {
    "hooks": {
      "SessionStart": [
        {
          "matcher": "startup|resume|clear|compact",
          "hooks": [
            {
              "type": "command",
              "command": "/Users/you/.local/bin/canyon manifest --harness codex"
            }
          ]
        }
      ]
    }
  }
  ```

  It runs the CLI by its full path, quoted when the path holds a space, so it works whatever your
  `PATH` is when Codex starts.

Running `canyon skill install` again reports both files `unchanged`, or rewrites them for a newer
CLI. To undo it, run `canyon skill uninstall --harness codex`: it removes the skill and Canyon's entry
in `~/.codex/hooks.json`, and leaves the rest of that file as it was.

## How Codex learns your org's rules

At the start of every session, once you've trusted the hook, Codex runs
`canyon manifest --harness codex`, which asks the Canyon you signed in to for your org's current
rules and prints them. Codex puts what it prints into the session. A rule an admin changes reaches
the next session you start.

The hook never stops a session. When the CLI can't reach Canyon, Codex's agent reads this, at most
once a day:

```text
Canyon: can't reach http://localhost:7777 (it may be down or gone). Nothing to do unless the user asks for Canyon. To use it again, start it (the demo: canyon demo up). To remove Canyon from your coding agents: canyon skill uninstall.
```

## Let it reach Canyon

`canyon deploy` uploads your app and waits for the review, over the network. Codex's default sandbox
for a project in version control, `workspace-write`, keeps the network off, so Codex asks you to
approve a command that needs it. To let `canyon` and every other command reach the network without
asking, turn it on in `~/.codex/config.toml`:

```toml
[sandbox_workspace_write]
network_access = true
```

## Add Canyon's MCP server

Canyon's MCP server, `canyon mcp`, gives Codex Canyon as tools, with typed arguments and results,
beside the `canyon` commands it already runs: your org's rules, an app's status and logs, and checking, deploying and promoting it. It runs
as the `canyon` you installed, signed in as it is. One command adds it to `~/.codex/config.toml`:

```sh
codex mcp add canyon -- canyon mcp
```

## With AGENTS.md instead

Codex also reads `AGENTS.md`, from the project's root down to the directory it works in, and from
`~/.codex/AGENTS.md` for every project. For a Codex too old for skills and hooks, or a team that
commits Canyon's instructions with the project, put them there, from the project's root:

```sh
canyon skill install --harness agents-md
```

```text
  Looking for coding agents in this environment…

  ✓ AGENTS.md      ./AGENTS.md                               written

  Your agent can now deploy. Try asking it:
    "deploy this app"
```

It appends Canyon's instructions between two marker lines and leaves the rest of the file as it
was, as [Other agents](/start/agents/other-agents/#agents-that-read-agentsmd) describes. With no
hook, Codex runs `canyon manifest` itself before it builds, as the instructions ask. A project that
carries Canyon's `AGENTS.md` block on a machine that also has the skill gives Codex the same text
twice, which does no harm.

## What differs from Claude Code

- **You trust the hook once.** Claude Code runs its hook from the start; Codex asks you to trust it
  the next time it starts, and runs it from then on.
- **The hook is one shell command.** Claude Code's names the CLI and its arguments apart; Codex's is
  the path and the arguments in one line, the path quoted when it needs to be.
- **The network is yours to allow**, as above.

## Ask it to deploy

This session ran before Canyon gave Codex its own skill and hook: Canyon's instructions were in the
project's `AGENTS.md`, so Codex asked for the org's rules itself. It ran `codex exec`, one run for
each of the person's three messages, in a project holding a small team-lunch poll whose browser
script carried a live payment key, with the network on, against a Canyon shown here as
`https://canyon.example.com`. It took 8 minutes.

The first message:

```text
This is our team-lunch poll app. Deploy it to Canyon exactly as it is, don't change anything yet.
```

Before anything else, Codex ran `canyon manifest`, as `AGENTS.md` asks, and read the org's rules.
Here they are as today's CLI prints them:

```text
Canyon's settings for this org, as canyon manifest prints them. They are in this session already: you need not run canyon manifest.

Canyon host: https://canyon.example.com

Policies that block:
- No secrets in the code
  To pass: Read each secret from an environment variable and store it with canyon secret set NAME.

Policies that only warn:
- No hand-built SQL from user input (blocks production)
  To pass: Pass the user's input to the query as a bound parameter; never build SQL text from it.
  In languages Canyon's scanners cover, it only warns, and blocks production; in the rest only the AI review checks it, if one is set up, and it only warns.
- Every route needs sign-in
- No known-vulnerable dependencies
- No unbounded refetch on user input
- Apps use the org's design system
- No user input reaching commands, files, pages or URLs
- Infrastructure config passes its benchmark checks
- Findings from checkov
- Findings from eslint
- Findings from gitleaks
- Findings from opengrep
- Findings from trivy

Policies for production only:
- Production needs a passing review (blocks)
  To pass: Fix every finding that blocks, then deploy to preview again: production needs a passing review.
- Approved stacks only (only warns)
- Every app has an owner (only warns)

Resources an app can declare in canyon.toml: postgres, s3

Outbound: an app's environment may be under an allowlist, where Canyon refuses calls to any outside service not on it. Only a person on the app's team can allow a new one, in Canyon's dashboard; you cannot. Never try to reach a refused service another way. Canyon also refuses any call out to the internet to an IP address, a one-word host name or a port other than 80 and 443, and no one can allow it: call a host name on 80 or 443.

Production: when the user asks for production, call canyon_promote, setting confirm only when they asked for production in this conversation; where Canyon's tools aren't loaded, run canyon deploy --env prod, adding --yes only then. It promotes; nobody else approves it.
```

Then it ran `canyon check`, which reviews the app without deploying it, and Canyon blocked it on
the key. Codex's answer:

```text
Canyon blocked deployment: it found a Stripe access token in `public/teamstats.js:1`, which could expose payment credentials. Nothing was deployed, and I changed no files.

Revoke/rotate that token before another deploy. To ship unchanged, an admin or security responder must approve the finding on the [Canyon app page](https://canyon.example.com/#/apps/qa-codex-secret-client).

Three nonblocking warnings also flagged missing route sign-in checks and potential path traversal.
```

The second message was `Ok, do what Canyon says.` Codex took the key out of the browser script,
had the app's server read it from `TEAMSTATS_KEY` instead, and ran `canyon check` again, which
passed. It didn't deploy yet:

```text
Fixed the secret exposure: vote tracking now uses a server-side `TEAMSTATS_KEY`. Local checks and Canyon review passed.

Before I deploy, revoke the leaked token and store its replacement using `canyon secret set TEAMSTATS_KEY`, then confirm both are done.

Five nonblocking warnings remain for route identity checks, repeated vote requests, and potential path traversal.
```

The third message gave it a new key, left out here. Codex stored it with
`canyon secret set TEAMSTATS_KEY`, reading the value from standard input, ran `canyon deploy`, and
answered:

```text
Deployed: [Team-lunch poll](https://qa-codex-secret-client.preview.canyon.example.com).

Access is private: only app owners and admins can open it. Canyon passed with five nonblocking warnings about identity checks, repeated votes, and potential path traversal.

The key is stored server-side. Rotate it again because it was pasted into chat.
```

[Fix it or approve it](/build/fix-or-approve/) shows the review's side of that loop.

