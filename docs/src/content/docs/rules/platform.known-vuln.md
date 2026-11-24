---
slug: rules/platform.known-vuln
title: "No known-vulnerable system libraries or build tools"
description: "The image preview runs holds no known-vulnerable package that Canyon put there and can fix."
---

`platform.known-vuln` · Off by default · found by Canyon, when you promote

**Beta.** This works, but it isn't complete or fully tested yet. Tell us what you find. The dashboard marks
this rule's rows `Beta`, and the CLI says `(beta)`.

## What it finds

An app's image holds three kinds of package. Canyon supplies the system libraries the app runs on,
such as OpenSSL. Canyon's build tools install Node, Python or Java, with what ships alongside them,
and the launcher that starts the app. The app supplies its own code and dependencies. This rule
looks only at what Canyon put there: the system libraries and the build tools.

When you run `canyon deploy --env prod`, the rule reads the newest vulnerability scan Canyon stored
for each layer of the image preview runs. It counts the high and critical vulnerabilities that already
have a published fix in a package Canyon can replace.

It doesn't pass in these cases:

- **It finds one.** Its line says how many packages.
- **No image scan is stored yet.** Its line reads "no image scan recorded".
- **Canyon can't trust what it has.** Its line reads "could not check: …" when that happens:
  - a layer was never matched;
  - the database its newest match used is more than 5 days old;
  - its last rescan didn't finish, so what it matched before is no answer now;
  - what Canyon stored of the image doesn't read whole;
  - the scan listed none of Canyon's packages.

A package with no published fix doesn't count, and neither does one Canyon can't tell the origin of.
Each becomes a note under the line. Your app's own dependencies are never counted here:
[`deps.known-vuln`](/rules/deps.known-vuln/) reads those at review and, placed before production, counts
what the build added that the review didn't see, a package of unknown origin among them.

**When it scans.** While the rule is on, each deploy to preview lists the packages in its image once,
and Canyon checks them against its vulnerability database straight away, so a promotion waits only
that long. While the rule is on, every `CANYON_PATCH_INTERVAL` (6 hours by default), Canyon also
checks the images that run, and the ones kept for a rollback, against its newest vulnerability
database, and lists any running image it hasn't listed yet, such as one deployed before you turned
the rule on. While the rule is Off, the default, Canyon lists and checks no image at all.

## Your own image scanner as an added source

If your organization already scans images with its own tool (Wiz, Prisma Cloud, Snyk, Amazon Inspector),
an admin can add that tool's reports to what this rule reads. First add the tool under Guardrails, then pick
it as the rule's source. Canyon's own scan always counts as well: the rule counts what either finds, and a
report that finds nothing never hides what Canyon found. Canyon also uses its own scan to tell which part
of the image each package is in.

Attach the tool's SARIF report to the image with
`PUT /api/v1/projects/<app>/images/<sha256:digest>/reports`. Anyone on the app's team with contributor
access or above can attach it. SARIF has no field for a package's fixed version or the part of the image
it's in, so each result names its package under `properties.canyon.package`:

```json
{ "ruleId": "CVE-2024-5535", "level": "error", "message": { "text": "CVE-2024-5535 in libssl3" },
  "properties": { "canyon": { "package": {
    "name": "libssl3", "version": "3.0.13-0ubuntu3.4", "fixed": "3.0.13-0ubuntu3.5",
    "layer": "sha256:…", "purl": "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04" } } } }
```

Map your tool's own fields into it when you export: in a Trivy report they are `PkgName`, `InstalledVersion`,
`FixedVersion`, `Layer.DiffID` and `PkgIdentifier.PURL`. A report with a result that names no package is
refused whole.

The rule then reads the newest report of each of the tool's categories for the image preview runs, beside
Canyon's own scan. A result whose fix version is missing, or that Canyon can't place in the image, becomes a
note rather than a count. Without a `purl`, a result in the system libraries is a note too. Its lines:

- "could not check: no <tool> report is attached to this image yet" until one is attached;
- "could not check: its <tool> report for <category> is <n> days old: attach a newer one" once any
  category's newest is more than 5 days old, clean or not, naming the category to attach again ("its <tool>
  report is <n> days old" for a report with no category);
- a note, "Canyon can't tell which part of the image it's in", for a result Canyon can't place.

Canyon's own patches still plan from its own scan alone.

## Why it matters

An app's team can fix only the code and dependencies they wrote. A vulnerable OpenSSL in the system
libraries, or an old npm that comes with Node, reaches production unless whoever runs Canyon updates it.

## How to fix it

You don't change the app for this. Canyon fixes a system library by moving the app onto newer
system libraries, and a package that came with its build tools in its next build tools update. Ask
whoever runs Canyon when that update comes.

**Rebuilds (beta).** When a newer Node.js, Python, Java or Go release fixes a package that came with
the build tools, Canyon can rebuild the app from the same code and the same dependencies on preview
and watch it. If anything else differs (the code, the dependencies the build resolved, or the build
settings), it stops and deploys nothing. Production is always asked: a person on the app's team
approves it in the dashboard, and production gets the image preview ran. An admin turns this on under
Settings › App hosting › Patching › **Rebuilds**: off by default, it needs patching on.

## Check the fix

`canyon status` lists the go-live checks with this rule's line while the rule is on. On the app's page in
the dashboard, a contributor and above sees the **System libraries** card: for preview and production,
what Canyon counts and where each package is, the notes, and what Canyon's fixes changed.

## If you think it's wrong

An admin sets the rule to Off, Advisory or Blocking under Guardrails → Policies. While it is Off, it
isn't in the go-live checks. As Advisory, a line that doesn't pass shows there and the promotion
goes ahead.
