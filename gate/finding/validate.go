package finding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// MaxSnippetLines is contracts/finding.md's ≤3 lines — a rule the profile schema cannot state,
// since maxLength counts characters. It binds the snippet AS SERIALIZED, and Location.validate
// below is the one place that applies it (the AI reviewer windows a candidate to it beforehand,
// off secrets.* where nothing masks): after redaction, which is the only form a log off the
// wire ever has, and the only form in which a secrets.* literal spanning lines has collapsed to
// the mask. Capping the span before redaction instead is canyonv2-xqx (a template of any length
// carries a credential, and refusing it aborts the whole scan).
const MaxSnippetLines = 3

// noLiveKey refuses a serialized log any string of which carries a live key. Each string is read on
// its own, decoded: across two strings gitleaks would pair one's keyword with another's value, and
// JSON's escaped quotes hide the delimiters it reads a key by.
func noLiveKey(serialized []byte) error {
	dec := json.NewDecoder(bytes.NewReader(serialized))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if s, ok := tok.(string); ok && HasLiveKey([]byte(s)) {
			return errors.New("a live key appears in the serialized log; redaction happens at construction")
		}
	}
}

// unreadableSnippet is the one snippet a gate.unreadable finding carries: never source, only a
// Canyon-run tool's, or an agent's that read nothing, digest over the tree (contracts/finding.md).
// runID is a gate run's id as finding.schema.json's carried_from has it.
var runID = regexp.MustCompile(`^gr_[a-z0-9]{4,}$`)

var unreadableSnippet = regexp.MustCompile(`^\((trivy|opengrep|eslint|grype|checkov|the AI review|custom rules) could not check this tree; tree sha256:[0-9a-f]{64}\)$`)

// Validate checks what a schema cannot express. The lexical half of the profile — field
// presence, enums, patterns, lengths, one location with four region bounds — is the two
// schemas' job and runs on the serialized bytes (contracts.CheckFindingSchemas); a gate runs
// both, in that order. Everything here fails closed: a log missing any of it is an error,
// never dropped and never defaulted. Errors name the run and result that failed, positionally,
// and never a value from the log — not the rule id either, since a log that has not been
// through the schemas can carry anything there. Three checks need the policy snapshot (seam 5) and live with
// it, in ValidateAgainst: that blocks is derived from state and severity, that every ruleId
// is in the snapshot and inside its producer's namespace, and that policy_name is the
// snapshot's name. A gate runs the schemas, then this, then ValidateAgainst.
func (l *Log) Validate() error {
	if l == nil {
		return errors.New("no log")
	}
	if l.Version != Version {
		return fmt.Errorf("version is not %s", Version)
	}
	if l.Schema != SchemaURI {
		return fmt.Errorf("$schema is not %s", SchemaURI)
	}
	if len(l.Runs) == 0 {
		return errors.New("no runs")
	}
	serialized, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := noLiveKey(serialized); err != nil {
		return err
	}
	seen := map[string]string{}
	for i := range l.Runs {
		run := &l.Runs[i]
		declared := make(map[string]bool, len(run.Tool.Driver.Rules))
		for _, rule := range run.Tool.Driver.Rules {
			declared[rule.ID] = true
		}
		for j := range run.Results {
			r := &run.Results[j]
			for _, s := range r.texts() { // invalid bytes are sanitized to U+FFFD on marshal, so the live-key match below could not see a split key; refuse them outright
				if !utf8.ValidString(s) {
					return fmt.Errorf("run %d result %d: a string is not valid UTF-8", i, j)
				}
			}
			if err := r.validate(declared); err != nil {
				return fmt.Errorf("run %d result %d: %w", i, j, err)
			}
			fp := r.PartialFingerprints.CanyonV1
			if prev, dup := seen[fp]; dup {
				return fmt.Errorf("run %d result %d: fingerprint already used by result %s", i, j, prev)
			}
			seen[fp] = fmt.Sprintf("%d/%d", i, j)
		}
	}
	return nil
}

