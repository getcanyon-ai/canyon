package opengrep

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shipped are vendor.go's folders, which a test cannot import (it builds alone), each with the rule
// files it had at v2.10.1.
var shipped = map[string]int{"python": 68, "go": 27, "java": 55, "javascript": 11, "csharp": 22, "scala": 87,
	"rules/lgpl/javascript": 83, "rules/lgpl/java": 4, "rules/lgpl/kotlin": 58}

// The bundle is its licences and every shipped folder's rules, and nothing else: a folder dropped,
// one added, a licence missing or an archive cut short fails here, without Docker.
func TestTheBundleShipsItsRulesAndLicences(t *testing.T) {
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(Bundle))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	for _, licence := range []string{"LICENSE", "rules/lgpl/LICENSE", "COPYING.GPL-3.0", "LICENSE.Apache-2.0"} {
		if !names[licence] {
			t.Errorf("the bundle lacks %s", licence)
		}
		delete(names, licence)
	}
	rules, err := Rules()
	if err != nil {
		t.Fatal(err)
	}
	counted := map[string]int{}
	for name := range names {
		folder := ""
		for f := range shipped {
			if strings.HasPrefix(name, f+"/") && len(f) > len(folder) {
				folder = f
			}
		}
		if ext := path.Ext(name); folder == "" || ext != ".yml" && ext != ".yaml" || rules[name] == nil {
			t.Errorf("%s is no rule of a shipped folder", name)
		}
		counted[folder]++
	}
	for f, n := range shipped {
		if counted[f] != n {
			t.Errorf("%s ships %d rules, want %d", f, counted[f], n)
		}
	}
}

// Every probe's expected rule is one the bundle carries, so the probe test reads a real id.
func TestEveryProbeExpectsARuleTheBundleCarries(t *testing.T) {
	rules, err := Rules()
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	for _, b := range rules {
		all.Write(b)
	}
	expects, err := filepath.Glob("testdata/probes/*/expect")
	if err != nil || len(expects) == 0 {
		t.Fatalf("no probes: %v", err)
	}
	for _, f := range expects {
		want, err := os.ReadFile(f) //nolint:gosec // test: a probe file the glob above found
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`(?m)^\s*- id: "?` + regexp.QuoteMeta(strings.TrimSpace(string(want))) + `"?$`).Match(all.Bytes()) {
			t.Errorf("%s: no rule %q in the bundle", f, want)
		}
	}
}
