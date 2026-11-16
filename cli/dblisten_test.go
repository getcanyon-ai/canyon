package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// lockedBuffer is a listener's output, written from its connections' goroutines and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// relayHost stands in for Canyon: it opens the golden's database session, and answers a connect as the relay
// does (contracts/routes.md): a refusal when refuse is set, else the WebSocket, Canyon's synthetic startup for
// backend 48213, then a close when closeWith is set or the client's own end.
type relayHost struct {
	mu        sync.Mutex
	session   json.RawMessage
	refuse    *reply
	closeWith *websocket.CloseError
	older     bool     // a host before the relay: it upgrades, and names no subprotocol
	reads     *reply   // GET /api/v1/db-sessions/:sid's answer, the session open when nil
	connects  []string // each connect's startup, decoded
	cancels   []string
	deletes   []string
	server    *httptest.Server
}

const testToken = "0123456789abcdef0123456789abcdef01234567"

func newRelayHost(t *testing.T) *relayHost {
	t.Helper()
	var q struct {
		Answer struct {
			Session json.RawMessage `json:"db_session"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(dbGolden(t)["preview_query"], &q); err != nil {
		t.Fatal(err)
	}
	// the golden's session, opened now for its 8 hours: a run on it ends at expires_at
	now := time.Now().UTC().Truncate(time.Second)
	h := &relayHost{session: []byte(strings.NewReplacer(`"opened_at": "2026-10-18T10:02:11Z"`, `"opened_at": "`+now.Format(time.RFC3339)+`"`,
		`"expires_at": "2026-10-18T18:02:11Z"`, `"expires_at": "`+now.Add(8*time.Hour).Format(time.RFC3339)+`"`).Replace(string(q.Answer.Session)))}
	if !strings.Contains(string(h.session), now.Format(time.RFC3339)) {
		t.Fatalf("the golden's session times moved: %s", h.session)
	}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *relayHost) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	refuse, closeWith, older, reads := h.refuse, h.closeWith, h.older, h.reads
	h.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.Count(strings.TrimPrefix(r.URL.Path, "/api/v1/db-sessions/"), "/") == 0 && strings.HasPrefix(r.URL.Path, "/api/v1/db-sessions/"):
		if reads != nil {
			w.WriteHeader(reads.code)
			_, _ = w.Write([]byte(reads.body))
			return
		}
		_, _ = w.Write(h.session)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/db-sessions"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(h.session)
	case strings.HasSuffix(r.URL.Path, "/connect"):
		startup, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("startup"))
		h.mu.Lock()
		h.connects = append(h.connects, string(startup))
		h.mu.Unlock()
		if refuse != nil {
			w.WriteHeader(refuse.code)
			_, _ = w.Write([]byte(refuse.body))
			return
		}
		offered := []string{dbSubprotocol}
		if older {
			offered = nil
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: offered})
		if err != nil {
			return
		}
		var first []byte
		for _, m := range []pgproto3.BackendMessage{&pgproto3.AuthenticationOk{}, &pgproto3.ParameterStatus{Name: "server_version", Value: "16.4"},
			&pgproto3.BackendKeyData{ProcessID: 48213, SecretKey: make([]byte, 4)}, &pgproto3.ReadyForQuery{TxStatus: 'I'}} {
			first, _ = m.Encode(first)
		}
		_ = ws.Write(r.Context(), websocket.MessageBinary, first)
		if closeWith != nil {
			_ = ws.Close(closeWith.Code, closeWith.Reason)
			return
		}
		for { // the client's bytes, until it ends
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	case strings.HasSuffix(r.URL.Path, "/cancel"):
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.cancels = append(h.cancels, string(body))
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		h.mu.Lock()
		h.deletes = append(h.deletes, r.URL.String())
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *relayHost) connectsSeen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.connects...)
}

// listening is a listener for the golden's database session on a free loopback port, its output in out.
func listening(t *testing.T, h *relayHost) (l *dbListener, addr string, out *lockedBuffer) {
	t.Helper()
	return listeningGated(t, h, nil)
}

// listeningGated is listening with each relay's reader held until reader closes.
func listeningGated(t *testing.T, h *relayHost, reader chan struct{}) (l *dbListener, addr string, out *lockedBuffer) {
	t.Helper()
	var s dbSession
	if err := json.Unmarshal(h.session, &s); err != nil {
		t.Fatal(err)
	}
	cert, err := loopbackCert()
	if err != nil {
		t.Fatal(err)
	}
	out = &lockedBuffer{}
	l = &dbListener{a: &app{out: out, err: out}, client: newClient(h.server.URL), session: s, app: "claims-portal", token: testToken, cert: cert, ended: make(chan string, 1), reader: reader}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ln.Close() })
	go l.serve(ctx, ln)
	return l, ln.Addr().String(), out
}

// The client hangs up as Canyon ends the database session (#2852): Canyon closes 4000 idle right after its startup, the
// client hangs up while the relay's reader is held, so the relay's own close handshake reads Canyon's 4000, and the
// run still ends, with Canyon's reason.
func TestCanyonsEndThatMeetsTheClientsHangUpEndsTheRun(t *testing.T) {
	h := newRelayHost(t)
	h.closeWith = &websocket.CloseError{Code: dbClosed, Reason: "idle"}
	reader := make(chan struct{})
	defer close(reader)
	l, addr, out := listeningGated(t, h, reader)
	conn, err := signIn(t, addr, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(context.Background())
	select {
	case reason := <-l.ended:
		if reason != "idle" {
			t.Errorf("the run ended %q, want idle", reason)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("the run did not end when Canyon's end met the client's hang-up\n%s", out)
	}
}

// eventually waits up to two seconds for cond.
func eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	for range 200 {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// sslmode=require connects through the listener's certificate and reaches Canyon's backend; verify-full refuses
// the certificate, which nothing vouches for, and the listener says why on the person's terminal (§3).
func TestRequireSucceedsVerifyFullFails(t *testing.T) {
	h := newRelayHost(t)
	_, addr, out := listening(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://dana:"+testToken+"@"+addr+"/claims-portal?sslmode=require")
	if err != nil {
		t.Fatalf("sslmode=require: %v\n%s", err, out)
	}
	if conn.PID() != 48213 || conn.ParameterStatus("server_version") != "16.4" {
		t.Errorf("connected to backend %d, server_version %q", conn.PID(), conn.ParameterStatus("server_version"))
	}
	_ = conn.Close(ctx)
	if _, err := pgconn.Connect(ctx, "postgres://dana:"+testToken+"@"+addr+"/claims-portal?sslmode=verify-full"); err == nil {
		t.Fatal("verify-full connected to a certificate nothing vouches for")
	}
	said := "· A client refused this listener's certificate. It is made for this run only: connect with sslmode=require, not verify-ca or verify-full.\n"
	if !eventually(t, func() bool { return strings.Contains(out.String(), said) }) {
		t.Errorf("the listener said\n%s", out)
	}
}

// raw is a client that speaks the protocol by hand: what it sends and what it reads are under test.
func raw(t *testing.T, addr string) (net.Conn, *pgproto3.Frontend) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, pgproto3.NewFrontend(conn, conn)
}

func send(t *testing.T, fe *pgproto3.Frontend, msgs ...pgproto3.FrontendMessage) {
	t.Helper()
	for _, m := range msgs {
		fe.Send(m)
	}
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, fe *pgproto3.Frontend) pgproto3.BackendMessage {
	t.Helper()
	msg, err := fe.Receive()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// A GSSENCRequest is declined with N and the same connection asks for TLS next, which it gets (§3).
func TestGSSEncThenSSL(t *testing.T) {
	_, addr, _ := listening(t, newRelayHost(t))
	conn, fe := raw(t, addr)
	answer := make([]byte, 1)
	send(t, fe, &pgproto3.GSSEncRequest{})
	if _, err := io.ReadFull(conn, answer); err != nil || answer[0] != 'N' {
		t.Fatalf("GSSENCRequest answered %q, %v", answer, err)
	}
	send(t, fe, &pgproto3.SSLRequest{})
	if _, err := io.ReadFull(conn, answer); err != nil || answer[0] != 'S' {
		t.Fatalf("SSLRequest after it answered %q, %v", answer, err)
	}
}

// A client asking protocol 3.2, or naming a _pq_. option, is told 3.0 and the options declined, then asked its
// password as any 3.0 client is.
func TestProtocol32GetsNegotiateProtocolVersion(t *testing.T) {
	_, addr, _ := listening(t, newRelayHost(t))
	_, fe := raw(t, addr)
	send(t, fe, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion32, Parameters: map[string]string{"user": "dana", "_pq_.b": "1", "_pq_.a": "1"}})
	if m, ok := receive(t, fe).(*pgproto3.NegotiateProtocolVersion); !ok || m.NewestMinorProtocol != 0 || !reflect.DeepEqual(m.UnrecognizedOptions, []string{"_pq_.a", "_pq_.b"}) {
		t.Fatalf("answered %#v", m)
	}
	if m, ok := receive(t, fe).(*pgproto3.AuthenticationSASL); !ok || !reflect.DeepEqual(m.AuthMechanisms, []string{"SCRAM-SHA-256"}) {
		t.Errorf("then asked %#v, not SCRAM-SHA-256", m)
	}
}

// refusedBefore is a client refused a FATAL with the message, and Canyon asked nothing.
func refusedBefore(t *testing.T, h *relayHost, fe *pgproto3.Frontend, code, message string) {
	t.Helper()
	if e, ok := receive(t, fe).(*pgproto3.ErrorResponse); !ok || e.Severity != "FATAL" || e.Code != code || e.Message != "Canyon: "+message {
		t.Errorf("answered %#v", e)
	}
	if seen := h.connectsSeen(); len(seen) != 0 {
		t.Errorf("Canyon was asked to connect: %q", seen)
	}
}

// signIn is a pgconn client through the listener's SCRAM with password and params: the connection, or the error the
// listener's FATAL came back as.
func signIn(t *testing.T, addr, password string, params map[string]string) (*pgconn.PgConn, error) {
	t.Helper()
	cfg, err := pgconn.ParseConfig("postgres://dana:" + url.QueryEscape(password) + "@" + addr + "/claims-portal?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(cfg.RuntimeParams, params)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
	}
	return conn, err
}

// fatalOf is the FATAL a connect failed on; "" for none.
func fatalOf(err error) (code, message string) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Severity == "FATAL" {
		return pg.Code, pg.Message
	}
	return "", ""
}

// The listener asks a client to prove it knows the token with SCRAM-SHA-256 and never for the token itself: a
// cleartext password is refused, as are replication, a wrong token and direct TLS, and none opens a WebSocket. A
// process squatting the remembered port while canyon db connect is not running so learns nothing it can reuse.
func TestOnlySCRAMAndNoneOfTheRefusedReachCanyon(t *testing.T) {
	h := newRelayHost(t)
	_, addr, _ := listening(t, h)
	_, fe := raw(t, addr)
	send(t, fe, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "dana"}})
	if m, ok := receive(t, fe).(*pgproto3.AuthenticationSASL); !ok || !reflect.DeepEqual(m.AuthMechanisms, []string{"SCRAM-SHA-256"}) {
		t.Fatalf("asked %#v, not SCRAM-SHA-256 alone", m)
	}
	send(t, fe, &pgproto3.PasswordMessage{Password: testToken})
	refusedBefore(t, h, fe, codeAuth, fatalPassword)
	_, fe = raw(t, addr)
	send(t, fe, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30, Parameters: map[string]string{"user": "dana", "replication": "database"}})
	refusedBefore(t, h, fe, codeRefused, fatalReplication)
	conn, fe := raw(t, addr)
	if _, err := conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05}); err != nil {
		t.Fatal(err)
	}
	refusedBefore(t, h, fe, codeRefused, fatalDirect)
	_, err := signIn(t, addr, strings.Replace(testToken, "0", "1", 1), nil)
	if code, message := fatalOf(err); code != codeAuth || message != "Canyon: "+fatalPassword {
		t.Errorf("a wrong token: %v", err)
	}
	if seen := h.connectsSeen(); len(seen) != 0 {
		t.Errorf("Canyon was asked to connect: %q", seen)
	}
}

// The client's parameters reach Canyon but user and database, which are the database session's; Canyon's startup
// reaches the client, and the listener names the connection and its backend. A cancel is forwarded only with the key
// the listener gave that connection: any other local process's guess is dropped, unsaid.
func TestARelayForwardsTheStartupAndOnlyItsOwnCancel(t *testing.T) {
	h := newRelayHost(t)
	_, addr, out := listening(t, h)
	conn, err := signIn(t, addr, testToken, map[string]string{"application_name": "psql", "options": "-c search_path=claims", "require_auth": "scram-sha-256"})
	if err != nil {
		t.Fatal(err)
	}
	if seen := h.connectsSeen(); len(seen) != 1 || seen[0] != `{"application_name":"psql","options":"-c search_path=claims"}` {
		t.Errorf("Canyon was asked %q", seen)
	}
	if !eventually(t, func() bool { return strings.Contains(out.String(), "  connection 1 opened (psql) · backend 48213\n") }) {
		t.Errorf("the listener said\n%s", out)
	}
	cancels := func() []string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]string{}, h.cancels...)
	}
	for _, key := range [][]byte{make([]byte, 4), {1, 2, 3, 4}} { // Canyon's own zero key, and a guess
		_, fe := raw(t, addr)
		send(t, fe, &pgproto3.CancelRequest{ProcessID: 48213, SecretKey: key})
	}
	time.Sleep(100 * time.Millisecond)
	if got := cancels(); len(got) != 0 {
		t.Fatalf("a cancel without the connection's key was forwarded: %q", got)
	}
	if err := conn.CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, func() bool { got := cancels(); return len(got) == 1 && got[0] == `{"process_id":48213}` }) {
		t.Errorf("cancels %q", cancels())
	}
}

// How a connection ends (§9): an ended database session's 410 is the client's FATAL and ends the run with its reason;
// a close with a reason ends it the same; Canyon restarting, or failing to check, keeps listening and says so.
func TestARelaysEndSaysWhy(t *testing.T) {
	for _, c := range []struct {
		name   string
		refuse *reply
		close  *websocket.CloseError
		ended  string
		says   string
	}{
		{"410", &reply{http.StatusGone, `{"error":"session_ended","message":"this database session has ended","reason":"idle"}`}, nil, "idle", ""},
		{"held", nil, &websocket.CloseError{Code: dbClosed, Reason: "held"}, "held", ""},
		{"restarting", nil, &websocket.CloseError{Code: dbClosed, Reason: "canyon_restarting"}, "", "· Canyon is restarting — still listening; reconnect your client.\n"},
		{"unchecked", nil, &websocket.CloseError{Code: websocket.StatusInternalError}, "", "· A connection failed: Canyon couldn't check the database session. Reconnect your client.\n"},
		{"unpinged", nil, &websocket.CloseError{Code: websocket.StatusGoingAway}, "", "· The connection to Canyon dropped: reconnect your client.\n"},
		{"too big", nil, &websocket.CloseError{Code: websocket.StatusMessageTooBig}, "", "· A message was over 1 MiB, Canyon's limit: the connection closed.\n"},
		{"refused", &reply{http.StatusTooManyRequests, `{"error":"busy","message":"You already have 10 database connections open: close one and try again."}`}, nil, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRelayHost(t)
			h.refuse, h.closeWith = c.refuse, c.close
			l, addr, out := listening(t, h)
			_, err := signIn(t, addr, testToken, nil)
			if c.refuse != nil {
				var body struct{ Message string }
				_ = json.Unmarshal([]byte(c.refuse.body), &body)
				want := body.Message
				if c.ended != "" {
					want = fatalEnded
				}
				if _, message := fatalOf(err); message != "Canyon: "+want {
					t.Errorf("the client read %v", err)
				}
			}
			var ended string
			select {
			case ended = <-l.ended:
			case <-time.After(time.Second):
			}
			if ended != c.ended || c.says != "" && !eventually(t, func() bool { return strings.Contains(out.String(), c.says) }) {
				t.Errorf("ended %q, said\n%s", ended, out)
			}
		})
	}
}

// A host that upgrades without canyon-db.1 is one from before the relay: the client is told it needs updating.
func TestAHostWithoutTheRelaySaysItNeedsUpdating(t *testing.T) {
	h := newRelayHost(t)
	h.older = true
	_, addr, _ := listening(t, h)
	_, err := signIn(t, addr, testToken, nil)
	if _, message := fatalOf(err); message != "Canyon: this Canyon doesn't relay database connections yet; it needs updating." {
		t.Errorf("the client read %v", err)
	}
}

// Each end's line (§9), by its close_reason.
func TestEachEndHasItsLine(t *testing.T) {
	h := newRelayHost(t)
	l, _, _ := listening(t, h)
	host := strings.TrimPrefix(h.server.URL, "http://")
	for reason, want := range map[string]string{
		"signed_out":      "Database session ended: your Canyon sign-in expired. Run: canyon login " + host,
		"ended":           "Database session ended by an owner or admin of claims-portal.",
		"idle":            "Database session ended after 30 min idle.",
		"closed":          "Database session ended: you ended it.",
		"expired":         "Database session ended: it reached its time limit, 8 h.",
		"access_changed":  "Database session ended: your access to claims-portal changed.",
		"setting_changed": "Database session ended: an admin changed Settings › Production data.",
		"database_gone":   "Database session ended: the app's database isn't available any more.",
		"held":            "Database session ended: production was put on hold.",
		"replaced":        "Database session ended: a read-write session replaced it.",
		"turned_off":      "Database session ended: an admin turned database sessions off.",
		"a_newer_reason":  "Database session ended for a reason this canyon doesn't know: update the CLI.",
	} {
		if got := l.endLine(reason); got != want {
			t.Errorf("%s: %q, want %q", reason, got, want)
		}
	}
}

// The verb end to end: the session opened, the listener on the remembered port with the remembered token, the
// lines of §9 byte for byte, a connection, and an end that exits 1.
func TestDBConnectListensAndEnds(t *testing.T) {
	h := newRelayHost(t)
	h.closeWith = &websocket.CloseError{Code: dbClosed, Reason: "idle"}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	config := t.TempDir()
	env := map[string]string{"CANYON_HOST": h.server.URL, "XDG_CONFIG_HOME": config, "NO_COLOR": "1"}
	path := dbLocalPath(func(k string) string { return env[k] }, h.server.URL, "claims-portal", "preview")
	writeFile(t, path, `{"token":"`+testToken+`","port":`+strconv.Itoa(port)+`}`)
	type result struct {
		out, errs string
		code      int
	}
	done := make(chan result, 1)
	go func() {
		out, errs, code := run(t, env, false, "db", "connect", "--app", "claims-portal")
		done <- result{out, errs, code}
	}()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}) {
		t.Fatal("nothing listens on the remembered port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := pgconn.Connect(ctx, "postgres://dana:"+testToken+"@"+addr+"/claims-portal?sslmode=require"); err == nil {
		_ = conn.Close(ctx)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end when its database session did")
	}
	head, _, _ := strings.Cut(r.out, "\n· ")
	var s dbSession
	_ = json.Unmarshal(h.session, &s)
	expires, _ := time.Parse(time.RFC3339, s.ExpiresAt)
	until := expires.Local().Format("15:04") // the session's expires_at, in this machine's zone
	want := strings.NewReplacer("<port>", strconv.Itoa(port), "<token>", testToken, "<until>", until).Replace(golden(t, "db-connect.txt"))
	if r.code != exitError || head != want {
		t.Errorf("exit %d, printed\n%s\ntestdata/db-connect.txt wants\n%s", r.code, r.out, want)
	}
	if !strings.HasSuffix(r.errs, "\n✗ Database session ended after 30 min idle.\n") || !strings.Contains(r.out, "connection 1 opened · backend 48213\n") {
		t.Errorf("stdout\n%s\nstderr\n%s", r.out, r.errs)
	}
	if body, err := os.ReadFile(filepath.Clean(path)); err != nil || !bytes.Contains(body, []byte(testToken)) {
		t.Errorf("the token was not kept: %s %v", body, err)
	}
}

// --new-token replaces the remembered token before the listener starts and keeps the port (#2841): the old token
// is refused as any wrong one is, and nothing reaches Canyon for it; the new one, which the printed URL carries and
// the file keeps, signs in; and the run says so.
func TestNewTokenRefusesTheOldOne(t *testing.T) {
	h := newRelayHost(t)
	h.closeWith = &websocket.CloseError{Code: dbClosed, Reason: "idle"}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	env := map[string]string{"CANYON_HOST": h.server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	path := dbLocalPath(func(k string) string { return env[k] }, h.server.URL, "claims-portal", "preview")
	writeFile(t, path, `{"token":"`+testToken+`","port":`+strconv.Itoa(port)+`}`)
	type result struct {
		out, errs string
		code      int
	}
	done := make(chan result, 1)
	go func() {
		out, errs, code := run(t, env, false, "db", "connect", "--app", "claims-portal", "--new-token")
		done <- result{out, errs, code}
	}()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}) {
		t.Fatal("nothing listens on the remembered port")
	}
	var kept dbLocal
	eventually(t, func() bool { // the file is written just after the listener starts
		body, err := os.ReadFile(filepath.Clean(path))
		return err == nil && json.Unmarshal(body, &kept) == nil && kept.Token != testToken
	})
	if kept.Token == testToken || len(kept.Token) != 40 || kept.Port != port {
		t.Errorf("kept token changed %v, length %d, port %d (want %d)", kept.Token != testToken, len(kept.Token), kept.Port, port)
	}
	_, err = signIn(t, addr, testToken, nil)
	if code, message := fatalOf(err); code != codeAuth || message != "Canyon: "+fatalPassword {
		t.Errorf("the old token: %v", err)
	}
	if seen := h.connectsSeen(); len(seen) != 0 {
		t.Errorf("Canyon was asked to connect for the old token: %q", seen)
	}
	if _, err := signIn(t, addr, kept.Token, nil); err != nil {
		t.Errorf("the new token: %v", err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end when its database session did")
	}
	if !strings.Contains(r.out, "· New token: update the password in your saved connection.\n") || !strings.Contains(r.out, ":"+kept.Token+"@") || strings.Contains(r.out, testToken) {
		t.Errorf("printed\n%s", r.out)
	}
}

// The ai block says a replaced token, so an agent can tell the person to update their saved connection (#2841's
// review), and says nothing of a kept one.
func TestTheAIBlockSaysANewToken(t *testing.T) {
	for _, renewed := range []bool{false, true} {
		var out strings.Builder
		l := &dbListener{a: &app{out: &out, err: &out, render: Render{Format: FormatAI}}, app: "claims-portal"}
		l.opened(dbOpened{}, "postgres://dana:<token>@127.0.0.1:54329/claims-portal", 54329, 0, false, renewed)
		if said := strings.Contains(out.String(), "\ntoken: new\n"); said != renewed {
			t.Errorf("--new-token %v: said %v\n%s", renewed, said, out.String())
		}
	}
}

// connecting runs db connect with args in the background, and answers the address the run says it listens on, read from
// its own line once it prints it, its output in out and its exit code on done. Never a port found to answer: one near
// another may be any process's (#2929).
func connecting(t *testing.T, env map[string]string, args ...string) (addr string, out *lockedBuffer, done chan int) {
	t.Helper()
	out, done = &lockedBuffer{}, make(chan int, 1)
	go func() {
		done <- Main(append([]string{"db", "connect", "--app", "claims-portal"}, args...), out, &lockedBuffer{}, strings.NewReader(""), func(k string) string { return env[k] }, false)
	}()
	const said = "Listening on "
	if !eventually(t, func() bool {
		_, rest, ok := strings.Cut(out.String(), said)
		end := strings.IndexAny(rest, " \n")
		if !ok || end < 0 {
			return false
		}
		addr = rest[:end]
		return true
	}) {
		t.Fatalf("the run says it listens nowhere:\n%s", out)
	}
	return addr, out, done
}

// takenPair holds the remembered port and, beside it, the next one, as another process on the machine may (#2929):
// the next one answers each connection by closing it. It answers the remembered port's listener and its port.
func takenPair(t *testing.T) (net.Listener, int) {
	t.Helper()
	for range 50 {
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := held.Addr().(*net.TCPAddr).Port
		other, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port+1))
		if err != nil {
			_ = held.Close()
			continue
		}
		t.Cleanup(func() { _ = other.Close() })
		go func() {
			for {
				c, err := other.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		return held, port
	}
	t.Fatal("no two free ports side by side")
	return nil, 0
}

// A run whose remembered port is taken listens on another for that run alone, and says so, but the port it remembers
// stays (#2841's review): a saved connection reaches the remembered port again once whatever held it lets go.
func TestATakenPortIsNotRemembered(t *testing.T) {
	h := newRelayHost(t)
	h.closeWith = &websocket.CloseError{Code: dbClosed, Reason: "idle"}
	held, port := takenPair(t)
	defer func() { _ = held.Close() }()
	env := map[string]string{"CANYON_HOST": h.server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	path := dbLocalPath(func(k string) string { return env[k] }, h.server.URL, "claims-portal", "preview")
	writeFile(t, path, `{"token":"`+testToken+`","port":`+strconv.Itoa(port)+`}`)
	addr, out, done := connecting(t, env)
	if _, err := signIn(t, addr, testToken, nil); err != nil {
		t.Errorf("the token: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end when its database session did")
	}
	var kept dbLocal
	if body, err := os.ReadFile(filepath.Clean(path)); err != nil || json.Unmarshal(body, &kept) != nil || kept.Port != port || kept.Token != testToken {
		t.Errorf("remembered port %d, token kept %v, want port %d (%v)", kept.Port, kept.Token == testToken, port, err)
	}
	_, moved, _ := strings.Cut(addr, ":")
	if want := "· Port " + strconv.Itoa(port) + " is taken: listening on " + moved + " for this run only.\n"; !strings.Contains(out.String(), want) {
		t.Errorf("printed\n%s\nwant the line %q", out, want)
	}
}

// A run already listening takes a --new-token made by another at its next sign-in (#2841's review): the second run
// moves to the next port, and the first refuses the old token from then on and takes the new one.
func TestARunningListenerRefusesTheOldTokenAfterARotation(t *testing.T) {
	h := newRelayHost(t)
	h.closeWith = &websocket.CloseError{Code: dbClosed, Reason: "idle"}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	env := map[string]string{"CANYON_HOST": h.server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	path := dbLocalPath(func(k string) string { return env[k] }, h.server.URL, "claims-portal", "preview")
	writeFile(t, path, `{"token":"`+testToken+`","port":`+strconv.Itoa(port)+`}`)
	first, _, firstDone := connecting(t, env)
	second, _, secondDone := connecting(t, env, "--new-token")
	var kept dbLocal
	if !eventually(t, func() bool {
		body, err := os.ReadFile(filepath.Clean(path))
		return err == nil && json.Unmarshal(body, &kept) == nil && kept.Token != testToken
	}) {
		t.Fatal("the second run kept the old token")
	}
	_, err = signIn(t, first, testToken, nil)
	if code, message := fatalOf(err); code != codeAuth || message != "Canyon: "+fatalPassword {
		t.Errorf("the first run took the old token after a rotation: %v", err)
	}
	for _, addr := range []string{first, second} {
		if _, err := signIn(t, addr, kept.Token, nil); err != nil {
			t.Errorf("%s refused the new token: %v", addr, err)
		}
	}
	for _, done := range []chan int{firstDone, secondDone} { // each run ends with the session its connection closed
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a run did not end when its database session did")
		}
	}
}

// A run ends with its database session, connected or not (#2131's review): at expires_at on its own timer, and,
// read every poll, when the host says it closed and why, or that the sign-in expired; while it stays open, the run
// goes on.
func TestTheRunEndsWithItsSession(t *testing.T) {
	for _, c := range []struct {
		name    string
		expires time.Duration
		reads   *reply
		ended   string
	}{
		{"expired", 200 * time.Millisecond, nil, "expired"},
		{"ended", time.Hour, &reply{http.StatusOK, `{"close_reason":"ended","closed_at":"2026-11-12T10:00:00Z"}`}, "ended"},
		{"signed out", time.Hour, &reply{http.StatusUnauthorized, `{"error":"unauthorized"}`}, "signed_out"},
		{"open", time.Hour, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRelayHost(t)
			h.reads = c.reads
			l, _, _ := listening(t, h)
			l.session.ExpiresAt = time.Now().Add(c.expires).UTC().Format(time.RFC3339Nano)
			l.poll = 50 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go l.watch(ctx)
			var ended string
			select {
			case ended = <-l.ended:
			case <-time.After(time.Second):
			}
			if ended != c.ended {
				t.Errorf("the run ended %q, want %q", ended, c.ended)
			}
		})
	}
}

// The relay reads a message of up to 1 MiB, the contract's, and closes 1009 on a bigger one (#2131's review):
// websocket.NetConn lifts every limit, so relayPipe sets it back.
func TestTheRelayReadsUpTo1MiB(t *testing.T) {
	closed := make(chan websocket.StatusCode, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Write(r.Context(), websocket.MessageBinary, bytes.Repeat([]byte{'d'}, dbMessageMost))
		_ = ws.Write(r.Context(), websocket.MessageBinary, bytes.Repeat([]byte{'d'}, dbMessageMost+1))
		_, _, err = ws.Read(r.Context())
		closed <- websocket.CloseStatus(err)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	relayPipe(ctx, ws)
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	got, err := io.ReadAll(io.LimitReader(&wsReader{ctx: rctx, ws: ws}, 3*dbMessageMost))
	if len(got) < dbMessageMost || err == nil && len(got) > dbMessageMost {
		t.Errorf("read %d bytes (%v); want the 1 MiB message and no more", len(got), err)
	}
	select {
	case code := <-closed:
		if code != websocket.StatusMessageTooBig {
			t.Errorf("a message over 1 MiB closed %d, want 1009", code)
		}
	case <-time.After(3 * time.Second):
		t.Error("a message over 1 MiB was read")
	}
}

// FuzzStartupMessage holds startupOf to §3 over any bytes a client sends as its startup: what does not decode is
// never a startup, and one that does forwards no user, database or replication in any case and no _pq_. option,
// declines only _pq_. options, and reaches Canyon as it was read.
func FuzzStartupMessage(f *testing.F) {
	for _, params := range []map[string]string{
		{"user": "dana", "database": "claims", "application_name": "psql", "client_encoding": "UTF8"},
		{"User": "dana", "DATABASE": "x", "options": "-c search_path=claims"},
		{"_pq_.protocol_managed_params": "1", "replication": "database"},
		{"Replication": "true"},
		{"user": "dana", "require_auth": "scram-sha-256", "Require_Auth": "password"},
		{"Application_Name": "psql", "application_name": "pgcli", "TimeZone": "UTC", "timezone": "CET"},
	} {
		for _, version := range []uint32{pgproto3.ProtocolVersion30, pgproto3.ProtocolVersion32} {
			b, _ := (&pgproto3.StartupMessage{ProtocolVersion: version, Parameters: params}).Encode(nil)
			f.Add(b)
			f.Add(b[:len(b)-1]) // its last NUL gone: malformed
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := pgproto3.NewBackend(bytes.NewReader(data), io.Discard).ReceiveStartupMessage()
		m, ok := msg.(*pgproto3.StartupMessage)
		if err != nil || !ok {
			return
		}
		s := startupOf(m)
		if s.fatal != "" {
			return
		}
		for name := range s.forward {
			if name != strings.ToLower(name) { // the server takes each name once, in lower case (contracts/routes.md, the connect row)
				t.Fatalf("forwarded %q, not in lower case, from %q", name, data)
			}
			if lower := strings.ToLower(name); lower == "user" || lower == "database" || lower == "replication" || lower == "require_auth" || strings.HasPrefix(lower, "_pq_.") {
				t.Fatalf("forwarded %q from %q", name, data)
			}
		}
		for _, name := range s.declined {
			if !strings.HasPrefix(strings.ToLower(name), "_pq_.") {
				t.Fatalf("declined %q, which is not a protocol option", name)
			}
		}
		param, err := startupParam(s.forward)
		var back map[string]string
		raw, _ := base64.RawURLEncoding.DecodeString(param)
		if err != nil || json.Unmarshal(raw, &back) != nil || len(back) != len(s.forward) {
			t.Fatalf("the startup %q does not reach Canyon as read: %v", param, err)
		}
	})
}

// lockedLines is a stream a test reads while a run may still write it.
type lockedLines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *lockedLines) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedLines) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// A run ends once every connection it carried has ended and said its last line: one still open when the database
// session ends is closed by the run, never left writing after the run returned, its closed line lost to the process
// exiting (#2921). Here the session expires while a client is connected, which nothing else closes.
func TestDBConnectEndsAfterItsConnectionsLastLines(t *testing.T) {
	h := newRelayHost(t) // no closeWith: Canyon keeps the connection open until the client ends it
	expires := time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339Nano)
	h.session = []byte(regexp.MustCompile(`"expires_at": "[^"]*"`).ReplaceAllString(string(h.session), `"expires_at": "`+expires+`"`))
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	env := map[string]string{"CANYON_HOST": h.server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	writeFile(t, dbLocalPath(func(k string) string { return env[k] }, h.server.URL, "claims-portal", "preview"), `{"token":"`+testToken+`","port":`+strconv.Itoa(port)+`}`)
	out, errs := &lockedLines{}, &lockedLines{}
	done := make(chan string, 1)
	go func() {
		Main([]string{"db", "connect", "--app", "claims-portal"}, out, errs, strings.NewReader(""), func(k string) string { return env[k] }, false)
		done <- out.String() // stdout as it stands the moment the run returns
	}()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if !eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}) {
		t.Fatal("nothing listens on the remembered port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://dana:"+testToken+"@"+addr+"/claims-portal?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // held open past the session's end: the run must close it
	select {
	case returned := <-done:
		if !strings.Contains(returned, "connection 1 opened") || !strings.Contains(returned, "connection 1 closed") {
			t.Errorf("the run returned with connection 1 still open, its closed line not said: stdout\n%s\nstderr\n%s", returned, errs.String())
		}
		if !strings.Contains(errs.String(), "Database session ended") {
			t.Errorf("the run's end line: stderr\n%s", errs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end when its database session expired")
	}
}
