package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartupFetchRetries (#2370): a choice 2 sidecar whose provider fails its first fetches — as a new pod's first
// calls through Canyon's egress gateway can — starts once the provider answers, each failure logged once; one whose
// provider never answers gives up after the bound, with the last error, so the sidecar exits as before.
func TestStartupFetchRetries(t *testing.T) {
	var failing atomic.Int32
	var asked atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked.Add(1); failing.Add(-1) >= 0 {
			http.Error(w, "unknown_caller", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = io.WriteString(w, `{"issuer":"`+srv.URL+`","authorization_endpoint":"`+srv.URL+`/auth","token_endpoint":"`+srv.URL+`/token","jwks_uri":"`+srv.URL+`/keys"}`)
		case "/keys":
			_, _ = io.WriteString(w, `{"keys":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var d frontDoor
	d.Choice, d.Signin.Issuer, d.Signin.ClientID = 2, srv.URL, "canyon-apps"

	failing.Store(2)
	var logs strings.Builder
	if v, err := startVerifier(d, srv.Client(), log.New(&logs, "", 0), 2*time.Second); err != nil || v == nil {
		t.Errorf("a provider that failed twice, then answered: %v, want the sidecar started", err)
	}
	if n, lines := asked.Load(), strings.Count(logs.String(), "\n"); n != 4 || lines != 2 {
		t.Errorf("%d fetch(es), %d line(s) logged; want two failures, each logged once, then the discovery and the key set:\n%s", n, lines, logs.String())
	}

	failing.Store(1 << 30)
	start := time.Now()
	if _, err := startVerifier(d, srv.Client(), log.New(io.Discard, "", 0), time.Second); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a provider that never answers: %v, want its last error", err)
	}
	if took := time.Since(start); took < time.Second-100*time.Millisecond || took > 2*time.Second {
		t.Errorf("gave up after %v, want the 1 s bound", took)
	}
}
