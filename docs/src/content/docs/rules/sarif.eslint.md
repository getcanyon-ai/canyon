---
slug: rules/sarif.eslint
title: "Findings from eslint"
description: "The switch that decides whether ESLint runs; ESLint's results themselves go to the injection rule."
---

`sarif.eslint` · Advisory by default · found by ESLint

## What it finds

This rule would hold ESLint's results that no other Canyon rule takes. But every ESLint result goes
to [No user input reaching commands, files, pages or URLs](/rules/code.injection/)
(`code.injection`), so in practice this rule holds no findings of its own.

What it does is decide whether ESLint runs at all. Canyon runs ESLint only while this rule is
Advisory or Blocking, and while **Canyon runs ESLint** is ticked under Guardrails → Scanners →
ESLint → Configure. Canyon's ESLint runs four rules, fixed in its own config, over the app's
JavaScript and TypeScript files outside `node_modules`: `security/detect-child-process`,
`security/detect-non-literal-fs-filename`, `no-eval` and `no-new-func`. The app's own ESLint config
is never read.

## Why it matters

Set to Off, ESLint doesn't run, and `code.injection` loses the checks Canyon added for the
JavaScript and TypeScript flaws that Opengrep's bundled rules miss.

## How to fix it

Fix the finding under `code.injection`, as Canyon's fix there says.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon's ESLint ignores `eslint-disable` comments and other inline config. An admin can list paths
to skip under Configure.

An admin can set this rule to Off under Guardrails → Scanners, which stops ESLint. If ESLint can't
finish, the review carries one blocking finding: "Canyon could not check this package with eslint",
which can be [approved](/build/fix-or-approve/#approve-it-instead) like any other.
