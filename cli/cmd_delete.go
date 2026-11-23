package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
)

// runDelete deletes this directory's app, destroying its data (routes.md, DELETE /api/v1/projects/:id).
// The host lists what goes; the person types the app's name, never a y/N; a machine passes --confirm.
func runDelete(ctx context.Context, a *app, args []string) int {
	var confirm string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&confirm, "confirm", "", "The app's name, for CI, where nothing can type it")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("delete takes no arguments, and got %q", rest[0]))
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	p, err := readProject(dir)
	if err != nil {
		return a.usage(err)
	}
	if confirm != "" && confirm != p.App {
		return a.usage(fmt.Errorf("--confirm must name %s, the app canyon.toml names, and got %q", cell(p.App), confirm))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	a.header(p.App, "")
	if _, err := a.checkBase(ctx, client, dir, p.App, "", false); err != nil { // guard (b), before the typed name (§6.6)
		return a.fail(err)
	}
	path := "/api/v1/projects/" + url.PathEscape(p.App)
	databases := false // known only when the project was read: a --confirm delete claims none
	if confirm == "" {
		var read struct {
			Project struct {
				Resources []resource      `json:"resources"`
				Envs      map[string]*env `json:"envs"`
			} `json:"project"`
		}
		if err := lacking(client.get(ctx, path, &read), p.App, client); err != nil { // the listing is a read: a delete is sent only once the name is typed; guard (a) from its 404 (§3.2)
			return a.fail(err)
		}
		resources := slices.DeleteFunc(read.Project.Resources, func(r resource) bool { return r.Env == "" }) // a bare declaration holds no data
		if !a.canPrompt() {
			return a.fail(fmt.Errorf("delete asks you to type the app's name, and nothing here can: re-run it with --confirm %s", cell(p.App)))
		}
		databases = len(resources) > 0
		if databases {
			a.errf("\n  Deleting %s destroys its secrets and these, with their data, immediately:\n", cell(p.App))
			for _, r := range resources {
				theirs := "" // the org's platform deletes what it provisioned, on the delete's remove (contracts/routes.md, #2432)
				if r.State == "provided" {
					theirs = ", your platform's"
				}
				a.errf("    %s %s in %s%s\n", cell(r.Type), cell(r.Name), cell(r.Env), theirs)
			}
		} else {
			a.errf("\n  Deleting %s destroys its secrets, immediately.\n", cell(p.App))
		}
		if prod := read.Project.Envs["prod"].running(); prod != nil {
			a.errf("  Production is live at %s, and goes offline with it.\n", cell(prod.URL))
		}
		a.errf("  Undeploy keeps them; this does not.\n  Type %s to delete it: ", cell(p.App))
		typed, _ := bufio.NewReader(a.in).ReadString('\n')
		if strings.TrimSpace(typed) != p.App {
			a.printf("\n  Nothing was deleted.\n")
			a.declined = true
			return exitError
		}
	}
	var answer struct{ Pending []map[string]any }
	pending, err := client.delPending(ctx, path+"?confirm="+url.QueryEscape(p.App), &answer)
	if err != nil {
		return a.deleteFail(lacking(err, p.App, client), cell(p.App)) // the route's 404 is the app's alone: guard (a) (§3.2)
	}
	if pending { // still under way: a database a driver is deleting, an environment the org's platform is removing, or both
		databases := slices.ContainsFunc(answer.Pending, func(row map[string]any) bool { return row["release"] == nil })
		releases := slices.ContainsFunc(answer.Pending, func(row map[string]any) bool { return row["release"] != nil })
		if databases || !releases {
			a.printf("\n  %s Canyon is still deleting %s's databases. Run this again to finish.\n", a.render.glyph(statePending), cell(p.App))
		}
		if releases {
			a.printf("\n  %s %s: being removed by your platform. Run this again to finish.\n", a.render.glyph(statePending), cell(p.App))
		}
		return exitOK
	}
	gone := ""
	if databases {
		gone = " Its databases and their data are gone."
	}
	a.printf("\n  %s Deleted %s.%s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), cell(p.App), gone)
	return exitOK
}

// deleteFail is the route's other answers in the person's words; anything else prints as every verb's do.
func (a *app) deleteFail(err error, name string) int {
	var api *APIError
	line, code := "", exitError
	switch {
	case !errors.As(err, &api):
		return a.fail(err)
	case api.Code == "teardown_failed":
		line = "Not deleted: " + api.Message + "."
	case api.Code == "person_required":
		line, code = "Only a person can delete "+name+": it holds production. Sign in as yourself with canyon login, then run this again.", exitBlocked
	case api.Code == "run_in_progress":
		line = "A run of " + name + " is under way" + runNamed(api.Extra["run"]) + ". Let it finish, then run this again."
	default:
		return a.fail(err)
	}
	a.unpark(false) // a refusal stands alone under ai: the parked first line is dropped, as fail drops it
	a.errf("\n  %s %s\n", a.render.glyph(stateFail), line)
	return code
}
