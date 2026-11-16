package gitops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

// Delivery is one release Canyon commits: a deploy's files, or a remove of its directory.
type Delivery struct {
	Release Release // a deploy's whole; a remove's ID, App and Env
	Remove  bool    // an undeploy's, a hold's or the brake's: canyon.json and resources/ stay, so the data does
	Delete  bool    // a project delete's: the directory goes
	Review  string  // the review's address, in the commit's message
	Date    time.Time
	Fresh   func(context.Context) error // read right before each push, under the caller's lock: an error stops it
}

// Landed is where a delivery is: its commit, on its branch or a pull request's, and the tree at its directory —
// "" for a remove that took the directory away — with what the templates said.
type Landed struct {
	Commit, Branch, Tree string
	PullRequest          *PullRequest
	Address              string
	Endpoints            []Endpoint
}

// errNoPullsAPI is production through a pull request with no API to open or read it through.
var errNoPullsAPI = errors.New("gitops: production goes through a pull request, and the deploy repo has no pull requests API")

// attempts is how often a push the branch moved under is made again on the new head, within one delivery.
const attempts = 5

// Deliver commits d: rendered in memory, then one deterministic commit on the branch's head, pushed — or for
// production through a pull request, on a branch of its own — never forced, and nothing when the directory is
// already what d makes. Every failure is a Problem, but a Fresh refusal, answered as it is.
func (r *Repo) Deliver(ctx context.Context, d Delivery) (Landed, error) {
	if r.PR(d.Release.Env) && r.Pulls == nil { // before any git write: never a push with no API to propose it through
		return Landed{}, errNoPullsAPI
	}
	if err := r.Ready(ctx); err != nil {
		return Landed{}, err
	}
	if p := r.probe(ctx, "git-upload-pack"); p != nil {
		return Landed{}, p
	}
	rel, branch := d.Release, r.Branch(d.Release.Env)
	rel.Store, rel.CA = r.Store, r.CA
	for range attempts {
		head, err := r.head(ctx, branch)
		if err != nil {
			return Landed{}, err
		}
		out := Landed{Branch: branch}
		change := Change{App: rel.App, Env: rel.Env, Date: d.Date, Keep: d.Remove && !d.Delete, Delete: d.Delete}
		mark, err := r.marker(ctx, head, rel.App, rel.Env) // a deploy's and a remove's alike: another Canyon's directory is never Canyon's to change
		if err != nil {
			return Landed{}, err
		}
		if mark != nil && mark.Canyon != rel.Canyon {
			return Landed{}, directoryTaken(r.envDir(rel.App, rel.Env), mark.Canyon)
		}
		if mark == nil { // a directory with no canyon.json is someone else's: never written over, nor removed
			if tree, err := r.TreeAt(ctx, head, r.envDir(rel.App, rel.Env)); err != nil {
				return Landed{}, err
			} else if tree != "" {
				return Landed{}, directoryForeign(r.envDir(rel.App, rel.Env))
			}
		}
		switch {
		case d.Delete:
			change.Message = "delete · " + rel.App + " · " + rel.Env + "\n\nCanyon-Release: " + rel.ID
		case d.Remove:
			change.Message = "remove · " + rel.App + " · " + rel.Env + "\n\nCanyon-Release: " + rel.ID
		default:
			tmpl, tree, err := r.templates(ctx, head)
			if err != nil {
				return Landed{}, err
			}
			rendered, err := Render(rel, tmpl, r.Root+"/.canyon")
			if err != nil {
				return Landed{}, err
			}
			change.Files, out.Address, out.Endpoints = rendered.Files, rendered.Address, rendered.Endpoints
			change.Message = rel.Version + " · " + rel.App + " · " + rel.Env + "\n\nCanyon reviewed it: " + d.Review + "\n\nCanyon-Release: " + rel.ID +
				"\nCanyon-Review: " + rel.GateRun + "\nCanyon-Version: " + rel.Package + "\nCanyon-Templates: " + tree
		}
		commit, tree, err := r.commit(ctx, head, change)
		if err != nil {
			return Landed{}, err
		}
		out.Tree = tree
		if commit == "" { // an earlier attempt landed, or the branch already is what this makes
			out.Commit = head
			return out, nil
		}
		if r.PR(rel.Env) {
			return r.propose(ctx, d, head, commit, out)
		}
		if err := d.Fresh(ctx); err != nil {
			return Landed{}, err
		}
		if err := r.push(ctx, commit, branch); errors.Is(err, errNotFastForward) {
			continue
		} else if err != nil {
			return Landed{}, err
		}
		out.Commit = commit
		return out, nil
	}
	return Landed{}, unreachable()
}

