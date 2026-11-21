---
title: Calling Canyon's API
description: How an admin's script signs in to Canyon's HTTP API, which calls a CLI sign-in can make, and which need the dashboard.
---

A few pages send you to Canyon's HTTP API: setting up [hand-off](/self-host/hand-off/) from a script,
[holding production](/org/who-can-do-what/#stopping-production),
[making a rule stricter for one app](#make-a-rule-stricter-for-one-app), and
[reading a review as SARIF](/reference/finding-format/#read-a-review-as-sarif). The API is under
`/api/v1` on your Canyon's address. Its routes aren't a published [contract](/reference/contracts/)
yet. This page covers how to sign in to it, and which calls only the dashboard can make.

## Sign in with the CLI

The API takes the same sign-in as the dashboard and the CLI: a Canyon session, sent as a cookie.
People get no separate API keys. For a script, use the session `canyon login` gave your machine.

1. Sign in: `canyon login https://canyon.example.com`, and approve the code in the dashboard.
2. Read the session from the CLI's config file, `~/.config/canyon/config.json`, or
   `$XDG_CONFIG_HOME/canyon/config.json` when that variable is set. The file holds the Canyon you
   signed in to last, in its `host` field.

   ```sh
   CANYON_URL=https://canyon.example.com
   session=$(jq -r .session ~/.config/canyon/config.json)
   ```

3. Send the session as the cookie. Its name follows the address you open Canyon at:
   `__Host-canyon_session` when it starts with `https://`, and `canyon_session` when it starts with
   `http://`, as on a laptop.

   ```sh
   curl -sS "$CANYON_URL/api/v1/policies" -b "__Host-canyon_session=$session"
   ```

A session acts as you. Each call is checked against your role and your level on the app, as the
dashboard's would be, and the audit trail records it as yours, made from the CLI. A coding agent using
your CLI sign-in is you too.

A CLI session lasts 7 days from `canyon login`, a dashboard session 12 hours. A session ends sooner when:

- you sign out: `canyon logout` ends the CLI's;
- you're taken off the org's membership lists, at your next call;
- an admin ends every session of yours with `DELETE /api/v1/sessions?subject=<your address or subject>`.

Canyon refuses a change (`POST`, `PUT`, `PATCH` or `DELETE`) that a browser reports came from another
site, with `403 cross_origin`. A script sends no such header, so this never applies to it.

### With sign-in off

On a Canyon with sign-in off, such as the one on your laptop, every caller is the admin and needs no
session. Nothing tells a browser from a script there, so no call below is refused for coming from a
script. Canyon has no CLI sign-in codes to approve, and no GitHub accounts to link, without sign-in.

## Tokens that aren't a sign-in

Under hand-off, Canyon gives your deployment platform a **host token**, and under a deploy repo a
**fetch-only token** too. Only an admin makes them, by saving the platform webhook or the deploy repo,
or by replacing one. Your platform sends them to fetch each release's access token and to report back,
and they do nothing else. They can't make any call on this page.

## What a CLI sign-in can do

An admin's CLI sign-in makes these calls, as the dashboard would make them for you.

| Call | What it does |
|---|---|
| `PUT /api/v1/preferences` | The org's settings, except the few in [What needs the dashboard](#what-needs-the-dashboard). |
| `PUT /api/v1/platform-repo`, `POST /api/v1/platform-repo/check`, `…/token`, `…/fetch-token`, `…/host-token` | Hand-off's [deploy repo](/self-host/hand-off/). |
| `PUT /api/v1/platform-webhook`, `POST /api/v1/platform-webhook/ping`, `…/token`, `…/secret` | Hand-off's [platform webhook](/self-host/hand-off/). |
| `PUT /api/v1/policies/<rule>`, `PUT /api/v1/custom-rules/<slug>` | A rule's state, and a custom rule. Not while production approval is on. |
| `POST /api/v1/projects/<app>/envs/prod/hold` | Stops an app's production and holds it. The body is `{"reason": "…"}`. A security responder can make this call too. |
| `PUT /api/v1/projects/<app>/policies/<rule>`, `DELETE …` | Makes a rule [stricter for one app](#make-a-rule-stricter-for-one-app). The app's owners can make this call too. |
| `DELETE /api/v1/sessions?subject=<address or subject>` | Ends every session of one person, such as when someone leaves. |

### Make a rule stricter for one app

An app's owner or an admin adds to a rule for one app alone. The body names the state before preview,
`state`, the state before production, `production`, or both, each `off`, `advisory` or `blocking`, and
each must be stricter than the org's:

```sh
curl -sS -X PUT "$CANYON_URL/api/v1/projects/claims-portal/policies/deps.known-vuln" \
  -b "__Host-canyon_session=$session" -H 'Content-Type: application/json' -d '{"state": "blocking"}'
```

Without `?apply=true`, the call only previews: it answers whether the version the app would promote
has findings the addition would block, and saves nothing. Add `?apply=true` to save it. `DELETE` on the same address takes the
addition away.

## Canyon's MCP server

Coding agents in the cloud reach Canyon over MCP at `/mcp`, with a token from Canyon's own OAuth
sign-in ([Connect your agent](/start/connect-your-agent/#in-the-cloud)). The agent finds every
address below on its own, from the first. They are served only with sign-in on.

| Address | What it is |
|---|---|
| `POST /mcp` | Canyon's MCP server, over MCP's streamable HTTP transport. It takes a bearer token from `/oauth/token`, and each tool is checked as the matching `/api/v1` call would be, as the person who allowed the agent. With sign-in off it answers `404`, `mcp_needs_sign_in`. |
| `GET /.well-known/oauth-protected-resource/mcp` | Where `/mcp` gets its tokens, and its two scopes, `read` and `act` |
| `GET /.well-known/oauth-authorization-server` | Canyon's OAuth addresses |
| `GET /oauth/authorize` | Opens the consent page in the browser. It requires PKCE with `S256` and `resource=<your Canyon>/mcp`. |
| `POST /oauth/token` | Exchanges the code for a token that lasts an hour, and refreshes it. The connection lasts 30 days. |
| `POST /oauth/register` | Registers a client that has no client ID document of its own. Public clients only, with no secret. |

An agent's calls are recorded in the audit trail as yours, made through MCP. A script uses a CLI
sign-in, as above, not these addresses.

## What needs the dashboard

On a Canyon with sign-in on, these calls take a dashboard session only. A CLI sign-in, and so a coding
agent, gets `403 dashboard_required`, with a message that says where in the dashboard to go.
[Who can do what](/org/who-can-do-what/#what-only-the-dashboard-can-do) says why each one is there.

| Call | What it is |
|---|---|
| `GET /api/v1/device-grants/<code>`, `POST …/approve`, `POST …/deny` | Approving or denying a CLI sign-in code |
| `POST /api/v1/promotion-approvals/<code>/approve`, `POST …/deny` | Deciding a promotion request |
| `PUT /api/v1/preferences` naming `prod_approval` or `push_approvers_linked` | Production approval, and **Approvers of a push must link GitHub** |
| `PUT /api/v1/egress`, `POST /api/v1/egress/networks/recreate` | How apps call out, and recreating apps' networks |
| `PUT /api/v1/projects/<app>/egress/<env>/mode` to `observe` or to `null`, `PUT /api/v1/projects/<app>/egress/<env>/destinations/<destination>` | Widening what one app may call |
| `DELETE /api/v1/projects/<app>/envs/prod/hold` | Lifting a hold on production |
| `POST /api/v1/gate-runs/<run>/findings/<fingerprint>/approve`, `POST /api/v1/gate-runs/<run>/findings/approve`, for a blocking finding | Approving a blocking finding |
| `GET /api/v1/github/link` | Linking your GitHub account |
| `POST /api/v1/oauth/requests/<request>/approve`, `POST …/deny` | Allowing or refusing a coding agent's connection |
| `DELETE /api/v1/sessions/<session>` | Ending one session, such as disconnecting an agent |

While production approval is on, these move to the dashboard too:

| Call | What it is |
|---|---|
| `PUT /api/v1/policies/<rule>`, `PUT /api/v1/custom-rules/<slug>` | A rule's state, and a custom rule |
| `PUT /api/v1/preferences` naming `approved_buildpacks` or `design_system_id`, `POST /api/v1/guardrails/setup` | The approved buildpacks, the design system, and **Set up with Canyon** |
| `POST /api/v1/gate-runs/<run>/findings/<fingerprint>/approve`, `POST /api/v1/gate-runs/<run>/findings/approve` | Approving any finding |
| `POST /api/v1/projects/<app>/db-sessions` that writes production data | Opening a session that can change production's data |

`POST /api/v1/projects/<app>/promotions` from a CLI sign-in then files a promotion request for a
person to decide in the dashboard, instead of promoting.

## Errors

Every refusal is JSON, `{"error": "<code>", "message": "…"}`:

| Status and code | Why |
|---|---|
| `401 unauthenticated` | No session, or it expired or was ended. Run `canyon login` again. |
| `403 admin_required` | Only an admin makes this call. The message names an admin to ask. |
| `403 dashboard_required` | Only the dashboard makes this call ([above](#what-needs-the-dashboard)). |
| `403 cross_origin` | A browser sent the change from another site. |
| `409 setup_required` | Nobody has finished setting up this Canyon. Until then it answers only the org's settings, `GET` and `PUT /api/v1/preferences`. |
