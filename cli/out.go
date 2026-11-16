package cli

// state is what a row is. cli.md §2: a glyph *and* a colour, never colour alone — a pipe, a CI log
// and a colourblind reader all lose the colour and must lose no information.
type state int

const (
	statePass state = iota
	stateFail
	stateWarn
	statePending
)

var (
	utf8Glyphs  = [...]string{"✓", "✗", "!", "·"}
	asciiGlyphs = [...]string{"+", "x", "!", "-"}
)

// Raw ANSI; the rest of §2's colours arrive with the verb that needs them.
const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"  // section headers
	ansiDim   = "\x1b[2m"  // timings, paths, secondary hints
	ansiRed   = "\x1b[31m" // blockers only: if the terminal is red, something stops you shipping
	ansiGreen = "\x1b[32m" // a check that passed
	ansiAmber = "\x1b[33m" // a warning, and the line that says it did not block
	ansiCyan  = "\x1b[36m" // a URL: the one thing on the screen to click

	ansiUnderline = "\x1b[4m"
)

// The three drawing methods hang off Render rather than off the invocation, because what they
// answer depends on nothing else: the ladder decided, and these read the decision.
func (r Render) glyph(s state) string {
	if r.ASCII {
		return asciiGlyphs[s]
	}
	return utf8Glyphs[s]
}

// punct is the header lines' punctuation: §2's fallback is about information, not only glyphs.
func (r Render) punct(utf8, ascii string) string {
	if r.ASCII {
		return ascii
	}
	return utf8
}

// paint wraps s when colour is on and answers s untouched when it is not.
func (r Render) paint(colour, s string) string {
	if !r.Color || colour == "" {
		return s
	}
	return colour + s + ansiReset
}
