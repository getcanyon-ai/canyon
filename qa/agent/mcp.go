package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
		// a copy of the operator's ~/.claude.json brings their own servers, connected, into every turn: a tester has none
		var others []string
		for name := range conf.MCPServers {
			if name != "canyon" {
				others = append(others, name)
			}
		}
		if len(others) > 0 {
			slices.Sort(others)
			return fmt.Errorf("via: mcp: the tester's ~/.claude.json has MCP servers other than canyon (%s), copied with it: remove them from mcpServers", strings.Join(others, ", "))
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

// verdictStates are a structured verdict's state, the run's (app-page.schema.json, a run's state), as the RESULT the
// CLI's block names: clean is PASSED, as a check's block says it whether or not a person approved what blocked.
var verdictStates = map[string]string{
	"clean": "PASSED", "blocked": "BLOCKED", "failed": "FAILED", "running": "STILL RUNNING", "pending": "STILL RUNNING",
}

// verdictBlock reads a canyon_check or canyon_deploy result whose text is its structured verdict, {run, state,
// failure_reason?, blocking, warnings, findings}, findings in Canyon's SARIF profile: Claude Code hands the model a
// tool's structuredContent, as JSON, in place of its text, so no CANYON REVIEW block is there to parse. nil for any
// other text.
func verdictBlock(text string) *block {
	var v struct {
		Run      string `json:"run"`
		State    string `json:"state"`
		Reason   string `json:"failure_reason"`
		Findings struct {
			Runs []struct {
				Results []struct {
					RuleID    string `json:"ruleId"`
					Locations []struct {
						Physical struct {
							Artifact struct {
								URI string `json:"uri"`
							} `json:"artifactLocation"`
							Region struct {
								Line int `json:"startLine"`
							} `json:"region"`
						} `json:"physicalLocation"`
					} `json:"locations"`
					Properties struct {
						Canyon struct {
							Blocks bool `json:"blocks"`
						} `json:"canyon"`
					} `json:"properties"`
				} `json:"results"`
			} `json:"runs"`
		} `json:"findings"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &v) != nil || v.Run == "" || verdictStates[v.State] == "" {
		return nil
	}
	b := &block{Kind: "REVIEW", Result: verdictStates[v.State], Run: v.Run, Reason: v.Reason,
		Head: []string{"gate run: " + v.Run, "(read from the tool's structured verdict: no text block reached the agent)"}}
	for _, r := range v.Findings.Runs {
		for _, res := range r.Results {
			at := ""
			if len(res.Locations) > 0 {
				l := res.Locations[0].Physical
				at = fmt.Sprintf(" (%s:%d)", l.Artifact.URI, l.Region.Line)
			}
			if res.Properties.Canyon.Blocks {
				b.Blocking = append(b.Blocking, res.RuleID+at)
			} else {
				b.Advisory = append(b.Advisory, res.RuleID+at)
			}
		}
	}
	return b
}
