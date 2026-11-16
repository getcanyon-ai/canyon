package main

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5"
)

// cleanGate makes the test server's gate answer clean for the test, then blocked again.
func cleanGate(t *testing.T) {
	t.Helper()
	pipe.Gate = runstest.CleanGolden
	t.Cleanup(func() { pipe.Gate = runstest.Golden })
}

func TestADeployThroughTheAPIRecordsThePreviewOnTheProjectAndAuditsWhoAsked(t *testing.T) {
	cleanGate(t)
	pool, ctx, base, c := withProject(t)
	_, uploaded := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); len(body["project"].(map[string]any)["envs"].(map[string]any)) != 0 {
		t.Fatalf("nothing deployed yet: %s", lastBody)
	}
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if code != 202 || run == "" {
		t.Fatalf("deploy: %d %s", code, lastBody)
	}
	if got := waitVerdict(t, c, base, run); got["state"] != "clean" {
		t.Fatalf("a deploy of the fixed app ends clean: %s", lastBody)
	}
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	envs := body["project"].(map[string]any)["envs"].(map[string]any)
	preview, _ := envs["preview"].(map[string]any)
	at, _ := preview["deployed_at"].(string)
	if len(envs) != 1 || preview["url"] != "http://claims-portal.preview.localhost:7777" || preview["package"] != packageOf(uploaded)["id"] || preview["image_digest"] != executortest.Digest || preview["gate_run"] != run || !strings.HasSuffix(at, "Z") || !reflect.DeepEqual(preview["deployed_by"], map[string]any{"kind": "person", "subject": "anonymous", "display": ""}) || preview["run_by"] != "canyon" || len(preview) != 7 {
		t.Fatalf("envs.preview (contracts/routes.md row 43): %s", lastBody)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("deployed_at: %v", err)
	}
	for action, want := range map[string]int{"POST /api/v1/projects/{id}/deploys": 1, "deploy preview": 1} {
		var n int
		var subject string
		if err := pool.QueryRow(ctx, "SELECT count(*), min(actor_subject) FROM audit_events WHERE action = $1 AND run_id = $2", action, run).Scan(&n, &subject); err != nil || n != want || subject != "anonymous" {
			t.Fatalf("audit rows for %q naming the run: %d by %q (%v)", action, n, subject, err)
		}
	}
	// the published URL is one the surface routes to this app's preview container
	if u, err := url.Parse(preview["url"].(string)); err != nil {
		t.Fatal(err)
	} else if app, env, ok := appSurface(u.Host); !ok || app != "claims-portal" || env != "preview" {
		t.Fatalf("the surface does not route the URL the deploy published: %q → %q %q %v", u.Host, app, env, ok)
	}
	// a deploy answers a gate run's refusals: 404 on an unknown project, 409 while a run is active
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/nothing/deploys", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("unknown project: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_busy00000', 'claims-portal', $1, 'running')", packageOf(uploaded)["id"]); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", ""); code != 409 || body["error"] != "run_in_progress" {
		t.Fatalf("a run in progress: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE id = 'gr_busy00000'"); err != nil { // leave nothing in flight
		t.Fatal(err)
	}
}

func TestADeployOnABlockedGateEndsBlockedWithNothingDeployed(t *testing.T) {
	pool, ctx, base, c := withProject(t) // the gate is the golden log: blocked
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if got := waitVerdict(t, c, base, run); got["state"] != "blocked" {
		t.Fatalf("blocked: %s", lastBody)
	}
	if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); len(body["project"].(map[string]any)["envs"].(map[string]any)) != 0 {
		t.Fatalf("nothing deployed on a blocked gate: %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys") + count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE kind IN ('build.log', 'deploy.completed')"); n != 0 {
		t.Fatalf("a blocked deploy built or deployed: %d rows", n)
	}
}

