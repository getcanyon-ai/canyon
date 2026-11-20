# Seam 1 — the Finding

**A Canyon finding is a SARIF 2.1.0 `result` in a strict profile**. One structure in and out: every producer — scanners, the LLM reviewer,
capability rules, and external tools via the SARIF adapter — emits it; the
dashboard, CLI (`human`, `ai`, `json`, `sarif`), and any CI's SARIF upload consume it unchanged.
**Redaction happens at construction — a credential literal never exists in a serialized
finding, in any format.**

The profile = plain SARIF the tools understand + a required `properties.canyon` bag the tools
ignore and Canyon requires. Nothing Canyon needs lives outside the bag; nothing in the bag is
optional except `secret_name`, `recorded`, `shown`, `carried_from` and `playbook`, and `required_end_state` and
`do_not` on a finding that does not block.

```jsonc
{ "version": "2.1.0", "$schema": "https://json.schemastore.org/sarif-2.1.0.json",
  "runs": [ { "columnKind": "unicodeCodePoints",  // columns count code points, and the run says so
    "tool": { "driver": { "name": "canyon/scanners", "version": "<gate version>",
                          "rules": [ { "id": "secrets.client-bundle", "name": "No secrets in the code" } ] } },
    "results": [ {
      "ruleId": "secrets.client-bundle",          // = policy id = --explain arg = audit event type
      "level": "error",                           // DERIVED from severity for tools' benefit; never read back
      "message": { "text": "Stripe live secret key is bundled into the browser build" },  // = canyon.title
      "locations": [ { "physicalLocation": {
          "artifactLocation": { "uri": "apps/web/src/lib/payments.ts" },
          "region": { "startLine": 14, "startColumn": 27, "endLine": 14, "endColumn": 63,   // THE span — redaction source of truth
                      "snippet": { "text": "const stripe = new Stripe(\"sk_live_51MZq8f••••7Kx2\");" } }  // pre-redacted; ≤3 lines as serialized
      } } ],                                      // no contextRegion: the profile has none
      "relatedLocations": [],                     // further spans of the same finding, same region shape
      "partialFingerprints": { "canyon/v1": "f_<64 hex>" },   // derived by the gate — the recipe below
      "properties": { "canyon": {
        "severity": "critical",                   // critical | high | medium | low  (exactly four)
        "blocks": true,                           // DERIVED SERVER-SIDE: policy is Blocking AND severity ≥ high (never on a Dockerfile).
                                                  // Agents and CI key ONLY off this field, never off severity or level.
        "title": "Stripe live secret key is bundled into the browser build",   // ≤120 chars, code is the subject
        "problem": "payments.ts is imported by CheckoutPanel.tsx, a client component — everything a client component imports ends up in the JavaScript your users download. This key can charge cards and issue refunds.",
        "fix": "Move the Stripe call behind a server route and read the key from process.env.STRIPE_SECRET_KEY. Rotate the key in Stripe — it has been in your code — then store the new one: canyon secret set STRIPE_SECRET_KEY.",
        "policy_name": "No secrets in the code",     // English name, for "why" lines
        "secret_name": "STRIPE_SECRET_KEY",       // OPTIONAL — only on credential findings; the variable the fix stores the rotated key under
        "shown": { "startColumn": 27, "endColumn": 52 },   // OPTIONAL — the caret on the masked snippet; the region stays 27–63
        "carried_from": "gr_7d3e8f2a91c04b56",    // OPTIONAL — only on an agent's result carried from an earlier review: the run whose review reported it
        // "playbook": { "slug": "service-conventions", "name": "How Meridian writes a service" },   // OPTIONAL — only on a custom rule's result whose rule points at a playbook: DERIVED SERVER-SIDE
        "required_end_state": "No credential literal appears anywhere under apps/web/src/. The Stripe client is constructed only in server-side code, and the key is read from process.env.STRIPE_SECRET_KEY at runtime.",   // REQUIRED when blocks is true
        "do_not": [ "move the value to a NEXT_PUBLIC_, VITE_, REACT_APP_, or PUBLIC_ variable", "move the literal into .env, a comment, a config file, or a different module", "rename the variable", "add an ignore comment, suppression pragma, or --force" ]   // REQUIRED when blocks is true; one line each
      } }
    } ] } ] }
```

## Profile rules — what makes a SARIF result a Canyon finding

- **A live key** is a secret gitleaks 8.30.1's default ruleset finds, in process, a `gitleaks:allow`
  comment ignored: Canyon's one secrets detector, whatever the vendor — a Stripe test key is a live
  key too, and a string its own allowlist and stopwords pass is not one, as the tool says. Wherever this profile refuses, masks or checks for a live key, it is
  that detector that decides. The reviewer's masking adds one rule of its own (below), which masks
  only and never finds.
- **Required on the log:** `version` = `2.1.0` and `$schema` = `https://json.schemastore.org/sarif-2.1.0.json`,
  byte-exact.
- **Required on every result:** `ruleId` · `level` · `message.text` · exactly one `locations[0]`
  with `artifactLocation.uri` and a full `region` (`startLine, startColumn, endLine, endColumn`,
  `snippet`) ·
  `partialFingerprints["canyon/v1"]` · `properties.canyon` with every field above except
  `secret_name`, `recorded`, `shown`, `carried_from` and `playbook`, and except `required_end_state` and `do_not` on a finding that does not block.
  A result missing any of these ⇒ **the run fails closed** (never dropped,
  never defaulted). A finding without a location cannot be rendered or redacted.
  `properties` carries the `canyon` bag and nothing else; line and column numbers are at most
  1 000 000 000.
- **`canyon.severity` is canonical; `level` is a projection of it** (critical, high → `error`;
  medium → `warning`; low → `note`) so tools that only read `level` behave sensibly. Canyon
  never reads `level` for its own findings.
- `canyon.severity` ∉ the four values ⇒ the run fails closed.
- **`canyon.blocks` is present on every finding; producers cannot set it** — the server derives
  it after policy lookup and overwrites whatever a producer wrote.
- **`canyon.playbook` is the server's, as `blocks` is** (**added 2026-11-20**, design/specs/mcp.md §3.4):
  `{slug, name}`, the org's playbook a `custom.*` rule points at, as the run's snapshot carries it on
  the rule (snapshot.md, *Custom rules*) — and only there: a finding of any other rule, or of a custom
  rule whose snapshot carries none, has no `playbook`. A producer never sets it; the server derives it
  after policy lookup and overwrites whatever a producer wrote, so a log whose `playbook` is not its
  rule's in the snapshot fails closed. It is how an agent is told where the org explains the rule, one
  more next step after the fix: `Read the playbook <name>: canyon playbook <slug>` in the CLI's
  `--format ai` block (design/specs/cli.md §5.2) and on the dashboard's finding. It is outside the
  `canyon/v1` recipe, so a playbook linked or removed never moves a fingerprint or an approval. The
  sample is `contracts/testdata/finding/custom-playbook.sarif.json`, against
  `contracts/testdata/snapshot/custom-rules.json`.
- `required_end_state` and `do_not` are required on any finding whose `blocks` is true, and
  permitted on any other. `do_not` names the specific wrong moves that reproduce this finding;
  each entry is one line. Both are governance prose the surfaces print verbatim and never
  paraphrase. The schema declares the two fields and their shape; the requirement is checked
  after policy lookup, where `blocks` is final, not at construction, where every finding says
  `blocks: true` until the server has judged it. The pipeline derives and checks every log
  against the run's snapshot; the gate library checks its own output against the
  schemas and the profile, but not yet against the snapshot.
