---
slug: rules/prod.human-approval
title: "A person promotes"
description: "A promotion to production comes from a person's sign-in, or from a person in the dashboard."
---

`prod.human-approval` · Blocking by default · found by Canyon, when you promote

## What it finds

Who is promoting. When you run `canyon deploy --env prod`, the rule passes when the request comes
from a person's sign-in, rather than from an automated caller. A coding agent signed in as
you counts as you. On a Canyon without sign-in, as
in the laptop demo, every request counts as a person, so the rule always passes. Its line in the
go-live checks says what that covers:

```text wrap
✓ A person promotes
    a person promotes, or an agent they ask, signed in as them
```

An org can go further. With **A person approves each promotion to production, here in Canyon** turned on, in the
Production section of Settings › People & sign-in, the CLI and agents only ask. `canyon deploy --env prod` prints a link and a code that expires in
24 hours, and a person on the app's team approves the promotion in Canyon's dashboard:

```text wrap
This org has a person approve production in Canyon.
Ask a person to approve: http://localhost:7777/#/approve?code=K7QP-4RXM
Code K7QP-4RXM · expires in 24 hours
```

That switch needs sign-in on, and only the dashboard can change it.

## Why it matters

Production changes when a person decides it should, not when an automated caller does.

## How to fix it

Promote with your own CLI sign-in, or ask your coding agent to run `canyon deploy --env prod` with it.
Where the org approves production in the dashboard, open the link the CLI prints and have a person on
the app's team approve it there. Canyon takes that approval only from the dashboard, never from the
CLI.

## Check the fix

`canyon status` lists the go-live checks with this rule's line. `canyon deploy --env prod` prints
the same list when it refuses.

## If you think it's wrong

This rule has no findings to approve. An admin can set it to Advisory or Off under Guardrails →
Policies. While **A person approves each promotion to production, here in Canyon** is on, the rule blocks whatever its
state.

While the rule is Blocking, only a person can also take production offline or cancel a promotion.
