package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
)

// receipt is the promote receipt (contracts/routes.md "The promote receipt"): one shape, at rest
// on the status read and inside the promote refusal, so the receipt on the page and the receipt
// that refused are the same thing.
type receipt struct {
	Org       string        `json:"org"`
	BlockedBy []string      `json:"blocked_by"`
	Rules     []receiptRule `json:"rules"`
	Ships     *ships        `json:"ships,omitempty"`   // a rollback review's, once it recorded the image it ships
	Held      *heldBody     `json:"held,omitempty"`    // production is held: not evaluated, and blocked by prod.held alone (contracts/routes.md, The hold)
	Waiting   string        `json:"waiting,omitempty"` // nothing runs in preview, or the one Canyon runs isn't running: not evaluated, and this is the one line
}

// ships is what a promotion of a rollback review ships (contracts/routes.md, The promote receipt).
type ships struct {
	Version string `json:"version"`
	Digest  string `json:"image_digest"`
}

// waitingOnPreview is the receipt's one line while nothing runs in preview (contracts/routes.md).
const waitingOnPreview = "Deploy a new version to preview to promote it."

type receiptRule struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Blocks bool   `json:"blocks"`
	Passed bool   `json:"passed"`
	Why    string `json:"why"`
	Unset  bool   `json:"unset,omitempty"` // nothing set to check against: neutral, never a failure (routes.md, decision D4)

	noNames string // prod.egress-allowed's line to a responder with no grant, where why names destinations (forResponder)

	// platform.known-vuln's alone, once it read its matches: the oldest database of each layer's newest
	// match, when it read them, the notes under the row, and the packages it counts (routes.md, The promote receipt).
	DBBuiltAt *string           `json:"db_built_at,omitempty"`
	ReadAt    *string           `json:"read_at,omitempty"`
	Notes     *int              `json:"notes,omitempty"`
	Packages  []platformPackage `json:"packages,omitempty"`

	// deps.known-vuln's alone, once it read the built app and found any: each package it counts, then each an
	// approval covers, with the fingerprints an approval names (routes.md, The promote receipt; the approve rows).
	BuiltApp []builtAppPackage `json:"built_app,omitempty"`
}

// builtAppPackage is a package only the built app holds, as deps.known-vuln's row lists it.
type builtAppPackage struct {
	Name         string             `json:"name"`
	Version      string             `json:"version"`
	Advisories   []platformAdvisory `json:"advisories"`
	Fingerprints []string           `json:"fingerprints"`
	Approved     bool               `json:"approved"`
}

// promoteRules is the four rules the receipt folds, in the snapshot's order. Any of the four may
// block or not as the org's edits say: setting prod.clean-gate to advisory lets promotions through.
func promoteRules(s gatepolicy.Snapshot) []gatepolicy.Rule {
	return slices.DeleteFunc(slices.Clone(s.Rules), func(r gatepolicy.Rule) bool { return !slices.Contains(promoteRuleIDs, r.ID) })
}

// pkgRef is a package as a receipt handles it: the id it compares by, the label it reads by.
type pkgRef struct{ id, label string }

// version names a package the way contracts/routes.md's *Naming a version* says — its label when
// it has one, else its id without the pkg_ prefix. The page renders a reason verbatim, so a reason
// never carries a pkg_ id.
func (p pkgRef) version() string {
	if p.label != "" {
		return p.label
	}
	return strings.TrimPrefix(p.id, "pkg_")
}

// runFacts is what the receipt knows about the run it is evaluated for.
type runFacts struct {
	id, state, failureReason string
	failedIn                 string // the step a run Canyon itself failed in, or ""
	pkg                      pkgRef
	newest                   string          // the newest run of pkg in the project, the loader's answer
	findings                 json.RawMessage // its verdict's log, nil before the verdict
	rollback                 bool            // a rollback review (contracts/routes.md, the gate-runs row's rollback_to): it reads no preview
	ships, reason            string          // the image it recorded to ship, "" for none, and the person's reason
	replaces                 *string         // the image production ran as it started, nil for none
	shipped                  bool            // a promotion of it went live: it is put back once
	startedBy                actor           // who started it: the zero actor on a run from before the columns
}

