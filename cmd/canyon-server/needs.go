package main

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
	"github.com/getcanyon-ai/canyon/runs"
)

// What needs a person (contracts/routes.md; design/EXPERIENCE.md D15; #2724): one ordered list per app, which the fleet
// read's rows and the status read carry. needFactsOf reads every app the caller sees in one statement, needsFrom adds
// what the other tables hold in a fixed set more, whatever the fleet's size, and the executor is asked once.

// needKinds is every kind in the list's order (contracts/routes.md, *What needs a person*).
var needKinds = []string{"held", "down", "refused", "refused_code", "secret", "approval", "blocked", "unchecked", "blocked_prod", "will_refuse",
	"patch", "access", "unowned", "never_reviewed", "recreate", "notify_failing", "licence", "patching_unlicensed"}

// noNeed is the rank of a row read whole that carries none: the risk sort puts it after every row that does.
const noNeed = 12

// need is one thing that waits on a person: its kind and environment, since when, whether the reader can resolve it,
// whom else to ask, and what its kind adds (fields), with its rank and what it names to order the list by.
type need struct {
	kind    string
	env     *string
	since   *time.Time
	you     bool
	ask     []askee
	rank    int
	subject string
	fields  map[string]any
}

// askee is someone else who acts on a need: an admin, by name.
type askee struct {
	Display string `json:"display"`
	As      string `json:"as"`
}

func (n need) MarshalJSON() ([]byte, error) {
	ask := n.ask
	if ask == nil {
		ask = []askee{}
	}
	m := map[string]any{"kind": n.kind, "env": n.env, "rank": n.rank, "since": nil, "you": n.you, "ask": ask}
	if n.since != nil {
		m["since"] = stamp(*n.since)
	}
	maps.Copy(m, n.fields)
	return json.Marshal(m)
}

// needRank is a kind's rank (contracts/routes.md, *What needs a person*): preview's down, refused and refused_code 7,
// will_refuse 6 while its rule blocks and 7 while it is advisory.
func needRank(kind string, env *string, blocks bool) int {
	switch {
	case kind == "will_refuse" && blocks:
		return 6
	case kind == "will_refuse":
		return 7
	case env != nil && *env == "preview" && (kind == "down" || kind == "refused" || kind == "refused_code"):
		return 7
	}
	return map[string]int{"held": 1, "down": 2, "refused": 3, "refused_code": 3, "secret": 4, "approval": 5, "blocked": 6, "unchecked": 6, "blocked_prod": 6,
		"patch": 8, "access": 9, "unowned": 10, "never_reviewed": 10, "recreate": 11, "notify_failing": 11, "licence": 11, "patching_unlicensed": 11}[kind]
}

// newNeed is a need of kind, ranked, with the fields it adds.
func newNeed(kind string, env *string, since *time.Time, you bool, fields map[string]any) need {
	blocks, _ := fields["blocks"].(bool)
	return need{kind: kind, env: env, since: since, you: you, rank: needRank(kind, env, blocks), fields: fields}
}

// sortNeeds puts one app's needs in the list's order: rank, then since, the oldest first and none last, then kind,
// then what it names.
func sortNeeds(needs []need) {
	slices.SortStableFunc(needs, func(a, b need) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), sinceCompare(a.since, b.since),
			cmp.Compare(slices.Index(needKinds, a.kind), slices.Index(needKinds, b.kind)), cmp.Compare(a.subject, b.subject))
	})
}

// sinceCompare orders two sinces, the oldest first and none last.
func sinceCompare(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}

var (
	prodNeedEnv    = new("prod")
	previewNeedEnv = new("preview")
)

func envNamed(env string) *string {
	if env == "prod" {
		return prodNeedEnv
	}
	return previewNeedEnv
}

// needFacts is one app as the fleet's derivation reads it for its needs.
type needFacts struct {
	id, level                 string
	team, claimable, secrets  bool
	secretsOpen               bool // a blocking secret of the latest review no unexpired approval covers
	ownersN                   int
	gateState, stage, version string
	since, created            time.Time
	heldAt                    *time.Time
	runs                      map[string]string    // env → who runs it now, canyon or org; absent while nothing is deployed there
	deployed                  map[string]time.Time // env → when what it runs was deployed there
	open                      []openFinding        // the package's run's blocking findings no unexpired approval covers, while it is blocked
}

