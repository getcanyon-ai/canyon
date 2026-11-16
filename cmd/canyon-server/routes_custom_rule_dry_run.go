package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/gate/custom"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
)

// A dry run's bounds (contracts/routes.md, the dry-run row): the apps it evaluates, how many at once, and
// how long it runs: under a front door's common 60-second read timeout, so its answer is never cut off
// (#2657). Vars, so a test can reach each without 50 apps or 50 seconds.
var (
	dryRunApps   = 50
	dryRunAtOnce = 4
	dryRunFor    = 50 * time.Second
)

// The dry run's reasons for an app it did not judge, in the routes row's words.
const (
	dryRunNoReview = "It has no review yet."
	dryRunTimedOut = "The dry run ran out of time."
	dryRunUnread   = "Canyon could not read the app's latest review."
	checkedByAI    = "This rule is checked by AI. A dry run checks a rule checked by code."
	dryRunBusy     = "Another dry run is running on this Canyon. Try again when it ends."
)

// dryRunLock takes the advisory lock a dry run holds for as long as it runs, so one runs at a time on the
// host, whichever server answers: a dry run fans out over every app. Named for the schema, as every lock
// here is, so two Canyons sharing a Postgres never hold each other's (#2657).
const dryRunLock = "SELECT pg_try_advisory_xact_lock(hashtext(current_schema() || ':custom-rule-dry-run'))"

// lockDryRun begins the transaction a dry run's lock is held in, and takes the lock: false when another
// dry run holds it. The lock is let go with the transaction, which a managed Postgres may end once it has
// been idle past its own timeout: it is idle while the apps are judged, so that timeout is off for it
// (#2657). The caller rolls tx back.
func lockDryRun(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}) (pgx.Tx, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	var mine bool
	if _, err = tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = 0"); err == nil {
		err = tx.QueryRow(ctx, dryRunLock).Scan(&mine)
	}
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, false, err
	}
	return tx, mine, nil
}

// dryRunApp is one app's line of a dry run.
type dryRunApp struct {
	Project string `json:"project"`
	Result  string `json:"result"`
	Why     string `json:"why,omitempty"`
}

// latestReviewed is each project's latest reviewed package: the package of its newest verdict that is no
// pull request's, check's, rollback review's or patch run's, as latestVerdicts picks the verdict.
const latestReviewed = `reviewed AS (SELECT DISTINCT ON (t.project_id) t.project_id, r.package_id, t.id FROM timeline_rows t
	JOIN runs r ON r.id = t.run_id JOIN packages pk ON pk.id = r.package_id AND pk.project_id = r.project_id AND pk.source_kind NOT IN ('repo-pr', 'check')
	WHERE t.kind = 'verdict' AND r.rollback_to IS NULL AND r.kind <> 'patch' ORDER BY t.project_id, t.id DESC)`

// dryRunCustomRule judges a custom rule's current version, checked by code, on every app's latest
// review as a review would (contracts/routes.md, the dry-run row): the 50 reviewed most recently, 4 at a
// time, for at most 50 seconds; every other app, and every one not judged in time, not checked. One
// at a time on the host, and an admin's alone (adminOnly).
func (a *app) dryRunCustomRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := customRulesOf(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: dry run: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	i := slices.IndexFunc(rules, func(c customRule) bool { return c.Rule == "custom."+r.PathValue("slug") })
	if i < 0 {
		writeErr(w, http.StatusNotFound, "not_found", "no such custom rule", nil)
		return
	}
	c := rules[i]
	if c.Check.By != custom.ByCode {
		writeErr(w, http.StatusConflict, "checked_by_ai", checkedByAI, nil)
		return
	}
	tx, mine, err := lockDryRun(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: dry run: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if !mine { // never queued: the admin asks again
		writeErr(w, http.StatusConflict, "dry_run_running", dryRunBusy, nil)
		return
	}
	check := c.Check
	rule := gatepolicy.Rule{ID: c.Rule, Name: c.Name, State: gatepolicy.Advisory, Producer: gatepolicy.Scanner, Instructions: c.Instructions, Check: &check}
	rows, err := a.pool.Query(ctx, `WITH `+latestReviewed+` SELECT p.id, coalesce(v.package_id, '') FROM projects p LEFT JOIN reviewed v ON v.project_id = p.id
		WHERE p.deleting_since IS NULL ORDER BY v.id DESC NULLS LAST, p.id`)
	if err != nil {
		a.log.Printf("canyon-server: dry run: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	apps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app [2]string, err error) { return app, row.Scan(&app[0], &app[1]) })
	if err != nil {
		a.log.Printf("canyon-server: dry run: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	judged, cancel := context.WithTimeout(ctx, dryRunFor)
	defer cancel()
	out := make([]dryRunApp, len(apps))
	slots := make(chan struct{}, dryRunAtOnce)
	var wg sync.WaitGroup
	n := 0
	for i, app := range apps {
		out[i] = dryRunApp{Project: app[0], Result: "not_checked"}
		switch {
		case app[1] == "":
			out[i].Why = dryRunNoReview
		case n == dryRunApps:
			out[i].Why = fmt.Sprintf("A dry run checks at most %d apps.", dryRunApps)
		default:
			n++
			wg.Go(func() {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
					out[i] = a.dryRunOn(judged, rule, app[0], app[1])
				case <-judged.Done():
					out[i].Why = dryRunTimedOut
				}
			})
		}
	}
	wg.Wait()
	slices.SortFunc(out, func(x, y dryRunApp) int { return strings.Compare(x.Project, y.Project) })
	writeJSON(w, http.StatusOK, map[string]any{"rule": c.Rule, "version": c.Version, "apps": out})
}

// dryRunOn is rule judged on project's package with the facts its review reads (runs.Pipeline.Facts),
// under the app's own trivy rule: a judgment ctx cut short is not checked, never a pass.
func (a *app) dryRunOn(ctx context.Context, rule gatepolicy.Rule, project, pkg string) dryRunApp {
	res := dryRunApp{Project: project, Result: "not_checked", Why: dryRunTimedOut}
	snap, err := a.projectPolicy(ctx, a.pool, project)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Printf("canyon-server: dry run of %s on %s: %v", rule.ID, project, err)
			res.Why = dryRunUnread
		}
		return res
	}
	in := runs.Input{Project: project, Package: pkg, Policy: gatepolicy.Snapshot{Rules: []gatepolicy.Rule{rule}}}
	if t := slices.IndexFunc(snap.Rules, func(r gatepolicy.Rule) bool { return r.ID == "sarif.trivy" }); t >= 0 {
		in.Policy.Rules = append(in.Policy.Rules, snap.Rules[t])
	}
	facts, err := a.pipe.Facts(ctx, in)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Printf("canyon-server: dry run of %s on %s: %v", rule.ID, project, err)
			res.Why = dryRunUnread
		}
		return res
	}
	result, why := custom.Judge(ctx, rule, facts)
	switch {
	case ctx.Err() != nil:
	case result == custom.Pass:
		res.Result, res.Why = "pass", ""
	case result == custom.Fail:
		res.Result, res.Why = "fail", ""
	default:
		res.Why = why
	}
	return res
}
