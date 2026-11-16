package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/contracts"
)

// The canonical rules and every default are the fixture's, which contracts/validate.py
// holds to EXPERIENCE D2 byte for byte. A rule added to the table instead of registered — at
// any state — fails here.
func TestTheCanonicalRulesAndTheDefaultsAreTheFixture(t *testing.T) {
	var want Snapshot
	dec := json.NewDecoder(bytes.NewReader(contracts.Snapshot))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&want); err != nil {
		t.Fatal(err)
	}
	got, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's version and hash are illustrative; the content is what must match.
	want.Version, want.Hash = got.Version, got.Hash
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("built:\n%+v\nfixture:\n%+v", got, want)
	}
}

// A registered rule installs Off, after the canonical rules, and the snapshot moves: a new hash, the next
// version.
func TestARegisteredRuleInstallsOff(t *testing.T) {
	var b Builder
	base, err := b.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Register(Registration{ID: "deps.license-deny", Name: "Only approved licenses", Producer: Scanner})
	got, err := b.Build(&base)
	if err != nil {
		t.Fatal(err)
	}
	want := Rule{ID: "deps.license-deny", Name: "Only approved licenses", State: Off, Producer: Scanner}
	if len(got.Rules) != len(canonical)+1 || !reflect.DeepEqual(got.Rules[len(canonical)], want) {
		t.Fatalf("rules after registering: %+v\nwant the canonical rules, then %+v", got.Rules, want)
	}
	if got.Hash == base.Hash || got.Version != base.Version+1 {
		t.Fatalf("registering moved nothing: %s v%d, base %s v%d", got.Hash, got.Version, base.Hash, base.Version)
	}
}

// Rebuilding from the same inputs is a no-op, whatever order the rules registered in: the
// same hash, and prev's version kept.
func TestANoOpRebuildKeepsHashAndVersion(t *testing.T) {
	license := Registration{ID: "deps.license-deny", Name: "Only approved licenses", Producer: Scanner}
	raw := Registration{ID: "sql.orm-raw", Name: "No raw queries through the ORM", Producer: Scanner}
	var first, second Builder
	first.Register(license)
	first.Register(raw)
	second.Register(raw)
	second.Register(license)
	prev, err := first.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	prev.Version = 7
	again, err := second.Build(&prev)
	if err != nil {
		t.Fatal(err)
	}
	if again.Hash != prev.Hash || again.Version != 7 {
		t.Fatalf("rebuild: %s v%d, want %s v7", again.Hash, again.Version, prev.Hash)
	}
}

