package events

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	p, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p, ctx
}

const project, run = "claims-portal", "gr_7kd2mq"

func seedRun(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO projects (id) VALUES ('" + project + "')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_7d3e8f', '" + project + "', 'aa', 'bb', 1, 'upload')",
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('" + run + "', '" + project + "', 'pkg_7d3e8f', 'running')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, ctx context.Context, p *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAppendRunRowReturnsItsIDAndTimestamp(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	row, err := AppendRun(ctx, p, run, project, RunState, map[string]any{"state": "pending", "queue_position": 0})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(row)
	if row.ID < 1 || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(row.TS) || !strings.Contains(string(wire), `"run":"gr_7kd2mq"`) || !strings.Contains(string(wire), `"data":{"queue_position":0,"state":"pending"}`) {
		t.Fatalf("row on the wire: %s", wire)
	}
}

func TestAppendRefusesAKindOutsideItsScope(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	if _, err := AppendOrg(ctx, p, project, Verdict, map[string]any{}); err == nil {
		t.Error("verdict is a run-scope kind; the org feed took it")
	}
	if _, err := AppendRun(ctx, p, run, project, TilesUpdated, map[string]any{}); err == nil {
		t.Error("tiles.updated is an org-scope kind; a run took it")
	}
	if count(t, ctx, p) != 0 {
		t.Fatal("a refused row was written")
	}
}

func TestAppendRefusesDataThatIsNotAnObject(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	if _, err := AppendOrg(ctx, p, project, AuditAppended, "ev_1"); err == nil || count(t, ctx, p) != 0 {
		t.Fatalf("a string payload must be refused before it is written: %v", err)
	}
}

func TestAppendsSerialiseOnTheAppendLock(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	holder, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", appendLock); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := AppendOrg(short, p, project, AuditAppended, map[string]any{"event_id": "ev_1"}); !errors.Is(err, context.DeadlineExceeded) || count(t, ctx, p) != 0 {
		t.Fatalf("an append must wait on the lock another process holds, not slip past it: %v", err)
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", appendLock); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendOrg(ctx, p, project, AuditAppended, map[string]any{"event_id": "ev_1"}); err != nil || count(t, ctx, p) != 1 {
		t.Fatalf("after release the append lands: %v", err)
	}
}

func ids(page Page) []int64 {
	out := []int64{}
	for _, r := range page.Rows {
		out = append(out, r.ID)
	}
	return out
}

func next(page Page) int64 {
	if page.Next == nil {
		return -1
	}
	return *page.Next
}