// ValidateAgainst checks the three things a log cannot be held to without seam 5 in hand
// (contracts/snapshot.md): blocks derived from the rule's state and the finding's severity
// and nowhere else, every ruleId in the snapshot and inside its producer's namespace, and
// policy_name the snapshot's name for that rule. It is separate from Validate because a
// caller without a snapshot — the redaction tests, a log read back off the wire — still has
// a profile to check; a gate has both and runs both. Fails closed like everything else: an
// unknown rule, an unknown producer, or a disagreement is an error. An out-of-namespace result
// is an error on every run: the reviewer drops its own before its log exists (gate/review.go,
// canyonv2-95s), so one reaching here is a producer's bug, the same as contracts/validate.py.
func (l *Log) ValidateAgainst(snap policy.Snapshot) error {
	if l == nil {
		return errors.New("no log")
	}
	// Validate refuses a log with no runs; so must this one, or a caller reaching for the
	// snapshot half alone is told an empty log is fine.
	if len(l.Runs) == 0 {
		return errors.New("no runs")
	}
	rules, err := rulesByID(snap)
	if err != nil {
		return err
	}
	granted, err := agentsByID(snap)
	if err != nil {
		return err
	}
	for i := range l.Runs {
		run := &l.Runs[i]
		// tool.driver.name says which producer a run is; a name that is not one of the three
		// is an agent's run, carrying its snapshot id (contracts/finding.md). Neither means
		// nothing in the snapshot vouches for these rules, so the run fails closed.
		producer, isProducer := policy.ByDriver(run.Tool.Driver.Name)
		grants, isAgent := granted[run.Tool.Driver.Name]
		if !isProducer && !isAgent {
			return fmt.Errorf("run %d: tool.driver.name is neither a producer nor an agent of the snapshot", i)
		}
		for j := range run.Results {
			r := &run.Results[j]
			c := r.Properties.Canyon
			rule, known := ruleOf(rules, run.Tool.Driver.Name, r.RuleID)
			if !known {
				return fmt.Errorf("run %d result %d: ruleId is not in the policy snapshot", i, j)
			}
			// Namespace-confined per producer: a producer's run carries only its own rules, an
			// agent's only the namespaces the snapshot grants it. A name that is both resolves to
			// the producer, as contracts/validate.py does; seam 5's schema keeps agent ids free of
			// the slash every driver name carries, so a valid snapshot cannot hold one.
			var confined bool
			switch {
			case isProducer && producer == policy.Sarif: // sarif.<tool>.<ruleId>, or the table's <policy>.<tool>.<ruleId>
				m, own, mapped := policy.Mapped(strings.TrimPrefix(run.Tool.Driver.Name, "sarif/"), r.RuleID)
				confined = mapped || rule.Producer == policy.Sarif && strings.HasPrefix(r.RuleID, rule.ID+".")
				if mapped && !heldToRow(c, m, own) {
					return fmt.Errorf("run %d result %d: its severity, fix or secret_name is not the policy table's", i, j)
				}
			case isProducer:
				confined = rule.Producer == producer
			case isAgent:
				ns, _, _ := strings.Cut(r.RuleID, ".")
				confined = slices.Contains(grants, ns)
			}
			if !confined {
				return fmt.Errorf("run %d result %d: ruleId is outside the namespace its run may emit", i, j)
			}
			// An unknown severity would compute blocks false and let a log that never ran
			// Validate pass here; the gate fails closed on every path, alone or in sequence.
			if _, err := c.Severity.Level(); err != nil {
				return fmt.Errorf("run %d result %d: %w", i, j, err)
			}
			// A state that is not one of the three is unknown state, and unknown state blocks.
			// Comparing it to Blocking alone would read a typo as "not blocking" and then
			// *require* the finding to carry blocks false — the invariant inside out.
			switch rule.State {
			case policy.Off, policy.Advisory, policy.Blocking:
			default:
				return fmt.Errorf("run %d result %d: the snapshot's state for this rule is not off, advisory or blocking", i, j)
			}
			blocks := Blocks(r.RuleID, rule.State, c.Severity)
			if c.Blocks == nil || *c.Blocks != blocks {
				return fmt.Errorf("run %d result %d: blocks is not the snapshot's state and this severity (%v)", i, j, blocks)
			}
			if blocks { // the one place blocks is final, so the one place the prose it requires can be demanded
				if c.RequiredEndState == "" {
					return fmt.Errorf("run %d result %d: required_end_state is missing on a finding that blocks", i, j)
				}
				if len(c.DoNot) == 0 {
					return fmt.Errorf("run %d result %d: do_not is missing or empty on a finding that blocks", i, j)
				}
			}
			if c.CarriedFrom != "" && !isAgent { // only a review's result is carried (contracts/finding.md)
				return fmt.Errorf("run %d result %d: carried_from is on a result of no agent's run", i, j)
			}
			if c.PolicyName != rule.Name {
				return fmt.Errorf("run %d result %d: policy_name is not the snapshot's name for this rule", i, j)
			}
		}
	}
	return nil
}

