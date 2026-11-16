package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// nextReport is the next report GET /api/v1/telemetry shows, parsed, checked against the schema.
func nextReport(t *testing.T, base string, c *http.Client) map[string]any {
	t.Helper()
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/telemetry", ""); code != 200 {
		t.Fatalf("GET /telemetry: %d %s", code, lastBody)
	}
	var state struct {
		Report string `json:"report"`
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(lastBody), &state); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(state.Report), &report); err != nil {
		t.Fatal(err)
	}
	if err := contractSchema(t, "telemetry.schema.json").Validate(report); err != nil {
		t.Errorf("the report is not the schema's: %v\n%s", err, state.Report)
	}
	return report
}

// The second version's fields (#2953), each from its own source on the fixture: a deleted app is no app; the period's
// deploys, reviews by who asked for them and by coding agent, failures by why and hand-off releases; people and agents active,
// and people with access, bucketed; how long the first review took; database sessions, outbound control and a
// notification channel; and each app's stack by its latest build's buildpacks. The report is the schema's second
// version, so every field it brings is there.
func TestTelemetryReportsTheSecondVersion(t *testing.T) {
	base, c := telemetryFixture(t)
	pool, ctx := poolOf(t, base), t.Context()
	for _, q := range []string{
		`INSERT INTO projects (id, deleted_at) VALUES ('gone', now())`,
		`UPDATE runs SET started_by_kind = 'agent', started_by_subject = 'agent:claude-code/ana', harness = 'codex' WHERE id = 'gr_acme1'`,
		`UPDATE runs SET started_by_kind = 'agent', started_by_subject = 'agent:claude-code/ana', deploy_started_at = now() - interval '9 minutes', buildpacks = '[{"id": "paketo-buildpacks/ca-certificates"}, {"id": "paketo-buildpacks/node-engine"}]' WHERE id = 'gr_acme2'`,
		`INSERT INTO runs (id, project_id, package_id, state, failure_reason, failed_in, created_at, ended_at) SELECT 'gr_acme4', 'acme-payroll', package_id, 'failed', 'error', 'scan', now() - interval '3 minutes', now() - interval '2 minutes' FROM runs WHERE id = 'gr_acme1'`,
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at) SELECT 'rel_1', 'acme-payroll', 'preview', 'gr_acme2', package_id, 'deploy', 'canyon/acme-payroll@sha256:` + strings.Repeat("a", 64) + `', gen_random_uuid(), 'deployed', now() FROM runs WHERE id = 'gr_acme2'`,
		`INSERT INTO sessions (id, actor_kind, actor_subject, flow, expires_at) VALUES ('agent-session-1', 'agent', 'agent:claude-code/ana', 'device', now() + interval '1 hour')`,
		`INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('acme-payroll', 'email', 'ana.ruiz@acme.example', 'owner', 'person', 'anonymous', '')`,
		`INSERT INTO db_sessions (id, project_id, env, resource, mode, subject, role, expires_at) VALUES ('dbs_0123456789abcdef', 'acme-payroll', 'preview', 'postgres', 'read', 'anonymous', 'p_anonymous', now() + interval '1 hour')`,
		`UPDATE egress_settings SET via = 'canyon'`,
		`INSERT INTO egress_modes (project_id, env, mode) VALUES ('acme-payroll', 'prod', 'allowlist')`,
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES ('acme-payroll', 'prod', 'api.stripe.com', 'pending', now(), now(), 3)`,
		`INSERT INTO notify_channels (id, name, kind, host, tail, key_ref, sealed_key, sealed_url, set_by_kind, set_by_subject, set_by_display) VALUES ('ops', 'Ops', 'slack', 'hooks.slack.com', 'xyz', 1, '\x00', '\x00', 'person', 'anonymous', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	r := nextReport(t, base, c)
	for _, want := range []struct {
		field string
		value any
	}{
		{"apps", 2.0},
		{"period_hours", 24.0},
		{"demo", false},
		{"sign_in", "off"},
		{"age_days", "0"},
		{"deploys", map[string]any{"preview": 1.0, "prod": 0.0}},
		{"authors", map[string]any{"person": 0.0, "agent": 2.0}},
		{"agents", map[string]any{"claude_code": 0.0, "codex": 1.0, "other": 1.0}},
		{"failures", map[string]any{"detect": 1.0, "error.scan": 1.0}},
		{"handoff", map[string]any{"deployed": 1.0, "failed": 0.0, "refused": 0.0, "undelivered": 0.0}},
		{"people", map[string]any{"d1": 2.0, "d7": 2.0, "d28": 2.0}}, // the anonymous admin's session, and the person who started the fixture's failed review
		{"agent_sessions", map[string]any{"d1": 1.0, "d7": 1.0, "d28": 1.0}},
		{"people_total", "1"},
		{"apps_active", map[string]any{"d7": 1.0, "d28": 1.0}},
		{"db_sessions", map[string]any{"setting": "off", "prod": "off", "read": 1.0, "write": 0.0}},
		{"outbound", map[string]any{"via": "canyon", "allowlist_apps": 1.0, "credential_routes": false, "routes": "0", "refusals": 1.0}},
		{"notify", map[string]any{"slack": 1.0, "teams": 0.0}},
		{"stacks", map[string]any{"node": 1.0}},
		{"run_by", "canyon"},
		{"github", false},
		{"group_sync", false},
	} {
		if !reflect.DeepEqual(r[want.field], want.value) {
			t.Errorf("%s is %v, want %v", want.field, r[want.field], want.value)
		}
	}
	if first, _ := r["first"].(map[string]any); first["check"] != "lt_10m" || first["block"] != "lt_10m" || first["fix"] != "lt_10m" || first["prod"] != nil {
		t.Errorf("first is %v, want check, block and fix under 10 minutes and no promotion yet", r["first"])
	}
}

// The period's counts start where the previous send's did (#2953): a review that ended 28 hours ago is counted when
// the last claim was 30 hours ago, which a rolling 24 hours would lose, and a server that sent nothing for ten days
// reports its last week, never more.
func TestTelemetryCountsSinceTheLastSend(t *testing.T) {
	base, c := telemetryFixture(t)
	pool, ctx := poolOf(t, base), t.Context()
	for _, q := range []string{
		`INSERT INTO telemetry DEFAULT VALUES ON CONFLICT (one) DO NOTHING`,
		`UPDATE telemetry SET claimed_at = now() - interval '30 hours'`,
		`INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at) SELECT 'gr_acme0', 'acme-payroll', package_id, 'clean', now() - interval '29 hours', now() - interval '28 hours' FROM runs WHERE id = 'gr_acme1'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	r := nextReport(t, base, c)
	if r["period_hours"] != 30.0 || !reflect.DeepEqual(r["checks"], map[string]any{"passed": 2.0, "advisory": 0.0, "blocked": 1.0}) {
		t.Errorf("30 hours since the last claim: period_hours %v, checks %v; want 30 and the review 28 hours ago among them", r["period_hours"], r["checks"])
	}
	if _, err := pool.Exec(ctx, `UPDATE telemetry SET claimed_at = now() - interval '10 days'`); err != nil {
		t.Fatal(err)
	}
	if r := nextReport(t, base, c); r["period_hours"] != 168.0 {
		t.Errorf("ten days since the last claim: period_hours %v, want 168, a week", r["period_hours"])
	}
}

