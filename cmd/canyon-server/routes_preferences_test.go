package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func putPreferences(t *testing.T, c *http.Client, base, body string) (int, map[string]any) {
	t.Helper()
	code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/preferences", body)
	return code, out
}

func getPreferences(t *testing.T, c *http.Client, base string) map[string]any {
	t.Helper()
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/preferences", "")
	if code != 200 {
		t.Fatalf("GET /preferences: %d %s", code, lastBody)
	}
	return out
}

// The approved list answers one question per language, so a second row for a language the body
// already named is refused — and the refusal leaves the stored list alone.
func TestOneLanguageCarriesOneRow(t *testing.T) {
	_, _, base, c := withProject(t)
	if code, _ := putPreferences(t, c, base, `{"stacks":[{"language":"TypeScript","frameworks":["React"]}]}`); code != 200 {
		t.Fatalf("the first list: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, c, base, `{"stacks":[{"language":"TypeScript","frameworks":["React"]},{"language":"TypeScript","frameworks":["Vite"]}]}`); code != 400 || body["error"] != "invalid_stacks" {
		t.Fatalf("two rows for one language: %d %s", code, lastBody)
	}
	if got := string(mustJSON(getPreferences(t, c, base)["stacks"])); got != `[{"frameworks":["React"],"language":"TypeScript"}]` {
		t.Fatalf("the refused PUT changed the list: %s", got)
	}
}

// A design system no row carries is a 404 from the registry read, not a decoded foreign-key
// violation — and nothing is written on the way to it.
func TestADesignSystemNoRowCarriesIsRefusedAndWritesNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	rows := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences'")
	}
	if code, _ := putPreferences(t, c, base, `{"design_system_id":"ds_0001"}`); code != 200 {
		t.Fatalf("a shipped row: %d %s", code, lastBody)
	}
	// A shipped row has no tokens address, and the answer has no tokens_url key at all — not a null,
	// which the contract's resolved shape refuses.
	if getPreferences(t, c, base); lastBody != `{"stacks":[],"approved_buildpacks":[],"buildpacks_seen":[],"design_system":{"id":"ds_0001","name":"shadcn/ui","namespace":"@shadcn/ui","registry_url":"https://ui.shadcn.com/r"},"setup_available":false,"ai_review":false,"org_name":"Meridian","allowed_domains":[],"allowed_emails":[],"admin_emails":[],"bootstrap_admins":[],"responder_emails":[],"groups_claim":"","onboarded_at":null,"run_by":"canyon","access_grace":"1h","delivery":"webhook","prod_approval":false,"push_approvers_linked":false,"patching":"off","rebuilds":"off","db_sessions":"off","overridden":[]}` {
		t.Fatalf("the resolved shipped row, byte for byte: %s", lastBody)
	}
	was := rows()
	if code, body := putPreferences(t, c, base, `{"design_system_id":"ds_nope"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an id no row carries: %d %s", code, lastBody)
	}
	ds, _ := getPreferences(t, c, base)["design_system"].(map[string]any)
	if ds["id"] != "ds_0001" {
		t.Fatalf("the refusal changed the stored design system: %s", lastBody)
	}
	if now := rows(); now != was {
		t.Fatalf("the refusal wrote %d audit row(s)", now-was)
	}
}

// An OpenAI host reviews with a model though it serves no setup chat: the read says the review is on
// and offers no panel (#2003).
func TestTheReadSaysTheReviewIsOnWhereTheHostReviewsWithoutTheSetupChat(t *testing.T) {
	testReviews = true
	t.Cleanup(func() { testReviews = false })
	_, _, base, c := withProject(t)
	if got := getPreferences(t, c, base); got["ai_review"] != true || got["setup_available"] != false {
		t.Fatalf("an OpenAI host: %s", lastBody)
	}
}

// The golden shapes: a fresh host, the fixture's PUT, and the fixture's read.
func TestThePreferencesAreTheGoldenShape(t *testing.T) {
	withModel(t, &scripted{}) // the golden reads setup_available: true, so this host has a model
	pool, ctx, base, c := withProject(t)
	getPreferences(t, c, base)
	if lastBody != `{"stacks":[],"approved_buildpacks":[],"buildpacks_seen":[],"design_system":null,"setup_available":true,"ai_review":true,"org_name":"Meridian","allowed_domains":[],"allowed_emails":[],"admin_emails":[],"bootstrap_admins":[],"responder_emails":[],"groups_claim":"","onboarded_at":null,"run_by":"canyon","access_grace":"1h","delivery":"webhook","prod_approval":false,"push_approvers_linked":false,"patching":"off","rebuilds":"off","db_sessions":"off","overridden":[]}` {
		t.Fatalf("a fresh host: %s", lastBody)
	}
	raw, err := os.ReadFile("../../contracts/testdata/guardrails/preferences.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	create := doc["design_system_create"].(map[string]any)
	_, made := registerDesignSystem(t, c, base, string(mustJSON(create["request"])))
	row, _ := made["design_system"].(map[string]any)
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatalf("registering Meridian: %s", lastBody)
	}

	golden := func(v any) string { // the id is this run's; every other byte is the fixture's
		return strings.ReplaceAll(string(mustJSON(v)), "ds_9f21", id)
	}
	put := doc["preferences_put"].(map[string]any)
	code, answer := putPreferences(t, c, base, strings.ReplaceAll(string(mustJSON(put["request"])), "ds_9f21", id))
	if want := golden(put["response"]); code != 200 || string(mustJSON(answer)) != want {
		t.Fatalf("the PUT answers the read's shape:\n got %s\nwant %s", mustJSON(answer), want)
	}
	if want := golden(doc["preferences"]); string(mustJSON(getPreferences(t, c, base))) != want {
		t.Fatalf("the read:\n got %s\nwant %s", lastBody, want)
	}
	// The fixture's PUT sets onboarded_at from null: the write that ends the setup state, on the
	// trail by name, its detail the keys that changed with their before and after.
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'complete setup'").Scan(&detail); err != nil {
		t.Fatalf("one audit row named complete setup: %v", err)
	}
	if got := string(mustJSON(detail["onboarded_at"])); got != `{"after":"2026-09-16T09:00:00Z","before":null}` || detail["org_name"] == nil || detail["stacks"] == nil {
		t.Fatalf("the detail: %v", detail)
	}
}

// The three refusals, each judged on the row as the PUT would leave it and answered before
// anything is written; the admin pair is one key; onboarded_at is set once.
func TestTheRefusalsAreJudgedOnTheRowAsThePutWouldLeaveIt(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	was := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	for _, tc := range []struct{ what, body, want string }{
		{"clearing the only list on a fresh row", `{"allowed_emails":[]}`, "no_members"},
		{"finishing with nobody allowed", `{"onboarded_at":"2026-09-16T09:00:00Z"}`, "no_members"},
		{"finishing with a domain and no admin", `{"allowed_domains":["meridian.example"],"onboarded_at":"2026-09-16T09:00:00Z"}`, "no_admins"},
		{"an empty admin list with no claim", `{"admin_emails":[]}`, "no_admins"},
	} {
		if code, body := putPreferences(t, c, base, tc.body); code != 400 || body["error"] != tc.want {
			t.Fatalf("%s: %d %s", tc.what, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != was {
		t.Fatalf("four refusals wrote %d row(s)", n-was)
	}
	// A claim stands in for an address; clearing the pair with no address is refused, with one it is not.
	if code, _ := putPreferences(t, c, base, `{"admin_claim":"groups","admin_value":"platform","allowed_domains":["meridian.example"]}`); code != 200 {
		t.Fatalf("a claim and a domain: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, c, base, `{"admin_claim":null,"admin_value":null}`); code != 400 || body["error"] != "no_admins" {
		t.Fatalf("clearing the only admin: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, c, base, `{"admin_emails":["dana@meridian.example"],"admin_claim":null,"admin_value":null}`); code != 200 || body["admin_claim"] != nil {
		t.Fatalf("an address replacing the claim: %d %s", code, lastBody)
	}
	// An address replacing the domain keeps a membership; clearing both lists is refused.
	if code, _ := putPreferences(t, c, base, `{"allowed_domains":[],"allowed_emails":["dana@meridian.example"]}`); code != 200 {
		t.Fatalf("an address replacing the domain: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, c, base, `{"allowed_emails":[]}`); code != 400 || body["error"] != "no_members" {
		t.Fatalf("removing the last member: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, c, base, `{"onboarded_at":"2026-09-16T09:00:00Z"}`); code != 200 {
		t.Fatalf("finishing: %d %s", code, lastBody)
	}
	if code, body := putPreferences(t, c, base, `{"onboarded_at":"2026-09-17T09:00:00Z"}`); code != 400 || body["error"] != "invalid_body" || getPreferences(t, c, base)["onboarded_at"] != "2026-09-16T09:00:00Z" {
		t.Fatalf("onboarded_at is set once and never changed: %d %s", code, lastBody)
	}
}

// The bounds the contract states on the organisation's settings, at the edge on either side.
func TestTheBoundsOfTheOrganisationsSettings(t *testing.T) {
	_, _, base, c := withProject(t)
	for _, tc := range []struct{ what, body string }{
		{"an org name nobody typed", `{"org_name":""}`},
		{"a 129-character org name", `{"org_name":"` + strings.Repeat("M", 129) + `"}`},
		{"an org name hiding a line separator", `{"org_name":"Meridian Insurance"}`},
		{"a domain without a dot", `{"allowed_domains":["meridian"]}`},
		{"a domain in upper case", `{"allowed_domains":["Meridian.example"]}`},
		{"an address without an at", `{"allowed_emails":["dana"]}`},
		{"an address in upper case", `{"admin_emails":["Dana@meridian.example"]}`},
		{"an address carrying a line break", `{"allowed_emails":["dana@meridian.example\n"]}`},
		{"an address carrying an escape sequence", `{"admin_emails":["dana\u001b[31m@meridian.example"]}`},
		{"a list that repeats an entry", `{"allowed_emails":["dana@meridian.example","dana@meridian.example"]}`},
		{"a null list", `{"allowed_domains":null}`},
		{"a list of numbers", `{"admin_emails":[1]}`},
		{"a claim without its value", `{"admin_claim":"groups"}`},
		{"a value without its claim", `{"admin_value":"platform"}`},
		{"a claim with a null value", `{"admin_claim":"groups","admin_value":null}`},
		{"a 257-character claim", `{"admin_claim":"` + strings.Repeat("g", 257) + `","admin_value":"platform"}`},
		{"onboarded_at that is not an instant", `{"onboarded_at":"yesterday"}`},
		{"onboarded_at cleared", `{"onboarded_at":null}`},
	} {
		if code, body := putPreferences(t, c, base, tc.body); code != 400 || body["error"] != "invalid_body" {
			t.Fatalf("%s: %d %s", tc.what, code, lastBody)
		}
	}
	for _, tc := range []struct{ what, body string }{
		{"a 128-character org name with accents", `{"org_name":"` + strings.Repeat("é", 128) + `"}`},
		{"a 253-character domain", `{"allowed_domains":["` + strings.Repeat("a", 249) + `.com"]}`},
		{"a 254-character address", `{"allowed_emails":["` + strings.Repeat("a", 237) + `@meridian.example"]}`},
		{"a 256-character claim", `{"admin_claim":"` + strings.Repeat("g", 256) + `","admin_value":"platform"}`},
	} {
		if code, _ := putPreferences(t, c, base, tc.body); code != 200 {
			t.Fatalf("%s: %d %s", tc.what, code, lastBody)
		}
	}
}

// A key the environment sets is read as the environment's value and named in overridden, and a
// PUT naming one is refused set_by_operator before anything is written — so no client needs to
// know an environment exists (design/ONBOARDING.md §1). The keys it leaves alone are the row's.
func TestAKeyTheEnvironmentSetsIsReadFromItAndRefusedOnAPut(t *testing.T) {
	withOIDC(t) // the domain meridian.example and the admin dana, from the environment; no claim
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodGet, base+preferencesPath, ""); code != 200 || !strings.Contains(lastBody, `"allowed_domains":["meridian.example"],"allowed_emails":[],"admin_emails":[],"bootstrap_admins":["dana@meridian.example"],`) || !strings.HasSuffix(lastBody, `"overridden":["allowed_domains"]}`) || !strings.Contains(lastBody, `"org_name":"Meridian"`) {
		t.Fatalf("the read answers the environment's values and names the keys it pins: %d %s", code, lastBody)
	}
	was := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_domains":["partner.example"],"org_name":"Meridian Insurance"}`); code != 409 || lastBody != `{"error":"set_by_operator","keys":["allowed_domains"],"message":"the environment sets these keys, so a PUT cannot"}` {
		t.Fatalf("a PUT naming a pinned key: %d %s", code, lastBody)
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT org_name FROM org_preferences").Scan(&name); err != nil || name != "" || count(t, ctx, pool, "SELECT count(*) FROM audit_events") != was {
		t.Fatalf("the refusal wrote the key beside the pinned ones: %q %v", name, err)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"org_name":"Meridian Insurance","allowed_emails":["kim@partner.example"]}`); code != 200 || string(mustJSON(body["allowed_emails"])) != `["kim@partner.example"]` {
		t.Fatalf("the keys the environment leaves alone are written: %d %s", code, lastBody)
	}
	// CANYON_ADMINS is the exception: it grants without pinning (canyonv2-cr23), so onboarding step 3
	// can add an admin on a host that has one, and the read answers both sources.
	if code, body, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"admin_emails":["bob@meridian.example"]}`); code != 200 || string(mustJSON(body["admin_emails"])) != `["bob@meridian.example"]` || string(mustJSON(body["bootstrap_admins"])) != `["dana@meridian.example"]` {
		t.Fatalf("adding an admin beside the environment's: %d %s", code, lastBody)
	}
}

