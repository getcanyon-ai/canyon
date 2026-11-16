package cli

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// manifestFixture is the contract's golden for routes.md's skill/manifest row, served verbatim:
// the bytes a host answers, never a copy typed here.
func manifestFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/guardrails/preferences.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden.Manifest
}

// serveManifest answers every request with body under status and records what it was asked for.
func serveManifest(t *testing.T, status int, body []byte) (base string, asked *[]string) {
	t.Helper()
	return serveManifestVia(t, status, body, `{"via":"canyon","mode":"observe"}`)
}

// serveManifestVia is serveManifest whose GET /api/v1/egress answers egress, a 500 when it is "" (contracts/routes.md,
// Egress control): the host's via, which the agent's manifest reads for its outbound line. asked lists the
// manifest's reads alone.
func serveManifestVia(t *testing.T, status int, body []byte, egress string) (base string, asked *[]string) {
	t.Helper()
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/egress" {
			if egress == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal","message":"internal error"}`))
				return
			}
			_, _ = w.Write([]byte(egress))
			return
		}
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL, seen
}

// isolated is a working directory and a config directory of this test's own: no run below reads
// this machine's session or host.
func isolated(t *testing.T) map[string]string {
	t.Helper()
	t.Chdir(t.TempDir())
	return xdg(t.TempDir())
}

