package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"text/template/parse"

	"github.com/getcanyon-ai/canyon/gitops"
)

// The stored token never follows an address to a new host (#2470): a save that names another host for the
// repository or its pulls API carries the token again; one that changes a branch, the path or the user does not.
func TestTheTokenNeverFollowsANewHost(t *testing.T) {
	before := &storedRepo{URL: "https://github.com/meridian/deploy", APIURL: "https://api.github.com", API: "github"}
	edit := func(url, api string) repoEdit {
		e := repoEdit{URL: url}
		e.APIURL = &api
		return e
	}
	withCA := func(e repoEdit, ca string) repoEdit {
		e.CA = &ca
		return e
	}
	withAPI := func(e repoEdit, api string) repoEdit {
		e.API = &api
		return e
	}
	for name, c := range map[string]struct {
		e     repoEdit
		moves bool
	}{
		"the same addresses":                  {edit("https://github.com/meridian/deploy", "https://api.github.com"), false},
		"another repository on the same host": {edit("https://github.com/meridian/other", "https://api.github.com"), false},
		"another host for the repository":     {edit("https://git.evil.example/meridian/deploy", "https://api.github.com"), true},
		"another port on the same host":       {edit("https://github.com:8443/meridian/deploy", "https://api.github.com"), true},
		"another host for the pulls API":      {edit("https://github.com/meridian/deploy", "https://api.evil.example"), true},
		"a CA where none was":                 {withCA(edit("https://github.com/meridian/deploy", "https://api.github.com"), "-----BEGIN CERTIFICATE-----"), true},
		"another API on the same hosts":       {withAPI(edit("https://github.com/meridian/deploy", "https://api.github.com"), "none"), true},
	} {
		if got := tokenMoves(before, c.e); got != c.moves {
			t.Errorf("%s: moves %v, want %v", name, got, c.moves)
		}
	}
}

// Flux's namespaces chart takes the deploy repo's path root as this server does (#2479): its root check is rootRe,
// so a root Canyon accepts is never one the chart refuses, and the chart never globs a root Canyon would refuse.
// The template is parsed as Helm's engine parses it, and its one regexMatch of $root read.
func TestTheFluxChartTakesTheRootAsCanyonDoes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "gitops", "namespaces", "templates", "flux.yaml")) //nolint:gosec // this tree's chart
	if err != nil {
		t.Fatal(err)
	}
	tree := parse.New("flux.yaml")
	tree.Mode = parse.SkipFuncCheck | parse.ParseComments
	set := map[string]*parse.Tree{} // the file's own tree and each template it defines, canyon.flux among them
	if _, err := tree.Parse(string(raw), "", "", set); err != nil {
		t.Fatal(err)
	}
	var patterns []string
	var visit func(parse.Node)
	visit = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.ListNode:
			if n != nil {
				for _, c := range n.Nodes {
					visit(c)
				}
			}
		case *parse.IfNode:
			visit(n.Pipe)
			visit(n.List)
			visit(n.ElseList)
		case *parse.RangeNode:
			visit(n.Pipe)
			visit(n.List)
			visit(n.ElseList)
		case *parse.ActionNode:
			visit(n.Pipe)
		case *parse.PipeNode:
			if n != nil {
				for _, c := range n.Cmds {
					visit(c)
				}
			}
		case *parse.CommandNode:
			if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "regexMatch" && len(n.Args) == 3 {
				s, isString := n.Args[1].(*parse.StringNode)
				v, isVar := n.Args[2].(*parse.VariableNode)
				if isString && isVar && len(v.Ident) == 1 && v.Ident[0] == "$root" {
					patterns = append(patterns, s.Text)
				}
			}
			for _, a := range n.Args {
				visit(a)
			}
		}
	}
	for _, tr := range set {
		visit(tr.Root)
	}
	if len(patterns) != 1 || patterns[0] != rootRe.String() {
		t.Errorf("flux.yaml checks $root against %q, want this server's rootRe, %q, alone", patterns, rootRe.String())
	}
}

