package runs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

const (
	RescanQueue    = "patch-rescan" // one rescan at a time across the replicas (design/PATCHING.md §3.2)
	RescanWorkflow = "patch.rescan" // the schedule's name too: DBOS keys each tick's workflow by it and the tick's time
	DefaultRescan  = 6 * time.Hour  // CANYON_PATCH_INTERVAL unset
	staleDatabase  = 5 * 24 * time.Hour
)

// RescanSchedule is CANYON_PATCH_INTERVAL as the cron DBOS fires the rescan on, in UTC: every replica's
// ticks fall on the same instants, so each tick is one workflow, whichever replica enqueues it. An
// interval that does not divide a minute, an hour or a day evenly would fall on no such instants, and is
// refused. DBOS keeps the schedule in its system database, so the replica that registered last sets it.
func RescanSchedule(every time.Duration) (string, error) {
	switch {
	case every <= 0 || every%time.Second != 0:
	case every < time.Minute && time.Minute%every == 0:
		return fmt.Sprintf("*/%d * * * * *", every/time.Second), nil
	case every < time.Hour && every%time.Minute == 0 && time.Hour%every == 0:
		return fmt.Sprintf("0 */%d * * * *", every/time.Minute), nil
	case every <= 24*time.Hour && every%time.Hour == 0 && (24*time.Hour)%every == 0:
		return fmt.Sprintf("0 0 */%d * * *", every/time.Hour), nil
	}
	return "", fmt.Errorf("%s divides no minute, hour or day evenly: every replica's ticks must fall on the same instants", every)
}

func (p *Pipeline) registerRescan(dc dbos.Context) error {
	schedule, err := RescanSchedule(cmp.Or(p.RescanEvery, DefaultRescan))
	if err != nil {
		return err
	}
	dbos.RegisterWorkflow(dc, p.imageSBOM, dbos.WithWorkflowName(ImageSBOMWorkflow), dbos.WithMaxRecoveryAttempts(3))
	dbos.RegisterWorkflow(dc, p.rescan, dbos.WithWorkflowName(RescanWorkflow), dbos.WithMaxRecoveryAttempts(3))
	for q, n := range map[string]int{PatchesQueue: positive(p.PatchConcurrency, 1), ImageQueue: 1, RescanQueue: 1} {
		if _, err := dbos.RegisterQueue(dc, q, dbos.WithGlobalConcurrency(n)); err != nil {
			return err
		}
	}
	return dbos.ApplySchedules(dc, []dbos.ScheduleSpec{{ScheduleName: RescanWorkflow, WorkflowName: RescanWorkflow, Schedule: schedule, CronTimezone: "UTC", QueueName: RescanQueue}})
}

// rescanImage is one project's scan of a running image, or the run image's own (RunImage, no project), with
// the layers a re-match covers, each with the slice the scan gave it: Canyon's, and apart from them the app
// layers deps.known-vuln alone reads (design/PATCHING.md §3.2, §3.6), absent from a plan journaled before.
type rescanImage struct {
	Project   string        `json:"project"`
	Digest    string        `json:"digest"`
	RunImage  bool          `json:"run_image,omitempty"`
	Layers    []rescanLayer `json:"layers"`
	AppLayers []rescanLayer `json:"app_layers,omitempty"`
}

// scanRow is the row of img's scan, for a match to mark: a project's scan of an app's image, or the run
// image's own.
func (img rescanImage) scanRow() (table, where string, args []any) {
	if img.RunImage {
		return "run_image_scans", "digest = $1", []any{img.Digest}
	}
	return "scanned_images", "project_id = $1 AND digest = $2", []any{img.Project, img.Digest}
}

type rescanLayer struct {
	DiffID string `json:"diff_id"`
	Part   string `json:"part"`
}

// rescanPlan is what one tick does: the images to read, which have no finished scan, and the scans to
// match, the longest unmatched first; and whether any image runs in a project whose rule is on, without
// which the run image is not read either.
type rescanPlan struct {
	Read   []imageScan   `json:"read"`
	Match  []rescanImage `json:"match"`
	RuleOn bool          `json:"rule_on,omitempty"`
}

