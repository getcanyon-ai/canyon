package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/provisioners"
)

const dbQuery = "/api/v1/projects/claims-portal/db/query"

// inAppDB runs sqls in claims-portal's database in env as the test server's superuser: what the app made, there
// before any person queries it. The database is named by a login issued for a setup person, which connects nowhere.
func inAppDB(t *testing.T, ctx context.Context, b provisioners.Issuer, env string, sqls ...string) {
	t.Helper()
	name := ""
	k := provisioners.Key{Project: "claims-portal", Env: env, Name: "db"}
	if err := b.Issue(ctx, k, provisioners.Person{Subject: "sub-setup", Email: "setup@meridian.example"}, provisioners.Read, time.Minute,
		func(_ context.Context, l provisioners.Login) error { name = l.Database; return nil }); err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	for _, sql := range sqls {
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// claimsTable is the app's table of claims, three of them, which anyone may read: what a query reads.
func claimsTable(t *testing.T, ctx context.Context, b provisioners.Issuer, env string) {
	t.Helper()
	inAppDB(t, ctx, b, env, "CREATE TABLE claims (status text, amount bigint)",
		"INSERT INTO claims VALUES ('open', 41), ('approved', 118), ('denied', NULL)",
		"CREATE TABLE ledger (n int)", "GRANT SELECT ON claims, ledger TO PUBLIC", "GRANT INSERT ON ledger TO PUBLIC")
}

// A query answers as the contract's answer: the rows as Postgres's text, NULL as null, each column named by
// format_type, its command, and the database session it ran in, whose counters count it. A second query attaches
// to the same session, and writes no row of the trail; the limit cuts the rows and says so. In write mode it says
// what it changed. The open's row says via query (contracts/routes.md, the trail).
func TestAQueryAnswersTheContract(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select status, amount from claims order by status"}`)
	if code != 200 {
		t.Fatalf("the query: %d %s", code, lastBody)
	}
	if err := shapeOf("db-query", "answer", lastBody); err != nil {
		t.Errorf("the answer is not the contract's: %v\n%s", err, lastBody)
	}
	want := `"columns":[{"name":"status","type":"text"},{"name":"amount","type":"bigint"}],"command":"SELECT"`
	if !strings.Contains(lastBody, want) || !strings.Contains(lastBody, `"row_count":3,"rows":[["approved","118"],["denied",null],["open","41"]],"truncated":false`) ||
		strings.Contains(lastBody, "rows_affected") {
		t.Errorf("the answer: %s", lastBody)
	}
	session := body["db_session"].(map[string]any)
	if session["queries"] != 1.0 || session["connections"] != 1.0 || session["bytes_from_db"].(float64) <= 0 || session["bytes_to_db"].(float64) <= 0 {
		t.Errorf("the session's counters: %v", session)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'db session opened' AND detail->>'via_tool' = 'query'"); n != 1 {
		t.Errorf("%d open rows via query, want 1", n)
	}
	code, again, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select status from claims order by status","limit":1}`)
	if code != 200 || again["truncated"] != true || again["row_count"] != 1.0 || again["db_session"].(map[string]any)["id"] != session["id"] ||
		again["db_session"].(map[string]any)["queries"] != 2.0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action LIKE 'db %'") != 1 {
		t.Errorf("a second query, cut at 1 row: %d %s", code, lastBody)
	}
	// the connection is the session's own, as dbConnect names it for the database's activity and logs
	if code, named, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select current_setting('application_name')"}`); code != 200 ||
		named["rows"].([]any)[0].([]any)[0] != "canyon:"+session["id"].(string)+":query" {
		t.Errorf("the connection's application_name: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"insert into ledger values (7)","replace":true}`); code != 200 ||
		body["command"] != "INSERT" || body["rows_affected"] != 1.0 {
		t.Errorf("a write: %d %s", code, lastBody)
	}
}

// A read query refuses while the person's role may EXECUTE any SECURITY DEFINER function outside the system schemas,
// whatever it calls (#2920's review): it runs as its owner, and what that owner may reach, a second connection or
// another definer, Canyon doesn't chase. One that calls nothing is named too; revoked from PUBLIC, the query goes
// through. That a read query's write fails in its READ ONLY transaction is the write session's test's.
func TestAReadQueryRefusesAnyDefinerItMayRun(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	inAppDB(t, ctx, b, "preview", "CREATE FUNCTION greet() RETURNS text LANGUAGE sql SECURITY DEFINER AS 'SELECT ''hello'''",
		"GRANT EXECUTE ON FUNCTION greet() TO PUBLIC")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, refusal, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`)
	if msg, _ := refusal["message"].(string); code != 409 || refusal["error"] != "read_only_unsafe" ||
		msg != "Canyon didn't open a read connection: anyone connecting could still change data here (EXECUTE on SECURITY DEFINER function public.greet()). Ask an admin of this app's database to revoke those rights, then connect again." {
		t.Fatalf("a read query a harmless definer is open to: %d %s", code, lastBody)
	}
	inAppDB(t, ctx, b, "preview", "REVOKE EXECUTE ON FUNCTION greet() FROM PUBLIC")
	if code, _, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`); code != 200 {
		t.Errorf("the definer revoked from PUBLIC: %d %s", code, lastBody)
	}
}

// A read query runs in the person's open write session, read-only, and replaces nothing (contracts/routes.md, Attach,
// don't replace): it sees the write's row, can't write one itself, and the session stays write for the next write.
func TestAReadQueryRunsInAnOpenWriteSessionReadOnly(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, wrote, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"insert into ledger values (1)"}`)
	if code != 200 {
		t.Fatalf("the write: %d %s", code, lastBody)
	}
	sid := wrote["db_session"].(map[string]any)["id"]
	code, read, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select count(*) from ledger"}`)
	if rows, _ := read["rows"].([]any); code != 200 || len(rows) != 1 || rows[0].([]any)[0] != "1" || read["db_session"].(map[string]any)["id"] != sid {
		t.Fatalf("a read in the write session: %d %s", code, lastBody)
	}
	if at := wrote["db_session"].(map[string]any)["expires_at"]; at == nil || read["db_session"].(map[string]any)["expires_at"] != at {
		t.Fatalf("a read in the write session moved its end: %v, then %v", at, read["db_session"].(map[string]any)["expires_at"])
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"insert into ledger values (2)"}`); code != 422 || body["sqlstate"] != "25006" {
		t.Fatalf("a read query that writes, in the write session: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"insert into ledger values (3)"}`); code != 200 || body["db_session"].(map[string]any)["id"] != sid ||
		count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE close_reason IS NOT NULL") != 0 {
		t.Fatalf("the write after the reads: %d %s", code, lastBody)
	}
}

