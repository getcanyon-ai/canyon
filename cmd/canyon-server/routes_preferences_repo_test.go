package main

import (
	"testing"
	"time"
)

// delivery is how releases reach the platform (design/HAND-OFF.md §1, §3b): webhook by default, on every read;
// an admin's to change, audited; under delivery: repo, run_by: org needs the deploy repo saved — repo_required in
// place of webhook_required — and a switch of delivery while the org runs apps needs the one it names saved too.
func TestDeliveryRepoNeedsTheDeployRepoSaved(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	onKubernetes(t)
	base, _ := start(t, pool, time.Hour)
	servesHandOff(t)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if got := getPreferences(t, carol, base)["delivery"]; got != "webhook" {
		t.Fatalf("a member reads delivery %v, want webhook", got)
	}
	if code, _ := putPreferences(t, carol, base, `{"delivery":"repo"}`); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's PUT: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"delivery":"git"}`); code != 400 || body["error"] != "invalid_body" {
		t.Fatalf("a delivery outside the two: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"delivery":"repo"}`); code != 200 || body["delivery"] != "repo" {
		t.Fatalf("delivery: repo under run_by canyon asks nothing: %d %s", code, lastBody)
	}
	withDoorRow(t, ctx, pool, 1)
	if code, body := putPreferences(t, dana, base, `{"run_by":"org"}`); code != 409 || body["error"] != "repo_required" || body["message"] != repoRequired {
		t.Fatalf("run_by: org under delivery: repo with no repo saved: %d %s", code, lastBody)
	}
	if got := getPreferences(t, dana, base)["run_by"]; got != "canyon" {
		t.Fatalf("the refusal wrote run_by %v", got)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO platform_repo (url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, token_hash, fetch_token_hash, set_by_kind, set_by_subject, set_by_display)
		VALUES ('https://github.com/meridian/deploy', 'x-access-token', 1, '', '', 'preview', 'main', 'pull_request', 'apps', 'github', 'https://api.github.com', 'canyon', sha256('host'::bytea), sha256('fetch'::bytea), 'person', 'dana', 'Dana')`); err != nil {
		t.Fatal(err)
	}
	if code, _ := putPreferences(t, dana, base, `{"run_by":"org"}`); code != 200 {
		t.Fatalf("run_by: org with the repo saved and no webhook: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"delivery":"webhook"}`); code != 409 || body["error"] != "webhook_required" {
		t.Fatalf("back to the webhook while the org runs apps, with none saved: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail->'delivery' = '{"before": "webhook", "after": "repo"}'`); n != 1 {
		t.Fatalf("the delivery change's audit row: %d", n)
	}
}
