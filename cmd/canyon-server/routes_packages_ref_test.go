package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/vault"
)

// A contributor packages a ref of their app's linked repository (design/specs/mcp.md §2.3): the ref's commit, as a
// push of it would be, current unless it is a check's; a read-only member and someone with no grant are refused
// before GitHub is asked; a caller with no GitHub account linked, an admin too, and one whose account can't read the
// repository are refused, asked each call; an app with no link, a ref GitHub doesn't know and a ref that isn't one are refused, and
// an unknown app is 404 to everyone.
func TestPackagesFromRepository(t *testing.T) {
	asked := fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal'), ('quote-engine', 'quote-engine')",
		"INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('claims-portal', 555, '', 42, 'acme/claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('claims-portal', 'email', 'carol@meridian.example', 'contributor', 'sub-carol', 'Carol', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'vic@meridian.example', 'viewer', 'sub-vic', 'Vic', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'pat@meridian.example', 'contributor', 'sub-pat', 'Pat', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'quinn@meridian.example', 'contributor', 'sub-quinn', 'Quinn', 'person', 'sub-dana', 'Dana'),
			('quote-engine', 'email', 'carol@meridian.example', 'contributor', 'sub-carol', 'Carol', 'person', 'sub-dana', 'Dana')`,
		"INSERT INTO github_links (github_id, subject, login) VALUES (7002, 'sub-carol', 'carol-gh'), (7001, 'sub-pat', 'nobody-gh')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	at := base + "/api/v1/projects/claims-portal/packages/from-repository"
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	for who, c := range map[string]*http.Client{
		"a read-only member":     signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false),
		"a member with no grant": signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false),
	} {
		if code, out, _ := do(t, c, http.MethodPost, at, `{"ref":"trunk"}`); code != 403 || out["error"] != "team_required" {
			t.Errorf("%s: %d %s", who, code, lastBody)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("GitHub was asked %q for someone below contributor", *asked)
	}
	// the caller's own GitHub read, each call, on a linked app: none linked, one that can't read it, an admin with none
	for who, w := range map[string]struct {
		c     *http.Client
		code  int
		error string
	}{
		"a contributor with no GitHub account linked":      {signedIn(t, ctx, pool, base, "sub-quinn", "quinn@meridian.example", false), 403, "github_link_required"},
		"a contributor whose GitHub account can't read it": {signedIn(t, ctx, pool, base, "sub-pat", "pat@meridian.example", false), 404, "not_found"},
		"an admin with no GitHub account linked":           {signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true), 403, "github_link_required"},
	} {
		if code, out, _ := do(t, w.c, http.MethodPost, at, `{"ref":"trunk"}`); code != w.code || out["error"] != w.error {
			t.Errorf("%s: %d %s", who, code, lastBody)
		}
	}
	if count(t, ctx, pool, "SELECT count(*) FROM packages") != 0 {
		t.Fatal("a refused caller packaged something")
	}
	code, body, _ := do(t, carol, http.MethodPost, at, `{"ref":"trunk"}`)
	pkg, _ := body["package"].(map[string]any)
	source, _ := pkg["source"].(map[string]any)
	if code != 201 || source["kind"] != "repo" || source["ref"] != mainSHA || source["actor"] != "sub-carol" || pkg["id"] == "" {
		t.Fatalf("a contributor packages trunk: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/packages/from-repository' AND project_id = 'claims-portal' AND actor_subject = 'sub-carol' AND detail->>'package' = '"+pkg["id"].(string)+"' AND detail->>'ref' = '"+mainSHA+"'"); n != 1 {
		t.Errorf("the package's audit row: %d", n)
	}
	var current string // headPackage's rule: the newest package neither a pull request's nor a check's
	if err := pool.QueryRow(ctx, "SELECT id FROM packages WHERE project_id = 'claims-portal' AND source_kind NOT IN ('repo-pr', 'check') ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&current); err != nil || current != pkg["id"] {
		t.Errorf("trunk's package is current: %s (%v)", current, err)
	}
	if code, out, _ := do(t, carol, http.MethodPost, at, `{"ref":"release/2","check":true}`); code != 201 || out["package"].(map[string]any)["source"].(map[string]any)["kind"] != "check" ||
		count(t, ctx, pool, "SELECT count(*) FROM packages WHERE project_id = 'claims-portal' AND source_kind NOT IN ('repo-pr', 'check') AND source_ref = '"+releaseSHA+"'") != 0 {
		t.Errorf("a check of release/2 is never current: %d %s", code, lastBody)
	}
	for _, body := range []string{`{}`, `{"ref":""}`, `{"ref":"../trunk"}`, `{"ref":"-x"}`, `{"ref":"a//b"}`, `{"ref":"trunk/"}`, `{"ref":7}`} {
		if code, out, _ := do(t, carol, http.MethodPost, at, body); code != 400 || out["error"] != "invalid_ref" && out["error"] != "bad_request" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if code, out, _ := do(t, carol, http.MethodPost, at, `{"ref":"no-such"}`); code != 404 || out["error"] != "ref_not_found" {
		t.Errorf("a ref GitHub doesn't know: %d %s", code, lastBody)
	}
	const forked = "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0" // a fork's commit, which GitHub resolves through the parent repository
	if code, out, _ := do(t, carol, http.MethodPost, at, `{"ref":"`+forked+`"}`); code != 404 || out["error"] != "ref_not_found" ||
		out["message"] != "f0f0f0f isn't on acme/claims-portal's own branches: name one of its branches or tags, or a commit on trunk." ||
		count(t, ctx, pool, "SELECT count(*) FROM packages WHERE source_ref = '"+forked+"'") != 0 {
		t.Errorf("a fork's commit: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, carol, http.MethodPost, base+"/api/v1/projects/quote-engine/packages/from-repository", `{"ref":"trunk"}`); code != 404 || out["error"] != "not_linked" {
		t.Errorf("an app with no link: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, carol, http.MethodPost, base+"/api/v1/projects/nothing/packages/from-repository", `{"ref":"trunk"}`); code != 404 || out["error"] != "not_found" {
		t.Errorf("an unknown app: %d %s", code, lastBody)
	}
}
