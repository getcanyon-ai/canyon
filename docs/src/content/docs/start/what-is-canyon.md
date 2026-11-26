---
title: What Canyon is
description: The problem Canyon solves, what happens on a deploy, and where Canyon runs.
---

Your teams started shipping apps with coding agents, and most of those apps are useful. Some also
hold a live key in the browser bundle, an `/admin` route anyone can open, or SQL built from a query
string. You can't ban the apps, and you can't review them all by hand.

Canyon reviews every deploy before it runs. Builders keep the coding agent they like: Claude Code,
Codex, Cursor, or any agent that reads an `AGENTS.md`. On a laptop the agent deploys with the
`canyon` CLI; in the cloud it connects to your Canyon's address and deploys what's pushed to GitHub
([Connect your agent](/start/connect-your-agent/)). The review's findings come back as a file, a line
and a fix, which the agent handles like a compiler error.
Every app gets an owner, a review history and an audit trail.

## What happens on a deploy

```diagram kind=flow title="Who does what on a deploy, from the agent's upload to production"
steps:
  - {who: Agent and CLI, icon: agent, step: 1, what: Uploads the source, more: "Without `.env` files, `node_modules` or `.git`."}
  - {who: "`canyon-server` and the executor", icon: server, tone: canyon, step: 2, what: Run the scanners, more: "gitleaks on the server; Trivy, Opengrep, ESLint and Checkov, and Grype where an admin turns it on, through the executor."}
  - {who: "`canyon-server`", icon: canyon, tone: canyon, step: 3, what: "The AI review and your rules decide", more: "Every finding names a rule, set to Off, Advisory or Blocking."}
  - paths:
      - label: A finding blocks
        tone: blocked
        steps:
          - {who: "`canyon-server`", tone: blocked, what: "Blocked: file, line, fix", more: "Nothing is built, and the version that was running keeps running."}
          - {who: Agent and CLI, what: Uploads the fixed source}
        loop: Back to step 2.
      - label: The review passes
        tone: passed
        steps:
          - {who: Executor, icon: passed, tone: passed, step: 4, what: Builds it and runs it in preview, more: "The agent gets the preview address."}
          - {who: A person, icon: person, tone: person, step: 5, what: Promotes it to production, more: "Or approves the promotion in the dashboard, where the org asks for that."}
          - {who: Executor, icon: server, tone: canyon, step: 6, what: Runs the same image in production, more: "With production's own secrets."}
```

1. **The CLI uploads the app's source.** It leaves out `.env` files, `node_modules` and `.git`.
   The app doesn't need to be a git repository.
2. **Canyon reviews it.** gitleaks looks for secrets, Trivy for known-vulnerable dependencies,
   Opengrep and ESLint for code patterns such as SQL built from user input, and Checkov for
   infrastructure config. An admin can turn on Grype too. When the host has an AI model, an AI
   review reads the code as well; without one, the review says the AI review was skipped. It never
   passes silently.
3. **Every finding names a rule.** Canyon ships 21, and an admin can add their own. An admin sets
   each rule to Off, Advisory or Blocking. A blocking finding stops the deploy: nothing is built,
   and the version that was running keeps running.
4. **A version that passes is built and runs in preview**, at its own address.
5. **Production is a person's decision.** `canyon deploy --env prod` promotes the same image that
   preview runs, with production's own secrets. An org can also require that a person approves each
   promotion in the dashboard.

## Where it runs

Two ways to try Canyon, and three ways to run it. What decides it: is there data you need to keep?
To try it, no: throw it away when you're done. To run it, yes: people sign in through your identity
provider, and you back up what Canyon holds.

| | Where | You need | People sign in | What it keeps | Guide |
|---|---|---|---|---|---|
| **Try it** | Your laptop | Docker. 15 to 20 minutes the first time, one `docker compose up` | Nobody: sign-in is off, on localhost only | Throwaway | [Try it on your laptop](/start/try-it-on-your-laptop/) |
| | One Kubernetes or k3s node (Helm chart) | A node and a domain. `helm install`, then three commands | Through the Pocket ID the chart brings, or your provider | Throwaway: no backups | [Try it on Kubernetes (Helm)](/self-host/helm/) |
| **Run it** | One server, plain Docker | A Linux server with Docker, a domain and an OIDC client | Through your identity provider | You back it up | [One Docker server](/self-host/docker/) |
| | One server, step by step (k3s) | A Linux server, a domain and an OIDC client | Through your identity provider | You back it up | [One k3s host](/self-host/k3s/) |
| | Your Kubernetes cluster: **production** | Your registry, gateway, identity provider and Postgres | Through your identity provider | You back it up | [Install on Kubernetes](/self-host/kubernetes/) |

One server is one point of failure: while it's down, so are Canyon and every app. Read its caveats
first: [on Docker](/self-host/docker/#caveats), where Canyon's executor holds the server's Docker
socket, or [on k3s](/self-host/k3s/#caveats). [Back up, restore and upgrade](/self-host/backup-and-upgrade/)
covers your cluster, one Docker server and [one k3s host](/self-host/backup-and-upgrade/#on-one-k3s-host),
whose two Postgres servers run inside its cluster. For production, EKS with ECR is tested.

If your platform already runs apps, Canyon can still review and build each release, then **hand it
off** to your pipeline instead of running it: [Your platform runs the apps](/self-host/hand-off/).

[How Canyon works](/concepts/how-canyon-works/) draws each part on Docker and on Kubernetes, and what
it may reach. [Choices at a glance](/concepts/modes/) puts every choice an install makes on one page,
with a diagram for each.

## What leaves your machine

Your code goes to your Canyon host: on the demo, that's your own laptop. The host calls an AI model
only if whoever runs it set one up, and then only review requests and the dashboard's setup chat go
to that endpoint.

## What Canyon doesn't do

- Canyon governs what is deployed through Canyon. Closing your org's other deploy paths is still
  your job.
- One Canyon serves one org. It signs everyone in through your own identity provider and is not
  multi-tenant.

**Next:** [Try it on your laptop](/start/try-it-on-your-laptop/): Canyon blocks a SQL injection, then
runs an app of your own. The first start takes 15 to 20 minutes at 25 Mbit/s, longer on a slower
line. If your company already runs Canyon,
[deploy on it](/start/deploy-on-your-companys-canyon/) instead.
