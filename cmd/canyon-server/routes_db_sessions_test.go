package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/provisioners"
)

const (
	dbSessions  = "/api/v1/projects/claims-portal/db-sessions"
	previewRead = `{"env":"preview","mode":"read"}`
	prodRead    = `{"env":"prod","mode":"read","reason":"find the duplicated March invoices"}`
	prodWrite   = `{"env":"prod","mode":"write","reason":"fix the duplicated March invoices","replace":true}`
)

// dbHost is responderHost's claims-portal — owner olive, contributor carl, viewer vic, admin dana,
// responder rhea — with dbReady's database: what a database session opens on. It answers the driver too.
func dbHost(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client, *provisioners.Builtin) {
	t.Helper()
	pool, ctx, base, olive := responderHost(t)
	return pool, ctx, base, olive, dbReady(t, ctx, pool)
}

// dbReady has claims-portal declare one postgres database, db, which the built-in driver on the test
// server makes ready in both environments, turns database sessions on (#2835), and answers that driver.
func dbReady(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *provisioners.Builtin {
	t.Helper()
	b, err := provisioners.NewBuiltin(ctx, pgtest.Exclusive(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	for _, env := range []string{"preview", "prod"} {
		k := provisioners.Key{Project: "claims-portal", Env: env, Name: "db"}
		if _, err := b.Ensure(ctx, k, "postgres"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := b.Delete(ctx, k); err != nil {
				t.Error(err)
			}
		})
	}
	runSQL(t, ctx, pool, `UPDATE packages SET resources = '{"db": "postgres"}'`, `UPDATE org_preferences SET db_sessions = 'on'`,
		`INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ('claims-portal', 'preview', 'db', 'postgres', 'builtin', 'ready'), ('claims-portal', 'prod', 'db', 'postgres', 'builtin', 'ready')`)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: b, {"postgres", "prod"}: b}
	return b
}

// roleHolds is how many roles the person role role is a member of, and whether it logs in, read as the
// test server's superuser.
func roleHolds(t *testing.T, ctx context.Context, role string) (groups int, login bool) {
	t.Helper()
	server, err := pgx.Connect(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close(ctx) }()
	if err := server.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid), r.rolcanlogin FROM pg_roles r WHERE r.rolname = $1`, role).Scan(&groups, &login); err != nil {
		t.Fatal(err)
	}
	return groups, login
}

// dbShape checks body against one definition of contracts/schema/db-session.schema.json.
func dbShape(t *testing.T, definition, body string) {
	t.Helper()
	if err := dbShapeOf(definition, body); err != nil {
		t.Errorf("%s is not the contract's: %v\n%s", definition, err, body)
	}
}

// dbShapeOf is why body is not one definition of contracts/schema/db-session.schema.json, nil when it is.
func dbShapeOf(definition, body string) error {
	return shapeOf("db-session", definition, body)
}

// shapeOf is why body is not one definition of contracts/schema/<schema>.schema.json, nil when it is, with
// every schema of contracts/schema/ added under its own $id, so the refs between them resolve without a network.
func shapeOf(schemaName, definition, body string) error {
	files, err := filepath.Glob("../../contracts/schema/*.schema.json")
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	c.UseRegexpEngine(func(pattern string) (jsonschema.Regexp, error) { return regexp.Compile(re2(pattern)) })
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the repository's own schemas, by glob
		var doc any
		if err == nil {
			doc, err = jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		}
		if err == nil {
			id, _ := doc.(map[string]any)["$id"].(string)
			err = c.AddResource(id, doc)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	schema, err := c.Compile("https://docs.getcanyon.ai/contracts/schema/" + schemaName + ".schema.json#/definitions/" + definition)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err != nil {
		return err
	}
	return schema.Validate(doc)
}

// re2 is a schema's pattern, written for the ECMAScript and Python engines CI's validator runs, as Go's
// RE2 reads it: a \uXXXX escape as \x{XXXX}, and the reason's one lookahead, ^(?=.*[^\sX])[^X]+$ — at
// least one visible character, and none of X — as its RE2 equal, ^[^X]*[^\sX][^X]*$.
func re2(pattern string) string {
	pattern = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).ReplaceAllString(pattern, `\x{$1}`)
	if m := regexp.MustCompile(`^\^\(\?=\.\*\[\^\\s(.*)\]\)\[\^(.*)\]\+\$$`).FindStringSubmatch(pattern); m != nil && m[1] == m[2] {
		return `^[^` + m[1] + `]*[^\s` + m[1] + `][^` + m[1] + `]*$`
	}
	return pattern
}

// runSQL runs each statement on pool, failing the test on the first error.
func runSQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// Opening a database session and listing them is a contributor's (design/PERMISSIONS.md, Database
// sessions): a viewer, a member with no access and a responder with no grant are refused team_required, and
// an agent under a contributor's address person_required, each with one denied row and nothing else
// written; the owner, a contributor, an admin and a responder granted contributor open theirs and read every
// open one, oldest first. One is ended by its person (closed) and by the app's owners and admins (ended), its
// closed row the contract's, and is not there for anyone else: a viewer, a contributor, a member with no
// access, a responder.
func TestAViewerIsRefused(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	for _, c := range []struct{ who, sub, email string }{{"a viewer", "sub-vic", "vic@meridian.example"},
		{"a member with no access", "sub-nora", "nora@meridian.example"}, {"a responder with no grant", "sub-rhea", responderEmail}} {
		refused := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			if code, body, _ := do(t, refused, method, base+dbSessions, previewRead); code != 403 || body["error"] != "team_required" {
				t.Errorf("%s: %s: %d %s", c.who, method, code, lastBody)
			}
		}
	}
	agent := signedIn(t, ctx, pool, base, "agent:claude-code/carl", "carl@meridian.example", false)
	runSQL(t, ctx, pool, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'agent:claude-code/carl'")
	if code, body, _ := do(t, agent, http.MethodPost, base+dbSessions, previewRead); code != 403 || body["error"] != "person_required" {
		t.Errorf("an agent: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'"); n != 7 || count(t, ctx, pool, "SELECT count(*) FROM db_sessions")+count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE 'db %'") != 0 {
		t.Fatalf("the refusals wrote %d denied row(s), or a database session", n)
	}
	carl, dana := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	ids := map[string]string{}
	for _, c := range []struct {
		who string
		c   *http.Client
	}{{"sub-olive", olive}, {"sub-carl", carl}, {"sub-dana", dana}} {
		code, body, _ := do(t, c.c, http.MethodPost, base+dbSessions, previewRead)
		person, _ := body["person"].(map[string]any)
		if role, _ := body["role"].(string); code != 201 || person["subject"] != c.who || body["yours"] != true || !strings.HasPrefix(role, "p_") {
			t.Fatalf("%s opens: %d %s", c.who, code, lastBody)
		}
		dbShape(t, "db_session", lastBody)
		ids[c.who] = body["id"].(string)
	}
	code, list, _ := do(t, carl, http.MethodGet, base+dbSessions, "")
	sessions, _ := list["db_sessions"].([]any)
	if code != 200 || len(sessions) != 3 || sessions[0].(map[string]any)["id"] != ids["sub-olive"] || sessions[1].(map[string]any)["yours"] != true || sessions[2].(map[string]any)["yours"] != false {
		t.Fatalf("carl's list: %d %s", code, lastBody)
	}
	dbShape(t, "list", lastBody)
	end := func(c *http.Client, who string) int {
		code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/db-sessions/"+ids[who], "")
		return code
	}
	for _, c := range []struct {
		who string
		c   *http.Client
	}{{"a viewer", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)}, {"a contributor", carl},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)}, {"a responder", signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)}} {
		if code := end(c.c, "sub-olive"); code != 404 || lastBody != `{"error":"not_found","message":"no such database session"}` {
			t.Errorf("%s ends the owner's: %d %s", c.who, code, lastBody)
		}
	}
	for _, c := range []struct {
		by     *http.Client
		whose  string
		reason string
		actor  string
	}{{olive, "sub-carl", "ended", "sub-olive"}, {dana, "sub-olive", "ended", "sub-dana"}, {dana, "sub-dana", "closed", "sub-dana"}} {
		if code := end(c.by, c.whose); code != 204 {
			t.Fatalf("%s's ended by %s: %d %s", c.whose, c.actor, code, lastBody)
		}
		if n := count(t, ctx, pool, `SELECT count(*) FROM db_sessions s JOIN audit_events e ON e.detail->>'db_session' = s.id
			WHERE s.id = '`+ids[c.whose]+`' AND s.close_reason = '`+c.reason+`' AND e.action = 'db session closed' AND e.detail->>'reason' = '`+c.reason+`' AND e.actor_subject = '`+c.actor+`'`); n != 1 {
			t.Errorf("%s's ended by %s: %d closed row(s) %s", c.whose, c.actor, n, c.reason)
		}
		closedShape(t, ctx, pool, ids[c.whose])
	}
	if code := end(dana, "sub-dana"); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db session closed'") != 3 {
		t.Fatalf("ending one closed already: %d, or it wrote a row", code)
	}
	runSQL(t, ctx, pool, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', '`+responderEmail+`', 'contributor', 'person', 'sub-olive', '')`)
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), http.MethodPost, base+dbSessions, previewRead); code != 201 {
		t.Fatalf("a responder granted contributor opens: %d %s", code, lastBody)
	}
}

