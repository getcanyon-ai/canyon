package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Only the proxy in front of Canyon asks, from loopback or an address CANYON_TRUSTED_PROXIES names, and
// with no X-Forwarded-For: a request it forwards from outside carries one, so nobody outside can list
// apps through it. Canyon's own host is answered without a database.
func TestTLSAskAnswersTheProxyAlone(t *testing.T) {
	a := &app{public: "https://canyon.example.com", proxies: proxies{netip.MustParsePrefix("10.214.4.2/32")}}
	ask := func(peer, domain string, header ...string) int {
		req := httptest.NewRequest(http.MethodGet, "/tls/ask?domain="+domain, nil)
		req.RemoteAddr = peer
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		a.tlsAsk(rec, req)
		return rec.Code
	}
	for _, peer := range []string{"127.0.0.1:50000", "[::1]:50000", "10.214.4.2:50000"} {
		if code := ask(peer, "canyon.example.com"); code != http.StatusOK {
			t.Errorf("Canyon's own host, asked from %s: %d", peer, code)
		}
		if code := ask(peer, "Canyon.Example.Com."); code != http.StatusOK {
			t.Errorf("Canyon's own host in another case, with a final dot, from %s: %d", peer, code)
		}
	}
	for _, peer := range []string{"10.214.4.3:50000", "203.0.113.7:50000", "10.214.4.1:50000"} {
		if code := ask(peer, "canyon.example.com"); code != http.StatusForbidden {
			t.Errorf("asked from %s, neither loopback nor the proxy: %d, want 403", peer, code)
		}
	}
	if code := ask("10.214.4.2:50000", "canyon.example.com", "X-Forwarded-For", "203.0.113.7"); code != http.StatusForbidden {
		t.Errorf("forwarded through the proxy from outside: %d, want 403", code)
	}
	if code := ask("127.0.0.1:50000", "canyon.example.com", "Forwarded", "for=203.0.113.7"); code != http.StatusForbidden {
		t.Errorf("forwarded, said with Forwarded: %d, want 403", code)
	}
	// The two addresses the Kubernetes front-door check dials are answered whether or not an app has that
	// name, and without a database (contracts/executor.md, certificate; #2463).
	for _, domain := range []string{"canyon-probe.preview.canyon.example.com", "Canyon-Probe.Prod.Canyon.Example.Com."} {
		if code := ask("10.214.4.2:50000", domain); code != http.StatusOK {
			t.Errorf("the front-door check's %s: %d, want 200", domain, code)
		}
	}
	if code := ask("10.214.4.3:50000", "canyon-probe.preview.canyon.example.com"); code != http.StatusForbidden {
		t.Errorf("the front-door check's address, asked by someone else: %d, want 403", code)
	}
	for _, domain := range []string{"x.evil.com", "canyon.example.com.evil.com", "a.b.preview.canyon.example.com", "a.staging.canyon.example.com",
		"-a.preview.canyon.example.com", "preview.canyon.example.com", "a.preview.canyon.example.org", "",
		"canyon-probe.staging.canyon.example.com", "canyon-probe.preview.canyon.example.org", "canyon-probe-preview.canyon.example.com", "x.canyon-probe.preview.canyon.example.com"} {
		if code := ask("127.0.0.1:50000", domain); code != http.StatusNotFound {
			t.Errorf("%q: %d, want 404", domain, code)
		}
	}
}