- `ruleId` is byte-exact against the policy snapshot and **namespace-confined per producer**.
  A scanner, server-state or promote-gate result outside its producer's namespace
  fails the run closed — it is a bug in Canyon, not noise. An AI reviewer candidate whose rule is
  outside the namespaces the snapshot grants it, or is not on, is dropped, as is one the profile
  refuses (the reviewer's run, below), and the reviewer's run carries one notification counting
  the drops; the rest of the review stands.
  `tool.driver.name` says which producer a run is: `canyon/scanners` → scanner,
  `canyon/server-state` → server-state, `canyon/promote-gate` → promote-gate, `sarif/<tool>` → the
  imported tool, whose results are `sarif.<tool>.<ruleId>`, read against the snapshot rule
  `sarif.<tool>`, or `<policy>.<tool>.<ruleId>` where the policy table selects them,
  read against that policy and confined by the table's rows, not by the policy's producer — the one
  exception to byte-exact; an agent's run
  carries its snapshot id (`canyon-review`, or `custom-rules` for custom rules) and may
  emit only the namespaces the snapshot grants it.
- **The reviewer's run** is appended after the scanners' and never alters them;
  the reviewer is shown source files only — every file of an application language, the ones *What
  nothing read is named* counts (below), by the same reading of its language, so a `config.ru`, a
  `Rakefile` or a `bin/rails` is Ruby it is shown — and the templates that render pages (`.html .htm
  .erb .jinja .jinja2 .j2 .vue .svelte`); never a dotenv file — one named `.env` or `.env.<anything>`
  (`.env.local`, `.env.production`), while a `.envoy.ts` or a `.environment.js` is code like any other
  (every name that began `.env` was hidden) — nor anything under `.git/`,
  `node_modules/` or `vendor/` (by its extensions alone it was shown 24 of a stock Rails
  app's 41 Ruby files, and no C or C++) — valid UTF-8, at most 16 MiB of them in all — over that the model is not called, never with a
  truncated request: the reviewer's run carries the warning `ai review · not checked: the source is
  larger than the AI review accepts (16 MiB)`, and the run goes on (a run stored before the split carries
  `ai review · not checked: the source is larger than the AI review accepts (2 MiB)` there, read the same). **Source more than one call takes is
  split across calls** (an app over the 2 MiB that was the limit then was
  never AI-reviewed): a split plus a map of the app's files. One call takes at most 894,960 bytes of
  prompt: claude-opus-5's window of 1,000,000 tokens, less the answer's 32,000 and 64,000 kept for the
  brief, the rules and the answer's schema, at 0.99 bytes per token — the fewest bytes per token
  Anthropic's `count_tokens` measured for claude-opus-5 on these calls as sent, line numbers included,
  over five real codebases (Django 2.48 to 2.78, Hugo 1.97 to 2.13, Excalidraw 1.11 to 2.40,
  three.js's examples 1.75 to 2.05 and its bundled libraries 1.10 to 1.51), 1.10 on base64-embedded
  WebAssembly, less 10%. The 2 MiB that was the limit then was one call at about 2 bytes per token: about
  a whole window, and more on denser code. While every shown file fits one call, the review
  is one call, sent as before. Otherwise the shown files, in the tree's path order so a folder's files
  stay together, are put in parts, each filled until the next file would pass the call's room, counted
  as sent: each line with its number, the path of every shown file, each quoted as a Go string literal
  (a path may hold any character a name may), which every call is sent first, and each file's place in
  an AI twin's list of files to check. Each part is one call, sent those paths and then its part's files;
  an AI twin none of whose files is in the part is left out of its call, never sent with an empty list;
  and its results may name only a file of its part: one naming another is dropped and counted as any
  invalid result is. A review is at most nineteen calls (16 MiB over one call's room, rounded up); one that
  would take more — short lines, or so many paths that each call is mostly the list — is over the
  ceiling, the same `TooLarge` warning, and no call is made. The parts are reviewed at most three at a
  time, and their results are one run, deduplicated as one call's are, cached as one run; the run's notes
  say `ai review · reviewed in N parts: the source is more than one model call takes`, `N` the calls. A file that
  alone is more than a call takes is in no part: the run carries the warning `ai review · not checked:
  too large for one AI review call: <path>`, the path quoted as in the list, one per such file, and is an
  agent that read nothing (below) whatever the other parts found — its check ends `skipped`, its other
  parts' results kept in its run. That outcome is the tree's own, so unlike the others below such a run
  is cached, and replayed it reads the same. **What it would be shown and is not is named, never silently
  clean** (a file that was not UTF-8 was skipped unsaid). A file it would
  be shown that is not valid UTF-8 — a Latin-1 Ruby file, a stray `0xFF` byte in a comment — is sent to no
  call, since Canyon never guesses an encoding: the run carries the warning `ai review · not checked: not
  valid UTF-8: <path>`, the path quoted as in the list, one per such file, the first 20 in path order,
  then `ai review · not checked: N more files not valid UTF-8`; like a file too large for one call it is
  an agent that read nothing (below), and its run is cached. Where no rule is sent — every rule on an AI
  twin none of whose files it can read (*The AI review where no scanner reads the language*) — no call is
  made and the run still names a twin's files that are not valid UTF-8, an agent that read nothing, and
  the dependency folders as above; a
  review cached before this change is forgotten, as it may have read clean over such a
  file. A file of an extension an application language has, which go-enry names no language for because
  it reads it as binary (one NUL byte in a comment), is shown when its first 16 KiB are valid UTF-8 — a
  byte never hides code — and named as above when the whole is not; one whose first 16 KiB are not (a
  video named `.ts`) is no source. Having no language, no scanner rule is known to read it, so each AI
  twin on takes it among its files. And where a file of an application language,
  or a template, sits under `.git/`, `node_modules/` or `vendor/`, the run carries one warning `ai review ·
  not checked: code in dependency folders: <path>`, `<path>` each such folder — the file's path up to
  that directory (`vendor`, `web/node_modules`) — once, quoted the same, in path order and joined by `, `,
  the first 20 then `, and N more`: never a file's name, never a byte of it. What a package manager put there is the app's
  dependencies, which `deps.known-vuln` reads by their manifests and lockfiles; no rule tells an app's own
  code there from theirs without guessing, so none of it is shown and the folders are named. That is no
  agent that read nothing: its check keeps its outcome, while its verdict, carrying a `not checked` line,
  is never called clean (*What nothing read is named*). A part whose call fails fails the review, whatever another
  part answered; else a part whose answer runs out of room is the whole review's, out of room (below).
  **Each round of three calls has 10 minutes** (a claude-opus-5 call of 951,845
  input tokens answered in 20 seconds, 16 of them before its first token; the rest is room for an answer
  of up to 32,000 tokens, as one call had before), so a review of
  `n` calls has `ceil(n/3)` × 10 minutes; a review whose calls have not all answered by then carries the
  warning `ai review · not checked: the model did not answer in the time the review allows`, is an agent
  that read nothing (below), and the run goes on, never failed. The package's checkout for review keeps
  its own 10 minutes. A model whose answer stops at its
  output limit (`max_tokens` on the Anthropic API, `length` on the OpenAI one) has not answered:
  the reviewer's run carries the warning `ai review · not checked: the model ran out of room for its
  answer`, and the run goes on, never failed (a stock Next 16 app's
  review stopped there and failed its deploy). It read nothing, as on a host with no model: its
  check ends `skipped`, never `clean`, so a rule it reviews placed blocking before
  production holds the promotion, and it is never cached, so the
  next run calls the model again. The answer's room is 32,000 tokens, thinking included. **An agent
  that read nothing** — no model, out of room, over 16 MiB or nineteen calls, a file too large for one call,
  a file not valid UTF-8, or out of time, its run carrying that note — is read the same in all: its check ends `skipped`
  whatever it found, its run is never cached but for a file too large for one call or not valid UTF-8 (above), and while a rule of its
  namespace is blocking before preview the run carries one blocking `gate.unreadable` finding for it
  (below), so a review that did not happen never passes a blocking rule. That finding is a tool's (*Canyon-run tools*, *A scan that did not finish*), the agent
  named `the AI review` (`custom rules` for the custom-rules agent): its snippet `(the AI review
  could not check this tree; tree sha256:<64 hex>)`, its `title` `Canyon could not check this package
  with the AI review`, its `problem` `could not check: the AI review: <reason>`, the reason `no model
  is configured on this Canyon host`, `the model ran out of room for its answer`, `the source is
  larger than it accepts (16 MiB)`, `a file is too large for one AI review call`, `a file is not valid
  UTF-8` or `the model did not answer in the time the review allows`, or `it did not run` for an agent with no run at all. As a
  tool's, an approval covers that agent on that tree, whichever the reason. Advisory, the run goes on
  with the note alone. A model is
  sent exactly the resolved model id, the rules the reviewer may emit, and those files by path — a
  split review's call its part's, after the path of every shown file — each with every secret gitleaks v8 finds in a shown file replaced by `••••` wherever it appears
  in any of them, longest first — its default rules plus one, a URL userinfo password, which
  masks and is never a finding (a `gitleaks:allow` comment is ignored). A literal that spans lines
  keeps its line breaks, so line numbers hold. Unlike the mask in a finding's snippet, the model's mask keeps no
  character of the literal. If a live key survives, the model
  is not called and the run fails. The answer's schema names the ids of the rules the reviewer may
  emit as the only values a result's `rule` may take, so a model cannot name a rule outside them; a
  candidate under any other rule is still dropped and counted, the backstop. With no rule to emit
  nothing is excerpted for a model and none is called — a live key in the source is gitleaks' finding, not
  this run's failure — and the run has no results and no notification (a first review's one candidate named a rule outside its list and was dropped, so
  the review said nothing). A candidate that names a file the model was not shown, lines
  outside that file or blank lines alone, or whose title,
  problem or fix carries a secret gitleaks finds, is dropped and counted the same way: it costs
  that candidate, never the run. A candidate whose title, problem or fix is
  empty or whitespace alone is **not dropped: Canyon writes that field** from the rule's name, which
  every snapshot rule carries (at least one character), so no candidate is
  dropped for it — the title reads the rule's name; the problem reads exactly `Canyon's AI review
  found that this code breaks "<name>" (<ruleId>) and gave no more detail.`; the fix reads exactly
  `Change this code so it follows "<name>": Canyon's review then no longer reports <ruleId> here.`,
  `<name>` the rule's name and `<ruleId>` its id
  (a review dropped the one unauthenticated route it found for an empty
  field and said `findings: 0`). The model named a place under a rule it may emit; an empty field is
  missing prose, not a wrong finding. Dropped and counted the same way are all but one of the candidates whose results
  would carry the same `ruleId`, uri and `region`: the model said one finding twice, and two results
  of it would ask for the fix, and an approval, twice (`GET
  /api/results serves voter names to anonymous callers` twice, at `server.js:76`, in different words).
  The one of highest severity is kept, the first in the model's answer among equals, so the model's
  order never lowers a finding below what blocks. A candidate's title over 80 code points is cut to 80 as an imported title is to 120 (*From the tool into the profile*, below): at a space, never mid-word. A candidate carries `required_end_state` and
  `do_not` too, the answer's schema asking for both on every candidate: each `do_not` entry is
  fitted to one line (a run of whitespace, line breaks included, reads as one space) and an empty
  entry is left out, and either field left empty by that is absent. A candidate whose rule the
  snapshot sets `blocking` in either gate at a severity that blocks (critical or high) — one whose
  `blocks` will be true after policy lookup, or that will block before production — and which has no `required_end_state` or no `do_not` is **not
  dropped: Canyon supplies its own**, so the finding stands at the
  model's line under the admin's own rule, and the ship stops. `required_end_state` then reads
  exactly `Canyon's review no longer reports <ruleId> for this code when it runs again, and the app
  still does what it did.`, `<ruleId>` the rule's own, and `do_not` is exactly `["Silence it with an
  ignore comment or a suppression pragma — Canyon does not read those.", "Remove or rename the
  flagged code just so the review stops matching it."]` — the same shape, and minted the same way,
  as an imported result's, which Canyon writes in full for every tool (*From the tool into the
  profile*, below). Dropping it instead is the one drop that could not cost only its candidate: an
  admin set that rule to `blocking`, and dropping the finding inverts that decision with the model as
  the party deciding, by omitting prose. Missing, unparseable or unknown state blocks; it never
  passes. Generic prose is weaker help than the model's own would have been, and Canyon saying so in
  its own words is the price of not dropping the finding.
  Two limits, stated so neither reads as more than it is. A candidate's severity is its own, and only
  `critical` and `high` block, so a model that answers `medium` for a critical breach of a blocking
  rule is an ordinary advisory and reaches none of this — the same trust boundary as any producer
  relabelling its own finding, not closed here. And a candidate whose rule id the model spells
  wrongly, or that sits outside the agent's namespaces, is dropped before any rule state is known,
  and stays a drop that costs only itself. The text checked for
  a secret is the title, problem, fix, `required_end_state` and every `do_not` entry. A candidate's columns are counted
  on the file as written and its snippet is the lines the model was shown, bullets and all, with
  `shown` where the bullets move the caret.
  With no model on the host it has no results and one `invocations[0].toolExecutionNotifications[0]`
  (`warning`) reading exactly `ai review · skipped (no model configured on this Canyon host)`. A run answered from a recording carries one
  (`note`) reading `ai review · replayed from a recording`, results or none, and each of its rows
  `canyon.recorded: true`, present only then. A run that dropped candidates
  carries one (`note`) reading exactly `ai review · dropped N invalid result(s)`, N the
  count and nothing of the candidates: what the model got wrong, never something the review did not
  read, so no surface tells a builder to pass it on (it was a
  `warning`, and an agent told to mention every warning did). A run the model answered — results or
  none, replayed or not — carries, last of the reviewer's own, one (`note`) reading exactly `ai review ·
  a model's second opinion: another review, even of a small change, may find different things; a blocking finding
  stays while its code is unchanged`; a run
  the model did not answer (skipped, out of room, too large, out of time, or every file too large for one call) carries none (a one-line fix and the next review found three findings the first had not, on routes the
  fix did not touch, under the same rules — the model's answers vary between calls and no setting
  pins them, so the run says so). A run the review cache answered — the same package
  tree, policy snapshot hash, resolved model id and agent reviewed
  before — is that review replayed
  with no model call and carries one more (`note`) reading
  `ai review · reused from an earlier review of the same code and policy`; a skipped or failed
  review is never reused. A model that fails, or whose answer is not the answer schema's JSON,
  fails the run (`failure_reason: model`); a severity outside the four is read as `critical`.
  **A region reviewed before keeps its severity** (**amended 2026-11-14**: two reviews of an
  unchanged `server.js`, the app changed elsewhere between them, rated `server.js:9` high then critical
  and `:15` medium then high). Each result of an agent's run, answered by the model or the review cache,
  is raised to the highest `severity` any of the project's last 20 completed reviews — its 20 newest
  `verdict` rows, of any run but this one, a review that read nothing, another branch's and a
  rollback's among them — gave a result of the same agent with the same `canyon/v1`, and never
  lowered: each of those was itself raised the same way, so unchanged code keeps the highest severity a
  review has given it while one of the last 20 holds it, and an edit inside its region, which moves
  `canyon/v1`, is rated afresh. A result the raise brings to a severity that blocks under its rule
  (either gate, as above) and which has no `required_end_state` or no `do_not` carries Canyon's own, as
  a candidate does. The earlier verdicts are read before `blocks` is derived; a read that fails, or a
  verdict whose findings are not a log in this profile (its `version` and `$schema` not those *Required on the log*
  names: `null` and `{}` are not), fails the run (`failure_reason: error`), and an earlier
  severity outside the four is read as `critical`. A project with no earlier verdict has nothing to raise.
  **A blocking finding an earlier review made stays while its code stands** (**added 2026-11-15**, the owner's
  decision: a review of code the review before had blocked, changed only elsewhere, did not report the finding and
  passed). After the raise, each result an agent's run of one of those 20 verdicts carries is carried into this
  review's run of the same agent when all of these hold: this review carries no result of that agent with the same
  `canyon/v1` (the model left it out, or the earlier review the cache replays did); its rule is in this run's
  snapshot, on, in a namespace the snapshot grants the agent, and its severity blocks under the rule (either gate,
  as above); and its code stands. **Its code stands** when the package holds a file at its `uri` and, at some line,
  the snippet the reviewer would build for a span of as many lines at the same columns — the file masked as the
  model is shown it, blank lines trimmed, capped and windowed as any snippet — is byte for byte its own snippet, so
  code that only moved in the file still stands and its lines are where it now is. A package without that file, or
  with no such line, changed its code: it is not carried. **Fail closed**: a file that cannot be read, is not valid
  UTF-8, or whose snippet cannot be built tells nothing either way, and the result is carried at its own lines. A
  carried result is the earlier one — its rule, words, governance prose, snippet, columns and `canyon/v1` — with its
  lines where its code now is, the highest severity those verdicts gave it, and `canyon.carried_from`, the id of
  the run whose review reported it (kept as it was when it is carried again). An approval of it is an approval of
  that `canyon/v1`, so it covers the carried result. So it stops when its code changes, when its rule stops
  blocking (turned off or advisory, or no longer granted to the agent) — a custom rule's edited instructions are not
  read, since no verdict records the text a finding was judged under — when it is approved, and when none of the 20
  newest verdicts carries it. A run that carries any says so in one more `note`: `ai review · 1 blocking finding
  from an earlier review: the code it's on hasn't changed`, or `ai review · N blocking findings from an earlier
  review: the code they're on hasn't changed`, `N` the count (`custom rules ·` on the custom-rules agent's run).
- **The AI review where no scanner reads the language** (Ruby had no
  SQL-injection or code-injection check). `ai.sql-string-concat` and `ai.code-injection` are
  `llm-review` rules, advisory by default, an admin's to make blocking, each the AI twin of a scanner
  rule (`sql.string-concat`, `code.injection`). The reviewer is sent one only for the files the
  scanners' coverage counts for its twin — the application-language files no running scanner rule of
  the twin reads (*What nothing read is named*) and the reviewer is shown, never a dotenv file nor one
  that is not UTF-8 (a Latin-1 file was scoped and counted as read, never shown) —
  and the request scopes it to exactly those files, in
  Canyon's words beside the rule's name, never as an admin's instructions; with none, the rule is not
  sent. A candidate for it in any other file is
  dropped, counted as a candidate outside the granted namespaces is: the model never widens what it
  was asked to check. The agent `canyon-review` is granted the `ai` namespace beside `perf` and
  `auth`.
- **`auth.unauthenticated-route` on a host that signs people in** (`CANYON_AUTH` `oidc`; the finding told agents to build a login of their own for apps Canyon
  already puts behind its sign-in). Every app there is behind company sign-in and private until shared,
  and Canyon hands it the person's identity token. So a result of
  that rule, once the checks above keep it, keeps the model's rule, lines and severity, and its `title`,
  `problem` and `fix` are Canyon's: `title` reads exactly `Any signed-in person can reach this route` (**amended 2026-11-14**: a model's title, "… without sign-in", sat over a problem that says Canyon signs people in);
  the severity stays the model's, raised as any reviewer result's is (*A region reviewed before keeps its
  severity*, above), since whether a placed rule blocks reads it and Canyon never weakens a
  placement; `problem` reads `<title>. Canyon signs people in before they reach
  this app, so only people with access to it reach this route; the route itself does not check who is
  calling.`, `<title>` the model's own title for the result, so what it named stays, and `fix` reads exactly `Don't build a login of your own:
  Canyon already signs people in. Where this route needs to know or check who is calling, read the
  identity token Canyon sends with every request in the Authorization: Bearer header, verify it with
  CANYON_IDENTITY_ISSUER, CANYON_IDENTITY_AUDIENCE and CANYON_IDENTITY_JWKS_URL, and use its sub,
  email and name. A destructive or admin-only route checks a role; an ordinary write records who did
  it, from the identity.` (`a route that changes or deletes data
  checks a role` had an agent lock a whole team out of posting until someone granted a role). Its `required_end_state`
  and `do_not` are Canyon's own above where it blocks, and absent where it does not: the model's would
  ask for the login again. When the rule fires, and what it blocks, are unchanged. On a host with auth
  off the model's words stand. Cached reviews from before each of these wordings were forgotten, so none
  replays the old words.
- **What Canyon could not check is one `gate.unreadable` finding, never an aborted run and never a
  clean line**: a Canyon-run tool whose scan did not finish (*Canyon-run tools*,
  below; but for a cluster with no room to run it, which fails the run), or an agent that read nothing while a rule of it is blocking before preview (above). `gate.unreadable` ("Canyon checks every file") is the gate's own rule, not a policy: no
  snapshot carries it and no policy edit turns it off. It is read as the scanner's and blocking, sits
  in the `canyon/scanners` run, and is declared there when a result carries it. Each finding is
  severity `high`, so it blocks, and only an approval with a reason gets past it. Its snippet never
  quotes source, since unread source may hold a credential no redaction has seen: it carries a
  digest over the tool's name, then the package's files, each file's path framed as its byte length
  and a colon, then the sha256 of its bytes in hex, in path order. The fingerprint hashes the snippet, so an approval
  covers that version of the package only, and any edit to a file blocks again. `problem` carries
  the reason, which is grammar words and never source text.
- **Secrets and dependencies are the tools'** (Canyon writes no detectors).
  No module of Canyon's reads them: gitleaks fills `secrets.client-bundle` (all but the policy table's
placeholder in a documentation file, which stays `sarif.gitleaks`'s) and trivy
  `deps.known-vuln`. With the tool off,
  the rule is named `not checked on this host`, never read by anything else. Nor is
  `sql.string-concat`: Opengrep fills it, one of its rules Canyon's own (*Canyon-run tools*, below).
  A key the code assembles at run time (`"sk_live_" + rest`) matches no gitleaks rule: the AI review
  reads it, and on a host with no model the run says so (`not checked: auth.unauthenticated-route: no
  model …`, *What nothing read is named*), so it never reads clean.
- **Custom rules**. The `custom-rules` agent's run reviews, in one call, every custom
  rule that is on and checked by `ai` (the policy snapshot's custom rules), the model shown each rule's `instructions` under its id and name and the same files
  `canyon-review` is shown, redacted the same way. Its results are `custom.<slug>`, and each is
  `high` whatever severity the model answered, so a Blocking custom rule blocks every finding it
  produces; in all else a custom rule's candidate is a reviewer candidate (above): the same checks,
  drops, governance prose and redaction. Its run's notifications are the reviewer's, each reading
  `custom rules ·` where the reviewer's reads `ai review ·`, and surfaces name its row "custom rules".
  It is shown only the files the reviewer is, so a package of other languages gives it nothing to
  read: what nothing read is named by the scanners' coverage notes, never a clean line.
- **A custom rule checked by `code`** (the policy snapshot's custom rules; added 2026-11-04) is the
  `canyon/scanners` run's, never the agent's, while it is on. The run reads the app's facts as
  Canyon reads them — the package's languages, files, dependencies, and the services and roles its
  `canyon.toml` declares, and the app's secrets by name, `{name, env, where}` in every environment as
  they stand when the review runs — and evaluates the rule's `expr` over them, once, under the
  facts' bounds and a 2-second deadline. **The dependencies** (facts version 3; added 2026-11-04) are
  what trivy lists of the package's files, run offline by Canyon's executor in the same review, and
  only while `sarif.trivy` is on and carries `run`; while it is not,
  or the scan did not finish, the fact is missing, never empty (`Canyon reads an app's packages with
  Trivy, which is off on this Canyon`, `Trivy did not finish reading the app's packages`). Each is
  `{name, version, ecosystem, direct}`: `ecosystem` the package's purl type (`go` for `golang`),
  `name` from its purl (a Maven package `group:artifact`, another namespaced one `namespace/name`, a
  PyPI one normalised as PEP 503 says), `version` exactly as the file locks it. A lockfile's development
  packages are among them; the app's own packages (trivy's `root` and `workspace`) are not. A file that names every package the app runs with is
  read — `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock`, `poetry.lock`, `uv.lock`,
  `Pipfile.lock`, a `go.mod` of go 1.17 or later, `Cargo.lock`, `Gemfile.lock`, `composer.lock`,
  `packages.lock.json`, `gradle.lockfile`, `mix.lock`, `pubspec.lock`, `Package.resolved`,
  `Podfile.lock`, `conan.lock`; any other file trivy lists packages of, a manifest without the file
  that locks it, a package with no version, and a language of the app with no such file make the fact
  missing, in a fixed reason (`requirements.txt names the packages it asks for, not every one pip
  installs: a uv.lock, poetry.lock or Pipfile.lock names them all`; `pom.xml doesn't name the packages
  Maven adds when it builds: a gradle.lockfile, or building with Gradle, names them all`). `direct` is
  trivy's: true for `direct`, false for `indirect`; where trivy cannot say (a `gradle.lockfile`), it is
  unknown, and an expression that reads `direct` — a field select CEL's checked tree holds, never a
  match on its text — could not be checked while any is (`dependencies: gradle.lockfile doesn't say
  which packages the app asks for itself`); one that never reads it is evaluated as any other. The
  outcome is one of three, and the run is never failed for it:
  - **It holds**: no result.
  - **It does not hold**: one `custom.<slug>` result, severity `high`, so it blocks wherever the rule is
    Blocking, in either gate. At the package's first file in path order, line 1, columns 1–2, its
    snippet `custom.<slug> checked by code: <says>`, `<says>` the check's own; its `title` the rule's
    name; its `problem` `"<name>" (custom.<slug>) does not hold for this app: Canyon checked it by code:
    <says>`; its `fix` `Change the app so that this holds: <says>`; its `required_end_state`
    `Canyon's check of custom.<slug> holds for this app when the review runs again: <says>`; its
    `do_not` `["Rename or move what the check reads just so it stops matching, without doing what the
    rule asks.", "Ask an admin to switch the rule to the AI review just to get past this finding."]`.
  - **It could not be checked**: a fact the expression reads is missing (Canyon reads a fact it
    cannot read whole as missing, never empty), its evaluation erred or ran to the deadline, it does
    not compile in this Canyon's facts, or its `facts_version` is not the version this Canyon reads.
    One `custom.<slug>` result, never a pass and never `gate.unreadable` (which blocks in the preview
    gate alone): severity `high`, so it blocks exactly where the rule is Blocking, in either gate, and
    is a warning where the rule is Advisory. At the same place, its snippet `custom.<slug> could not be
    checked by code`; its `title` `Canyon could not check "<name>"`; its `problem` `could not check:
    custom.<slug>: <why>`, `<why>` Canyon's reason in grammar words, file paths, language and
    package names, never a file's contents and never a value the evaluation read
    (`dependencies: Python with no uv.lock, poetry.lock or Pipfile.lock`, `dependencies: pom.xml: a
    package has no version`, `languages: Canyon could not read src/x.h`, `it did not finish within 2 seconds`, `its evaluation failed`, `this rule was saved for an
    older Canyon: an admin saves it again`, `this Canyon can't run this rule's check: an admin saves it
    again`); a path in it is at most its last 60 characters; its `fix` `Change the app so Canyon can
    read what this rule checks, or have an admin switch the rule to the AI review.`; its
    `required_end_state` `Canyon checks custom.<slug> by code when the review runs again.`; its
    `do_not` `["Remove the file Canyon could not read just to get past this finding."]`.
  - **Approvals**: either result is approved as every blocking finding is (`POST /api/v1/gate-runs/:run/findings/:fingerprint/approve`: an
    admin or a security responder who did not start the review, with a reason), and its approval is
    keyed by its fingerprint, which frames one more field (below): the facts the expression read when it
    did not hold, or the reason it could not be checked. So an approval of the one never covers the
    other, an approval of a failure covers those same facts only — a deploy that changes what the rule
    reads is judged again — and an approval of a could-not-check covers that same reason only.
