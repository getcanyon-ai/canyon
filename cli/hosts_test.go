package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionHost is a demo host that keeps sessions: /auth/config answers auth off and the public it
// is given, the preferences mint the next session for a caller that carries none, DELETE
// /api/v1/sessions ends the one it is sent, and every other route answers a session it minted —
// handing back rotate in its place once, when rotate is set, and running during first. The cookie is
// named by public, as canyon-server names it. Every request is kept.
type sessionHost struct {
	public string
	mint   []string
	rotate string
	during func()
	live   map[string]bool
	seen   []string
}

func (h *sessionHost) serve(t *testing.T) string {
	t.Helper()
	h.live = map[string]bool{}
	set := func(w http.ResponseWriter, value string) {
		h.live[value] = true
		http.SetCookie(w, &http.Cookie{Name: sessionCookieOf(h.public), Value: value, Path: "/", HttpOnly: true}) //nolint:gosec // a demo host on loopback http sets no Secure
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookieOf(h.public))
		h.seen = append(h.seen, r.Method+" "+r.URL.Path+" "+cookie.String())
		switch {
		case r.URL.Path == "/auth/config":
			_, _ = fmt.Fprintf(w, `{"canyon":true,"auth":"off","org":"","device":false,"public":%q}`, h.public)
		case r.URL.Path == "/api/v1/preferences" && cookie == nil && len(h.mint) > 0:
			set(w, h.mint[0])
			h.mint = h.mint[1:]
			_, _ = io.WriteString(w, `{"stacks":[]}`)
		case cookie == nil || !h.live[cookie.Value]:
			writeUnauthenticated(w)
		case r.Method == http.MethodDelete:
			delete(h.live, cookie.Value)
			w.WriteHeader(http.StatusNoContent)
		default:
			if h.rotate != "" {
				if h.during != nil {
					h.during()
				}
				delete(h.live, cookie.Value)
				set(w, h.rotate)
				h.rotate = ""
			}
			_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":null}`)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// last is the cookie the host's latest request carried: "" for none, and "<no request>" before any.
func (h *sessionHost) last() string {
	if len(h.seen) == 0 {
		return "<no request>"
	}
	seen := h.seen[len(h.seen)-1]
	return seen[strings.LastIndex(seen, " ")+1:]
}

func hostsAt(dir string) string { return filepath.Join(dir, "canyon", "hosts") }

// hostsIn is what hosts/ holds: nothing when there is no hosts/, or none this test can read.
func hostsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(hostsAt(dir))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// fileOf is the name §8.4 gives an httptest host's file: http, its address, and its port.
func fileOf(host string) string {
	return "http_" + strings.ReplaceAll(strings.TrimPrefix(host, "http://"), ":", "_") + ".json"
}

func cookieOf(session string) string { return sessionCookie + "=" + session }

func fleet(t *testing.T, env map[string]string, args ...string) {
	t.Helper()
	if _, errOut, code := run(t, env, false, append([]string{"fleet"}, args...)...); code != exitOK {
		t.Fatalf("fleet %v exited %d:\n%s", args, code, errOut)
	}
}

func login(t *testing.T, dir, host string) {
	t.Helper()
	if _, errOut, code := run(t, xdg(dir), false, "login", host); code != exitOK {
		t.Fatalf("login %s exited %d:\n%s", host, code, errOut)
	}
}

// Signing in to a second host keeps the first (cli.md §8.2): B becomes the default, as login has
// always made it, A moves to hosts/ with its session, and --host or CANYON_HOST reach A with A's.
// Signing in to A again moves B the same way, and A's old file, stale once config.json names A, is
// revoked and removed.
func TestLoginToASecondHostKeepsTheFirst(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1"), aSession("a2")}}
	b := &sessionHost{mint: []string{aSession("b1")}}
	hostA, hostB := a.serve(t), b.serve(t)
	login(t, dir, hostA)
	login(t, dir, hostB)
	fleet(t, xdg(dir), "--host", hostA)
	if a.last() != cookieOf(aSession("a1")) {
		t.Errorf("--host A after signing in to B carried %q", a.last())
	}
	env := xdg(dir)
	env["CANYON_HOST"] = hostA
	fleet(t, env)
	if a.last() != cookieOf(aSession("a1")) {
		t.Errorf("CANYON_HOST=A carried %q", a.last())
	}
	fleet(t, xdg(dir))
	if b.last() != cookieOf(aSession("b1")) {
		t.Errorf("the default host is B, and it was offered %q", b.last())
	}
	if got := saved(t, dir); got.Host != hostB || got.Session != aSession("b1") {
		t.Errorf("config.json names %q, holding B's session %v", got.Host, got.Session == aSession("b1"))
	}
	if got := hostsIn(t, dir); len(got) != 1 || got[0] != fileOf(hostA) {
		t.Fatalf("hosts/ holds %q", got)
	}
	file := filepath.Join(hostsAt(dir), fileOf(hostA))
	if perm(t, file) != 0o600 || perm(t, hostsAt(dir)) != 0o700 {
		t.Errorf("A's file is %v in a %v directory", perm(t, file), perm(t, hostsAt(dir)))
	}
	if body, _ := os.ReadFile(file); string(body) != fmt.Sprintf(`{"host":%q,"session":%q}`, hostA, aSession("a1")) { //nolint:gosec // the test's own temp dir
		t.Errorf("A's file is %s", body)
	}
	login(t, dir, hostA) // A is the default again, with a new session; B moves to hosts/
	if got := strings.Join(a.seen[len(a.seen)-1:], ""); got != "DELETE /api/v1/sessions "+cookieOf(aSession("a1")) {
		t.Errorf("A's old session, stale once config.json names A, was not revoked: A last saw %q", got)
	}
	if got := hostsIn(t, dir); len(got) != 1 || got[0] != fileOf(hostB) {
		t.Errorf("hosts/ holds %q", got)
	}
	fleet(t, xdg(dir), "--host", hostB)
	if b.last() != cookieOf(aSession("b1")) {
		t.Errorf("--host B after signing in to A again carried %q", b.last())
	}
}

// config.json keeps the bytes an older CLI reads and writes, and is authoritative for the host it
// names: a call to another host rotates that host's file and leaves config.json's bytes alone, and
// a hosts/ file for config.json's own host — an older CLI signed in there again — is never offered,
// and is revoked and removed by the next logout.
func TestConfigJSONIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1"), aSession("a3")}}
	b := &sessionHost{mint: []string{aSession("b1")}}
	hostA, hostB := a.serve(t), b.serve(t)
	b.public = hostB
	login(t, dir, hostA)
	login(t, dir, hostB)
	want := fmt.Sprintf(`{"host":%q,"public":%q,"session":%q}`, hostB, hostB, aSession("b1"))
	same := func(when string) {
		t.Helper()
		if body, _ := os.ReadFile(configAt(dir)); string(body) != want {
			t.Errorf("%s, config.json is\n%s\nwant\n%s", when, body, want)
		}
	}
	same("after login B")
	a.rotate = aSession("a2")
	fleet(t, xdg(dir), "--host", hostA)
	same("after A rotated its session")
	fleet(t, xdg(dir), "--host", hostA)
	if a.last() != cookieOf(aSession("a2")) {
		t.Errorf("the session A rotated was not kept in its file: the next call carried %q", a.last())
	}
	if out, errOut, code := run(t, xdg(dir), false, "logout", hostA); code != exitOK || !strings.Contains(out, "Signed out of "+hostA) {
		t.Fatalf("logout A exited %d:\n%s%s", code, out, errOut)
	}
	same("after logout A")
	if got := a.seen[len(a.seen)-1]; got != "DELETE /api/v1/sessions "+cookieOf(aSession("a2")) {
		t.Errorf("logout A did not revoke A's session: A last saw %q", got)
	}
	if got := hostsIn(t, dir); len(got) != 0 {
		t.Errorf("logout A left hosts/ holding %q", got)
	}
	// An older CLI signs in to B again while hosts/ still holds B: config.json is the current session.
	login(t, dir, hostA)
	b.live[aSession("b2")] = true
	old := fmt.Sprintf(`{"host":%q,"session":%q}`, hostB, aSession("b2"))
	plant(t, dir, old, 0o600)
	fleet(t, xdg(dir))
	if b.last() != cookieOf(aSession("b2")) {
		t.Errorf("config.json's host was offered %q, not config.json's session", b.last())
	}
	if out, errOut, code := run(t, xdg(dir), false, "logout"); code != exitOK || !strings.Contains(out, "Signed out of "+hostB) {
		t.Fatalf("logout exited %d:\n%s%s", code, out, errOut)
	}
	if b.live[aSession("b1")] || b.live[aSession("b2")] {
		t.Errorf("logout left B's sessions live: the stale file's %v, config.json's %v", b.live[aSession("b1")], b.live[aSession("b2")])
	}
	if got := hostsIn(t, dir); len(got) != 0 {
		t.Errorf("the stale file for B was not removed: %q", got)
	}
	if body, _ := os.ReadFile(configAt(dir)); string(body) != fmt.Sprintf(`{"host":%q}`, hostB) {
		t.Errorf("after logout config.json is %s", body)
	}
}

// One canonical form, from the URL alone: scheme and host lower-cased, the default port and a
// trailing / dropped. config.json's host is canonicalised on read and the file left as it is, and a
// hosts/ file is named for its host and refused when its name does not re-derive from it.
func TestCanonicalHost(t *testing.T) {
	for typed, want := range map[string]string{
		"HTTPS://Canyon.Meridian.EXAMPLE:443/": "https://canyon.meridian.example",
		"http://LocalHost:80":                  "http://localhost",
		"http://localhost:7777/":               "http://localhost:7777",
		"https://canyon.meridian.example:80":   "https://canyon.meridian.example:80",
		"http://canyon.meridian.example:443":   "http://canyon.meridian.example:443",
		"http://[::1]:80/":                     "http://[::1]",
		"https://canyon.meridian.example:0443": "https://canyon.meridian.example",
		"http://localhost:07777":               "http://localhost:7777",
	} {
		if got, err := validHost("--host", typed); err != nil || got != want {
			t.Errorf("%s is %q (%v), want %q", typed, got, err, want)
		}
	}
	for _, typed := range []string{"http://localhost:0", "http://localhost:65536"} {
		if got, err := validHost("--host", typed); err == nil {
			t.Errorf("%s is a host, %q", typed, got)
		}
	}

	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	hostA := a.serve(t)
	a.live[aSession("a0")] = true
	written := fmt.Sprintf(`{"host":%q,"session":%q}`, strings.ToUpper(hostA)+"/", aSession("a0"))
	plant(t, dir, written, 0o600) // as an older CLI may have left it
	fleet(t, xdg(dir), "--host", hostA)
	if a.last() != cookieOf(aSession("a0")) {
		t.Errorf("config.json's host, written another way, was offered %q", a.last())
	}
	if body, _ := os.ReadFile(configAt(dir)); string(body) != written {
		t.Errorf("reading config.json rewrote it: %s", body)
	}

	b := &sessionHost{mint: []string{aSession("b1")}}
	hostB := b.serve(t)
	login(t, dir, hostB)
	if got := hostsIn(t, dir); len(got) != 1 || got[0] != fileOf(hostA) {
		t.Fatalf("hosts/ holds %q, want %q", got, fileOf(hostA))
	}
	c := &sessionHost{}
	hostC := c.serve(t)
	misnamed := filepath.Join(hostsAt(dir), fileOf(hostC)) // C's name, holding A
	if err := os.Rename(filepath.Join(hostsAt(dir), fileOf(hostA)), misnamed); err != nil {
		t.Fatal(err)
	}
	_, errOut, _ := run(t, xdg(dir), false, "fleet", "--host", hostC)
	if c.last() != "" || !strings.Contains(errOut, misnamed) {
		t.Errorf("a file whose name its host doesn't re-derive was used: C was offered %q, and stderr said:\n%s", c.last(), errOut)
	}
}

// public names the cookie and never picks a session: a host answering another's public is offered
// only its own session, and one this machine never signed in to is offered none.
func TestPublicNeverPicksASession(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	hostA := a.serve(t)
	b := &sessionHost{mint: []string{aSession("b1")}, public: hostA}
	c := &sessionHost{public: hostA}
	hostB, hostC := b.serve(t), c.serve(t)
	login(t, dir, hostA)
	login(t, dir, hostB)
	fleet(t, xdg(dir))
	if b.last() != cookieOf(aSession("b1")) {
		t.Errorf("B, answering A's public, was offered %q", b.last())
	}
	fleet(t, xdg(dir), "--host", hostA)
	if a.last() != cookieOf(aSession("a1")) {
		t.Errorf("A was offered %q", a.last())
	}
	if _, _, code := run(t, xdg(dir), false, "fleet", "--host", hostC); code != exitError || c.last() != "" {
		t.Errorf("C, answering A's public and never signed in to, exited %d and was offered %q", code, c.last())
	}
}

// A redirect is never followed with the session: Go's jar matches cookies by host and not by port,
// so a hop to another port of the same address would carry it.
func TestARedirectLeavingTheHostCarriesNoSession(t *testing.T) {
	dir := t.TempDir()
	elsewhere := &sessionHost{}
	there := elsewhere.serve(t)
	moved := httptest.NewServer(http.RedirectHandler(there+"/api/v1/fleet", http.StatusTemporaryRedirect))
	t.Cleanup(moved.Close)
	writeConfig(t, dir, moved.URL, aSession("ab"), 0o600)
	if _, _, code := run(t, xdg(dir), false, "fleet"); code != exitError || len(elsewhere.seen) != 0 {
		t.Errorf("fleet exited %d, and the redirect's target saw %q", code, elsewhere.seen)
	}
}

// `canyon logout <url>` signs out of the host the URL names, in the canonical form: config.json's as
// plain logout when it names it, else its hosts/ file, which goes even when the host can't be reached
// — and that is said. A host this machine holds no session for says it is not signed in there and
// exits 0; a file other accounts can read has its session revoked first, and is removed with a line.
func TestLogoutNamesTheHostItSignsOutOf(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	b := &sessionHost{mint: []string{aSession("b1")}}
	hostA, hostB := a.serve(t), b.serve(t)
	login(t, dir, hostA)
	login(t, dir, hostB)
	out, errOut, code := run(t, xdg(dir), false, "logout", strings.ToUpper(hostB)+"/")
	if code != exitOK || errOut != "" || !strings.Contains(out, "Signed out of "+hostB+"\n") || b.live[aSession("b1")] {
		t.Errorf("logout of config.json's host, typed another way, exited %d with B's session live %v:\n%s%s", code, b.live[aSession("b1")], out, errOut)
	}
	if got := saved(t, dir); got.Host != hostB || got.Session != "" {
		t.Errorf("config.json names %q and holds a session %v", got.Host, got.Session != "")
	}
	if got := hostsIn(t, dir); len(got) != 1 || got[0] != fileOf(hostA) {
		t.Errorf("signing out of B touched hosts/: %q", got)
	}

	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	down := filepath.Join(hostsAt(dir), fileOf(gone.URL))
	if err := os.WriteFile(down, []byte(fmt.Sprintf(`{"host":%q,"session":%q}`, gone.URL, aSession("cd"))), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "\n  ! Removed this machine's session for " + gone.URL + ", but " + gone.URL + " couldn't be reached to end it: it works until it expires or an admin revokes it.\n"
	if out, errOut, code := run(t, xdg(dir), false, "logout", gone.URL); code != exitOK || out != "" || errOut != want {
		t.Errorf("logout of a host that is down exited %d, stdout %q, stderr:\n%q\nwant:\n%q", code, out, errOut, want)
	}
	if _, err := os.Stat(down); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file for a host that is down was kept: %v", err)
	}

	if out, _, code := run(t, xdg(dir), false, "logout", "http://never.example:7777"); code != exitOK || out != "\n  This machine is not signed in to http://never.example:7777.\n" {
		t.Errorf("logout of a host never signed in to exited %d:\n%q", code, out)
	}
	loose := filepath.Join(hostsAt(dir), fileOf(hostA))
	if err := os.Chmod(loose, 0o644); err != nil { //nolint:gosec // 0644 is the fault under test
		t.Fatal(err)
	}
	out, errOut, code = run(t, xdg(dir), false, "logout", hostA) // ending a session is not trusting it
	if code != exitOK || errOut != "\n  ! "+loose+": other accounts can read it, so it is removed.\n" || out != "\n  ✓ Signed out of "+hostA+"\n" || a.live[aSession("a1")] {
		t.Errorf("logout of a host whose file other accounts can read exited %d, left its session live %v:\n%s%s", code, a.live[aSession("a1")], out, errOut)
	}
	if got := hostsIn(t, dir); len(got) != 0 {
		t.Errorf("the refused file was kept: %q", got)
	}
	if out, _, code := run(t, xdg(dir), false, "logout", "--help"); code != exitOK || !strings.Contains(out, "\nUSAGE\n  canyon logout [<url>] [flags]\n") || strings.Contains(out, "--host") {
		t.Errorf("logout --help exited %d:\n%s", code, out)
	}
}

// A file's name is the canonical host in [a-z0-9.-], so no name is ambiguous, none needs escaping
// and none leaves hosts/; a host outside it, or whose name would pass 200 bytes, gets no file.
func TestAHostOutsideTheAlphabetGetsNoFile(t *testing.T) {
	long := "https://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + ".example"
	for host, want := range map[string]string{
		"https://canyon.meridian.example":     "https_canyon.meridian.example.json",
		"http://localhost:7777":               "http_localhost_7777.json",
		"http://127.0.0.1:7777":               "http_127.0.0.1_7777.json",
		"http://[::1]:7777":                   "",
		"http://xn--bcher-kva.example":        "http_xn--bcher-kva.example.json",
		"http://bücher.example":               "",
		"http://a_b.example":                  "",
		long:                                  "",
		"https://" + strings.Repeat("a", 189): "https_" + strings.Repeat("a", 189) + ".json", // 200 bytes
		"https://" + strings.Repeat("a", 190): "",                                            // 201
	} {
		if got := hostFile(host); got != want {
			t.Errorf("%s is filed as %q, want %q", host, got, want)
		}
	}
	dir := t.TempDir()
	login(t, dir, (&sessionHost{mint: []string{aSession("a1")}}).serve(t))
	writeConfig(t, dir, "http://[::1]:7777", aSession("cd"), 0o600)
	login(t, dir, (&sessionHost{mint: []string{aSession("b1")}}).serve(t))
	plant(t, dir, `{"host":"http://signed.out.example:7777"}`, 0o600) // signed out, it has nothing to keep
	login(t, dir, (&sessionHost{mint: []string{aSession("c1")}}).serve(t))
	if got := hostsIn(t, dir); len(got) != 0 {
		t.Errorf("a host that gets no file, or a signed-out one, was filed: %q", got)
	}
}

// The cookie a revoke carries is named by the host's public URL, as every other call's is: an https
// host reached over an http port-forward reads __Host-canyon_session, whether the session is
// config.json's, a hosts/ file's, or a stale one's.
func TestLogoutSendsTheCookieItsPublicNames(t *testing.T) {
	dir := t.TempDir()
	h := &sessionHost{public: "https://canyon.example.com", mint: []string{aSession("a1"), aSession("a2")}}
	p := &sessionHost{mint: []string{aSession("b1")}}
	host, plain := h.serve(t), p.serve(t)
	deleted := func(session string) string { return "DELETE /api/v1/sessions __Host-" + cookieOf(session) }
	login(t, dir, host)
	login(t, dir, plain)
	if _, errOut, code := run(t, xdg(dir), false, "logout", host); code != exitOK || h.seen[len(h.seen)-1] != deleted(aSession("a1")) {
		t.Errorf("logout <url> exited %d and the host last saw %q:\n%s", code, h.seen[len(h.seen)-1], errOut)
	}
	login(t, dir, host)
	if _, errOut, code := run(t, xdg(dir), false, "logout"); code != exitOK || h.seen[len(h.seen)-1] != deleted(aSession("a2")) {
		t.Errorf("logout exited %d and the host last saw %q:\n%s", code, h.seen[len(h.seen)-1], errOut)
	}
	h.live[aSession("a3")] = true
	stale := fmt.Sprintf(`{"host":%q,"public":%q,"session":%q}`, host, h.public, aSession("a3"))
	if err := os.WriteFile(filepath.Join(hostsAt(dir), fileOf(host)), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := run(t, xdg(dir), false, "logout"); code != exitOK || h.seen[len(h.seen)-1] != deleted(aSession("a3")) {
		t.Errorf("the stale sweep exited %d and the host last saw %q:\n%s", code, h.seen[len(h.seen)-1], errOut)
	}
}

// A stale hosts/ file holding config.json's own session is removed and not revoked: that session is
// the one this machine signs in with.
func TestAStaleFileHoldingTheCurrentSessionIsNotRevoked(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	hostA := a.serve(t)
	login(t, dir, hostA)
	if err := (&config{Host: hostA, Session: aSession("a1")}).saveHost(getenvIn(dir)); err != nil {
		t.Fatal(err)
	}
	run(t, xdg(dir), false, "logout", "http://elsewhere.example:7777")
	if !a.live[aSession("a1")] || len(hostsIn(t, dir)) != 0 {
		t.Errorf("the stale copy of config.json's session: revoked %v, files left %q", !a.live[aSession("a1")], hostsIn(t, dir))
	}
}

// An older CLI signs in to A again while hosts/ still holds A's old session; the next login to B moves
// A's current session to hosts/ over that file, so the old one is read first and revoked once B is
// saved.
func TestLoginRevokesTheStaleFileTheMoveWritesOver(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	b := &sessionHost{mint: []string{aSession("b1"), aSession("b2")}}
	hostA, hostB := a.serve(t), b.serve(t)
	login(t, dir, hostA)
	login(t, dir, hostB)
	a.live[aSession("a2")] = true
	plant(t, dir, fmt.Sprintf(`{"host":%q,"session":%q}`, hostA, aSession("a2")), 0o600) // the older CLI's login
	login(t, dir, hostB)
	if a.live[aSession("a1")] || !a.live[aSession("a2")] {
		t.Errorf("A's stale session is live %v, its current one %v", a.live[aSession("a1")], a.live[aSession("a2")])
	}
	fleet(t, xdg(dir), "--host", hostA)
	if a.last() != cookieOf(aSession("a2")) {
		t.Errorf("--host A carried %q", a.last())
	}
}

// A hosts/ this CLI refuses is never written into either: the login still signs in, and says the
// old host's session could not be kept.
func TestALoginThatCannotKeepTheOldHostSaysSo(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}}
	hostA := a.serve(t)
	login(t, dir, hostA)
	if err := os.Mkdir(hostsAt(dir), 0o755); err != nil { //nolint:gosec // the loose directory is the case under test
		t.Fatal(err)
	}
	b := &sessionHost{mint: []string{aSession("b1")}}
	hostB := b.serve(t)
	_, errOut, code := run(t, xdg(dir), false, "login", hostB)
	if code != exitOK || !strings.Contains(errOut, "The session for "+hostA+" could not be kept: "+hostsAt(dir)+": other accounts can enter it") {
		t.Errorf("login exited %d:\n%s", code, errOut)
	}
	if got := saved(t, dir); got.Host != hostB || len(hostsIn(t, dir)) != 0 {
		t.Errorf("config.json names %q and hosts/ holds %q", got.Host, hostsIn(t, dir))
	}
}

