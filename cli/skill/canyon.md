# Canyon

Canyon deploys this app, but runs a review first. The review can block. When it
blocks, it tells you exactly what to fix and how.

## Canyon's tools

If Canyon's MCP tools are loaded (`canyon_check`, `canyon_deploy`,
`canyon_promote`, `canyon_status`, `canyon_logs`, `canyon_rules`), use them
first. Each runs the command it is named for, `canyon_promote` being
`canyon deploy --env prod`, and returns the same output, so everything below
applies to them. Set `canyon_promote`'s `confirm` only when the user asked for
production in this conversation. When the tools aren't loaded, run the commands.

## Deploying

From the app's root directory, run:

    canyon deploy

Read the output. It ends in one of six states:

- `RESULT: PASSED` — report the `live url` to the user. You are done.
- `RESULT: BLOCKED` — continue below.
- `RESULT: APPROVED` — a person approved what blocked it. Follow its NEXT STEPS,
  and do not change that code on your own.
- `RESULT: FAILED` — the run did not finish. Follow its NEXT STEPS. Change the
  app's code only when its `reason:` is `build` or `start`, or when it has a
  `missing:` line: then add that file as its NEXT STEPS say.
- `RESULT: STILL RUNNING` — the deploy was still running when Canyon last
  answered, and this command stopped following it. Follow its NEXT STEPS. Never
  deploy again or take the app offline because of it.
- `RESULT: UNKNOWN` — Canyon said the run ended, and this command couldn't read
  how. Follow its NEXT STEPS. Never deploy again or take the app offline because
  of it.

If the deploy output has a `source:` line that says `kept by Canyon`, Canyon
keeps every version in git for the user: don't create a git repository or a
remote, and don't push anywhere. Otherwise (a `source:` line naming a repository
the user pushes to, or no `source:` line), commit and push only when the user
asks; `canyon deploy` previews work that isn't committed yet.

If the app has no `canyon.toml`, write one at its root before the first deploy,
with the one-line description the rules below ask for and no `app` line: the
first `canyon deploy` makes the app and adds its `app` line itself. Never write
or change an `app` line.

    description = "Log team expenses and send them to finance"

An app that needs a database declares one below that line, and Canyon gives it
`DATABASE_URL`. Do not install a database or write a connection string.

    [resources]
    db = { type = "postgres" }

To look at an app's data, run `canyon db query "select …"`: one statement, run
as the user, read-only unless you add `--write`. Never read the app's
DATABASE_URL, and never ask the user for a connection string or a database
password. Add `--write` only when the user confirms the exact statement. If a
database client must connect, `canyon db connect` prints a url that carries a
token: never paste it anywhere but that client.

If `canyon db` answers `db_sessions_off`, pass its sentence to the user: an
admin turns database sessions on in Settings › Databases. Don't look for another
way into the database.

An app that gives its users roles declares each one, with a line saying what the
role lets a person do. A role's name is lowercase letters, digits and dashes,
starting with a letter: `expense-approver`, never `expense_approver`.

    [roles]
    expense-approver = "Approves expenses over 1,000 EUR"

Before you build, run `canyon manifest` and follow what it prints, unless its
output is already in this session.

Run `canyon check` before the first `canyon deploy` of a new app, and again after
a change that adds a dependency, a route or a secret. It runs the review a deploy
runs, without deploying, so you fix what blocks before the deploy.

A static single-page app (a Vite app, say) runs no server of its own: give it this
`project.toml` at its root and Canyon serves the files its build writes.
`BP_WEB_SERVER_ROOT` is the directory they are written to — `dist` for Vite.

    [_]
    schema-version = "0.2"

    [[io.buildpacks.build.env]]
    name = "BP_WEB_SERVER"
    value = "nginx"

    [[io.buildpacks.build.env]]
    name = "BP_WEB_SERVER_ROOT"
    value = "dist"

    [[io.buildpacks.build.env]]
    name = "BP_WEB_SERVER_ENABLE_PUSH_STATE"
    value = "true"

A plain static site (an `index.html` at its root) needs the same file with
`value = "."` for `BP_WEB_SERVER_ROOT` and no push-state entry.

A Python web app (FastAPI, Flask) needs a `Procfile` at its root naming the command
that starts it on `$PORT`, or Python starts with nothing to run and exits:

    web: uvicorn main:app --host 0.0.0.0 --port $PORT

for FastAPI, with `uvicorn` in `requirements.txt` (`fastapi[standard]` brings it),
or for Flask, with `gunicorn` in `requirements.txt`:

    web: gunicorn app:app --bind 0.0.0.0:$PORT

A PHP app on Postgres requires both `ext-pdo` and `ext-pdo_pgsql` in `composer.json`
(and its `composer.lock`): Paketo loads only the extensions Composer asks for, and
`pdo_pgsql` needs `pdo` loaded first.

## Fixing a blocked deploy

Before your next tool call, tell the user what Canyon found, as the output's
NEXT STEPS 1 gives it. Don't open or change a file until you have.

Then, for each finding with `blocks: yes`:

1. Open the file at the given line.
2. Make the code match the finding's `required end state`. The `suggested fix`
   steps are usually correct — follow them unless the code makes them wrong.
3. Obey the finding's `do not:` list exactly. Those are the ways this finding
   comes back on the next run.
4. If the fix needs a value kept outside the code — an API key, a password, a
   connection string — read it from the environment, and give the user
   `canyon secret set NAME` to run in their own terminal, NAME being the
   variable the code reads. Any name works; the one in the finding is a
   suggestion. Never invent, guess, retype, or placeholder a secret value.

