package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// Platform patches (design/PATCHING.md §4.2, §4.5; design/PERMISSIONS.md, Platform patches; contracts/routes.md,
// the Platform rows): an admin reads every platform identity Canyon patches onto, approves one for production across
// the fleet, and resumes one its breaker paused or stalled; each change from a browser session.
func routesPatching(api *http.ServeMux, a *app) {
	if a.auth != authOIDC { // as the production approvals: with sign-in off nobody can decide a patch approval
		return
	}
	api.HandleFunc("GET /patching/identities", a.adminOnly(a.identities))
	api.HandleFunc("POST /patching/identities/{id}/approve", a.adminOnly(a.approveIdentity))
	api.HandleFunc("POST /patching/identities/{id}/resume", a.adminOnly(a.resumeIdentity))
}

// patchApps is an identity's apps counted by their patch state (patch.schema.json's identity).
type patchApps struct {
	Planned        int `json:"planned"`
	PatchedPreview int `json:"patched_preview"`
	Asked          int `json:"asked"`
	Patched        int `json:"patched"`
	Reverted       int `json:"reverted"`
	Held           int `json:"held"`
	Denied         int `json:"denied"`
}

// tripped is an identity's breaker: when it tripped, and the apps that tripped it.
type tripped struct {
	At       string   `json:"at"`
	Projects []string `json:"projects"`
}

// fleetApproved is the admin's fleet approval of an identity.
type fleetApproved struct {
	By     actor  `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// platformIdentity is one platform identity as the identities read answers it (contracts/routes.md).
type platformIdentity struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"` // rebase, or rebuild (contracts/routes.md, A rebuild patch)
	RunImage  string         `json:"run_image"`
	Builder   string         `json:"builder,omitempty"` // a rebuild identity's builder, as GET /builder named it
	PlannedAt string         `json:"planned_at"`
	Wave      int            `json:"wave"`
	Apps      patchApps      `json:"apps"`
	Paused    *tripped       `json:"paused,omitempty"`
	Stalled   *tripped       `json:"stalled,omitempty"`
	Approved  *fleetApproved `json:"approved,omitempty"`
}

// identitiesSQL reads $1's identity, or every one for "", newest planned first, each app counted by the patch state
// of production where production has one, else of preview (contracts/routes.md, the identities row; design/PATCHING.md
// §0.3): reverted once its watch saw the patched app exit or restart, in either environment; patched while production
// runs the image an approved patch approval of the identity shipped; held while production is held, the identity
// paused, or the patch failed; denied while its team denied the patch approval and production still runs the image it
// replaces; asked while a patch approval of it is open; patched_preview while preview runs the patch; planned while a
// patch is queued, in a wave or running. A rebase refused for good is none of them, and so is an app deleted: its rows
// stay, and its name in paused's projects.
const identitiesSQL = `SELECT i.id, i.kind, i.run_image, coalesce(i.builder, ''), i.planned_at, i.wave, i.paused_at, i.paused_projects, i.stalled_at, i.stalled_projects,
	i.approved_by_kind, i.approved_by_subject, i.approved_by_display, i.approved_at, i.approved_reason,
	count(*) FILTER (WHERE s.state = 'planned'), count(*) FILTER (WHERE s.state = 'patched_preview'), count(*) FILTER (WHERE s.state = 'asked'),
	count(*) FILTER (WHERE s.state = 'patched'), count(*) FILTER (WHERE s.state = 'reverted'), count(*) FILTER (WHERE s.state = 'held'),
	count(*) FILTER (WHERE s.state = 'denied')
FROM patch_identities i LEFT JOIN LATERAL (SELECT CASE
	WHEN a.outcome = 'reverted' THEN 'reverted'
	WHEN EXISTS (SELECT 1 FROM promotion_approvals x JOIN deploys d ON d.project_id = x.project_id AND d.env = 'prod' AND d.image = x.image
		WHERE x.project_id = a.project_id AND x.kind = 'patch' AND x.patch_identity = a.identity AND x.state = 'approved') THEN 'patched'
	WHEN EXISTS (SELECT 1 FROM promotion_approvals x JOIN deploys d ON d.project_id = x.project_id AND d.env = 'prod' AND d.image = x.replaces_image
		WHERE x.project_id = a.project_id AND x.kind = 'patch' AND x.patch_identity = a.identity AND x.state = 'denied') THEN 'denied'
	WHEN i.paused_at IS NOT NULL OR a.outcome IN ('failed', 'failed_start') OR EXISTS (SELECT 1 FROM prod_holds h WHERE h.project_id = a.project_id) THEN 'held'
	WHEN EXISTS (SELECT 1 FROM promotion_approvals x WHERE x.project_id = a.project_id AND x.kind = 'patch' AND x.patch_identity = a.identity
		AND x.state = 'pending' AND x.expires_at > now()) THEN 'asked'
	WHEN a.outcome IN ('running', 'passed') AND EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = a.project_id AND d.env = 'preview' AND d.gate_run = a.run) THEN 'patched_preview'
	WHEN a.outcome IN ('planned', 'running', 'ended_early') THEN 'planned'
	END AS state FROM patch_wave_apps a JOIN projects p ON p.id = a.project_id AND p.deleted_at IS NULL AND p.deleting_since IS NULL
	WHERE a.identity = i.id) s ON true
WHERE $1 = '' OR i.id = $1
GROUP BY i.id ORDER BY i.planned_at DESC, i.id`

