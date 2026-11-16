package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"go/types"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/internal/executortest"
)

const (
	outboundApp       = "/api/v1/projects/claims-portal/egress"
	dashboardRequired = `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}`
	personRequired    = `{"error":"person_required","message":"a person changes how apps call out"}`
)

// outbound is a host with sign-in on where claims-portal runs preview and prod on an internal network, its floor
// enforced: olive owns it, carl contributes, vic reads it, nina has no access, dana is an admin; olive, carl and
// dana hold a CLI session too.
type outbound struct {
	pool                         *pgxpool.Pool
	ctx                          context.Context
	base                         string
	olive, carl, vic, nina, dana *http.Client
	oliveCLI, carlCLI, danaCLI   *http.Client
}

func outboundHost(t *testing.T) outbound {
	t.Helper()
	withOIDC(t) // dana is the admin
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	h := outbound{pool: pool, ctx: ctx, base: base}
	h.olive = signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, h.olive) // carl contributes, vic reads
	h.carl = signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	h.vic = signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	h.nina = signedIn(t, ctx, pool, base, "sub-nina", "nina@meridian.example", false)
	h.dana = signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	h.oliveCLI = cliIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example")
	h.carlCLI = cliIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example")
	h.danaCLI = cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	for _, q := range []string{
		`INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, egress) SELECT project_id, e, id, 'canyon/claims-portal@'||$1, 'http://claims-portal.'||e||'.localhost:7777', 'gr_seed', '{"via": "canyon"}'
			FROM packages, unnest(ARRAY['preview', 'prod']) e WHERE project_id = 'claims-portal'`,
		"INSERT INTO app_networks (project_id, internal) SELECT 'claims-portal', true WHERE $1::text IS NOT NULL",
		"UPDATE egress_settings SET credential_routes = $1::text IS NOT NULL", // on, as an admin turns it, for the routes' tests; TestCredentialRoutesGateRoutesAndTrust turns it off (#2143)
	} {
		if _, err := pool.Exec(ctx, q, executortest.Digest); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// exec runs each statement on the host's database.
func (h outbound) exec(t *testing.T, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := h.pool.Exec(h.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// asAgent makes the subject's browser session an agent's until the test sets it back.
func (h outbound) asAgent(t *testing.T, sub string, agent bool) {
	t.Helper()
	kind := "person"
	if agent {
		kind = "agent"
	}
	if _, err := h.pool.Exec(h.ctx, "UPDATE sessions SET actor_kind = $1 WHERE actor_subject = $2 AND flow = 'browser'", kind, sub); err != nil {
		t.Fatal(err)
	}
}

// envRead is one env of claims-portal's egress read, as a caller with the level reads it, its shape the contract's.
func (h outbound) envRead(t *testing.T, env string) map[string]any {
	t.Helper()
	code, out, _ := do(t, h.dana, http.MethodGet, h.base+outboundApp, "")
	if code != 200 {
		t.Fatalf("the app's egress read: %d %s", code, lastBody)
	}
	egressShape(t, "project_read", lastBody)
	return out["envs"].(map[string]any)[env].(map[string]any)
}

func destinationPath(env, destination string) string {
	return outboundApp + "/" + env + "/destinations/" + neturl.PathEscape(destination)
}

func strs(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// design/PERMISSIONS.md, Outbound: allowing a destination is a widening, a person's from a browser at every level
// that may widen — a contributor's, an owner's and an admin's CLI sessions are 403 dashboard_required and an
// agent 403 person_required, each one denied row, nothing allowed — and below contributor it is 403
// team_required, a read-only member's and a member with no access alike. From a browser a contributor, an owner
// and an admin allow, one egress.allow row each, none for a repeat.
func TestAllowFromCLISessionIsDashboardRequired(t *testing.T) {
	h := outboundHost(t)
	path := h.base + destinationPath("preview", "api.stripe.com")
	for name, c := range map[string]*http.Client{"a contributor's CLI": h.carlCLI, "an owner's CLI": h.oliveCLI, "an admin's CLI": h.danaCLI} {
		if code, _, _ := do(t, c, http.MethodPut, path, ""); code != 403 || lastBody != dashboardRequired {
			t.Errorf("%s: %d %s, want %s", name, code, lastBody, dashboardRequired)
		}
	}
	for name, c := range map[string]*http.Client{"a read-only member": h.vic, "a member with no access": h.nina} {
		if code, _, _ := do(t, c, http.MethodPut, path, ""); code != 403 || lastBody != teamRequiredTo(c == h.vic) {
			t.Errorf("%s: %d %s, want team_required", name, code, lastBody)
		}
	}
	h.asAgent(t, "sub-carl", true)
	if code, _, _ := do(t, h.carl, http.MethodPut, path, ""); code != 403 || lastBody != personRequired {
		t.Errorf("a contributor's agent: %d %s, want %s", code, lastBody, personRequired)
	}
	h.asAgent(t, "sub-carl", false)
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_allowed"); n != 0 {
		t.Fatalf("%d entries allowed by refused callers", n)
	}
	for code, want := range map[string]int{"dashboard_required": 3, "person_required": 1} {
		if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = '"+code+"'"); n != want {
			t.Errorf("%d denied rows of %s, want %d", n, code, want)
		}
	}
	for _, step := range []struct {
		who  *http.Client
		path string
	}{{h.carl, path}, {h.carl, path}, {h.olive, h.base + destinationPath("prod", "api.stripe.com")}, {h.dana, h.base + destinationPath("prod", "hooks.slack.com")}} {
		if code, _, _ := do(t, step.who, http.MethodPut, step.path, ""); code != 204 {
			t.Fatalf("PUT %s from a browser: %d %s", step.path, code, lastBody)
		}
	}
	if got := strs(h.envRead(t, "prod")["allowed"]); !slices.Equal(got, []string{"api.stripe.com", "hooks.slack.com"}) {
		t.Errorf("prod allows %v", got)
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.allow' AND project_id = 'claims-portal'`); n != 3 {
		t.Errorf("%d egress.allow rows, want one for each entry and none for the repeat", n)
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.allow' AND detail = '{"env": "preview", "destination": "api.stripe.com"}'`); n != 1 {
		t.Errorf("%d egress.allow rows naming preview's api.stripe.com, want 1", n)
	}
}

// design/PERMISSIONS.md, Outbound: below contributor there is no outbound — a read-only member and a member with
// no access are 403 team_required on the app's egress read and on every route that changes it, the owners named
// and nothing else, never a destination the app was refused, allowed or called, and nothing changes. Its
// contributor, owner and admin read the refused destination's request.
func TestReadOnlyLearnsNothing(t *testing.T) {
	h := outboundHost(t)
	h.exec(t,
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES ('claims-portal', 'prod', 'api.openai.com', 'pending', now() - interval '1 hour', now(), 14)`,
		`INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com')`,
		`INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
			VALUES ('claims-portal', 'prod', 'api.openai.com', 443, now() - interval '1 hour', now(), 14, 'refused', '4xx', 'connect', '{UAEX}')`)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, outboundApp, ""},
		{http.MethodPut, outboundApp + "/prod/mode", `{"mode":"allowlist"}`},
		{http.MethodPut, destinationPath("prod", "api.openai.com"), ""},
		{http.MethodDelete, destinationPath("prod", "api.stripe.com"), ""},
		{http.MethodPost, outboundApp + "/prod/requests/api.openai.com/dismiss", ""},
	}
	for name, c := range map[string]*http.Client{"a read-only member": h.vic, "a member with no access": h.nina} {
		for _, r := range routes {
			if code, _, _ := do(t, c, r.method, h.base+r.path, r.body); code != 403 || lastBody != teamRequiredTo(c == h.vic) {
				t.Errorf("%s's %s %s: %d %s, want team_required naming the owners alone", name, r.method, r.path, code, lastBody)
			}
		}
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM egress_requests WHERE state = 'pending'`) + count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_allowed") +
		count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_modes"); n != 2 {
		t.Fatalf("a refused caller changed the app's outbound: %d rows, want the request and the entry alone", n)
	}
	for name, c := range map[string]*http.Client{"its contributor": h.carl, "its owner": h.olive, "an admin": h.dana} {
		code, out, _ := do(t, c, http.MethodGet, h.base+outboundApp, "")
		if code != 200 {
			t.Fatalf("%s's read: %d %s", name, code, lastBody)
		}
		egressShape(t, "project_read", lastBody)
		if reqs := out["envs"].(map[string]any)["prod"].(map[string]any)["requests"].([]any); len(reqs) != 1 || reqs[0].(map[string]any)["destination"] != "api.openai.com" {
			t.Errorf("%s reads prod's requests %v, want api.openai.com's", name, reqs)
		}
	}
}

// design/EGRESS.md §3.3, §3.4: a destination is allowed as the canonicalisation leaves an entry a person types —
// Bücher.DE. is xn--bcher-kva.de — and what it refuses is 400 invalid_destination; an entry closes each request
// of the env it allows, pending or dismissed, *.stripe.com any name below stripe.com and never stripe.com itself.
func TestAllowCanonicalisesAndClosesRequests(t *testing.T) {
	h := outboundHost(t)
	for _, d := range []string{"10.0.0.1", "0x7f000001", "api.0x7f", "[::1]", "localhost", "api.stripe.com:443", "user@api.stripe.com", "api.*.stripe.com", "*", "*.com", strings.Repeat("a", 64) + ".example.com"} {
		if code, out, _ := do(t, h.dana, http.MethodPut, h.base+destinationPath("prod", d), ""); code != 400 || out["error"] != "invalid_destination" {
			t.Errorf("%q: %d %s, want 400 invalid_destination", d, code, lastBody)
		}
	}
	h.exec(t, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.stripe.com', 'pending', now(), now(), 3),
		('claims-portal', 'prod', 'a.b.stripe.com', 'dismissed', now(), now(), 1),
		('claims-portal', 'prod', 'stripe.com', 'pending', now(), now(), 2),
		('claims-portal', 'preview', 'api.stripe.com', 'pending', now(), now(), 1)`)
	for _, d := range []string{"Bücher.DE.", "*.stripe.com"} {
		if code, _, _ := do(t, h.dana, http.MethodPut, h.base+destinationPath("prod", d), ""); code != 204 {
			t.Fatalf("%q: %d %s", d, code, lastBody)
		}
	}
	prod := h.envRead(t, "prod")
	if got := strs(prod["allowed"]); !slices.Equal(got, []string{"*.stripe.com", "xn--bcher-kva.de"}) {
		t.Errorf("prod allows %v, want the canonical entries, sorted", got)
	}
	var left []string
	for _, r := range prod["requests"].([]any) {
		left = append(left, r.(map[string]any)["destination"].(string))
	}
	if !slices.Equal(left, []string{"stripe.com"}) {
		t.Errorf("prod's requests %v, want stripe.com's alone: *.stripe.com allows every name below it and never stripe.com", left)
	}
	if n := len(h.envRead(t, "preview")["requests"].([]any)); n != 1 {
		t.Errorf("preview holds %d requests, want its own, which prod's entry does not close", n)
	}
	storeDoor(t, h.ctx, h.pool, "https://login.meridian.example")
	if code, out, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, ""); code != 200 || len(strs(out["platform"])) != 0 {
		t.Errorf("platform on Docker, which has no front door: %d %v, want none", code, out["platform"])
	}
}

