package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
	"github.com/getcanyon-ai/canyon/internal/names"
)

// runMCP serves Canyon's tools to a coding agent over MCP on stdin and stdout (design/specs/mcp.md §1),
// signed in as every other verb is. Each tool runs the verb it stands for, as `canyon <verb>` would in
// the tool's directory, so its checks, refusals and lines are the CLI's own. It never signs in: with
// no session, every tool answers the CLI's not-signed-in line.
func runMCP(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("mcp takes no arguments, and got %q", rest[0]))
	}
	err = a.mcpServer(ctx).Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(a.in), Writer: nopCloser{a.out}})
	if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		a.errf("\n  %s canyon mcp ended: %s\n", a.render.glyph(stateFail), err)
		return exitError
	}
	return exitOK
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// mcpServer is the local server: the read tools and resources, and instructions read once, now.
func (a *app) mcpServer(ctx context.Context) *mcp.Server {
	a.transport = transportMCPLocal // every request it makes says so, a tool call's too (design/specs/mcp.md §7)
	s := mcp.NewServer(&mcp.Implementation{Name: "canyon", Title: "Canyon", Version: Version}, &mcp.ServerOptions{Instructions: a.mcpInstructions(ctx)})
	for _, t := range append(mcptools.Tools(), mcptools.LocalOnly()...) {
		if t.Name == mcptools.Playbook { // a playbook is the org's: no app, no dir
			s.AddTool(t, a.mcpPlaybook(t))
			continue
		}
		t := mcptools.Local(t)
		s.AddTool(t, a.mcpTool(t))
	}
	s.AddResource(&mcp.Resource{URI: mcptools.ManifestURI, Name: "manifest", Title: "Canyon's rules", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.ManifestAppTemplate, Name: "manifest-app", Title: "Canyon's rules for one app", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.PlaybookTemplate, Name: "playbook", Title: "One of the org's playbooks", MIMEType: "text/markdown"}, a.playbookResource)
	s.AddPrompt(&mcp.Prompt{Name: tourPrompt, Title: "Canyon's guided tour", Description: "A chapter of the guided tour on the laptop demo, as canyon demo tour gives it to an agent: 1, 2, 3 or end, the visitor's own chapter when left out.",
		Arguments: []*mcp.PromptArgument{{Name: "chapter", Description: "1, 2, 3 or end; left out, the chapter the visitor is on"}}}, a.tourPromptChapter)
	return s
}

// mcpTool is a tool's handler on either server: the verb it stands for, in the tool's directory locally, and on
// /mcp for the app it names, which there is always given.
func (a *app) mcpTool(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct { // every tool's, each schema taking its own
			App, Env, Version, Reason, Dir, Ref, Run string
			Lines                                    int
			Confirm                                  bool
		}
		if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
			return toolRefused(err), nil
		}
		dir := ""
		if a.remote { // no directory names the app on /mcp: the tool does, as an app id, but canyon_rules may name none
			if (args.App != "" || t.Name != mcptools.Rules) && (!names.ValidProject(args.App) || len(args.App) > maxProjectName) {
				return toolRefused(fmt.Errorf("app %q is not a Canyon app id: lowercase letters, digits and dashes, beginning with a letter, at most %d characters", args.App, maxProjectName)), nil
			}
		} else {
			var err error
			if dir, err = a.toolDir(args.Dir); err != nil {
				return toolRefused(err), nil
			}
		}
		switch t.Name {
		case mcptools.Rules:
			return a.mcpRules(ctx, dir, args.App), nil
		case mcptools.Status:
			out, err := a.verbIn(ctx, dir, args.App, command{name: "status", json: true, run: runStatus}, "--format", "json")
			if err != nil {
				return toolRefused(err), nil
			}
			text := out
			if appSpoke(out) { // the JSON keeps the host's shape: the first line says which of its words are the app's
				text = statusAppSays + "\n" + out
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil // text alone: mcpVerdict says why
		case mcptools.Check:
			return a.mcpVerdict(ctx, req, dir, "", "", command{name: "check", run: runCheck}), nil
		case mcptools.Deploy:
			return a.mcpVerdict(ctx, req, dir, "", "", command{name: "deploy", run: runDeploy}), nil // canyon deploy writes the app's id, never the agent
		case mcptools.CheckRef:
			return a.mcpVerdict(ctx, req, "", args.App, args.Ref, command{name: "check", run: runCheck}), nil
		case mcptools.Review:
			return a.mcpReview(ctx, args.App, args.Run), nil
		case mcptools.DeployRef:
			return a.mcpVerdict(ctx, req, "", args.App, args.Ref, command{name: "deploy", run: runDeploy}), nil
		case mcptools.Promote:
			return a.mcpPromote(ctx, req, dir, args.App, args.Version, args.Reason, args.Confirm), nil
		}
		verb := []string{"--format", "ai", "--redaction", "masked", "--env", args.Env, "--lines", strconv.Itoa(args.Lines)}
		if args.App != "" {
			verb = append(verb, "--app", args.App)
		}
		out, err := a.verbIn(ctx, dir, "", command{name: "logs", run: runLogs}, verb...)
		if err != nil {
			return toolRefused(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil
	}
}

// tourPrompt is canyon_tour: each chapter of canyon demo tour as an MCP prompt (design/specs/mcp.md §11).
const tourPrompt = "canyon_tour"

var tourChapters = map[string]int{"1": tourFirst, "2": tourSecurity, "3": tourProd, "end": tourEnd}

// tourPromptChapter is the chapter's instructions, word for word as canyon demo tour prints them for an agent. It
// moves no place: plant, next, skip and end stay the tour's own forms.
func (a *app) tourPromptChapter(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	named := req.Params.Arguments["chapter"]
	chapter, ok := tourChapters[named]
	if named == "" {
		state, err := loadTour(a.getenv)
		if err != nil {
			return nil, err
		}
		chapter, ok = state.Chapter, true
	}
	if !ok {
		return nil, fmt.Errorf("the tour has no chapter %q: it has 1, 2, 3 and end", named)
	}
	return &mcp.GetPromptResult{Description: tourTitles[chapter], Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: tourAgent[chapter]}}}}, nil
}

