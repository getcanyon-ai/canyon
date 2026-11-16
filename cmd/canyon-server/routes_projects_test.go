package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOwnerClaimAddsTheActorRenamesItAndRaisesTheOrgRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`); code != 200 || lastBody != `{"owners":[{"kind":"person","subject":"anonymous","display":"Dana Whitfield"}]}` {
		t.Fatalf("claim: %d %s", code, lastBody)
	}
	if _, _, _ = do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"D. Whitfield"}`); lastBody != `{"owners":[{"kind":"person","subject":"anonymous","display":"D. Whitfield"}]}` {
		t.Fatalf("the same actor claiming again renames itself, the set does not grow: %s", lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"`+strings.Repeat("é", 128)+`"}`); code != 200 {
		t.Fatalf("128 characters, not bytes: %d %s", code, lastBody)
	}
	for _, body := range []string{`{"display":""}`, `{}`, `{"display":"` + strings.Repeat("x", 129) + `"}`, `{"display":"` + strings.Repeat("é", 129) + `"}`} {
		if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", body); code != 400 || out["error"] != "invalid_display" {
			t.Fatalf("%s: %d %v", body[:40], code, out)
		}
	}
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/nothing/owner", `{"display":"x"}`); code != 404 || out["error"] != "not_found" {
		t.Fatalf("unknown project: %d %v", code, out)
	}
	// the three claims that took place are audited and announced on the fleet timeline; the five refused ones are not
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows r JOIN audit_events e ON e.id = r.data->>'event_id' WHERE r.scope = 'org' AND r.kind = 'audit.appended' AND r.project_id = 'claims-portal' AND e.action = 'PUT /api/v1/projects/{id}/owner' AND e.detail->>'display' <> ''"); n != 3 {
		t.Fatalf("org rows for the three claims that took place: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE project_id <> 'claims-portal'"); n != 0 {
		t.Fatalf("the claim on the unknown project left %d audit rows", n)
	}
}

func TestDeleteHidesTheProjectAndKeepsItsLedger(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', 'claims-portal', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_ended000', 'claims-portal', 'pkg_7d3e8f', 'blocked', now())",
		`UPDATE projects SET report_key_ref = 1, report_sealed_key = '\x00', report_sealed_value = '\x00' WHERE id = 'claims-portal'`, // its report key, as its first attach sealed it
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	orgRows := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE scope = 'org'")
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 || lastBody != "" {
		t.Fatalf("delete: %d %q", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE id = 'gr_ended000'"); n != 1 {
		t.Fatalf("the run stays: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects WHERE report_key_ref IS NULL AND report_sealed_key IS NULL AND report_sealed_value IS NULL"); n != 1 {
		t.Fatal("the report key goes with the project's secrets")
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); code != 404 {
		t.Fatalf("the single read after the delete: %d %s", code, lastBody)
	}
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/projects", ""); lastBody != `{"projects":[]}` {
		t.Fatalf("the list after the delete: %s", lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 404 {
		t.Fatalf("a second delete: %d", code)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"x"}`); code != 404 {
		t.Fatalf("a claim on a deleted project: %d", code)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 409 || body["error"] != "deleted" {
		t.Fatalf("the id stays taken (the ledger is keyed on it), and says it was deleted: %d %v", code, body)
	}
	// the ledger stays: the creation, the claim and the delete are on it; the refused attempts after the delete are not
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE project_id = 'claims-portal'"); n != 3 {
		t.Fatalf("audit rows naming the project (create, claim, delete): %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE project_id = 'claims-portal'"); n != 1 {
		t.Fatalf("owner grants: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE scope = 'org'"); n != orgRows+1 {
		t.Fatalf("org rows: %d, want %d (the delete)", n, orgRows+1)
	}
}

func TestListShowsEachLiveProjectWithItsStateAndOwnersById(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, name := range []string{"payroll", "claims-portal", "gone"} {
		do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`)
	}
	do(t, c, http.MethodPut, base+"/api/v1/projects/payroll/owner", `{"display":"Dana Whitfield"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	for _, q := range []string{
		"UPDATE projects SET created_at = '2026-08-20T00:00:00Z'", // payroll has no package: its latest is `created` since the project was
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', 'claims-portal', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_new00000', 'claims-portal', 'pkg_7d3e8f', 'blocked', '2026-09-01T10:00:00Z')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	do(t, c, http.MethodGet, base+"/api/v1/projects", "")
	want := `{"projects":[{"id":"claims-portal","name":"claims-portal","owners":[],"owner_groups":[],"summary":"","claimable":true,"access":{"level":"none","everyone":false,"people":0,"groups":0,"requests":0},"stack":"","gate_state":"blocked","envs":{},"latest":{"package":"pkg_7d3e8f","stage":"blocked","since":"2026-09-01T10:00:00Z"}},{"id":"payroll","name":"payroll","owners":[{"kind":"person","subject":"anonymous","display":"Dana Whitfield"}],"owner_groups":[],"summary":"","claimable":true,"access":{"level":"owner","everyone":false,"people":0,"groups":0,"requests":0},"stack":"","gate_state":"never_reviewed","envs":{},"latest":{"package":null,"stage":"created","since":"2026-08-20T00:00:00Z"}}]}`
	if lastBody != want {
		t.Fatalf("the list on the wire:\n got %s\nwant %s", lastBody, want)
	}
}

// Both project reads answer the fleet's row (routes.md row 47: one shape, everywhere a project is
// listed); the fleet adds only last_run, the single read only resources[] (none in the golden). The
// golden fleet has a preview and a prod deploy with a recorded actor, so deployed_by and the null
// promoted_by are compared too.
func TestProjectReadsAnswerTheFleetRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	c := client(t)
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects", "")
	byID := map[string]any{}
	for _, p := range list["projects"].([]any) {
		byID[p.(map[string]any)["id"].(string)] = p
	}
	for _, a := range fleet["apps"].([]any) {
		app := a.(map[string]any)
		delete(app, "last_run")
		delete(app, "warnings")                            // the fleet row's alone, as last_run is
		delete(app, "needs")                               // and needs (contracts/routes.md, *What needs a person*)
		for _, env := range app["envs"].(map[string]any) { // the fleet row's alone, as last_run is (contracts/routes.md, envs.<env>.state)
			delete(env.(map[string]any), "state")
		}
		id := app["id"].(string)
		code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/"+id, "")
		one, _ := got["project"].(map[string]any)
		resources, _ := one["resources"].([]any)
		repository, linked := one["repository"]
		delete(one, "resources")
		delete(one, "repository") // the single read's alone, as resources[] is (#1917)
		auto, hasAuto := one["auto_deploy"]
		delete(one, "auto_deploy") // and auto_deploy (#1923)
		if code != 200 || resources == nil || len(resources) != 0 || !linked || repository != nil || !hasAuto || auto != false || !reflect.DeepEqual(one, app) {
			t.Fatalf("GET /projects/%s:\n got %s\nwant %s", id, lastBody, mustJSON(app))
		}
		if !reflect.DeepEqual(byID[id], app) {
			t.Fatalf("%s in the list:\n got %s\nwant %s", id, mustJSON(byID[id]), mustJSON(app))
		}
	}
	if want := []string{"claims-archive", "claims-portal", "commission-calc", "endorsement-hub", "fnol-intake", "premium-ledger", "quote-engine"}; !reflect.DeepEqual(ids(t, map[string]any{"apps": list["projects"]}), want) {
		t.Fatalf("the list stays by id: %v", ids(t, map[string]any{"apps": list["projects"]}))
	}
}

// A project read's preview names the package running there by its own label — an older upload,
// not the newest one still sitting unreleased (contracts/routes.md, *Naming a version*).
func TestTheProjectReadNamesPreviewByItsPackagesLabelNotTheNewestUpload(t *testing.T) {
	cleanGate(t)
	_, _, base, c := withProject(t)
	_, older := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.0"})
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", `{"package":"v1.0"}`)
	run, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("deploy the labelled upload: %d %s", code, lastBody)
	}
	uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), map[string]string{"Canyon-Package-Label": "v2.0"}) // newer, never deployed
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	p := body["project"].(map[string]any)
	if preview := p["envs"].(map[string]any)["preview"].(map[string]any); preview["package"] != packageOf(older)["id"] || preview["label"] != "v1.0" {
		t.Fatalf("the project read's preview: %s, want the older upload's label v1.0", lastBody)
	}
	if got := p["latest"].(map[string]any)["label"]; got != "v2.0" {
		t.Fatalf("latest is still the newest upload's own label: %v", got)
	}
}