func TestTheAppSurfaceProxiesToTheAppsContainerAndEveryOtherHostFallsThrough(t *testing.T) {
	var gotHost, gotPath, gotProto, dialed string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath, gotProto = r.Host, r.URL.RequestURI(), r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		if strings.HasPrefix(addr, "down-") {
			return nil, errors.New("no such host")
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := (&app{own: "canyon.preview.example.com"}).surfaces(mux, transport) // a control plane published under an app-shaped name; auth off
	serve := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder"+path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := serve("claims-portal.preview.localhost:7777", "/claims/42?tab=notes"); rec.Code != http.StatusTeapot || gotHost != "claims-portal.preview.localhost:7777" || gotPath != "/claims/42?tab=notes" || dialed != "claims-portal-preview:3000" || gotProto != "http" {
		t.Fatalf("preview: %d host %q path %q dialed %q proto %q", rec.Code, gotHost, gotPath, dialed, gotProto)
	}
	if rec := serve("Claims-Portal.Preview.LOCALHOST", "/"); rec.Code != http.StatusTeapot || dialed != "claims-portal-preview:3000" { // DNS is not case sensitive
		t.Fatalf("mixed case: %d dialed %q", rec.Code, dialed)
	}
	if rec := serve("claims-portal.prod.example.com", "/"); rec.Code != http.StatusTeapot || dialed != "claims-portal-prod:3000" {
		t.Fatalf("prod: %d dialed %q", rec.Code, dialed)
	}
	for _, host := range []string{"localhost:7777", "127.0.0.1:7777", "[::1]:7777", "canyon.preview.example.com", "CANYON.preview.example.com", "claims-portal.staging.localhost", "foo-.preview.localhost", "claims-portal.preview", "claims-portal.preview:7777", "preview.localhost", ""} {
		if rec := serve(host, "/health"); rec.Code != http.StatusNoContent {
			t.Fatalf("%q is not an app surface, want the server's own routes: %d", host, rec.Code)
		}
	}
	if rec := serve("down-app.preview.localhost:7777", "/"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"error":"app_unreachable"`) {
		t.Fatalf("an app that is not answering: %d %s", rec.Code, rec.Body.String())
	}
}

// Behind a proxy that ends TLS every request reaches the server as http. The app is told the scheme
// people open it at, CANYON_PUBLIC_URL's, so an app that insists on https neither loops nor links to http.
func TestTheAppSurfaceTellsTheAppTheSchemePeopleOpenItAt(t *testing.T) {
	var gotProto string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
	}))
	defer upstream.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	for public, want := range map[string]string{"https://canyon.example.com": "https", "http://localhost:7777": "http"} {
		h := (&app{public: public}).surfaces(http.NotFoundHandler(), transport)                             // auth off: no sign-in in the way
		req := httptest.NewRequest(http.MethodGet, "http://claims-portal.preview.canyon.example.com/", nil) // plain http, as from the proxy
		req.Header.Set("X-Forwarded-Proto", "gopher")                                                       // a client's own claim is never passed on
		h.ServeHTTP(httptest.NewRecorder(), req)
		if gotProto != want {
			t.Errorf("CANYON_PUBLIC_URL=%s: the app is told %q, want %q", public, gotProto, want)
		}
	}
}

// A client's own claims about the connection never reach the app: Rack reads X-Forwarded-Ssl: on as
// https, others X-Forwarded-Scheme, X-Forwarded-Port, X-Real-IP or Forwarded. The proxy sets its own
// X-Forwarded-For, -Host and -Proto and passes none of these on.
func TestTheAppSurfacePassesOnNoClientForwardingHeaders(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer upstream.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	h := (&app{public: "https://canyon.example.com"}).surfaces(http.NotFoundHandler(), transport)
	req := httptest.NewRequest(http.MethodGet, "http://claims-portal.preview.canyon.example.com/", nil)
	forged := map[string]string{"X-Forwarded-Ssl": "on", "X-Forwarded-Scheme": "https", "X-Forwarded-Port": "443", "X-Real-Ip": "10.0.0.1", "Forwarded": "for=10.0.0.1;proto=https",
		// who the person is, as an auth proxy says it: only Canyon's identity token says that to an app
		"X-Forwarded-Email": "dana@meridian.example", "X-Forwarded-User": "dana", "X-Forwarded-Groups": "admins", "X-Forwarded-Preferred-Username": "dana",
		"X-Auth-Request-Email": "dana@meridian.example", "X-Auth-Request-Access-Token": "forged", "Remote-User": "dana",
		"X-Forwarded-Access-Token": "forged", "X-Forwarded-Prefix": "/admin", "X-Amzn-Oidc-Identity": "dana", "X-Amzn-Oidc-Data": "forged",
		"True-Client-Ip": "10.0.0.1", "X-Client-Ip": "10.0.0.1", "Client-Ip": "10.0.0.1",
		"X-Scheme": "https", "Front-End-Https": "on", "X-Url-Scheme": "https", "X-Webauth-User": "dana", "X-Remote-User": "dana", "Cf-Connecting-Ip": "10.0.0.1", "X-Cluster-Client-Ip": "10.0.0.1",
		// Rack, Django, PHP and WSGI read X_Forwarded_Email as HTTP_X_FORWARDED_EMAIL, as they read X-Forwarded-Email
		"X_Forwarded_Email": "dana@meridian.example", "X_Forwarded_Proto": "https", "X_Forwarded_Ssl": "on", "x_FORWARDED_user": "dana", "X-Forwarded_Groups": "admins"}
	for k, v := range forged {
		req.Header[k] = []string{v} // as the client wrote it: an underscore's name is never canonicalised on the wire
	}
	req.Header.Set("Authorization", "Bearer forged")
	req = req.WithContext(context.WithValue(req.Context(), tokenKey{}, "canyons-own")) // as admit leaves it: Canyon's identity token for the app
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil {
		t.Fatal("the app was never reached")
	}
	for k := range forged {
		if v := got.Values(k); len(v) != 0 {
			t.Errorf("the app was handed the client's %s: %q", k, v)
		}
	}
	if got.Get("X-Forwarded-Proto") != "https" || got.Get("X-Forwarded-Host") != "claims-portal.preview.canyon.example.com" || got.Get("Authorization") != "Bearer canyons-own" {
		t.Errorf("the proxy's own: proto %q host %q authorization %q", got.Get("X-Forwarded-Proto"), got.Get("X-Forwarded-Host"), got.Get("Authorization"))
	}
}

func TestAProjectNameIsADNSLabel(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, name := range []string{"foo-", strings.Repeat("a", 56), "a--", "-a"} { // 56: the container name <name>-preview would pass 63
		if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`); code != 400 || body["error"] != "invalid_id" {
			t.Fatalf("%q: %d %s", name, code, lastBody)
		}
	}
	for _, name := range []string{"a", "a-b", strings.Repeat("a", 55), "a--b"} {
		if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+name+`"}`); code != 201 {
			t.Fatalf("%q: %d %s", name, code, lastBody)
		}
	}
	// the database holds the same line on its own (migration 0006), whatever reaches it
	for _, id := range []string{"foo-", strings.Repeat("a", 56)} {
		if _, err := pool.Exec(context.Background(), "INSERT INTO projects (id, name) VALUES ($1, $1)", id); err == nil {
			t.Fatalf("the projects CHECK accepted %q", id)
		}
	}
}

