package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
)

const additionAt = "/api/v1/projects/claims-portal/policies/"

// An app's additions answer contracts/testdata/policies/meridian.json: the preview reads the app's
// promote_run and writes nothing; the apply answers the rule's row with the addition on it, and one
// app_policy.set row naming the gate whose state in force it changed; one that would weaken is
// not_stricter. The additions show to the app's team and admins on GET /policies and the audit read's
// rule filter, never to anyone else, and a DELETE removes the addition with one app_policy.remove row.
func TestPolicyAppAdditionsAnswerTheGolden(t *testing.T) {
	withModel(t, &scripted{}) // placing an AI rule needs a model on the host (#1555)
	pool, ctx, base, olive, carl, vic, nora, dana := teamOf(t)
	raw, err := os.ReadFile("../../contracts/testdata/policies/meridian.json")
	var want map[string]map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &want)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET actor_subject = 'dana@meridian.example', actor_display = 'Dana Whitfield' WHERE actor_subject = 'sub-dana'`); err != nil {
		t.Fatal(err)
	}
	seedVerdict(t, ctx, pool, "claims-portal", "gr_a1", "auth.unauthenticated-route@high", "auth.unauthenticated-route@critical", "auth.unauthenticated-route@medium")
	put := want["app_policy_put"]
	path := base + additionAt + fmt.Sprint(put["rule"])
	if code, got, _ := do(t, olive, http.MethodPut, path, string(mustJSON(put["request"]))); code != 200 || !reflect.DeepEqual(got["would_block"], put["response"].(map[string]any)["would_block"]) || len(got) != 1 {
		t.Fatalf("the preview: %d %s", code, lastBody)
	}
	written := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM app_policy_additions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE 'app_policy.%'")
	}
	if n := written(); n != 0 {
		t.Fatalf("the preview wrote %d row(s)", n)
	}
	code, got, _ := do(t, dana, http.MethodPut, path+"?apply=true", string(mustJSON(put["request"])))
	res, _ := put["response"].(map[string]any)
	adds, _ := got["policy"].(map[string]any)["additions"].([]any)
	if len(adds) == 1 {
		stamped(t, adds[0].(map[string]any), res["policy"].(map[string]any)["additions"].([]any)[0].(map[string]any), "set_at", 0)
	}
	if code != 200 || !reflect.DeepEqual(got, res) {
		t.Fatalf("app_policy_put answered %d %s,\nwant the golden's", code, lastBody)
	}
	var row struct {
		Action, Project string
		Detail          map[string]any
	}
	if err := pool.QueryRow(ctx, "SELECT action, project_id, detail FROM audit_events WHERE action LIKE 'app_policy.%'").Scan(&row.Action, &row.Project, &row.Detail); err != nil ||
		!reflect.DeepEqual(map[string]any{"action": row.Action, "project": row.Project, "detail": row.Detail}, put["audit"]) {
		t.Fatalf("the app_policy.set row: %+v %v", row, err)
	}
	for who, c := range map[string]*http.Client{"the owner": olive, "a contributor": carl, "an admin": dana} {
		if r := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route"); !reflect.DeepEqual(r["additions"], got["policy"].(map[string]any)["additions"]) {
			t.Errorf("%s reads the additions: %v", who, r["additions"])
		}
		if _, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?rule=auth.unauthenticated-route", ""); len(page["events"].([]any)) != 1 {
			t.Errorf("%s reads the rule's history: %s", who, lastBody)
		}
	}
	for who, c := range map[string]*http.Client{"a viewer": vic, "a member with no access": nora} {
		if r := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route"); r["additions"] != nil {
			t.Errorf("%s reads the additions: %v", who, r["additions"])
		}
		if _, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?rule=auth.unauthenticated-route", ""); len(page["events"].([]any)) != 0 {
			t.Errorf("%s reads the app's history: %s", who, lastBody)
		}
	}
	before := written()
	ns := want["not_stricter"]
	for _, c := range []struct{ rule, body, want string }{
		{fmt.Sprint(ns["rule"]), string(mustJSON(ns["request"])), string(mustJSON(ns["response"]))},
		{"secrets.client-bundle", `{"production":"blocking","state":"blocking"}`, string(mustJSON(ns["response"]))}, // either field not stricter refuses the whole
		{"gate.unreadable", `{"state":"blocking"}`, `{"error":"not_found","message":"no policy carries that id"}`},
		{"auth.unauthenticated-route", `{}`, `{"error":"invalid_state","message":"the body is state and production, each off, advisory or blocking"}`},
		{"auth.unauthenticated-route", `{"state":"loud"}`, `{"error":"invalid_state","message":"the body is state and production, each off, advisory or blocking"}`},
		{"sarif.trivy", `{"state":"blocking","run":false}`, `{"error":"invalid_state","message":"the body is state and production, each off, advisory or blocking"}`},
		{"auth.unauthenticated-route", `{"state":"blocking","grace_days":14}`, `{"error":"invalid_grace_days","message":"an app's addition takes effect at once: it takes no grace_days"}`},
		{"prod.human-approval", `{"production":"blocking"}`, `{"error":"production_not_offered","message":"` + productionNotOffered + `"}`},
	} {
		for _, apply := range []string{"", "?apply=true"} {
			if _, _, _ = do(t, olive, http.MethodPut, base+additionAt+c.rule+apply, c.body); lastBody != c.want {
				t.Errorf("%s %s%s: %s, want %s", c.rule, c.body, apply, lastBody, c.want)
			}
		}
	}
	if code, _, _ := do(t, carl, http.MethodPut, path+"?apply=true", `{"state":"blocking"}`); code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive") {
		t.Errorf("a contributor: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/projects/nope/policies/auth.unauthenticated-route?apply=true", `{"state":"blocking"}`); code != 404 {
		t.Errorf("an unknown app: %d %s", code, lastBody)
	}
	if n := written(); n != before {
		t.Fatalf("the refusals wrote %d row(s)", n-before)
	}
	if code, _, _ := do(t, olive, http.MethodDelete, path, ""); code != 204 {
		t.Fatalf("the delete: %d %s", code, lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'app_policy.remove' AND project_id = 'claims-portal'").Scan(&detail); err != nil || detail != `{"rule": "auth.unauthenticated-route"}` {
		t.Fatalf("the app_policy.remove row: %s %v", detail, err)
	}
	if code, _, _ := do(t, olive, http.MethodDelete, path, ""); code != 404 || policyOf(t, getPolicies(t, dana, base), "auth.unauthenticated-route")["additions"] != nil {
		t.Fatalf("the addition is gone: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-olive'"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if code, _, _ := do(t, olive, method, path+"?apply=true", `{"state":"blocking"}`); code != 403 || !strings.Contains(lastBody, `"error":"person_required"`) {
			t.Errorf("an agent's %s: %d %s", method, code, lastBody)
		}
	}
}

// A project's runs use its effective snapshot, contracts/testdata/snapshot/claims-portal.json: the
// org's, with the app's additions applied in each gate, at the org's version and its own hash — so the
// additions are in the verdict cache's key — and the run journals it; a project with none runs the org's.
func TestPolicyProjectsRunsUseItsEffectiveSnapshot(t *testing.T) {
	// placing an AI rule needs a model on the host (#1555)
	withModel(t, &scripted{})
	shipped(t) // the golden is the host as it ships, its default tools on
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	if code, _, _ := do(t, c, http.MethodPut, base+additionAt+"auth.unauthenticated-route?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("the addition: %d %s", code, lastBody)
	}
	a := &app{pool: pool, boot: testPolicy()}
	org, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.projectPolicy(ctx, pool, "claims-portal")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../contracts/testdata/snapshot/claims-portal.json")
	var golden gatepolicy.Snapshot
	if err == nil {
		err = json.Unmarshal(raw, &golden)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(got.Rules)) != string(mustJSON(golden.Rules)) || got.Version != org.Version || got.Hash == org.Hash {
		t.Fatalf("claims-portal's snapshot: v%d %s, the org's v%d %s\n%+v", got.Version, got.Hash, org.Version, org.Hash, got.Rules)
	}
	if other, err := a.projectPolicy(ctx, pool, "quote-engine"); err != nil || !reflect.DeepEqual(other, org) {
		t.Fatalf("a project with no additions: %v", err)
	}
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	waitVerdict(t, c, base, run)
	ws, err := durable.ListWorkflows(durable, dbos.WithFilterWorkflowIDs(run), dbos.WithFilterLoadInput(true))
	var in runs.Input
	if err == nil && len(ws) == 1 {
		s, _ := ws[0].Input.(string)
		err = json.Unmarshal([]byte(s), &in)
	}
	if err != nil || in.Policy.Hash != got.Hash {
		t.Fatalf("the run journals %s, want claims-portal's %s (%v)", in.Policy.Hash, got.Hash, err)
	}
}

// With no model on the host, an app's addition that would leave an AI rule unchecked where it blocks —
// blocking before preview, or placed before production — is 409 no_model as the org's own edit is,
// and writes nothing (#1555).
func TestPolicyAnAdditionOfAnAIRuleNeedsAModel(t *testing.T) {
	pool, ctx, base, olive, _, _, _, _ := teamOf(t)
	path := base + additionAt + "auth.unauthenticated-route?apply=true"
	for _, body := range []string{`{"state":"blocking"}`, `{"production":"advisory"}`} {
		if code, got, _ := do(t, olive, http.MethodPut, path, body); code != 409 || got["error"] != "no_model" || got["message"] != noModelReview {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM app_policy_additions"); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
}
