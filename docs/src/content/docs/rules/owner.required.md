---
slug: rules/owner.required
title: "Every app has an owner"
description: "An app going to production has at least one person who owns it."
---

`owner.required` · Advisory by default · found by Canyon, when you promote

## What it finds

When you run `canyon deploy --env prod`, this rule checks that the app has at least one owner. Its
line in the go-live checks names them, as in Canyon's CLI output:

```
✓ Every app has an owner
    owned by Dana Whitfield
```

With no owner it reads "no owner has claimed this app".

On a Canyon with sign-in on, whoever creates an app is its owner. On a Canyon without sign-in, nobody
is, until someone claims it.

## Why it matters

When the app breaks in production, there is nobody to ask.

## How to fix it

Give the app an owner. In the dashboard's fleet, the list of apps, an app with no owner shows
**No owner**, and, for an admin on a Canyon with sign-in on, a way to assign one. **Claim** shows only where Canyon
would accept your claim. Anyone may claim an app that has none of these:

- an owner of any kind, a person or a group. A group that owns an app is not counted by this rule,
  which wants a person, but it does keep others from claiming the app;
- a secret, in either environment;
- anything deployed, in either environment;
- a link to a GitHub repository, or a version that came from GitHub, even if the link was removed
  since.

Any other app is its owners' and admins' to claim: an admin adopts it and names its owner. See
[Who can do what](/org/who-can-do-what/).

## Check the fix

`canyon status` lists the go-live checks with this rule's line. `canyon deploy --env prod` prints
the same list when it refuses.

## If you think it's wrong

This rule has no findings to approve. An admin can set it to Off or Blocking under Guardrails →
Policies. As Advisory, a line that doesn't pass shows in the go-live checks and the promotion goes
ahead.
