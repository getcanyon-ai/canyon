package githubapp

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/gitpush"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// KeepWorkflow is the name the keep workflow registers under (contracts/routes.md, Kept in GitHub; #1926).
const KeepWorkflow = "github-keep"

// Keep is what the keep workflow receives: the version an upload made current, and who uploaded it.
type Keep struct {
	Project string `json:"project"`
	Package string `json:"package"`
	Display string `json:"display"`
	Email   string `json:"email,omitempty"` // the uploader's work address, for Co-authored-by; "" leaves the line out
}

// cloneScheme is what a kept repository's clone url starts with: GitHub's, never another transport.
var cloneScheme = "https://"

// keepWait is how long the keep workflow waits for the version's first run to end, keepPoll how often it looks.
var keepWait, keepPoll = 40 * time.Minute, 5 * time.Second

// QueueKeep, inside an upload's transaction, records the commit main was at as the version's base and starts the
// keep workflow, when one org keeps code and no repository the person pushes to feeds the app. It writes nothing
// otherwise. The workflow runs after the upload answers: an upload never waits on GitHub.
func QueueKeep(ctx context.Context, tx pgx.Tx, durable dbos.Client, k Keep) error {
	var keeping, linked, keeps bool
	var base string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM github_app WHERE keeps_code),
		EXISTS (SELECT 1 FROM project_repositories WHERE project_id = $1),
		COALESCE((SELECT keeps FROM project_repositories WHERE project_id = $1), false),
		COALESCE((SELECT kept_commit FROM project_repositories WHERE project_id = $1), '')`, k.Project).Scan(&keeping, &linked, &keeps, &base); err != nil {
		return err
	}
	if !keeping || (linked && !keeps) {
		return nil
	}
	if _, err := tx.Exec(ctx, "UPDATE packages SET kept_base = $3 WHERE id = $2 AND project_id = $1", k.Project, k.Package, base); err != nil {
		return err
	}
	_, err := dbos.Enqueue[any](durable, runs.Queue, KeepWorkflow, k, dbos.WithEnqueueWorkflowID("keep/"+k.Project+"/"+k.Package+"/"+rand.Text()[:8]), dbos.WithEnqueueTransaction(tx))
	return err
}

// keep makes the app's kept repository if it has none, waits for the version's first run, and commits the version
// on top of the commit main was at when it was made, if that run let it through and it is still current. Every
// refusal is said in kept_error; GitHub's own no waits for the next version, anything else fails the step, which is tried again.
func (c *Courier) keep(ctx dbos.Context, k Keep) (string, error) {
	if _, err := dbos.RunAsStep(ctx, func(s context.Context) (struct{}, error) { return struct{}{}, c.keptRepository(s, k) }, dbos.WithStepName("repository"), retried); err != nil {
		return "", err
	}
	passed, err := dbos.RunAsStep(ctx, func(s context.Context) (string, error) { return c.passedRun(s, k) }, dbos.WithStepName("review"), retried)
	if err != nil || passed == "" {
		return "", err
	}
	return dbos.RunAsStep(ctx, func(s context.Context) (string, error) { return c.commitKept(s, k, passed) }, dbos.WithStepName("commit"), retried)
}

// keeper is the keeping org's App and a live installation of it.
type keeper struct {
	App          App
	Account      string
	Installation int64
}

func (c *Courier) keeper(ctx context.Context, q querier) (keeper, error) {
	var k keeper
	var installation int64
	if err := q.QueryRow(ctx, `SELECT a.account, i.installation_id FROM github_app a JOIN github_installations i ON i.app_id = a.app_id AND i.deleted_at IS NULL
		WHERE a.keeps_code ORDER BY i.created_at LIMIT 1`).Scan(&k.Account, &installation); err != nil {
		return keeper{}, err
	}
	app, err := LoadApp(ctx, q, c.Vault, installation)
	k.App, k.Installation = app, installation
	return k, err
}

// keptRepository makes the app's repository in the keeping org when it has no link: private, named after the app
// with four random characters, picked again when GitHub says the name is taken. A refusal is the app's keep_error.
// Two keeps of one app take turns, so two quick uploads make one repository; a repository made for an app
// deleted or linked meanwhile is deleted again, never left behind unlinked.
func (c *Courier) keptRepository(ctx context.Context, k Keep) error {
	conn, done, err := c.keeping(ctx, k.Project)
	if err != nil {
		return err
	}
	defer done()
	var linked bool
	var name string
	err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM project_repositories WHERE project_id = $1), name FROM projects WHERE id = $1 AND deleted_at IS NULL", k.Project).Scan(&linked, &name)
	if err != nil || linked {
		return ignoreNoRows(err)
	}
	kp, err := c.keeper(ctx, conn)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.keepError(ctx, conn, k.Project, "Canyon's App isn't installed on the organization that keeps code.")
	}
	if err != nil {
		return err
	}
	gh := New(c.API, kp.App.ID, kp.App.Key)
	var made struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	for try := 0; ; try++ {
		_, err = gh.as(ctx, kp.Installation, http.MethodPost, "/orgs/"+kp.Account+"/repos", githubJSON, map[string]any{"name": RepoName(name), "private": true, "description": "Kept by Canyon: " + k.Project}, &made)
		var status statusError
		if err == nil || !errors.As(err, &status) || status != http.StatusUnprocessableEntity || try == 4 { // 422: the name is taken
			break
		}
	}
	if err != nil { // said on the app either way; GitHub's own no waits for the next version, anything else is tried again
		said := c.keepError(ctx, conn, k.Project, "GitHub refused making the repository: "+err.Error())
		if status := statusError(0); errors.As(err, &status) && status < 500 && status != http.StatusTooManyRequests {
			return said
		}
		return errors.Join(said, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name, keeps)
		SELECT $1, $2, '', $3, $4, true FROM projects WHERE id = $1 AND deleted_at IS NULL ON CONFLICT DO NOTHING`, k.Project, made.ID, kp.Installation, made.FullName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 { // the app went, or an admin linked it, while GitHub made the repository
		body, err := gh.as(ctx, kp.Installation, http.MethodDelete, "/repos/"+made.FullName, githubJSON, nil, nil)
		if err == nil {
			err = body.Close()
		}
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE projects SET keep_error = '' WHERE id = $1", k.Project); err != nil {
		return err
	}
	if err := events.AuditTx(fromGitHub(ctx), tx, canyonActor, "create kept repository", k.Project, "", map[string]any{"repo_id": made.ID, "full_name": made.FullName}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var canyonActor = events.Actor{Kind: "canyon", Subject: "github", Display: "Canyon"}

// KeepLock is the key of an app's keep lock, which a hand-over takes too (#1938).
func KeepLock(project string) string { return "canyon-keep:" + project }

// keeping holds the app's keep lock on one connection of its own, which everything the keep reads and
// writes goes through, so a keep never waits on the pool while it holds the lock: an app's keeps make its
// repository and commit one at a time. done unlocks and gives the connection back; one that can't be
// unlocked is closed, never pooled with the lock still held.
func (c *Courier) keeping(ctx context.Context, project string) (*pgxpool.Conn, func(), error) {
	conn, err := c.Pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	key := KeepLock(project)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		_ = conn.Conn().Close(context.WithoutCancel(ctx))
		conn.Release()
		return nil, nil, err
	}
	return conn, func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}, nil
}

func ignoreNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// RepoName is a kept repository's name: the app's name lowercased, each run of anything but letters and digits
// one dash, trimmed, cut to 50, "app" when nothing is left, then a dash and four letters or digits.
func RepoName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	base = strings.TrimRight(base[:min(len(base), 50)], "-")
	if base == "" {
		base = "app"
	}
	suffix := make([]byte, 4)
	for i := range suffix {
		n, _ := rand.Int(rand.Reader, big.NewInt(36))
		suffix[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[n.Int64()]
	}
	return base + "-" + string(suffix)
}

func (c *Courier) keepError(ctx context.Context, q querier, project, why string) error {
	_, err := q.Exec(ctx, "UPDATE projects SET keep_error = $2 WHERE id = $1", project, why)
	return err
}

// passedRun waits for the first run of the version since it was made current and answers its id when it let the
// version through — clean, or blocked with every blocking finding approved — and the version is still current.
// "" otherwise, or when no run ends in time.
func (c *Courier) passedRun(ctx context.Context, k Keep) (string, error) {
	for deadline := time.Now().Add(keepWait); time.Now().Before(deadline); {
		var run, state string
		var current bool
		err := c.Pool.QueryRow(ctx, `SELECT r.id, r.state,
			(SELECT p.id FROM packages p WHERE p.project_id = $1 AND p.source_kind NOT IN ('repo-pr', 'check') ORDER BY p.created_at DESC, p.id DESC LIMIT 1) = $2
			FROM runs r JOIN packages p ON p.id = r.package_id AND p.project_id = r.project_id
			WHERE r.project_id = $1 AND r.package_id = $2 AND r.kind = 'gate' AND r.created_at >= p.created_at AND r.ended_at IS NOT NULL
			ORDER BY r.created_at LIMIT 1`, k.Project, k.Package).Scan(&run, &state, &current)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return "", err
		case !current || (state != "clean" && state != "blocked"):
			return "", nil
		case state == "clean":
			return run, nil
		default:
			var findings json.RawMessage
			if err := c.Pool.QueryRow(ctx, "SELECT t.data->'findings' FROM timeline_rows t WHERE t.run_id = $1 AND t.kind = 'verdict'", run).Scan(&findings); err != nil {
				return "", err
			}
			_, unapproved, err := runs.Approvals(ctx, c.Pool, k.Project, run, findings)
			if err != nil || unapproved > 0 {
				return "", err
			}
			return run, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(keepPoll):
		}
	}
	return "", nil
}