// openFinding is one of those findings: its rule and, for gate.unreadable, why it could not check.
type openFinding struct {
	Rule    string `json:"rule"`
	Problem string `json:"problem"`
}

// needFactsSQL is every live app the caller sees, by the fleet's one derivation ($1 to $4 the caller), or the one $5 names.
var needFactsSQL = fleetDerivation + `SELECT id, level, team, claimable, secrets, secrets_open, owners_n, gate_state, stage, coalesce(label, substr(package, 5), ''), since, created_at,
  (hold->>'at')::timestamptz,
  CASE WHEN preview_pending <> 'null'::jsonb THEN 'org' WHEN preview_package IS NOT NULL THEN preview_run_by END,
  CASE WHEN prod_package IS NOT NULL OR prod_pending <> 'null'::jsonb THEN CASE WHEN prod_pending <> 'null'::jsonb THEN 'org' ELSE prod_run_by END END,
  open_blocking, preview_at, prod_at
FROM fleet WHERE $5::text = '' OR id = $5 ORDER BY id`

// needFactsOf reads the facts of every app the caller sees, or of id alone.
func needFactsOf(ctx context.Context, q querier, id string) ([]needFacts, error) {
	rows, err := q.Query(ctx, needFactsSQL, append(callerArgs(ctx), id)...)
	if err != nil {
		return nil, err
	}
	var f needFacts
	var preview, prod *string
	var previewAt, prodAt *time.Time
	var open []byte
	var out []needFacts
	_, err = pgx.ForEachRow(rows, []any{&f.id, &f.level, &f.team, &f.claimable, &f.secrets, &f.secretsOpen, &f.ownersN, &f.gateState, &f.stage, &f.version, &f.since, &f.created,
		&f.heldAt, &preview, &prod, &open, &previewAt, &prodAt}, func() error {
		g := f
		g.runs, g.deployed, g.open = map[string]string{}, map[string]time.Time{}, nil
		if previewAt != nil {
			g.deployed["preview"] = *previewAt
		}
		if prodAt != nil {
			g.deployed["prod"] = *prodAt
		}
		if preview != nil {
			g.runs["preview"] = *preview
		}
		if prod != nil {
			g.runs["prod"] = *prod
		}
		if len(open) > 0 {
			if err := json.Unmarshal(open, &g.open); err != nil {
				return err
			}
		}
		out = append(out, g)
		return nil
	})
	return out, err
}

// readsEnv is whether the caller reads env's state on the app: a row read whole and a viewer's both, a responder's
// production, nothing at none (contracts/routes.md, the fleet read).
func readsEnv(ctx context.Context, f needFacts, env string) bool {
	return f.team || f.level == "viewer" || env == "prod" && roleOf(ctx) == "responder"
}

// statesOf is whether each environment Canyon runs of the apps the caller reads runs now, from one executor call for
// them all (sayRunning): app → env → state.
func (a *app) statesOf(ctx context.Context, facts []needFacts) map[string]map[string]string {
	envs := map[string]map[string]any{}
	for _, f := range facts {
		for env, by := range f.runs {
			if by == "canyon" && readsEnv(ctx, f, env) {
				if envs[f.id] == nil {
					envs[f.id] = map[string]any{}
				}
				envs[f.id][env] = map[string]any{"run_by": "canyon"}
			}
		}
	}
	a.sayRunning(ctx, envs, false, false)
	out := map[string]map[string]string{}
	for id, project := range envs {
		out[id] = map[string]string{}
		for env, e := range project {
			out[id][env], _ = e.(map[string]any)["state"].(string)
		}
	}
	return out
}

// hostReasons are the reasons a check could not finish that reviewing again fails on (contracts/finding.md, Canyon-run
// tools): the host's to fix.
var hostReasons = []string{"its vulnerability database is over 5 days old", "this Canyon host has no image for it", "could not load all of its rules",
	"did not answer SARIF", "answered more than Canyon reads"}

