package finding

import (
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/policy"
)

// goldenLog is the log these tests decode and mutate: two scanner results, the hero secret first,
// and the reviewer's recorded AI result — every shape the profile judges. The claims-portal log the
// product produces is contracts/testdata/finding/claims-portal.sarif.json.
const goldenLog = "../testdata/review/claims-portal.sarif.json"

func readGolden(t *testing.T) ([]byte, Log) {
	t.Helper()
	raw, err := os.ReadFile(goldenLog)
	if err != nil {
		t.Fatal(err)
	}
	var log Log
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&log); err != nil {
		t.Fatalf("the golden log has a field these types do not: %v", err)
	}
	return raw, log
}

// The Go types cover the golden log field for field: it decodes with unknown fields
// refused, validates, and encodes back to the same document. A field the profile does
// not have is refused the same way.
func TestGoldenLogRoundTrips(t *testing.T) {
	raw, log := readGolden(t)
	if err := log.Validate(); err != nil {
		t.Fatalf("golden log: %v", err)
	}
	out, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("re-encoded log differs from the golden log:\n%s", out)
	}
	extra := bytes.Replace(raw, []byte(`"severity": "critical",`), []byte(`"severity": "critical", "score": 9,`), 1)
	dec := json.NewDecoder(bytes.NewReader(extra))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&log); err == nil {
		t.Fatal("decoded a canyon bag with a field the profile does not have")
	}
}

// Every severity projects onto its level and nothing else does.
func TestSeverityProjectsOntoLevel(t *testing.T) {
	for sev, want := range map[Severity]string{Critical: "error", High: "error", Medium: "warning", Low: "note", "urgent": ""} {
		if got, err := sev.Level(); got != want || (err == nil) != (want != "") {
			t.Errorf("%s.Level() = %q, %v; want %q", sev, got, err, want)
		}
	}
}

// A log that breaks a rule no schema can state fails closed — never dropped, never
// defaulted — and the error never repeats what the log said.
func TestValidateFailsClosed(t *testing.T) {
	empty, key := "", stripeKey("sk", "Q1")
	cases := map[string]func(*Log){
		"wrong version": func(l *Log) { l.Version = "2.0.0" },
		"wrong $schema": func(l *Log) { l.Schema = "https://example.com/other.json" },
		"no runs":       func(l *Log) { l.Runs = nil },
		"undeclared rule": func(l *Log) {
			l.Runs[0].Tool.Driver.Rules = slices.DeleteFunc(l.Runs[0].Tool.Driver.Rules, func(r Rule) bool { return r.ID == "secrets.client-bundle" })
		},
		"unknown severity":         func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.Severity = Severity(key) },
		"level not the projection": func(l *Log) { l.Runs[0].Results[0].Level = "note" },
		"low is not a warning":     func(l *Log) { l.Runs[1].Results[0].Properties.Canyon.Severity = Low },
		"blocks absent":            func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.Blocks = nil },
		"title differs from text":  func(l *Log) { l.Runs[0].Results[0].Message.Text = "something else" },
		"region ends before start": func(l *Log) { l.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.EndLine = 3 },
		"region ends at its start": func(l *Log) { l.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.EndColumn = 27 },
		"related region ends early": func(l *Log) {
			r := &l.Runs[1].Results[0]
			r.RelatedLocations = append(r.RelatedLocations, r.Locations[0])
			r.RelatedLocations[0].PhysicalLocation.Region.EndColumn = 1
		},
		"snippet over 3 lines":    func(l *Log) { l.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text = "a\nb\nc\nd" },
		"snippet over 3 CR lines": func(l *Log) { l.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text = "a\rb\rc\rd" },
		"shown past its snippet":  func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.Shown = &Shown{27, 200} },
		"masked, shown dropped":   func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.Shown = nil },
		"duplicate fingerprint": func(l *Log) {
			l.Runs[1].Results[0].PartialFingerprints.CanyonV1 = l.Runs[0].Results[0].PartialFingerprints.CanyonV1
		},
		"secret_name off secrets.*": func(l *Log) {
			l.Runs[1].Results[0].Properties.Canyon.SecretName = l.Runs[0].Results[0].Properties.Canyon.SecretName
		},
		"secret_name present, empty": func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.SecretName = &empty },
		"live key in a snippet":      func(l *Log) { l.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text = key },
		"live key in a problem":      func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.Problem = "the key " + key },
		"live key where a name goes": func(l *Log) { l.Runs[0].Results[0].Properties.Canyon.SecretName = &key },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, log := readGolden(t)
			mutate(&log)
			err := log.Validate()
			if err == nil {
				t.Fatal("validated a log that breaks the profile")
			}
			if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "something else") {
				t.Fatalf("the error repeats the log's content: %v", err)
			}
		})
	}
}