// commitKept commits the version on top of its recorded base with a plain push, never forced: when main has moved
// past it, or GitHub refuses the push, the link's kept_error says why and nothing is overwritten.
func (c *Courier) commitKept(ctx context.Context, k Keep, run string) (string, error) {
	conn, done, err := c.keeping(ctx, k.Project)
	if err != nil {
		return "", err
	}
	defer done()
	var repoID, installation int64
	var fullName, base, tree, label string
	current := func() error { // the kept link, and the version still the app's current one
		return conn.QueryRow(ctx, `SELECT l.repo_id, l.installation_id, l.full_name, p.kept_base, p.tree_hash, COALESCE(p.label, '')
		FROM project_repositories l JOIN packages p ON p.project_id = l.project_id AND p.id = $2 WHERE l.project_id = $1 AND l.keeps AND l.state = 'linked'
		  AND p.id = (SELECT c.id FROM packages c WHERE c.project_id = $1 AND c.source_kind NOT IN ('repo-pr', 'check') ORDER BY c.created_at DESC, c.id DESC LIMIT 1)`,
			k.Project, k.Package).Scan(&repoID, &installation, &fullName, &base, &tree, &label)
	}
	if err := current(); err != nil {
		return "", ignoreNoRows(err)
	}
	version := cmp.Or(label, strings.TrimPrefix(k.Package, "pkg_")) // contracts/routes.md, Naming a version
	app, err := LoadApp(ctx, conn, c.Vault, installation)
	if err != nil {
		return "", err
	}
	gh := New(c.API, app.ID, app.Key)
	var remote struct {
		CloneURL string `json:"clone_url"`
	}
	if _, err := gh.as(ctx, installation, http.MethodGet, "/repositories/"+strconv.FormatInt(repoID, 10), githubJSON, nil, &remote); err != nil {
		return "", errors.Join(c.keptError(ctx, conn, k.Project, "Canyon's App can't reach "+fullName+": "+err.Error()), err)
	}
	var bot struct{ ID int64 } // the App's bot user, whose noreply address attributes the commit to it
	if _, err := gh.as(ctx, installation, http.MethodGet, "/users/"+url.PathEscape(app.Slug+"[bot]"), githubJSON, nil, &bot); err != nil {
		return "", err
	}
	if !strings.HasPrefix(remote.CloneURL, cloneScheme) { // git reads anything else as an option or another transport
		return "", fmt.Errorf("GitHub answered a clone url that isn't %s…", cloneScheme)
	}
	token, err := gh.token(ctx, installation)
	if err != nil {
		return "", err
	}
	g := gitAs{dir: filepath.Join(c.Repos, k.Project+".git"), token: token, bot: app.Slug + "[bot]", botID: bot.ID}
	head, err := g.head(ctx, remote.CloneURL)
	if err != nil {
		return "", errors.Join(c.keptError(ctx, conn, k.Project, "GitHub refused "+version+": "+err.Error()), err)
	}
	message, at, err := c.keptMessage(ctx, conn, k, run, version)
	if err != nil {
		return "", err
	}
	g.date = at // the commit is the same commit every time it is made, so a retry knows its own push
	if head != base {
		mine, err := g.commit(ctx, remote.CloneURL, tree, base, message)
		if err != nil { // git or GitHub failed: said, and tried again
			return "", errors.Join(c.keptError(ctx, conn, k.Project, "GitHub refused "+version+": "+err.Error()), err)
		}
		if mine != head {
			return "", c.keptError(ctx, conn, k.Project, version+" is deployed but not in GitHub: someone pushed to main after it was made, and Canyon never overwrites a push. Run canyon pull, then canyon deploy.")
		}
		return c.keptAs(ctx, conn, k, run, head) // pushed before a retry: only the record is left
	}
	commit, err := g.commit(ctx, remote.CloneURL, tree, base, message)
	if err == nil {
		if err = current(); err != nil { // an upload made another version current meanwhile: that one's keep commits
			return "", ignoreNoRows(err)
		}
		err = g.push(ctx, remote.CloneURL, commit)
	}
	if err != nil {
		why := "GitHub refused " + version + ": " + err.Error()
		if strings.Contains(why, "GH006") || strings.Contains(why, "GH013") { // GitHub's codes for a protected branch and a ruleset
			why += " Add Canyon's App to the rule's bypass list on GitHub, or hand this repository to developers."
		}
		return "", errors.Join(c.keptError(ctx, conn, k.Project, why), err) // said, and tried again: a refusal is refused again, a dropped connection isn't
	}
	return c.keptAs(ctx, conn, k, run, commit)
}

