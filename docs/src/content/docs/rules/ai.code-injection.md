---
slug: rules/ai.code-injection
title: "No user input reaching commands, files, pages or URLs (AI review)"
description: "User input reaching commands, files, pages, URLs or code, in the files no scanner rule reads, found by Canyon's AI review."
---

`ai.code-injection` · Advisory by default · found by the AI review, when an AI model is set up

## What it finds

The same thing as [No user input reaching commands, files, pages or URLs](/rules/code.injection/)
(`code.injection`), in the files that rule's scanners can't read. ESLint reads JavaScript and TypeScript, and Opengrep's
injection rules read some other languages. Each app file in a language none of the running rules
read, such as PHP or Ruby, goes to the AI review under this rule instead, if the AI review can read it as text. The AI review checks only those files for it,
never the ones a scanner reads.

It runs only while `code.injection` is on and Canyon runs Opengrep or ESLint. When every app file is
in a language they read, the AI review doesn't check this rule at all.

The review's notes name what it covered, in a line such as `checked by the AI review:
code.injection: PHP (…) — no scanner rule reads PHP`.

## Why it matters

Whoever controls the input can run a command, read a file, put script in another user's page, or
send the app's requests somewhere else.

## How to fix it

The review writes its own fix for each finding. When it gives none, Canyon prints:

> Change this code so it follows "No user input reaching commands, files, pages or URLs (AI
> review)": Canyon's review then no longer reports ai.code-injection here.

The fix Canyon prints for the scanners' rule applies to the same code: "Keep the user's input data:
pass it as an argument or a parameter, never as part of a command, path, page, URL or code, and
check it against the values allowed."

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, the AI review included, without deploying.
A model's answers vary between calls, so another review may find different things, even after a
small change.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies.

With this rule Off, or on a Canyon with no AI model, the files no scanner reads are listed as not
checked under `code.injection`. An admin can't set this rule to Blocking without a model. If the
model is removed while the rule is Blocking, every deploy carries one blocking finding: "Canyon
could not check this package with the AI review".
