---
title: Usage reports
description: Once a day, Canyon sends one short report of counts, never names. It's on by default, and an admin can turn it off.
---

Once a day, a Canyon server sends one short report of counts to Canyon's makers: how many apps it
has, how reviews went, which built-in rules fired. It tells us whether people get from a first review
to production, and where they get stuck. It never names your company, your apps or your people.

It's on by default. Any admin can turn it off.

## What is sent

- **A random install id,** made at the server's first start and never derived from its host.
- **The Canyon version,** the platform it runs on, such as linux/arm64, how it was installed, and whether
  it's Canyon's demo, which is counted apart.
- **How old the install is,** as a range, such as 7–27 days.
- **How many apps** the server has, and how many had a review in the last week and the last month.
- **Reviews since the last report,** usually a day's, by result: passed, passed with advice, blocked.
  Also by who asked for it: a person or a coding agent, and which agent when Canyon can tell.
- **Deploys** to preview and production, reviews and deploys that failed and why, and releases handed to
  your own platform and how they ended.
- **The milestones** the server has reached, and how long after the install started each came, as a
  range: a first review, a first blocked review, a first app that passed after being blocked, a first
  promotion to production.
- **Fixes:** how many blocked apps passed again, and the median minutes and reviews it took.
- **Promotions to production,** and how many a person approved or denied.
- **How many people used Canyon** in the last day, week and month, and how many coding agents'
  sessions. Counted, never named. How many people have access, as a range, such as 6–25.
- **The rules that fired,** counted by their built-in name. A rule your org wrote counts only as
  `custom`.
- **The scanners the server runs,** whether the AI review is on, and the kind of model it uses:
  Anthropic, OpenAI, or none.
- **Which features are on:** sign-in, your own rules (how many, never their names), built-in rules you
  changed, where apps run, database sessions, outbound control, patching, production approvals,
  notification channels, GitHub, sign-in groups, and whether an Enterprise licence is in use. For the
  licence, its state alone.
- **The languages your apps are built in,** from their buildpacks.
- **What people ran into that Canyon doesn't do yet,** by kind ([Ask for a feature](/reference/ask-for-a-feature/)).

[TELEMETRY.md](https://github.com/getcanyon-ai/canyon/blob/main/TELEMETRY.md) lists every field, with an
example of each. It's generated from the same definition the server's report is held to, so the two
never differ.

## Never sent

Code, file names or paths, repo or app names, hostnames, domains, cluster names, user names, emails, IP
addresses, finding text, custom rule names or content, secrets, environment variables, AI review content.
If a field could identify a company or a person, it isn't sent.

## See exactly what is sent

```sh
canyon telemetry show
```

This prints the next report exactly as the server will send it, under one line saying whether it goes
and, if not, why. As an admin you can also see the report, and the last report sent with when, in
**Settings › Telemetry**.

## Turn it off

Any one of these turns it off:

- An admin runs `canyon telemetry off`, or chooses **Turn off** in **Settings › Telemetry**.
  Turning it back on is done there too, in the dashboard. On a Canyon with sign-in,
  `canyon telemetry on` prints `An admin turns telemetry on in Canyon's dashboard: Settings › Telemetry.`
  and exits 2.
- `CANYON_TELEMETRY=0` or `DO_NOT_TRACK=1` in the server's environment. An admin can't turn it back
  on while one is set.
- `telemetry.enabled: false` in the Helm chart's values.
- `CI` set in the server's environment, so test and pipeline installs never send.
- The server can't reach Canyon's collector, `usage.myagentcontrol.ai`, on its first send, as in an air-gapped install. It tries
  once, for 2 seconds, then stays off until an admin turns it back on in **Settings › Telemetry**. It
  never retries, and never logs an error.

Only an admin can see the report or turn it on or off. For anyone else, these commands and the
Settings page are refused. Turning it on is done in the dashboard, by a person: an agent, or an admin's
CLI session on a Canyon with sign-in, is refused.

## A new install id

To give your server a new random id, run `DELETE FROM telemetry;` on Canyon's database. The server
makes a new one the next time it reads it.
