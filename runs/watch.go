package runs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/provisioners"
)

// DefaultPatchWatch is how long a patched preview is watched when CANYON_PATCH_WATCH is unset (design/PATCHING.md
// §4.4).
const DefaultPatchWatch = 15 * time.Minute

// watchEvery is how often a watch reads the patched container's state (contracts/routes.md, Patch runs, step 8);
// a variable so a test need not wait, read while the watch runs and so set atomically (#2664).
var watchEvery = swappableOf(30 * time.Second)

// Swappable is a value a running workflow reads while it may be set: atomic, so every set is ordered with
// every read (#2664). The zero Swappable holds T's zero value.
type Swappable[T any] struct{ v atomic.Pointer[T] }

// Load is the value s holds.
func (s *Swappable[T]) Load() T {
	if v := s.v.Load(); v != nil {
		return *v
	}
	var zero T
	return zero
}

// Store sets s to v.
func (s *Swappable[T]) Store(v T) { s.v.Store(&v) }

func swappableOf[T any](v T) *Swappable[T] {
	s := &Swappable[T]{}
	s.Store(v)
	return s
}

// envWords is an environment as a patch run's lines name it (contracts/routes.md, Patch runs, Its lines).
var envWords = map[string]string{"preview": "preview", "prod": "production"}

// shipsPatchSQL is, on a row of runs, a run that watches what it deploys (contracts/routes.md, Patch runs): a patch
// run, or the promote run a patch approval started (Production approvals) — whether or not it could keep the row
// it replaced, so it is watched all the same and its revert fails closed.
const shipsPatchSQL = `(runs.kind = 'patch' OR runs.kind = 'promote' AND EXISTS (SELECT 1 FROM promotion_approvals x WHERE x.run_id = runs.id AND x.kind = 'patch'))`

// watchingSQL is, on a row of runs, a run whose watch runs or is due: one that ships a patch (shipsPatchSQL), its
// /run recorded live, and not reverting.
const watchingSQL = shipsPatchSQL + ` AND runs.reverting_at IS NULL
	AND EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = runs.id AND dedupe IN ('deploy:preview', 'deploy:prod'))`

// patchRun is the patch run whose image in's environment row names while that row is the patch's: the run itself,
// or the patch run a promotion ships (its gate run).
func (in Input) patchRun() string {
	if in.Promote != nil {
		return in.Promote.GateRun
	}
	return in.Run
}

// WatchWorkflow is a patch run's watch as a workflow of its own (#2567), on the gate queue, which has no limit: its
// run's slot of the patches queue frees once its /run is recorded, as a watch neither rebases nor scans (design/PATCHING.md
// §4.2, Concurrency).
const WatchWorkflow = "patch.watch"

// watchID is the id of run's watch workflow, one a run; it begins with the run's id and a dash, as its tasks' do (Dequeue).
func watchID(run string) string { return run + "-watch" }

// watchInput is the patch.watch workflow's input: the patch run's own, and its gate's counts, which its end writes.
type watchInput struct {
	Input
	Blocking int `json:"blocking"`
	Warnings int `json:"warnings"`
}

// watchPatch is the patch.watch workflow: the patch run's watch (watch), unless the run ended before it began — a
// person's request ended the watch while it waited its turn — which leaves the run as it ended and writes nothing.
func (p *Pipeline) watchPatch(ctx dbos.Context, in watchInput) (Result, error) {
	var state string
	if err := p.Pool.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", in.Run).Scan(&state); err != nil {
		return Result{}, err
	}
	if state != "running" {
		return Result{State: state}, nil
	}
	return p.watch(ctx, in.Input, in.Blocking, in.Warnings)
}

// watched is the watch step's checkpoint: the signal, exited or restarted; or that the read at watch_until did
// not answer (blind); or that a request ended the run meanwhile; none of them, the watch passed.
type watched struct {
	How   string `json:"how,omitempty"`
	Blind bool   `json:"blind,omitempty"`
	Ended bool   `json:"ended,omitempty"`
}

