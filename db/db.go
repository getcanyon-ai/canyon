// Package db opens the control plane's Postgres pool and applies the embedded migrations.
// Migrations are plain SQL files under migrations/, applied in name order exactly once each
// and recorded in schema_migrations; running Migrate again is a no-op.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open connects to url with a pool of at most maxConns connections — pgxpool's own default, four on a
// small node, when it is 0 — never in plaintext once it has TLS (NoPlaintextFallback), and verifies the connection.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if err := NoPlaintextFallback(&cfg.ConnConfig.Config); err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// NoPlaintextFallback keeps one of Canyon's own connections off plaintext once it has TLS (#2819): pgx's default
// when a URL names no sslmode, and prefer, try TLS and then plaintext, so a man in the middle who breaks the
// handshake reads the login and every statement. It drops each try without TLS when the first has it. A first try
// in plaintext with TLS after it, allow, is an error naming the setting, never the URL; one with no TLS at all,
// disable, is left as it is, for the caller to say it is unencrypted.
func NoPlaintextFallback(cfg *pgconn.Config) error {
	if cfg.TLSConfig == nil {
		if slices.ContainsFunc(cfg.Fallbacks, func(f *pgconn.FallbackConfig) bool { return f.TLSConfig != nil }) {
			return errors.New("sslmode=allow tries plaintext first: set sslmode=verify-full with sslrootcert, or require at the least")
		}
		return nil
	}
	cfg.Fallbacks = slices.DeleteFunc(cfg.Fallbacks, func(f *pgconn.FallbackConfig) bool { return f.TLSConfig == nil })
	return nil
}

// Pending counts the embedded migrations not yet recorded; zero means the schema is current.
// On a schema that was never migrated the bookkeeping table is missing and this is an error.
func Pending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return 0, err
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE name = ANY($1)`, names).Scan(&applied); err != nil {
		return 0, err
	}
	return len(names) - applied, nil
}

// migrateLock is the advisory lock every starter takes around Migrate, so concurrent servers
// migrate once and the others wait and find the work done.
const migrateLock = 0x63616e796f6e // "canyon"

// Migrate applies every embedded migration not yet recorded, in name order, each in its own
// transaction together with its record. Returns the number applied. Concurrent starters
// serialise on an advisory lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	return migrate(ctx, pool, migrations)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return 0, err
	}
	defer func() { // the unlock must run even if ctx is done: a lock left on a pooled connection would block the next server
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return 0, err
	}
	sort.Strings(names)
	applied := 0
	for _, name := range names {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if done {
			_ = tx.Rollback(ctx)
			continue
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}
