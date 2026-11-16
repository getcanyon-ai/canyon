//go:build !noee

package runs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// prePatch is preview's row as dana's deploy left it, which a revert puts back.
type prePatch struct{ image, gateRun, sha string }

// watchedPatch is claims-portal's patch run once its watch runs, watched for watch, and preview's row before it.
func watchedPatch(t *testing.T, watch time.Duration) (*pgxpool.Pool, context.Context, *executortest.Fake, string, prePatch) {
	t.Helper()
	p, ctx, f, _ := patchable(t)
	var b prePatch
	if err := p.QueryRow(ctx, "SELECT image, gate_run, run_config_sha256 FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&b.image, &b.gateRun, &b.sha); err != nil {
		t.Fatal(err)
	}
	was := pipe.PatchWatch.Load()
	pipe.PatchWatch.Store(watch)
	t.Cleanup(func() { pipe.PatchWatch.Store(was) })
	if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
		t.Fatalf("the patch run: %d started (%v)", n, err)
	}
	t.Cleanup(func() { // a watch the test leaves running reads its run's end and stops, before the next test re-points pipe
		if _, err := p.Exec(context.Background(), "UPDATE runs SET state = 'failed', failure_reason = 'interrupted', ended_at = now() WHERE ended_at IS NULL"); err != nil {
			t.Error(err)
		}
		time.Sleep(4 * watchEvery.Load())
	})
	run, _, _ := patchRun(t, ctx, p, project)
	rowOf(t, ctx, p, run, "patch:watching")
	return p, ctx, f, run, b
}

// rowOf waits for the run's row of dedupe and answers when it was written.
func rowOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, run, dedupe string) time.Time {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		var ts time.Time
		err := p.QueryRow(ctx, "SELECT ts FROM timeline_rows WHERE run_id = $1 AND dedupe = $2", run, dedupe).Scan(&ts)
		if err == nil {
			return ts
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never wrote %s: lines %q", dedupe, lastLines(t, ctx, p, run))
		}
	}
}

// outcomeOf is the app's patch's outcome in its identity's waves.
func outcomeOf(t *testing.T, ctx context.Context, p *pgxpool.Pool) string {
	t.Helper()
	var outcome string
	if err := p.QueryRow(ctx, "SELECT outcome FROM patch_wave_apps WHERE project_id = $1", project).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

// watchStep is the newest status of the run's watch step.
func watchStep(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) string {
	t.Helper()
	var status string
	if err := p.QueryRow(ctx, "SELECT data->>'status' FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'step' = 'watch' ORDER BY id DESC LIMIT 1", run).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// An exit or a restart of the patched container in its watch reverts it: the kept image /run again, preview's
// row the one the patch replaced — its image, gate run and fingerprint — the audit row revert patch preview,
// actor Canyon, with the patch's detail, the watch step failed, line reverted naming how, the run
// failed/reverted, and the app's patch reverted for the breaker (contracts/routes.md, Patch runs, The revert).
func TestAnExitOrARestartReverts(t *testing.T) {
	for how, signal := range map[string]func(f *executortest.Fake){
		"exited":    func(f *executortest.Fake) { f.Exit(project, "preview") },
		"restarted": func(f *executortest.Fake) { f.Restart(project, "preview") },
	} {
		t.Run(how, func(t *testing.T) {
			p, ctx, f, run, b := watchedPatch(t, 30*time.Second)
			signal(f)
			if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
				t.Fatalf("the patch run: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
			if f.Runs.Load() != 3 || f.Ran().Image != b.image {
				t.Fatalf("%d /runs, the last of %s: want the kept %s run again", f.Runs.Load(), f.Ran().Image, b.image)
			}
			var image, gateRun, sha string
			if err := p.QueryRow(ctx, "SELECT image, gate_run, run_config_sha256 FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image, &gateRun, &sha); err != nil {
				t.Fatal(err)
			}
			if (prePatch{image, gateRun, sha}) != b {
				t.Fatalf("preview's row after the revert: %+v, want %+v", prePatch{image, gateRun, sha}, b)
			}
			var same bool
			if err := p.QueryRow(ctx, `SELECT r.detail = d.detail AND r.actor_kind = 'canyon' FROM audit_events r, audit_events d
				WHERE r.run_id = $1 AND r.action = 'revert patch preview' AND d.run_id = $1 AND d.action = 'patch preview'`, run).Scan(&same); err != nil || !same {
				t.Fatalf("the revert patch preview row: the patch's detail, actor Canyon %v (%v)", same, err)
			}
			lines := lastLines(t, ctx, p, run)
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, "Canyon reverted preview at ") || !strings.HasSuffix(last, " UTC: the patched app "+how+", so the image it replaced runs again") {
				t.Fatalf("its last line: %q", last)
			}
			if s, o := watchStep(t, ctx, p, run), outcomeOf(t, ctx, p); s != "failed" || o != PatchReverted {
				t.Fatalf("its watch step %s, its outcome %s", s, o)
			}
		})
	}
}

// A person's request and a revert never both /run (contracts/routes.md, Patch runs, The revert): a start that
// ends the watch before the revert's compare-and-set — its workflow's cancel not landed yet, as on another
// replica — goes ahead alone, the revert putting nothing back; a start after it is 409 run_in_progress, and the
// revert alone /runs.
func TestRevertRacesRequestOneRun(t *testing.T) {
	t.Run("the request first", func(t *testing.T) {
		p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
		var person atomic.Pointer[string]
		hook := func(at string) { // on the workflow's goroutine: an error, never a Fatal
			if at != "before" || person.Load() != nil {
				return
			}
			var started Run
			err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) {
				started, _, err = StartTx(ctx, tx, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: testPolicy()})
				return err
			})
			if err != nil {
				t.Errorf("a start ahead of the revert: %v", err)
			}
			person.Store(&started.ID)
		}
		pipe.reverting.Store(&hook)
		t.Cleanup(func() { pipe.reverting.Store(nil) })
		f.Restart(project, "preview")
		if state, reason := waitEnded(t, ctx, p, run); state != "clean" {
			t.Fatalf("the watch a start ended: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
		}
		if person.Load() == nil {
			t.Fatal("the revert never reached its compare-and-set")
		}
		if state, reason := waitEnded(t, ctx, p, *person.Load()); state != "clean" {
			t.Fatalf("the person's deploy: %s %s", state, reasonOf(reason))
		}
		var reverts int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 'revert patch preview'").Scan(&reverts); err != nil {
			t.Fatal(err)
		}
		if f.Runs.Load() != 3 || reverts != 0 {
			t.Fatalf("a request and a revert: %d /runs after the patch's, %d revert rows; want the person's alone", f.Runs.Load()-2, reverts)
		}
		if lines := lastLines(t, ctx, p, run); lines[len(lines)-1] != "The watch ended early: Dana Whitfield started another run of this app" || outcomeOf(t, ctx, p) != PatchEndedEarly {
			t.Fatalf("the watch the start ended: %q, outcome %s", lines, outcomeOf(t, ctx, p))
		}
	})
	for _, at := range []string{"locked", "cas"} { // the revert holds the undeploy lock, before and after its compare-and-set: run_in_progress, never undeploying
		t.Run("the revert first, "+at, func(t *testing.T) {
			p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
			var refused atomic.Pointer[error]
			hook := func(now string) {
				if now != at || refused.Load() != nil {
					return
				}
				_, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
				refused.Store(&err)
			}
			pipe.reverting.Store(&hook)
			t.Cleanup(func() { pipe.reverting.Store(nil) })
			f.Restart(project, "preview")
			if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
				t.Fatalf("the revert: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
			var said error // nil: the hook never ran
			if err := refused.Load(); err != nil {
				said = *err
			}
			if !errors.Is(said, ErrRunInProgress) {
				t.Fatalf("a start during the revert: %v, want run_in_progress", said)
			}
			if f.Runs.Load() != 3 {
				t.Fatalf("a revert and a request: %d /runs after the patch's; want the revert's alone", f.Runs.Load()-2)
			}
		})
	}
}

