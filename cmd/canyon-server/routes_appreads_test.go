package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAppPage writes the rows behind contracts/testdata/app-page/claims-portal.json by hand,
// with the fixture's ids and instants (the seedGolden pattern): through the API each call would
// write its own ids, clock and audit rows. gr_4d71's verdict passed and its build failed after,
// so its counts stand beside failed/build; its starter was never recorded.
func seedAppPage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO projects (id, name, stack, created_at) VALUES ('claims-portal', 'claims-portal', 'Next.js 15 · Postgres', '2026-08-29T15:08:41Z');
INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_actor, label, created_at) VALUES
 ('pkg_9e40', 'claims-portal', '3b1f6f0a9d2c4e8b7a6f5e4d3c2b1a0f9e8d7c6b', 'c7d2e9a4b1f0836d5e2a9c4b7f1d0e3a6b8c9d2f', 1, 'upload', 'agent:claude-code/dana', 'v1.4', '2026-09-02T09:29:47Z'),
 ('pkg_7a1c', 'claims-portal', '8e2d4c6b0a1f3e5d7c9b2a4f6e8d0c1b3a5f7e9d', '1a9f3e7d5c2b8a4f6e0d1c3b5a7f9e2d4c6b8a0f', 1, 'upload', 'agent:claude-code/dana', 'v1.3', '2026-09-01T11:24:05Z'),
 ('pkg_5c02', 'claims-portal', 'f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5', '5d8c1b4a7f0e3d6c9b2a5f8e1d4c7b0a3f6e9d2c', 1, 'upload', 'dana@meridian.example', 'v1.2', '2026-08-29T15:10:33Z'),
 ('pkg_2b17', 'claims-portal', '9c8b7a6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b', '2e5b8d1a4c7f0e3b6d9a2c5f8e1b4d7a0c3f6e9b', 1, 'check', 'dana@meridian.example', NULL, '2026-08-29T15:09:40Z');
INSERT INTO runs (id, project_id, package_id, state, failure_reason, started_by_kind, started_by_subject, started_by_display, created_at, ended_at) VALUES
 ('gr_8f2a', 'claims-portal', 'pkg_9e40', 'blocked', NULL, 'agent', 'agent:claude-code/dana', 'Dana''s agent', '2026-09-02T09:30:40Z', '2026-09-02T09:31:12Z'),
 ('gr_6c12', 'claims-portal', 'pkg_7a1c', 'clean', NULL, 'agent', 'agent:claude-code/dana', 'Dana''s agent', '2026-09-01T11:25:02Z', '2026-09-01T11:26:40Z'),
 ('gr_4d71', 'claims-portal', 'pkg_5c02', 'failed', 'build', NULL, NULL, NULL, '2026-08-29T15:11:20Z', '2026-08-29T15:14:58Z'),
 ('gr_3e05', 'claims-portal', 'pkg_2b17', 'clean', NULL, 'person', 'dana@meridian.example', 'Dana Whitfield', '2026-08-29T15:09:41Z', '2026-08-29T15:10:02Z');
INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES
 ('run', 'gr_8f2a', 'claims-portal', 'verdict', 'verdict', '{"state": "blocked", "blocking": 1, "warnings": 3}'),
 ('run', 'gr_6c12', 'claims-portal', 'verdict', 'verdict', '{"state": "clean", "blocking": 0, "warnings": 1}'),
 ('run', 'gr_4d71', 'claims-portal', 'verdict', 'verdict', '{"state": "clean", "blocking": 0, "warnings": 1}'),
 ('run', 'gr_3e05', 'claims-portal', 'verdict', 'verdict', '{"state": "clean", "blocking": 0, "warnings": 1}');
