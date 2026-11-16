package gitops

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/internal/gitpush"
)

// Repo is the company's deploy repo as the platform-repo row saves it, and the bare repository of Canyon's own
// that git works in.
type Repo struct {
	URL, User, Token string
	Preview, Prod    string // the branches
	ProdPR           bool   // production through a pull request; else pushed
	Root             string
	Store, CA        string       // the ClusterSecretStore Canyon's ExternalSecret names, and Canyon's CA ("" for none): every release's
	Pulls            PullRequests // nil under api: none
	Dir              string       // the bare repository, under CANYON_REPOS
	Canyon           string       // Canyon's public URL: canyon.json's, and the author's address
	HTTP             *http.Client
}

// Branch is the branch an environment's releases are written to.
func (r *Repo) Branch(env string) string {
	if env == "prod" {
		return r.Prod
	}
	return r.Preview
}

// PR is whether an environment's releases go through a pull request.
func (r *Repo) PR(env string) bool { return env == "prod" && r.ProdPR }

// envDir is the release's directory in the repo.
func (r *Repo) envDir(app, env string) string { return r.Root + "/" + app + "/" + env }

func (r *Repo) git(date time.Time) gitpush.Git {
	host := "canyon"
	if u, err := url.Parse(r.Canyon); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return gitpush.Git{Dir: r.Dir, User: r.User, Token: r.Token, Name: "Canyon", Email: "canyon@" + host, Date: date}
}

// Ready makes the bare repository when it is not there yet: Canyon's own working copy of nothing.
func (r *Repo) Ready(ctx context.Context) error {
	if _, err := os.Stat(path.Join(r.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	_, err := r.git(time.Time{}).Run(ctx, "init", "--bare", "-q", r.Dir)
	return err
}

// errNotFastForward is a push the branch's head moved under: fetched and made again on the new head.
var errNotFastForward = errors.New("gitops: the branch moved")

// probe asks the repo for its refs over Go's own https, under the token, redirects refused: no connection,
// a certificate Canyon does not trust, a refused token — each the problem it is, never the remote's words.
// service git-receive-pack asks whether the token may push.
func (r *Repo) probe(ctx context.Context, service string) *Problem {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(r.URL, "/")+"/info/refs?service="+service, nil)
	if err != nil {
		return unreachable()
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(r.User+":"+r.Token)))
	resp, err := r.client().Do(req)
	if err != nil {
		var unknown x509.UnknownAuthorityError
		var invalid x509.CertificateInvalidError
		var host x509.HostnameError
		var verify *tls.CertificateVerificationError
		if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) || errors.As(err, &verify) {
			return tlsUntrusted()
		}
		return unreachable()
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		if service == "git-receive-pack" {
			return noWrite()
		}
		return authRefused()
	}
	return unreachable()
}

func (r *Repo) client() *http.Client {
	c := r.HTTP
	if c == nil {
		c = &http.Client{}
	}
	once := *c
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &once
}

// head is the branch's head, fetched into the bare repository: "" and branch_missing when there is none.
func (r *Repo) head(ctx context.Context, branch string) (string, error) {
	g := r.git(time.Time{})
	out, err := g.Run(ctx, "ls-remote", r.URL, "refs/heads/"+branch)
	if err != nil {
		return "", r.transport(ctx)
	}
	sha, _, _ := strings.Cut(out, "\t")
	if sha == "" {
		return "", branchMissing(branch)
	}
	if _, err := g.Run(ctx, "fetch", "--no-tags", "-q", r.URL, "+refs/heads/"+branch+":refs/canyon/"+branch); err != nil {
		return "", r.transport(ctx)
	}
	return sha, nil
}

// transport is why git could not talk to the repo: Go's own probe of the same URL says which problem.
func (r *Repo) transport(ctx context.Context) error {
	if p := r.probe(ctx, "git-upload-pack"); p != nil {
		return p
	}
	return unreachable()
}

// TreeAt is the tree at path in commit, "" when there is none.
func (r *Repo) TreeAt(ctx context.Context, commit, at string) (string, error) {
	out, err := r.git(time.Time{}).Run(ctx, "ls-tree", "-d", commit, "--", at)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	fields := strings.Fields(strings.SplitN(out, "\t", 2)[0])
	if len(fields) != 3 || fields[1] != "tree" {
		return "", fmt.Errorf("gitops: %s in %s is not a tree", at, commit)
	}
	return fields[2], nil
}