// A check — a run that deploys nothing — leaves a watch running: its start is 409 run_in_progress, and the watch
// goes on to revert the exit it then sees (contracts/routes.md, Patch runs).
func TestACheckLeavesAWatchRunning(t *testing.T) {
	p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
	if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana}); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("a check during a watch: %v, want run_in_progress", err)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1 AND ended_at IS NULL", run).Scan(&state); err != nil || state != "running" {
		t.Fatalf("the watch after a check: %q (%v)", state, err)
	}
	if until, err := WatchingUntil(ctx, p, run); err != nil || until == nil || time.Until(*until) <= 0 || time.Until(*until) > 30*time.Second { // what the check's 409 names (#2568)
		t.Fatalf("the watch's watch_until on its row: %v (%v)", until, err)
	}
	f.Exit(project, "preview")
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
		t.Fatalf("the watch a check left running: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
}

// The revert's own write after its /run is retried: a failure of it once ends the run reverted all the same,
// the kept image /run again and one revert patch preview row — never a run left running and reverting.
func TestARevertsWriteThatFailsIsRetried(t *testing.T) {
	p, ctx, f, run, b := watchedPatch(t, 30*time.Second)
	for _, q := range []string{
		"CREATE SEQUENCE revert_write_fails",
		`CREATE FUNCTION fail_revert_once() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF nextval('revert_write_fails') = 1 THEN RAISE EXCEPTION 'the revert''s write fails once'; END IF; RETURN NEW; END $$`,
		"CREATE TRIGGER fail_revert_once BEFORE INSERT ON audit_events FOR EACH ROW WHEN (NEW.action = 'revert patch preview') EXECUTE FUNCTION fail_revert_once()",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	f.Restart(project, "preview")
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
		t.Fatalf("a revert whose write failed once: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
	var image string
	var reverts int
	if err := p.QueryRow(ctx, "SELECT image, (SELECT count(*) FROM audit_events WHERE action = 'revert patch preview') FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image, &reverts); err != nil {
		t.Fatal(err)
	}
	if image != b.image || reverts != 1 || f.Runs.Load() != 4 {
		t.Fatalf("preview runs %s, %d revert rows, %d /runs: want the kept image, one row, the revert's /run twice", image, reverts, f.Runs.Load())
	}
}

// A revert whose /run fails says why in one of its own fixed words, never the executor's, and never that the
// patched app runs: the patched image stays, the run ends failed/error, and the breaker counts it
// (contracts/routes.md, Patch runs, revert-failed).
func TestARevertThatFailsSaysWhyInItsOwnWords(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		fail         func(f *executortest.Fake)
	}{
		{"a kept image gone", "it is no longer on this Canyon", func(f *executortest.Fake) { f.NoImage.Store(1) }},
		{"a /run refused", "Canyon couldn't put the old image back", func(f *executortest.Fake) { f.RefuseRun.Store(1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
			c.fail(f)
			f.Exit(project, "preview")
			if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "error" {
				t.Fatalf("a revert that failed: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
			lines := lastLines(t, ctx, p, run)
			if want := "Canyon could not put back the image the patch replaced: " + c.reason + ". The patched image stays in preview: deploy again to replace it"; lines[len(lines)-1] != want {
				t.Fatalf("its last line: %q, want %q", lines[len(lines)-1], want)
			}
			if _, gateRun, _ := previewRow(t, ctx, p); gateRun != run || outcomeOf(t, ctx, p) != PatchReverted {
				t.Fatalf("preview's row names %s, the outcome %s: want the patch's, reverted", gateRun, outcomeOf(t, ctx, p))
			}
		})
	}
}

// A resumed run watches until the journaled watch_until, never a watch of its own length from its resume: its
// watch ends at the same instant, with one watching line (contracts/routes.md, Patch runs, step 8).
func TestWatchResumesAtWatchUntil(t *testing.T) {
	const watch = 3 * time.Second
	p, ctx, _, run, _ := watchedPatch(t, watch)
	began := rowOf(t, ctx, p, run, "patch:watching")
	if err := dbos.CancelWorkflow(durable, watchID(run)); err != nil { // the server stops mid-watch
		t.Fatal(err)
	}
	if st := settled(t, watchID(run)); st != dbos.WorkflowStatusCancelled {
		t.Fatalf("the stopped watch's workflow: %s", st)
	}
	time.Sleep(watch / 2)
	if _, err := dbos.ResumeWorkflow[any](durable, watchID(run)); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "clean" {
		t.Fatalf("the resumed watch: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
	passed := rowOf(t, ctx, p, run, "patch:passed")
	if d := passed.Sub(began); d < watch-time.Second || d >= watch+watch/2 {
		t.Fatalf("the resumed watch passed %s after it began, want about %s: never a whole watch from its resume", d, watch)
	}
	var watching int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM timeline_rows WHERE run_id = $1 AND data->>'line' LIKE 'Watching preview until %'", run).Scan(&watching); err != nil || watching != 1 {
		t.Fatalf("%d watching lines (%v)", watching, err)
	}
}

// A patch run's watch holds no slot of the patches queue (#2567; design/PATCHING.md §4.2, Concurrency): with
// CANYON_PATCH_CONCURRENCY at 1, a second app's patch run starts while the first app's watch runs, its gate-run
// ended once its /run was recorded, and the watch goes on.
func TestAWatchHoldsNoPatchSlot(t *testing.T) {
	if pipe.PatchConcurrency > 1 {
		t.Fatalf("PatchConcurrency %d: the test needs the patches queue's one slot", pipe.PatchConcurrency)
	}
	p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
	image := "canyon/wave-b@" + sha('b')
	runningPreview(t, ctx, p, f, "wave-b", sha('b'))
	if _, err := p.Exec(ctx, "UPDATE patch_identities SET wave = 1 WHERE id = $1", fakeRunImage); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO patch_wave_apps (identity, project_id, image, wave) VALUES ($1, 'wave-b', $2, 1)", fakeRunImage, image); err != nil {
		t.Fatal(err)
	}
	if why, err := pipe.StartPatch(ctx, "wave-b", Patch{Identity: fakeRunImage, RunImage: fakeRunImage, Image: image}); err != nil || why != WhyStarted {
		t.Fatalf("wave-b's patch run: %s (%v)", why, err)
	}
	second, _, _ := patchRun(t, ctx, p, "wave-b")
	for deadline := time.Now().Add(10 * time.Second); status(t, second) == dbos.WorkflowStatusEnqueued; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("wave-b's patch run is still queued 10 s into %s's watch: the watch holds the patches queue's one slot", project)
		}
	}
	var watching bool
	if err := p.QueryRow(ctx, "SELECT ended_at IS NULL FROM runs WHERE id = $1", run).Scan(&watching); err != nil || !watching {
		t.Fatalf("%s's watch ended before wave-b's patch run started (%v)", project, err)
	}
	if st := status(t, run); st != dbos.WorkflowStatusSuccess {
		t.Fatalf("%s's gate-run, its /run recorded: %s, want it ended", project, st)
	}
	if st := status(t, watchID(run)); st != dbos.WorkflowStatusPending {
		t.Fatalf("%s's watch: %s, want it under way", project, st)
	}
}

// A gate-run canyon-v10 journaled mid-watch, its watch inline — "watch until" where this release enqueues
// patch.watch — resumes on this release and watches inline as journaled, to its end: never a step its journal does
// not name, which DBOS refuses, leaving the run running and unwatched (#2567, the review of #2607). The v10 journal is
// this run's own, its enqueue step replaced by the "watch until" step its watch journaled, then put back on its queue
// as handOver does.
func TestAGateRunJournaledMidWatchResumesItInline(t *testing.T) {
	p, ctx, _, run, _ := watchedPatch(t, 2*time.Second)
	if st := settled(t, run); st != dbos.WorkflowStatusSuccess {
		t.Fatalf("the gate-run, its /run recorded: %s", st)
	}
	if err := dbos.CancelWorkflow(durable, watchID(run)); err != nil { // as if no patch.watch was ever enqueued
		t.Fatal(err)
	}
	ops := durableSchema + ".operation_outputs"
	tag, err := p.Exec(ctx, `WITH enq AS (DELETE FROM `+ops+` WHERE workflow_uuid = $1 AND function_name = 'DBOS.enqueue' RETURNING function_id)
		INSERT INTO `+ops+` (workflow_uuid, function_id, output, error, function_name, started_at_epoch_ms, completed_at_epoch_ms, serialization, application_name)
		SELECT $1, enq.function_id, w.output, w.error, w.function_name, w.started_at_epoch_ms, w.completed_at_epoch_ms, w.serialization, w.application_name
		FROM enq, `+ops+` w WHERE w.workflow_uuid = $2 AND w.function_name = 'watch until'`, run, watchID(run))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("the v10 journal: %d rows (%v)", tag.RowsAffected(), err)
	}
	if _, err := p.Exec(ctx, "UPDATE "+durableSchema+".workflow_status SET status = $2, queue_name = $3, started_at_epoch_ms = NULL, output = NULL WHERE workflow_uuid = $1",
		run, string(dbos.WorkflowStatusEnqueued), Queue); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "clean" {
		t.Fatalf("the resumed v10 gate-run's watch: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
	if st := settled(t, run); st != dbos.WorkflowStatusSuccess {
		t.Fatalf("the resumed v10 gate-run: %s", st)
	}
	if lines := lastLines(t, ctx, p, run); lines[len(lines)-1] != "The watch passed: the patched app neither exited nor restarted" {
		t.Fatalf("its last line: %q", lines[len(lines)-1])
	}
}

// A stop mid-watch ends it clean: an undeploy of preview, a delete of the project and a person's start each end
// the watch — its step done, line ended naming who and how, the run clean — and nothing is put back; the app's
// patch reads ended_early, which proves nothing (contracts/routes.md, Patch runs, A watch ends clean).
func TestStopMidWatchEndsClean(t *testing.T) {
	var started string // the start's own run, waited for before the test ends
	for _, c := range []struct {
		name, how string
		end       func(ctx context.Context, tx pgx.Tx) (string, error)
	}{
		{"an undeploy", "took preview offline", func(ctx context.Context, tx pgx.Tx) (string, error) {
			b, err := BrakeTx(ctx, tx, project, "preview", dana, false)
			return b.Ended, err
		}},
		{"a delete", "took preview offline", func(ctx context.Context, tx pgx.Tx) (string, error) {
			return EndWatchTx(ctx, tx, project, dana)
		}},
		{"a start", "started another run of this app", func(ctx context.Context, tx pgx.Tx) (string, error) {
			run, ended, err := StartTx(ctx, tx, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: testPolicy()})
			started = run.ID
			return ended, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, run, _ := watchedPatch(t, 30*time.Second)
			var ended string
			if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { ended, err = c.end(ctx, tx); return err }); err != nil || ended != run {
				t.Fatalf("%s mid-watch ended %q, not the patch run %s (%v)", c.name, ended, run, err)
			}
			Cancel(durable, run) // the watch, should it read first, finds its run ended and leaves it so
			var state string
			if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1 AND ended_at IS NOT NULL", run).Scan(&state); err != nil || state != "clean" {
				t.Fatalf("the watch %s ended: %q (%v)", c.name, state, err)
			}
			f.Restart(project, "preview") // what it stops afterwards is no signal to anyone
			time.Sleep(4 * watchEvery.Load())
			lines := lastLines(t, ctx, p, run)
			if want := "The watch ended early: Dana Whitfield " + c.how; lines[len(lines)-1] != want {
				t.Fatalf("its last line: %q, want %q", lines[len(lines)-1], want)
			}
			ran := int32(2) // dana's deploy and the patch's: nothing is put back
			if started != "" {
				if state, reason := waitEnded(t, ctx, p, started); state != "clean" {
					t.Fatalf("the start's own run: %s %s", state, reasonOf(reason))
				}
				ran = 3 // and the start's own deploy
			}
			if s, o := watchStep(t, ctx, p, run), outcomeOf(t, ctx, p); s != "done" || o != PatchEndedEarly || f.Runs.Load() != ran {
				t.Fatalf("its watch step %s, outcome %s, %d /runs", s, o, f.Runs.Load())
			}
		})
	}
}

