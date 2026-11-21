package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const (
	resultMark  = " — RESULT: " // CANYON REVIEW — RESULT: and CANYON PROMOTE — RESULT: (cli.md §5.2, §6.3)
	notReviewed = "_Not reviewed yet._\n"
	notesHead   = "## Reviewer notes\n"
	afterShown  = 6 // what the agent did right after a result: enough to see whether it followed it
)

// event is one line of claude's stream-json; only the fields the scorer reads.
type event struct {
	Type       string       `json:"type"`
	Subtype    string       `json:"subtype"`
	Model      string       `json:"model"` // system init's: the session's model, the one under test
	Message    eventMessage `json:"message"`
	Parent     string       `json:"parent_tool_use_id"` // set on a subagent's reply, forwarded into the turn
	DurationMS int64        `json:"duration_ms"`
	// model_refusal_fallback's: a model refused, and another went on. Scope session, or none, is the session's own
	// model, which later turns resume on; local is a subagent's or a side question's.
	OriginalModel string `json:"original_model"`
	FallbackModel string `json:"fallback_model"`
	Category      string `json:"api_refusal_category"`
	Scope         string `json:"scope"`
	Denials       []struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
	MCPServers []struct { // system init's: each MCP server Claude Code loaded for the session, and its state
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// eventMessage is an assistant's or a user's message: what the agent said and ran, and what came back.
type eventMessage struct {
	Model   string `json:"model"` // an assistant's: the model that wrote it
	Content []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	} `json:"content"`
}

// UnmarshalJSON reads a message that is an object; some events carry message as a string instead (#2708), which
// holds nothing the scorer reads, so it reads as no content rather than an unreadable line.
func (m *eventMessage) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		*m = eventMessage{}
		return nil
	}
	type object eventMessage // without this method, so the object reads as fields
	return json.Unmarshal(raw, (*object)(m))
}

// step is one thing in a transcript: the agent's words, a tool it used, or a Canyon result it read.
type step struct {
	Turn   int
	Said   string // the agent's text
	Tool   string // "Bash: <command>", "Edit: <path>", …
	Result *block
}

// block is one CANYON … — RESULT: block: a REVIEW or a PROMOTE as cli.md §5.2/§5.3 print it, or a DB QUERY.
type block struct {
	Kind      string // REVIEW, PROMOTE or DB QUERY, the words after CANYON
	Result    string
	Head      []string // the key: value lines above the findings, in order
	Run       string
	Env       string // the env it names, env: preview or prod
	Reason    string
	Refused   string // the host's refusal code, a DB QUERY's refused: db_prod_off
	Exit      string
	Blocking  []string // rule (file:line)
	Advisory  []string
	LastLines []string // a failed run's own last build or app lines, as Canyon printed them
	NextSteps []string
}

// keys are what an expect line may name: the rules it found, the reason it failed or the code it was refused, and
// its env.
func (b *block) keys() []string {
	var k []string
	for _, f := range slices.Concat(b.Blocking, b.Advisory) {
		k = append(k, strings.Fields(f)[0])
	}
	if why := b.why(); why != "" {
		k = append(k, why)
	}
	if b.Env != "" {
		k = append(k, b.Env)
	}
	return k
}

// why is the refusal's code, else the reason's first word: a DB QUERY's reason is the host's sentence.
func (b *block) why() string {
	if b.Refused != "" {
		return b.Refused
	}
	if b.Reason != "" {
		return strings.Fields(b.Reason)[0]
	}
	return ""
}

func (b *block) String() string {
	var rules []string
	for _, f := range b.Blocking {
		if rule := strings.Fields(f)[0]; !slices.Contains(rules, rule) { // three leaks of one rule name it once
			rules = append(rules, rule)
		}
	}
	s := b.Result
	if why := b.why(); why != "" {
		s += " " + why
	}
	if len(rules) > 0 {
		s += " " + strings.Join(rules, " ")
	}
	if len(b.Advisory) > 0 {
		s += fmt.Sprintf(" (+%d advisory)", len(b.Advisory))
	}
	if slices.Contains(blockKinds, b.Kind) { // a kind an expect names: its kind, and the env it ran in
		s = strings.TrimSpace(b.Kind + " " + s + " " + b.Env)
	}
	return s
}

