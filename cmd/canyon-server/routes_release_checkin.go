package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gitops"
	"github.com/jackc/pgx/v5"
)

// checkInActor is who puts a delivery: repo release live: Canyon, at a pod's check-in (contracts/routes.md, the
// access row), never the platform and never a member.
var checkInActor = actor{Kind: "canyon", Subject: "check-in", Display: "Canyon"}

// checkInWait bounds one settle by a check-in: a fetch of the branch, one read of the pull request, the write.
var checkInWait = time.Minute

// settling is one release's settle by check-in at a time, and when the next may run after one that could not
// read the branch or the pull request: a minute, doubling to fifteen (#2471). Per server: every
// replica's check-ins of one release are its pods', and a second one finds the release settled or still waiting.
type settling struct {
	mu    sync.Mutex
	busy  map[string]bool
	after map[string]time.Time
	wait  map[string]time.Duration
}

func (s *settling) begin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy, s.after, s.wait = map[string]bool{}, map[string]time.Time{}, map[string]time.Duration{}
	}
	if s.busy[id] || time.Now().Before(s.after[id]) {
		return false
	}
	s.busy[id] = true
	return true
}

// end lets the release be settled again: at the next check-in, or after its backoff when this one failed.
func (s *settling) end(id string, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, id)
	if !failed {
		delete(s.after, id)
		delete(s.wait, id)
		return
	}
	s.wait[id] = min(max(2*s.wait[id], time.Minute), 15*time.Minute)
	s.after[id] = time.Now().Add(s.wait[id])
}

// repoRelease is what a check-in reads of a delivery: repo release: where it landed and what Canyon rendered.
type repoRelease struct {
	State, Env, Project, Run, Package, Commit, Branch, Tree, Address string
	PullNumber                                                       *int
	PullRequest                                                      *string
	Endpoints                                                        []reportedResource
}

func readRepoRelease(ctx context.Context, q querier, id string) (repoRelease, error) {
	var r repoRelease
	var commit, branch, tree, address *string
	var endpoints []byte
	err := q.QueryRow(ctx, `SELECT state, env, project_id, run_id, package_id, commit_sha, branch, tree, address, pull_number, pull_request, endpoints
		FROM releases WHERE id = $1 AND via = 'repo' AND event = 'deploy'`, id).
		Scan(&r.State, &r.Env, &r.Project, &r.Run, &r.Package, &commit, &branch, &tree, &address, &r.PullNumber, &r.PullRequest, &endpoints)
	if err != nil {
		return r, err
	}
	if commit == nil || branch == nil || tree == nil || address == nil { // committed releases have all four: one that has not, nothing proves
		return r, fmt.Errorf("release %s is accepted with no commit recorded", id)
	}
	r.Commit, r.Branch, r.Tree, r.Address = *commit, *branch, *tree, *address
	if len(endpoints) > 0 {
		if err := json.Unmarshal(endpoints, &r.Endpoints); err != nil {
			return r, err
		}
	}
	if r.Endpoints == nil {
		r.Endpoints = []reportedResource{}
	}
	return r, nil
}

