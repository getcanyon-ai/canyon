package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// oidcHost is a host that authenticates: /auth/config says so, /auth/device mints one grant, the
// poll answers the scripted codes in turn and then the session, and everything else is 401 without
// it. Every request's method, path and cookie is kept, so "it sent it once" is provable.
type oidcHost struct {
	polls   []string // the 400 codes the poll answers before the session, in order; a number is that bare status, "drop" a closed connection
	session string
	setup   bool // the host is in the setup state, as /auth/config says
	seen    []string
}

func (h *oidcHost) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie)
		h.seen = append(h.seen, r.Method+" "+r.URL.Path+" "+cookie.String())
		switch r.Method + " " + r.URL.Path {
		case "GET /auth/config":
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"oidc","org":"Meridian Insurance","device":true,"setup":`+fmt.Sprint(h.setup)+`}`)
		case "POST /auth/device":
			_, _ = io.WriteString(w, `{"device_code":"`+aSession("0d")+`","user_code":"K7QP-4RXM","verification_uri":"http://localhost:7777/#/device","verification_uri_complete":"http://localhost:7777/#/device?code=K7QP-4RXM","expires_in":600,"interval":5}`)
		case "POST /auth/device/token":
			var body struct {
				DeviceCode string `json:"device_code"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.DeviceCode != aSession("0d") {
				t.Errorf("the poll carried %q", body.DeviceCode)
			}
			if len(h.polls) > 0 {
				code := h.polls[0]
				h.polls = h.polls[1:]
				if status, err := strconv.Atoi(code); err == nil { // a proxy's answer while the host restarts: no envelope
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "<html>"+http.StatusText(status)+"</html>")
					return
				}
				if code == "drop" {
					conn, _, _ := http.NewResponseController(w).Hijack()
					_ = conn.Close()
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"`+code+`","message":"the host's words"}`)
				return
			}
			_, _ = io.WriteString(w, `{"session":"`+h.session+`","expires_at":"2026-10-20T09:00:00Z","subject":"sub-dana","display":"Dana Whitfield","org":"Meridian Insurance"}`)
		default: // everything behind the session stage
			switch {
			case cookie == nil || cookie.Value != h.session:
				writeUnauthenticated(w)
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			default:
				_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":null}`)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func writeUnauthenticated(w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"error":"unauthenticated","message":"sign in to Canyon"}`)
}

// timed runs one invocation on a clock that records every sleep instead of taking it, and moves
// its own time on by each one.
func timed(t *testing.T, dir string, args ...string) (out, errOut string, code int, slept []time.Duration) {
	t.Helper()
	var o, e strings.Builder
	env := map[string]string{"XDG_CONFIG_HOME": dir, "NO_COLOR": "1", "CANYON_ASCII": "1"}
	at := time.Now()
	a := &app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] },
		clock: clock{now: func() time.Time { return at }, sleep: func(d time.Duration) { slept = append(slept, d); at = at.Add(d) }}}
	code = a.main(args)
	return o.String(), e.String(), code, slept
}

func TestLoginWalksTheDeviceFlowAndHonoursSlowDown(t *testing.T) {
	dir, session := t.TempDir(), aSession("ab")
	h := &oidcHost{polls: []string{"authorization_pending", "slow_down", "authorization_pending"}, session: session}
	host := h.serve(t)
	out, errOut, code, slept := timed(t, dir, "login", host)
	if code != exitOK {
		t.Fatalf("login exited %d:\n%s", code, errOut)
	}
	want := "\n  Your code:   K7QP-4RXM\n\n  Open http://localhost:7777/#/device?code=K7QP-4RXM and approve it.\n  The code expires in 10 minutes.\n\n  Waiting...\n" +
		"\n  + Signed in to " + host + " as Dana Whitfield\n    Meridian Insurance\n\n  canyon fleet                    see what your org has already built\n"
	if got := errOut + out; got != want {
		t.Errorf("the transcript was:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(out+errOut, session) {
		t.Error("login printed the session value")
	}
	if got := strings.Join(h.seen, "\n"); got != "GET /auth/config \nPOST /auth/device \nPOST /auth/device/token \nPOST /auth/device/token \nPOST /auth/device/token \nPOST /auth/device/token " {
		t.Errorf("the host saw:\n%s", got)
	}
	if got := fmt.Sprint(slept); got != "[5s 5s 10s 10s]" { // every interval, and one more for each slow_down from the moment it was said
		t.Errorf("the polls waited %s", got)
	}
	if got := saved(t, dir); got.Host != host || got.Session != session {
		t.Errorf("the file holds host %q, and the session the host issued %v", got.Host, got.Session == session)
	}
	if got := perm(t, configAt(dir)); got != 0o600 {
		t.Errorf("login left the file at %v", got)
	}
}

// A host restarting while login waits is not an answer (design/AUTH.md §4.2): a proxy's 502, 503 or
// 504, or a connection that closes, is polled through, each wait twice the last up to 30 s, and the
// first real answer puts the interval back (QA batch 2026-10-07: a test cluster restart ended a waiting login
// on a bare "Service Unavailable").
func TestLoginWaitsThroughAHostRestarting(t *testing.T) {
	dir, session := t.TempDir(), aSession("ab")
	h := &oidcHost{polls: []string{"503", "502", "drop", "504", "authorization_pending"}, session: session}
	host := h.serve(t)
	out, errOut, code, slept := timed(t, dir, "login", host)
	if code != exitOK || !strings.Contains(out, "Signed in to "+host) {
		t.Fatalf("login exited %d:\n%s%s", code, errOut, out)
	}
	if got := fmt.Sprint(slept); got != "[5s 10s 20s 30s 30s 5s]" {
		t.Errorf("the polls waited %s", got)
	}
	if got := saved(t, dir); got.Session != session {
		t.Error("the file does not hold the session the host issued")
	}
}

// And a host that never comes back before the code expires ends the wait then, saying what it last
// answered and the line that starts again.
func TestLoginStopsWaitingForADownHostWhenTheCodeExpires(t *testing.T) {
	for poll, said := range map[string]string{"503": "503 Service Unavailable", "drop": "no answer"} {
		dir := t.TempDir()
		h := &oidcHost{polls: slices.Repeat([]string{poll}, 100), session: aSession("ab")}
		host := h.serve(t)
		out, errOut, code, slept := timed(t, dir, "login", host)
		var waited time.Duration
		for _, d := range slept {
			waited += d
		}
		want := "\n  x " + host + " stopped answering (" + said + "), and the code expired while Canyon waited.\n\n  Run: canyon login " + host + "\n"
		if code != exitError || out != "" || !strings.HasSuffix(errOut, want) {
			t.Errorf("%s: exited %d, stdout %q, stderr:\n%s", poll, code, out, errOut)
		}
		if waited < 600*time.Second || waited >= 630*time.Second {
			t.Errorf("%s: it gave up after %s of a 600 s code", poll, waited)
		}
		if _, err := os.Stat(configAt(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a login that never signed in wrote the config: %v", poll, err)
		}
	}
}

// Back from a restart, the wait is the interval slow_down left it at, and an answer that is not a
// restart — a 500 — still ends the login at once.
func TestARestartKeepsSlowDownAndA500StillEndsTheLogin(t *testing.T) {
	h := &oidcHost{polls: []string{"slow_down", "503", "authorization_pending"}, session: aSession("ab")}
	if _, errOut, code, slept := timed(t, t.TempDir(), "login", h.serve(t)); code != exitOK || fmt.Sprint(slept) != "[5s 10s 20s 10s]" {
		t.Errorf("exited %d after waits %v:\n%s", code, slept, errOut)
	}
	h = &oidcHost{polls: []string{"500"}, session: aSession("ab")}
	if _, errOut, code, slept := timed(t, t.TempDir(), "login", h.serve(t)); code != exitError || len(slept) != 1 || !strings.Contains(errOut, "Internal Server Error") {
		t.Errorf("a 500 exited %d after waits %v:\n%s", code, slept, errOut)
	}
}

// The setup line is said only on a host that is in setup: on any other it sends a person to ask
// about an install that finished long ago.
func TestACodeThatEndsWithoutASessionSaysWhich(t *testing.T) {
	for _, c := range []struct {
		code  string
		setup bool
		want  string
	}{
		{"expired_token", false, "The code expired before it was approved.\n\n  Run: canyon login "},
		{"expired_token", true, "The code expired before it was approved.\n\n  Canyon is still being set up here, so nobody can approve a sign-in yet — ask whoever installed it.\n\n  Run: canyon login "},
		{"access_denied", false, "That sign-in was denied in the browser."},
	} {
		dir := t.TempDir()
		h := &oidcHost{polls: []string{"authorization_pending", c.code}, session: aSession("ab"), setup: c.setup}
		host := h.serve(t)
		out, errOut, code, _ := timed(t, dir, "login", host)
		if code != exitError || out != "" || !strings.Contains(errOut, c.want) || strings.Contains(errOut, "set up") != c.setup {
			t.Errorf("%s: exited %d, stdout %q, stderr:\n%s", c.code, code, out, errOut)
		}
		if _, err := os.Stat(configAt(dir)); len(h.seen) != 4 || !errors.Is(err, os.ErrNotExist) { // the config, the grant, two polls, and no file
			t.Errorf("%s: after the terminal answer the host saw %q and the config is %v", c.code, h.seen, err)
		}
		verbsNamed(t, errOut)
	}
}

// An https host reads the session under __Host-canyon_session (cmd/canyon-server/session.go): a jar
// seeded under the plain name sends a cookie the host never reads, and every verb is 401.
func TestTheCookieIsNamedTheWayTheHostReadsIt(t *testing.T) {
	session := aSession("ab")
	var names []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range r.Cookies() {
			names = append(names, c.Name)
		}
		if c, err := r.Cookie("__Host-" + sessionCookie); err != nil || c.Value != session {
			writeUnauthenticated(w)
			return
		}
		_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":null}`)
	}))
	t.Cleanup(server.Close)
	c := newClient(server.URL)
	c.HTTP.Transport = server.Client().Transport
	c.seed(session)
	var page map[string]any
	if err := c.get(context.Background(), "/api/v1/fleet", &page); err != nil {
		t.Fatalf("an https host answered %v", err)
	}
	if strings.Join(names, ",") != "__Host-"+sessionCookie {
		t.Errorf("the request carried cookies %q", names)
	}
	if c.session() != session || sessionCookieOf(defaultHost) != sessionCookie {
		t.Error("the client reads the session back under the name it seeded, and a demo host over http keeps the plain name")
	}
}

