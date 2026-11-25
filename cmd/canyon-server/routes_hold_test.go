package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	holdPath       = "/api/v1/projects/claims-portal/envs/prod/hold"
	holdReason     = `{"reason":"Exports claimant SSNs unauthenticated (INC-2231)"}`
	holdRefusal    = `{"admins":["dana@meridian.example"],"error":"responder_required","message":"only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy."}`
	liftRefusal    = `{"admins":["dana@meridian.example"],"error":"responder_required","message":"only a security responder who is not on this app's team, or an admin, can lift its hold."}`
	liftInBrowser  = `{"error":"dashboard_required","message":"lift it in Canyon's dashboard"}`
	responderEmail = "rhea@meridian.example"
)

// responderHost is an oidc host whose responders list rhea, with claims-portal (owner olive, contributor
// carl, viewer vic) running in production, its container up.
func responderHost(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client) {
	t.Helper()
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail}); err != nil {
		t.Fatal(err)
	}
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run)
		SELECT project_id, 'prod', id, 'canyon/claims-portal@'||$1, 'http://claims-portal.prod.localhost:7777', 'gr_seed' FROM packages WHERE project_id = 'claims-portal'`, executortest.Digest); err != nil {
		t.Fatal(err)
	}
	fake.Start("claims-portal", "prod")
	return pool, ctx, base, olive
}

// liftPath is the lift's address for the hold claims-portal has now, `held_at` its `at` as the reads answer
// it (contracts/routes.md, The hold), with query after it; with no hold, none.
func liftPath(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, query string) string {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(ctx, "SELECT max(held_at) FROM prod_holds WHERE project_id = 'claims-portal'").Scan(&at); err != nil {
		t.Fatal(err)
	}
	q := neturl.Values{}
	if at != nil {
		q.Set("held_at", at.UTC().Format(time.RFC3339))
	}
	path := base + holdPath + "?" + q.Encode()
	if query != "" {
		path += "&" + query
	}
	return path
}

// fromCLI is a client holding a device session for sub and email: the CLI's.
func fromCLI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base, sub, email string) *http.Client {
	t.Helper()
	c := signedIn(t, ctx, pool, base, sub, email, false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET flow = 'device' WHERE id = (SELECT id FROM sessions WHERE actor_subject = $1 ORDER BY created_at DESC LIMIT 1)", sub); err != nil {
		t.Fatal(err)
	}
	return c
}

// fleetRowOf is the app's row on a fleet page, nil when the page does not carry it.
func fleetRowOf(page map[string]any, id string) map[string]any {
	apps, _ := page["apps"].([]any)
	for _, a := range apps {
		if row, _ := a.(map[string]any); row["id"] == id {
			return row
		}
	}
	return nil
}

// A responder with no grant reads the app's fleet row — the reduced one, plus production's address and
// state and its hold — and, of the app's routes, only what approving a blocking finding needs (#2204): the
// runs list, a run's read and SARIF, the status read and the approvals. Every other app route is
// team_required, logs and pull among them. The Held filter reveals a hold to no one who does not read it.
func TestResponderReadsOnlyTheFleetRow(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	_, page, _ := do(t, rhea, http.MethodGet, base+"/api/v1/fleet", "")
	const row = `{"access":{"level":"none"},"claimable":false,"envs":{"prod":{"state":"running","url":"http://claims-portal.prod.localhost:7777"}},"id":"claims-portal","name":"claims-portal","owner_groups":[],"owners":[{"display":"sub-olive","kind":"person","subject":"sub-olive"}],"summary":""}`
	if viewer, _ := page["viewer"].(map[string]any); viewer["role"] != "responder" || string(mustJSON(fleetRowOf(page, "claims-portal"))) != row {
		t.Fatalf("the responder's fleet: %s", lastBody)
	}
	if _, read, _ := do(t, rhea, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); string(mustJSON(read["project"])) != row {
		t.Fatalf("the responder's project read: %s", lastBody)
	}
	for _, r := range append(slices.Clone(teamRoutes), struct {
		method, path, body string
		passed             int
	}{http.MethodGet, "/api/v1/projects/claims-portal/logs?env=prod", "", 200}) {
		code, _, _ := do(t, rhea, r.method, base+r.path, r.body)
		if slices.Contains(approverReads, r.method+" "+r.path) {
			if code != r.passed {
				t.Errorf("the responder, %s %s: %d %s, want %d", r.method, r.path, code, lastBody, r.passed)
			}
			continue
		}
		if code != 403 || lastBody != teamRequiredBody {
			t.Errorf("the responder, %s %s: %d %s, want team_required", r.method, r.path, code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2231', 'person', 'sub-rhea', 'Rhea Okafor')`); err != nil {
		t.Fatal(err)
	}
	vic, nora := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	for _, c := range []struct {
		who   string
		c     *http.Client
		reads bool
	}{{"the responder", rhea, true}, {"the owner", olive, true}, {"the admin", signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), true}, {"a viewer", vic, false}, {"a member with no access", nora, false}} {
		_, held, _ := do(t, c.c, http.MethodGet, base+"/api/v1/fleet?held=true", "")
		_, all, _ := do(t, c.c, http.MethodGet, base+"/api/v1/fleet", "")
		_, read, _ := do(t, c.c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		hold, _ := fleetRowOf(all, "claims-portal")["hold"].(map[string]any)
		readHold, _ := read["project"].(map[string]any)["hold"].(map[string]any)
		if (fleetRowOf(held, "claims-portal") != nil) != c.reads || (hold != nil) != c.reads || (readHold != nil) != c.reads {
			t.Errorf("%s: held filter %v, fleet row hold %v, project read hold %v; want %v", c.who, fleetRowOf(held, "claims-portal") != nil, hold, readHold, c.reads)
		}
		if c.reads && (hold["reason"] != "INC-2231" || hold["version"] != nil || hold["by"].(map[string]any)["subject"] != "sub-rhea") {
			t.Errorf("%s reads the hold %v", c.who, hold)
		}
	}
}

// The responder claim is matched at sign-in, as the admin claim is, whether the operator pins it or the
// row holds it, and kept on the session: a person the claim names is a responder for that session, one it
// does not is a member. The pair is both or neither at boot.
func TestResponderClaimAtSignIn(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	for _, source := range []string{"environment", "row"} {
		env := map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example"}
		if source == "environment" {
			env["CANYON_RESPONDER_CLAIM"], env["CANYON_RESPONDER_VALUE"] = "groups", "security-responders"
		}
		cfg, err := oidcConfigOf(envOf(env))
		if err != nil {
			t.Fatal(err)
		}
		_, p := idp.discovered(t)
		testAuth, testOIDC, testProvider = authOIDC, cfg, p
		t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
		pool, ctx := migrated(t)
		if source == "row" {
			if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_claim = 'groups', responder_value = 'security-responders'"); err != nil {
				t.Fatal(err)
			}
		}
		base, logs := start(t, pool, time.Hour)
		for _, c := range []struct {
			email  string
			groups []any
			role   string
		}{{responderEmail, []any{"staff", "security-responders"}, "responder"}, {"sam@meridian.example", []any{"staff"}, "member"}} {
			idp.bend = map[string]any{"email": c.email, "groups": c.groups, "sub": "sub-" + c.email}
			b := browser(t)
			callback, _, _, _ := startSignIn(t, b, base, idp)
			if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
				t.Fatalf("%s, %s signs in: %d %s", source, c.email, resp.StatusCode, resp.Header.Get("Location"))
			}
			if _, page, _ := do(t, b, http.MethodGet, base+"/api/v1/fleet", ""); page["viewer"].(map[string]any)["role"] != c.role {
				t.Fatalf("%s, %s reads the fleet as %s", source, c.email, lastBody)
			}
			var claimed bool
			if err := pool.QueryRow(ctx, "SELECT claimed_responder FROM sessions WHERE email = $1", c.email).Scan(&claimed); err != nil || claimed != (c.role == "responder") {
				t.Fatalf("%s, %s's session claimed_responder %v (%v)", source, c.email, claimed, err)
			}
			if !strings.Contains(logs.String(), c.email+" admin_claim=unset role="+c.role+"\n") {
				t.Fatalf("%s: the sign-in line names the role %s:\n%s", source, c.role, logs.String())
			}
		}
	}
	for name, env := range map[string]map[string]string{"the claim alone": {"CANYON_RESPONDER_CLAIM": "groups"}, "the value alone": {"CANYON_RESPONDER_VALUE": "security-responders"}} {
		env["CANYON_OIDC_ISSUER"], env["CANYON_OIDC_CLIENT_ID"], env["CANYON_ADMINS"] = idp.URL, "canyon", "dana@meridian.example"
		if _, err := oidcConfigOf(envOf(env)); err == nil || err.Error() != "CANYON_RESPONDER_CLAIM and CANYON_RESPONDER_VALUE are set together or not at all" {
			t.Fatalf("%s at boot: %v", name, err)
		}
	}
}

