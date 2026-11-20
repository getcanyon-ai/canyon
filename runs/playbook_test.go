package runs

import (
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/policy"
)

// contracts/finding.md, canyon.playbook (design/specs/mcp.md §3.4): a custom rule's finding carries the playbook
// its rule points at as the run's snapshot carries it, slug and name, so an agent is told where the org explains
// the rule. The server derives it as it derives blocks: a rule whose snapshot carries no playbook — one removed —
// gives a finding with none, and no model is ever sent one.
func TestAFindingNamesItsRulesPlaybook(t *testing.T) {
	files := map[string]string{"index.js": "require('lodash');\n", "package.json": `{"dependencies": {"lodash": "^4"}}`,
		"package-lock.json": `{"lockfileVersion": 3, "packages": {"": {"dependencies": {"lodash": "^4"}}, "node_modules/lodash": {"version": "4.17.21"}}}`}
	snap := noLodash()
	at := len(snap.Rules) - 1
	snap.Rules[at].Playbook = &policy.Playbook{Slug: "service-conventions", Name: "How Meridian writes a service"}
	blocking, sent := reviewedWith(t, snap, files, lodashLocked)
	if len(blocking) != 1 || blocking[0].RuleID != "custom.no-lodash" {
		t.Fatalf("blocking: %+v", blocking)
	}
	if got := blocking[0].Properties.Canyon.Playbook; got == nil || *got != *snap.Rules[at].Playbook {
		t.Fatalf("the finding's playbook is %+v, want its rule's %+v", got, snap.Rules[at].Playbook)
	}
	for _, rules := range sent {
		for _, id := range rules {
			if strings.Contains(id, "service-conventions") {
				t.Fatalf("a model call was sent the playbook: %v", sent)
			}
		}
	}
	if blocking, _ := reviewedWith(t, noLodash(), files, lodashLocked); len(blocking) != 1 || blocking[0].Properties.Canyon.Playbook != nil {
		t.Fatalf("a rule whose snapshot carries no playbook gave %+v", blocking)
	}
}
