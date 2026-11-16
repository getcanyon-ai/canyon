package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeProject puts a canyon.toml in a fresh directory and answers the directory.
func writeProject(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, projectFile), []byte(body), 0o600))
	return dir
}

// The file design/specs/cli.md §1 prints, as it prints it: padded around the `=`, a comment above
// it, and a description the server reads and the skill tells an agent to write. An owner line an
// older file carries is read past too, and so is everything else TOML says a string may be — a
// literal string, a trailing comment, an escaped quote — as the server's parser reads it (#1254).
// Then a name of exactly the cap, which the route takes.
func TestTheFileTheSpecPrintsIsRead(t *testing.T) {
	for _, body := range []string{
		"# the app this directory is\napp         = \"expense-tracker\"\ndescription = \"Log team expenses and send them to finance\"\n\n",
		"app   = \"expense-tracker\"\nowner = \"ada@meridian.example\"\n",
		"app = 'expense-tracker'\n",
		"app = \"expense-tracker\"  # the app's id\n",
		"app = \"expense-tracker\"\ndescription = \"The \\\"probe\\\" app\"\n",
		"app = \"\"\"expense-tracker\"\"\"\n",
	} {
		if p, err := readProject(writeProject(t, body)); err != nil || p.App != "expense-tracker" {
			t.Fatalf("%q read as %q: %v", body, p.App, err)
		}
	}
	at := strings.Repeat("a", maxProjectName)
	if p, err := readProject(writeProject(t, "app = \""+at+"\"\n")); err != nil || p.App != at {
		t.Errorf("a name of exactly %d characters was refused: %v", maxProjectName, err)
	}
}

// Everything the reader will not read: not TOML, a key twice, a key or a section Canyon does not read —
// a [build] section that would pass and configure nothing — and a byte-order mark. A file it
// half-understands names the wrong app, so each is an exit and never a skip, naming what it is.
func TestEveryLineTheReaderWillNotRead(t *testing.T) {
	for _, tc := range []struct{ name, body, quoted string }{
		{"an array", "app = [\"claims-portal\"]\n", `app`},
		{"a key v1 has no use for", "app = \"claims-portal\"\nstack = \"next\"\n", `stack`},
		{"a section Canyon does not read", "app = \"claims-portal\"\n\n[build]\ncommand = \"make\"\n", `[build]`},
		{"the app named twice", "app = \"claims-portal\"\napp = \"claims-portal\"\n", `app = "claims-portal"`},
		{"the owner named twice", "app = \"ok\"\nowner = \"a@b.c\"\nowner = \"d@e.f\"\n", `owner = "d@e.f"`},
		{"an escape byte on an unread line", "app\x1b[2J\n", "app\x1b[2J"},
		{"a byte-order mark", "\ufeffapp = \"claims-portal\"\n", "\ufeff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var syntax *ProjectSyntax
			if _, err := readProject(writeProject(t, tc.body)); !errors.As(err, &syntax) {
				t.Fatalf("%s was read instead of refused: %v", tc.name, err)
			} else if syntax.Text != tc.quoted {
				t.Errorf("the refusal carried %q instead of %q", syntax.Text, tc.quoted)
			} else if msg := syntax.Error(); !strings.Contains(msg, strconv.Quote(tc.quoted)) {
				t.Errorf("the message prints the line raw instead of quoted:\n%s", msg)
			}
		})
	}
}

// A section is the server's to read ([resources] and [roles], contracts/routes.md): what is in one is
// never the file's app, so the app is named above the first section or not at all.
func TestTheReaderStopsAtTheFirstSection(t *testing.T) {
	p, err := readProject(writeProject(t, "app = \"claims-portal\"\n\n[resources]\ndb = { type = \"postgres\" }\napp = 'anything'\n"))
	if err != nil || p.App != "claims-portal" {
		t.Fatalf("a file with a [resources] section read as %q: %v", p.App, err)
	}
	if _, err := readProject(writeProject(t, "[roles]\napp = \"claims-portal\"\n")); err == nil || !strings.Contains(err.Error(), "names no app") {
		t.Fatalf("an app named only below a section was read: %v", err)
	}
}

// The app id is the one POST /api/v1/projects takes, checked against the pattern both sides share
// (internal/names): a name the route would refuse never costs a round trip to learn it.
func TestTheAppNameIsTheOneTheRouteTakes(t *testing.T) {
	for _, app := range []string{"Claims-Portal", "claims_portal", "1claims", "claims-", "claims.portal", "", strings.Repeat("a", maxProjectName+1)} {
		if _, err := readProject(writeProject(t, "app = \""+app+"\"\n")); err == nil {
			t.Errorf("%q was read as an app id", app)
		}
	}
}

// A directory with no file is its own answer, so `check` and `deploy` can tell it apart from a bad
// one rather than guessing the app from the directory's name.
func TestADirectoryWithNoFileSaysSo(t *testing.T) {
	dir := t.TempDir()
	var missing *NoProjectFile
	if _, err := readProject(dir); !errors.As(err, &missing) || missing.Dir != dir {
		t.Fatalf("a directory with no %s answered %v", projectFile, err)
	}
}