// A write query switches the person's own read session on its own while no client is connected through it, and never
// while one is (#2843's review): then it is session_open, the read session left open, until the request says replace.
func TestAWriteQueryReplacesAReadSessionOnlyWhileNothingIsConnected(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, read, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`)
	if code != 200 {
		t.Fatalf("the read: %d %s", code, lastBody)
	}
	sid := read["db_session"].(map[string]any)["id"].(string)
	k := provisioners.Key{Project: "claims-portal", Env: "preview", Name: "db"}
	live := dial(t, ctx, b, k, provisioners.Person{Subject: "sub-carl", Email: "carl@meridian.example"}, provisioners.Read)
	write := `{"env":"preview","mode":"write","sql":"insert into ledger values (1)"}`
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, write); code != 409 || body["error"] != "session_open" || body["mode"] != "read" ||
		count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+sid+"' AND close_reason IS NULL") != 1 {
		t.Fatalf("a write while a client is connected through the read: %d %s", code, lastBody)
	}
	_ = live.Close(ctx)
	code, wrote, _ := do(t, carl, http.MethodPost, base+dbQuery, write)
	if code != 200 || wrote["db_session"].(map[string]any)["mode"] != "write" ||
		count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+sid+"' AND close_reason = 'replaced'") != 1 {
		t.Fatalf("a write once nothing is connected: %d %s", code, lastBody)
	}
}

// One statement a call: more than one is the database's refusal at Parse, 400 invalid_sql in its words, and none
// of them runs (the issue's red: send it on the simple protocol and both run). A syntax error is refused there too.
func TestTwoStatementsAreRefused(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"insert into ledger values (1); insert into ledger values (2)"}`)
	if code != 400 || body["error"] != "invalid_sql" || body["message"] == "" {
		t.Fatalf("two statements: %d %s", code, lastBody)
	}
	if err := shapeOf("db-query", "refusal", lastBody); err != nil {
		t.Errorf("the refusal is not the contract's: %v", err)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"selec 1"}`); code != 400 || body["error"] != "invalid_sql" {
		t.Errorf("a syntax error: %d %s", code, lastBody)
	}
	code, read, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"select count(*) from ledger"}`)
	if rows, _ := read["rows"].([]any); code != 200 || len(rows) != 1 || rows[0].([]any)[0] != "0" {
		t.Fatalf("the ledger after them: %d %s", code, lastBody)
	}
}

