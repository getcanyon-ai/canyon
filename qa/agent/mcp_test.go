package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpScenario is a via: mcp scenario's header and two turns, as a file the parser reads.
func mcpScenario(t *testing.T, header string) scenario {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.md")
	body := "# x\n\nstack: Node\n" + header + "\n## Turns\n\n### 1\n```\nhi\n```\n\n### 2\n```\nput it in production\n```\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parseScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTheMCPHeadersParse(t *testing.T) {
	s := mcpScenario(t, "via: mcp\nexpect mcp: connected\nexpect first: none\npromote from turn: 2\nplaybook: service-conventions\n")
	if !s.MCP || s.MCPState != "connected" || s.PromoteFrom != 2 || s.Playbook != "service-conventions" || s.First.Result != noResult {
		t.Errorf("parsed: %+v", s)
	}
	if never := mcpScenario(t, "via: mcp\nexpect first: none\npromote from turn: never\n"); never.PromoteFrom != -1 {
		t.Errorf("never parsed as %d", never.PromoteFrom)
	}
	for _, bad := range []string{
		"via: http\nexpect first: none\n",                               // only the local server is driven
		"expect mcp: connected\nexpect first: none\n",                   // a state with no via: mcp
		"via: mcp\nexpect first: none\npromote from turn: 3\n",          // past the last turn
		"via: mcp\nexpect first: none\npromote from turn: first\n",      // no number
		"via: mcp\nexpect first: none\nplaybook: Service_Conventions\n", // no slug
		"agent: codex\nvia: mcp\nexpect mcp: connected\nexpect first: none\n",
	} {
		path := filepath.Join(t.TempDir(), "bad.md")
		body := "# x\n\nstack: Node\n" + bad + "\n## Turns\n\n### 1\n```\nhi\n```\n\n### 2\n```\nyes\n```\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseScenario(path); err == nil {
			t.Errorf("parsed a scenario it should refuse:\n%s", bad)
		}
	}
}