// The role read live: off the list, a responder is a member on their next request; an admin on the list is
// an admin; and a device grant a claimed responder approves mints a CLI session that is a responder too.
func TestTheResponderRoleIsReadLiveAndCarriedToTheCLI(t *testing.T) {
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "dana@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	role := func(c *http.Client) string {
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		viewer, _ := page["viewer"].(map[string]any)
		return fmt.Sprint(viewer["role"])
	}
	rhea, dana := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if role(rhea) != "responder" || role(dana) != "admin" {
		t.Fatalf("listed: rhea %s, dana %s", role(rhea), role(dana))
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = '{}'"); err != nil {
		t.Fatal(err)
	}
	if role(rhea) != "member" {
		t.Fatalf("off the list, on the same session: %s", role(rhea))
	}
	sam := signedIn(t, ctx, pool, base, "sub-sam", "sam@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET claimed_responder = true WHERE actor_subject = 'sub-sam'"); err != nil {
		t.Fatal(err)
	}
	device, user, _ := mint(t, base)
	if code, _, _ := do(t, sam, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("the claimed responder approves: %d %s", code, lastBody)
	}
	code, body := poll(t, base, device)
	if code != 200 {
		t.Fatalf("the CLI's poll: %d %s", code, lastBody)
	}
	cli := client(t)
	u, _ := neturl.Parse(base)
	cli.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: body["session"].(string)}}) //nolint:gosec // test: the session the poll answered
	if role(cli) != "responder" || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device' AND claimed_responder") != 1 {
		t.Fatalf("the CLI session of a claimed responder: %s", role(cli))
	}
}

// The responder list and claim are an admin's to read and set: a member's read carries none of the three,
// an operator-pinned pair is overridden and set_by_operator, and a change is audited before and after.
func TestTheRespondersAreAnAdminsToReadAndSet(t *testing.T) {
	withOIDC(t)
	c, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": "https://login.example.com/v2.0", "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example",
		"CANYON_RESPONDER_CLAIM": "groups", "CANYON_RESPONDER_VALUE": "security-responders"}))
	if err != nil {
		t.Fatal(err)
	}
	testOIDC = c
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana, sam := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), signedIn(t, ctx, pool, base, "sub-sam", "sam@meridian.example", false)
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"responder_emails":["rhea@meridian.example"]}`); code != 200 || len(body["responder_emails"].([]any)) != 1 || body["responder_claim"] != "groups" ||
		!slices.Contains(body["overridden"].([]any), "responder_value") {
		t.Fatalf("an admin sets the responders: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences' AND detail = '{"responder_emails":{"before":[],"after":["rhea@meridian.example"]}}'`); n != 1 {
		t.Fatalf("the change's audit row: %d", n)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"responder_claim":"roles","responder_value":"sec"}`); code != 409 || body["error"] != "set_by_operator" {
		t.Fatalf("the operator's pair: %d %s", code, lastBody)
	}
	_, read, _ := do(t, sam, http.MethodGet, base+preferencesPath, "")
	for _, key := range []string{"responder_emails", "responder_claim", "responder_value"} {
		if _, ok := read[key]; ok {
			t.Fatalf("a member is answered %s: %s", key, lastBody)
		}
	}
	if code, _, _ := do(t, sam, http.MethodPut, base+preferencesPath, `{"responder_emails":["sam@meridian.example"]}`); code != 403 {
		t.Fatalf("a member sets the responders: %d %s", code, lastBody)
	}
}

// Holding is a responder's or an admin's, never a level's: the owner, a contributor, a viewer and a member
// with no access are each responder_required naming the admins, and an agent holding the role is
// person_required. The responder's hold stops the container, takes production off envs, turns the pending
// approval outdated and writes stop app; then every production write is 409 held, even an admin's.
func TestOwnerCannotHold(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	if _, err := pool.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at)
		SELECT 'ABCDEFGH', project_id, gate_run, package_id, image, 'person', 'sub-olive', 'sub-olive', 'cli', now() + interval '1 day' FROM deploys WHERE env = 'prod'`); err != nil {
		t.Fatal(err)
	}
	trail := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'")
	for _, c := range []struct{ who, sub, email string }{{"the owner", "sub-olive", "olive@meridian.example"}, {"a contributor", "sub-carl", "carl@meridian.example"},
		{"a viewer", "sub-vic", "vic@meridian.example"}, {"a member with no access", "sub-nora", "nora@meridian.example"}} {
		if code, _, _ := do(t, signedIn(t, ctx, pool, base, c.sub, c.email, false), http.MethodPost, base+holdPath, holdReason); code != 403 || lastBody != holdRefusal {
			t.Errorf("%s holds: %d %s", c.who, code, lastBody)
		}
	}
	agent := signedIn(t, ctx, pool, base, "agent:claude-code/rhea", responderEmail, false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'agent:claude-code/rhea'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, agent, http.MethodPost, base+holdPath, holdReason); code != 403 || body["error"] != "person_required" {
		t.Fatalf("an agent under a responder's address holds: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM prod_holds") + count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'") - trail; n != 0 || fake.Stops.Load() != 0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != 5 {
		t.Fatalf("the refusals wrote %d row(s) beside their denied rows and stopped %d container(s)", n, fake.Stops.Load())
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, body, _ := do(t, rhea, http.MethodPost, base+holdPath, `{"reason":""}`); code != 400 || body["error"] != "invalid_reason" {
		t.Fatalf("a hold with no reason: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 || fake.Stops.Load() != 1 || fake.Stopped().Target != "prod" || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 0 {
		t.Fatalf("the responder holds: %d %s, %d stop(s)", code, lastBody, fake.Stops.Load())
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'stop app' AND actor_subject = 'sub-rhea' AND project_id = 'claims-portal'
		AND detail = jsonb_build_object('env', 'prod', 'reason', 'Exports claimant SSNs unauthenticated (INC-2231)', 'cancelled', '[]'::jsonb, 'package', (SELECT package_id FROM prod_holds), 'image_digest', '`+executortest.Digest+`', 'stop', 'stopped')`); n != 1 {
		t.Fatalf("the stop app row: %d", n)
	}
	if code, body, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 409 || body["error"] != "held" {
		t.Fatalf("holding again: %d %s", code, lastBody)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, w := range []struct {
		who          string
		c            *http.Client
		method, path string
		body         string
	}{
		{"the owner's promotion", olive, http.MethodPost, promotions, `{"gate_run":"gr_seed"}`},
		{"an admin's promotion", dana, http.MethodPost, promotions, `{"gate_run":"gr_seed"}`},
		{"approving the approval", olive, http.MethodPost, "/api/v1/promotion-approvals/ABCDEFGH/approve", ""},
		{"undeploying production", olive, http.MethodDelete, prodEnv, ""},
		{"deleting the app", olive, http.MethodDelete, "/api/v1/projects/claims-portal?confirm=claims-portal", ""},
	} {
		code, body, _ := do(t, w.c, w.method, base+w.path, w.body)
		hold, _ := body["hold"].(map[string]any)
		if code != 409 || body["error"] != "held" || hold["reason"] != "Exports claimant SSNs unauthenticated (INC-2231)" || len(body["admins"].([]any)) != 1 ||
			!strings.HasPrefix(body["message"].(string), "Production was stopped by sub-rhea on ") {
			t.Errorf("%s while held: %d %s", w.who, code, lastBody)
		}
	}
	_, ap, _ := do(t, olive, http.MethodGet, base+"/api/v1/promotion-approvals/ABCDEFGH", "")
	if held, _ := ap["held"].(map[string]any); ap["state"] != "outdated" || !heldBy(t, ctx, pool, held) {
		t.Fatalf("the approval asked before the hold says the hold: %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") + count(t, ctx, pool, "SELECT count(*) FROM projects WHERE deleting_since IS NOT NULL"); n != 0 {
		t.Fatalf("held, a promotion started or the delete marked the app: %d", n)
	}
	_, status, _ := do(t, olive, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	if receipt, _ := status["receipt"].(map[string]any); status["hold"] == nil || receipt["held"] == nil || len(receipt["rules"].([]any)) != 0 || receipt["blocked_by"].([]any)[0] != "prod.held" {
		t.Fatalf("the status read while held: %s", lastBody)
	}
}

// heldBy is whether held is the approval read's record of the hold claims-portal has now (contracts/routes.md,
// Production approvals): rhea held it, at the hold's instant, for holdReason's reason.
func heldBy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, held map[string]any) bool {
	t.Helper()
	var at string
	if err := pool.QueryRow(ctx, `SELECT to_char(held_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM prod_holds`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return string(mustJSON(held)) == `{"at":"`+at+`","by":{"display":"sub-rhea","kind":"person","subject":"sub-rhea"},"reason":"Exports claimant SSNs unauthenticated (INC-2231)"}`
}

// Lifting is a responder's off the app's team, or an admin's, from a browser alone: the role is read
// before the session, so the owner from the CLI is responder_required, and a responder or an admin from
// the CLI is dashboard_required. A responder holding a grant on the app never lifts its hold.
func TestLiftFromCLIIsDashboardRequired(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "carl@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  string
		c    *http.Client
		want string
	}{
		{"the owner in a browser", olive, liftRefusal},
		{"the owner from the CLI", fromCLI(t, ctx, pool, base, "sub-olive", "olive@meridian.example"), liftRefusal},
		{"a responder who contributes to the app", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), liftRefusal},
		{"the responder from the CLI", fromCLI(t, ctx, pool, base, "sub-rhea", responderEmail), liftInBrowser},
		{"an admin from the CLI", fromCLI(t, ctx, pool, base, "sub-dana", "dana@meridian.example"), liftInBrowser},
	} {
		if code, _, _ := do(t, c.c, http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != c.want {
			t.Errorf("%s lifts: %d %s", c.who, code, lastBody)
		}
	}
	if count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != 5 {
		t.Fatal("a refused lift touched the hold, or wrote no denied row")
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 0 {
		t.Fatalf("the responder lifts in a browser: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'lift hold' AND actor_subject = 'sub-rhea'
		AND detail @> '{"held_by":{"kind":"person","subject":"sub-rhea"},"reason":"Exports claimant SSNs unauthenticated (INC-2231)","started":false}' AND detail ? 'held_at'`); n != 1 {
		t.Fatalf("the lift hold row: %d", n)
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, base+holdPath, ""); code != 404 {
		t.Fatalf("lifting what is not held: %d %s", code, lastBody)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPost, base+holdPath, holdReason); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'stop app' AND actor_subject = 'sub-dana' AND NOT detail ? 'package' AND detail->>'stop' = 'nothing'") != 1 {
		t.Fatalf("an admin holds production running nothing: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, "start=true"), ""); code != 409 || body["error"] != "nothing_held" {
		t.Fatalf("starting a hold that ran nothing: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("an admin lifts in a browser: %d %s", code, lastBody)
	}
}

// A lift names the hold it read (held_at): one read before the hold now — a stale dialog — lifts nothing and
// starts nothing, and is told the hold there is now; one with no held_at is refused before anything is read.
func TestAStaleLiftLiftsNothing(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	var at time.Time
	if err := pool.QueryRow(ctx, "SELECT held_at FROM prod_holds").Scan(&at); err != nil {
		t.Fatal(err)
	}
	now := at.UTC().Truncate(time.Second).Format(time.RFC3339)
	stale := neturl.QueryEscape(at.Add(-time.Hour).UTC().Format(time.RFC3339))
	for _, query := range []string{"?held_at=" + stale, "?held_at=" + stale + "&start=true"} {
		code, body, _ := do(t, rhea, http.MethodDelete, base+holdPath+query, "")
		hold, _ := body["hold"].(map[string]any)
		if code != 409 || body["error"] != "hold_changed" || body["message"] != "production's hold changed since you read it: read it again" || hold["at"] != now || hold["reason"] != "Exports claimant SSNs unauthenticated (INC-2231)" {
			t.Errorf("a stale lift %s: %d %s", query, code, lastBody)
		}
	}
	for _, query := range []string{"", "?held_at=yesterday"} {
		if code, body, _ := do(t, rhea, http.MethodDelete, base+holdPath+query, ""); code != 400 || body["error"] != "invalid_held_at" {
			t.Errorf("a lift naming no hold %q: %d %s", query, code, lastBody)
		}
	}
	if count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'lift hold'") != 0 || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatal("a stale lift touched the hold, wrote a lift hold row or started a run")
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, base+holdPath+"?held_at="+neturl.QueryEscape(now), ""); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 0 {
		t.Fatalf("the lift of the hold read: %d %s", code, lastBody)
	}
}

// Start again promotes the held image's exact bytes, with no build and no review, as the one production
// start a responder or an admin makes: production runs the digest it ran when held, and the trail says
// it was a restart, naming the version it started (#2244).
func TestStartAgainRunsTheHeldDigest(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	var held, deployer, pkg string
	if err := pool.QueryRow(ctx, "SELECT image, deployed_by_subject, package_id FROM deploys WHERE env = 'prod'").Scan(&held, &deployer, &pkg); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 0 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	builds, ran, gates := fake.Builds.Load(), fake.Runs.Load(), count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'gate'")
	code, body, _ := do(t, c, http.MethodDelete, liftPath(t, ctx, pool, base, "start=true"), "")
	run, _ := runOf(body)["id"].(string)
	if code != 202 || runOf(body)["state"] != "pending" || waitVerdict(t, c, base, run)["state"] != "clean" {
		t.Fatalf("start again: %d %s", code, lastBody)
	}
	if r := fake.Ran(); fake.Builds.Load() != builds || fake.Runs.Load() != ran+1 || r.Target != "prod" || r.Image != held || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'gate'") != gates {
		t.Fatalf("one /run of the held image, no build, no review: %d builds, %d runs, %+v", fake.Builds.Load()-builds, fake.Runs.Load()-ran, r)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM deploys WHERE env = 'prod' AND image = '`+held+`' AND gate_run = '`+deploy+`' AND deployed_by_subject = '`+deployer+`'`); n != 1 {
		t.Fatalf("production's row, under the deployer the held row named: %d", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'deploy prod' AND run_id = '`+run+`' AND detail @> '{"restart":true}'`) +
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'lift hold' AND run_id = '`+run+`' AND detail @> '{"started":true,"package":"`+pkg+`"}'`); n != 2 {
		t.Fatalf("the trail names the restart and the version it started: %d of 2 rows", n)
	}
	stops := fake.Stops.Load()
	if code, _, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 204 || fake.Stops.Load() != stops+1 || count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 0 {
		t.Fatalf("held again once it runs again: %d %s", code, lastBody)
	}
}

// plantHold holds claims-portal's production as the hold route records it, by rhea, at a fixed instant.
func plantHold(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO prod_holds (project_id, package_id, image, gate_run, reason, held_by_kind, held_by_subject, held_by_display, held_at)
		SELECT 'claims-portal', d.package_id, d.image, d.gate_run, 'Exports claimant SSNs unauthenticated (INC-2231)', 'person', 'rhea@meridian.example', 'Rhea Okafor', '2026-10-11T09:40:00Z'
		FROM (SELECT) one LEFT JOIN deploys d ON d.project_id = 'claims-portal' AND d.env = 'prod'`); err != nil {
		t.Fatal(err)
	}
}

