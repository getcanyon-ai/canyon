package main

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
)

// envPatch is envs.<env>.patch (contracts/routes.md, Patch runs; patch.schema.json's env_patch): the environment's
// patch state, at contributor and above.
type envPatch struct {
	State    string `json:"state"`
	Since    string `json:"since"`
	Identity string `json:"identity,omitempty"`
	Run      string `json:"run,omitempty"`
	Approval string `json:"approval,omitempty"`
	Why      string `json:"why,omitempty"`
}

// planFact is the app's newest place in an identity's waves: what became of its patch, and that patch run's end.
type planFact struct {
	identity, outcome, run, image  string
	at                             time.Time
	paused                         *time.Time
	runState, failure, notPlatform string
}

// rowFact is one environment's deploy row as the patch state reads it: whether its gate run is a patch run, and for
// preview that patch's place in its waves and the image it replaced; for production whether its history marks the
// image patched, and the patch run that made it.
type rowFact struct {
	image, gateRun string
	deployed       time.Time
	patchRun       bool
	identity       string // the patch's identity, where its waves name it
	outcome        string
	outcomeAt      *time.Time
	replaced       string
	paused         *time.Time
	patched        bool
	madeBy         string
}

// approvalFact is the app's newest patch approval: open when pending, unexpired and current as its read reads it
// (routes_promotion_approvals.go, patchMoved), the production image it replaces, and the promote run its approval
// started, and whether it runs.
type approvalFact struct {
	code, identity, patchRun string
	replaces                 string
	promoteRun               string
	state                    string
	open, promoting          bool
	requested                time.Time
	decided                  *time.Time
}

// patchFacts is what an app's patch state is read from (contracts/routes.md, envs.<env>.patch): on is the one
// condition Canyon patches it under (runs.Pipeline.Patching).
type patchFacts struct {
	on            bool
	plan          *planFact
	preview, prod *rowFact
	approval      *approvalFact
	held          *time.Time
}

