package main

import (
	"encoding/json"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
)

// blocksProduction is the receipt's own answer for one finding (#2206): under a placed rule, its id or one
// under it, at a severity the rule blocks, and never a Dockerfile check's.
func TestBlocksProductionIsTheReceiptsAnswer(t *testing.T) {
	rules := []string{"sql.string-concat", "iac.misconfig"}
	for _, c := range []struct {
		rule, severity string
		want           bool
	}{
		{"sql.string-concat", "high", true},
		{"sql.string-concat.opengrep.tainted", "critical", true},
		{"sql.string-concat", "medium", false},
		{"sql.string-concatenated", "high", false},
		{"iac.misconfig.checkov.CKV_DOCKER_2", "high", false},
		{"iac.misconfig.checkov.CKV_K8S_1", "high", true},
		{"auth.unauthenticated-route", "critical", false},
	} {
		if got := blocksProduction(rules, c.rule, finding.Severity(c.severity)); got != c.want {
			t.Errorf("%s at %s: %v, want %v", c.rule, c.severity, got, c.want)
		}
	}
	if blocksProduction(nil, "sql.string-concat", "critical") {
		t.Error("nothing placed blocks a finding before production")
	}
}

// withProduction marks the findings blocksProduction answers for, and nothing else.
func TestWithProductionMarksWhatBlocksBeforeProduction(t *testing.T) {
	in := `[{"ruleId":"sql.string-concat","properties":{"canyon":{"blocks":false,"severity":"high"}}},{"ruleId":"sql.string-concat","properties":{"canyon":{"blocks":false,"severity":"low"}}}]`
	out, err := withProduction(json.RawMessage(in), []string{"sql.string-concat"})
	if err != nil {
		t.Fatal(err)
	}
	var read []map[string]any
	if err := json.Unmarshal(out, &read); err != nil {
		t.Fatal(err)
	}
	if read[0]["blocks_production"] != true || read[1]["blocks_production"] != nil {
		t.Fatalf("marked: %s", out)
	}
}

// covered keeps an approval on a blocking finding only when verdictSQL computed that it covers it, and on an
// advisory one always; a finding that does not say whether it blocks reads as blocking; covers itself is
// never answered.
func TestCoveredKeepsOnlyTheApprovalsThatCover(t *testing.T) {
	approval := func(covers bool) string {
		raw, _ := json.Marshal(map[string]any{"reason": "r", "expires_at": "2099-01-01T00:00:00Z", "approved_by": map[string]any{"kind": "person", "subject": "s", "display": "d"}, "covers": covers})
		return string(raw)
	}
	in := `[` +
		`{"ruleId":"secrets.x","properties":{"canyon":{"blocks":true}},"approval":` + approval(false) + `},` +
		`{"ruleId":"secrets.x","properties":{"canyon":{"blocks":true}},"approval":` + approval(true) + `},` +
		`{"ruleId":"perf.x","properties":{"canyon":{"blocks":false}},"approval":` + approval(false) + `},` +
		`{"ruleId":"secrets.x","properties":{"canyon":{}},"approval":` + approval(false) + `},` +
		`{"ruleId":"sql.string-concat","properties":{"canyon":{"blocks":false,"severity":"high"}},"approval":` + approval(false) + `}]`
	out, err := covered(json.RawMessage(in), []string{"sql.string-concat"})
	if err != nil {
		t.Fatal(err)
	}
	var read []struct {
		Approval map[string]any `json:"approval"`
	}
	if err := json.Unmarshal(out, &read); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{false, true, true, false, false} {
		kept := read[i].Approval != nil
		if kept != want {
			t.Errorf("finding %d keeps its approval %v, want %v: %s", i, kept, want, out)
		}
		if _, leaked := read[i].Approval["covers"]; kept && leaked {
			t.Errorf("finding %d answers covers: %s", i, out)
		}
	}
}