// The org's deployed report on production while it is held writes nothing and is 409 held, naming no
// hold and no admins — the token is the platform's — and Canyon sends a remove of what it started, which
// supersedes the release; the same report again is superseded and sends another, as every deployed report
// while held does: a stop recorded on its run is no stop pending once its release went out.
func TestOrgReportCannotWriteProdWhileHeld(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	for _, q := range []string{
		"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
		"INSERT INTO runs (id, project_id, package_id, state, kind, started_by_kind, started_by_subject, started_by_display, stop_requested_at) VALUES ('gr_prom01', 'claims-portal', 'pkg_7d3e8f', 'running', 'promote', 'person', 'priya@meridian.example', 'Priya Raman', now())",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run) VALUES ('rel_prod1', 'claims-portal', 'prod', 'gr_prom01', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'accepted', now(), 'gr_8f2a')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	plantHold(t, ctx, pool)
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 409 || lastBody != `{"error":"held","message":"production is held"}` {
		t.Fatalf("a deployed report while held: %d %s", code, lastBody)
	}
	if code, body, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 409 || body["error"] != "superseded" { // the remove supersedes it
		t.Fatalf("the same report again: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'") + count(t, ctx, pool, "SELECT count(*) FROM deploy_history"); n != 0 {
		t.Fatalf("production written while held: %d row(s)", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'prod' AND package_id = 'pkg_7d3e8f' AND image = '"+reportImage+"'"); n != 2 {
		t.Fatalf("the removes sent: %d, want one per deployed report while held", n)
	}
}

// A held app is never claimed: it runs nothing, holds no secret and has no owner, and still the claim is
// owner_required, as one running something is.
func TestAHeldAppIsNeverClaimed(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-portal')"); err != nil {
		t.Fatal(err)
	}
	plantHold(t, ctx, pool)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	claimable := func() (bool, bool) {
		_, page, _ := do(t, rhea, http.MethodGet, base+"/api/v1/fleet", "")
		_, read, _ := do(t, rhea, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		return fleetRowOf(page, "claims-portal")["claimable"] == true, read["project"].(map[string]any)["claimable"] == true
	}
	if row, read := claimable(); row || read {
		t.Fatalf("a held app reads claimable: fleet row %v, project read %v", row, read)
	}
	if code, body, _ := do(t, rhea, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Rhea"}`); code != 403 || body["error"] != "owner_required" {
		t.Fatalf("a responder claims a held app: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM prod_holds"); err != nil {
		t.Fatal(err)
	}
	if row, read := claimable(); !row || !read {
		t.Fatalf("once lifted it reads claimable: fleet row %v, project read %v", row, read)
	}
	if code, _, _ := do(t, rhea, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Rhea"}`); code != 200 {
		t.Fatalf("the same claim once the hold is lifted: %d %s", code, lastBody)
	}
}

// While held, a caller below a route's level gets that route's refusal and no word of the hold — a viewer,
// a member with no access, a responder off the team; a responder who contributes gets 409 held where a
// contributor may write. Deleting lists what it would destroy before it names the hold.
func TestHeldRefusalsFollowTheCallersLevel(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	if _, err := pool.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at)
		SELECT 'ABCDEFGH', project_id, gate_run, package_id, image, 'person', 'sub-olive', 'sub-olive', 'cli', now() + interval '1 day' FROM deploys WHERE env = 'prod'`); err != nil {
		t.Fatal(err)
	}
	plantHold(t, ctx, pool)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "carl@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	writes := []struct{ name, method, path, body string }{
		{"promote", http.MethodPost, promotions, `{"gate_run":"gr_seed"}`},
		{"approve", http.MethodPost, "/api/v1/promotion-approvals/ABCDEFGH/approve", ""},
		{"undeploy production", http.MethodDelete, prodEnv, ""},
		{"delete", http.MethodDelete, "/api/v1/projects/claims-portal?confirm=claims-portal", ""},
	}
	for _, c := range []struct {
		who        string
		c          *http.Client
		want, last string // the refusal of the first three writes, then of the delete
	}{
		{"a viewer", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), "team_required", "owner_required"},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), "team_required", "owner_required"},
		{"a responder off the team", signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), "team_required", "owner_required"},
		{"a responder who contributes", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), "held", "owner_required"},
	} {
		for i, wr := range writes {
			want := c.want
			if i == len(writes)-1 {
				want = c.last
			}
			code, body, _ := do(t, c.c, wr.method, base+wr.path, wr.body)
			_, named := body["hold"]
			if body["error"] != want || (want == "held") != (code == 409 && named) || (want != "held" && (code != 403 || named)) {
				t.Errorf("%s, %s while held: %d %s, want %s", c.who, wr.name, code, lastBody, want)
			}
		}
	}
	if code, body, _ := do(t, olive, http.MethodDelete, base+"/api/v1/projects/claims-portal", ""); code != 409 || body["error"] != "confirm_required" {
		t.Fatalf("the owner's delete without confirm: %d %s, want the listing first", code, lastBody)
	}
	if code, body, _ := do(t, olive, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 409 || body["error"] != "held" {
		t.Fatalf("the owner's confirmed delete: %d %s", code, lastBody)
	}
}

// The trail: a responder reads the stop app and lift hold rows they made, on an app they have no grant on;
// another member reads neither; the app's team reads both under the app, among its governance rows.
func TestTheHoldsRowsOnTheTrail(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("the lift: %d %s", code, lastBody)
	}
	actions := func(c *http.Client, query string) string {
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit"+query, "")
		var got []string
		for _, e := range page["events"].([]any) {
			if a := e.(map[string]any)["action"].(string); a == "stop app" || a == "lift hold" {
				got = append(got, a)
			}
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct {
		who, query, want string
		c                *http.Client
	}{
		{"the responder", "", "lift hold,stop app", rhea},
		{"another member", "", "", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)},
		{"the owner, under the app", "?project=claims-portal", "lift hold,stop app", olive},
		{"the owner, governance", "?category=governance", "lift hold,stop app", olive},
		{"the owner, access", "?category=access", "", olive},
	} {
		if got := actions(c.c, c.query); got != c.want {
			t.Errorf("%s reads %q, want %q", c.who, got, c.want)
		}
	}
}

// A hold that meets a promotion switching production answers 202 stopping; the run stops the container
// once its switch lands and writes the one stop app row, naming it as cancelled.
func TestAHoldMeetingAPromotionIsCarriedOutByTheRun(t *testing.T) {
	pool, ctx, base, c, deploy := previewed(t)
	parked := make(chan struct{})
	fake.HoldRun.Store(&parked)
	var once sync.Once
	release := func() { once.Do(func() { fake.HoldRun.Store(nil); close(parked) }) }
	t.Cleanup(release)
	ran := fake.Runs.Load()
	_, started, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
	run, _ := runOf(started)["id"].(string)
	for deadline := time.Now().Add(15 * time.Second); fake.Runs.Load() == ran; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the promotion never reached /run")
		}
	}
	code, body, _ := do(t, c, http.MethodPost, base+holdPath, holdReason)
	if stopping, _ := body["stopping"].(map[string]any); code != 202 || runOf(stopping)["id"] != run {
		t.Fatalf("a hold mid-switch: %d %s", code, lastBody)
	}
	release()
	if state := waitVerdict(t, c, base, run)["state"]; state != "failed" {
		t.Fatalf("the promotion the hold met: %s", lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'stop app' AND run_id = '`+run+`' AND detail->'cancelled' = '["`+run+`"]' AND detail->>'stop' = 'stopped'`); n != 1 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'stop app'") != 1 {
		t.Fatalf("the stop app row: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM deploys WHERE env = 'prod'"); n != 0 {
		t.Fatalf("production's row after the stop: %d", n)
	}
}

// A /stop that fails keeps the hold and answers 500; the retry resumes and writes one stop app row, with
// the first request's reason, whatever the retry says.
func TestAFailedStopResumesWithTheFirstReason(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	fake.FailStop.Store(true)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 500 || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 {
		t.Fatalf("a failed stop: %d %s", code, lastBody)
	}
	fake.FailStop.Store(false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, `{"reason":"a second reason"}`); code != 204 {
		t.Fatalf("the retry: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'stop app'"); n != 1 ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'stop app' AND detail->>'reason' = 'Exports claimant SSNs unauthenticated (INC-2231)'") != 1 {
		t.Fatalf("stop app rows: %d, want one with the first reason", n)
	}
}

// Under the org's platform the hold sends a remove in place of /stop: 202 {release}, one stop app row,
// and asking again answers the open remove and writes none.
func TestAnOrgRunHoldSendsARemove(t *testing.T) {
	pool, ctx, base, _, _ := reportFixture(t)
	if _, err := pool.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('claims-portal', 'prod', 'pkg_7d3e8f', '"+reportImage+"', 'https://claims.apps.meridian.example', 'gr_8f2a', 'org')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'"); err != nil {
		t.Fatal(err)
	}
	c, stops := client(t), fake.Stops.Load()
	code, body, _ := do(t, c, http.MethodPost, base+holdPath, holdReason)
	release, _ := body["release"].(string)
	if code != 202 || release == "" || fake.Stops.Load() != stops {
		t.Fatalf("an org-run hold: %d %s", code, lastBody)
	}
	if code, again, _ := do(t, c, http.MethodPost, base+holdPath, holdReason); code != 202 || again["release"] != release {
		t.Fatalf("asked again: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'stop app' AND detail->>'image_digest' = 'sha256:"+reportDigest+"'"); n != 1 {
		t.Fatalf("stop app rows: %d", n)
	}
}

// The held status read and its refusal are the contract's fixtures, byte for byte where they are the
// hold's: contracts/testdata/app-page/claims-portal-held-status.json and -held-refusal.json.
func TestTheHeldReadsAreTheGoldenShape(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	for _, q := range []string{
		"UPDATE packages SET label = 'v1.3'",
		"UPDATE org_preferences SET admin_emails = '{dana@meridian.example,founder@meridian.example}'",
		`INSERT INTO prod_holds (project_id, package_id, image, gate_run, reason, held_by_kind, held_by_subject, held_by_display, held_at)
			SELECT 'claims-portal', id, 'canyon/claims-portal@` + executortest.Digest + `', 'gr_6c12', 'Exports claimant SSNs unauthenticated (INC-2231)', 'person', 'rhea@meridian.example', 'Rhea Okafor', '2026-10-11T09:40:00Z' FROM packages`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	golden := func(name string) map[string]any {
		raw, err := os.ReadFile("../../contracts/testdata/app-page/" + name) //nolint:gosec // test: one of two fixture names this test passes
		var doc map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &doc)
		}
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	status := golden("claims-portal-held-status.json")
	_, got, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	for _, key := range []string{"hold", "receipt"} {
		if string(mustJSON(got[key])) != string(mustJSON(status[key])) {
			t.Errorf("the status read's %s:\n got %s\nwant %s", key, mustJSON(got[key]), mustJSON(status[key]))
		}
	}
	want := golden("claims-portal-held-refusal.json")
	if code, refused, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"gr_6c12"}`); code != 409 || string(mustJSON(refused)) != string(mustJSON(want)) {
		t.Fatalf("the refusal:\n got %d %s\nwant %s", code, lastBody, mustJSON(want))
	}
}

// The lift's team is the owner and contributor grants, now or as the hold recorded them: a responder
// who contributed when production was held — by address, or by subject under an address their identity
// provider changed since — or is in a group that did, never lifts it, though the grant is gone; a
// responder holding only a viewer grant lifts it.
func TestALiftIsRefusedToTheTeamTheHoldRecorded(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'group', 'payments', 'contributor', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'ria@meridian.example', 'sub-ria', 'contributor', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{responderEmail, "carl@meridian.example", "gus@meridian.example", "vic@meridian.example", "ria.new@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE grantee IN ('carl@meridian.example', 'payments', 'ria@meridian.example')"); err != nil {
		t.Fatal(err)
	}
	gus := signedIn(t, ctx, pool, base, "sub-gus", "gus@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET groups = '{payments}' WHERE actor_subject = 'sub-gus'"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who string
		c   *http.Client
	}{{"a responder who contributed then", signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)}, {"a responder in a group that contributed then", gus},
		{"a responder who contributed then, known by subject under a new address", signedIn(t, ctx, pool, base, "sub-ria", "ria.new@meridian.example", false)}} {
		if code, _, _ := do(t, c.c, http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != liftRefusal {
			t.Errorf("%s lifts: %d %s", c.who, code, lastBody)
		}
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("a responder holding a viewer grant lifts: %d %s", code, lastBody)
	}
}

// The lift's other ways: a run of the app active refuses start=true and keeps the hold; start must be
// exactly true; a lift leaves the approvals the hold outdated as they are, saying the hold. A hold is taken from the CLI as
// from a browser; an agent is responder_required by its member role, and an admin's agent or Canyon
// itself person_required.
func TestTheLiftsOtherWays(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	if _, err := pool.Exec(ctx, `INSERT INTO promotion_approvals (code, project_id, gate_run, package_id, image, requested_by_kind, requested_by_subject, requested_by_display, requested_via, expires_at)
		SELECT 'ABCDEFGH', project_id, gate_run, package_id, image, 'person', 'sub-olive', 'sub-olive', 'cli', now() + interval '1 day' FROM deploys WHERE env = 'prod'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ who, sub, email, kind, want string }{
		{"an agent of a member", "agent:cursor/nora", "nora@meridian.example", "agent", "responder_required"},
		{"an admin's agent", "agent:cursor/dana", "dana@meridian.example", "agent", "person_required"},
		{"Canyon itself under an admin's address", "canyon:brake", "dana@meridian.example", "canyon", "person_required"},
	} {
		cl := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $2 WHERE actor_subject = $1", c.sub, c.kind); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := do(t, cl, http.MethodPost, base+holdPath, holdReason); code != 403 || body["error"] != c.want {
			t.Errorf("%s holds: %d %s, want %s", c.who, code, lastBody, c.want)
		}
	}
	if code, _, _ := do(t, fromCLI(t, ctx, pool, base, "sub-rhea", responderEmail), http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("a responder holds from the CLI: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind) SELECT 'gr_busy', project_id, id, 'running', 'gate' FROM packages"); err != nil {
		t.Fatal(err)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, body, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, "start=true"), ""); code != 409 || body["error"] != "run_in_progress" || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 {
		t.Fatalf("start again while a run is active: %d %s", code, lastBody)
	}
	_, held, _ := do(t, olive, http.MethodGet, base+"/api/v1/promotion-approvals/ABCDEFGH", "")
	if code, _, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, "start=yes"), ""); code != 204 || count(t, ctx, pool, "SELECT count(*) FROM runs WHERE kind = 'promote'") != 0 {
		t.Fatalf("start=yes is no start: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM promotion_approvals WHERE state = 'outdated'"); n != 1 {
		t.Fatalf("the approval the hold outdated, after the lift: %d outdated", n)
	}
	if _, ap, _ := do(t, olive, http.MethodGet, base+"/api/v1/promotion-approvals/ABCDEFGH", ""); held["held"] == nil || string(mustJSON(ap["held"])) != string(mustJSON(held["held"])) {
		t.Fatalf("the approval still says the hold after the lift: %s, before it %s", lastBody, mustJSON(held))
	}
}

