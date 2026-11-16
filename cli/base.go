package cli

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The base is the package this directory last pulled or sent, in .canyon/base beside canyon.toml
// (cli.md §6.6): `app` and `package` in assignment()'s format, per directory, self-ignoring.
const (
	baseDir  = ".canyon"
	baseFile = "base"
)

// validPackage is what a package id may look like on the wire in either direction: broader than
// what the host mints, narrower than a header can carry, so an edited file cannot inject one.
var validPackage = regexp.MustCompile(`^pkg_[A-Za-z0-9._-]{1,120}$`)

// readBase is the package this directory last recorded for app, or "" when there is none: no file,
// or a base recorded under another app, which is no base. One that can't be used — a file that can't
// be read, one that names no app (cut short, or not a base), a package of app that isn't an id — is an
// error, never no base: the verbs that check the base refuse it (cli.md §6.6), and an edited file puts
// nothing on the wire.
func readBase(dir, app string) (string, error) {
	body, err := os.ReadFile(filepath.Join(dir, baseDir, baseFile)) //nolint:gosec // the directory's own record, beside its canyon.toml
	// No file is no base, and nor is a plain file named .canyon, which is the user's.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("it cannot be read: " + cause(err))
	}
	var forApp, pkg string
	for _, line := range strings.Split(string(body), "\n") {
		switch key, value, _ := assignment(strings.TrimSpace(line)); key {
		case "app":
			forApp = value
		case "package":
			pkg = value
		}
	}
	switch {
	case forApp == "":
		return "", errors.New("it names no app")
	case forApp != app:
		return "", nil
	case !validPackage.MatchString(pkg):
		return "", errors.New("the package in it is not a package id")
	}
	return pkg, nil
}

