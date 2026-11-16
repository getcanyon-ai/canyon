package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// platformPackage is a package platform.known-vuln counts, with each advisory at high or above against it
// and what fixes it (contracts/routes.md, The promote receipt); to a contributor also the layers that hold it and
// whether it is Go's standard library (#2579).
type platformPackage struct {
	Name       string             `json:"name"`
	Version    string             `json:"version"`
	Advisories []platformAdvisory `json:"advisories"`
	GoStdlib   bool               `json:"go_stdlib,omitempty"`
	Layers     []packageLayer     `json:"layers,omitempty"`
}

type platformAdvisory struct {
	ID    string `json:"id"`
	Fixed string `json:"fixed"`
}

// packageLayer is a layer holding a counted package: its owner, and the package's class there.
type packageLayer struct {
	Owner string `json:"owner"`
	Class string `json:"class"`
}

// What a counted package waits for, by its layers' classes (contracts/finding.md, System libraries and build tools): newer system
// libraries for the os class's, a newer buildpack or lifecycle for the rest; "" where the receipt names no layer
// (below contributor). While preview's patch is planned the system libraries Canyon uses now fix the os class's, so
// Canyon is trying that fix in preview (#2594). The dashboard says the same (design/specs/screens.md).
const (
	waitsOS    = "no fix from Canyon yet"
	patchingOS = "Canyon is trying a fix in preview"
	waitsTools = "waits for Canyon's next build tools update"
)

// fixes are what fix p, the system libraries' first, each once; patching is preview's patch planned.
func (p platformPackage) fixes(patching bool) []string {
	var out []string
	if slices.ContainsFunc(p.Layers, func(l packageLayer) bool { return l.Class == "os" }) {
		out = append(out, map[bool]string{false: waitsOS, true: patchingOS}[patching])
	}
	if slices.ContainsFunc(p.Layers, func(l packageLayer) bool { return l.Class != "os" }) {
		out = append(out, waitsTools)
	}
	return out
}

// withPatch is r as preview's patch state reads it — preview's image is the one the receipt reads: while its patch is
// planned, platform.known-vuln's row says Canyon is trying a fix of its os packages (#2594). Every other state
// leaves r as it is: once the patch is in preview the receipt reads the patched image, and what it counts still waits.
func (r receipt) withPatch(preview *env) receipt {
	if preview == nil || preview.Patch == nil || preview.Patch.State != "planned" {
		return r
	}
	r.Rules = slices.Clone(r.Rules)
	for i := range r.Rules {
		r.Rules[i].patching = true
	}
	return r
}

// label is the package as a person reads it: Go's standard library by that name.
func (p platformPackage) label() string {
	if p.GoStdlib {
		return "Go's standard library"
	}
	return p.Name
}

// shownPackages is how many counted packages the human checklist names before it points to the rest.
const shownPackages = 3

// packageLines are the human checklist's lines under platform.known-vuln's row: its first packages, by what fixes
// them, the system libraries' first and each fix said once above its own (#2579), each with its first advisory and
// what fixes it, then how to read every one (cli.md §6.1).
func (a *app) packageLines(r receiptRule) {
	packages := slices.Clone(r.Packages)
	slices.SortStableFunc(packages, func(x, y platformPackage) int { return cmp.Compare(firstFix(x), firstFix(y)) })
	said := ""
	for i, p := range packages {
		if i == shownPackages {
			a.printf("  %s\n", a.faint(fmt.Sprintf("    and %d more: canyon status --format ai lists every one", len(packages)-i)))
			return
		}
		if fixes := p.fixes(r.patching); len(fixes) > 0 && fixes[0] != said {
			said = fixes[0]
			a.printf("  %s\n", a.faint("    "+said+":"))
		}
		first := p.Advisories[0]
		line := fmt.Sprintf("%s %s %s %s fixed in %s", p.label(), p.Version, a.dot(), first.ID, first.Fixed)
		if more := len(p.Advisories) - 1; more > 0 {
			line += fmt.Sprintf(" %s and %d more %s", a.dot(), more, map[bool]string{true: "advisory", false: "advisories"}[more == 1])
		}
		for j, l := range wrap(cell(line), proseWidth-8) { // a package's own lines hang under its name
			a.printf("  %s\n", a.faint(map[bool]string{true: "    ", false: "      "}[j == 0]+l))
		}
	}
}

// firstFix orders packages by what fixes them: the system libraries', then the rest, then those the receipt names
// no layer of.
func firstFix(p platformPackage) int {
	switch fixes := p.fixes(false); {
	case len(fixes) == 0:
		return 2
	case fixes[0] == waitsOS:
		return 0
	}
	return 1
}

// aiPackages is each package platform.known-vuln counts, every advisory with what fixes it, then what it waits
// for, one line each under its row (cli.md §6.1.2).
func (a *app) aiPackages(r receiptRule) {
	for _, p := range r.Packages {
		advisories := make([]string, 0, len(p.Advisories))
		for _, adv := range p.Advisories {
			advisories = append(advisories, adv.ID+" fixed in "+adv.Fixed)
		}
		line := fmt.Sprintf("%s %s · %s", p.label(), p.Version, strings.Join(advisories, "; "))
		for _, fix := range p.fixes(r.patching) {
			line += " · " + fix
		}
		a.printf("    package: %s\n", cell(line))
	}
}
