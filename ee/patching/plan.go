package patching

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/runs"
)

// planned is one preview the tick plans a rebase of.
type planned struct {
	image string
	fixes []runs.PlatformFinding
}

// Why a tick's plan started no patch run of an app it read (#2575), beside the core's: why it read none
// (runs.WhyNone) and why a start started none (runs.Pipeline.StartPatch). Each is one cause, distinct from every
// other, and names no image's contents. An app the plan read and gave no cause is whyUnknown, never read as
// nothing to do (fail closed).
const (
	whyNoPreview  = "no app has a preview"
	whyRefused    = "a rebase of its image onto this run image was refused for good"
	whyNoPrint    = "Canyon didn't record the settings its preview was deployed with: deploy it once to patch it"
	whyNoDigest   = "its preview's image is not named by digest"
	whyPolicy     = "its policy could not be composed"
	whyUnread     = "its image's read could not check"
	whyNoFinding  = "no counted finding at high or above"
	whyNoFix      = "the run image fixes none of its counted findings"
	whyBump       = "its counted findings are in runtimes or build tools, which a rebase does not fix"
	whyRunUnread  = "the run image's own scan could not be read"
	whyStopped    = "its preview is not running"
	whyUnderWay   = "a patch run of it is under way"
	whyWatchedOut = "its patch onto this run image passed"
	whyEnded      = "its watch ended early: planned again once its preview runs another image"
	whyReverted   = "its patch onto this run image reverted, and is not retried"
	whyNoStart    = "its patch onto this run image failed to start, and is not retried"
	whyFailed     = "its patch run onto this run image failed: planned again once its preview runs another image"
	whyPaused     = "patching onto this run image is paused until an admin resumes it"
	whyWave       = "it waits for its wave"
	whyStall      = "it waits for its wave, which a stall holds until an admin resumes it"
	whyUnknown    = "for a reason the plan does not name"
)

// planSaid is what one tick's plan says (#2575): why it read no app, or the run image it planned onto and,
// for each app it read, that its patch run started or why not.
type planSaid struct {
	why  string
	run  string
	apps map[string]string
}

// String is the line the tick logs: the apps whose patch runs started, then each other cause with its apps,
// at most five named each.
func (s planSaid) String() string {
	if s.why != "" {
		return "runs: the patch plan started no patch run: " + s.why
	}
	if len(s.apps) == 0 {
		return "runs: the patch plan onto " + s.run + " started no patch run: " + whyNoPreview
	}
	by := map[string][]string{}
	for app, why := range s.apps {
		why = cmp.Or(why, whyUnknown) // an app with no cause is never nothing to do
		by[why] = append(by[why], app)
	}
	named := func(apps []string) string {
		slices.Sort(apps)
		names := strings.Join(apps[:min(len(apps), 5)], ", ")
		if len(apps) > 5 {
			names += fmt.Sprintf(" and %d more", len(apps)-5)
		}
		return names
	}
	line := fmt.Sprintf("runs: the patch plan onto %s started %d patch run(s)", s.run, len(by[runs.WhyStarted]))
	if started := by[runs.WhyStarted]; len(started) > 0 {
		line += " (" + named(started) + ")"
	}
	delete(by, runs.WhyStarted)
	sep := ": "
	for _, why := range slices.Sorted(maps.Keys(by)) {
		line += fmt.Sprintf("%s%d not, %s (%s)", sep, len(by[why]), why, named(by[why]))
		sep = "; "
	}
	return line
}

// causes are planSaid's causes in the identities read's words (contracts/routes.md, the identities row's plan): why
// the plan read no app, then each app's. A cause not here is unknown, never nothing to do (fail closed).
var causes = map[string]string{
	runs.WhyServerOff: "server_off", runs.WhyOrgOff: "org_off", runs.WhyNotWired: "not_wired", whyNoPreview: "no_preview",
	runs.WhyStarted: "started", whyRefused: "refused", whyNoPrint: "no_fingerprint", whyNoDigest: "no_digest", whyPolicy: "policy", runs.WhyRuleOff: "rule_off",
	whyUnread: "unread", whyNoFinding: "no_finding", whyNoFix: "no_fix", whyBump: "runtime", whyRunUnread: "run_image_unread",
	whyStopped: "stopped", whyUnderWay: "under_way", whyWatchedOut: "passed", whyEnded: "ended_early", whyReverted: "reverted",
	whyNoStart: "failed_start", whyFailed: "failed", whyPaused: "paused", whyWave: "wave", whyStall: "stalled",
	runs.WhyGoesFirst: "goes_first", runs.WhyOffline: "offline", runs.WhyMoved: "moved", whyUnknown: "unknown",
}

