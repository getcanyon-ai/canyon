---
title: First boot and sign-in
description: What the first admin sees when Canyon starts with sign-in on, how setup decides who may sign in, and how the CLI signs in.
---

This page starts where [Sign-in, the AI model and keys](/org/set-up/) ends: Canyon is running with
sign-in on, and `CANYON_ADMINS` names you.

```diagram kind=flow title="First boot: only the first admins sign in until one of them finishes setup"
steps:
  - {who: Canyon, icon: canyon, tone: canyon, step: 1, what: Starts in setup, more: "With sign-in on, and `CANYON_ADMINS` naming its first admins."}
  - {who: Canyon, icon: key, tone: canyon, step: 2, what: "Only the `CANYON_ADMINS` addresses can sign in", more: "Canyon serves nothing but the setup screen, and nobody can approve a CLI sign-in yet."}
  - {who: The first admin, icon: person, tone: person, step: 3, what: Answers Set up Canyon, more: "The organization's name, who may sign in, and who administers Canyon."}
  - via: Finish setup
  - {who: Everyone on the lists, icon: passed, tone: passed, step: 4, what: Can sign in, more: The fleet and every other page open.}
```

## Until setup is finished

A new Canyon starts in setup. While it is in setup:

- Only the addresses in `CANYON_ADMINS` can sign in. Anyone else who signs in through your identity
  provider lands on one line: "Canyon isn't set up yet. Ask the person who installed it to finish
  setup." There is no button, because trying again can't help.
- Canyon serves nothing but the setup screen. The fleet, the apps and every other page wait.
- Nobody can approve a CLI sign-in. `canyon login` against a Canyon in setup waits for a code nobody
  can approve, and when the code expires the CLI says Canyon is still being set up.

Each person turned away like this is recorded in the [audit trail](/org/audit-trail/).

## Sign in

Open Canyon. The page says "Sign in to Canyon" and has one button, "Continue to company sign-in". It
sends you to your identity provider and back. Canyon never sees your password.

Canyon knows you by the `email` claim in your sign-in, or `preferred_username` when there is no
`email` claim. It has to be an email address. A sign-in whose `email_verified` claim is present and
not true is refused.

A browser sign-in lasts 12 hours.

## The setup screen

The first admin to sign in sees "Set up Canyon": three questions, then a summary. Nothing is saved
until the last step, so you can walk away halfway and the host stays in setup.

1. **What is this organization called?** The name is shown on every app's go-live checklist.
2. **Who may sign in?** Two lists:
   - **People**: single addresses. Your own address is already on the list. Canyon does not send
     invitations: add an address, then tell that person to open Canyon and sign in.
   - **Domains**: anyone at an allowed domain who can sign in to your identity provider can sign in
     to Canyon. A box offers "Allow everyone at" your own domain. It is never ticked for you: if
     your address is at a public mail provider, ticking it would let in all of that provider.

   "Continue" stays disabled until at least one person or one domain is on a list.
3. **Who administers Canyon?** The admins:
   - **From the environment**: the `CANYON_ADMINS` addresses. They are admins whatever the screen
     says, and can't be removed here.
   - **By email**: more admin addresses. Yours is already on the list.
   - **By sign-in group (optional)**: a claim and a value. Anyone whose sign-in carries that claim
     with that value is an admin. Canyon reads it at sign-in, so it takes effect at a person's next
     sign-in.

   An admin address that is on neither sign-in list is added to the allowed addresses, and the step
   says so.
4. **Ready.** A summary of the three answers, each with an edit link. "Finish setup" saves them.

When Canyon refuses to finish, it says why above the buttons. It refuses when nobody would be allowed
to sign in, when there would be no admin address and no admin claim, or when no admin address is on
either sign-in list.

Finishing is recorded in the audit trail as "finished setting Canyon up". From then on the fleet is
served and everyone on the lists can sign in.

### Values set on the server

`CANYON_ORG`, `CANYON_OIDC_ALLOWED_DOMAINS`, `CANYON_OIDC_ALLOWED_EMAILS`, `CANYON_ADMIN_CLAIM` with
`CANYON_ADMIN_VALUE`, and `CANYON_RESPONDER_CLAIM` with `CANYON_RESPONDER_VALUE` each fix one of these answers in the server's
environment. The screen shows such a value read-only, with "Set on the server, by" and the
variable's name. Change it there, not on the screen.

`CANYON_ADMINS` is different: it adds admins and never replaces the list on the screen.

## Changing it later

Every answer is on **Settings** later, in the same words:

- **Organization** holds the name.
- **People & sign-in** holds who may sign in, who administers Canyon, and **Groups**: the groups
  claim, the sign-in claim that lists a person's groups (`CANYON_GROUPS_CLAIM` fixes it on the
  server). Once it is set, owners can share an app with a group. It also holds the **Production** switch, "Production is approved in the dashboard".

Only an admin can save these. A member sees the values, except the admin claim, and no Save button.

Removing a person from the lists takes effect on their next request. Taking an address off the
admin list makes that person a member on their next request, unless `CANYON_ADMINS` or the admin
claim still makes them an admin.

## Sign in from the CLI

`canyon login <Canyon's address>` signs the CLI in with a code a person approves in the browser.
[Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in) walks through it;
send your developers there once setup is finished. On a Canyon with sign-in off, `canyon login`
connects without a code.

## Connect your developers' agents

Your developers connect their coding agents in one of two ways ([Connect your agent](/start/connect-your-agent/)):

- **On a laptop:** `canyon login <Canyon's address>`, then `canyon skill install`. It sets up Canyon's skill, its
  session hook and its MCP server for each agent it finds.
- **In the cloud:** Claude Code on the web and Cursor's cloud agents connect to `<Canyon's address>/mcp`, and each
  person allows it once in the browser. That needs Canyon reachable from the internet, Canyon's GitHub
  App on your repositories ([Connect GitHub](/build/github/)), and each person's GitHub account linked.

## Usage reports

Once a day, Canyon sends a short report of counts, never names. It's on by default.
[Usage reports](/org/usage-reports/) says what is sent and how to turn it off.