// A 401 on any verb is one block and exit 1, after exactly one request: the next one would carry
// the same session and earn the same answer.
func TestASessionTheHostRefusesIsSaidOnceAndNeverRetried(t *testing.T) {
	dir := t.TempDir()
	h := &oidcHost{session: aSession("ab")}
	host := h.serve(t)
	writeConfig(t, dir, host, aSession("cd"), 0o600)
	out, errOut, code := run(t, xdg(dir), false, "fleet")
	if code != exitError || out != "" {
		t.Errorf("fleet exited %d with stdout %q", code, out)
	}
	if want := "\n  ✗ Your session has expired.\n\n  Run: canyon login " + host + "\n"; errOut != want {
		t.Errorf("stderr was:\n%q\nwant:\n%q", errOut, want)
	}
	if len(h.seen) != 1 {
		t.Errorf("the host saw %d requests: %q", len(h.seen), h.seen)
	}
	if got := saved(t, dir); got.Session != aSession("cd") {
		t.Error("a refused verb rewrote the file")
	}
}

func TestLogoutRevokesTheSessionBeforeForgettingIt(t *testing.T) {
	dir := t.TempDir()
	h := &oidcHost{session: aSession("ab")}
	host := h.serve(t)
	writeConfig(t, dir, host, h.session, 0o600)
	env := xdg(dir)
	env["CANYON_HOST"] = "http://elsewhere:7777" // the session is the saved host's, and that is where it is revoked
	out, errOut, code := run(t, env, false, "logout")
	if code != exitOK || errOut != "" || !strings.Contains(out, "Signed out of "+host) {
		t.Fatalf("logout exited %d:\n%s%s", code, out, errOut)
	}
	if strings.Join(h.seen, "\n") != "DELETE /api/v1/sessions "+sessionCookie+"="+h.session {
		t.Errorf("the host saw:\n%s", strings.Join(h.seen, "\n"))
	}
	if got := saved(t, dir); got.Host != host || got.Session != "" {
		t.Errorf("after logout the file holds host %q and a session %v", got.Host, got.Session != "")
	}
	if _, errOut, code := run(t, xdg(dir), false, "fleet"); code != exitError || !strings.Contains(errOut, "You're not signed in to "+host+".") { // signed out is not expired
		t.Errorf("the next fleet exited %d:\n%s", code, errOut)
	}
	if out, _, code := run(t, xdg(dir), false, "logout"); code != exitOK || !strings.Contains(out, "not signed in.\n\n  canyon login "+host) {
		t.Errorf("a second logout exited %d:\n%s", code, out)
	}
	for said, args := range map[string][]string{
		"logout takes no --host": {"logout", "--host", "http://b:7777"},
		"logout takes one host":  {"logout", "http://a:7777", "http://b:7777"},
		"must be http(s)://host": {"logout", "now"},
	} {
		if _, errOut, code := run(t, xdg(dir), false, args...); code != exitError || !strings.Contains(errOut, said) {
			t.Errorf("%v exited %d:\n%s", args, code, errOut)
		}
	}
}

