---
title: "When a review blocks you: fix or approve"
description: What a blocked deploy prints, how to fix the code and check it again, and how a person approves a finding with a reason and an expiry.
---

When a review blocks your deploy, nothing is built. Fix the code and run `canyon check`, or, if the
code is right as it is, [have the finding approved](#approve-it-instead).

Every `canyon deploy` runs a **review** before anything is built: Canyon's scanners, and its AI
review when an AI model is set up, read the code. Each problem they report is a **finding**, and
each finding names the **rule** it breaks. Your org sets each rule to Off, Advisory or Blocking
([Set guardrails](/org/guardrails/)), and can do so separately for the two places a rule is
checked: before preview and before production. When a rule that is Blocking before preview finds a
high or critical problem, that finding is a **blocking finding**: nothing is built and nothing is
deployed. A rule that is Blocking only before production lets the version reach preview and stops
its promotion instead ([below](#approve-it-instead)). The output says what to change, where, and
which rule asked for it.

```diagram kind=flow title="A blocked deploy: fix the code, or have someone approve the finding"
steps:
  - {who: Your coding agent, icon: agent, what: "`canyon deploy`", more: The review runs before anything is built.}
  - {who: Canyon, icon: blocked, tone: blocked, what: "Blocked: nothing is built", more: "A rule that is Blocking before preview found a high or critical problem."}
  - paths:
      - label: The code is wrong
        steps:
          - {who: Your coding agent, icon: agent, what: Fixes the code, more: "`canyon check` runs the same review without deploying."}
      - label: The code is right as it is
        tone: person
        steps:
          - {who: An admin or a security responder, icon: person, tone: person, what: "Approves the finding, with a reason", more: "And an expiry, 30 days unless they change it, at most 89. Never on a deploy they started."}
  - {who: Your coding agent, icon: agent, what: "`canyon deploy` again"}
  - {who: Passed, icon: passed, tone: passed, what: Running in preview, more: Nothing blocking.}
```

## What a blocked deploy prints

```sh
canyon deploy
```

```text
  No app in canyon.toml — setting up this app.
  app: sturdy-fern-wu0 (named "claims-search" after this folder)

  ✓ Wrote canyon.toml — commit it so your team deploys the same app.

  canyon · sturdy-fern-wu0 → preview · localhost:7777

  ✓ Package    3 files · 921 B · → localhost:7777

  Review
    · trivy: waiting for a scanner slot (1st in line)

  ✗ Review blocked · 1 blocking finding
    · AI review          off (no AI model set up on this Canyon)
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✗ opengrep           1 blocking finding
    ✓ trivy              clean

  Nothing was built and nothing was deployed.

  ────────────────────────────────────────────────────────────────────────────

  critical  Improper neutralization of special elements used in an SQL command (SQL Injection)
  server.js:12:26
  found by opengrep (2 rules)

    12 │   const { rows } = await pool.query("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'");
       │                          ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

  Improper neutralization of special elements used in an SQL command (SQL
  Injection). Untrusted input concatinated with raw SQL query can result in
  SQL Injection.

  fix   Pass the user's input to the query as a bound parameter instead of
        building the SQL text from it.
  why   Policy "No hand-built SQL from user input"

  ────────────────────────────────────────────────────────────────────────────

  1 blocking finding · 0 warnings

  Fix it, then check again without deploying:
    canyon check

  Only an admin or a security responder can approve a blocking finding, and never their own.

  Open in Canyon http://localhost:7777/#/apps/sturdy-fern-wu0

  Done in 26.1s · run gr_ac0264c230e39bc3
```

- Each finding names the file and line, the scanner that found it, a `fix` line and a `why` line.
  The `why` line names the org's rule, here [No hand-built SQL from user input](/rules/sql.string-concat/).
- The command exits with code 2: Canyon held the deploy back, here because of the review. Any other
  error exits 1, so a script can tell the two apart.
- `Open in Canyon` is the app's page in the dashboard, where the same findings are listed.
- An agent reads the same findings in a form made for it. The fields are on
  [Finding format](/reference/finding-format/).

## Fix the code

1. Change the code as the `fix` line says.
2. Run `canyon check`. It runs the same review `canyon deploy` runs, and deploys nothing.
3. When it says the review found nothing blocking, run `canyon deploy` again.

Canyon's scanners don't read ignore comments or suppression pragmas in your code. Silencing a
finding that way only brings it back on the next run. A key the review found in the code goes into
`canyon secret set`, not into a `.env` file: the CLI leaves `.env` files out of what it uploads, so
the app would never see it. See [Secrets](/build/secrets/).

The Canyon skill (the instructions `canyon` installs for your coding agent) tells the agent to do
this loop itself: fix, `canyon check`, deploy. After two rounds that still end blocked, it stops and
shows you the findings and the app's page.

## Approve it instead

Sometimes the code is right and the finding is not, or the risk is accepted for now. Then an admin
or a [security responder](/org/who-can-do-what/#roles-on-canyon-as-a-whole) approves the finding,
with a reason and an expiry date, on the app's page in the dashboard. Nobody approves a blocking
finding of a deploy they started, so whoever deployed it asks one of them to:

1. Open the `Open in Canyon` link from the output.
2. On the blocking finding, click **Approve with a reason**.
3. Write the **Reason**, and pick when the **Approval expires**: 30 days out unless you change it,
   at most 89. It lasts to the end of that day, UTC.
4. Click **Approve**.

Then deploy again:

```sh
canyon deploy
```

This is the laptop demo's SQL-injection app deployed again after its finding was approved. `⋯`
marks the lines left out. The demo runs without sign-in, so the approver reads `anonymous`:

```text
  ⋯
  ! Review approved · 1 blocking finding, approved by anonymous until 25 Oct
    "Internal tool behind the VPN; the parameterised query ships next sprint"
    · AI review          off (no AI model set up on this Canyon)
    ✓ checkov            clean
    ✓ eslint             clean
    ✓ gitleaks           clean
    ✗ opengrep           1 blocking finding
    ✓ trivy              clean

  Every blocking problem is approved, so this version goes to preview.
  ⋯
  ✓ Build      [25.0s]
  ✓ Deploy     preview

  Live   http://sturdy-fern-wu0.preview.localhost:7777 · answered 200 OK at /
  Fleet  http://localhost:7777/#/apps/sturdy-fern-wu0

  Done in 52.2s · run gr_174835ef3427a860
```

- **Who can approve:** a Canyon admin or a security responder, on any app. Never on a deploy they
  started themselves, including one a coding agent started, signed in as them. The app's owners
  and contributors can't approve a blocking finding; the page says who can instead. See
  [Who can do what](/org/who-can-do-what/).
- **The CLI has no approve command.** A blocking finding is approved on the app's page, in a
  browser, and nowhere else.
- **It is on the record.** The approval carries the approver's name, the reason and the date. It
  goes into the [audit trail](/org/audit-trail/), which every admin can read, with who started the
  deploy. The finding stays on the page, marked approved until that date.
- **It covers the same finding on later deploys**, production included, until it expires. Fixing the
  finding doesn't end the approval: if the same finding comes back before that date, it's still
  covered. After that date the finding blocks again.
- **Some findings block only production.** A rule can be placed to block before production instead
  of before preview. Its finding reads **advisory before preview · blocks before production**: the
  version reaches preview, and the finding must be fixed or approved the same way before the app
  can be promoted ([Get to production](/build/production/)).
- **Fixed needs no approval.** A new version is reviewed from scratch; if its review finds nothing
  blocking, it passes. A key that leaked still needs rotating: Canyon asks for it, but can't check it.
- **The button is on the findings of the app's newest deploy.** An app that has only been checked,
  never deployed, shows its findings without it: run `canyon deploy` first.
- **One dependency, several findings:** when one scanner finds several problems in the same
  dependency, the page groups them and **Approve all _N_** approves them together.

## On a Canyon without sign-in

The laptop demo runs without sign-in. There, every request counts as a person and as an admin, so
anyone who can reach Canyon can approve a finding. Turn sign-in on before real teams use it
([Sign-in, the AI model and keys](/org/set-up/)).