// unchecked is the checks a blocked run's open findings could not finish, when every one is gate.unreadable and says
// which tool and why (`could not check: <tool>: <reason>`); false otherwise, and the run reads as blocked.
func unchecked(open []openFinding) ([]map[string]any, bool) {
	var checks []map[string]any
	for _, o := range open {
		tool, reason, ok := strings.Cut(strings.TrimPrefix(o.Problem, "could not check: "), ": ")
		if o.Rule != "gate.unreadable" || !strings.HasPrefix(o.Problem, "could not check: ") || !ok || tool == "" || reason == "" {
			return nil, false
		}
		fix := "review"
		if slices.Contains(hostReasons, reason) {
			fix = "host"
		}
		checks = append(checks, map[string]any{"tool": tool, "reason": reason, "fix": fix})
	}
	return checks, len(checks) > 0
}

// secretsAlone is whether every open finding is a secret's, which the secret need says with its fix, so a version blocked
// by its secrets alone is one need (contracts/routes.md, *What needs a person*, blocked); none to read is not.
func secretsAlone(open []openFinding) bool {
	return len(open) > 0 && !slices.ContainsFunc(open, func(o openFinding) bool { return !strings.HasPrefix(o.Rule, "secrets.") })
}

// codeRefusal is why no allowlist lets a recorded call through: a dotted IPv4 address, which records keep as it is,
// or a port other than 80 and 443 (contracts/routes.md, *What needs a person*); "" for any other.
func codeRefusal(destination string, port int) string {
	if _, err := netip.ParseAddr(destination); err == nil {
		return "address"
	}
	if port != 80 && port != 443 {
		return "port"
	}
	return ""
}

// needsFrom is each app's needs, ordered, from facts and states (statesOf), by app; an app with none is absent.
func (a *app) needsFrom(ctx context.Context, q querier, facts []needFacts, states map[string]map[string]string) (map[string][]need, error) {
	role, admin := roleOf(ctx), roleOf(ctx) == "admin"
	var team, owned []string
	for _, f := range facts {
		if f.team {
			team = append(team, f.id)
		}
		if f.team && (admin || f.level == "owner") {
			owned = append(owned, f.id)
		}
	}
	admins, err := a.adminNames(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string][]need{}
	add := func(id string, n need) { out[id] = append(out[id], n) }
	lifts, err := liftsOf(ctx, q, facts)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, f := range facts {
		if f.heldAt != nil && (f.team || role == "responder") {
			held[f.id] = true
			n := newNeed("held", prodNeedEnv, f.heldAt, lifts[f.id], nil)
			n.ask = admins
			add(f.id, n)
		}
		if !f.team {
			continue
		}
		for _, env := range []string{"prod", "preview"} {
			onPurpose := env == "prod" && f.heldAt != nil // production stopped by its hold, which says so
			if state := states[f.id][env]; f.runs[env] == "canyon" && (state == "stopped" || state == "restarting") && !onPurpose {
				at, ok := f.deployed[env]
				n := newNeed("down", envNamed(env), nil, true, map[string]any{"state": state})
				if ok {
					n.since = &at
				}
				add(f.id, n)
			}
		}
		if f.secretsOpen { // an approved secret is a person's decision, no need
			add(f.id, newNeed("secret", nil, nil, true, map[string]any{"version": f.version}))
		}
		if f.stage == "blocked" && (!f.secrets || !secretsAlone(f.open)) {
			since := f.since
			if checks, ok := unchecked(f.open); ok {
				n := newNeed("unchecked", nil, &since, true, map[string]any{"version": f.version, "count": len(f.open), "checks": checks})
				n.ask = admins
				add(f.id, n)
			} else {
				// a blocked run's verdict holds a blocking finding: one with no verdict row to count still blocks, as one
				n := newNeed("blocked", nil, &since, true, map[string]any{"version": f.version, "count": max(len(f.open), 1)})
				n.ask = admins
				add(f.id, n)
			}
		}
		if f.ownersN == 0 {
			n := newNeed("unowned", nil, nil, f.claimable, nil)
			n.ask = admins
			add(f.id, n)
		}
		if f.gateState == "never_reviewed" {
			created := f.created
			add(f.id, newNeed("never_reviewed", nil, &created, true, nil))
		}
	}
	for _, read := range []func(context.Context, querier, []string, map[string]bool, func(string, need)) error{a.refusedNeeds, codeNeeds, a.approvalNeeds, a.patchNeeds} {
		if err := read(ctx, q, team, held, add); err != nil {
			return nil, err
		}
	}
	if err := accessNeeds(ctx, q, owned, add); err != nil {
		return nil, err
	}
	if admin && !a.kube {
		if err := recreateNeeds(ctx, q, team, add); err != nil {
			return nil, err
		}
	}
	for id := range out {
		sortNeeds(out[id])
	}
	return out, nil
}

