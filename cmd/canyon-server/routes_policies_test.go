package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
)

const policyPath = "/api/v1/policies/auth.unauthenticated-route"

func getPolicies(t *testing.T, c *http.Client, base string) []map[string]any {
	t.Helper()
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/policies", "")
	if code != 200 {
		t.Fatalf("GET /policies: %d %s", code, lastBody)
	}
	rows := out["policies"].([]any)
	got := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.(map[string]any))
	}
	return got
}

func policyOf(t *testing.T, rows []map[string]any, id string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	t.Fatalf("no row for %s in %s", id, lastBody)
	return nil
}

// With no edits the list is the twenty-one rules in the snapshot's order at their canonical states, and
// a never-edited rule carries no edit fields at all — not nulls.
func TestWithNoEditsTheListIsTheSnapshot(t *testing.T) {
	_, _, base, c := withProject(t)
	got := getPolicies(t, c, base)
	boot := testPolicy().Rules
	if len(got) != len(boot) || len(boot) != 21 {
		t.Fatalf("%d rows for %d rules: %s", len(got), len(boot), lastBody)
	}
	for i, r := range boot {
		if row := got[i]; row["id"] != r.ID || row["name"] != r.Name || row["state"] != string(r.State) || len(row) != 3 {
			t.Fatalf("row %d: %v, want %+v with no edit fields", i, row, r)
		}
	}
}

// An applied edit shows as the state with who set it and when; a scheduled one keeps the old
// state and carries the change and its start under scheduled.
func TestAnAppliedEditShowsAndAScheduledOneIsStillAhead(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	edit(t, ctx, pool, "owner.required", "blocking", "0")
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject, set_by_display) VALUES ('auth.unauthenticated-route', 'blocking', 2, 14, now() + interval '14 days', 'person', 'lee@meridian.example', 'Lee Park')"); err != nil {
		t.Fatal(err)
	}
	rows := getPolicies(t, c, base)
	applied := policyOf(t, rows, "owner.required")
	by, _ := applied["set_by"].(map[string]any)
	if applied["state"] != "blocking" || applied["grace_days"] != 0.0 || applied["scheduled"] != nil || by["subject"] != "dana@meridian.example" || by["display"] != "Dana Whitfield" || applied["set_at"] == nil {
		t.Fatalf("the applied edit: %v", applied)
	}
	ahead := policyOf(t, rows, "auth.unauthenticated-route")
	sched, _ := ahead["scheduled"].(map[string]any)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(sched["effective_at"]))
	if ahead["state"] != "advisory" || ahead["grace_days"] != 14.0 || sched["state"] != "blocking" || err != nil || at.Before(time.Now().Add(13*24*time.Hour)) || ahead["set_by"].(map[string]any)["display"] != "Lee Park" {
		t.Fatalf("the scheduled edit: %v (%v)", ahead, err)
	}
	if untouched := policyOf(t, rows, "secrets.client-bundle"); len(untouched) != 3 {
		t.Fatalf("a rule beside the edited ones grew edit fields: %v", untouched)
	}
}

