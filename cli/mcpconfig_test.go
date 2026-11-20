package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A person's ~/.claude.json, as Claude Code writes its own state: other keys, another server, two-space indent.
const personsClaudeJSON = `{
  "numStartups": 12,
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/"
    }
  },
  "projects": {"/work/app": {"allowedTools": []}}
}
`

// A person's ~/.codex/config.toml: comments, another server, a table after it.
const personsCodexTOML = `# my settings
model = "o3" # the default

[mcp_servers.docs]
command = "npx"
args = ["-y", "docs-mcp"]

[profiles.fast]
model = "o4-mini"
`

const (
	claudeServer = `"canyon":{"type":"stdio","command":"/usr/local/bin/canyon","args":["mcp"]}`
	codexTable   = "[mcp_servers.canyon]\ncommand = '/usr/local/bin/canyon'\nargs = ['mcp']\n"
)

// Install adds Canyon's server and nothing else: every byte of the person's file stays where it was, a second
// run writes nothing, a moved canyon is replaced where it stands, and uninstall gives back the file it found.
func TestSkillInstallMCPKeepsEveryOtherByte(t *testing.T) {
	home, _ := harnessHome(t)
	claude, codex := filepath.Join(home, ".claude.json"), filepath.Join(home, ".codex", "config.toml")
	must(t, os.WriteFile(claude, []byte(personsClaudeJSON), 0o600), os.WriteFile(codex, []byte(personsCodexTOML), 0o600))
	env := map[string]string{"HOME": home}
	for _, harness := range []string{targetClaude, targetCodex} {
		if _, errOut, code := run(t, env, false, "skill", "install", "--harness", harness); code != exitOK || errOut != "" {
			t.Fatalf("install %s: exit %d, stderr %q", harness, code, errOut)
		}
	}
	wantClaude := strings.Replace(personsClaudeJSON, "\n    }\n  },", "\n    },"+claudeServer+"\n  },", 1)
	if got := fileBytes(t, claude); got != wantClaude {
		t.Errorf("~/.claude.json after install:\n%s\nwant\n%s", got, wantClaude)
	}
	if got := fileBytes(t, codex); got != personsCodexTOML+"\n"+codexTable {
		t.Errorf("~/.codex/config.toml after install:\n%s", got)
	}
	out, _, _ := run(t, env, false, "skill", "install", "--harness", targetClaude)
	if !strings.Contains(out, "~/.claude.json                            unchanged") {
		t.Errorf("a second install:\n%s", out)
	}
	moved := Self
	Self = "/opt/canyon/bin/canyon"
	t.Cleanup(func() { Self = moved })
	run(t, env, false, "skill", "install", "--harness", targetCodex)
	if got := fileBytes(t, codex); got != personsCodexTOML+"\n"+strings.Replace(codexTable, "/usr/local/bin/", "/opt/canyon/bin/", 1) {
		t.Errorf("~/.codex/config.toml after a moved canyon installed again:\n%s", got)
	}
	for _, harness := range []string{targetClaude, targetCodex} {
		if _, errOut, code := run(t, env, false, "skill", "uninstall", "--harness", harness); code != exitOK || errOut != "" {
			t.Fatalf("uninstall %s: exit %d, stderr %q", harness, code, errOut)
		}
	}
	if got := fileBytes(t, claude); got != personsClaudeJSON {
		t.Errorf("~/.claude.json after uninstall:\n%s", got)
	}
	if got := fileBytes(t, codex); got != personsCodexTOML {
		t.Errorf("~/.codex/config.toml after uninstall:\n%s", got)
	}
}

// A server named canyon that runs something else is the person's: install and uninstall leave it, and say so.
// A file that doesn't parse is refused with nothing written, and so is a link to another file.
func TestSkillInstallMCPLeavesWhatIsntCanyons(t *testing.T) {
	home, cwd := harnessHome(t)
	env := map[string]string{"HOME": home}
	theirs := `{"mcpServers":{"canyon":{"command":"node","args":["my-canyon.js"]}}}`
	cursor := filepath.Join(cwd, ".cursor", "mcp.json")
	must(t, os.WriteFile(cursor, []byte(theirs), 0o600))
	for _, verb := range []string{"install", "uninstall"} {
		out, _, code := run(t, env, false, "skill", verb, "--harness", targetCursor)
		if code != exitOK || !strings.Contains(out, mcpByHand) || fileBytes(t, cursor) != theirs {
			t.Errorf("%s: exit %d\n%s\n%s", verb, code, out, fileBytes(t, cursor))
		}
	}
	codex := filepath.Join(home, ".codex", "config.toml")
	inline := "[mcp_servers]\ncanyon = { command = \"canyon\", args = [\"mcp\"] }\n" // install writes a table: an inline one is theirs
	must(t, os.WriteFile(codex, []byte(inline), 0o600))
	if out, _, _ := run(t, env, false, "skill", "install", "--harness", targetCodex); !strings.Contains(out, mcpByHand) || fileBytes(t, codex) != inline {
		t.Errorf("an inline canyon server:\n%s\n%s", out, fileBytes(t, codex))
	}
	for path, body := range map[string]string{codex: "model = \n", cursor: "{not json"} {
		must(t, os.WriteFile(path, []byte(body), 0o600))
	}
	for harness, path := range map[string]string{targetCodex: codex, targetCursor: cursor} {
		before := fileBytes(t, path)
		_, errOut, code := run(t, env, false, "skill", "install", "--harness", harness)
		if code != exitError || !strings.Contains(errOut, "cannot be read as") || fileBytes(t, path) != before {
			t.Errorf("%s: exit %d, stderr %q", harness, code, errOut)
		}
	}
	real := filepath.Join(t.TempDir(), "claude.json")
	must(t, os.WriteFile(real, []byte("{}\n"), 0o600), os.Symlink(real, filepath.Join(home, ".claude.json")))
	_, errOut, code := run(t, env, false, "skill", "install", "--harness", targetClaude)
	if code != exitError || !strings.Contains(errOut, "~/.claude.json is a link to another file") || fileBytes(t, real) != "{}\n" {
		t.Errorf("a linked ~/.claude.json: exit %d, stderr %q", code, errOut)
	}
}
