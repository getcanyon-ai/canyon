package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// A relay, a cancel and a read are their person's alone (contracts/routes.md, the read, connect and cancel rows;
// #2129): to the app's owner, an admin or another contributor, the database session is 404, before their startup or
// body is read. Its person's startup that names user, database or replication is 400 invalid_startup, nothing
// dialled; their cancel is 204; their read answers it open, then closed with when and why; and once it has ended, a
// connect is 410 session_ended with its reason.
func TestARelayAndACancelAreItsPersonsAlone(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	startup := func(json string) string { return base64.RawURLEncoding.EncodeToString([]byte(json)) }
	connect := base + "/api/v1/db-sessions/" + sid + "/connect?startup=" + startup(`{"application_name":"psql"}`)
	cancel := base + "/api/v1/db-sessions/" + sid + "/cancel"
	for who, c := range map[string]*http.Client{"its app's owner": olive, "an admin": dana} {
		if code, body, _ := do(t, c, http.MethodGet, connect, ""); code != 404 || body["error"] != "not_found" {
			t.Errorf("%s connects: %d %s", who, code, lastBody)
		}
		if code, _, _ := do(t, c, http.MethodPost, cancel, `{"process_id":1}`); code != 404 {
			t.Errorf("%s cancels: %d %s", who, code, lastBody)
		}
	}
	for _, bad := range []string{`{"replication":"database"}`, `{"Database":"postgres"}`, `{"_pq_.x":"1"}`} {
		code, _, _ := do(t, carl, http.MethodGet, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup(bad), "")
		if code != 400 || !strings.Contains(lastBody, `"invalid_startup"`) {
			t.Errorf("carl's startup %s: %d %s", bad, code, lastBody)
		}
		dbShape(t, "refusal", `{"status":400,"body":`+lastBody+`}`)
	}
	if code, _, _ := do(t, carl, http.MethodPost, cancel, `{"process_id":48213}`); code != 204 {
		t.Errorf("carl cancels a pid that is no backend of his: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT connections FROM db_sessions WHERE id = '"+sid+"'"); n != 0 {
		t.Errorf("a refused connect counted %d connections", n)
	}
	read := base + "/api/v1/db-sessions/" + sid
	for who, c := range map[string]*http.Client{"its app's owner": olive, "an admin": dana} {
		if code, body, _ := do(t, c, http.MethodGet, read, ""); code != 404 || body["error"] != "not_found" {
			t.Errorf("%s reads it: %d %s", who, code, lastBody)
		}
	}
	if code, body, _ := do(t, carl, http.MethodGet, read, ""); code != 200 || body["id"] != sid || body["closed_at"] != nil || body["close_reason"] != nil {
		t.Errorf("carl reads it open: %d %s", code, lastBody)
	}
	dbShape(t, "db_session", lastBody)
	if code, _, _ := do(t, carl, http.MethodDelete, base+"/api/v1/db-sessions/"+sid, ""); code != 204 {
		t.Fatalf("carl ends it: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, carl, http.MethodGet, connect, ""); code != 410 || body["error"] != "session_ended" || body["reason"] != "closed" {
		t.Errorf("a connect after its end: %d %s", code, lastBody)
	}
	dbShape(t, "refusal", `{"status":410,"body":`+lastBody+`}`)
	if code, body, _ := do(t, carl, http.MethodGet, read, ""); code != 200 || body["close_reason"] != "closed" || body["closed_at"] == nil {
		t.Errorf("carl reads it closed: %d %s", code, lastBody)
	}
	dbShape(t, "db_session", lastBody)
}

// A replica's pass over its relays (design/DATABASE-SESSIONS.md §6; #2129): one whose database session closed ends
// with the row's reason; one whose own sign-in was revoked ends signed_out while the database session goes on; one
// still good is kept, and what it carried is added to its row.
func TestTheRelaysPassEndsWhatNoLongerHolds(t *testing.T) {
	pool, ctx, base, olive, _ := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	opened := map[string]string{}
	for who, c := range map[string]*http.Client{"sub-olive": olive, "sub-carl": carl} {
		code, body, _ := do(t, c, http.MethodPost, base+dbSessions, previewRead)
		if code != 201 {
			t.Fatalf("%s opens: %d %s", who, code, lastBody)
		}
		opened[who] = body["id"].(string)
	}
	signins := map[string]string{}
	for who := range opened {
		var id string
		if err := pool.QueryRow(ctx, "SELECT id FROM sessions WHERE actor_subject = $1 ORDER BY created_at DESC LIMIT 1", who).Scan(&id); err != nil {
			t.Fatal(err)
		}
		signins[who] = id
	}
	a := appOf(t, base)
	ended := map[string]string{}
	relay := func(who, sid, signin string) *dbRelay {
		r := &dbRelay{sid: sid, subject: who, signin: signin}
		r.endFn = func(_ websocket.StatusCode, reason, _ string, _ bool) { ended[who+" "+sid] = reason }
		if a.relays.admit(r, 100) != "" {
			t.Fatal("admit")
		}
		t.Cleanup(func() { a.relays.release(r) })
		return r
	}
	kept := relay("sub-olive", opened["sub-olive"], signins["sub-olive"])
	kept.fromDB.Store(1024)
	revoked := relay("sub-carl", opened["sub-carl"], signins["sub-carl"])
	closedOne := relay("sub-olive", "dbs_00000000000000aa", signins["sub-olive"])
	runSQL(t, ctx, pool, "UPDATE sessions SET revoked_at = now() WHERE id = '"+signins["sub-carl"]+"'",
		`INSERT INTO db_sessions (id, project_id, env, resource, mode, subject, role, expires_at, closed_at, close_reason) VALUES ('dbs_00000000000000aa', 'claims-portal', 'preview', 'db', 'read', 'sub-olive', 'p_x', now() + interval '1 hour', now(), 'idle')`)
	if err := a.checkRelays(ctx); err != nil {
		t.Fatal(err)
	}
	if ended["sub-carl "+revoked.sid] != "signed_out" || ended["sub-olive "+closedOne.sid] != "idle" || ended["sub-olive "+kept.sid] != "" {
		t.Fatalf("the pass ended %v", ended)
	}
	if n := count(t, ctx, pool, "SELECT bytes_from_db FROM db_sessions WHERE id = '"+kept.sid+"'"); n != 1024 {
		t.Errorf("the kept relay's 1024 bytes reached its row as %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE id = '"+revoked.sid+"' AND closed_at IS NULL"); n != 1 {
		t.Error("ending a relay signed_out closed its database session")
	}
}

// A relay carries a Postgres connection end to end (#2129, with #2118's dbConnect): the client reads Canyon's startup —
// AuthenticationOk, the backend's key data, ReadyForQuery — then a query runs as the person's own role, read-only, and
// the row counts the connection.
func TestARelayCarriesAQueryAsThePerson(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid, role := body["id"].(string), body["role"].(string)
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	connectAt := base + "/api/v1/db-sessions/" + sid + "/connect?startup=" + startup
	// ledger is granted INSERT to PUBLIC, so the person's role could write it: the read connection refuses before it
	// dials, naming it (#2826), and goes through once it is revoked
	const unsafe = `{"error":"read_only_unsafe","message":"Canyon didn't open a read connection: anyone connecting could still change data here (INSERT on public.ledger). Ask an admin of this app's database to revoke those rights, then connect again."}`
	if _, resp, err := websocket.Dial(ctx, connectAt, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}}); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("a read connection PUBLIC may write through: %v %v, want 409", err, resp)
	} else if said, _ := io.ReadAll(resp.Body); strings.TrimSpace(string(said)) != unsafe {
		t.Errorf("the refusal: %s, want %s", said, unsafe)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'canyon:"+sid+":psql'"); n != 0 {
		t.Errorf("%d backends of the refused connection", n)
	}
	inAppDB(t, ctx, b, "preview", "REVOKE INSERT ON ledger FROM PUBLIC")
	ws, _, err := websocket.Dial(ctx, connectAt, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}})
	if err != nil {
		t.Fatalf("carl connects: %v", err)
	}
	defer func() { _ = ws.CloseNow() }()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	fe := pgproto3.NewFrontend(conn, conn)
	var pid uint32
	for ready := false; !ready; {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the startup: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.BackendKeyData:
			pid = m.ProcessID
		case *pgproto3.ReadyForQuery:
			ready = true
		case *pgproto3.ErrorResponse:
			t.Fatalf("the startup: %s", m.Message)
		}
	}
	ask := func(sql string) (rows []string, failure string) {
		fe.Send(&pgproto3.Query{String: sql})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			switch m := msg.(type) {
			case *pgproto3.DataRow:
				var cells []string
				for _, v := range m.Values {
					cells = append(cells, string(v))
				}
				rows = append(rows, strings.Join(cells, "|"))
			case *pgproto3.ErrorResponse:
				failure = m.Code
			case *pgproto3.ReadyForQuery:
				return rows, failure
			}
		}
	}
	if rows, failure := ask("select status from claims order by status"); failure != "" || strings.Join(rows, ",") != "approved,denied,open" {
		t.Errorf("the query: %q %s", rows, failure)
	}
	if rows, _ := ask("select pg_has_role(current_user, '" + role + "', 'member'), current_setting('application_name')"); len(rows) != 1 || rows[0] != "t|canyon:"+sid+":psql" {
		t.Errorf("it ran as %q, want a login of %s as canyon:%s:psql", rows, role, sid)
	}
	// the read session's default read-only stops a plain write first, though the client may SET it off
	if _, failure := ask("insert into ledger values (1)"); failure != "25006" { // read_only_sql_transaction
		t.Errorf("a plain write in a read session: %q, want refused by its default read-only", failure)
	}
	// with that default off, the read group's grants are what enforce read: claims takes no INSERT from it
	if _, failure := ask("set default_transaction_read_only = off"); failure != "" {
		t.Fatalf("turning the default off: %q", failure)
	}
	for _, write := range []string{"insert into claims values ('x', 1)", "insert into ledger values (1)"} {
		if _, failure := ask(write); failure != "42501" { // insufficient_privilege
			t.Errorf("%s with the default off: %q, want 42501", write, failure)
		}
	}
	if pid == 0 || count(t, ctx, pool, "SELECT connections FROM db_sessions WHERE id = '"+sid+"'") != 1 {
		t.Errorf("backend %d, or the row did not count the connection", pid)
	}
}

