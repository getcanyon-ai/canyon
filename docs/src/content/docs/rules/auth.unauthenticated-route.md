---
slug: rules/auth.unauthenticated-route
title: "Every route needs sign-in"
description: "A route of the app that anyone can reach without signing in, found by Canyon's AI review."
---

`auth.unauthenticated-route` · Advisory by default · found by the AI review, when an AI model is set up

## What it finds

A route that serves data or changes something without checking who is calling. No scanner reads
for this: the AI review does, on a Canyon where the operator set up an AI model. The review reads
the app's source files and templates. It never reads `.env` files, or dependencies' code under
`node_modules/` or `vendor/`, and it masks every secret gitleaks finds before the model sees the
code.

In an example app, `claims-portal`, the review reports two routes. One serves an org's policies
with no session check, and the middleware leaves `/api/*` out of its matcher:

```text wrap
high  /api/admin/policies is reachable without a session
apps/web/src/app/api/admin/policies/route.ts:8:3

   8 │   return NextResponse.json(await db.policies.forOrg(params.orgId));
```

The other sets a session cookie for any email it is sent:

```
high  /api/session starts a session without checking one
apps/web/src/app/api/session/route.ts:8:3
```

## Why it matters

Anyone who finds the route gets what it returns, or can do what it does, without an account.

## How to fix it

The review writes a fix for each finding. For the first route above it wrote:

> Either add /api/admin/policies to the middleware matcher, or return early from the handler when
> the request has no cp_session cookie. The matcher is the safer of the two — it covers routes
> nobody's written yet.

On a Canyon that signs people in before they reach an app, the finding's title is
`Any signed-in person can reach this route`: only people Canyon signed in reach it, and the route
doesn't check which person is calling. The model's own title starts the problem, so you still see
which route it means. Canyon also replaces the model's fix with its own:

> Don't build a login of your own: Canyon already signs people in. Where this route needs to know or
> check who is calling, read the identity token Canyon sends with every request in the
> Authorization: Bearer header, verify it with CANYON_IDENTITY_ISSUER, CANYON_IDENTITY_AUDIENCE and
> CANYON_IDENTITY_JWKS_URL, and use its sub, email and name. A destructive or admin-only route checks
> a role; an ordinary write records who did it, from the identity.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, the AI review included, without deploying.
A model's answers vary between calls, so another review may find different things, even after a
small change. A blocking finding an earlier review made still blocks while the code it's on hasn't changed,
until the code changes or someone approves it.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies.

On a Canyon with no AI model, the review says the AI review read nothing, and an admin can't set this
rule to Blocking. If the model is removed while the rule is Blocking, every deploy carries one
blocking finding: "Canyon could not check this package with the AI review". A review that didn't
happen never passes a blocking rule.