// identitiesOf is id's identity, or every one for "", read on q.
func identitiesOf(ctx context.Context, q querier, id string) ([]platformIdentity, error) {
	rows, err := q.Query(ctx, identitiesSQL, id)
	if err != nil {
		return nil, err
	}
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (platformIdentity, error) {
		var out platformIdentity
		var planned time.Time
		var pausedAt, stalledAt, approvedAt *time.Time
		var paused, stalled []string
		var kind, subject, display, reason *string
		err := row.Scan(&out.ID, &out.Kind, &out.RunImage, &out.Builder, &planned, &out.Wave, &pausedAt, &paused, &stalledAt, &stalled, &kind, &subject, &display, &approvedAt, &reason,
			&out.Apps.Planned, &out.Apps.PatchedPreview, &out.Apps.Asked, &out.Apps.Patched, &out.Apps.Reverted, &out.Apps.Held, &out.Apps.Denied)
		out.PlannedAt = at(planned)
		if pausedAt != nil { // the row's checks keep each pair together
			out.Paused = &tripped{At: at(*pausedAt), Projects: paused}
		}
		if stalledAt != nil {
			out.Stalled = &tripped{At: at(*stalledAt), Projects: stalled}
		}
		if approvedAt != nil { // and the approval's five
			out.Approved = &fleetApproved{By: actor{Kind: *kind, Subject: *subject, Display: *display}, At: at(*approvedAt), Reason: *reason}
		}
		return out, err
	})
}

// answerIdentity answers id's identity, 404 for an id no identity has.
func (a *app) answerIdentity(ctx context.Context, w http.ResponseWriter, id string) {
	got, err := identitiesOf(ctx, a.pool, id)
	switch {
	case err != nil:
		a.log.Printf("canyon-server: platform identity %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	case len(got) == 0:
		writeErr(w, http.StatusNotFound, "not_found", noIdentity, nil)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"identity": got[0]})
	}
}

const noIdentity = "no platform identity has that id"

func (a *app) identities(w http.ResponseWriter, r *http.Request) {
	got, err := identitiesOf(r.Context(), a.pool, "")
	var named *namedNow
	if err == nil {
		named, err = a.namedNow(r.Context())
	}
	var said *planSaid
	if err == nil {
		said, err = planOf(r.Context(), a.pool)
	}
	if err != nil {
		a.log.Printf("canyon-server: platform identities: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": got, "run_image": named, "plan": said})
}

