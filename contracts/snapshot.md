# Seam 5 — the policy snapshot

The one versioning primitive. Everything that shapes a verdict lives here, except the reports
attached to the package (finding.md, *In: external SARIF*); the AI review cache keys on
`(tree_hash, snapshot_hash, resolved_model, agent)` — an attached report is a run of its own, which
the review neither reads nor caches — so editing anything below invalidates exactly the cached
reviews it should. `hash` is `snap_` and the hex sha256, 64 digits, of the snapshot as `gate/policy`
marshals it with `version` 0 and `hash` empty; the goldens' 8-digit hashes are illustrative: the
server computes the hash of the snapshot it boots from, and no test recomputes a golden's `hash`.

```jsonc
{ "version": 7,
  "hash": "snap_c41a…",              // stable across no-op rebuilds
  "rules": [                          // THE 21 — id = policy id = --explain arg = audit type
    { "id": "secrets.client-bundle",      "name": "No secrets in the code",        "state": "blocking", "producer": "scanner" },
    { "id": "sql.string-concat",          "name": "No hand-built SQL from user input", "state": "blocking", "producer": "scanner" },
    { "id": "prod.clean-gate",            "name": "Production needs a passing review", "state": "blocking", "producer": "promote-gate" },
    { "id": "prod.human-approval",        "name": "A person promotes",                 "state": "blocking", "producer": "promote-gate" },
    { "id": "auth.unauthenticated-route", "name": "Every route needs sign-in",         "state": "advisory", "producer": "llm-review" },
    { "id": "deps.known-vuln",            "name": "No known-vulnerable dependencies",  "state": "advisory", "producer": "scanner" },
    { "id": "stack.blessed",              "name": "Approved stacks only",              "state": "advisory", "producer": "server-state" },
    { "id": "owner.required",             "name": "Every app has an owner",            "state": "advisory", "producer": "server-state" },
    { "id": "perf.unbounded-refetch",     "name": "No unbounded refetch on user input","state": "advisory", "producer": "llm-review" },
    { "id": "design.org-system",          "name": "Apps use the org's design system",  "state": "advisory", "producer": "scanner" },
    { "id": "code.injection",             "name": "No user input reaching commands, files, pages or URLs", "state": "advisory", "producer": "scanner" },
    { "id": "iac.misconfig",              "name": "Infrastructure config passes its benchmark checks",     "state": "advisory", "producer": "scanner" },
    { "id": "ai.sql-string-concat",       "name": "No hand-built SQL from user input (AI review)", "state": "advisory", "producer": "llm-review" },
    { "id": "ai.code-injection",          "name": "No user input reaching commands, files, pages or URLs (AI review)", "state": "advisory", "producer": "llm-review" },
    { "id": "sarif.checkov",              "name": "Findings from checkov",             "state": "advisory", "producer": "sarif", "run": true },
    { "id": "sarif.eslint",               "name": "Findings from eslint",              "state": "advisory", "producer": "sarif", "run": true },
    { "id": "sarif.gitleaks",             "name": "Findings from gitleaks",            "state": "advisory", "producer": "sarif" },
    { "id": "sarif.opengrep",             "name": "Findings from opengrep",            "state": "advisory", "producer": "sarif", "run": true },
    { "id": "sarif.trivy",                "name": "Findings from trivy",               "state": "advisory", "producer": "sarif", "run": true },
    { "id": "platform.known-vuln",        "name": "No known-vulnerable system libraries or build tools", "state": "off", "producer": "promote-gate" },
    { "id": "prod.egress-allowed",        "name": "Production allows everything preview connects to", "state": "off", "producer": "server-state" }
  ],
  "exclusions": [".env*", "!.env.example", "node_modules", ".git", "/dist", "/build", "/.next", ".venv"],   // the package filter — served, never .gitignore
  "agents": [ { "id": "canyon-review", "namespace": ["perf", "auth", "ai"], "model": "resolved-model-id" } ],
  "component_menu": ["postgres", "s3"],           // Score resource type names (score.dev), never invented ones: they are written into repositories
  "stacks": [],                       // what agents are told to build with, [{language, frameworks[]}] — one row per language, empty until an admin sets it (routes.md, GET/PUT /preferences)
  "approved_buildpacks": ["paketo-buildpacks/node-engine"],   // the buildpack ids stack.blessed approves (routes.md, GET/PUT /preferences); sorted by id; absent while an admin has set none, so the hash stands (amended 2026-10-04, #636)
  "limits": { "gate_runs_per_project_per_hour": 30, "gate_runs_per_actor_per_hour": 60 },
  "capabilities": { "design_systems": { "enabled": true, "org_default": null } }
}
```

