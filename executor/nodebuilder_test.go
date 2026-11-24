package executor

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// choose.go's lists were read from the detect code at the pins chosenFor names: while any of those pins differs from
// what deploy/k8s/builds.yaml and builder.Dockerfile pin, the lists may no longer be a superset, and this fails until
// the detect code is read again (contracts/executor.md, A Node app builds on Canyon's Node builder).
func TestTheNodeBuildersRuleIsReadForTheseVeryPins(t *testing.T) {
	pinRE := regexp.MustCompile(`docker\.io/paketobuildpacks/([a-z0-9-]+)@(sha256:[0-9a-f]{64})`) // the Paketo bump's own pattern
	pinned := map[string]string{}
	for _, f := range []string{"../deploy/k8s/builds.yaml", "builder.Dockerfile"} {
		b, err := os.ReadFile(f) //nolint:gosec // the repository's own file
		must(t, err)
		for _, m := range pinRE.FindAllStringSubmatch(string(b), -1) {
			pinned[m[1]] = m[2]
		}
	}
	for name, digest := range chosenFor {
		if pinned[name] != digest {
			t.Errorf("paketobuildpacks/%s is pinned at %s, and choose.go was read at %s: read its detect code again, then move chosenFor", name, pinned[name], digest)
		}
	}
}

