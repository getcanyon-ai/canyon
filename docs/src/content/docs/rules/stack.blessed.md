---
slug: rules/stack.blessed
title: "Approved stacks only"
description: "Every buildpack that built the version in preview is one the org approved."
---

`stack.blessed` · Advisory by default · found by Canyon, when you promote

## What it finds

Canyon builds apps with buildpacks: build steps that detect the app's language and install what it
needs. Canyon records which ones each build ran. When you run `canyon deploy --env prod`, this rule
checks the build of the version preview runs. It passes when every buildpack that build ran is on
the org's list of approved buildpacks. Its line in the go-live checks names them: "built with …" when it passes, "not approved: …" with the ones
that aren't when it doesn't. When Canyon has no record of which buildpacks the build ran, it reads
"no stack recorded" and doesn't pass.

Until an admin approves at least one buildpack, the rule has nothing to check against. It reads "no
approved stacks set" and never blocks, whatever its state. This is what Canyon's CLI shows on a
Canyon where none is set:

```
· Approved stacks only
    no approved stacks set
```

The **Approved stacks** list of languages and frameworks is a different thing: it tells coding
agents what to use and blocks nothing. This rule reads the approved buildpacks alone.

## Why it matters

An app built on a stack the org didn't approve would reach production without anyone choosing that stack.

## How to fix it

Build the app on an approved stack, so that its build runs only approved buildpacks, and deploy it
to preview again. Or, if the stack is fine, have an admin approve its buildpacks under Guardrails →
What agents are told → Approved buildpacks. That list shows every buildpack Canyon has seen an app
build with.

## Check the fix

`canyon status` lists the go-live checks with this rule's line. `canyon deploy --env prod` prints
the same list when it refuses.

## If you think it's wrong

This rule has no findings to approve. An admin can approve the buildpack, or set the rule to Off or
Blocking under Guardrails → Policies. As Advisory, a line that doesn't pass shows in the go-live
checks and the promotion goes ahead.