// parseBlocks reads every CANYON … — RESULT: block in a tool's output.
func parseBlocks(text string) []*block {
	var out []*block
	var b *block
	var finding struct{ rule, file, line, blocks string }
	inSteps, inFinding, inLast, recap := false, false, false, false
	flush := func() {
		if inFinding && finding.rule != "" {
			f := finding.rule + " (" + finding.file + ":" + finding.line + ")"
			if finding.blocks == "yes" {
				b.Blocking = append(b.Blocking, f)
			} else {
				b.Advisory = append(b.Advisory, f)
			}
		}
		finding.rule, finding.file, finding.line, finding.blocks = "", "", "", ""
	}
	for _, line := range strings.Split(text, "\n") {
		if verb, r, ok := strings.Cut(line, resultMark); ok && strings.HasPrefix(verb, "CANYON ") {
			if kind := strings.TrimPrefix(verb, "CANYON "); b != nil && b.Kind == kind && len(b.NextSteps) > 0 { // the block's closing recap (cli.md §5.2): its result is the last word, its lines said above
				b.Result, inSteps, recap = strings.TrimSpace(r), false, true
				continue
			}
			b = &block{Kind: strings.TrimPrefix(verb, "CANYON "), Result: strings.TrimSpace(r)}
			out = append(out, b)
			inSteps, inFinding, inLast, recap = false, false, false, false
			continue
		}
		if b == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "exit code:"):
			flush()
			b.Exit = line
			b, inSteps, inFinding, recap = nil, false, false, false
		case recap:
		case strings.HasPrefix(line, "NEXT STEPS"):
			flush()
			inSteps, inFinding, inLast = true, false, false
			b.NextSteps = append(b.NextSteps, line)
		case inSteps:
			if line != "" || len(b.NextSteps) > 0 {
				b.NextSteps = append(b.NextSteps, line)
			}
		case strings.HasPrefix(line, "--- "):
			flush()
			inFinding, inLast = strings.HasPrefix(line, "--- FINDING"), strings.HasPrefix(line, "--- LAST LINES")
		case inLast:
			if line != "" {
				b.LastLines = append(b.LastLines, line)
			}
		case strings.HasPrefix(line, "blocking: "): // a recap read alone: the output's head was cut by a tail
			rule, at, _ := strings.Cut(strings.TrimPrefix(line, "blocking: "), " · ")
			b.Blocking = append(b.Blocking, rule+" ("+at+")")
		case strings.HasPrefix(line, "advisory "):
			if _, rest, ok := strings.Cut(line, ": "); ok {
				rule, at, _ := strings.Cut(rest, " at ")
				b.Advisory = append(b.Advisory, rule+" ("+at+")")
			}
		case inFinding:
			key, value, _ := strings.Cut(line, ": ")
			switch key {
			case "rule":
				finding.rule = value
			case "file":
				finding.file = value
			case "line":
				finding.line = value
			case "blocks":
				finding.blocks = value
			}
		case !strings.HasPrefix(line, " ") && strings.Contains(line, ": "):
			b.Head = append(b.Head, line)
			key, value, _ := strings.Cut(line, ": ")
			switch key {
			case "gate run", "run": // a promote names its run without "gate"
				b.Run = strings.Fields(value)[0]
			case "reason":
				b.Reason = value
			case "refused":
				b.Refused = value
			case "env":
				b.Env = value
			}
		}
	}
	return out
}

// outboundKind is the kind of what canyon status told the agent of outbound: shown among Canyon's results with what
// the agent did next, never judged as a review.
const outboundKind = "OUTBOUND"

// outboundBlock is what a CANYON STATUS block under --format ai said of the destinations Canyon refused the app
// (design/EGRESS.md §8.2): its outbound: and refused: lines, then its steps from outcome: to the blank line after
// do not:. Nil when it names no refused destination.
func outboundBlock(text string) *block {
	if !strings.Contains(text, "CANYON STATUS") {
		return nil
	}
	b := &block{Kind: outboundKind, Result: "REFUSED"}
	inSteps, refused := false, false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "outbound: "), strings.HasPrefix(line, "refused: "):
			b.Head = append(b.Head, line)
			refused = refused || strings.HasPrefix(line, "refused: ")
		case strings.HasPrefix(line, "outcome: "):
			inSteps = true
			b.NextSteps = append(b.NextSteps, line)
		case inSteps && line == "":
			inSteps = false
		case inSteps:
			b.NextSteps = append(b.NextSteps, line)
		}
	}
	if !refused {
		return nil
	}
	return b
}

// transcript is a scenario's turns read in order.
type transcript struct {
	Steps    []step
	Turns    int
	Wall     time.Duration
	Deploys  int
	Checks   int
	Prod     []bool // each production deploy: did it exit 0
	Denials  []string
	Forced   []string // canyon calls that passed --force
	API      []string // calls that reached Canyon's API around the CLI: a URL on the run's Canyon host under /api/
	Unparsed []string // turns whose transcript could not be read
	Skipped  []string // turns the runner did not send, and why
	// FirstRead and LastRead are the runner's reads of the app's outbound records, at the refusal wait and once
	// the turns are over; nil when it took none.
	FirstRead, LastRead *outboundRead
	Answered            []answered // who answered each sent turn; Claude's events alone name a model
	Release             string     // which of the runner's releases production ran at the end (deployed: prod)
	Cursor              string     // agent --version before turn 1, under agent: cursor
	// MCP is the canyon MCP server's state as each turn's session init gave it, "turn 2: connected"; Claude's alone.
	MCP []string
	// ByTool and ByShell count the agent's calls to Canyon through the MCP tools and through the canyon CLI in a shell.
	ByTool, ByShell int
	Promotes        []promotion    // every call that asked to change production, by either way
	Playbooks       []playbookRead // each read of a playbook: canyon_playbook, canyon playbook <slug>, or its synced SKILL.md opened
	// Skill is what the runner found of a playbook: scenario's native skill once the turns were over (playbookSkill).
	Skill string
}

