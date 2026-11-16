---
slug: rules/secrets.client-bundle
title: "No secrets in the code"
description: "A credential written into the app's source, found by gitleaks before anything is built."
---

`secrets.client-bundle` · Blocking by default · found by gitleaks

## What it finds

A key, token or password written into a file of the app. Canyon runs gitleaks with gitleaks' own
default rules over every file the CLI uploads. Canyon adds no rules of its own: gitleaks decides what
a secret is.

Every gitleaks result lands here as critical, with one exception: gitleaks' catch-all rule
`generic-api-key`, when it fires in a documentation file of prose such as a `README.md`. That one
goes to Findings from gitleaks (`sarif.gitleaks`) instead.

The CLI leaves `.env` files out of the upload, except `.env.example`, so gitleaks reads the code
you ship, not your local settings.

In an example app, `claims-portal`, `apps/web/src/lib/payments.ts` builds a Stripe client with a
key on line 14 shaped like a live one. The review prints it like this, the value masked:

```text wrap
critical  Found a Stripe Access Token, posing a risk to payment processing services and sensitive financial data
apps/web/src/lib/payments.ts:14:1

  14 │ ••••
```

Canyon never prints the secret's value.

## Why it matters

Anyone who can read the code or the built app can use the key. Removing it from the code doesn't
undo that: it has already leaked.

## How to fix it

This is the fix Canyon prints, with the variable name it suggests:

> Rotate this credential — it has been in your code. If nothing reads it, delete it from the code;
> otherwise store the new one with canyon secret set STRIPE_ACCESS_TOKEN and read it from the
> environment.

1. Revoke the old value with whoever issued it, and make a new one.
2. If nothing reads it, delete it from the code.
3. Otherwise change the code to read it from an environment variable, and store the new value:

   ```
   canyon secret set STRIPE_ACCESS_TOKEN
   ```

   It reads the value from a prompt or a pipe, never from the command line. Any variable name works;
   the one in the finding is a suggestion.

Moving the value into a `.env` file, a comment, a config file or a client-side variable such as
`NEXT_PUBLIC_*` or `VITE_*` doesn't fix it. That is the same secret in a different place.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

A `gitleaks:allow` comment doesn't silence it: Canyon tells gitleaks to ignore those comments.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Advisory or Off under Guardrails → Policies.

gitleaks runs only while [Findings from gitleaks](/rules/sarif.gitleaks/) (`sarif.gitleaks`) is
Advisory or Blocking. Set to Off, nothing checks this rule, and the review says so.
