package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// placeholders stand for the user's leaked key ({{FAKE_KEY}}) and the rotated one they paste in
// chat ({{NEW_KEY}}): the runner swaps in keys built at run time, so no key is ever committed.
var placeholders = []string{"{{FAKE_KEY}}", "{{NEW_KEY}}"}

// scenario is one Markdown file: its header lines, the files the user already has, and the user's
// turns word for word.
type scenario struct {
	Name  string // the file's base name; the app is qa-<name>
	Agent string // who runs the turns: claude-code, unless an agent: line names codex or cursor
	Stack string
	First expect // the first Canyon result the run must print
	Last  expect // the last one; zero when the scenario does not say
	Prod  string // "promoted" or "not promoted"; empty when the scenario does not say
	Rules []policy
	// Deployed is a deployed: preview line: the runner deploys the seed to preview as the tester before
	// turn 1, so the user's app is already running when they first speak. Released is deployed: prod: the seed
	// goes on to production, then Next, the ## Next release files written over it, to preview and production (#2160).
	Deployed bool
	Released bool
	Next     []seedFile
	Egress   []egressMode
	// Secrets are secret: lines: runtime values the runner sets before the seed deploys, made up at run time.
	Secrets []seededSecret
	// DBProd is a db prod: line, the host's db_prod the scenario needs: an admin sets it from a browser, never the
	// runner, so the runner checks it before the app is made and refuses a host set otherwise.
	DBProd string
	Seed   []seedFile
	Turns  []userTurn
}

// egressMode is an app env's outbound mode, allowlist, set as its owner may once the seed is deployed.
type egressMode struct{ Env, Mode string }

// seededSecret is a runtime secret of an app env the user had set before they first speak: secret: preview NAME.
type seededSecret struct{ Env, Name string }

// outboundResults are the expects judged against Canyon's records of what the app called out to, which the
// runner reads, never against a block the agent read: REFUSED, an egress request filed for the destination;
// REACHED, a call to it that went through after the refusal the run waited for. An expect that names its block's
// kind is a block's, whatever its RESULT.
var outboundResults = []string{"REFUSED", "REACHED"}

// blockKinds are the kinds an expect may name before its RESULT, so it is judged against that kind's blocks alone:
// expect last: DB QUERY REFUSED db_prod_off.
var blockKinds = []string{"DB QUERY"}

// expect is a RESULT and the rule ids, failure reason, refusal code or env that must be among what it names; Kind,
// when it names one, the block's kind.
type expect struct {
	Kind   string
	Result string
	Keys   []string
}

func (e expect) String() string {
	return strings.TrimSpace(strings.TrimSpace(e.Kind+" "+e.Result) + " " + strings.Join(e.Keys, " "))
}

// outbound is whether e is judged against the runner's outbound reads, never a block.
func (e expect) outbound() bool { return e.Kind == "" && slices.Contains(outboundResults, e.Result) }

type seedFile struct{ Path, Body string }

// userTurn is one user message, word for word; IfUnfixed sends it only while the last Canyon result
// is BLOCKED or FAILED, so "do what Canyon says" is never sent once the agent fixed it on its own.
// OnceAsked sends it only once Canyon asks a person to approve its fix for the app's production.
// OnceRefused sends it only once Canyon has refused a call the app made out and filed an egress request;
// OnceAllowed only once a person has allowed every destination that wait saw refused.
type userTurn struct {
	Text        string
	IfUnfixed   bool
	OnceAsked   bool
	OnceRefused bool
	OnceAllowed bool
}

// A turn heading's markers: `### 2 (only if the last result is BLOCKED or FAILED)`, and `### 2 (once
// Canyon asks to approve its fix for production)`, which waits for Canyon's patch of the app; `### 1 (once
// Canyon refuses a call out)` and `### 2 (once a person allows it)`, which wait on the app's outbound records.
const (
	onlyIfUnfixed = "(only if the last result is BLOCKED or FAILED)"
	onceAsked     = "(once Canyon asks to approve its fix for production)"
	onceRefused   = "(once Canyon refuses a call out)"
	onceAllowed   = "(once a person allows it)"
)

// policy is a rule the app holds to a stricter state than the org's, set before the first turn.
type policy struct{ Rule, State string }

