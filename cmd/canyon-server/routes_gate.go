package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

func routesGate(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects/{id}/gate-runs", a.teamOnly(func(w http.ResponseWriter, r *http.Request) { a.startGateRun(w, r, false) }))
	api.HandleFunc("POST /projects/{id}/deploys", a.teamOnly(func(w http.ResponseWriter, r *http.Request) { a.startGateRun(w, r, true) })) // a deploy IS a gate run that continues into build and run on clean (contracts/routes.md row 62)
	// the run read, its SARIF and the runs list are a responder's too, with no grant: what approving needs (#2204)
	api.HandleFunc("GET /gate-runs/{id}", a.runReviewOnly(a.getGateRun))
	api.HandleFunc("POST /gate-runs/{id}/cancel", a.runTeamOnly(a.cancelRun))
	api.HandleFunc("GET /gate-runs/{id}/sarif", a.runReviewOnly(a.getGateRunSARIF))
	api.HandleFunc("GET /projects/{id}/gate-runs", a.reviewOnly(a.listGateRuns))
}

// runRow is one row of the gate-runs list (contracts/routes.md): the run read without its
// findings, plus who started it and when. The counts are the verdict's, whatever came after it.
type runRow struct {
	ID            string       `json:"id"`
	Package       string       `json:"package"`
	Label         *string      `json:"label,omitempty"` // the reviewed package's label, so a run row names a version (contracts/routes.md, *Naming a version*)
	Check         bool         `json:"check,omitempty"` // the run reviewed a check's package: never a version of the app
	State         string       `json:"state"`
	FailureReason *string      `json:"failure_reason,omitempty"`
	FailedIn      *string      `json:"failed_in,omitempty"` // the step a run Canyon itself failed in (contracts/routes.md, A failure reason in words)
	Blocking      int          `json:"blocking"`
	Warnings      int          `json:"warnings"`
	StartedBy     *actor       `json:"started_by"`
	StartedAt     string       `json:"started_at"`
	EndedAt       *string      `json:"ended_at"`
	PullRequest   *pullRequest `json:"pull_request,omitempty"` // a run of a pull request's package (#1947)
	Approved      bool         `json:"approved,omitempty"`     // a blocked run every blocking finding of which an approval covers now (#1955)
	findings      json.RawMessage
}

// pullRequest names the pull request a package came from (contracts/routes.md; #1947).
type pullRequest struct {
	Number int    `json:"number"`
	Branch string `json:"branch"`
}

// runsPageSQL is one page of a project's runs, newest first, from the cursor (an empty id is the
// start), one row past the limit; runs_latest serves it, the verdict row is one lookup on
// timeline_rows_dedupe, and a run without one counts zero.
const runsPageSQL = `SELECT r.id, r.package_id, p.label, p.source_kind IS NOT DISTINCT FROM 'check', r.state, r.failure_reason, r.failed_in,
       COALESCE((v.data->>'blocking')::int, 0), COALESCE((v.data->>'warnings')::int, 0),
       r.started_by_kind, r.started_by_subject, r.started_by_display, r.created_at, r.ended_at,
       p.source_kind IS NOT DISTINCT FROM 'repo-pr', COALESCE(p.pr_number, 0), COALESCE(p.pr_branch, ''), v.data->'findings'
FROM runs r
LEFT JOIN packages p ON p.project_id = r.project_id AND p.id = r.package_id
LEFT JOIN LATERAL (SELECT data FROM timeline_rows t WHERE t.run_id = r.id AND t.dedupe = 'verdict') v ON true
WHERE r.project_id = $1 AND r.kind = 'gate' AND ($3::text = '' OR (r.created_at, r.id) < ($2::timestamptz, $3))
ORDER BY r.created_at DESC, r.id DESC LIMIT $4`

