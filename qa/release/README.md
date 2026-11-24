# The checks a release needs

CI runs what it can. These are the checks by hand, and what a release must have passed before its tag
([RELEASING.md](../../RELEASING.md)).

- **`checks.txt`**: each check, and the paths whose change calls for it. An `always` check names the paths it answers
  for; `nothing` lines name the paths with nothing to check by hand, and why. Every tracked file is in one.
- **`results/<tag>.txt`**: what was done for a release, a line each: `pass | <id> | <commit> | <note>` or
  `skip | <id> | <commit> | <why>`. `<commit>` is the whole id of the commit on main it was done at. A skip says why.
- **`go run ./internal/tools/release needs <releases.json>`** lists what the changes since the last published release
  call for, from `gh api repos/getcanyon-ai/canyon-releases/releases --paginate --slurp > releases.json`; `needs <from>
  <to>` for any range.

The release refuses its tag when a check its changes call for has no line, or when the check's paths changed after its
commit. An `always` check is stale after any change but to `docs/`, `qa/release/` and `design/`. So commit the results
file last, and tag that commit. A check marked `after` is done once the release is out, in the same file, with the
tagged commit: the next release's tag waits for it.

A fix CI can't test adds its check here in the same pull request.

**Nothing private goes here:** no host, account, person or path. What names one is in the maintainers' private notes.

## The checks

### upgrade

On a real cluster installed from the release before with the Kubernetes manifests, and on one with the Helm chart, an
app in preview and production: upgrade as the docs say. Both answer throughout, then deploy and promote it unchanged.

### rollback

From this release back to the one before, as *Roll back a failed upgrade* says, then forward again. Sign-ins, apps,
secrets and the audit trail are as they were.

### agents

A round of the agent journeys ([qa/agent](../agent/README.md)), its scorecards reviewed.

### sign-in

Real clients sign in ([hand-checks.md](../agent/hand-checks.md)): Claude Code's own `/mcp`, MCP Inspector, Codex.

### windows

Two agent windows on one sign-in, for longer than an access token lasts. Neither is asked to sign in again.

### connector

Canyon as a claude.ai connector, and the web session in hand-checks.md.

### dashboard

Each screen the release changes, by hand, as an owner, a contributor, read-only, no access and an admin.

### screens

Those screens in dark mode and at phone width.

### responder

A security responder on a real Canyon stops production, holds it and lifts it.

### app-sign-in

An app behind Canyon's front door signs a person in on a real Canyon; someone without access is refused.

### github

Connect GitHub on a real Canyon: a push to a linked repository is reviewed, and the review shows on the pull request.

### hand-off

A release handed off to a real platform, through a deploy repository and through a webhook: it runs, and Canyon sees it.

### licence

A valid licence unlocks its features in Settings; a bad or expired one is refused, and what was set keeps working.

### group-sync

A person signs in through a provider that sends groups: a group's grant lets them in, and leaving the group takes it away.

### outbound

An app's calls: allowed and recorded, refused, then allowed by an admin and sent.

### patching

A running app moves onto a newer run image, and one that fails is put back.

### helm

Install the chart by hand on a fresh cluster, then upgrade it from the release before; `telemetry.enabled: false`
leaves the server's environment with `CANYON_TELEMETRY=0`, with `canyon.existingSecret` too.

### docker-demo

`canyon demo up` from the release's own images on a clean machine, the tour, `canyon demo down`.

### docker-team

A team's Canyon on one Docker server, as the docs say: sign-in, a deploy, production, an upgrade from the release before.

### runtime-node

An app on each Node line the builder has builds, starts and answers; the note names the lines.

### runtime-python

The same for each Python line.

### runtime-go

The same for each Go line.

### runtime-java

The same for each Java line, and a native image.

### runtime-ruby

The same for each Ruby line.

### runtime-php

The same for each PHP line.

### runtime-dotnet

The same for each .NET line.

### runtime-static

The same for nginx and httpd.

### telemetry

The collector that runs takes this release's report: its schema is the tagged one, or a newer one that takes it.

### first-report

The first report from a Canyon on this release arrived, with the release's version.
