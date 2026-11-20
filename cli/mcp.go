package cli

import (
	"context"
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
	for _, t := range mcptools.Tools() {
		t := mcptools.Local(t)
		s.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				mcptools.LogsArgs        // the widest: the rules' and status's are its app alone
				Dir               string `json:"dir"`
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
	return s
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
	return &app{out: &out, err: &errs, in: strings.NewReader(""), getenv: a.getenv, clock: a.clock, render: render, hostFlag: a.hostFlag, ctx: ctx, wd: dir}
}

// verbIn runs `canyon <verb> <args>` in dir, as the app named when it names one: what it printed, or its refusal.
func (a *app) verbIn(ctx context.Context, dir, named string, verb command, args ...string) (string, error) {
	c := a.sub(ctx, dir)
	c.args, c.named, c.verb = append([]string{verb.name}, args...), named, verb
	code := verb.run(ctx, c, args)
	c.remember() // a session it rotated outlives the call that earned it, as main's
	if code != exitOK {
		return "", errors.New(strings.TrimSpace(c.out.(*strings.Builder).String() + c.err.(*strings.Builder).String()))
	}
	return c.out.(*strings.Builder).String(), nil
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

// toolDir is a tool's dir, against the server's own directory where it's relative or omitted.
func (a *app) toolDir(dir string) (string, error) {
	wd, err := a.getwd()
	if err != nil {
		return "", err
	}
	if dir == "" {
		return wd, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wd, dir)
	}
	return filepath.Clean(dir), nil
}