func canonicalSnapshot(t *testing.T) policy.Snapshot {
	t.Helper()
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// withState is the canonical snapshot with one rule's state changed.
func withState(t *testing.T, id string, state policy.State) policy.Snapshot {
	t.Helper()
	snap := canonicalSnapshot(t)
	for i := range snap.Rules {
		if snap.Rules[i].ID == id {
			snap.Rules[i].State = state
		}
	}
	return snap
}

// producerRun is a run of p carrying one finding of the rule id, named as the snapshot names it.
func producerRun(t *testing.T, snap policy.Snapshot, p policy.Producer, id string, sev Severity, blocks bool) Run {
	t.Helper()
	_, golden := readGolden(t)
	run := golden.Runs[0]
	run.Tool.Driver.Name = p.Driver()
	r := run.Results[0]
	r.RuleID, r.Properties.Canyon.Severity, r.Properties.Canyon.Blocks = id, sev, &blocks
	for _, rule := range snap.Rules {
		if rule.ID == id {
			r.Properties.Canyon.PolicyName = rule.Name
		}
	}
	run.Results = []Result{r}
	return run
}

// The three checks that need seam 5 in hand (contracts/snapshot.md). The golden log passes
// against the canonical snapshot: a scanner run carrying scanner rules, an agent run inside
// the one namespace the snapshot grants it, blocks derived from the rule's state and the
// finding's severity, and policy_name the snapshot's own name for that rule.
func TestValidateAgainstTheSnapshot(t *testing.T) {
	snap := canonicalSnapshot(t)
	yes, no := true, false
	_, golden := readGolden(t)
	if err := golden.ValidateAgainst(snap); err != nil {
		t.Fatalf("the golden log against the canonical snapshot: %v", err)
	}
	// A blocking rule below high blocks nothing — the cell the golden never visits. Both
	// directions, since only the pair pins the threshold rather than one side of it.
	_, below := readGolden(t)
	below.Runs[0].Results[0].Properties.Canyon.Severity = Medium
	below.Runs[0].Results[0].Properties.Canyon.Blocks = &no
	if err := below.ValidateAgainst(snap); err != nil {
		t.Fatalf("a blocking rule at medium severity does not block: %v", err)
	}
	// ...and at high it does. The golden's high finding sits on an advisory rule, so without this
	// a threshold moved up to critical would pass everything.
	_, high := readGolden(t)
	high.Runs[0].Results[0].Properties.Canyon.Severity = High
	if err := high.ValidateAgainst(snap); err != nil {
		t.Fatalf("a blocking rule at high severity blocks: %v", err)
	}
	// An off rule blocks nothing, even at critical.
	_, off := readGolden(t)
	off.Runs[0].Results[0].Properties.Canyon.Blocks = &no
	if err := off.ValidateAgainst(withState(t, "secrets.client-bundle", policy.Off)); err != nil {
		t.Fatalf("an off rule at critical severity does not block: %v", err)
	}
	// The two producers the golden has no run for answer for their own rules the same way.
	others := Log{Version: Version, Schema: SchemaURI, Runs: []Run{
		producerRun(t, snap, policy.ServerState, "owner.required", High, false),
		producerRun(t, snap, policy.PromoteGate, "prod.clean-gate", Critical, true),
	}}
	if err := others.ValidateAgainst(snap); err != nil {
		t.Fatalf("server-state and promote-gate runs carrying their own rules: %v", err)
	}
	// Each case breaks exactly one guard, and want names which one. Asserting only that some
	// error came back cannot tell "the guard under test fired" from "a different guard caught
	// it first" — which is exactly how an unisolated guard hides behind its neighbour.
	cases := map[string]struct {
		mutate func(*Log)
		want   string
	}{
		"blocks true on an advisory rule": {func(l *Log) {
			l.Runs[1].Results[0].Properties.Canyon.Blocks = &yes
		}, "blocks is not the snapshot's"},
		"blocks false on a blocking critical": {func(l *Log) {
			l.Runs[0].Results[0].Properties.Canyon.Blocks = &no
		}, "blocks is not the snapshot's"},
		"blocks true on a blocking rule below high": {func(l *Log) {
			l.Runs[0].Results[0].Properties.Canyon.Severity = Medium
			l.Runs[0].Results[0].Properties.Canyon.Blocks = &yes
		}, "blocks is not the snapshot's"},
		// Isolated to membership on purpose: an agent run carrying an unknown rule *inside* the
		// namespace it is granted, with blocks and policy_name matching what a zero rule implies.
		// On a producer run an unknown rule is caught by the namespace check instead, since a
		// zero-value rule's producer matches nothing — which would leave "is this rule in the
		// snapshot at all" looking redundant when it is the only thing standing here.
		"a rule the snapshot has never heard of": {func(l *Log) {
			l.Runs[1].Results[0].RuleID = "perf.invented-rule"
			l.Runs[1].Results[0].Properties.Canyon.PolicyName = ""
		}, "not in the policy snapshot"},
		// Namespace-confined per producer: perf.* is llm-review's, so a canyon/scanners run may
		// not carry it even though the rule is in the snapshot.
		"a scanner run carrying perf.*": {func(l *Log) {
			l.Runs[0].Results[0].RuleID, l.Runs[0].Results[0].Properties.Canyon.SecretName = "perf.unbounded-refetch", nil
			l.Runs[0].Results[0].Properties.Canyon.PolicyName = "No unbounded refetch on user input"
		}, "outside the namespace"},
		// ...and an agent may emit only the namespaces the snapshot grants it: canyon-review is
		// granted perf and auth, so sql.* is outside it.
		"the agent emitting outside its grant": {func(l *Log) {
			l.Runs[1].Results[0].RuleID = "sql.string-concat"
			l.Runs[1].Results[0].Properties.Canyon.PolicyName = "No hand-built SQL from user input"
		}, "outside the namespace"},
		"policy_name differs from the snapshot's": {func(l *Log) {
			l.Runs[0].Results[0].Properties.Canyon.PolicyName = "No secrets in the code-7f3a"
		}, "policy_name is not the snapshot's"},
		// Byte for byte, not case-folded.
		"policy_name differs only in case": {func(l *Log) {
			l.Runs[0].Results[0].Properties.Canyon.PolicyName = strings.ToUpper(l.Runs[0].Results[0].Properties.Canyon.PolicyName)
		}, "policy_name is not the snapshot's"},
		// secrets.* is the scanner's, so a server-state run may not carry it.
		"a server-state run carrying a scanner's rule": {func(l *Log) {
			l.Runs[0].Tool.Driver.Name = policy.ServerState.Driver()
		}, "outside the namespace"},
		"a driver that is neither a producer nor an agent": {func(l *Log) {
			l.Runs[0].Tool.Driver.Name = "canyon/invented-7f3a"
		}, "neither a producer nor an agent"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, log := readGolden(t)
			c.mutate(&log)
			err := log.ValidateAgainst(snap)
			if err == nil {
				t.Fatal("validated a log the policy snapshot refuses")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("a different guard fired: got %q, want one naming %q", err, c.want)
			}
			if strings.Contains(err.Error(), "7f3a") {
				t.Fatalf("the error repeats the log's content: %v", err)
			}
		})
	}
}

