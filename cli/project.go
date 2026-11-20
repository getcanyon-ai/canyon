package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// projectFile is committed, so a teammate who clones the repo inherits it (design/specs/cli.md §1).
const projectFile = "canyon.toml"

// maxProjectName is the cap POST /api/v1/projects puts on a name: a DNS label with room for the
// `-preview` its container carries, 55 + 8 = 63 (contracts/routes.md).
const maxProjectName = 55

// maxAppName is the cap POST /api/v1/projects puts on an app's display name, in characters.
const maxAppName = 100

// NoProjectFile is a directory with no canyon.toml at all, or NoApp, one whose canyon.toml has no app
// line: an app not made yet. `canyon deploy` answers both by making the app and writing its id (§3.2,
// setup.go); every other verb says so.
type NoProjectFile struct {
	Dir   string
	NoApp bool
}

func (e *NoProjectFile) Error() string {
	if e.NoApp {
		return filepath.Join(e.Dir, projectFile) + " names no app yet: canyon deploy makes the app and writes its id there"
	}
	return "there is no " + projectFile + " in " + e.Dir
}

// ProjectSyntax is canyon.toml read with the TOML parser the server reads it with, and refused: not
// TOML, or holding what Canyon does not read — a top-level key but app, description and owner, a
// section but [resources] and [roles] — so a file Canyon half-reads, or a setting it ignores, is said
// (#1254). A half-understood file names the wrong app.
type ProjectSyntax struct {
	Path string
	Line int    // the line TOML stopped at; 0 when the file is TOML
	Text string // that line, or what Canyon does not read
	Why  string
}

// Error quotes the line: a verb prints this, and an unread line is arbitrary bytes off a disk.
func (e *ProjectSyntax) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s line %d is not TOML: %q (%s).", e.Path, e.Line, e.Text, e.Why)
	}
	return fmt.Sprintf("%s holds %q, which Canyon does not read: %s.", e.Path, e.Text, e.Why)
}

// project is canyon.toml read. `description` is the server's to read at upload (cli.md §1), and
// `owner` is accepted and ignored: no v1 route takes it. Types is each [resources] type as written,
// which only the server checks: the CLI reads it to ask for one the host refused (askS3).
type project struct {
	App   string
	Types []string
}

// project is the app the verb is about: the one an MCP tool names, else the one canyon.toml names in
// the verb's directory.
func (a *app) project() (project, error) {
	if a.named != "" {
		if len(a.named) > maxProjectName || !names.ValidProject(a.named) { // readProject's two rules for canyon.toml's
			return project{}, fmt.Errorf("app %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter, at most %d characters", a.named, maxProjectName)
		}
		return project{App: a.named}, nil
	}
	dir, err := a.getwd()
	if err != nil {
		return project{}, err
	}
	return readProject(dir)
}

func readProject(dir string) (project, error) {
	path := filepath.Join(dir, projectFile)
	body, err := os.ReadFile(path) //nolint:gosec // the app's own committed file, at the directory the verb was run in
	if errors.Is(err, fs.ErrNotExist) {
		if _, link := os.Lstat(path); link == nil { // a link whose target is gone: something is here, so nothing is written through it
			return project{}, fmt.Errorf("%q is a link to a file that is not there: point it at one, or remove it", path)
		}
		return project{}, &NoProjectFile{Dir: dir}
	}
	if err != nil {
		return project{}, fmt.Errorf("%q cannot be read: %s", path, plainly(err))
	}
	if bytes.HasPrefix(body, []byte("\ufeff")) { // a Windows editor's byte-order mark: no TOML parser here reads past it, nor the server's
		return project{}, &ProjectSyntax{Path: path, Line: 1, Text: "\ufeff", Why: "it starts with a byte-order mark; save it as UTF-8 without one"}
	}
	var file map[string]any
	if err := toml.Unmarshal(body, &file); err != nil {
		var de *toml.DecodeError
		line := 1
		if errors.As(err, &de) {
			line, _ = de.Position()
		}
		lines := strings.Split(string(body), "\n")
		return project{}, &ProjectSyntax{Path: path, Line: line, Text: strings.TrimSpace(lines[min(line, len(lines))-1]), Why: strings.TrimPrefix(err.Error(), "toml: ")}
	}
	var p project
	for _, key := range slices.Sorted(maps.Keys(file)) {
		_, table := file[key].(map[string]any)
		switch {
		case key == "app" || key == "description" || key == "owner":
			value, ok := file[key].(string)
			if !ok {
				return project{}, &ProjectSyntax{Path: path, Text: key, Why: key + " is a string, as " + key + ` = "<text>"`}
			}
			if key == "app" {
				p.App = value
			}
		case key == "resources":
			resources, _ := file[key].(map[string]any)
			for _, entry := range resources {
				if declared, ok := entry.(map[string]any); ok {
					if typ, ok := declared["type"].(string); ok {
						p.Types = append(p.Types, typ)
					}
				}
			}
		case key == "roles": // the server's to read (contracts/routes.md, the upload row)
		case table:
			return project{}, &ProjectSyntax{Path: path, Text: "[" + key + "]", Why: "its sections are [resources] and [roles], and Canyon detects the build itself"}
		default:
			return project{}, &ProjectSyntax{Path: path, Text: key, Why: "above its first [section] it holds app, description and owner"}
		}
	}
	_, named := file["app"]
	switch {
	case !named: // an app = "" is a line, and a wrong one: only a file without the key is an app not made yet
		return project{}, &NoProjectFile{Dir: dir, NoApp: true}
	case len(p.App) > maxProjectName:
		return project{}, fmt.Errorf("%s names an app of %d characters, and a Canyon app id is at most %d", path, len(p.App), maxProjectName)
	case !names.ValidProject(p.App):
		return project{}, fmt.Errorf("%s names the app %q, which is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter", path, p.App)
	}
	return p, nil
}

// plainly is the half of a filesystem error a person reads — "is a directory", "permission denied" —
// without Go's `open <path>:` in front of it, because the line that prints this already names the path.
func plainly(err error) string {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}

// assignment splits `key = "value"` in two. Deliberately not a TOML parser: the value is one
// double-quoted string with no escape and nothing after it, and anything else comes back unread.
func assignment(text string) (key, value string, ok bool) {
	eq := strings.Index(text, "=")
	if eq < 0 {
		return "", "", false
	}
	key, value = strings.TrimSpace(text[:eq]), strings.TrimSpace(text[eq+1:])
	if key == "" || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", "", false
	}
	if inner := value[1 : len(value)-1]; !strings.ContainsAny(inner, `"\`) {
		return key, inner, true
	}
	return "", "", false
}
