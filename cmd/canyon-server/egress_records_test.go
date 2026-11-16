package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/getcanyon-ai/canyon/executor"
)

// checked is e as the check answered it: its dynamic metadata, which the entry carries (contracts/routes.md,
// The check), and Envoy's UAEX on a refusal.
func checked(e *accesslogdatav3.HTTPAccessLogEntry, app, env, outcome, reason string) *accesslogdatav3.HTTPAccessLogEntry {
	md := map[string]any{"outcome": outcome}
	if app != "" {
		md["app"] = app
	}
	if env != "" {
		md["env"] = env
	}
	if reason != "" {
		md["reason"] = reason
	}
	fields, _ := structpb.NewStruct(md)
	e.CommonProperties.Metadata = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{extAuthz: fields}}
	if outcome == "refused" {
		e.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UnauthorizedDetails: &accesslogdatav3.ResponseFlags_Unauthorized{}}
	}
	return e
}

// The receiver keeps what the check said: a row's last_outcome is the check's outcome, and the caller the
// check named wins over the address's — it is the caller the call was decided for, which only egress-control
// writes. A call it named no caller for (unknown_caller) is the address's; one it named out of contract,
// no one's. The check's outcome comes first; Envoy's UAEX makes a call refused only when the entry carries no
// metadata, the check having not answered.
func TestTheCheckNamesTheCallerAndTheOutcome(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "intake-bot", Target: "prod", Kind: "web"})
	unanswered := entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.anthropic.com:443", 503)
	unanswered.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UnauthorizedDetails: &accesslogdatav3.ResponseFlags_Unauthorized{}}
	flagged := checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.mistral.ai:443", 200), "claims-portal", "prod", "allowed", "")
	flagged.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{UnauthorizedDetails: &accesslogdatav3.ResponseFlags_Unauthorized{}}
	f.send(t, unanswered, flagged,
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200), "claims-portal", "prod", "allowed", ""),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 403), "claims-portal", "prod", "refused", "not_allowed"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "hooks.slack.com:443", 403), "", "", "refused", "unknown_caller"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "10.0.0.1:443", 403), "claims-portal", "prod", "refused", "invalid_destination"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.github.com:443", 200), "Claims Portal", "prod", "allowed", ""),
	)
	got := map[string]string{}
	rows, err := f.pool.Query(f.ctx, "SELECT app || ' ' || env || ' ' || destination, last_outcome FROM egress_destinations")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	want := map[string]string{
		"claims-portal prod api.stripe.com": "allowed",
		"claims-portal prod api.openai.com": "refused",
		"intake-bot prod hooks.slack.com":   "refused",
		"claims-portal prod 10.0.0.1":       "refused",
		"intake-bot prod api.anthropic.com": "refused",
		"claims-portal prod api.mistral.ai": "allowed",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	if n := count(t, f.ctx, f.pool, "SELECT coalesce(sum(calls), 0) FROM egress_gaps WHERE reason = 'unattributed'"); n != 1 {
		t.Errorf("%d unattributed calls, want the one the check named out of contract", n)
	}
}

// A call Envoy routes nowhere is refused (contracts/routes.md, the receiver): the check never sees it, so the entry carries
// no metadata and no UAEX, only Envoy's NR, and it never left. A port other than 80 and 443 is one, refused in every mode
// (egress.schema.json's mode), and so is anything else Envoy has no route for, whatever its port: never an allowed call that
// failed (#2723). The check's say comes first.
func TestAPortEnvoyRoutesNowhereIsRefused(t *testing.T) {
	noRoute := func(e *accesslogdatav3.HTTPAccessLogEntry) *accesslogdatav3.HTTPAccessLogEntry {
		e.CommonProperties.ResponseFlags = &accesslogdatav3.ResponseFlags{NoRouteFound: true}
		return e
	}
	for _, c := range []struct {
		name    string
		e       *accesslogdatav3.HTTPAccessLogEntry
		port    int
		outcome string
	}{
		{"a tunnel to 8443", noRoute(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "example.com:8443", 404)), 8443, "refused"},
		{"a plain request to 8080", noRoute(entry("172.28.3.11", corev3.RequestMethod_GET, "http", "example.com:8080", 404)), 8080, "refused"},
		{"an https request to 8443", noRoute(entry("172.28.3.11", corev3.RequestMethod_GET, "https", "example.com:8443", 404)), 8443, "refused"},
		{"a tunnel to a name no host name is", noRoute(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "localhost:8443", 404)), 443, "refused"},
		{"a plain request to 443", noRoute(entry("172.28.3.11", corev3.RequestMethod_GET, "http", "example.com:443", 404)), 443, "refused"},
		{"a tunnel to 443 the check never saw", entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "example.com:443", 200), 443, "allowed"},
		{"a plain request to 80", entry("172.28.3.11", corev3.RequestMethod_GET, "http", "example.com", 200), 80, "allowed"},
		{"a tunnel to 443 the check allowed", checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "example.com:443", 200), "claims-portal", "prod", "allowed", ""), 443, "allowed"},
	} {
		got := callOf(c.e, time.Now(), false)
		if got.port != c.port || got.outcome != c.outcome {
			t.Errorf("%s: port %d, %s; want port %d, %s", c.name, got.port, got.outcome, c.port, c.outcome)
		}
	}
}

