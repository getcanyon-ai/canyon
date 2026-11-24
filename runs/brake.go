package runs

import (
	"cmp"
	"context"
	"errors"
	"log"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// UndeployAction is the undeploy route's audit action: a run that carries out a stop writes the
// route's row (contracts/routes.md, The brake).
const UndeployAction = "DELETE /api/v1/projects/{id}/envs/{env}"

// StopOf is what a stop did to what ran, the `stop` of its audit row (contracts/routes.md, the undeploy row): Canyon
// stopped what it ran, a remove stands for what the org's platform runs, or nothing ran there. A stop Canyon couldn't
// make is `failed`, which its caller says itself.
func StopOf(canyon, removal bool) string {
	switch {
	case canyon:
		return "stopped"
	case removal:
		return "remove_sent"
	}
	return "nothing"
}

// stopRetries is how often a run tries again to stop its environment before it ends failed/error, and
// stopEvery the server-start reconcile's wait between its tries; variables so a test need not wait.
var (
	stopRetries = 10
	stopEvery   = 2 * time.Second
)

// stopCeiling bounds one stop the server-start reconcile tries: the executor's /stop of one app.
const stopCeiling = 30 * time.Second

// errBraked is a run's release step meeting an undeploy: the run ends through the brake.
var errBraked = errors.New("runs: its environment was taken offline")

// Writes is the environment a run writes: prod for a promote run, preview for a deploy run and a patch run
// (contracts/routes.md, Patch runs), none for a check. ShownDeploys's CASE says the same in SQL, and
// Input.writes off a workflow's input: change one, change all three.
func Writes(kind string, deploys bool) string {
	switch {
	case kind == "promote":
		return "prod"
	case deploys, kind == "patch":
		return "preview"
	}
	return ""
}

// writes is in's environment, as Writes reads it off the row.
func (in Input) writes() string {
	kind := "gate"
	switch {
	case in.Promote != nil:
		kind = "promote"
	case in.Patch != nil:
		kind = "patch"
	}
	return Writes(kind, in.Deploy)
}

// ShownDeploys is the deploys rows the project read, the status read, the fleet and the receipt read (contracts/routes.md,
// The brake, What reads it): a row Canyon runs leaves envs once the run writing its environment, carrying out
// a stop an undeploy recorded on it, has begun ending, or while an undeploy or a hold with no run stops it
// itself — the row marked stopping_since, its undeploy lock held — so it never reads stopped while it is taken offline.
// The row itself stays until the stop lands, and reads again when the run ends, or the lock is let go, without
// landing it. The mark is read first, so only a marked row tries the lock: tryUndeploy's, on the row's project.
const ShownDeploys = `(SELECT * FROM deploys d WHERE d.run_by = 'org' OR (CASE WHEN d.stopping_since IS NULL THEN true
  ELSE pg_try_advisory_xact_lock_shared(hashtext(current_schema() || ':undeploy:' || d.project_id)) END) AND NOT EXISTS (SELECT 1 FROM runs b WHERE b.project_id = d.project_id
  AND b.state IN ('pending', 'running') AND b.stop_requested_at IS NOT NULL AND b.ending_at IS NOT NULL
  AND d.env = CASE WHEN b.kind = 'promote' THEN 'prod' WHEN b.deploys OR b.kind = 'patch' THEN 'preview' END))` // the CASE is Writes, in SQL: change one, change both

// Braked is what an undeploy's first step did to the run writing its environment.
type Braked struct {
	Run      *Run   // the run writing the environment, nil when there is none
	Ended    string // the pending run it ended: cancel its workflow after the commit
	Asked    bool   // the running run it asked to end: take its tasks out of the queue after the commit
	Stopping bool   // a stop is pending on Run, which carries it out: the route answers 202 stopping
}

// BrakeTx is the undeploy's first step (contracts/routes.md, The brake), inside the caller's
// transaction, which commits before any executor call: the run writing env is ended when pending, or a patch
// run before its /run, or its watch — a patch run's, or a promote run's shipping a patch — ended clean once its
// /run is recorded,
// asked to end before its deploy step, and past it, neither its deploy.completed row written nor the
// run already ending, has the stop recorded on it — pending when it has sent no release, else the
// undeploy's remove supersedes that release. A check and the other environment's runs are left alone.
// The request undeploy row is written when the run changes: asking again writes nothing, and a hold's
// brake (hold) writes none, the hold being the record (contracts/routes.md, The hold).
func BrakeTx(ctx context.Context, tx pgx.Tx, project, env string, by events.Actor, hold bool) (Braked, error) {
	if err := events.LockTx(ctx, tx); err != nil {
		return Braked{}, err
	}
	var run Run
	var kind string
	var deploys, begun, ending, stopAsked, completed, released, reverting, shipsPatch bool
	err := tx.QueryRow(ctx, `SELECT id, state, kind, deploys, deploy_started_at IS NOT NULL, ending_at IS NOT NULL, stop_requested_at IS NOT NULL,
		EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = runs.id AND dedupe = 'deploy:' || $2),
		EXISTS (SELECT 1 FROM releases WHERE run_id = runs.id AND event = 'deploy'), reverting_at IS NOT NULL, `+shipsPatchSQL+`
		FROM runs WHERE project_id = $1 AND state IN ('pending', 'running') FOR UPDATE`, project, env).
		Scan(&run.ID, &run.State, &kind, &deploys, &begun, &ending, &stopAsked, &completed, &released, &reverting, &shipsPatch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Braked{}, nil
	}
	if err != nil || Writes(kind, deploys) != env {
		return Braked{}, err
	}
	b := Braked{Run: &run}
	switch {
	case run.State == "pending", kind == "patch" && !begun: // a patch run before its /run is ended as a pending run is (contracts/routes.md, Patch runs)
		if err := EndInterrupted(ctx, tx, run.ID, project); err != nil {
			return Braked{}, err
		}
		b.Ended, run.State = run.ID, "failed"
	case shipsPatch && completed && !ending && !reverting: // its watch runs, a patch run's or a promote run's shipping a patch: the undeploy or the hold ends it clean (contracts/routes.md, Patch runs)
		how := "took " + envWords[env] + " offline"
		if hold {
			how = "stopped production"
		}
		if err := endWatchTx(ctx, tx, run.ID, project, by, how); err != nil {
			return Braked{}, err
		}
		b.Ended, run.State = run.ID, "clean"
	case stopAsked: // asked again: the first who asked is kept
		b.Stopping = !released
		return b, nil
	case ending || completed: // it touches env no more, or what it wrote is recorded: the route stops what is there
		return b, nil
	case !begun:
		tag, err := tx.Exec(ctx, "UPDATE runs SET cancel_requested_at = now() WHERE id = $1 AND cancel_requested_at IS NULL", run.ID)
		if err != nil {
			return Braked{}, err
		}
		b.Asked = true
		if tag.RowsAffected() == 0 { // asked already
			return b, nil
		}
	default:
		if _, err := tx.Exec(ctx, "UPDATE runs SET stop_requested_at = now(), stop_requested_by_kind = NULLIF($2, ''), stop_requested_by_subject = $3, stop_requested_by_display = $4 WHERE id = $1",
			run.ID, by.Kind, by.Subject, by.Display); err != nil {
			return Braked{}, err
		}
		b.Stopping = !released
	}
	if hold {
		return b, nil
	}
	return b, events.AuditTx(ctx, tx, by, "request undeploy", project, run.ID, map[string]any{"env": env, "run": run.ID})
}

// stopAsk is the stop an undeploy recorded on a run, read as the run ends.
type stopAsk struct {
	Asked    bool         `json:"asked,omitempty"`
	By       events.Actor `json:"by"`
	Released bool         `json:"released,omitempty"` // the run had sent its release: the undeploy's remove supersedes it
}

// ending marks run ending and reads the stop recorded on it, in one write under the ledger lock the
// undeploy's first step takes: a stop is either recorded before, and carried out here, or meets an
// ending run, which the undeploy treats as done and stops what is there itself.
func (p *Pipeline) ending(ctx context.Context, run string) (stopAsk, error) {
	var s stopAsk
	err := p.write(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE runs SET ending_at = coalesce(ending_at, now()) WHERE id = $1
			RETURNING stop_requested_at IS NOT NULL, coalesce(stop_requested_by_kind, ''), coalesce(stop_requested_by_subject, ''), coalesce(stop_requested_by_display, ''),
			EXISTS (SELECT 1 FROM releases WHERE run_id = $1 AND event = 'deploy')`, run).Scan(&s.Asked, &s.By.Kind, &s.By.Subject, &s.By.Display, &s.Released)
	})
	return s, err
}

// stopOnEnd is a run's end meeting a stop recorded on it: carried out first, the end then the brake's.
// Only a run that deploys can carry one: the stop is recorded past its deploy step.
func (p *Pipeline) stopOnEnd(ctx dbos.Context, in Input, state, reason, step string) (string, string, string, error) {
	env := in.writes()
	if env == "" {
		return state, reason, step, nil
	}
	ask, err := dbos.RunAsStep(ctx, func(c context.Context) (stopAsk, error) { return p.ending(c, in.Run) }, dbos.WithStepName("ending"))
	if err != nil || !ask.Asked {
		return state, reason, step, err
	}
	stopped := true
	if !ask.Released {
		_, serr := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.stopEnv(c, in.Project, env) },
			dbos.WithStepName("stop"), dbos.WithStepMaxRetries(stopRetries))
		if serr != nil && (ctx.Err() != nil || errors.Is(serr, dbos.ErrWorkflowCancelled)) {
			return "", "", "", serr
		}
		if serr != nil {
			log.Printf("runs: run %s (%s): stopping %s: %v", in.Run, in.Project, env, serr)
		}
		stopped = serr == nil
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		return struct{}{}, p.stopped(c, p.dc, in, env, ask, stopped)
	}, dbos.WithStepName("stopped")); err != nil {
		return "", "", "", err
	}
	return braked(stopped)
}

// braked is how a run that met the brake ends: interrupted, or Canyon's own failure in its deploy
// when the stop failed past its retries.
func braked(stopped bool) (state, reason, step string, err error) {
	if !stopped {
		return "failed", "error", "deploy", nil
	}
	return "failed", "interrupted", "", nil
}

// stopEnv stops env's container, unless the org's platform runs env: its remove goes with the record.
func (p *Pipeline) stopEnv(ctx context.Context, project, env string) error {
	var runBy string
	if err := p.Pool.QueryRow(ctx, "SELECT run_by FROM deploys WHERE project_id = $1 AND env = $2", project, env).Scan(&runBy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if runBy == "org" {
		return nil
	}
	return p.Executor.Stop(ctx, project, env)
}

// stopped records a stop the run carried out: env off envs — the row deleted, or under run_by: org a
// remove sent — and the route's audit row naming who asked; or, the stop failed, the row as it was,
// saying what runs there. Then the run's line (contracts/events.md, the brake's lines). Recorded once:
// a replay finds the line and writes nothing.
func (p *Pipeline) stopped(ctx context.Context, dc dbos.Client, in Input, env string, ask stopAsk, ok bool) error {
	who := cmp.Or(ask.By.Display, ask.By.Subject)
	where := map[string]string{"preview": "preview", "prod": "production"}[env]
	return p.write(ctx, func(tx pgx.Tx) error {
		var done bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'brake')", in.Run).Scan(&done); err != nil || done {
			return err
		}
		var hold *Hold    // a hold met the run mid-switch: its stop is the hold's (contracts/routes.md, The hold)
		var digest string // the image it was switching in, off its row: the reconcile's Input carries no Promote
		if env == "prod" {
			var err error
			if hold, err = ReadHold(ctx, tx, in.Project); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT coalesce(split_part(promote_image, '@', 2), '') FROM runs WHERE id = $1", in.Run).Scan(&digest); err != nil {
				return err
			}
		}
		var line string
		audit, stop := false, ""
		if !ok {
			var version string
			if err := tx.QueryRow(ctx, "SELECT coalesce(label, substr(id, 5)) FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&version); err != nil {
				return err
			}
			line = "Taken offline while deploying, as " + who + " asked, but Canyon could not stop " + where + ": " + version +
				" runs there. Take it offline again with canyon undeploy --env " + env
			if hold != nil { // no undeploy is allowed while held: an admin looks the run up, and the trail says the stop failed
				line = heldStopLine(version, in.Run)
				if err := events.AuditTx(ctx, tx, hold.By, "stop app", in.Project, in.Run, map[string]any{"env": env, "package": in.Package, "image_digest": digest, "reason": hold.Reason, "cancelled": []string{in.Run}, "stop": "failed", "stopped": false}); err != nil {
					return err
				}
			}
		} else {
			var runBy string
			if err := tx.QueryRow(ctx, "SELECT run_by FROM deploys WHERE project_id = $1 AND env = $2 FOR UPDATE", in.Project, env).Scan(&runBy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			var orgMode bool // read on this transaction: a second connection, waited for under the ledger lock, would starve the pool
			if err := tx.QueryRow(ctx, "SELECT run_by = 'org' FROM org_preferences").Scan(&orgMode); err != nil {
				return err
			}
			open, err := OpenRelease(ctx, tx, in.Project, env)
			if err != nil {
				return err
			}
			// as the undeploy route does: a row Canyon ran goes, and an environment the org's platform runs, or
			// has a release of open — this run's, or one an earlier run left open — gets a remove
			if _, err := tx.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1 AND env = $2 AND run_by = 'canyon'", in.Project, env); err != nil {
				return err
			}
			handedOff := ask.Released || runBy == "org" || open.ID != ""
			audit = runBy != "org" || !handedOff
			removed := false
			if handedOff {
				_, sent, err := SendRemoveTx(ctx, tx, dc, in.Project, env)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				audit, removed = (audit && !ask.Released) || sent, err == nil
			}
			if hold != nil && ask.Released && !removed { // held, what it handed the platform may have gone live after the hold's remove completed: a remove of it goes out again
				var handed string
				err := tx.QueryRow(ctx, "SELECT id FROM releases WHERE run_id = $1 AND event = 'deploy' ORDER BY created_at DESC LIMIT 1", in.Run).Scan(&handed)
				if err == nil {
					_, _, err = SendRemoveOfTx(ctx, tx, dc, handed)
					removed = err == nil
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			stop = StopOf(!ask.Released && !orgMode, removed) // what this run handed the platform, or never handed it, Canyon didn't run
			switch {
			case ask.Released && removed:
				line = "Taken offline while your platform was deploying, as " + who + " asked: Canyon sent it a remove"
			case ask.Released:
				line = "Taken offline while your platform was deploying, as " + who + " asked: nothing of it went live"
			case orgMode:
				line = "Taken offline before the release was sent, as " + who + " asked: nothing was handed to your platform"
			default:
				line = "Taken offline while deploying, as " + who + " asked: Canyon stopped " + where
			}
		}
		if audit && hold != nil {
			if err := events.AuditTx(ctx, tx, hold.By, "stop app", in.Project, in.Run, map[string]any{"env": env, "package": in.Package, "image_digest": digest, "reason": hold.Reason, "cancelled": []string{in.Run}, "stop": stop}); err != nil {
				return err
			}
		}
		if audit && (hold == nil || ask.By.Kind != hold.By.Kind || ask.By.Subject != hold.By.Subject) { // an undeploy asked first: its row too
			if err := events.AuditTx(ctx, tx, ask.By, UndeployAction, in.Project, in.Run, map[string]any{"env": env, "package": in.Package, "run": in.Run, "stop": stop}); err != nil {
				return err
			}
		}
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "brake", map[string]any{"line": line})
		return err
	})
}

// stopNow is stopOnEnd for a run the server-start reconcile ends, DBOS having given up on it: the stop
// recorded on it is carried out here — tried a few times, an executor rolled with the server may not
// be up yet — and the end is the brake's; with none, it ends interrupted.
func (p *Pipeline) stopNow(ctx context.Context, dc dbos.Client, in Input, env string) (state, reason, step string, err error) {
	if env == "" {
		return "failed", "interrupted", "", nil
	}
	ask, err := p.ending(ctx, in.Run)
	if err != nil || !ask.Asked {
		return "failed", "interrupted", "", err
	}
	stopped := true
	if !ask.Released {
		try := func() error { // each try bounded, so an executor that never answers cannot hold the server's start
			call, cancel := context.WithTimeout(ctx, stopCeiling)
			defer cancel()
			return p.stopEnv(call, in.Project, env)
		}
		serr := try()
		for n := 1; serr != nil && n < 3 && ctx.Err() == nil; n++ {
			time.Sleep(stopEvery)
			serr = try()
		}
		if serr != nil {
			log.Printf("runs: run %s (%s): stopping %s: %v", in.Run, in.Project, env, serr)
		}
		stopped = serr == nil
	}
	if err := p.stopped(ctx, dc, in, env, ask, stopped); err != nil {
		return "", "", "", err
	}
	return braked(stopped)
}
