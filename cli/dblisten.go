package cli

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgproto3"
)

// The listener's half of the Postgres protocol (design/DATABASE-SESSIONS.md §3): it answers a client on loopback as a
// server would until the password, checks that against the local token, and only then opens the WebSocket that
// carries the connection to Canyon, which logs in as the person. Nothing a client sends reaches Canyon before that.

const (
	dbSubprotocol  = "canyon-db.1"
	dbStartupLimit = 30 * time.Second // a client that has not signed in by now is dropped
	dbClosed       = websocket.StatusCode(4000)
	tlsHandshake   = 0x16 // a TLS record's first byte: libpq 17's sslnegotiation=direct
	codeAuth       = "28P01"
	codeRefused    = "08004"
	dbMessageMost  = 1 << 20          // a binary message, either way (contracts/routes.md, the connect row)
	dbPoll         = 30 * time.Second // how often a run reads its database session, connected or not
)

// The FATALs a client sees (§3, §9), each after "Canyon: ".
const (
	fatalEnded       = "this database session has ended. Run canyon db connect again."
	fatalPassword    = "the password isn't this session's: use the URL canyon db connect printed."
	fatalReplication = "replication connections aren't relayed."
	fatalDirect      = "drop sslnegotiation=direct; this listener negotiates TLS the usual way."
)

type tlsCert = tls.Certificate

// loopbackCert is an ECDSA certificate for 127.0.0.1 and localhost, made in memory for this run and kept nowhere:
// sslmode=require works on it, and verify-ca and verify-full fail, as nothing can vouch for it.
func loopbackCert() (tlsCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsCert{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tlsCert{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "canyon db connect"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tlsCert{}, err
	}
	return tlsCert{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// clientStartup is what the listener makes of a client's StartupMessage: the parameters it forwards, the protocol
// options it declines, whether it negotiates down to 3.0, the client's name, and a refusal.
type clientStartup struct {
	forward   map[string]string
	declined  []string
	negotiate bool
	tool      string
	fatal     string
}

// startupOf reads a StartupMessage (§3): user and database are the database session's, and require_auth the client's
// own (the printed URL's, which a pgx client sends), so dropped; replication is
// refused; a _pq_. option is declined, and asking 3.2 or naming one negotiates 3.0; every other parameter is
// forwarded, its name in lower case, as Postgres reads it and the server takes it: a name the same as one before it
// but for case is dropped, the server refusing two such names (contracts/routes.md, the connect row).
func startupOf(m *pgproto3.StartupMessage) clientStartup {
	s := clientStartup{forward: map[string]string{}, negotiate: m.ProtocolVersion != pgproto3.ProtocolVersion30}
	for _, name := range slices.Sorted(maps.Keys(m.Parameters)) { // sorted: which of two such names is kept never varies
		value := m.Parameters[name]
		switch lower := strings.ToLower(name); {
		case lower == "replication":
			return clientStartup{fatal: fatalReplication}
		case lower == "user", lower == "database", lower == "require_auth": // the last is libpq's own; a client that sends it (pgx) means it for us
		case strings.HasPrefix(lower, "_pq_."):
			s.declined, s.negotiate = append(s.declined, name), true
		default:
			if _, twice := s.forward[lower]; !twice {
				s.forward[lower] = value
			}
		}
	}
	slices.Sort(s.declined)
	s.tool = s.forward["application_name"]
	return s
}

// startupParam is the forwarded parameters as GET …/connect?startup= carries them: base64url JSON.
func startupParam(forward map[string]string) (string, error) {
	b, err := json.Marshal(forward)
	return base64.RawURLEncoding.EncodeToString(b), err
}

// serve accepts clients until the listener closes, each handled in a goroutine of its own that stop waits for.
func (l *dbListener) serve(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !l.hold(conn) { // the run is stopping: nothing new is carried
			_ = conn.Close()
			return
		}
		go func() {
			defer l.live.Done()
			defer l.release(conn)
			l.handle(ctx, conn)
		}()
	}
}

// hold counts a client in, unless the run is stopping.
func (l *dbListener) hold(conn net.Conn) bool {
	l.connsMu.Lock()
	defer l.connsMu.Unlock()
	if l.stopping {
		return false
	}
	if l.conns == nil {
		l.conns = map[net.Conn]bool{}
	}
	l.conns[conn] = true
	l.live.Add(1)
	return true
}

func (l *dbListener) release(conn net.Conn) {
	l.connsMu.Lock()
	defer l.connsMu.Unlock()
	delete(l.conns, conn)
}

// stop ends the run's listening and every connection it carries, then waits until each has said its last line: the
// run's own last line comes after them, and nothing a connection says is lost to the process exiting (#2921).
func (l *dbListener) stop(ln net.Listener) {
	_ = ln.Close()
	l.connsMu.Lock()
	l.stopping = true
	for conn := range l.conns {
		_ = conn.Close() // the client's end: its relay closes Canyon's side, says its closed line and returns
	}
	l.connsMu.Unlock()
	l.live.Wait()
}

// fatal answers the client a FATAL, Canyon's words after "Canyon: ", and ends its connection.
func fatal(be *pgproto3.Backend, code, message string) {
	be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: code, Message: "Canyon: " + message})
	_ = be.Flush()
}

