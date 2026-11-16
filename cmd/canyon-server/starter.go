package main

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/getcanyon-ai/canyon/ingest"
)

// fix is the one file a stock starter is missing (contracts/routes.md, What a starter is missing).
type fix struct {
	File     string       `json:"file"`
	Why      string       `json:"why"`
	Examples []fixExample `json:"examples"`
}

type fixExample struct {
	For  string `json:"for"`
	Text string `json:"text"`
}

const staticToml = `[_]
schema-version = "0.2"

[[io.buildpacks.build.env]]
name = "BP_WEB_SERVER"
value = "nginx"

[[io.buildpacks.build.env]]
name = "BP_WEB_SERVER_ROOT"
`

// starterFix names the file from the run's facts alone: its failure reason, the package's root file
// names and the buildpacks of the group that passed detection. Nil in every other case.
func starterFix(reason string, buildpacks, root []string) *fix {
	ran := func(id string) bool { return slices.Contains(buildpacks, "paketo-buildpacks/"+id) }
	switch {
	case reason == "detect" && slices.Contains(root, "index.html") && !slices.Contains(root, "project.toml"):
		return &fix{File: "project.toml",
			Why:      "No buildpack recognised the package: a static site needs a project.toml at its root that has nginx serve its files.",
			Examples: []fixExample{{For: "a static site served from its root", Text: staticToml + "value = \".\"\n"}}}
	case reason == "start" && ran("node-engine") && !ran("npm-start") && !ran("node-start") && !ran("yarn-start") && !ran("procfile"):
		return &fix{File: "project.toml",
			Why: "The build gave the app no start command, so it could not start: a single-page app needs a project.toml at its root that has nginx serve what its build writes.",
			Examples: []fixExample{{For: "a single-page app whose build writes dist, as Vite's does",
				Text: staticToml + "value = \"dist\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_WEB_SERVER_ENABLE_PUSH_STATE\"\nvalue = \"true\"\n"}}}
	case reason == "start" && ran("python-start") && !ran("procfile"):
		return &fix{File: "Procfile",
			Why: "The build gave the app no start command, so Python started with nothing to run and exited: a Python web app needs a Procfile at its root naming the command that starts it on $PORT.",
			Examples: []fixExample{{For: "FastAPI, with uvicorn in requirements.txt (fastapi[standard] brings it)", Text: "web: uvicorn main:app --host 0.0.0.0 --port $PORT\n"},
				{For: "Flask, with gunicorn in requirements.txt", Text: "web: gunicorn app:app --bind 0.0.0.0:$PORT\n"}}}
	}
	return nil
}

// setFix sets the run read's fix on a failed detect or start; only a failed detect reads the
// package's root file names.
func (a *app) setFix(ctx context.Context, out *gateRun, project, commit string, buildpacks []string) error {
	if out.State != "failed" || out.FailureReason == nil {
		return nil
	}
	var root []string
	if *out.FailureReason == "detect" {
		var err error
		if root, err = ingest.RootNames(ctx, filepath.Join(a.repos, project+".git"), commit); err != nil {
			return err
		}
	}
	out.Fix = starterFix(*out.FailureReason, buildpacks, root)
	return nil
}
