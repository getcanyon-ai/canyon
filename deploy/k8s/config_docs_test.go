package k8s_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// configuration is the docs site's configuration reference: every CANYON_ variable an operator sets
// on the server or an executor, or that Canyon sets for an app.
var configuration = filepath.Join("..", "..", "docs", "src", "content", "docs", "self-host", "configuration.md")

// TestEveryVariableIsInTheConfigurationReference holds the reference to the code: every CANYON_ name
// these packages spell out (read with os.Getenv or a getenv parameter, listed in a table of images, set
// in an app's environment) is on the page, so a new setting can't ship unwritten. A name built at run
// time ("CANYON_IMAGE_"+tool) is the page's to spell out; a prefix alone is no name.
func TestEveryVariableIsInTheConfigurationReference(t *testing.T) {
	page, err := os.ReadFile(configuration) //nolint:gosec // a fixed path in this repository
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"cmd/canyon-server", "cmd/canyon-executor", "executor", "cmd/canyon-executor-kube", "executor/kube"} {
		files, err := filepath.Glob(filepath.Join("..", "..", dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil || !isVariable(name) {
					return true
				}
				if !strings.Contains(string(page), "`"+name+"`") {
					t.Errorf("%s names %s, which the configuration reference does not list", f, name)
				}
				return true
			})
		}
	}
}

// isVariable is a whole CANYON_ variable name: upper-case letters, digits and underscores, not ending in
// one (a prefix).
func isVariable(s string) bool {
	rest, ok := strings.CutPrefix(s, "CANYON_")
	if !ok || rest == "" || strings.HasSuffix(rest, "_") {
		return false
	}
	return strings.Trim(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == ""
}