Rules:

- **`exclusions`** is what never leaves the builder's tree, read the same by the CLI and the server
  (`ingest.Excluded`) over each slash path of the package: an entry is a `path.Match` glob over one
  path segment, at any depth — `node_modules` anywhere — except one that starts with `/`, which
  matches the first segment alone, the package root's own entry: `/dist`, `/build` and `/.next` are the build
  output beside the root's manifest, and `src/build/` or `internal/build/` is source. `/.next` is there
  because Next.js writes keys of its own into `.next/prerender-manifest.json`, and packed they blocked
  the app on the secrets rule (QA batch 2026-10-07). One that starts
  with `!` lets through a segment another entry matched: no name starting `.env` travels — `.envrc`,
  `.env.local`, `.envs/` among them — but the file `.env.example`, which documents the variables and
  holds none (gitleaks reads it like any file). `.git`, `.gitattributes` and `.gitmodules` are excluded at any depth whatever the list says.
- States: `off · advisory · blocking` — the three states ARE the trust tiers.
- **Two gates** (design/EXPERIENCE.md D14). A rule's state *before preview* is its `state`; its state
  *before production* is its `production`, Off when absent: a rule not placed in a gate is Off there.
  `production` is `advisory` or `blocking`, carried only on a rule placed before production, so a rule
  has one spelling and a snapshot with no rule placed there hashes as it did before the field. The six
  go-live rules — `prod.clean-gate`, `prod.human-approval`, `owner.required`, `stack.blessed`,
  `platform.known-vuln`, `prod.egress-allowed` — are never
  placed before preview: their `state` is their before-production state, and they never carry
  `production`. A rule is **on** when its `state` or its `production` is not Off; every "on" in the seams
  means this, and a review runs every rule that is on, so the findings before production judges always
  exist.
- **A new rule registers here and installs `off`** — every rule but the nineteen above it, whose
  defaults are theirs. The CI test proves a rule added with any other default fails.
