package cli

import (
	"encoding/json"
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

// aSession is a value of the shape canyon-server mints — 32 bytes as 64 hex characters — and never
// one a host issued. No test touches the real HOME: the injected getenv points at a t.TempDir().
func aSession(pair string) string { return strings.Repeat(pair, 32) }

func xdg(dir string) map[string]string        { return map[string]string{"XDG_CONFIG_HOME": dir} }
func getenvIn(dir string) func(string) string { return func(k string) string { return xdg(dir)[k] } }
func configAt(dir string) string              { return filepath.Join(dir, "canyon", "config.json") }

// plant writes a config file verbatim at an exact mode, whatever the umask would have made of it:
// some of the fixtures are bodies the writer itself would refuse.
func plant(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	path := configAt(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfig(t *testing.T, dir, host, session string, mode os.FileMode) string {
	t.Helper()
	return plant(t, dir, fmt.Sprintf(`{"host":%q,"session":%q}`, host, session), mode)
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func saved(t *testing.T, dir string) config {
	t.Helper()
	body, err := os.ReadFile(configAt(dir))
	if err != nil {
		t.Fatal(err)
	}
	var c config
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// canyonHost answers the way a demo host does — auth off on /auth/config, the preferences behind
// it: it records the session every request carried, and mints the one it was given.
func canyonHost(t *testing.T, mint string, carried *[]string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie) // String() on a nil cookie is the empty string
		*carried = append(*carried, cookie.String())
		if r.URL.Path == "/auth/config" {
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"off","org":"","device":false}`)
			return
		}
		if mint != "" {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: mint, Path: "/", HttpOnly: true}) //nolint:gosec // Secure is what a host on plain loopback HTTP cannot set, which is the case under test
		}
		_, _ = io.WriteString(w, `{"stacks":[],"design_system":null,"setup_available":false}`)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A config another account can read is ignored whole, never repaired: a chmod would hide that the
// session may already have been taken.
func TestALooseConfigIsRefusedAndNotRepaired(t *testing.T) {
	dir := t.TempDir()
	var carried []string
	host := canyonHost(t, "", &carried)
	path := writeConfig(t, dir, host, aSession("ab"), 0o644)
	_, errOut, code := run(t, xdg(dir), false, "fleet", "--host", host)
	if code != exitOK {
		t.Fatalf("fleet exited %d: %s", code, errOut)
	}
	if len(carried) != 1 || carried[0] != "" {
		t.Errorf("a config other accounts can read still sent its session: %q", carried)
	}
	if !strings.Contains(errOut, path) || !strings.Contains(errOut, "chmod 600") {
		t.Errorf("the warning named neither the file nor the repair:\n%s", errOut)
	}
	if got := perm(t, path); got != 0o644 {
		t.Errorf("the file was repaired to %v instead of refused", got)
	}
	// The help resolves no host, so it reads no file. `version` does, because it prints the host.
	if out, errOut, code := run(t, xdg(dir), false); code != exitOK || errOut != "" || !strings.Contains(out, "USAGE") {
		t.Errorf("canyon alone exited %d and said %q", code, errOut)
	}
	if _, errOut, _ := run(t, xdg(dir), false, "version"); !strings.Contains(errOut, path) {
		t.Errorf("canyon version did not read the config it resolves a host from:\n%s", errOut)
	}
}

// A file written 0644 is a session readable by every account on the box, and the mode argument of
// an O_CREATE|O_TRUNC open does not touch a file that already exists.
func TestTheConfigIsWrittenAtSixHundredWhateverWasThere(t *testing.T) {
	dir := t.TempDir()
	getenv := getenvIn(dir)
	first := &config{Host: "http://box:7777", Session: aSession("ab")}
	if err := first.save(getenv); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, configAt(dir)); got != 0o600 {
		t.Errorf("a fresh config landed at %v", got)
	}
	if got := perm(t, filepath.Dir(configAt(dir))); got != 0o700 {
		t.Errorf("the directory landed at %v", got)
	}
	if err := os.Chmod(configAt(dir), 0o644); err != nil { //nolint:gosec // 0644 is the fault under test: a save must not leave it there
		t.Fatal(err)
	}
	second := &config{Host: "http://box:7777", Session: aSession("cd")}
	if err := second.save(getenv); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, configAt(dir)); got != 0o600 {
		t.Errorf("a save over a loose file left it at %v", got)
	}
	if err := (&config{Host: "http://box:7777", Session: "nope"}).save(getenv); err == nil {
		t.Error("a session that is not 64 hex characters was written")
	}
	if got := saved(t, dir); got.Session != aSession("cd") {
		t.Error("the refused write disturbed the file it would have replaced")
	}
	entries, err := os.ReadDir(filepath.Dir(configAt(dir)))
	if err != nil || len(entries) != 1 {
		t.Errorf("the saves left %d files in the directory (%v)", len(entries), err)
	}
}

// canyon.toml is committed to git and carries two keys, neither of them a host: a repository must
// not be able to point somebody else's CLI at a host of its choosing.
func TestNoFileBesideTheAppNamesTheHost(t *testing.T) {
	t.Chdir(t.TempDir())
	body := "app   = \"expense-tracker\"\nhost  = \"http://elsewhere.example.com\"\n"
	if err := os.WriteFile("canyon.toml", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveHost("", func(string) string { return "" }, &config{})
	if err != nil || got != defaultHost {
		t.Errorf("resolveHost read the directory's canyon.toml: %q, %v", got, err)
	}
}

// The stored session belongs to the stored host, or `--host b.example` hands b the cookie a issued.
// A host that matches no row for the cookie it was sent replaces it and the answer carries the new
// value: writing that back keeps one machine to one row, while the host is left alone, or a one-off
// `--host` would become sticky.
func TestTheStoredSessionIsForItsOwnHostAndIsKeptCurrent(t *testing.T) {
	dir, fresh := t.TempDir(), aSession("cd")
	var toSaved, toOther []string
	host := canyonHost(t, fresh, &toSaved)
	other := canyonHost(t, aSession("ef"), &toOther)
	stored := aSession("ab")
	writeConfig(t, dir, host+"/", stored, 0o600) // as an address bar leaves it: validHost normalises both sides
	out, errOut, code := run(t, xdg(dir), false, "fleet")
	if code != exitOK { // the saved host is the third rung
		t.Fatalf("fleet to the saved host exited %d: %s", code, errOut)
	}
	if strings.Contains(out+errOut, stored) || strings.Contains(out+errOut, fresh) {
		t.Error("a session value reached a stream")
	}
	if len(toSaved) != 1 || !strings.Contains(toSaved[0], stored) {
		t.Errorf("the saved host was not offered its own session: %q", toSaved)
	}
	if got := saved(t, dir); got.Session != fresh || got.Host != host {
		t.Errorf("after a rotation the file holds host %q, and the fresh session %v", got.Host, got.Session == fresh)
	}
	if _, errOut, code := run(t, xdg(dir), false, "fleet", "--host", other); code != exitOK {
		t.Fatalf("fleet to another host exited %d: %s", code, errOut)
	}
	if len(toOther) != 1 || toOther[0] != "" {
		t.Errorf("--host was handed the session another host issued: %q", toOther)
	}
	if got := saved(t, dir); got.Host != host || got.Session != fresh {
		t.Errorf("a call made with --host rewrote the file: %q", got.Host)
	}
	empty := t.TempDir() // a verb that only reads may never create the file; `canyon login` will
	if _, errOut, code := run(t, xdg(empty), false, "fleet", "--host", other); code != exitOK {
		t.Fatalf("fleet on a machine with no config exited %d: %s", code, errOut)
	}
	if _, err := os.Stat(configAt(empty)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a machine that has never logged in gained a config (%v)", err)
	}
}

// A host that failed the verb has not earned a rewrite. A 403 or a 500 carries Set-Cookie too, and
// a working session must not be replaced by the one a refusal handed back.
func TestAFailedCallDoesNotRewriteTheSession(t *testing.T) {
	dir, stored, handed := t.TempDir(), aSession("ab"), aSession("cd")
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: handed, Path: "/", HttpOnly: true}) //nolint:gosec // a demo host on loopback http sets no Secure, and what it hands back is the case under test
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(refuses.Close)
	writeConfig(t, dir, refuses.URL, stored, 0o600)
	if _, _, code := run(t, xdg(dir), false, "fleet"); code != exitError {
		t.Fatalf("a host answering 500 exited %d", code)
	}
	if got := saved(t, dir); got.Session != stored {
		t.Errorf("a failed call rewrote the session, and the file now holds what the 500 handed back: %v", got.Session == handed)
	}
}

// A file this CLI will not use says so in one line, names the way out, and is ignored whole:
// half-trusting a file somebody else may have written is worse than starting over.
func TestAConfigThisCLIWillNotUseSaysSoAndIsIgnored(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"not JSON at all", "{", "it is not the JSON Canyon writes"},
		{"a session no host minted", `{"host":"http://box:7777","session":"nope"}`, "not one a Canyon host minted"},
		{"a session of the wrong length", `{"session":"` + strings.Repeat("ab", 40) + `"}`, "not one a Canyon host minted"},
		{"a host Canyon cannot use", `{"host":"ftp://box","session":"` + aSession("ab") + `"}`, "must be http(s)://host[:port]"},
	} {
		dir := t.TempDir()
		path := plant(t, dir, c.body, 0o600)
		var errOut strings.Builder
		a := &app{out: io.Discard, err: &errOut, getenv: getenvIn(dir)}
		if got := a.loadConfig(); *got != (config{}) {
			t.Errorf("%s: the file was used anyway: %+v", c.name, got)
		}
		if line := errOut.String(); !strings.Contains(line, path) || !strings.Contains(line, c.want) || !strings.Contains(line, "rm "+path) {
			t.Errorf("%s: the line was:\n%s", c.name, line)
		}
	}
}

// $HOME/.config is the second rung. A machine with neither variable has nowhere to keep a session,
// which is not a fault to report: remember never reaches the writer, so the line about a session
// that could not be saved appears zero times rather than once per call.
func TestTheHomeRungAndAMachineWithNeither(t *testing.T) {
	home, stored := t.TempDir(), aSession("ab")
	var carried []string
	host := canyonHost(t, aSession("cd"), &carried)
	onlyHome := map[string]string{"HOME": home}
	writeConfig(t, filepath.Join(home, ".config"), host, stored, 0o600) // exactly where the rung says
	if _, errOut, code := run(t, onlyHome, false, "fleet"); code != exitOK {
		t.Fatalf("fleet under HOME exited %d: %s", code, errOut)
	}
	if len(carried) != 1 || !strings.Contains(carried[0], stored) {
		t.Errorf("the config under $HOME/.config was not read: %q", carried)
	}
	if _, errOut, code := run(t, nil, false, "fleet", "--host", host); code != exitOK || errOut != "" {
		t.Errorf("a machine with neither variable exited %d and said %q", code, errOut)
	}
}