// watchFor is CANYON_PATCH_WATCH, DefaultPatchWatch unset.
func (p *Pipeline) watchFor() time.Duration {
	if w := p.PatchWatch.Load(); w > 0 {
		return w
	}
	return DefaultPatchWatch
}

// readContainer is the patched container's signal, read with POST /running of it alone (contracts/executor.md):
// restarted when it was started again since its /run, exited when it neither runs nor restarts, "" while it runs.
func (p *Pipeline) readContainer(ctx context.Context, project, env string) (string, error) {
	if p.Executor == nil {
		return "", errors.New("no executor configured")
	}
	c := executor.Container{App: project, Target: env}
	res, err := p.Executor.RunningOf(ctx, []executor.Container{c}, false)
	switch {
	case err != nil:
		return "", err
	case slices.ContainsFunc(res.Restarting, func(r executor.Restarting) bool { return r.App == project && r.Target == env }):
		return "restarted", nil
	case !slices.Contains(res.Running, c):
		return "exited", nil
	}
	return "", nil
}

// watchContainer reads the patched container every watchEvery until watch_until, idempotently, so a resumed run
// reads again until the same instant (contracts/routes.md, Patch runs, step 8). Its one signal is an exit or a
// restart. A read that fails is read again at the next tick — a restart is counted from the /run, and an exit
// stays — but the read at watch_until must answer, else the watch is blind: it never passes on a read it did
// not make. A run a request ended meanwhile is left as the request ended it.
func (p *Pipeline) watchContainer(ctx context.Context, in Input, env string, until time.Time) (watched, error) {
	for {
		last := !time.Now().Before(until)
		var ended bool
		err := p.Pool.QueryRow(ctx, "SELECT ended_at IS NOT NULL FROM runs WHERE id = $1", in.Run).Scan(&ended)
		if err == nil && ended {
			return watched{Ended: true}, nil
		}
		how := ""
		if err == nil {
			how, err = p.readContainer(ctx, in.Project, env)
		}
		switch {
		case ctx.Err() != nil:
			return watched{}, ctx.Err()
		case err == nil && how != "":
			return watched{How: how}, nil
		case err != nil && last:
			log.Printf("runs: run %s (%s): the patched container's state at the watch's end: %v", in.Run, in.Project, err)
			return watched{Blind: true}, nil
		case last:
			return watched{}, nil
		case err != nil:
			log.Printf("runs: run %s (%s): the patched container's state, read again in %s: %v", in.Run, in.Project, watchEvery.Load(), err)
		}
		select {
		case <-ctx.Done():
			return watched{}, ctx.Err()
		case <-time.After(min(watchEvery.Load(), time.Until(until))):
		}
	}
}

