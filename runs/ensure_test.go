package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var dbKey = provisioners.Key{Project: project, Env: "preview", Name: "db"}

// ensureFunc is a driver whose Ensure answers ready with the outputs, or the error; no test here deletes.
type ensureFunc func(context.Context, provisioners.Key, string) (map[string]string, error)

func (e ensureFunc) Ensure(ctx context.Context, k provisioners.Key, typ string) (provisioners.Result, error) {
	out, err := e(ctx, k, typ)
	return provisioners.Result{State: provisioners.Ready, Outputs: out}, err
}

func (ensureFunc) Name() string { return "ensure-func" }

func (ensureFunc) Delete(context.Context, provisioners.Key) (provisioners.Result, error) {
	panic("delete")
}

// declaringDB is deploying() with the package declaring db = postgres and d as the driver.
func declaringDB(t *testing.T, d provisioners.Driver) (*pgxpool.Pool, context.Context, *executortest.Fake) {
	t.Helper()
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres"}' WHERE id = $1`, pkg); err != nil {
		t.Fatal(err)
	}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: d}
	t.Cleanup(func() { pipe.Drivers = nil })
	return p, ctx, f
}

func deployOnce(t *testing.T, ctx context.Context, p *pgxpool.Pool) (id, state string, reason *string) {
	t.Helper()
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	state, reason = waitEnded(t, ctx, p, run.ID)
	return run.ID, state, reason
}

func handed(f *executortest.Fake) string {
	_, env := f.Handed()
	return env["DATABASE_URL"]
}

func lastLine(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) string {
	t.Helper()
	var line string
	// a deploy's access lines after its deploy.completed row are their own story (access_test.go)
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' AND coalesce(dedupe, '') NOT IN ('access', 'description') AND coalesce(dedupe, '') NOT LIKE 'access:%' ORDER BY id DESC LIMIT 1", run).Scan(&line); err != nil {
		t.Fatal(err)
	}
	return line
}