// relayStarted connects c to the database session sid as psql and reads the connection's start up to
// ReadyForQuery, failing the test on anything else.
func relayStarted(t *testing.T, ctx context.Context, base string, c *http.Client, sid string) *websocket.Conn {
	t.Helper()
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	ws, _, err := websocket.Dial(ctx, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup, &websocket.DialOptions{HTTPClient: c, Subprotocols: []string{"canyon-db.1"}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	fe := pgproto3.NewFrontend(conn, conn)
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("the startup: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return ws
		}
	}
}

// A relay ends the moment its database session does, close code 4000 expired, its backend ended: never a check's
// interval later (#2129's review). The session here ends 2 seconds after it connects.
func TestARelayEndsWithItsDatabaseSession(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	claimsTable(t, ctx, b, "preview")
	inAppDB(t, ctx, b, "preview", "REVOKE INSERT ON ledger FROM PUBLIC") // a read connection refuses while PUBLIC may write (#2826)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	if code, _, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead); code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	var sid string
	if err := pool.QueryRow(ctx, "UPDATE db_sessions SET expires_at = now() + interval '2 seconds' RETURNING id").Scan(&sid); err != nil {
		t.Fatal(err)
	}
	ws := relayStarted(t, ctx, base, carl, sid)
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = ws.Read(rctx)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4000 || ce.Reason != "expired" || time.Since(start) > 4*time.Second {
		t.Fatalf("after %s the relay answered %v; want close 4000 expired at the session's end", time.Since(start).Round(time.Millisecond), err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'canyon:"+sid+":psql'"); n != 0 {
		t.Errorf("%d backends of the relay left after it ended", n)
	}
}

// A connect the login refuses says why, with the refusal's own 409, never 502 database_unreachable, which reads as a
// database Canyon couldn't reach (#2826): public opened to CREATE since the session opened is unsafe_schema, as the
// open's is, and nothing is dialled.
func TestAConnectSaysWhyTheLoginRefused(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	inAppDB(t, ctx, b, "preview", "GRANT CREATE ON SCHEMA public TO PUBLIC")
	const unsafe = `{"error":"unsafe_schema","message":"PUBLIC can still create in schema public, which only its owner or a superuser can revoke. Ask an admin of this app's database to run REVOKE CREATE ON SCHEMA public FROM PUBLIC, then try again."}`
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	_, resp, err := websocket.Dial(ctx, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("a connect with public open to CREATE: %v %v, want 409", err, resp)
	}
	if said, _ := io.ReadAll(resp.Body); strings.TrimSpace(string(said)) != unsafe {
		t.Errorf("the refusal: %s, want %s", said, unsafe)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'canyon:"+sid+":psql'"); n != 0 {
		t.Errorf("%d backends of the refused connection", n)
	}
}

// A connect whose checks can't be judged is 503 check_failed and closes nothing, and one whose database doesn't answer
// the login is 502 database_unreachable (#2865): the two never blur, and neither is a session ended.
func TestAConnectTellsAFailedCheckFromAnUnreachableDatabase(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	connect := func(why string, status int, want string) {
		t.Helper()
		_, resp, err := websocket.Dial(ctx, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}})
		if err == nil || resp == nil || resp.StatusCode != status {
			t.Errorf("%s: %v %v, want %d", why, err, resp, status)
			return
		}
		said, _ := io.ReadAll(resp.Body)
		if got := strings.TrimSpace(string(said)); got != want {
			t.Errorf("%s: %s, want %s", why, got, want)
		}
		dbShape(t, "refusal", `{"status":`+strconv.Itoa(status)+`,"body":`+strings.TrimSpace(string(said))+`}`)
		if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"); n != 1 {
			t.Errorf("%s: %d database sessions open, want carl's", why, n)
		}
	}
	runSQL(t, ctx, pool, "ALTER TABLE project_access RENAME TO project_access_kept") // carl's level can't be read
	connect("a check that can't be judged", 503, `{"error":"check_failed","message":"`+dbConnectUnchecked+`"}`)
	runSQL(t, ctx, pool, "ALTER TABLE project_access_kept RENAME TO project_access")
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: refusing{b, errors.New("dial tcp: connection refused")}}
	connect("a database that doesn't answer", 502, `{"error":"database_unreachable","message":"Canyon couldn't reach the database of claims-portal in preview: try again."}`)
	// a refusal of a database the login reached says what Canyon found, as the open's does, never 502 (#2910)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: refusing{b, &provisioners.NotAsMade{Found: notAsMadeFound}}}
	connect("a role Canyon didn't make", 409, `{"error":"not_as_made","message":"`+loginRefusals["not_as_made"]+`"}`)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: refusing{b, fmt.Errorf("builtin postgres: issue: %w", provisioners.ErrNoDatabase)}}
	connect("a database that isn't there", 409, `{"error":"database_not_ready","message":"`+loginRefusals["database_not_ready"]+`"}`)
}

