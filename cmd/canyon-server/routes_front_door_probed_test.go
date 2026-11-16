package main

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// unprobedFixture is contracts/testdata/front-door/meridian.json's get_unprobed: a choice 1 saved beside a platform
// webhook while the probe found the cluster unenforced (#2438).
func unprobedFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/front-door/meridian.json")
	var f struct {
		GetUnprobed map[string]any `json:"get_unprobed"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &f)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f.GetUnprobed
}

// notEnforcingLine is D5's not-enforcing line (design/EXPERIENCE.md; #2507): probe_note's lead while the stored door's
// save did not find NetworkPolicy enforced, in place of the not-checked line.
const notEnforcingLine = "Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced, so no deploy goes through, whoever runs the apps."

const unprobedPut = `{"choice":1,"gateway":"oidc","issuer":"https://login.meridian.example","audience":"canyon-gateway","jwks_url":"https://login.meridian.example/keys","cookie":"_pomerium","domain":"apps.meridian.example","form":"app-env.domain"}`

// A cluster that can't pass the probe still reaches hand-off (#2438): without a platform webhook the probe refuses the
// door, as does a choice 2 beside one; a choice 1 beside one is saved not probed, the fixture's answer, and run_by: org
// goes through. Back to canyon is refused while the door is not probed, and goes through once a save passes.
func TestAChoice1ThatFailsTheProbeIsSavedUnprobedBesideAWebhook(t *testing.T) {
	withOIDC(t) // Canyon signs in at login.example.com: the fixture's door is another issuer's
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	servesHandOff(t)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	want := unprobedFixture(t)
	fix := strings.TrimPrefix(want["probe_note"].(string), notEnforcingLine+" ")
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: fix})
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, unprobedPut); code != 409 || body["error"] != "probe_failed" || body["message"] != fix {
		t.Fatalf("with no webhook the probe refuses the door: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`); code != 200 {
		t.Fatalf("the webhook: %d %s", code, lastBody)
	}
	choice2 := `{"form":"app.env.domain","choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps","client_secret":"oidc-client-secret-not-real"}}`
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, choice2); code != 409 || body["error"] != "probe_failed" {
		t.Fatalf("a choice 2 beside a webhook is still refused: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM front_doors"); n != 0 {
		t.Fatalf("a refused save wrote %d row(s)", n)
	}
	code, saved, _ := do(t, dana, http.MethodPut, base+doorPath, unprobedPut)
	door, _ := saved["front_door"].(map[string]any)
	if code != 200 || door == nil {
		t.Fatalf("a choice 1 beside a webhook: %d %s", code, lastBody)
	}
	want["front_door"].(map[string]any)["set_by"], want["front_door"].(map[string]any)["set_at"] = door["set_by"], door["set_at"]
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("the save answers the fixture's unprobed read:\n%s", lastBody)
	}
	if code, read, _ := do(t, carol, http.MethodGet, base+doorPath, ""); code != 200 || !reflect.DeepEqual(read, want) {
		t.Fatalf("a member reads it, the note included: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"run_by":"org"}`); code != 200 || body["run_by"] != "org" {
		t.Fatalf("hand-off behind the unprobed door: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"run_by":"canyon"}`); code != 409 || body["error"] != "front_door_unchecked" || body["message"] != frontDoorUnchecked {
		t.Fatalf("back to canyon behind it: %d %s", code, lastBody)
	}
	if got := getPreferences(t, dana, base)["run_by"]; got != "org" {
		t.Fatalf("the refusal wrote run_by %v", got)
	}
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"})
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, unprobedPut); code != 200 || body["front_door"].(map[string]any)["probed"] != true || body["probe_note"] != "" {
		t.Fatalf("the same door, its probe passed: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, dana, base, `{"run_by":"canyon"}`); code != 200 || body["run_by"] != "canyon" {
		t.Fatalf("back to canyon once the door passed: %d %s", code, lastBody)
	}
}

// A probe that can't finish, under canyon, refuses the save and changes nothing, even beside a webhook: a passing blip
// must never demote a probed door and stop every deploy. Under org, where nothing is refused, it saves a choice 1 not
// probed, D5's probe_unavailable sentence its why; the reason stays in the log (#2438).
func TestAProbeThatCannotFinishRefusesUnderCanyonAndSavesUnderOrg(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`); code != 200 {
		t.Fatalf("the webhook: %d %s", code, lastBody)
	}
	alb := `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain","domain":"apps.meridian.example"}`
	if code, saved, _ := do(t, c, http.MethodPut, base+doorPath, alb); code != 200 || saved["front_door"].(map[string]any)["probed"] != true {
		t.Fatalf("the door, probed: %d %s", code, lastBody)
	}
	var before string
	if err := pool.QueryRow(ctx, "SELECT to_jsonb(front_doors)::text FROM front_doors").Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.FailProbe.Store(true)
	if code, body, _ := do(t, c, http.MethodPut, base+doorPath, alb); code != 502 || body["error"] != "probe_unavailable" || body["message"] != probeUnavailable {
		t.Fatalf("under canyon, a probe that could not finish: %d %s", code, lastBody)
	}
	var after string
	if err := pool.QueryRow(ctx, "SELECT to_jsonb(front_doors)::text FROM front_doors").Scan(&after); err != nil || after != before {
		t.Fatalf("the probed door changed: %s, was %s (%v)", after, before, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	code, saved, _ := do(t, c, http.MethodPut, base+doorPath, alb)
	if code != 200 || saved["front_door"].(map[string]any)["probed"] != false || saved["probe_note"] != notEnforcingLine+" "+probeUnavailable {
		t.Fatalf("under org, a probe that could not finish: %d %s", code, lastBody)
	}
	if !strings.Contains(logs.String(), "its prober never reached") || count(t, ctx, pool, "SELECT count(*) FROM front_doors WHERE probed_at IS NULL AND probe_fix <> ''") != 1 {
		t.Fatalf("the reason is in the log, the row not probed: %s", logs.String())
	}
}

// Every save records whether its probe found NetworkPolicy enforced, whatever the certificate (#2507): a deploy under
// either run_by builds only behind a door that says so. Under org, beside a webhook, a plugin that ignores policy, one
// that lets a new pod out before its policy is in force (the probe's late fix) and a probe that could not finish are
// each saved not enforcing; enforced with the certificate missing is saved enforcing but not probed. The first save
// inserts the row and every later one, the same door, updates it: one that finds it enforced again moves the time it
// was seen, which db/migrations/0110's trigger never clears; another door, not enforced, is a fresh row seen nowhere.
// The answer's front_door says enforcing as the row does.
func TestEverySaveRecordsWhetherNetworkPolicyIsEnforced(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`); code != 200 {
		t.Fatalf("the webhook: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	alb := func(domain string) string {
		return `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain","domain":"` + domain + `"}`
	}
	for _, s := range []struct {
		name, domain    string
		probe           *executor.ProbeResult // nil: the probe could not finish
		probed, enforce bool
	}{
		{"enforced and ok", "apps.meridian.example", &executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"}, true, true},
		{"enforced and ok again", "apps.meridian.example", &executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"}, true, true},
		{"not enforced", "apps.meridian.example", &executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: "Turn enforcement on."}, false, false},
		{"enforced, the certificate missing", "apps.meridian.example", &executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "missing", Fix: "Cover the names."}, false, true},
		{"another door, not enforced", "apps2.meridian.example", &executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: "Turn enforcement on."}, false, false},
		{"a new pod out before its policy", "apps.meridian.example", &executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: "Apply a pod's policy before it starts."}, false, false},
		{"the probe could not finish", "apps.meridian.example", nil, false, false},
	} {
		fake.FailProbe.Store(s.probe == nil)
		if s.probe != nil {
			fake.ProbeAnswers(*s.probe)
		}
		var before *time.Time
		_ = pool.QueryRow(ctx, "SELECT network_policy_enforced_at FROM front_doors").Scan(&before) // none before the first save
		if code, saved, _ := do(t, c, http.MethodPut, base+doorPath, alb(s.domain)); code != 200 || saved["front_door"].(map[string]any)["probed"] != s.probed || saved["front_door"].(map[string]any)["enforcing"] != s.enforce {
			t.Fatalf("%s: %d %s", s.name, code, lastBody)
		}
		var seen *time.Time
		if err := pool.QueryRow(ctx, "SELECT network_policy_enforced_at FROM front_doors").Scan(&seen); err != nil || (seen != nil) != s.enforce || seen != nil && before != nil && !seen.After(*before) {
			t.Fatalf("%s: network_policy_enforced_at %v, was %v, want it set and moved: %v (%v)", s.name, seen, before, s.enforce, err)
		}
	}
}