// Blocks is contracts/snapshot.md's one formula: state == blocking && severity ≥ high, and never a
// result of a Dockerfile check. Derive, ValidateAgainst, the reviewer's candidates and the promote
// receipt all read it, so none can disagree. It is safe as one because
// TestDeriveOverEveryStateAndSeverity holds the expected cells in its own table: a test that computed
// them through Blocks would make the guard a tautology.
func Blocks(ruleID string, state policy.State, severity Severity) bool {
	return state == policy.Blocking && (severity == Critical || severity == High) && !DockerfileCheck(ruleID)
}

// DockerfileChecks are the id prefixes of checkov's dockerfile checks under iac.misconfig, the ids its
// dockerfile framework gives them (checkov 3.3.19). Canyon builds every app with buildpacks and from
// no Dockerfile, so what one names never runs: its result is an advisory whatever the rule's state, in
// either gate (contracts/snapshot.md, A finding on a Dockerfile never blocks).
var DockerfileChecks = []string{"iac.misconfig.checkov.CKV_DOCKER_", "iac.misconfig.checkov.CKV2_DOCKER_"}

// DockerfileCheck is whether ruleID is one of checkov's dockerfile checks.
func DockerfileCheck(ruleID string) bool {
	return slices.ContainsFunc(DockerfileChecks, func(p string) bool { return strings.HasPrefix(ruleID, p) })
}

// Unreadable is the gate's own rule (contracts/finding.md): a Canyon-run tool whose scan did not
// finish is one finding under it, never an aborted run and never a clean line. It is no policy — no
// snapshot carries it and no edit turns it off — so rulesByID adds it to every snapshot's rules,
// the scanners' and blocking.
var Unreadable = Rule{ID: "gate.unreadable", Name: "Canyon checks every file"}

// rulesByID indexes a snapshot's rules by id and refuses one that carries an id twice. Ranging
// into a map is last-wins, so Derive and ValidateAgainst would each keep whichever copy came
// last, AGREE with each other, and leave Check — the thing that owns the invariant — unable to
// see the disagreement: an off copy of a blocking rule takes a critical finding to blocks false
// and the deploy proceeds. Worse, the placement of the copy would decide the answer, which is a
// precedence rule nothing states. A snapshot that says two things about one rule is unknown
// state, and unknown state blocks. policy.Builder.Build refuses a repeated id, but neither of
// these takes a Build result — they take a Snapshot from any caller, and seam 5's schema has no
// uniqueness constraint to lean on.
func rulesByID(snap policy.Snapshot) (map[string]policy.Rule, error) {
	rules := make(map[string]policy.Rule, len(snap.Rules)+1)
	for _, rule := range append(slices.Clone(snap.Rules), policy.Rule{ID: Unreadable.ID, Name: Unreadable.Name, State: policy.Blocking, Producer: policy.Scanner}) {
		if _, twice := rules[rule.ID]; twice {
			return nil, errors.New("the policy snapshot carries a rule id twice")
		}
		rules[rule.ID] = rule
	}
	return rules, nil
}