// Check runs all three steps, so it catches what any one of them catches: a shape only the
// schemas refuse, an invariant only Validate sees, and a disagreement only the snapshot can
// see. Each case below is refused by exactly one of the three and want names that step's
// error, so a Check that quietly dropped a step goes red on that step's case.
// The governance prose is demanded where blocks is final (ValidateAgainst) and shaped where it
// is lexical (Validate): each case names the field it refuses. The golden's two advisory
// findings carry neither and validate, which is the permitted-not-required half.
func TestABlockingFindingCarriesItsGovernanceProse(t *testing.T) {
	snap := canonicalSnapshot(t)
	for name, c := range map[string]struct {
		mutate func(*Canyon)
		want   string
	}{
		"required_end_state removed":       {func(c *Canyon) { c.RequiredEndState = "" }, "required_end_state"},
		"do_not removed":                   {func(c *Canyon) { c.DoNot = nil }, "do_not"},
		"do_not empty":                     {func(c *Canyon) { c.DoNot = []string{} }, "do_not"},
		"a do_not entry with a line break": {func(c *Canyon) { c.DoNot[0] += "\nor the reverse" }, "do_not"},
		"a do_not entry with U+2028":       {func(c *Canyon) { c.DoNot[0] += "\u2028or the reverse" }, "do_not"},
		"do_not an empty list, not blocking": {func(c *Canyon) {
			c.RequiredEndState, c.DoNot = "", []string{}
		}, "do_not"},
	} {
		t.Run(name, func(t *testing.T) {
			_, log := readGolden(t)
			c.mutate(&log.Runs[0].Results[0].Properties.Canyon)
			err := cmp.Or(log.Validate(), log.ValidateAgainst(snap))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
	_, log := readGolden(t)
	blocking := log.Runs[0].Results[0].Properties.Canyon
	log.Runs[1].Results[0].Properties.Canyon.RequiredEndState, log.Runs[1].Results[0].Properties.Canyon.DoNot = blocking.RequiredEndState, blocking.DoNot
	if err := cmp.Or(log.Validate(), log.ValidateAgainst(snap)); err != nil {
		t.Fatalf("the prose on an advisory finding is permitted, never forbidden: %v", err)
	}
}

func TestCheckRunsAllThreeSteps(t *testing.T) {
	snap := canonicalSnapshot(t)
	_, golden := readGolden(t)
	if err := Check(&golden, snap); err != nil {
		t.Fatalf("the golden log: %v", err)
	}
	if err := Check(nil, snap); err == nil || err.Error() != "no log" {
		t.Errorf("a nil log: %v", err)
	}
	no := false
	for name, c := range map[string]struct {
		mutate func(*Log)
		want   string
	}{
		// The schemas own the lexical half: canyon/v1's pattern is theirs, and Validate only
		// ever checks fingerprints for uniqueness.
		"a shape only the schemas refuse": {func(l *Log) {
			l.Runs[0].Results[0].PartialFingerprints.CanyonV1 = "f_short"
		}, "finding log"},
		// Validate owns what no schema can say: message.text and canyon.title are one string.
		"an invariant only Validate sees": {func(l *Log) {
			l.Runs[0].Results[0].Message.Text = "a title the bag does not carry"
		}, "message.text differs from canyon.title"},
		// The snapshot owns blocks: nothing without it can tell true from false here.
		"a disagreement only the snapshot sees": {func(l *Log) {
			l.Runs[0].Results[0].Properties.Canyon.Blocks = &no
		}, "blocks is not the snapshot's"},
	} {
		t.Run(name, func(t *testing.T) {
			_, log := readGolden(t)
			c.mutate(&log)
			if err := Check(&log, snap); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want the step whose error names %q", err, c.want)
			}
		})
	}
}

// ValidateAgainst is reachable on its own, so it fails closed on its own. Each case here needs
// a log or a snapshot the golden cannot express, and each isolates a guard that the
// golden-mutation table above cannot reach.
func TestValidateAgainstFailsClosedAlone(t *testing.T) {
	snap := canonicalSnapshot(t)
	refused := func(what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want the guard whose error names %q", what, err, want)
		}
	}
	refused("a nil log", (*Log)(nil).ValidateAgainst(snap), "no log")
	// Validate refuses a log with no runs; alone, this must too.
	refused("a log with no runs", (&Log{}).ValidateAgainst(snap), "no runs")
	// An unrecognised driver on a run carrying no results: the per-result loop never executes,
	// so the driver guard is the only thing that can refuse it. With a populated run the
	// namespace check catches the same mutation first, and the guard looks redundant.
	empty := &Log{Version: Version, Schema: SchemaURI, Runs: []Run{
		{Tool: Tool{Driver: Driver{Name: "canyon/invented"}}, Results: []Result{}},
	}}
	refused("a run whose driver is in neither table", empty.ValidateAgainst(snap), "neither a producer nor an agent")
	// A name that is both a producer's driver and an agent's id resolves to the producer, as
	// contracts/validate.py does. The agent branch reads a namespace prefix and never the rule's
	// producer, so taking it would let a canyon/scanners run carry llm-review's rules. Seam 5's
	// schema cannot hold this snapshot; a Go value can.
	collided := canonicalSnapshot(t)
	collided.Agents = append(collided.Agents, policy.Agent{ID: policy.Scanner.Driver(), Namespace: []string{"perf"}, Model: "m"})
	_, log := readGolden(t)
	log.Runs[0].Results = log.Runs[1].Results // the reviewer's perf.* finding, alone: the secrets.* one is outside the grant too, and would hide which branch ran
	refused("a scanner run carrying perf.* beside an agent of the same name", log.ValidateAgainst(collided), "outside the namespace")
	// An unknown severity, with Validate never having run, on the advisory finding: blocks false
	// is what a bare comparison computes there, so no other guard can fire in its place.
	_, sev := readGolden(t)
	sev.Runs[1].Results[0].Properties.Canyon.Severity = Severity("urgent")
	refused("an unknown severity on an advisory rule", sev.ValidateAgainst(snap), "severity is not one of")
	// A state outside the three reads as "not blocking" under a bare == comparison, and then
	// requires the finding to carry blocks false — the invariant inside out. blocks carries what
	// that comparison would compute, so the blocks guard cannot fire first and hide this one.
	no := false
	_, st := readGolden(t)
	st.Runs[0].Results[0].Properties.Canyon.Blocks = &no
	refused("a rule state outside the three", st.ValidateAgainst(withState(t, "secrets.client-bundle", policy.State("Blocking"))), "not off, advisory or blocking")
}

// Derive writes what ValidateAgainst then refuses to disagree with. The golden log's blocks are
// already what the canonical snapshot implies, so a log whose every blocks is the opposite comes
// back from Derive equal to the golden's — and passes the check that owns the invariant.
func TestDeriveWritesBlocksFromTheSnapshot(t *testing.T) {
	snap := canonicalSnapshot(t)
	_, want := readGolden(t)
	_, log := readGolden(t)
	for i := range log.Runs {
		for j := range log.Runs[i].Results {
			wrong := !*log.Runs[i].Results[j].Properties.Canyon.Blocks
			log.Runs[i].Results[j].Properties.Canyon.Blocks = &wrong // a producer's claim, which cannot stand
		}
	}
	if err := Derive(&log, snap); err != nil {
		t.Fatal(err)
	}
	for i := range log.Runs {
		for j := range log.Runs[i].Results {
			got := *log.Runs[i].Results[j].Properties.Canyon.Blocks
			if w := *want.Runs[i].Results[j].Properties.Canyon.Blocks; got != w {
				t.Fatalf("run %d result %d: derived blocks %v, the golden says %v", i, j, got, w)
			}
		}
	}
	if err := log.ValidateAgainst(snap); err != nil {
		t.Fatalf("a derived log disagrees with the snapshot that derived it: %v", err)
	}
	// Off is not blocking: the same critical finding under a rule nobody turned on does not block.
	_, off := readGolden(t)
	if err := Derive(&off, withState(t, "secrets.client-bundle", policy.Off)); err != nil {
		t.Fatal(err)
	}
	if *off.Runs[0].Results[0].Properties.Canyon.Blocks {
		t.Fatal("a critical finding under an off rule blocks")
	}
}

// Derive fails closed where ValidateAgainst does, and writes nothing at all when it does: a log
// it refused is never half-derived, or a caller that ignored the error ships some findings judged
// and some not.
func TestDeriveFailsClosed(t *testing.T) {
	snap := canonicalSnapshot(t)
	refused := func(what string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want the guard whose error names %q", what, err, want)
		}
	}
	refused("a nil log", Derive(nil, snap), "no log")
	// Validate and ValidateAgainst both refuse a log with no runs; so must this.
	refused("a log with no runs", Derive(&Log{}, snap), "no runs")

	_, unknown := readGolden(t)
	unknown.Runs[0].Results[0].RuleID = "secrets.not-a-rule"
	refused("a ruleId the snapshot does not carry", Derive(&unknown, snap), "not in the policy snapshot")

	_, sev := readGolden(t)
	sev.Runs[1].Results[0].Properties.Canyon.Severity = Severity("urgent")
	refused("an unknown severity", Derive(&sev, snap), "severity is not one of")

	_, st := readGolden(t)
	refused("a rule state outside the three", Derive(&st, withState(t, "secrets.client-bundle", policy.State("Blocking"))),
		"not off, advisory or blocking")

	// The scanners' result is derivable and the reviewer's is not, so a Derive that wrote as it walked would leave
	// the first judged by this snapshot and the second carrying whatever a producer wrote.
	_, half := readGolden(t)
	no := false
	half.Runs[0].Results[0].Properties.Canyon.Blocks = &no // the wrong answer for a blocking rule at critical
	half.Runs[1].Results[0].Properties.Canyon.Severity = Severity("urgent")
	if err := Derive(&half, snap); err == nil {
		t.Fatal("derived a log with an unknown severity in it")
	}
	if *half.Runs[0].Results[0].Properties.Canyon.Blocks {
		t.Fatal("a refused Derive wrote to the log: the scanners' result was judged and the reviewer's was not")
	}
}