- **`design.org-system`, from the package's manifests**. While the rule
  is on and the snapshot carries the org's default design system (`capabilities.design_systems.org_default`),
  the `canyon/scanners` run reads every file named `package.json` outside `.git/`,
  `node_modules/` and `vendor/` — an app's UI often sits in a subfolder (`web/`, `client/`) beside a
  server's manifest at the root — each with a JSON parser, never by pattern. The app uses the design
  system when any of them lists the default's `namespace` as a key of its `dependencies` or
  `devDependencies`, byte for byte. The two rows Canyon ships are
  not npm packages: an agent copies their components in with the shadcn CLI, which records it in
  `components.json`. So under `@shadcn/ui` the app uses it when any `components.json` outside those
  directories is a JSON object, and under `@canyon/ui` when any `components.json`'s `registries` object has the key
  `@canyon`. The rule judges only an app with a UI: one where any of those manifests lists, the same way, one of
  `react`, `react-dom`, `preact`, `vue`, `svelte`, `solid-js`, `@angular/core`, `lit`, `ember-source`,
  `@builder.io/qwik`, `next`, `nuxt`, `@sveltejs/kit`, `astro`, `gatsby` or `@remix-run/react` — the UI libraries,
  and the frameworks that bring one without listing it — or any `components.json` is a JSON object, which only the
  shadcn CLI writes, for a UI. An app with neither has no result: its run carries, after any note below, one
  `note`, never a `warning`, `design.org-system: no package.json in this app lists a UI framework, so there was no
  UI to check`, with the rule's `associatedRule` and `properties.canyon` as below. Nothing was left unchecked, so it
  never sets the run's `not_checked`, and surfaces show it in its own group, `Didn't apply`, by the rule's policy
  name, never among what nothing read.
  Loose HTML, templates and JSX with none of those listed are no UI here: telling a page from an email template by
  its file is a guess, and the two systems Canyon ships build on React. Otherwise the run carries one result of
  `design.org-system`, severity `high` — advisory while the rule is, blocking only when an admin sets it
  Blocking — on the manifest nearest the package root (fewest `/` in its path, then path order), its
  region the top-level `"dependencies"` key, quotes included, else its opening `{`, its snippet that line:
  - `title` `The app doesn't use <name>, the org's design system`;
  - `problem` `The org's design system is <name> (<namespace>), and no package.json in this app lists
    it. Canyon read <path>, <path> and <path>.`, every manifest read, in path order (`Canyon read
    <path>.` for one, `<path> and <path>` for two); under `@shadcn/ui` `…, and this app has no
    components.json, which the shadcn CLI writes. Canyon read …`; under `@canyon/ui` `…, and no
    components.json in this app lists the @canyon registry. Canyon read …`, the components.json files
    after the manifests;
  - `fix` `Add <namespace> to the dependencies of the package.json that holds the UI (registry:
    <registry_url>), and build the UI from its components.`; under `@shadcn/ui` `Run npx shadcn@latest
    init where the UI's package.json is, then add components with npx shadcn@latest add <component>.`;
    under `@canyon/ui` `Add "@canyon": "<registry_url>/{name}.json" to registries in the UI's
    components.json, then add components with npx shadcn@latest add @canyon/<component>.`;
  - `required_end_state` `The app's UI is built from <name>'s components, and its package.json lists
    <namespace>.` (under the shipped two, `…, and its components.json records it.`); `do_not` `list
    <namespace> without using its components`, `copy <name>'s components in by hand`.
  A `package.json` that is not valid JSON is not read, and is named by its own note (below); with no
  manifest read there is no result, only those notes. A `components.json` that is not valid JSON
  records nothing. The rule off, or no default set, nothing is read and nothing is said: with no
  default there is nothing to check against, as `stack.blessed` reads with no approved buildpack,
  and a line on every review would call every clean verdict
  partial (below) for a choice the org has not made.
- **What nothing read is named, never a clean verdict and never blocking**.
  The `canyon/scanners` run carries `warning` notifications, in this order:
  - `not checked on this host: <rule>, …` for the enabled rules this host could not run.
  - `not checked: <rule>: <Language> (<n> files) — no rule Canyon runs reads <Language>`, one per
    enabled rule a tool fills and language of the
    package's files no rule of that tool, mapped into that rule, names in its `languages`: the
    package's languages are go-enry's (Linguist's data) over its files outside `.git/`, `node_modules/` and `vendor/` (never Linguist's vendor list, which takes an app's own `external/` or `cache/`),
    the languages apps are written in only — JavaScript, TypeScript, TSX, Python, Go, Java, Kotlin,
    Ruby, PHP, C#, Rust, Scala, Swift, C and C++, never a build or shell script; Opengrep reads, for a rule, the languages of each rule it is sent, the vendored ones and Canyon's, whose
    CWE that rule's row selects (an org's rules repo adds none here, so a language it reads may be
    named), and ESLint JavaScript and TypeScript — so `sql.string-concat` reads `Ruby (3 files)` not
    checked when no CWE-89 rule names Ruby. Rules in the snapshot's order, languages in name order.
    While that rule's AI twin (`ai.sql-string-concat` for `sql.string-concat`, `ai.code-injection`
    for `code.injection`) is on and the host has a model, the line for the language's files the reviewer
    is shown is instead the `note`
    `checked by the AI review: <rule>: <Language> (<n> files) — no scanner rule reads <Language>`, and
    the rest keep the `not checked` line with their own count, after the note: the
    AI review reads those files for that rule (*The reviewer's run*), and when it reads nothing its
    own run's not-checked warning says so.
  - `not checked: deps.known-vuln: <n> unpinned requirements in <file> — trivy reads == pins only`,
    one per `requirements*.txt` holding a requirement line that pins no version with `==`.
  - `not checked: deps.known-vuln: <file> — trivy does not read npm-shrinkwrap.json`, one per
    `npm-shrinkwrap.json` outside `.git/`, `node_modules/` and `vendor/`: the pinned trivy's filesystem scan reads no
    such file (its documented files; measured), so its dependencies are never checked.
  - while `auth.unauthenticated-route` is the AI review's and on, on a host with no model, the one
    line `not checked: auth.unauthenticated-route: no model is configured on this Canyon host, so the
    AI review read nothing`. With a model there is no line: the reviewer is shown every file of an
    application language but a dotenv one or one under `.git/`, `node_modules/` or `vendor/`, and one not
    valid UTF-8, code in those folders, a source past its 16 MiB, or a file too large for one call, is its
    own warning (the per-language line it replaces named only files the extensions missed).
  - while `design.org-system` is on and the snapshot carries a default (*`design.org-system`*, above):
    `not checked: design.org-system: no package.json in this app; Canyon reads the design system from
    package.json`, when no manifest is there to read (a Python or Go app, say); and `not checked:
    design.org-system: <path> is not valid JSON`, one per manifest the parser refuses.

  Each line above that names one rule, every one but `not checked on this host`, the `checked by
  the AI review` note included, carries that rule as SARIF's `associatedRule.id`, and its
  `properties.canyon` holds `policy_name`, the snapshot's name for the rule (as a finding's), and
  `why`, the line's words after `<rule>: `: a surface names the policy, as it names a finding's,
  and keeps the reason whole. The `message.text` stays as above, read whole by the verdict log and
  the CLI.

  After them, one `note`, never a `warning`: `Canyon builds with buildpacks and does not read this
  Dockerfile`, when the package tree's root holds a file named exactly `Dockerfile`. Buildpacks
  build the package, so the Dockerfile a builder wrote is not what runs, and nothing
  else says so. A `Dockerfile` below the root (`docker/Dockerfile`) gets none. It says nothing was
  left unchecked, so it never sets the run's `not_checked`, and surfaces show it under the run's own
  row, never among what nothing read. A result of one of checkov's dockerfile checks, on that
  `Dockerfile` or any other, says so in its own
  words too: its `problem`, trailing `.` and spaces trimmed, ends `. Canyon builds this app with
  buildpacks and never builds from this Dockerfile, so this does not block; it matters only where
  something else builds from it.` — Canyon's sentence, never masked — and its `title` is that
  problem's first sentence as ever. Its policy and severity are unchanged, and it never blocks
  (a blocking finding on a file Canyon says it does not read; such a finding is an advisory whatever `iac.misconfig`'s state, on any
  Dockerfile, the sentence saying so).

  `<n> files` reads `1 file` for one. A verdict whose log carries any line saying `not checked` — these, and the
  reviewer's own over its cap — is never called clean: where a clean verdict reads `clean` or `passed`, it reads `nothing blocking in what
  was checked` — a clean run may carry warnings, so never "no findings" — on the CLI's verdict line
  and the dashboard's alike (the run read's `not_checked`; `--format json` carries it as
  `not_checked: true` beside `result: "passed"`, which CI reads), and the lines are shown under it. It still does not
  block.
- **`region` is the redaction span.** The `snippet.text`,
  `message.text`, and every string in `properties.canyon` — `required_end_state` and each
  `do_not` entry included — are redacted at construction from the regions. A finding built from a known literal and serialized every way the product can — including the
  SARIF log itself — shows the literal nowhere. On a `secrets.*` finding of Canyon's own scanner — not an imported result the
  policy table maps under `secrets.*`, whose region is hidden whole
  (*In: external SARIF*) — the region is the string literal as the producer's
  parser delimits it, **quotes included**: the
  credential is what lies between the delimiters, and the quotes stay visible around the mask.
  A **template literal** is one such token, backtick to backtick, substitutions and all; when it
  is **tagged** — `` gql`…` ``, `` styled.div`…` ``, `` sql`…` `` — the token begins at the
  backtick and the tag is not part of it. A producer whose parser delimited the token **declares
  the token itself**, source text and delimiters included, and the gate checks it is what the
  span's own lines and columns cut out: the two come from one token or they disagree, so a span
  that names the wrong columns cannot be declared. What the gate cannot do is re-derive lexical
  state — telling a literal's opening delimiter from the closing one of the literal before it
  needs the lexer that read the file, not the line it sat on — so a declared span is the
  **producer's parser's assertion**, checked for self-consistency, and in-tree producers being
  reviewed is what stands behind it. Whether or not it is declared, the span must be **one whole
  token**: opened by a string delimiter, closed by that same delimiter unescaped, and carrying no
  unescaped one in between. A span cut at a `\"` ends inside the literal and leaves the rest of it
  in the snippet; a span carrying an unescaped delimiter closes on some later token's end and masks
  across two literals, leaving a fragment of each. Those are properties of the text the span's
  coordinates cut out, so they hold for a declaration and a guess alike. A `secrets.*` snippet is
  the span's whole line, so it can hold a key another finding names. Any live key in it
  is masked as well, and so is every string literal whose value is one once its escapes are decoded, whenever the snippet lexes on its own. So two keys
  on one line are two findings and neither snippet shows either key.
  A producer that declares nothing gets one further rule, the conservative
  reading: the span must abut whitespace or the punctuation an expression is built from on each
  side, and anything else is refused. That is a guess kept safe, not a verified token. The
  punctuation it admits is `(`, `)`, `[`, `]`, `{`, `}`, `,`, `;`, `:`, `?`, `*`, `%`, `!`, `<`,
  `>`, `&`, `|`, `^`, `~` — deliberately **not** `=`, `+`, `-` or `/`, which are what base64,
  base64url, a JWT and a UUID are built from, so that a span starting on one literal's closing
  delimiter has a credential's own last character beside it and is refused. The cost is the
  unspaced forms: `return"x"`, `a+"x"+b` and the JSX attribute `apiKey="x"` are all refused when
  nothing declared the token. The spaced forms pass, and an in-tree producer declares.
