package githubapp

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
	"github.com/pelletier/go-toml/v2"
)

// Workflow is the name the check workflow registers under; a delivery's id is its workflow id.
const Workflow = "github-check"

// Check is what the workflow receives: the commit to review and who pushed it, frozen at the hook.
type Check struct {
	Installation int64  `json:"installation"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	Login        string `json:"login"`
	User         int64  `json:"user,omitempty"`         // the pushing account's numeric id: whoever linked it authors the run (#2228)
	PullRequest  bool   `json:"pull_request,omitempty"` // a pull request's head: reviewed, never the project's current package
	Ref          string `json:"ref,omitempty"`          // a push's branch, refs/heads/<default>: whose code-scanning analyses are read
	Project      string `json:"project,omitempty"`      // the app linked to the repository (#1917): "" is no app, the one neutral check
	RepoID       int64  `json:"repo_id,omitempty"`      // the repository's GitHub id, which the link names
	Folder       string `json:"folder,omitempty"`       // the link's folder, "" its root
	Number       int    `json:"number,omitempty"`       // a pull request's number and head branch, its package's (#1947)
	Branch       string `json:"branch,omitempty"`
}

// name is the check's: Canyon gate for a root link, so required-check rules hold, and one per folder.
func (in Check) name() string {
	if in.Folder == "" {
		return "Canyon gate"
	}
	return "Canyon gate · " + in.Folder
}

// The review's bounds: a start waits out another running run of the project busyTries times,
// busyWait apart, and the run is read every poll until it ends or ceiling passes.
var busyWait, poll = 15 * time.Second, 5 * time.Second

const busyTries, ceiling = 40, 35 * time.Minute

// A tool's analysis at the commit is asked for reportTries more times, reportWait apart: ten minutes.
var reportWait = 30 * time.Second

const reportTries = 20

// maxReport is the most of an analysis's SARIF read, sarifDeadline the longest its download takes.
const maxReport = 10 << 20

var sarifDeadline = 2 * time.Minute

// retried is every step's retry: a failed GitHub or database call is tried three more times, backing off.
var retried = dbos.WithStepMaxRetries(3)

// answered stops retried at GitHub's 4xx, which asking again does not change: code scanning off, a permission not granted.
var answered = dbos.WithStepRetryPredicate(func(err error) bool {
	var status statusError
	return !errors.As(err, &status) || status/100 != 4
})

// fromGitHub is ctx with the trail's channel for what the App's work writes (contracts/routes.md, the audit read).
func fromGitHub(ctx context.Context) context.Context {
	return events.WithProvenance(ctx, events.Provenance{Via: "github"})
}

// Register puts the workflow on dc, before dbos.Launch; it rides the gate runs' queue.
func (c *Courier) Register(dc dbos.Context) {
	dbos.RegisterWorkflow(dc, c.check, dbos.WithWorkflowName(Workflow), dbos.WithMaxRecoveryAttempts(3))
	dbos.RegisterWorkflow(dc, c.keep, dbos.WithWorkflowName(KeepWorkflow), dbos.WithMaxRecoveryAttempts(3))
}

var errOtherApp = errors.New("canyon.toml names another app")

// appLine is the app canyon.toml at tree's root names, "" when it names none or does not read: the
// declarations' own check says what is wrong with a file that does not.
func appLine(tree string) string {
	f, err := os.Open(filepath.Join(tree, "canyon.toml")) //nolint:gosec // the package's own tree, after ingest's checks
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10+1)) // the declarations' cap, read no further
	var file struct {
		App string `toml:"app"`
	}
	if err != nil || len(raw) > 64<<10 || toml.Unmarshal(raw, &file) != nil {
		return ""
	}
	return file.App
}

// ending is how a check ends, the package the commit became and the run that reviewed it, if either happened.
type ending struct {
	Package, Run, Conclusion, Title, Summary string
	Tree                                     string // a folder's GitHub tree a push packaged: the link's once its review finishes clean or blocked (#1917)
	Deploy                                   bool   // the run deploys to preview: its check ends on its verdict, never its build (#1923)
	Annotations                              []Annotation
	Refusal                                  error // a tree packCommit refused: errOtherApp, the declarations' error, or ingest's
}

// check opens the app's `Canyon gate` check, packages and reviews the commit, ends the check with the findings and
// answers the package's id. An opened check never stays in progress: a step's error past its retries fails it.
func (c *Courier) check(ctx dbos.Context, in Check) (_ string, err error) {
	app, err := LoadApp(ctx, c.Pool, c.Vault, in.Installation) // the App the push or pull request came to
	if err != nil {
		return "", err
	}
	gh := New(c.API, app.ID, app.Key)
	id, err := dbos.RunAsStep(ctx, func(s context.Context) (int64, error) {
		return gh.CreateCheckRun(s, in.Installation, in.Owner, in.Repo, CheckRun{Name: in.name(), HeadSHA: in.SHA, Status: "in_progress"})
	}, dbos.WithStepName("check-created"), retried)
	if err != nil {
		return "", err
	}
	defer func() { // every error's one path, logged; but a shutdown or a cancel is DBOS's to resume or end
		if err == nil || ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) {
			return
		}
		c.Log.Printf("canyon-server: github check of %s/%s at %s: %v", in.Owner, in.Repo, in.SHA, err)
		_, _ = dbos.RunAsStep(ctx, func(s context.Context) (struct{}, error) {
			return struct{}{}, gh.UpdateCheckRun(s, in.Installation, in.Owner, in.Repo, id, CheckRun{Status: "completed", Conclusion: "failure", Output: &Output{Title: "Canyon could not finish this review", Summary: "Push again; if it keeps failing, the Canyon server's log says why."}})
		}, dbos.WithStepName("unfinished"), retried)
	}()
	end, err := dbos.RunAsStep(ctx, func(s context.Context) (ending, error) { return c.pack(s, gh, in) }, dbos.WithStepName("package"), retried)
	if err == nil && end.Conclusion == "" {
		if err = c.reports(ctx, gh, in, end.Package); err == nil {
			end, err = c.review(ctx, in, end.Package, end.Tree)
		}
	}
	if err != nil {
		return "", err
	}
	for batch := end.Annotations; ; { // 50 to a request, each appending to the last; the conclusion rides the last
		run := CheckRun{Output: &Output{Title: end.Title, Summary: end.Summary, Annotations: batch[:min(len(batch), 50)]}}
		if batch = batch[len(run.Output.Annotations):]; len(batch) == 0 {
			run.Status, run.Conclusion = "completed", end.Conclusion
		}
		if _, err := dbos.RunAsStep(ctx, func(s context.Context) (struct{}, error) {
			return struct{}{}, gh.UpdateCheckRun(s, in.Installation, in.Owner, in.Repo, id, run)
		}, dbos.WithStepName("conclusion"), retried); err != nil || len(batch) == 0 {
			return end.Package, err
		}
	}
}

// unlinked ends the check of a repository no live app is linked to (#1917).
var unlinked = ending{Conclusion: "neutral", Title: "This repository isn't linked to a Canyon app", Summary: "An admin links it in Settings › GitHub."}

// review starts a gate run of the package, waiting out another running run of the project, and awaits
// its end. A project still busy past the retries fails the check: a review was due and was not done.
func (c *Courier) review(ctx dbos.Context, in Check, pkg, tree string) (ending, error) {
	for try := 1; ; try++ {
		end, err := dbos.RunAsStep(ctx, func(s context.Context) (ending, error) { return c.start(s, in, pkg) }, dbos.WithStepName("run"), retried)
		switch {
		case err != nil || end.Conclusion != "":
			return end, err
		case end.Run != "":
			return dbos.RunAsStep(ctx, func(s context.Context) (ending, error) { return c.await(s, in, end, tree) }, dbos.WithStepName("await"), retried)
		case try > busyTries:
			return ending{Package: pkg, Conclusion: "failure", Title: "Another Canyon run was in progress", Summary: "Another Canyon run for " + in.Project + " was in progress; push again."}, nil
		}
		if _, err := dbos.Sleep(ctx, busyWait); err != nil {
			return ending{}, err
		}
	}
}

// start records a gate run of the package as a user's start does, under the lock a project delete marks
// under: a project deleted or being deleted since is no app; a running run of the project is no run yet.
func (c *Courier) start(ctx context.Context, in Check, pkg string) (ending, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return ending{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live, deploy bool
	if err := events.LockTx(ctx, tx); err != nil {
		return ending{}, err
	}
	// a deploy is the switch's and the link's as they stand now, under the lock: a push from a repository the app
	// was unlinked from, or one archived since, is reviewed and never deployed (#1923)
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL AND deleting_since IS NULL),
		EXISTS (SELECT 1 FROM projects p JOIN project_repositories l ON l.project_id = p.id
			WHERE p.id = $1 AND p.auto_deploy AND l.repo_id = $2 AND l.folder = $3 AND l.state = 'linked')`, in.Project, in.RepoID, in.Folder).Scan(&live, &deploy); err != nil || !live {
		return unlinked, err
	}
	deploy = deploy && !in.PullRequest // a push deploys to preview where its owners turned that on; a pull request never (#1923)
	snap, err := c.Policy(ctx, tx, in.Project)
	if err != nil {
		return ending{}, err
	}
	run, superseded, err := runs.StartTx(ctx, tx, c.Durable, runs.StartInput{Project: in.Project, Package: pkg, Deploy: deploy, Actor: actor(in), GitHub: in.User, Policy: snap})
	if errors.Is(err, runs.ErrRunInProgress) || errors.Is(err, runs.ErrUndeploying) { // no run yet: review waits and tries again
		return ending{Package: pkg}, nil
	}
	var limit *runs.RateLimited
	if errors.As(err, &limit) { // no retry: a review due and not done fails the check, as a busy project's does
		return limitReached(in, pkg, limit, time.Now()), nil
	}
	if err != nil {
		return ending{}, err
	}
	what := "gate run from github"
	if deploy {
		what = "deploy from github"
	}
	if err := events.AuditTx(fromGitHub(ctx), tx, actor(in), what, in.Project, run.ID, map[string]any{"package": pkg}); err != nil {
		return ending{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ending{}, err
	}
	runs.Cancel(c.Durable, superseded)
	return ending{Package: pkg, Run: run.ID, Deploy: deploy}, nil
}

