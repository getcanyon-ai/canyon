package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	doorPath = "/api/v1/front-door"
	doorARN  = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef"
	doorOIDC = `"choice":1,"gateway":"oidc","issuer":"https://login.meridian.example","audience":"canyon-gateway","jwks_url":"https://login.meridian.example/keys","cookie":"_pomerium"`
)

// onKubernetes starts the next test server with CANYON_RUNTIME=kubernetes, its public URL on the fixture's host.
func onKubernetes(t *testing.T) {
	t.Helper()
	testKube, testPublic = true, "http://canyon.meridian.example"
	t.Cleanup(func() { testKube, testPublic = false, "http://localhost:7777" })
}

// doorFixture is contracts/testdata/front-door/meridian.json: one save told end to end.
func doorFixture(t *testing.T) (put string, probe executor.ProbeResult, refusal, get map[string]any) {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/front-door/meridian.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Put         json.RawMessage      `json:"put"`
		ProbeResult executor.ProbeResult `json:"probe_result"`
		Refusal     map[string]any       `json:"refusal"`
		Get         map[string]any       `json:"get"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return string(f.Put), f.ProbeResult, f.Refusal, f.Get
}

// The fixture's save, end to end: a member reads, and is refused the PUT before anything is asked; the
// admin's save the probe refuses answers the fixture's refusal and writes nothing; once the probe passes,
// the answer and every later read are the fixture's GET, with one audit row.
func TestTheFrontDoorSaveIsTheFixtures(t *testing.T) {
	withOIDC(t)
	testOIDC.issuer = "https://login.meridian.example" // the fixture's Canyon signs in where its front door does
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	servesHandOff(t) // the fixture's host lacks nothing hand-off needs
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	put, probe, refusal, get := doorFixture(t)
	if code, _, _ := do(t, carol, http.MethodGet, base+doorPath, ""); code != 200 || lastBody != `{"default_domain":"canyon.meridian.example","discovery_note":"","egress_note":"","front_door":null,"grants_note":"","hand_off_needs":[],"probe_note":"","runtime":"kubernetes","warning":""}` {
		t.Fatalf("a member's GET before any save: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carol, http.MethodPut, base+doorPath, put); code != 403 || lastBody != adminRequired || fake.Probes.Load() != 0 {
		t.Fatalf("a member's PUT: %d %s, %d probe(s)", code, lastBody, fake.Probes.Load())
	}
	fake.ProbeAnswers(probe)
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, put); code != 409 || !reflect.DeepEqual(body, refusal) {
		t.Fatalf("the save the probe refused: %d %s", code, lastBody)
	}
	if domain, form := fake.Probed(); domain != "canyon.meridian.example" || form != "app.env.domain" || count(t, ctx, pool, "SELECT count(*) FROM front_doors") != 0 {
		t.Fatalf("probed %s %s; a refused save writes nothing", domain, form)
	}
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok", Warning: get["warning"].(string)}) // saved, and warned of
	code, saved, _ := do(t, dana, http.MethodPut, base+doorPath, put)
	door, _ := saved["front_door"].(map[string]any)
	want := get["front_door"].(map[string]any)
	want["set_by"], want["set_at"] = map[string]any{"kind": "person", "subject": "dana@meridian.example", "display": "dana@meridian.example"}, door["set_at"]
	if at, err := time.Parse(time.RFC3339, door["set_at"].(string)); code != 200 || !reflect.DeepEqual(saved, get) || err != nil || len(door["set_at"].(string)) != 20 || time.Since(at) > time.Minute {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	answered := lastBody
	if code, _, _ := do(t, carol, http.MethodGet, base+doorPath, ""); code != 200 || lastBody != answered {
		t.Fatalf("a member reads what was saved: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'set front door' AND actor_subject = 'dana@meridian.example'").Scan(&detail); err != nil ||
		!reflect.DeepEqual(detail, map[string]any{"before": nil, "after": door, "secret_replaced": true}) || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'") != 1 {
		t.Fatalf("one audit row: %v (%v)", detail, err)
	}
}

// Each refusal, in the contract's order, is answered before the probe is asked and before anything is
// written; invalid_front_door names the first key it refuses and never a value. A domain omitted is the
// public URL's host, which has to be one a front door can carry.
func TestAFrontDoorSaveIsRefusedInOrderBeforeTheProbe(t *testing.T) {
	pool, ctx := migrated(t)
	docker, _ := start(t, pool, time.Hour)
	if code, body, _ := do(t, client(t), http.MethodPut, docker+doorPath, `{"form":"x"}`); code != 409 || body["error"] != "not_kubernetes" || body["message"] != onDocker {
		t.Fatalf("a Docker host's PUT: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, client(t), http.MethodGet, docker+doorPath, ""); code != 200 || lastBody != `{"default_domain":"localhost","discovery_note":"","egress_note":"","front_door":null,"grants_note":"","hand_off_needs":[],"probe_note":"","runtime":"docker","warning":""}` {
		t.Fatalf("a Docker host's GET: %d %s", code, lastBody)
	}
	onKubernetes(t)
	testPublic = "http://localhost:7777"
	onLocalhost, _ := start(t, pool, time.Hour)
	alb := `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain"}`
	if code, body, _ := do(t, client(t), http.MethodPut, onLocalhost+doorPath, alb); code != 400 || body["error"] != "invalid_domain" {
		t.Fatalf("localhost defaulted: %d %s", code, lastBody)
	}
	testPublic = "http://canyon.meridian.example"
	base, _ := start(t, pool, time.Hour)
	signin := func(fields string) string {
		return `{"form":"app.env.domain","choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"` + fields + `}}`
	}
	for _, tc := range []struct{ body, code, key string }{
		{`[]`, "invalid_front_door", ""},
		{`null`, "invalid_front_door", ""},
		{`{"choice":9}`, "invalid_form", ""},
		{`{"form":"app.domain","choice":9}`, "invalid_form", ""},
		{`{"form":"app.env.domain","domain":"prod.meridian.example","choice":9}`, "invalid_domain", ""},
		{`{"form":"app.env.domain","domain":"Meridian.example"}`, "invalid_domain", ""},
		{`{"form":"app.env.domain","domain":null}`, "invalid_domain", ""},
		{`{"form":"app.env.domain","domain":"` + strings.Repeat("a.", 95) + `io"}`, "invalid_domain", ""},
		{`{"form":"app.env.domain","extra":1,` + doorOIDC + `}`, "invalid_front_door", "extra"},
		{`{"form":"app.env.domain","choice":3}`, "invalid_front_door", "choice"},
		{`{"form":"app.env.domain","choice":1}`, "invalid_front_door", "gateway"},
		{`{"form":"app.env.domain",` + strings.Replace(doorOIDC, "https://login", "http://login", 1) + `}`, "invalid_front_door", "issuer"},
		{`{"form":"app.env.domain",` + strings.Replace(doorOIDC, `,"cookie":"_pomerium"`, "", 1) + `}`, "invalid_front_door", "cookie"},
		{`{"form":"app.env.domain","choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","issuer":"https://login.meridian.example"}`, "invalid_front_door", "issuer"},
		{strings.Replace(signin(`,"client_secret":"s"`), `"choice":2`, `"choice":2,"gateway":"oidc"`, 1), "invalid_front_door", "gateway"},
		{signin(""), "invalid_front_door", "signin.client_secret"},
		{signin(`,"client_secret":"s","scope":"all"`), "invalid_front_door", "signin.scope"},
	} {
		code, body, _ := do(t, client(t), http.MethodPut, base+doorPath, tc.body)
		msg, _ := body["message"].(string)
		if code != 400 || body["error"] != tc.code || msg == "" || tc.key != "" && !strings.HasPrefix(msg, tc.key+":") || strings.Contains(msg, "login.meridian.example") {
			t.Fatalf("%s: %d %s", tc.body, code, lastBody)
		}
	}
	if n := fake.Probes.Load(); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM front_doors")+count(t, ctx, pool, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatalf("a refused save asked the probe %d time(s) or wrote a row", n)
	}
}

