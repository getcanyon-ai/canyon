package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// Only the collector's own 2xx is a send that reached it: a redirect is not followed, so the body is never POSTed
// again elsewhere, and the 3xx counts as unreached (#2807's review).
func TestTelemetryFollowsNoRedirect(t *testing.T) {
	var elsewhere int
	there := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer there.Close()
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		hop := httptest.NewServer(http.RedirectHandler(there.URL, code))
		reached, err := telemetryPost(t.Context(), hop.URL, []byte(`{}`))
		hop.Close()
		if reached || err != nil {
			t.Errorf("a %d: reached %v, %v", code, reached, err)
		}
	}
	if elsewhere != 0 {
		t.Errorf("the report followed a redirect %d times", elsewhere)
	}
}

// rules_fired names only what firedAs can yield: the canonical rules, the tools Canyon runs itself, and custom. The
// schema's enum is exactly that list, so the seam refuses any other name and a new built-in rule fails here until
// the schema names it.
func TestTheSchemaNamesExactlyTheRulesFiredAsYields(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/schema/telemetry.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			RulesFired struct {
				PropertyNames struct {
					Enum []string `json:"enum"`
				} `json:"propertyNames"`
			} `json:"rules_fired"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	snap, err := (&gatepolicy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"custom"}
	for _, r := range snap.Rules {
		want = append(want, firedAs(r.ID))
	}
	for tool := range ranTools {
		want = append(want, firedAs(tool))
	}
	slices.Sort(want)
	want = slices.Compact(want)
	got := slices.Sorted(slices.Values(schema.Properties.RulesFired.PropertyNames.Enum))
	if !slices.Equal(got, want) {
		t.Errorf("rules_fired's names are %v; firedAs yields %v", got, want)
	}
}

// Canyon's own test stacks never send a report: CI's environment never reaches their servers, so each sets
// CANYON_TELEMETRY=0 itself — the E2E stack's two servers, which the browser journeys run on, and both kind
// clusters, the manifests' and the chart's (#2807's review).
func TestCanyonsOwnStacksSendNothing(t *testing.T) {
	raw, err := os.ReadFile("../../e2e/compose.e2e.yml")
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Environment []string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"server", "canyon-b"} {
		if !slices.Contains(compose.Services[name].Environment, "CANYON_TELEMETRY=0") {
			t.Errorf("e2e/compose.e2e.yml's %s does not set CANYON_TELEMETRY=0", name)
		}
	}
	for file, line := range map[string]string{
		"../../.github/kind/up.sh":    `  --from-literal=CANYON_TELEMETRY=0 \`,
		"../../.github/kind/chart.sh": `  echo "telemetry: {enabled: false}"`,
	} {
		raw, err := os.ReadFile(file) //nolint:gosec // test: the repository's own scripts
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(strings.Split(string(raw), "\n"), line) {
			t.Errorf("%s has no line %q", file, line)
		}
	}
}

// The report goes to Canyon's real collector, over HTTPS, at /v1/report: never a placeholder host, which would make
// every install's first send unreachable and turn telemetry off on all of them (#2820).
func TestTheCollectorIsReal(t *testing.T) {
	u, err := url.Parse(telemetryEndpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || strings.HasSuffix(u.Hostname(), ".invalid") || u.Path != "/v1/report" {
		t.Errorf("telemetryEndpoint %q: want https://<the collector's host>/v1/report (%v)", telemetryEndpoint, err)
	}
}

// The environment turns telemetry off whatever an admin sets: CANYON_TELEMETRY=0, DO_NOT_TRACK=1, or CI set to
// anything at all. An install mode the schema does not name refuses the boot.
func TestTelemetryConfigOf(t *testing.T) {
	for _, c := range []struct {
		env map[string]string
		off bool
	}{
		{nil, false},
		{map[string]string{"CANYON_TELEMETRY": "0"}, true},
		{map[string]string{"CANYON_TELEMETRY": "1"}, false},
		{map[string]string{"DO_NOT_TRACK": "1"}, true},
		{map[string]string{"CI": "true"}, true},
		{map[string]string{"CI": "1"}, true},
	} {
		got, err := telemetryConfigOf(func(k string) string { return c.env[k] })
		if err != nil || got.envOff != c.off || got.url != telemetryEndpoint {
			t.Errorf("%v: off %v, url %q, %v; want off %v", c.env, got.envOff, got.url, err, c.off)
		}
	}
	if c, err := telemetryConfigOf(func(k string) string { return map[string]string{"CANYON_INSTALL_MODE": "helm_one_node"}[k] }); err != nil || c.mode != "helm_one_node" {
		t.Errorf("helm_one_node: %q %v", c.mode, err)
	}
	if _, err := telemetryConfigOf(func(k string) string { return map[string]string{"CANYON_INSTALL_MODE": "acme-prod-cluster"}[k] }); err == nil {
		t.Error("an install mode the schema does not name booted")
	}
}

// A built-in rule is counted by its own id, a rule that extends it by the built-in's, and every other rule, a custom
// one first, only as custom: a name an org wrote never leaves the server.
func TestFiredAsNamesOnlyBuiltInRules(t *testing.T) {
	for rule, want := range map[string]string{
		"secrets.client-bundle":     "secrets.client-bundle",
		"iac.misconfig.trivy.DS026": "iac.misconfig",
		"custom.acme-payroll-scan":  "custom",
		"acme.payroll":              "custom",
		"secrets.client-bundlex":    "custom",
		"sarif.grype":               "sarif.grype", // a tool Canyon runs itself, which an admin turns on: built in
		"sarif.grype.CVE-2024-1234": "sarif.grype",
		"sarif.acme-scanner":        "custom", // a tool an operator imported at runtime: the data, never the registry
		"sarif.acme-scanner.X1":     "custom",
		"":                          "custom",
	} {
		if got := firedAs(rule); got != want {
			t.Errorf("%q counts as %q, want %q", rule, got, want)
		}
	}
}

func TestP50IsTheLowerMedian(t *testing.T) {
	for _, c := range []struct {
		xs   []int
		want int
	}{{nil, 0}, {[]int{7}, 7}, {[]int{9, 3}, 3}, {[]int{5, 1, 9}, 5}, {[]int{4, 1, 3, 2}, 2}} {
		if got := p50(c.xs); got != c.want {
			t.Errorf("p50(%v) = %d, want %d", c.xs, got, c.want)
		}
	}
}

// strings walks every key and string value of a decoded JSON document.
func jsonStrings(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			*out = append(*out, k)
			jsonStrings(e, out)
		}
	case []any:
		for _, e := range x {
			jsonStrings(e, out)
		}
	case string:
		*out = append(*out, x)
	}
}

// planted are the fixture's names, hosts, emails and words: none may appear anywhere in a report.
var planted = []string{"acme", "payroll", "claims-portal", "ana.ruiz", "meridian", "db.internal", "10.20.30.40", "hunter2", "STRIPE_KEY", "/srv/app"}

// telemetryFixture is an org full of names: an app named after its company, a blocked review of a custom rule and a
// built-in one whose findings quote an email, a host, an IP and a secret's name, the fix that passed after it, a
// review whose Dockerfile Canyon didn't read, and a build no buildpack recognised.
func telemetryFixture(t *testing.T) (base string, c *http.Client) {
	t.Helper()
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"acme-payroll"}`); code != 201 { // a new app takes an id or a name, not both
		t.Fatalf("the second app: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/acme-payroll/name", `{"name":"ACME Payroll at db.internal"}`); code != 200 {
		t.Fatalf("naming the second app: %d %s", code, lastBody)
	}
	_, up := upload(t, c, base, "acme-payroll", tarOf(t, map[string]string{"srv/app/main.go": "package main"}), "", "")
	pkg := packageOf(up)["id"]
	finding := func(rule string) string {
		return fmt.Sprintf(`{"ruleId": %q, "message": {"text": "STRIPE_KEY=hunter2 for ana.ruiz@acme.example on db.internal (10.20.30.40) in /srv/app/main.go"}}`, rule)
	}
	blocked := `{"state": "blocked", "blocking": 2, "warnings": 0, "findings": {"runs": [{"tool": {"driver": {"name": "canyon/scanners"}}, "results": [` +
		finding("custom.acme-payroll-scan") + ", " + finding("secrets.client-bundle") + `]}]}}`
	clean := `{"state": "clean", "blocking": 0, "warnings": 0, "findings": {"runs": []}}`
	note := `{"producer": "canyon/scanners", "sarif": {"runs": [{"invocations": [{"toolExecutionNotifications": [{"level": "note", "message": {"text": "` + dockerfileNote + `"}}]}], "results": []}]}}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`WITH r AS (INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_acme1', 'acme-payroll', $1, 'blocked', now() - interval '40 minutes', now() - interval '39 minutes', 'person', 'ana.ruiz@acme.example', 'Ana Ruiz'))
			INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_acme1', 'acme-payroll', 'verdict', 'verdict', $2)`, []any{pkg, blocked}},
		{`WITH r AS (INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_acme2', 'acme-payroll', $1, 'clean', now() - interval '10 minutes', now() - interval '9 minutes', 'person', 'ana.ruiz@acme.example', 'Ana Ruiz'))
			INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_acme2', 'acme-payroll', 'verdict', 'verdict', $2), ('run', 'gr_acme2', 'acme-payroll', 'producer.sarif', 'sarif:canyon/scanners', $3)`, []any{pkg, clean, note}},
		{`INSERT INTO runs (id, project_id, package_id, state, failure_reason, created_at, ended_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_acme3', 'acme-payroll', $1, 'failed', 'detect', now() - interval '5 minutes', now() - interval '4 minutes', 'person', 'ana.ruiz@acme.example', 'Ana Ruiz')`, []any{pkg}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return base, c
}

// The report counts the fixture and names nothing of it: walking every key and string of the next report finds none
// of the org's names, hosts, emails, addresses, secrets or paths, the custom rule counts only as custom, and the
// report is the schema's (#2807).
func TestTelemetryCarriesNothingOfTheOrgs(t *testing.T) {
	base, c := telemetryFixture(t)
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/telemetry", ""); code != 200 {
		t.Fatalf("GET /telemetry: %d %s", code, lastBody)
	}
	var state struct {
		Report string `json:"report"` // the string of the send's exact bytes (contracts/routes.md, Telemetry)
	}
	if err := json.Unmarshal([]byte(lastBody), &state); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(state.Report), &report); err != nil {
		t.Fatal(err)
	}
	if err := contractSchema(t, "telemetry.schema.json").Validate(report); err != nil {
		t.Errorf("the report is not the schema's: %v\n%s", err, state.Report)
	}
	var said []string
	jsonStrings(report, &said)
	for _, s := range said {
		for _, p := range planted {
			if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
				t.Errorf("the report carries %q in %q:\n%s", p, s, state.Report)
			}
		}
	}
	want := map[string]any{"apps": 2.0, "checks": map[string]any{"passed": 1.0, "advisory": 0.0, "blocked": 1.0},
		"milestones": []any{"first_check", "first_block", "first_fix"}, "fixes": map[string]any{"count": 1.0, "minutes_p50": 31.0, "attempts_p50": 2.0},
		"rules_fired": map[string]any{"secrets.client-bundle": 1.0, "custom": 1.0}, "unsupported": map[string]any{"language_unrecognised": 1.0, "own_dockerfile_ignored": 1.0}}
	for k, v := range want {
		if !reflect.DeepEqual(report[k], v) {
			t.Errorf("%s is %v, want %v", k, report[k], v)
		}
	}
}

