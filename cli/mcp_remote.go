package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
	"github.com/getcanyon-ai/canyon/internal/names"
)

// errNoDirectory is getwd's answer on /mcp: a remote tool works from what Canyon holds, never a directory.
var errNoDirectory = errors.New("a tool on Canyon's own /mcp works from the app Canyon holds, never a directory")

// RemoteServer is canyon-server's /mcp for one request (design/specs/mcp.md §2): the local server's tools, handlers
// and words, minus the two that read a directory, plus the two that work from an app's linked repository. Every
// request a tool makes goes to rt with base as its host, which canyon-server serves in-process as the caller. Nothing
// here reads this machine: no directory, no home, no config or session file, and no environment variable.
// base is the host's public URL: every link a tool answers names it, and nothing dials it, as rt serves every request.
func RemoteServer(ctx context.Context, rt http.RoundTripper, base, version string) *mcp.Server {
	return remoteServer(ctx, rt, base, version, wallClock)
}

func remoteServer(ctx context.Context, rt http.RoundTripper, base, version string, clk clock) *mcp.Server {
	a := remoteApp(ctx, remoteClient(base, rt))
	a.clock = clk
	instructions := mcptools.Preamble
	if m, _, err := a.sub(ctx, "").readManifest(ctx, "", hookWithin); err == nil { // the org's rules: no directory names an app here
		instructions += "\n\n" + agentManifest(m, false)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "canyon", Title: "Canyon", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	for _, t := range append(mcptools.Tools(), mcptools.RemoteOnly()...) {
		switch t.Name {
		case mcptools.Playbook:
			s.AddTool(t, remotely(a.mcpPlaybook(t)))
			continue
		case mcptools.CreateApp:
			s.AddTool(t, remotely(a.mcpCreateApp(t)))
			continue
		case mcptools.SecretLink:
			t := mcptools.Remote(t)
			s.AddTool(t, remotely(a.mcpSecretLink(t)))
			continue
		}
		t := mcptools.Remote(t)
		s.AddTool(t, remotely(a.mcpTool(t)))
	}
	s.AddResource(&mcp.Resource{URI: mcptools.ManifestURI, Name: "manifest", Title: "Canyon's rules", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.ManifestAppTemplate, Name: "manifest-app", Title: "Canyon's rules for one app", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.PlaybookTemplate, Name: "playbook", Title: "One of the org's playbooks", MIMEType: "text/markdown"}, a.playbookResource)
	return s
}

// remoteApp is an invocation on /mcp: its client given, its config empty and never loaded, an environment that
// holds nothing, and the notice decided (none).
func remoteApp(ctx context.Context, client *Client) *app {
	var out, errs strings.Builder
	render, _ := resolveRender(string(FormatAI), noEnv, false)
	return &app{out: &out, err: &errs, in: strings.NewReader(""), getenv: noEnv, clock: wallClock, render: render, ctx: ctx,
		http: client, conf: &config{}, noticed: true, remote: true}
}

func noEnv(string) string { return "" }

// remoteClient is one tool call's client on /mcp: rt to base, and a jar of its own that no other call shares.
func remoteClient(base string, rt http.RoundTripper) *Client {
	jar, _ := cookiejar.New(nil) // documented never to fail for nil options
	return &Client{Base: base, HTTP: &http.Client{Transport: rt, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// packaged is a check's or deploy's package: a directory's upload, or on /mcp the ref of the app's linked repository,
// which Canyon packages itself (contracts/routes.md, packages/from-repository).
func (a *app) packaged(ctx context.Context, client *Client, dir string, run *checkRun) error {
	if a.ref == "" {
		return a.prologue(ctx, client, dir, run)
	}
	var made struct {
		Package struct {
			ID     string `json:"id"`
			Commit string `json:"commit"`
		} `json:"package"`
	}
	body := map[string]any{"ref": a.ref}
	if a.verb.name == "check" { // review-only, as a check's upload is: it never moves the app's current package
		body["check"] = true
	}
	if err := client.postJSON(ctx, "/api/v1/projects/"+url.PathEscape(run.App)+"/packages/from-repository", body, http.StatusCreated, &made); err != nil {
		return err
	}
	if !validPackage.MatchString(made.Package.ID) {
		return &NotCanyon{Host: client.Base, Status: http.StatusCreated}
	}
	run.Package = made.Package.ID
	a.printf("\n  %s Package    %s %s %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), cell(a.ref), a.dot(), cell(shortCommit(made.Package.Commit)))
	return nil
}

// shortCommit is a commit as a person reads it: its first twelve characters.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// signInOff is whether the host's sign-in is off (GET /auth/config). On /mcp it is on, and nothing is asked: the host
// serves /mcp only with sign-in, and a remote tool reaches /api/v1 alone.
func (a *app) signInOff(ctx context.Context, client *Client) (bool, error) {
	if a.remote {
		return false, nil
	}
	var auth authConfig
	err := client.get(ctx, "/auth/config", &auth)
	return auth.Auth == "off", err
}

// mcpCreateApp is canyon_create_app (design/specs/mcp.md §2.3): an app made for the caller from a repository, its
// owner, and linked to it under the Link a repository rule; a link refused deletes the app it made, and says what the
// person can do about it.
func (a *app) mcpCreateApp(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args mcptools.CreateAppArgs
		if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
			return toolRefused(err), nil
		}
		c := a.sub(ctx, "")
		client, page := c.http, c.http.page()
		name := args.Name
		if name == "" {
			_, name, _ = strings.Cut(args.Repository, "/")
			if args.Folder != "" {
				name = path.Base(args.Folder)
			}
		}
		var made struct {
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
		}
		if err := client.postJSON(ctx, "/api/v1/projects", map[string]string{"name": name}, http.StatusCreated, &made); err != nil {
			return toolRefused(c.refusal(err)), nil
		}
		id := made.Project.ID
		if !names.ValidProject(id) {
			return toolRefused(&NotCanyon{Host: client.Base, Status: http.StatusCreated}), nil
		}
		at := "/api/v1/projects/" + url.PathEscape(id)
		err := client.put(ctx, at+"/repository", map[string]string{"full_name": args.Repository, "folder": args.Folder}, http.StatusOK, nil)
		if err == nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(createdApp, cell(name), id, cell(args.Repository), page+"/#/apps/"+id, id)}}}, nil // text alone, as every tool
		}
		if derr := error(nil); derr != nil { // PLANTED FAULT: a refused link keeps the app // the link refused: the app it made goes, so nothing half-made stays
			return toolRefused(fmt.Errorf("%w; and the app %s it made couldn't be deleted: %s", c.refusal(err), id, c.sub(ctx, "").refusal(derr))), nil
		}
		switch {
		case isRefusal(err, http.StatusForbidden, "github_link_required"):
			return toolRefused(fmt.Errorf(linkGitHubFirst, page+"/api/v1/github/link")), nil //nolint:staticcheck // D5's line, said whole
		case isRefusal(err, http.StatusNotFound, "not_found"):
			return toolRefused(fmt.Errorf(askForAccess, cell(args.Repository), page+"/#/settings/github")), nil //nolint:staticcheck // D5's line, said whole
		}
		return toolRefused(c.refusal(err)), nil
	}
}