// The container gets a DATABASE_URL that opens the app's own database, sealed as a provisioned row, the record ready.
func TestADeployDeclaringPostgresHandsTheContainerItsOwnDatabase(t *testing.T) {
	b, err := provisioners.NewBuiltin(context.Background(), pgtest.Exclusive(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.Delete(context.Background(), dbKey); b.Close() })
	p, ctx, f := declaringDB(t, b)
	run, state, reason := deployOnce(t, ctx, p)
	if state != "clean" || reason != nil {
		t.Fatalf("a deploy whose database is ensured ends clean: %s %s", state, reasonOf(reason))
	}
	first := handed(f)
	if line := lastLine(t, ctx, p, run); line != "postgres db: ready" {
		t.Fatalf("the timeline says the database is ready: %q", line)
	}
	if _, hits := journaled(t, ctx, p, run, first); hits != 0 {
		t.Fatal("the URL reached the journal")
	}
	// a deploy that fails on another resource, then a plain redeploy: the running preview's URL is kept, and opens its own database
	for _, declared := range []string{`{"db": "postgres", "store": "s3"}`, `{"db": "postgres"}`} {
		if _, err := p.Exec(ctx, "UPDATE packages SET resources = $1 WHERE id = $2", declared, pkg); err != nil {
			t.Fatal(err)
		}
		deployOnce(t, ctx, p)
	}
	c, err := pgx.Connect(ctx, first)
	if err != nil {
		t.Fatalf("the container's first DATABASE_URL does not open: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	var db, user string
	if err := c.QueryRow(ctx, "SELECT current_database(), current_user").Scan(&db, &user); err != nil || !strings.HasPrefix(db, "c_") || user != db || handed(f) != first {
		t.Fatalf("the app's own database and login, the URL kept: %q %q (%v)", db, user, err)
	}
	var sealedReady bool
	if err := p.QueryRow(ctx, `SELECT r.state = 'ready' AND r.driver = 'builtin' AND s.origin = 'provisioned' AND s.set_by_kind = 'canyon' AND s.scope = 'runtime' AND s.version = 1
		FROM resources r, secrets s WHERE r.env = 'preview' AND r.name = 'db' AND s.env = 'preview' AND s.name = 'DATABASE_URL'`).Scan(&sealedReady); err != nil || !sealedReady {
		t.Fatalf("the record ready and the row sealed once as provisioned: %v (%v)", sealedReady, err)
	}
}

// s3 is served by a platform team's adapter: the deploy asks it for the store, and the container
// gets STORAGE_URL and nothing else the adapter answered.
func TestADeployDeclaringS3HandsTheContainerOnlyTheAdaptersStorageURL(t *testing.T) {
	storage := "https://" + "app-key" + ":" + "app-secret" + "@objects.internal/claims-portal-files?region=eu-central-1" // built from pieces: no credential-shaped literal
	var asked string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = r.Method + " " + r.URL.Path + " " + string(body)
		out, _ := json.Marshal(map[string]any{"outputs": map[string]string{"STORAGE_URL": storage, "NODE_OPTIONS": "--require /tmp/x.js"}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(s.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("adapter-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := provisioners.NewHTTP(s.URL+"/canyon", tokenFile, "")
	if err != nil {
		t.Fatal(err)
	}
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"files": "s3"}' WHERE id = $1`, pkg); err != nil {
		t.Fatal(err)
	}
	pipe.Drivers = provisioners.Drivers{{"s3", "preview"}: d}
	t.Cleanup(func() { pipe.Drivers = nil })
	run, state, reason := deployOnce(t, ctx, p)
	if _, env := f.Handed(); state != "clean" || !maps.Equal(env, map[string]string{"STORAGE_URL": storage}) {
		t.Fatalf("a deploy declaring s3: %s %s, the container got %v", state, reasonOf(reason), env)
	}
	if want := "PUT /canyon/" + project + `/preview/files {"type":"s3"}`; asked != want || lastLine(t, ctx, p, run) != "s3 files: ready" {
		t.Fatalf("the adapter was asked %q, want %q; the timeline says %q", asked, want, lastLine(t, ctx, p, run))
	}
}

// Neither a replayed ensure step nor a redeploy re-issues anything while the record is ready and
// its row exists: the running app keeps its password.
func TestAReplayOrARedeployKeepsTheCredential(t *testing.T) {
	var calls int
	p, ctx, f := declaringDB(t, ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		calls++
		return map[string]string{"DATABASE_URL": fmt.Sprintf("value-ensure-%d", calls)}, nil
	}))
	run, state, reason := deployOnce(t, ctx, p)
	if state != "clean" || calls != 1 {
		t.Fatalf("first deploy: %s %s, %d ensures", state, reasonOf(reason), calls)
	}
	if res, err := pipe.ensure(ctx, Input{Run: run, Project: project, Package: pkg, Deploy: true, Actor: dana}, "preview", nil, nil); err != nil || res.Failed != "" || calls != 1 {
		t.Fatalf("the replayed step: %+v %v, %d ensures", res, err, calls)
	}
	var v, audits int
	if _, state, _ := deployOnce(t, ctx, p); state != "clean" || calls != 1 || handed(f) != "value-ensure-1" {
		t.Fatalf("a redeploy keeps the credential: %s, %d ensures, handed %q", state, calls, handed(f))
	}
	var driver string
	if err := p.QueryRow(ctx, "SELECT (SELECT version FROM secrets WHERE name = 'DATABASE_URL'), (SELECT count(*) FROM audit_events WHERE action = 'ensure resource'), (SELECT driver FROM resources)").Scan(&v, &audits, &driver); err != nil || v != 1 || audits != 1 || driver != "ensure-func" {
		t.Fatalf("after a replay and a redeploy: version %d, %d audit rows, made by %q (%v)", v, audits, driver, err)
	}
}

// A hand-set DATABASE_URL is never overwritten: the deploy fails naming it, no container starts.
func TestAHandSetOutputFailsTheDeployAndIsKept(t *testing.T) {
	p, ctx, f := declaringDB(t, ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		return map[string]string{"DATABASE_URL": "postgres://provisioned"}, nil
	}))
	sealed(t, ctx, p, "preview", "runtime", "DATABASE_URL", "postgres://mine")
	run, state, reason := deployOnce(t, ctx, p)
	if state != "failed" || reasonOf(reason) != "error" || f.Runs.Load() != 0 {
		t.Fatalf("refused before any container: %s %s, %d runs", state, reasonOf(reason), f.Runs.Load())
	}
	want := "DATABASE_URL is set by hand; remove it with canyon secret rm DATABASE_URL so Canyon can provide postgres db"
	if line := lastLine(t, ctx, p, run); line != "postgres db: failed: "+want {
		t.Fatalf("the timeline names the row: %q", line)
	}
	var kept bool
	if err := p.QueryRow(ctx, "SELECT s.version = 1 AND s.origin IS NULL AND r.state = 'failed' AND r.message = $1 FROM secrets s, resources r WHERE s.name = 'DATABASE_URL' AND r.name = 'db'", want).Scan(&kept); err != nil || !kept {
		t.Fatalf("the user's row kept, the record failed with the message: %v (%v)", kept, err)
	}
}