// mcpPlaybook is canyon_playbook: a playbook's body as its admin wrote it, each character that hides text written
// visibly (visible), counted as a read by the tool, and the playbook whole as structured content.
func (a *app) mcpPlaybook(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args mcptools.PlaybookArgs
		if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
			return toolRefused(err), nil
		}
		p, _, err := a.readPlaybook(ctx, args.Slug, "tool")
		if err != nil {
			return toolRefused(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: visible(p.Body)}}}, nil // text alone: mcpVerdict says why
	}
}

// playbookResource reads canyon://playbooks/<slug>: the same body, counted as a read by the resource.
func (a *app) playbookResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	slug, _ := strings.CutPrefix(req.Params.URI, mcptools.PlaybookURI)
	if !playbookSlug(slug) { // the tool's schema holds its slug to this; a resource's URI is the client's to write
		return nil, fmt.Errorf("%s is no playbook's address: canyon://playbooks/<slug>, a slug of a-z, 0-9 and -", cell(req.Params.URI))
	}
	p, _, err := a.readPlaybook(ctx, slug, "resource")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: visible(p.Body)}}}, nil
}

// readPlaybook is GET /playbooks/:slug counted as way, or its refusal as the CLI says it: a slug that names none is
// canyon playbook's own line.
func (a *app) readPlaybook(ctx context.Context, slug, way string) (playbookRead, json.RawMessage, error) {
	c := a.sub(ctx, "")
	c.verb, c.args = command{name: "playbook"}, []string{"playbook", slug} // a refusal's line to run again is canyon playbook's
	client, err := c.client()
	if err != nil {
		return playbookRead{}, nil, c.refusal(err)
	}
	p, raw, err := fetchPlaybook(ctx, client, slug, way)
	var api *APIError
	switch {
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return p, nil, fmt.Errorf(noPlaybook, cell(slug)) //nolint:staticcheck // D5's line, said whole as the tool's answer
	case err != nil:
		return p, nil, c.refusal(err)
	}
	c.remember()
	return p, raw, nil
}

// mcpInstructions are the preamble and the manifest as the session hook hands it, for the app the
// server's directory names; where the read fails, the hook's line for it, so the agent is told why.
func (a *app) mcpInstructions(ctx context.Context) string {
	c := a.sub(ctx, "")
	m, _, err := c.readManifest(ctx, c.dirApp(), hookWithin) // a server's start never waits on Canyon, as a session's doesn't
	if err != nil {
		line, _ := hookLine(err)
		return mcptools.Preamble + "\n\n" + line
	}
	return mcptools.Preamble + "\n\n" + agentManifest(m, false)
}

// mcpRules is canyon_rules: the agent's rendering of the manifest, as the session hook hands it.
func (a *app) mcpRules(ctx context.Context, dir, named string) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	if named == "" {
		named = c.dirApp()
	}
	m, _, err := c.readManifest(ctx, named, requestTimeout)
	if err != nil {
		return toolRefused(c.refusal(err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: agentManifest(m, false)}}} // text alone: mcpVerdict says why
}

