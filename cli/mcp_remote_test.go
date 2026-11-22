package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const remoteBase = "http://canyon.mcp.invalid"

// remoteConnect is a client connected in memory to canyon-server's /mcp server, whose every request goes to host
// through a round tripper that refuses any but remoteBase, as canyon-server's serves the API alone.
func remoteConnect(t *testing.T, host string, clk clock, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	to, err := url.Parse(host)
	if err != nil {
		t.Fatal(err)
	}
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme+"://"+r.URL.Host != remoteBase || !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			t.Errorf("a tool asked %s", r.URL)
		}
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = to.Scheme, to.Host, ""
		return http.DefaultTransport.RoundTrip(out)
	})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := remoteServer(ctx, rt, remoteBase, "test", clk).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// hostile is a working directory and environment that would steer a tool elsewhere if it read either: a
// canyon.toml naming another app, a .canyon/base naming another package, and CANYON_HOST at another host.
func hostile(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"someone-else\"\n")
	writeFile(t, filepath.Join(dir, ".canyon", "base"), "app = \"someone-else\"\npackage = \"pkg_other\"\n")
	t.Chdir(dir)
	t.Setenv("CANYON_HOST", "http://127.0.0.1:1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
}

// On /mcp, the tools that read a directory are gone, the two that package a linked repository's ref and the one that
// makes an app from a repository are there, no tool takes a dir, and each but the rules', the playbook's and
// canyon_create_app's needs app, as an id: nothing on the server names one for it.
func TestMCPRemoteListsItsToolsWithTheAppRequired(t *testing.T) {
	h := &mcpHost{}
	cs := remoteConnect(t, h.serve(t), wallClock, nil)
	got, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range got.Tools {
		names = append(names, tool.Name)
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, dir := props["dir"]; dir {
			t.Errorf("%s takes a dir on /mcp", tool.Name)
		}
		required, _ := schema["required"].([]any)
		if needs := tool.Name != mcptools.Playbook && tool.Name != mcptools.Rules && tool.Name != mcptools.CreateApp; needs != slices.Contains(required, any("app")) {
			t.Errorf("%s requires %v", tool.Name, schema["required"])
		}
	}
	slices.Sort(names)
	if want := []string{mcptools.CheckRef, mcptools.CreateApp, mcptools.DeployRef, mcptools.Logs, mcptools.Playbook, mcptools.Promote, mcptools.Review, mcptools.Rules, mcptools.SecretLink, mcptools.Status}; !slices.Equal(names, want) {
		t.Errorf("tools %q, want %q", names, want)
	}
}

// A remote tool reads nothing of the machine it runs on: run in a directory and environment that name another app,
// another package and another host, it asks this Canyon about the app it was given and nothing else, and an id that
// isn't one is refused before anything is asked.
func TestMCPRemoteReadsNothingOfThisMachine(t *testing.T) {
	h := &mcpHost{}
	host := h.serve(t) // the fixtures are read before the directory changes
	hostile(t)
	cs := remoteConnect(t, host, wallClock, nil)
	for _, tool := range []string{mcptools.Rules, mcptools.Status, mcptools.Logs} {
		if res, text := callTool(t, cs, tool, map[string]any{"app": "claims-portal"}); res.IsError {
			t.Errorf("%s: %s", tool, text)
		}
	}
	if res, text := callTool(t, cs, mcptools.Rules, map[string]any{}); res.IsError { // the org's rules: no canyon.toml names an app here
		t.Errorf("the org's rules: %s", text)
	}
	for _, asked := range h.requests() {
		if strings.Contains(asked, "someone-else") || strings.Contains(asked, "pkg_other") {
			t.Errorf("a tool read the machine's directory: it asked %s", asked)
		}
	}
	before := len(h.requests())
	for _, app := range []string{"../etc", "Claims", "claims portal", strings.Repeat("a", 200)} {
		if res, _ := callTool(t, cs, mcptools.Status, map[string]any{"app": app}); !res.IsError {
			t.Errorf("app %q was taken", app)
		}
	}
	if after := h.requests(); len(after) != before {
		t.Errorf("a refused app id was asked about: %q", after[before:])
	}
	a := remoteApp(context.Background(), remoteClient(remoteBase, nil))
	if _, err := a.getwd(); err == nil {
		t.Error("getwd answered a directory on /mcp")
	}
	if c := a.sub(context.Background(), "/etc"); !c.remote || c.wd != "" || c.getenv("HOME") != "" || c.config().Host != "" {
		t.Errorf("a tool's invocation on /mcp: remote %v, wd %q, HOME %q, config %+v", c.remote, c.wd, c.getenv("HOME"), c.config())
	}
}