// prBranch is a release's pull request branch.
func prBranch(rel Release) string {
	return "canyon/" + rel.App + "/" + rel.Env + "/" + strings.TrimPrefix(rel.ID, "rel_")
}

// propose pushes commit to the release's own branch and opens its pull request into production's branch — the
// open one of that branch when there is one, looked for first, since GitLab documents no answer to a second — then
// closes the environment's older Canyon pull requests of this repo's own branches: one merged later would put an
// older release back, and a fork's is never Canyon's, nor its to close. A branch already there is reused only when its head is one commit,
// the release's tree at its directory and nothing else changed, on a parent in production's history (prod, its
// head): else someone could have put their own commit under Canyon's, and a merge would take it along.
func (r *Repo) propose(ctx context.Context, d Delivery, prod, commit string, out Landed) (Landed, error) {
	rel, branch := d.Release, prBranch(d.Release)
	if err := d.Fresh(ctx); err != nil { // before any write: a stale retry pushes nothing, opens nothing and closes nothing
		return Landed{}, err
	}
	g := r.git(time.Time{})
	existing, err := g.Run(ctx, "ls-remote", r.URL, "refs/heads/"+branch)
	if err != nil {
		return Landed{}, r.transport(ctx)
	}
	if sha, _, _ := strings.Cut(existing, "\t"); sha != "" { // an earlier attempt pushed it: kept when it is this release's tree
		if _, err := g.Run(ctx, "fetch", "--no-tags", "-q", r.URL, "+refs/heads/"+branch+":refs/canyon/"+branch); err != nil {
			return Landed{}, r.transport(ctx)
		}
		if tree, err := r.TreeAt(ctx, sha, r.envDir(rel.App, rel.Env)); err != nil {
			return Landed{}, err
		} else if tree != out.Tree {
			return Landed{}, protected(branch)
		}
		if err := r.onlyUnder(ctx, sha, r.envDir(rel.App, rel.Env)); err != nil { // one commit, changing the release's directory alone, or never proposed
			return Landed{}, protected(branch)
		}
		if _, err := g.Run(ctx, "merge-base", "--is-ancestor", sha+"^", prod); err != nil { // its parent in production's history, or never proposed
			return Landed{}, protected(branch)
		}
		commit = sha
	} else {
		if err := r.push(ctx, commit, branch); err != nil {
			if errors.Is(err, errNotFastForward) {
				return Landed{}, protected(branch)
			}
			return Landed{}, err
		}
	}
	title := rel.Version + " · " + rel.App + " · " + rel.Env
	if d.Remove || d.Delete {
		title = "remove · " + rel.App + " · " + rel.Env
	}
	open, err := r.Pulls.Open(ctx)
	if err != nil {
		return Landed{}, pullsAPI(r.Pulls.Endpoint())
	}
	var pr PullRequest
	if i := slices.IndexFunc(open, func(o PullRequest) bool { return canyons(r.Pulls, o, branch, r.Prod) }); i >= 0 {
		pr = open[i]
	} else if pr, err = r.Pulls.Create(ctx, title, branch, r.Prod, "Canyon reviewed this release: "+d.Review+"\n\nMerge it to put it in production; it goes live once a pod checks in."); errors.Is(err, errExists) {
		pr, err = r.find(ctx, branch)
	}
	if err != nil {
		return Landed{}, pullsAPI(r.Pulls.Endpoint())
	}
	for _, o := range open {
		if o.Number != pr.Number && r.Pulls.Own(o) && o.Base.Ref == r.Prod && strings.HasPrefix(o.Head.Ref, "canyon/"+rel.App+"/"+rel.Env+"/") {
			err = errors.Join(err, r.Pulls.Close(ctx, o.Number))
		}
	}
	if err != nil { // an older one still open: tried again, never left — a check-in refuses it anyway
		return Landed{}, pullsAPI(r.Pulls.Endpoint())
	}
	out.Commit, out.PullRequest = commit, &pr
	return out, nil
}

