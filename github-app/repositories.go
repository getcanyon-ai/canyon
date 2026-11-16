package githubapp

import (
	"context"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Seen is a repository a live installation of one of Canyon's Apps sees (#1917).
type Seen struct {
	Repository
	Installation int64
	Account      string // the installation's, the org whose App sees it
}

// Visible answers every repository Canyon's Apps see, read from GitHub now with each live installation's
// token, and why GitHub didn't answer for an account, whose repositories are left out. Nothing is stored.
func Visible(ctx context.Context, pool *pgxpool.Pool, v *vault.Vault, api string) ([]Seen, map[string]error, error) {
	rows, err := pool.Query(ctx, "SELECT installation_id, account FROM github_installations WHERE deleted_at IS NULL AND app_id IS NOT NULL ORDER BY lower(account), installation_id")
	if err != nil {
		return nil, nil, err
	}
	installations, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID      int64
		Account string
	}])
	if err != nil {
		return nil, nil, err
	}
	var seen []Seen
	unreachable := map[string]error{}
	for _, in := range installations {
		app, err := LoadApp(ctx, pool, v, in.ID)
		if err != nil {
			return nil, nil, err
		}
		repos, err := New(api, app.ID, app.Key).Repositories(ctx, in.ID)
		if err != nil {
			unreachable[in.Account] = err
			continue
		}
		for _, r := range repos {
			seen = append(seen, Seen{Repository: r, Installation: in.ID, Account: in.Account})
		}
	}
	return seen, unreachable, nil
}

// Head is the check of the commit a repository's default branch points at, as a push of it is checked,
// by the org: the review an import starts (#1917). It asks GitHub, so it is read before any transaction.
func Head(ctx context.Context, pool *pgxpool.Pool, v *vault.Vault, api string, r Seen) (Check, error) {
	app, err := LoadApp(ctx, pool, v, r.Installation)
	if err != nil {
		return Check{}, err
	}
	sha, err := New(api, app.ID, app.Key).Head(ctx, r.Installation, r.Owner.Login, r.Name, r.DefaultBranch)
	if err != nil {
		return Check{}, err
	}
	return Check{Installation: r.Installation, Owner: r.Owner.Login, Repo: r.Name, SHA: sha, Login: r.Account, Ref: "refs/heads/" + r.DefaultBranch, RepoID: r.ID}, nil
}

// Enqueue starts check inside tx, once per app and commit.
func Enqueue(tx pgx.Tx, durable dbos.Client, check Check) error {
	_, err := dbos.Enqueue[any](durable, runs.Queue, Workflow, check, dbos.WithEnqueueWorkflowID("import/"+check.Project+"/"+check.SHA), dbos.WithEnqueueTransaction(tx))
	return err
}
