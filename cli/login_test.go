package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// verbsNamed reads the commands back out of what was printed the way a reader does, and holds every
// line to the rule the help follows: it may not offer a verb this binary does not dispatch.
func verbsNamed(t *testing.T, printed string) {
	t.Helper()
	fields := strings.Fields(printed)
	for i, field := range fields {
		if field != "canyon" || i+1 == len(fields) {
			continue
		}
		verb := fields[i+1]
		if strings.HasPrefix(verb, "-") { // `canyon --version` is a flag, not a verb
			continue
		}
		found := false
		for _, c := range commands {
			found = found || c.name == verb
		}
		if !found {
			t.Errorf("the output offers `canyon %s`, which the dispatch slice does not have:\n%s", verb, printed)
		}
	}
}

// First contact is made with an empty jar. A host whose auth is off serves that caller as the anonymous
// person and sets no cookie (contracts/routes.md), so its preferences are the proof this is Canyon, the
// file holds no session, and the host that goes in it is the one validHost returned rather than the URL
// the user typed (#1673).
func TestLoginProvesAnAuthOffHostByItsPreferences(t *testing.T) {
	dir := t.TempDir()
	var carried []string
	host := canyonHost(t, "", &carried)
	out, errOut, code := run(t, xdg(dir), false, "login", strings.ToUpper(host)+"/") // as an address bar leaves it
	if code != exitOK || errOut != "" {
		t.Fatalf("login exited %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "Connected to "+host) {
		t.Errorf("login printed:\n%s", out)
	}
	verbsNamed(t, out+errOut)
	if got := saved(t, dir); got.Host != host || got.Session != "" {
		t.Errorf("the file holds host %q and a session %q no host set", got.Host, got.Session)
	}
	if got := perm(t, configAt(dir)); got != 0o600 {
		t.Errorf("login left the file at %v", got)
	}
	if strings.Join(carried, ",") != "," { // the config read, then the proof: both with nothing in the jar
		t.Errorf("login's first contact was not made with an empty jar: %q", carried)
	}
}

// A host that still mints a session keeps it: the file holds what it set, never printed.
func TestLoginKeepsTheSessionAHostMints(t *testing.T) {
	dir, session := t.TempDir(), aSession("ab")
	var carried []string
	out, errOut, code := run(t, xdg(dir), false, "login", canyonHost(t, session, &carried))
	if code != exitOK || strings.Contains(out+errOut, session) {
		t.Fatalf("login exited %d, or printed the session:\n%s%s", code, out, errOut)
	}
	if got := saved(t, dir); got.Session != session {
		t.Errorf("the file lost the session the host minted")
	}
}

// A host whose public URL is https names its cookie __Host-, however it is reached: through an http
// port-forward, login reads the name off GET /auth/config's public, keeps the session it set under it,
// and every later verb sends it back (#964). Named from the typed http host, it was never read again.
func TestAPortForwardedHTTPSHostKeepsItsSession(t *testing.T) {
	dir, session, public := t.TempDir(), aSession("cd"), "https://canyon.example.com"
	var carried []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie("__Host-" + sessionCookie)
		carried = append(carried, cookie.String())
		if r.URL.Path == "/auth/config" {
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"off","org":"","device":false,"public":"`+public+`"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-" + sessionCookie, Value: session, Path: "/", HttpOnly: true}) //nolint:gosec // the server sets Secure only under TLS, and this forward is http
		_, _ = io.WriteString(w, `{"stacks":[],"design_system":null,"setup_available":false}`)
	}))
	t.Cleanup(host.Close)
	if _, errOut, code := run(t, xdg(dir), false, "login", host.URL); code != exitOK {
		t.Fatalf("login exited %d: %s", code, errOut)
	}
	if got := saved(t, dir); got.Session != session || got.Public != public {
		t.Fatalf("the file holds the session %v and public %q", got.Session == session, got.Public)
	}
	carried = nil
	if _, errOut, code := run(t, xdg(dir), false, "fleet"); code != exitOK || len(carried) != 1 || !strings.Contains(carried[0], session) {
		t.Errorf("fleet exited %d (%s) and sent %q, not the session login kept", code, errOut, carried)
	}
}

// The empty jar is the whole proof, so login builds its own client. Through a.client() a machine
// that already had a session for this host would carry it, and the preferences would answer for that
// session rather than prove the host serves a stranger.
func TestLoginProvesTheHostEvenWhenThisMachineHasASession(t *testing.T) {
	dir, stored := t.TempDir(), aSession("ab")
	var carried []string
	host := canyonHost(t, "", &carried)
	writeConfig(t, dir, host, stored, 0o600)
	if _, errOut, code := run(t, xdg(dir), false, "login", host); code != exitOK {
		t.Fatalf("login exited %d: %s", code, errOut)
	}
	if strings.Join(carried, ",") != "," {
		t.Errorf("first contact carried the session this machine already had: %q", carried)
	}
}