INSERT INTO audit_events (id, ts, actor_kind, actor_subject, actor_display, action, project_id, run_id, detail) VALUES
 ('ev_c8a1', '2026-09-02T09:31:02Z', 'canyon', 'gate', 'Canyon', 'verdict', 'claims-portal', 'gr_8f2a', '{"package": "pkg_9e40", "state": "blocked", "blocking": 1, "warnings": 3}'),
 ('ev_7b40', '2026-09-01T11:26:01Z', 'canyon', 'gate', 'Canyon', 'verdict', 'claims-portal', 'gr_6c12', '{"package": "pkg_7a1c", "state": "clean", "blocking": 0, "warnings": 1}'),
 ('ev_4e19', '2026-08-29T15:12:30Z', 'canyon', 'gate', 'Canyon', 'verdict', 'claims-portal', 'gr_4d71', '{"package": "pkg_5c02", "state": "clean", "blocking": 0, "warnings": 1}'),
 ('ev_b7e2', '2026-09-02T09:30:40Z', 'agent', 'agent:claude-code/dana', 'Dana''s agent', 'POST /api/v1/projects/{id}/deploys', 'claims-portal', 'gr_8f2a', '{"package": "pkg_9e40"}'),
 ('ev_a91c', '2026-09-02T09:29:47Z', 'agent', 'agent:claude-code/dana', 'Dana''s agent', 'POST /api/v1/projects/{id}/packages', 'claims-portal', NULL, '{"package": "pkg_9e40", "label": "v1.4"}'),
 ('ev_77d0', '2026-09-01T11:26:40Z', 'agent', 'agent:claude-code/dana', 'Dana''s agent', 'deploy preview', 'claims-portal', 'gr_6c12', '{"package": "pkg_7a1c", "image_digest": "sha256:cfd86d789f71ac8e8eb75d90a61aedbcc2767236bc29255ecf3395c1886bd93d", "url": "http://claims-portal.preview.localhost:7777"}'),
 ('ev_6f3b', '2026-09-01T11:25:02Z', 'agent', 'agent:claude-code/dana', 'Dana''s agent', 'POST /api/v1/projects/{id}/deploys', 'claims-portal', 'gr_6c12', '{"package": "pkg_7a1c"}'),
 ('ev_5e18', '2026-09-01T11:24:05Z', 'agent', 'agent:claude-code/dana', 'Dana''s agent', 'POST /api/v1/projects/{id}/packages', 'claims-portal', NULL, '{"package": "pkg_7a1c", "label": "v1.3"}'),
 ('ev_4c07', '2026-08-29T15:11:20Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'POST /api/v1/projects/{id}/deploys', 'claims-portal', 'gr_4d71', '{"package": "pkg_5c02"}'),
 ('ev_3b9a', '2026-08-29T15:10:33Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'POST /api/v1/projects/{id}/packages', 'claims-portal', NULL, '{"package": "pkg_5c02", "label": "v1.2"}'),
 ('ev_3a5d', '2026-08-29T15:10:02Z', 'canyon', 'gate', 'Canyon', 'verdict', 'claims-portal', 'gr_3e05', '{"package": "pkg_2b17", "state": "clean", "blocking": 0, "warnings": 1}'),
 ('ev_3a2c', '2026-08-29T15:09:41Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'POST /api/v1/projects/{id}/gate-runs', 'claims-portal', 'gr_3e05', '{"package": "pkg_2b17"}'),
 ('ev_3a1b', '2026-08-29T15:09:40Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'POST /api/v1/projects/{id}/packages', 'claims-portal', NULL, '{"package": "pkg_2b17", "kind": "check"}'),
 ('ev_2a64', '2026-08-29T15:09:12Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'PUT /api/v1/projects/{id}/owner', 'claims-portal', NULL, '{"display": "Dana Whitfield"}'),
 ('ev_1f50', '2026-08-29T15:08:41Z', 'person', 'dana@meridian.example', 'Dana Whitfield', 'POST /api/v1/projects', 'claims-portal', NULL, '{}');
UPDATE audit_events SET via = 'canyon' WHERE id IN ('ev_c8a1', 'ev_7b40', 'ev_4e19', 'ev_77d0');
UPDATE audit_events SET via = 'cli', role = 'admin', session = 'ses_5f0c2e8a9b1d3f47', source = '203.0.113.24' WHERE id IN ('ev_b7e2', 'ev_a91c')`); err != nil {
		t.Fatal(err)
	}
}

func appPage(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/app-page/claims-portal.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// appLists are the app page's paged reads: the fixture key, the list key on the wire, the route, the query the fixture was walked with.
var appLists = []struct{ fixture, key, path, query string }{
	{"packages", "packages", "/projects/claims-portal/packages", "?limit=2"},
	{"gate_runs", "runs", "/projects/claims-portal/gate-runs", "?limit=2"},
	{"audit", "events", "/audit", "?limit=4&project=claims-portal"},
}

// walk follows next from the first page to the last, one page per call of visit.
func walk(t *testing.T, c *http.Client, url string, visit func(page map[string]any)) {
	t.Helper()
	for next := ""; ; {
		code, page, _ := do(t, c, http.MethodGet, url+next, "")
		if code != 200 {
			t.Fatalf("%s: %d %s", url+next, code, lastBody)
		}
		visit(page)
		s, ok := page["next"].(string)
		if !ok {
			if page["next"] != nil {
				t.Fatalf("next is a string or null: %s", lastBody)
			}
			return
		}
		next = "&cursor=" + s
	}
}

func TestAppPageListsAnswerTheGoldenPagesNextIncluded(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	c, doc := client(t), appPage(t)
	for _, list := range appLists {
		want, n := doc[list.fixture].([]any), 0
		walk(t, c, base+"/api/v1"+list.path+list.query, func(page map[string]any) {
			if n >= len(want) || !reflect.DeepEqual(page, want[n]) {
				t.Fatalf("%s page %d:\n got %s\nwant %s", list.fixture, n, lastBody, mustJSON(want[n:]))
			}
			n++
		})
		if n != len(want) {
			t.Fatalf("%s: walked %d pages, the fixture has %d", list.fixture, n, len(want))
		}
	}
}

// The audit read pages by number as the fleet does (contracts/routes.md, the audit read: total and page): page n is the
// golden walk's page n, past the last is the last, and every page answers the walk's total.
func TestTheAuditReadPagesByNumber(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	c, want := client(t), appPage(t)["audit"].([]any)
	for n, asked := range []int{1, 2, 3, 4, 9} {
		code, page, _ := do(t, c, http.MethodGet, fmt.Sprintf("%s/api/v1/audit?limit=4&project=claims-portal&page=%d", base, asked), "")
		golden := want[min(n, len(want)-1)].(map[string]any)
		if code != 200 || !reflect.DeepEqual(page["events"], golden["events"]) || page["total"] != golden["total"] || page["page"] != float64(min(asked, len(want))) {
			t.Fatalf("page=%d: %d %s\nwant the golden walk's page %d", asked, code, lastBody, min(asked, len(want)))
		}
	}
	for _, query := range []string{"page=0", "page=x", "page=2&cursor=" + want[0].(map[string]any)["next"].(string), "page=2&cursor=nope"} {
		if status, body, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?"+query, ""); status != 400 || body["error"] != "invalid_page" {
			t.Errorf("?%s: %d %v, want 400 invalid_page", query, status, body)
		}
	}
}

// Two rows made in the same microsecond, and two one microsecond apart inside one wire second:
// walked one row at a time, every id comes once. The first pair catches a cursor keyed on the
// timestamp alone, the second a cursor cut to the wire's whole second. One subtest per appLists
// entry that has a case below: packages, gate runs and the trail.
func TestAppListsPageThroughATimestampTieAndAcrossASecondWithoutRepeatOrGap(t *testing.T) {
	cases := map[string]struct {
		seed string
		want []string
	}{
		"packages": {
			seed: `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, created_at) VALUES
 ('pkg_same1', 'claims-portal', 't', 'c', 1, 'upload', '2026-08-30T10:00:00.123456Z'), ('pkg_same2', 'claims-portal', 't', 'c', 1, 'upload', '2026-08-30T10:00:00.123456Z'),
 ('pkg_usec1', 'claims-portal', 't', 'c', 1, 'upload', '2026-08-31T10:00:00.000001Z'), ('pkg_usec2', 'claims-portal', 't', 'c', 1, 'upload', '2026-08-31T10:00:00.000002Z')`,
			want: []string{"pkg_9e40", "pkg_7a1c", "pkg_usec2", "pkg_usec1", "pkg_same2", "pkg_same1", "pkg_5c02"},
		},
		"gate_runs": {
			seed: `INSERT INTO runs (id, project_id, package_id, state, created_at) VALUES
 ('gr_same1', 'claims-portal', 'pkg_5c02', 'clean', '2026-08-30T10:00:00.123456Z'), ('gr_same2', 'claims-portal', 'pkg_5c02', 'clean', '2026-08-30T10:00:00.123456Z'),
 ('gr_usec1', 'claims-portal', 'pkg_5c02', 'clean', '2026-08-31T10:00:00.000001Z'), ('gr_usec2', 'claims-portal', 'pkg_5c02', 'clean', '2026-08-31T10:00:00.000002Z')`,
			want: []string{"gr_8f2a", "gr_6c12", "gr_usec2", "gr_usec1", "gr_same2", "gr_same1", "gr_4d71", "gr_3e05"},
		},
		"audit": {
			seed: `INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, project_id) VALUES
 ('ev_same1', '2026-08-30T10:00:00.123456Z', 'canyon', 'sweeper', 'packages expired', 'claims-portal'), ('ev_same2', '2026-08-30T10:00:00.123456Z', 'canyon', 'sweeper', 'packages expired', 'claims-portal'),
 ('ev_usec1', '2026-08-31T10:00:00.000001Z', 'canyon', 'sweeper', 'packages expired', 'claims-portal'), ('ev_usec2', '2026-08-31T10:00:00.000002Z', 'canyon', 'sweeper', 'packages expired', 'claims-portal')`,
			want: []string{"ev_c8a1", "ev_b7e2", "ev_a91c", "ev_77d0", "ev_7b40", "ev_6f3b", "ev_5e18", "ev_usec2", "ev_usec1", "ev_same2", "ev_same1", "ev_4e19", "ev_4c07", "ev_3b9a", "ev_3a5d", "ev_3a2c", "ev_3a1b", "ev_2a64", "ev_1f50"},
		},
	}
	for _, list := range appLists {
		tc, ok := cases[list.fixture]
		if !ok {
			continue
		}
		t.Run(list.fixture, func(t *testing.T) {
			pool, ctx := migrated(t)
			base, _ := start(t, pool, time.Hour)
			seedAppPage(t, ctx, pool)
			if _, err := pool.Exec(ctx, tc.seed); err != nil {
				t.Fatal(err)
			}
			var walked []string
			walk(t, client(t), base+"/api/v1"+list.path+"?limit=1", func(page map[string]any) {
				for _, row := range page[list.key].([]any) {
					walked = append(walked, row.(map[string]any)["id"].(string))
				}
			})
			if !reflect.DeepEqual(walked, tc.want) {
				t.Fatalf("walking at limit=1:\n got %v\nwant %v", walked, tc.want)
			}
		})
	}
}

func TestAppPageListsRefuseABadLimitOrCursorAndClampAt200(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) SELECT 'pkg_x' || i, 'claims-portal', 't', 'c', 1, 'upload' FROM generate_series(1, 201) i;
INSERT INTO runs (id, project_id, package_id, state, ended_at) SELECT 'gr_x' || i, 'claims-portal', 'pkg_5c02', 'clean', now() FROM generate_series(1, 201) i;
INSERT INTO audit_events (id, actor_kind, actor_subject, action) SELECT 'ev_x' || i, 'canyon', 'sweeper', 'packages expired' FROM generate_series(1, 201) i`); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	for _, list := range appLists {
		path := base + "/api/v1" + list.path
		if code, page, _ := do(t, c, http.MethodGet, path+"?limit=500", ""); code != 200 || len(page[list.key].([]any)) != 200 || page["next"] == nil {
			t.Fatalf("%s?limit=500 is clamped to 200 rows with a next page: %d, %d rows, next %v", path, code, len(page[list.key].([]any)), page["next"])
		}
		for query, code := range map[string]string{
			"limit=0": "invalid_limit", "limit=ten": "invalid_limit", "cursor=nope": "invalid_cursor",
			"cursor=" + base64.RawURLEncoding.EncodeToString([]byte("yesterday|pkg_x")): "invalid_cursor", // ours in shape, not in content: refused before Postgres sees it
			"cursor=" + pageCursor{time.Now(), "a\x00b"}.String():                       "invalid_cursor", // a NUL id would otherwise reach Postgres as a 500
			"cursor=" + pageCursor{time.Now(), "a\xffb"}.String():                       "invalid_cursor", // so would an id that is not UTF-8
		} {
			if status, body, _ := do(t, c, http.MethodGet, path+"?"+query, ""); status != 400 || body["error"] != code {
				t.Errorf("%s?%s: %d %v, want 400 %s", path, query, status, body, code)
			}
		}
	}
}

func TestAppPageListsAnswer404ForAnUnknownOrDeletedProjectAndAnEmptyPageForABareOne(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"bare"}`)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	for _, list := range []struct{ path, empty string }{{"packages", `{"next":null,"packages":[]}`}, {"gate-runs", `{"next":null,"runs":[]}`}} {
		for _, id := range []string{"nothing", "gone"} {
			if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/"+id+"/"+list.path, ""); code != 404 || body["error"] != "not_found" {
				t.Errorf("%s of %s: %d %s", list.path, id, code, lastBody)
			}
		}
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/bare/"+list.path, ""); code != 200 || lastBody != list.empty {
			t.Errorf("%s of a project with none: %d %s", list.path, code, lastBody)
		}
	}
}

// A version with no label is named by its id, so its row carries no label key at all — not a null,
// which a screen would have to test for (contracts/routes.md, *Naming a version*).
func TestARunOfAnUnlabelledVersionCarriesNoLabelKey(t *testing.T) {
	_, _, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "package.json": "{}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	waitVerdict(t, c, base, run)
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	rows, _ := page["runs"].([]any)
	row, _ := rows[0].(map[string]any)
	if _, named := row["label"]; named || !strings.Contains(lastBody, `"package"`) {
		t.Fatalf("no label is no key: %s", lastBody)
	}
}

// A run started through the API carries the session's actor onto its row and the list reads it
// back; the counts beside it are the verdict's, the same numbers the single read answers.
func TestARunStartedThroughTheAPIListsItsStarterAndItsVerdictCounts(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "package.json": "{}\n"}), "", "")
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	run, _ := runOf(started)["id"].(string)
	single := waitVerdict(t, c, base, run)
	// a second run of the same project, seeded with its own different verdict: proves the counts
	// beside a row come from that row's own verdict, not any run's of the project
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_other0000', 'claims-portal', (SELECT package_id FROM runs WHERE id = $1), 'clean', now())", run); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_other0000', 'claims-portal', 'verdict', 'verdict', '{"state": "clean", "blocking": 0, "warnings": 5}')`); err != nil {
		t.Fatal(err)
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	rows, _ := page["runs"].([]any)
	if len(rows) != 2 {
		t.Fatalf("both runs: %s", lastBody)
	}
	byID := make(map[string]map[string]any, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		byID[row["id"].(string)] = row
	}
	row, ok := byID[run]
	if !ok {
		t.Fatalf("the started run is on the page: %s", lastBody)
	}
	if want := map[string]any{"kind": "person", "subject": "anonymous", "display": ""}; !reflect.DeepEqual(row["started_by"], want) {
		t.Fatalf("started_by is the session's actor: %v, want %v", row["started_by"], want)
	}
	if row["id"] != run || row["state"] != single["state"] || row["blocking"] != single["blocking"] || row["warnings"] != single["warnings"] || row["ended_at"] == nil {
		t.Fatalf("the row beside the single read:\n row %v\nread %v", row, single)
	}
	if other := byID["gr_other0000"]; other == nil || other["blocking"] != 0.0 || other["warnings"] != 5.0 {
		t.Fatalf("the other run reports its own verdict counts, not the started run's: %v", other)
	}
}

