package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signedInTo is a tester HOME signed in to url.
func signedInTo(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	conf := filepath.Join(home, ".config", "canyon")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "config.json"), []byte(`{"host":"`+url+`","session":"s3ss"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestAnEgressScenarioDeploysRestrictsAndWaits(t *testing.T) {
	dir := t.TempDir()
	write := func(name, header, turns string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("stack: Node\n"+header+"\n## Turns\n\n"+turns), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	head := "deployed: preview\negress: preview allowlist\nexpect first: REFUSED api.openai.com\nexpect last: REACHED api.openai.com\n"
	turns := "### 1 " + onceRefused + "\n```\nIt's broken.\n```\n\n### 2\n```\nAnother way?\n```\n\n### 3 " + onceAllowed + "\n```\nWorks now?\n```\n"
	s, err := parseScenario(write("ok.md", head, turns))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Deployed || len(s.Egress) != 1 || s.Egress[0] != (egressMode{"preview", "allowlist"}) || !s.Turns[0].OnceRefused || s.Turns[1] != (userTurn{Text: "Another way?"}) || !s.Turns[2].OnceAllowed {
		t.Errorf("scenario: %+v", s)
	}
	for name, sc := range map[string][2]string{
		"undeployed-mode.md": {"egress: preview allowlist\nexpect first: PASSED\n", "### 1\n```\nHi.\n```\n"}, // the mode route refuses an env nothing runs
		"undeployed-wait.md": {"expect first: PASSED\n", "### 1 " + onceRefused + "\n```\nHi.\n```\n"},        // nothing runs to refuse
		"misspelt.md":        {head, "### 1 " + onceRefused + "\n```\nHi.\n```\n\n### 2 (once a person allowed it)\n```\n.\n```\n"},
		"allow-unrefused.md": {"deployed: preview\nexpect first: PASSED\n", "### 1\n```\nHi.\n```\n\n### 2 " + onceAllowed + "\n```\nOk?\n```\n"},
		"no-destination.md":  {strings.Replace(head, "REFUSED api.openai.com", "REFUSED", 1), turns},
		"no-refusal-wait.md": {head, "### 1\n```\nHi.\n```\n"}, // nothing reads what an outbound expect is judged against
		"bad-egress.md":      {strings.Replace(head, "preview allowlist", "preview off", 1), turns},
		"observe.md":         {strings.Replace(head, "preview allowlist", "preview observe", 1), turns}, // a widening is a person's, never the runner's
		"deployed-to-qa.md":  {strings.Replace(head, "deployed: preview", "deployed: qa", 1), turns},
	} {
		if _, err := parseScenario(write(name, sc[0], sc[1])); err == nil {
			t.Errorf("%s parsed", name)
		}
	}

	// the owner narrows preview; a refused PUT stops the scenario
	var puts []string
	answer := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		puts = append(puts, r.Method+" "+r.URL.Path+" "+string(body))
		w.WriteHeader(answer)
		if answer != http.StatusNoContent {
			_, _ = w.Write([]byte(`{"error":"floor_not_enforced"}`))
		}
	}))
	defer srv.Close()
	home := signedInTo(t, srv.URL)
	if err := restrict(home, "qa-x", s.Egress); err != nil {
		t.Fatal(err)
	}
	if strings.Join(puts, "\n") != `PUT /api/v1/projects/qa-x/egress/preview/mode {"mode":"allowlist"}` {
		t.Errorf("calls: %q", puts)
	}
	answer = http.StatusConflict
	if err := restrict(home, "qa-x", s.Egress); err == nil || !strings.Contains(err.Error(), "floor_not_enforced") {
		t.Errorf("a refused mode: %v", err)
	}
}

func TestTheSeedDeploysOnlyToAReviewThatPassed(t *testing.T) {
	for printed, ok := range map[string]bool{
		"CANYON REVIEW — RESULT: PASSED\n\napp: qa-x\ngate run: gr_1\n\nexit code: 0\n":  true,
		"CANYON REVIEW — RESULT: BLOCKED\n\napp: qa-x\ngate run: gr_1\n\nexit code: 0\n": false, // a canyon whose exit status lies
		"": false,
	} {
		home, run := t.TempDir(), t.TempDir()
		dir := filepath.Join(run, "egress")
		app := filepath.Join(dir, "qa-egress") // <run-dir>/<name>/<app>, as runOne lays it out
		for _, d := range []string{filepath.Join(home, "bin"), app} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		// what the CLI packs stays with the run: its TMPDIR is the run directory's tmp/
		script := "#!/bin/sh\n[ \"$1 $2 $3\" = 'deploy --format ai' ] || exit 2\n[ \"$TMPDIR\" = '" + filepath.Join(run, "tmp") + "' ] || exit 3\nprintf '%s' '" + printed + "'\n"
		bin := filepath.Join(home, "bin", "canyon")
		if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // test: a fake canyon the runner runs, its owner's alone
			t.Fatal(err)
		}
		err := deploySeed(t.Context(), home, dir, app, "setup.log")
		if (err == nil) != ok {
			t.Errorf("after %q: %v", printed, err)
		}
		if kept, _ := os.ReadFile(filepath.Join(dir, "setup.log")); string(kept) != printed {
			t.Errorf("setup.log: %q", kept)
		}
	}
}