// canyon_promote on /mcp is the local one: the person is asked the CLI's question, bound to what ships, and
// production changes only on their yes; with no directory, nothing compares a checkout to preview.
func TestMCPRemotePromoteAsksThePersonFirst(t *testing.T) {
	whole := page(t)
	hosts := map[string]*promoteHost{}
	servers := map[string]string{}
	for _, answer := range []string{"accept", "decline", "cancel"} { // the fixtures are read before the directory changes
		hosts[answer] = clean(t, whole, http.StatusAccepted, started)
		servers[answer] = hosts[answer].serve(t).URL
		hosts[answer].pages, hosts[answer].tick = listed(t, whole, "pkg_7a1c", "not-this-directory"), at
	}
	hostile(t)
	for _, answer := range []string{"accept", "decline", "cancel"} {
		h, server := hosts[answer], servers[answer]
		var asked []string
		opts := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked = append(asked, req.Params.Message)
			return &mcp.ElicitResult{Action: answer}, nil
		}}
		cs := remoteConnect(t, server, clock{now: func() time.Time { return h.tick }, sleep: func(d time.Duration) { h.tick = h.tick.Add(d) }}, opts)
		_, text := callTool(t, cs, mcptools.Promote, map[string]any{"app": "claims-portal"})
		wantPosts := map[string]int{"accept": 1, "decline": 0, "cancel": 0}[answer]
		if len(h.posts) != wantPosts || !slices.Equal(asked, []string{"Promote claims-portal v1.3 to production?"}) {
			t.Errorf("%s: posts %q, asked %q, text:\n%s", answer, h.posts, asked, text)
		}
		why := map[string]string{"decline": "Nothing was promoted: the person said no.", "cancel": "Nothing was promoted: nobody answered the question."}[answer]
		if why != "" && (!strings.Contains(text, why) || !strings.Contains(text, "a person opens "+remoteBase+"/#/apps/claims-portal") || cliCommand.MatchString(text) ||
			!strings.Contains(text, notAnotherWayRemote)) {
			t.Errorf("%s on /mcp: want %q, the app's page and that the agent doesn't promote it, no CLI command:\n%s", answer, why, text)
		}
	}
}

// /mcp is stateless (design/specs/mcp.md §2.1): every request stands alone, on whichever replica takes it. Production's
// question still binds: the call ends with the question as an input request, and the client's retry, a request of its
// own with no session between the two, carries the answer, which the server takes for this question about this release.
func TestMCPRemotePromoteAnswerLandsWithNoSession(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	server := h.serve(t).URL
	h.pages, h.tick = listed(t, whole, "pkg_7a1c", "not-this-directory"), at
	hostile(t)
	to, _ := url.Parse(server)
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = to.Scheme, to.Host, ""
		return http.DefaultTransport.RoundTrip(out)
	})
	clk := clock{now: func() time.Time { return h.tick }, sleep: func(d time.Duration) { h.tick = h.tick.Add(d) }}
	var posts int
	mcpURL := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("a request carried a session: %s", r.Header.Get("Mcp-Session-Id"))
		}
		posts++
		return remoteServer(r.Context(), rt, remoteBase, "test", clk)
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(mcpURL.Close)
	var asked []string
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked = append(asked, req.Params.Message)
		return &mcp.ElicitResult{Action: "accept"}, nil
	}}).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: mcpURL.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	_, text := callTool(t, cs, mcptools.Promote, map[string]any{"app": "claims-portal"})
	if len(h.posts) != 1 || !slices.Equal(asked, []string{"Promote claims-portal v1.3 to production?"}) || !strings.Contains(text, "RESULT: PROMOTED") {
		t.Errorf("posts %q, asked %q, %d requests, text:\n%s", h.posts, asked, posts, text)
	}
}

