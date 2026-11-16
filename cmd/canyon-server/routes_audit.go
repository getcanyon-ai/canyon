package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// auditEvent is the trail's element on the wire (contracts/routes.md); ts is whole seconds in UTC.
type auditEvent struct {
	ID       string          `json:"id"`
	TS       string          `json:"ts"`
	Actor    actor           `json:"actor"`
	Action   string          `json:"action"`
	Category string          `json:"category"`
	Via      *string         `json:"via,omitempty"`     // how it was done; none on a row written before it was recorded
	Role     *string         `json:"role,omitempty"`    // the person's role when a session of theirs acted
	Session  *string         `json:"session,omitempty"` // the session's digest, to an admin alone
	Source   *string         `json:"source,omitempty"`  // the client address, to an admin alone
	Project  *string         `json:"project,omitempty"`
	Run      *string         `json:"run,omitempty"`
	Detail   json.RawMessage `json:"detail"`
}

// categories is each action outside governance by its category (contracts/routes.md, Audit categories):
// every other action, a new one included, is governance.
var categories = map[string]string{
	"sign in": "access", "sign out": "access", "GET /api/v1/projects/{id}/logs": "access", "sign in refused": "access", "denied": "access",
	"verdict": "outcome", "build failed": "outcome", "detect failed": "outcome", "start failed": "outcome",
	"build had no room": "outcome", "ensure resource": "outcome", "deploy preview": "outcome",
	"patch preview": "outcome", "revert patch preview": "outcome",
	"platform reported deployed": "outcome", "platform reported removed": "outcome", "release live by check-in": "outcome", "release removed by commit": "outcome", "commit release": "outcome", "update sidecar": "outcome", "egress.request": "outcome", "integration.channel.failing": "outcome",
	"db session opened": "access", "db session attached": "access", "db session closed": "access",
}

func categoryOf(action string) string {
	if c, ok := categories[action]; ok {
		return c
	}
	return "governance"
}

// categoryActions is the actions filter categories= reads as: the actions of the chosen categories
// (in), or, with governance chosen, those of the categories not chosen (out); both nil when none is chosen.
func categoryActions(chosen []string) (in, out []string, ok bool) {
	picked := map[string]bool{}
	for _, c := range chosen {
		if c != "governance" && c != "access" && c != "outcome" {
			return nil, nil, false
		}
		picked[c] = true
	}
	if len(picked) == 0 {
		return nil, nil, true
	}
	in, out = []string{}, []string{}
	for action, c := range categories {
		if picked[c] {
			in = append(in, action)
		} else {
			out = append(out, action)
		}
	}
	if picked["governance"] {
		return nil, out, true
	}
	return in, nil, true
}

// commaList reads a comma-separated filter: empty is no filter; each item 1–size characters, at most most of them.
func commaList(v string, most, size int) ([]string, bool) {
	if v == "" {
		return nil, true
	}
	items := strings.Split(v, ",")
	if len(items) > most {
		return nil, false
	}
	for _, item := range items {
		if item == "" || !filterText(item, size) {
			return nil, false
		}
	}
	return items, true
}

// filterText is a filter value Postgres can compare: valid UTF-8, no NUL, at most size characters.
func filterText(v string, size int) bool {
	return utf8.ValidString(v) && !strings.ContainsRune(v, 0) && utf8.RuneCountInString(v) <= size
}

func routesAudit(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /audit", a.listAudit)
}

// auditWhere narrows the trail by actor kind, an inclusive since, a project and the changes to one rule ($4), to the
// apps in $5 and, of the rows naming no app, the caller's own sign-ins, sign-outs and refusals, and on any app the stops
// and lifts they made (a responder's, design/PERMISSIONS.md), a person's whose subject is $12 (every row when $5 is null),
// an app's access requests, grants and refusals naming it only when the caller owns it, $13 (design/PERMISSIONS.md,
// The org's own records); and by a category's actions in ($6) or out ($7), the actions named ($8), an actor
// subject ($9), an exclusive until ($10) and a channel ($11).
const auditWhere = `
WHERE ($1::text = '' OR actor_kind = $1) AND ($2::timestamptz IS NULL OR ts >= $2) AND ($3::text = '' OR project_id = $3)
  AND ($4::text = '' OR (action IN ('policy.set', 'custom_rule.set', 'app_policy.set', 'app_policy.remove') AND detail->>'rule' = $4))
  AND ($5::text[] IS NULL OR project_id = ANY($5)
    OR project_id IS NULL AND action IN ('sign in', 'sign out', 'denied', 'sign in refused') AND actor_kind = 'person' AND actor_subject = $12
    OR action IN ('stop app', 'lift hold') AND actor_kind = 'person' AND actor_subject = $12)
  AND ($5::text[] IS NULL OR action NOT IN ('request access', 'dismiss access request', 'share app', 'unshare app', 'denied')
    OR project_id IS NULL AND action = 'denied' OR project_id = ANY($13))
  AND ($6::text[] IS NULL OR action = ANY($6)) AND ($7::text[] IS NULL OR NOT action = ANY($7))
  AND ($8::text[] IS NULL OR action = ANY($8)) AND ($9::text = '' OR actor_subject = $9)
  AND ($10::timestamptz IS NULL OR ts < $10) AND ($11::text = '' OR via = $11)`