// storeDoorJSON replaces the host's front door with door, choice 1's: no secret stored.
func storeDoorJSON(t *testing.T, h outbound, door string) {
	t.Helper()
	h.exec(t, "DELETE FROM front_doors")
	if _, err := h.pool.Exec(h.ctx, "INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana', 'Dana')", []byte(door)); err != nil {
		t.Fatal(err)
	}
}

// design/EGRESS.md §3.3: the platform destinations are the front door's sign-in hosts, computed by the server,
// on no team's allowlist: allowing one is 409 platform_destination, and the read lists none on an allowlist,
// one allowed before the door moved to it included. A host the canonicalisation refuses — an IP literal, a
// single label — is no platform destination, which the check would refuse before reading them; an alb's is its
// region's key endpoint. A front door is Kubernetes', and so are its platform destinations.
func TestAPlatformDestinationIsOnNoAllowlist(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t)
	h.exec(t, `INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'login.meridian.example')`)
	storeDoor(t, h.ctx, h.pool, "https://Login.Meridian.Example/realms/apps")
	for _, d := range []string{"login.meridian.example", "LOGIN.meridian.example."} {
		if code, _, _ := do(t, h.dana, http.MethodPut, h.base+destinationPath("preview", d), ""); code != 409 ||
			lastBody != `{"error":"platform_destination","message":"Canyon allows login.meridian.example for every app already"}` {
			t.Errorf("%q: %d %s, want 409 platform_destination", d, code, lastBody)
		}
	}
	code, out, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, "")
	if code != 200 {
		t.Fatalf("%d %s", code, lastBody)
	}
	egressShape(t, "project_read", lastBody)
	if got := strs(out["platform"]); !slices.Equal(got, []string{"login.meridian.example"}) {
		t.Errorf("platform %v, want the door's sign-in host", got)
	}
	if got := strs(out["envs"].(map[string]any)["prod"].(map[string]any)["allowed"]); len(got) != 0 {
		t.Errorf("prod's allowlist reads %v, want none: a platform destination is on no team's list", got)
	}
	for _, tc := range []struct{ door, want string }{
		{`{"choice": 1, "gateway": "oidc", "issuer": "https://10.0.0.7/realms/apps", "audience": "apps", "jwks_url": "https://keycloak:8443/keys", "cookie": "c", "domain": "canyon.meridian.example", "form": "app.env.domain"}`, ""},
		{`{"choice": 1, "gateway": "oidc", "issuer": "https://login.meridian.example:8443", "audience": "apps", "jwks_url": "https://keys.meridian.example/jwks", "cookie": "c", "domain": "canyon.meridian.example", "form": "app.env.domain"}`, "keys.meridian.example login.meridian.example"},
		{`{"choice": 1, "gateway": "alb", "alb_arn": "arn:aws:elasticloadbalancing:eu-west-1:123456789012:loadbalancer/app/apps/0123456789abcdef", "domain": "canyon.meridian.example", "form": "app.env.domain"}`, "public-keys.auth.elb.eu-west-1.amazonaws.com"},
	} {
		storeDoorJSON(t, h, tc.door)
		if code, out, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, ""); code != 200 || strings.Join(strs(out["platform"]), " ") != tc.want {
			t.Errorf("%s: %d platform %v, want %q", tc.door, code, out["platform"], tc.want)
		}
		egressShape(t, "project_read", lastBody)
	}
}