// liftsOf is, of each held app the caller reads, whether they lift its hold, by the lift's own rule (liftsOffTeam): an
// admin every one, a responder those off the team now and on the hold's record, nobody else any. One statement reads
// a responder's holds' records, whatever the fleet's size.
func liftsOf(ctx context.Context, q querier, facts []needFacts) (map[string]bool, error) {
	out := map[string]bool{}
	levels := map[string]string{}
	for _, f := range facts {
		if f.heldAt != nil {
			levels[f.id] = f.level
			out[f.id] = roleOf(ctx) == "admin"
		}
	}
	if roleOf(ctx) != "responder" || len(levels) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT h.project_id, h.team_emails, h.team_subjects, h.team_groups,
		EXISTS (SELECT 1 FROM project_access g WHERE g.project_id = h.project_id AND g.kind = 'group' AND g.level IN ('owner', 'contributor'))
		FROM prod_holds h WHERE h.project_id = ANY($1)`, slices.Collect(maps.Keys(levels)))
	if err != nil {
		return nil, err
	}
	var app string
	var h runs.Hold
	var grouped bool
	_, err = pgx.ForEachRow(rows, []any{&app, &h.TeamEmails, &h.TeamSubjects, &h.TeamGroups, &grouped}, func() error {
		record := h
		out[app] = liftsOffTeam(ctx, levels[app], &record, grouped)
		return nil
	})
	return out, err
}

// adminNames is the org's admins, each by the display name of their newest session, else the address admin_required
// names, sorted: whom a need that is theirs to hand on names.
func (a *app) adminNames(ctx context.Context, q querier) ([]askee, error) {
	rows, err := q.Query(ctx, `SELECT coalesce((SELECT s.actor_display FROM sessions s WHERE lower(s.email) = e AND s.actor_display <> '' ORDER BY s.created_at DESC LIMIT 1), e)
		FROM unnest($1::text[]) e`, a.orgNow(ctx).admins)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	slices.Sort(names)
	out := []askee{}
	for _, n := range slices.Compact(names) {
		out = append(out, askee{Display: n, As: "admin"})
	}
	return out, err
}

// refusedNeeds adds the egress requests each env refuses now (refusingNow), of the apps ids names.
func (a *app) refusedNeeds(ctx context.Context, q querier, ids []string, held map[string]bool, add func(string, need)) error {
	platform, err := a.platformDestinations(ctx, q)
	if err != nil {
		return err
	}
	now, err := refusingNow(ctx, q, ids, platform)
	if err != nil {
		return err
	}
	for k, r := range now {
		first := r.first
		n := newNeed("refused", envNamed(k.env), &first, true, map[string]any{"destination": k.destination, "refusals": r.refusals,
			"last_refused_at": stamp(r.last), "other_calls": r.otherCalls, "held": k.env == "prod" && held[k.app]})
		n.subject = k.destination
		add(k.app, n)
	}
	return nil
}

// codeNeeds adds each destination an env's records hold refused that no allowlist can let through, seen since its running
// container started less the receipt's margin (#2738), the deployment through Canyon.
func codeNeeds(ctx context.Context, q querier, ids []string, _ map[string]bool, add func(string, need)) error {
	rows, err := q.Query(ctx, `SELECT e.app, e.env, e.destination, e.port, e.calls, e.first_seen_at, e.last_seen_at FROM egress_destinations e
		JOIN `+runs.ShownDeploys+` d ON d.project_id = e.app AND d.env = e.env
		WHERE e.app = ANY($1) AND e.last_outcome = 'refused' AND d.egress->>'via' = 'canyon'
		AND e.last_seen_at >= coalesce(d.container_started_at, d.run_started_at, d.deployed_at) - make_interval(secs => $2)`, ids, egressSeenMargin.Seconds())
	if err != nil {
		return err
	}
	var app, env, dest string
	var port int
	var calls int64
	var first, last time.Time
	_, err = pgx.ForEachRow(rows, []any{&app, &env, &dest, &port, &calls, &first, &last}, func() error {
		if why := codeRefusal(dest, port); why != "" {
			since := first
			n := newNeed("refused_code", envNamed(env), &since, true, map[string]any{"destination": dest, "port": port, "why": why, "calls": calls, "last_seen_at": stamp(last)})
			n.subject = dest
			add(app, n)
		}
		return nil
	})
	return err
}

// approvalNeeds adds each production approval pending, unexpired and current as its read reads it (Production
// approvals): preview still runs what it names, or for a rollback review's, production still runs what it replaces.
// Never with sign-in off, where no approval is asked.
func (a *app) approvalNeeds(ctx context.Context, q querier, ids []string, _ map[string]bool, add func(string, need)) error {
	if a.auth != authOIDC {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT x.project_id, x.code, x.requested_by_kind, x.requested_by_subject, x.requested_by_display, x.requested_at, x.expires_at,
		coalesce((SELECT p.label FROM packages p WHERE p.project_id = x.project_id AND p.id = x.package_id), substr(x.package_id, 5))
		FROM promotion_approvals x LEFT JOIN runs g ON g.id = x.gate_run
		WHERE x.project_id = ANY($1) AND x.kind = 'promotion' AND x.state = 'pending' AND x.expires_at > now()
		AND CASE WHEN g.rollback_to IS NOT NULL THEN (SELECT d.image FROM deploys d WHERE d.project_id = x.project_id AND d.env = 'prod') IS NOT DISTINCT FROM g.rollback_replaces
			ELSE EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = x.project_id AND d.env = 'preview' AND d.package_id = x.package_id AND d.image = x.image) END`, ids)
	if err != nil {
		return err
	}
	var app, code, version string
	var by actor
	var asked, expires time.Time
	_, err = pgx.ForEachRow(rows, []any{&app, &code, &by.Kind, &by.Subject, &by.Display, &asked, &expires, &version}, func() error {
		since := asked
		add(app, newNeed("approval", prodNeedEnv, &since, true, map[string]any{"version": version, "asked_by": by, "code": shown(code), "expires_at": stamp(expires)}))
		return nil
	})
	return err
}