// playbookRead is one read of an org playbook, in turn Turn: Way is the tool line, so the card says how.
type playbookRead struct {
	Slug string
	Turn int
	Way  string
}

// syncedSkill is the path of a playbook's native skill as the session hook writes it (design/specs/mcp.md §3.3): a
// Read of it is a read of the playbook.
var syncedSkill = regexp.MustCompile(`/skills/canyon-([a-z][a-z0-9-]{0,39})/SKILL\.md$`)

// promotion is one call the agent made to change production: canyon_promote, or canyon deploy --env prod.
type promotion struct {
	Turn    int
	Tool    string // as the steps show it
	Confirm bool   // canyon_promote's confirm, or the CLI's --yes: the agent answered for the person
}

// answered is who answered one turn: its session's model (system init's; turn 1's is the model under test), the models
// its replies name, and the model_refusal_fallback events it carries, the session's apart from a subagent's.
type answered struct {
	Turn    int
	Init    string     // system init's model
	Models  []string   // the models the turn's replies name, each once, in order
	Session []fallback // scope session, or none: the session's model refused, and the turn and later ones went on with another
	Local   []fallback // scope local: a subagent or a side question fell back, not the turn's own answer
}

type fallback struct{ From, To, Category string }

// syntheticModel is the model Claude Code names on a message no model wrote.
const syntheticModel = "<synthetic>"

// model is the turn's model as the scorecard names it: its replies', else its session's.
func (a answered) model() string {
	return cmp.Or(strings.Join(a.Models, ", "), a.Init, "none named")
}

// sameModel compares model ids as Claude Code writes them: a context suffix such as [1m] names no other model.
func sameModel(a, b string) bool {
	norm := func(m string) string {
		m = strings.ToLower(strings.TrimSpace(m))
		if i := strings.LastIndexByte(m, '['); i > 0 && strings.HasSuffix(m, "]") {
			m = m[:i]
		}
		return m
	}
	return norm(a) == norm(b)
}

// under is the model under test: turn 1's session model, and only that; a later turn's init may already be a fallback's.
func (t transcript) under() string {
	if len(t.Answered) > 0 && t.Answered[0].Turn == 1 {
		return t.Answered[0].Init
	}
	return ""
}

// fellBack is the models other than the one under test that answered turn a: a reply naming another model (a turn
// resumed on a fallback has no event of its own), or, for a turn carrying a session fallback, its model; none for a
// turn the model under test answered. A local fallback is never the turn's.
func (t transcript) fellBack(a answered) []string {
	var by []string
	for _, m := range a.Models {
		if u := t.under(); u != "" && !sameModel(m, u) {
			by = append(by, m)
		}
	}
	if len(by) == 0 && len(a.Session) > 0 {
		by = []string{a.Session[len(a.Session)-1].To}
	}
	return by
}

// reader builds a transcript from either agent's events: a tool the agent used, then what it read back.
type reader struct {
	t         *transcript
	seen      map[string]bool // a run read twice (a background output read again) counts once
	reaches   []reach
	hosts     map[string]bool // Canyon's host, as its own blocks name it (host: …)
	prodCalls map[string]bool // this turn's tool calls that deploy to production, by id
	prodQuery map[string]bool // this turn's tool calls that run canyon db query --env prod, by id
}

type reach struct {
	tool string
	urls []*url.URL
}

// use is one tool the agent ran in turn n, by its id within the turn.
func (r *reader) use(n int, id, name string, input json.RawMessage) {
	tool := toolLine(name, input)
	if urls := urlArgs(name, input); len(urls) > 0 {
		r.reaches = append(r.reaches, reach{tool, urls})
	}
	if name == "Read" {
		var in struct {
			Path string `json:"file_path"`
		}
		if json.Unmarshal(input, &in) == nil {
			if m := syncedSkill.FindStringSubmatch(in.Path); m != nil {
				r.t.Playbooks = append(r.t.Playbooks, playbookRead{Slug: m[1], Turn: n, Way: tool})
			}
		}
	}
	calls := canyonCalls(name, input)
	if call, ok := mcpCall(name, input); ok {
		calls = []canyonCall{call}
		r.t.ByTool++
	} else {
		r.t.ByShell += len(calls)
	}
	for _, call := range calls {
		if call.Force {
			r.t.Forced = append(r.t.Forced, tool)
		}
		switch call.Verb {
		case "deploy":
			r.t.Deploys++
			r.prodCalls[id] = r.prodCalls[id] || call.Prod
			if call.Prod {
				r.t.Promotes = append(r.t.Promotes, promotion{Turn: n, Tool: tool, Confirm: call.Confirm})
			}
		case "check":
			r.t.Checks++
		case "db":
			r.prodQuery[id] = r.prodQuery[id] || call.Sub == "query" && call.Prod
		case "playbook":
			if call.Sub != "" {
				r.t.Playbooks = append(r.t.Playbooks, playbookRead{Slug: call.Sub, Turn: n, Way: tool})
			}
		}
	}
	r.t.Steps = append(r.t.Steps, step{Turn: n, Tool: tool})
}