// keepSaid replaces the one patch_plan_said row with what a tick's plan said, as the identities read serves it: why it
// read no app — failed when the plan failed, so no app's cause outlives a plan that could not say it — or the identity
// it planned onto and each app it read, by its cause.
func keepSaid(ctx context.Context, p *runs.Pipeline, s planSaid, failed bool) error {
	cause := func(why string) string { return cmp.Or(causes[cmp.Or(why, whyUnknown)], causes[whyUnknown]) }
	var why, identity *string
	apps := map[string]string{}
	switch {
	case failed:
		why = new("failed")
	case s.why != "":
		why = new(cause(s.why))
	default:
		if id := runs.RunImageDigest(s.run); id != "" {
			identity = &id
		}
		if len(s.apps) == 0 {
			why = new(cause(whyNoPreview))
		}
		for app, w := range s.apps {
			apps[app] = cause(w)
		}
	}
	_, err := p.Pool.Exec(ctx, `INSERT INTO patch_plan_said (said_at, identity, why, apps) VALUES (now(), $1, $2, $3)
		ON CONFLICT (one) DO UPDATE SET said_at = EXCLUDED.said_at, identity = EXCLUDED.identity, why = EXCLUDED.why, apps = EXCLUDED.apps`, identity, why, apps)
	return err
}

// Plan is a tick's plan (design/PATCHING.md §4.2), run while sign-in is on — with it off nobody can decide a patch
// approval, so patching reads off — for each preview of a project whose platform.known-vuln is on and whose preview
// runs: its image's checked read, judged against the match of the run image GET /builder names now. Each preview to
// rebase is an app of that run image's identity, an app planned before that no longer is — a stopped preview among
// them, so a stopped canary is replaced — leaves it, and its waves move; then a patch run starts for each app of a
// wave under way whose project has no other run. Nothing is planned while GET /builder names no run image, nor
// rebased onto one whose match does not read (fail closed), nor onto a pair a rebase refused for good. The rebuild
// plan follows (planRebuilds). It answers how many patch runs it started, and logs one line saying which, and why
// each other app it read started none (planSaid); a plan that fails says why in its error, which the tick logs. What
// it said, or that it failed, is kept for the identities read (keepSaid).
func (Patching) Plan(ctx context.Context, p *runs.Pipeline) (int, error) {
	said := planSaid{apps: map[string]string{}}
	rebuilds := map[string]planned{}
	err := planTick(ctx, p, &said, rebuilds)
	rebuilt := 0
	if err == nil {
		log.Print(said)
		var rerr error
		if rebuilt, rerr = planRebuilds(ctx, p, said.run, rebuilds); rerr != nil { // the rebase plan's causes are kept whatever the rebuild plan does
			log.Printf("runs: the rebuild plan: %v", rerr)
		}
	}
	if ctx.Err() == nil {
		err = cmp.Or(err, keepSaid(ctx, p, said, err != nil))
	}
	started := rebuilt
	for _, why := range said.apps {
		if why == runs.WhyStarted {
			started++
		}
	}
	return started, err
}

