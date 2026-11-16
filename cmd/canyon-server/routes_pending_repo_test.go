package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// pending.pull_request follows pending (design/PERMISSIONS.md, The deploy repo): a contributor, an owner and an
// admin read the pull request a production release waits on, and how the release reaches the platform; a viewer
// and a member with no access never do, and never learn the deploy repo's name from any read.
func TestPullRequestFollowsPendingAtEveryLevel(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	const pr = "https://github.com/meridian/deploy/pull/42"
	runSQL(t, ctx, pool,
		"UPDATE deploys SET run_by = 'org', url = 'https://claims.apps.meridian.example' WHERE project_id = 'claims-portal' AND env = 'prod'",
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, via, pull_request, pull_number)
			SELECT 'rel_pr42', 'claims-portal', 'prod', 'gr_seed', id, 'deploy', 'canyon/claims-portal@x', gen_random_uuid(), 'accepted', now(), now(), 'repo', '`+pr+`', 42 FROM packages WHERE project_id = 'claims-portal'`)
	reads := func(c *http.Client) (map[string]any, string) {
		out, raw := map[string]any{}, ""
		_, one, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		out["project"], raw = one["project"], raw+lastBody
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		out["fleet"], raw = fleetRowOf(page, "claims-portal"), raw+lastBody
		if code, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code == 200 {
			out["status"] = status
		}
		return out, raw + lastBody
	}
	want := map[string]any{"url": pr, "number": float64(42)}
	for who, c := range map[string]*http.Client{
		"the owner":     olive,
		"a contributor": signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false),
		"an admin":      signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false),
	} {
		got, _ := reads(c)
		for read, row := range got {
			pending, _ := envIn(row, "prod")["pending"].(map[string]any)
			if pending["via"] != "repo" || !reflect.DeepEqual(pending["pull_request"], want) {
				t.Errorf("%s's %s: pending %v", who, read, pending)
			}
		}
	}
	for who, c := range map[string]*http.Client{
		"a viewer":                signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false),
		"a member with no access": signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false),
	} {
		got, raw := reads(c)
		for read, row := range got {
			if _, ok := envIn(row, "prod")["pending"]; ok {
				t.Errorf("%s's %s carries pending", who, read)
			}
		}
		if strings.Contains(raw, "meridian/deploy") {
			t.Errorf("%s read the deploy repo's name", who)
		}
	}
}
