package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// statements counts the statements a request of one subject sends, whatever loop the server runs beside it.
type statements struct {
	subject string
	n       atomic.Int64
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if actorOf(ctx).Subject == s.subject {
		s.n.Add(1)
	}
	return ctx
}

func (s *statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// derivations counts the statements a request of one subject sends that run the fleet derivation whole.
type derivations struct {
	subject string
	n       atomic.Int64
}

func (d *derivations) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if actorOf(ctx).Subject == d.subject && strings.Contains(data.SQL, fleetDerivation) {
		d.n.Add(1)
	}
	return ctx
}

func (d *derivations) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// needyHost is a host with sign-in, its pool counting dana's statements, and dana, its admin, signed in.
func needyHost(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client, *statements) {
	t.Helper()
	withOIDC(t) // dana is the admin
	plain, ctx := migrated(t)
	cfg := plain.Config()
	counted := &statements{subject: "sub-dana"}
	cfg.ConnConfig.Tracer = counted
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	base, _ := start(t, pool, time.Hour)
	return pool, ctx, base, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), counted
}

// seedNeedy writes apps from to to, each with a need of every kind the database holds: production refusing a
// destination an Allow can let through and one nobody can, a blocking secret, a production approval asked, a request
// for access and a network to recreate; nothing started, so both environments read stopped.
func seedNeedy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, from, to int) {
	t.Helper()
	const apps = `WITH n AS (SELECT 'app-' || lpad(i::text, 4, '0') AS id, lpad(to_hex(i), 6, '0') AS h, i FROM generate_series($1::int, $2::int) i) `
	verdict := `'{"state": "blocked", "blocking": 1, "warnings": 0, "findings": {"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "canyon/scanners"}}, "results": [` +
		`{"ruleId": "secrets.client-bundle", "partialFingerprints": {"canyon/v1": "f_` + strings.Repeat("ab", 32) + `"}, "properties": {"canyon": {"blocks": true}}}]}]}}'::jsonb`
	for _, q := range []string{
		apps + `INSERT INTO projects (id, name, stack, created_at) SELECT id, id, 'Go', now() - interval '2 days' FROM n`,
		apps + `INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display)
			SELECT id, 'email', 'olive@meridian.example', 'owner', 'sub-olive', 'Olive', 'person', 'sub-olive', 'Olive' FROM n`,
		apps + `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, label, created_at)
			SELECT 'pkg_' || h, id, 't', 'c', 1, 'upload', 'v1.' || i, now() - interval '2 hours' FROM n`,
		apps + `INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at) SELECT 'gr_' || h, id, 'pkg_' || h, 'blocked', now() - interval '90 minutes', now() - interval '80 minutes' FROM n`,
		apps + `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) SELECT 'run', 'gr_' || h, id, 'verdict', 'verdict', ` + verdict + ` FROM n`,
		apps + `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, egress, run_started_at, deployed_at)
			SELECT id, e, 'pkg_' || h, 'canyon/' || id || '@sha256:' || lpad(h, 64, '0'), 'http://' || id || '.' || e || '.localhost:7777', 'gr_' || h, '{"via": "canyon"}',
			now() - interval '1 hour', now() - interval '1 hour' FROM n, unnest(ARRAY['preview', 'prod']) e`,
		apps + `INSERT INTO egress_modes (project_id, env, mode) SELECT id, 'prod', 'allowlist' FROM n`,
		apps + `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) SELECT id, 'prod', 'api.stripe.com', 'pending', now(), now(), 3 FROM n`,
		apps + `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
			SELECT id, 'prod', '1.1.1.1', 443, now(), now(), 2, 'refused', '4xx', 'connect', '{UAEX}'::text[] FROM n
			UNION ALL SELECT id, 'preview', 'api.stripe.com', 443, now(), now(), 5, 'allowed', '2xx', 'connect', '{}'::text[] FROM n`,
		apps + `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at)
			SELECT upper(lpad(h, 8, '0')), id, 'gr_' || h, 'pkg_' || h, 'canyon/' || id || '@sha256:' || lpad(h, 64, '0'), 'agent', 'agent:claude-code/carl', 'Carl''s agent', 'cli', now() + interval '1 day' FROM n`,
		apps + `INSERT INTO access_requests (project_id, email, subject, display) SELECT id, 'nina@meridian.example', 'sub-nina', 'Nina' FROM n`,
		apps + `INSERT INTO app_networks (project_id, internal) SELECT id, NULL FROM n`,
	} {
		if _, err := pool.Exec(ctx, q, from, to); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
}

// needKindsOf is each row's needs as "kind env", by app, from a fleet read.
func needKindsOf(t *testing.T, body map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, row := range body["apps"].([]any) {
		app := row.(map[string]any)
		needs, _ := app["needs"].([]any)
		for _, n := range needs {
			need := n.(map[string]any)
			out[app["id"].(string)] = append(out[app["id"].(string)], fmt.Sprintf("%v %v", need["kind"], need["env"]))
		}
	}
	return out
}

// The fleet read computes what needs a person for every app the reader reads, so its cost is bounded by statements, never
// by apps (contracts/routes.md, *What needs a person*; #2724): one app and two hundred, each with a need of every kind
// the database holds, answer in the same number of statements, and the page is the contract's.
func TestTheFleetReadCountsItsStatementsNotItsApps(t *testing.T) {
	pool, ctx, base, dana, counted := needyHost(t)
	fleetSchema := contractSchema(t, "fleet.schema.json")
	read := func(apps int) int64 {
		t.Helper()
		counted.n.Store(0)
		code, body, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet?limit=200", "")
		n := counted.n.Load()
		if code != 200 {
			t.Fatalf("the fleet of %d: %d %s", apps, code, lastBody)
		}
		validJSON(t, fleetSchema, lastBody)
		kinds := needKindsOf(t, body)
		want := []string{"down prod", "refused prod", "refused_code prod", "secret <nil>", "approval prod", "down preview", "access <nil>", "recreate <nil>"}
		if len(kinds) != min(apps, 200) || !slices.Equal(kinds["app-0001"], want) {
			t.Fatalf("the fleet of %d: %d rows carry needs, app-0001's %v, want %v", apps, len(kinds), kinds["app-0001"], want)
		}
		if got := body["tiles"].(map[string]any)["needs_you"]; got != float64(apps*len(want)) {
			t.Fatalf("the fleet of %d: tiles.needs_you %v, want %d", apps, got, apps*len(want))
		}
		return n
	}
	seedNeedy(t, ctx, pool, 1, 1)
	one := read(1)
	seedNeedy(t, ctx, pool, 2, 200)
	if many := read(200); many != one || one == 0 {
		t.Fatalf("the fleet read sent %d statements for one app and %d for two hundred: one per app is unbounded", one, many)
	} else {
		t.Logf("the fleet read sent %d statements for one app and for two hundred", one)
	}
}

// The fleet read runs its derivation once (#95): its needs, count, page and tiles all read the one result, where they
// each ran it whole, four passes of ~120ms on the test bed after #91. The answer is the same either way, which the
// fleet's other tests hold.
func TestTheFleetReadRunsItsDerivationOnce(t *testing.T) {
	withOIDC(t) // dana is the admin
	plain, ctx := migrated(t)
	cfg := plain.Config()
	counted := &derivations{subject: "sub-dana"}
	cfg.ConnConfig.Tracer = counted
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	seedNeedy(t, ctx, pool, 1, 30)
	for _, query := range []string{"limit=50", "limit=1", "needs_you=true&limit=50", "sort=name&limit=10", "tile=blocked&sort=changed"} {
		counted.n.Store(0)
		if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet?"+query, ""); code != 200 {
			t.Fatalf("fleet?%s: %d %s", query, code, lastBody)
		}
		if n := counted.n.Load(); n != 1 {
			t.Errorf("fleet?%s ran the fleet derivation %d times, want once", query, n)
		}
	}
}