// find is the open pull request of branch into production's, from this repo: one a fork opened under the same
// branch name is never Canyon's, and none found is an error — the release is tried again, never a duplicate kept.
func (r *Repo) find(ctx context.Context, branch string) (PullRequest, error) {
	open, err := r.Pulls.Open(ctx)
	if err != nil {
		return PullRequest{}, err
	}
	if i := slices.IndexFunc(open, func(pr PullRequest) bool { return canyons(r.Pulls, pr, branch, r.Prod) }); i >= 0 {
		return open[i], nil
	}
	return PullRequest{}, errors.New("gitops: the API said the pull request exists, and lists none of this repo's branch")
}

// Check is what the dashboard's check of the repo proves (contracts/routes.md, the platform-repo row's
// POST …/check): both branches are there, the templates render a sample of every type they are found for,
// the token pushes and deletes a throwaway branch, and under a pull requests API it lists the open ones.
// It answers each problem once, in that order, and the templates it found, as .canyon/<path>.
func (r *Repo) Check(ctx context.Context, sample Release) ([]Problem, []string, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, nil, err
	}
	var problems []Problem
	say := func(err error) error {
		p, ok := IsProblem(err)
		if !ok {
			return err
		}
		if !slices.ContainsFunc(problems, func(q Problem) bool { return q.Code == p.Code }) {
			problems = append(problems, *p)
		}
		return nil
	}
	if p := r.probe(ctx, "git-upload-pack"); p != nil {
		return []Problem{*p}, []string{}, nil
	}
	heads := map[string]string{}
	for _, branch := range []string{r.Preview, r.Prod} {
		head, err := r.head(ctx, branch)
		if err := say(err); err != nil {
			return nil, nil, err
		}
		heads[branch] = head
	}
	found := []string{}
	for _, branch := range []string{r.Preview, r.Prod} {
		if heads[branch] == "" {
			continue
		}
		tmpl, _, err := r.templates(ctx, heads[branch])
		if err != nil {
			return nil, nil, err
		}
		rel := sample
		rel.Env = map[string]string{r.Preview: "preview", r.Prod: "prod"}[branch]
		rel.Resources = nil
		for name := range tmpl {
			if !slices.Contains(found, ".canyon/"+name) {
				found = append(found, ".canyon/"+name)
			}
			if typ, ok := strings.CutPrefix(name, "resources/"); ok && strings.HasSuffix(typ, ".yaml") {
				typ = strings.TrimSuffix(typ, ".yaml")
				rel.Resources = append(rel.Resources, Resource{Name: typ, Type: typ})
			}
		}
		if _, err := Render(rel, tmpl, r.Root+"/.canyon"); say(err) != nil {
			return nil, nil, err
		}
	}
	slices.Sort(found)
	if head := heads[r.Preview]; head != "" {
		var n [4]byte
		_, _ = rand.Read(n[:])
		branch := "canyon/check-" + hex.EncodeToString(n[:])
		if err := r.push(ctx, head, branch); err != nil {
			if errors.Is(err, errNotFastForward) {
				err = protected(branch)
			}
			if err := say(err); err != nil {
				return nil, nil, err
			}
		} else if err := say(r.remove(ctx, branch)); err != nil {
			return nil, nil, err
		}
	}
	if r.ProdPR && r.Pulls != nil {
		if _, err := r.Pulls.Open(ctx); err != nil {
			_ = say(pullsAPI(r.Pulls.Endpoint()))
		}
	}
	return problems, found, nil
}

// MergedAs is whether the release's pull request was merged with its head Canyon's commit into production's branch
// from this repo's own, merged otherwise — edited, retargeted or a fork's: changed, never merged — closed unmerged,
// or is open: what a check-in, and a remove, reads before it settles.
func (r *Repo) MergedAs(ctx context.Context, number int, commit string) (state string, err error) {
	if r.Pulls == nil {
		return "", errNoPullsAPI
	}
	pr, err := r.Pulls.Get(ctx, number)
	switch {
	case err != nil:
		return "", err
	case pr.Merged && (pr.Base.Ref != r.Prod || !r.Pulls.Own(pr)):
		return "changed", nil
	case pr.Merged && pr.Head.SHA == commit:
		return "merged", nil
	case pr.Merged:
		return "changed", nil
	case pr.State == "closed":
		return "closed", nil
	}
	return "open", nil
}