- **Columns count Unicode code points** — 1-based, `endColumn` exclusive — and every run says
  so with SARIF's own `"columnKind": "unicodeCodePoints"`, so GitHub and every other SARIF
  reader place the span where Canyon does (SARIF's default is UTF-16 code units). The profile
  accepts no other value, and a run without it is refused.
- **`snippet` holds the span's lines — or a window of them, below — and carries `text` and nothing
  else, and is ≤3 lines as serialized — after redaction, not before**; a location, its region and its snippets carry only
  the fields above, so nothing SARIF allows beside them (`binary`, `rendered`, offsets) slips past
  redaction. The cap binds what is written, which is the only form a log off the wire ever has, and
  it is the one rule: nothing caps the span before redaction, since that is what refuses a template
  before it can collapse. On a `secrets.*` finding a literal that spans lines is masked whole, so a
  template of any length serializes as one line — the first line up to the opening delimiter, the
  mask, the last line after the closing one — and `region.startLine…endLine` may name more lines
  than the snippet shows. Off `secrets.*` nothing is masked, so the span's own lines are the
  snippet and more than three of them are refused all the same — but for an imported region, which
  shows its first three (*In: external SARIF*). A line ends at **LF, CR, CRLF,
  U+2028 or U+2029** — the same five the mask treats as terminators, so a snippet the mask hides
  whole cannot pass the cap by carrying a separator the counter does not know. Both implementations
  of the count (the gate's and the contract check's) are bound by
  that list, not merely by agreeing with each other.
- **`region` is always the file's: its lines and columns are where the span is in the file as
  written**, which is what a SARIF reader highlights and what the reviewer masks from. When the
  snippet as serialized puts the span somewhere else — a masked literal shorter than the key, or a
  window — the result carries **`canyon.shown`**, `{startColumn, endColumn}`: the caret for
  `locations[0]`, counted in code points on `snippet.text` as serialized, `startColumn` on its
  first line and `endColumn` (exclusive) on its last. It is present only when it differs from the
  region's columns; absent, the region's columns are the caret on the snippet. Either way the caret
  lies within the snippet's lines, and a renderer draws it from `shown` when present. The
  fingerprint does not read it.
- **A snippet over 400 characters is a window, never a failed run**. After
  redaction, the text is cut to 398 characters, with `…` (U+2026) in place of each cut end, so it
  is at most 400. It starts 100 before the span's start (or at the snippet's start), moved back by
  at most 50 characters to the start of the word the cut would split — a run of letters, digits,
  `_`, `.` and `-` — so a cut never leaves part of a name a secrets detector skips whole
  (`publicKey`'s `ublicKey`, which gitleaks reads as a key's name). If the window then holds a
  live key the whole text doesn't, its start moves back a word at a time until it holds none, as
  long as the span still starts inside it. That happens when the cut drops a match the detector
  skips whole and leaves a key name inside its value reading alone: `token=…` cut from
  `publicKey = "token=…"`. A span longer than the window is cut with it, and `shown` places the caret in the window. The window
  keeps the ≤3-line cap (it only removes text) and is part of the snippet the fingerprint hashes.
- **There is no `contextRegion`**: a location carries its `region` and nothing
  else, and the schema refuses one that carries SARIF's `physicalLocation.contextRegion`. The
  lines around a span are source nobody has reviewed — any of them can hold a credential no span
  flags, and redaction masks only what a span names — so no finding ships them.