// closedShape checks the db session closed row of the database session sid against the contract's
// closed_detail.
func closedShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sid string) {
	t.Helper()
	var detail string
	if err := pool.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'db session closed' AND detail->>'db_session' = $1", sid).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	dbShape(t, "closed_detail", detail)
}

// A prod database session needs a reason where it writes a row — its open, and the first attach of each
// other sign-in — and nowhere else: a later request of a sign-in on its trail, the opener's included, needs
// none and records none (design/PERMISSIONS.md, Database sessions). Before it, db_prod says whether
// production is reachable at all — with or without a reason, naming no page that sets it — and after it, writing
// there is an owner's.
func TestProdNeedsAReason(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	for _, open := range []string{prodRead, `{"env":"prod","mode":"read"}`} {
		if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, open); code != 409 || body["error"] != "db_prod_off" || body["message"] != "Canyon doesn't open production databases yet: this version opens preview databases only." {
			t.Fatalf("db_prod off, %s: %d %s", open, code, lastBody)
		}
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'read'")
	for _, reason := range []string{``, `,"reason":""`, `,"reason":"   "`, `,"reason":"right\u202eto left"`, `,"reason":"` + strings.Repeat("r", 501) + `"`} {
		if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, `{"env":"prod","mode":"read"`+reason+`}`); code != 400 || body["error"] != "reason_required" {
			t.Errorf("an open with %q: %d %s", reason, code, lastBody)
		}
	}
	code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, prodRead)
	if code != 201 || opened["reason"] != "find the duplicated March invoices" || opened["idle_timeout"] != 900.0 {
		t.Fatalf("the open with its reason: %d %s", code, lastBody)
	}
	dbShape(t, "db_session", lastBody)
	sid := opened["id"].(string)
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'db session opened' AND detail->>'reason' = 'find the duplicated March invoices' AND detail->>'via_tool' = 'dashboard'`); n != 1 {
		t.Fatalf("the open's row: %d", n)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, `{"env":"prod","mode":"read"}`); code != 200 || body["id"] != sid {
		t.Fatalf("the opener's attach without a reason: %d %s", code, lastBody)
	}
	cli := fromCLI(t, ctx, pool, base, "sub-carl", "carl@meridian.example")
	if code, body, _ := do(t, cli, http.MethodPost, base+dbSessions, `{"env":"prod","mode":"read"}`); code != 400 || body["error"] != "reason_required" {
		t.Fatalf("another sign-in's first attach without a reason: %d %s", code, lastBody)
	}
	for range 2 {
		if code, body, _ := do(t, cli, http.MethodPost, base+dbSessions, `{"env":"prod","mode":"read","reason":"INC-2231 from the terminal"}`); code != 200 || body["id"] != sid {
			t.Fatalf("its attach with one: %d %s", code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'db session attached' AND detail->>'reason' = 'INC-2231 from the terminal' AND via = 'cli' AND detail->>'via_tool' = 'connect'`); n != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE 'db %'") != 2 {
		t.Fatalf("the attach rows: %d of the CLI's, and the trail holds others", n)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, prodWrite); code != 409 || body["error"] != "db_prod_read_only" {
		t.Fatalf("a prod write while db_prod is read: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'write'")
	if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, prodWrite); code != 403 || body["error"] != "owner_required" || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != 1 {
		t.Fatalf("a contributor's prod write: %d %s", code, lastBody)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false) // an admin reads each row's session
	code, audit, _ := do(t, dana, http.MethodGet, base+"/api/v1/audit?project=claims-portal", "")
	events, _ := audit["events"].([]any)
	if code != 200 || len(events) == 0 {
		t.Fatalf("the trail: %d %s", code, lastBody)
	}
	for _, e := range events {
		if action, _ := e.(map[string]any)["action"].(string); strings.HasPrefix(action, "db ") {
			dbShape(t, "audit_row", string(mustJSON(e)))
		}
	}
}

