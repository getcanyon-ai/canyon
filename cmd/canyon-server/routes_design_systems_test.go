package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

const meridian = `{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://registry.meridian.example/ui"}`

func registerDesignSystem(t *testing.T, c *http.Client, base, body string) (int, map[string]any) {
	t.Helper()
	code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/design-systems", body)
	return code, out
}

func TestOneNamespaceCarriesOneDesignSystem(t *testing.T) {
	_, _, base, c := withProject(t)
	if code, body := registerDesignSystem(t, c, base, `{"name":"Ours","namespace":"@shadcn/ui","registry_url":"https://ours.example/r"}`); code != 409 || body["error"] != "exists" {
		t.Fatalf("a shipped namespace again: %d %s", code, lastBody)
	}
	if code, _ := registerDesignSystem(t, c, base, meridian); code != 201 {
		t.Fatalf("a namespace nobody carries: %d %s", code, lastBody)
	}
	if code, body := registerDesignSystem(t, c, base, meridian); code != 409 || body["error"] != "exists" { // the 409 is about the namespace, not about being shipped
		t.Fatalf("the same namespace a moment later: %d %s", code, lastBody)
	}
}

func TestTheRegistryPageIsTheGoldenShape(t *testing.T) {
	_, _, base, c := withProject(t)
	doc := guardrailsGolden(t)
	create := doc["design_system_create"].(map[string]any)
	code, body := registerDesignSystem(t, c, base, string(mustJSON(create["request"])))
	made, _ := body["design_system"].(map[string]any)
	if code != 201 || made["source_type"] != "registry" {
		t.Fatalf("registering Meridian: %d %s", code, lastBody)
	}
	want := string(mustJSON(doc["design_systems"]))
	for from, to := range map[string]string{"ds_9f21": made["id"].(string), "2026-03-11T14:22:07Z": made["created_at"].(string), "tomas@meridian.example": "anonymous"} {
		want = strings.ReplaceAll(want, from, to) // the id, the clock and the actor are this run's; every other byte is the golden's
	}
	code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/design-systems", "")
	if code != 200 || string(mustJSON(got)) != want {
		t.Fatalf("the registry page:\n got %s\nwant %s", mustJSON(got), want)
	}
}