// A probe that finds the cluster unenforced refuses the save with its fix verbatim; one that could not
// tell refuses it with D5's sentence, the reason in the log. Nothing is written either way.
func TestAProbeThatDoesNotPassRefusesTheSave(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	alb := `{"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `","form":"app-env.domain","domain":"apps.meridian.example"}`
	fix := `This cluster does not enforce NetworkPolicy, so an app's port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.`
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "not_enforced", Certificate: "ok", Fix: fix})
	if code, body, _ := do(t, client(t), http.MethodPut, base+doorPath, alb); code != 409 ||
		!reflect.DeepEqual(body, map[string]any{"error": "probe_failed", "message": fix, "network_policy": "not_enforced", "certificate": "ok"}) {
		t.Fatalf("an unenforced cluster: %d %s", code, lastBody)
	}
	if domain, form := fake.Probed(); domain != "apps.meridian.example" || form != "app-env.domain" {
		t.Fatalf("probed %s %s", domain, form)
	}
	fake.FailProbe.Store(true)
	if code, body, _ := do(t, client(t), http.MethodPut, base+doorPath, alb); code != 502 ||
		!reflect.DeepEqual(body, map[string]any{"error": "probe_unavailable", "message": probeUnavailable}) || !strings.Contains(logs.String(), "its prober never reached") {
		t.Fatalf("a probe that could not tell: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM front_doors") + count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 0 {
		t.Fatalf("a refused save wrote %d row(s)", n)
	}
}

