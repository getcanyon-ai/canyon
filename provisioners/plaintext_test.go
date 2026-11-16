package provisioners

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/getcanyon-ai/canyon/db"
)

// Canyon's own connections never fall back to plaintext (#2819): with TLS first, each try without it is dropped, a
// second host's TLS try kept; allow, plaintext first, refuses to start before anything is dialled, naming the
// setting and never the URL; disable stays plaintext alone.
func TestNoPlaintextFallback(t *testing.T) {
	for _, u := range []string{"postgres://canyon@a.example,b.example/canyon", "postgres://canyon@a.example,b.example/canyon?sslmode=prefer"} {
		cfg, err := pgconn.ParseConfig(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.NoPlaintextFallback(cfg); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0].Host != "b.example" || cfg.Fallbacks[0].TLSConfig == nil {
			t.Errorf("%s: first try's TLS %t, fallbacks %+v; want TLS to a.example, then TLS to b.example alone", u, cfg.TLSConfig != nil, cfg.Fallbacks)
		}
	}
	cfg, err := pgconn.ParseConfig("postgres://canyon@a.example/canyon?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.NoPlaintextFallback(cfg); err != nil || cfg.TLSConfig != nil || len(cfg.Fallbacks) != 0 {
		t.Errorf("sslmode=disable: %v, TLS %t, %d fallbacks; want it as it is", err, cfg.TLSConfig != nil, len(cfg.Fallbacks))
	}

	for _, mode := range []string{"", "prefer"} {
		cfg, err := LoginConfig(Login{Host: "a.example", Port: 5433, Database: "app", User: "p_dana", Secret: NewSecret(""), TLS: TLSMode{SSLMode: mode}})
		if err != nil {
			t.Fatalf("a login under sslmode %q: %v", mode, err)
		}
		if cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 {
			t.Errorf("a login under sslmode %q: TLS %t, %d fallbacks; want TLS alone", mode, cfg.TLSConfig != nil, len(cfg.Fallbacks))
		}
	}
	if _, err := LoginConfig(Login{Host: "a.example", Port: 5433, Database: "app", User: "p_dana", Secret: NewSecret("never-in-an-error"), TLS: TLSMode{SSLMode: "allow"}}); err == nil ||
		!strings.Contains(err.Error(), "sslmode=allow") || strings.Contains(err.Error(), "never-in-an-error") {
		t.Errorf("a login under allow: %v; want a refusal naming sslmode=allow, never its password", err)
	}

	const allow = "postgres://canyon:never-in-an-error@127.0.0.1:1/canyon?sslmode=allow" //nolint:gosec // a synthetic password the refusal must not carry; port 1: nothing is dialled before it
	ctx := context.Background()
	if _, err := NewBuiltin(ctx, allow, ""); err == nil || !strings.Contains(err.Error(), "sslmode=allow") || strings.Contains(err.Error(), "never-in-an-error") {
		t.Errorf("the apps database's admin url under allow: %v; want a refusal naming sslmode=allow, never the url", err)
	}
	if _, err := db.Open(ctx, allow, 0); err == nil || !strings.Contains(err.Error(), "sslmode=allow") || strings.Contains(err.Error(), "never-in-an-error") {
		t.Errorf("Canyon's own database url under allow: %v; want a refusal naming sslmode=allow, never the url", err)
	}
}