func TestAURLCarryingACredentialIsRefusedAndStoresNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, tc := range []struct{ body, want string }{
		{`{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://tomas:hunter@registry.meridian.example/ui"}`, "invalid_registry_url"},
		{`{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://registry.meridian.example/ui","tokens_url":"https://tomas:hunter@registry.meridian.example/ui/tokens.json"}`, "invalid_tokens_url"},
		{`{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://registry.meridian.example/ui","tokens_url":null}`, "invalid_tokens_url"}, // a null is not a URL; leave the key out
		{`{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"HTTPS://registry.meridian.example/ui"}`, "invalid_registry_url"},                 // the scheme as the contract spells it
		{`{"name":"Meridian UI","namespace":"@meridian/ui","registry_url":"https://:443/ui"}`, "invalid_registry_url"},                                      // a port is not a host
	} {
		if code, body := registerDesignSystem(t, c, base, tc.body); code != 400 || body["error"] != tc.want {
			t.Fatalf("%s: %d %s", tc.want, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM design_systems WHERE source_type = 'registry'"); n != 0 {
		t.Fatalf("%d row(s) registered: Canyon never stores a registry credential", n)
	}
}

func TestPlainHTTPIsALoopbackHostOnly(t *testing.T) {
	_, _, base, c := withProject(t)
	post := func(namespace, url string) (int, map[string]any) {
		return registerDesignSystem(t, c, base, fmt.Sprintf(`{"name":"Meridian UI","namespace":%q,"registry_url":%q}`, namespace, url))
	}
	if code, body := post("@meridian/ui", "http://registry.meridian.example/ui"); code != 400 || body["error"] != "invalid_registry_url" {
		t.Fatalf("http off a loopback host: %d %s", code, lastBody)
	}
	for i, url := range []string{"http://localhost:9000/ui", "http://127.0.0.1/ui"} { // with a port and without
		if code, _ := post(fmt.Sprintf("@local%d/ui", i), url); code != 201 {
			t.Fatalf("%s: %d %s", url, code, lastBody)
		}
	}
}

// An IP literal naming a private, link-local (the cloud metadata service), loopback,
// unspecified or IPv4-mapped address is refused before anything is stored: the URL is written
// into every builder's agent config, and the first server-side fetch of it would reach the
// org's own network. Only the literal is judged; no name is resolved.
func TestARegistryURLNamingAPrivateAddressIsRefused(t *testing.T) {
	_, _, base, c := withProject(t)
	post := func(namespace, url string) (int, map[string]any) {
		return registerDesignSystem(t, c, base, fmt.Sprintf(`{"name":"Meridian UI","namespace":%q,"registry_url":%q}`, namespace, url))
	}
	for _, url := range []string{"https://169.254.169.254/latest/meta-data/", "https://10.0.0.5/registry", "https://[::1]/x", "https://0.0.0.0/x", "https://[::ffff:10.0.0.5]/x"} {
		if code, body := post("@meridian/ui", url); code != 400 || body["error"] != "invalid_registry_url" {
			t.Fatalf("%s: %d %s", url, code, lastBody)
		}
	}
	for i, url := range []string{"http://localhost:4000", "https://registry.example"} {
		if code, _ := post(fmt.Sprintf("@public%d/ui", i), url); code != 201 {
			t.Fatalf("%s: %d %s", url, code, lastBody)
		}
	}
}

func TestOneAuditRowPerRegistrationAndNoneForTheRest(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	rows := func() int {
		return count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/design-systems'")
	}
	registerDesignSystem(t, c, base, meridian)
	if n := rows(); n != 1 {
		t.Fatalf("a registration wrote %d audit rows", n)
	}
	do(t, c, http.MethodGet, base+"/api/v1/design-systems", "")
	registerDesignSystem(t, c, base, meridian)                                                               // the 409
	registerDesignSystem(t, c, base, `{"name":"","namespace":"@x/ui","registry_url":"https://x.example/r"}`) // the 400
	if n := rows(); n != 1 {
		t.Fatalf("a read, a refused duplicate and a refused body took the count to %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM timeline_rows WHERE kind = 'audit.appended'"); n != 1 { // the project's create, and nothing from the registry: it belongs to the org
		t.Fatalf("expected one org timeline row, the project's create, and no more from the registry: got %d", n)
	}
}

func TestTheBoundsAndTheDoorsThatAreNotOpen(t *testing.T) {
	_, _, base, c := withProject(t)
	post := func(name, namespace string) (int, map[string]any) {
		return registerDesignSystem(t, c, base, string(mustJSON(map[string]any{"name": name, "namespace": namespace, "registry_url": "https://registry.meridian.example/ui"})))
	}
	if code, body := post(strings.Repeat("a", 65), "@meridian/ui"); code != 400 || body["error"] != "invalid_name" {
		t.Fatalf("a 65-character name: %d %s", code, lastBody)
	}
	if code, body := post("Meridian\tUI", "@meridian/ui"); code != 400 || body["error"] != "invalid_name" {
		t.Fatalf("a name carrying a control character: %d %s", code, lastBody)
	}
	if code, body := post("Meridian UI", "@"+strings.Repeat("n", 125)+"/ui"); code != 400 || body["error"] != "invalid_namespace" {
		t.Fatalf("a 129-character namespace: %d %s", code, lastBody)
	}
	if code, _ := post(strings.Repeat("é", 64), "@meridian/ui"); code != 201 { // 64 characters, 128 bytes
		t.Fatalf("a 64-character name with an accent: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/design-system", `{"design_system_id":"ds_0001"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("the per-project override is not routed in v1: %d %s", code, lastBody)
	}
}

// The body cannot name what the server decides: source_type, the id, who and when. A row is
// `registry` whatever the body says, its id is the server's, in the contract's shape.
func TestTheBodyCannotForgeWhatTheServerDecides(t *testing.T) {
	_, _, base, c := withProject(t)
	code, body := registerDesignSystem(t, c, base, `{"name":"Ours","namespace":"@ours/ui","registry_url":"https://ours.example/r","source_type":"shipped","id":"ds_0001","created_by":"root"}`)
	row, _ := body["design_system"].(map[string]any)
	if code != 201 || row["source_type"] != "registry" || row["created_by"] != "anonymous" {
		t.Fatalf("the server decides source_type, id and author: %d %s", code, lastBody)
	}
	if id, _ := row["id"].(string); !regexp.MustCompile(`^ds_[a-z0-9]{4,}$`).MatchString(id) || id == "ds_0001" {
		t.Fatalf("the id is the server's, in the contract's shape: %q", id)
	}
}

// The list is ordered by when a row was made, then its id — not by id alone, which the two
// shipped rows happen to satisfy. A newer row whose id sorts first still comes last.
func TestTheRegistryListsRowsByAgeThenId(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if _, err := pool.Exec(ctx, `INSERT INTO design_systems (id, name, namespace, source_type, registry_url, created_by) VALUES ('ds_0000', 'Early id, late row', '@late/ui', 'registry', 'https://late.example/r', 'anonymous')`); err != nil {
		t.Fatal(err)
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/design-systems", "")
	rows, _ := page["design_systems"].([]any)
	last, _ := rows[len(rows)-1].(map[string]any)
	if len(rows) != 3 || last["id"] != "ds_0000" {
		t.Fatalf("the newest row comes last whatever its id: %s", lastBody)
	}
}

// The upper bound admits exactly what it says: 128 code points of namespace is a namespace.
func TestANamespaceOfExactlyOneHundredAndTwentyEightCodePointsIsAccepted(t *testing.T) {
	_, _, base, c := withProject(t)
	ns := "@" + strings.Repeat("é", 126) + "/"
	if code, _ := registerDesignSystem(t, c, base, `{"name":"Wide","namespace":"`+ns+`","registry_url":"https://wide.example/r"}`); code != 201 {
		t.Fatalf("128 code points is within the bound: %d %s", code, lastBody)
	}
}
