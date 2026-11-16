---
slug: rules/sarif.grype
title: "Findings from grype"
description: "Grype's results, under one rule of their own, and the switch that decides whether Grype runs. Off until an admin adds it."
---

`sarif.grype` · not on until an admin adds it · found by Grype

## What it finds

Grype's findings: packages the app depends on that have a published advisory. Every Grype result
stays here, under the Grype rule's own id. No other Canyon rule takes them;
Trivy's vulnerability findings go to [No known-vulnerable dependencies](/rules/deps.known-vuln/)
(`deps.known-vuln`) instead, so with both tools running the same advisory can show twice, once
under each rule.

Grype isn't one of the rules Canyon ships with. An admin adds it under Guardrails → Scanners →
Grype: choose a state, tick **Canyon runs Grype** under Configure, and save. It is added Off
unless they choose Advisory or Blocking.

This rule's state also decides whether Grype runs at all: Canyon runs it only while this rule is
Advisory or Blocking, and while **Canyon runs Grype** is ticked.

## Why it matters

Grype matches the app's packages against its own advisory database. Each finding states its risk in
the advisory's own words.

## How to fix it

Upgrade the package to a version the advisory lists as fixed, then update the lockfile. Canyon
prints Grype's own fix when the rule has one.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon gives Grype its own configuration, so a `.grype.yaml` in the app doesn't silence a finding.
An admin can list paths to skip under Configure.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Blocking, or to Off under Guardrails → Scanners, which also stops Grype.

If Grype can't finish, for example because its vulnerability database is over 5 days old or couldn't
be downloaded, the review carries one blocking finding: "Canyon could not check this package with
grype".
