---
title: Outbound (beta)
description: Send apps' calls out through Canyon, see what each app calls, and switch to an allowlist, in Settings › Outbound and on each app's page.
---

An admin decides in **Settings › Outbound** whether apps call the internet directly or through
Canyon's egress gateway, which records each destination, and whether a host name on port 80 or 443 is
only recorded (**Observe**) or refused unless allowed (**Allowlist**).
[Choices at a glance](/concepts/modes/#outbound-beta) draws both and says when to pick which.

Outbound is beta: it works, but it isn't complete or fully tested yet.

Outbound covers running apps, not builds. Whatever it says, a build reaches DNS and public addresses on
ports 80 and 443, plus what an admin of the host names for it (`CANYON_BUILD_ALLOW` on Docker), and
nothing else private: never the cloud's metadata address or Canyon's own networks
([configuration](/self-host/configuration/#docker-executor)). On Kubernetes, Canyon builds nothing
where its front-door check hasn't seen the cluster enforce NetworkPolicy
([Strict mode on the AWS VPC CNI](/self-host/kubernetes/#strict-mode-on-the-aws-vpc-cni)).

With Outbound *Off*, apps call out directly, but not anywhere: on Docker an app reaches public addresses
on any port and its own database, never the host Canyon runs on, the cloud's metadata address, Canyon's
own networks or a private address that an admin of the host hasn't named in `CANYON_APPS_ALLOW`
([configuration](/self-host/configuration/#docker-executor)); on Kubernetes, public ports 80 and 443 and
the apps' database. *Through Canyon* records each call and can refuse public destinations too.

## Who sees and changes it

- **Every member** reads Settings › Outbound: how apps call out, whether Canyon adds credentials and,
  while apps' calls go through Canyon, the org's mode. A member's page says "Only an admin can change
  how apps call out."
- **An admin** changes all three.
- **An app's owner** can make one of its environments stricter than the org's, on the app's page.
- **The app's contributors, its owners and admins** can allow a destination for one environment.

Only a person signed in to the dashboard can widen what an app may call. On a Canyon with sign-in on,
the API refuses the CLI's session with `dashboard_required` ([Calling Canyon's API](/reference/api/#what-needs-the-dashboard)).

## Send apps' calls through Canyon

1. Open **Settings › Outbound**. Under *How apps call out*, choose **Through Canyon**, and click
   **Save**.
2. If the org's mode is already allowlist, the page first lists what allowlist would refuse, from
   what Canyon recorded before, and asks "Send calls through Canyon under allowlist? From each app's
   next deploy, Canyon refuses every destination not on its allowlist." Allow what apps need, then
   click **Turn on Through Canyon**.

It reaches each app environment at its next deploy or promotion. On a Docker host, apps whose network
was made before are listed under *Networks to recreate*. **Recreate** restarts that app, production
included, on the network the setting gives it.

On Kubernetes, Canyon refuses *Through Canyon* while the front door signs people in at a sign-in
provider served only through your cluster's own gateway: apps whose calls go through Canyon couldn't
reach it, so nobody could sign in to them. The page says so above the choice.

## Switch to allowlist

The org's mode is **Observe** until an admin changes it. To change it:

1. Open **Settings › Outbound**. *How apps call out* must be **Through Canyon**: the mode applies
   only to calls that go through Canyon.
2. Under *What apps may call*, choose **Allowlist**. Before anything changes, the page lists every
   destination your apps called in the last 30 days that allowlist would refuse, by app and
   environment.
3. Click **Allow** beside each destination an app needs. It goes on that app environment's
   allowlist.
4. Click **Save**, then **Switch to allowlist**. Running apps follow the new mode within seconds,
   without waiting for their next deploy.

To go back, choose **Observe** and click **Save**. Nothing asks first, because it only widens what apps
may reach.

## Credentials

Adding credentials is off until an admin turns it on. While it's off, Canyon adds no credential to
apps' calls, and setting one is refused: "Canyon adds no credentials to apps' calls until an admin turns
them on in Settings › Outbound".

To turn it on, open **Settings › Outbound**, tick **Add credentials to apps' calls** under
*Credentials*, and click **Save**. From each app environment's next deploy or promotion, while its
calls go through Canyon, the app trusts Canyon's certificate: Canyon puts it in `/etc/canyon-egress` and
sets `SSL_CERT_DIR`, `NODE_EXTRA_CA_CERTS` and `REQUESTS_CA_BUNDLE`, the last holding the system's
certificates as well as Canyon's. Canyon decrypts only calls to a destination with a credential; every
other call passes through as before. An environment with a secret of its own named any of the three keeps
it, trusts none of Canyon's certificates, and Canyon adds no credential to its calls.

To turn it off, untick the box and click **Save**. Canyon stops adding credentials at once, so calls that
need one will fail. Apps keep trusting its certificate until their next deploy or promotion.

## What every mode refuses

Observe isn't a free pass. Under either mode, Canyon refuses a call:

- to an IP address or a single-label name, such as `10.0.4.7` or `keycloak`;
- to any port other than 80 and 443.

It also lets an app environment reach at most 30 new destinations a minute, a limit an admin can
change in Settings › Outbound. A name it already reached in the last 30 days, or that its allowlist
allows, doesn't count as new.

## On an app's page

The *Outbound* card shows each environment's mode and whose it is. An owner can make an environment
stricter than the org's (**Allowlist**) or set it back to **The org's**. An admin can also set an
environment to **Observe** while the org is on allowlist. The app's contributors, its owners and
admins can click **Allow** beside a destination the environment's allowlist refuses; someone with
read-only access sees no *Outbound* card.

An app calling a destination its allowlist doesn't hold gets `403` with the header
`Canyon-Egress: not_allowed`, and a line with a link to that card. The first refusal of a destination
files a request, and the [fleet](/org/dashboard/)'s "Only apps that need you" picks the app for its
contributors, owners and admins. The card lists each environment's requests under *Refused*, with how
often and how lately each was refused:

- **Allow** puts the destination on that environment's allowlist. Only a person signed in to the dashboard can.
- **Dismiss** says you don't want it allowed. It files no new request, and Canyon keeps counting its refusals. The
  app's contributors, owners and admins can, from any session, but an agent can't. A dismissed destination stays
  listed with its **Allow**.

While production is held, a person can't allow anything for production, though removing an entry still works.

Under *Production*, while its mode is allowlist, the card also lists what preview connects to and production
doesn't allow, each with **Allow**. That's where you answer the go-live check
[Production allows everything preview connects to](/rules/prod.egress-allowed/).

`canyon status` says each environment's mode and what it refused since its version went live, a dismissed
destination marked so. Under `--format ai` it also tells the agent that only a person can allow a destination, and
gives it this card's link.

A rollback runs under today's allowlist, not the one its version had: Canyon puts back the code, not the outbound
rules. Its go-live checks list any destination a person dismissed that production refuses today, unless your own
platform runs production.

**Platform destinations** are always allowed: the hosts Canyon's own sign-in sidecars call,
including the hosts the sign-in provider's discovery document names. The
[configuration reference](/self-host/configuration/#outbound-modes-beta) lists them, and the settings
an operator sets for the egress gateway.
