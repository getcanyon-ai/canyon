package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

func skillManifest(t *testing.T, c *http.Client, base string) (int, map[string]any) {
	t.Helper()
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/skill/manifest", "")
	return code, out
}

// TestTheManifestIsTheGoldenShape registers the fixture's design system, PUTs the fixture's
// preferences, and compares the manifest byte for byte with the fixture's golden — the fixture's
// host, "http://localhost:7777", is the test server's configured public URL, so no substitution
// is needed there.
func TestTheManifestIsTheGoldenShape(t *testing.T) {
	shipped(t) // every policy the host ships with, its default tools among them
	_, _, base, c := withProject(t)
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
	put := doc["preferences_put"].(map[string]any)
	if code, _ := putPreferences(t, c, base, strings.ReplaceAll(string(mustJSON(put["request"])), "ds_9f21", id)); code != 200 {
		t.Fatalf("setting the fixture's preferences: %d %s", code, lastBody)
	}
	code, got := skillManifest(t, c, base)
	want := doc["manifest"]
	if code != 200 || string(mustJSON(got)) != string(mustJSON(want)) {
		t.Fatalf("the golden shape:\n got %d %s\nwant %s", code, mustJSON(got), mustJSON(want))
	}
}

// TestHostIsConfigurationNotTheRequest asks with a foreign Host header; the answer must still
// carry the server's configured public URL, never the request's.
func TestHostIsConfigurationNotTheRequest(t *testing.T) {
	_, _, base, c := withProject(t)
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/skill/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "elsewhere.localhost:9999" // a name this host was not configured under; under auth off a name beyond localhost is refused 421 (loopbackOnly)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /skill/manifest with a foreign Host: %d %s", resp.StatusCode, raw)
	}
	if out["host"] != "http://localhost:7777" {
		t.Fatalf("a request Host must not steer the manifest's host: %v", out["host"])
	}
}

// TestAFreshHostManifest checks the empty-preferences shape: stacks is [], design_system is
// null, and every policy and the component menu are there regardless.
func TestAFreshHostManifest(t *testing.T) {
	shipped(t) // every policy the host ships with, its default tools among them
	_, _, base, c := withProject(t)
	code, got := skillManifest(t, c, base)
	if code != 200 {
		t.Fatalf("GET /skill/manifest: %d %s", code, lastBody)
	}
	stacks, _ := got["stacks"].([]any)
	if got["stacks"] == nil || len(stacks) != 0 {
		t.Fatalf("a fresh host's stacks must be [], never null: %s", lastBody)
	}
	if got["design_system"] != nil {
		t.Fatalf("a fresh host's design_system must be null: %s", lastBody)
	}
	policies, _ := got["policies"].([]any)
	on := slices.DeleteFunc(slices.Clone(testPolicy().Rules), func(r gatepolicy.Rule) bool { return !r.On() }) // platform.known-vuln and prod.egress-allowed install off: no agent is handed either
	if len(policies) != len(on) || len(on) != len(testPolicy().Rules)-2 {
		t.Fatalf("every policy that is on, regardless of preferences: %s", lastBody)
	}
	types, _ := got["component_types"].([]any)
	if len(types) != 2 {
		t.Fatalf("the snapshot's component menu, regardless of preferences: %s", lastBody)
	}
}

// TestTheDesignSystemCarriesNoID checks the manifest's design_system keys are exactly name,
// namespace, registry_url, tokens_url — never id, which the registry row carries.
func TestTheDesignSystemCarriesNoID(t *testing.T) {
	_, _, base, c := withProject(t)
	if code, _ := putPreferences(t, c, base, `{"design_system_id":"ds_0001"}`); code != 200 {
		t.Fatalf("setting the shipped default: %d %s", code, lastBody)
	}
	code, got := skillManifest(t, c, base)
	if code != 200 {
		t.Fatalf("GET /skill/manifest: %d %s", code, lastBody)
	}
	ds, _ := got["design_system"].(map[string]any)
	if ds == nil {
		t.Fatalf("the resolved default: %s", lastBody)
	}
	if _, has := ds["id"]; has {
		t.Fatalf("the manifest's design_system carries an id: %v", ds)
	}
	if len(ds) != 3 || ds["name"] != "shadcn/ui" || ds["namespace"] != "@shadcn/ui" || ds["registry_url"] != "https://ui.shadcn.com/r" {
		t.Fatalf("name, namespace, registry_url only (shadcn carries no tokens_url): %v", ds)
	}
}