// A snapshot carrying one rule id twice says two things about that rule. The map Derive and
// ValidateAgainst each build from snap.Rules is last-wins, so the two agree on whichever copy
// came last and Check — the thing that owns the invariant — cannot see the disagreement: an off
// copy of a blocking rule appended to the canonical snapshot takes the golden's critical finding
// to blocks false, and the placement of the copy decides the answer. Unknown state blocks, and a
// snapshot that contradicts itself is unknown state. Build refuses a repeated id, but neither of
// these takes a Build result — they take a Snapshot from any caller, and seam 5's schema has no
// uniqueness constraint (canyonv2-v9t).
func TestASnapshotThatSaysTwoThingsIsRefused(t *testing.T) {
	twice := func(t *testing.T, first bool) policy.Snapshot {
		t.Helper()
		snap := canonicalSnapshot(t)
		dup := policy.Rule{ID: "secrets.client-bundle", Name: "No secrets in the code", State: policy.Off, Producer: policy.Scanner}
		if first {
			snap.Rules = append([]policy.Rule{dup}, snap.Rules...)
			return snap
		}
		snap.Rules = append(snap.Rules, dup)
		return snap
	}
	for _, first := range []bool{false, true} {
		_, log := readGolden(t)
		snap := twice(t, first)
		if err := Derive(&log, snap); err == nil {
			t.Errorf("copy first=%v: derived against a snapshot carrying one rule id twice; blocks came back %v",
				first, *log.Runs[0].Results[0].Properties.Canyon.Blocks)
		}
		if err := log.ValidateAgainst(snap); err == nil {
			t.Errorf("copy first=%v: validated against a snapshot carrying one rule id twice", first)
		}
	}
}

