package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

const (
	emailPath  = "/api/v1/custom-rules/customer-email-encrypted"
	emailRule  = "custom.customer-email-encrypted"
	emailName  = "Customer emails are encrypted at rest"
	emailText  = "Flag code that writes a customer's email address to a database column without encrypting it."
	emailBody  = `{"name":"` + emailName + `","instructions":"` + emailText + `"}`
	customPath = "/api/v1/policies/" + emailRule
)

func customWrites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM custom_rule_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'custom_rule.set'")
}

func composedWith(t *testing.T, ctx context.Context, pool *pgxpool.Pool) gatepolicy.Snapshot {
	t.Helper()
	s, err := (&app{pool: pool, boot: testPolicy()}).effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A custom rule saves off at version 1 with one audit row of its texts; the next save is version 2
// with each text's before and after; a save that changes nothing writes nothing. Every member reads
// the list; the policies list carries the rule, off; the snapshot carries its instructions.
func TestACustomRuleSavesOffAndItsAuditCarriesTheDiff(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	before := composedWith(t, ctx, pool)
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, emailBody); code != 200 || out["custom_rule"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("the first save: %d %s", code, lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'custom_rule.set'").Scan(&detail); err != nil ||
		detail != `{"name": {"after": "`+emailName+`"}, "rule": "`+emailRule+`", "check": {"by": "ai"}, "version": 1, "instructions": {"after": "`+emailText+`"}}` {
		t.Fatalf("the first audit row: %s (%v)", detail, err)
	}
	if row := policyOf(t, getPolicies(t, c, base), emailRule); row["state"] != "off" || row["name"] != emailName {
		t.Fatalf("the policies row: %v", row)
	}
	snap := composedWith(t, ctx, pool)
	if r := snap.Rules[len(snap.Rules)-1]; r.ID != emailRule || r.Instructions != emailText || r.State != gatepolicy.Off || r.Producer != gatepolicy.LLMReview ||
		len(snap.Agents) != 1 || snap.Version != before.Version+1 || snap.Hash == before.Hash {
		t.Fatalf("the snapshot: %+v, agents %+v, version %d after %d", r, snap.Agents, snap.Version, before.Version)
	}
	renamed := strings.Replace(emailBody, "at rest", "everywhere", 1)
	if code, _, _ := do(t, c, http.MethodPut, base+emailPath, renamed); code != 200 {
		t.Fatalf("the second save: %d %s", code, lastBody)
	}
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'custom_rule.set' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil ||
		!strings.Contains(detail, `"name": {"after": "Customer emails are encrypted everywhere", "before": "`+emailName+`"}`) || !strings.Contains(detail, `"version": 2`) ||
		!strings.Contains(detail, `"instructions": {"after": "`+emailText+`", "before": "`+emailText+`"}`) {
		t.Fatalf("the second audit row: %s (%v)", detail, err)
	}
	second := composedWith(t, ctx, pool)
	if code, _, _ := do(t, c, http.MethodPut, base+emailPath, renamed); code != 200 || customWrites(t, ctx, pool) != 4 || composedWith(t, ctx, pool).Hash != second.Hash {
		t.Fatalf("a save that changes nothing: %d, %d rows", code, customWrites(t, ctx, pool))
	}
	if second.Version != snap.Version+1 || second.Hash == snap.Hash {
		t.Fatalf("the second save moves the version once: %d after %d", second.Version, snap.Version)
	}
	reworded := strings.Replace(renamed, "without encrypting it", "unencrypted", 1)
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, reworded); code != 200 || out["custom_rule"].(map[string]any)["version"] != float64(3) ||
		composedWith(t, ctx, pool).Rules[len(snap.Rules)-1].Instructions != strings.Replace(emailText, "without encrypting it", "unencrypted", 1) {
		t.Fatalf("an edit to the instructions alone: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/custom-rules", ""); code != 200 || len(out["custom_rules"].([]any)) != 1 ||
		out["custom_rules"].([]any)[0].(map[string]any)["version"] != float64(3) {
		t.Fatalf("the list: %d %s", code, lastBody)
	}
}

// No CLI verb changes the pipeline, and a custom rule's instructions are what a rule checks: an agent or
// canyon actor is refused 403 person_required before the body is read, writing nothing but its denied row
// (contracts/routes.md, the custom-rules row; #1972).
func TestACustomRuleAnAgentOrCanyonChangesNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, kind := range []string{"agent", "canyon"} {
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1", kind); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{emailBody, "{}"} { // {}: refused before the body is read
			if code, _, _ := do(t, c, http.MethodPut, base+emailPath, body); code != 403 || lastBody != `{"error":"person_required","message":"only a person can change a custom rule"}` {
				t.Errorf("%s %s: %d %s", kind, body, code, lastBody)
			}
		}
	}
	if n := customWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'person_required'"); n != 4 {
		t.Fatalf("four denied rows, got %d", n)
	}
}

