package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, file, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil { //nolint:gosec // a path under the test's own temporary directory
		t.Fatal(err)
	}
}

func TestAModuleWithNoLicenceFileFails(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/nolicence\n")
	write(t, filepath.Join(root, "README.md"), "no licence here\n")
	write(t, filepath.Join(root, "sub", "LICENSE"), "a licence for sub alone\n")
	_, err := moduleLicenceFiles("example.com/nolicence", root, map[string]bool{filepath.Join(root, "sub"): true})
	if err == nil || !strings.Contains(err.Error(), "example.com/nolicence has no licence file at its root") {
		t.Fatalf("a module with no licence file at its root: got %v, want it refused", err)
	}
}

func TestAModulesLicenceFilesAreItsRootsAndItsLinkedPackages(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "LICENSE"), "root\n")
	write(t, filepath.Join(root, "NOTICE.txt"), "notice\n")
	write(t, filepath.Join(root, "license.go"), "package x\n")
	write(t, filepath.Join(root, "third_party", "LICENSE"), "between\n")
	write(t, filepath.Join(root, "third_party", "forked", "x.go"), "package forked\n")
	write(t, filepath.Join(root, "unlinked", "LICENSE"), "not linked\n")
	files, err := moduleLicenceFiles("example.com/m", root, map[string]bool{filepath.Join(root, "third_party", "forked"): true})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(files)
	slices.Sort(got)
	want := []string{"example.com/m/LICENSE", "example.com/m/NOTICE.txt", "example.com/m/third_party/LICENSE"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAnNPMPackageWithNoLicenceFileFails(t *testing.T) {
	web := t.TempDir()
	write(t, filepath.Join(web, "package-lock.json"), `{"lockfileVersion": 3, "packages": {
		"": {},
		"node_modules/rolldown": {"license": "MIT", "dev": true},
		"node_modules/tailwindcss": {"license": "MIT", "dev": true},
		"node_modules/vite": {"license": "MIT", "dev": true},
		"node_modules/a": {"license": "MIT"},
		"node_modules/devonly": {"license": "GPL-3.0-only", "dev": true}}}`)
	write(t, filepath.Join(web, "node_modules", "rolldown", "LICENSE"), "MIT\n")
	write(t, filepath.Join(web, "node_modules", "tailwindcss", "LICENSE"), "MIT\n")
	write(t, filepath.Join(web, "node_modules", "vite", "LICENSE.md"), "MIT\n")
	write(t, filepath.Join(web, "node_modules", "a", "index.js"), "\n")
	_, err := npmComponents(web)
	if err == nil || !strings.Contains(err.Error(), "npm package node_modules/a has no licence file") {
		t.Fatalf("got %v, want node_modules/a refused", err)
	}
	if strings.Contains(err.Error(), "devonly") {
		t.Fatalf("a devDependency outside the bundle was read: %v", err)
	}

	write(t, filepath.Join(web, "node_modules", "a", "LICENSE"), "MIT\n")
	comps, err := npmComponents(web)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range comps {
		names = append(names, c.name)
	}
	slices.Sort(names)
	if want := []string{"a", "rolldown", "tailwindcss", "vite"}; !slices.Equal(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestOnlyAllowedLicencesAndNamedExceptionsPass(t *testing.T) {
	for _, c := range []struct {
		name, spdx string
		ok         bool
	}{
		{"x", "MIT", true},
		{"x", "Apache-2.0 AND BSD-3-Clause", true},
		{"x", "(MIT OR GPL-3.0-only)", true},
		{"x", "GPL-3.0-only", false},
		{"x", "MIT AND GPL-3.0-only", false},
		{"x", "Apache-2.0 WITH LLVM-exception", false},
		{"x", "CC0-1.0", false},
		{"github.com/therootcompany/xz", "CC0-1.0", true},
		{"x", "", false},
		{"x", "MIT AND", false},
		{"x", "(MIT", false},
	} {
		err := permitted(c.name, c.spdx)
		if (err == nil) != c.ok {
			t.Errorf("%s under %q: got %v, want permitted %v", c.name, c.spdx, err, c.ok)
		}
	}
}

// fakeRepo is a repository with every release binary, one third-party module (example.com/dep,
// MIT, replaced by a local directory), the dashboard's lockfile and node_modules, and the vendored
// files: the smallest tree run accepts.
func fakeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/repo\n\ngo 1.26\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n")
	write(t, filepath.Join(root, "dep", "go.mod"), "module example.com/dep\n\ngo 1.26\n")
	write(t, filepath.Join(root, "dep", "dep.go"), "package dep\n\nfunc Dep() {}\n")
	write(t, filepath.Join(root, "dep", "LICENSE"), "MIT License, the dep's\n")
	for _, b := range binaries {
		write(t, filepath.Join(root, "cmd", b.name, "main.go"), "package main\n\nimport \"example.com/dep\"\n\nfunc main() { dep.Dep() }\n")
	}
	write(t, filepath.Join(root, "web", "package-lock.json"), `{"lockfileVersion": 3, "packages": {"": {},
		"node_modules/rolldown": {"license": "MIT", "dev": true}, "node_modules/tailwindcss": {"license": "MIT", "dev": true},
		"node_modules/vite": {"license": "MIT", "dev": true}}}`)
	write(t, filepath.Join(root, "web", "node_modules", "rolldown", "LICENSE"), "rolldown's\n")
	write(t, filepath.Join(root, "web", "node_modules", "tailwindcss", "LICENSE"), "tailwind's\n")
	write(t, filepath.Join(root, "web", "node_modules", "vite", "LICENSE.md"), "vite's\n")
	for _, v := range vendored {
		for _, f := range v.files {
			if filepath.Ext(f) != ".tar" {
				write(t, filepath.Join(root, f), v.name+"'s licence\n")
				continue
			}
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(&tar.Header{Name: "LICENSE", Mode: 0o600, Size: 7, Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("rules'\n")); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(root, f), buf.String())
		}
	}
	return root
}

func TestAModuleWithNoReviewedLicenceFails(t *testing.T) {
	root := fakeRepo(t)
	err := run(root, filepath.Join(root, "NOTICES"), "linux/amd64", false, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "module example.com/dep has no reviewed licence") {
		t.Fatalf("a module goLicences does not name: got %v, want it refused", err)
	}
	err = run(root, filepath.Join(root, "NOTICES"), "linux/amd64", false, map[string]string{"example.com/dep": "MIT", "example.com/gone": "MIT"})
	if err == nil || !strings.Contains(err.Error(), "goLicences names example.com/gone, which no binary links") {
		t.Fatalf("a goLicences entry no binary links: got %v, want it refused", err)
	}
}

func TestCheckFailsOnDriftAndPassesOnItsOwnOutput(t *testing.T) {
	root := fakeRepo(t)
	file, table := filepath.Join(root, "NOTICES"), map[string]string{"example.com/dep": "MIT"}
	if err := run(root, file, "linux/amd64", false, table); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"example.com/dep\n    MIT; in canyon, canyon-server, canyon-executor, canyon-executor-kube, canyon-identity\n", "example.com/dep/LICENSE\n", "MIT License, the dep's\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the notices lack %q:\n%s", want, b)
		}
	}
	if err := run(root, file, "linux/amd64", true, table); err != nil {
		t.Fatalf("check against its own output: %v", err)
	}
	write(t, file, strings.Replace(string(b), "MIT License, the dep's", "MIT License, someone's", 1))
	if err := run(root, file, "linux/amd64", true, table); err == nil || !strings.Contains(err.Error(), "is stale from its line") {
		t.Fatalf("check against a drifted file: got %v, want it stale", err)
	}
}