// The fleet read of five hundred apps, each needing a person in every way the database holds, answers well inside the
// shell's poll (the bound set in #2724): the median of five reads, under the test's -race.
func TestTheFleetReadsFiveHundredNeedyAppsInTime(t *testing.T) {
	pool, ctx, base, dana, _ := needyHost(t)
	seedNeedy(t, ctx, pool, 1, 500)
	var took []time.Duration
	for range 5 {
		at := time.Now()
		if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet?limit=50", ""); code != 200 {
			t.Fatalf("the fleet: %d %s", code, lastBody)
		}
		took = append(took, time.Since(at))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	t.Logf("the fleet read of 500 needy apps: median %v, of %v", took[2], took)
	if took[2] > fleetReadBound {
		t.Fatalf("the fleet read of 500 needy apps took %v (median of %v), over %v", took[2], took, fleetReadBound)
	}
}

// The fleet read's derivation, which it runs four times a read (needs, count, page, tiles), reads each app's newest
// verdict by its run through timeline_rows_run, never by scanning every verdict of the fleet once an app: on the test bed
// that one subquery, run 378 times, read every one of 913 verdicts each time, 400ms of a 525ms pass (#87). Any part of
// the plan that runs once an app and reads timeline_rows reads it by the run's index.
func TestTheFleetDerivationReadsEachAppsVerdictByItsRun(t *testing.T) {
	pool, ctx, _, _, _ := needyHost(t)
	seedNeedy(t, ctx, pool, 1, 500)
	if _, err := pool.Exec(ctx, "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	var plan []map[string]any
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+needFactsSQL, "dana@meridian.example", []string{"", "sub-dana"}, []string{}, true, "").Scan(&plan); err != nil {
		t.Fatal(err)
	}
	// indexes is the indexes a scan reads by: an index scan's own, a bitmap heap scan's every bitmap index scan's (a
	// BitmapAnd of two included)
	var indexes func(node map[string]any) []string
	indexes = func(node map[string]any) []string {
		if name, ok := node["Index Name"].(string); ok {
			return []string{name}
		}
		var out []string
		kids, _ := node["Plans"].([]any)
		for _, kid := range kids {
			if k := kid.(map[string]any); strings.HasPrefix(k["Node Type"].(string), "Bitmap") {
				out = append(out, indexes(k)...)
			}
		}
		return out
	}
	var bad []string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if loops, _ := node["Actual Loops"].(float64); node["Relation Name"] == "timeline_rows" && loops > 1 && !slices.Contains(indexes(node), "timeline_rows_run") {
			bad = append(bad, fmt.Sprintf("%s on timeline_rows by %q, %v times", node["Node Type"], indexes(node), loops))
		}
		kids, _ := node["Plans"].([]any)
		for _, kid := range kids {
			walk(kid.(map[string]any))
		}
	}
	walk(plan[0]["Plan"].(map[string]any))
	if len(bad) > 0 {
		t.Errorf("the fleet derivation reads timeline_rows once an app without the run's index:\n%s", strings.Join(bad, "\n"))
	}
}

