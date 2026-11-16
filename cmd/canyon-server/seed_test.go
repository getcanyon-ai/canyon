package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate/finding"
)

func TestSeedWritesTheRosterTheRoutesCanRead(t *testing.T) {
	t.Setenv("CANYON_DEMO_SEED", "1")
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if seeded, err := seedDemo(ctx, pool); err != nil || !seeded {
		t.Fatalf("seedDemo on an empty database: %v, %v", seeded, err)
	}
	for q, want := range map[string]int{ // no live run: the boot sweep would end one, so the roster carries none
		"SELECT count(*) FROM projects": 7, "SELECT count(*) FROM project_access": 6, "SELECT count(*) FROM packages": 9,
		"SELECT count(*) FROM runs": 7, "SELECT count(*) FROM deploys": 4, "SELECT count(*) FROM timeline_rows": 2, "SELECT count(*) FROM audit_events": 21,
		"SELECT count(*) FROM runs WHERE state IN ('pending', 'running')": 0,
	} {
		if got := count(t, ctx, pool, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	c := client(t)
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	got := map[string]any{}
	for _, r := range page["runs"].([]any) {
		got[r.(map[string]any)["id"].(string)] = r.(map[string]any)["blocking"]
	}
	if want := map[string]any{"gr_8f2a": 1.0, "gr_6c12": 0.0}; !reflect.DeepEqual(got, want) { // the fleet calls claims-portal blocked; the run list must say why
		t.Errorf("the blocking counts: %v, want %v", got, want)
	}
	for _, id := range []string{"gr_8f2a", "gr_3a07"} { // every blocked run says what blocked it, never "Blocked — 0 blocking findings"
		if _, run, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+id, ""); run["run"].(map[string]any)["blocking"] != 1.0 || len(run["run"].(map[string]any)["findings"].([]any)) != 1 {
			t.Errorf("the run read of %s: %s", id, lastBody)
		}
	}
	if _, trail, _ := do(t, c, http.MethodGet, base+"/api/v1/audit", ""); len(trail["events"].([]any)) == 0 { // the story's history, beside its 30-day versions
		t.Errorf("the demo's audit trail: %s", lastBody)
	}
}
func TestSeedLeavesAHostThatIsNotTheDemoEmpty(t *testing.T) {
	pool, ctx := migrated(t)
	for _, v := range []string{"", "0", "true"} { // the demo compose file sets 1; a company's own host sets nothing
		t.Setenv("CANYON_DEMO_SEED", v)
		if seeded, err := seedDemo(ctx, pool); seeded || err != nil {
			t.Fatalf("CANYON_DEMO_SEED=%q: %v, %v", v, seeded, err)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects"); n != 0 {
		t.Fatalf("%d project(s) on a host that asked for none", n)
	}
}

func TestSeedRunsOnceAcrossRestarts(t *testing.T) {
	t.Setenv("CANYON_DEMO_SEED", "1")
	pool, ctx := migrated(t)
	if _, err := seedDemo(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if seeded, err := seedDemo(ctx, pool); err != nil || seeded { // the volume outlives `down` and the roster's ids are fixed
		t.Fatalf("the second boot: %v, %v", seeded, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now()"); err != nil {
		t.Fatal(err)
	}
	if seeded, err := seedDemo(ctx, pool); err != nil || seeded { // a deleted project still holds its primary key
		t.Fatalf("a database whose projects are all soft-deleted: %v, %v", seeded, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM projects"); n != 7 {
		t.Fatalf("%d project(s) after three boots, want the roster's 7", n)
	}
}

func TestSeedFailsClosedWhenTheRosterCannotBeApplied(t *testing.T) {
	t.Setenv("CANYON_DEMO_SEED", "1")
	pool, ctx := migrated(t)
	broken := strings.Replace(demoSeed, "('fnol-intake', 'fnol-intake',", "('fnol-intak', 'fnol-intake',", 1) // its owner row now points at no project
	if seeded, err := seedWith(ctx, pool, broken); broken == demoSeed || err == nil || seeded {
		t.Fatalf("a roster the schema refuses is an error the boot dies on: %v, %v", seeded, err)
	}
}

// Every finding the demo seed carries is a finding the contract allows (contracts/finding.md,
// Profile rules): the dashboard renders a seeded result as it renders a real one, so a seeded
// result missing its location takes the app page down (canyonv2-5r11.19).
func TestEverySeededFindingIsAContractFinding(t *testing.T) {
	pool, ctx := migrated(t)
	seedGolden(t, ctx, pool)
	rows, err := pool.Query(ctx, "SELECT run_id, data->'findings' FROM timeline_rows WHERE kind = 'verdict'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var run string
		var raw []byte
		if err := rows.Scan(&run, &raw); err != nil {
			t.Fatal(err)
		}
		var log finding.Log
		if err := contracts.CheckFindingSchemas(raw); err != nil {
			t.Errorf("%s's verdict: %v", run, err)
		} else if err := json.Unmarshal(raw, &log); err != nil {
			t.Errorf("%s's verdict: %v", run, err)
		} else if err := log.Validate(); err != nil {
			t.Errorf("%s's verdict: %v", run, err)
		}
		n++
	}
	if err := rows.Err(); err != nil || n == 0 {
		t.Fatalf("the seed's verdicts: %d read, %v", n, err)
	}
}
