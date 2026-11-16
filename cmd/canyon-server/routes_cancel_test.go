package main

import (
	"net/http"
	"testing"
)

const (
	cancelRoute   = "POST /api/v1/gate-runs/{id}/cancel"
	cancelAudited = "SELECT count(*) FROM audit_events WHERE action = '" + cancelRoute + "'"
)

// A pending run ends at once, interrupted, with one audit row naming who started it; a running one is
// asked once to end itself; a run whose deploy began, or that ended, is refused and nothing is written.
func TestCancelEndsAPendingRunAsksARunningOneAndRefusesOnceTheDeployBegan(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	insert := func(id, state, kind string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display)
			SELECT $1, project_id, id, $2, $3, 'agent', 'claude-code:dana@meridian.example', 'Claude Code' FROM packages LIMIT 1`, id, state, kind); err != nil {
			t.Fatal(err)
		}
	}
	cancel := func(id string) (int, map[string]any) {
		t.Helper()
		code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/"+id+"/cancel", "")
		return code, body
	}
	insert("gr_pending0000", "pending", "gate")
	if code, _ := cancel("gr_pending0000"); code != 200 || lastBody != `{"run":{"id":"gr_pending0000","state":"failed"}}` {
		t.Fatalf("a pending run: %d %s", code, lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT a.detail->>'started_by' FROM runs r JOIN audit_events a ON a.run_id = r.id AND a.action = $1
		WHERE r.id = 'gr_pending0000' AND r.state = 'failed' AND r.failure_reason = 'interrupted' AND r.ended_at IS NOT NULL`, cancelRoute).Scan(&detail); err != nil ||
		detail != `{"kind": "agent", "display": "Claude Code", "subject": "claude-code:dana@meridian.example"}` {
		t.Fatalf("ended interrupted, the audit row naming who started it: %q (%v)", detail, err)
	}
	if code, body := cancel("gr_pending0000"); code != 409 || body["error"] != "ended" || count(t, ctx, pool, cancelAudited) != 1 {
		t.Fatalf("an ended run: %d %s", code, lastBody)
	}
	insert("gr_running0000", "running", "gate")
	for range 2 { // asked once: the second answers the same and writes nothing
		if code, _ := cancel("gr_running0000"); code != 202 || lastBody != `{"run":{"id":"gr_running0000","state":"running"}}` {
			t.Fatalf("a running run: %d %s", code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = 'gr_running0000' AND state = 'running' AND cancel_requested_at IS NOT NULL"); n != 1 || count(t, ctx, pool, cancelAudited) != 2 {
		t.Fatalf("the running run is asked to end itself, once: %d", n)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', ended_at = now() WHERE id = 'gr_running0000'"); err != nil {
		t.Fatal(err)
	}
	insert("gr_deploying00", "running", "promote")
	if _, err := pool.Exec(ctx, "UPDATE runs SET deploy_started_at = now() WHERE id = 'gr_deploying00'"); err != nil {
		t.Fatal(err)
	}
	if code, body := cancel("gr_deploying00"); code != 409 || body["error"] != "too_late" || body["message"] != "Too late to cancel: production is already switching to this version." ||
		count(t, ctx, pool, "SELECT count(*) FROM runs WHERE cancel_requested_at IS NOT NULL") != 1 || count(t, ctx, pool, cancelAudited) != 2 {
		t.Fatalf("a deploy that began: %d %s", code, lastBody)
	}
	if code, body := cancel("gr_nothing0000"); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown run: %d %s", code, lastBody)
	}
}

// Who may promote may cancel a promotion: while prod.human-approval blocks, an agent is refused and
// nothing is asked of the run; a gate run it may cancel.
func TestAnAgentCannotCancelAPromotionWhileHumanApprovalBlocks(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) SELECT 'gr_promote0000', project_id, id, 'running', 'promote' FROM packages LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/gr_promote0000/cancel", ""); code != 403 || body["error"] != "person_required" ||
		count(t, ctx, pool, "SELECT count(*) FROM runs WHERE cancel_requested_at IS NOT NULL") != 0 || count(t, ctx, pool, cancelAudited) != 0 {
		t.Fatalf("an agent cancelling a promotion: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET kind = 'gate' WHERE id = 'gr_promote0000'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/gr_promote0000/cancel", ""); code != 202 {
		t.Fatalf("an agent cancelling a gate run: %d %s", code, lastBody)
	}
}
