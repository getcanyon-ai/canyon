---
title: Take the tour
description: One prompt for your coding agent starts Canyon's guided tour on the laptop demo. Three chapters, three kinds of rule, from a blocked deploy to a working app in production.
---

The tour is one prompt. You paste it into your coding agent, and the agent walks you through Canyon
on the [laptop demo](/start/try-it-on-your-laptop/), a chapter at a time. You play two parts: for a
moment the platform team, who sets a rule, and then a builder, who asks an agent for an app. Canyon
blocks what the agent ships, says why, and the agent fixes it. Nothing is simulated: every step is a
real deploy on your Canyon, and at the end you have a working app in production.

| | |
|---|---|
| **Time** | Minutes, once the demo is up |
| **You need** | Claude Code, Codex, Cursor, Gemini CLI or another coding agent, and what the [laptop demo](/start/try-it-on-your-laptop/#what-you-need) needs: Docker 28 or newer, 4 CPUs and 6 GB of memory for it, about 18 GB of free disk (30 GB with Docker's containerd image store) |
| **You get** | A claims tracker in your org's design system, with a safe search, running in preview and in production |

Canyon checks an app at three stages: when a coding agent deploys, on every pull request, and before
production. The tour's three chapters show three kinds of rule: your company's standards, security,
and what production needs.

```diagram kind=compare title="The tour: you set a rule, Canyon blocks the agent twice, the agent fixes both, and the app goes to production"
columns:
  - label: "Chapter 1: Standards"
    sub: Your agent ignores the design system.
    steps:
      - {who: You, icon: person, tone: person, what: Make the design system mandatory, more: In the dashboard. Only a person changes a policy.}
      - {who: Your coding agent, icon: agent, what: Deploys a page of its own making}
      - {who: Canyon, icon: blocked, tone: blocked, what: "Blocked: nothing is built", more: "The agent gets the file, the line and the fix."}
      - {who: Your coding agent, icon: passed, tone: passed, what: Rebuilds it in the design system, more: "Passed, and live in preview."}
  - label: "Chapter 2: Security"
    sub: Your agent writes a flaw.
    steps:
      - {who: You, icon: person, tone: person, what: Ask for a search by policy number}
      - {who: Your coding agent, icon: agent, what: Builds the SQL from the request}
      - {who: Canyon, icon: blocked, tone: blocked, what: "Blocked: nothing is built", more: "The finding names the line."}
      - {who: Your coding agent, icon: passed, tone: passed, what: Changes one line, more: "Passed, and live in preview."}
  - label: "Chapter 3: Production"
    sub: The company's rules decide.
    steps:
      - {who: You, icon: person, tone: person, what: Ask for production}
      - {who: Canyon, icon: canyon, tone: canyon, what: Shows what production needs}
      - {who: Your coding agent, icon: passed, tone: passed, what: Promotes the version that passed, more: "The same image as preview, with production's own secrets."}
```

## The prompt

Open your agent in an empty folder and paste this. It is the one thing you copy:

```text
Set up Canyon and give me the guided tour.
1. If the canyon command is missing, install it: curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh
2. Ask me to run "canyon demo up" in a terminal of my own, and wait until I say Canyon is ready. A first start takes a while, so do not run it yourself.
3. Run "canyon demo tour" and follow it. One chapter at a time; ask me before each.
```

**You start the demo, once, when the agent asks.** A first start takes longer than a coding agent
waits for a command, so this one is yours, in a terminal of your own:

```sh
canyon demo up
```

`canyon demo up` checks your machine, then starts the demo and names each stage as it goes. A first
start takes 20 to 30 minutes at 25 Mbit/s, longer on a slower line, and a restart 5 to 10 minutes.
It ends on these two lines, and then you tell the agent Canyon is ready:

```text
  ✓ Canyon is ready at http://localhost:7777

  Next, take the guided tour: canyon demo tour
```

The tour's words ship in the CLI, so the prompt stays this short: `canyon demo tour` prints the chapter
you're on as instructions for the agent, and the agent follows them. Its first step is
`canyon skill install`, which gives your agent Canyon's skill, its session hook and Canyon's tools over
MCP. The agent then reviews, deploys and promotes with those tools: `canyon_check`, `canyon_deploy` and
`canyon_promote`. Where your agent loads tools only when it starts, it uses the same commands in the
shell until its next start; the tour says so. It reports every step in the
same four lines: what happened, how Canyon helped, what would have happened without Canyon, and
what that means at scale.

**In Cursor and other agents,** have the agent run `canyon demo tour --format ai`. The CLI recognises
only Claude Code and Codex, so anywhere else `canyon demo tour` prints the chapter for a person reading
along, with the commands to walk it by hand. An agent with Canyon's tools can also read a chapter as
the MCP prompt `canyon_tour`.

## The three chapters

1. **Standards: your agent ignores the design system.** You open the dashboard and make your org's
   design system mandatory, with one click. Then you ask the agent for a claims tracker. It ships a page of its own
   making, Canyon blocks it, and the agent rebuilds the page in the design system.
2. **Security: your agent writes a flaw.** You ask for a search by policy number. The agent builds
   the SQL from the request, Canyon blocks it, and one changed line passes.
3. **Production: the company's rules decide.** You ask for production. Canyon shows what production
   needs, and promotes the version that passed review.

You decide how much of it you take:

| You say | The agent runs | What happens |
|---|---|---|
| "the short one", when the tour opens | `canyon demo tour skip` | Security only, with no dashboard step |
| "skip", at any time | `canyon demo tour skip` | On to the next chapter. It starts from the app the skipped chapter would have left |
| "end", at any time | `canyon demo tour end` | The recap and the ways on |

## What happens

These are Canyon's own outputs from a run of the tour, as your agent reads them from Canyon's tools or
the CLI, which say the same, and we check them against a fresh run on the demo. `⋯` marks lines left out. Your app gets a name of its
own where this run's is `fluffy-raven-dq7`.

**You set a rule.** Chapter 1 starts in the dashboard, because only a person changes a policy. The
agent can't, and the tour tells it so. The demo already has **Canyon** as its design system; the
agent gives you one link, `http://localhost:7777/#/guardrails/policies?rule=design.org-system`,
which opens *Policies* at the marked row. Set **Apps use the org's design system** to **Blocking**
and press **Save** in the bar at the bottom (not *Save, tightening in 14 days*, which blocks nothing
today). From that moment Canyon hands both to every agent that asks:

```text wrap
- Apps use the org's design system
⋯
Design system: Canyon (@canyon/ui)
```

**The agent builds, and the first deploy is blocked.** You ask for a claims tracker.
`canyon demo tour plant` writes the app, the same one for every visitor, so the chapter blocks the same
way on every machine:

```text wrap
  ✓ Wrote 4 files into ./canyon-tour
    canyon.toml
    package.json
    public/index.html
    server.js
  PLANTED: the tour wrote this app, so every visitor starts from the same code. It is the quick version a coding agent often writes: public/index.html has colours, fonts and a list of its own making, and nothing from the org's design system.
```

Deployed unchanged, it stops before anything is built:

```text wrap
CANYON REVIEW — RESULT: BLOCKED
⋯
outcome: nothing was built and nothing was deployed.
⋯
   Canyon blocked this deploy:
   - The app doesn't use Canyon, the org's design system (package.json:11). Suggested fix: Add "@canyon": "https://canyon.example/registry/canyon/{name}.json" to registries in the UI's components.json, then add components with npx shadcn@latest add @canyon/<component>.
⋯
CANYON REVIEW — RESULT: BLOCKED
blocking: design.org-system · package.json:11
exit code: 2
```

**The agent rebuilds the page in the design system.** In a project of your own it fetches the
components from your registry. The demo's registry isn't online, so the tour hands them over, and
says so:

```text wrap
  ✓ Wrote 3 files into ./canyon-tour
    components.json
    public/canyon.css
    public/index.html
  The tour wrote the page again, in the org's design system: components.json names its registry, and public/canyon.css is its theme. In a real project the shadcn CLI fetches these from the registry; this demo's registry is not online, so the tour hands them over.
```

`canyon_check` runs the same review without deploying, and the second deploy is live in preview:

```text wrap
CANYON REVIEW — RESULT: PASSED
⋯
live url: http://fluffy-raven-dq7.preview.localhost:7777
⋯
exit code: 0
```

**The agent adds a search, and Canyon blocks it.** In chapter 2 you ask for a search by policy
number. The tour plants the quick version, and says what it planted:

```text wrap
  PLANTED: server.js now has a search that builds its SQL from the request. It is the quick version a coding agent often writes, put here on purpose so you can watch Canyon stop it.
```

```text wrap
CANYON REVIEW — RESULT: BLOCKED
⋯
   Canyon blocked this deploy:
   - Improper neutralization of special elements used in an SQL command ('SQL Injection') (server.js:22). Suggested fix: Pass the user's input to the query as a bound parameter instead of building the SQL text from it.
⋯
CANYON REVIEW — RESULT: BLOCKED
blocking: sql.string-concat · server.js:22
exit code: 2
```

Preview still runs the version from chapter 1: a blocked deploy replaces nothing.

**The agent fixes the line.** It asks whether you want to fix it yourself. First it reads your org's
[playbook](/org/playbooks/) on how its claims apps are written, which the demo comes with, and which
names the same fix. Either way it's one line, the policy number as a bound parameter:

```js
const rows = db.prepare("SELECT * FROM claims WHERE policy_number = ?").all(req.query.policy);
```

After `canyon_check` and `canyon_deploy`, open the preview address and search for `MRD-1002`. The
app's `/claims` answers:

```text wrap
[{"policy_number":"MRD-1002","claim":"Windscreen, stone chip","status":"Settled"}]
```

Now search for `' OR '1'='1`, the input that would have returned every claim from the blocked
version. It's a policy number like any other now, and it finds nothing:

```text
[]
```

The demo needs no AI model, so its review runs the scanners only. With
[one set up](/start/try-it-on-your-laptop/#add-an-ai-model), the AI review also reports findings
that don't block, such as the claims list having no sign-in. The agent names them and leaves them.

**You decide about production.** In chapter 3 you ask for production, and the agent promotes the
version that passed review:

```text wrap
CANYON PROMOTE — RESULT: PROMOTED
⋯
exit code: 0
```

Production runs the same image as preview, with production's own secrets. Nothing is rebuilt. On the
demo nothing else stands in the way: sign-in is off, so Canyon counts every request as a person's.
Where sign-in is on, an org can
[have a person approve production](/build/production/#when-your-org-has-a-person-approve-production)
in the dashboard, and the agent can only ask. The fleet address shows both blocked versions, the
ones that passed and the promotion on the app's page, and every step in the audit trail.

**The end** is a recap and four ways on: your own app, your pipeline, your team, and real workloads.
[Connect your agent](/start/connect-your-agent/) connects your other coding agents: with the CLI on
a laptop, as in the tour, or by a team Canyon's address in the cloud. The demo has sign-in off, so it
serves no address for cloud agents.

## Run it again

The tour leaves one thing changed on your Canyon: **Apps use the org's design system** is still
Blocking. *Guardrails*, then *Policies*, sets it back to Advisory.

To take the tour again, forget your place and remove the folder. The next tour gets an app of its
own:

```sh
canyon demo tour reset
rm -rf canyon-tour
```

`canyon demo tour status` says which chapter you're on at any time.

**Next:** [Deploy an app of your own](/start/try-it-on-your-laptop/#deploy-an-app-of-your-own), or
[Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/) when the Canyon isn't the
demo on your laptop.