// Nothing is written before the host has proved it is Canyon, and nothing at all for an invocation
// that never reached one.
func TestLoginWritesNothingItCouldNotEarn(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	off, oidc := `{"canyon":true,"auth":"off"}`, `{"canyon":true,"auth":"oidc"}`
	var ignored []string
	badCookie := canyonHost(t, "x", &ignored) // the preferences, and a cookie of no shape a host mints
	for _, c := range []struct {
		name string
		args []string
		want []string // §9's shape: what happened, and the line to run instead
	}{
		{"a host that fails", []string{"login", broken.URL}, []string{"Internal Server Error"}},
		{"an answer with no preferences in it", []string{"login", answering(t, off, `{"stacks":null}`)}, []string{"is not Canyon"}}, // the key alone is not the shape: the contract says a list
		{"preferences with no stacks in them", []string{"login", answering(t, off, `{"tiles":{}}`)}, []string{"is not Canyon"}},
		{"a host that answers /auth/config 401", []string{"login", answering(t, "", off)}, []string{"is not Canyon"}},
		{"a host that says auth is off and then 401", []string{"login", answering(t, off, "")}, []string{"is not Canyon"}},
		{"a host that says it is not Canyon", []string{"login", answering(t, `{"canyon":false,"auth":"oidc"}`, "")}, []string{"is not Canyon"}},
		{"an auth this binary does not know", []string{"login", answering(t, `{"canyon":true,"auth":"saml"}`, "")}, []string{"is not Canyon"}},
		{"a device grant with nothing in it", []string{"login", answering(t, oidc, `{}`)}, []string{"is not Canyon"}},
		{"a cookie of the wrong shape", []string{"login", badCookie}, []string{"is not Canyon"}},
		{"--host beside the argument", []string{"login", "http://box:7777", "--host", "http://b:7777"}, []string{"login names its host as an argument"}},
		{"--host instead of the argument", []string{"login", "--host", "http://b:7777"}, []string{"login names its host as an argument"}},
		{"--host before the verb", []string{"--host", "http://b:7777", "login", "http://a:7777"}, []string{"login names its host as an argument"}},
		{"a token whose variable was unset", []string{"login", "--token", "", "http://box:7777"}, []string{"--token is not available."}},
		{"no host named", []string{"login"}, []string{
			"canyon login needs a host when nothing can answer the prompt.",
			"\n  canyon login http://box:7777\n"}},
		{"a token there is nothing to do with", []string{"login", "--token", "unused"}, []string{
			"canyon login --token is not available.",
			"Canyon issues no tokens; a person signs in and the session is theirs.",
			"Sign in instead:  canyon login http://box:7777"}},
		{"a host that is not a URL", []string{"login", "box:7777"}, []string{"must be http(s)://host[:port]"}},
	} {
		dir, before := t.TempDir(), aSession("ab")
		writeConfig(t, dir, "http://box:7777", before, 0o600)
		out, errOut, code := run(t, xdg(dir), false, c.args...)
		if code != exitError || out != "" {
			t.Errorf("%s: exited %d with stdout %q", c.name, code, out)
		}
		for _, want := range c.want {
			if !strings.Contains(errOut, want) {
				t.Errorf("%s: stderr lost %q:\n%s", c.name, want, errOut)
			}
		}
		verbsNamed(t, errOut)
		if strings.Contains(errOut, "expired") {
			t.Errorf("%s: login spoke of a session expiring, inside the verb that would mint one:\n%s", c.name, errOut)
		}
		if got := saved(t, dir); got.Host != "http://box:7777" || got.Session != before {
			t.Errorf("%s: a login that did not finish rewrote the file: host %q", c.name, got.Host)
		}
	}
}