func isRefusal(err error, status int, code string) bool {
	_, ok := refusal(err, status, code)
	return ok
}

// canyon_create_app's lines (design/EXPERIENCE.md, D5).
const (
	createdApp      = "Made %s (%s), linked to %s. Its page: %s\nNext: canyon_deploy_ref with app %s and the branch to deploy."
	linkGitHubFirst = "Nothing was made: Canyon reads a repository for you only where your own GitHub account can. Open %s in your browser to link your GitHub account, then ask again."
	askForAccess    = "Nothing was made: Canyon's GitHub App doesn't see %s. Ask an admin to give Canyon's GitHub App access to it, in Settings › GitHub: %s"
)

// handoffOf is how long a's tools follow a run before handing it off: about a minute's call on /mcp, ten locally.
func handoffOf(a *app) time.Duration {
	if a.remote {
		return mcptools.RemoteHandoff
	}
	return mcptools.Handoff
}

// mcpReview is canyon_review: a run of the app named, read once it has ended and said as canyon check says a verdict,
// every finding with how to fix it; while it runs, that it does.
func (a *app) mcpReview(ctx context.Context, app, run string) *mcp.CallToolResult {
	c := a.sub(ctx, "")
	c.verb, c.named = command{name: "check"}, app
	client := c.http
	at := "/api/v1/projects/" + url.PathEscape(app)
	if err := client.get(ctx, at+"/status?run="+url.QueryEscape(run), &struct{}{}); err != nil { // one that isn't this app's gate run is 404
		return toolRefused(c.refusal(err))
	}
	var read struct {
		Run runResult `json:"run"`
	}
	if err := client.get(ctx, "/api/v1/gate-runs/"+url.PathEscape(run), &read); err != nil {
		return toolRefused(c.refusal(err))
	}
	if read.Run.State == "pending" || read.Run.State == "running" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(reviewRunning, cell(run), cell(app))}}}
	}
	checked := checkRun{App: app, Host: client.page(), Run: read.Run.ID, Package: read.Run.Package, Result: read.Run}
	c.park()
	c.appRules(ctx, client, &checked)
	code := c.verdict(checked)
	return &mcp.CallToolResult{IsError: code == exitError, Content: []mcp.Content{&mcp.TextContent{Text: c.said()}}}
}

// reviewRunning is canyon_review's line for a run still going (design/EXPERIENCE.md, D5).
const reviewRunning = "%s is still running. Call canyon_review with app %s and this run again in a minute."