func TestEveryMutatingRouteWritesOneAuditRowNamedAfterItsPatternAndReadsWriteNone(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, step := range []struct{ method, path, body, action string }{
		{http.MethodPost, "/projects", `{"id":"claims-portal"}`, "POST /api/v1/projects"},
		{http.MethodPut, "/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`, "PUT /api/v1/projects/{id}/owner"},
		{http.MethodDelete, "/projects/claims-portal?confirm=claims-portal", "", "DELETE /api/v1/projects/{id}"},
	} {
		before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
		do(t, c, step.method, base+"/api/v1"+step.path, step.body)
		var named int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = $1", step.action).Scan(&named); err != nil || named != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events") != before+1 {
			t.Fatalf("%s %s: %d rows named %q (%v)", step.method, step.path, named, step.action, err)
		}
	}
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	for _, path := range []string{"/projects", "/projects/claims-portal", "/audit", "/audit?actor=person"} {
		do(t, c, http.MethodGet, base+"/api/v1"+path, "")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("four reads left %d audit rows", n-before)
	}
}

func TestAuditListFiltersByActorAndSinceAndAnswersTheSeamShape(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	// two rows canyon wrote in the same instant, the higher id inserted first: the order is by id, not by arrival
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, project_id, run_id, detail) VALUES
		('ev_c4ny0n0009', '2030-01-01T00:00:00Z', 'canyon', 'reconcile', 'run ended: interrupted', 'claims-portal', 'gr_9p4x1c', '{}'),
		('ev_c4ny0n0000', '2030-01-01T00:00:00Z', 'canyon', 'reconcile', 'run ended: interrupted', 'claims-portal', 'gr_7kd2mq', '{"reason": "boot"}'),
		('ev_c4ny0n000a', '2030-01-02T00:00:00Z', 'canyon', 'sweeper', 'packages expired', NULL, NULL, '{}')`); err != nil { // names nothing: the keys are absent on the wire
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_events SET ts = '2026-01-01T00:00:00Z' WHERE actor_kind = 'person'"); err != nil { // no wall clock in the since cases below
		t.Fatal(err)
	}
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/audit", "")
	events, _ := body["events"].([]any)
	if len(events) != 4 || body["next"] != nil {
		t.Fatalf("the whole trail, newest first, on one page: %s", lastBody)
	}
	first, _ := events[0].(map[string]any)
	_, hasProject := first["project"]
	_, hasRun := first["run"]
	if first["id"] != "ev_c4ny0n000a" || hasProject || hasRun {
		t.Fatalf("a row naming nothing carries neither key: %v", first)
	}
	last, _ := events[3].(map[string]any)
	_, hasRun = last["run"]
	if last["action"] != "POST /api/v1/projects" || last["project"] != "claims-portal" || hasRun {
		t.Fatalf("the person's row names the project it created and carries no run key at all: %v", last)
	}
	if events[1].(map[string]any)["id"] != "ev_c4ny0n0009" {
		t.Fatalf("a tie on ts is broken by id, the higher first: %s", lastBody)
	}
	canonical, _ := json.Marshal(events[2]) // sorted keys
	if want := `{"action":"run ended: interrupted","actor":{"display":"","kind":"canyon","subject":"reconcile"},"category":"governance","detail":{"reason":"boot"},"id":"ev_c4ny0n0000","project":"claims-portal","run":"gr_7kd2mq","ts":"2030-01-01T00:00:00Z"}`; string(canonical) != want {
		t.Fatalf("the event element:\n got %s\nwant %s", canonical, want)
	}
	for query, want := range map[string]int{
		"?actor=canyon": 3, "?actor=person": 1, "?actor=agent": 0,
		"?since=2026-01-01T00:00:00Z": 4, "?since=2029-12-31T00:00:00Z": 3, "?since=2030-01-01T00:00:00Z": 3, "?since=2030-01-01T00:00:00.000001Z": 1, "?since=2030-01-01T00:00:00%2B00:00": 3,
		"?actor=canyon&since=2031-01-01T00:00:00Z": 0,
	} {
		if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/audit"+query, ""); len(body["events"].([]any)) != want {
			t.Fatalf("%s: %s, want %d events", query, lastBody, want)
		}
	}
	for query, code := range map[string]string{"?actor=bogus": "invalid_actor", "?since=yesterday": "invalid_since", "?since=2030-01-01": "invalid_since"} {
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/audit"+query, ""); status != 400 || body["error"] != code {
			t.Fatalf("%s: %d %v", query, status, body)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 4 {
		t.Fatalf("the audit reads themselves left rows: %d", n-4)
	}
}

// The rows a mutation makes and its audit row are written by one transaction: Postgres stamps
// every row with the transaction that wrote it (xmin), so the stamps must match, checked right
// after each mutation, before a later one re-stamps the row.
func TestEveryMutationAndItsAuditRowShareOneTransaction(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	same := func(action, change string) {
		t.Helper()
		var audit, changed string
		if err := pool.QueryRow(ctx, "SELECT xmin::text FROM audit_events WHERE action = $1", action).Scan(&audit); err != nil {
			t.Fatalf("%s: the audit row: %v", action, err)
		}
		if err := pool.QueryRow(ctx, change).Scan(&changed); err != nil {
			t.Fatalf("%s: the changed row: %v", action, err)
		}
		if changed != audit {
			t.Fatalf("%s: the change (xmin %s) and its audit row (xmin %s) were written by different transactions", action, changed, audit)
		}
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	same("POST /api/v1/projects", "SELECT xmin::text FROM projects WHERE id = 'claims-portal'")
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	same("PUT /api/v1/projects/{id}/owner", "SELECT xmin::text FROM project_access WHERE project_id = 'claims-portal'")
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	same("POST /api/v1/projects/{id}/packages", "SELECT xmin::text FROM packages WHERE id = '"+packageOf(body)["id"].(string)+"'")
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	same("DELETE /api/v1/projects/{id}", "SELECT xmin::text FROM projects WHERE id = 'claims-portal'") // the soft delete's UPDATE re-stamps the row
}

// A deploy row written before deployed_by existed answers null for it, on the project read and
// on the fleet alike (routes.md row 47: no backfill).
// A change whose audit row cannot be written does not take place: the request fails and the
// transaction rolls the change back with it. The trigger stands in for the write failing; it
// lives in this test's own schema (pgtest.Schema), so no other test can meet it.
func TestAFailedAuditWriteFailsTheRequest(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit write refused'; END $$;
		CREATE TRIGGER refuse_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP TRIGGER refuse_audit ON audit_events; DROP FUNCTION refuse_audit()"); err != nil {
			t.Error(err)
		}
	})
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 500 || body["error"] != "internal" {
		t.Fatalf("a create whose audit row cannot be written: %d %s, want 500 internal", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects"); n != 0 {
		t.Fatalf("the project was written without its audit row: %d rows", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows"); n != 0 {
		t.Fatalf("the org timeline was written without its audit row: %d rows", n)
	}
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); code != 404 {
		t.Fatalf("the project is not there to read: %d %s", code, lastBody)
	}
}

func TestAPreMigrationDeployRowAnswersANullDeployedBy(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"legacy"}`)
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_1e9acy', 'legacy', 'aa', 'bb', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_1e9acy00', 'legacy', 'pkg_1e9acy', 'clean', '2026-09-01T10:00:00Z');
INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_at) VALUES
 ('legacy', 'preview', 'pkg_1e9acy', 'canyon/legacy@sha256:aa', 'http://legacy.preview.localhost:7777', 'gr_1e9acy00', '2026-09-01T10:01:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/projects/legacy", "/api/v1/fleet"} {
		status, _, _ := do(t, c, http.MethodGet, base+path, "")
		if status != 200 || !strings.Contains(lastBody, `"deployed_by":null`) {
			t.Fatalf("%s: a deploy row with no recorded actor answers deployed_by null: %d %s", path, status, lastBody)
		}
	}
}

