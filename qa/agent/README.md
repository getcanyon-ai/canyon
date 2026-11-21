# Agent journeys

Headless Claude Code, or Codex or Cursor where a scenario says `agent: codex` or `agent: cursor`, builds and ships small apps
through Canyon as a signed-in tester would; a scorer reads what Canyon told it and what it did next.

## Run a batch

1. **A tester HOME**, signed in once for the batch: an empty directory holding `bin/canyon` from a
   release (checksum verified) and a copy of `~/.claude.json` only. On macOS Claude's login is in
   the Keychain, found under `$HOME/Library/Keychains`: link that directory into the tester HOME
   (`ln -s ~/Library/Keychains $H/Library/Keychains`) or headless `claude` is "Not logged in". Then `HOME=$H canyon login <host>` (a person approves the code as a member; a demo with sign-in off asks no one, and the runner calls it with no session),
   `HOME=$H claude -p hi` once so `~/.claude` exists, and `HOME=$H canyon skill install`.
   A batch with a Codex scenario also needs `codex` on the PATH (`codex exec` runs headless) and its
   sign-in in the tester HOME, linked so no copy of the credential is left in the run:
   `mkdir -p $H/.codex && ln -s ~/.codex/auth.json $H/.codex/auth.json` on a machine signed in to
   Codex, and nothing else of `~/.codex`, whose own `AGENTS.md` would reach every turn. Codex needs no skill install here: the runner installs Codex's skill and hook in the tester HOME.
   Put the run directory outside any git repository and outside your own home: both agents read
   instruction files (`CLAUDE.md`, `AGENTS.md`) from the directories above the app, and Claude Code
   reads a `.claude/` above it as a project's, so a run under `~` gives every turn your own
   `~/.claude/skills` (2026-11-07: 53 skills a tester doesn't have, one of them on running these journeys).
   The tester HOME may stay under `~`: what counts is where the app is.
2. **Run**, two scenarios at a time, into a new directory:
   `go run ./qa/agent run $H <run-dir> qa/agent/scenarios/*.md`. A rerun on a host that already
   has the apps takes `--suffix r2` and gets fresh ones, `qa-<name>-r2`: a fresh copy can't deploy
   over an app another copy made. Before a scenario's first turn the runner creates its app as the
   tester, and refuses one that already exists. After its last turn (or ctrl-c) it takes the app
   offline in preview and production through the undeploy route, asking again while a run that was
   switching the environment stops it (data and secrets kept; answers in `undeploy.log`), so a batch
   never fills the host;
   `--keep-apps` leaves them up. Each scenario's app is
   `<run-dir>/<name>/qa-<name>/`, whose `canyon.toml` the runner starts with that app's line. Every
   process the runner starts as the tester gets `TMPDIR=<run-dir>/tmp`, so what the CLI packs stays
   with the run; each user turn is one
   `claude -p` (`--continue` from the second), written to `turn-<n>.jsonl`. Turns run with
   `--permission-mode acceptEdits` and the tool list in `run.go`; anything else is denied and
   lands on the scorecard. Before a Codex scenario's first turn the runner installs Canyon's Codex
   skill and session hook in the tester HOME (`canyon skill install --harness codex`, as a Codex user
   does once), then trusts that hook as `/hooks` in Codex would: `codex app-server`'s `hooks/list`
   gives the hook's key and hash, and `config/batchWrite` records them under `hooks.state` in the
   tester's `~/.codex/config.toml` (`codextrust.go`). It refuses a tester HOME whose Codex lists any
   hook but Canyon's, so no turn runs a hook nobody reviewed, and never passes
   `--dangerously-bypass-hook-trust`. Each turn is one `codex exec --json` in Codex's
   `workspace-write` sandbox with the network on (`codexConfig` in `codex.go`). After turn 1 the
   runner reads Codex's record of the session (`~/.codex/sessions/…/rollout-…-<thread>.jsonl`) and
   stops the scenario unless the hook put Canyon's manifest in it: Codex skips an untrusted hook
   silently, and a Codex release that hashes hooks differently would otherwise go unnoticed. After
   every turn it stops the scenario when the app's `.codex` holds a `hooks.json` or a `config.toml`
   that trusts hooks, or Codex lists a trusted hook that is not Canyon's. At the scenario's end it
   removes the trust Codex gave the app's project on turn 1 (`projects."<app>"` in the tester's
   `config.toml`), through the same `config/batchWrite`, so a batch leaves no app trusted. The
   second turn on resumes the first's thread, written to `turn-<n>.jsonl` in
   Codex's own events, with its wall time in `turn-<n>.wall`. `codex exec` asks nobody: a command
   its sandbox refuses fails in place, so a Codex scorecard has no permission denials to list.
   A batch of 18 scenarios is a few hundred Claude turns; an account that reaches its usage limit
   mid-batch fails the rest with `claude: exit status 1` (see `turn-<n>.stderr`). Rerun just those
   scenarios with a new `--suffix`, since their apps are already claimed.
