package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// agentPages are the docs site's Get started pages, where each coding agent's setup shows what
// canyon skill install printed for it.
var agentPages = filepath.Join("..", "docs", "src", "content", "docs", "start")

// TestTheAgentPagesShowWhatSkillInstallPrints holds every skill install a page shows to the goldens:
// each of its lines is one some skill-install golden holds, so a change to what install prints
// fails here until the pages show it too. The one line that differs by machine, the path Claude
// Code's hook runs, is compared as the goldens write it.
func TestTheAgentPagesShowWhatSkillInstallPrints(t *testing.T) {
	var golden []string
	files, _ := filepath.Glob(filepath.Join("testdata", "skill-install*.txt"))
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // a golden this test names
		if err != nil {
			t.Fatal(err)
		}
		golden = append(golden, strings.Split(string(b), "\n")...)
	}
	shown := 0
	err := filepath.WalkDir(agentPages, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // a docs page under the directory this test names
		if err != nil {
			return err
		}
		for _, block := range textBlocks(string(b)) {
			if !slices.ContainsFunc(block.lines, func(l string) bool { return strings.Contains(l, "Looking for coding agents") }) {
				continue
			}
			shown++
			for i, line := range block.lines {
				if line != "" && !slices.Contains(golden, hookPath(line)) {
					t.Errorf("%s:%d shows a line canyon skill install does not print (cli/testdata/skill-install*.txt):\n%s", path, block.start+i, line)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if shown == 0 {
		t.Fatalf("no page under %s shows canyon skill install's output", agentPages)
	}
}

// textBlock is one ```text fence of a page: its lines without the fence's own indent, and the page
// line the first one is on.
type textBlock struct {
	start int
	lines []string
}

// textBlocks are a page's ```text fences, as a list item indents them or not.
func textBlocks(page string) []textBlock {
	var blocks []textBlock
	var open *textBlock
	indent := ""
	for i, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case open == nil && trimmed == "```text":
			indent = line[:len(line)-len(strings.TrimLeft(line, " "))]
			open = &textBlock{start: i + 2}
		case open != nil && trimmed == "```":
			blocks = append(blocks, *open)
			open = nil
		case open != nil:
			open.lines = append(open.lines, strings.TrimPrefix(line, indent))
		}
	}
	return blocks
}

// hookPath writes the path in Claude Code's or Codex's hook line as the goldens do: install puts the
// path this canyon was run from there, which is the reader's own. Claude Code's is the whole command,
// before its args; Codex's comes before manifest in its one shell command. Cursor's group has no
// matcher and names canyon bare, the same on every machine, so it is compared as it is.
func hookPath(line string) string {
	before, rest, ok := strings.Cut(line, `"command":"`)
	if !ok || !strings.Contains(line, `"matcher"`) {
		return line
	}
	for _, end := range []string{`","args"`, ` manifest --harness `} {
		if _, after, ok := strings.Cut(rest, end); ok {
			return before + `"command":"/usr/local/bin/canyon` + end + after
		}
	}
	return line
}
