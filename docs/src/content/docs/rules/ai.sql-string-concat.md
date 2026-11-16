---
slug: rules/ai.sql-string-concat
title: "No hand-built SQL from user input (AI review)"
description: "SQL built from user input, in the files Opengrep's SQL rules can't read, found by Canyon's AI review."
---

`ai.sql-string-concat` · Advisory by default · found by the AI review, when an AI model is set up

## What it finds

The same thing as [No hand-built SQL from user input](/rules/sql.string-concat/)
(`sql.string-concat`), in the files that rule's scanner can't read. Opengrep's SQL rules read some languages and not others. Each app file in a
language they don't read, such as PHP or Ruby, goes to the AI review under this rule instead, if the AI review can read it as text. The AI
review checks only those files for it, never the ones Opengrep reads.

It runs only while `sql.string-concat` is on and Canyon runs Opengrep. When every app file is in a
language Opengrep reads, the AI review doesn't check this rule at all.

The review's notes name what it covered, in a line such as `checked by the AI review:
sql.string-concat: PHP (…) — no scanner rule reads PHP`.

## Why it matters

Whoever sends the request can rewrite the statement: read other rows, change them, or drop the table.

## How to fix it

The review writes its own fix for each finding. When it gives none, Canyon prints:

> Change this code so it follows "No hand-built SQL from user input (AI review)": Canyon's review
> then no longer reports ai.sql-string-concat here.

The fix Canyon prints for the scanner's rule applies to the same code: "Pass the user's input to the
query as a bound parameter instead of building the SQL text from it."

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, the AI review included, without deploying.
A model's answers vary between calls, so another review may find different things, even after a
small change.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies.

With this rule Off, or on a Canyon with no AI model, the files Opengrep can't read are listed as not
checked under `sql.string-concat`. An admin can't set this rule to Blocking without a model. If the
model is removed while the rule is Blocking, every deploy carries one blocking finding: "Canyon
could not check this package with the AI review".
