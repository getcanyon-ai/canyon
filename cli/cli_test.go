package cli

import (
	"flag"
	"io"
	"sort"
	"strings"
	"testing"
)

// run is one invocation, stdout and stderr apart: which stream a line lands on is under test.
func run(t *testing.T, env map[string]string, tty bool, args ...string) (out, err string, code int) {
	t.Helper()
	return answer(t, "", env, tty, args...) // nothing to read: a machine no prompt can ask
}

func answer(t *testing.T, input string, env map[string]string, tty bool, args ...string) (out, err string, code int) {
	t.Helper()
	var o, e strings.Builder
	code = Main(args, &o, &e, strings.NewReader(input), func(k string) string { return env[k] }, tty)
	return o.String(), e.String(), code
}

// helpVerbs reads the verb rows back out of the help the way a reader does.
func helpVerbs(help string) string {
	var verbs []string
	group := false
	for _, line := range strings.Split(help, "\n") {
		switch {
		case line == "SHIP", line == "THIS APP", line == "YOUR ORG", line == "SETUP":
			group = true
		case line == "":
			group = false
		case group && strings.HasPrefix(line, "  "):
			verbs = append(verbs, strings.Fields(line)[0])
		}
	}
	sort.Strings(verbs)
	return strings.Join(verbs, ",")
}

// The tagline and the four groups, in the dashboard's words (#1403): what you ship, this app, your
// org, and setting up.
func TestTheHelpLeadsWithTheTaglineAndFourGroups(t *testing.T) {
	out, _, _ := run(t, nil, false, "--help")
	if !strings.HasPrefix(out, "\nCanyon: review, ship and govern the apps your teams build with AI.\n") {
		t.Errorf("the help does not lead with the tagline:\n%s", out)
	}
	at := -1
	for _, group := range []string{"\nSHIP\n", "\nTHIS APP\n", "\nYOUR ORG\n", "\nSETUP\n"} {
		i := strings.Index(out, group)
		if i <= at {
			t.Errorf("group %q missing or out of order:\n%s", strings.TrimSpace(group), out)
		}
		at = i
	}
	if strings.Contains(out, "GOVERN") || strings.Contains(out, "vibe-coded") {
		t.Errorf("the help still carries the old words:\n%s", out)
	}
}

// §8.3 is the help, byte for byte: every description and flag line a person reads.
func TestTheHelpIsSection83(t *testing.T) {
	out, _, _ := run(t, nil, false, "--help")
	if spec := strings.TrimPrefix(specFence(t, "### 8.3 `canyon --help`"), "$ canyon --help\n"); out != spec {
		t.Errorf("canyon --help printed\n%s\n§8.3 of design/specs/cli.md is\n%s", out, spec)
	}
}

// A flag with no usage is accepted and never offered: --explain is refused by name, -f is a habit.
func TestHiddenFlagsAreNotInTheVerbsHelp(t *testing.T) {
	for verb, hidden := range map[string]string{"check": "--explain", "logs": "--f "} {
		out, _, _ := run(t, nil, false, verb, "--help")
		if !strings.Contains(out, "FLAGS") || strings.Contains(out, hidden) {
			t.Errorf("canyon %s --help offers %q:\n%s", verb, hidden, out)
		}
	}
}

func TestTheHelpIsExactlyTheVerbsItCanRun(t *testing.T) {
	out, _, code := run(t, nil, false)
	if code != exitOK || !strings.Contains(out, "USAGE") {
		t.Errorf("canyon alone exited %d and printed:\n%s", code, out)
	}
	// One list read from both ends: the binary may not hide a verb it runs, nor offer one it cannot.
	want := make([]string, 0, len(commands))
	for _, c := range commands {
		want = append(want, c.name)
	}
	sort.Strings(want)
	if got := helpVerbs(out); got != strings.Join(want, ",") {
		t.Errorf("the help lists %q; the dispatch slice has %q", got, strings.Join(want, ","))
	}
	if out, err, code := run(t, nil, false, "whoami"); code != exitError || out != "" || !strings.Contains(err, `Unknown command "whoami"`) {
		t.Errorf("an unknown verb exited %d, stdout %q, stderr:\n%s", code, out, err)
	}
}