// seedVerdict writes a project's latest verdict by hand — a package, a run and the verdict row —
// carrying one finding per (rule, severity) pair given, under the run id.
func seedVerdict(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project, run string, findings ...string) {
	t.Helper()
	results := make([]string, 0, len(findings))
	for _, f := range findings {
		rule, severity, _ := strings.Cut(f, "@")
		results = append(results, fmt.Sprintf(`{"ruleId": %q, "properties": {"canyon": {"severity": %q, "blocks": false}}}`, rule, severity))
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_%[2]s', '%[1]s', 't', 'c', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('%[2]s', '%[1]s', 'pkg_%[2]s', 'clean', now());
INSERT INTO timeline_rows (scope, run_id, project_id, kind, data) VALUES ('run', '%[2]s', '%[1]s', 'verdict', '{"state": "clean", "findings": {"runs": [{"results": [%[3]s]}]}}')`,
		project, run, strings.Join(results, ", "))); err != nil {
		t.Fatal(err)
	}
}

// The preview counts at the target state, never the current one: the rule is advisory today, so
// at its current state nothing would block. Only the latest verdict per project counts, only
// high and critical findings under this rule count, and a project with none is left out.
func TestThePreviewCountsAtTheTargetState(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"renewal-reminder"}`)
	seedVerdict(t, ctx, pool, "claims-portal", "gr_p3old", "auth.unauthenticated-route@high", "auth.unauthenticated-route@high", "auth.unauthenticated-route@high", "auth.unauthenticated-route@critical")
	seedVerdict(t, ctx, pool, "claims-portal", "gr_p3new", "auth.unauthenticated-route@high", "auth.unauthenticated-route@critical", "auth.unauthenticated-route@low", "secrets.client-bundle@critical")
	seedVerdict(t, ctx, pool, "quote-engine", "gr_p3q", "auth.unauthenticated-route@high")
	seedVerdict(t, ctx, pool, "renewal-reminder", "gr_p3r", "auth.unauthenticated-route@medium")
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"retired-app"}`)
	seedVerdict(t, ctx, pool, "retired-app", "gr_p3x", "auth.unauthenticated-route@critical")
	do(t, c, http.MethodDelete, base+"/api/v1/projects/retired-app?confirm=retired-app", "") // a deleted project's verdict stays on the trail and out of the preview
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath, `{"state":"blocking","grace_days":14}`); code != 200 || lastBody != `{"would_block":[{"project":"claims-portal","findings":2},{"project":"quote-engine","findings":1}]}` {
		t.Fatalf("the preview at blocking: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath, `{"state":"advisory"}`); code != 200 || lastBody != `{"would_block":[]}` {
		t.Fatalf("the preview at advisory: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM policy_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events") - audited; n != 0 {
		t.Fatalf("two previews wrote %d row(s)", n)
	}
	if got := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route"); got["state"] != "advisory" || len(got) != 3 {
		t.Fatalf("a preview moved the rule: %v", got)
	}
}

// The refusals the contract names, each before anything is written — and ?apply=true, which is
// P4's, is refused rather than answered as a preview nobody can tell from an edit.
func TestThePutRefusesWhatTheContractRefuses(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	for _, r := range []struct{ name, path, body, want string }{
		{"a fourth state", policyPath, `{"state":"paused"}`, "invalid_state"},
		{"no state", policyPath, `{"grace_days":3}`, "invalid_state"},
		{"a judgement mode, which no rule takes", policyPath, `{"judgement":"off"}`, "bad_request"},
		{"grace 366", policyPath, `{"state":"blocking","grace_days":366}`, "invalid_grace_days"},
		{"grace -1", policyPath, `{"state":"blocking","grace_days":-1}`, "invalid_grace_days"},
		{"grace 1.5", policyPath, `{"state":"blocking","grace_days":1.5}`, "invalid_grace_days"},
		{"grace as text", policyPath, `{"state":"blocking","grace_days":"14"}`, "invalid_grace_days"},
		{"not json", policyPath, `state=blocking`, "bad_request"},
		{"a rule the snapshot does not carry", "/api/v1/policies/nope.rule", `{"state":"blocking"}`, "not_found"},
	} {
		want := 400
		if r.want == "not_found" {
			want = 404
		}
		if code, body, _ := do(t, c, http.MethodPut, base+r.path, r.body); code != want || body["error"] != r.want {
			t.Errorf("%s: %d %s, want %d %s", r.name, code, lastBody, want, r.want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM policy_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE '%policies%'"); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath, `{"state":"off","grace_days":365}`); code != 200 {
		t.Fatalf("the bounds, inclusive: %d %s", code, lastBody)
	}
}

// The preview's SQL and the gate's derivation are two spellings of one formula. Held together
// over every severity: the seeded verdict's log, one finding per severity under the rule, is
// derived by finding.Derive against a snapshot where the rule blocks, and the preview's count
// must equal the blocks that derivation marks.
func TestThePreviewCountsWhatTheGateWouldBlock(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	seedVerdict(t, ctx, pool, "claims-portal", "gr_sev", "auth.unauthenticated-route@critical", "auth.unauthenticated-route@high", "auth.unauthenticated-route@medium", "auth.unauthenticated-route@low")
	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT data->'findings' FROM timeline_rows WHERE run_id = 'gr_sev' AND kind = 'verdict'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var log finding.Log
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	snap, err := gatepolicy.WithStates(testPolicy(), map[string]gatepolicy.State{"auth.unauthenticated-route": gatepolicy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	if err := finding.Derive(&log, snap); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, r := range log.Runs[0].Results {
		if r.Properties.Canyon.Blocks != nil && *r.Properties.Canyon.Blocks {
			want++
		}
	}
	code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/auth.unauthenticated-route", `{"state":"blocking"}`)
	if want == 0 || code != 200 || lastBody != fmt.Sprintf(`{"would_block":[{"project":"claims-portal","findings":%d}]}`, want) {
		t.Fatalf("the preview over the four severities: %d %s, want %d (what the gate derives)", code, lastBody, want)
	}
}

// A result of checkov's dockerfile checks never blocks, so the preview counts none of them: iac.misconfig
// held blocking would block the one Terraform result, never the two on a Dockerfile (#1881).
func TestThePreviewCountsNoDockerfileCheck(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedVerdict(t, ctx, pool, "claims-portal", "gr_docker", "iac.misconfig.checkov.CKV_DOCKER_2@high", "iac.misconfig.checkov.CKV2_DOCKER_1@critical", "iac.misconfig.checkov.CKV_AWS_24@high")
	for _, put := range []string{`{"state":"blocking"}`, `{"production":"blocking"}`} {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/iac.misconfig", put); code != 200 || lastBody != `{"would_block":[{"project":"claims-portal","findings":1}]}` {
			t.Errorf("%s: %d %s", put, code, lastBody)
		}
	}
}

// manifestState is the state the skill manifest hands out for one rule, "" when it hands none.
func manifestState(m map[string]any, id string) string {
	rows, _ := m["policies"].([]any)
	for _, r := range rows {
		if row, _ := r.(map[string]any); row["id"] == id {
			return fmt.Sprint(row["state"])
		}
	}
	return ""
}

// The apply lands one edit row and one policy.set row in one transaction — the two share a
// transaction id — with the contract's detail, answers the rule as GET now shows it beside the
// preview at the target state, and a second apply moves the version once more.
func TestAnApplyWritesTheEditAndItsAuditRowTogether(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	seedVerdict(t, ctx, pool, "claims-portal", "gr_p4a", "auth.unauthenticated-route@high")
	code, body, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"blocking"}`)
	row, _ := body["policy"].(map[string]any)
	by, _ := row["set_by"].(map[string]any)
	if code != 200 || row["id"] != "auth.unauthenticated-route" || row["state"] != "blocking" || row["grace_days"] != 0.0 || row["scheduled"] != nil || by["subject"] != "anonymous" || row["set_at"] == nil || !strings.HasSuffix(lastBody, `,"would_block":[{"project":"claims-portal","findings":1}]}`) {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	var edits, audits int
	var atOnce, together bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM policy_edits), (SELECT count(*) FROM audit_events WHERE action = 'policy.set'),
		(SELECT bool_and(effective_at <= now() AND version = 1 AND grace_days = 0 AND set_by_kind = 'person') FROM policy_edits),
		(SELECT xmin FROM policy_edits) = (SELECT xmin FROM audit_events WHERE action = 'policy.set')`).Scan(&edits, &audits, &atOnce, &together); err != nil {
		t.Fatal(err)
	}
	if edits != 1 || audits != 1 || !atOnce || !together {
		t.Fatalf("one edit in effect and one audit row in the same transaction: %d edit(s), %d audit row(s), at once %v, together %v", edits, audits, atOnce, together)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil || string(mustJSON(detail)) != `{"after":"blocking","before":"advisory","name":"Every route needs sign-in","rule":"auth.unauthenticated-route"}` {
		t.Fatalf("the detail: %s (%v)", mustJSON(detail), err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("the second apply: %d %s", code, lastBody)
	}
	var versions, before string
	if err := pool.QueryRow(ctx, "SELECT array_agg(version ORDER BY id)::text, (SELECT detail->>'before' FROM audit_events WHERE action = 'policy.set' ORDER BY ts DESC, id DESC LIMIT 1) FROM policy_edits").Scan(&versions, &before); err != nil || versions != "{1,2}" || before != "blocking" || policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route")["state"] != "off" {
		t.Fatalf("the second edit: versions %s, before %s (%v); %s", versions, before, err, lastBody)
	}
}

// The database's clock can step back between two applies (a VM's clock is resynced to its host's
// every few seconds): the second edit is still the newer one, and in effect at once.
func TestAnApplyAfterTheClockStepsBackIsStillTheRulesState(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("the first apply: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE policy_edits SET set_at = now() + interval '1 second', effective_at = now() + interval '1 second'"); err != nil { // as if the clock stepped back a second since
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("the second apply: %d %s", code, lastBody)
	}
	if got := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route"); got["state"] != "off" || got["scheduled"] != nil {
		t.Fatalf("the rule after the clock stepped back: %v, want off with nothing scheduled", got)
	}
}

// An apply with a grace is scheduled: GET shows the old state with the edit ahead, the manifest
// still hands out the old state, and the audit row carries the grace.
func TestAnApplyWithGraceIsScheduledAndBitesNothingYet(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	code, body, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"blocking","grace_days":14}`)
	row, _ := body["policy"].(map[string]any)
	sched, _ := row["scheduled"].(map[string]any)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(sched["effective_at"]))
	if code != 200 || row["state"] != "advisory" || row["grace_days"] != 14.0 || sched["state"] != "blocking" || err != nil || at.Before(time.Now().Add(13*24*time.Hour)) {
		t.Fatalf("the scheduled apply: %d %s (%v)", code, lastBody, err)
	}
	if got := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route"); got["state"] != "advisory" || got["scheduled"] == nil {
		t.Fatalf("GET after the scheduled apply: %v", got)
	}
	if _, m := skillManifest(t, c, base); manifestState(m, "auth.unauthenticated-route") != "advisory" {
		t.Fatalf("the manifest during the grace: %s", lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil || string(mustJSON(detail)) != `{"after":"blocking","before":"advisory","grace_days":14,"name":"Every route needs sign-in","rule":"auth.unauthenticated-route"}` {
		t.Fatalf("the detail: %s (%v)", mustJSON(detail), err)
	}
}

// An applied edit reaches the next manifest read and the next receipt with nothing restarted:
// an off rule leaves the manifest, and owner.required set blocking blocks the unowned project.
func TestAnAppliedEditReachesTheManifestAndTheReceiptWithoutARestart(t *testing.T) {
	_, _, base, c, _ := previewed(t) // a receipt is evaluated only for a preview
	if _, m := skillManifest(t, c, base); manifestState(m, "auth.unauthenticated-route") != "advisory" || manifestState(m, "owner.required") != "advisory" {
		t.Fatalf("the manifest before: %s", lastBody)
	}
	for _, apply := range []struct{ rule, state string }{{"auth.unauthenticated-route", "off"}, {"owner.required", "blocking"}} {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+apply.rule+"?apply=true", `{"state":"`+apply.state+`"}`); code != 200 {
			t.Fatalf("applying %s %s: %d %s", apply.rule, apply.state, code, lastBody)
		}
	}
	if _, m := skillManifest(t, c, base); manifestState(m, "auth.unauthenticated-route") != "" || manifestState(m, "owner.required") != "blocking" {
		t.Fatalf("an applied edit does not reach agents: %s", lastBody)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if r, _ := status["receipt"].(map[string]any); fmt.Sprint(r["blocked_by"]) != "[owner.required]" { // preview's review is clean; nobody owns the app
		t.Fatalf("the receipt after owner.required went blocking: %s", lastBody)
	}
}

// A run started after an apply derives against the edited state: the golden log's critical
// finding under secrets.client-bundle stops blocking once the rule is advisory, so the run ends
// clean with three warnings where the boot snapshot ends it blocked. (The other way round — a
// rule set blocking — the golden log's finding under it lacks the required_end_state the profile
// demands of a blocking finding, and the gate refuses the log, as it should.)
func TestARunStartedAfterAnApplyDerivesAgainstTheEditedState(t *testing.T) {
	_, _, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/secrets.client-bundle?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	if got := waitVerdict(t, c, base, run); got["state"] != "clean" || got["blocking"] != 0.0 || got["warnings"] != 4.0 {
		t.Fatalf("the run derives against the edited state: %s", lastBody)
	}
}

// An apply that changes nothing writes nothing, as PUT /preferences does: the target is the
// effective state, at once, with nothing scheduled. Scheduling the same state ahead is a change,
// and so is withdrawing that schedule at once.
func TestAnApplyThatChangesNothingWritesNothing(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	code, body, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", `{"state":"advisory"}`)
	if row, _ := body["policy"].(map[string]any); code != 200 || len(row) != 3 || row["state"] != "advisory" || string(mustJSON(body["would_block"])) != "[]" {
		t.Fatalf("the no-op apply: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM policy_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events") - audited; n != 0 {
		t.Fatalf("a no-op apply wrote %d row(s)", n)
	}
	for _, body := range []string{`{"state":"blocking","grace_days":14}`, `{"state":"advisory"}`} {
		if code, _, _ := do(t, c, http.MethodPut, base+policyPath+"?apply=true", body); code != 200 {
			t.Fatalf("%s: %d %s", body, code, lastBody)
		}
	}
	got := policyOf(t, getPolicies(t, c, base), "auth.unauthenticated-route")
	if n := count(t, ctx, pool, "SELECT count(*) FROM policy_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events") - audited; got["state"] != "advisory" || got["scheduled"] != nil || n != 4 {
		t.Fatalf("withdrawing a scheduled edit is a change: %v, %d row(s)", got, n)
	}
}

// The demo roster previews as the reader's proof says. The preview counts by severity, and
// demo_seed.sql's two blocked runs are the only findings a demo host holds: a seeded row carrying
// blocks but no severity previews as nothing, and the demo's Policies screen shows an empty
// impact for a rule that is blocking one app.
func TestTheDemoRosterPreviewsAsTwoBlockedProjects(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	code, body, _ := do(t, client(t), http.MethodPut, base+"/api/v1/policies/secrets.client-bundle", `{"state":"blocking"}`)
	if got := fmt.Sprint(body["would_block"]); code != 200 || got != "[map[findings:1 project:claims-portal] map[findings:1 project:commission-calc]]" {
		t.Fatalf("the demo roster at secrets.client-bundle blocking: %d %s", code, lastBody)
	}
}

// now() stamps two fields the golden cannot fix: an apply's set_at, and a scheduled edit's
// effective_at, which a fixed date would carry past its start. Each is checked to land where
// now() puts it, then taken into the golden, so every other byte is compared whole.
func stamped(t *testing.T, got, want map[string]any, key string, from time.Duration) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, fmt.Sprint(got[key]))
	if err != nil || at.Before(time.Now().Add(from-time.Minute)) || at.After(time.Now().Add(from+time.Minute)) {
		t.Fatalf("%s: %v (%v), want now + %s", key, got[key], err, from)
	}
	want[key] = got[key]
}

