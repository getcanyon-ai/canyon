package cli

import (
	"os"
	"strings"
	"testing"
)

func TestTheFormatLadder(t *testing.T) {
	for _, c := range []struct {
		name, flagged      string
		env                map[string]string
		tty, colour, ascii bool
		want               Format
	}{
		{name: "CANYON_FORMAT wins over the flag", flagged: "human", env: map[string]string{"CANYON_FORMAT": "json"}, tty: true, want: FormatJSON},
		{name: "the flag wins over a harness", flagged: "human", env: map[string]string{"CLAUDECODE": "1"}, want: FormatHuman},
		{name: "a harness wins over a terminal", env: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, tty: true, want: FormatAI},
		{name: "so does a sandboxing harness", env: map[string]string{"CODEX_SANDBOX": "seatbelt"}, tty: true, want: FormatAI},
		{name: "a terminal is human, in colour", tty: true, colour: true, want: FormatHuman},
		{name: "a pipe is human without colour", want: FormatHuman},
		{name: "NO_COLOR ends the colour", env: map[string]string{"NO_COLOR": "1"}, tty: true, want: FormatHuman},
		{name: "FORCE_COLOR starts it", env: map[string]string{"FORCE_COLOR": "1"}, colour: true, want: FormatHuman},
		{name: "FORCE_COLOR=0 ends it, even on a terminal", env: map[string]string{"FORCE_COLOR": "0"}, tty: true, want: FormatHuman},
		{name: "CANYON_ASCII drops the glyphs", env: map[string]string{"CANYON_ASCII": "1"}, tty: true, colour: true, ascii: true, want: FormatHuman},
		{name: "so does a locale that is not UTF-8", env: map[string]string{"LC_ALL": "C"}, ascii: true, want: FormatHuman},
		{name: "a UTF-8 locale keeps them", env: map[string]string{"LANG": "en_GB.UTF-8"}, want: FormatHuman},
		{name: "NO_COLOR beats FORCE_COLOR", env: map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, tty: true, want: FormatHuman},
		{name: "LC_ALL beats LC_CTYPE", env: map[string]string{"LC_ALL": "en_US.UTF-8", "LC_CTYPE": "C"}, want: FormatHuman},
		{name: "LC_CTYPE beats LANG", env: map[string]string{"LC_CTYPE": "C", "LANG": "en_US.UTF-8"}, ascii: true, want: FormatHuman},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveRender(c.flagged, func(k string) string { return c.env[k] }, c.tty)
			if err != nil {
				t.Fatal(err)
			}
			if got != (Render{Format: c.want, Color: c.colour, ASCII: c.ascii}) {
				t.Errorf("resolved %+v, want format %s colour=%v ascii=%v", got, c.want, c.colour, c.ascii)
			}
		})
	}
	if _, err := resolveRender("yaml", func(string) string { return "" }, false); err == nil {
		t.Error("a format nobody renders is an error, not a default")
	}
	f, err := os.Create(t.TempDir() + "/not-a-tty") // step 4's mechanism
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if IsTTY(f) {
		t.Error("a regular file read as a terminal")
	}
}

// What the ladder decided has to reach the bytes, or the whole ladder is decoration: colour on a
// terminal, nothing on a pipe, and the glyphs told apart either way.
func TestWhatTheLadderDecidedReachesTheOutput(t *testing.T) {
	terminal, err := resolveRender("", func(string) string { return "" }, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := terminal.paint(ansiBold, "Canyon"); !strings.Contains(got, "\x1b[") {
		t.Errorf("a terminal's rendering painted no escape byte: %q", got)
	}
	pipe, err := resolveRender("", func(string) string { return "" }, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, colour := range []string{ansiBold, ansiDim, ansiReset} {
		if got := pipe.paint(colour, "Canyon"); got != "Canyon" {
			t.Errorf("a pipe's rendering painted %q", got)
		}
	}
	// §2: every state is told apart without colour, and a terminal that cannot show the glyphs
	// still loses no information.
	utf8, ascii := Render{}, Render{ASCII: true}
	seen, seenASCII := map[string]bool{}, map[string]bool{}
	for _, s := range []state{statePass, stateFail, stateWarn, statePending} {
		glyph, fallback := utf8.glyph(s), ascii.glyph(s)
		if glyph == "" || fallback == "" || seen[glyph] || seenASCII[fallback] {
			t.Errorf("state %d has no distinct glyph: %q / %q", s, glyph, fallback)
		}
		for i := 0; i < len(fallback); i++ {
			if fallback[i] < 0x20 || fallback[i] > 0x7e {
				t.Errorf("state %d's ASCII fallback %q is not printable ASCII", s, fallback)
			}
		}
		seenASCII[fallback] = true
		seen[glyph] = true
	}
	// The punctuation follows the same rule, so an ASCII terminal gets printable bytes throughout.
	if utf8.punct("·", "|") != "·" || ascii.punct("·", "|") != "|" {
		t.Error("punct must follow the glyph set it was resolved with")
	}
}
