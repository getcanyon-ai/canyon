//go:build !noee

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rebuildIdentity is the builder a rebuild patch here rebuilt on: its identity is the builder's digest.
var rebuildIdentity = "sha256:" + strings.Repeat("e1", 32)

// rebuiltPatch makes run a rebuild patch of the image claims-portal's preview runs now, as passedPatch makes a rebase
// whose watch passed, under rebuildIdentity: kind rebuild, the run's build recording node-engine first, and its patch
// preview row naming a runtime fix (contracts/routes.md, A rebuild patch). The org's rebuilds reads ask.
func rebuiltPatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deploy, run string) string {
	t.Helper()
	for _, q := range []string{
		`UPDATE org_preferences SET rebuilds = 'ask'`,
		`INSERT INTO patch_identities (id, kind, run_image, builder) VALUES ('` + rebuildIdentity + `', 'rebuild', 'canyon-run:noble', '` + rebuildIdentity + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v: %s", err, q)
		}
	}
	rebuilt := passedPatch(t, ctx, pool, deploy, run, "d4")
	for _, q := range []string{
		`UPDATE patch_wave_apps SET identity = '` + rebuildIdentity + `' WHERE run = '` + run + `'`,
		`UPDATE runs SET build_image = (SELECT image FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'),
			buildpacks = '[{"id": "paketo-buildpacks/node-engine"}, {"id": "paketo-buildpacks/npm-install"}]' WHERE id = '` + run + `'`,
		`UPDATE runs SET rebuilt_with = 'a newer Node.js' WHERE id = '` + run + `'`,
		`UPDATE audit_events SET detail = '{"proof": "same_inputs", "same_inputs": {"values_key": 1, "buildpacks": ["paketo-buildpacks/node-engine", "paketo-buildpacks/npm-install"]},
			"fixed": [{"name": "cross-spawn", "from": "7.0.3", "to": "7.0.5", "vulnerability": "CVE-2024-21538", "class": "runtime"}]}' WHERE run_id = '` + run + `' AND action = 'patch preview'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v: %s", err, q)
		}
	}
	return rebuilt
}

// With sign-in off nobody can decide a rebuild's approval: rebuilds reads off, and a PUT of ask is refused.
func TestWithSignInOffRebuildsIsOff(t *testing.T) {
	_, _, base, c := withProject(t)
	if got := getPreferences(t, c, base)["rebuilds"]; got != "off" {
		t.Fatalf("off: %v", got)
	}
	if code, _ := putPreferences(t, c, base, `{"rebuilds":"ask"}`); code != 409 || !strings.HasPrefix(lastBody, `{"error":"signin_required","message":`) {
		t.Fatalf("ask with sign-in off: %d %s", code, lastBody)
	}
}

// A rebuild's production is always asked of the app's team (contracts/routes.md, A rebuild patch): its approval reads
// rebuild with the rebuild's own proof, naming Node.js by the buildpacks that ran; a read-only member is refused it and
// a contributor approves it in the browser; the admin's fleet approval of its identity is refused 409 always_asked,
// writing nothing; and with rebuilds off nothing is asked for it.
func TestARebuildIsAlwaysAskedOfTheTeam(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	rebuilt := rebuiltPatch(t, ctx, pool, deploy, "gr_rebuilda")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'off'"); err != nil {
		t.Fatal(err)
	}
	if n := filePatches(t, ctx); n != 0 {
		t.Fatalf("rebuilds off, the tick filed %d", n)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'ask'"); err != nil {
		t.Fatal(err)
	}
	at := base + approvalsAt + patchCode(t, ctx, pool)
	_, read, _ := do(t, browser, http.MethodGet, at, "")
	fixes, _ := read["fixes"].([]any)
	if read["rebuild"] != true || read["identity"] != rebuildIdentity || read["image"] != rebuilt[strings.Index(rebuilt, "@")+1:] ||
		read["proof"] != "Rebuilt from the same code and the same dependencies with a newer Node.js. A rebuild is a new program, not a copy of what production runs, so a person on your team decides." ||
		len(fixes) != 1 || string(mustJSON(fixes[0])) != `{"class":"runtime","from":"7.0.3","name":"cross-spawn","to":"7.0.5","vulnerability":"CVE-2024-21538"}` {
		t.Fatalf("the rebuild's approval: %s", lastBody)
	}
	patchShape(t, "approval_read", read)
	// the image read's patched[] names each fix's class as the run recorded it: a runtime fix is no system library (#2631)
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET detail = detail || jsonb_build_object('image', $1::text) WHERE run_id = 'gr_rebuilda' AND action = 'patch preview'`,
		rebuilt[strings.Index(rebuilt, "@")+1:]); err != nil {
		t.Fatal(err)
	}
	if _, img, _ := do(t, browser, http.MethodGet, base+"/api/v1/projects/claims-portal/envs/preview/image", ""); len(img["patched"].([]any)) != 1 ||
		img["patched"].([]any)[0].(map[string]any)["class"] != "runtime" {
		t.Fatalf("preview's image read after the rebuild: %s", lastBody)
	}
	if got, _, _ := do(t, browser, http.MethodPost, base+identitiesAt+"/"+rebuildIdentity+"/approve", approveReason); got != 409 ||
		lastBody != `{"error":"always_asked","message":"Canyon asks each app's team before a rebuild goes to production"}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'approve patch identity'") != 0 {
		t.Fatalf("an admin approves a rebuild for every app: %d %s", got, lastBody)
	}
	_, idents, _ := do(t, browser, http.MethodGet, base+identitiesAt, "")
	patchShape(t, "identities_read", idents)
	// the org's rebuilds turned off: the open approval reads outdated, and approving it ships nothing (#2619's review)
	contributor := teamMember(t, ctx, pool, base, "sub-erin", "erin@meridian.example", "contributor")
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'off'"); err != nil {
		t.Fatal(err)
	}
	ran := fake.Runs.Load()
	if _, read, _ := do(t, contributor, http.MethodGet, at, ""); read["state"] != "outdated" {
		t.Fatalf("rebuilds off, the approval reads: %s", lastBody)
	}
	if got, _, _ := do(t, contributor, http.MethodPost, at+"/approve", ""); got != 409 || lastBody != patchOutdatedBody || fake.Runs.Load() != ran {
		t.Fatalf("a contributor approves it with rebuilds off: %d %s, %d /runs", got, lastBody, fake.Runs.Load()-ran)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET rebuilds = 'ask'"); err != nil {
		t.Fatal(err)
	}
	viewer := teamMember(t, ctx, pool, base, "sub-vic", "vic@meridian.example", "viewer")
	if got, refused, _ := do(t, viewer, http.MethodPost, at+"/approve", ""); got != 403 || refused["error"] != "team_required" {
		t.Fatalf("a read-only member approves it: %d %s", got, lastBody)
	}
	got, started, _ := do(t, contributor, http.MethodPost, at+"/approve", "")
	run, _ := runOf(started)["id"].(string)
	if got != 202 || waitVerdict(t, contributor, base, run)["state"] != "clean" || fake.Ran().Target != "prod" || fake.Ran().Image != rebuilt {
		t.Fatalf("a contributor approves the rebuild: %d %s, ran %+v", got, lastBody, fake.Ran())
	}
}
