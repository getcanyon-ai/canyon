package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// setup is cli.md §3.2: a first deploy in a directory whose canyon.toml is missing, or names no app,
// makes the app and writes its id at the top of the file. --app names the id exactly, and the deploy
// makes it or joins it; otherwise the host picks the id and the directory's name is the app's name
// (#1911). Nothing is asked: the name is for people, and the host makes the id. A canyon.toml that
// will not parse is the app's own text, and never reaches here.
func (a *app) setup(ctx context.Context, client *Client, dir, flagged string, noApp bool) (project, error) {
	opening := "No " + projectFile + " here"
	if noApp {
		opening = "No app in " + projectFile
	}
	id, note := flagged, "from --app"
	if flagged != "" {
		if err := appID("--app", flagged); err != nil { // before the block prints: an id no route takes is not a question
			return project{}, err
		}
	} else {
		if err := writable(dir); err != nil { // before the host makes an app this directory could not record
			return project{}, err
		}
		name := []rune(filepath.Base(dir))
		name = name[:min(len(name), maxAppName)]
		var made struct {
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
		}
		if err := client.postJSON(ctx, "/api/v1/projects", map[string]string{"name": string(name)}, http.StatusCreated, &made); err != nil {
			return project{}, err
		}
		id, note = made.Project.ID, "named "+strconv.Quote(string(name))+" after this folder"
		if err := appID("the id the host answered", id); err != nil { // it is written into canyon.toml unquoted
			return project{}, err
		}
	}
	a.printf("\n  %s %s setting up this app.\n  app: %s (%s)\n", opening, a.dash(), id, note)
	if err := writeProjectFile(dir, id); err != nil {
		return project{}, err
	}
	a.printf("\n  %s Wrote %s %s commit it so your team deploys the same app.\n",
		a.render.paint(ansiGreen, a.render.glyph(statePass)), projectFile, a.dash())
	a.wrote = projectFile + " (app " + id + ", " + note + ")"
	return project{App: id}, nil
}

// appID refuses an id this host's routes will not take: the host never slugifies one (contracts/routes.md,
// POST /projects {id}), so neither does the CLI.
func appID(note, name string) error {
	switch {
	case len(name) > maxProjectName:
		return fmt.Errorf("%s %q is %d characters, and a Canyon app id is at most %d", note, name, len(name), maxProjectName)
	case !names.ValidProject(name):
		return fmt.Errorf("%s %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter", note, name)
	}
	return nil
}

// confirmed is deploy --env prod's answer: it asks about code customers are about to see, so Enter is
// no and only a typed yes is one (§6.3).
func confirmed(in io.Reader) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	typed := strings.ToLower(strings.TrimSpace(line))
	return typed == "y" || typed == "yes"
}

// writable is whether canyon.toml can be written here, asked by opening it for writing, or creating
// and removing a file beside it when there is none, and changing nothing.
func writable(dir string) error {
	path := filepath.Join(dir, projectFile)
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // the app's own committed file
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.CreateTemp(dir, ".canyon-*")
		if err == nil {
			defer func() { _ = os.Remove(f.Name()) }()
		}
	}
	if err != nil {
		return fmt.Errorf("%q cannot be written: %s", path, plainly(err))
	}
	return f.Close()
}

// writeProjectFile puts the app line at the top of canyon.toml, above whatever the file already holds,
// at the mode a committed source file has. A directory that will not take it is a Canyon line, not a
// Go one, naming the line to add: the app is made by then.
func writeProjectFile(dir, id string) error {
	path := filepath.Join(dir, projectFile)
	rest, err := os.ReadFile(path) //nolint:gosec // the app's own committed file, which readProject has just read
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%q cannot be read: %s", path, plainly(err))
	}
	// the id passed appID, so it needs no escaping
	line := "app = \"" + id + "\""
	if err := os.WriteFile(path, append([]byte(line+"\n"), rest...), 0o644); err != nil { //nolint:gosec // committed with the app's source, and read by everyone who clones it
		return fmt.Errorf("%q cannot be written: %s. Add the line %s to it yourself", path, plainly(err), line)
	}
	return nil
}
