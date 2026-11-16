---
title: Rules
description: The rules Canyon ships, what finds each one, and whether it blocks out of the box.
---

Every finding names one of these rules. An admin sets each one to Off, Advisory or Blocking for the
org under [Guardrails](/org/guardrails/), and can add custom rules:

- **Blocking:** the rule stops a deploy for its high and critical findings. Checkov's Dockerfile
  checks never block, since Canyon never builds from a Dockerfile. The go-live checks, marked "when
  you promote" below, stop a promotion to production instead.
- **Advisory:** the rule's findings are shown, and stop nothing.
- **Off:** the rule isn't checked.

"Found by" names the tool that reports the rule: a scanner Canyon runs (gitleaks, Trivy, Opengrep,
ESLint, Checkov, and Grype once an admin adds it), Canyon's AI review, which needs an AI model set up on your Canyon, or Canyon
itself.

## Blocking by default

- [No secrets in the code](/rules/secrets.client-bundle/) · `secrets.client-bundle` · found by gitleaks
- [No hand-built SQL from user input](/rules/sql.string-concat/) · `sql.string-concat` · found by Opengrep, with GitLab's sast-rules and Canyon's own rule
- [Production needs a passing review](/rules/prod.clean-gate/) · `prod.clean-gate` · found by Canyon, when you promote
- [A person promotes](/rules/prod.human-approval/) · `prod.human-approval` · found by Canyon, when you promote

## Advisory by default

- [Every route needs sign-in](/rules/auth.unauthenticated-route/) · `auth.unauthenticated-route` · found by the AI review, when a model is set
- [No known-vulnerable dependencies](/rules/deps.known-vuln/) · `deps.known-vuln` · found by Trivy
- [Approved stacks only](/rules/stack.blessed/) · `stack.blessed` · found by Canyon, when you promote
- [Every app has an owner](/rules/owner.required/) · `owner.required` · found by Canyon, when you promote
- [No unbounded refetch on user input](/rules/perf.unbounded-refetch/) · `perf.unbounded-refetch` · found by the AI review, when a model is set
- [Apps use the org's design system](/rules/design.org-system/) · `design.org-system` · found by Canyon, from the app's package.json
- [No user input reaching commands, files, pages or URLs](/rules/code.injection/) · `code.injection` · found by Opengrep and ESLint
- [Infrastructure config passes its benchmark checks](/rules/iac.misconfig/) · `iac.misconfig` · found by Checkov and Trivy
- [No hand-built SQL from user input (AI review)](/rules/ai.sql-string-concat/) · `ai.sql-string-concat` · found by the AI review, when a model is set
- [No user input reaching commands, files, pages or URLs (AI review)](/rules/ai.code-injection/) · `ai.code-injection` · found by the AI review, when a model is set

## Off by default

- [No known-vulnerable system libraries or build tools](/rules/platform.known-vuln/) · `platform.known-vuln` · found by Canyon, when you promote
- [Production allows everything preview connects to](/rules/prod.egress-allowed/) · `prod.egress-allowed` · found by Canyon, when you promote

## Everything else a scanner reports

These catch a tool's findings that don't belong to a named rule above, one rule per tool. Each one
also decides whether its tool runs at all: set to Off, the tool doesn't run. They start as
Advisory, except Grype's, which isn't there until an admin adds it.

- [Findings from checkov](/rules/sarif.checkov/) · `sarif.checkov` · found by Checkov
- [Findings from eslint](/rules/sarif.eslint/) · `sarif.eslint` · found by ESLint
- [Findings from gitleaks](/rules/sarif.gitleaks/) · `sarif.gitleaks` · found by gitleaks
- [Findings from grype](/rules/sarif.grype/) · `sarif.grype` · found by Grype, once an admin adds it
- [Findings from opengrep](/rules/sarif.opengrep/) · `sarif.opengrep` · found by Opengrep
- [Findings from trivy](/rules/sarif.trivy/) · `sarif.trivy` · found by Trivy
