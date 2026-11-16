package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// wizReport is the tool wiz's SARIF report of an image: each result its rule id, level and the package it names
// under properties.canyon.package, nil for none.
func wizReport(results ...obj) string {
	out := make([]any, 0, len(results))
	for _, r := range results {
		out = append(out, r)
	}
	return string(mustJSON(obj{"version": "2.1.0", "runs": []any{obj{"tool": obj{"driver": obj{"name": "Wiz", "version": "2026.11"}}, "results": out}}}))
}

func wizFinding(rule string, pkg obj) obj {
	r := obj{"ruleId": rule, "level": "error", "message": obj{"text": rule}}
	if pkg != nil {
		r["properties"] = obj{"canyon": obj{"package": pkg}}
	}
	return r
}

// imageHostPool is claims-portal with the golden image's scan stored and preview running it, and its database for the
// sessions a test makes of its own: dana is the admin, olive the owner, carl a contributor, vic read-only and nora on
// no team.
func imageHostPool(t *testing.T) (base, digest string, dana, olive, carl, vic, nora *http.Client, g platformGolden, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	withOIDC(t)
	pool, ctx = migrated(t)
	base, _ = start(t, pool, time.Hour)
	olive = signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	dana = signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	carl = signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic = signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	nora = signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	g = loadPlatformGolden(t)
	seedPlatform(t, ctx, pool, g, "claims-portal", g.Digest)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT 'claims-portal', 'preview', package_id, 'canyon/claims-portal@' || $1, 'http://claims-portal.preview.localhost:7777', id, 'canyon' FROM runs WHERE id = 'gr_seed'`, g.Digest); err != nil {
		t.Fatal(err)
	}
	return base, g.Digest, dana, olive, carl, vic, nora, g, pool, ctx
}

// agentOf is a CLI session of sub's under email, as an agent the person runs: its actor an agent's.
func agentOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, sub, email string) *http.Client {
	t.Helper()
	c := cliIn(t, ctx, pool, base, sub, email)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = $1 AND flow = 'device'", sub); err != nil {
		t.Fatal(err)
	}
	return c
}

const libssl3Purl = "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?arch=amd64&distro=ubuntu-24.04"

// Attaching an image report is a contributor's, as a package's is (design/PERMISSIONS.md, Platform patches): a
// read-only member and one with no access are refused team_required, nothing stored; a contributor attaches wiz's
// report once an admin added wiz, to an image the app runs, one audit row naming it; an image it neither runs nor keeps
// is 404, a tool nobody added 400 unknown_tool, and a result naming no package refuses the report whole.
func TestImageReportAttachMemberRefused(t *testing.T) {
	base, digest, dana, _, carl, vic, nora, _, pool, ctx := imageHostPool(t)
	at := base + "/api/v1/projects/claims-portal/images/" + digest + "/reports"
	body := wizReport(wizFinding("CVE-2024-5535", obj{"name": "libssl3", "version": "3.0.13-0ubuntu3.4", "fixed": "3.0.13-0ubuntu3.5",
		"layer": "sha256:" + strings.Repeat("2", 64), "purl": libssl3Purl}))
	for who, c := range map[string]*http.Client{"a read-only member": vic, "no access": nora} {
		if code, _, _ := do(t, c, http.MethodPut, at, body); code != 403 || lastBody != teamRequiredTo(c == vic) {
			t.Fatalf("%s attaches: %d %s", who, code, lastBody)
		}
	}
	if code, refused, _ := do(t, carl, http.MethodPut, at, body); code != 400 || refused["error"] != "unknown_tool" {
		t.Fatalf("a tool nobody added: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/sarif.wiz?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the admin adds wiz: %d %s", code, lastBody)
	}
	if code, refused, _ := do(t, carl, http.MethodPut, at, wizReport(wizFinding("CVE-2024-5535", nil))); code != 400 || refused["error"] != "invalid_report" ||
		!strings.HasPrefix(refused["message"].(string), "result 0: properties.canyon.package is not there") {
		t.Fatalf("a result naming no package: %d %s", code, lastBody)
	}
	other := base + "/api/v1/projects/claims-portal/images/sha256:" + strings.Repeat("9", 64) + "/reports"
	if code, _, _ := do(t, carl, http.MethodPut, other, body); code != 404 || lastBody != `{"error":"not_found","message":"This app doesn't run or keep that image."}` {
		t.Fatalf("an image the app neither runs nor keeps: %d %s", code, lastBody)
	}
	code, out, _ := do(t, carl, http.MethodPut, at, body)
	if code != 200 || out["tool"] != "wiz" || out["results"] != float64(1) || out["dropped"] != float64(0) {
		t.Fatalf("a contributor attaches: %d %s", code, lastBody)
	}
	// a contributor's CLI, and an agent under a contributor's address, attach as a CI job does a package's report
	for who, c := range map[string]*http.Client{"a contributor's CLI": cliIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example"),
		"an agent of a contributor's": agentOf(t, ctx, pool, base, "sub-cora", "carl@meridian.example")} {
		if code, _, _ := do(t, c, http.MethodPut, at, body); code != 200 {
			t.Fatalf("%s attaches: %d %s", who, code, lastBody)
		}
	}
	if code, _, _ := do(t, agentOf(t, ctx, pool, base, "sub-vera", "vic@meridian.example"), http.MethodPut, at, body); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Fatalf("an agent of a read-only member's attaches: %d %s", code, lastBody)
	}
	if _, rows, _ := do(t, dana, http.MethodGet, base+"/api/v1/audit?project=claims-portal", ""); !strings.Contains(string(mustJSON(rows)), `"action":"attach image report"`) {
		t.Fatalf("no attach image report row: %s", lastBody)
	}
}

// platform.known-vuln's source is an admin's, a person's, from the dashboard while production is approved there
// (design/PERMISSIONS.md, Platform patches): an owner and a plain member are refused admin_required, an agent of the admin's
// person_required, the admin's CLI dashboard_required with prod_approval on; a tool nobody added is 400 invalid_source in
// the admin's words. Once named, the image read is Canyon's own matches and wiz's reports together — an empty report
// hides nothing, a fixed libssl3 is added, its unfixed one a note — and trivy alone again is stored as no source at all.
func TestTheRulesSourceIsAnAdmins(t *testing.T) {
	base, digest, dana, olive, carl, _, nora, _, pool, ctx := imageHostPool(t)
	at := base + "/api/v1/policies/platform.known-vuln"
	for who, c := range map[string]*http.Client{"an owner": olive, "a plain member": nora} {
		if code, refused, _ := do(t, c, http.MethodPut, at+"?apply=true", `{"settings":{"source":"wiz"}}`); code != 403 || refused["error"] != "admin_required" {
			t.Fatalf("%s names the source: %d %s", who, code, lastBody)
		}
	}
	if code, refused, _ := do(t, agentOf(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), http.MethodPut, at+"?apply=true", `{"settings":{"source":"wiz"}}`); code != 403 || refused["error"] != "person_required" {
		t.Fatalf("an agent of the admin's names the source: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/preferences", `{"prod_approval":true}`); code != 200 {
		t.Fatalf("prod_approval on: %d %s", code, lastBody)
	}
	if code, refused, _ := do(t, cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), http.MethodPut, at+"?apply=true", `{"settings":{"source":"wiz"}}`); code != 403 || refused["error"] != "dashboard_required" {
		t.Fatalf("the admin's CLI names the source with prod_approval on: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, at, `{"settings":{"source":"wiz"}}`); code != 400 ||
		lastBody != `{"error":"invalid_source","message":"Canyon doesn't know wiz yet: add it under Guardrails first, then pick it here."}` {
		t.Fatalf("a tool nobody added: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, at, `{"run":true}`); code != 409 {
		t.Fatalf("run on the rule: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/sarif.wiz?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the admin adds wiz: %d %s", code, lastBody)
	}
	read := base + "/api/v1/projects/claims-portal/envs/preview/image"
	if code, _, _ := do(t, dana, http.MethodPut, at+"?apply=true", `{"settings":{"source":"wiz"}}`); code != 200 {
		t.Fatalf("the admin names wiz: %d %s", code, lastBody)
	}
	if _, img, _ := do(t, carl, http.MethodGet, read, ""); img["state"] != "could_not_check" || img["why"] != "could not check: no wiz report is attached to this image yet" {
		t.Fatalf("no report yet: %s", lastBody)
	}
	osLayer := "sha256:" + strings.Repeat("2", 64)
	reports := base + "/api/v1/projects/claims-portal/images/" + digest + "/reports"
	if code, _, _ := do(t, carl, http.MethodPut, reports, wizReport()); code != 200 { // HIGH 2 of #2630's review: an empty report hides nothing
		t.Fatalf("carl attaches an empty report: %d %s", code, lastBody)
	}
	if _, img, _ := do(t, carl, http.MethodGet, read, ""); img["state"] != "checked" ||
		img["why"] != "3 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 4 notes: no fix published, or Canyon can't tell what put it there or which part of the image it's in" {
		t.Fatalf("an empty report's read: %s", lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodPut, reports, wizReport(
		wizFinding("CVE-2024-5535", obj{"name": "libssl3", "version": "3.0.13-0ubuntu3.4", "fixed": "3.0.13-0ubuntu3.5", "layer": osLayer, "purl": libssl3Purl}),
		wizFinding("CVE-2025-0001", obj{"name": "libssl3", "version": "3.0.13-0ubuntu3.4", "layer": osLayer, "purl": libssl3Purl}),
		wizFinding("CVE-2025-0002", obj{"name": "zlib1g", "version": "1.3", "fixed": "1.3.1"}))); code != 200 {
		t.Fatalf("carl attaches: %d %s", code, lastBody)
	}
	_, img, _ := do(t, carl, http.MethodGet, read, "")
	if fs, _ := img["findings"].([]any); img["state"] != "checked" || len(fs) != 5 ||
		img["why"] != "3 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 6 notes: no fix published, or Canyon can't tell what put it there or which part of the image it's in" {
		t.Fatalf("the matches and wiz's read: %s", lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, at+"?apply=true", `{"settings":{"source":"trivy"}}`); code != 200 {
		t.Fatalf("back to trivy: %d %s", code, lastBody)
	}
	if _, img, _ := do(t, carl, http.MethodGet, read, ""); strings.Contains(img["why"].(string), "which part of the image") {
		t.Fatalf("trivy's read again: %s", lastBody)
	}
}