// A read session can open no second connection from inside the database (#2918; contracts/routes.md, the connect and
// query rows): with dblink installed, whose functions PUBLIC may EXECUTE, a read query and a read connect are refused
// 409 read_only_unsafe naming its functions, before anything runs or is dialled, and the session stays open.
func TestAReadSessionCanOpenNoSecondConnection(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t) // nothing granted past read, so ReadOnly lets a connect through and this check alone judges it
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead) // an open dials nothing, so it is not judged
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	inAppDB(t, ctx, b, "preview", "CREATE EXTENSION dblink")
	const said = "Canyon didn't open a read connection: anyone connecting could still change data here (EXECUTE on dblink function public.dblink(text), "
	code, refusal, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`)
	if msg, _ := refusal["message"].(string); code != 409 || refusal["error"] != "read_only_unsafe" || !strings.HasPrefix(msg, said) || !strings.HasSuffix(msg, "Ask an admin of this app's database to revoke those rights, then connect again.") {
		t.Errorf("a read query with dblink installed: %d %s", code, lastBody)
	}
	dbShape(t, "refusal", `{"status":409,"body":`+lastBody+`}`)
	if n := count(t, ctx, pool, "SELECT queries FROM db_sessions WHERE id = '"+sid+"'"); n != 0 {
		t.Errorf("the refused query was counted as run: %d", n)
	}
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	_, resp, err := websocket.Dial(ctx, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("a read connect with dblink installed: %v %v, want 409", err, resp)
	}
	if got, _ := io.ReadAll(resp.Body); !strings.Contains(string(got), `"error":"read_only_unsafe"`) || !strings.Contains(string(got), said) {
		t.Errorf("the connect's refusal: %s", got)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'canyon:"+sid+":psql'"); n != 0 {
		t.Errorf("%d backends of the refused connection", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"); n != 1 {
		t.Errorf("%d database sessions open after the refusals, want carl's", n)
	}
}

// A SECURITY DEFINER wrapper the app owns runs dblink as the app (#2920's review): with dblink revoked from PUBLIC and
// granted to the app's login, a read query is still refused 409 read_only_unsafe, naming the wrapper.
func TestAReadQueryCanNotWriteThroughADblinkWrapper(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	inAppDB(t, ctx, b, "preview", "CREATE EXTENSION dblink", "REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC",
		// the app's login owns its database: it is granted dblink, and makes the wrapper, as its own
		"DO $$ BEGIN EXECUTE format('GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO %I', pg_catalog.pg_get_userbyid((SELECT datdba FROM pg_catalog.pg_database WHERE datname = current_database()))); "+
			"EXECUTE format('SET ROLE %I', pg_catalog.pg_get_userbyid((SELECT datdba FROM pg_catalog.pg_database WHERE datname = current_database()))); END $$",
		"CREATE FUNCTION helper(conn text, sql text) RETURNS text LANGUAGE sql SECURITY DEFINER AS 'SELECT public.dblink_exec(conn, sql)'",
		"RESET ROLE")
	code, refusal, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`)
	if msg, _ := refusal["message"].(string); code != 409 || refusal["error"] != "read_only_unsafe" ||
		msg != "Canyon didn't open a read connection: anyone connecting could still change data here (EXECUTE on SECURITY DEFINER function public.helper(conn text, sql text)). Ask an admin of this app's database to revoke those rights, then connect again." {
		t.Errorf("a read query with an app-owned dblink wrapper: %d %s", code, lastBody)
	}
}