// TestTheManifestFollowsTheRowWithNoRestart checks the manifest is built fresh on every request:
// a PUT to preferences changes the next GET, with nothing to restart or invalidate.
func TestTheManifestFollowsTheRowWithNoRestart(t *testing.T) {
	_, _, base, c := withProject(t)
	if code, _ := putPreferences(t, c, base, `{"design_system_id":"ds_0001"}`); code != 200 {
		t.Fatalf("setting the shipped default: %d %s", code, lastBody)
	}
	_, before := skillManifest(t, c, base)
	if code, _ := putPreferences(t, c, base, `{"stacks":[{"language":"Go","frameworks":["chi"]}],"design_system_id":null}`); code != 200 {
		t.Fatalf("the second PUT: %d %s", code, lastBody)
	}
	code, after := skillManifest(t, c, base)
	if code != 200 {
		t.Fatalf("GET /skill/manifest: %d %s", code, lastBody)
	}
	if before["design_system"] == nil || after["design_system"] != nil {
		t.Fatalf("design_system did not follow the PUT: before %v after %v", before["design_system"], after["design_system"])
	}
	if string(mustJSON(after["stacks"])) != `[{"frameworks":["chi"],"language":"Go"}]` {
		t.Fatalf("stacks did not follow the PUT: %s", lastBody)
	}
}

// TestAnOffRuleIsNeverHandedOut checks the manifest carries the rules that are on only — a rule Off
// before preview and placed before production among them, with its production — in the snapshot's
// order, and answers a list — never null — when nothing is left.
func TestAnOffRuleIsNeverHandedOut(t *testing.T) {
	rules := []gatepolicy.Rule{{ID: "a.one", Name: "One", State: gatepolicy.Blocking}, {ID: "b.two", Name: "Two", State: gatepolicy.Off},
		{ID: "c.three", Name: "Three", State: gatepolicy.Advisory}, {ID: "d.four", Name: "Four", State: gatepolicy.Off, Production: gatepolicy.Blocking}}
	if got := string(mustJSON(manifestPolicies(rules))); got != `[{"id":"a.one","name":"One","state":"blocking"},{"id":"c.three","name":"Three","state":"advisory"},{"id":"d.four","name":"Four","state":"off","production":"blocking"}]` {
		t.Fatalf("the manifest's policies: %s", got)
	}
	if got := manifestPolicies(nil); got == nil {
		t.Fatal("no rules must answer [], not null")
	}
}

