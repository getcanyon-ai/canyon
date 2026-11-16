package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// accessRead is the Access group's one shape (contracts/routes.md), answered by the GET and by every
// PUT: who holds a grant, whether everyone does, and who has asked.
type accessRead struct {
	Everyone *struct {
		Roles []string `json:"roles"`
	} `json:"everyone"`
	Entries []struct {
		Kind    string   `json:"kind"`
		Grantee string   `json:"grantee"`
		Level   string   `json:"level"`
		Roles   []string `json:"roles"`
	} `json:"entries"`
	Requests []struct {
		Email       string `json:"email"`
		RequestedAt string `json:"requested_at"`
		Level       string `json:"level"` // none or viewer, the asker's when they asked; "" before Canyon recorded it (contracts/routes.md)
	} `json:"requests"`
}

// levels are the words screens say (design/EXPERIENCE.md D6) and the API's values under them.
var levels = map[string]string{"owner": "owner", "contributor": "contributor", "read-only": "viewer"}

// runShare is cli.md §6.7: the app's team, and one grant added, replaced or removed. Every change
// answers with the team as it now stands.
func runShare(ctx context.Context, a *app, args []string) int {
	var named, level, remove string
	roles := []string{} // sent as [] and never null: the route reads an omitted list as [], not a null one
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&named, "app", "", "The app to share, when it is not the one "+projectFile+" names")
		fs.StringVar(&level, "level", "", "owner | contributor | read-only (default read-only)")
		fs.Func("role", "An app role to grant with it; repeatable", func(r string) error { roles = append(roles, r); return nil })
		fs.StringVar(&remove, "remove", "", "Remove a grant: an email, group:<name> or everyone")
	})
	if err != nil {
		return a.usage(err)
	}
	who := remove
	switch {
	case len(rest) > 1:
		return a.usage(fmt.Errorf("share takes one email, group:<name> or everyone, and got %q as well", rest[1]))
	case remove != "" && (len(rest) > 0 || level != "" || len(roles) > 0):
		return a.usage(errors.New("--remove takes the grant alone: canyon share --remove sam@example.com"))
	case len(rest) == 1:
		who = rest[0]
	case remove == "" && (level != "" || len(roles) > 0):
		return a.usage(errors.New("--level and --role belong to a grant: canyon share sam@example.com --level contributor"))
	}
	kind, grantee := "email", who
	switch name, group := strings.CutPrefix(who, "group:"); {
	case who == "everyone":
		kind = "everyone"
	case group && name == "":
		return a.usage(errors.New("group: needs the group's name: canyon share group:claims-engineering"))
	case group:
		kind, grantee = "group", name
	}
	if level == "" {
		level = "read-only"
	} else if kind == "everyone" {
		return a.usage(errors.New("everyone is read-only, so --level is refused"))
	}
	value, ok := levels[level]
	if !ok {
		return a.usage(fmt.Errorf("--level is owner, contributor or read-only, and got %q", level))
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
	auth, err := firstContact(ctx, client)
	if err != nil {
		return a.fail(err)
	}
	if auth.Auth == "off" { // nothing is shared where nobody signs in, and the routes are not served there
		// a refusal stands alone under ai: the parked first line is dropped
		a.unpark(false)
		a.errf("\n  Sign-in is off on this Canyon, so anyone who can reach it can open every app. Turn sign-in on to share with specific people.\n") // a fact about the host, not a failure of yours: no ✗
		return exitError
	}
	if who != "" { // a grant changes the app; the list alone is a read (§6.6)
		dir, err := os.Getwd()
		if err != nil {
			return a.usage(err)
		}
		if _, err := a.checkBase(ctx, client, dir, app, "", false); err != nil { // guard (b)
			return a.fail(err)
		}
	}
	at := "/api/v1/projects/" + url.PathEscape(app) + "/access"
	grant := at + "/" + kind + "/" + url.PathEscape(grantee)
	var read accessRead
	switch {
	case who == "":
		err = client.get(ctx, at, &read)
	case remove != "":
		if err = client.del(ctx, grant); err == nil {
			err = client.get(ctx, at, &read)
		}
	default:
		err = client.put(ctx, grant, map[string]any{"level": value, "roles": roles}, http.StatusOK, &read)
	}
	switch {
	case named != "": // --app has no canyon.toml behind it: the host's words
	case remove == "": // canyon.toml names the app: guard (a), from the first read or grant's 404 (§3.2)
		err = lacking(err, app, client)
	default: // a removal's 404 can be "no such grant": the app's own read says which
		err = lackingAsked(ctx, err, app, client)
	}
	if err != nil {
		return a.fail(err)
	}
	said := cell(who) // the grant as it was typed, and everyone as the list says it
	if kind == "everyone" {
		said = "everyone at " + cell(auth.Org)
	}
	switch {
	case remove != "":
		a.printf("\n  Removed %s. The app may still let them in for up to a minute.\n", said)
	case who != "":
		if err := a.shared(ctx, client, app, said); err != nil {
			return a.fail(err)
		}
	}
	a.renderAccess(app, auth.Org, read, time.Now())
	return exitOK
}

