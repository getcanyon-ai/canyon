package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The two facts a CLI reads about this host are the host's own: the version stamped into this
// binary and the public URL it was configured with — never the Host header a request arrived with.
func TestAuthConfigCarriesTheHostsOwnVersionAndPublicURL(t *testing.T) {
	pool, ctx := migrated(t)
	setRow(t, ctx, pool, "org_name = 'Acme Insurance'") // the name the org onboarded as: /auth/config, the receipt and canyon login answer the row, not a name stamped at boot
	base, _ := start(t, pool, time.Hour)
	req, err := http.NewRequest(http.MethodGet, base+"/auth/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "elsewhere.localhost:9999" // a name this host was not configured under; under auth off a name beyond localhost is refused 421 (loopbackOnly)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		CLI, Public, Org string
		Canyon           bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CLI != version || got.Public != "http://localhost:7777" || !got.Canyon || got.Org != "Acme Insurance" {
		t.Errorf("/auth/config said cli %q, public %q and org %q; want the stamped %q, the configured public URL and the row's name", got.CLI, got.Public, got.Org, version)
	}
}
