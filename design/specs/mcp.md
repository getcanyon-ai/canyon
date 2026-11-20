# Canyon over MCP

Status: proposed. Nothing here is built. The seam changes in §5 ship with their code, each as the
first commit of the PR that needs it (AGENTS.md, rule 5).

Canyon reaches a coding agent today through three things `canyon skill install` writes: a skill, a
session hook that runs `canyon manifest`, and the `canyon` CLI the agent shells out to. MCP adds a
fourth door with typed tools, typed results and server instructions. It comes in two forms, designed
together:

- **Local:** `canyon mcp`, a stdio server inside the CLI binary, signed in as the CLI is.
- **Remote:** `/mcp` on canyon-server, streamable HTTP, behind Canyon's own OAuth.

Both expose the same tools with the same names, arguments and results. Remote has no tool that reads
local files, and adds two tools that work from a linked repository instead (§2.4).

**What doesn't change:** the CLI stays complete, and MCP is never the only way to do something. A tool
is a route the CLI already calls, with the same level, refusals and audit rows. MCP adds no
permission and widens none (§4).

## 1. Local: `canyon mcp`

`canyon mcp` speaks MCP over stdio, using the official Go SDK (§9). It reads the host and session as
every other verb does: `CANYON_HOST`, then `~/.config/canyon/config.json`. A machine with no session
starts the server anyway. Every tool then answers `isError: true` with the CLI's own not-signed-in
line, so the agent tells the person to run `canyon login`. The server never runs a login itself,
because a device code shown inside a tool result would have the agent driving the person's sign-in.

### 1.1 Tools

