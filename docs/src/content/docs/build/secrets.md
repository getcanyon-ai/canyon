---
title: Secrets
description: Store the keys, passwords and settings an app reads, per environment, with canyon secret, and keep them out of the code.
---

Canyon calls every value an app reads from its environment a **secret**: API keys, passwords, and
plain settings too. They go in Canyon, not in the code. Canyon keeps each value encrypted on the
server and hands it to the app as an environment variable. No command and no page ever shows a value
again.

## Set one

```sh
canyon secret set GREETING
```

```text
  canyon · amber-gecko-w4g · localhost:7777

  ✓ Set GREETING for amber-gecko-w4g · preview · runtime
    Takes effect at the next deploy: canyon deploy.
```

- **The value comes from a prompt or a pipe, never from the command line**, where the shell would
  record it in its history. The prompt shows nothing as you type. Where nobody can answer a prompt,
  as in a coding agent or CI, pipe it in: `printf %s "$VALUE" | canyon secret set NAME`. For a
  multi-line value, such as a key file: `canyon secret set NAME < key.pem`.
- **Each environment has its own values.** `--env` is `preview` (the default) or `prod`. Set the
  production value separately:

```sh
canyon secret set GREETING --env prod
```

```text
  canyon · amber-gecko-w4g · localhost:7777

  ✓ Set GREETING for amber-gecko-w4g · prod · runtime
    Takes effect at the next promotion: canyon deploy --env prod.
```

- **Runtime or build.** A runtime secret, `--where runtime` (the default), is read by the app while
  it runs. A build secret, `--where build`, is read while Canyon builds the app's image, for example
  a token to install a private package. Build secrets exist only on preview, because production
  runs the image preview built. A changed build secret makes the next deploy build again.
- **Names a Canyon on one Docker server refuses.** There, Canyon's own tools run beside the app and
  read some names from the environment a secret travels in, so:
  - no secret, runtime or build, may be named anything starting `DOCKER_`, in any case
    (`DOCKER_USERNAME`, `docker_password`): the app won't start, and its deploy, promotion,
    rollback or Canyon's fix of its system libraries fails naming it;
  - no build secret may be named `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`,
    `SSL_CERT_FILE`, `SSL_CERT_DIR`, `GODEBUG`, `REGISTRY_AUTH_FILE`, `XDG_RUNTIME_DIR`, or anything
    starting `PACK_`, in any case: the build tool runs outside the build's network and would read
    it too, so the build is refused. A runtime secret may take these names.

  Give such a secret another name ([Troubleshooting](/reference/troubleshooting/#a-secrets-name-is-refused)).
  On Kubernetes all these names are fine.
- **It takes effect on the next deploy.** Run `canyon deploy` again for preview to read it. Set
  production's values before you promote ([Get to production](/build/production/)).
- `canyon secret set` works before the first deploy too: it sets up the app the way a first deploy
  does.

## See which are set

```sh
canyon secret list
```

```text
  canyon · amber-gecko-w4g · localhost:7777

  amber-gecko-w4g

  NAME      ENV      WHERE    SET BY     SET
  GREETING  preview  runtime  anonymous  just now
```

The list shows names, environments, who set each one and when, and never a value. `SET BY` names
the person who set it. On a Canyon without sign-in, such as the laptop demo, it reads `anonymous`.

`canyon deploy --env prod` and the app's **Promote** button both warn about a runtime secret set for
preview but not for production:

```text
  canyon · amber-gecko-w4g preview → prod · localhost:7777

  Promoting e87c28 (passed review) from preview to production. Nothing runs in production yet.
  Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.
  ! GREETING is set for preview but not production. Set it with: canyon secret set GREETING --env prod

  ✗ deploy --env prod asks before it changes production, and nothing here can answer: re-run it with --yes
```

## Remove one

```sh
canyon secret rm GREETING --env prod
```

```text
  canyon · amber-gecko-w4g · localhost:7777

  ✓ Removed GREETING from amber-gecko-w4g · prod
```

It removes one name from one environment, and doesn't ask first.

## Values Canyon sets itself

- **Databases.** An app that declares a database in `canyon.toml` gets `DATABASE_URL` from Canyon,
  in each environment. You can't set or remove it by hand. See [Databases](/build/databases/).
- **Names you can't use:** `PORT`, `PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, and any name starting
  `CANYON_IDENTITY_` or `CNB_`. Canyon or the app's runtime sets those.
- **Limits:** a name is letters, digits and underscores, not starting with a digit, at most 128
  characters. A value is at most 65,536 bytes; one environment's values together at most 1 MiB.

## Who can set them

The app's owners and contributors, and Canyon admins. Someone with read-only access to the app
doesn't see even the names. See [Share an app](/build/share/).

## A key in the code

Out of the box, the review blocks a key it finds in the code, under the rule
[No secrets in the code](/rules/secrets.client-bundle/). The fix is always the same: change the code
to read the value from the environment, and store it with `canyon secret set` under the name the
code reads.

- **A key that was in the code has leaked.** Rotate it at its source and set the new one.
- **Don't move it into a `.env` file.** The CLI leaves `.env` files out of what it uploads, so the
  app never sees them.
- **Don't move it into a browser variable** (`NEXT_PUBLIC_*`, `VITE_*`, `REACT_APP_*`, `PUBLIC_*`).
  Those are built into the page every visitor downloads.
- **A value pasted into a coding agent's chat** is in the chat's log. The Canyon skill (the
  instructions `canyon` installs for your agent) pipes it straight into `canyon secret set` and
  tells you to rotate it.

The server's encryption key and how to rotate it are on
[Sign-in, the AI model and keys](/org/set-up/).
