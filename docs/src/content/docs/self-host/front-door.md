---
title: How people reach apps
description: The front door, defined once. The address an app answers at, who signs people in before the app sees them, a picture of each choice, and when to pick which.
---

"The front door is how people get to an app on this Canyon: the address it answers at, and who signs
them in before the app sees them." That is how the dashboard puts it, in **Settings › App hosting**,
under *How people reach apps*, where an admin sets it. Every other page that says *front door*
means this.

Every app is private to its team until an owner shares it. On every request, something in front of
the app checks who the person is and whether they may open it, then hands the app a token that
Canyon signs, with the person's email, name and app roles
([Sign-in inside your app](/build/sign-in-in-your-app/)). The app never builds a login.

## On one Docker host

**On one Docker host there is nothing to choose.** Canyon's server is every app's front door: with
sign-in on, it checks the person's Canyon session and the app's access list, then forwards the
request with Canyon's token. The laptop demo has sign-in off, so its server forwards every request
with no check and no token: anyone who reaches it opens every app.

```mermaid title="One Docker host, with sign-in on: Canyon's server is the front door"
flowchart TD
  browser["Browser<br>app.preview.your-canyon"]
  server["canyon-server<br>checks the Canyon session<br>and the app's access list"]:::canyon
  app["Your app, port 3000<br>on its own Docker network"]
  browser --> server
  server -->|"Canyon's identity token"| app
```

Settings › App hosting says so on a Docker host: "This Canyon runs apps on Docker and serves them
itself, so there's nothing to set here."

## On Kubernetes

An admin sets three things in **Settings › App hosting**. Until they are saved, no app can run: a
deploy ends "Nobody can reach an app until this is set, so no app can run on this Canyon yet. An admin sets it in
Settings › App hosting."

- **The domain** apps live under: Canyon's own host, unless the admin names another.
- **The address form**: how an app's address is built from the domain. `app.env.domain` gives
  `claims-portal.preview.example.com`, under two wildcards, `*.preview.<domain>` and
  `*.prod.<domain>`. `app-env.domain` gives `claims-portal-preview.example.com`, under one,
  `*.<domain>`.
- **Who signs people in**: one of the choices below.

Whichever is chosen, Canyon's sidecar `canyon-identity` runs in each app's pod, in front of the app:
it checks who the front door says the person is, reads the app's access list, and hands the app
Canyon's token. Only the sidecar can reach the app's port.

### Who signs people in

The dashboard offers two choices, "Your sign-in proxy signs people in" and "Canyon signs people in", and
the first comes in two kinds. Pick one:

- **Your sign-in proxy signs people in, as a standard OIDC gateway**, if an OIDC proxy already sits in
  front of your internal apps and can pass on the signed-in person's ID token as
  `Authorization: Bearer`. People sign in once, at your gateway, for every app.
- **Your sign-in proxy signs people in, as AWS load balancer sign-in**, if your apps sit behind an AWS
  Application Load Balancer whose listener rule signs people in with OIDC. The load balancer sends
  each person to your provider with `redirect_uri=https://<address>/oauth2/idpresponse`, an address
  AWS fixes for each host. The OIDC client its listener rule names must accept each app's
  `https://<address>/oauth2/idpresponse`, in both environments, or a wildcard where your provider
  allows one. Otherwise opening a new app stops at your provider, which says the redirect URI isn't
  registered. This is apart from the address Canyon's own sign-in uses, `/auth/callback`: where
  the load balancer uses Canyon's own client, that client must accept both.
- **Canyon signs people in** if your gateway only routes and ends TLS. A sign-in proxy,
  oauth2-proxy, runs beside each app, against an OIDC client you register. That client must accept
  each app's `https://<address>/oauth2/callback`, in both environments, or a wildcard where your
  provider allows one.

