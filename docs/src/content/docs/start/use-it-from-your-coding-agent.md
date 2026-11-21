---
title: Use it from your coding agent
description: Teach your coding agent to deploy with Canyon. This page sets up Claude Code; Cursor, Codex and other agents each have their own.
---

Your coding agent deploys with the same `canyon` CLI you use. You set it up once, then ask for a
deploy in your own words. `canyon skill install` gives the agent Canyon's instructions: when to
deploy, how to read a review, and how to fix what blocks it. Where it puts them depends on the agent:

| Your agent | Run | It writes | Your org's rules reach it |
|---|---|---|---|
| Claude Code (this page) | `canyon skill install` | `~/.claude/skills/canyon/SKILL.md` and a hook in `~/.claude/settings.json`, for every project on your machine | At the start of every session |
| [Cursor](/start/agents/cursor/) | `canyon skill install`, in the project | `.cursor/rules/canyon.mdc` and a hook in `.cursor/hooks.json` | At the start of every chat |
| [Codex](/start/agents/codex/) | `canyon skill install` | `~/.agents/skills/canyon/SKILL.md` and a hook in `~/.codex/hooks.json`, for every project on your machine | At the start of every session, once you trust the hook in Codex |
| [GitHub Copilot, Aider, Gemini CLI and others](/start/agents/other-agents/) | `canyon skill install --harness agents-md`, in the project | `AGENTS.md` | The agent runs `canyon manifest` before it builds |

The instructions are the same text everywhere; only the file and how it gets read change.

The rest of this page sets up Claude Code. Everything below was run against the
[laptop demo](/start/try-it-on-your-laptop/), on a machine where Claude Code had never been set up.

```mermaid title="Your agent deploys with the CLI: it learns your org's rules at session start and reads the review like a compiler's output"
sequenceDiagram
  participant Y as You
  participant A as Claude Code
  participant L as canyon CLI
  participant C as Canyon
  A->>L: At session start: canyon manifest
  L->>C: Ask for your org's rules
  L-->>A: The rules, for this session
  Y->>A: "deploy this app"
  A->>L: canyon deploy
  L->>C: Upload, review, build
  C-->>L: Findings, or a live address
  L-->>A: The result, written for an agent
  A-->>Y: The link, or a fix and a new deploy
```

## Set it up

1. **Install the CLI and sign it in**, as [Try it on your laptop](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own)
   shows, or, on your company's Canyon, as [Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in)
   does.

2. **Start Claude Code once.** `canyon skill install` writes into Claude Code's own directory,
   `~/.claude`, and refuses until it exists:

   ```text
     Looking for coding agents in this environment…

     ✗ No coding agent found: neither ~/.claude, ~/.codex nor ./.cursor exists here. Start Claude Code once, then run this again.
     · Codex, Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md:
         canyon skill install --harness agents-md      appends to ./AGENTS.md
         canyon skill install --harness codex          Codex's own skill and hook, once Codex has made ~/.codex
   ```

3. **Install the skill:**

   ```sh
   canyon skill install
   ```

   ```text
     Looking for coding agents in this environment…

     ✓ Claude Code    ~/.claude/skills/canyon/SKILL.md          written
     · Canyon's session hook, in ~/.claude/settings.json under hooks.SessionStart:
         {"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"/Users/you/.local/bin/canyon","args":["manifest","--harness","claude-code"]}]}
       It runs this canyon by its path: if you move it, run canyon skill install again from the new place.
     ✓ Claude Code    ~/.claude/settings.json                   written
     · Codex, Cursor, GitHub Copilot's agent, and other agents that read AGENTS.md:
         canyon skill install --harness agents-md      appends to ./AGENTS.md
         canyon skill install --harness codex          Codex's own skill and hook, once Codex has made ~/.codex

     Canyon's tools load the next time your agent starts; sign this machine in first with canyon login if you haven't.
     Then try asking it:
       "deploy this app"
   ```

   Where Codex is set up too it sets up [Codex](/start/agents/codex/), and in a project with a
   `.cursor` directory, [Cursor](/start/agents/cursor/). `--harness agents-md` appends the same
   instructions to the project's `AGENTS.md`, for [other agents](/start/agents/other-agents/) that
   read one.

## What it writes

- **`~/.claude/skills/canyon/SKILL.md`**: when to use Canyon (deploy, ship, share a link, fix a
  blocked review) and how to read what `canyon deploy` prints.
- **A `SessionStart` hook in `~/.claude/settings.json`**, global to every Claude Code session on
  your machine. At each start it runs `canyon manifest --harness claude-code`, which hands the agent
  your org's current rules from the Canyon you signed in to, and how to pass each one that blocks. It runs the CLI by its full path, shown
  in the output above.

Running `canyon skill install` again reports both files `unchanged`, or rewrites them for a newer
CLI. To undo it, run `canyon skill uninstall`: it removes the skill and Canyon's entry in
`~/.claude/settings.json`, leaving the rest of that file as it was. It also removes what it wrote for
Codex, and for Cursor in the project you run it from; `--harness claude-code` limits it to Claude
Code.

## Add Canyon's MCP server

Canyon's MCP server, `canyon mcp`, gives Claude Code Canyon as tools, with typed arguments and
results, beside the `canyon` commands it already runs: your org's rules, an app's status and logs, and checking, deploying and promoting it.
It runs as the `canyon` you installed, signed in as it is. `canyon skill install` adds it for you. To add
only the server, for every project on your machine:

