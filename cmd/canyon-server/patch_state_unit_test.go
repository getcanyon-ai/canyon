package main

import (
	"strings"
	"testing"
	"time"
)

// Each environment's patch state as the reads derive it from what Canyon recorded (contracts/routes.md,
// envs.<env>.patch, How each is read): none while Canyon does not patch the app, and each state of preview's and
// production's, a held one with the why its patch run's end or the hold, pause or preview gives it. wait and
// could_not_rescan are the status read's alone (TestStatusReadsWaitAndCouldNotRescan).
func TestEnvPatchStates(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 10, 29, 10, m, 0, 0, time.UTC) }
	ptr := func(t time.Time) *time.Time { return &t }
	const old, patched, other = "canyon/claims-portal@sha256:aa", "canyon/claims-portal@sha256:bb", "canyon/claims-portal@sha256:cc"
	ident := patchIdentity
	plan := func(outcome string) *planFact {
		return &planFact{identity: ident, outcome: outcome, run: "gr_patch", image: old, at: at(5)}
	}
	unpatched := &rowFact{image: old, gateRun: "gr_seed", deployed: at(0)}
	patchedPreview := &rowFact{image: patched, gateRun: "gr_patch", deployed: at(2), patchRun: true, identity: ident, outcome: "passed", outcomeAt: ptr(at(17)), replaced: old}
	prodOld := &rowFact{image: old, gateRun: "gr_seed", deployed: at(1)}
	failed := func(state, failure, notPlatform string) *planFact {
		p := plan("failed")
		p.runState, p.failure, p.notPlatform = state, failure, notPlatform
		return p
	}
	for _, c := range []struct {
		name string
		env  string
		f    patchFacts
		want envPatch
	}{
		{"patching off reads none", "preview", patchFacts{on: false, plan: plan("planned"), preview: unpatched}, envPatch{State: "none", Since: "2026-10-29T10:00:00Z"}},
		{"nothing recorded reads none", "preview", patchFacts{on: true, preview: unpatched}, envPatch{State: "none", Since: "2026-10-29T10:00:00Z"}},
		{"a plan is planned", "preview", patchFacts{on: true, plan: plan("planned"), preview: unpatched}, envPatch{State: "planned", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch"}},
		{"a plan of an image preview no longer runs is none", "preview", patchFacts{on: true, plan: plan("planned"), preview: &rowFact{image: other, gateRun: "gr_new", deployed: at(9)}},
			envPatch{State: "none", Since: "2026-10-29T10:09:00Z"}},
		{"a paused identity holds it", "preview", patchFacts{on: true, plan: func() *planFact { p := plan("planned"); p.paused = ptr(at(7)); return p }(), preview: unpatched},
			envPatch{State: "held", Since: "2026-10-29T10:07:00Z", Identity: ident, Run: "gr_patch", Why: "paused"}},
		{"a blocked gate holds it", "preview", patchFacts{on: true, plan: failed("blocked", "", ""), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "blocked"}},
		{"changed layers hold it", "preview", patchFacts{on: true, plan: failed("failed", "not_platform_only", "layers_changed"), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "layers_changed"}},
		{"a changed config holds it", "preview", patchFacts{on: true, plan: failed("failed", "not_platform_only", "config_changed"), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "config_changed"}},
		{"a regression holds it", "preview", patchFacts{on: true, plan: failed("failed", "patch_regressed", ""), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "patch_regressed"}},
		{"an ineffective patch holds it", "preview", patchFacts{on: true, plan: failed("failed", "patch_ineffective", ""), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "patch_ineffective"}},
		{"any other end holds it failed", "preview", patchFacts{on: true, plan: failed("failed", "error", ""), preview: unpatched}, envPatch{State: "held", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch", Why: "failed"}},
		{"a refused rebase is none", "preview", patchFacts{on: true, plan: plan("refused"), preview: unpatched}, envPatch{State: "none", Since: "2026-10-29T10:00:00Z"}},
		{"a revert in preview is reverted", "preview", patchFacts{on: true, plan: plan("reverted"), preview: unpatched}, envPatch{State: "reverted", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_patch"}},
		{"preview running the patch is patched_preview", "preview", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview},
			envPatch{State: "patched_preview", Since: "2026-10-29T10:17:00Z", Identity: ident, Run: "gr_patch"}},
		{"production reads none before it is asked", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld}, envPatch{State: "none", Since: "2026-10-29T10:01:00Z"}},
		{"an open patch approval is asked", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld,
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", state: "pending", open: true, requested: at(18)}},
			envPatch{State: "asked", Since: "2026-10-29T10:18:00Z", Identity: ident, Run: "gr_patch", Approval: "K7QF-2M9X"}},
		{"an approval no longer current is not asked", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld,
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", state: "pending", requested: at(18)}}, envPatch{State: "none", Since: "2026-10-29T10:01:00Z"}},
		{"an approved patch going to production is planned", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld,
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", state: "approved", promoting: true, requested: at(18), decided: ptr(at(20))}},
			envPatch{State: "planned", Since: "2026-10-29T10:20:00Z", Identity: ident, Run: "gr_patch"}},
		{"production held holds it", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld, held: ptr(at(19))},
			envPatch{State: "held", Since: "2026-10-29T10:19:00Z", Identity: ident, Run: "gr_patch", Why: "production_held"}},
		{"preview ahead holds it", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: &rowFact{image: other, gateRun: "gr_older", deployed: at(1)}},
			envPatch{State: "held", Since: "2026-10-29T10:01:00Z", Identity: ident, Run: "gr_patch", Why: "preview_ahead"}},
		{"a paused identity holds production", "prod", patchFacts{on: true, plan: plan("passed"), preview: func() *rowFact { r := *patchedPreview; r.paused = ptr(at(21)); return &r }(), prod: prodOld},
			envPatch{State: "held", Since: "2026-10-29T10:21:00Z", Identity: ident, Run: "gr_patch", Why: "paused"}},
		{"production running the patch is patched", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: &rowFact{image: patched, gateRun: "gr_patch", deployed: at(25), patched: true, madeBy: "gr_patch"}},
			envPatch{State: "patched", Since: "2026-10-29T10:25:00Z", Run: "gr_patch"}},
		{"a rollback to a patched image is patched", "prod", patchFacts{on: true, preview: unpatched, prod: &rowFact{image: patched, gateRun: "gr_rollback", deployed: at(40), patched: true, madeBy: "gr_patch"}},
			envPatch{State: "patched", Since: "2026-10-29T10:40:00Z", Run: "gr_patch"}},
		{"a revert in production is reverted, its run the promote run that watched production", "prod", patchFacts{on: true, plan: plan("reverted"), preview: patchedPreview, prod: prodOld,
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", promoteRun: "gr_promote", state: "approved", requested: at(18), decided: ptr(at(20))}},
			envPatch{State: "reverted", Since: "2026-10-29T10:05:00Z", Identity: ident, Run: "gr_promote"}},
		{"a revert in production no approval names reads no run", "prod", patchFacts{on: true, plan: plan("reverted"), preview: patchedPreview, prod: prodOld},
			envPatch{State: "reverted", Since: "2026-10-29T10:05:00Z", Identity: ident}},
		// #2601: its team denied the patch, so production keeps what it fixes, and Canyon asks no more for the identity.
		{"a denied patch approval is denied while production runs the image it would replace", "prod", patchFacts{on: true, plan: plan("passed"), preview: patchedPreview, prod: prodOld,
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", replaces: old, state: "denied", requested: at(18), decided: ptr(at(22))}},
			envPatch{State: "denied", Since: "2026-10-29T10:22:00Z", Identity: ident, Run: "gr_patch", Approval: "K7QF-2M9X"}},
		{"a denial production has moved past is none", "prod", patchFacts{on: true, preview: &rowFact{image: other, gateRun: "gr_new", deployed: at(30)}, prod: &rowFact{image: other, gateRun: "gr_new", deployed: at(31)},
			approval: &approvalFact{code: "K7QF2M9X", identity: ident, patchRun: "gr_patch", replaces: old, state: "denied", requested: at(18), decided: ptr(at(22))}},
			envPatch{State: "none", Since: "2026-10-29T10:31:00Z"}},
		{"preview still runs the patch production reverted", "preview", patchFacts{on: true, plan: plan("reverted"), preview: patchedPreview, prod: prodOld},
			envPatch{State: "patched_preview", Since: "2026-10-29T10:17:00Z", Identity: ident, Run: "gr_patch"}},
	} {
		got := previewPatch(c.f)
		if c.env == "prod" {
			got = prodPatch(c.f)
		}
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		patchShape(t, "env_patch", got)
	}
}

// patchShape checks body against one definition of contracts/schema/patch.schema.json.
func patchShape(t *testing.T, definition string, body any) {
	t.Helper()
	if err := shapeOf("patch", definition, string(mustJSON(body))); err != nil {
		t.Fatalf("%s is not the contract's: %v\n%s", definition, err, mustJSON(body))
	}
}

// patchIdentity is the platform identity the patches here roll out in.
var patchIdentity = "sha256:" + strings.Repeat("5f", 32)
