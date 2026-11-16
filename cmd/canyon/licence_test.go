package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// cliPlatforms is the Makefile's CLI_PLATFORMS: every platform the canyon CLI is released for.
var cliPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}

// TestCLIIsApache holds the built canyon CLI to Apache-2.0 alone (LICENSE, #2878): every package of this
// module it links, on every platform it ships for, has the Apache-2.0 text as the nearest LICENSE up from its
// directory. A package under the root LICENSE's AGPL-3.0 that becomes reachable fails, named with its platform.
func TestCLIIsApache(t *testing.T) {
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	module := os.DirFS(root) // every read stays inside the module's tree
	apache, err := fs.ReadFile(module, "cli/LICENSE")
	if err != nil {
		t.Fatalf("cli/LICENSE, the Apache-2.0 text: %v", err)
	}
	if !bytes.Contains(apache, []byte("Apache License\n                           Version 2.0, January 2004")) {
		t.Fatal("cli/LICENSE is not the Apache-2.0 text")
	}
	for _, platform := range cliPlatforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		list := exec.Command("go", "list", "-deps", "-f", "{{if and .Module .Module.Main}}{{.Dir}}{{end}}", ".")
		list.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		out, err := list.Output()
		if err != nil {
			t.Fatalf("go list -deps for %s: %v", platform, err)
		}
		dirs := strings.Fields(string(out))
		if len(dirs) == 0 {
			t.Fatalf("go list -deps for %s named no package of this module", platform)
		}
		for _, dir := range dirs {
			rel, err := filepath.Rel(root, dir)
			if err != nil || !fs.ValidPath(filepath.ToSlash(rel)) {
				t.Fatalf("%s: go list named %s, outside the module at %s", platform, dir, root)
			}
			licence, at := nearestLicence(module, filepath.ToSlash(rel))
			if !bytes.Equal(licence, apache) {
				t.Errorf("%s: the canyon CLI links %s, whose licence (%s) is not Apache-2.0", platform, rel, at)
			}
		}
	}
}

// nearestLicence is the first LICENSE file found walking up from dir, a path in module, to its root, with that
// file's path in module.
func nearestLicence(module fs.FS, dir string) ([]byte, string) {
	for {
		at := path.Join(dir, "LICENSE")
		if b, err := fs.ReadFile(module, at); err == nil {
			return b, at
		}
		if dir == "." {
			return nil, "no LICENSE"
		}
		dir = path.Dir(dir)
	}
}
