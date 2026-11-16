// Package pgtest gives a test its own empty schema on the Postgres that CANYON_TEST_DATABASE_URL
// names (CI starts one as a service). Callers migrate it themselves when they need the tables.
package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema drops and recreates a schema named after the test and returns a pool whose every
// connection lives in it: a session setting on one pooled connection would not survive the pool
// handing out another. On CI the URL is required; a skip there would hide every database test.
func Schema(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := URL(t)
	ctx := context.Background()
	schema := schemaName(t.Name())
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, ctx
}

// URL is CANYON_TEST_DATABASE_URL, for a test that needs the server rather than a schema on it.
// Unset, the test skips, except on CI.
func URL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CANYON_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CANYON_TEST_DATABASE_URL not set on CI: the database tests would silently not run")
		}
		t.Skip("CANYON_TEST_DATABASE_URL not set")
	}
	return url
}

// Exclusive is URL for a test that creates cluster-wide objects — the built-in driver's databases
// and logins, named from the app's key, not the test's schema. It holds one advisory lock until the
// test ends, so such tests in parallel packages never share an app's database mid-test.
func Exclusive(t *testing.T) string {
	t.Helper()
	url := URL(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended('pgtest.exclusive', 0))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) }) // closing the session releases the lock
	return url
}

// schemaName is the test's schema: its lowercased name, a subtest's '/' and any other character
// an unquoted identifier cannot hold read as '_', or, past Postgres's 63-character identifier limit,
// a prefix and a hash of the whole name — Postgres would otherwise truncate silently, and two long
// names sharing their first 61 characters would share a schema.
func schemaName(test string) string {
	name := "t_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, strings.ToLower(test))
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:46] + "_" + hex.EncodeToString(sum[:8])
}
