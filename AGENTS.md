# AGENTS.md

Instructions for coding agents working in this repository. People start with
[CONTRIBUTING.md](CONTRIBUTING.md), which has the same rules for outside contributors.

Canyon reviews every deploy coding agents make and runs the apps that pass: a Go server and CLI, an
executor that scans, builds and runs apps, and a React dashboard. **Canyon** is the only name users
see; `getcanyon-ai` in the Go module path is not a product name.

## Where things live

| | |
|---|---|
| Binaries | `cmd/`: `canyon` (the CLI), `canyon-server`, `canyon-executor` (Docker), `canyon-executor-kube`, `canyon-identity` (the apps' sign-in sidecar) |
| The CLI and the skill it installs | `cli/`, `cli/skill/canyon.md`; its printed output is golden in `cli/testdata/` |
| The review | `gate/` (scanners, the AI review, the 21 rules in `gate/policy/canonical.go`) · `runs/` (durable runs, deploys, promotion) |
| Running apps | `executor/` (Docker and Kubernetes), `provisioners/` (app databases), `identity/` |
| The dashboard | `web/` (React, Tailwind; tokens generated from `designsystem/canyon/`) |
| **The five data contracts**: findings, events, routes, executor, snapshot | `contracts/`: read the relevant one BEFORE writing anything that produces or renders these shapes |
| Product decisions and canonical strings | `design/EXPERIENCE.md` (the D5 strings are byte-exact product surface, never paraphrase) |
| Who may see and do what | `design/PERMISSIONS.md`: every route, screen and CLI verb that touches an app follows it |
| How the system works | `design/ARCHITECTURE.md` |
| Install and operate | `deploy/k8s/`, `deploy/helm/canyon/`, `docker-compose.yml` (append-only; the E2E stack adds `e2e/compose.e2e.yml` and never edits it) |
| Canyon's own design registry: tokens, the seven looks, the badge | `designsystem/canyon/README.md` |
| The docs site | `docs/` ([docs/README.md](docs/README.md)) |
| Tests beyond unit tests | `e2e/` (the stack, the browser journeys, clusters, hand-off) · `qa/agent/` (headless agent journeys) · `scripts/tour-check.sh` (the docs' tour on the demo) |

Conflicts: `EXPERIENCE.md` wins on product, `ARCHITECTURE.md` on system design,
`PERMISSIONS.md` on who may see and do what, and the losing text gets fixed **in the same PR** as
the code. One doc that can drift, not two.

## Commands

Every command is a `Makefile` target and CI runs the same targets: the table is in
[CONTRIBUTING.md](CONTRIBUTING.md#set-up). CI runs on Canyon's own runners (`runs-on` label
`canyon-arm`, on spot; the jobs of 7 minutes or more ask for `canyon-arm-ondemand`), so a push is
cheap: **push and let CI run the checks** — every one your change's
paths call for (the race and database tests, lint, the stack, e2e-ui, kind); the journeys,
e2e-ui and the tour run nightly on `main` when it moved since the last green nightly, or by hand, and a red
run on `main` opens or comments on one issue labelled ci. On the laptop run only
the quick, narrow tests: `make test PKG=./cmd/canyon-server RUN=TestServer`, then
`make test-affected`, every package your change can reach. **Never run `make preflight` or any
e2e (`make e2e`, `make e2e-ui`, `make test-kube`, `make tour-check`) locally**, except to debug a red CI check.
**Keep this file true: if a change makes any line here false, the same PR fixes it.**

## The five rules

1. **No PR without an issue.** The issue quotes the design doc verbatim, states acceptance as
   runnable commands, and declares its directories.
2. **Show the gate failing.** Every new test, check, or scanner rule is demonstrated red
   before green: plant a behavioural fault, run the check (a quick unit test locally; anything
   heavier by pushing the fault and letting CI run it), paste the failing output in the PR, then
   push the green. Confirm in the CI log that the check ran on the diff — the test is
   listed as run, not skipped.
3. **One PR per change, one branch off `main`. No stacked PRs.** Batch related small fixes into
   the PR at hand (or one follow-up), never one PR each. PR size is no objection.
4. **Never two agents in one file.** Merge one branch at a time.
5. **A seam change and its code go in ONE PR, the seam change as its first commit**, so a reviewer checks the code against it. The five seams live in `contracts/` (findings · events · routes · executor · snapshot).
   Each gets a CI check — a seam without a failing check is a document, not a contract.
   Emitting or rendering one of these shapes? Match the contract file byte-for-byte on field
   names and enums; if the contract seems wrong, stop and raise it — never improvise around it.
   The seam is `contracts/*.md`, `contracts/schema/*.json` and `contracts/testdata/**`; the
   Python and Go checkers in `contracts/` are implementations and follow the normal PR rules.

## Hotspots

Hotspots are structural, not remembered: routes register per-track (`routes_<area>.go`);
migrations are numbered in order (`0001_init.sql`, `0002_…`) and **never edited after merge**; `docker-compose.yml` is
append-only, one service per PR.

## Style

- **Solve the verified problem.** No speculative layers, no defense against hypothetical
  threats, no abstraction before a pattern repeats 3×. Delete before adding. Smallest
  working diff.
- **No heuristics.** Use real parsers, linters, and schemas — never regex guesses about code
  structure.
- **Fail closed on every gate path.** Missing, unparseable, or unknown state blocks — it
  never passes. This is the product; treat it as a hard invariant.
- **No secrets anywhere but the vault**: not in code, logs, findings, events, or test
  fixtures — names, never values.
- **Canyon** is the only user-facing name.
- Go (server, CLI) · React + Tailwind (dashboard). Scanner rules ship with must-fail AND
  must-pass fixture apps; vendored OSS rules (`gate/opengrep/sast-rules.tar`) are not ours to
  fixture — a probe app per language in `gate/opengrep/testdata/probes/` must report its rule, and
  CI rebuilds the archive from the pinned commit. Vendored rules may be LGPL-3.0 only unmodified,
  each file's header kept, with the LGPL-3.0 and GPL-3.0 texts in the archive and a NOTICE naming
  the source and tag, and rebuildable by an operator (`make vendor-sast-rules`). No dead code, no TODO comments — file an issue instead.

## Conventions

- **Commits:** `type(scope): imperative subject` ≤72 chars — e.g. `feat(gate): add
  sql.string-concat scanner`. PR titles the same. Never `--no-verify`, never force-push.
  Sign off every commit (`git commit -s`): the DCO line CONTRIBUTING.md asks for.
- **This is the only AGENTS.md.** No per-directory agent files — propose changes to this one.
- **New dependencies:** MIT/Apache/BSD/MPL/ISC/0BSD licenses only, except CC0-1.0
  (github.com/therootcompany/xz), MIT AND PostgreSQL (github.com/jackc/pgerrcode) and OFL-1.1 (the IBM
  Plex, Space Grotesk and JetBrains Mono fonts), each in `exceptions` in internal/tools/thirdparty/main.go, the reviewed list the
  generator holds and fails closed beyond; verify the module is already in `go.mod`/`package.json`
  before importing; a new one is named in the PR body, and `make third-party-notices` adds it to
  `THIRD_PARTY_NOTICES` (a Go module's licence first goes in `internal/tools/thirdparty/licences.go`).
- **Fixtures** live in `testdata/` dirs — invisible to `go build`, `go vet`, and golangci-lint
  by Go convention, which is why fixture apps live there and nowhere else. Per scanner rule:
  `gate/scanners/<rule>/testdata/must-fail/` and `must-pass/` (one app each, the smallest that
  trips / passes the rule). Shared fixture apps: `gate/testdata/apps/<app>/` (the canonical
  claims-portal). Golden contract fixtures: `contracts/testdata/<seam>/`. Fixture credentials
  are synthetic shapes (`sk_live_` + filler), never real values.
- **A test that reads another directory's Go source** (as `deploy/k8s` reads the server's) goes in
  `readers` in `internal/tools/testsel/main.go`, or `make test-affected` misses it: no import graph
  shows such a read. Reading its own directory's source needs nothing.

## Engineering rules

- Never touch an existing migration. Change a seam only as rule 5 says: the first commit of the
  PR whose code needs it.
- Rule ids (`secrets.client-bundle`, …) and the D5 strings are load-bearing — byte-exact.
- **Permissions are part of every feature.** Anything that reads or changes an app names its
  minimum level from `design/PERMISSIONS.md` (in `contracts/routes.md` or its spec) and what a
  caller below it sees; its tests cover one allowed and one refused caller at that boundary; the
  QA plan covers owner, contributor, read-only, no access and admin. A change to the model is a
  change to `design/PERMISSIONS.md` first, as the first commit of the PR that builds it (rule 5).
- A new vault-sealed table is added in THREE places: `main.go`'s two boot-invariant checks AND
  `cmd/canyon-server/rotate.go` (the `rewrapOne` list + the remaining count), with
  `rotate_test.go`'s `sealedRow` enumeration.
- A release that adds, removes or renames a DBOS workflow, or changes one's input or steps so a run the
  release before journaled would not resume, bumps `durableVersion` and records the new set under it in
  `workflowSets` (both `runs/durable.go`), leaving every recorded set as it is; `runs.Launch` refuses to
  start until it does. A server claims only its own version's work, so a shared version lets the release
  before claim a workflow it has no handler for (#2271). A release that bumps it rolls back only to the
  release that brought #2271 or a later one (earlier ones cannot take its work over), and its release
  notes say so. (2026-10-15)
- gitleaks: a `.gitleaksignore` fingerprint pinned to a PR-branch commit dies at squash-merge
  (main's push scan is full-history; the squash commit is new). For test fixtures use a
  `.gitleaks.toml` path AND value allowlist (like claims-portal) or an inline
  `// gitleaks:allow` — never branch fingerprints.