// watch is a patch run's watch, from the /run it recorded (contracts/routes.md, Patch runs, step 8), and the watch
// of production by the promote run a patch approval started (Production approvals): watch_until journaled, then
// line watching, then the container read until then. It passes at its end, line passed; a read that cannot tell
// at its end is blind, nothing reverted; an exit or a restart reverts. A request that ended the run meanwhile
// ended it clean itself (EndWatchTx). Its outcome is the app's in its identity's waves (design/PATCHING.md §0.3,
// §4.2): every end of a patch run's watch, and a promotion's revert, which a revert in preview is too — reverted,
// counted by the breaker, and never asked for again (contracts/routes.md, Production approvals, filed again). The
// promotion's other ends leave the outcome its patch run recorded.
func (p *Pipeline) watch(ctx dbos.Context, in Input, blocking, warnings int) (Result, error) {
	env := in.writes()
	line := func(key, text string) error {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			return struct{}{}, p.record(c, in, provisioners.Key{}, "", "", "", "", "patch:"+key, text)
		}, dbos.WithStepName("patch line"))
		return err
	}
	outcome := func(outcome string) error {
		if in.Patch == nil && outcome != PatchReverted {
			return nil
		}
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			var identity string
			if in.Patch != nil {
				identity = in.Patch.Identity
			} else if err := p.Pool.QueryRow(c, "SELECT patch_identity FROM promotion_approvals WHERE run_id = $1 AND kind = 'patch'", in.Run).Scan(&identity); err != nil {
				return struct{}{}, err // the approval that started it names the identity
			}
			if err := p.RecordPatchOutcome(c, identity, in.Project, outcome); err != nil || outcome != PatchPassed {
				return struct{}{}, err
			}
			// a watch that ran to its end is when its patch approval comes due; one not filed now is filed by the next tick
			if p.Patcher == nil {
				return struct{}{}, c.Err()
			}
			if _, err := p.Patcher.FileApprovals(c, p, in.Project); err != nil && c.Err() == nil {
				log.Printf("runs: run %s (%s): its patch approval, filed again at the next tick: %v", in.Run, in.Project, err)
			}
			return struct{}{}, c.Err()
		}, dbos.WithStepName("patch outcome"))
		return err
	}
	until, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now().Add(p.watchFor()).UTC(), nil }, dbos.WithStepName("watch until"))
	if err != nil {
		return Result{}, err
	}
	began, err := p.began(ctx, in, "watch")
	if err != nil {
		return Result{}, err
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { // line watching, and watch_until on the run's row, which a check refused meanwhile names (the gate-runs row's 409)
		return struct{}{}, p.write(c, func(tx pgx.Tx) error {
			if _, err := tx.Exec(c, "UPDATE runs SET watch_until = $2 WHERE id = $1", in.Run, until); err != nil {
				return err
			}
			_, err := events.AppendRunTx(c, tx, in.Run, in.Project, events.BuildLog, "patch:watching",
				map[string]any{"line": "Watching " + envWords[env] + " until " + until.Format("15:04") + " UTC: if the patched app exits or restarts, Canyon puts back the image it replaced"})
			return err
		})
	}, dbos.WithStepName("patch line")); err != nil {
		return Result{}, err
	}
	seen, err := dbos.RunAsStep(ctx, func(c context.Context) (watched, error) { return p.watchContainer(c, in, env, until) }, dbos.WithStepName("watch"))
	if err != nil {
		return Result{}, err
	}
	end := func(done bool, out, state, reason, key, text string) (Result, error) {
		if err := p.ended(ctx, in, "watch", began, done); err != nil {
			return Result{}, err
		}
		if err := line(key, text); err != nil {
			return Result{}, err
		}
		if err := outcome(out); err != nil {
			return Result{}, err
		}
		return p.finish(ctx, in, state, reason, blocking, warnings)
	}
	switch {
	case seen.Ended:
		return Result{State: "clean", Blocking: blocking, Warnings: warnings}, nil
	case seen.Blind:
		return end(false, PatchFailed, "failed", "error", "blind", "Canyon could not tell whether the patched app runs, so the patch goes no further: it runs in "+envWords[env]+" as deployed")
	case seen.How == "":
		return end(true, PatchPassed, "clean", "", "passed", "The watch passed: the patched app neither exited nor restarted")
	}
	r, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) { return p.revert(c, in, env, seen.How, began) },
		dbos.WithStepName("revert"), dbos.WithStepMaxRetries(recordRetries)) // idempotent: a retry finds its end written, or the row still the patch's and /runs the kept image again
	if err != nil {
		return Result{}, err
	}
	switch r {
	case "ended": // a request ended it first: it wrote the end
		return Result{State: "clean", Blocking: blocking, Warnings: warnings}, nil
	case "superseded":
		if err := outcome(PatchEndedEarly); err != nil {
			return Result{}, err
		}
		return p.finish(ctx, in, "clean", "", blocking, warnings)
	}
	if err := outcome(PatchReverted); err != nil { // a revert that failed counts too: the patched app exited or restarted
		return Result{}, err
	}
	if r == "revert-failed" {
		return p.finish(ctx, in, "failed", "error", blocking, warnings)
	}
	return p.finish(ctx, in, "failed", "reverted", blocking, warnings)
}