// ai_review is whether the AI review runs: on in the policy, and a model set up on the host (#2843). The same policy
// on a host with no model reports false, as its reviews say "skipped, no AI model set up".
func TestTelemetrysAIReviewNeedsAModel(t *testing.T) {
	t.Cleanup(func() { testReviews = false })
	for _, model := range []bool{false, true} {
		testReviews = model
		_, _, base, c := withProject(t)
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/telemetry", ""); code != 200 {
			t.Fatalf("GET /telemetry: %d %s", code, lastBody)
		}
		var state struct{ Report string }
		var report struct {
			AIReview bool `json:"ai_review"`
		}
		if err := json.Unmarshal([]byte(lastBody), &state); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(state.Report), &report); err != nil || report.AIReview != model {
			t.Errorf("a host whose model is set up %v reports ai_review %v (%v): %s", model, report.AIReview, err, state.Report)
		}
	}
}

// The next report and the switch are an admin's: a member is refused both, 403. It is on by default, with no admin
// having to see it first, and there is no route to say one has (the owner's decision, 2026-11-12). Turning it on
// starts the sends again, so it is a person's in the dashboard: the admin's CLI session is 403 dashboard_required,
// and turns it off as a browser does (#2807). Off is an admin's off, on lifts it, and the
// server's environment keeps it off whatever an admin sets. A body that is not {enabled} is 400.
func TestTelemetryIsAnAdminsReadAndSwitch(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	at := base + "/api/v1/telemetry"
	for _, c := range []struct{ method, url string }{{http.MethodGet, at}, {http.MethodPut, at}} {
		if code, _, _ := do(t, member, c.method, c.url, `{"enabled":false}`); code != 403 || lastBody != adminRequired {
			t.Errorf("a member's %s %s: %d %s", c.method, c.url, code, lastBody)
		}
	}
	if code, body, _ := do(t, dana, http.MethodGet, at, ""); code != 200 || body["enabled"] != true || body["off_because"] != nil {
		t.Fatalf("a new install's read: on by default, %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPost, at+"/seen", ""); code != 404 && code != 405 {
		t.Errorf("POST …/seen is no route any more: %d %s", code, lastBody)
	}
	danaCLI := cliIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example")
	if code, body, _ := do(t, dana, http.MethodPut, at, `{"enabled":false}`); code != 200 || body["enabled"] != false || body["off_because"] != "admin" {
		t.Fatalf("off: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, danaCLI, http.MethodPut, at, `{"enabled":true}`); code != 403 || lastBody != dashboardRequired {
		t.Fatalf("on from the admin's CLI session: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodPut, at, `{"enabled":true}`); code != 200 || body["enabled"] != true {
		t.Fatalf("on: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, danaCLI, http.MethodPut, at, `{"enabled":false}`); code != 200 || body["off_because"] != "admin" {
		t.Fatalf("off from the admin's CLI session: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, at, `{"enabled":true}`); code != 200 {
		t.Fatalf("on again: %d %s", code, lastBody)
	}
	for _, bad := range []string{`{}`, `{"enabled":"no"}`, `[]`} {
		if code, _, _ := do(t, dana, http.MethodPut, at, bad); code != 400 {
			t.Errorf("%s: %d %s", bad, code, lastBody)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/telemetry'`).Scan(&rows); err != nil || rows != 4 {
		t.Errorf("%d audit rows for four switches: %v", rows, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'dashboard_required'"); n != 1 {
		t.Errorf("%d denied rows for the CLI session's one refusal", n)
	}
	appOf(t, base).telemetry.envOff = true
	if code, body, _ := do(t, dana, http.MethodPut, at, `{"enabled":true}`); code != 200 || body["enabled"] != false || body["off_because"] != "env" {
		t.Errorf("on under the environment's off: %d %s", code, lastBody)
	}
}

// collector stands in for Canyon's collector: it counts the POSTs and keeps the last body.
type collector struct {
	mu    sync.Mutex
	posts int
	body  []byte
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts, c.body = c.posts+1, body
}

// On by default: the first day's send goes with no admin having seen anything (the owner's decision, 2026-11-12), one
// POST a day whose body is byte for byte the report an admin reads; the environment's off sends nothing; a first send
// that reaches nothing turns it off and tries no more, until an admin turns it on (#2807).
func TestTelemetrySendsOnceADayAndOnlyWhatWasShown(t *testing.T) {
	base, c := telemetryFixture(t)
	a := appOf(t, base)
	got := &collector{}
	srv := httptest.NewServer(got)
	defer srv.Close()
	a.telemetry.url = srv.URL
	ctx := t.Context()
	do(t, c, http.MethodGet, base+"/api/v1/telemetry", "")
	var shown struct {
		Report string `json:"report"`
	}
	if err := json.Unmarshal([]byte(lastBody), &shown); err != nil {
		t.Fatal(err)
	}
	a.telemetry.envOff = true
	if sent, _ := a.sendTelemetry(ctx); sent || got.posts != 0 {
		t.Fatalf("the environment's off still sent: %d", got.posts)
	}
	a.telemetry.envOff = false
	if sent, err := a.sendTelemetry(ctx); !sent || err != nil || got.posts != 1 || string(got.body) != shown.Report {
		t.Fatalf("the day's send: %v %v, %d posts\nsent  %s\nshown %s", sent, err, got.posts, got.body, shown.Report)
	}
	if sent, _ := a.sendTelemetry(ctx); sent || got.posts != 1 {
		t.Errorf("a second send the same day: %d posts", got.posts)
	}
	do(t, c, http.MethodGet, base+"/api/v1/telemetry", "")
	var after struct {
		LastReport *string `json:"last_report"`
		LastSentAt *string `json:"last_sent_at"`
	}
	if err := json.Unmarshal([]byte(lastBody), &after); err != nil || after.LastSentAt == nil || after.LastReport == nil || *after.LastReport != string(got.body) {
		t.Errorf("the last report shown is not the one sent: %v\n%v\nsent %s", err, after.LastReport, got.body)
	}
	pool := poolOf(t, base)
	if _, err := pool.Exec(ctx, "UPDATE telemetry SET claimed_at = NULL, sent_at = NULL"); err != nil { // a new install's first day
		t.Fatal(err)
	}
	srv.Close() // the collector is unreachable: an air-gapped install
	if sent, err := a.sendTelemetry(ctx); !sent || err != nil {
		t.Fatalf("the unreachable first send: %v %v", sent, err)
	}
	var off string
	if err := pool.QueryRow(ctx, "SELECT coalesce(off_because, '') FROM telemetry").Scan(&off); err != nil || off != "unreachable" {
		t.Fatalf("after an unreachable first send, off_because %q %v", off, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE telemetry SET claimed_at = NULL"); err != nil {
		t.Fatal(err)
	}
	if sent, _ := a.sendTelemetry(ctx); sent {
		t.Error("an unreachable install tried again")
	}
	if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/telemetry", `{"enabled":true}`); code != 200 || body["enabled"] != true {
		t.Errorf("an admin turning it back on: %d %s", code, lastBody)
	}
}

// Only a 2xx from the collector is a send that reached it: a proxy between the server and an unresolvable collector
// answers 502 itself, and that first send is unreachable like a refused dial, never a report marked sent.
func TestTelemetryCountsOnlyTheCollectorsYes(t *testing.T) {
	base, _ := telemetryFixture(t)
	a := appOf(t, base)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer proxy.Close()
	a.telemetry.url = proxy.URL
	if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
		t.Fatalf("the first send: %v %v", sent, err)
	}
	var off string
	var sent bool
	if err := poolOf(t, base).QueryRow(t.Context(), "SELECT coalesce(off_because, ''), sent_at IS NOT NULL OR last_report IS NOT NULL FROM telemetry").Scan(&off, &sent); err != nil || off != "unreachable" || sent {
		t.Errorf("a 502 first send: off_because %q, marked sent %v, %v", off, sent, err)
	}
}