// An app being deleted takes no hold and starts nothing again (409 deleting), and a lift while an undeploy
// or a hold of the app holds its undeploy lock is 409 undeploying, the hold kept.
func TestDeletingAndUndeployingRefuseTheHoldRoutes(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleting_since = now()"); err != nil {
		t.Fatal(err)
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, body, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 409 || body["error"] != "deleting" || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 0 {
		t.Fatalf("holding an app being deleted: %d %s", code, lastBody)
	}
	plantHold(t, ctx, pool)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, body, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, "start=true"), ""); code != 409 || body["error"] != "deleting" || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 {
		t.Fatalf("starting again an app being deleted: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleting_since = NULL"); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	unlock, err := runs.LockUndeploy(ctx, conn.Conn(), "claims-portal")
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, dana, http.MethodDelete, base+holdPath, ""); code != 409 || body["error"] != "undeploying" || count(t, ctx, pool, "SELECT count(*) FROM prod_holds") != 1 {
		t.Fatalf("lifting while the undeploy lock is held: %d %s", code, lastBody)
	}
	unlock()
	if code, _, _ := do(t, dana, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("lifting once the lock is let go: %d %s", code, lastBody)
	}
}

// A grant made to an address before its person signed in is bound to their subject at sign-in, so the
// hold's team record names them by subject: a contributor who later signs in under another address, with
// the responder claim, never lifts the hold placed while they contributed, though the grant is gone.
func TestAnEmailGrantIsBoundAtSignInSoANewAddressNeverLifts(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example",
		"CANYON_RESPONDER_CLAIM": "groups", "CANYON_RESPONDER_VALUE": "security-responders"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	signIn := func(email string, groups []any) *http.Client {
		idp.bend = map[string]any{"email": email, "email_verified": true, "groups": groups, "sub": "sub-carl"}
		b := browser(t)
		callback, _, _, _ := startSignIn(t, b, base, idp)
		if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
			t.Fatalf("%s signs in: %d %s", email, resp.StatusCode, resp.Header.Get("Location"))
		}
		return b
	}
	signIn("carl@meridian.example", []any{})
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE grantee = 'carl@meridian.example' AND subject = 'sub-carl'"); n != 1 {
		t.Fatalf("the grant bound to its person's subject: %d", n)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the admin holds: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE grantee = 'carl@meridian.example'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, signIn("carl.new@meridian.example", []any{"security-responders"}), http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != liftRefusal {
		t.Fatalf("the contributor, a responder under a new address, lifts: %d %s", code, lastBody)
	}
}