// An agent id repeats the same way, one loop above: granted was built by ranging snap.Agents,
// last-wins, exactly as the rule map was. Namespace confinement is the only thing tying an
// agent run's rule ids to what the snapshot lets it emit, so a second, wider copy of an agent
// hands it another producer's namespaces — and since an agent supplies severity too, it can then
// put a blocking rule at low and derive blocks false. Seam 5's schema constrains an agent id's
// shape and never its uniqueness, and contracts/validate.py refuses a repeated rule id but not a
// repeated agent id (canyonv2-v9t). Placement must not decide a grant.
func TestASnapshotThatSaysTwoThingsAboutAnAgentIsRefused(t *testing.T) {
	twice := func(t *testing.T, first bool) policy.Snapshot {
		t.Helper()
		snap := canonicalSnapshot(t)
		wide := snap.Agents[0]
		wide.Namespace = []string{"perf", "secrets", "sql"}
		for i := range snap.Agents {
			snap.Agents[i].Namespace = []string{"nothing"}
		}
		if first {
			snap.Agents = append([]policy.Agent{wide}, snap.Agents...)
			return snap
		}
		snap.Agents = append(snap.Agents, wide)
		return snap
	}
	for _, first := range []bool{false, true} {
		_, log := readGolden(t)
		snap := twice(t, first)
		if err := log.ValidateAgainst(snap); err == nil {
			t.Errorf("copy first=%v: validated against a snapshot naming one agent twice", first)
		}
	}
}