// handle is one client: TLS and protocol negotiated, the password checked, then one WebSocket relaying it.
func (l *dbListener) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(dbStartupLimit))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	var rw io.ReadWriter = struct {
		io.Reader
		io.Writer
	}{br, conn}
	be := pgproto3.NewBackend(rw, rw)
	if first[0] == tlsHandshake {
		fatal(be, codeRefused, fatalDirect)
		return
	}
	var startup *pgproto3.StartupMessage
	for startup == nil {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.GSSEncRequest:
			if _, err := rw.Write([]byte{'N'}); err != nil {
				return
			}
		case *pgproto3.SSLRequest:
			if _, err := rw.Write([]byte{'S'}); err != nil {
				return
			}
			secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{l.cert}, MinVersion: tls.VersionTLS12})
			if err := secure.HandshakeContext(ctx); err != nil {
				if op := (*net.OpError)(nil); errors.As(err, &op) && op.Op == "remote error" {
					l.verified.Do(func() {
						l.say(false, "%s A client refused this listener's certificate. It is made for this run only: connect with sslmode=require, not verify-ca or verify-full.\n",
							l.a.render.glyph(statePending))
					})
				}
				return
			}
			rw = secure
			be = pgproto3.NewBackend(secure, secure)
		case *pgproto3.CancelRequest:
			// only with the key this listener gave that connection: another local process's guess is dropped, unsaid
			key, live := l.keys.Load(m.ProcessID)
			if !live || subtle.ConstantTimeCompare([]byte(key.(string)), m.SecretKey) != 1 {
				return
			}
			_ = l.client.postJSON(ctx, "/api/v1/db-sessions/"+url.PathEscape(l.session.ID)+"/cancel", map[string]uint32{"process_id": m.ProcessID}, http.StatusNoContent, nil)
			return
		case *pgproto3.StartupMessage:
			startup = m
		}
	}
	s := startupOf(startup)
	if s.fatal != "" {
		fatal(be, codeRefused, s.fatal)
		return
	}
	if s.negotiate {
		be.Send(&pgproto3.NegotiateProtocolVersion{NewestMinorProtocol: 0, UnrecognizedOptions: s.declined})
	}
	if !l.scram(be) {
		fatal(be, codeAuth, fatalPassword) // nothing reaches Canyon
		return
	}
	_ = conn.SetDeadline(time.Time{}) // signed in: the connection lasts as long as the client keeps it
	l.relay(ctx, rw, be, s)
}

