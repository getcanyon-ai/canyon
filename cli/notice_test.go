package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	installLine = "canyon v0.5.0 is out; you have v0.4.1. Download it from " + releases
	aDay        = 24 * time.Hour
)

var noon = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// stamped makes this binary a release for one test; "dev" never notices.
func stamped(t *testing.T, v string) {
	t.Helper()
	was := Version
	Version = v
	t.Cleanup(func() { Version = was })
}

// hostAt is a host whose /auth/config names cli 0.5.0 and counts how often it was asked.
func hostSaying(t *testing.T, cli string) (base string, asked *atomic.Int32) {
	t.Helper()
	asked = new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/auth/config" {
			t.Errorf("asked %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(`{"canyon":true,"auth":"off","org":"Acme","device":false,"cli":"` + cli + `","public":"http://localhost:7777"}`))
	}))
	t.Cleanup(server.Close)
	return server.URL, asked
}

// settled is a stdout that waits for the check before the verb's first line lands, so the answer is
// there when the verb ends and the test can say what the read found. The read itself stays
// non-blocking: TestTheNoticeNeverDelaysTheVerb holds the host and watches the verb leave.
func noticeApp(env map[string]string, at time.Time, tty bool) (a *app, errOut *strings.Builder) {
	errOut = new(strings.Builder)
	a = &app{err: errOut, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, tty: tty,
		clock: clock{now: func() time.Time { return at }}}
	return a, errOut
}

// verb runs `canyon version` at the given instant and answers stderr, once the goroutine — if one
// was started — is done: none outlives its test.
func verb(t *testing.T, env map[string]string, at time.Time, tty bool, args ...string) (out, errOut string, code int) {
	t.Helper()
	a, e := noticeApp(env, at, tty) // root.go's real ordering: a loopback host answers well inside the grace
	o := new(strings.Builder)
	a.out = o
	code = a.main(append([]string{"version"}, args...))
	if a.notice != nil {
		<-a.notice.done
	}
	return o.String(), e.String(), code
}

func statePath(t *testing.T, env map[string]string) string {
	t.Helper()
	return filepath.Join(env["XDG_CONFIG_HOME"], "canyon", noticeFile)
}