// remoteArgs are the arguments a call of each remote tool takes in TestMCPRemoteNeverTouchesTheMachine: a tool added
// without an entry fails the test, so none runs unchecked.
var remoteArgs = map[string]map[string]any{
	mcptools.Rules:      {"app": "claims-portal"},
	mcptools.Status:     {"app": "claims-portal"},
	mcptools.Logs:       {"app": "claims-portal"},
	mcptools.Playbook:   {"slug": "service-conventions"},
	mcptools.Promote:    {"app": "claims-portal", "confirm": true},
	mcptools.CheckRef:   {"app": "claims-portal", "ref": "main"},
	mcptools.DeployRef:  {"app": "claims-portal", "ref": "main"},
	mcptools.CreateApp:  {"repository": "acme/claims-portal"},
	mcptools.Review:     {"app": "claims-portal", "run": "gr_8f2a"},
	mcptools.SecretLink: {"app": "claims-portal", "name": "STRIPE_KEY", "env": "prod"},
}

// Every tool on /mcp answers the same whatever the machine it runs on holds: run once in a plain directory with a plain
// environment, then in a directory nothing can read whose canyon.toml and .canyon/base name another app, with HOME,
// XDG_CONFIG_HOME and TMPDIR at paths that don't exist and CANYON_HOST, CANYON_HARNESS and CANYON_ASCII set, each
// tool's answer and every request it made are the same. The tools are the server's own list, so a new one is checked.
func TestMCPRemoteNeverTouchesTheMachine(t *testing.T) {
	frozen := clock{now: func() time.Time { return at }, sleep: func(time.Duration) {}}
	run := func(h *mcpHost, host string) (map[string]string, []string) {
		t.Helper()
		cs := remoteConnect(t, host, frozen, nil)
		listed, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		answers := map[string]string{}
		for _, tool := range listed.Tools {
			args, ok := remoteArgs[tool.Name]
			if !ok {
				t.Fatalf("%s has no entry in remoteArgs: give it one, so it's run here", tool.Name)
			}
			res, text := callTool(t, cs, tool.Name, args)
			answers[tool.Name] = fmt.Sprintf("error %v: %s", res.IsError, text)
		}
		return answers, h.requests()
	}
	plain, hostileHost := &mcpHost{}, &mcpHost{}
	plainURL, hostileURL := plain.serve(t), hostileHost.serve(t) // the fixtures are read before the directory changes
	t.Chdir(t.TempDir())
	wantAnswers, wantAsked := run(plain, plainURL)
	hostile(t)
	for k, v := range map[string]string{"HOME": "/nonexistent/home", "XDG_CONFIG_HOME": "/nonexistent/config", "TMPDIR": "/nonexistent/tmp",
		"CANYON_HARNESS": "claude_code", "CANYON_ASCII": "1"} {
		t.Setenv(k, v)
	}
	dir, _ := os.Getwd()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory needs its search bit: the test's own back, so it can be removed
	gotAnswers, gotAsked := run(hostileHost, hostileURL)
	for tool, want := range wantAnswers {
		if cliCommand.MatchString(want) { // and on /mcp no answer sends the agent to a terminal
			t.Errorf("%s names %q on /mcp:\n%s", tool, cliCommand.FindAllString(want, -1), want)
		}
		if got := gotAnswers[tool]; got != want {
			t.Errorf("%s answered otherwise on a hostile machine:\n got %s\nwant %s", tool, got, want)
		}
	}
	if !slices.Equal(gotAsked, wantAsked) {
		t.Errorf("the tools asked otherwise on a hostile machine:\n got %q\nwant %q", gotAsked, wantAsked)
	}
}