// The rendering, byte for byte, under CANYON_ASCII too: for a person the dashboard's labels and the
// host's values, one per line; for an agent — under ai, and as a session hook hands it over — the same
// values written for an agent, with no admin's heading and no rule ids, saying it need not ask again.
func TestManifestRendersTheFixtureByteForByte(t *testing.T) {
	fixture, want, agent := manifestFixture(t), golden(t, "manifest.txt"), golden(t, "manifest-agent.txt") // read before the directory moves
	env := isolated(t)
	base, asked := serveManifest(t, http.StatusOK, fixture)
	for name, c := range map[string]struct {
		args   []string
		golden string
	}{
		"human":       {[]string{"manifest", "--host", base}, want},
		"ai":          {[]string{"manifest", "--host", base, "--format", "ai"}, strings.Replace(agent, ", as canyon manifest prints them. They are in this session already: you need not run canyon manifest.\n", ".\n", 1)}, // the hook's line is the hook's
		"claude-code": {[]string{"manifest", "--host", base, "--harness", targetClaude}, agent},
		"codex":       {[]string{"manifest", "--host", base, "--harness", targetCodex}, agent}, // Codex's hook reads plain stdout, as Claude Code's does
	} {
		env["CANYON_ASCII"] = "1"
		out, errOut, code := run(t, env, true, c.args...)
		if code != exitOK || errOut != "" {
			t.Fatalf("%s: exit %d, stderr %q", name, code, errOut)
		}
		if out != c.golden {
			t.Errorf("%s rendered\n%s\nwant\n%s", name, out, c.golden)
		}
	}
	if want := "GET /api/v1/skill/manifest"; (*asked)[0] != want {
		t.Errorf("asked for %q, want %q", (*asked)[0], want)
	}
	delete(env, "CANYON_ASCII")
	env["CANYON_HOST"] = base // the shipped ladder with nothing typed: the environment names the host
	if out, errOut, code := run(t, env, false, "manifest"); code != exitOK || out != want || errOut != "" {
		t.Errorf("through CANYON_HOST: exit %d, stderr %q, rendered:\n%s", code, errOut, out)
	}
	delete(env, "CANYON_HOST")
	if _, errOut, code := run(t, env, false, "manifest", "--host", base, "--format", "json"); code != exitError || !strings.Contains(errOut, "manifest has no json rendering") {
		t.Errorf("--format json: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := run(t, env, false, "manifest", "--host", base, "--harness", "emacs"); code != exitError || !strings.Contains(errOut, "--harness takes claude-code, codex or cursor, the agents with a session hook") {
		t.Errorf("an unknown harness: exit %d, stderr %q", code, errOut)
	}
}

// A value is one line, whatever it carries: a policy name holding a newline, a tab and an escape
// comes out spelled, and the sections after it are still where they were.
func TestManifestValuesCannotForgeALine(t *testing.T) {
	env := isolated(t)
	body, err := json.Marshal(manifest{Host: "http://h", Policies: []manifestPolicy{{ID: "x", Name: "No secrets\nApproved stacks\n  Ruby\t\x1b[31m", State: "blocking"}},
		Stacks: []stack{{Language: "Go", Frameworks: []string{"Chi"}}}})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := serveManifest(t, http.StatusOK, body)
	out, _, code := run(t, env, false, "manifest", "--host", base)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want := "What your agents are handed\n\nCanyon host\nhttp://h\n\nPolicies\n" +
		`No secrets\nApproved stacks\n  Ruby\t\x1b[31m  blocking  x` + "\n\nApproved stacks\nGo\n  Chi\n\nProduction\npromoted from the CLI or the dashboard\n"
	if out != want {
		t.Errorf("rendered\n%q\nwant\n%q", out, want)
	}
}

// A section the org has not filled is absent: no bare label, and no sentence about the absence.
// Production is never empty: it says where a promotion is approved, whichever it is.
func TestManifestOmitsAnEmptySectionWhole(t *testing.T) {
	env := isolated(t)
	base, _ := serveManifest(t, http.StatusOK, []byte(`{"host":"http://h","policies":[],"component_types":[],"stacks":[],"design_system":null,"prod_approval":true}`))
	out, _, code := run(t, env, false, "manifest", "--host", base)
	if want := "What your agents are handed\n\nCanyon host\nhttp://h\n\nProduction\napproved by a person in the dashboard\n"; code != exitOK || out != want {
		t.Errorf("exit %d, rendered\n%q\nwant\n%q", code, out, want)
	}
}

// A session hook never fails a session: under --harness a closed host, a dead session and a host's
// refusal are each one line on stdout and exit 0. Without the flag they are §9's block, on stderr.
func TestManifestUnderAHarnessNeverFailsTheSession(t *testing.T) {
	env := isolated(t)
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	expired, _ := serveManifest(t, http.StatusUnauthorized, []byte(`{"error":"unauthorized","message":"sign in"}`))
	refused, _ := serveManifest(t, http.StatusServiceUnavailable, []byte(`{"error":"draining","message":"this host is\ndraining"}`))
	other, _ := serveManifest(t, http.StatusOK, []byte(`{"ok":true}`))
	for name, c := range map[string]struct{ host, line string }{
		"closed":     {gone.URL, goneLine(gone.URL)},
		"signed out": {expired, signedOutLine(expired)},                                  // no session was sent, so none expired
		"refused":    {refused, `Canyon: this host is\ndraining (503 draining).` + "\n"}, // login is not the way back from a host that answered
		"not Canyon": {other, "Canyon: " + other + " answered something that is not Canyon (HTTP 200). Nothing to do unless the user asks for Canyon. To remove Canyon from your coding agents: canyon skill uninstall.\n"},
	} {
		out, errOut, code := run(t, env, false, "manifest", "--host", c.host, "--harness", targetClaude)
		if code != exitOK || out != c.line || errOut != "" {
			t.Errorf("%s under --harness: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
		out, errOut, code = run(t, env, false, "manifest", "--host", c.host)
		if code != exitError || out != "" || errOut == "" {
			t.Errorf("%s without --harness: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
	}
	// What refuses the run before any request — a format the environment set, a host the config
	// cannot use — is the same one line under a harness, and the usage block without one.
	for name, c := range map[string]struct{ env, want string }{
		"CANYON_FORMAT=json":       {"CANYON_FORMAT=json", "Canyon: manifest has no json rendering.\n"},
		"a host that is not a URL": {"CANYON_HOST=box:7777", "must be http(s)://host[:port]"},
	} {
		k, v, _ := strings.Cut(c.env, "=")
		env[k] = v
		out, errOut, code := run(t, env, false, "manifest", "--harness", targetCursor)
		var envelope struct {
			AdditionalContext string `json:"additional_context"`
		}
		if code != exitOK || errOut != "" || json.Unmarshal([]byte(out), &envelope) != nil || !strings.Contains(envelope.AdditionalContext, c.want) || !strings.HasPrefix(envelope.AdditionalContext, "Canyon: ") {
			t.Errorf("%s under --harness cursor: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
		if out, _, code := run(t, env, false, "manifest"); code != exitError || out != "" {
			t.Errorf("%s without --harness: exit %d, stdout %q", name, code, out)
		}
		delete(env, k)
	}
}

// Cursor's hook reads JSON: one key, carrying the same bytes the plain form prints — the
// rendering, or the one line a dead host earns.
func TestManifestForCursorIsTheTextInAnEnvelope(t *testing.T) {
	fixture := manifestFixture(t)
	isolated(t)
	live, _ := serveManifest(t, http.StatusOK, fixture)
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	for _, host := range []string{live, gone.URL} {
		// a config directory of its own each: the hook says a gone host once a day, and each run is its first today
		plain, _, _ := run(t, xdg(t.TempDir()), false, "manifest", "--host", host, "--harness", targetClaude)
		out, errOut, code := run(t, xdg(t.TempDir()), false, "manifest", "--host", host, "--harness", targetCursor)
		if code != exitOK || errOut != "" {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		var envelope map[string]string
		if err := json.Unmarshal([]byte(out), &envelope); err != nil || len(envelope) != 1 || envelope["additional_context"] != plain {
			t.Errorf("--harness cursor printed %q (%v); want one key additional_context carrying %q", out, err, plain)
		}
	}
}

// goneLine is what a session hook says of a loopback host it cannot reach, the first time in a day:
// every httptest host is 127.0.0.1, so the line names the demo.
func goneLine(host string) string {
	return "Canyon: can't reach " + host + " (it may be down or gone). Nothing to do unless the user asks for Canyon. To use it again, start it (the demo: canyon demo up). To remove Canyon from your coding agents: canyon skill uninstall.\n"
}

// signedOutLine is what a session hook says of a host this machine has no session on.
func signedOutLine(host string) string {
	return "Canyon: this machine is not signed in to " + host + ". Nothing to do unless the user asks for Canyon; then they run canyon login " + host + ". To remove Canyon from your coding agents: canyon skill uninstall.\n"
}

// The line an agent reads at session start says what is wrong, that it asks nothing of the agent
// until the user asks for Canyon, and how a person removes Canyon: an agent must never set out to fix
// Canyon on its own, and a host that is gone must never nag (#2699's copy pass). A host it cannot
// reach — refused, unrouted, unresolved, or silent past the hook's deadline — is to be started, and a
// loopback one is the demo; a session that has ended is the user's to sign in to again. Those, an
// answer that is not Canyon's and a TLS failure are said once a day; a refusal in Canyon's own words
// is said every time, as it is.
func TestTheHookLineAsksNothingOfTheAgent(t *testing.T) {
	const remote = "https://canyon.example.com"
	for name, c := range map[string]struct {
		err   error
		line  string
		daily bool
	}{
		"refused, a remote host": {&Unreachable{Host: remote}, "Canyon: can't reach https://canyon.example.com (it may be down or gone). Nothing to do unless the user asks for Canyon. To use it again, start it. To remove Canyon from your coding agents: canyon skill uninstall.\n", true},
		"silent, localhost":      {&NoAnswer{Host: "http://localhost:7777", Within: hookWithin}, "Canyon: can't reach http://localhost:7777 (it may be down or gone). Nothing to do unless the user asks for Canyon. To use it again, start it (the demo: canyon demo up). To remove Canyon from your coding agents: canyon skill uninstall.\n", true},
		"refused, IPv6 loopback": {&Unreachable{Host: "http://[::1]:7777"}, goneLine("http://[::1]:7777"), true},
		"expired":                {&SessionExpired{Host: remote, Sent: true}, "Canyon: the session at https://canyon.example.com has expired. Nothing to do unless the user asks for Canyon; then they run canyon login https://canyon.example.com. To remove Canyon from your coding agents: canyon skill uninstall.\n", true},
		"signed out":             {&SessionExpired{Host: remote}, signedOutLine(remote), true},
		"not Canyon":             {&NotCanyon{Host: remote, Status: http.StatusFound}, "Canyon: https://canyon.example.com answered something that is not Canyon (HTTP 302). Nothing to do unless the user asks for Canyon. To remove Canyon from your coding agents: canyon skill uninstall.\n", true},
		"TLS":                    {transportError(remote, hookWithin, &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}), "Canyon: https://canyon.example.com: tls: failed to verify certificate: x509: certificate signed by unknown authority. Nothing to do unless the user asks for Canyon. To remove Canyon from your coding agents: canyon skill uninstall.\n", true},
		"Canyon's refusal":       {&APIError{Status: http.StatusServiceUnavailable, Code: "draining", Message: "draining"}, "Canyon: draining (503 draining).\n", false},
	} {
		if line, daily := hookLine(c.err); line != c.line || daily != c.daily {
			t.Errorf("%s: hookLine said (daily %v)\n%q\nwant (daily %v)\n%q", name, daily, line, c.daily, c.line)
		}
	}
}

// A session hook gives the host hookWithin, never the 30 s a person's verb waits: a host that takes
// the connection and never answers is a host it cannot reach, said once a day, and the session starts
// on time (#2703). The listener accepts and holds every connection without a byte back.
func TestTheHookDoesNotWaitOnAHostThatNeverAnswers(t *testing.T) {
	env := isolated(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	host := "http://" + ln.Addr().String()
	for n, want := range []string{goneLine(host), ""} {
		start := time.Now()
		out, errOut, code := run(t, env, false, "manifest", "--host", host, "--harness", targetClaude)
		if took := time.Since(start); took > hookWithin+2*time.Second || code != exitOK || errOut != "" || out != want {
			t.Errorf("session %d: took %s, exit %d, stdout %q, stderr %q; want within %s, exit 0, stdout %q", n+1, took, code, out, errOut, hookWithin, want)
		}
	}
}

// A host the session hook cannot reach is said once a day per host, whichever agent's hook asks (cli.md
// §7.4; the tour: after the demo was cleaned up, every session in every project began with it). So
// are a session that has ended, an answer that is not Canyon's, and a TLS failure. The sessions in
// between hear nothing, and every run exits 0. A refusal in Canyon's own words is said every session.
// A day later, or of another host, a line is said again; a record that cannot be read or written says
// it every time, as before there was one. Plain manifest is never quieted: a person who asks is told.
func TestTheSessionHookSaysAGoneHostOnceADay(t *testing.T) {
	env := isolated(t)
	gone, other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	other.Close()
	signedOut, _ := serveManifest(t, http.StatusUnauthorized, []byte(`{"error":"unauthorized","message":"sign in"}`))
	refused, _ := serveManifest(t, http.StatusServiceUnavailable, []byte(`{"error":"draining","message":"this host is\ndraining"}`))
	notCanyon, _ := serveManifest(t, http.StatusOK, []byte(`{"ok":true}`))
	untrusted := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshake this test fails on purpose is not news
	untrusted.StartTLS()
	t.Cleanup(untrusted.Close)
	hook := func(host, harness string) string {
		t.Helper()
		out, errOut, code := run(t, env, false, "manifest", "--host", host, "--harness", harness)
		if code != exitOK || errOut != "" {
			t.Errorf("--harness %s at %s: exit %d, stderr %q", harness, host, code, errOut)
		}
		return out
	}
	if out := hook(gone.URL, targetClaude); out != goneLine(gone.URL) {
		t.Errorf("the first session: %q", out)
	}
	if out := hook(gone.URL, targetClaude); out != "" {
		t.Errorf("the second session today: %q", out)
	}
	if out := hook(gone.URL, targetCodex); out != "" {
		t.Errorf("Codex's session, the same host: %q", out)
	}
	if out := hook(gone.URL, targetCursor); out != `{"additional_context":""}`+"\n" {
		t.Errorf("Cursor's session, the same host: %q", out)
	}
	if out := hook(other.URL, targetClaude); out != goneLine(other.URL) {
		t.Errorf("another host: %q", out)
	}
	if out, errOut, code := run(t, env, false, "manifest", "--host", gone.URL); code != exitError || out != "" || !strings.Contains(errOut, "Can't reach a Canyon host at "+gone.URL) {
		t.Errorf("plain manifest: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	for n := 1; n <= 2; n++ { // Canyon's own refusal: the host answered, and says so every session
		if out := hook(refused, targetClaude); out != `Canyon: this host is\ndraining (503 draining).`+"\n" {
			t.Errorf("a refusal, session %d: %q", n, out)
		}
	}
	for name, c := range map[string]struct{ host, line string }{
		"signed out": {signedOut, signedOutLine(signedOut)},
		"not Canyon": {notCanyon, "Canyon: " + notCanyon + " answered something that is not Canyon (HTTP 200). Nothing to do unless the user asks for Canyon. To remove Canyon from your coding agents: canyon skill uninstall.\n"},
	} {
		if out := hook(c.host, targetClaude); out != c.line {
			t.Errorf("%s, the first session: %q", name, out)
		}
		if out := hook(c.host, targetClaude); out != "" {
			t.Errorf("%s, the second session today: %q", name, out)
		}
	}
	// A certificate this machine does not trust: the platform's verifier words it, so the line is held
	// by its host in front and the two sentences behind.
	if out := hook(untrusted.URL, targetClaude); !strings.HasPrefix(out, "Canyon: "+untrusted.URL+": tls: ") ||
		!strings.HasSuffix(out, ". Nothing to do unless the user asks for Canyon. To remove Canyon from your coding agents: canyon skill uninstall.\n") {
		t.Errorf("TLS, the first session: %q", out)
	}
	if out := hook(untrusted.URL, targetClaude); out != "" {
		t.Errorf("TLS, the second session today: %q", out)
	}
	record := filepath.Join(env["XDG_CONFIG_HOME"], "canyon", "hook-lines.json")
	yesterday, err := json.Marshal(map[string]time.Time{goneLine(gone.URL): time.Now().Add(-25 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(record, yesterday, 0o600))
	if out := hook(gone.URL, targetClaude); out != goneLine(gone.URL) {
		t.Errorf("a day later: %q", out)
	}
	must(t, os.WriteFile(record, []byte("not the JSON Canyon writes"), 0o600))
	if out := hook(gone.URL, targetClaude); out != goneLine(gone.URL) {
		t.Errorf("a record it cannot read: %q", out)
	}
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a mode")
	}
	env = xdg(t.TempDir())
	locked := filepath.Join(env["XDG_CONFIG_HOME"], "canyon")
	must(t, os.Mkdir(locked, 0o500))                  // a config directory this user cannot add a file to
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // and back, so the temp directory can be removed
	for n := 1; n <= 2; n++ {
		if out := hook(gone.URL, targetClaude); out != goneLine(gone.URL) {
			t.Errorf("a record it cannot write, session %d: %q", n, out)
		}
	}
}

// An agent's manifest leaves out what asks nothing of it — an off policy, a section with no values — and
// says a state this build does not know as the host sent it, never dropping that policy; a value is one
// line whatever it carries.
// prod.human-approval is the last line's to say, in both modes: its name alone had agents tell the user a
// person on the team promotes, then promote it themselves (QA round 3).
func TestTheAgentsManifestSaysOnlyWhatAsksSomethingOfIt(t *testing.T) {
	env := isolated(t)
	body, err := json.Marshal(manifest{Host: "http://h", Policies: []manifestPolicy{
		{ID: "a", Name: "Blocks\n- forged", State: "blocking"}, {ID: "b", Name: "Switched off", State: "off"}, {ID: "c", Name: "Later", State: "staged"},
		{ID: "d", Name: "Warns, then blocks", State: "advisory", Production: "blocking"}, {ID: "e", Name: "Production's", State: "off", Production: "blocking"},
		{ID: "stack.blessed", Name: "Approved stacks only", State: "blocking"}, {ID: "prod.egress-allowed", Name: "Production allows everything preview connects to", State: "blocking"},
		{ID: "prod.human-approval", Name: "A person promotes", State: "blocking"}}})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := serveManifest(t, http.StatusOK, body)
	out, _, code := run(t, env, false, "manifest", "--host", base, "--format", "ai")
	want := "Canyon's settings for this org.\n" + // run by an agent: the session hook's line is the hook's alone
		"\nCanyon host: http://h\n\nPolicies that block:\n- Blocks\\n- forged\n\nPolicies that only warn:\n- Warns, then blocks (blocks production)\n" +
		"\nPolicies (staged):\n- Later\n\nPolicies for production only:\n- Production's (blocks)\n- Approved stacks only (blocks)\n- Production allows everything preview connects to (blocks)\n" +
		"\n" + outboundTold + "\n" + secretsHanded + "\n" +
		"\nProduction: when the user asks for production, you run canyon deploy --env prod (add --yes only when they asked in this conversation). It promotes; nobody else approves it.\n"
	if code != exitOK || out != want {
		t.Errorf("exit %d, rendered\n%q\nwant\n%q", code, out, want)
	}
	// Where a person approves production, the agent is told it runs the command, then hands the link over and
	// never approves it.
	body, _ = json.Marshal(manifest{Host: "http://h", ProdApproval: true})
	base, _ = serveManifest(t, http.StatusOK, body)
	out, _, _ = run(t, env, false, "manifest", "--host", base, "--format", "ai")
	if !strings.HasSuffix(out, "\nProduction: a person on the app's team approves it in Canyon's dashboard. When the user asks for production, you run canyon deploy --env prod; Canyon answers WAITING FOR APPROVAL with a link: give the link to the user and stop. Never open or approve it yourself.\n") {
		t.Errorf("on, rendered\n%s", out)
	}
}

const outboundTold = "Outbound: an app's environment may be under an allowlist, where Canyon refuses calls to any outside service not on it. Only a person on the app's team can allow a new one, in Canyon's dashboard; you cannot. Never try to reach a refused service another way. Canyon also refuses any call out to the internet to an IP address, a one-word host name or a port other than 80 and 443, and no one can allow it: call a host name on 80 or 443.\n"

// An agent adding a call to a new outside service learned only after its deploy that a person allows it (#2708), so
// the manifest says it once, above the production line, a session hook's text and --format ai alike, wherever the
// host's via is canyon (GET /api/v1/egress): where apps call out directly, Canyon refuses nothing. A via read that
// fails says nothing of it, and never fails the manifest. A person's rendering is the dashboard's values, and this is
// no value.
func TestTheAgentsManifestNamesOutboundOnce(t *testing.T) {
	env := isolated(t)
	for _, c := range []struct {
		name     string
		egress   string
		approval bool
		told     bool
	}{
		{"via canyon", `{"via":"canyon","mode":"observe"}`, false, true},
		{"via canyon, a person approves production", `{"via":"canyon","mode":"allowlist"}`, true, true},
		{"via off", `{"via":"off","mode":"allowlist"}`, false, false},
		{"a via read that fails", "", false, false},
	} {
		body, err := json.Marshal(manifest{Host: "http://h", ProdApproval: c.approval})
		if err != nil {
			t.Fatal(err)
		}
		base, _ := serveManifestVia(t, http.StatusOK, body, c.egress)
		for _, args := range [][]string{{"--format", "ai"}, {"--harness", targetClaude}} {
			out, errs, code := run(t, env, false, append([]string{"manifest", "--host", base}, args...)...)
			told := strings.Count(out, outboundTold) == 1 && strings.Contains(out, "\n\n"+outboundTold+"\n"+secretsHanded+"\n\nProduction: ")
			if code != exitOK || errs != "" || told != c.told || !c.told && strings.Contains(out, "allowlist") {
				t.Errorf("%s, %q: exit %d, stderr %q, rendered\n%s", c.name, args, code, errs, out)
			}
		}
		if out, _, _ := run(t, env, false, "manifest", "--host", base); strings.Contains(out, "allowlist") {
			t.Errorf("%s: a person's rendering says it:\n%s", c.name, out)
		}
	}
}

// An AI twin checks its scanner rule's policy where no scanner reads the language, so the agent reads that
// policy once, under the stricter of the two states this app holds, with one line saying each side's when
// they differ: listed twice, it both blocked and only warned (#2694). A twin whose scanner rule is not
// handed out, a rule off before preview, and a state outside the contract keep a line each. The cases are
// testdata/manifest-twins.json's, which the dashboard's tests read too, so the two renderings agree.
func TestTheAgentsManifestSaysAnAITwinsPolicyOnce(t *testing.T) {
	raw, err := os.ReadFile("testdata/manifest-twins.json")
	var table struct {
		Cases []struct {
			Name     string   `json:"name"`
			Manifest manifest `json:"manifest"`
			Agent    string   `json:"agent"`
		} `json:"cases"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &table)
	}
	if err != nil || len(table.Cases) == 0 {
		t.Fatalf("the shared table: %v", err)
	}
	for _, c := range table.Cases {
		c.Manifest.App = "x"
		if out := agentManifest(c.Manifest, true); !strings.Contains(out, "\nCanyon host: http://h\n\n"+c.Agent+"\n\n") {
			t.Errorf("%s: rendered\n%s\nwant within it\n%s", c.Name, out, c.Agent)
		}
	}
}

// In an app's directory the manifest asks for the app, and the app's own stricter rules are said
// after the org's, with who set them, for an agent and for a person (cli.md §7.4): an agent told the
// org's states alone said "this rule only warns" of a rule the app holds as blocking (QA batch 2).
func TestManifestInAnAppSaysTheAppsOwnRules(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/guardrails/preferences.json")
	var golden struct {
		ManifestApp json.RawMessage `json:"manifest_app"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &golden)
	}
	if err != nil {
		t.Fatal(err)
	}
	env := isolated(t)
	base, asked := serveManifest(t, http.StatusOK, golden.ManifestApp)
	inProject(t, "claims-portal")
	env["CANYON_HOST"] = base
	for args, want := range map[string]string{
		"--format ai":    "\nThis app (claims-portal) holds these stricter than the org's, set by its team:\n- Every route needs sign-in: blocks (set by Olive Park)\n  To pass: On every route that isn't public, verify the Authorization: Bearer token Canyon sends; don't add a login.\n",
		"--format human": "\nThis app (claims-portal)\nEvery route needs sign-in  blocking  auth.unauthenticated-route  set by Olive Park\n",
	} {
		out, errs, code := run(t, env, false, append([]string{"manifest"}, strings.Fields(args)...)...)
		if code != exitOK || errs != "" || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, stderr %q, missing %q in\n%s", args, code, errs, want, out)
		}
	}
	for _, got := range *asked {
		if got != "GET /api/v1/skill/manifest?app=claims-portal" {
			t.Errorf("asked %q", got)
		}
	}
}

// Outside an app's directory it asks for the org's manifest alone, as before.
func TestManifestOutsideAnAppAsksForTheOrgsAlone(t *testing.T) {
	fixture := manifestFixture(t) // before the directory moves
	env := isolated(t)
	base, asked := serveManifest(t, http.StatusOK, fixture)
	env["CANYON_HOST"] = base
	if _, errs, code := run(t, env, false, "manifest", "--format", "ai"); code != exitOK || len(*asked) != 1 || (*asked)[0] != "GET /api/v1/skill/manifest" {
		t.Errorf("exit %d, stderr %q, asked %q", code, errs, *asked)
	}
}

// The session hook refreshes a skill another canyon wrote, so the agent reads the text of the canyon it
// runs: the file becomes skill install's, and the hook says so once, first. A current file, a missing
// one and a person's file with no block are left alone and add nothing (cli.md §7.4; the 2026-10-08 QA
// rerun: an upgraded canyon kept the old skill on disk).
func TestTheSessionHookRefreshesAnOlderSkill(t *testing.T) {
	fixture, agent := manifestFixture(t), golden(t, "manifest-agent.txt")
	home, _ := harnessHome(t)
	env := xdg(t.TempDir())
	env["HOME"], env["CANYON_ASCII"] = home, "1"
	base, _ := serveManifest(t, http.StatusOK, fixture)
	skill := filepath.Join(home, ".claude", "skills", "canyon", "SKILL.md")
	must(t, os.MkdirAll(filepath.Dir(skill), 0o750), os.WriteFile(skill, []byte(claudeFront+oldBlock()), 0o600))
	want, err := skillText(targetClaude)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude)
	if code != exitOK || errOut != "" || out != "Canyon updated its skill to the one canyon "+Version+" ships.\n"+agent {
		t.Errorf("exit %d, stderr %q, out:\n%s", code, errOut, out)
	}
	if got := fileBytes(t, skill); got != want {
		t.Errorf("the skill file is still:\n%s", got)
	}
	must(t, os.WriteFile(skill, []byte(claudeFront+oldBlock()), 0o600))
	env["CANYON_FORMAT"] = "json" // a format the hook refuses: the refresh comes first all the same
	if out, _, code := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); code != exitOK || !strings.HasPrefix(out, "Canyon updated its skill") || fileBytes(t, skill) != want {
		t.Errorf("under a refused format: exit %d, out:\n%s", code, out)
	}
	delete(env, "CANYON_FORMAT")
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent { // current now: nothing to say
		t.Errorf("a current skill: out:\n%s", out)
	}
	codex := filepath.Join(home, ".agents", "skills", "canyon", "SKILL.md") // Codex's skill, refreshed by Codex's hook alike
	must(t, os.MkdirAll(filepath.Dir(codex), 0o750), os.WriteFile(codex, []byte(claudeFront+oldBlock()), 0o600))
	if out, _, code := run(t, env, false, "manifest", "--host", base, "--harness", targetCodex); code != exitOK || out != "Canyon updated its skill to the one canyon "+Version+" ships.\n"+agent || fileBytes(t, codex) != want {
		t.Errorf("Codex's older skill: exit %d, out:\n%s\nfile:\n%s", code, out, fileBytes(t, codex))
	}
	must(t, os.WriteFile(skill, []byte("my own notes\n"), 0o600))
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent || fileBytes(t, skill) != "my own notes\n" {
		t.Errorf("a file with no block: out:\n%s", out)
	}
	must(t, os.Remove(skill))
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent {
		t.Errorf("no skill file: out:\n%s", out)
	}
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Errorf("the hook wrote a skill nobody installed: %v", err)
	}
}

// The session hook moves Canyon's entry an older install wrote in shell form to exec form, this binary
// by its path, and says so once: the entry is replaced where it stands and every other hook and key
// is left as it was. A second run, plain manifest, a group a person built around Canyon's command,
// and a binary main found no path for each leave the file alone (cli.md §7.4; QA round 5: an upgrade
// kept the bare canyon that fails with exit 127 where canyon is not on the PATH).
func TestTheSessionHookMovesAShellFormEntryToExecForm(t *testing.T) {
	fixture, agent := manifestFixture(t), golden(t, "manifest-agent.txt")
	home, _ := harnessHome(t)
	env := xdg(t.TempDir())
	env["HOME"], env["CANYON_ASCII"] = home, "1"
	base, _ := serveManifest(t, http.StatusOK, fixture)
	was := Self
	t.Cleanup(func() { Self = was })
	Self = "/opt/canyon/bin/canyon"
	theirs := `{"matcher":"startup","hooks":[{"type":"command","command":"echo hello & welcome"}]}`
	settings := filepath.Join(home, ".claude", "settings.json")
	shell := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo pre <tool>"}]}],` +
		`"SessionStart":[` + theirs + `,{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"canyon manifest --harness claude-code"}]}]}}`
	write := func(body string) {
		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		must(t, os.WriteFile(settings, []byte(body), 0o600), os.Chtimes(settings, old, old))
	}
	untouched := func(what, body string) {
		t.Helper()
		if info, err := os.Stat(settings); err != nil || fileBytes(t, settings) != body || info.ModTime().Year() != 2020 {
			t.Errorf("%s: the file was touched:\n%s", what, fileBytes(t, settings))
		}
	}

	write(shell)
	out, errOut, code := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude)
	if code != exitOK || errOut != "" || out != "Canyon updated its session hook to run /opt/canyon/bin/canyon directly.\n"+agent {
		t.Errorf("exit %d, stderr %q, out:\n%s", code, errOut, out)
	}
	f := readHooks(t, settings)
	start := f.Hooks["SessionStart"]
	if len(start) != 2 || compact(t, start[0]) != theirs || compact(t, start[1]) != strings.Replace(claudeHookEntry, "/usr/local/bin/canyon", Self, 1) ||
		len(f.Hooks["PreToolUse"]) != 1 || !strings.Contains(fileBytes(t, settings), `"model": "opus"`) {
		t.Errorf("after the hook ran:\n%s", fileBytes(t, settings))
	}
	migrated := fileBytes(t, settings)
	write(migrated)
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent {
		t.Errorf("a second run: out:\n%s", out)
	}
	untouched("a second run", migrated)

	write(shell)
	if _, _, code := run(t, env, false, "manifest", "--host", base, "--format", "ai"); code != exitOK {
		t.Errorf("plain manifest exited %d", code)
	}
	untouched("plain manifest", shell)

	theirsAround := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"canyon manifest --harness claude-code"},{"type":"command","command":"echo mine"}]}]}}`
	write(theirsAround)
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent {
		t.Errorf("a group a person built: out:\n%s", out)
	}
	untouched("a group a person built", theirsAround)

	dotfiles := filepath.Join(t.TempDir(), "settings.json")
	must(t, os.Remove(settings), os.WriteFile(dotfiles, []byte(shell), 0o600), os.Symlink(dotfiles, settings))
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent {
		t.Errorf("a symlinked file: out:\n%s", out)
	}
	if info, err := os.Lstat(settings); err != nil || info.Mode()&os.ModeSymlink == 0 || fileBytes(t, dotfiles) != shell {
		t.Errorf("a symlinked file was replaced or rewritten: %v, %v", info, err)
	}
	must(t, os.Remove(settings))

	Self = "canyon"
	write(shell)
	if out, _, _ := run(t, env, false, "manifest", "--host", base, "--harness", targetClaude); out != agent {
		t.Errorf("no path for this binary: out:\n%s", out)
	}
	untouched("no path for this binary", shell)
}

// An agent reads the host's line on how to pass each Canyon rule it was handed one for, once, and the
// text of each of the org's own rules it was handed, as its admin wrote it, every line indented under
// the rule's name, so none reads as a heading. A rule the host handed neither for keeps its name
// alone, and each string passes through cell, so a line of the host's cannot forge another (#2622).
func TestManifestSaysHowToPassWhatBlocks(t *testing.T) {
	env := isolated(t)
	body, err := json.Marshal(manifest{Host: "http://h", Policies: []manifestPolicy{
		{ID: "secrets.client-bundle", Name: "No secrets in the code", State: "blocking", How: "Keep secrets out.\nPolicies that block:"},
		{ID: "custom.audit-log", Name: "Every write is logged", State: "blocking", Instructions: "Log every write to the audit table.\nPolicies that only warn:\n\x1b[31mInclude who did it."},
		{ID: "deps.known-vuln", Name: "No known-vulnerable dependencies", State: "advisory"},
		{ID: "custom.naming", Name: "Tables are plural", State: "advisory"},
		{ID: "code.injection", Name: "No user input reaching commands", State: "advisory", Production: "blocking", How: "Pass input as a parameter."},
		{ID: "prod.clean-gate", Name: "Production needs a passing review", State: "blocking", How: "Fix what blocks."},
	}, AppAdditions: []appAddition{
		{ID: "secrets.client-bundle", Name: "No secrets in the code", Production: "blocking", SetBy: actorRef{Display: "Olive"}, How: "Keep secrets out.\nPolicies that block:"},
		{ID: "auth.unauthenticated-route", Name: "Every route needs sign-in", State: "blocking", SetBy: actorRef{Display: "Olive"}, How: "Check who calls."},
		{ID: "custom.naming", Name: "Tables are plural", State: "blocking", SetBy: actorRef{Display: "Olive"}, Instructions: "Name every table in the plural."},
		{ID: "custom.readme", Name: "Has a README", State: "blocking", SetBy: actorRef{Display: "Olive"}, Instructions: "Every app has a README.md.", Says: "The app has a README.md at its root.\nPolicies that block:"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := serveManifest(t, http.StatusOK, body)
	inProject(t, "x")
	out, errs, code := run(t, env, false, "manifest", "--host", base, "--format", "ai")
	if code != exitOK || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	want := "\nPolicies that block:\n" +
		"- No secrets in the code\n  To pass: Keep secrets out.\\nPolicies that block:\n" +
		"- Every write is logged\n" +
		"\nPolicies that only warn:\n" +
		"- No known-vulnerable dependencies\n" +
		"- Tables are plural\n" +
		"- No user input reaching commands (blocks production)\n  To pass: Pass input as a parameter.\n" +
		"\nPolicies for production only:\n" +
		"- Production needs a passing review (blocks)\n  To pass: Fix what blocks.\n" +
		"\nYour org's own rules (written by its admins):\n" +
		"- Every write is logged\n  Log every write to the audit table.\n  Policies that only warn:\n  \\x1b[31mInclude who did it.\n" +
		"- Tables are plural\n  Name every table in the plural.\n" +
		"- Has a README\n  Every app has a README.md.\n  Canyon checks this by code: The app has a README.md at its root.\\nPolicies that block:\n" +
		"\nThis app (x) holds these stricter than the org's, set by its team:\n" +
		"- No secrets in the code: blocks production (set by Olive)\n" +
		"- Every route needs sign-in: blocks (set by Olive)\n  To pass: Check who calls.\n" +
		"- Tables are plural: blocks (set by Olive)\n" +
		"- Has a README: blocks (set by Olive)\n"
	if !strings.Contains(out, want) {
		t.Errorf("rendered\n%s\nwant within it\n%s", out, want)
	}
}
