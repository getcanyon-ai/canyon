// Package policy builds seam 5, the policy snapshot (contracts/snapshot.md): the one
// versioning primitive. Everything that shapes a verdict lives in it and the verdict cache
// keys on its hash, so editing anything in it invalidates exactly the cached verdicts it
// should.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/contracts"
)

// State is a rule's trust tier. There are three.
type State string

const (
	Off      State = "off"
	Advisory State = "advisory"
	Blocking State = "blocking"
)

// Producer is what emits a rule's findings; each owns the namespaces its canonical rules sit
// in (contracts/validate.py derives PRODUCER_NAMESPACES the same way).
type Producer string

const (
	Scanner     Producer = "scanner"
	PromoteGate Producer = "promote-gate"
	ServerState Producer = "server-state"
	LLMReview   Producer = "llm-review"
	// Sarif is an imported tool's: one rule per tool, sarif.<tool> (contracts/snapshot.md).
	Sarif Producer = "sarif"
)

// Driver is the tool.driver.name a producer's runs carry, byte-exact with
// contracts/finding.md. Only these three have one: an agent's run carries its snapshot id
// instead, and the snapshot's roster answers for it; an imported tool's carries sarif/<tool>.
func (p Producer) Driver() string {
	switch p {
	case Scanner:
		return "canyon/scanners"
	case ServerState:
		return "canyon/server-state"
	case PromoteGate:
		return "canyon/promote-gate"
	}
	return ""
}

// ByDriver resolves a run's tool.driver.name to the producer whose rules it may carry. The
// one table both the runner and seam 1's validator read, so a renamed driver cannot mean one
// thing in gate and another in gate/finding. sarif/<tool> is an imported tool's run, Sarif's.
func ByDriver(name string) (Producer, bool) {
	for _, p := range []Producer{Scanner, ServerState, PromoteGate} {
		if p.Driver() == name {
			return p, true
		}
	}
	if strings.HasPrefix(name, "sarif/") {
		return Sarif, true
	}
	return "", false
}

// Snapshot mirrors contracts/schema/snapshot.schema.json field for field.
type Snapshot struct {
	Version       int      `json:"version"`
	Hash          string   `json:"hash"`
	Rules         []Rule   `json:"rules"`
	Exclusions    []string `json:"exclusions"`
	Agents        []Agent  `json:"agents"`
	ComponentMenu []string `json:"component_menu"`
	Stacks        []Stack  `json:"stacks"`
	// ApprovedBuildpacks is the buildpack ids stack.blessed approves, absent while none is, so the hash
	// stands until an admin approves one (contracts/snapshot.md).
	ApprovedBuildpacks []string              `json:"approved_buildpacks,omitempty"`
	Limits             Limits                `json:"limits"`
	Capabilities       map[string]Capability `json:"capabilities"`
}

// Rule is a policy: id = policy id = --explain arg = audit event type.
type Rule struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State State  `json:"state"` // before preview; a go-live rule's before production, its only gate

	// Production is the rule's state before production, advisory or blocking, carried only on a rule
	// placed there: absent is Off, so a snapshot with no rule placed there hashes as it did before the
	// field (contracts/snapshot.md, Two gates).
	Production State    `json:"production,omitempty"`
	Producer   Producer `json:"producer"`

	// Run and Settings are a Canyon-run tool's (sarif.trivy, sarif.opengrep, sarif.eslint,
	// sarif.grype, sarif.checkov): whether Canyon runs it on this host, and its allow-list — each named only
	// when set, so a no-op never moves the hash (contracts/snapshot.md).
	Run      bool     `json:"run,omitempty"`
	Settings Settings `json:"settings,omitzero"`

	// Instructions is a custom rule's text, the admin's, on custom.* alone (contracts/snapshot.md,
	// Custom rules); the hash covers it, so every run journals the text it was judged against.
	Instructions string `json:"instructions,omitempty"`

	// Check is how a custom rule's version is checked, on custom.* alone, fixed when the version is
	// saved (contracts/snapshot.md, Custom rules): by code, its producer Scanner, or by AI, LLMReview.
	Check *Check `json:"check,omitempty"`

	// Playbook is the org's playbook a custom rule's newest version points at, on custom.* alone and only while
	// the playbook stands (contracts/snapshot.md, Custom rules): the rule's findings carry the same
	// (contracts/finding.md, canyon.playbook). No review is sent it: the reviewer reads Instructions and Check.
	Playbook *Playbook `json:"playbook,omitempty"`
}

// Playbook names one of the org's playbooks: its slug and its name (design/specs/mcp.md §3.4).
type Playbook struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Check is a custom rule's check: By "code", with Expr and Says, or "ai"; FactsVersion the version of
// the facts it was written against (gate/custom).
type Check struct {
	By           string `json:"by"`
	Expr         string `json:"expr,omitempty"`
	Says         string `json:"says,omitempty"`
	FactsVersion int    `json:"facts_version"`
}

// On is whether the rule is not Off in either gate: a review runs every rule that is on, so the
// findings before production judges always exist (contracts/snapshot.md, Two gates). A state outside
// the three is not Off, so it is on: unknown state never takes a rule out of a review.
func (r Rule) On() bool {
	return r.State != Off || r.Production != ""
}

// Settings is a Canyon-run tool's allow-list, every tool's keys in one; each tool takes only its
// own (contracts/routes.md, the policies row).
type Settings struct {
	Scanners   []string       `json:"scanners,omitempty"`    // trivy
	Rules      *OpengrepRules `json:"rules,omitempty"`       // opengrep
	Frameworks []string       `json:"frameworks,omitempty"`  // checkov
	SkipChecks []string       `json:"skip_checks,omitempty"` // checkov
	SkipPaths  []string       `json:"skip_paths,omitempty"`  // every tool
	Source     string         `json:"source,omitempty"`      // platform.known-vuln's: an imported tool whose image reports it reads, "" for trivy's own
}

