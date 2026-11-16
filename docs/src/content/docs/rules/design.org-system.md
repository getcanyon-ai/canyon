---
slug: rules/design.org-system
title: "Apps use the org's design system"
description: "An app whose package.json doesn't list the org's design system, found by Canyon's review."
---

`design.org-system` · Advisory by default · found by Canyon, from the app's package.json

## What it finds

An app that doesn't use the design system the org picked: the shared UI components every app should
build from. An admin picks it under Guardrails → What agents are told → Design system, and agents
are handed its name, package and registry.

Every review reads each `package.json` in the app, wherever it is: the UI often lives in a folder of
its own, such as `web/` or `client/`, beside a server's `package.json` at the root. Nothing under
`node_modules/`, `vendor/` or `.git/` is read. The app passes when any of them lists the design
system's package, for example `@meridian/ui`, under `dependencies` or `devDependencies`.

Canyon's two built-in design systems aren't npm packages: agents copy their components in with the
shadcn CLI, which writes a `components.json`. So for **shadcn/ui** the app passes when it has a
`components.json`, and for **Canyon** when a `components.json` lists the `@canyon` registry.

An app that doesn't pass gets one finding, on the `package.json` nearest the app's root, at its
`"dependencies"` line. Its problem line names every `package.json` Canyon read:

```
The app doesn't use Meridian UI, the org's design system
package.json:5
The org's design system is Meridian UI (@meridian/ui), and no package.json in this app lists it.
Canyon read package.json and web/package.json.
```

The review says the rule was not checked, and never passes it, when the app has no `package.json`
(a Python or Go app, for example), and names each `package.json` that isn't valid JSON, which it
skips.

The rule checks nothing until you pick a default design system. Until then the review says nothing
about it, and its row under Guardrails → Policies says "Checks nothing until you pick a default design
system", linking to the picker.

## Why it matters

Apps built outside the org's design system look and behave unlike the rest of the org's apps.

## How to fix it

This is the fix Canyon prints for a design system that is an npm package:

> Add @meridian/ui to the dependencies of the package.json that holds the UI (registry:
> https://registry.meridian.example/ui), and build the UI from its components.

For shadcn/ui, it says to run `npx shadcn@latest init` where the UI's `package.json` is. For
Canyon, it says to add the `@canyon` registry to the UI's `components.json`.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can pick another default design system, or set the rule to Off or Blocking under Guardrails → Policies. Set to Blocking, its finding stops a
deploy.
