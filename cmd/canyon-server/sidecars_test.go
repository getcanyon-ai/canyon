package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/runs"
)

// sidecarImage is the image the fake executor's rolls move onto.
var sidecarImage = "ghcr.io/getcanyon-ai/canyon-identity@sha256:" + strings.Repeat("fe", 32)

// sidecarAnswers runs the server's sidecar loop against an executor that answers each POST /sidecars with
// the next of answers — "" drops the connection unanswered — and the last one from then on, until it has
// answered after(n) is true for the calls made, and answers the loop's log.
func sidecarAnswers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, answers []string, after func(calls int) bool) string {
	t.Helper()
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sidecars" {
			http.NotFound(w, r)
			return
		}
		answer := answers[min(int(calls.Add(1)), len(answers))-1]
		if answer == "" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(fake.Close)
	tick, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var logs syncBuffer
	go func() {
		(&app{pool: pool, pipe: &runs.Pipeline{Executor: &executor.Client{Base: fake.URL}}, kube: true}).rollSidecars(tick, log.New(&logs, "", 0), 20*time.Millisecond)
		close(done)
	}()
	for deadline := time.Now().Add(20 * time.Second); !after(int(calls.Load())); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the server asked %d times in 20 s:\n%s", calls.Load(), logs.String())
		}
	}
	cancel()
	<-done
	return logs.String()
}

// sidecarRows is the trail's update sidecar rows, oldest first, each as one line.
func sidecarRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT project_id || ' ' || (detail->>'env') || ' ' || (detail->>'roll') || ' ' || (detail->>'image') || ' ' || actor_kind || ':' || actor_subject || ' via ' || via
		FROM audit_events WHERE action = 'update sidecar' ORDER BY ts, id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// projectsFor makes each app, as POST /projects does.
func projectsFor(t *testing.T, base string, ids ...string) {
	t.Helper()
	c := client(t)
	for _, id := range ids {
		if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"`+id+`"}`); code != 201 {
			t.Fatalf("create %s: %d %s", id, code, lastBody)
		}
	}
}

// Each roll the executor answers (contracts/executor.md, POST /sidecars) is one `update sidecar` row on its
// app, by Canyon via canyon, written the first time an answer names it: the executor answers it again on
// every call until the environment's next /run, and a second answer writes no second row. Nothing else is
// written — no run, no deploy, no history. An answer that rolled nothing and left one alone writes nothing
// and says so; an answer out of the contract's shape writes nothing and is logged.
func TestEachSidecarRollIsOneUpdateSidecarRow(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	projectsFor(t, base, "claims-portal", "expenses")
	both := `{"image":"` + sidecarImage + `","rolled":[{"app":"claims-portal","target":"preview","roll":"4f1c0b9a"},{"app":"claims-portal","target":"prod","roll":"0b9a4f1c"}],"failed":1}`
	before := count(t, ctx, pool, "SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM deploys) + (SELECT count(*) FROM deploy_history)")
	logged := sidecarAnswers(t, ctx, pool, []string{
		// the same state, answered twice
		both, both,
		// nothing rolled, one left alone
		`{"image":"` + sidecarImage + `","rolled":[],"failed":1}`,
		// out of the contract
		`{"image":"` + sidecarImage + `","rolled":[{"app":"expenses","target":"preview","roll":"9a4f1c0b"}],"failed":0,"skipped":0}`,
	}, func(calls int) bool { return calls >= 5 })
	want := []string{
		"claims-portal preview 4f1c0b9a " + sidecarImage + " canyon:upgrade via canyon",
		"claims-portal prod 0b9a4f1c " + sidecarImage + " canyon:upgrade via canyon",
	}
	if got := sidecarRows(t, ctx, pool); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the update sidecar rows:\n%s\nwant one per roll:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if after := count(t, ctx, pool, "SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM deploys) + (SELECT count(*) FROM deploy_history)"); after != before {
		t.Errorf("a roll wrote %d runs, deploys or history rows: it is no deploy", after-before)
	}
	for _, line := range []string{
		"canyon-server: sign-in sidecars on " + sidecarImage + ": 2 recorded, 1 not moved (the executor's log names each)\n",
		"canyon-server: sign-in sidecars on " + sidecarImage + ": 0 recorded, 1 not moved (the executor's log names each)\n",
		"POST /sidecars answered out of the contract",
	} {
		if !strings.Contains(logged, line) {
			t.Errorf("the log says nothing of %q:\n%s", line, logged)
		}
	}
}

// A roll whose answer was lost, or whose row could not be written, is written from a later answer: the
// executor answers it until the environment's next /run (contracts/executor.md, POST /sidecars). A roll of
// an app this host has no row of is logged on every answer naming it, and written once the app exists.
func TestALostSidecarRollIsRecordedFromALaterAnswer(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	projectsFor(t, base, "claims-portal")
	ledger := `{"image":"` + sidecarImage + `","rolled":[{"app":"claims-portal","target":"preview","roll":"4f1c0b9a"},{"app":"ledger","target":"prod","roll":"1c0b9a4f"}],"failed":0}`
	logged := sidecarAnswers(t, ctx, pool, []string{"", ledger}, func(calls int) bool { return calls >= 3 })
	if got := sidecarRows(t, ctx, pool); len(got) != 1 || !strings.HasPrefix(got[0], "claims-portal preview 4f1c0b9a ") {
		t.Fatalf("after a lost answer, the rows are %v, want claims-portal preview's from the answer after it", got)
	}
	if !strings.Contains(logged, "the sign-in sidecar of ledger-prod is on "+sidecarImage+", not recorded yet: this host has no app ledger") {
		t.Errorf("the roll of an app this host has no row of is not logged:\n%s", logged)
	}
	projectsFor(t, base, "ledger")
	sidecarAnswers(t, ctx, pool, []string{ledger}, func(calls int) bool { return calls >= 2 })
	if got := sidecarRows(t, ctx, pool); len(got) != 2 || !strings.HasPrefix(got[1], "ledger prod 1c0b9a4f ") {
		t.Errorf("once ledger exists, the rows are %v, want its roll written once", got)
	}
}