// keptAs records commit as the version's kept commit, with its audit row.
func (c *Courier) keptAs(ctx context.Context, conn *pgxpool.Conn, k Keep, run, commit string) (string, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE project_repositories SET kept_commit = $2, kept_error = '' WHERE project_id = $1", k.Project, commit); err != nil {
		return "", err
	}
	if err := events.AuditTx(fromGitHub(ctx), tx, canyonActor, "keep version in github", k.Project, run, map[string]any{"package": k.Package, "commit": commit}); err != nil {
		return "", err
	}
	return commit, tx.Commit(ctx)
}

func (c *Courier) keptError(ctx context.Context, q querier, project, why string) error {
	_, err := q.Exec(ctx, "UPDATE project_repositories SET kept_error = $2 WHERE project_id = $1", project, why)
	return err
}

// keptMessage is the commit's message (contracts/routes.md, Kept in GitHub): the version and the app, who
// deployed or uploaded it, the review's counts, then the trailers.
func (c *Courier) keptMessage(ctx context.Context, q querier, k Keep, run, version string) (string, time.Time, error) {
	var blocking, warnings int
	var deployed bool
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT COALESCE((t.data->>'blocking')::int, 0), COALESCE((t.data->>'warnings')::int, 0),
		EXISTS (SELECT 1 FROM audit_events e WHERE e.run_id = $1 AND e.action = 'POST /api/v1/projects/{id}/deploys'), r.ended_at
		FROM timeline_rows t JOIN runs r ON r.id = t.run_id WHERE t.run_id = $1 AND t.kind = 'verdict'`, run).Scan(&blocking, &warnings, &deployed, &at); err != nil {
		return "", at, err
	}
	who := strings.Join(strings.Fields(k.Display), " ") // one line: a name never adds a trailer of its own
	how := "Uploaded by " + who + "."
	if deployed {
		how = "Deployed with canyon deploy by " + who + "."
	}
	msg := fmt.Sprintf("%s · %s\n\n%s\nCanyon's review passed: %d blocking, %d advisory.\n\nCanyon-Version: %s\nCanyon-Review: %s/#/apps/%s",
		version, k.Project, how, blocking, warnings, k.Package, c.Public, k.Project)
	if k.Email != "" {
		msg += fmt.Sprintf("\nCo-authored-by: %s <%s>", who, k.Email)
	}
	return msg, at, nil
}

// gitAs runs git on the app's own repository as the App: its installation token rides in an http header set
// through git's environment, never in a URL, an argument or a log line.
type gitAs struct {
	dir, token, bot string
	botID           int64
	date            time.Time // the commit's author and committer date
}

// run runs git as the App's bot, attributed to its noreply address (internal/gitpush): GitHub's own words, its
// remote: lines, else git's last; never the token.
func (g gitAs) run(ctx context.Context, args ...string) (string, error) {
	email := strconv.FormatInt(g.botID, 10) + "+" + g.bot + "@users.noreply.github.com"
	return gitpush.Git{Dir: g.dir, User: "x-access-token", Token: g.token, Name: g.bot, Email: email, Date: g.date}.Run(ctx, args...)
}

// head is the commit the remote's main is at, "" for a repository with none yet.
func (g gitAs) head(ctx context.Context, url string) (string, error) {
	out, err := g.run(ctx, "ls-remote", url, "refs/heads/main")
	if err != nil || out == "" {
		return "", err
	}
	sha, _, _ := strings.Cut(out, "\t")
	return sha, nil
}

// commit makes one commit of tree on top of parent, fetched first so git holds it; no parent for a repository's first.
func (g gitAs) commit(ctx context.Context, url, tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		if _, err := g.run(ctx, "fetch", "--no-tags", url, "refs/heads/main"); err != nil {
			return "", err
		}
		args = append(args, "-p", parent)
	}
	return g.run(ctx, args...)
}

// push sends commit to main, never forced: git refuses anything but a fast-forward on its own.
func (g gitAs) push(ctx context.Context, url, commit string) error {
	_, err := g.run(ctx, "push", url, commit+":refs/heads/main")
	return err
}
