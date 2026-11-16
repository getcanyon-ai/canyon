//go:build !noee

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
	"github.com/getcanyon-ai/canyon/vault"
)

// testSigner is the key the tests' licences are signed with, made at test time: no private key is ever committed.
var testSigner = func() ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}()

// testExpiry is when the test servers' default licence expires: well after any test ends.
var testExpiry = time.Now().UTC().Truncate(time.Second).AddDate(10, 0, 0)

// testLicenceEnv is the next test server's CANYON_LICENSE: by default a licence in force for every feature, so the
// tests of what an Enterprise feature does run licensed; the licence's own tests change it.
var testLicenceEnv = signedLicence(testExpiry, licence.Features...)

// In a build with Canyon Enterprise each test server has it, as the release does: its licence trusts the test key
// alone, and each feature's wiring sets its hook from it.
func init() {
	testEE = func(keys *vault.Vault) eeHooks {
		h := eeHooks{licence: licence.New([]ed25519.PublicKey{testSigner.Public().(ed25519.PublicKey)}, testLicenceEnv)}
		for _, wire := range eeFeatures {
			wire(&h, keys)
		}
		return h
	}
}

// signedLicence is a licence for Meridian Insurance, 50 seats, expiring at expires, for these features.
func signedLicence(expires time.Time, names ...string) string {
	text, err := licence.Sign(testSigner, licence.Payload{Customer: "Meridian Insurance", Issued: expires.AddDate(-1, 0, 0), Expires: expires, Seats: 50, Features: names})
	if err != nil {
		panic(err)
	}
	return string(text)
}

// withLicenceEnv starts the next test servers with CANYON_LICENSE set to env, "" for unset, until the test ends.
func withLicenceEnv(t *testing.T, env string) {
	t.Helper()
	prior := testLicenceEnv
	testLicenceEnv = env
	t.Cleanup(func() { testLicenceEnv = prior })
}

// licensed is the licensing of the server at base.
func licensed(t *testing.T, base string) *licence.Licensing {
	t.Helper()
	return appOf(t, base).ee.licence.(*licence.Licensing)
}

// licenceAt sets the licence's clock of the server at base.
func licenceAt(t *testing.T, base string, now time.Time) {
	t.Helper()
	licensed(t, base).Now = func() time.Time { return now }
}