// rescan is the patch.rescan workflow, one per tick (design/PATCHING.md §3.2). A tick an interval late or
// more does nothing: the next one is due. Otherwise it drops the scans no image that runs or is kept for
// a rollback names, and the slices no scan names; asks image.sbom of each such image, of a project whose
// rule is on, that has no finished scan; and matches each such scan's covered layers offline on the
// executor's newest database, each distinct slice once. A scan's new matches are stored together or not at
// all, and one that did not match them all reads could not check until a later one does. Last, it reads
// and matches the run image GET /builder names (rescanRunImage) while an image runs in a project whose rule
// is on, and plans patches against that match (planPatches). It answers the slices it matched.
func (p *Pipeline) rescan(ctx dbos.Context, tick dbos.ScheduledWorkflowInput) (int, error) {
	now, err := dbos.RunAsStep(ctx, func(context.Context) (time.Time, error) { return time.Now(), nil }, dbos.WithStepName("now"))
	if err != nil || now.Sub(tick.ScheduledTime) > cmp.Or(p.RescanEvery, DefaultRescan) {
		return 0, err
	}
	// keep is what a tick that reaches no plan says, in a step of its own, so Settings never reads the causes of a tick
	// before under this one's time (#2588, the review of #2607): failed when it failed before its plan, else why it read
	// no app as the plan says it — patching off, or nothing to plan with — and otherwise that no app has the rule on.
	keep := func(failed bool) {
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			var why string
			var err error
			if !failed {
				if why, err = p.WhyNone(c); err == nil {
					why = cmp.Or(why, whyNoRule)
					log.Print("runs: the patch plan started no patch run: " + why)
				}
			}
			return struct{}{}, p.keepWhy(c, why, failed || err != nil)
		}, dbos.WithStepName("plan said")); err != nil && ctx.Err() == nil {
			log.Printf("runs: the patch plan's causes: %v", err)
		}
	}
	stopped := func(err error) bool { return ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) }
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.retain(c) }, dbos.WithStepName("retain")); err != nil {
		if !stopped(err) {
			keep(true)
		}
		return 0, err
	}
	plan, err := dbos.RunAsStep(ctx, func(c context.Context) (rescanPlan, error) { return p.rescanPlan(c) }, dbos.WithStepName("plan"))
	if err != nil {
		if !stopped(err) {
			keep(true)
		}
		return 0, err
	}
	for _, in := range plan.Read {
		in.Run = RescanWorkflow + "-" + tick.ScheduledTime.UTC().Format(time.RFC3339)
		if _, err := dbos.Enqueue[any](ctx, ImageQueue, ImageSBOMWorkflow, in, dbos.WithEnqueueWorkflowID(imageScanID(in.Project, ImageDigest(in.Image), in.Run))); err != nil {
			return 0, err
		}
	}
	matched, failed := map[string]bool{}, 0
	var first error
	for _, img := range plan.Match {
		parts, err := dbos.RunAsStep(ctx, func(c context.Context) ([]string, error) { return p.matchImage(c, img, matched) }, dbos.WithStepName("rematch"))
		if err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)) {
			return 0, err
		}
		if err != nil {
			failed, first = failed+1, cmpErr(first, err)
			continue
		}
		for _, part := range parts {
			matched[part] = true
		}
	}
	if failed > 0 {
		log.Printf("runs: the rescan could not match %d of %d images: %v", failed, len(plan.Match), first)
	}
	if !plan.RuleOn {
		keep(false)
		return len(matched), nil
	}
	parts, err := dbos.RunAsStep(ctx, func(c context.Context) ([]string, error) { return p.rescanRunImage(c, matched) }, dbos.WithStepName("run image"))
	switch {
	case err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)):
		return 0, err
	case err != nil:
		log.Printf("runs: the rescan could not read or match the run image: %v", err)
	}
	for _, part := range parts {
		matched[part] = true
	}
	// its line is planPatches': what it started, and why each other app it read started none (#2575)
	_, err = dbos.RunAsStep(ctx, func(c context.Context) (int, error) {
		started, err := p.planPatches(c)
		if p.Patcher == nil {
			return started, cmpErr(err, c.Err())
		}
		// then each patch whose watch passed is asked for production again while none is open (design/PATCHING.md §4.5)
		if filed, ferr := p.Patcher.FileApprovals(c, p, ""); ferr != nil && c.Err() == nil {
			log.Printf("runs: the patch approvals: %v", ferr)
		} else if filed > 0 {
			log.Printf("runs: the rescan filed %d patch approval(s)", filed)
		}
		// and each one of an identity an admin approved for the fleet is decided as it comes due (§4.5)
		if decided, derr := p.Patcher.DecideFleet(c, p, ""); derr != nil && c.Err() == nil {
			log.Printf("runs: the fleet approvals: %v", derr)
		} else if decided > 0 {
			log.Printf("runs: the fleet approvals approved %d patch approval(s)", decided)
		}
		return started, cmpErr(err, c.Err())
	}, dbos.WithStepName("patch plan"))
	switch {
	case err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)):
		return 0, err
	case err != nil:
		log.Printf("runs: the patch plan: %v", err)
	}
	return len(matched), nil
}

