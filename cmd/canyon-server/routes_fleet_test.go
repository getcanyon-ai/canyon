package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedGolden writes the rows behind contracts/testdata/fleet/page.json from demo_seed.sql, the same
// text the demo host boots with: this suite is what holds that roster to the fixture. The verdict
// row on claims-portal's blocked run is what the secrets_exposed tile reads.
func seedGolden(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, demoSeed); err != nil {
		t.Fatal(err)
	}
}

// runGolden starts what the golden page reads running, so each row's needs are the page's: claims-archive's
// production, which nothing started, reads stopped, a need.
func runGolden() {
	fake.Start("claims-portal", "preview")
	fake.Start("endorsement-hub", "preview")
	fake.Start("premium-ledger", "prod")
}

func golden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/fleet/page.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func ids(t *testing.T, page map[string]any) (out []string) {
	t.Helper()
	apps, ok := page["apps"].([]any)
	if !ok {
		t.Fatalf("no apps[] in %s", lastBody)
	}
	for _, a := range apps {
		out = append(out, a.(map[string]any)["id"].(string))
	}
	return out
}

// A row's warnings are its last run's verdict's advisory count: a passed app with advisories never reads
// as a clean one on the fleet (the test cluster audit), and a run with no verdict yet, or no run, counts none.
func TestAFleetRowCountsItsLastRunsWarnings(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	// premium-ledger's last run passed with no verdict row in the seed: its verdict, with two advisories
	if _, err := pool.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES
	  ('run', 'gr_4b60', 'premium-ledger', 'verdict', 'verdict', '{"state": "clean", "blocking": 0, "warnings": 2, "findings": {"version": "2.1.0", "runs": []}}')`); err != nil {
		t.Fatal(err)
	}
	_, got, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet", "")
	warnings := map[string]any{}
	for _, a := range got["apps"].([]any) {
		app := a.(map[string]any)
		warnings[app["id"].(string)] = app["warnings"]
	}
	if warnings["premium-ledger"] != 2.0 || warnings["quote-engine"] != 0.0 || warnings["claims-portal"] != 0.0 {
		t.Fatalf("warnings by app: %v", warnings)
	}
}

func TestFleetAnswersTheGoldenPage(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	runGolden()
	asked := fake.RunningOfs.Load()
	code, got, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet", "")
	want := golden(t)
	if n := fake.RunningOfs.Load() - asked; n != 1 {
		t.Fatalf("the fleet read asked the executor %d times what runs; once answers the page", n)
	}
	if code != 200 || !reflect.DeepEqual(got["tiles"], want["tiles"]) {
		t.Fatalf("the tiles: %d %v, want %v", code, got["tiles"], want["tiles"])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the golden page:\n got %s\nwant %s", lastBody, mustJSON(want))
	}
}

// Status and fleet say what runs, not what the last deploy recorded (#1241): an app whose container
// stopped reads stopped, and one the executor could not be asked about in time reads unknown — never
// running.
func TestStatusAndFleetSayWhetherTheAppRuns(t *testing.T) {
	cleanGate(t)
	_, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	if run, _ := runOf(started)["id"].(string); waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("a deploy of the fixed app ends clean: %s", lastBody)
	}
	says := func(want string) {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		preview, _ := status["envs"].(map[string]any)["preview"].(map[string]any)
		_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		row, _ := fleet["apps"].([]any)[0].(map[string]any)["envs"].(map[string]any)["preview"].(map[string]any)
		if preview["state"] != want || row["state"] != want {
			t.Fatalf("status says %v and the fleet %v, want %s", preview["state"], row["state"], want)
		}
	}
	says("running")
	if err := (&executor.Client{Base: fake.URL}).Stop(ctx, "claims-portal", "preview"); err != nil { // its container is gone: the app exited and nothing started it again
		t.Fatal(err)
	}
	says("stopped")
	fake.FailRunning.Store(true)
	says("unknown")
	fake.FailRunning.Store(false)
	fake.Start("claims-portal", "preview")
	was := runningWithin
	runningWithin = 50 * time.Millisecond
	hold := make(chan struct{})
	fake.HoldRunning.Store(&hold)
	t.Cleanup(func() { runningWithin = was; close(hold) })
	says("unknown") // an executor that does not answer in time is not asked longer: the read answers
}

// sayRunning asks only about what Canyon runs (#1241): an environment the org runs gets no state and
// is never asked about, so a page of nothing else makes no executor call; on a mixed page only
// Canyon's are asked, in one call. An executor that is missing, or older than POST /running (its
// 404), reads unknown, never running.
func TestSayRunningAsksOnlyAboutWhatCanyonRuns(t *testing.T) {
	f := executortest.New(t)
	f.Start("claims-portal", "preview")
	a := &app{pipe: &runs.Pipeline{Executor: &executor.Client{Base: f.URL}}, log: log.New(io.Discard, "", 0)}
	env := func(by string) map[string]any {
		return map[string]any{"url": "http://claims-portal.preview.localhost:7777", "run_by": by}
	}
	page := func() map[string]map[string]any {
		return map[string]map[string]any{"broker-portal": {"preview": map[string]any{"run_by": "org", "pending": map[string]any{}}, "prod": env("org")}, "claims-portal": {"preview": env("canyon"), "prod": env("org")}}
	}
	states := func(p map[string]map[string]any) (out []any) {
		for _, id := range []string{"broker-portal", "claims-portal"} {
			for _, e := range []string{"preview", "prod"} {
				m, _ := p[id][e].(map[string]any)
				out = append(out, m["state"])
			}
		}
		return out
	}
	org := map[string]map[string]any{"broker-portal": page()["broker-portal"]}
	if a.sayRunning(t.Context(), org, false, false); f.RunningOfs.Load() != 0 || !reflect.DeepEqual(org["broker-portal"], page()["broker-portal"]) {
		t.Fatalf("a page the org runs all of asked %d times: %v", f.RunningOfs.Load(), org)
	}
	for _, c := range []struct {
		name  string
		setup func()
		want  any
	}{
		{"an executor that answers", func() {}, "running"},
		{"an executor older than POST /running", func() { f.NoRunningOf.Store(true) }, "unknown"},
		{"no executor", func() { a.pipe.Executor = nil }, "unknown"},
	} {
		c.setup()
		p, before := page(), f.RunningOfs.Load()
		a.sayRunning(t.Context(), p, false, false)
		if got := states(p); !reflect.DeepEqual(got, []any{nil, nil, c.want, nil}) {
			t.Errorf("%s: states %v, want only claims-portal's preview %v", c.name, got, c.want)
		}
		if asked := f.RunningOfs.Load() - before; asked > 1 {
			t.Errorf("%s: asked %d times", c.name, asked)
		}
	}
}

func TestFleetClampsTheLimitAndPagesWithoutRepeatOrGap(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	runGolden()
	c := client(t)
	var walked []string
	for url, prev := base+"/api/v1/fleet?limit=2", 2; ; {
		code, page, _ := do(t, c, http.MethodGet, url, "")
		n := len(ids(t, page))
		if code != 200 || n > 2 || prev != 2 { // a page after a short one: next pointed past the end
			t.Fatalf("%s: %d %s (the page before had %d rows)", url, code, lastBody, prev)
		}
		walked = append(walked, ids(t, page)...)
		next, ok := page["next"].(string)
		if !ok {
			if page["next"] != nil {
				t.Fatalf("next is a string or null: %s", lastBody)
			}
			break
		}
		url, prev = base+"/api/v1/fleet?limit=2&cursor="+next, n
	}
	if want := ids(t, golden(t)); !reflect.DeepEqual(walked, want) {
		t.Fatalf("walking at limit=2:\n got %v\nwant %v", walked, want)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) SELECT 'extra-' || i, 'extra-' || i FROM generate_series(1, 201) i"); err != nil {
		t.Fatal(err)
	}
	if code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?limit=500", ""); code != 200 || len(page["apps"].([]any)) != 200 || page["next"] == nil {
		t.Fatalf("limit=500 is clamped to 200 rows with a next page: %d, %d rows, next %v", code, len(page["apps"].([]any)), page["next"])
	}
	for query, code := range map[string]string{"limit=0": "invalid_limit", "limit=ten": "invalid_limit", "cursor=nope": "invalid_cursor", "cursor=" + fleetCursor{Sort: "risk", Rank: 2147483648, ID: "x"}.String(): "invalid_cursor"} { // a rank past the seven buckets is refused, never sent to Postgres
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, ""); status != 400 || body["error"] != code {
			t.Errorf("?%s: %d %v, want 400 %s", query, status, body, code)
		}
	}
}

// A page by its number (contracts/routes.md, the fleet read: page) is the rows at its offset under the sort and filters, each
// answering the same total and the page it served; a page past the last is the last, and a cursor read answers page null.
func TestFleetPagesByNumberAndClampsPastTheLast(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	runGolden()
	c := client(t)
	var walked []string
	for n := 1; n <= 4; n++ {
		code, page, _ := do(t, c, http.MethodGet, fmt.Sprintf("%s/api/v1/fleet?limit=2&page=%d", base, n), "")
		if code != 200 || page["total"] != 7.0 || page["page"] != float64(n) {
			t.Fatalf("page %d: %d %s", n, code, lastBody)
		}
		walked = append(walked, ids(t, page)...)
	}
	if want := ids(t, golden(t)); !reflect.DeepEqual(walked, want) {
		t.Fatalf("pages 1 to 4 at limit=2:\n got %v\nwant %v", walked, want)
	}
	for query, want := range map[string]struct {
		ids         string
		total, page float64
	}{
		"limit=2&page=9":                    {"premium-ledger", 7, 4}, // past the last: the last
		"limit=2&page=99999999999999999999": {"premium-ledger", 7, 4}, // past any integer too
		"tile=blocked&limit=1&page=2":       {"commission-calc", 2, 2},
		"sort=name&limit=3&page=2":          {"endorsement-hub,fnol-intake,premium-ledger", 7, 2},
		"q=nothing-matches&page=3":          {"", 0, 1}, // no rows: one page, the empty one
	} {
		code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, "")
		if got := strings.Join(ids(t, page), ","); code != 200 || got != want.ids || page["total"] != want.total || page["page"] != want.page {
			t.Errorf("?%s: %d %q total %v page %v, want %q total %v page %v", query, code, got, page["total"], page["page"], want.ids, want.total, want.page)
		}
	}
	_, first, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?limit=2", "")
	if first["page"] != 1.0 {
		t.Fatalf("a read with neither page nor cursor answers page 1: %s", lastBody)
	}
	if code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?limit=2&cursor="+first["next"].(string), ""); code != 200 || page["page"] != nil || page["total"] != 7.0 {
		t.Fatalf("a read by cursor answers page null: %d %s", code, lastBody)
	}
	for _, query := range []string{"page=0", "page=-1", "page=-99999999999999999999", "page=two", "page=1.5", "page=2&cursor=" + first["next"].(string), "page=2&cursor=nope"} {
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, ""); status != 400 || body["error"] != "invalid_page" {
			t.Errorf("?%s: %d %v, want 400 invalid_page", query, status, body)
		}
	}
}

func TestFleetFiltersNarrowThePageAndNeverTheTiles(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	runGolden()
	c := client(t)
	all, tiles := "claims-archive,claims-portal,commission-calc,quote-engine,fnol-intake,endorsement-hub,premium-ledger", golden(t)["tiles"]
	for query, want := range map[string]string{
		"tile=blocked": "claims-portal,commission-calc", "tile=secrets_exposed": "claims-portal,commission-calc", "tile=never_reviewed": "quote-engine", "tile=unowned": "quote-engine",
		"owner=priya@meridian.example": "commission-calc,endorsement-hub", "owner=Priya+Raman": "", // a subject, never a display name
		"stage=awaiting_review": "claims-archive,quote-engine", "stage=production": "premium-ledger",
		"running=production": "claims-archive,premium-ledger", "running=preview": "claims-portal,endorsement-hub", "running=nothing": "commission-calc,quote-engine,fnol-intake",
		"stack=remix": "claims-archive,endorsement-hub", "stack=.": "premium-ledger", "stack=%25": "", // literal substrings: '.' matches Next.js, '%' matches nothing
		"q=CLAIMS": "claims-archive,claims-portal", "q=" + strings.Repeat("x", 64): "",
		"needs_you=true": "claims-archive,claims-portal,commission-calc,quote-engine", "needs_you=yes": all, "tile=blocked&owner=dana@meridian.example": "claims-portal",
	} {
		code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, "")
		if got := strings.Join(ids(t, page), ","); code != 200 || got != want {
			t.Errorf("?%s: %d %q, want %q", query, code, got, want)
		}
		if !reflect.DeepEqual(page["tiles"], tiles) {
			t.Errorf("?%s narrowed the tiles: %v", query, page["tiles"])
		}
	}
	for query, code := range map[string]string{
		"tile=owned": "invalid_tile", "stage=deployed": "invalid_stage", "running=both": "invalid_running", "access=public": "invalid_access",
		"q=" + strings.Repeat("x", 65): "invalid_q", "stack=" + strings.Repeat("é", 65): "invalid_stack",
	} {
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, ""); status != 400 || body["error"] != code {
			t.Errorf("?%s: %d %v, want 400 %s", query, status, body, code)
		}
	}
}

func TestFleetQMatchesTheNameAsWellAsTheId(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('proj-7f3a', 'Claims Portal')"); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{"q=portal": "proj-7f3a", "q=7F3A": "proj-7f3a", "q=archive": ""} {
		if _, page, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet?"+query, ""); strings.Join(ids(t, page), ",") != want {
			t.Errorf("?%s: %s, want %q", query, lastBody, want)
		}
	}
}

// The newest package running in both environments is production, not preview: the CASE checks
// prod first, and since is the prod deploy's instant, not the preview's.
func TestFleetStageProductionWinsOverPreviewForTheSamePackage(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ('both-envs', 'both-envs');
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_b0th', 'both-envs', 't', 'c', 1, 'upload');
INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_b0th', 'both-envs', 'pkg_b0th', 'clean', now());
INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_at) VALUES
 ('both-envs', 'preview', 'pkg_b0th', 'canyon/both-envs@sha256:aa', 'http://both-envs.preview.localhost:7777', 'gr_b0th', '2026-09-03T10:00:00Z'),
 ('both-envs', 'prod', 'pkg_b0th', 'canyon/both-envs@sha256:aa', 'http://both-envs.prod.localhost:7777', 'gr_b0th', '2026-09-04T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_, page, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet", "")
	latest, _ := page["apps"].([]any)[0].(map[string]any)["latest"].(map[string]any)
	if latest["stage"] != "production" || latest["since"] != "2026-09-04T10:00:00Z" {
		t.Fatalf("latest: %v, want production since the prod deploy", latest)
	}
}

func TestBannerIsDerivedFromTheListenerAndThePeer(t *testing.T) {
	for _, c := range []struct{ listener, peer, want string }{
		{"127.0.0.1:7777", "127.0.0.1:50123", "loopback"}, {"[::1]:7777", "[::1]:50123", "loopback"}, {"127.0.0.1:7777", "[::1]:50123", "loopback"}, {"0.0.0.0:7777", "127.0.0.1:50123", "exposed"}, {"127.0.0.1:7777", "10.0.0.5:50123", "exposed"},
	} {
		if got := banner(c.listener, c.peer); got != c.want {
			t.Errorf("banner(%q, %q) = %s, want %s", c.listener, c.peer, got, c.want)
		}
	}
}

// Behind the compose demo's port mapping the listener is :7777 and the peer the bridge's gateway, so
// the container's own pair reads exposed whatever the host publishes (#1892). With a loopback IP
// published, the banner is that address's alone: loopback from any peer. demo refuses to boot on any
// other published address, so 0.0.0.0 here is the fail-closed fallback to the container's own pair.
func TestBannerBehindAPortMappingIsThePublishedAddress(t *testing.T) {
	for _, c := range []struct{ published, want string }{
		{"127.0.0.1:7777", "loopback"}, {"[::1]:7777", "loopback"}, {"0.0.0.0:7777", "exposed"}, {"", "exposed"},
	} {
		a := &app{auth: authDemo, listen: "[::]:7777", publish: c.published}
		if got := a.banner("10.214.2.1:50123"); got != c.want {
			t.Errorf("published %q: banner %s, want %s", c.published, got, c.want)
		}
	}
	if got := (&app{auth: authOIDC, publish: "127.0.0.1:7777"}).banner("127.0.0.1:1"); got != "none" {
		t.Errorf("an authenticated host's banner is %s, want none", got)
	}
	if got := (&app{auth: authOff, listen: "127.0.0.1:7777", publish: "0.0.0.0:7777"}).banner("127.0.0.1:1"); got != "loopback" { // the published address is the compose demo's alone
		t.Errorf("off on loopback with a published address set: banner %s, want loopback", got)
	}
}

// CANYON_PUBLISHED_ADDR is host:port with a numeric port; anything else fails the boot.
func TestThePublishedAddressIsHostPort(t *testing.T) {
	for v, ok := range map[string]bool{"127.0.0.1:7777": true, "[::1]:7777": true, "": true, "127.0.0.1": false, "127.0.0.1:": false, "127.0.0.1:x": false, "127.0.0.1:70000": false} {
		got, err := publishedAddrOf(v)
		if (err == nil) != ok || (ok && got != v) {
			t.Errorf("%q: %q, %v", v, got, err)
		}
	}
}

// D1: a package whose every blocking finding carries an unexpired approval reads approved, never
// blocked — its own tile and filter, since its last approval — and blocked again once the approval
// expires: the stage is derived, never stored (contracts/routes.md, canyonv2-5r11.10).
func TestEveryBlockerApprovedReadsApprovedUntilTheApprovalExpires(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	blockedRun(t, c, base)
	read := func(query string) (latest, tiles map[string]any, n int) {
		t.Helper()
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet"+query, "")
		apps := page["apps"].([]any)
		if len(apps) > 0 {
			latest = apps[0].(map[string]any)["latest"].(map[string]any)
		}
		return latest, page["tiles"].(map[string]any), len(apps)
	}
	if latest, tiles, _ := read(""); latest["stage"] != "blocked" || tiles["blocked"] != 1.0 || tiles["approved"] != 0.0 {
		t.Fatalf("before the approval: %s", lastBody)
	}
	approve(t, ctx, pool, "now() + interval '1 hour'")
	var at string
	if err := pool.QueryRow(ctx, `SELECT to_char(max(approved_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM approvals`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if latest, tiles, _ := read(""); latest["stage"] != "approved" || latest["since"] != at || tiles["blocked"] != 0.0 || tiles["approved"] != 1.0 {
		t.Fatalf("every blocker approved, at %s: %s", at, lastBody)
	}
	if _, _, n := read("?tile=approved"); n != 1 {
		t.Fatalf("the approved tile selects what it counted: %s", lastBody)
	}
	byAI := func(kind, subject, display string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE approvals SET approved_by_kind = $1, approved_by_subject = $2, approved_by_display = $3", kind, subject, display); err != nil {
			t.Fatal(err)
		}
	}
	byAI("canyon", "ai", "Canyon's AI") // one an earlier Canyon wrote: only a person's approval covers a finding
	if latest, tiles, _ := read(""); latest["stage"] != "blocked" || tiles["approved"] != 0.0 {
		t.Fatalf("an approval by Canyon's AI approved the package: %s", lastBody)
	}
	byAI("person", "dana@meridian.example", "Dana Whitfield")
	if _, err := pool.Exec(ctx, "UPDATE approvals SET approves_blocking = false"); err != nil { // filed outside the approver rule (#2204)
		t.Fatal(err)
	}
	if latest, tiles, _ := read(""); latest["stage"] != "blocked" || tiles["approved"] != 0.0 {
		t.Fatalf("an approval filed outside the approver rule approved the package: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE approvals SET approves_blocking = true, expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if latest, tiles, _ := read(""); latest["stage"] != "blocked" || tiles["approved"] != 0.0 {
		t.Fatalf("after the approval expired: %s", lastBody)
	}
}

// Every blocker, never any: with two blocking findings and one approved, the package stays blocked.
func TestOneOfTwoBlockersApprovedStaysBlocked(t *testing.T) {
	second := "f_" + strings.Repeat("1", 64)
	pipe.Gate = func(ctx context.Context, dir, tree string, snap gatepolicy.Snapshot, report gate.Report) (finding.Log, error) {
		l, err := runstest.Golden(ctx, dir, tree, snap, report)
		for i := range l.Runs {
			for _, r := range l.Runs[i].Results {
				if *r.Properties.Canyon.Blocks { // the hero again, under its own fingerprint
					r.PartialFingerprints.CanyonV1 = second
					l.Runs[i].Results = append(l.Runs[i].Results, r)
					return l, err
				}
			}
		}
		return l, err
	}
	t.Cleanup(func() { pipe.Gate = runstest.Golden })
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	blockedRun(t, c, base)
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approves_blocking)
		SELECT 'claims-portal', f->>'ruleId', f->'partialFingerprints'->>'canyon/v1', 'one of two', now() + interval '1 hour', 'person', 'dana@meridian.example', true
		FROM timeline_rows t, jsonb_path_query(t.data, '$.findings.runs[*].results[*] ? (@.properties.canyon.blocks == true)') f
		WHERE t.kind = 'verdict' AND f->'partialFingerprints'->>'canyon/v1' <> $1`, second); err != nil {
		t.Fatal(err)
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	if stage := page["apps"].([]any)[0].(map[string]any)["latest"].(map[string]any)["stage"]; stage != "blocked" {
		t.Fatalf("one of two blockers approved: %s", lastBody)
	}
}

// The fleet counts its stats over the apps the tiles count, never narrowed by a filter, scope or sort: how
// many, their distinct owners, those deployed to production and to preview, those passed, and their deploys in
// the seven days before the read; a failed app has its tile and its filter; total is every row the filters
// select across the pages (contracts/routes.md, the fleet read).
func TestFleetCountsStatsAFailedTileAndItsTotal(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	c := client(t)
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, project_id) VALUES
	  ('ev_d1', now() - interval '1 day', 'person', 'dana@meridian.example', 'deploy preview', 'claims-portal'),
	  ('ev_d2', now() - interval '2 days', 'person', 'dana@meridian.example', 'deploy prod', 'claims-portal'),
	  ('ev_d3', now() - interval '8 days', 'person', 'dana@meridian.example', 'deploy prod', 'claims-portal'),
	  ('ev_d4', now() - interval '1 hour', 'person', 'dana@meridian.example', 'verdict', 'claims-portal'),
	  ('ev_d5', now() - interval '3 days', 'agent', 'platform', 'platform reported deployed', 'claims-archive'),
	  ('ev_d6', now() - interval '3 days', 'agent', 'platform', 'platform reported deployed', 'claims-archive')`); err != nil { // the org's platform's deploys count too
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET detail = jsonb_build_object('env', CASE id WHEN 'ev_d5' THEN 'prod' ELSE 'preview' END) WHERE id IN ('ev_d5', 'ev_d6')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'failed', failure_reason = 'build' WHERE id = (SELECT id FROM runs WHERE project_id = 'commission-calc' ORDER BY created_at DESC LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"apps": 7.0, "owners": 4.0, "production": 2.0, "preview": 2.0, "passed": 4.0, "deploys_7d": 4.0, "production_7d": 2.0}
	for _, query := range []string{"", "?tile=blocked", "?scope=team", "?sort=name&limit=1"} {
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet"+query, "")
		if !reflect.DeepEqual(page["stats"], want) {
			t.Errorf("%s: stats %v, want %v", query, page["stats"], want)
		}
	}
	// scope=team is the reader's own grants: this host's anonymous admin holds none
	for query, total := range map[string]float64{"": 7, "?limit=2": 7, "?tile=failed": 1, "?tile=blocked&limit=1": 1, "?q=claims": 2, "?scope=team": 0} {
		if _, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet"+query, ""); page["total"] != total {
			t.Errorf("%s: total %v, want %v", query, page["total"], total)
		}
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?tile=failed", "")
	if got := ids(t, page); !reflect.DeepEqual(got, []string{"commission-calc"}) || page["tiles"].(map[string]any)["failed"] != 1.0 {
		t.Fatalf("the failed tile and its filter: %v, tiles %v", got, page["tiles"])
	}
}

// The fleet sorts by risk, by the newest change or by name, and a cursor walks the sort it was issued for alone:
// under each, a walk at limit=2 holds every row once, in that order; a cursor of another sort and an unknown sort or
// scope are refused (contracts/routes.md, the fleet read).
func TestFleetSortsByRiskChangeOrNameAndACursorKeepsItsSort(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedGolden(t, ctx, pool)
	c := client(t)
	walk := func(sort string) []string {
		var walked []string
		url := base + "/api/v1/fleet?limit=2&sort=" + sort
		for {
			_, page, _ := do(t, c, http.MethodGet, url, "")
			walked = append(walked, ids(t, page)...)
			next, ok := page["next"].(string)
			if !ok {
				return walked
			}
			url = base + "/api/v1/fleet?limit=2&sort=" + sort + "&cursor=" + next
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET deployed_at = '2026-08-28T10:03:17Z' WHERE env = 'prod'"); err != nil { // two production apps, one since: id breaks the tie
		t.Fatal(err)
	}
	_, now, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	all := ids(t, now) // the risk order
	if got := walk("risk"); !reflect.DeepEqual(got, all) {
		t.Errorf("risk: %v, want %v", got, all)
	}
	byName := slices.Sorted(slices.Values(all))
	if got := walk("name"); !reflect.DeepEqual(got, byName) {
		t.Errorf("name: %v, want %v", got, byName)
	}
	var since []struct{ id, at string }
	for _, a := range now["apps"].([]any) {
		app := a.(map[string]any)
		since = append(since, struct{ id, at string }{app["id"].(string), app["latest"].(map[string]any)["since"].(string)})
	}
	slices.SortFunc(since, func(a, b struct{ id, at string }) int {
		return cmp.Or(strings.Compare(b.at, a.at), strings.Compare(a.id, b.id))
	})
	var changed []string
	for _, s := range since {
		changed = append(changed, s.id)
	}
	if got := walk("changed"); !reflect.DeepEqual(got, changed) {
		t.Errorf("changed: %v, want %v", got, changed)
	}
	_, first, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?limit=2", "")
	for query, code := range map[string]string{"sort=newest": "invalid_sort", "scope=mine": "invalid_scope", "sort=name&cursor=" + first["next"].(string): "invalid_cursor"} {
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?"+query, ""); status != 400 || body["error"] != code {
			t.Errorf("?%s: %d %v, want 400 %s", query, status, body, code)
		}
	}
}