// placedRule is a rule placed before production beyond the go-live four, as the receipt reads it from
// the run it is evaluated for (contracts/routes.md, The promote receipt).
type placedRule struct {
	rule       gatepolicy.Rule
	journaled  bool   // on in the snapshot the run journaled, so its findings exist
	unread     string // reviewed by an agent whose check on the run was skipped, so no findings: why, after the version
	blocking   int    // the run's findings of it that block before production
	unapproved int    // of those, the ones no unexpired approval covers

	// deps.known-vuln's alone: the packages its findings in the run name, and its read of the built app the receipt
	// is for, nil before it is read, which never passes
	reported map[trivy.PackageKey]bool
	app      *builtApp
}

// receiptInput is everything evaluate looks at; receiptFacts and the project read load it. promotionFacts
// adds preview's answer, read under the same lock and refused outside the receipt.
type receiptInput struct {
	org          string
	rules        []gatepolicy.Rule    // the four promote rules, in the snapshot's order, at the states the org's edits leave them
	approved     []string             // the snapshot's approved buildpacks; empty: stack.blessed is unset
	ran          []executor.Buildpack // what preview's image's build ran (the deploys row); nil: none recorded
	run          *runFacts            // nil: no run has looked at this app
	unapproved   int                  // the run's blocking findings with no unexpired approval
	preview      *pkgRef              // the package preview runs; nil: nothing is running there
	previewState string               // envs.preview.state: "" when nobody asked, or the org runs it
	restarts     int                  // envs.preview.restarting.restarts when it reads restarting: 0 when the read carries no count
	owners       []actor              // in owner order: set_at, then subject
	actor        actor                // who is asking or reading
	approvalOn   bool                 // the org has a person approve production in the dashboard (design/AUTH.md §4.5)
	signInOff    bool                 // nobody signs in here, so nobody owns an app: owner.required is neutral (contracts/routes.md)
	dashboard    bool                 // the actor's session is a browser's
	fleet        bool                 // the fleet approval decides, as the admin who gave it in the browser (contracts/routes.md, the Platform rows)
	placed       []placedRule         // the other rules placed before production, in the snapshot's order
	answered     *answered.Answer     // what preview's app answered after its deploy, nil for none: refused outside the receipt, never read by evaluate
	held         *runs.Hold           // production's hold, nil while it is not held
	platform     *imageRead           // platform.known-vuln's read of the image the receipt is for, nil while the rule is off
	egress       *egressRead          // prod.egress-allowed's read of what preview connected to, nil while the rule is off
}

// blessed is stack.blessed: every buildpack preview's image ran is on the approved list, by id, each
// named by its recorded name, else its id (contracts/routes.md, the promote receipt).
func blessed(approved []string, ran []executor.Buildpack) (bool, string) {
	switch {
	case len(approved) == 0:
		return false, "no approved stacks set"
	case len(ran) == 0:
		return false, "no stack recorded"
	}
	var all, not []string
	for _, bp := range ran {
		all = append(all, cmp.Or(bp.Name, bp.ID))
		if !slices.Contains(approved, bp.ID) {
			not = append(not, cmp.Or(bp.Name, bp.ID))
		}
	}
	if len(not) > 0 {
		return false, "not approved: " + strings.Join(not, ", ")
	}
	return true, "built with " + strings.Join(all, ", ")
}

