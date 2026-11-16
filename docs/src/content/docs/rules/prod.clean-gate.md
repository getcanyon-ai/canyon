---
slug: rules/prod.clean-gate
title: "Production needs a passing review"
description: "Only a version whose review passed can be promoted to production."
---

`prod.clean-gate` · Blocking by default · found by Canyon, when you promote

## What it finds

When you run `canyon deploy --env prod`, Canyon reads the newest review of the version preview runs.
The rule passes when that review ended with no blocking finding, or with every blocking finding
approved. It does not pass when:

- the review is still pending or running,
- Canyon's deploy of that version stopped on an error, such as a build that broke,
- the review found blocking findings that no one approved,
- the review named isn't of the version preview runs, or a newer review of that version exists,
- a rule placed before production found blocking findings that no one approved.

Each case gives its own reason on the rule's line of the go-live checks. From Canyon's CLI output:

```
✗ Production needs a passing review
    v1.4's review found 1 blocking finding, not approved
```

When nothing runs in preview, Canyon checks nothing and says why instead, for example "Deploy a new
version to preview to promote it."

## Why it matters

Production would run code that the org's own blocking rules stopped in review.

## How to fix it

Fix the blocking findings and deploy again: the new version's review has to pass before it can be
promoted. Or, if the code is right as it is, [have each blocking finding approved](/build/fix-or-approve/#approve-it-instead).
A review whose blocking findings are all approved passes this rule.

## Check the fix

`canyon status` lists the go-live checks, this rule first, with the reason it passes or not.
`canyon deploy --env prod` prints the same list when it refuses, and promotes nothing.

## If you think it's wrong

This rule has no findings of its own to approve. [Have the review's blocking findings
approved](/build/fix-or-approve/#approve-it-instead) instead.

An admin can set the rule to Advisory or Off under Guardrails → Policies. Advisory lets a promotion
through a review that didn't pass; the go-live checks still show the rule.