// fleetReadBound is how long the fleet read of TestTheFleetReadsFiveHundredNeedyAppsInTime may take.
const fleetReadBound = 3 * time.Second

// Each need reaches the levels that may read it (design/PERMISSIONS.md; contracts/routes.md, *What needs a person*): a
// refused call the team's, contributor and up, never a read-only member's; a request for access its owners' and
// admins', never a contributor's; a network to recreate an admin's, never an owner's. A row below contributor carries no
// needs at all, and the reader's queue and count hold what their rows carry.
func TestEachNeedReachesOnlyWhoMayReadIt(t *testing.T) {
	h := outboundHost(t)
	fake.Start("claims-portal", "preview")
	fake.Start("claims-portal", "prod")
	h.exec(t, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES ('claims-portal', 'prod', 'api.stripe.com', 'pending', now(), now(), 3)`,
		"INSERT INTO access_requests (project_id, email, subject, display) VALUES ('claims-portal', 'gus@meridian.example', 'sub-gus', 'Gus')",
		"UPDATE app_networks SET internal = NULL WHERE project_id = 'claims-portal'")
	fleetSchema := contractSchema(t, "fleet.schema.json")
	for _, c := range []struct {
		who  string
		c    *http.Client
		want []string
	}{
		{"an admin", h.dana, []string{"refused prod", "access <nil>", "recreate <nil>"}},
		{"its owner", h.olive, []string{"refused prod", "access <nil>"}},
		{"a contributor", h.carl, []string{"refused prod"}},
		{"a read-only member", h.vic, nil},
		{"a member with no access", h.nina, nil},
	} {
		code, body, _ := do(t, c.c, http.MethodGet, h.base+"/api/v1/fleet", "")
		if code != 200 {
			t.Fatalf("%s's fleet: %d %s", c.who, code, lastBody)
		}
		validJSON(t, fleetSchema, lastBody)
		got := needKindsOf(t, body)["claims-portal"]
		if !slices.Equal(withoutReview(got), c.want) {
			t.Errorf("%s reads %v, want %v", c.who, got, c.want)
		}
		carried := 0
		for _, kinds := range needKindsOf(t, body) {
			carried += len(kinds)
		}
		count, queued := body["tiles"].(map[string]any)["needs_you"], slices.Contains(fleetIDs(t, c.c, h.base+"/api/v1/fleet?needs_you=true"), "claims-portal")
		if count != float64(carried) || queued != (len(got) > 0) {
			t.Errorf("%s: tiles.needs_you %v and queued %t, but the rows carry %d needs, claims-portal's %v", c.who, count, queued, carried, got)
		}
	}
}

// An approved secret is a person's decision ("a test key", "rotated already"), so it is no need, on the fleet read and
// the status read alike; one no unexpired approval covers is (contracts/routes.md, *What needs a person*, secret; #2762).
func TestAnApprovedSecretIsNoNeed(t *testing.T) {
	pool, ctx, base, dana, _ := needyHost(t)
	seedNeedy(t, ctx, pool, 1, 2)
	// Both reviews were started by a person, so an admin's approval covers their findings; app-0001's secret has one.
	for _, q := range []string{
		`UPDATE runs SET started_by_kind = 'person', started_by_subject = 'sub-carl' WHERE project_id IN ('app-0001', 'app-0002')`,
		`INSERT INTO approvals (project_id, rule, fingerprint, reason, expires_at, approved_by_kind, approved_by_subject, approved_by_display, approves_blocking)
			VALUES ('app-0001', 'secrets.client-bundle', 'f_` + strings.Repeat("ab", 32) + `', 'a test key', now() + interval '30 days', 'person', 'sub-dana', 'Dana', true)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	code, body, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet?limit=200", "")
	if code != 200 {
		t.Fatalf("the fleet read: %d %s", code, lastBody)
	}
	kinds := needKindsOf(t, body)
	if slices.Contains(kinds["app-0001"], "secret <nil>") {
		t.Errorf("an approved secret is a need: app-0001's %v", kinds["app-0001"])
	}
	if !slices.Contains(kinds["app-0002"], "secret <nil>") {
		t.Errorf("a secret nobody approved is no need: app-0002's %v", kinds["app-0002"])
	}
	for app, want := range map[string]bool{"app-0001": false, "app-0002": true} {
		code, status, _ := do(t, dana, http.MethodGet, base+"/api/v1/projects/"+app+"/status", "")
		if code != 200 {
			t.Fatalf("%s's status read: %d %s", app, code, lastBody)
		}
		needs, _ := status["needs"].([]any)
		got := slices.ContainsFunc(needs, func(n any) bool { return n.(map[string]any)["kind"] == "secret" })
		if got != want {
			t.Errorf("%s's status read carries a secret need: %t, want %t (%v)", app, got, want, needs)
		}
	}
}

// withoutReview leaves out the needs a fixture's review leaves, which this test is not about.
func withoutReview(kinds []string) []string {
	return slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k == "never_reviewed <nil>" || k == "blocked <nil>" || k == "secret <nil>" })
}