// BranchTree fetches branch and answers the tree at the release's directory on its head: what a check-in
// compares with the tree Canyon rendered (design/HAND-OFF.md §3b, Deployed).
func (r *Repo) BranchTree(ctx context.Context, app, env string) (string, error) {
	head, err := r.head(ctx, r.Branch(env))
	if err != nil {
		return "", err
	}
	return r.TreeAt(ctx, head, r.envDir(app, env))
}

// entry is one line of a tree: its mode, type, object and name.
type entry struct{ mode, kind, object, name string }

// entries is a tree's lines, "" for no tree.
func (r *Repo) entries(ctx context.Context, tree string) ([]entry, error) {
	if tree == "" {
		return nil, nil
	}
	out, err := r.git(time.Time{}).Run(ctx, "ls-tree", "-z", tree)
	if err != nil {
		return nil, err
	}
	var es []entry
	for line := range strings.SplitSeq(out, "\x00") {
		if line == "" {
			continue
		}
		meta, name, ok := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("gitops: a tree line git did not write: %q", line)
		}
		es = append(es, entry{f[0], f[1], f[2], name})
	}
	return es, nil
}

// mktree writes a tree of es, sorted as git sorts them.
func (r *Repo) mktree(ctx context.Context, es []entry) (string, error) {
	var in bytes.Buffer
	for _, e := range es {
		fmt.Fprintf(&in, "%s %s %s\t%s\x00", e.mode, e.kind, e.object, e.name)
	}
	out, err := r.git(time.Time{}).Output(ctx, in.Bytes(), "mktree", "-z")
	return strings.TrimSpace(out), err
}

// subtree is files as a tree: each a blob, directories made of their paths.
func (r *Repo) subtree(ctx context.Context, files map[string][]byte) (string, error) {
	g := r.git(time.Time{})
	var es []entry
	dirs := map[string]map[string][]byte{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if dir, rest, nested := strings.Cut(name, "/"); nested {
			if dirs[dir] == nil {
				dirs[dir] = map[string][]byte{}
			}
			dirs[dir][rest] = files[name]
			continue
		}
		blob, err := g.Output(ctx, files[name], "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		es = append(es, entry{"100644", "blob", strings.TrimSpace(blob), name})
	}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		tree, err := r.subtree(ctx, dirs[dir])
		if err != nil {
			return "", err
		}
		es = append(es, entry{"040000", "tree", tree, dir})
	}
	return r.mktree(ctx, es)
}

// graft is base with the tree at the path's segments replaced by sub, "" removing it, every tree on the way
// made again and nothing else changed.
func (r *Repo) graft(ctx context.Context, base string, segments []string, sub string) (string, error) {
	es, err := r.entries(ctx, base)
	if err != nil {
		return "", err
	}
	name := segments[0]
	at := slices.IndexFunc(es, func(e entry) bool { return e.name == name })
	child := sub
	if len(segments) > 1 {
		below := ""
		if at >= 0 && es[at].kind == "tree" {
			below = es[at].object
		}
		if child, err = r.graft(ctx, below, segments[1:], sub); err != nil {
			return "", err
		}
	}
	if at >= 0 {
		es = slices.Delete(es, at, at+1)
	}
	if child != "" {
		es = append(es, entry{"040000", "tree", child, name})
	}
	if len(es) == 0 {
		return "", nil
	}
	return r.mktree(ctx, es)
}

// Change is what one commit does to a release's directory.
type Change struct {
	App, Env string
	Files    map[string][]byte // a deploy's: the directory becomes exactly these
	Keep     bool              // a remove's: everything but canyon.json and resources/ goes
	Delete   bool              // a project delete's: the directory goes
	Message  string
	Date     time.Time // the release's: the same commit every time it is made
}

// commit makes c's commit on parent: the tree at the directory and the commit, "" for both when the directory
// already is what c makes. Every path it changes is under the directory, or it is refused.
func (r *Repo) commit(ctx context.Context, parent string, c Change) (commit, tree string, err error) {
	dir := r.envDir(c.App, c.Env)
	root, err := r.git(time.Time{}).Run(ctx, "rev-parse", parent+"^{tree}")
	if err != nil {
		return "", "", err
	}
	was, err := r.TreeAt(ctx, parent, dir)
	if err != nil {
		return "", "", err
	}
	var sub string
	switch {
	case c.Delete:
	case c.Keep:
		es, err := r.entries(ctx, was)
		if err != nil {
			return "", "", err
		}
		es = slices.DeleteFunc(es, func(e entry) bool { return e.name != "canyon.json" && e.name != "resources" })
		if len(es) > 0 {
			if sub, err = r.mktree(ctx, es); err != nil {
				return "", "", err
			}
		}
	default:
		if sub, err = r.subtree(ctx, c.Files); err != nil {
			return "", "", err
		}
	}
	if sub == was {
		return "", sub, nil
	}
	next, err := r.graft(ctx, root, strings.Split(dir, "/"), sub)
	if err != nil {
		return "", "", err
	}
	if commit, err = r.git(c.Date).Run(ctx, "commit-tree", next, "-p", parent, "-m", c.Message); err != nil {
		return "", "", err
	}
	if err := r.onlyUnder(ctx, commit, dir); err != nil {
		return "", "", err
	}
	return commit, sub, nil
}