// revertKeys are the lines a revert ends on, as dedupe keys: a replay that finds one answers it.
var revertKeys = []string{"patch:reverted", "patch:superseded", "patch:revert-failed"}

// revert puts back the image the patch replaced (contracts/routes.md, Patch runs, The revert). Under the project's
// undeploy lock, held across its checks and its /run as a writer of the brake holds it, one transaction under the
// ledger lock reads the run FOR UPDATE and moves it to reverting by compare-and-set: only while the run has not
// ended — a person's request ends a watch, and a start refuses a reverting run (StartTx) — so a request and a
// revert never both /run. A row no longer the patch's is left alone (superseded). Then the kept image is /run,
// and the row is the one the patch replaced, with the audit row revert patch <env>; a /run that fails leaves the
// patched image in place, its row the patch's (revert-failed, one of its fixed reasons, the cause logged). It
// answers ended, superseded, reverted or revert-failed. It is idempotent, so its step is retried: a retry finds
// its end written, or the row still the patch's and /runs the kept image again.
func (p *Pipeline) revert(ctx context.Context, in Input, env, how string, began time.Time) (string, error) {
	var done string
	if err := p.Pool.QueryRow(ctx, "SELECT coalesce((SELECT dedupe FROM timeline_rows WHERE run_id = $1 AND dedupe = ANY($2) LIMIT 1), '')", in.Run, revertKeys).Scan(&done); err != nil {
		return "", err
	}
	if done != "" {
		return done[len("patch:"):], nil
	}
	hook := func(at string) {
		if h := p.reverting.Load(); h != nil {
			(*h)(at)
		}
	}
	hook("before")
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	unlock, err := lockUndeployFor(ctx, conn, in.Project)
	if err != nil {
		return "", err
	}
	defer unlock()
	hook("locked")
	where := envWords[env]
	ran := time.Since(began)
	write := func(fn func(tx pgx.Tx) error) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if err := events.LockTx(ctx, tx); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	step := func(tx pgx.Tx, status string) error {
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.Progress, "watch:"+status, map[string]any{"step": "watch", "status": status, "duration_ms": ran.Milliseconds()})
		return err
	}
	var outcome string
	var spec *Egress
	var image *string
	if err := write(func(tx pgx.Tx) error {
		var ended bool
		if err := tx.QueryRow(ctx, "SELECT ended_at IS NOT NULL FROM runs WHERE id = $1 FOR UPDATE", in.Run).Scan(&ended); err != nil {
			return err
		}
		if ended { // a person's request ended the watch first: it goes ahead, and nothing is put back
			outcome = "ended"
			return nil
		}
		var ours bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = $2 AND gate_run = $3)", in.Project, env, in.patchRun()).Scan(&ours); err != nil {
			return err
		}
		if !ours {
			outcome = "superseded"
			if err := step(tx, "done"); err != nil {
				return err
			}
			_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "patch:superseded", map[string]any{"line": "The watch ended: " + where + " runs another version now, so nothing was put back"})
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE runs SET reverting_at = coalesce(reverting_at, now()) WHERE id = $1", in.Run); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT (patch_replaced->>'image'), (patch_replaced->'egress') FROM runs WHERE id = $1", in.Run).Scan(&image, &spec)
	}); err != nil || outcome != "" {
		return outcome, err
	}
	hook("cas")
	failed := func(reason string) (string, error) { // one of revert-failed's fixed reasons: never the executor's words
		return "revert-failed", write(func(tx pgx.Tx) error {
			if err := step(tx, "failed"); err != nil {
				return err
			}
			_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "patch:revert-failed",
				map[string]any{"line": "Canyon could not put back the image the patch replaced: " + reason + ". The patched image stays in " + where + ": deploy again to replace it"})
			return err
		})
	}
	if image == nil || spec == nil { // fail closed: never a /run of an image Canyon did not record
		return failed("Canyon did not record which image it was")
	}
	_, _, carried, err := p.RunApp(ctx, in.Run, in.Project, env, *image, *spec, "")
	if err != nil && ctx.Err() != nil {
		return "", err
	}
	if err != nil {
		log.Printf("runs: run %s (%s): the revert's /run: %v", in.Run, in.Project, err)
	}
	var exited *executor.Exited
	switch {
	case errors.Is(err, executor.ErrNoImage):
		return failed("it is no longer on this Canyon")
	case errors.As(err, &exited):
		return failed("it did not stay up")
	case err != nil:
		return failed("Canyon couldn't put the old image back")
	}
	return "reverted", write(func(tx pgx.Tx) error {
		// all a /run writes on the row: a patch of preview keeps its package, who deployed it and the rest, which a
		// promotion of it writes anew in production, so both put back the row as it was — but its trusted roots and
		// its container's start, which are this /run's (contracts/routes.md, Interception, Trust; #2738)
		tag, err := tx.Exec(ctx, `UPDATE deploys d SET package_id = o.package_id, image = o.image, url = o.url, gate_run = o.gate_run, deployed_at = o.deployed_at,
			deployed_by_kind = o.deployed_by_kind, deployed_by_subject = o.deployed_by_subject, deployed_by_display = o.deployed_by_display,
			promoted_by_kind = o.promoted_by_kind, promoted_by_subject = o.promoted_by_subject, promoted_by_display = o.promoted_by_display, run_by = o.run_by,
			build_digest = o.build_digest, buildpacks = o.buildpacks, egress = o.egress, values_hmac = o.values_hmac, values_key = o.values_key, builder = o.builder,
			run_started_at = o.run_started_at, answered = o.answered, run_config = o.run_config, run_config_sha256 = o.run_config_sha256, run_config_at = o.run_config_at,
			egress_roots = coalesce($5::text[], d.egress_roots), container_started_at = coalesce($6, o.container_started_at)
			FROM runs r, jsonb_populate_record(NULL::deploys, r.patch_replaced) o WHERE r.id = $1 AND d.project_id = $2 AND d.env = $3 AND d.gate_run = $4`, in.Run, in.Project, env, in.patchRun(), carried.Settled(), startedOf(&carried))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 { // the undeploy lock is held and a start refuses a reverting run: nothing else writes it
			return errors.New("the row the revert puts back is no longer the patch's")
		}
		var detail json.RawMessage
		if err := tx.QueryRow(ctx, "SELECT detail FROM audit_events WHERE run_id = $1 AND action = $2", in.Run, "patch "+env).Scan(&detail); err != nil {
			return err
		}
		if err := events.AuditTx(ctx, tx, PatchActor, "revert patch "+env, in.Project, in.Run, detail); err != nil {
			return err
		}
		if err := step(tx, "failed"); err != nil {
			return err
		}
		var at string
		if err := tx.QueryRow(ctx, "SELECT to_char(now() AT TIME ZONE 'UTC', 'HH24:MI')").Scan(&at); err != nil {
			return err
		}
		_, err = events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "patch:reverted",
			map[string]any{"line": "Canyon reverted " + where + " at " + at + " UTC: the patched app " + how + ", so the image it replaced runs again"})
		return err
	})
}

