package main

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

// previewOf is envs.preview of the fixture's project read, nil while the environment is absent.
func previewOf(t *testing.T, c *http.Client, base string) map[string]any {
	t.Helper()
	code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	if code != 200 {
		t.Fatalf("the project read: %d %s", code, lastBody)
	}
	project, _ := body["project"].(map[string]any)
	envs, _ := project["envs"].(map[string]any)
	preview, _ := envs["preview"].(map[string]any)
	return preview
}

// envs.preview carries run_by — org once the platform reported it live — and pending while a
// release of the environment is open: through sent, accepted, undelivered and refused, with its
// event, and gone once the release is reported; the fleet row is the same row. With a release
// open and nothing deployed yet the environment is env_pending alone (contracts/routes.md,
// envs.<env>; release.schema.json).
func TestEnvsCarryRunByAndThePendingRelease(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	c := client(t)
	var firstSentAt time.Time
	if err := pool.QueryRow(ctx, "SELECT sent_at FROM releases WHERE id = 'rel_8f2a'").Scan(&firstSentAt); err != nil {
		t.Fatal(err)
	}
	alone := map[string]any{"run_by": "org", "pending": map[string]any{"release": "rel_8f2a", "event": "deploy", "sent_at": firstSentAt.UTC().Format(time.RFC3339), "state": "accepted", "via": "webhook"}}
	if preview := previewOf(t, c, base); !reflect.DeepEqual(preview, alone) {
		t.Fatalf("a release open and nothing deployed is env_pending alone: %v", preview)
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the report: %d %s", code, lastBody)
	}
	preview := previewOf(t, c, base)
	if _, open := preview["pending"]; preview["run_by"] != "org" || open {
		t.Fatalf("reported live: %v", preview)
	}
	var sentAt time.Time
	if err := pool.QueryRow(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_next', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '7f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4d', 'sent', now()) RETURNING sent_at", reportImage).Scan(&sentAt); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"release": "rel_next", "event": "deploy", "sent_at": sentAt.UTC().Format(time.RFC3339), "state": "sent", "via": "webhook"}
	for _, state := range []string{"sent", "accepted", "undelivered", "refused"} {
		if _, err := pool.Exec(ctx, "UPDATE releases SET state = $1 WHERE id = 'rel_next'", state); err != nil {
			t.Fatal(err)
		}
		want["state"] = state
		if got := previewOf(t, c, base)["pending"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: pending %v, want %v", state, got, want)
		}
	}
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	apps, _ := fleet["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("the fleet: %s", lastBody)
	}
	row, _ := apps[0].(map[string]any)["envs"].(map[string]any)["preview"].(map[string]any)
	if row["run_by"] != "org" || !reflect.DeepEqual(row["pending"], want) {
		t.Fatalf("the fleet row: %v", row)
	}
	// prod runs the pd/pdr branch, a separate expression from preview's pv/pvr one — assert it too.
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'prod', 'pkg_7d3e8f', $1, 'https://claims.apps.meridian.example', 'gr_8f2a', 'org')", reportImage); err != nil {
		t.Fatal(err)
	}
	var prodSentAt time.Time
	if err := pool.QueryRow(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_prod', 'claims-portal', 'prod', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '8f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4e', 'sent', now()) RETURNING sent_at", reportImage).Scan(&prodSentAt); err != nil {
		t.Fatal(err)
	}
	_, projectBody, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	prod, _ := projectBody["project"].(map[string]any)["envs"].(map[string]any)["prod"].(map[string]any)
	wantProd := map[string]any{"release": "rel_prod", "event": "deploy", "sent_at": prodSentAt.UTC().Format(time.RFC3339), "state": "sent", "via": "webhook"}
	if prod["run_by"] != "org" || !reflect.DeepEqual(prod["pending"], wantProd) {
		t.Fatalf("envs.prod: %v", prod)
	}
	for _, state := range []string{"deployed", "failed", "removed"} {
		if _, err := pool.Exec(ctx, "UPDATE releases SET state = $1 WHERE id = 'rel_next'", state); err != nil {
			t.Fatal(err)
		}
		preview := previewOf(t, c, base)
		if _, open := preview["pending"]; open || preview["run_by"] != "org" {
			t.Fatalf("%s: %v", state, preview)
		}
	}
}

