package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
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
		if err != nil || got.envOff != c.off || got.url != telemetryEndpoint || got.every != telemetryEvery {
			t.Errorf("%v: off %v, url %q, every %v, %v; want off %v", c.env, got.envOff, got.url, got.every, err, c.off)
		}
	}
	if c, err := telemetryConfigOf(func(k string) string { return map[string]string{"CANYON_INSTALL_MODE": "helm_one_node"}[k] }); err != nil || c.mode != "helm_one_node" {
		t.Errorf("helm_one_node: %q %v", c.mode, err)
	}
	if _, err := telemetryConfigOf(func(k string) string { return map[string]string{"CANYON_INSTALL_MODE": "acme-prod-cluster"}[k] }); err == nil {
		t.Error("an install mode the schema does not name booted")
	}
}

// telemetryOffVars are the variables that turn telemetry off on a server.
var telemetryOffVars = []string{"CANYON_TELEMETRY", "DO_NOT_TRACK", "CI"}

// shippedEnv decodes a YAML file into v.
func shippedEnv(t *testing.T, file string, v any) {
	t.Helper()
	raw, err := os.ReadFile(file) //nolint:gosec // test: the repository's own files
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// composeServerEnv is the server's environment in a compose file, NAME=value each.
func composeServerEnv(t *testing.T, file string) map[string]string {
	t.Helper()
	var compose struct {
		Services map[string]struct {
			Environment []string `yaml:"environment"`
			EnvFile     any      `yaml:"env_file"`
		} `yaml:"services"`
	}
	shippedEnv(t, file, &compose)
	server := compose.Services["server"]
	if len(server.Environment) == 0 || server.EnvFile != nil {
		t.Fatalf("%s: the server has %d variables and env_file %v; want its environment in the file", file, len(server.Environment), server.EnvFile)
	}
	env := map[string]string{}
	for _, kv := range server.Environment {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	return env
}

// manifestServerEnv is the server container's own environment in deploy/k8s/server.yaml.
func manifestServerEnv(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/k8s/server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for docs := yaml.NewDecoder(bytes.NewReader(raw)); ; {
		var d struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct{ Name, Value string } `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := docs.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil && d.Kind == "Deployment" {
			t.Fatal(err)
		}
		if d.Kind == "Deployment" {
			for _, c := range d.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					env[e.Name] = e.Value
				}
			}
		}
	}
	if len(env) == 0 {
		t.Fatal("deploy/k8s/server.yaml: no server environment found")
	}
	return env
}

// Telemetry is on by default on every install Canyon ships (the owner's decision, 2026-11-12): the environment each
// gives its server sets none of the variables that turn it off, reads as a sender that runs, and says how it was
// installed. The chart's own render is deploy/helm/check-rendered.py's.
func TestEveryShippedInstallSendsByDefault(t *testing.T) {
	demo, team := composeServerEnv(t, "../../docker-compose.yml"), composeServerEnv(t, "../../deploy/docker/team.yml")
	merged := maps.Clone(demo)
	maps.Copy(merged, team) // as compose merges the two, each entry by its name
	for install, s := range map[string]struct {
		env  map[string]string
		mode string
	}{
		"docker-compose.yml":     {demo, "laptop"},
		"deploy/docker/team.yml": {merged, "one_server"},
		"deploy/k8s/server.yaml": {manifestServerEnv(t), ""}, // the operator's Secret says it: the manifest's own env would win over it
	} {
		for _, name := range telemetryOffVars {
			if v, set := s.env[name]; set {
				t.Errorf("%s sets %s=%q on the server: telemetry is on by default", install, name, v)
			}
		}
		c, err := telemetryConfigOf(func(k string) string { return s.env[k] })
		if err != nil || c.envOff || c.url != telemetryEndpoint || c.every != telemetryEvery {
			t.Errorf("%s: telemetry off %v, url %q, every %v, %v", install, c.envOff, c.url, c.every, err)
		}
		if c.mode != s.mode {
			t.Errorf("%s says it was installed by %q, want %q", install, c.mode, s.mode)
		}
	}
	var values struct {
		Telemetry struct {
			Enabled any `yaml:"enabled"`
		} `yaml:"telemetry"`
	}
	if shippedEnv(t, "../../deploy/helm/canyon/values.yaml", &values); values.Telemetry.Enabled != true {
		t.Errorf("the chart's telemetry.enabled is %v, want true", values.Telemetry.Enabled)
	}
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, word := range strings.FieldsFunc(line, func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if slices.Contains(telemetryOffVars, word) {
				t.Errorf("the image's Dockerfile names %s: %s", word, strings.TrimSpace(line))
			}
		}
	}
}

// A server whose environment sets nothing sends its first report as it starts: serve runs the sender, at the interval
// telemetryConfigOf gives it.
func TestAServerSendsItsFirstReportAsItStarts(t *testing.T) {
	got := &collector{}
	srv := httptest.NewServer(got)
	defer srv.Close()
	c, err := telemetryConfigOf(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c.url = srv.URL
	testTelemetry = c
	t.Cleanup(func() { testTelemetry = telemetryConfig{} })
	pool, _ := migrated(t)
	start(t, pool, time.Hour)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got.mu.Lock()
		posts, body := got.posts, got.body
		got.mu.Unlock()
		if posts > 0 {
			if err := contractSchema(t, "telemetry.schema.json").Validate(anyOf(t, body)); err != nil {
				t.Errorf("the first report is not the schema's: %v\n%s", err, body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("10 seconds after it started, the server has sent no report")
		}
	}
}

// anyOf is a JSON document, decoded.
func anyOf(t *testing.T, raw []byte) (v any) {
	t.Helper()
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
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
// POST a day whose body is byte for byte the report an admin reads; the environment's off sends nothing; a later day's
// send that reaches nothing turns nothing off (#2807).
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
	for _, on := range []string{`{"enabled":false}`, `{"enabled":true}`} { // off and on again the same day sends no second report
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/telemetry", on); code != 200 {
			t.Fatalf("%s: %d %s", on, code, lastBody)
		}
	}
	if sent, _ := a.sendTelemetry(ctx); sent || got.posts != 1 {
		t.Errorf("turned off and on after the day's send, it sent again: %d posts", got.posts)
	}
	srv.Close() // a later day's failure is that day's alone
	clockOn(t, poolOf(t, base), 24*time.Hour)
	if sent, err := a.sendTelemetry(ctx); !sent || err != nil {
		t.Fatalf("the next day's send, which reaches nothing: %v %v", sent, err)
	}
	if off, tried := telemetryOffAnd(t, poolOf(t, base)); off != "" || tried {
		t.Errorf("after a later day's failure: off_because %q, trying again %v", off, tried)
	}
}

// clockOn is the fake clock: the database's clock d on, its row's times d back.
func clockOn(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), "UPDATE telemetry SET claimed_at = claimed_at - $1::interval, tried_since = tried_since - $1::interval, sent_at = sent_at - $1::interval", d); err != nil {
		t.Fatal(err)
	}
}

// telemetryOffAnd is why the row is off, "" for on, and whether it is trying for its first report.
func telemetryOffAnd(t *testing.T, pool *pgxpool.Pool) (off string, tried bool) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), "SELECT coalesce(off_because, ''), tried_since IS NOT NULL FROM telemetry").Scan(&off, &tried); err != nil {
		t.Fatal(err)
	}
	return off, tried
}

// unreached is a collector's URL that nothing answers.
func unreached(t *testing.T) string {
	t.Helper()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	return gone.URL
}

// firstTries are the hours after the first at which a server that reaches nothing tries: doubling, at most a week
// apart, for 90 days.
var firstTries = []int{0, 1, 3, 7, 15, 31, 63, 127, 255, 423, 591, 759, 927, 1095, 1263, 1431, 1599, 1767, 1935, 2103}

// The schedule (the owner, 2026-11-23): the waits double from an hour, stop at a week, and end 90 days after the first
// try. A try that came late only waits longer, never less.
func TestTelemetryNextDoublesToAWeekForNinetyDays(t *testing.T) {
	first := time.Date(2026, 11, 23, 14, 4, 0, 0, time.UTC)
	for _, late := range []time.Duration{0, time.Hour} {
		var tries []int
		for last := first; ; {
			tries = append(tries, int(last.Sub(first)/time.Hour))
			next, until := telemetryNext(first, last)
			if until != first.Add(90*24*time.Hour) {
				t.Fatalf("it ends %v after the first try, want 90 days", until.Sub(first))
			}
			if next.After(until) {
				break
			}
			if wait := next.Sub(last); wait < time.Hour || wait > 7*24*time.Hour {
				t.Fatalf("a wait of %v after the try at hour %d", wait, tries[len(tries)-1])
			}
			last = next.Add(late)
		}
		if late == 0 && !slices.Equal(tries, firstTries) {
			t.Errorf("tries at hours %v, want %v", tries, firstTries)
		}
		if len(tries) > 20 {
			t.Errorf("%d tries with each %v late, want at most 20", len(tries), late)
		}
	}
}

// A server that never reaches the collector tries for 90 days, then goes quiet by itself until an admin turns it on:
// looked at again right after each look, as a restart or a second replica does, it makes exactly its 20 tries.
func TestTelemetryTriesForNinetyDaysThenGoesQuiet(t *testing.T) {
	base, c := telemetryFixture(t)
	a, pool := appOf(t, base), poolOf(t, base)
	a.telemetry.url = unreached(t)
	// Looked at within an hour of each try, where a look could try early or late, and once a day between.
	near := func(hour int) bool {
		return hour%24 == 0 || slices.ContainsFunc(firstTries, func(try int) bool { return hour >= try-1 && hour <= try+1 })
	}
	var tries []int
	for hour := range 91 * 24 {
		if clockOn(t, pool, time.Hour); !near(hour) {
			continue
		}
		for look := range 2 {
			sent, err := a.sendTelemetry(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if sent && look == 0 {
				tries = append(tries, hour)
			} else if sent {
				t.Fatalf("a second look at hour %d tried again", hour)
			}
		}
		if off, _ := telemetryOffAnd(t, pool); off != map[bool]string{false: "", true: "unreachable"}[hour >= firstTries[len(firstTries)-1]] {
			t.Fatalf("at hour %d, off_because %q", hour, off)
		}
	}
	if !slices.Equal(tries, firstTries) {
		t.Errorf("tries at hours %v, want %v", tries, firstTries)
	}
	// An admin turning it on starts the same tries afresh; on while it is on already starts nothing.
	for range 2 {
		if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/telemetry", `{"enabled":true}`); code != 200 || body["enabled"] != true {
			t.Fatalf("turning it on: %d %s", code, lastBody)
		}
	}
	tries = nil
	for hour := range 8 {
		if sent, err := a.sendTelemetry(t.Context()); err != nil {
			t.Fatal(err)
		} else if sent {
			tries = append(tries, hour)
		}
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/telemetry", `{"enabled":true}`); code != 200 {
			t.Fatalf("turning it on while it tries: %d %s", code, lastBody)
		}
		if sent, _ := a.sendTelemetry(t.Context()); sent {
			t.Fatalf("at hour %d, turning on what is on tried again", hour)
		}
		clockOn(t, pool, time.Hour)
	}
	if !slices.Equal(tries, []int{0, 1, 3, 7}) {
		t.Errorf("after an admin turned it on, tries at hours %v, want 0, 1, 3, 7", tries)
	}
}

// While no report has reached the collector, the state says when it tries next and when it gives up; a first report
// that reaches it, whenever it comes, ends the tries: it counts from the first try, and it is daily from then on, where
// a later failure turns nothing off.
func TestTelemetryOutlivesASlowFirstBoot(t *testing.T) {
	base, c := telemetryFixture(t)
	a, pool := appOf(t, base), poolOf(t, base)
	a.telemetry.url = unreached(t)
	for _, wait := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		clockOn(t, pool, wait)
		if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
			t.Fatalf("a try %v after the one before: %v %v", wait, sent, err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/telemetry", "")
		if code != 200 {
			t.Fatalf("GET /telemetry: %d %s", code, lastBody)
		}
		if err := contractSchema(t, "telemetry.schema.json#/definitions/state").Validate(body); err != nil {
			t.Errorf("the state is not the schema's: %v\n%s", err, lastBody)
		}
		return body
	}
	body := read()
	next, err := time.Parse(time.RFC3339, fmt.Sprint(body["next_try_at"]))
	until, err2 := time.Parse(time.RFC3339, fmt.Sprint(body["gives_up_at"]))
	if err != nil || err2 != nil || body["enabled"] != true || time.Until(next).Round(time.Minute) != 4*time.Hour || time.Until(until).Round(time.Minute) != 90*24*time.Hour-3*time.Hour {
		t.Errorf("after the third try, at hour 3: next try %v, gives up %v, enabled %v; want in 4 hours, 90 days from the first, on", body["next_try_at"], body["gives_up_at"], body["enabled"])
	}
	got := &collector{}
	srv := httptest.NewServer(got)
	a.telemetry.url = srv.URL
	clockOn(t, pool, 4*time.Hour)
	if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil || got.posts != 1 {
		t.Fatalf("the try at hour 7: %v %v, %d reports reached the collector", sent, err, got.posts)
	}
	var report struct {
		PeriodHours int `json:"period_hours"`
	}
	if err := json.Unmarshal(got.body, &report); err != nil || report.PeriodHours != 7 {
		t.Errorf("the first report's period_hours %d (%v), want 7, from the first try", report.PeriodHours, err)
	}
	if body := read(); strings.Contains(lastBody, "next_try_at") || strings.Contains(lastBody, "gives_up_at") || body["last_sent_at"] == nil { // absent, never null
		t.Errorf("after it reached the collector: %s", lastBody)
	}
	srv.Close()
	for day := range 3 {
		clockOn(t, pool, time.Hour)
		if sent, _ := a.sendTelemetry(t.Context()); sent {
			t.Fatalf("an hour after day %d's send it sent again", day)
		}
		clockOn(t, pool, 23*time.Hour)
		if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
			t.Fatalf("day %d's send: %v %v", day+1, sent, err)
		}
		if off, tried := telemetryOffAnd(t, pool); off != "" || tried {
			t.Fatalf("day %d's failure: off_because %q, trying again %v", day+1, off, tried)
		}
	}
}

// Replicas that look at once make one try between them, the first and a later one alike.
func TestTelemetryIsTriedByOneReplica(t *testing.T) {
	base, _ := telemetryFixture(t)
	a, pool := appOf(t, base), poolOf(t, base)
	a.telemetry.url = unreached(t)
	for _, wait := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		clockOn(t, pool, wait)
		var tries atomic.Int32
		var replicas sync.WaitGroup
		for range 6 {
			replicas.Go(func() {
				if sent, err := a.sendTelemetry(t.Context()); err != nil {
					t.Error(err)
				} else if sent {
					tries.Add(1)
				}
			})
		}
		if replicas.Wait(); tries.Load() != 1 {
			t.Errorf("%v on, six replicas made %d tries, want 1", wait, tries.Load())
		}
	}
}

// A server that stopped mid-try never tries sooner for it, and one started after the 90 days turns it off and tries
// nothing. A row off as unreachable, as an earlier release left it, stays off.
func TestTelemetryKeepsTheScheduleAcrossRestarts(t *testing.T) {
	base, c := telemetryFixture(t)
	a, pool := appOf(t, base), poolOf(t, base)
	got := &collector{}
	srv := httptest.NewServer(got)
	defer srv.Close()
	a.telemetry.url = srv.URL
	row := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, _, err := telemetryRow(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	row("UPDATE telemetry SET claimed_at = now(), tried_since = now() - interval '3 hours'") // claimed at hour 3, and stopped before the POST
	for _, wait := range []time.Duration{0, time.Minute, 3*time.Hour + 58*time.Minute} {
		clockOn(t, pool, wait)
		if sent, err := a.sendTelemetry(t.Context()); sent || err != nil {
			t.Fatalf("started %v on, under its 4-hour wait, it tried: %v", wait, err)
		}
	}
	row("UPDATE telemetry SET claimed_at = now() - interval '91 days', tried_since = now() - interval '91 days'")
	if sent, err := a.sendTelemetry(t.Context()); sent || err != nil {
		t.Fatalf("started after the 90 days, it tried: %v", err)
	}
	if off, _ := telemetryOffAnd(t, pool); off != "unreachable" {
		t.Errorf("started after the 90 days: off_because %q", off)
	}
	row("UPDATE telemetry SET off_because = 'admin', claimed_at = now() - interval '100 days', tried_since = now() - interval '100 days'") // an admin's off, during the tries
	if sent, _ := a.sendTelemetry(t.Context()); sent {
		t.Error("an admin's off was tried")
	}
	if off, _ := telemetryOffAnd(t, pool); off != "admin" {
		t.Errorf("100 days after an admin turned it off as it tried: off_because %q", off)
	}
	row("UPDATE telemetry SET off_because = 'unreachable', claimed_at = now() - interval '200 days', tried_since = NULL, sent_at = NULL") // as v0.11.0 left it
	if sent, err := a.sendTelemetry(t.Context()); sent || err != nil || got.posts != 0 {
		t.Errorf("a row off as unreachable: sent %v %v, %d posts", sent, err, got.posts)
	}
	// Rolled back to v0.11.0 as it tried, which sends daily and knows no tried_since, then upgraded: it has reported, so
	// it is daily and no 90 days end it.
	row("UPDATE telemetry SET off_because = NULL, claimed_at = now() - interval '24 hours', tried_since = now() - interval '91 days', sent_at = now() - interval '24 hours'")
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/telemetry", ""); code != 200 || strings.Contains(lastBody, "next_try_at") {
		t.Errorf("a row that has reported since its tries reads as trying: %d %s", code, lastBody)
	}
	if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil || got.posts != 1 {
		t.Errorf("a row that has reported since its tries: sent %v %v, %d posts", sent, err, got.posts)
	}
	if off, tried := telemetryOffAnd(t, pool); off != "" || tried {
		t.Errorf("a row that has reported since its tries, after a send: off_because %q, trying again %v", off, tried)
	}
}

// A look may come half a minute short of a wait, so a tick a moment early skips no hour, and no more.
func TestTelemetryLooksToTheHalfMinute(t *testing.T) {
	base, _ := telemetryFixture(t)
	a, pool := appOf(t, base), poolOf(t, base)
	a.telemetry.url = unreached(t)
	for _, c := range []struct {
		wait time.Duration
		due  bool
	}{{0, true}, {59 * time.Minute, false}, {40 * time.Second, true}} { // an hour less 20 seconds, at the last
		clockOn(t, pool, c.wait)
		if sent, err := a.sendTelemetry(t.Context()); sent != c.due || err != nil {
			t.Errorf("%v on: tried %v %v, want %v", c.wait, sent, err, c.due)
		}
	}
}

// What changes the row while the last try is on its way stands: a try that reaches the collector is on whatever another
// replica gave up on meanwhile, and a failed one turns off neither an admin's off nor the tries an admin started afresh.
func TestTelemetryKeepsWhatChangesDuringItsLastTry(t *testing.T) {
	for name, c := range map[string]struct {
		during string // what changes meanwhile; "" is another replica's look
		status int
		off    string
	}{
		"another replica gave up, and it reached": {"", http.StatusNoContent, ""},
		"it failed":                           {"UPDATE telemetry SET instance_id = instance_id", http.StatusBadGateway, "unreachable"},
		"an admin turned it off":              {"UPDATE telemetry SET off_because = 'admin'", http.StatusBadGateway, "admin"},
		"an admin turned it off, and it sent": {"UPDATE telemetry SET off_because = 'admin'", http.StatusNoContent, "admin"},
		"an admin started it afresh":          {"UPDATE telemetry SET claimed_at = NULL, tried_since = NULL", http.StatusBadGateway, ""},
	} {
		t.Run(name, func(t *testing.T) {
			base, _ := telemetryFixture(t)
			a, pool := appOf(t, base), poolOf(t, base)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.during == "" {
					if sent, err := a.sendTelemetry(context.Background()); sent || err != nil {
						t.Errorf("the other replica's look: sent %v %v", sent, err)
					}
				} else if _, err := pool.Exec(context.Background(), c.during); err != nil {
					t.Error(err)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			a.telemetry.url = srv.URL
			if _, _, _, _, err := telemetryRow(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
			// the last try is due: at hour 2103, its 168 hours since the one at 1935 over
			if _, err := pool.Exec(t.Context(), "UPDATE telemetry SET tried_since = now() - interval '2103 hours', claimed_at = now() - interval '168 hours'"); err != nil {
				t.Fatal(err)
			}
			if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
				t.Fatalf("the last try: %v %v", sent, err)
			}
			if off, _ := telemetryOffAnd(t, pool); off != c.off {
				t.Errorf("after it: off_because %q, want %q", off, c.off)
			}
		})
	}
}

// Only a 2xx from the collector is a send that reached it: a proxy between the server and an unresolvable collector
// answers 502 itself, a failed try like a refused dial, tried again an hour later, never a report marked sent.
func TestTelemetryCountsOnlyTheCollectorsYes(t *testing.T) {
	base, _ := telemetryFixture(t)
	a := appOf(t, base)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer proxy.Close()
	a.telemetry.url = proxy.URL
	if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
		t.Fatalf("the first send: %v %v", sent, err)
	}
	if sent, _ := a.sendTelemetry(t.Context()); sent {
		t.Error("it tried again at once")
	}
	clockOn(t, poolOf(t, base), time.Hour)
	if sent, err := a.sendTelemetry(t.Context()); !sent || err != nil {
		t.Fatalf("the try an hour later: %v %v", sent, err)
	}
	var off string
	var sent, tried bool
	if err := poolOf(t, base).QueryRow(t.Context(), "SELECT coalesce(off_because, ''), sent_at IS NOT NULL OR last_report IS NOT NULL, tried_since IS NOT NULL FROM telemetry").Scan(&off, &sent, &tried); err != nil || off != "" || sent || !tried {
		t.Errorf("two 502s: off_because %q, marked sent %v, trying again %v, %v", off, sent, tried, err)
	}
}