// A deploy run mid-build already has its verdict's counts while still running — the verdict is
// written before the build (contracts/schema/app-page.schema.json zeroes counts for pending only);
// pinned so nobody "fixes" it to zero.
func TestAGateRunListsItsVerdictCountsWhileStillRunning(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, created_at) VALUES ('gr_running000', 'claims-portal', 'pkg_9e40', 'running', '2026-09-03T09:00:00Z');
INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_running000', 'claims-portal', 'verdict', 'verdict', '{"state": "blocked", "blocking": 1, "warnings": 2}')`); err != nil {
		t.Fatal(err)
	}
	_, page, _ := do(t, client(t), http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs?limit=1", "")
	rows, _ := page["runs"].([]any)
	row, _ := rows[0].(map[string]any)
	if row["id"] != "gr_running000" || row["state"] != "running" || row["blocking"] != 1.0 || row["warnings"] != 2.0 {
		t.Fatalf("a run mid-build already carries its verdict's counts: %s", lastBody)
	}
}

// The trail never 404s: a project id that matches nothing, a deleted project's, an id of no
// shape at all — each is a page, and the deleted project's rows are still on it. ts is whole
// seconds however the row was stamped.
func TestAuditPageNarrowsByProjectNever404sAndAnswersWholeSeconds(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	for query, want := range map[string]int{"project=gone": 2, "project=nothing": 0, "project=Not%20an%20id": 0, "project=claims-portal&actor=person": 6, "project=claims-portal&since=2026-09-02T00:00:00Z": 3} {
		if code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?"+query, ""); code != 200 || len(page["events"].([]any)) != want {
			t.Errorf("?%s: %d %s, want %d events", query, code, lastBody, want)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, project_id) VALUES ('ev_usec', '2030-05-05T05:05:40.123456Z', 'canyon', 'sweeper', 'packages expired', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	if _, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?project=claims-portal&limit=1", ""); len(ids(t, map[string]any{"apps": page["events"]})) != 1 || page["events"].([]any)[0].(map[string]any)["ts"] != "2030-05-05T05:05:40Z" {
		t.Fatalf("a row stamped to the microsecond answers whole seconds: %s", lastBody)
	}
}

