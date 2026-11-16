package policy

import "strings"

// canonical is the twenty-one rules of EXPERIENCE D2 — id, English name, demo default, producer, and
// run on the four tools Canyon runs through its executor by default — byte-exact and in the
// fixture's order. contracts/validate.py holds the fixture to D2 and
// TestTheCanonicalRulesAndTheDefaultsAreTheFixture holds this table to the fixture. Every other rule
// joins through Register and installs Off: sarif.grype among them, the one executor tool an admin
// adds (contracts/finding.md, Canyon-run tools), so its results stay under its own rule.
var canonical = []Rule{
	{ID: "secrets.client-bundle", Name: "No secrets in the code", State: Blocking, Producer: Scanner},
	{ID: "sql.string-concat", Name: "No hand-built SQL from user input", State: Blocking, Producer: Scanner},
	{ID: "prod.clean-gate", Name: "Production needs a passing review", State: Blocking, Producer: PromoteGate},
	{ID: "prod.human-approval", Name: "A person promotes", State: Blocking, Producer: PromoteGate},
	{ID: "auth.unauthenticated-route", Name: "Every route needs sign-in", State: Advisory, Producer: LLMReview},
	{ID: "deps.known-vuln", Name: "No known-vulnerable dependencies", State: Advisory, Producer: Scanner},
	{ID: "stack.blessed", Name: "Approved stacks only", State: Advisory, Producer: ServerState},
	{ID: "owner.required", Name: "Every app has an owner", State: Advisory, Producer: ServerState},
	{ID: "perf.unbounded-refetch", Name: "No unbounded refetch on user input", State: Advisory, Producer: LLMReview},
	{ID: "design.org-system", Name: "Apps use the org's design system", State: Advisory, Producer: Scanner},
	{ID: "code.injection", Name: "No user input reaching commands, files, pages or URLs", State: Advisory, Producer: Scanner},
	{ID: "iac.misconfig", Name: "Infrastructure config passes its benchmark checks", State: Advisory, Producer: Scanner},
	{ID: "ai.sql-string-concat", Name: "No hand-built SQL from user input (AI review)", State: Advisory, Producer: LLMReview},
	{ID: "ai.code-injection", Name: "No user input reaching commands, files, pages or URLs (AI review)", State: Advisory, Producer: LLMReview},
	{ID: "sarif.checkov", Name: "Findings from checkov", State: Advisory, Producer: Sarif, Run: true},
	{ID: "sarif.eslint", Name: "Findings from eslint", State: Advisory, Producer: Sarif, Run: true},
	{ID: "sarif.gitleaks", Name: "Findings from gitleaks", State: Advisory, Producer: Sarif},
	{ID: "sarif.opengrep", Name: "Findings from opengrep", State: Advisory, Producer: Sarif, Run: true},
	{ID: "sarif.trivy", Name: "Findings from trivy", State: Advisory, Producer: Sarif, Run: true},
	{ID: "platform.known-vuln", Name: "No known-vulnerable system libraries or build tools", State: Off, Producer: PromoteGate}, // installs off (contracts/snapshot.md)
	{ID: "prod.egress-allowed", Name: "Production allows everything preview connects to", State: Off, Producer: ServerState},    // installs off (contracts/snapshot.md)
}

// Canonical is the canonical rule a finding's rule id is under, itself or the one it extends
// (iac.misconfig.trivy.DS026 is iac.misconfig's): a built-in rule, which telemetry may name (#2807). False
// for every other id, a custom rule's among them.
func Canonical(ruleID string) (string, bool) {
	for _, r := range canonical {
		if ruleID == r.ID || strings.HasPrefix(ruleID, r.ID+".") {
			return r.ID, true
		}
	}
	return "", false
}

// defaults is everything but the rules, as the fixture sets it. A fresh value every call, so
// no snapshot shares a slice with another.
func defaults() Snapshot {
	return Snapshot{
		Exclusions:    []string{".env*", "!.env.example", "node_modules", ".git", "/dist", "/build", "/.next", ".venv"},
		Agents:        []Agent{{ID: "canyon-review", Namespace: []string{"perf", "auth", "ai"}, Model: "resolved-model-id"}},
		ComponentMenu: []string{"postgres", "s3"},
		Stacks:        []Stack{}, // empty, never nil: a nil slice marshals as null, which the schema refuses
		Limits:        Limits{GateRunsPerProjectPerHour: 30, GateRunsPerActorPerHour: 60},
		Capabilities:  map[string]Capability{"design_systems": {Enabled: true}},
	}
}

// howToPass is one line per Canyon rule on how to pass it, taken from the "How to fix it" of the rule's
// docs page (docs/src/content/docs/rules/<id>.md). GET /skill/manifest hands it under each Canyon rule
// that blocks, so an agent knows how to meet a rule before a deploy blocks on it, and the dashboard's
// "What agents are told" shows the same words (contracts/routes.md; #2622). TestEveryRuleSaysHowToPassIt
// holds it to the rules: a rule can't ship without its line.
var howToPass = map[string]string{
	"secrets.client-bundle":      "Read each secret from an environment variable and store it with canyon secret set NAME.",
	"sql.string-concat":          "Pass the user's input to the query as a bound parameter; never build SQL text from it.",
	"prod.clean-gate":            "Fix every finding that blocks, then deploy to preview again: production needs a passing review.",
	"prod.human-approval":        "Promote to production only when the user asks for it.",
	"auth.unauthenticated-route": "On every route that isn't public, verify the Authorization: Bearer token Canyon sends; don't add a login.",
	"deps.known-vuln":            "Upgrade each flagged package to a version its advisory lists as fixed, then update the lockfile.",
	"stack.blessed":              "Build with a language your org approved (listed below); an admin approves any other.",
	"owner.required":             "Ask the user to claim the app in Canyon's dashboard, or to ask an admin to give it an owner.",
	"perf.unbounded-refetch":     "Debounce requests driven by typing, or filter on the server instead of refetching on every change.",
	"design.org-system":          "Build the UI from the org's design system below: add its package and use its components.",
	"code.injection":             "Pass user input as an argument or parameter, never inside a command, path, page, URL or code.",
	"iac.misconfig":              "Set each setting a check flags in the infrastructure files to the value that check asks for.",
	"ai.sql-string-concat":       "Pass the user's input to the query as a bound parameter; never build SQL text from it.",
	"ai.code-injection":          "Pass user input as an argument or parameter, never inside a command, path, page, URL or code.",
	"sarif.checkov":              "Set each setting a check flags in the infrastructure files to the value that check asks for.",
	"sarif.eslint":               "Apply the fix Canyon prints for each line eslint flags.",
	"sarif.gitleaks":             "Revoke any real credential gitleaks finds, make a new one, and read it from the environment.",
	"sarif.grype":                "Upgrade each flagged package to a version its advisory lists as fixed, then update the lockfile.",
	"sarif.opengrep":             "Apply the fix Canyon prints for each line opengrep flags.",
	"sarif.trivy":                "Apply the fix Canyon prints for each finding trivy reports.",
	"platform.known-vuln":        "Nothing to change in the app's code: the fix is a system library update from Canyon or whoever runs it.",
	"prod.egress-allowed":        "A person allows each host name in Canyon's dashboard; anything else has to change in the code.",
}

// HowToPass is the line on how to pass the Canyon rule id names, "" for a rule that has none: an
// org's own (custom.*), whose text is its admin's instructions.
func HowToPass(id string) string {
	return howToPass[id]
}