// manifestResource reads canyon://manifest, the org's, and canyon://manifest/<app>, with that app's additions.
func (a *app) manifestResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI
	named, _ := strings.CutPrefix(uri, mcptools.ManifestURI)
	named = strings.TrimPrefix(named, "/")
	c := a.sub(ctx, "")
	_, raw, err := c.readManifest(ctx, named, requestTimeout)
	if err != nil {
		return nil, c.refusal(err)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(raw)}}}, nil
}

// readManifest is GET /skill/manifest, with named's additions when it names an app, as canyon manifest reads it.
func (a *app) readManifest(ctx context.Context, named string, within time.Duration) (manifest, json.RawMessage, error) {
	a.verb = command{name: "manifest"}
	var m manifest
	at := "/api/v1/skill/manifest"
	if named != "" {
		a.named = named
		if _, err := a.project(); err != nil { // the id canyon.toml would be held to
			return m, nil, err
		}
		at += "?app=" + url.QueryEscape(named)
	}
	client, err := a.client()
	if err != nil {
		return m, nil, err
	}
	var raw json.RawMessage
	began := a.clock.now()
	err = client.do(ctx, http.MethodGet, at, "", nil, 0, nil, http.StatusOK, within, answerLimit, &raw)
	if err == nil && (json.Unmarshal(raw, &m) != nil || m.Host == "") { // as canyon manifest: the one field the contract never leaves empty
		err = &NotCanyon{Host: client.Base, Status: http.StatusOK}
	}
	if err != nil {
		return m, nil, err
	}
	a.remember()
	m.App, m.Outbound, m.Remote = named, viaCanyon(ctx, client, within-a.clock.now().Sub(began)), a.remote
	return m, raw, nil
}

// sub is one tool call's invocation: this server's environment and host, its own output, the tool's
// directory, within the call.
func (a *app) sub(ctx context.Context, dir string) *app {
	var out, errs strings.Builder
	render, _ := resolveRender(string(FormatAI), a.getenv, false) // ai is a format resolveRender always takes
	c := &app{out: &out, err: &errs, in: strings.NewReader(""), getenv: a.getenv, clock: a.clock, render: render, hostFlag: a.hostFlag, ctx: ctx, wd: dir,
		transport: a.transport}
	if a.remote { // on /mcp: the server's client, its own cookie-less copy per call, and still nothing of this machine's
		c.http, c.conf, c.noticed, c.remote, c.wd = remoteClient(a.http.Base, a.http.HTTP.Transport), &config{}, true, true, ""
	}
	return c
}

// appSpoke is whether a status read's JSON carries the app's own words: an environment's answered said or error.
func appSpoke(read string) bool {
	var s statusJSON
	if json.Unmarshal([]byte(read), &s) != nil {
		return true // a read this build can't tell about is said to be the app's: fail closed
	}
	for _, e := range s.Envs {
		if e != nil && e.Answered != nil && (e.Answered.Said != "" || e.Answered.Error != "") {
			return true
		}
	}
	return false
}

// verbIn runs `canyon <verb> <args>` in dir, as the app named when it names one: what it printed, or its refusal.
func (a *app) verbIn(ctx context.Context, dir, named string, verb command, args ...string) (string, error) {
	c := a.sub(ctx, dir)
	c.named = named
	if code := c.run(verb, args...); code != exitOK {
		return "", errors.New(c.said())
	}
	return c.out.(*strings.Builder).String(), nil
}

// run runs `canyon <verb> <args>` as this invocation, its exit code.
func (a *app) run(verb command, args ...string) int {
	a.args, a.verb = append([]string{verb.name}, args...), verb
	code := verb.run(a.ctx, a, args)
	a.remember() // a session it rotated outlives the call that earned it, as main's
	return code
}

// said is everything this invocation printed, stdout then stderr.
func (a *app) said() string {
	return strings.TrimSpace(a.out.(*strings.Builder).String() + a.err.(*strings.Builder).String())
}

