package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// validRef is a ref the route asks GitHub for: a branch, a tag or a commit's sha, in the characters git allows
// unquoted in a ref, at most 255; ".." , "//", a leading "-" or "/" and a trailing "/" are refused before GitHub is asked.
var validRef = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,254}$`)

func refOK(ref string) bool {
	return validRef.MatchString(ref) && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.HasSuffix(ref, "/")
}

// packageFromRepository is POST /projects/:id/packages/from-repository {ref, check?} (contracts/routes.md;
// design/specs/mcp.md §2.3): a ref of the app's linked repository packaged on request, through the link's
// installation, as a push of that commit is, for a caller whose own linked GitHub account can read that repository
// now, asked of GitHub each call, an admin's included; with check, a check's package, never current. It reviews and deploys
// nothing: POST …/gate-runs or …/deploys with the package follows.
func (a *app) packageFromRepository(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !runs.ValidProject.MatchString(id) { // the id names a directory on disk: only a project id reaches it
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	var body struct {
		Ref   *string `json:"ref"`
		Check bool    `json:"check"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Ref == nil || !refOK(*body.Ref) {
		writeErr(w, http.StatusBadRequest, "invalid_ref", "ref is a branch, a tag or a commit of the app's linked repository", nil)
		return
	}
	menu, err := componentsOf(a.snap.ComponentMenu)
	if err != nil {
		a.internal(w, "package from repository", err)
		return
	}
	courier := &githubapp.Courier{Pool: a.pool, Vault: a.vault, API: a.github, Repos: a.repos, Exclusions: a.snap.Exclusions, Log: a.log,
		Declare: func(tree string) (map[string]string, map[string]string, *string, error) {
			offered, err := a.provisioned(ctx, slices.Clone(menu))
			if err != nil {
				return nil, nil, nil, err
			}
			return declarations(tree, offered)
		}, // the upload's and the push's read of canyon.toml
		FirstDeclarations: func(ctx context.Context, tx pgx.Tx, project string, declared map[string]string) ([]string, error) {
			return firstDeclarations(ctx, tx, project, declared)
		},
	}
	got, err := courier.PackRef(ctx, id, *body.Ref, body.Check, actorOf(ctx), action(r))
	switch {
	case errors.Is(err, githubapp.ErrNotLinked):
		writeErr(w, http.StatusNotFound, "not_linked", "this app is not linked to a repository", nil)
		return
	case errors.Is(err, githubapp.ErrNoGitHubLink):
		writeErr(w, http.StatusForbidden, "github_link_required", githubLinkRequired, nil)
		return
	case errors.Is(err, githubapp.ErrNoAccess): // as a repository no App sees: nothing says the repository exists
		writeErr(w, http.StatusNotFound, "not_found", "none of Canyon's GitHub Apps sees that repository", nil)
		return
	case errors.Is(err, githubapp.ErrRefNotFound):
		writeErr(w, http.StatusNotFound, "ref_not_found", "the linked repository has no branch, tag or commit "+*body.Ref, nil)
		return
	case errors.Is(err, githubapp.ErrGitHub):
		a.log.Printf("canyon-server: package %s from its repository at %s: %v", id, *body.Ref, err)
		writeErr(w, http.StatusBadGateway, "github_unavailable", "GitHub didn't answer. Try again in a minute.", nil)
		return
	case err != nil:
		a.internal(w, "package from repository", err)
		return
	}
	var refused *declarationError
	switch {
	case got.Refused == "not_own": // a commit off the repository's own branches, a fork's: as no such ref of it
		writeErr(w, http.StatusNotFound, "ref_not_found", got.Message, nil)
		return
	case got.Refused == "folder_not_found":
		writeErr(w, http.StatusNotFound, "folder_not_found", got.Message, nil)
		return
	case got.Refused == "other_app":
		writeErr(w, http.StatusConflict, "other_app", got.Message, nil)
		return
	case errors.As(got.Cause, &refused):
		writeErr(w, http.StatusBadRequest, refused.code, refused.Error(), nil)
		return
	case got.Refused != "" || errors.Is(got.Cause, ingest.ErrRequest):
		writeErr(w, http.StatusBadRequest, "bad_request", got.Message, nil)
		return
	}
	var out packageOut
	if err := a.pool.QueryRow(ctx, "SELECT id, label, tree_hash, commit, source_kind, source_ref, source_actor FROM packages WHERE id = $1 AND project_id = $2", got.Package, id).
		Scan(&out.ID, &out.Label, &out.TreeHash, &out.Commit, &out.Source.Kind, &out.Source.Ref, &out.Source.Actor); err != nil {
		a.internal(w, "package from repository", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"package": out})
}
