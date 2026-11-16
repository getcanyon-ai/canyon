package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A secret: line is a runtime secret the user had set before they first speak (#2162's scenario: OPENAI_API_KEY in
// preview, so the version running after an Allow still calls OpenAI). It names an env of the app's and a name, and
// nothing else.
func TestASecretLineNamesAnEnvAndAName(t *testing.T) {
	dir := t.TempDir()
	write := func(name, header string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("stack: Node\nexpect first: PASSED\n"+header+"\n## Turns\n\n### 1\n```\nHi.\n```\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	s, err := parseScenario(write("ok.md", "secret: preview OPENAI_API_KEY\nsecret: prod STRIPE_KEY"))
	if err != nil || len(s.Secrets) != 2 || s.Secrets[0] != (seededSecret{"preview", "OPENAI_API_KEY"}) || s.Secrets[1] != (seededSecret{"prod", "STRIPE_KEY"}) {
		t.Errorf("parsed %+v, %v", s.Secrets, err)
	}
	for name, header := range map[string]string{
		"no-name.md":       "secret: preview",
		"an-env-no-app.md": "secret: staging OPENAI_API_KEY",
		"a-value.md":       "secret: preview OPENAI_API_KEY sk-proj-0123",
	} {
		if _, err := parseScenario(write(name, header)); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
}

// The runner sets each secret as the tester would, canyon secret set NAME --env <env> in the app with the value on
// stdin, before the seed deploys. The value is made up at run time, a new one each run, the shape of an OpenAI key
// that no account holds; it never reaches a log, which keeps the CLI's answer alone. A refusal stops the scenario.
func TestTheRunnerSetsAMadeUpSecretBeforeTheSeedDeploys(t *testing.T) {
	var values []string
	for _, refused := range []bool{false, false, true} {
		home, run := t.TempDir(), t.TempDir()
		dir := filepath.Join(run, "egress")
		app := filepath.Join(dir, "qa-egress")
		for _, d := range []string{filepath.Join(home, "bin"), app} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		got := filepath.Join(run, "stdin")
		exit := "0"
		if refused {
			exit = "1"
		}
		script := "#!/bin/sh\n[ \"$*\" = 'secret set OPENAI_API_KEY --env preview' ] || exit 2\n[ \"$(pwd -P)\" = \"$(cd '" + app + "' && pwd -P)\" ] || exit 3\n" +
			"cat > '" + got + "'\necho '  ✓ OPENAI_API_KEY set for preview'\nexit " + exit + "\n"
		bin := filepath.Join(home, "bin", "canyon")
		if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // test: a fake canyon the runner runs, its owner's alone
			t.Fatal(err)
		}
		err := setSecrets(t.Context(), home, dir, app, []seededSecret{{"preview", "OPENAI_API_KEY"}})
		if (err != nil) != refused {
			t.Errorf("refused %v: %v", refused, err)
		}
		value, _ := os.ReadFile(got)
		if !regexp.MustCompile(`^sk-proj-[0-9a-f]{40}\n$`).Match(value) {
			t.Errorf("the value on stdin is %q, want a made-up key's shape", value)
		}
		values = append(values, string(value))
		logged, _ := os.ReadFile(filepath.Join(dir, "secrets.log"))
		if string(logged) != "  ✓ OPENAI_API_KEY set for preview\n" || strings.Contains(string(logged)+errString(err), strings.TrimSpace(string(value))) {
			t.Errorf("secrets.log %q, error %v: the CLI's answer alone, never the value", logged, err)
		}
	}
	if values[0] == values[1] {
		t.Errorf("two runs set the same value %q: it is made up at run time", values[0])
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
