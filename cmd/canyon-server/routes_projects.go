package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/internal/names"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// project is the response shape of contracts/routes.md's projects group. owners[] elements are
// the actor triple; envs is empty until deploys exist; stack is empty until detection lands. Whether
// the caller reads it whole rides unanswered: below contributor they are answered reduced() instead.
type project struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Owners      []actor         `json:"owners"`
	OwnerGroups []string        `json:"owner_groups"` // the groups holding a grant at owner, sorted: whom to ask where no person owns it
	Summary     string          `json:"summary"`      // the current package's description, "" when it has none
	Claimable   bool            `json:"claimable"`    // whether this caller's claim would be accepted now: the dashboard offers Claim by it
	Access      appAccess       `json:"access"`
	Stack       string          `json:"stack"`
	GateState   string          `json:"gate_state"`
	Envs        map[string]any  `json:"envs"`
	Latest      latest          `json:"latest"`
	Hold        json.RawMessage `json:"hold,omitempty"` // production's hold, while it is held (contracts/routes.md, The hold)
	team        bool
}

// appAccess is the caller's own level on the app and who it is shared with (contracts/routes.md, the
// project read): private exactly when everyone is false and people and groups are 0. Requests is the
// count of its pending access requests, answered to its owners and admins only.
type appAccess struct {
	Level    string `json:"level"`
	Everyone bool   `json:"everyone"`
	People   int    `json:"people"`
	Groups   int    `json:"groups"`
	Requests *int   `json:"requests,omitempty"`
}

// reducedRow is an app as someone below contributor reads it, on this read, the list and the fleet
// (contracts/routes.md, App teams): its name, owners and summary, their own level, and nothing else —
// but a viewer's adds envs, where the app is live, so they can open it.
type reducedRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Owners      []actor  `json:"owners"`
	OwnerGroups []string `json:"owner_groups"`
	Summary     string   `json:"summary"`
	Claimable   bool     `json:"claimable"`
	Access      struct {
		Level string `json:"level"`
	} `json:"access"`
	Envs  *viewerEnvs     `json:"envs,omitempty"`  // a viewer's and a responder's alone: nil at none, so the key is absent
	Hold  json.RawMessage `json:"hold,omitempty"`  // a responder's alone (contracts/routes.md, The hold)
	Needs []need          `json:"needs,omitempty"` // a responder's held need, on the fleet alone (contracts/routes.md, What needs a person)
}

// viewerEnvs is what a viewer reads of the app's environments: each a url and, on one Canyon runs, its
// state, or, on one the org runs, when a pod last checked in (#2452) — never what it runs, from which run
// or who put it there.
type viewerEnvs struct {
	Preview *viewerEnv `json:"preview,omitempty"`
	Prod    *viewerEnv `json:"prod,omitempty"`
}

type viewerEnv struct {
	URL    string `json:"url"`
	State  string `json:"state,omitempty"`
	SeenAt string `json:"seen_at,omitempty"`
}

// reduced is the row below contributor. A responder's adds production's address and state, and its hold
// (design/PERMISSIONS.md, Security responders): nothing of preview but a viewer's. Anyone else reads a held
// production as empty.
func (p project) reduced(responder bool) reducedRow {
	r := reducedRow{ID: p.ID, Name: p.Name, Owners: p.Owners, OwnerGroups: p.OwnerGroups, Summary: p.Summary, Claimable: p.Claimable}
	r.Access.Level = p.Access.Level
	if p.Access.Level == "viewer" {
		r.Envs = &viewerEnvs{Preview: viewerEnvOf(p.Envs["preview"]), Prod: viewerEnvOf(p.Envs["prod"])}
		if p.Hold != nil && !responder { // a held production reads as one with nothing deployed to whoever cannot read its hold, a row the org runs or a stop that failed among it
			r.Envs.Prod = nil
		}
	}
	if responder {
		if prod := viewerEnvOf(p.Envs["prod"]); r.Envs == nil && prod != nil {
			r.Envs = &viewerEnvs{Prod: prod}
		}
		r.Hold = p.Hold
	}
	return r
}

