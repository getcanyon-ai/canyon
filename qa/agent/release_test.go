package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deployed: prod releases the seed to production, then ## Next release over it; expect prod: first release is judged
// on what production runs once the turns are over, and needs the releases it names (#2160).
func TestAReleasedScenarioParsesItsNextRelease(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	turns := "## Turns\n\n### 1\n```\nPut it back.\n```\n"
	seedOf := func(section string) string {
		return "## " + section + "\n\nFile: `server.js`\n```js\n// " + section + "\n```\n\n"
	}
	s, err := parseScenario(write("ok.md", "stack: Node\ndeployed: prod\nexpect first: PROMOTED\nexpect prod: first release\n\n"+seedOf("Seed")+seedOf("Next release")+turns))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Deployed || !s.Released || s.Prod != "first release" || len(s.Seed) != 1 || len(s.Next) != 1 || s.Next[0] != (seedFile{"server.js", "// Next release\n"}) {
		t.Errorf("scenario: %+v", s)
	}
	for name, body := range map[string]string{
		"next-unreleased.md":  "stack: Node\ndeployed: preview\nexpect first: PASSED\n\n" + seedOf("Next release") + turns,    // nothing releases it
		"first-unreleased.md": "stack: Node\ndeployed: preview\nexpect first: PASSED\nexpect prod: first release\n\n" + turns, // no release recorded
		"bad-prod.md":         "stack: Node\ndeployed: prod\nexpect first: PASSED\nexpect prod: v1\n\n" + turns,
		"no-next-release.md":  "stack: Node\ndeployed: prod\nexpect first: PASSED\nexpect prod: first release\n\n" + seedOf("Seed") + turns, // nothing to tell the first release from
	} {
		if _, err := parseScenario(write(name, body)); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
}

// Which release production ran once the turns were over, from the package and digest the runner kept of each: both
// must match, since a rebuild of the first release's code can carry its digest (pack's fixed timestamps) under a new
// package (#2784 review). Fail closed: a file missing or out of shape, or a read that failed, is never a release.
func TestProdReleaseNamesWhatProductionRuns(t *testing.T) {
	ran := func(pkg, digest string) string { return `{"package":"` + pkg + `","digest":"` + digest + `"}` }
	releases := `{"first":` + ran("pkg_v1", "sha256:aa") + `,"last":` + ran("pkg_v2", "sha256:bb") + `}`
	for _, c := range []struct{ name, released, prodLast, want string }{
		{"put back", releases, ran("pkg_v1", "sha256:aa"), "first release"},
		{"left broken", releases, ran("pkg_v2", "sha256:bb"), "last release"},
		{"a rebuild of the first release's code, its digest the same", releases, ran("pkg_v3", "sha256:aa"), "another version"},
		{"the first release's package, another image", releases, ran("pkg_v1", "sha256:cc"), "another version"},
		{"a third version", releases, ran("pkg_v3", "sha256:cc"), "another version"},
		{"undeployed", releases, ran("", ""), "nothing in production"},
		{"a read that failed", releases, `{"unread":"the status read: 503"}`, "not read (" + prodLastFile + ")"},
		{"no last read", releases, "", "not read (" + prodLastFile + ")"},
		{"no releases", "", ran("pkg_v1", "sha256:aa"), "not read (" + releasedFile + ")"},
		{"releases out of shape", `{"first":{"package":"","digest":""}}`, ran("", ""), "not read (" + releasedFile + ")"},
		{"releases of the old shape", `{"first":"sha256:aa","last":"sha256:bb"}`, ran("pkg_v1", "sha256:aa"), "not read (" + releasedFile + ")"},
	} {
		dir := t.TempDir()
		for file, body := range map[string]string{releasedFile: c.released, prodLastFile: c.prodLast} {
			if body == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := prodRelease(dir); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The scorecard's production row under expect prod: first release is the release production runs at the end, never
// whether a turn promoted: a rollback to another version promotes too.
// Setup fails as setup, never as a score: a release the status read names no package or image of, and a next release
// that left production on the first's package or image, which no turn could then be told apart from (#2784 review).
func TestReleasesThatCannotBeToldApartFailTheSetup(t *testing.T) {
	v1, v2 := ran{Package: "pkg_v1", Digest: "sha256:aa"}, ran{Package: "pkg_v2", Digest: "sha256:bb"}
	for _, c := range []struct {
		name string
		r    released
		ok   bool
	}{
		{"two releases", released{First: v1, Last: v2}, true},
		{"no image for the first", released{First: ran{Package: "pkg_v1"}, Last: v2}, false},
		{"no package for the first", released{First: ran{Digest: "sha256:aa"}, Last: v2}, false},
		{"no image for the last", released{First: v1, Last: ran{Package: "pkg_v2"}}, false},
		{"the last on the first's image", released{First: v1, Last: ran{Package: "pkg_v2", Digest: "sha256:aa"}}, false},
		{"the last on the first's package", released{First: v1, Last: ran{Package: "pkg_v1", Digest: "sha256:bb"}}, false},
	} {
		if err := c.r.check(); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestTheScorecardJudgesWhichReleaseProductionRuns(t *testing.T) {
	line := func(v any) string { b, _ := json.Marshal(v); return string(b) + "\n" }
	turn := line(map[string]any{"type": "system", "subtype": "init", "model": "claude-opus-5-5"}) +
		line(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-opus-5-5", "content": []map[string]any{
			{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]string{"command": "canyon deploy --env prod --version v1"}}}}}) +
		line(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1",
			"content": "CANYON PROMOTE — RESULT: PROMOTED\n\napp: qa-x\nenv: prod\n\nexit code: 0"}}}})
	scenario := "stack: Node\ndeployed: prod\nexpect first: PROMOTED\nexpect prod: first release\n\n## Next release\n\nFile: `server.js`\n```js\n// v2\n```\n\n## Turns\n\n### 1\n```\nPut it back.\n```\n"
	run := t.TempDir()
	for name, prod := range map[string]string{"back": "sha256:aa", "other": "sha256:cc"} {
		dir := filepath.Join(run, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"scenario.md": scenario, "turn-1.jsonl": turn,
			releasedFile: `{"first":{"package":"pkg_v1","digest":"sha256:aa"},"last":{"package":"pkg_v2","digest":"sha256:bb"}}`, prodLastFile: `{"package":"pkg_v1","digest":"` + prod + `"}`} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := score(run); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"back": "| production | first release | first release | ok |", "other": "| production | first release | another version | MISS |"} {
		card, err := os.ReadFile(filepath.Join(run, name, "scorecard.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(card), want+"\n") {
			t.Errorf("%s's scorecard lacks %q:\n%s", name, want, card)
		}
	}
}

