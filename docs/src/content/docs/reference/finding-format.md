---
title: Finding format
description: Canyon's findings are SARIF 2.1.0 with a few required fields. How to read them, and how to bring your own tool's reports.
---

Every finding Canyon reports is a [SARIF 2.1.0](https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html)
result. SARIF is the standard JSON format for static analysis results. Any tool that reads SARIF can read Canyon's review, and any tool that writes SARIF can add
its findings to it. The full contract is
[Findings](/reference/contracts/finding/), and
[finding.schema.json](/contracts/schema/finding.schema.json) checks it.

## Read a review as SARIF

`canyon check --format sarif` prints the review as one SARIF log: one `run` for Canyon's scanners,
one for its AI review, and one per imported tool. The same log is served at
`GET /api/v1/gate-runs/<run>/sarif` ([with your sign-in](/reference/api/)), where `<run>` is the review's id, such as the `gr_…` id at the
end of `canyon deploy`'s output. GitHub code
scanning, GitLab and Azure DevOps accept it as it is. An approved finding carries its approval as a
SARIF suppression, `{"kind": "external", "justification": "<the reason>"}`.

On top of plain SARIF, every result has:

- `ruleId`: the Canyon rule, such as `sql.string-concat`, or `sarif.<tool>.<the tool's rule id>` for an imported tool's own rule.
- `level`: `error`, `warning` or `note`, from the severity.
- `locations`: exactly one, with a full region (start and end line and column, in code points) and the code as a snippet of at most 400 characters. A secret in it is masked, with `••••` in place of its middle.
- `partialFingerprints["canyon/v1"]`: the finding's identity across runs, `f_` and 64 hex characters. An approval matches on it.
- `properties.canyon.severity`: `critical`, `high`, `medium` or `low`.
- `properties.canyon.blocks`: whether this finding stops a deploy to preview. A rule set to Blocking stops its high and critical findings, except Checkov's Dockerfile checks. Promotion to production has rules of its own.
- `properties.canyon.title`, `problem`, `fix`: the headline, why it matters, and how to fix it. This is what the person or the coding agent reads.
- `properties.canyon.policy_name`: the rule's name, such as *No hand-built SQL from user input*.
- `properties.canyon.required_end_state`, `do_not`: what the code must do afterwards, and the workarounds that only bring the finding back. Always on a finding that blocks, and on some others.
- `properties.canyon.secret_name`: on a secret, the name to store it under with `canyon secret set`.

One result, from an example app's admin route with no sign-in check:

```json
{
  "ruleId": "auth.unauthenticated-route",
  "level": "error",
  "message": {
    "text": "/api/admin/policies is reachable without a session"
  },
  "locations": [
    {
      "physicalLocation": {
        "artifactLocation": {
          "uri": "apps/web/src/app/api/admin/policies/route.ts"
        },
        "region": {
          "startLine": 8,
          "startColumn": 3,
          "endLine": 8,
          "endColumn": 68,
          "snippet": {
            "text": "  return NextResponse.json(await db.policies.forOrg(params.orgId));"
          }
        }
      }
    }
  ],
  "relatedLocations": [],
  "partialFingerprints": {
    "canyon/v1": "f_1ddb72ad9a5d06de4d2cb9c67e3fb584437c9f58dcc7f33b4f026c8fc7484dbf"
  },
  "properties": {
    "canyon": {
      "severity": "high",
      "blocks": false,
      "title": "/api/admin/policies is reachable without a session",
      "problem": "There's no session check in the handler, and middleware.ts:22 leaves /api/admin/policies out of the matcher — so nothing upstream is checking either. Any caller without a session gets what this handler returns.",
      "fix": "Either add /api/admin/policies to the middleware matcher, or return early from the handler when the request has no cp_session cookie. The matcher is the safer of the two — it covers routes nobody's written yet.",
      "policy_name": "Every route needs sign-in",
      "required_end_state": "/api/admin/policies answers only a request that carries a session: the middleware matcher covers it, or the handler returns before anything else when the session cookie is missing.",
      "do_not": [
        "check a query parameter or header the caller sets instead of the session cookie",
        "move the handler's logic to another route outside the matcher",
        "add an ignore comment or suppression pragma"
      ]
    }
  }
}
```

## Bring your own tool's reports

Attach a tool's SARIF report to the code you're checking or deploying:

```sh
canyon check --sarif semgrep.sarif
canyon deploy --sarif semgrep.sarif
```

`--sarif` repeats, one report per file. When [Canyon's GitHub App](/build/github/) checks a push or
a pull request, it attaches GitHub code scanning's results for that commit, for each tool whose rule
is on. A report is the uploader's word that the tool ran on this code: Canyon checks that it matches
the code, not that it is complete.

- **Which tools:** gitleaks, Trivy, Opengrep, ESLint and Checkov, unless an admin turned one off.
  Any other tool once an admin has added its rule, `sarif.<tool>`, to the org's rules and chosen its
  state. A report can't create a rule.
- **Its shape:** SARIF 2.1.0 JSON, one `run`, at most 10 MiB and 1,000 results. The tool's name is
  `tool.driver.name`, lowercased with other characters turned into `-` (`Semgrep OSS` becomes
  `semgrep-oss`). A second report from the same tool, for the same code, replaces the first.
- **What Canyon trusts:** each result's location, rule id, level and message. It reads the code
  from its own copy of the upload and ignores the tool's snippets and fingerprints. A result marked
  as suppressed, or pointing outside the uploaded files, is dropped and counted; a report that
  isn't valid is refused whole, with the reason.
- **What blocks:** the five built-in tools' rules start as Advisory. An admin decides whether each
  one blocks, the same as for Canyon's own rules ([Set guardrails](/org/guardrails/)).