// canyon_create_app makes the app for the caller and links it, its name the repository's or the folder's when none is
// given; a link refused deletes the app it made and says what the person can do: link their GitHub account, or ask an
// admin to let Canyon's GitHub App see the repository.
func TestMCPRemoteCreateAppMakesAndLinksOrUndoes(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    map[string]any
		link    func(w http.ResponseWriter)
		asked   []string
		says    string
		isError bool
	}{
		{"linked", map[string]any{"repository": "acme/claims-portal"}, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"repository":{}}`)) },
			[]string{`POST /api/v1/projects {"name":"claims-portal"}`, `PUT /api/v1/projects/quiet-river-4k2/repository {"folder":"","full_name":"acme/claims-portal"}`},
			"Made claims-portal (quiet-river-4k2), linked to acme/claims-portal. Its page: " + remoteBase + "/#/apps/quiet-river-4k2\nNext: canyon_deploy_ref with app quiet-river-4k2 and the branch to deploy.", false},
		{"a folder names it", map[string]any{"repository": "acme/mono", "folder": "apps/billing"}, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"repository":{}}`)) },
			[]string{`POST /api/v1/projects {"name":"billing"}`, `PUT /api/v1/projects/quiet-river-4k2/repository {"folder":"apps/billing","full_name":"acme/mono"}`},
			"Made billing (quiet-river-4k2), linked to acme/mono.", false},
		{"no GitHub account linked", map[string]any{"repository": "acme/claims-portal"}, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"github_link_required","message":"Link your GitHub account first"}`))
		}, []string{`POST /api/v1/projects {"name":"claims-portal"}`, `PUT /api/v1/projects/quiet-river-4k2/repository {"folder":"","full_name":"acme/claims-portal"}`, `DELETE /api/v1/projects/quiet-river-4k2?confirm=quiet-river-4k2 `},
			"Nothing was made: Canyon reads a repository for you only where your own GitHub account can. Open " + remoteBase + "/api/v1/github/link in your browser to link your GitHub account, then ask again.", true},
		{"another app holds it", map[string]any{"repository": "acme/claims-portal"}, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"linked","message":"the app claims-portal is linked to this repository and folder","app":"claims-portal"}`))
		}, []string{`POST /api/v1/projects {"name":"claims-portal"}`, `PUT /api/v1/projects/quiet-river-4k2/repository {"folder":"","full_name":"acme/claims-portal"}`, `DELETE /api/v1/projects/quiet-river-4k2?confirm=quiet-river-4k2 `},
			"Nothing was made. Canyon couldn't link the repository: the app claims-portal is linked to this repository and folder. Deploy it with canyon_deploy_ref and app claims-portal.", true},
		{"no App sees it", map[string]any{"repository": "acme/secret"}, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","message":"none of Canyon's GitHub Apps sees that repository"}`))
		}, []string{`POST /api/v1/projects {"name":"secret"}`, `PUT /api/v1/projects/quiet-river-4k2/repository {"folder":"","full_name":"acme/secret"}`, `DELETE /api/v1/projects/quiet-river-4k2?confirm=quiet-river-4k2 `},
			"Nothing was made: Canyon's GitHub App doesn't see acme/secret. Ask an admin to give Canyon's GitHub App access to it, in Settings › GitHub: " + remoteBase + "/#/settings/github", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var canonical any
				if json.Unmarshal(body, &canonical) == nil {
					body, _ = json.Marshal(canonical)
				}
				if r.URL.Path != "/api/v1/skill/manifest" { // the server's instructions, read at its start
					asked = append(asked, r.Method+" "+r.URL.RequestURI()+" "+string(body))
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"project":{"id":"quiet-river-4k2"}}`))
				case r.Method == http.MethodPut:
					c.link(w)
				case r.Method == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(host.Close)
			cs := remoteConnect(t, host.URL, wallClock, nil)
			res, text := callTool(t, cs, mcptools.CreateApp, c.args)
			for i := range asked {
				asked[i] = strings.TrimSuffix(asked[i], " null")
			}
			if !slices.Equal(asked, c.asked) || !strings.Contains(text, c.says) || res.IsError != c.isError {
				t.Errorf("asked %q\nisError %v, text:\n%s", asked, res.IsError, text)
			}
		})
	}
}

// refHost is a gate host that packages a ref of the linked repository, as POST …/packages/from-repository answers.
type refHost struct{ *gateHost }

func (h refHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/packages/from-repository") {
		h.asked = append(h.asked, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"package":{"id":"pkg_1","commit":"a1b2c3d4e5f60718293a4b5c6d7e8f9001122334"}}`)
		return
	}
	h.gateHost.ServeHTTP(w, r)
}