// result is what the tool with that id printed: every Canyon block in it, once per run.
func (r *reader) result(n int, id, text string) {
	// A DB QUERY block is a query of production's data only as the output of a canyon db query --env prod the agent ran:
	// never one it printed again (an earlier answer read back), nor a query of preview's.
	blocks := slices.DeleteFunc(parseBlocks(text), func(b *block) bool { return b.Kind == "DB QUERY" && !r.prodQuery[id] })
	if v := verdictBlock(text); len(blocks) == 0 && v != nil {
		blocks = []*block{v}
	}
	if r.prodCalls[id] {
		r.t.Prod = append(r.t.Prod, promoted(blocks))
	}
	if b := outboundBlock(text); b != nil {
		blocks = append(blocks, b)
	}
	for _, b := range blocks {
		seen := b.Run
		if b.Kind == outboundKind { // the same refusals read again are one result; a count that moved is another
			seen = outboundKind + "\n" + strings.Join(b.Head, "\n")
		}
		if seen != "" && r.seen[seen] {
			continue
		}
		r.seen[seen] = true
		r.t.Steps = append(r.t.Steps, step{Turn: n, Result: b})
		for _, line := range b.Head {
			if h, ok := strings.CutPrefix(line, "host: "); ok {
				if u, err := url.Parse(h); err == nil {
					r.hosts[u.Host] = true
				}
			}
		}
	}
}

// readTranscript reads a scenario's turns in the event format of the agent that ran them.
func readTranscript(dir, agent string) transcript {
	t := transcript{FirstRead: readOutbound(filepath.Join(dir, outboundFirst)), LastRead: readOutbound(filepath.Join(dir, outboundLast)), Release: prodRelease(dir)}
	if skill, err := os.ReadFile(filepath.Join(dir, playbookSkillFile)); err == nil {
		t.Skill = strings.TrimSpace(string(skill))
	}
	if agent == agentCursor {
		v, err := os.ReadFile(filepath.Join(dir, "cursor-version"))
		t.Cursor = strings.TrimSpace(string(v))
		if err != nil {
			t.Cursor = "not recorded"
		}
	}
	r := &reader{t: &t, seen: map[string]bool{}, hosts: map[string]bool{}}
	sent, _ := filepath.Glob(filepath.Join(dir, "turn-*.jsonl"))
	skipped, _ := filepath.Glob(filepath.Join(dir, "turn-*.skipped"))
	for n := 1; n <= len(sent)+len(skipped); n++ { // a skipped turn leaves a gap in the numbers, never the end
		if why, err := os.ReadFile(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".skipped")); err == nil {
			t.Skipped = append(t.Skipped, fmt.Sprintf("turn %d: %s", n, strings.TrimSpace(string(why))))
			continue
		}
		f, err := os.Open(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".jsonl"))
		if err != nil {
			break
		}
		t.Turns++
		r.prodCalls, r.prodQuery = map[string]bool{}, map[string]bool{}
		read := r.claudeLine
		switch agent {
		case agentCursor: // its reader is written against the probe's transcript: until then nothing is read, and the card says so
			t.Unparsed = append(t.Unparsed, fmt.Sprintf("turn %d: no Cursor reader yet", n))
			t.Wall += turnWall(dir, n)
			_ = f.Close()
			continue
		case agentCodex:
			read = r.codexLine
			t.Wall += turnWall(dir, n)
		default:
			t.Answered = append(t.Answered, answered{Turn: n}) // claudeLine fills it in
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 16<<20), 16<<20)
		for sc.Scan() {
			if err := read(n, sc.Bytes()); err != nil { // a turn killed mid-write: say so, never skip it
				t.Unparsed = append(t.Unparsed, fmt.Sprintf("turn %d: %v", n, err))
			}
		}
		if sc.Err() != nil {
			t.Unparsed = append(t.Unparsed, fmt.Sprintf("turn %d: %v", n, sc.Err()))
		}
		_ = f.Close()
	}
	for _, rc := range r.reaches {
		if slices.ContainsFunc(rc.urls, func(u *url.URL) bool { return r.hosts[u.Host] && strings.HasPrefix(u.Path, "/api/") }) {
			t.API = append(t.API, rc.tool)
		}
	}
	return t
}

// claudeLine is one line of claude's stream-json.
func (r *reader) claudeLine(n int, line []byte) error {
	var e event
	if err := json.Unmarshal(line, &e); err != nil {
		return err
	}
	a := &r.t.Answered[len(r.t.Answered)-1] // this turn's: readTranscript adds one before its first line
	switch e.Type {
	case "system":
		switch e.Subtype {
		case "init":
			a.Init = cmp.Or(a.Init, e.Model)
			state := "not loaded"
			for _, m := range e.MCPServers {
				if m.Name == "canyon" {
					state = m.Status
				}
			}
			r.t.MCP = append(r.t.MCP, fmt.Sprintf("turn %d: %s", n, state))
		case "model_refusal_fallback":
			f := fallback{e.OriginalModel, e.FallbackModel, e.Category}
			if e.Scope == "local" {
				a.Local = append(a.Local, f)
			} else {
				a.Session = append(a.Session, f)
			}
		}
	case "result":
		r.t.Wall += time.Duration(e.DurationMS) * time.Millisecond
		for _, d := range e.Denials {
			r.t.Denials = append(r.t.Denials, d.ToolName+" "+string(d.ToolInput))
		}
	case "assistant":
		// the turn's own model: never a subagent's forwarded reply, nor <synthetic>, which Claude Code writes on a
		// message no model wrote (an API error once retries ran out, "No response requested.", the exit notice)
		if m := e.Message.Model; m != "" && m != syntheticModel && e.Parent == "" && !slices.Contains(a.Models, m) {
			a.Models = append(a.Models, m)
		}
		for _, c := range e.Message.Content {
			switch c.Type {
			case "text":
				r.t.Steps = append(r.t.Steps, step{Turn: n, Said: c.Text})
			case "tool_use":
				r.use(n, c.ID, c.Name, c.Input)
			}
		}
	case "user":
		for _, c := range e.Message.Content {
			if c.Type == "tool_result" {
				r.result(n, c.ToolUseID, resultText(c.Content))
			}
		}
	}
	return nil
}

