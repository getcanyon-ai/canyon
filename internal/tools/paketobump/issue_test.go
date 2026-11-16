package main

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// issues is a repository's open patching issues, recording what the bump files.
type issues struct {
	list    []issue
	created []string // each opened issue's body
	edited  map[int]string
}

func (f *issues) open() ([]issue, error) { return f.list, nil }

func (f *issues) create(_, body string) error {
	f.created = append(f.created, body)
	return nil
}

func (f *issues) edit(number int, _, body string) error {
	f.edited[number] = body
	return nil
}

func bumpTo(digest string) result {
	return result{
		Moves: []move{{Repo: run, Old: "sha256:" + strings.Repeat("1", 64), New: "sha256:" + strings.Repeat(digest, 64)}},
		Lines: map[string][]string{"os": {"- `libssl3` 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5"}},
	}
}

// filed is an open issue with body, written by author.
func filed(number int, login string, isBot bool, body string) issue {
	i := issue{Number: number, Body: body}
	i.Author.Login, i.Author.IsBot = login, isBot
	return i
}

// The bump files one issue and never a second: the open issue the workflow filed for the same pins is
// updated, one it filed for older pins is updated to these, and with none, one is opened. It reads an
// issue for its key alone: one another author wrote, or whose marker names a branch, is never the bump's,
// and the branch it pushes and links is always the pins' own. A branch that does not push files nothing.
func TestIssueDedupesOntoTheOpenIssue(t *testing.T) {
	r := bumpTo("2")
	_, same := r.issue("acme/canyon")
	_, older := bumpTo("3").issue("acme/canyon")
	evil := "<!-- paketo-bump key=" + r.key() + " branch=ci/evil -->\nhttps://github.com/acme/canyon/compare/main...ci/evil?expand=1\n"
	forged := strings.ReplaceAll(same, r.branch(), "ci/evil") // a person edited the workflow's issue
	unrelated := filed(3, bot, true, "The full e2e run is red on main")
	for name, c := range map[string]struct {
		open    []issue
		pushErr error
		edited  int // 0: one opened
		wantErr bool
	}{
		"none open":                     {open: []issue{unrelated}},
		"open for the same pins":        {open: []issue{unrelated, filed(9, bot, true, older), filed(7, bot, true, forged)}, edited: 7},
		"open for older pins":           {open: []issue{unrelated, filed(9, bot, true, older)}, edited: 9},
		"another author's for the pins": {open: []issue{filed(5, "mallory", false, same), filed(6, bot, false, same)}},
		"a marker naming a branch":      {open: []issue{filed(8, bot, true, evil)}},
		"the branch fails to push":      {open: []issue{unrelated}, pushErr: errors.New("rejected"), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			gh := &issues{list: c.open, edited: map[int]string{}}
			var pushed []string
			err := file(r, "acme/canyon", gh, func(branch string) error {
				pushed = append(pushed, branch)
				return c.pushErr
			})
			if (err != nil) != c.wantErr || !slices.Equal(pushed, []string{r.branch()}) {
				t.Fatalf("file: %v, pushed %v; want error %v, pushed %s", err, pushed, c.wantErr, r.branch())
			}
			bodies := append(slices.Clone(gh.created), slices.Collect(maps.Values(gh.edited))...)
			switch {
			case c.wantErr && len(bodies) > 0:
				t.Fatalf("a branch that did not push filed %d issues", len(bodies))
			case c.wantErr:
				return
			case len(bodies) != 1 || (c.edited == 0) != (len(gh.created) == 1) || c.edited != 0 && gh.edited[c.edited] == "":
				t.Fatalf("opened %d and edited %v; want issue #%d edited (0: one opened), never a second", len(gh.created), slices.Sorted(maps.Keys(gh.edited)), c.edited)
			}
			if body := bodies[0]; !strings.HasPrefix(body, "<!-- paketo-bump key="+r.key()+" -->\n") ||
				!strings.Contains(body, "https://github.com/acme/canyon/compare/main..."+r.branch()+"?expand=1") || strings.Contains(body, "ci/evil") {
				t.Errorf("the filed body does not link the pins' own branch %s alone:\n%s", r.branch(), body)
			}
		})
	}
}
