package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gitops"
	"github.com/jackc/pgx/v5"
)

// handOff is the run's one journaled read of where its app goes (design/HAND-OFF.md §1, §3b): whether the
// org's platform runs it, and how its releases reach the platform. v6's "run by" step answers it whole.
type handOff struct {
	Org bool   `json:"org"`
	Via string `json:"via,omitempty"` // webhook | repo; "" in a run that read run_by alone, which is webhook
}

// handOffOf reads run_by and delivery together, so a switch of either between the two never ships a release
// one way that was decided the other.
func (p *Pipeline) handOffOf(ctx context.Context) (handOff, error) {
	var runBy, via string
	err := p.Pool.QueryRow(ctx, "SELECT run_by, delivery FROM org_preferences").Scan(&runBy, &via)
	return handOff{Org: runBy == "org", Via: via}, err
}

// deployRepoLock is the advisory lock every commit to the deploy repo takes: pushes to one repo take turns, so
// what each re-reads right before its push is what it pushes over.
const deployRepoLock = "canyon-deploy-repo"

// deployRepoDir is Canyon's bare repository of the deploy repo: no project id has an underscore, so it is never
// an app's.
const deployRepoDir = "_deploy-repo.git"

// DeployRepo is the platform-repo row (contracts/routes.md, the platform-repo row) as the repo Canyon commits to,
// its token opened from the vault: pgx.ErrNoRows when none is saved.
func (p *Pipeline) DeployRepo(ctx context.Context, q Querier) (*gitops.Repo, error) {
	var r gitops.Repo
	var mode, api, apiURL string
	var ca *string
	var keyRef int
	var sealedKey, sealedValue []byte
	if err := q.QueryRow(ctx, `SELECT url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, ca FROM platform_repo`).
		Scan(&r.URL, &r.User, &keyRef, &sealedKey, &sealedValue, &r.Preview, &r.Prod, &mode, &r.Root, &api, &apiURL, &r.Store, &ca); err != nil {
		return nil, err
	}
	token, err := p.Vault.Open(keyRef, "platform-repo", "repo", "token", sealedKey, sealedValue)
	if err != nil {
		return nil, err
	}
	r.Token, r.ProdPR, r.Dir, r.Canyon = string(token), mode == "pull_request", filepath.Join(p.Repos, deployRepoDir), p.PublicURL
	if ca != nil {
		r.CA = *ca
	}
	var transport http.RoundTripper = releaseDeliveryTransport()
	if p.RepoTransport != nil {
		transport = p.RepoTransport
	}
	r.HTTP = &http.Client{Transport: transport}
	switch api { // never a nil pointer of either kind in Pulls: nil is api: none
	case "github":
		pulls, err := gitops.NewPulls(apiURL, r.URL, r.Token, r.HTTP)
		if err != nil {
			return nil, err
		}
		r.Pulls = pulls
	case "gitlab":
		mrs, err := gitops.NewMergeRequests(apiURL, r.URL, r.Token, r.HTTP)
		if err != nil {
			return nil, err
		}
		r.Pulls = mrs
	case "none":
	default: // fail closed: an API Canyon does not speak is never read as none
		return nil, fmt.Errorf("runs: the deploy repo's api %q is not one Canyon speaks", api)
	}
	if r.ProdPR && r.Pulls == nil {
		return nil, errors.New("runs: the deploy repo takes production through a pull request, and has no pull requests API")
	}
	return &r, nil
}

// committed is one attempt to commit a release, the step's checkpoint: where it landed, or why not.
type committed struct {
	Problem     *gitops.Problem     `json:"problem,omitempty"`
	Commit      string              `json:"commit,omitempty"`
	Branch      string              `json:"branch,omitempty"`
	Tree        string              `json:"tree,omitempty"`
	PullRequest *gitops.PullRequest `json:"pull_request,omitempty"`
	Address     string              `json:"address,omitempty"`
	Endpoints   []gitops.Endpoint   `json:"endpoints,omitempty"`
	At          string              `json:"at"`
	Stale       bool                `json:"stale,omitempty"`     // a newer release of the environment was written meanwhile: nothing is pushed over it
	Uncleared   bool                `json:"uncleared,omitempty"` // its review no longer clears it (Pipeline.clears)
}

var (
	errStale     = errors.New("runs: a newer release of the environment was written")
	errUncleared = errors.New("runs: its review no longer clears the release")
)

// commitBound is how long one attempt may hold the deploy repo's lock, git's fetch, commit and push together:
// past it the attempt is unreachable and tried again, so a stalled remote never stops every other release.
var commitBound = 3 * time.Minute