// lockUndeployFor takes the project's undeploy lock on conn for a revert, waiting while an undeploy, a hold or a
// recreation holds it: an undeploy or a hold ends the watch meanwhile, which the revert's compare-and-set reads.
func lockUndeployFor(ctx context.Context, conn *pgxpool.Conn, project string) (func(), error) {
	for {
		unlock, err := LockUndeploy(ctx, conn.Conn(), project)
		if !errors.Is(err, ErrUndeployBusy) || ctx.Err() != nil {
			return unlock, err
		}
	}
}

// watchingTx is the project's run whose watch runs (watchingSQL), read FOR UPDATE in tx under the ledger lock the
// caller took, and the environment it watches; "" when none does, one reverting among them.
func watchingTx(ctx context.Context, tx pgx.Tx, project string) (run, env string, err error) {
	err = tx.QueryRow(ctx, `SELECT id, CASE WHEN kind = 'promote' THEN 'prod' ELSE 'preview' END FROM runs
		WHERE project_id = $1 AND state IN ('pending', 'running') AND `+watchingSQL+` FOR UPDATE`, project).Scan(&run, &env)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return run, env, err
}

// endWatchTx ends a watch clean in tx, under the ledger lock the caller took (contracts/routes.md, Patch runs, A
// watch ends clean): its watch step done, line ended naming who and how, the run clean, and its app's outcome in
// its identity's waves ended_early — it proves nothing, so it files no patch approval and moves no wave on. The
// caller cancels its workflow after the commit; the image the patch replaced is no longer kept from the next
// /run of the app.
func endWatchTx(ctx context.Context, tx pgx.Tx, run, project string, by events.Actor, how string) error {
	var began *time.Time
	if err := tx.QueryRow(ctx, "SELECT min(ts) FROM timeline_rows WHERE run_id = $1 AND kind = 'progress' AND data->>'step' = 'watch'", run).Scan(&began); err != nil {
		return err
	}
	if began != nil {
		if _, err := events.AppendRunTx(ctx, tx, run, project, events.Progress, "watch:done", map[string]any{"step": "watch", "status": "done", "duration_ms": time.Since(*began).Milliseconds()}); err != nil {
			return err
		}
	}
	if _, err := events.AppendRunTx(ctx, tx, run, project, events.BuildLog, "patch:ended", map[string]any{"line": "The watch ended early: " + cmp.Or(by.Display, by.Subject) + " " + how}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE runs SET state = 'clean' WHERE id = $1", run); err != nil {
		return err
	}
	if _, err := events.AppendRunTx(ctx, tx, run, project, events.RunState, "state:clean", map[string]any{"state": "clean"}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE patch_wave_apps SET outcome = 'ended_early', outcome_at = now() WHERE run = $1 AND outcome = 'running'", run); err != nil {
		return err
	}
	return events.EndTx(ctx, tx, run)
}

