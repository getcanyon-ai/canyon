package main

import (
	"bytes"
	"testing"

	"github.com/getcanyon-ai/canyon/contracts"
)

func TestLoadSnapshotReadsTheFixtureAndRefusesOneWithoutExclusionsOrWithABadGlob(t *testing.T) {
	s, err := loadSnapshot(contracts.Snapshot)
	if err != nil || len(s.Exclusions) == 0 || s.Exclusions[0] != ".env*" {
		t.Fatalf("the fixture: %+v (%v)", s, err)
	}
	// A snapshot with no menu answers a list, not null.
	if s, err := loadSnapshot([]byte(`{"exclusions": ["a"]}`)); err != nil || s.ComponentMenu == nil || len(s.ComponentMenu) != 0 {
		t.Fatalf("an absent menu empty: %#v (%v)", s.ComponentMenu, err)
	}
	for name, raw := range map[string]string{
		"empty":         `{}`,
		"no exclusions": `{"exclusions": []}`,
		"not json":      `nope`,
		"bad glob":      `{"exclusions": ["["]}`,
	} {
		if _, err := loadSnapshot([]byte(raw)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestLoadPolicySnapshotReadsTheFixtureAndRefusesOneWithoutRules(t *testing.T) {
	s, err := loadPolicySnapshot(contracts.Snapshot)
	if err != nil || len(s.Rules) != 21 {
		t.Fatalf("the fixture: %d rules (%v)", len(s.Rules), err)
	}
	if _, err := loadPolicySnapshot([]byte(`{"rules": []}`)); err == nil || err.Error() != "snapshot: no rules" {
		t.Fatalf("no rules: %v", err)
	}
	twice := bytes.Replace(contracts.Snapshot, []byte(`"id": "owner.required"`), []byte(`"id": "prod.clean-gate"`), 1)
	if _, err := loadPolicySnapshot(twice); err == nil || err.Error() != `snapshot: rule "prod.clean-gate" appears twice` {
		t.Fatalf("a repeated rule id: %v", err)
	}
	missing := bytes.Replace(contracts.Snapshot, []byte(`"id": "owner.required"`), []byte(`"id": "owner.optional"`), 1)
	if _, err := loadPolicySnapshot(missing); err == nil || err.Error() != "snapshot: no rule owner.required" {
		t.Fatalf("a promote rule missing: %v", err)
	}
	unlimited := bytes.Replace(contracts.Snapshot, []byte(`"gate_runs_per_actor_per_hour": 60`), []byte(`"gate_runs_per_actor_per_hour": 0`), 1)
	if _, err := loadPolicySnapshot(unlimited); err == nil || err.Error() != "snapshot: a gate-run limit under 1" {
		t.Fatalf("a zero limit: %v", err)
	}
	if got := string(mustJSON(promoteRules(s))); got != `[{"id":"prod.clean-gate","name":"Production needs a passing review","state":"blocking","producer":"promote-gate"},{"id":"prod.human-approval","name":"A person promotes","state":"blocking","producer":"promote-gate"},{"id":"stack.blessed","name":"Approved stacks only","state":"advisory","producer":"server-state"},{"id":"owner.required","name":"Every app has an owner","state":"advisory","producer":"server-state"},{"id":"platform.known-vuln","name":"No known-vulnerable system libraries or build tools","state":"off","producer":"promote-gate"},{"id":"prod.egress-allowed","name":"Production allows everything preview connects to","state":"off","producer":"server-state"}]` {
		t.Fatalf("the go-live rules in the snapshot's order: %s", got)
	}
}