// heldToRow reports whether a result the table maps under m carries the row's: its severity where the
// row names one, its fix where the row's is not the tool's, and on a secrets.* policy the secret_name
// the tool's rule id own gives (contracts/snapshot.md, Tool results mapped to a policy).
func heldToRow(c Canyon, m policy.Mapping, own string) bool {
	secret := ""
	if strings.HasPrefix(m.Policy, "secrets.") {
		secret = policy.SecretName(own)
	}
	named := c.SecretName != nil && *c.SecretName == secret
	return (m.Severity == "" || string(c.Severity) == m.Severity) && (m.Fix == "" || c.Fix == strings.ReplaceAll(m.Fix, "<SECRET_NAME>", secret)) &&
		(secret == "" && c.SecretName == nil || secret != "" && named)
}

// ruleOf is the snapshot rule a result in a run under driver reads: its own ruleId, byte-exact,
// except in an imported tool's run, sarif/<tool>, whose results read the tool's one rule,
// sarif.<tool>, or the policy the table maps them under — the contract's one exception
// (contracts/finding.md; contracts/snapshot.md, Tool results mapped to a policy).
func ruleOf(rules map[string]policy.Rule, driver, ruleID string) (policy.Rule, bool) {
	if p, _ := policy.ByDriver(driver); p == policy.Sarif {
		tool := strings.TrimPrefix(driver, "sarif/")
		if m, _, ok := policy.Mapped(tool, ruleID); ok {
			ruleID = m.Policy
		} else {
			ruleID = "sarif." + tool
		}
	}
	rule, known := rules[ruleID]
	return rule, known
}

// agentsByID is the same refusal for snap.Agents, and for the same reason: granted was built by
// ranging them, last-wins, so a second copy of an agent silently replaces its namespace list and
// the placement of the copy decides the grant. Confinement is the only thing tying an agent run's
// rule ids to what the snapshot lets it emit, so a widened copy hands the agent another producer's
// namespaces — and an agent supplies severity too, so it can then put a blocking rule at low.
func agentsByID(snap policy.Snapshot) (map[string][]string, error) {
	granted := make(map[string][]string, len(snap.Agents))
	for _, a := range snap.Agents {
		if _, twice := granted[a.ID]; twice {
			return nil, errors.New("the policy snapshot names an agent twice")
		}
		granted[a.ID] = a.Namespace
	}
	return granted, nil
}

