---
slug: rules/sarif.trivy
title: "Findings from trivy"
description: "Trivy's results that no Canyon rule maps, and the switch that decides whether Trivy runs."
---

`sarif.trivy` · Advisory by default · found by Trivy

## What it finds

Trivy's own findings, where no other Canyon rule takes them. Canyon sorts each Trivy result by the
tags of the Trivy rule behind it:

- `vulnerability` goes to [No known-vulnerable dependencies](/rules/deps.known-vuln/)
  (`deps.known-vuln`).
- `misconfiguration` goes to [Infrastructure config passes its benchmark checks](/rules/iac.misconfig/)
  (`iac.misconfig`).
- Any other result stays here, under the Trivy rule's own id.

Trivy runs its vulnerability and misconfiguration scans by default. An admin chooses its scans,
Vulnerabilities, Misconfiguration and Secrets, under Guardrails → Scanners → Trivy → Configure.

This rule's state also decides whether Trivy runs at all: Canyon runs it only while this rule is
Advisory or Blocking, and while **Canyon runs Trivy** is ticked under Configure.

## Why it matters

Each finding states its risk in the Trivy rule's own words.

## How to fix it

Canyon prints the tool's own fix when the rule has one, or a pointer to the Trivy rule when it has
none.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon gives Trivy no ignore file, so a `.trivyignore` doesn't silence a finding. An admin can list
paths to skip under Configure.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Blocking, or to Off under Guardrails → Scanners, which also stops Trivy: then nothing checks
`deps.known-vuln`, and the review says so.

If Trivy can't finish, for example because its vulnerability database is over 5 days old or couldn't
be downloaded, the review carries one blocking finding: "Canyon could not check this package with
trivy".
