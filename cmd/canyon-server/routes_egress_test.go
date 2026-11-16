package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/runs"
)

// egressShape checks body against one definition of contracts/schema/egress.schema.json.
func egressShape(t *testing.T, definition, body string) {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile("../../contracts/schema/egress.schema.json#/definitions/" + definition)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err == nil {
		err = schema.Validate(doc)
	}
	if err != nil {
		t.Fatalf("%s is not the contract's: %v\n%s", definition, err, body)
	}
}

const (
	noNetwork = `{"error":"egress_not_ready","message":"an app's NetworkPolicy is written at every deploy: there is no network to recreate"}`
	noProxy   = `{"error":"egress_not_ready","message":"Canyon can't send apps' calls out through its egress gateway: this server has no CANYON_EGRESS_PROXY. Leave this off until it does."}`
)

// design/PERMISSIONS.md, Outbound: the fleet's destinations are an admin's. Every row last seen in the last 30
// days, of every app, as stored, the latest seen first, then by app, env, destination and port, and every gap
// ending in the window, whichever app it names, the oldest first. An owner, a contributor, a read-only member
// and a member with no app are each 403 admin_required, one denied row each, and read nothing.
func TestFleetDestinationsAreAnAdmins(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // carl contributes, vic reads
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	nina := signedIn(t, ctx, pool, base, "sub-nina", "nina@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/egress/destinations", ""); code != 200 {
		t.Fatalf("an admin's empty read: %d %s", code, lastBody)
	}
	egressShape(t, "fleet_read", lastBody)
	for _, row := range []struct {
		app, env, dest string
		port           int
		seen           string
		flags          string
	}{
		{"claims-portal", "prod", "hooks.slack.com", 443, "1 hour", "{}"},
		{"claims-portal", "prod", "api.stripe.com", 443, "1 hour", "{DC,UF}"},
		{"case-tracker", "preview", "api.anthropic.com", 443, "1 hour", "{}"},
		{"claims-portal", "preview", "registry.npmjs.org", 443, "1 minute", "{}"},
		{"case-tracker", "prod", "api.openai.com", 443, "31 days", "{}"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
			VALUES ($1, $2, $3, $4, date_trunc('second', now()) - interval '40 days', date_trunc('second', now()) - $5::interval, 2, 'allowed', '2xx', 'connect', $6)`,
			row.app, row.env, row.dest, row.port, row.seen, row.flags); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES (now() - interval '1 hour', now() - interval '59 minutes', 'dropped', 3, NULL, NULL),
		(now() - interval '32 days', now() - interval '31 days', 'receiver_down', NULL, NULL, NULL),
		(now() - interval '2 hours', now() - interval '2 hours', 'over_cap', 7, 'claims-portal', 'prod'),
		(now() - interval '3 hours', now() - interval '3 hours', 'over_cap', 4, 'case-tracker', 'prod')`); err != nil {
		t.Fatal(err)
	}
	code, out, _ := do(t, dana, http.MethodGet, base+"/api/v1/egress/destinations", "")
	if code != 200 {
		t.Fatalf("an admin's read: %d %s", code, lastBody)
	}
	egressShape(t, "fleet_read", lastBody)
	var rows, gaps []string
	for _, d := range out["destinations"].([]any) {
		d := d.(map[string]any)
		rows = append(rows, fmt.Sprintf("%s %s %s %v %v", d["app"], d["env"], d["destination"], d["port"], d["last_response_flags"]))
	}
	want := []string{"claims-portal preview registry.npmjs.org 443 []", "case-tracker preview api.anthropic.com 443 []", "claims-portal prod api.stripe.com 443 [DC UF]", "claims-portal prod hooks.slack.com 443 []"}
	if !slices.Equal(rows, want) {
		t.Errorf("destinations %q; want %q", rows, want)
	}
	for _, g := range out["gaps"].([]any) {
		g := g.(map[string]any)
		gaps = append(gaps, fmt.Sprintf("%v %v", g["reason"], g["app"]))
	}
	if want := []string{"over_cap case-tracker", "over_cap claims-portal", "dropped <nil>"}; !slices.Equal(gaps, want) {
		t.Errorf("gaps %q; want %q", gaps, want)
	}
	for name, who := range map[string]*http.Client{"an owner": olive, "a contributor": carl, "a read-only member": vic, "a member with no app": nina} {
		if code, _, _ := do(t, who, http.MethodGet, base+"/api/v1/egress/destinations", ""); code != 403 || lastBody != `{"error":"admin_required","message":"only an admin can do this: ask dana@meridian.example"}` {
			t.Errorf("%s's read: %d %s, want 403 admin_required", name, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'admin_required' AND detail->>'action' LIKE '%/egress/destinations'"); n != 4 {
		t.Errorf("%d denied rows, want one for each of the four refused", n)
	}
}

// design/PERMISSIONS.md, Outbound: every member reads via — a contributor, a read-only member and one with
// no app at all alike; the proxy, the receiver, its streams and its last entry are an admin's. A change is
// an admin's (admin_required), a person's (person_required), from a browser (dashboard_required), in that
// order — an admin in a browser allowed, an owner refused; canyon is refused on a Docker host that names no
// Envoy, fail closed; one egress.set row when via moves, none when it does not.
func TestTheEgressSettingIsReadByEveryMemberAndSetByAnAdminInABrowser(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // carl contributes, vic reads
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	nobody := signedIn(t, ctx, pool, base, "sub-nina", "nina@meridian.example", false)
	for name, who := range map[string]*http.Client{"an owner": olive, "a contributor": carl, "a read-only member": vic, "a member with no app": nobody} {
		if code, _, _ := do(t, who, http.MethodGet, base+"/api/v1/egress", ""); code != 200 || lastBody != `{"credential_routes":false,"mode":"observe","via":"off"}` {
			t.Fatalf("%s's read: %d %s, want via, the org's mode and whether Canyon adds credentials alone", name, code, lastBody)
		}
	}
	egressShape(t, "setting_member", lastBody)
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/egress", ""); code != 200 || lastBody != `{"ca":{"signing":null,"roots":[],"rotates_at":null},"canyon_refused":"`+egressNoProxy+`","caps":{"destinations":500,"new_names":30,"pending":50},"credential_routes":false,"last_entry_at":null,"mode":"observe","networks":{},"networks_to_recreate":[],"proxy":null,"receiver":false,"replicas":[],"streams":0,"via":"off"}` {
		t.Fatalf("an admin's read, which says before the choice why canyon is refused (#2300): %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	for _, tc := range []struct {
		who  *http.Client
		want string
	}{
		{olive, `{"error":"admin_required","message":"only an admin can do this: ask dana@meridian.example"}`},
		{danaCLI, `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}`},
		{dana, noProxy},
	} {
		if code, _, _ := do(t, tc.who, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code < 400 || lastBody != tc.want {
			t.Errorf("PUT: %d %s, want %s", code, lastBody, tc.want)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-dana' AND flow = 'browser'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"off"}`); code != 403 || body["error"] != "person_required" {
		t.Fatalf("an admin's agent: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person'"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"via":"istio"}`, `{"via":"off","other":1}`, `{"via":null}`, `{}`} {
		if code, out, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", body); code != 400 || out["error"] != "invalid_via" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/egress", `{"via":"off"}`); code != 403 {
		t.Fatalf("an owner's off: %d %s, want admin_required", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil { // as a host set so before the PUT refused it
		t.Fatal(err)
	}
	for range 2 {
		if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"off"}`); code != 200 || !strings.Contains(lastBody, `"via":"off"`) {
			t.Fatalf("the admin's browser: %d %s", code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.set' AND detail = '{"via": {"after": "off", "before": "canyon"}}'`); n != 1 {
		t.Fatalf("%d egress.set rows, want one for the change and none for the repeat", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' IN ('person_required', 'dashboard_required')"); n != 2 {
		t.Fatalf("%d denied rows for the actor's and the session's refusals, want 2", n)
	}
}

// On a demo host (sign-in off) every caller is the anonymous admin and no session tells a browser from the
// CLI: a PUT is refused nothing, canyon taken on a Docker host that names Envoy (#2254).
func TestADemoHostSetsEgressFromAnyCaller(t *testing.T) {
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 || !strings.Contains(lastBody, `"via":"canyon"`) {
		t.Fatalf("canyon on a demo host: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"off"}`); code != 200 || !strings.Contains(lastBody, `"via":"off"`) {
		t.Fatalf("off on a demo host: %d %s", code, lastBody)
	}
}

// contracts/routes.md, Egress control (#2286): a Kubernetes host takes canyon from an admin in a browser, as a
// Docker host does, with no probe of its own — the front door's gates every /run there — and refuses it for the
// level to the app's owner, its contributor, its read-only member and a member with no app; while the server
// names no Envoy it refuses canyon, fail closed, its admin's read saying so before the choice. The read lists no
// network to recreate: it has none (#2300).
func TestPutEgressCanyonOnKubernetes(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // olive owns it, carl contributes, vic reads
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	nina := signedIn(t, ctx, pool, base, "sub-nina", "nina@meridian.example", false)
	pipe.EgressProxy = "http://canyon-egress.canyon.svc:3128" // deploy/k8s/server.yaml's
	t.Cleanup(func() { pipe.EgressProxy = "" })
	for _, c := range []struct {
		who    string
		client *http.Client
	}{{"the app's owner", olive}, {"its contributor", carl}, {"its read-only member", vic}, {"a member with no app", nina}} {
		if code, out, _ := do(t, c.client, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 403 || out["error"] != "admin_required" {
			t.Fatalf("%s's canyon on Kubernetes: %d %s, want admin_required", c.who, code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 ||
		lastBody != `{"ca":{"signing":null,"roots":[],"rotates_at":null},"canyon_refused":null,"caps":{"destinations":500,"new_names":30,"pending":50},"credential_routes":false,"last_entry_at":null,"mode":"observe","networks":null,"networks_to_recreate":null,"proxy":"http://canyon-egress.canyon.svc:3128","receiver":false,"replicas":[],"streams":0,"via":"canyon"}` {
		t.Fatalf("the admin's canyon on Kubernetes: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.set' AND detail = '{"via": {"after": "canyon", "before": "off"}}'`); n != 1 {
		t.Fatalf("%d egress.set rows, want one", n)
	}
	pipe.EgressProxy = ""
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/egress", ""); code != 200 ||
		lastBody != `{"ca":{"signing":null,"roots":[],"rotates_at":null},"canyon_refused":"`+egressNoProxy+`","caps":{"destinations":500,"new_names":30,"pending":50},"credential_routes":false,"last_entry_at":null,"mode":"observe","networks":null,"networks_to_recreate":null,"proxy":null,"receiver":false,"replicas":[],"streams":0,"via":"canyon"}` {
		t.Fatalf("the admin's read on Kubernetes with no CANYON_EGRESS_PROXY: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 409 || lastBody != noProxy {
		t.Fatalf("canyon on Kubernetes with no CANYON_EGRESS_PROXY: %d %s", code, lastBody)
	}
}

// design/EGRESS.md §2.5 (#2286): on Kubernetes, which has no app network, floor is read from the deploy rows, per
// env — enforced when every env Canyon runs ran under canyon, not_enforced when the host is canyon and an env
// ran off, null otherwise — never from a network record.
func TestOnKubernetesTheFloorIsReadFromTheDeployRows(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	pkg := packageOf(body)["id"]
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_egress1', 'claims-portal', $1, 'clean', now())",
		"INSERT INTO app_networks (project_id, internal) SELECT 'claims-portal', true WHERE $1::text IS NOT NULL", // a stale record from a Docker era: never read on Kubernetes
	} {
		if _, err := pool.Exec(ctx, q, pkg); err != nil {
			t.Fatal(err)
		}
	}
	set := func(env, spec, runBy string) { // spec "" is no row, "null" a row with no spec recorded
		t.Helper()
		if _, err := pool.Exec(ctx, "DELETE FROM deploys WHERE env = $1", env); err != nil || spec == "" {
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, egress) VALUES ('claims-portal', $1, $2, 'canyon/claims-portal@sha256:"+strings.Repeat("a", 64)+"', 'http://x', 'gr_egress1', $3, CASE WHEN $4 = 'null' THEN NULL ELSE jsonb_build_object('via', $4::text) END)",
			env, pkg, runBy, spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, via, preview, prod, prodBy string
		floor                            any
	}{
		{"both canyon", "canyon", "canyon", "canyon", "canyon", "enforced"},
		{"both canyon, the host off", "off", "canyon", "canyon", "canyon", "enforced"},
		{"prod off", "canyon", "canyon", "off", "canyon", "not_enforced"},
		{"preview off, prod canyon", "canyon", "off", "canyon", "canyon", "not_enforced"},
		{"prod off, the host off", "off", "canyon", "off", "canyon", nil},
		{"prod with no spec recorded", "canyon", "canyon", "null", "canyon", "not_enforced"},
		{"preview alone, canyon", "canyon", "canyon", "", "", "enforced"},
		{"preview alone, off", "canyon", "off", "", "", "not_enforced"},
		{"nothing deployed", "canyon", "", "", "", nil},
		{"prod off, the org's platform running it", "canyon", "canyon", "off", "org", "enforced"},
		{"the org's platform running all of it", "canyon", "", "off", "org", nil},
	} {
		if _, err := pool.Exec(ctx, "UPDATE egress_settings SET via = $1", tc.via); err != nil {
			t.Fatal(err)
		}
		set("preview", tc.preview, "canyon")
		set("prod", tc.prod, tc.prodBy)
		if floor := floorOf(t, c, base); floor != tc.floor {
			t.Errorf("%s: floor %v, want %v", tc.name, floor, tc.floor)
		}
	}
}

// The Kubernetes floor is Kubernetes' alone (#2286): on a Docker host the floor is still the app's network, as its
// record says, never the deploy rows — an app network recorded internal is enforced though prod ran off, and one
// recorded open is not though every env ran canyon.
func TestOnDockerTheFloorIsStillTheNetwork(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_egress1', 'claims-portal', $1, 'clean', now())",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, egress) VALUES ('claims-portal', 'prod', $1, 'canyon/claims-portal@sha256:" + strings.Repeat("a", 64) + "', 'http://x', 'gr_egress1', '{\"via\": \"off\"}')",
		"UPDATE egress_settings SET via = 'canyon' WHERE $1::text IS NOT NULL",
	} {
		if _, err := pool.Exec(ctx, q, packageOf(body)["id"]); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		internal bool
		prod     string
		floor    any
	}{{true, "off", "enforced"}, {false, "canyon", "not_enforced"}} {
		if _, err := pool.Exec(ctx, "INSERT INTO app_networks (project_id, internal) VALUES ('claims-portal', $1) ON CONFLICT (project_id) DO UPDATE SET internal = $1", tc.internal); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE deploys SET egress = jsonb_build_object('via', $1::text)", tc.prod); err != nil {
			t.Fatal(err)
		}
		if floor := floorOf(t, c, base); floor != tc.floor {
			t.Errorf("a network recorded internal %t, prod run under %s: floor %v, want %v", tc.internal, tc.prod, floor, tc.floor)
		}
	}
}

// design/EGRESS.md §8.3: what an app called out to in the last 30 days, per env — each destination's row,
// last seen, calls and last result, the latest seen first, then by destination and port — under the spec
// its running version was deployed with (off for a row from before one was recorded, null where nothing is
// deployed); a row unseen for longer and another app's rows are not in it; every gap ending in the window
// that names no app is, whosever, and its own over_cap gap, never another app's.
func TestAnAppsEgressReadListsItsDestinationsAndNamesTheGaps(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if code != 201 {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	pkg := packageOf(body)["id"]
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_egress1', 'claims-portal', $1, 'clean', now())",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, egress) VALUES ('claims-portal', 'prod', $1, 'canyon/claims-portal@sha256:" + strings.Repeat("a", 64) + "', 'http://x', 'gr_egress1', '{\"via\": \"canyon\"}')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) VALUES ('claims-portal', 'preview', $1, 'canyon/claims-portal@sha256:" + strings.Repeat("a", 64) + "', 'http://x', 'gr_egress1')",
		"INSERT INTO app_networks (project_id, internal) SELECT 'claims-portal', true WHERE $1::text IS NOT NULL", // its network, as prod's start recorded it
	} {
		if _, err := pool.Exec(ctx, q, pkg); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		app, env, dest string
		port           int
		seen           string
		calls          int
		class, flags   string
	}{
		{"claims-portal", "prod", "api.stripe.com", 443, "2 minutes", 6, "5xx", "{DC,UF}"},
		{"claims-portal", "prod", "hooks.slack.com", 8443, "1 hour", 3, "2xx", "{}"},
		{"claims-portal", "prod", "hooks.slack.com", 443, "1 hour", 3, "2xx", "{}"},
		{"claims-portal", "prod", "api.openai.com", 443, "31 days", 50, "2xx", "{}"},
		{"claims-portal", "preview", "registry.npmjs.org", 443, "5 minutes", 2, "3xx", "{}"},
		{"case-tracker", "prod", "api.anthropic.com", 443, "1 minute", 9, "2xx", "{}"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
			VALUES ($1, $2, $3, $4, date_trunc('second', now()) - interval '40 days', date_trunc('second', now()) - $5::interval, $6, 'allowed', $7, 'connect', $8)`,
			row.app, row.env, row.dest, row.port, row.seen, row.calls, row.class, row.flags); err != nil {
			t.Fatal(err)
		}
	}
	// #2692: prod's api.stripe.com last refused a call while adding its credential; every other row never did
	if _, err := pool.Exec(ctx, `UPDATE egress_destinations SET last_credential_reason = 'inject_unavailable', last_credential_refused_at = '2026-11-09T10:00:00.4Z'
		WHERE app = 'claims-portal' AND env = 'prod' AND destination = 'api.stripe.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES (now() - interval '1 hour', now() - interval '59 minutes', 'dropped', 3, NULL, NULL),
		(now() - interval '32 days', now() - interval '31 days', 'receiver_down', NULL, NULL, NULL),
		(now() - interval '2 hours', now() - interval '2 hours', 'over_cap', 7, 'claims-portal', 'prod'),
		(now() - interval '3 hours', now() - interval '3 hours', 'over_cap', 4, 'case-tracker', 'prod')`); err != nil {
		t.Fatal(err)
	}
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/egress", "")
	if code != 200 {
		t.Fatalf("%d %s", code, lastBody)
	}
	egressShape(t, "project_read", lastBody)
	if out["floor"] != "enforced" {
		t.Errorf("floor %v; want enforced, prod's row recording canyon", out["floor"])
	}
	envs := out["envs"].(map[string]any)
	read := func(env string) (via any, got []string) {
		e := envs[env].(map[string]any)
		for _, d := range e["destinations"].([]any) {
			d := d.(map[string]any)
			got = append(got, fmt.Sprintf("%s:%v:%v:%s:%s", d["destination"], d["port"], d["calls"], d["last_status_class"], mustJSONString(d["last_response_flags"])))
		}
		return e["via"], got
	}
	via, prod := read("prod")
	if want := []string{`api.stripe.com:443:6:5xx:["DC","UF"]`, `hooks.slack.com:443:3:2xx:[]`, `hooks.slack.com:8443:3:2xx:[]`}; via != "canyon" || strings.Join(prod, " ") != strings.Join(want, " ") {
		t.Fatalf("prod: via %v, %v; want canyon, %v", via, prod, want)
	}
	via, preview := read("preview")
	if want := []string{`registry.npmjs.org:443:2:3xx:[]`}; via != "off" || strings.Join(preview, " ") != strings.Join(want, " ") {
		t.Fatalf("preview: via %v, %v; want off, %v", via, preview, want)
	}
	var refusals []string
	for _, env := range []string{"prod", "preview"} {
		for _, d := range envs[env].(map[string]any)["destinations"].([]any) {
			d := d.(map[string]any)
			refusals = append(refusals, fmt.Sprintf("%s:%v:%s", d["destination"], d["port"], mustJSONString(d["last_credential_refusal"])))
		}
	}
	if want := []string{`api.stripe.com:443:{"at":"2026-11-09T10:00:00Z","reason":"inject_unavailable"}`, "hooks.slack.com:443:null", "hooks.slack.com:8443:null", "registry.npmjs.org:443:null"}; !slices.Equal(refusals, want) {
		t.Errorf("each row's last credential refusal: %v, want %v, to the second, null where none", refusals, want)
	}
	var gaps []string
	for _, g := range out["gaps"].([]any) {
		g := g.(map[string]any)
		gaps = append(gaps, fmt.Sprintf("%s:%v:%v", g["reason"], g["calls"], g["app"]))
	}
	if want := []string{"over_cap:7:claims-portal", "dropped:3:<nil>"}; !slices.Equal(gaps, want) {
		t.Fatalf("the gaps: %v, want %v: the window's that name no app, and its own over_cap alone", gaps, want)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"case-files"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/case-files/egress", ""); code != 200 || out["envs"].(map[string]any)["prod"].(map[string]any)["via"] != nil || out["floor"] != nil {
		t.Fatalf("an app deployed nowhere: %d %s, want via and floor null", code, lastBody)
	}
}

func mustJSONString(v any) string { return string(mustJSON(v)) }

const recreate = "/api/v1/egress/networks/recreate"

// egressOf is the egress spec each env's deploy row records, preview's then prod's, "-" for none.
func egressOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var specs string
	if err := pool.QueryRow(ctx, "SELECT string_agg(coalesce(egress->>'via', '-'), ' ' ORDER BY env) FROM deploys WHERE project_id = 'claims-portal'").Scan(&specs); err != nil {
		t.Fatal(err)
	}
	return specs
}

// floorOf is the app's records' floor, as GET /api/v1/projects/claims-portal/egress reads it.
func floorOf(t *testing.T, c *http.Client, base string) any {
	t.Helper()
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/egress", "")
	if code != 200 {
		t.Fatalf("the app's egress read: %d %s", code, lastBody)
	}
	egressShape(t, "project_read", lastBody)
	return out["floor"]
}

// design/EGRESS.md §2.2 (the owner's decision, #2254): turning egress on lists an app running on a network made
// before it — its records say floor: not enforced, and a deploy routed via canyon is refused — and an admin's
// Recreate networks makes the network internal and starts preview and prod again from their rows, through
// Envoy, each row recording canyon; turning egress off lists it again, and recreating it open starts both
// again directly. Each recreation is one egress.networks.recreate row.
func TestRecreateNetworksMovesAnAppAcrossTheFloorBothWays(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	cleanGate(t)
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	listed := func(via string, want ...string) {
		t.Helper()
		code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/egress", "")
		egressShape(t, "setting_admin", lastBody)
		if got := fmt.Sprint(out["networks_to_recreate"]); code != 200 || out["via"] != via || got != fmt.Sprint(want) || out["canyon_refused"] != nil {
			t.Fatalf("the admin's read: %d %s; want via %s, networks_to_recreate %v, canyon_refused null", code, lastBody, via, want)
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon on a Docker host that names Envoy: %d %s", code, lastBody)
	}
	listed("canyon", "claims-portal")
	if floor := floorOf(t, c, base); floor != "not_enforced" {
		t.Fatalf("floor %v before the recreation; want not_enforced", floor)
	}
	before := fake.Runs.Load()
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 {
		t.Fatalf("Recreate networks under via canyon: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	if internal, made := fake.Network("claims-portal"); !internal || !made || fake.Recreates.Load() != 1 || fake.Runs.Load() != before+2 || fake.Ran().Egress == nil || fake.Ran().Egress.Proxy != "http://canyon-egress:3128" {
		t.Fatalf("under via canyon: internal %t, %d recreations, %d /runs since, the last carrying %+v; want the network internal and preview and prod run again through Envoy",
			internal, fake.Recreates.Load(), fake.Runs.Load()-before, fake.Ran().Egress)
	}
	if specs := egressOf(t, ctx, pool); specs != "canyon canyon" {
		t.Fatalf("the rows record %s; want canyon canyon, what runs", specs)
	}
	listed("canyon")
	if floor := floorOf(t, c, base); floor != "enforced" {
		t.Fatalf("floor %v after the recreation; want enforced", floor)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"off"}`); code != 200 {
		t.Fatalf("via off: %d %s", code, lastBody)
	}
	listed("off", "claims-portal")
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 {
		t.Fatalf("Recreate networks under via off: %d %s", code, lastBody)
	}
	if internal, made := fake.Network("claims-portal"); internal || !made || fake.Ran().Egress != nil || fake.Ran().Run != "" || egressOf(t, ctx, pool) != "off off" {
		t.Fatalf("under via off: internal %t, the last /run carrying %+v, the rows %s; want the network open, both run again directly", internal, fake.Ran().Egress, egressOf(t, ctx, pool))
	}
	listed("off")
	if floor := floorOf(t, c, base); floor != nil {
		t.Fatalf("floor %v on an open network under via off; want null", floor)
	}
	for _, detail := range []string{`{"failed": null, "started": ["prod", "preview"], "stopped": [], "internal": true}`, `{"failed": null, "started": ["prod", "preview"], "stopped": [], "internal": false}`} {
		if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.networks.recreate' AND project_id = 'claims-portal' AND detail = '`+detail+`'`); n != 1 {
			t.Errorf("%d egress.networks.recreate rows %s, want one", n, detail)
		}
	}
}

// Recreate networks /runs each env again with today's values, so each row records that /run's run-config — its
// spec and the secrets it was handed — and the fingerprint over it (contracts/routes.md, Patch runs, step 5),
// never the one the deploy before it recorded.
func TestRecreateNetworksRecordsWhatItsRunCarried(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	cleanGate(t)
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	fingerprints := func() (preview, prod string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT run_config_sha256 FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'),
			(SELECT run_config_sha256 FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod')`).Scan(&preview, &prod); err != nil {
			t.Fatal(err)
		}
		return preview, prod
	}
	preview, prod := fingerprints()
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY", `{"value":"value-recreate-4d1f"}`); code != 204 {
		t.Fatalf("a preview secret: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 {
		t.Fatalf("Recreate networks: %d %s", code, lastBody)
	}
	for _, env := range []string{"preview", "prod"} {
		var config runs.RunConfig
		var fingerprint, stored string
		if err := pool.QueryRow(ctx, "SELECT run_config, run_config_sha256, run_config::text FROM deploys WHERE project_id = 'claims-portal' AND env = $1", env).Scan(&config, &fingerprint, &stored); err != nil {
			t.Fatal(err)
		}
		if want, _ := config.Fingerprint(); want != fingerprint || config.Egress.Via != "canyon" || strings.Contains(stored, "value-recreate-4d1f") {
			t.Fatalf("%s's row after the recreation: %+v %s; want the canyon spec it ran under, no value, and its parts' fingerprint", env, config, fingerprint)
		}
		if _, set := config.Secrets["STRIPE_SECRET_KEY"]; set != (env == "preview") {
			t.Fatalf("%s's row names the secrets %v; want the ones its /run was handed", env, config.Secrets)
		}
	}
	if again, againProd := fingerprints(); again == preview || againProd == prod {
		t.Fatalf("the fingerprints %s %s, before the recreation %s %s; want each moved", again, againProd, preview, prod)
	}
}

// A recreation's answer when envs didn't start again names each in a person's words, one or two, a new deploy for an image
// gone and another recreation for anything else, both when they failed each way (design/EXPERIENCE.md D5; #2774).
func TestRecreateStoppedSaysEachEnvInWords(t *testing.T) {
	for _, c := range []struct {
		gone, broke []string
		want        string
	}{
		{nil, []string{"preview"}, "app's network is recreated. Preview didn't start again and stays stopped: recreate it again."},
		{nil, []string{"prod", "preview"}, "app's network is recreated. Production and preview didn't start again and stay stopped: recreate it again."},
		{[]string{"prod"}, nil, "app's network is recreated. Production can't start again: the image it ran is no longer on this Canyon, so app needs a new deploy."},
		{[]string{"prod", "preview"}, nil, "app's network is recreated. Production and preview can't start again: the images they ran are no longer on this Canyon, so app needs a new deploy."},
		{[]string{"preview"}, []string{"prod"}, "app's network is recreated. Production didn't start again and stays stopped: recreate it again. Preview can't start again: the image it ran is no longer on this Canyon, so app needs a new deploy."},
	} {
		if got := recreateStopped("app", c.gone, c.broke); got != c.want {
			t.Errorf("gone %v, broke %v: %q, want %q", c.gone, c.broke, got, c.want)
		}
	}
}

// Fail closed (the owner's decision, #2254): a recreation whose executor call fails, or whose env does not start
// again, is 502 recreate_failed and leaves the app stopped — never running on its old open network — its rows
// as they were; the same call again repeats the whole of it. Each attempt that reached the executor is one
// egress.networks.recreate row, saying what failed — and on a failed executor call claiming nothing stopped,
// which its failure does not say. Its answer is a person's words, never the executor's, which the server log
// keeps; an env whose image is gone needs a new deploy, which recreating it again never gives it (#2774).
func TestRecreateNetworksThatFailsLeavesTheAppStopped(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = ""; fake.FailRun = false; fake.NoImage.Store(0) })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	running := func() bool {
		t.Helper()
		_, err := pipe.Executor.Running(ctx, "claims-portal", "preview")
		return err == nil
	}
	fake.FailRecreate.Store(true)
	before := fake.Runs.Load()
	if code, out, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 502 || out["error"] != "recreate_failed" || running() || fake.Runs.Load() != before {
		t.Fatalf("an executor call that fails: %d %s, preview running %t, %d /runs; want 502 recreate_failed, the app stopped, nothing started", code, lastBody, running(), fake.Runs.Load()-before)
	}
	if want := `{"error":"recreate_failed","message":"claims-portal's network wasn't recreated, so the environments Canyon stopped for it stay stopped. Recreate it again."}`; lastBody != want {
		t.Fatalf("an executor call that fails says %s, want %s", lastBody, want)
	}
	fake.FailRecreate.Store(false)
	fake.FailRun = true
	if code, out, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 502 || out["error"] != "recreate_failed" || running() {
		t.Fatalf("a /run that fails after the stop: %d %s, preview running %t; want 502 naming preview, left stopped", code, lastBody, running())
	}
	if want := `{"error":"recreate_failed","message":"claims-portal's network is recreated. Preview didn't start again and stays stopped: recreate it again."}`; lastBody != want {
		t.Fatalf("a /run that fails says %s, want %s", lastBody, want)
	}
	fake.FailRun = false
	fake.NoImage.Store(1000) // the image preview ran is gone from this host
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 502 || running() ||
		lastBody != `{"error":"recreate_failed","message":"claims-portal's network is recreated. Preview can't start again: the image it ran is no longer on this Canyon, so claims-portal needs a new deploy."}` {
		t.Fatalf("an image gone: %d %s, preview running %t; want 502 saying it needs a new deploy, never to recreate it again", code, lastBody, running())
	}
	fake.NoImage.Store(0)
	if specs := egressOf(t, ctx, pool); specs != "off" {
		t.Fatalf("the row records %s; want off, as it was: nothing runs under canyon", specs)
	}
	fake.FailRun = false
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 || !running() || egressOf(t, ctx, pool) != "canyon" {
		t.Fatalf("the same call again: %d %s, preview running %t, the row %s", code, lastBody, running(), egressOf(t, ctx, pool))
	}
	for detail, want := range map[string]int{`{"failed": "network", "started": [], "stopped": [], "internal": true}`: 1, `{"failed": "start", "started": [], "stopped": ["preview"], "internal": true}`: 2} {
		if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.networks.recreate' AND detail = '`+detail+`'`); n != want {
			t.Errorf("%d rows %s for the recreations that failed, want %d", n, detail, want)
		}
	}
}

// design/PERMISSIONS.md, Host settings: Recreate networks is an admin's, a person's, from a browser — an
// admin in a browser allowed, an owner refused admin_required, the admin's CLI dashboard_required and an
// agent person_required, each before the body is read; an app Canyon runs nothing of is 404, and a
// Kubernetes host has no network to recreate.
func TestRecreateNetworksIsAnAdminsInABrowser(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	if code, out, _ := do(t, dana, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 404 || out["error"] != "not_found" {
		t.Fatalf("an app Canyon runs nothing of: %d %s, want 404", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by, egress) SELECT 'claims-portal', 'preview', package_id, $1, 'http://claims-portal.preview.localhost:7777', 'gr_seed', 'canyon', '{\"via\": \"off\"}' FROM runs WHERE id = 'gr_seed'",
		"canyon/claims-portal@"+executortest.Digest); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		who  *http.Client
		code int
		want string
	}{
		{olive, 403, "admin_required"},
		{danaCLI, 403, "dashboard_required"},
	} {
		if code, out, _ := do(t, tc.who, http.MethodPost, base+recreate, `{}`); code != tc.code || out["error"] != tc.want {
			t.Errorf("%s: %d %s", tc.want, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-dana' AND flow = 'browser'"); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := do(t, dana, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 403 || out["error"] != "person_required" {
		t.Fatalf("an admin's agent: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'person'"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"app":"claims-portal","internal":true}`, `{"app":7}`} {
		if code, out, _ := do(t, dana, http.MethodPost, base+recreate, body); code != 400 || out["error"] != "invalid_app" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 || fake.Recreates.Load() != 1 {
		t.Fatalf("the admin in a browser: %d %s, %d recreations", code, lastBody, fake.Recreates.Load())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' IN ('person_required', 'dashboard_required')"); n != 2 {
		t.Fatalf("%d denied rows for the actor's and the session's refusals, want 2", n)
	}
}

// contracts/routes.md, Egress control: a Kubernetes host has no app network to recreate, and says so (#2286).
func TestRecreateNetworksOnKubernetesIsNotReady(t *testing.T) {
	onKubernetes(t)
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 409 || lastBody != noNetwork || fake.Recreates.Load() != 0 {
		t.Fatalf("recreate on Kubernetes: %d %s", code, lastBody)
	}
}

// floor and networks_to_recreate read the network as its last recreation recorded it, never the rows (review
// of #2266): recreated open under via off with preview failing to start again and prod started first, the app
// reads open — no floor under via off, not_enforced and listed once via is canyon again — though preview's row
// still records canyon.
func TestRecreateNetworksRecordsTheNetworkNotTheRows(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	cleanGate(t)
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = ""; fake.FailRunOf = "" })
	via := func(v string) {
		t.Helper()
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"`+v+`"}`); code != 200 {
			t.Fatalf("via %s: %d %s", v, code, lastBody)
		}
	}
	via("canyon")
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 || fake.Ran().Target != "preview" {
		t.Fatalf("Recreate networks under via canyon: %d %s, the last /run %s; want prod started again first, preview last", code, lastBody, fake.Ran().Target)
	}
	via("off")
	fake.FailRunOf = "preview"
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 502 || egressOf(t, ctx, pool) != "canyon off" {
		t.Fatalf("Recreate networks under via off, preview failing: %d %s, the rows %s; want 502, preview's row as it was", code, lastBody, egressOf(t, ctx, pool))
	}
	if floor := floorOf(t, c, base); floor != nil {
		t.Fatalf("floor %v on the network recreated open under via off; want null, not what preview's row says", floor)
	}
	via("canyon")
	if floor := floorOf(t, c, base); floor != "not_enforced" {
		t.Fatalf("floor %v once via is canyon again; want not_enforced: the network is open", floor)
	}
	if code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/egress", ""); code != 200 || fmt.Sprint(out["networks_to_recreate"]) != "[claims-portal]" {
		t.Fatalf("the admin's read once via is canyon again: %d %s; want claims-portal listed", code, lastBody)
	}
}

// One recreation at a time on a host (review of #2266): a second while one runs is 409 busy at once, never a
// wait behind it.
func TestRecreateNetworksOneAtATime(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	hold := make(chan struct{})
	fake.HoldRun.Store(&hold)
	t.Cleanup(func() { fake.HoldRun.Store(nil) })
	recreation := func() <-chan int {
		answered := make(chan int, 1)
		go func() {
			res, err := c.Post(base+recreate, "application/json", strings.NewReader(`{"app":"claims-portal"}`))
			if err != nil {
				answered <- 0
				return
			}
			_ = res.Body.Close()
			answered <- res.StatusCode
		}()
		return answered
	}
	before := fake.Runs.Load()
	first := recreation()
	for deadline := time.Now().Add(20 * time.Second); fake.Runs.Load() == before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(hold)
			t.Fatal("the first recreation never reached its /run")
		}
	}
	select {
	case code := <-recreation():
		if code != 409 {
			t.Errorf("a second recreation while one runs: %d, want 409 busy", code)
		}
	case <-time.After(10 * time.Second):
		t.Error("a second recreation while one runs waited behind it, want 409 busy at once")
	}
	close(hold)
	if code := <-first; code != 200 {
		t.Fatalf("the first recreation: %d", code)
	}
}

// design/EGRESS.md §2.2 (review of #2266): an app whose production is held is never recreated — a hold whose
// /stop failed leaves production's row, and the recreation refuses 409 held, stopping and starting nothing —
// nor is an app being deleted (409 deleting).
func TestRecreateNetworksRefusesAHeldOrDeletingApp(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	fake.FailStop.Store(true)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 500 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 1 {
		t.Fatalf("a hold whose stop failed: %d %s, want production's row left", code, lastBody)
	}
	fake.FailStop.Store(false)
	runs := fake.Runs.Load()
	if code, out, _ := do(t, dana, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 409 || out["error"] != "held" || fake.Recreates.Load() != 0 || fake.Runs.Load() != runs {
		t.Fatalf("a held app: %d %s, %d recreations, %d /runs; want 409 held, nothing stopped or started", code, lastBody, fake.Recreates.Load(), fake.Runs.Load()-runs)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM prod_holds; UPDATE projects SET deleting_since = now() WHERE id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := do(t, dana, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 409 || out["error"] != "deleting" || fake.Recreates.Load() != 0 {
		t.Fatalf("an app being deleted: %d %s, want 409 deleting, nothing recreated", code, lastBody)
	}
}

// Fail closed (codex on #2266): an executor call that fails may have changed the network, so the app's network
// is recorded unknown before it — never floor enforced after the failure, and listed to recreate.
func TestRecreateNetworksThatFailsReadsTheNetworkUnknown(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 200 || floorOf(t, c, base) != "enforced" {
		t.Fatalf("the first recreation: %d %s, floor %v", code, lastBody, floorOf(t, c, base))
	}
	fake.FailRecreate.Store(true)
	if code, _, _ := do(t, c, http.MethodPost, base+recreate, `{"app":"claims-portal"}`); code != 502 {
		t.Fatalf("a recreation the executor failed: %d %s", code, lastBody)
	}
	if floor := floorOf(t, c, base); floor != "not_enforced" {
		t.Fatalf("floor %v after the executor failed; want not_enforced: what it left is unknown", floor)
	}
	if code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/egress", ""); code != 200 || fmt.Sprint(out["networks_to_recreate"]) != "[claims-portal]" {
		t.Fatalf("the admin's read after the executor failed: %d %s; want claims-portal listed", code, lastBody)
	}
}

// A delete of the app while its network is recreated (codex on #2266): the delete waits until the recreation
// ends and then stops what it started — never an env left running for an app that is gone.
func TestRecreateNetworksAndADeleteNeverLeaveTheAppRunning(t *testing.T) {
	_, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	hold := make(chan struct{})
	fake.HoldRun.Store(&hold)
	t.Cleanup(func() { fake.HoldRun.Store(nil) })
	answered := func(method, url, body string) <-chan int {
		ch := make(chan int, 1)
		go func() {
			req, err := http.NewRequest(method, url, strings.NewReader(body))
			if err != nil {
				ch <- 0
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				ch <- 0
				return
			}
			_ = res.Body.Close()
			ch <- res.StatusCode
		}()
		return ch
	}
	before := fake.Runs.Load()
	recreation := answered(http.MethodPost, base+recreate, `{"app":"claims-portal"}`)
	for deadline := time.Now().Add(20 * time.Second); fake.Runs.Load() == before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(hold)
			t.Fatal("the recreation never reached its /run")
		}
	}
	deleted := answered(http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	var deleteCode int
	select {
	case deleteCode = <-deleted:
		t.Errorf("the delete answered %d while the recreation ran; want it to wait until the recreation ends", deleteCode)
	case <-time.After(3 * time.Second):
	}
	close(hold)
	if code := <-recreation; code != 200 {
		t.Errorf("the recreation: %d, want 200: it was under way before the delete", code)
	}
	if deleteCode == 0 {
		deleteCode = <-deleted
	}
	if deleteCode != 204 {
		t.Errorf("the delete after the recreation: %d, want 204", deleteCode)
	}
	if _, err := pipe.Executor.Running(ctx, "claims-portal", "preview"); err == nil {
		t.Fatal("preview runs after the app was deleted: the recreation started it past the delete's stop")
	}
}

// No deadlock through the pool (codex on #2266): on a server whose pool holds four connections, two undeploys
// and a hold of the app arrive while its recreation runs — each waiting on the app's undeploy lock with a
// connection of its own, the recreation needing one more to start preview again. Every waiter answers within
// the lock's bound (20 s), and the recreation completes.
func TestRecreateNetworksNeverStarvesThePool(t *testing.T) {
	pool, ctx := migrated(t)
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"], cfg.MaxConns = schema, 4
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	cleanGate(t)
	appAnswersOK(t)
	base, _ := start(t, small, time.Hour)
	c := anonymous(t, ctx, small, base)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	deploy, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, deploy)["state"] != "clean" || promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("preview and prod: %s", lastBody)
	}
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	hold := make(chan struct{})
	fake.HoldRun.Store(&hold)
	t.Cleanup(func() { fake.HoldRun.Store(nil) })
	type answer struct {
		code  int
		after time.Duration
	}
	call := func(method, path, body string, wait time.Duration) <-chan answer {
		ch := make(chan answer, 1)
		go func() {
			began := time.Now()
			rctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			req, err := http.NewRequestWithContext(rctx, method, base+path, strings.NewReader(body))
			if err != nil {
				ch <- answer{}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				ch <- answer{after: time.Since(began)}
				return
			}
			_ = res.Body.Close()
			ch <- answer{res.StatusCode, time.Since(began)}
		}()
		return ch
	}
	before := fake.Runs.Load()
	recreation := call(http.MethodPost, recreate, `{"app":"claims-portal"}`, 90*time.Second)
	for deadline := time.Now().Add(20 * time.Second); fake.Runs.Load() == before; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(hold)
			t.Fatal("the recreation never reached prod's /run")
		}
	}
	waiters := []<-chan answer{
		call(http.MethodDelete, "/api/v1/projects/claims-portal/envs/preview", "", 45*time.Second),
		call(http.MethodDelete, "/api/v1/projects/claims-portal/envs/preview", "", 45*time.Second),
		call(http.MethodPost, "/api/v1/projects/claims-portal/envs/prod/hold", `{"reason":"INC-2254 a hold during a recreation"}`, 45*time.Second),
	}
	time.Sleep(time.Second) // each holds a connection, waiting on the app's undeploy lock
	close(hold)
	for i, w := range waiters {
		if a := <-w; a.code == 0 || a.code >= 500 || a.after > 35*time.Second {
			t.Errorf("waiter %d: %d after %s; want an answer, never a 5xx, within the lock's 20 s bound", i, a.code, a.after.Round(time.Second))
		}
	}
	if a := <-recreation; a.code != 200 {
		t.Fatalf("the recreation: %d after %s; want 200 — it completes", a.code, a.after.Round(time.Second))
	}
}

// With a sign-in served through the cluster's own gateway, PUT canyon is 409 with D5's line while the front door signs in
// there — a floored app's sidecars could not reach it — and the admin's read says so in canyon_refused, so
// Settings › Outbound shows it above the choice; at another provider it is taken. A member is refused first,
// admin_required, as on every host (#2316, the owner's decision).
func TestPutEgressCanyonIsRefusedAtASigninBehindTheGateway(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	testBehind = "id.canyon.meridian.example"
	t.Cleanup(func() { testBehind = "" })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	pipe.EgressProxy = "http://canyon-egress.canyon.svc:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	storeDoor(t, ctx, pool, "https://id.canyon.meridian.example")
	if code, out, _ := do(t, olive, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 403 || out["error"] != "admin_required" {
		t.Fatalf("a member's canyon: %d %s, want admin_required", code, lastBody)
	}
	want, _ := json.Marshal(map[string]string{"error": "egress_not_ready", "message": egressGatewaySignin})
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 409 || lastBody != string(want) {
		t.Fatalf("the admin's canyon at a sign-in behind the gateway: %d %s, want %s", code, lastBody, want)
	}
	if code, out, _ := do(t, dana, http.MethodGet, base+"/api/v1/egress", ""); code != 200 || out["canyon_refused"] != egressGatewaySignin || out["via"] != "off" {
		t.Fatalf("the admin's read at a sign-in behind the gateway: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	storeDoor(t, ctx, pool, "https://login.meridian.example")
	if code, out, _ := do(t, dana, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 || out["canyon_refused"] != nil || out["via"] != "canyon" {
		t.Fatalf("the admin's canyon at another provider: %d %s", code, lastBody)
	}
}

// storeDoor replaces the host's front door with a choice 2 one signing in at issuer, its secret stored.
func storeDoor(t *testing.T, ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, issuer string) {
	t.Helper()
	if _, err := q.Exec(ctx, "DELETE FROM front_doors"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, "INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 1, '', '', 'person', 'dana', 'Dana')",
		[]byte(`{"choice": 2, "signin": {"issuer": "`+issuer+`", "client_id": "canyon-apps"}, "domain": "canyon.meridian.example", "form": "app.env.domain"}`)); err != nil {
		t.Fatal(err)
	}
}

// A front door moved behind the cluster's gateway while PUT canyon waits on the mutation lock — the lock a
// front-door save takes — is seen by that PUT, which reads the door under the lock: canyon is refused, never
// written beside a door whose save answered no warning under via off (#2316, the delta review).
func TestPutEgressCanyonSeesAFrontDoorSavedMeanwhile(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	testBehind = "id.canyon.meridian.example"
	t.Cleanup(func() { testBehind = "" })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	pipe.EgressProxy = "http://canyon-egress.canyon.svc:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	storeDoor(t, ctx, pool, "https://login.meridian.example")
	held, err := pool.Begin(ctx) // a front-door save, between PUT canyon's start and its write
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if err := events.LockTx(ctx, held); err != nil {
		t.Fatal(err)
	}
	storeDoor(t, ctx, held, "https://id.canyon.meridian.example")
	answered := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/egress", strings.NewReader(`{"via":"canyon"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := dana.Do(req)
		if err != nil {
			answered <- err.Error()
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		answered <- fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}()
	waitFor(t, "PUT canyon to wait on the mutation lock", func() bool {
		var waiting bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)").Scan(&waiting); err != nil {
			t.Error(err)
			return true
		}
		return waiting
	})
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(egressGatewaySignin)
	if got, want := <-answered, `409 {"error":"egress_not_ready","message":`+string(msg)+`}`; got != want {
		t.Fatalf("PUT canyon with the door moved behind the gateway meanwhile: %s, want %s", got, want)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM egress_settings WHERE via = 'canyon'"); n != 0 {
		t.Fatal("PUT canyon wrote canyon beside a door behind the gateway")
	}
}

// CANYON_KUBE_GATEWAY_SIGNIN is the issuer of a sign-in behind the gateway, an https URL, and the server keeps its host,
// lower-cased; unset is none, and anything else refuses the start.
func TestGatewaySigninIsAnHTTPSIssuer(t *testing.T) {
	for v, want := range map[string]string{"": "", "https://ID.canyon.meridian.example": "id.canyon.meridian.example", "https://id.canyon.meridian.example/realms/apps": "id.canyon.meridian.example"} {
		if got, err := gatewaySigninOf(v); got != want || err != nil {
			t.Errorf("%q: %q %v, want %q", v, got, err, want)
		}
	}
	for _, v := range []string{"http://id.canyon.meridian.example", "id.canyon.meridian.example", "https://", "true"} {
		if got, err := gatewaySigninOf(v); err == nil || !strings.HasPrefix(err.Error(), "CANYON_KUBE_GATEWAY_SIGNIN") {
			t.Errorf("%q: %q %v, want a refusal naming the setting", v, got, err)
		}
	}
}

// The admin's settings read names each app networks_to_recreate lists with its network now, internal, open or unknown
// (a recreation under way or one that failed), so Settings › Outbound reads them in this one call (#2767).
func TestTheSettingsReadNamesEachAppsNetwork(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	for _, tc := range []struct{ via, internal, want string }{{"canyon", "false", "open"}, {"off", "true", "internal"}, {"canyon", "NULL", "unknown"}} {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"`+tc.via+`"}`); code != 200 {
			t.Fatalf("via %s: %d %s", tc.via, code, lastBody)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app_networks (project_id, internal) VALUES ('claims-portal', `+tc.internal+`)
			ON CONFLICT (project_id) DO UPDATE SET internal = excluded.internal`); err != nil {
			t.Fatal(err)
		}
		code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/egress", "")
		egressShape(t, "setting_admin", lastBody)
		networks, _ := out["networks"].(map[string]any)
		if code != 200 || fmt.Sprint(out["networks_to_recreate"]) != "[claims-portal]" || networks["claims-portal"] != tc.want || len(networks) != 1 {
			t.Errorf("via %s, the network internal = %s: %d %s; want claims-portal listed, its network %s", tc.via, tc.internal, code, lastBody, tc.want)
		}
	}
}
