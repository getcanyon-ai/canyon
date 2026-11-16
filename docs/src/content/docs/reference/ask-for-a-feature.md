---
title: Ask for a feature
description: Where Canyon doesn't do something yet, it says so and gives you a link to ask for it. canyon feedback gives you a link for every kind of issue.
---

Where Canyon refuses something it doesn't do yet, the last line says so, with a link:

```
  ✗ This host can't build this kind of app.
  It builds:
    Paketo Buildpack for Node.js
    …
  No buildpack recognised the package, so nothing was compiled: not an error in the code, not a policy block.
  Not supported yet. Ask for it: https://github.com/getcanyon-ai/canyon/issues/new?category=language_unrecognised&os=linux&template=missing-feature.yml&version=v0.10.0
```

The link opens a new issue on Canyon's repository, already filled in with what Canyon doesn't do, your
Canyon version and your operating system. It never carries your app, your host or a path.
Add what you were trying to do, and submit it.

You see this line for:

- **An app no buildpack recognises,** so Canyon can't build it.
- **An S3 bucket in `canyon.toml`** on a Canyon that can't create one.
- **A Dockerfile at the app's root.** Canyon builds with buildpacks and doesn't read it. Nothing is
  refused, so you see the link once per app on your machine, not on every deploy.

When your coding agent hits one, it passes the line on to you word for word and doesn't change your
app to get around it.

## canyon feedback

```
$ canyon feedback
Ask for a feature: https://github.com/getcanyon-ai/canyon/issues/new?os=linux&template=missing-feature.yml&version=v0.10.0
Canyon blocked something it shouldn't: https://github.com/getcanyon-ai/canyon/issues/new?os=linux&template=blocked-wrongly.yml&version=v0.10.0
Canyon missed something it should have caught: https://github.com/getcanyon-ai/canyon/issues/new?os=linux&template=missed.yml&version=v0.10.0
Canyon didn't install or start: https://github.com/getcanyon-ai/canyon/issues/new?os=linux&template=install-failed.yml&version=v0.10.0
```

Each link opens one kind of issue, filled in with your Canyon version and operating system:

| Kind | Use it when |
|---|---|
| **Missing feature** | Canyon doesn't do something you need. |
| **Canyon blocked something it shouldn't** | A finding is on code that is fine. |
| **Canyon missed something it should have caught** | Code passed that Canyon should have flagged. |
| **Canyon didn't install or start** | An install, an upgrade or a start failed. |

Each asks where Canyon runs and its version. Before you paste code or output into an issue, remove every
secret, customer name and private address: issues are public.

To report a security vulnerability, don't open an issue. Report it privately as
[SECURITY.md](https://github.com/getcanyon-ai/canyon/blob/main/SECURITY.md) describes.