// viewerEnvOf is one environment as a viewer reads it: there when something is deployed there with a
// url, whoever runs it; nil otherwise, so one waiting on its first release ({run_by, pending}, no url)
// says nothing, and nothing is ever {url: ""}.
func viewerEnvOf(e any) *viewerEnv {
	m, _ := e.(map[string]any)
	url, deployed := m["url"].(string)
	if !deployed || url == "" {
		return nil
	}
	state, _ := m["state"].(string)
	seen, _ := m["seen_at"].(string)
	return &viewerEnv{URL: url, State: state, SeenAt: seen}
}

// routesProjects registers the projects group on the /api/v1 mux. Each state-changing handler
// writes its audit row inside its own transaction (events.AuditTx); the reads write none.
func routesProjects(api *http.ServeMux, a *app) {
	api.HandleFunc("POST /projects", a.createProject)
	api.HandleFunc("GET /projects", a.listProjects)
	api.HandleFunc("GET /projects/{id}", a.getProject)
	api.HandleFunc("PUT /projects/{id}/owner", a.claimProject)
	api.HandleFunc("PUT /projects/{id}/name", a.ownerOnly(a.renameProject))
	api.HandleFunc("DELETE /projects/{id}", a.deleteProject)
	api.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { // everything unrouted under /api/v1 answers the contract's shape too
		writeErr(w, http.StatusNotFound, "not_found", "no such route", nil)
	})
}

