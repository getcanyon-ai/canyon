//go:build e2e

package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// e2eNodeBuilder is the Node builder make builder-node makes, which make test-e2e makes first: a test of it fails,
// never skips, when it isn't there.
const e2eNodeBuilder = "canyon-builder-node:local"

// The Node builder chooses what the full builder would choose (contracts/executor.md, A Node app builds on Canyon's
// Node builder; #165's trial trees): each tree builds through an executor with the Node builder, and through one with
// the full builder alone, and the buildpacks that ran, or a failed detect, are the same. A tree the Node builder may
// not take builds on the full one either way; one it takes builds on it.
func TestEndToEndTheNodeBuilderChoosesWhatTheFullBuilderWould(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	server := "require('http').createServer((q, s) => s.end('node ' + process.version)).listen(process.env.PORT || 8080);\n"
	pkg := `{"name":"t","version":"1.0.0","private":true,"scripts":{"start":"node server.js"}}` + "\n"
	static := `{"name":"s","version":"1.0.0","private":true,"scripts":{"build":"mkdir -p dist && cp index.html dist/"}}` + "\n"
	trees := map[string]map[string]string{
		"node-and-requirements":   {"package.json": pkg, "server.js": server, "requirements.txt": "flask==3.1.3\n"},
		"static-with-projecttoml": {"package.json": static, "index.html": "<p>s</p>\n", "project.toml": "[_]\nschema-version = \"0.2\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_WEB_SERVER\"\nvalue = \"nginx\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_WEB_SERVER_ROOT\"\nvalue = \"dist\"\n"},
		"static-no-projecttoml":   {"package.json": static, "index.html": "<p>s</p>\n"},
		"procfile-only":           {"Procfile": "web: sleep infinity\n"},
		"no-match":                {"notes.txt": "nothing to build\n"},
		"node-py-in-subdir":       {"package.json": pkg, "server.js": server, "tools/seed.py": "print('seed')\n"},
		"node-py-at-root":         {"package.json": pkg, "server.js": server, "seed.py": "print('seed')\n"},
		"node-nginx-conf":         {"package.json": pkg, "server.js": server, "nginx.conf": "events {}\nhttp { server { listen 8080; } }\n"},
		"node-one-bin-script":     {"package.json": pkg, "server.js": server, "docker/bin/entrypoint.sh": "#!/bin/sh\nexec node server.js\n"},
	}
	dirs := map[string]string{"demo-claims-portal": "../demo/apps/claims-portal", "tour-app": "../cli/testdata/tour/app"}
	for name, files := range trees {
		dir := t.TempDir()
		for p, body := range files {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
			must(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600))
		}
		dirs[name] = dir
	}
	ran := func(e *Executor, app, dir string) (string, bool) {
		var log bytes.Buffer
		res, err := e.Build(ctx, BuildRequest{App: app, Package: "pkg_e2en"}, treeOf(t, dir), &log)
		var undetected *Undetected
		if errors.As(err, &undetected) {
			return "undetected: " + strings.Join(undetected.Builds, ","), false
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", app, err, tail(log.String(), 40))
		}
		defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/"+app+"@")).Run()
		var ids []string
		for _, bp := range res.Buildpacks {
			ids = append(ids, bp.ID)
		}
		return strings.Join(ids, ","), strings.Contains(log.String(), "This app needs Canyon's full build image")
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range dirs {
			if !yield(k) {
				return
			}
		}
	}) {
		full, _ := ran(&Executor{Join: []string{}}, "e2en-full-"+name, dirs[name])
		node, _ := ran(&Executor{Join: []string{}, NodeBuilder: e2eNodeBuilder}, "e2en-node-"+name, dirs[name])
		t.Logf("%s: %s", name, full)
		if node != full {
			t.Errorf("%s: the full builder ran %q, and with the Node builder %q", name, full, node)
		}
	}
}

// On the Node builder, which carries NODE_ENV and NEXT_PRIVATE_OUTPUT_TRACE_ROOT itself: a stock Next 16 app builds with
// Turbopack and serves its page, so both still apply over what npm-install hands on (contracts/executor.md, Every build
// runs with); and a build value reaches the build beside them, handed as --env alone.
func TestEndToEndTheNodeBuilderKeepsTheBuildVariablesPrecedence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}, NodeBuilder: e2eNodeBuilder}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "next16-node", Package: "pkg_e2e4"}, treeOf(t, filepath.Join("testdata", "apps", "next16-stock")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/next16-node@")).Run()
	if !strings.Contains(log.String(), "Next.js 16.3.8 (Turbopack)") || strings.Contains(log.String(), "Builder creation completed") {
		t.Fatalf("not Turbopack on the Node builder itself:\n%s", tail(log.String(), 60))
	}
	run, err := e.Run(ctx, RunRequest{App: "next16-node", Target: "preview", Image: res.Image, Network: "canyon-app-next16-node"})
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "next16-node", "preview") }()
	if body := get(ctx, t, "next16-node", run, "/"); !strings.Contains(body, "To get started, edit the") {
		t.Fatalf("GET / is not the app's page:\n%s", body)
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"v","version":"1.0.0","private":true,"scripts":{"build":"echo NODE_ENV=$NODE_ENV TRACE=$NEXT_PRIVATE_OUTPUT_TRACE_ROOT VALUE=$CANYON_E2E_VALUE","start":"node server.js"}}`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "server.js"), []byte("require('http').createServer((q, s) => s.end('ok')).listen(process.env.PORT || 8080);\n"), 0o600))
	log.Reset()
	res, err = e.Build(ctx, BuildRequest{App: "values-node", Package: "pkg_e2e5", BuildValues: map[string]string{"CANYON_E2E_VALUE": "seen"}}, treeOf(t, dir), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/values-node@")).Run()
	if !strings.Contains(log.String(), "NODE_ENV=production TRACE=/ VALUE=seen") {
		t.Fatalf("the build script did not see the builder's two variables and the build value:\n%s", tail(log.String(), 60))
	}
}
