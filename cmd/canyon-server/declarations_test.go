package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/provisioners"
)

// provisioning gives the server's pipeline a driver for each type in preview until the test ends, so a
// declaration of it is accepted at upload (#1255). No deploy here asks one to ensure anything.
func provisioning(t *testing.T, types ...string) {
	t.Helper()
	drivers := provisioners.Drivers{}
	for _, typ := range types {
		drivers[[2]string{typ, "preview"}] = &provisioners.Builtin{}
	}
	pipe.Drivers = drivers
	t.Cleanup(func() { pipe.Drivers = nil })
}

// withResources is a package whose canyon.toml carries the given [resources] lines.
func withResources(t *testing.T, lines string) string {
	t.Helper()
	return tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n\n[resources]\n" + lines, "src/app.ts": "export {}\n"})
}

// Every declaration the server refuses, each with the words that tell the author what to write;
// a refused upload is no package.
func TestUploadRefusesEveryDeclarationItCannotServe(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	provisioning(t, "postgres")
	for _, tc := range []struct{ name, toml, says string }{
		{"two types give one env var", "db = { type = \"postgres\" }\ncache = { type = \"postgres\" }\n", `resources "cache" and "db" both give the app DATABASE_URL`},
		{"an unknown type names the menu", "db = { type = \"mysql\" }\n", `"mysql", which this host does not offer: the types are postgres, s3`},
		{"a type this host does not provision yet", "files = { type = \"s3\" }\n", `resource "files" has type "s3", which this host does not provision yet`}, // s3 has no driver here (#1255)
		{"a name off the pattern", "Main_DB = { type = \"postgres\" }\n", `resource "Main_DB": a resource name is lowercase letters`},
		{"a key beside type", "db = { type = \"postgres\", size = \"large\" }\n", `resource "db": a declaration is db = { type = "<type>" } and nothing else`},
		{"a bare string", "db = \"postgres\"\n", `resource "db": a declaration is db = { type = "<type>" }`},
		{"a type that is not a string", "db = { type = 5 }\n", `resource "db": a declaration is`},
		{"not TOML at all", "db = { type = \n", "canyon.toml is not TOML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := upload(t, c, base, "claims-portal", withResources(t, tc.toml), "", "")
			if msg, _ := body["message"].(string); code != 400 || body["error"] != "invalid_resources" || !strings.Contains(msg, tc.says) {
				t.Fatalf("%d %s", code, lastBody)
			}
		})
	}
	nested := func(open, close string) string { // 64 KiB of nesting, under the size cap: refused as TOML, not a crash
		n := (64<<10 - 64) / (len(open) + len(close))
		return "x = " + strings.Repeat(open, n) + "1" + strings.Repeat(close, n) + "\n"
	}
	form := `a [resources] section of name = { type = "<type>" } lines` // resources in any other form
	for _, tc := range [][2]string{
		{"resources = \"postgres\"\n", form},
		{"resources = { db = { type = \"postgres\" } }\n", form},
		{"resources.db = { type = \"postgres\" }\n", form},
		{"[resources.db]\ntype = \"postgres\"\n", form},
		{"[resources]\n[resources.db]\ntype = \"postgres\"\n", form},
		{"[resources]\ndb.type = \"postgres\"\n", form},
		{"[[resources]]\ndb = { type = \"postgres\" }\n", form},
		{nested("[", "]"), "canyon.toml is not TOML"},
		{nested("{b=", "}"), "canyon.toml is not TOML"},
		{strings.Repeat("#", 64<<10+1), "canyon.toml is larger than 64 KiB"},
	} {
		code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n" + tc[0]}), "", "")
		if msg, _ := body["message"].(string); code != 400 || body["error"] != "invalid_resources" || !strings.Contains(msg, tc[1]) {
			t.Errorf("%d bytes ending %.8q: %d %.200s", len(tc[0]), tc[0][max(0, len(tc[0])-8):], code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages"); n != 0 {
		t.Fatalf("a refused upload is no package: %d rows", n)
	}
}

// The same tree again answers its package, whatever its canyon.toml says (a pre-declaration binary took it).
func TestReuploadingAnExistingTreeIsNotRefusedForItsDeclaration(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	tree := withResources(t, "db = { type = \"mysql\" }\n")
	repo, _ := reposByBase.Load(strings.TrimPrefix(base, "http://"))
	pkg, err := ingest.Ingest(ctx, filepath.Join(repo.(string), "claims-portal.git"), strings.NewReader(tree), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_ref, source_actor) VALUES ($1, 'claims-portal', $2, $3, $4, 'upload', '', 'ana')", pkg.ID, pkg.TreeHash, pkg.Commit, pkg.Bytes); err != nil {
		t.Fatal(err)
	}
	code, body := uploadPulled(t, c, base, "claims-portal", tree)
	if p := packageOf(body); code != 201 || p["id"] != pkg.ID || p["source"].(map[string]any)["actor"] != "anonymous" { // its last uploader (contracts/routes.md, canyonv2-o49o)
		t.Fatalf("the existing package: %d %s", code, lastBody)
	}
}

