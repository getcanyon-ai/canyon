// Package auditactions holds the check that every action the server records is one the dashboard has words for (#152):
// the server keeps no one list of them, so this test reads its source.
package auditactions

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write testdata/actions.txt from the source")

// root is the repository, from this directory.
const root = "../../.."

// The calls that pass events.AuditTx an action that isn't a string literal, by file, function and argument, and every
// action each can be. A new one fails the test until it is named here, with its actions.
// action(r) is the route's own method and pattern under /api/v1 (cmd/canyon-server/audit.go).
var variable = map[string][]string{
	"cmd/canyon-server/licence.go putLicence action(r)":                 {"PUT /api/v1/licence"},
	"cmd/canyon-server/routes_cancel.go cancelRun action(r)":            {"POST /api/v1/gate-runs/{id}/cancel"},
	"cmd/canyon-server/routes_db_sessions.go insertDBSession action":    {"db session opened", "db write session opened"},
	"cmd/canyon-server/routes_design_systems.go newDesignSystem action": {"POST /api/v1/design-systems"},
	"cmd/canyon-server/routes_envs.go undeploy action(r)":               {"DELETE /api/v1/projects/{id}/envs/{env}"},
	"cmd/canyon-server/routes_gate.go startRun action(r)":               {"POST /api/v1/projects/{id}/gate-runs", "POST /api/v1/projects/{id}/deploys"},
	"cmd/canyon-server/routes_logs.go auditRead action(r)":              {"GET /api/v1/projects/{id}/logs"},
	"cmd/canyon-server/routes_packages.go uploadPackage action(r)":      {"POST /api/v1/projects/{id}/packages"},
	"cmd/canyon-server/routes_platform_repo.go replaceRepoHash action":  {"rotate fetch token", "replace platform repo host token"},
	"cmd/canyon-server/routes_playbooks.go savePlaybook action":         {"set playbook", "restore playbook"},
	"cmd/canyon-server/routes_preferences.go applyPreferences action":   {"PUT /api/v1/preferences", "complete setup", "POST /api/v1/guardrails/setup"},
	"cmd/canyon-server/routes_projects.go createProject action(r)":      {"POST /api/v1/projects"},
	"cmd/canyon-server/routes_projects.go claimProject action(r)":       {"PUT /api/v1/projects/{id}/owner"},
	"cmd/canyon-server/routes_projects.go deleteProject action(r)":      {"DELETE /api/v1/projects/{id}"},
	"cmd/canyon-server/routes_promote.go promoteRun action(r)":          {"POST /api/v1/projects/{id}/promotions"}, // the approval's own route writes approve promotion
	"cmd/canyon-server/routes_promote.go approve action(r)":             {"POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve"},
	"cmd/canyon-server/routes_promote.go approveFindings action(r)":     {"POST /api/v1/gate-runs/{run}/findings/approve"},
	"cmd/canyon-server/routes_release_checkin.go refuseCheckIn action":  {"pull request closed", "pull request changed", "release tree mismatch"},
	"cmd/canyon-server/routes_secrets.go deleteSecret action(r)":        {"DELETE /api/v1/projects/{id}/secrets/{name}"},
	"cmd/canyon-server/routes_secrets.go putSecret action(r)":           {"PUT /api/v1/projects/{id}/secrets/{name}"},
	"cmd/canyon-server/routes_sessions.go deleteSession action":         {"revoke session", "disconnect agent"},
	"cmd/canyon-server/routes_telemetry.go putTelemetry action(r)":      {"PUT /api/v1/telemetry"},
	"github-app/work.go start what":                                     {"gate run from github", "deploy from github"},
	"github-app/work.go packCommit action":                              {"package from github", "POST /api/v1/projects/{id}/packages/from-repository"},
	"runs/brake.go stopped UndeployAction":                              {"DELETE /api/v1/projects/{id}/envs/{env}"},
	`runs/pipeline.go deploy "deploy "+env`:                             {"deploy preview", "deploy prod"},
	"runs/pipeline.go recordProd action":                                {"deploy prod", "patch prod"},
	"runs/pipeline.go end action":                                       {"build failed", "detect failed", "start failed", "build had no room"},
	`runs/watch.go revert "revert patch "+env`:                          {"revert patch preview", "revert patch prod"},
}

// recorded is every action events.AuditTx is called with in the repository's Go, outside tests, and the calls it can't
// read an action off.
func recorded(t *testing.T) (actions []string, unread []string) {
	t.Helper()
	set := token.NewFileSet()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !auditTx(call.Fun, file.Name.Name) || len(call.Args) < 4 {
					return true
				}
				if lit, ok := call.Args[3].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					action, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					seen[action] = true
					return true
				}
				site := filepath.ToSlash(rel) + " " + fn.Name.Name + " " + expr(set, call.Args[3])
				names, known := variable[site]
				if !known {
					unread = append(unread, site)
				}
				for _, action := range names {
					seen[action] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for action := range seen {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	return actions, unread
}

// auditTx is whether a call's function is events.AuditTx: from any package, or within events.
func auditTx(fun ast.Expr, pkg string) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "events" && f.Sel.Name == "AuditTx"
	case *ast.Ident:
		return pkg == "events" && f.Name == "AuditTx"
	}
	return false
}

// expr is an argument as written, for naming its call.
func expr(set *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	start, end := set.Position(e.Pos()), set.Position(e.End())
	src, err := os.ReadFile(start.Filename)
	if err != nil || end.Offset > len(src) {
		return "?"
	}
	b.Write(src[start.Offset:end.Offset])
	return b.String()
}

func TestEveryRecordedActionIsListed(t *testing.T) {
	actions, unread := recorded(t)
	for _, site := range unread {
		t.Errorf("events.AuditTx is called with an action this test can't read: %s. Name it in variable, with every action it can be", site)
	}
	got := strings.Join(actions, "\n") + "\n"
	if *update {
		if err := os.WriteFile("testdata/actions.txt", []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/actions.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		listed := strings.Split(strings.TrimSpace(string(want)), "\n")
		var added, gone []string
		for _, a := range actions {
			if !slices.Contains(listed, a) {
				added = append(added, a)
			}
		}
		for _, a := range listed {
			if !slices.Contains(actions, a) {
				gone = append(gone, a)
			}
		}
		t.Errorf("testdata/actions.txt isn't the actions the source records: new %q, gone %q. Run go test ./internal/tools/auditactions -update, and give each new one words in web/src/strings.ts", added, gone)
	}
}