// Derive writes canyon.blocks on every finding from the policy snapshot, overwriting whatever
// a producer wrote: contracts/finding.md says producers cannot set it and the server derives it
// after policy lookup, and contracts/snapshot.md says it is `state == blocking && severity ≥
// high` — nowhere else. It is ValidateAgainst's sibling and says the same thing the other way
// round: Derive writes what ValidateAgainst then refuses to disagree with. Nothing here enforces
// that order — the two are separate exported calls — and the two directions are not symmetric.
// Check without Derive is safe: ValidateAgainst refuses a log whose blocks disagrees with the
// snapshot, so an unjudged log fails closed. Derive without Check is the hazardous one, because
// it writes an authoritative blocks onto a log that has passed neither schema nor Validate nor
// the checks only ValidateAgainst makes, and that value then looks server-derived to every
// consumer downstream. canyonv2-amj wires the two together behind the gate's one entry point;
// until it lands, a caller that reaches for Derive alone is the thing to watch for in review.
//
// Every result is judged before any is written, so a call that returns an error wrote nothing at
// all and no log is left half-derived. That guarantee is per call and no wider: a log already
// derived against one snapshot keeps those answers when a later Derive refuses it, so a caller
// that ignores errors across two calls still ships the earlier snapshot's verdict — Check against
// the intended snapshot is what catches that, which is the other half of why the two run together.
//
// It fails closed on the three things the derivation itself reads: an unknown rule, an unknown
// severity, or a state outside the three is an error. It is not all of ValidateAgainst's guards,
// and the rest are deliberately left there rather than said twice — namespace confinement, the
// driver vouching that says a run is a producer's or an agent's, and policy_name. So Derive will judge a result from a run nothing in the
// snapshot vouches for, and Check then refuses the log: derived, then refused, in that order.
//
// What it removes from a producer is the blocks bit, not the verdict. blocks is a function of the
// snapshot's state and the producer's own severity, and severity is judged only for membership in
// the four — never for truthfulness. A producer that relabels a critical finding medium still
// gets blocks false out of Derive and past Check. That is the contract's trust boundary, not a
// gap here: canyon.severity is producer-set and canonical, in-tree scanners are trusted, and an
// imported SARIF rule installs Off. Derive makes blocks the server's; it does not make a lying
// producer safe.
//
// An absent blocks is filled rather than refused: the contract says the server overwrites
// whatever a producer wrote, and nothing written is a special case of whatever. Validate's
// "blocks is missing" guard stays live for every caller that has not derived. Each result gets a
// fresh pointer, so a Result copied elsewhere that shares a producer's *bool is never mutated
// underneath — but note a Log is a header over shared backing arrays, so deriving a *copy* of a
// log still writes through to the original's results; give each snapshot its own decode.
func Derive(l *Log, snap policy.Snapshot) error {
	if l == nil {
		return errors.New("no log")
	}
	// Validate and ValidateAgainst both refuse a log with no runs; so must this, or a gate that
	// derives before it checks is told an empty log was judged.
	if len(l.Runs) == 0 {
		return errors.New("no runs")
	}
	rules, err := rulesByID(snap)
	if err != nil {
		return err
	}
	blocks := make([][]bool, len(l.Runs))
	for i := range l.Runs {
		run := &l.Runs[i]
		blocks[i] = make([]bool, len(run.Results))
		for j := range run.Results {
			c := run.Results[j].Properties.Canyon
			rule, known := ruleOf(rules, run.Tool.Driver.Name, run.Results[j].RuleID)
			if !known {
				return fmt.Errorf("run %d result %d: ruleId is not in the policy snapshot", i, j)
			}
			// An unknown severity would derive blocks false and ship a finding nobody judged.
			if _, err := c.Severity.Level(); err != nil {
				return fmt.Errorf("run %d result %d: %w", i, j, err)
			}
			// A state outside the three is unknown state, and unknown state blocks. Comparing it
			// to Blocking alone would read a typo as "not blocking" and derive false from it.
			switch rule.State {
			case policy.Off, policy.Advisory, policy.Blocking:
			default:
				return fmt.Errorf("run %d result %d: the snapshot's state for this rule is not off, advisory or blocking", i, j)
			}
			blocks[i][j] = Blocks(run.Results[j].RuleID, rule.State, c.Severity)
		}
	}
	for i := range l.Runs {
		for j := range l.Runs[i].Results {
			l.Runs[i].Results[j].Properties.Canyon.Blocks = &blocks[i][j]
		}
	}
	return nil
}

