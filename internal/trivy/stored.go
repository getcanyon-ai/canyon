package trivy

import (
	"encoding/json"
	"regexp"
	"slices"
)

// LayerDiffID is the property trivy writes on each component: the diff ID of the layer that holds it.
const LayerDiffID = "aquasecurity:trivy:LayerDiffID"

// Component is a CycloneDX component as trivy's image SBOM writes it, and as Canyon stores it per layer
// (contracts/finding.md, System libraries and build tools).
type Component struct {
	BOMRef     string `json:"bom-ref"`
	Type       string `json:"type"`
	Group      string `json:"group"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	PURL       string `json:"purl"`
	Properties []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"properties"`
}

func (c Component) Prop(name string) string {
	for _, p := range c.Properties {
		if p.Name == name {
			return p.Value
		}
	}
	return ""
}

// FullName is the package's name as a person reads it: its group and a slash before it when it has one.
func (c Component) FullName() string {
	if c.Group != "" {
		return c.Group + "/" + c.Name
	}
	return c.Name
}

// Vuln is one vulnerability of trivy's JSON report (contracts/executor.md, target sbom).
type Vuln struct {
	VulnerabilityID string `json:"VulnerabilityID"`
	PkgIdentifier   struct {
		BOMRef string `json:"BOMRef"`
	} `json:"PkgIdentifier"`
	FixedVersion string `json:"FixedVersion"`
	Severity     string `json:"Severity"`
	PrimaryURL   string `json:"PrimaryURL"`
	Layer        struct {
		DiffID string `json:"DiffID"`
	} `json:"Layer"`
}

// Report is what Canyon reads of trivy's JSON report of a re-match (contracts/executor.md, target sbom).
type Report struct {
	SchemaVersion int      `json:"SchemaVersion"`
	Results       []Result `json:"Results"`
}

type Result struct {
	Vulnerabilities []Vuln `json:"Vulnerabilities"`
}

// storedReport is a report as decoded off the store, each element a pointer so that a null one is told
// apart from an empty one.
type storedReport struct {
	SchemaVersion int `json:"SchemaVersion"`
	Results       []*struct {
		Vulnerabilities []*Vuln `json:"Vulnerabilities"`
	} `json:"Results"`
}

// diffID is a layer's diff ID as trivy and the image config write it.
var diffID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// DiffIDsRead is whether an image's diff IDs read whole: each sha256: and 64 lowercase hex, none twice.
func DiffIDsRead(ids []string) bool {
	return len(slices.Compact(slices.Sorted(slices.Values(ids)))) == len(ids) &&
		!slices.ContainsFunc(ids, func(d string) bool { return !diffID.MatchString(d) })
}

// DecodeComponents is a stored list of components; false when it is not one, null included, or when any
// element is null or lacks its bom-ref, type or name: an element that does not read whole is never a
// component of no class that drops out of the read.
func DecodeComponents(raw []byte) ([]Component, bool) {
	var stored []*Component
	if json.Unmarshal(raw, &stored) != nil || stored == nil {
		return nil, false
	}
	out := make([]Component, 0, len(stored))
	for _, c := range stored {
		if c == nil || c.BOMRef == "" || c.Type == "" || c.Name == "" {
			return nil, false
		}
		out = append(out, *c)
	}
	return out, true
}

// DecodeReport is a stored match's report; false when it is not trivy's: SchemaVersion 2, Results a list
// (contracts/executor.md, target sbom). An empty object, null or a report with no Results never reads as a
// match that found nothing.
// Every result and vulnerability reads whole too: none null, and each vulnerability names its id, its
// component's BOMRef and its layer.
func DecodeReport(raw []byte) (Report, bool) {
	var stored storedReport
	if json.Unmarshal(raw, &stored) != nil || stored.SchemaVersion != 2 || stored.Results == nil {
		return Report{}, false
	}
	out := Report{SchemaVersion: stored.SchemaVersion, Results: make([]Result, 0, len(stored.Results))} // an empty list is a report that matched nothing
	for _, r := range stored.Results {
		if r == nil {
			return Report{}, false
		}
		var vulns []Vuln
		for _, v := range r.Vulnerabilities {
			if v == nil || v.VulnerabilityID == "" || v.PkgIdentifier.BOMRef == "" || v.Layer.DiffID == "" {
				return Report{}, false
			}
			vulns = append(vulns, *v)
		}
		out.Results = append(out.Results, Result{Vulnerabilities: vulns})
	}
	return out, true
}

// StoredIn reports whether every component stored under layer is trivy's for that layer: each carries its
// LayerDiffID, and names layer.
func StoredIn(components []Component, layer string) bool {
	return !slices.ContainsFunc(components, func(c Component) bool { return c.Prop(LayerDiffID) != layer })
}

// Package is a library component of an image's layer, with its class.
type Package struct {
	Component
	Layer, Class string
}

// Packages are the library components of each of the image's layers, bottom first, each classed by its
// layer's owner (owners as Owners answers them).
func Packages(diffIDs []string, owners map[string]string, layers map[string][]Component) []Package {
	var out []Package
	for _, d := range diffIDs {
		for _, c := range layers[d] {
			if c.Type == "library" {
				out = append(out, Package{c, d, Class(owners[d], c.Prop("aquasecurity:trivy:PkgType"), c.PURL)})
			}
		}
	}
	return out
}

// Covered are the layers a re-match covers, bottom first: each holding a package of a platform class
// (os, runtime, lifecycle or unknown), and with app each holding an app package too, as deps.known-vuln
// reads them for a project that places it before production (contracts/finding.md, The built app).
func Covered(diffIDs []string, pkgs []Package, app bool) []string {
	var out []string
	for _, d := range diffIDs {
		if slices.ContainsFunc(pkgs, func(p Package) bool { return p.Layer == d && (app || p.Class != "app") }) {
			out = append(out, d)
		}
	}
	return out
}

// Joins is whether every vulnerability of a layer's match names that layer and a package it holds, by
// BOMRef: a match that does not is never read (contracts/finding.md, The join).
func Joins(r Report, layer string, pkgs []Package) bool {
	for _, res := range r.Results {
		for _, v := range res.Vulnerabilities {
			if v.Layer.DiffID != layer || !slices.ContainsFunc(pkgs, func(p Package) bool { return p.Layer == layer && p.BOMRef == v.PkgIdentifier.BOMRef }) {
				return false
			}
		}
	}
	return true
}
