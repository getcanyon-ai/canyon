package main

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/jackc/pgx/v5"
)

// repoLink is an app's link to a repository, as the project read and the link route answer it (#1917).
type repoLink struct {
	RepoID     *int64  `json:"repo_id"` // null while Canyon makes a kept repository (#1926)
	FullName   string  `json:"full_name"`
	Folder     string  `json:"folder"`
	State      string  `json:"state"`
	Keeps      bool    `json:"keeps"`
	KeptCommit *string `json:"kept_commit"`
	KeptError  string  `json:"kept_error"`
}

// linkedApp is one app a repository listing names under the repository it is linked to.
type linkedApp struct {
	Folder string `json:"folder"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
}

// routesRepositories registers linking apps to GitHub repositories (contracts/routes.md, #1917):
// every route an admin's.
func routesRepositories(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /github/repositories", a.adminOnly(a.githubRepositories))
	api.HandleFunc("POST /github/repositories/{repo}/import", a.adminOnly(a.importRepository))
	api.HandleFunc("PUT /projects/{id}/repository", a.linkRepository)
	api.HandleFunc("DELETE /projects/{id}/repository", a.unlinkRepository)
	api.HandleFunc("PUT /projects/{id}/auto-deploy", a.ownerOnly(a.setAutoDeploy))
	api.HandleFunc("PUT /projects/{id}/repository/keeps", a.ownerOnly(a.handOver))
}

// setAutoDeploy turns on or off whether a push to the app's linked repository deploys to preview
// (contracts/routes.md, PUT …/auto-deploy; #1923): its owners' and admins' call.
func (a *app) setAutoDeploy(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var body struct {
		AutoDeploy *bool `json:"auto_deploy"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AutoDeploy == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "auto_deploy is true or false", nil)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "auto-deploy", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var linked bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM project_repositories WHERE project_id = $1)", id).Scan(&linked); err != nil || !linked {
		if err == nil {
			writeErr(w, http.StatusNotFound, "not_linked", "this app is not linked to a repository", nil)
			return
		}
		a.internal(w, "auto-deploy", err)
		return
	}
	err = setAutoDeploy(ctx, tx, id, *body.AutoDeploy)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "auto-deploy", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auto_deploy": *body.AutoDeploy})
}

// setAutoDeploy writes the app's switch, and its audit row when it changes.
func setAutoDeploy(ctx context.Context, tx pgx.Tx, id string, on bool) error {
	tag, err := tx.Exec(ctx, "UPDATE projects SET auto_deploy = $2 WHERE id = $1 AND auto_deploy <> $2", id, on)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return events.AuditTx(ctx, tx, actorOf(ctx), "set auto-deploy", id, "", map[string]any{"auto_deploy": on})
}

// handOver is PUT /projects/:id/repository/keeps {keeps: false} (contracts/routes.md; #1938): the repository
// Canyon keeps the app's code in is the developers' from now on. The link stays, so its pushes are versions;
// Canyon commits no more, and never takes a repository back.
func (a *app) handOver(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body) != 1 || body["keeps"] != false {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is {\"keeps\": false}: Canyon hands a repository over, and never takes one back", nil)
		return
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		a.internal(w, "hand over", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// the app's keep lock before the append lock, the order a keep takes them in: a keep under way finishes,
	// or starts after this and sees keeps false, and commits nothing (github-app/keep.go)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", githubapp.KeepLock(id)); err != nil {
		a.internal(w, "hand over", err)
		return
	}
	if err := events.LockTx(ctx, tx); err != nil {
		a.internal(w, "hand over", err)
		return
	}
	var link repoLink
	var kept bool
	err = tx.QueryRow(ctx, "SELECT repo_id, full_name, keeps FROM project_repositories WHERE project_id = $1 FOR UPDATE", id).Scan(&link.RepoID, &link.FullName, &kept)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_linked", "this app is not linked to a repository", nil)
		return
	}
	if err == nil && kept {
		if _, err = tx.Exec(ctx, "UPDATE project_repositories SET keeps = false, kept_error = '' WHERE project_id = $1", id); err == nil {
			err = events.AuditTx(ctx, tx, actorOf(ctx), "hand over repository", id, "", map[string]any{"repo_id": link.RepoID, "full_name": link.FullName})
		}
	}
	var read *repoLink
	if err == nil {
		read, err = linkOf(ctx, tx, id)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "hand over", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": read})
}

var folderSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validFolder is a link's folder: "" for the root, or segments of letters, digits, . _ and - joined
// by /, none of them . or .., at most 255 characters.
func validFolder(folder string) bool {
	if folder == "" {
		return true
	}
	for _, segment := range strings.Split(folder, "/") {
		if !folderSegment.MatchString(segment) || segment == "." || segment == ".." {
			return false
		}
	}
	return len(folder) <= 255
}

func (a *app) githubRepositories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	seen, unreachable, err := a.visible(ctx)
	if err != nil {
		a.internal(w, "github repositories", err)
		return
	}
	rows, err := a.pool.Query(ctx, `SELECT l.repo_id, l.folder, p.id, p.name, l.state FROM project_repositories l
		JOIN projects p ON p.id = l.project_id AND p.deleted_at IS NULL ORDER BY l.folder, p.id`)
	if err != nil {
		a.internal(w, "github repositories", err)
		return
	}
	apps := map[int64][]linkedApp{}
	var repo int64
	var one linkedApp
	if _, err := pgx.ForEachRow(rows, []any{&repo, &one.Folder, &one.ID, &one.Name, &one.State}, func() error {
		apps[repo] = append(apps[repo], one)
		return nil
	}); err != nil {
		a.internal(w, "github repositories", err)
		return
	}
	type listed struct {
		RepoID        int64       `json:"repo_id"`
		FullName      string      `json:"full_name"`
		Org           string      `json:"org"`
		DefaultBranch string      `json:"default_branch"`
		Apps          []linkedApp `json:"apps"`
	}
	out := make([]listed, 0, len(seen))
	for _, s := range seen {
		out = append(out, listed{s.ID, s.FullName, s.Account, s.DefaultBranch, append([]linkedApp{}, apps[s.ID]...)})
	}
	slices.SortFunc(out, func(x, y listed) int {
		return strings.Compare(strings.ToLower(x.FullName), strings.ToLower(y.FullName))
	})
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out, "unreachable": append([]string{}, unreachable...)})
}

// visible is githubapp.Visible, each org GitHub didn't answer for logged with why.
func (a *app) visible(ctx context.Context) ([]githubapp.Seen, []string, error) {
	seen, failed, err := githubapp.Visible(ctx, a.pool, a.vault, a.github)
	unreachable := slices.Sorted(maps.Keys(failed))
	for _, account := range unreachable {
		a.log.Printf("canyon-server: github repositories of %s: %v", account, failed[account])
	}
	return seen, unreachable, err
}

// seenRepository is the repository a live installation sees under id, or under fullName, owner/name in any case,
// when id is 0; written false when there is none, or GitHub did not answer for an org that might see it.
func (a *app) seenRepository(ctx context.Context, w http.ResponseWriter, id int64, fullName string) (githubapp.Seen, bool) {
	seen, unreachable, err := a.visible(ctx)
	if err != nil {
		a.internal(w, "github repository", err)
		return githubapp.Seen{}, false
	}
	if at := slices.IndexFunc(seen, func(s githubapp.Seen) bool {
		return id != 0 && s.ID == id || id == 0 && strings.EqualFold(s.FullName, fullName)
	}); at >= 0 {
		return seen[at], true
	}
	if len(unreachable) > 0 {
		writeErr(w, http.StatusBadGateway, "github_unavailable", "GitHub didn't answer for "+strings.Join(unreachable, ", ")+". Try again in a minute.", nil)
		return githubapp.Seen{}, false
	}
	writeErr(w, http.StatusNotFound, "not_found", "none of Canyon's GitHub Apps sees that repository", nil)
	return githubapp.Seen{}, false
}

// takenBy answers the app linked to repo and folder, "" for none.
func takenBy(ctx context.Context, tx pgx.Tx, repo int64, folder string) (string, error) {
	var app string
	err := tx.QueryRow(ctx, "SELECT project_id FROM project_repositories WHERE repo_id = $1 AND folder = $2", repo, folder).Scan(&app)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return app, err
}