// While held, production reads as empty to anyone who cannot read the hold — a viewer, a member with no
// access — on the project read and the fleet, even where its row stays: one the org runs, or a stop that
// failed. A responder reads it, and its hold.
func TestAHeldProductionReadsEmptyBelowTheHold(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	plantHold(t, ctx, pool) // production's row stays, as an org's does until its platform reports removed
	for _, c := range []struct {
		who      string
		c        *http.Client
		wantProd bool
	}{
		{"a viewer", signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), false},
		{"a member with no access", signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false), false},
		{"a responder", signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false), true},
	} {
		_, page, _ := do(t, c.c, http.MethodGet, base+"/api/v1/fleet", "")
		_, read, _ := do(t, c.c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		for name, row := range map[string]map[string]any{"fleet row": fleetRowOf(page, "claims-portal"), "project read": read["project"].(map[string]any)} {
			envs, _ := row["envs"].(map[string]any)
			_, prod := envs["prod"]
			if _, hold := row["hold"]; prod != c.wantProd || hold != c.wantProd {
				t.Errorf("%s, %s: envs %v, hold %v; want production and the hold %v", c.who, name, envs, row["hold"], c.wantProd)
			}
		}
	}
}

// A sign-in binds a grant to its person only on an address the provider asserts verified in its email
// claim, only a grant naming no subject yet under its issuer, and says so on the trail, naming the issuer;
// a grant bound to one person never names another who signs in with that address later, and one bound
// under another issuer is bound again under this one (#2306).
func TestABindingNeedsAVerifiedAddressAndNeverRebinds(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, subject_issuer, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'carl@meridian.example', '', '', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'anna@meridian.example', 'sub-anna', '` + idp.URL + `', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'olga@meridian.example', 'sub-olga', 'https://login.contoso.example', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'pia@meridian.example', 'sub-pia', '', 'contributor', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stampIssuer(ctx, pool, idp.URL); err != nil { // as the server's start does: pia's grant was bound before issuers were recorded
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'quinn@meridian.example', 'sub-quinn', 'contributor', 'person', 'sub-dana', '')`); err != nil { // bound since by a binary that records no issuer, mid-upgrade
		t.Fatal(err)
	}
	signIn := func(bend map[string]any) *http.Client {
		idp.bend = bend
		b := browser(t)
		callback, _, _, _ := startSignIn(t, b, base, idp)
		if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
			t.Fatalf("%v signs in: %d %s", bend, resp.StatusCode, resp.Header.Get("Location"))
		}
		return b
	}
	subjectOf := func(grantee string) string {
		var s string
		if err := pool.QueryRow(ctx, "SELECT subject FROM project_access WHERE grantee = $1", grantee).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, bend := range map[string]map[string]any{
		"no email_verified":         {"email": "carl@meridian.example", "sub": "sub-carl"},
		"only a preferred_username": {"preferred_username": "carl@meridian.example", "email_verified": true, "sub": "sub-carl"},
	} {
		signIn(bend)
		if got := subjectOf("carl@meridian.example"); got != "" || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'bind grants'") != 0 {
			t.Fatalf("%s bound the grant to %q", name, got)
		}
	}
	signIn(map[string]any{"email": "carl@meridian.example", "email_verified": true, "sub": "sub-carl"})
	if got := subjectOf("carl@meridian.example"); got != "sub-carl" ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'bind grants' AND actor_subject = 'sub-carl' AND detail = '{"email": "carl@meridian.example", "issuer": "`+idp.URL+`", "projects": ["claims-portal"]}'`) != 1 {
		t.Fatalf("a verified address binds the grant, on the trail: %q", got)
	}
	olga := signIn(map[string]any{"email": "olga@meridian.example", "email_verified": true, "sub": "sub-olga-here"})
	var issuer string
	if err := pool.QueryRow(ctx, "SELECT subject_issuer FROM project_access WHERE grantee = 'olga@meridian.example'").Scan(&issuer); err != nil || subjectOf("olga@meridian.example") != "sub-olga-here" || issuer != idp.URL ||
		count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'bind grants' AND actor_subject = 'sub-olga-here' AND detail = '{"email": "olga@meridian.example", "issuer": "`+idp.URL+`", "projects": ["claims-portal"], "rebound": [{"issuer": "https://login.contoso.example", "project": "claims-portal", "subject": "sub-olga"}]}'`) != 1 {
		t.Fatalf("a grant bound under another issuer is bound again under this one, on the trail: %q under %q (%v)", subjectOf("olga@meridian.example"), issuer, err)
	}
	if code, _, _ := do(t, olga, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
		t.Fatalf("the person it is bound to again reads the app: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, olga, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Olga"}`); code != 200 {
		t.Fatalf("the person it is bound to again claims the app: %d %s", code, lastBody)
	}
	impostor := signIn(map[string]any{"email": "anna@meridian.example", "email_verified": true, "sub": "sub-someone-else"})
	if got := subjectOf("anna@meridian.example"); got != "sub-anna" {
		t.Fatalf("a bound grant was rebound to %q", got)
	}
	if code, body, _ := do(t, impostor, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 403 || body["error"] != "team_required" {
		t.Fatalf("another person holding a bound grant's address reads the app: %d %s", code, lastBody)
	}
	if signIn(map[string]any{"email": "quinn@meridian.example", "email_verified": true, "sub": "sub-not-quinn"}); subjectOf("quinn@meridian.example") != "sub-quinn" {
		t.Fatalf("a grant a binary that records no issuer bound was rebound to %q", subjectOf("quinn@meridian.example"))
	}
	pias := signIn(map[string]any{"email": "pia@meridian.example", "email_verified": true, "sub": "sub-not-pia"})
	if got := subjectOf("pia@meridian.example"); got != "sub-pia" {
		t.Fatalf("a grant bound before issuers were recorded was rebound to %q", got)
	}
	if code, body, _ := do(t, pias, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 403 || body["error"] != "team_required" {
		t.Fatalf("another person at the address of a grant bound before issuers were recorded reads the app: %d %s", code, lastBody)
	}
}

// A person grant names its person and nobody else: bound under Canyon's sign-in's issuer, by its subject
// alone — never another person holding its address, old or current; unbound, by its address as stored,
// whatever its case in the session; bound under another issuer, by its address as though unbound, never by
// its subject; bound by a binary that recorded no issuer — a rolling upgrade's — as Canyon's sign-in's, by its
// subject alone, as that binary read it (#2306); a group grant by the group; and a caller with no subject or
// no address names no grant by it.
func TestAGrantMatchesItsPersonAndNobodyElse(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, subject_issuer, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'anna@meridian.example', 'sub-anna', '` + goodOIDC["CANYON_OIDC_ISSUER"] + `', 'owner', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'carl@meridian.example', '', '', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'ivo@meridian.example', 'sub-ivo', 'https://login.contoso.example', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'lea@meridian.example', 'sub-lea', '', 'viewer', 'person', 'sub-dana', ''),
			       ('claims-portal', 'group', 'payments', '', '', 'contributor', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		who, sub, email, group, want string
	}{
		{"the bound person under a new address", "sub-anna", "anna.w@meridian.example", "", "owner"},
		{"another person holding the bound grant's address", "sub-bob", "anna@meridian.example", "", "none"},
		{"the unbound grant's address, in another case", "sub-carl", "Carl@Meridian.Example", "", "contributor"},
		{"another address", "sub-x", "carl.other@meridian.example", "", "none"},
		{"another issuer's subject of a grant bound under it", "sub-ivo", "ivo.new@meridian.example", "", "none"},
		{"the address of a grant bound under another issuer", "sub-ivan", "ivo@meridian.example", "", "contributor"},
		{"the subject of a grant bound with no issuer recorded", "sub-lea", "lea.new@meridian.example", "", "viewer"},
		{"another person at its address", "sub-leah", "lea@meridian.example", "", "none"},
		{"a member of the granted group", "sub-gus", "gus@meridian.example", "payments", "contributor"},
		{"a caller with no subject", "", "nora@meridian.example", "", "none"},
	} {
		cl := signedIn(t, ctx, pool, base, c.sub, c.email, false)
		if c.group != "" {
			if _, err := pool.Exec(ctx, "UPDATE sessions SET groups = $2 WHERE actor_subject = $1", c.sub, []string{c.group}); err != nil {
				t.Fatal(err)
			}
		}
		_, read, _ := do(t, cl, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		project, _ := read["project"].(map[string]any)
		access, _ := project["access"].(map[string]any)
		if access["level"] != c.want {
			t.Errorf("%s reads the app at %v, want %s: %s", c.who, access["level"], c.want, lastBody)
		}
	}
}

// A grant bound before issuers were recorded is Canyon's current sign-in's, as the binary before read it: the
// server's start records that issuer on it, and on a pending request's subject, so it still names its person
// alone, never another at its address (#2306). With sign-in off nothing is recorded.
func TestAGrantBoundBeforeIssuersWereIsCanyonsIssuers(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'ola@meridian.example', 'sub-ola', 'contributor', 'person', 'sub-dana', ''),
			       ('claims-portal', 'email', 'carl@meridian.example', '', 'viewer', 'person', 'sub-dana', '')`,
		"INSERT INTO access_requests (project_id, email, subject) VALUES ('claims-portal', 'ned@meridian.example', 'sub-ned')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	issuer := goodOIDC["CANYON_OIDC_ISSUER"]
	if n, err := stampIssuer(ctx, pool, issuer); err != nil || n != 2 ||
		count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE subject_issuer = '"+issuer+"'") != 1 || count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE subject_issuer = '"+issuer+"'") != 1 {
		t.Fatalf("the bound grant and the request, stamped: %d (%v)", n, err)
	}
	base, _ := start(t, pool, time.Hour)
	for _, c := range []struct{ who, sub, email, want string }{
		{"its person under a new address", "sub-ola", "ola.new@meridian.example", "contributor"},
		{"another person at its address", "sub-other", "ola@meridian.example", "none"},
	} {
		_, read, _ := do(t, signedIn(t, ctx, pool, base, c.sub, c.email, false), http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		project, _ := read["project"].(map[string]any)
		if access, _ := project["access"].(map[string]any); access["level"] != c.want {
			t.Errorf("%s reads the app at %v, want %s: %s", c.who, access["level"], c.want, lastBody)
		}
	}
}

// A subject bound with sign-in off is the anonymous admin's, never an identity provider's: once sign-in is on
// it matches nobody, not an account whose sub is the same string, and its grant is read as not yet bound, by
// address — whether bound by this binary or one before issuers were recorded (#2306).
func TestAGrantBoundWithSignInOffNamesNoIdPSubject(t *testing.T) {
	pool, ctx := migrated(t)
	off, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('payroll'), ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'olive@meridian.example', 'anonymous', 'owner', 'person', 'anonymous', '')`, // a binary before issuers were recorded, sign-in off
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := do(t, client(t), http.MethodPut, off+"/api/v1/projects/payroll/owner", `{"display":"Anonymous"}`); code != 200 {
		t.Fatalf("the anonymous admin's claim with sign-in off: %d %s", code, lastBody)
	}
	withOIDC(t)                                                                                  // the operator turns sign-in on: the start records the issuer on what it has to
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET onboarded_at = now()"); err != nil { // an onboarded host, as migrated leaves one under oidc
		t.Fatal(err)
	}
	if _, err := stampIssuer(ctx, pool, goodOIDC["CANYON_OIDC_ISSUER"]); err != nil {
		t.Fatal(err)
	}
	base, _ := start(t, pool, time.Hour)
	for _, c := range []struct{ who, sub, email, app, want string }{
		{"an account whose sub is the anonymous admin's", "anonymous", "anon@meridian.example", "payroll", "none"},
		{"the same, on the older grant", "anonymous", "anon@meridian.example", "claims-portal", "none"},
		{"the owner at the older grant's address", "sub-olive", "olive@meridian.example", "claims-portal", "owner"},
	} {
		_, read, _ := do(t, signedIn(t, ctx, pool, base, c.sub, c.email, false), http.MethodGet, base+"/api/v1/projects/"+c.app, "")
		project, _ := read["project"].(map[string]any)
		if access, _ := project["access"].(map[string]any); access["level"] != c.want {
			t.Errorf("%s reads %s at %v, want %s: %s", c.who, c.app, access["level"], c.want, lastBody)
		}
	}
}