// Choice 2's client secret is sealed, and never answered, logged or in an audit row. A save without one
// keeps it for the same client and refuses another; a save that changes nothing is probed and writes no
// row; a secret sent again is sealed anew, one row; choice 1 removes it.
func TestTheSignInSecretIsSealedAndEachChangeIsOneAuditRow(t *testing.T) {
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	const planted = "cs-front-door-9f3kq2x7" // the sign-in client's, which nothing may echo
	two := func(clientID, secretField string) string {
		return `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"` + clientID + `"` + secretField + `},"form":"app.env.domain"}`
	}
	save := func(body string) map[string]any {
		t.Helper()
		code, out, _ := do(t, client(t), http.MethodPut, base+doorPath, body)
		if code != 200 || strings.Contains(lastBody, planted) {
			t.Fatalf("%s: %d %s", body, code, lastBody)
		}
		return out
	}
	sealed := func() []byte {
		t.Helper()
		var keyRef int
		var sealedKey, sealedValue []byte
		if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_value FROM front_doors").Scan(&keyRef, &sealedKey, &sealedValue); err != nil {
			t.Fatal(err)
		}
		if opened, err := vaultOf(t, base).Open(keyRef, "front-door", "signin", "client_secret", sealedKey, sealedValue); err != nil || string(opened) != planted || bytes.Contains(sealedValue, []byte(planted)) {
			t.Fatalf("the sealed secret opens to %q (%v)", opened, err)
		}
		return sealedValue
	}
	first := save(two("canyon-apps", `,"client_secret":"`+planted+`"`))
	firstSealed := sealed()
	if kept := save(two("canyon-apps", "")); !reflect.DeepEqual(kept, first) || fake.Probes.Load() != 2 || !bytes.Equal(sealed(), firstSealed) {
		t.Fatalf("a save that changes nothing: %v after %d probe(s)", kept, fake.Probes.Load())
	}
	save(two("canyon-apps", `,"client_secret":"`+planted+`"`))
	if bytes.Equal(sealed(), firstSealed) {
		t.Fatal("a secret sent again is sealed anew")
	}
	if code, body, _ := do(t, client(t), http.MethodPut, base+doorPath, two("other-client", "")); code != 400 || body["message"] != noSecretStored {
		t.Fatalf("another client without a secret: %d %s", code, lastBody)
	}
	save(`{"form":"app.env.domain",` + doorOIDC + `}`)
	if n := count(t, ctx, pool, "SELECT count(*) FROM front_doors WHERE key_ref IS NULL AND sealed_key IS NULL AND sealed_value IS NULL"); n != 1 {
		t.Fatal("choice 1 keeps no secret")
	}
	if code, body, _ := do(t, client(t), http.MethodPut, base+doorPath, two("canyon-apps", "")); code != 400 || body["message"] != noSecretStored {
		t.Fatalf("the secret went with choice 2: %d %s", code, lastBody)
	}
	rows, _ := pool.Query(ctx, "SELECT detail FROM audit_events WHERE action = 'set front door' ORDER BY ts")
	details, err := pgx.CollectRows(rows, pgx.RowTo[map[string]any])
	if err != nil || len(details) != 3 || details[0]["secret_replaced"] != true || details[1]["secret_replaced"] != true || details[2]["secret_replaced"] != false ||
		!reflect.DeepEqual(details[1]["before"], first["front_door"]) || details[2]["after"].(map[string]any)["gateway"] != "oidc" ||
		strings.Contains(string(mustJSON(details)), planted) || strings.Contains(logs.String(), planted) {
		t.Fatalf("three changes, three rows, never the secret: %s", mustJSON(details))
	}
}

