---
slug: rules/deps.known-vuln
title: "No known-vulnerable dependencies"
description: "A dependency pinned at a version with a published security advisory, found by Trivy."
---

`deps.known-vuln` · Advisory by default · found by Trivy

## What it finds

A package the app depends on, at a version a published advisory names. Canyon runs Trivy over the
app's files, and every result of a Trivy rule tagged `vulnerability` lands here, at the severity
Trivy rates it.

For example, a `package-lock.json` that pins `lodash` 4.17.20 gets, among others,
CVE-2021-23337: high, fixed in 4.17.21.

Two things Trivy can't read, and the review names them as not checked under this rule:

- `npm-shrinkwrap.json`.
- A line of a `requirements*.txt` that pins no version with `==`.

## Before production: what the build added

A build can add a package no lockfile names: an npm `postinstall` that downloads one, a pip build, a
Maven plugin that fetches a jar, or a buildpack the app names in its `project.toml`. A Maven app whose
parent or BOM lives in a remote repository can also hold versions the review couldn't work out. The
review never builds the app, so it can't see these.

When an admin places this rule before production, Canyon also checks the app as it was built, the
version preview runs, before it's promoted. Each package there with a high or critical advisory that
the review didn't report is counted on this rule's line of the go-live checklist, as your team's to
fix:

> lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21 fixes it

A package the review already reported isn't counted again. Canyon keeps what the review found, not
every package your lockfiles name, so a package your lockfile names that gets a new advisory after its
review is counted on this line too, until the app is reviewed again: the next review reports it as an
ordinary finding.

Until Canyon has checked the built app, the line reads "could not be checked" and doesn't pass. Canyon
checks it once the version is in preview, and again each time its vulnerability database is updated.
When an admin places the rule before production, Canyon checks the apps' versions in preview straight
away. If the change is scheduled for later, the check waits for Canyon's next scheduled check, up to 6
hours by default, or for the app's next deploy.

## Why it matters

The flaw the advisory describes ships with the app, and anyone who reads the advisory knows how to
use it.

## How to fix it

This is the fix Canyon prints:

> Upgrade the package to a version the advisory lists as fixed, then update the lockfile.

Each finding names the installed version and the version that fixes it.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
A package only the built app holds is approved the same way, from the dashboard: the go-live check's row
lists each one with **Approve**, for an admin, or a responder who didn't start the review. An approval
covers that package's name and version across rebuilds. If your lockfile names
the package and it got its advisory after the app's review, review it again: deploy it again, and the
new review reports it as an ordinary finding, which you fix or have approved as any other.
An admin can set the rule to Off or Blocking under Guardrails → Policies. Set to Blocking, it stops a deploy for its high
and critical findings.

Trivy runs only while [Findings from trivy](/rules/sarif.trivy/) (`sarif.trivy`) is Advisory or
Blocking, Canyon runs it on your Canyon, and its vulnerability scan is on. If Trivy can't finish, for example because its
vulnerability database is over 5 days old, the review carries one blocking finding: "Canyon could
not check this package with trivy".
