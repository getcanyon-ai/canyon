package sarif

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// ImagePackage is the package an image report's result is in, as the result names it in its properties under
// canyon.package (contracts/finding.md, An imported image result): what a tool's export maps its own fields to, since
// SARIF names no package, fixed version or layer of its own. Layer is the diff ID of the image's layer that holds it;
// PURL its package URL, by which Canyon tells a system library from another package.
type ImagePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Fixed   string `json:"fixed,omitempty"`
	Layer   string `json:"layer,omitempty"`
	PURL    string `json:"purl,omitempty"`
}

// ImageResult is one vulnerability of an image report as Canyon keeps it: its id, its severity and its package —
// names and versions, never any of the report's prose.
type ImageResult struct {
	Vulnerability string           `json:"vulnerability"`
	Severity      finding.Severity `json:"severity"`
	Package       ImagePackage     `json:"package"`
}

// ImageReport is one tool's report of an image, normalised.
type ImageReport struct {
	Tool, Category, Version string
	Results                 []ImageResult
	Dropped                 int
	FirstDropped            string
}

var layerID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NormalizeImage reads one tool's report of an image against the snapshot (contracts/finding.md, An imported image
// result): SARIF 2.1.0, one run, its tool one the org added, at most 1,000 results, as a package's report is. A result
// of kind pass, informational or notApplicable says nothing is wrong and is dropped; any other names its package
// under properties.canyon.package, else the report is refused — a vulnerability Canyon cannot place is never left out.
func NormalizeImage(raw []byte, snap *policy.Snapshot) (ImageReport, error) {
	var in report
	if err := json.Unmarshal(raw, &in); err != nil || in.Version != finding.Version {
		return ImageReport{}, Invalid{"the report is not SARIF 2.1.0 JSON"}
	}
	if len(in.Runs) != 1 {
		return ImageReport{}, Invalid{"the report is not exactly one run"}
	}
	run := in.Runs[0]
	a := attach{tool: tag(run.Tool.Driver.Name), snap: snap, rules: map[ruleKey]rule{}, extensions: map[string]int{}}
	for in, c := range slices.Backward(append([]component{run.Tool.Driver}, run.Tool.Extensions...)) {
		for _, r := range slices.Backward(c.Rules) {
			a.rules[ruleKey{in, r.ID}] = r
		}
		if in > 0 && c.Name != "" {
			a.extensions[c.Name] = in
		}
	}
	rep := ImageReport{Tool: a.tool, Category: tag(run.AutomationDetails.ID), Version: cmp.Or(run.Tool.Driver.Version, "unknown"), Results: []ImageResult{}}
	switch v := run.Tool.Driver.Version; {
	case len(a.tool) < 1 || len(a.tool) > 40:
		return ImageReport{}, Invalid{"tool.driver.name is not 1 to 40 characters once normalised"}
	case len(rep.Category) > 128:
		return ImageReport{}, Invalid{"automationDetails.id is over 128 characters once normalised"}
	case utf8.RuneCountInString(v) > 128 || finding.HasLiveKey([]byte(v)):
		return ImageReport{}, Invalid{"driver.version is over 128 characters or carries a live key"}
	case !slices.ContainsFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif."+a.tool && r.Producer == policy.Sarif }):
		return ImageReport{}, UnknownTool{a.tool}
	case len(run.Results) > 1000:
		return ImageReport{}, TooManyResults{a.tool, len(run.Results)}
	}
	for i, res := range run.Results {
		if slices.Contains([]string{"pass", "informational", "notApplicable"}, res.Kind) {
			if rep.Dropped++; rep.Dropped == 1 {
				rep.FirstDropped = fmt.Sprintf("result %d: kind is %s", i, res.Kind)
			}
			continue
		}
		if !slices.Contains([]string{"", "fail", "open", "review"}, res.Kind) {
			return ImageReport{}, Invalid{fmt.Sprintf("result %d: kind is not one SARIF 2.1.0 names", i)}
		}
		id, _, severity, err := a.rated(res)
		if err != nil {
			return ImageReport{}, Invalid{fmt.Sprintf("result %d: %s", i, err.(Invalid).Message)}
		}
		if why := packageWrong(res.Properties.Canyon.Package); why != "" {
			return ImageReport{}, Invalid{fmt.Sprintf("result %d: properties.canyon.package %s", i, why)}
		}
		rep.Results = append(rep.Results, ImageResult{Vulnerability: id, Severity: severity, Package: *res.Properties.Canyon.Package})
	}
	return rep, nil
}

// packageWrong is what is wrong with a result's package, "" for nothing: a name of 1 to 214 characters and a version of
// 1 to 128, a fixed version and a purl of at most 128 and 2,048 when named, a layer a diff ID, each plain text with
// no live key, since each is stored and shown.
func packageWrong(p *ImagePackage) string {
	plain := func(s string, most int) bool {
		return utf8.ValidString(s) && utf8.RuneCountInString(s) <= most && !finding.HasLiveKey([]byte(s)) &&
			!slices.ContainsFunc([]rune(s), func(r rune) bool { return r < ' ' || r == 0x7f })
	}
	switch {
	case p == nil:
		return "is not there: map the tool's package, version, fixed version and layer to it"
	case p.Name == "" || !plain(p.Name, 214):
		return "names no name of 1 to 214 characters"
	case p.Version == "" || !plain(p.Version, 128):
		return "names no version of 1 to 128 characters"
	case !plain(p.Fixed, 128):
		return "names a fixed version over 128 characters"
	case p.Layer != "" && !layerID.MatchString(p.Layer):
		return "names a layer that is not sha256: and 64 lowercase hex"
	case !plain(p.PURL, 2048):
		return "names a purl over 2,048 characters"
	}
	return ""
}
