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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// mcpHost is a stub Canyon: the manifest fixture, the status page fixture and a log, each as routes.md
// answers it, with every request it was asked recorded; refuse, when set, answers every route.
type mcpHost struct {
	mu         sync.Mutex
	asked      []string
	transports []string // each request's Canyon-Transport, in order
	refuse     func(w http.ResponseWriter)
	playbook   []byte // GET /api/v1/playbooks/service-conventions's answer; the contract's golden when nil
}

func (h *mcpHost) serve(t *testing.T) string {
	t.Helper()
	manifest, whole, playbook := manifestFixture(t), page(t), h.playbook
	if playbook == nil {
		playbook = playbookFixture(t)
	}
	var pages []json.RawMessage
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.asked = append(h.asked, r.Method+" "+r.URL.RequestURI())
		h.transports = append(h.transports, r.Header.Get("Canyon-Transport"))
		refuse := h.refuse
		h.mu.Unlock()
		if refuse != nil {
			refuse(w)
			return
		}
		switch path := r.URL.Path; {
		case path == "/api/v1/skill/manifest":
			_, _ = w.Write(manifest)
		case path == "/api/v1/playbooks/service-conventions":
			_, _ = w.Write(playbook)
		case strings.HasPrefix(path, "/api/v1/playbooks/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","message":"no playbook by that slug"}`)
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
	return mcpConnect(t, host, wallClock, nil)
}

// mcpConnect is a client with opts connected in memory to canyon mcp's server, run in the test's directory
// against host on clk.
func mcpConnect(t *testing.T, host string, clk clock, opts *mcp.ClientOptions) (*mcp.ClientSession, map[string]string) {
	t.Helper()
	env := map[string]string{"CANYON_HOST": host, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_ASCII": "1"}
	a := &app{out: io.Discard, err: io.Discard, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, clock: clk}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := a.mcpServer(ctx).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, opts).Connect(ctx, ct, nil)
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

// The seven tools: canyon_playbook by a slug alone, the org's, and the rest each with the local server's dir; the
// four reads read-only and the three that act not, promotion's alone open-world; the two manifest resources, and a
// playbook's.
func TestMCPListsTheToolsAndTheManifestResources(t *testing.T) {
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
		reads := tool.Name == mcptools.Rules || tool.Name == mcptools.Status || tool.Name == mcptools.Logs || tool.Name == mcptools.Playbook
		hints := tool.Annotations
		if hints == nil || hints.ReadOnlyHint != reads || bytes.Contains(schema, []byte(`"dir"`)) == (tool.Name == mcptools.Playbook) || // a playbook is the org's: no dir
			!reads && (hints.DestructiveHint == nil || *hints.DestructiveHint != (tool.Name == mcptools.Promote) || hints.IdempotentHint ||
				hints.OpenWorldHint == nil || *hints.OpenWorldHint != (tool.Name == mcptools.Promote)) { // promotion alone replaces what customers reach
			t.Errorf("%s: annotations %+v, input schema %s", tool.Name, hints, schema)
		}
		if tool.OutputSchema != nil { // a client that held a result to it would refuse a newer host's answer (design/specs/mcp.md §1.1)
			t.Errorf("%s declares an outputSchema", tool.Name)
		}
	}
	if want := []string{mcptools.Check, mcptools.Deploy, mcptools.Logs, mcptools.Playbook, mcptools.Promote, mcptools.Rules, mcptools.Status}; !slices.Equal(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}
	resources, err := cs.ListResources(context.Background(), nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != mcptools.ManifestURI {
		t.Errorf("resources %+v, %v", resources, err)
	}
	templates, err := cs.ListResourceTemplates(context.Background(), nil)
	if err != nil || len(templates.ResourceTemplates) != 2 || templates.ResourceTemplates[0].URITemplate != mcptools.ManifestAppTemplate ||
		templates.ResourceTemplates[1].URITemplate != mcptools.PlaybookTemplate {
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
	must(t, os.Mkdir("empty", 0o750))
	res, text = callTool(t, cs, mcptools.Status, map[string]any{"dir": "empty"})
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

// checkIn is a check's or a deploy's stub host, and the client of a server run in projectDir against it.
func checkIn(t *testing.T, h *gateHost, opts *mcp.ClientOptions) (*mcp.ClientSession, string) {
	t.Helper()
	if h.start.code == 0 {
		h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	}
	if h.page == "" {
		h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	cs, _ := mcpConnect(t, server.URL, wallClock, opts)
	return cs, server.URL
}

// canyon_check is canyon check --format ai as text, and the verdict typed: the run's counts and its SARIF log as
// the host serves it, byte for byte. A blocked review is a verdict, not a failed call.
func TestMCPCheckIsCanyonChecksVerdict(t *testing.T) {
	sarif := string(contracts.GoldenLog)
	want, _, code := verbingIn(t, &gateHost{run: aRun("blocked", 1, 1, blocker, advisory), sarif: sarif}, projectDir(t), "check", "--format", "ai")
	if code != exitBlocked {
		t.Fatalf("canyon check exited %d", code)
	}
	h := &gateHost{run: aRun("blocked", 1, 1, blocker, advisory), sarif: sarif}
	cs, host := checkIn(t, h, nil)
	res, text := callTool(t, cs, mcptools.Check, nil)
	text = strings.NewReplacer(host, "http://localhost:7777", strings.TrimPrefix(host, "http://"), "localhost:7777").Replace(text)
	var v mcptools.Verdict
	structured, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(structured, &v); err != nil || res.IsError || v.Run != "gr_8f2a" || v.State != "blocked" || v.Blocking != 1 || v.Warnings != 1 || !sameJSON(v.Findings, []byte(sarif)) {
		t.Errorf("isError %v, structured %s, %v", res.IsError, structured, err)
	}
	if text != strings.TrimSpace(want) {
		t.Errorf("text:\n%s\nwant canyon check's:\n%s", text, want)
	}
	if !slices.Contains(h.asked, "GET /api/v1/gate-runs/gr_8f2a/sarif") {
		t.Errorf("the SARIF log was never read: %v", h.asked)
	}
	for _, tool := range append(mcptools.Tools(), mcptools.LocalOnly()...) {
		if tool.OutputSchema != nil { // a client that held a result to it would refuse a newer host's findings (design/specs/mcp.md §1.1)
			t.Errorf("%s declares an outputSchema", tool.Name)
		}
	}
}

// A deploy that went live carries its url, what the app answered and who can open it; each progress row is a
// progress notification to a client that asked for them.
func TestMCPDeployCarriesTheURLAndTheRunsProgress(t *testing.T) {
	var mu sync.Mutex
	var told []string
	opts := &mcp.ClientOptions{ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, req.Params.Message)
	}}
	page := strings.Replace(buildPage(t, []string{"building"}, true, "private · you are an owner"), `"rows":[`,
		`"rows":[{"id":1,"ts":"2026-09-02T09:31:04Z","kind":"progress","data":{"step":"scan","tool":"trivy","status":"running"}},`, 1)
	h := &gateHost{run: aRun("clean", 0, 0), sarif: string(contracts.GoldenLog), page: page}
	cs, _ := checkIn(t, h, opts)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcptools.Deploy, Meta: mcp.Meta{"progressToken": "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	var v mcptools.Verdict
	structured, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal(structured, &v) != nil || res.IsError || v.State != "clean" || v.URL != liveURL || len(v.Access) != 1 {
		t.Errorf("isError %v, structured %s", res.IsError, structured)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(told, "scan trivy: running") {
		t.Errorf("progress %q", told)
	}
}

// Past the hand-off a run is state running, its id and its page: the tool answers, and the run goes on.
func TestMCPHandsARunOffAfterTenMinutes(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page: `{"rows":[],"next":7}`} // a run that never ends
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	now := time.Now()
	start := now
	var mu sync.Mutex
	clk := clock{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, sleep: func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }}
	cs, _ := mcpConnect(t, server.URL, clk, nil)
	res, text := callTool(t, cs, mcptools.Check, nil)
	var v mcptools.Verdict
	structured, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal(structured, &v) != nil || res.IsError || v.State != "running" || v.Run != "gr_8f2a" || !strings.HasSuffix(v.FleetURL, "/#/apps/claims-portal") || !strings.Contains(text, "after 10 minutes") {
		t.Errorf("isError %v, structured %s, text:\n%s", res.IsError, structured, text)
	}
	if followed := clk.now().Sub(start); followed < mcptools.Handoff || followed > mcptools.Handoff+time.Minute {
		t.Errorf("followed for %s", followed)
	}
}

// canyon_promote sends nothing until the person says yes: through elicitation where the client can ask, with
// confirm where it can't, and never on neither.
func TestMCPPromoteAsksThePersonFirst(t *testing.T) {
	whole := page(t)
	for _, c := range []struct {
		name    string
		elicit  string // the person's answer, "" for a client that can't ask
		confirm bool
		posts   int
		says    string
		isError bool
	}{
		{"asked, yes", "accept", false, 1, "RESULT: PROMOTED", false},
		{"asked, no", "decline", false, 0, "Nothing was promoted.", false},
		{"asked, no, confirm set", "decline", true, 0, "Nothing was promoted.", false}, // the person's answer outranks the agent's confirm
		{"can't ask, no confirm", "", false, 0, "Ask the user whether to put", true},   // the verb's exit 1: the agent asks, and calls again
		{"can't ask, confirm", "", true, 1, "RESULT: PROMOTED", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := clean(t, whole, http.StatusAccepted, started)
			server := h.serve(t)
			t.Chdir(appDir(t))
			h.pages, h.tick = listed(t, whole, "pkg_7a1c", appTree(t)), at
			var asked []string
			var opts *mcp.ClientOptions
			if c.elicit != "" {
				opts = &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					asked = append(asked, req.Params.Message)
					return &mcp.ElicitResult{Action: c.elicit}, nil
				}}
			}
			cs, _ := mcpConnect(t, server.URL, clock{now: func() time.Time { return h.tick }, sleep: func(d time.Duration) { h.tick = h.tick.Add(d) }}, opts)
			res, text := callTool(t, cs, mcptools.Promote, map[string]any{"confirm": c.confirm})
			if len(h.posts) != c.posts || !strings.Contains(text, c.says) || res.IsError != c.isError {
				t.Errorf("posts %q, isError %v, text:\n%s", h.posts, res.IsError, text)
			}
			if c.elicit != "" && !slices.Equal(asked, []string{"Promote claims-portal v1.3 to production?"}) { // D5's, as the terminal asks it
				t.Errorf("asked %q", asked)
			}
		})
	}
}

// A tool's dir is the server's directory or one under it, links resolved: a path outside it, or a link out of
// it, is refused before anything is read or sent, so canyon_deploy can't package a home directory.
func TestMCPDirStaysInsideTheServersDirectory(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), sarif: string(contracts.GoldenLog)}
	cs, _ := checkIn(t, h, nil)
	before := len(h.asked) // the instructions' manifest read
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, projectFile), "app = \"claims-portal\"\n")
	must(t, os.Symlink(outside, "elsewhere"))
	for _, tool := range []string{mcptools.Check, mcptools.Deploy} {
		for _, dir := range []string{outside, "..", "elsewhere"} {
			res, text := callTool(t, cs, tool, map[string]any{"dir": dir})
			if !res.IsError || !strings.Contains(text, "is outside") {
				t.Errorf("%s in %s: isError %v, %q", tool, dir, res.IsError, text)
			}
		}
	}
	if len(h.asked) != before {
		t.Errorf("a refused dir reached the host: %v", h.asked[before:])
	}
	must(t, os.Mkdir("web", 0o750))
	writeFile(t, filepath.Join("web", projectFile), "app = \"claims-portal\"\n")
	if res, text := callTool(t, cs, mcptools.Check, map[string]any{"dir": "web"}); res.IsError {
		t.Errorf("a directory under the server's: %q", text)
	}
}

// The person's answer is to what they were asked: where what preview ships moved between the question and the
// answer, the call asks again rather than promote what nobody said yes to, and an answer under an id the server
// never issued is no answer.
func TestMCPPromoteAsksAgainWhenPreviewMoves(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	server := h.serve(t)
	t.Chdir(appDir(t))
	h.pages, h.tick = listed(t, whole, "pkg_7a1c", appTree(t)), at
	var asked []string
	opts := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked = append(asked, req.Params.Message)
		if len(asked) == 1 { // preview is reviewed again while the person reads the question
			h.status = mutate(t, h.status, func(read map[string]any) { read["promote_run"] = "gr_7777" })
			return &mcp.ElicitResult{Action: "accept"}, nil
		}
		return &mcp.ElicitResult{Action: "decline"}, nil
	}}
	cs, _ := mcpConnect(t, server.URL, clock{now: func() time.Time { return h.tick }, sleep: func(d time.Duration) { h.tick = h.tick.Add(d) }}, opts)
	if res, text := callTool(t, cs, mcptools.Promote, nil); len(h.posts) != 0 || len(asked) != 2 || !strings.Contains(text, "Nothing was promoted.") {
		t.Errorf("posts %q, asked %q, isError %v, text:\n%s", h.posts, asked, res.IsError, text)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcptools.Promote, InputResponses: mcp.InputResponseMap{"promote": &mcp.ElicitResult{Action: "accept"}}})
	if err != nil || len(h.posts) != 0 {
		t.Errorf("an answer under an id never issued: posts %q, %v, %+v", h.posts, err, res)
	}
}

// What a host or an app wrote reaches the client as the terminal shows it: a control byte or an escape sequence in
// a progress row, an access line or what the app answered is spelled out, never sent raw.
func TestMCPHandsOnNoControlByteRaw(t *testing.T) {
	var mu sync.Mutex
	var told []string
	opts := &mcp.ClientOptions{ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, req.Params.Message)
	}}
	page := strings.Replace(buildPage(t, []string{"building"}, true, "private\x1b[2J · you are an owner"), `"rows":[`,
		`"rows":[{"id":1,"ts":"2026-09-02T09:31:04Z","kind":"progress","data":{"step":"scan","tool":"trivy\u001b]0;owned\u0007","status":"running"}},`, 1)
	h := &gateHost{run: aRun("clean", 0, 0), sarif: string(contracts.GoldenLog), page: page}
	cs, _ := checkIn(t, h, opts)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcptools.Deploy, Meta: mcp.Meta{"progressToken": "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	var v mcptools.Verdict
	structured, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal(structured, &v) != nil || len(v.Access) != 1 || strings.ContainsAny(strings.Join(v.Access, ""), "\x1b\x07") {
		t.Errorf("access %q", v.Access)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || strings.ContainsAny(told[0], "\x1b\x07") || !strings.Contains(told[0], `trivy\x1b]0;owned\x07`) {
		t.Errorf("progress %q", told)
	}
}

// Each chapter of the tour is the prompt canyon_tour, word for word as canyon demo tour hands it to an agent; with
// no chapter named, the visitor's own; and a chapter the tour lacks is refused, never answered with another.
func TestMCPServesEachTourChapterAsAPrompt(t *testing.T) {
	h := &mcpHost{}
	cs, env := mcpSession(t, h.serve(t))
	ctx := context.Background()
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "canyon_tour" {
		t.Fatalf("prompts %+v, %v", prompts, err)
	}
	said := func(chapter string) (string, error) {
		res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "canyon_tour", Arguments: map[string]string{"chapter": chapter}})
		if err != nil {
			return "", err
		}
		return res.Messages[0].Content.(*mcp.TextContent).Text, nil
	}
	for chapter, n := range map[string]int{"1": tourFirst, "2": tourSecurity, "3": tourProd, "end": tourEnd} {
		if text, err := said(chapter); err != nil || text != tourAgent[n] {
			t.Errorf("chapter %s: %v\n%s", chapter, err, text)
		}
	}
	path, err := tourPath(func(k string) string { return env[k] })
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(`{"chapter":3}`), 0o600))
	if text, err := said(""); err != nil || text != tourAgent[tourProd] {
		t.Errorf("no chapter named, on chapter 3: %v\n%s", err, text)
	}
	if text, err := said("4"); err == nil || !strings.Contains(err.Error(), `the tour has no chapter "4"`) {
		t.Errorf("chapter 4: %v\n%s", err, text)
	}
}
