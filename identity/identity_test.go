package identity

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
)

// verifier is what an app builds from its three CANYON_IDENTITY_* variables: a real OIDC
// library fetching the key set from the URL, pinned to the issuer, the audience and ES256.
func verifier(t *testing.T, k *Key, audience string) *oidc.IDTokenVerifier {
	t.Helper()
	doc, err := k.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(doc) }))
	t.Cleanup(srv.Close)
	return oidc.NewVerifier("http://localhost:7777", oidc.NewRemoteKeySet(context.Background(), srv.URL), &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.ES256}})
}

// A key sealed and parsed back mints tokens the original's key set verifies; the audience is the
// app, so another app refuses the token — one app cannot replay what it was handed against another.
func TestMintVerifiesWithTheJWKSAndOnlyForItsApp(t *testing.T) {
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	der, err := k.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(der)
	if err != nil || back.ID != k.ID || k.ID == "" {
		t.Fatalf("a parsed key is the same key: kid %q → %q %v", k.ID, back.ID, err)
	}
	raw, err := back.Mint(Claims{Issuer: "http://localhost:7777", Subject: "u1", Email: "dana@meridian.example", Name: "Dana Whitfield", Audience: "claims-portal", Env: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := verifier(t, k, "claims-portal").Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("an app verifying with the key set: %v", err)
	}
	var got map[string]any
	if err := tok.Claims(&got); err != nil {
		t.Fatal(err)
	}
	if got["aud"] != "claims-portal" || got["sub"] != "u1" || got["email"] != "dana@meridian.example" || got["name"] != "Dana Whitfield" || got["env"] != "preview" || got["exp"].(float64)-got["iat"].(float64) != TTL.Seconds() {
		t.Fatalf("claims %v", got)
	}
	if roles, ok := got["roles"].([]any); !ok || len(roles) != 0 {
		t.Fatalf("no roles is [], never null: %v", got["roles"])
	}
	header, err := base64.RawURLEncoding.DecodeString(raw[:strings.IndexByte(raw, '.')])
	if err != nil || !strings.Contains(string(header), `"alg":"ES256"`) || !strings.Contains(string(header), `"kid":"`+k.ID+`"`) {
		t.Fatalf("the header names ES256 and the key: %s %v", header, err)
	}
	if _, err := verifier(t, k, "quote-engine").Verify(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("quote-engine accepted claims-portal's token: %v", err)
	}
}