Then verify:

    canyon check

`canyon check` runs the identical review `canyon deploy` runs, without deploying.
It takes about a minute. When it prints `RESULT: PASSED` or `RESULT: APPROVED`,
run `canyon deploy` again and report the `live url`.
If it says Canyon is watching its patch of the preview until a time, run
`canyon deploy` if you were going to deploy anyway, which ends the watch;
otherwise run `canyon check` again after that time, never in a loop before it.

## Rules

- Never add an ignore comment, a suppression pragma, or `--force`. Suppressions
  are audited and do not clear a blocking finding.
- If Canyon says the live version is not the one you started from, do not run
  either command. Show the person both options and ask which they want.
- Never move a secret into a `.env` file, a comment, a config file, or a
  client-side variable (`NEXT_PUBLIC_*`, `VITE_*`, `REACT_APP_*`, `PUBLIC_*`) to
  make a finding go away. That is the same finding.
- Never commit a secret value while fixing one.
- Never print a file that holds a secret (`cat .env`, `cat key.pem`) to see what
  is in it: its value then sits in the chat log. `canyon secret list` names the
  secrets the app has; to find where the code reads one, search for its name,
  never its value.
- Never ask for a secret's value in the conversation, and never take one there.
  When the app needs one, give the user `canyon secret set NAME` to run in their
  own terminal: it reads the value there, so it never passes through you. If the
  user pastes one anyway, don't use it: tell them it is now in the chat log, to
  rotate it, and to set the new one with that command.
- A secret value that is only in the code has leaked: ask the user to rotate it
  and set the new one, before the next deploy.
- Never remove an app's credential in Canyon. Only a person can set one, and its
  value can't be read back, so removing it loses it for good.
- If canyon secret set answers remove the app's credentials in <env> first, stop
  and show the user that line. Whether to remove a credential is their decision.
- Findings with `blocks: no` do not block. Tell the user they exist;
  fix them only if asked.
- After two full fix-and-recheck rounds that still end in BLOCKED, stop. Show the
  user the remaining findings and ask how they want to proceed. Do not keep looping.
- You cannot approve a blocking finding, and neither can the user. An admin or a
  security responder can, with a reason, on its page in Canyon: `<host>/#/apps/<app>`,
  from the output's `host` and `app` lines, but never one of a review they started.
  When a finding won't clear, show the user it and that page, say an admin or a
  security responder must approve it, and stop.
- Before the first deploy, write a one-line `description` in `canyon.toml`, above
  any `[section]`, saying what the app does.
- Leave the `app` line in `canyon.toml` to `canyon deploy`: it writes the id
  Canyon picked, and an id you write may be someone else's app.
- After a deploy, tell the user who can open the link (the access line).
- When the user asks for production, you run `canyon deploy --env prod`, adding
  `--yes` only when they asked for production in this conversation. Before it, run
  `canyon secret list`: for each runtime secret set for preview and not for
  production, ask the person to set it themselves with
  `canyon secret set NAME --env prod` in their own terminal, or whether
  production should go without it. Never ask for the value in the chat. Never
  tell the user a person on the team will promote it when you are the one who
  runs the command.
- Where the org has a person approve production, that same command ends WAITING
  FOR APPROVAL with an approval url: give the user the link, say a person on the
  app's team approves it in Canyon, and stop. Never open or approve that url
  yourself.
- When an output says `RESULT: HELD`, a security responder or an admin stopped
  production: tell the user the reason line, and that only a responder or an
  admin lets it run again, in Canyon's dashboard. Never try to work around a
  hold. Deploying a fix to preview is allowed.
- When Canyon refuses something it doesn't do yet (a build no buildpack
  recognised, or an s3 resource this Canyon doesn't provide), the refusal ends
  with "Not supported yet. Ask for it: <link>". Pass that line to the user word
  for word, with the link, and don't change the app to get past the refusal.
- An app's environment may be under an allowlist, where Canyon refuses calls to
  any outside service not on it. Only a person on the app's team can allow a new
  one, in Canyon's dashboard; you cannot. Never try to reach a refused service
  another way. Canyon also refuses any call out to the internet to an IP address,
  a one-word host name or a port other than 80 and 443, and no one can allow it:
  call a host name on 80 or 443. When a call out fails, `canyon logs` and
  `canyon status` list, within a minute, the outside services Canyon refused as
  not allowed; they don't list the other refusals. Once a person allows one, the
  app's next call to it goes through with no deploy: check that call before you
  change any code, and never deploy again for it.
- Put an earlier version back only when the user asks: run the command
  `canyon status` prints under Earlier in production.
- A line ending `(your platform's pods: nothing to do in Canyon)` is about the
  org's own platform, which runs the app: tell the user, and never redeploy,
  undeploy or roll back because of it.
- When a deploy names a pull request (`opened a pull request for`, or
  `The pull request for <version> is open`), production goes live once someone
  merges it in the org's deploy repo and a pod checks in: give the user the link.
  Never merge, edit or close it yourself, and never deploy again to hurry it.
- Never run `canyon share` unless the user asks to share or send someone a link.
  Then share with exactly who they name, read-only unless they say otherwise, and
  `everyone` only when they say everyone.
- When you tell the user what the review caught, before you fix it, say plainly why it
  matters. They asked for a link, not a security lecture — one or two sentences.

## Other commands

- `canyon status` — what this app runs, and what stands between it and production
- `canyon logs --env preview` — the app's last log lines, when a deployed app misbehaves
- `canyon deploy --env prod` — put what preview runs into production, or, where the
  org has a person approve production, ask for that