// A member reads the custom rules and saves none: the PUT is 403 before anything is written.
func TestAMemberCannotWriteACustomRule(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if code, _, _ := do(t, member, http.MethodPut, base+emailPath, emailBody); code != 403 || lastBody != adminRequired || customWrites(t, ctx, pool) != 0 {
		t.Fatalf("a member's save: %d %s, %d rows", code, lastBody, customWrites(t, ctx, pool))
	}
	if code, _, _ := do(t, member, http.MethodGet, base+"/api/v1/custom-rules", ""); code != 200 || lastBody != `{"custom_rules":[]}` {
		t.Fatalf("a member's read: %d %s", code, lastBody)
	}
}

// What the routes row refuses, each 400 invalid_custom_rule with nothing written.
func TestACustomRuleOutsideItsBoundsIsRefused(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	key := "AKIA" + "QJ5K7MZ2RT4WX6YB" // assembled here, so no file of the repo carries one whole
	for name, req := range map[string]struct{ path, body string }{
		"a slug with a digit":           {"/api/v1/custom-rules/soc2", emailBody},
		"a slug over 40":                {"/api/v1/custom-rules/" + strings.Repeat("a", 41), emailBody},
		"an empty name":                 {emailPath, `{"name":"","instructions":"x"}`},
		"a name over 80":                {emailPath, `{"name":"` + strings.Repeat("n", 81) + `","instructions":"x"}`},
		"a name on two lines":           {emailPath, `{"name":"a\nb","instructions":"x"}`},
		"instructions over 2,000":       {emailPath, `{"name":"n","instructions":"` + strings.Repeat("i", 2001) + `"}`},
		"a control character":           {emailPath, `{"name":"n","instructions":"a\u0007b"}`},
		"a C1 control character":        {emailPath, `{"name":"n","instructions":"a\u0085b"}`},
		"a secret in the instructions":  {emailPath, `{"name":"n","instructions":"use aws_access_key_id=` + key + `"}`},
		"a secret in the name":          {emailPath, `{"name":"` + key + `","instructions":"x"}`},
		"a key the route does not take": {emailPath, `{"name":"n","instructions":"x","state":"blocking"}`},
	} {
		code, out, _ := do(t, c, http.MethodPut, base+req.path, req.body)
		if want := "invalid_custom_rule"; name == "a key the route does not take" {
			want = "bad_request"
			if code != 400 || out["error"] != want {
				t.Errorf("%s: %d %s", name, code, lastBody)
			}
		} else if code != 400 || out["error"] != want {
			t.Errorf("%s: %d %s", name, code, lastBody)
		}
	}
	if n := customWrites(t, ctx, pool); n != 0 {
		t.Fatalf("a refused save wrote %d rows", n)
	}
}