// TestWhatBlocksHandsHowToMeetIt checks the manifest hands, where a rule blocks in either gate, a Canyon
// rule's line on how to pass it and a custom rule's text — beside it, on one checked by code, the
// condition Canyon checks — and a rule's name alone where it only warns; a rule off in both gates is
// not handed at all (#2622, #2639).
func TestWhatBlocksHandsHowToMeetIt(t *testing.T) {
	rules := []gatepolicy.Rule{
		{ID: "secrets.client-bundle", Name: "No secrets in the code", State: gatepolicy.Blocking},
		{ID: "code.injection", Name: "No user input reaching commands", State: gatepolicy.Advisory, Production: gatepolicy.Blocking},
		{ID: "deps.known-vuln", Name: "No known-vulnerable dependencies", State: gatepolicy.Advisory},
		{ID: "custom.audit", Name: "Audit", State: gatepolicy.Blocking, Instructions: "Log every write."},
		{ID: "custom.late", Name: "Late", State: gatepolicy.Off, Production: gatepolicy.Blocking, Instructions: "Name tables in the plural."},
		{ID: "custom.warn", Name: "Warn", State: gatepolicy.Advisory, Production: gatepolicy.Advisory, Instructions: "Prefer UUIDs."},
		{ID: "custom.gone", Name: "Gone", State: gatepolicy.Off, Instructions: "Never seen."},
		{ID: "custom.readme", Name: "Readme", State: gatepolicy.Blocking, Instructions: "Every app has a README.md.",
			Check: &gatepolicy.Check{By: "code", Expr: "'README.md' in files", Says: "The app has a README.md at its root.", FactsVersion: 2}},
		{ID: "custom.license", Name: "License", State: gatepolicy.Advisory, Instructions: "Every app has a LICENSE.",
			Check: &gatepolicy.Check{By: "code", Expr: "'LICENSE' in files", Says: "The app has a LICENSE at its root.", FactsVersion: 2}},
	}
	want := `[{"id":"secrets.client-bundle","name":"No secrets in the code","state":"blocking","how":` + string(mustJSON(gatepolicy.HowToPass("secrets.client-bundle"))) + `},` +
		`{"id":"code.injection","name":"No user input reaching commands","state":"advisory","production":"blocking","how":` + string(mustJSON(gatepolicy.HowToPass("code.injection"))) + `},` +
		`{"id":"deps.known-vuln","name":"No known-vulnerable dependencies","state":"advisory"},` +
		`{"id":"custom.audit","name":"Audit","state":"blocking","instructions":"Log every write."},` +
		`{"id":"custom.late","name":"Late","state":"off","production":"blocking","instructions":"Name tables in the plural."},` +
		`{"id":"custom.warn","name":"Warn","state":"advisory","production":"advisory"},` +
		`{"id":"custom.readme","name":"Readme","state":"blocking","instructions":"Every app has a README.md.","says":"The app has a README.md at its root."},` +
		`{"id":"custom.license","name":"License","state":"advisory"}]`
	if got := string(mustJSON(manifestPolicies(rules))); got != want {
		t.Fatalf("the manifest's policies:\n got %s\nwant %s", got, want)
	}
}