// stalled is the built-in driver whose Issue says it began on began, then waits for let before it does
// anything: a login under way, held at its start.
type stalled struct {
	*provisioners.Builtin
	began, let chan struct{}
}

func (s stalled) Issue(ctx context.Context, k provisioners.Key, p provisioners.Person, m provisioners.Mode, ttl time.Duration, connect func(context.Context, provisioners.Login) error) error {
	s.began <- struct{}{}
	<-s.let
	return s.Builtin.Issue(ctx, k, p, m, ttl, connect)
}

// send is one request from c answered on the channel by its status, -1 when it never got one: what a
// test's goroutine runs, where do's t.Fatal and lastBody are not for it.
func send(c *http.Client, method, url, body string) <-chan int {
	status := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			status <- -1
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	return status
}

// An end racing a login on the same database session waits for it, on the person role's lock, and then
// revokes: whichever comes first, no login lands after the close (design/DATABASE-SESSIONS.md §6, Races).
// The login here is an attach's, held at its start while the end is asked; once both answer the session
// is closed and the person's role holds no group and logs in no more.
func TestEndRacingAConnectLeavesNoLogin(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("the open: %d %s", code, lastBody)
	}
	sid, role := opened["id"].(string), opened["role"].(string)
	s := stalled{Builtin: b, began: make(chan struct{}, 2), let: make(chan struct{})}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: s, {"postgres", "prod"}: s} // both: a resource finds its driver by name
	attached := send(carl, http.MethodPost, base+dbSessions, previewRead)
	select {
	case <-s.began:
	case code := <-attached:
		t.Fatalf("the attach answered %d and never issued a login", code)
	}
	ended := send(carl, http.MethodDelete, base+"/api/v1/db-sessions/"+sid, "")
	e := 0
	select {
	case e = <-ended:
		t.Errorf("the end answered %d while the attach's login was under way", e)
	case <-time.After(500 * time.Millisecond):
	}
	close(s.let)
	if e == 0 {
		e = <-ended
	}
	if a := <-attached; a != 200 || e != 204 {
		t.Fatalf("the attach answered %d, the end %d", a, e)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+sid+"' AND close_reason = 'closed'") + count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"); n != 1 {
		t.Fatalf("the end closed %s and left none open: %d", sid, 2-n)
	}
	if groups, login := roleHolds(t, ctx, role); groups != 0 || login {
		t.Fatalf("a login landed after the close: %s holds %d group(s), logs in: %v", role, groups, login)
	}
}

// Each check closes a database session that fails it, with its reason, Canyon the actor, and passes one
// that does not (design/DATABASE-SESSIONS.md §6): the opener's sign-in revoked, their level lowered,
// db_prod narrowed, prod_approval turned on under a prod write opened outside the browser, the database not
// ready, the lifetime past, production held under a prod write, and the idle window past with no statement
// running. Each closed row is the contract's.
func TestTheChecksCloseADatabaseSessionWithItsReason(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	a := appOf(t, base)
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'write'")
	open := "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"
	for _, c := range []struct {
		reason     string
		sub, email string
		body       string
		fail, undo []string
		cli        bool
	}{
		{"signed_out", "sub-carl", "carl@meridian.example", previewRead, []string{"UPDATE sessions SET revoked_at = now() WHERE id = (SELECT opened_by_session FROM db_sessions WHERE closed_at IS NULL)"}, nil, false},
		{"access_changed", "sub-carl", "carl@meridian.example", previewRead, []string{"UPDATE project_access SET level = 'viewer' WHERE grantee = 'carl@meridian.example'"},
			[]string{"UPDATE project_access SET level = 'contributor' WHERE grantee = 'carl@meridian.example'"}, false},
		{"access_changed", "sub-carl", "carl@meridian.example", prodRead, []string{"DELETE FROM project_access WHERE grantee = 'carl@meridian.example'"},
			[]string{"INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-olive', '')"}, false},
		{"setting_changed", "sub-carl", "carl@meridian.example", prodRead, []string{"UPDATE org_preferences SET db_prod = 'off'"}, []string{"UPDATE org_preferences SET db_prod = 'write'"}, false},
		{"setting_changed", "sub-olive", "olive@meridian.example", prodWrite, []string{"UPDATE org_preferences SET db_prod = 'read'"}, []string{"UPDATE org_preferences SET db_prod = 'write'"}, false},
		{"setting_changed", "sub-olive", "olive@meridian.example", prodWrite, []string{"UPDATE org_preferences SET prod_approval = true"}, []string{"UPDATE org_preferences SET prod_approval = false"}, true},
		{"database_gone", "sub-olive", "olive@meridian.example", previewRead, []string{"UPDATE resources SET state = 'failed' WHERE env = 'preview'"}, []string{"UPDATE resources SET state = 'ready'"}, false},
		{"expired", "sub-olive", "olive@meridian.example", previewRead, []string{"UPDATE db_sessions SET expires_at = now() - interval '1 second' WHERE closed_at IS NULL"}, nil, false},
		{"held", "sub-olive", "olive@meridian.example", prodWrite, []string{`INSERT INTO prod_holds (project_id, package_id, image, gate_run, reason, held_by_kind, held_by_subject, held_by_display, held_at)
			SELECT 'claims-portal', package_id, image, gate_run, 'Exports claimant SSNs unauthenticated (INC-2231)', 'person', 'sub-rhea', 'Rhea Okafor', now() FROM deploys WHERE env = 'prod'`}, []string{"DELETE FROM prod_holds"}, false},
		{"idle", "sub-olive", "olive@meridian.example", previewRead, []string{"UPDATE db_sessions SET last_activity_at = now() - interval '31 minutes' WHERE closed_at IS NULL"}, nil, false},
	} {
		c.body = strings.Replace(c.body, `"replace":true`, `"replace":false`, 1)
		who := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		if c.cli {
			who = fromCLI(t, ctx, pool, base, c.sub, c.email)
		}
		code, body, _ := do(t, who, http.MethodPost, base+dbSessions, c.body)
		if code != 201 {
			t.Fatalf("%s: the open: %d %s", c.reason, code, lastBody)
		}
		if err := a.checkDBSessions(ctx, time.Time{}, map[string]int{}); err != nil || count(t, ctx, pool, open) != 1 {
			t.Fatalf("%s: the checks closed a database session that passes them (%v)", c.reason, err)
		}
		runSQL(t, ctx, pool, c.fail...)
		if err := a.checkDBSessions(ctx, time.Time{}, map[string]int{}); err != nil {
			t.Fatalf("%s: %v", c.reason, err)
		}
		if n := count(t, ctx, pool, `SELECT count(*) FROM db_sessions s JOIN audit_events e ON e.detail->>'db_session' = s.id WHERE s.id = '`+body["id"].(string)+`'
			AND s.close_reason = '`+c.reason+`' AND e.action = 'db session closed' AND e.detail->>'reason' = '`+c.reason+`' AND e.actor_kind = 'canyon' AND e.actor_subject = 'db' AND e.via = 'canyon'`); n != 1 || count(t, ctx, pool, open) != 0 {
			t.Fatalf("%s: %d closed row(s) for it", c.reason, n)
		}
		closedShape(t, ctx, pool, body["id"].(string))
		runSQL(t, ctx, pool, c.undo...)
	}
}