func (a *app) importRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo, err := strconv.ParseInt(r.PathValue("repo"), 10, 64)
	if err != nil || repo <= 0 {
		writeErr(w, http.StatusNotFound, "not_found", "none of Canyon's GitHub Apps sees that repository", nil)
		return
	}
	var body struct {
		Folder string  `json:"folder"`
		Name   *string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validFolder(body.Folder) {
		writeErr(w, http.StatusBadRequest, "invalid_folder", "a folder is a path of letters, digits, dots, dashes and underscores, at most 255 characters", nil)
		return
	}
	if body.Name != nil && !plainText(*body.Name, 100) {
		writeErr(w, http.StatusBadRequest, "invalid_name", "an app's name is one line of plain text, at most 100 characters", nil)
		return
	}
	seen, ok := a.seenRepository(ctx, w, repo, "")
	if !ok {
		return
	}
	name := seen.Name
	if body.Name != nil {
		name = *body.Name
	} else if body.Folder != "" {
		name = path.Base(body.Folder)
	}
	check, err := githubapp.Head(ctx, a.pool, a.vault, a.github, seen) // GitHub's answer before the ledger lock is taken
	if err != nil {
		a.log.Printf("canyon-server: import %s: %v", seen.FullName, err)
		writeErr(w, http.StatusBadGateway, "github_unavailable", "GitHub didn't answer for "+seen.FullName+". Try again in a minute.", nil)
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "import", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if app, err := takenBy(ctx, tx, repo, body.Folder); err != nil || app != "" {
		a.linked(w, app, err)
		return
	}
	id, _, err := insertProject(ctx, tx, "", name) // no owner: a linked app is an admin's to adopt (design/PERMISSIONS.md)
	if err == nil {
		_, err = tx.Exec(ctx, "INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ($1, $2, $3, $4, $5)", id, repo, body.Folder, seen.Installation, seen.FullName)
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "import repository", id, "", map[string]any{"repo_id": repo, "folder": body.Folder, "full_name": seen.FullName})
	}
	if err == nil {
		check.Project, check.Folder = id, body.Folder
		err = githubapp.Enqueue(tx, a.durable, check)
	}
	var p project
	if err == nil {
		p, err = project1(ctx, tx, id)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "import", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": p})
}

// linkGuard is the link routes' order (contracts/routes.md, the Level table): an unknown or deleted
// app's 404, then a person below contributor's 403. false means the response is written.
func (a *app) linkGuard(w http.ResponseWriter, r *http.Request) bool {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return false
	}
	return roleOf(ctx) == "admin" || !a.teamRequired(ctx, w, a.pool, id)
}

// readable is the repository a member below admin may link (design/PERMISSIONS.md, linking a repository; #1917):
// one Canyon's App sees that one of the GitHub accounts they linked can read on GitHub now. A repository no App
// sees and one they can't read answer alike, so no private repository's existence leaks. false means the response
// is written.
func (a *app) readable(ctx context.Context, w http.ResponseWriter, seen githubapp.Seen) bool {
	err := githubapp.Reads(ctx, a.pool, a.vault, a.github, seen.Installation, seen.Owner.Login, seen.Name, actorOf(ctx).Subject)
	switch {
	case err == nil:
		return true
	case errors.Is(err, githubapp.ErrNoGitHubLink):
		writeErr(w, http.StatusForbidden, "github_link_required", githubLinkRequired, nil)
	case errors.Is(err, githubapp.ErrNoAccess):
		writeErr(w, http.StatusNotFound, "not_found", "none of Canyon's GitHub Apps sees that repository", nil)
	case errors.Is(err, githubapp.ErrGitHub):
		a.log.Printf("canyon-server: github access of %s: %v", seen.FullName, err)
		writeErr(w, http.StatusBadGateway, "github_unavailable", "GitHub didn't answer for "+seen.FullName+". Try again in a minute.", nil)
	default:
		a.internal(w, "github access", err)
	}
	return false
}

// githubLinkRequired is github_link_required's message (design/EXPERIENCE.md, D5).
const githubLinkRequired = "Link your GitHub account first: Canyon reads a repository for you only where your own GitHub account can read it."

