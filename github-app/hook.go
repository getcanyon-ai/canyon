package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/names"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Courier answers GitHub's deliveries (ServeHTTP) and drives the github-check workflow each one starts.
type Courier struct {
	Pool       *pgxpool.Pool
	Vault      *vault.Vault
	Durable    dbos.Client
	API        string                                                                                 // CANYON_GITHUB_API
	Public     string                                                                                 // CANYON_PUBLIC_URL, a kept commit's Canyon-Review link
	Repos      string                                                                                 // <Repos>/<project>.git, as the upload ingests into
	Exclusions []string                                                                               // the snapshot's, as every upload's
	Declare    func(tree string) (resources, roles map[string]string, description *string, err error) // the upload's read of canyon.toml: an error is a refusal
	Log        *log.Logger

	FirstDeclarations func(ctx context.Context, tx pgx.Tx, project string, declared map[string]string) ([]string, error) // the upload's: one audit row each
	Policy            func(ctx context.Context, tx pgx.Tx, project string) (policy.Snapshot, error)                      // the project's policy a run starts under, as a user's start composes it
	Attach            func(ctx context.Context, project, pkg string, raw []byte, by events.Actor) error                  // the reports route's attach, less the route's rate limit
}

// repository is one of installation_repositories' repositories.
type repository struct {
	ID       int64
	FullName string `json:"full_name"`
}

// delivery is the part of a push, pull request, repository or installation payload the courier reads.
type delivery struct {
	Action, Ref, After string
	PullRequest        struct {
		Number int
		Head   struct {
			SHA  string
			Ref  string   // the head branch, which a pull request's package names (#1947)
			Repo struct { // null when the head's repository is gone
				FullName string `json:"full_name"`
			}
		}
	} `json:"pull_request"`
	Repository struct {
		ID            int64
		Name          string
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct{ Login string }
	}
	Added        []repository `json:"repositories_added"` // installation_repositories
	Removed      []repository `json:"repositories_removed"`
	Installation struct {
		ID      int64
		Account struct {
			Login string
			ID    int64
		}
	}
	Sender struct {
		Login string
		ID    int64
	}
}

// ServeHTTP is POST /webhooks/github (contracts/routes.md, Open), judged in the contract's order.
func (c *Courier) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil { // past 5 MiB, or a connection gone that reads no answer
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "a delivery is at most 5 MiB")
		return
	}
	app, _ := strconv.ParseInt(r.Header.Get("X-GitHub-Hook-Installation-Target-ID"), 10, 64) // the App the delivery is to: one per org (#1914)
	secret, err := c.hookSecret(r.Context(), app)
	if errors.Is(err, errNoApp) {
		writeErr(w, http.StatusServiceUnavailable, "not_configured", "no GitHub App is registered on this host")
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.internal(w, err)
		return
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if err != nil || !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil)))) { // no App by that id is a signature that is no App's
		writeErr(w, http.StatusUnauthorized, "bad_signature", "the delivery's signature is not this App's")
		return
	}
	var d delivery
	event := r.Header.Get("X-GitHub-Event")
	accepted := false
	if json.Unmarshal(body, &d) == nil && d.served(event) {
		accepted, err = c.accept(r.Context(), app, r.Header.Get("X-GitHub-Delivery"), event, d)
	}
	switch {
	case err != nil:
		c.internal(w, err)
	case accepted:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "{}\n")
	default: // an event not served, or from an installation with no live row
		w.WriteHeader(http.StatusNoContent)
	}
}

// served: a push to the default branch, a pull request opened or pushed to from the repository itself
// (never a fork's), an installation created or deleted, and what GitHub says of a repository a link names.
func (d delivery) served(event string) bool {
	switch event {
	case "pull_request":
		return (d.Action == "opened" || d.Action == "synchronize") && d.Repository.FullName != "" &&
			d.PullRequest.Head.Repo.FullName == d.Repository.FullName && validSHA(d.PullRequest.Head.SHA)
	case "installation":
		return d.Action == "created" || d.Action == "deleted"
	case "push":
		return d.Repository.DefaultBranch != "" && d.Ref == "refs/heads/"+d.Repository.DefaultBranch && validSHA(d.After)
	case "repository": // keeps links (#1917)
		return d.Repository.ID > 0 && slices.Contains([]string{"renamed", "transferred", "deleted", "archived", "unarchived", "created"}, d.Action)
	case "installation_repositories":
		return d.Action == "added" || d.Action == "removed"
	}
	return false
}

