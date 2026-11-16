---
title: Canyon Enterprise
description: A few features need a Canyon Enterprise licence. An admin adds it in Settings › Licence. If it expires, nothing stops working.
---

Canyon is free and open source. A few features are part of Canyon Enterprise and need a licence:

- **Credentials Canyon adds.** Canyon adds an API key to an app's calls, so the app never holds it.
  See [Outbound](/org/outbound/).
- **Patching.** Canyon patches the system libraries under your apps.
- **Access by sign-in group.** An app is shared with a group from your sign-in provider.

Everything else is free: the whole review, custom rules, preview and production, approvals, rollback,
the fleet, sharing, sign-in, outbound allowlists, Slack and Teams, Docker, Kubernetes, the CLI and the
skill.

## Add a licence

An admin opens **Settings › Licence**, pastes the licence, and chooses **Save licence**. Canyon checks
it on the server, with no network call. The page then shows who it's for, its seats, when it ends, and
the features it unlocks.

If you run Canyon yourself, you can set the licence in the server's environment instead:
`CANYON_LICENSE` holds the licence's text, or the path of a file holding it. Set there, it wins over
one pasted in the dashboard, and the dashboard shows it read-only.

`canyon licence` prints the same thing in a terminal. Anyone signed in can see which features are
unlocked; only an admin sees the rest.

## When a licence ends

**Nothing that's set stops working.** Credentials Canyon adds keep being added, patching keeps running,
and everyone keeps the access their groups give them. A licence never loosens a review.

What changes is that nobody can add an Enterprise feature or turn one on: a credential for a new
destination, a group added to an app or given more access, patching turned on. Everything that makes
access smaller or fixes a problem always works, with or without a licence:

- replacing a credential's value, to rotate a key that leaked;
- giving a group less access, or removing it;
- turning any Enterprise feature off, or removing what it set.

For 30 days after it ends, the licence still unlocks its features, so you have time to renew. After
that, adding or turning on an Enterprise feature says it needs Canyon Enterprise, until an admin adds a
new licence.

Admins see a line under **Needs attention** on the fleet once a licence has ended, and while Canyon
can't read the licence it has.

## Upgrading, and new installs

When you upgrade to a Canyon with licences, whatever you had set keeps working: credentials, group
access and patching all carry on, licence or not.

A new install without a licence starts with patching off. If the server is set to patch apps
(`CANYON_PATCHING=on`) but no licence unlocks patching, Canyon patches nothing, says so once in the
server's log, and admins see a line under **Needs attention**.