// A stale file whose host can't be reached is removed all the same, and that is said.
func TestAStaleFileForAHostThatIsDownIsRemovedAndSaid(t *testing.T) {
	dir := t.TempDir()
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	plant(t, dir, fmt.Sprintf(`{"host":%q}`, gone.URL), 0o600)
	if err := (&config{Host: gone.URL, Session: aSession("cd")}).saveHost(getenvIn(dir)); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, xdg(dir), false, "logout")
	want := "\n  ! Removed this machine's session for " + gone.URL + ", but " + gone.URL + " couldn't be reached to end it: it works until it expires or an admin revokes it.\n"
	if code != exitOK || errOut != want || !strings.Contains(out, "not signed in") || len(hostsIn(t, dir)) != 0 {
		t.Errorf("logout exited %d, left %q:\n%s%q", code, hostsIn(t, dir), out, errOut)
	}
}

// A hosts/ file is held to config.json's rules, and to two of its own: a regular file, not a link to
// one, in a hosts/ only this account can enter. Each refusal is one line, and no session is sent.
func TestAHostsFileThisCLIWillNotUseIsRefused(t *testing.T) {
	for _, c := range []struct{ name, want, host, session, how string }{
		{"not JSON", "it is not the JSON Canyon writes", "", "", "{"},
		{"a session no host minted", "not one a Canyon host minted", "", "nope", ""},
		{"a host with a username", "carries a username or password", "dana@", aSession("ab"), ""},
		{"a link", "it is not a regular file", "", aSession("ab"), "link"},
		{"a hosts/ others can enter", "other accounts can enter it", "", aSession("ab"), "0755"},
	} {
		dir := t.TempDir()
		h := &sessionHost{}
		host := h.serve(t)
		h.live[aSession("ab")] = true
		file, body := filepath.Join(hostsAt(dir), fileOf(host)), c.how
		if body != "{" {
			body = fmt.Sprintf(`{"host":%q,"session":%q}`, strings.Replace(host, "//", "//"+c.host, 1), c.session)
		}
		if err := os.MkdirAll(hostsAt(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		written := file
		if c.how == "link" {
			written = filepath.Join(dir, "elsewhere.json")
			if err := os.Symlink(written, file); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(written, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.how == "0755" {
			if err := os.Chmod(hostsAt(dir), 0o755); err != nil { //nolint:gosec // the loose directory is the case under test
				t.Fatal(err)
			}
		}
		_, errOut, _ := run(t, xdg(dir), false, "fleet", "--host", host)
		if h.last() != "" || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, hostsAt(dir)) {
			t.Errorf("%s: the host was offered %q, and stderr said:\n%s", c.name, h.last(), errOut)
		}
	}
}

// A rotated session is written back only over the file it was read from: another invocation that
// changed the file meanwhile — a login — keeps what it wrote.
func TestARotationDoesNotWriteOverAnotherInvocation(t *testing.T) {
	dir := t.TempDir()
	a := &sessionHost{mint: []string{aSession("a1")}, rotate: aSession("a2")}
	hostA := a.serve(t)
	login(t, dir, hostA)
	meanwhile := fmt.Sprintf(`{"host":"http://other.example:7777","session":%q}`, aSession("cd"))
	a.during = func() { plant(t, dir, meanwhile, 0o600) }
	fleet(t, xdg(dir))
	if body, _ := os.ReadFile(configAt(dir)); string(body) != meanwhile {
		t.Errorf("the rotation wrote over the login that ran meanwhile: %s", body)
	}
}