// A declaration is audited the first time the project makes it: a newer package that repeats it,
// or the same tree again, writes no second row; a new name, or the same name with another type, does.
func TestTheFirstDeclarationOfAResourceIsOneAuditRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	provisioning(t, "postgres", "s3")
	declares := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'")
	}
	first := withResources(t, "db = { type = \"postgres\" }\n")
	if code, _ := uploadPulled(t, c, base, "claims-portal", first); code != 201 || declares() != 1 {
		t.Fatalf("the first declaration: %d, %d rows: %s", code, declares(), lastBody)
	}
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'declare resource'").Scan(&detail); err != nil || !strings.Contains(detail, `"name": "db"`) || !strings.Contains(detail, `"type": "postgres"`) {
		t.Fatalf("the row's detail: %s (%v)", detail, err)
	}
	uploadPulled(t, c, base, "claims-portal", first)
	if code, _ := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n[resources]\ndb = { type = \"postgres\" }\n", "b.ts": "b"})); code != 201 || declares() != 1 {
		t.Fatalf("a repeat was audited again: %d, %d rows", code, declares())
	}
	if code, _ := uploadPulled(t, c, base, "claims-portal", withResources(t, "db = { type = \"s3\" }\nfiles = { type = \"postgres\" }\n")); code != 201 || declares() != 3 {
		t.Fatalf("a new name and a new type for db: %d, %d rows", code, declares())
	}
}

// resources[] on the single read is the records and the current package's declarations: a
// declaration with no record reads declared, a record answers for its name per env, a resource a
// newer package dropped stays while its record exists, and one dropped with no record is gone.
func TestTheProjectReadListsRecordsAndTheCurrentDeclarations(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	provisioning(t, "postgres", "s3")
	read := func() string {
		t.Helper()
		code, body, _ := do(t, c, "GET", base+"/api/v1/projects/claims-portal", "")
		if code != 200 {
			t.Fatalf("the read: %d %s", code, lastBody)
		}
		return string(mustJSON(body["project"].(map[string]any)["resources"])) // re-encoded, so each element's keys are sorted
	}
	if got := read(); got != `[]` {
		t.Fatalf("no package: %s", got)
	}
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a.ts": "a"}))
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages WHERE resources IS NULL"); n != 1 || read() != `[]` {
		t.Fatalf("a package with no canyon.toml declares nothing: %d NULL rows, %s", n, read())
	}
	uploadPulled(t, c, base, "claims-portal", withResources(t, "db = { type = \"postgres\" }\nfiles = { type = \"s3\" }\n"))
	if got := read(); got != `[{"name":"db","state":"declared","type":"postgres"},{"name":"files","state":"declared","type":"s3"}]` {
		t.Fatalf("declared: %s", got)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resources (project_id, env, name, type, driver, state) VALUES
		('claims-portal', 'prod', 'db', 'postgres', 'builtin', 'pending'), ('claims-portal', 'preview', 'db', 'postgres', 'builtin', 'ready')`); err != nil {
		t.Fatal(err)
	}
	uploadPulled(t, c, base, "claims-portal", withResources(t, "cache = { type = \"s3\" }\n"))
	if got := read(); got != `[{"name":"cache","state":"declared","type":"s3"},{"env":"preview","name":"db","state":"ready","type":"postgres"},{"env":"prod","name":"db","state":"pending","type":"postgres"}]` {
		t.Fatalf("db dropped with records, files dropped without: %s", got)
	}
}

// The fixture's resources[] is what the project read answers for the rows behind it: the current
// package (pkg_9e40) declares db, the one preview runs (pkg_7a1c) declared reports, which has a record, and
// files, which the org's platform provided when a release of it reported (#2432).
func TestTheProjectReadAnswersTheGoldenResources(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres"}' WHERE id = 'pkg_9e40';
UPDATE packages SET resources = '{"reports": "postgres", "files": "s3"}' WHERE id = 'pkg_7a1c';
INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'reports', 'postgres', 'builtin', 'ready');
INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at)
	VALUES ('rel_9e41', 'claims-portal', 'preview', 'gr_9e41', 'pkg_7a1c', 'deploy', 'registry.example/canyon/claims-portal@sha256:`+reportDigest+`', '5f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b', 'deployed', now());
INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ('claims-portal', 'preview', 'files', 's3://claims-portal-files', 'rel_9e41')`); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, client(t), "GET", base+"/api/v1/projects/claims-portal", "")
	if want := appPage(t)["resources"]; code != 200 || !reflect.DeepEqual(body["project"].(map[string]any)["resources"], want) {
		t.Fatalf("%d %s\nwant %s", code, lastBody, mustJSON(want))
	}
}

