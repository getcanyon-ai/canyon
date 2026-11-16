---
title: Sign-in inside your app
description: How your app learns who is signed in, from the identity token Canyon sends with every request, and how roles and access work.
---

Your app does not build a login. Canyon signs people in before they reach it, checks that they may
open it, and tells the app who they are in a signed token on every request.

```mermaid title="Every request: Canyon's front door checks the person, the app reads who they are from a token Canyon signed"
sequenceDiagram
  participant B as Browser
  participant F as Canyon's front door
  participant A as Your app
  B->>F: GET /expenses
  Note over F: Signed in? May they open this app?
  F->>A: GET /expenses with the identity token
  A->>F: Fetch the key set, and again for a key it doesn't hold
  Note over A: Verify the token, read email and roles
  A-->>B: The page
```

The front door is Canyon's server on one Docker host and the `canyon-identity` sidecar in the app's
pod on Kubernetes ([Where the check runs](#where-the-check-runs)). The app's code is the same on both.

## The identity token

With sign-in on, every request reaches your app with this header:

```text
Authorization: Bearer <identity token>
```

Canyon replaces whatever `Authorization` header the browser sent. The browser's Canyon cookies never
reach the app. The app's own cookies do.

The token is a JWT, signed with ES256, good for five minutes. Its claims:

| Claim | What it is |
|---|---|
| `sub` | the person's id at your identity provider |
| `email` | their email address |
| `name` | their name |
| `roles` | the app roles this person holds, `[]` when none |
| `aud` | the app's id |
| `env` | `preview` or `prod` |
| `iss` | Canyon's public URL |

## Verify it

Every app container gets three variables to verify the token with:

- `CANYON_IDENTITY_ISSUER`: the token's `iss`.
- `CANYON_IDENTITY_AUDIENCE`: the token's `aud`, the app's id.
- `CANYON_IDENTITY_JWKS_URL`: the key set that signs the token.

Fetch the key set from `CANYON_IDENTITY_JWKS_URL`. Never copy a key into your code: on Kubernetes
each running copy of the app has a key of its own, and a new one each time its sidecar restarts, so
fetch the key set again when a token names a key you don't hold. `jose`'s remote key set does, but
by default at most once every 30 seconds, so for up to 30 seconds after a sidecar restarts the app
answers `401`. Setting `cooldownDuration: 0` fetches at once; on Kubernetes the key set is served
inside the pod, so that costs nothing. This is an example in Node.js with the `jose` package:

```js
import { createRemoteJWKSet, jwtVerify } from "jose";

const keys = createRemoteJWKSet(new URL(process.env.CANYON_IDENTITY_JWKS_URL), {
  cooldownDuration: 0, // fetch a new key at once, as after a sidecar restart
});

async function whoIsCalling(req) {
  const token = req.headers.authorization?.replace(/^Bearer /, "");
  const { payload } = await jwtVerify(token, keys, {
    issuer: process.env.CANYON_IDENTITY_ISSUER,
    audience: process.env.CANYON_IDENTITY_AUDIENCE,
    algorithms: ["ES256"],
  });
  return payload; // payload.email, payload.name, payload.roles
}
```

A route that deletes or changes something important checks a role. An ordinary write records who
did it, from the token. The review's rule [Every route needs sign-in](/rules/auth.unauthenticated-route/)
asks for exactly this.

## Roles

An app declares its roles in `canyon.toml`, each with one line saying what it lets a person do. This
is an example:

```toml
[roles]
expense-approver = "Approves expenses over 1,000 EUR"
```

A role's name is lowercase letters, digits and dashes, starting with a letter, at most 40
characters: `expense-approver`, never `expense_approver`. Its line is at most 120 characters. An app
has at most 16 roles.

The app's owners give roles to people when they share the app. Canyon refuses a role that no
version of the app declares. The app compares the strings in `roles` itself: Canyon has no finer
permission system.

## Who can open the app

A new app is private. Its owners and Canyon's admins can open it, and nobody else until an owner
shares it. Owners share it on the app's page, under **Who can open it**, or with `canyon share`
(see [Share an app](/build/share/)). For example:

```sh
canyon share ana@company.example --role expense-approver
canyon share group:finance --level contributor
canyon share everyone
```

Owners, contributors and read-only people can all open the app. `everyone` means everyone in your
organisation, read-only. A person's roles are the roles of every grant that names them, their
groups or everyone. Admins open every app, but hold only the roles a grant gives them.

Someone without access is sent to a page in Canyon that says whom to ask, with an
**Ask for access** button.

## Where the check runs

**On one Docker host with sign-in on**, Canyon's server is the app's front door. It checks the
person's session and the app's access on every request, then sends the token.

**On Kubernetes**, each app's pod runs a Canyon sidecar, `canyon-identity`, in front of the app. It
verifies who the [front door](/self-host/front-door/) says the person is, reads the app's access
list, and sends the token. Your admin picks who signs people in, in **Settings › App hosting**:
[How people reach apps](/self-host/front-door/) draws the request path for each choice. The app's
code is the same either way.

**In the laptop demo, sign-in is off.** Canyon sends no identity token, and anyone who can reach
Canyon can open every app. `canyon status` says so on
its access line:

```text
  access       anyone who can reach this Canyon (sign-in is off)
```

The three `CANYON_IDENTITY_*` variables are still set, so the same code runs everywhere. Handle a
request with no token in the demo, for example by showing the page without a name.