// The routes answer contracts/testdata/policies/meridian.json whole: the list with one applied and
// one scheduled edit, the preview, the apply and a member's 403. The dashboard's Policies screen
// is built against the golden, so a field the server drops or renames fails here.
func TestThePolicyRoutesAnswerTheGolden(t *testing.T) {
	// auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	withModel(t, &scripted{})
	shipped(t) // the golden is the host as it ships, its default tools on; no run here scans
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
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET actor_display = 'Dana Whitfield' WHERE actor_subject = 'dana@meridian.example';
		INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject, set_by_display, set_at) VALUES
		('owner.required', 'blocking', 1, 0, '2026-09-22T09:14:03Z', 'person', 'dana@meridian.example', 'Dana Whitfield', '2026-09-22T09:14:03Z'),
		('deps.known-vuln', 'blocking', 2, 14, now() + interval '14 days', 'person', 'dana@meridian.example', 'Dana Whitfield', '2026-09-23T08:30:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claims-portal", "quote-engine"} {
		do(t, dana, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`)
	}
	seedVerdict(t, ctx, pool, "claims-portal", "gr_g1", "auth.unauthenticated-route@high", "auth.unauthenticated-route@critical", "auth.unauthenticated-route@low")
	seedVerdict(t, ctx, pool, "quote-engine", "gr_g2", "auth.unauthenticated-route@high")

	code, got, _ := do(t, dana, http.MethodGet, base+"/api/v1/policies", "")
	scheduled := func(doc map[string]any) map[string]any { // deps.known-vuln's, the sixth rule
		rows, _ := doc["policies"].([]any)
		if len(rows) < 6 {
			t.Fatalf("GET /policies answered %d %s", code, lastBody)
		}
		row, _ := rows[5].(map[string]any)
		s, _ := row["scheduled"].(map[string]any)
		return s
	}
	stamped(t, scheduled(got), scheduled(want["policies"]), "effective_at", 14*24*time.Hour)
	if code != 200 || !reflect.DeepEqual(got, want["policies"]) {
		t.Fatalf("GET /policies answered %d %s,\nwant the golden's policies", code, lastBody)
	}
	path := base + "/api/v1/policies/" + fmt.Sprint(want["preview"]["rule"])
	if code, got, _ := do(t, dana, http.MethodPut, path, string(mustJSON(want["preview"]["request"]))); code != 200 || !reflect.DeepEqual(got, want["preview"]["response"]) {
		t.Fatalf("the preview answered %d %s,\nwant the golden's preview", code, lastBody)
	}
	if code, got, _ := do(t, member, http.MethodPut, path+"?apply=true", string(mustJSON(want["apply"]["request"]))); code != 403 || !reflect.DeepEqual(got, want["member"]) {
		t.Fatalf("a member's apply answered %d %s,\nwant the golden's 403", code, lastBody)
	}
	for _, name := range []string{"apply", "scanner_apply"} {
		path = base + "/api/v1/policies/" + fmt.Sprint(want[name]["rule"]) + "?apply=true"
		code, got, _ = do(t, dana, http.MethodPut, path, string(mustJSON(want[name]["request"])))
		res, _ := want[name]["response"].(map[string]any)
		row, _ := got["policy"].(map[string]any)
		stamped(t, row, res["policy"].(map[string]any), "set_at", 0)
		if code != 200 || !reflect.DeepEqual(got, res) {
			t.Fatalf("%s answered %d %s,\nwant the golden's", name, code, lastBody)
		}
	}
}

// gate.unreadable is in no snapshot, so the route answers it as any rule the snapshot does not carry:
// 404 on the preview and the apply alike, before anything is written.
func TestGateUnreadableIsNotFound(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, body := range []string{`{"state":"advisory"}`, `{"run":false}`, `{}`} {
		for _, apply := range []string{"", "?apply=true"} {
			if code, got, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/gate.unreadable"+apply, body); code != 404 || got["error"] != "not_found" {
				t.Errorf("%s %s: %d %s, want 404 not_found", apply, body, code, lastBody)
			}
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
}

const toolPath = "/api/v1/policies/sarif.semgrep"

// policyWrites counts the rows an edit to a rule writes: its edit rows, a tool's run and settings,
// and its policy.set rows.
func policyWrites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM policy_edits") + count(t, ctx, pool, "SELECT count(*) FROM scanner_edits") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'policy.set'")
}

// An imported tool is added by applying its rule: the preview answers that nothing would block,
// writes nothing and leaves the list as it was; the apply registers the tool off and sets its state
// in one edit and one policy.set row with before off, and GET lists it after the snapshot's rules. Added, it is
// edited like any rule: the preview counts its findings, sarif.<tool>.<ruleId>, and no other tool's.
func TestAnApplyAddsAnImportedTool(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+toolPath, `{"state":"blocking"}`); code != 200 || lastBody != `{"would_block":[]}` || policyWrites(t, ctx, pool) != 0 || len(getPolicies(t, c, base)) != len(testPolicy().Rules) {
		t.Fatalf("the preview: %d %s, %d row(s) written", code, lastBody, policyWrites(t, ctx, pool))
	}
	code, body, _ := do(t, c, http.MethodPut, base+toolPath+"?apply=true", `{"state":"advisory"}`)
	row, _ := body["policy"].(map[string]any)
	if code != 200 || row["id"] != "sarif.semgrep" || row["name"] != "Findings from semgrep" || row["state"] != "advisory" || row["grace_days"] != 0.0 || row["set_at"] == nil || string(mustJSON(body["would_block"])) != "[]" {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil || policyWrites(t, ctx, pool) != 2 || string(mustJSON(detail)) != `{"after":"advisory","before":"off","rule":"sarif.semgrep"}` {
		t.Fatalf("one edit and one policy.set row: %d row(s), detail %s (%v)", policyWrites(t, ctx, pool), mustJSON(detail), err)
	}
	if rows, n := getPolicies(t, c, base), len(testPolicy().Rules); len(rows) != n+1 || rows[n]["id"] != "sarif.semgrep" || rows[n]["state"] != "advisory" {
		t.Fatalf("GET after the add: %s", lastBody)
	}
	seedVerdict(t, ctx, pool, "claims-portal", "gr_s1", "sarif.semgrep.no-eval@high", "sarif.semgrep.no-eval@low", "sarif.semgrep-oss.no-eval@high")
	if code, _, _ := do(t, c, http.MethodPut, base+toolPath, `{"state":"blocking"}`); code != 200 || lastBody != `{"would_block":[{"project":"claims-portal","findings":1}]}` {
		t.Fatalf("the preview of an added tool: %d %s", code, lastBody)
	}
}

// A tool id outside the normalised form is a rule no snapshot carries, 404 on the preview and the
// apply alike and before anything is written; a well-formed one is a rule the route serves, so its
// refusals are the rule's. The bounds are inclusive: 40 characters, and adding a tool off adds it.
func TestAToolIDOutsideTheNormalisedFormIsNotFound(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, id := range []string{"sarif.Semgrep", "sarif.-semgrep", "sarif.semgrep-", "sarif.sem--grep", "sarif.sem_grep", "sarif.semgrep.no-eval", "sarif." + strings.Repeat("a", 41), "sarif."} {
		for _, apply := range []string{"", "?apply=true"} {
			if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+id+apply, `{"state":"blocking"}`); code != 404 || body["error"] != "not_found" {
				t.Errorf("%s%s: %d %s, want 404 not_found", id, apply, code, lastBody)
			}
		}
	}
	for body, want := range map[string]string{`{"colour":"red"}`: "bad_request", `{}`: "invalid_state"} {
		if _, got, _ := do(t, c, http.MethodPut, base+toolPath+"?apply=true", body); got["error"] != want {
			t.Errorf("%s: %s, want %s", body, lastBody, want)
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
	longest := "sarif." + strings.Repeat("a", 40)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+longest+"?apply=true", `{"state":"off"}`); code != 200 || policyOf(t, getPolicies(t, c, base), longest)["state"] != "off" {
		t.Fatalf("a 40-character tool added off: %d %s", code, lastBody)
	}
}

// A member cannot add a tool: the preview and the apply are 403 before anything is written.
func TestAMemberCannotAddATool(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	for path, body := range map[string]string{toolPath: `{"state":"blocking"}`, trivyPath: `{"state":"blocking","run":true,"settings":{"scanners":["vuln"]}}`} {
		for _, apply := range []string{"", "?apply=true"} {
			if code, _, _ := do(t, member, http.MethodPut, base+path+apply, body); code != 403 || lastBody != adminRequired {
				t.Errorf("a member's PUT %s%s: %d %s", path, apply, code, lastBody)
			}
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 || len(getPolicies(t, member, base)) != len(testPolicy().Rules) {
		t.Fatalf("a member's refused add wrote %d row(s): %s", n, lastBody)
	}
}

const (
	trivyPath = "/api/v1/policies/sarif.trivy"
	grypePath = "/api/v1/policies/sarif.grype"
)

// A Canyon-run tool not yet added takes run and settings beside the state that adds it: the preview writes nothing;
// the apply answers them on the rule, the composed snapshot carries them — so each run journals them —
// and its one policy.set row names each one's before and after. Setting what is set writes nothing
// and leaves the hash where it was; {} clears the settings and leaves run as it is.
func TestACanyonRunToolTakesRunAndSettings(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	composed := func() gatepolicy.Snapshot {
		t.Helper()
		s, err := (&app{pool: pool, boot: testPolicy()}).effectivePolicy(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	settings := `{"skip_paths":["vendor"]}`
	body := `{"state":"advisory","run":true,"settings":` + settings + `}`
	if code, _, _ := do(t, c, http.MethodPut, base+grypePath, body); code != 200 || lastBody != `{"would_block":[]}` || policyWrites(t, ctx, pool) != 0 {
		t.Fatalf("the preview: %d %s, %d row(s) written", code, lastBody, policyWrites(t, ctx, pool))
	}
	if code, _, _ := do(t, c, http.MethodPut, base+grypePath+"?apply=true", body); code != 200 || !strings.HasSuffix(lastBody, `,"run":true,"settings":`+settings+`},"would_block":[]}`) {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	snap := composed()
	if got := string(mustJSON(snap.Rules[len(snap.Rules)-1])); got != `{"id":"sarif.grype","name":"Findings from grype","state":"advisory","producer":"sarif","run":true,"settings":`+settings+`}` || policyWrites(t, ctx, pool) != 3 {
		t.Fatalf("the snapshot's rule: %s, %d row(s) written", got, policyWrites(t, ctx, pool))
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil ||
		string(mustJSON(detail)) != `{"after":"advisory","before":"off","rule":"sarif.grype","run":{"after":true,"before":false},"settings":{"after":`+settings+`,"before":{}}}` {
		t.Fatalf("the detail: %s (%v)", mustJSON(detail), err)
	}
	if row := policyOf(t, getPolicies(t, c, base), "sarif.grype"); row["run"] != true || string(mustJSON(row["settings"])) != settings {
		t.Fatalf("GET: %v", row)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+grypePath+"?apply=true", `{"run":true,"settings":`+settings+`}`); code != 200 || policyWrites(t, ctx, pool) != 3 || composed().Hash != snap.Hash {
		t.Fatalf("setting what is set: %d %s, %d row(s) written", code, lastBody, policyWrites(t, ctx, pool))
	}
	if code, _, _ := do(t, c, http.MethodPut, base+grypePath+"?apply=true", `{"settings":{}}`); code != 200 || policyWrites(t, ctx, pool) != 5 || composed().Hash == snap.Hash {
		t.Fatalf("clearing the settings: %d %s, %d row(s) written", code, lastBody, policyWrites(t, ctx, pool))
	}
	if row := policyOf(t, getPolicies(t, c, base), "sarif.grype"); row["run"] != true || row["settings"] != nil {
		t.Fatalf("GET after clearing the settings: %v", row)
	}
}

// A tool Canyon runs by default needs no adding: on a host as it ships, its run and settings are
// edited alone, with no state, and the apply's detail names run's before as the default, on.
func TestADefaultToolsRunIsEditedWithoutAState(t *testing.T) {
	shipped(t)
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+trivyPath+"?apply=true", `{"run":false}`); code != 200 {
		t.Fatalf("run off on a default tool: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil ||
		string(mustJSON(detail)) != `{"name":"Findings from trivy","rule":"sarif.trivy","run":{"after":false,"before":true}}` {
		t.Fatalf("the detail: %s (%v)", mustJSON(detail), err)
	}
	if row := policyOf(t, getPolicies(t, c, base), "sarif.trivy"); row["run"] != nil || row["state"] != "advisory" {
		t.Fatalf("GET after run off: %v", row)
	}
}

// The refusals of run and settings, on the preview and the apply alike and before anything is written:
// a rule Canyon does not run is a 409; a tool not yet added takes them only beside the state that adds
// it; a key outside the tool's allow-list, or a value its shape refuses, is invalid_settings naming the
// first in the body's order; and a key the route does not take is refused, never ignored.
func TestRunAndSettingsRefusalsWriteNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	sha := strings.Repeat("a", 40)
	for _, r := range []struct {
		rule, body string
		code       int
		want       string
	}{
		{"sarif.semgrep", `{"state":"advisory","run":true}`, 409, "run_not_offered"},
		{"secrets.client-bundle", `{"settings":{}}`, 409, "run_not_offered"},
		{"sarif.grype", `{"run":true}`, 400, "invalid_state"},
		{"sarif.trivy", `{"state":"off","settings":{"scanners":["vuln"],"frameworks":["terraform"]}}`, 400, "invalid_settings"},
		{"sarif.trivy", `{"state":"off","settings":{"scanners":["vuln","license"]}}`, 400, "invalid_settings"},
		{"sarif.trivy", `{"state":"off","settings":{"scanners":["vuln","vuln"]}}`, 400, "invalid_settings"},
		{"sarif.trivy", `{"state":"off","settings":{"scanners":[]}}`, 400, "invalid_settings"},
		{"sarif.grype", `{"state":"off","settings":{"skip_paths":["-rf"]}}`, 400, "invalid_settings"},
		{"sarif.grype", `{"state":"off","settings":{"skip_paths":["a/../b"]}}`, 400, "invalid_settings"},
		{"sarif.grype", `{"state":"off","settings":{"skip_paths":["/etc"]}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"http://rules.example/r","commit":"` + sha + `"}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"main"}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r\u001b[2J","commit":"` + sha + `"}}}`, 400, "invalid_settings"}, // shown on the Policies screen and the trail
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules\u007f.example/r","commit":"` + sha + `"}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"` + sha + `","paths":["../ruby"]}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"` + sha + `","paths":["ruby","ruby"]}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"` + sha + `","paths":["java/"]}}}`, 400, "invalid_settings"},
		{"sarif.opengrep", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"` + sha + `","folders":["ruby"]}}}`, 400, "invalid_settings"},
		{"sarif.checkov", `{"state":"off","settings":{"frameworks":["kubernetes"]}}`, 400, "invalid_settings"},
		{"sarif.checkov", `{"state":"off","settings":{"skip_checks":["BC_AWS_1"]}}`, 400, "invalid_settings"},
		{"sarif.checkov", `{"state":"off","settings":null}`, 400, "invalid_settings"},
		{"sarif.trivy", `{"state":"off","run":true,"colour":"red"}`, 400, "bad_request"},
	} {
		for _, apply := range []string{"", "?apply=true"} {
			if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+r.rule+apply, r.body); code != r.code || body["error"] != r.want {
				t.Errorf("%s%s %s: %d %s, want %d %s", r.rule, apply, r.body, code, lastBody, r.code, r.want)
			}
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+trivyPath+"?apply=true", `{"state":"off","settings":{"skip_paths":["-rf"],"colour":"red"}}`); code != 400 ||
		lastBody != `{"error":"invalid_settings","message":"settings \"skip_paths\" is at most 64 distinct paths of the package, each at most 256 characters, never absolute, never through .. and never starting with -"}` {
		t.Errorf("the first to fail is named: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/sarif.grype?apply=true", `{"state":"off","settings":{"scanners":["vuln"]}}`); code != 400 ||
		lastBody != `{"error":"invalid_settings","message":"settings \"scanners\": grype takes skip_paths"}` {
		t.Errorf("another tool's key: %d %s", code, lastBody)
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/sarif.opengrep?apply=true", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"`+sha+`"},"skip_paths":["..a/.b"]}}`); code != 200 {
		t.Fatalf("opengrep's rules and a path that only starts with dots: %d %s", code, lastBody)
	}
	_, body, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/sarif.opengrep?apply=true", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"`+sha+`","paths":["ruby","php/lang"]}}}`)
	if rules := fmt.Sprint(body["policy"].(map[string]any)["settings"]); rules != "map[rules:map[commit:"+sha+" paths:[php/lang ruby] repo:https://rules.example/r]]" { // sorted: one spelling
		t.Fatalf("opengrep's rules with paths: %s", lastBody)
	}
	_, body, _ = do(t, c, http.MethodPut, base+"/api/v1/policies/sarif.opengrep?apply=true", `{"state":"off","settings":{"rules":{"repo":"https://rules.example/r","commit":"`+sha+`","paths":[]}}}`)
	if rules := fmt.Sprint(body["policy"].(map[string]any)["settings"]); rules != "map[rules:map[commit:"+sha+" repo:https://rules.example/r]]" {
		t.Fatalf("no paths is the one spelling of all of them: %s", lastBody)
	}
}