// whyHeld is a patch run's end as held's why (contracts/routes.md, envs.<env>.patch).
func whyHeld(p planFact) string {
	switch {
	case p.runState == "blocked":
		return "blocked"
	case p.failure == "not_platform_only" && (p.notPlatform == "layers_changed" || p.notPlatform == "config_changed"):
		return p.notPlatform
	case p.failure == "not_same_inputs", p.failure == "patch_regressed", p.failure == "patch_ineffective":
		return p.failure
	}
	return "failed"
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// previewPatch is preview's state (contracts/routes.md, envs.<env>.patch, In preview). A plan applies only while
// preview runs the image it was planned on, or the patch it deployed: a deploy since clears it.
func previewPatch(f patchFacts) envPatch {
	pv := f.preview
	none := envPatch{State: "none", Since: stamp(pv.deployed)}
	if !f.on {
		return none
	}
	if pl := f.plan; pl != nil && pv.gateRun != pl.run && pv.image == pl.image {
		switch pl.outcome {
		case runs.PatchReverted:
			return envPatch{State: "reverted", Since: stamp(pl.at), Identity: pl.identity, Run: pl.run}
		case runs.PatchFailed, runs.PatchFailedStart:
			return envPatch{State: "held", Since: stamp(pl.at), Identity: pl.identity, Run: pl.run, Why: whyHeld(*pl)}
		case runs.PatchPlanned, runs.PatchRunning, runs.PatchEndedEarly:
			if pl.paused != nil {
				return envPatch{State: "held", Since: stamp(*pl.paused), Identity: pl.identity, Run: pl.run, Why: "paused"}
			}
			return envPatch{State: "planned", Since: stamp(pl.at), Identity: pl.identity, Run: pl.run}
		}
	}
	if pv.patchRun && pv.identity != "" {
		since := pv.deployed
		if pv.outcome == runs.PatchPassed && pv.outcomeAt != nil {
			since = *pv.outcomeAt
		}
		return envPatch{State: "patched_preview", Since: stamp(since), Identity: pv.identity, Run: pv.gateRun}
	}
	return none
}

// prodPatch is production's state (contracts/routes.md, envs.<env>.patch, In production).
func prodPatch(f patchFacts) envPatch {
	pd := f.prod
	none := envPatch{State: "none", Since: stamp(pd.deployed)}
	if !f.on {
		return none
	}
	if ap := f.approval; ap != nil {
		switch {
		case ap.state == "pending" && ap.open:
			return envPatch{State: "asked", Since: stamp(ap.requested), Identity: ap.identity, Run: ap.patchRun, Approval: runs.ShownCode(ap.code)}
		case ap.state == "approved" && ap.promoting && ap.decided != nil:
			return envPatch{State: "planned", Since: stamp(*ap.decided), Identity: ap.identity, Run: ap.patchRun}
		case ap.state == "denied" && ap.decided != nil && pd.image == ap.replaces: // its team declined it, and Canyon asks no more for the identity
			return envPatch{State: "denied", Since: stamp(*ap.decided), Identity: ap.identity, Run: ap.patchRun, Approval: runs.ShownCode(ap.code)}
		}
	}
	pv := f.preview
	if pl := f.plan; pl != nil && pl.outcome == runs.PatchReverted && pv != nil && pv.gateRun == pl.run {
		// production's watch, and so its revert line, is the promote run's the approval started (runs, the watch)
		reverted := envPatch{State: "reverted", Since: stamp(pl.at), Identity: pl.identity}
		if ap := f.approval; ap != nil && ap.state == "approved" && ap.patchRun == pl.run {
			reverted.Run = ap.promoteRun
		}
		return reverted
	}
	if pv != nil && pv.patchRun && pv.outcome == runs.PatchPassed && pv.identity != "" && pd.image != pv.image {
		held := envPatch{State: "held", Identity: pv.identity, Run: pv.gateRun}
		switch {
		case f.held != nil:
			held.Since, held.Why = stamp(*f.held), "production_held"
			return held
		case pd.image != pv.replaced:
			held.Since, held.Why = stamp(pd.deployed), "preview_ahead"
			return held
		case pv.paused != nil:
			held.Since, held.Why = stamp(*pv.paused), "paused"
			return held
		}
	}
	if pd.patched {
		return envPatch{State: "patched", Since: stamp(pd.deployed), Run: pd.madeBy}
	}
	return none
}

// patchAskOpen is whether patch approval x is open but for the switch, as its read reads it (patchMoved), what needs a
// person reads as a patch waiting (needs.go): pending,
// unexpired, preview running what it names, production what it replaces, and its identity not paused.
const patchAskOpen = `(x.state = 'pending' AND x.expires_at > now()
		AND EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = x.project_id AND d.env = 'preview' AND d.package_id = x.package_id AND d.image = x.image)
		AND EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = x.project_id AND d.env = 'prod' AND d.image = x.replaces_image)
		AND EXISTS (SELECT 1 FROM patch_identities i WHERE i.id = x.patch_identity AND i.paused_at IS NULL))`

// patchingFor is runs.Pipeline.Patching of each of ids, read once for them all: the operator's and the org's switch,
// the org's policy, and the apps' own platform.known-vuln additions in one query.
func (a *app) patchingFor(ctx context.Context, q querier, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if a.pipe == nil || len(ids) == 0 {
		return out, nil
	}
	if on, err := a.pipe.PatchingOn(ctx, q); err != nil || !on {
		return out, err
	}
	org, err := a.effectivePolicy(ctx, q)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(org.Rules, func(r gatepolicy.Rule) bool { return r.ID == platformRule })
	if i < 0 { // the boot snapshot refuses to load without it
		return nil, errors.New("the snapshot carries no " + platformRule)
	}
	rows, err := q.Query(ctx, `SELECT project_id, COALESCE(state, ''), COALESCE(production, '') FROM app_policy_additions WHERE rule = $2 AND project_id = ANY($1)`, ids, platformRule)
	if err != nil {
		return nil, err
	}
	adds := map[string]addition{}
	var id string
	var add addition
	if _, err := pgx.ForEachRow(rows, []any{&id, &add.State, &add.Production}, func() error { adds[id] = add; return nil }); err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = adds[id].applied(org.Rules[i]).State != gatepolicy.Off
	}
	return out, nil
}