// A producer that omits blocks entirely is repaired rather than refused: contracts/finding.md
// says the server overwrites whatever a producer wrote, and nothing written is a special case of
// whatever. The same file says a missing required field is never defaulted, so a reader can reach
// either conclusion; this pins the one Derive takes. Validate's guard stays live for every caller
// that has not derived.
func TestDeriveFillsAnAbsentBlocks(t *testing.T) {
	snap := canonicalSnapshot(t)
	_, log := readGolden(t)
	log.Runs[0].Results[0].Properties.Canyon.Blocks = nil // a producer that wrote nothing at all
	if err := Derive(&log, snap); err != nil {
		t.Fatal(err)
	}
	if b := log.Runs[0].Results[0].Properties.Canyon.Blocks; b == nil || !*b {
		t.Fatalf("an absent blocks on a critical finding under a blocking rule derived %v", b)
	}
	_, bare := readGolden(t)
	bare.Runs[0].Results[0].Properties.Canyon.Blocks = nil
	if err := bare.Validate(); err == nil {
		t.Fatal("Validate accepted a finding carrying no blocks; only Derive may fill one")
	}
}

// The formula is `state == blocking && severity >= high` (contracts/snapshot.md) and the fixtures
// alone do not pin it: the golden carries a blocking rule only at critical, so deleting the high
// half of the disjunct, or moving the threshold up, passes every other test in this file. The
// whole cross-product against the rule written out once more, and the derived log agreeing with
// the verifier that owns the same invariant.
func TestDeriveOverEveryStateAndSeverity(t *testing.T) {
	for _, state := range []policy.State{policy.Off, policy.Advisory, policy.Blocking} {
		for _, sev := range []Severity{Critical, High, Medium, Low} {
			t.Run(string(state)+"/"+string(sev), func(t *testing.T) {
				snap := withState(t, "secrets.client-bundle", state)
				_, log := readGolden(t)
				log.Runs[0].Results[0].Properties.Canyon.Severity = sev
				if err := Derive(&log, snap); err != nil {
					t.Fatal(err)
				}
				want := state == policy.Blocking && (sev == Critical || sev == High)
				if got := *log.Runs[0].Results[0].Properties.Canyon.Blocks; got != want {
					t.Fatalf("a %s finding under a %s rule derived blocks %v; the rule makes it %v", sev, state, got, want)
				}
				if err := log.ValidateAgainst(snap); err != nil {
					t.Fatalf("a derived log disagrees with the snapshot that derived it: %v", err)
				}
			})
		}
	}
}

// What the schemas own is not re-checked here, and what they leave optional stays optional:
// a log without relatedLocations validates, a snippet that ends in a newline is not four lines,
// and a nil log is an error rather than a panic.
func TestValidateLeavesTheSchemasTheirJob(t *testing.T) {
	_, log := readGolden(t)
	log.Runs[0].Results[0].RelatedLocations = nil
	log.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.Snippet.Text = "a\nb\nc\n"
	log.Runs[0].Results[0].Properties.Canyon.Shown = &Shown{StartColumn: 1, EndColumn: 2} // the caret on the new snippet
	if err := log.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (*Log)(nil).Validate(); err == nil {
		t.Fatal("a nil log validated")
	}
}

// texts is what New's live-key safety net and Validate's UTF-8 check walk: a field left out of it
// is a field those two never see. Both new strings are in it.
func TestTextsCarriesTheGovernanceProse(t *testing.T) {
	_, log := readGolden(t)
	r := log.Runs[0].Results[0]
	texts := strings.Join(r.texts(), "\n")
	for _, want := range append([]string{r.Properties.Canyon.RequiredEndState}, r.Properties.Canyon.DoNot...) {
		if !strings.Contains(texts, want) {
			t.Fatalf("texts() omits %q", want)
		}
	}
}