// prodRow is production's deploy row: every column a revert puts back that tells two rows apart, the actors'
// kinds, subjects and displays among them.
type prodRow struct{ pkg, image, gateRun, sha, runBy, buildpacks, egress, deployedBy, promotedBy string }

// readProd is production's row now.
func readProd(t *testing.T, ctx context.Context, p *pgxpool.Pool) prodRow {
	t.Helper()
	var r prodRow
	if err := p.QueryRow(ctx, `SELECT package_id, image, gate_run, coalesce(run_config_sha256, ''), run_by, coalesce(buildpacks::text, ''), coalesce(egress::text, ''),
		concat_ws('/', deployed_by_kind, deployed_by_subject, deployed_by_display), concat_ws('/', promoted_by_kind, promoted_by_subject, promoted_by_display)
		FROM deploys WHERE project_id = $1 AND env = 'prod'`, project).Scan(&r.pkg, &r.image, &r.gateRun, &r.sha, &r.runBy, &r.buildpacks, &r.egress, &r.deployedBy, &r.promotedBy); err != nil {
		t.Fatal(err)
	}
	return r
}

// promotedThenPatched is claims-portal with production running dana's promotion — its row then made to differ from
// any the patch's promotion writes, in its package, buildpacks and who deployed and promoted it, so a revert that
// misses a column shows; its egress stays, as the app's one Docker network decides it for both environments — and
// preview the patch whose watch passed, its patch approval pending: production's row, the patch run and the image
// it put in preview.
func promotedThenPatched(t *testing.T, watch time.Duration) (*pgxpool.Pool, context.Context, *executortest.Fake, prodRow, string, string) {
	t.Helper()
	p, ctx, f, _ := patchable(t)
	image, deployRun, _ := previewRow(t, ctx, p)
	if _, state, reason := promote(t, ctx, p, deployRun, image); state != "clean" {
		t.Fatalf("dana's promotion: %s %s", state, reasonOf(reason))
	}
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) SELECT 'pkg_9c1d2e', project_id, tree_hash, commit, size_bytes, source_kind FROM packages WHERE id = '" + pkg + "'",
		`UPDATE deploys SET package_id = 'pkg_9c1d2e', buildpacks = '[]', deployed_by_kind = 'agent', deployed_by_subject = 'claude-code',
			deployed_by_display = 'Claude Code', promoted_by_kind = 'person', promoted_by_subject = 'olga@meridian.example', promoted_by_display = 'Olga Petrova'
			WHERE project_id = '` + project + `' AND env = 'prod'`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	before := readProd(t, ctx, p)
	patchRun, state, reason := patched(t, ctx, p)
	if state != "clean" {
		t.Fatalf("the patch run: %s %s, lines %q", state, reason, lastLines(t, ctx, p, patchRun))
	}
	patchedImage, _, _ := previewRow(t, ctx, p)
	was := pipe.PatchWatch.Load()
	pipe.PatchWatch.Store(watch)
	t.Cleanup(func() { pipe.PatchWatch.Store(was) })
	return p, ctx, f, before, patchRun, patchedImage
}