// Every path that writes a grant's subject applies sign-in's rule: only a session whose address the
// provider asserted verified binds it. Unverified, the app its person creates, the app they claim and the
// access an owner shares on their request each name them by address alone; verified, by subject.
func TestEveryGrantPathBindsOnAVerifiedAddressAlone(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	unverified := func(sub, email string) *http.Client {
		c := signedIn(t, ctx, pool, base, sub, email, false)
		if _, err := pool.Exec(ctx, "UPDATE sessions SET email_verified = false WHERE actor_subject = $1", sub); err != nil {
			t.Fatal(err)
		}
		return c
	}
	issuer := goodOIDC["CANYON_OIDC_ISSUER"]
	subjectOf := func(project, grantee string) string { // the subject, and the issuer after a space: none for no subject (#2306)
		var s string
		if err := pool.QueryRow(ctx, "SELECT rtrim(subject || ' ' || subject_issuer) FROM project_access WHERE project_id = $1 AND grantee = $2", project, grantee).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	una := unverified("sub-una", "una@meridian.example")
	if code, _, _ := do(t, una, http.MethodPost, base+"/api/v1/projects", `{"id":"made-unverified"}`); code != 201 || subjectOf("made-unverified", "una@meridian.example") != "" {
		t.Fatalf("an app created on an unverified address: %d %s, its owner grant %q", code, lastBody, subjectOf("made-unverified", "una@meridian.example"))
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-vera", "vera@meridian.example", false), http.MethodPost, base+"/api/v1/projects", `{"id":"made-verified"}`); code != 201 || subjectOf("made-verified", "vera@meridian.example") != "sub-vera "+issuer {
		t.Fatalf("an app created on a verified address: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('unclaimed')"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, una, http.MethodPut, base+"/api/v1/projects/unclaimed/owner", `{"display":"Una"}`); code != 200 || subjectOf("unclaimed", "una@meridian.example") != "" {
		t.Fatalf("a claim on an unverified address: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, unverified("sub-ned", "ned@meridian.example"), http.MethodPost, base+"/api/v1/projects/made-verified/access-requests", ""); code != 201 ||
		count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'ned@meridian.example' AND subject = '' AND subject_issuer = ''") != 1 {
		t.Fatalf("an access request on an unverified address: %d %s", code, lastBody)
	}
	vera := signedIn(t, ctx, pool, base, "sub-vera", "vera@meridian.example", false)
	if code, _, _ := do(t, vera, http.MethodPut, base+"/api/v1/projects/made-verified/access/email/ned@meridian.example", `{"level":"viewer"}`); code != 200 || subjectOf("made-verified", "ned@meridian.example") != "" {
		t.Fatalf("the share of an unverified request: %d %s", code, lastBody)
	}
}

// A contributor not yet bound when production was held is on its record by address alone; their first
// verified sign-in after it adds their subject to the record, so once the grant goes and their address
// changes they still never lift it, start=true or not.
func TestABindingAfterTheHoldStillKeepsTheTeamOff(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example",
		"CANYON_RESPONDER_CLAIM": "groups", "CANYON_RESPONDER_VALUE": "security-responders"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the admin holds: %d %s", code, lastBody)
	}
	signIn := func(email string, groups []any) *http.Client {
		idp.bend = map[string]any{"email": email, "email_verified": true, "groups": groups, "sub": "sub-carl"}
		b := browser(t)
		callback, _, _, _ := startSignIn(t, b, base, idp)
		if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
			t.Fatalf("%s signs in: %d %s", email, resp.StatusCode, resp.Header.Get("Location"))
		}
		return b
	}
	signIn("carl@meridian.example", []any{})
	if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE grantee = 'carl@meridian.example'"); err != nil {
		t.Fatal(err)
	}
	carl := signIn("carl.new@meridian.example", []any{"security-responders"})
	for _, path := range []string{holdPath + "?start=true", holdPath} {
		if code, _, _ := do(t, carl, http.MethodDelete, base+path, ""); code != 403 || lastBody != liftRefusal {
			t.Fatalf("the contributor, bound after the hold, under a new address, lifts %s: %d %s", path, code, lastBody)
		}
	}
}