// A new app is private (contracts/routes.md, App teams): POST /projects writes its creator as its owner,
// an email grant of the session's address, subject and display. Another member is not one.
func TestTheCreatorIsTheNewAppsOwner(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	alice := signedIn(t, ctx, pool, base, "sub-alice", "alice@meridian.example", false)
	bob := signedIn(t, ctx, pool, base, "sub-bob", "bob@meridian.example", false)
	if code, _, _ := do(t, alice, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 || !strings.Contains(lastBody, `"owners":[{"kind":"person","subject":"sub-alice","display":"sub-alice"}]`) {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE project_id = 'claims-portal' AND kind = 'email' AND grantee = 'alice@meridian.example' AND level = 'owner' AND granted_by_subject = 'sub-alice'"); n != 1 {
		t.Fatalf("the creator's owner grant: %d", n)
	}
	if code, _, _ := do(t, bob, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Bob Ives"}`); code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-alice") {
		t.Fatalf("another member claiming the new app: %d %s", code, lastBody)
	}
}

// ownerRequiredBody is the refusal on app, asking who (contracts/routes.md, App teams).
func ownerRequiredBody(app, who string) string {
	return `{"error":"owner_required","message":"only the owners of ` + app + ` and admins can do this: ask ` + who + `"}`
}

// The claim (contracts/routes.md, PUT …/owner): anyone may claim an app with no grant at owner, a
// person's or a group's, while it holds no secret and nothing is deployed; any other app is its owners'
// and admins' — an ownerless app holding either is an admin's to adopt. A refusal writes nothing; a claim
// by someone already on the team raises their grant to owner and keeps its roles.
func TestClaimIsOpenOnlyOnAnAppWithNoOwnerNoSecretAndNothingDeployed(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('empty', 'empty'), ('keyed', 'keyed'), ('running', 'running'), ('owned', 'owned'), ('grouped', 'grouped'), ('shared', 'shared')",
		`INSERT INTO secrets (project_id, env, name, scope, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ('keyed', 'preview', 'API_KEY', 'runtime', '\x00', '\x00', 'canyon', 'canyon')`,
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_1', 'running', 't', 'c', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_1', 'running', 'pkg_1', 'clean')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) VALUES ('running', 'prod', 'pkg_1', 'i', 'u', 'gr_1')",
		`INSERT INTO project_access (project_id, kind, grantee, level, subject, subject_issuer, display, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('owned', 'email', 'alice@meridian.example', 'owner', 'sub-alice', '` + goodOIDC["CANYON_OIDC_ISSUER"] + `', 'Alice Park', 'person', 'sub-alice', 'Alice Park'),
			('grouped', 'group', 'claims-team', 'owner', '', '', '', 'person', 'sub-alice', 'Alice Park')`,
		`INSERT INTO project_access (project_id, kind, grantee, level, roles, subject, subject_issuer, display, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('shared', 'email', 'bob@meridian.example', 'contributor', '{approver}', 'sub-bob', '` + goodOIDC["CANYON_OIDC_ISSUER"] + `', 'Bob Ives', 'person', 'sub-dana', 'Dana Whitfield')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	alice := signedIn(t, ctx, pool, base, "sub-alice", "alice@meridian.example", false)
	bob := signedIn(t, ctx, pool, base, "sub-bob", "bob@meridian.example", false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, read := range []string{"/api/v1/fleet?sort=name", "/api/v1/projects"} { // the fleet and the list answer it on every row, whole or reduced
		_, body, _ := do(t, bob, http.MethodGet, base+read, "")
		rows, _ := body["apps"].([]any)
		if rows == nil {
			rows, _ = body["projects"].([]any)
		}
		var open []string
		for _, r := range rows {
			if row := r.(map[string]any); row["claimable"] == true {
				open = append(open, row["id"].(string))
			} else if row["claimable"] != false {
				t.Fatalf("%s: %s carries no claimable: %s", read, row["id"], lastBody)
			}
		}
		if strings.Join(open, ",") != "empty,shared" {
			t.Fatalf("%s: bob may claim %v, want empty and shared, the one he contributes to", read, open)
		}
	}
	for _, c := range []struct {
		who, app string
		c        *http.Client
		want     int
		ask      string // whom the refusal asks
	}{
		{"bob", "empty", bob, 200, ""}, {"bob", "keyed", bob, 403, "an admin"}, {"bob", "running", bob, 403, "an admin"}, {"bob", "owned", bob, 403, "Alice Park"},
		{"bob", "grouped", bob, 403, "a member of the claims-team group"},
		{"alice, its owner", "owned", alice, 200, ""}, {"dana, an admin", "keyed", dana, 200, ""}, {"dana, an admin", "running", dana, 200, ""},
		{"bob, a contributor", "shared", bob, 200, ""}, {"dana, an admin", "grouped", dana, 200, ""},
	} {
		// the read's claimable is the claim's own answer, asked of the same caller just before it (the dashboard offers Claim by it)
		_, read, _ := do(t, c.c, http.MethodGet, base+"/api/v1/projects/"+c.app, "")
		if claimable, _ := read["project"].(map[string]any)["claimable"].(bool); claimable != (c.want == 200) {
			t.Fatalf("%s reading %s: claimable %v, but the claim answers %d: %s", c.who, c.app, claimable, c.want, lastBody)
		}
		if code, _, _ := do(t, c.c, http.MethodPut, base+"/api/v1/projects/"+c.app+"/owner", `{"display":"Claimer"}`); code != c.want || (code == 403 && lastBody != ownerRequiredBody(c.app, c.ask)) {
			t.Fatalf("%s claiming %s: %d %s, want %d", c.who, c.app, code, lastBody, c.want)
		}
	}
	var grants string
	if err := pool.QueryRow(ctx, "SELECT string_agg(concat_ws(' ', project_id, grantee, level, NULLIF(array_to_string(roles, '|'), ''), display), ',' ORDER BY project_id, grantee) FROM project_access WHERE kind = 'email'").Scan(&grants); err != nil ||
		grants != "empty bob@meridian.example owner Claimer,grouped dana@meridian.example owner Claimer,keyed dana@meridian.example owner Claimer,"+
			"owned alice@meridian.example owner Claimer,running dana@meridian.example owner Claimer,shared bob@meridian.example owner approver Claimer" {
		t.Fatalf("the grants after the claims: %q %v", grants, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/projects/{id}/owner'"); n != 6 {
		t.Fatalf("audit rows: %d, want the six claims that took place", n)
	}
}

const (
	deleteProject = "/api/v1/projects/claims-portal"
	confirmed     = deleteProject + "?confirm=claims-portal"
)

// Deleting a project destroys its database for real, and only once the id is sent back: without
// it, or with another, the answer lists what would go and nothing is stopped, dropped or hidden.
func TestDeleteDestroysTheDatabaseOnlyWithTheProjectIdSentBack(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	b, err := provisioners.NewBuiltin(ctx, pgtest.Exclusive(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	out, err := b.Ensure(ctx, provisioners.Key{Project: "claims-portal", Env: "preview", Name: "db"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: b}
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/DATABASE_URL", `{"value":"`+out.Outputs["DATABASE_URL"]+`"}`)
	if _, err := pool.Exec(ctx, "INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'db', 'postgres', 'builtin', 'ready')"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{deleteProject, deleteProject + "?confirm=claims", deleteProject + "?confirm="} {
		if code, _, _ := do(t, c, http.MethodDelete, base+path, ""); code != 409 || lastBody != `{"error":"confirm_required","message":"deleting a project destroys its resources and their data immediately: send ?confirm=\u003cproject id\u003e","resources":[{"name":"db","type":"postgres","env":"preview","state":"ready"}]}` {
			t.Fatalf("%s: %d %s", path, code, lastBody)
		}
	}
	conn, err := pgx.Connect(ctx, out.Outputs["DATABASE_URL"])
	if err != nil {
		t.Fatalf("an unconfirmed delete destroyed the database: %v", err)
	}
	_ = conn.Close(ctx)
	if fake.Stops.Load() != 0 || count(t, ctx, pool, "SELECT count(*) FROM secrets") != 1 || count(t, ctx, pool, "SELECT count(*) FROM resources WHERE state = 'ready'") != 1 {
		t.Fatalf("an unconfirmed delete changed something: %d stops", fake.Stops.Load())
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 {
		t.Fatalf("the confirmed delete: %d %s", code, lastBody)
	}
	if fake.Stops.Load() != 2 {
		t.Fatalf("both envs' containers stop first: %d stops", fake.Stops.Load())
	}
	if conn, err := pgx.Connect(ctx, out.Outputs["DATABASE_URL"]); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("the database survived the project's delete")
	}
	if n := count(t, ctx, pool, "SELECT (SELECT count(*) FROM resources) + (SELECT count(*) FROM secrets) + (SELECT count(*) FROM projects WHERE deleted_at IS NULL) + (SELECT count(*) FROM audit_events WHERE action = 'DELETE /api/v1/projects/{id}')"); n != 1 {
		t.Fatalf("records, secrets or the live project remain, or not one audit row: %d", n)
	}
	var destroyed string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'delete resource' AND project_id = 'claims-portal'").Scan(&destroyed); err != nil || destroyed != `{"env": "preview", "name": "db", "type": "postgres"}` {
		t.Fatalf("the destroyed database is attributed: %q (%v)", destroyed, err)
	}
}

// A record goes only when its driver answers gone: a failure or a pending answer keeps it, the
// secrets and the project, and a repeat delete resumes where the last one stopped, newest first.
// Refusals come first and stop nothing: an agent while prod holds data, a run in flight.
func TestDeleteKeepsEachRecordUntilItsDriverAnswersGone(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"sk_keep_7f3a"}`)
	if _, err := pool.Exec(ctx, `INSERT INTO resources (project_id, env, name, type, driver, state, updated_at) VALUES
		('claims-portal', 'preview', 'old', 'postgres', 'builtin', 'ready', '2026-01-01'), ('claims-portal', 'prod', 'new', 'postgres', 'builtin', 'ready', '2026-02-01');
		UPDATE sessions SET actor_kind = 'agent'`); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 403 || body["error"] != "person_required" { // prod's data weighs what taking prod down does
		t.Fatalf("an agent deleting prod's data: %d %s", code, lastBody)
	}
	answers := map[string]provisioners.Result{"new": {State: provisioners.Gone}, "old": {State: provisioners.Failed, Message: "quota"}}
	errs := map[string]error{"old": errors.New("unreachable")}
	var asked []string
	d := deleter(func(k provisioners.Key) (provisioners.Result, error) {
		asked = append(asked, fmt.Sprint(k.Name, fake.Stops.Load())) // both containers stop before a driver is asked
		return answers[k.Name], errs[k.Name]
	})
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: d, {"postgres", "prod"}: d}
	records := func() int { return count(t, ctx, pool, "SELECT count(*) FROM resources WHERE state = 'deleting'") } // a kept record is marked
	kept := func() bool {
		return count(t, ctx, pool, "SELECT (SELECT count(*) FROM secrets) + (SELECT count(*) FROM projects WHERE deleted_at IS NULL)") == 2
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person'; INSERT INTO runs (id, project_id, package_id, state) SELECT 'gr_busy00000', project_id, package_id, 'running' FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 409 || body["error"] != "run_in_progress" || fake.Stops.Load() != 0 || len(asked) != 0 {
		t.Fatalf("a run in flight: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // an error, then a Failed answer
		if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 502 || lastBody != `{"error":"teardown_failed","message":"the postgres old in preview could not be deleted: its data is kept, and deleting again resumes"}` || !kept() || records() != 1 {
			t.Fatalf("a driver failure: %d %s, %d records", code, lastBody, records())
		}
		delete(errs, "old")
	}
	// the one gone answer is on the trail, /status shows nothing running, and nothing new starts until the delete finishes
	if count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'delete resource' AND detail->>'name' = 'new'") != 1 || count(t, ctx, pool, "SELECT count(*) FROM deploys") != 0 {
		t.Fatalf("after the 502s: the gone record's audit row, and no deploys row")
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", ""); code != 409 || body["error"] != "deleting" {
		t.Fatalf("a deploy during the delete: %d %s", code, lastBody)
	}
	answers["old"] = provisioners.Result{State: provisioners.Pending}
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 202 || lastBody != `{"pending":[{"name":"old","type":"postgres","env":"preview","state":"deleting"}]}` || !kept() || records() != 1 || fmt.Sprint(asked) != "[new2 old2 old4 old6]" {
		t.Fatalf("a pending answer: %d %s, %d records, asked %v", code, lastBody, records(), asked)
	}
	answers["old"] = provisioners.Result{State: provisioners.Gone}
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 || records() != 0 || kept() {
		t.Fatalf("the resumed delete: %d %s", code, lastBody)
	}
}

