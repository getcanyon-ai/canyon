---
title: Connect your agent
description: On a laptop, the canyon CLI connects every coding agent with one command. In the cloud, an agent connects to your Canyon's address and you allow it once in the browser.
---

Canyon gives a coding agent its tools over MCP (the Model Context Protocol): your org's rules and
playbooks, an app's status and logs, a review, a deploy and a promotion, each with typed arguments
and results. Each tool checks who you are and what you may do, as the `canyon` command of the same
name does.

How an agent connects depends on where it runs:

| | On a laptop | In the cloud |
|---|---|---|
| Agents | Claude Code, Codex, Cursor and others on your machine | Claude Code on the web, Cursor's cloud agents |
| You set up | The `canyon` CLI: `canyon login`, then `canyon skill install` | Your Canyon's address, `https://<canyon>/mcp`, allowed once in your browser |
| The code it reviews and deploys | The folder you're in, including what you haven't pushed | A pushed branch of a GitHub repository linked to the app |

```diagram kind=compare title="Two ways an agent connects: canyon mcp on your laptop, or your Canyon's address from the cloud"
columns:
  - label: On a laptop
    icon: laptop
    sub: Claude Code, Codex, Cursor and others on your machine
    steps:
      - who: You, once
        what: "`canyon login`, then `canyon skill install`"
        more: Writes Canyon's skill, its session hook and its MCP server into each agent it finds.
      - via: the agent starts it, over stdio
      - who: On your machine
        icon: canyon
        tone: canyon
        what: "`canyon mcp`, inside the CLI"
        more: Signed in as the CLI is.
      - via: Canyon's API, as you
      - who: It reviews and deploys
        icon: passed
        tone: passed
        what: The folder you're in
        more: Including what you haven't pushed.
  - label: In the cloud
    icon: cloud
    sub: Claude Code on the web, Cursor's cloud agents
    steps:
      - who: You, once
        what: "Add `https://<canyon>/mcp` as a connector"
        more: "For Claude Code on the web, a custom connector in claude.ai; for Cursor, a team MCP server."
      - via: the agent's first call
      - who: You, in your browser
        icon: person
        tone: person
        what: Allow \<agent\> to use Canyon as you?
        more: To see, or to check, deploy and promote. It lasts 7 days.
      - via: Canyon's API, as you
      - who: It reviews and deploys
        icon: passed
        tone: passed
        what: A pushed branch on GitHub
        more: Of the app's linked repository, when your GitHub account can read it.
