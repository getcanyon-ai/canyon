package cli

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/url"
	"os"
)

// runRename is cli.md §6.9: the app's display name, which its owners and admins change. Only the name
// changes: the id stays its address, canyon.toml's app and every --app (routes.md, PUT …/name).
func runRename(ctx context.Context, a *app, args []string) int {
	var named string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&named, "app", "", "The app to rename, when it is not the one "+projectFile+" names")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) != 1 {
		return a.usage(errors.New(`rename takes the app's new name, quoted when it has spaces: canyon rename "Claims portal"`))
	}
	app, err := appNamed(named)
	if err != nil {
		return a.usage(err)
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	a.header(app, "")
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	if _, err := a.checkBase(ctx, client, dir, app, "", false); err != nil { // guard (b): a rename changes the app
		return a.fail(err)
	}
	var read struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	}
	err = client.put(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/name", map[string]string{"name": rest[0]}, http.StatusOK, &read)
	if named == "" { // canyon.toml names the app: guard (a) when the app's own read is 404 too (§3.2) — a host older than the route answers it 404 as well; --app has the host's words
		err = lackingAsked(ctx, err, app, client)
	}
	if err != nil {
		return a.fail(err)
	}
	a.printf("\n  %s is now named %s. Its address, canyon.toml and --app stay %s.\n", cell(app), cell(read.Project.Name), cell(app))
	return exitOK
}
