package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/runs"
)

// livePatch is a running patch run of claims-portal's preview, before its /run, whose workflow waits on a
// queue no server serves, so whether its request cancelled it reads off the workflow's status.
func livePatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "gr_" + hex.EncodeToString(b)
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display)
		SELECT $1, project_id, package_id, 'running', 'patch', 'canyon', 'patch', 'Canyon' FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := dbos.Enqueue[any](durable, "unserved-by-any-server", runs.Workflow, runs.Input{Run: id, Project: "claims-portal"}, dbos.WithEnqueueWorkflowID(id)); err != nil {
		t.Fatal(err)
	}
	return id
}

// patchEnded fails the test unless the patch run ended failed/interrupted and its workflow was cancelled.
func patchEnded(t *testing.T, ctx context.Context, pool *pgxpool.Pool, what, id string) {
	t.Helper()
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+id+"' AND state = 'failed' AND failure_reason = 'interrupted'"); n != 1 {
		t.Errorf("%s: the patch run did not end interrupted", what)
	}
	h, err := dbos.RetrieveWorkflow[any](durable, id)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		st, err := h.GetStatus()
		if err != nil {
			t.Fatal(err)
		}
		if st.Status == dbos.WorkflowStatusCancelled {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s: the patch run's workflow is %s, not cancelled", what, st.Status)
			return
		}
	}
}

// A person's delete goes first: a patch run of the app before its /run is ended failed/interrupted and its
// workflow cancelled — never a 409 run_in_progress — and the delete goes ahead (contracts/routes.md, Patch
// runs and the delete row).
func TestDeleteEndsARunningPatchRun(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	id := livePatch(t, ctx, pool)
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 {
		t.Fatalf("a delete over a running patch run: %d %s", code, lastBody)
	}
	patchEnded(t, ctx, pool, "the delete", id)
}

// A lift's start of the held image is a promotion: it ends a patch run of the app before its /run, cancels
// its workflow after the commit, and goes ahead (contracts/routes.md, Patch runs and The hold).
func TestALiftsStartEndsARunningPatchRun(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	id := livePatch(t, ctx, pool)
	if code, _, _ := do(t, c, http.MethodDelete, liftPath(t, ctx, pool, base, "start=true"), ""); code != 202 {
		t.Fatalf("start again over a running patch run: %d %s", code, lastBody)
	}
	patchEnded(t, ctx, pool, "the lift's start", id)
}

// An admin's Recreate networks ends a patch run of the app before its /run, cancels its workflow, and goes
// ahead (contracts/routes.md, Patch runs and Egress control).
func TestRecreateNetworksEndsARunningPatchRun(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	id := livePatch(t, ctx, pool)
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 {
		t.Fatalf("Recreate networks over a running patch run: %d %s", code, lastBody)
	}
	patchEnded(t, ctx, pool, "the recreation", id)
}

// watchingPatch is livePatch past its /run: its deploy.completed row written and its watch begun, as a patch run
// whose watch runs is.
func watchingPatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	id := livePatch(t, ctx, pool)
	if _, err := pool.Exec(ctx, `WITH r AS (UPDATE runs SET deploy_started_at = now() WHERE id = $1)
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES
		('run', $1, 'claims-portal', 'deploy.completed', 'deploy:preview', '{"env": "preview"}'),
		('run', $1, 'claims-portal', 'progress', NULL, '{"step": "watch", "status": "running"}')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A delete during a patch's watch ends the watch clean — line ended, its run clean, its workflow cancelled —
// and goes ahead (contracts/routes.md, Patch runs: after its /run, the request ends the watch clean).
func TestDeleteEndsAWatchClean(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	id := watchingPatch(t, ctx, pool)
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 {
		t.Fatalf("a delete during a watch: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+id+"' AND state = 'clean' AND ended_at IS NOT NULL"); n != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE run_id = '"+id+"' AND data->>'line' LIKE 'The watch ended early: % took preview offline'") != 1 {
		t.Fatal("the delete did not end the watch clean, saying so")
	}
	h, err := dbos.RetrieveWorkflow[any](durable, id)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if st, err := h.GetStatus(); err != nil || st.Status == dbos.WorkflowStatusCancelled {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the watch's workflow is %s, not cancelled", st.Status)
		}
	}
}

// A recreation of the app's network starts no run, so during a patch's watch it is refused as during the patch's
// /run — 409 run_in_progress in its own words, naming the run — and the watch goes on: its run still running, its
// workflow never cancelled (contracts/routes.md, Patch runs, amended 2026-10-29, #2100).
func TestRecreateNetworksDuringAWatchIsRefused(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	id := watchingPatch(t, ctx, pool)
	code, body, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`)
	if run, _ := body["run"].(map[string]any); code != 409 || body["error"] != "run_in_progress" || body["message"] != "a run of this project is running" || run["id"] != id {
		t.Fatalf("Recreate networks during a watch: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+id+"' AND state = 'running' AND ended_at IS NULL"); n != 1 {
		t.Fatal("the recreation ended the watch")
	}
	h, err := dbos.RetrieveWorkflow[any](durable, id)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := h.GetStatus(); err != nil || st.Status == dbos.WorkflowStatusCancelled {
		t.Fatalf("the watch's workflow after a refused recreation: %+v (%v)", st, err)
	}
	runs.Cancel(durable, id) // no workflow outlives its test
}

