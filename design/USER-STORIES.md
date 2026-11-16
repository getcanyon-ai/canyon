# Canyon — User Stories

One story per persona. Two personas land on the repo: the
**platform engineer** (primary) and the **builder** (secondary). The third comes from the
shared vocabulary in `specs/screens.md` — every action in Canyon is attributed to one of three
actors, *a person, an agent, or Canyon* — and the CLI's `--format ai`, the shipped skill, and
the `do not:` block are designed for that second actor. Canyon itself acts, but it is not a
user, so it has no story here.

Each story is written against the canonical experience in `EXPERIENCE.md` (D1–D12, D14 onward). Strings in
backticks that appear in D5 are byte-exact product surface. Where an older detail spec still
says `blocker/warn/note` or port `8080`, D1 and D3 win.

---

## 1. The platform engineer

**As** a platform engineer whose company has forty vibe-coded apps built by different people in
different AI tools, with no owners, no review, and secrets sitting in frontend bundles,
**I want** one command that shows me the whole fleet and a gate that refuses production until
an app is clean,
**so that** I can govern what my colleagues ship without becoming the person who hand-reviews
every app.

### Context

This is the visitor. They found the repo because they are actively searching for a way out.
Their first twenty minutes follow the map in `EXPERIENCE.md`: recognition, one
command, watching the gate stop something real, then their own app, then the governance
screens. Day 2 is the governor's arc: BYO infrastructure, policy tuning, onboarding builders,
the morning review queue, trends, the incident drill.

### Acceptance

- **One command, no signup.** `docker compose up` boots to the fleet at `http://localhost:7777`,
  bound to `127.0.0.1`. The first screen is the fleet, not a login. If the port is taken,
  compose fails loudly and says which port to change (D3).
- **The fleet is the accusation.** The four tiles read `2 secrets in client bundles` ·
  `5 apps never reviewed` · `2 apps with no owner` · `2 apps blocked`,
  over the eight Meridian Insurance apps, risk-sorted, with what each app runs in production and
  in preview, where its newest upload has got to, and its owner (D4). A never-reviewed row shows
  `Never reviewed` beside the app name and is never rendered as "0 findings";
  `Never reviewed. Canyon can't tell you whether this app is safe.` is said once
  per page, on the app page and the owner card, never per row (D5, D6).
- **The gate visibly stops something.** `claims-portal` shows three findings with file:line
  and the import-graph evidence for the Stripe key, and the verdict
  `✗ Blocked — 1 blocking finding`. The promote button is present, disabled, and labeled with
  the thing they want; the receipt lists what passed next to what didn't and reads
  `Meridian requires a passing review to reach production.` No tooltip carries any of this (D6).
- **The fix is theirs to trigger.** The dashboard control "Simulate Dana's agent pushing the
  fix" re-reviews; findings dim, never disappear, until the result lands. The receipt row flips,
  the button fills, promote confirms with
  `Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.`, and the production card
  carries `<version> · passed review`. The first fleet tile decrements live (D4, D6).
- **The hidden horrors surface.** Reviewing `quote-engine` (one click, about ten seconds, one
  bar) turns "Never reviewed" into a `sql.string-concat` critical. `Review all` runs in parallel
  and lands as one toast (D4).
- **Policy is the product.** The Policies screen lists the rule IDs of D2 with their English
  names, each in exactly one of `Off · Advisory · Blocking`. Changing a state shows an impact
  preview over the current fleet and offers an optional grace period in days (14 in the
  example). The copy states
  that policies gate promotion, never running apps (D2, D11).
- **The audit trail answers "what did the robots ship."** Every event is attributed to a
  person, an agent, or Canyon, and `People · Agents · Canyon` is a first-class filter.
  Overrides carry a free-text reason and a name, forever; resolved and approved are never one
  word (D6, `specs/screens.md`).
- **The host is honest about itself.** The demo banner reads
  `No auth in demo mode. Anyone who can reach this port is an admin — bound to localhost only.`
  and escalates to `This Canyon is reachable from the network and has no authentication.` when
  the listener or peer is non-loopback. Both are derived from the listener and peer address,
  never asserted (D5).
- **The model is their call.** With no `CANYON_MODEL_PROVIDER`, `CANYON_MODEL`,
  `CANYON_MODEL_URL`, `ANTHROPIC_API_KEY` or `CANYON_MODEL_TOKEN` on the server container,
  naming one of them without a credential that resolves fails the boot rather than
  half-offering the feature. With none of them, the AI review row renders
  `ai review · skipped (no model configured on this Canyon host)` and never passes silently
  (D10).

### Out of scope for this story

Fleet import, team mode, OIDC and roles, SSO wizard, provider registry UI, the 60-day trends
chart on a zero-day install (D11, `specs/screens.md` "What I cut").

---

## 2. The builder

**As** a builder who lives in an AI harness and has never heard of a review gate,
**I want** to ask my agent to deploy and get a shareable link back,
**so that** I can send my colleague the app without learning a new tool, changing how I work,
or reading a security lecture.

### Context

Dana Whitfield builds `claims-portal` in Claude Code (D4). The rule: the
builder must never have to change how they work. They stay in the harness, deploy, findings
come back as file:line feedback their AI can fix, redeploy, link. The gate feels like a
helpful senior engineer, not a cop. The asymmetry in `specs/cli.md` §7.3 is the whole
product: near-zero cost to the builder, complete governance for the org.

### Acceptance

- **No setup step exists.** `canyon deploy` runs from any app directory. There is no `init`,
  no YAML, no format flag to pick. If `canyon.toml` is missing, deploy creates it, and it
  holds only `app`; owner is server state (D7, D12).