// On /mcp a run still going after about a minute's call is handed off with its id, an ingress or a web client cutting
// a longer call before it answers, and its steps name canyon_review, which follows it again as long, and hands it off
// again while it still goes on.
func TestMCPRemoteHandsARunOffAfter45Seconds(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}, page: `{"rows":[],"next":7}`} // a run that never ends
	server := httptest.NewServer(refHost{h})
	t.Cleanup(server.Close)
	now := time.Now()
	start := now
	var mu sync.Mutex
	clk := clock{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, sleep: func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }}
	cs := remoteConnect(t, server.URL, clk, nil)
	res, text := callTool(t, cs, mcptools.CheckRef, map[string]any{"app": "claims-portal", "ref": "main"})
	if res.IsError || !strings.Contains(text, "after 45 seconds") || !strings.Contains(text, "Call canyon_review with app claims-portal and run gr_8f2a in a minute") {
		t.Errorf("isError %v, text:\n%s", res.IsError, text)
	}
	if followed := clk.now().Sub(start); followed < mcptools.RemoteHandoff || followed > mcptools.RemoteHandoff+10*time.Second {
		t.Errorf("followed for %s", followed)
	}
	h.run = `{"run":{"id":"gr_8f2a","project":"claims-portal","package":"pkg_9e40","state":"running","blocking":0,"warnings":0,"progress":[],"findings":[]}}`
	start = clk.now()
	res, text = callTool(t, cs, mcptools.Review, map[string]any{"app": "claims-portal", "run": "gr_8f2a"}) // still going: followed again, and handed off again
	if res.IsError || !strings.Contains(text, "Call canyon_review with app claims-portal and run gr_8f2a in a minute") {
		t.Errorf("canyon_review of a run still going: isError %v, text:\n%s", res.IsError, text)
	}
	if followed := clk.now().Sub(start); followed < mcptools.RemoteHandoff || followed > mcptools.RemoteHandoff+10*time.Second {
		t.Errorf("canyon_review followed for %s", followed)
	}
}

// canyon_review reads back any run a hand-off gave the agent, a check's or a deploy's, on /mcp and canyon mcp alike, and
// follows it as the verb that started it: a check is a check's verdict, even though its package is never current and
// the status read never names its run; a deploy that went live says so, with its live url; and a run of another app
// is refused.
func TestMCPRemoteReviewReadsBackAHandedOffRun(t *testing.T) {
	ofApp := func(run, project string, deploys bool) string { // the run read, as the server answers it, naming its app
		extra := `"project":"` + project + `",`
		if deploys {
			extra += `"deploys":true,`
		}
		return strings.Replace(run, `{"run":{`, `{"run":{`+extra, 1)
	}
	noRun := reply{http.StatusNotFound, `{"error":"not_found","message":"no such run of this project"}`} // a check's run, on the status read
	for _, c := range []struct {
		name    string
		run     string
		page    string
		says    []string
		isError bool
	}{
		{"a check, ended", ofApp(aRun("clean", 0, 0), "claims-portal", false), "", []string{"CANYON REVIEW — RESULT: PASSED", "outcome: nothing was deployed."}, false},
		{"a check, blocked", ofApp(aRun("blocked", 1, 0, blocker), "claims-portal", false), "", []string{"RESULT: BLOCKED"}, false},
		{"a deploy, live", ofApp(aRun("clean", 0, 0), "claims-portal", true), "live", []string{"RESULT: PASSED", "live url: http://claims-portal.preview.localhost:7777", "Report the live url to the user"}, false},
		{"another app's run", ofApp(aRun("clean", 0, 0), "quote-engine", false), "", []string{"gr_8f2a is not a run of claims-portal"}, true},
	} {
		for _, remote := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, remote %v", c.name, remote), func(t *testing.T) {
				h := &gateHost{run: c.run, status: noRun, page: `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"clean"}}],"next":null}`}
				if c.page == "live" { // a deploy's run is the current package's, so the status read does know it
					h.page, h.status = buildPage(t, []string{"building"}, true, "private · you are an owner"), reply{http.StatusOK, `{}`}
				}
				server := httptest.NewServer(h)
				t.Cleanup(server.Close)
				var cs *mcp.ClientSession
				if remote {
					cs = remoteConnect(t, server.URL, wallClock, nil)
				} else {
					cs, _ = mcpConnect(t, server.URL, wallClock, nil)
				}
				res, text := callTool(t, cs, mcptools.Review, map[string]any{"app": "claims-portal", "run": "gr_8f2a"})
				for _, want := range c.says {
					if !strings.Contains(text, want) {
						t.Errorf("want %q; isError %v, text:\n%s", want, res.IsError, text)
					}
				}
				if res.IsError != c.isError || strings.Contains(text, "There is no live url") && c.page == "live" {
					t.Errorf("isError %v, text:\n%s", res.IsError, text)
				}
			})
		}
	}
}