// reviewed moves a push's folder tree onto its link once its review let it through — clean, or every
// blocking finding approved, the check's success: a push that leaves the folder alone then has nothing
// new, and one whose review did not finish, or blocked it, is reviewed again (#1917).
func (c *Courier) reviewed(ctx context.Context, in Check, tree, conclusion string) error {
	if in.PullRequest || in.Folder == "" || conclusion != "success" {
		return nil
	}
	_, err := c.Pool.Exec(ctx, "UPDATE project_repositories SET last_tree = $2 WHERE project_id = $1 AND repo_id = $3 AND folder = $4", in.Project, tree, in.RepoID, in.Folder)
	return err
}

// limitReached ends the check of a start past a gate-run limit, naming when a push is let through again.
func limitReached(in Check, pkg string, limit *runs.RateLimited, now time.Time) ending {
	whose := "this app"
	if limit.PerActor {
		whose = in.Login
	}
	again := now.Add(limit.RetryAfter + time.Minute - 1).Truncate(time.Minute).UTC().Format("15:04")
	line := fmt.Sprintf("Canyon's limit of %d reviews an hour for %s is reached. Push again after %s UTC.", limit.Limit, whose, again)
	return ending{Package: pkg, Conclusion: "failure", Title: "Canyon's review limit is reached", Summary: line}
}