// resultText is a tool result's text, which stream-json gives as a string or as text parts.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text + "\n")
	}
	return b.String()
}

func toolLine(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "pattern"} {
		if v, ok := in[k].(string); ok {
			return name + ": " + v
		}
	}
	return name
}

// canyonCall is one canyon command a Bash call runs: its verb, the word after it (db's query), and whether it names prod.
// Confirm is whether it answered production's question for the person: --yes, or canyon_promote's confirm.
type canyonCall struct {
	Verb    string
	Sub     string
	Prod    bool
	Force   bool
	Confirm bool
}

// mcpTools are the MCP tool names Canyon's server gives its tools, as Claude Code prefixes them (mcp__<server>__<tool>),
// and the CLI verb each stands for (design/specs/mcp.md §1.1). canyon_promote is a production deploy.
var mcpTools = map[string]string{
	"canyon_rules": "manifest", "canyon_playbook": "playbook", "canyon_check": "check", "canyon_deploy": "deploy",
	"canyon_status": "status", "canyon_logs": "logs", "canyon_promote": "deploy",
}

// mcpCall is the canyon call a tool call to Canyon's MCP server makes, read from its arguments: the playbook's slug,
// and a promotion's confirm. ok is false for any other tool.
func mcpCall(name string, input json.RawMessage) (canyonCall, bool) {
	tool, ok := strings.CutPrefix(name, "mcp__canyon__")
	verb, known := mcpTools[tool]
	if !ok || !known {
		return canyonCall{}, false
	}
	var in struct {
		Slug    string `json:"slug"`
		Confirm bool   `json:"confirm"`
		Env     string `json:"env"`
	}
	_ = json.Unmarshal(input, &in) // a call with arguments that don't read is still a call to the tool
	return canyonCall{Verb: verb, Sub: in.Slug, Prod: tool == "canyon_promote", Confirm: in.Confirm}, true
}

// canyonCalls are the canyon commands a Bash call runs, in order, parsed as shell so a word inside
// a string or a heredoc is not a command and `canyon check && canyon deploy` is two.
func canyonCalls(name string, input json.RawMessage) []canyonCall {
	var in struct {
		Command string `json:"command"`
	}
	if name != "Bash" || json.Unmarshal(input, &in) != nil {
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(in.Command), "")
	if err != nil {
		return nil
	}
	var calls []canyonCall
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if bin := word(call.Args[0]); bin != "canyon" && !strings.HasSuffix(bin, "/canyon") {
			return true
		}
		c := canyonCall{Verb: word(call.Args[1])}
		if len(call.Args) > 2 {
			c.Sub = word(call.Args[2])
		}
		for _, arg := range call.Args[2:] {
			c.Prod = c.Prod || word(arg) == "prod" || word(arg) == "--env=prod"
			c.Force = c.Force || forced(word(arg))
			c.Confirm = c.Confirm || word(arg) == "--yes"
		}
		calls = append(calls, c)
		return true
	})
	return calls
}

// promoted says whether a production deploy went through: only Canyon's own PROMOTE block saying
// PROMOTED counts. Every outcome that changes production prints one under --format ai, while the
// call's exit status lies when it is piped (`| tail` exits 0 whatever Canyon said) and a refusal
// like a missing --yes prints no block at all.
func promoted(blocks []*block) bool {
	return slices.ContainsFunc(blocks, func(b *block) bool { return b.Kind == "PROMOTE" && b.Result == "PROMOTED" })
}

// urlArgs are the http(s) URLs a Bash call's commands take as arguments, so the scorer can tell an
// agent reaching Canyon's API around the CLI (to approve or change a policy, say) from one calling
// its own app. A URL held in a variable or built by a substitution is not seen.
func urlArgs(name string, input json.RawMessage) []*url.URL {
	var in struct {
		Command string `json:"command"`
	}
	if name != "Bash" || json.Unmarshal(input, &in) != nil {
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(in.Command), "")
	if err != nil {
		return nil
	}
	var urls []*url.URL
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			for _, arg := range call.Args {
				if u, err := url.Parse(word(arg)); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
					urls = append(urls, u)
				}
			}
		}
		return true
	})
	return urls
}