// patchFactsSQL reads each app's patch facts ($1 the apps): its newest plan, the deploy rows Canyon runs with
// their patch runs, its newest patch approval with whether it is open as its read reads it, and production's hold.
const patchFactsSQL = `SELECT p.id,
	a.identity, a.outcome, a.run, a.image, a.outcome_at, ai.paused_at, ar.state, ar.failure_reason, ar.not_platform_only,
	pv.image, pv.deployed_at, pv.gate_run, pvr.kind = 'patch', pva.identity, pva.outcome, pva.outcome_at, pvr.patch_replaced->>'image', pvi.paused_at,
	pd.image, pd.deployed_at, pd.gate_run, coalesce(ph.patched, false),
	CASE WHEN ph.patched THEN (SELECT y.gate_run FROM deploy_history y JOIN runs r ON r.id = y.gate_run AND r.kind = 'patch'
		WHERE y.project_id = p.id AND y.image = pd.image ORDER BY y.deployed_at DESC, y.id DESC LIMIT 1) END,
	x.code, x.state, x.patch_identity, x.gate_run, x.replaces_image, coalesce(x.run_id, ''), x.requested_at, x.decided_at,
	` + patchAskOpen + `,
	coalesce(xr.state IN ('pending', 'running'), false),
	h.held_at
FROM unnest($1::text[]) p(id)
LEFT JOIN LATERAL (SELECT * FROM patch_wave_apps w WHERE w.project_id = p.id ORDER BY w.outcome_at DESC, w.identity LIMIT 1) a ON true
LEFT JOIN patch_identities ai ON ai.id = a.identity
LEFT JOIN runs ar ON ar.id = a.run
LEFT JOIN deploys pv ON pv.project_id = p.id AND pv.env = 'preview' AND pv.run_by = 'canyon'
LEFT JOIN runs pvr ON pvr.id = pv.gate_run
LEFT JOIN patch_wave_apps pva ON pva.project_id = p.id AND pva.run = pv.gate_run AND pvr.kind = 'patch'
LEFT JOIN patch_identities pvi ON pvi.id = pva.identity
LEFT JOIN deploys pd ON pd.project_id = p.id AND pd.env = 'prod' AND pd.run_by = 'canyon'
LEFT JOIN LATERAL (SELECT patched FROM deploy_history y WHERE y.project_id = p.id AND y.image = pd.image ORDER BY y.deployed_at DESC, y.id DESC LIMIT 1) ph ON true
LEFT JOIN LATERAL (SELECT * FROM promotion_approvals z WHERE z.project_id = p.id AND z.kind = 'patch' ORDER BY z.requested_at DESC, z.code LIMIT 1) x ON true
LEFT JOIN runs xr ON xr.id = x.run_id
LEFT JOIN prod_holds h ON h.project_id = p.id`

// patchFactsOf reads the patch facts of projects, with on read only for an app that has any: one with none reads
// none whatever the switch, and composes no policy.
func (a *app) patchFactsOf(ctx context.Context, q querier, projects []string) (map[string]patchFacts, error) {
	rows, err := q.Query(ctx, patchFactsSQL, projects)
	if err != nil {
		return nil, err
	}
	out := map[string]patchFacts{}
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var id string
			var pl struct {
				identity, outcome, run, image, runState, failure, notPlatform *string
				at, paused                                                    *time.Time
			}
			var pv, pd struct {
				image, gateRun, identity, outcome, replaced, madeBy *string
				deployed, outcomeAt, paused                         *time.Time
				patchRun, patched                                   *bool
			}
			var x struct {
				code, state, identity, patchRun, replaces, promoteRun *string
				requested, decided                                    *time.Time
				open                                                  *bool
				promoting                                             bool
			}
			var held *time.Time
			if err := rows.Scan(&id, &pl.identity, &pl.outcome, &pl.run, &pl.image, &pl.at, &pl.paused, &pl.runState, &pl.failure, &pl.notPlatform,
				&pv.image, &pv.deployed, &pv.gateRun, &pv.patchRun, &pv.identity, &pv.outcome, &pv.outcomeAt, &pv.replaced, &pv.paused,
				&pd.image, &pd.deployed, &pd.gateRun, &pd.patched, &pd.madeBy,
				&x.code, &x.state, &x.identity, &x.patchRun, &x.replaces, &x.promoteRun, &x.requested, &x.decided, &x.open, &x.promoting, &held); err != nil {
				return err
			}
			f := patchFacts{held: held}
			if pl.identity != nil {
				f.plan = &planFact{identity: *pl.identity, outcome: *pl.outcome, run: deref(pl.run), image: *pl.image, at: *pl.at, paused: pl.paused,
					runState: deref(pl.runState), failure: deref(pl.failure), notPlatform: deref(pl.notPlatform)}
			}
			if pv.image != nil {
				f.preview = &rowFact{image: *pv.image, gateRun: *pv.gateRun, deployed: *pv.deployed, patchRun: pv.patchRun != nil && *pv.patchRun,
					identity: deref(pv.identity), outcome: deref(pv.outcome), outcomeAt: pv.outcomeAt, replaced: deref(pv.replaced), paused: pv.paused}
			}
			if pd.image != nil {
				f.prod = &rowFact{image: *pd.image, gateRun: *pd.gateRun, deployed: *pd.deployed, patched: *pd.patched, madeBy: deref(pd.madeBy)}
			}
			if x.code != nil {
				f.approval = &approvalFact{code: *x.code, state: *x.state, identity: *x.identity, patchRun: *x.patchRun, replaces: deref(x.replaces), promoteRun: deref(x.promoteRun), requested: *x.requested, decided: x.decided,
					open: x.open != nil && *x.open, promoting: x.promoting}
			}
			out[id] = f
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}
	on, err := a.patchingFor(ctx, q, projects)
	for id, f := range out {
		f.on = on[id]
		out[id] = f
	}
	return out, err
}

