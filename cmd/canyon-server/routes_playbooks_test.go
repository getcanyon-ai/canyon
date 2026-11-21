package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

const playbooksAt = "/api/v1/playbooks"

// playbookJSON is a PUT's body.
func playbookJSON(name, kind, when, body string) string {
	return string(mustJSON(map[string]string{"name": name, "kind": kind, "when": when, "body": body}))
}

// playbooksGolden is contracts/testdata/guardrails/preferences.json.
func playbooksGolden(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/guardrails/preferences.json")
	var doc map[string]map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// seedPlaybook saves slug's next version as an admin's dashboard would, in SQL: name, a skill, one when line and body.
func seedPlaybook(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, name string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO playbook_versions (slug, version, name, kind, when_line, body, set_by_kind, set_by_subject, set_by_display)
		SELECT $1, coalesce(max(version), 0) + 1, $2, 'skill', 'Read before you add a route.', '# How we build'||chr(10), 'person', 'sub-dana', 'Dana Kim' FROM playbook_versions WHERE slug = $1`, slug, name); err != nil {
		t.Fatal(err)
	}
}

// seedGoldenPlaybooks saves the golden's playbooks as the versions it lists them at: incident-notes' one, and
// service-conventions' three.
func seedGoldenPlaybooks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	doc := playbooksGolden(t)
	versions := doc["playbook_versions"]["versions"].([]any)
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i].(map[string]any)
		if _, err := pool.Exec(ctx, `INSERT INTO playbook_versions (slug, version, name, kind, when_line, body, set_by_kind, set_by_subject, set_by_display) VALUES ($1, $2, $3, $4, $5, $6, 'person', 'sub-dana', 'Dana Kim')`,
			doc["playbook_versions"]["slug"], int(v["version"].(float64)), v["name"], v["kind"], v["when"], v["body"]); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range doc["playbooks"]["playbooks"].([]any) {
		if e := e.(map[string]any); e["slug"] != doc["playbook_versions"]["slug"] {
			if _, err := pool.Exec(ctx, `INSERT INTO playbook_versions (slug, version, name, kind, when_line, body, set_by_kind, set_by_subject, set_by_display) VALUES ($1, 1, $2, $3, $4, '# Incidents'||chr(10), 'person', 'sub-dana', 'Dana Kim')`,
				e["slug"], e["name"], e["kind"], e["when"]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// playbookWrites is every row a playbook's change writes.
func playbookWrites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, ctx, pool, "SELECT count(*) FROM playbook_versions") + count(t, ctx, pool, "SELECT count(*) FROM playbook_removals") +
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action IN ('set playbook', 'restore playbook', 'remove playbook')")
}

// lastPlaybookAudit is the newest playbook audit row's detail.
func lastPlaybookAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action string) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = $1 ORDER BY ts DESC, id DESC LIMIT 1", action).Scan(&detail); err != nil {
		t.Fatalf("the %s row: %v", action, err)
	}
	return detail
}

// The playbook routes answer contracts/testdata/guardrails/preferences.json (contracts/routes.md, the playbooks row):
// a version saved with its diff, an old one restored, the list, one, the versions, and the manifest's playbooks.
func TestPlaybookRoutesAnswerTheGolden(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET actor_display = 'Dana Kim' WHERE actor_subject = 'sub-dana'`); err != nil {
		t.Fatal(err)
	}
	doc := playbooksGolden(t)
	slug := doc["playbook_versions"]["slug"].(string)
	versions := doc["playbook_versions"]["versions"].([]any)
	first := versions[len(versions)-1].(map[string]any)
	note := doc["playbooks"]["playbooks"].([]any)[0].(map[string]any)
	for path, body := range map[string]string{note["slug"].(string): playbookJSON(note["name"].(string), note["kind"].(string), note["when"].(string), "# Incidents\n"),
		slug: playbookJSON(first["name"].(string), first["kind"].(string), first["when"].(string), first["body"].(string))} {
		if code, _, _ := do(t, dana, http.MethodPut, base+playbooksAt+"/"+path, body); code != 200 {
			t.Fatalf("saving %s: %d %s", path, code, lastBody)
		}
	}
	put := doc["playbook_put"]
	code, got, _ := do(t, dana, http.MethodPut, base+playbooksAt+"/"+slug, string(mustJSON(put["request"])))
	want := put["response"].(map[string]any)
	if code != 200 || got["playbook"] == nil {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	stamped(t, got["playbook"].(map[string]any), want["playbook"].(map[string]any), "updated_at", 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the save answered %s,\nwant the golden's", lastBody)
	}
	if detail := lastPlaybookAudit(t, ctx, pool, "set playbook"); !reflect.DeepEqual(detail, put["audit"].(map[string]any)["detail"]) {
		t.Fatalf("the save's audit row: %s,\nwant the golden's", mustJSON(detail))
	}
	restore := doc["playbook_restore"]
	code, got, _ = do(t, dana, http.MethodPost, base+playbooksAt+"/"+slug+"/restore", string(mustJSON(restore["request"])))
	want = restore["response"].(map[string]any)
	if code != 200 || got["playbook"] == nil {
		t.Fatalf("the restore: %d %s", code, lastBody)
	}
	stamped(t, got["playbook"].(map[string]any), want["playbook"].(map[string]any), "updated_at", 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the restore answered %s,\nwant the golden's", lastBody)
	}
	if detail := lastPlaybookAudit(t, ctx, pool, "restore playbook"); !reflect.DeepEqual(detail, restore["audit"].(map[string]any)["detail"]) {
		t.Fatalf("the restore's audit row: %s,\nwant the golden's", mustJSON(detail))
	}
	countGoldenPlaybooks(t, ctx, pool, base, dana)
	code, got, _ = do(t, dana, http.MethodGet, base+playbooksAt, "")
	listed := doc["playbooks"]
	if code != 200 || len(got["playbooks"].([]any)) != 2 {
		t.Fatalf("the list: %d %s", code, lastBody)
	}
	for i, e := range got["playbooks"].([]any) {
		stamped(t, e.(map[string]any), listed["playbooks"].([]any)[i].(map[string]any), "updated_at", 0)
	}
	if !reflect.DeepEqual(got, listed) {
		t.Fatalf("the list answered %s,\nwant the golden's", lastBody)
	}
	code, got, _ = do(t, dana, http.MethodGet, base+playbooksAt+"/"+slug, "")
	if code != 200 || got["playbook"] == nil {
		t.Fatalf("the read: %d %s", code, lastBody)
	}
	stamped(t, got["playbook"].(map[string]any), doc["playbook"]["playbook"].(map[string]any), "updated_at", 0)
	if !reflect.DeepEqual(got, doc["playbook"]) {
		t.Fatalf("the read answered %s,\nwant the golden's", lastBody)
	}
	code, got, _ = do(t, dana, http.MethodGet, base+playbooksAt+"/"+slug+"/versions", "")
	if code != 200 || len(got["versions"].([]any)) != len(versions) {
		t.Fatalf("the versions: %d %s", code, lastBody)
	}
	for i, v := range got["versions"].([]any) {
		stamped(t, v.(map[string]any), versions[i].(map[string]any), "set_at", 0)
	}
	if !reflect.DeepEqual(got, doc["playbook_versions"]) {
		t.Fatalf("the versions answered %s,\nwant the golden's", lastBody)
	}
	if code, got := skillManifest(t, dana, base); code != 200 || !reflect.DeepEqual(got["playbooks"], doc["manifest"]["playbooks"]) {
		t.Fatalf("the manifest's playbooks: %d %v,\nwant the golden's %v", code, got["playbooks"], doc["manifest"]["playbooks"])
	}
}

// Every member reads a playbook that stands, and the list; writing, removing, restoring and every version are an
// admin's, a person's, from a browser (design/PERMISSIONS.md, Playbooks): a member is 403 admin_required, an agent
// or canyon actor 403 person_required, the admin's CLI session 403 dashboard_required, each with one denied row
// and nothing else written.
func TestPlaybookWritesAreAnAdminsFromABrowser(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, got, _ := do(t, carl, http.MethodGet, base+playbooksAt+"/service-conventions", ""); code != 200 || got["playbook"].(map[string]any)["body"] != "# How we build\n" {
		t.Fatalf("a member's read: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, carl, http.MethodGet, base+playbooksAt, ""); code != 200 || len(got["playbooks"].([]any)) != 1 {
		t.Fatalf("a member's list: %d %s", code, lastBody)
	}
	writes := playbookWrites(t, ctx, pool)
	body := playbookJSON("n", "note", "w", "b")
	for name, req := range map[string]struct{ method, path, body string }{
		"a save":      {http.MethodPut, "/service-conventions", body},
		"a removal":   {http.MethodDelete, "/service-conventions", ""},
		"a restore":   {http.MethodPost, "/service-conventions/restore", `{"version":1}`},
		"the history": {http.MethodGet, "/service-conventions/versions", ""},
	} {
		if code, _, _ := do(t, carl, req.method, base+playbooksAt+req.path, req.body); code != 403 || lastBody != adminRequired {
			t.Errorf("a member's %s: %d %s", name, code, lastBody)
		}
	}
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	if code, _, _ := do(t, danaCLI, http.MethodPut, base+playbooksAt+"/service-conventions", body); code != 403 || lastBody != `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}` {
		t.Errorf("the admin's CLI session: %d %s", code, lastBody)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, kind := range []string{"agent", "canyon"} {
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1 WHERE actor_subject = 'sub-dana'", kind); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := do(t, dana, http.MethodPut, base+playbooksAt+"/service-conventions", "{}"); code != 403 || lastBody != `{"error":"person_required","message":"only a person can change a playbook"}` {
			t.Errorf("an %s actor: %d %s", kind, code, lastBody)
		}
	}
	if n := playbookWrites(t, ctx, pool); n != writes {
		t.Fatalf("the refusals wrote %d row(s)", n-writes)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' IN ('admin_required', 'person_required', 'dashboard_required')"); n != 7 {
		t.Fatalf("%d denied rows, want 7: four member's, the CLI's and the two actors'", n)
	}
}

// What the routes row refuses, each with nothing written: a field out of shape or a secret, 400 invalid_playbook in
// D5's words; a key the route does not take, 400 bad_request; a 51st, 409 too_many_playbooks; a version or a
// playbook it doesn't have, 404.
func TestPlaybookRefusalsWriteNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	key := "AKIA" + "QJ5K7MZ2RT4WX6YB" // assembled here, so no file of the repo carries one whole
	ok := func(name, kind, when, body string) string { return playbookJSON(name, kind, when, body) }
	for name, req := range map[string]struct{ slug, body, message string }{
		"a slug with a capital":     {"Service", ok("n", "skill", "w", "b"), "A playbook's slug is 1 to 40 of a-z, 0-9 and -, beginning with a letter."},
		"a slug over 40":            {strings.Repeat("a", 41), ok("n", "skill", "w", "b"), "A playbook's slug is 1 to 40 of a-z, 0-9 and -, beginning with a letter."},
		"a slug with a digit first": {"1st", ok("n", "skill", "w", "b"), "A playbook's slug is 1 to 40 of a-z, 0-9 and -, beginning with a letter."},
		"an empty name":             {"svc", ok("", "skill", "w", "b"), "A playbook's name is 1 to 80 characters on one line."},
		"a name over 80":            {"svc", ok(strings.Repeat("n", 81), "skill", "w", "b"), "A playbook's name is 1 to 80 characters on one line."},
		"a kind that is neither":    {"svc", ok("n", "rule", "w", "b"), "A playbook is a skill or a note."},
		"a when over 200":           {"svc", ok("n", "skill", strings.Repeat("w", 201), "b"), "When to read it is one line of 1 to 200 characters."},
		"a when on two lines":       {"svc", ok("n", "skill", "a\nb", "b"), "When to read it is one line of 1 to 200 characters."},
		"an empty body":             {"svc", ok("n", "skill", "w", ""), "A playbook's body is 1 to 64 KiB of Markdown, with no control character but a newline or a tab."},
		"a body over 64 KiB":        {"svc", ok("n", "skill", "w", strings.Repeat("b", 64<<10+1)), "A playbook's body is 1 to 64 KiB of Markdown, with no control character but a newline or a tab."},
		"a control character":       {"svc", ok("n", "skill", "w", "a\u0007b"), "A playbook's body is 1 to 64 KiB of Markdown, with no control character but a newline or a tab."},
		"a secret in the body":      {"svc", ok("n", "skill", "w", "aws_access_key_id="+key), playbookSecret},
		"a secret in when":          {"svc", ok("n", "skill", "aws_access_key_id="+key, "b"), playbookSecret},
		"a tag character":           {"svc", ok("n", "skill", "w", "ok\U000E0041"), "A playbook's body holds U+E0041 at character 3, a character that hides or reorders text: remove it."},
		"a skill's shell line":      {"svc", ok("n", "skill", "w", "Status: !`git status`"), playbookCommand},
	} {
		if code, out, _ := do(t, c, http.MethodPut, base+playbooksAt+"/"+req.slug, req.body); code != 400 || out["error"] != "invalid_playbook" || out["message"] != req.message {
			t.Errorf("%s: %d %s", name, code, lastBody)
		}
	}
	if code, out, _ := do(t, c, http.MethodPut, base+playbooksAt+"/svc", `{"name":"n","kind":"skill","when":"w","body":"b","version":2}`); code != 400 || out["error"] != "bad_request" {
		t.Errorf("a key the route does not take: %d %s", code, lastBody)
	}
	if n := playbookWrites(t, ctx, pool); n != 0 {
		t.Fatalf("a refused save wrote %d row(s)", n)
	}
	for i := range maxPlaybooks {
		seedPlaybook(t, ctx, pool, fmt.Sprintf("p-%d", i), "Playbook")
	}
	if code, out, _ := do(t, c, http.MethodPut, base+playbooksAt+"/one-more", ok("n", "note", "w", "b")); code != 409 || out["error"] != "too_many_playbooks" || out["message"] != playbookTooMany {
		t.Errorf("a 51st: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+playbooksAt+"/p-0", ok("Renamed", "skill", "w", "b")); code != 200 {
		t.Errorf("a 50th's next version: %d %s", code, lastBody)
	}
	for name, req := range map[string]struct{ method, path, body string }{
		"a version it never had":    {http.MethodPost, "/p-1/restore", `{"version":9}`},
		"a playbook it never had":   {http.MethodPost, "/nope/restore", `{"version":1}`},
		"removing one it never had": {http.MethodDelete, "/nope", ""},
		"reading one it never had":  {http.MethodGet, "/nope", ""},
		"the history of none":       {http.MethodGet, "/nope/versions", ""},
	} {
		if code, out, _ := do(t, c, req.method, base+playbooksAt+req.path, req.body); code != 404 || out["error"] != "not_found" {
			t.Errorf("%s: %d %s", name, code, lastBody)
		}
	}
	if code, out, _ := do(t, c, http.MethodPost, base+playbooksAt+"/p-1/restore", `{"version":0}`); code != 400 || out["error"] != "bad_request" {
		t.Errorf("a restore of version 0: %d %s", code, lastBody)
	}
}

// A removal keeps every version, readable to admins alone (contracts/routes.md, the playbooks row): the playbook is
// gone from the list, the read and the manifest, removing it again is 404, and a restore or a save makes it stand
// again at the next version. A save that changes nothing writes nothing.
func TestPlaybookRemovalKeepsItsVersionsForAdmins(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	body := playbookJSON("How Meridian writes a service", "skill", "Read before you add a route.", "# How we build\n")
	if code, _, _ := do(t, c, http.MethodPut, base+playbooksAt+"/service-conventions", body); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	writes := playbookWrites(t, ctx, pool)
	if code, got, _ := do(t, c, http.MethodPut, base+playbooksAt+"/service-conventions", body); code != 200 || got["playbook"].(map[string]any)["version"] != float64(1) || playbookWrites(t, ctx, pool) != writes {
		t.Fatalf("a save that changes nothing: %d %s, %d rows", code, lastBody, playbookWrites(t, ctx, pool)-writes)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+playbooksAt+"/service-conventions", ""); code != 204 {
		t.Fatalf("the removal: %d %s", code, lastBody)
	}
	if detail := lastPlaybookAudit(t, ctx, pool, "remove playbook"); !reflect.DeepEqual(detail, map[string]any{"slug": "service-conventions", "version": float64(1)}) {
		t.Fatalf("the removal's row: %v", detail)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+playbooksAt+"/service-conventions", ""); code != 404 {
		t.Errorf("a removed playbook read: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+playbooksAt, ""); code != 200 || lastBody != `{"playbooks":[]}` {
		t.Errorf("the list after the removal: %d %s", code, lastBody)
	}
	if _, got := skillManifest(t, c, base); len(got["playbooks"].([]any)) != 0 {
		t.Errorf("the manifest hands out a removed playbook: %v", got["playbooks"])
	}
	if code, got, _ := do(t, c, http.MethodGet, base+playbooksAt+"/service-conventions/versions", ""); code != 200 || got["removed"] != true || len(got["versions"].([]any)) != 1 {
		t.Errorf("an admin's history of a removed playbook: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+playbooksAt+"/service-conventions", ""); code != 404 {
		t.Errorf("removing it again: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, c, http.MethodPost, base+playbooksAt+"/service-conventions/restore", `{"version":1}`); code != 200 || got["playbook"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("restoring it: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, c, http.MethodGet, base+playbooksAt+"/service-conventions/versions", ""); code != 200 || got["removed"] != false || len(got["versions"].([]any)) != 2 {
		t.Errorf("the history once it stands again: %d %s", code, lastBody)
	}
}

// A custom rule points at a playbook (contracts/routes.md, the custom-rules row): one the org doesn't have is 400;
// one it has is kept with the version, read back and audited, and the snapshot carries its slug and name while it
// stands, following its name and leaving it when it is removed, with the version moving each time; the rule's own
// list still names it, and pointing at none is a version of its own.
func TestPlaybookLinkFromACustomRule(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedPlaybook(t, ctx, pool, "service-conventions", "How Meridian writes a service")
	linked := func(slug string) string {
		return `{"name":"` + emailName + `","instructions":"` + emailText + `","playbook":` + slug + `}`
	}
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, linked(`"nope"`)); code != 400 || out["error"] != "invalid_custom_rule" || out["message"] != noSuchPlaybook || customWrites(t, ctx, pool) != 0 {
		t.Fatalf("a playbook the org doesn't have: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, linked(`"Bad Slug"`)); code != 400 || out["message"] != noSuchPlaybook {
		t.Fatalf("a slug out of shape: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, linked(`"service-conventions"`)); code != 200 || out["custom_rule"].(map[string]any)["playbook"] != "service-conventions" {
		t.Fatalf("the link: %d %s", code, lastBody)
	}
	ruleOf := func(s gatepolicy.Snapshot) gatepolicy.Rule {
		t.Helper()
		for _, r := range s.Rules {
			if r.ID == emailRule {
				return r
			}
		}
		t.Fatalf("no %s in the snapshot", emailRule)
		return gatepolicy.Rule{}
	}
	snap := composedWith(t, ctx, pool)
	if pb := ruleOf(snap).Playbook; pb == nil || *pb != (gatepolicy.Playbook{Slug: "service-conventions", Name: "How Meridian writes a service"}) {
		t.Fatalf("the snapshot's playbook: %+v", pb)
	}
	seedPlaybook(t, ctx, pool, "service-conventions", "Meridian services") // renamed
	renamed := composedWith(t, ctx, pool)
	if pb := ruleOf(renamed).Playbook; pb == nil || pb.Name != "Meridian services" || renamed.Version <= snap.Version || renamed.Hash == snap.Hash {
		t.Fatalf("after the rename: %+v, version %d after %d", pb, renamed.Version, snap.Version)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+playbooksAt+"/service-conventions", ""); code != 204 {
		t.Fatalf("removing the playbook: %d %s", code, lastBody)
	}
	removed := composedWith(t, ctx, pool)
	if ruleOf(removed).Playbook != nil || removed.Version <= renamed.Version {
		t.Fatalf("a removed playbook is still carried: %+v, version %d after %d", ruleOf(removed).Playbook, removed.Version, renamed.Version)
	}
	if code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/custom-rules", ""); code != 200 || out["custom_rules"].([]any)[0].(map[string]any)["playbook"] != "service-conventions" {
		t.Fatalf("the rule's list after the removal: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+emailPath, linked(`null`)); code != 200 || out["custom_rule"].(map[string]any)["playbook"] != nil || out["custom_rule"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("pointing at none: %d %s", code, lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail->>'playbook' FROM audit_events WHERE action = 'custom_rule.set' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil ||
		detail != `{"after": null, "before": "service-conventions"}` {
		t.Fatalf("the unlink's audit row: %s (%v)", detail, err)
	}
}