// The gate before production answers contracts/testdata/policies/meridian.json: the apply that places
// a rule there, its preview read from each project's promote_run, and its policy.set row; production on
// a go-live rule is refused on the preview and the apply alike, writing nothing. A production outside
// the three is invalid_state, and a grace schedules it as it does a state, production_scheduled: each
// gate's pending edit is its own, neither hiding the other's.
func TestPolicyGateBeforeProductionAnswersTheGolden(t *testing.T) {
	shipped(t)
	withOIDC(t)
	withModel(t, &scripted{}) // placing an AI rule needs a model on the host
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
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET actor_display = 'Dana Whitfield'`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claims-portal", "quote-engine"} {
		do(t, dana, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`)
	}
	seedVerdict(t, ctx, pool, "quote-engine", "gr_q1", "deps.known-vuln@high")
	seedVerdict(t, ctx, pool, "quote-engine", "gr_q2", "deps.known-vuln@high", "deps.known-vuln@critical")                                                                                                                        // the latest verdict, of a package preview does not run: before production never reads it
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, gate_run, image, url) VALUES ('quote-engine', 'preview', 'pkg_gr_q1', 'gr_q1', 'img', 'http://quote-engine.preview.localhost')`); err != nil { // promote_run is gr_q1, the newest run of what preview runs
		t.Fatal(err)
	}
	if _, st, _ := do(t, dana, http.MethodGet, base+"/api/v1/projects/quote-engine/status", ""); st["promote_run"] != "gr_q1" { // the preview read picks the run the status read does
		t.Fatalf("quote-engine's promote_run: %s", lastBody)
	}
	pa, ps := want["production_apply"], want["production_scheduled"]
	path := base + "/api/v1/policies/" + fmt.Sprint(pa["rule"]) + "?apply=true"
	code, got, _ := do(t, dana, http.MethodPut, path, string(mustJSON(ps["request"]))) // pending first, then superseded by the apply in force now
	sres, _ := ps["response"].(map[string]any)
	if row, _ := got["policy"].(map[string]any); row != nil {
		stamped(t, row, sres["policy"].(map[string]any), "set_at", 0)
		if p, ok := row["scheduled_production"].(map[string]any); ok {
			stamped(t, p, sres["policy"].(map[string]any)["scheduled_production"].(map[string]any), "effective_at", 14*24*time.Hour)
		}
	}
	if code != 200 || !reflect.DeepEqual(got, sres) {
		t.Fatalf("production_scheduled answered %d %s,\nwant the golden's", code, lastBody)
	}
	code, got, _ = do(t, dana, http.MethodPut, path, string(mustJSON(pa["request"])))
	res, _ := pa["response"].(map[string]any)
	row, _ := got["policy"].(map[string]any)
	stamped(t, row, res["policy"].(map[string]any), "set_at", 0)
	if code != 200 || !reflect.DeepEqual(got, res) {
		t.Fatalf("production_apply answered %d %s,\nwant the golden's", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'policy.set' ORDER BY ts DESC LIMIT 1").Scan(&detail); err != nil || !reflect.DeepEqual(detail, pa["audit"].(map[string]any)["detail"]) {
		t.Fatalf("the policy.set row: %v %v", detail, err)
	}
	writes := policyWrites(t, ctx, pool)
	no := want["production_not_offered"]
	for _, apply := range []string{"", "?apply=true"} {
		if code, got, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+fmt.Sprint(no["rule"])+apply, string(mustJSON(no["request"]))); code != 409 || !reflect.DeepEqual(got, no["response"]) {
			t.Fatalf("production_not_offered%s answered %d %s", apply, code, lastBody)
		}
	}
	if code, got, _ := do(t, dana, http.MethodPut, path, `{"production":"loud"}`); code != 400 || got["error"] != "invalid_state" {
		t.Fatalf("a production outside the three: %d %s", code, lastBody)
	}
	if n := policyWrites(t, ctx, pool); n != writes {
		t.Fatalf("the refusals wrote %d row(s)", n-writes)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+policyPath+"?apply=true", `{"production":"advisory","grace_days":14}`); code != 200 {
		t.Fatalf("a graced production: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+policyPath+"?apply=true", `{"state":"blocking","grace_days":7}`); code != 200 {
		t.Fatalf("a graced state: %d %s", code, lastBody)
	}
	r := policyOf(t, getPolicies(t, dana, base), "auth.unauthenticated-route")
	pre, _ := r["scheduled"].(map[string]any)
	prod, _ := r["scheduled_production"].(map[string]any)
	if r["production"] != nil || pre["state"] != "blocking" || prod["state"] != "advisory" || pre["effective_at"] == prod["effective_at"] {
		t.Fatalf("each gate's pending edit, each with its own instant: %s", lastBody)
	}
}

// No CLI verb changes the pipeline: an agent or canyon actor is refused 403 person_required on the
// preview and the apply alike, before anything is written (contracts/routes.md, the policies row).
func TestPolicyAnAgentOrCanyonChangesNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, kind := range []string{"agent", "canyon"} {
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1", kind); err != nil {
			t.Fatal(err)
		}
		for _, apply := range []string{"", "?apply=true"} {
			if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/deps.known-vuln"+apply, `{"production":"blocking"}`); code != 403 || lastBody != `{"error":"person_required","message":"only a person can change a policy"}` {
				t.Errorf("%s%s: %d %s", kind, apply, code, lastBody)
			}
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
}

// A member placing a rule is refused admin_required, as any policy edit is, writing nothing.
func TestPolicyAMemberPlacesNothing(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	for _, apply := range []string{"", "?apply=true"} {
		if code, _, _ := do(t, member, http.MethodPut, base+"/api/v1/policies/deps.known-vuln"+apply, `{"production":"blocking"}`); code != 403 || lastBody != adminRequired {
			t.Errorf("a member%s: %d %s", apply, code, lastBody)
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
}

// One PUT may set both gates: one edit row naming both, its policy.set row carrying both pairs; with a
// grace both wait, scheduled and scheduled_production each at the edit's one instant.
func TestPolicyOnePutSetsBothGates(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	path := base + "/api/v1/policies/deps.known-vuln"
	if code, _, _ := do(t, c, http.MethodPut, path, `{"state":"blocking","production":"blocking"}`); code != 200 || policyWrites(t, ctx, pool) != 0 {
		t.Fatalf("the preview: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"blocking","production":"blocking"}`); code != 200 || got["policy"].(map[string]any)["state"] != "blocking" || got["policy"].(map[string]any)["production"] != "blocking" {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'policy.set'").Scan(&detail); err != nil || count(t, ctx, pool, "SELECT count(*) FROM policy_edits") != 1 ||
		detail != `{"name": "No known-vulnerable dependencies", "rule": "deps.known-vuln", "after": "blocking", "before": "advisory", "production": {"after": "blocking", "before": "off"}}` {
		t.Fatalf("one edit, one row with both pairs: %s %v", detail, err)
	}
	if code, _, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"off","production":"advisory","grace_days":3}`); code != 200 {
		t.Fatalf("a graced edit of both gates: %d %s", code, lastBody)
	}
	r := policyOf(t, getPolicies(t, c, base), "deps.known-vuln")
	pre, _ := r["scheduled"].(map[string]any)
	prod, _ := r["scheduled_production"].(map[string]any)
	if pre["state"] != "off" || prod["state"] != "advisory" || pre["effective_at"] == nil || pre["effective_at"] != prod["effective_at"] {
		t.Fatalf("both gates wait for the one instant: %s", lastBody)
	}
}

// With no model on the host, placing a rule the AI review checks, or setting it blocking before preview,
// is 409 no_model on the preview and the apply alike, writing nothing:
// contracts/testdata/policies/meridian.json's production_no_model. Advisory before preview, and placing
// a scanner's rule, are not refused.
func TestPolicyPlacingAnAIRuleNeedsAModel(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	raw, err := os.ReadFile("../../contracts/testdata/policies/meridian.json")
	var want map[string]map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &want)
	}
	if err != nil {
		t.Fatal(err)
	}
	nm := want["production_no_model"]
	for _, apply := range []string{"", "?apply=true"} {
		for _, req := range []string{string(mustJSON(nm["request"])), `{"state":"blocking"}`} { // blocking before preview would block every run unchecked (#1555)
			if code, got, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+fmt.Sprint(nm["rule"])+apply, req); code != 409 || !reflect.DeepEqual(got, nm["response"]) {
				t.Errorf("production_no_model%s %s: %d %s", apply, req, code, lastBody)
			}
		}
	}
	if n := policyWrites(t, ctx, pool); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
	for path, body := range map[string]string{policyPath: `{"state":"advisory"}`, "/api/v1/policies/deps.known-vuln": `{"production":"blocking"}`} {
		if code, _, _ := do(t, c, http.MethodPut, base+path+"?apply=true", body); code != 200 {
			t.Errorf("%s %s: %d %s", path, body, code, lastBody)
		}
	}
}

// Placing a rule Blocking before production names, in would_block, a project whose promote_run did not
// have the rule on, at 0 findings — its promotion would be held, the receipt having nothing of the rule
// to read — and not one whose promote_run had it on and found nothing that blocks.
func TestPolicyPlacingARuleNamesWhatNeverRanIt(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	review := func(project string) {
		t.Helper()
		upload(t, c, base, project, tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/"+project+"/gate-runs", "")
		run, _ := runOf(started)["id"].(string)
		waitVerdict(t, c, base, run)
	}
	path := base + "/api/v1/policies/design.org-system"
	if code, _, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("off: %d %s", code, lastBody)
	}
	review("claims-portal") // design.org-system off
	if code, _, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("advisory: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	review("quote-engine") // design.org-system on, nothing of it found
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, _, _ := do(t, c, http.MethodPut, path, `{"production":"blocking"}`); code != 200 || lastBody != `{"would_block":[{"project":"claims-portal","findings":0}]}` {
		t.Fatalf("the preview: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, path, `{"production":"advisory"}`); code != 200 || lastBody != `{"would_block":[]}` {
		t.Fatalf("advisory holds nothing: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events") - audited; n != 0 {
		t.Fatalf("the previews wrote %d row(s)", n)
	}
}

// Before preview, a preview at blocking names in unchecked each project whose latest review ran with the rule off and
// holds nothing of it, until a review with the rule on (#155): a rule Off, then Advisory, then previewed at Blocking
// with no review since. Never a project whose verdict holds a result under the rule, which was checked, nor below
// blocking, nor before production, where such a project is held in would_block at 0 findings.
func TestPoliciesPreviewNamesWhatNoReviewChecked(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	review := func(project string) {
		t.Helper()
		upload(t, c, base, project, tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/"+project+"/gate-runs", "")
		run, _ := runOf(started)["id"].(string)
		waitVerdict(t, c, base, run)
	}
	path := base + "/api/v1/policies/design.org-system"
	if code, _, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("off: %d %s", code, lastBody)
	}
	review("claims-portal") // design.org-system off
	if code, _, _ := do(t, c, http.MethodPut, path+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("advisory: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	review("quote-engine") // design.org-system on, nothing of it found
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"renewal-reminder"}`)
	seedVerdict(t, ctx, pool, "renewal-reminder", "gr_unchecked", "design.org-system@medium") // no journal, but its verdict holds the rule
	audited := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	for put, want := range map[string]string{
		`{"state":"blocking"}`:      `{"unchecked":["claims-portal"],"would_block":[]}`,
		`{"state":"advisory"}`:      `{"would_block":[]}`,
		`{"production":"blocking"}`: `{"would_block":[{"project":"claims-portal","findings":0},{"project":"renewal-reminder","findings":0}]}`,
	} {
		if code, _, _ := do(t, c, http.MethodPut, path, put); code != 200 || lastBody != want {
			t.Errorf("the preview of %s: %d %s, want %s", put, code, lastBody, want)
		}
	}
	review("claims-portal") // design.org-system on now
	if code, _, _ := do(t, c, http.MethodPut, path, `{"state":"blocking"}`); code != 200 || lastBody != `{"would_block":[]}` {
		t.Fatalf("the preview once every app is reviewed with the rule on: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events") - audited; n != 0 {
		t.Fatalf("the previews wrote %d row(s)", n)
	}
}

