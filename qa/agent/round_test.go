package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBin writes executable scripts into dir and puts it first on the runner's PATH.
func fakeBin(t *testing.T, dir string, scripts map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, script := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test: a fake the runner runs, its owner's alone
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Every scenario runs at once by default, each under a HOME of its own copied from the tester's: three scenarios whose
// agent waits until all three have started finish only if none waits for a slot, and each turn saw its own HOME.
func TestEveryScenarioRunsAtOnceUnderAHomeOfItsOwn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects" { // the runner claims each app
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	template := signedInTo(t, srv.URL)
	if err := os.MkdirAll(filepath.Join(template, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "bin", "canyon"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test: a fake canyon
		t.Fatal(err)
	}
	// claude: note its HOME beside the app, then wait up to 10 s for all three turns to have started
	fakeBin(t, filepath.Join(t.TempDir(), "bin"), map[string]string{"claude": "#!/bin/sh\necho \"$HOME\" > ../home-seen\n" +
		"mkdir -p \"$TMPDIR/started\" && touch \"$TMPDIR/started/$$\"\n" +
		"i=0; while [ \"$(ls \"$TMPDIR/started\" | wc -l)\" -lt 3 ]; do i=$((i+1)); [ $i -gt 100 ] && exit 9; sleep 0.1; done\n"})
	var files []string
	for _, name := range []string{"a", "b", "c"} {
		path := filepath.Join(t.TempDir(), name+".md")
		if err := os.WriteFile(path, []byte("stack: Node\nexpect first: none\n\n## Turns\n\n### 1\n```\nHi.\n```\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
	}
	run := filepath.Join(t.TempDir(), "r1")
	if err := runAll(template, run, options{keepApps: true}, files); err != nil {
		t.Fatalf("three scenarios at once: %v", err)
	}
	homes := map[string]bool{}
	for _, name := range []string{"a", "b", "c"} {
		seen, err := os.ReadFile(filepath.Join(run, name, "home-seen"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(seen)); got != filepath.Join(run, name, "home") {
			t.Errorf("%s's turn ran under HOME %s, not its own", name, got)
		}
		homes[strings.TrimSpace(string(seen))] = true
		if _, err := os.Stat(filepath.Join(run, name, "home", ".config", "canyon", "config.json")); err != nil {
			t.Errorf("%s's HOME is no copy of the tester's: %v", name, err)
		}
	}
	if len(homes) != 3 {
		t.Errorf("three scenarios shared HOMEs: %v", homes)
	}
	report, err := os.ReadFile(filepath.Join(run, "REPORT.md"))
	if err != nil || !strings.Contains(string(report), "3 scenarios, 3 at once · 3 passed") || !strings.Contains(string(report), "| qa-a | claude-code · CLI | pass |") ||
		!strings.Contains(string(report), "required checks by hand not passed: 1") ||
		!strings.Contains(string(report), "| Claude Code: its own /mcp sign-in (required) | not recorded |") {
		t.Errorf("REPORT.md: %s (%v)", report, err)
	}
}

// hostedTemplate is a hosted template HOME with both grants kept for host, ending at until, and the operator's own
// MCP server in its .claude.json.
func hostedTemplate(t *testing.T, host string, until time.Time) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, tokenDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"read", "act"} {
		body := "tok-" + scope + "\n" + until.UTC().Format(time.RFC3339) + "\n" + host + "\nCanyon QA runner (hosted " + scope + ")\n"
		if err := os.WriteFile(filepath.Join(dir, tokenDir, "token-"+scope), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"mcpServers":{"playwright":{"command":"npx"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A hosted scenario's agent has Canyon by URL alone: its one MCP server is the URL with its scope's token, the other
// scope's grant is not in its HOME, and a canyon anywhere it can reach stops the scenario before turn 1.
func TestAHostedAgentHasCanyonByURLAndNoCLI(t *testing.T) {
	fakeBin(t, filepath.Join(t.TempDir(), "bin"), map[string]string{"claude": "#!/bin/sh\n"})
	template := hostedTemplate(t, "https://canyon.example", time.Now().Add(time.Hour))
	dir := t.TempDir()
	home, g, err := hostedHome(template, dir, "read")
	if err != nil {
		t.Fatal(err)
	}
	if g.token != "tok-read" || g.host != "https://canyon.example" {
		t.Errorf("grant: %+v", g)
	}
	var conf struct {
		Onboarded  bool `json:"hasCompletedOnboarding"`
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Command string            `json:"command"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	c, ok := conf.MCPServers["canyon"]
	if !conf.Onboarded || len(conf.MCPServers) != 1 || !ok || c.Type != "http" || c.URL != "https://canyon.example/mcp" || c.Command != "" ||
		c.Headers["Authorization"] != "Bearer tok-read" {
		t.Errorf(".claude.json: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(home, tokenDir)); !os.IsNotExist(err) {
		t.Errorf("the kept grants were copied into the agent's HOME: %v", err)
	}
	env := hostedEnv(home, filepath.Join(dir, "qa-x"))
	if err := noCLI(home, env); err != nil {
		t.Errorf("a HOME with no canyon: %v", err)
	}
	if !strings.HasPrefix(env[0], "PATH="+filepath.Join(home, ".local", "bin")+":") || strings.Contains(env[0], os.Getenv("HOME")+"/.local/bin") {
		t.Errorf("the agent's PATH: %s", env[0])
	}
	if err := os.MkdirAll(filepath.Join(home, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "tools", "canyon"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test: a fake canyon
		t.Fatal(err)
	}
	if err := noCLI(home, env); err == nil || !strings.Contains(err.Error(), "tools/canyon") {
		t.Errorf("a canyon in the agent's HOME: %v", err)
	}
	if err := noCLI(t.TempDir(), []string{"PATH=" + filepath.Join(home, "tools")}); err == nil || !strings.Contains(err.Error(), "on the agent's PATH") {
		t.Errorf("a canyon on the agent's PATH: %v", err)
	}
	if _, _, err := hostedHome(hostedTemplate(t, "https://canyon.example", time.Now().Add(10*time.Minute)), t.TempDir(), "act"); err == nil ||
		!strings.Contains(err.Error(), "run agent consent again") {
		t.Errorf("a grant ending within the round: %v", err)
	}
}

func TestTheHostedLinesParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.md")
	body := "stack: Node\nvia: mcp-remote\nscope: act\nlinked repo: {{REPO}}\nhome: revoke\nexpect tool: canyon_check_ref once\nexpect tool: canyon_deploy_ref\n" +
		"expect first: none\nsecret: preview WEATHER_API_KEY\n\n## Turns\n\n### 1\n```\nDeploy it.\n```\n\n### 2 (once a person disconnects the agent)\n```\nAgain.\n```\n\n" +
		"### 3 (once the person sets the secrets)\n```\nDone.\n```\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parseScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Remote || !s.MCP || s.Scope != "act" || s.Repo != repoPlaceholder || s.Home != "revoke" || len(s.Tools) != 2 ||
		s.Tools[0] != (expectTool{"canyon_check_ref", true}) || s.Tools[1] != (expectTool{"canyon_deploy_ref", false}) ||
		!s.Turns[1].OnceDisconnected || !s.Turns[2].OnceSecrets {
		t.Errorf("parsed: %+v", s)
	}
	if err := os.WriteFile(path, []byte("stack: Node\nexpect first: WAITING FOR APPROVAL\nexpect last: STILL RUNNING\n\n## Turns\n\n### 1\n```\nhi\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := parseScenario(path); err != nil || s.First.Result != "WAITING FOR APPROVAL" || len(s.First.Keys) != 0 || s.Last.Result != "STILL RUNNING" {
		t.Errorf("a RESULT of more than one word: %+v, %v", s.First, err)
	}
	for _, bad := range []string{
		"via: mcp-remote\nexpect first: none\n",                                            // no scope
		"via: mcp\nscope: act\nexpect first: none\n",                                       // a scope with no URL
		"via: mcp-remote\nscope: write\nexpect first: none\n",                              // no such scope
		"linked repo: {{REPO}}\nexpect first: none\n",                                      // a repo with no URL
		"via: mcp-remote\nscope: act\napp: agent\ndeployed: preview\nexpect first: none\n", // the runner deploys an app the agent makes later
		"via: mcp-remote\nscope: act\nlinked repo: not a repo\nexpect first: none\n",       // no owner/name
		"via: mcp-remote\nscope: act\nexpect tool: deploy\nexpect first: none\n",           // not a canyon_ tool
		"agent: codex\nvia: mcp-remote\nscope: act\nexpect first: none\n",                  // only Claude Code's config is written
	} {
		bad := bad
		p := filepath.Join(t.TempDir(), "bad.md")
		if err := os.WriteFile(p, []byte("stack: Node\n"+bad+"\n## Turns\n\n### 1\n```\nhi\n```\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseScenario(p); err == nil {
			t.Errorf("parsed a scenario it should refuse:\n%s", bad)
		}
	}
	p := filepath.Join(t.TempDir(), "shared.md") // a disconnect on the shared grant would end every scenario's
	if err := os.WriteFile(p, []byte("stack: Node\nvia: mcp-remote\nscope: act\nexpect first: none\n\n## Turns\n\n### 1\n```\nhi\n```\n\n### 2 (once a person disconnects the agent)\n```\nhi\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseScenario(p); err == nil || !strings.Contains(err.Error(), "home: of its own") {
		t.Errorf("a disconnect on the shared grant: %v", err)
	}
}

// A hosted scenario scores like a local one: the remote tools are Canyon reached by its tools, an expect tool: line is
// judged on their calls, and a made-up secret's value in any turn fails it.
func TestHostedToolsScoreLikeLocalOnes(t *testing.T) {
	dir := t.TempDir()
	blocked := "CANYON REVIEW — RESULT: BLOCKED\n\napp: qa-x\ngate run: gr_1\n\nexit code: 1\n"
	passed := "CANYON REVIEW — RESULT: PASSED\n\napp: qa-x\ngate run: gr_2\n\nexit code: 0\n"
	turn1 := claudeEvents(t, "connected",
		[3]string{"mcp__canyon__canyon_check_ref", `{"app":"qa-x","ref":"qa/qa-x"}`, blocked},
		[3]string{"mcp__canyon__canyon_deploy_ref", `{"app":"qa-x","ref":"qa/qa-x"}`, passed},
		[3]string{"mcp__canyon__canyon_status", `{"app":"qa-x"}`, "WEATHER_API_KEY is sk-proj-" + strings.Repeat("ab", 20)})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(turn1), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "h.md")
	if err := os.WriteFile(path, []byte("stack: Node\nvia: mcp-remote\nscope: act\nexpect tool: canyon_check_ref once\nexpect tool: canyon_create_app\n"+
		"secret: preview WEATHER_API_KEY\nexpect first: BLOCKED\nexpect last: PASSED\n\n## Turns\n\n### 1\n```\nhi\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parseScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := readTranscript(dir, agentClaude)
	if tr.Checks != 1 || tr.Deploys != 1 || tr.ByTool != 3 || tr.ByShell != 0 {
		t.Errorf("checks %d, deploys %d, by tool %d, by shell %d", tr.Checks, tr.Deploys, tr.ByTool, tr.ByShell)
	}
	card, _ := scorecard(s, tr)
	for _, want := range []string{
		"| Canyon reached by | its tools at Canyon's URL | 3 tool calls · 0 canyon in a shell | ok |",
		"| tool | canyon_check_ref once | canyon_check_ref: 1 calls | ok |",
		"| tool | canyon_create_app | canyon_create_app: 0 calls | MISS |",
		"| secret's value | never in a transcript | in a transcript | MISS |",
		"| first result | BLOCKED |",
		"| last result | PASSED |",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the scorecard has no %q:\n%s", want, card)
		}
	}
}

// An app the agent made with canyon_create_app is read from its turn, so it goes offline with the run.
func TestTheAppsTheAgentMadeAreRead(t *testing.T) {
	dir := t.TempDir()
	made := "Made lunch-poll (lunch-poll-x7k2), linked to org/lunch. Its page: https://canyon.example/#/apps/lunch-poll-x7k2\nNext: canyon_deploy_ref with app lunch-poll-x7k2 and the branch to deploy."
	turn := claudeEvents(t, "connected", [3]string{"mcp__canyon__canyon_create_app", `{"repository":"org/lunch"}`, made})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(turn), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := agentApps(dir); len(got) != 1 || got[0] != "lunch-poll-x7k2" {
		t.Errorf("the apps the agent made: %v", got)
	}
}

// The by-hand checks a person records land in REPORT.md, and the required one counts until it passes.
func TestTheRequiredHandCheckCountsUntilItPasses(t *testing.T) {
	run := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(run, "hand-checks.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("fail | Claude Code: its own /mcp sign-in | the consent page never came back\npass | MCP Inspector | read and act\n")
	rows, missing := handChecks(run)
	if missing != 1 || !strings.Contains(rows, "| Claude Code: its own /mcp sign-in (required) | fail the consent page never came back |") ||
		!strings.Contains(rows, "| MCP Inspector | pass read and act |") {
		t.Errorf("missing %d:\n%s", missing, rows)
	}
	write("pass | Claude Code: its own /mcp sign-in | refreshed after its token was expired\n")
	if _, missing := handChecks(run); missing != 0 {
		t.Errorf("a passed required check still counts: %d", missing)
	}
}

// The grant's session is ended through SQL by its digest alone: the disconnect route is a browser's. Only the
// sessions of this grant's client are named.
func TestRevokeSQLNamesOnlyThisGrantsSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"sessions":[{"id":"ses_0123456789abcdef","flow":"oauth","client":"Canyon QA runner (revoke act)"},
			{"id":"ses_fedcba9876543210","flow":"oauth","client":"Canyon QA runner (hosted act)"},
			{"id":"ses_1111111111111111","flow":"device"},
			{"id":"ses_x'); DROP TABLE sessions; --","flow":"oauth","client":"Canyon QA runner (revoke act)"}]}`))
	}))
	defer srv.Close()
	home, dir := signedInTo(t, srv.URL), t.TempDir()
	path, err := revokeSQL(home, dir, grant{client: "Canyon QA runner (revoke act)"})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := os.ReadFile(path)
	if !strings.Contains(string(sql), "IN ('ses_0123456789abcdef');") || strings.Contains(string(sql), "fedcba") || strings.Contains(string(sql), "DROP") {
		t.Errorf("revoke.sql: %s", sql)
	}
	if _, err := revokeSQL(home, dir, grant{client: "Canyon QA runner (gone act)"}); err == nil || !strings.Contains(err.Error(), "agent consent again") {
		t.Errorf("no session of the grant's client: %v", err)
	}
}

// The clone pushes over SSH with the deploy key alone, GitHub's host key pinned: the agent's pushes go the same way.
func TestTheCloneReachesTheRepositoryWithTheDeployKeyAlone(t *testing.T) {
	app := t.TempDir()
	if err := git(app, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := git(app, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"); err != nil {
		t.Fatal(err)
	}
	if err := origin(app, "org/repo", "qa/qa-x", "/keys"); err != nil {
		t.Fatal(err)
	}
	read := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", app}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := read("remote", "get-url", "origin"); got != "git@github.com:org/repo.git" {
		t.Errorf("origin: %s", got)
	}
	if got := read("config", "core.sshCommand"); got != "ssh -i '/keys/id_ed25519' -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile='/keys/known_hosts'" {
		t.Errorf("core.sshCommand: %s", got)
	}
	if got := read("branch", "--show-current"); got != "qa/qa-x" {
		t.Errorf("branch: %s", got)
	}
}
