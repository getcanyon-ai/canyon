---
title: Share an app
description: Who can open an app and who can change it, how to share it with a person, a group or everyone, and how to ask for access.
---

A new app is private. The person who made it is its owner, and nobody else but Canyon's admins can
open it until an owner shares it, with `canyon share` or on the app's page, with a person, a group or
everyone at your org. With sign-in on, a person signs in through your identity provider (such as
Okta, Entra ID or Google) before the app sees them.

## Who signs in to Canyon

- **Admins** can do everything on every app. They also change Canyon's settings and the org's
  rules. The first ones are set when Canyon is installed; more are added in Settings › People &
  sign-in → Who administers Canyon, by email or by a group from your identity provider.
- **Security responders** are your security team. They can approve blocking findings and stop
  production on any app. The role alone lets them deploy nothing: on an app shared with them, they
  have that app's level like anyone else. See
  [Who can do what](/org/who-can-do-what/#roles-on-canyon-as-a-whole).
- **Members** are everyone else who may sign in: the domains and addresses listed under Settings ›
  People & sign-in → Who may sign in. A member can make an app, and holds a level on each app
  someone shares with them.

Removing someone from that list takes effect on their next request. See
[Sign-in, the AI model and keys](/org/set-up/).

## The levels on an app

Each person holds one level on each app. The level decides what they can do with it:

| Level on the app | Use the running app | See its findings, runs, logs and secret names | Deploy, promote, set secrets | Share it, delete it |
|---|---|---|---|---|
| **Owner** | yes | yes | yes | yes |
| **Contributor** | yes | yes | yes | no |
| **Read-only** | yes | no | no | no |
| **No access** | no | no | no | no |

- No level on an app lets someone approve a blocking finding
  ([who does](/org/who-can-do-what/#approving-a-blocking-finding)).
- A read-only person sees the app's name, its owners, its one-line description and its live
  addresses (**Open preview**, **Open production**), and nothing of what runs there.
- A **grant** is one line of the app's sharing list: a person, a group or everyone, with a level.
  A person's level is the highest of the grants that match them, by email, by group, or as
  everyone. Their app roles (below) are all of those grants' roles together.
- An app always keeps an owner. Add another owner before you remove or demote the last one.

## Share it

Owners and admins share from the CLI or from the app's page. From the CLI, in the app's directory:

```sh
canyon share sam@example.com                          # read-only
canyon share sam@example.com --level contributor
canyon share group:claims-engineering --level contributor
canyon share everyone                                 # everyone at your org, read-only
```

From a Canyon with sign-in on (the output below is from one, with its address and org replaced by
`example.com` and Example Co), `canyon share everyone` answers with the link to send and the app's
team as it now stands:

```text
  canyon · dapper-orchid-fvw · canyon.example.com

  Shared with everyone at Example Co. They can open it within about a minute:
    https://dapper-orchid-fvw.preview.example.com

  dapper-orchid-fvw

  sam@example.com                owner
  everyone at Example Co         read-only
```

- **Read-only is the default.** `--level` is `owner`, `contributor` or `read-only`.
- **Everyone** means everyone who may sign in to your Canyon. It is always read-only.
- **Groups** come from your identity provider. They work once an admin sets the **Groups claim**
  (the field of the sign-in token that lists a person's groups) in Settings › People & sign-in →
  Groups.
- **App roles** are roles your app defines for itself, such as `expense-approver`, declared in its
  `canyon.toml`. `--role expense-approver` grants one, and is repeatable. The app reads a person's
  roles when they sign in to it ([Sign-in in your app](/build/sign-in-in-your-app/)).
- **It reaches the app within about a minute.** A removal can also take up to a minute.
- `--app <id>` shares a different app from the one `canyon.toml` names.

To see who has access, and who has asked:

```sh
canyon share
```

```text
  canyon · dapper-orchid-fvw · canyon.example.com

  dapper-orchid-fvw

  sam@example.com                owner
  everyone at Example Co         read-only
```

To take a grant away, `canyon share --remove everyone` (or an address, or `group:<name>`):

```text
  canyon · dapper-orchid-fvw · canyon.example.com

  Removed everyone at Example Co. The app may still let them in for up to a minute.

  dapper-orchid-fvw

  sam@example.com                owner
```

On the app's page, the same list is the **Who can open it** card. Type an address, `group:name` or
**Everyone at** your org into **Share with**, pick the level, and click **Share**. Each row can
be removed, and a person's or a group's level changed there.

## Ask for access

- **No access:** a person who opens an app they can't reach sees **You don't have access to** the
  app, who owns it, and **Ask for access**.
- **Read-only:** the app's page says **Your access: read-only.** and has **Ask to join its
  team**, to ask to be able to change the app.
- **Owners and admins see the request** under **Requests** on the **Who can open it** card, with
  **Dismiss** and **Add as read-only**. A read-only person's request has **Add to its team**
  instead, which makes them a contributor. In `canyon share` each request has the command that
  grants it: `canyon share <email> --level contributor` for a read-only person's.

Canyon sends no email. Tell the owner you asked.

## Your agent and sharing

`canyon status` has an **access** line: private, or who it is shared with, and your own level. The
Canyon skill (the instructions `canyon` installs for your coding agent) tells the agent to say who
can open the link after each deploy. It runs `canyon share` only when you ask, with exactly the
people you name, read-only unless you say otherwise.

## On a Canyon without sign-in

The laptop demo runs without sign-in. There, anyone who can reach Canyon can open every app, so
there is nothing to share: `canyon share` says so and changes nothing. Turn sign-in on to share
with specific people ([Sign-in, the AI model and keys](/org/set-up/)).