func TestRunTimelinePagesWithAnExclusiveCursor(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	for i := range 5 {
		if _, err := AppendRun(ctx, p, run, project, BuildLog, map[string]any{"line": strings.Repeat("=", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		after int64
		want  []int64
		next  int64
	}{{0, []int64{1, 2}, 2}, {2, []int64{3, 4}, 4}, {4, []int64{5}, 5}, {5, []int64{}, 5}} {
		page, err := RunTimeline(ctx, p, run, c.after, 2, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(page); !slices.Equal(got, c.want) || next(page) != c.next {
			t.Errorf("after=%d: rows %v next %d, want %v next %d (the cursor is exclusive, the run has not ended)", c.after, got, next(page), c.want, c.next)
		}
		if wire, _ := json.Marshal(page); len(c.want) == 0 && string(wire) != `{"rows":[],"next":5}` {
			t.Errorf("an empty page on the wire is rows [], not null: %s", wire)
		}
	}
	if _, err := RunTimeline(ctx, p, "gr_nope00", 0, 2, true); !errors.Is(err, ErrNoRun) {
		t.Errorf("an unknown run is ErrNoRun, got %v", err)
	}
}

func TestLimitDefaultsToAHundredAndCapsAtTwoHundred(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	for i := 0; i < 201; i++ {
		if _, err := AppendOrg(ctx, p, project, AuditAppended, map[string]any{"event_id": "ev"}); err != nil {
			t.Fatal(err)
		}
	}
	for limit, want := range map[int]int{0: 100, -5: 100, 1000: 200, 3: 3} {
		page, err := OrgTimeline(ctx, p, 0, limit, nil)
		if err != nil || len(page.Rows) != want {
			t.Errorf("limit %d: %d rows (%v), want %d", limit, len(page.Rows), err, want)
		}
	}
}

func TestNextIsNullOnlyWhenTheRunHasEnded(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	for range 3 {
		if _, err := AppendRun(ctx, p, run, project, BuildLog, map[string]any{"line": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if page, _ := RunTimeline(ctx, p, run, 0, 100, true); page.Next == nil {
		t.Fatal("a live run never answers null: the client would stop polling")
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET state = 'clean', ended_at = now() WHERE id = $1", run); err != nil {
		t.Fatal(err)
	}
	if page, _ := RunTimeline(ctx, p, run, 0, 100, true); page.Next != nil {
		t.Errorf("ended and nothing follows: next must be null, got %d", *page.Next)
	}
	if page, _ := RunTimeline(ctx, p, run, 0, 2, true); next(page) != 2 {
		t.Errorf("ended but a full page: next must be the cursor, got %d", next(page))
	}
	if page, _ := RunTimeline(ctx, p, run, 2, 2, true); len(page.Rows) != 1 || page.Next != nil {
		t.Errorf("the last partial page of an ended run: 1 row and null, got %v next %d", ids(page), next(page))
	}
}

func TestOrgTimelineNeverEndsAndCarriesNoRun(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	if _, err := AppendOrg(ctx, p, project, RunTransition, map[string]any{"project": project, "run": run, "state": "clean"}); err != nil {
		t.Fatal(err)
	}
	page, err := OrgTimeline(ctx, p, 0, 10, nil)
	if err != nil || page.Next == nil || len(page.Rows) != 1 {
		t.Fatalf("page %+v err %v", page, err)
	}
	wire, _ := json.Marshal(page.Rows[0])
	var top map[string]json.RawMessage
	if err := json.Unmarshal(wire, &top); err != nil {
		t.Fatal(err)
	}
	if _, hasRun := top["run"]; hasRun || string(top["scope"]) != `"org"` {
		t.Fatalf("an org row carries no run key: %s", wire)
	}
	if page, _ = OrgTimeline(ctx, p, *page.Next, 10, nil); len(page.Rows) != 0 || page.Next == nil {
		t.Fatalf("the org feed never ends: %+v", page)
	}
}

// The Go catalogue is the schema's enum, per scope; contracts/validate.py ties the schema to events.md.
func TestTheKindCatalogueIsTheSchemasEnum(t *testing.T) {
	raw, err := os.ReadFile("../contracts/schema/timeline-row.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Kind struct{ Enum []Kind }
		}
		AllOf []struct {
			If struct {
				Properties struct {
					Scope struct{ Const string }
				}
			}
			Then struct {
				Properties struct {
					Kind struct{ Enum []Kind }
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	perScope := map[string][]Kind{}
	for _, a := range schema.AllOf {
		if a.If.Properties.Scope.Const != "" {
			perScope[a.If.Properties.Scope.Const] = a.Then.Properties.Kind.Enum
		}
	}
	all := append(append([]Kind{}, RunKinds...), OrgKinds...)
	for name, got := range map[string][2][]Kind{"all": {all, schema.Properties.Kind.Enum}, "run": {RunKinds, perScope["run"]}, "org": {OrgKinds, perScope["org"]}} {
		if !slices.Equal(got[0], got[1]) {
			t.Errorf("%s: Go %v, schema %v", name, got[0], got[1])
		}
	}
}

func TestNothingLandsAfterTheRunEnded(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	if _, err := AppendRun(ctx, p, run, project, BuildLog, map[string]any{"line": "last"}); err != nil {
		t.Fatal(err)
	}
	if err := End(ctx, p, run); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendRun(ctx, p, run, project, BuildLog, map[string]any{"line": "late"}); !errors.Is(err, ErrEnded) || count(t, ctx, p) != 1 {
		t.Fatalf("a row after End is one a reader was told does not exist: %v", err)
	}
	if page, _ := RunTimeline(ctx, p, run, 1, 100, true); page.Next != nil {
		t.Fatal("ended and read through: next must be null")
	}
	if err := End(ctx, p, run); err != nil {
		t.Fatalf("End twice is a no-op: %v", err)
	}
	if err := End(ctx, p, "gr_nope00"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("End of an unknown run: %v", err)
	}
}

// ts is taken when the lock is granted, not when the transaction began: a waiting appender's
// row must not carry a timestamp older than the row it waited for.
func TestTimestampsAreTakenWhenTheLockIsGranted(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	holder, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", appendLock); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := AppendOrg(ctx, p, project, AuditAppended, map[string]any{"event_id": "ev_waited"})
		done <- err
	}()
	time.Sleep(1100 * time.Millisecond) // the waiter's transaction has begun; now() is already fixed for it
	var released time.Time
	if err := holder.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&released); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", appendLock); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var ts time.Time
	if err := p.QueryRow(ctx, "SELECT ts FROM timeline_rows").Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts.Before(released) {
		t.Fatalf("the waiter's row is stamped %v, before the lock was released at %v: that is transaction start, not grant time", ts, released)
	}
}

// The trail reads newest first, so an action is never timestamped before the one it followed, even
// when the host clock has stepped back (a VM's does): a row already a minute ahead stays behind the next.
func TestAnAuditRowIsNeverOlderThanTheOneBefore(t *testing.T) {
	p, ctx := testPool(t)
	seedRun(t, ctx, p)
	ahead := time.Now().Add(time.Minute)
	if _, err := p.Exec(ctx, "INSERT INTO audit_events (id, actor_kind, actor_subject, actor_display, action, detail, ts) VALUES ('ev_ahead', 'person', 'dana', 'Dana', 'before', '{}', $1)", ahead); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := AuditTx(ctx, tx, Actor{Kind: "person", Subject: "dana", Display: "Dana"}, "after", project, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var newest string
	if err := p.QueryRow(ctx, "SELECT action FROM audit_events ORDER BY ts DESC LIMIT 1").Scan(&newest); err != nil || newest != "after" {
		t.Fatalf("the action written last reads as newest: %q %v", newest, err)
	}
}

// A row written under a request's provenance carries it; one written with none on its context — a
// workflow's step — is Canyon's own, via canyon, with no role, session or address (contracts/routes.md,
// the audit read).
func TestAuditRowsCarryTheirProvenanceAndCanyonsOwnSayCanyon(t *testing.T) {
	p, ctx := testPool(t)
	write := func(c context.Context, id string) {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := LockTx(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if err := AuditTx(c, tx, Actor{Kind: "person", Subject: id}, "sign out", "", "", nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	write(WithProvenance(ctx, Provenance{Via: "cli", Role: "member", Session: "ses_0123456789abcdef", Source: "203.0.113.7"}), "asked")
	write(ctx, "workflow")
	write(WithProvenance(ctx, Provenance{Role: "admin", Source: "127.0.0.1"}), "sessionless")
	for subject, want := range map[string]string{"asked": "cli member ses_0123456789abcdef 203.0.113.7", "workflow": "canyon   ", "sessionless": " admin  127.0.0.1"} {
		var got string
		if err := p.QueryRow(ctx, "SELECT concat_ws(' ', coalesce(via, ''), coalesce(role, ''), coalesce(session, ''), coalesce(source, '')) FROM audit_events WHERE actor_subject = $1", subject).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", subject, got, err, want)
		}
	}
}
