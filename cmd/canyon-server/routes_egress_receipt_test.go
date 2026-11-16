package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	egressPolicyPath = "/api/v1/policies/" + egressRule
	claimsStatus     = "/api/v1/projects/claims-portal/status"
)

// seenRow is a row of app's env, last seen ago, an interval.
func seenRow(app, env, destination, ago string) string {
	return fmt.Sprintf(`INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		VALUES ('%s', '%s', '%s', 443, now() - interval '%s', now() - interval '%s', 1, 'allowed', '2xx', 'connect', '{}')`, app, env, destination, ago, ago)
}

// receiverUp is the heartbeat a flush writes: the receiver isn't running under test, so a test beats before it reads.
const receiverUp = "INSERT INTO egress_receiver (seen_at) VALUES (now()) ON CONFLICT (one) DO UPDATE SET seen_at = EXCLUDED.seen_at"

// wentLiveInPreview is what a deploy of the image preview runs leaves behind it, its deploy.completed, under run at ts.
func wentLiveInPreview(project, run, ts string) string {
	return fmt.Sprintf(`INSERT INTO timeline_rows (ts, scope, run_id, project_id, kind, dedupe, data)
		SELECT %s, 'run', '%s', d.project_id, 'deploy.completed', 'deploy:preview', jsonb_build_object('env', 'preview', 'image_digest', split_part(d.image, '@', 2))
		FROM deploys d WHERE d.project_id = '%s' AND d.env = 'preview'`, ts, run, project)
}