// evaluate folds the four rules into the receipt. Pure: no database, no clock — the loader
// applied the expiry in SQL. blocked_by is the blocking rules that did not pass, in rules order;
// empty means the promotion goes ahead.
func evaluate(in receiptInput) receipt {
	out := receipt{Org: in.org, BlockedBy: []string{}, Rules: []receiptRule{}}
	if in.held != nil { // nothing is promoted while production is held, whatever the rules say, and nothing waits on preview
		out.Held, out.BlockedBy = heldOf(*in.held), []string{"prod.held"}
		return out
	}
	switch { // the receipt answers this preview, never the app's status: an empty one is not a failed check
	case in.run != nil && in.run.rollback: // or the image a rollback review ships: it reads no preview
	case in.preview == nil:
		out.Waiting = waitingOn(in)
		return out
	case in.previewState == "restarting": // nor from one of the up-windows of a preview that keeps restarting, and the line names the way out (#1722)
		times := "" // the read carried no count
		switch {
		case in.restarts == 1:
			times = " once"
		case in.restarts > 1:
			times = fmt.Sprintf(" %d times", in.restarts)
		}
		out.Waiting = in.preview.version() + " restarted" + times + " since it was deployed; deploy again once it's fixed (the build is reused)."
		return out
	case in.previewState == "stopped": // fail closed: nothing is promoted from a preview that isn't running
		out.Waiting = in.preview.version() + " isn't running in preview: canyon logs says why."
		return out
	case in.previewState != "" && in.previewState != "running": // unknown, or a state this server does not know
		out.Waiting = "Canyon couldn't check whether " + in.preview.version() + " runs in preview."
		return out
	}
	for _, r := range in.rules {
		if r.ID == platformRule { // on the receipt while it is on, and read from the image, never a run (routes.md)
			if r.State == gatepolicy.Off {
				continue
			}
			read := imageRead{State: "not_scanned", Why: "no image scan recorded"} // nothing read never passes
			if in.platform != nil {
				read = *in.platform
			}
			row := platformRow(r, read)
			if row.Blocks && !row.Passed {
				out.BlockedBy = append(out.BlockedBy, r.ID)
			}
			out.Rules = append(out.Rules, row)
			continue
		}
		if r.ID == egressRule { // on the receipt while it is on, and a rule nothing read never passes (routes.md)
			if r.State == gatepolicy.Off {
				continue
			}
			version := "" // a rollback's row names none
			if in.preview != nil {
				version = in.preview.version()
			}
			row := egressRow(r, in.egress, version)
			if row.Blocks && !row.Passed && !row.Unset { // a neutral row, as an unset rule below, holds nothing whatever its state
				out.BlockedBy = append(out.BlockedBy, r.ID)
			}
			out.Rules = append(out.Rules, row)
			continue
		}
		var passed, unset bool
		var why string
		switch r.ID {
		case "prod.clean-gate":
			passed, why = cleanGateRule(in)
		case "prod.human-approval": // EXPERIENCE.md: a person clicks promote, solo click OK; the org's prod_approval decides where (design/AUTH.md §4.5)
			passed, why = in.actor.Kind == "person", "a person promotes, or an agent they ask, with their CLI sign-in" // the rule, never who is reading
			if in.approvalOn {
				passed, why = passed && (in.dashboard || in.fleet), "a person approves it in Canyon's dashboard; the CLI only asks"
			}
		case "stack.blessed":
			passed, why = blessed(in.approved, in.ran)
			unset = len(in.approved) == 0
		case "owner.required":
			passed, why = len(in.owners) > 0, ownedBy(in.owners)
			if in.signInOff && len(in.owners) == 0 && r.State == gatepolicy.Advisory { // nobody signs in here to own one: at its default an app nobody claimed fails nothing; turned Blocking, the org asked for an owner (contracts/routes.md)
				passed, why, unset = false, "no owner needed: sign-in is off on this Canyon", true
			}
		}
		blocks := r.State == gatepolicy.Blocking || r.ID == "prod.human-approval" && in.approvalOn // the switch holds whatever the rule's state
		rule := receiptRule{ID: r.ID, Name: r.Name, Blocks: blocks, Passed: passed, Why: why, Unset: unset}
		if rule.Blocks && !rule.Passed && !rule.Unset { // an unset rule is neutral whatever its state: nothing to fail against
			out.BlockedBy = append(out.BlockedBy, r.ID)
		}
		out.Rules = append(out.Rules, rule)
	}
	for _, p := range in.placed {
		passed, why := placedRuleRow(in, p)
		var built []builtAppPackage
		if p.rule.ID == depsRule && passed { // the built app is read once its review's row would pass (routes.md, The promote receipt)
			passed, why = cmp.Or(p.app, &builtApp{why: "no image scan recorded"}).row(in.run.pkg.version(), why)
			built = p.app.listed()
		}
		rule := receiptRule{ID: p.rule.ID, Name: p.rule.Name, Blocks: p.rule.Production == gatepolicy.Blocking, Passed: passed, Why: why, BuiltApp: built}
		if rule.Blocks && !rule.Passed {
			out.BlockedBy = append(out.BlockedBy, rule.ID)
		}
		out.Rules = append(out.Rules, rule)
	}
	if in.run != nil && in.run.ships != "" {
		_, digest, _ := strings.Cut(in.run.ships, "@")
		out.Ships = &ships{Version: in.run.pkg.version(), Digest: digest}
	}
	return out
}

