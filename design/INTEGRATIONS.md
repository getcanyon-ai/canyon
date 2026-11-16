# Integrations — telling a channel when production needs a person

**Built** (#2725): the channels and the summary, then the messages, the outbox and `notify_failing`,
then Settings › Integrations and the failing channel on the fleet. The owner's decision, 2026-11-08: *"Yes we need to allow for such
integration."* Today Canyon sends no messages. A refused call in production goes unnoticed until someone
opens Canyon or an agent mentions it. This file is the source of truth for the messages Canyon posts to a
team's chat: what it posts, when, to where, and who sets it up. The lead decided its open questions on
2026-11-09; the owner can overrule any of them (*Decisions*, at the end).

## What Canyon posts, and when

Canyon posts from the same list the dashboard shows, `needs[]` (contracts/routes.md, *What needs a person*).
It never works out its own, so a channel and a screen never disagree.

**Which needs.** Production's needs (`env: prod`), and one need of the app:

| `kind` | Why it's worth a message |
|---|---|
| `held` | Production was stopped by a responder or an admin, and stays stopped until one of them lifts it. |
| `down` | Production isn't running: stopped or restarting. |
| `refused` | Production can't reach a destination, and a person on the team can allow it. |
| `refused_code` | Production can't reach a destination nobody can allow. It's the code's to fix. |
| `approval` | A promotion waits for a person to approve it. |
| `patch` | A patch of production waits for a person to approve it. |
| `secret` | The latest review found a secret in the app's code. It says something leaked, so it posts, by name only. |
| `notify_failing` | A channel Canyon posts to is failing (*Failure is seen*). It posts to the org's other channels. |

The dashboard shows these too, but they're never posted:
- **Preview's needs.** Preview is where a team works; a message for each one would be noise.
- **The app's other needs** (`blocked`, `unchecked`, `unowned`, `never_reviewed`). They block a version,
  not what runs.
- **`access`.** It goes to the app's owners, not a channel.
- **`recreate`.** It's an admin's, on Settings.

**When: on the server's own tick.** Once a minute, the server computes every app's needs itself, as the
fleet read does, in the same fixed number of statements whatever the fleet's size. It doesn't wait for anyone
to have the dashboard open, so messages never depend on a reader. With more than one server, the one holding
the tick's advisory lock computes and the others skip that minute.

It compares each need with what it last posted (*Once, then only on a change*). A need found while the
executor's state could not be read is not posted, and neither is its clearing. That follows the needs list's
own rule: what it could not read is never all clear, and never a new alarm either.

## What a message says

**Names only.** A message carries:
- the app's name;
- what's wrong, in D5's words for that need, the same as the dashboard's;
- the destination by its host name and port, for `refused` and `refused_code`;
- the version, for `approval`, `patch` and `secret`;
- when the need began;
- a link to the app's page in Canyon, where a person acts on it.

For example:

> **claims-portal** · production can't reach `api.stripe.com`. A person on the app's team can allow it in
> Canyon. *Since 14:02 UTC* · [Open in Canyon](https://canyon.meridian.example/#/apps/claims-portal/outbound)

> **claims-portal** has a secret in its code. *Since 14:02 UTC* · [Open in Canyon](https://canyon.meridian.example/#/apps/claims-portal)

**When a need clears, one short line:**

> **claims-portal** · production's calls to `api.stripe.com` no longer need a person.

It says no more than that: a refused call clears when a person allows it, when one dismisses it, or when
production stops making it, and the tick can't tell which.

Slack's incoming webhooks don't return the message they posted, so there's no thread to reply in. The
clearing is a plain line. A thread would need a bot token, which v1 doesn't ask for.

**Never:**
- a secret's name or value, or a resource's value: `secret` says only that the code holds one;
- a finding's code, snippet or fix;
- an app's log line (`stopped.said`);
- a URL's userinfo, an approval's `code` (whoever holds it can open the approval page), or a person's address.

A need that carries a free-text field from the app (a stopped app's last line) is posted without it.

The message is built from a fixed template per kind, filled with named fields. The test for #2725 sets
secret values on every field a message could draw from and checks that no message carries one.

**One message model, two renderings.** Slack's incoming webhooks and Teams' workflow webhooks take
different bodies:
- Slack: `{text, blocks}`.
- Teams: an Adaptive Card wrapped as `{type: "message", attachments: [{contentType:
  "application/vnd.microsoft.card.adaptive", content}]}`. Teams' old Office 365 connectors are retired.

So Canyon has **one message model**, `{kind, need, state, app, env, fields, since, link}`, `state` being
`open` or `cleared`, the line D5's for the need and state (contracts/routes.md, *Integrations*). The channel's `kind`, `slack` or `teams`, picks how the server renders it. Neither body
is the other's, so they can't share one generic webhook shape. A third kind, a generic JSON webhook signed as
the platform webhook is, fits the model unchanged. It isn't built now.

## Once, then only on a change

**Per need.** Each need has a key: app, `env`, `kind`, and for `refused` and `refused_code` the
destination and port.
- It is posted **once**, when it first appears.
- It is posted again only when it **changes in a way a person acts on**: `down`'s `state` changes, an
  `approval`'s, `patch`'s or `secret`'s version changes, or production is held while a `refused` need is
  open (an Allow then waits on the hold).
- A count going up is not a change: a `refused` need refused 3 times, then 300, is one message.
- When it clears, it posts the one clearing line.

**Flapping.** A need that clears and comes back within **30 minutes** is the same need. Its clearing isn't
posted until it has stayed clear that long, and its return posts nothing. One that comes back later is new.

**Per channel.**
- At most **one message a second**, as Slack allows an incoming webhook.
- At most **20 messages a minute**. Past that, one message says *"N more messages about production: open
  Canyon."* with a link to the fleet, and the rest are not posted one by one.

**On connect, a summary.** When a channel is connected, or when an admin asks for a test, Canyon posts one
summary of what's open now:

> Canyon will post here when production needs a person. Right now: **3** production needs, across
> claims-portal and endorsement-hub. [Open in Canyon](https://canyon.meridian.example/#/fleet)

With nothing open, it reads *"Right now nothing in production needs a person."* That message is the test
message too. The needs it summed up count as posted, so none of them is then posted one by one.

The server keeps what it posted, per channel and need key, in a table with when and what changed, so a
restart never queues the same need twice (the outbox's own delivery is at least once, below).

## Setup

**Who: an admin** (design/PERMISSIONS.md, Settings). On Settings › Integrations, an admin adds a channel:
- a name, *"#platform-alerts"*;
- its `kind`, Slack or Teams;
- its incoming webhook URL, from Slack's app settings, or from a Teams workflow *"When a Teams webhook
  request is received"*.

Saving it posts the summary above first, and saves only when Slack or Teams answers it: a URL that doesn't work is never saved. *Send a test message* posts it again.

**Scope.** The org has at most 5 channels, and each gets every app's needs above. Sending an app's needs to
its own team's channel isn't built now.

**The URL is checked.**
- It must be `https`.
- Its host must be one the channel's kind posts to: `hooks.slack.com` for Slack, and for Teams the
  workflow hosts (`*.logic.azure.com`, `*.webhook.office.com`, `*.powerplatform.com`).
- Any other host is refused, `400`, so a typo or a pasted link can't make Canyon post elsewhere.

**Server egress.** The server, not an app, posts. On Kubernetes its NetworkPolicy already lets it out on 443
to any public address (deploy/k8s/server.yaml). On a host with a firewall, the operator allows those hosts
(docs, self-host).

## The webhook URL is a secret

**It is a bearer credential.** Whoever has it can post to the channel. So it is held as a secret is:
- sealed in the vault, under its own data key, as `secrets` rows are (vault/), in a new table,
  `notify_channels (id, name, kind, key_ref, sealed_key, sealed_url, created_by_*, created_at,
  failing_since, failing_status)`;
- never answered by any read. Settings shows the host and the URL's last four characters,
  `hooks.slack.com/…/a1B9`;
- never logged, and never in an audit row or a timeline row.

**A new sealed table lives in three places** (AGENTS.md):
- `main.go`'s two boot-invariant checks, `refuseNewKeyOverSealedRows` and `refuseRowsUnderAMissingKey`;
- `cmd/canyon-server/rotate.go`'s `rewrapOne` list and its remaining count;
- `rotate_test.go`'s `sealedRow` enumeration.

The build PR adds it to all three, and its tests cover a rotation and a missing key.

## Delivery, failure and audit

**Delivery.**
- Sending is a small outbox: a row per message, sent by a loop that leases it, so two servers never post
  one at the same time. Delivery is at least once (decided 2026-11-12, #2725): a server that stops between
  the channel's 2xx and marking the row sent posts it again once the lease lapses, and a post the channel took
  but answered too late is retried. Incoming webhooks take no idempotency key.
- Each post has 10 seconds.
- A `429` or a `5xx` is retried with backoff, honouring `Retry-After`, for up to an hour. After that the
  message is dropped.
- A `404` or `410` is a revoked webhook: the channel is marked failing and nothing more is posted until an
  admin fixes it.

The outbox keeps each message's history for 30 days: when it was posted, and what Slack or Teams answered.
That record is how an admin checks a send, not the audit read.

**Failure is seen.**
- A failing channel says so on Settings, with when it started and the status its webhook answered, never
  the URL.
- While any channel fails, admins get a need, `notify_failing`, rank 11 beside `recreate`. That's a new
  kind in *What needs a person*. It posts to the org's other channels, if there are any, so a channel that
  fails never fails silently.

**Audit** (contracts/routes.md, the audit read), with the channel's name, kind and URL host, never the URL:
- `integration.channel.set`;
- `integration.channel.removed`;
- `integration.channel.tested`;
- `integration.channel.failing`, with the answer's status, once as it starts failing.

Each message sent is in the outbox's history, not one audit row each.

## Permissions

| Who | Channels on Settings | A test message | What a message shows |
|---|---|---|---|
| An admin | Adds, changes, removes. Reads names, kinds, hosts and failures, never URLs. | Sends | — |
| A member, a responder | Reads nothing of them: the reads answer `403 admin_required` | — | — |
| Whoever reads the channel | — | — | App names, production's state, destination host names, version labels, and a link that still asks them to sign in |

A message goes to everyone in the channel, Canyon users or not. That's why it carries names only. The
admin who connects a channel decides who reads it, and the setup screen says so: *"Everyone in this channel
will see your apps' names and what production can't reach."*

## Seams the build adds

The rows go first, in the PR that builds them (AGENTS.md rule 5). It's built in two:
- **A1:** the channels, their sealed table, the URL's host check, the summary, and this file;
- **A2:** the tick, the dedupe, the outbox, the need and clearing messages, and `notify_failing`.

- **contracts/routes.md:**
  - `GET /api/v1/integrations/channels`;
  - `PUT /api/v1/integrations/channels/:id`;
  - `DELETE /api/v1/integrations/channels/:id`;
  - `POST /api/v1/integrations/channels/:id/test`;
  - *The message*: its model, both renderings, the summary and its never-list;
  - the `notify_failing` need in *What needs a person*;
  - the four audit actions.
- **design/PERMISSIONS.md:** a row for Integrations: admin.
- **design/EXPERIENCE.md:** the message and clearing sentences per kind, the summary, the setup warning and
  the failing line.
- **A schema** for the message model, `contracts/schema/integrations.schema.json`, so the two renderings
  are checked against one shape.

**Acceptance** (#2725):
- a need posts once, again only when it changes, and once when it clears;
- no secret value set on any field a message draws from reaches a message;
- an admin sets a channel and a member is refused.

## Decisions

The lead decided these on 2026-11-09; the owner can overrule any of them.

1. **`secret`:** it posts, by name only: *"<app> has a secret in its code."*
2. **Clearing:** one short line when a need clears. A plain line, since an incoming webhook has no thread.
3. **On connect:** a summary of what's open, which doubles as the test message.
4. **A generic webhook:** not now. Slack and Teams only, and the model leaves room for it.
5. **Per-app channels:** not now. Org-wide channels only.
6. **A failing channel:** it's a need for admins, so it never fails silently.
7. **Audit:** a channel set, removed, tested and failing. Sends stay in the outbox's history.
8. **Preview:** never, for now.
9. **When:** on the server's own once-a-minute tick, never on a reader's request, with the fixed statement
   count.
