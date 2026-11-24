package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// A stored report with counts no report may carry — 1e5, 3000000000, "x" — leaves the weekly read answering: the
// counts within 0 to 1,000,000 are summed, the rest left out and counted as skipped (#2808's review: one such report
// once failed every weekly read).
func TestTheWeeklyReadSurvivesBadCounts(t *testing.T) {
	s, ctx := stored(t)
	good := with(golden(t), func(map[string]any) {})
	bad := `{"instance_id": "33333333-3333-4333-8333-333333333333", "milestones": ["first_check"], "checks": {"passed": "x", "advisory": 1e5, "blocked": 3000000000},
		"unsupported": {"language_unrecognised": 1e5, "own_dockerfile_ignored": 3000000000}, "rules_fired": {"custom": "x"}}`
	for _, body := range []string{string(good), bad, `{"instance_id": 7, "unsupported": "x", "rules_fired": [1]}`} {
		if _, err := s.pool.Exec(ctx, "INSERT INTO reports (report) VALUES ($1)", body); err != nil { // as a report the collector would never keep
			t.Fatal(err)
		}
	}
	weeks, err := s.weekly(ctx, 1)
	if err != nil || len(weeks) != 1 {
		t.Fatalf("the weekly read: %v %+v", err, weeks)
	}
	w := weeks[0]
	if !reflect.DeepEqual(w.Unsupported, []named{{"language_unrecognised", 100002}, {"own_dockerfile_ignored", 1}}) ||
		!reflect.DeepEqual(w.RulesFired, []named{{"secrets.client-bundle", 2}, {"custom", 1}, {"sql.string-concat", 1}}) || w.Skipped != 2 || w.Installs != 3 {
		got, _ := json.MarshalIndent(w, "", "  ")
		t.Errorf("the week:\n%s", got)
	}
}

// stored is a Postgres store over a fresh schema with the collector's table.
func stored(t *testing.T) (pgStore, context.Context) {
	t.Helper()
	pool, ctx := pgtest.Schema(t)
	if _, err := pool.Exec(ctx, "SET TIME ZONE 'UTC'; "+tableSQL); err != nil {
		t.Fatal(err)
	}
	return pgStore{pool}, ctx
}