// relay opens the WebSocket for one signed-in client and carries its bytes both ways: Canyon's synthetic startup
// first, its backend's id read for the line and for a cancel, then whatever either end sends until one closes.
func (l *dbListener) relay(ctx context.Context, client io.ReadWriter, be *pgproto3.Backend, s clientStartup) {
	param, err := startupParam(s.forward)
	if err != nil {
		return
	}
	path := "/api/v1/db-sessions/" + url.PathEscape(l.session.ID) + "/connect?startup=" + param
	ws, resp, err := websocket.Dial(ctx, l.client.Base+path, &websocket.DialOptions{HTTPClient: l.client.HTTP, Subprotocols: []string{dbSubprotocol},
		HTTPHeader: http.Header{"User-Agent": {"canyon/" + Version}}})
	if err != nil {
		l.refused(be, resp, path, err)
		return
	}
	defer func() { _ = ws.CloseNow() }()
	if ws.Subprotocol() != dbSubprotocol {
		fatal(be, codeRefused, "this Canyon doesn't relay database connections yet; it needs updating.")
		return
	}
	pipe := relayPipe(ctx, ws)
	in := &wsReader{ctx: context.WithoutCancel(ctx), ws: ws}
	fe := pgproto3.NewFrontend(in, io.Discard)
	var first []byte
	var pid uint32
	for {
		msg, err := fe.Receive()
		if err != nil {
			l.closed(err)
			return
		}
		if key, ok := msg.(*pgproto3.BackendKeyData); ok { // Canyon sends no secret: the listener gives the client its own
			secret := make([]byte, 4)
			if _, err := rand.Read(secret); err != nil {
				return
			}
			pid, key.SecretKey = key.ProcessID, secret
			l.keys.Store(pid, string(secret))
			defer l.keys.CompareAndDelete(pid, string(secret))
		}
		if first, err = msg.Encode(first); err != nil {
			return
		}
		if _, ready := msg.(*pgproto3.ReadyForQuery); ready {
			break
		}
	}
	for fe.ReadBufferLen() > 0 { // what came in the same read as the startup: the backend's, never dropped
		msg, err := fe.Receive()
		if err != nil {
			return
		}
		if first, err = msg.Encode(first); err != nil {
			return
		}
	}
	if _, err := client.Write(first); err != nil {
		return
	}
	n := l.count.Add(1)
	tool := ""
	if s.tool != "" {
		tool = " (" + cell(s.tool) + ")"
	}
	l.say(false, "%s %s  connection %d opened%s %s backend %d\n", l.a.render.glyph(statePending), time.Now().Format("15:04"), n, tool, l.a.dot(), pid)
	go func() {
		_, _ = io.Copy(pipe, client)
		// The client's close can meet Canyon's: the close handshake then reads Canyon's 4000 before the relay's reader
		// does, and answers it here. It still ends the run, at once rather than at the next poll.
		var ce websocket.CloseError
		if err := ws.Close(websocket.StatusNormalClosure, ""); errors.As(err, &ce) && ce.Code == dbClosed && ce.Reason != "canyon_restarting" {
			l.end(ce.Reason)
		}
	}()
	if l.reader != nil {
		<-l.reader
	}
	var from atomic.Int64
	_, err = io.Copy(client, &countedReader{r: in, n: &from})
	l.fromDB.Add(from.Load())
	l.say(false, "%s %s  connection %d closed %s %s from the database\n", l.a.render.glyph(statePending), time.Now().Format("15:04"), n, l.a.dot(), size(from.Load()))
	l.closed(err)
}

// relayPipe is the WebSocket as the bytes written to it. A message read is at most 1 MiB, the contract's: NetConn
// lifts every limit, so it is set after, and a bigger message closes 1009.
func relayPipe(ctx context.Context, ws *websocket.Conn) net.Conn {
	pipe := websocket.NetConn(context.WithoutCancel(ctx), ws, websocket.MessageBinary)
	ws.SetReadLimit(dbMessageMost)
	return pipe
}

