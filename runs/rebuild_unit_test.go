package runs

import (
	"testing"
)

// builtPython is the build preview runs as same_inputs reads it: claims-portal's package, its values under key version 1,
// Paketo's Python buildpacks and the packages pip installed from a requirements.txt.
func builtPython(deps ...appDep) buildInputs {
	return buildInputs{Package: "pkg_7d3e8f", Tree: "sha256:" + "7d" + "3e8f", Values: "9f2c", Key: 1,
		Buildpacks: []string{"paketo-buildpacks/ca-certificates", "paketo-buildpacks/cpython", "paketo-buildpacks/pip-install", "paketo-buildpacks/python-start"}, BuildpacksRead: true,
		Deps: deps, DepsRead: true}
}

func pypi(name, version string) appDep {
	return appDep{Type: "python-pkg", Name: name, Version: version, PURL: "pkg:pypi/" + name + "@" + version}
}

// A requirements.txt that pins nothing resolves anew on every build: the same package, values and buildpacks, but pip
// installed a newer flask. same_inputs fails on the app's dependencies, naming what changed (design/PATCHING.md §5).
func TestSameInputsUnpinnedRequirementsFails(t *testing.T) {
	was := builtPython(pypi("flask", "3.0.0"), pypi("werkzeug", "3.0.1"))
	now := builtPython(pypi("flask", "3.0.3"), pypi("werkzeug", "3.0.1"))
	if differs, change := sameInputs(was, now); differs != inputDependencies || change != "flask 3.0.0 → 3.0.3" {
		t.Fatalf("an unpinned requirements.txt that resolved anew: %q %q, want dependencies, flask 3.0.0 → 3.0.3", differs, change)
	}
	if differs, change := sameInputs(was, builtPython(pypi("flask", "3.0.0"), pypi("werkzeug", "3.0.1"), pypi("blinker", "1.8.2"))); differs != inputDependencies || change != "blinker none → 1.8.2" {
		t.Fatalf("a package only the rebuild has: %q %q", differs, change)
	}
	if differs, change := sameInputs(was, builtPython(pypi("flask", "3.0.0"))); differs != inputDependencies || change != "werkzeug 3.0.1 → none" {
		t.Fatalf("a package only preview's build has: %q %q", differs, change)
	}
	if differs, _ := sameInputs(was, builtPython(pypi("werkzeug", "3.0.1"), pypi("flask", "3.0.0"))); differs != "" {
		t.Fatalf("the same packages pinned: %q, want the same inputs", differs)
	}
}

// Two builds' values-only HMACs compare only under one key version: one made under another is never compared, equal or
// not, and a build that recorded none differs (contracts/routes.md, A rebuild patch, step 4).
func TestSameInputsRotatedKeyIDRefusesCompare(t *testing.T) {
	was, now := builtPython(), builtPython()
	now.Key = 2 // the vault's key rotated since preview's build: the same values give another HMAC under it
	if differs, _ := sameInputs(was, now); differs != inputKeyRotated {
		t.Fatalf("values made under key versions 1 and 2: %q, want key_rotated", differs)
	}
	now.Values = "c41a"
	if differs, _ := sameInputs(was, now); differs != inputKeyRotated {
		t.Fatalf("other values under another key version: %q, want key_rotated", differs)
	}
	for name, mutate := range map[string]func(b *buildInputs){
		"no HMAC recorded":    func(b *buildInputs) { b.Values = "" },
		"no key id recorded":  func(b *buildInputs) { b.Key = 0 },
		"other values, key 1": func(b *buildInputs) { b.Values = "c41a" },
	} {
		was := builtPython()
		mutate(&was)
		if differs, _ := sameInputs(was, builtPython()); differs != inputValues {
			t.Errorf("%s: %q, want values", name, differs)
		}
	}
}