// egressRuleOn is the outbound host with preview running through Canyon and the receiver up, production under an
// allowlist holding api.stripe.com, and the rule blocking.
func egressRuleOn(t *testing.T) outbound {
	t.Helper()
	h := outboundHost(t)
	fake.Start("claims-portal", "preview")
	h.exec(t, receiverUp, "UPDATE deploys SET run_config_at = deployed_at WHERE project_id = 'claims-portal'", wentLiveInPreview("claims-portal", "gr_seed", "d.deployed_at"),
		"INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		"INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com')")
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+egressPolicyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	return h
}

// row is the rule's row of the receipt c reads at url, a flush having just beaten, and the receipt.
func (h outbound) row(t *testing.T, c *http.Client, url string) (map[string]any, map[string]any) {
	t.Helper()
	h.exec(t, receiverUp)
	return egressRowOf(t, c, url)
}

// egressRowOf is the rule's row of the receipt a status read answers, and the receipt.
func egressRowOf(t *testing.T, c *http.Client, url string) (map[string]any, map[string]any) {
	t.Helper()
	code, status, _ := do(t, c, http.MethodGet, url, "")
	if code != 200 {
		t.Fatalf("the status read: %d %s", code, lastBody)
	}
	receipt, _ := status["receipt"].(map[string]any)
	rules, _ := receipt["rules"].([]any)
	for _, r := range rules {
		if row, _ := r.(map[string]any); row["id"] == egressRule {
			return row, receipt
		}
	}
	t.Fatalf("no row of %s on the receipt: %s", egressRule, lastBody)
	return nil, nil
}

// The path every Docker app takes when it first turns on egress: preview went live under off, an admin sets canyon and
// recreates its network. What it reached before is on no row, and no row is no evidence, so the row can't pass until it
// deploys again. Then a destination it connected to that production doesn't allow refuses the promotion, naming it on
// the status read and in the refusal alike; allowing it, a person's in the dashboard, clears the receipt and the
// promotion goes through.
func TestPromoteReceiptNamesTheDestination(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	beat := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, receiverUp); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/egress", `{"via":"canyon"}`); code != 200 {
		t.Fatalf("via canyon: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/egress/networks/recreate", `{"app":"claims-portal"}`); code != 200 {
		t.Fatalf("recreating the network: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+egressPolicyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	beat()
	if row, _ := egressRowOf(t, c, base+claimsStatus); row["why"] != egressNoRecord || row["passed"] != false {
		t.Fatalf("recreated onto Canyon after it went live: %s", lastBody)
	}
	cleanGate(t)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	deploy, _ := runOf(started)["id"].(string)
	if waitVerdict(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("deploying again: %s", lastBody)
	}
	beat()
	if row, _ := egressRowOf(t, c, base+claimsStatus); row["passed"] != true {
		t.Fatalf("deployed again through Canyon, nothing recorded: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, seenRow("claims-portal", "preview", "api.openai.com", "1 second")); err != nil {
		t.Fatal(err)
	}
	const line = "production does not allow api.openai.com"
	if row, receipt := egressRowOf(t, c, base+claimsStatus); row["why"] != line || row["passed"] != false || row["blocks"] != true || !slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
		t.Fatalf("the status read's receipt: %s", lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+promotions, `{"gate_run":"`+deploy+`"}`)
	if code != 409 || body["error"] != "gate_not_clean" || !strings.Contains(lastBody, line) {
		t.Fatalf("the promotion: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+outboundApp+"/prod/destinations/api.openai.com", ""); code != 204 {
		t.Fatalf("allowing it: %d %s", code, lastBody)
	}
	beat()
	if row, _ := egressRowOf(t, c, base+claimsStatus); row["passed"] != true || !strings.HasPrefix(fmt.Sprint(row["why"]), "production allows everything ") {
		t.Fatalf("once allowed: %s", lastBody)
	}
	beat()
	if got := promoted(t, c, base, deploy); got["state"] != "clean" {
		t.Fatalf("the promotion once allowed: %s", lastBody)
	}
}

// What the row reads is what the promotion decides on: the window opens the margin before go-live on the deploy row
// as written, so a tunnel from before go-live is read, and a row older than the margin is not. A row of production's, or
// of another app, says nothing of preview.
func TestTheRowReadsTheWindowFromTheDeployRow(t *testing.T) {
	h := egressRuleOn(t)
	status := h.base + claimsStatus
	h.exec(t, "UPDATE deploys SET deployed_at = now() - interval '20 seconds', run_config_at = now() - interval '20 seconds' WHERE project_id = 'claims-portal' AND env = 'preview'",
		seenRow("claims-portal", "preview", "old.example.com", "2 minutes"),
		seenRow("claims-portal", "prod", "prod-only.example.com", "1 second"),
		seenRow("ledger", "preview", "ledger-only.example.com", "1 second"),
		seenRow("claims-portal", "preview", "api.stripe.com", "1 second"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Fatalf("a row older than the margin, an allowed one and others' read: %s", lastBody)
	}
	h.exec(t, seenRow("claims-portal", "preview", "tunnel.example.com", "37 seconds"))
	if row, _ := h.row(t, h.olive, status); row["why"] != "production does not allow tunnel.example.com" {
		t.Fatalf("a tunnel opened 17 s before go-live, read 20 s after it: %s", lastBody)
	}
}

// The first go-live is read from the timeline, by the image's digest: the image's oldest deploy of it, never another
// image's, so deploying it again keeps what it connected to and a new image starts clean. Gaps stay on the running
// deployment, whose could-not-check lines say a redeploy clears them. No deploy of the image on record never passes.
func TestARowIsReadFromTheImagesFirstGoLive(t *testing.T) {
	h := egressRuleOn(t)
	status := h.base + claimsStatus
	const other = "canyon/claims-portal@sha256:" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	h.exec(t, "INSERT INTO runs (id, project_id, package_id, state, ended_at) SELECT 'gr_first', 'claims-portal', package_id, 'clean', now() FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'",
		wentLiveInPreview("claims-portal", "gr_first", "now() - interval '3 hours'"),
		"UPDATE deploys SET deployed_at = now() - interval '10 minutes', run_config_at = now() - interval '10 minutes' WHERE project_id = 'claims-portal' AND env = 'preview'",
		seenRow("claims-portal", "preview", "api.openai.com", "1 hour"),
		`INSERT INTO egress_gaps (from_at, to_at, reason, calls) VALUES (now() - interval '2 hours', now() - interval '1 hour', 'unattributed', 1)`)
	if row, _ := h.row(t, h.olive, status); row["why"] != "production does not allow api.openai.com" {
		t.Errorf("the same image deployed again: %s", lastBody)
	}
	h.exec(t, "DELETE FROM egress_destinations")
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Errorf("a gap before the running deployment: %s", lastBody)
	}
	h.exec(t, seenRow("claims-portal", "preview", "api.openai.com", "3 hours 1 minute"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Errorf("the outgoing version's last call, a minute before the image first went live, deployed again since: %s", lastBody)
	}
	h.exec(t, "DELETE FROM egress_destinations")
	h.exec(t, seenRow("claims-portal", "preview", "api.openai.com", "1 hour"),
		"UPDATE deploys SET image = '"+other+"' WHERE project_id = 'claims-portal' AND env = 'preview'")
	fake.StartAs("claims-portal", "preview", other)
	if row, _ := h.row(t, h.olive, status); row["why"] != egressNoRecord || row["passed"] != false {
		t.Errorf("an image with no deploy on record: %s", lastBody)
	}
	h.exec(t, "INSERT INTO runs (id, project_id, package_id, state, ended_at) SELECT 'gr_other', 'claims-portal', package_id, 'clean', now() FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'",
		wentLiveInPreview("claims-portal", "gr_other", "now() - interval '10 minutes'"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Errorf("a new image: %s", lastBody)
	}
}

// Each gap is read as §3.4 is written: one in the window that names no app holds, and preview's own over_cap; one that
// ended before the window, another app's and production's hold nothing. The window is the rows' own.
func TestTheRowReadsTheGapsInTheWindow(t *testing.T) {
	h := egressRuleOn(t)
	status := h.base + claimsStatus
	h.exec(t, "UPDATE deploys SET deployed_at = now() - interval '1 hour', run_config_at = now() - interval '1 hour' WHERE project_id = 'claims-portal' AND env = 'preview'")
	gap := func(to, reason, app, env string) string {
		return fmt.Sprintf(`INSERT INTO egress_gaps (from_at, to_at, reason, calls, app, env) VALUES (now() - interval '2 days', now() - interval '%s', '%s', 1, %s, %s)`, to, reason, app, env)
	}
	h.exec(t, gap("2 hours", "unattributed", "NULL", "NULL"),
		gap("1 minute", "over_cap", "'ledger'", "'preview'"),
		gap("1 minute", "over_cap", "'claims-portal'", "'prod'"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Errorf("an older gap, another app's and production's: %s", lastBody)
	}
	h.exec(t, gap("59 minutes", "envoy_dropped", "NULL", "NULL"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != false || row["why"] != egressNoRecord {
		t.Errorf("a gap that names no app: %s", lastBody)
	}
	h.exec(t, "DELETE FROM egress_gaps WHERE reason = 'envoy_dropped'", gap("61 minutes", "dropped", "NULL", "NULL"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != false {
		t.Errorf("a gap ending in the margin before go-live: %s", lastBody)
	}
	h.exec(t, "DELETE FROM egress_gaps WHERE reason = 'dropped'", gap("1 minute", "over_cap", "'claims-portal'", "'preview'"))
	if row, _ := h.row(t, h.olive, status); row["passed"] != false || !strings.Contains(fmt.Sprint(row["why"]), "over 500 destinations") {
		t.Errorf("preview's over_cap: %s", lastBody)
	}
}

// With no record of preview's calls there is nothing to compare, and no row is no evidence: a preview not through
// Canyon or on an open network, and a receiver that stopped, each never pass. Where a deploy to preview can't help, the
// line gives the Outbound card's fix (#2723): the app's old network to recreate, or the host's via off.
func TestTheRowNeverPassesWithNothingToRead(t *testing.T) {
	h := egressRuleOn(t)
	status := h.base + claimsStatus
	if row, _ := h.row(t, h.olive, status); row["passed"] != true {
		t.Fatalf("the host as it stands: %s", lastBody)
	}
	for _, c := range []struct {
		name, edit, undo, want string
	}{
		{"under via off", `UPDATE deploys SET egress = '{"via": "off"}' WHERE env = 'preview'`, `UPDATE deploys SET egress = '{"via": "canyon"}' WHERE env = 'preview'`, egressNoRecord},
		{"on an open network, the host's via off", "UPDATE app_networks SET internal = false", "UPDATE app_networks SET internal = true", egressDirect},
		{"on an open network, the host's via canyon", "UPDATE app_networks SET internal = false; UPDATE egress_settings SET via = 'canyon'",
			"UPDATE app_networks SET internal = true; UPDATE egress_settings SET via = 'off'", egressOpenNetwork},
		{"recreated after it went live", "UPDATE deploys SET run_config_at = deployed_at + interval '1 minute' WHERE env = 'preview'", "UPDATE deploys SET run_config_at = deployed_at WHERE env = 'preview'", egressNoRecord},
		{"the receiver down", "UPDATE egress_receiver SET seen_at = now() - interval '16 seconds'", receiverUp, egressCantRead},
		{"the receiver's clock ahead", "UPDATE egress_receiver SET seen_at = now() + interval '1 hour'", receiverUp, egressCantRead},
		{"the receiver never up", "DELETE FROM egress_receiver", receiverUp, egressCantRead},
	} {
		h.exec(t, c.edit)
		if row, _ := egressRowOf(t, h.olive, status); row["passed"] != false || row["why"] != c.want {
			t.Errorf("%s: %s", c.name, lastBody)
		}
		h.exec(t, c.undo)
	}
}

// orgPreview is the host with preview's row the org's, and the org's run_by setting at setting.
func (h outbound) orgPreview(t *testing.T, setting string) {
	t.Helper()
	h.exec(t, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'", "UPDATE org_preferences SET run_by = '"+setting+"'")
}

// Preview's row and the org's setting both say org: the org's platform runs preview and a promotion goes to it too, so
// the row is neutral at either state and holds nothing. It is on the receipt, where a person sees it.
func TestAPreviewTheOrgRunsIsNeutralWhileAPromotionGoesToTheOrg(t *testing.T) {
	h := egressRuleOn(t)
	h.orgPreview(t, "org")
	for _, state := range []string{"blocking", "advisory"} {
		if code, _, _ := do(t, h.dana, http.MethodPut, h.base+egressPolicyPath+"?apply=true", `{"state":"`+state+`"}`); code != 200 {
			t.Fatalf("setting the rule %s: %d %s", state, code, lastBody)
		}
		row, receipt := h.row(t, h.olive, h.base+claimsStatus)
		if row["why"] != egressOrgRuns || row["unset"] != true || row["passed"] != false || row["blocks"] != (state == "blocking") {
			t.Errorf("at %s the row: %s", state, lastBody)
		}
		if slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
			t.Errorf("at %s a neutral row holds the promotion: %s", state, lastBody)
		}
	}
}

// A switch back to canyon leaves preview's row the org's, and the promotion starts the image on Canyon, behind
// production's allowlist, with nothing read of what it connects to. So the row blocks, the refusal says what clears it,
// and it is never neutral (design/HAND-OFF.md §1, Switching).
func TestAPreviewTheOrgRunsBlocksOnceAPromotionWouldBeCanyons(t *testing.T) {
	h := egressRuleOn(t)
	h.orgPreview(t, "canyon")
	row, receipt := h.row(t, h.olive, h.base+claimsStatus)
	if row["why"] != egressNoRecord || row["unset"] != nil || row["passed"] != false || row["blocks"] != true {
		t.Fatalf("the row: %s", lastBody)
	}
	if !slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
		t.Fatalf("the row holds nothing: %s", lastBody)
	}
}

// Fail closed: a run_by or a setting that reads as neither value blocks and is never neutral. The columns are NOT NULL
// and CHECKed, so each test here takes that away first, as a value a later release wrote would read.
func TestARunByThatDoesNotReadBlocks(t *testing.T) {
	for _, c := range []struct {
		name string
		edit []string
	}{
		{"preview's row, a value this build does not know", []string{"ALTER TABLE deploys DROP CONSTRAINT deploys_run_by_check", "UPDATE org_preferences SET run_by = 'org'", "UPDATE deploys SET run_by = 'elsewhere' WHERE project_id = 'claims-portal' AND env = 'preview'"}},
		{"the setting, a value this build does not know", []string{"ALTER TABLE org_preferences DROP CONSTRAINT org_preferences_run_by_check", "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'", "UPDATE org_preferences SET run_by = 'elsewhere'"}},
		{"the setting, nothing there", []string{"ALTER TABLE org_preferences ALTER COLUMN run_by DROP NOT NULL", "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'", "UPDATE org_preferences SET run_by = NULL"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := egressRuleOn(t)
			h.exec(t, c.edit...)
			row, receipt := h.row(t, h.olive, h.base+claimsStatus)
			if row["why"] != egressCantRead || row["unset"] != nil || row["passed"] != false {
				t.Errorf("the row: %s", lastBody)
			}
			if !slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
				t.Errorf("the row holds nothing: %s", lastBody)
			}
		})
	}
}

// A rollback reads no preview, so who runs preview changes nothing of its row: it passes on its one line, and is no
// neutral row, with preview's row and the setting both the org's.
func TestARollbacksRowIsTheSameWhoeverRunsPreview(t *testing.T) {
	pool, ctx, base, c, first, a, _, _ := twiceInProduction(t)
	if code, _, _ := do(t, c, http.MethodPut, base+egressPolicyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`","rollback_to":"`+strings.TrimPrefix(a, "canyon/claims-portal@")+`","reason":"INC-2231"}`)
	review, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, review)["state"] != "clean" {
		t.Fatalf("the rollback review: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'", "UPDATE org_preferences SET run_by = 'org'")
	row, receipt := egressRowOf(t, c, base+claimsStatus+"?run="+review)
	if row["why"] != egressRollback || row["passed"] != true || row["blocks"] != false || row["unset"] != nil || slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
		t.Errorf("the rollback's receipt: %s", lastBody)
	}
}

// A production the org runs has no Allow on its Outbound card, so a rollback's row lists nothing a person dismissed there
// and passes on its plain line. The line stops nothing, so a value that doesn't read leaves it as it was.
func TestARollbackListsNoDismissalsOnAProductionTheOrgRuns(t *testing.T) {
	pool, ctx, base, c, first, a, _, _ := twiceInProduction(t)
	if code, _, _ := do(t, c, http.MethodPut, base+egressPolicyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	runSQL(t, ctx, pool, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES ('claims-portal', 'prod', 'hooks.slack.com', 'dismissed', now(), now(), 3)`)
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`","rollback_to":"`+strings.TrimPrefix(a, "canyon/claims-portal@")+`","reason":"INC-2231"}`)
	review, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, review)["state"] != "clean" {
		t.Fatalf("the rollback review: %d %s", code, lastBody)
	}
	const line = "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card."
	reads := func(name, why string, passed bool) {
		t.Helper()
		row, receipt := egressRowOf(t, c, base+claimsStatus+"?run="+review)
		if row["why"] != why || row["passed"] != passed || row["blocks"] != false || slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
			t.Errorf("%s: %s", name, lastBody)
		}
	}
	reads("Canyon's production", line, false)
	runSQL(t, ctx, pool, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'prod'")
	reads("the org's production", egressRollback, true)
	runSQL(t, ctx, pool, "ALTER TABLE deploys DROP CONSTRAINT deploys_run_by_check", "UPDATE deploys SET run_by = 'elsewhere' WHERE project_id = 'claims-portal' AND env = 'prod'")
	reads("a value this build does not know", line, false)
}

// Off, nothing is read and nothing holds: no row, whatever preview connected to. An app's own blocking addition
// reads as the org's does.
func TestTheRowFollowsTheAppsEffectiveState(t *testing.T) {
	h := egressRuleOn(t)
	status := h.base + claimsStatus
	h.exec(t, receiverUp, seenRow("claims-portal", "preview", "api.openai.com", "1 second"))
	if code, _, _ := do(t, h.dana, http.MethodPut, h.base+egressPolicyPath+"?apply=true", `{"state":"off"}`); code != 200 {
		t.Fatalf("turning the rule off: %d %s", code, lastBody)
	}
	if _, _, _ = do(t, h.olive, http.MethodGet, status, ""); strings.Contains(lastBody, egressRule) {
		t.Fatalf("off, yet on the receipt: %s", lastBody)
	}
	holdEgress(t, h.ctx, h.pool, "claims-portal", "blocking")
	if row, _ := h.row(t, h.olive, status); row["why"] != "production does not allow api.openai.com" || row["blocks"] != true {
		t.Fatalf("the app's own addition: %s", lastBody)
	}
}

// Outbound is a contributor's and up (design/PERMISSIONS.md, Outbound): the row names the destination to one, and to
// a responder with no grant says the same without it.
func TestAResponderReadsNoDestinationOnTheStatusRead(t *testing.T) {
	h := egressRuleOn(t)
	h.exec(t, "UPDATE org_preferences SET responder_emails = ARRAY['"+responderEmail+"']", seenRow("claims-portal", "preview", "api.openai.com", "1 second"))
	rhea := signedIn(t, h.ctx, h.pool, h.base, "sub-rhea", responderEmail, false)
	if row, _ := h.row(t, h.carl, h.base+claimsStatus); row["why"] != "production does not allow api.openai.com" {
		t.Errorf("a contributor reads: %s", lastBody)
	}
	if row, _ := h.row(t, rhea, h.base+claimsStatus); row["why"] != egressNoNames || strings.Contains(lastBody, "api.openai.com") {
		t.Errorf("a responder with no grant reads: %s", lastBody)
	}
}

// A rollback reads no preview, so the rule at blocking refuses none: not one with a destination in preview production
// doesn't allow, and not one of an app offline in both environments, which Guardrails' preview names nowhere. Its row
// lists what a person dismissed that production refuses today, and nothing production has since allowed.
func TestARollbackPassesTheRuleAndListsDismissals(t *testing.T) {
	pool, ctx, base, c, first, a, _, _ := twiceInProduction(t)
	if code, _, _ := do(t, c, http.MethodPut, base+egressPolicyPath+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	for _, q := range []string{
		"INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		"INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
			('claims-portal', 'prod', 'hooks.slack.com', 'dismissed', now(), now(), 3),
			('claims-portal', 'prod', 'api.stripe.com', 'dismissed', now(), now(), 1),
			('claims-portal', 'prod', 'api.openai.com', 'pending', now(), now(), 1),
			('claims-portal', 'preview', 'preview-only.example.com', 'dismissed', now(), now(), 1)`,
		seenRow("claims-portal", "preview", "api.openai.com", "1 second"),
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+first+`","rollback_to":"`+strings.TrimPrefix(a, "canyon/claims-portal@")+`","reason":"INC-2231"}`)
	review, _ := runOf(started)["id"].(string)
	if code != 202 || waitVerdict(t, c, base, review)["state"] != "clean" {
		t.Fatalf("the rollback review: %d %s", code, lastBody)
	}
	want := "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card."
	if row, receipt := egressRowOf(t, c, base+claimsStatus+"?run="+review); row["why"] != want || row["passed"] != false || row["blocks"] != false || len(receipt["blocked_by"].([]any)) != 0 {
		t.Fatalf("the rollback's receipt: %s", lastBody)
	}
	if promoted(t, c, base, review)["state"] != "clean" {
		t.Fatalf("the rollback: %s", lastBody)
	}
	for _, env := range []string{"preview", "prod"} {
		if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/envs/"+env, ""); code != 204 {
			t.Fatalf("undeploying %s: %d %s", env, code, lastBody)
		}
	}
	if got := wouldHold(t, c, base+egressPolicyPath, "blocking"); got != "" {
		t.Errorf("an app offline in both environments with production history is held: %q", got)
	}
	if row, receipt := egressRowOf(t, c, base+claimsStatus+"?run="+review); row["blocks"] != false || slices.Contains(receipt["blocked_by"].([]any), any(egressRule)) {
		t.Errorf("its rollback review's row: %s", lastBody)
	}
}

// rowNeeds is the kinds of the needs claims-portal's fleet row carries for c, each with its env, in order, and whether
// needs_you's queue holds it; nil and false where the row carries no needs.
func rowNeeds(t *testing.T, c *http.Client, base string) ([]string, bool) {
	t.Helper()
	code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet?limit=200", "")
	if code != 200 {
		t.Fatalf("the fleet: %d %s", code, lastBody)
	}
	var kinds []string
	for _, row := range body["apps"].([]any) {
		app := row.(map[string]any)
		if app["id"] != "claims-portal" {
			continue
		}
		needs, _ := app["needs"].([]any)
		for _, n := range needs {
			need := n.(map[string]any)
			kinds = append(kinds, fmt.Sprintf("%v %v", need["kind"], need["env"]))
		}
	}
	return kinds, slices.Contains(fleetIDs(t, c, base+"/api/v1/fleet?needs_you=true"), "claims-portal")
}

// A pending egress request its env refuses is a refused need on the app's row, which puts it in the queue, for its
// contributors, owners and admins, never below (design/PERMISSIONS.md, Outbound); dismissed, the need goes.
func TestFleetNeedsYouCountsPendingEgressRequests(t *testing.T) {
	h := outboundHost(t)
	people := map[string]*http.Client{"an owner": h.olive, "a contributor": h.carl, "an admin": h.dana}
	refused := func(c *http.Client) (bool, bool) {
		t.Helper()
		kinds, queued := rowNeeds(t, c, h.base)
		return slices.Contains(kinds, "refused prod"), queued
	}
	if need, _ := refused(h.carl); need {
		t.Fatalf("a refused need with no request: %s", lastBody)
	}
	h.exec(t, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES ('claims-portal', 'prod', 'api.openai.com', 'pending', now(), now(), 1)`)
	for who, c := range people {
		if need, queued := refused(c); !need || !queued {
			t.Errorf("%s: need %t, queued %t, with an egress request pending; want both", who, need, queued)
		}
	}
	for who, c := range map[string]*http.Client{"a read-only member": h.vic, "a member with no access": h.nina} {
		if need, queued := refused(c); need || queued {
			t.Errorf("%s: need %t, queued %t; want neither below contributor", who, need, queued)
		}
	}
	if code, _, _ := do(t, h.carl, http.MethodPost, h.base+outboundApp+"/prod/requests/api.openai.com/dismiss", ""); code != 204 {
		t.Fatalf("dismissing it: %d %s", code, lastBody)
	}
	if need, _ := refused(h.carl); need {
		t.Error("dismissed, the row still carries a refused need")
	}
}

// The refused tile counts an app with an egress request pending, as needs_you picks it, so the fleet's Needs you names
// it (#2723): for the app's contributors, owners and admins, never below (design/PERMISSIONS.md, Outbound), and its
// filter selects exactly the apps it counts. A dismissed request counts nothing.
func TestTheRefusedTileCountsPendingEgressRequests(t *testing.T) {
	h := outboundHost(t)
	refused := func(c *http.Client) (float64, []string) {
		t.Helper()
		code, body, _ := do(t, c, http.MethodGet, h.base+"/api/v1/fleet", "")
		if code != 200 {
			t.Fatalf("the fleet: %d %s", code, lastBody)
		}
		n, _ := body["tiles"].(map[string]any)["refused"].(float64)
		return n, fleetIDs(t, c, h.base+"/api/v1/fleet?tile=refused")
	}
	if n, ids := refused(h.carl); n != 0 || len(ids) != 0 {
		t.Fatalf("with no request: %v %v", n, ids)
	}
	h.exec(t, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist'), ('claims-portal', 'preview', 'allowlist')",
		`INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals) VALUES
		('claims-portal', 'prod', 'api.openai.com', 'pending', now(), now(), 4),
		('claims-portal', 'preview', 'api.openai.com', 'pending', now(), now(), 2)`)
	for who, c := range map[string]*http.Client{"an owner": h.olive, "a contributor": h.carl, "an admin": h.dana} {
		if n, ids := refused(c); n != 1 || !slices.Equal(ids, []string{"claims-portal"}) {
			t.Errorf("%s: the refused tile %v, its filter %v; want 1 and claims-portal, once for both envs", who, n, ids)
		}
	}
	for who, c := range map[string]*http.Client{"a read-only member": h.vic, "a member with no access": h.nina} {
		if n, ids := refused(c); n != 0 || len(ids) != 0 {
			t.Errorf("%s: the refused tile %v, its filter %v; want nothing below contributor", who, n, ids)
		}
	}
	for _, env := range []string{"prod", "preview"} {
		if code, _, _ := do(t, h.carl, http.MethodPost, h.base+outboundApp+"/"+env+"/requests/api.openai.com/dismiss", ""); code != 204 {
			t.Fatalf("dismissing it in %s: %d %s", env, code, lastBody)
		}
	}
	if n, ids := refused(h.carl); n != 0 || len(ids) != 0 {
		t.Errorf("dismissed, the tile %v and its filter %v still count it", n, ids)
	}
}

// need is whether production's one pending request is a need, as the refused tile, its filter, the app's fleet row
// and the app's egress read's refusing each say: needRefused or needNone.
func (h outbound) need(t *testing.T) string {
	t.Helper()
	code, body, _ := do(t, h.carl, http.MethodGet, h.base+"/api/v1/fleet", "")
	if code != 200 {
		t.Fatalf("the fleet: %d %s", code, lastBody)
	}
	code, egress, _ := do(t, h.carl, http.MethodGet, h.base+outboundApp, "")
	if code != 200 {
		t.Fatalf("the egress read: %d %s", code, lastBody)
	}
	requests := egress["envs"].(map[string]any)["prod"].(map[string]any)["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("production's requests: %s", lastBody)
	}
	kinds, _ := rowNeeds(t, h.carl, h.base)
	return fmt.Sprintf("tile %v, filter %v, listed %t, refusing %v", body["tiles"].(map[string]any)["refused"], fleetIDs(t, h.carl, h.base+"/api/v1/fleet?tile=refused"),
		slices.Contains(kinds, "refused prod"), requests[0].(map[string]any)["refusing"])
}

const needRefused, needNone = "tile 1, filter [claims-portal], listed true, refusing true", "tile 0, filter [], listed false, refusing false"

// refusedInProd is production under its allowlist with one pending request for destination, refused now.
func refusedInProd(t *testing.T, h outbound, destination string) {
	t.Helper()
	h.exec(t, "INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')")
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals)
		VALUES ('claims-portal', 'prod', $1, 'pending', now(), now(), 3)`, destination); err != nil {
		t.Fatal(err)
	}
}

// A request stays pending after its env stops refusing the name, and is then no need: the refused tile, the app's
// fleet row and the app's egress read's refusing all say so, by one predicate (contracts/routes.md, the refused tile; #2723).
// Each guard alone takes production's one request out: its mode observe, its running deployment not through Canyon, the
// request dismissed, refused only before that deployment went live, or an allowlist entry allowing the name. A call
// the running version made as it started, after the run that started it began less the receipt's margin and before its
// deploy row says it went live, is its own and counts.
func TestARefusalTheEnvNoLongerMakesIsNoNeed(t *testing.T) {
	h := outboundHost(t)
	refusedInProd(t, h, "api.anthropic.com")
	if got := h.need(t); got != needRefused {
		t.Fatalf("refused now: %s, want %s", got, needRefused)
	}
	for _, c := range []struct{ name, edit, undo string }{
		{"under observe", "UPDATE egress_modes SET mode = 'observe' WHERE env = 'prod'", "UPDATE egress_modes SET mode = 'allowlist' WHERE env = 'prod'"},
		{"its deployment not through Canyon", `UPDATE deploys SET egress = '{"via": "off"}' WHERE env = 'prod'`, `UPDATE deploys SET egress = '{"via": "canyon"}' WHERE env = 'prod'`},
		{"dismissed", "UPDATE egress_requests SET state = 'dismissed'", "UPDATE egress_requests SET state = 'pending'"},
		{"refused only before its deployment went live", `UPDATE egress_requests SET (first_refused_at, last_refused_at) =
			(SELECT deployed_at - interval '2 hours', deployed_at - interval '1 hour' FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod')`,
			"UPDATE egress_requests SET (first_refused_at, last_refused_at) = (now(), now())"},
		{"allowed by an entry", "INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.anthropic.com')", "DELETE FROM egress_allowed"},
	} {
		h.exec(t, c.edit)
		if got := h.need(t); got != needNone {
			t.Errorf("%s: %s, want %s", c.name, got, needNone)
		}
		h.exec(t, c.undo)
		if got := h.need(t); got != needRefused {
			t.Fatalf("%s undone: %s, want %s", c.name, got, needRefused)
		}
	}
	h.exec(t, "UPDATE deploys SET run_started_at = deployed_at - interval '3 minutes' WHERE project_id = 'claims-portal' AND env = 'prod'")
	for _, c := range []struct{ name, at, want string }{
		{"refused as the run that started it ran, before its deployment went live", "run_started_at + interval '1 minute'", needRefused},
		{"refused inside the margin before that run started", "run_started_at - interval '1 minute'", needRefused},
		{"refused past the margin before that run started", "run_started_at - interval '2 minutes'", needNone},
	} {
		h.exec(t, `UPDATE egress_requests SET (first_refused_at, last_refused_at) =
			(SELECT `+c.at+`, `+c.at+` FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod')`)
		if got := h.need(t); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// The deploy step started the container minutes into the run, as a preview's review and build take (#2738): the
	// outgoing version's refusals before then, less the margin, are no need of the version that runs now.
	h.exec(t, "UPDATE deploys SET run_started_at = deployed_at - interval '20 minutes', container_started_at = deployed_at - interval '1 minute' WHERE project_id = 'claims-portal' AND env = 'prod'")
	for _, c := range []struct{ name, at, want string }{
		{"refused during the run, before the container started", "container_started_at - interval '10 minutes'", needNone},
		{"refused past the margin before the container started", "container_started_at - interval '2 minutes'", needNone},
		{"refused inside the margin before the container started", "container_started_at - interval '1 minute'", needRefused},
		{"refused once the container started", "container_started_at + interval '30 seconds'", needRefused},
	} {
		h.exec(t, `UPDATE egress_requests SET (first_refused_at, last_refused_at) =
			(SELECT `+c.at+`, `+c.at+` FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod')`)
		if got := h.need(t); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// A call no allowlist can let through, a dotted address, counts from the container's start too (#2738): the
	// outgoing version's, during the run, is no refused_code need of the version that runs now.
	h.exec(t, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags)
		VALUES ('claims-portal', 'prod', '1.1.1.1', 443, now(), now(), 2, 'refused', '4xx', 'connect', '{UAEX}')`)
	for _, c := range []struct {
		name, at string
		want     bool
	}{
		{"an address refused during the run, before the container started", "container_started_at - interval '10 minutes'", false},
		{"an address refused once the container started", "container_started_at + interval '30 seconds'", true},
	} {
		h.exec(t, `UPDATE egress_destinations SET (first_seen_at, last_seen_at) =
			(SELECT `+c.at+`, `+c.at+` FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod') WHERE destination = '1.1.1.1'`)
		if kinds, _ := rowNeeds(t, h.carl, h.base); slices.Contains(kinds, "refused_code prod") != c.want {
			t.Errorf("%s: the row's needs %v, want refused_code %t", c.name, kinds, c.want)
		}
	}
}

// A request for a platform destination, a name the env always reaches on Kubernetes once a front door signs its apps
// in, is no need: the env never refuses it, under any mode or allowlist (contracts/routes.md, the refused tile; #2723).
func TestARequestForAPlatformDestinationIsNoNeed(t *testing.T) {
	onKubernetes(t)
	h := outboundHost(t)
	refusedInProd(t, h, "login.meridian.example")
	if got := h.need(t); got != needRefused {
		t.Fatalf("with no front door: %s, want %s", got, needRefused)
	}
	storeDoor(t, h.ctx, h.pool, "https://login.meridian.example")
	if got := platformRead(t, h); got != "login.meridian.example" {
		t.Fatalf("platform %q, want the front door's issuer", got)
	}
	if got := h.need(t); got != needNone {
		t.Errorf("a platform destination: %s, want %s", got, needNone)
	}
}

// wouldHold is who the preview at path holds at state, each at any count.
func wouldHold(t *testing.T, c *http.Client, path, state string) string {
	t.Helper()
	code, out, _ := do(t, c, http.MethodPut, path, `{"state":"`+state+`"}`)
	if code != 200 {
		t.Fatalf("the preview of %s at %s: %d %s", path, state, code, lastBody)
	}
	rows, _ := out["would_block"].([]any)
	held := make([]string, 0, len(rows))
	for _, row := range rows {
		r, _ := row.(map[string]any)
		held = append(held, fmt.Sprint(r["project"]))
	}
	return strings.Join(held, " ")
}

// throughCanyon gives project a preview the rule can read: on the records, its network internal.
func throughCanyon(t *testing.T, ctx context.Context, pool *pgxpool.Pool, project string) {
	t.Helper()
	for _, q := range []string{
		"UPDATE deploys SET egress = '{\"via\": \"canyon\"}', run_config_at = deployed_at WHERE project_id = $1 AND env = 'preview'",
		"INSERT INTO app_networks (project_id, internal) VALUES ($1, true) ON CONFLICT (project_id) DO UPDATE SET internal = true",
	} {
		if _, err := pool.Exec(ctx, q, project); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, wentLiveInPreview(project, "gr_"+project, "d.deployed_at")); err != nil {
		t.Fatal(err)
	}
}

// Guardrails tells the truth once the rule reads: of the apps it would read, it names only those whose row would hold,
// a destination production refuses counted, could-not-check at none; an app with nothing to answer for is not named.
func TestTheEgressRulePreviewNamesOnlyTheAppsItsRowWouldHold(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, p := range []string{"claims-portal", "clear", "refused", "unread"} {
		seedLive(t, ctx, pool, p, "preview")
	}
	throughCanyon(t, ctx, pool, "clear")
	throughCanyon(t, ctx, pool, "refused")
	throughCanyon(t, ctx, pool, "claims-portal")
	for _, q := range []string{receiverUp,
		"UPDATE egress_settings SET mode = 'allowlist'",
		seenRow("refused", "preview", "api.openai.com", "1 second"), seenRow("refused", "preview", "hooks.slack.com", "1 second"),
		seenRow("clear", "preview", "api.stripe.com", "1 second"),
		"INSERT INTO egress_allowed (project_id, env, entry) VALUES ('clear', 'prod', '*.stripe.com')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+egressPolicyPath, `{"state":"blocking"}`); code != 200 || lastBody != `{"would_block":[{"project":"refused","findings":2},{"project":"unread","findings":0}]}` {
		t.Errorf("at blocking: %d %s", code, lastBody)
	}
}

// Guardrails says what the receipt would: a neutral row holds nothing, so the app isn't named, and the same preview is
// named at none the moment a promotion would be Canyon's.
func TestTheEgressRulePreviewLeavesOutANeutralRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedLive(t, ctx, pool, "claims-portal", "preview")
	runSQL(t, ctx, pool, "UPDATE deploys SET run_by = 'org' WHERE project_id = 'claims-portal' AND env = 'preview'", "UPDATE org_preferences SET run_by = 'org'")
	if got := wouldHold(t, c, base+egressPolicyPath, "blocking"); got != "" {
		t.Errorf("the org's on both sides, yet it holds %q", got)
	}
	runSQL(t, ctx, pool, "UPDATE org_preferences SET run_by = 'canyon'")
	if got := wouldHold(t, c, base+egressPolicyPath, "blocking"); got != "claims-portal" {
		t.Errorf("preview the org's and a promotion Canyon's, yet it holds %q", got)
	}
}

// Only a blocking addition of this rule keeps an app named below blocking: one to another rule says nothing of this one.
func TestAnAdditionToAnotherRuleHoldsNothingInTheEgressPreview(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	seedLive(t, ctx, pool, "claims-portal", "preview")
	if _, err := pool.Exec(ctx, "INSERT INTO app_policy_additions (project_id, rule, state, set_by_kind, set_by_subject) VALUES ('claims-portal', 'deps.known-vuln', 'blocking', 'person', 'sub-olive')"); err != nil {
		t.Fatal(err)
	}
	if got := wouldHold(t, c, base+egressPolicyPath, "advisory"); got != "" {
		t.Errorf("a blocking addition to another rule holds %q", got)
	}
	if got := wouldHold(t, c, base+egressPolicyPath, "blocking"); got != "claims-portal" {
		t.Errorf("at blocking the app reads could not check, yet holds %q", got)
	}
}