// design/PERMISSIONS.md, Outbound: narrowing works from any session and any actor, a held production's included —
// a contributor's agent and CLI, an owner's and an admin's CLI remove entries — and the entry stored as
// :destination is written goes, else the one it canonicalises to; 400 invalid_destination only when neither
// names one. While production is held, allowing on prod is 409 held, after the level and the canonicalisation,
// and preview is unaffected; an owner's or an admin's agent allowing is 403 person_required. An admin on a
// deleted app is 404 on every outbound route.
func TestNarrowingWorksFromAnySessionAndAHeldProduction(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, `INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com'), ('claims-portal', 'prod', 'api.123'), ('claims-portal', 'prod', 'xn--bcher-kva.de'),
		('claims-portal', 'prod', 'a.example.com'), ('claims-portal', 'prod', 'b.example.com')`,
		`INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2231', 'person', 'sub-rhea', 'Rhea Okafor')`)
	h.asAgent(t, "sub-carl", true)
	if code, _, _ := do(t, h.carl, http.MethodDelete, h.base+destinationPath("prod", "api.stripe.com"), ""); code != 204 {
		t.Fatalf("a contributor's agent narrowing a held production: %d %s", code, lastBody)
	}
	h.asAgent(t, "sub-carl", false)
	for _, d := range []string{"api.123", "Bücher.de", "never.example.com"} { // api.123: stored before its last label read as an IPv4 address
		if code, _, _ := do(t, h.carlCLI, http.MethodDelete, h.base+destinationPath("prod", d), ""); code != 204 {
			t.Errorf("a contributor's CLI removing %q: %d %s", d, code, lastBody)
		}
	}
	for d, c := range map[string]*http.Client{"a.example.com": h.oliveCLI, "b.example.com": h.danaCLI} {
		if code, _, _ := do(t, c, http.MethodDelete, h.base+destinationPath("prod", d), ""); code != 204 {
			t.Errorf("removing %q from an owner's or an admin's CLI: %d %s", d, code, lastBody)
		}
	}
	if code, out, _ := do(t, h.carlCLI, http.MethodDelete, h.base+destinationPath("prod", "10.0.0.1"), ""); code != 400 || out["error"] != "invalid_destination" {
		t.Errorf("removing what names no entry and canonicalises to nothing: %d %s", code, lastBody)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_allowed"); n != 0 {
		t.Errorf("%d entries left", n)
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.disallow' AND detail->>'env' = 'prod' AND detail->>'destination' IN ('api.stripe.com', 'api.123', 'xn--bcher-kva.de', 'a.example.com', 'b.example.com')`); n != 5 {
		t.Errorf("%d egress.disallow rows, want one per entry removed, as stored, and none for one that was not there", n)
	}
	if code, out, _ := do(t, h.carl, http.MethodPut, h.base+destinationPath("prod", "hooks.slack.com"), ""); code != 409 || out["error"] != "held" {
		t.Errorf("allowing on a held production: %d %s, want 409 held", code, lastBody)
	}
	if code, out, _ := do(t, h.carl, http.MethodPut, h.base+destinationPath("prod", "10.0.0.1"), ""); code != 400 || out["error"] != "invalid_destination" {
		t.Errorf("an IP literal on a held production: %d %s, want 400 before the hold", code, lastBody)
	}
	if code, _, _ := do(t, h.vic, http.MethodPut, h.base+destinationPath("prod", "hooks.slack.com"), ""); code != 403 || strings.Contains(lastBody, "held") {
		t.Errorf("a read-only member: %d %s, want team_required, never learning of the hold", code, lastBody)
	}
	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+destinationPath("preview", "hooks.slack.com"), ""); code != 204 {
		t.Errorf("allowing on preview while production is held: %d %s", code, lastBody)
	}
	for _, sub := range []string{"sub-olive", "sub-dana"} {
		h.asAgent(t, sub, true)
	}
	for name, c := range map[string]*http.Client{"an owner's agent": h.olive, "an admin's agent": h.dana} {
		if code, _, _ := do(t, c, http.MethodPut, h.base+destinationPath("preview", "api.openai.com"), ""); code != 403 || lastBody != personRequired {
			t.Errorf("%s allowing: %d %s", name, code, lastBody)
		}
	}
	for _, sub := range []string{"sub-olive", "sub-dana"} {
		h.asAgent(t, sub, false)
	}
	for _, path := range []string{"/api/v1/projects/claims-portal/egress/staging/destinations/api.stripe.com", "/api/v1/projects/case-files/egress/prod/destinations/api.stripe.com"} {
		if code, _, _ := do(t, h.nina, http.MethodPut, h.base+path, ""); code != 404 {
			t.Errorf("%s: %d %s, want 404 before the level", path, code, lastBody)
		}
	}
	h.exec(t, "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'")
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, outboundApp, ""},
		{http.MethodPut, outboundApp + "/preview/mode", `{"mode":"allowlist"}`},
		{http.MethodPut, destinationPath("preview", "api.openai.com"), ""},
		{http.MethodDelete, destinationPath("preview", "hooks.slack.com"), ""},
		{http.MethodPost, outboundApp + "/preview/requests/api.openai.com/dismiss", ""},
	} {
		if code, out, _ := do(t, h.dana, r.method, h.base+r.path, r.body); code != 404 || out["error"] != "not_found" {
			t.Errorf("an admin's %s %s on a deleted app: %d %s, want 404", r.method, r.path, code, lastBody)
		}
	}
}

// design/PERMISSIONS.md, Outbound, Mode: an owner sets their app env's mode stricter than the org's — allowlist,
// a narrowing, from any session — or back to the org's, null, a widening, a person's from a browser; an owner's
// observe is 409 looser_than_org; a contributor is 403 owner_required whatever the mode, and a read-only member
// 403 team_required; an admin sets any of the three, observe and null as widenings. The body is parsed after
// the level and before the actor, a body it cannot read 400 invalid_mode with no row. A mode is refused while
// the app's floor is not enforced, then while the org runs its apps; a widening of a held production is 409
// held, a narrowing goes on. One egress.mode.set row when the env's own mode moves.
func TestAnAppEnvsModeIsAnOwnersStricterAndAnAdminsEitherWay(t *testing.T) {
	h := outboundHost(t)
	mode := func(c *http.Client, env, body string) int {
		code, _, _ := do(t, c, http.MethodPut, h.base+outboundApp+"/"+env+"/mode", body)
		return code
	}
	if code := mode(h.vic, "prod", `{"mode":"allowlist"}`); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Errorf("a read-only member: %d %s", code, lastBody)
	}
	if code := mode(h.carl, "prod", `{"mode":"allowlist"}`); code != 403 || !strings.Contains(lastBody, `"error":"owner_required"`) {
		t.Errorf("a contributor's stricter mode: %d %s, want owner_required", code, lastBody)
	}
	trail := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events")
	for _, body := range []string{`{"mode":"off"}`, `{}`, `{"mode":"allowlist","env":"prod"}`, `{"mode":1}`, `not json`, `{"mode":"allowlist"} {}`, `{"mode":"allowlist"}}`} {
		if code, out, _ := do(t, h.oliveCLI, http.MethodPut, h.base+outboundApp+"/prod/mode", body); code != 400 || out["error"] != "invalid_mode" {
			t.Errorf("%s: %d %s, want 400 invalid_mode", body, code, lastBody)
		}
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events"); n != trail {
		t.Errorf("a body the route cannot read wrote %d audit rows", n-trail)
	}
	if code := mode(h.oliveCLI, "prod", `{"mode":"allowlist"}`); code != 204 {
		t.Fatalf("an owner's stricter mode from the CLI, a narrowing: %d %s", code, lastBody)
	}
	if prod := h.envRead(t, "prod"); prod["mode"] != "allowlist" || prod["app_mode"] != "allowlist" {
		t.Errorf("prod reads mode %v, app_mode %v", prod["mode"], prod["app_mode"])
	}
	if preview := h.envRead(t, "preview"); preview["mode"] != "observe" || preview["app_mode"] != nil {
		t.Errorf("preview reads mode %v, app_mode %v, want the org's", preview["mode"], preview["app_mode"])
	}
	if code := mode(h.oliveCLI, "prod", `{"mode":null}`); code != 403 || lastBody != dashboardRequired {
		t.Errorf("an owner's widening from the CLI: %d %s", code, lastBody)
	}
	h.asAgent(t, "sub-olive", true)
	if code := mode(h.olive, "prod", `{"mode":null}`); code != 403 || lastBody != personRequired {
		t.Errorf("an owner's agent widening: %d %s", code, lastBody)
	}
	if code := mode(h.olive, "prod", `{"mode":"allowlist"}`); code != 204 {
		t.Errorf("an owner's agent narrowing: %d %s", code, lastBody)
	}
	h.asAgent(t, "sub-olive", false)
	if code := mode(h.olive, "prod", `{"mode":"observe"}`); code != 409 || lastBody != `{"error":"looser_than_org","message":"only an admin can set an app's mode looser than the org's"}` {
		t.Errorf("an owner's observe: %d %s", code, lastBody)
	}
	if code := mode(h.danaCLI, "preview", `{"mode":"observe"}`); code != 403 || lastBody != dashboardRequired {
		t.Errorf("an admin's looser mode from the CLI: %d %s", code, lastBody)
	}
	for range 2 {
		if code := mode(h.dana, "preview", `{"mode":"observe"}`); code != 204 {
			t.Errorf("an admin's looser mode from a browser: %d %s", code, lastBody)
		}
	}
	h.exec(t, `INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2231', 'person', 'sub-rhea', 'Rhea Okafor')`)
	if code, out, _ := do(t, h.olive, http.MethodPut, h.base+outboundApp+"/prod/mode", `{"mode":null}`); code != 409 || out["error"] != "held" {
		t.Errorf("widening a held production: %d %s, want 409 held", code, lastBody)
	}
	if code := mode(h.olive, "preview", `{"mode":null}`); code != 204 {
		t.Errorf("widening preview while production is held: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM prod_holds")
	if code := mode(h.olive, "prod", `{"mode":null}`); code != 204 {
		t.Errorf("an owner back to the org's from a browser: %d %s", code, lastBody)
	}
	if code := mode(h.dana, "prod", `{"mode":"allowlist"}`); code != 204 {
		t.Errorf("an admin's stricter mode: %d %s", code, lastBody)
	}
	if code := mode(h.danaCLI, "prod", `{"mode":null}`); code != 403 || lastBody != dashboardRequired {
		t.Errorf("an admin back to the org's from the CLI: %d %s", code, lastBody)
	}
	if code := mode(h.dana, "prod", `{"mode":null}`); code != 204 {
		t.Errorf("an admin back to the org's from a browser: %d %s", code, lastBody)
	}
	for want, n := range map[string]int{
		`{"env": "prod", "app_mode": {"after": "allowlist", "before": null}}`:  2,
		`{"env": "preview", "app_mode": {"after": "observe", "before": null}}`: 1,
		`{"env": "preview", "app_mode": {"after": null, "before": "observe"}}`: 1,
		`{"env": "prod", "app_mode": {"after": null, "before": "allowlist"}}`:  2,
	} {
		if got := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.mode.set' AND project_id = 'claims-portal' AND detail = '"+want+"'"); got != n {
			t.Errorf("%d egress.mode.set rows %s, want %d", got, want, n)
		}
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.mode.set'"); n != 6 {
		t.Errorf("%d egress.mode.set rows, want one per move and none for a mode set again", n)
	}
	// one denied row a 403: the read-only member's and the contributor's level, the owner's agent, and the
	// owner's and the admin's CLI sessions widening
	for code, want := range map[string]int{"team_required": 1, "owner_required": 1, "person_required": 1, "dashboard_required": 3} {
		if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = '"+code+"'"); n != want {
			t.Errorf("%d denied rows of %s, want %d", n, code, want)
		}
	}
	h.exec(t, "UPDATE app_networks SET internal = false", "UPDATE org_preferences SET run_by = 'org'")
	if code := mode(h.olive, "prod", `{"mode":"allowlist"}`); code != 409 || lastBody != `{"error":"floor_not_enforced","message":"this app's calls do not all leave through Canyon's egress gateway yet"}` {
		t.Errorf("a mode while the floor is not enforced: %d %s", code, lastBody)
	}
	h.exec(t, "UPDATE app_networks SET internal = true")
	if code := mode(h.olive, "prod", `{"mode":"allowlist"}`); code != 409 || lastBody != `{"error":"egress_external","message":"this Canyon hands its apps to your own platform, so their calls don't go through Canyon"}` {
		t.Errorf("a mode while the org runs its apps: %d %s", code, lastBody)
	}
}