// A rule's history is the changes to it alone: its policy edits and, for a custom rule, its saved
// versions, never another rule's and never a row that merely mentions it.
func TestAuditPageNarrowsToOneRulesChanges(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for at, row := range [][3]string{ // in the order they happened
		{"ev_p1", "policy.set", `{"rule":"sql.string-concat","before":"blocking","after":"advisory"}`},
		{"ev_p2", "policy.set", `{"rule":"deps.known-vuln","before":"advisory","after":"blocking"}`},
		{"ev_c1", "custom_rule.set", `{"rule":"custom.no-pii","version":1}`},
		{"ev_x1", "approve", `{"rule":"sql.string-concat"}`},
		{"ev_c2", "policy.set", `{"rule":"custom.no-pii","before":"off","after":"advisory"}`},
	} {
		if _, err := pool.Exec(ctx, "INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action, detail) VALUES ($1, now() + make_interval(secs => $4), 'person', 'dana@meridian.example', $2, $3)", row[0], row[1], row[2], at); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string][]string{
		"rule=sql.string-concat":          {"ev_p1"},
		"rule=custom.no-pii":              {"ev_c2", "ev_c1"}, // its versions and its state, newest first
		"rule=custom.no-pii&limit=1":      {"ev_c2"},
		"rule=custom.no-pii&actor=canyon": {},
		"rule=nothing":                    {},
	} {
		code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?"+query, "")
		got := []string{}
		for _, e := range page["events"].([]any) {
			got = append(got, e.(map[string]any)["id"].(string))
		}
		if code != 200 || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("?%s: %d %v, want %v", query, code, got, want)
		}
	}
}

