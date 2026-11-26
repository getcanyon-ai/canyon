package runs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// The host's work queues (contracts/finding.md, Canyon-run tools; routes.md, the deploys row; #1652):
// at most ScanConcurrency scans and BuildConcurrency builds run at once across every run and server
// replica, first come first served by when each was queued. grype, which the executor runs one scan of
// at a time on Kubernetes, has its own queue of one of those slots, so its scans never hold the others'. A wait
// is never a failure: a waiting step writes its place in line, and a cancel takes it out of the queue.
const (
	ScanQueue  = "scans"
	GrypeQueue = "scans-grype"
	BuildQueue = "builds"

	scanTaskName  = "scan"
	buildTaskName = "build"
)

// The capacities when the server's settings name none: Docker's (contracts/finding.md); deploy/k8s
// sets Kubernetes' own.
const (
	defaultScanConcurrency  = 2
	defaultBuildConcurrency = 1
)

// waitPoll is how often the server reads each waiting task's place in line, and waitLogged how long a
// wait runs before the server logs it, a capacity to raise; vars so a test can lower them.
var (
	waitPoll   = 10 * time.Second
	waitLogged = 10 * time.Minute
)

func queueOf(tool string) string {
	if tool == "grype" {
		return GrypeQueue
	}
	return ScanQueue
}

// registerQueues puts the scan and build tasks and their queues on dc.
func (p *Pipeline) registerQueues(dc dbos.Context) error {
	p.queues = map[string]dbos.Queue{}
	dbos.RegisterWorkflow(dc, p.scanTask, dbos.WithWorkflowName(scanTaskName), dbos.WithMaxRecoveryAttempts(3))
	dbos.RegisterWorkflow(dc, p.buildTask, dbos.WithWorkflowName(buildTaskName), dbos.WithMaxRecoveryAttempts(3))
	for name, n := range map[string]int{ScanQueue: positive(p.ScanConcurrency, defaultScanConcurrency) - 1, GrypeQueue: 1, BuildQueue: positive(p.BuildConcurrency, defaultBuildConcurrency)} {
		q, err := dbos.RegisterQueue(dc, name, dbos.WithGlobalConcurrency(n))
		if err != nil {
			return err
		}
		p.queues[name] = q
	}
	return nil
}

func positive(n, otherwise int) int {
	if n > 0 {
		return n
	}
	return otherwise
}

// progress writes the run's progress row for a step (contracts/events.md, progress): tool for a scan
// alone, position for queued alone, the running time for done and failed alone.
func (p *Pipeline) progress(ctx dbos.Context, in Input, step, tool, status string, position int, ran time.Duration) error {
	_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		data := map[string]any{"step": step, "status": status}
		if tool != "" {
			data["tool"] = tool
		}
		if position > 0 {
			data["position"] = position
		}
		if status == "done" || status == "failed" {
			data["duration_ms"] = ran.Milliseconds()
		}
		_, err := events.AppendRun(c, p.Pool, in.Run, in.Project, events.Progress, data)
		return struct{}{}, err
	}, dbos.WithStepName("progress"))
	return err
}

// began writes a step running and answers when, journaled, for ended's running time.
func (p *Pipeline) began(ctx dbos.Context, in Input, step string) (time.Time, error) {
	at, err := now(ctx)
	if err != nil {
		return at, err
	}
	return at, p.progress(ctx, in, step, "", "running", 0, 0)
}

// now is the time, journaled.
func now(ctx dbos.Context) (time.Time, error) {
	return dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now(), nil }, dbos.WithStepName("began"))
}

// ended writes a step done or failed, with its running time since began.
func (p *Pipeline) ended(ctx dbos.Context, in Input, step string, began time.Time, done bool) error {
	ran, err := dbos.RunAsStep(ctx, func(context.Context) (time.Duration, error) { return time.Since(began), nil }, dbos.WithStepName("ended"))
	if err != nil {
		return err
	}
	status := "failed"
	if done {
		status = "done"
	}
	return p.progress(ctx, in, step, "", status, 0, ran)
}

// unwritten journals a step named name that does nothing and answers the zero T, where the release before
// ran a step this one no longer needs: a run journaled there by it resumes under the same durableVersion
// (runs/durable.go).
func unwritten[T any](ctx dbos.Context, name string) error {
	_, err := dbos.RunAsStep(ctx, func(context.Context) (T, error) { var zero T; return zero, nil }, dbos.WithStepName(name))
	return err
}

