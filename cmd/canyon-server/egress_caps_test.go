package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"

	"github.com/getcanyon-ai/canyon/contracts"
)

// The caps' defaults, 0130's: what a host that never set one holds.
const (
	defaultDestinations = 500
	defaultPending      = 50
)

// The three egress caps are an admin's to set (design/EGRESS.md §3.3, §14; #2691; contracts/routes.md, the egress
// setting): a host that never set one reads today's numbers; an admin from a browser sets any of them, a key left out
// keeping its value, one egress.set row naming what moved; a member is refused; a cap out of its range, or no whole
// number, is 400 invalid_caps and nothing moves — never a value that means no cap. The policy carries new_names, and
// known[] follows the destinations cap up to 500, the most an egress-control takes.
func TestEgressCaps(t *testing.T) {
	h := outboundHost(t)
	egressWait = time.Second // rawPolicy reads the policy as a replica that never asks again: a change no longer waits it out (#136)
	t.Cleanup(func() { egressWait = egressDefault })
	caps := func() string {
		code, out, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/egress", "")
		if code != 200 {
			t.Fatalf("the admin's read: %d %s", code, lastBody)
		}
		return fmt.Sprint(out["caps"])
	}
	if got := caps(); got != "map[destinations:500 new_names:30 pending:50]" {
		t.Errorf("a host that never set a cap reads %s, want the defaults: 500 destinations, 30 new names, 50 pending", got)
	}
	egressShape(t, "setting_admin", lastBody)
	if code, _, _ := do(t, h.vic, http.MethodPut, h.base+"/api/v1/egress", `{"caps":{"new_names":60}}`); code != 403 || !strings.Contains(lastBody, `"admin_required"`) {
		t.Errorf("a member setting a cap: %d %s, want 403 admin_required", code, lastBody)
	}
	if code, _, _ := do(t, h.vic, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 || strings.Contains(lastBody, "caps") {
		t.Errorf("a member's read: %d %s, want via and mode alone", code, lastBody)
	}
	for _, body := range []string{`{"caps":{}}`, `{"caps":null}`, `{"caps":30}`, `{"caps":{"new_names":0}}`, `{"caps":{"new_names":601}}`, `{"caps":{"destinations":49}}`,
		`{"caps":{"destinations":5001}}`, `{"caps":{"pending":0}}`, `{"caps":{"pending":501}}`, `{"caps":{"pending":"50"}}`, `{"caps":{"pending":50.5}}`, `{"caps":{"pending":5e1}}`,
		`{"caps":{"pending":null}}`, `{"caps":{"pending":-1}}`, `{"caps":{"rows":500}}`, `{"via":"canyon","caps":{"new_names":0}}`} {
		if code, out, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", body); code != 400 || out["error"] != "invalid_caps" {
			t.Errorf("%s: %d %s, want 400 invalid_caps", body, code, lastBody)
		}
	}
	if got := caps(); got != "map[destinations:500 new_names:30 pending:50]" {
		t.Errorf("after refused bodies the caps read %s, want them unmoved", got)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"caps":{"new_names":60,"destinations":50}}`); code != 200 {
		t.Fatalf("an admin setting two caps: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	if got := caps(); got != "map[destinations:50 new_names:60 pending:50]" {
		t.Errorf("after setting two the caps read %s, want destinations 50, new_names 60, pending kept at 50", got)
	}
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"caps":{"new_names":60}}`); code != 200 {
		t.Errorf("a cap set to what it is: %d %s", code, lastBody)
	}
	const moved = `{"caps": {"after": {"pending": 50, "new_names": 60, "destinations": 50}, "before": {"pending": 50, "new_names": 30, "destinations": 500}}}`
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.set'"); n != 1 ||
		count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.set' AND detail = '"+moved+"'") != 1 {
		t.Errorf("%d egress.set rows, want one naming the caps before and after, none for a PUT that moved nothing", n)
	}
	h.exec(t, `INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'prod') ON CONFLICT DO NOTHING;
		INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags, admitted_at)
		SELECT 'claims-portal', 'prod', 'n' || i || '.example.com', 443, now() - make_interval(secs => i), now() - make_interval(secs => i), 1, 'allowed', '2xx', 'connect', '{}', now() - make_interval(secs => i)
		FROM generate_series(1, 60) i`)
	_, base := policyAt(t, h.pool)
	code, p := pullPolicy(t, base, "canyon-egress-a", "have=")
	known := -1
	for _, e := range p.Envs {
		if e.App == "claims-portal" && e.Env == "prod" {
			known = len(e.Known)
		}
	}
	if code != http.StatusOK || p.Caps.NewNames != 60 || known != 50 {
		t.Errorf("the policy: %d, new_names %d, prod's known[] %d names, want 200, the admin's 60, and the destinations cap's 50", code, p.Caps.NewNames, known)
	}

	// Past 500, known[] stops at 500, the most an egress-control takes: one over fails CheckEgressPolicy, and every
	// replica would refuse every policy whole.
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+"/api/v1/egress", `{"caps":{"destinations":1000}}`); code != 200 {
		t.Fatalf("an admin setting destinations to 1000: %d %s", code, lastBody)
	}
	h.exec(t, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags, admitted_at)
		SELECT 'claims-portal', 'prod', 'm' || i || '.example.com', 443, now() - make_interval(secs => i), now() - make_interval(secs => i), 1, 'allowed', '2xx', 'connect', '{}', now() - make_interval(secs => i)
		FROM generate_series(1, 600) i`)
	code, raw := pullPolicyRaw(t, base, "canyon-egress-a", "have=")
	var wide egressPolicy
	if err := json.Unmarshal(raw, &wide); code != http.StatusOK || err != nil {
		t.Fatalf("the policy with 660 names admitted: %d %v", code, err)
	}
	known = -1
	for _, e := range wide.Envs {
		if e.App == "claims-portal" && e.Env == "prod" {
			known = len(e.Known)
		}
	}
	if err := contracts.CheckEgressPolicy(raw); known != 500 || err != nil {
		t.Errorf("destinations 1000 and 660 names admitted: prod's known[] %d names, the policy checks: %v; want 500, and it passes", known, err)
	}
}

// The receiver holds the caps as the admin set them (#2691): a destinations cap lowered below the rows an app env
// holds deletes none — they go on — and its new name is over_cap; a pending cap of 1 files one request and no more.
func TestEgressCapsHoldTheRecords(t *testing.T) {
	f := newEgressFixture(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO projects (id) VALUES ('claims-portal'); UPDATE egress_settings SET cap_destinations = 50, cap_pending = 1`); err != nil {
		t.Fatal(err)
	}
	f.fill(t, "claims-portal", "prod", 60)
	checkedCall := func(dest, outcome, reason string, status uint32) *accesslogdatav3.HTTPAccessLogEntry {
		return checked(entry("172.28.3.11", corev3.RequestMethod_CONNECT, "", dest+":443", status), "claims-portal", "prod", outcome, reason)
	}
	f.send(t, checkedCall("fill-1.example.com", "allowed", "", 200), checkedCall("api.stripe.com", "allowed", "", 200),
		checkedCall("api.openai.com", "refused", "not_allowed", 403), checkedCall("api.anthropic.com", "refused", "not_allowed", 403))
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_destinations WHERE app = 'claims-portal'"); n != 60 {
		t.Errorf("%d rows, want the 60 held, none deleted and none added past the cap of 50", n)
	}
	if n := count(t, f.ctx, f.pool, "SELECT calls FROM egress_destinations WHERE destination = 'fill-1.example.com'"); n != 2 {
		t.Errorf("a held row past the cap has %d calls, want 2: it goes on", n)
	}
	if got := f.overCapOf(t); fmt.Sprint(got) != "[claims-portal/prod:3]" {
		t.Errorf("over_cap %v, want the three new names' calls", got)
	}
	if n := count(t, f.ctx, f.pool, "SELECT count(*) FROM egress_requests WHERE state = 'pending'"); n != 1 {
		t.Errorf("%d pending requests, want the cap's 1", n)
	}
}
