---
title: Canyon documentation
description: Canyon reviews every deploy your coding agents make, and runs the apps that pass.
template: splash
hero:
  tagline: Your coding agents ship apps. Canyon reviews every deploy.
  actions:
    - text: Try it on your laptop
      link: /start/try-it-on-your-laptop/
      icon: right-arrow
    - text: What Canyon is
      link: /start/what-is-canyon/
      variant: minimal
editUrl: false
---

Canyon sits between your coding agents and the apps they ship. An agent deploys with the `canyon`
CLI on a laptop, or by your Canyon's address from the cloud. Canyon reviews the code against your org's rules, builds and runs only what passes, and puts
every app behind your company's sign-in. It runs on one Docker host or on your Kubernetes cluster.

<div class="reader-cards">
<div class="reader-card"><p class="reader-card-title"><a href="/start/deploy-on-your-companys-canyon/">I build apps with a coding agent</a></p><p>Your company runs Canyon: sign in and deploy through it. New to Canyon? <a href="/start/try-it-on-your-laptop/">Try it on your laptop</a> first.</p></div>
<div class="reader-card"><p class="reader-card-title"><a href="/start/what-is-canyon/#where-it-runs">I install and run Canyon</a></p><p>Pick where it runs, then follow that guide: the Helm chart, one Docker server, one k3s host, or your Kubernetes cluster.</p></div>
<div class="reader-card"><p class="reader-card-title"><a href="/org/first-boot/">I administer my org's Canyon</a></p><p>First boot and sign-in, who can do what, and the rules every app must pass.</p></div>
<div class="reader-card"><p class="reader-card-title"><a href="/self-host/hand-off/">My platform runs the apps</a></p><p>Canyon reviews and builds each release, and your own pipeline runs it.</p></div>
</div>

**Coding agents:** start at [`/llms.txt`](/llms.txt). Every page is also available as Markdown: add
`.md` to its address.

## A deploy, from blocked to live

```mermaid title="A deploy's journey: blocked, fixed, passed, live in preview, promoted by a person"
flowchart TD
  agent["Coding agent<br>runs canyon deploy"]
  review["Canyon reviews it<br>scanners, AI review, your rules"]:::canyon
  blocked["Blocked: nothing is built<br>the agent gets a file, a line and a fix"]:::blocked
  preview["Built and live in preview<br>at its own address"]:::passed
  prod["Production<br>the same image, its own secrets"]:::person
  agent --> review
  review -->|"a blocking finding"| blocked
  blocked -->|"the agent fixes the code<br>and deploys again"| agent
  review -->|"passed"| preview
  preview -->|"a person promotes"| prod
```

Here is an excerpt of what the review prints when it blocks, from a real run against the demo:
an app whose search builds its SQL from the request. `⋯` marks lines left out between the parts
shown:

```text
  ✗ Review blocked · 1 blocking finding
    · AI review          off (no AI model set up on this Canyon)
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✗ opengrep           1 blocking finding
    ✓ trivy              clean
  ⋯
  critical  Improper neutralization of special elements used in an SQL command ('SQL Injection')
  server.js:20:27

    20 │   const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all();
       │                           ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  ⋯
  fix   Pass the user's input to the query as a bound parameter instead of
        building the SQL text from it.
  why   Policy "No hand-built SQL from user input"
  ⋯
  Fix it, then check again without deploying:
    canyon check
```

The agent treats it like a compiler error: it fixes the line and deploys again. A person decides
what goes to production.

## Where to go next

- **[How Canyon works](/concepts/how-canyon-works/)**: a picture of every part, each part on Docker
  and on Kubernetes, and what it may reach.
- **[Choices at a glance](/concepts/modes/)**: where Canyon runs, how people sign in, outbound and
  patching, each with a diagram and when to pick it.