// A proxy with no address to trust asks with CANYON_TLS_ASK_TOKEN in the query (#2672): the right token
// from any address is the proxy's ask, a wrong or missing one is 403, a forwarded request is 403 whatever
// it carries, and with the token unset no token, an empty one included, is accepted. The request log
// records the path alone, so the token never reaches it.
func TestTLSAskTakesTheTokenFromAnyAddress(t *testing.T) {
	token := "test-" + strings.Repeat("ask-token-", 4) // synthetic, as fixture credentials are
	var logged bytes.Buffer
	ask := func(a *app, query string, header ...string) int {
		req := httptest.NewRequest(http.MethodGet, "/tls/ask?"+query, nil)
		req.RemoteAddr = "10.42.0.17:50000" // a pod address nothing trusts
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		routesTLS(mux, a)
		pipeline(mux, log.New(&logged, "", 0)).ServeHTTP(rec, req)
		return rec.Code
	}
	a := &app{public: "https://canyon.example.com", askToken: token}
	for _, c := range []struct {
		query  string
		header []string
		want   int
	}{
		{"domain=canyon.example.com&token=" + token, nil, http.StatusOK},
		{"token=" + token + "&domain=canyon-probe.prod.canyon.example.com", nil, http.StatusOK},
		{"domain=made-up.preview.canyon.example.com.evil&token=" + token, nil, http.StatusNotFound},
		{"domain=canyon.example.com&token=" + token[:len(token)-1] + "x", nil, http.StatusForbidden},
		{"domain=canyon.example.com&token=" + token[:20], nil, http.StatusForbidden},
		{"domain=canyon.example.com", nil, http.StatusForbidden},
		{"domain=canyon.example.com&token=", nil, http.StatusForbidden},
		{"domain=canyon.example.com&token=" + token, []string{"X-Forwarded-For", "203.0.113.7"}, http.StatusForbidden},
	} {
		if code := ask(a, c.query, c.header...); code != c.want {
			t.Errorf("%s %v: %d, want %d", c.query, c.header, code, c.want)
		}
	}
	unset := &app{public: "https://canyon.example.com"}
	for _, q := range []string{"domain=canyon.example.com&token=", "domain=canyon.example.com&token=" + token} {
		if code := ask(unset, q); code != http.StatusForbidden {
			t.Errorf("CANYON_TLS_ASK_TOKEN unset, %s: %d, want 403", q, code)
		}
	}
	if strings.Contains(logged.String(), token[:20]) || !strings.Contains(logged.String(), "GET /tls/ask 200") {
		t.Errorf("the request log holds the token, or no line for the ask:\n%s", logged.String())
	}
}

// The server starts only on a token a guess or a query's encoding can't meet.
func TestTLSAskTokenIsLongAndPlain(t *testing.T) {
	for v, ok := range map[string]bool{
		"": true, strings.Repeat("a", 32): true, strings.Repeat("A1._~-", 10): true, strings.Repeat("a", 512): true,
		strings.Repeat("a", 31): false, strings.Repeat("a", 513): false, strings.Repeat("a", 31) + "+": false,
		strings.Repeat("a", 31) + "=": false, strings.Repeat("a", 31) + "&": false, strings.Repeat("a", 31) + " ": false,
	} {
		if _, err := askTokenOf(v); (err == nil) != ok {
			t.Errorf("askTokenOf(%d characters ending %q): %v, want accepted %v", len(v), v[max(0, len(v)-1):], err, ok)
		}
	}
}

// A name is compared as ASCII: Go's lower-casing folds the Kelvin sign (U+212A) to k, so a name with any
// byte past ASCII is no name Canyon serves. CANYON_PUBLIC_URL's host is compared lower-cased too, however
// the operator wrote it. Neither reaches a database.
func TestTLSAskReadsNamesAsASCII(t *testing.T) {
	ask := func(public, domain string) int {
		a := &app{public: public}
		req := httptest.NewRequest(http.MethodGet, "/tls/ask?domain="+url.QueryEscape(domain), nil)
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		a.tlsAsk(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		public, domain string
		want           int
	}{
		{"https://kiosk.example.com", "kiosk.example.com", http.StatusOK},
		{"https://kiosk.example.com", "Kiosk.example.com", http.StatusNotFound},
		{"https://kiosk.example.com", "canyon-probe.preview.Kiosk.example.com", http.StatusNotFound},
		{"https://Canyon.Example.COM", "canyon.example.com", http.StatusOK},
		{"https://Canyon.Example.COM:8443", "Canyon-Probe.Prod.canyon.example.com", http.StatusOK},
	} {
		if code := ask(c.public, c.domain); code != c.want {
			t.Errorf("public %s, asked for %q: %d, want %d", c.public, c.domain, code, c.want)
		}
	}
}

// An app's address is answered while the app exists: an app nobody made, and one deleted, are not.
func TestTLSAskAnswersAnAppThatExists(t *testing.T) {
	pool, ctx, base, _ := withProject(t) // claims-portal; the public URL is http://localhost:7777
	ask := func(domain string) int {
		res, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + "/tls/ask?domain=" + domain) //nolint:noctx // the client's timeout bounds it
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for domain, want := range map[string]int{"claims-portal.preview.localhost": 200, "claims-portal.prod.localhost": 200, "localhost": 200,
		"nobody-made-me.preview.localhost": 404, "claims-portal.preview.example.com": 404} {
		if code := ask(domain); code != want {
			t.Errorf("%s: %d, want %d", domain, code, want)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if code := ask("claims-portal.preview.localhost"); code != http.StatusNotFound {
		t.Errorf("a deleted app: %d, want 404", code)
	}
}