// Every [roles] section and description the server refuses, each with the words that tell the author
// what to write; a refused upload is no package.
func TestUploadRefusesRolesAndADescriptionItCannotRead(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	var seventeen strings.Builder
	for i := range 17 {
		fmt.Fprintf(&seventeen, "role-%d = \"Role %d\"\n", i, i)
	}
	form, text := `a [roles] section of name = "what the role lets a person do" lines`, "description in canyon.toml is one line of text in quotes"
	for _, tc := range [][3]string{ // canyon.toml after its app line, the error, the words
		{"roles = \"approver\"\n", "invalid_roles", form},
		{"roles = { approver = \"Approves claims\" }\n", "invalid_roles", form},
		{"roles.approver = \"Approves claims\"\n", "invalid_roles", form},
		{"[roles.approver]\nline = \"Approves claims\"\n", "invalid_roles", form},
		{"[[roles]]\napprover = \"Approves claims\"\n", "invalid_roles", form},
		{"[roles]\napprover.line = \"Approves claims\"\n", "invalid_roles", form},
		{"[roles]\nApprover = \"Approves claims\"\n", "invalid_roles", `role "Approver": a role name is lowercase letters, digits and dashes`},
		// a dashed name shown, not only described: an agent's spot_manager cost a check (the 2026-10-08 QA rerun)
		{"[roles]\nspot_manager = \"Manages spots\"\n", "invalid_roles", `role "spot_manager": a role name is lowercase letters, digits and dashes, starting with a letter, at most 40 characters, as in expense-approver`},
		{"[roles]\napprover = 5\n", "invalid_roles", `role "approver": a role is approver = "what it lets a person do" and nothing else`},
		{"[roles]\napprover = { line = \"Approves claims\" }\n", "invalid_roles", `role "approver": a role is approver =`},
		{"[roles]\napprover = \"" + strings.Repeat("a", 121) + "\"\n", "invalid_roles", `role "approver": its line is 121 characters, at most 120`},
		{"[roles]\n" + seventeen.String(), "invalid_roles", "canyon.toml declares 17 roles: an app has at most 16"},
		{"description = 5\n", "invalid_description", text},
		{"description = [\"Files claims\"]\n", "invalid_description", text},
		{"description.short = \"Files claims\"\n", "invalid_description", text},
		{"description = \"" + strings.Repeat("a", 201) + "\"\n", "invalid_description", "description in canyon.toml is 201 characters: at most 200"},
		{"description = \"Files \\u001b[31mclaims\"\n", "invalid_description", "description in canyon.toml is one line of plain text: no line breaks or control characters"},
		{"description = \"\"\"Files claims\nand tracks them\"\"\"\n", "invalid_description", "description in canyon.toml is one line of plain text: no line breaks or control characters"},
		{"[roles]\napprover = \"Approves \\u009b2J claims\"\n", "invalid_roles", `role "approver": its line is one line of plain text: no line breaks or control characters`},
		{"description = \"Files \\u202eclaims\"\n", "invalid_description", "description in canyon.toml is one line of plain text: no line breaks or control characters"},
		{"description = \"Files\\u200bclaims\"\n", "invalid_description", "description in canyon.toml is one line of plain text: no line breaks or control characters"},
		{"[roles]\napprover = \"Approves\\u2028claims\"\n", "invalid_roles", `role "approver": its line is one line of plain text: no line breaks or control characters`},
	} {
		code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n" + tc[0]}), "", "")
		if msg, _ := body["message"].(string); code != 400 || body["error"] != tc[1] || !strings.Contains(msg, tc[2]) {
			t.Errorf("%.40q: %d %.200s", tc[0], code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages"); n != 0 {
		t.Fatalf("a refused upload is no package: %d rows", n)
	}
}

// The most the server accepts — 16 roles of 120 characters, a description of 200, counted in
// characters, not bytes — is stored with the package as written, and so are an empty [roles] and "".
func TestUploadStoresTheRolesAndDescriptionItAccepts(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	line, description := strings.Repeat("é", 120), strings.Repeat("ü", 200)
	file := fmt.Sprintf("app = \"claims-portal\"\ndescription = %q\n\n[roles]\n", description)
	for i := range 16 {
		file += fmt.Sprintf("role-%d = %q\n", i, line)
	}
	stored := func(toml string) (roles map[string]string, description *string) {
		t.Helper()
		code, body := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": toml}))
		if code != 201 {
			t.Fatalf("%d %s", code, lastBody)
		}
		if err := pool.QueryRow(ctx, "SELECT roles, description FROM packages WHERE id = $1", packageOf(body)["id"]).Scan(&roles, &description); err != nil {
			t.Fatal(err)
		}
		return roles, description
	}
	if roles, got := stored(file); len(roles) != 16 || roles["role-15"] != line || got == nil || *got != description {
		t.Fatalf("16 roles of 120 characters and 200 of description: %d roles, %q", len(roles), roles["role-15"])
	}
	if roles, got := stored("app = \"claims-portal\"\ndescription = \"\"\n[roles]\n"); roles == nil || len(roles) != 0 || got == nil || *got != "" {
		t.Fatalf("an empty [roles] and an empty description: %v %v", roles, got)
	}
}

// summary is the current package's description on the single read, the list and the fleet row: ""
// before any package, the description once one declares it, "" again when a newer package has none.
// A pull request's package is never current, so its description is never the summary.
func TestTheDescriptionIsTheSummaryOnEveryRead(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	summaries := func() []any {
		t.Helper()
		_, one, _ := do(t, c, "GET", base+"/api/v1/projects/claims-portal", "")
		_, list, _ := do(t, c, "GET", base+"/api/v1/projects", "")
		_, fleet, _ := do(t, c, "GET", base+"/api/v1/fleet", "")
		return []any{one["project"].(map[string]any)["summary"], list["projects"].([]any)[0].(map[string]any)["summary"], fleet["apps"].([]any)[0].(map[string]any)["summary"]}
	}
	want := func(summary, when string) {
		t.Helper()
		if got := summaries(); !reflect.DeepEqual(got, []any{summary, summary, summary}) {
			t.Fatalf("%s: the read, the list and the fleet answer %q, want %q", when, got, summary)
		}
	}
	want("", "no package")
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\ndescription = \"Files and tracks insurance claims\"\n"}))
	want("Files and tracks insurance claims", "a package with a description")
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_ref, source_actor, description, created_at)
		VALUES ('pkg_pr', 'claims-portal', 'pr', 'pr', 1, 'repo-pr', 'pr', 'github:dana', 'A pull request''s', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	want("Files and tracks insurance claims", "a newer pull request")
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a.ts": "a"}))
	want("", "a newer package with none")
}
