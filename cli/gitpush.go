package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
)

// commitID is a commit GitHub delivered, as the 409's moved_from names it: forty hex digits, so it
// never reaches git as an option.
var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pushed is the 409's moved_from when the head came from a push (contracts/routes.md, stale_base; #1938),
// "" otherwise.
func (e *staleBase) pushed() string {
	var from struct{ Kind, Ref string }
	if json.Unmarshal(e.api.Extra["moved_from"], &from) != nil || from.Kind != "repo" || !commitID.MatchString(from.Ref) {
		return ""
	}
	return from.Ref
}

// holds answers whether dir's git already has commit in its HEAD's history, and whether git could say:
// a directory that is not a working tree, or no git, is no answer; a commit git has never seen is not
// held (cli.md §3.6).
func holds(ctx context.Context, dir, commit string) (has, known bool) {
	git := func(args ...string) error {
		return exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Run() //nolint:gosec // the deploy's own directory, and a commit id checked to be forty hex
	}
	if git("rev-parse", "--is-inside-work-tree") != nil {
		return false, false
	}
	if git("cat-file", "-e", commit+"^{commit}") != nil {
		return false, true
	}
	err := git("merge-base", "--is-ancestor", commit, "HEAD")
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, true
	case errors.As(err, &exit) && exit.ExitCode() == 1: // git's "not an ancestor"; 128 is every error
		return false, true
	}
	return false, false
}

// onThePush is cli.md §3.6's pushed head: true when this copy already holds the pushed commit, so the
// deploy goes on with the head as its base after one line; false leaves the refusal, marked as git's
// to settle when a repository feeds the app.
func (a *app) onThePush(ctx context.Context, client *Client, dir string, stale *staleBase) bool {
	commit := stale.pushed()
	if commit == "" {
		return false
	}
	has, known := holds(ctx, dir, commit)
	if has {
		var by, at string
		_ = json.Unmarshal(stale.api.Extra["moved_by"], &by)
		_ = json.Unmarshal(stale.api.Extra["moved_at"], &at)
		said := fmt.Sprintf("main moved: %s pushed %s %s.", cell(cmp.Or(by, "someone")), commit[:7], a.movedAgo(at))
		a.moved = said + " Your working tree already has that commit; deploying on top of it."
		a.printf("\n  %s %s\n    Your working tree already has that commit; deploying on top of it.\n", a.render.paint(ansiAmber, a.render.glyph(stateWarn)), said)
		return true
	}
	if known {
		if repositoryFeeding(ctx, client, stale.app) != "" {
			stale.commit = commit
		}
	}
	return false
}

// repositoryFeeding is the repository people push to that feeds app, by its full name, as the project read
// says: "" for an app Canyon keeps the code of, one no repository feeds, or a read that fails, which is no answer.
func repositoryFeeding(ctx context.Context, client *Client, app string) string {
	var read struct {
		Project struct {
			Repository *repoSource `json:"repository"`
		} `json:"project"`
	}
	if client.get(ctx, "/api/v1/projects/"+url.PathEscape(app), &read) != nil {
		return ""
	}
	if r := read.Project.Repository; r != nil && !r.Keeps && r.State != "deleted" { // pending is Canyon's, keeps true
		return r.FullName
	}
	return ""
}