// await reads the run every poll until it ends, and concludes from it; past the ceiling the check fails.
func (c *Courier) await(ctx context.Context, in Check, end ending, tree string) (ending, error) {
	for deadline := time.Now().Add(ceiling); ; {
		var state, reason, step, verdict string
		var done bool
		err := c.Pool.QueryRow(ctx, `SELECT state, COALESCE(failure_reason, ''), COALESCE(failed_in, ''), ended_at IS NOT NULL,
			COALESCE((SELECT t.data->>'state' FROM timeline_rows t WHERE t.run_id = runs.id AND t.kind = 'verdict'), '') FROM runs WHERE id = $1`, end.Run).Scan(&state, &reason, &step, &done, &verdict)
		switch {
		case err != nil:
			return ending{}, err
		case end.Deploy && verdict != "": // a deploy's review is done at its verdict; the build and the deploy are the app page's
			end, err := concluded(ctx, c.Pool, end, verdict, "", "")
			if err == nil {
				err = c.reviewed(ctx, in, tree, end.Conclusion)
			}
			return end, err
		case done:
			end, err := concluded(ctx, c.Pool, end, state, reason, step)
			if err == nil {
				err = c.reviewed(ctx, in, tree, end.Conclusion)
			}
			return end, err
		case time.Now().After(deadline):
			end.Conclusion, end.Title, end.Summary = "failure", "The review did not finish", "Canyon's review of "+end.Package+" did not finish in 35 minutes."
			return end, nil
		}
		select {
		case <-ctx.Done():
			return ending{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// pack makes the commit, or the link's folder of it, a package of the linked app, as an upload does: a
// push's the current one, a pull request's never; the check fails on a refused tree. A folder that did
// not change since the link's last push is not packaged.
func (c *Courier) pack(ctx context.Context, gh *Client, in Check) (ending, error) {
	var last string // the app still live and linked as the delivery found it: unlinked, relinked or archived since, nothing is packaged
	err := c.Pool.QueryRow(ctx, `SELECT l.last_tree FROM project_repositories l JOIN projects p ON p.id = l.project_id AND p.deleted_at IS NULL
		WHERE l.project_id = $1 AND l.repo_id = $2 AND l.folder = $3 AND l.state = 'linked'`, in.Project, in.RepoID, in.Folder).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return unlinked, nil
	}
	if err != nil {
		return ending{}, err
	}
	var tree string // a folder's GitHub tree at the commit: unchanged since the link's last reviewed push, nothing to do
	if in.Folder != "" {
		if tree, err = gh.FolderTree(ctx, in.Installation, in.Owner, in.Repo, in.SHA, in.Folder); err != nil {
			return ending{}, err
		}
		switch tree {
		case "":
			return ending{Conclusion: "failure", Title: in.Folder + " isn't in this commit", Summary: "Canyon packages this app from " + in.Folder + ". Move it back, or an admin links the app to its new folder."}, nil
		case last:
			return ending{Conclusion: "neutral", Title: "No change under " + in.Folder, Summary: "Nothing under " + in.Folder + " changed since this app's last version, so there is nothing new to review."}, nil
		}
	}
	kind := "repo"
	if in.PullRequest {
		kind = "repo-pr"
	}
	return c.packCommit(fromGitHub(ctx), gh, in, tree, kind, actor(in), "package from github")
}

// packCommit makes the commit, or the link's folder of it, a package of in.Project as kind, by the actor, with
// one audit row under action: "repo" the current package, as a push's; "repo-pr" and "check" never the current
// one, as a pull request's and a check's upload's are. A refused tree is an ending with a failure, nothing packaged.
func (c *Courier) packCommit(ctx context.Context, gh *Client, in Check, tree, kind string, by events.Actor, action string) (ending, error) {
	body, err := gh.Tarball(ctx, in.Installation, in.Owner, in.Repo, in.SHA)
	if err != nil {
		return ending{}, fmt.Errorf("%w: %w", ErrGitHub, err)
	}
	defer func() { _ = body.Close() }()
	gz, err := gzip.NewReader(body)
	if err != nil {
		return ending{}, err
	}
	var declared, roles map[string]string
	var description *string
	var refused error
	var named string
	pkg, err := ingest.IngestFolder(ctx, filepath.Join(c.Repos, in.Project+".git"), gz, c.Exclusions, in.Folder, kind == "repo", func(tree string) error {
		if named = appLine(tree); named != "" && named != in.Project {
			return errOtherApp
		}
		declared, roles, description, refused = c.Declare(tree)
		return refused
	})
	if errors.Is(err, errOtherApp) { // an app line is optional; one that is there says which app the code is (#1938)
		if r := []rune(named); len(r) > 80 { // the pusher's words, kept to a title's length
			named = string(r[:80]) + "…"
		}
		named = strconv.QuoteToGraphic(named)
		named = named[1 : len(named)-1] // one line of visible characters, unquoted
		return ending{Conclusion: "failure", Title: "canyon.toml names " + named + ", not " + in.Project,
			Summary: "This repository feeds " + in.Project + ". Change the app line to " + in.Project + ", or delete it.", Refusal: errOtherApp}, nil
	}
	if refused != nil || errors.Is(err, ingest.ErrRequest) {
		return ending{Conclusion: "failure", Title: "Canyon refused this commit", Summary: err.Error(), Refusal: err}, nil
	}
	if err != nil {
		return ending{}, err
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return ending{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return ending{}, err
	}
	var firsts []string
	if kind == "repo" {
		if declared == nil { // a tree a pull request or a check packaged first: its declarations become the project's with this push
			if err := tx.QueryRow(ctx, "SELECT resources FROM packages WHERE id = $1 AND project_id = $2 AND source_kind IN ('repo-pr', 'check')", pkg.ID, in.Project).Scan(&declared); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return ending{}, err
			}
		}
		if firsts, err = c.FirstDeclarations(ctx, tx, in.Project, declared); err != nil { // read before the insert, as an upload's
			return ending{}, err
		}
	}
	// as an upload's: created_at is never before the project's newest package, and the same tree again keeps its first
	// source; but a push of a tree only a pull request or a check packaged makes it current, as a push of a new tree would
	if _, err := tx.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_ref, source_actor, resources, roles, description, pr_number, pr_branch, created_at) VALUES ($1, $2, $3, $4, $5, $9, $6, $7, $8, $10, $11, $12, $13, GREATEST(now(), (SELECT max(created_at) + interval '1 microsecond' FROM packages WHERE project_id = $2)))
		ON CONFLICT (id, project_id) DO UPDATE SET source_kind = 'repo', source_ref = EXCLUDED.source_ref, source_actor = EXCLUDED.source_actor, created_at = EXCLUDED.created_at
		WHERE packages.source_kind IN ('repo-pr', 'check') AND EXCLUDED.source_kind = 'repo'`,
		pkg.ID, in.Project, pkg.TreeHash, pkg.Commit, pkg.Bytes, in.SHA, by.Subject, declared, kind, roles, description, in.Number, in.Branch); err != nil {
		return ending{}, err
	}
	if err := events.AuditTx(ctx, tx, by, action, in.Project, "", map[string]any{"package": pkg.ID, "ref": in.SHA, "kind": kind}); err != nil {
		return ending{}, err
	}
	for _, name := range firsts {
		if err := events.AuditTx(ctx, tx, by, "declare resource", in.Project, "", map[string]any{"name": name, "type": declared[name], "package": pkg.ID}); err != nil {
			return ending{}, err
		}
	}
	return ending{Package: pkg.ID, Tree: tree}, tx.Commit(ctx)
}

// imports is what a check reads of the composed policy: the tools a run imports, and the per-actor limit.
type imports struct {
	Tools []string
	Limit int
}

// scanned is one reading of the analyses: of the tools a run imports, the newest analysis at the commit per
// (tool, category), and the tools with analyses but none at the commit yet.
type scanned struct {
	Due     []Analysis
	Pending []string
}

// reports attaches the commit's code-scanning analyses to its package before the review starts, when a rule
// sarif.<tool> is on. Matched on the commit, never the tree: ingest leaves files out. A tool with analyses
// but none at the commit is waited for, a pull request's never: GitHub analyses its merge commit. Each attaches
// as the pusher, so the line reads "report attached by <login>" and counts toward the pusher's rate limit; a
// pusher past it attaches nothing. GitHub's refusal (code scanning off, the permission not granted), the
// attach's, and a tool waited for in vain are each one log line and no report for the package: the review goes on.
func (c *Courier) reports(ctx dbos.Context, gh *Client, in Check, pkg string) error {
	imp, err := dbos.RunAsStep(ctx, func(s context.Context) (imports, error) { return c.imports(s, in.Project) }, dbos.WithStepName("imports"), retried)
	if err != nil || len(imp.Tools) == 0 {
		return err
	}
	var got scanned
	for try := 0; ; try++ {
		got, err = dbos.RunAsStep(ctx, func(s context.Context) (scanned, error) {
			all, err := gh.Analyses(s, in.Installation, in.Owner, in.Repo, in.Ref)
			return atCommit(all, imp.Tools, in.SHA), err
		}, dbos.WithStepName("analyses"), retried, answered)
		if err != nil {
			c.Log.Printf("canyon-server: github check of %s/%s at %s: code scanning not read: %v", in.Owner, in.Repo, in.SHA, err)
			return nil
		}
		if len(got.Pending) == 0 || in.PullRequest {
			break
		}
		if try == reportTries {
			for _, tool := range got.Pending {
				c.Log.Printf("canyon-server: github check of %s/%s: %s: no code-scanning analysis for %s after 10 minutes", in.Owner, in.Repo, tool, in.SHA)
			}
			break
		}
		if _, err := dbos.Sleep(ctx, reportWait); err != nil {
			return err
		}
	}
	for _, a := range got.Due {
		within, err := dbos.RunAsStep(ctx, func(s context.Context) (bool, error) { return c.attach(s, gh, in, pkg, imp.Limit, a) }, dbos.WithStepName("report"))
		if err != nil {
			c.Log.Printf("canyon-server: github check of %s/%s at %s: %s analysis %d not attached: %v", in.Owner, in.Repo, in.SHA, a.Tool.Name, a.ID, err)
			continue
		}
		if !within {
			c.Log.Printf("canyon-server: github check of %s/%s at %s: %s is past the limit of %d reviews an hour: no code-scanning report attached", in.Owner, in.Repo, in.SHA, in.Login, imp.Limit)
			return nil
		}
	}
	return nil
}

// imports reads the project's composed policy as a run's start does: its tools are those whose rule sarif.<tool> is on (contracts/snapshot.md, Two gates).
func (c *Courier) imports(ctx context.Context, project string) (imports, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return imports{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	snap, err := c.Policy(ctx, tx, project)
	if err != nil {
		return imports{}, err
	}
	imp := imports{Limit: snap.Limits.GateRunsPerActorPerHour}
	for _, r := range snap.Rules {
		if tool, ok := strings.CutPrefix(r.ID, "sarif."); ok && r.Producer == policy.Sarif && r.On() {
			imp.Tools = append(imp.Tools, tool)
		}
	}
	return imp, nil
}

// atCommit reads the analyses for tools at sha, a tool named as finding.md normalises a driver's name.
func atCommit(all []Analysis, tools []string, sha string) scanned {
	var got scanned
	at := map[string]bool{}    // a tool with analyses: whether one is at sha
	due := map[[2]string]int{} // (tool, category): its analysis's index in got.Due
	for _, a := range all {
		tool := strings.Join(strings.FieldsFunc(strings.ToLower(a.Tool.Name), func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') }), "-")
		if !slices.Contains(tools, tool) {
			continue
		}
		at[tool] = at[tool] || a.CommitSHA == sha
		if a.CommitSHA != sha {
			continue
		}
		if i, ok := due[[2]string{tool, a.Category}]; !ok {
			due[[2]string{tool, a.Category}] = len(got.Due)
			got.Due = append(got.Due, a)
		} else if a.CreatedAt.After(got.Due[i].CreatedAt) {
			got.Due[i] = a
		}
	}
	for _, tool := range tools {
		if seen, ok := at[tool]; ok && !seen {
			got.Pending = append(got.Pending, tool)
		}
	}
	return got
}

// attach reads the analysis's SARIF, to the reports route's cap within sarifDeadline, and attaches it to the
// package as the pusher, under GitHub's category; false, reading nothing, when the pusher is past the per-actor
// limit, as the route judges it.
func (c *Courier) attach(ctx context.Context, gh *Client, in Check, pkg string, limit int, a Analysis) (bool, error) {
	err := runs.ActorLimited(ctx, c.Pool, limit, actor(in))
	var limited *runs.RateLimited
	if errors.As(err, &limited) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	download, cancel := context.WithTimeout(ctx, sarifDeadline)
	defer cancel()
	body, err := gh.AnalysisSARIF(download, in.Installation, in.Owner, in.Repo, a.ID)
	if err != nil {
		return true, err
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, maxReport+1))
	if err != nil {
		return true, err
	}
	if len(raw) > maxReport {
		return true, errors.New("the report is over 10 MiB")
	}
	return true, c.Attach(fromGitHub(ctx), in.Project, pkg, categorised(raw, a.Category), actor(in)) // its attach report row is the App's work
}

// categorised is raw with its run's automationDetails.id set to the analysis's category. The download is "a subset
// of the analysis data", so the id may not survive it, and a language matrix's reports would then share one
// category and replace each other. The attach digests the report as rewritten; a body not of one run is left as
// it is, for the attach to refuse.
func categorised(raw []byte, category string) []byte {
	var report map[string]json.RawMessage
	var run []map[string]json.RawMessage
	if json.Unmarshal(raw, &report) != nil || json.Unmarshal(report["runs"], &run) != nil || len(run) != 1 || run[0] == nil {
		return raw
	}
	run[0]["automationDetails"], _ = json.Marshal(map[string]string{"id": category}) // strings and raw JSON: none can fail
	report["runs"], _ = json.Marshal(run)
	out, _ := json.Marshal(report)
	return out
}

// actor is the courier acting for the GitHub user who pushed: an agent, so it can never approve a finding.
func actor(in Check) events.Actor {
	return events.Actor{Kind: "agent", Subject: "github:" + in.Login, Display: in.Login}
}
