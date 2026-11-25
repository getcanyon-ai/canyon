# Contributing to Canyon

Thank you for helping. The most useful thing you can send is a **wrong finding**: a false positive,
or something the review missed, with the smallest code that shows it. A review people turn off
protects nobody, so both are bugs.

## Before you write code

Open an issue first, or comment on an existing one, so nobody builds the same thing twice. Say what
you'll change and how you'll know it works. Questions and ideas go to
[Discussions](https://github.com/getcanyon-ai/canyon/discussions); security problems go through
[SECURITY.md](SECURITY.md), never a public issue.

## Set up

You need Go (the version in `go.mod`), Node 22 with npm for the dashboard and the docs site, and
Docker 28 or newer with Compose v2 for the demo and the end-to-end tests. Every command is a
`Makefile` target, and CI runs the same targets:

| | |
|---|---|
| Set up | `make setup`: Go modules and the pinned golangci-lint |
| Build | `make build`: `bin/canyon-server` with the dashboard inside (needs Node), and `bin/canyon` · `make cli` builds the CLI alone |
| Run the demo | `make demo`, then open `http://localhost:7777` · `make demo-down` removes it and every app it started · `make demo-check` boots it, checks the image carries the dashboard and the demo apps, and tears it down · `make tour-check` runs the docs' tour on it as an agent does, and holds the page's outputs to that run |
| Dashboard | `make web` builds the bundle · `make web-test` runs its tests · `make web-dev` runs vite on :5173 against a local server |
| One test | `make test PKG=./cmd/canyon-server RUN=TestServer` |
| What a change can affect | `make test-affected`: the packages Go's import graph (under `-race`) says a change since `origin/main` reaches (`BASE=` for another ref), plus the declared readers of a changed directory's Go source. Every package runs instead when a changed file is not Go source (`go.mod`, `go.sum`, the `Makefile`, `db/migrations`, testdata, goldens, docs, contracts: tests open these across packages) or is Go source no package builds (a build tag, a testdata app) |
| Custom rules' cost bounds | `make test-bounds`: `gate/custom`'s and `internal/tools/ruleeval`'s tests that measure expressions at the facts' largest, without `-race` (under it they are skipped); CI runs it beside `make test-rest` |
| All tests | `make test-all`: every package, the server's and `runs`' tests each split into `N=2` shards run at once · `make test-shards` those two alone (`SHARD_PKG=./runs SHARD=i/n` one shard of one; CI runs the server's in 4, `runs` in 3) · `make test-rest` the rest |
| Most of what CI runs, on one machine | `make preflight`, to debug a red CI check. CI runs on GitHub's hosted runners, or on Canyon's own where the repository's `CANYON_RUNNERS` is `self-hosted`: push and let it run the checks your change calls for, and run only `make test` or `make test-affected` before pushing |
| Without Canyon Enterprise | `rm -rf ee && go build -tags noee ./...` builds Canyon without `ee/` (design/ARCHITECTURE.md §8); with `GOFLAGS=-tags=noee`, every `make test` target tests that build. CI's `noee` jobs do both, so a core change that leans on `ee/` fails there |
| Lint and tidy | `make lint` (golangci-lint, gofmt, goimports) · `make tidy-check` |
| The stack, end to end | `make e2e-up` (`MODEL=1` adds the stub model) · `make e2e` · `make e2e-ui` (`SHARD=i/n` one shard) · `make e2e-down` · `make e2e-ui-typecheck` type-checks the journeys · `make test-e2e` builds and serves the fixture apps with the real builder (Docker and pack, on Linux) |
| Kubernetes | `make test-kube` against the kind cluster `KUBECONFIG` names · `make e2e-handoff` the hand-off journey · `make e2e-cluster` the cluster suite against `$E2E_CANYON_URL` · `make e2e-cluster-egress` one app's way out there, with no fresh install (O1) · `make e2e-cluster-handoff-repo` its hand-off through a deploy repo alone (H4–H8) · `make test-cluster-unit` that suite's own pieces, no cluster needed |
| Scanners | `make opengrep-rules-test` runs the vendored rules over their probe apps · `make vendor-sast-rules` and `make vendor-sarif` re-vendor the rules and the SARIF schema (a diff after means upstream moved) · `make pack-install` installs the pinned pack CLI |
| Third-party notices | `make third-party-notices` regenerates `THIRD_PARTY_NOTICES`, the licence texts of every Go module the release binaries link and every npm package in the dashboard; it fails on a component with no licence file or a licence outside the list in AGENTS.md (a Go module new to a build list needs its licence, read by a person, in `internal/tools/thirdparty/licences.go`) · `make third-party-notices-check`, which CI runs. A dependency bump, Dependabot's included, that adds or drops a module or package or changes a licence text fails that check until `make third-party-notices` is run and its output committed on the bump's branch |
| Design tokens | `make tokens` regenerates the dashboard's and the docs site's CSS from `designsystem/canyon/dashboard.tokens.json` · `make tokens-check` |
| Docs | `make docs-site` builds the docs site into `docs/dist` · `make docs-cli` rewrites its CLI reference from the CLI's help · `make docs-screens` re-takes the admin guides' screenshots from the demo at localhost:7777 · `make docs-links` checks every relative link in the other Markdown (needs [lychee](https://lychee.cli.rs)) |
| CLI release | `make cli-release`: archives per OS and architecture plus `SHA256SUMS` in `bin/release/`; pushing a `v*` tag publishes them as a GitHub release ([RELEASING.md](RELEASING.md)) · `make install-test` builds a throwaway local release (`v0.0.0-installtest`, replacing `bin/release`) and checks `install.sh` installs it and refuses a tampered `SHA256SUMS` · `make images-release PLATFORM=linux/amd64 REPO=<registry>/<namespace>` pushes the nine images for one platform by digest (the release workflow sets both, and `SOURCE`) · `make images-load` builds them into the local Docker and checks each one's licence label and notices |

Working on Canyon itself? Set DO_NOT_TRACK=1 (or CANYON_TELEMETRY=0) so your local Canyon doesn't send usage reports.

## How Canyon is tested

- **Go and the dashboard:** unit tests beside the code (`go test`, Vitest). Database tests need a
  Postgres in `CANYON_TEST_DATABASE_URL`; without it they are skipped, so a green run without one
  proves only that the code compiles.
- **CLI output:** golden files in `cli/testdata/`. A change to what the CLI prints is a change to
  its golden file, reviewed like code.
- **The stack:** `make e2e-up` and `make e2e` run the shipped `docker-compose.yml` with a stub for
  GitHub and the model.
- **The dashboard in a browser:** `make e2e-ui` runs Playwright journeys at desktop and phone width
  against that stack. Around every step it diffs the database, the containers and the logs, and a
  change the step didn't declare fails it ([e2e/ui/README.md](e2e/ui/README.md)).
- **The tour:** `make tour-check` runs the docs' Take the tour on the demo built from the tree, and
  fails when a line the page pastes isn't one the run printed.
- **Kubernetes:** `make test-kube` against a kind cluster, and `e2e/cluster/` against a real one.
- **Coding agents:** `qa/agent/` runs headless Claude Code through real tasks and scores what it did
  with Canyon's answers ([qa/agent/README.md](qa/agent/README.md)).
- **What a pull request from a fork runs:** the fast checks, which are lint, the Go and database tests, the
  dashboard's tests, the images and the docs. The stack suites (the browser journeys, sign-in, outbound,
  patching, the tour and Kubernetes) run on GitHub's runners on every push to `main` and nightly, so your pull
  request never waits on them; a maintainer's own pull request runs them too.

## Pull requests

- **One pull request per change, one branch off `main`.** Batch related small fixes into it rather
  than opening one pull request each; its size is no objection. Don't stack pull requests.
- **Show the test failing first.** For a bug fix or a new check, paste the failing output from
  before your fix into the pull request. A check nobody has seen fail proves nothing.
- **Fail closed.** Anything on the review's path that is missing, unreadable or unknown blocks; it
  never passes.
- **Contracts are agreements.** A change to the shapes in `contracts/` (findings, events, routes,
  executor, snapshot) goes in the same pull request as the code that uses it, as its first commit,
  so a reviewer checks the code against it.
- **Commits** read `type(scope): what it does`, at most 72 characters, for example
  `fix(gate): keep an AI finding with an empty title`. Never force-push a branch under review: add
  a commit.
- **Sign off every commit** with `git commit -s`. It adds a `Signed-off-by:` line, which certifies
  the [Developer Certificate of Origin](https://developercertificate.org): you wrote the change or
  have the right to submit it under the project's licence.
- **Sign the [contributor licence agreement](CLA.md)** once, on your first pull request: the CLA
  check comments with a link, and you reply with the sentence it gives. You keep the copyright in
  your work. The agreement lets PlatCo GmbH, which publishes Canyon, license your contribution
  under Canyon's open-source licences and under its commercial ones, such as the enterprise
  edition's. It is the Apache Software Foundation's standard individual CLA, with PlatCo as the
  party. If your employer has rights in your work, it signs the corporate one, [CCLA.md](CCLA.md),
  first. The sign-off and the CLA are both needed.
- **New dependencies** are MIT, Apache-2.0, BSD, MPL-2.0, ISC or 0BSD, and the pull request names
  them.
- **Keep the docs true.** A change that makes a sentence in the docs false fixes it in the same pull
  request.

## Maintainers: the stack suites on private runners

This section is for maintainers. A pull request from a fork is never affected.

A maintainer's own pull request can run its stack suites on Canyon's private runners instead of GitHub's:
`scripts/ci-private.sh <pr>`.

- It starts the trusted workflow `private-ci.yml` in Canyon's private CI repository, with the pull request's
  number and exact head commit. That workflow checks the pull request again, fetches the commit as plain files
  and runs the suites. Nothing of the pull request's `.github` runs there, and the script pushes no git ref.
- It labels the pull request `ci:private`, so the stack suites here skip, the way a suite the change doesn't
  reach skips. From the next push on, which carries the label, every other job here skips too, since the private
  runners run them all; only the paths jobs, the secret scan and the CLA check run here. A run already going, or
  re-run, keeps the labels its push had.
- It prints the private run's address.

The run reaches only the suites the change can: `scripts/ci-select.sh` reads the pull request's changed paths and
its table, `scripts/ci-select.txt`, beside the `.github/suites` lists. The private workflow runs both as main holds
them, never the pull request's copy. Every change runs lint, tidy, web, test-rest and gitleaks. Every suite runs
when a changed path is on no line of the table, or touches `.github/`, the Makefile, `go.mod`, a Dockerfile,
`db/migrations/`, `e2e/` or the selector. A run of main runs every suite. What a change would run:
`git diff -z --name-only --no-renames origin/main...HEAD | scripts/ci-select.sh`.

`scripts/ci-private-status.sh <pr>` says whether that run passed for the head as it is now, how many suites ran
and which ones the selector skipped. It exits 0 only when it did, so merge on 0. Each new push needs `scripts/ci-private.sh <pr>` again; the label stays.

If the private runners are down, `scripts/ci-private.sh --off <pr>` takes the label off and runs the suites here
again.

Refused: a pull request from a fork, and one opened by anyone outside `CANYON_PRIVATE_AUTHORS`. The label is
honoured only on pull requests by those authors.

Both scripts need:
- `CANYON_PRIVATE_REPO`, the private CI repository as `<owner>/<name>`;
- `CANYON_PRIVATE_GH_CONFIG_DIR`, the gh login that may start its workflow, when it isn't the one in
  `GH_CONFIG_DIR`.

## Style

Solve the problem you can show, with the smallest change that does it. Use real parsers and
linters, not pattern guesses about code. No secrets in code, logs, findings or test fixtures: test
values are synthetic and don't look like real keys. **Canyon** is the only name users see.

## Licence

Everything you contribute is under the licence of the code it joins, as [LICENSE](LICENSE) says:
AGPL-3.0-only for most of Canyon, Apache-2.0 for `cli/`, `contracts/` and the other directories
with an Apache-2.0 LICENSE (the packages the CLI is built from), 0BSD for
`deploy/gitops/`, and the Elastic License 2.0 for `ee/` directories. No package outside
`ee/` imports one inside it; a feature reaches `ee/` through a hook the core declares (design/ARCHITECTURE.md §8). The
[CLA](CLA.md) also lets PlatCo GmbH license it under other terms.