// OpengrepRules is the rules repository the server fetches for opengrep, at one commit; Paths, when
// set, are the repo's directories whose rules files alone are read.
type OpengrepRules struct {
	Repo   string   `json:"repo"`
	Commit string   `json:"commit"`
	Paths  []string `json:"paths,omitempty"`
}

type Agent struct {
	ID        string   `json:"id"`
	Namespace []string `json:"namespace"`
	Model     string   `json:"model"`
}

// Stack is one approved language and the frameworks approved with it — one row per language,
// as the org's preferences carry them (routes.md, GET/PUT /api/v1/preferences).
type Stack struct {
	Language   string   `json:"language"`
	Frameworks []string `json:"frameworks"`
}

type Limits struct {
	GateRunsPerProjectPerHour int `json:"gate_runs_per_project_per_hour"`
	GateRunsPerActorPerHour   int `json:"gate_runs_per_actor_per_hour"`
}

type Capability struct {
	Enabled    bool          `json:"enabled"`
	OrgDefault *DesignSystem `json:"org_default,omitempty"`
}

// DesignSystem is the org's default, as an agent reads it: the package namespace it declares
// and where to fetch it, never a registry row id. Absent and null both mean nothing configured.
type DesignSystem struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	RegistryURL string `json:"registry_url"`
	TokensURL   string `json:"tokens_url,omitempty"`
}

// Registration is a rule joining the snapshot. It carries no state: a new rule installs
// Off, and turning it on is a policy edit, never a default.
type Registration struct {
	ID       string
	Name     string
	Producer Producer
}

// Builder makes snapshots: the canonical rules, then every registered rule, Off. The
// zero value is ready to use. Register every rule before the first Build; a Builder is not
// safe for concurrent use.
type Builder struct {
	registered []Rule
}

// Register adds a rule, Off. Build refuses one it cannot place.
func (b *Builder) Register(r Registration) {
	b.registered = append(b.registered, Rule{ID: r.ID, Name: r.Name, State: Off, Producer: r.Producer})
}

// Build assembles the snapshot and fails closed: a repeated id, an id outside its producer's
// namespace, or anything seam 5's schema refuses is an error, never dropped. Registered rules
// follow the canonical rules sorted by id, so the order they registered in does not move the hash. The
// hash covers everything but version and hash, so it is stable across no-op rebuilds; the
// version is 1 with no prev, prev's while the hash is prev's, and one more once it moves.
// prev contributes its version and hash only, never its rules: states come from the table
// and from Off.
func (b *Builder) Build(prev *Snapshot) (Snapshot, error) {
	registered := slices.Clone(b.registered)
	slices.SortFunc(registered, func(x, y Rule) int { return strings.Compare(x.ID, y.ID) })
	s := defaults()
	s.Rules = append(slices.Clone(canonical), registered...)
	seen := make(map[string]bool, len(s.Rules))
	for _, r := range s.Rules {
		if seen[r.ID] {
			return Snapshot{}, fmt.Errorf("rule %s is registered twice", r.ID)
		}
		seen[r.ID] = true
		ns, _, _ := strings.Cut(r.ID, ".")
		if !slices.ContainsFunc(canonical, func(c Rule) bool { return c.Producer == r.Producer && strings.HasPrefix(c.ID, ns+".") }) {
			return Snapshot{}, fmt.Errorf("rule %s is outside producer %q's namespace", r.ID, r.Producer)
		}
	}
	hash, err := hashOf(s)
	if err != nil {
		return Snapshot{}, err
	}
	s.Hash = hash
	s.Version = 1
	if prev != nil {
		s.Version = prev.Version
		if prev.Hash != s.Hash {
			s.Version++
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return Snapshot{}, err
	}
	if err := contracts.CheckSnapshotSchema(raw); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// WithStates is prev with each named rule's state replaced: the snapshot the server composes per
// request from the org's policy edits (contracts/snapshot.md). It fails closed the way Build does —
// an id prev does not carry or a state outside the three is an error, never dropped — and follows
// Build's version rule: the version is prev's, and one more once the hash moves. prev is left as
// it was, and the result shares everything but its rules with prev: read it, never write it.
func WithStates(prev Snapshot, states map[string]State) (Snapshot, error) {
	s := prev
	s.Rules = slices.Clone(prev.Rules)
	for id, st := range states {
		if !slices.Contains([]State{Off, Advisory, Blocking}, st) {
			return Snapshot{}, fmt.Errorf("rule %s: %q is not a state", id, st)
		}
		i := slices.IndexFunc(s.Rules, func(r Rule) bool { return r.ID == id })
		if i < 0 {
			return Snapshot{}, fmt.Errorf("rule %s is not in the snapshot", id)
		}
		s.Rules[i].State = st
	}
	hash, err := hashOf(s)
	if err != nil {
		return Snapshot{}, err
	}
	if hash != prev.Hash {
		s.Version++
	}
	s.Hash = hash
	raw, err := json.Marshal(s)
	if err != nil {
		return Snapshot{}, err
	}
	if err := contracts.CheckSnapshotSchema(raw); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// hashOf is snap_ + the sha256 of everything in s but its version and hash.
func hashOf(s Snapshot) (string, error) {
	s.Version, s.Hash = 0, ""
	content, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return "snap_" + hex.EncodeToString(sum[:]), nil
}