// contracts/routes.md, The check: the first not_allowed refusal of (app, env, destination) files an egress
// request, pending, by Canyon, with one egress.request audit row; each later one counts on it, dismissed or
// not; a destination an entry allows now files none, nor does an env holding 50 pending. A not_allowed an
// egress-control from before #2344 let through, outcome allowed — a replica of the release before, mid-rollout —
// files one all the same.
func TestANotAllowedRefusalFilesOneRequest(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO projects (id) VALUES ('claims-portal');
		INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', '*.slack.com');
		INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals)
			VALUES ('claims-portal', 'prod', 'api.anthropic.com', 'dismissed', now() - interval '1 day', now() - interval '1 day', 2)`); err != nil {
		t.Fatal(err)
	}
	refusal := func(dest string) *accesslogdatav3.HTTPAccessLogEntry {
		return checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", dest+":443", 403), "claims-portal", "prod", "refused", "not_allowed")
	}
	f.send(t, refusal("api.openai.com"), refusal("api.openai.com"), refusal("api.anthropic.com"), refusal("hooks.slack.com"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.mistral.ai:443", 200), "claims-portal", "prod", "allowed", "not_allowed"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "10.0.0.1:443", 403), "claims-portal", "prod", "refused", "invalid_destination"))
	f.send(t, refusal("api.openai.com"))
	requests := strings.Join(f.rowsOf(t, "(SELECT destination, state, refusals FROM egress_requests ORDER BY destination)"), "\n")
	want := strings.Join([]string{
		`{"destination":"api.anthropic.com","state":"dismissed","refusals":3}`,
		`{"destination":"api.mistral.ai","state":"pending","refusals":1}`,
		`{"destination":"api.openai.com","state":"pending","refusals":3}`,
	}, "\n")
	if requests != want {
		t.Errorf("requests:\n%s\nwant:\n%s", requests, want)
	}
	audit := f.rowsOf(t, "(SELECT action, actor_kind, project_id, detail FROM audit_events)")
	if len(audit) != 2 || !strings.Contains(strings.Join(audit, "\n"), `{"action":"egress.request","actor_kind":"canyon","project_id":"claims-portal","detail":{"env": "prod", "destination": "api.openai.com"}}`) {
		t.Errorf("audit rows %v, want one egress.request a request filed", audit)
	}

	var pending []*accesslogdatav3.HTTPAccessLogEntry
	for i := range defaultPending {
		pending = append(pending, refusal(fmt.Sprintf("n%d.example.com", i)))
	}
	f.send(t, pending...)
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_requests WHERE state = 'pending'"); n != defaultPending {
		t.Errorf("%d pending, want %d at most", n, defaultPending)
	}
}

// Only ext_authz's own namespace names a caller and an outcome: the same keys in another namespace (a filter
// an app's header could feed), in the entry's request headers, or a caller with no app, name no one, and the
// destination is the authority's, canonicalised, whatever the metadata says.
func TestOnlyTheChecksNamespaceNamesTheCaller(t *testing.T) {
	f := newEgressFixture(t)
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "intake-bot", Target: "prod", Kind: "web"})
	elsewhere := entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200)
	forged, _ := structpb.NewStruct(map[string]any{"app": "claims-portal", "env": "prod", "outcome": "allowed", "destination": "api.openai.com"})
	elsewhere.CommonProperties.Metadata = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{"envoy.filters.http.header_to_metadata": forged}}
	elsewhere.Request.RequestHeaders = map[string]string{"app": "claims-portal", "env": "prod", "x-canyon-caller": "claims-portal/prod"}
	f.send(t, elsewhere,
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "API.OpenAI.com.:443", 200), "claims-portal", "prod", "allowed", ""),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "hooks.slack.com:443", 200), "", "prod", "allowed", ""))
	rows := strings.Join(f.rowsOf(t, "(SELECT app, env, destination FROM egress_destinations ORDER BY 1, 2, 3)"), "\n")
	want := `{"app":"claims-portal","env":"prod","destination":"api.openai.com"}` + "\n" + `{"app":"intake-bot","env":"prod","destination":"api.stripe.com"}`
	if rows != want {
		t.Errorf("rows:\n%s\nwant:\n%s", rows, want)
	}
	if n := count(t, f.ctx, f.pool, "SELECT coalesce(sum(calls), 0) FROM egress_gaps WHERE reason = 'unattributed'"); n != 1 {
		t.Errorf("%d unattributed calls, want the one whose check named an env and no app", n)
	}
}

// A name an app was only refused — rate_limited, not_allowed, port — never becomes known, so being refused
// never makes a name not new to the rate limit: the receiver sets admitted_at only for a call that went
// through, and the policy's known[] reads it (contracts/routes.md, the policy row).
func TestARefusedNameNeverBecomesKnown(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO projects (id) VALUES ('claims-portal'); INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'prod')`); err != nil {
		t.Fatal(err)
	}
	refusal := func(authority, reason string) *accesslogdatav3.HTTPAccessLogEntry {
		return checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", authority, 403), "claims-portal", "prod", "refused", reason)
	}
	f.send(t, refusal("flood.example.com:443", "rate_limited"), refusal("flood.example.com:443", "rate_limited"),
		refusal("api.openai.com:443", "not_allowed"), refusal("api.anthropic.com:8443", "port"),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.stripe.com:443", 200), "claims-portal", "prod", "allowed", ""),
		checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.mistral.ai:443", 200), "claims-portal", "prod", "allowed", "not_allowed"))
	f.send(t, refusal("api.stripe.com:443", "rate_limited")) // admitted before, refused since: still known
	_, base := policyAt(t, f.pool)
	code, p := pullPolicy(t, base, "canyon-egress-a", "have=")
	if code != http.StatusOK || len(p.Envs) != 1 || !slices.Equal(p.Envs[0].Known, []string{"api.mistral.ai", "api.stripe.com"}) {
		t.Errorf("%d %+v, want known[] the names a call went through to alone: api.mistral.ai, api.stripe.com", code, p.Envs)
	}
}