// accept records app's delivery id and stores the installation or enqueues the check under that id, in one
// transaction. A redelivery does nothing; false is an installation with no live row of app's, nothing written.
func (c *Courier) accept(ctx context.Context, app int64, id, event string, d delivery) (bool, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "INSERT INTO github_deliveries (delivery_id, event, app_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", id, event, app)
	if err != nil || tag.RowsAffected() == 0 {
		return err == nil, err
	}
	var live bool
	switch {
	case event == "installation" && d.Action == "created": // on the org that owns the App alone (#1914)
		if live, err = InstallationCreated(ctx, tx, app, d.Installation.ID, d.Installation.Account.Login, d.Installation.Account.ID); err != nil || !live {
			return false, err
		}
	case event == "installation":
		err = InstallationDeleted(ctx, tx, app, d.Installation.ID)
	default:
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM github_installations WHERE installation_id = $1 AND app_id = $2 AND deleted_at IS NULL)", d.Installation.ID, app).Scan(&live); err != nil || !live {
			return false, err
		}
		switch {
		case event == "repository" && d.Action == "created": // an app, in an org whose new repositories become apps (#1923); else 204
			if live, err = importNew(ctx, tx, app, d, []repository{{d.Repository.ID, d.Repository.FullName}}); err != nil || !live {
				return false, err
			}
		case event == "installation_repositories" && d.Action == "added":
			if err = relink(ctx, tx, event, d); err == nil {
				_, err = importNew(ctx, tx, app, d, d.Added)
			}
		case event == "repository" || event == "installation_repositories":
			err = relink(ctx, tx, event, d)
		default:
			err = c.enqueue(ctx, tx, app, id, event, d)
		}
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// enqueue starts one check per app linked to the repository, each its own workflow under the delivery's
// id and the app's, or the one neutral check of a repository no app is linked to (#1917). The links take
// the name and installation the delivery gives the repository; a push moves a kept repository's kept_commit
// to its head, and Canyon's own push of a kept version starts no check (#1926).
func (c *Courier) enqueue(ctx context.Context, tx pgx.Tx, app int64, id, event string, d delivery) error {
	check := Check{Installation: d.Installation.ID, Owner: d.Repository.Owner.Login, Repo: d.Repository.Name, SHA: d.After, Login: d.Sender.Login, User: d.Sender.ID, PullRequest: event == "pull_request", RepoID: d.Repository.ID}
	head := "" // a push's head, kept_commit's next
	if check.PullRequest {
		check.SHA, check.Number, check.Branch = d.PullRequest.Head.SHA, d.PullRequest.Number, d.PullRequest.Head.Ref
	} else {
		check.Ref, head = d.Ref, d.After
	}
	var own bool // only the App's installation token pushes as its bot
	if err := tx.QueryRow(ctx, "SELECT $2 = slug || '[bot]' FROM github_app WHERE app_id = $1", app, d.Sender.Login).Scan(&own); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE project_repositories r SET full_name = $2, installation_id = $3, kept_commit = CASE WHEN r.keeps AND $4 <> '' THEN $4 ELSE r.kept_commit END
		FROM projects p WHERE r.repo_id = $1 AND r.state = 'linked' AND p.id = r.project_id AND p.deleted_at IS NULL RETURNING r.project_id, r.folder, r.keeps`,
		d.Repository.ID, d.Repository.FullName, d.Installation.ID, head)
	if err != nil {
		return err
	}
	var links []Check
	var kept bool
	for rows.Next() {
		one, keeps := check, false
		if err := rows.Scan(&one.Project, &one.Folder, &keeps); err != nil {
			rows.Close()
			return err
		}
		kept = kept || keeps
		if !keeps || !own || head == "" { // Canyon's own push of a kept version starts no check
			links = append(links, one)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(links) == 0 && !kept {
		_, err := dbos.Enqueue[any](c.Durable, runs.Queue, Workflow, check, dbos.WithEnqueueWorkflowID(id), dbos.WithEnqueueTransaction(tx))
		return err
	}
	for _, one := range links {
		if _, err := dbos.Enqueue[any](c.Durable, runs.Queue, Workflow, one, dbos.WithEnqueueWorkflowID(id+"/"+one.Project), dbos.WithEnqueueTransaction(tx)); err != nil {
			return err
		}
	}
	return nil
}

// importNew makes an app of each repository no link names, at its root, as an import makes one: the
// host's id, the repository's name, no owner, by the delivery's sender (#1923). false: the App's org does
// not make its new repositories apps, and nothing is written. It is reviewed at its next push.
func importNew(ctx context.Context, tx pgx.Tx, app int64, d delivery, repos []repository) (bool, error) {
	var imports, own bool // a repository the App made itself is one Canyon keeps, linked as it is made (#1926)
	if err := tx.QueryRow(ctx, "SELECT imports_new, $2 = slug || '[bot]' FROM github_app WHERE app_id = $1", app, d.Sender.Login).Scan(&imports, &own); err != nil || !imports || own {
		return false, err
	}
	by := events.Actor{Kind: "agent", Subject: "github:" + d.Sender.Login, Display: d.Sender.Login}
	for _, r := range repos {
		var linked bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM project_repositories WHERE repo_id = $1)", r.ID).Scan(&linked); err != nil {
			return false, err
		}
		if linked { // of any state, at any folder: an admin's choice, or Canyon's own repository
			continue
		}
		_, name, _ := strings.Cut(r.FullName, "/")
		var id string
		for range 8 { // as POST /projects {name} picks: never an id any project has had
			id = names.NewApp()
			tag, err := tx.Exec(ctx, "INSERT INTO projects (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING", id, name)
			if err != nil {
				return false, err
			}
			if tag.RowsAffected() == 1 {
				break
			}
			id = ""
		}
		if id == "" {
			return false, errors.New("eight picked app ids in a row were taken")
		}
		if _, err := tx.Exec(ctx, "INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ($1, $2, '', $3, $4)", id, r.ID, d.Installation.ID, r.FullName); err != nil {
			return false, err
		}
		if err := events.AuditTx(fromGitHub(ctx), tx, by, "import repository", id, "", map[string]any{"repo_id": r.ID, "folder": "", "full_name": r.FullName}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// relink keeps the links to a repository as GitHub reports it: its new name, deleted, archived again or
// not, out of the App's reach or back in it. Each link whose state changes is one audit row on its app.
func relink(ctx context.Context, tx pgx.Tx, event string, d delivery) error {
	if d.Action == "renamed" || d.Action == "transferred" {
		_, err := tx.Exec(ctx, "UPDATE project_repositories SET full_name = $2 WHERE repo_id = $1", d.Repository.ID, d.Repository.FullName)
		return err
	}
	type move struct {
		repo         int64
		to, from     string // from "" is any state but to
		installation int64  // the installation whose access changed; 0 for what GitHub says of the repository itself
	}
	var moves []move
	switch {
	case event == "repository" && d.Action == "unarchived":
		moves = []move{{d.Repository.ID, "linked", "archived", 0}}
	case event == "repository" && d.Action == "deleted":
		moves = []move{{d.Repository.ID, "deleted", "", 0}}
	case event == "repository":
		moves = []move{{d.Repository.ID, "archived", "linked", 0}}
	default:
		to, from, repos := "removed", "linked", d.Removed
		if d.Action == "added" {
			to, from, repos = "linked", "removed", d.Added
		}
		for _, r := range repos {
			moves = append(moves, move{r.ID, to, from, d.Installation.ID})
		}
	}
	by := events.Actor{Kind: "agent", Subject: "github:" + d.Sender.Login, Display: d.Sender.Login}
	for _, m := range moves {
		rows, err := tx.Query(ctx, `UPDATE project_repositories SET state = $2 WHERE repo_id = $1 AND state <> $2 AND ($3 = '' OR state = $3)
			AND ($4 = 0 OR installation_id = $4) RETURNING project_id, full_name`, m.repo, m.to, m.from, m.installation)
		if err != nil {
			return err
		}
		changed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Project, FullName string }])
		if err != nil {
			return err
		}
		for _, link := range changed {
			if err := events.AuditTx(fromGitHub(ctx), tx, by, "repository changed", link.Project, "", map[string]any{"repo_id": m.repo, "full_name": link.FullName, "state": m.to}); err != nil {
				return err
			}
		}
	}
	return nil
}

// errNoApp is a host with no App registered at all.
var errNoApp = errors.New("github: no App is registered")

// hookSecret opens app's webhook secret and nothing else: the private key stays sealed in the hook.
// pgx.ErrNoRows: no App has that id; errNoApp: there is none.
func (c *Courier) hookSecret(ctx context.Context, app int64) ([]byte, error) {
	var keyRef *int
	var hookKey, sealed []byte
	var registered bool
	if err := c.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_app), a.key_ref, a.sealed_hook_key, a.sealed_hook_secret
		FROM (SELECT 1) one LEFT JOIN github_app a ON a.app_id = $1`, app).Scan(&registered, &keyRef, &hookKey, &sealed); err != nil {
		return nil, err
	}
	switch {
	case !registered:
		return nil, errNoApp
	case keyRef == nil:
		return nil, pgx.ErrNoRows
	}
	return c.Vault.Open(*keyRef, "github", "app", "webhook_secret", hookKey, sealed)
}

func (c *Courier) internal(w http.ResponseWriter, err error) {
	c.Log.Printf("canyon-server: github webhook: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}