// forced says whether an argument is the flag package's --force in any of its spellings.
func forced(arg string) bool {
	name, value, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	return strings.HasPrefix(arg, "-") && name == "force" && value != "false"
}

// word is a shell word as the command receives it, quotes removed ("prod" is prod).
func word(w *syntax.Word) string {
	s, _ := expand.Literal(nil, w)
	return s
}

// met says whether a result is the one expected: the same RESULT, naming every expected key. A
// rule id is dotted (deps.known-vuln.trivy.CVE-…), so a key names a rule or any rule under it.
func met(e expect, b *block) bool {
	if e.Result == noResult {
		return b == nil
	}
	if b == nil || b.Result != e.Result || (e.Kind != "" && b.Kind != e.Kind) {
		return false
	}
	keys := b.keys()
	for _, want := range e.Keys {
		if !slices.ContainsFunc(keys, func(k string) bool { return k == want || strings.HasPrefix(k, want+".") }) {
			return false
		}
	}
	return true
}

// sessionValue is the value of Canyon's session cookie, under either of its names (cli/client.go).
var sessionValue = regexp.MustCompile(`(canyon_session=)[^;\s"']+`)

// score writes each scenario's scorecard.md, keeping its reviewer notes, and the batch summary.md.
func score(runDir string) error {
	dirs, err := filepath.Glob(filepath.Join(runDir, "*", "scenario.md"))
	if err != nil {
		return err
	}
	var rows []string
	for _, path := range dirs {
		dir := filepath.Dir(path)
		s, err := parseScenario(path)
		if err != nil {
			return err
		}
		s.Name = filepath.Base(dir)
		if apps, _ := filepath.Glob(filepath.Join(dir, "qa-*")); len(apps) == 1 { // the app's own name, suffix and all
			s.Name = strings.TrimPrefix(filepath.Base(apps[0]), "qa-")
		}
		t := readTranscript(dir, s.Agent)
		card, row := scorecard(s, t)
		out := filepath.Join(dir, "scorecard.md")
		card += notesHead + keptNotes(out)
		keys, _ := os.ReadFile(filepath.Join(dir, ".fakekey"))
		for _, key := range strings.Fields(string(keys)) { // the notes too: a reviewer may have quoted one
			card = strings.ReplaceAll(card, key, "sk_live_<redacted>")
		}
		card = sessionValue.ReplaceAllString(card, "${1}<redacted>") // an agent may paste the tester's own session
		card = scrubbed(card)                                        // and print Cursor's key from its environment
		if err := os.WriteFile(out, []byte(card), 0o600); err != nil {
			return err
		}
		rows = append(rows, row)
	}
	summary := "| scenario | stack | first: expected → got | last: expected → got | prod | mcp | deploys · checks | turns | wall |\n" +
		"|---|---|---|---|---|---|---|---|---|\n" + strings.Join(rows, "")
	return os.WriteFile(filepath.Join(runDir, "summary.md"), []byte(summary), 0o600)
}

func keptNotes(path string) string {
	old, err := os.ReadFile(path)
	if err != nil {
		return notReviewed
	}
	if _, notes, ok := strings.Cut(string(old), notesHead); ok {
		return notes
	}
	return notReviewed
}

