package githubapp

import (
	"errors"
	"testing"

	"github.com/getcanyon-ai/canyon/events"
)

// refs is what the fake GitHub's commits answer resolves: each ref to the commit whose tarball hooked serves (its first
// hex digit picks the tree). A 40-hex ref is its own commit; any other ref is GitHub's 422.
var refs = map[string]string{
	"main":       "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334", // index.js
	"v1.0":       "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334",
	"release/2":  "8888888888888888888888888888888888888888", // checked.js
	"other-app":  "cccccccccccccccccccccccccccccccccccccccc", // canyon.toml names other-app
	"bad-toml":   "7777777777777777777777777777777777777777", // a canyon.toml that is not TOML
	"monorepo":   "6111111111111111111111111111111111111111", // apps/billing and apps/other
	"no-billing": "6000000000000000000000000000000000000000", // apps/other alone
}

// A ref of the linked repository is packaged on request (design/specs/mcp.md §2.3), only for a caller one of whose
// linked GitHub accounts can read it on GitHub: resolved to its commit through
// the link's installation and packaged as a push's is, the same tree once; by the person who asked, under the action
// they asked by, never via GitHub; a check's never current. No link, no such ref, a folder not in the commit,
// canyon.toml naming another app and a refused declaration package nothing.
func TestPackRef(t *testing.T) {
	g, pool, _ := hooked(t)
	if _, state := begin(g); finish(g, state, onlyState(state), "good").Code != 302 {
		t.Fatal("the App did not register")
	}
	if _, err := courier.PackRef(ctx, "claims-portal", "main", false, events.Actor{}, "x"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("an app with no link: %v", err)
	}
	linked(t, pool)
	carol := events.Actor{Kind: "person", Subject: "sub-carol", Display: "Carol"}
	const action = "POST /api/v1/projects/{id}/packages/from-repository"
	// the caller's own GitHub read, on every call: none linked, then one that can't read the repository, then one that can
	if _, err := courier.PackRef(ctx, "claims-portal", "main", false, carol, action); !errors.Is(err, ErrNoGitHubLink) {
		t.Fatalf("no GitHub account linked: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO github_links (github_id, subject, login) VALUES (7001, 'sub-carol', 'nobody-gh')"); err != nil {
		t.Fatal(err)
	}
	if _, err := courier.PackRef(ctx, "claims-portal", "main", false, carol, action); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("a linked account that can't read the repository: %v", err)
	}
	if rows(t, pool, "SELECT count(*) FROM packages") != 0 {
		t.Fatal("a refused caller packaged something")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO github_links (github_id, subject, login) VALUES (7002, 'sub-carol', 'carol-gh')"); err != nil {
		t.Fatal(err)
	}
	got, err := courier.PackRef(ctx, "claims-portal", "main", false, carol, action)
	if err != nil || got.Commit != refs["main"] || got.Package == "" || got.Refused != "" {
		t.Fatalf("main: %+v %v", got, err)
	}
	again, err := courier.PackRef(ctx, "claims-portal", "v1.0", false, carol, action)
	if err != nil || again.Package != got.Package {
		t.Fatalf("a tag at the same commit is the same package: %+v %v", again, err)
	}
	var who string
	if err := pool.QueryRow(ctx, `SELECT p.source_kind || ' ' || p.source_ref || ' ' || p.source_actor FROM packages p WHERE p.id = $1`, got.Package).Scan(&who); err != nil || who != "repo "+refs["main"]+" sub-carol" {
		t.Fatalf("main's package: %q %v", who, err)
	}
	if n := rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = '"+action+"' AND actor_subject = 'sub-carol' AND actor_kind = 'person' AND detail->>'package' = '"+got.Package+"' AND detail->>'ref' = '"+refs["main"]+"' AND via IS DISTINCT FROM 'github'"); n != 2 {
		t.Fatalf("one audit row each time, the person's, never via GitHub: %d", n)
	}
	if rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource' AND actor_subject = 'sub-carol'") == 0 ||
		rows(t, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource' AND actor_subject IS DISTINCT FROM 'sub-carol'") != 0 {
		t.Fatal("the package's declarations are the person's")
	}
	checked, err := courier.PackRef(ctx, "claims-portal", "release/2", true, carol, action)
	if err != nil || checked.Package == "" || checked.Commit != refs["release/2"] {
		t.Fatalf("a check of release/2: %+v %v", checked, err)
	}
	var current string // headPackage's rule: the newest package neither a pull request's nor a check's
	if err := pool.QueryRow(ctx, "SELECT id FROM packages WHERE source_kind NOT IN ('repo-pr', 'check') ORDER BY created_at DESC, id DESC LIMIT 1").Scan(&current); err != nil || current != got.Package ||
		rows(t, pool, "SELECT count(*) FROM packages WHERE id = '"+checked.Package+"' AND source_kind = 'check'") != 1 {
		t.Fatalf("a check's package is never current: the current one is %s (%v)", current, err)
	}
	if _, err := courier.PackRef(ctx, "claims-portal", "no-such-branch", false, carol, action); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("a ref GitHub doesn't know: %v", err)
	}
	if out, err := courier.PackRef(ctx, "claims-portal", "other-app", false, carol, action); err != nil || out.Refused != "other_app" || out.Package != "" ||
		out.Message != "canyon.toml names other-app, not claims-portal. This repository feeds claims-portal. Change the app line to claims-portal, or delete it." {
		t.Errorf("canyon.toml naming another app: %+v %v", out, err)
	}
	if out, err := courier.PackRef(ctx, "claims-portal", "bad-toml", false, carol, action); err != nil || out.Refused != "bad_request" || out.Cause == nil || out.Message != "canyon.toml is not TOML" {
		t.Errorf("a refused declaration: %+v %v", out, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE project_repositories SET folder = 'apps/billing' WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if out, err := courier.PackRef(ctx, "claims-portal", "no-billing", false, carol, action); err != nil || out.Refused != "folder_not_found" || out.Package != "" ||
		out.Message != "apps/billing isn't in 6000000: Canyon packages this app from apps/billing." {
		t.Errorf("a folder not in the commit: %+v %v", out, err)
	}
	// the link's folder alone: apps/billing holds index.js as main's root does, so its tree is main's, the same package;
	// the whole commit, apps/ and all, would be another
	if out, err := courier.PackRef(ctx, "claims-portal", "monorepo", false, carol, action); err != nil || out.Commit != refs["monorepo"] || out.Package != got.Package {
		t.Errorf("the link's folder of a commit, want main's package %s: %+v %v", got.Package, out, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if _, err := courier.PackRef(ctx, "claims-portal", "main", false, carol, action); !errors.Is(err, ErrNotLinked) {
		t.Errorf("a deleted app: %v", err)
	}
}
