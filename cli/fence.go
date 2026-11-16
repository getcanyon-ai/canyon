package cli

import "strings"

// fenceState tracks whether a line sits inside a fenced code block. The rule is CommonMark's core
// one and no more: a fence opens on a run of three or more backticks or tildes at the start of a
// line, and closes on a run of the same character at least as long with nothing else on the line.
// The marker finder needs it for one thing — a marker inside an example is an example — and the
// test reads the spec's own fence with it, so the two cannot disagree about where a fence ends.
type fenceState struct {
	char byte
	open int
}

// inside reports whether the line is part of a fenced code block, its own delimiter lines included.
func (f *fenceState) inside(line string) bool {
	char, n := fenceRun(strings.TrimRight(strings.TrimSuffix(line, "\n"), " \t"))
	switch {
	case f.open == 0:
		if n >= 3 {
			f.char, f.open = char, n
			return true
		}
	case char == f.char && n >= f.open:
		f.char, f.open = 0, 0
		return true
	}
	return f.open > 0
}

// fenceRun is the leading run of backticks or tildes on a line, and nothing follows it on a closing
// fence — so it also reports how long the run is, which is what tells a closer from a longer opener.
func fenceRun(line string) (byte, int) {
	line = strings.TrimLeft(line, " \t")
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	return line[0], n
}