| Tool | Does | Calls | Result |
|---|---|---|---|
| `canyon_rules` | The org's rules, and the app's additions with `app` | `GET /api/v1/skill/manifest?app=` | the manifest, typed (`guardrails.schema.json#/properties/manifest`) |
| `canyon_check` | Reviews a directory without deploying, as `canyon check` | `POST …/packages` (a check's), `POST …/gate-runs`, then polls | the verdict (below) |
| `canyon_deploy` | Deploys a directory to preview, as `canyon deploy` | `POST …/packages`, `POST …/deploys`, then polls | the verdict, and on success the deploy's `url`, `answered` and access line |
| `canyon_status` | The app's environments, receipt and needs, as `canyon status` | `GET …/status` | the status read, as the caller's level trims it (§4) |
| `canyon_logs` | The app's last lines in one environment | `GET …/logs?env=&lines=&redaction=masked` | text, at most 500 lines |
| `canyon_promote` | Promotes what preview runs, or puts back an earlier version, as `canyon deploy --env prod [--version]` | `GET …/status`, `POST …/promotions`, then polls | the promote block's fields, or the approval URL when a person approves production |

- **Arguments.**
  - `dir` defaults to the server's working directory. The app is the one `canyon.toml` names there, or `app`.
  - `canyon_logs` takes `env` (`preview` or `prod`) and `lines`.
  - `canyon_promote` takes `version` and `reason` for a rollback, and `confirm`.
  - Every argument is validated as the CLI validates its flags, and a refusal is the CLI's own line.
- **Results reuse the findings contract.**
  - A verdict's `structuredContent` is `{run, state, failure_reason?, blocking, warnings, findings}`. `findings` holds the run's results in Canyon's SARIF profile (`contracts/finding.md`), byte for byte as `GET /gate-runs/:id/sarif` serves them, and each tool's `outputSchema` references `finding.schema.json`.
  - The text `content` is the CLI's `--format ai` block for the same outcome (`design/specs/cli.md` §5.2), so an agent that reads only text reads what it reads today.
  - No tool result is a new shape where an existing one fits.
- **Annotations.**
  - `canyon_rules`, `canyon_status` and `canyon_logs` are `readOnlyHint`.
  - `canyon_check` writes a check's package and a run.
  - `canyon_deploy` and `canyon_promote` are `destructiveHint: false, idempotentHint: false`.
  - `canyon_promote` is `openWorldHint`, because it changes what customers reach.
- **Promotion asks.**
  - `canyon_promote` sends nothing until it has an answer. Where the client supports MCP elicitation, the server asks the person: `Promote <app> <version> to production?`, the CLI's §6.3 question.
  - Elsewhere it needs `confirm: true`, which the server instructions say to set only when the person asked for production in this conversation. That's the skill's rule for `--yes`.
  - When the org has a person approve production, the tool asks nothing and returns the approval URL, as the CLI's WAITING FOR APPROVAL block does.
- **Long runs.** A check or deploy outlasts most clients' tool-call timeout.
  - The tool sends MCP progress notifications from the run's timeline (seam 2), one per `progress` row.
  - Past 10 minutes it returns `state: "running"`, the run id and the fleet URL. `canyon_status` then reads where it ended, as the CLI's "keeps going without this terminal" line does today.

### 1.2 No secret value passes through a tool

- **No tool sets, reads or returns a secret's value.** There is no `canyon_secret_set`. Where a value is missing, a tool says so by name, with the line the CLI already prints: the person runs `canyon secret set NAME [--env prod]` in their own terminal, and the agent never asks for the value in the chat.
- `canyon_logs` always asks for `redaction=masked`. A log line holding a secret's value reaches the model as the secret's name, never the value.
- Arguments are never logged. A tool's audit row is the route's own, as today.

### 1.3 Instructions and resources

- **The server's `instructions`** are the org manifest as `canyon manifest --harness <h>` renders it today (cli.md §7.4), read once at `initialize`. A client that puts instructions in the model's context therefore gives the agent the org's rules with no hook. The session hook stays where it's installed, because a client may ignore instructions.
- **Resources**, so a client can attach one on demand:
  - `canyon://manifest`: the typed manifest.
  - `canyon://manifest/<app>`: the manifest with that app's additions.
  - `canyon://playbooks/<slug>`: one playbook's body (§3).
- A resource read is the same route call as the tool, at the same level.

### 1.4 `canyon skill install` writes MCP config too

Install writes the skill, the hook and the MCP config together for each harness it finds, and
`uninstall` removes all three. Each file keeps everything that isn't Canyon's, byte for byte, as the
hook entries do today (cli.md §7.1).

| Harness | Skill (today) | Hook (today) | MCP config (new) |
|---|---|---|---|
| Claude Code | `~/.claude/skills/canyon/SKILL.md` | `hooks.SessionStart` in `~/.claude/settings.json` | `mcpServers.canyon` in `~/.claude.json`, user scope: `{"type":"stdio","command":"<this canyon's path>","args":["mcp"]}` |
| Codex | `~/.agents/skills/canyon/SKILL.md` | `hooks.SessionStart` in `~/.codex/hooks.json` | `[mcp_servers.canyon]` in `~/.codex/config.toml`: `command = "<path>"`, `args = ["mcp"]` |
| Cursor | `./.cursor/rules/canyon.mdc` | `hooks.sessionStart` in `./.cursor/hooks.json` | `mcpServers.canyon` in `./.cursor/mcp.json`: `{"command":"canyon","args":["mcp"]}`. It names `canyon` bare, as the hook does, because the project file is often committed |
| AGENTS.md | `./AGENTS.md` block | none | none: AGENTS.md names no harness to configure |

- **Parsers.** `~/.claude.json` is Claude Code's own state file, which it rewrites while it runs. Install edits only the `mcpServers.canyon` key, through a real JSON parser, re-reading the file just before the write. `config.toml` is edited through `github.com/pelletier/go-toml/v2`, already in `go.mod`. Neither is ever patched by text.
- **Paths are verified first.** The build PR checks each of these paths against the harness versions it names, as cli.md §7.1 records for the hooks.
- **The skill's text changes once MCP is installed.** It names the tools first and the CLI second: the CLI stays the fallback when the client hasn't loaded the server.

## 2. Remote: `/mcp` on canyon-server

### 2.1 Transport

- **Endpoints.** `POST /mcp` and `GET /mcp` carry MCP's streamable HTTP transport. `DELETE /mcp` ends an `Mcp-Session-Id`.
- **Origin.** The server validates `Origin` against the host's public URL, as MCP's streamable HTTP transport requires, and refuses any other with `403`.
- **Where it's served.** The route serves only on the host's public URL behind TLS. A plain-`http` public URL serves no `/mcp`.

### 2.2 Authorization

Canyon follows the MCP authorization specification: `/mcp` is an OAuth 2.1 protected resource.

- **Protected-resource metadata** (RFC 9728) is served at `/.well-known/oauth-protected-resource/mcp`: `resource` is `<public URL>/mcp`, and `authorization_servers` is `[<public URL>]`. A request with no token is answered `401` with `WWW-Authenticate: Bearer resource_metadata="<that URL>"`.
- **Canyon is the authorization server**, as it already is for the CLI. Canyon runs the device flow itself rather than asking the customer's identity provider to issue CLI tokens (design/AUTH.md §4.2), and for the same reason it doesn't delegate MCP clients to that provider:
  - most corporate providers don't register arbitrary MCP clients;
  - their tokens would name an audience Canyon doesn't control.
- **What Canyon serves:**
  - authorization-server metadata (RFC 8414) at `/.well-known/oauth-authorization-server`;
  - `GET /oauth/authorize`, with PKCE `S256` required and `resource` (RFC 8707) required, equal to `<public URL>/mcp`;
  - `POST /oauth/token`, for `authorization_code` and `refresh_token`;
  - client identification by Client ID Metadata Documents, with Dynamic Client Registration (RFC 7591) at `POST /oauth/register` as the fallback the specification allows (open question 2).
- **How a client signs in:**
  1. The client opens `/oauth/authorize` in the person's browser.
  2. Canyon's own browser sign-in runs there (AUTH.md §4.1), against the customer's identity provider, unchanged.
  3. Canyon shows a consent screen: the client's name and redirect host, which account, and what it may do. "Everything you can do in Canyon, as you" is the only scope; MCP adds no narrower one in v1 (open question 3).
  4. The person allows it.
- **The token.** Canyon issues an access token that is a Canyon session:
  - `flow: oauth`, a new value beside `browser | device | demo`;
  - bound to the resource and to the client id;
  - expiring as a CLI session does.
  - It is a person's session, so a tool acts as that person: `kind: person`, recorded under their name (AUTH.md §4.4).
  - `dashboard_required` refuses it, as it refuses a CLI session.
- **Revoking.** Sign-outs revoke these sessions like any other (`DELETE /api/v1/sessions`). Settings › People & sign-in lists them with their client's name, through a new sessions list (§5).
- **With sign-in off, it fails closed.** On a host with `CANYON_AUTH` off, such as the laptop demo, Canyon serves neither `/mcp` nor the metadata or OAuth routes. `/mcp` answers `404 {error:"mcp_needs_sign_in", message}`, D5's line naming `canyon mcp` as the way on this machine. A host where every caller is the anonymous admin must not hand that power to anything that can reach a port.

### 2.3 Tools

The remote server has every local tool except `canyon_check` and `canyon_deploy`, which read a local
directory. It adds two tools that work from a linked repository:

| Tool | Does | Calls |
|---|---|---|
| `canyon_check_ref` | Reviews a linked repository's ref without deploying | `POST …/packages/from-repository {ref}` (new, §5), then `POST …/gate-runs` |
| `canyon_deploy_ref` | Deploys a linked repository's ref to preview | the same package, then `POST …/deploys` |

- **The server already supports linked repositories.** The GitHub App links an app to a repository or a folder of it (`PUT …/repository`, admin), and on a push its check workflow packages the commit (`github-app/work.go`, `pack`). What's missing is packaging a ref on request.
- **The new route** resolves `ref` to a commit through the app's installation and packages it with the same `pack` path, a folder link's included. It answers `404 not_linked` for an app with no link.
- **Unchanged rules.** A pull request's package goes to preview only as the deploys row allows today: never from this route.
- **`instructions` and resources** are §1.3's, read on the server, for the caller.

### 2.4 Pushing it to an org's agents

A platform team rolls out the remote server, and optionally the session hook, through each agent's
org-managed settings. Docs ship one page per agent (`docs/start/agents/`) with the exact block.

| Harness | MCP server, org-managed | Session hook, org-managed |
|---|---|---|
| Claude Code | `managed-mcp.json` beside the managed settings: `{"mcpServers":{"canyon":{"type":"http","url":"https://<canyon>/mcp"}}}` | `hooks.SessionStart` in `managed-settings.json`, as `skill install` writes it today, if the CLI is on those machines |
| Codex | `[mcp_servers.canyon]` with `url = "https://<canyon>/mcp"` in the managed config | the managed `hooks.json`, where Codex reads one |
| Cursor | the team's MCP settings, where Cursor's admin plan offers them | `hooks.json` committed to the repositories the team uses |

- **Remote needs no hook.** A remote-only org needs no CLI on the machine: the server's `instructions` carry the manifest. The hook is for orgs that also run the CLI.
- **These paths change between harness releases.** The docs PR checks each one against the harness version it names, and none of them is hard-coded in Canyon.

## 3. Org playbooks

Platform teams keep their own skills and architecture notes in Canyon, and Canyon hands them to
every agent the way it hands out the rules.

- **What a playbook is.** `{slug, name, kind, when, body, version, updated_by, updated_at}`:
  - `kind`: `skill` or `note`;
  - `when`: one line on when to read it, at most 200 characters, which the manifest carries;
  - `body`: Markdown, at most 64 KiB.
  - An org keeps at most 50.
- **Where they're kept.** Guardrails › Playbooks. Writing one is an admin's act, a person's, from a browser (`person_required`, `dashboard_required`), as a custom rule's instructions are.
- **Versions and audit.**
  - Every save is a new `version`, and every version is kept.
  - The audit log gets `set playbook` (`detail{slug, version, diff}`) and `remove playbook`.
  - A removal keeps its versions, readable to admins.
- **How they reach agents:**
  - The manifest gains `playbooks[]`: `{slug, name, kind, when, version}`, never the body, so the session hook's context stays small.
  - MCP serves each body as `canyon://playbooks/<slug>`, and the server `instructions` name them.
  - `canyon playbook <slug>` prints a body for hook-only harnesses.
- **What a body must not hold.** It is org-written text handed to a model, so the save refuses one that holds anything Canyon's secret scanners flag, as a custom rule's instructions are held.
- **Free or Enterprise?** Open question 1.

## 4. Permissions

Each tool names the route it calls, and that route's minimum level and refusals apply unchanged.
Below the level, a tool answers `isError: true` with the route's own message, and never a partial
result it wasn't allowed.

| Tool / resource | Minimum level | Below it |
|---|---|---|
| `canyon_rules`, `canyon://manifest` | signed-in member | not signed in: the CLI's login line |
| `canyon_rules` with `app`, `canyon://manifest/<app>` | viewer on the app | `team_required`, with the owners to ask |
| `canyon_status` | viewer; contributor for technical detail | a viewer gets the reduced read; a responder with no grant gets the receipt-only read (PERMISSIONS.md, *Security responders*) |
| `canyon_logs` | contributor | `team_required`, or `403` with the reduced-read message |
| `canyon_check`, `canyon_check_ref` | contributor | `team_required` |
| `canyon_deploy`, `canyon_deploy_ref` | contributor | `team_required` |
| `canyon_promote` | contributor, and not held | `team_required` · `409 held` · the approval URL where a person approves production |
| `canyon://playbooks/<slug>` | signed-in member | not signed in |
| Playbook writes | admin, a person, in a browser | `admin_required` · `person_required` · `dashboard_required` |

- **Unchanged by MCP:**
  - Approving a finding (blocking findings stay a browser's, never the author's).
  - Holding and lifting.
  - Sharing.
  - Every widening of outbound.
  - Every Settings change.
- **No tool approves, holds, shares, or changes a policy, a setting or outbound.** These stay in the dashboard.
- **The QA plan** covers owner, contributor, read-only, no access and admin, as PERMISSIONS.md's *Applying it* asks, for the local and the remote server alike.

## 5. The seams it touches

Named here, and edited later in the PR that builds each.

- **contracts/routes.md:**
  - `/mcp` (POST, GET, DELETE), and the two `.well-known` metadata routes;
  - `/oauth/authorize`, `/oauth/token` and `/oauth/register`;
  - `POST /api/v1/projects/:id/packages/from-repository {ref}`;
  - the playbook routes (`GET /api/v1/playbooks`, `GET|PUT|DELETE /api/v1/playbooks/:slug`, `GET …/:slug/versions`);
  - `playbooks[]` on `GET /skill/manifest`;
  - a session's `flow` gaining `oauth`;
  - a sessions list, `GET /api/v1/sessions`, new: a person's own sessions, and every session to an admin, with each OAuth client's name;
  - the `Canyon-Transport` header (§7);
  - each new route's minimum level in the Level table.
- **contracts/schema/guardrails.schema.json:** `playbook`, `playbook_put` and `playbook_versions`, and `manifest.playbooks`.
- **contracts/schema/telemetry.schema.json:** `agents_via` (§7).
- **design/PERMISSIONS.md:** an *MCP* section (a tool is its route; an OAuth session is a person's session; consent is a browser's), and *Playbooks*.
- **design/EXPERIENCE.md** (D5, D7):
  - the consent screen's lines;
  - the instructions' preamble;
  - `mcp_needs_sign_in`'s line;
  - elicitation's question (the CLI's own);
  - the playbook screen's strings;
  - the command count going from twenty-two to twenty-four (`mcp`, `playbook`).