- **`platform.known-vuln`** (design/PATCHING.md §3.3) is a go-live rule no review reads: it judges
  the image preview runs from the newest stored match of each of its layers, when the receipt is read
  (routes.md, *The promote receipt*), because `check` never builds. It is listed, and installs `off` as a
  new rule does; whether it ships advisory is not yet decided. It carries no
  `run`, and `settings` only as `{source}` (added 2026-11-03, #2077): the imported tool whose image reports it
  adds to Canyon's own matches, never replacing them (finding.md, *An imported image result*), absent for trivy's own alone. Its
  producer is `promote-gate`, which owns the `platform` namespace as it owns every `prod` rule but
  `prod.egress-allowed`.
- **`prod.egress-allowed`** (design/EGRESS.md §3.4) is a go-live rule no review reads either: it judges
  what preview connected to against what production allows, when the receipt is read (routes.md, *The promote
  receipt*). It is listed, installs `off` as a new rule does, and carries neither `run` nor `settings`. Its
  producer is `server-state`, as `stack.blessed`'s and `owner.required`'s is, because it reads what the
  server holds, so `prod` is the one namespace with two producers. A producer's run is still confined to
  its own rules (finding.md): a `canyon/server-state` run can't carry `prod.clean-gate`.
- **`deps.known-vuln` placed before production reads the built app too** (design/PATCHING.md §3.6;
  decided 2026-11-01, #2355): while it carries `production`, its receipt row also counts the known-vulnerable
  packages of the image preview runs that its review did not report (routes.md, *The promote receipt*;
  finding.md, *The built app*), and Canyon scans and matches that image as it does while
  `platform.known-vuln` is on. Its state before preview reads lockfiles alone, as `check` never builds.
- **The tools Canyon runs by default are five of the seventeen** (decided 2026-10-01: Canyon writes
  no detectors; the pinned OSS tools detect, on every review, with nothing for an admin to switch
  on). `sarif.trivy`, `sarif.opengrep`, `sarif.eslint` and `sarif.checkov` ship advisory with `run`
  on — ESLint for the JavaScript and TypeScript flaws the vendored Opengrep rules miss (#1376); `sarif.gitleaks`
  ships advisory and takes neither `run` nor `settings`: Canyon runs gitleaks in process, never
  through the executor, on every review while `sarif.gitleaks` is on (finding.md, *Canyon-run
  tools*). Two rules stated before this are reversed for these five alone: "a new rule installs
  `off`" — they ship advisory — and the next rule's "an admin adds it" — they are in the snapshot
  with no edit, and an edit turns them off as it turns any rule off. "No per-rule state for a tool's
  own rules" still holds: a tool's results take its rule's state, except where the table below maps
  a result to a policy. Adding them moved every org's snapshot hash once each time.
- **An imported tool is one rule**, `sarif.<tool>` (`<tool>` matches `^[a-z0-9]+(-[a-z0-9]+)*$`, 1 to 40 characters, finding.md and routes.md's policies row),
  producer `sarif`, name `Findings from <tool>`. An admin adds it by applying it with a state
  (routes.md, the policies row): registering and the edit are one step, so it installs `off` like
  every new rule and the edit sets its state. Every `sarif.<tool>.<ruleId>` finding takes its state
  from it; there is no per-rule state for a tool's own rules, except that a result the policy table
  below maps takes its policy's state instead. An upload never registers one.
- **A tool Canyon runs through its executor** — `sarif.trivy`, `sarif.opengrep`, `sarif.eslint`,
  `sarif.grype`, `sarif.checkov` (finding.md, *Canyon-run tools*) — may carry `run: true` and `settings`, its allow-list (routes.md,
  the policies row), composed from the org's policy edits like a state: `run` only when true and
  `settings` only when not empty, so a no-op never moves the hash. No other rule carries either.
  The hash covers both, so every run journals what it scanned with.
- `blocks` on a finding is derived from `state == blocking && severity ≥ high` — nowhere else: the
  run's gate is before preview, so a stored verdict's `blocks` is before preview's. Before production
  is the promote receipt's, derived the same way from the before-production state when it is read
  (routes.md, *The promote receipt*).
- **A finding on a Dockerfile never blocks**, whatever its rule's state in either gate: a result of
  one of checkov's dockerfile checks, its id `iac.misconfig.checkov.CKV_DOCKER_<n>` or
  `iac.misconfig.checkov.CKV2_DOCKER_<n>` (the ids checkov 3.3.19's dockerfile framework gives its
  checks; trivy checks no Dockerfile, its misconfig scan being kubernetes, helm and azure-arm alone,
  executor.md). Canyon builds every app with buildpacks and builds from no Dockerfile (executor.md),
  so what such a finding names never runs. It is in the run at its severity, an advisory, and its
  problem says why (finding.md); the rule's state stays the admin's and the app's for every other
  result, a Kubernetes manifest's or Terraform's among them. Its `blocks` is false before preview and
  before production alike, and the policies preview counts it in no `would_block` (**amended
  2026-10-08**, the QA batch's strict-iac: an app holding `iac.misconfig` blocking was stopped on a
  Dockerfile Canyon does not read).
- Rule ids are namespace-confined per producer. A Canyon producer's result outside its
  namespace fails the run closed; an agent's result outside its granted namespaces, or under a
  rule that is not on, is dropped and counted in one notification on its run (finding.md).
- `stacks`, `approved_buildpacks` and a capability's `org_default` are server state composed from the org's preferences
  row, so an edit to that row moves the hash and invalidates exactly the verdicts cached against
  the old one.
- A rule's `state` and `production` are server state composed the same way, from the org's policy
  edits (routes.md, `PUT /policies/:rule`), per gate: in each gate the newest edit naming that gate
  whose start has passed wins, the canonical default otherwise. `grace_days` is the edit's, never the
  snapshot's: it is not in the hash. The hash covers both gates, so an edit to either moves it and
  re-reviews every app.
- **A project's runs use its effective snapshot**: the org's, with the app's additions (routes.md,
  `PUT /projects/:id/policies/:rule`) applied — in each gate the stricter of the org's state and the
  addition's (`off` < `advisory` < `blocking`) — written as above, `production` only where the rule is
  then placed before production. It carries the org's `version`, and its `hash` is computed as any snapshot's, so
  a project's additions are in its verdict cache's key; a project with none runs the org's snapshot.
  Nothing else about additions is in a snapshot. The golden is `contracts/testdata/snapshot/claims-portal.json`.
- On `org_default`, absent and `null` mean the same thing: nothing configured. The golden carries `null`; the Go mirror omits the key when unset (`omitempty`), and the hash is over what `Build` marshals, so the two never differ in a hash.
- Every snapshot edit is an audit event with a diff.

## Custom rules

An org's own rules, written by an admin in plain words (routes.md, `/api/v1/custom-rules`). Each is
checked by code where the facts Canyon reads from an app decide it, and by Canyon's AI reviewer
otherwise (amended 2026-11-04, #2639). Users see them as **custom rules**.

- **A rule each**, `custom.<slug>`: the slug 1 to 40 characters, a lowercase letter then lowercase
  letters and `-`, fixed once the rule exists, so an edit keeps the id an approval of its finding
  covers; `name` 1 to 80 code points, no control character; `instructions`,
  the admin's text, 1 to 2,000 code points, no control character but a newline. Only a `custom.*`
  rule carries `instructions` and `check`, and every one carries both, Off or not, so an edit to
  either moves the hash. It installs `off`, like every new rule, and its state is composed from the
  org's policy edits like any rule's. At most 20 on a host.
- **How it is checked, `check`, is fixed per rule version**: `{by, expr?, says?, facts_version}`,
  decided once, when the version is saved (routes.md, the custom-rules row), so a review never
  translates a rule again.
  - `by` is `code` or `ai`. `code` carries `expr`, a CEL expression over the app's facts (gate/custom:
    languages, files, dependencies, services, roles and secrets), true when the app follows the rule,
    at most 4,096 code points; `says`, the expression in plain words, one sentence of 1 to 280 code
    points, no control character, carrying no secret gitleaks finds; and `facts_version`, the version
    of the facts' names, types and meanings the expression was written against, an integer from 1.
    `ai` carries `facts_version` alone.
  - A rule checked by `code` has producer `scanner`: the scanners' run evaluates it (finding.md,
    *Custom rules*). One checked by `ai` has producer `llm-review`, as every custom rule had before.
  - This Canyon reads facts version 3 (#2644: dependencies from trivy's package list, finding.md); a
    rule saved against another version could not be checked until an admin saves it again.
  - The secrets fact is every secret the app has set, in every environment, each `{name, env, where}`,
    never a value, read as they stand when the review runs: a secret removed after a review is seen at
    the next one.
- **One agent reviews those checked by AI**: while any custom rule checked by `ai` is on, the
  snapshot carries the agent `{id: "custom-rules", namespace: ["custom"], model}`, after
  `canyon-review`; while none is, it carries no such agent, even while rules checked by `code` are
  on. No other agent claims `custom`, and the agent is sent only the rules checked by `ai`, so no rule
  is judged twice. Its one call per review covers every such rule on (finding.md, *Custom rules*).
  The hash covers each rule's `instructions` and `check`, so every run journals the text and the
  check it was judged against, and an edit re-reviews.

## Tool results mapped to a policy

Canyon writes no detectors: the pinned OSS tools detect, and this table, fixed per release, puts
their results under the policies admins already set. Whether a tool runs, and whether its run is in
a verdict, is its `sarif.<tool>` rule's, as for every tool (finding.md, *In: external SARIF*). A
result of that run which a row selects is the row's policy's finding: its `ruleId` is
`<policy>.<tool>.<the tool's ruleId>`, it takes the policy's state and `policy_name`, and its
severity, where the row names one, and its fix, where the row's is not the tool's, are the row's
(finding.md, *From the tool into the profile*). The first row that selects it wins, in table order. A row selects by the tool's rule
metadata alone — and, where its cell says so, by whether the result's file is documentation as
Linguist classifies its path (go-enry's `IsDocumentation`) — never by a state, so a result is mapped once, at attach, and keeps its id in every
run — an approval follows it whatever the policies do — while its policy's state is read in each run
and each gate: in a gate where its policy is Off it is in the run and blocks nothing there, as no finding
of an Off rule does. A
result no row selects keeps `sarif.<tool>.<ruleId>` under `sarif.<tool>`'s state; a run never drops
an imported result. The tool is the attach's, never the report's `driver.name`,
and the prefix is Canyon's: a report's own ruleId is never read as this shape (a semgrep report's
`secrets.client-bundle.gitleaks.x` is `sarif.semgrep.secrets.client-bundle.gitleaks.x`). A report is
mapped whoever attached it, Canyon or an uploader, so a policy's state applies to its tool's results
in place of the tool rule's. A row reads only the tool's own rule metadata and that classification
of a result's path, never the code, and no row names a `sarif.*` policy.

| Policy | Tool | Selects | Severity | Fix |
|---|---|---|---|---|
| `secrets.client-bundle` | `gitleaks` | every result but a `generic-api-key` one in a documentation file of prose | `critical` | `Rotate this credential — it has been in your code. If nothing reads it, delete it from the code; otherwise store the new one with canyon secret set <SECRET_NAME> and read it from the environment.` |
| `sql.string-concat` | `opengrep` | a result whose rule's `properties.tags` has a tag that is, or starts with then `:`, `CWE-89` | the tool's | `Pass the user's input to the query as a bound parameter instead of building the SQL text from it.` |
| `deps.known-vuln` | `trivy` | a result whose rule's `properties.tags` has the tag `vulnerability` | the tool's | `Upgrade the package to a version the advisory lists as fixed, then update the lockfile.` |
| `code.injection` | `opengrep` | a result whose rule's `properties.tags` has a tag that is, or starts with then `:`, one of `CWE-78`, `CWE-77`, `CWE-94`, `CWE-95`, `CWE-22`, `CWE-23`, `CWE-79`, `CWE-601`, `CWE-918`, `CWE-611`, `CWE-502` | the tool's | `Keep the user's input data: pass it as an argument or a parameter, never as part of a command, path, page, URL or code, and check it against the values allowed.` |
| `code.injection` | `eslint` | every result | the tool's | `Keep the user's input data: pass it as an argument or a parameter, never as part of a command, path, page, URL or code, and check it against the values allowed.` |
| `iac.misconfig` | `checkov` | every result | the tool's | the tool's |
| `iac.misconfig` | `trivy` | a result whose rule's `properties.tags` has the tag `misconfiguration` | the tool's | `Set what the check names to the value it asks for, in the file at this line.` |

A fix that is the tool's is the one the tool offers, as for a result no row selects (finding.md, *From
the tool into the profile*): checkov's is its rule's `help.text`, the check and the resource it names
(**amended 2026-10-07**, the QA batch: the row's own read `Set what the check names to the value it
asks for, in the file at this line.`, which says less than the check's own words).

`CWE-23`, relative path traversal, joined `code.injection`'s row: three vendored JavaScript rules carry
it (`generic-path-traversal`, `express_lfr`, `express_lfr_warning`) where their siblings carry `CWE-22`,
and one of them stayed `sarif.opengrep.*` beside three `code.injection` findings on one line. This moved
their results' ids from `sarif.opengrep.*`, and their state to `code.injection`'s: an approval of one no
longer matches (**amended 2026-10-07**, #1845, the QA batch).

The secrets row's fix says both ways out, because a row reads no code and cannot tell whether
anything reads the credential: a key in a test fixture nothing runs was told to move to `canyon secret
set` and be read from the environment (**amended 2026-10-07**, #1850, QA batch 2's strict-hidden-secret).

A documentation file of prose is one Linguist reads as documentation by its path (go-enry's
`IsDocumentation`: a README, CHANGELOG, CONTRIBUTING or LICENSE, or anything under `docs/`, `demo/`,
`samples/` or `examples/`) and as prose by its content (Markdown, Text, reStructuredText): a
`License.tsx` or a `docs/` app's `config.ts` is code, and selected. `generic-api-key` is gitleaks'
catch-all pattern, and the placeholder token in every stock NestJS app's README badge matches it; there it
keeps `sarif.gitleaks.generic-api-key`, at the row's severity, under `sarif.gitleaks`'s state — a
warning by default, blocking when an admin sets `sarif.gitleaks` blocking — while every other gitleaks
rule, a provider's key format or another specific pattern, is `secrets.client-bundle`'s wherever it
is. This moved such a README result's id once: an approval of it no longer matches (**amended
2026-10-05**, #1683).

`<SECRET_NAME>` is the finding's `secret_name`: the tool's `ruleId` upper-cased in ASCII, each run of
characters outside `A-Z` and `0-9` one `_`, a leading or trailing `_` dropped, and `SECRET_` put in
front when what is left does not start with a letter (`stripe-access-token` →
`STRIPE_ACCESS_TOKEN`, `1password-secret-key` → `SECRET_1PASSWORD_SECRET_KEY`), so it is always a
name `canyon secret set` takes and never carries anything else. "The tool's" severity is what any
imported result gets (finding.md). `auth.unauthenticated-route` has no row: no rule in the pinned
rulesets reads route authentication, and a CWE tag such as CWE-306 names other things (an empty
database password), so mapping it would mislabel them. Which results a row selects is the tool's
tag metadata, which the normalised log does not carry: the gate's attach tests hold it against the
pinned tools' real output.