// Asking the other mode is session_open until the request says replace, which closes the open one replaced
// and opens the new one in one transaction. A prod write is opened in the browser while prod_approval is
// on, and the CLI then attaches to it; with production held, neither opens nor attaches one.
func TestReplaceAndTheProdWriteRules(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	code, read, _ := do(t, olive, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("the read: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, olive, http.MethodPost, base+dbSessions, `{"env":"preview","mode":"write"}`); code != 409 || body["error"] != "session_open" || body["mode"] != "read" {
		t.Fatalf("the write without replace: %d %s", code, lastBody)
	}
	code, write, _ := do(t, olive, http.MethodPost, base+dbSessions, `{"env":"preview","mode":"write","replace":true}`)
	if code != 201 || write["mode"] != "write" || count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+read["id"].(string)+"' AND close_reason = 'replaced'") != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db write session opened' OR action = 'db session closed' AND detail->>'reason' = 'replaced' AND actor_subject = 'sub-olive'") != 2 {
		t.Fatalf("the replace: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'write', prod_approval = true")
	cli := fromCLI(t, ctx, pool, base, "sub-olive", "olive@meridian.example")
	if code, _, _ := do(t, cli, http.MethodPost, base+dbSessions, prodWrite); code != 403 || lastBody != `{"error":"dashboard_required","message":"open it in Canyon's dashboard"}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'dashboard_required' AND via = 'cli'") != 1 {
		t.Fatalf("a prod write from the CLI under prod_approval: %d %s", code, lastBody)
	}
	code, opened, _ := do(t, olive, http.MethodPost, base+dbSessions, prodWrite)
	if code != 201 {
		t.Fatalf("the prod write from the browser: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, cli, http.MethodPost, base+dbSessions, prodWrite); code != 200 || body["id"] != opened["id"] ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db session attached' AND via = 'cli' AND detail->>'mode' = 'write'") != 1 {
		t.Fatalf("the CLI's attach to it: %d %s", code, lastBody)
	}
	plantHold(t, ctx, pool)
	if code, body, _ := do(t, cli, http.MethodPost, base+dbSessions, prodWrite); code != 409 || body["error"] != "held" {
		t.Fatalf("an attach while held: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), http.MethodPost, base+dbSessions, prodRead); code != 201 {
		t.Fatalf("a prod read while held goes on, for the investigation: %d %s", code, lastBody)
	}
}

// Ctrl-C in connect ends a database session only while no connection of it is live: if_idle=true is 409
// in_use while the person's role has a backend, and ends it once none is left. Two closers racing write one
// closed row.
func TestIfIdleWaitsForALiveConnectionAndTwoClosersWriteOneRow(t *testing.T) {
	pool, ctx, base, olive, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("the open: %d %s", code, lastBody)
	}
	end := base + "/api/v1/db-sessions/" + opened["id"].(string)
	k := provisioners.Key{Project: "claims-portal", Env: "preview", Name: "db"}
	live := dial(t, ctx, b, k, provisioners.Person{Subject: "sub-carl", Email: "carl@meridian.example"}, provisioners.Read)
	if code, body, _ := do(t, carl, http.MethodDelete, end+"?if_idle=true", ""); code != 409 || body["error"] != "in_use" {
		t.Fatalf("if_idle with a live connection: %d %s", code, lastBody)
	}
	_ = live.Close(ctx)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if a, err := b.Activity(ctx, k, provisioners.Person{Subject: "sub-carl"}); err == nil && a.Live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed connection's backend still open 5s on")
		}
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, c := range []*http.Client{carl, olive} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- <-send(c, http.MethodDelete, end+"?if_idle=true", "")
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 204 {
			t.Errorf("a closer: %d", code)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db session closed'"); n != 1 {
		t.Fatalf("two closers wrote %d closed row(s)", n)
	}
}

// failingRevoke is the built-in driver whose Revoke fails, as one on a database that does not answer.
type failingRevoke struct{ *provisioners.Builtin }

func (failingRevoke) Revoke(context.Context, provisioners.Key, provisioners.Person) error {
	return errors.New("the apps database did not answer")
}

// A close whose revoke fails still closes, and marks the database session revoke_pending, as does one whose
// driver this replica does not run: only a database with no record has nothing to revoke. The sweep runs the
// revoke again, on any replica, until it succeeds, and then the role holds nothing.
func TestAFailedRevokeIsRunAgainByTheSweep(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	a := appOf(t, base)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	healthy := pipe.Drivers
	for _, c := range []struct {
		why     string
		drivers provisioners.Drivers
	}{
		{"a revoke that fails", provisioners.Drivers{{"postgres", "preview"}: failingRevoke{b}, {"postgres", "prod"}: failingRevoke{b}}}, // both: a resource finds its driver by name
		{"a driver this replica does not run", provisioners.Drivers{}},
	} {
		pipe.Drivers = healthy
		code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
		if code != 201 {
			t.Fatalf("%s: the open: %d %s", c.why, code, lastBody)
		}
		sid, role := opened["id"].(string), opened["role"].(string)
		pending := "SELECT count(*) FROM db_sessions WHERE id = '" + sid + "' AND close_reason = 'closed' AND revoke_pending"
		pipe.Drivers = c.drivers
		if code, _, _ := do(t, carl, http.MethodDelete, base+"/api/v1/db-sessions/"+sid, ""); code != 204 {
			t.Fatalf("%s: the end: %d %s", c.why, code, lastBody)
		}
		if groups, _ := roleHolds(t, ctx, role); count(t, ctx, pool, pending) != 1 || groups != 1 {
			t.Fatalf("%s: the close: pending %d, the role holds %d group(s)", c.why, count(t, ctx, pool, pending), groups)
		}
		if err := a.retryRevokes(ctx); err == nil || count(t, ctx, pool, pending) != 1 {
			t.Fatalf("%s: a retry that fails again: %v, pending %d", c.why, err, count(t, ctx, pool, pending))
		}
		pipe.Drivers = healthy
		if err := a.retryRevokes(ctx); err != nil || count(t, ctx, pool, pending) != 0 {
			t.Fatalf("%s: the retry: %v, pending %d", c.why, err, count(t, ctx, pool, pending))
		}
		if groups, login := roleHolds(t, ctx, role); groups != 0 || login {
			t.Fatalf("%s: after the retry the role holds %d group(s), logs in: %v", c.why, groups, login)
		}
	}
}

// A revoke still pending runs before the person's next open on the key, and is never skipped: a backend that
// did SET ROLE to the write group outlives a new read grant, so a demotion to read leaves no writer behind.
// While the revoke cannot run, the open is refused 502 and writes nothing.
func TestAPendingRevokeRunsBeforeTheNextOpen(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, `{"env":"preview","mode":"write"}`)
	if code != 201 {
		t.Fatalf("the write: %d %s", code, lastBody)
	}
	k, carlP := provisioners.Key{Project: "claims-portal", Env: "preview", Name: "db"}, provisioners.Person{Subject: "sub-carl", Email: "carl@meridian.example"}
	writer := dial(t, ctx, b, k, carlP, provisioners.Write)
	var group string
	if err := writer.QueryRow(ctx, `SELECT b.rolname FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = current_user)`).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, "SET ROLE "+pgx.Identifier{group}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	healthy := pipe.Drivers
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: failingRevoke{b}, {"postgres", "prod"}: failingRevoke{b}}
	if code, _, _ := do(t, carl, http.MethodDelete, base+"/api/v1/db-sessions/"+opened["id"].(string), ""); code != 204 {
		t.Fatalf("the end: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 502 || body["error"] != "database_unreachable" || count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL") != 0 {
		t.Fatalf("an open while its revoke cannot run: %d %s", code, lastBody)
	}
	pipe.Drivers = healthy
	if code, _, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 201 || count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE revoke_pending") != 0 {
		t.Fatalf("the open once it can: %d %s", code, lastBody)
	}
	if err := writer.Ping(ctx); err == nil {
		t.Fatal("the old write backend outlived the read open")
	}
}

// dial issues person a login on the key in mode through b and connects with it, as a relay would: the
// connection is closed when the test ends.
func dial(t *testing.T, ctx context.Context, b provisioners.Issuer, k provisioners.Key, p provisioners.Person, mode provisioners.Mode) *pgx.Conn {
	t.Helper()
	var c *pgx.Conn
	if err := b.Issue(ctx, k, p, mode, time.Minute, func(ctx context.Context, l provisioners.Login) error {
		u, err := neturl.Parse(pgtest.URL(t)) // the test server's TLS, as the login's
		if err == nil {
			u.User, u.Host, u.Path = neturl.UserPassword(l.User, l.Secret.Reveal()), net.JoinHostPort(l.Host, strconv.Itoa(l.Port)), "/"+l.Database
			c, err = pgx.Connect(ctx, u.String())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.WithoutCancel(ctx)) })
	return c
}

// With sign-in off the anonymous admin opens a preview database session, which names no address; a
// stored db_prod of write is read off there, so production is refused (design/DATABASE-SESSIONS.md §5).
func TestWithSignInOffTheAnonymousAdminOpensPreviewOnly(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", ""); code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	dbReady(t, ctx, pool)
	code, opened, _ := do(t, c, http.MethodPost, base+dbSessions, previewRead)
	if person, _ := opened["person"].(map[string]any); code != 201 || person["subject"] != anonymousSubject || person["email"] != "" || opened["yours"] != true {
		t.Fatalf("the anonymous admin's open: %d %s", code, lastBody)
	}
	dbShape(t, "db_session", lastBody)
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'write'")
	if code, body, _ := do(t, c, http.MethodPost, base+dbSessions, prodRead); code != 409 || body["error"] != "db_prod_off" {
		t.Fatalf("prod with sign-in off: %d %s", code, lastBody)
	}
}

// Ends of one database session as many at once as the server's pool holds connections all answer: the one
// that closes it revokes on its own transaction while the rest wait on the person role's lock, and never
// waits for a connection they hold.
func TestEndsAsManyAsThePoolAllAnswer(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("the open: %d %s", code, lastBody)
	}
	n := 2 * int(poolOf(t, base).Config().MaxConns)
	c := &http.Client{Jar: olive.Jar, Timeout: 20 * time.Second}
	codes := make([]<-chan int, n)
	for i := range codes {
		codes[i] = send(c, http.MethodDelete, base+"/api/v1/db-sessions/"+opened["id"].(string), "")
	}
	for i, answered := range codes {
		if code := <-answered; code != 204 {
			t.Errorf("end %d of %d: %d", i+1, n, code)
		}
	}
	if closed := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db session closed'"); closed != 1 {
		t.Fatalf("%d ends wrote %d closed row(s)", n, closed)
	}
}

// The sweep forgets the role of a person with no database session on the database for 30 days, judged under
// their role's lock: one with a session open there keeps theirs, and so does one who opened again since an
// old one closed.
func TestTheSweepForgetsOnlyARetiredRole(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	carl, dana := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	roles := map[string]string{}
	for who, c := range map[string]*http.Client{"carl": carl, "dana": dana, "olive": olive} {
		code, opened, _ := do(t, c, http.MethodPost, base+dbSessions, previewRead)
		if code != 201 {
			t.Fatalf("%s's open: %d %s", who, code, lastBody)
		}
		roles[who] = opened["role"].(string)
		if who != "olive" {
			if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/db-sessions/"+opened["id"].(string), ""); code != 204 {
				t.Fatalf("%s's end: %d %s", who, code, lastBody)
			}
		}
	}
	runSQL(t, ctx, pool, "UPDATE db_sessions SET opened_at = now() - interval '32 days', closed_at = now() - interval '31 days' WHERE closed_at IS NOT NULL")
	if code, _, _ := do(t, dana, http.MethodPost, base+dbSessions, previewRead); code != 201 {
		t.Fatalf("dana's open since: %d %s", code, lastBody)
	}
	if err := appOf(t, base).forgetRetired(ctx); err != nil {
		t.Fatal(err)
	}
	for who, kept := range map[string]bool{"carl": false, "dana": true, "olive": true} {
		if exists := roleExists(t, ctx, roles[who]); exists != kept {
			t.Errorf("%s's role %s after the sweep: there %v, want %v", who, roles[who], exists, kept)
		}
	}
}

// roleExists is whether the test server has the role role.
func roleExists(t *testing.T, ctx context.Context, role string) bool {
	t.Helper()
	server, err := pgx.Connect(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close(ctx) }()
	var exists bool
	if err := server.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// The checks fail closed: an expired database session closes expired whatever else cannot be judged, and
// one whose checks cannot be judged dbCheckTries sweeps in a row closes with the reason of the check that
// failed; errors between sweeps that judge it, however many, close nothing.
func TestTheChecksFailClosed(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	a := appOf(t, base)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	ids := map[string]string{}
	for who, c := range map[string]*http.Client{"carl": carl, "olive": olive} {
		code, opened, _ := do(t, c, http.MethodPost, base+dbSessions, previewRead)
		if code != 201 {
			t.Fatalf("%s's open: %d %s", who, code, lastBody)
		}
		ids[who] = opened["id"].(string)
	}
	runSQL(t, ctx, pool, "UPDATE db_sessions SET expires_at = now() - interval '1 second' WHERE id = '"+ids["carl"]+"'", "ALTER TABLE resources RENAME TO resources_away")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), "ALTER TABLE IF EXISTS resources_away RENAME TO resources")
	})
	closed := func(who string) string {
		var reason *string
		if err := pool.QueryRow(ctx, "SELECT close_reason FROM db_sessions WHERE id = $1", ids[who]).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if reason == nil {
			return ""
		}
		return *reason
	}
	failing := map[string]int{}
	sweep := func(away bool) {
		t.Helper()
		if err := a.checkDBSessions(ctx, time.Time{}, failing); (err != nil) != away {
			t.Fatalf("a sweep with the resources table away %v: %v", away, err)
		}
	}
	sweep(true)
	if closed("carl") != "expired" || closed("olive") != "" {
		t.Fatalf("the first sweep: carl's %q, want expired; olive's %q, want open", closed("carl"), closed("olive"))
	}
	for range 2 * dbCheckTries { // errors between judged sweeps are no run of them
		runSQL(t, ctx, pool, "ALTER TABLE resources_away RENAME TO resources")
		sweep(false)
		runSQL(t, ctx, pool, "ALTER TABLE resources RENAME TO resources_away")
		sweep(true)
	}
	if closed("olive") != "" || len(failing) != 1 {
		t.Fatalf("errors between judged sweeps closed olive's %q, or failing holds %v", closed("olive"), failing)
	}
	for range dbCheckTries - 2 {
		sweep(true)
	}
	if closed("olive") != "" {
		t.Fatalf("%d errors in a row closed olive's %q", dbCheckTries-1, closed("olive"))
	}
	sweep(true)
	if closed("olive") != "database_gone" || len(failing) != 0 {
		t.Fatalf("%d errors in a row: olive's %q, want database_gone; failing holds %v", dbCheckTries, closed("olive"), failing)
	}
}

// A person opens at most ten database sessions a minute, counted from the rows every replica writes: the
// eleventh is 429 busy, judged before its login, so it adds no row and the role holds no group.
func TestTheEleventhOpenInAMinuteIsBusy(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	role := ""
	for i := range dbOpensAMinute {
		code, opened, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
		if code != 201 {
			t.Fatalf("open %d: %d %s", i+1, code, lastBody)
		}
		role = opened["role"].(string)
		if code, _, _ := do(t, carl, http.MethodDelete, base+"/api/v1/db-sessions/"+opened["id"].(string), ""); code != 204 {
			t.Fatalf("end %d: %d %s", i+1, code, lastBody)
		}
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 429 || body["error"] != "busy" {
		t.Fatalf("the eleventh open: %d %s", code, lastBody)
	}
	dbShape(t, "refusal", `{"status":429,"body":`+lastBody+`}`)
	if groups, _ := roleHolds(t, ctx, role); count(t, ctx, pool, "SELECT count(*) FROM db_sessions") != dbOpensAMinute || groups != 0 {
		t.Fatalf("the busy open wrote a row or left the role %d group(s)", groups)
	}
}

// notAsMadeFound is a refusal of a database the login reached, as the built-in driver words it (contracts/routes.md,
// not_as_made), and loginRefusals what the open, the query and the connect say for each such refusal of
// claims-portal's preview database (#2910): never "couldn't reach … try again", which can't work.
const notAsMadeFound = "your role p_0123456789abcdef01234567 is also a member of c_0123456789abcdef01234567_write, by a grant Canyon can't revoke"

var loginRefusals = map[string]string{
	"not_as_made":        "Canyon didn't log you in to the database of claims-portal in preview: " + notAsMadeFound + ". Ask an admin of this app's database to remove that, then try again.",
	"database_not_ready": "The database db of claims-portal in preview isn't ready: deploy claims-portal to preview again.",
}

// refusing is the built-in driver whose Issue answers err, as a database that refuses a session's limits,
// keeps public open to CREATE, or does not answer.
type refusing struct {
	*provisioners.Builtin
	err error
}

func (r refusing) Issue(context.Context, provisioners.Key, provisioners.Person, provisioners.Mode, time.Duration, func(context.Context, provisioners.Login) error) error {
	return r.err
}

// adapterOnly is a platform's adapter that issues no logins to people: a Driver, never an Issuer.
type adapterOnly struct{ provisioners.Driver }

func (adapterOnly) Name() string { return "https://provision.meridian.example/canyon" }

// Each refusal the open answers past the caller's own is the contract's, body and all: an app that is not
// there, a database the app does not declare or does not name among two, one not ready, one an adapter made,
// and the login's own — public open to CREATE, limits the database refuses, a database that does not answer.
func TestTheOpensRefusalsAreTheContracts(t *testing.T) {
	pool, ctx, base, olive, b := dbHost(t)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	refused := func(why string, c *http.Client, path, body string, status int, code string) {
		t.Helper()
		got, answer, _ := do(t, c, http.MethodPost, base+path, body)
		if got != status || answer["error"] != code {
			t.Errorf("%s: %d %s, want %d %s", why, got, lastBody, status, code)
			return
		}
		dbShape(t, "refusal", `{"status":`+strconv.Itoa(status)+`,"body":`+lastBody+`}`)
	}
	refused("an app that is not there, to an admin", dana, "/api/v1/projects/nope/db-sessions", previewRead, 404, "not_found")
	refused("an app that is not there, to a member", olive, "/api/v1/projects/nope/db-sessions", previewRead, 404, "not_found")
	refused("a database it does not declare", olive, dbSessions, `{"env":"preview","mode":"read","resource":"events"}`, 409, "no_database")
	runSQL(t, ctx, pool, `UPDATE packages SET resources = '{"db": "postgres", "events": "postgres"}'`)
	refused("none named of two", olive, dbSessions, previewRead, 409, "which_database")
	if lastBody != `{"error":"which_database","message":"claims-portal declares more than one database: name one as its resource","resources":["db","events"]}` {
		t.Errorf("which_database names them: %s", lastBody)
	}
	refused("a declared one with no record", olive, dbSessions, `{"env":"preview","mode":"read","resource":"events"}`, 409, "database_not_ready")
	runSQL(t, ctx, pool, `UPDATE packages SET resources = '{"db": "postgres"}'`, "UPDATE resources SET state = 'pending' WHERE env = 'preview'")
	refused("one not ready", olive, dbSessions, previewRead, 409, "database_not_ready")
	runSQL(t, ctx, pool, "UPDATE resources SET state = 'ready'", "UPDATE resources SET driver = 'https://provision.meridian.example/canyon' WHERE env = 'prod'")
	healthy := pipe.Drivers
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: b, {"postgres", "prod"}: adapterOnly{}}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'read'")
	refused("one an adapter made", olive, dbSessions, prodRead, 409, "driver_unsupported")
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{{provisioners.ErrUnsafeSchema, 409, "unsafe_schema"}, {fmt.Errorf("builtin postgres: issue: %w: temp_file_limit", provisioners.ErrLimitsUnsupported), 409, "limits_unsupported"},
		{&provisioners.NotAsMade{Found: notAsMadeFound}, 409, "not_as_made"},
		{fmt.Errorf("builtin postgres: issue %s: the database of it: %w", "x", provisioners.ErrNoDatabase), 409, "database_not_ready"},
		{errors.New("dial tcp: connection refused"), 502, "database_unreachable"}} {
		pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: refusing{b, c.err}, {"postgres", "prod"}: refusing{b, c.err}}
		refused(c.err.Error(), olive, dbSessions, previewRead, c.status, c.code)
		if c.code == "unsafe_schema" && !strings.Contains(lastBody, `"message":"PUBLIC can still create in schema public, which only its owner or a superuser can revoke. Ask an admin of this app's database to run REVOKE CREATE ON SCHEMA public FROM PUBLIC, then try again."`) {
			t.Errorf("the open's unsafe_schema says %s", lastBody) // contracts/routes.md, the db-sessions row (#2826)
		}
		if said := loginRefusals[c.code]; said != "" && !strings.Contains(lastBody, `"message":"`+said+`"`) {
			t.Errorf("the open's %s says %s, want %s", c.code, lastBody, said) // contracts/routes.md, the db-sessions row (#2910)
		}
		// the query opens through the same path, so it refuses the same way, never 502 for a database it reached
		refused("a query: "+c.err.Error(), olive, "/api/v1/projects/claims-portal/db/query", `{"env":"preview","mode":"read","sql":"select 1"}`, c.status, c.code)
	}
	pipe.Drivers = healthy
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied' AND action LIKE 'db %'"); n != 0 {
		t.Fatalf("the refusals wrote %d row(s)", n)
	}
}

// One replica at a time holds the sweep's lease, a session-level lock on Canyon's database: a second is
// refused it until the first lets it go.
func TestOneReplicaAtATimeHoldsTheSweepsLease(t *testing.T) {
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	a, logger := appOf(t, base), log.New(logs, "", 0)
	first := a.takeLease(ctx, logger)
	if first == nil {
		t.Fatal("no replica holds the lease")
	}
	if second := a.takeLease(ctx, logger); second != nil {
		_ = second.Close(ctx)
		t.Fatal("a second replica holds the lease beside the first")
	}
	_ = first.Close(ctx)
	if third := a.takeLease(ctx, logger); third == nil {
		t.Fatal("no replica takes the lease once the first let it go")
	} else {
		_ = third.Close(ctx)
	}
}

// A statement still running is activity, however long since the last: a database session past its idle
// window whose role runs one is not closed, and its last activity moves to now.
func TestARunningStatementIsNotIdle(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, _, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 201 {
		t.Fatalf("the open: %d %s", code, lastBody)
	}
	k := provisioners.Key{Project: "claims-portal", Env: "preview", Name: "db"}
	report := dial(t, ctx, b, k, provisioners.Person{Subject: "sub-carl", Email: "carl@meridian.example"}, provisioners.Read)
	done := make(chan error, 1)
	go func() { _, err := report.Exec(ctx, "SELECT pg_sleep(3)"); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if activity, err := b.Activity(ctx, k, provisioners.Person{Subject: "sub-carl"}); err == nil && activity.Active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the report never ran")
		}
	}
	runSQL(t, ctx, pool, "UPDATE db_sessions SET last_activity_at = now() - interval '31 minutes'")
	if err := appOf(t, base).checkDBSessions(ctx, time.Time{}, map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL AND last_activity_at > now() - interval '1 minute'"); n != 1 {
		t.Fatalf("a running report was judged idle: %d open and active", n)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// An admin passes the level a prod write needs and nothing else: db_prod, the reason and the browser rule
// bind them as anyone (design/PERMISSIONS.md, Database sessions).
func TestAnAdminsProdWriteIsBoundAsAnyones(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	cli := fromCLI(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	for _, c := range []struct {
		why, settings string
		c             *http.Client
		body          string
		status        int
		code          string
	}{
		{"with db_prod off", "db_prod = 'off'", dana, prodWrite, 409, "db_prod_off"},
		{"with db_prod read", "db_prod = 'read'", dana, prodWrite, 409, "db_prod_read_only"},
		{"with no reason", "db_prod = 'write'", dana, `{"env":"prod","mode":"write"}`, 400, "reason_required"},
		{"from the CLI under prod_approval", "db_prod = 'write', prod_approval = true", cli, prodWrite, 403, "dashboard_required"},
	} {
		runSQL(t, ctx, pool, "UPDATE org_preferences SET "+c.settings)
		if code, body, _ := do(t, c.c, http.MethodPost, base+dbSessions, c.body); code != c.status || body["error"] != c.code {
			t.Errorf("an admin's prod write %s: %d %s", c.why, code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+dbSessions, prodWrite); code != 201 {
		t.Fatalf("an admin's prod write from the browser: %d %s", code, lastBody)
	}
}

// Database sessions as a whole are an admin's switch (#2835): off from any of an admin's sessions, which closes every
// open database session turned_off and ends its relays; while off, an open, a query and a connect are 409
// db_sessions_off, nothing issued; on again from a browser alone, never from the CLI.
func TestDBSessionsOff(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	danaCLI := fromCLI(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	var signin, role string
	if err := pool.QueryRow(ctx, "SELECT id FROM sessions WHERE actor_subject = 'sub-carl' ORDER BY created_at DESC LIMIT 1").Scan(&signin); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT role FROM db_sessions WHERE id = $1", sid).Scan(&role); err != nil {
		t.Fatal(err)
	}
	a := appOf(t, base)
	ended := ""
	relay := &dbRelay{sid: sid, subject: "sub-carl", signin: signin}
	relay.endFn = func(_ websocket.StatusCode, reason, _ string, _ bool) { ended = reason }
	if a.relays.admit(relay, 100) != "" {
		t.Fatal("admit")
	}
	t.Cleanup(func() { a.relays.release(relay) })

	if code, body, _ := do(t, danaCLI, http.MethodPut, base+"/api/v1/preferences", `{"db_sessions":"off"}`); code != 200 || body["db_sessions"] != "off" {
		t.Fatalf("an admin turns database sessions off from the CLI: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+sid+"' AND close_reason = 'turned_off'"); n != 1 || ended != "turned_off" {
		t.Errorf("turned off: carl's database session closed turned_off %d time(s), its relay ended %q", n, ended)
	}

	server, err := pgx.Connect(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close(ctx) }()
	issued := func() (until *time.Time) {
		if err := server.QueryRow(ctx, "SELECT rolvaliduntil FROM pg_catalog.pg_roles WHERE rolname = $1", role).Scan(&until); err != nil {
			t.Fatal(err)
		}
		return until
	}
	before, rows := issued(), count(t, ctx, pool, "SELECT count(*) FROM db_sessions")
	connect := base + "/api/v1/db-sessions/" + sid + "/connect?startup=" + base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	for what, req := range map[string][3]string{"an open": {http.MethodPost, base + dbSessions, previewRead},
		"a query": {http.MethodPost, base + dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`}, "a connect": {http.MethodGet, connect, ""}} {
		if code, body, _ := do(t, carl, req[0], req[1], req[2]); code != 409 || body["error"] != "db_sessions_off" || body["message"] != dbSessionsOff {
			t.Errorf("%s while database sessions are off: %d %s", what, code, lastBody)
		}
		dbShape(t, "refusal", `{"status":409,"body":`+lastBody+`}`)
	}
	if after := issued(); !equalTimes(before, after) || count(t, ctx, pool, "SELECT count(*) FROM db_sessions") != rows {
		t.Errorf("a refusal while off issued a login or wrote a row: valid until %v, was %v", after, before)
	}

	if code, body, _ := do(t, danaCLI, http.MethodPut, base+"/api/v1/preferences", `{"db_sessions":"on"}`); code != 403 || body["error"] != "dashboard_required" {
		t.Errorf("an admin turns database sessions on from the CLI: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+"/api/v1/preferences", `{"db_sessions":"on"}`); code != 200 || body["db_sessions"] != "on" {
		t.Fatalf("an admin turns database sessions on from a browser: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 201 {
		t.Errorf("an open once database sessions are on again: %d %s", code, lastBody)
	}
}

// racing is the built-in driver whose Issue calls first before the login: what happens between an open's read of
// the switch and its login.
type racing struct {
	*provisioners.Builtin
	first func()
}

func (r racing) Issue(ctx context.Context, k provisioners.Key, p provisioners.Person, m provisioners.Mode, ttl time.Duration, connect func(context.Context, provisioners.Login) error) error {
	r.first()
	return r.Builtin.Issue(ctx, k, p, m, ttl, connect)
}

// An open whose login races an admin turning database sessions off ends with the switch (#2864): the turn-off
// commits while the open is issuing its login, and the admin's PUT answers only once that database session is
// closed turned_off, never leaving it to the sweep.
func TestAnOpenRacingTheSwitchEndsWithIt(t *testing.T) {
	pool, ctx, base, olive, b := dbHost(t)
	danaCLI := fromCLI(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	put := make(chan string, 1)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: racing{b, func() {
		go func() {
			req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/preferences", strings.NewReader(`{"db_sessions":"off"}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := danaCLI.Do(req)
			if err != nil {
				put <- err.Error()
				return
			}
			_ = resp.Body.Close()
			put <- resp.Status
		}()
		for off := ""; off != "off"; time.Sleep(10 * time.Millisecond) { // the turn-off committed, its PUT not yet answered
			if err := pool.QueryRow(ctx, "SELECT db_sessions FROM org_preferences").Scan(&off); err != nil {
				t.Error(err)
				return
			}
		}
	}}}
	code, body, _ := do(t, olive, http.MethodPost, base+dbSessions, previewRead)
	if status := <-put; status != "200 OK" {
		t.Fatalf("the admin's turn-off: %s", status)
	}
	if code != 201 {
		t.Fatalf("the racing open: %d %s", code, lastBody)
	}
	if reason := ""; pool.QueryRow(ctx, "SELECT coalesce(close_reason, '') FROM db_sessions WHERE id = $1", body["id"]).Scan(&reason) != nil || reason != "turned_off" {
		t.Errorf("the racing open's database session, once the turn-off answered: close_reason %q, want turned_off", reason)
	}
}

// An open that passed the switch's first read, then waits on its lock while the switch goes off, is refused (#2864):
// turn-off ran to its end before the open's transaction began, so only the open's second read, under the lock, stops
// it. 409 db_sessions_off, no login issued, no row written.
func TestAnOpenReadsTheSwitchAgainUnderItsLock(t *testing.T) {
	pool, ctx, base, olive, b := dbHost(t)
	var issued atomic.Bool
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: racing{b, func() { issued.Store(true) }}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", dbSwitch); err != nil {
		t.Fatal(err)
	}
	answer := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, base+dbSessions, strings.NewReader(previewRead))
		req.Header.Set("Content-Type", "application/json")
		resp, err := olive.Do(req)
		if err != nil {
			answer <- err.Error()
			return
		}
		said, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answer <- strconv.Itoa(resp.StatusCode) + " " + strings.TrimSpace(string(said))
	}()
	for count(t, ctx, pool, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted") == 0 { // the open waits on the switch
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, "UPDATE org_preferences SET db_sessions = 'off'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil { // the switch off, and its lock let go
		t.Fatal(err)
	}
	if got, want := <-answer, `409 {"error":"db_sessions_off","message":"`+dbSessionsOff+`"}`; got != want {
		t.Errorf("the open waiting on the switch: %s, want %s", got, want)
	}
	if issued.Load() || count(t, ctx, pool, "SELECT count(*) FROM db_sessions") != 0 {
		t.Errorf("the refused open issued a login (%v) or wrote a row", issued.Load())
	}
}

// equalTimes is whether two nullable times are the same instant, or both null.
func equalTimes(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}
