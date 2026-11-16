package main

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"reflect"
	"strconv"
	"testing"
)

// The server refuses a secret name exactly where the Docker executor would refuse it (#2490), from a second copy
// of the executor's unexported list: this reads executor/executor.go with go/parser and holds the two equal — the
// names of reserved, each with its child flag, in order, and the prefixes isReserved passes to prefixed, PACK_ under
// build && alone (internal/tools/testsel lists this package as a reader of executor's source).
func TestSecretNamesRefusedAreTheDockerExecutors(t *testing.T) {
	file, err := parser.ParseFile(gotoken.NewFileSet(), "../../executor/executor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []dockerName
	var prefixes []string
	built := map[string]bool{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				v, ok := spec.(*ast.ValueSpec)
				if !ok || len(v.Names) != 1 || v.Names[0].Name != "reserved" || len(v.Values) != 1 {
					continue
				}
				list, ok := v.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatal("executor's reserved is not a composite literal")
				}
				for _, elt := range list.Elts {
					pair, ok := elt.(*ast.CompositeLit)
					if !ok || len(pair.Elts) != 2 {
						t.Fatalf("an entry of executor's reserved is not {name, child}: %#v", elt)
					}
					lit, ok1 := pair.Elts[0].(*ast.BasicLit)
					flag, ok2 := pair.Elts[1].(*ast.Ident)
					name, err := strconv.Unquote(lit.Value)
					if !ok1 || !ok2 || err != nil || flag.Name != "true" && flag.Name != "false" {
						t.Fatalf("an entry of executor's reserved is not {\"NAME\", true|false}: %#v", pair)
					}
					names = append(names, dockerName{name, flag.Name == "true"})
				}
			}
		case *ast.FuncDecl:
			if d.Name.Name != "isReserved" {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if and, ok := n.(*ast.BinaryExpr); ok && and.Op == gotoken.LAND {
					if build, ok := and.X.(*ast.Ident); ok && build.Name == "build" {
						if p := prefixArg(and.Y); p != "" {
							built[p] = true
						}
					}
				}
				if p := prefixArg(n); p != "" {
					prefixes = append(prefixes, p)
				}
				return true
			})
		}
	}
	if len(names) == 0 || !reflect.DeepEqual(names, dockerReserved) {
		t.Errorf("the server's dockerReserved is not the executor's reserved:\n server   %v\n executor %v", dockerReserved, names)
	}
	var ours []string
	for _, p := range dockerPrefixes {
		ours = append(ours, p.prefix)
		if p.build != built[p.prefix] {
			t.Errorf("the executor refuses %s on a build alone: %v, the server: %v", p.prefix, built[p.prefix], p.build)
		}
	}
	if !reflect.DeepEqual(ours, prefixes) {
		t.Errorf("the server's prefixes %v are not the executor's %v", ours, prefixes)
	}
}

// prefixArg is the string literal a prefixed("…") call names, or "".
func prefixArg(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return ""
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "prefixed" {
		return ""
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}