// commitOnce is one attempt: the deploy repo loaded, its lock taken on a connection of its own, and the
// delivery made within commitBound, every push preceded by Fresh — the release still the environment's newest,
// not removed since, for a deploy still cleared, and on production not held — so an old retry never writes over
// a newer directory, and nothing reaches production while it is held.
func (p *Pipeline) commitOnce(ctx context.Context, id string, d gitops.Delivery, cleared func(context.Context) bool) (committed, error) {
	at := time.Now().UTC().Format(time.RFC3339)
	repo, err := p.DeployRepo(ctx, p.Pool)
	if err != nil {
		return committed{}, err
	}
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		return committed{}, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", deployRepoLock); err != nil {
		return committed{}, err
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", deployRepoLock); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx)) // never pooled with the lock still held
		}
	}()
	d.Fresh = func(c context.Context) error {
		var newer, held bool
		if err := conn.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM releases n JOIN releases r ON n.project_id = r.project_id AND n.env = r.env
			WHERE r.id = $1 AND n.created_at > r.created_at),
			EXISTS (SELECT 1 FROM releases r JOIN prod_holds h ON h.project_id = r.project_id WHERE r.id = $1 AND r.env = 'prod' AND r.event = 'deploy')`, id).Scan(&newer, &held); err != nil {
			return err
		}
		if newer || held { // a hold's own remove is no deploy: it goes through
			return errStale
		}
		if cleared != nil && !cleared(c) {
			return errUncleared
		}
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, commitBound)
	defer cancel()
	landed, err := repo.Deliver(bounded, d)
	if prob, ok := gitops.IsProblem(err); ok {
		return committed{Problem: prob, At: at}, nil
	}
	if err != nil && bounded.Err() != nil && ctx.Err() == nil { // the bound stopped it, not the run: a stalled remote, tried again
		return committed{Problem: gitops.Unreached(), At: at}, nil
	}
	switch {
	case errors.Is(err, errStale):
		return committed{Stale: true, At: at}, nil
	case errors.Is(err, errUncleared):
		return committed{Uncleared: true, At: at}, nil
	case err != nil:
		return committed{}, err
	}
	return committed{Commit: landed.Commit, Branch: landed.Branch, Tree: landed.Tree, PullRequest: landed.PullRequest, Address: landed.Address, Endpoints: landed.Endpoints, At: at}, nil
}

// commitLoop makes id's delivery as deliver sends a webhook's (design/HAND-OFF.md §3's retries, §3b's
// outcomes): each attempt a step, a problem that may pass tried again a minute apart for five minutes then
// every fifteen, for an hour in all, each retry a line; a refusal, or the hour's end, marks the release and
// says why. say is the run's, nil for a remove, which has no run. It answers the attempt that landed.
func (p *Pipeline) commitLoop(ctx dbos.Context, id, version string, d gitops.Delivery, cleared func(context.Context) bool, say func(string) error) (committed, error) {
	mark := func(state string) error {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.markRelease(c, id, state) }, dbos.WithStepName(state))
		return err
	}
	tell := func(line string) error {
		if say == nil {
			return nil
		}
		return say(line)
	}
	var first time.Time
	for {
		att, err := dbos.RunAsStep(ctx, func(c context.Context) (committed, error) { return p.commitOnce(c, id, d, cleared) }, dbos.WithStepName("commit"))
		if err != nil {
			return committed{}, err
		}
		if att.Stale {
			return att, errBraked
		}
		if att.Uncleared {
			if err := mark("undelivered"); err != nil {
				return committed{}, err
			}
			return att, errNotCleared
		}
		if att.Problem == nil {
			return att, nil
		}
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			_, err := p.Pool.Exec(c, "UPDATE releases SET repo_problem = $2, attempted_at = $3 WHERE id = $1", id, att.Problem.Code, att.At)
			return struct{}{}, err
		}, dbos.WithStepName("problem")); err != nil {
			return committed{}, err
		}
		sentAt, err := time.Parse(time.RFC3339, att.At)
		if err != nil {
			return committed{}, err
		}
		if first.IsZero() {
			first = sentAt
		}
		if !att.Problem.Retries() {
			if err := mark("refused"); err != nil {
				return committed{}, err
			}
			line := "Your deploy repo refused " + version + ": " + att.Problem.Message
			if att.Problem.Code == gitops.TemplateMiss || att.Problem.Code == gitops.TemplateBad || att.Problem.Code == gitops.NameTooLong {
				line = "Canyon couldn't render " + version + ": " + att.Problem.Message
			}
			if err := tell(line); err != nil {
				return committed{}, err
			}
			return att, errReleaseRefused
		}
		elapsed, interval := sentAt.Sub(first), releaseFastRetry
		if elapsed >= releaseFastWindow {
			interval = releaseSlowRetry
		}
		if elapsed+interval > releaseCeiling {
			if err := mark("undelivered"); err != nil {
				return committed{}, err
			}
			if err := tell("Canyon couldn't commit " + version + " to your deploy repo in an hour: " + att.Problem.Message); err != nil {
				return committed{}, err
			}
			return att, errReleaseUndelivered
		}
		if err := tell(fmt.Sprintf("your deploy repo didn't take %s (%s) · retrying in %s", version, att.Problem.Code, shortDuration(interval))); err != nil {
			return committed{}, err
		}
		if _, err := dbos.Sleep(ctx, interval); err != nil {
			return committed{}, err
		}
	}
}

// commitActor is who a commit's own rows are by: Canyon, committing.
var commitActor = events.Actor{Kind: "canyon", Subject: "commit", Display: "Canyon"}

// recordCommit writes where the release landed, and the commit release audit row, once it is accepted.
func (p *Pipeline) recordCommit(ctx context.Context, id string, c committed) error {
	var pr *string
	var number *int
	if c.PullRequest != nil {
		pr, number = &c.PullRequest.URL, &c.PullRequest.Number
	}
	endpoints, err := json.Marshal(c.Endpoints)
	if err != nil {
		return err
	}
	return p.write(ctx, func(tx pgx.Tx) error {
		var project, env, run string
		if err := tx.QueryRow(ctx, `UPDATE releases SET commit_sha = $2, branch = $3, tree = $4, pull_request = $5, pull_number = $6, address = nullif($7, ''), endpoints = $8,
			repo_problem = NULL, attempted_at = $9 WHERE id = $1 RETURNING project_id, env, run_id`, id, c.Commit, c.Branch, c.Tree, pr, number, c.Address, endpoints, c.At).Scan(&project, &env, &run); err != nil {
			return err
		}
		detail := map[string]any{"release": id, "env": env, "commit": c.Commit, "branch": c.Branch}
		if pr != nil {
			detail["pull_request"] = *pr
		}
		return events.AuditTx(ctx, tx, commitActor, "commit release", project, run, detail)
	})
}

// sendByCommit is sendRelease under delivery: repo (design/HAND-OFF.md §3b): the release committed to the deploy
// repo in place of a webhook, then the wait on a pod's check-in.
func (p *Pipeline) sendByCommit(ctx dbos.Context, in Input, rel releaseRow, base deployRelease) error {
	say := func(line string) error { return p.appendBuildLog(ctx, in, line) }
	version := releaseVersion(base)
	d := gitops.Delivery{Release: repoRelease(base, p.PublicURL), Review: base.GateURL, Date: rel.Created}
	cleared := func(c context.Context) bool { return p.clears(c, in) }
	landed, err := p.commitLoop(ctx, rel.ID, version, d, cleared, say)
	if err != nil {
		return err
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.recordCommit(c, rel.ID, landed) }, dbos.WithStepName("committed")); err != nil {
		return err
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.markRelease(c, rel.ID, "accepted") }, dbos.WithStepName("accepted")); err != nil {
		return err
	}
	line := "committed " + version + " to " + landed.Branch + " · waiting for a pod to check in"
	if landed.PullRequest != nil {
		line = "opened a pull request for " + version + ": " + landed.PullRequest.URL + " · production goes live once it is merged and a pod checks in"
	}
	if err := say(line); err != nil {
		return err
	}
	return p.awaitCheckIn(ctx, in, rel.ID, version, landed.PullRequest)
}

// releaseVersion is the release's version, as Naming a version names it (contracts/routes.md).
func releaseVersion(base deployRelease) string {
	if base.Package.Label != nil {
		return *base.Package.Label
	}
	return strings.TrimPrefix(base.Package.ID, "pkg_")
}

// repoRelease is the deploy body as what Canyon commits: the same release, one producer for both ways.
func repoRelease(base deployRelease, public string) gitops.Release {
	rel := gitops.Release{Canyon: public, ID: base.Release, App: base.App, Env: base.Environment, GateRun: base.GateRun, Package: base.Package.ID,
		Version: releaseVersion(base), Image: base.Image, Identity: base.Sidecar.Image, AppEnv: base.AppEnv, SidecarEnv: base.Sidecar.Env, Variables: base.Variables}
	for _, r := range base.Resources {
		rel.Resources = append(rel.Resources, gitops.Resource{Name: r.Name, Type: r.Type, Gives: r.Gives})
	}
	if base.Platform != nil { // a body journaled before it named one pins nothing (#2528)
		rel.Arch = base.Platform.Architecture
	}
	return rel
}

// awaitCheckIn waits on a repo release's first check-in (design/HAND-OFF.md §3b, Deployed) as awaitReport waits on
// a report: deployed is live, a platform's failed or removed report ends the run as it does there, a refusal
// the check-in wrote its own line for ends it too; every five minutes a line, at ten the one that says where
// to look, and at reportCeiling the run ends with the release still open, the pull request's link when
// production waits on a merge.
func (p *Pipeline) awaitCheckIn(ctx dbos.Context, in Input, id, version string, pr *gitops.PullRequest) error {
	note := stillWaitingEvery
	for waited := time.Duration(0); ; {
		r, err := dbos.RunAsStep(ctx, func(c context.Context) (reported, error) { return p.readReport(c, id) }, dbos.WithStepName("report"))
		if err != nil {
			return err
		}
		switch r.State {
		case "deployed":
			return p.appendBuildLog(ctx, in, "live at "+r.URL+" · a pod checked in")
		case "failed":
			if err := p.appendBuildLog(ctx, in, "Your platform reported: "+r.Reason); err != nil {
				return err
			}
			return errReleaseFailed
		case "removed":
			if err := p.appendBuildLog(ctx, in, "Your platform removed the release before it went live"); err != nil {
				return err
			}
			return errReleaseRemoved
		case "refused": // the check-in found the branch or the pull request not Canyon's, and said so
			return errReleaseRefused
		}
		if r.Stopped {
			return errBraked
		}
		if waited >= reportCeiling {
			break
		}
		if _, err := dbos.Sleep(ctx, reportCheckEvery); err != nil {
			return err
		}
		waited += reportCheckEvery
		if waited >= note && waited < reportCeiling {
			line := fmt.Sprintf("still waiting for a pod to check in (%s)", shortDuration(note))
			if note == 2*stillWaitingEvery { // ten minutes in the contract's words, whatever a test shrinks the wait to
				line = "Canyon committed " + version + " 10 minutes ago and no pod has checked in: look at your Argo CD or Flux sync"
			}
			if err := p.appendBuildLog(ctx, in, line); err != nil {
				return err
			}
			note += stillWaitingEvery
		}
	}
	msg := "No pod has checked in on " + version + " in 30 minutes. The release stays open: when a pod checks in, the environment goes live."
	if pr != nil {
		open, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			repo, err := p.DeployRepo(c, p.Pool)
			if err != nil || repo.Pulls == nil {
				return true, nil //nolint:nilerr // the merge cannot be read: the line names the pull request, which is what the person can act on
			}
			got, err := repo.Pulls.Get(c, pr.Number) // its state as the API says it: open, or neither merged nor closed yet
			return err != nil || got.State == "open" && !got.Merged, nil
		}, dbos.WithStepName("merged"))
		if err != nil {
			return err
		}
		if open {
			msg = "The pull request for " + version + " is open: " + pr.URL + ". Production goes live once it is merged and a pod checks in."
		}
	}
	if err := p.appendBuildLog(ctx, in, msg); err != nil {
		return err
	}
	return errReleaseOpen
}

// mergeWait is how long a remove's pull request is asked after, once a minute: after it the environment reads
// removing · waiting on your merge, and an undeploy or a delete asked again asks again (RecheckRemove).
var (
	mergeWait  = 30 * time.Minute
	mergeEvery = time.Minute
)

// removeByCommit is the remove workflow under the release's via: repo — the directory committed away, as the
// remove's own row says (deletes_resources: all of it, else everything but canyon.json and resources/). A push
// is removed at once; a pull request once merged, refused when closed, and left accepted after mergeWait.
func (p *Pipeline) removeByCommit(ctx dbos.Context, in RemoveInput) (string, error) {
	row, err := dbos.RunAsStep(ctx, func(c context.Context) (removing, error) { return p.removing(c, in.Release) }, dbos.WithStepName("remove"))
	if err != nil {
		return "", err
	}
	d := gitops.Delivery{Release: gitops.Release{Canyon: p.PublicURL, ID: in.Release, App: in.Project, Env: in.Env}, Remove: true, Delete: row.Deletes, Date: row.Created}
	landed, err := p.commitLoop(ctx, in.Release, "the remove", d, nil, nil)
	switch {
	case errors.Is(err, errReleaseRefused):
		return "refused", nil
	case errors.Is(err, errReleaseUndelivered):
		return "undelivered", nil
	case errors.Is(err, errBraked): // a newer release of the environment came first: its directory is never removed under it
		return "superseded", nil
	case err != nil:
		return "", err
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.recordCommit(c, in.Release, landed) }, dbos.WithStepName("committed")); err != nil {
		return "", err
	}
	if landed.PullRequest == nil {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.removedByCommit(c, in.Release) }, dbos.WithStepName("removed"))
		return "removed", err
	}
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.markRelease(c, in.Release, "accepted") }, dbos.WithStepName("accepted")); err != nil {
		return "", err
	}
	for waited := time.Duration(0); waited < mergeWait; waited += mergeEvery {
		state, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) { return p.RecheckRemove(c, in.Release) }, dbos.WithStepName("merge"))
		if err != nil {
			return "", err
		}
		if state != "accepted" {
			return state, nil
		}
		if _, err := dbos.Sleep(ctx, mergeEvery); err != nil {
			return "", err
		}
	}
	return "accepted", nil
}

// removing is what a remove's commit reads of its own row.
type removing struct {
	Deletes bool      `json:"deletes"`
	Created time.Time `json:"created"`
}

func (p *Pipeline) removing(ctx context.Context, id string) (removing, error) {
	var r removing
	err := p.Pool.QueryRow(ctx, "SELECT deletes_resources, created_at FROM releases WHERE id = $1", id).Scan(&r.Deletes, &r.Created)
	r.Created = r.Created.UTC().Truncate(time.Second)
	return r, err
}

// RecheckRemove asks a repo remove's pull request once: merged unedited, the remove is removed; closed or
// edited, refused; open, it stays accepted. The workflow asks once a minute for mergeWait, and an undeploy or a
// delete asked again after that asks here. An API error is no answer: still accepted.
func (p *Pipeline) RecheckRemove(ctx context.Context, id string) (string, error) {
	var state string
	var number *int
	var commit *string
	if err := p.Pool.QueryRow(ctx, "SELECT state, pull_number, commit_sha FROM releases WHERE id = $1 AND via = 'repo' AND event = 'remove'", id).Scan(&state, &number, &commit); err != nil {
		return "", err
	}
	if state != "accepted" || number == nil || commit == nil {
		return state, nil
	}
	repo, err := p.DeployRepo(ctx, p.Pool)
	if err != nil {
		return "", err
	}
	if repo.Pulls == nil {
		return "accepted", nil
	}
	merged, err := repo.MergedAs(ctx, *number, *commit)
	switch {
	case err != nil || merged == "open":
		return "accepted", nil //nolint:nilerr // no answer from the API: asked again later
	case merged == "merged":
		return "removed", p.removedByCommit(ctx, id)
	}
	return "refused", p.markReleaseFrom(ctx, id, "accepted", "refused")
}

// markReleaseFrom moves the release from one state to another, and from no other.
func (p *Pipeline) markReleaseFrom(ctx context.Context, id, from, to string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE releases SET state = $3 WHERE id = $1 AND state = $2", id, from, to)
		return err
	})
}

// removedByCommit is a remove's commit on its branch as a platform's removed report is (cmd/canyon-server, settle):
// the release removed and, unless a newer release of the environment or a deploy Canyon ran there came after it,
// the environment the org ran taken off and, for a project delete's, what the platform provided there with it.
func (p *Pipeline) removedByCommit(ctx context.Context, id string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		var project, env, run, commit string
		var deletes, superseded bool
		if err := tx.QueryRow(ctx, `UPDATE releases r SET state = 'removed' WHERE r.id = $1 AND r.state IN ('sent', 'accepted')
			RETURNING r.project_id, r.env, r.run_id, coalesce(r.commit_sha, ''), r.deletes_resources,
				EXISTS (SELECT 1 FROM releases n WHERE n.project_id = r.project_id AND n.env = r.env AND n.created_at > r.created_at)
				OR r.canyon_since IS NOT NULL`, id).
			Scan(&project, &env, &run, &commit, &deletes, &superseded); errors.Is(err, pgx.ErrNoRows) {
			return nil // removed already, or reported on meanwhile
		} else if err != nil {
			return err
		}
		if !superseded {
			if _, err := tx.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1 AND env = $2 AND run_by = 'org'", project, env); err != nil {
				return err
			}
			if deletes {
				if _, err := tx.Exec(ctx, "DELETE FROM platform_resources WHERE project_id = $1 AND env = $2", project, env); err != nil {
					return err
				}
			}
		}
		return events.AuditTx(ctx, tx, commitActor, "release removed by commit", project, run, map[string]any{"release": id, "env": env, "commit": commit})
	})
}