- **design/specs/cli.md:** `canyon mcp` and `canyon playbook`, the skill-install rows (§7.1), and §7.2's skill text.
- **design/AUTH.md:** a new §4.7, Canyon as an OAuth authorization server for MCP clients.
- **runs:** a new DBOS workflow, if packaging a ref is one, bumps `durableVersion` (AGENTS.md, *Engineering rules*).

## 6. The PR sequence for the build

One PR per independent change, each off `main` and merged before the next starts, never stacked.
Each runs its acceptance in CI, and shows its new checks red first.

1. **`canyon mcp`, read tools.**
   - What: `canyon_rules`, `canyon_status`, `canyon_logs`, the instructions and the manifest resources. The SDK dependency is in this PR.
   - Acceptance: `make test PKG=./cli RUN=TestMCP` (an in-memory MCP client against a stub host) · `make third-party-notices-check` · `make test PKG=./cmd/canyon RUN=TestCLIIsApache`.
2. **Local write tools.**
   - What: `canyon_check`, `canyon_deploy` and `canyon_promote`, with elicitation and `confirm`, progress, and the 10-minute hand-off.
   - Acceptance: `make test PKG=./cli RUN='TestMCP(Check|Deploy|Promote)'` · `make demo-check` (gains a step that drives a check and a deploy through `canyon mcp`).