// createProject makes an app (contracts/routes.md, POST /projects; #1911): {name} is a display name,
// and the host picks the id, never one any project has had; {id} is exactly that id, as canyon.toml or
// --app names it. The id names a directory, so it is a project id in the seam's sense either way.
func (a *app) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	switch {
	case (body.Name == "") == (body.ID == ""):
		writeErr(w, http.StatusBadRequest, "bad_request", "a new app takes a name, or an id, and not both", nil)
		return
	case body.ID != "" && (!runs.ValidProject.MatchString(body.ID) || len(body.ID) > 55): // 55: the container <id>-preview is a DNS label too
		writeErr(w, http.StatusBadRequest, "invalid_id", "an app id is lowercase letters, digits and dashes, starting with a letter and not ending with a dash, at most 55 characters", nil)
		return
	case body.ID == "" && !plainText(body.Name, 100):
		writeErr(w, http.StatusBadRequest, "invalid_name", "an app's name is one line of plain text, at most 100 characters", nil)
		return
	}
	ctx := r.Context()
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, made, err := insertProject(ctx, tx, body.ID, body.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !made {
		var deleted bool // a deleted project keeps its name and its ledger, and every route answers it 404: never "exists" (#864)
		// read on the transaction's own connection: a second one, asked of the pool while this holds the
		// ledger lock, deadlocks once every connection waits on that lock (#1693)
		if err := tx.QueryRow(ctx, "SELECT deleted_at IS NOT NULL FROM projects WHERE id = $1", id).Scan(&deleted); err == nil && deleted {
			writeErr(w, http.StatusConflict, "deleted", "An app named "+id+" was deleted, and its name is not reused: name the app something else", nil)
			return
		}
		writeErr(w, http.StatusConflict, "exists", "a project with that name exists", nil)
		return
	}
	act := actorOf(ctx)
	if address := addressOf(ctx); address != "" { // the creator is its owner (contracts/routes.md, App teams); a host without sign-in has no address, and nobody is its owner
		if _, err := tx.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, subject, subject_issuer, display, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ($1, 'email', $2, 'owner', $6, $7, $4, $5, $3, $4)`, id, address, act.Subject, act.Display, act.Kind, a.boundSubject(ctx), a.boundIssuer(ctx)); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	if err := events.AuditTx(ctx, tx, act, action(r), id, "", nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	p, err := project1(ctx, tx, id) // read inside the transaction: nothing is durable until the commit answers
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": p})
}

// renameProject gives the app another display name (contracts/routes.md, PUT …/name; #1909): only
// projects.name changes, never the id, which stays its address, its audit subject and every route's :id.
// One audit row when the name changes; the answer is the single read.
func (a *app) renameProject(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !plainText(body.Name, 100) {
		writeErr(w, http.StatusBadRequest, "invalid_name", "an app's name is one line of plain text, at most 100 characters", nil)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "rename", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var from string
	err = tx.QueryRow(ctx, "SELECT name FROM projects WHERE id = $1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if err == nil && from != body.Name {
		if _, err = tx.Exec(ctx, "UPDATE projects SET name = $2 WHERE id = $1", id, body.Name); err == nil {
			err = events.AuditTx(ctx, tx, actorOf(ctx), "rename app", id, "", map[string]any{"from": from, "to": body.Name})
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "rename", err)
		return
	}
	a.getProject(w, r)
}

// pickAppID is a test's seam: names.NewApp unless a test makes the picks collide.
var pickAppID = names.NewApp

// insertProject writes the project row: exactly id, named for itself, or else one the host picks under
// name. made is false only for an id that is taken; a picked id that is taken, by a live app or a
// deleted one, is picked again. A conflict leaves the transaction usable.
func insertProject(ctx context.Context, tx pgx.Tx, id, name string) (string, bool, error) {
	if id != "" {
		tag, err := tx.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $1) ON CONFLICT DO NOTHING", id)
		return id, err == nil && tag.RowsAffected() == 1, err
	}
	for range 8 { // 64 × 62 × 36³ ids: a ninth pick in a row taken is a host that cannot make one
		id = pickAppID()
		tag, err := tx.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING", id, name)
		if err != nil || tag.RowsAffected() == 1 {
			return id, err == nil, err
		}
	}
	return "", false, errors.New("eight picked app ids in a row were taken")
}

func (a *app) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := projects(r.Context(), a.pool, "")
	team := map[string]map[string]any{} // the patch state is technical detail: rows read whole alone
	for _, p := range ps {
		if p.team {
			team[p.ID] = p.Envs
		}
	}
	if err == nil {
		_, err = a.sayPatch(r.Context(), a.pool, team)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	rows := make([]any, len(ps))
	for i, p := range ps {
		rows[i] = p
		if !p.team {
			rows[i] = p.reduced(roleOf(r.Context()) == "responder")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": rows})
}

func (a *app) getProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := project1(ctx, a.pool, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(r.PathValue("id")), nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !p.team {
		responder := roleOf(ctx) == "responder"
		switch { // a viewer's row says whether each environment runs, a responder's whether production does, as the fleet's
		case p.Access.Level == "viewer":
			a.sayRunning(ctx, map[string]map[string]any{p.ID: p.Envs}, false, false)
		case responder:
			a.sayRunning(ctx, map[string]map[string]any{p.ID: {"prod": p.Envs["prod"]}}, false, false)
		}
		writeJSON(w, http.StatusOK, map[string]any{"project": p.reduced(responder)})
		return
	}
	_, err = a.patchFactsOne(ctx, p.Envs, p.ID)
	var resources []resourceRow
	if err == nil {
		resources, err = resourcesOf(ctx, a.pool, p.ID)
	}
	var link *repoLink
	var auto bool
	if err == nil {
		link, err = linkOf(ctx, a.pool, p.ID)
	}
	if err == nil {
		err = a.pool.QueryRow(ctx, "SELECT auto_deploy FROM projects WHERE id = $1", p.ID).Scan(&auto)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": struct { // the single read alone carries resources[], auto_deploy and repository: the list and the fleet row do not
		project
		Resources  []resourceRow `json:"resources"`
		AutoDeploy bool          `json:"auto_deploy"`
		Repository *repoLink     `json:"repository"`
	}{p, resources, auto, link}})
}

// openToClaim is whether anyone may claim the app p (contracts/routes.md, PUT …/owner): no grant at owner
// of any kind, no secret, nothing deployed, no hold on its production (contracts/routes.md, The hold), no
// repository link and no package from GitHub, linked since or not — a repository's app is an admin's to adopt (#1917). The claim and every read's claimable test this one expression.
const openToClaim = `(NOT EXISTS (SELECT 1 FROM project_access WHERE project_id = p.id AND level = 'owner') AND NOT EXISTS (SELECT 1 FROM secrets WHERE project_id = p.id)
	AND NOT EXISTS (SELECT 1 FROM deploys WHERE project_id = p.id) AND NOT EXISTS (SELECT 1 FROM prod_holds WHERE project_id = p.id)
	AND NOT EXISTS (SELECT 1 FROM project_repositories WHERE project_id = p.id)
	AND NOT EXISTS (SELECT 1 FROM packages WHERE project_id = p.id AND source_kind IN ('repo', 'repo-pr')))`

// claimProject makes the caller an owner under the given display; claiming again renames them and
// removes nobody (contracts/routes.md). Anyone may claim an app openToClaim; any other app is its
// owners' and admins' to claim, so a claim never hands over live credentials, a running service or a
// repository's code.
func (a *app) claimProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Display string `json:"display"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if n := utf8.RuneCountInString(body.Display); n == 0 || n > 128 {
		writeErr(w, http.StatusBadRequest, "invalid_display", "a display name is 1 to 128 characters", nil)
		return
	}
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live, open bool // read under the ledger lock a secret's write and a deploy's take, so neither lands between this and the grant
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL), `+openToClaim+` FROM (SELECT $1::text AS id) p`, id).Scan(&live, &open); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !live {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if !open && a.ownerRequired(ctx, w, tx, id) {
		return
	}
	grantee := addressOf(ctx)
	if grantee == "" { // a host without sign-in has no address: the anonymous admin's claim is held under its subject, as the owner row was
		grantee = act.Subject
	}
	var bound, issuer string // a grant at this address bound to another person is theirs: a claim never promotes it, nor one bound under another issuer, whose subject may be anyone's here — its person's sign-in binds it again first
	if err := tx.QueryRow(ctx, "SELECT subject, subject_issuer FROM project_access WHERE project_id = $1 AND kind = 'email' AND grantee = $2", id, grantee).Scan(&bound, &issuer); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if bound != "" && (bound != act.Subject || cmp.Or(issuer, issuerOf(ctx)) != issuerOf(ctx)) { // '' is a binary's that recorded no issuer: this one's
		writeErr(w, http.StatusConflict, "grant_bound", "the grant to this address names another person: an owner or an admin shares it to you", nil)
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, subject, subject_issuer, display, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ($1, 'email', $2, 'owner', $6, $7, $4, $5, $3, $4)
		ON CONFLICT (project_id, kind, grantee) DO UPDATE SET level = 'owner', display = EXCLUDED.display,
			subject = CASE WHEN project_access.subject = '' THEN EXCLUDED.subject ELSE project_access.subject END,
			subject_issuer = CASE WHEN project_access.subject = '' THEN EXCLUDED.subject_issuer ELSE project_access.subject_issuer END`, id, grantee, act.Subject, body.Display, act.Kind, a.boundSubject(ctx), a.boundIssuer(ctx)); err != nil { // a bound grant keeps its subject
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err := noteBinding(ctx, tx, a.boundSubject(ctx), grantee, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err := events.AuditTx(ctx, tx, act, action(r), id, "", map[string]any{"display": body.Display}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	p, err := project1(ctx, tx, id)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil {
		_, _, err = a.reput(ctx, id) // as a share: the running app's file names the new owner
	}
	if err != nil {
		a.accessFailed(w, "claim "+id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"owners": p.Owners})
}

// deleteProject destroys the project's resources and data, then hides it; ledger and runs stay (contracts/routes.md).
// Without ?confirm=<the project id> it lists what would go. With it, the project is marked deleting first (no run starts,
// no promotion lands, until the delete finishes), containers stop and deploys go, every record is marked deleting and goes,
// audited, only once its driver answers gone: failed or pending keeps it, the secrets and the project, and a retry resumes.
// Last the executor forgets the app's images and build caches; a failure there keeps the project too.
func (a *app) deleteProject(w http.ResponseWriter, r *http.Request) {
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	internal := func(err error) {
		a.log.Printf("canyon-server: delete %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
	if !a.projectLive(ctx, w, id) || a.ownerRequired(ctx, w, a.pool, id) {
		return
	}
	var prod bool // prod is live while it is deployed or holds data; the org's platform's is read with the mark, below
	if err := a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'prod') OR EXISTS (SELECT 1 FROM resources WHERE project_id = $1 AND env = 'prod')
		OR EXISTS (SELECT 1 FROM secrets WHERE project_id = $1 AND env = 'prod')`, id).Scan(&prod); err != nil {
		internal(err)
		return
	}
	if prod && a.personRequired(ctx, w, act, id, "only a person can take production down") {
		return
	}
	records, err := recordsOf(ctx, a.pool, id)
	if err != nil {
		internal(err)
		return
	}
	if r.URL.Query().Get("confirm") == id {
		a.recheckRepoRemoves(ctx, id, "") // a delete asked again asks again of its removes' pull requests (#2470)
	}
	if r.URL.Query().Get("confirm") != id {
		provided, err := providedOf(ctx, a.pool, id) // the platform's, which its delete's remove asks it to delete (#2432)
		if err != nil {
			internal(err)
			return
		}
		writeErr(w, http.StatusConflict, "confirm_required", "deleting a project destroys its resources and their data immediately: send ?confirm=<project id>", map[string]any{"resources": append(records, provided...)})
		return
	}
	person, err := a.personNeeded(ctx, act)
	if err != nil {
		internal(err)
		return
	}
	mark, err := a.mutation(ctx) // the ledger lock a run's start and a promotion's write take: each is before the mark or refused
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = mark.Rollback(ctx) }()
	held, err := runs.ReadHold(ctx, mark, id) // the app is evidence until its hold is lifted (contracts/routes.md, The hold)
	if err != nil {
		internal(err)
		return
	}
	if held != nil {
		a.refuseHeld(ctx, w, mark, *held)
		return
	}
	patch, err := runs.EndPatchTx(ctx, mark, id) // a person's request goes first: a patch run before its /run is ended (contracts/routes.md, Patch runs)
	if err != nil {
		internal(err)
		return
	}
	if patch == "" { // after its /run, the delete ends its watch clean: it takes the environment watched offline
		if patch, err = runs.EndWatchTx(ctx, mark, id, act); err != nil {
			internal(err)
			return
		}
	}
	if a.runInProgress(ctx, w, mark, id) {
		return
	}
	// Where the org's platform may hold the app's data (#2432), read once, with the mark and the project's row
	// locked: the checks here and every remove below go by this one set.
	if _, err := mark.Exec(ctx, "SELECT 1 FROM projects WHERE id = $1 FOR NO KEY UPDATE", id); err != nil {
		internal(err)
		return
	}
	targets, err := deleteTargets(ctx, mark, id)
	if err != nil {
		internal(err)
		return
	}
	// Each target's remove goes the way its last deploy went (#2470): a webhook, or the deploy repo, which must be saved.
	rows, err := mark.Query(ctx, `SELECT DISTINCT coalesce((SELECT r.via FROM releases r WHERE r.project_id = $1 AND r.env = e AND r.event = 'deploy' ORDER BY r.created_at DESC, r.id DESC LIMIT 1), 'webhook')
		FROM unnest($2::text[]) e`, id, targets)
	if err != nil {
		internal(err)
		return
	}
	vias, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		internal(err)
		return
	}
	var hooked, repoed bool
	if err := mark.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform_webhook), EXISTS (SELECT 1 FROM platform_repo)").Scan(&hooked, &repoed); err != nil {
		internal(err)
		return
	}
	if person && slices.Contains(targets, "prod") { // production's data on the platform is live production
		_ = mark.Rollback(ctx)
		a.refusePerson(ctx, w, id, "only a person can take production down")
		return
	}
	if slices.Contains(vias, "webhook") && !hooked { // never marked deleting to wait on a remove that cannot be sent
		writeErr(w, http.StatusConflict, "webhook_required", "this app's data is on your platform, and no platform webhook is set to tell it to delete it: set one in Settings › App hosting first", nil)
		return
	}
	if slices.Contains(vias, "repo") && !repoed {
		writeErr(w, http.StatusConflict, "repo_required", repoRequired, nil)
		return
	}
	if _, err := mark.Exec(ctx, "UPDATE projects SET deleting_since = COALESCE(deleting_since, now()) WHERE id = $1", id); err != nil {
		internal(err)
		return
	}
	if err := mark.Commit(ctx); err != nil {
		internal(err)
		return
	}
	runs.Cancel(a.durable, patch)
	// a recreation of the app's network under way holds its undeploy lock and starts no env after the mark:
	// wait for it, so the stops below stop what it started (contracts/routes.md, Egress control)
	if err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error { return runs.LockUndeployTx(ctx, tx, id) }); err != nil {
		if !undeployBusy(w, err) {
			internal(err)
		}
		return
	}
	rows, err = a.pool.Query(ctx, "SELECT env FROM deploys WHERE project_id = $1 AND run_by = 'org'", id)
	if err != nil {
		internal(err)
		return
	}
	orgRuns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		internal(err)
		return
	}
	handedOff, err := a.sendRemoves(ctx, id, targets) // each environment with a release of the org's: a remove that deletes, in place of POST /stop where the org runs it
	if err != nil {
		if !undeployBusy(w, err) {
			internal(err)
		}
		return
	}
	for _, env := range []string{"preview", "prod"} {
		if slices.Contains(orgRuns, env) { // no container of Canyon's there
			continue
		}
		if err := a.pipe.Executor.Stop(ctx, id, env); err != nil { // idempotent: a container that is not there is stopped
			internal(err)
			return
		}
	}
	if _, err := a.pool.Exec(ctx, "WITH gone AS (DELETE FROM deploys WHERE project_id = $1 AND run_by = 'canyon') UPDATE resources SET state = 'deleting' WHERE project_id = $1", id); err != nil { // /status shows nothing running; the org's rows go with its removed reports
		internal(err)
		return
	}
	var waiting []resourceRow
	for _, res := range records {
		d, ok := a.pipe.Drivers.Named(res.Driver) // the driver that made it, whichever serves its type today
		if !ok {
			writeErr(w, http.StatusBadGateway, "teardown_failed", "the driver that made "+res.Name+" is no longer configured: its data is kept", nil)
			return
		}
		answer, err := d.Delete(ctx, provisioners.Key{Project: id, Env: *res.Env, Name: res.Name})
		if err != nil || answer.State != provisioners.Gone && answer.State != provisioners.Pending { // an error or Failed
			a.log.Printf("canyon-server: delete %s: %s %s in %s: %v %s %s", id, res.Type, res.Name, *res.Env, err, answer.State, answer.Message)
			writeErr(w, http.StatusBadGateway, "teardown_failed", "the "+res.Type+" "+res.Name+" in "+*res.Env+" could not be deleted: its data is kept, and deleting again resumes", nil)
			return
		}
		if answer.State == provisioners.Pending {
			res.State = "deleting"
			waiting = append(waiting, res)
			continue
		}
		if err := a.dropRecord(ctx, act, id, res); err != nil {
			internal(err)
			return
		}
	}
	if len(waiting) > 0 || len(handedOff) > 0 {
		pending := make([]any, 0, len(waiting)+len(handedOff))
		for _, res := range waiting {
			pending = append(pending, res)
		}
		for _, rel := range handedOff {
			pending = append(pending, rel)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"pending": pending})
		return
	}
	if err := a.pipe.Executor.Forget(ctx, id); err != nil { // its images and build caches; the containers stopped above
		a.log.Printf("canyon-server: delete %s: forget: %v", id, err)
		writeErr(w, http.StatusBadGateway, "teardown_failed", "the app's images and build caches could not be removed: deleting again resumes", nil)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		internal(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Under the ledger lock a late report's remove takes too: an environment whose newest delete's remove is not
	// removed — one a late report sent since the removes above, open, or failed already — is waited on, and the
	// same request again sends it as a target (#2432).
	rows, err = tx.Query(ctx, `SELECT id, env FROM (SELECT DISTINCT ON (env) id, env, state FROM releases WHERE project_id = $1 AND event = 'remove' AND deletes_resources
		ORDER BY env, created_at DESC) d WHERE state <> 'removed' ORDER BY env`, id)
	if err != nil {
		internal(err)
		return
	}
	open, err := pgx.CollectRows(rows, pgx.RowToStructByPos[handedOffEnv])
	if err != nil {
		internal(err)
		return
	}
	if len(open) > 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"pending": open})
		return
	}
	tag, err := tx.Exec(ctx, "WITH gone AS (DELETE FROM secrets WHERE project_id = $1), routed AS (DELETE FROM egress_routes WHERE project_id = $1), asked AS (DELETE FROM promotion_approvals WHERE project_id = $1), unlinked AS (DELETE FROM project_repositories WHERE project_id = $1) UPDATE projects SET deleted_at = now(), report_key_ref = NULL, report_sealed_key = NULL, report_sealed_value = NULL WHERE id = $1 AND deleted_at IS NULL", id) // the report key and the value routes go with the secrets, its production approvals and its repository link (#1917) with the app
	if err != nil {
		internal(err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if err := events.AuditTx(ctx, tx, act, action(r), id, "", nil); err != nil {
		internal(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internal(err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handedOffEnv is an environment the org's platform still runs while its project is deleted: the
// remove release it waits on, a delete's pending[] row (contracts/routes.md, the delete row).
type handedOffEnv struct {
	Release string `json:"release"`
	Env     string `json:"env"`
}

// deleteTargets is where a project delete sends a remove that deletes (#2432): each environment the org runs,
// each with a deploy release whose package declared resources — a release that never went live reported no
// endpoint, so the release says what the platform may hold — each the platform reported provisioning for, each with a deploy
// release still open (the platform may be starting it: the delete's remove supersedes it, and a late report on it
// is refused), and each whose newest delete's remove has not reported removed — open, failed, undelivered or
// refused: a cleanup that failed stays a target. Nothing of it is read from run_by, so switching it never moves an environment in
// or out. A project Canyon alone ran has none.
func deleteTargets(ctx context.Context, q querier, project string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT env FROM deploys WHERE project_id = $1 AND run_by = 'org'
		UNION SELECT r.env FROM releases r JOIN packages p ON p.id = r.package_id AND p.project_id = r.project_id
			WHERE r.project_id = $1 AND r.event = 'deploy' AND coalesce(p.resources, '{}') <> '{}'
		UNION SELECT env FROM platform_resources WHERE project_id = $1
		UNION SELECT env FROM releases WHERE project_id = $1 AND event = 'deploy' AND state IN ('sent', 'accepted')
		UNION SELECT env FROM (SELECT DISTINCT ON (env) env, state FROM releases WHERE project_id = $1 AND event = 'remove' AND deletes_resources
			ORDER BY env, created_at DESC) d WHERE state <> 'removed'
		ORDER BY 1`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// sendRemoves sends — or finds open — a project delete's remove release, one that deletes, for each of the
// targets the mark read (deleteTargets), undeployed or not. Each is sent in its own transaction under
// the project's undeploy lock, so a concurrent delete finds the one this sent and never sends a second; and
// answers them: the delete waits until the org reports each removed, which takes its deploy row and its
// provided resources away, and the same request again resumes. One reported removed is done.
func (a *app) sendRemoves(ctx context.Context, id string, targets []string) ([]handedOffEnv, error) {
	var out []handedOffEnv
	for _, env := range targets {
		var release string
		err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) (err error) {
			if err := runs.LockUndeployTx(ctx, tx, id); err != nil {
				return err
			}
			if err := events.LockTx(ctx, tx); err != nil { // undeploy, then the ledger, as BrakeTx: a late report's remove and this one never both go
				return err
			}
			release, err = runs.SendDeleteRemoveTx(ctx, tx, a.durable, id, env, false)
			return err
		})
		if err != nil {
			return nil, err
		}
		if release == "" {
			continue
		}
		out = append(out, handedOffEnv{Release: release, Env: env})
	}
	return out, nil
}

// forgetDeleted asks the executor once per boot to forget every deleted project: one deleted before
// the delete did so kept its images and build caches. The executor rolled with the server may not be
// up yet, so a forget that fails is logged and retried, the wait doubling to five minutes, until it
// succeeds (#1858); each attempt is bounded, so an executor that never answers cannot hold the pass.
func forgetDeleted(ctx context.Context, pool *pgxpool.Pool, e *executor.Client, logger *log.Logger, wait time.Duration) {
	rows, err := pool.Query(ctx, "SELECT id FROM projects WHERE deleted_at IS NOT NULL ORDER BY id")
	if err != nil {
		logger.Printf("canyon-server: forget deleted projects: %v", err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		logger.Printf("canyon-server: forget deleted projects: %v", err)
		return
	}
	forget := func(id string) error {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return e.Forget(ctx, id)
	}
	for {
		var failed []string
		for _, id := range ids {
			if err := forget(id); err != nil {
				if ctx.Err() != nil {
					return // the server is stopping: the next boot asks again
				}
				logger.Printf("canyon-server: forget deleted project %s (retrying in %s): %v", id, wait, err)
				failed = append(failed, id)
			}
		}
		if ids = failed; len(ids) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Minute)
	}
}

// dropRecord removes a record its driver answered gone for, with the audit row that attributes the destroyed data.
func (a *app) dropRecord(ctx context.Context, act actor, id string, res resourceRow) error {
	tx, err := a.mutation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "DELETE FROM resources WHERE project_id = $1 AND env = $2 AND name = $3", id, *res.Env, res.Name); err != nil {
		return err
	}
	if err := events.AuditTx(ctx, tx, act, "delete resource", id, "", map[string]any{"name": res.Name, "type": res.Type, "env": *res.Env}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recordsOf is the project's resource records, the newest first: what a delete destroys, in its order.
func recordsOf(ctx context.Context, q querier, project string) ([]resourceRow, error) {
	rows, err := q.Query(ctx, "SELECT name, type, env, state, NULL::text, driver FROM resources WHERE project_id = $1 ORDER BY updated_at DESC, env, name", project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[resourceRow])
}

// providedOf is what the org's platform reported provisioning for the project, as the delete's listing
// names it (#2432): state provided, no endpoint — the delete's remove asks the platform to delete it.
func providedOf(ctx context.Context, q querier, project string) ([]resourceRow, error) {
	rows, err := q.Query(ctx, "SELECT name, type, env, 'provided', NULL::text, '' FROM ("+providedSQL+") p(name, type, env, state, endpoint) ORDER BY env, name", project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[resourceRow])
}

// querier is what the reads run on: the pool, or the transaction a mutation is still inside; a hook's, so ee/ reads
// on it too (design/ARCHITECTURE.md §8).
type querier = enterprise.Querier

// project1 reads one live project.
func project1(ctx context.Context, q querier, id string) (project, error) {
	ps, err := projects(ctx, q, id)
	if err != nil {
		return project{}, err
	}
	if len(ps) == 0 {
		return project{}, pgx.ErrNoRows
	}
	return ps[0], nil
}

// projects is every live project by id, or the one named — the fleet's row (routes.md row 47: one
// shape, everywhere a project is listed) without last_run, as the caller in ctx reads it.
func projects(ctx context.Context, q querier, id string) ([]project, error) {
	rows, err := q.Query(ctx, projectsSQL, append(callerArgs(ctx), id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ps := []project{}
	for rows.Next() {
		f, _, err := scanFleetRow(rows)
		if err != nil {
			return nil, err
		}
		ps = append(ps, f.project)
	}
	return ps, rows.Err()
}
