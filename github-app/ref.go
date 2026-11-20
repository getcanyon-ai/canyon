package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5"
)

// The refusals of packaging a ref on request (contracts/routes.md, POST …/packages/from-repository): the app has
// no live link, GitHub knows no such ref in the linked repository, or GitHub didn't answer.
var (
	ErrNotLinked   = errors.New("this app is not linked to a repository")
	ErrRefNotFound = errors.New("the linked repository has no such ref")
	ErrGitHub      = errors.New("github didn't answer")
)

// Packed is a ref packaged on request: the commit it named and the package it became, or, with nothing packaged,
// Refused and Message for a tree Canyon refuses: folder_not_found (the link's folder is not in the commit),
// other_app (canyon.toml names another app), or bad_request, whose Cause is the declarations' or ingest's error.
type Packed struct {
	Commit, Package  string
	Refused, Message string
	Cause            error
}

// PackRef packages ref of the repository project is linked to (design/specs/mcp.md §2.3), when one of the GitHub
// accounts by linked can read that repository now (Reads: ErrNoGitHubLink, ErrNoAccess): the ref resolved to its
// commit through the link's installation, then that commit, or the link's folder of it, packaged as a push's is,
// by by, with one audit row under action. check makes a check's package, never the current one; otherwise it is
// a repository package and current, as a push of that commit makes it.
func (c *Courier) PackRef(ctx context.Context, project, ref string, check bool, by events.Actor, action string) (Packed, error) {
	var in Check
	var fullName string
	err := c.Pool.QueryRow(ctx, `SELECT l.installation_id, l.full_name, l.repo_id, l.folder FROM project_repositories l
		JOIN projects p ON p.id = l.project_id AND p.deleted_at IS NULL WHERE l.project_id = $1 AND l.state = 'linked'`, project).
		Scan(&in.Installation, &fullName, &in.RepoID, &in.Folder)
	if errors.Is(err, pgx.ErrNoRows) {
		return Packed{}, ErrNotLinked
	}
	if err != nil {
		return Packed{}, err
	}
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return Packed{}, fmt.Errorf("github: the link of %s names %q, no owner/repository", project, fullName)
	}
	_ = Reads // FAULT, planted to show the access tests red: no caller read
	app, err := LoadApp(ctx, c.Pool, c.Vault, in.Installation)
	if err != nil {
		return Packed{}, err
	}
	gh := New(c.API, app.ID, app.Key)
	sha, err := gh.Commit(ctx, in.Installation, owner, repo, ref)
	if errors.Is(err, ErrRefNotFound) {
		return Packed{}, err
	}
	if err != nil {
		return Packed{}, fmt.Errorf("%w: %w", ErrGitHub, err)
	}
	in.Owner, in.Repo, in.SHA, in.Project = owner, repo, sha, project
	tree := ""
	if in.Folder != "" {
		if tree, err = gh.FolderTree(ctx, in.Installation, owner, repo, sha, in.Folder); err != nil {
			return Packed{}, fmt.Errorf("%w: %w", ErrGitHub, err)
		}
		if tree == "" {
			return Packed{Commit: sha, Refused: "folder_not_found", Message: in.Folder + " isn't in " + sha[:7] + ": Canyon packages this app from " + in.Folder + "."}, nil
		}
	}
	kind := "repo"
	if check {
		kind = "check"
	}
	end, err := c.packCommit(ctx, gh, in, tree, kind, by, action)
	if err != nil {
		return Packed{}, err
	}
	switch {
	case end.Refusal == nil:
		return Packed{Commit: sha, Package: end.Package}, nil
	case errors.Is(end.Refusal, errOtherApp):
		return Packed{Commit: sha, Refused: "other_app", Message: end.Title + ". " + end.Summary}, nil
	default:
		return Packed{Commit: sha, Refused: "bad_request", Message: end.Summary, Cause: end.Refusal}, nil
	}
}

// Commit answers the commit ref names in owner/repo: a branch, a tag or a commit's sha. ErrRefNotFound when
// GitHub has none (404, or 422 for a name it cannot read as a ref). A ref's slashes stay path separators, as
// GitHub reads a branch named release/2.
func (c *Client) Commit(ctx context.Context, installation int64, owner, repo, ref string) (string, error) {
	segments := strings.Split(ref, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	_, err := c.installed(ctx, installation, http.MethodGet, owner, repo, "/commits/"+strings.Join(segments, "/"), githubJSON, nil, &out)
	if status := statusError(0); errors.As(err, &status) && (status == http.StatusNotFound || status == http.StatusUnprocessableEntity) {
		return "", ErrRefNotFound
	}
	if err != nil {
		return "", err
	}
	if !validSHA(out.SHA) {
		return "", fmt.Errorf("github: %s/%s's %s answered no commit", owner, repo, ref)
	}
	return out.SHA, nil
}
