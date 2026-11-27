package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The Node builder's first condition (contracts/executor.md, A Node app builds on Canyon's Node builder): the tree and
// its build environment carry nothing that could make a buildpack group the full builder orders before nodejs pass.
// Every entry below was read from those groups' detect code at the pins chosenFor names (#174, #165): entry names and
// globs, and build variables set at all, never their values. It is a superset: a tree it can't rule out builds on
// canyon-builder, which then decides as it always did. It never names the app's language.

// chosenFor is what the lists below were read for: the full builder's pin (its order) and each buildpack group's,
// ruby to java before nodejs, and nodejs's own, as executor/builder.Dockerfile and deploy/k8s/builds.yaml pin them.
// Written without their repository, so the Paketo bump never moves them: a test fails while any pin differs, until
// the detect code is read again at the new one and these are moved by hand.
var chosenFor = map[string]string{
	"ubuntu-noble-builder": "sha256:5553d472fefc8dc1d9fb794422ec2f931fd2bf470c107b1786b048096c0d7f45",
	"ruby":                 "sha256:34f98ac9a162aeeb4d49df493a776eaadee75f33e73162578389f0946aaff137", // 3.5.1
	"dotnet-core":          "sha256:02160f7339e1fed14f3dc48843f37f19fab8422f84e7217cbf5f9775c7abd6c8", // 4.15.2
	"go":                   "sha256:db78f48f0b6d51607537db04c91e1611cea283bc05e09ab9554ddb9af7b33253", // 4.23.5
	"python":               "sha256:a0dfb1b73cdc69c9d31638ad1e1ebce55f2aded0c3061c40c2d502de3d59a965", // 2.70.0
	"php":                  "sha256:3cee3ccfaaf4eda4b1341f211af9fbbe55ed2955ae71b29c5c6abd8fac1d7801", // 3.8.2
	"web-servers":          "sha256:a01f811143ca2629a1cfb2b053a63de0b845cf6e38d19ca375f2c3d6c62c6e97", // 4.16.3
	"java-native-image":    "sha256:60486e497d2554626f621fef1ab8866acd23328541d0f5bc34d2689a5630af0c", // 14.15.1
	"java":                 "sha256:fc85cd4628fcdb50089d55331e2b48511195993111972d05b24d3df40332e153", // 22.10.0
	"nodejs":               "sha256:ba6341b9dee1a3733f912fcf843e4c0e4b251dbf0ea5abdc09a819f2c3015f79", // 10.17.0
}

// rootNames are entries at the tree's root, of any type, whose name alone can make an earlier group pass.
var rootNames = map[string]string{
	"Gemfile": "ruby", "Rakefile": "ruby", "Rakefile.rb": "ruby", "rakefile": "ruby", "rakefile.rb": "ruby",
	"requirements.txt": "python", "pyproject.toml": "python", "Pipfile.lock": "python", "uv.lock": "python",
	"environment.yml": "python", "pixi.lock": "python", "package-list.txt": "python",
	"nginx.conf": "web-servers", "httpd.conf": "web-servers",
	"pom.xml": "java", "build.gradle": "java", "build.gradle.kts": "java", "settings.gradle": "java",
	"settings.gradle.kts": "java", "project.clj": "java", "deps.edn": "java", "build.sbt": "java",
	"WEB-INF": "java", "META-INF": "java", "usr": "java", "wlp": "java",
}

// rootGlobs are patterns over the root's entry names, as path.Match reads them.
var rootGlobs = map[string]string{
	"*.runtimeconfig.json": "dotnet-core", "*.csproj": "dotnet-core", "*.fsproj": "dotnet-core", "*.vbproj": "dotnet-core",
	"*.go": "go", "*.py": "python", "*.php": "php", "*.war": "java", "*.ear": "java",
}