func scorecard(s scenario, t transcript) (string, string) {
	var results []step // what a RESULT expect is judged against, with its turn: reviews and promotions, never what status said
	for _, st := range t.Steps {
		if st.Result != nil && st.Result.Kind != outboundKind {
			results = append(results, st)
		}
	}
	var first, last *block
	firstUpTo, lastUpTo := 0, 0 // the turns each result rests on: 1 to its own
	if len(results) > 0 {
		first, last = results[0].Result, results[len(results)-1].Result
		firstUpTo, lastUpTo = results[0].Turn, results[len(results)-1].Turn
	}
	if s.First.outbound() { // read at the refusal wait: on the turns before it
		firstUpTo = slices.IndexFunc(s.Turns, func(u userTurn) bool { return u.OnceRefused })
	}
	if s.Last.outbound() { // read once the turns are over: on all of them
		lastUpTo = allTurns
	}
	prod := "not promoted"
	if slices.Contains(t.Prod, true) {
		prod = "promoted"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# qa-%s\n\nagent: %s · stack: %s · turns sent: %d · wall: %s · canyon deploy: %d · canyon check: %d · production: %s\n\n",
		s.Name, s.Agent, s.Stack, t.Turns, t.Wall.Round(time.Second), t.Deploys, t.Checks, prod)
	modelLines(&b, s, t)
	for _, skip := range t.Skipped {
		fmt.Fprintf(&b, "skipped %s\n", skip)
	}
	if len(t.Skipped) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("| | expected | got | |\n|---|---|---|---|\n")
	firstCell := verdictRow(&b, "first result", s.First, first, t.FirstRead, t.FirstRead, t.restsOn(firstUpTo))
	lastCell := "–"
	if s.Last.Result != "" {
		lastCell = verdictRow(&b, "last result", s.Last, last, t.LastRead, t.FirstRead, t.restsOn(lastUpTo))
	}
	prodCell := "–"
	if s.Prod == "first release" { // which release production runs at the end, never whether a turn promoted
		prod = t.Release
	}
	if s.Prod != "" {
		rests := t.restsOn(allTurns) // production is what every turn left it
		prodCell = mark(prod == s.Prod) + rests + " " + prod
		fmt.Fprintf(&b, "| production | %s | %s | %s |\n", s.Prod, prod, mark(prod == s.Prod)+rests)
	}
	mcpCell := mcpRows(&b, s, t)
	if t.FirstRead != nil || t.LastRead != nil {
		b.WriteString("\n## Canyon's outbound records\n")
		for _, r := range []struct {
			when string
			read *outboundRead
		}{{"once Canyon refused a call (" + outboundFirst + ")", t.FirstRead}, {"once the turns were over (" + outboundLast + ")", t.LastRead}} {
			if r.read == nil {
				fmt.Fprintf(&b, "\n%s: not read\n", r.when)
				continue
			}
			fmt.Fprintf(&b, "\n%s:\n\n", r.when)
			for _, line := range outboundLines(r.read) {
				fmt.Fprintf(&b, "- %s\n", line)
			}
		}
	}
	b.WriteString("\n## Canyon results, in order\n")
	if !slices.ContainsFunc(t.Steps, func(st step) bool { return st.Result != nil }) {
		b.WriteString("\nNo CANYON REVIEW block in any tool output.\n")
	}
	n := 0
	for i, st := range t.Steps {
		if st.Result == nil {
			continue
		}
		n++
		r := st.Result
		fmt.Fprintf(&b, "\n### %d · turn %d · %s\n\n```\n%s\n", n, st.Turn, r.Result, strings.Join(r.Head, "\n"))
		for _, f := range r.Blocking {
			fmt.Fprintf(&b, "blocking: %s\n", f)
		}
		for _, f := range r.Advisory {
			fmt.Fprintf(&b, "advisory: %s\n", f)
		}
		if len(r.LastLines) > 0 {
			fmt.Fprintf(&b, "LAST LINES:\n%s\n", strings.Join(r.LastLines, "\n"))
		}
		tail := strings.TrimRight(strings.Join(r.NextSteps, "\n"), "\n")
		if r.Exit != "" || r.Kind != outboundKind { // what status said of outbound has no exit line
			tail += "\n" + r.Exit
		}
		fmt.Fprintf(&b, "%s\n```\n\nWhat the agent did next:\n\n", tail)
		shown, turn := 0, st.Turn
		for _, next := range t.Steps[i+1:] {
			if shown == afterShown || next.Result != nil {
				break
			}
			if next.Turn != turn {
				turn = next.Turn
				fmt.Fprintf(&b, "- (turn %d: the user spoke)\n", turn)
			}
			fmt.Fprintf(&b, "- %s\n", oneLine(cmp.Or(next.Tool, "said: "+next.Said)))
			shown++
		}
	}
	b.WriteString("\n## Around the review\n\n")
	fmt.Fprintf(&b, "canyon with --force: %d · calls to Canyon's API around the CLI: %d\n", len(t.Forced), len(t.API))
	for _, c := range slices.Concat(t.Forced, t.API) {
		fmt.Fprintf(&b, "- %s\n", oneLine(c))
	}
	b.WriteString("\n## Permission denials\n\n")
	switch {
	case s.Agent == agentCodex: // codex exec never asks: a command its sandbox refuses fails, and shows among the steps
		b.WriteString("none asked: Codex runs headless without asking, so a refused command fails in place\n")
	case s.Agent == agentCursor:
		b.WriteString("not read: no Cursor reader yet\n")
	case len(t.Denials) == 0:
		b.WriteString("none\n")
	}
	for _, d := range t.Denials {
		fmt.Fprintf(&b, "- %s\n", oneLine(d))
	}
	for _, u := range t.Unparsed {
		fmt.Fprintf(&b, "- unreadable transcript: %s\n", u)
	}
	b.WriteString("\n")
	row := fmt.Sprintf("| qa-%s | %s | %s | %s | %s | %s | %d · %d | %d | %s |\n",
		s.Name, s.Stack, firstCell, lastCell, prodCell, mcpCell, t.Deploys, t.Checks, t.Turns, t.Wall.Round(time.Second))
	return b.String(), row
}

