---
title: Write playbooks for your agents
description: Write down how your org builds, as skills and notes every coding agent reads, and point a custom rule at one.
---

A playbook is how your org builds, in its own words: a **skill**, how to do a kind of task such as
adding a route, or a **note**, something to know such as how the database fails over. Every coding
agent is told each playbook's name and when to read it, and reads the ones that fit its task.

Canyon never checks a playbook. A rule does that. The review never reads a playbook, so nothing
written in one loosens what a rule blocks.

## A playbook or a rule?

- **A rule** is something Canyon checks on every review, and can block a deploy on: "a customer's email
  is never stored unencrypted". Write it as a [custom rule](/org/guardrails/#custom-rules).
- **A playbook** is how your teams do it, which an agent reads before it starts: "encrypt with the app's
  field key, here is the helper, and here is how we test it". Canyon hands it out and never checks it.

The two work together. A custom rule can point at a playbook, so when the rule blocks, the agent is
told where the org explains how to meet it.

## Write one

Only an admin writes playbooks, in the dashboard: **Guardrails › Playbooks**.

1. Click "New playbook", or "Import a SKILL.md" to start from a skill file you already have. An
   imported file's `name` becomes the playbook's name and slug, its `description` when to read it, and
   the rest the body.
2. Give it a name, pick **Skill** or **Note**, and write one line on when to read it. An agent reads
   that line to decide whether the playbook fits its task, so say when, not what.
3. Write the body in Markdown. "Preview" shows how it reads.
4. Save it. Each save is a new version.

"History" lists every version, shows what changed between any two, and restores an older one as a new
version. "Remove" stops agents being handed it and keeps its versions. A Canyon keeps up to 50
playbooks, and a playbook can't hold a secret: a save with a key or a password in it is refused,
because agents read it and a model reads what they read.

Every change is on the [audit trail](/org/audit-trail/).

## A worked example

Meridian's platform team writes a skill, **How Meridian writes a service**, slug
`service-conventions`, to read "before you add a route or a query to a Meridian service":

````markdown
# Writing a Meridian service

Every route reads its input through the request schema, and every query passes it as a bound parameter:

```ts
db.query("SELECT * FROM claims WHERE id = $1", [claimId])
```

Never build SQL text from a request, and never log a customer's email.
````

They also have a custom rule, **Customer emails are encrypted at rest**. In the rule's editor they pick
the playbook under **Playbook**. When the rule blocks, the finding an agent gets carries one more step
after the fix:

```text
suggested fix:
  Encrypt the address with the app's field key before the insert, and decrypt it where it is read.

then:
  Read the playbook How Meridian writes a service: canyon playbook service-conventions
```

On the app's page the finding shows the same step under **Then**. If the playbook is removed later,
the rule keeps working and its findings stop naming it.

## How agents get it

Every agent is handed the list at the start of each session, with the rest of Canyon's settings. It
names each playbook and when to read it, never the body:

```text
Your org's playbooks, how it builds, written by its admins. Before a task, read each one whose when fits it: run canyon playbook <slug>, or call canyon_playbook with its slug where Canyon's tools are loaded.
- incident-notes: What our incidents taught us. When: Read when you handle a database error or a retry.
- service-conventions: How Meridian writes a service. When: Read before you add a route or a query to a Meridian service.
```

How the agent reads the body depends on the agent.

### Claude Code and Codex: a real skill

Where the agent has skills of its own, a playbook that is a skill becomes one. At the start of each
session, Canyon's session hook writes it to the agent's skills:

- Claude Code: `~/.claude/skills/canyon-<slug>/SKILL.md`
- Codex: `~/.agents/skills/canyon-<slug>/SKILL.md`

```text
$ cat ~/.claude/skills/canyon-service-conventions/SKILL.md
---
name: "service-conventions"
description: "Read before you add a route or a query to a Meridian service."
---

# Writing a Meridian service

Every route reads its input through the request schema, and every query passes it as a bound parameter:
…
```

The hook fetches a playbook only when its version changed, and the agent sees a new skill from its next
session. It writes, updates and removes only the `canyon-*` directories it wrote, each marked by a
`.canyon` file naming the Canyon and the version, so it never touches another skill. A playbook removed,
or turned into a note, is removed from disk. A note is never written to disk. `canyon skill uninstall`
removes them all:

```text
$ canyon skill uninstall --harness claude-code

  Removing Canyon from the coding agents in this environment…

  · Claude Code    ~/.claude/skills/canyon/SKILL.md          not installed
  · Claude Code    ~/.claude/settings.json                   not installed
  · Claude Code    ~/.claude.json                            not installed
  ✓ Claude Code    ~/.claude/skills/canyon-service-conventions/   removed
```

### Any agent with Canyon's MCP tools

The `canyon_playbook` tool takes a playbook's slug and answers its body. For clients that read
resources, `canyon://playbooks/<slug>` answers the same. A finding through the tools names the tool:
`Read the playbook How Meridian writes a service: call canyon_playbook with slug service-conventions`.

### Everywhere else

`canyon playbook <slug>` prints the body, for Cursor's rules, `AGENTS.md` and any agent without MCP:

````text
$ canyon playbook service-conventions
# Writing a Meridian service

Every route reads its input through the request schema, and every query passes it as a bound parameter:

```ts
db.query("SELECT * FROM claims WHERE id = $1", [claimId])
```

Never build SQL text from a request, and never log a customer's email.
````

```text
$ canyon playbook release-checklist

  ✗ No playbook release-checklist in this org. canyon manifest lists its playbooks.
```

## Reads and machines

Each playbook's card shows two counts, neither naming anyone:

- **Reads in 30 days:** each fetch of the body through the tool, the resource or `canyon playbook`.
- **On N machines**, for a skill: the agent sessions whose skill holds the playbook's current version,
  over the last 30 days.

Canyon can't see an agent reading its local copy of a skill, so "on N machines" says where the current
version is, not how often it's read. A machine is counted by a keyed hash of its session, under a key
that changes every 30 days and is never stored, so a count can't be traced back to a person. The rows
behind it are deleted after 30 days.

## Who can do what

Every member reads the playbooks, the way their agents do. Only an admin writes, restores or removes
one, and only in the dashboard: see [who can do what](/org/who-can-do-what/).