3. **Score**: the run ends with it; `go run ./qa/agent score <run-dir>` again re-scores. It writes
   `<name>/scorecard.md` and `summary.md`, keeping each scorecard's `## Reviewer notes`.
4. **Review**: write each scorecard's reviewer notes from its transcript (did fixes come from NEXT
   STEPS or finding text; lines misread or ignored; wording), then `REPORT.md` with a fix list.

The scorer parses only Canyon's `CANYON REVIEW`, `CANYON PROMOTE` and `CANYON DB QUERY` `— RESULT:` blocks
(findings, NEXT STEPS; cli.md §5.2–5.3; a query's env and refusal), the `outbound:` and `refused:` lines of a `CANYON STATUS`
block with its steps from `outcome:` (design/EGRESS.md §8.2), the runner's outbound reads, and the agent's own events (Claude's stream-json, or Codex's
`exec --json`, whose shell commands it reads through the `zsh -lc` around them): first and last result against the scenario, deploys and
checks, whether a production deploy exited 0, turns, wall time, permission denials, any canyon
call with `--force` or call to Canyon's `/api/` around the CLI, and the agent's next six actions
after each result. It names the model that answered each turn, from Claude's events (Codex's name
none). The model under test is turn 1's session model. A turn is a fallback turn when one of its own
replies names another model, or when it carries
a `model_refusal_fallback` of scope `session` or no scope. A context suffix such as `[1m]` names no
other model. A subagent's forwarded reply (`parent_tool_use_id` set) is not the turn's own, and a
`<synthetic>` message, which no model wrote (an API error once retries run out, "No response
requested.", the exit notice), names no model at all. A `--continue` turn after such a refusal
stays on the fallback with no event of its own, and still counts. The scorecard says so in bold, and
every score that rests on such a turn says so too: a review result from that turn or later, a last
outbound read, and production. A `scope: local` fallback is a subagent's or a side question's, not
the turn's answer: it gets a line of its own and flags nothing. Judgement is the reviewer's.

## Cursor

A batch with an `agent: cursor` scenario runs Cursor's headless CLI, `agent`. Before any scenario
starts, the runner readies the tester HOME once:

- **The key.** It needs `CURSOR_API_KEY` in the runner's own environment: a Cursor user API key
  (Cursor's dashboard, API Keys). The runner hands it only to the Cursor processes it starts, never to
  the `canyon` calls it makes itself, and never writes it anywhere. **But the agent's shell inherits
  Cursor's environment, so every command the agent runs sees the key, `canyon` and Canyon's session
  hook included.** So after every turn the runner replaces its value with `<CURSOR_API_KEY>` in
  `turn-<n>.jsonl` and `turn-<n>.stderr`, and a turn it can't scrub fails. It stops the scenario when
  any file in the app holds the value, since the next deploy would upload it. Every runner error and
  every scorecard, the notes included, carries the name in place of the value.
- **No `~/.claude`.** Cursor runs Claude Code's hooks and reads its settings, so the runner refuses a
  tester HOME that has a `~/.claude` at all, or a `~/.cursor/hooks.json` that defines any hook. A
  Claude Code batch's tester HOME has both, so a Cursor batch takes a tester HOME of its own:
  `bin/canyon`, signed in, and no `~/.claude`.
- **The CLI, pinned.** It installs `cursorVersion` (`cursor.go`) from Cursor's versioned archive,
  never by running Cursor's install script. It keeps the archive in `~/.local/share/cursor-agent/`
  and checks its sha256 against the one recorded for the platform every batch, downloading it again
  when it no longer checks. Then it unpacks it afresh, replacing whatever
  `~/.local/share/cursor-agent/versions/<version>` held, and links `~/.local/bin/agent` to it, as the
  script lays it out. Every process the runner starts as the tester has the tester's `~/.local/bin`
  first on its `PATH`. Every call passes `--disable-auto-update`. To move the pin, take the version Cursor's install script names
  (`https://cursor.com/install`, read, not run), and record its archive's sha256 for each platform.
- **Permissions.** It writes the tools Claude Code is allowed, in Cursor's words (`Shell(canyon)`,
  `Read(**)`, `Write(**)`, …), with `approvalMode` `allowlist` into the tester's
  `~/.cursor/cli-config.json`, keeping Cursor's other keys. Print mode asks nobody and refuses the rest.

For each Cursor scenario it sets Canyon up in the app as a Cursor user does: `mkdir .cursor`, then
`canyon skill install --harness cursor`, which writes Canyon's rule (`.cursor/rules/canyon.mdc`) and
session hook (`.cursor/hooks.json`). It records `agent --version` in `cursor-version`, which the
scorecard names, and creates the chat with `agent create-chat`. Each turn is one `agent -p` with
`--output-format stream-json`, `--trust` (so the app's hook runs) and `--resume <chat>`, written to
`turn-<n>.jsonl`, with its wall time in `turn-<n>.wall`.

After every turn it stops the scenario when the app's `.cursor/hooks.json` is not as
`canyon skill install` wrote it, the app has a `.cursor/cli.json` or a Claude Code hook, the tester
has a `~/.claude` or a hook, the tester's permissions are not the runner's, or any file in the app
holds Cursor's key. After turn 1 it stops unless Canyon's
manifest reached the agent, through its hook or its rule. **No reader reads Cursor's events yet**: the
first run, the probe, gives the transcript the reader is written against. Until then that check fails
closed, every Cursor scenario stops after turn 1 with its `turn-1.jsonl` kept, and its scorecard says
`no Cursor reader yet` instead of a score.

## A scenario

One Markdown file in `scenarios/`. Header lines above the first `##`: `agent: codex` or `agent: cursor` (optional;
`claude-code` without it, and any other agent is refused), `stack: Node/Express`,
`expect first: BLOCKED secrets.client-bundle`, `expect last: PASSED`, `expect prod: promoted`. An
expect is a RESULT and the rule ids or failure reason (`build`, `start`, …) it must name; a rule id
also matches the rules under it (`deps.known-vuln` matches `deps.known-vuln.trivy.CVE-…`). It may also name
the env the block names (`PROMOTED prod`) and, for a refusal, the host's code from its `refused:` line. An
expect that starts with `DB QUERY` is judged against `canyon db query`'s blocks alone, never a review's or a
promotion's: `expect last: DB QUERY OK prod`, `expect last: DB QUERY REFUSED db_prod_off`. Such a block is read only
from the output of a `canyon db query --env prod` the agent ran (its command parsed as shell, as deploy's and check's
are), never from a block it printed again nor a query of preview's. Without it,
`REFUSED` and `REACHED` are outbound expects (below).
`expect last` and `expect prod` (or `not promoted`, or `first release` under `deployed: prod`, below) are optional. `policy: deps.known-vuln blocking`
(any number of them) makes that rule stricter for the scenario's app alone. Before the first turn
the runner creates the app as the tester, who then owns it, and an owner may tighten an app's
checks (contracts/routes.md, `PUT /api/v1/projects/:id/policies/:rule`), so a rule that only warns
on the org blocks here. The agent never sees it happen.

`db prod: read` (or `off`, or `write`) is the host's `db_prod` the scenario needs. No screen sets it yet (#2121), and
the runner never does: before the app is made it reads the setting as
the tester (`GET /api/v1/preferences`) and stops the scenario on a host set otherwise. One host has one setting, so
scenarios that need different ones run in batches of their own.

`secret: preview NAME` (any number, `preview` or `prod`) is a runtime secret the user had set: before the seed deploys, the runner sets NAME as the tester would, `canyon secret set NAME --env <env>` in the app, its value on stdin. The value is made up at run time, `sk-proj-` and 40 random hex digits, an OpenAI key's shape that no account holds. The runner never prints, logs or writes it, and `secrets.log` keeps the CLI's answer alone. A call that sends it is turned down by the service (`401`), inside a tunnel Canyon counts as allowed. The app's own log can still carry the service's masked echo of it (OpenAI names its last four characters).

`deployed: preview` has the runner deploy the seed to preview as the tester before the first turn
(`canyon deploy --format ai`, its output in `setup.log`), so the user's app is already running when
they first speak; anything but a review that PASSED stops the scenario before any turn.
`deployed: prod` goes on: the runner puts the seed in production as the tester would (`canyon deploy --env prod --yes
--format ai`, its output in `setup-prod-1.log`). Then, when the scenario has a `## Next release` (its files written
the way `## Seed`'s are), the runner writes those files over the seed and commits them. It deploys that release to
preview (`setup-2.log`) and puts it in production too (`setup-prod-2.log`). Anything but a promotion that exits 0 and
says PROMOTED stops the scenario before any turn. So does a release whose package or image the status read doesn't
name, and a next release that leaves production on the first's package or image, since no turn could be told apart
from it. The runner keeps the package and image digest production ran after each release in `released.json`. Once the
turns are over, however they end, it keeps what production runs in `prod-last.json`. `expect prod: first release` needs
a `## Next release`, and is judged on those two files, never on whether a turn promoted. Both the package and the
digest must match: a rebuild of the first release's code can carry its digest under a new package.
Production reads `first release`, `last release`, `another version`, `nothing in production` or `not read`, so an
undeploy, a code change and a new deploy all fail it (#2160). `egress:
preview allowlist` (any number, `preview` or `prod`) then puts that app env under `allowlist` as its
owner may from any session (contracts/routes.md, `PUT /api/v1/projects/:id/egress/:env/mode`).
`observe` is refused: it widens, which only a person does, in the dashboard. It needs `deployed:`:
the route refuses an app Canyon runs nothing of (`409 floor_not_enforced`). Restrict every env the
agent could reach, or a promotion to one that follows the org's `observe` is a way around a refusal.

The runner judges two expects against Canyon's records of what the app called out to
(`GET /api/v1/projects/:id/egress`, read as the tester), never against a block the agent read, each
naming one or more destinations: `REFUSED api.openai.com`, an egress request filed for it, and
`REACHED api.openai.com`, a call to it that went through (allowed, answered 2xx) after the refusal
the run waited for, in the env that refused it: a call that went through in an env the run never
saw refuse it (a promotion to a production that allows it) reads `NOT REACHED`. A heading `### 1 (once Canyon refuses a call out)` waits until an env of the
app holds a pending egress request, reading every 20 s for ten minutes, keeps that read as
`outbound-first.json`, and is what `expect first` is judged against; turn 1 may carry it under
`deployed:`. A heading `### 3 (once a person allows it)` prints the destinations and the app's
Outbound card, for the operator to ask a person on the app's team to click Allow there, then waits
up to an hour until each destination that read held pending is on its env's allowlist (a Dismiss
never ends it). Once the turns are over the runner reads the records again, for up to three minutes
until they hold an outbound `expect last`, and keeps the read as `outbound-last.json`, which it is
judged against. The scorecard lists both reads, and among Canyon's results what `canyon status`
told the agent of the destinations it refused, with what the agent did next.

`## Seed` is the user's existing app, each file as ``File: `path` `` and a fence, committed before
the first turn; `## Turns` has one `###` and fence per user message, word for word. A heading
`### 2 (only if the last result is BLOCKED or FAILED)` sends that turn only while the last Canyon
result is BLOCKED or FAILED: a "do what Canyon says" is pointless once the agent fixed it on its own.
Otherwise the runner logs `turn 2: skipped, nothing to fix (last result PASSED)`, leaves
`turn-2.skipped`, and the scorecard lists it; the last result is then the earlier turn's. Any other
text in parentheses is an error, never a silent always-send. Never mark a turn whose prompt is
the thing under test (a refusal like "switch the check off"): skipped, the scenario tests nothing.
A heading `### 2 (once Canyon asks to approve its fix for production)` waits, before sending the turn,
until the app's status reads production's patch `asked` (contracts/routes.md, `envs.<env>.patch`):
Canyon has patched preview and wants a person's approval. It reads the status every 20 s, prints each
state as it changes, and after an hour fails the scenario naming the last one; what makes Canyon
patch (a newer run image on the host) is the operator's, outside the turns.
`## What good looks like` is the reviewer's yardstick.

`{{FAKE_KEY}}` (a key leaked in the user's code) and `{{NEW_KEY}}` (the rotated one they paste)
become `sk_live_…` keys built at run time (`{{APP}}` becomes the app's name, for a seeded
`canyon.toml`), kept mode 600 in `<name>/.fakekey`, redacted from
scorecards.

## Through Canyon's MCP tools

From the release that ships `canyon mcp`, `canyon skill install` also writes Canyon's MCP server into each
agent's config: `mcpServers.canyon` in Claude Code's `~/.claude.json`, `[mcp_servers.canyon]` in Codex's
`~/.codex/config.toml` (design/specs/mcp.md §1.4). Every Claude turn allows the server's tools
(`mcp__canyon`), and the scorer reads a call to one as it reads the CLI verb it stands for:
`canyon_check` a check, `canyon_deploy` a deploy, `canyon_promote` a production deploy, its text result
the same `--format ai` block. Codex's `mcp_tool_call` items read the same way.

A scenario's MCP lines, each judged in its verdict table and summed up in the summary's `mcp` column:

- `via: mcp`: the agent should reach Canyon through the tools. Before turn 1 the runner refuses a tester
  HOME whose agent config has no canyon server running `canyon mcp`. The scorecard counts tool calls
  against `canyon` commands in a shell, and passes only on tools alone.
- `expect mcp: connected` (Claude Code alone): every turn's session init lists the `canyon` server
  connected.
- `promote from turn: N`, or `never`: the first turn whose user asked for production. A promotion in an
  earlier turn, by `canyon_promote` or `canyon deploy --env prod`, fails it, and the scorecard says
  whether the call answered for the person (`confirm`, or `--yes`).
- `playbook: <slug>`: an org playbook a custom rule points at. An admin writes both in the dashboard
  before the batch; the runner checks the playbook exists as the tester and stops the scenario
  otherwise. The scorecard says whether the agent read it (`canyon_playbook`, or `canyon playbook`),
  and whether the session hook synced it as a skill (`canyon-<slug>/SKILL.md` and its `.canyon` marker
  in the agent's skills) once the turns were over.
- `expect first: none`: the agent should get no review, promotion or query result at all, as when it
  only reads an app's logs.