// Settled before anything prints, or the help and every error render with a zero value nobody chose.
func TestWhatIsPrintedBeforeAnyVerbIsStillRendered(t *testing.T) {
	if out, _, _ := run(t, nil, true); !strings.Contains(out, "\x1b[") {
		t.Errorf("no escape byte in the help on a terminal:\n%q", out)
	}
	if out, _, _ := run(t, map[string]string{"NO_COLOR": "1"}, true); strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR left an escape byte:\n%q", out)
	}
	// Under CANYON_ASCII every byte of the shipped output is printable ASCII, punctuation included.
	for _, args := range [][]string{{}, {"version"}} {
		out, _, _ := run(t, map[string]string{"CANYON_ASCII": "1"}, false, args...)
		for _, r := range out {
			if r > '~' || (r < ' ' && r != '\n') {
				t.Errorf("canyon %v under CANYON_ASCII printed %q in:\n%s", args, r, out)
				break
			}
		}
	}
}

func TestTheHostPrecedenceRefusesAHostItCannotUse(t *testing.T) {
	for _, c := range []struct{ flagged, env, saved, want string }{
		{want: defaultHost},
		{saved: "http://saved:7777", want: "http://saved:7777"},
		{env: "https://canyon.example.com", want: "https://canyon.example.com"},
		{saved: "http://saved:7777", env: "https://canyon.example.com", want: "https://canyon.example.com"},
		{flagged: "http://box:7777", env: "https://canyon.example.com", saved: "http://saved:7777", want: "http://box:7777"},
		{flagged: "http://box:7777/", want: "http://box:7777"}, // an address bar's trailing slash
		{flagged: "HTTP://Box:7777", want: "http://box:7777"},  // one host, however it was typed
		{saved: "HTTP://Box:7777/", want: "http://box:7777"},   // and the same for the one on disk
	} {
		got, err := resolveHost(c.flagged, func(string) string { return c.env }, &config{Host: c.saved})
		if err != nil || got != c.want {
			t.Errorf("resolveHost(%q, %q, %q) = %q, %v; want %q", c.flagged, c.env, c.saved, got, err, c.want)
		}
	}
	// Userinfo would be printed and sent as basic auth; an empty host quietly dials this machine.
	for _, bad := range []string{"localhost:7777", "ftp://box", "http://box:7777/api", "http://ada:hunter2@box", "http://:7777", "http://box?", "http://box#frag"} {
		if _, err := resolveHost(bad, func(string) string { return "" }, &config{}); err == nil {
			t.Errorf("resolveHost accepted %q", bad)
		}
	}
	// The value is quoted in every refusal: unquoted, escape bytes in it rewrite the terminal.
	_, err, code := run(t, map[string]string{"CANYON_HOST": "http://box/\x1b[2J\x1b[H spoofed"}, false, "version")
	if code != exitError || strings.ContainsRune(err, '\x1b') {
		t.Errorf("a host carrying escape bytes exited %d and printed them raw:\n%q", code, err)
	}
}

// Before anyone signs in the host is the default, a local demo nobody set up: version says so and how to
// sign in, never "connected to" a localhost (walkthrough 2.1).
func TestVersionBeforeLoginSaysItIsNotSignedIn(t *testing.T) {
	out, _, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "version")
	if code != exitOK || strings.Contains(out, "connected to") || !strings.Contains(out, "not signed in: it would reach "+defaultHost) || !strings.Contains(out, "canyon login") {
		t.Errorf("canyon version before login: exit %d\n%s", code, out)
	}
}