// A tree goes to canyon-builder when anything in it, or in its build environment, could make a group the full
// builder orders before nodejs pass; the trees the demo and the tour build, and a Node app's ordinary files, go to the
// Node builder. Each case is a trial tree of #165's, or one of each kind on the list.
func TestTheNodeBuilderIsChosenOnlyWhenNoEarlierGroupCouldPass(t *testing.T) {
	node := map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`, "server.js": "require('http')"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range node {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	full := map[string]struct {
		tree   map[string]string
		values map[string]string
	}{
		"requirements.txt beside package.json (a)":        {with(map[string]string{"requirements.txt": "flask\n"}), nil},
		"a static site, project.toml's BP_WEB_SERVER (b)": {with(map[string]string{"project.toml": "[_]\nschema-version = \"0.2\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_WEB_SERVER\"\nvalue = \"nginx\"\n"}), nil},
		"Gemfile beside package.json (c)":                 {with(map[string]string{"Gemfile": "source 'https://rubygems.org'\n"}), nil},
		"seed.py at the root":                             {with(map[string]string{"seed.py": "print(1)\n"}), nil},
		"nginx.conf":                                      {with(map[string]string{"nginx.conf": "events {}\n"}), nil},
		"one <dir>/bin/<file>":                            {with(map[string]string{"docker/bin/entrypoint.sh": "#!/bin/sh\n"}), nil},
		"a jar deep in the tree":                          {with(map[string]string{"vendor/lib/x/tool.jar": "PK"}), nil},
		"a .go file under cmd/":                           {with(map[string]string{"cmd/tool/main.go": "package main\n"}), nil},
		"main.go at the root":                             {with(map[string]string{"main.go": "package main\n"}), nil},
		"a .NET project file":                             {with(map[string]string{"app.csproj": "<Project/>"}), nil},
		"index.php":                                       {with(map[string]string{"index.php": "<?php\n"}), nil},
		"pom.xml":                                         {with(map[string]string{"pom.xml": "<project/>"}), nil},
		"META-INF":                                        {with(map[string]string{"META-INF/MANIFEST.MF": "Main-Class: x\n"}), nil},
		"a build value BP_WEB_SERVER":                     {node, map[string]string{"BP_WEB_SERVER": "nginx"}},
		"a build value BP_GO_TARGETS":                     {node, map[string]string{"BP_GO_TARGETS": "./x"}},
		"a build value NODE_ENV, which only --env can replace": {node, map[string]string{"NODE_ENV": "development"}},
		"project.toml names a buildpack group":                 {with(map[string]string{"project.toml": "[_]\nschema-version = \"0.2\"\n\n[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n"}), nil},
		"project.toml that can't be read":                      {with(map[string]string{"project.toml": "[_\n"}), nil},
	}
	e := &Executor{}
	for name, c := range full {
		dir := t.TempDir()
		for p, body := range c.tree {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
			must(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600))
		}
		if why := e.fullBuilderFor(context.Background(), dir, c.values); why == "" {
			t.Errorf("%s: the Node builder was chosen", name)
		}
	}
	nodeTrees := map[string]map[string]string{
		"package.json and server.js":                    node,
		"a static site without project.toml (b)":        {"package.json": `{"scripts":{"build":"mkdir -p dist"}}`, "index.html": "<p>"},
		"a .py file below the root":                     with(map[string]string{"tools/seed.py": "print(1)\n"}),
		"bin/<file> at depth two, not <dir>/bin/<file>": with(map[string]string{"bin/www": "#!/bin/sh\n"}),
		"a Procfile alone, which detect then decides":   {"Procfile": "web: node server.js\n"},
		"a project.toml with a Node build variable":     with(map[string]string{"project.toml": "[_]\nschema-version = \"0.2\"\n\n[[io.buildpacks.build.env]]\nname = \"BP_NODE_RUN_SCRIPTS\"\nvalue = \"build\"\n"}),
	}
	for name, tree := range nodeTrees {
		dir := t.TempDir()
		for p, body := range tree {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
			must(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600))
		}
		if why := e.fullBuilderFor(context.Background(), dir, map[string]string{"NEXT_PUBLIC_API_URL": "https://api.example"}); why != "" {
			t.Errorf("%s: canyon-builder was chosen: %s", name, why)
		}
	}
	for _, dir := range []string{"../demo/apps/claims-portal", "../demo/apps/endorsement-hub", "../demo/apps/claims-archive", "../demo/apps/premium-ledger", "../demo/apps/claims-portal-next", "../cli/testdata/tour/app"} {
		if why := e.fullBuilderFor(context.Background(), dir, nil); why != "" {
			t.Errorf("%s, which the demo or the tour builds: canyon-builder was chosen: %s", dir, why)
		}
	}
}

// GET /builder's identity with a Node builder covers both builders: the Node builder's image ID and canyon-builder's
// file, never canyon-builder's image ID, which only a download makes.
func TestTheIdentityCoversBothBuildersWithoutTheFullOnesImage(t *testing.T) {
	one, two := nodeIdentity("sha256:"+strings.Repeat("a", 64)), nodeIdentity("sha256:"+strings.Repeat("b", 64))
	if one == two || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(one) {
		t.Fatalf("another Node builder, the same identity, or not sha256:<64 hex>: %s %s", one, two)
	}
	saved := builderImage.repo
	defer func() { builderImage.repo = saved }()
	builderImage.repo = "canyon-builder:000000000000"
	if nodeIdentity("sha256:"+strings.Repeat("a", 64)) == one {
		t.Fatal("another builder.Dockerfile, the same identity")
	}
}

// The Node builder's log is held until its detection passed: a tree it detects nothing in shows canyon-builder's
// build alone; one that fails later shows all of it.
func TestTheNodeBuildersLogIsHeldUntilDetectionPassed(t *testing.T) {
	var out bytes.Buffer
	g := &gated{out: &out}
	_, _ = io.WriteString(g, "===> ANALYZING\n===> DETECTING\n")
	if out.Len() != 0 {
		t.Fatalf("written before detection passed: %q", out.String())
	}
	_, _ = io.WriteString(g, "===> RESTORING\n")
	_, _ = io.WriteString(g, "===> BUILDING\n")
	if out.String() != "===> ANALYZING\n===> DETECTING\n===> RESTORING\n===> BUILDING\n" {
		t.Fatalf("not all of it, in order: %q", out.String())
	}
}

// With a Node builder, a Node app builds on it with neither --run-image nor --env when it has no build values, so pack
// builds on the builder itself; a build value is handed as --env alone, never the two the builder carries; a tree it
// can't take, or CANYON_BUILDER set, builds on canyon-builder as before.
func TestANodeAppBuildsOnTheNodeBuilderWithNoEphemeralCopy(t *testing.T) {
	const nodeRef = "ghcr.io/getcanyon-ai/canyon-builder-node@sha256:4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e"
	build := func(builder string, values map[string]string, tree map[string]string) string {
		e, dir := fakes(t)
		e.NodeBuilder, e.Builder, builderImage.id = nodeRef, builder, ""
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: values}, bytes.NewReader(tarOf(t, tree)), io.Discard); err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
		return string(argv)
	}
	node := map[string]string{"package.json": "{}", "server.js": ""}
	if argv := build("", nil, node); !strings.Contains(argv, "\n--builder\n"+nodeRef+"\n") || strings.Contains(argv, "--run-image") || strings.Contains(argv, "--env") {
		t.Errorf("a Node app with no build values: pack argv %q", argv)
	}
	if argv := build("", map[string]string{"NEXT_PUBLIC_API_URL": "https://api.example"}, node); !strings.Contains(argv, "\n--env\nNEXT_PUBLIC_API_URL\n") || strings.Contains(argv, "NODE_ENV") || !strings.Contains(argv, nodeRef) {
		t.Errorf("a Node app with a build value: pack argv %q", argv)
	}
	tag := "canyon-builder:" + builderImage.repo[len("canyon-builder:"):]
	if argv := build("", nil, map[string]string{"package.json": "{}", "requirements.txt": ""}); !strings.Contains(argv, "\n--builder\n"+tag+"\n") || !strings.Contains(argv, "\n--run-image\ncanyon-run:noble\n") || !strings.Contains(argv, "\n--env\nNODE_ENV\n") {
		t.Errorf("a tree the Node builder can't take: pack argv %q", argv)
	}
	if argv := build("paketobuildpacks/ubuntu-noble-builder", nil, node); strings.Contains(argv, nodeRef) {
		t.Errorf("CANYON_BUILDER set: pack argv %q", argv)
	}
}

// A tree the Node builder detects nothing in builds on canyon-builder, which decides as it always did, and the build's
// log has nothing of the Node builder's attempt.
func TestATreeTheNodeBuilderDetectsNothingInBuildsOnTheFullOne(t *testing.T) {
	const nodeRef = "ghcr.io/getcanyon-ai/canyon-builder-node@sha256:4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e"
	e, dir := fakes(t)
	e.NodeBuilder, builderImage.id = nodeRef, ""
	must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> \""+dir+"/pack-argv\"\ncase \"$*\" in *canyon-builder-node*) echo '===> DETECTING'; echo 'node attempt'; echo '"+lifecycleFailed+"20'; exit 1;; esac\necho '===> RESTORING'; echo 'full build'; exit 0\n"), 0o700)) //nolint:gosec // a test fake must be executable
	var out bytes.Buffer
	if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"Procfile": "web: x\n"})), &out); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	if strings.Count(string(argv), "build\ncanyon/claims-portal:") != 2 || strings.Contains(out.String(), "node attempt") || !strings.Contains(out.String(), "full build") ||
		!strings.Contains(out.String(), "This app needs Canyon's full build image, which this Docker host doesn't have yet. Downloading it once: up to a 1.4 GB download, 3.3 GB on disk, about 7 minutes at 25 Mbit/s and longer on a slower line\n") {
		t.Fatalf("pack argv %q\nlog %q", argv, out.String())
	}
}