// The trail narrows on the server by category, action, actor subject and an exclusive until, every
// filter with the others, and each row carries its action's category: an action routes.md's table does
// not name is governance (contracts/routes.md, Audit categories).
func TestAuditPageNarrowsByCategoryActionSubjectAndUntil(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for at, row := range [][3]string{ // in the order they happened
		{"ev_in", "sign in", "dana@meridian.example"},
		{"ev_up", "POST /api/v1/projects/{id}/packages", "dana@meridian.example"},
		{"ev_vd", "verdict", "gate"},
		{"ev_pr", "deploy prod", "sam@meridian.example"},
		{"ev_nw", "a new action", "sam@meridian.example"},
	} {
		if _, err := pool.Exec(ctx, "INSERT INTO audit_events (id, ts, actor_kind, actor_subject, action) VALUES ($1, '2026-09-01T10:00:00Z'::timestamptz + make_interval(mins => $4), 'person', $3, $2)", row[0], row[1], row[2], at); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]string{
		"":                                   "ev_nw:governance ev_pr:governance ev_vd:outcome ev_up:governance ev_in:access",
		"category=access":                    "ev_in:access",
		"category=outcome,access":            "ev_vd:outcome ev_in:access",
		"category=governance":                "ev_nw:governance ev_pr:governance ev_up:governance",
		"category=governance,outcome,access": "ev_nw:governance ev_pr:governance ev_vd:outcome ev_up:governance ev_in:access",
		"category=governance,access":         "ev_nw:governance ev_pr:governance ev_up:governance ev_in:access",
		"category=governance&limit=2":        "ev_nw:governance ev_pr:governance",
		"subject=":                           "ev_nw:governance ev_pr:governance ev_vd:outcome ev_up:governance ev_in:access",
		"action=deploy%20prod,verdict":       "ev_pr:governance ev_vd:outcome",
		"action=nothing":                     "",
		"subject=sam@meridian.example":       "ev_nw:governance ev_pr:governance",
		"until=2026-09-01T10:02:00Z":         "ev_up:governance ev_in:access",
		"since=2026-09-01T10:02:00Z&until=2026-09-01T10:02:00Z":                        "",
		"category=governance&subject=dana@meridian.example&until=2026-09-01T10:03:00Z": "ev_up:governance",
	} {
		code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?"+query, "")
		got := []string{}
		for _, e := range page["events"].([]any) {
			got = append(got, fmt.Sprintf("%s:%s", e.(map[string]any)["id"], e.(map[string]any)["category"]))
		}
		if code != 200 || strings.Join(got, " ") != want {
			t.Errorf("?%s: %d %v, want %q", query, code, got, want)
		}
	}
	for query, want := range map[string]string{
		"category=audit": "invalid_category",
		"category=":      "",
		"action=" + strings.Repeat("a,", 64) + "a": "invalid_action",
		"action=" + strings.Repeat("a", 201):       "invalid_action",
		"action=a,,b":                              "invalid_action",
		"subject=" + strings.Repeat("s", 321):      "invalid_subject",
		"until=yesterday":                          "invalid_until",
		"subject=a%00b":                            "invalid_subject",
		"action=%FF":                               "invalid_action",
	} {
		code, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?"+query, "")
		if want == "" && code != 200 || want != "" && (code != 400 || page["error"] != want) {
			t.Errorf("?%.40s: %d %s, want %q", query, code, lastBody, want)
		}
	}
}