3. **`skill install` writes MCP config.**
   - What: the three harnesses, plus uninstall, and the skill text names the tools.
   - Acceptance: `make test PKG=./cli RUN='TestSkill(Install|Uninstall)MCP'` · `make test PKG=./qa/agent`.
4. **Telemetry, the collector first.**
   - What: `telemetry.schema.json`'s `agents_via`, and the collector's intake.
   - Acceptance: `python3 contracts/validate.py` · `make test PKG=./cmd/canyon-collector`.
5. **Telemetry, the server and CLI.**
   - What: `Canyon-Transport`, a `runs.transport` migration, and the report's `agents_via`. Merged once PR 4 is live.
   - Acceptance: `make test PKG=./cmd/canyon-server RUN=TestTelemetry` · `make test PKG=./cli RUN=TestMCPTransport`.
6. **Canyon as OAuth authorization server.**
   - What: the metadata routes, authorize, token, register, the consent screen, `flow: oauth` sessions, and the sessions list (`GET /api/v1/sessions`) in Settings.
   - Acceptance: `make test PKG=./cmd/canyon-server RUN=TestOAuth` · `cd web && npx vitest run src/screens/Consent.test.tsx` · `make e2e-ui` (a consent journey per level).
7. **Remote `/mcp`.**
   - What: the read tools and `canyon_promote`, instructions and resources, and the sign-in-off refusal.
   - Acceptance: `make test PKG=./cmd/canyon-server RUN=TestMCPRemote` · `go test -tags oidc -run TestMCP ./e2e/oidc/` (the stack with sign-in, an MCP client through the full OAuth flow).