func TestTwoVersionsLicencesBothApply(t *testing.T) {
	web := t.TempDir()
	write(t, filepath.Join(web, "package-lock.json"), `{"lockfileVersion": 3, "packages": {"": {},
		"node_modules/rolldown": {"license": "MIT", "dev": true},
		"node_modules/tailwindcss": {"license": "MIT", "dev": true},
		"node_modules/vite": {"license": "MIT", "dev": true},
		"node_modules/a": {"license": "MIT OR GPL-3.0-only"},
		"node_modules/b/node_modules/a": {"license": "GPL-3.0-only"}}}`)
	for _, d := range []string{"rolldown", "tailwindcss", "vite", "a", "b/node_modules/a"} {
		write(t, filepath.Join(web, "node_modules", filepath.FromSlash(d), "LICENSE"), d+"\n")
	}
	for range 5 { // map order differs run to run: the result must not
		comps, err := npmComponents(web)
		if err != nil {
			t.Fatal(err)
		}
		if comps[0].name != "a" || comps[0].spdx != "(GPL-3.0-only) AND (MIT OR GPL-3.0-only)" {
			t.Fatalf("got %s under %q, want a under both versions' licences, each parenthesised", comps[0].name, comps[0].spdx)
		}
		if err := permitted(comps[0].name, comps[0].spdx); err == nil {
			t.Fatalf("%q passed: the GPL-only version's licence was absorbed into the other's OR", comps[0].spdx)
		}
	}
}