// Every action routes.md's Audit categories table names is in its category here, read from the file, and
// every action this server places outside governance is in the table.
func TestAuditCategoriesAreRoutesMds(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/routes.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(raw), "### Audit categories")
	rows := regexp.MustCompile("(?m)^\\| `([^`]+)` \\| (governance|access|outcome) \\|$").FindAllStringSubmatch(table, -1)
	if len(rows) == 0 {
		t.Fatal("routes.md has no Audit categories table")
	}
	named := map[string]bool{}
	for _, row := range rows {
		named[row[1]] = true
		if got := categoryOf(row[1]); got != row[2] {
			t.Errorf("%s is %s here; routes.md says %s", row[1], got, row[2])
		}
	}
	for action := range categories {
		if !named[action] {
			t.Errorf("%s has a category here that routes.md's table does not name", action)
		}
	}
}

// Each row says how and from where it was done: a browser session's request via the dashboard, with the
// person's role, the session's digest — never its secret — and the client address; a row via canyon reads
// as one (events_test proves the write's default). Which session and from where are an admin's to read;
// via narrows the trail (contracts/routes.md, the audit read).
func TestAuditRowsSayHowAndFromWhere(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	erin := signedIn(t, ctx, pool, base, "sub-erin", "erin@meridian.example", false)
	if code, _, _ := do(t, erin, http.MethodPost, base+"/api/v1/projects", `{"id":"erins"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	var secret string
	if err := pool.QueryRow(ctx, "SELECT id FROM sessions WHERE actor_subject = 'sub-erin'").Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO audit_events (id, actor_kind, actor_subject, action, via) VALUES ('ev_wf', 'canyon', 'gate', 'verdict', 'canyon')"); err != nil {
		t.Fatal(err)
	}
	row := func(c *http.Client, query string) map[string]any {
		t.Helper()
		_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?action=POST+/api/v1/projects"+query, "")
		events, _ := page["events"].([]any)
		if len(events) != 1 {
			t.Fatalf("?%s: %s", query, lastBody)
		}
		return events[0].(map[string]any)
	}
	got := row(dana, "")
	if got["via"] != "dashboard" || got["role"] != "member" || got["session"] != sessionDigest(secret) || got["source"] != "127.0.0.1" || strings.Contains(lastBody, secret) {
		t.Fatalf("an admin's read of a browser session's row: %v", got)
	}
	if got := row(erin, ""); got["via"] != "dashboard" || got["session"] != nil || got["source"] != nil {
		t.Fatalf("a member's read: %v; which session and from where are an admin's", got)
	}
	for query, want := range map[string]int{"via=dashboard": 1, "via=canyon": 1, "via=cli": 0} {
		_, page, _ := do(t, dana, http.MethodGet, base+"/api/v1/audit?"+query, "")
		if n := len(page["events"].([]any)); n != want {
			t.Errorf("?%s: %d rows, want %d", query, n, want)
		}
	}
	if code, body, _ := do(t, dana, http.MethodGet, base+"/api/v1/audit?via=email", ""); code != 400 || body["error"] != "invalid_via" {
		t.Fatalf("an unknown via: %d %s", code, lastBody)
	}
}