// planTick is Plan's rebase plan, each cause it finds written to said.
func planTick(ctx context.Context, p *runs.Pipeline, said *planSaid, rebuilds map[string]planned) error {
	if why, err := p.WhyNone(ctx); err != nil {
		return err
	} else if why != "" {
		said.why = why
		return nil
	}
	run, err := p.Executor.RunImage(ctx)
	identity := runs.RunImageDigest(run)
	if err != nil || identity == "" {
		return cmp.Or(err, fmt.Errorf("GET /builder names no run image by digest (%q): no rebase is planned", run))
	}
	said.run = run
	named, err := runs.RunImageOf(ctx, p.Pool, run, time.Now())
	if err != nil {
		return err
	}
	plans, err := planPreviews(ctx, p, run, named, said.apps, rebuilds)
	if err != nil {
		return err
	}
	if err := runningOnly(ctx, p, plans, said.apps); err != nil {
		return err
	}
	if err := recordedOnly(ctx, p, plans, said.apps); err != nil {
		return err
	}
	var due []string
	if err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		var err error
		if due, err = writePlan(ctx, tx, "rebase", identity, run, "", plans); err != nil {
			return err
		}
		return whyNotDue(ctx, tx, identity, plans, said.apps)
	}); err != nil {
		return err
	}
	for _, project := range due {
		pl := plans[project]
		why, err := p.StartPatch(ctx, project, runs.Patch{Identity: identity, RunImage: run, Image: pl.image, Fixes: pl.fixes})
		if err != nil {
			return err
		}
		said.apps[project] = why
	}
	return nil
}

// whyNotDue writes, in tx after writePlan, why each app plans names is not due: what its identity's waves hold
// of it (whyWaiting). A due app's cause is its start's, written after.
func whyNotDue(ctx context.Context, tx pgx.Tx, identity string, plans map[string]planned, said map[string]string) error {
	names := make([]string, 0, len(plans))
	for project := range plans {
		names = append(names, project)
		said[project] = whyUnknown
	}
	rows, err := tx.Query(ctx, `SELECT a.project_id, a.outcome, coalesce(a.wave, -1), i.wave, i.paused_at IS NOT NULL, i.stalled_at IS NOT NULL
		FROM patch_wave_apps a JOIN patch_identities i ON i.id = a.identity WHERE a.identity = $1 AND a.project_id = ANY($2)`, identity, names)
	if err != nil {
		return err
	}
	var project, outcome string
	var wave, at int
	var paused, stalled bool
	_, err = pgx.ForEachRow(rows, []any{&project, &outcome, &wave, &at, &paused, &stalled}, func() error {
		said[project] = whyWaiting(outcome, wave, at, paused, stalled)
		return nil
	})
	return err
}

// whyWaiting is why an app of an identity's waves is not due now: its outcome there, its wave (-1, none yet),
// the identity's wave, and whether the identity is paused or stalled. A planned app due now is whyUnknown
// until its start says otherwise, as is any outcome this does not name.
func whyWaiting(outcome string, wave, at int, paused, stalled bool) string {
	switch outcome {
	case runs.PatchPlanned:
		switch {
		case paused:
			return whyPaused
		case wave >= 0 && wave <= at:
			return whyUnknown
		case stalled:
			return whyStall
		}
		return whyWave
	case runs.PatchRunning:
		return whyUnderWay
	case runs.PatchPassed:
		return whyWatchedOut
	case runs.PatchEndedEarly:
		return whyEnded
	case runs.PatchReverted:
		return whyReverted
	case runs.PatchFailedStart:
		return whyNoStart
	case runs.PatchFailed:
		return whyFailed
	case runs.PatchRefused:
		return whyRefused
	}
	return whyUnknown
}

// runningOnly leaves in plans the previews that run: a preview that is stopped, or may not run, is never
// patched, nor holds a wave; said names each it leaves out.
func runningOnly(ctx context.Context, p *runs.Pipeline, plans map[string]planned, said map[string]string) error {
	if len(plans) == 0 {
		return nil
	}
	asked := make([]executor.Container, 0, len(plans))
	for project := range plans {
		asked = append(asked, executor.Container{App: project, Target: "preview"})
	}
	up, err := p.Executor.RunningOf(ctx, asked, false)
	if err != nil {
		return err
	}
	for project := range plans {
		if !slices.Contains(up.Running, executor.Container{App: project, Target: "preview"}) {
			delete(plans, project)
			said[project] = whyStopped
		}
	}
	return nil
}