// An admin's save of a front door off Canyon's sign-in's issuer answers the line saying person grants won't
// apply there, and so does every read after; a save on Canyon's issuer answers none; an alb's always answers
// when they do. None refuses the save, and a member's is refused before anything is asked (#2299).
func TestAFrontDoorSaveSaysWhetherPersonGrantsApply(t *testing.T) {
	withOIDC(t) // Canyon signs in at login.example.com/v2.0
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"})
	g := grantsNotesFixture(t)
	other, alb := g.Cases[1].GrantsNote, g.Cases[4].GrantsNote
	for _, c := range []struct{ name, door, want string }{
		{"an oidc gateway on another issuer", doorOIDC, other},
		{"an oidc gateway on Canyon's", `"choice":1,"gateway":"oidc","issuer":"https://login.example.com/v2.0","audience":"canyon-gateway","jwks_url":"https://login.example.com/keys","cookie":"_pomerium"`, ""},
		{"choice 2 on another issuer", `"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps","client_secret":"oidc-client-secret-not-real"}`, other},
		{"an alb", `"choice":1,"gateway":"alb","alb_arn":"` + doorARN + `"`, alb},
	} {
		if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+c.door+`}`); code != 200 || body["grants_note"] != c.want {
			t.Fatalf("%s, saved: %d %s", c.name, code, lastBody)
		}
		if code, body, _ := do(t, carol, http.MethodGet, base+doorPath, ""); code != 200 || body["grants_note"] != c.want {
			t.Fatalf("%s, read after: %d %s", c.name, code, lastBody)
		}
	}
	if code, _, _ := do(t, carol, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+doorOIDC+`}`); code != 403 || lastBody != adminRequired || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'") != 4 {
		t.Fatalf("a member's save: %d %s", code, lastBody)
	}
}

// grantsNotes is contracts/testdata/front-door/grants-notes.json: doors, Canyon's sign-in's issuer, and the
// grants_note each answers — D5's lines as the contract pins them, never this package's constants.
type grantsNotes struct {
	SigninIssuer string `json:"signin_issuer"`
	Cases        []struct {
		Door       hostDoor `json:"door"`
		GrantsNote string   `json:"grants_note"`
	} `json:"cases"`
}

func grantsNotesFixture(t *testing.T) grantsNotes {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/front-door/grants-notes.json")
	var g grantsNotes
	if err == nil {
		err = json.Unmarshal(raw, &g)
	}
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A front door off Canyon's sign-in's issuer says person grants won't apply there; an alb always says when
// they do; Canyon's own issuer, or sign-in off, says nothing — each as the contract's golden says (#2299).
func TestTheFrontDoorSaysWherePersonGrantsApply(t *testing.T) {
	g := grantsNotesFixture(t)
	for i, c := range g.Cases {
		if got := grantsNote(c.Door, g.SigninIssuer); got != c.GrantsNote {
			t.Errorf("case %d: %q, want %q", i, got, c.GrantsNote)
		}
		if got := grantsNote(c.Door, ""); got != "" {
			t.Errorf("case %d with sign-in off: %q", i, got)
		}
	}
}

func TestTheRuntimeIsDockerUnlessKubernetes(t *testing.T) {
	for in, want := range map[string]bool{"": false, "docker": false, "kubernetes": true} {
		if got, err := kubernetesOf(in); got != want || err != nil {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	if _, err := kubernetesOf("k8s"); err == nil {
		t.Fatal("an unknown runtime fails the boot")
	}
}

// Under run_by: org choice 2 is refused before anything is written or probed (contracts/routes.md,
// the front-door row; design/HAND-OFF.md §6): a choice-2 door would ship to a sidecar with no
// oauth2-proxy secrets. A choice 1 is probed, and the probe never refuses it: a webhook is saved
// whenever run_by is org, so one that fails is stored not probed (#2438).
func TestUnderOrgTheProbeNeverRefusesAChoice1AndChoice2IsRefused(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	withWebhookRow(t, ctx, pool)
	base, _ := start(t, pool, time.Hour)
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "missing", Fix: "The certificate at canyon-probe.preview.canyon.meridian.example does not cover it. Add *.preview.canyon.meridian.example and *.prod.canyon.meridian.example to the certificate and save again."})
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	choice2 := `{"form":"app.env.domain","choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps","client_secret":"oidc-client-secret-not-real"}}`
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, choice2); code != 409 || body["error"] != "signin_unsupported" || body["message"] != signinUnsupported {
		t.Fatalf("choice 2 under org: %d %s", code, lastBody)
	}
	if rows, audits, probes := count(t, ctx, pool, "SELECT count(*) FROM front_doors"), count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'"), fake.Probes.Load(); rows != 0 || audits != 0 || probes != 0 {
		t.Fatalf("the refusal wrote or probed: %d row(s), %d audit row(s), %d probe(s)", rows, audits, probes)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+doorOIDC+`}`); code != 200 || body["front_door"].(map[string]any)["probed"] != false {
		t.Fatalf("choice 1 under org, its probe failed: %d %s", code, lastBody)
	}
	if rows, probes := count(t, ctx, pool, "SELECT count(*) FROM front_doors WHERE probed_at IS NULL"), fake.Probes.Load(); rows != 1 || probes != 1 {
		t.Fatalf("choice 1 is probed and saved not probed: %d row(s), %d probe(s)", rows, probes)
	}
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"})
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+doorOIDC+`}`); code != 200 || body["front_door"].(map[string]any)["probed"] != true || body["probe_note"] != "" {
		t.Fatalf("the same door under org, its probe passed: %d %s", code, lastBody)
	}
}