// readsPreview is whether the run is of the package preview runs and that package's newest run, and
// when not, clean-gate's reason: a later run of the package is never hidden behind this one.
func readsPreview(in receiptInput) (bool, string) {
	v := in.run.pkg.version()
	switch {
	case in.run.rollback && in.run.newest != in.run.id: // a rollback review ships the image it recorded, never preview's, while it is its package's newest review
		return false, v + " has a newer review than the one named"
	case in.run.rollback && in.run.shipped: // and once
		return false, v + "'s review has put it back already; review it again"
	case in.run.rollback:
		return true, ""
	case in.run.pkg.id != in.preview.id:
		return false, "the review named is " + v + "'s; preview runs " + in.preview.version()
	case in.run.newest != in.run.id:
		return false, v + " has a newer review than the one named"
	}
	return true, ""
}

// placedRuleRow is fail-closed (contracts/routes.md, The promote receipt): it reads only the run
// clean-gate passes on — ended, and preview's package's newest — else it does not pass and says what
// clean-gate says of the run; a rule that was not on when the run started, or whose agent's check was
// skipped, has no findings to read either; else it passes when every finding of it that blocks before
// production is approved.
func placedRuleRow(in receiptInput, p placedRule) (bool, string) {
	if in.run == nil || in.run.state != "clean" && in.run.state != "blocked" {
		_, why := cleanGateRule(in)
		return false, why
	}
	if ok, why := readsPreview(in); !ok {
		return false, why
	}
	v := in.run.pkg.version()
	switch {
	case !p.journaled:
		return false, v + " was reviewed before this check ran; check it again"
	case p.unread != "":
		return false, v + p.unread
	case p.unapproved > 0:
		return false, fmt.Sprintf("%s's review found %d blocking finding%s, not approved", v, p.unapproved, plural(p.unapproved))
	case p.blocking > 0:
		return true, v + " passed review: every blocking finding approved"
	}
	return true, v + " passed review: no blocking finding"
}

// approvers is who approves a blocking finding, as a blocking finding's fix names them (contracts/finding.md;
// design/PERMISSIONS.md): never its team, so the line never tells a member to approve (#2237).
const approvers = "an admin or a security responder who didn't start this review"

// waitingOn is the receipt's one line while nothing runs in preview: why nothing is there, when the run
// it is read for says, since deploying again would only repeat a block or a failure; else the generic
// line (contracts/routes.md, The promote receipt).
func waitingOn(in receiptInput) string {
	r := in.run
	if r == nil {
		return waitingOnPreview
	}
	v := r.pkg.version()
	switch {
	case r.state == "blocked" && in.unapproved == 1:
		return "Nothing is in preview: " + v + " was blocked at review. Fix the finding, or have " + approvers + " approve it with a reason, then deploy."
	case r.state == "blocked" && in.unapproved > 1:
		return "Nothing is in preview: " + v + " was blocked at review. Fix the findings, or have " + approvers + " approve them with a reason, then deploy."
	case r.state == "failed" && r.failureReason == "build":
		return "Nothing is in preview: the build of " + v + " failed."
	case r.state == "failed" && r.failureReason == "start":
		return "Nothing is in preview: " + v + " didn't stay up after it started."
	case r.state == "failed" && r.failureReason == "detect":
		return "Nothing is in preview: this host can't build " + v + "'s kind of app."
	case r.state == "failed" && r.failureReason == "no_room":
		return "Nothing is in preview: the cluster had no room to build " + v + "."
	case r.state == "failed" && r.failureReason == "scan_no_room":
		return "Nothing is in preview: the cluster had no room to review " + v + ". Deploy it again once it has."
	}
	return waitingOnPreview
}