// design/PERMISSIONS.md, Outbound: dismissing an egress request is a person's from any session — a contributor's,
// an owner's and an admin's agent is 403 person_required, one denied row each; their CLI sessions dismiss — of a
// pending request of the destination as an entry canonicalises it, else 404; the read lists the requests
// pending first, then dismissed, each the latest refused first. One egress.request.dismiss row a dismissal.
func TestDismissIsAPersonsFromAnySession(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.openai.com', 'pending', now() - interval '3 hours', now() - interval '1 hour', 14),
		('claims-portal', 'prod', 'api.anthropic.com', 'pending', now() - interval '3 hours', now() - interval '2 hours', 2),
		('claims-portal', 'prod', 'hooks.zapier.com', 'dismissed', now() - interval '3 hours', now() - interval '30 minutes', 1),
		('claims-portal', 'prod', 'a.example.com', 'pending', now() - interval '8 hours', now() - interval '6 hours', 1),
		('claims-portal', 'prod', 'b.example.com', 'pending', now() - interval '8 hours', now() - interval '7 hours', 1)`)
	dismiss := func(c *http.Client, d string) int {
		code, _, _ := do(t, c, http.MethodPost, h.base+outboundApp+"/prod/requests/"+neturl.PathEscape(d)+"/dismiss", "")
		return code
	}
	if code := dismiss(h.vic, "api.openai.com"); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Errorf("a read-only member: %d %s", code, lastBody)
	}
	agents := map[string]*http.Client{"sub-carl": h.carl, "sub-olive": h.olive, "sub-dana": h.dana}
	for sub, c := range agents {
		h.asAgent(t, sub, true)
		if code := dismiss(c, "api.openai.com"); code != 403 || lastBody != personRequired {
			t.Errorf("%s's agent: %d %s", sub, code, lastBody)
		}
		h.asAgent(t, sub, false)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND detail->>'error' = 'person_required'"); n != 3 {
		t.Errorf("%d denied rows for the agents, want 3", n)
	}
	for _, step := range []struct {
		who *http.Client
		d   string
	}{{h.carlCLI, "api.openai.com"}, {h.carlCLI, "API.Anthropic.COM."}, {h.oliveCLI, "a.example.com"}, {h.danaCLI, "b.example.com"}} {
		if code := dismiss(step.who, step.d); code != 204 {
			t.Errorf("dismissing %q from the CLI: %d %s", step.d, code, lastBody)
		}
	}
	for _, d := range []string{"api.openai.com", "hooks.zapier.com", "never.example.com", "10.0.0.1"} {
		if code, out, _ := do(t, h.carl, http.MethodPost, h.base+outboundApp+"/prod/requests/"+d+"/dismiss", ""); code != 404 || out["error"] != "not_found" {
			t.Errorf("%s, no pending request: %d %s", d, code, lastBody)
		}
	}
	h.exec(t, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.mistral.ai', 'pending', now() - interval '5 hours', now() - interval '4 hours', 1)`)
	var got []string
	for _, r := range h.envRead(t, "prod")["requests"].([]any) {
		r := r.(map[string]any)
		got = append(got, r["destination"].(string)+":"+r["state"].(string))
	}
	if want := []string{"api.mistral.ai:pending", "hooks.zapier.com:dismissed", "api.openai.com:dismissed", "api.anthropic.com:dismissed", "a.example.com:dismissed", "b.example.com:dismissed"}; !slices.Equal(got, want) {
		t.Errorf("prod's requests %v, want %v", got, want)
	}
	if n := count(t, h.ctx, h.pool, `SELECT count(*) FROM audit_events WHERE action = 'egress.request.dismiss' AND detail IN ('{"env": "prod", "destination": "api.openai.com"}', '{"env": "prod", "destination": "api.anthropic.com"}',
		'{"env": "prod", "destination": "a.example.com"}', '{"env": "prod", "destination": "b.example.com"}')`); n != 4 {
		t.Errorf("%d egress.request.dismiss rows, want one per dismissal", n)
	}
}

// contracts/routes.md, Egress control: the org's default mode is every member's to read and an admin's to set from
// a browser, beside via or alone, one JSON object and nothing after it; it is refused while via — the body's
// when it names one — is off, then while the org runs its apps; a widening is 409 held while a production it
// widens is held — the mode's, of an app following the org's; via off's, of any app. One egress.set row names
// what moved. An app env with no mode of its own follows it.
func TestTheOrgModeIsAnAdminsAndNeedsTheFloor(t *testing.T) {
	h := outboundHost(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	put := func(body string) int {
		code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", body)
		return code
	}
	const floorOff = `{"error":"floor_not_enforced","message":"apps' calls do not leave through Canyon's egress gateway while it is off"}`
	if code := put(`{"mode":"allowlist"}`); code != 409 || lastBody != floorOff {
		t.Errorf("a mode under via off: %d %s", code, lastBody)
	}
	for body, want := range map[string]string{`{"mode":"off"}`: "invalid_mode", `{"mode":null}`: "invalid_mode", `{"via":"canyon","mode":"open"}`: "invalid_mode", `{"mode":"allowlist","x":1}`: "invalid_via",
		`{"mode":"allowlist"}}`: "invalid_via", `{"via":"off"} {}`: "invalid_via"} {
		if code, out, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", body); code != 400 || out["error"] != want {
			t.Errorf("%s: %d %s, want %s", body, code, lastBody, want)
		}
	}
	if code := put(`{"via":"canyon","mode":"allowlist"}`); code != 200 || !strings.Contains(lastBody, `"mode":"allowlist"`) || !strings.Contains(lastBody, `"via":"canyon"`) {
		t.Fatalf("via and mode together: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	if code := put(`{"via":"off","mode":"observe"}`); code != 409 || lastBody != floorOff {
		t.Errorf("a mode beside via off: %d %s", code, lastBody)
	}
	h.exec(t, `INSERT INTO prod_holds (project_id, reason, held_by_kind, held_by_subject, held_by_display) VALUES ('claims-portal', 'INC-2231', 'person', 'sub-rhea', 'Rhea Okafor')`)
	if code, out, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"mode":"observe"}`); code != 409 || out["error"] != "held" ||
		!strings.HasPrefix(out["message"].(string), "claims-portal: Production was stopped by Rhea Okafor") || out["hold"] == nil {
		t.Errorf("observe while a production following the org's is held: %d %s, want 409 held naming it", code, lastBody)
	}
	h.exec(t, `INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')`)
	if code := put(`{"via":"off"}`); code != 409 || !strings.Contains(lastBody, `"error":"held"`) {
		t.Errorf("via off while a production is held, its own mode or not: %d %s, want 409 held", code, lastBody)
	}
	if code := put(`{"mode":"observe"}`); code != 200 {
		t.Errorf("a mode under via canyon, the held production keeping its own: %d %s", code, lastBody)
	}
	h.exec(t, "DELETE FROM prod_holds")
	for _, want := range []string{`{"via": {"after": "canyon", "before": "off"}, "mode": {"after": "allowlist", "before": "observe"}}`, `{"mode": {"after": "observe", "before": "allowlist"}}`} {
		if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.set' AND detail = '"+want+"'"); n != 1 {
			t.Errorf("%d egress.set rows %s, want 1", n, want)
		}
	}
	h.exec(t, "UPDATE org_preferences SET run_by = 'org'")
	if code := put(`{"via":"off","mode":"allowlist"}`); code != 409 || lastBody != floorOff {
		t.Errorf("a mode beside via off while the org runs its apps: %d %s, want floor_not_enforced first (design/PERMISSIONS.md, Outbound, Order)", code, lastBody)
	}
	if code := put(`{"mode":"allowlist"}`); code != 409 || lastBody != `{"error":"egress_external","message":"the org runs its apps, so their calls do not leave through Canyon"}` {
		t.Errorf("a mode while the org runs its apps: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.vic, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 || lastBody != `{"credential_routes":true,"mode":"observe","via":"canyon"}` {
		t.Errorf("a member's read: %d %s", code, lastBody)
	}
	egressShape(t, "setting_member", lastBody)
	h.exec(t, "UPDATE egress_settings SET mode = 'allowlist'")
	if preview := h.envRead(t, "preview"); preview["mode"] != "allowlist" || preview["app_mode"] != nil {
		t.Errorf("preview reads mode %v, app_mode %v, want the org's allowlist", preview["mode"], preview["app_mode"])
	}
}

