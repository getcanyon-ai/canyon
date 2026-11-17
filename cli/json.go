package cli

import (
	"bytes"
	"encoding/json"
	"net/url"
)

// jsonRun is cli.md §5.4: one object for CI, stable keys, absent rather than null.
type jsonRun struct {
	Result     string          `json:"result"`
	Reason     string          `json:"failure_reason,omitempty"` // on a failed run only, as the host answered it
	NotChecked bool            `json:"not_checked,omitempty"`    // passed, and the review names what it did not read
	App        string          `json:"app"`
	Env        string          `json:"env,omitempty"`
	GateRun    string          `json:"gate_run"`
	StartedAt  string          `json:"started_at,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Counts     jsonCounts      `json:"counts"`
	Findings   []jsonFinding   `json:"findings"`
	Notes      []producerNote  `json:"notes"`
	URLs       jsonURLs        `json:"urls"`
	Deployment *jsonDeployment `json:"deployment,omitempty"`
	ExitCode   int             `json:"exit_code"`
}

type jsonCounts struct {
	Findings int `json:"findings"`
	Blocking int `json:"blocking"`
}

type jsonFinding struct {
	Severity         string   `json:"severity"`
	Blocks           bool     `json:"blocks"`
	Rule             string   `json:"rule"`
	File             string   `json:"file"`
	Line             int      `json:"line"`
	Column           int      `json:"column"`
	Problem          string   `json:"problem"`
	Fix              string   `json:"fix"`
	PolicyName       string   `json:"policy_name"`
	SecretName       *string  `json:"secret_name,omitempty"`
	CarriedFrom      string   `json:"carried_from,omitempty"`
	RequiredEndState string   `json:"required_end_state,omitempty"`
	DoNot            []string `json:"do_not,omitempty"`
}

type jsonURLs struct {
	App string `json:"app"`
}

type jsonDeployment struct {
	URL string `json:"url"`
	Env string `json:"env"`
}

var jsonResults = map[string]string{"clean": "passed", "blocked": "blocked", "failed": "failed"}

// jsonVerdict writes §5.4's object and returns the code the human verdict would: the body carries it
// too, so CI can read one thing. Every string is the host's, unpainted; encoding/json escapes the rest.
func (a *app) jsonVerdict(run checkRun, code int) int {
	res := run.Result
	out := jsonRun{Result: jsonResults[res.State], Reason: res.FailureReason, NotChecked: res.NotChecked, App: run.App, GateRun: run.Run, StartedAt: run.StartedAt,
		DurationMS: run.Elapsed.Milliseconds(), Counts: jsonCounts{Findings: len(res.Findings), Blocking: res.Blocking},
		Findings: make([]jsonFinding, 0, len(res.Findings)), Notes: append([]producerNote{}, run.Notes...), URLs: jsonURLs{App: run.Host + "/#/apps/" + run.App}, ExitCode: code}
	if a.verb.name == "deploy" {
		out.Env = "preview" // the target the deploy banner names; the deploy row's own when it shipped
	}
	if a.shipped(run) {
		// CI acts on this url unattended, so a host may not hand it a scheme a browser would run
		// or a bare string: http(s) with a host, or the run is refused like any other bad answer.
		if u, err := url.Parse(run.Deploy.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			a.errf("\n  %s %s answered a deploy url that is not http(s) (run %s).\n", a.render.glyph(stateFail), run.Host, visible(run.Run))
			return exitError
		}
		out.Env = run.Deploy.Env
		out.Deployment = &jsonDeployment{URL: run.Deploy.URL, Env: run.Deploy.Env}
	}
	for _, f := range res.Findings {
		bag, where := f.Properties.Canyon, f.Locations[0].PhysicalLocation
		blocks, _ := f.Blocking() // verdict has already refused a finding that says nothing
		out.Findings = append(out.Findings, jsonFinding{Severity: bag.Severity, Blocks: blocks, Rule: f.RuleID,
			File: where.ArtifactLocation.URI, Line: where.Region.StartLine, Column: where.Region.StartColumn,
			Problem: bag.Problem, Fix: bag.Fix, PolicyName: bag.PolicyName, SecretName: bag.SecretName, CarriedFrom: bag.CarriedFrom,
			RequiredEndState: bag.RequiredEndState, DoNot: bag.DoNot})
	}
	a.unpark(false)
	var body bytes.Buffer // whole, then one write: half an object on stdout is worse than none
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false) // a fix that says `a > b` reads as written; the object is for CI, not a browser
	if err := enc.Encode(out); err != nil {
		return a.usage(err)
	}
	_, _ = a.out.Write(body.Bytes())
	return code
}