// seedLive gives project, added if it is new, a run and a deploy in each of envs; with no envs it is a live app
// with nothing deployed.
func seedLive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project string, envs ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $1) ON CONFLICT DO NOTHING", project); err != nil {
		t.Fatal(err)
	}
	run := "gr_" + project
	seedVerdict(t, ctx, pool, project, run)
	for _, env := range envs {
		if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) VALUES ($1, $2, $3, $4, $5, $6)",
			project, env, "pkg_"+run, "canyon/"+project+"@sha256:0", "http://"+project+"."+env+".localhost:7777", run); err != nil {
			t.Fatal(err)
		}
	}
}

// holdEgress gives project an addition of prod.egress-allowed at state.
func holdEgress(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project, state string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO app_policy_additions (project_id, rule, state, set_by_kind, set_by_subject) VALUES ($1, $2, $3, 'person', 'sub-olive')", project, egressRule, state); err != nil {
		t.Fatal(err)
	}
}

// egressPreview is who the preview at path holds at state, in order, each at 0 findings or it fails: a preview seedLive
// wrote records no spec, so its row reads could not check.
func egressPreview(t *testing.T, c *http.Client, path, state string) string {
	t.Helper()
	code, out, _ := do(t, c, http.MethodPut, path, `{"state":"`+state+`"}`)
	if code != 200 {
		t.Fatalf("the preview of %s at %s: %d %s", path, state, code, lastBody)
	}
	rows, _ := out["would_block"].([]any)
	held := make([]string, 0, len(rows))
	for _, row := range rows {
		r, _ := row.(map[string]any)
		if r["findings"] != 0.0 {
			t.Fatalf("the preview of %s at %s counts findings: %s", path, state, lastBody)
		}
		held = append(held, fmt.Sprint(r["project"]))
	}
	return strings.Join(held, " ")
}