- **`partialFingerprints["canyon/v1"]` is derived by the gate, last, over the final log —
  after namespace drops — and overwrites whatever a producer wrote.** For each result, with
  `r` = `locations[0].physicalLocation`:
  `"f_" + lowercase_hex(sha256(frame(ruleId) + frame(r.artifactLocation.uri) + frame(r.region.snippet.text) + frame(decimal(r.region.startColumn)) + frame(decimal(n))))`,
  where `frame(s)` = `decimal(byte_length(utf8(s))) + ":" + utf8(s)`; **for a `custom.*` result of the
  `canyon/scanners` run** (a custom rule checked by code, *Custom rules*, above) one more field is
  framed after `n`: `frame(lowercase_hex(sha256(f)))`, `f` the facts the rule's expression reads as one
  JSON object, keys in byte order, each fact by name, when it does not hold, or `could not check: <why>`
  when it could not be checked, a fact past its bounds keyed by its reason without the count (`files:
  more than the 10000 Canyon checks by code`), so a deploy that only moves the count keeps the approval
  — stored nowhere, so an approval follows those facts or that reason and
  no text anyone reads carries it — and its uri is framed as the empty string: its place is the
  package's first file, whichever that is, so a file added before it never moves the fingerprint; **for a result of a
  `sarif/<tool>` run only**, one more field is framed after `n`:
  `frame(lowercase_hex(HMAC-SHA-256(k, t)))`, `t` the region's bytes as they stand in the package
  file, start to end, line terminators as stored — empty on a file that is not valid UTF-8, whose
  bytes are never hashed — and `k` the project's key: 32 random bytes Canyon
  generates per project, sealed in the vault, never exported or shown. The gate re-reads the region from the package's
  stored tree when it derives `canyon/v1`; the digest is stored nowhere, so the snippet stays
  hidden and an edit inside the region moves the fingerprint. A file-level result's region (In:
  external SARIF) is one line of its file, so an edit elsewhere in the file does not move it: its
  approval is that finding in that file, and once the tool stops reporting it, the approval
  matches nothing. On a file that is not valid UTF-8 a file-level result has no line and `t` is
  empty, so no edit to the file moves it; its approval is that finding in that file the same way.
  Every other result's recipe is
  the five fields above, unchanged — all 64 digits, the snippet text exactly as serialized and never normalised; a string that is
  not valid Unicode fails the run closed. **Every field is framed by its UTF-8 byte length, never
  its code-point count**, so a language whose string length counts code points (Python) and one
  whose length counts bytes (Go) derive the same digest for a snippet carrying the mask. The
  framing is what makes the serialization unambiguous: a uri may carry a newline and a snippet is
  legitimately multi-line, so joining the fields on `"\n"` lets two different findings hash alike
  — within one run the duplicate fails the run closed, but across runs a crafted finding could
  inherit another finding's approval. `startColumn` is counted in the run's `columnKind`. `n` is
  the occurrence: the 0-based index, in log order, among the log's results with the same
  `ruleId`, uri, snippet text and `startColumn`; a `sarif/<tool>` run's results
  are sorted at attach by (uri, `startLine`, `startColumn`, `ruleId`), strings by code point, so
  the tool's output order cannot move a fingerprint. So one line repeated in a file is two findings, and a sibling that differs in any of the four never renumbers a finding. An
  index over results sharing only rule, uri and snippet let a sibling added above renumber a
  finding, and let a different finding on another column become the first and inherit its
  approval — which is what lets a deploy through. The one case the recipe leaves ambiguous is
  identical flagged text at the same column, which is the same finding by definition. On a
  `secrets.*` finding that includes a different literal masked to the same text: a multi-line
  literal masked whole, replaced at the same column, keeps the fingerprint — a recorded residual.
  An imported finding's fingerprint frames a keyed digest, never a bare hash, so a published
  fingerprint is no commitment to the hidden bytes that a low-entropy secret could be guessed
  against offline without the project's key.
  It stays the same across runs while the flagged lines are unchanged, wherever they move in the
  file — approvals key on `(project, rule, fingerprint)`
  and match all of it; surfaces may show the first 12 digits — and moves when
  those lines change, whitespace included. `relatedLocations` are not part of it. On a
  `secrets.*` finding the mask is part of the snippet. The recipe changed once before any
  release; approvals stored under earlier fingerprints stop matching
  and fail closed. An imported region past three lines changed its snippet once, before any release
  (hidden whole, then its first three lines), so an approval of one stops matching
  the same way. Any change to the recipe or the mask after release is `canyon/v2`.
- Voice: the subject is the code, never the person; no "violation"/"failed";
  `problem` names the real consequence; `fix` is executable.

## In: external SARIF (the adapter)