// onlyUnder is nil when commit has one parent and changes nothing outside dir: what Canyon pushes, and what it
// proposes from a branch an earlier attempt pushed.
func (r *Repo) onlyUnder(ctx context.Context, commit, dir string) error {
	g := r.git(time.Time{})
	parents, err := g.Run(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	if f := strings.Fields(parents); len(f) != 2 {
		return fmt.Errorf("gitops: %s is not one commit on one parent: nothing is pushed", commit)
	}
	changed, err := g.Run(ctx, "diff-tree", "-r", "--name-only", "--no-commit-id", "-z", commit+"^", commit)
	if err != nil {
		return err
	}
	for name := range strings.SplitSeq(changed, "\x00") {
		if name != "" && !strings.HasPrefix(name, dir+"/") { // never anything outside the release's own directory
			return fmt.Errorf("gitops: a commit of %s would change %s: nothing is pushed", dir, name)
		}
	}
	return nil
}

// push sends commit to branch, never forced: git refuses anything but a fast-forward, and the report
// --porcelain writes says which refusal it was.
func (r *Repo) push(ctx context.Context, commit, branch string) error {
	return r.send(ctx, commit+":refs/heads/"+branch, branch)
}

// send pushes refspec, the remote's refusal a problem in D5's words and never git's error, which carries what the
// remote said.
func (r *Repo) send(ctx context.Context, refspec, branch string) error {
	out, err := r.git(time.Time{}).Output(ctx, nil, "push", "--porcelain", r.URL, refspec)
	if err == nil {
		return nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[0] != "!" {
			continue
		}
		if strings.HasPrefix(fields[2], "[rejected]") { // non-fast-forward, fetch first: the branch moved
			return errNotFastForward
		}
		return protected(branch) // [remote rejected]: a rule or a hook
	}
	if p := r.probe(ctx, "git-receive-pack"); p != nil {
		return p
	}
	return unreachable()
}

// remove deletes branch on the remote: a check's throwaway, never a branch of a release.
func (r *Repo) remove(ctx context.Context, branch string) error {
	return r.send(ctx, ":refs/heads/"+branch, branch)
}

// templates is <root>/.canyon at commit: each file under it, by its path below it, and the tree's id for the
// commit's Canyon-Templates trailer ("none" without one).
func (r *Repo) templates(ctx context.Context, commit string) (Templates, string, error) {
	tree, err := r.TreeAt(ctx, commit, r.Root+"/.canyon")
	if err != nil || tree == "" {
		return Templates{}, "none", err
	}
	g := r.git(time.Time{})
	out, err := g.Run(ctx, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, "", err
	}
	tmpl := Templates{}
	for line := range strings.SplitSeq(out, "\x00") {
		meta, name, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); !ok || len(f) != 3 || f[1] != "blob" {
			continue
		}
		blob := strings.Fields(meta)[2]
		size, err := g.Run(ctx, "cat-file", "-s", blob)
		if err != nil {
			return nil, "", err
		}
		if n, err := strconv.Atoi(size); err != nil || n > templateMax { // render refuses it: it is never read whole
			tmpl[name] = make([]byte, templateMax+1)
			continue
		}
		body, err := g.Output(ctx, nil, "cat-file", "blob", blob)
		if err != nil {
			return nil, "", err
		}
		tmpl[name] = []byte(body)
	}
	return tmpl, tree, nil
}

// marker is the directory's canyon.json on commit, nil when there is none.
func (r *Repo) marker(ctx context.Context, commit, app, env string) (*Canyon, error) {
	out, err := r.git(time.Time{}).Run(ctx, "ls-tree", commit, "--", r.envDir(app, env)+"/canyon.json")
	if err != nil || out == "" {
		return nil, err
	}
	body, err := r.git(time.Time{}).Run(ctx, "cat-file", "blob", commit+":"+r.envDir(app, env)+"/canyon.json")
	if err != nil {
		return nil, err
	}
	var c Canyon
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return &Canyon{}, nil //nolint:nilerr // a canyon.json Canyon cannot read names no Canyon: another's, never overwritten
	}
	return &c, nil
}