// liveImages are the images an environment runs and the ones kept for a rollback (contracts/routes.md,
// earlier[]: the executor keeps them), of each project not deleted.
const liveImages = `SELECT d.project_id, d.image FROM deploys d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL
UNION SELECT h.project_id, h.image FROM deploy_history h JOIN projects p ON p.id = h.project_id AND p.deleted_at IS NULL WHERE h.kept`

// sweptImages are the images whose scans the sweep keeps: liveImages, and the image each patch run pending or
// running rebased, which no deploy row names between the run's step 3, where its scan is stored and read, and
// its deploy (#2587). A run that ended keeps nothing: its image is preview's then, or no longer read. Never the
// rescan's own reads, which leave a patch run's image to the run.
const sweptImages = liveImages + `
UNION SELECT r.project_id, r.rebased_image FROM runs r WHERE r.kind = 'patch' AND r.state IN ('pending', 'running') AND r.rebased_image IS NOT NULL`

// dropScans and dropParts are the sweep, run in one transaction: a scan of an image nothing runs or keeps, nor a
// patch run reads (sweptImages), a deleted project's among them, then every slice no scan names — an app's or the
// run image's — with its matches. Neither drops a row seen
// since the transaction began (now()): a deploy that stood on a scan while the sweep ran, or a scan that
// stored a slice, marked it, and Postgres checks a row's own seen_at again when a writer changed it under
// the sweep — where a liveness read from the sweep's snapshot would miss the deploy.
const (
	dropScans = `DELETE FROM scanned_images s WHERE s.seen_at < now() AND NOT EXISTS (SELECT 1 FROM (` + sweptImages + `) l
		WHERE l.project_id = s.project_id AND split_part(l.image, '@', 2) = s.digest)`
	dropParts = `DELETE FROM layer_parts s WHERE s.seen_at < now() AND NOT EXISTS (SELECT 1 FROM image_layers l WHERE l.part_sha = s.part_sha)
		AND NOT EXISTS (SELECT 1 FROM run_image_layers r WHERE r.part_sha = s.part_sha)`
)

// retain is the sweep.
func (p *Pipeline) retain(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, dropScans); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, dropParts)
		return err
	})
}

