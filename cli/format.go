// Package cli is the canyon binary. This half is the rendering: what §5.1's ladder resolves out of
// the environment, and the glyphs and colour §2 draws with. It depends on nothing else here.
package cli

import (
	"fmt"
	"os"
	"strings"
)

// Format is how one invocation renders. cli.md §5.1: the builder never picks it.
type Format string

const (
	FormatHuman Format = "human"
	FormatAI    Format = "ai"
	FormatJSON  Format = "json"
	FormatSARIF Format = "sarif" // check only: the run's SARIF log, for a CI to upload
	FormatCSV   Format = "csv"   // db query only: the rows, for a spreadsheet or a script
)

// harnessVars are the variables an AI harness sets for the processes it runs, so an agent gets
// agent output without asking, and the harness each names, which the CLI sends as Canyon-Harness for
// the usage report's agents (#2953). Each was read off a running harness or its binary; the three names
// §5.1 also listed could not be, and cli.md §5.1 now says why.
var harnessVars = []struct{ name, harness string }{{"CLAUDECODE", "claude_code"}, {"CLAUDE_CODE_ENTRYPOINT", "claude_code"}, {"CODEX_SANDBOX", "codex"}}

// harnessOf is the coding agent the CLI runs under, by the first of harnessVars set, "" for none.
func harnessOf(getenv func(string) string) string {
	for _, v := range harnessVars {
		if getenv(v.name) != "" {
			return v.harness
		}
	}
	return ""
}

// Render is the format, and whether colour and the UTF-8 glyphs are safe on this stream. One
// invocation resolves it once, before anything prints.
type Render struct {
	Format Format
	Color  bool
	ASCII  bool
}

// resolveRender walks §5.1's ladder and settles colour and glyphs alongside it.
func resolveRender(flagged string, getenv func(string) string, tty bool) (Render, error) {
	r := Render{
		Format: FormatHuman,
		Color:  colourWanted(getenv, tty),
		ASCII:  getenv("CANYON_ASCII") != "" || !utf8Locale(getenv),
	}
	for _, pick := range []string{getenv("CANYON_FORMAT"), flagged} { // 1 the escape hatch, 2 the flag
		if pick == "" || pick == "auto" {
			continue
		}
		switch f := Format(pick); f {
		case FormatHuman, FormatAI, FormatJSON, FormatSARIF, FormatCSV:
			r.Format = f
		default:
			return r, fmt.Errorf("unknown format %q — auto, human, ai, json, sarif or csv", pick)
		}
		return r.plain(), nil
	}
	if harnessOf(getenv) != "" { // 3 a harness is reading
		r.Format = FormatAI
		return r.plain(), nil
	}
	// 4 a TTY and 5 a pipe both answer human — a pipe is not an agent — differing only in colour.
	return r, nil
}

// colourWanted: NO_COLOR ends it, so does FORCE_COLOR=0 (the convention CI images set), any other
// FORCE_COLOR starts it in a pipe, and otherwise colour takes a terminal.
func colourWanted(getenv func(string) string, tty bool) bool {
	force := getenv("FORCE_COLOR")
	switch {
	case getenv("NO_COLOR") != "" || force == "0":
		return false
	case force != "":
		return true
	}
	return tty
}

// plain drops colour from anything an agent or a script reads.
func (r Render) plain() Render {
	if r.Format != FormatHuman {
		r.Color = false
	}
	return r
}

// utf8Locale reads LC_ALL, LC_CTYPE then LANG, the first one set. Nothing set is no opinion and
// the glyphs stay; CANYON_ASCII=1 is the way out.
func utf8Locale(getenv func(string) string) bool {
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if l := strings.ToUpper(getenv(v)); l != "" {
			return strings.Contains(l, "UTF-8") || strings.Contains(l, "UTF8")
		}
	}
	return true
}

// IsTTY is the ladder's step 4. Nothing it cannot prove is a terminal: an unreadable descriptor and
// a null device it cannot identify both answer no, because the cost of a wrong yes is a question
// asked where nothing can answer it.
func IsTTY(f *os.File) bool {
	info, err := f.Stat()
	discard, missing := os.Stat(os.DevNull)
	if err != nil || missing != nil {
		return false
	}
	return isTerminal(info, discard)
}

// isTerminal is a character device on the other end, and not the null device — which is a character
// device that reads back nothing, and is how a harness hands a verb its stdin.
func isTerminal(info, discard os.FileInfo) bool {
	return info.Mode()&os.ModeCharDevice != 0 && !os.SameFile(info, discard)
}
