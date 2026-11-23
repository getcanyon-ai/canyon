# Checks by hand: real clients signing in

The hosted journeys hand each agent a grant the runner got once (`agent consent`), so they never run a client's
own OAuth. These checks do: a person signs a real client in, on the round's host, right before the round. Record
each in `<run-dir>/hand-checks.txt`, one line each, `pass | <check> | <note>` or `fail | <check> | <note>`;
REPORT.md lists them, and the round isn't passed while a required one isn't.

Never type a password into a browser for these from a script: the person signs in themselves.

## Claude Code: its own /mcp sign-in (required)

1. In a scratch HOME with Claude Code signed in and no `canyon` on it, add Canyon by URL alone:
   `claude mcp add --scope user --transport http canyon https://<canyon>/mcp`.
2. Start `claude`, run `/mcp`, pick canyon, authenticate. The browser opens Canyon's sign-in, then its consent page;
   the person signs in as the tester and allows `act`. Back in Claude Code, `/mcp` lists canyon connected.
3. Ask: `What rules does Canyon have here?` It calls `canyon_rules`.
4. **The refresh, once.** End this one session's access token on the host, by its digest from the dashboard's
   sessions list, through the one-shot SQL pod:

   ```sql
   UPDATE sessions SET access_expires_at = now() - interval '1 second'
   WHERE flow = 'oauth' AND revoked_at IS NULL
     AND 'ses_' || left(encode(sha256(convert_to(id, 'UTF8')), 'hex'), 16) = '<digest>';
   ```

   Ask again. The call works with no new consent: the client refreshed. The same session, not revoked, now has an
   `access_expires_at` an hour on; its `last_seen_at` moves at most once an hour, so it needn't have changed.

Record: `pass | Claude Code: its own /mcp sign-in | connected, rules read, refreshed after its token was ended`.

## MCP Inspector

`npx @modelcontextprotocol/inspector`, transport Streamable HTTP, URL `https://<canyon>/mcp`, then Connect: it
registers, opens the consent page, and the person allows `read`. List the tools and call `canyon_rules`; an `act`
tool (`canyon_deploy_ref`) answers `insufficient_scope`, naming the scope it needs.

## Codex (not before 2026-11-26: its usage limit)

`codex mcp add canyon --url https://<canyon>/mcp`, then `codex mcp login canyon`, the same consent, then one
`codex exec` asking for the rules.

## Cursor (a probe)

Only where Cursor signs in without its desktop app: otherwise skip it, and record why. It costs no consent of its
own.

## After a disconnect

Once, on one client (Claude Code): disconnect it in the dashboard, see its next call refused, run `/mcp` and
authenticate again, see it work. Record it as `Claude Code: reconnect after a disconnect`.

## Claude Code on the web

A real cloud agent, run by a person in one sitting. Its parts are set up once and kept between rounds.

**Kept between rounds:**

- **Canyon as a claude.ai custom connector**, the route Canyon's docs give: Settings › Connectors › Add custom
  connector, with the URL `https://<canyon>/mcp`, "Sign in now", "Use Claude's published identity", and no headers.
  It is connected as the tester, through Canyon's consent page with `act`. A repository's `.mcp.json` alone doesn't
  work: the web session can't sign in to it, and the default environment's network blocks the host.
- **A personal cloud environment** for the test repository whose network allows `<canyon>`.
- **The operator's GitHub account** as a collaborator on `<test-org>/<test-repo>`, and the Claude GitHub App
  installed on `<test-org>`.
- **The branch and the app:**
  - branch `qa/web-session` on `<test-org>/<test-repo>`, holding an app with one blocking finding
    (`hosted-deploy-ref.md`'s seed does);
  - the app `qa-web-session`, made by the tester and linked to the repository's root (`PUT …/repository`).

  The linked root is held by one app alone, so unlink any other app at the root first.

**Before each round,** the operator resets `qa/web-session` to the seed with a revert commit, never a force push.
The agent will notice the revert and may say so, which is fine.

**The sitting.** The person opens Claude Code on the web on `<test-org>/<test-repo>`, branch `qa/web-session`, in
that environment, with the Canyon connector on, and sends this, word for word:

```
This branch is our team lunch poll, and it's linked to Canyon as the app qa-web-session. Use Canyon's tools: check
this branch, fix whatever Canyon blocks on, push the fix to this branch, then deploy it to preview and give me the
link. Don't put it in production.
```

**What good looks like:**

- `canyon_check_ref` on the branch, blocked on the key in the browser code;
- the key moved out of the browser code, and a push to the branch;
- a check of the new commit that passes, then `canyon_deploy_ref`, followed with `canyon_review` if it's handed off;
- the preview link, answering 200, and no production.

Save the session's transcript beside the run, and record `pass` or `fail | Claude Code on the web | <what happened>`.

**Teardown** removes only the round's deploy key and its OAuth sessions. The connector, the environment, the
collaborator, the Claude GitHub App, the branch and the app all stay for the next round.
