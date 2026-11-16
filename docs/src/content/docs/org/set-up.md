---
title: Sign-in, the AI model and keys
description: Turn on sign-in through your identity provider, give the review a model, and rotate the encryption key that protects stored secrets.
---

These are settings on the Canyon server, set in its environment. On Kubernetes they go in the
`canyon-server` Secret; the [install guide](/self-host/kubernetes/)
lists every other one.

## Sign-in

Canyon signs people in through your own identity provider, from your issuer's own discovery
document. It never sees a password and keeps none of your provider's tokens: it makes a session of its
own.

```mermaid title="Signing in to Canyon: your identity provider says who the person is, Canyon makes its own session"
sequenceDiagram
  participant B as Browser
  participant C as Canyon
  participant I as Your identity provider
  B->>C: Open Canyon
  C-->>B: Continue to company sign-in
  B->>I: Sign in
  I-->>B: Back to /auth/callback, with a code
  B->>C: /auth/callback
  C->>I: Exchange the code for an ID token
  Note over C: Is this address allowed to sign in?
  C-->>B: A Canyon session, for 12 hours
```

The CLI signs in with a code a person approves in the browser
([Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/#2-sign-in)). The three
variables Canyon always needs:

```sh
CANYON_OIDC_ISSUER=https://login.microsoftonline.com/<tenant>/v2.0
CANYON_OIDC_CLIENT_ID=<the app registration's client id>
CANYON_ADMINS=you@yourcompany.example
```

- Register `<CANYON_PUBLIC_URL>/auth/callback` as the client's redirect URI, and set
  `CANYON_PUBLIC_URL` to the `https` address people open Canyon at. Without it, or with an `http`
  address on any host but this machine's own, Canyon refuses to start. Canyon asks for the scopes
  `openid profile email`. If the client has a secret, set
  `CANYON_OIDC_CLIENT_SECRET`; without one, Canyon signs in as a public client with PKCE.
- `CANYON_ADMINS` are the first admins, the only people who can sign in until an admin finishes
  setup. [Who can do what](/org/who-can-do-what/) lists what admins, members and each level on an
  app may do.
- On the setup screen, the first admin says who else may sign in: a domain, a list of addresses, or
  both. To fix them in the environment instead, set `CANYON_OIDC_ALLOWED_DOMAINS`,
  `CANYON_OIDC_ALLOWED_EMAILS`, or both. The screen then shows them read-only, and with both set,
  every `CANYON_ADMINS` address must be on one of them or Canyon refuses to start.
- Entra, Okta, Google Workspace and any other OIDC provider take the same values. Canyon checks the
  issuer when it starts and refuses to start on a bad one, rather than starting and letting nobody
  in.
- `CANYON_AUTH` is `oidc` when unset. To run Canyon for yourself on one machine without sign-in, set
  `CANYON_AUTH=off`: everyone who reaches it is an admin, so Canyon refuses to start unless
  `CANYON_ADDR` and `CANYON_PUBLIC_URL` are both loopback. The laptop demo comes with sign-in off
  ([how](/start/try-it-on-your-laptop/#sign-in-is-off)).

## The AI model

Without a model, the scanners still run and the AI review reports itself as skipped on every run. It
never passes silently. On the Claude API, a model is one variable:

```sh
ANTHROPIC_API_KEY=<your key>
CANYON_MODEL=claude-opus-5   # the default; the id as your provider names it
```

- **A gateway that speaks the Claude API**, including one in front of Bedrock or Vertex: set
  `CANYON_MODEL_URL`, with `ANTHROPIC_API_KEY` or `CANYON_MODEL_TOKEN`. Canyon doesn't call Bedrock
  or Vertex directly: `CANYON_MODEL_PROVIDER=bedrock` or `vertex` refuses to start.
- **OpenAI:** `CANYON_MODEL_PROVIDER=openai`, `OPENAI_API_KEY` and `CANYON_MODEL`, which has no
  default there. The dashboard's setup chat stays off on OpenAI.
- One model serves the setup chat and the review. Canyon calls the provider's own address, or the
  one `CANYON_MODEL_URL` names, never a base URL it finds elsewhere in the environment. Other
  `OPENAI_*` variables in the host's environment, such as an organization or project id or custom
  headers, still apply to the OpenAI client.
- A provider named without its key refuses to start.

The model is sent the rules it may report on and the app's source files only, never `.env*`, `.git/`
or `node_modules/`, with every secret masked as `••••`.

## Rotate the encryption key

Canyon encrypts stored secrets with a key kept on the server's volume. To move them to a new key:

```sh
docker compose exec server /canyon-server rotate-kek                     # the demo
kubectl -n canyon exec deploy/canyon-server -- /canyon-server rotate-kek  # Kubernetes
```

- Each run writes a fresh key version beside the old ones (`kek.2`, `kek.3`), moves every row that
  isn't on it, and prints how many moved and how many remain.
- It refuses while the database is behind the binary.
- A value set before the server restarts is still sealed under its boot version and counted as
  remaining: run it again.
- Canyon never deletes a key file. Remove one once no row names it.
- A backup taken before the rotation still opens with the old key. If a key was compromised, replace
  each value at its source and set it again with `canyon secret set NAME`.