// recordedOnly leaves in plans the previews whose deploy row recorded what a patch's config check compares against
// (runs.ConfigRecorded): one that recorded none fails that check every time, so it is never planned, not as the canary
// nor in a wave (design/PATCHING.md §4.2; #265), and said says to deploy it once. A preview no longer on the image the
// plan read has moved; one another platform runs is runningOnly's, left as it is.
func recordedOnly(ctx context.Context, p *runs.Pipeline, plans map[string]planned, said map[string]string) error {
	for project, pl := range plans {
		var spec *runs.Egress
		var parts []byte
		var fingerprint *string
		var runBy string
		err := p.Pool.QueryRow(ctx, "SELECT run_by, egress, run_config, run_config_sha256 FROM deploys WHERE project_id = $1 AND env = 'preview' AND image = $2",
			project, pl.image).Scan(&runBy, &spec, &parts, &fingerprint)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			delete(plans, project)
			said[project] = runs.WhyMoved
		case err != nil:
			return err
		case runBy == "canyon" && !runs.ConfigRecorded(spec, parts, fingerprint):
			delete(plans, project)
			said[project] = whyNoPrint
		}
	}
	return nil
}

// planPreviews is the rebase plan of each preview, of a project whose rule is on, onto run, and the findings that
// would fix; named is the run image as the plan reads it, its match maybe not read. said names why each other
// preview is not, a pair a rebase refused for good among them. In rebuilds it puts each app it plans no rebase for
// with a counted runtime or lifecycle finding at or above high, those findings its fixes (contracts/routes.md, A
// rebuild patch).
func planPreviews(ctx context.Context, p *runs.Pipeline, run string, named runs.NamedRunImage, said map[string]string, rebuilds map[string]planned) (map[string]planned, error) {
	policyOf, readOf := p.ProjectPolicy.Load(), p.PlatformRead.Load()
	if policyOf == nil || readOf == nil { // WhyNone said so before the tick read anything; set since, the plan reads nothing
		return nil, nil
	}
	rows, err := p.Pool.Query(ctx, `SELECT d.project_id, d.image,
			EXISTS (SELECT 1 FROM patch_refusals r WHERE r.project_id = d.project_id AND r.digest = split_part(d.image, '@', 2) AND r.run_image = $1)
		FROM deploys d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL WHERE d.env = 'preview' ORDER BY 1`, run)
	if err != nil {
		return nil, err
	}
	type preview struct {
		project, image string
		refused        bool
	}
	var previews []preview
	var pv preview
	if _, err := pgx.ForEachRow(rows, []any{&pv.project, &pv.image, &pv.refused}, func() error {
		previews = append(previews, pv)
		return nil
	}); err != nil {
		return nil, err
	}
	plans := map[string]planned{}
	for _, pv := range previews {
		said[pv.project] = whyUnknown
		digest := runs.ImageDigest(pv.image)
		switch {
		case pv.refused:
			said[pv.project] = whyRefused
			continue
		case digest == "":
			said[pv.project] = whyNoDigest
			continue
		}
		snap, err := policyOf(ctx, p.Pool, pv.project)
		if err != nil {
			log.Printf("runs: the patch plan leaves %s: its policy: %v", pv.project, err)
			said[pv.project] = whyPolicy
			continue
		}
		if !runs.ReadsImages(snap) {
			said[pv.project] = runs.WhyRuleOff
			continue
		}
		read, err := readOf(ctx, pv.project, digest)
		if err != nil {
			return nil, err
		}
		if !read.Checked { // could not rescan: nothing is planned on a read that did not check
			said[pv.project] = whyUnread
			continue
		}
		outcome, fixes := named.Plan(read.Findings)
		if outcome != runs.OutcomeRebase {
			if runtime := runs.RebuildFixes(read.Findings); len(runtime) > 0 {
				rebuilds[pv.project] = planned{image: pv.image, fixes: runtime}
			}
		}
		switch outcome {
		case runs.OutcomeRebase:
			plans[pv.project] = planned{image: pv.image, fixes: fixes}
		case runs.OutcomeNothing:
			said[pv.project] = whyNoFinding
		case runs.OutcomeWait:
			said[pv.project] = whyNoFix
		case runs.OutcomeBump:
			said[pv.project] = whyBump
		case runs.OutcomeUnchecked:
			said[pv.project] = whyRunUnread
		}
	}
	return plans, nil
}

