---
slug: rules/sarif.opengrep
title: "Findings from opengrep"
description: "Opengrep's results that no Canyon rule maps, and the switch that decides whether Opengrep runs."
---

`sarif.opengrep` · Advisory by default · found by Opengrep

## What it finds

Opengrep's own findings, where no other Canyon rule takes them. Canyon runs Opengrep with GitLab's
sast-rules, open-source rules that ship with Canyon, and Canyon's own SQL rule. An admin can add the org's own rules
repository under Guardrails → Scanners → Opengrep → Configure. Canyon sorts each result by the CWE
tags of the rule behind it (CWE is the standard catalogue of weakness types):

- SQL injection (CWE-89) goes to [No hand-built SQL from user input](/rules/sql.string-concat/)
  (`sql.string-concat`).
- Command, code and path injection, cross-site scripting, open redirect, server-side request forgery,
  XML external entities and unsafe deserialization go to
  [No user input reaching commands, files, pages or URLs](/rules/code.injection/) (`code.injection`).
- Anything else stays here, under the Opengrep rule's own id.

For example, a Go server that starts with `http.ListenAndServe` and no timeouts:

```go
_ = http.ListenAndServe(":"+port, nil)
```

GitLab's rule `go_http_rule-http-serve` reports it, tagged CWE-770, which no Canyon rule takes. It
lands here as `sarif.opengrep.go_http_rule-http-serve`, at low severity.

This rule's state also decides whether Opengrep runs at all: Canyon runs it only while this rule is
Advisory or Blocking, and while **Canyon runs Opengrep** is ticked under Configure.

## Why it matters

Each finding states its risk in the Opengrep rule's own words. For the Go server above: clients that
open connections and never finish their requests can leave it accepting no new connections.

## How to fix it

Canyon prints the tool's own fix when the rule has one. When it has none, it prints, for the example above:
"Change this code so opengrep no longer reports go_http_rule-http-serve here; the line above says what it
found." The line above is the finding's problem, which carries the rule's description.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon doesn't read `nosemgrep` comments or a `.semgrepignore` file, so neither silences a finding.
An admin can list paths to skip under Configure.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Blocking, or to Off under Guardrails → Scanners, which also stops Opengrep: then nothing
checks `sql.string-concat`, and the review says so.

If Opengrep can't finish, the review carries one blocking finding: "Canyon could not check this
package with opengrep".