// scanJob is one tool's scan of a run's package, the scan task's input.
type scanJob struct {
	In      Input
	Rule    policy.Rule
	Reports []int64
}

// scanTask is one scan, run when the queue gives it a slot: asked again inside its slot while the
// executor answers busy — then real overload — and never failed for it.
func (p *Pipeline) scanTask(ctx dbos.Context, j scanJob) (scanned, error) {
	tool := strings.TrimPrefix(j.Rule.ID, "sarif.")
	if stop, err := p.cancelled(ctx, j.In, false); err != nil || stop { // a cancel that landed before it was queued
		return scanned{}, cmpErr(err, errCancelled)
	}
	if err := p.progress(ctx, j.In, "scan", tool, "running", 0, 0); err != nil {
		return scanned{}, err
	}
	for waiting := false; ; {
		res, err := dbos.RunAsStep(ctx, func(c context.Context) (scanned, error) {
			started := time.Now()
			res, err := p.scan(c, j.In, j.Rule, j.Reports)
			res.Ran = time.Since(started) // a busy answer's is not read
			return res, err
		}, dbos.WithStepName("scan"))
		switch {
		case err != nil:
			return scanned{}, cmpErr(err, p.progress(ctx, j.In, "scan", tool, "failed", 0, res.Ran))
		case res.Busy == nil && res.Unfinished != "":
			return res, p.progress(ctx, j.In, "scan", tool, "failed", 0, res.Ran)
		case res.Busy == nil:
			return res, p.progress(ctx, j.In, "scan", tool, "done", 0, res.Ran)
		}
		if !waiting { // waiting on the executor, inside its slot: said once
			if err := p.progress(ctx, j.In, "scan", tool, "queued", 0, 0); err != nil {
				return scanned{}, err
			}
			waiting = true
		}
		if _, err := dbos.Sleep(ctx, res.Busy.RetryAfter); err != nil {
			return scanned{}, err
		}
		if stop, err := p.cancelled(ctx, j.In, false); err != nil || stop { // a cancel lands before the next ask
			return scanned{}, cmpErr(err, errCancelled)
		}
	}
}

// buildTask is the run's build, run when the queue gives it a slot, asked again inside it while the
// executor answers busy, never failed for it.
func (p *Pipeline) buildTask(ctx dbos.Context, in Input) (built, error) {
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop { // a cancel that landed before it was queued
		return built{}, cmpErr(err, errCancelled)
	}
	if err := p.progress(ctx, in, "build", "", "running", 0, 0); err != nil {
		return built{}, err
	}
	for waiting := false; ; {
		b, err := dbos.RunAsStep(ctx, func(c context.Context) (built, error) {
			started := time.Now()
			b, err := p.build(c, in)
			b.Ran = time.Since(started)
			return b, err
		}, dbos.WithStepName("build"))
		switch {
		case err != nil:
			return built{}, cmpErr(err, p.progress(ctx, in, "build", "", "failed", 0, b.Ran))
		case b.Busy == nil && b.Failed != "":
			return b, p.progress(ctx, in, "build", "", "failed", 0, b.Ran)
		case b.Busy == nil:
			return b, p.progress(ctx, in, "build", "", "done", 0, b.Ran)
		}
		if !waiting {
			if err := p.progress(ctx, in, "build", "", "queued", 0, 0); err != nil {
				return built{}, err
			}
			waiting = true
		}
		if _, err := dbos.Sleep(ctx, b.Busy.RetryAfter); err != nil {
			return built{}, err
		}
		if stop, err := p.cancelled(ctx, in, false); err != nil || stop { // a cancel lands before the next ask
			return built{}, cmpErr(err, errCancelled)
		}
	}
}

// errCancelled ends a task whose run a cancel was asked of; the run, reading it, ends interrupted.
var errCancelled = errors.New("runs: the run was cancelled")