// A PUT writes only the keys it names: an absent key is unchanged, and a null clears the design
// system without touching the registry row it named.
func TestAnAbsentKeyIsUnchangedAndANullClears(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	stored := func() (string, string) { // what a later reader is told, not what the answer said at the time
		t.Helper()
		out := getPreferences(t, c, base)
		ds, _ := out["design_system"].(map[string]any)
		id, _ := ds["id"].(string)
		return id, string(mustJSON(out["stacks"]))
	}
	putPreferences(t, c, base, `{"stacks":[{"language":"Python","frameworks":["FastAPI"]}],"design_system_id":"ds_0001"}`)
	putPreferences(t, c, base, `{"stacks":[{"language":"Go","frameworks":["net/http"]}]}`)
	if id, stacks := stored(); id != "ds_0001" || stacks != `[{"frameworks":["net/http"],"language":"Go"}]` {
		t.Fatalf("naming the stacks cleared the design system: %s", lastBody)
	}
	if _, body := putPreferences(t, c, base, `{"design_system_id":null}`); body["design_system"] != nil {
		t.Fatalf("a null clears it: %s", lastBody)
	}
	if id, stacks := stored(); id != "" || stacks != `[{"frameworks":["net/http"],"language":"Go"}]` {
		t.Fatalf("clearing the design system cleared the stacks: %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM org_preferences WHERE design_system_id IS NULL"); n != 1 {
		t.Fatal("the column still carries an id")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM design_systems WHERE id = 'ds_0001'"); n != 1 {
		t.Fatal("clearing the default deleted the registry row")
	}
	putPreferences(t, c, base, `{"stacks":[]}`)
	if id, stacks := stored(); id != "" || stacks != `[]` {
		t.Fatalf("a cleared design system stays cleared: %s", lastBody)
	}
}

// The audit row is the record of what changed: only the keys that moved, and nothing at all from
// a refusal or from a PUT that repeats what is stored.
func TestTheAuditRowCarriesOnlyTheKeysThatChanged(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	rows := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences'")
	}
	both := `{"stacks":[{"language":"Python","frameworks":["FastAPI"]}],"design_system_id":"ds_0001"}`
	putPreferences(t, c, base, both)
	was := rows()
	putPreferences(t, c, base, `{"stacks":[{"language":"Go","frameworks":["net/http"]}],"design_system_id":"ds_0001"}`)
	if rows() != was+1 {
		t.Fatalf("a PUT that changed the stacks wrote %d row(s)", rows()-was)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'PUT /api/v1/preferences' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	moved, _ := detail["stacks"].(map[string]any)
	if _, named := detail["design_system_id"]; named || len(detail) != 1 {
		t.Fatalf("the row names a key that did not change: %v", detail)
	}
	if string(mustJSON(moved["before"])) != `[{"frameworks":["FastAPI"],"language":"Python"}]` || string(mustJSON(moved["after"])) != `[{"frameworks":["net/http"],"language":"Go"}]` {
		t.Fatalf("the before and the after: %v", moved)
	}
	was = rows()
	putPreferences(t, c, base, `{"stacks":[{"language":"Go","frameworks":["React"]},{"language":"Go","frameworks":["Vite"]}]}`) // the 400
	putPreferences(t, c, base, `{"design_system_id":"ds_nope"}`)                                                                // the 404
	putPreferences(t, c, base, `{"stacks":[{"language":"Go","frameworks":["net/http"]}],"design_system_id":"ds_0001"}`)         // changes nothing
	if now := rows(); now != was {
		t.Fatalf("two refusals and a PUT that changed nothing wrote %d row(s)", now-was)
	}
}

// The bounds the contract states, at the edge on either side.
func TestTheBoundsOfTheApprovedList(t *testing.T) {
	_, _, base, c := withProject(t)
	seventeen := make([]stack, 17)
	for i := range seventeen {
		seventeen[i] = stack{Language: fmt.Sprintf("Language %d", i), Frameworks: []string{"One"}}
	}
	for _, tc := range []struct{ what, body, want string }{
		{"a language nobody typed", `{"stacks":[{"language":"","frameworks":["React"]}]}`, "invalid_stacks"},
		{"a language with no framework", `{"stacks":[{"language":"TypeScript","frameworks":[]}]}`, "invalid_stacks"},
		{"seventeen languages", string(mustJSON(map[string]any{"stacks": seventeen})), "invalid_stacks"},
		{"a 65-character framework", `{"stacks":[{"language":"TypeScript","frameworks":["` + strings.Repeat("R", 65) + `"]}]}`, "invalid_stacks"},
		{"a body naming neither key", `{}`, "invalid_body"},
		{"a null list", `{"stacks":null}`, "invalid_stacks"},
		{"a framework that is not a name", `{"stacks":[{"language":"TypeScript","frameworks":[1]}]}`, "invalid_stacks"},
		{"a list of lists", `{"stacks":[[{"language":"TypeScript","frameworks":["React"]}]]}`, "invalid_stacks"},
		{"a design system named by a number", `{"design_system_id":7}`, "invalid_design_system"},
		{"a framework hiding a direction override", `{"stacks":[{"language":"Go","frameworks":["net\u202ehttp"]}]}`, "invalid_stacks"},
		{"a language that is only spaces", `{"stacks":[{"language":"    ","frameworks":["React"]}]}`, "invalid_stacks"},
		{"a language with a line separator in it", `{"stacks":[{"language":"Go\u2028Rust","frameworks":["chi"]}]}`, "invalid_stacks"},
	} {
		if code, body := putPreferences(t, c, base, tc.body); code != 400 || body["error"] != tc.want {
			t.Fatalf("%s: %d %s", tc.what, code, lastBody)
		}
	}
	if code, _ := putPreferences(t, c, base, `{"stacks":[{"language":"`+strings.Repeat("é", 64)+`","frameworks":["React"]}]}`); code != 200 { // 64 characters, 128 bytes
		t.Fatalf("a 64-character language with an accent: %d %s", code, lastBody)
	}
	// The upper bounds admit exactly what they say, and two languages differing only by case are two.
	sixteen := make([]stack, 16)
	for i := range sixteen {
		sixteen[i] = stack{Language: fmt.Sprintf("Language %d", i), Frameworks: []string{"One"}}
	}
	frameworks := make([]string, 16)
	for i := range frameworks {
		frameworks[i] = fmt.Sprintf("Framework %d", i)
	}
	for _, tc := range []struct{ what, body string }{
		{"sixteen languages", string(mustJSON(map[string]any{"stacks": sixteen}))},
		{"sixteen frameworks", string(mustJSON(map[string]any{"stacks": []stack{{Language: "Go", Frameworks: frameworks}}}))},
		{"Go and go", `{"stacks":[{"language":"Go","frameworks":["chi"]},{"language":"go","frameworks":["gin"]}]}`},
	} {
		if code, _ := putPreferences(t, c, base, tc.body); code != 200 {
			t.Fatalf("%s: %d %s", tc.what, code, lastBody)
		}
	}
}

// The read answers who is reading — the address the session stores and the role — and answers the
// admin claim and its value to an admin only (contracts/routes.md row 219, canyonv2-7mtz). The
// member's `overridden` is the admin's: a variable's name is not its value, and a member who cannot
// tell a redacted field from an unset one reads an operator-pinned claim as no claim at all.
func TestTheReadAnswersTheViewerAndRedactsTheAdminClaimForAMember(t *testing.T) {
	withOIDC(t) // CANYON_OIDC_ALLOWED_DOMAINS is meridian.example, pinned; CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	// The claim is the row's and not the environment's, so it is a value somebody set rather than one
	// an operator pinned, and `overridden` names only the domains the operator did pin.
	if code, _ := putPreferences(t, admin, base, `{"admin_claim":"groups","admin_value":"canyon-admins"}`); code != 200 {
		t.Fatalf("setting the pair: %d %s", code, lastBody)
	}
	read := getPreferences(t, admin, base)
	if read["admin_claim"] != "groups" || read["admin_value"] != "canyon-admins" {
		t.Fatalf("an admin is answered the pair: %s", lastBody)
	}
	if got := string(mustJSON(read["viewer"])); got != `{"email":"dana@meridian.example","role":"admin"}` {
		t.Fatalf("the admin's viewer: %s", got)
	}
	was := read["overridden"]
	read = getPreferences(t, member, base)
	for _, key := range []string{"admin_claim", "admin_value"} {
		if _, answered := read[key]; answered {
			t.Fatalf("a member is answered %s: %s", key, lastBody)
		}
	}
	if got := string(mustJSON(read["viewer"])); got != `{"email":"carol@meridian.example","role":"member"}` {
		t.Fatalf("the member's viewer: %s", got)
	}
	if got, want := string(mustJSON(read["overridden"])), string(mustJSON(was)); got != want {
		t.Fatalf("the member is told %s where the admin is told %s; the keys the environment sets are named to both", got, want)
	}
	// The redaction is the answer's, never the row's: the claim the member could not read is still
	// the claim the row holds, and the next admin read says so.
	if again := getPreferences(t, admin, base); again["admin_claim"] != "groups" {
		t.Fatalf("the member read changed the row: %s", lastBody)
	}
}

// The trail records the admin claim being set and being cleared, with its real before and after.
// This is the redaction's trap: applyPreferences reads the row through the same loaders the read
// does, for the before and the after of the audit row, so a redaction tidied down into
// readPreferences or orgPreferences would write null on both sides of a pair that moved — the PUT
// that set or cleared the org's admin claim would be recorded as having changed nothing, and the
// governance record would say nobody ever touched it.
func TestTheTrailRecordsTheAdminClaimMovingAndNotANullOnEitherSide(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _ := putPreferences(t, c, base, `{"allowed_domains":["meridian.example"],"admin_claim":"groups","admin_value":"canyon-admins"}`); code != 200 {
		t.Fatalf("setting the pair: %d %s", code, lastBody)
	}
	if code, _ := putPreferences(t, c, base, `{"admin_emails":["dana@meridian.example"],"admin_claim":null,"admin_value":null}`); code != 200 {
		t.Fatalf("an address replacing the claim: %d %s", code, lastBody)
	}
	pair := func(nth int) string {
		var detail map[string]any
		if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'PUT /api/v1/preferences' ORDER BY ts, id OFFSET $1 LIMIT 1", nth).Scan(&detail); err != nil {
			t.Fatalf("the %d-th write's audit row: %v", nth, err)
		}
		return string(mustJSON(detail["admin_claim"])) + " " + string(mustJSON(detail["admin_value"]))
	}
	if got, want := pair(0), `{"after":"groups","before":null} {"after":"canyon-admins","before":null}`; got != want {
		t.Fatalf("the claim being set:\n got %s\nwant %s", got, want)
	}
	if got, want := pair(1), `{"after":null,"before":"groups"} {"after":null,"before":"canyon-admins"}`; got != want {
		t.Fatalf("the claim being cleared:\n got %s\nwant %s", got, want)
	}
}