**A report is the uploader's word that the tool ran on this package.** Canyon checks it matches
the package, not that it is complete: leaving a report out, or replacing one, is visible (the audit
row and the verdict's line for the tool), not prevented. Canyon does not run these tools; the six
it can run itself are *Canyon-run tools*, below.

- **Only tools the snapshot carries.** Gitleaks, Trivy, Opengrep, ESLint and Checkov always;
  any other is added by applying its rule `sarif.<tool>` with a state; an
  upload never creates a rule.
- **Bound to a package.** A report arrives by `PUT /api/v1/projects/:id/packages/:pkg/reports`:
  SARIF 2.1.0, at most 10 MiB, exactly one `run`. The tool is its
  `tool.driver.name` lowercased, each run of characters outside `[a-z0-9]` replaced by one `-`,
  leading and trailing `-` trimmed: 1 to 40 characters, else the report is refused
  (`Semgrep OSS` → `semgrep-oss`). Its category is the run's `automationDetails.id` normalised the
  same way, empty when absent, at most 128 characters, else the report is refused: CodeQL writes
  one report per language under one driver name, told apart only by it. One report per (package, tool, category); a second
  replaces it. A gate run includes the reports attached before it started, and its own scans'
  attaches, which replace them; a report attached after the run started is the next run's. The run
  reads them by id — the digests it journals when it starts and when each of its scans attaches —
  never whatever is newest when the gate reads.
- **Normalised at attach, never in a run.** An error in the report refuses it whole (`400`,
  nothing stored): not SARIF 2.1.0 JSON; a compressed body; not one run; a tool the snapshot does
  not carry; over 1,000 results (counted before any file is read); a run's `newlineSequences`
  with more than 8 entries, a duplicate, or an entry that is empty or over 8 bytes; a
  `driver.version` over 128 code points or carrying a live key; a rule's
  `security-severity` that is neither a number from 0 to 10, as a JSON number or a numeric string,
  nor one of the words `critical`, `high`, `medium`, `low` or `info` in any ASCII case; a rule's
  `shortDescription.text` over 1,024 characters; a
  result with no `ruleId` (or `rule.id`) of 1 to 128 printable ASCII characters and no whitespace;
  `level: none`; no `message.text`, or one over 4,096 characters; a fix it takes
  (`fixes[0].description.text`, or the rule's `help.text` in its place, unless it is the rule's whole document, which is no fix) over 4,096 characters;
  no `locations[0].physicalLocation` with an `artifactLocation.uri`, or one whose `region` has no
  `startLine`; a region the input rules below refuse; a live key the unredacted-literal
  check finds, or a `ruleId` carrying region text it finds. Otherwise a result is **dropped and counted**, never refused, when its `kind` is
  anything other than `fail` (SARIF's default when absent), a value SARIF does not name included,
  its `suppressions` is non-empty, its uri is refused by the input rules below or is not a file of
  the package, its file is not valid UTF-8 and it is not file-level (below), its region is empty
  (it starts where it ends) other than as a file-level result's, its region ends past its file,
  or it is file-level and its file, valid UTF-8, has no non-empty line; the answer is
  `{tool, category, results, dropped, first_dropped?}`. Nothing is
  decided in a run: a run never drops an imported result and never fails on one.
- **Input rules.** `artifactLocation.uri` is percent-decoded once, and the decoded uri — the tree
  path the result is stored, sorted and fingerprinted under — is looked up exactly in the package's tree index — never joined to a filesystem path — and its result dropped
  if it is absolute, carries a `..` segment, a backslash or any scheme (`file://` included), or a
  `uriBaseId` present and other than `%SRCROOT%` or `ROOTPATH` (absent is allowed; `ROOTPATH`,
  which trivy writes, is read exactly as `%SRCROOT%`). A region is lines and columns: an omitted `startColumn`,
  `endLine` or `endColumn` takes SARIF's default; a region given only by offsets (`charOffset`, `byteOffset`),
  a number below 1 or above 1 000 000 000, or an end before its start is refused. Columns are
  counted in the run's `columnKind` (SARIF's default, UTF-16 code units, when absent) and
  converted to code points against the file; a column inside a character is past the file. Lines
  are placed by the run's `newlineSequences` (SARIF's default `["\r\n", "\n"]` when absent) and
  restated under that default, which is what the verdict's run, carrying none, declares.
  **A file-level result** — one with no `region`, or whose region starts and ends at line 1,
  column 1, which is how grype places every dependency's finding, and trivy a dependency it has
  no lines for — names its file, not a span in it: it is kept. In a lockfile trivy (0.74.0) places
  a dependency at its package entry's lines — lodash 4.17.20 in `package-lock.json` at 20:1–23:1 —
  a span the input rules read like any other, so the finding keeps that location. On a file that
  is valid UTF-8 a file-level result's region is restated as the file's first non-empty line
  whole, column 1 to that line's end under that default; a file with no non-empty line has nothing
  for it to name, and the result is dropped. On a file that is not
  valid UTF-8 — a jar, where grype places a dependency it found inside — the file's bytes are read
  only to learn that they are not valid UTF-8 and whether line 1 is empty (they begin with one of
  the run's `newlineSequences`), and nothing of them is kept, shown or hashed; its region is line
  1, columns 1–2: a placeholder, as `gate.unreadable`'s is, since the file has no lines to place.
  An empty region anywhere else is dropped.
- **What Canyon trusts:** locations, rule ids, levels and messages — never the tool's snippets,
  `contextRegion`, `partialFingerprints` or `fingerprints`, which are ignored. The snippet is read
  from the package's stored tree at the region, never a checkout, and the placeholder takes
  nothing from its file. **An imported region is shown as a Canyon finding's is**: its
  lines from the stored tree, windowed past 400 characters, with any live key in them masked as on
  `secrets.*`, so a person sees the code a tool flagged (a hidden
  `pool.query(••••)` told nobody what blocked). **It is hidden whole** under `secrets.*`; on every
gitleaks result, whatever it maps under, since gitleaks matches values it decoded that no mask
reads; where its
  rule's `properties.tags` call it a credential (`secret`, `CWE-798`, `CWE-259` or `CWE-321`, a CWE
  read as the policy table reads one), whatever policy it maps under; on a file-level result (below);
  where a line of it carries a terminator other than LF; and on a dependency result (below) past the
  snippet's three lines, a lockfile's package entry whose `resolved` URL may carry a registry's
  credential no mask reads. **Otherwise a region past the snippet's three lines is shown by its first
  three** (ESLint's region over a whole seven-line `fs.readFile(…)`
  call was hidden whole and printed `••••;`, a line with no secret in it masked as if it held one):
  the snippet is the region's lines from `startLine` through `startLine + 2`, from the first line's
  start, windowed and with any live key in them masked as on any shown region, `shown` ending at the
  third line's end, or the window's; the region stays the file's. A shown region that will not
  build (a live key straddling its edge, or the third line's end) is hidden whole instead, never refused. Hidden whole, the snippet is the region's first line up to `startColumn`, then `••••`, then
  its last line from `endColumn` — one line, whatever the region spans, `shown` the mask's four
  columns, so no delimiter or token
  rule applies to it; the snippet cap still counts the profile's five terminators, so the first
  line's part is cut after the last of them before `startColumn` and the last line's before the
  first after `endColumn`. Any live key left in it is masked as on `secrets.*`.
  A file-level result's snippet is the mask alone, `••••` — its region is one line whole, or the
  placeholder on a file that is not valid UTF-8 — so nothing of the file is shown, and its card's
  file:line is all it names.
  Every string a result takes from the report (`ruleId`, `message.text`, a fix, the rule's
  `help.text` and `shortDescription.text`) runs through the unredacted-literal check at attach: it may carry no live key,
  and, on a result whose region is hidden whole, other than a dependency result, no region text of 8 or
  more code points (the placeholder has none; a shown region is in its snippet already, so quoting it
  hides nothing, and the lines a region shown by its first three leaves out hide nothing either: a live
  key quoted from them is refused as every string's is): a `ruleId` carrying such a run is refused — except on a report Canyon's own
  scan attached (*Canyon-run tools*), whose ids name rules Canyon sent or the tool carries — a grype id
  its vulnerability and package — never the code: `javascript_react_rule-dangerouslysetinnerhtml` beside the flagged
  `dangerouslySetInnerHTML` is not refused there, and an upload carrying it is — and in every other string each
  run of 8 or more code points it shares with the region is **masked, not refused** — each maximal
  stretch of code points that lie in such a run is replaced by one `••••` before the string is used
  — so the region stays hidden from the stored text, and a pinned
  ruleset whose rule messages carry a code example (GitLab's sast-rules: `cur.execute("…` beside the
  flagged `db.execute("…`) is not refused for it. **A dependency result** is one of trivy's whose rule's `properties.tags`
  carries `vulnerability`. Its region is a lockfile's package entry, and its message and its rule's
  `help.text` carry the advisory's link, whose `https://` is 8 code points the entry's `resolved`
  URL carries too, so the region-text half refused every real trivy vulnerability; the live-key half reads its every string as any other's, so a forged
  dependency result can carry a lockfile line, a credential in a `resolved` URL included, into
  stored text: the attacher could read that line already, and the attach is audited under their
  name. Text only; markup is never rendered. The mask answers, as a refusal did, whether 8 code
  points are in the region, so for whoever attaches it reads the file a character at a time: the
  per-actor rate limit is the brake, and
  attach rights go only to actors who can read the project's code.
- **From the tool into the profile**, per result:

| From the tool | Into the profile |
|---|---|
| `ruleId` | `ruleId` = `sarif.<tool>.<ruleId>`; the finding takes its state from the snapshot rule `sarif.<tool>`, and `policy_name` is that rule's name. A result a row of the policy table selects is `<policy>.<tool>.<ruleId>` instead, whatever the policy's state, its state in each run and `policy_name` the policy's, and on a `secrets.*` policy it carries `secret_name`, the name the policy gives that secret (the rule's page shows it, such as `STRIPE_ACCESS_TOKEN`) |
| the row's severity, when a row selects the result and names one | that severity |
| else rule `properties.security-severity` if present: GitHub's 0–10 scale, a JSON number or a numeric string, or a word | a number by GitHub's bands: ≥ 9.0 critical · ≥ 7.0 high · ≥ 4.0 medium · else low; a word, in any ASCII case, as itself — `critical`, `high`, `medium`, `low` — and `info` as low. The words are what the GitLab sast-rules Opengrep runs write (`High`, `CRITICAL`): their own rating, so Canyon reads it rather than the rule's level, which rates their SQL injection rules `warning` |
| else `level` | `error` → high · `warning` → medium · `note` → low · absent → SARIF's default (the rule's `defaultConfiguration.level`, else `warning`) · `none` → the report is refused |
| `message.text`, after the rule's `shortDescription.text` if present | `canyon.problem`: the rule's `shortDescription.text` with trailing `.` and spaces trimmed — and when the text before its first `: ` is not empty and is followed, after that `: `, by the same text in any ASCII case and `: `, with the first copy and its `: ` left out, once (trivy's `lodash: lodash: Arbitrary code execution…` reads `lodash: Arbitrary code execution…`, and `lodash: Lodash: Prototype pollution…` reads `Lodash: Prototype pollution…`; the 1,024-character limit below is the description as the tool wrote it) — then `. `, then `message.text`, unless `message.text` with trailing `.` and spaces trimmed is that same description, when it is the description alone (checkov's message is its check's description: it read `Ensure port 22 is not exposed. Ensure port 22 is not exposed`); or `message.text` alone when the rule has no `shortDescription` (gitleaks' message names only its rule and file, so the description is what says what was found). Wherever `message.text` is read here, a message that is the rule's `fullDescription.text`, both with surrounding whitespace trimmed — the rule's whole document, not this result's words — is read as that document's first paragraph: its text before the first blank line, its lines' surrounding whitespace dropped and the lines joined by one space (the GitLab sast-rules opengrep runs write the rule's document as every result's message: a SQL injection's problem read 36 lines, wrapped mid-sentence where the rule's YAML wrapped) (a checkov dockerfile check's problem ends in Canyon's sentence, as the root Dockerfile note above says) · `canyon.title`: the problem's text before its first `. `, or all of it when it has none, cut to 120 code points: over 120, its first 119 cut back to the last space in them, or all 119 when there is none, trailing space dropped, then `…` (a CWE's own name runs past 80; never mid-word, as `can require si…` was) |
| `fixes[0].description.text` if present, else the rule's `help.text` | `canyon.fix`; for a result a row selects, the policy table's fix whatever the tool offers (trivy's help is the advisory, opengrep's its message), unless the row's fix is the tool's (checkov's); a text that is the rule's `fullDescription.text`, both with surrounding whitespace trimmed, is the rule's whole document and no fix of the tool's own (the GitLab sast-rules opengrep runs write every rule's document as its `help.text`: the fix of `go_http_rule-http-serve` repeated the problem, wrapped where the rule's YAML wrapped); when the tool offers neither and no row selects it, `fix` reads `Change this code so <tool> no longer reports <ruleId> here; the line above says what it found.`, the line above it being `problem`, which carries the rule's description |
| the tool and `ruleId` | on every imported result, whether or not it blocks, since `blocks` is derived in the run: `canyon.required_end_state` reads exactly `<tool> no longer reports <ruleId> for this code when it runs again, and the app still does what it did.` and `canyon.do_not` is exactly `["Silence it in <tool>'s settings or with an ignore comment — Canyon does not read those.", "Remove or rename the flagged code just so the tool stops matching it."]`, `<tool>` the normalised tool name and `<ruleId>` the tool's own |

`canyon/v1` is derived last, as for every finding, with the region's digest framed in (the recipe
above), so an approval follows an imported finding across runs as it follows Canyon's and stops
covering it once the hidden code changes — for a file-level result, its one line, and on a file
that is not valid UTF-8 no edit ends it, only the tool no longer reporting it (the recipe above).

- **The raw report is not kept.** What is stored, and what the report's `producer.sarif` row holds
  (one timeline row per report a run includes, so per `(tool, category)`), is the report's sha256, the tool, its category and `driver.version`, the attaching
  actor's display, the dropped count and the first drop's reason, the rules it could not
  load and the files it did not read whole (below), and the normalised
  results: profile results without `partialFingerprints`, which the gate derives in the
  run, and whose `blocks` it overwrites there. Redacting arbitrary third-party JSON would be a guess.
- **In a run** each attached report of a tool whose rule is on is one run of the verdict log,
  driver `sarif/<tool>`, `automationDetails.id` its category when not empty, version the report's
  `driver.version` (else `unknown`). It carries the results and one `note`,
  `<tool> · report attached by <actor display>`, and one `warning`,
  `<tool> · dropped N result(s) at attach`, when N > 0. For a tool Canyon runs whose answer carries
  `unloaded` — the rules Opengrep dropped (an invalid regex, say) while it
  ran the rest — one `warning` per rule, `<tool> · not checked: rule <id> could not load`, `<id>`
  printable ASCII of at most 128 characters with no live key, else `<tool> · not checked: a rule
  could not load`; the first 20 in name order, then `<tool> · not checked: N more rules could not
  load`. So a rule the org wrote that never ran is named, never a clean line (*What nothing read is
  named*). Likewise, for a tool Canyon runs whose answer carries `unparsed`,
  and for any ESLint report, each `toolConfigurationNotifications` or `toolExecutionNotifications`
  entry of its run whose `descriptor.id` is `ESL0999` — ESLint's word that it could not parse a file —
  naming that file by its first location's `artifactLocation.uri`, one `warning`
  per path that is exactly a file of the package, `<tool> · not checked: <path> was not read
  whole`, the first 20 in path order, then `<tool> · not checked: N more files were not read whole`;
  a path the package does not hold names nothing. And a report of ESLint Canyon ran whose run names
  no `artifacts` — its image's formatter lists every file it linted — read none of the tree: each file
  of the package ESLint reads (its config's extensions, outside `node_modules`) is named so. No message text is read for either. A tool with no report for the package is
  one run with no results and one `warning`, `<tool> · no report for this package`, and no
  `producer.sarif` row, which holds a report's digest: its line comes from this run. A Canyon-run
  tool whose scan in this run did not finish (*Canyon-run tools*, below) has, on each of its runs,
  first, before its other notes, or in place of `no report for this package`, the `warning` `<tool> ·
  could not check: <why>`, `<why>` the reason its `gate.unreadable` finding's `problem` carries,
  whatever report the package already had: a report
  attached before this run never reads as this run's clean scan (a
  rerun whose trivy timed out listed `✓ trivy clean` above its own could-not-check). A Canyon-run
  tool that had no file of the package to read has, on its run, after any could-not-check, the `note`
  `<tool> · read no file: <why>`: ESLint, `no JavaScript or TypeScript here`, when the package's
  languages (*What nothing read is named*) hold no JavaScript, TypeScript or TSX; Opengrep, `no
  language its rules read is here`, when they hold none of the languages of the rules it is sent and
  it is sent no rules repo of the org's (`settings.rules`), whose rules' languages Canyon does not
  read. Only a run that holds no result carries it: a result is a file the tool read. Its
  empty run is then no clean line (a Rails app's `✓ eslint clean`
  read as a check of its code). Surfaces
  render each as the tool's row of the gate list — `✓ semgrep  1 warning · attached by ci-bot`,
  `– snyk  no report for this package`, `– eslint  no JavaScript or TypeScript here`, and on the CLI `✗ trivy  could not check: did not finish in time` — and an imported finding as any other, plus
  `from <tool> · rule <ruleId>`; its card shows file:line and never the hidden code. A tool whose
  rule is not on contributes nothing.

### A dependency's name and version

trivy names the dependency a vulnerability is in on its own lines of `message.text`, `Package:
<name>` and `Installed Version: <version>`, and the problem carries that text after the rule's
description (the mapping above). That is where a finding's dependency is read, and the only place:
in a `sarif/trivy` run, the problem's last `Package: ` before its first `\nInstalled Version: `,
with no line break between them, is the name — what follows it up to that line break — and the
installed version is the rest of that line. A finding with no such pair, or with an empty name or an empty version, names no dependency. A
file, a rule id, a lockfile entry or another tool's prose never names one: a tool that does not
write the two fields leaves its findings ungrouped. The run read carries it, and the findings of one dependency from one tool are what a person approves
together.

The lines after the installed version say the rest, each read only from its own line, the last of
each kind (the dashboard printed this message as a paragraph):
`Vulnerability <id>` is the finding's `vulnerability`; `Fixed Version: <versions>` is its `fixed`,
the versions as trivy wrote them; and `Link: [<id>](<url>)` is its `link`, the url, only when the line
is exactly that with the same id as `vulnerability` and a url of `https://` and then one or more
characters from U+0021 to U+007E other than `)` and `@` — no user part, so the host it names is the
one it opens. Each is absent when its line is missing or empty, and any other link is not carried: a
page links only https, and only under the id it names. An attached report whose driver is trivy
writes these lines too, so a link's host is the report's.
`fixed_in` is the one version to upgrade to: the lowest version that fixes every finding of the same
tool, dependency and `blocks` in the verdict whose `fixed` names any, carried on every finding of
that group, one with no `fixed` too,
by the rule the CLI uses for a package's advisories — a CVE's list names a fix per
release line, so a version fixes it when the list names one at or below it in its own `MAJOR.MINOR`,
or it is at or above the list's highest; candidates are the versions the lists name at or above the
installed one, a `>=X.Y.Z` read as `X.Y.Z` — and absent when the installed version or any listed
version is not a plain `X.Y.Z`, or no candidate fixes them all, or the group has more than 200
findings naming a fix or one of them names more than 32 versions: trivy names a handful, and a
report shaped otherwise gets no `fixed_in` rather than a read that runs for minutes (a security
review's finding). Blocking and advisory findings are
grouped apart, as the CLI groups them.

### One problem, one entry

A person counts problems, not results: two of a tool's rules matching one line, or one package's five
CVEs, are one thing to fix. An **entry** is the findings of one verdict that are one problem, and every
count a person reads — the verdict's `blocking` and `warnings`, every screen's, the CLI's, a GitHub
check's — counts entries. Blocking and advisory findings are never in one entry. Findings join when:

- **a package's CVEs**: trivy's findings this section reads a dependency from, of one Canyon rule (the
  policy before `.trivy.`; `sarif.trivy` for one no policy covers), one dependency name and installed
  version, and one file;
- **one rule on one line**: any other finding a tool found — its rule `<policy>.<tool>.<rule>` or
  `sarif.<tool>.<rule>` — of one Canyon rule (the policy, or `sarif.<tool>`), one file
  and one start line, whatever the tool;
- never for Canyon's own findings, a reviewer's or a scanner's with no tool's rule: two on one line are
  two problems.

One implementation numbers them: the verdict's counts, the receipt's and the
CLI's joins are its, and the run read carries its number on each finding
(`GET /api/v1/gate-runs/:id`, `entry`), which the dashboard joins and counts by — never a rule of its own.
The run read's own `blocking` and `warnings` stay its findings' results: a CLI released before entries
checks them against the list, and an old CLI never breaks against a new server. Each finding stays its own in the log, in
`--format json` and for every approval; an entry is open while any of its findings has no unexpired
approval.

### Canyon-run tools

Gitleaks, Trivy, Opengrep, ESLint, Grype and Checkov Canyon can run itself: Trivy, Opengrep,
ESLint, Grype and Checkov through its executor, while the tool's rule `sarif.<tool>` has `run` on,
with the tool's settings, and the snapshot carries both, so every run journals what it scanned with; gitleaks in process, while
`sarif.gitleaks` is on. All but Grype do so by default, with nothing for an admin to switch on. ESLint runs four rules fixed in its image, every result of which the
policy table maps to `code.injection`.

- **When.** A gate run of a package asks the executor's `POST /scan` once per tool
  whose rule carries `run` and is on, and runs gitleaks over the package's tree while
  `sarif.gitleaks` is on, before the gate reads the package's reports, and includes what those
  scans attached, by id. The tools scan at once, each in a step of its own, started in tool-name
  order; a tool that could not check the package is named in tool-name order (below). Gitleaks'
  report is Canyon's gitleaks scan's: its default ruleset, each finding's secret,
  match and line cleared before the report is written and its region widened to whole lines, so no
  byte of the report carries the secret; the report attaches like any scan's. Opengrep's rules are GitLab's sast-rules v2.10.1, its MIT, Apache-2.0 and LGPL-3.0 folders
  only, vendored and pinned by commit, and Canyon's one rule of its own:
  a TypeScript and JavaScript taint
  rule, tagged `CWE-89` so the table maps it into `sql.string-concat`, for SQL text built from a
  request — a Next.js route handler's request and params (destructured or `ctx.params`), a page's
  `searchParams` and `params` (destructured or `props.`), a Pages Router API route's `req.query` and
  `req.body`, a function's `FormData` parameter (a server action's), an Express handler's
  `req.query`, `req.body` and `req.params` — reaching the first argument of `query`, `execute`, `raw`,
  `unsafe`, `prepare`, `exec`, `$queryRawUnsafe` or `$executeRawUnsafe`, or its `text` or `sql` when
  that argument is an object (pg, mysql2), whose values are bound; the `sql` and `Prisma.sql` tags
  bind their substitutions, and `Number(…)` and `parseInt(…)` make a number of it. Its
  `security-severity` is `Critical`. Another framework's request (Fastify, Hono, Koa, …) is no source
  of it, and nothing names that. No open-source ruleset catches this; the rule goes the day
  one does. All of them, always,
  plus the rules repo its settings name, which the server fetches at that commit (below). **A scan
  waits its turn and never fails for waiting**: the host runs at most
  `CANYON_SCAN_CONCURRENCY` scans at once across every run and every server on its database (2 by default;
  `deploy/k8s` sets 10), first come first served by when each was queued, and a scan past that waits
  in the host's scan queue until a slot frees. A tool the executor runs one scan of at a time — grype
  on Kubernetes — has its own queue of one beside it, so its scans never hold the
  others' slots. A busy answer from the executor is asked again after its `Retry-After`, inside the
  scan's slot, for as long as the executor answers so — busy then means real overload. While a scan waits, its run's progress says so and where it stands.
  A wait is never a finding and never fails the run: past 10 minutes of waiting the server logs it, a
  capacity to raise. A cancel takes the run's waiting scans out of the queue at once and ends the run
  `interrupted`; a scan already running ends its step as before. A scan's 15 minutes are its running
  time alone — from the call the executor accepts, a pod waiting for a node included (the cluster's
  capacity, not Canyon's queue), never while it waits its turn; a scan a server resumes after a restart — its own,
  or a stopped server's — asks again and starts its 15 minutes afresh. The first scan with trivy or grype on a host waits so while the executor
  downloads the tool's vulnerability database.
- **The org's rules repo.** When `sarif.opengrep`'s settings name `rules: {repo, commit, paths?}`,
  the rules part carries, beside those vendored files, every rules file of that commit under `org/`
  at its path in the repo — and, when `paths` names directories of the repo, only those whose path
  is one of them or begins with one and `/`, so a repo's non-rules YAML (statistics, CI) is left out
  and Opengrep is never handed a file it cannot load (`rules_error`): a tree entry of mode `100644` or `100755` (a regular file) whose name ends
  `.yml` or `.yaml` and whose path has no segment starting with `.`. Nothing else in the repo is
  read: a symlink (`120000`) or submodule (`160000`) entry is never followed or fetched, and a file
  is its blob as stored, so a Git LFS pointer is the pointer, never the object it names.
  - **The fetch** is in process, over connections the server dials itself — never the git CLI,
    whose name resolution and proxy environment the server cannot pin: git's smart HTTP,
    `https://` only, depth 1, that one commit, no tags, no redirect followed, no proxy.
  - **The address.** The server resolves the repo's host itself and refuses it when any address
    the name resolves to is not public: loopback (`127.0.0.0/8`, `::1`), private (`10.0.0.0/8`,
    `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7`), link-local (`169.254.0.0/16`,
    `fe80::/10`), this network (`0.0.0.0/8`, `::`), broadcast (`255.255.255.255`) or multicast
    (`224.0.0.0/4`, `ff00::/8`) — so the cloud metadata addresses, `169.254.169.254` and
    `fd00:ec2::254`, by their class. An IPv6 address carrying an IPv4 one is judged as that IPv4
    address: IPv4-mapped `::ffff:0:0/96` and NAT64 `64:ff9b::/96` by their last 32 bits, 6to4
    `2002::/16` by bits 16–47; Teredo `2001::/32` is refused. It then connects to an address it
    checked and never resolves again.
  - **TLS.** The handshake on that connection sends the repo's host name (SNI) and verifies the
    certificate against that name under the system's roots, never the address; a certificate
    that does not verify fails the fetch, and nothing skips the check.
  - **Caps.** 60 seconds, and 50 MiB: what is received and what is unpacked from it each stop at
    50 MiB. The rules part the server builds — the vendored files, the org's and the tar's own
    headers — is checked against the executor's 50 MiB before it is sent, so the executor is never
    sent a part it would refuse: over it is `it is over 50 MiB` too.
  - **Kept by `(repo, commit)`.** The fetched commit's id is checked equal to `commit` before its
    files are kept; a commit's files never change, so a run with a kept commit fetches nothing.
  - **A private repo** is read with the credential an admin set for it
    (`PUT /api/v1/scanners/opengrep/credential`), sent as HTTP Basic on that connection alone, and
    only when the settings' `repo` equals the credential's: both lower-cased in scheme and host, the
    port `:443` dropped, and a trailing `/` then a trailing `.git` dropped from the path.
  - **Rule ids.** Opengrep runs with `--no-rewrite-rule-ids`, and its SARIF keeps one rule entry per
    id. An org rule with the `id` of a vendored rule, or of Canyon's own, would take that rule's tags
    and severity, and read as its finding. So a commit holding one is refused before the scan, naming
    the first such id in file-name order. A file that is not YAML declares no id here: opengrep names
    it as a rule that could not load (*In a run*, above).
  - **A fetch that fails** is opengrep's could-not-check (below), its reason `its rules repo could
    not be fetched: <why>`, `<why>` one of `its host is not a public address`, `it took over 60
    seconds`, `it is over 50 MiB`, `it has no .yml or .yaml file`, `it has no .yml or .yaml file under its paths`, `its repository was not found`
    (the remote answered 404), `it asks for a credential, and none is set for it` (401 or 403 with no
    credential), `it refused the credential set for it` (401 or 403 with one), `it answered with a
    redirect, which Canyon never follows`, `its certificate does not verify`, `that commit is not in
    it` (another commit answered in its place), `git could not fetch that commit; the server's log has
    git's own error` (anything else, a commit the remote does not have among them: the server logs
    git's own error, never the URL or the credential), or
    `its rule <id> has the id of one of Canyon's rules` (an admin is
    told which). No reason quotes the remote's answer; the one id named is a bundled rule's.
- **A SARIF answer attaches through the same attach as an upload** (above): the same normalisation, drops and refusals, the same stored row, `producer.sarif` row and
  verdict line. The actor is Canyon,
  `{kind: "canyon", subject: "scan", display: "Canyon"}`, so the note reads
  `trivy · report attached by Canyon` and the row `✓ trivy  clean`, naming nobody: a tool Canyon ran is one more row of the review. The
  tool is the one scanned, never read from `tool.driver.name`. Canyon's scans do not pass through the
  route, so the per-actor rate limit, which is for people and agents attaching there, does not count them.
  **It writes no audit row**: the run's `verdict` audit row records the review, and the run's timeline
  holds each report (a row per tool per run was half the trail and
  buried the rows people wrote).
- **One report per `(package, tool, category)`, whoever attached it**: the newest attach wins, a
  scan replacing an upload or an upload a scan. An upload's `attach report` row's `replaced` is the
  digest it replaced, for a scan the digest on that scan's run's `producer.sarif` row; a scan writes no
  audit row, so a scan replacing an upload shows as its run's `producer.sarif` row, `attached_by`
  Canyon, after the upload's row. Canyon
  does not make a tool either uploaded or run. So after a run, an upload may replace Canyon's own
  scan for the next run; while `run` stays on, that run scans again and its scan replaces the upload.
  If an admin turns `run` off with the rule still blocking, the uploaded report is what later runs
  read: detectable on the audit trail (the upload's `attach report` row), not prevented.
- **No severity floor per tool.** An attached result blocks as every finding does,
  `state == blocking && severity ≥ high`, a Dockerfile's never.
- **A scan that did not finish is one blocking `gate.unreadable` finding**, never a clean line: an
  answer of `not_finished` other than `no_room` (below; `no_image` too: the tool is on, by default or by an admin), no answer within
  15 minutes of the scan running (it had failed the run as Canyon's own
  error), or SARIF the attach refuses — never a scan still waiting for its turn (a 180th busy
  answer was one). It sits in the `canyon/scanners` run, at the
  package's first file in path order, line 1, columns 1–2; its snippet reads
  `(<tool> could not check this tree; tree sha256:<64 hex>)`, the digest over the tool's name,
  framed as a path is, then every file of the package, framed as above, so an approval covers that
  tool on that tree only. Its
  `title` reads exactly `Canyon could not check this package with <tool>`, and its
  `problem` reads exactly `could not check: <tool>: <reason>`, the reason `did not finish in time` (timeout, or no answer in time),
  `this Canyon host has no image for it` (no_image), `stopped before it finished` (killed),
  `did not answer SARIF` (not_sarif), `answered more than
  Canyon reads` (over_cap), `its vulnerability database could not be downloaded` (db_missing), `its vulnerability
  database is over 5 days old` (db_stale), `could not load all of its rules` (rules_error), the rules repo's fetch reason (above), or the attach's refusal message. Only a person's approval
  with a reason gets past it. Its `fix` reads `Review the package again once <tool> can run, or have
  <approvers> approve it with a reason.`, `<approvers>` reading `an admin or a security responder who
  didn't start this review`. Nothing is attached, so the package keeps the report
  it had. These reasons are a package scan's: the image and sbom targets of `/scan`,
  whose `not_finished` can carry `reason: "java"` or `over_cap`'s `image`/`report`, answer
  `platform.known-vuln`, whose texts are its own (*System libraries and build tools*, below) and do not reuse these.
- **A scan the cluster had no room to run is no finding: the run ends `failed`, `scan_no_room`**
  (as a blocking `gate.unreadable` it read as a policy block and
  offered an approval of a review that never ran). When any tool's scan answers `not_finished`
  `no_room`, the run waits for its other scans, then ends `failed` with `failure_reason` `scan_no_room`
  before the review: nothing is judged, so there is no verdict, no finding and nothing to approve. A
  tool the cluster had no room for attaches nothing, so the package keeps the report it had of that
  tool, and a tool whose scan finished attaches its report as any scan does. It is capacity, never the package's fault, a policy, nor
  Canyon failing, and the same deploy goes through once the cluster has room. Each such tool
  writes one `build.log` row before the run ends, in tool-name order, reading exactly `<tool>: the
  cluster had no room to run it (<the scheduler's message>)`; the scheduler's message is quoted with
  every control or format character dropped, runs of spaces made one, and at most 500 characters, and
  a message that is empty once cleaned reads `<tool>: the cluster had no room to run it`.
- **Evidence.** A scan's stored report also holds the image digest that ran and, for trivy and
  grype, `db_built_at`, beside the digest, tool and actor every report holds.

### System libraries and build tools: `platform.known-vuln`

A finding in the image an app runs, composed by Canyon from trivy's re-match of the image's SBOM. No review writes one, because the gate never builds. The image read and the promote receipt read them. Each follows the profile above, in a `canyon/promote-gate` run, with these differences:

- **Whose layer.** Each layer of the image is owned as the image's `io.buildpacks.lifecycle.metadata`
  names it:
  - `run-image`: every layer from the bottom up to the run image's `topLayer`;
  - `lifecycle`: the `launcher` and `config` layers;
  - `app`: the `app` layers, and the `sbom` layer: the executor's image scan skips the SBOM documents
    the lifecycle writes there, so anything listed in it the build put there, as an app's own buildpack
    can;
  - a buildpack's `key`: each of that buildpack's `layers`.

  A layer the label names under two owners, or does not name, has none, and its packages are `unknown`.
  When no layer can be owned at all, the read is `could not check`,
  never every package `unknown`. That covers four cases:
  - the image has no label;
  - the label does not parse;
  - its `runImage.topLayer` is not one of the image's diff IDs;
  - no layer up to that top layer is owned `run-image`.
- **Class** is f(owner, package type, purl): the type is trivy's `aquasecurity:trivy:PkgType`, the purl
  parsed by the purl spec. The first rule that holds is the class:
  - `lifecycle` when the owner is `lifecycle`;
  - for a `deb` purl, `os` when the owner is `run-image`, and `unknown` otherwise;
  - `runtime` when the owner is a runtime provider: `paketo-buildpacks/node-engine`, `cpython`,
    `bellsoft-liberica`, `go-dist`, `ca-certificates` or `spring-boot`;
  - `runtime` for a `gobinary` package in an installer's layer (below): the installer's own binary;
  - `runtime` for Go's standard library, `pkg:golang/stdlib`, when the owner is `app` or
    `paketo-buildpacks/go-build`: the app's own Go build, whose standard library is go-dist's. Anywhere
    else Go's standard library takes its binary's layer's class, as the rest of that binary does: a newer
    build of that binary fixes it;
  - `app` when the owner is `app`, or one of the installers: `paketo-buildpacks/npm-install`,
    `yarn-install`, `pnpm-install`, `pip-install`, `pipenv-install`, `poetry-install`,
    `conda-env-update`, `bundle-install`, `composer-install`, `go-mod-vendor`, `maven` or `gradle`;
  - `unknown` for everything else, and for a package trivy names no layer of the image for.

  A buildpack of another namespace with the same name is `unknown`, and so is an operator's own.
- **`IMAGE`.** `artifactLocation` is `{uri, uriBaseId: "IMAGE"}`, and its `uri` is a path inside the
  image. For a `deb` package it is `var/lib/dpkg/status`, the database dpkg keeps them in. For any other
  component it is trivy's `aquasecurity:trivy:FilePath`, or `.` when trivy names none. Only a
  `platform.known-vuln` finding carries `uriBaseId: "IMAGE"`, and a `platform.known-vuln` finding always
  carries it. A finding where one holds without the other fails closed.
- **Region and snippet.** The region is line 1, from column 1 to one past the snippet's last code point.
  It spans the snippet, as every region does, since an end column is exclusive. Its snippet is
  `<name> <version>`, written by Canyon.
  `<name>` is the component's CycloneDX `name`, with its `group` and a `/` before it when it has one,
  and `<version>` is its `version`.
- **Fingerprint.** `canyon/v1` is `"f_" + lowercase_hex(sha256(frame(ruleId) + frame(vulnerability) +
  frame(name) + frame(version) + frame(class)))`, framed as above. No path, layer or digest goes into
  it, so an approval covers that vulnerability in that version of that package across rebuilds.
- **Severity.** trivy's `Severity`, lowercased. `UNKNOWN`, or any value outside the four, reads `high`:
  a severity Canyon cannot read is not low.
- **Texts.** `title` is `<vulnerability> in <name> <version>`, cut to 120 code points. `problem` is
  `<vulnerability> is published against <name> <version>, which Canyon put in this app's <where>.
  <fixed> fixes it.`, with `<where>` `system libraries` for the `os` class and `build tools` for `runtime`
  and `lifecycle`, never the class's own name, `<fixed>` as trivy's `FixedVersion` wrote it, and `fix it`
  for `fixes it` when that names more than one version, `, ` between them. `fix` is the class's:
  - `os`: `Canyon fixes this: it updates <name> to <fixed> under the app. The app is not rebuilt.`
  - `runtime`: `Canyon fixes this in its next build tools update, with a newer <buildpack>.`, where
    `<buildpack>` is the id of the buildpack whose layer holds the component. A Go `stdlib` of the app's
    own Go build is `paketo-buildpacks/go-dist`'s.
  - `lifecycle`: `Canyon fixes this in its next build tools update, with a newer app launcher.`

  On a finding that blocks, `required_end_state` is `The image preview runs holds <name> at a version
  that fixes <vulnerability>.`, and `do_not` is `["change the app's code or dependencies for it"]`.
- **Its dependency** comes from the report's own fields, never from prose. `name` and `version` are
  the component's. `vulnerability` is `VulnerabilityID`, and `fixed` is `FixedVersion`. `link` is
  `PrimaryURL`, by the link rule of *A dependency's name and version*. `layer` is the diff ID of the
  layer that holds the component.
- **What counts, and what is a note.** A vulnerability is a finding only when its component's class
  is `os`, `runtime` or `lifecycle` and trivy names a `FixedVersion`. Everything else is a **note**
  `{name, version, layer, class, why}`, never a finding and never clean:
  - `no_fix`: the class is one of those three, and no fix is published;
  - `unknown`: the class is `unknown`, so Canyon can't tell what put the package there;
  - `unplaced`: an imported tool's result Canyon can't place in the image (*An imported image result*);
  - `not_checked`: the re-match never read the component. That covers a package without a purl, a
    package the answer lists in `unscanned`, and a package trivy names no
    layer for. A package is a `library` component. An `application` or `operating-system` component
    holds packages and is not one.

  A component with several such vulnerabilities is one note per `why`. Matched app-class components
  are neither findings nor notes of this rule: `deps.known-vuln` reads them at review, and counts before
  production each one its review did not (*The built app*, below), as it counts an `unknown` one, which
  stays this rule's note. A re-match covers a layer when the layer holds a package of class `os`,
  `runtime`, `lifecycle` or `unknown`, and, for a project that places `deps.known-vuln` before
  production, a layer holding an `app` package too.
- **Stored by content, read as the project's own**. An image scan is the
  project's: stored under the project of the run that deployed the image and the image's digest, and read
  by that project alone; two projects running one digest each have their own scan, label and layers. An
  image's layer is read from what that scan's own SBOM listed in it: the slice the re-match sends, the layer's components and
  the image's operating-system component, stored under its sha256, and named per diff ID by the image.
  A layer's match is stored under that slice's sha256 and read by every image naming the slice. Nothing
  is read by diff ID alone: another image holding the same layer may list other packages in it, since
  trivy reads a layer's packages through the layers above it, and its scan never changes this one's read.
  The run image Canyon builds onto now is scanned too, by the same image scan with no app, stored under
  its own digest and no project's, its slices shared by content and matched as an app's are. Only the
  patch plan reads it, to tell a run image that carries a fix from one that does not: no finding, note
  or receipt row is made of it.
- **The join.** A vulnerability is joined to its component by `PkgIdentifier.BOMRef` within the layer
  its match is stored under, never by name and version. Its `Layer.DiffID` must be that layer, and so
  must the component's `aquasecurity:trivy:LayerDiffID`. A vulnerability that joins no component that
  way makes the whole match unreadable: the rule reads `could not check`, never clean.
- **What is stored must read whole.** Each of these makes the read `could not check`, never clean:
  - a stored component whose `aquasecurity:trivy:LayerDiffID` is missing or is not the layer it is stored
    under;
  - a stored list of components that is not a list, `null` included, or any element of it that is `null`
    or lacks a `bom-ref`, a `type` or a `name`;
  - a stored report that is not trivy's: `SchemaVersion` 2 and `Results` a list;
  - in a report, a result or a vulnerability that is `null`, or a vulnerability without its
    `VulnerabilityID`, `PkgIdentifier.BOMRef` or `Layer.DiffID`;
  - an image's diff IDs holding one that is not `sha256:` and 64 lowercase hex, or one named twice;
  - a match whose database was built after the read.

### An imported image result

**Added 2026-11-03.** An org that scans images with its own tool (Wiz, Prisma Cloud, Snyk, Amazon Inspector)
attaches that tool's report of an image (`PUT /api/v1/projects/:id/images/:digest/reports`), and an admin adds the
tool as `platform.known-vuln`'s `settings.source`. **The tool adds to Canyon's own scan and never replaces it**: the rule
counts what Canyon's matches found and what the tool's newest report of the image in each of its categories found
together, a finding both name — the same package, version and vulnerability — once, so a report that finds nothing,
or names another image's packages, hides nothing Canyon found. SARIF names no package, fixed version or layer, and Canyon
reads no tool's prose for one, so each result names its package in its properties, which the org's export maps its
own fields to:

```jsonc
"properties": { "canyon": { "package": {
  "name": "libssl3",                       // 1 to 214 characters
  "version": "3.0.13-0ubuntu3.4",          // the installed version, 1 to 128
  "fixed": "3.0.13-0ubuntu3.5",            // the version that fixes it; absent when none is published
  "layer": "sha256:…",                     // the diff ID of the image's layer that holds it
  "purl": "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04" // its package URL
} } }
```

A worked example: one vulnerability of a Trivy JSON image report (`trivy image --format json`),

```jsonc
{ "VulnerabilityID": "CVE-2024-5535", "PkgName": "libssl3", "InstalledVersion": "3.0.13-0ubuntu3.4",
  "FixedVersion": "3.0.13-0ubuntu3.5", "Severity": "HIGH", "Layer": { "DiffID": "sha256:…" },
  "PkgIdentifier": { "PURL": "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04" } }
```

is the result `{"ruleId": "CVE-2024-5535", "level": "error", "message": {"text": "CVE-2024-5535 in libssl3"},
"properties": {"canyon": {"package": {"name": "libssl3", "version": "3.0.13-0ubuntu3.4", "fixed":
"3.0.13-0ubuntu3.5", "layer": "sha256:…", "purl": "pkg:deb/…"}}}}`: `PkgName`, `InstalledVersion`, `FixedVersion`,
`Layer.DiffID` and `PkgIdentifier.PURL` mapped one for one, the severity its rule's `security-severity` or the level.
A tool whose export names no layer maps none, and each of its results is a note Canyon can't place: a per-vendor
adapter waits for an org to ask.

Each string is plain text with no live key, the purl at most 2,048 characters, the layer `sha256:` and 64 hex. The purl
is optional, but without one a result in the layers of Canyon's system libraries is a note, `unknown`: a package's class
there is read from its purl's type (a `deb` package is a system library), as a component's is. The
result's `ruleId` is the vulnerability, its severity read as a package report's is (*In: external SARIF*). A result
of kind `pass`, `informational` or `notApplicable` is dropped; any other without `properties.canyon.package`, or with
one of these wrong, refuses the report. What is kept is each result's id, severity and package: names and versions.

**What counts, and what is a note**, against Canyon's own image scan of the same image, which places each layer
(*Canyon's layers*): a result counts when its `layer` is one of the image's, its class there — the layer's owner
and the purl, as a component's is — is `os`, `runtime` or `lifecycle`, and it names `fixed`. Else it is a **note**
`{name, version, layer, class, why}`, never a finding and never clean: `no_fix`, the class is one of the three and
no fix is named; `unknown`, the class is `unknown`; **`unplaced`**, it names no `layer` or one the image does not
have, so Canyon can't tell which part of the image it is in (`layer` `null`, `class` `unknown`). A result in the
app's own layers is neither, as a match's is. Its finding is a match's (*Canyon's layers*), its region's snippet
`<name> <version>` and its `dependency` the package.

### The built app: `deps.known-vuln` before production

A dependency only the image holds — a build step's download, a Maven app's inherited versions the offline
review scan could not resolve, a package of a buildpack outside Canyon's builder — is in no lockfile the
review read. While `deps.known-vuln` is placed before production, its row on the promote receipt also
reads the image the receipt is for, from the same stored scan and
matches as *System libraries and build tools*, and counts each such package. No finding is written of it: no run holds one,
so an approval names it by its fingerprints alone (below).

- **Which packages.** Each `library` component of the image whose class (*System libraries and build tools*, Class) is
  `app` or `unknown`. Its layers are those holding such a package; each must have its newest match read
  as *System libraries and build tools* reads one — present, within the 5 days, built before the read, joined by `BOMRef`
  within its layer — and the image's own scan must read whole as it does there.
- **What counts.** A package with a vulnerability, in its layer's newest match, at `high` or `critical`
  — trivy's `Severity` lowercased, any value outside the four read as `high` — whether or not trivy
  names a `FixedVersion`. Each package is counted once, by its name and version, its name as *Region and
  snippet* writes it.
- **What the review already reported.** A package is not counted when a finding of the run the row reads,
  under `deps.known-vuln` (its `ruleId` that id or under it), names the same package: the same purl type,
  name and version. The finding's name and version are its dependency's (*A dependency's name and
  version*); its purl type is its file's, by the file's name alone — `package-lock.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `bun.lock` and `package.json` `npm`; `requirements.txt`, `Pipfile.lock`,
  `poetry.lock`, `uv.lock` and `pyproject.toml` `pypi`; `pom.xml`, `gradle.lockfile` and a name ending
  `.jar`, `.war` or `.ear` `maven`; `go.mod` and `go.sum` `golang`; `Gemfile.lock` `gem`; `composer.lock`
  `composer`; `Cargo.lock` `cargo`; `packages.lock.json` `nuget` — and a file named otherwise names no
  type, so its finding stands for no package of the image and a package is counted rather than dropped.
  A component's type and name are its purl's, parsed by the purl spec, and its version its `version`: a `maven` name is its namespace, `:` and its
  name, as trivy names a Java package; any other is its namespace, `/` and its name, or its name alone.
  A `pypi` name compares as its spec normalises it, lowercase with each run of `-`, `_` and `.` one `-`.
- **Not checked.** A package of those classes the match never read — without a purl, or in the answer's
  `unscanned` — is not checked: never counted, never clean, and the row's `why` names how many. So is a
  `library` component trivy names no layer of the image for, which is `unknown` and in no layer's match.
- **What the review reported, not what a lockfile names.** The review keeps its findings, not the packages
  its lockfiles name, so a package a lockfile names that had no advisory at its review and has one now is
  counted here too, until a review reports it.
- **Fingerprint.** Each advisory at `high` or above of a counted package has a `canyon/v1` for each class
  the package is held in, as *System libraries and build tools* derives one: `"f_" +
  lowercase_hex(sha256(frame("deps.known-vuln") + frame(vulnerability) + frame(name) + frame(version) +
  frame(class)))`, `<name>` as *Region and snippet* writes it and `<class>` `app` or `unknown`. No path,
  layer or digest goes into it, so an approval covers that vulnerability in that version of that package
  across rebuilds; a package whose class moves has another, and counts again until it is approved again (fail
  closed). Nor does its ecosystem: a package of one name, version and class that two ecosystems hold, with the
  same vulnerability, has one fingerprint, and one approval covers both.
- **Approved.** A package is not counted while approvals cover every fingerprint of it, wherever the
  promote receipt reads it: a package is approved whole, never by one advisory of several.
- **A re-match of its own.** The app layers a project matches for this row alone — those holding no package
  of a platform class — are matched apart from *System libraries and build tools*: one that does not match marks only this
  row's read, `its last rescan did not finish`, never the platform rule's read or the patch plan.

## Out: SARIF for any CI

A gate run serializes as one SARIF log, one `run` per producer (`tool.driver.name` =
the producer's driver name above). The log is served at `GET /api/v1/gate-runs/:id/sarif` and printed by `canyon check --format sarif`, for GitHub code scanning, GitLab or
Azure DevOps to upload — the profile adds nothing those reject and removes nothing they need.
An approval travels as a SARIF suppression, `{kind: "external", justification}`, carrying the
approval's reason only; once uploaded, a reason is third-party data. The GitHub App uploading to
code scanning itself is not built (it needs `security-events: write`).