// patchNeeds adds each patch approval open (patchAskOpen) while Canyon patches the app (contracts/routes.md, Patch runs).
func (a *app) patchNeeds(ctx context.Context, q querier, ids []string, _ map[string]bool, add func(string, need)) error {
	if a.pipe == nil {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT x.project_id, x.code, x.requested_at, x.expires_at,
		coalesce((SELECT p.label FROM packages p WHERE p.project_id = x.project_id AND p.id = x.package_id), substr(x.package_id, 5))
		FROM promotion_approvals x WHERE x.project_id = ANY($1) AND x.kind = 'patch' AND `+patchAskOpen, ids)
	if err != nil {
		return err
	}
	type ask struct {
		app, code, version string
		asked, expires     time.Time
	}
	var x ask
	var asks []ask
	if _, err := pgx.ForEachRow(rows, []any{&x.app, &x.code, &x.asked, &x.expires, &x.version}, func() error { asks = append(asks, x); return nil }); err != nil || len(asks) == 0 {
		return err
	}
	apps := make([]string, len(asks))
	for i, x := range asks {
		apps[i] = x.app
	}
	on, err := a.patchingFor(ctx, q, apps)
	for _, x := range asks {
		if on[x.app] {
			since := x.asked
			add(x.app, newNeed("patch", prodNeedEnv, &since, true, map[string]any{"version": x.version, "code": shown(x.code), "expires_at": stamp(x.expires)}))
		}
	}
	return err
}

// accessNeeds adds each request for access to the apps ids names, those the caller owns or, an admin, every one, with the
// address Add and Dismiss name it by, and the asker's level when they asked: null on a request from before it was
// recorded, never none (#2774).
func accessNeeds(ctx context.Context, q querier, ids []string, add func(string, need)) error {
	rows, err := q.Query(ctx, "SELECT project_id, subject, coalesce(nullif(display, ''), email), email, requested_at, level FROM access_requests WHERE project_id = ANY($1)", ids)
	if err != nil {
		return err
	}
	var app, email string
	var by actor
	var asked time.Time
	var level *string
	_, err = pgx.ForEachRow(rows, []any{&app, &by.Subject, &by.Display, &email, &asked, &level}, func() error {
		since := asked
		n := newNeed("access", nil, &since, true, map[string]any{"asked_by": actor{Kind: "person", Subject: by.Subject, Display: by.Display}, "email": email, "level": level})
		n.subject = by.Display
		add(app, n)
		return nil
	})
	return err
}

// recreateNeeds adds, to an admin on Docker, each app whose network is not what the host's via makes it (the org's
// egress read's networks_to_recreate), with what its network is now.
func recreateNeeds(ctx context.Context, q querier, ids []string, add func(string, need)) error {
	rows, err := q.Query(ctx, `SELECT p.id, coalesce((SELECT CASE WHEN n.internal IS NULL THEN 'unknown' WHEN n.internal THEN 'internal' ELSE 'open' END
			FROM app_networks n WHERE n.project_id = p.id), 'open'), s.via
		FROM projects p CROSS JOIN egress_settings s WHERE p.id = ANY($1) AND EXISTS (SELECT 1 FROM deploys WHERE project_id = p.id AND run_by = 'canyon')
		AND (`+runs.NetworkUnknown("p.id")+` OR `+runs.NetworkInternal("p.id")+` <> (s.via = 'canyon'))`, ids)
	if err != nil {
		return err
	}
	var app, network, via string
	_, err = pgx.ForEachRow(rows, []any{&app, &network, &via}, func() error {
		add(app, newNeed("recreate", nil, nil, true, map[string]any{"network": network, "via": via}))
		return nil
	})
	return err
}

// requestKey names one egress request: its app, env and destination.
type requestKey struct{ app, env, destination string }

// refusingRequest is one egress request its env refuses now: how often and when, and the calls the other env's records
// hold to its destination.
type refusingRequest struct {
	refusals, otherCalls int64
	first, last          time.Time
}

// refusingNow is the egress requests their env refuses now, of the apps ids names (contracts/routes.md, the fleet read's
// refused tile; #2723): pending; refused by the env's mode, its own else the org's, as the check decides
// (egresscontrol.Refusal, the platform destinations allowed for every app); the env's running deployment through Canyon,
// its spec's via canyon (one under off calls out directly, and nothing refuses it); and refused since the deploy step
// started that deployment's container, less the promote receipt's margin (egressSeenMargin): a version calls out as it
// starts, before its deploy row's deployed_at, and never during the review and build before it, when the outgoing
// version still runs (#2738). A row from before container_started_at has run_started_at, else deployed_at. A request
// stays pending after the mode lets the name through, or after a deploy of a version that no longer calls it, and is
// then no need. The one predicate behind the refused tile, needs_you and the egress read's refusing.
func refusingNow(ctx context.Context, q querier, ids, platform []string) (map[requestKey]refusingRequest, error) {
	rows, err := q.Query(ctx, `SELECT r.project_id, r.env, r.destination, coalesce(m.mode, s.mode),
		ARRAY(SELECT a.entry FROM egress_allowed a WHERE a.project_id = r.project_id AND a.env = r.env), r.refusals, r.first_refused_at, r.last_refused_at,
		coalesce((SELECT sum(e.calls) FROM egress_destinations e WHERE e.app = r.project_id AND e.env <> r.env AND e.destination = r.destination), 0)::bigint
		FROM egress_requests r JOIN `+runs.ShownDeploys+` d ON d.project_id = r.project_id AND d.env = r.env
		CROSS JOIN egress_settings s LEFT JOIN egress_modes m ON m.project_id = r.project_id AND m.env = r.env
		WHERE r.state = 'pending' AND d.egress->>'via' = 'canyon' AND r.last_refused_at >= coalesce(d.container_started_at, d.run_started_at, d.deployed_at) - make_interval(secs => $2)
		AND r.project_id = ANY($1)`, ids, egressSeenMargin.Seconds())
	if err != nil {
		return nil, err
	}
	out := map[requestKey]refusingRequest{}
	var k requestKey
	var mode string
	var allowed []string
	var r refusingRequest
	_, err = pgx.ForEachRow(rows, []any{&k.app, &k.env, &k.destination, &mode, &allowed, &r.refusals, &r.first, &r.last, &r.otherCalls}, func() error {
		if egresscontrol.Refusal(k.destination, slices.Contains(platform, k.destination), mode, allowed) != "" {
			out[k] = r
		}
		return nil
	})
	return out, err
}