8. **Packaging a linked repository's ref.**
   - What: the route, and `canyon_check_ref` and `canyon_deploy_ref`.
   - Acceptance: `make test PKG=./github-app RUN=TestPackRef` · `make test PKG=./cmd/canyon-server RUN=TestPackagesFromRepository`.
9. **Playbooks, the server and dashboard.**
   - What: the schema, routes, Guardrails › Playbooks, the audit, and the manifest's `playbooks[]`.
   - Acceptance: `make test PKG=./cmd/canyon-server RUN=TestPlaybook` · `cd web && npx vitest run src/screens/Playbooks.test.tsx` · `python3 contracts/validate.py`.
10. **Playbooks, handed out.**
    - What: `canyon playbook`, and the playbook resources on both servers.
    - Acceptance: `make test PKG=./cli RUN=TestPlaybook` · `make test PKG=./cmd/canyon-server RUN=TestMCPPlaybook`.
11. **Distribution.**
    - What: `server.json`, the registry publish in the release workflow, install links, and per-agent docs.
    - Acceptance: `make docs-site docs-links` · `python3 contracts/validate.py` (server.json against the registry's schema).
12. **The tour on MCP.**
    - What: §8.
    - Acceptance: `make tour-check` (also runs the tour through `canyon mcp`).

## 7. Telemetry

- **`Canyon-Harness`** keeps naming the agent, never the transport:
  - **Local server:** it reads the same environment markers as the CLI (cli.md §5.1), and failing those, the MCP client's `clientInfo.name` mapped through an exact table to `claude_code` or `codex`.
  - **Remote server:** it has only `clientInfo.name`, through the same table.
  - A name not in the table sends no harness, as today.
- **A new header, `Canyon-Transport`,** carries `mcp-local` from `canyon mcp`. The remote server records `mcp-remote` itself. Absent means the CLI. It's kept on the run beside `harness`.
- **The report gains `agents_via`:** `{cli, mcp_local, mcp_remote}`, counts of reviews by how they were asked for. It's still a count, never a name.

## 8. Distribution

- **MCP Registry.** A `server.json` at the repository root describes the local server. Its package is the CLI itself, as the registry's package type for a downloadable binary allows: per-platform archives from canyon-releases, each with its SHA-256. The release workflow publishes it with the registry's publisher tool. The remote server isn't listed: its URL is each org's own (open question 4).
- **Install links.** The docs give each agent its own:
  - Cursor's MCP install deep link, carrying `{"command":"canyon","args":["mcp"]}`;
  - VS Code's `vscode:mcp/install` link;
  - `claude mcp add canyon -- canyon mcp`;
  - `codex mcp add canyon -- canyon mcp`.
  - `canyon skill install` stays the one command that does all of it.
- **The tour switches to MCP.**
  - `canyon demo up` ends by running `skill install` (open question 5).
  - The tour's beats name `canyon_check` and `canyon_deploy` where the agent has the server, and the shell commands where it doesn't.
  - Each chapter is also served as an MCP prompt, `canyon_tour`, with `chapter` as its argument.
  - The website's one-line prompt is unchanged.

## 9. New dependencies

| Module | Licence | Why |
|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` (v1.8.0) | Apache-2.0 AND MIT (its LICENSE: new code Apache-2.0, earlier contributions MIT until relicensed) | the MCP server, both transports, and its auth helpers |
| `github.com/google/jsonschema-go` | MIT | the SDK's tool schemas |
| `github.com/yosida95/uritemplate/v3` | BSD-3-Clause | resource templates |
| `github.com/segmentio/encoding` | MIT | the SDK's JSON |
| `github.com/segmentio/asm` (indirect) | MIT-0 | through segmentio/encoding |
| `github.com/golang-jwt/jwt/v5` | MIT | the SDK's auth package |
| `golang.org/x/oauth2`, `x/time`, `x/tools` | BSD-3-Clause | the SDK's |

- **MIT-0 isn't on AGENTS.md's licence list**, so it needs an `exceptions` entry, or the SDK built without it (open question 6).
- **The CLI stays Apache-2.0.** Every module above is compatible, and `TestCLIIsApache` keeps the CLI's own packages so.

## Open questions

1. **Playbooks: free or Canyon Enterprise?** For the owner to decide.
2. **Client identification.** Client ID Metadata Documents alone, or Dynamic Client Registration too, for clients that don't support the former yet?
3. **Scopes.** One scope, "everything you can do, as you", or a read-only scope for remote clients from v1?
4. **The remote server in the MCP Registry.** List it with the URL left to each org, or keep remote out of the registry?
5. **The demo installs the agent config.** Should `canyon demo up` run `canyon skill install`, given it writes into the person's agent config?
6. **MIT-0.** Add `segmentio/asm` to the reviewed exceptions, or keep the SDK out of the CLI until it no longer needs it?
7. **The registry's namespace.** `ai.getcanyon/canyon` (domain-verified) or `io.github.getcanyon-ai/canyon`?