// A host takes 20 custom rules: a 21st is 409 too_many_rules, and a saved one still takes its next version.
func TestATwentyFirstCustomRuleIsRefused(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for i := range 20 {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/custom-rules/rule-"+string(rune('a'+i)), `{"name":"n","instructions":"x"}`); code != 200 {
			t.Fatalf("rule %d: %d %s", i, code, lastBody)
		}
	}
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, emailBody); code != 409 || out["error"] != "too_many_rules" || out["message"] != "A Canyon host takes 20 custom rules." {
		t.Fatalf("a 21st: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/custom-rules/rule-a", `{"name":"n","instructions":"y"}`); code != 200 {
		t.Fatalf("a saved rule's next version: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(DISTINCT rule) FROM custom_rule_edits"); n != 20 {
		t.Fatalf("%d rules", n)
	}
}

// The custom-rules agent is in the snapshot exactly while a custom rule is on, after canyon-review,
// claiming the custom namespace: turned on, the agent comes; turned off again, it goes; placed before
// production alone, it is on, and the agent comes again.
func TestTheSnapshotCarriesTheCustomRulesAgentOnlyWhileOneIsOn(t *testing.T) {
	withModel(t, &scripted{})
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+emailPath, emailBody); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	for _, body := range []string{`{"state":"blocking"}`, `{"state":"advisory"}`, `{"state":"off"}`, `{"production":"advisory"}`} {
		if code, _, _ := do(t, c, http.MethodPut, base+customPath+"?apply=true", body); code != 200 {
			t.Fatalf("%s: %d %s", body, code, lastBody)
		}
		snap := composedWith(t, ctx, pool)
		agents := fmt.Sprint(snap.Agents)
		want := "[{canyon-review [perf auth ai] resolved-model-id}]"
		if body != `{"state":"off"}` {
			want = "[{canyon-review [perf auth ai] resolved-model-id} {custom-rules [custom] resolved-model-id}]"
		}
		if agents != want {
			t.Fatalf("%s: agents %s, want %s", body, agents, want)
		}
	}
}