// design/PERMISSIONS.md, Outbound: a responder on an app not shared with them reads nothing of its outbound and
// changes none of it, 403 team_required; one who holds a grant on the app acts on its outbound with that grant
// — a contributor's: reads, allows from a browser, and is 403 owner_required on its mode.
func TestAResponderActsOnOutboundByTheirGrantAlone(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, "UPDATE org_preferences SET responder_emails = ARRAY['"+responderEmail+"']")
	rhea := signedIn(t, h.ctx, h.pool, h.base, "sub-rhea", responderEmail, false)
	routes := []struct {
		method, path, body string
		granted            int
	}{
		{http.MethodGet, outboundApp, "", 200},
		{http.MethodPut, destinationPath("prod", "api.openai.com"), "", 204},
		{http.MethodPut, outboundApp + "/prod/mode", `{"mode":"allowlist"}`, 403},
	}
	for _, r := range routes {
		if code, _, _ := do(t, rhea, r.method, h.base+r.path, r.body); code != 403 || lastBody != teamRequiredBody {
			t.Errorf("a responder with no grant, %s %s: %d %s, want team_required", r.method, r.path, code, lastBody)
		}
	}
	h.exec(t, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', '`+responderEmail+`', 'contributor', 'person', 'sub-olive', '')`)
	for _, r := range routes {
		if code, out, _ := do(t, rhea, r.method, h.base+r.path, r.body); code != r.granted || code == 403 && out["error"] != "owner_required" {
			t.Errorf("a responder holding a contributor's grant, %s %s: %d %s, want %d", r.method, r.path, code, lastBody, r.granted)
		}
	}
}

// googleDiscovery is Google's discovery document, the part Canyon reads: its token, user-info and key set
// endpoints are each on another host than its issuer.
const googleDiscovery = `{"issuer":"https://accounts.google.com","authorization_endpoint":"https://accounts.google.com/o/oauth2/v2/auth","token_endpoint":"https://oauth2.googleapis.com/token","userinfo_endpoint":"https://openidconnect.googleapis.com/v1/userinfo","jwks_uri":"https://www.googleapis.com/oauth2/v3/certs"}`

const googlePlatform = "accounts.google.com oauth2.googleapis.com openidconnect.googleapis.com www.googleapis.com"

// platformRead is the platform destinations the app's egress read carries, space-separated.
func platformRead(t *testing.T, h outbound) string {
	t.Helper()
	code, out, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, "")
	if code != 200 {
		t.Fatalf("the app's egress read: %d %s", code, lastBody)
	}
	return strings.Join(strs(out["platform"]), " ")
}