// mcpVerdict is canyon_check and canyon_deploy: the verb under ai, its block as the one text content. No tool sends
// structuredContent: Claude Code hands a model a result's structuredContent in place of its text, and the block's NEXT
// STEPS — what to tell the user, when to stop, who approves, what to revoke, the playbook to read, who can open the app —
// are in the text alone (the bed journeys, v0.11.0).
func (a *app) mcpVerdict(ctx context.Context, req *mcp.CallToolRequest, dir, named, ref string, verb command, args ...string) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	c.named, c.ref = named, ref // on /mcp: the app the tool names, and the ref of its linked repository Canyon packages
	c.handoff, c.onRow = handoffOf(a), progress(ctx, req)
	code := c.run(verb, append([]string{"--format", "ai"}, args...)...)
	return &mcp.CallToolResult{IsError: code == exitError, Content: []mcp.Content{&mcp.TextContent{Text: c.said()}}}
}

// mcpPromote is canyon_promote: canyon deploy --env prod under ai, with --yes only where confirm says the
// person asked for production. A client that can ask the person is asked the CLI's question, where the verb
// would ask it, as an input request (SEP-2322): this call ends before anything is sent, and the client calls
// again with the answer, which the verb, reading the receipt again, takes as the person's.
func (a *app) mcpPromote(ctx context.Context, req *mcp.CallToolRequest, dir, named, version, reason string, confirm bool) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	c.named, c.handoff = named, handoffOf(a)
	args := []string{"--format", "ai", "--env", "prod"}
	if version != "" {
		args = append(args, "--version", version)
	}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	var wanted, id string
	if p := req.Session.InitializeParams(); p != nil && p.Capabilities != nil && p.Capabilities.Elicitation != nil { // a client that can ask the person always does: confirm is the agent's word, not theirs
		c.elicit = func(question, ships string) (asked, yes bool) {
			id = promoteAsk(question, ships)
			if answer, answered := req.Params.InputResponses[id].(*mcp.ElicitResult); answered { // an answer to this question about this release, and no other
				return true, answer.Action == "accept"
			}
			wanted = question
			return false, false
		}
	} else if confirm {
		args = append(args, "--yes")
	}
	code := c.run(command{name: "deploy", run: runDeploy}, args...)
	if wanted != "" { // nothing was sent: the person is asked, and the client calls again with the answer
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{id: &mcp.ElicitParams{Message: wanted,
			RequestedSchema: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}}}}
	}
	return &mcp.CallToolResult{IsError: code == exitError && !c.declined, Content: []mcp.Content{&mcp.TextContent{Text: c.said()}}}
}

// promoteAsk is production's question's id among a call's input requests: a digest of the question, which names
// the app and the version, and of what ships under that label. The client answers under it, and a retry takes only
// the answer to the question it would ask now: where preview moved between the two, the person is asked again.
func promoteAsk(question, ships string) string {
	sum := sha256.Sum256([]byte(question + "\x00" + ships))
	return "promote-" + hex.EncodeToString(sum[:12])
}

// progress is a tool call's progress notifications, one per progress row of the run it follows; nil where
// the client asked for none.
func progress(ctx context.Context, req *mcp.CallToolRequest) func(timelineRow) {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	n := 0
	return func(row timelineRow) {
		var p struct{ Step, Tool, Status string }
		if row.Kind != "progress" || json.Unmarshal(row.Data, &p) != nil {
			return
		}
		n++
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(n), Message: cell(strings.TrimSpace(p.Step+" "+p.Tool) + ": " + p.Status)})
	}
}

// refusal is err as the CLI says it: §9's lines, the verb's own where it has some.
func (a *app) refusal(err error) error {
	a.fail(err)
	return errors.New(strings.TrimSpace(a.out.(*strings.Builder).String() + a.err.(*strings.Builder).String()))
}

func toolRefused(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// dirApp is the app canyon.toml names in this invocation's directory, "" where none does: the org's rules alone.
func (a *app) dirApp() string {
	dir, err := a.getwd()
	if err != nil {
		return ""
	}
	p, err := readProject(dir)
	if err != nil {
		return ""
	}
	return p.App
}

// toolDir is a tool's dir, against the server's own directory where it's relative or omitted, links
// resolved. It must be that directory or one under it: an agent that reaches Canyon only through these
// tools can't have canyon_deploy package ~/.ssh or the whole home directory.
func (a *app) toolDir(dir string) (string, error) {
	wd, err := a.getwd()
	if err == nil {
		wd, err = filepath.EvalSymlinks(wd)
	}
	if err != nil {
		return "", err
	}
	if dir == "" {
		return wd, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wd, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("dir %q can't be read: %s", dir, plainly(err))
	}
	if rel, err := filepath.Rel(wd, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dir %q is outside %s, the directory canyon mcp runs in: a tool reads only that directory and the ones under it", dir, wd)
	}
	return resolved, nil
}
