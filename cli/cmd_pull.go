package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/internal/names"
)

// runPull fetches an app's current package into a directory (cli.md §6.6): the tar streamed straight
// into the server's own unpacker, then the package recorded as the directory's base. The base is
// written last and any older one cleared first, so a pull that fails halfway leaves no base.
func runPull(ctx context.Context, a *app, args []string) int {
	var named string
	var force bool
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&named, "app", "", "The app to pull, when the directory holds no "+projectFile)
		fs.BoolVar(&force, "force", false, "Write over a directory that is not empty (nothing in it is deleted)")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 1 {
		return a.usage(fmt.Errorf("pull takes one directory, and got %q as well", rest[1]))
	}
	dir, into := ".", "this directory"
	if len(rest) == 1 {
		dir, into = rest[0], rest[0]
	}
	app, err := pullApp(dir, named)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	fed := ""
	if !force { // its code is in git: git pull brings it (cli.md §6.6, #1938); --force pulls anyway
		fed = repositoryFeeding(ctx, client, app)
	}
	if fed != "" {
		a.errf("\n  %s %s's code is in github.com/%s. Use git pull.\n", a.render.glyph(stateFail), cell(app), cell(fed))
		return exitError
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return a.usage(fmt.Errorf("%q cannot be created: %s", dir, plainly(err)))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return a.usage(fmt.Errorf("%q cannot be read: %s", dir, plainly(err)))
	}
	if len(entries) > 0 && !force {
		return a.usage(fmt.Errorf("%s is not empty. canyon pull --force writes the package's files over what is there and deletes nothing", into))
	}
	body, header, err := client.stream(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/packages/current", "application/x-tar")
	if err != nil { // a refused or unreachable host touches nothing here: the old base is still true of the tree
		if named == "" { // canyon.toml names the app: guard (a), when its own read says the host lacks it (§3.2)
			err = lackingAsked(ctx, err, app, client)
		}
		if _, none := refusal(err, http.StatusNotFound, "not_found"); none {
			a.errf("\n  %s %s has no package yet, so there is nothing to pull (or there is no such app).\n", a.render.glyph(stateFail), cell(app))
			return exitError
		}
		return a.fail(err)
	}
	defer func() { _ = body.Close() }()
	id := header.Get("Canyon-Package")
	if err := checkPackage(id); err != nil { // before a byte lands: a bad id is not worth a half tree
		return a.fail(fmt.Errorf("%w (the Canyon-Package header)", err))
	}
	if err := clearBase(dir); err != nil { // before the first byte: a base must never name a tree that is not there
		return a.fail(err)
	}
	pkg, err := ingest.UntarWorkTree(body, dir) // the host's unpacker and its caps, at a working tree's modes; the host already applied the exclusions
	if err != nil {
		_ = clearBase(dir) // a package that carried a base file of its own must not leave it behind a failure
		return a.fail(err)
	}
	if err := writeBase(dir, app, id); err != nil {
		return a.fail(err)
	}
	a.printf("\n  %s %s\n\n    %s   %s   into %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)),
		a.render.paint(ansiBold, "Pulled "+cell(app)+" "+a.dot()+" "+cell(id)), plural(pkg.Files, "file", "files"), size(pkg.Bytes), cell(into))
	return exitOK
}

// pullApp is --app, else the directory's canyon.toml, else a usage error naming --app. A canyon.toml
// naming another app is refused whatever the flags say: --force is about contents, never about which app.
func pullApp(dir, named string) (string, error) {
	if named != "" && (len(named) > maxProjectName || !names.ValidProject(named)) { // the same two rules readProject applies
		return "", fmt.Errorf("--app %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter, at most %d characters", named, maxProjectName)
	}
	p, err := readProject(dir)
	var none *NoProjectFile
	switch {
	case errors.As(err, &none) && named == "" && none.NoApp:
		return "", fmt.Errorf("%s here names no app, so name it: canyon pull --app <id>", projectFile)
	case errors.As(err, &none) && named == "":
		return "", fmt.Errorf("there is no %s in %s, so name the app: canyon pull --app <name>", projectFile, dir)
	case errors.As(err, &none):
		return named, nil
	case err != nil:
		return "", err
	case named != "" && named != p.App:
		return "", fmt.Errorf("%s here already names the app %q, and --app asked for %q: pull into another directory", projectFile, p.App, named)
	}
	return p.App, nil
}
