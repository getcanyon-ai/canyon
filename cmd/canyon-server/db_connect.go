package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// dbConnect is a database session's one connection as the login l, made inside the issuer's connect (dbOpen's
// connect): the startup parameters params first, but for application_name in any case, then application_name
// canyon:<sid>:<tool>, which no parameter overrides, and every byte it carries counted on counted. The query calls it with tool "query" (#2118); the relay
// (#2129) calls it with the client's startup parameters and its own tool's name. The login's password is in the
// connection's config alone, for this one connect.
func dbConnect(ctx context.Context, l provisioners.Login, sid, tool string, params map[string]string, counted *countingConn) (*pgx.Conn, error) {
	cfg, err := provisioners.LoginConfig(l) // never a plaintext fallback, allow refused (#2813 review, #2819)
	if err != nil {
		return nil, err
	}
	for k, v := range params {
		if !strings.EqualFold(k, "application_name") { // Postgres reads a name in any case (#2129 review)
			cfg.RuntimeParams[k] = v
		}
	}
	cfg.RuntimeParams["application_name"] = "canyon:" + sid + ":" + tool
	dial := cfg.DialFunc
	var loggedIn atomic.Bool
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !loggedIn.CompareAndSwap(false, true) {
			return c, nil // a cancel request's own connection (pgx dials it through this func): never the counted one (#2932)
		}
		counted.Conn = c
		return counted, nil
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// countingConn is a connection that counts the bytes it carries each way, and nothing else: it holds no byte of
// them, buffers none, and logs none (#2118 review).
type countingConn struct {
	net.Conn
	from, to atomic.Int64 // read from the database; written to it
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.from.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.to.Add(int64(n))
	return n, err
}