// A body the query can't run is refused before anything opens: an empty statement, one over 64 KiB, a limit or a
// timeout out of bounds, in the contract's words; no database session, no login.
func TestTheQuerysBoundsAreJudgedBeforeItOpens(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	long := `"` + strings.Repeat("x", 64<<10+1) + `"`
	for body, want := range map[string]string{
		`{"env":"preview","mode":"read","sql":""}`:                           "invalid_sql",
		`{"env":"preview","mode":"read","sql":` + long + `}`:                 "invalid_sql",
		`{"env":"preview","mode":"read","sql":"select 1","limit":0}`:         "invalid_limit",
		`{"env":"preview","mode":"read","sql":"select 1","limit":10001}`:     "invalid_limit",
		`{"env":"preview","mode":"read","sql":"select 1","timeout":"500ms"}`: "invalid_timeout",
		`{"env":"preview","mode":"read","sql":"select 1","timeout":"6m"}`:    "invalid_timeout",
		`{"env":"preview","mode":"read","sql":"select 1","timeout":"soon"}`:  "invalid_timeout",
	} {
		if code, got, _ := do(t, carl, http.MethodPost, base+dbQuery, body); code != 400 || got["error"] != want {
			t.Errorf("%.80s: %d %s, want 400 %s", body, code, lastBody, want)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions"); n != 0 {
		t.Errorf("a refused body opened %d database sessions", n)
	}
}

// A statement past its timeout is 422 statement_timeout; one that fails as it runs is 422 query_failed with its
// SQLSTATE; one that names a table that isn't there is refused at Parse, where Postgres analyses it: 400 invalid_sql.
// The counters count all three.
func TestAStatementThatFailsIs422AndCounted(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select pg_sleep(3)","timeout":"1s"}`); code != 422 || body["error"] != "statement_timeout" {
		t.Errorf("a statement past its timeout: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1/0"}`); code != 422 || body["error"] != "query_failed" || body["sqlstate"] != "22012" {
		t.Errorf("a division by zero: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select * from nothing_here"}`); code != 400 || body["error"] != "invalid_sql" {
		t.Errorf("a missing table: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT queries FROM db_sessions WHERE closed_at IS NULL"); n != 3 {
		t.Errorf("the failed statements counted %d queries, want 3", n)
	}
}

// A row over the 10 MiB cap on an answer is refused before the server holds it (#2813's review): the connection
// reads no message larger than the cap, so `repeat('x', 1000000000)` never puts a gigabyte in the server's memory.
// It is 422 query_failed with Postgres's program_limit_exceeded, saying the cap in plain words, never a 502, and the
// counters count it. A row under the cap still comes back whole.
func TestARowOverTheAnswersCapIsRefusedBeforeItIsHeld(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select repeat('x', 11 * 1024 * 1024)"}`)
	if code != 422 || body["error"] != "query_failed" || body["sqlstate"] != "54000" || !strings.Contains(lastBody, "10 MiB") {
		t.Fatalf("an 11 MiB row: %d %.300s", code, lastBody)
	}
	if err := shapeOf("db-query", "refusal", lastBody); err != nil {
		t.Errorf("the refusal is not the contract's: %v", err)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select repeat('x', 9 * 1024 * 1024)"}`); code != 200 ||
		body["truncated"] != false || len(body["rows"].([]any)[0].([]any)[0].(string)) != 9<<20 {
		t.Errorf("a 9 MiB row: %d %.300s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT queries FROM db_sessions WHERE closed_at IS NULL"); n != 2 {
		t.Errorf("counted %d queries, want 2", n)
	}
}

// A statement of nothing, only whitespace or a comment, is Executed by Postgres to no command at all: 400
// invalid_sql, the statement is empty, never an answer without a command (#2813's review).
func TestAStatementOfNothingIsEmpty(t *testing.T) {
	pool, ctx, base, _, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	for _, sql := range []string{`"   "`, `"-- only a comment"`, `"/* and this */"`} {
		code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":`+sql+`}`)
		if code != 400 || body["error"] != "invalid_sql" || body["message"] != "the statement is empty" {
			t.Errorf("%s: %d %s", sql, code, lastBody)
		}
	}
}

// Read mode refuses every way to write, the database's own refusal at Execute, 422 query_failed (#2813's review):
// a write inside a WITH, a sequence's nextval, a temporary table, all read_only_sql_transaction (25006); and a turn
// back to read-write, which Postgres refuses once the transaction has run a query (Canyon's statement_timeout is one)
// as active_sql_transaction (25001). Every grant the writes need is given, so only read mode refuses them.
func TestReadModeRefusesEveryWayToWrite(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	inAppDB(t, ctx, b, "preview", "INSERT INTO ledger VALUES (1)", "GRANT DELETE ON ledger TO PUBLIC",
		"CREATE SEQUENCE invoice_no", "GRANT USAGE ON SEQUENCE invoice_no TO PUBLIC")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	for sql, sqlstate := range map[string]string{
		"with d as (delete from ledger returning n) select * from d": "25006",
		"select nextval('invoice_no')":                               "25006",
		"create temp table scratch (n int)":                          "25006",
		"set transaction read write":                                 "25001",
	} {
		code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"`+sql+`"}`)
		if code != 422 || body["error"] != "query_failed" || body["sqlstate"] != sqlstate {
			t.Errorf("%s: %d %s, want 422 query_failed %s", sql, code, lastBody, sqlstate)
		}
	}
	inAppDB(t, ctx, b, "preview", "DO $$ BEGIN IF (SELECT count(*) FROM ledger) <> 1 THEN RAISE 'read mode deleted'; END IF; END $$")
}

// A statement never runs past its database session's end: one that would outlive it is cut at the end, as a
// timeout, and the end doesn't move; once the end has passed, the checks close the session and nothing runs.
func TestAStatementNeverOutlivesItsSession(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, _, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"select 1"}`); code != 200 {
		t.Fatalf("the open: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE db_sessions SET expires_at = now() + interval '2 seconds'")
	var ends time.Time
	if err := pool.QueryRow(ctx, "SELECT expires_at FROM db_sessions").Scan(&ends); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"select pg_sleep(5)"}`); code != 422 || body["error"] != "statement_timeout" {
		t.Errorf("a statement past the session's end: %d %s", code, lastBody)
	}
	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT expires_at FROM db_sessions").Scan(&now); err != nil || !now.Equal(ends) {
		t.Errorf("the session's end moved: %s, was %s (%v)", now, ends, err)
	}
	runSQL(t, ctx, pool, "UPDATE db_sessions SET expires_at = now() - interval '1 second'")
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"write","sql":"insert into ledger values (9)"}`); code != 410 || body["error"] != "session_ended" || body["reason"] != "expired" {
		t.Errorf("a query on an expired session: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE close_reason = 'expired'"); n != 1 {
		t.Errorf("%d sessions closed expired, want 1", n)
	}
	inAppDB(t, ctx, b, "preview", "DO $$ BEGIN IF (SELECT count(*) FROM ledger) <> 0 THEN RAISE 'the expired session wrote'; END IF; END $$")
}

// unreadable is the built-in driver whose login, once, leaves Canyon's preferences unreadable once it has connected:
// a moment's failure to read them between a query's login and its checks.
type unreadable struct {
	*provisioners.Builtin
	pool *pgxpool.Pool
	once *sync.Once
}

func (u unreadable) Issue(ctx context.Context, k provisioners.Key, p provisioners.Person, m provisioners.Mode, ttl time.Duration, connect func(context.Context, provisioners.Login) error) error {
	return u.Builtin.Issue(ctx, k, p, m, ttl, func(ctx context.Context, l provisioners.Login) error {
		err := connect(ctx, l)
		u.once.Do(func() {
			_, failed := u.pool.Exec(ctx, "CREATE TABLE preferences_kept AS SELECT * FROM org_preferences; DELETE FROM org_preferences")
			err = errors.Join(err, failed)
		})
		return err
	})
}

// A query whose checks can't be judged runs nothing and closes nothing (#2865): a moment's failure to read the
// preferences is 503 check_failed, never a session closed turned_off as though an admin had turned them off, and
// the next query runs in the same database session.
func TestAQueryWhoseChecksFailClosesNothing(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: unreadable{b, pool, &sync.Once{}}}
	write := `{"env":"preview","mode":"write","sql":"insert into ledger values (9)"}`
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, write); code != 503 || body["error"] != "check_failed" || body["message"] != dbQueryUnchecked {
		t.Errorf("a query whose checks can't be judged: %d %s", code, lastBody)
	}
	dbShape(t, "refusal", `{"status":503,"body":`+lastBody+`}`)
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"); n != 1 {
		t.Errorf("%d database sessions open once its checks failed, want 1: closed %s", n, lastBody)
	}
	inAppDB(t, ctx, b, "preview", "DO $$ BEGIN IF (SELECT count(*) FROM ledger) <> 0 THEN RAISE 'the unchecked query wrote'; END IF; END $$")
	runSQL(t, ctx, pool, "INSERT INTO org_preferences SELECT * FROM preferences_kept")
	if code, _, _ := do(t, carl, http.MethodPost, base+dbQuery, write); code != 200 {
		t.Errorf("the next query: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions"); n != 1 {
		t.Errorf("the next query ran in %d database sessions, want the one", n)
	}
}