// A check during a patch's watch is refused 409 run_in_progress naming the watch and its watch_until, and the
// watch goes on; the 409 for any other running run names no watch (contracts/routes.md, the gate-runs row; #2568).
func TestACheckDuringAWatchNamesUntilWhen(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	id := watchingPatch(t, ctx, pool)
	t.Cleanup(func() { runs.Cancel(durable, id) })
	if _, err := pool.Exec(ctx, "UPDATE runs SET watch_until = '2026-10-29T14:35:12Z' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "{}")
	run, _ := body["run"].(map[string]any)
	watch, _ := body["watch"].(map[string]any)
	if code != 409 || body["error"] != "run_in_progress" || run["id"] != id || watch["until"] != "2026-10-29T14:35:12Z" {
		t.Fatalf("a check during a watch: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = '"+id+"' AND state = 'running' AND ended_at IS NULL"); n != 1 {
		t.Fatal("the check ended the watch")
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET reverting_at = now() WHERE id = $1", id); err != nil { // reverting: no watch runs
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "{}"); code != 409 || body["watch"] != nil {
		t.Fatalf("a check during a revert: %d %s", code, lastBody)
	}
}

// A patch run is the status read's promote_run, and its package's newest run, only while preview runs the image
// it deployed; it is never the project's last_run (contracts/routes.md, Patch runs). A person's promotion of it
// ships the patched image, its row holding no build digest or values HMAC, as a patch leaves it, writes patch
// prod and marks production's history patched; an undeploy of preview meanwhile leaves its review clearing it,
// as a gate run's does. Once preview runs another version, a promotion naming it is refused as one naming an
// older gate run is.
func TestAPatchRunIsPromoteRunWhilePreviewRunsItsImage(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	patched := "canyon/claims-portal@sha256:" + strings.Repeat("6a", 32)
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, kind, started_by_kind, started_by_subject, started_by_display, author_kind, author_subject, author_display)
		SELECT 'gr_patch', project_id, package_id, 'clean', now(), 'patch', 'canyon', 'patch', 'Canyon', 'person', 'anonymous', 'Dana' FROM runs WHERE id = $1),
		v AS (INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_patch', project_id, kind, dedupe, data FROM timeline_rows WHERE run_id = $1 AND kind IN ('verdict', 'deploy.completed'))
		UPDATE deploys SET image = $2, gate_run = 'gr_patch', build_digest = NULL, values_hmac = NULL, values_key = NULL WHERE project_id = 'claims-portal' AND env = 'preview'`, deploy, patched); err != nil {
		t.Fatal(err)
	}
	fake.Start("claims-portal", "preview")
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if status["promote_run"] != "gr_patch" || status["last_run"] != deploy {
		t.Fatalf("while preview runs the patch's image: promote_run %v, last_run %v", status["promote_run"], status["last_run"])
	}
	if run := promoted(t, c, base, "gr_patch"); run["state"] != "clean" || fake.Ran().Target != "prod" || fake.Ran().Image != patched {
		t.Fatalf("the promotion of the patch: %s, ran %+v", mustJSON(run), fake.Ran())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'patch prod' AND project_id = 'claims-portal'"); n != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM deploy_history WHERE gate_run = 'gr_patch' AND patched") != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'deploy prod'") != 0 {
		t.Fatalf("the promotion of a patch: %d patch prod rows, or its history not marked patched", n)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'"); err != nil { // an undeploy of preview
		t.Fatal(err)
	}
	if ok, err := pipe.Clears(ctx, "claims-portal", "gr_patch"); err != nil || !ok {
		t.Fatalf("the patch's review, preview undeployed: clears %v (%v)", ok, err)
	}
	cleanGate(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "b": "2"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	next, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, next)["state"] != "clean" {
		t.Fatalf("the next deploy: %s", lastBody)
	}
	if _, status, _ = do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); status["promote_run"] != next {
		t.Fatalf("once preview runs another image: promote_run %v, want %s", status["promote_run"], next)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"gr_patch"}`); code != 409 {
		t.Fatalf("a promotion naming the patch run preview no longer runs: %d %s", code, lastBody)
	}
}