// TestThePublicURLIsSchemeAndHostOnly checks the boot refuses a public URL the manifest contract
// would refuse, and forgives only a lone trailing slash.
func TestThePublicURLIsSchemeAndHostOnly(t *testing.T) {
	for _, s := range []string{"ftp://canyon.example", "canyon.example:7777", "http://", "https://user@canyon.example", "https://canyon.example/api", "https://canyon.example?x=1", "https://canyon.example?", "https://canyon.example#f", "https://:443"} {
		if _, err := publicURLOf(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for s, want := range map[string]string{"http://localhost:7777": "http://localhost:7777", "https://canyon.example/": "https://canyon.example"} {
		if u, err := publicURLOf(s); err != nil || u.Scheme+"://"+u.Host != want {
			t.Errorf("%q: %v %v", s, u, err)
		}
	}
}

// With ?app=, the manifest names the app's own additions to the org's rules, and who set them, to the
// app's team and admins (contracts/routes.md): an agent told the org's states alone said "this rule
// only warns" of a rule the app holds as blocking (QA batch 2, 2026-10-07). Anyone else, an unknown
// app, and no ?app= at all get the org's manifest and no refusal, because a session hook reads it.
func TestTheManifestNamesAnAppsOwnRulesToItsTeam(t *testing.T) {
	withModel(t, &scripted{}) // placing an AI rule needs a model on the host (#1555)
	_, _, base, olive, carl, vic, nora, dana := teamOf(t)
	raw, err := os.ReadFile("../../contracts/testdata/guardrails/preferences.json")
	var doc map[string]map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	want := doc["manifest_app"]["app_additions"].([]any)[0].(map[string]any)
	if code, _, _ := do(t, olive, http.MethodPut, base+additionAt+fmt.Sprint(want["id"])+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("olive's addition: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"the owner": olive, "a contributor": carl, "an admin": dana} {
		_, got, _ := do(t, c, http.MethodGet, base+"/api/v1/skill/manifest?app=claims-portal", "")
		adds, _ := got["app_additions"].([]any)
		if len(adds) != 1 {
			t.Fatalf("%s: app_additions %v in %s", who, got["app_additions"], lastBody)
		}
		add := adds[0].(map[string]any)
		by, _ := add["set_by"].(map[string]any)
		if add["id"] != want["id"] || add["name"] != want["name"] || add["state"] != want["state"] || add["production"] != nil || by["subject"] != "sub-olive" || by["kind"] != "person" {
			t.Errorf("%s reads %v, want the golden's with olive as set_by", who, add)
		}
	}
	// The org catches up: the rule is blocking for everyone, so the app adds nothing to it.
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+fmt.Sprint(want["id"])+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("the org's edit: %d %s", code, lastBody)
	}
	if _, got, _ := do(t, olive, http.MethodGet, base+"/api/v1/skill/manifest?app=claims-portal", ""); got["app_additions"] != nil {
		t.Errorf("an addition the org has caught up with is still handed out: %v", got["app_additions"])
	}
	for who, ask := range map[string]struct {
		c   *http.Client
		app string
	}{"a viewer": {vic, "?app=claims-portal"}, "a member with no access": {nora, "?app=claims-portal"}, "an unknown app": {olive, "?app=nope"}, "no app": {olive, ""}} {
		if code, got, _ := do(t, ask.c, http.MethodGet, base+"/api/v1/skill/manifest"+ask.app, ""); code != 200 || got["app_additions"] != nil {
			t.Errorf("%s: %d %v", who, code, got["app_additions"])
		}
	}
}

// TestTheManifestHandsTheTextOfACustomRuleThatBlocks reads the route end to end: an org's own rule that
// only warns is handed by name, one an app makes blocking hands its text under that app's additions,
// and one the org makes blocking hands it under the policies (contracts/routes.md; #2622).
func TestTheManifestHandsTheTextOfACustomRuleThatBlocks(t *testing.T) {
	withModel(t, &scripted{}) // turning an AI rule on needs a model on the host (#1555)
	_, _, base, olive, _, _, _, dana := teamOf(t)
	if code, _, _ := do(t, dana, http.MethodPut, base+emailPath, emailBody); code != 200 {
		t.Fatalf("the custom rule: %d %s", code, lastBody)
	}
	read := func(what string) (map[string]any, map[string]any) {
		t.Helper()
		_, got, _ := do(t, olive, http.MethodGet, base+"/api/v1/skill/manifest?app=claims-portal", "")
		var rule, add map[string]any
		for _, p := range got["policies"].([]any) {
			if p.(map[string]any)["id"] == emailRule {
				rule = p.(map[string]any)
			}
		}
		adds, _ := got["app_additions"].([]any)
		if len(adds) > 0 {
			add = adds[0].(map[string]any)
		}
		if rule == nil {
			t.Fatalf("%s: the manifest hands no %s: %s", what, emailRule, lastBody)
		}
		return rule, add
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+customPath+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the org's edit to advisory: %d %s", code, lastBody)
	}
	if rule, add := read("warn-only"); rule["instructions"] != nil || add != nil {
		t.Errorf("a custom rule that only warns hands %v, additions %v", rule, add)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+additionAt+emailRule+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("olive's addition: %d %s", code, lastBody)
	}
	if rule, add := read("the app's addition"); rule["instructions"] != nil || add["id"] != emailRule || add["instructions"] != emailText {
		t.Errorf("an addition that blocks: policy %v, addition %v", rule, add)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+customPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("the org's edit to blocking: %d %s", code, lastBody)
	}
	if rule, add := read("the org's"); rule["instructions"] != emailText || add != nil {
		t.Errorf("a custom rule that blocks: policy %v, addition %v", rule, add)
	}
}
