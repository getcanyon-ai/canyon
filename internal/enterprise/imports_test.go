package enterprise

import (
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/getcanyon-ai/canyon"

// TestCoreImportsNoEnterprise holds the core to design/ARCHITECTURE.md §8: no Go file outside ee/, tests included,
// imports a package under ee/ unless its own //go:build constraint requires !noee, so a build without Canyon
// Enterprise (-tags noee) never compiles it, whatever other tags, GOOS or GOARCH that build has. Every file is read
// here, in this process, so Go's test cache sees each one and a new import is never answered from the cache.
func TestCoreImportsNoEnterprise(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if mod, err := fs.ReadFile(os.DirFS(root), "go.mod"); err != nil || !strings.HasPrefix(string(mod), "module "+module+"\n") {
		t.Fatalf("%s/go.mod is not module %s (%v)", root, module, err)
	}
	read := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		name := d.Name()
		if d.IsDir() {
			if rel == "ee" || path != root && (name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir // as go build ./... skips them; ee/ is Canyon Enterprise's own
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		read++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		var expr constraint.Expr
		for _, g := range f.Comments {
			if g.Pos() >= f.Package {
				break
			}
			for _, c := range g.List {
				if constraint.IsGoBuild(c.Text) {
					if expr, err = constraint.Parse(c.Text); err != nil {
						return err
					}
				}
			}
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if (p == module+"/ee" || strings.HasPrefix(p, module+"/ee/")) && !requiresNoNoee(expr) {
				t.Errorf("%s imports %s outside //go:build !noee: the core never imports Canyon Enterprise's code (design/ARCHITECTURE.md §8)", filepath.ToSlash(rel), p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read < 500 {
		t.Fatalf("read %d Go files under %s, want the module's every one", read, root)
	}
}

// requiresNoNoee is whether expr is false whenever noee is set, whatever the file's other tags are: every
// assignment of them is tried. A file with no constraint builds everywhere.
func requiresNoNoee(expr constraint.Expr) bool {
	if expr == nil {
		return false
	}
	var tags []string
	expr.Eval(func(tag string) bool {
		if tag != "noee" {
			tags = append(tags, tag)
		}
		return false
	})
	for set := range 1 << len(tags) {
		if expr.Eval(func(tag string) bool {
			for i, t := range tags {
				if t == tag {
					return set&(1<<i) != 0
				}
			}
			return tag == "noee"
		}) {
			return false
		}
	}
	return true
}

// The constraint judge: a file is left out of a build without Canyon Enterprise only when noee rules it out under
// every other tag.
func TestRequiresNoNoee(t *testing.T) {
	for line, want := range map[string]bool{
		"//go:build !noee":                   true,
		"//go:build !noee && e2e":            true,
		"//go:build !noee && !linux":         true,
		"//go:build e2e":                     false,
		"//go:build linux":                   false,
		"//go:build !noee || e2e":            false,
		"//go:build (!noee && e2e) || linux": false,
		"//go:build noee":                    false,
	} {
		expr, err := constraint.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		if got := requiresNoNoee(expr); got != want {
			t.Errorf("%s: %t, want %t", line, got, want)
		}
	}
	if requiresNoNoee(nil) {
		t.Error("no constraint: a file every build compiles")
	}
}
