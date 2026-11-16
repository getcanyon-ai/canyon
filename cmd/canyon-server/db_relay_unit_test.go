package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// A request carrying Origin is a browser's, whose page could ride the person's cookie: it is refused 403
// cross_origin before the session is read or anything is dialled (contracts/routes.md, the connect row; #2129).
func TestAConnectWithAnOriginIsRefused(t *testing.T) {
	a := &app{pool: unreachable(t), log: log.New(os.Stderr, "", 0)} // a refusal that read the database would answer 500 here
	r := httptest.NewRequest(http.MethodGet, "/db-sessions/dbs_0cf24d382accf9ea/connect", nil)
	r.SetPathValue("sid", "dbs_0cf24d382accf9ea")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	a.connectDBSession(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"cross_origin"`) {
		t.Fatalf("a connect with Origin: %d %s", w.Code, w.Body)
	}
	if err := dbShapeOf("refusal", `{"status":403,"body":`+strings.TrimSpace(w.Body.String())+`}`); err != nil {
		t.Errorf("the refusal is not the contract's: %v", err)
	}
}

// What the client reads as its connection's start, byte for byte: a pgx bump that changes HijackedConn fails to
// compile here, and one that changes an encoding fails the bytes. The backend's secret key is never handed on.
func TestTheSyntheticStartupIsGolden(t *testing.T) {
	hj := &pgconn.HijackedConn{
		PID:               48213,
		SecretKey:         []byte{0xde, 0xad, 0xbe, 0xef},
		ParameterStatuses: map[string]string{"server_version": "17.2", "client_encoding": "UTF8", "DateStyle": "ISO, MDY"},
		TxStatus:          'I',
	}
	got, err := syntheticStartup(hj)
	if err != nil {
		t.Fatal(err)
	}
	golden := strings.TrimSpace(readHex(t))
	if hex.EncodeToString(got) != golden {
		t.Errorf("the synthetic startup is\n%x\nwant\n%s", got, golden)
	}
	if strings.Contains(hex.EncodeToString(got), "deadbeef") {
		t.Error("the backend's secret key reached the client")
	}
}

func readHex(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/db-startup.hex")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Whatever the client sends as its startup, the relay forwards only what the contract allows (db-session.schema.json,
// startup): never user, database or replication, in any case, never a protocol option, never a NUL, at most 64
// names and 1,024 characters a value, from at most 4 KiB encoded, never two names the same but for case, and application_name
// in lower case alone; anything else is refused whole.
func FuzzStartupParameters(f *testing.F) {
	g, err := os.ReadFile("../../contracts/testdata/db-session/meridian.json")
	if err != nil {
		f.Fatal(err)
	}
	var doc struct {
		Relay struct {
			Startup json.RawMessage `json:"startup"`
		} `json:"relay"`
	}
	if err := json.Unmarshal(g, &doc); err != nil {
		f.Fatal(err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, seed := range []string{
		enc(string(doc.Relay.Startup)),
		enc(`{"replication":"database"}`), enc(`{"Database":"postgres"}`), enc(`{"USER":"postgres"}`),
		enc(`{"_pq_.command_protocol":"1"}`), enc(`{"a":"x\u0000y"}`), enc(`{"9bad":"x"}`), enc(`{"a":1}`), enc(`[]`), "!!", "",
		enc(`{"APPLICATION_NAME":"x"}`), enc(`{"application_name":"psql","Application_Name":"x"}`), enc(`{"TimeZone":"UTC","timezone":"CET"}`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		params, err := decodeStartup(raw)
		if err != nil {
			return
		}
		if len(raw) > dbStartupMost || len(params) > dbStartupNames {
			t.Fatalf("forwarded %d names from %d bytes", len(params), len(raw))
		}
		seen := map[string]bool{}
		for name, value := range params {
			lower := strings.ToLower(name)
			// Postgres reads a name in any case: two that differ only in case are one parameter set twice, and
			// application_name in any other case could overwrite Canyon's canyon:<sid>:<tool> (#2129 review).
			if seen[lower] || (lower == "application_name" && name != "application_name") {
				t.Fatalf("forwarded %q beside a name the same but for case, or application_name in another case", name)
			}
			seen[lower] = true
			if lower == "user" || lower == "database" || lower == "replication" || strings.HasPrefix(lower, "_pq_.") || !dbStartupName.MatchString(name) {
				t.Fatalf("forwarded the name %q", name)
			}
			if strings.ContainsRune(value, 0) || !utf8.ValidString(value) || utf8.RuneCountInString(value) > dbStartupValueMost {
				t.Fatalf("forwarded %q = %q", name, value)
			}
		}
	})
}

// Postgres reads a startup name in any case, so whatever case a parameter names application_name in, the backend
// is sent Canyon's canyon:<sid>:<tool> alone (#2129 review).
func TestNoStartupNameOverridesCanyonsApplicationName(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan map[string]string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer func() { _ = c.Close() }()
		m, err := pgproto3.NewBackend(c, c).ReceiveStartupMessage()
		sm, _ := m.(*pgproto3.StartupMessage)
		if err != nil || sm == nil {
			got <- nil
			return
		}
		got <- sm.Parameters
	}()
	l := provisioners.Login{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Database: "app", User: "dana", Secret: provisioners.NewSecret(""), TLS: provisioners.TLSMode{SSLMode: "disable"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = dbConnect(ctx, l, "dbs_0cf24d382accf9ea", "psql", map[string]string{"APPLICATION_NAME": "x", "Application_name": "y", "TimeZone": "UTC"}, &countingConn{})
	params := <-got
	for name, value := range params {
		if strings.EqualFold(name, "application_name") && (name != "application_name" || value != "canyon:dbs_0cf24d382accf9ea:psql") {
			t.Errorf("the backend was sent %s=%q", name, value)
		}
	}
	if params["application_name"] != "canyon:dbs_0cf24d382accf9ea:psql" || params["TimeZone"] != "UTC" {
		t.Errorf("the backend was sent %v", params)
	}
}

// A login whose sslmode would try plaintext first is refused before anything is dialled (#2129's review, on #2822's
// LoginConfig): the relay's dial errs naming the setting, which the connect answers 502 database_unreachable, and
// nothing reaches the database's port. The built-in driver refuses such an admin URL at start, so this is the
// relay's own guard.
func TestARelayNeverDialsInPlaintextFirst(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	dialled := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			dialled <- struct{}{}
			_ = c.Close()
		}
	}()
	l := provisioners.Login{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Database: "app", User: "dana", Secret: provisioners.NewSecret("SECRETVALUE"), TLS: provisioners.TLSMode{SSLMode: "allow"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dbConnect(ctx, l, "dbs_0cf24d382accf9ea", "psql", nil, &countingConn{}); err == nil || !strings.Contains(err.Error(), "sslmode=allow") || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("a login under sslmode allow: %v, want refused naming the setting alone", err)
	}
	select {
	case <-dialled:
		t.Error("the database's port was dialled")
	case <-time.After(200 * time.Millisecond):
	}
}

// The caps are judged under the registry's lock: the host's, the person's, and none at all once shutting down; an
// end asked for before a relay runs ends it as it starts.
func TestTheRelaysCapsAndAnEndBeforeItRuns(t *testing.T) {
	var rs dbRelays
	for i := range dbPersonRelays {
		if rs.admit(&dbRelay{subject: "dana"}, 100) != "" {
			t.Fatalf("dana's relay %d was refused", i+1)
		}
	}
	if rs.admit(&dbRelay{subject: "dana"}, 100) != "person" {
		t.Error("dana's eleventh relay was admitted")
	}
	if rs.admit(&dbRelay{subject: "olive"}, 11) != "" || rs.admit(&dbRelay{subject: "carl"}, 11) != "host" {
		t.Error("the host's cap of 11 was not the twelfth relay's refusal")
	}
	rs.closing = true
	if rs.admit(&dbRelay{subject: "nora"}, 100) == "" {
		t.Error("a relay was admitted while the server shuts down")
	}
	r := &dbRelay{}
	r.end(dbClosed, "signed_out", "check:signed_out", true)
	var got string
	r.mu.Lock()
	r.endFn = func(_ websocket.StatusCode, reason, _ string, _ bool) { got = reason }
	pending := r.pending
	r.mu.Unlock()
	if pending == nil {
		t.Fatal("an end before the relay ran was dropped")
	}
	pending()
	if got != "signed_out" {
		t.Errorf("the end before it ran ended it %q", got)
	}
}

// The host's and the person's caps count every connection this server opens to an apps database for a person, a
// relay's and a query's alike (design/DATABASE-SESSIONS.md §6; #2814): with CANYON_DB_MAX_CONNECTIONS at 100 the
// 101st is refused, a person's 11th is refused whichever kind it is, each with its cap's message, and a released
// query's slot is free again.
func TestDBCaps(t *testing.T) {
	var rs dbRelays
	var releases []func()
	for i := range 100 { // ten people, five relays and five queries each
		who := "person-" + strconv.Itoa(i/10)
		if i%2 == 0 {
			if over := rs.admit(&dbRelay{subject: who}, 100); over != "" {
				t.Fatalf("connection %d, a relay of %s, refused: %s", i+1, who, over)
			}
			continue
		}
		release, over := rs.reserve(who, 100)
		if over != "" {
			t.Fatalf("connection %d, a query of %s, refused: %s", i+1, who, over)
		}
		releases = append(releases, release)
	}
	if _, over := rs.reserve("nora", 100); over != "host" {
		t.Errorf("the 101st connection, nora's first query: %q, want the host's cap", over)
	}
	if over := rs.admit(&dbRelay{subject: "nora"}, 100); over != "host" {
		t.Errorf("the 101st connection, nora's first relay: %q, want the host's cap", over)
	}
	if _, over := rs.reserve("person-0", 1000); over != "person" {
		t.Errorf("person-0's 11th connection, a query, five relays and five queries open: %q, want the person's cap", over)
	}
	if over := rs.admit(&dbRelay{subject: "person-0"}, 1000); over != "person" {
		t.Errorf("person-0's 11th connection, a relay: %q, want the person's cap", over)
	}
	releases[0]()
	releases[0]() // a release is once, however often it is called
	if release, over := rs.reserve("person-0", 100); over != "" {
		t.Errorf("a query once person-0's first query released its slot: %q", over)
	} else {
		release()
	}
	if got := busySaid("person", 100); got != "You already have 10 database connections open: close one and try again." {
		t.Errorf("the person's cap says %q", got)
	}
	if got := busySaid("host", 100); got != "Canyon is at its limit of 100 database connections: try again in a minute." {
		t.Errorf("the host's cap says %q", got)
	}
}

// A pass that cannot read the relays' rows at all ends every relay, never keeping one on a state nobody read
// (design/DATABASE-SESSIONS.md §7; #2129).
func TestASweepThatCannotReadEndsEveryRelay(t *testing.T) {
	a := &app{pool: unreachable(t), log: log.New(os.Stderr, "", 0)}
	ended := map[string]string{}
	for _, sid := range []string{"dbs_0000000000000001", "dbs_0000000000000002"} {
		r := &dbRelay{sid: sid, subject: "dana"}
		r.endFn = func(_ websocket.StatusCode, _, cause string, _ bool) { ended[sid] = cause }
		if a.relays.admit(r, 100) != "" {
			t.Fatal("admit")
		}
	}
	if err := a.checkRelays(context.Background()); err == nil || len(ended) != 2 || ended["dbs_0000000000000001"] != "check:unread" {
		t.Fatalf("a pass that could not read: %v, ended %v", err, ended)
	}
}

// unreachable is a pool whose every query fails: a database that is down.
func unreachable(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/nothing?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestTheHostsCapIsAWholeNumberOrTheBootFails(t *testing.T) {
	for v, want := range map[string]int{"": dbHostConnections, "1": 1, "250": 250} {
		if got, err := dbMaxConnsOf(v); err != nil || got != want {
			t.Errorf("%q: %d %v", v, got, err)
		}
	}
	for _, v := range []string{"0", "-1", "ten", "10001", "1.5"} {
		if _, err := dbMaxConnsOf(v); err == nil {
			t.Errorf("%q was taken as a cap", v)
		}
	}
}