// The warning is the latest probe's: once the operator closes the node ports, saving the same door
// again clears it, with no audit row, since the door did not change (#1435).
func TestSavingTheSameDoorAgainRefreshesTheWarning(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	put, _, _, get := doorFixture(t)
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok", Warning: get["warning"].(string)})
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, put); code != 200 || body["warning"] != get["warning"] {
		t.Fatalf("the first save: %d %s", code, lastBody)
	}
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"})
	if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"},"form":"app.env.domain"}`); code != 200 || body["warning"] != "" {
		t.Fatalf("the same door again, the ports closed: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodGet, base+doorPath, ""); code != 200 || body["warning"] != "" || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set front door'") != 1 {
		t.Fatalf("read after: %d %s", code, lastBody)
	}
}

// gatewayNote is the front door's egress_note at a sign-in served through the cluster's own gateway, as the contract's
// schema pins it — D5's line, never this package's constant.
func gatewayNote(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/schema/front-door.schema.json")
	var schema struct {
		Definitions struct {
			Get struct {
				Properties struct {
					EgressNote struct {
						Enum []string `json:"enum"`
					} `json:"egress_note"`
				} `json:"properties"`
			} `json:"get"`
		} `json:"definitions"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &schema)
	}
	if enum := schema.Definitions.Get.Properties.EgressNote.Enum; err != nil || len(enum) != 2 {
		t.Fatalf("front-door.schema.json's egress_note: %v %v", enum, err)
	}
	return schema.Definitions.Get.Properties.EgressNote.Enum[1]
}

// With a sign-in served through the cluster's own gateway (CANYON_KUBE_GATEWAY_SIGNIN), an admin's save of a front door that
// signs in there while the host's via is canyon is taken — refusing it could lock admins out — and answers D5's
// line saying routed apps can't reach it, as does every read after; under via off, or at another provider, it
// answers none. A member's save is refused before anything is asked (#2316, the owner's decision).
func TestAFrontDoorBehindTheGatewaySaysRoutedAppsCantReachIt(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	testBehind = "id.canyon.meridian.example"
	t.Cleanup(func() { testBehind = "" })
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	fake.ProbeAnswers(executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"})
	note := gatewayNote(t)
	behind := `"choice":2,"signin":{"issuer":"https://ID.canyon.meridian.example","client_id":"canyon-apps","client_secret":"oidc-client-secret-not-real"}`
	for _, c := range []struct{ name, via, door, want string }{
		{"the sign-in behind the gateway under via canyon", "canyon", behind, note},
		{"the sign-in behind the gateway under via off", "off", behind, ""},
		{"an oidc gateway whose key set is behind the gateway", "canyon", `"choice":1,"gateway":"oidc","issuer":"https://login.meridian.example","audience":"canyon-gateway","jwks_url":"https://id.canyon.meridian.example/keys","cookie":"_pomerium"`, note},
		{"another provider under via canyon", "canyon", doorOIDC, ""},
	} {
		if _, err := pool.Exec(ctx, "UPDATE egress_settings SET via = $1", c.via); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := do(t, dana, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+c.door+`}`); code != 200 || body["egress_note"] != c.want {
			t.Fatalf("%s, saved: %d %s", c.name, code, lastBody)
		}
		if code, body, _ := do(t, carol, http.MethodGet, base+doorPath, ""); code != 200 || body["egress_note"] != c.want {
			t.Fatalf("%s, read after: %d %s", c.name, code, lastBody)
		}
	}
	if code, _, _ := do(t, carol, http.MethodPut, base+doorPath, `{"form":"app.env.domain",`+behind+`}`); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's save: %d %s", code, lastBody)
	}
}

// signsInAt is the gateway sign-in's rule: choice 2's signin.issuer, choice 1 oidc's issuer or jwks_url, on the
// host, compared without case; never an alb, whose issuer Canyon does not hold.
func TestSignsInAtComparesTheDoorsSigninHosts(t *testing.T) {
	const host = "id.canyon.meridian.example"
	for _, c := range []struct {
		door hostDoor
		want bool
	}{
		{hostDoor{Choice: 2, Signin: signin{Issuer: "https://ID.canyon.meridian.example"}}, true},
		{hostDoor{Choice: 2, Signin: signin{Issuer: "https://id.canyon.meridian.example:443/realms/apps"}}, true},
		{hostDoor{Choice: 2, Signin: signin{Issuer: "https://login.meridian.example"}}, false},
		{hostDoor{Choice: 2, Signin: signin{Issuer: "https://id.canyon.meridian.example.evil.example"}}, false},
		{hostDoor{Choice: 1, Gateway: "oidc", Issuer: "https://id.canyon.meridian.example", JWKSURL: "https://keys.meridian.example/jwks"}, true},
		{hostDoor{Choice: 1, Gateway: "oidc", Issuer: "https://login.meridian.example", JWKSURL: "https://id.canyon.meridian.example/jwks"}, true},
		{hostDoor{Choice: 1, Gateway: "alb", ALBARN: doorARN}, false},
	} {
		if got := signsInAt(c.door, host); got != c.want {
			t.Errorf("%+v: %v, want %v", c.door, got, c.want)
		}
	}
}

// The front door saved again with a new client secret, the door otherwise the same, is a new save: the next
// deploy's row records it and its fingerprint moves, never holding the secret; a save that changes nothing
// keeps the fingerprint (contracts/routes.md, Patch runs, step 5).
func TestAFrontDoorSavedAgainMovesTheRunConfig(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	cleanGate(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	const first, second = "cs-first-3b7d91", "cs-second-8e1a42" // the sign-in client's, which the row never holds
	save := func(secretField string) {
		t.Helper()
		body := `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"` + secretField + `},"domain":"canyon.meridian.example","form":"app.env.domain"}`
		if code, _, _ := do(t, dana, http.MethodPut, base+doorPath, body); code != 200 {
			t.Fatalf("the save: %d %s", code, lastBody)
		}
	}
	deploy := func() string {
		t.Helper()
		_, started, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
		if run := waitVerdict(t, olive, base, runOf(started)["id"].(string)); run["state"] != "clean" {
			t.Fatalf("the deploy: %s", lastBody)
		}
		var fingerprint, stored string
		if err := pool.QueryRow(ctx, "SELECT run_config_sha256, run_config::text FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'").Scan(&fingerprint, &stored); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stored, first) || strings.Contains(stored, second) || !strings.Contains(stored, "canyon.meridian.example") {
			t.Fatalf("the row's run-config %s: want the door as saved, never its secret", stored)
		}
		return fingerprint
	}
	save(`,"client_secret":"` + first + `"`)
	was := deploy()
	save("") // the same door, the stored secret kept: no new save
	if kept := deploy(); kept != was {
		t.Fatalf("a save that changes nothing moved the fingerprint: %s, want %s", kept, was)
	}
	save(`,"client_secret":"` + second + `"`)
	if moved := deploy(); moved == was {
		t.Fatalf("a new client secret kept the fingerprint %s", moved)
	}
}

