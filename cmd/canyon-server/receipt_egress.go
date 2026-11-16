package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/destination"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
	"github.com/getcanyon-ai/canyon/runs"
)

// prod.egress-allowed's row (design/EGRESS.md §3.4; contracts/routes.md, The promote receipt): what the image preview
// runs connected to since it first went live, against what production allows now. egressOf loads it, egressFacts.read
// judges it, and egressRow words it for evaluate.

const (
	// egressPeriodic is how often Envoy logs a tunnel that runs on: access_log_flush_interval in deploy/egress/envoy.yaml.
	egressPeriodic = 60 * time.Second
	// egressSeenMargin is how far before go-live the read starts, because a live tunnel's last_seen_at lags it: it
	// moves at a periodic entry and reaches its row at the flush after, which may spend four POST /callers first. A
	// tunnel opened before go-live and connected past it is on a row this reads, though its next entry has not come.
	// Stricter, never looser: what the outgoing version connected to in the margin counts too.
	egressSeenMargin = egressPeriodic + egressFlushEvery + egressAddressesFlush/egressCallersAsked*egressCallersWait
	// egressDownAfter is how far the receiver's heartbeat may sit from the read: older, a boot would write the time as
	// receiver_down, a gap no row says while the receiver is down (egressReceiver.boot). The server's clock stamps it and
	// the database's reads it, so the same bound holds ahead of the read: a clock further ahead says nothing.
	egressDownAfter = egressFlushEvery * 3 / 2
)

// The row's lines (design/EXPERIENCE.md D5). A could-not-check line says what a person can do, so one line serves every
// read a deploy to preview clears, and one every read time clears.
const (
	egressOrgRuns  = "not checked: preview runs on your own platform" // neutral: the org's platform runs preview and a promotion goes to it too
	egressNoRecord = "could not check: Canyon doesn't have a full record of preview's connections. Deploy to preview again. If this persists, ask an admin."
	egressCantRead = "could not check: Canyon can't read preview's connections right now. Try again in a few minutes. If this persists, ask an admin."
	egressOverCap  = "could not check: preview connected to over 500 destinations, more than Canyon tracks. Connect to fewer and deploy to preview. If this persists, ask an admin."
	egressRollback = "not compared: a rollback puts back what production ran"
	egressNoNames  = "production does not allow everything preview connected to. The app's team can fix this." // the line naming destinations, to a responder with no grant
	egressInvalid  = "an invalid address"                                                                      // the row of a name no host name is
	egressWhyLimit = 200                                                                                       // the most a row's why holds (app-page.schema.json)
)

// The fix the app's Outbound card gives where a deploy to preview goes around Canyon again (#2723): the host's via
// off, the app's floor null; or, on Docker, the app on its old network, which a deploy via canyon is refused on. Under
// Off a null floor is an open network or one Canyon can't tell, so the first names the setting, never the network (#2774).
const (
	egressDirect      = "could not check: Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound."
	egressOpenNetwork = "could not check: preview may skip Canyon until an admin recreates its network in Settings › Outbound."
)

// egressRead is the rule's read of the app the receipt is for, nil while the rule is off.
type egressRead struct {
	unread    string       // why it could not check; "" once it read
	orgRuns   bool         // the org's platform runs preview and a promotion goes to it too: Canyon has nothing to hold the version to
	denied    []string     // the destinations production does not allow, each once, sorted
	refused   []egressSeen // the same, as rows: will_refuse's denied (needs.go), in denied's order
	rollback  bool         // a rollback review's receipt, which reads no preview
	dismissed []string     // on a rollback, the destinations a person dismissed that production refuses today, sorted
}

