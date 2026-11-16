package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
)

func componentTypesOf(t *testing.T, c *http.Client, base string) (int, map[string]any) {
	t.Helper()
	code, out, _ := do(t, c, http.MethodGet, base+"/api/v1/component-types", "")
	return code, out
}

// TestTheComponentMenuIsTheGoldenShape compares the answer byte for byte with the guardrails
// fixture's component_types object, the routes_skill_test.go pattern.
func TestTheComponentMenuIsTheGoldenShape(t *testing.T) {
	_, _, base, c := withProject(t)
	raw, err := os.ReadFile("../../contracts/testdata/guardrails/preferences.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	code, got := componentTypesOf(t, c, base)
	want := doc["component_types"]
	if code != 200 || string(mustJSON(got)) != string(mustJSON(want)) {
		t.Fatalf("the golden shape:\n got %d %s\nwant %s", code, mustJSON(got), mustJSON(want))
	}
}

// TestTheRowsAreExactlyTheSnapshotsMenuInItsOrder hands the handler a snapshot whose menu is
// reversed; the rows must follow the menu, not the table.
func TestTheRowsAreExactlyTheSnapshotsMenuInItsOrder(t *testing.T) {
	a := &app{snap: snapshot{ComponentMenu: []string{"s3", "postgres"}}}
	rec := httptest.NewRecorder()
	a.componentTypes(rec, httptest.NewRequest(http.MethodGet, "/component-types", nil))
	var out struct {
		Rows []componentType `json:"component_types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("GET /component-types: %d %s", rec.Code, rec.Body.String())
	}
	want := []string{"s3", "postgres"}
	if len(out.Rows) != len(want) {
		t.Fatalf("rows: want %v, got %s", want, rec.Body.String())
	}
	for i, name := range want {
		if out.Rows[i].Type != name {
			t.Fatalf("row %d: want %q (the menu's order), got %q", i, name, out.Rows[i].Type)
		}
	}
}

// TestAMenuNameTheServerCannotDescribeFailsTheBoot: a snapshot naming redis, which the table
// has no row for, is refused at load, naming redis.
func TestAMenuNameTheServerCannotDescribeFailsTheBoot(t *testing.T) {
	raw := `{"exclusions": ["a"], "component_menu": ["postgres", "redis"], "rules": [{"id": "prod.clean-gate", "name": "n", "state": "blocking"}, {"id": "prod.human-approval", "name": "n", "state": "blocking"}, {"id": "stack.blessed", "name": "n", "state": "advisory"}, {"id": "owner.required", "name": "n", "state": "advisory"}]}`
	snap, err := loadSnapshot([]byte(raw))
	if err == nil { // two assertions on purpose: one inverted condition cannot pass both
		t.Fatalf("a menu naming redis loaded: %+v", snap)
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Fatalf("the refusal must name redis: %v", err)
	}
	if rows, err := componentsOf([]string{"postgres", "postgres"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a menu naming a type twice is a defect, not two rows: %v %v", rows, err)
	}
	if _, err := componentsOf([]string{"postgres", "s3"}); err != nil {
		t.Fatalf("the menu the fixture carries must load: %v", err)
	}
}

// TestTheMenuRowsAndTheManifestNamesAreTwoShapesOfOneMenu: the manifest's component_types are
// the bare names of the rows this route answers, in the same order (routes.md rows 173 and 180).
func TestTheMenuRowsAndTheManifestNamesAreTwoShapesOfOneMenu(t *testing.T) {
	_, _, base, c := withProject(t)
	_, rows := componentTypesOf(t, c, base)
	_, man := skillManifest(t, c, base)
	list, ok := rows["component_types"].([]any)
	if !ok {
		t.Fatalf("the rows are not a list: %s", mustJSON(rows))
	}
	var names []any
	for _, r := range list {
		names = append(names, r.(map[string]any)["type"])
	}
	if string(mustJSON(names)) != string(mustJSON(man["component_types"])) || len(names) == 0 {
		t.Fatalf("one menu, two shapes: rows name %s, the manifest says %s", mustJSON(names), mustJSON(man["component_types"]))
	}
}

// TestTheMenuIsReadBehindTheSessionStageAndWritesNoAuditRow: a cookie-less caller is handed a
// session, and the read leaves the audit trail untouched.
func TestTheMenuIsReadBehindTheSessionStageAndWritesNoAuditRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	if code, _, _ := do(t, client(t), http.MethodGet, base+"/api/v1/component-types", ""); code != 200 {
		t.Fatalf("GET /component-types: %d %s", code, lastBody)
	}
	if after := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); after != before {
		t.Fatalf("a read writes no audit row: %d before, %d after", before, after)
	}
	withOIDC(t) // behind the session stage: where sessions are required, a caller without one is refused
	signIn, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodGet, signIn+"/api/v1/component-types", ""); code != 401 {
		t.Fatalf("GET /component-types without a session on a sign-in host: %d, want 401", code)
	}
}

// An empty menu is legitimate (snapshot.go) and answers an empty list, never null: a CLI ranges
// over the rows, and a null is the one shape that would make it choke.
func TestAnEmptyMenuAnswersAnEmptyListNeverNull(t *testing.T) {
	a := &app{snap: snapshot{ComponentMenu: []string{}}}
	rec := httptest.NewRecorder()
	a.componentTypes(rec, httptest.NewRequest(http.MethodGet, "/component-types", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"component_types":[]}` {
		t.Fatalf("an empty menu: %d %s", rec.Code, rec.Body.String())
	}
}

// A type is available exactly when this host has a driver for it in preview, and gives the env names its driver promises.
func TestATypeIsAvailableOnlyOnAHostThatProvisionsIt(t *testing.T) {
	rows, _ := componentsOf([]string{"postgres", "s3"})
	for _, row := range rows {
		if !slices.Equal(row.Gives, provisioners.Promised(row.Type)) || len(row.Gives) == 0 {
			t.Fatalf("%s gives %v, its driver promises %v", row.Type, row.Gives, provisioners.Promised(row.Type))
		}
	}
	if got := (&app{pipe: &runs.Pipeline{}}).offered(rows, false); got[0].Status != "planned" || got[1].Status != "planned" {
		t.Fatalf("no provisioning: %+v", got)
	}
	if got := (&app{pipe: &runs.Pipeline{Drivers: provisioners.Drivers{{"postgres", "preview"}: &provisioners.Builtin{}}}}).offered(rows, false); got[0].Status != "available" || got[1].Status != "planned" {
		t.Fatalf("provisioning postgres: %+v", got)
	}
	fresh, _ := componentsOf([]string{"postgres", "s3"}) // under run_by: org the platform provides every type, drivers or none (#2432)
	if got := (&app{pipe: &runs.Pipeline{}}).offered(fresh, true); got[0].Status != "available" || got[1].Status != "available" {
		t.Fatalf("under org: %+v", got)
	}
	adapter, err := provisioners.NewHTTP("https://storage.internal/canyon", writeToken(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for drivers, want := range map[*runs.Pipeline]string{
		{Drivers: provisioners.Drivers{{"s3", "prod"}: adapter}}:    "planned", // a deploy first needs preview
		{Drivers: provisioners.Drivers{{"s3", "preview"}: adapter}}: "available",
	} {
		rows, _ := componentsOf([]string{"postgres", "s3"})
		if got := (&app{pipe: drivers}).offered(rows, false); got[0].Status != "planned" || got[1].Status != want {
			t.Fatalf("provisioning s3 by an adapter, want s3 %s: %+v", want, got)
		}
	}
}

func writeToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("adapter-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