// Under api: gitlab the repository is a group's project, nested or not, production goes through a merge request,
// and the API is GitLab's on the repository's own host and port: https://<host>/api/v4 when left out, and any other
// host refused, so the token goes nowhere else (#2480).
func TestAGitLabRepoKeepsItsAPIOnItsHost(t *testing.T) {
	edit := func(url string, api *string, mode string) repoEdit {
		e := repoEdit{URL: url}
		gitlab := "gitlab"
		e.API, e.APIURL, e.Prod.Mode = &gitlab, api, &mode
		e.Preview.Branch, e.Prod.Branch = "preview", "main"
		return e
	}
	at := func(s string) *string { return &s }
	for name, c := range map[string]struct {
		e    repoEdit
		code string
		api  string
	}{
		"a group's project, through a merge request": {edit("https://gitlab.meridian.example/platform/deploy.git", nil, "pull_request"), "", "https://gitlab.meridian.example/api/v4"},
		"a nested group's, pushed":                   {edit("https://gitlab.meridian.example/platform/apps/deploy", nil, "push"), "", "https://gitlab.meridian.example/api/v4"},
		"its port kept":                              {edit("https://gitlab.meridian.example:8443/platform/deploy", nil, "pull_request"), "", "https://gitlab.meridian.example:8443/api/v4"},
		"a relative root on its host":                {edit("https://gitlab.meridian.example/gitlab/platform/deploy", at("https://gitlab.meridian.example/gitlab/api/v4"), "pull_request"), "", "https://gitlab.meridian.example/gitlab/api/v4"},
		"a url outside GitLab's root":                {edit("https://gitlab.meridian.example/platform/deploy", at("https://gitlab.meridian.example/gitlab/api/v4"), "pull_request"), "invalid_api", ""},
		"an API that is not /api/v4":                 {edit("https://gitlab.meridian.example/platform/deploy", at("https://gitlab.meridian.example/api/v3"), "pull_request"), "invalid_api", ""},
		"no group":                                   {edit("https://gitlab.meridian.example/deploy", nil, "pull_request"), "invalid_repo_url", ""},
		"the API on another host":                    {edit("https://gitlab.meridian.example/platform/deploy", at("https://gitlab.evil.example/api/v4"), "pull_request"), "invalid_api", ""},
		"the API on another port":                    {edit("https://gitlab.meridian.example/platform/deploy", at("https://gitlab.meridian.example:8443/api/v4"), "pull_request"), "invalid_api", ""},
		"GitHub's API":                               {edit("https://gitlab.meridian.example/platform/deploy", at("https://api.github.com"), "pull_request"), "invalid_api", ""},
	} {
		code, _ := c.e.check()
		if code != c.code {
			t.Errorf("%s: %q, want %q", name, code, c.code)
			continue
		}
		if code == "" && *c.e.APIURL != c.api {
			t.Errorf("%s: api_url %s, want %s", name, *c.e.APIURL, c.api)
		}
	}
	other := "bitbucket"
	e := edit("https://gitlab.meridian.example/platform/deploy", nil, "pull_request")
	e.API = &other
	if code, _ := e.check(); code != "invalid_api" {
		t.Errorf("an api outside github, gitlab and none: %q", code)
	}
}

// A save that moves from one pull requests API to another carries the token again, whatever the hosts: GitHub
// Enterprise and GitLab can share one, and a token made for one API is not the other's (#2486's review).
func TestTheTokenNeverFollowsANewAPI(t *testing.T) {
	before := &storedRepo{URL: "https://git.meridian.example/meridian/deploy", APIURL: "https://git.meridian.example/api/v3", API: "github"}
	gitlab, apiURL := "gitlab", "https://git.meridian.example/api/v4"
	e := repoEdit{URL: before.URL, API: &gitlab, APIURL: &apiURL}
	if !tokenMoves(before, e) {
		t.Error("GitHub Enterprise to GitLab on one host kept the stored token")
	}
	github, same := "github", before.APIURL
	if tokenMoves(before, repoEdit{URL: before.URL, API: &github, APIURL: &same}) {
		t.Error("the same API and addresses asked for the token again")
	}
}

// A relative-root GitLab names the project below its root, URL-encoded whole: the address the save keeps reaches
// the project the url names, never one with the root in its path.
func TestARelativeRootGitLabNamesItsProjectBelowTheRoot(t *testing.T) {
	gitlab, apiURL, mode := "gitlab", "https://gitlab.meridian.example/gitlab/api/v4", "pull_request"
	e := repoEdit{URL: "https://gitlab.meridian.example/gitlab/platform/deploy.git", API: &gitlab, APIURL: &apiURL}
	e.Prod.Mode, e.Preview.Branch, e.Prod.Branch = &mode, "preview", "main"
	if code, msg := e.check(); code != "" {
		t.Fatalf("%s %s", code, msg)
	}
	m, err := gitops.NewMergeRequests(*e.APIURL, e.URL, "", nil)
	if err != nil || url.PathEscape(m.Path) != "platform%2Fdeploy" {
		t.Fatalf("the project: %+v %v, want platform%%2Fdeploy", m, err)
	}
}