// uncheckable is the built-in driver whose read checks can't read what they judge, as a catalog read that fails in a
// database that answered (#2920's review).
type uncheckable struct {
	*provisioners.Builtin
	readOnly, second bool
}

func (u uncheckable) ReadOnly(ctx context.Context, l provisioners.Login) error {
	if u.readOnly {
		return fmt.Errorf("builtin postgres: read only: %w: role \"p_x\" does not exist", provisioners.ErrUnchecked)
	}
	return u.Builtin.ReadOnly(ctx, l)
}

func (u uncheckable) NoSecondConnection(ctx context.Context, l provisioners.Login) error {
	if u.second {
		return fmt.Errorf("builtin postgres: second connection: %w: role \"p_x\" does not exist", provisioners.ErrUnchecked)
	}
	return u.Builtin.NoSecondConnection(ctx, l)
}

// A read check that can't read what it judges is a check Canyon couldn't judge, 503 check_failed with its route's own
// message, never 502 database_unreachable: on the query, NoSecondConnection's; on the connect, each of the two.
func TestAnUnreadReadCheckIsCheckFailed(t *testing.T) {
	pool, ctx, base, _, b := dbHost(t)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	code, body, _ := do(t, carl, http.MethodPost, base+dbSessions, previewRead)
	if code != 201 {
		t.Fatalf("carl opens: %d %s", code, lastBody)
	}
	sid := body["id"].(string)
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: uncheckable{b, false, true}}
	if code, body, _ := do(t, carl, http.MethodPost, base+dbQuery, `{"env":"preview","mode":"read","sql":"select 1"}`); code != 503 || body["error"] != "check_failed" || body["message"] != dbQueryUnchecked {
		t.Errorf("a read query whose NoSecondConnection can't read: %d %s", code, lastBody)
	}
	startup := base64.RawURLEncoding.EncodeToString([]byte(`{"application_name":"psql"}`))
	for _, c := range []struct {
		why string
		u   uncheckable
	}{{"ReadOnly", uncheckable{b, true, false}}, {"NoSecondConnection", uncheckable{b, false, true}}} {
		pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: c.u}
		_, resp, err := websocket.Dial(ctx, base+"/api/v1/db-sessions/"+sid+"/connect?startup="+startup, &websocket.DialOptions{HTTPClient: carl, Subprotocols: []string{"canyon-db.1"}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("a read connect whose %s can't read: %v %v, want 503", c.why, err, resp)
			continue
		}
		if got, _ := io.ReadAll(resp.Body); strings.TrimSpace(string(got)) != `{"error":"check_failed","message":"`+dbConnectUnchecked+`"}` {
			t.Errorf("a read connect whose %s can't read: %s", c.why, got)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM db_sessions WHERE closed_at IS NULL"); n != 1 {
		t.Errorf("%d database sessions open, want carl's: a check that can't read closes nothing", n)
	}
}