// The status read carries the fleet row's needs, the same list in the same order, so the app page's band and the
// fleet never disagree (contracts/routes.md, *What needs a person*).
func TestTheStatusReadCarriesTheFleetRowsNeeds(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.stripe.com', 'pending', now(), now(), 3), ('claims-portal', 'preview', 'hooks.slack.com', 'pending', now(), now(), 1)`,
		"INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'preview', 'allowlist')")
	statusSchema := contractSchema(t, "app-page.schema.json#/definitions/status")
	_, fleet, _ := do(t, h.olive, http.MethodGet, h.base+"/api/v1/fleet", "")
	var row any
	for _, a := range fleet["apps"].([]any) {
		if a.(map[string]any)["id"] == "claims-portal" {
			row = a.(map[string]any)["needs"]
		}
	}
	code, status, _ := do(t, h.olive, http.MethodGet, h.base+"/api/v1/projects/claims-portal/status", "")
	if code != 200 {
		t.Fatalf("the status read: %d %s", code, lastBody)
	}
	validJSON(t, statusSchema, lastBody)
	if got, want := string(mustJSON(status["needs"])), string(mustJSON(row)); got != want || !strings.Contains(got, `"refused"`) || !strings.Contains(got, `"down"`) {
		t.Fatalf("the status read's needs:\n got %s\nthe fleet row's %s", got, want)
	}
}

