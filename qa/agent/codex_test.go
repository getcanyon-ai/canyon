package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// copyRun copies a fixture run's scenario directory into a fresh run directory.
func copyRun(t *testing.T, fixture string) (run, dir string) {
	t.Helper()
	run = t.TempDir()
	dir = filepath.Join(run, fixture)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "run", fixture, "*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return run, dir
}

func TestScoreReadsACodexRun(t *testing.T) {
	run, dir := copyRun(t, "codex")
	if err := score(run); err != nil {
		t.Fatal(err)
	}
	card, _ := os.ReadFile(filepath.Join(dir, "scorecard.md"))
	for _, want := range []string{
		"agent: codex · stack: Node · turns sent: 2 · wall: 3m15s · canyon deploy: 2 · canyon check: 1 · production: not promoted",
		"| first result | BLOCKED secrets.client-bundle | BLOCKED secrets.client-bundle.gitleaks.stripe-access-token | ok |",
		"| last result | PASSED | PASSED | ok |",
		"### 1 · turn 1 · BLOCKED",
		"### 3 · turn 2 · PASSED", // the check and the deploy in one command: two runs, two results
		"- said: Canyon blocked it: the key in public/teamstats.js reaches the browser.",
		"- Edit (update): /app/server.js",
		"- Write: /app/public/config.js",
		"- Bash: canyon check && canyon deploy --env prod --yes", // the script, not Codex's zsh -lc around it
		"canyon with --force: 0 · calls to Canyon's API around the CLI: 1",
		"- Bash: curl -s https://canyon.example/api/v1/projects/qa-codex/status",
		"- said: (Codex: stream disconnected before completion; retrying)",
		"none asked: Codex runs headless without asking",
	} {
		if !strings.Contains(string(card), want) {
			t.Errorf("scorecard lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(string(card), "unreadable transcript") || strings.Count(string(card), "### ") != 3 {
		t.Errorf("a started item was read as its own step, or a line went unread:\n%s", card)
	}
	if last, ok := unfixed(dir, agentCodex); last != "PASSED" || ok {
		t.Errorf("unfixed after the codex run: %q, %v", last, ok)
	}
	if last, _ := unfixed(dir, agentClaude); last != "none" { // the formats are not interchangeable: read as Claude's, nothing is a result
		t.Errorf("a codex transcript read as Claude's found %q", last)
	}
}

func TestACodexTurnResumesTheFirstOnesThread(t *testing.T) {
	first := codexArgs("Deploy it.", "")
	want := []string{"exec", "--json", "--skip-git-repo-check", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true", "Deploy it."}
	if !slices.Equal(first, want) {
		t.Errorf("turn 1: %q", first)
	}
	_, dir := copyRun(t, "codex")
	thread, err := codexThread(dir)
	if err != nil || thread != "01a1a573-0000-7000-8000-00000000c0de" {
		t.Fatalf("thread %q, %v", thread, err)
	}
	later := codexArgs("Ok, do what Canyon says.", thread)
	want = []string{"exec", "resume", "--json", "--skip-git-repo-check", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true", thread, "Ok, do what Canyon says."}
	if !slices.Equal(later, want) {
		t.Errorf("a later turn: %q", later)
	}
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(`{"type":"turn.started"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := codexThread(dir); err == nil { // never a fresh thread that forgot the first turn
		t.Error("a turn 1 with no thread.started gave a thread to resume")
	}
}

func TestCodexsShellIsReadThrough(t *testing.T) {
	for command, want := range map[string]string{
		`/bin/zsh -lc 'canyon deploy --env prod'`: "canyon deploy --env prod",
		`bash -c "canyon check && canyon deploy"`: "canyon check && canyon deploy",
		`/bin/sh -c 'echo "a b"'`:                 `echo "a b"`,
		`canyon deploy`:                           "canyon deploy",
		`FOO=1 /bin/zsh -lc 'canyon deploy'`:      `FOO=1 /bin/zsh -lc 'canyon deploy'`, // not the bare shell Codex runs: left as it is
		`/bin/zsh -lc 'canyon deploy' extra`:      `/bin/zsh -lc 'canyon deploy' extra`,
		`/bin/zsh -x 'canyon deploy'`:             `/bin/zsh -x 'canyon deploy'`,
		`/bin/zsh -lc 'a'; /bin/zsh -lc 'b'`:      `/bin/zsh -lc 'a'; /bin/zsh -lc 'b'`,
		`python3 -c 'print(1)'`:                   `python3 -c 'print(1)'`,
		`/bin/zsh -lc 'unterminated`:              `/bin/zsh -lc 'unterminated`,
	} {
		if got := shellScript(command); got != want {
			t.Errorf("shellScript(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestAScenarioNamesItsAgent(t *testing.T) {
	dir := t.TempDir()
	write := func(header string) (scenario, error) {
		path := filepath.Join(dir, "s.md")
		if err := os.WriteFile(path, []byte(header+"stack: Go\nexpect first: PASSED\n\n## Turns\n\n### 1\n```\nDeploy it.\n```\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return parseScenario(path)
	}
	if s, err := write(""); err != nil || s.Agent != agentClaude {
		t.Errorf("no agent line: %q, %v", s.Agent, err)
	}
	if s, err := write("agent: codex\n"); err != nil || s.Agent != agentCodex {
		t.Errorf("agent: codex: %q, %v", s.Agent, err)
	}
	if s, err := write("agent: cursor\n"); err != nil || s.Agent != agentCursor {
		t.Errorf("agent: cursor: %q, %v", s.Agent, err)
	}
	if _, err := write("agent: aider\n"); err == nil { // an agent the runner cannot drive is refused, never run as Claude
		t.Error("agent: aider parsed")
	}
}
