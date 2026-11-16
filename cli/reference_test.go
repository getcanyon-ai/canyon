package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reference is the docs site's CLI page (docs/README.md): every command's --help as this binary
// prints it, so the page can never describe a flag the CLI doesn't have.
var reference = filepath.Join("..", "docs", "src", "content", "docs", "reference", "cli.md")

// forms are the verbs with forms of their own, each with its own --help: the ones that set app.form
// (cmd_secret.go, cmd_skill.go, cmd_db.go, cmd_db_connect.go). A new form goes here too.
var forms = [][]string{{"secret", "list"}, {"secret", "set"}, {"secret", "rm"}, {"skill", "install"}, {"skill", "uninstall"}, {"db", "query"}, {"db", "sessions"}, {"db", "end"}, {"db", "connect"}, {"demo", "tour"}}

func referencePage(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("---\ntitle: CLI reference\ndescription: Every canyon command and flag, as the CLI's own --help prints them.\n---\n\n")
	b.WriteString("This page is the CLI's own help, generated from it. CI fails when it differs from what the CLI\nprints, so it always matches the CLI built from the same commit.\n")
	section := func(args ...string) {
		out, _, code := run(t, nil, false, append(args, "--help")...)
		if code != exitOK {
			t.Fatalf("canyon %s --help exited %d", strings.Join(args, " "), code)
		}
		b.WriteString("\n## " + strings.Join(append([]string{"canyon"}, args...), " ") + "\n\n```text\n" + strings.Trim(out, "\n") + "\n```\n")
	}
	section()
	for _, c := range commands {
		section(c.name)
		for _, f := range forms {
			if f[0] == c.name {
				section(f...)
			}
		}
	}
	return b.String()
}

// TestTheCLIReferenceIsTheHelp holds the docs page to the help; CANYON_UPDATE_DOCS=1 rewrites it.
func TestTheCLIReferenceIsTheHelp(t *testing.T) {
	want := referencePage(t)
	if os.Getenv("CANYON_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(reference, []byte(want), 0o644); err != nil { //nolint:gosec // a docs page, world-readable like every file in the repo
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(reference) //nolint:gosec // a path this test names
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != want {
		t.Fatalf("%s is not what the CLI prints: run make docs-cli", reference)
	}
}
