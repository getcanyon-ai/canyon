---
title: Who can do what
description: Every action in Canyon, and who may take it, from a member the app isn't shared with to an admin, a security responder and a coding agent using someone's CLI sign-in.
---

What someone may do with an app depends on their role on Canyon and their level on that app, and a
coding agent counts as the person whose CLI sign-in it uses. [The table](#what-each-person-can-do-with-an-app)
has every action; Canyon's own settings and what only the dashboard can do follow it.

## How Canyon decides

Canyon decides what someone may do from two things:

- **Their role on Canyon as a whole:** member, security responder or admin. It is set in Settings ›
  People & sign-in.
- **Their level on each app:** owner, contributor, read-only, or no access at all. An app's owners
  set it when they share the app ([Share an app](/build/share/)).

A coding agent never has access of its own. It works through a person's CLI sign-in, and Canyon
treats it as that person.

```mermaid title="Who may do what: a role on Canyon, a level on each app, and an agent that counts as its person"
flowchart TD
  agent["A coding agent<br>on your CLI sign-in"]
  person["You, signed in"]:::person
  role["Your role on Canyon<br>member, responder,<br>admin"]
  level["Your level on the app<br>owner, contributor,<br>read-only, no access"]
  decision["What you may do<br>with the app"]:::canyon
  agent -->|"counts as you"| person
  person --> role
  person --> level
  role --> decision
  level --> decision
```

On a Canyon without sign-in, such as the laptop demo, everyone who reaches Canyon counts as an admin,
and nothing on this page is refused.

## Roles on Canyon as a whole

- **Member:** anyone who may sign in, because their address or domain is on the list in Settings ›
  People & sign-in → Who may sign in. Signing in for the first time makes them a member. A member can
  make apps, and holds a level on each app shared with them.
- **Security responder:** a member an admin names, by address in Settings › People & sign-in, or by
  a claim your identity provider sends, such as a group. A responder can approve a blocking finding and stop
  production on any app, shared with them or not. The responder role alone lets nobody deploy,
  promote, set secrets or open an app: a responder does those only on an app shared with them, at
  that app's level.
- **Admin:** can do everything on every app, and change Canyon's own settings. Two things stop even an
  admin: nobody approves a blocking finding of a deploy they started, and nobody changes a
  production that is held (see [Stopping production](#stopping-production)).

Someone who hasn't signed in can do nothing but sign in.

## Levels on each app

- **Owner:** everything with the app, including sharing it and deleting it.
- **Contributor:** works on the app: deploys, promotes, sets secrets, reads its findings and logs.
- **Read-only:** opens the running app, and sees nothing of how it is built.
- **No access:** sees the app in the list, and can ask for access.

Each level is per app, so the same person can own one app and have no access to another. An admin
passes every check on every app without holding a level on it.

## What each person can do with an app

✓ means yes, ✗ means no. **Member, no access** is a member the app isn't shared with. **Responder**
is a security responder the app isn't shared with; a responder the app is shared with also has that
level's column. Actions marked **dashboard only** are done signed in to the dashboard in a browser,
never from the CLI, so a coding agent can't do them.

| Action | Member, no access | Read-only (viewer) | Contributor | Owner | Responder | Admin |
|---|---|---|---|---|---|---|
| See the app in the list | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Open the running app | ✗ | ✓ | ✓ | ✓ | ✗ | ✓ |
| Ask for access | ✓ | ✓ | ✗ | ✗ | ✓ | ✗ |
| See its findings | ✗ | ✗ | ✓ | ✓ | ✓ | ✓ |
| See its runs, versions, logs and secret names | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Upload, check, and deploy to preview | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Deploy a pull request's version to preview (dashboard only) | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Set and remove secrets | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Promote to production, or put an earlier version back | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Approve or deny a promotion request (dashboard only) | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Approve a finding that blocks nothing | ✗ | ✗ | ✓ | ✓ | ✓ | ✓ |
| Approve a blocking finding (dashboard only) | ✗ | ✗ | ✗ | ✗ | ✓ | ✓ |
| Take preview or production offline, or cancel a run | ✗ | ✗ | ✓ | ✓ | ✗ | ✓ |
| Stop production and hold it | ✗ | ✗ | ✗ | ✗ | ✓ | ✓ |
| Lift a hold (dashboard only) | ✗ | ✗ | ✗ | ✗ | ✓ | ✓ |
| Share it, change someone's level, answer access requests | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ |
| Rename it | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ |
| Delete it | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ |
| Turn auto-deploy from GitHub on or off | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ |
| Make a rule stricter for this app alone | ✗ | ✗ | ✗ | ✓ | ✗ | ✓ |

- **A member with no access** sees only the app's name, owners and description in the list.
- **Read-only** people also see the app's live addresses, and whether each runs. They open the app
  with the app roles it was shared with.
- **A responder** sees only what approving and stopping need ([below](#what-a-responder-sees)).
- A refusal says whom to ask: the app's first owner, else a member of the group that owns it, else
  an admin.

### Approving a blocking finding

A **blocking finding** is one whose rule blocks it, before preview or before production. Only an
admin or a security responder approves one, signed in to the dashboard. Nobody approves a blocking
finding of a deploy they started, including one a coding agent started with their CLI sign-in. An
approval an admin or a responder makes on someone else's deploy never counts for one they started
themselves.

A deploy that a GitHub push started belongs to whoever linked the GitHub account that pushed
([Link your GitHub account](/build/github/#link-your-github-account)). Until someone links that
account, the person who pushed, if they are an admin or a responder, can approve it. An admin can
close that gap by turning on **Approvers of a push must link GitHub** in Guardrails: then only
someone who linked their GitHub account approves a push's blocking finding.

A finding that blocks nothing can also be approved by the app's owners and contributors. While
production approval is on, that too is dashboard only (see
[What only the dashboard can do](#what-only-the-dashboard-can-do)).

### Stopping production

A security responder or an admin can stop an app's production and **hold** it, with a reason, when
something in production must not keep running. A coding agent on a responder's or an admin's CLI
sign-in can do it too, since it counts as them; Canyon's own automated steps, and the token your
deployment platform uses under [hand-off](/self-host/hand-off/), can't. There are three ways:

- from the CLI: `canyon undeploy --app <app> --env prod --hold --reason "…"`;
- in the dashboard: **Stop production** on the app's row in the fleet, the dashboard's list of apps;
- through the API: `POST /api/v1/projects/<app>/envs/prod/hold`, with a CLI sign-in
  ([Calling Canyon's API](/reference/api/)).

While production is held, nobody promotes it, puts back an earlier version, takes it offline or
deletes the app, an admin included. Preview, checks and secrets go on, so the team can ship its fix
to preview and rotate what leaked.

When your own platform runs the app ([hand-off](/self-host/hand-off/)), holding production asks
your platform to remove it, and the app lets nobody in within about a minute, even while your
platform's pods are still running (see
[When an environment is stopped](/self-host/hand-off/#when-an-environment-is-stopped)).

A responder or an admin lifts the hold with **Lift the hold** on the fleet row, in the dashboard
only, and can start the held version again. Nobody on the app's team (an owner or a contributor)
lifts its hold unless they are an admin, nor does anyone who was on the team when it was held.

### Claiming an app nobody owns

Any member can claim an app nobody looks after yet, and become its owner. That is an app with no
owner of any kind (a person or a group), no secret, nothing deployed, production not held, and
nothing from GitHub. The dashboard shows **Claim** only where Canyon would accept it. Any other app
without an owner is an admin's to adopt: the admin names its owner.

### What a responder sees

On an app that isn't shared with them, a security responder sees what approving and stopping need,
and nothing else:

- in the list: the app's name, owners and description, production's address, whether it runs, and
  whether it is held;
- on the app's page: its reviews, each one's findings with their few lines of code, and the go-live
  checks.

A responder doesn't see preview, earlier versions, who deployed them, logs, secret names, or anything
else of the app.

### Reading the audit trail

- An admin reads every row.
- Every member reads their own sign-ins, sign-outs and refusals.
- An app's owners and contributors also read every row of that app.
- A responder also reads the stops and lifts they made.

See [The audit trail](/org/audit-trail/).

### Other details

- **Renaming** changes the app's name alone. Its address, its `canyon.toml` and `--app` keep the
  app's id. Rename in the app's ⋯ menu, or with `canyon rename`.
- **Everyone** at your org can be given read-only access to an app, never more.
- **An app always keeps an owner.** The last one can't be removed or demoted until another is added.
- **Owners also decide who writes the app's code**: for an app whose code Canyon keeps on GitHub,
  they can hand the repository to developers.

## Canyon's own settings

Only an admin changes these. A member, at any level on any app, is refused and told which admin to
ask. A coding agent on an admin's CLI sign-in can change them too, except where this list says
otherwise:

- Set rules and scanners to Off, Advisory or Blocking, and write custom rules. Dashboard only while
  production approval is on.
- Change the approved buildpacks or the design system, or use **Set up with Canyon**. Dashboard only
  while production approval is on.
- Change the approved stacks.
- Write, restore or remove a playbook. Only a person, and only in the dashboard. Every member can
  read the playbooks.
- Connect GitHub, import a repository, link an app to a repository or unlink it.
- Change who may sign in and who is an admin, and sign someone out everywhere.
- Change how people reach apps (the front door) and the platform webhook.
- Turn production approval on or off. Dashboard only.
- Turn **Approvers of a push must link GitHub** on or off. Dashboard only, and turning it on needs
  sign-in on.
- Change the outbound traffic setting, which decides where apps may connect. Dashboard only.
- See, connect, change, test or remove the [Slack and Teams channels](/org/integrations/) Canyon posts to.

## What only the dashboard can do

These need someone signed in to the dashboard in a browser. A CLI sign-in, and so a coding agent, is
refused and told to use the dashboard. The API calls behind each are listed in
[Calling Canyon's API](/reference/api/#what-needs-the-dashboard):

- Approve or deny a CLI sign-in code. Otherwise a CLI sign-in could approve a fresh one for itself
  and never expire.
- Approve or deny a promotion request.
- Turn production approval on or off.
- Turn **Approvers of a push must link GitHub** on or off.
- Change the outbound traffic setting.
- Deploy a pull request's version to preview.
- Promote a version whose preview answered with an error.
- Approve a blocking finding.
- Lift a hold on production.

**Production approval** is the setting **Production is approved in the dashboard**, in Settings ›
People & sign-in → Production. An admin turns it on to make every promotion a person's decision in
the dashboard. While it is on, these move to the dashboard too:

- Promote. The CLI sends a promotion request instead, and a person approves it in the dashboard
  ([Get to production](/build/production/#when-your-org-has-a-person-approve-production)).
- Approve a finding that blocks nothing.
- Change a rule's state or a custom rule.
- Change the approved buildpacks or the design system, or use **Set up with Canyon**.