// Each input in its order, the first that differs named, and what either build did not record never the same
// (contracts/routes.md, A rebuild patch, step 4).
func TestSameInputsComparesEachInputInOrder(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(was, now *buildInputs)
		want   string
	}{
		{"the same", func(_, _ *buildInputs) {}, ""},
		{"another package", func(_, now *buildInputs) { now.Package = "pkg_0aa1" }, inputPackage},
		{"another tree", func(_, now *buildInputs) { now.Tree = "sha256:0aa1" }, inputPackage},
		{"no tree", func(was, now *buildInputs) { was.Tree, now.Tree = "", "" }, inputPackage},
		{"another package before rotated values", func(_, now *buildInputs) { now.Package, now.Key = "pkg_0aa1", 2 }, inputPackage},
		{"buildpacks in another order", func(_, now *buildInputs) {
			now.Buildpacks = []string{"paketo-buildpacks/cpython", "paketo-buildpacks/ca-certificates", "paketo-buildpacks/pip-install", "paketo-buildpacks/python-start"}
		}, inputBuildpacks},
		{"another buildpack", func(_, now *buildInputs) { now.Buildpacks = append(now.Buildpacks[:3:3], "paketo-buildpacks/procfile") }, inputBuildpacks},
		{"no buildpacks recorded", func(was, _ *buildInputs) { was.Buildpacks, was.BuildpacksRead = nil, false }, inputBuildpacks},
		{"an SBOM that does not read", func(_, now *buildInputs) { now.DepsRead = false }, inputDependencies},
	} {
		was, now := builtPython(pypi("flask", "3.0.0")), builtPython(pypi("flask", "3.0.0"))
		c.mutate(&was, &now)
		if differs, _ := sameInputs(was, now); differs != c.want {
			t.Errorf("%s: %q, want %q", c.name, differs, c.want)
		}
	}
}

// What a rebuild names, from what it fixed: the runtime whose layers hold every fixed package, else newer build tools —
// the app launcher's, a helper's or two runtimes' fixes, never a guess (contracts/routes.md, line same).
func TestRebuildNamesTheRuntime(t *testing.T) {
	fix := func(class, owner string) PlatformFinding {
		return PlatformFinding{Name: "p", Class: class, Owner: owner}
	}
	for _, c := range []struct {
		name  string
		fixed []PlatformFinding
		want  string
	}{
		{"npm's own, in node-engine's layer", []PlatformFinding{fix("runtime", "paketo-buildpacks/node-engine")}, "a newer Node.js"},
		{"pip's, in cpython's layer", []PlatformFinding{fix("runtime", "paketo-buildpacks/cpython")}, "a newer Python"},
		{"the JRE's helper", []PlatformFinding{fix("runtime", "paketo-buildpacks/bellsoft-liberica")}, "a newer Java"},
		{"Go's standard library in the app's own build", []PlatformFinding{fix("runtime", "app")}, "a newer Go"},
		{"npm-install's own binary", []PlatformFinding{fix("runtime", "paketo-buildpacks/npm-install")}, "newer build tools"},
		{"ca-certificates' helper", []PlatformFinding{fix("runtime", "paketo-buildpacks/ca-certificates")}, "newer build tools"},
		{"the app launcher's", []PlatformFinding{fix("lifecycle", "lifecycle")}, "newer build tools"},
		{"Node.js and the launcher", []PlatformFinding{fix("runtime", "paketo-buildpacks/node-engine"), fix("lifecycle", "lifecycle")}, "newer build tools"},
		{"two runtimes", []PlatformFinding{fix("runtime", "paketo-buildpacks/node-engine"), fix("runtime", "paketo-buildpacks/cpython")}, "newer build tools"},
		{"nothing", nil, "newer build tools"},
		{"a fix whose layer's owner was not read", []PlatformFinding{fix("runtime", "")}, "newer build tools"},
	} {
		if got := RebuildNewer(c.fixed); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := RebuildProof("a newer Node.js"); got != "Rebuilt from the same code and the same dependencies with a newer Node.js. A rebuild is a new program, not a copy of what production runs, so a person on your team decides." {
		t.Errorf("the proof: %q", got)
	}
}

// A rebuild's line for each input that differed, as routes.md's lines say it.
func TestRebuildNotSameLines(t *testing.T) {
	for _, c := range []struct {
		p         proved
		key, line string
	}{
		{proved{Differs: inputPackage}, "not-same", "Its code changed when Canyon rebuilt v1.4, so it did not deploy it"},
		{proved{Differs: inputValues}, "not-same", "Its build settings changed when Canyon rebuilt v1.4, so it did not deploy it"},
		{proved{Differs: inputBuildpacks}, "not-same", "How it is built changed when Canyon rebuilt v1.4, so it did not deploy it"},
		{proved{Differs: inputDependencies, Change: "flask 3.0.0 → 3.0.3"}, "not-pinned", "Its dependencies changed when Canyon rebuilt v1.4 (flask 3.0.0 → 3.0.3), so it did not deploy it: pin them in a lockfile, then deploy again"},
		{proved{Differs: inputKeyRotated}, "rotated", "Canyon can't compare v1.4's build settings: Canyon's key for build settings changed since v1.4 was built, so it did not deploy the rebuild"},
	} {
		if key, line := notSameLine(c.p, "v1.4"); key != c.key || line != c.line {
			t.Errorf("%s: %s %q", c.p.Differs, key, line)
		}
	}
}