// A promotion asked for while the project is being deleted is refused before its run starts: no run,
// no container. An agent may delete a preview-only project, not one deployed to prod.
func TestDeleteFreezesPromotionsAndAsksAPersonWhileProdIsDeployed(t *testing.T) {
	pool, ctx, base, c, run := previewed(t)
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleting_since = now()"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+run+`"}`); code != 409 || body["error"] != "deleting" || fake.Runs.Load() != 1 || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatalf("a promotion during the delete: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, url, gate_run FROM deploys; UPDATE sessions SET actor_kind = 'agent'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 403 || body["error"] != "person_required" {
		t.Fatalf("an agent deleting a project deployed to prod: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM deploys WHERE env = 'prod'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 {
		t.Fatalf("an agent deleting a preview-only project: %d %s", code, lastBody)
	}
}

// A record goes to the driver that made it, by name, never to whichever serves its type today: one
// whose driver is no longer configured is kept with its data, and the delete answers why.
func TestDeleteAsksTheDriverThatMadeTheRecord(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if _, err := pool.Exec(ctx, "INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'db', 'postgres', 'https://gone.internal/canyon', 'ready')"); err != nil {
		t.Fatal(err)
	}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: deleter(func(provisioners.Key) (provisioners.Result, error) {
		panic("the builtin never deletes what an adapter made")
	})}
	t.Cleanup(func() { pipe.Drivers = nil })
	if code, body, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 502 || body["error"] != "teardown_failed" || body["message"] != "the driver that made db is no longer configured: its data is kept" {
		t.Fatalf("a record whose driver is gone: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT (SELECT count(*) FROM resources) + (SELECT count(*) FROM projects WHERE deleted_at IS NULL)"); n != 2 {
		t.Fatalf("the record or the project went: %d", n)
	}
}

// deleter is a driver that only deletes, answering as the function says; it stands in for the builtin.
type deleter func(provisioners.Key) (provisioners.Result, error)

func (deleter) Name() string { return "builtin" }

func (deleter) Ensure(context.Context, provisioners.Key, string) (provisioners.Result, error) {
	return provisioners.Result{}, nil
}

func (d deleter) Delete(_ context.Context, k provisioners.Key) (provisioners.Result, error) {
	return d(k)
}

// The executor forgets the app's images and build caches before the project is marked deleted: a
// failure there keeps the project, as a driver's does, and the same request again completes. A boot
// asks it again for every deleted project, so one deleted before this landed is forgotten too.
func TestDeleteForgetsTheAppsImagesBeforeTheProjectGoes(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	fake.FailForget.Store(true)
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 502 || lastBody != `{"error":"teardown_failed","message":"the app's images and build caches could not be removed: deleting again resumes"}` || fake.Forgets.Load() != 1 {
		t.Fatalf("a failed forget: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects WHERE deleted_at IS NULL"); n != 1 {
		t.Fatalf("a failed forget deleted the project")
	}
	fake.FailForget.Store(false)
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 || fake.Forgets.Load() != 2 {
		t.Fatalf("deleting again: %d %s", code, lastBody)
	}
	forgetDeleted(ctx, pool, &executor.Client{Base: fake.URL}, log.New(io.Discard, "", 0), time.Millisecond)
	if fake.Forgets.Load() != 3 {
		t.Fatalf("the boot pass forgets the deleted project: %d forgets", fake.Forgets.Load())
	}
}

// The server and executor roll together, so the boot pass can start before the executor answers: a
// refused forget is retried until it does, never left for the next server restart (#1858).
func TestTheBootPassRetriesAForgetUntilTheExecutorAnswers(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	if code, _, _ := do(t, c, http.MethodDelete, base+confirmed, ""); code != 204 {
		t.Fatalf("delete: %d %s", code, lastBody)
	}
	before := fake.Forgets.Load()
	down := &notUpYet{}
	down.refusals.Store(2)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second) // a pass that never succeeds fails here, never hangs
	defer cancel()
	forgetDeleted(ctx, pool, &executor.Client{Base: fake.URL, HTTP: &http.Client{Transport: down}}, log.New(io.Discard, "", 0), time.Millisecond)
	if fake.Forgets.Load() != before+1 || down.refusals.Load() > 0 {
		t.Fatalf("the boot pass gave up on an executor not up yet: %d forgets reached it", fake.Forgets.Load()-before)
	}
}

// notUpYet refuses the connection its first refusals times, as an executor still starting does.
type notUpYet struct{ refusals atomic.Int32 }

func (n *notUpYet) RoundTrip(r *http.Request) (*http.Response, error) {
	if n.refusals.Add(-1) >= 0 {
		return nil, syscall.ECONNREFUSED
	}
	return http.DefaultTransport.RoundTrip(r)
}

// A deleted project keeps its name and its ledger, and every route answers it 404: creating that name
// again says it was deleted, never that it exists, which the CLI would read as the app being there (#864).
func TestCreatingADeletedProjectsNameSaysItWasDeleted(t *testing.T) {
	_, _, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", ""); code != 200 && code != 204 {
		t.Fatalf("delete: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	if msg, _ := body["message"].(string); code != 409 || body["error"] != "deleted" || !strings.Contains(msg, "An app named gone was deleted") {
		t.Fatalf("creating a deleted app's name: %d %s", code, lastBody)
	}
}

// Nothing inside a mutation's transaction takes a second connection: eight creates of an app that
// exists, at once, on a pool of four — pgxpool's default on a small node — each answer 409 within
// seconds. On the test cluster eleven deploys at once deadlocked the server for 30 s (#1693): every create held
// its connection and the ledger lock's queue, and the one holding the lock asked the pool for a second.
func TestConcurrentCreatesOfAnExistingNameAllAnswerOnAPoolOfFour(t *testing.T) {
	pool, ctx := migrated(t)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config().Copy()
	cfg.MaxConns = 4
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	a := &app{pool: small}
	codes := make(chan int, 8)
	for range 8 {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			a.createProject(rec, httptest.NewRequestWithContext(cctx, http.MethodPost, "/api/v1/projects", strings.NewReader(`{"id":"claims-portal"}`)))
			codes <- rec.Code
		}()
	}
	for range 8 {
		if code := <-codes; code != http.StatusConflict {
			t.Errorf("a create of an existing name answered %d, want 409", code)
		}
	}
}

// Renaming an app is its owners' and admins' (contracts/routes.md, PUT …/name; #1909): anyone else is
// refused owner_required with one denied row and nothing changes; an owner and an admin rename it, and
// only its name changes — its id stays every route's — with one audit row per change and none for the
// name it already has.
func TestAnOwnerOrAnAdminRenamesAnAppAndItsIdStays(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	carl, dana := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	rename := base + "/api/v1/projects/claims-portal/name"
	for who, c := range map[string]*http.Client{"a contributor": carl, "a viewer": signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false),
		"a member with no access": signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)} {
		if code, _, _ := do(t, c, http.MethodPut, rename, `{"name":"Motor claims"}`); code != 403 || lastBody != ownerRequiredBody("claims-portal", "sub-olive") {
			t.Errorf("%s renames the app: %d %s", who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects WHERE name = 'claims-portal'") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND project_id = 'claims-portal'"); n != 4 {
		t.Fatalf("a refused rename changed the name, or wrote other than one denied row each: %d", n)
	}
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"two\nlines"}`, `{"name":"` + strings.Repeat("a", 101) + `"}`} {
		if code, _, _ := do(t, olive, http.MethodPut, rename, body); code != 400 || !strings.Contains(lastBody, `"invalid_name"`) {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	for _, c := range []struct {
		who  string
		c    *http.Client
		name string
	}{{"the owner", olive, "Motor claims"}, {"the owner, the same name again", olive, "Motor claims"}, {"an admin", dana, "Claims portal"}} {
		code, body, _ := do(t, c.c, http.MethodPut, rename, `{"name":"`+c.name+`"}`)
		if p, _ := body["project"].(map[string]any); code != 200 || p["id"] != "claims-portal" || p["name"] != c.name {
			t.Fatalf("%s renames it %q: %d %s", c.who, c.name, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'rename app' AND project_id = 'claims-portal' AND (
		(actor_subject = 'sub-olive' AND detail = '{"from": "claims-portal", "to": "Motor claims"}') OR (actor_subject = 'sub-dana' AND detail = '{"from": "Motor claims", "to": "Claims portal"}'))`); n != 2 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'rename app'") != 2 {
		t.Fatalf("one audit row per change, none for the same name: %d", n)
	}
	if code, _, _ := do(t, carl, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Fatalf("the app is still claims-portal to every route: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/projects/nope/name", `{"name":"x"}`); code != 404 {
		t.Fatalf("an unknown app: %d %s", code, lastBody)
	}
}
