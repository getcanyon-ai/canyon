package main

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The docs site's "Who can do what" page has a column for every org role role() answers and every app
// level identity/level.go ranks: a role added to either is red here until the page says what it may do.
func TestWhoCanDoWhatHasAColumnForEveryRoleAndLevel(t *testing.T) {
	page, err := os.ReadFile("../../docs/src/content/docs/org/who-can-do-what.md")
	if err != nil {
		t.Fatal(err)
	}
	var header string
	for line := range strings.SplitSeq(string(page), "\n") {
		if strings.HasPrefix(line, "| Action |") {
			header = strings.ToLower(line)
			break
		}
	}
	if header == "" {
		t.Fatal("the page has no table headed | Action |")
	}
	names := append(stringsIn(t, "../../identity/level.go", func(n ast.Node) ast.Node { // rank's keys: its values are numbers
		if spec, ok := n.(*ast.ValueSpec); ok && len(spec.Names) == 1 && spec.Names[0].Name == "rank" {
			return spec.Values[0]
		}
		return nil
	}), stringsIn(t, "session.go", func(n ast.Node) ast.Node { // what role returns
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "role" {
			return fn.Body
		}
		return nil
	})...)
	if len(names) < 5 {
		t.Fatalf("read %v from level.go and session.go: want the three levels and the two roles", names)
	}
	for _, name := range names {
		if !strings.Contains(header, name) {
			t.Errorf("who-can-do-what.md has no column for %q: %s", name, header)
		}
	}
}

// stringsIn is the string literals under the node pick finds in file.
func stringsIn(t *testing.T, file string, pick func(ast.Node) ast.Node) []string {
	t.Helper()
	f, err := parser.ParseFile(gotoken.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		root := pick(n)
		if root == nil {
			return true
		}
		ast.Inspect(root, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
				s, _ := strconv.Unquote(lit.Value)
				out = append(out, s)
			}
			return true
		})
		return false
	})
	return out
}
