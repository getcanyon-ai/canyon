package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// specFence is the one fenced block under a heading of design/specs/cli.md, bounded to that
// section: the read stops at the next heading, so a fence further down the file is never the one.
func specFence(t *testing.T, heading string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "design", "specs", "cli.md"))
	if err != nil {
		t.Fatalf("the spec the shipped skill text is copied from: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, heading)
	if start < 0 {
		t.Fatalf("design/specs/cli.md carries no heading %q", heading)
	}
	var fence fenceState
	var fences []string
	var body []string
section:
	for _, line := range lines[start+1:] {
		was := fence.open
		switch inside := fence.inside(line); {
		case !inside && strings.HasPrefix(line, "#"):
			break section
		case was == 0 && fence.open > 0:
			body = nil
		case was > 0 && fence.open == 0:
			fences = append(fences, strings.Join(body, "\n")+"\n")
		case inside:
			body = append(body, line)
		}
	}
	if fence.open > 0 {
		t.Fatalf("the fence under %q is never closed", heading)
	}
	if len(fences) != 1 {
		t.Fatalf("%q holds %d fenced blocks, and the shipped skill text is the one of them", heading, len(fences))
	}
	return fences[0]
}

func TestTheShippedSkillTextIsTheSpecsOwnFence(t *testing.T) {
	want := specFence(t, "### 7.2 The skill")
	got := claudeFront + skillBody
	if got == want {
		return
	}
	shipped, spec := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(shipped), len(spec)) {
		if quoted(shipped, i) != quoted(spec, i) {
			t.Fatalf("the shipped text and §7.2 of design/specs/cli.md differ at line %d.\n  shipped: %s\n  spec:    %s",
				i+1, quoted(shipped, i), quoted(spec, i))
		}
	}
}

func quoted(lines []string, i int) string {
	if i >= len(lines) {
		return "(no line)"
	}
	return strconv.Quote(lines[i])
}

func TestTheShippedTextNamesNoVerbThisBinaryLacks(t *testing.T) {
	verbs := make(map[string]bool, len(commands))
	for _, c := range commands {
		verbs[c.name] = true
	}
	fields := strings.Fields(strings.NewReplacer("`", " ", "\n", " ").Replace(skillBody))
	for i, f := range fields {
		if f != "canyon" || i+1 == len(fields) {
			continue
		}
		if verb := strings.Trim(fields[i+1], ".,…"); !verbs[verb] {
			t.Errorf("the shipped text tells an agent to run %q, which this binary has no verb for", "canyon "+verb)
		}
	}
}

