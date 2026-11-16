//go:build !noee

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
)

// THE FAIL-CLOSED RULE (#2879), platform patching: patching turned on under a licence still asks once it has lapsed,
// and leaving it as it is or turning it off stays free (#2881's review), while turning it back on is 409
// licence_required.
func TestALapsedLicenceKeepsPatching(t *testing.T) {
	h := outboundHost(t)
	if prefs := getPreferences(t, h.dana, h.base); prefs["patching"] != "ask" {
		t.Fatalf("patching under the licence: %v", prefs["patching"])
	}

	licenceAt(t, h.base, testExpiry.Add(licence.Grace+time.Hour)) // lapsed: an hour past grace

	if prefs := getPreferences(t, h.dana, h.base); prefs["patching"] != "ask" {
		t.Errorf("lapsed: patching %v, want as set", prefs["patching"])
	}
	for _, c := range []struct{ what, body string }{{"patching as it stands", `{"patching":"ask"}`}, {"patching off", `{"patching":"off"}`}} {
		if code, _ := putPreferences(t, h.dana, h.base, c.body); code != 200 {
			t.Errorf("lapsed, %s: %d %s, want 200", c.what, code, lastBody)
		}
	}
	if code, _ := putPreferences(t, h.dana, h.base, `{"patching":"ask"}`); code != 409 || lastBody != patchingRequired {
		t.Errorf("lapsed, patching back on: %d %s, want 409 %s", code, lastBody, patchingRequired)
	}
}

// Moved from routes_rebuild_patches_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// The org's rebuilds is an admin's, changed from a browser session as patching is (design/PERMISSIONS.md, Platform
// patches): off by default; an owner is refused 403 admin_required, the admin's CLI 403 dashboard_required with one
// denied row before its body is read; the admin's browser turns it on, audited before and after; and it reads off
// while patching is off (contracts/routes.md, the preferences row).
func TestRebuildsAdminBrowserOnly(t *testing.T) {
	pool, ctx, base, browser, cli, _ := patchHost(t)
	if got := getPreferences(t, browser, base)["rebuilds"]; got != "off" {
		t.Fatalf("off by default: %v", got)
	}
	owner := teamMember(t, ctx, pool, base, "sub-olga", "olga@meridian.example", "owner")
	if code, _ := putPreferences(t, owner, base, `{"rebuilds":"ask"}`); code != 403 || lastBody != adminRequired {
		t.Fatalf("an owner: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, cli, base, `{"rebuilds":"auto"}`); code != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'") != 1 {
		t.Fatalf("the admin's CLI: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, browser, base, `{"rebuilds":"auto"}`); code != 400 {
		t.Fatalf("off or ask: %d %s", code, lastBody)
	}
	if code, out := putPreferences(t, browser, base, `{"rebuilds":"ask"}`); code != 200 || out["rebuilds"] != "ask" ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail = '{"rebuilds":{"before":"off","after":"ask"}}'::jsonb`) != 1 {
		t.Fatalf("the admin's browser turns it on: %d %s", code, lastBody)
	}
	// patching off turns rebuilds off with it, in one write and one audit row; rebuilds on while patching is off is
	// refused, telling the admin to turn patching on first (#2619's review)
	if code, out := putPreferences(t, browser, base, `{"patching":"off"}`); code != 200 || out["rebuilds"] != "off" ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail = '{"patching":{"before":"ask","after":"off"},"rebuilds":{"before":"ask","after":"off"}}'::jsonb`) != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM org_preferences WHERE rebuilds = 'off'") != 1 {
		t.Fatalf("patching off turns rebuilds off: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, browser, base, `{"rebuilds":"ask"}`); code != 409 ||
		lastBody != `{"error":"patching_off","message":"Turn on patching first: Canyon rebuilds apps only while it patches them."}` {
		t.Fatalf("rebuilds on while patching is off: %d %s", code, lastBody)
	}
	if code, out := putPreferences(t, browser, base, `{"patching":"ask","rebuilds":"ask"}`); code != 200 || out["rebuilds"] != "ask" {
		t.Fatalf("both on in one PUT: %d %s", code, lastBody)
	}
}

// Moved from routes_patch_approvals_test.go (#2913): it changes an Enterprise feature, so it runs with Canyon Enterprise.
// patching is the org's switch, off or ask, answered as saved: changed by an admin from a browser session alone —
// a member is refused, the admin's CLI too with one denied row — on the trail with its before and after; off, no
// patch approval is filed (contracts/routes.md, the preferences row; design/PATCHING.md §9).
func TestThePatchingSwitch(t *testing.T) {
	pool, ctx, base, browser, cli, deploy := approvalHost(t, false)
	if promoted(t, browser, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	pipe.Patches.Store(true)
	t.Cleanup(func() { pipe.Patches.Store(false) })
	if code, _, _ := do(t, browser, http.MethodPut, base+"/api/v1/policies/platform.known-vuln?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("platform.known-vuln on: %d %s", code, lastBody)
	}
	if got := getPreferences(t, browser, base)["patching"]; got != "ask" {
		t.Fatalf("ask by default: %v", got)
	}
	member := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	if code, _ := putPreferences(t, member, base, `{"patching":"off"}`); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, cli, base, `{"patching":"off"}`); code != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'") != 1 {
		t.Fatalf("the admin's CLI: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, browser, base, `{"patching":"auto"}`); code != 400 {
		t.Fatalf("off or ask: %d %s", code, lastBody)
	}
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	if code, out := putPreferences(t, browser, base, `{"patching":"off"}`); code != 200 || out["patching"] != "off" ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail = '{"patching":{"before":"ask","after":"off"}}'::jsonb`) != 1 {
		t.Fatalf("the admin's browser turns it off: %d %s", code, lastBody)
	}
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("off, the tick filed %d", n)
	}
	if code, out := putPreferences(t, browser, base, `{"patching":"ask"}`); code != 200 || out["patching"] != "ask" || filePatches(t, ctx) != 1 {
		t.Fatalf("ask again, and the passed patch is asked: %d %s", code, lastBody)
	}
}