// rescanPlan is this tick's work over the images that run or are kept, of each project whose
// platform.known-vuln is on (design/PATCHING.md §3.3) or whose deps.known-vuln is placed before production
// (§3.6): read each image with no finished scan, and match each finished scan's covered layers, its app layers
// too for the latter, the scans matched longest ago first, so no project waits behind
// another's every tick. A project whose policy does not read is logged and left: its reads go stale and
// say so, never pass.
func (p *Pipeline) rescanPlan(ctx context.Context) (rescanPlan, error) {
	plan := rescanPlan{Read: []imageScan{}, Match: []rescanImage{}}
	policyOf := p.ProjectPolicy.Load()
	if policyOf == nil {
		return plan, nil
	}
	rows, err := p.Pool.Query(ctx, liveImages+" ORDER BY 1, 2")
	if err != nil {
		return plan, err
	}
	var live []imageScan
	var project, image string
	if _, err := pgx.ForEachRow(rows, []any{&project, &image}, func() error {
		if ImageDigest(image) != "" {
			live = append(live, imageScan{Project: project, Image: image})
		}
		return nil
	}); err != nil {
		return plan, err
	}
	type reads struct{ images, platform, app bool }
	on := map[string]reads{}
	var projects, digests []string
	for _, in := range live {
		r, asked := on[in.Project]
		if !asked {
			snap, err := policyOf(ctx, p.Pool, in.Project)
			if err != nil {
				log.Printf("runs: the rescan leaves %s: its policy: %v", in.Project, err)
			}
			r = reads{images: err == nil && scansImages(snap), platform: err == nil && ReadsImages(snap), app: err == nil && readsBuiltApp(snap)}
			on[in.Project] = r
		}
		if r.images {
			projects, digests = append(projects, in.Project), append(digests, ImageDigest(in.Image))
		}
		plan.RuleOn = plan.RuleOn || r.platform // the run image is read for the patch plan alone
	}
	rows, err = p.Pool.Query(ctx, `SELECT l.project, l.digest, s.project_id IS NOT NULL AND s.not_finished IS NULL FROM unnest($1::text[], $2::text[]) AS l (project, digest)
		LEFT JOIN scanned_images s ON s.project_id = l.project AND s.digest = l.digest
		ORDER BY s.rematched_at NULLS FIRST, s.scanned_at NULLS FIRST, l.project, l.digest`, projects, digests)
	if err != nil {
		return plan, err
	}
	var digest string
	var finished bool // a scan of it is stored, and it finished
	var scanned [][2]string
	if _, err := pgx.ForEachRow(rows, []any{&project, &digest, &finished}, func() error {
		if finished {
			scanned = append(scanned, [2]string{project, digest})
			return nil
		}
		for _, in := range live {
			if in.Project == project && ImageDigest(in.Image) == digest {
				plan.Read = append(plan.Read, in)
				break
			}
		}
		return nil
	}); err != nil {
		return plan, err
	}
	for _, s := range scanned {
		img, err := p.toMatch(ctx, s[0], s[1], on[s[0]].app)
		if err != nil {
			return plan, err
		}
		if img != nil && !containsScan(plan.Match, *img) {
			plan.Match = append(plan.Match, *img)
		}
	}
	return plan, nil
}

func containsScan(images []rescanImage, img rescanImage) bool {
	for _, i := range images {
		if i.Project == img.Project && i.Digest == img.Digest {
			return true
		}
	}
	return false
}

// toMatch is a project's finished scan of digest with the layers a re-match covers, and with app the layers
// holding an app package and no package of a platform class apart, nil when it covers none or Canyon's read
// would not read it: its read says could not check whatever a match says.
func (p *Pipeline) toMatch(ctx context.Context, project, digest string, app bool) (*rescanImage, error) {
	var diffIDs []string
	var label string
	err := p.Pool.QueryRow(ctx, "SELECT diff_ids, coalesce(lifecycle, '') FROM scanned_images WHERE project_id = $1 AND digest = $2 AND not_finished IS NULL",
		project, digest).Scan(&diffIDs, &label)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	owners, labelled := trivy.Owners(diffIDs, label)
	if !labelled || !trivy.DiffIDsRead(diffIDs) {
		return nil, nil
	}
	rows, err := p.Pool.Query(ctx, `SELECT l.diff_id, l.part_sha, s.components FROM image_layers l JOIN layer_parts s USING (part_sha)
		WHERE l.project_id = $1 AND l.digest = $2 AND l.diff_id = ANY($3)`, project, digest, diffIDs)
	if err != nil {
		return nil, err
	}
	layers, parts, whole := map[string][]trivy.Component{}, map[string]string{}, true
	var d, part string
	var raw []byte
	if _, err := pgx.ForEachRow(rows, []any{&d, &part, &raw}, func() error {
		cs, ok := trivy.DecodeComponents(raw)
		whole = whole && ok && trivy.StoredIn(cs, d)
		layers[d], parts[d] = cs, part
		return nil
	}); err != nil {
		return nil, err
	}
	if !whole || len(layers) != len(diffIDs) {
		return nil, nil
	}
	img := rescanImage{Project: project, Digest: digest}
	pkgs := trivy.Packages(diffIDs, owners, layers)
	platform := trivy.Covered(diffIDs, pkgs, false)
	for _, d := range platform {
		img.Layers = append(img.Layers, rescanLayer{DiffID: d, Part: parts[d]})
	}
	for _, d := range trivy.Covered(diffIDs, pkgs, app) {
		if !slices.Contains(platform, d) {
			img.AppLayers = append(img.AppLayers, rescanLayer{DiffID: d, Part: parts[d]})
		}
	}
	if len(img.Layers) == 0 && len(img.AppLayers) == 0 {
		return nil, nil
	}
	return &img, nil
}