// The issue's own form, go run ./qa/agent run $H runs/r1 …, names the run directory, and may name the tester HOME,
// relative to where the runner starts, while canyon and claude run in the app: their TMPDIR must still be the run
// directory's tmp/, and their HOME the tester's.
func TestRelativeRunAndHomeDirectoriesReachEveryProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost: // the runner claims the app
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete: // and takes it offline after its turns
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	home := signedInTo(t, srv.URL)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// canyon for the seed's deploy and claude for the turn, each failing unless, from the app, TMPDIR is a directory
	// and HOME the tester's, which holds the sign-in
	tmpdir := "#!/bin/sh\n[ -d \"$TMPDIR\" ] || { echo \"TMPDIR $TMPDIR is no directory here\" >&2; exit 3; }\n" +
		"[ -d \"$HOME/.config/canyon\" ] || { echo \"HOME $HOME holds no sign-in here\" >&2; exit 4; }\n"
	for name, script := range map[string]string{
		"canyon": tmpdir + "printf 'CANYON REVIEW — RESULT: PASSED\\n\\napp: qa-reldir\\ngate run: gr_1\\n\\nexit code: 0\\n'\n",
		"claude": tmpdir,
	} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test: a fake the runner runs, its owner's alone
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")) // the runner looks claude up on its own PATH: this fake, never the real one
	scenario := filepath.Join(t.TempDir(), "reldir.md")
	if err := os.WriteFile(scenario, []byte("stack: Node\ndeployed: preview\nexpect first: PASSED\n\n## Turns\n\n### 1\n```\nHi.\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	rel, err := filepath.Rel(cwd, home) // the tester HOME as $H may name it, from where the runner starts
	if err != nil {
		t.Fatal(err)
	}
	if err := runAll(rel, filepath.Join("runs", "r1"), options{}, []string{scenario}); err != nil {
		log, _ := os.ReadFile(filepath.Join("runs", "r1", "reldir", "setup.log"))
		t.Fatalf("%v\nsetup.log: %s", err, log)
	}
}

func TestTurnsWaitForCanyonsRefusalThenAPersonsAllow(t *testing.T) {
	reads := []string{
		`{"envs":{"preview":{"mode":"allowlist","allowed":[],"requests":[]},"prod":{"requests":[]}}}`,
		`{"envs":{"preview":{"mode":"allowlist","allowed":[],"requests":[{"destination":"api.openai.com","state":"pending","last_refused_at":"2026-11-07T12:00:05Z","refusals":1}]},"prod":{}}}`,
		`{"envs":{"preview":{"mode":"allowlist","allowed":[],"requests":[{"destination":"api.openai.com","state":"dismissed","last_refused_at":"2026-11-07T12:00:35Z","refusals":2}]},"prod":{}}}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/qa-x/egress" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(reads[min(n, len(reads)-1)]))
		n++
	}))
	defer srv.Close()
	home := signedInTo(t, srv.URL)
	defer func(was time.Duration) { patchEvery = was }(patchEvery)
	patchEvery = time.Millisecond

	body, err := waitRead(t.Context(), home, "qa-x", outboundPath("qa-x"), "outbound reads", "no request", time.Minute, refusedYet)
	if err != nil || string(body) != reads[1] {
		t.Fatalf("the refusal wait ended on %s: %v", body, err)
	}
	var first outboundRead
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if got := waitingFor(first); got != "api.openai.com in preview" {
		t.Errorf("waiting for %q", got)
	}

	// a Dismiss is no Allow: the wait goes on past it, and gives up naming it
	if _, err := waitRead(t.Context(), home, "qa-x", outboundPath("qa-x"), "outbound reads", "no person allowed it", 20*time.Millisecond, allowedYet(first)); err == nil || !strings.Contains(err.Error(), "api.openai.com in preview not allowed yet") {
		t.Errorf("a dismissed request: %v", err)
	}
	reads = append(reads, `{"envs":{"preview":{"mode":"allowlist","allowed":["*.openai.com"],"requests":[]},"prod":{}}}`)
	n = 2
	if body, err := waitRead(t.Context(), home, "qa-x", outboundPath("qa-x"), "outbound reads", "no person allowed it", time.Minute, allowedYet(first)); err != nil || string(body) != reads[3] {
		t.Errorf("the allow wait ended on %s: %v", body, err)
	}
}

func TestOutboundExpectsAreJudgedOnCanyonsRecords(t *testing.T) {
	const first = `{"envs":{"preview":{"mode":"allowlist","allowed":[],"requests":[{"destination":"api.openai.com","state":"pending","last_refused_at":"2026-11-07T12:00:35Z","refusals":3}],
"destinations":[{"destination":"api.openai.com","port":443,"last_seen_at":"2026-11-07T12:00:35Z","calls":4,"last_outcome":"refused","last_status_class":"4xx"}]},"prod":{}}}`
	reached := `{"envs":{"preview":{"mode":"allowlist","allowed":["api.openai.com"],"requests":[],
"destinations":[{"destination":"api.openai.com","port":443,"last_seen_at":"2026-11-07T12:09:05Z","calls":9,"last_outcome":"allowed","last_status_class":"2xx"}]},"prod":{}}}`
	stale := strings.Replace(reached, "12:09:05Z", "11:59:05Z", 1) // the call from before preview was restricted
	// production, promoted and never restricted, reaches OpenAI while preview, where it was refused, still is
	around := strings.Replace(first, `"prod":{}`, `"prod":{"mode":"observe","allowed":[],"requests":[],
"destinations":[{"destination":"api.openai.com","port":443,"last_seen_at":"2026-11-07T12:09:05Z","calls":3,"last_outcome":"allowed","last_status_class":"2xx"}]}`, 1)
	status := "CANYON STATUS\n\napp: qa-egress\noutbound: preview · only allowed destinations · 1 destination refused since 3f2a went live\n" +
		"refused: api.openai.com · not allowed 3 times · last 2026-11-07T12:00:35Z\n" +
		"outcome: Canyon refused these calls with HTTP 403. They never reached the destination.\n" +
		"do:\n  1. Tell the user which destinations were refused and why the app needs each one.\n" +
		"do not:\n  - unset or change HTTPS_PROXY, HTTP_PROXY or NO_PROXY\n\nproduction checklist:\n  passed: x\n"
	turn := func(id, command, out string) string {
		use, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"command": command}}}}})
		res, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": out}}}})
		return string(use) + "\n" + string(res) + "\n"
	}
	for last, want := range map[string]string{
		reached: "| last result | REACHED api.openai.com | REACHED api.openai.com (preview: allowed 2xx, last 2026-11-07T12:09:05Z) | ok |",
		stale:   "| last result | REACHED api.openai.com | NOT REACHED api.openai.com (preview: allowed 2xx, last 2026-11-07T11:59:05Z) | MISS |",
		around:  "| last result | REACHED api.openai.com | NOT REACHED api.openai.com (prod: allowed 2xx, last 2026-11-07T12:09:05Z, where the run never saw it refused) | MISS |",
	} {
		run := t.TempDir()
		dir := filepath.Join(run, "egress")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"scenario.md": "stack: Node\ndeployed: preview\negress: preview allowlist\nexpect first: REFUSED api.openai.com\nexpect last: REACHED api.openai.com\n\n## Turns\n\n### 1 " + onceRefused + "\n```\nIt's broken.\n```\n",
			"turn-1.jsonl": turn("t1", "canyon status", status) + turn("t2", "canyon status", status) + // the same refusals read twice are one result
				turn("t3", "canyon status", strings.Replace(status, "not allowed 3 times", "not allowed 5 times", 1)),
			outboundFirst: first,
			outboundLast:  last,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := score(run); err != nil {
			t.Fatal(err)
		}
		card, _ := os.ReadFile(filepath.Join(dir, "scorecard.md"))
		for _, line := range []string{
			want,
			"| first result | REFUSED api.openai.com | REFUSED api.openai.com (preview: pending, 3 refusals, last 2026-11-07T12:00:35Z) | ok |",
			"- preview · allowlist · allowed: none · request api.openai.com pending, 3 refusals, last 2026-11-07T12:00:35Z · row api.openai.com:443 refused 4xx, 4 calls, last 2026-11-07T12:00:35Z",
			"### 1 · turn 1 · REFUSED\n\n```\noutbound: preview · only allowed destinations · 1 destination refused since 3f2a went live\nrefused: api.openai.com · not allowed 3 times",
			"  - unset or change HTTPS_PROXY, HTTP_PROXY or NO_PROXY\n",
			"### 2 · turn 1 · REFUSED", // the count moved: a second result
		} {
			if !strings.Contains(string(card), line) {
				t.Errorf("scorecard lacks %q:\n%s", line, card)
			}
		}
		if strings.Contains(string(card), "### 3 ·") || strings.Contains(string(card), "production checklist") {
			t.Errorf("a refusal read again counted twice, or status's checklist read as a step:\n%s", card)
		}
	}
}
