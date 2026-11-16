package runs

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
)

// PatchActor starts every patch run (contracts/routes.md, Patch runs): it says who started the run, never
// whose code it is.
var PatchActor = events.Actor{Kind: "canyon", Subject: "patch", Display: "Canyon"}

// Patch is a patch run's plan: a rebase onto RunImage, or with Rebuild a rebuild on Builder (design/PATCHING.md §5),
// of the image preview ran when planned, for the fixes the plan named, in its identity's wave (design/PATCHING.md
// §4.3).
type Patch struct {
	Identity string            `json:"identity"`
	RunImage string            `json:"run_image"`
	Image    string            `json:"image"`
	Wave     int               `json:"wave"`
	Fixes    []PlatformFinding `json:"fixes"`
	Rebuild  bool              `json:"rebuild,omitempty"`
	Builder  string            `json:"builder,omitempty"`
}

// Patcher is what decides and schedules Canyon's patches, Canyon Enterprise's (ee/patching; design/ARCHITECTURE.md
// §8): each rescan tick's plan, filing the patch approvals of watches that passed, and the fleet approval's
// decisions. nil in a server built without it, which reads patching as off (patchingOn), so it plans no patch run,
// files no patch approval and decides none (contracts/routes.md, Licence).
type Patcher interface {
	// Plan is a tick's plan: the patch runs it started, its causes kept for the identities read.
	Plan(ctx context.Context, p *Pipeline) (started int, err error)
	// FileApprovals asks production for each patch whose watch passed in preview, project's alone or every app's
	// for "": how many it filed.
	FileApprovals(ctx context.Context, p *Pipeline, project string) (int, error)
	// DecideFleet decides the open patch approvals of identity, or of every identity for "", that an admin approved
	// for the fleet: how many it approved.
	DecideFleet(ctx context.Context, p *Pipeline, identity string) (int, error)
}

// Why a tick plans onto no run image before it reads one (WhyNone; #2575), and why a start started no patch run
// (StartPatch). Each is one cause, distinct from every other and from the plan's own, and names no image's contents.
const (
	WhyServerOff   = "this server does not patch: CANYON_PATCHING is not on, or sign-in is off"
	WhyOrgOff      = "the org's patching is off"
	WhyNotWired    = "this server has no executor, image read or policy to plan with"
	whyNoRule      = "no app has platform.known-vuln on"
	WhyStarted     = "started"
	WhyRuleOff     = "platform.known-vuln is off"
	WhyGoesFirst   = "another run of it goes first"
	WhyOffline     = "it is being taken offline"
	WhyMoved       = "it changed since the plan: another image, deleted, or its identity paused or stalled"
	WhyRebuildBusy = "another rebuild runs first"
)

// noPlanCauses are the identities read's words for why a tick planned onto no run image (contracts/routes.md, the
// identities row's plan); a cause not here is unknown (fail closed).
var noPlanCauses = map[string]string{WhyServerOff: "server_off", WhyOrgOff: "org_off", WhyNotWired: "not_wired", whyNoRule: "rule_off"}

// keepWhy replaces the one patch_plan_said row with why a tick planned onto no run image, failed when it failed
// before it could say, as the identities read serves it.
func (p *Pipeline) keepWhy(ctx context.Context, why string, failed bool) error {
	code, ok := noPlanCauses[why]
	switch {
	case failed:
		code = "failed"
	case !ok:
		code = "unknown"
	}
	_, err := p.Pool.Exec(ctx, `INSERT INTO patch_plan_said (said_at, identity, why, apps) VALUES (now(), NULL, $1, '{}')
		ON CONFLICT (one) DO UPDATE SET said_at = EXCLUDED.said_at, identity = EXCLUDED.identity, why = EXCLUDED.why, apps = EXCLUDED.apps`, code)
	return err
}

// WhyNone is why a tick plans onto no run image before it reads one, "" when it may: patching off on the server or for
// the org — a server built without Canyon Enterprise reads the org's as off — or nothing to plan with.
func (p *Pipeline) WhyNone(ctx context.Context) (string, error) {
	switch {
	case !p.Patches.Load():
		return WhyServerOff, nil
	case p.PlatformRead.Load() == nil || p.ProjectPolicy.Load() == nil || p.Executor == nil:
		return WhyNotWired, nil
	}
	if on, err := p.patchingOn(ctx, p.Pool); err != nil {
		return "", err
	} else if !on {
		return WhyOrgOff, nil
	}
	return "", nil
}

// planPatches is a tick's patch plan (design/PATCHING.md §4.2): the Patcher's, or, in a server built without
// Canyon Enterprise, why it plans none (WhyNone), logged and kept for the identities read (keepWhy).
func (p *Pipeline) planPatches(ctx context.Context) (int, error) {
	if p.Patcher != nil {
		return p.Patcher.Plan(ctx, p)
	}
	why, err := p.WhyNone(ctx)
	if err == nil {
		log.Print("runs: the patch plan started no patch run: " + why)
	}
	if ctx.Err() == nil {
		err = cmpErr(err, p.keepWhy(ctx, why, err != nil))
	}
	return 0, err
}