// sweepEnded is what writePlan makes of an app whose patch run ended recording no outcome: planned again when
// it ended interrupted before its /run — a person's run went first — or clean before it, as a gate-only patch
// run of the release before ended; ended_early when a request ended it at its /run, which proves nothing; failed
// when it ended any other way before its /run. One whose run deployed waits on its watch.
const sweepEnded = `UPDATE patch_wave_apps a SET outcome = CASE WHEN r.deploy_started_at IS NOT NULL THEN 'ended_early'
		WHEN r.state = 'clean' OR r.failure_reason = 'interrupted' THEN 'planned' ELSE 'failed' END,
	run = CASE WHEN r.deploy_started_at IS NULL AND (r.state = 'clean' OR r.failure_reason = 'interrupted') THEN NULL ELSE a.run END, outcome_at = now()
	FROM runs r WHERE a.outcome = 'running' AND r.id = a.run AND r.ended_at IS NOT NULL AND (r.deploy_started_at IS NULL OR r.failure_reason = 'interrupted')`

// writePlan writes a plan of kind rebase or rebuild onto identity in tx: the identity, each app it planned, the waves
// moved, and answers the apps due now. A plan replaces the planned apps of its own kind's identities alone: an app
// planned before that the plan no longer names leaves it; one whose run ended before its /run recording no outcome is
// swept (sweepEnded); one whose preview now runs another image that needs the rebase is planned again with it, in the
// wave it was in — one whose watch ended early in no wave yet, and never as the canary — unless its patch reverted or
// failed to start: those stay as they ended, for the breaker (design/PATCHING.md §4.2). The apps due are planned, in a
// wave under way or done, of an identity not paused, and not the app a stall of it names, which the stall leaves out;
// each is one plans names.
func writePlan(ctx context.Context, tx pgx.Tx, kind, identity, run, builder string, plans map[string]planned) ([]string, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO patch_identities (id, run_image, kind, builder) VALUES ($1, $2, $3, nullif($4, ''))
		ON CONFLICT (id) DO UPDATE SET run_image = EXCLUDED.run_image WHERE patch_identities.kind = EXCLUDED.kind`, identity, run, kind, builder); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(plans))
	for project := range plans {
		names = append(names, project)
	}
	if _, err := tx.Exec(ctx, sweepEnded); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM patch_wave_apps a USING patch_identities i WHERE i.id = a.identity AND i.kind = $3
		AND a.outcome = 'planned' AND (a.identity <> $1 OR NOT a.project_id = ANY($2))`, identity, names, kind); err != nil {
		return nil, err
	}
	for project, pl := range plans {
		if _, err := tx.Exec(ctx, `INSERT INTO patch_wave_apps (identity, project_id, image) VALUES ($1, $2, $3)
			ON CONFLICT (identity, project_id) DO UPDATE SET image = EXCLUDED.image, outcome = 'planned', run = NULL, outcome_at = now(),
				wave = CASE WHEN patch_wave_apps.outcome = 'ended_early' THEN NULL ELSE patch_wave_apps.wave END,
				no_canary = patch_wave_apps.no_canary OR patch_wave_apps.outcome = 'ended_early'
			WHERE patch_wave_apps.image <> EXCLUDED.image AND patch_wave_apps.outcome IN ('planned', 'passed', 'refused', 'failed', 'ended_early')`, identity, project, pl.image); err != nil {
			return nil, err
		}
	}
	if err := moveWaves(ctx, tx, identity); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.project_id FROM patch_wave_apps a JOIN patch_identities i ON i.id = a.identity
		WHERE a.identity = $1 AND i.paused_at IS NULL AND NOT a.project_id = ANY(coalesce(i.stalled_projects, '{}')) AND a.outcome = 'planned' AND a.wave <= i.wave ORDER BY a.project_id`, identity)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