// design/EGRESS.md §3.3, contracts/routes.md, the front-door row: an admin's save of a choice 2 front door records
// the token, user-info and key set endpoints its issuer's discovery names as platform destinations — the policy's
// and the read's, on no allowlist — each as the canonicalisation keeps it, an IP literal none; a member's save is
// refused before anything is fetched. A discovery that cannot be read — none, another issuer's, a user-info
// endpoint that is not https — never refuses the save: it saves, records nothing and answers discovery_note, on
// every read after too. An issuer whose host is no destination is never fetched and answers no note. A save that
// changes nothing fetches again; a choice 1 records nothing.
func TestPlatformDestinationsCarryTheDiscoveryEndpoints(t *testing.T) {
	onKubernetes(t)
	docs := idpAt{
		"https://accounts.google.com":    googleDiscovery,
		"https://login.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://10.0.0.9/token","jwks_uri":"https://keys.meridian.example/jwks"}`,
		"https://plain.meridian.example": `{"issuer":"https://plain.meridian.example","authorization_endpoint":"https://plain.meridian.example/auth","token_endpoint":"https://plain.meridian.example/token","userinfo_endpoint":"http://plain.meridian.example/me","jwks_uri":"https://plain.meridian.example/keys"}`,
		"https://other.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://login.meridian.example/token","jwks_uri":"https://login.meridian.example/keys"}`,
	}
	var named atomic.Bool // the login door's discovery names a user-info endpoint
	var asked atomic.Int32
	withDiscovery(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		asked.Add(1)
		if named.Load() {
			return idpAt{"https://login.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://10.0.0.9/token","userinfo_endpoint":"https://me.meridian.example/userinfo","jwks_uri":"https://keys.meridian.example/jwks"}`}.RoundTrip(r)
		}
		return docs.RoundTrip(r)
	}))
	h := outboundHost(t)
	door := func(issuer, secret string) string {
		return `{"choice":2,"signin":{"issuer":"` + issuer + `","client_id":"canyon-apps"` + secret + `},"form":"app.env.domain"}`
	}
	const secret = `,"client_secret":"cs-discovery-test"`
	save := func(body, what string) map[string]any {
		t.Helper()
		code, out, _ := do(t, h.dana, http.MethodPut, h.base+doorPath, body)
		if code != 200 {
			t.Fatalf("%s: %d %s", what, code, lastBody)
		}
		h.exec(t, "UPDATE replaced_front_doors SET replaced_at = now() - interval '31 minutes'") // each door this test replaces, past its 30 minutes in use
		return out
	}
	if code, _, _ := do(t, h.carl, http.MethodPut, h.base+doorPath, door("https://accounts.google.com", secret)); code != 403 || lastBody != adminRequired || asked.Load() != 0 {
		t.Fatalf("a member's save: %d %s, %d fetch(es)", code, lastBody, asked.Load())
	}
	if out := save(door("https://accounts.google.com", secret), "Google's front door"); out["discovery_note"] != "" {
		t.Errorf("Google's door, its discovery read: discovery_note %q", out["discovery_note"])
	}
	if got := platformRead(t, h); got != googlePlatform {
		t.Errorf("platform %q, want the issuer and the token, user-info and key set endpoints its discovery names", got)
	}
	if got, err := appOf(t, h.base).platformDestinations(h.ctx, h.pool); err != nil || !slices.Contains(got, "oauth2.googleapis.com") {
		t.Errorf("the policy's platform destinations %v (%v), want the token endpoint's", got, err)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+destinationPath("preview", "oauth2.googleapis.com"), ""); code != 409 ||
		lastBody != `{"error":"platform_destination","message":"Canyon allows oauth2.googleapis.com for every app already"}` {
		t.Errorf("allowing the token endpoint: %d %s, want 409 platform_destination", code, lastBody)
	}
	save(door("https://login.meridian.example", secret), "a token endpoint on an IP address")
	if got := platformRead(t, h); got != "keys.meridian.example login.meridian.example" {
		t.Errorf("platform %q, want no IP address and no user-info endpoint where the discovery names none", got)
	}
	for _, issuer := range []string{"https://plain.meridian.example", "https://other.meridian.example", "https://none.meridian.example"} {
		if out := save(door(issuer, secret), issuer); out["discovery_note"] != discoveryNote {
			t.Errorf("%s: discovery_note %q, want D5's discovery line", issuer, out["discovery_note"])
		}
		if code, out, _ := do(t, h.carl, http.MethodGet, h.base+doorPath, ""); code != 200 || out["discovery_note"] != discoveryNote {
			t.Errorf("%s: a member's read %d %s, want the discovery line", issuer, code, lastBody)
		}
		if got := platformRead(t, h); got != strings.TrimPrefix(issuer, "https://") {
			t.Errorf("%s: platform %q, want its issuer alone, nothing of a discovery that could not be read", issuer, got)
		}
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM front_door_discovery WHERE issuer IN ('https://plain.meridian.example', 'https://other.meridian.example', 'https://none.meridian.example')"); n != 0 {
		t.Errorf("%d discovery row(s) recorded from documents that could not be read", n)
	}
	asked.Store(0)
	if out := save(door("https://10.0.0.7/realms/apps", secret), "an issuer at an IP address"); out["discovery_note"] != "" || asked.Load() != 0 || platformRead(t, h) != "" {
		t.Errorf("an issuer that goes direct: discovery_note %q, %d fetch(es), platform %q; want none of either", out["discovery_note"], asked.Load(), platformRead(t, h))
	}
	save(door("https://login.meridian.example", secret), "the login door again")
	named.Store(true)
	audits := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'")
	save(door("https://login.meridian.example", ""), "a save that changes nothing")
	if got, n := platformRead(t, h), count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'"); got != "keys.meridian.example login.meridian.example me.meridian.example" || n != audits {
		t.Errorf("a save that changes nothing: platform %q, %d audit row(s); want the discovery fetched again, no row", got, n-audits)
	}
	rows := count(t, h.ctx, h.pool, "SELECT count(*) FROM front_door_discovery")
	asked.Store(0)
	if out := save(`{"form":"app.env.domain",`+doorOIDC+`}`, "choice 1"); out["discovery_note"] != "" || asked.Load() != 0 || count(t, h.ctx, h.pool, "SELECT count(*) FROM front_door_discovery") != rows {
		t.Errorf("choice 1: discovery_note %q, %d fetch(es); want none, and no discovery recorded", out["discovery_note"], asked.Load())
	}
}

// The server's minute loop records the discovery of each choice 2 issuer in use with none recorded — a door saved
// while its provider could not be read, or before saves recorded it — for as long as the server runs: a fetch that
// fails is tried again each interval, logged at its first failure and then at most hourly, the door's endpoints no
// platform destination meanwhile, until one records them; a door saved after that, while the provider could not be
// read, is recorded the same way. A recorded issuer, and a choice 1, are fetched no more.
func TestTheDiscoveryIsRecordedEveryMinuteWhileInUse(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t)
	storeDoor(t, h.ctx, h.pool, "https://accounts.google.com")
	if got := platformRead(t, h); got != "accounts.google.com" {
		t.Fatalf("platform %q before the discovery is recorded, want the issuer alone", got)
	}
	var asked atomic.Int32
	a := appOf(t, h.base)
	a.discovery = roundTrip(func(r *http.Request) (*http.Response, error) {
		if asked.Add(1) <= 2 {
			return nil, errors.New("connection refused")
		}
		return idpAt{"https://accounts.google.com": googleDiscovery, "https://login.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://tokens.meridian.example/token","jwks_uri":"https://login.meridian.example/keys"}`}.RoundTrip(r)
	})
	var logs syncBuffer
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { a.recordDiscovery(ctx, log.New(&logs, "", 0), 10*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(10 * time.Second); count(t, h.ctx, h.pool, "SELECT count(*) FROM front_door_discovery") == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not recorded 10 s on, after %d fetch(es)", asked.Load())
		}
	}
	if got := platformRead(t, h); got != googlePlatform {
		t.Errorf("platform %q, want the endpoints the discovery names", got)
	}
	if lines := strings.Count(logs.String(), "connection refused"); lines != 1 {
		t.Errorf("%d failure(s) logged of two, want the first alone: then at most one an hour", lines)
	}
	time.Sleep(100 * time.Millisecond) // ten intervals more: a recorded issuer is fetched no more
	storeDoor(t, h.ctx, h.pool, "https://login.meridian.example")
	for deadline := time.Now().Add(10 * time.Second); platformRead(t, h) != "login.meridian.example tokens.meridian.example"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("a door saved later is not recorded 10 s on: platform %q", platformRead(t, h))
		}
	}
	storeDoorJSON(t, h, `{"form":"app.env.domain",`+doorOIDC+`}`)
	time.Sleep(100 * time.Millisecond)
	if n := asked.Load(); n != 4 {
		t.Errorf("%d fetch(es); want two failures, the third recording, the fourth the later door's, and none for a recorded issuer or a choice 1", n)
	}
}

// A save that records an issuer's discovery between the loop's fetch of it and its write stands: the loop's write
// changes nothing, and says so.
func TestASaveRecordingTheDiscoveryFirstStands(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t)
	storeDoor(t, h.ctx, h.pool, "https://accounts.google.com")
	a := appOf(t, h.base)
	a.discovery = roundTrip(func(r *http.Request) (*http.Response, error) {
		if _, err := h.pool.Exec(r.Context(), "INSERT INTO front_door_discovery (issuer, hosts) VALUES ('https://accounts.google.com', '{saved.googleapis.com}') ON CONFLICT DO NOTHING"); err != nil {
			return nil, err
		}
		return idpAt{"https://accounts.google.com": googleDiscovery}.RoundTrip(r)
	})
	var logs syncBuffer
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { a.recordDiscovery(ctx, log.New(&logs, "", 0), time.Hour); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), "recorded by a save meanwhile"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the loop's write said nothing of the save's: %q", logs.String())
		}
	}
	cancel()
	<-done
	if got := platformRead(t, h); got != "accounts.google.com saved.googleapis.com" {
		t.Errorf("platform %q, want the save's hosts", got)
	}
}

// design/EGRESS.md §3.3: the platform destinations are every front door in use's, so a door replaced keeps its hosts
// — its issuer's, its key set's, its discovery's — while a running deploy under via canyon was given it, and drops
// them, moving the policy's version, once the last such deploy is given another; a deploy recorded without its run
// config adds none.
func TestADoorReplacedKeepsItsHostsWhileADeployUsesIt(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t) // claims-portal's preview and prod run under via canyon
	runsBehind := func(door string) {
		t.Helper()
		h.exec(t, `UPDATE deploys SET run_config = jsonb_build_object('egress', jsonb_build_object('via', 'canyon'), 'run_by', 'canyon',
			'front_door', jsonb_build_object('door', '`+door+`'::jsonb, 'set_at', '2026-10-19T09:00:00Z')), run_config_sha256 = repeat('a', 64), run_config_at = now()
			WHERE project_id = 'claims-portal'`)
	}
	h.exec(t, `INSERT INTO front_door_discovery (issuer, hosts) VALUES ('https://accounts.google.com', '{oauth2.googleapis.com,openidconnect.googleapis.com,www.googleapis.com}')`)
	storeDoorJSON(t, h, `{"form":"app.env.domain",`+doorOIDC+`}`)
	google := `{"choice": 2, "signin": {"issuer": "https://accounts.google.com", "client_id": "canyon-apps"}, "domain": "canyon.meridian.example", "form": "app.env.domain"}`
	runsBehind(google)
	if got := platformRead(t, h); got != "accounts.google.com login.meridian.example oauth2.googleapis.com openidconnect.googleapis.com www.googleapis.com" {
		t.Errorf("platform %q, want the stored door's and the one preview runs behind, its discovery's hosts too", got)
	}
	seq := func() int {
		t.Helper()
		return count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	}
	before := seq()
	runsBehind(`{"form": "app.env.domain", ` + doorOIDC + `}`)
	if got, after := platformRead(t, h), seq(); got != "login.meridian.example" || after == before {
		t.Errorf("deployed again behind the stored door: platform %q, seq %d → %d; want the old door's hosts gone and the version moved", got, before, after)
	}
}

