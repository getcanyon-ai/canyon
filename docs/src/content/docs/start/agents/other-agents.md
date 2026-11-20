---
title: Other agents
description: Set Canyon up for GitHub Copilot, Aider, Gemini CLI, or any other coding agent.
---

Any coding agent that can run commands can deploy with Canyon: it runs the same `canyon` CLI you
do. It needs Canyon's instructions where it reads its own, and `canyon` installed and signed in where
it runs, as [Try it on your laptop](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own) or
[Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in) shows.

None of these agents has a session hook Canyon uses, so nothing hands them your org's rules when a
session starts. Canyon's instructions ask for them instead:

```text
Before you build, run `canyon manifest` and follow what it prints, unless its
output is already in this session.
```

The agent runs `canyon manifest` itself, and the Canyon you signed in to answers with your org's
current rules.

## Agents that read AGENTS.md

From the project's root:

```sh
canyon skill install --harness agents-md
```

```text
  Looking for coding agents in this environment…

  ✓ AGENTS.md      ./AGENTS.md                               written

  Your agent can now deploy. Try asking it:
    "deploy this app"
```

It writes Canyon's instructions between two marker lines in the project's `AGENTS.md`, creating the
file if there is none. When the file already exists, they go at its end and everything else in it
stays as it was:

```text
# Our project

Run the tests with make test.
<!-- canyon:skill v:1 hash:44d567a26175 -->
# Canyon

Canyon deploys this app, but runs a review first. The review can block. When it
blocks, it tells you exactly what to fix and how.
```

and the block ends with `<!-- /canyon:skill -->`. Running the command again replaces only what is
between the markers, or reports the file `unchanged`. To undo it, run
`canyon skill uninstall --harness agents-md`: it removes the block, markers included, and the file too
if nothing else is in it. Commit `AGENTS.md` and everyone who works on the project gets the same
instructions.

### GitHub Copilot

Copilot's cloud agent, Copilot CLI and Copilot Chat in VS Code read `AGENTS.md`; the nearest one
to the files Copilot works on wins. Only a Copilot running where `canyon` is installed and signed in
can deploy: Copilot CLI, or Copilot in your editor, on your machine. Copilot's cloud agent works in
its own environment on GitHub, which has neither unless you set it up there.

Copilot in VS Code also takes Canyon's MCP server, `canyon mcp`: your org's rules, and an app's
status and logs, as tools with typed arguments and results, run as the `canyon` you installed and
signed in as it is. With `canyon` on your
`PATH`, open this link and VS Code shows the server to install:

[Add Canyon to VS Code](vscode:mcp/install?%7B%22name%22%3A%22canyon%22%2C%22command%22%3A%22canyon%22%2C%22args%22%3A%5B%22mcp%22%5D%7D)

### Aider

Aider reads `AGENTS.md` when you tell it to:

```sh
aider --read AGENTS.md
```

or once for the project, in `.aider.conf.yml`:

```yaml
read: AGENTS.md
```

### Gemini CLI

Gemini CLI reads `GEMINI.md` unless you name other files. Name `AGENTS.md` beside it in the
project's `.gemini/settings.json`, or in `~/.gemini/settings.json` for every project:

```json
{
  "context": {
    "fileName": ["AGENTS.md", "GEMINI.md"]
  }
}
```

## Any other agent

`canyon skill --print --harness agents-md` prints the same instructions, with nothing above them for
any one agent, and writes no file:

```sh
canyon skill --print --harness agents-md | head -7
```

```text
<!-- canyon:skill v:1 hash:44d567a26175 -->
# Canyon

Canyon deploys this app, but runs a review first. The review can block. When it
blocks, it tells you exactly what to fix and how.

## Deploying
```

Put them wherever your agent reads its instructions, for example
`canyon skill --print --harness agents-md > CONVENTIONS.md`. Run it again after you upgrade
`canyon`, so the agent reads the instructions this CLI ships.

The Codex page shows the same instructions at work in `AGENTS.md`:
[a session from a blocked deploy to a fixed one](/start/agents/codex/#ask-it-to-deploy).