// A signed-in session is seen when it makes a request, at most once an hour (#2953): the usage report counts a person
// who only reads, and an hour's requests write one row update, not one each.
func TestASessionIsSeenAtMostHourly(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	seen := func() time.Duration {
		t.Helper()
		var ago *float64
		if err := pool.QueryRow(ctx, "SELECT extract(epoch FROM now() - last_seen_at) FROM sessions WHERE actor_subject = 'sub-dana'").Scan(&ago); err != nil {
			t.Fatal(err)
		}
		if ago == nil {
			return -1
		}
		return time.Duration(*ago * float64(time.Second))
	}
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/projects", ""); code != 200 {
		t.Fatalf("a read: %d %s", code, lastBody)
	}
	if ago := seen(); ago < 0 || ago > time.Minute {
		t.Fatalf("after a request, last seen %v ago, want now", ago)
	}
	for _, c := range []struct {
		set     string
		touched bool
	}{{"30 minutes", false}, {"2 hours", true}} {
		if _, err := pool.Exec(ctx, "UPDATE sessions SET last_seen_at = now() - interval '"+c.set+"' WHERE actor_subject = 'sub-dana'"); err != nil {
			t.Fatal(err)
		}
		do(t, dana, http.MethodGet, base+"/api/v1/projects", "")
		if ago := seen(); (ago < time.Minute) != c.touched {
			t.Errorf("seen %s ago, then a request: last seen %v ago, touched %v", c.set, ago, c.touched)
		}
	}
}

// A review keeps the coding agent the CLI ran under (#2953), as its Canyon-Harness names it: claude_code or codex;
// any other value, and none, keep nothing.
func TestAReviewKeepsItsHarness(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, up := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"index.js": "console.log(1)"}), "", "")
	for _, h := range []struct{ sent, kept string }{{"codex", "codex"}, {"cursor", ""}, {"", ""}, {"claude_code", "claude_code"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", strings.NewReader(`{"package":"`+packageOf(up)["id"].(string)+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if h.sent != "" {
			req.Header.Set("Canyon-Harness", h.sent)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var started struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&started)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted || started.Run.ID == "" {
			t.Fatalf("Canyon-Harness %q: %d", h.sent, resp.StatusCode)
		}
		var kept *string
		if err := pool.QueryRow(ctx, "SELECT harness FROM runs WHERE id = $1", started.Run.ID).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		got := ""
		if kept != nil {
			got = *kept
		}
		if got != h.kept {
			t.Errorf("Canyon-Harness %q kept %q, want %q", h.sent, got, h.kept)
		}
	}
}