// The query holds the open's permissions (design/PERMISSIONS.md, Database sessions): a contributor reads and a viewer
// is refused team_required; a prod write is an owner's, a contributor's refused owner_required; with db_prod off a prod
// read is refused db_prod_off before any login. The person's login never reaches a log line, the trail or an answer.
func TestTheQueryHoldsTheOpensPermissions(t *testing.T) {
	pool, ctx, base, olive, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	claimsTable(t, ctx, b, "prod")
	var mu sync.Mutex
	var said bytes.Buffer
	appOf(t, base).log = log.New(lockedWriter{&mu, &said}, "", 0)
	secrets := recordSecrets(t, b)
	carl, vic := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	if code, body, _ := do(t, vic, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`); code != 403 || body["error"] != "team_required" {
		t.Errorf("a viewer: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`); code != 200 {
		t.Errorf("a contributor: %d %s", code, lastBody)
	}
	prodRead := `{"env":"prod","mode":"read","sql":"select 1","reason":"count yesterday's claims"}`
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, prodRead); code != 409 || body["error"] != "db_prod_off" ||
		count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE env = 'prod'") != 0 {
		t.Errorf("db_prod off: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET db_prod = 'write'")
	prodWrite := `{"env":"prod","mode":"write","sql":"insert into ledger values (1)","reason":"fix the duplicated March invoices"}`
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, prodWrite); code != 403 || body["error"] != "owner_required" {
		t.Errorf("a contributor's prod write: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, olive, http.MethodPost, base+dbQuery, prodWrite); code != 200 || body["rows_affected"] != 1.0 {
		t.Errorf("the owner's prod write: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1/0 as canary_statement"}`); code != 422 || body["error"] != "query_failed" {
		t.Errorf("a failing query, for the log: %d %s", code, lastBody)
	}
	var trail string
	if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(detail::text, ' '), '') FROM audit_events").Scan(&trail); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	logged := said.String()
	mu.Unlock()
	if len(*secrets) == 0 {
		t.Fatal("no login was issued")
	}
	for _, s := range *secrets {
		if strings.Contains(logged, s) || strings.Contains(trail, s) {
			t.Errorf("an issued password reached the log or the trail")
		}
	}
	if strings.Contains(trail, "canary_statement") || strings.Contains(logged, "canary_statement") || strings.Contains(trail, "insert into ledger") {
		t.Errorf("a statement was logged or stored:\n%s\n%s", logged, trail)
	}
}

// lockedWriter is a log's writer the test reads while the server writes it.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// recordingIssuer is the built-in driver, recording each password it issues for the test to look for.
type recordingIssuer struct {
	*provisioners.Builtin
	mu      sync.Mutex
	secrets *[]string
}

func (r *recordingIssuer) Issue(ctx context.Context, k provisioners.Key, p provisioners.Person, m provisioners.Mode, ttl time.Duration, connect func(context.Context, provisioners.Login) error) error {
	return r.Builtin.Issue(ctx, k, p, m, ttl, func(ctx context.Context, l provisioners.Login) error {
		r.mu.Lock()
		*r.secrets = append(*r.secrets, l.Secret.Reveal())
		r.mu.Unlock()
		return connect(ctx, l)
	})
}

// recordSecrets has the test server issue logins through a recordingIssuer, and answers what it records.
func recordSecrets(t *testing.T, b *provisioners.Builtin) *[]string {
	t.Helper()
	secrets := &[]string{}
	r := &recordingIssuer{Builtin: b, secrets: secrets}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: r, {"postgres", "prod"}: r}
	return secrets
}

// A read query can't lift its own READ ONLY (#2826): Canyon begins the transaction READ ONLY and runs its own statement
// first, so a statement that asks for read-write fails, a transaction's mode being set before its first query, and the
// next read query begins READ ONLY again. That is why a query needs no ReadOnly check and a relayed connection does.
func TestAReadQueryCannotLiftItsReadOnly(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview") // ledger takes INSERT from PUBLIC: only the transaction holds a read query to read
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	for _, lift := range []string{"SET TRANSACTION READ WRITE", "SET transaction_read_only = off", "SELECT set_config('transaction_read_only', 'off', true)"} {
		body, _ := json.Marshal(map[string]string{"env": "preview", "mode": "read", "sql": lift})
		if code, said, _ := do(t, carl, http.MethodPost, base+dbQuery, string(body)); code != 422 || said["sqlstate"] != "25001" { // active_sql_transaction
			t.Errorf("%s: %d %s, want 422 with 25001", lift, code, lastBody)
		}
		if code, said, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"insert into ledger values (9)"}`); code != 422 || said["sqlstate"] != "25006" {
			t.Errorf("a write after %s: %d %s, want 422 with 25006", lift, code, lastBody)
		}
	}
	code, read, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select count(*) from ledger"}`)
	if rows, _ := read["rows"].([]any); code != 200 || len(rows) != 1 || rows[0].([]any)[0] != "0" {
		t.Errorf("the ledger after them: %d %s, want no row written", code, lastBody)
	}
}