// A deploy recorded without its run config — before #2373, or by the org's report — names no door: it adds no
// platform destination, and the read still answers (contracts/routes.md, the front-door row).
func TestADeployWithoutItsRunConfigAddsNoDoor(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t) // claims-portal's preview and prod run under via canyon, recorded without a run config
	storeDoorJSON(t, h, `{"form":"app.env.domain",`+doorOIDC+`}`)
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM deploys JOIN egress_envs USING (project_id, env) WHERE run_config IS NULL"); n != 2 {
		t.Fatalf("%d running deploy(s) without a run config, want preview's and prod's", n)
	}
	if got := platformRead(t, h); got != "login.meridian.example" {
		t.Errorf("platform %q, want the stored door's alone", got)
	}
}

// main starts recordDiscovery on a Kubernetes host, for the server's whole life: a door saved while its provider
// was unreachable, or before its discovery was recorded, is recorded by nothing else (contracts/routes.md, the
// front-door row). Read from main.go's syntax tree, as no test runs main.
func TestMainRecordsTheDiscoveryOnKubernetes(t *testing.T) {
	file, err := parser.ParseFile(gotoken.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var started bool
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if kube, ok := n.(*ast.IfStmt); ok && types.ExprString(kube.Cond) == "kube" {
					for _, s := range kube.Body.List {
						if g, ok := s.(*ast.GoStmt); ok && types.ExprString(g.Call.Fun) == "a.recordDiscovery" {
							started = true
						}
					}
				}
				return true
			})
		}
	}
	if !started {
		t.Error("main starts no go a.recordDiscovery(…) under if kube")
	}
}

// A deploy's step names its env before its /run, so the deploy row's first write, carrying its run_config, names no
// env anew: the write itself moves the policy's version, so egress-control holds the door it adds.
func TestADeploysFirstRunConfigMovesThePolicy(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t)
	h.exec(t, `INSERT INTO front_door_discovery (issuer, hosts) VALUES ('https://accounts.google.com', '{oauth2.googleapis.com,openidconnect.googleapis.com,www.googleapis.com}')`,
		"DELETE FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'",
		"INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'preview')") // as the deploy's step names it
	storeDoorJSON(t, h, `{"form":"app.env.domain",`+doorOIDC+`}`)
	before := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, egress, run_config, run_config_sha256, run_config_at)
		SELECT project_id, 'preview', id, 'canyon/claims-portal@'||$1, 'http://claims-portal.preview.localhost:7777', 'gr_seed', '{"via": "canyon"}',
			jsonb_build_object('egress', jsonb_build_object('via', 'canyon'), 'run_by', 'canyon', 'front_door', jsonb_build_object('door',
				'{"choice": 2, "signin": {"issuer": "https://accounts.google.com", "client_id": "canyon-apps"}, "domain": "canyon.meridian.example", "form": "app.env.domain"}'::jsonb,
				'set_at', '2026-10-19T09:00:00Z')), repeat('a', 64), now()
		FROM packages WHERE project_id = 'claims-portal'`, executortest.Digest); err != nil {
		t.Fatal(err)
	}
	if after := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy"); after == before {
		t.Errorf("the deploy row's first write left seq at %d: egress-control keeps the platform destinations without its door", after)
	}
	if got := platformRead(t, h); got != "accounts.google.com login.meridian.example oauth2.googleapis.com openidconnect.googleapis.com www.googleapis.com" {
		t.Errorf("platform %q, want the stored door's and the new deploy's", got)
	}
}

// A door a save replaced stays in use for 30 minutes — a /run that read it may still be starting its sidecars —
// its hosts platform destinations still; past them the minute loop forgets it, which moves the policy's version.
func TestAReplacedDoorStaysInUseForThirtyMinutes(t *testing.T) {
	onKubernetes(t)
	withDiscovery(t, idpAt{"https://accounts.google.com": googleDiscovery})
	h := outboundHost(t)
	for _, body := range []string{`{"choice":2,"signin":{"issuer":"https://accounts.google.com","client_id":"canyon-apps","client_secret":"cs-discovery-test"},"form":"app.env.domain"}`,
		`{"form":"app.env.domain",` + doorOIDC + `}`} {
		if code, _, _ := do(t, h.dana, http.MethodPut, h.base+doorPath, body); code != 200 {
			t.Fatalf("%s: %d %s", body, code, lastBody)
		}
	}
	if got := platformRead(t, h); got != "accounts.google.com login.meridian.example oauth2.googleapis.com openidconnect.googleapis.com www.googleapis.com" {
		t.Errorf("platform %q just after Google's door was replaced, want its hosts still", got)
	}
	h.exec(t, "UPDATE replaced_front_doors SET replaced_at = now() - interval '31 minutes'")
	before := count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { appOf(t, h.base).recordDiscovery(ctx, log.New(io.Discard, "", 0), time.Hour); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); count(t, h.ctx, h.pool, "SELECT count(*) FROM replaced_front_doors") != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the minute loop kept a door replaced 31 minutes ago")
		}
	}
	cancel()
	<-done
	if got, after := platformRead(t, h), count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy"); got != "login.meridian.example" || after == before {
		t.Errorf("past its 30 minutes: platform %q, seq %d → %d; want the replaced door's hosts gone and the version moved", got, before, after)
	}
}

// An issuer's recorded hosts only grow: a sidecar started before its jwks_uri moved keeps refreshing at the host its
// document named then, so a save reading the new document adds the new host and keeps the old.
func TestAnIssuersRecordedHostsOnlyGrow(t *testing.T) {
	onKubernetes(t)
	withDiscovery(t, idpAt{"https://login.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://login.meridian.example/token","jwks_uri":"https://keys-b.meridian.example/jwks"}`})
	h := outboundHost(t)
	h.exec(t, `INSERT INTO front_door_discovery (issuer, hosts) VALUES ('https://login.meridian.example', '{keys-a.meridian.example,login.meridian.example}')`)
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+doorPath, `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps","client_secret":"cs-discovery-test"},"form":"app.env.domain"}`); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	if got := platformRead(t, h); got != "keys-a.meridian.example keys-b.meridian.example login.meridian.example" {
		t.Errorf("platform %q, want the moved key set's new host and its old one", got)
	}
}