// cleanGateRule is fail-closed, checked in this order: a failed build has zero findings and must
// never read as clean, so the state precedes the count. A reason names versions, never runs
// (contracts/routes.md, *The promote receipt*): a person reads "v1.4", not "gr_8f2a". Past the run's
// own checks it fails on any rule placed blocking before production whose row fails, pointing at the row,
// which counts the findings, so it never passes above a placed row refusing.
func cleanGateRule(in receiptInput) (bool, string) {
	r := in.run
	if r == nil {
		return false, "no run has looked at this app"
	}
	v := r.pkg.version()
	previews, other := readsPreview(in)
	production, uncounted := placedOpen(in.placed)
	switch {
	case r.state == "pending" || r.state == "running":
		return false, v + "'s review is " + r.state
	case r.state == "failed":
		return false, v + "'s run failed: " + events.FailureWords(r.failureReason, r.failedIn)
	case in.unapproved > 0:
		return false, fmt.Sprintf("%s's review found %d blocking finding%s, not approved", v, in.unapproved, plural(in.unapproved))
	case r.rollback && r.ships == "": // its findings approved only after it ended: nothing was recorded to ship
		return false, v + "'s review recorded no image to put back; review it again"
	case !previews:
		return false, other
	case production > 0, uncounted: // each placed rule's row counts its own findings: counted here too, one reads as two (#2723)
		return false, v + "'s review isn't clean for production. Fix its findings that block production, then deploy to preview again."
	case r.state == "blocked":
		return true, v + " passed review: every blocking finding approved"
	case r.state == "clean":
		return true, v + " passed review: no blocking finding"
	}
	return false, v + "'s review is " + r.state // a state this rule does not know never passes
}

// placedOpen is the placed rules' part of clean-gate's judgement: how many of their findings that block
// before production no approval covers, and whether a placed row fails with no count to read.
func placedOpen(placed []placedRule) (production int, uncounted bool) {
	for _, p := range placed {
		switch {
		case p.rule.Production != gatepolicy.Blocking:
		case !p.journaled || p.unread != "": // its row fails before any count, so none is read from it
			uncounted = true
		default:
			production += p.unapproved
			uncounted = uncounted || p.rule.ID == depsRule && (p.app == nil || !p.app.clean()) // the built app counts nothing an approval covers
		}
	}
	return production, uncounted
}

// clearsReview is the receipt's judgement of review run's findings, read now, under the app's effective
// snapshot now: prod.clean-gate, while it blocks, over every finding blocking before preview, and every
// rule placed blocking before production over its own — the receipt's own facts (receiptFacts, placedRules)
// and counts (placedOpen), each finding covered as runs.CoversBlocking covers it, expiry included. It is
// what the promotions route judged as it started a promotion under that review (ReceiptRun), asked again by
// the promotion's deploy step before production starts (runs.Pipeline.Clears; contracts/routes.md, the
// approve rows). Fail closed, as the receipt: no review, a run not found or one with no verdict clears
// nothing a blocking rule reads.
func (a *app) clearsReview(ctx context.Context, project, run string) (bool, error) {
	snap, err := a.projectPolicy(ctx, a.pool, project)
	var facts *runFacts
	var unapproved int
	if err == nil && run != "" {
		facts, unapproved, err = receiptFacts(ctx, a.pool, project, run, "", runs.ShipMargin)
	}
	var placed []placedRule
	if err == nil {
		placed, err = a.placedRules(ctx, a.pool, project, facts, snap, runs.ShipMargin)
	}
	if err == nil && slices.ContainsFunc(placed, func(p placedRule) bool { return p.rule.ID == depsRule }) {
		err = a.builtAppOf(ctx, project, facts, placed, runs.ShipMargin)
	}
	if err != nil {
		return false, err
	}
	gate := slices.ContainsFunc(promoteRules(snap), func(r gatepolicy.Rule) bool { return r.ID == "prod.clean-gate" && r.State == gatepolicy.Blocking })
	reviewed := facts != nil && facts.findings != nil && (facts.state == "clean" || facts.state == "blocked")
	production, uncounted := placedOpen(placed)
	return (!gate || reviewed && unapproved == 0) && production == 0 && !uncounted, nil
}