// shared is §6.7's line once a grant lands: an app's own sign-in learns of it about a minute after
// this does, so it says so, with the address of each environment that runs, the links to send.
func (a *app) shared(ctx context.Context, client *Client, app, grant string) error {
	var read struct {
		Project struct {
			Envs map[string]*env `json:"envs"`
		} `json:"project"`
	}
	if err := client.get(ctx, "/api/v1/projects/"+url.PathEscape(app), &read); err != nil {
		a.printf("\n  Shared with %s.\n", grant) // the grant landed; a read that fails after it must not read as a share that did not
		return nil
	}
	var urls []string
	for _, name := range []string{"preview", "prod"} {
		if e := read.Project.Envs[name].running(); e != nil {
			urls = append(urls, e.URL)
		}
	}
	if len(urls) == 0 {
		a.printf("\n  Shared with %s.\n", grant)
		return nil
	}
	a.printf("\n  Shared with %s. They can open it within about a minute:\n", grant)
	for _, u := range urls {
		a.printf("    %s\n", a.render.paint(ansiCyan+ansiUnderline, cell(u)))
	}
	return nil
}

// appNamed is --app when it is given, checked as readProject checks canyon.toml, else the app this
// directory's canyon.toml names.
func appNamed(named string) (string, error) {
	if named != "" {
		if len(named) > maxProjectName || !names.ValidProject(named) {
			return "", fmt.Errorf("--app %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter, at most %d characters", named, maxProjectName)
		}
		return named, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	p, err := readProject(dir) // share reads the file; writing it is deploy's alone (§3.2)
	return p.App, err
}

// renderAccess is §6.7's screen: each grant as it would be typed, its level in the screens' words
// and its roles, then everyone, then the requests with the command that grants each.
func (a *app) renderAccess(app, org string, read accessRead, now time.Time) {
	a.printf("\n  %s\n\n", a.render.paint(ansiBold, cell(app)))
	var team [][3]string
	for _, e := range read.Entries {
		who := cell(e.Grantee)
		if e.Kind == "group" {
			who = "group:" + who
		}
		word := cell(e.Level)
		if e.Level == "viewer" { // the API's value; screens say read-only
			word = "read-only"
		}
		team = append(team, [3]string{who, word, roleList(e.Roles)})
	}
	if read.Everyone != nil {
		team = append(team, [3]string{"everyone at " + cell(org), "read-only", roleList(read.Everyone.Roles)})
	}
	a.shareRows(team)
	if len(read.Requests) > 0 {
		a.printf("\n  Requests:\n")
		var asked [][3]string
		for _, r := range read.Requests {
			asked = append(asked, [3]string{cell(r.Email), a.ago(r.RequestedAt, now), shareHint(cell(r.Email), r.Level)})
		}
		a.shareRows(asked)
	}
}

// shareHint is the command that answers a request: a read-only member's at contributor, since a plain share adds
// them read-only again and changes nothing (#2774).
func shareHint(email, level string) string {
	if level == "viewer" {
		return "canyon share " + email + " --level contributor"
	}
	return "canyon share " + email
}

// shareRows is one list, its names padded to the longest of them (at least 29) so the levels line up.
func (a *app) shareRows(rows [][3]string) {
	width := 29
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r[0]))
	}
	for _, r := range rows {
		a.printf("  %s\n", strings.TrimRight(pad(r[0], width)+"  "+pad(r[1], 12)+"  "+r[2], " "))
	}
}

func roleList(roles []string) string {
	shown := make([]string, len(roles))
	for i, r := range roles {
		shown[i] = cell(r)
	}
	return strings.Join(shown, ", ")
}