```mermaid title="Your sign-in proxy signs people in, standard OIDC: the sidecar verifies the gateway's ID token"
flowchart TD
  browser["Browser"]
  gw["Your OIDC gateway<br>signs the person in"]
  idp["Your identity provider"]
  router["canyon-router"]:::canyon
  subgraph pod["The app's pod"]
    sidecar["canyon-identity<br>verifies the ID token<br>checks the app's access list"]:::canyon
    app["Your app, port 3000"]
  end
  browser --> gw
  gw <-->|"sign-in"| idp
  gw -->|"Authorization: Bearer, the gateway's ID token"| router
  router --> sidecar
  sidecar -->|"Canyon's identity token"| app
```

```mermaid title="Your sign-in proxy signs people in, AWS load balancer: the sidecar verifies what the load balancer signed"
flowchart TD
  browser["Browser"]
  alb["Your AWS load balancer<br>its listener rule signs the person in"]
  idp["Your identity provider"]
  router["canyon-router"]:::canyon
  subgraph pod["The app's pod"]
    sidecar["canyon-identity<br>verifies x-amzn-oidc-data<br>against that load balancer's key<br>checks the app's access list"]:::canyon
    app["Your app, port 3000"]
  end
  browser --> alb
  alb <-->|"sign-in"| idp
  alb -->|"x-amzn-oidc-data, signed by the load balancer"| router
  router --> sidecar
  sidecar -->|"Canyon's identity token"| app
```

```mermaid title="Canyon signs people in: oauth2-proxy beside each app signs the person in, the sidecar verifies its token"
flowchart TD
  browser["Browser"]
  gw["Your gateway or ingress<br>TLS and routing only"]
  router["canyon-router"]:::canyon
  idp["Your identity provider<br>your OIDC client"]
  subgraph pod["The app's pod"]
    proxy["oauth2-proxy<br>signs the person in"]:::canyon
    sidecar["canyon-identity<br>verifies the ID token<br>checks the app's access list"]:::canyon
    app["Your app, port 3000"]
  end
  browser --> gw
  gw --> router
  router --> proxy
  idp <-->|"sign-in, /oauth2/callback"| proxy
  proxy -->|"the ID token"| sidecar
  sidecar -->|"Canyon's identity token"| app
```

`canyon-router` in these pictures is Canyon's own, there only while Canyon runs the apps. When
[your platform runs them](/self-host/hand-off/), your platform routes each request to the sidecar.

| | A sign-in proxy that speaks OpenID Connect (OIDC) | AWS load balancer sign-in | Canyon signs people in |
|---|---|---|---|
| Who signs people in | Your gateway | Your load balancer | oauth2-proxy, beside each app |
| What you give Canyon | The gateway's issuer, audience and key set URL, and its session cookie's name | The load balancer's ARN, and its session cookie's name if it isn't `AWSELBAuthSessionCookie` | Your OIDC client's issuer, client ID and secret |
| What your provider must accept | Your gateway's own callback, one address | Each app's `https://<address>/oauth2/idpresponse`, in both environments | Each app's `https://<address>/oauth2/callback`, in both environments |
| What runs beside each app | `canyon-identity` | `canyon-identity` | oauth2-proxy, then `canyon-identity` |
| [Your platform runs the apps](/self-host/hand-off/) | Works | Works | Not supported |
| In the API and the Helm chart's `frontDoor` | `choice: 1`, gateway `oidc` | `choice: 1`, gateway `alb` | `choice: 2` |

The sidecar removes your gateway's session cookie before a request reaches the app, so no app can
use it to reach another. Where Canyon signs people in, oauth2-proxy asks your provider for the scopes
`openid email profile offline_access`.

Where your provider must accept each app's address, with the load balancer or with Canyon signing
people in, a wildcard saves adding every app, if your provider allows one:

