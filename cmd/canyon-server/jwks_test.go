package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/vault"
)

// The first boot generates the key and seals it; every later boot opens that same key.
func TestTheIdentityKeyIsGeneratedOnceAndSealed(t *testing.T) {
	pool, ctx := migrated(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := identityKey(ctx, pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	again, err := identityKey(ctx, pool, keys)
	if err != nil || again.ID != first.ID {
		t.Fatalf("a second boot opens the key the first sealed: %q → %q %v", first.ID, again.ID, err)
	}
	der, err := first.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	var sealed []byte
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM identity_keys), (SELECT sealed_value FROM identity_keys LIMIT 1)").Scan(&rows, &sealed); err != nil || rows != 1 || bytes.Contains(sealed, der) {
		t.Fatalf("one row, the key sealed, never plain: %d rows %v", rows, err)
	}
}

// Apps reach this listener, so it answers the key set at its one path and 404 to everything else:
// never the API, the dashboard or a health route, and never the private half.
func TestTheJWKSListenerServesTheKeySetAndNothingElse(t *testing.T) {
	key, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := key.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(jwksHandler(doc))
	defer srv.Close()
	for _, c := range []struct{ method, path string }{{"GET", jwksPath}, {"GET", "/"}, {"GET", "/api/v1/fleet"}, {"GET", "/ready"}, {"GET", "/.well-known/openid-configuration"}, {"GET", jwksPath + "/"}, {"POST", jwksPath}} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if served := c == struct{ method, path string }{"GET", jwksPath}; served != (resp.StatusCode == 200) || (!served && resp.StatusCode != 404) {
			t.Errorf("%s %s answered %d", c.method, c.path, resp.StatusCode)
		} else if served && (resp.Header.Get("Content-Type") != "application/json" || !bytes.Equal(body, doc) || strings.Contains(string(body), `"d"`)) {
			t.Errorf("the key set, public half alone: %q %s", resp.Header.Get("Content-Type"), body)
		}
	}
}

func TestTheJWKSAddrIsWhereAppsReachTheServer(t *testing.T) {
	for in, want := range map[string][2]string{
		"":                  {":7780", "http://canyon-server:7780/.well-known/jwks.json"}, // the contract's example, and compose's names
		"canyon.apps:9443":  {":9443", "http://canyon.apps:9443/.well-known/jwks.json"},
		":7780":             {},
		"http://canyon:780": {},
	} {
		listen, url, err := jwksAddrOf(in)
		if (err != nil) != (want == [2]string{}) || listen != want[0] || url != want[1] {
			t.Errorf("%q: %q %q %v", in, listen, url, err)
		}
	}
}
