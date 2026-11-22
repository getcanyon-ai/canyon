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

// Every Claude turn loads Canyon's server alone: the HOME's canyon entry, under --strict-mcp-config, so the account's
// connectors and a synced plugin's servers never reach the agent; a HOME with another server, or a turn whose session
// loaded one anyway, stops the scenario.
func TestEveryClaudeTurnLoadsCanyonAlone(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude.json"), `{"theme":"dark","mcpServers":{"canyon":{"type":"stdio","command":"/h/bin/canyon","args":["mcp"]}}}`)
	// claude: print the MCP config it was given, then a session init naming the servers it loaded
	fakeBin(t, filepath.Join(t.TempDir(), "bin"), map[string]string{"claude": "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = --strict-mcp-config ] && strict=1; [ \"$1\" = --mcp-config ] && cfg=$2; shift; done\n" +
		"[ \"$strict\" = 1 ] || exit 7\n" +
		"printf '{\"type\":\"system\",\"subtype\":\"init\",\"mcp_servers\":[{\"name\":\"canyon\",\"status\":\"connected\"}%s]}\\n' \"$EXTRA\"\n" +
		"cp \"$cfg\" ../given.json\n"})
	app := filepath.Join(dir, "qa-x")
	if err := os.MkdirAll(app, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runTmp(filepath.Dir(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := turn(t.Context(), agentClaude, home, dir, app, 1, "hi", false); err != nil {
		t.Fatalf("a turn with canyon alone: %v", err)
	}
	given, _ := os.ReadFile(filepath.Join(filepath.Dir(app), "given.json"))
	if string(given) != `{"mcpServers":{"canyon":{"type":"stdio","command":"/h/bin/canyon","args":["mcp"]}}}` {
		t.Errorf("the MCP config the turn was given: %s", given)
	}
	write(filepath.Join(dir, "turn-1.jsonl"), `{"type":"system","subtype":"init","mcp_servers":[{"name":"canyon","status":"connected"},{"name":"claude.ai Gmail","status":"needs-auth"}]}`+"\n")
	if got := othersLoaded(filepath.Join(dir, "turn-1.jsonl")); len(got) != 1 || got[0] != "claude.ai Gmail" {
		t.Errorf("the servers other than canyon a session loaded: %v", got)
	}
	write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"canyon":{"command":"canyon"},"playwright":{"command":"npx"}}}`)
	if _, err := strictMCP(home, dir); err == nil || !strings.Contains(err.Error(), "playwright") {
		t.Errorf("a HOME with another server: %v", err)
	}
}

// One repository and folder link to one app alone, and every linked scenario shares {{REPO}}: each scenario's code goes
// in a folder named after its app, and its link names that folder.
func TestALinkedScenarioLinksAFolderOfItsOwn(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/projects/qa-x/repository" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"repository":{"state":"linked"}}`))
	}))
	defer srv.Close()
	if err := link(signedInTo(t, srv.URL), "qa-x", "org/repo", "qa-x"); err != nil {
		t.Fatal(err)
	}
	if got["full_name"] != "org/repo" || got["folder"] != "qa-x" {
		t.Errorf("the link sent %v", got)
	}
	seed := []seedFile{{Path: "server.js", Body: "x"}, {Path: "public/index.html", Body: "y"}}
	moved := inFolder(seed, "qa-x")
	if moved[0].Path != "qa-x/server.js" || moved[1].Path != "qa-x/public/index.html" || seed[0].Path != "server.js" {
		t.Errorf("moved %v, from %v", moved, seed)
	}
}