- [Okta](https://support.okta.com/help/s/article/Does-Okta-Support-Wildcard-Redirect-URIs) does,
  once the app's "Allow wildcard * in sign-in URI redirect" is on: one `*`, in the lowest
  subdomain only. For the `app.env.domain` form, `https://*.preview.<domain>/oauth2/idpresponse` and
  `https://*.prod.<domain>/oauth2/idpresponse`.
- [Microsoft Entra ID](https://learn.microsoft.com/en-us/entra/identity-platform/reply-url) does
  only for an app that signs in work or school accounts alone, through its manifest, and Microsoft
  advises against it.
- [Google](https://support.google.com/cloud/answer/15549257) never does: a redirect URI must match
  exactly.

Without a wildcard, add each app's addresses to the client as apps are created, or use a standard
OIDC gateway, whose provider accepts the gateway's own callback alone.

### Saving it

Saving runs a check of the cluster and the certificate first, and the page says "Checking this
cluster and the certificate…" while it runs. Canyon saves the front door only when the cluster
enforces NetworkPolicy and the certificate covers two addresses under the app wildcards,
`canyon-probe.preview.<domain>` and `canyon-probe.prod.<domain>` (`canyon-probe-preview.<domain>` and
`canyon-probe-prod.<domain>` in the other form); otherwise it shows the fix. Once saved, it says
"Saved. Apps change at their next deploy." A running app keeps the front door it was deployed with:
a change reaches each app at its next deploy or promotion.

One exception, for an organization whose own platform runs the apps: with a platform webhook or a deploy repo saved,
a **Your sign-in proxy signs people in** front door that fails the check is saved anyway, marked **not
checked on this cluster**, with what the check found beside it. A check that can't finish still
refuses the save, and changes nothing, while Canyon runs the apps. Your platform can run apps behind it;
Canyon never runs one behind it, and switching back to Canyon running apps waits until a save
passes the check ([Your platform runs the apps](/self-host/hand-off/)). Canyon still builds each
image on its own cluster, so it builds only where the stored front door's save found NetworkPolicy
enforced: if it didn't, or the check couldn't finish, every deploy fails before the build with "Canyon hasn't seen this
cluster enforce NetworkPolicy, so it builds nothing here", until enforcement is on and the front door
is saved again. A missing certificate alone doesn't stop a build. Under Canyon running the apps, a save
the check refuses because the cluster doesn't enforce NetworkPolicy also marks the stored front door not
checked, whichever front door you were saving: nothing runs or builds behind it until a save passes.

Only an admin changes the front door. A member sees the settings read-only, under "Only an admin can
change the front door."

### Sharing with a person, and the sign-in issuer

A grant to a person, once they have signed in to Canyon, opens an app only when the front door's
token comes from Canyon's own issuer, `CANYON_OIDC_ISSUER`. A grant to someone who hasn't signed in
yet matches their address on any front door. With any other issuer, people still open apps through
group and **everyone** grants, Settings › App hosting says so in an amber line, and each app's
sidecar logs the first token from that issuer.

Entra ID gives a person a different subject for each client, so there the front door must use
Canyon's own client, `CANYON_OIDC_CLIENT_ID`: the client Canyon signs people in with, the client your
gateway's token is issued to, or the ALB's client. When Canyon signs people in with Canyon's own
client, that client's app registration must accept every app's `https://<app address>/oauth2/callback`
as well as Canyon's own. For the ALB's sign-in, set its issuer to Canyon's issuer exactly as the
server logs it at start (`auth oidc, issuer …`), with no `/` at its end: Canyon takes the `iss` the
load balancer signs as the issuer of the person's subject, and compares it exactly.

## Your gateway's side

Your gateway sends Canyon's own host to the Service `canyon-server` and the app wildcards to the
Service `canyon-router`, and holds a certificate that covers them. The install guides set it up:
[Install on Kubernetes](/self-host/kubernetes/#install) (with an AWS load balancer example),
[One k3s host](/self-host/k3s/#7-the-gateway), and the [Helm chart](/self-host/helm/), whose bootstrap
sets the front door from its `frontDoor` values when it brings its own sign-in provider.