// startApproved is promotedThenPatched with the patch approval approved by dana, as the approve route does: the
// promote run it started, watched for watch, and production's row before it. moved, when set, runs in that
// transaction once the run is started.
func startApproved(t *testing.T, watch time.Duration, moved func(ctx context.Context, tx pgx.Tx) error) (*pgxpool.Pool, context.Context, *executortest.Fake, string, prodRow) {
	t.Helper()
	p, ctx, f, before, patchRun, patchedImage := promotedThenPatched(t, watch)
	var code string
	if err := p.QueryRow(ctx, "SELECT code FROM promotion_approvals WHERE project_id = $1 AND kind = 'patch' AND state = 'pending' AND image = $2", project, patchedImage).Scan(&code); err != nil {
		t.Fatalf("the patch approval its watch filed: %v", err)
	}
	var run Run
	if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { // the approve route's transaction: the run, and the approval approved naming it
		if run, _, err = StartTx(ctx, tx, durable, StartInput{Project: project, Package: pkg, Actor: dana,
			Promote: &Promotion{Image: patchedImage, GateRun: patchRun, DeployedBy: dana, ReceiptRun: patchRun}}); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE promotion_approvals SET state = 'approved', decided_by_kind = $2, decided_by_subject = $3, decided_by_display = $4, decided_at = now(), run_id = $5
			WHERE code = $1`, code, dana.Kind, dana.Subject, dana.Display, run.ID); err != nil || moved == nil {
			return err
		}
		return moved(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cancel(durable, run.ID) }) // its watch never outlives its test
	return p, ctx, f, run.ID, before
}

// approvedPatch is startApproved's promote run once production's row records it live.
func approvedPatch(t *testing.T, watch time.Duration) (*pgxpool.Pool, context.Context, *executortest.Fake, string, prodRow) {
	t.Helper()
	p, ctx, f, run, before := startApproved(t, watch, nil)
	rowOf(t, ctx, p, run, "live:prod")
	return p, ctx, f, run, before
}

// An approved patch's promotion watches production as the patch watched preview, and an exit there puts back the
// image production ran (contracts/routes.md, Production approvals and Patch runs): the kept image /run again,
// production's row the one the promotion replaced — every column of it — the audit row revert patch prod, actor
// Canyon, with patch prod's detail, the watch step failed, line reverted, and the run failed/reverted.
func TestPatchPromoteWatchesProduction(t *testing.T) {
	p, ctx, f, run, before := approvedPatch(t, 30*time.Second)
	if live := readProd(t, ctx, p); live.pkg == before.pkg || live.buildpacks == before.buildpacks || live.deployedBy == before.deployedBy || live.promotedBy == before.promotedBy {
		t.Fatalf("the patch's promotion wrote production's row as it was: %+v; the test cannot tell a revert that misses a column", live)
	}
	f.Exit(project, "prod")
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
		t.Fatalf("the approved patch's promotion, its production exited: %s %s, lines %q, production %+v", state, reasonOf(reason), lastLines(t, ctx, p, run), readProd(t, ctx, p))
	}
	if f.Runs.Load() != 5 || f.Ran().Image != before.image || f.Ran().Target != "prod" {
		t.Fatalf("%d /runs, the last of %s in %s: want the kept %s run again in prod", f.Runs.Load(), f.Ran().Image, f.Ran().Target, before.image)
	}
	if after := readProd(t, ctx, p); after != before {
		t.Fatalf("production's row after the revert:\n got %+v\nwant %+v", after, before)
	}
	var same bool
	if err := p.QueryRow(ctx, `SELECT r.detail = d.detail AND r.actor_kind = 'canyon' FROM audit_events r, audit_events d
		WHERE r.run_id = $1 AND r.action = 'revert patch prod' AND d.run_id = $1 AND d.action = 'patch prod'`, run).Scan(&same); err != nil || !same {
		t.Fatalf("the revert patch prod row: patch prod's detail, actor Canyon %v (%v)", same, err)
	}
	lines := lastLines(t, ctx, p, run)
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "Canyon reverted production at ") || !strings.HasSuffix(last, " UTC: the patched app exited, so the image it replaced runs again") {
		t.Fatalf("its last line: %q", last)
	}
	if s := watchStep(t, ctx, p, run); s != "failed" {
		t.Fatalf("its watch step: %s", s)
	}
}

// A patch that failed in production is the app's reverted in its identity's waves, as a revert in preview is, and
// so is never asked for again (contracts/routes.md, Production approvals, filed again; design/PATCHING.md §0.3):
// once its production watch saw the patched app exit — reverted, or its revert failed and production was put back
// on the replaced image since — the tick files no approval for that app and identity, though production runs what
// the patch replaces and preview the patch.
func TestAPatchThatFailedInProductionIsNotAskedAgain(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		fail         func(f *executortest.Fake)
	}{
		{"reverted", "reverted", func(*executortest.Fake) {}},
		{"its revert failed", "error", func(f *executortest.Fake) { f.NoImage.Store(1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx, f, run, before := approvedPatch(t, 30*time.Second)
			c.fail(f)
			f.Exit(project, "prod")
			if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != c.reason {
				t.Fatalf("the approved patch's promotion, its production exited: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
			if o := outcomeOf(t, ctx, p); o != PatchReverted {
				t.Fatalf("the app's patch in its identity's waves: %s, want %s", o, PatchReverted)
			}
			if _, err := p.Exec(ctx, "UPDATE deploys SET image = $2 WHERE project_id = $1 AND env = 'prod'", project, before.image); err != nil { // as a revert, or a person, leaves it
				t.Fatal(err)
			}
			n, err := pipe.Patcher.FileApprovals(ctx, pipe, project)
			var pending int
			if qerr := p.QueryRow(ctx, "SELECT count(*) FROM promotion_approvals WHERE project_id = $1 AND state = 'pending'", project).Scan(&pending); qerr != nil {
				t.Fatal(qerr)
			}
			if err != nil || n != 0 || pending != 0 {
				t.Fatalf("the tick after the patch failed in production filed %d, %d pending (%v): want none for that identity", n, pending, err)
			}
		})
	}
}

// A production watch whose read at watch_until does not answer is blind, as in preview: nothing is put back, the
// patched image stays in production, the run ends failed/error on line blind, and the image it replaced stays kept
// while production's row is the patch's — production's previous image, the one its newest history row replaced
// (contracts/routes.md, Patch runs, A watch ends clean; design/OPERATIONS.md §1.5).
func TestABlindProductionWatchRevertsNothing(t *testing.T) {
	was := watchEvery.Load()
	watchEvery.Store(time.Hour) // one read as the watch begins, and one at its end
	t.Cleanup(func() { watchEvery.Store(was) })
	p, ctx, f, run, before := approvedPatch(t, 2*time.Second)
	f.FailRunning.Store(true)
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "error" {
		t.Fatalf("a blind production watch: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
	lines := lastLines(t, ctx, p, run)
	if want := "Canyon could not tell whether the patched app runs, so the patch goes no further: it runs in production as deployed"; lines[len(lines)-1] != want {
		t.Fatalf("its last line: %q, want %q", lines[len(lines)-1], want)
	}
	if s := watchStep(t, ctx, p, run); s != "failed" || f.Runs.Load() != 4 || readProd(t, ctx, p).image == before.image {
		t.Fatalf("its watch step %s, %d /runs, production %s: nothing reverted blind", s, f.Runs.Load(), readProd(t, ctx, p).image)
	}
	keep, err := pipe.keep(ctx, project, "canyon/"+project+"@"+sha('9'))
	if err != nil {
		t.Fatal(err)
	}
	if _, digest, _ := strings.Cut(before.image, "@"); !slices.Contains(keep, digest) {
		t.Fatalf("a blind production watch released the image it replaced: kept %v", keep)
	}
}

// A person's own promotion of the patched preview ships it, patch prod on the trail, and is not watched: only the
// promote run a patch approval started watches production (contracts/routes.md, Production approvals).
func TestAPersonsPromotionOfAPatchedPreviewIsNotWatched(t *testing.T) {
	p, ctx, _, _, patchRun, patchedImage := promotedThenPatched(t, 30*time.Second)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana,
		Promote: &Promotion{Image: patchedImage, GateRun: patchRun, DeployedBy: dana, ReceiptRun: patchRun}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cancel(durable, run.ID) })
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("a person's promotion of the patched preview: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run.ID))
	}
	var watched, recorded bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe LIKE 'patch:%'), patch_replaced IS NOT NULL FROM runs WHERE id = $1`, run.ID).
		Scan(&watched, &recorded); err != nil || watched || recorded {
		t.Fatalf("a person's promotion watched %v, recorded what it replaced %v (%v)", watched, recorded, err)
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM audit_events WHERE run_id = $1 AND action = 'patch prod'", run.ID); n != 1 {
		t.Fatalf("%d patch prod rows, want one", n)
	}
}

