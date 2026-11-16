package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5/pgxpool"
)

type poolAndCtx struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

func rowsOf(body map[string]any) []any {
	rows, _ := body["rows"].([]any)
	return rows
}

// seededRun is a project, a package and a run the tests append rows to, with no workflow behind it.
func seededRun(t *testing.T) (base string, c *http.Client, run string, pool poolAndCtx) {
	t.Helper()
	p, ctx, base, c := withProject(t)
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', 'claims-portal', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_7kd2mq', 'claims-portal', 'pkg_7d3e8f', 'running')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return base, c, "gr_7kd2mq", poolAndCtx{p, ctx}
}

func TestARunTimelinePagesItsRowsAndClosesWhenTheRunEnds(t *testing.T) {
	base, c, run, db := seededRun(t)
	for i := range 5 {
		if _, err := events.AppendRun(db.ctx, db.pool, run, "claims-portal", events.BuildLog, map[string]any{"line": "[builder] step " + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// page 1: two rows, next = the last id
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?limit=2", "")
	rows := rowsOf(body)
	if len(rows) != 2 || body["next"] == nil || rows[0].(map[string]any)["kind"] != "build.log" || rows[0].(map[string]any)["run"] != run {
		t.Fatalf("page 1: %s", lastBody)
	}
	next := int64(body["next"].(float64))
	if next != int64(rows[1].(map[string]any)["id"].(float64)) {
		t.Fatalf("next is the last id seen: %s", lastBody)
	}
	// page 2 after the cursor: the next two, none of the first
	_, body, _ = do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?after="+strconv.FormatInt(next, 10)+"&limit=2", "")
	if rows = rowsOf(body); len(rows) != 2 || int64(rows[0].(map[string]any)["id"].(float64)) <= next {
		t.Fatalf("page 2: %s", lastBody)
	}
	next = int64(body["next"].(float64))
	// page 3: the last row, and the run is still open so next is not null
	_, body, _ = do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?after="+strconv.FormatInt(next, 10)+"&limit=2", "")
	if rows = rowsOf(body); len(rows) != 1 || body["next"] == nil {
		t.Fatalf("page 3 while the run is open: %s", lastBody)
	}
	next = int64(body["next"].(float64))
	if _, err := db.pool.Exec(db.ctx, "UPDATE runs SET state = 'clean' WHERE id = $1", run); err != nil {
		t.Fatal(err)
	}
	if err := events.End(db.ctx, db.pool, run); err != nil {
		t.Fatal(err)
	}
	// after the end: nothing follows, and the poller is told so
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?after="+strconv.FormatInt(next, 10), ""); lastBody != `{"rows":[],"next":null}` {
		t.Fatalf("after the end: %s", lastBody)
	}
}

func TestARunTimelineOfAnUnknownRunIsNotFoundTheRoutesWriteNoAuditRowAndADeletedProjectsRunStaysReadable(t *testing.T) {
	base, c, run, db := seededRun(t)
	before := count(t, db.ctx, db.pool, "SELECT count(*) FROM audit_events")
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_nothing/timeline", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown run: %d %s", code, lastBody)
	}
	do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline", "")
	do(t, c, http.MethodGet, base+"/api/v1/timeline", "")
	if n := count(t, db.ctx, db.pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("the polls left %d audit rows", n-before)
	}
	// a soft-deleted project's runs stay readable (contracts/routes.md row 44): the run's timeline is not the project's
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", "")
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline", ""); code != 200 {
		t.Fatalf("the run of a deleted project: %d %s", code, lastBody)
	}
}