// A query holds one of the host's and the person's connections while its connection is open (design/DATABASE-SESSIONS.md
// §6; #2814): past the person's 10 or the host's cap it is 429 busy with that cap's message, judged before the login,
// so nothing runs and no database session opens; and a query gives its slot back once it answers, or fails.
func TestAQueryHoldsAConnectionSlot(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	a := appOf(t, base)
	read := `{"env":"preview","mode":"read","sql":"select count(*) from claims"}`
	held := []*dbRelay{}
	for range dbPersonRelays { // carl's ten connections, held as relays
		r := &dbRelay{subject: "sub-carl"}
		if over := a.relays.admit(r, 100); over != "" {
			t.Fatal(over)
		}
		held = append(held, r)
	}
	code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, read)
	if code != 429 || body["error"] != "busy" || body["message"] != "You already have 10 database connections open: close one and try again." {
		t.Errorf("carl's query with ten connections open: %d %s", code, lastBody)
	}
	dbShape(t, "refusal", `{"status":429,"body":`+lastBody+`}`)
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions"); n != 0 {
		t.Errorf("a busy query opened %d database session(s)", n)
	}
	for _, r := range held {
		a.relays.release(r)
	}
	held = held[:0]
	for i := range dbHostConnections { // ten other people's ten connections each fill the host's default cap
		r := &dbRelay{subject: "sub-other-" + strconv.Itoa(i/dbPersonRelays)}
		if over := a.relays.admit(r, a.dbHostCap()); over != "" {
			t.Fatal(over)
		}
		held = append(held, r)
	}
	code, body, _ = do(t, carl, http.MethodPost, base+dbQuery, read)
	if code != 429 || body["error"] != "busy" || body["message"] != "Canyon is at its limit of 100 database connections: try again in a minute." {
		t.Errorf("carl's query on a full host: %d %s", code, lastBody)
	}
	for _, r := range held {
		a.relays.release(r)
	}
	for i := range dbPersonRelays + 1 { // more queries than carl's 10: each gives its slot back, answered or failed
		sql := read
		if i%2 == 0 {
			sql = `{"env":"preview","mode":"read","sql":"select nope from claims"}`
		}
		if code, _, _ := do(t, carl, http.MethodPost, base+dbQuery, sql); code == 429 {
			t.Fatalf("carl's query %d was busy: a slot was never given back: %s", i+1, lastBody)
		}
	}
}