// The minute loop's own reads of the database that fail are logged as an issuer's fetches are: the first, then at
// most one an hour, never a line a minute (#2344's security review). A pool whose server answers nothing fails every one.
func TestTheLoopLogsADatabaseFailureAtMostHourly(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://canyon@127.0.0.1:1/canyon?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var logs syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	(&app{pool: pool}).recordDiscovery(ctx, log.New(&logs, "", 0), 10*time.Millisecond)
	if n := strings.Count(logs.String(), "\n"); n != 2 {
		t.Errorf("%d line(s) logged over 300 ms of failing passes, want one for the replaced doors and one for the doors in use:\n%s", n, logs.String())
	}
}

// The hosts a choice 2's discovery names, as its save records them: the token, user-info and key set endpoints'
// hosts, canonical, sorted, each once, an IP literal none — and none at all, never nil, when that is every one. A
// document that cannot be read — none, another issuer's, a user-info endpoint that is not https, one past
// discoveryCap, a redirect, which the production client never follows — is an error. An issuer whose own host is
// no destination — an IP literal, a single label — goes direct: it is never fetched.
func TestDiscoveredHostsAreTheEndpointsCanonical(t *testing.T) {
	docs := idpAt{
		"https://accounts.google.com": googleDiscovery,
		"https://login.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://Keys.Meridian.Example./token",` +
			`"userinfo_endpoint":"https://10.0.0.9/me","jwks_uri":"https://keys.meridian.example/jwks"}`,
		"https://ip.meridian.example":    `{"issuer":"https://ip.meridian.example","authorization_endpoint":"https://ip.meridian.example/auth","token_endpoint":"https://10.0.0.9/token","jwks_uri":"https://[::1]/keys"}`,
		"https://plain.meridian.example": `{"issuer":"https://plain.meridian.example","authorization_endpoint":"https://plain.meridian.example/auth","token_endpoint":"https://plain.meridian.example/token","userinfo_endpoint":"http://plain.meridian.example/me","jwks_uri":"https://plain.meridian.example/keys"}`,
		"https://other.meridian.example": `{"issuer":"https://login.meridian.example","authorization_endpoint":"https://login.meridian.example/auth","token_endpoint":"https://login.meridian.example/token","jwks_uri":"https://login.meridian.example/keys"}`,
		"https://moved.meridian.example": `{"issuer":"https://hop.meridian.example","authorization_endpoint":"https://hop.meridian.example/auth","token_endpoint":"https://hop.meridian.example/token","jwks_uri":"https://hop.meridian.example/keys"}`,
		"https://big.meridian.example": `{"issuer":"https://big.meridian.example","authorization_endpoint":"https://big.meridian.example/auth","token_endpoint":"https://big.meridian.example/token","jwks_uri":"https://big.meridian.example/keys"` +
			strings.Repeat(" ", discoveryCap) + `}`,
	}
	var asked []string
	a := &app{discovery: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.Host)
		if r.URL.Host == "hop.meridian.example" { // the document is elsewhere: a client that follows it reads hop's issuer there
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://moved.meridian.example/.well-known/openid-configuration"}}, Body: http.NoBody, Request: r}, nil
		}
		return docs.RoundTrip(r)
	})}
	for issuer, want := range map[string][]string{
		"https://accounts.google.com":    {"oauth2.googleapis.com", "openidconnect.googleapis.com", "www.googleapis.com"},
		"https://login.meridian.example": {"keys.meridian.example"},
		"https://ip.meridian.example":    {},
	} {
		if got, err := a.discoveredHosts(context.Background(), issuer); err != nil || got == nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q (%v), want %q", issuer, got, err, want)
		}
	}
	for _, issuer := range []string{"https://plain.meridian.example", "https://other.meridian.example", "https://none.meridian.example", "https://hop.meridian.example", "https://big.meridian.example"} {
		if got, err := a.discoveredHosts(context.Background(), issuer); err == nil || errors.Is(err, errGoesDirect) {
			t.Errorf("%s: %q (%v), want an error reading it", issuer, got, err)
		}
	}
	asked = nil
	for _, issuer := range []string{"https://10.0.0.7/realms/apps", "https://keycloak:8443/realms/apps", "https://[fd00::7]/realms/apps"} {
		if got, err := a.discoveredHosts(context.Background(), issuer); !errors.Is(err, errGoesDirect) {
			t.Errorf("%s: %q (%v), want errGoesDirect", issuer, got, err)
		}
	}
	if len(asked) != 0 {
		t.Errorf("fetched %v for a provider that goes direct", asked)
	}
}