// Under run_by: canyon a probe that finds NetworkPolicy not enforced refuses the save, and first clears the stored door's
// probed and enforcing, whatever door the PUT names (#2507): the cluster was just seen not to enforce it, so nothing runs
// or builds behind any door until a save passes. A refusal for the certificate alone, NetworkPolicy enforced, and a
// probe that could not finish change nothing; none writes an audit row.
func TestAProbeFindingNetworkPolicyUnenforcedClearsTheStoredDoor(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	alb := func(domain string) string {
		return `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain","domain":"` + domain + `"}`
	}
	if code, saved, _ := do(t, c, http.MethodPut, base+doorPath, alb("apps.meridian.example")); code != 200 || saved["front_door"].(map[string]any)["probed"] != true {
		t.Fatalf("the door, probed: %d %s", code, lastBody)
	}
	stored := func() string {
		t.Helper()
		var row string
		if err := pool.QueryRow(ctx, "SELECT to_jsonb(front_doors)::text FROM front_doors").Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := stored()
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "missing", Fix: "Cover the names."})
	if code, body, _ := do(t, c, http.MethodPut, base+doorPath, alb("apps2.meridian.example")); code != 409 || body["error"] != "probe_failed" {
		t.Fatalf("the certificate alone: %d %s", code, lastBody)
	}
	fake.FailProbe.Store(true)
	if code, _, _ := do(t, c, http.MethodPut, base+doorPath, alb("apps2.meridian.example")); code != 502 {
		t.Fatalf("a probe that could not finish: %d %s", code, lastBody)
	}
	if after := stored(); after != before {
		t.Fatalf("a refusal that did not find NetworkPolicy unenforced changed the door: %s, was %s", after, before)
	}
	fake.FailProbe.Store(false)
	fix := `This cluster does not enforce NetworkPolicy, so an app's port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: fix})
	if code, body, _ := do(t, c, http.MethodPut, base+doorPath, alb("apps2.meridian.example")); code != 409 || body["error"] != "probe_failed" || body["message"] != fix {
		t.Fatalf("another door on a cluster not enforcing: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM front_doors WHERE probed_at IS NULL AND network_policy_enforced_at IS NULL AND door->>'domain' = 'apps.meridian.example'"); n != 1 {
		t.Fatalf("the stored door is still probed or enforcing: %s", stored())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'"); n != 1 {
		t.Fatalf("%d set front door audit row(s): the refusal writes none", n)
	}
	if code, read, _ := do(t, c, http.MethodGet, base+doorPath, ""); code != 200 || read["front_door"].(map[string]any)["probed"] != false ||
		read["front_door"].(map[string]any)["enforcing"] != false || read["probe_note"] != notEnforcingLine+" "+fix {
		t.Fatalf("the read after: %d %s", code, lastBody)
	}
}

// A refusal that clears the stored door gives it that refusal's why (#2507, review round 2): beside a webhook under
// run_by: canyon, a choice 1 stored not probed for its certificate alone, then a choice 2 refused on a cluster not
// enforcing NetworkPolicy — the note names the switch to turn on, never the certificate's names it said before, and the
// server's log says the stored door was cleared.
func TestARefusalThatClearsTheDoorGivesItsOwnWhy(t *testing.T) {
	onKubernetes(t)
	pool, _ := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`); code != 200 {
		t.Fatalf("the webhook: %d %s", code, lastBody)
	}
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "missing", Fix: "Cover the names."})
	alb := `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain","domain":"apps.meridian.example"}`
	if code, saved, _ := do(t, c, http.MethodPut, base+doorPath, alb); code != 200 || saved["front_door"].(map[string]any)["probed"] != false ||
		saved["probe_note"] != "Not checked on this cluster: your platform can run apps behind this front door, but Canyon won't until a save passes the check. Cover the names." {
		t.Fatalf("a choice 1 beside a webhook, its certificate missing: %d %s", code, lastBody)
	}
	fix := `This cluster does not enforce NetworkPolicy, so an app's port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: fix})
	choice2 := `{"form":"app.env.domain","choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps","client_secret":"oidc-client-secret-not-real"}}`
	if code, body, _ := do(t, c, http.MethodPut, base+doorPath, choice2); code != 409 || body["error"] != "probe_failed" || body["message"] != fix {
		t.Fatalf("a choice 2 on a cluster not enforcing: %d %s", code, lastBody)
	}
	if code, read, _ := do(t, c, http.MethodGet, base+doorPath, ""); code != 200 || read["front_door"].(map[string]any)["enforcing"] != false || read["probe_note"] != notEnforcingLine+" "+fix {
		t.Fatalf("the cleared door's note: %d %s", code, lastBody)
	}
	if !strings.Contains(logs.String(), "the stored front door is no longer probed or enforcing") {
		t.Fatalf("the log says nothing of the cleared door: %s", logs.String())
	}
}