// checkBase is guard (b), cli.md §6.6: a package id is its tree, so a host this tree was never sent to
// has no such id, and a checkout whose base names a package of app asks the host for it before the
// verb changes anything. Only the route's own 404,
// unknown_package, refuses; any other 404 is a host older than the route, or an app gone, and the
// verb goes on to its own answer. A 200 must be that package's row, or it is not Canyon answering.
// made is an app this invocation created, which has no package to find (§3.2). base is what an
// upload sends; env is the verb's environment, for the ai block, "" for a verb with none.
func (a *app) checkBase(ctx context.Context, c *Client, dir, app, env string, made bool) (base string, err error) {
	base, err = readBase(dir, app)
	if err != nil {
		return "", &baseRefused{app: app, host: c.Base, env: env, why: err.Error()}
	}
	if base == "" || made {
		return base, nil
	}
	var read struct {
		Package struct {
			ID string `json:"id"`
		} `json:"package"`
	}
	err = c.get(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/packages/"+url.PathEscape(base), &read)
	var api *APIError
	switch _, unknown := refusal(err, http.StatusNotFound, "unknown_package"); {
	case unknown:
		return "", &baseRefused{app: app, host: c.Base, env: env, base: base}
	case err == nil && read.Package.ID != base:
		return "", &NotCanyon{Host: c.Base, Status: http.StatusOK}
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return base, nil
	}
	return base, err
}

// baseRefused is guard (b)'s refusal: base, a package the host doesn't have, or why the base can't be used.
type baseRefused struct{ app, host, env, base, why string }

func (e *baseRefused) Error() string {
	if e.why != "" {
		return ".canyon/base can't be used: " + e.why
	}
	return e.base + " isn't on " + e.host
}

// sentElsewhere is the sentence after the refusal's first, said of a base the host doesn't have.
const sentElsewhere = "This checkout was last sent or pulled with it, so it went to another Canyon, or this one no longer has it."

// refusedBase renders §6.6's refusal, exit 1, from fail: on stderr, each other host this machine is
// signed in to with the verb path to run there, and the way to say this one is right; under ai the
// verb's block, naming those hosts with no command, since which Canyon an app belongs on is the person's.
func (a *app) refusedBase(e *baseRefused) int {
	verb := cmp.Or(a.form, a.verb.name) // the verb path alone, a rollback's version shaped as one aside: never a name or value typed after it
	if verb == "undeploy" {
		verb += " --env " + e.env // a flag undeploy can't run without, never a secret's name
	}
	var others []string
	if e.why == "" {
		others = a.signedIn(e.host)
	}
	if a.render.Format != FormatAI {
		glyph := a.render.glyph(stateFail)
		if e.why != "" {
			a.errf("\n  %s %s. If this checkout belongs on %s, run: rm .canyon/base\n", glyph, e.Error(), e.host)
			return exitError
		}
		a.errf("\n  %s %s.\n    %s\n\n", glyph, e.Error(), sentElsewhere)
		for i, host := range others {
			label := ""
			if i == 0 {
				label = "Another Canyon:"
			}
			a.errf("    %-18scanyon %s --host %s\n", label, verb, host)
		}
		a.errf("    %-18srm .canyon/base, then canyon %s again\n", "This one:", verb)
		return exitError
	}
	nothing := " Nothing was sent."
	if a.form == "secret set" {
		nothing = " Nothing was set."
	}
	a.printf("%s — RESULT: REFUSED\n\napp: %s\n", a.head(), cell(e.app))
	if e.env != "" {
		a.printf("env: %s\n", cell(e.env))
	}
	a.printf("host: %s\n", cell(e.host))
	if e.why != "" {
		a.printf("outcome: %s.%s\n\n", cell(e.Error()), nothing)
		a.printf("NEXT STEPS:\n1. Tell the user the outcome line, and ask whether this checkout belongs on %s.\n"+
			"2. Only if the user says it does, run: rm .canyon/base\n"+
			"3. Do not delete .canyon/base unless the user says this checkout belongs on %s, and do not add --force, --host or --create, or set CANYON_HOST to get past this.\n", cell(e.host), cell(e.host))
	} else {
		a.printf("base: %s\noutcome: %s. %s%s\n", cell(e.base), cell(e.Error()), sentElsewhere, nothing)
		hosts := ""
		if len(others) > 0 {
			a.printf("also signed in to:\n")
			for _, host := range others {
				a.printf("  %s\n", cell(host))
			}
			hosts = " and the hosts above"
		}
		a.printf("\nNEXT STEPS:\n1. Tell the user the outcome line%s, and ask which Canyon %s belongs on.\n"+
			"2. Stop until they answer. Do not delete .canyon/, pull, or add --force, --host or --create, or set CANYON_HOST to get past this.\n", hosts, cell(e.app))
	}
	a.printf("\nexit code: %d\n", exitError)
	return exitError
}

// checkPackage is the one rule for a package id this build will record and later send back as a
// header: pull checks the host's answer before it writes a byte, and writeBase checks it again.
func checkPackage(pkg string) error {
	if pkg == "" {
		return errors.New("the host named no package")
	}
	if !validPackage.MatchString(pkg) {
		return errors.New("the host answered a package id this build will not record: " + cell(pkg))
	}
	return nil
}

// writeBase records pkg as app's base through the temp-file-and-rename config.save uses, beside a
// .gitignore that hides the directory. The user's own .gitignore is never touched. It records the
// base last, atomically, and returns a hard error; the caller decides what that means (pull fails,
// since nothing else happened; an upload that succeeded should warn, not fail).
func writeBase(dir, app, pkg string) error {
	if err := checkPackage(pkg); err != nil {
		return err
	}
	at := filepath.Join(dir, baseDir)
	if err := os.MkdirAll(at, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(at, ".gitignore"), []byte("*\n"), 0o644); err != nil { //nolint:gosec // one glob, no secret
		return err
	}
	// Both values are ids by here, so neither needs escaping.
	return atomicWrite(filepath.Join(at, baseFile), []byte("app     = \""+app+"\"\npackage = \""+pkg+"\"\n"), true)
}

// clearBase removes the record before a pull writes a byte, so a pull that fails halfway leaves no
// base naming a tree that is not there.
func clearBase(dir string) error {
	if err := os.Remove(filepath.Join(dir, baseDir, baseFile)); !os.IsNotExist(err) {
		return err
	}
	return nil
}
