package gate

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// A Flask and Ruby package with a C helper, under the defaults: what no running tool's rules read is
// named per rule and language, the unpinned requirement trivy cannot read is counted, the shrinkwrap it
// never reads is named, what the AI review checks in their place is named as checked, and the AI review,
// shown every file of them, names nothing (contracts/finding.md, What nothing read is named).
func TestCoverageNamesWhatNothingRead(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"app.py":                             "from flask import Flask\napp = Flask(__name__)\n",
		"lib/users.rb":                       "class Users\nend\n",
		"lib/external/client.rb":             "class Client\nend\n",            // Linguist calls external/ vendored; it is the app's
		"lib/legacy.rb":                      "# caf\xe9\nclass Legacy\nend\n", // Latin-1, which the AI review is never shown
		"config/.env.rb":                     "X = 1\n",                        // nor a .env* file
		"native/hash.c":                      "int main(void) { return 0; }\n",
		"requirements.txt":                   "# the web app\nflask>=2\nrequests==2.32.3\n-r dev.txt\n",
		"web/npm-shrinkwrap.json":            `{"lockfileVersion": 3}`,
		"node_modules/x/npm-shrinkwrap.json": `{"lockfileVersion": 3}`, // vendored: not the app's
		"README.md":                          "hello\n",
		"Dockerfile":                         "FROM scratch\n", // a build file, never an application language
		"run.sh":                             "echo hi\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := Coverage(tree, snap, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range notes {
		got = append(got, n.Level+" "+n.Message.Text)
	}
	want := []string{
		"note checked by the AI review: sql.string-concat: C (1 file) — no scanner rule reads C",
		"note checked by the AI review: sql.string-concat: Ruby (2 files) — no scanner rule reads Ruby",
		"warning not checked: sql.string-concat: Ruby (2 files) — no rule Canyon runs reads Ruby",
		"note checked by the AI review: code.injection: C (1 file) — no scanner rule reads C",
		"note checked by the AI review: code.injection: Ruby (2 files) — no scanner rule reads Ruby",
		"warning not checked: code.injection: Ruby (2 files) — no rule Canyon runs reads Ruby",
		"warning not checked: deps.known-vuln: 1 unpinned requirement in requirements.txt — trivy reads == pins only",
		"warning not checked: deps.known-vuln: web/npm-shrinkwrap.json — trivy does not read npm-shrinkwrap.json",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("notes:\n%q\nwant:\n%q", got, want)
	}
	named(t, snap, notes)
	// With its AI twin off, what no scanner rule reads is not checked.
	for i, r := range snap.Rules {
		if r.ID == "ai.sql-string-concat" {
			snap.Rules[i].State = policy.Off
		}
	}
	if notes, err = Coverage(tree, snap, true); err != nil || notes[1].Message.Text != "not checked: sql.string-concat: Ruby (4 files) — no rule Canyon runs reads Ruby" {
		t.Fatalf("twin off: %+v %v", notes, err)
	}
	// On a host with no model the AI review read nothing: one line says so, whatever the languages.
	if notes, err = Coverage(tree, snap, false); err != nil || notes[len(notes)-1].Message.Text != "not checked: auth.unauthenticated-route: no model is configured on this Canyon host, so the AI review read nothing" {
		t.Fatalf("no model: %+v %v", notes, err)
	}
	named(t, snap, notes)
}

// named fails unless each line carries its rule, the snapshot's name for it and the words after the
// rule, so a surface names the policy and keeps the reason whole (contracts/finding.md; #1668).
func named(t *testing.T, snap policy.Snapshot, notes []finding.Notification) {
	t.Helper()
	for _, n := range notes {
		i := -1
		if n.AssociatedRule != nil {
			i = slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == n.AssociatedRule.ID })
		}
		if i < 0 || n.Properties == nil || n.Properties.Canyon.PolicyName != snap.Rules[i].Name || n.Properties.Canyon.Why == "" ||
			!strings.HasSuffix(n.Message.Text, ": "+n.AssociatedRule.ID+": "+n.Properties.Canyon.Why) {
			t.Fatalf("not named: %+v", n)
		}
	}
}

// A tool, or a rule it fills, Off before preview and placed before production is on: the rule is read
// for the languages the tool's rules read, and its gaps named, as under any state it runs in
// (contracts/snapshot.md, Two gates).
func TestCoverageReadsAToolPlacedBeforeProductionAlone(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"lib/users.rb": "class Users\nend\n", "requirements.txt": "flask>=2\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range snap.Rules {
		if r.ID == "sarif.opengrep" || r.ID == "sarif.eslint" || r.ID == "sarif.trivy" || r.ID == "sql.string-concat" {
			snap.Rules[i].State, snap.Rules[i].Production = policy.Off, policy.Blocking
		}
	}
	notes, err := Coverage(tree, snap, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range notes {
		got = append(got, n.Message.Text)
	}
	for _, want := range []string{"checked by the AI review: sql.string-concat: Ruby (1 file) — no scanner rule reads Ruby",
		"not checked: deps.known-vuln: 1 unpinned requirement in requirements.txt — trivy reads == pins only"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
}

// A tool Canyon runs that had no file of the package to read says why, never a clean line
// (contracts/finding.md; #1742): ESLint on Ruby alone, Opengrep too, whose rules read no Ruby; one
// JavaScript file gives both something to read.
func TestReadNothingNamesAToolWithNoFileToRead(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	ruby := map[string]string{"app/models/user.rb": "class User\nend\n", "README.md": "hello\n", "bin/setup": "#!/bin/sh\necho hi\n"}
	for _, c := range []struct {
		files map[string]string
		want  map[string]string
	}{
		{ruby, map[string]string{"eslint": "no JavaScript or TypeScript here", "opengrep": "no language its rules read is here"}},
		{map[string]string{"app/models/user.rb": ruby["app/models/user.rb"], "app/javascript/app.js": "console.log(1)\n"}, map[string]string{}},
	} {
		dir := t.TempDir()
		for name, body := range c.files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tree, err := Walk(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ReadNothing(tree, snap); err != nil || !maps.Equal(got, c.want) {
			t.Fatalf("%v: got %v %v, want %v", slices.Sorted(maps.Keys(c.files)), got, err, c.want)
		}
	}
	// An org's rules repo may read Ruby: Canyon does not read its rules' languages, so Opengrep says nothing.
	for i, r := range snap.Rules {
		if r.ID == "sarif.opengrep" {
			snap.Rules[i].Settings.Rules = &policy.OpengrepRules{Repo: "https://example.test/org/rules.git", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user.rb"), []byte(ruby["app/models/user.rb"]), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ReadNothing(tree, snap); err != nil || !maps.Equal(got, map[string]string{"eslint": "no JavaScript or TypeScript here"}) {
		t.Fatalf("with an org's rules: %v %v", got, err)
	}
}