func TestTheNoticeSpeaksOnceADay(t *testing.T) {
	stamped(t, "v0.4.1")
	env := isolated(t)
	base, asked := hostSaying(t, "v0.5.0")
	env["CANYON_HOST"] = base
	out, errOut, code := verb(t, env, noon, true)
	if code != exitOK || !strings.HasPrefix(out, "Canyon CLI v0.4.1 ") {
		t.Fatalf("version exited %d and printed %q", code, out)
	}
	if want := "\n  · " + installLine + "\n"; errOut != want {
		t.Errorf("first run said\n%q\nwant\n%q", errOut, want)
	}
	state, err := os.ReadFile(statePath(t, env))
	if err != nil || !strings.Contains(string(state), `"checked":"2026-09-21T12:00:00Z"`) {
		t.Errorf("the state file after the first run: %q, %v", state, err)
	}
	if _, errOut, _ := verb(t, env, noon.Add(23*time.Hour+59*time.Minute), true); errOut != "" {
		t.Errorf("a minute short of a day later the verb still said %q", errOut)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("two runs asked the host %d times, want 1", n)
	}
	if _, errOut, _ := verb(t, env, noon.Add(aDay), true); !strings.Contains(errOut, installLine) {
		t.Errorf("a day later the verb said %q", errOut)
	}
	if err := os.WriteFile(statePath(t, env), []byte("not the JSON canyon writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, _ := verb(t, env, noon.Add(aDay), true); !strings.Contains(errOut, installLine) {
		t.Errorf("with a malformed state file the verb said %q", errOut)
	}
	for _, mine := range []string{"v0.5.0", "v0.5.0-3-gabcdef", "v0.10.0", "dev"} { // the host's release, one past it, ten past it, and no release: none is older
		stamped(t, mine)
		if err := os.Remove(statePath(t, env)); err != nil {
			t.Fatal(err)
		}
		if _, errOut, _ := verb(t, env, noon, true); errOut != "" {
			t.Errorf("at %s the verb said %q", mine, errOut)
		}
	}
	stamped(t, "v0.9.0") // numerically, not as text: 0.10.0 is out
	env["CANYON_HOST"], _ = hostSaying(t, "v0.10.0")
	if _, errOut, _ := verb(t, env, noon.Add(aDay), true); !strings.Contains(errOut, "canyon v0.10.0 is out; you have v0.9.0.") {
		t.Errorf("against a 0.10.0 host a 0.9.0 binary said %q", errOut)
	}
}

func TestTheNoticeIsSilentToAnAgent(t *testing.T) {
	stamped(t, "v0.4.1")
	base, asked := hostSaying(t, "v0.5.0")
	for name, tc := range map[string]struct {
		env  map[string]string
		tty  bool
		args []string
	}{
		"CLAUDECODE":              {map[string]string{"CLAUDECODE": "1"}, true, nil},
		"--format ai":             {nil, true, []string{"--format", "ai"}},
		"--format json":           {nil, true, []string{"--format", "json"}},
		"CANYON_NO_UPDATE_NOTICE": {map[string]string{noticeVar: "1"}, true, nil},
		"stderr not a terminal":   {nil, false, nil},
	} {
		env := isolated(t)
		env["CANYON_HOST"] = base
		for k, v := range tc.env {
			env[k] = v
		}
		if _, errOut, code := verb(t, env, noon, tc.tty, tc.args...); code != exitOK || errOut != "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
		if _, err := os.Stat(statePath(t, env)); err == nil {
			t.Errorf("%s: a silent run wrote the state file", name)
		}
		if n := asked.Load(); n != 0 { // silent means request-free: `--format` after the verb's name included
			t.Errorf("%s: a silent run asked the host %d times", name, n)
		}
	}
}

func TestTheNoticeNeverDelaysTheVerb(t *testing.T) {
	stamped(t, "v0.4.1")
	env := isolated(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"canyon":true,"auth":"off","cli":"v0.5.0"}`))
	}))
	t.Cleanup(server.Close)
	env["CANYON_HOST"] = server.URL
	a, errOut := noticeApp(env, noon, true)
	var out strings.Builder
	a.out = &out
	start := time.Now()
	code := a.main([]string{"version"}) // the host has not answered: the verb leaves after the grace, not the deadline
	if waited := time.Since(start); waited < 100*time.Millisecond || waited >= time.Second {
		t.Errorf("the verb waited %s for a host that had not answered; want the grace and no more", waited)
	}
	close(release)
	<-a.notice.done
	if code != exitOK || !strings.HasPrefix(out.String(), "Canyon CLI v0.4.1 ") || errOut.String() != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(statePath(t, env)); err == nil {
		t.Error("an unanswered check wrote the state file")
	}
}

func TestTheNoticeNamesAStaleSkillBlock(t *testing.T) {
	env := isolated(t) // Version is "dev": no host is asked, and the block line needs none
	env["HOME"] = t.TempDir()
	skill := filepath.Join(env["HOME"], ".claude", "skills", "canyon", "SKILL.md")
	if err := os.MkdirAll(filepath.Join(env["HOME"], ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := run(t, env, false, "skill", "install", "--harness", targetClaude); code != exitOK {
		t.Fatalf("skill install exited %d: %s", code, errOut)
	}
	if _, errOut, _ := verb(t, env, noon, true); errOut != "" {
		t.Errorf("with this binary's block installed the verb said %q", errOut)
	}
	// An older canyon installed this: the block's marker carries the stamp of its own, older text.
	if err := os.WriteFile(skill, []byte(skillFronts[targetClaude]+oldBlock()), 0o600); err != nil { //nolint:gosec // test: a path under t.TempDir()
		t.Fatal(err)
	}
	if _, errOut, _ := verb(t, env, noon.Add(aDay), true); errOut != "\n  · "+staleLine+"\n" {
		t.Errorf("with an older block installed the verb said %q", errOut)
	}
	// The blocks are read after the verb: the install that just rewrote an older block is not
	// told, in the same breath, that the block is older.
	if err := os.Remove(statePath(t, env)); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := run(t, env, true, "skill", "install", "--harness", targetClaude); code != exitOK || strings.Contains(errOut, staleLine) {
		t.Errorf("skill install over an older block exited %d and said %q", code, errOut)
	}
}

// Both lines at once, in that order under one blank line; and a host that gave no answer still
// ends the pass, so a down host is not asked again for a day and the block line is not repeated.
func TestTheNoticeSaysBothLinesOnceAndADownHostEndsThePass(t *testing.T) {
	stamped(t, "v0.4.1")
	env := isolated(t)
	env["HOME"] = t.TempDir()
	base, _ := hostSaying(t, "v0.5.0")
	env["CANYON_HOST"] = base
	skill := filepath.Join(env["HOME"], ".claude", "skills", "canyon", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte(skillFronts[targetClaude]+oldBlock()), 0o600); err != nil { //nolint:gosec // test: a path under t.TempDir()
		t.Fatal(err)
	}
	if _, errOut, _ := verb(t, env, noon, true); errOut != "\n  · "+installLine+"\n  · "+staleLine+"\n" {
		t.Errorf("with both to say the verb said %q", errOut)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	env["CANYON_HOST"] = gone.URL
	if _, errOut, _ := verb(t, env, noon.Add(aDay), true); errOut != "\n  · "+staleLine+"\n" {
		t.Errorf("with the host down the verb said %q", errOut)
	}
	if _, errOut, _ := verb(t, env, noon.Add(aDay+time.Hour), true); errOut != "" {
		t.Errorf("an hour after a down host the verb said %q", errOut)
	}
}
