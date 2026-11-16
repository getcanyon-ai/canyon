package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy" // policy is the skill manifest's row type here
)

// snapshot is what the server reads from the policy snapshot (contracts/snapshot.md): the
// exclusions every upload is filtered by and the component menu the skill manifest hands an
// agent. The rules boot through loadPolicySnapshot and are read composed over the org's edits
// (policy.go), never from here, and so are the gate-run limits runs.StartTx holds a start to.
type snapshot struct {
	ComponentMenu []string `json:"component_menu"` // the skill manifest's component_types, unfiltered: an empty list is legitimate
	Exclusions    []string `json:"exclusions"`
}

// loadPolicySnapshot decodes the same fixture as seam 5's own type, for the pipeline: the gate
// derives every finding's blocks from a rule's state in it. A snapshot with no rules would
// refuse every log, so it refuses the boot instead.
func loadPolicySnapshot(raw []byte) (gatepolicy.Snapshot, error) {
	var s gatepolicy.Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return gatepolicy.Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	if len(s.Rules) == 0 {
		return gatepolicy.Snapshot{}, errors.New("snapshot: no rules")
	}
	if s.Limits.GateRunsPerProjectPerHour < 1 || s.Limits.GateRunsPerActorPerHour < 1 { // a zero limit would answer every start 429
		return gatepolicy.Snapshot{}, errors.New("snapshot: a gate-run limit under 1")
	}
	seen := make(map[string]bool, len(s.Rules))
	for _, r := range s.Rules { // a repeated id boots clean and then fails every run at the gate: refused here instead
		if seen[r.ID] {
			return gatepolicy.Snapshot{}, fmt.Errorf("snapshot: rule %q appears twice", r.ID)
		}
		seen[r.ID] = true
	}
	for _, id := range promoteRuleIDs { // the receipt folds exactly these: a fixture without one boots a receipt that cannot block on it
		if !seen[id] {
			return gatepolicy.Snapshot{}, fmt.Errorf("snapshot: no rule %s", id)
		}
	}
	// The fixture's version and hash are illustrative (contracts/snapshot.md). The boot snapshot is
	// the base every request's snapshot is composed from, so it carries the hash of its own content:
	// composed with no edits it is itself, and the first edit moves the version by one, not by two.
	return gatepolicy.WithStates(s, nil)
}

// egressRule is the go-live rule that judges what preview called against what production allows
// (design/EGRESS.md §3.4).
const egressRule = "prod.egress-allowed"

// promoteRuleIDs are the go-live rules the receipt answers for (contracts/routes.md "The promote receipt"):
// the four, and platform.known-vuln and prod.egress-allowed while each is on.
var promoteRuleIDs = []string{"prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required", platformRule, egressRule}

// loadSnapshot parses the embedded fixture once at boot and fails closed: no exclusions would
// ship .env into the gate and a bad glob would refuse every upload.
func loadSnapshot(raw []byte) (snapshot, error) {
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	if len(s.Exclusions) == 0 {
		return snapshot{}, errors.New("snapshot: no exclusions")
	}
	for _, pat := range s.Exclusions {
		if _, err := path.Match(pat, ""); err != nil { // ingest refuses every upload on a bad glob; refuse the boot instead
			return snapshot{}, fmt.Errorf("snapshot: exclusion %q: %w", pat, err)
		}
	}
	if s.ComponentMenu == nil { // the manifest's component_types is a list, never null (contracts/schema/guardrails.schema.json)
		s.ComponentMenu = []string{}
	}
	if _, err := componentsOf(s.ComponentMenu); err != nil { // GET /component-types answers one row per entry: a name with no row fails the boot
		return snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	return s, nil
}
