package trivy

import (
	"slices"
	"strings"
	"testing"
)

const (
	base = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	top  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// component is a stored component of layer, as trivy writes it.
func component(ref, typ, name, purl, layer string) Component {
	c := Component{BOMRef: ref, Type: typ, Name: name, PURL: purl}
	if layer != "" {
		c.Properties = append(c.Properties, struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}{LayerDiffID, layer})
	}
	return c
}

// An image's diff IDs read whole only as sha256: and 64 lowercase hex, each named once.
func TestDiffIDsRead(t *testing.T) {
	for ids, want := range map[string]bool{
		base + " " + top:                      true,
		"":                                    true,
		base + " " + base:                     false,
		strings.ToUpper(base[7:]) + " " + top: false,
		"sha512:00 " + top:                    false,
	} {
		if got := DiffIDsRead(strings.Fields(ids)); got != want {
			t.Errorf("%q: %v, want %v", ids, got, want)
		}
	}
}

// A stored list of components reads only as a list whose every element carries its bom-ref, type and
// name; a stored report only as trivy's, every result and vulnerability whole.
func TestStoredRowsReadWhole(t *testing.T) {
	for raw, ok := range map[string]bool{
		`[]`: true,
		`[{"bom-ref":"pkg:npm/a@1","type":"library","name":"a","properties":[{"name":"` + LayerDiffID + `","value":"` + base + `"}]}]`: true,
		`null`: false, `{}`: false, `[null]`: false,
		`[{"type":"library","name":"a"}]`:    false,
		`[{"bom-ref":"x","name":"a"}]`:       false,
		`[{"bom-ref":"x","type":"library"}]`: false,
	} {
		if _, got := DecodeComponents([]byte(raw)); got != ok {
			t.Errorf("components %s: %v, want %v", raw, got, ok)
		}
	}
	vuln := `{"VulnerabilityID":"CVE-1","PkgIdentifier":{"BOMRef":"x"},"Layer":{"DiffID":"` + base + `"}}`
	for raw, ok := range map[string]bool{
		`{"SchemaVersion":2,"Results":[]}`:                                   true,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[` + vuln + `]}]}`: true,
		`{"SchemaVersion":1,"Results":[]}`:                                   false,
		`{"SchemaVersion":2}`:                                                false,
		`{"SchemaVersion":2,"Results":[null]}`:                               false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[null]}]}`:         false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgIdentifier":{"BOMRef":"x"}}]}]}`: false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"PkgIdentifier":{"BOMRef":"x"},"Layer":{"DiffID":"x"}}]}]}`:    false,
		`null`: false,
	} {
		if _, got := DecodeReport([]byte(raw)); got != ok {
			t.Errorf("report %s: %v, want %v", raw, got, ok)
		}
	}
}

// A layer's stored components are its own only when each names it; the packages are the library components
// classed by their layer's owner, and a re-match covers the layers holding one of a platform class, and the
// app's own too for a project whose deps.known-vuln reads the built app.
func TestPackagesAndTheLayersARematchCovers(t *testing.T) {
	libssl := component("pkg:deb/ubuntu/libssl3@3", "library", "libssl3", "pkg:deb/ubuntu/libssl3@3", base)
	express := component("pkg:npm/express@4", "library", "express", "pkg:npm/express@4", top)
	app := component("x", "application", "package-lock.json", "", top)
	if !StoredIn([]Component{libssl}, base) || StoredIn([]Component{libssl}, top) || StoredIn([]Component{component("y", "library", "y", "", "")}, base) {
		t.Fatal("a component stored under another layer, or none, reads as the layer's own")
	}
	layers := map[string][]Component{base: {libssl}, top: {express, app}}
	pkgs := Packages([]string{base, top}, map[string]string{base: RunImage, top: App}, layers)
	if len(pkgs) != 2 || pkgs[0].Class != "os" || pkgs[0].Layer != base || pkgs[1].Class != "app" {
		t.Fatalf("the packages: %+v", pkgs)
	}
	if got := Covered([]string{base, top}, pkgs, false); !slices.Equal(got, []string{base}) {
		t.Fatalf("covered: %v, want the run image's layer alone", got)
	}
	if got := Covered([]string{base, top}, pkgs, true); !slices.Equal(got, []string{base, top}) { // deps.known-vuln placed before production reads the app's layers too
		t.Fatalf("covered with the app's layers: %v, want both", got)
	}
}

// A match joins only when each vulnerability names the layer it is stored under and a package that layer
// holds, by bom-ref.
func TestAMatchJoinsItsLayersPackages(t *testing.T) {
	pkgs := []Package{{Component: component("pkg:deb/ubuntu/libssl3@3", "library", "libssl3", "", base), Layer: base}, {Component: component("pkg:npm/a@1", "library", "a", "", top), Layer: top}}
	vuln := func(ref, layer string) Report {
		v := Vuln{VulnerabilityID: "CVE-1"}
		v.PkgIdentifier.BOMRef, v.Layer.DiffID = ref, layer
		return Report{SchemaVersion: 2, Results: []Result{{Vulnerabilities: []Vuln{v}}}}
	}
	for name, c := range map[string]struct {
		r    Report
		want bool
	}{
		"its own package":         {vuln("pkg:deb/ubuntu/libssl3@3", base), true},
		"nothing matched":         {Report{SchemaVersion: 2}, true},
		"another layer's package": {vuln("pkg:npm/a@1", base), false},
		"another layer named":     {vuln("pkg:deb/ubuntu/libssl3@3", top), false},
		"no package of it":        {vuln("pkg:npm/b@1", base), false},
	} {
		if got := Joins(c.r, base, pkgs); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if n := component("x", "library", "core", "", "").FullName(); n != "core" {
		t.Errorf("a name without a group: %s", n)
	}
	if c := (Component{Group: "@babel", Name: "core"}); c.FullName() != "@babel/core" {
		t.Errorf("a grouped name: %s", c.FullName())
	}
}