func TestABadCursorOrLimitIsRefusedAndTheLimitIsClamped(t *testing.T) {
	base, c, run, db := seededRun(t)
	for query, code := range map[string]string{"?after=x": "invalid_after", "?after=-1": "invalid_after", "?after=1.5": "invalid_after", "?after=%201": "invalid_after", "?after=99999999999999999999": "invalid_after", "?limit=y": "invalid_limit", "?limit=-1": "invalid_limit", "?limit=%2B": "invalid_limit"} {
		for _, path := range []string{"/gate-runs/" + run + "/timeline", "/timeline"} {
			if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1"+path+query, ""); status != 400 || body["error"] != code {
				t.Fatalf("%s%s: %d %s, want %s", path, query, status, lastBody, code)
			}
		}
	}
	for i := range 201 {
		if _, err := events.AppendRun(db.ctx, db.pool, run, "claims-portal", events.BuildLog, map[string]any{"line": strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]int{"": 100, "?limit=0": 100, "?limit=500": 200, "?limit=7": 7, "?after=0&limit=3": 3, "?after=%2B1&limit=3": 3} { // an explicit 0 is the start; a plus sign is an integer's
		if _, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline"+query, ""); len(rowsOf(body)) != want {
			t.Fatalf("%q: %d rows, want %d", query, len(rowsOf(body)), want)
		}
	}
	// a cursor past the last id: an empty page, and the cursor stays where the poller put it
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?after=1000000", ""); lastBody != `{"rows":[],"next":1000000}` {
		t.Fatalf("past the last id: %s", lastBody)
	}
}

func TestTheOrgTimelineServesTheFleetRowsInOrder(t *testing.T) {
	base, c, _, db := seededRun(t)
	// the project's creation is already on the feed (its audit row); two more from the API and one appended directly
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`)
	if _, err := events.AppendOrg(db.ctx, db.pool, "claims-portal", events.TilesUpdated, map[string]any{"tiles": map[string]any{"secrets_exposed": 2, "never_reviewed": 5, "unowned": 2, "stacks": 6}}); err != nil {
		t.Fatal(err)
	}
	_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/timeline", "")
	rows := rowsOf(body)
	if len(rows) != 3 || body["next"] == nil {
		t.Fatalf("the feed: %s", lastBody)
	}
	kinds := []string{}
	for _, r := range rows {
		row := r.(map[string]any)
		if _, has := row["run"]; has || row["scope"] != "org" || row["project"] != "claims-portal" {
			t.Fatalf("an org row carries no run key: %v", row)
		}
		kinds = append(kinds, row["kind"].(string))
	}
	if kinds[0] != "audit.appended" || kinds[1] != "audit.appended" || kinds[2] != "tiles.updated" {
		t.Fatalf("in order: %v", kinds)
	}
	last := int64(rows[2].(map[string]any)["id"].(float64))
	if _, _, _ = do(t, c, http.MethodGet, base+"/api/v1/timeline?after="+strconv.FormatInt(last, 10), ""); lastBody != `{"rows":[],"next":`+strconv.FormatInt(last, 10)+`}` {
		t.Fatalf("the feed never ends: next stays the cursor: %s", lastBody)
	}
}

// The real thing: a run started through the API, its timeline polled the way the CLI polls it,
// until the driver's terminal step closes the poll.
func TestARealRunsTimelineTellsItsStoryAndClosesAtTheVerdict(t *testing.T) {
	_, _, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	var kinds []string
	after := int64(0)
	for deadline := time.Now().Add(20 * time.Second); ; {
		_, body, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/"+run+"/timeline?after="+strconv.FormatInt(after, 10), "")
		for _, r := range rowsOf(body) {
			row := r.(map[string]any)
			kind, _ := row["kind"].(string)
			if data, _ := row["data"].(map[string]any); kind == "run.state" {
				kind += ":" + data["state"].(string)
			}
			kinds = append(kinds, kind)
		}
		if body["next"] == nil {
			break
		}
		after = int64(body["next"].(float64))
		if time.Now().After(deadline) {
			t.Fatalf("the poll never closed: %v", kinds)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if want := "run.state:pending,run.state:running,producer.sarif,producer.sarif,verdict,run.state:blocked"; strings.Join(kinds, ",") != want {
		t.Fatalf("the story:\n got %s\nwant %s", strings.Join(kinds, ","), want)
	}
}