// A licence that lapses is an admin's need: their fleet says so, a contributor's doesn't (THE FAIL-CLOSED RULE,
// #2879; what each feature keeps doing once it lapses is ee_<feature>_test.go's).
func TestALapsedLicenceIsAnAdminsNeed(t *testing.T) {
	h := outboundHost(t)
	licenceAt(t, h.base, testExpiry.Add(licence.Grace+time.Hour)) // lapsed: an hour past grace
	fleet := contractSchema(t, "fleet.schema.json")
	if code, _, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || !strings.Contains(lastBody, `"kind":"licence"`) || !strings.Contains(lastBody, `"licence":"lapsed"`) {
		t.Errorf("lapsed: the admin's fleet: %d %s, want the licence among org_needs", code, lastBody)
	} else {
		validJSON(t, fleet, lastBody)
	}
	if code, page, _ := do(t, h.carl, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || len(page["org_needs"].([]any)) != 0 {
		t.Errorf("lapsed: a contributor's fleet carries %v, want no org need", page["org_needs"])
	}
}

// A licence unlocks only the features it names, and only while it is in force: valid, then grace for 30 days,
// then lapsed. A feature this server doesn't gate is shown and unlocks nothing (what each feature's routes answer:
// ee_<feature>_test.go).
func TestALicenceUnlocksWhatItNamesWhileInForce(t *testing.T) {
	withLicenceEnv(t, signedLicence(testExpiry, "group_sync", "audit_export"))
	h := outboundHost(t)
	admin := contractSchema(t, "licence.schema.json#/definitions/state")
	for _, c := range []struct {
		at     time.Time
		state  string
		allows string
	}{
		{testExpiry.Add(-time.Hour), "valid", `["group_sync"]`},
		{testExpiry.Add(time.Hour), "grace", `["group_sync"]`},
		{testExpiry.Add(licence.Grace), "lapsed", `[]`},
	} {
		licenceAt(t, h.base, c.at)
		code, body, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/licence", "")
		if code != 200 || body["state"] != c.state || !strings.Contains(lastBody, `"allows":`+c.allows) || !strings.Contains(lastBody, `"features":["group_sync","audit_export"]`) ||
			body["source"] != "env" || body["customer"] != "Meridian Insurance" || body["grace_until"] != stamp(testExpiry.Add(licence.Grace)) {
			t.Errorf("at %s: %d %s, want %s allowing %s", c.at, code, lastBody, c.state, c.allows)
		}
		validJSON(t, admin, lastBody)
		code, _, _ = do(t, h.carl, http.MethodGet, h.base+"/api/v1/licence", "")
		if want := `{"state":"` + c.state + `","allows":` + c.allows + `}`; code != 200 || lastBody != want {
			t.Errorf("at %s, a member's read: %d %s, want %s", c.at, code, lastBody, want)
		}
		validJSON(t, admin, lastBody)
	}
}

// One preferences PUT that turns patching or rebuilds to ask and sets another groups claim adds two Enterprise
// features, and needs a licence for each: a licence naming one of them unlocks only that one, so the PUT is refused for
// the other before anything is written, and a licence naming both writes both.
func TestAPreferencesEditNeedsALicenceForEveryFeatureItAdds(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, c := range []struct {
		what, patching string // the licence, and patching as stored before the PUT
		licence        []string
		body, want     string
	}{
		{"patching on and another claim, under a licence naming patching", "off", []string{"platform_patching"}, `{"patching":"ask","groups_claim":"roles"}`, groupsRequired},
		{"rebuilds on and another claim, under a licence naming patching", "ask", []string{"platform_patching"}, `{"rebuilds":"ask","groups_claim":"roles"}`, groupsRequired},
		{"patching on and another claim, under a licence naming groups", "off", []string{"group_sync"}, `{"patching":"ask","groups_claim":"roles"}`, patchingRequired},
		{"rebuilds on and another claim, under a licence naming groups", "ask", []string{"group_sync"}, `{"rebuilds":"ask","groups_claim":"roles"}`, patchingRequired},
		{"patching on and another claim, under no licence", "off", nil, `{"patching":"ask","groups_claim":"roles"}`, patchingRequired},
	} {
		if _, err := pool.Exec(ctx, "UPDATE org_preferences SET patching = $1, rebuilds = 'off', groups_claim = 'groups'", c.patching); err != nil {
			t.Fatal(err)
		}
		licensed(t, base).Env = ""
		if c.licence != nil {
			licensed(t, base).Env = signedLicence(testExpiry, c.licence...)
		}
		if code, _ := putPreferences(t, dana, base, c.body); code != 409 || lastBody != c.want {
			t.Errorf("%s: %d %s, want 409 %s", c.what, code, lastBody, c.want)
		}
		var patching, rebuilds, claim string
		if err := pool.QueryRow(ctx, "SELECT patching, rebuilds, groups_claim FROM org_preferences").Scan(&patching, &rebuilds, &claim); err != nil || patching != c.patching || rebuilds != "off" || claim != "groups" {
			t.Errorf("%s: wrote patching %q, rebuilds %q, groups claim %q, want nothing written: %v", c.what, patching, rebuilds, claim, err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET patching = 'off', rebuilds = 'off', groups_claim = 'groups'"); err != nil {
		t.Fatal(err)
	}
	licensed(t, base).Env = signedLicence(testExpiry, "platform_patching", "group_sync")
	if code, prefs := putPreferences(t, dana, base, `{"patching":"ask","rebuilds":"ask","groups_claim":"roles"}`); code != 200 || prefs["patching"] != "ask" || prefs["rebuilds"] != "ask" || prefs["groups_claim"] != "roles" {
		t.Errorf("patching, rebuilds and another claim under a licence naming both: %d %s, want all three written", code, lastBody)
	}
}

// What is not a licence Canyon signed is invalid, and unlocks nothing: a text that is no licence file, a path the
// server can't read, and a licence another key signed.
func TestAnInvalidLicenceUnlocksNothingAndSaysWhy(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	foreign, err := licence.Sign(other, licence.Payload{Customer: "Meridian Insurance", Issued: time.Now().UTC().Truncate(time.Second), Expires: testExpiry, Seats: 5, Features: licence.Features})
	if err != nil {
		t.Fatal(err)
	}
	onDisk := filepath.Join(t.TempDir(), "canyon.licence")
	if err := os.WriteFile(onDisk, []byte(signedLicence(testExpiry, "outbound_keys")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	for _, c := range []struct{ env, want string }{
		{"not a licence", `{"state":"invalid","allows":[],"source":"env","problem":"unreadable"}`},
		{filepath.Join(t.TempDir(), "missing.licence"), `{"state":"invalid","allows":[],"source":"env","problem":"unreadable"}`},
		{string(foreign), `{"state":"invalid","allows":[],"source":"env","problem":"unsigned"}`},
		{onDisk, `"state":"valid","allows":["outbound_keys"],"source":"env"`},
	} {
		licensed(t, base).Env = c.env
		if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/licence", ""); code != 200 || !strings.Contains(lastBody, c.want) {
			t.Errorf("CANYON_LICENSE %.40q: %d %s, want %s", c.env, code, lastBody, c.want)
		}
	}
	licensed(t, base).Env = "not a licence"
	if code, page, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 || !strings.Contains(lastBody, `"kind":"licence"`) || !strings.Contains(lastBody, `"licence":"invalid"`) {
		t.Errorf("an invalid licence on the admin's fleet: %d %v", code, page["org_needs"])
	}
}

// Setting the licence is an admin's, a person's in the dashboard (design/PERMISSIONS.md, Licence): a member is 403
// admin_required, an agent under the admin 403 person_required in the licence's own words, the admin's CLI session
// 403 dashboard_required; what no compiled key verifies is 400 and stores
// nothing; one that verifies is stored, audited, and read by every member as whether it unlocks anything. While
// CANYON_LICENSE is set no PUT changes it.
func TestTheLicenceIsAnAdminsToSetInTheDashboard(t *testing.T) {
	withLicenceEnv(t, "")
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	agent := signedIn(t, ctx, pool, base, "sub-dana-agent", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = 'agent' WHERE actor_subject = 'sub-dana-agent'"); err != nil {
		t.Fatal(err)
	}
	at := base + "/api/v1/licence"
	body := func(text string) string {
		raw, _ := json.Marshal(map[string]string{"licence": text})
		return string(raw)
	}
	for who, c := range map[string]*http.Client{"the admin": dana, "a member": carol} {
		if code, _, _ := do(t, c, http.MethodGet, at, ""); code != 200 || lastBody != `{"state":"none","allows":[]}` {
			t.Errorf("%s reads no licence: %d %s", who, code, lastBody)
		}
	}
	good := signedLicence(testExpiry, "outbound_keys", "group_sync")
	if code, _, _ := do(t, carol, http.MethodPut, at, body(good)); code != 403 || lastBody != adminRequired {
		t.Errorf("a member sets it: %d %s, want 403 admin_required", code, lastBody)
	}
	const personRequired = `{"error":"person_required","message":"A person sets Canyon's licence, in the dashboard: Settings › Licence."}`
	if code, _, _ := do(t, agent, http.MethodPut, at, body(good)); code != 403 || lastBody != personRequired {
		t.Errorf("an agent under the admin sets it: %d %s, want 403 %s", code, lastBody, personRequired)
	}
	if code, _, _ := do(t, danaCLI, http.MethodPut, at, body(good)); code != 403 || lastBody != dashboardRequired {
		t.Errorf("the admin's CLI session sets it: %d %s, want 403 dashboard_required", code, lastBody)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	foreign, _ := licence.Sign(other, licence.Payload{Customer: "Meridian Insurance", Issued: time.Now().UTC().Truncate(time.Second), Expires: testExpiry, Seats: 5, Features: licence.Features})
	const invalid = `{"error":"invalid_licence","message":"This isn't a Canyon licence, or Canyon didn't sign it."}`
	for _, bad := range []string{"not a licence", string(foreign)} {
		if code, _, _ := do(t, dana, http.MethodPut, at, body(bad)); code != 400 || lastBody != invalid {
			t.Errorf("%.30q: %d %s, want 400 %s", bad, code, lastBody, invalid)
		}
	}
	for _, bad := range []string{`{}`, `{"licence":""}`, `{"licence":7}`} {
		if code, _, _ := do(t, dana, http.MethodPut, at, bad); code != 400 {
			t.Errorf("%s: %d %s, want 400", bad, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM licence"); n != 0 {
		t.Fatalf("%d licences stored by refused PUTs", n)
	}
	code, out, _ := do(t, dana, http.MethodPut, at, body("\n"+good+"\n"))
	if code != 200 || out["state"] != "valid" || out["source"] != "dashboard" || out["seats"] != float64(50) || !strings.Contains(lastBody, `"allows":["outbound_keys","group_sync"]`) {
		t.Fatalf("the admin sets it: %d %s", code, lastBody)
	}
	validJSON(t, contractSchema(t, "licence.schema.json#/definitions/state"), lastBody)
	if code, _, _ := do(t, carol, http.MethodGet, at, ""); code != 200 || lastBody != `{"state":"valid","allows":["outbound_keys","group_sync"]}` {
		t.Errorf("a member reads it: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/licence' AND detail->>'customer' = 'Meridian Insurance'`); n != 1 {
		t.Errorf("%d audit rows for one licence set", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'"); n != 3 {
		t.Errorf("%d denied rows for the three 403s", n)
	}

	licensed(t, base).Env = good
	if code, _, _ := do(t, dana, http.MethodPut, at, body(good)); code != 409 || lastBody != `{"error":"set_by_operator","message":"CANYON_LICENSE sets this server's licence, so it can't be changed in the dashboard."}` {
		t.Errorf("a PUT while CANYON_LICENSE is set: %d %s", code, lastBody)
	}
}

// Patching at upgrade (contracts/routes.md, Licence; #2881's review): what an install runs when it upgrades
// keeps running, and a new one starts off. At start, a host in setup with no app and no licence naming
// platform_patching writes patching off, and says once that CANYON_PATCHING turns nothing on; one with such a
// licence, or one that already has an app, keeps patching as it is, whatever its licence.
func TestANewInstallStartsPatchingOffWithoutALicence(t *testing.T) {
	const said = "canyon-server: CANYON_PATCHING is on, but Canyon patches nothing: patching is off in Settings › App hosting, and turning it on needs a licence, which an admin adds in Settings › Licence"
	for _, c := range []struct {
		name          string
		env           string
		app, off, log bool // off: an admin turned patching off before this start
		want          string
	}{
		{"a new install with no licence", "", false, false, true, "off"},
		{"a new install with a licence", signedLicence(testExpiry, "platform_patching"), false, false, false, "ask"},
		{"an upgraded install with an app and no licence", "", true, false, false, "ask"},
		{"an install whose admin turned patching off, with no licence", "", true, true, true, "off"},
		{"a new install whose licence has lapsed", signedLicence(time.Now().UTC().Truncate(time.Second).Add(-licence.Grace-time.Hour), "platform_patching"), false, false, true, "off"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withLicenceEnv(t, c.env)
			withOIDC(t)
			pool, ctx := migrated(t)
			base, _ := start(t, pool, time.Hour)
			if c.app {
				seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false))
			}
			inSetup(t, ctx, pool)
			if c.off {
				if _, err := pool.Exec(ctx, "UPDATE org_preferences SET patching = 'off'"); err != nil {
					t.Fatal(err)
				}
			}
			var logs strings.Builder
			for range 2 { // every start runs it: the second finds the first's work done and writes no second row
				if err := appOf(t, base).patchingAtStart(ctx, true, log.New(&logs, "", 0)); err != nil {
					t.Fatal(err)
				}
			}
			rows := map[bool]int{true: 1}[c.want == "off" && !c.off] // Canyon's own write alone is audited here
			if n := count(t, ctx, pool, `SELECT count(*) FROM audit_events WHERE action = 'patching off at start' AND actor_kind = 'canyon' AND detail = '{"patching": {"after": "off", "before": "ask"}}'`); n != rows {
				t.Errorf("%d audit rows for patching turned off at start, want %d", n, rows)
			}
			var patching string
			if err := pool.QueryRow(ctx, "SELECT patching FROM org_preferences").Scan(&patching); err != nil || patching != c.want {
				t.Errorf("patching %q, want %q: %v", patching, c.want, err)
			}
			if got := strings.Contains(logs.String(), said); got != c.log {
				t.Errorf("logged %q, want the line %t", logs.String(), c.log)
			}
		})
	}
}

// While the server's CANYON_PATCHING is on, patching is off and no licence unlocks it, an admin's Needs you says Canyon
// patches nothing; no member is told, and patching on or a licence that unlocks it ends the need.
func TestPatchingNoLicenceUnlocksIsAnAdminsNeed(t *testing.T) {
	withLicenceEnv(t, "")
	h := outboundHost(t)
	pipe.Patches.Store(true)
	t.Cleanup(func() { pipe.Patches.Store(false) })
	h.exec(t, "UPDATE org_preferences SET patching = 'off'")
	// The whole page is the contract's, its rows' envs.<env>.patch among them, which patching on carries (#2883).
	fleet := contractSchema(t, "fleet.schema.json")
	if code, _, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || !strings.Contains(lastBody, `"kind":"patching_unlicensed"`) {
		t.Errorf("the admin's fleet: %d %s, want patching_unlicensed among org_needs", code, lastBody)
	} else if !strings.Contains(lastBody, `"patch":{"state":`) {
		t.Errorf("the admin's fleet carries no envs.<env>.patch with patching on: %s", lastBody)
	} else {
		validJSON(t, fleet, lastBody)
	}
	if code, page, _ := do(t, h.carl, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || len(page["org_needs"].([]any)) != 0 {
		t.Errorf("a contributor's fleet carries %v, want no org need", page["org_needs"])
	} else {
		validJSON(t, fleet, lastBody)
	}
	if code, _, _ := do(t, h.vic, http.MethodGet, h.base+"/api/v1/fleet", ""); code != 200 || strings.Contains(lastBody, `"patch":{`) {
		t.Errorf("a viewer's fleet: %d %s, want no envs.<env>.patch on a reduced row", code, lastBody)
	} else {
		validJSON(t, fleet, lastBody)
	}
	licensed(t, h.base).Env = signedLicence(testExpiry, "platform_patching")
	if do(t, h.dana, http.MethodGet, h.base+"/api/v1/fleet", ""); strings.Contains(lastBody, `"kind":"patching_unlicensed"`) {
		t.Errorf("licensed: the admin's fleet still says patching needs a licence: %s", lastBody)
	}
	licensed(t, h.base).Env = ""
	h.exec(t, "UPDATE org_preferences SET patching = 'ask'")
	if do(t, h.dana, http.MethodGet, h.base+"/api/v1/fleet", ""); strings.Contains(lastBody, `"kind":"patching_unlicensed"`) {
		t.Errorf("patching on, as an upgraded install keeps it: the admin's fleet still says it needs a licence: %s", lastBody)
	}
}
