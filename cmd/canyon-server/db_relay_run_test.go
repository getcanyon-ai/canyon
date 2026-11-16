package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// relayed runs relay over a stand-in backend, the database end of a pipe, and answers the client's WebSocket once it
// has read the connection's start, and the database's end.
func relayed(t *testing.T, relay *dbRelay) (*websocket.Conn, net.Conn) {
	t.Helper()
	a := &app{pool: unreachable(t), log: log.New(io.Discard, "", 0)}
	end, db := net.Pipe()
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{dbSubprotocol}})
		if err != nil {
			return
		}
		a.relayRun(ws, &pgconn.HijackedConn{Conn: end, Frontend: pgproto3.NewFrontend(end, end), PID: 1, TxStatus: 'I', ParameterStatuses: map[string]string{}}, relay)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{Subprotocols: []string{dbSubprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("the connection's start: %v", err)
	}
	return ws, db
}

// A relay ends with its database session, close code 4000 expired, on a timer of its own: never a check's interval
// later (#2129's review).
func TestARelayEndsTheMomentItsSessionDoes(t *testing.T) {
	ws, _ := relayed(t, &dbRelay{sid: "dbs_0000000000000001", ends: time.Now().Add(300 * time.Millisecond), endsAs: "expired"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != dbClosed || ce.Reason != "expired" {
		t.Fatalf("at the session's end the relay answered %v; want close 4000 expired", err)
	}
}

// A binary message up to 1 MiB reaches the database whole, and one over it closes 1009 (contracts/routes.md, the
// connect row): the limit canyon db connect frames to.
func TestARelayReadsAMessageUpTo1MiB(t *testing.T) {
	ws, db := relayed(t, &dbRelay{sid: "dbs_0000000000000002"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg := bytes.Repeat([]byte{'q'}, dbMessageMost)
	go func() { _ = ws.Write(ctx, websocket.MessageBinary, msg) }()
	got := make([]byte, len(msg))
	_ = db.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(db, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("a 1 MiB message reached the database as %d bytes: %v", len(got), err)
	}
	ws2, db2 := relayed(t, &dbRelay{sid: "dbs_0000000000000003"})
	go func() { _, _ = io.Copy(io.Discard, db2) }() // the database reads what it is sent, as a socket's buffer would
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	go func() { _ = ws2.Write(ctx2, websocket.MessageBinary, append(msg, 'q')) }()
	_, _, err := ws2.Read(ctx2)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusMessageTooBig {
		t.Fatalf("a message over 1 MiB: %v; want close 1009", err)
	}
}

// The hijacked connection keeps no config: it holds the login's password, which nothing needs once logged in
// (#2129's review).
func TestAHijackedConnectionHoldsNoPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		b := pgproto3.NewBackend(c, c)
		if _, err := b.ReceiveStartupMessage(); err != nil {
			return
		}
		b.Send(&pgproto3.AuthenticationOk{})
		b.Send(&pgproto3.BackendKeyData{ProcessID: 7, SecretKey: []byte{1, 2, 3, 4}})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		_ = b.Flush()
		_, _ = io.Copy(io.Discard, c)
	}()
	cfg, err := pgx.ParseConfig("postgres://dana:SECRETVALUE@" + ln.Addr().String() + "/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	hj, err := hijacked(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hj.Conn.Close() }()
	if hj.PID != 7 || hj.Config != nil {
		t.Errorf("the hijacked connection: pid %d, config kept %v", hj.PID, hj.Config != nil)
	}
}