// The text is the binary's and not a setup's: a config the machine cannot use and a host it never
// reaches are both left alone, so the run prints on any machine and nothing but the text comes out.
func TestSkillPrintWritesTheTextAndTouchesNothing(t *testing.T) {
	var hits atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer host.Close()
	for _, c := range []struct{ harness, front string }{
		{targetClaude, claudeFront},
		{targetCursor, cursorFront},
		{targetAgents, agentsFront},
	} {
		t.Run(c.harness, func(t *testing.T) {
			cwd, home := t.TempDir(), t.TempDir()
			t.Chdir(cwd)
			conf := filepath.Join(home, ".config", "canyon", "config.json")
			if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(conf, []byte("not the JSON Canyon writes"), 0o600); err != nil {
				t.Fatal(err) // a read of it warns on stderr, which the run below must not do
			}
			out, errOut, code := run(t, map[string]string{"HOME": home, "CANYON_HOST": host.URL}, false, "skill", "--print", "--harness", c.harness)
			if code != exitOK || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			block := skillBlock(skillBody)
			if want := c.front + block; out != want { // the bytes install will write, markers included
				t.Errorf("--print wrote\n%s\nwant\n%s", out, want)
			}
			// The fronts and the marker as bytes, not as the constants the verb itself reads: a typo in one is a typo here too.
			if lit := map[string]string{targetClaude: "---\nname: canyon\ndescription: ", targetCursor: "---\ndescription: ", targetAgents: "<!-- canyon:skill v:1 hash:"}[c.harness]; !strings.HasPrefix(out, lit) || (c.harness == targetCursor && !strings.Contains(out, "\nalwaysApply: false\n---\n\n<!-- canyon:skill v:1 hash:")) {
				t.Errorf("%s: the front matter does not open on its own bytes:\n%s", c.harness, out)
			}
			if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
				t.Errorf("--print left %v in the working directory (%v)", entries, err)
			}
			if entries, err := os.ReadDir(filepath.Join(home, ".claude")); !os.IsNotExist(err) { // the tree install will write
				t.Errorf("--print left %v under the home directory (%v)", entries, err)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("--print reached the host %d times", n)
	}
}

func TestTheSkillFormsAndFormats(t *testing.T) {
	t.Chdir(t.TempDir())
	out, errOut, code := run(t, nil, false, "skill")
	if code != exitOK || errOut != "" || !strings.Contains(out, "canyon skill --print") {
		t.Fatalf("canyon skill alone: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if o, e, code := run(t, nil, false, "skill", "remove"); code != exitError || o != "" || !strings.Contains(e, `got "remove"`) {
		t.Errorf("a form this binary lacks: exit %d, stdout %q, stderr %q", code, o, e)
	}
	if o, e, code := run(t, nil, false, "skill", "--print", "--harness", "emacs"); code != exitError || o != "" || !strings.Contains(e, `unknown --harness value "emacs": use claude-code, codex, cursor or agents-md`) {
		t.Errorf("an unknown harness: exit %d, stdout %q, stderr %q", code, o, e)
	}
	want, err := skillText(targetClaude)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "ai"} { // the text is the artifact, not a verdict: no wrapping, no refusal
		if o, e, code := run(t, nil, false, "skill", "--print", "--format", format); code != exitOK || e != "" || o != want {
			t.Errorf("--format %s: exit %d, stderr %q, stdout\n%s", format, code, e, o)
		}
	}
}

// The accepted harness names are exactly the four §7.1 documents: a stray entry in the map, such
// as install's "all", would print under a name no help text or spec expects.
func TestNoHarnessIsAcceptedButTheFour(t *testing.T) {
	for h := range skillFronts {
		if h != targetClaude && h != targetCodex && h != targetCursor && h != targetAgents {
			t.Errorf("skillFronts accepts %q, which no harness name or help text expects", h)
		}
	}
	if len(skillFronts) != 4 {
		t.Errorf("skillFronts has %d entries, want 4", len(skillFronts))
	}
}

// The skill tells an agent to run canyon check before a new app's first deploy, and after a change that
// adds a dependency, a route or a secret, so what blocks shows before the deploy does (#2622).
// An agent adding a call to a new outside service learned only after its deploy that a person allows it, and a
// refusal reached the app's log as a cancelled request (#2708): the skill says both, once. After the Allow, an agent
// read the app's old failures as current and changed its code and deployed again (#2722): it says what to do then.
func TestSkillNamesOutboundOnce(t *testing.T) {
	want := "- An app's environment may be under an allowlist, where Canyon refuses calls to\n" +
		"  any outside service not on it. Only a person on the app's team can allow a new\n" +
		"  one, in Canyon's dashboard; you cannot. Never try to reach a refused service\n" +
		"  another way. Canyon also refuses any call out to the internet to an IP address,\n" +
		"  a one-word host name or a port other than 80 and 443, and no one can allow it:\n" +
		"  call a host name on 80 or 443. When a call out fails, `canyon logs` and\n" +
		"  `canyon status` list, within a minute, the outside services Canyon refused as\n" +
		"  not allowed; they don't list the other refusals. Once a person allows one, the\n" +
		"  app's next call to it goes through with no deploy: check that call before you\n" +
		"  change any code, and never deploy again for it.\n"
	if !strings.Contains(skillBody, want) || strings.Count(skillBody, "allowlist") != 1 {
		t.Errorf("the shipped skill does not say once\n%s", want)
	}
}

// An agent that needs an app's data runs canyon db query as the user, never reads DATABASE_URL and never asks for a
// connection string
// (design/DATABASE-SESSIONS.md §9; #2138): a write only on the exact statement, and no word of production, which this
// version doesn't open (#2843).
func TestTheSkillTellsAnAgentToQueryTheDatabase(t *testing.T) {
	want := "To look at an app's data, run `canyon db query \"select …\"`: one statement, run\n" +
		"as the user, read-only unless you add `--write`. Never read the app's\n" +
		"DATABASE_URL, and never ask the user for a connection string or a database\n" +
		"password. Add `--write` only when the user confirms the exact statement. If a\n" +
		"database client must connect, `canyon db connect` prints a url that carries a\n" +
		"token: never paste it anywhere but that client.\n"
	if !strings.Contains(skillBody, want) || strings.Contains(skillBody, "--env prod --reason") {
		t.Errorf("the shipped skill does not say\n%s\nor steers an agent to production's data", want)
	}
}

// canyon check refuses a canyon.toml with no app (cli.md §3.2), which a new app has until its first deploy writes one:
// the skill sends a new app to canyon deploy first, and canyon check after it (the bed journeys, v0.11.0).
func TestSkillNamesCanyonCheckAfterAFirstDeploy(t *testing.T) {
	want := "A new app's first `canyon deploy` reviews it before anything runs, and writes the\napp's id into `canyon.toml`. After that, run `canyon check` after a change that adds\na dependency, a route or a secret"
	if !strings.Contains(skillBody, want) || strings.Contains(skillBody, "`canyon check` before the first `canyon deploy`") {
		t.Errorf("the shipped skill does not say %q, or still sends a new app to canyon check first", want)
	}
}

// An agent offered to take a key pasted into the chat (#2162's journey, #2737), where a value lands in the agent's
// transcript and its provider's logs: the installed skill, in every harness, and the session briefing give the person
// canyon secret set NAME to run in their own terminal, and say never to ask for or take a value in the conversation.
// Every passage of each naming canyon secret set, against testdata/secrets-in-chat.txt.
func TestTheSkillSaysNeverTakeASecretInChat(t *testing.T) {
	naming := func(text string) string { // a passage starts at a list item or after a blank line
		var passages []string
		for line := range strings.Lines(text) {
			starts := len(passages) == 0 || strings.TrimSpace(line) == "" || strings.HasPrefix(line, "- ") ||
				len(line) > 2 && line[0] >= '0' && line[0] <= '9' && line[1] == '.'
			if starts {
				passages = append(passages, "")
			}
			if strings.TrimSpace(line) != "" {
				passages[len(passages)-1] += line
			}
		}
		var out strings.Builder
		for _, p := range passages {
			if strings.Contains(p, "canyon secret set") {
				out.WriteString(p)
			}
		}
		return out.String()
	}
	skill := ""
	for _, harness := range []string{targetClaude, targetCodex, targetCursor, targetAgents} {
		text, err := skillText(harness)
		if err != nil {
			t.Fatal(err)
		}
		if got := naming(text); skill == "" {
			skill = got
		} else if got != skill {
			t.Errorf("%s's skill says\n%s\nwhere %s's says\n%s", harness, got, targetClaude, skill)
		}
	}
	got := "The skill, in every harness:\n" + skill + "The session briefing:\n" + naming(agentManifest(manifest{Host: "http://h"}, true))
	if want := golden(t, "secrets-in-chat.txt"); got != want {
		t.Errorf("rendered\n%s\ntestdata/secrets-in-chat.txt wants\n%s", got, want)
	}
}