// Check is seam 1 whole: the two schemas on the serialized bytes, then the invariants no
// schema can state, then the three the policy snapshot owns. Each step fails closed on its
// own, so the order only decides which error a log with several faults reports. Neither half is safe alone — Validate never sees a rule's state, and
// ValidateAgainst never sees the schemas' shape — so a caller holding a snapshot reaches for
// this rather than for the parts, and a log that skipped either is a log nobody has checked.
func Check(l *Log, snap policy.Snapshot) error {
	if l == nil {
		return errors.New("no log")
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := contracts.CheckFindingSchemas(raw); err != nil {
		return err
	}
	if err := l.Validate(); err != nil {
		return err
	}
	return l.ValidateAgainst(snap)
}

func (r *Result) validate(declared map[string]bool) error {
	c := r.Properties.Canyon
	if !declared[r.RuleID] {
		return errors.New("ruleId is not declared in tool.driver.rules")
	}
	level, err := c.Severity.Level()
	if err != nil {
		return err
	}
	if r.Level != level {
		return fmt.Errorf("level is not the projection of severity (%s)", level)
	}
	if c.Blocks == nil {
		return errors.New("blocks is missing")
	}
	if r.Message.Text != c.Title {
		return errors.New("message.text differs from canyon.title")
	}
	for _, loc := range append(r.Locations, r.RelatedLocations...) {
		if err := loc.validate(); err != nil {
			return err
		}
		if r.RuleID == Unreadable.ID && !unreadableSnippet.MatchString(loc.PhysicalLocation.Region.Snippet.Text) {
			return errors.New("a gate.unreadable snippet is not a tool's tree digest")
		}
	}
	if len(r.Locations) > 0 { // the caret a renderer draws: shown when set, else the region's own columns
		g := r.Locations[0].PhysicalLocation.Region
		at := Shown{StartColumn: g.StartColumn, EndColumn: g.EndColumn}
		if c.Shown != nil {
			at = *c.Shown
		}
		ls := snippetLines(g.Snippet.Text)
		first, last := utf8.RuneCountInString(ls[0]), utf8.RuneCountInString(ls[len(ls)-1])
		if at.StartColumn < 1 || at.EndColumn < 1 || at.StartColumn > first+1 || at.EndColumn > last+1 || (len(ls) == 1 && at.EndColumn < at.StartColumn) {
			return errors.New("the caret is outside the snippet it is drawn on")
		}
	}
	if c.CarriedFrom != "" && !runID.MatchString(c.CarriedFrom) {
		return errors.New("carried_from is not a run id")
	}
	if c.SecretName != nil {
		if *c.SecretName == "" {
			return errors.New("secret_name is present but empty")
		}
		if !strings.HasPrefix(r.RuleID, "secrets.") {
			return errors.New("secret_name only belongs on secrets.* findings")
		}
	}
	for _, entry := range c.DoNot { // lexical, so true at construction; whether the list is required at all is the snapshot's call
		if entry == "" || strings.ContainsAny(entry, terminators) {
			return errors.New("a do_not entry is empty or carries a line break")
		}
	}
	if c.DoNot != nil && len(c.DoNot) == 0 { // an empty list is never the shape; omitempty would hide it from the schema
		return errors.New("do_not is an empty list")
	}
	return nil
}

func (loc Location) validate() error {
	g := loc.PhysicalLocation.Region
	if g.EndLine < g.StartLine || (g.EndLine == g.StartLine && g.EndColumn <= g.StartColumn) {
		return errors.New("a region ends before it starts")
	}
	if lines(g.Snippet.Text) > MaxSnippetLines {
		return fmt.Errorf("a snippet is over %d lines", MaxSnippetLines)
	}
	return nil
}

// lines counts a snippet's lines; a trailing terminator ends the last line rather than starting
// another. It counts every terminator mask() treats as one — LF, CR, CRLF, U+2028 and U+2029 —
// because a snippet the mask hides whole must not pass the cap by carrying a separator the count
// does not know: a consumer honouring U+2028 renders it over more lines than the cap allows.
// contracts/validate.py's snippet_lines is the same function and the two must agree.
func lines(s string) int { return len(snippetLines(s)) }

// snippetLines splits a snippet into its lines as lines counts them.
func snippetLines(s string) []string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(s)
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
