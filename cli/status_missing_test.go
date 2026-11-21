package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Production missing a value set for preview is said beside what production answered (contracts/routes.md, the status
// row's envs.prod.missing; #2786): each name with the command that sets it, in the words a promotion says it in, and
// under ai the same line and step a promotion gives, so an agent names the cause of a 500 there and asks the user for
// the value. The app page's golden for a stopped production that misses one, rendered whole.
func TestStatusSaysAValueProductionIsMissing(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	raw, err := os.ReadFile("../contracts/testdata/app-page/claims-portal-prod-stopped-status.json")
	if err != nil {
		t.Fatal(err)
	}
	var s statusRead
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	s.Access = &teamAccess{Level: "owner", People: 1}
	render := func(format Format) string {
		var out strings.Builder
		a := &app{out: &out}
		if format == FormatAI {
			a.render = Render{Format: FormatAI}
		}
		a.renderStatus("claims-portal", "http://localhost:7777", s, &runRow{ID: "gr_8f2a", State: "clean", EndedAt: "2026-09-02T09:57:41Z"}, at)
		return out.String()
	}
	human, ai := render(FormatHuman), render(FormatAI)
	if line := "               ! ORDERS_TOKEN is set for preview but not production. Set it with: canyon secret set ORDERS_TOKEN --env prod\n"; !strings.Contains(human, line) {
		t.Errorf("status lacks %q:\n%s", line, human)
	}
	if line := "not set for production: ORDERS_TOKEN\n  " + (&app{}).missingStep() + "\n"; !strings.Contains(ai, line) {
		t.Errorf("status --format ai lacks %q:\n%s", line, ai)
	}
	wants(t, "status-prod-missing.txt", human)
	wants(t, "status-ai-prod-missing.txt", ai)
}