// storedMatch is one slice's re-match as it is stored (contracts/finding.md, System libraries and build tools).
type storedMatch struct {
	Part      string
	DiffID    string
	DBBuiltAt time.Time
	Report    json.RawMessage
	Unscanned []string
}

// matchImage matches the slice of each of a scan's covered layers not matched yet this tick, then stores
// the matches in one transaction, each replacing its slice's older ones — Canyon's read reads a slice's
// newest alone — and marks the scan matched. A layer that did not match stores none of them and marks the
// scan's rescan failed, which its read reads as could not check until a later match of them all. Its app
// layers are matched the same way apart, after them: one that does not match marks only the scan's app
// rescan, which deps.known-vuln's read of the built app alone reads, and is logged, never this answer's error.
func (p *Pipeline) matchImage(ctx context.Context, img rescanImage, matched map[string]bool) ([]string, error) {
	table, where, args := img.scanRow()
	found, err := p.matchLayers(ctx, img.Layers, matched)
	if err == nil {
		err = storeMatches(ctx, p.Pool, found, "UPDATE "+table+" SET rematched_at = now(), rescan_failed_at = NULL, rescan_failed = NULL WHERE "+where, args)
	}
	if err != nil {
		if _, uerr := p.Pool.Exec(context.WithoutCancel(ctx), "UPDATE "+table+" SET rescan_failed_at = now(), rescan_failed = $"+strconv.Itoa(len(args)+1)+" WHERE "+where,
			append(args, schedulerSaid(err.Error()))...); uerr != nil {
			return nil, errors.Join(err, uerr)
		}
		return nil, fmt.Errorf("%s of %s: %w", img.Digest, cmp.Or(img.Project, "the run image"), err)
	}
	parts := make([]string, 0, len(found))
	for _, m := range found {
		parts = append(parts, m.Part)
	}
	if len(img.AppLayers) == 0 {
		return parts, nil
	}
	app, err := p.matchLayers(ctx, img.AppLayers, matched)
	if err == nil {
		err = storeMatches(ctx, p.Pool, app, "UPDATE scanned_images SET app_rescan_failed_at = NULL, app_rescan_failed = NULL WHERE "+where, args)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		if _, uerr := p.Pool.Exec(context.WithoutCancel(ctx), "UPDATE scanned_images SET app_rescan_failed_at = now(), app_rescan_failed = $"+strconv.Itoa(len(args)+1)+" WHERE "+where,
			append(args, schedulerSaid(err.Error()))...); uerr != nil {
			return nil, uerr
		}
		log.Printf("runs: the app layers of %s of %s did not match: %v", img.Digest, img.Project, err)
		return parts, nil
	}
	for _, m := range app {
		parts = append(parts, m.Part)
	}
	return parts, nil
}

