package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// What the runner found of a playbook: scenario's native skill once the turns were over, kept beside the transcripts
// for the scorer, which reads only the run directory.
const (
	playbookSkillFile = "playbook-skill"
	skillSynced       = "synced"
)

// mcpConfigured fails a via: mcp scenario before turn 1 when skill install wrote no canyon MCP server for its agent: a
// run without the server would score the agent for not using tools it was never given (design/specs/mcp.md §1.4).
func mcpConfigured(home, agent string) error {
	switch agent {
	case agentClaude:
		raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
		if err != nil {
			return fmt.Errorf("via: mcp: the tester's ~/.claude.json: %w", err)
		}
		var conf struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &conf); err != nil {
			return fmt.Errorf("via: mcp: the tester's ~/.claude.json does not read: %w", err)
		}
		if c, ok := conf.MCPServers["canyon"]; !ok || c.Command == "" || strings.Join(c.Args, " ") != "mcp" {
			return fmt.Errorf("via: mcp: ~/.claude.json has no mcpServers.canyon running canyon mcp: run canyon skill install in the tester HOME with this release's CLI")
		}
	case agentCodex:
		raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		if err != nil {
			return fmt.Errorf("via: mcp: the tester's ~/.codex/config.toml: %w", err)
		}
		var conf struct {
			MCPServers map[string]struct {
				Command string   `toml:"command"`
				Args    []string `toml:"args"`
			} `toml:"mcp_servers"`
		}
		if err := toml.Unmarshal(raw, &conf); err != nil {
			return fmt.Errorf("via: mcp: the tester's ~/.codex/config.toml does not read: %w", err)
		}
		if c, ok := conf.MCPServers["canyon"]; !ok || c.Command == "" || strings.Join(c.Args, " ") != "mcp" {
			return fmt.Errorf("via: mcp: ~/.codex/config.toml has no [mcp_servers.canyon] running canyon mcp: canyon skill install --harness codex writes it")
		}
	default:
		return fmt.Errorf("via: mcp: no MCP config is checked for %s", agent)
	}
	return nil
}

// playbookExists fails a playbook: scenario before turn 1 when the org has no such playbook: an admin writes one, in
// the dashboard, and the runner never can (design/specs/mcp.md §3.2).
func playbookExists(home, slug string) error {
	s, err := signedIn(home)
	if err != nil {
		return err
	}
	a, err := s.call("GET", "/api/v1/playbooks/"+url.PathEscape(slug), nil)
	if err != nil {
		return fmt.Errorf("playbook: %s: %w", slug, err)
	}
	if a.status != 200 {
		return fmt.Errorf("playbook: the org has no playbook %s (%s): an admin adds it in Guardrails › Playbooks, and points the scenario's custom rule at it, before the batch", slug, a)
	}
	return nil
}

// recordSkill keeps what the session hook left of the playbook once the turns are over: synced when the agent's
// skills hold canyon-<slug>/SKILL.md beside the .canyon marker the hook writes, else what is missing.
func recordSkill(home, dir, agent, slug string) error {
	skills := filepath.Join(home, ".claude", "skills")
	if agent == agentCodex {
		skills = filepath.Join(home, ".agents", "skills")
	}
	at := filepath.Join(skills, "canyon-"+slug)
	got := skillSynced
	for _, f := range []string{"SKILL.md", ".canyon"} {
		if _, err := os.Stat(filepath.Join(at, f)); err != nil {
			rel, _ := filepath.Rel(home, filepath.Join(at, f))
			got = "missing: ~/" + filepath.ToSlash(rel)
			break
		}
	}
	return os.WriteFile(filepath.Join(dir, playbookSkillFile), []byte(got+"\n"), 0o600)
}
