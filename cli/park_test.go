package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// park parks the narration: json throws it away, ai keeps it only for a run with no result to
// render, and so no block, human never parks. unpark hands stdout back and replays only when asked.
func TestParkParksTheNarrationPerFormat(t *testing.T) {
	for _, c := range []struct {
		format Format
		keep   bool
		want   string
	}{
		{FormatJSON, true, ""}, {FormatJSON, false, ""},
		{FormatAI, true, "narration"}, {FormatAI, false, ""},
		{FormatHuman, true, "narration"},
	} {
		var out strings.Builder
		a := &app{out: &out, render: Render{Format: c.format}}
		a.park()
		a.park() // a second park is a no-op, never a leak of the real stdout
		a.printf("narration")
		a.unpark(c.keep)
		a.unpark(c.keep) // a second unpark is a no-op too
		a.printf("")
		if out.String() != c.want {
			t.Errorf("%s keep=%v: stdout %q, want %q", c.format, c.keep, out.String(), c.want)
		}
		if a.live != nil || a.parked != nil {
			t.Errorf("%s: unpark left stdout parked", c.format)
		}
	}
}

// The verb defers the unpark, and the ai verdict has already dropped what was parked: a clean
// check under ai prints §5.3's block and nothing of the transcript above it.
func TestTheBlockReplacesTheTranscriptWhenTheVerbReturns(t *testing.T) {
	dir := freshDir(t, "claims-portal")
	must(t, os.WriteFile(filepath.Join(dir, projectFile), []byte("app = \"claims-portal\"\n"), 0o600))
	h := &gateHost{project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page:  `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`,
		run:   aRun("clean", 0, 0)}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(dir)
	out, errOut, code := answer(t, "", map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_FORMAT": "ai"}, false, "check")
	if code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr:\n%s\nstdout:\n%s", code, errOut, out)
	}
	if !strings.HasPrefix(out, "CANYON REVIEW — RESULT: PASSED\n\napp: claims-portal\nhost: "+server.URL+"\ngate run: gr_8f2a (2026-09-02T09:31:04Z)\nfindings: 0 · blocking: 0\n"+aiCheckedOutcome+"\n"+aiCheckedSteps) {
		t.Errorf("the clean check under ai did not open on §5.3's block:\n%s", out)
	}
	for _, leak := range []string{"canyon " + Version, "Package", "Passed"} {
		if strings.Contains(out, leak) {
			t.Errorf("the human narration %q leaked into the block:\n%s", leak, out)
		}
	}
}

// outcome is the one home of the exit code: every state, both verbs, and whether a verdict
// renders at all. The human goldens pin the prints; this pins the numbers.
func TestOutcomeIsTheOneHomeOfTheExitCode(t *testing.T) {
	for _, c := range []struct {
		name    string
		verb    string
		state   string
		shipped bool
		code    int
		renders bool
	}{
		{"clean check", "check", "clean", false, exitOK, true},
		{"clean deploy that shipped", "deploy", "clean", true, exitOK, true},
		{"clean deploy that shipped nothing", "deploy", "clean", false, exitError, false},
		{"blocked check", "check", "blocked", false, exitBlocked, true},
		{"blocked deploy that shipped nothing", "deploy", "blocked", false, exitBlocked, true},
		{"blocked deploy under approvals", "deploy", "blocked", true, exitOK, true},
		{"failed", "check", "failed", false, exitError, true},
		{"unknown state", "check", "weird", false, exitError, false},
	} {
		run := goldenRun(t)
		run.Result.State = c.state
		if c.shipped {
			run.Deploy = &deployed{Env: "preview", URL: "http://claims-portal.preview.localhost:7777"}
		}
		var out, errOut strings.Builder
		a := &app{out: &out, err: &errOut, render: Render{Format: FormatHuman}, verb: command{name: c.verb}}
		code, renders := a.outcome(run)
		if code != c.code || renders != c.renders {
			t.Errorf("%s: exit %d renders=%v, want %d renders=%v", c.name, code, renders, c.code, c.renders)
		}
	}
}

// A failed run under ai is its block and nothing else (cli.md §5.2): no glyph, no colour code, no
// Package row and no build log on stdout, and nothing on stderr, whichever verb ran it.
func TestAFailedRunUnderAIPrintsItsBlockAndNoTranscript(t *testing.T) {
	failing := append(buildLines[:2:2], "\x1b[31mstep failed\x1b[0m", "ERROR: failed to build")
	for verb, reason := range map[string]string{"deploy": "build", "check": "error"} {
		dir := freshDir(t, "claims-portal")
		must(t, os.WriteFile(filepath.Join(dir, projectFile), []byte("app = \"claims-portal\"\n"), 0o600))
		h := &gateHost{project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
			start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
			page:  buildPage(t, failing, false),
			run:   `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"` + reason + `","findings":[]}}`}
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		t.Chdir(dir)
		out, errOut, code := answer(t, "", map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_FORMAT": "ai"}, false, verb)
		if code != exitError || errOut != "" || !strings.HasPrefix(out, "CANYON REVIEW — RESULT: FAILED\n") || !strings.HasSuffix(out, "\nexit code: 1\n") {
			t.Fatalf("a failed %s under ai: exit %d, stderr %q, stdout:\n%s", verb, code, errOut, out)
		}
		for _, leak := range append(utf8Glyphs[:3:3], "\x1b", `\x1b`, "│", "Package", "Review", "Build failed") {
			if strings.Contains(out, leak) {
				t.Errorf("a failed %s under ai printed %q:\n%s", verb, leak, out)
			}
		}
	}
}