// egressRow is the rule's row of the receipt. Nothing read never passes. A rollback's row never blocks: it says what
// production refuses of what a person dismissed as an advisory line, which reads no preview and holds nothing. A row
// orgRuns reads is neutral whatever its state, as an unset stack.blessed is.
func egressRow(r gatepolicy.Rule, read *egressRead, version string) receiptRule {
	row := receiptRule{ID: r.ID, Name: r.Name, Blocks: r.State == gatepolicy.Blocking}
	switch {
	case read == nil:
		row.Why = egressCantRead
	case read.orgRuns:
		row.Unset, row.Why = true, egressOrgRuns
	case read.rollback && len(read.dismissed) > 0:
		lead, tail := "this version may need ", ", which production does not allow. A person can change that on the Outbound card."
		row.Blocks, row.Why, row.noNames = false, lead+namedWithin(read.dismissed, egressWhyLimit-len(lead)-len(tail))+tail, egressRollback
	case read.rollback:
		row.Blocks, row.Passed, row.Why = false, true, egressRollback
	case len(read.denied) > 0:
		lead := "production does not allow "
		row.Why, row.noNames = lead+namedWithin(read.denied, egressWhyLimit-len(lead)), egressNoNames
	case read.unread != "":
		row.Why = read.unread
	default:
		row.Passed, row.Why = true, "production allows everything "+version+" has connected to so far"
	}
	return row
}