// deployByCheckIn settles a delivery: repo release at a check-in whose app answers (design/HAND-OFF.md §3b,
// Deployed). First, before any git or API read: a release a newer one of its environment followed — a deploy or a
// remove — or a production release while production is held goes straight to the report's decision path, which
// answers it superseded or held, with their removes: an older release's late check-in is never a tamper alarm.
// Else its pull request merged with Canyon's commit when it opened one — open is not yet, closed or edited refuses
// it — and the branch's tree at its directory the one Canyon rendered, else refused; then the decision path, as a
// deployed report of the release's digest and what Canyon rendered. An error reading the branch or the pull
// request settles nothing and backs off: it is never live.
func (a *app) deployByCheckIn(ctx context.Context, id string) {
	if !a.settling.begin(id) {
		return
	}
	failed := true
	defer func() { a.settling.end(id, failed) }()
	ctx, cancel := context.WithTimeout(ctx, checkInWait)
	defer cancel()
	rel, err := readRepoRelease(ctx, a.pool, id)
	if err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: %v", id, err)
		return
	}
	if rel.State != "accepted" {
		failed = false
		return
	}
	var stale bool
	if err := a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM releases n, releases r WHERE r.id = $1 AND n.project_id = r.project_id AND n.env = r.env AND n.created_at > r.created_at)
		OR ($3 = 'prod' AND EXISTS (SELECT 1 FROM prod_holds WHERE project_id = $2))`, id, rel.Project, rel.Env).Scan(&stale); err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: %v", id, err)
		return
	}
	if !stale && !a.agreesWithGit(ctx, id, rel, &failed) {
		return
	}
	a.settleByCheckIn(ctx, id, rel, &failed)
}

// agreesWithGit is whether the deploy repo holds what Canyon committed for rel: its pull request merged with its
// commit when it opened one, and the branch's tree at its directory the one rendered. false settles nothing more:
// open is not yet, closed, edited or another tree refuses it, and an error leaves *failed set, which backs off.
func (a *app) agreesWithGit(ctx context.Context, id string, rel repoRelease, failed *bool) bool {
	repo, err := a.pipe.DeployRepo(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: the deploy repo: %v", id, err)
		return false
	}
	version, err := a.versionOf(ctx, rel)
	if err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: %v", id, err)
		return false
	}
	if rel.PullNumber != nil {
		if repo.Pulls == nil {
			a.log.Printf("canyon-server: settle release %s by check-in: it opened a pull request, and the deploy repo has no pulls API now", id)
			return false
		}
		merged, err := repo.MergedAs(ctx, *rel.PullNumber, rel.Commit)
		if err != nil {
			a.log.Printf("canyon-server: settle release %s by check-in: its pull request could not be read", id) // never the API's words
			return false
		}
		switch merged {
		case "open": // a pod of it runs, but production's branch does not hold it yet: not yet live, never refused
			*failed = false
			return false
		case "closed":
			*failed = !a.refuseCheckIn(ctx, id, rel, "pull request closed", "The pull request for "+version+" was closed without merging: nothing went live", map[string]any{"pull_request": *rel.PullRequest})
			return false
		case "changed":
			*failed = !a.refuseCheckIn(ctx, id, rel, "pull request changed", "The pull request for "+version+" was changed after Canyon opened it: nothing went live", map[string]any{"pull_request": *rel.PullRequest})
			return false
		}
	}
	found, err := repo.BranchTree(ctx, rel.Project, rel.Env)
	if err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: the branch could not be read: %v", id, problemCode(err))
		return false
	}
	if found != rel.Tree {
		path := repo.Root + "/" + rel.Project + "/" + rel.Env
		*failed = !a.refuseCheckIn(ctx, id, rel, "release tree mismatch", path+" on "+rel.Branch+" isn't what Canyon committed for "+version+": nothing went live",
			map[string]any{"branch": rel.Branch, "expected": rel.Tree, "found": found})
		return false
	}
	return true
}

// settleByCheckIn is the report's decision path for a release a check-in settles: live, or refused as a report
// would be — superseded, held, its project deleting — with their removes, answered to nobody.
func (a *app) settleByCheckIn(ctx context.Context, id string, rel repoRelease, failed *bool) {
	if !validAddress(rel.Address) {
		a.log.Printf("canyon-server: settle release %s by check-in: the address it was committed with is not one an app takes", id)
		return
	}
	var out outcome
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		r, err := readRelease(ctx, tx, id)
		if err != nil {
			return err
		}
		if !endpointsFit(rel.Endpoints, r.Declared) { // what Canyon rendered, checked as a report's are: nothing outside what the release declares reaches the record (#2471)
			return fmt.Errorf("the endpoints it was committed with are not the release's resources")
		}
		_, digest, _ := strings.Cut(r.Image, "@")
		out, err = a.settle(ctx, tx, r, releaseReport{Status: "deployed", Address: rel.Address, Digest: digest, Resources: rel.Endpoints, Proof: "check-in", Commit: rel.Commit},
			checkInActor, events.Provenance{Via: "canyon"})
		if err != nil || out.status == http.StatusOK {
			return err
		}
		// Refused as a report would be — superseded, held, its project deleting — the release never goes live, so
		// no later check-in of it reads the branch again.
		_, err = tx.Exec(ctx, "UPDATE releases SET state = 'refused' WHERE id = $1 AND state = 'accepted'", id)
		return err
	})
	if err != nil {
		a.log.Printf("canyon-server: settle release %s by check-in: %v", id, err)
		return
	}
	if out.status != http.StatusOK {
		a.log.Printf("canyon-server: release %s checked in and was not put live: %v", id, out.body["error"])
	}
	*failed = false
}

// refuseCheckIn marks a repo release refused at its check-in, with its one build.log line and its audit row: the
// branch or the pull request is not what Canyon committed. true once written.
func (a *app) refuseCheckIn(ctx context.Context, id string, rel repoRelease, action, line string, detail map[string]any) bool {
	detail["release"], detail["env"] = id, rel.Env
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE releases SET state = 'refused' WHERE id = $1 AND state = 'accepted'", id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if _, err := events.AppendRunTx(ctx, tx, rel.Run, rel.Project, events.BuildLog, "check-in refused:"+id, map[string]any{"line": line}); err != nil && !errors.Is(err, events.ErrEnded) {
			return err
		}
		return events.AuditTx(events.WithProvenance(ctx, events.Provenance{Via: "canyon"}), tx, checkInActor, action, rel.Project, rel.Run, detail)
	})
	if err != nil {
		a.log.Printf("canyon-server: refuse release %s at its check-in: %v", id, err)
		return false
	}
	return true
}

// versionOf is the release's version, as Naming a version names it (contracts/routes.md).
func (a *app) versionOf(ctx context.Context, rel repoRelease) (string, error) {
	var version string
	err := a.pool.QueryRow(ctx, "SELECT coalesce(label, substr(id, 5)) FROM packages WHERE id = $1 AND project_id = $2", rel.Package, rel.Project).Scan(&version)
	return version, err
}

// problemCode is err's problem code, or "error": what a log line may say of a deploy repo's failure.
func problemCode(err error) string {
	if p, ok := gitops.IsProblem(err); ok {
		return p.Code
	}
	return "error"
}

// recheckRepoRemoves asks once more of each delivery: repo remove of project waiting on its pull request past
// the workflow's 30 minutes (design/HAND-OFF.md §3b, Remove): merged, it is removed now. env "" is every
// environment. Best effort, bounded: an error leaves the remove waiting, as it was.
func (a *app) recheckRepoRemoves(ctx context.Context, project, env string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := a.pool.Query(ctx, `SELECT id FROM releases WHERE project_id = $1 AND ($2 = '' OR env = $2) AND via = 'repo' AND event = 'remove'
		AND state = 'accepted' AND pull_number IS NOT NULL`, project, env)
	if err != nil {
		a.log.Printf("canyon-server: the removes of %s waiting on a merge: %v", project, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		a.log.Printf("canyon-server: the removes of %s waiting on a merge: %v", project, err)
		return
	}
	for _, id := range ids {
		if _, err := a.pipe.RecheckRemove(ctx, id); err != nil {
			a.log.Printf("canyon-server: ask after remove %s's pull request: %v", id, err)
		}
	}
}
