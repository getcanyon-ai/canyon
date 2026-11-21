# Hosted MCP journeys: drafts for the next round

These scenarios drive an agent that reaches Canyon only by URL: `https://<canyon>/mcp`, the remote server,
behind Canyon's own OAuth (design/specs/mcp.md §2), with no `canyon` CLI on the machine (§10.1). They are
drafts. They live outside `scenarios/` because the runner can't run them yet: they need hosted MCP on the
test Canyon, and the lines below in `scenario.go`.

## The lines they use

- `via: mcp-remote`: the agent's MCP config names Canyon by URL, `{"type":"http","url":"<host>/mcp"}` in
  Claude Code's `~/.claude.json`, and the tester HOME has no `bin/canyon`. The runner checks both before
  turn 1, and the scorecard passes only on remote tools.
- `scope: read` or `scope: act`: the scope the tester's OAuth grant holds (§2.2). A person allows it once
  per tester HOME, in a browser (below); the runner reads the grant's scope from Canyon (`GET
  /api/v1/sessions`, the session's `flow: oauth` and scopes) and refuses a HOME that holds another.
- `linked repo: <owner>/<repo>`: the app is linked to that repository, one the org's Canyon App sees
  (§2.3, §10.1). The runner pushes the seed to a branch of its own there before turn 1, and deletes the
  branch at the end.
- `expect tool: <name>`: the agent called that tool at least once (`canyon_check_ref`, say).
- The MCP lines already in use: `expect mcp: connected`, `promote from turn`, `expect first`, `expect last`,
  `expect prod`.

## Before a batch

1. **A tester HOME per scope**, with Claude Code signed in and no `canyon` CLI. Add Canyon by URL:
   `claude mcp add --scope user --transport http canyon https://<canyon>/mcp`.
2. **The OAuth grant, once, by a person:** start `claude` in that HOME, run `/mcp`, and authenticate canyon.
   A browser opens Canyon's sign-in, then its consent screen; the person signs in as the tester (carol, a
   member of the org) and allows the scope the HOME is for. Headless turns then reuse the token.
3. **For the `linked repo:` scenarios:** a repository the org's Canyon App sees, and push access to it for
   the runner. The app is linked to it by a contributor (§10.1), which the runner does when it claims the
   app.

## The scenarios

| File | What it checks |
|---|---|
| `hosted-connect-oauth.md` | the URL connects after the OAuth grant; the agent reads rules and status with no CLI |
| `hosted-deploy-ref.md` | a pushed branch reviewed with `canyon_check_ref`, blocked, fixed and pushed again, deployed with `canyon_deploy_ref` |
| `hosted-secret-link.md` | an app that needs a secret: the agent gives the person the app page's Secrets section, never asks for the value |
| `hosted-promote-asks.md` | `canyon_promote` over the URL: asks first, promotes only once the person asked |
| `hosted-readonly-refused.md` | a `read`-scope grant: a deploy is refused `insufficient_scope`, and the agent says so without a way around it |
