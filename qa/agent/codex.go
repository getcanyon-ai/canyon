package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"
)

// The agents a scenario may name on its agent: line. Claude Code reads the skill canyon skill install
// put in the tester's ~/.claude; Codex the skill and session hook the runner installs in the tester's
// ~/.agents and ~/.codex before a Codex scenario's first turn.
const (
	agentClaude = "claude-code"
	agentCodex  = "codex"
)

// codexConfig is the sandbox every Codex turn runs in: the app's directory writable and the network
// on, which canyon deploy needs; the rest of the tester's HOME stays read-only. codex exec never asks
// for approval, so a command the sandbox refuses fails in place.
var codexConfig = []string{"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true"}

// codexArgs is one user turn as codex exec: the first starts a thread, every later one resumes it by
// its id, which the first turn's thread.started event carries. Codex runs only hooks someone trusted:
// Canyon's, which installCodex trusted, and never one the agent writes into the app as it works.
func codexArgs(text, thread string) []string {
	args := []string{"exec"}
	if thread != "" {
		args = append(args, "resume")
	}
	args = append(append(args, "--json", "--skip-git-repo-check"), codexConfig...)
	if thread != "" {
		args = append(args, thread)
	}
	return append(args, text)
}

// codexThread is the thread turn 1 started, from its thread.started event.
func codexThread(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "turn-1.jsonl"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 16<<20), 16<<20)
	for sc.Scan() {
		var e codexEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type == "thread.started" && e.ThreadID != "" {
			return e.ThreadID, nil
		}
	}
	return "", errors.New("turn-1.jsonl has no thread.started event: nothing to resume")
}

// installCodex writes Canyon's skill and session hook for Codex into the tester's HOME and trusts the
// hook, as a Codex user does once: canyon skill install --harness codex, then trusting it when Codex asks
// (trustCodexHook). It needs the tester's ~/.codex, where Codex's sign-in is.
func installCodex(home, app string) error {
	cmd := exec.Command(filepath.Join(home, "bin", "canyon"), "skill", "install", "--harness", agentCodex)
	cmd.Dir, cmd.Env = app, testerEnv(home, app)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("canyon skill install --harness codex: %w: %s", err, out)
	}
	return trustCodexHook(home, app)
}

// turnWall is the turn's wall time as the runner measured it (turn-<n>.wall), since Codex's events
// carry none and no reader reads Cursor's yet; zero when the file is missing.
func turnWall(dir string, n int) time.Duration {
	raw, err := os.ReadFile(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".wall"))
	if err != nil {
		return 0
	}
	d, _ := time.ParseDuration(strings.TrimSpace(string(raw)))
	return d
}

// codexEvent is one line of codex exec --json; only the fields the scorer reads.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"` // an error event's
	Error    struct {
		Message string `json:"message"`
	} `json:"error"` // a turn.failed event's
	Item struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		Changes          []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
		// an mcp_tool_call's: the server and tool, the arguments the model passed, and what the tool answered
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
		Result    *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"item"`
}

// codexLine is one line of codex exec --json. Only completed items count: a started one says again
// what its completion says. A shell command reads as a Bash call of the command Codex's shell ran.
func (r *reader) codexLine(n int, line []byte) error {
	var e codexEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return err
	}
	switch e.Type {
	case "turn.failed":
		r.t.Steps = append(r.t.Steps, step{Turn: n, Said: "(Codex: the turn failed: " + e.Error.Message + ")"})
	case "error":
		r.t.Steps = append(r.t.Steps, step{Turn: n, Said: "(Codex: " + e.Message + ")"})
	case "item.completed":
		it := e.Item
		switch it.Type {
		case "agent_message":
			r.t.Steps = append(r.t.Steps, step{Turn: n, Said: it.Text})
		case "command_execution":
			input, _ := json.Marshal(map[string]string{"command": shellScript(it.Command)}) // a map of strings, so the encoder cannot fail // a map of strings, so the encoder cannot fail
			r.use(n, it.ID, "Bash", input)
			r.result(n, it.ID, it.AggregatedOutput)
		case "mcp_tool_call": // named as Claude Code names an MCP tool, so one reader judges both agents' calls
			r.use(n, it.ID, "mcp__"+it.Server+"__"+it.Tool, it.Arguments)
			var text strings.Builder
			if it.Result != nil {
				for _, c := range it.Result.Content {
					text.WriteString(c.Text + "\n")
				}
			}
			if it.Error != nil {
				text.WriteString(it.Error.Message + "\n")
			}
			r.result(n, it.ID, text.String())
		case "file_change":
			for _, c := range it.Changes { // named as Claude's tools are: a new file is a Write, any other change an Edit
				tool := "Write: " + c.Path
				if c.Kind != "add" {
					tool = "Edit (" + c.Kind + "): " + c.Path
				}
				r.t.Steps = append(r.t.Steps, step{Turn: n, Tool: tool})
			}
		}
	}
	return nil
}

// shellScript is the script a `/bin/zsh -lc '<script>'` runs, as Codex reports every command; any
// other command is itself.
func shellScript(command string) string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return command
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 3 || len(call.Assigns) > 0 {
		return command
	}
	switch filepath.Base(word(call.Args[0])) {
	case "sh", "bash", "zsh":
	default:
		return command
	}
	if flag := word(call.Args[1]); flag != "-c" && flag != "-lc" {
		return command
	}
	return word(call.Args[2])
}