// parseScenario reads the header lines above the first "## " heading, the "File: `path`" fences
// under "## Seed", and one fence per "### " heading under "## Turns".
func parseScenario(path string) (scenario, error) {
	f, err := os.Open(path)
	if err != nil {
		return scenario{}, err
	}
	defer func() { _ = f.Close() }()
	s := scenario{Name: strings.TrimSuffix(filepath.Base(path), ".md"), Agent: agentClaude}
	var section, file string
	var fence []string
	inFence, turnOpen := false, false
	var mark userTurn // the open turn's marker
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if inFence {
			if strings.TrimSpace(line) != "```" {
				fence = append(fence, line)
				continue
			}
			inFence = false
			body := strings.Join(fence, "\n") + "\n"
			switch {
			case section == "Seed" && file != "":
				s.Seed = append(s.Seed, seedFile{Path: file, Body: body})
				file = ""
			case section == "Next release" && file != "":
				s.Next = append(s.Next, seedFile{Path: file, Body: body})
				file = ""
			case section == "Turns" && turnOpen:
				mark.Text = strings.TrimSuffix(body, "\n")
				s.Turns = append(s.Turns, mark)
				turnOpen = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "```"):
			inFence, fence = true, nil
		case strings.HasPrefix(line, "## "):
			section = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		case strings.HasPrefix(line, "### ") && section == "Turns":
			_, rest, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "### ")), " ")
			mark = userTurn{IfUnfixed: rest == onlyIfUnfixed, OnceAsked: rest == onceAsked, OnceRefused: rest == onceRefused, OnceAllowed: rest == onceAllowed}
			if rest != "" && mark == (userTurn{}) { // a misspelt marker would send the turn whatever the result
				return scenario{}, fmt.Errorf("%s: turn heading %q: after its number the only text is one of %s", path, line,
					strings.Join([]string{onlyIfUnfixed, onceAsked, onceRefused, onceAllowed}, ", "))
			}
			turnOpen = true
		case (section == "Seed" || section == "Next release") && strings.HasPrefix(line, "File: `"):
			file = strings.TrimSuffix(strings.TrimPrefix(line, "File: `"), "`")
		case section == "":
			if err := s.header(line); err != nil {
				return scenario{}, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return scenario{}, err
	}
	switch {
	case s.Stack == "":
		return scenario{}, fmt.Errorf("%s: no stack: line", path)
	case s.First.Result == "":
		return scenario{}, fmt.Errorf("%s: no expect first: line", path)
	case len(s.Turns) == 0:
		return scenario{}, fmt.Errorf("%s: no turns under ## Turns", path)
	case s.Turns[0].IfUnfixed || s.Turns[0].OnceAsked:
		return scenario{}, fmt.Errorf("%s: turn 1 has no result before it to wait for", path)
	case len(s.Egress) > 0 && !s.Deployed: // the mode route refuses an env Canyon runs nothing of (floor_not_enforced)
		return scenario{}, fmt.Errorf("%s: egress: needs deployed: preview, so the env runs before its mode is set", path)
	case len(s.Next) > 0 && !s.Released:
		return scenario{}, fmt.Errorf("%s: ## Next release needs deployed: prod, which releases it", path)
	case s.Prod == "first release" && (!s.Released || len(s.Next) == 0):
		return scenario{}, fmt.Errorf("%s: expect prod: first release needs deployed: prod and a ## Next release, which production must be told apart from", path)
	}
	refusal := -1 // the first turn that waits for a refusal
	for i, t := range s.Turns {
		switch {
		case t.OnceRefused && refusal < 0:
			refusal = i
		case t.OnceAllowed && refusal < 0: // it waits for what a refusal wait saw
			return scenario{}, fmt.Errorf("%s: turn %d waits for a person to allow what no earlier turn waited to see refused", path, i+1)
		}
	}
	if refusal == 0 && !s.Deployed {
		return scenario{}, fmt.Errorf("%s: turn 1 waits for a refusal of an app nothing deployed: add deployed: preview", path)
	}
	for _, e := range []expect{s.First, s.Last} {
		if e.outbound() && (len(e.Keys) == 0 || refusal < 0) {
			return scenario{}, fmt.Errorf("%s: expect %s names a destination and needs a turn %s, whose read it is judged against", path, e, onceRefused)
		}
	}
	return s, nil
}

func (s *scenario) header(line string) error {
	key, value, ok := strings.Cut(line, ": ")
	if !ok {
		return nil
	}
	switch key {
	case "agent":
		if value != agentClaude && value != agentCodex && value != agentCursor {
			return fmt.Errorf("agent: %q is none of %s, %s and %s", value, agentClaude, agentCodex, agentCursor)
		}
		s.Agent = value
	case "stack":
		s.Stack = value
	case "expect first", "expect last":
		var kind string
		for _, k := range blockKinds {
			if rest, ok := strings.CutPrefix(value, k+" "); ok {
				kind, value = k, rest
			}
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return fmt.Errorf("%s: names no RESULT", key)
		}
		e := expect{Kind: kind, Result: fields[0], Keys: fields[1:]}
		if key == "expect first" {
			s.First = e
		} else {
			s.Last = e
		}
	case "expect prod":
		if !validProd(value) {
			return fmt.Errorf("expect prod: %q is none of %s", value, strings.Join(prodExpects, ", "))
		}
		s.Prod = value
	case "policy":
		fields := strings.Fields(value)
		if len(fields) != 2 {
			return fmt.Errorf("policy: %q is not <rule> <state>", value)
		}
		s.Rules = append(s.Rules, policy{Rule: fields[0], State: fields[1]})
	case "deployed":
		if value != "preview" && value != "prod" {
			return fmt.Errorf("deployed: %q: the runner deploys a seed to preview, or on to prod", value)
		}
		s.Deployed, s.Released = true, value == "prod"
	case "egress":
		fields := strings.Fields(value)
		// allowlist alone: it narrows, as an owner may from any session; observe widens, which only a person does
		// (contracts/routes.md, PUT /api/v1/projects/:id/egress/:env/mode)
		if len(fields) != 2 || !slices.Contains([]string{"preview", "prod"}, fields[0]) || fields[1] != "allowlist" {
			return fmt.Errorf("egress: %q is not <preview|prod> allowlist", value)
		}
		s.Egress = append(s.Egress, egressMode{Env: fields[0], Mode: fields[1]})
	case "secret":
		fields := strings.Fields(value)
		if len(fields) != 2 || !slices.Contains([]string{"preview", "prod"}, fields[0]) {
			return fmt.Errorf("secret: %q is not <preview|prod> NAME", value)
		}
		s.Secrets = append(s.Secrets, seededSecret{Env: fields[0], Name: fields[1]})
	case "db prod": // the values of db_prod (contracts/routes.md, GET /api/v1/preferences)
		if !slices.Contains([]string{"off", "read", "write"}, value) {
			return fmt.Errorf("db prod: %q is none of off, read and write", value)
		}
		s.DBProd = value
	}
	return nil
}