// canyon_secret_link answers the app page's Secrets form on one secret, for the person to open in their own browser: a
// link, never a value, and none for an app the caller can't read or a name no secret may have.
func TestMCPRemoteSecretLinkOpensTheSecretsForm(t *testing.T) {
	h := &mcpHost{}
	cs := remoteConnect(t, h.serve(t), wallClock, nil)
	res, text := callTool(t, cs, mcptools.SecretLink, map[string]any{"app": "claims-portal", "name": "STRIPE_KEY", "env": "prod"})
	if res.IsError || text != "Give the user this link: "+remoteBase+"/#/apps/claims-portal/secrets?env=prod&name=STRIPE_KEY\nIt opens the app's Secrets form on STRIPE_KEY for production in their own browser, where they type the value. Never ask for the value in this conversation." {
		t.Errorf("isError %v, text:\n%s", res.IsError, text)
	}
	if res, _ := callTool(t, cs, mcptools.SecretLink, map[string]any{"app": "claims-portal", "name": "<img src=x>"}); !res.IsError {
		t.Error("a name no secret may have was linked")
	}
	h.refuse = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found","message":"no app claims-portal on this Canyon"}`)
	}
	if res, text := callTool(t, cs, mcptools.SecretLink, map[string]any{"app": "claims-portal", "name": "STRIPE_KEY"}); !res.IsError || strings.Contains(text, "#/apps/") {
		t.Errorf("an app the caller can't read: %v %s", res.IsError, text)
	}
}

// Where the CLI says canyon secret set, a step on /mcp names canyon_secret_link's link instead: no terminal is in reach
// there, and the value is never the agent's.
func TestMCPRemoteStepsNameTheSecretLink(t *testing.T) {
	remote, local := &app{remote: true}, &app{}
	for name, got := range map[string]string{
		"the missing step":       remote.missingStep(),
		"the failed promotion":   remote.secretAsk(),
		"a leaked secret":        fmt.Sprintf(aiRotateStep, "src/app.ts:1", remote.secretSet("canyon secret set NAME in their own terminal", "")),
		"a stopped start":        fmt.Sprintf(aiStartSteps, remote.secretSet("canyon secret set", "")),
		"the manifest's secrets": agentManifest(manifest{Host: "http://localhost:7777", Remote: true}, false),
	} {
		if strings.Contains(got, "canyon secret set") || !strings.Contains(got, "canyon_secret_link") {
			t.Errorf("%s on /mcp: %s", name, got)
		}
	}
	if !strings.Contains(local.missingStep(), "canyon secret set NAME --env prod in their own terminal") ||
		!strings.Contains(agentManifest(manifest{Host: "http://localhost:7777"}, false), secretsHanded) {
		t.Error("the CLI's own steps changed")
	}
}

// misworded is what a rewrite of a CLI step leaves that no tool step says: a flag after a tool, a terminal, a host after
// connecting again.
var misworded = regexp.MustCompile(`canyon_[a-z_]+ --[a-z]|in a terminal|again https?://`)