// A refused release stays pending only until something newer is actually live: once the admin
// switches the environment back to canyon and Canyon deploys over it, the deploy is newer than the
// refused release, and pending must be gone — never a stale one left over from before the switch
// (fleet.go's preview_pending/prod_pending, "AND (deployed_at IS NULL OR pvr.created_at >
// deployed_at)").
func TestPendingClearsWhenCanyonRedeploysOverARefusedRelease(t *testing.T) {
	pool, ctx, base, path, token := reportFixture(t)
	c := client(t)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, c, http.MethodPost, path, deployed, reportHeaders(token)); code != 200 {
		t.Fatalf("the org's report: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) VALUES ('rel_refused', 'claims-portal', 'preview', 'gr_8f2a', 'pkg_7d3e8f', 'deploy', $1, '9f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4f', 'refused', now())", reportImage); err != nil {
		t.Fatal(err)
	}
	preview := previewOf(t, c, base)
	if _, open := preview["pending"]; preview["run_by"] != "org" || !open {
		t.Fatalf("the refused release is pending until something newer is live: %v", preview)
	}
	// the switch to canyon: Canyon deploys over it, strictly after the refused release.
	if _, err := pool.Exec(ctx, "UPDATE deploys SET run_by = 'canyon', deployed_at = now() WHERE project_id = 'claims-portal' AND env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	preview = previewOf(t, c, base)
	if _, open := preview["pending"]; preview["run_by"] != "canyon" || open {
		t.Fatalf("a Canyon deploy after the refused release leaves no pending: %v", preview)
	}
}

// An environment waiting on its first release runs nothing: the fleet row shows it as env_pending
// alone and keeps the stage its runs give it — never preview — and the status receipt's
// prod.clean-gate reads nothing running in preview, as it does with no environment at all.
func TestAnEnvironmentWaitingOnItsFirstReleaseRunsNothing(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	c := client(t)
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	apps, _ := fleet["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("the fleet: %s", lastBody)
	}
	app, _ := apps[0].(map[string]any)
	preview, _ := app["envs"].(map[string]any)["preview"].(map[string]any)
	latest, _ := app["latest"].(map[string]any)
	if _, deployed := preview["url"]; preview["run_by"] != "org" || preview["pending"] == nil || deployed || latest["stage"] == "preview" {
		t.Fatalf("the fleet row: envs.preview %v, stage %v", preview, latest["stage"])
	}
	for running, want := range map[string]int{"preview": 0, "production": 0, "nothing": 1} { // routes.md: running= counts an environment with a url
		if _, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?running="+running, ""); len(page["apps"].([]any)) != want {
			t.Fatalf("running=%s: %s", running, lastBody)
		}
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/promotions", `{"gate_run":"gr_8f2a"}`); code != 409 || body["error"] != "nothing_in_preview" {
		t.Fatalf("promote with preview waiting on its first release: %d %s", code, lastBody)
	}
	code, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if code != 200 || status["promote_run"] == nil || status["promote_run"] != status["last_run"] { // nothing runs in preview, so the receipt's run is the newest
		t.Fatalf("the status read: %d %s", code, lastBody)
	}
	if r := status["receipt"].(map[string]any); r["waiting"] != "Deploy a new version to preview to promote it." || len(r["rules"].([]any)) != 0 {
		t.Fatalf("the receipt with a release open and nothing deployed waits: %v", r) // nothing runs in preview yet
	}
}

// pending laid over an environment Canyon runs — a switch to org after its deploy, or a row written
// before the column — answers run_by org: a release goes out under org alone (release.schema.json).
func TestAPendingOverACanyonRunEnvironmentAnswersRunByOrg(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	c := client(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_at) VALUES ('claims-portal', 'preview', 'pkg_7d3e8f', $1, 'http://claims-portal.preview.localhost:7777', 'gr_8f2a', now() - interval '1 hour')", reportImage); err != nil {
		t.Fatal(err)
	}
	if preview := previewOf(t, c, base); preview["run_by"] != "org" || preview["pending"] == nil || preview["url"] == nil {
		t.Fatalf("a release open over a Canyon-run preview: %v", preview)
	}
}