// prod.egress-allowed is in no verdict, so its preview reads each live app's preview as its receipt row does: neither
// here is through Canyon, so each reads could not check, at 0 findings. Never one with nothing deployed, nor a deleted
// one. An app's addition holds that app alone, and below blocking nothing (contracts/routes.md, the policies row).
func TestTheEgressRulePreviewReadsEachLiveApp(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedLive(t, ctx, pool, "claims-portal", "preview")
	seedLive(t, ctx, pool, "ledger", "preview")
	seedLive(t, ctx, pool, "unshipped")
	seedLive(t, ctx, pool, "gone", "preview", "prod")
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'gone'"); err != nil {
		t.Fatal(err)
	}
	org, addition := base+"/api/v1/policies/"+egressRule, base+additionAt+egressRule
	for _, want := range []struct{ path, state, held string }{
		{org, "blocking", "claims-portal ledger"},
		{org, "advisory", ""},
		{addition, "blocking", "claims-portal"},
		{addition, "advisory", ""},
	} {
		if got := egressPreview(t, c, want.path, want.state); got != want.held {
			t.Errorf("%s at %s holds %q, want %q", want.path, want.state, got, want.held)
		}
	}
}

// An app's blocking addition holds it whatever the org's state, and the org's Off doesn't clear it, so the org's
// row names it below blocking: that app alone, never one whose addition only warns, one with none, or a deleted
// one. An addition's own preview replaces what the app holds, so softening it names nobody.
func TestTheEgressRulePreviewNamesAnAppItsOwnAdditionHolds(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedLive(t, ctx, pool, "claims-portal", "preview")
	seedLive(t, ctx, pool, "ledger", "preview")
	seedLive(t, ctx, pool, "payroll", "preview")
	seedLive(t, ctx, pool, "warned", "preview")
	seedLive(t, ctx, pool, "gone", "preview")
	for project, state := range map[string]string{"claims-portal": "blocking", "payroll": "blocking", "warned": "advisory", "gone": "blocking"} {
		holdEgress(t, ctx, pool, project, state)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'gone'"); err != nil {
		t.Fatal(err)
	}
	org, addition := base+"/api/v1/policies/"+egressRule, base+additionAt+egressRule
	for _, state := range []string{"off", "advisory"} {
		if got := egressPreview(t, c, org, state); got != "claims-portal payroll" {
			t.Errorf("the org at %s holds %q, want the two apps whose own addition blocks", state, got)
		}
	}
	if got := egressPreview(t, c, org, "blocking"); got != "claims-portal ledger payroll warned" {
		t.Errorf("the org at blocking holds %q", got)
	}
	if got := egressPreview(t, c, addition, "advisory"); got != "" {
		t.Errorf("an addition softened to advisory holds %q: it replaces the one that blocks", got)
	}
}

