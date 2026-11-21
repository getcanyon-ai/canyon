---
title: Connect your agent
description: Give a coding agent Canyon's tools over MCP, with one command for every agent on the machine or one click for one agent.
---

Canyon gives a coding agent its tools over MCP (the Model Context Protocol): your org's rules, an
app's status and logs, a review, a deploy and a promotion, each with typed arguments and results.
Each tool does what the `canyon` command of the same name does, with the same checks on who you are.
The MCP server is `canyon mcp`: it runs as the `canyon` you installed, signed in as it is, so install
the CLI and run `canyon login` first, as [Try it on your laptop](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own)
or [Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in) shows.

There are two ways to connect an agent:

| | Sets up |
|---|---|
| [The CLI](#the-cli) | Every agent it finds: Canyon's skill, the session hook that hands the agent your org's rules, and the MCP server |
| [One click](#one-click) | The MCP server alone, in one agent |

## The CLI

```sh
canyon skill install
```

It sets up each agent it finds, and prints every file it writes. Here, on a machine with Claude Code
and Codex, in a project with a `.cursor` directory:

```text
  ✓ Claude Code    ~/.claude/skills/canyon/SKILL.md          written
⋯
  ✓ Claude Code    ~/.claude/settings.json                   written
  · Canyon's MCP server, in ~/.claude.json under mcpServers.canyon:
⋯
  ✓ Claude Code    ~/.claude.json                            written
  ✓ Codex          ~/.agents/skills/canyon/SKILL.md          written
⋯
  ✓ Codex          ~/.codex/hooks.json                       written
  · Canyon's MCP server, in ~/.codex/config.toml:
⋯
  ✓ Codex          ~/.codex/config.toml                      written
  ✓ Cursor         ./.cursor/rules/canyon.mdc                written
⋯
  ✓ Cursor         ./.cursor/hooks.json                      written
  · Canyon's MCP server, in ./.cursor/mcp.json under mcpServers.canyon:
      {"type":"stdio","command":"canyon","args":["mcp"]}
  ✓ Cursor         ./.cursor/mcp.json                        written
```

Each file keeps everything that isn't Canyon's. `canyon skill uninstall` removes the skill, the hook
and the server again, and leaves the rest of each file as it was. An agent that loads its MCP servers
only when it starts has Canyon's tools from its next start.

## One click

To add only the MCP server to one agent:

| Your agent | How |
|---|---|
| [Claude Code](/start/use-it-from-your-coding-agent/#add-canyons-mcp-server) | `claude mcp add --scope user canyon -- canyon mcp` |
| [Codex](/start/agents/codex/#add-canyons-mcp-server) | `codex mcp add canyon -- canyon mcp` |
| [Cursor](/start/agents/cursor/#add-canyons-mcp-server-in-one-click) | The *Add Canyon to Cursor* link on its page |
| [GitHub Copilot in VS Code](/start/agents/other-agents/#github-copilot) | The *Add Canyon to VS Code* link on its page |

Each agent's page has the details, and [other agents](/start/agents/other-agents/) covers agents
that read `AGENTS.md`.

## What still needs the CLI

MCP gives your agent Canyon's tools. A few things stay with the `canyon` CLI:

- **Signing in.** The MCP server uses the CLI's own sign-in: `canyon login`.
- **The session hook**, which hands an agent your org's rules when a session starts, runs
  `canyon manifest`.
- **Setting a secret's value.** No tool takes one, so a value never passes through the chat. Set it
  with [`canyon secret set`](/build/secrets/) in your own terminal, or on the app's page in the
  [dashboard](/build/secrets/#in-the-dashboard).
- **The laptop demo**, which runs on your machine: `canyon demo`.
- **CI**, which runs `canyon check` or `canyon deploy` with a token.