note: "**Either way, the agent can do only what you can.** Each tool does what the `canyon` command of the same name does. It can't approve findings, change settings or sharing, or see secret values."
```

Connect each agent one way, not both: one `canyon` server per agent.

In the dashboard, **Settings › Agents** shows both, with your Canyon's address filled in, right above
the **Sessions** list where you disconnect an agent.

## On a laptop

Install the CLI and sign it in, as [Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in)
shows, then:

```sh
canyon skill install
```

It sets up each agent it finds, and prints every file it writes: Canyon's skill, the session hook
that hands the agent your org's rules, and Canyon's MCP server, `canyon mcp`, which runs inside the
CLI, signed in as it is. Here, on a machine with Claude Code and Codex, in a project with a
`.cursor` directory:

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

To add only the MCP server to one agent instead:

| Your agent | How |
|---|---|
| [Claude Code](/start/use-it-from-your-coding-agent/#add-canyons-mcp-server) | `claude mcp add --scope user canyon -- canyon mcp` |
| [Codex](/start/agents/codex/#add-canyons-mcp-server) | `codex mcp add canyon -- canyon mcp` |
| [Cursor](/start/agents/cursor/#add-canyons-mcp-server-in-one-click) | The *Add Canyon to Cursor* link on its page |
| [GitHub Copilot in VS Code](/start/agents/other-agents/#github-copilot) | The *Add Canyon to VS Code* link on its page |

[Other agents](/start/agents/other-agents/) covers agents that read `AGENTS.md`.

## In the cloud

A cloud agent runs on its vendor's machines, not yours, so it has no `canyon` CLI and none of your
files. It connects to your Canyon's MCP server by address, and reviews and deploys what you've
pushed to GitHub.

### What it needs

- **Your Canyon reachable from the internet, over `https`, with sign-in on.** The agent's vendor
  calls it from their machines. A Canyon with sign-in off, such as the laptop demo, serves no address.
- **Canyon's GitHub App, installed on your repositories** by an admin ([Connect GitHub](/build/github/#before-you-start-your-admin-connects-github)).
- **Your own GitHub account, linked:** at the foot of the dashboard's side menu, choose **Link
  GitHub account** ([Link your GitHub account](/build/github/#link-your-github-account)). Canyon
  reviews and deploys a repository only when your account can read it on GitHub.

### Connect it

1. Add `https://<canyon>/mcp` to the agent as an MCP server, the way its page shows:
   [Claude Code on the web](/start/use-it-from-your-coding-agent/#in-the-cloud) or
   [Cursor's cloud agents](/start/agents/cursor/#in-the-cloud).
2. The first time the agent uses it, Canyon opens in your browser. Sign in if you aren't, and Canyon
   asks **Allow \<agent\> to use Canyon as you?**, with where and when the request came from.
3. It names what the agent asks for:
   - **See your org's rules, and the apps you can see: their status, and logs where you're a
     contributor;**
   - **Check, deploy and promote the apps you contribute to.**

   It can do only what you can. It can't approve findings, change settings or sharing, or see secret
   values.
4. Choose **Allow**. Only allow a connection you started yourself.

The connection lasts 7 days, as a CLI sign-in does; then you allow it again. Under **Settings › Agents**, **Sessions** lists each one as
`<agent> (MCP)`; **Disconnect** ends it at once.

### What the agent can do

| Tool | Does |
|---|---|
| `canyon_rules` | Your org's rules, as `canyon manifest` shows them |
| `canyon_playbook` | One of your org's [playbooks](/org/playbooks/) |
| `canyon_status`, `canyon_logs` | An app's status, and its last log lines with secrets masked |
| `canyon_secret_link` | A link for you to open: the app's Secrets form, filled in with the secret's name and environment, where you type the value. The agent never sees it |
| `canyon_check_ref`, `canyon_deploy_ref` | Reviews a pushed branch, tag or commit of the app's repository, or deploys it to preview |
| `canyon_review` | How a review or deploy ended, when it took longer than one call can wait: after 45 seconds the agent gets the run back, and calls this until the run ends. A deploy that went live says so, with its address |
| `canyon_create_app` | Makes an app from a GitHub repository, with you as its owner, linked to the repository. At most 10 a day for each person |
| `canyon_promote` | Promotes to production, after asking you |

`canyon_rules`, `canyon_playbook`, `canyon_status`, `canyon_logs`, `canyon_secret_link` and
`canyon_review` need only the first kind of access you allow, to see. The rest need the second, to
check, deploy and promote.

**Production's question.** Where the agent can show you a question, `canyon_promote` asks you
there, and promotes only on your yes. An agent that can't show one says you asked for it, and
Canyon has only its word. To have a person decide every time, whatever the agent, turn on
[production approval](/build/production/#when-your-org-has-a-person-approve-production): each
promotion an agent asks for then waits for a person to approve it in the dashboard.

### For admins: one setup for the whole team

Only where the agent's vendor offers it:

- **Claude Code on the web:** add Canyon as a custom connector under *Organization settings ›
  Connectors* in claude.ai: on a Team plan an Owner or Primary Owner, on Enterprise also a custom
  role with *Manage access to Libraries*. Each person connects it with their own account and allows
  it once, as above ([Claude Code's page](/start/use-it-from-your-coding-agent/#in-the-cloud)).
- **Cursor:** add it as a team MCP server under **Dashboard › Plugins & MCPs**. Cursor's cloud agents
  get it, and each person allows it once.

Push the address to cloud agents only. On laptops, `canyon skill install` already sets up
`canyon mcp`. A claude.ai connector also reaches Claude Code on laptops signed in with a claude.ai
subscription, beside `canyon mcp`; [its page](/start/use-it-from-your-coding-agent/#in-the-cloud)
shows how to turn it off there.

## What still needs the CLI

- **Deploying code you haven't pushed.** Only something on your machine can send the folder you're
  in: `canyon deploy`, or `canyon mcp`'s `canyon_deploy`.
- **Signing in on a laptop.** `canyon mcp` uses the CLI's own sign-in, `canyon login`. A cloud agent
  signs in in your browser instead.
- **The session hook**, which hands an agent on your machine your org's rules when a session starts.
  A cloud agent gets them from Canyon's MCP server when it connects.
- **The laptop demo**, which runs on your machine: `canyon demo`.
- **CI**, which runs `canyon check` or `canyon deploy` with a token.

**A secret's value never passes through an agent.** You set it yourself, on the app page's
[Secrets section](/build/secrets/#in-the-dashboard) or with [`canyon secret set`](/build/secrets/)
in your own terminal.

Once connected, either way, an agent is also handed your org's [playbooks](/org/playbooks/): how your
teams build, which it reads when they fit its task.