// The hash covers everything but version and hash: moving any one field moves it, so an edit
// anywhere invalidates the verdicts cached against the old snapshot.
func TestTheHashCoversEveryFieldButVersionAndHash(t *testing.T) {
	built := func() Snapshot {
		s, err := (&Builder{}).Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := built()
	for name, edit := range map[string]func(*Snapshot){
		"rules":               func(s *Snapshot) { s.Rules[0].State = Advisory },
		"production":          func(s *Snapshot) { s.Rules[5].Production = Blocking },
		"exclusions":          func(s *Snapshot) { s.Exclusions = s.Exclusions[1:] },
		"agents":              func(s *Snapshot) { s.Agents[0].Model = "another-model" },
		"component_menu":      func(s *Snapshot) { s.ComponentMenu = s.ComponentMenu[1:] },
		"stacks":              func(s *Snapshot) { s.Stacks = []Stack{{Language: "Go", Frameworks: []string{"net/http"}}} },
		"approved_buildpacks": func(s *Snapshot) { s.ApprovedBuildpacks = []string{"paketo-buildpacks/go-build"} },
		"limits":              func(s *Snapshot) { s.Limits.GateRunsPerActorPerHour++ },
		"capabilities":        func(s *Snapshot) { s.Capabilities["design_systems"] = Capability{} },
		"capabilities.org_default": func(s *Snapshot) {
			s.Capabilities["design_systems"] = Capability{Enabled: true, OrgDefault: &DesignSystem{Name: "Meridian", Namespace: "@meridian/ui", RegistryURL: "https://registry.meridian.example/ui"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := built()
			edit(&s)
			if got, err := hashOf(s); err != nil || got == base.Hash {
				t.Fatalf("editing %s left the hash at %s (%v)", name, got, err)
			}
		})
	}
	s := built()
	s.Version, s.Hash = 9, "snap_00000000"
	if got, err := hashOf(s); err != nil || got != base.Hash {
		t.Fatalf("version and hash moved the hash: %s, want %s (%v)", got, base.Hash, err)
	}
}

// Build fails closed on a rule it cannot place: nothing is dropped, nothing defaulted.
func TestBuildRefusesARuleItCannotPlace(t *testing.T) {
	license := Registration{ID: "deps.license-deny", Name: "Only approved licenses", Producer: Scanner}
	for name, rs := range map[string][]Registration{
		"a canonical rule again":           {{ID: "secrets.client-bundle", Name: "Again", Producer: Scanner}},
		"the same new rule twice":          {license, license},
		"outside its producer's namespace": {{ID: "perf.n-plus-one", Name: "No N+1 queries", Producer: Scanner}},
		"an unknown producer":              {{ID: "ops.pager", Name: "Someone is on call", Producer: "ops"}},
		"an id the schema refuses":         {{ID: "deps.Upper-Case", Name: "Upper case", Producer: Scanner}},
		"no name":                          {{ID: "deps.license-deny", Producer: Scanner}},
	} {
		t.Run(name, func(t *testing.T) {
			var b Builder
			for _, r := range rs {
				b.Register(r)
			}
			if s, err := b.Build(nil); err == nil {
				t.Fatalf("built %s with %+v", s.Hash, rs)
			}
		})
	}
}

// WithStates fails closed like Build: a rule the snapshot does not carry and a state outside the
// three are errors, and neither answers a snapshot.
func TestWithStatesRefusesAnUnknownRuleAndAStateOutsideTheThree(t *testing.T) {
	base, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithStates(base, map[string]State{"nope.rule": Blocking}); err == nil || err.Error() != "rule nope.rule is not in the snapshot" {
		t.Fatalf("an unknown rule: %v", err)
	}
	if _, err := WithStates(base, map[string]State{"owner.required": "loud"}); err == nil || err.Error() != `rule owner.required: "loud" is not a state` {
		t.Fatalf("a state outside the three: %v", err)
	}
}

// An edit that changes a state moves the hash and the version by one; one that names the state
// the rule already has moves neither. The input is never mutated: the server composes every
// request from the one boot snapshot.
func TestAnEditMovesTheHashAndTheVersion(t *testing.T) {
	base, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := base
	before.Rules = slices.Clone(base.Rules)
	got, err := WithStates(base, map[string]State{"owner.required": Blocking})
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules[7].State != Blocking || got.Hash == base.Hash || got.Version != base.Version+1 {
		t.Fatalf("edited: %s %s v%d; base %s v%d", got.Rules[7].State, got.Hash, got.Version, base.Hash, base.Version)
	}
	if !reflect.DeepEqual(base, before) { // checked here, after one call: a second call writing the default back would hide it
		t.Fatalf("the input moved:\n%+v\nwas:\n%+v", base, before)
	}
	same, err := WithStates(base, map[string]State{"owner.required": Advisory})
	if err != nil {
		t.Fatal(err)
	}
	if same.Hash != base.Hash || same.Version != base.Version {
		t.Fatalf("a no-op edit: %s v%d; base %s v%d", same.Hash, same.Version, base.Hash, base.Version)
	}
	if !reflect.DeepEqual(base, before) {
		t.Fatalf("the input moved:\n%+v\nwas:\n%+v", base, before)
	}
}

// The effective-snapshot golden decodes into the mirror and marshals back to its own content: a rule
// placed before production carries production between state and producer, and no other rule carries
// it. Absent is Off, so the golden with its one placement taken out hashes as the built snapshot does.
func TestTheEffectiveSnapshotGoldenRoundTrips(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/testdata/snapshot/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden Snapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&golden); err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	back, err := json.Marshal(golden)
	if err == nil {
		err = errors.Join(json.Unmarshal(raw, &want), json.Unmarshal(back, &got))
	}
	if err == nil { // absent and null are one value on org_default, and the mirror omits it (snapshot.md)
		delete(want["capabilities"].(map[string]any)["design_systems"].(map[string]any), "org_default")
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("the golden through the mirror (%v):\n%s", err, back)
	}
	if i := bytes.Index(back, []byte(`"state":"advisory","production":"blocking","producer":"llm-review"`)); i < 0 || bytes.Count(back, []byte(`"production"`)) != 1 {
		t.Fatalf("production is carried once, between state and producer:\n%s", back)
	}
	built, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	golden.Rules[4].Production = ""
	if h, err := hashOf(golden); err != nil || h != built.Hash {
		t.Fatalf("with no rule placed the golden hashes %s, the built snapshot %s (%v)", h, built.Hash, err)
	}
}

// A rule is on while it is not Off in either gate: Off before preview and placed before production
// is on, Off in both is not, and a state outside the three is on.
func TestARuleIsOnWhileEitherGateHasIt(t *testing.T) {
	for _, c := range []struct {
		r  Rule
		on bool
	}{
		{Rule{State: Off}, false},
		{Rule{State: Advisory}, true},
		{Rule{State: Off, Production: Advisory}, true},
		{Rule{State: Off, Production: Blocking}, true},
		{Rule{State: "loud"}, true},
	} {
		if c.r.On() != c.on {
			t.Errorf("%+v: on %v, want %v", c.r, c.r.On(), c.on)
		}
	}
}

// prod.egress-allowed (design/EGRESS.md §3.4) is in every snapshot, installed Off like any new rule, and
// server-state's: it reads what the server holds, though promote-gate owns the rest of its namespace.
func TestProdEgressAllowedInstallsOff(t *testing.T) {
	s, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Rule{ID: "prod.egress-allowed", Name: "Production allows everything preview connects to", State: Off, Producer: ServerState}
	if i := slices.IndexFunc(s.Rules, func(r Rule) bool { return r.ID == want.ID }); i < 0 || !reflect.DeepEqual(s.Rules[i], want) {
		t.Fatalf("the snapshot's rules: %+v\nwant %+v among them", s.Rules, want)
	}
}

// platform.known-vuln (design/PATCHING.md §3.3) is in every snapshot, installed Off like any new rule,
// promote-gate's — the producer that owns its namespace — and refused under any other producer.
func TestPlatformKnownVulnRegistered(t *testing.T) {
	s, err := (&Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Rule{ID: "platform.known-vuln", Name: "No known-vulnerable system libraries or build tools", State: Off, Producer: PromoteGate}
	if i := slices.IndexFunc(s.Rules, func(r Rule) bool { return r.ID == want.ID }); i < 0 || !reflect.DeepEqual(s.Rules[i], want) {
		t.Fatalf("the snapshot's rules: %+v\nwant %+v among them", s.Rules, want)
	}
	var b Builder
	b.Register(Registration{ID: "platform.base-image", Name: "Base images are current", Producer: Scanner})
	if _, err := b.Build(nil); err == nil {
		t.Fatal("a scanner's rule in platform's namespace was built")
	}
}