// cliCommand is a CLI command in an agent's text: what no step on /mcp may name, where no terminal is in reach.
var cliCommand = regexp.MustCompile(`\bcanyon (check|deploy|status|logs|secret|login|logout|share|undeploy|pull|playbook|manifest|delete|rename)\b`)

// Every block the CLI writes for an agent, said on /mcp, names Canyon's tools and never a CLI command: the ai goldens,
// each variant the CLI renders, run through what /mcp says in their place. The goldens stay the CLI's, byte for byte.
func TestMCPRemoteBlocksNameToolsNeverCommands(t *testing.T) {
	files, err := filepath.Glob("testdata/*ai*.txt")
	if err != nil || len(files) < 30 {
		t.Fatalf("the ai goldens: %d, %v", len(files), err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the package's own goldens
		if err != nil {
			t.Fatal(err)
		}
		said := remoteSaid(string(raw))
		var outside strings.Builder // the app's fenced lines are its own, and pass as they are
		var fence fenceState
		for _, line := range strings.SplitAfter(said, "\n") {
			if !fence.inside(line) {
				outside.WriteString(line)
			}
		}
		if cliCommand.MatchString(outside.String()) || misworded.MatchString(outside.String()) {
			t.Errorf("%s on /mcp still names %q, or says %q:\n%s", f, cliCommand.FindAllString(outside.String(), -1), misworded.FindAllString(outside.String(), -1), said)
		}
	}
}

// The steps the phrase table can't word are worded for /mcp where they're written: an app not on this Canyon is made
// with canyon_create_app, an ended sign-in is reconnected in the agent's settings, a silent failure's log is
// canyon_logs; and the app's own fenced lines pass through as the app wrote them, CLI words and all.
func TestMCPRemoteWordsItsOwnSteps(t *testing.T) {
	var out, errs strings.Builder
	a := aiApp(&out, "check")
	a.err, a.remote = &errs, true
	a.lacked(&notOnHost{app: "claims-portal", host: remoteBase})
	if text := out.String(); !strings.Contains(text, "call canyon_create_app with its repository") || cliCommand.MatchString(text) || strings.Contains(text, "--create") {
		t.Errorf("an app not on this Canyon, on /mcp:\n%s", text)
	}
	out.Reset()
	a.fail(&SessionExpired{Host: remoteBase, Sent: true})
	if text := errs.String(); !strings.Contains(text, "Connect Canyon again in your agent's MCP settings") || cliCommand.MatchString(text) {
		t.Errorf("an ended sign-in, on /mcp:\n%s", text)
	}
	for _, steps := range []string{fmt.Sprintf(aiBuildQuietSteps, a.seeTheLog("the log")), fmt.Sprintf(aiStartQuietSteps, a.seeTheLog("its log"), a.secretSet("canyon secret set", ""))} {
		if !strings.Contains(steps, "call canyon_logs") || strings.Contains(steps, "in a terminal") {
			t.Errorf("a silent failure, on /mcp:\n%s", steps)
		}
	}
	fenced := "Run: canyon deploy\n```\nnpm ERR! try: canyon deploy --force in a terminal\n```\n"
	if got := remoteSaid(fenced); got != "Call canyon_deploy_ref\n```\nnpm ERR! try: canyon deploy --force in a terminal\n```\n" {
		t.Errorf("the app's fenced lines were reworded:\n%s", got)
	}
}

// /mcp's instructions and every tool's description name only the tools it serves, and no CLI command.
func TestMCPRemoteInstructionsNameItsOwnTools(t *testing.T) {
	h := &mcpHost{}
	cs := remoteConnect(t, h.serve(t), wallClock, nil)
	said := []string{cs.InitializeResult().Instructions}
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		raw, _ := json.Marshal(tool)
		said = append(said, string(raw))
	}
	notServed := regexp.MustCompile(`\bcanyon_(check|deploy)\b`) // canyon mcp's directory tools
	for _, text := range said {
		if cliCommand.MatchString(text) || notServed.MatchString(text) || strings.Contains(text, "canyon CLI") {
			t.Errorf("on /mcp: %q, %q in:\n%s", cliCommand.FindAllString(text, -1), notServed.FindAllString(text, -1), text)
		}
	}
}
