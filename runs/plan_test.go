package runs

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/trivy"
)

func libssl3(severity, vuln string) PlatformFinding {
	return PlatformFinding{Name: "libssl3", Version: "3.0.13-0ubuntu3.4", Vulnerability: vuln, Fixed: "3.0.13-0ubuntu3.5", Class: "os", Severity: severity}
}

// The plan considers only counted findings at or above high, the severity at which the rule can block:
// Ubuntu's low fixes never restart an app (design/PATCHING.md §4.2). An os finding the named run image fixes
// is a rebase naming what it fixes; one it does not fix waits; one whose run image match does not read
// plans nothing, fail closed; runtime and lifecycle findings alone are Phase 3's bump.
func TestPlanLowFindingPlansNothing(t *testing.T) {
	held := map[string]bool{"libssl3": true}
	fixed := &runImageVulns{held: held, matched: map[string][]string{}, unchecked: map[string]bool{}}
	behind := &runImageVulns{held: held, matched: map[string][]string{"libssl3": {"CVE-2024-6119"}}, unchecked: map[string]bool{}}
	unread := &runImageVulns{held: held, matched: map[string][]string{}, unchecked: map[string]bool{"libssl3": true}}
	without := &runImageVulns{held: map[string]bool{"libc6": true}, matched: map[string][]string{}, unchecked: map[string]bool{}}
	npm := PlatformFinding{Name: "npm", Version: "10.8.2", Vulnerability: "CVE-2024-1", Fixed: "10.9.0", Class: "runtime", Severity: "high"}
	for _, tc := range []struct {
		name     string
		findings []PlatformFinding
		run      *runImageVulns
		want     string
		fixes    int
	}{
		{"a medium os finding", []PlatformFinding{libssl3("medium", "CVE-2024-6119")}, fixed, OutcomeNothing, 0},
		{"a low os finding", []PlatformFinding{libssl3("low", "CVE-2024-6119")}, fixed, OutcomeNothing, 0},
		{"no finding", nil, fixed, OutcomeNothing, 0},
		{"a high os finding the run image fixes", []PlatformFinding{libssl3("high", "CVE-2024-6119"), libssl3("medium", "CVE-2024-2")}, fixed, OutcomeRebase, 1},
		{"a critical os finding the run image fixes", []PlatformFinding{libssl3("critical", "CVE-2024-6119")}, fixed, OutcomeRebase, 1},
		{"a high os finding the run image still has", []PlatformFinding{libssl3("high", "CVE-2024-6119")}, behind, OutcomeWait, 0},
		{"one fixed, one not", []PlatformFinding{libssl3("high", "CVE-2024-6119"), libssl3("high", "CVE-2024-9")}, behind, OutcomeRebase, 1},
		{"a run image that could not match the package", []PlatformFinding{libssl3("high", "CVE-2024-6119")}, unread, OutcomeWait, 0},
		{"a run image without the package", []PlatformFinding{libssl3("high", "CVE-2024-6119")}, without, OutcomeWait, 0},
		{"a run image match that does not read", []PlatformFinding{libssl3("high", "CVE-2024-6119")}, nil, OutcomeUnchecked, 0},
		{"a runtime finding", []PlatformFinding{npm}, fixed, OutcomeBump, 0},
	} {
		if got, fixes := plan(tc.findings, tc.run); got != tc.want || len(fixes) != tc.fixes {
			t.Errorf("%s: %s, %d fixes; want %s, %d", tc.name, got, len(fixes), tc.want, tc.fixes)
		}
	}
}

// runImageFixture is a run image's facts: one layer holding libssl3, its newest match built at built
// naming vulns against it.
func runImageFixture(t *testing.T, built time.Time, vulns ...string) runImageFacts {
	t.Helper()
	layer := "sha256:" + strings.Repeat("1", 64)
	purl := "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.5?distro=ubuntu-24.04"
	c := trivy.Component{BOMRef: purl, Type: "library", Name: "libssl3", Version: "3.0.13-0ubuntu3.5", PURL: purl}
	c.Properties = append(c.Properties, struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{trivy.LayerDiffID, layer})
	var vs []map[string]any
	for _, v := range vulns {
		vs = append(vs, map[string]any{"VulnerabilityID": v, "PkgIdentifier": map[string]any{"BOMRef": purl}, "Layer": map[string]any{"DiffID": layer}})
	}
	report, err := json.Marshal(map[string]any{"SchemaVersion": 2, "Results": []any{map[string]any{"Vulnerabilities": vs}}})
	if err != nil {
		t.Fatal(err)
	}
	return runImageFacts{diffIDs: []string{layer}, layers: map[string][]trivy.Component{layer: {c}},
		newest: map[string]storedMatch{layer: {DBBuiltAt: built, Report: report}}, readable: true}
}

// The run image's match is read only when it is whole, fresh and read in full: a match past the 5 days or
// of no date, a later rescan that failed, a layer never matched or a match naming a package it does not hold,
// a stored row that does not read, a package trivy placed in no layer, a scan that found no os package at
// all — each is no match at all, so the plan rebases nothing onto it (design/PATCHING.md §3.2).
func TestARunImageMatchThatDoesNotReadIsNone(t *testing.T) {
	now := time.Now()
	good := runImageFixture(t, now.Add(-time.Hour), "CVE-2024-9")
	read := readRunImage(good, now)
	if read == nil || !slices.Equal(read.matched["libssl3"], []string{"CVE-2024-9"}) || !read.carries(libssl3("high", "CVE-2024-6119")) || read.carries(libssl3("high", "CVE-2024-9")) {
		t.Fatalf("a fresh match: %+v", read)
	}
	layer := good.diffIDs[0]
	for name, edit := range map[string]func(f *runImageFacts){
		"6 days old": func(f *runImageFacts) {
			m := f.newest[layer]
			m.DBBuiltAt = now.Add(-6 * 24 * time.Hour)
			f.newest[layer] = m
		},
		"from the future": func(f *runImageFacts) { m := f.newest[layer]; m.DBBuiltAt = now.Add(time.Hour); f.newest[layer] = m },
		"rescan failed":   func(f *runImageFacts) { f.failed = true },
		"never matched":   func(f *runImageFacts) { delete(f.newest, layer) },
		"a layer missing": func(f *runImageFacts) { delete(f.layers, layer) },
		"unreadable":      func(f *runImageFacts) { f.readable = false },
		"unplaced":        func(f *runImageFacts) { f.unplaced = []trivy.Component{{BOMRef: "x", Type: "library", Name: "libc6"}} },
		"no os package":   func(f *runImageFacts) { f.layers[layer] = []trivy.Component{} },
		"another layer's match": func(f *runImageFacts) {
			m := f.newest[layer]
			m.Report = json.RawMessage(`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgIdentifier":{"BOMRef":"pkg:deb/ubuntu/zlib1g@1"},"Layer":{"DiffID":"` + layer + `"}}]}]}`)
			f.newest[layer] = m
		},
	} {
		f := runImageFixture(t, now.Add(-time.Hour), "CVE-2024-9")
		f.newest, f.layers = maps.Clone(f.newest), maps.Clone(f.layers)
		edit(&f)
		if got := readRunImage(f, now); got != nil {
			t.Errorf("%s: read as %+v", name, got)
		}
	}
}