// sayPatch sets patch on each environment Canyon runs of projects read whole (contracts/routes.md, envs.<env>.patch),
// by id, from one read of their facts. It answers the facts, which the status read reads again.
func (a *app) sayPatch(ctx context.Context, q querier, envs map[string]map[string]any) (map[string]patchFacts, error) {
	if len(envs) == 0 || a.pipe == nil || !a.pipe.Patches.Load() { // until the operator turns patching on (beta), no read says a patch state
		return nil, nil
	}
	ids := make([]string, 0, len(envs))
	for id := range envs {
		ids = append(ids, id)
	}
	facts, err := a.patchFactsOf(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for id, f := range facts {
		if m, ok := envs[id]["preview"].(map[string]any); ok && f.preview != nil && m["run_by"] == "canyon" {
			m["patch"] = previewPatch(f)
		}
		if m, ok := envs[id]["prod"].(map[string]any); ok && f.prod != nil && m["run_by"] == "canyon" {
			m["patch"] = prodPatch(f)
		}
	}
	return facts, nil
}

// sayPatchNow is the status read's second look (contracts/routes.md, envs.<env>.patch): an environment that reads
// none while Canyon patches the app reads could_not_rescan when its image read could not check, or has a counted os
// finding the named run image's match cannot tell of, and wait when that run image carries no fix for one.
func (a *app) sayPatchNow(ctx context.Context, project string, envs map[string]any, f patchFacts) error {
	if a.pipe == nil {
		return nil
	}
	if !f.on {
		return nil
	}
	var named *runs.NamedRunImage
	for _, env := range []string{"preview", "prod"} {
		m, ok := envs[env].(map[string]any)
		if !ok {
			continue
		}
		state, ok := m["patch"].(envPatch)
		digest, _ := m["image_digest"].(string)
		if !ok || state.State != "none" || digest == "" {
			continue
		}
		f, err := loadImage(ctx, a.pool, project, digest) // as the patch plan reads it (main, PlatformRead)
		if err != nil {
			return err
		}
		read := platformImage(readImage(f, env, digest, gatepolicy.Advisory, platformRule, time.Now()))
		if read.Checked && named == nil { // read once a read, and only for an image that checked
			n, err := a.pipe.NamedRunImage(ctx)
			if err != nil {
				return err
			}
			named = &n
		}
		switch {
		case !read.Checked, named.Unchecked(read.Findings):
			state.State = "could_not_rescan"
		case named.Waits(read.Findings):
			state.State = "wait"
		}
		m["patch"] = state
	}
	return nil
}

// patchFactsOne is one app's facts, for a caller that already holds its envs; the zero facts when it has none.
func (a *app) patchFactsOne(ctx context.Context, envs map[string]any, project string) (patchFacts, error) {
	facts, err := a.sayPatch(ctx, a.pool, map[string]map[string]any{project: envs})
	return facts[project], err
}