// secretSet is how a step names setting the secret NAME: local, as the CLI says it, or on /mcp the link canyon_secret_link
// answers, opened in the person's own browser (design/specs/mcp.md §2.3); env is the environment a remote step names.
func (a *app) secretSet(local, env string) string {
	if !a.remote {
		return local
	}
	if env != "" {
		return "the link canyon_secret_link gives for NAME and env " + env + ", opened in their own browser"
	}
	return "the link canyon_secret_link gives for NAME, opened in their own browser"
}

// secretAsk is the ask for a value production lacks, as a step says it.
func (a *app) secretAsk() string {
	if a.remote {
		return "give the user the link canyon_secret_link gives for NAME and env prod, to open in their own browser"
	}
	return "ask the user to run canyon secret set NAME --env prod in their own terminal"
}

// missingStep is the step for the names set for preview only.
func (a *app) missingStep() string { return fmt.Sprintf(aiMissingStep, a.secretAsk()) }

// mcpSecretLink is canyon_secret_link: the app page's Secrets form on one secret, as a link the person opens in their
// own browser, which takes the value there; the app is read first, so a link is never handed for one the caller can't see.
func (a *app) mcpSecretLink(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			App, Name, Env string
		}
		if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
			return toolRefused(err), nil
		}
		if !names.ValidProject(args.App) || len(args.App) > maxProjectName {
			return toolRefused(fmt.Errorf("app %q is not a Canyon app id", args.App)), nil
		}
		c := a.sub(ctx, "")
		c.named = args.App
		if err := c.http.get(ctx, "/api/v1/projects/"+url.PathEscape(args.App), &struct{}{}); err != nil {
			return toolRefused(c.refusal(err)), nil
		}
		link := c.http.page() + "/#/apps/" + url.PathEscape(args.App) + "/secrets?" + url.Values{"name": {args.Name}, "env": {args.Env}}.Encode()
		where := map[string]string{"preview": "preview", "prod": "production"}[args.Env]
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(secretLinkLine, link, args.Name, where)}}}, nil
	}
}

// secretLinkLine is canyon_secret_link's answer (design/EXPERIENCE.md, D5): a link and a name, never a value.
const secretLinkLine = "Give the user this link: %s\nIt opens the app's Secrets form on %s for %s in their own browser, where they type the value. Never ask for the value in this conversation." //nolint:gosec // G101: words about a secret, none in them

// remoteSaid is what a block the CLI wrote for an agent says on /mcp: each CLI command named as the tool that does it
// there, so a remote agent, with no terminal, is never sent to one. The CLI's own text is untouched.
func remoteSaid(text string) string { return remoteWords.Replace(text) }

// remoteWords names each CLI command an agent's block may carry as /mcp's tool for it. A command a block gains that
// has no entry here fails TestMCPRemoteBlocksNameToolsNeverCommands over the goldens.
// ponytail: exact phrases of the CLI's own blocks, longest first; a renderer that takes the server as an argument is
// the upgrade if the table outgrows a screen.
var remoteWords = func() *strings.Replacer {
	tools := [][2]string{
		{"canyon deploy --env prod --version ", "canyon_promote with version "},
		{"canyon deploy --env prod --yes", "canyon_promote with confirm"},
		{"canyon deploy --env prod", "canyon_promote"},
		{"canyon deploy", "canyon_deploy_ref"},
		{"canyon check", "canyon_check_ref"},
		{"canyon status", "canyon_status"},
		{"canyon logs for that environment (--env preview or --env prod)", "canyon_logs for that environment (env preview or prod)"},
		{"canyon logs --env prod", "canyon_logs with env prod"},
		{"canyon logs --env preview", "canyon_logs with env preview"},
		{"canyon logs", "canyon_logs"},
		{"canyon playbook ", "canyon_playbook with slug "},
		{"canyon secret set NAME in their own terminal", "the link canyon_secret_link gives for NAME, opened in their own browser"},
		{"canyon secret set NAME in their own\nterminal", "the link canyon_secret_link gives for NAME, opened in their own\nbrowser"},
		{"canyon secret set ", "the link canyon_secret_link gives for "},
		{"canyon secret set", "the link canyon_secret_link gives"},
		{"canyon share <email> · canyon share everyone", "on its page in Canyon"},
		{"canyon login", "connecting Canyon again"},
	}
	flags := []string{" --yes", " and confirm"} // a flag a command above carried, where no command is left to carry it
	var pairs []string
	for _, verb := range [][2]string{{"Run: ", "Call "}, {"run: ", "call "}, {"Run ", "Call "}, {"run ", "call "}, {"", ""}} {
		for _, t := range tools {
			pairs = append(pairs, verb[0]+t[0], verb[1]+t[1])
		}
	}
	return strings.NewReplacer(append(pairs, flags...)...)
}()

// remotely is h with every text it answers said as /mcp says it (remoteSaid).
func remotely(h mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := h(ctx, req)
		if res != nil {
			for _, c := range res.Content {
				if text, ok := c.(*mcp.TextContent); ok {
					text.Text = remoteSaid(text.Text)
				}
			}
		}
		return res, err
	}
}
