package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// mcpHost is a stub Canyon: the manifest fixture, the status page fixture and a log, each as routes.md
// answers it, with every request it was asked recorded; refuse, when set, answers every route.
type mcpHost struct {
	mu     sync.Mutex
	asked  []string
	refuse func(w http.ResponseWriter)
}

func (h *mcpHost) serve(t *testing.T) string {
	t.Helper()
	manifest, whole := manifestFixture(t), page(t)
	var pages []json.RawMessage
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.asked = append(h.asked, r.Method+" "+r.URL.RequestURI())
		refuse := h.refuse
		h.mu.Unlock()
		if refuse != nil {
			refuse(w)
			return
		}
		switch path := r.URL.Path; {
		case path == "/api/v1/skill/manifest":
			_, _ = w.Write(manifest)
		case path == "/api/v1/egress":
			_, _ = io.WriteString(w, `{"via":"canyon","mode":"observe"}`)
		case path == "/auth/config":
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"oidc"}`)
		case path == "/api/v1/projects/claims-portal":
			_, _ = io.WriteString(w, `{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`)
		case strings.HasSuffix(path, "/gate-runs"):
			_, _ = w.Write(pages[0])
		case strings.HasSuffix(path, "/logs"):
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Canyon-Log-Redaction", "masked")
			_, _ = io.WriteString(w, "listening on :8080\nDATABASE_URL=[DATABASE_URL]\n")
		default:
			_, _ = w.Write(whole["status"])
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (h *mcpHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// mcpSession is a client connected in memory to canyon mcp's server, run in a directory whose
// canyon.toml names claims-portal, against host.
func mcpSession(t *testing.T, host string) (*mcp.ClientSession, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	t.Chdir(dir)
	env := map[string]string{"CANYON_HOST": host, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_ASCII": "1"}
	a := &app{out: io.Discard, err: io.Discard, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, clock: wallClock}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := a.mcpServer(ctx).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, env
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

// The three read tools, each read-only, each with the local server's dir, and the two manifest resources.
func TestMCPListsTheReadToolsAndTheManifestResources(t *testing.T) {
	h := &mcpHost{}
	cs, _ := mcpSession(t, h.serve(t))
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		schema, _ := json.Marshal(tool.InputSchema)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !bytes.Contains(schema, []byte(`"dir"`)) || !bytes.Contains(schema, []byte(`"app"`)) {
			t.Errorf("%s: annotations %+v, input schema %s", tool.Name, tool.Annotations, schema)
		}
	}
	if want := []string{mcptools.Logs, mcptools.Rules, mcptools.Status}; !slices.Equal(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}
	resources, err := cs.ListResources(context.Background(), nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != mcptools.ManifestURI {
		t.Errorf("resources %+v, %v", resources, err)
	}
	templates, err := cs.ListResourceTemplates(context.Background(), nil)
	if err != nil || len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].URITemplate != mcptools.ManifestAppTemplate {
		t.Errorf("templates %+v, %v", templates, err)
	}
}

// canyon mcp speaks MCP on its stdin and stdout, and ends when the client closes them.
func TestMCPServesOverStdio(t *testing.T) {
	h := &mcpHost{}
	env := map[string]string{"CANYON_HOST": h.serve(t), "XDG_CONFIG_HOME": t.TempDir()}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int)
	go func() {
		var errs strings.Builder
		done <- Main([]string{"mcp"}, outW, &errs, inR, func(k string) string { return env[k] }, false)
		_ = outW.Close()
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, text := callTool(t, cs, mcptools.Rules, nil); res.IsError {
		t.Errorf("canyon_rules over stdio: %q", text)
	}
	_ = cs.Close()
	if code := <-done; code != exitOK {
		t.Errorf("canyon mcp exited %d once the client closed", code)
	}
}

// The instructions are the preamble, then the manifest as canyon manifest writes it for an agent.
func TestMCPInstructionsAreThePreambleAndTheManifest(t *testing.T) {
	registry, err := os.ReadFile("../design/EXPERIENCE.md") // read before the directory moves
	if err != nil || !bytes.Contains(registry, []byte("| `"+mcptools.Preamble+"` |")) {
		t.Errorf("the preamble is not D5's string byte for byte: %v", err)
	}
	h := &mcpHost{}
	host := h.serve(t)
	cs, env := mcpSession(t, host)
	manifest, _, code := run(t, env, false, "manifest", "--format", "ai")
	if code != exitOK {
		t.Fatalf("canyon manifest exited %d", code)
	}
	if got, want := cs.InitializeResult().Instructions, mcptools.Preamble+"\n\n"+manifest; got != want {
		t.Errorf("instructions:\n%s\nwant:\n%s", got, want)
	}
}

// canyon_rules is the host's manifest, byte for byte, as structured content, and the agent's text as text;
// for the app the directory names, or the one it's given.
func TestMCPRulesIsTheManifestTypedAndAsText(t *testing.T) {
	h, fixture := &mcpHost{}, manifestFixture(t) // read before the directory moves
	cs, env := mcpSession(t, h.serve(t))
	manifest, _, _ := run(t, env, false, "manifest", "--format", "ai")
	res, text := callTool(t, cs, mcptools.Rules, nil)
	structured, _ := json.Marshal(res.StructuredContent)
	if res.IsError || text != manifest || !sameJSON(structured, fixture) {
		t.Errorf("isError %v, text:\n%s\nstructured %s", res.IsError, text, structured)
	}
	var tool *mcp.Tool
	for _, each := range mcptools.Tools() {
		if each.Name == mcptools.Rules {
			tool = each
		}
	}
	schema, _ := json.Marshal(tool.OutputSchema)
	var output jsonschema.Schema
	if err := json.Unmarshal(schema, &output); err != nil {
		t.Fatal(err)
	}
	resolved, err := output.Resolve(nil)
	var got any
	_ = json.Unmarshal(structured, &got)
	if err != nil || resolved.Validate(got) != nil {
		t.Errorf("the manifest doesn't pass canyon_rules's outputSchema: %v, %v", err, resolved.Validate(got))
	}
	callTool(t, cs, mcptools.Rules, map[string]any{"app": "billing"})
	if asked := h.requests(); !slices.Contains(asked, "GET /api/v1/skill/manifest?app=claims-portal") || !slices.Contains(asked, "GET /api/v1/skill/manifest?app=billing") {
		t.Errorf("asked %v", asked)
	}
	res, text = callTool(t, cs, mcptools.Rules, map[string]any{"app": "Not An Id"})
	if !res.IsError || !strings.Contains(text, "is not a Canyon app id") {
		t.Errorf("a bad app id: isError %v, %q", res.IsError, text)
	}
	contents, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: mcptools.ManifestURI + "/billing"})
	if err != nil || len(contents.Contents) != 1 || !sameJSON([]byte(contents.Contents[0].Text), fixture) {
		t.Errorf("the app's resource: %+v, %v", contents, err)
	}
}

// canyon_status is canyon status --format json, from the tool's directory.
func TestMCPStatusIsCanyonStatusAsJSON(t *testing.T) {
	h := &mcpHost{}
	cs, env := mcpSession(t, h.serve(t))
	want, _, code := run(t, env, false, "status", "--format", "json")
	if code != exitOK {
		t.Fatalf("canyon status exited %d", code)
	}
	res, text := callTool(t, cs, mcptools.Status, nil)
	structured, _ := json.Marshal(res.StructuredContent)
	if res.IsError || text != want || !sameJSON(structured, []byte(want)) {
		t.Errorf("isError %v, text %s, structured %s, want %s", res.IsError, text, structured, want)
	}
	res, text = callTool(t, cs, mcptools.Status, map[string]any{"dir": t.TempDir()})
	if !res.IsError || !strings.Contains(text, projectFile) {
		t.Errorf("a directory with no canyon.toml: isError %v, %q", res.IsError, text)
	}
}

// canyon_logs asks for the masked read and never follows; what the schema doesn't take is refused before any request.
func TestMCPLogsAsksForTheMaskedReadAndNothingElse(t *testing.T) {
	h := &mcpHost{}
	cs, _ := mcpSession(t, h.serve(t))
	res, text := callTool(t, cs, mcptools.Logs, map[string]any{"env": "prod", "lines": 50})
	if res.IsError || !strings.Contains(text, "DATABASE_URL=[DATABASE_URL]") {
		t.Errorf("isError %v, %q", res.IsError, text)
	}
	if asked := h.requests(); !slices.Contains(asked, "GET /api/v1/projects/claims-portal/logs?env=prod&follow=false&lines=50&redaction=masked") {
		t.Errorf("asked %v", asked)
	}
	before := len(h.requests())
	for _, args := range []map[string]any{{"lines": 201}, {"env": "staging"}, {"follow": true}} {
		if res, text := callTool(t, cs, mcptools.Logs, args); !res.IsError {
			t.Errorf("%v: %q", args, text)
		}
	}
	if after := h.requests(); len(after) != before {
		t.Errorf("a refused call asked %v", after[before:])
	}
}

// With no session, every tool answers the CLI's not-signed-in line and the instructions the hook's.
func TestMCPNotSignedInIsTheCLIsLoginLine(t *testing.T) {
	h := &mcpHost{refuse: func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }}
	host := h.serve(t)
	cs, _ := mcpSession(t, host)
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, "this machine is not signed in to "+host) {
		t.Errorf("instructions %q", got)
	}
	for _, name := range []string{mcptools.Rules, mcptools.Status, mcptools.Logs} {
		res, text := callTool(t, cs, name, nil)
		if !res.IsError || !strings.Contains(text, "You're not signed in to "+host) || !strings.Contains(text, "Run: canyon login "+host) {
			t.Errorf("%s: isError %v, %q", name, res.IsError, text)
		}
	}
}

// A refusal is the host's own line, as the CLI prints it.
func TestMCPARefusalIsTheCLIsLine(t *testing.T) {
	h := &mcpHost{refuse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden","message":"Only the app's team reads its logs."}`)
	}}
	cs, _ := mcpSession(t, h.serve(t))
	if res, text := callTool(t, cs, mcptools.Logs, nil); !res.IsError || !strings.Contains(text, "Only the app's team reads its logs.") {
		t.Errorf("isError %v, %q", res.IsError, text)
	}
}

// sameJSON is whether a and b hold the same JSON value, whatever their spacing and key order.
func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && jsonEqual(x, y)
}