// A held production is a need of everyone who reads its hold (contracts/routes.md, *What needs a person*): a
// responder with no grant lifts it, their reduced row carrying it alone; the team reads it, theirs to hand on to the
// admins, and production stopped by its hold is no down need; a read-only member reads nothing.
func TestAHeldProductionIsTheRespondersToLift(t *testing.T) {
	pool, ctx, base, _ := responderHost(t) // carl contributes, vic reads
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	needOf := func(c *http.Client) []any {
		t.Helper()
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		for _, a := range body["apps"].([]any) {
			if app := a.(map[string]any); app["id"] == "claims-portal" {
				needs, _ := app["needs"].([]any)
				return needs
			}
		}
		t.Fatalf("no claims-portal row: %s", lastBody)
		return nil
	}
	held := func(needs []any) map[string]any {
		for _, n := range needs {
			if need := n.(map[string]any); need["kind"] == "held" {
				return need
			}
			if need := n.(map[string]any); need["kind"] == "down" && need["env"] == "prod" {
				t.Errorf("a held production is a down need too: %v", needs)
			}
		}
		return nil
	}
	if needs := needOf(rhea); len(needs) != 1 || held(needs)["you"] != true {
		t.Errorf("the responder reads %v, want the held need alone, theirs to lift", needs)
	}
	if need := held(needOf(carl)); need == nil || need["you"] != false || len(need["ask"].([]any)) == 0 {
		t.Errorf("a contributor reads %v, want the held need, the admins' to lift", need)
	}
	if need := held(needOf(dana)); need == nil || need["you"] != true {
		t.Errorf("the admin reads %v, want the held need, theirs to lift", need)
	}
	if needs := needOf(vic); needs != nil {
		t.Errorf("a read-only member reads %v", needs)
	}
	_, status, _ := do(t, rhea, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if needs, _ := status["needs"].([]any); len(needs) != 1 || held(needs) == nil {
		t.Errorf("the responder's status read carries %v, want the held need alone", status["needs"])
	}
}

// What the fleet read could not check is never all clear (contracts/routes.md, the fleet read's unchecked; #2724): with
// the executor refusing, each environment Canyon runs of the apps read whole is counted unchecked, and none is a need
// of its own; with it answering, none is.
func TestAnExecutorTheFleetCannotReadIsNeverAllClear(t *testing.T) {
	h := outboundHost(t)
	fleetSchema := contractSchema(t, "fleet.schema.json")
	read := func() map[string]any {
		t.Helper()
		code, body, _ := do(t, h.carl, http.MethodGet, h.base+"/api/v1/fleet", "")
		if code != 200 {
			t.Fatalf("the fleet: %d %s", code, lastBody)
		}
		validJSON(t, fleetSchema, lastBody)
		return body
	}
	fake.Start("claims-portal", "preview")
	fake.Start("claims-portal", "prod")
	if got := read()["unchecked"]; got != 0.0 {
		t.Fatalf("with the executor answering, unchecked %v, want 0", got)
	}
	fake.FailRunning.Store(true)
	body := read()
	if got := body["unchecked"]; got != 2.0 {
		t.Errorf("with the executor refusing, unchecked %v, want claims-portal's 2 environments", got)
	}
	if slices.ContainsFunc(needKindsOf(t, body)["claims-portal"], func(k string) bool { return strings.HasPrefix(k, "down ") }) {
		t.Errorf("an environment it couldn't check is a down need: %v", needKindsOf(t, body))
	}
	if code, _, _ := do(t, h.vic, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || !strings.Contains(lastBody, `"unchecked":0`) {
		t.Errorf("a read-only member's unchecked counts apps they don't read whole: %d %s", code, lastBody)
	}
}

// The held need's you is the lift's own (liftsOffTeam): a responder holding a viewer grant lifts it, one who
// contributed when production was held never does, though the grant has gone (design/PERMISSIONS.md, off the app's team).
func TestTheHeldNeedIsTheLiftsOwnRule(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "carl@meridian.example", "vic@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)    // a viewer grant
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false) // a contributor when it is held
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE grantee = 'carl@meridian.example'"); err != nil {
		t.Fatal(err)
	}
	you := func(c *http.Client) any {
		t.Helper()
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		for _, a := range body["apps"].([]any) {
			if app := a.(map[string]any); app["id"] == "claims-portal" {
				needs, _ := app["needs"].([]any)
				for _, n := range needs {
					if need := n.(map[string]any); need["kind"] == "held" {
						return need["you"]
					}
				}
			}
		}
		return nil
	}
	if got := you(vic); got != true {
		t.Errorf("a responder with a viewer grant: you %v, want true, as the lift lets them", got)
	}
	if got := you(carl); got != false {
		t.Errorf("a responder on the team the hold recorded: you %v, want false, as the lift refuses them", got)
	}
	if code, _, _ := do(t, carl, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 403 {
		t.Errorf("carl lifts: %d, want 403: the need and the lift disagree", code)
	}
	if code, _, _ := do(t, vic, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Errorf("vic lifts: %d %s, want 204: the need and the lift disagree", code, lastBody)
	}
}

// fleetIDs is the ids of a fleet read's rows.
func fleetIDs(t *testing.T, c *http.Client, url string) []string {
	t.Helper()
	code, body, _ := do(t, c, http.MethodGet, url, "")
	if code != 200 {
		t.Fatalf("%s: %d %s", url, code, lastBody)
	}
	var ids []string
	for _, row := range body["apps"].([]any) {
		ids = append(ids, row.(map[string]any)["id"].(string))
	}
	return ids
}