// With nothing in preview a receipt reads no row and a rollback's never holds, so an app in production alone is named
// nowhere, while one in both is, once however many environments it is in (contracts/routes.md, the policies row).
func TestTheEgressRulePreviewNamesNoAppInProductionAlone(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedLive(t, ctx, pool, "claims-portal", "prod")
	seedLive(t, ctx, pool, "ledger", "preview", "prod")
	org, addition := base+"/api/v1/policies/"+egressRule, base+additionAt+egressRule
	if got := egressPreview(t, c, org, "blocking"); got != "ledger" {
		t.Errorf("the org at blocking holds %q", got)
	}
	if got := egressPreview(t, c, addition, "blocking"); got != "" {
		t.Errorf("an addition on an app in production alone holds %q", got)
	}
}

// platform.known-vuln is read from the image preview runs, never a verdict (contracts/routes.md, The promote
// receipt), so its preview counts what the receipt row would hold: an image with no scan recorded at no
// package, and once scanned the packages it counts; turned off, nothing (#2264).
func TestThePlatformRulePreviewCountsImageMatches(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	preview := func(state string) string {
		t.Helper()
		code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+platformRule, `{"state":"`+state+`"}`)
		if code != 200 {
			t.Fatalf("the preview at %s: %d %s", state, code, lastBody)
		}
		return string(mustJSON(out["would_block"]))
	}
	if got := preview("blocking"); got != `[{"findings":0,"project":"claims-portal"}]` {
		t.Fatalf("no image scan recorded, held all the same: %s", got)
	}
	seedPlatform(t, ctx, pool, loadPlatformGolden(t), "claims-portal", executortest.Digest)
	if got := preview("blocking"); got != `[{"findings":3,"project":"claims-portal"}]` {
		t.Fatalf("the image's three counted packages: %s", got)
	}
	if got := preview("advisory"); got != `[]` {
		t.Fatalf("advisory holds nothing: %s", got)
	}
}