// responderIssuer starts the next server under oidc against a fresh issuer: members by the domain, dana
// the admin, the groups claim security-responders meaning responder.
func responderIssuer(t *testing.T) (*testIDP, *pgxpool.Pool, context.Context, string) {
	t.Helper()
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example",
		"CANYON_RESPONDER_CLAIM": "groups", "CANYON_RESPONDER_VALUE": "security-responders"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-portal')"); err != nil {
		t.Fatal(err)
	}
	return idp, pool, ctx, base
}

// signInAs walks the issuer's sign-in with bend over its ID token, and answers the browser holding the session.
func signInAs(t *testing.T, idp *testIDP, base string, bend map[string]any) *http.Client {
	t.Helper()
	idp.bend = bend
	b := browser(t)
	callback, _, _, _ := startSignIn(t, b, base, idp)
	if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
		t.Fatalf("%v signs in: %d %s", bend, resp.StatusCode, resp.Header.Get("Location"))
	}
	return b
}

// The hold's team record has no way off it: every owner and contributor address, bound or not, and every
// subject a session of one carried, so a person on the team when production was held never lifts it under
// a new address — signed in unverified (Entra), granted after their only sign-in, or another person at a
// bound grant's address.
func TestTheHoldsTeamRecordHasNoWayOff(t *testing.T) {
	grant := func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, grantee, subject string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', $1, $2, 'contributor', 'person', 'sub-dana', '')`, grantee, subject); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base string) {
		t.Helper()
		if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, base+holdPath, holdReason); code != 204 {
			t.Fatalf("the admin holds: %d %s", code, lastBody)
		}
	}
	responder := []any{"security-responders"}
	for name, c := range map[string]func(t *testing.T, idp *testIDP, pool *pgxpool.Pool, ctx context.Context, base string) *http.Client{
		"signed in unverified, then a new address": func(t *testing.T, idp *testIDP, pool *pgxpool.Pool, ctx context.Context, base string) *http.Client {
			grant(t, ctx, pool, "carl@meridian.example", "")
			signInAs(t, idp, base, map[string]any{"email": "carl@meridian.example", "sub": "sub-carl"})
			hold(t, ctx, pool, base)
			return signInAs(t, idp, base, map[string]any{"email": "carl.new@meridian.example", "sub": "sub-carl", "groups": responder})
		},
		"granted after their only sign-in, then a new address": func(t *testing.T, idp *testIDP, pool *pgxpool.Pool, ctx context.Context, base string) *http.Client {
			signInAs(t, idp, base, map[string]any{"email": "carl@meridian.example", "email_verified": true, "sub": "sub-carl"})
			grant(t, ctx, pool, "carl@meridian.example", "")
			hold(t, ctx, pool, base)
			return signInAs(t, idp, base, map[string]any{"email": "carl.new@meridian.example", "sub": "sub-carl", "groups": responder})
		},
		"another person at a bound grant's address": func(t *testing.T, idp *testIDP, pool *pgxpool.Pool, ctx context.Context, base string) *http.Client {
			grant(t, ctx, pool, "anna@meridian.example", "sub-anna")
			hold(t, ctx, pool, base)
			return signInAs(t, idp, base, map[string]any{"email": "anna@meridian.example", "sub": "sub-anna2", "groups": responder})
		},
		"bound after the hold, the grant gone, then a new address": func(t *testing.T, idp *testIDP, pool *pgxpool.Pool, ctx context.Context, base string) *http.Client {
			grant(t, ctx, pool, "carl@meridian.example", "")
			hold(t, ctx, pool, base)
			signInAs(t, idp, base, map[string]any{"email": "carl@meridian.example", "email_verified": true, "sub": "sub-carl"})
			if _, err := pool.Exec(ctx, "DELETE FROM project_access WHERE grantee = 'carl@meridian.example'"); err != nil {
				t.Fatal(err)
			}
			return signInAs(t, idp, base, map[string]any{"email": "carl.new@meridian.example", "email_verified": true, "sub": "sub-carl", "groups": responder})
		},
	} {
		idp, pool, ctx, base := responderIssuer(t) // a host each: pgtest names its schema after the test, and a subtest's name holds a slash
		lifter := c(t, idp, pool, ctx, base)
		for _, path := range []string{holdPath + "?start=true", holdPath} {
			if code, _, _ := do(t, lifter, http.MethodDelete, base+path, ""); code != 403 || lastBody != liftRefusal {
				t.Errorf("%s lifts %s: %d %s", name, path, code, lastBody)
			}
		}
	}
}

// A responder whose sign-in could not read their groups whole — an overage, a claim absent — never lifts a
// hold that records a group: they may be in it.
func TestALiftNeedsTheGroupsReadWholeWhenTheHoldRecordsAGroup(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'group', 'payments', 'contributor', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET groups_read = 'overage' WHERE actor_subject = 'sub-rhea'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != liftRefusal {
		t.Fatalf("a responder whose groups were not read lifts: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET groups_read = 'ok' WHERE actor_subject = 'sub-rhea'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, liftPath(t, ctx, pool, base, ""), ""); code != 204 {
		t.Fatalf("once read whole: %d %s", code, lastBody)
	}
}

// A sign-in naming its person ambiguously is refused: an address whose local part is not ASCII (no case
// folding apart from the provider's), a subject over 200 characters (never cut to another's).
func TestASignInWithAnAmbiguousNameIsRefused(t *testing.T) {
	idp, pool, ctx, base := responderIssuer(t)
	for name, bend := range map[string]map[string]any{
		"a Kelvin sign in the address": {"email": "Kate@meridian.example", "email_verified": true, "sub": "sub-kate"},
		"a subject of 201 characters":  {"email": "kate@meridian.example", "email_verified": true, "sub": strings.Repeat("s", 201)},
	} {
		idp.bend = bend
		b := browser(t)
		callback, _, _, _ := startSignIn(t, b, base, idp)
		if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/#/signin" {
			t.Errorf("%s: %d %s, want the sign-in refused", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("a refused sign-in left %d session(s)", n)
	}
}

// A claim at an address whose grant names another person is refused, never a silent promotion; a second
// person asking access from an address replaces the stale request's subject; a CLI session approved from
// an unverified browser session binds no grant it writes.
func TestGrantsAreNeverHandedAcrossPeople(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, subject_issuer, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', 'anna@meridian.example', 'sub-anna', '` + goodOIDC["CANYON_OIDC_ISSUER"] + `', 'viewer', 'person', 'sub-dana', '')`,
		"INSERT INTO projects (id) VALUES ('quote-engine'), ('ledger')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('ledger', 'email', 'xia@meridian.example', 'sub-xia', 'viewer', 'person', 'sub-dana', '')`, // bound by a binary that records no issuer, mid-upgrade
		"INSERT INTO access_requests (project_id, email, subject) VALUES ('claims-portal', 'zoe@meridian.example', 'sub-zoe')",
		`INSERT INTO project_access (project_id, kind, grantee, subject, subject_issuer, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('quote-engine', 'email', 'anna@meridian.example', 'sub-anna-old', 'https://login.contoso.example', 'viewer', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// A grant bound under another issuer is no claim's to promote either: not another person's at its address,
	// nor the holder of its old subject under this issuer, who may be another person too. Signing in binds it
	// again, and then its person claims (#2306).
	for _, sub := range []string{"sub-other", "sub-anna-old"} {
		if code, body, _ := do(t, signedIn(t, ctx, pool, base, sub, "anna@meridian.example", false), http.MethodPut, base+"/api/v1/projects/quote-engine/owner", `{"display":"Anna"}`); code != 409 || body["error"] != "grant_bound" ||
			count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE project_id = 'quote-engine' AND level = 'viewer' AND subject = 'sub-anna-old'") != 1 {
			t.Fatalf("%s claiming at a grant bound under another issuer: %d %s", sub, code, lastBody)
		}
	}
	// One a binary that records no issuer bound is Canyon's sign-in's, as it read it: its own person claims it,
	// and an unverified person at a pending request's address does not replace that request (#2306).
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-xia", "xia@meridian.example", false), http.MethodPut, base+"/api/v1/projects/ledger/owner", `{"display":"Xia"}`); code != 200 {
		t.Fatalf("its own person claiming a grant bound with no issuer recorded: %d %s", code, lastBody)
	}
	zoe := signedIn(t, ctx, pool, base, "sub-zoe2", "zoe@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email_verified = false WHERE actor_subject = 'sub-zoe2'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, zoe, http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 409 || body["error"] != "request_pending" {
		t.Fatalf("an unverified person's request over one with no issuer recorded: %d %s", code, lastBody)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email = 'anna@meridian.example' WHERE actor_subject = 'sub-dana'"); err != nil { // an admin at another person's bound address
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET admin_emails = '{anna@meridian.example}'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana"}`); code != 409 || body["error"] != "grant_bound" ||
		count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE grantee = 'anna@meridian.example' AND level = 'viewer' AND subject = 'sub-anna'") != 1 {
		t.Fatalf("a claim at another person's bound address: %d %s", code, lastBody)
	}
	for _, sub := range []string{"sub-ned", "sub-ned2"} {
		if code, _, _ := do(t, signedIn(t, ctx, pool, base, sub, "ned@meridian.example", false), http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 201 {
			t.Fatalf("%s asks: %d %s", sub, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'ned@meridian.example' AND subject = 'sub-ned2'"); n != 1 {
		t.Fatalf("a verified second person's request replaced the stale one: %d", n)
	}
	ned3 := signedIn(t, ctx, pool, base, "sub-ned3", "ned@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email_verified = false WHERE actor_subject = 'sub-ned3'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, ned3, http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 409 || body["error"] != "request_pending" ||
		count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'ned@meridian.example' AND subject = 'sub-ned2'") != 1 {
		t.Fatalf("an unverified person's request over a verified one's: %d %s", code, lastBody)
	}
	una := signedIn(t, ctx, pool, base, "sub-una", "una@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email_verified = false WHERE actor_subject = 'sub-una'"); err != nil {
		t.Fatal(err)
	}
	device, user, _ := mint(t, base)
	if code, _, _ := do(t, una, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("an unverified browser approves the CLI: %d %s", code, lastBody)
	}
	code, body := poll(t, base, device)
	if code != 200 {
		t.Fatalf("the poll: %d %s", code, lastBody)
	}
	cli := client(t)
	u, _ := neturl.Parse(base)
	cli.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: body["session"].(string)}}) //nolint:gosec // test: the session the poll answered
	if code, _, _ := do(t, cli, http.MethodPost, base+"/api/v1/projects", `{"id":"made-from-cli"}`); code != 201 ||
		count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE project_id = 'made-from-cli' AND grantee = 'una@meridian.example' AND subject = ''") != 1 {
		t.Fatalf("an app the unverified person's CLI creates: %d %s", code, lastBody)
	}
}

