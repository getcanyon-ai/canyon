package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// The database relay (design/DATABASE-SESSIONS.md §3, §6; contracts/routes.md, Database sessions; #2129): one
// WebSocket per Postgres connection of `canyon db connect`, carrying the person's own bytes to a backend logged in
// as their role. The client never names a host, port, database or user: the target is the database session's.

const (
	dbSubprotocol      = "canyon-db.1"
	dbPing             = 20 * time.Second // an ALB idles a socket out at 60 s
	dbRelayCheck       = 15 * time.Second // how often a replica re-reads the rows and sign-ins of the relays it carries
	dbHostConnections  = 100              // CANYON_DB_MAX_CONNECTIONS's default: every relay on this server
	dbPersonRelays     = 10               // one person's relays on this server
	dbStartupMost      = 4 << 10          // the startup parameter, encoded
	dbStartupNames     = 64
	dbStartupValueMost = 1024
	dbMessageMost      = 1 << 20                    // a binary message from the client: canyon db connect frames to it
	dbClosed           = websocket.StatusCode(4000) // a relay Canyon ends: its reason a close_reason, or canyon_restarting
)

// dbStartupName is a startup parameter's name as the contract allows it (db-session.schema.json, startup).
var dbStartupName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,62}$`)

// errStartup is a startup the relay will not forward; its message is the refusal's.
var errStartup = errors.New("the client's startup parameters can't be relayed: user, database and replication are the database session's")

// decodeStartup is the client's startup parameters, as GET …/connect?startup= carries them: base64url JSON of one
// object of strings, at most 4 KiB encoded, at most 64 names. A name outside the contract's pattern, a protocol
// option (_pq_.), a value over 1,024 characters, one that is not UTF-8 or holds a NUL, or user, database or replication in any
// case, is errStartup: nothing is dialled for it.
func decodeStartup(raw string) (map[string]string, error) {
	if len(raw) > dbStartupMost {
		return nil, errStartup
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	if err != nil {
		return nil, errStartup
	}
	var params map[string]string
	if err := json.Unmarshal(b, &params); err != nil || len(params) > dbStartupNames {
		return nil, errStartup
	}
	seen := make(map[string]bool, len(params))
	for name, value := range params {
		// Postgres reads a name in any case, so two names the same but for case are one set twice, and
		// application_name in another case would overwrite the canyon:<sid>:<tool> dbConnect sets.
		switch lower := strings.ToLower(name); {
		case !dbStartupName.MatchString(name), strings.HasPrefix(lower, "_pq_."),
			lower == "user", lower == "database", lower == "replication",
			seen[lower], lower == "application_name" && name != lower,
			!utf8.ValidString(value), utf8.RuneCountInString(value) > dbStartupValueMost, strings.ContainsRune(value, 0):
			return nil, errStartup
		default:
			seen[lower] = true
		}
	}
	if params == nil {
		params = map[string]string{}
	}
	return params, nil
}

// syntheticStartup is what the client reads as its connection's start, once Canyon has logged in as the person:
// AuthenticationOk, the backend's parameters in name order, its key data — the pid the CLI's cancel names, and no
// secret, which no cancel needs (§3) — and ReadyForQuery in the backend's transaction status.
func syntheticStartup(hj *pgconn.HijackedConn) ([]byte, error) {
	out, err := (&pgproto3.AuthenticationOk{}).Encode(nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(hj.ParameterStatuses))
	for name := range hj.ParameterStatuses {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if out, err = (&pgproto3.ParameterStatus{Name: name, Value: hj.ParameterStatuses[name]}).Encode(out); err != nil {
			return nil, err
		}
	}
	if out, err = (&pgproto3.BackendKeyData{ProcessID: hj.PID, SecretKey: make([]byte, 4)}).Encode(out); err != nil {
		return nil, err
	}
	return (&pgproto3.ReadyForQuery{TxStatus: hj.TxStatus}).Encode(out)
}

// dbRelay is one live relay.
type dbRelay struct {
	sid, subject, signin, tool string
	key                        provisioners.Key
	person                     provisioners.Person
	issuer                     provisioners.Issuer
	n                          int64 // the connection's number in its database session
	pid                        uint32
	opened                     time.Time
	ends                       time.Time // when the database session or the sign-in that connected ends, the first
	endsAs                     string    // and which: expired or signed_out
	fromDB, toDB               atomic.Int64
	flushedFrom, flushedTo     int64

	mu      sync.Mutex
	endFn   func(code websocket.StatusCode, reason, cause string, terminate bool) // nil until the relay runs
	pending func()                                                                // an end asked for before it ran
}

// end ends the relay: at once while it runs, and as it starts when asked before, so a relay admitted while the
// server shuts down or a check fails never runs.
func (r *dbRelay) end(code websocket.StatusCode, reason, cause string, terminate bool) {
	r.mu.Lock()
	fn := r.endFn
	if fn == nil && r.pending == nil {
		r.pending = func() { r.end(code, reason, cause, terminate) }
	}
	r.mu.Unlock()
	if fn != nil {
		fn(code, reason, cause, terminate)
	}
}

// dbRelays is a replica's live relays, and the caps judged under its lock (§6, Caps): every connection this server
// opens to an apps database for a person while it is open, a relay's and a query's, counts toward the host's and the
// person's.
type dbRelays struct {
	mu        sync.Mutex
	live      map[*dbRelay]bool
	queries   int            // the queries whose one connection is open now
	perPerson map[string]int // a person's relays and queries
	closing   bool           // the server is shutting down: no relay starts
}

// room is the cap one more connection of subject's would pass, "" when it fits: the person's, then the host's,
// which a server shutting down is at. Under rs.mu.
func (rs *dbRelays) room(subject string, host int) string {
	if rs.live == nil {
		rs.live, rs.perPerson = map[*dbRelay]bool{}, map[string]int{}
	}
	switch {
	case rs.perPerson[subject] >= dbPersonRelays:
		return "person"
	case rs.closing || len(rs.live)+rs.queries >= host:
		return "host"
	}
	return ""
}

// admit reserves a relay for subject, or names the cap it would pass (room).
func (rs *dbRelays) admit(r *dbRelay, host int) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if over := rs.room(r.subject, host); over != "" {
		return over
	}
	rs.live[r] = true
	rs.perPerson[r.subject]++
	return ""
}

// reserve holds one connection of subject's for a query, its release once that connection is closed, or names the
// cap it would pass (room) and holds nothing.
func (rs *dbRelays) reserve(subject string, host int) (release func(), over string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if over := rs.room(subject, host); over != "" {
		return nil, over
	}
	rs.queries++
	rs.perPerson[subject]++
	var once sync.Once
	return func() {
		once.Do(func() {
			rs.mu.Lock()
			defer rs.mu.Unlock()
			rs.queries--
			rs.perPerson[subject]--
		})
	}, ""
}

// busySaid is 429 busy's message for the cap a connection would pass (contracts/routes.md, the connect and query rows).
func busySaid(over string, host int) string {
	if over == "person" {
		return "You already have " + strconv.Itoa(dbPersonRelays) + " database connections open: close one and try again."
	}
	return "Canyon is at its limit of " + strconv.Itoa(host) + " database connections: try again in a minute."
}

func (rs *dbRelays) release(r *dbRelay) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.live[r] {
		delete(rs.live, r)
		rs.perPerson[r.subject]--
	}
}

// all is a copy of the live relays, so none is ended under the lock.
func (rs *dbRelays) all() []*dbRelay {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]*dbRelay, 0, len(rs.live))
	for r := range rs.live {
		out = append(out, r)
	}
	return out
}

// shutdown refuses every relay to come and ends every live one, close code 4000 canyon_restarting: the database
// sessions survive the restart, and the CLI keeps listening (§6, Replicas).
func (a *app) shutdownRelays() {
	a.relays.mu.Lock()
	a.relays.closing = true
	a.relays.mu.Unlock()
	for _, r := range a.relays.all() {
		r.end(dbClosed, "canyon_restarting", "terminated", false)
	}
}

func routesDBRelay(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /db-sessions/{sid}", a.getDBSession)
	api.HandleFunc("GET /db-sessions/{sid}/connect", a.connectDBSession)
	api.HandleFunc("POST /db-sessions/{sid}/cancel", a.cancelDBStatement)
}

// getDBSession answers the caller's own database session, open or closed, with closed_at and close_reason once
// closed: how canyon db connect, no client connected, learns it ended and why (contracts/routes.md; #2129).
func (a *app) getDBSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, reason, closedAt := a.dbOwn(w, r)
	if s == nil {
		return
	}
	out := struct {
		dbSessionRead
		ClosedAt    string `json:"closed_at,omitempty"`
		CloseReason string `json:"close_reason,omitempty"`
	}{dbSessionRead: s.read(ctx), CloseReason: reason}
	if closedAt != nil {
		out.ClosedAt = closedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// dbOwn is the database session sid when it is the caller's own, which a read, a relay and a cancel are alone, with
// why and when it closed, read in the same row; nil with the 404 written for anyone else, an owner or an admin
// included, and for no such session.
func (a *app) dbOwn(w http.ResponseWriter, r *http.Request) (*dbRow, string, *time.Time) {
	ctx, sid := r.Context(), r.PathValue("sid")
	var s dbRow
	var reason *string
	var closedAt *time.Time
	err := a.pool.QueryRow(ctx, "SELECT "+dbColumns+", close_reason, closed_at FROM db_sessions WHERE id = $1", sid).Scan(append(s.dest(), &reason, &closedAt)...)
	act := actorOf(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && (act.Kind != "person" || act.Subject != s.subject):
		writeErr(w, http.StatusNotFound, "not_found", "no such database session", nil)
		return nil, "", nil
	case err != nil:
		a.log.Printf("canyon-server: database session %s: %v", sid, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return nil, "", nil
	}
	if reason == nil {
		return &s, "", closedAt
	}
	return &s, *reason, closedAt
}

// connectDBSession relays one Postgres connection of the caller's own open database session. The refusals come in
// the contract's order, each before anything is dialled: a browser, the session, the startup, the caps, the
// checks. Then, under the person role's lock, one login, its connection hijacked and handed to the WebSocket.
func (a *app) connectDBSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Header.Get("Origin") != "" { // the CLI sends none; a browser always does, and its page could ride the person's cookie
		writeErr(w, http.StatusForbidden, "cross_origin", "a browser cannot connect to a database session: use canyon db connect", nil)
		return
	}
	s, ended, _ := a.dbOwn(w, r)
	if s == nil || !a.dbSessionsAllowed(w, r) {
		return
	}
	if ended != "" {
		writeErr(w, http.StatusGone, "session_ended", "this database session has ended", map[string]any{"reason": ended})
		return
	}
	params, err := decodeStartup(r.URL.Query().Get("startup"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_startup", err.Error(), nil)
		return
	}
	if s.mode == "read" { // a default the client can SET off, not a guard: the read group's grants enforce read (§4)
		for name := range params {
			if strings.EqualFold(name, "default_transaction_read_only") {
				delete(params, name)
			}
		}
		params["default_transaction_read_only"] = "on"
	}
	relay := &dbRelay{sid: s.id, subject: s.subject, signin: signinOf(ctx), key: s.key(), person: s.person(), tool: dbTool(params["application_name"]), opened: time.Now(),
		ends: s.expiresAt, endsAs: "expired"}
	if a.auth == authOIDC && relay.signin != "" {
		var signedIn time.Time
		if err := a.pool.QueryRow(ctx, "SELECT expires_at FROM sessions WHERE id = $1", relay.signin).Scan(&signedIn); err != nil {
			a.internal(w, "database session "+s.id, err)
			return
		}
		if signedIn.Before(relay.ends) {
			relay.ends, relay.endsAs = signedIn, "signed_out"
		}
	}
	if over := a.relays.admit(relay, a.dbHostCap()); over != "" {
		writeErr(w, http.StatusTooManyRequests, "busy", busySaid(over, a.dbHostCap()), nil)
		return
	}
	admitted := true
	defer func() {
		if admitted { // refused below, before any relay ran
			a.relays.release(relay)
		}
	}()
	if reason, err := a.dbCheck(ctx, *s, time.Time{}); err != nil || reason != "" {
		if reason != "" && err == nil {
			_ = a.closeDB(ctx, *s, reason, canyonDB, false)
			writeErr(w, http.StatusGone, "session_ended", "this database session has ended", map[string]any{"reason": reason})
			return
		}
		a.log.Printf("canyon-server: database session %s: its checks: %v", s.id, err) // Canyon's own read, never the database's dial (#2865)
		writeErr(w, http.StatusServiceUnavailable, "check_failed", dbConnectUnchecked, nil)
		return
	}
	backend, err := a.relayLogin(ctx, *s, relay, params)
	if err != nil {
		a.log.Printf("canyon-server: database session %s: the relay's login: %v", s.id, err)
		if errors.Is(err, provisioners.ErrUnchecked) { // a read check couldn't read what it judges: nothing dialled (#2865)
			writeErr(w, http.StatusServiceUnavailable, "check_failed", dbConnectUnchecked, nil)
			return
		}
		if code, said := loginRefusal(err, s.project, s.env, s.resource); code != "" { // the open's own refusals, never database_unreachable
			writeErr(w, http.StatusConflict, code, said, nil)
			return
		}
		writeErr(w, http.StatusBadGateway, "database_unreachable", "Canyon couldn't reach the database of "+s.project+" in "+s.env+": try again.", nil)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{dbSubprotocol}})
	if err != nil || ws.Subprotocol() != dbSubprotocol {
		_ = backend.Conn.Close()
		a.terminateBackend(relay)
		if err == nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "canyon-db.1 is the one subprotocol")
		}
		return
	}
	admitted = false
	a.relayRun(ws, backend, relay)
}

// dbTool is the client's application_name as the backend's own names it, canyon:<sid>:<tool>: its letters, digits,
// dots, dashes and underscores, at most 32, else "client".
func dbTool(name string) string {
	b := []byte{}
	for _, c := range []byte(name) {
		if len(b) < 32 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return "client"
	}
	return string(b)
}

// relayLogin logs in as the person, under their role's lock with the open row re-read, and hijacks the connection. A
// refusal is err, which the caller answers as the open does (loginRefusal): a read login's role that could change data
// or open a second connection is *ReadOnlyUnsafe.
func (a *app) relayLogin(ctx context.Context, s dbRow, relay *dbRelay, params map[string]string) (*pgconn.HijackedConn, error) {
	var backend *pgconn.HijackedConn
	err := a.underRole(ctx, s.key(), s.subject, func(tx pgx.Tx) error {
		open, err := openRow(ctx, tx, s.key(), s.subject)
		if err != nil {
			return err
		}
		if open == nil || open.id != s.id { // closed or replaced since the checks: nothing logs in for it
			return errors.New("the database session closed meanwhile")
		}
		issuer, refusal, err := a.dbIssuer(ctx, tx, s.key())
		if err != nil || refusal != "" {
			return errors.Join(err, errors.New(refusal))
		}
		relay.issuer = issuer
		if err := issuer.Issue(ctx, s.key(), s.person(), provisioners.Mode(s.mode), dbLoginTTL, func(cctx context.Context, l provisioners.Login) error {
			if s.mode == "read" { // the client controls a relayed connection's transactions: only the role's rights hold it to read (#2826)
				if err := issuer.ReadOnly(cctx, l); err != nil {
					return err
				}
				if err := issuer.NoSecondConnection(cctx, l); err != nil { // its grants hold this connection alone (#2918)
					return err
				}
			}
			conn, err := dbConnect(cctx, l, s.id, relay.tool, params, &countingConn{}) // the relay counts its own bytes: this count is the query's
			if err != nil {
				return err
			}
			backend, err = hijacked(cctx, conn)
			return err
		}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "UPDATE db_sessions SET connections = connections + 1, last_activity_at = now(), relayed_by = nullif($2, '') WHERE id = $1 RETURNING connections", s.id, a.replica).Scan(&relay.n)
	})
	if err != nil {
		if backend != nil {
			_ = backend.Conn.Close()
		}
		return nil, err // a refusal is loginRefusal's, never database_unreachable (#2826, #2910, #2918)
	}
	relay.pid = backend.PID
	return backend, nil
}

// hijacked takes the logged-in connection over from pgx, its config dropped: that holds the login's password, which
// nothing needs once logged in, for the relay's whole life otherwise (#2129's review).
func hijacked(ctx context.Context, conn *pgx.Conn) (*pgconn.HijackedConn, error) {
	if err := conn.PgConn().SyncConn(ctx); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	hj, err := conn.PgConn().Hijack()
	if err != nil {
		return nil, err
	}
	hj.Config = nil
	return hj, nil
}

// relayRun carries bytes both ways until either end closes or Canyon ends the relay, pinging the client every
// dbPing, and writes the relay's one log line as it closes.
func (a *app) relayRun(ws *websocket.Conn, backend *pgconn.HijackedConn, relay *dbRelay) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var once sync.Once
	cause := make(chan string, 1)
	relay.mu.Lock()
	relay.endFn = func(code websocket.StatusCode, reason, why string, terminate bool) {
		once.Do(func() {
			cause <- why
			_ = ws.Close(code, reason)
			_ = backend.Conn.Close()
			if terminate {
				a.terminateBackend(relay)
			}
			stop()
		})
	}
	pending := relay.pending
	relay.mu.Unlock()
	if pending != nil {
		pending()
	}
	if !relay.ends.IsZero() { // the moment the database session or the sign-in ends, never a check's interval later
		end := time.AfterFunc(time.Until(relay.ends), func() { relay.end(dbClosed, relay.endsAs, "timer:"+relay.endsAs, true) })
		defer end.Stop()
	}
	first, err := syntheticStartup(backend)
	if err == nil {
		first, err = bufferedFrom(backend, first)
	}
	client := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	ws.SetReadLimit(dbMessageMost) // after NetConn, which lifts every limit: a bigger message closes 1009
	if err == nil {
		_, err = client.Write(first)
	}
	if err != nil {
		relay.end(websocket.StatusInternalError, "", "ws_close", true)
	} else {
		go func() { // to the database: the person's bytes
			_, err := io.Copy(backend.Conn, &countedReader{r: client, n: &relay.toDB})
			relay.end(websocket.StatusNormalClosure, "", map[bool]string{true: "client_eof", false: "ws_close"}[err == nil], true)
		}()
		go func() { // from the database
			_, err := io.Copy(client, &countedReader{r: backend.Conn, n: &relay.fromDB})
			relay.end(websocket.StatusNormalClosure, "", map[bool]string{true: "db_eof", false: "ws_close"}[err == nil], false)
		}()
		go func() {
			tick := time.NewTicker(dbPing)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					pctx, cancel := context.WithTimeout(ctx, dbPing)
					err := ws.Ping(pctx)
					cancel()
					if err != nil && ctx.Err() == nil {
						relay.end(websocket.StatusGoingAway, "", "ping_timeout", true)
						return
					}
				}
			}
		}()
	}
	<-ctx.Done()
	why := <-cause
	a.relays.release(relay)
	a.flushRelay(context.Background(), relay)
	a.log.Printf("canyon-server: database relay %s #%d: backend %d, application_name %q, %d bytes from the database, %d to it, %s, end_cause %s",
		relay.sid, relay.n, relay.pid, relay.tool, relay.fromDB.Load(), relay.toDB.Load(), time.Since(relay.opened).Round(time.Second), why)
}

// bufferedFrom appends what the hijacked frontend had already read from the backend, re-encoded, so no message the
// backend sent before the hijack is lost.
func bufferedFrom(backend *pgconn.HijackedConn, out []byte) ([]byte, error) {
	for backend.Frontend.ReadBufferLen() > 0 {
		msg, err := backend.Frontend.Receive()
		if err != nil {
			return nil, err
		}
		if out, err = msg.Encode(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// countedReader counts the bytes read through it.
type countedReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// terminateBackend ends the relay's backend and verifies it gone, on a context of its own (provisioners.Issuer).
func (a *app) terminateBackend(relay *dbRelay) {
	if relay.issuer == nil || relay.pid == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), provisioners.ConnectTimeout)
	defer cancel()
	if err := relay.issuer.Terminate(ctx, relay.key, relay.person, int(relay.pid)); err != nil {
		a.log.Printf("canyon-server: database relay %s #%d: ending its backend: %v", relay.sid, relay.n, err)
	}
}

// flushRelay adds what the relay carried since its last flush to its row, and its activity when it carried any.
func (a *app) flushRelay(ctx context.Context, relay *dbRelay) {
	from, to := relay.fromDB.Load(), relay.toDB.Load()
	df, dt := from-relay.flushedFrom, to-relay.flushedTo
	if df == 0 && dt == 0 {
		return
	}
	if _, err := a.pool.Exec(ctx, `UPDATE db_sessions SET bytes_from_db = bytes_from_db + $2, bytes_to_db = bytes_to_db + $3, last_activity_at = now() WHERE id = $1`, relay.sid, df, dt); err != nil {
		a.log.Printf("canyon-server: database relay %s: its counters: %v", relay.sid, err)
		return
	}
	relay.flushedFrom, relay.flushedTo = from, to
}

// checkRelays is one pass over this replica's relays (§6, Replicas): each whose row has closed ends with the row's
// reason; each whose own sign-in no longer holds ends signed_out, its backend ended, the database session going on
// under its opener; each that cannot be judged ends too, and a pass that cannot read at all ends every relay.
// Their counters are flushed.
func (a *app) checkRelays(ctx context.Context) error {
	live := a.relays.all()
	if len(live) == 0 {
		return nil
	}
	ids := make([]string, len(live))
	for i, r := range live {
		ids[i] = r.sid
	}
	rows, err := a.pool.Query(ctx, "SELECT id, close_reason FROM db_sessions WHERE id = ANY($1)", ids)
	closed := map[string]*string{}
	if err == nil {
		var id string
		var reason *string
		_, err = pgx.ForEachRow(rows, []any{&id, &reason}, func() error {
			closed[id] = reason
			return nil
		})
	}
	if err != nil { // nothing could be judged: every relay ends, never kept on a state no one read
		for _, r := range live {
			r.end(websocket.StatusInternalError, "", "check:unread", true)
		}
		return fmt.Errorf("the relays' rows: %w", err)
	}
	for _, r := range live {
		reason, found := closed[r.sid]
		switch {
		case !found:
			r.end(websocket.StatusInternalError, "", "check:unread", true)
			continue
		case reason != nil:
			r.end(dbClosed, *reason, "check:"+*reason, true)
			continue
		}
		if a.auth == authOIDC {
			if _, _, _, ok := a.sessionFor(ctx, r.signin); r.signin == "" || !ok {
				r.end(dbClosed, "signed_out", "check:signed_out", true)
				continue
			}
		}
		a.flushRelay(ctx, r)
	}
	return nil
}

// sweepRelays checks this replica's relays every interval until ctx is done.
func (a *app) sweepRelays(ctx context.Context, logger *log.Logger, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.checkRelays(ctx); err != nil {
				logger.Printf("canyon-server: the database relays' checks: %v", err)
			}
		}
	}
}

// cancelDBStatement cancels the statement one backend of the caller's own database session runs (§3, Cancel,
// stateless): any replica can, as the database's own pg_cancel_backend of a pid the person's role owns, and it
// answers 204 cancelled or not. Anyone else is 404.
func (a *app) cancelDBStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, ended, _ := a.dbOwn(w, r) // anyone else's is 404 before their body is read
	if s == nil {
		return
	}
	var body struct {
		ProcessID int64 `json:"process_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if ended == "" && body.ProcessID >= 1 && body.ProcessID <= 1<<31-1 {
		issuer, refusal, err := a.dbIssuer(ctx, a.pool, s.key())
		if err == nil && refusal == "" {
			err = issuer.Cancel(ctx, s.key(), s.person(), int(body.ProcessID))
		}
		if err != nil {
			a.log.Printf("canyon-server: database session %s: a cancel: %v", s.id, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// dbMaxConnsOf is CANYON_DB_MAX_CONNECTIONS: unset is dbHostConnections; anything but a whole number from 1 to
// 10000 refuses the boot, never a cap read as none.
func dbMaxConnsOf(v string) (int, error) {
	if v == "" {
		return dbHostConnections, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 10000 {
		return 0, errors.New("CANYON_DB_MAX_CONNECTIONS must be a whole number from 1 to 10000")
	}
	return n, nil
}

// dbHostCap is CANYON_DB_MAX_CONNECTIONS, its default where none was set.
func (a *app) dbHostCap() int {
	if a.dbMaxConns > 0 {
		return a.dbMaxConns
	}
	return dbHostConnections
}