// An approved patch's promotion never ships over a production that moved since the approval: before its /run it
// reads production's row again, and one no longer running the image the approval replaces ends the run
// failed/interrupted, nothing started, on its own line (contracts/routes.md, Production approvals, approving).
func TestAMovedProductionIsNeverShippedOver(t *testing.T) {
	p, ctx, f, run, _ := startApproved(t, 30*time.Second, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE deploys SET image = $2 WHERE project_id = $1 AND env = 'prod'", project, "canyon/"+project+"@"+sha('7'))
		return err
	})
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "interrupted" {
		t.Fatalf("a promotion of the patch over a moved production: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
	}
	lines := lastLines(t, ctx, p, run)
	if want := "deploying to production: failed: production runs another version now: Canyon asks again if the patch still applies; the version running there is unchanged"; lines[len(lines)-1] != want {
		t.Fatalf("its last line: %q, want %q", lines[len(lines)-1], want)
	}
	if n := count(t, ctx, p, "SELECT count(*) FROM audit_events WHERE run_id = $1 AND action = 'patch prod'", run); f.Runs.Load() != 3 || n != 0 {
		t.Fatalf("%d /runs, %d patch prod rows: nothing started", f.Runs.Load(), n)
	}
}

// The requests that end a patch run's watch end an approved patch's watch of production the same, each line ended
// naming production, and nothing is put back; a check leaves it running, 409 run_in_progress, with no watch, which
// the gate-runs row carries exactly for a patch run (contracts/routes.md, Patch runs: the same holds of a promote
// run shipping a patch).
func TestAStopEndsAProductionWatchClean(t *testing.T) {
	t.Run("a check", func(t *testing.T) {
		p, ctx, f, run, _ := approvedPatch(t, 30*time.Second)
		rowOf(t, ctx, p, run, "patch:watching")
		if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana}); !errors.Is(err, ErrRunInProgress) {
			t.Fatalf("a check during production's watch: %v, want run_in_progress", err)
		}
		if until, err := WatchingUntil(ctx, p, run); err != nil || until != nil {
			t.Fatalf("a promote run's watch_until named as a patch run's: %v (%v)", until, err)
		}
		var state string
		if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1 AND ended_at IS NULL", run).Scan(&state); err != nil || state != "running" {
			t.Fatalf("production's watch after a check: %q (%v)", state, err)
		}
		f.Exit(project, "prod") // the watch the check left running goes on to revert, and ends within the test
		if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reasonOf(reason) != "reverted" {
			t.Fatalf("the watch a check left running: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
		}
	})
	var started string // the start's own run, waited for before its case ends
	for _, c := range []struct {
		name, how string
		end       func(ctx context.Context, tx pgx.Tx) (string, error)
	}{
		{"an undeploy of production", "took production offline", func(ctx context.Context, tx pgx.Tx) (string, error) {
			b, err := BrakeTx(ctx, tx, project, "prod", dana, false)
			return b.Ended, err
		}},
		{"a hold", "stopped production", func(ctx context.Context, tx pgx.Tx) (string, error) {
			b, err := BrakeTx(ctx, tx, project, "prod", dana, true)
			return b.Ended, err
		}},
		{"a delete", "took production offline", func(ctx context.Context, tx pgx.Tx) (string, error) {
			return EndWatchTx(ctx, tx, project, dana)
		}},
		{"a start", "started another run of this app", func(ctx context.Context, tx pgx.Tx) (string, error) {
			run, ended, err := StartTx(ctx, tx, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: testPolicy()})
			started = run.ID
			return ended, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			started = ""
			p, ctx, f, run, _ := approvedPatch(t, 30*time.Second)
			rowOf(t, ctx, p, run, "patch:watching")
			var ended string
			if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) (err error) { ended, err = c.end(ctx, tx); return err }); err != nil || ended != run {
				t.Fatalf("%s mid-watch ended %q, not the promote run %s (%v)", c.name, ended, run, err)
			}
			Cancel(durable, run)
			ran := f.Runs.Load()
			f.Exit(project, "prod") // what it stops afterwards is no signal to anyone
			time.Sleep(4 * watchEvery.Load())
			var state string
			if err := p.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1 AND ended_at IS NOT NULL", run).Scan(&state); err != nil || state != "clean" {
				t.Fatalf("the watch %s ended: %q (%v)", c.name, state, err)
			}
			lines := lastLines(t, ctx, p, run)
			if want := "The watch ended early: Dana Whitfield " + c.how; lines[len(lines)-1] != want {
				t.Fatalf("its last line: %q, want %q", lines[len(lines)-1], want)
			}
			var reverts int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 'revert patch prod'").Scan(&reverts); err != nil {
				t.Fatal(err)
			}
			if started != "" { // the start's own deploy, in preview
				if state, reason := waitEnded(t, ctx, p, started); state != "clean" {
					t.Fatalf("the start's own run: %s %s", state, reasonOf(reason))
				}
				ran++
			}
			if s := watchStep(t, ctx, p, run); s != "done" || reverts != 0 || f.Runs.Load() != ran {
				t.Fatalf("its watch step %s, %d revert rows, %d /runs since it ended: nothing put back", s, reverts, f.Runs.Load()-ran)
			}
		})
	}
}

