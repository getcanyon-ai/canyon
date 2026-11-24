---
title: Deploy on your company's Canyon
description: Your company runs Canyon and you have its address. Sign the CLI in, connect your coding agent, and ship a first app, private until you share it.
---

Your company runs Canyon, and you have its address, such as `https://canyon.yourcompany.example`.
Sign the `canyon` CLI in to it once, and you or your coding agent can deploy through it. Everything
you deploy is private to you until you share it.

## What's different from the laptop demo

| | The laptop demo | Your company's Canyon |
|---|---|---|
| Address | `http://localhost:7777` | Your company's `https` address |
| Signing in | Nobody signs in: sign-in is off | Through your identity provider, in the browser; the CLI signs in with a code you approve there |
| Who opens your app | Anyone who can reach the demo | You and Canyon's admins, until you [share it](/build/share/) |
| An app's address | `<app>.preview.localhost:7777` | Under your company's domain, such as `https://<app>.preview.<domain>` |
| A blocking finding the code is right about | Anyone can approve it | [Someone else approves it](/build/fix-or-approve/#approve-it-instead), never you on your own deploy |
| The AI review | Off: the demo has no AI model | On, if your admin set up a model |
| Production | You promote | You promote, or your org has [a person approve it in the dashboard](/build/production/#when-your-org-has-a-person-approve-production) |

## 1. Install the CLI

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh
```

## 2. Sign in

```sh
canyon login https://canyon.yourcompany.example
```

The CLI signs in with a code you approve in a browser signed in as you:

```mermaid title="CLI sign-in: the terminal shows a code, you approve it in a browser signed in as you"
sequenceDiagram
  participant T as canyon login
  participant C as Canyon
  participant B as You, in a browser
  T->>C: Start a sign-in
  C-->>T: A code and a link, for 10 minutes
  T->>C: Asks until it is approved
  Note over B: Open the link and check the code
  B->>C: Approve, signed in as you
  C-->>T: Signed in as you, for 7 days
```

1. The CLI prints a code and a link, and says how long the code lasts: 10 minutes.
2. Open the link. If you aren't signed in yet, the page says "Sign in to approve the Canyon CLI" and
   sends you to company sign-in first.
3. The page "Sign in to the Canyon CLI" shows the code, the address the request came from, and when.
   Check the code matches your terminal. The page names the account you are approving as, with
   "Switch account" to change it.
4. Click "Approve" or "Deny". The link fills in the code, but only your click approves it. Only a
   browser sign-in can approve or deny a code: a CLI sign-in is refused.

Approving gives that machine your access for 7 days. Anything running there acts as you, including
a coding agent and its deploys. Only approve a code you started yourself. The CLI then says it is
signed in, and names you and your organization. `canyon login --token` is not available: a person
signs in, and every CLI session is that person's.

If the code expires and the CLI says Canyon is still being set up, nobody can approve a sign-in
there yet: ask whoever installed it to finish setup.

## 3. Connect your coding agent

```sh
canyon skill install
```

It sets up Claude Code and Codex, and Cursor in a project that has a `.cursor` directory, and prints
every file it writes:

- **Canyon's skill**, which teaches the agent to deploy with Canyon and fix what the review finds;
- **a session hook**, which hands the agent your org's current rules from the Canyon you signed in
  to at each session start;
- **Canyon's MCP server**, `canyon mcp`, which gives the agent Canyon as tools, signed in as the CLI
  is.

[Connect your agent](/start/connect-your-agent/#on-a-laptop) shows what it writes for each agent.
`canyon skill uninstall` takes it all out again.

**A coding agent in the cloud,** such as Claude Code on the web or Cursor's cloud agents, has no
CLI. It connects to `https://canyon.yourcompany.example/mcp` instead, you allow it once in your
browser, and it deploys what you've pushed to GitHub ([In the cloud](/start/connect-your-agent/#in-the-cloud)).

## 4. Deploy

From the app's directory, ask your agent to deploy it, or run:

```sh
canyon deploy
```

- **The first deploy writes `canyon.toml`** with the app's id. Commit it, so everyone who clones the
  code deploys the same app. Add a one-line `description` to it first, so colleagues see what the app
  does ([`canyon.toml`](/reference/canyon-toml/)).
- **Canyon reviews the code before anything is built.** When the review blocks, the output names the
  file, the line and the fix, and the agent fixes it and deploys again
  ([Fix or approve](/build/fix-or-approve/)).
- **Your app is private:** only you and Canyon's admins can open it until you share it.
  `canyon status` shows who can, on its access line:

  ```text
    access       private · you are an owner
  ```

  Opening the address sends people through sign-in first, and the app learns who they are
  from a token Canyon signs ([Sign-in inside your app](/build/sign-in-in-your-app/)).

## 5. Share it

```sh
canyon share sam@yourcompany.example                  # read-only
canyon share sam@yourcompany.example --level contributor
canyon share everyone                                 # everyone at your org, read-only
```

[Share an app](/build/share/) has the levels, groups and app roles.

## 6. Put it in production

```sh
canyon deploy --env prod
```

It promotes the exact image preview runs, with production's own secrets. Where your org has a person
approve each promotion, it prints a link instead, and someone on the app's team approves it in the
dashboard. See [Get to production](/build/production/).

When something stops you, [Troubleshooting](/reference/troubleshooting/) lists the messages Canyon
prints and what to do about each.