// groupIssuer is responderIssuer with the groups claim read as the person's groups: payments a contributor
// group of claims-portal.
func groupIssuer(t *testing.T) (*testIDP, *pgxpool.Pool, context.Context, string) {
	t.Helper()
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example",
		"CANYON_RESPONDER_CLAIM": "groups", "CANYON_RESPONDER_VALUE": "security-responders", "CANYON_GROUPS_CLAIM": "groups"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('claims-portal')",
		`INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'group', 'payments', 'contributor', 'person', 'sub-dana', '')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return idp, pool, ctx, base
}

// A group contributor who leaves the group never lifts the hold placed while they were in it: the record
// takes the subject of every session in one of the app's team groups when production is held, and of
// every sign-in in one after.
func TestAGroupContributorWhoLeavesNeverLifts(t *testing.T) {
	for _, signedInFirst := range []bool{true, false} {
		idp, pool, ctx, base := groupIssuer(t)
		hold := func() {
			if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, base+holdPath, holdReason); code != 204 {
				t.Fatalf("the admin holds: %d %s", code, lastBody)
			}
		}
		if !signedInFirst {
			hold()
		}
		signInAs(t, idp, base, map[string]any{"email": "gus@meridian.example", "email_verified": true, "sub": "sub-gus", "groups": []any{"payments"}})
		if signedInFirst {
			hold()
		}
		gus := signInAs(t, idp, base, map[string]any{"email": "gus@meridian.example", "email_verified": true, "sub": "sub-gus", "groups": []any{"security-responders"}}) // left payments
		if code, _, _ := do(t, gus, http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != liftRefusal {
			t.Errorf("signed in before the hold %v: the former group contributor lifts: %d %s", signedInFirst, code, lastBody)
		}
	}
}

// A session that could not read its groups whole never lifts while the app has a team group, one granted
// after the hold included.
func TestUnreadGroupsNeverLiftPastAGroupGrantedAfterTheHold(t *testing.T) {
	pool, ctx, base, _ := responderHost(t)
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodPost, base+holdPath, holdReason); code != 204 {
		t.Fatalf("the hold: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'group', 'payments', 'contributor', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET groups_read = 'overage' WHERE actor_subject = 'sub-rhea'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, rhea, http.MethodDelete, base+holdPath, ""); code != 403 || lastBody != liftRefusal {
		t.Fatalf("unread groups past a group granted after the hold: %d %s", code, lastBody)
	}
}

// A sign-in and a hold placement are serialised on the ledger lock: the sign-in waits on it as the hold
// does, and whichever lands first, the person signing in to a recorded address ends on the record.
func TestASignInAndAHoldNeverLeaveAWindow(t *testing.T) {
	idp, pool, ctx, base := responderIssuer(t)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'carl@meridian.example', 'contributor', 'person', 'sub-dana', '')`); err != nil {
		t.Fatal(err)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	idp.bend = map[string]any{"email": "carl@meridian.example", "sub": "sub-carl"}
	carl := browser(t)
	callback, _, _, _ := startSignIn(t, carl, base, idp)
	locked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(ctx) }() // let go on a failure too, or the server's shutdown waits on the hold blocked behind it
	if err := events.LockTx(ctx, locked); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		err  error
	}
	call := func(c *http.Client, method, url, body string) <-chan answer {
		out := make(chan answer, 1)
		go func() {
			req, _ := http.NewRequest(method, url, strings.NewReader(body))
			resp, err := c.Do(req)
			if err != nil {
				out <- answer{0, err}
				return
			}
			_ = resp.Body.Close()
			out <- answer{resp.StatusCode, nil}
		}()
		return out
	}
	signIn, holding := call(carl, http.MethodGet, callback, ""), call(dana, http.MethodPost, base+holdPath, holdReason)
	select {
	case a := <-signIn:
		t.Fatalf("the sign-in finished while the ledger lock was held: %+v", a)
	case a := <-holding:
		t.Fatalf("the hold finished while the ledger lock was held: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
	_ = locked.Rollback(ctx)
	if a := <-signIn; a.err != nil || a.code != 302 {
		t.Fatalf("the sign-in: %+v", a)
	}
	if a := <-holding; a.err != nil || a.code != 204 {
		t.Fatalf("the hold: %+v", a)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM prod_holds WHERE 'sub-carl' = ANY(team_subjects)"); n != 1 {
		t.Fatal("the person who signed in while the hold was placed is not on its record")
	}
}

// The hold lands on an org promotion waiting on its release: its remove goes out and completes, then the
// platform reports the original release deployed, late. Superseded, it still sends another remove while
// held — the promotion still waiting does not hold it back — so nothing the platform started stays up.
func TestALateOrgDeployWhileHeldIsRemovedAgain(t *testing.T) {
	pool, ctx, base, _, token := reportFixture(t)
	for _, q := range []string{
		"UPDATE runs SET state = 'clean', ended_at = now() WHERE id = 'gr_8f2a'",
		"UPDATE org_preferences SET run_by = 'org'",
		"INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_prom01', 'claims-portal', 'pkg_7d3e8f', 'running', 'promote', now(), 'person', 'priya@meridian.example', 'Priya Raman')",
		"INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run) VALUES ('rel_prod1', 'claims-portal', 'prod', 'gr_prom01', 'pkg_7d3e8f', 'deploy', '" + reportImage + "', gen_random_uuid(), 'accepted', now(), 'gr_8f2a')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	c := client(t)
	code, body, _ := do(t, c, http.MethodPost, base+holdPath, holdReason)
	first, _ := body["release"].(string)
	if code != 202 || first == "" {
		t.Fatalf("the hold under the org: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, client(t), http.MethodPost, base+"/releases/"+first+"/report", `{"status":"removed"}`, reportHeaders(token)); code != 200 {
		t.Fatalf("the hold's remove reported removed: %d %s", code, lastBody)
	}
	deployed := `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"}`
	if code, body, _ := doWith(t, client(t), http.MethodPost, base+"/releases/rel_prod1/report", deployed, reportHeaders(token)); code != 409 || body["error"] != "superseded" {
		t.Fatalf("the late deployed report: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases WHERE event = 'remove' AND env = 'prod' AND id <> '"+first+"' AND image = '"+reportImage+"'"); n != 1 {
		t.Fatalf("removes after the late deployed report: %d, want one: what the platform started stays up", n)
	}
}

// A sign-in whose address is over the limit is refused, never cut: cut, a 200-character address with a
// domain after it would be another person's, and bind their grants.
func TestALongAddressIsRefusedNeverCut(t *testing.T) {
	idp, pool, ctx, base := responderIssuer(t)
	victim := strings.Repeat("v", 183) + "@meridian.example"
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', $1, 'owner', 'person', 'sub-dana', '')`, victim); err != nil {
		t.Fatal(err)
	}
	idp.bend = map[string]any{"email": victim + ".evil.example", "email_verified": true, "sub": "sub-attacker"}
	b := browser(t)
	callback, _, _, _ := startSignIn(t, b, base, idp)
	if resp := fetch(t, b, callback); resp.StatusCode != 302 || resp.Header.Get("Location") != "/#/signin" {
		t.Fatalf("an address over the limit: %d %s, want the sign-in refused", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM project_access WHERE subject = ''") + count(t, ctx, pool, "SELECT count(*) FROM sessions"); n != 1 {
		t.Fatalf("the victim's grant was bound, or a session minted: %d", n)
	}
}