// A run handed off after 45 seconds and then read with canyon_review scores by its verdict: its STILL RUNNING is no
// result of its own, and the later block of the same run is not dropped as a repeat.
func TestAHandedOffRunScoresByItsVerdict(t *testing.T) {
	dir := t.TempDir()
	running := "CANYON REVIEW — RESULT: STILL RUNNING\n\napp: qa-x\ngate run: gr_1\n\nexit code: 0\n"
	passed := "CANYON REVIEW — RESULT: PASSED\n\napp: qa-x\ngate run: gr_1\n\nexit code: 0\n"
	turn := claudeEvents(t, "connected",
		[3]string{"mcp__canyon__canyon_deploy_ref", `{"app":"qa-x","ref":"b"}`, running},
		[3]string{"mcp__canyon__canyon_review", `{"app":"qa-x","run":"gr_1"}`, running},
		[3]string{"mcp__canyon__canyon_review", `{"app":"qa-x","run":"gr_1"}`, passed})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(turn), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "h.md")
	if err := os.WriteFile(path, []byte("stack: Node\nexpect first: PASSED\nexpect last: PASSED\n\n## Turns\n\n### 1\n```\nhi\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parseScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	card, _ := scorecard(s, readTranscript(dir, agentClaude))
	for _, want := range []string{"| first result | PASSED | PASSED | ok |", "| last result | PASSED | PASSED | ok |"} {
		if !strings.Contains(card, want) {
			t.Errorf("the scorecard has no %q:\n%s", want, card)
		}
	}
}

// A turn that waits for the person to set a secret waits first for the agent's deploy to end, failed included: the
// status read names the last run by its id, and the run's own read says its state.
func TestTheSecretsTurnWaitsForTheRunToEnd(t *testing.T) {
	state := "running"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/qa-x/status":
			_, _ = w.Write([]byte(`{"envs":{},"last_run":"gr_1","project":"qa-x"}`))
		case "/api/v1/gate-runs/gr_1":
			_, _ = w.Write([]byte(`{"run":{"id":"gr_1","state":"` + state + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	s, err := signedIn(signedInTo(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ state, want string }{{"running", "running"}, {"pending", "pending"}, {"failed", "failed"}, {"clean", "clean"}} {
		state = c.state
		if got := runState(s, "qa-x"); got != c.want {
			t.Errorf("a run %s reads %q", c.state, got)
		}
	}
	state = "failed"
	if err := waitRunEnded(t.Context(), signedInTo(t, srv.URL), "qa-x", time.Second); err != nil {
		t.Errorf("a failed run is an ended one: %v", err)
	}
}

// An expect said: line passes on a Canyon tool's answer holding those words, and on nothing else: not the agent's own
// words, and not another tool's output.
func TestExpectSaidReadsCanyonsAnswers(t *testing.T) {
	dir := t.TempDir()
	turn := claudeEvents(t, "connected",
		[3]string{"Bash", `{"command":"echo Nothing was promoted: nobody answered the question."}`, "Nothing was promoted: nobody answered the question."},
		[3]string{"mcp__canyon__canyon_promote", `{"confirm":true}`, "Nothing was promoted: nobody answered the question.\nTo promote it, a person opens https://c.example/#/apps/qa-x."})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(turn), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "p.md")
	body := "stack: Node\nexpect first: none\nexpect said: Nothing was promoted: nobody answered the question.\nexpect said: To promote it, a person opens\n" +
		"expect said: in a terminal where they can answer\n\n## Turns\n\n### 1\n```\nhi\n```\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parseScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	card, _ := scorecard(s, readTranscript(dir, agentClaude))
	for _, want := range []string{
		"| Canyon said | Nothing was promoted: nobody answered the question. | said | ok |",
		"| Canyon said | To promote it, a person opens | said | ok |",
		"| Canyon said | in a terminal where they can answer | not in any Canyon answer | MISS |",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the scorecard has no %q:\n%s", want, card)
		}
	}
	dir2 := t.TempDir() // the same words from a shell alone are no answer of Canyon's
	if err := os.WriteFile(filepath.Join(dir2, "turn-1.jsonl"), []byte(claudeEvents(t, "connected",
		[3]string{"Bash", `{"command":"echo x"}`, "Nothing was promoted: nobody answered the question."})), 0o600); err != nil {
		t.Fatal(err)
	}
	if card, _ := scorecard(s, readTranscript(dir2, agentClaude)); !strings.Contains(card, "| Canyon said | Nothing was promoted: nobody answered the question. | not in any Canyon answer | MISS |") {
		t.Errorf("a shell's output counted as Canyon's answer:\n%s", card)
	}
}

// expect first: any judges nothing: the scenario rests on its other lines.
func TestExpectAnyResultJudgesNothing(t *testing.T) {
	any := expect{Result: anyResult}
	if !met(any, nil) || !met(any, &block{Kind: "REVIEW", Result: "PASSED"}) || met(expect{Result: noResult}, &block{Kind: "REVIEW", Result: "PASSED"}) {
		t.Error("expect first: any judged a result")
	}
}