// With no model on the host a custom rule can't run, so turning one on is 409 no_model on the preview
// and the apply alike, before anything is written; turning it off is no refusal.
func TestACustomRuleIsNotTurnedOnWithoutAModel(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+emailPath, emailBody); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	for _, apply := range []string{"", "?apply=true"} {
		for _, body := range []string{`{"state":"advisory"}`, `{"production":"blocking"}`} { // on in either gate
			if code, out, _ := do(t, c, http.MethodPut, base+customPath+apply, body); code != 409 || out["error"] != "no_model" || out["message"] != noModel {
				t.Fatalf("turning on%s with %s: %d %s", apply, body, code, lastBody)
			}
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("a refused turn wrote %d rows", n)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+customPath+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("off: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/custom.never-saved?apply=true", `{"state":"off"}`); code != 404 {
		t.Fatalf("a custom rule never saved: %d %s", code, lastBody)
	}
}

// An OpenAI host reviews with a model though it has no setup chat: a custom rule turns on there.
func TestACustomRuleTurnsOnWhereTheHostReviewsWithoutTheSetupChat(t *testing.T) {
	testReviews = true
	t.Cleanup(func() { testReviews = false })
	_, _, base, c := withProject(t)
	do(t, c, http.MethodPut, base+emailPath, emailBody)
	if code, _, _ := do(t, c, http.MethodPut, base+customPath+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("turning on: %d %s", code, lastBody)
	}
}

// Custom rules are listed and composed by id bytewise, never by the database's collation, which
// orders "a-c" after "ab" where it ignores the hyphen.
func TestCustomRulesAreInBytewiseOrder(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, slug := range []string{"ab", "a-c"} {
		do(t, c, http.MethodPut, base+"/api/v1/custom-rules/"+slug, `{"name":"n","instructions":"x"}`)
	}
	snap := composedWith(t, ctx, pool)
	if got := fmt.Sprint(snap.Rules[len(snap.Rules)-2].ID, " ", snap.Rules[len(snap.Rules)-1].ID); got != "custom.a-c custom.ab" {
		t.Fatalf("the snapshot's order: %s", got)
	}
}

// The custom-rules routes answer contracts/testdata/policies/meridian.json: the list, a version saved
// with its audit row, and a rule turned on with no model.
func TestTheCustomRuleRoutesAnswerTheGolden(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	raw, err := os.ReadFile("../../contracts/testdata/policies/meridian.json")
	var want map[string]map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &want)
	}
	if err != nil {
		t.Fatal(err)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	put := want["custom_rule_put"]
	detail := put["audit"].(map[string]any)["detail"].(map[string]any)
	before := func(field string) string { return detail[field].(map[string]any)["before"].(string) }
	listed := want["custom_rules"]["custom_rules"].([]any)
	other := listed[1].(map[string]any)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET actor_display = 'Dana Kim' WHERE actor_subject = 'sub-dana'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO custom_rule_edits (rule, version, name, instructions, check_facts_version, set_by_kind, set_by_subject, set_by_display, set_at) VALUES
		($1, 1, $2, $3, $8, 'person', 'sub-dana', 'Dana Kim', '2026-09-29T08:00:00Z'), ($4, 1, $5, $6, $8, 'person', 'sub-dana', 'Dana Kim', $7)`,
		"custom."+put["slug"].(string), before("name"), before("instructions"), other["rule"], other["name"], other["instructions"], other["set_at"],
		int(other["check"].(map[string]any)["facts_version"].(float64))); err != nil { // saved by AI at this release's facts version, as a save now is
		t.Fatal(err)
	}
	byCode := listed[2].(map[string]any) // the golden's rule checked by code, saved as a translation would save it
	check := byCode["check"].(map[string]any)
	if _, err := pool.Exec(ctx, `INSERT INTO custom_rule_edits (rule, version, name, instructions, check_by, check_expr, check_says, check_facts_version, set_by_kind, set_by_subject, set_by_display, set_at)
		VALUES ($1, 1, $2, $3, 'code', $4, $5, $6, 'person', 'sub-dana', 'Dana Kim', $7)`,
		byCode["rule"], byCode["name"], byCode["instructions"], check["expr"], check["says"], int(check["facts_version"].(float64)), byCode["set_at"]); err != nil {
		t.Fatal(err)
	}
	seedPlaybook(t, ctx, pool, put["request"].(map[string]any)["playbook"].(string), "How Meridian writes a service") // the playbook the golden's rule points at
	code, got, _ := do(t, dana, http.MethodPut, base+"/api/v1/custom-rules/"+put["slug"].(string), string(mustJSON(put["request"])))
	saved, _ := got["custom_rule"].(map[string]any)
	if code != 200 || saved == nil {
		t.Fatalf("the save answered %d %s", code, lastBody)
	}
	res := put["response"].(map[string]any)
	stamped(t, saved, res["custom_rule"].(map[string]any), "set_at", 0)
	if !reflect.DeepEqual(got, res) {
		t.Fatalf("the save answered %s,\nwant the golden's", lastBody)
	}
	var audit map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'custom_rule.set'").Scan(&audit); err != nil || !reflect.DeepEqual(audit, detail) {
		t.Fatalf("the audit row: %s (%v),\nwant the golden's", mustJSON(audit), err)
	}
	code, got, _ = do(t, dana, http.MethodGet, base+"/api/v1/custom-rules", "")
	first := got["custom_rules"].([]any)[0].(map[string]any)
	first["set_at"] = listed[0].(map[string]any)["set_at"] // the save's own instant, stamped above
	if code != 200 || !reflect.DeepEqual(got, want["custom_rules"]) {
		t.Fatalf("the list answered %d %s,\nwant the golden's", code, lastBody)
	}
	nm := want["no_model"]
	if code, got, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+nm["rule"].(string)+"?apply=true", string(mustJSON(nm["request"]))); code != 409 || !reflect.DeepEqual(got, nm["response"]) {
		t.Fatalf("no_model answered %d %s,\nwant the golden's", code, lastBody)
	}
}

// translator answers each translation with its next text, the last one again once they run out, and
// counts the calls.
type translator struct {
	answers []string
	err     error
	calls   int
}

func (m *translator) Ask(context.Context, string, string, string, map[string]any) (string, error) {
	m.calls++
	answer := m.answers[min(m.calls, len(m.answers))-1]
	return answer, m.err
}

// withTranslator is the test server's model asked how a custom rule is checked, set before start.
func withTranslator(t *testing.T, m *translator) {
	t.Helper()
	testTranslate = m
	t.Cleanup(func() { testTranslate = nil })
}

// The rule a code check is admitted for in these tests: one call over the files, quick at their bounds
// even under the race detector, which slows a pass over every file past admission's time.
const (
	readmePath = "/api/v1/custom-rules/has-readme"
	readmeRule = "custom.has-readme"
	readmeText = "Every app has a README.md at its root."
	readmeBody = `{"name":"Has a README","instructions":"` + readmeText + `"}`
	readmeExpr = `'README.md' in files`
	readmeSays = "The app has a README.md at its root."
)

func translation(checker, expr, says string) string {
	b, _ := json.Marshal(map[string]string{"checker": checker, "expr": expr, "says": says, "why": "because"})
	return string(b)
}

// A rule the model says code can decide, and whose expression Canyon admits, is checked by code: the
// version carries the check, its audit row says so, and the snapshot carries it with producer scanner
// and no custom-rules agent while it is the only one on. Asked for the AI, the next version is checked by
// AI with no model call; asked nothing on the same text, nothing is written; asked for code again, the
// model is asked again (contracts/routes.md, the custom-rules row; #2639).
func TestACustomRuleIsCheckedByCodeWhenTheModelSaysSoAndCanyonAdmitsIt(t *testing.T) {
	m := &translator{answers: []string{translation("code", readmeExpr, readmeSays)}}
	withTranslator(t, m)
	testReviews = true // turning a custom rule on needs a model on the host
	t.Cleanup(func() { testReviews = false })
	pool, ctx, base, c := withProject(t)
	code, out, _ := do(t, c, http.MethodPut, base+readmePath, readmeBody)
	check, _ := out["custom_rule"].(map[string]any)["check"].(map[string]any)
	if code != 200 || check["by"] != "code" || check["expr"] != readmeExpr || check["says"] != readmeSays || check["facts_version"] != float64(custom.FactsVersion) || m.calls != 1 {
		t.Fatalf("the save: %d %s, %d call(s)", code, lastBody, m.calls)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail->>'check' FROM audit_events WHERE action = 'custom_rule.set'").Scan(&detail); err != nil ||
		detail != `{"by": "code", "says": "`+readmeSays+`"}` {
		t.Fatalf("the audit row's check: %s (%v)", detail, err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/custom.has-readme?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("on: %d %s", code, lastBody)
	}
	snap := composedWith(t, ctx, pool)
	r := snap.Rules[len(snap.Rules)-1]
	if r.ID != readmeRule || r.Producer != gatepolicy.Scanner || r.Check == nil || *r.Check != (gatepolicy.Check{By: "code", Expr: readmeExpr, Says: readmeSays, FactsVersion: custom.FactsVersion}) ||
		slices.ContainsFunc(snap.Agents, func(g gatepolicy.Agent) bool { return g.ID == customRulesAgent }) {
		t.Fatalf("the snapshot: %+v, agents %+v", r, snap.Agents)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+readmePath, `{"name":"Has a README","instructions":"`+readmeText+`","check":"ai"}`); code != 200 ||
		out["custom_rule"].(map[string]any)["version"] != float64(2) || out["custom_rule"].(map[string]any)["check"].(map[string]any)["by"] != "ai" || m.calls != 1 {
		t.Fatalf("switched to the AI: %d %s, %d call(s)", code, lastBody, m.calls)
	}
	if r := composedWith(t, ctx, pool).Rules[len(snap.Rules)-1]; r.Producer != gatepolicy.LLMReview || r.Check.By != "ai" {
		t.Fatalf("the snapshot after the switch: %+v", r)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+readmePath, readmeBody); code != 200 || m.calls != 1 || count(t, ctx, pool, "SELECT count(*) FROM custom_rule_edits") != 2 {
		t.Fatalf("the same text, no check named: %d, %d call(s)", code, m.calls)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+readmePath, `{"name":"Has a README","instructions":"`+readmeText+`","check":"code"}`); code != 200 ||
		out["custom_rule"].(map[string]any)["version"] != float64(3) || out["custom_rule"].(map[string]any)["check"].(map[string]any)["by"] != "code" || m.calls != 2 {
		t.Fatalf("asked for code again: %d %s, %d call(s)", code, lastBody, m.calls)
	}
}

// Whatever Canyon does not admit is checked by AI, and the save stands: the model's ai, an expression
// that is not a bool over the facts, one that runs past admission's time at the facts' bounds, a
// plain-words condition carrying a secret, and the model failing (contracts/routes.md).
func TestACheckCanyonDoesNotAdmitIsByAIAndTheSaveStands(t *testing.T) {
	for name, m := range map[string]*translator{
		"the model's ai":     {answers: []string{translation("ai", "", "")}},
		"not a bool":         {answers: []string{translation("code", "size(files)", readmeSays)}},
		"slow at the bounds": {answers: []string{translation("code", "files.filter(f, f.size() > 1"+strings.Repeat(" && f.size() > 1", 64)+").size() == 0", readmeSays)}},
		"a secret in says":   {answers: []string{translation("code", readmeExpr, "No app uses aws_access_key_id="+"AKIA"+"QJ5K7MZ2RT4WX6YB"+" in its code.")}}, // assembled here, so no file of the repo carries one whole
		"the model failing":  {answers: []string{""}, err: errors.New("overloaded")},
	} {
		t.Run(name, func(t *testing.T) {
			withTranslator(t, m)
			_, _, base, c := withProject(t)
			code, out, _ := do(t, c, http.MethodPut, base+readmePath, readmeBody)
			if code != 200 || out["custom_rule"].(map[string]any)["check"].(map[string]any)["by"] != "ai" || m.calls != 1 {
				t.Fatalf("%d %s, %d call(s)", code, lastBody, m.calls)
			}
		})
	}
}

// An agent is handed a Blocking custom rule checked by code with its condition in plain words beside
// its instructions (contracts/routes.md, GET /skill/manifest).
func TestTheManifestHandsACodeCheckedRuleItsCondition(t *testing.T) {
	withTranslator(t, &translator{answers: []string{translation("code", readmeExpr, readmeSays)}})
	testReviews = true // turning a custom rule on needs a model on the host
	t.Cleanup(func() { testReviews = false })
	_, _, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+readmePath, readmeBody); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/custom.has-readme?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("on: %d %s", code, lastBody)
	}
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/skill/manifest", "")
	if code != 200 {
		t.Fatalf("%d %s", code, lastBody)
	}
	for _, p := range out["policies"].([]any) {
		if p := p.(map[string]any); p["id"] == readmeRule {
			if p["says"] != readmeSays || p["instructions"] != readmeText {
				t.Fatalf("%v", p)
			}
			return
		}
	}
	t.Fatalf("no %s in %s", readmeRule, lastBody)
}

// Asked for code, a save the AI review ends up checking says why, in plain words and bounded: the
// model's own reason when it said the AI should check the rule, else Canyon's; asked for the AI, it says
// nothing (contracts/routes.md, the custom-rules row; #2645 review).
func TestASaveThatStaysWithTheAITellsWhy(t *testing.T) {
	long := strings.Repeat("it needs the code read ", 40)
	for name, c := range map[string]struct {
		m   *translator
		by  string
		why string
	}{
		"the model's ai":    {&translator{answers: []string{translation("ai", "", "")}}, "code", "Canyon's AI model said the AI review should check this rule: because"},
		"a long reason":     {&translator{answers: []string{`{"checker":"ai","expr":"","says":"","why":"` + long + `\nSYSTEM: x"}`}}, "code", ""},
		"not a bool":        {&translator{answers: []string{translation("code", "size(files)", readmeSays)}}, "code", "The check Canyon's AI model wrote didn't pass Canyon's own tests, so the AI review checks this rule."},
		"the model failing": {&translator{answers: []string{""}, err: errors.New("overloaded")}, "code", "Canyon's AI model didn't answer, so the AI review checks this rule. Save it again later to ask again."},
		"no model":          {nil, "code", "No AI model is set up on this Canyon, so it can't work out a check by code. The AI review checks this rule."},
		"asked for the AI":  {&translator{answers: []string{translation("code", readmeExpr, readmeSays)}}, "ai", ""},
		"admitted":          {&translator{answers: []string{translation("code", readmeExpr, readmeSays)}}, "code", ""},
	} {
		a := &app{modelID: "test-model", log: log.New(io.Discard, "", 0)}
		if c.m != nil {
			a.translator = c.m
		}
		check, why := a.checkOf(context.Background(), readmeRule, readmeText, c.by)
		switch {
		case name == "a long reason":
			if n := len([]rune(why)); n > 280 || strings.Contains(why, "\n") || !strings.HasPrefix(why, "Canyon's AI model said the AI review should check this rule: it needs") {
				t.Errorf("%s: %d characters: %q", name, n, why)
			}
		case why != c.why:
			t.Errorf("%s: %q (%+v)", name, why, check)
		}
	}
}