```sh
claude mcp add --scope user canyon -- canyon mcp
```

## In the cloud

Claude Code on the web runs on Anthropic's machines, with no `canyon` CLI and none of your files. It
connects to your Canyon's MCP server by address, `https://<canyon>/mcp`, and reviews and deploys
what you've pushed to GitHub. [In the cloud](/start/connect-your-agent/#in-the-cloud) has what that
needs: your Canyon reachable from the internet, Canyon's GitHub App, and your GitHub account linked.

Add Canyon as a **custom connector in claude.ai**, with `https://<canyon>/mcp` as its address:

1. **An admin adds it once for the organisation:** *Organization settings › Connectors*
   (claude.ai/admin-settings/connectors), then *Add › Custom › Web*. On a Team plan only Owners and
   Primary Owners can; on Enterprise, also a custom role with *Manage access to Libraries*.
2. **Each person connects it with their own account** in claude.ai, and Canyon asks them in the
   browser to allow it.

Claude Code's cloud sessions get the organisation's connectors, with that same permission, and need
no network rule for them. Their calls reach Canyon from Anthropic's servers, which is why your
Canyon must be reachable from the internet.

**The connector also shows up in Claude Code on your laptop** when Claude Code is signed in with a
claude.ai subscription. It doesn't with an API key (`ANTHROPIC_API_KEY`), Bedrock, Vertex or a
setup token. It doesn't replace `canyon mcp`: a server you add in Claude Code takes the place of a
connector only when both have the same address, so the agent sees both. On a laptop where
`canyon skill install` set up `canyon mcp`, keep using that one: it deploys what you haven't pushed.
A connector the agent doesn't use folds under *Show unused connectors* in `/mcp`. To turn Canyon's
connector off in Claude Code on that laptop alone, add it to your own `~/.claude/settings.json`,
which cloud sessions don't read:

```json
{"deniedMcpServers": [{"serverUrl": "https://<canyon>/mcp"}]}
```

Don't roll Canyon out with Claude Code's `managed-mcp.json`. Once that file is deployed, Claude Code
loads only the servers it lists and refuses every other one, `canyon mcp` included. A managed file
on a device doesn't reach a cloud session either.

Checked against Anthropic's docs in November 2026:
[custom connectors](https://support.claude.com/en/articles/11175166),
[adding one](https://claude.com/docs/connectors/custom/add-unlisted),
[how connectors reach Claude Code](https://code.claude.com/docs/en/mcp#how-connectors-reach-claude-code),
[cloud network access](https://code.claude.com/docs/en/cloud-environments#network-access)
and [managed MCP](https://code.claude.com/docs/en/managed-mcp).

## Ask it to deploy

In an empty directory, this one message:

```text
Make me a tiny web page for my team that just says hello, and deploy it with Canyon so I can share the link.
```

Claude Code loaded the skill, wrote `index.html`, `canyon.toml` and `project.toml`, and ran
`canyon deploy`. It took 69 seconds and 8 turns. Its answer:

```text
Your hello page is live. Share this link with your team:

**http://funny-walrus-6sf.preview.localhost:7777**

Anyone who can reach this Canyon host can open it, since sign-in is off for previews.

**What I built**, in the app directory:
- `index.html` with a single centered "Hello, team 👋" heading.
- `canyon.toml` describing the app. Canyon added its own app id on the first deploy.
- `project.toml` telling Canyon to serve the folder as a plain static site.

**Review result**: Canyon's review passed with no findings. The host noted it skipped the AI review step because no model is configured on this Canyon instance. The app page in Canyon is at http://localhost:7777/#/apps/funny-walrus-6sf.

If you want it in production later, say so and I'll promote it.
```

When the CLI isn't talking to a terminal, `canyon deploy` prints its result for the agent. Here is
what the agent read:

```text
CANYON REVIEW — RESULT: PASSED

app: funny-walrus-6sf
env: preview
access: anyone who can reach this Canyon (sign-in is off)
wrote: canyon.toml (app funny-walrus-6sf, named "app" after this folder)
host: http://localhost:7777
gate run: gr_dc7930bdbed16838 (2026-10-11T05:48:14Z)
findings: 0 · blocking: 0
live url: http://funny-walrus-6sf.preview.localhost:7777
answered: 200 OK at /
fleet url: http://localhost:7777/#/apps/funny-walrus-6sf

review coverage:
  ai review · skipped (no model configured on this Canyon host)
These are the host's notes on its own review. Mention them to the user.

NEXT STEPS:
1. Report the live url to the user and say who can open it (the access line). Do not share it unless the user asks.
2. To put this in production later: canyon deploy --env prod

CANYON REVIEW — RESULT: PASSED
live url: http://funny-walrus-6sf.preview.localhost:7777
access: anyone who can reach this Canyon (sign-in is off)

exit code: 0
```

When the review blocks, the same block names each finding's file, line and fix, and the agent fixes
the code and deploys again. [Fix it or approve it](/build/fix-or-approve/) shows that loop, and
[Production](/build/production/) what happens when you ask the agent to put the app in production.

**Next:** [Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/): what changes
when the Canyon isn't the demo on your laptop.
