---
slug: rules/sarif.gitleaks
title: "Findings from gitleaks"
description: "gitleaks' results that no Canyon rule takes, and the switch that decides whether gitleaks runs."
---

`sarif.gitleaks` · Advisory by default · found by gitleaks

## What it finds

gitleaks' own findings, where [No secrets in the code](/rules/secrets.client-bundle/)
(`secrets.client-bundle`) doesn't take them. Canyon runs gitleaks, with its default rules, inside
the Canyon server. Every gitleaks result goes to
`secrets.client-bundle` except one kind: a match of gitleaks' catch-all rule `generic-api-key` in a
documentation file of prose, such as a `README.md`. That one stays here: rated critical, but this rule is Advisory by default, so it doesn't block.

For example, the stock NestJS starter's `README.md` carries a placeholder token in its
CircleCI badge link. It lands here as `sarif.gitleaks.generic-api-key`, a warning that doesn't block.
The same pattern in code, a `License.tsx` or a docs site's `config.ts`, goes to
`secrets.client-bundle`, and so does a key whose format names its provider, such as a Stripe key,
even in a documentation file.

This rule's state also decides whether gitleaks runs at all: Canyon runs it only while this rule is
Advisory or Blocking.

## Why it matters

If the value in the documentation is a real credential, anyone who reads the file can use it.

## How to fix it

If the value is a placeholder, nothing needs fixing. If it is a real credential, treat it as a
leaked secret: revoke it, make a new one, and keep the new one out of the file. Canyon prints the
tool's own fix when the rule has one, or a pointer to the gitleaks rule when it has none.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule under Guardrails → Policies. Blocking makes these documentation findings block, since they are
rated critical. Off stops gitleaks: then nothing checks `secrets.client-bundle`, and the review says
so.
