---
title: Contracts
description: The published specifications for what Canyon sends and receives, each with its JSON schema.
---

A **contract** is the exact shape of data Canyon sends or accepts, written down field by field.
Canyon's tests check every example in a contract against its JSON schema, so what a contract says is
what Canyon does.

| Contract | Read it if you… |
|---|---|
| [Findings](/reference/contracts/finding/) | read Canyon's findings in another tool, or bring your own scanner's reports. A finding is a SARIF 2.1.0 result with a few required fields. The short version is [Finding format](/reference/finding-format/). Schema: [finding.schema.json](/contracts/schema/finding.schema.json). |
| [Provisioner adapter](/reference/contracts/provisioner/) | create apps' databases and storage through your own tooling, such as Crossplane or Terraform, instead of Canyon's built-in Postgres. It is the two HTTPS calls your service answers. See [Databases for your app](/build/databases/#for-platform-teams-your-own-adapter). |
| [Release schema](/contracts/schema/release.schema.json) | run Canyon's apps on your own deployment platform. It is the release Canyon sends, the report your platform sends back, and the webhook settings. See [Your platform runs the apps](/self-host/hand-off/). |

Canyon's other interfaces, such as its HTTP API, aren't published yet. How to sign in to the API for
the calls these pages describe is in [Calling Canyon's API](/reference/api/).
