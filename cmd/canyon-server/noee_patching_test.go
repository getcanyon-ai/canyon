//go:build noee

package main

import (
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A server built without Canyon Enterprise patches nothing whatever patching stores (contracts/routes.md, Licence, A
// server built without Canyon Enterprise, Platform patching): what is stored reads as it is, turning patching or
// rebuilds on is 409 licence_required after the refusals before it, turning it off is free, a patch approval filed
// before reads outdated, a fleet approval and a resume are 409 licence_required with nothing written, and a server
// set to patch says so at start and in the admin's fleet read.
func TestWithoutEnterpriseNothingIsPatched(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	if got := getPreferences(t, browser, base)["patching"]; got != "ask" {
		t.Fatalf("patching as stored: %v, want ask", got)
	}
	if do(t, browser, http.MethodGet, base+"/api/v1/fleet", ""); !strings.Contains(lastBody, `"kind":"patching_unlicensed"`) {
		t.Errorf("the admin's fleet while patching reads ask: %s, want patching_unlicensed among org_needs", lastBody)
	}
	var logs strings.Builder
	if err := appOf(t, base).patchingAtStart(ctx, true, log.New(&logs, "", 0)); err != nil ||
		logs.String() != "canyon-server: CANYON_PATCHING is on, but this Canyon is built without Canyon Enterprise, so it patches nothing\n" {
		t.Errorf("the start's line: %q (%v)", logs.String(), err)
	}

	// a patch approval filed before reads outdated, and nothing decides it
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if _, err := pool.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via,
		expires_at, kind, patch_identity, replaces_image, fixes) SELECT 'NXEEPTCH', project_id, gate_run, package_id, image, 'canyon', 'patch', 'Canyon', 'canyon', $1, 'patch', $2,
		(SELECT image FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'), '[]' FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'`,
		time.Now().Add(time.Hour), patchIdentity); err != nil {
		t.Fatal(err)
	}
	if _, read, _ := do(t, browser, http.MethodGet, base+"/api/v1/promotion-approvals/NXEEPTCH", ""); read["state"] != "outdated" {
		t.Errorf("a patch approval filed before: %s, want outdated", lastBody)
	}

	// the fleet approval and a resume: 409 licence_required, nothing written
	identity := base + "/api/v1/patching/identities/" + patchIdentity
	if code, _, _ := do(t, browser, http.MethodPost, identity+"/approve", `{"reason":"the fleet's libssl3"}`); code != 409 || lastBody != patchingRequired ||
		count(t, ctx, pool, "SELECT count(*) FROM patch_identities WHERE approved_at IS NOT NULL") != 0 {
		t.Errorf("a fleet approval: %d %s, want 409 %s and none approved", code, lastBody, patchingRequired)
	}
	if code, _, _ := do(t, browser, http.MethodPost, identity+"/resume", ""); code != 409 || lastBody != `{"error":"not_paused","message":"this platform identity is neither paused nor stalled"}` {
		t.Errorf("resuming one not paused: %d %s, want 409 not_paused first", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE patch_identities SET paused_at = now(), paused_projects = '{claims-portal}'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, browser, http.MethodPost, identity+"/resume", ""); code != 409 || lastBody != patchingRequired ||
		count(t, ctx, pool, "SELECT count(*) FROM patch_identities WHERE paused_at IS NULL") != 0 {
		t.Errorf("a resume: %d %s, want 409 %s and still paused", code, lastBody, patchingRequired)
	}

	// turning rebuilds on is refused, patching off is free, and then the refusals keep their order
	if code, _ := putPreferences(t, browser, base, `{"rebuilds":"ask"}`); code != 409 || lastBody != patchingRequired {
		t.Errorf("rebuilds on: %d %s, want 409 %s", code, lastBody, patchingRequired)
	}
	if code, out := putPreferences(t, browser, base, `{"patching":"off"}`); code != 200 || out["patching"] != "off" {
		t.Errorf("patching off: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, browser, base, `{"rebuilds":"ask"}`); code != 409 ||
		lastBody != `{"error":"patching_off","message":"Turn on patching first: Canyon rebuilds apps only while it patches them."}` {
		t.Errorf("rebuilds on while patching is off: %d %s, want patching_off first", code, lastBody)
	}
	if code, _ := putPreferences(t, browser, base, `{"patching":"ask","rebuilds":"ask"}`); code != 409 || lastBody != patchingRequired ||
		count(t, ctx, pool, "SELECT count(*) FROM org_preferences WHERE patching = 'off' AND rebuilds = 'off'") != 1 {
		t.Errorf("both on in one PUT: %d %s, want 409 %s and both still off", code, lastBody, patchingRequired)
	}
}