// The loaders redact for nobody, whatever context they are handed. This is the invariant the
// redaction's comment states, pinned rather than described: applyPreferences reads the row through
// readPreferences and orgPreferences for the before and the after of the audit row, and an empty
// context — no role, no session, as a system write would carry — must still be handed the pair.
func TestTheLoadersRedactForNobodyWhateverContextTheyAreHanded(t *testing.T) {
	pool, _ := migrated(t)
	a := &app{pool: pool}
	claim, value := "groups", "canyon-admins"
	var moved string
	if _, _, err := a.applyPreferences(context.Background(), actor{Kind: "canyon", Subject: "canyon"}, "PUT /api/v1/preferences",
		prefEdit{adminClaim: &claim, adminValue: &value, setAdmin: true},
		func(before, after preferences) map[string]any {
			moved = string(mustJSON(before.AdminClaim)) + " to " + string(mustJSON(after.AdminClaim))
			return nil // no audit row wanted: what the loaders hand this function is the whole subject
		}); err != nil {
		t.Fatal(err)
	}
	if moved != `null to "groups"` {
		t.Fatalf("a context with no role was handed %s; the redaction is the read's, never the loaders'", moved)
	}
}

// The worse half of the same trap: a write that names only the pair moves only the pair, so under a
// redacting loader the detail is empty and applyPreferences writes no audit row at all — not a row
// with the field missing, no row. Changing who may administer the organisation would leave nothing.
func TestAWriteNamingOnlyTheAdminClaimIsOnTheTrail(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _ := putPreferences(t, c, base, `{"admin_claim":"groups","admin_value":"canyon-admins"}`); code != 200 {
		t.Fatalf("naming only the pair: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'PUT /api/v1/preferences'").Scan(&detail); err != nil {
		t.Fatalf("the write's audit row: %v", err)
	}
	if got := string(mustJSON(detail)); got != `{"admin_claim":{"after":"groups","before":null},"admin_value":{"after":"canyon-admins","before":null}}` {
		t.Fatalf("the row names the pair and what it did: %s", got)
	}
}

// admission decides putPreferences' re-PUT of every app's access file (contracts/executor.md, PUT
// /access): finishing setup flips accessFile's membership from admins-only to the org's real lists,
// so it has to count as a change even when named alone, with nothing else on the same PUT.
func TestAdmissionCarriesTheSetupState(t *testing.T) {
	on := "2026-09-16T09:00:00Z"
	before, after := preferences{}, preferences{OnboardedAt: &on}
	if !changed(admission(before), admission(after)) {
		t.Fatal("finishing setup did not change admission(): a running app would keep its old access file for up to a minute")
	}
}

// The approved buildpacks are at most 64 buildpack ids, each once; anything else is refused by name
// and nothing is written (contracts/routes.md, GET/PUT /preferences; #636).
func TestApprovedBuildpacksOutsideTheirBoundsAreRefused(t *testing.T) {
	_, _, base, c := withProject(t)
	var many []string
	for i := range 65 {
		many = append(many, fmt.Sprintf("%q", fmt.Sprintf("bp/%d", i)))
	}
	for _, body := range []string{`{"approved_buildpacks":null}`, `{"approved_buildpacks":"paketo-buildpacks/node-engine"}`, `{"approved_buildpacks":["a","a"]}`,
		`{"approved_buildpacks":["has space"]}`, `{"approved_buildpacks":["-leading"]}`, `{"approved_buildpacks":[` + strings.Join(many, ",") + `]}`} {
		if code, out := putPreferences(t, c, base, body); code != 400 || out["error"] != "invalid_approved_buildpacks" {
			t.Fatalf("%.60s: %d %s", body, code, lastBody)
		}
	}
	if got := getPreferences(t, c, base)["approved_buildpacks"]; fmt.Sprint(got) != "[]" {
		t.Fatalf("a refusal wrote: %v", got)
	}
}