// A request and its record count the same refused calls (#2718): N not_allowed refusals of one destination, sent
// across flushes — two, then one, then three — are N refusals of its request and N calls of its row, each flush
// adding what it took. Past that they count different things, by design (contracts/routes.md, Egress requests): a
// call refused for a reason no allowlist entry lifts — unknown_caller, while the policy does not yet name a new
// pod's address — is a call of the row, the address's caller's, and no refusal of the request.
func TestARequestsRefusalsMatchItsRecord(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO projects (id) VALUES ('claims-portal')`); err != nil {
		t.Fatal(err)
	}
	f.exec.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web"})
	refusal := func() *accesslogdatav3.HTTPAccessLogEntry {
		return checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 403), "claims-portal", "prod", "refused", "not_allowed")
	}
	counts := func() (request, record int) {
		return count(t, f.ctx, f.pool, "SELECT coalesce(sum(refusals), 0) FROM egress_requests WHERE project_id = 'claims-portal' AND env = 'prod' AND destination = 'api.openai.com'"),
			count(t, f.ctx, f.pool, "SELECT coalesce(sum(calls), 0) FROM egress_destinations WHERE app = 'claims-portal' AND env = 'prod' AND destination = 'api.openai.com' AND port = 443")
	}
	n := 0
	for _, flush := range []int{2, 1, 3} {
		var batch []*accesslogdatav3.HTTPAccessLogEntry
		for range flush {
			batch = append(batch, refusal())
		}
		f.send(t, batch...)
		n += flush
		if request, record := counts(); request != n || record != n {
			t.Errorf("after %d not_allowed refusals over flushes of %d: the request counts %d refusals and the row %d calls, want %d each", n, flush, request, record, n)
		}
	}
	f.send(t, checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", "api.openai.com:443", 403), "", "", "refused", "unknown_caller"))
	if request, record := counts(); request != n || record != n+1 {
		t.Errorf("an unknown_caller refusal more: the request counts %d refusals and the row %d calls, want %d and %d: the row counts every call, the request those an allowlist entry would let through", request, record, n, n+1)
	}
}