// promoteSeed fails the setup, never a score, when the status read after a PROMOTED names no package or no image in
// production: a release the turns can't be told apart from (#2784 review).
func TestPromoteSeedNeedsAPackageAndAnImage(t *testing.T) {
	for _, c := range []struct {
		name, prod string
		ok         bool
	}{
		{"both named", `{"package":"pkg_v1","image_digest":"sha256:aa"}`, true},
		{"no package", `{"package":"","image_digest":"sha256:aa"}`, false},
		{"no image", `{"package":"pkg_v1","image_digest":""}`, false},
		{"nothing in production", `null`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/projects/qa-x/status" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"envs":{"prod":` + c.prod + `}}`))
		}))
		home, run := signedInTo(t, srv.URL), t.TempDir()
		dir := filepath.Join(run, "x")
		app := filepath.Join(dir, "qa-x")
		for _, d := range []string{filepath.Join(home, "bin"), app} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		script := "#!/bin/sh\n[ \"$1 $2 $3 $4 $5 $6\" = 'deploy --env prod --yes --format ai' ] || exit 2\n" +
			"printf 'CANYON PROMOTE — RESULT: PROMOTED\\n\\napp: qa-x\\nenv: prod\\n\\nexit code: 0\\n'\n"
		bin := filepath.Join(home, "bin", "canyon")
		if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // test: a fake canyon the runner runs, its owner's alone
			t.Fatal(err)
		}
		got, err := promoteSeed(t.Context(), home, dir, app, 1)
		if (err == nil) != c.ok || (c.ok && got != (ran{Package: "pkg_v1", Digest: "sha256:aa"})) {
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
		srv.Close()
	}
}