// listGateRuns answers one page of the live project's runs. A read, so no audit row.
func (a *app) listGateRuns(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	after, limit, ok := pageQuery(w, r.URL.Query())
	if !ok {
		return
	}
	rows, err := a.pool.Query(ctx, runsPageSQL, id, after.ts, after.id, limit+1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer rows.Close()
	list, keys := []runRow{}, []pageCursor{}
	for rows.Next() {
		var row runRow
		var kind, subject, display *string
		var at time.Time
		var ended *time.Time
		var pr pullRequest
		var fromPR bool
		if err := rows.Scan(&row.ID, &row.Package, &row.Label, &row.Check, &row.State, &row.FailureReason, &row.FailedIn, &row.Blocking, &row.Warnings, &kind, &subject, &display, &at, &ended, &fromPR, &pr.Number, &pr.Branch, &row.findings); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if kind != nil { // the start writes the three together; a run from before the column answers null
			row.StartedBy = &actor{Kind: *kind, Subject: *subject, Display: *display}
		}
		row.StartedAt = at.UTC().Format(time.RFC3339)
		if fromPR {
			row.PullRequest = &pr
		}
		if ended != nil {
			s := ended.UTC().Format(time.RFC3339)
			row.EndedAt = &s
		}
		list, keys = append(list, row), append(keys, pageCursor{at, row.ID})
	}
	if rows.Err() != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	for i := range list { // a blocked run's approvals as they stand now: an approval expires, or another is given
		if list[i].State != "blocked" || list[i].findings == nil {
			continue
		}
		_, unapproved, err := runs.Approvals(ctx, a.pool, id, list[i].ID, list[i].findings)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		list[i].Approved = unapproved == 0
	}
	list, next := trim(list, keys, limit)
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "next": next})
}