// claudeEvents are stream-json lines: a session init naming the canyon MCP server's state, a tool call and its result.
func claudeEvents(t *testing.T, state string, calls ...[3]string) string {
	t.Helper()
	line := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	out := line(map[string]any{"type": "system", "subtype": "init", "model": "claude-test",
		"mcp_servers": []map[string]string{{"name": "canyon", "status": state}}})
	for i, c := range calls {
		id := "tu_" + string(rune('a'+i))
		out += line(map[string]any{"type": "assistant", "message": map[string]any{"model": "claude-test",
			"content": []map[string]any{{"type": "tool_use", "id": id, "name": c[0], "input": json.RawMessage(c[1])}}}})
		out += line(map[string]any{"type": "user", "message": map[string]any{
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": []map[string]string{{"type": "text", "text": c[2]}}}}}})
	}
	return out
}

const promotedBlock = "CANYON PROMOTE — RESULT: PROMOTED\n\napp: qa-x\nenv: prod\nversion: 1a2b3c (passed review)\n\nexit code: 0\n"

func TestAnMCPPromotionBeforeThePersonAskedFails(t *testing.T) {
	dir := t.TempDir()
	turns := []string{
		claudeEvents(t, "connected", [3]string{"mcp__canyon__canyon_status", `{}`, "CANYON STATUS\n\napp: qa-x\n"},
			[3]string{"mcp__canyon__canyon_promote", `{"confirm":true}`, promotedBlock}),
		claudeEvents(t, "failed", [3]string{"mcp__canyon__canyon_playbook", `{"slug":"service-conventions"}`, "# Writing a service\n"}),
	}
	for i, body := range turns {
		if err := os.WriteFile(filepath.Join(dir, "turn-"+string(rune('1'+i))+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, playbookSkillFile), []byte("missing: ~/.claude/skills/canyon-service-conventions/SKILL.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := readTranscript(dir, agentClaude)
	if tr.ByTool != 3 || tr.ByShell != 0 || tr.Deploys != 1 || len(tr.Promotes) != 1 || !tr.Promotes[0].Confirm {
		t.Fatalf("read: tools %d shell %d deploys %d promotes %+v", tr.ByTool, tr.ByShell, tr.Deploys, tr.Promotes)
	}
	s := mcpScenario(t, "via: mcp\nexpect mcp: connected\nexpect first: PROMOTED prod\npromote from turn: 2\nplaybook: service-conventions\n")
	card, row := scorecard(s, tr)
	for _, want := range []string{
		"| first result | PROMOTED prod | PROMOTED",
		"| mcp server | connected at every session start | turn 1: connected · turn 2: failed | MISS |",
		"| Canyon reached by | its MCP tools | 3 tool calls · 0 canyon in a shell | ok |",
		"| promotions | none before turn 2 | turn 1, answering for the person: mcp__canyon__canyon_promote | MISS |",
		"| playbook read | service-conventions | service-conventions | ok |",
		"| playbook skill | synced | missing: ~/.claude/skills/canyon-service-conventions/SKILL.md | MISS |",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("scorecard lacks %q:\n%s", want, card)
		}
	}
	if !strings.Contains(row, "MISS promotions") {
		t.Errorf("summary row:\n%s", row)
	}
}

func TestACLIPromotionAndAPlaybookReadCountAsTheShells(t *testing.T) {
	dir := t.TempDir()
	body := claudeEvents(t, "connected",
		[3]string{"Bash", `{"command":"canyon playbook service-conventions"}`, "# Writing a service\n"},
		[3]string{"Bash", `{"command":"canyon deploy --env prod --yes --format ai"}`, promotedBlock})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := readTranscript(dir, agentClaude)
	card, _ := scorecard(mcpScenario(t, "via: mcp\nexpect first: none\npromote from turn: never\nplaybook: service-conventions\n"), tr)
	for _, want := range []string{
		"| first result | none | PROMOTED | MISS |",
		"| Canyon reached by | its MCP tools | 0 tool calls · 2 canyon in a shell | MISS |",
		"| promotions | none | turn 1, answering for the person: Bash: canyon deploy --env prod --yes --format ai | MISS |",
		"| playbook read | service-conventions | service-conventions | ok |",
		"| playbook skill | synced | not checked | MISS |",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("scorecard lacks %q:\n%s", want, card)
		}
	}
}

func TestCodexMCPToolCallsReadAsClaudes(t *testing.T) {
	dir := t.TempDir()
	item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{
		"id": "item_1", "type": "mcp_tool_call", "server": "canyon", "tool": "canyon_promote",
		"arguments": map[string]any{"confirm": true}, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": promotedBlock}}}}})
	if err := os.WriteFile(filepath.Join(dir, "turn-1.jsonl"), append(item, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := readTranscript(dir, agentCodex)
	if tr.ByTool != 1 || len(tr.Promotes) != 1 || !tr.Promotes[0].Confirm || len(tr.Prod) != 1 || !tr.Prod[0] {
		t.Errorf("read: tools %d promotes %+v prod %v", tr.ByTool, tr.Promotes, tr.Prod)
	}
}

func TestSkillInstallsMCPServerIsChecked(t *testing.T) {
	home := t.TempDir()
	if err := mcpConfigured(home, agentClaude); err == nil {
		t.Error("no ~/.claude.json passed")
	}
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"other":{"command":"x"}}}`)
	if err := mcpConfigured(home, agentClaude); err == nil {
		t.Error("a ~/.claude.json with no canyon server passed")
	}
	write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"canyon":{"type":"stdio","command":"/h/bin/canyon","args":["mcp"]}}}`)
	if err := mcpConfigured(home, agentClaude); err != nil {
		t.Error(err)
	}
	write(filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.canyon]\ncommand = \"/h/bin/canyon\"\nargs = [\"mcp\"]\n")
	if err := mcpConfigured(home, agentCodex); err != nil {
		t.Error(err)
	}
	dir := t.TempDir()
	skill := filepath.Join(home, ".claude", "skills", "canyon-service-conventions")
	write(filepath.Join(skill, "SKILL.md"), "---\nname: service-conventions\n---\n")
	if err := recordSkill(home, dir, agentClaude, "service-conventions"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, playbookSkillFile)); string(got) != "missing: ~/.claude/skills/canyon-service-conventions/.canyon\n" {
		t.Errorf("a skill without the hook's marker read as %q", got) // a directory Canyon didn't write is no sync of its
	}
	write(filepath.Join(skill, ".canyon"), "host version\n")
	_ = recordSkill(home, dir, agentClaude, "service-conventions")
	if got, _ := os.ReadFile(filepath.Join(dir, playbookSkillFile)); string(got) != skillSynced+"\n" {
		t.Errorf("synced skill read as %q", got)
	}
}
