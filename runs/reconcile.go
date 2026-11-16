package runs

import (
	"context"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// Reconcile ends every run the driver can no longer finish: a run the table calls live
// (pending or running) whose workflow — a patch run's watch's once it has one — DBOS has given up
// on, errored, cancelled, or never recorded. Each ends failed, interrupted, through the same writer as the workflow's terminal
// step — or, an undeploy having recorded a stop on it, carries that stop out first and ends as the
// brake ends a run (stopNow). It runs once after dbos.Launch and before this server's listener opens, so a run this
// server resumes and a run this sweep ends never race, and a stuck run never holds the project's
// one-active-run index past a restart. Returns how many runs it ended.
func (p *Pipeline) Reconcile(ctx context.Context, dc dbos.Client) (int, error) {
	rows, err := p.Pool.Query(ctx, "SELECT id, project_id, package_id, kind, deploys FROM runs WHERE state IN ('pending', 'running') ORDER BY created_at")
	if err != nil {
		return 0, err
	}
	var live []liveRun
	for rows.Next() {
		var l liveRun
		var kind string
		var deploys bool
		if err := rows.Scan(&l.Run, &l.Project, &l.Package, &kind, &deploys); err != nil {
			rows.Close()
			return 0, err
		}
		l.env = Writes(kind, deploys)
		live = append(live, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(live) == 0 {
		return 0, err
	}
	ids := make([]string, 0, 2*len(live))
	for _, in := range live {
		ids = append(ids, in.Run, watchID(in.Run))
	}
	found, err := dc.ListWorkflows(dc, dbos.WithFilterWorkflowIDs(ids...), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return 0, fmt.Errorf("runs: reconcile: %w", err)
	}
	status := map[string]dbos.WorkflowStatusType{}
	for _, w := range found {
		status[w.ID] = w.Status
	}
	ended := 0
	for _, in := range live {
		st, ok := status[watchID(in.Run)] // a patch run past its /run is its watch's to drive (#2567)
		if !ok {
			st, ok = status[in.Run]
		}
		if ok && st != dbos.WorkflowStatusError && st != dbos.WorkflowStatusCancelled && st != dbos.WorkflowStatusMaxRecoveryAttemptsExceeded {
			// enqueued, pending or delayed: DBOS will drive it. SUCCESS is left alone too: the runs
			// table and the statuses are two reads, so a run that finished between them reads
			// live-then-succeeded, and ending it would trip on its own terminal row.
			continue
		}
		state, reason, step, err := p.stopNow(ctx, dc, in.Input, in.env)
		if err != nil {
			return ended, err
		}
		if err := p.End(ctx, in.Input, state, reason, step); err != nil {
			return ended, err
		}
		if err := Dequeue(dc, in.Run); err != nil { // its waiting tasks would run for an ended run
			return ended, err
		}
		ended++
	}
	return ended, nil
}

// liveRun is a run the table calls live, and the environment it writes.
type liveRun struct {
	Input
	env string
}