// A driver's error is transient (provisioners.Driver): the deploy asks again, as it does a pending
// resource but with no waiting line, and goes on once it answers; one still erring at the 30-minute
// deadline fails the run with its error, no container started (#1013). A type with no driver, or a
// record being deleted, fails the run with the message at once.
func TestAFailedEnsureFailsTheRunAndStartsNoContainer(t *testing.T) {
	var asked atomic.Int32
	p, ctx, f := declaringDB(t, ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		if asked.Add(1) < 3 {
			return nil, errors.New("the server refused the login")
		}
		return map[string]string{"DATABASE_URL": "value-db"}, nil
	}))
	was := pendingEvery
	pendingEvery = 10 * time.Millisecond
	t.Cleanup(func() { pendingEvery = was })
	run, state, reason := deployOnce(t, ctx, p)
	var lines string
	if err := p.QueryRow(ctx, "SELECT string_agg(data->>'line', '|' ORDER BY id) FROM timeline_rows WHERE run_id = $1 AND data->>'line' ~ '^postgres '", run).Scan(&lines); err != nil ||
		state != "clean" || asked.Load() != 3 || f.Runs.Load() != 1 || lines != "postgres db: creating|postgres db: ready" {
		t.Fatalf("a driver that errs twice, then answers: %s %s, %d asks, %d runs, %q (%v)", state, reasonOf(reason), asked.Load(), f.Runs.Load(), lines, err)
	}
	if _, err := p.Exec(ctx, "UPDATE resources SET state = 'pending'"); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{}) // a run of its own, held before its first write, for the step at the deadline
	pipe.hold.Store(&held)
	next, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	asked.Store(0) // erring again
	res, err := pipe.ensure(ctx, Input{Run: next.ID, Project: project, Package: pkg}, "preview", map[string]time.Time{"db": time.Now().Add(-resourceDeadline)}, nil)
	asked.Store(2) // the held run's own ask answers
	close(held)
	pipe.hold.Store(nil)
	waitEnded(t, ctx, p, next.ID)
	if err != nil || res.Failed != "the server refused the login" {
		t.Fatalf("an error at the deadline: %+v %v", res, err)
	}
	runs := f.Runs.Load()
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		panic("no Ensure before every type has a driver")
	})}
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres", "store": "s3"}' WHERE id = $1`, pkg); err != nil {
		t.Fatal(err)
	}
	run, state, _ = deployOnce(t, ctx, p)
	err = p.QueryRow(ctx, "SELECT string_agg(data->>'line', '|' ORDER BY id) FROM timeline_rows WHERE run_id = $1 AND data->>'line' ~ '^(postgres|s3) '", run).Scan(&lines)
	if state != "failed" || f.Runs.Load() != runs || err != nil || lines != "s3 store: creating|s3 store: failed: this host does not provision s3 in preview" {
		t.Fatalf("db + s3 with no s3 driver: %s, %d runs, %q (%v)", state, f.Runs.Load()-runs, lines, err)
	}
	pipe.Drivers = provisioners.Drivers{{"postgres", "prod"}: ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		panic("a preview deploy never asks prod's driver")
	})}
	if run, _, _ = deployOnce(t, ctx, p); lastLine(t, ctx, p, run) != "postgres db: failed: this host does not provision postgres in preview" {
		t.Fatalf("a driver for prod only: %q", lastLine(t, ctx, p, run))
	}
	pipe.Drivers = nil
	if run, _, _ = deployOnce(t, ctx, p); lastLine(t, ctx, p, run) != "postgres db: failed: this host does not provision postgres in preview: set CANYON_APP_DATABASE_URL" {
		t.Fatalf("no postgres driver: %q", lastLine(t, ctx, p, run))
	}
	pipe.Drivers = provisioners.Drivers{{"postgres", "preview"}: ensureFunc(func(context.Context, provisioners.Key, string) (map[string]string, error) {
		panic("a deleting record is never ensured")
	})}
	if _, err := p.Exec(ctx, `UPDATE packages SET resources = '{"db": "postgres"}'; UPDATE resources SET state = 'deleting' WHERE name = 'db'`); err != nil {
		t.Fatal(err)
	}
	var still string
	run, state, _ = deployOnce(t, ctx, p)
	if err := p.QueryRow(ctx, "SELECT state FROM resources WHERE name = 'db'").Scan(&still); err != nil || state != "failed" || still != "deleting" || lastLine(t, ctx, p, run) != "postgres db: failed: postgres db is being deleted" {
		t.Fatalf("a deleting record: run %s, record %s (%v), %q", state, still, err, lastLine(t, ctx, p, run))
	}
}