// The minute loop refreshes, once a day, the discovery of each choice 2 issuer in use with hosts recorded, so a
// provider that moves an endpoint to a new host is noticed (#2389): a document read adds its hosts to those recorded,
// each new one logged, and moves the policy's version; one that finds nothing new moves nothing. A refresh that
// fails keeps every recorded host, is tried again every minute, marks the row once and says so in discovery_note
// until a read succeeds — the loop's or a save's — and neither the mark nor its clearing moves the policy's version,
// which names neither.
func TestARecordedDiscoveryIsRefreshedDaily(t *testing.T) {
	onKubernetes(t)
	const issuer = "https://login.meridian.example"
	const failedNote = "Canyon couldn't read your sign-in provider's discovery document in its daily check, and keeps trying every minute. If your provider moved an endpoint, apps whose outbound mode is allowlist can't sign people in until Canyon reads it again."
	const hosts = "keys-a.meridian.example keys-b.meridian.example login.meridian.example"
	var asked atomic.Int32
	var failing atomic.Bool
	withDiscovery(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		asked.Add(1)
		if failing.Load() {
			return nil, errors.New("connection refused")
		}
		return idpAt{issuer: `{"issuer":"` + issuer + `","authorization_endpoint":"` + issuer + `/auth","token_endpoint":"` + issuer + `/token","jwks_uri":"https://keys-b.meridian.example/jwks"}`}.RoundTrip(r)
	}))
	h := outboundHost(t)
	h.exec(t, `INSERT INTO front_door_discovery (issuer, hosts) VALUES ('`+issuer+`', '{keys-a.meridian.example,login.meridian.example}')`)
	storeDoor(t, h.ctx, h.pool, issuer)
	seq := func() int {
		t.Helper()
		return count(t, h.ctx, h.pool, "SELECT seq FROM egress_policy")
	}
	note := func() string {
		t.Helper()
		code, out, _ := do(t, h.carl, http.MethodGet, h.base+doorPath, "")
		if code != 200 {
			t.Fatalf("the front door's read: %d %s", code, lastBody)
		}
		return out["discovery_note"].(string)
	}
	until := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not 10 s on, after %d fetch(es); platform %q", what, asked.Load(), platformRead(t, h))
			}
		}
	}
	mark := func() (at time.Time) {
		t.Helper()
		if err := h.pool.QueryRow(h.ctx, "SELECT refresh_failed_at FROM front_door_discovery").Scan(&at); err != nil {
			t.Fatalf("the failure's mark: %v", err)
		}
		return at
	}
	backdate := func() { h.exec(t, "UPDATE front_door_discovery SET recorded_at = now() - interval '25 hours'") } // a day and more ago
	var logs syncBuffer
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { appOf(t, h.base).recordDiscovery(ctx, log.New(&logs, "", 0), 10*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(100 * time.Millisecond) // ten intervals: a discovery recorded within the day is not fetched
	if n := asked.Load(); n != 0 {
		t.Errorf("%d fetch(es) of a discovery recorded within the day, want none", n)
	}
	before := seq()
	backdate()
	until("the moved key set's host recorded", func() bool { return strings.Contains(platformRead(t, h), "keys-b") })
	if got, after := platformRead(t, h), seq(); got != hosts || after == before {
		t.Errorf("refreshed: platform %q, seq %d → %d; want the new host beside the old, and the version moved", got, before, after)
	}
	if got := note(); !strings.Contains(logs.String(), "keys-b.meridian.example") || got != "" {
		t.Errorf("refreshed: log %q, discovery_note %q; want the new host logged, no note", logs.String(), got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := asked.Load(); n != 1 {
		t.Errorf("%d fetch(es), want one: a discovery refreshed is not fetched again that day", n)
	}

	quiet := seq()
	backdate()
	until("a refresh finding nothing new", func() bool {
		return asked.Load() >= 2 && count(t, h.ctx, h.pool, "SELECT count(*) FROM front_door_discovery WHERE recorded_at > now() - interval '1 hour'") == 1
	})
	if got, after := platformRead(t, h), seq(); got != hosts || after != quiet || strings.Count(logs.String(), "anew") != 1 {
		t.Errorf("nothing new: platform %q, seq %d → %d, log %q; want the hosts as they were, the version where it was, nothing logged", got, quiet, after, logs.String())
	}

	failing.Store(true)
	prior := seq()
	backdate()
	until("the failed refresh's note", func() bool { return note() == failedNote })
	failed, first := seq(), mark()
	until("five tries more", func() bool { return asked.Load() >= 8 })
	if got, after, again := platformRead(t, h), seq(), mark(); got != hosts || failed != prior || after != prior || !again.Equal(first) {
		t.Errorf("failing: platform %q, seq %d → %d → %d, marked %v then %v; want every recorded host kept, the version where it was, the row marked once", got, prior, failed, after, first, again)
	}
	if lines := strings.Count(logs.String(), "connection refused"); lines != 1 {
		t.Errorf("%d failure(s) logged, want the first alone: then at most one an hour", lines)
	}

	failing.Store(false)
	until("the note cleared", func() bool { return note() == "" })
	if got, after := platformRead(t, h), seq(); got != hosts || after != prior {
		t.Errorf("read again: platform %q, seq %d → %d; want the hosts as they were, and the version where it was", got, prior, after)
	}

	// A save that reads the document clears the note too, the door replaced or not: marked again, the row is made not
	// due, so only the save can clear it.
	for _, secret := range []string{`,"client_secret":"cs-discovery-test"`, ""} {
		failing.Store(true)
		backdate()
		until("the failed refresh's note again", func() bool { return note() == failedNote })
		h.exec(t, "UPDATE front_door_discovery SET recorded_at = now()")
		failing.Store(false)
		body := `{"choice":2,"signin":{"issuer":"` + issuer + `","client_id":"canyon-apps"` + secret + `},"form":"app.env.domain"}`
		if code, out, _ := do(t, h.dana, http.MethodPut, h.base+doorPath, body); code != 200 || out["discovery_note"] != "" {
			t.Errorf("a save reading the document (secret %q): %d %s, want no note", secret, code, lastBody)
		}
		if got := note(); got != "" {
			t.Errorf("the read after a save reading the document (secret %q): discovery_note %q, want none", secret, got)
		}
	}
}

// #2722: after an Allow, an agent read the app's old failures as current, as nothing said the destination was
// allowed, by whom or when. The PUT that puts an entry there records the person, the time and the latest refusal of
// the requests it closed, none when it closed none, and the read answers them in allows[], sorted as allowed[] is,
// to a contributor and up. A repeat records nothing new; a DELETE takes the record with the entry, and the next PUT
// records its own. An entry allowed before #2722 is on allowed[] and has none. Below contributor the read is 403
// team_required, naming no Allow (design/PERMISSIONS.md, Outbound).
func TestAnAllowRecordsWhoAllowedItWhenAndTheRefusalsItClosed(t *testing.T) {
	h := outboundHost(t)
	h.exec(t, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.openai.com', 'pending', '2026-11-08T11:13:29Z', '2026-11-08T11:37:41Z', 28),
		('claims-portal', 'prod', 'chat.openai.com', 'dismissed', '2026-11-08T10:00:00Z', '2026-11-08T10:05:00Z', 2),
		('claims-portal', 'preview', 'api.openai.com', 'pending', '2026-11-08T11:13:29Z', '2026-11-08T11:50:00Z', 3)`,
		`INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'hooks.slack.com')`)
	before := time.Now().Add(-time.Minute)
	for _, step := range []struct {
		who         *http.Client
		destination string
	}{{h.carl, "*.openai.com"}, {h.olive, "api.stripe.com"}, {h.dana, "*.openai.com"}} {
		if code, _, _ := do(t, step.who, http.MethodPut, h.base+destinationPath("prod", step.destination), ""); code != 204 {
			t.Fatalf("PUT %s: %d %s", step.destination, code, lastBody)
		}
	}
	display := func(sub string) string {
		var d string
		if err := h.pool.QueryRow(h.ctx, "SELECT actor_display FROM sessions WHERE actor_subject = $1 AND flow = 'browser'", sub).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	type allow struct{ entry, by, display, refused string }
	allows := func(env string) []allow {
		t.Helper()
		code, out, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, "") // a contributor's read
		if code != 200 {
			t.Fatalf("the contributor's read: %d %s", code, lastBody)
		}
		egressShape(t, "project_read", lastBody)
		var got []allow
		for _, a := range out["envs"].(map[string]any)[env].(map[string]any)["allows"].([]any) {
			a := a.(map[string]any)
			by := a["by"].(map[string]any)
			if at, err := time.Parse(time.RFC3339, a["at"].(string)); err != nil || at.Before(before) || by["kind"] != "person" {
				t.Errorf("%s's Allow of %v: at %v, by %v, want a person, now", env, a["entry"], a["at"], by)
			}
			refused, _ := a["last_refused_at"].(string)
			got = append(got, allow{a["entry"].(string), by["subject"].(string), by["display"].(string), refused})
		}
		return got
	}
	want := []allow{{"*.openai.com", "sub-carl", display("sub-carl"), "2026-11-08T11:37:41Z"}, {"api.stripe.com", "sub-olive", display("sub-olive"), ""}}
	if got := allows("prod"); !slices.Equal(got, want) {
		t.Errorf("prod's allows %v, want %v: the first PUT's person and the latest refusal it closed, none for an entry that closed none, none for one allowed before", got, want)
	}
	if got := strs(h.envRead(t, "prod")["allowed"]); !slices.Equal(got, []string{"*.openai.com", "api.stripe.com", "hooks.slack.com"}) {
		t.Errorf("prod allows %v", got)
	}
	if got := allows("preview"); len(got) != 0 {
		t.Errorf("preview's allows %v, want none: prod's Allow is prod's", got)
	}
	if code, _, _ := do(t, h.carl, http.MethodDelete, h.base+destinationPath("prod", "*.openai.com"), ""); code != 204 {
		t.Fatalf("DELETE: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+destinationPath("prod", "*.openai.com"), ""); code != 204 {
		t.Fatalf("PUT again: %d %s", code, lastBody)
	}
	want[0] = allow{"*.openai.com", "sub-dana", display("sub-dana"), ""}
	if got := allows("prod"); !slices.Equal(got, want) {
		t.Errorf("after a DELETE and a PUT, prod's allows %v, want %v: the record went with the entry, and the new one closed nothing", got, want)
	}
	if code, _, _ := do(t, h.vic, http.MethodGet, h.base+outboundApp, ""); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Errorf("a read-only member's read: %d %s, want team_required naming the owners alone", code, lastBody)
	}
}