- **The trust statement is on every run.** The package step prints
  `.env and node_modules excluded → localhost:7777` in the demo, naming the destination.
  The server publishes the exclusion list; `.gitignore` is never a gate input and git history
  is never ingested (D5, D12).
- **A block is feedback, not a wall.** When the gate blocks, every finding names a file and
  line, one sentence of why with the real consequence, a chosen fix and why, and what the fix
  doesn't cover ("rotate it"). The verdict ends with
  `Nothing was built and nothing was deployed.` and that sentence is literally true because
  the gate never builds (D8, D9).
- **The builder never pastes a secret.** When a leaked key is the finding, the fix adopts it
  with `canyon secret set <NAME> --from-gate`: the value is held encrypted for fifteen
  minutes, never printed, never logged, never in the API response, and the adoption is audited
  with a rotation nudge (D7, D11).
- **Check is deploy.** `canyon check` runs the identical server-side gate `canyon deploy`
  runs and stops before build. Same package hash, same scanners, same model, same policy
  snapshot; a re-check on the same package cannot flip the verdict (D7, D9).
- **A link comes back.** A passing deploy prints the preview URL,
  `http://<app>.preview.localhost:7777`. Cron apps get no link because they have no URL (D3,
  D11).
- **Advisory findings stay advisory.** The xlsx CVE is severity high under an Advisory policy:
  the agent mentions it in a sentence and fixes it only if asked. The gate is not hunting for
  excuses to block (D1).
- **The org gets everything for free.** After Dana's deploy, the fleet has a row with the
  harness `(detected)`, an owner they can claim, a clean gate run, a vaulted secret, and an
  audit trail. Nobody filed a ticket (`specs/cli.md` §7.3).
- **Missing config is not a policy block.** A boot failure from a missing variable is worded
  as *"this is the app missing a variable, not a policy block — set it with
  `canyon secret set`"* (D11).

### Out of scope for this story

Promoting to production. That is a person clicking promote on the dashboard under
`prod.human-approval`, and in v1 a solo click is enough (D11). Fixing findings is the agent's
story, below.

---

## 3. The builder's agent

**As** the AI agent that wrote the code and was asked for a link,
**I want** the gate's verdict in a shape I can act on without interpretation: what blocks,
where, the required end state, the exact commands, and the moves that don't count,
**so that** I fix what blocks, avoid re-tripping the same finding, hand the human a link in a
sentence or two, and stop with a named exit instead of looping.

### Context

Canyon's answer to "why not gitleaks, semgrep, and CI" is this persona: *CI tells a human.
Canyon tells the agent that wrote the code, in the format it already fixes compiler errors
in, and refuses the deploy until it's fixed* (D11). The agent is one of the three actor types
in the audit trail, attributed as `Dana's agent · Claude Code`. It is a courier's consumer:
all scanning, policy, and verdicts are server-side; the agent only renders and acts
(`ARCHITECTURE.md`).

### Acceptance

- **The format picks itself.** `--format auto` detects the harness from its environment
  variables and renders `ai`: no ANSI, no spinners, no redraws, stable ordering, `key: value`
  lines that survive being quoted into a context window. The detection table is empirically
  verified before implementation (D7, D11).
- **The first line is the verdict.** Output begins `CANYON REVIEW — RESULT: BLOCKED` or
  `CANYON REVIEW — RESULT: PASSED`, and a blocked verdict carries
  `Nothing was built and nothing was deployed.` (D5).
- **Every finding says whether it blocks.** Each finding carries `severity:` and an explicit
  `blocks: yes|no`, plus rule id, file, line, column, redacted source, `required end state`,
  a `suggested fix` with "Run exactly:" commands, and a `do not:` block naming the cheat
  moves: moving the value to a `NEXT_PUBLIC_`, `VITE_`, `REACT_APP_`, or `PUBLIC_` variable,
  moving the literal to `.env` or a comment, renaming, adding a suppression or `--force`.
  Each of those produces the same finding on the next run (D1, D7).
- **The skill keys off `blocks`, never severity words.** The shipped skill reads *"For each
  finding with `blocks: yes`…"*. It ships with the binary; `canyon skill install` writes it per
  harness, marker-delimited and idempotent, `--print`-able and auditable against the release.
  `canyon manifest` hands agents only typed data: host URL, active policy list
  (D7, `ARCHITECTURE.md` §4).
- **Secrets never transit the chat.** The agent runs the exact
  `canyon secret set <NAME> --from-gate` line the finding printed. Adoption resolves to the
  most recent gate run with a live candidate, so window ordering does not matter. The agent
  never invents, guesses, retypes, or placeholders a value (D7).
- **Exit codes are the contract.** `0` passed, `1` error, `2` blocked, and nothing else (D7).
- **Two rounds, then a named exit.** After two full fix-and-recheck rounds that still end
  BLOCKED, the agent stops, shows the remaining findings, and relays
  *"Ask your platform team to review this finding at `<url>` — they can override it with a
  reason."* It does not keep looping (D7, D11).
- **The human gets a link, not a lecture.** On PASSED the agent reports the `live url`, says
  in one or two sentences what the gate caught and why it mattered, and mentions advisory
  findings without fixing them unasked (D8, `specs/cli.md` §7.2).
- **Its work is attributed.** Every event the agent causes lands in the audit trail under the
  agent actor with the harness mark, filterable under `Agents` (`specs/screens.md`).

### Out of scope for this story

Inline suppressions, a `fix` command, a TUI, client-side scanning of any kind. The gate a
client runs is a gate a client can skip (D7, `ARCHITECTURE.md`).
