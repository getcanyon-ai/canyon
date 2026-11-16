# Contracts — the five frozen seams

These files are **the agreements between the two tracks**. Everything the engine emits and
everything the surfaces render meets here. Per AGENTS.md rule 5: once ratified, changing any
of these is its own PR, approved by both humans, merged before any consumer.

Status: **DRAFT — ratify in the Week-0 session, then wire each into CI** (the W0 issues).
A seam without a failing CI check is a document, not a contract.

| File | Seam | CI check (added at W0) |
|---|---|---|
| `finding.md` | what a finding is — a strict SARIF 2.1.0 profile | `contracts/validate.py`: golden SARIF log vs the SARIF 2.1.0 schema (vendored, sha256-pinned) and the profile schema + `blocks` derived from the snapshot, `level` from `severity`, `title` = `message.text`, unique fingerprints derived by the `canyon/v1` recipe, `secret_name` only on `secrets.*`, no live-key literal · Go: `contracts.CheckFindingSchemas` embeds both schemas for the gate — the two schema checks only; the invariants above stay in `gate/finding.Validate`, `blocks` with canyonv2-4g5 — and `make test PKG=./contracts` proves the golden log passes both and a broken field fails the schema that owns it |
| `events.md` | the run timeline — persisted rows, polled | `contracts/validate.py`: the recorded claims-portal run (generated from the seam-1 log by `testdata/timeline/generate.py`) — every row vs `timeline-row.schema.json`, ids and timestamps monotonic within each scope, kinds from their scope's table and all exercised, run states pending → running → terminal, embedded SARIF passes the seam-1 checks and belongs to its producer, verdict counts match its findings, the first verdict is the golden log, story blocked → clean → deploy |
| `routes.md` | HTTP API request/response shapes | `contracts/validate.py`: the fleet page fixture vs `fleet.schema.json`, then row 75's invariants — the risk order covering the stage enum exactly, one complete page, three of the four tiles counted back out of its rows (`secrets_exposed` reads a verdict and `refused` the egress requests, so they are the Go route's test), `apps[]` in stage → staleness → id order, `since` agreeing with `deployed_at` on the two stages an environment defines, each project named once, no environment under a project with no package, and seam 2's `tiles.updated` rows carrying this route's tile vocabulary and no other · Go tests in `cmd/canyon-server`, per route group as each lands (Open group: `/health`, `/ready`, `/auth/config`'s shape with the four flow routes' 404 while auth is off, the error shape); the app-page fixture vs `app-page.schema.json`, then the invariants of the three paged reads (packages, gate runs, the audit trail) and the promote receipt — every page newest first, a cursor boundary that repeats no row and steps strictly older, `next` null on the last page only, every run's package and every event's run named in the fixture, the receipt's four rules in order with the snapshot's names and states, `blocked_by` exactly the blocking rules that did not pass, the refusal's error code following from it, an approve body that carries both fields, and the approval answering that body for a finding of the golden log; the dependency approval (`testdata/app-page/birthday-list-dependency-approval.json`): each finding's `dependency` is its tool's own lines (finding.md), the body's findings one tool's and one dependency's, the answer one approval per finding in the body's order with its reason and expiry, by a person, and one audit row naming the dependency and listing the findings · Go tests in `cmd/canyon-server`, per route group as each lands (Open group: `/health`, `/ready`, `/auth/config`'s shape with the four flow routes' 404 while auth is off, the error shape); a promotion's package, label, image digest, gate run and deployer equal the preview row's; and *Naming a version* — every run row, environment and promotion labels its package the way the packages pages do, a fleet environment agrees with its row's `latest`, and no receipt `why` carries a `pkg_` id (validate.py rebuilds every reason from the data through `clean_gate`/`version_name`, so a raw id fails on the reason's text; the schema itself allows a label spelled `pkg_…`); the guardrails fixture (`testdata/guardrails/preferences.json`) vs `guardrails.schema.json`, then the five things the schema cannot say — one row per language, the setup turn's last write per tool equal to what `GET /preferences` answers, a proposal naming a rule the snapshot carries with the two state vocabularies equal and the turn writing nothing, the resolved design system one of the registry rows by namespace, and the skill manifest (`manifest` in the same fixture, the golden for `GET /api/v1/skill/manifest`) derived whole: its `stacks` the preferences read's, its `design_system` the resolved default without `id`, its `policies` `{id, name, state}` for every snapshot rule that is not `off`, in the snapshot's order, with a Canyon rule's `how` and a custom rule's `instructions` where it blocks in either gate and never where it only warns (the schema refusing `how` on a custom rule, `instructions` on a Canyon rule, and either on a warn-only one), its `component_types` the snapshot's `component_menu` — Score resource type names; the secrets fixture (`testdata/secrets/claims-portal.json`) vs `secrets.schema.json` — the whole list of one project, served by `cmd/canyon-server`'s route test and by the CLI's — then the four things the schema cannot say: the rows in the server's env-then-name order and that order deliberately not name order, `(env, name)` named once, `origin` `user` until a producer writes one, and every name one the executor's env accepts; the front-door fixture (`testdata/front-door/meridian.json`) vs `front-door.schema.json`, then the three things the schema cannot say — the answer is the body saved, its secret aside and its domain defaulted, a refusal is the probe's own answer with its fix verbatim naming the wildcards the form needs, a deploy's address follows the form, and the front door is `/run`'s `front_door` field for field — the two schemas' shared fields and choice rules equal, the stored door with its secret back one a Kubernetes `/run` carries; the release fixture (`testdata/release/meridian.json`) vs `release.schema.json`, then the seven things the schema cannot say (`seam3_release`): the report's digest is the release's, the headers are the body's, the sidecar's front door and the access route's answer are what a Kubernetes `/run` carries, every callback address is Canyon's, a resource gives what its type gives, and the pending environment is one the project read answers; the egress records fixture (`testdata/egress/claims-portal.json`) vs `egress.schema.json` — a row carrying a path, a resolved address or a header value refused (design/EGRESS.md §2.4) — then the four things the schema cannot say (`seam3_egress`): a key is one row in its minute or hour, an hour is the sum of its minutes, the flags are sorted, and an app env names at most 500 destinations a day before the rest are `other`; then the gaps (a dropped or unrecorded call counted, `receiver_down` counting none) and the reads: an app's (`GET /api/v1/projects/:id/egress`) is its minutes summed per env, destination and port, the most called first, with every gap of its window, and the host setting's (`GET`/`PUT /api/v1/egress`) answers a member `via` alone and an admin the `proxy` a `/run` names, the receiver's open `streams` and its `last_entry_at`; `other` is on port 443; a dropped gap no one counted (a message too large to read) and an envoy_dropped one (Envoy restarted, a stream failed to open, a read failed) are taken, an unattributed one that counts none refused; `POST /egress/dropped`'s body says its calls, a whole number from 1 to 2⁴⁰ or `null`, and names no app; System libraries and build tools (`seam3_platform`, `testdata/platform/claims-portal.json`): the image read (`GET …/envs/:env/image`) and the `platform.known-vuln` receipt row derived again from an image answer and its re-matches — each layer's owner from the lifecycle label, each component's class, the counted findings and their fingerprints, the notes (a purl-less or `unscanned` component is a note, never clean), each layer's newest match, and a match over 5 days old reading `could not check` — then refused: a row passing on a 6-day-old match, an `IMAGE` finding under another rule, and a platform finding without `IMAGE`; database sessions (`seam3_db_sessions`, `testdata/db-session/meridian.json`, `db-session.schema.json` and `db-query.schema.json`): a preview query, the connect that attaches to it on the opener's own sign-in (no row, no reason), a prod read with its reason, an owner's prod write opened in the dashboard under `prod_approval` and her CLI, another sign-in, attaching to it with its reason, the list, every close reason on the trail, the open's refusals, the relay's wire and one `/people` answer — then the things the schema cannot say: a database session lives at most 8 h in preview and 1 h in prod and each later read of it has no less activity, the query's rows fit its columns, its limit and its truncation, its timeout within 1 s to 5 min as Go reads a duration, each call is answered the database session it asked for (an attach the open one: a sign-in other than the opener's with its `db session attached` row, carrying its reason in prod, the opener's with none), the list is every open one, oldest first, as its open row recorded it, `yours` the reader's, the trail opens each once by a person at contributor or above under the org's `db_prod` — a prod write by an owner or an admin and, under `prod_approval`, from the dashboard — attaches only while open as its person, one row per database session and sign-in, never the opener's, closes it at most once by the actor its reason names (`held` a prod write's, `setting_changed` a prod one's), each action in its *Audit categories* row and on the fixture's app, one person role per person and database, and `/people` answers a login expiring within the asked TTL; refused: a read carrying a password, a prod database session without a reason, a reason over 500 characters, blank, or holding a format or bidi character, a `/people` answer not `verify-full` or whose host, database or user is not a name, a startup naming `user` or `replication`, and a refusal without its context |
| `executor.md` | `/build` + `/run` | `contracts/validate.py`: the four shapes vs `executor.schema.json`, `/run` re-uses the built digest (invariant 4), no `PORT` in env, container `<app>-<target>`, `keep` digests exactly, never a tag, at most 16; on Kubernetes, the image under the executor's registry, the access file and front door on `/run` and neither on Docker's, the sidecar's key set, and the Service and NetworkPolicy `/run` writes — the choice's sidecar port and never 3000 (invariant 10), out only to DNS, the apps database on 5433 and public 80/443 (invariant 11); invariant 1 (only `where: build` values reach a build) is tested in the control plane, invariants 2 and 3 in `executor/`; and `/run`'s egress and `POST /callers` (`seam4_egress`): the database the env names is in `no_proxy`, `/callers` answers addresses asked, each once, in order, never one docker handed out (the upper /25), and a fixed address names its target; and `/scan`'s image and sbom targets (`seam4_image`): the image scanned is the one `/build` answered, and each vulnerability a re-match names carries the `LayerDiffID` of the image's component it matched, one of the image's diff IDs |
| `snapshot.md` | the policy snapshot + the 21 rules | `contracts/validate.py`: schema + golden fixture + the twenty-one rules byte-exact vs EXPERIENCE D2 + namespace-per-producer + every-rule-but-the-nineteen-installs-off; Go: `gate/policy` registration tests |

Beside the seams, `provisioner.md` is what a platform team's provisioning adapter implements; its optional `/people` call is checked with the database sessions (the `routes.md` row).

Source of truth for the *content*: `design/EXPERIENCE.md` (D1, D2, D4, D5) and
`design/ARCHITECTURE.md`. These files only pin the *shapes*.

## finding.md's fixtures

The published contract (the docs site's *Findings* page) names no test or fixture; these are its.

**Mapped fixture:** `contracts/testdata/finding/mapped-gitleaks.sarif.json` — gitleaks 8.30.1's
`stripe-access-token` result on the claims-portal's `payments.ts:14`, as the snapshot.md table maps
it under `secrets.client-bundle`: critical, the table's fix, `secret_name` `STRIPE_ACCESS_TOKEN`,
its title the rule's `shortDescription`, the region hidden whole. `contracts/validate.py` checks it
as it checks the imported fixture, and refuses a mapped result whose policy, severity, fix or
`secret_name` is not the table's.

**Imported fixture:** `contracts/testdata/finding/imported-semgrep.sarif.json` — one semgrep
result as a run carries it, checked by `contracts/validate.py` against both schemas, the profile
invariants, the hidden region and the run's notification; its region is in the claims-portal
fixture app, whose file the validator re-reads to check the hidden snippet and recompute
`canyon/v1`, region digest included, under a test-only project key of 32 zero bytes
(`TEST_PROJECT_KEY`).

**The fixture:** `contracts/testdata/platform/claims-portal.json` holds an image answer, its re-matches,
the image read and the receipt row derived from them. `contracts/validate.py` derives the classes,
findings and notes again, and checks the fingerprints and the row.

**The patch fixture:** `contracts/testdata/patch/claims-portal.json` (`patch.schema.json`) is two rebase
patches of claims-portal's preview told end to end, one that passed its watch and one that was
reverted: executor.md's `/rebase` answer, each patch run's timeline, read and runs list, the
`gate_state` and `promote_run` they leave, preview's row and image read, the patch approval Canyon
files, the org's `run.transition`, the identities read and the trail (routes.md, *Patch runs*); and
the same patch's identity and `/rebase` answer on a Kubernetes host, whose `run_image` is
`<repository>@sha256:<hex>`, any tag stripped. `contracts/validate.py` derives the proof from the rebase's facts and the planned run image, holds
every patch line to routes.md's *Its lines* byte for byte, and refuses a patch run that deploys before
its proof row, a proof row of another run, a deploy to production, a rebase that changed more than the
run image or landed on another — on Kubernetes the planned digest under another repository too, its
`run_image` compared byte for byte — a watch before the deploy, a `gate_state` or `promote_run` that reads
a patch run it may not, an image read that could not check, and an approval, audit row or read naming
another run, image, fix or production image than the patch's.

**Golden fixture:** `contracts/testdata/finding/claims-portal.sarif.json` — the canonical
claims-portal findings from EXPERIENCE D4 as one SARIF log. `contracts/validate.py` checks it
against the SARIF 2.1.0 schema (vendored under `contracts/vendor/`, sha256-pinned), the profile schema
(`contracts/schema/finding.schema.json`), and the invariants above: `blocks` derived from the
snapshot, `level` projected from `severity`, `title` = `message.text`, unique fingerprints,
`secret_name` only on `secrets.*`, `required_end_state` and a non-empty single-line `do_not` on
every finding that blocks, and no live key anywhere in the serialized log — nothing gitleaks 8.30.1's
default ruleset finds (`contracts/livekey_test.go` runs it over every contract golden; `validate.py`
checks the `sk_live_`/`rk_live_` shape alone).
The same two schemas are embedded in Go as `contracts.CheckFindingSchemas`, which is what the
gate runs on producer output; the invariants a schema cannot express run in `gate/finding`.

## provisioner.md's tests

The published contract names no test. Its rules are pinned, in `provisioners/`, `runs/` and `cmd/canyon-server/`, by:

- `TestAKeyIsCheckedBeforeAnyRequest`
- `TestTheReferenceAdapterProvisionsThenDeletes`
- `TestReissueGivesEveryBuiltinDatabaseAFreshPassword`
- `TestTheFileChoosesADriverPerTypeAndEnvAndUnsetIsBuiltinPostgres`
- `TestAFailedEnsureFailsTheRunAndStartsNoContainer`
- `TestATypeIsAvailableOnlyOnAHostThatProvisionsIt`
- `TestDeleteAsksTheDriverThatMadeTheRecord`
- `TestTheFileIsRefusedWhole`
- `TestEachAnswerIsItsState`
- `TestARedirectIsFailedAndTheTokenStaysHome`
- `TestATimeoutOrAClosedConnectionIsTransient`
- `TestADeployDeclaringS3HandsTheContainerOnlyTheAdaptersStorageURL`
- `TestTheAdapterURLIsHTTPSOrLoopbackAndTheTokenFileHoldsAToken`
- `TestTheAdapterCertificateIsVerifiedAgainstItsCAFile`
