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

   Ask again. The call works with no new consent: the client refreshed. The sessions list still shows that one
   session, with a later `last_seen_at`.

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

## Claude Code on the web: one sitting

A real cloud agent, run by the person in one sitting. Before it, the operator:

1. pushes a branch, `qa/web-session`, to the test repository with the deploy key, holding an app with one blocking
   finding (`hosted-deploy-ref.md`'s seed does), and a `.mcp.json` at its root:

   ```json
   {
     "mcpServers": {
       "canyon": { "type": "http", "url": "https://<canyon>/mcp" }
     }
   }
   ```

2. makes the app `qa-web-session` as the tester and links it to the repository (`PUT …/repository`).

The person opens Claude Code on the web on that repository and branch, connects canyon when asked (the consent
page, as the tester, `act`), and sends this, word for word:

```
This branch is our team lunch poll, and it's linked to Canyon as the app qa-web-session. Use Canyon's tools: check
this branch, fix whatever Canyon blocks on, push the fix to this branch, then deploy it to preview and give me the
link. Don't put it in production.
```

Good: `canyon_check_ref` on the branch, blocked; the key moved out of the browser code; a push; `canyon_check_ref`
or `canyon_deploy_ref` on the new commit, passed; the preview link; no production. Save the session's transcript
beside the run, and record `pass` or `fail | Claude Code on the web | <what happened>`; where the web session
can't sign in to a remote MCP server at all, that is the finding.
