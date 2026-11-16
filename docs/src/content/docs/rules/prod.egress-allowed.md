---
slug: rules/prod.egress-allowed
title: "Production allows everything preview connects to"
description: "Everything preview connects to is allowed in production."
---

`prod.egress-allowed` · Off by default · found by Canyon, when you promote

## What it finds

An app can connect to one set of destinations in preview and be allowed a different set in production.
This rule catches the difference before you promote, so a version doesn't reach production only to have its
connections refused.

When you promote, Canyon reads every destination preview connected to since the image it runs first went live there,
and asks of each what production would answer now. A destination production wouldn't allow fails the rule, and its line
in the go-live checks names it: "production does not allow api.openai.com". Deploying the same image again doesn't
clear it. A new image does.

The read of each deployment also starts 90 seconds early, so a connection already open at that moment counts. That can
catch the last calls of the version before: a call just before go-live counts until you deploy again.

There is no soak time: Canyon reads what preview has done so far, so promoting seconds after a fresh deploy passes
with whatever it has connected to by then. A long plain `http://` call still running isn't counted until it ends.

Production allows a destination the way a call there would be answered:

- Under an allowlist, it allows a destination on that allowlist, or one of the
  [platform destinations](/org/outbound/#on-an-apps-page).
- Under Observe, it allows any host name on port 80 or 443.
- Under either, it never allows an IP address, a single-label name or any other port, so a call like that fails this
  rule whatever production's mode.

**When Canyon can't compare the two, the rule doesn't pass.** Its line starts "could not check" and says what to do. The
line is short, so this table says what can be behind it:

| Canyon can't compare because | What to do |
|---|---|
| Not all of preview's connections since it went live went through Canyon | Send them [through Canyon](/org/outbound/#send-apps-calls-through-canyon), then deploy to preview again: recreating the network alone leaves what it reached before unread |
| Your platform still runs preview, but Canyon runs apps again, so production would be Canyon's | Deploy to preview again, so Canyon runs it and records its connections |
| Canyon lost some connection records since the version went live | Deploy to preview again, let it run, then promote |
| Canyon can't tell when the image first went live in preview, as no earlier deploy of it is on record | Deploy to preview again, then promote |
| The version went live more than 30 days ago, and Canyon keeps 30 days of records | Deploy to preview again, then promote |
| Preview connected to over 500 destinations, more than Canyon tracks | Connect to fewer and deploy to preview. A gap that ended within 90 seconds before go-live still counts, so after a fix you may need a second deploy a few minutes later |
| Canyon isn't receiving connection records right now, or couldn't read them | Try again in a few minutes, or ask an admin to check the egress gateway |

**When your own platform runs the app, Canyon doesn't check it.** Canyon doesn't control the outbound connections of an
app your platform runs. While your platform runs preview and promotions go to your platform too, the line reads "not
checked: preview runs on your own platform", and it stops nothing, whether the rule is Advisory or Blocking.

A **rollback** is never held by this rule: it puts back what production ran, so there is no preview to compare. Its
line lists any destination a person dismissed that production refuses today, because the version you put back may
try to connect to it. If your own platform runs production it lists none, as there is nothing to allow in Canyon.

Set to Advisory the rule shows its line and stops nothing. Set to Blocking it stops the promotion until the line
passes.

## Why it matters

A connection that worked in preview and is refused in production fails in front of your users.

## How to fix it

A person allows each host name in Canyon's dashboard; anything else has to change in the code. Open the
app's [*Outbound* card](/org/outbound/#on-an-apps-page): under *Production* it lists what preview connects to and
production doesn't allow, each with **Allow**. Only a person in the dashboard can allow one: an agent can't, and
neither can the CLI. A production that is held can't be widened until the hold is lifted.

No one can allow an IP address, a single-label name or a port other than 80 and 443, so change that call in the code
and deploy the new version. Do the same for a destination the app doesn't need: one the new image hasn't connected to
no longer counts.
