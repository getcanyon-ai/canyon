---
slug: rules/perf.unbounded-refetch
title: "No unbounded refetch on user input"
description: "Code that reads everything again on every keystroke, found by Canyon's AI review."
---

`perf.unbounded-refetch` · Advisory by default · found by the AI review, when an AI model is set up

## What it finds

A list that reads everything again on every keystroke. No scanner reads for this: the AI review
does, on a Canyon where the operator set up an AI model. The review reads the app's source files and
templates, and masks every secret gitleaks finds before the model sees the code.

In an example app, `claims-portal`, the claims table refetches on every change to the search box:

```
medium  ClaimsTable refetches every claim on each keystroke
apps/web/src/components/ClaimsTable.tsx:61:3

  61 │   useEffect(() => { refetch(); }, [query]);
```

## Why it matters

Each character typed is a full read of the data behind the list. That is fine on a small dataset and
slow on a real one.

## How to fix it

The review writes a fix for each finding. For the table above it wrote:

> Debounce to ~250ms, or move the filter server-side. Neither is urgent.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, the AI review included, without deploying.
A model's answers vary between calls, so another review may find different things, even after a
small change. A blocking finding an earlier review made still blocks while the code it's on hasn't changed,
until the code changes or someone approves it.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies.

On a Canyon with no AI model, the AI review reads nothing, and an admin can't set this rule to
Blocking. If the model is removed while the rule is Blocking, every deploy carries one blocking
finding: "Canyon could not check this package with the AI review".