// startGateRun records a run of the project's package (the current one unless named), enqueues
// its workflow and writes the audit row naming the run, all in one transaction: a 409 or a 429
// leaves nothing.
func (a *app) startGateRun(w http.ResponseWriter, r *http.Request, deploy bool) {
	ctx, id := r.Context(), r.PathValue("id")
	if !runs.ValidProject.MatchString(id) {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	var body struct {
		Package    string  `json:"package"`
		RollbackTo string  `json:"rollback_to"` // a rollback review of that package (contracts/routes.md, the gate-runs row)
		Reason     *string `json:"reason"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is not one JSON object", nil)
		return
	}
	if len(bytes.TrimSpace(raw)) != 0 { // no body, however it was sent, means the current package
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	reason := ""
	switch {
	case deploy && (body.RollbackTo != "" || body.Reason != nil):
		writeErr(w, http.StatusBadRequest, "bad_request", "a deploy takes no rollback_to or reason: a rollback review is a gate run", nil)
		return
	case body.Reason != nil:
		reason = *body.Reason
		hides := func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) } // an escape, a bidi override, a NUL Postgres text refuses
		if n := utf8.RuneCountInString(reason); body.RollbackTo == "" || n == 0 || n > 1024 || strings.ContainsFunc(reason, hides) {
			writeErr(w, http.StatusBadRequest, "invalid_reason", "a reason is 1 to 1024 characters of plain text, beside rollback_to", nil)
			return
		}
	}
	for attempt := range 2 { // a run that is running when the start looks and ended before the 409 is read: look again, once
		run, superseded, busy, err := a.startRun(ctx, r, id, body.Package, deploy, body.RollbackTo, reason)
		switch {
		case err != nil:
			var refusal *refused
			if errors.As(err, &refusal) {
				writeErr(w, refusal.status, refusal.code, refusal.message, nil)
				return
			}
			var limit *runs.RateLimited
			if errors.As(err, &limit) {
				writeErr(w, http.StatusTooManyRequests, "rate_limited", limitMessage(limit, id), map[string]any{"retry_after_s": int(limit.RetryAfter.Seconds())})
				return
			}
			a.log.Printf("canyon-server: start a run of %s: %v", id, err)
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		case busy != nil:
			extra := map[string]any{"run": busy}
			until, err := runs.WatchingUntil(ctx, a.pool, busy.ID) // Canyon's patch watch: until when, which a deploy ends now (contracts/routes.md, the gate-runs row)
			if err != nil {
				a.log.Printf("canyon-server: the watch of %s's run %s: %v", id, busy.ID, err)
			}
			if until != nil {
				extra["watch"] = map[string]string{"until": until.UTC().Format(time.RFC3339)}
			}
			writeErr(w, http.StatusConflict, "run_in_progress", "a run of this project is running", extra)
			return
		case run.ID == "" && attempt == 0:
			continue
		case run.ID == "":
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		runs.Cancel(a.durable, superseded)
		writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
		return
	}
}

// limitMessage is the 429's plain words: whose limit, and in how many minutes, rounded up.
func limitMessage(e *runs.RateLimited, project string) string {
	whose := project
	if e.PerActor {
		whose = "you"
	}
	minutes := int(math.Ceil(e.RetryAfter.Minutes()))
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("Limit reached: %d reviews an hour for %s. Try again in %d %s.", e.Limit, whose, minutes, unit)
}

// refused is a start the transaction turned away: the contract's 404s, and 409 deleting or undeploying.
type refused struct {
	status        int
	code, message string
}

func (e *refused) Error() string { return e.message }

// undeploying is a start refused while an undeploy of the project holds its lock (runs.StartTx).
func undeploying(id string) *refused {
	return &refused{http.StatusConflict, "undeploying", "An undeploy of " + id + " is in progress; try again in a moment."}
}

// startRun is one attempt inside one transaction. It returns the run and the superseded run's
// id on success; busy when another run of the project is active; an empty run and no error
// when the start was refused for a run that had ended by the time it was read back, which the
// caller retries once; a refused error for the contract's 404s, a project being deleted and one
// being undeployed, and a rollback review of an image production did not run for that package.
func (a *app) startRun(ctx context.Context, r *http.Request, id, pkg string, deploy bool, rollbackTo, reason string) (run runs.Run, superseded string, busy *runs.Run, err error) {
	tx, err := a.mutation(ctx)
	if err != nil {
		return runs.Run{}, "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live, deleting bool // under the ledger lock the delete marks the project under: a start is before the mark or refused
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL), EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleting_since IS NOT NULL)", id).Scan(&live, &deleting); err != nil {
		return runs.Run{}, "", nil, err
	}
	if !live {
		return runs.Run{}, "", nil, &refused{http.StatusNotFound, "not_found", notOnCanyon(id)}
	}
	if deleting {
		return runs.Run{}, "", nil, &refused{http.StatusConflict, "deleting", "this project is being deleted"}
	}
	if pkg == "" {
		head, err := headPackage(ctx, tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return runs.Run{}, "", nil, &refused{http.StatusNotFound, "not_found", "no package yet"}
		}
		if err != nil {
			return runs.Run{}, "", nil, err
		}
		pkg = head.ID
	} else {
		// by id, then by label (contracts/routes.md): the project's own, and the project live — not a fact
		// the liveness check above is trusted to carry down here. The id is looked up first, so an id wins
		// over a label of the same text by construction, not by a sort.
		resolved, err := packageNamed(ctx, tx, id, pkg, deploy)
		if errors.Is(err, pgx.ErrNoRows) && deploy && (fromDashboard(ctx) || a.auth != authOIDC) { // a person deploys a pull request's version to preview (#1947)
			resolved, err = pullRequestDeployable(ctx, tx, id, pkg)
		}
		var notPassed *refused
		if errors.As(err, &notPassed) {
			return runs.Run{}, "", nil, notPassed
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return runs.Run{}, "", nil, &refused{http.StatusNotFound, "not_found", "no such package of this project"}
		}
		if err != nil {
			return runs.Run{}, "", nil, err
		}
		pkg = resolved
	}
	if rollbackTo != "" { // one of production's earlier versions of that package, read under this lock (contracts/routes.md, the gate-runs row)
		earlier, err := runs.ReadEarlier(ctx, tx, id)
		if err != nil {
			return runs.Run{}, "", nil, err
		}
		if !slices.ContainsFunc(earlier, func(e runs.Earlier) bool { return e.Package == pkg && e.Digest == rollbackTo }) {
			return runs.Run{}, "", nil, &refused{http.StatusConflict, "not_earlier", "That isn't one of production's last five versions: canyon status lists them."}
		}
	}
	snap, err := a.projectPolicy(ctx, tx, id) // the project's effective snapshot, under the ledger lock an apply also takes: the run derives against the states at its start row, and the journal keeps them for a replay
	if err != nil {
		return runs.Run{}, "", nil, err
	}
	requests := deploy && roleOf(ctx) == "admin" // the deploy's access lines name pending requests to an owner or an admin (design/PERMISSIONS.md, Access requests)
	if deploy && !requests {
		level, err := levelOf(ctx, tx, id)
		if err != nil {
			return runs.Run{}, "", nil, err
		}
		requests = level == "owner"
	}
	run, superseded, err = runs.StartTx(ctx, tx, a.durable, runs.StartInput{Project: id, Package: pkg, Deploy: deploy, Actor: actorOf(ctx), RollbackTo: rollbackTo, Reason: reason, Policy: snap, Requests: requests, Laptop: a.telemetry.mode == "laptop"}) // CANYON_INSTALL_MODE: the demo's host (#237)
	if errors.Is(err, runs.ErrUndeploying) {
		return runs.Run{}, "", nil, undeploying(id)
	}
	if errors.Is(err, runs.ErrRunInProgress) {
		_ = tx.Rollback(ctx)
		var active runs.Run
		err := a.pool.QueryRow(ctx, "SELECT id, state FROM runs WHERE project_id = $1 AND state IN ('pending', 'running')", id).Scan(&active.ID, &active.State)
		if errors.Is(err, pgx.ErrNoRows) { // it ended in the meantime
			return runs.Run{}, "", nil, nil
		}
		if err != nil {
			return runs.Run{}, "", nil, err
		}
		return runs.Run{}, "", &active, nil
	}
	if err != nil {
		return runs.Run{}, "", nil, err
	}
	if h := r.Header.Get("Canyon-Harness"); slices.Contains(harnesses, h) { // the coding agent the CLI runs under, for the usage report's agents (#2953); any other value is no harness
		if _, err := tx.Exec(ctx, "UPDATE runs SET harness = $1 WHERE id = $2", h, run.ID); err != nil {
			return runs.Run{}, "", nil, err
		}
	}
	if transport := transportOf(r); transport != "" {
		if _, err := tx.Exec(ctx, "UPDATE runs SET transport = $1 WHERE id = $2", transport, run.ID); err != nil {
			return runs.Run{}, "", nil, err
		}
	}
	detail := map[string]any{"package": pkg}
	if rollbackTo != "" { // a rollback review's: what it would put back, and why (contracts/routes.md, the gate-runs row)
		detail["rollback_to"] = rollbackTo
		if reason != "" {
			detail["reason"] = reason
		}
	}
	if err := events.AuditTx(ctx, tx, actorOf(ctx), action(r), id, run.ID, detail); err != nil {
		return runs.Run{}, "", nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return runs.Run{}, "", nil, err
	}
	return run, superseded, nil, nil
}

// gateRun is the run read (contracts/routes.md row 54): the runs row plus the verdict row's
// counts and findings, which are the verdict log's results in log order.
type gateRun struct {
	ID            string          `json:"id"`
	Package       string          `json:"package"`
	State         string          `json:"state"`
	FailureReason *string         `json:"failure_reason,omitempty"`
	FailedIn      *string         `json:"failed_in,omitempty"`         // the step a run Canyon itself failed in
	NotPlatform   *string         `json:"not_platform_only,omitempty"` // beside a patch run's failure reason not_platform_only alone (contracts/events.md)
	NotSame       *string         `json:"not_same_inputs,omitempty"`   // beside a rebuild's failure reason not_same_inputs alone (contracts/events.md)
	Project       string          `json:"project"`                     // the run's app
	Deploys       bool            `json:"deploys,omitempty"`           // a run POST …/deploys started: canyon_review follows it as a deploy
	Kind          string          `json:"kind,omitempty"`              // patch on a patch run, absent on every other
	Builds        *[]string       `json:"builds,omitempty"`            // a failed detect's alone: what the host builds, [] when its builder names none
	Errored       bool            `json:"errored,omitempty"`           // a failed detect's alone: a buildpack errored while detecting
	Fix           *fix            `json:"fix,omitempty"`               // a failed detect's or start's: the file a stock starter is missing
	NotChecked    bool            `json:"not_checked,omitempty"`       // the verdict log carries a "not checked" note: never read as clean
	Approvable    bool            `json:"approvable,omitempty"`        // the reader may approve the run's blocking findings, by who they are (#2204)
	LinkGitHub    bool            `json:"link_github,omitempty"`       // they would, but the org asks approvers of a push to link GitHub and they linked none (#2342)
	Blocking      int             `json:"blocking"`
	Warnings      int             `json:"warnings"`
	Progress      json.RawMessage `json:"progress"` // each (step, tool)'s newest progress row, in first-seen order (contracts/routes.md)
	Findings      json.RawMessage `json:"findings"`
}

// progressSQL is the run read's progress: for each (step, tool) its newest progress row's data, in the
// order each first appeared (contracts/events.md, progress).
const progressSQL = `SELECT COALESCE(jsonb_agg(data ORDER BY first), '[]') FROM (
  SELECT DISTINCT ON (data->>'step', COALESCE(data->>'tool', '')) data,
    min(id) OVER (PARTITION BY data->>'step', COALESCE(data->>'tool', '')) AS first
  FROM timeline_rows WHERE run_id = $1 AND kind = 'progress'
  ORDER BY data->>'step', COALESCE(data->>'tool', ''), id DESC) latest`

// notCheckedSQL is whether the run's verdict log carries a note saying "not checked" — the scanners'
// coverage notes, and the reviewer's over its cap (contracts/finding.md,
// What nothing read is named).
const notCheckedSQL = `SELECT EXISTS (SELECT 1 FROM timeline_rows t,
  jsonb_path_query(t.data, '$.findings.runs[*].invocations[*].toolExecutionNotifications[*].message.text') m(text)
  WHERE t.run_id = $1 AND t.kind = 'verdict' AND strpos(m.text #>> '{}', 'not checked') > 0)`

// verdictSQL is the run's verdict row: its counts, and its log's results in log order, each
// carrying the tool.driver.name of the run it is in as `tool`, and the project's unexpired approval of it on (rule, fingerprint) — the key the receipt nets
// on (runs.Approvals), a person's only, with whether it covers the finding were it blocking (runs.CoversBlocking,
// against this run's author), which covered reads and drops — and a result nothing approves carrying nothing more.
var verdictSQL = `SELECT COALESCE((t.data->>'blocking')::int, 0), COALESCE((t.data->>'warnings')::int, 0),
  (SELECT COALESCE(jsonb_agg(CASE WHEN a.rule IS NULL THEN f.finding ELSE f.finding || jsonb_build_object('approval', jsonb_build_object(
      'reason', a.reason, 'expires_at', to_char(a.expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
      'approved_by', jsonb_build_object('kind', a.approved_by_kind, 'subject', a.approved_by_subject, 'display', a.approved_by_display),
      'covers', ` + runs.CoversBlocking("g", "now()") + `)) END
      || jsonb_build_object('tool', r.run->'tool'->'driver'->>'name')
    ORDER BY r.rn, f.n), '[]')
   FROM jsonb_array_elements(t.data->'findings'->'runs') WITH ORDINALITY r(run, rn)
   CROSS JOIN LATERAL jsonb_array_elements(r.run->'results') WITH ORDINALITY f(finding, n)
   LEFT JOIN runs g ON g.id = $1
   LEFT JOIN approvals a ON a.project_id = $2 AND a.rule = f.finding->>'ruleId'
     AND a.fingerprint = f.finding->'partialFingerprints'->>'canyon/v1' AND a.expires_at > now()
     AND a.approved_by_kind <> 'canyon')
FROM timeline_rows t WHERE t.run_id = $1 AND t.kind = 'verdict'`

// dependency is a finding's dependency as the run read carries it (contracts/routes.md, the run read).
type dependency struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Vulnerability string `json:"vulnerability,omitempty"`
	Fixed         string `json:"fixed,omitempty"`
	Link          string `json:"link,omitempty"`
	FixedIn       string `json:"fixed_in,omitempty"`
}

// withDependencies puts on each finding whose tool names its dependency that dependency, as dependencyOf
// reads it, with fixed_in, the version that fixes every finding of its tool, dependency and blocks whose
// fixed names any (contracts/finding.md); any other finding is left as it is.
func withDependencies(findings json.RawMessage) (json.RawMessage, error) {
	var all []map[string]json.RawMessage
	if err := json.Unmarshal(findings, &all); err != nil {
		return nil, err
	}
	type key struct {
		tool, name, version string
		blocks              bool
	}
	deps, keys, lists := make([]*dependency, len(all)), make([]key, len(all)), map[key][][]string{}
	for i, f := range all {
		var tool string
		if json.Unmarshal(f["tool"], &tool) != nil || tool != "sarif/trivy" || f["properties"] == nil { // only trivy's findings name one, in their problem
			continue
		}
		var props struct {
			Canyon struct {
				Problem string `json:"problem"`
				Blocks  bool   `json:"blocks"`
			} `json:"canyon"`
		}
		if err := json.Unmarshal(f["properties"], &props); err != nil {
			return nil, err
		}
		a, ok := dependencyOf(tool, props.Canyon.Problem)
		if !ok {
			continue
		}
		deps[i] = &dependency{Name: a.Name, Version: a.Installed, Vulnerability: a.Vulnerability, Fixed: a.Fixed, Link: a.Link}
		keys[i] = key{tool, a.Name, a.Installed, props.Canyon.Blocks}
		if a.Fixed != "" {
			lists[keys[i]] = append(lists[keys[i]], strings.Split(a.Fixed, ", "))
		}
	}
	fixedIn := map[key]string{} // once a group: its lists are the same for every finding of it
	for k, list := range lists {
		fixedIn[k], _ = trivy.FixedIn(k.version, list)
	}
	for i, d := range deps {
		if d == nil {
			continue
		}
		d.FixedIn = fixedIn[keys[i]]
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		all[i]["dependency"] = raw
	}
	return json.Marshal(all)
}

// resultCounts is how many of the findings block and how many do not, results and not entries: the run
// read's counts, which a CLI released before entries checks against the findings it lists (contracts/routes.md,
// the run read; #2013). Every other read counts the verdict's entries.
func resultCounts(findings json.RawMessage) (blocking, warnings int, err error) {
	var all []struct {
		Properties struct {
			Canyon struct {
				Blocks *bool `json:"blocks"`
			} `json:"canyon"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(findings, &all); err != nil {
		return 0, 0, err
	}
	for _, f := range all {
		if runs.Blocks(f.Properties.Canyon.Blocks) { // a finding that does not say reads as blocking
			blocking++
		} else {
			warnings++
		}
	}
	return blocking, warnings, nil
}

// covered keeps on each finding the approval verdictSQL attached only where it covers it, as runs.Approvals
// reads it (contracts/routes.md, the approve rows; #2204): a finding that blocks — before preview, as runs.Blocks
// reads it, or before production under rules — keeps one only where verdictSQL computed covers, and an
// advisory one any. covers is dropped from what is answered.
func covered(findings json.RawMessage, rules []string) (json.RawMessage, error) {
	var all []map[string]json.RawMessage
	var read []struct {
		finding.Result
		Approval *struct {
			Covers bool `json:"covers"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(findings, &all); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(findings, &read); err != nil {
		return nil, err
	}
	for i, f := range read {
		if f.Approval == nil {
			continue
		}
		blocks := runs.Blocks(f.Properties.Canyon.Blocks) || blocksProduction(rules, f.RuleID, f.Properties.Canyon.Severity)
		if blocks && !f.Approval.Covers {
			delete(all[i], "approval")
			continue
		}
		var approval map[string]json.RawMessage
		if err := json.Unmarshal(all[i]["approval"], &approval); err != nil {
			return nil, err
		}
		delete(approval, "covers")
		raw, err := json.Marshal(approval)
		if err != nil {
			return nil, err
		}
		all[i]["approval"] = raw
	}
	return json.Marshal(all)
}

// withProduction marks each finding that blocks before production under rules with blocks_production, the
// receipt's own answer for it (contracts/routes.md, the run read; #2206), so a screen never works it out.
func withProduction(findings json.RawMessage, rules []string) (json.RawMessage, error) {
	var all []map[string]json.RawMessage
	var read []finding.Result
	if err := json.Unmarshal(findings, &all); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(findings, &read); err != nil {
		return nil, err
	}
	for i, f := range read {
		if blocksProduction(rules, f.RuleID, f.Properties.Canyon.Severity) {
			all[i]["blocks_production"] = json.RawMessage("true")
		}
	}
	return json.Marshal(all)
}

// withEntries puts on each finding its entry, the index of its entry's first finding (contracts/finding.md,
// One problem, one entry), as finding.Entries numbers the log the findings flatten.
func withEntries(findings json.RawMessage) (json.RawMessage, error) {
	var all []map[string]json.RawMessage
	var read []struct {
		finding.Result
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(findings, &all); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(findings, &read); err != nil {
		return nil, err
	}
	var log finding.Log // a run a finding, each its tool's: what Entries reads of the log the findings flatten
	for _, one := range read {
		log.Runs = append(log.Runs, finding.Run{Tool: finding.Tool{Driver: finding.Driver{Name: one.Tool}}, Results: []finding.Result{one.Result}})
	}
	for i, n := range finding.Entries(log) {
		all[i]["entry"] = json.RawMessage(strconv.Itoa(n))
	}
	return json.Marshal(all)
}

func (a *app) getGateRun(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	out := gateRun{ID: id, Findings: json.RawMessage("[]")}
	var project, commit string
	var buildpacks []string
	// a deploy that reused its package's image recorded no buildpacks of its own: the build's are the package's newest
	err := a.pool.QueryRow(ctx, `SELECT r.project_id, r.deploys, r.package_id, r.state, r.failure_reason, r.failed_in, r.not_platform_only, r.not_same_inputs, CASE WHEN r.kind = 'patch' THEN 'patch' ELSE '' END, CASE WHEN r.failure_reason = 'detect' THEN COALESCE(r.builds, '{}') END,
  r.failure_reason IS NOT DISTINCT FROM 'detect' AND r.detect_errored, p.commit,
  ARRAY(SELECT b->>'id' FROM jsonb_array_elements(COALESCE(r.buildpacks, (SELECT o.buildpacks FROM runs o WHERE o.project_id = r.project_id
    AND o.package_id = r.package_id AND o.buildpacks IS NOT NULL ORDER BY o.created_at DESC LIMIT 1), '[]')) b)
  FROM runs r JOIN packages p ON p.id = r.package_id AND p.project_id = r.project_id WHERE r.id = $1`, id).Scan(&project, &out.Deploys, &out.Package, &out.State, &out.FailureReason, &out.FailedIn, &out.NotPlatform, &out.NotSame, &out.Kind, &out.Builds, &out.Errored, &commit, &buildpacks)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	// the runs row first, the verdict row second: the driver commits the verdict before the terminal
	// state, so a terminal state read here implies the verdict row exists; read the other way round a
	// poller could stop on blocked with an empty findings list
	err = a.pool.QueryRow(ctx, verdictSQL, id, project).Scan(&out.Blocking, &out.Warnings, &out.Findings) // one row; the counts are the findings' below: the unique index on (run_id, dedupe)
	var production []string
	if err == nil {
		production, err = a.productionOf(ctx, project)
	}
	if err == nil {
		out.Findings, err = covered(out.Findings, production)
	}
	if err == nil {
		out.Findings, err = withDependencies(out.Findings)
	}
	if err == nil {
		out.Findings, err = withEntries(out.Findings)
	}
	if err == nil {
		out.Findings, err = withProduction(out.Findings, production)
	}
	if err == nil {
		out.Blocking, out.Warnings, err = resultCounts(out.Findings)
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		var author runAuthor
		if author, err = authorOf(ctx, a.pool, id); err == nil {
			code, _ := a.approverRefusal(ctx, author)
			out.Approvable, out.LinkGitHub = code == "", code == "github_link_required"
			err = a.pool.QueryRow(ctx, notCheckedSQL, id).Scan(&out.NotChecked)
		}
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		err = a.pool.QueryRow(ctx, progressSQL, id).Scan(&out.Progress)
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		err = a.setFix(ctx, &out, project, commit, buildpacks)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out.Project = project
	writeJSON(w, http.StatusOK, map[string]any{"run": out})
}

// getGateRunSARIF is the run's verdict log as stored, for any CI to upload: each finding an
// unexpired approval covers — found as the run read finds it, verdictSQL — carries a SARIF
// suppression whose justification is the approval's reason, and nothing of who approved it.
func (a *app) getGateRunSARIF(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var project string
	var stored []byte
	err := a.pool.QueryRow(ctx, `SELECT r.project_id, t.data->'findings' FROM runs r
		LEFT JOIN timeline_rows t ON t.run_id = r.id AND t.kind = 'verdict' WHERE r.id = $1`, id).Scan(&project, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no such run", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if stored == nil {
		writeErr(w, http.StatusConflict, "no_verdict", "this run has no verdict", nil)
		return
	}
	var counts [2]int
	var flat []byte
	err = a.pool.QueryRow(ctx, verdictSQL, id, project).Scan(&counts[0], &counts[1], &flat)
	var production []string
	if err == nil {
		production, err = a.productionOf(ctx, project)
	}
	if err == nil {
		flat, err = covered(flat, production)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	body, err := suppressed(stored, flat)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Content-Type", "application/sarif+json")
	_, _ = w.Write(body)
}

// suppressed sets, on each result of log whose flattened twin in flat (the log's results in log
// order, as verdictSQL answers them) carries an approval, one external suppression justified by
// the approval's reason.
func suppressed(log, flat []byte) ([]byte, error) {
	var findings []struct {
		Approval *struct {
			Reason string `json:"reason"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(flat, &findings); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(log))
	dec.UseNumber() // a number in the log goes back out as it came in
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	disagree := errors.New("the verdict's results and its flattened findings disagree")
	runs, _ := doc["runs"].([]any)
	n := 0
	for _, run := range runs {
		fields, _ := run.(map[string]any)
		results, _ := fields["results"].([]any)
		for _, res := range results {
			result, ok := res.(map[string]any)
			if !ok || n >= len(findings) {
				return nil, disagree
			}
			if ap := findings[n].Approval; ap != nil {
				result["suppressions"] = []any{map[string]any{"kind": "external", "justification": ap.Reason}}
			}
			n++
		}
	}
	if n != len(findings) {
		return nil, disagree
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // a fix that says `a > b` reads as written
	err := enc.Encode(doc)
	return out.Bytes(), err
}

// packageNamed resolves what a request called a package — an id, else a label — to the id of a
// package of the live project; for a deploy never a pull request's or a check's, which are reviewed
// and never deployed (contracts/routes.md). pgx.ErrNoRows when neither names one.
// pullRequestDeployable is a pull request's package named by id or label, while the newest run of it
// let it through: clean, or blocked with every blocking finding approved (contracts/routes.md, the
// deploys row; #1947). ErrNoRows when there is none; review_not_passed otherwise.
func pullRequestDeployable(ctx context.Context, tx pgx.Tx, project, name string) (string, error) {
	var resolved, state, run string
	var findings json.RawMessage
	err := tx.QueryRow(ctx, `SELECT p.id, COALESCE(r.state, ''), COALESCE(r.id, ''), (SELECT t.data->'findings' FROM timeline_rows t WHERE t.run_id = r.id AND t.dedupe = 'verdict')
		FROM packages p LEFT JOIN LATERAL (SELECT id, state FROM runs WHERE project_id = p.project_id AND package_id = p.id AND kind = 'gate' ORDER BY created_at DESC, id DESC LIMIT 1) r ON true
		WHERE p.project_id = $1 AND p.source_kind = 'repo-pr' AND (p.id = $2 OR p.label = $2) ORDER BY p.id = $2 DESC LIMIT 1`, project, name).Scan(&resolved, &state, &run, &findings)
	if err != nil {
		return "", err
	}
	passed := state == "clean"
	if state == "blocked" && findings != nil { // a blocked run with no verdict row has nothing approved
		_, unapproved, err := runs.Approvals(ctx, tx, project, run, findings)
		if err != nil {
			return "", err
		}
		passed = unapproved == 0
	}
	if !passed {
		return "", &refused{http.StatusConflict, "review_not_passed", "Canyon's review of this pull request hasn't passed"}
	}
	return resolved, nil
}

func packageNamed(ctx context.Context, q rower, project, name string, deploy bool) (string, error) {
	const live = "SELECT p.id FROM packages p JOIN projects pr ON pr.id = p.project_id AND pr.deleted_at IS NULL WHERE p.project_id = $1 AND (NOT $3 OR p.source_kind NOT IN ('repo-pr', 'check')) AND "
	var resolved string
	err := q.QueryRow(ctx, live+"p.id = $2", project, name, deploy).Scan(&resolved)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, live+"p.label = $2", project, name, deploy).Scan(&resolved)
	}
	return resolved, err
}
