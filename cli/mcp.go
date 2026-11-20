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
	s := mcp.NewServer(&mcp.Implementation{Name: "canyon", Title: "Canyon", Version: Version}, &mcp.ServerOptions{Instructions: a.mcpInstructions(ctx)})
	for _, t := range append(mcptools.Tools(), mcptools.LocalOnly()...) {
		if t.Name == mcptools.Playbook { // a playbook is the org's: no app, no dir
			s.AddTool(t, a.mcpPlaybook(t))
			continue
		}
		t := mcptools.Local(t)
		s.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct { // every tool's, each schema taking its own
				App, Env, Version, Reason, Dir string
				Lines                          int
				Confirm                        bool
			}
			if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
				return toolRefused(err), nil
			}
			dir, err := a.toolDir(args.Dir)
			if err != nil {
				return toolRefused(err), nil
			}
			switch t.Name {
			case mcptools.Rules:
				return a.mcpRules(ctx, dir, args.App), nil
			case mcptools.Status:
				out, err := a.verbIn(ctx, dir, args.App, command{name: "status", json: true, run: runStatus}, "--format", "json")
				if err != nil {
					return toolRefused(err), nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}, StructuredContent: json.RawMessage(out)}, nil
			case mcptools.Check:
				return a.mcpVerdict(ctx, req, dir, command{name: "check", run: runCheck}), nil
			case mcptools.Deploy:
				verb := []string{}
				if args.App != "" {
					verb = append(verb, "--app", args.App)
				}
				return a.mcpVerdict(ctx, req, dir, command{name: "deploy", run: runDeploy}, verb...), nil
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
		})
	}
	s.AddResource(&mcp.Resource{URI: mcptools.ManifestURI, Name: "manifest", Title: "Canyon's rules", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.ManifestAppTemplate, Name: "manifest-app", Title: "Canyon's rules for one app", MIMEType: "application/json"}, a.manifestResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: mcptools.PlaybookTemplate, Name: "playbook", Title: "One of the org's playbooks", MIMEType: "text/markdown"}, a.playbookResource)
	return s
}

// mcpPlaybook is canyon_playbook: a playbook's body as its admin wrote it, counted as a read by the tool, and the
// playbook whole as structured content.
func (a *app) mcpPlaybook(t *mcp.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args mcptools.PlaybookArgs
		if err := mcptools.Decode(t, req.Params.Arguments, &args); err != nil {
			return toolRefused(err), nil
		}
		p, raw, err := a.readPlaybook(ctx, args.Slug, "tool")
		if err != nil {
			return toolRefused(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: p.Body}}, StructuredContent: raw}, nil
	}
}

// playbookResource reads canyon://playbooks/<slug>: the same body, counted as a read by the resource.
func (a *app) playbookResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	slug, _ := strings.CutPrefix(req.Params.URI, mcptools.PlaybookURI)
	p, _, err := a.readPlaybook(ctx, slug, "resource")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: p.Body}}}, nil
}

// readPlaybook is GET /playbooks/:slug counted as way, or its refusal as the CLI says it: a slug that names none is
// canyon playbook's own line.
func (a *app) readPlaybook(ctx context.Context, slug, way string) (playbookRead, json.RawMessage, error) {
	c := a.sub(ctx, "")
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

// mcpRules is canyon_rules: the typed manifest, and the agent's rendering of it as text.
func (a *app) mcpRules(ctx context.Context, dir, named string) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	if named == "" {
		named = c.dirApp()
	}
	m, raw, err := c.readManifest(ctx, named, requestTimeout)
	if err != nil {
		return toolRefused(c.refusal(err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: agentManifest(m, false)}}, StructuredContent: raw}
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
	m.App, m.Outbound = named, viaCanyon(ctx, client, within-a.clock.now().Sub(began))
	return m, raw, nil
}

// sub is one tool call's invocation: this server's environment and host, its own output, the tool's
// directory, within the call.
func (a *app) sub(ctx context.Context, dir string) *app {
	var out, errs strings.Builder
	render, _ := resolveRender(string(FormatAI), a.getenv, false) // ai is a format resolveRender always takes
	return &app{out: &out, err: &errs, in: strings.NewReader(""), getenv: a.getenv, clock: a.clock, render: render, hostFlag: a.hostFlag, ctx: ctx, wd: dir, tools: true}
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

// mcpVerdict is canyon_check and canyon_deploy: the verb under ai, its block as text, and the verdict typed,
// with the run's SARIF log as the host serves it. A run followed past the hand-off is state running.
func (a *app) mcpVerdict(ctx context.Context, req *mcp.CallToolRequest, dir string, verb command, args ...string) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	c.handoff, c.onRow = mcptools.Handoff, progress(ctx, req)
	code := c.run(verb, append([]string{"--format", "ai"}, args...)...)
	res := &mcp.CallToolResult{IsError: code == exitError, Content: []mcp.Content{&mcp.TextContent{Text: c.said()}}}
	run := c.ended
	if run == nil { // refused before any verdict: the verb's lines alone
		return res
	}
	// Every string as the terminal shows it (cell): a host's or an app's control bytes never reach the client raw.
	// The SARIF log is the host's JSON byte for byte, as the contract hands it on, and JSON escapes them.
	v := mcptools.Verdict{Run: cell(run.Run), State: cell(run.Result.State), FailureReason: cell(run.Result.FailureReason), Blocking: run.Result.Blocking, Warnings: run.Result.Warnings}
	switch {
	case code == exitStillRunning:
		v.State, v.FleetURL = "running", cell(run.Host+"/#/apps/"+run.App)
	case v.State == "clean" || v.State == "blocked":
		if err := c.http.getUpTo(ctx, "/api/v1/gate-runs/"+url.PathEscape(run.Run)+"/sarif", timelineLimit, &v.Findings); err != nil {
			res.IsError = true // a verdict without its findings is never handed on as one
			res.Content = append(res.Content, &mcp.TextContent{Text: c.sub(ctx, dir).refusal(err).Error()})
			return res
		}
	}
	if d := run.Deploy; d != nil {
		v.URL = cell(d.URL)
		for _, line := range run.Access {
			v.Access = append(v.Access, cell(line))
		}
		if w := d.Answered; w != nil {
			v.Answered, _ = json.Marshal(answered{Status: w.Status, Said: cell(w.Said), Error: cell(w.Error)}) // a struct of strings: it marshals
		}
	}
	res.StructuredContent = v
	return res
}

// mcpPromote is canyon_promote: canyon deploy --env prod under ai, with --yes only where confirm says the
// person asked for production. A client that can ask the person is asked the CLI's question, where the verb
// would ask it, as an input request (SEP-2322): this call ends before anything is sent, and the client calls
// again with the answer, which the verb, reading the receipt again, takes as the person's.
func (a *app) mcpPromote(ctx context.Context, req *mcp.CallToolRequest, dir, named, version, reason string, confirm bool) *mcp.CallToolResult {
	c := a.sub(ctx, dir)
	c.named, c.handoff = named, mcptools.Handoff
	args := []string{"--format", "ai", "--env", "prod"}
	if version != "" {
		args = append(args, "--version", version)
	}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	var wanted, id string
	if caps := req.Session.InitializeParams().Capabilities; caps != nil && caps.Elicitation != nil { // a client that can ask the person always does: confirm is the agent's word, not theirs
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