// A watch whose read at watch_until does not answer is blind: no revert, nothing goes further — the watch step
// failed, line blind, the run failed/error, the app's patch failed. Reads that failed before it are read again,
// so a container that exited meanwhile and is absent at the last read is an exit, never blind (contracts/
// routes.md, Patch runs, step 8 and A watch ends clean).
func TestABlindWatchRevertsNothing(t *testing.T) {
	for _, c := range []struct {
		name       string
		lastAnswer bool // the read at watch_until answers
		reason     string
	}{
		{"the last read fails", false, "error"},
		{"the container exited, the last read answers", true, "reverted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			was := watchEvery.Load()
			watchEvery.Store(time.Hour) // one read as the watch begins, and one at its end
			t.Cleanup(func() { watchEvery.Store(was) })
			p, ctx, f, _ := patchable(t)
			pipeWas := pipe.PatchWatch.Load()
			pipe.PatchWatch.Store(2 * time.Second)
			t.Cleanup(func() { pipe.PatchWatch.Store(pipeWas) })
			if n, err := pipe.planPatches(ctx); err != nil || n != 1 {
				t.Fatalf("the patch run: %d started (%v)", n, err)
			}
			f.FailRunning.Store(true) // from the plan's own read on: the watch's reads fail
			run, _, _ := patchRun(t, ctx, p, project)
			rowOf(t, ctx, p, run, "patch:watching")
			f.Exit(project, "preview")
			f.FailRunning.Store(!c.lastAnswer)
			state, reason := waitEnded(t, ctx, p, run)
			if state != "failed" || reasonOf(reason) != c.reason {
				t.Fatalf("the watch: %s %s, lines %q", state, reasonOf(reason), lastLines(t, ctx, p, run))
			}
			if c.lastAnswer {
				return // TestAnExitOrARestartReverts says what a revert leaves
			}
			lines := lastLines(t, ctx, p, run)
			if lines[len(lines)-1] != "Canyon could not tell whether the patched app runs, so the patch goes no further: it runs in preview as deployed" {
				t.Fatalf("its last line: %q", lines[len(lines)-1])
			}
			if s, o := watchStep(t, ctx, p, run), outcomeOf(t, ctx, p); s != "failed" || o != PatchFailed || f.Runs.Load() != 2 {
				t.Fatalf("its watch step %s, outcome %s, %d /runs: nothing reverted blind", s, o, f.Runs.Load())
			}
			kept := func() bool { // the image the patch replaced, in what the next /run of the app keeps
				t.Helper()
				keep, err := pipe.keep(ctx, project, "canyon/"+project+"@"+sha('9'))
				if err != nil {
					t.Fatal(err)
				}
				return slices.Contains(keep, executortest.Digest)
			}
			if !kept() {
				t.Fatal("a blind watch released the image the patch replaced while preview's row is the patch's")
			}
			if _, err := p.Exec(ctx, "UPDATE deploys SET gate_run = (SELECT id FROM runs WHERE project_id = $1 AND kind = 'gate' ORDER BY created_at LIMIT 1) WHERE project_id = $1 AND env = 'preview'", project); err != nil { // another run of preview replaced it
				t.Fatal(err)
			}
			if kept() {
				t.Fatal("the image a blind patch replaced is still kept once preview's row is no longer the patch's")
			}
		})
	}
}

// A patch run past its /run is its watch's to drive (#2567): its gate-run ended, a watch DBOS will not drive ends it
// failed/interrupted as a run's own workflow would.
func TestReconcileEndsAPatchRunWhoseWatchWasCancelled(t *testing.T) {
	p, ctx, _, run, _ := watchedPatch(t, 30*time.Second)
	if err := dbos.CancelWorkflow(durable, watchID(run)); err != nil {
		t.Fatal(err)
	}
	if st := settled(t, watchID(run)); st != dbos.WorkflowStatusCancelled {
		t.Fatalf("the cancelled watch: %s", st)
	}
	if n, err := pipe.Reconcile(ctx, durable); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || reason == nil || *reason != "interrupted" {
		t.Fatalf("a patch run whose watch DBOS will not drive ends interrupted: %s %v", state, reason)
	}
}