func TestLogoutOfASessionTheHostAlreadyEndedStillForgetsIt(t *testing.T) {
	dir := t.TempDir()
	h := &oidcHost{session: aSession("ab")}
	host := h.serve(t)
	writeConfig(t, dir, host, aSession("cd"), 0o600) // not the one the host still has
	out, _, code := run(t, xdg(dir), false, "logout")
	if code != exitOK || !strings.Contains(out, "Signed out of "+host+" (that session had already expired)") {
		t.Errorf("logout exited %d:\n%s", code, out)
	}
	if got := saved(t, dir); got.Host != host || got.Session != "" {
		t.Errorf("the file holds host %q and a session %v", got.Host, got.Session != "")
	}
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	writeConfig(t, dir, gone.URL, aSession("cd"), 0o600)
	if _, errOut, code := run(t, xdg(dir), false, "logout"); code != exitError || !strings.Contains(errOut, "Can't reach") {
		t.Errorf("logout against a dead port exited %d:\n%s", code, errOut)
	}
	if got := saved(t, dir); got.Session != aSession("cd") {
		t.Error("a logout the host never heard forgot the session, leaving it live there")
	}
}

// The hint for a host that cannot be reached names a line that runs: logout takes no --host, so
// it is offered none.
func TestLogoutOffersNoHostFlagWhenItCannotReach(t *testing.T) {
	dir := t.TempDir()
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	writeConfig(t, dir, gone.URL, aSession("cd"), 0o600)
	_, errOut, code := run(t, xdg(dir), false, "logout")
	if code != exitError || !strings.Contains(errOut, "Can't reach") {
		t.Fatalf("logout against a dead port exited %d:\n%s", code, errOut)
	}
	if strings.Contains(errOut, "--host") || strings.Contains(errOut, "somewhere else") {
		t.Errorf("logout's hint offers a line logout refuses:\n%s", errOut)
	}
}

// A 401 to a machine that sent no session is not an expiry: it was never signed in to that host
// (cli.md §8.2, amended 2026-10-01).
func TestNoSessionSentIsNotSignedInNotExpired(t *testing.T) {
	h := &oidcHost{session: aSession("ab")}
	host := h.serve(t)
	_, errOut, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "CANYON_HOST": host}, false, "fleet")
	if want := "\n  ✗ You're not signed in to " + host + ".\n\n  Run: canyon login " + host + "\n"; code != exitError || errOut != want {
		t.Errorf("exit %d, stderr:\n%q\nwant:\n%q", code, errOut, want)
	}
}

// login's help names its argument and offers no flag that exists only to say it is not one.
func TestLoginHelpNamesItsURLAndNotTheTokenFlag(t *testing.T) {
	out, _, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "login", "--help")
	if code != exitOK || !strings.Contains(out, "\nUSAGE\n  canyon login [<url>] [flags]\n") || strings.Contains(out, "--token") || strings.Contains(out, "--host") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if _, errOut, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "login", "--token", "x"); code != exitError || !strings.Contains(errOut, "--token is not available") {
		t.Errorf("--token is refused still: exit %d, stderr %q", code, errOut)
	}
}