func cmpErr(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

// tasksEnded waits for the tasks to end, reading their state between durable sleeps that grow from a
// second to waitPoll, so a short task is read at once and a long one costs few journal entries; a
// blocking GetResult on a queued task would outlive a shutdown. It answers once every one has ended.
func tasksEnded(ctx dbos.Context, ids ...string) error {
	if len(ids) == 0 { // no filter lists every workflow
		return nil
	}
	for nap := time.Second; ; nap = min(2*nap, waitPoll) {
		states, err := dbos.ListWorkflows(ctx, dbos.WithFilterWorkflowIDs(ids...), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			return err
		}
		ended := 0
		for _, w := range states {
			switch w.Status {
			case dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusPending, dbos.WorkflowStatusDelayed:
			default:
				ended++
			}
		}
		if ended == len(ids) {
			return nil
		}
		if _, err := dbos.Sleep(ctx, nap); err != nil {
			return err
		}
	}
}

// Dequeue takes every task of the run still waiting in a queue out of it: a cancel lands before any of
// them runs, and the run, awaiting them, ends interrupted (contracts/finding.md, Canyon-run tools). A task
// already running ends its step as before.
func Dequeue(dc dbos.Client, run string) error {
	waiting, err := dbos.ListWorkflows(dc, dbos.WithFilterWorkflowIDPrefix(run+"-"), dbos.WithFilterStatus(dbos.WorkflowStatusEnqueued),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return err
	}
	for _, w := range waiting {
		if err := dbos.CancelWorkflow(dc, w.ID); err != nil {
			return err
		}
	}
	return nil
}

// ReportQueues writes, every waitPoll until ctx ends, each waiting task's place in its queue onto its
// run as a queued progress row when it changes (contracts/events.md, progress), and logs once a task that
// has waited past waitLogged: a capacity to raise. Every server replica runs it; a place already written
// is never written twice (the row's dedupe), so replicas never double a row, and a replica writes only a
// place it has not written itself, so an unchanged queue takes no lock.
func (p *Pipeline) ReportQueues(ctx context.Context, dc dbos.Client) {
	placed, logged := map[string]int{}, map[string]bool{}
	for tick := time.NewTicker(waitPoll); ; {
		seen := map[string]bool{}
		for _, q := range []string{ScanQueue, GrypeQueue, BuildQueue} {
			waiting, err := dbos.ListWorkflows(dc, dbos.WithFilterQueueName(q), dbos.WithFilterStatus(dbos.WorkflowStatusEnqueued),
				dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
			if err != nil {
				log.Printf("runs: the %s queue: %v", q, err)
				continue
			}
			for i, w := range waiting { // oldest first: the order the queue takes them in
				run, step, tool, ok := taskOf(w.ID)
				if !ok {
					continue
				}
				seen[w.ID] = true
				if placed[w.ID] != i+1 {
					if err := p.queued(ctx, run, step, tool, i+1); err != nil {
						log.Printf("runs: run %s's place in the %s queue: %v", run, q, err)
					} else {
						placed[w.ID] = i + 1
					}
				}
				if waited := time.Since(w.CreatedAt); waited > waitLogged && !logged[w.ID] {
					log.Printf("runs: run %s has waited %s for a %s slot: raise CANYON_SCAN_CONCURRENCY or CANYON_BUILD_CONCURRENCY if the host can take more", run, waited.Round(time.Second), step)
					logged[w.ID] = true
				}
			}
		}
		for id := range placed { // a task no longer waiting is forgotten
			if !seen[id] {
				delete(placed, id)
				delete(logged, id)
			}
		}
		select {
		case <-ctx.Done():
			tick.Stop()
			return
		case <-tick.C:
		}
	}
}

// taskOf reads a task's id, <run>-scan-<tool> or <run>-build, back into its run, step and tool.
func taskOf(id string) (run, step, tool string, ok bool) {
	if run, ok = strings.CutSuffix(id, "-build"); ok {
		return run, "build", "", true
	}
	run, tool, ok = strings.Cut(id, "-scan-")
	return run, "scan", tool, ok
}

// queued writes the task's place in line once: the dedupe key carries the place. A task that has
// begun — it started between the queue's read and this write — keeps its own rows newest.
func (p *Pipeline) queued(ctx context.Context, run, step, tool string, place int) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		var project string
		var ended, begun bool
		if err := tx.QueryRow(ctx, `SELECT project_id, ended_at IS NOT NULL, EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND kind = 'progress'
			AND data->>'step' = $2 AND COALESCE(data->>'tool', '') = $3 AND data->>'status' <> 'queued') FROM runs WHERE id = $1`, run, step, tool).Scan(&project, &ended, &begun); err != nil || ended || begun {
			return err
		}
		data := map[string]any{"step": step, "status": "queued", "position": place}
		if tool != "" {
			data["tool"] = tool
		}
		_, err := events.AppendRunTx(ctx, tx, run, project, events.Progress, fmt.Sprintf("queued:%s:%s:%d", step, tool, place), data)
		return err
	})
}