// WatchingUntil is run's watch_until while it is a patch run whose watch runs — its watch begun, neither ended nor
// reverting — and nil otherwise: a check refused 409 run_in_progress then says until when (contracts/routes.md, the
// gate-runs row's watch, exactly a patch run's: a promote run's watch of production carries none).
func WatchingUntil(ctx context.Context, q Querier, run string) (*time.Time, error) {
	var until *time.Time
	err := q.QueryRow(ctx, "SELECT watch_until FROM runs WHERE id = $1 AND kind = 'patch' AND ended_at IS NULL AND reverting_at IS NULL", run).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return until, err
}

// EndWatchTx ends the project's watch clean, as endWatchTx says, for a request that is no start of a run — a delete
// of the project, which takes the environment it watches offline — under the ledger lock it takes: the run it
// ended, for the caller to cancel after the commit, or "" when no watch runs. One reverting is left as it is: the
// caller's own check refuses it.
func EndWatchTx(ctx context.Context, tx pgx.Tx, project string, by events.Actor) (string, error) {
	if err := events.LockTx(ctx, tx); err != nil {
		return "", err
	}
	run, env, err := watchingTx(ctx, tx, project)
	if err != nil || run == "" {
		return "", err
	}
	return run, endWatchTx(ctx, tx, run, project, by, "took "+envWords[env]+" offline")
}
