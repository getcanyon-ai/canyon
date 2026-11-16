---
title: Cursor
description: Set Canyon up in a Cursor project, see the rule and hook it writes, and what Cursor reads at the start of a chat.
---

Cursor's agent deploys with the same `canyon` CLI you use, set up once per project. Install the CLI
and sign it in first, as [Try it on your laptop](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own)
or [Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in) shows.

## Set it up

Canyon writes into the project's `.cursor` directory and never creates it. In a project without
one, `canyon skill install --harness cursor` says so and writes nothing:

```text
  ✗ Cursor is not set up here: ./.cursor does not exist, and Canyon does not create it
```

Make it (`mkdir .cursor`), then, from the project's root:

```sh
canyon skill install
```

```text
  Looking for coding agents in this environment…

  ✓ Cursor         ./.cursor/rules/canyon.mdc                written
  · Canyon's session hook, in ./.cursor/hooks.json under hooks.sessionStart:
      {"command":"canyon manifest --harness cursor"}
  ✓ Cursor         ./.cursor/hooks.json                      written
  · Codex, Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md:
      canyon skill install --harness agents-md      appends to ./AGENTS.md
      canyon skill install --harness codex          Codex's own skill and hook, once Codex has made ~/.codex

  Your agent can now deploy. Try asking it:
    "deploy this app"
```

On a machine where Claude Code or Codex is set up too, the same command writes their files as well,
as [Claude Code's page](/start/use-it-from-your-coding-agent/#set-it-up) and
[Codex's](/start/agents/codex/#set-it-up) show. Cursor also reads `AGENTS.md`, so you don't need the
lines it suggests: one copy of the instructions is enough.

## What it writes

Both files are the project's, so you can commit them and everyone who opens the project in Cursor
gets them.

- **`.cursor/rules/canyon.mdc`**: Canyon's instructions as a Cursor project rule. Its description
  says when to use it (deploy, ship, share a link, fix a blocked review), and `alwaysApply: false`
  lets Cursor's agent pull the rule in when a request matches that description:

  ```text
  ---
  description: Ship and re-ship this app through Canyon. Use when the user asks to
    deploy, ship, publish, preview, share, or send someone a link to an app — and
    whenever a Canyon gate has blocked a deploy and needs fixing.
  alwaysApply: false
  ---

  <!-- canyon:skill v:1 hash:44d567a26175 -->
  # Canyon

  Canyon deploys this app, but runs a review first. The review can block. When it
  blocks, it tells you exactly what to fix and how.
  ```

- **`.cursor/hooks.json`**: a `sessionStart` hook, kept beside any hooks you already have:

  ```json
  {
    "hooks": {
      "sessionStart": [
        {
          "command": "canyon manifest --harness cursor"
        }
      ]
    },
    "version": 1
  }
  ```

  It runs `canyon` by name, not by a path on your machine, so it works for everyone who opens the
  project, as long as `canyon` is on their `PATH`.

Running `canyon skill install` again reports both files `unchanged`, or rewrites them for a newer
CLI. To undo it, run `canyon skill uninstall --harness cursor` from the project's root: it removes
the rule and Canyon's entry in `.cursor/hooks.json`, and leaves the rest of that file as it was.

## How Cursor learns your org's rules

When you start a chat, Cursor runs the hook. `canyon manifest --harness cursor` asks the Canyon you
signed in to for your org's current rules and prints them as `additional_context`, which Cursor adds
to the chat's starting context. A rule an admin changes reaches the next chat you start.

The hook never stops a chat. When the CLI can't reach Canyon, Cursor's agent reads this, at most
once a day:

```text
{"additional_context":"Canyon: can't reach http://localhost:7777 (it may be down or gone). Nothing to do unless the user asks for Canyon. To use it again, start it (the demo: canyon demo up). To remove Canyon from your coding agents: canyon skill uninstall.\n"}
```

Cursor's cloud agents don't run `sessionStart` hooks. There, the rule's own instructions tell the
agent to run `canyon manifest` before it builds.

## What differs from Claude Code

- **Per project, not per machine.** Claude Code's files live in `~/.claude` and reach every project;
  Cursor's live in the project, and each project gets its own.
- **The hook runs `canyon` by name.** Claude Code's runs the CLI by its full path, since that hook is
  yours alone. Cursor's may be committed, so it relies on `canyon` being on `PATH`.
- **The rule loads when it fits.** Claude Code picks its skill by its description; Cursor does the
  same with the rule's description.

Once set up, you ask for a deploy the same way:
[Claude Code's page](/start/use-it-from-your-coding-agent/#ask-it-to-deploy) shows a whole session
and what the agent read.