// storeMatches stores found in one transaction with mark, each match replacing its slice's older ones.
func storeMatches(ctx context.Context, pool *pgxpool.Pool, found []storedMatch, mark string, args []any) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, m := range found {
			if _, err := tx.Exec(ctx, `INSERT INTO part_matches (part_sha, db_built_at, matched_at, report, unscanned) VALUES ($1, $2, now(), $3, $4)
				ON CONFLICT (part_sha, db_built_at) DO NOTHING`, m.Part, m.DBBuiltAt, m.Report, m.Unscanned); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM part_matches WHERE part_sha = $1
				AND db_built_at < (SELECT max(db_built_at) FROM part_matches WHERE part_sha = $1)`, m.Part); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, mark, args...)
		return err
	})
}

// matchLayers re-matches each layer's slice not matched yet, as it was hashed, through target sbom.
func (p *Pipeline) matchLayers(ctx context.Context, layers []rescanLayer, matched map[string]bool) ([]storedMatch, error) {
	var found []storedMatch
	for _, l := range layers {
		if matched[l.Part] {
			continue
		}
		var system, raw []byte
		if err := p.Pool.QueryRow(ctx, "SELECT os, components FROM layer_parts WHERE part_sha = $1", l.Part).Scan(&system, &raw); err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.DiffID, err)
		}
		cs, ok := trivy.DecodeComponents(raw)
		part, err := sbomPart(system, raw)
		switch {
		case err != nil:
			return nil, fmt.Errorf("layer %s: %w", l.DiffID, err)
		case !ok || partSHA(part) != l.Part:
			return nil, fmt.Errorf("layer %s: its stored slice is not the one it was stored as", l.DiffID)
		}
		res, err := p.askScan(ctx, executor.ScanRequest{Tool: "trivy", Target: "sbom"}, part)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.DiffID, err)
		}
		m, err := matchOf(l.DiffID, trivy.Packages([]string{l.DiffID}, nil, map[string][]trivy.Component{l.DiffID: cs}), res, time.Now())
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.DiffID, err)
		}
		m.Part = l.Part
		found = append(found, m)
	}
	return found, nil
}

// sbomPart is what the re-match sends of a stored layer (contracts/executor.md, target sbom): its
// components as trivy wrote them, beside the image's operating-system component when it has one, each in
// its canonical form, so the part is the same bytes however it was stored.
func sbomPart(system, components []byte) ([]byte, error) {
	var cs []json.RawMessage
	if err := json.Unmarshal(components, &cs); err != nil {
		return nil, err
	}
	if system != nil {
		cs = append([]json.RawMessage{system}, cs...)
	}
	for i, c := range cs {
		var err error
		if cs[i], err = canonical(c); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "components": cs})
}

// matchOf is what a re-match answer stores for layer at now: trivy's report and the bom-refs it never
// matched, on a database built within the 5 days. Anything else — any not_finished, a stale or undated
// database, a report Canyon's read would not read, past what Canyon keeps, or naming a package the layer
// does not hold — is an error, and stores nothing: never a clean match.
func matchOf(layer string, pkgs []trivy.Package, res executor.ScanResult, now time.Time) (storedMatch, error) {
	switch {
	case res.NotFinished != "":
		return storedMatch{}, fmt.Errorf("the re-match did not finish: %s %s", res.NotFinished, res.Reason)
	case len(res.Report) > maxStored:
		return storedMatch{}, fmt.Errorf("a report of %d bytes, past the %d kept", len(res.Report), maxStored)
	}
	built, err := time.Parse(time.RFC3339, res.DBBuiltAt)
	switch {
	case err != nil:
		return storedMatch{}, fmt.Errorf("a re-match on a database of no date: %w", err)
	case built.After(now):
		return storedMatch{}, fmt.Errorf("a database built at %s, after now", res.DBBuiltAt)
	case now.Sub(built) > staleDatabase:
		return storedMatch{}, fmt.Errorf("a database built at %s, over 5 days ago", res.DBBuiltAt)
	}
	report, ok := trivy.DecodeReport(res.Report)
	switch {
	case !ok:
		return storedMatch{}, errors.New("a report that is not trivy's")
	case !trivy.Joins(report, layer, pkgs):
		return storedMatch{}, errors.New("a match naming a package the layer does not hold")
	}
	return storedMatch{DiffID: layer, DBBuiltAt: built, Report: res.Report, Unscanned: append([]string{}, res.Unscanned...)}, nil
}