// auditPageSQL is one page of the trail, newest first, from the cursor (an empty id, $15, is the start) and $17 rows past it,
// one row past the limit. A row naming a labelled package carries its label, so a deploy's or a review's row names the
// version as its upload's does (#967). It is ordered by the 0012 page index; a rule's view walks it, since a rule has few changes.
const auditPageSQL = `SELECT id, ts, actor_kind, actor_subject, actor_display, action, via, role, session, source, project_id, run_id,
  detail || COALESCE((SELECT jsonb_build_object('label', p.label) FROM packages p
    WHERE NOT detail ? 'label' AND p.id = detail->>'package' AND p.project_id = audit_events.project_id AND p.label IS NOT NULL), '{}')
FROM audit_events` + auditWhere + `
  AND ($15::text = '' OR (ts, id) < ($14::timestamptz, $15))
ORDER BY ts DESC, id DESC LIMIT $16 OFFSET $17`

// auditCountSQL is how many rows the filters select, across every page (contracts/routes.md, the audit read: total).
const auditCountSQL = `SELECT count(*) FROM audit_events` + auditWhere

// listAudit answers one page of the trail, the latest change first: to a member their team apps' rows and
// their own sign-ins, sign-outs and refusals (contracts/routes.md, the audit read). A project that matches nothing is an empty
// page, never a 404: a deleted project's rows stay reachable here. Naming an app the member is below
// contributor on is team_required, before any row is read.
func (a *app) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("actor")
	switch kind {
	case "", "person", "agent", "canyon":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_actor", "actor is one of person, agent, canyon", nil)
		return
	}
	var since *time.Time
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_since", "since is an RFC 3339 timestamp", nil)
			return
		}
		since = &t
	}
	var until *time.Time
	if s := q.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_until", "until is an RFC 3339 timestamp", nil)
			return
		}
		until = &t
	}
	subject := q.Get("subject")
	if !filterText(subject, 320) {
		writeErr(w, http.StatusBadRequest, "invalid_subject", "subject is at most 320 characters of text", nil)
		return
	}
	chosen, ok := commaList(q.Get("category"), 3, 10)
	in, out, known := categoryActions(chosen)
	if !ok || !known {
		writeErr(w, http.StatusBadRequest, "invalid_category", "category is a list of governance, access and outcome", nil)
		return
	}
	actions, ok := commaList(q.Get("action"), 64, 200)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_action", "action is a list of 1 to 64 actions, each 1 to 200 characters", nil)
		return
	}
	via := q.Get("via")
	if via != "" && !slices.Contains([]string{"dashboard", "cli", "github", "platform", "canyon"}, via) {
		writeErr(w, http.StatusBadRequest, "invalid_via", "via is one of dashboard, cli, github, platform, canyon", nil)
		return
	}
	asked, ok := pageNumber(w, q)
	if !ok {
		return
	}
	after, limit, ok := pageQuery(w, q)
	if !ok {
		return
	}
	ctx, project := r.Context(), q.Get("project")
	if project != "" {
		var known bool // carried by a project, live or deleted: an id no project has carried is an empty page
		if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)", project).Scan(&known); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if known && a.teamRequired(ctx, w, a.pool, project) {
			return
		}
	}
	apps, err := teamApps(ctx, a.pool)
	var owned []string // the apps whose access requests, grants and refusals the member reads (design/PERMISSIONS.md, The org's own records)
	if err == nil {
		owned, err = appsAt(ctx, a.pool, "owner")
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	filters := []any{kind, since, project, q.Get("rule"), apps, in, out, actions, subject, until, via, actorOf(ctx).Subject, owned}
	var total, offset int
	if err := a.pool.QueryRow(ctx, auditCountSQL, filters...).Scan(&total); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	var page any // null on a read by cursor
	if q.Get("cursor") == "" {
		n := servedPage(asked, total, limit)
		page, offset = n, (n-1)*limit
	}
	rows, err := a.pool.Query(ctx, auditPageSQL, append(filters, after.ts, after.id, limit+1, offset)...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer rows.Close()
	events, keys := []auditEvent{}, []pageCursor{}
	for rows.Next() {
		var e auditEvent
		var ts time.Time
		if err := rows.Scan(&e.ID, &ts, &e.Actor.Kind, &e.Actor.Subject, &e.Actor.Display, &e.Action, &e.Via, &e.Role, &e.Session, &e.Source, &e.Project, &e.Run, &e.Detail); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		e.TS, e.Category = ts.UTC().Format(time.RFC3339), categoryOf(e.Action)
		if roleOf(ctx) != "admin" { // which session and from where are an admin's to read
			e.Session, e.Source = nil, nil
		}
		events, keys = append(events, e), append(keys, pageCursor{ts, e.ID})
	}
	if rows.Err() != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	events, next := trim(events, keys, limit)
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "total": total, "page": page, "next": next})
}