// planSaid is the identities read's plan (contracts/routes.md, the identities row): what the newest tick's patch plan
// said, as runs keeps it — why it read no app, or the identity it planned onto and each app's cause.
type planSaid struct {
	At       string      `json:"at"`
	Identity *string     `json:"identity"`
	Why      string      `json:"why,omitempty"`
	Apps     []appCaused `json:"apps"`
}

// appCaused is one app the plan read, with its cause.
type appCaused struct {
	Project string `json:"project"`
	Why     string `json:"why"`
}

// planOf reads the plan, nil before any tick planned; an app deleted, or being deleted, since is left out.
func planOf(ctx context.Context, q querier) (*planSaid, error) {
	var at time.Time
	var out planSaid
	err := q.QueryRow(ctx, `SELECT s.said_at, s.identity, coalesce(s.why, ''), coalesce((SELECT jsonb_agg(jsonb_build_object('project', a.key, 'why', a.value) ORDER BY a.key)
		FROM jsonb_each_text(s.apps) a JOIN projects p ON p.id = a.key AND p.deleted_at IS NULL AND p.deleting_since IS NULL), '[]')
		FROM patch_plan_said s`).Scan(&at, &out.Identity, &out.Why, &out.Apps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.At = at.UTC().Format(time.RFC3339)
	return &out, nil
}

// namedNow is the identities read's run_image (contracts/routes.md): the run image Canyon names now, how many apps
// have an environment that reads wait on the status read, and their ids, sorted.
type namedNow struct {
	Image    string   `json:"image"`
	Behind   int      `json:"behind"`
	Projects []string `json:"projects"`
}

// namedNow reads it, each app as the status read reads it (sayPatch, then sayPatchNow); nil when no tick has read a
// run image or its match does not read, so nothing is read as not behind.
func (a *app) namedNow(ctx context.Context) (*namedNow, error) {
	if a.pipe == nil { // no answer without a look: patching off, by the operator or the org, reads can't tell
		return nil, nil
	}
	if on, err := a.pipe.PatchingOn(ctx, a.pool); err != nil || !on {
		return nil, err
	}
	named, err := a.pipe.NamedRunImage(ctx)
	if err != nil || named.Image == "" || !named.Reads() {
		return nil, err
	}
	rows, err := a.pool.Query(ctx, `SELECT d.project_id, d.env, split_part(d.image, '@', 2) FROM deploys d JOIN projects p ON p.id = d.project_id AND p.deleted_at IS NULL
		WHERE d.run_by = 'canyon' ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	envs := map[string]map[string]any{}
	var id, env, digest string
	if _, err := pgx.ForEachRow(rows, []any{&id, &env, &digest}, func() error {
		if envs[id] == nil {
			envs[id] = map[string]any{}
		}
		envs[id][env] = map[string]any{"run_by": "canyon", "image_digest": digest}
		return nil
	}); err != nil {
		return nil, err
	}
	facts, err := a.sayPatch(ctx, a.pool, envs)
	if err != nil {
		return nil, err
	}
	out := &namedNow{Image: named.Image, Projects: []string{}}
	for id, project := range envs {
		if err := a.sayPatchNow(ctx, id, project, facts[id]); err != nil {
			return nil, err
		}
		state := func(env string) string {
			m, _ := project[env].(map[string]any)
			p, _ := m["patch"].(envPatch)
			return p.State
		}
		switch {
		case state("preview") == "could_not_rescan", state("prod") == "could_not_rescan": // an app Canyon could not look at: no count is said
			return nil, nil
		case state("preview") == "wait", state("prod") == "wait":
			out.Behind++
			out.Projects = append(out.Projects, id)
		}
	}
	slices.Sort(out.Projects)
	return out, nil
}

// errNoIdentity is an id no identity has: the 404.
var errNoIdentity = errors.New("no such identity")

// approveIdentity is the fleet approval (contracts/routes.md): an admin, in the browser, approves one identity for
// production across the fleet, and its open patch approvals are decided at once, each later one as it comes due
// (runs' DecideFleet). Approving an approved identity answers it and writes nothing.
func (a *app) approveIdentity(w http.ResponseWriter, r *http.Request) {
	if !inDashboard(w, r) { // before the body is read
		return
	}
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: approve platform identity %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	var kind string
	err := a.pool.QueryRow(ctx, "SELECT kind FROM patch_identities WHERE id = $1", id).Scan(&kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", noIdentity, nil)
		return
	case err != nil:
		internal(err)
		return
	case kind == "rebuild": // a rebuild is always its app's team's to decide (contracts/routes.md, A rebuild patch)
		writeErr(w, http.StatusConflict, "always_asked", "Canyon asks each app's team before a rebuild goes to production", nil)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !plainText(body.Reason, 1024) {
		writeErr(w, http.StatusBadRequest, "invalid_reason", "reason is 1 to 1024 characters of plain text", nil)
		return
	}
	approved := false
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		var runImage string
		var already bool
		err := tx.QueryRow(ctx, "SELECT run_image, approved_at IS NOT NULL FROM patch_identities WHERE id = $1 FOR UPDATE", id).Scan(&runImage, &already)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoIdentity
		}
		if err != nil || already {
			return err
		}
		if a.ee.patching == nil { // a server built without Canyon Enterprise approves nothing for the fleet (contracts/routes.md, Licence)
			return licenceErr{Feature: platformPatching}
		}
		if err := a.ee.patching.Approve(ctx, tx, id, act, addressOf(ctx), body.Reason); err != nil {
			return err
		}
		approved = true
		return events.AuditTx(ctx, tx, act, "approve patch identity", "", "", map[string]any{"identity": id, "run_image": runImage, "reason": body.Reason})
	})
	var lic licenceErr
	switch {
	case errors.Is(err, errNoIdentity):
		writeErr(w, http.StatusNotFound, "not_found", noIdentity, nil)
		return
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
		return
	case err != nil:
		internal(err)
		return
	}
	if approved { // now: each open patch approval of it, on a context of Canyon's own, as a tick decides them
		if _, err := a.ee.patching.DecideFleet(context.Background(), a.pipe, id); err != nil {
			a.log.Printf("canyon-server: the fleet approval of %s, decided again at the next tick: %v", id, err)
		}
	}
	a.answerIdentity(ctx, w, id)
}

// resumeIdentity resumes an identity its breaker paused or stalled (contracts/routes.md): the apps that reverted
// stay out of it, its waves go on from the one it stopped in, and its breaker counts afresh.
func (a *app) resumeIdentity(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	if !fromDashboard(ctx) {
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		deny(ctx, "dashboard_required", "")
		return
	}
	errNotPaused := errors.New("not paused")
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		var runImage string
		var stopped bool
		err := tx.QueryRow(ctx, "SELECT run_image, paused_at IS NOT NULL OR stalled_at IS NOT NULL FROM patch_identities WHERE id = $1 FOR UPDATE", id).Scan(&runImage, &stopped)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNoIdentity
		case err != nil:
			return err
		case !stopped:
			return errNotPaused
		case a.ee.patching == nil: // a server built without Canyon Enterprise resumes nothing (contracts/routes.md, Licence)
			return licenceErr{Feature: platformPatching}
		}
		if err := a.ee.patching.Resume(ctx, tx, id); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, act, "resume patching", "", "", map[string]any{"identity": id, "run_image": runImage})
	})
	var lic licenceErr
	switch {
	case errors.Is(err, errNoIdentity):
		writeErr(w, http.StatusNotFound, "not_found", noIdentity, nil)
	case errors.As(err, &lic):
		writeLicenceRequired(w, lic)
	case errors.Is(err, errNotPaused):
		writeErr(w, http.StatusConflict, "not_paused", "this platform identity is neither paused nor stalled", nil)
	case err != nil:
		a.log.Printf("canyon-server: resume platform identity %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		a.answerIdentity(ctx, w, id)
	}
}

// fleetKey marks a decision the fleet approval makes: the admin decided it in the browser when they approved its
// identity, and its audit row names the identity (contracts/routes.md, Production approvals).
type fleetKey struct{}

// fleetDecides is whether ctx carries a fleet approval's decision.
func fleetDecides(ctx context.Context) bool {
	return ctx.Value(fleetKey{}) != nil
}

// decision keeps the approve route's answer to a fleet approval's decision: its status, never sent.
type decision struct {
	header http.Header
	status int
}

func (d *decision) Header() http.Header { return d.header }

func (d *decision) WriteHeader(status int) {
	if d.status == 0 {
		d.status = status
	}
}

func (d *decision) Write(b []byte) (int, error) {
	d.WriteHeader(http.StatusOK)
	return len(b), nil
}

// decidePatch is runs' DecidePatch: the approve route for the patch approval code, asked by the admin who approved its
// identity for the fleet, with no promote_anyway — no browser showed preview's answer — on a context of Canyon's own,
// via canyon. Only while that person is still an admin (stillAdmin), else runs.ErrApproverGone and nothing is decided.
// It answers whether the approval now reads approved by them with its run, read again after the route answered: a
// refusal, or any answer but that, leaves it pending for the app's team.
func (a *app) decidePatch(ctx context.Context, code string, by actor) (bool, error) {
	if ok, err := a.stillAdmin(ctx, code, by); err != nil || !ok {
		return false, cmp.Or(err, runs.ErrApproverGone)
	}
	ctx = context.WithValue(context.WithValue(context.WithValue(ctx, actorKey{}, by), roleKey{}, "admin"), fleetKey{}, true)
	ctx = events.WithProvenance(ctx, events.Provenance{Via: "canyon"})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/promotion-approvals/"+code+"/approve", http.NoBody)
	if err != nil {
		return false, err
	}
	r.SetPathValue("code", code)
	w := &decision{header: http.Header{}}
	if ap, ok := a.approvalOf(w, r); ok && ap.patch != nil {
		a.promoteRun(w, r, ap.project, ap.gateRun, &ap, false)
	}
	if w.status >= http.StatusInternalServerError {
		return false, fmt.Errorf("the approve route answered %d", w.status)
	}
	var approved bool
	err = a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM promotion_approvals WHERE code = $1 AND kind = 'patch' AND state = 'approved' AND run_id IS NOT NULL
		AND decided_by_kind = $2 AND decided_by_subject = $3)`, code, by.Kind, by.Subject).Scan(&approved)
	return approved, err
}

// stillAdmin is whether the person who approved code's identity for the fleet is an admin now, as a request of theirs
// would be judged (session.go's member and role), from the address their session carried when they approved: one
// recorded, a member of the org, and on the admin list, compared case-insensitively, or holding a live sign-in under
// that address and the current issuer, as a session is read (#2306), that claimed admin while the org still names an
// admin claim. Fail closed: what cannot be read, or reads otherwise, is not an admin.
func (a *app) stillAdmin(ctx context.Context, code string, by actor) (bool, error) {
	if by.Kind != "person" || a.oidc == nil {
		return false, nil
	}
	var email string
	var claimed bool
	err := a.pool.QueryRow(ctx, `SELECT i.approved_by_email, EXISTS (SELECT 1 FROM sessions s WHERE s.actor_kind = 'person' AND s.actor_subject = $2
		AND lower(s.email) = lower(i.approved_by_email) AND s.claimed_admin AND (s.issuer = $3 OR s.issuer = '') AND s.revoked_at IS NULL AND s.expires_at > now())
		FROM promotion_approvals x JOIN patch_identities i ON i.id = x.patch_identity
		WHERE x.code = $1 AND i.approved_by_kind = 'person' AND i.approved_by_subject = $2`, code, by.Subject, a.oidc.issuer).Scan(&email, &claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	o, err := a.orgOf(ctx, a.pool)
	if err != nil {
		return false, err
	}
	claimed = claimed && o.adminClaim != ""
	return strings.TrimSpace(email) != "" && a.member(email, o) && (claimed || slices.Contains(o.admins, strings.ToLower(email))), nil
}