// namedWithin is names joined by ", " in at most room characters: as many as fit and how many more, or their count
// when not one does, as one name may be longer than the room.
func namedWithin(names []string, room int) string {
	if all := strings.Join(names, ", "); len(all) <= room {
		return all
	}
	room -= len(fmt.Sprintf(" and %d more", len(names)))
	shown, used := 0, 0
	for _, n := range names {
		if used += len(n) + len(", "); used-len(", ") > room {
			break
		}
		shown++
	}
	if shown == 0 {
		return fmt.Sprintf("%d destination%s", len(names), plural(len(names)))
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

// prodPolicy is what production allows now: the env's mode, its own else the org's, its allowlist and the platform
// destinations.
type prodPolicy struct {
	mode              string
	allowed, platform []string
}

// allows is whether production's check would let a call through, by the check's own two steps: Canonical, which
// refuses in every mode, and Refusal. Its rate limit, between them, is no policy: a name it held back is allowed.
func (p prodPolicy) allows(dest string, port int) bool {
	name, _, err := destination.Canonical(net.JoinHostPort(dest, strconv.Itoa(port)), port)
	return err == nil && egresscontrol.Refusal(name, slices.Contains(p.platform, name), p.mode, p.allowed) == ""
}

// egressSeen is a row of preview's as the rule reads it.
type egressSeen struct {
	destination string
	port        int
	lastSeen    time.Time
}

// name is how the receipt names the row: an allowlist holds names, so a port only where that name is refused for it.
func (s egressSeen) name() string {
	switch {
	case s.destination == "invalid":
		return egressInvalid
	case s.port != 80 && s.port != 443:
		return s.destination + ":" + strconv.Itoa(s.port)
	}
	return s.destination
}

// windowGap is a gap the window holds: one that names no app, or the app's own over_cap of one env.
type windowGap struct{ app, env *string }

// egressGapHolds is what the gaps in the window do to the row, the line it reads, "" for none: the one place the rule
// decides it (design/EGRESS.md §3.4, Gaps). As §3.4 is written a gap that names no app may be any app's, so it
// fails closed, and so does preview's own over_cap, whose calls no row holds. Production's over_cap says nothing of preview.
func egressGapHolds(gaps []windowGap) string {
	unnamed := false
	for _, g := range gaps {
		switch {
		case g.app == nil:
			unnamed = true
		case g.env != nil && *g.env == "preview":
			return egressOverCap
		}
	}
	if unnamed {
		return egressNoRecord
	}
	return ""
}

// egressReadFrom is where the read starts for a version that went live at wentLive.
func egressReadFrom(wentLive time.Time) time.Time { return wentLive.Add(-egressSeenMargin) }

// egressFacts is everything the rule judges: preview's deploy row, its image's first go-live, the org's run_by setting,
// the app's floor, the receiver's heartbeat, preview's rows and the gaps in the window, and production's policy.
type egressFacts struct {
	now, wentLive time.Time  // the read, and when the running deployment went live
	firstLive     *time.Time // when the image preview runs first went live there, nil when no deploy of it reads
	configAt      *time.Time // when the row's spec was last written, nil on a row from before the column
	runBy         string     // who runs preview, its deploy row's; "" when it reads as nothing
	setting       string     // where a promotion goes, the org's run_by (design/HAND-OFF.md §1); "" when it reads as nothing
	via, floor    *string
	kube          bool
	heartbeat     *time.Time
	rows          []egressSeen
	gaps          []windowGap
	prod          prodPolicy
}

// through is whether preview's calls are all on the records since it went live: its row says it ran under canyon, on
// Docker its network leaves it no way around Envoy, and that spec is the one it went live under. A recreation writes
// run_config_at and leaves deployed_at, a deploy writes both with one now(): later, what it reached before is on no
// row, and no row is no evidence. Kubernetes has no app network: each env's own pod is floored by its spec.
func (f egressFacts) through() bool {
	return f.via != nil && *f.via == "canyon" && (f.kube || f.floor != nil && *f.floor == "enforced") && f.configAt != nil && !f.configAt.After(f.wentLive)
}

// read judges the facts, the first that holds its answer (contracts/routes.md, The promote receipt): what can't be
// read, then what production doesn't allow, which is true whatever else is missing, then what could hide more. Rows
// read from the image's first go-live, so deploying the same image again clears nothing it did. The margin hangs off
// the running deployment, never off that first go-live, which doesn't move: what the outgoing version did just before
// it counts until a deploy goes past it, as that call may be no call of this image's. What a redeploy is the remedy
// for, the records' keep and the gaps, reads from the running deployment too.
//
// Neutral takes both reads saying org. The setting decides where a promotion goes, and a switch back to canyon leaves
// preview's row as it was, so a row saying org alone may be an image about to start behind production's allowlist, with
// nothing read of what it connects to. Either read that says neither value blocks: it is never neutral.
func (f egressFacts) read() egressRead {
	known := func(by string) bool { return by == "canyon" || by == "org" }
	switch {
	case !known(f.runBy), !known(f.setting):
		return egressRead{unread: egressCantRead}
	case f.runBy == "org" && f.setting == "org":
		return egressRead{orgRuns: true}
	case f.runBy == "org":
		return egressRead{unread: egressNoRecord}
	case !f.through() && f.floor == nil: // the app's floor as its Outbound card reads it: nothing holds it to Canyon
		return egressRead{unread: egressDirect}
	case !f.through() && !f.kube && *f.floor == "not_enforced":
		return egressRead{unread: egressOpenNetwork}
	case !f.through(), f.firstLive == nil:
		return egressRead{unread: egressNoRecord}
	}
	from := minTime(*f.firstLive, egressReadFrom(f.wentLive))
	var denied []string
	var refused []egressSeen
	for _, row := range f.rows {
		if !row.lastSeen.Before(from) && !f.prod.allows(row.destination, row.port) {
			denied, refused = append(denied, row.name()), append(refused, row)
		}
	}
	if len(denied) > 0 {
		slices.Sort(denied)
		slices.SortFunc(refused, func(a, b egressSeen) int {
			return cmp.Or(strings.Compare(a.name(), b.name()), cmp.Compare(a.port, b.port))
		})
		return egressRead{denied: slices.Compact(denied), refused: refused}
	}
	switch {
	case f.now.Sub(egressReadFrom(f.wentLive)) > egressKept:
		return egressRead{unread: egressNoRecord}
	case f.heartbeat == nil || f.now.Sub(*f.heartbeat).Abs() > egressDownAfter:
		return egressRead{unread: egressCantRead}
	}
	return egressRead{unread: egressGapHolds(f.gaps)}
}

// egressOf is the rule's read of project for its receipt, nil while the rule is off: then it is not on the receipt and
// nothing is read. A rollback review reads no preview.
func (a *app) egressOf(ctx context.Context, q querier, snap gatepolicy.Snapshot, project string, rollback bool) (*egressRead, error) {
	i := slices.IndexFunc(snap.Rules, func(r gatepolicy.Rule) bool { return r.ID == egressRule })
	switch {
	case i < 0: // the boot snapshot refuses to load without it, so a snapshot that lacks it is no snapshot to read
		return nil, errors.New("the snapshot carries no " + egressRule)
	case snap.Rules[i].State == gatepolicy.Off:
		return nil, nil
	}
	platform, err := a.platformDestinations(ctx, q)
	if err != nil {
		return nil, err
	}
	if rollback {
		return readEgressRollback(ctx, q, project, platform)
	}
	return a.readEgressPreview(ctx, q, project, platform)
}

// readEgressRollback reads what a rollback's row lists: each destination a person dismissed in production that production
// refuses today, its allowlist being live policy (design/EGRESS.md §2.5). A production the org runs lists none, as the
// Outbound card has no Allow there. Only a row saying org does: the line stops nothing, so one that doesn't read keeps it.
func readEgressRollback(ctx context.Context, q querier, project string, platform []string) (*egressRead, error) {
	prod, err := prodPolicyOf(ctx, q, project, platform)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, "SELECT destination FROM egress_requests WHERE project_id = $1 AND env = 'prod' AND state = 'dismissed' AND NOT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'prod' AND run_by = 'org') ORDER BY destination COLLATE \"C\"", project)
	if err != nil {
		return nil, err
	}
	dismissed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return &egressRead{rollback: true, dismissed: slices.DeleteFunc(dismissed, func(d string) bool { return prod.allows(d, 443) })}, nil
}

// firstGoLive is the oldest go-live in preview of the image a deploys row d runs: every deploy of it writes one
// deploy.completed, and the timeline is never edited.
const firstGoLive = `(SELECT min(t.ts) FROM timeline_rows t JOIN runs r ON r.id = t.run_id
	WHERE r.project_id = d.project_id AND t.dedupe = 'deploy:preview' AND t.data->>'image_digest' = split_part(d.image, '@', 2))`

// readEgressPreview is the rule's read of what preview runs, whatever the rule's state: the receipt's and Guardrails'
// preview's. clock_timestamp, not now(): a promotion's transaction may have waited on the ledger lock since it began.
// The org's setting is read with preview's row, and either one that doesn't read reads as "", which fails closed.
func (a *app) readEgressPreview(ctx context.Context, q querier, project string, platform []string) (*egressRead, error) {
	var f egressFacts
	f.kube = a.kube
	err := q.QueryRow(ctx, "SELECT clock_timestamp(), d.deployed_at, "+firstGoLive+", d.run_config_at, coalesce(d.run_by, ''), coalesce((SELECT run_by FROM org_preferences), ''), d.egress->>'via', (SELECT seen_at FROM egress_receiver) FROM "+runs.ShownDeploys+" d WHERE d.project_id = $1 AND d.env = 'preview'", project).
		Scan(&f.now, &f.wentLive, &f.firstLive, &f.configAt, &f.runBy, &f.setting, &f.via, &f.heartbeat)
	if errors.Is(err, pgx.ErrNoRows) { // preview went between the reads: nothing read, which never passes
		return &egressRead{unread: egressCantRead}, nil
	}
	if err != nil {
		return nil, err
	}
	if f.floor, err = a.egressFloor(ctx, q, project); err != nil {
		return nil, err
	}
	if f.prod, err = prodPolicyOf(ctx, q, project, platform); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, "SELECT destination, port, last_seen_at FROM egress_destinations WHERE app = $1 AND env = 'preview'", project)
	if err != nil {
		return nil, err
	}
	var row egressSeen
	if _, err := pgx.ForEachRow(rows, []any{&row.destination, &row.port, &row.lastSeen}, func() error {
		f.rows = append(f.rows, row)
		return nil
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, "SELECT app, env FROM egress_gaps WHERE to_at >= $1 AND (app IS NULL OR app = $2)", egressReadFrom(f.wentLive), project)
	if err != nil {
		return nil, err
	}
	var gap windowGap
	if _, err := pgx.ForEachRow(rows, []any{&gap.app, &gap.env}, func() error {
		f.gaps = append(f.gaps, gap) // each scan allocates the strings it points at
		return nil
	}); err != nil {
		return nil, err
	}
	read := f.read()
	return &read, nil
}

// prodPolicyOf is what production allows now, live policy never recorded on a deploy row.
func prodPolicyOf(ctx context.Context, q querier, project string, platform []string) (prodPolicy, error) {
	p := prodPolicy{platform: platform}
	err := q.QueryRow(ctx, "SELECT coalesce((SELECT mode FROM egress_modes WHERE project_id = $1 AND env = 'prod'), (SELECT mode FROM egress_settings))", project).Scan(&p.mode)
	if err != nil {
		return p, err
	}
	rows, err := q.Query(ctx, "SELECT entry FROM egress_allowed WHERE project_id = $1 AND env = 'prod'", project)
	if err != nil {
		return p, err
	}
	p.allowed, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return p, err
}