// wsReader is the bytes the WebSocket carries in, message after message. NetConn reads a 1001 as the end of the bytes,
// as it does a 1000, and its code is then gone; this reader ends at a 1000 alone and returns any other close as it
// came, so closed can say a ping was missed.
type wsReader struct {
	ctx context.Context
	ws  *websocket.Conn
	r   io.Reader
}

func (w *wsReader) Read(p []byte) (int, error) {
	for {
		if w.r == nil {
			_, r, err := w.ws.Reader(w.ctx)
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return 0, io.EOF
			}
			if err != nil {
				return 0, err
			}
			w.r = r
		}
		n, err := w.r.Read(p)
		if errors.Is(err, io.EOF) {
			w.r = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

// watch ends the run when its database session ends, a client connected or not (§9): at expires_at on a timer of
// its own, and, read every l.poll, when the host says it closed, with its close_reason, or that the sign-in expired.
// A read that fails otherwise changes nothing: the next one, or a relay's close, says.
func (l *dbListener) watch(ctx context.Context) {
	if at, err := time.Parse(time.RFC3339, l.session.ExpiresAt); err == nil {
		expired := time.AfterFunc(time.Until(at), func() { l.end("expired") })
		defer expired.Stop()
	}
	tick := time.NewTicker(l.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var read struct {
			CloseReason string `json:"close_reason"`
		}
		err := l.client.get(ctx, "/api/v1/db-sessions/"+url.PathEscape(l.session.ID), &read)
		var signedOut *SessionExpired
		switch {
		case errors.As(err, &signedOut):
			l.end("signed_out")
		case err == nil && read.CloseReason != "":
			l.end(read.CloseReason)
		}
	}
}

// closed reads how Canyon ended a relay: canyon_restarting keeps listening, 1011 and 1001 are this connection failing,
// each its own line, and a close_reason ends the run. A normal close says nothing more.
func (l *dbListener) closed(err error) {
	var ce websocket.CloseError
	switch {
	case errors.Is(err, websocket.ErrMessageTooBig) || errors.As(err, &ce) && ce.Code == websocket.StatusMessageTooBig: // either end's 1 MiB
		l.say(false, "%s A message was over 1 MiB, Canyon's limit: the connection closed.\n", l.a.render.glyph(statePending))
	case !errors.As(err, &ce):
	case ce.Code == dbClosed && ce.Reason == "canyon_restarting":
		l.say(false, "%s Canyon is restarting — still listening; reconnect your client.\n", l.a.render.glyph(statePending))
	case ce.Code == dbClosed:
		l.end(ce.Reason)
	case ce.Code == websocket.StatusInternalError:
		l.say(false, "%s A connection failed: Canyon couldn't check the database session. Reconnect your client.\n", l.a.render.glyph(statePending))
	case ce.Code == websocket.StatusGoingAway: // a ping went unanswered
		l.say(false, "%s The connection to Canyon dropped: reconnect your client.\n", l.a.render.glyph(statePending))
	}
}

// refused is the host's answer to a connect it did not upgrade, said to the client as a FATAL: an ended database
// session ends the run too, as does a sign-in that expired.
func (l *dbListener) refused(be *pgproto3.Backend, resp *http.Response, path string, err error) {
	if resp == nil {
		fatal(be, codeRefused, err.Error())
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	refusal := apiError(l.client.Base, path, resp.StatusCode, body, true)
	var expired *SessionExpired
	var api *APIError
	switch {
	case errors.As(refusal, &expired):
		fatal(be, codeRefused, "your Canyon sign-in expired. Run: canyon login "+l.client.name())
		l.end("signed_out")
	case errors.As(refusal, &api) && api.Code == "session_ended":
		var reason string
		_ = json.Unmarshal(api.Extra["reason"], &reason)
		fatal(be, codeRefused, fatalEnded)
		l.end(reason)
	case errors.As(refusal, &api):
		fatal(be, codeRefused, api.Message)
	default:
		fatal(be, codeRefused, refusal.Error())
	}
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