// answering is a host that says what the case needs on /auth/config and, on every other path, the
// second body — or 401 where a body is "": everything a login can be refused by, short of the wire.
func answering(t *testing.T, config, rest string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := rest
		if r.URL.Path == "/auth/config" {
			body = config
		}
		if body == "" {
			writeUnauthenticated(w)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// §9's "running somewhere else" line has to be a line that runs: login names its host as an
// argument, so the --host form every other verb is offered would exit 1 here.
func TestADeadPortOffersLoginTheLineLoginTakes(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	dir := t.TempDir()
	_, errOut, code := run(t, xdg(dir), false, "login", gone.URL)
	if code != exitError || !strings.Contains(errOut, "Can't reach a Canyon host at "+gone.URL) {
		t.Errorf("a dead port exited %d and printed:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "  canyon login https://canyon.example.com\n") || strings.Contains(errOut, "--host") {
		t.Errorf("the line it offered was not one login takes:\n%s", errOut)
	}
	verbsNamed(t, errOut)
}

// A hint names the host this machine would otherwise reach, so the one on offer is the one the
// reader is already pointed at rather than the demo they may never run.
func TestAHintNamesTheHostThisMachineWouldReach(t *testing.T) {
	fresh := t.TempDir()
	if _, errOut, _ := run(t, xdg(fresh), false, "login"); !strings.Contains(errOut, "canyon login "+defaultHost) {
		t.Errorf("with nothing saved the hint was:\n%s", errOut)
	}
	dir := t.TempDir()
	writeConfig(t, dir, "http://saved:7777", aSession("ab"), 0o600)
	if _, errOut, _ := run(t, xdg(dir), false, "login"); !strings.Contains(errOut, "canyon login http://saved:7777") {
		t.Errorf("with a saved host the hint was:\n%s", errOut)
	}
	env := xdg(dir)
	env["CANYON_HOST"] = "https://canyon.example.com"
	if _, errOut, _ := run(t, env, false, "login"); !strings.Contains(errOut, "canyon login https://canyon.example.com") {
		t.Errorf("with CANYON_HOST set the hint was:\n%s", errOut)
	}
}

// Nothing is claimed until the file holds it: a machine with nowhere to write must not be told it
// is connected to anything.
func TestNoSuccessIsPrintedUntilTheFileHoldsIt(t *testing.T) {
	var carried []string
	host := canyonHost(t, aSession("ab"), &carried)
	out, errOut, code := run(t, nil, false, "login", host) // neither XDG_CONFIG_HOME nor HOME
	if code != exitError || strings.Contains(out, "Connected") {
		t.Errorf("login exited %d and printed:\n%s", code, out)
	}
	if !strings.Contains(errOut, "nowhere to keep a session") {
		t.Errorf("the failure did not say why:\n%s", errOut)
	}
}

func TestThePromptOffersTheStoredHostAndEnterTakesIt(t *testing.T) {
	dir, minted, stored := t.TempDir(), aSession("ef"), aSession("ab")
	var carried []string
	host := canyonHost(t, minted, &carried)
	writeConfig(t, dir, host, stored, 0o600)
	out, errOut, code := answer(t, "\n", xdg(dir), true, "login")
	if code != exitOK {
		t.Fatalf("Enter at the prompt exited %d:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "  Canyon host ["+host+"]: ") {
		t.Errorf("the prompt did not offer the host this machine saved:\n%q", errOut)
	}
	if strings.Contains(out, "Canyon host") {
		t.Errorf("the prompt was written to stdout, which is the answer's stream alone:\n%s", out)
	}
	if !strings.Contains(out, "Connected to "+host) {
		t.Errorf("Enter did not accept the host the prompt offered:\n%s", out)
	}
	if strings.Contains(out+errOut, stored) || strings.Contains(out+errOut, minted) {
		t.Error("a session value reached a stream")
	}
	if got := saved(t, dir); got.Host != host || got.Session != minted {
		t.Errorf("after the prompt the file holds host %q, and the session the host minted %v", got.Host, got.Session == minted)
	}
	verbsNamed(t, out+errOut)
}

func TestThePromptOffersTheDemoWhenNothingIsSaved(t *testing.T) {
	prompt := "\n  Canyon host [" + defaultHost + "]: "
	out, errOut, code := answer(t, "\n", xdg(t.TempDir()), true, "login")
	if !strings.HasPrefix(errOut, prompt) {
		t.Fatalf("with nothing saved the prompt was:\n%q", errOut)
	}
	// Whatever answers the default port today, the call Enter started is the one that named it.
	// Nothing answers the demo port here, so the call fails at the host — never at the prompt.
	if got := out + strings.TrimPrefix(errOut, prompt); !strings.Contains(got, defaultHost) || strings.Contains(got, "needs a host") {
		t.Errorf("Enter did not carry the offered host into the call (exit %d):\n%s", code, got)
	}
}

func TestWhatIsTypedAtThePromptIsTheHostLoginUses(t *testing.T) {
	var carried []string
	host := canyonHost(t, aSession("cd"), &carried)
	for _, c := range []struct {
		name, input, want string
		code              int
	}{
		{"a host typed as an address bar leaves it", strings.ToUpper(host) + "/\n", "Connected to " + host, exitOK},
		{"a host that is not a URL", "box:7777\n", `the host you named "box:7777" must be http(s)://host[:port]`, exitError},
		{"a terminal that closed without a line", "", "canyon login needs a host when nothing can answer the prompt.", exitError},
	} {
		dir := t.TempDir()
		out, errOut, code := answer(t, c.input, xdg(dir), true, "login")
		if code != c.code || !strings.Contains(out+errOut, c.want) {
			t.Errorf("%s: exited %d and said:\n%s%s", c.name, code, out, errOut)
		}
		verbsNamed(t, out+errOut)
		if c.code != exitOK {
			if _, err := os.Stat(configAt(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: a login that did not finish left a config (%v)", c.name, err)
			}
			continue
		}
		if got := saved(t, dir); got.Host != host { // the host the CLI resolved, not the URL that was typed
			t.Errorf("%s: the file holds host %q", c.name, got.Host)
		}
	}
}