// The verb, its two formats, and the globals on either side of it: a script handed prose where it
// asked for JSON has no signal that it did not get what it asked for.
func TestVersionAndTheGlobalFlagsOnEitherSideOfIt(t *testing.T) {
	out, _, code := run(t, map[string]string{"CANYON_HOST": "https://canyon.example.com"}, false, "version")
	if code != exitOK || !strings.Contains(out, "Canyon CLI ") || !strings.Contains(out, "connected to https://canyon.example.com") {
		t.Errorf("canyon version printed %q, exit %d", out, code)
	}
	if _, _, code := run(t, map[string]string{"CANYON_HOST": "nonsense"}, false, "--version"); code != exitOK {
		t.Errorf("canyon --version exited %d on a machine whose CANYON_HOST is wrong", code)
	}
	if out, err, code := run(t, map[string]string{"CANYON_HOST": "nonsense"}, false, "version"); code != exitError || out != "" || !strings.Contains(err, "CANYON_HOST") {
		t.Errorf("a wrong CANYON_HOST: exit %d, stdout %q, stderr %q", code, out, err)
	}
	before, _, code := run(t, nil, false, "--host", "https://canyon.example.com", "version")
	after, _, code2 := run(t, nil, false, "version", "--host", "https://canyon.example.com")
	if code != exitOK || code2 != exitOK || before != after {
		t.Errorf("before the verb %q (exit %d), after it %q (exit %d)", before, code, after, code2)
	}
	if out, err, code := run(t, nil, false, "version", "extra", "--host", "http://box:7777"); code != exitError || out != "" || !strings.Contains(err, "takes no arguments") {
		t.Errorf("an argument beside a flag: exit %d, stdout %q, stderr %q", code, out, err)
	}
	if out, _, code := run(t, nil, false, "version", "--format", "json"); code != exitOK || !strings.HasPrefix(strings.TrimSpace(out), `{"host":`) {
		t.Errorf("canyon version --format json exited %d and printed %q", code, out)
	}
	a := &app{out: io.Discard, err: io.Discard, getenv: func(string) string { return "" }, verb: command{name: "status"}}
	if _, err := a.parse([]string{"--format", "json"}, func(*flag.FlagSet) {}); err == nil || !strings.Contains(err.Error(), "status has no json rendering") {
		t.Errorf("a verb with no json rendering accepted the flag: %v", err)
	}
	if _, err := a.parse([]string{"--format", "ai"}, func(*flag.FlagSet) {}); err != nil {
		t.Errorf("ai must fall back to plain, not refuse: %v", err)
	}
}

// Every error line goes through one writer, so a flag name or an argument carrying escape bytes is
// spelled out there too, not only a host.
func TestNoErrorPathPrintsAControlByteRaw(t *testing.T) {
	for _, args := range [][]string{{"--\x1b[2J"}, {"version", "--\x1b[2J"}, {"version", "extra", "-\x1b[2J"}, {"\x1b[2Jnonsense"}} {
		out, err, code := run(t, nil, false, args...) // no terminal: the help carries no colour of its own
		if code != exitError || strings.ContainsRune(out+err, 0x1b) || !strings.Contains(err, `\x1b`) {
			t.Errorf("%q: exit %d, stderr %q", args, code, err)
		}
	}
}

// visible spells out what draws nothing and still changes what is read: the bidi controls that
// reorder a line, the zero-width characters, and the tag characters a model reads and a person never sees.
func TestVisibleSpellsOutWhatDrawsNothing(t *testing.T) {
	for in, want := range map[string]string{
		"a\u202eb\u202ac": `a\u202eb\u202ac`,
		"a\u2066b\u2069c": `a\u2066b\u2069c`,
		"a\u200bb\u200fc": `a\u200bb\u200fc`,
		"a\u2060b\ufeffc": `a\u2060b\ufeffc`,
		"a\U000e0041b":    `a\U000e0041b`,
		"a\U000e007fb":    `a\U000e007fb`,
		"café → ✓":        "café → ✓",
	} {
		if got := visible(in); got != want {
			t.Errorf("visible(%q) = %q, want %q", in, got, want)
		}
	}
}
