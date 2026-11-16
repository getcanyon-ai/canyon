---
slug: rules/sarif.checkov
title: "Findings from checkov"
description: "The switch that decides whether Checkov runs; Checkov's results themselves go to the infrastructure rule."
---

`sarif.checkov` · Advisory by default · found by Checkov

## What it finds

This rule would hold Checkov's results that no other Canyon rule takes. But every Checkov result goes
to [Infrastructure config passes its benchmark checks](/rules/iac.misconfig/) (`iac.misconfig`), so
in practice this rule holds no findings of its own.

What it does is decide whether Checkov runs at all. Canyon runs Checkov only while this rule is
Advisory or Blocking, and while **Canyon runs Checkov** is ticked under Guardrails → Scanners →
Checkov → Configure. There an admin also chooses its frameworks, Terraform, CloudFormation and
Dockerfile by default, the checks to skip, and the paths to skip.

## Why it matters

Set to Off, Checkov doesn't run, and no review reads the app's Terraform, CloudFormation or
Dockerfiles.

## How to fix it

Fix the finding under `iac.misconfig`. Canyon prints Checkov's own guidance for each check.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon doesn't read `checkov:skip` comments or a `.checkov.yaml` in the app. To skip a check for
every app, an admin lists it under Configure.

An admin can set this rule to Off under Guardrails → Scanners, which stops Checkov: Terraform,
CloudFormation and Dockerfiles go unchecked. If Checkov can't finish, the review carries one
blocking finding: "Canyon could not check this package with checkov", which can be
[approved](/build/fix-or-approve/#approve-it-instead) like any other.