// patchRunSQL inserts a patch run, its author the author of the run whose image it rebases (contracts/routes.md,
// Patch runs): preview's row's gate_run, as that run recorded its own (Author) — none when it recorded none, or
// recorded one in part: a person or an agent with a subject and a display, never one made up.
var patchRunSQL = `INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display, kind, deploys,
		author_kind, author_subject, author_display, author_github)
	SELECT $1, $2, $3, 'pending', $4, $5, $6, 'patch', false, w.kind, w.subject, w.display, w.github
	FROM (SELECT 1) one LEFT JOIN LATERAL (SELECT a.kind, a.subject, a.display, a.github FROM deploys d JOIN runs g ON g.id = d.gate_run,
		LATERAL (SELECT ` + Author("g", "kind") + ` AS kind, ` + Author("g", "subject") + ` AS subject, ` + Author("g", "display") + ` AS display, ` + Author("g", "github") + ` AS github) a
		WHERE d.project_id = $2 AND d.env = 'preview' AND a.kind IN ('person', 'agent') AND a.subject IS NOT NULL AND a.display IS NOT NULL) w ON true`

// StartPatch starts project's patch run of the plan pt — its identity, a rebase's run image or a rebuild's builder,
// the image preview ran when planned and the fixes the plan named — on the patches queue, and marks its app running in
// identity's waves, in one transaction under the ledger lock every start takes: only while the project is live and
// not being deleted, no other run of it is pending or running, no undeploy holds it, preview still runs the image the
// plan named, the identity is not paused, nor stalled naming the app, and has reached the app's wave, a rebuild only
// while no other rebuild is before its /run, and the project's platform.known-vuln is on — each read again here, after
// the plan's own commit — so a person's run always goes first (contracts/routes.md, Patch runs). It answers
// WhyStarted when it started one, else why not.
func (p *Pipeline) StartPatch(ctx context.Context, project string, pt Patch) (string, error) {
	identity := pt.Identity
	var why string
	policyOf := p.ProjectPolicy.Load()
	if policyOf == nil {
		return WhyNotWired, nil
	}
	err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
		if err := UndeployingTx(ctx, tx, project); errors.Is(err, ErrUndeploying) { // the app goes offline: nothing to patch
			why = WhyOffline
			return nil
		} else if err != nil {
			return err
		}
		var pkg string
		var wave int
		var busy bool
		err := tx.QueryRow(ctx, `SELECT d.package_id, a.wave, EXISTS (SELECT 1 FROM runs r WHERE r.project_id = d.project_id AND r.state IN ('pending', 'running'))
			FROM deploys d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL AND p.deleting_since IS NULL
			JOIN patch_wave_apps a ON a.project_id = d.project_id AND a.identity = $3 AND a.outcome = 'planned'
			JOIN patch_identities i ON i.id = a.identity AND i.paused_at IS NULL AND NOT a.project_id = ANY(coalesce(i.stalled_projects, '{}')) AND a.wave <= i.wave
			WHERE d.project_id = $1 AND d.env = 'preview' AND d.image = $2`, project, pt.Image, identity).Scan(&pkg, &wave, &busy)
		switch {
		case errors.Is(err, pgx.ErrNoRows): // gone, moved on or paused since the plan
			why = WhyMoved
			return nil
		case err != nil:
			return err
		case busy: // another run of the project goes first
			why = WhyGoesFirst
			return nil
		}
		if pt.Rebuild { // one rebuild at a time on the host, up to its /run (contracts/routes.md, A rebuild patch)
			var building bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs r JOIN patch_wave_apps a ON a.run = r.id JOIN patch_identities i ON i.id = a.identity AND i.kind = 'rebuild'
				WHERE r.state IN ('pending', 'running') AND r.deploy_started_at IS NULL)`).Scan(&building); err != nil || building {
				why = WhyRebuildBusy
				return err
			}
		}
		// the snapshot as a start composes it, on this transaction under the ledger lock a policy apply also
		// takes — never through another connection of the pool, which a lock waiter may have taken the last of
		snap, err := policyOf(ctx, tx, project)
		if err != nil {
			return err
		}
		if !ReadsImages(snap) { // platform.known-vuln turned off since the plan: nothing is patched for it (design/PATCHING.md §3.3)
			why = WhyRuleOff
			return nil
		}
		id, err := newID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, patchRunSQL, id, project, pkg, PatchActor.Kind, PatchActor.Subject, PatchActor.Display); err != nil {
			return err
		}
		if _, err := events.AppendRunTx(ctx, tx, id, project, events.RunState, "state:pending", map[string]any{"state": "pending"}); err != nil {
			return err
		}
		reports, err := CurrentReports(ctx, tx, pkg, importedTools(snap))
		if err != nil {
			return err
		}
		in := Input{Run: id, Project: project, Package: pkg, Actor: PatchActor, Policy: snap, Reports: reports,
			Patch: &Patch{Identity: identity, RunImage: pt.RunImage, Image: pt.Image, Wave: wave, Fixes: pt.Fixes, Rebuild: pt.Rebuild, Builder: pt.Builder}}
		if _, err := dbos.Enqueue[any](p.dc, PatchesQueue, Workflow, in, dbos.WithEnqueueWorkflowID(id), dbos.WithEnqueueTransaction(tx)); err != nil {
			return fmt.Errorf("runs: enqueue: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE patch_wave_apps SET outcome = 'running', run = $3, outcome_at = now() WHERE identity = $1 AND project_id = $2", identity, project, id); err != nil {
			return err
		}
		why = WhyStarted
		return nil
	})
	return why, err
}