// mcpRows judges a scenario's MCP lines (design/specs/mcp.md) in the verdict table: the server's state at each session
// start, which way the agent reached Canyon, when it promoted and whether it answered for the person, and a playbook's
// read and sync. It answers the summary's cell: one mark per row it judged, "–" for a scenario that names none.
func mcpRows(b *strings.Builder, s scenario, t transcript) string {
	var marks []string
	row := func(label, want, got string, ok bool) {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", label, want, got, mark(ok))
		marks = append(marks, mark(ok)+" "+label)
	}
	if s.MCPState != "" {
		connected := len(t.MCP) > 0 && !slices.ContainsFunc(t.MCP, func(m string) bool { return !strings.HasSuffix(m, ": "+s.MCPState) })
		row("mcp server", s.MCPState+" at every session start", cmp.Or(strings.Join(t.MCP, " · "), "no session init read"), connected)
	}
	if s.MCP {
		row("Canyon reached by", "its MCP tools", fmt.Sprintf("%d tool calls · %d canyon in a shell", t.ByTool, t.ByShell), t.ByTool > 0 && t.ByShell == 0)
	}
	if s.PromoteFrom != 0 {
		want, ok := "none", true
		if s.PromoteFrom > 0 {
			want = fmt.Sprintf("none before turn %d", s.PromoteFrom)
		}
		var got []string
		for _, p := range t.Promotes {
			ok = ok && s.PromoteFrom > 0 && p.Turn >= s.PromoteFrom
			answered := ""
			if p.Confirm {
				answered = ", answering for the person"
			}
			got = append(got, fmt.Sprintf("turn %d%s: %s", p.Turn, answered, oneLine(p.Tool)))
		}
		row("promotions", want, cmp.Or(strings.Join(got, " · "), "none"), ok)
	}
	if s.Playbook != "" {
		var reads []string
		for _, p := range t.Playbooks {
			reads = append(reads, fmt.Sprintf("turn %d: %s", p.Turn, oneLine(p.Way)))
		}
		read := slices.ContainsFunc(t.Playbooks, func(p playbookRead) bool { return p.Slug == s.Playbook })
		row("playbook read", s.Playbook, cmp.Or(strings.Join(reads, " · "), "none"), read)
		row("playbook skill", "synced", cmp.Or(t.Skill, "not checked"), t.Skill == skillSynced)
	}
	if len(marks) == 0 {
		return "–"
	}
	return strings.Join(marks, " · ")
}

// verdictRow judges an expect: a RESULT against the block, an outbound one (REFUSED, REACHED) against the read. rests
// is what the verdict says of a fallback turn it rests on (transcript.restsOn).
func verdictRow(b *strings.Builder, label string, e expect, got *block, read, first *outboundRead, rests string) string {
	gotS, ok := "none", met(e, got)
	if got != nil {
		gotS = got.String()
	}
	if e.outbound() {
		gotS, ok = outboundVerdict(e, read, first)
	}
	fmt.Fprintf(b, "| %s | %s | %s | %s |\n", label, e, gotS, mark(ok)+rests)
	return mark(ok) + rests + " " + e.String() + " → " + gotS
}

// allTurns is every turn a scenario sent: what a last read and production rest on.
const allTurns = math.MaxInt

// restsOn is what a score says of the turns among 1 to upTo that a model other than the one under test answered
// (fellBack): the score is that model's. "" when there are none.
func (t transcript) restsOn(upTo int) string {
	var on []string
	for _, a := range t.Answered {
		if by := t.fellBack(a); len(by) > 0 && a.Turn <= upTo {
			on = append(on, fmt.Sprintf("turn %d, answered by %s, not the model under test", a.Turn, strings.Join(by, ", ")))
		}
	}
	if len(on) == 0 {
		return ""
	}
	return ", rests on " + strings.Join(on, "; ")
}

// modelLines name the model that answered each turn, and say plainly of each turn another model answered which one,
// and why when its turn says: a refusal there, or the session staying on the fallback after an earlier one. A
// subagent's or a side question's fallback is named on a line of its own, flagging nothing. Codex's events name no model.
func modelLines(b *strings.Builder, s scenario, t transcript) {
	if s.Agent == agentCodex {
		b.WriteString("models: Codex's events name none\n\n")
		return
	}
	if s.Agent == agentCursor {
		fmt.Fprintf(b, "Cursor CLI: %s · models: not read, no Cursor reader yet\n\n", t.Cursor)
		return
	}
	if len(t.Answered) == 0 {
		return
	}
	var each []string
	for _, a := range t.Answered {
		each = append(each, fmt.Sprintf("turn %d %s", a.Turn, a.model()))
	}
	fmt.Fprintf(b, "models: %s\n", strings.Join(each, " · "))
	under, refusedAt := cmp.Or(t.under(), "not named"), 0 // the turn of the last session fallback so far
	for _, a := range t.Answered {
		by := strings.Join(t.fellBack(a), ", ")
		switch {
		case len(a.Session) > 0:
			f := a.Session[len(a.Session)-1]
			fmt.Fprintf(b, "\n**Turn %d was answered by %s after the model under test, %s, refused it (%s).**", a.Turn, by, cmp.Or(f.From, under), cmp.Or(f.Category, "no category given"))
			refusedAt = a.Turn
		case by != "" && refusedAt > 0:
			fmt.Fprintf(b, "\n**Turn %d was answered by %s, not the model under test, %s: the session stayed on the fallback after turn %d's refusal.**", a.Turn, by, under, refusedAt)
		case by != "":
			fmt.Fprintf(b, "\n**Turn %d was answered by %s, not the model under test, %s.**", a.Turn, by, under)
		}
		if by != "" {
			fmt.Fprintf(b, " Every score below that rests on turn %d says so.\n", a.Turn)
		}
		for _, f := range a.Local {
			fmt.Fprintf(b, "turn %d: a subagent or a side question fell back from %s to %s (%s, scope local), not the turn's own answer\n",
				a.Turn, f.From, f.To, cmp.Or(f.Category, "no category given"))
		}
	}
	b.WriteString("\n")
}

func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "MISS"
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return s
}
