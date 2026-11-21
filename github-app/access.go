package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A person's own read of a repository on GitHub (design/PERMISSIONS.md, linking a repository; #1917): Canyon's App
// usually sees every repository of an org, so a member below admin reaches a repository's code through Canyon only
// where one of the GitHub accounts they linked (GET /api/v1/github/link) can read it on GitHub. ErrNoGitHubLink:
// they linked none. ErrNoAccess: none of them can read it, which a caller answers as a repository no App sees.
var (
	ErrNoGitHubLink = errors.New("no GitHub account is linked to this person")
	ErrNoAccess     = errors.New("no GitHub account of this person can read the repository")
)

// Reads answers nil when one of the GitHub accounts subject linked can read owner/repo now, as GitHub answers the
// installation, asked on every call: access lost on GitHub is lost here at once. GitHub not answering is ErrGitHub,
// never a yes: the caller refuses.
func Reads(ctx context.Context, pool *pgxpool.Pool, v *vault.Vault, api string, installation int64, owner, repo, subject string) error {
	if subject == "" {
		return ErrNoGitHubLink
	}
	rows, err := pool.Query(ctx, "SELECT login FROM github_links WHERE subject = $1 ORDER BY linked_at, github_id", subject)
	if err != nil {
		return err
	}
	logins, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(logins) == 0 {
		return ErrNoGitHubLink
	}
	app, err := LoadApp(ctx, pool, v, installation)
	if err != nil {
		return err
	}
	gh := New(api, app.ID, app.Key)
	for _, login := range logins {
		ok, err := gh.CanRead(ctx, installation, owner, repo, login)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrGitHub, err)
		}
		if ok {
			return nil
		}
	}
	return ErrNoAccess
}

// CanRead answers whether GitHub's login can read owner/repo: its permission there is read, write or admin. GitHub's
// 404, a login that is no collaborator of a private repository or no account at all, is no.
func (c *Client) CanRead(ctx context.Context, installation int64, owner, repo, login string) (bool, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	_, err := c.installed(ctx, installation, http.MethodGet, owner, repo, "/collaborators/"+url.PathEscape(login)+"/permission", githubJSON, nil, &out)
	if status := statusError(0); errors.As(err, &status) && status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch out.Permission {
	case "admin", "write", "read":
		return true, nil
	case "none":
		return false, nil
	}
	return false, fmt.Errorf("github: %s's permission on %s/%s is %q", login, owner, repo, out.Permission)
}
