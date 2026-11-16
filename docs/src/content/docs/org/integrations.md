---
title: Slack and Teams
description: Tell a Slack or Teams channel when something in production needs a person, from Settings › Integrations.
---

Canyon can post to your team's Slack or Teams channels when something in production needs a person,
so nobody has to keep Canyon open to notice. An admin connects each channel in
**Settings › Integrations**.

## What Canyon posts, and when

Canyon posts the same things the dashboard's *Needs attention* lists for production:

- production was stopped by a responder or an admin;
- production isn't running, or keeps restarting;
- production can't reach a destination, whether a person on the app's team can allow it or the code has
  to change;
- a version, or a patch, waits for a person to approve it for production;
- the latest review found a secret in an app's code;
- Canyon can't post to another of your channels (below).

It doesn't post what happens in preview, a new version blocked at review (unless for a secret), requests
for access, or apps without an owner. Those stay on the dashboard.

Each one is posted **once**, when it starts. It's posted again only when it changes in a way someone
acts on: a stopped app starts restarting, a different version waits for approval, or production is
held after a destination was refused. A destination refused 3 times and then 300 times is still one
message.

When something has been resolved for 30 minutes, Canyon posts one short line saying so. If it comes back
within those 30 minutes, nothing more is posted.

A channel gets at most 20 messages a minute. Past that, one message says how many more there are and
links to Canyon.

## What a message says, and what it never says

Everyone in the channel reads each message, whether or not they use Canyon. So a message carries names
only:

- the app's name;
- what's wrong, in the same words the dashboard uses;
- the destination's host name and port, when production can't reach one;
- the version's label, for an approval, a patch or a secret;
- when it started;
- a link to the app in Canyon. On a Canyon with sign-in, it asks the reader to sign in.

A message never carries a secret's name or value, a finding's code or fix, an app's log lines, the code
for approving a promotion, or anyone's email address.

For example:

> claims-portal · production can't reach api.stripe.com. A person on the app's team can allow it in
> Canyon. Since 9 Nov 14:02 UTC.

## Connect a channel

Only an admin sees **Settings › Integrations**, and the org can connect up to 5 channels. Each channel
gets every app's messages.

### Slack

1. In Slack, add an incoming webhook to the channel: create a Slack app at
   [api.slack.com/apps](https://api.slack.com/apps), turn on **Incoming Webhooks**, and add a webhook for
   the channel.
2. Copy the webhook URL. It starts with `https://hooks.slack.com/`.

### Teams

1. In Teams, open the channel's **More options** (…), choose **Workflows**, and pick
   *Send webhook alerts to a channel*.
2. Copy the workflow's URL.

Teams shows these posts in the activity feed as "Workflows posted a new message", without a preview.

### Then, in Canyon

1. Open **Settings › Integrations** and click **Add a channel**.
2. Give it a name, such as `#platform-alerts`, choose the chat app, and paste the webhook URL.
3. Click **Save**.

Saving posts a summary of what needs a person now to the channel. If that post fails, nothing is saved,
and Canyon says what the chat app answered. A URL on a host other than Slack's or Teams'
([Firewalls](#firewalls) lists them) is refused before anything is posted.

**Send a test message** posts the summary again. **Change** renames a channel or gives it a new URL;
leave the URL empty to keep the current one. **Remove** stops Canyon posting there.

The webhook URL works like a password: anyone who has it can post to the channel. Canyon keeps it
encrypted and never shows it again. Settings shows only its host and its last four characters.

Adding or changing a channel is done in the dashboard, by a person. It decides who reads every app's name,
so Canyon refuses it from an agent, and, on a Canyon with sign-in, from an admin's CLI session. An admin
removes or tests a channel from any session, the CLI's included.

## When a channel stops working

If Slack or Teams answers that the webhook URL no longer exists (404 or 410), usually because someone
removed the webhook or the workflow, Canyon stops posting to that channel. Then:

- its row in **Settings › Integrations** says when Canyon stopped posting there and what the URL answered;
- every admin sees it in *Needs attention* on the dashboard;
- Canyon posts it to your other channels, if you have any, so a broken channel doesn't go unnoticed.

To fix it, make a new webhook in Slack or Teams, click **Change** on the channel, paste the new URL and
save. Messages Canyon couldn't post while the channel was broken aren't sent later: saving posts a fresh
summary instead.

When Slack or Teams is busy or briefly unreachable, Canyon tries again for up to an hour before giving up
on that message.

A message is delivered **at least once**. Canyon never posts the same message from two servers at once,
but now and then a channel may get one twice: when a server stops just after Slack or Teams took a message
and before it recorded that, or when Slack or Teams took a message but answered too late. Their incoming
webhooks have no way to say a message was already received.

## Firewalls

Canyon's server posts the messages, not your apps. It sends HTTPS requests on port 443 to:

- `hooks.slack.com`, for Slack;
- for Teams, the workflow's own host, which ends in `.logic.azure.com`, `.webhook.office.com` or
  `.powerplatform.com`.

Installed with Canyon's Helm chart or `deploy/k8s`, the server can already reach public addresses on port
443, so there's nothing to change.
If your network limits what the server reaches, allow those hosts.
