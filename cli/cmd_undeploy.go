package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// envNames is how a person says each environment: the flag's word for preview, the full one for prod.
var envNames = map[string]string{"preview": "preview", "prod": "production"}

// stopping is what undeploy says when the host answers 202 {stopping} (design/EXPERIENCE.md D5).
var stopping = map[string]string{
	"preview": "Preview is stopping: a deploy was already switching it, and Canyon stops it the moment it lands.",
	"prod":    "Production is stopping: a promotion was already switching it, and Canyon stops it the moment it lands.",
}

// holdUsage is the one form a hold takes (design/OPERATIONS.md §3.8).
const holdUsage = `canyon undeploy --app <app> --env prod --hold --reason "<why>"`

// runUndeploy takes one environment offline and keeps its data (routes.md, DELETE .../envs/:env).
// Taking prod down weighs what putting it up does, so prod asks the question deploy --env prod asks.
// With --hold a responder or an admin stops production and holds it (routes.md, The hold).
func runUndeploy(ctx context.Context, a *app, args []string) int {
	var env, named, reason string
	var yes, hold bool
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&env, "env", "", "preview | prod")
		fs.StringVar(&named, "app", "", "The app to take offline, when it is not the one "+projectFile+" names")
		fs.BoolVar(&hold, "hold", false, "For a security responder or an admin: stop production and hold it until one of them lets it run again, in Canyon's dashboard")
		fs.StringVar(&reason, "reason", "", "Why production is held, with --hold")
		fs.BoolVar(&yes, "yes", false, "Take prod offline without asking, for CI, where nothing can answer")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("undeploy takes no arguments, and got %q", rest[0]))
	}
	a.park() // under ai a refusal's block replaces the first line, as on check (§6.6); every return unparks
	defer a.unpark(true)
	// No default: the verb stops an app, so the environment is always the one the person typed, but for
	// a hold, which holds production alone and so means it (#2258).
	if hold && env == "" {
		env = "prod"
	}
	where, ok := envNames[env]
	switch {
	case !ok:
		return a.usage(fmt.Errorf("undeploy needs --env preview or --env prod, and got %q", env))
	case hold && reason == "":
		return a.usage(errors.New("--hold needs --reason: " + holdUsage))
	case !hold && reason != "":
		return a.usage(errors.New("--reason goes with --hold: " + holdUsage))
	case hold && env != "prod":
		return a.usage(errors.New("--hold holds production alone: " + holdUsage))
	}
	dir, err := os.Getwd()
	if err != nil {
		return a.usage(err)
	}
	app, err := appNamed(named) // undeploy reads canyon.toml; writing it is deploy's alone (§3.2)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	if hold {
		a.header(app, " prod "+a.render.punct("→", "->")+" held")
	} else {
		a.header(app, "")
		if _, err := a.checkBase(ctx, client, dir, app, env, false); err != nil { // guard (b), before production's question (§6.6); a hold sends no tree, and its responder reads no package
			return a.fail(err)
		}
	}
	if _, err := ensureProject(ctx, client, app, false); err != nil { // guard (a), after (b) as delete's read is, and before production's question (§3.2)
		if lacks := (*notOnHost)(nil); errors.As(err, &lacks) {
			lacks.env = env
		}
		return a.fail(err)
	}
	if hold {
		return a.holdProd(ctx, client, app, reason, yes)
	}
	if env == "prod" {
		if code, ok := a.confirm(yes, "Take "+cell(app)+" offline in production?", "Nothing was taken offline."); !ok {
			return code
		}
	}
	var release struct {
		Release  string
		Stopping *struct{} // a run was already switching the environment: it stops it once it lands (contracts/routes.md, The brake)
	}
	pending, err := client.delPending(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/envs/"+env, &release)
	if err != nil {
		return a.undeployFail(err, cell(app), where)
	}
	if pending && release.Stopping != nil {
		a.printf("\n  %s %s\n", a.render.glyph(statePending), stopping[env])
		return exitOK
	}
	if pending { // the org's platform runs it: a remove release went out, and the env waits on its report (design/HAND-OFF.md §7)
		a.printf("\n  %s %s in %s: removing · waiting on your platform. Its data and secrets are kept.\n", a.render.glyph(statePending), cell(app), where)
		return exitOK
	}
	a.printf("\n  %s Took %s offline in %s. Its data and secrets are kept.\n",
		a.render.paint(ansiGreen, a.render.glyph(statePass)), cell(app), where)
	return exitOK
}

// holdProd stops production and holds it, after saying what that does and asking as taking prod
// offline asks (design/OPERATIONS.md §3.8). It answers as the brake does: stopped, a run already
// switching production stops it once it lands, or the org's platform removes it. Only the dashboard
// lifts a hold, so the verb ends by saying so.
func (a *app) holdProd(ctx context.Context, client *Client, app, reason string, yes bool) int {
	a.printf("\n  Stopping %s in production and holding it: nobody can promote it until you or an admin let it run again.\n", cell(app))
	a.printf("  Its team can still deploy to preview.\n")
	if code, ok := a.confirm(yes, "Stop and hold "+cell(app)+" in production?", "Nothing was held."); !ok {
		return code
	}
	var answer struct {
		Release  string
		Stopping *struct{}
	}
	body := struct {
		Reason string `json:"reason"`
	}{reason}
	pending, err := client.postPending(ctx, "/api/v1/projects/"+url.PathEscape(app)+"/envs/prod/hold", body, &answer)
	if err != nil {
		err = lacking(err, app, client) // its 404 is the app's alone: guard (a) (§3.2)
		if lacks := (*notOnHost)(nil); errors.As(err, &lacks) {
			lacks.env = "prod"
		}
		return a.undeployFail(err, cell(app), envNames["prod"])
	}
	held := "  " + a.render.paint(ansiGreen, a.render.glyph(statePass)) + " Held    prod " + a.dot() + " stopped"
	switch {
	case pending && answer.Stopping != nil:
		held = "  " + a.render.glyph(statePending) + " Held    prod " + a.dot() + " stopping\n  " + stopping["prod"]
	case pending:
		held = "  " + a.render.glyph(statePending) + " Held    prod " + a.dot() + " removing " + a.dot() + " waiting on your platform"
	}
	a.printf("\n%s\n\n  %s\n", held, a.faint("Lift it in Canyon's dashboard when it may run again."))
	return exitOK
}

// undeployFail is the route's refusals in the person's words; anything else prints as every verb's do.
func (a *app) undeployFail(err error, name, where string) int {
	var api *APIError
	if !errors.As(err, &api) {
		return a.fail(err)
	}
	line, code := "", exitError
	switch {
	case api.Status == http.StatusNotFound && api.Code == "not_found":
		line = "Nothing is running in " + where + "."
	case api.Status == http.StatusForbidden && api.Code == "person_required":
		line, code = "Only a person can take "+name+" offline in "+where+". Sign in as yourself with canyon login, then run this again.", exitBlocked
	case api.Status == http.StatusConflict && api.Code == "run_in_progress":
		line = "A run of " + name + " is under way" + runNamed(api.Extra["run"]) + ". Let it finish, then run this again."
	case api.Status == http.StatusInternalServerError && api.Code == "internal":
		line = "The host could not stop " + name + " in " + where + ". Run this again: it picks up where it stopped."
	default:
		return a.fail(err)
	}
	a.unpark(false) // a refusal stands alone under ai: the parked first line is dropped, as fail drops it
	a.errf("\n  %s %s\n", a.render.glyph(stateFail), line)
	return code
}