func (a *app) linkRepository(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.linkGuard(w, r) {
		return
	}
	var body struct {
		RepoID   int64  `json:"repo_id"`
		FullName string `json:"full_name"`
		Folder   string `json:"folder"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if (body.RepoID == 0) == (body.FullName == "") {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body names the repository by repo_id or by full_name, owner/name, not both", nil)
		return
	}
	if !validFolder(body.Folder) {
		writeErr(w, http.StatusBadRequest, "invalid_folder", "a folder is a path of letters, digits, dots, dashes and underscores, at most 255 characters", nil)
		return
	}
	admin := roleOf(ctx) == "admin"
	if !admin { // no GitHub account linked: refused before GitHub is asked anything
		var linked bool
		if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM github_links WHERE subject = $1)", actorOf(ctx).Subject).Scan(&linked); err != nil {
			a.internal(w, "link", err)
			return
		}
		if !linked || actorOf(ctx).Subject == "" {
			writeErr(w, http.StatusForbidden, "github_link_required", githubLinkRequired, nil)
			return
		}
	}
	seen, ok := a.seenRepository(ctx, w, body.RepoID, body.FullName)
	if !ok || !admin && false && !a.readable(ctx, w, seen) { // FAULT, planted to show the access tests red
		return
	}
	body.RepoID = seen.ID
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "link", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var linked bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM project_repositories WHERE project_id = $1)", id).Scan(&linked); err != nil || linked {
		if err == nil {
			writeErr(w, http.StatusConflict, "app_linked", "this app is linked to a repository: unlink it first", nil)
			return
		}
		a.internal(w, "link", err)
		return
	}
	if app, err := takenBy(ctx, tx, body.RepoID, body.Folder); err != nil || app != "" {
		a.linked(w, app, err)
		return
	}
	link := repoLink{RepoID: &body.RepoID, FullName: seen.FullName, Folder: body.Folder, State: "linked"}
	tag, err := tx.Exec(ctx, `INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name)
		SELECT id, $2, $3, $4, $5 FROM projects WHERE id = $1 AND deleted_at IS NULL`, id, link.RepoID, link.Folder, seen.Installation, link.FullName) // deleted while GitHub was asked: no link
	if err == nil && tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "link repository", id, "", map[string]any{"repo_id": link.RepoID, "folder": link.Folder, "full_name": link.FullName})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "link", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": link})
}

func (a *app) unlinkRepository(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.linkGuard(w, r) {
		return
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		a.internal(w, "unlink", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var link repoLink
	err = tx.QueryRow(ctx, "DELETE FROM project_repositories WHERE project_id = $1 RETURNING repo_id, folder, full_name", id).Scan(&link.RepoID, &link.Folder, &link.FullName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_linked", "this app is not linked to a repository", nil)
		return
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, actorOf(ctx), "unlink repository", id, "", map[string]any{"repo_id": link.RepoID, "folder": link.Folder, "full_name": link.FullName})
	}
	if err == nil { // a yes given for one repository never deploys another's pushes (#1923)
		err = setAutoDeploy(ctx, tx, id, false)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		a.internal(w, "unlink", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linked answers a repository and folder another app has, or the error that kept it from asking.
func (a *app) linked(w http.ResponseWriter, app string, err error) {
	if err != nil {
		a.internal(w, "link", err)
		return
	}
	writeErr(w, http.StatusConflict, "linked", "the app "+app+" is linked to this repository and folder", map[string]any{"app": app})
}

// internal logs what failed and answers the contract's 500.
func (a *app) internal(w http.ResponseWriter, what string, err error) {
	a.log.Printf("canyon-server: %s: %v", what, err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// linkOf is an app's link for the project read: pending while a keeping organization is set and Canyon has
// yet to make the app's repository (#1926), nil without either.
func linkOf(ctx context.Context, q querier, id string) (*repoLink, error) {
	var link repoLink
	err := q.QueryRow(ctx, `SELECT l.repo_id, COALESCE(l.full_name, ''), COALESCE(l.folder, ''), COALESCE(l.state, 'pending'), COALESCE(l.keeps, true), NULLIF(l.kept_commit, ''), COALESCE(l.kept_error, p.keep_error)
		FROM projects p LEFT JOIN project_repositories l ON l.project_id = p.id
		WHERE p.id = $1 AND (l.project_id IS NOT NULL OR EXISTS (SELECT 1 FROM github_app WHERE keeps_code))`, id).Scan(&link.RepoID, &link.FullName, &link.Folder, &link.State, &link.Keeps, &link.KeptCommit, &link.KeptError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &link, err
}
