package main

import (
	"slices"
	"strings"
	"testing"
)

// A failed run names the file a stock starter is missing from its facts alone, in exactly the three
// cases contracts/routes.md lists (What a starter is missing; #1654), and nothing otherwise.
func TestStarterFixNamesTheMissingFileFromFactsAlone(t *testing.T) {
	node := []string{"paketo-buildpacks/ca-certificates", "paketo-buildpacks/node-engine", "paketo-buildpacks/npm-install", "paketo-buildpacks/node-run-script"}
	python := []string{"paketo-buildpacks/ca-certificates", "paketo-buildpacks/cpython", "paketo-buildpacks/pip", "paketo-buildpacks/pip-install", "paketo-buildpacks/python-start"}
	with := func(group []string, id string) []string { return append(slices.Clone(group), id) }
	for _, c := range []struct {
		name, reason string
		buildpacks   []string
		root         []string
		file, line   string // line: one line the first example's text holds
	}{
		{"a static site with no project.toml", "detect", nil, []string{"index.html", "style.css"}, "project.toml", `value = "."`},
		{"a static site with a project.toml", "detect", nil, []string{"index.html", "project.toml"}, "", ""},
		{"an undetected tree with no index.html", "detect", nil, []string{"main.rs", "Cargo.toml"}, "", ""},
		{"a Node build with no start command", "start", node, nil, "project.toml", `value = "dist"`},
		{"a Node build with npm start", "start", with(node, "paketo-buildpacks/npm-start"), nil, "", ""},
		{"a Node build with node start", "start", with(node, "paketo-buildpacks/node-start"), nil, "", ""},
		{"a Node build with yarn start", "start", with(node, "paketo-buildpacks/yarn-start"), nil, "", ""},
		{"a Node build with a Procfile", "start", with(node, "paketo-buildpacks/procfile"), nil, "", ""},
		{"a Python build with no Procfile", "start", python, nil, "Procfile", "web: uvicorn main:app --host 0.0.0.0 --port $PORT"},
		{"a Python build with a Procfile", "start", with(python, "paketo-buildpacks/procfile"), nil, "", ""},
		{"a start with no buildpacks recorded", "start", nil, nil, "", ""},
		{"a failed build", "build", python, []string{"index.html"}, "", ""},
	} {
		fix := starterFix(c.reason, c.buildpacks, c.root)
		if c.file == "" {
			if fix != nil {
				t.Errorf("%s: no fix, got %+v", c.name, fix)
			}
			continue
		}
		if fix == nil || fix.File != c.file || fix.Why == "" || len(fix.Examples) == 0 || !hasLine(fix.Examples[0].Text, c.line) {
			t.Errorf("%s: %s holding %q, got %+v", c.name, c.file, c.line, fix)
		}
	}
	if fix := starterFix("start", python, nil); len(fix.Examples) != 2 || fix.Examples[1].Text != "web: gunicorn app:app --bind 0.0.0.0:$PORT\n" {
		t.Errorf("a Python build's Procfile gives FastAPI's line and Flask's: %+v", fix)
	}
	if fix := starterFix("start", node, nil); !hasLine(fix.Examples[0].Text, `name = "BP_WEB_SERVER_ENABLE_PUSH_STATE"`) {
		t.Errorf("a single-page app's project.toml turns push state on: %q", fix.Examples[0].Text)
	}
}

func hasLine(text, line string) bool { return slices.Contains(strings.Split(text, "\n"), line) }