// builtAppOf reads onto placed the built app a promotion under run ships, as its receipt read it: the image a
// rollback review recorded, else the one preview runs; none recorded reads no scan, which never passes.
func (a *app) builtAppOf(ctx context.Context, project string, run *runFacts, placed []placedRule, margin time.Duration) error {
	var image string
	switch {
	case run != nil && run.rollback:
		image = run.ships
	default:
		err := a.pool.QueryRow(ctx, "SELECT d.image FROM "+runs.ShownDeploys+" d WHERE d.project_id = $1 AND d.env = 'preview'", project).Scan(&image)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return readBuiltApps(ctx, a.pool, project, runIDOf(run), image, placed, margin)
}

// runIDOf is the id of the run the receipt is read for, "" with none.
func runIDOf(run *runFacts) string {
	if run == nil {
		return ""
	}
	return run.id
}

func ownedBy(owners []actor) string {
	switch len(owners) {
	case 0:
		return "no owner has claimed this app"
	case 1:
		return "owned by " + nameOf(owners[0])
	}
	return fmt.Sprintf("owned by %s and %d other%s", nameOf(owners[0]), len(owners)-1, plural(len(owners)-1))
}

func nameOf(a actor) string {
	if a.Display != "" {
		return a.Display
	}
	return a.Subject
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// receiptFactsSQL is the gate run — the one named, or the project's newest when none is, of one
// package when one is named — the newest gate run of its package, and its verdict's findings, null
// before the verdict. A patch run that deployed is read when named, as a gate run is — so a promotion of it
// clears as one of a gate run does, whatever preview runs by then — and is its package's newest gate run while
// preview's row names it, preview running the image it deployed, or while it is the one named (contracts/routes.md,
// Patch runs); it is never the project's newest. A rollback review is read only when named: it ships what production ran, so it
// never stands in for the run behind preview, nor is a newer review of preview's package; its own newest
// is its package's newest gate run of any kind, and whether a promotion of it went live is read beside it. A promote run is none, and a pull request's or a check's run is never the project's: it is
// reported on its GitHub check, or by the CLI that ran the check.
const receiptFactsSQL = `WITH r AS (SELECT id, state, failure_reason, failed_in, package_id, rollback_to, rollback_reason, rollback_replaces, ships, started_by_kind, started_by_subject, started_by_display FROM runs
    WHERE project_id = $1 AND (kind = 'gate' OR kind = 'patch' AND ($2::text <> '' AND EXISTS (SELECT 1 FROM timeline_rows t WHERE t.run_id = runs.id AND t.dedupe = 'deploy:preview')
        OR $3::text <> '' AND id IN (SELECT gate_run FROM deploys WHERE project_id = $1 AND env = 'preview')))
      AND ($2::text = '' AND rollback_to IS NULL OR id = $2) AND ($3::text = '' OR package_id = $3)
      AND package_id NOT IN (SELECT id FROM packages WHERE project_id = $1 AND source_kind IN ('repo-pr', 'check'))
    ORDER BY created_at DESC, id DESC LIMIT 1)
SELECT r.id, r.state, COALESCE(r.failure_reason, ''), COALESCE(r.failed_in, ''), r.package_id,
  COALESCE((SELECT p.label FROM packages p WHERE p.project_id = $1 AND p.id = r.package_id), ''),
  (SELECT n.id FROM runs n WHERE n.project_id = $1 AND (n.kind = 'gate' OR n.id = r.id OR n.kind = 'patch' AND n.id IN (SELECT gate_run FROM deploys WHERE project_id = $1 AND env = 'preview'))
    AND (n.rollback_to IS NULL OR r.rollback_to IS NOT NULL) AND n.package_id = r.package_id ORDER BY n.created_at DESC, n.id DESC LIMIT 1),
  (SELECT t.data->'findings' FROM timeline_rows t WHERE t.run_id = r.id AND t.kind = 'verdict'),
  r.rollback_to IS NOT NULL, COALESCE(r.ships, ''), COALESCE(r.rollback_reason, ''), r.rollback_replaces,
  EXISTS (SELECT 1 FROM deploy_history h WHERE h.project_id = $1 AND h.gate_run = r.id),
  COALESCE(r.started_by_kind, ''), COALESCE(r.started_by_subject, ''), COALESCE(r.started_by_display, '')
FROM r`

// receiptFacts loads what evaluate cannot see: the run named — or the project's newest when run is
// "", of pkg when pkg is not "" — the newest run of its package, and its unapproved blocking count,
// expires_at > now() applied by runs.Approvals so evaluate needs no clock. A nil run is a project no
// run has looked at, or a run that is not this project's.
func receiptFacts(ctx context.Context, q rower, project, run, pkg string, margin time.Duration) (*runFacts, int, error) {
	var r runFacts
	err := q.QueryRow(ctx, receiptFactsSQL, project, run, pkg).Scan(&r.id, &r.state, &r.failureReason, &r.failedIn, &r.pkg.id, &r.pkg.label, &r.newest, &r.findings,
		&r.rollback, &r.ships, &r.reason, &r.replaces, &r.shipped, &r.startedBy.Kind, &r.startedBy.Subject, &r.startedBy.Display)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	_, unapproved, err := runs.ApprovalsWithin(ctx, q, project, r.id, r.findings, margin)
	if err != nil {
		return nil, 0, err
	}
	return &r, unapproved, nil
}

// placedRules loads every rule the snapshot places before production but the go-live four, in its
// order, read against run: whether the rule was on in the snapshot the run journaled at its start, and
// its findings in the run that block before production — derived as canyon.blocks is, from the
// production state — and how many no unexpired approval covers. An approval covers its finding in both
// gates. A run with no verdict loads nothing more: evaluate fails it closed.
func (a *app) placedRules(ctx context.Context, q querier, project string, run *runFacts, snap gatepolicy.Snapshot, margin time.Duration) ([]placedRule, error) {
	var out []placedRule
	for _, r := range snap.Rules {
		if r.Production != "" && !slices.Contains(promoteRuleIDs, r.ID) {
			out = append(out, placedRule{rule: r})
		}
	}
	if len(out) == 0 || run == nil || run.findings == nil {
		return out, nil
	}
	journaled, err := a.journaled(run.id)
	if err != nil {
		return nil, err
	}
	skipped, err := skippedChecks(ctx, q, run.id)
	if err != nil {
		return nil, err
	}
	var log finding.Log
	if err := json.Unmarshal(run.findings, &log); err != nil {
		return nil, err
	}
	for i := range out {
		id := out[i].rule.ID
		out[i].journaled = slices.ContainsFunc(journaled.Rules, func(r gatepolicy.Rule) bool { return r.ID == id && r.On() })
		ns, _, _ := strings.Cut(id, ".")
		if id == depsRule {
			out[i].reported = reportedIn(log)
		}
		for _, g := range journaled.Agents {
			if out[i].rule.Producer != gatepolicy.LLMReview || !skipped[g.ID] || !slices.Contains(g.Namespace, ns) {
				continue
			}
			out[i].unread = unreadWhy[gate.Skipped] // a skipped check whose run names nothing more: the host had no model
			if j := slices.IndexFunc(log.Runs, func(r finding.Run) bool { return r.Tool.Driver.Name == g.ID && gate.Unread(r) != "" }); j >= 0 {
				out[i].unread = unreadWhy[gate.Unread(log.Runs[j])]
			}
		}
		if out[i].rule.Production != gatepolicy.Blocking {
			continue
		}
		var blocking json.RawMessage
		blocking, out[i].blocking = blockingUnder(log, out[i].rule.ID)
		if _, out[i].unapproved, err = runs.ApprovalsWithin(ctx, q, project, run.id, blocking, margin); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// blockingUnder is log's findings under rule that block at blocking, each marked so — the log
// runs.Approvals covers — and how many entries they are (contracts/finding.md, One problem, one
// entry).
func blockingUnder(log finding.Log, rule string) (json.RawMessage, int) {
	yes := true
	for i := range log.Runs {
		var kept []finding.Result
		for _, r := range log.Runs[i].Results {
			if blocksUnder(rule, r.RuleID, r.Properties.Canyon.Severity) {
				r.Properties.Canyon.Blocks = &yes
				kept = append(kept, r)
			}
		}
		log.Runs[i].Results = kept
	}
	raw, _ := json.Marshal(log) // a log json.Unmarshal built marshals back
	n, _ := finding.Count(log)
	return raw, n
}

// blocksUnder is whether a finding of ruleID at severity blocks under rule placed blocking: a finding
// under the rule carries its id, or extends it.
func blocksUnder(rule, ruleID string, severity finding.Severity) bool {
	return (ruleID == rule || strings.HasPrefix(ruleID, rule+".")) && finding.Blocks(ruleID, gatepolicy.Blocking, severity)
}

// productionRules is the rules snap places blocking before production but the go-live four: the rules
// whose findings the receipt counts against a promotion (placedRules).
func productionRules(snap gatepolicy.Snapshot) []string {
	var out []string
	for _, r := range snap.Rules {
		if r.Production == gatepolicy.Blocking && !slices.Contains(promoteRuleIDs, r.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

// blocksProduction is whether a finding of ruleID at severity blocks before production under rules, the
// receipt's own answer for it (blockingUnder): the run read's blocks_production (contracts/routes.md; #2206).
func blocksProduction(rules []string, ruleID string, severity finding.Severity) bool {
	return slices.ContainsFunc(rules, func(rule string) bool { return blocksUnder(rule, ruleID, severity) })
}

// journaled is the snapshot run journaled at its start (runs.Input), read from its workflow, never
// recomposed. A workflow that is gone, or an input that does not decode, answers the zero snapshot:
// no rule on, so nothing placed since passes on it.
func (a *app) journaled(run string) (gatepolicy.Snapshot, error) {
	ws, err := a.durable.ListWorkflows(a.durable, dbos.WithFilterWorkflowIDs(run), dbos.WithFilterLoadInput(true), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return gatepolicy.Snapshot{}, err
	}
	raw, ok := "", false
	if len(ws) == 1 {
		raw, ok = ws[0].Input.(string)
	}
	var in runs.Input
	if !ok || json.Unmarshal([]byte(raw), &in) != nil {
		a.log.Printf("canyon-server: run %s: its journaled snapshot does not read", run)
		return gatepolicy.Snapshot{}, nil
	}
	return in.Policy, nil
}

// unreadWhy is a placed row's why, after the version, by the note of the agent's run that read nothing
// (gate.Unread; contracts/routes.md, The promote receipt).
var unreadWhy = map[string]string{
	gate.Skipped:          "'s AI review did not run; check it again once a model is configured",
	gate.OutOfRoom:        "'s AI review ran out of room for its answer; check it again",
	gate.TooLarge:         "'s AI review did not run: the source is larger than it accepts (16 MiB)",
	gate.TooLargeForACall: "'s AI review could not read a file too large for one model call; the review names it",
	gate.NotUTF8:          "'s AI review could not read a file that is not valid UTF-8; the review names it",
	gate.TimedOut:         "'s AI review did not answer in the time it allows; check it again",
	gate.TooLargeBefore:   "'s AI review did not run: the source was over the 2 MiB it took then; check it again, as a larger app is now reviewed in parts",
}

// skippedChecks is the agents whose check on run ended skipped (contracts/events.md, check.completed):
// the agent read nothing (gate.Unread).
func skippedChecks(ctx context.Context, q querier, run string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT data->>'check' FROM timeline_rows WHERE run_id = $1 AND kind = 'check.completed' AND data->>'status' = 'skipped'`, run)
	if err != nil {
		return nil, err
	}
	checks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := map[string]bool{}
	for _, c := range checks {
		out[c] = true
	}
	return out, err
}