func TestASecondDeployReplacesThePreviewRow(t *testing.T) {
	cleanGate(t)
	pool, ctx, base, c := withProject(t)
	deploy := func(tree map[string]string) (string, map[string]any) {
		t.Helper()
		uploadPulled(t, c, base, "claims-portal", tarOf(t, tree))
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
		run, _ := runOf(started)["id"].(string)
		if got := waitVerdict(t, c, base, run); got["state"] != "clean" {
			t.Fatalf("deploy: %s", lastBody)
		}
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		return run, body["project"].(map[string]any)["envs"].(map[string]any)["preview"].(map[string]any)
	}
	firstRun, first := deploy(map[string]string{"src/app.ts": "export {}\n"})
	secondRun, second := deploy(map[string]string{"src/app.ts": "export const fixed = true\n"})
	if second["package"] == first["package"] || second["gate_run"] != secondRun || second["gate_run"] == firstRun || second["deployed_at"].(string) < first["deployed_at"].(string) || second["url"] != first["url"] {
		t.Fatalf("the second deploy replaces the row:\n first %v\nsecond %v", first, second)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys"); n != 1 {
		t.Fatalf("one row per project and environment: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE kind = 'deploy.completed'"); n != 2 {
		t.Fatalf("the history is the ledger's: %d deploy rows", n)
	}
}

// A deploy's access lines name the app's pending requests to its owner and to an admin, judged by the
// request that starts the run, and to no contributor (design/PERMISSIONS.md, Access requests;
// contracts/events.md, build.log): the rows the run writes after its deploy.completed row, which the run's
// timeline then serves to its owners and admins alone.
func TestADeploysAccessLinesNameRequestsToAnOwnerOrAnAdminAlone(t *testing.T) {
	withOIDC(t) // dana is the admin
	cleanGate(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // olive owns it, carl contributes, vic reads
	carl, dana := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	// the timeline's judgement fails closed: a level that cannot be read, or a run that names no app, reads no request
	if _, err := pool.Exec(ctx, "INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', 'otto@meridian.example', 'owner', 'person', 'sub-olive', '')"); err != nil {
		t.Fatal(err)
	}
	judge := &app{pool: pool, log: log.New(io.Discard, "", 0)}
	member := func(ctx context.Context, email string) context.Context {
		return context.WithValue(context.WithValue(ctx, roleKey{}, "member"), emailKey{}, email)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		who  string
		ctx  context.Context
		run  string
		sees bool
	}{
		{"an owner", member(ctx, "otto@meridian.example"), "gr_seed", true},
		{"a contributor", member(ctx, "carl@meridian.example"), "gr_seed", false},
		{"an owner, on a run no app has", member(ctx, "otto@meridian.example"), "gr_nope", false},
		{"an owner, whose level cannot be read", member(gone, "otto@meridian.example"), "gr_seed", false},
	} {
		if got := judge.readsRequests(c.ctx, c.run); got != c.sees {
			t.Errorf("%s reads the requests of %s: %v, want %v", c.who, c.run, got, c.sees)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO access_requests (project_id, email, subject, requested_at) VALUES ('claims-portal', 'sam@meridian.example', '', now() - interval '2 hours 5 minutes')"); err != nil {
		t.Fatal(err)
	}
	shared, asked := "Shared with 2 people.", "Access requests: sam@meridian.example, 2h ago · canyon share sam@meridian.example"
	none := "No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."
	var owners string // the owner's run, which names the request
	for _, c := range []struct {
		who    string
		client *http.Client
		want   []string
	}{{"its owner", olive, []string{shared, asked, none}}, {"an admin", dana, []string{shared, asked, none}}, {"a contributor", carl, []string{shared, none}}} {
		code, started, _ := do(t, c.client, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
		run, _ := runOf(started)["id"].(string)
		if code != 202 || run == "" {
			t.Fatalf("%s's deploy: %d %s", c.who, code, lastBody)
		}
		if got := waitVerdict(t, c.client, base, run); got["state"] != "clean" {
			t.Fatalf("%s's deploy ends clean: %s", c.who, lastBody)
		}
		rows, _ := pool.Query(ctx, `SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log'
			AND id > (SELECT id FROM timeline_rows WHERE run_id = $1 AND kind = 'deploy.completed') ORDER BY id`, run)
		if got, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(got, c.want) {
			t.Fatalf("%s's deploy wrote after deploy.completed:\n got %q (%v)\nwant %q", c.who, got, err, c.want)
		}
		owners = cmp.Or(owners, run)
	}
	// the run's timeline serves the request's row to its owner and an admin, and to a contributor leaves it
	// out with no sign of it (contracts/events.md, build.log, amended 2026-10-26)
	for _, c := range []struct {
		who    string
		client *http.Client
		sees   bool
	}{{"its owner", olive, true}, {"an admin", dana, true}, {"a contributor", carl, false}} {
		code, body, _ := do(t, c.client, http.MethodGet, base+"/api/v1/gate-runs/"+owners+"/timeline?limit=200", "")
		if code != 200 {
			t.Fatalf("%s reads the timeline: %d %s", c.who, code, lastBody)
		}
		var lines []string
		for _, r := range body["rows"].([]any) {
			if line, ok := r.(map[string]any)["data"].(map[string]any)["line"].(string); ok {
				lines = append(lines, line)
			}
		}
		if !slices.Contains(lines, shared) || slices.Contains(lines, asked) != c.sees || strings.Contains(lastBody, "sam@meridian.example") != c.sees {
			t.Fatalf("%s reads the owner's deploy: request served %v, want %v: %q", c.who, slices.Contains(lines, asked), c.sees, lines)
		}
	}
}

// A deploy of an app everyone at the org can open names the org as the server says it now, through the
// pipeline's Org as main wires it (contracts/events.md, build.log; design/EXPERIENCE.md D5).
func TestADeployOfAnAppEveryoneCanOpenNamesTheOrg(t *testing.T) {
	withOIDC(t)
	cleanGate(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	for _, q := range []string{
		"INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'everyone', '', 'viewer', 'person', 'sub-olive', '')",
		"UPDATE org_preferences SET org_name = 'Acme Insurance'",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	code, started, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if code != 202 || run == "" {
		t.Fatalf("the deploy: %d %s", code, lastBody)
	}
	if got := waitVerdict(t, olive, base, run); got["state"] != "clean" {
		t.Fatalf("the deploy ends clean: %s", lastBody)
	}
	var first string
	if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log'
		AND id > (SELECT id FROM timeline_rows WHERE run_id = $1 AND kind = 'deploy.completed') ORDER BY id LIMIT 1), '')`, run).Scan(&first); err != nil || first != "Shared with everyone at Acme Insurance." {
		t.Fatalf("the first line after deploy.completed: %q (%v)", first, err)
	}
}