// answers is a driver answering each Ensure with the next of its results, then the last one, and
// noting how many containers had started when it was asked.
type answers struct {
	results []provisioners.Result
	f       *executortest.Fake
	calls   atomic.Int32
	started atomic.Int32 // the most containers started by the time of any ask
}

func (a *answers) Ensure(context.Context, provisioners.Key, string) (provisioners.Result, error) {
	n := int(a.calls.Add(1)) - 1
	a.started.Store(max(a.started.Load(), a.f.Runs.Load()))
	return a.results[min(n, len(a.results)-1)], nil
}

func (*answers) Name() string { return "answers" }

func (*answers) Delete(context.Context, provisioners.Key) (provisioners.Result, error) {
	panic("delete")
}

var ready = provisioners.Result{State: provisioners.Ready, Outputs: map[string]string{"DATABASE_URL": "value-db"}}

// A database the platform is still creating holds the deploy: no container until the driver answers
// ready, one waiting line however often it was asked; one pending past the deadline fails the run.
func TestADeployWaitsForAPendingResource(t *testing.T) {
	d := &answers{results: []provisioners.Result{{State: provisioners.Pending}, {State: provisioners.Pending}, ready}}
	p, ctx, f := declaringDB(t, d)
	d.f = f
	was := pendingEvery
	pendingEvery = 10 * time.Millisecond
	t.Cleanup(func() { pendingEvery = was })
	run, state, reason := deployOnce(t, ctx, p)
	var lines string
	if err := p.QueryRow(ctx, "SELECT string_agg(data->>'line', '|' ORDER BY id) FROM timeline_rows WHERE run_id = $1 AND data->>'line' ~ '^postgres '", run).Scan(&lines); err != nil ||
		state != "clean" || d.calls.Load() != 3 || d.started.Load() != 0 || f.Runs.Load() != 1 || lines != "postgres db: creating|postgres db: waiting on the platform|postgres db: ready" {
		t.Fatalf("a deploy past a pending database: %s %s, %d asks, %d containers by the last ask, %q (%v)", state, reasonOf(reason), d.calls.Load(), d.started.Load(), lines, err)
	}
	if res, err := pipe.ensure(ctx, Input{Run: run, Project: project, Package: pkg}, "preview", map[string]time.Time{"db": time.Now().Add(-resourceDeadline)}, nil); err != nil || d.calls.Load() != 3 || res.Pending != "" {
		t.Fatalf("a replay asks nothing of a ready record: %+v %v", res, err)
	}
	if _, err := p.Exec(ctx, "UPDATE resources SET state = 'pending'"); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{}) // a run of its own, held before its first write, for the step at the deadline
	pipe.hold.Store(&held)
	next, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	d.results = []provisioners.Result{{State: provisioners.Pending}}
	res, err := pipe.ensure(ctx, Input{Run: next.ID, Project: project, Package: pkg}, "preview", map[string]time.Time{"db": time.Now().Add(-resourceDeadline)}, nil)
	d.results = []provisioners.Result{ready}
	close(held)
	pipe.hold.Store(nil)
	waitEnded(t, ctx, p, next.ID)
	if err != nil || res.Failed != "not ready after 30 minutes" {
		t.Fatalf("pending past the deadline: %+v %v", res, err)
	}
}
