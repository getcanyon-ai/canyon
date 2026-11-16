package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// says renders err the way the named verb's failure does, with colour on, and answers what the
// reader saw on stderr. Colour on is the point: §2 keeps red for blockers, so none of this may be
// painted. The verb is a parameter because §9's "running somewhere else" line differs with it.
func says(verb string, err error) string {
	var out strings.Builder
	(&app{out: io.Discard, err: &out, verb: command{name: verb}, render: Render{Color: true}}).fail(err)
	return out.String()
}

func read(t *testing.T, base string) error {
	t.Helper()
	var page map[string]any
	return newClient(base).get(context.Background(), "/api/v1/fleet", &page)
}

// What a host answers is not always a page, and the reader is told which kind of not: the
// contract's message when there is one, otherwise that this is not Canyon — never the decoder's.
func TestWhatAHostAnswersThatIsNotAPage(t *testing.T) {
	for _, c := range []struct {
		name                 string
		handler              http.HandlerFunc
		want, unwanted, code string
		status               int
	}{
		{name: "the contract's error envelope", want: "not yours to read", code: "forbidden", status: 403, handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden","message":"this host is not yours to read"}`)
		}},
		{name: "an error with no envelope", want: "Bad Gateway", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>gateway</html>")
		}},
		{name: "a proxy's login page", want: "is not Canyon", unwanted: "invalid character", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>please log in</html>")
		}},
		{name: "a proxy's message-only body", want: "Bad Gateway", unwanted: "upstream", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"upstream unavailable"}`)
		}},
		{name: "a redirect we do not follow", want: "is not Canyon", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/next" {
				t.Error("the redirect was followed")
			}
			http.Redirect(w, r, "/next", http.StatusFound)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(c.handler)
			defer server.Close()
			err := read(t, server.URL)
			if err == nil {
				t.Fatal("the answer was read as a page")
			}
			if line := says("fleet", err); !strings.Contains(line, c.want) || (c.unwanted != "" && strings.Contains(line, c.unwanted)) {
				t.Errorf("the reader saw %q; want %q in it", line, c.want)
			}
			// An envelope keeps both halves: a caller branches on the code and shows the message.
			var api *APIError
			if c.code != "" && (!errors.As(err, &api) || api.Code != c.code || api.Status != c.status) {
				t.Errorf("the envelope became %+v", err)
			}
		})
	}
}

// One client per invocation, with its jar: the session a host mints on first contact has to be on
// the second call, and CLI-3 makes two POSTs in one run.
func TestTheClientSendsNoOriginAndKeepsItsSession(t *testing.T) {
	var carried []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			// The host's same-origin guard refuses a wrong one; a CLI has no origin to claim.
			http.Error(w, `{"error":"forbidden","message":"cross-origin request refused"}`, http.StatusForbidden)
			return
		}
		cookie, _ := r.Cookie("canyon_session") // String() on a nil cookie is the empty string
		carried = append(carried, cookie.String())
		http.SetCookie(w, &http.Cookie{Name: "canyon_session", Value: "pancake", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}) //nolint:gosec // what a demo host on plain loopback HTTP sends, which is the thing under test
		_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":null}`)
	}))
	t.Cleanup(server.Close)
	a := &app{getenv: func(string) string { return "" }, hostFlag: server.URL}
	for range 2 {
		client, err := a.client()
		if err != nil {
			t.Fatal(err)
		}
		var page map[string]any
		if err := client.get(context.Background(), "/api/v1/fleet", &page); err != nil {
			t.Fatal(err)
		}
	}
	if len(carried) != 2 || carried[0] != "" || !strings.Contains(carried[1], "pancake") {
		t.Errorf("the session the host minted did not survive the second call: %q", carried)
	}
}

// §9's three-line block is for a port nothing is listening at. A certificate and a timeout get one
// line each, because "is the demo running?" is the wrong next move for both.
func TestOnlyADeadPortGetsTheDemoBlock(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	var unreachable *Unreachable
	err := read(t, gone.URL)
	if !errors.As(err, &unreachable) || unreachable.Err == nil {
		t.Fatalf("a dead port became %+v", err)
	}
	if line := says("fleet", err); !strings.Contains(line, "canyon demo up") || !strings.Contains(line, "canyon fleet --host") {
		t.Errorf("a dead port lost §9's block:\n%s", line)
	}
	secure := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(secure.Close)
	if line := says("fleet", read(t, secure.URL)); strings.Contains(line, "canyon demo up") || !strings.Contains(line, "certificate") {
		t.Errorf("a certificate we do not trust read as %q", line)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var page map[string]any
	if err := newClient(gone.URL).get(expired, "/api/v1/fleet", &page); err == nil || !strings.Contains(err.Error(), "no answer in 30s") {
		t.Errorf("a timeout said %v", err)
	}
	// Red is a blocker and neither of these is one; nor is a stack trace an answer.
	if line := says("fleet", err); strings.ContainsRune(line, '\x1b') || strings.Contains(line, "goroutine") {
		t.Errorf("a transport failure printed %q", line)
	}
}

// A deadline belongs to the call, not to the client: the promotions POST starts a production
// container before it answers, and thirty seconds there is a client giving up on work already under
// way. The line names the deadline that actually expired, because "no answer in 30s" under a call
// given three minutes is a lie about what was waited for.
func TestACallWaitsTheDeadlineItNamedAndSaysWhichOneExpired(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // the client hung up, which is the case under test
		case <-time.After(400 * time.Millisecond):
		}
	}))
	t.Cleanup(slow.Close)
	err := newClient(slow.URL).postWithin(context.Background(), "/api/v1/slow", struct{}{}, http.StatusOK, 50*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "no answer in 50ms") {
		t.Errorf("a call given 50ms against a handler that takes 400ms answered %v", err)
	}
}

// A dial that times out is a blackholed host, not a dead port: it gets the one-line cause, not §9's
// "canyon demo up".
func TestADialTimeoutIsNotADeadPort(t *testing.T) {
	err := transportError("http://box:7777", requestTimeout, &net.OpError{Op: "dial", Err: &net.DNSError{IsTimeout: true}})
	var unreachable *Unreachable
	if errors.As(err, &unreachable) || !strings.Contains(err.Error(), "no answer") {
		t.Errorf("got %v", err)
	}
}
