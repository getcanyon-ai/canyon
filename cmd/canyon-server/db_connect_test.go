package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// A database session's connection never falls back to plaintext (#2813's review): pgx's prefer, the default when the
// admin URL names no sslmode, tries TLS without verifying it and then plaintext, so a man in the middle could push the
// person's password and every row into the clear. TLS with no fallback, as require is; allow, plaintext first, is
// refused and names the sslmode alone; disable, the operator's own choice (compose), and verify-full stay as given.
func TestADatabaseSessionNeverFallsBackToPlaintext(t *testing.T) {
	login := func(sslmode string) provisioners.Login {
		return provisioners.Login{Host: "apps-db", Port: 5432, Database: "c_x", User: "p_x", Secret: provisioners.NewSecret("SECRETVALUE"),
			TLS: provisioners.TLSMode{SSLMode: sslmode}}
	}
	for _, mode := range []string{"", "prefer", "require"} {
		cfg, err := provisioners.LoginConfig(login(mode))
		if err != nil || cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 {
			t.Errorf("sslmode %q: TLS %v, %d fallbacks, %v; want TLS and none", mode, cfg != nil && cfg.TLSConfig != nil, fallbacks(cfg), err)
		}
	}
	_, err := provisioners.LoginConfig(login("allow"))
	if err == nil || !strings.Contains(err.Error(), "sslmode=allow") || strings.Contains(err.Error(), "SECRETVALUE") || strings.Contains(err.Error(), "apps-db") {
		t.Errorf("sslmode allow: %v, want refused, naming the sslmode alone", err)
	}
	if cfg, err := provisioners.LoginConfig(login("disable")); err != nil || cfg.TLSConfig != nil || len(cfg.Fallbacks) != 0 {
		t.Errorf("sslmode disable: %v, %d fallbacks; want plaintext as given", err, fallbacks(cfg))
	}
	cfg, err := provisioners.LoginConfig(login("verify-full"))
	if err != nil || cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName != "apps-db" || len(cfg.Fallbacks) != 0 {
		t.Errorf("sslmode verify-full: %v; want verified TLS as given", err)
	}
}

func fallbacks(cfg *pgx.ConnConfig) int {
	if cfg == nil {
		return -1
	}
	return len(cfg.Fallbacks)
}

// A cancel request dials its own connection through the config's DialFunc: the session's counted connection stays the
// one it logged in on, so a statement after a cancel still reaches the database, and closing it closes that one. A
// cancel that took the counted connection over raced the connect's own close and leaked the login's socket.
func TestACancelLeavesTheCountedConnectionAlone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cancels, closed := make(chan struct{}, 1), make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				be := pgproto3.NewBackend(c, c)
				m, err := be.ReceiveStartupMessage()
				if _, ok := m.(*pgproto3.CancelRequest); ok {
					cancels <- struct{}{}
					return
				}
				if err != nil {
					return
				}
				be.Send(&pgproto3.AuthenticationOk{})
				be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 2}})
				be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if be.Flush() != nil {
					return
				}
				for {
					m, err := be.Receive()
					if err != nil {
						close(closed)
						return
					}
					if _, ok := m.(*pgproto3.Query); ok {
						be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
						be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
						if be.Flush() != nil {
							return
						}
					}
				}
			}()
		}
	}()
	l := provisioners.Login{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Database: "app", User: "dana", Secret: provisioners.NewSecret(""), TLS: provisioners.TLSMode{SSLMode: "disable"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counted := &countingConn{}
	conn, err := dbConnect(ctx, l, "dbs_0cf24d382accf9ea", "query", nil, counted)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.PgConn().CancelRequest(ctx); err != nil {
		t.Fatal(err)
	}
	<-cancels
	if _, err := conn.PgConn().Exec(ctx, "select 1").ReadAll(); err != nil {
		t.Errorf("a statement after a cancel: %v", err)
	}
	_ = conn.Close(ctx)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("closing the session's connection left the login's socket open")
	}
}