// buildVariables are the build variables whose being set at all can make an earlier group pass, or names a path
// detect reads; and the executor's own two, which only --env can replace (the Node builder carries them).
var buildVariables = map[string]string{
	"BP_DOTNET_PROJECT_PATH": "dotnet-core", "BP_GO_TARGETS": "go", "BP_GO_WORKDIR": "go", "BP_PHP_WEB_DIR": "php",
	"BP_PHP_SERVER": "php", "BP_NGINX_CONF_LOCATION": "web-servers", "BP_WEB_SERVER": "web-servers",
	"BP_MAVEN_POM_FILE": "java", "BP_GRADLE_BUILD_FILE": "java", "BP_EXECUTABLE_JAR_LOCATION": "java",
	"BP_APPLICATION_SCRIPT": "java", "BP_TOMEE_EAR_ENABLED": "java",
	"NODE_ENV": "the Node builder's own", "NEXT_PRIVATE_OUTPUT_TRACE_ROOT": "the Node builder's own",
}

// buildNames is every variable the build in dir sets: its build values, and the build environment of the project.toml
// Canyon reads. why is "" unless that project.toml can't be read or names buildpacks, and then says which.
func (e *Executor) buildNames(ctx context.Context, dir string, values map[string]string) (names map[string]bool, why string) {
	names = map[string]bool{}
	for k := range values {
		names[k] = true
	}
	switch b, err := os.ReadFile(filepath.Join(dir, "project.toml")); { //nolint:gosec // dir is the build's own
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return names, "project.toml can't be read"
	default:
		var w written
		if err := e.read(ctx, b, "", &w); err != nil { // with no builder it reads the file alone
			return names, "project.toml can't be read: " + err.Error()
		}
		if w.named {
			return names, "project.toml names buildpacks"
		}
		if w.IO.Buildpacks.Build != nil {
			for _, v := range w.IO.Buildpacks.Build.Env {
				names[v.Name] = true
			}
		}
	}
	return names, ""
}

// ownBuilderFor is whether a build on canyon-builder may build on ownBuilder: CANYON_BUILDER and CANYON_IMAGE_RUN
// unset, and nothing in the build naming either of the executor's own two build variables, which ownBuilder's own
// build environment would override. A project.toml that can't be read, or names buildpacks, builds as before.
func (e *Executor) ownBuilderFor(ctx context.Context, dir string, values map[string]string) bool {
	if e.Builder != "" || e.RunImage != "" {
		return false
	}
	names, why := e.buildNames(ctx, dir, values)
	if why != "" {
		return false
	}
	for name := range BuildEnv(nil) {
		if names[name] {
			return false
		}
	}
	return true
}

// fullBuilderFor is why the tree in dir, with its build values and its project.toml's build environment, builds on
// canyon-builder: "" when nothing rules the Node builder out, else the entry or variable that did and the group it
// came from. An entry that can't be read rules it out. The tree holds directories and regular files alone (untar), so
// no link leads detect outside what this reads.
func (e *Executor) fullBuilderFor(ctx context.Context, dir string, values map[string]string) string {
	names, why := e.buildNames(ctx, dir, values)
	if why != "" {
		return why
	}
	for name := range names {
		if group, ok := buildVariables[name]; ok {
			return name + " is set (" + group + ")"
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "the tree can't be read"
	}
	for _, d := range entries {
		if group, ok := rootNames[d.Name()]; ok {
			return d.Name() + " (" + group + ")"
		}
		for glob, group := range rootGlobs {
			if ok, _ := path.Match(glob, d.Name()); ok {
				return d.Name() + " (" + group + ")"
			}
		}
	}
	walked := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case !d.IsDir() && strings.HasSuffix(d.Name(), ".jar"):
			why = rel + " (java)" // executable-jar walks the whole tree for one with a Main-Class
		case len(parts) == 3 && parts[1] == "bin":
			why = rel + " (java)" // dist-zip's */bin/*, as filepath.Glob matches it from the root
		case !d.IsDir() && parts[0] == "cmd" && strings.HasSuffix(d.Name(), ".go"):
			why = rel + " (go)" // go-build walks cmd/ for a *.go at any depth
		}
		if why != "" {
			return fs.SkipAll
		}
		return nil
	})
	if walked != nil {
		return "the tree can't be read"
	}
	return why
}