// The table holds a report and when it arrived, and columns read from those two (#2953), nothing else: a report
// posted through the collector from an address lands without it, and no column could hold one.
func TestTheDatabaseHoldsNoAddress(t *testing.T) {
	s, ctx := stored(t)
	c, _, logged := testCollector(t)
	c.store = s
	post(c, with(golden(t), func(map[string]any) {}), "198.51.100.9")
	var columns, all string
	if err := s.pool.QueryRow(ctx, `SELECT string_agg(column_name, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name = 'reports' AND table_schema = current_schema()`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(string_agg(r::text, ''), '') FROM reports r`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if columns != "id,received_at,report,instance_id,day,version,install_mode,demo" || !strings.Contains(all, "7c9e6679-7425-40de-944b-e07fc1f90ae7") {
		t.Fatalf("columns %q, rows %s", columns, all)
	}
	for _, ip := range []string{"198.51.100.9", "10.0.0.2"} {
		if strings.Contains(all, ip) || strings.Contains(logged.String(), ip) {
			t.Errorf("%s reached the database or a log:\n%s\n%s", ip, all, logged)
		}
	}
}

// The weekly answers from a fixture of two installs that both first reported in one week: one still reviewing a
// week on and not four, the other four weeks on and not one; their milestones; and the summed unsupported and rules.
func TestTheWeeklyNumbersFromAFixture(t *testing.T) {
	s, ctx := stored(t)
	now := time.Now().UTC()
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monday = monday.AddDate(0, 0, -((int(monday.Weekday())+6)%7)-6*7) // six weeks before this week's Monday
	report := golden(t)
	fixture := []struct {
		at  time.Time
		set func(map[string]any)
	}{
		{monday.AddDate(0, 0, 1), func(m map[string]any) {
			m["instance_id"], m["milestones"], m["checks"] = "11111111-1111-4111-8111-111111111111", []string{"first_check"}, map[string]int{"passed": 1, "advisory": 0, "blocked": 0}
			m["unsupported"], m["rules_fired"] = map[string]int{"language_unrecognised": 2}, map[string]int{"custom": 3}
		}},
		{monday.AddDate(0, 0, 1+8), func(m map[string]any) {
			m["instance_id"], m["checks"], m["unsupported"], m["rules_fired"] = "11111111-1111-4111-8111-111111111111", map[string]int{"passed": 0, "advisory": 0, "blocked": 1}, map[string]int{}, map[string]int{}
		}},
		{monday.AddDate(0, 0, 1+29), func(m map[string]any) {
			m["instance_id"], m["checks"], m["unsupported"], m["rules_fired"] = "11111111-1111-4111-8111-111111111111", map[string]int{"passed": 0, "advisory": 0, "blocked": 0}, map[string]int{}, map[string]int{}
		}},
		{monday.AddDate(0, 0, 2), func(m map[string]any) {
			m["instance_id"], m["milestones"], m["checks"] = "22222222-2222-4222-8222-222222222222", []string{"first_check", "first_block", "first_fix"}, map[string]int{"passed": 0, "advisory": 0, "blocked": 0}
			m["unsupported"], m["rules_fired"] = map[string]int{"language_unrecognised": 1, "own_dockerfile_ignored": 1}, map[string]int{"secrets.client-bundle": 1, "custom": 1}
		}},
		{monday.AddDate(0, 0, 2+30), func(m map[string]any) {
			m["instance_id"], m["checks"], m["unsupported"], m["rules_fired"] = "22222222-2222-4222-8222-222222222222", map[string]int{"passed": 0, "advisory": 2, "blocked": 0}, map[string]int{}, map[string]int{}
		}},
	}
	schema, err := compileSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixture {
		body := with(report, f.set)
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil || schema.Validate(doc) != nil {
			t.Fatalf("a fixture report the schema refuses: %v %s", schema.Validate(doc), body)
		}
		if _, err := s.pool.Exec(ctx, "INSERT INTO reports (received_at, report) VALUES ($1, $2)", f.at, body); err != nil {
			t.Fatal(err)
		}
	}
	weeks, err := s.weekly(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	var first *week
	for i := range weeks {
		if weeks[i].Week == monday.Format("2006-01-02") {
			first = &weeks[i]
		}
	}
	want := week{Week: monday.Format("2006-01-02"), Installs: 2, NewInstalls: 2, Funnel: map[string]int{"first_check": 2, "first_block": 1, "first_fix": 1},
		Deploying:  map[string]ratio{"1w": {Of: 2, Deploying: 1}, "4w": {Of: 2, Deploying: 1}},
		Retention:  map[string]kept{"1w": {Of: 2, Reported: 1, Deploying: 1}, "4w": {Of: 2, Reported: 2, Deploying: 1}, "12w": {Of: 2}},
		InstallsBy: map[string][]named{"version": {{"v0.9.0", 2}}, "install_mode": {{"helm_one_node", 2}}, "platform": {{"linux/arm64", 2}}},
		Sums: sums{Deploys: map[string]int{"preview": 0, "prod": 0}, Checks: map[string]int{"passed": 1, "advisory": 0, "blocked": 0},
			AgentsVia: map[string]int{"cli": 0, "mcp_local": 0, "mcp_remote": 0}, Failures: []named{},
			Handoff: map[string]int{"deployed": 0, "failed": 0, "refused": 0, "undelivered": 0}, Fixes: 4},
		People: map[string]spread{"d1": {}, "d7": {}, "d28": {}}, Features: map[string]share{}, First: map[string]map[string]int{}, // first-version reports carry none
		Unsupported: []named{{"language_unrecognised", 3}, {"own_dockerfile_ignored", 1}}, RulesFired: []named{{"custom", 4}, {"secrets.client-bundle", 1}}}
	if first == nil || !reflect.DeepEqual(*first, want) {
		got, _ := json.MarshalIndent(weeks, "", "  ")
		t.Errorf("the fixture's week:\n%s\nwant %+v", got, want)
	}
}

// An instance's report is kept unless its last arrived under 20 hours ago: one resent or replayed is dropped and the
// first kept, so the week's sums count it once (#2808's review). A sender's two, 23 hours apart, are both kept, on one
// UTC day too; another instance's own is never dropped for them.
func TestAnInstancesReportIsKeptAgainAfterTwentyHours(t *testing.T) {
	s, ctx := stored(t)
	first := with(golden(t), func(m map[string]any) { m["unsupported"] = map[string]int{"language_unrecognised": 2} })
	again := with(golden(t), func(m map[string]any) { m["unsupported"] = map[string]int{"language_unrecognised": 5} })
	other := with(golden(t), func(m map[string]any) { m["instance_id"] = "11111111-1111-4111-8111-111111111111" })
	for i, step := range []struct {
		later time.Duration // since the step before
		body  []byte
		kept  bool
	}{{0, first, true}, {0, again, false}, {0, first, false}, {0, other, true}, {19*time.Hour + 59*time.Minute, again, false}, {3*time.Hour + time.Minute, again, true}, {23 * time.Hour, first, true}, {time.Hour, first, false}} {
		if _, err := s.pool.Exec(ctx, "UPDATE reports SET received_at = received_at - $1::interval", step.later); err != nil {
			t.Fatal(err)
		}
		if kept, err := s.save(ctx, step.body); err != nil || kept != step.kept {
			t.Fatalf("step %d: kept %v (%v), want %v", i, kept, err, step.kept)
		}
	}
	// the first two 23 hours apart, as a UTC day's 00:30 and 23:30
	if _, err := s.pool.Exec(ctx, `UPDATE reports SET received_at = '2026-06-01T00:30:00Z'::timestamptz + (n - 1) * interval '23 hours'
		FROM (SELECT id, row_number() OVER (ORDER BY received_at) AS n FROM reports WHERE instance_id <> '11111111-1111-4111-8111-111111111111') o WHERE o.id = reports.id AND n <= 2`); err != nil {
		t.Fatalf("two reports of one instance on one UTC day: %v", err)
	}
	var sum int
	if err := s.pool.QueryRow(ctx, "SELECT sum((report->'unsupported'->>'language_unrecognised')::int) FROM reports WHERE day = '2026-06-01'").Scan(&sum); err != nil || sum != 7 {
		t.Errorf("that day's sum over the two: %d %v, want 7", sum, err)
	}
	if n := countRows(t, ctx, s); n != 4 {
		t.Errorf("%d reports kept, want 4", n)
	}
}

// Reports of one instance that arrive at once keep one between them, whichever instance's.
func TestReportsAtOnceKeepOne(t *testing.T) {
	s, ctx := stored(t)
	for n := range 10 {
		body := with(golden(t), func(m map[string]any) { m["instance_id"] = fmt.Sprintf("1111111%d-1111-4111-8111-111111111111", n) })
		var kept atomic.Int32
		var senders sync.WaitGroup
		start := make(chan struct{})
		for range 4 {
			senders.Go(func() {
				<-start
				if ok, err := s.save(ctx, body); err != nil {
					t.Error(err)
				} else if ok {
					kept.Add(1)
				}
			})
		}
		close(start)
		if senders.Wait(); kept.Load() != 1 || countRows(t, ctx, s) != n+1 {
			t.Fatalf("four at once from instance %d: %d kept, %d stored, want 1 and %d", n, kept.Load(), countRows(t, ctx, s), n+1)
		}
	}
}

// The dropped table holds days, reasons and counts, nothing of a report or its sender; a later flush adds to its day's
// row, and the rows past 13 months go as the reports do. A collector that had the unique index starts over it.
func TestTheDroppedTableHoldsCountsAlone(t *testing.T) {
	s, ctx := stored(t)
	c, _, logged := testCollector(t)
	c.store = s
	for range 2 {
		post(c, []byte(`{"hostname": "db.internal", "instance_id": "7c9e6679-7425-40de-944b-e07fc1f90ae7"}`), "198.51.100.9")
		post(c, []byte("STRIPE_KEY=hunter2"), "198.51.100.9")
		if err := c.drops.flush(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	var columns, rows string
	if err := s.pool.QueryRow(ctx, `SELECT string_agg(column_name, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name = 'dropped' AND table_schema = current_schema()`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT coalesce(string_agg(concat_ws(' ', (now() AT TIME ZONE 'UTC')::date - day, reason, n), ',' ORDER BY reason), '') FROM dropped`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if got := read(); columns != "day,reason,n" || got != "0 not_json 2,0 not_schema 2" || logged.Len() != 0 {
		t.Errorf("columns %q, rows %q, logged %q", columns, got, logged.String())
	}
	if _, err := s.pool.Exec(ctx, "UPDATE dropped SET day = day - 500 WHERE reason = 'not_json'"); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "0 not_schema 2" {
		t.Errorf("after the prune: %q", got)
	}
	if _, err := s.pool.Exec(ctx, `CREATE UNIQUE INDEX reports_one_a_day ON reports ((report->>'instance_id'), ((received_at AT TIME ZONE 'UTC')::date)); `+tableSQL); err != nil {
		t.Fatal(err)
	}
	var old bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass('reports_one_a_day') IS NOT NULL").Scan(&old); err != nil || old {
		t.Errorf("the collector before's unique index is still there: %v %v", old, err)
	}
}

func countRows(t *testing.T, ctx context.Context, s pgStore) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM reports").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The second version's numbers from a fixture of five installs, fourteen weeks back so 12 weeks on has passed (#2953):
// A and B new that week, A still reporting and reviewing a week on and reporting 12 weeks on; E reporting first at
// a month old, so an install but not a new one; a demo, counted apart and in nothing else; and D, under a day old, in
// no install count, though its period counts are summed. People, features and first come from each install's last
// report, of the week or ever.
func TestTheWeeklyCountsTheSecondVersion(t *testing.T) {
	s, ctx := stored(t)
	now := time.Now().UTC()
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monday = monday.AddDate(0, 0, -((int(monday.Weekday())+6)%7)-14*7)
	const a, b, c, d, e = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	quiet := func(m map[string]any) { m["checks"] = map[string]int{"passed": 0, "advisory": 0, "blocked": 0} }
	fixture := []struct {
		at  time.Time
		set func(map[string]any)
	}{
		{monday.AddDate(0, 0, 1), func(m map[string]any) { m["instance_id"], m["age_days"] = a, "1-6" }},
		{monday.AddDate(0, 0, 1+7), func(m map[string]any) { m["instance_id"], m["age_days"] = a, "7-27" }},
		{monday.AddDate(0, 0, 1+85), func(m map[string]any) { m["instance_id"], m["age_days"] = a, "90+"; quiet(m) }},
		{monday.AddDate(0, 0, 2), func(m map[string]any) {
			m["instance_id"], m["age_days"], m["version"], m["arch"], m["run_by"], m["github"] = b, "1-6", "v1.0.0", "amd64", "org", false
			delete(m, "install_mode")
			m["people"], m["deploys"], m["failures"], m["first"] = map[string]int{"d1": 1, "d7": 1, "d28": 1}, map[string]int{"preview": 1, "prod": 0}, map[string]int{}, map[string]string{"check": "1h_1d"}
			delete(m, "agents_via")
		}},
		{monday.AddDate(0, 0, 2), func(m map[string]any) { m["instance_id"], m["age_days"], m["demo"] = c, "1-6", true }},
		{monday.AddDate(0, 0, 3), func(m map[string]any) {
			m["instance_id"], m["age_days"], m["deploys"] = d, "0", map[string]int{"preview": 100, "prod": 0}
		}},
		{monday.AddDate(0, 0, 3), func(m map[string]any) {
			m["instance_id"], m["age_days"], m["licence"] = e, "28-89", map[string]string{"state": "lapsed"}
			m["people"] = map[string]int{"d1": 10, "d7": 20, "d28": 30}
		}},
	}
	schema, err := compileSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixture {
		body := with(goldenV2(t), f.set)
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil || schema.Validate(doc) != nil {
			t.Fatalf("a fixture report the schema refuses: %v %s", schema.Validate(doc), body)
		}
		if _, err := s.pool.Exec(ctx, "INSERT INTO reports (received_at, report) VALUES ($1, $2)", f.at, body); err != nil {
			t.Fatal(err)
		}
	}
	var demos int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM reports WHERE demo AND instance_id = $1 AND day = $2", c, monday.AddDate(0, 0, 2)).Scan(&demos); err != nil || demos != 1 {
		t.Fatalf("the demo's row by its columns: %d %v", demos, err)
	}
	weeks, err := s.weekly(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var got *week
	for i := range weeks {
		if weeks[i].Week == monday.Format("2006-01-02") {
			got = &weeks[i]
		}
	}
	if got == nil {
		t.Fatalf("no week %s in %+v", monday.Format("2006-01-02"), weeks)
	}
	w := *got
	for _, c := range []struct {
		what      string
		got, want any
	}{
		{"installs, new, demos", []int{w.Installs, w.NewInstalls, w.DemoInstalls.Active, w.DemoInstalls.New}, []int{3, 2, 1, 1}},
		{"retention", w.Retention, map[string]kept{"1w": {Of: 2, Reported: 1, Deploying: 1}, "4w": {Of: 2}, "12w": {Of: 2, Reported: 1}}},
		{"installs by", w.InstallsBy, map[string][]named{"version": {{"v0.9.0", 2}, {"v1.0.0", 1}}, "install_mode": {{"helm_one_node", 2}, {"unset", 1}},
			"platform": {{"linux/arm64", 2}, {"linux/amd64", 1}}}},
		{"deploys, D's among them", w.Sums.Deploys, map[string]int{"preview": 9 + 1 + 100 + 9, "prod": 2 + 0 + 0 + 2}},
		{"failures", w.Sums.Failures, []named{{"build", 6}, {"detect", 6}, {"error.scan", 3}}},
		// B's server is from before agents_via and sends none; the demo's is counted apart.
		{"reviews by how they were asked for", w.Sums.AgentsVia, map[string]int{"cli": 9 * 3, "mcp_local": 3 * 3, "mcp_remote": 1 * 3}},
		{"people", w.People, map[string]spread{"d1": {Of: 3, Median: 3, Sum: 14}, "d7": {Of: 3, Median: 5, Sum: 26}, "d28": {Of: 3, Median: 8, Sum: 39}}},
		{"github, run_by org, licence", []share{w.Features["github"], w.Features["run_by_org"], w.Features["licence"]}, []share{{3, 2}, {3, 1}, {3, 2}}},
		{"first", w.First, map[string]map[string]int{"check": {"lt_10m": 1, "1h_1d": 1}, "block": {"10m_1h": 1}, "fix": {"1h_1d": 1}, "prod": {"1d_7d": 1}}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.what, c.got, c.want)
		}
	}
}
