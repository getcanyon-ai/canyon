package gate

import (
	"bytes"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// DesignRule is the rule the scanners' run reads from the package's manifests itself, no tool filling it
// (contracts/finding.md, design.org-system).
const DesignRule = "design.org-system"

// uiFrameworks are the dependencies that make an app one with a UI: the UI libraries, and the frameworks that bring
// one without listing it (contracts/finding.md, design.org-system). An app none of whose manifests lists one, and
// with no components.json, has no UI to check.
var uiFrameworks = []string{"react", "react-dom", "preact", "vue", "svelte", "solid-js", "@angular/core", "lit", "ember-source",
	"@builder.io/qwik", "next", "nuxt", "@sveltejs/kit", "astro", "gatsby", "@remix-run/react"}

// shadcnConfig is one components.json of the package, as the shadcn CLI wrote it.
type shadcnConfig struct {
	path string
	doc  map[string]json.RawMessage
}

// manifest is one package.json of the package as design.org-system reads it.
type manifest struct {
	path string
	src  []byte
	deps map[string]bool // the keys of its dependencies and devDependencies
}

// Design is design.org-system over tree: while the rule is on and the snapshot carries the org's default
// design system, the result of an app none of whose manifests declares it, and the notes naming what it
// could not read (contracts/finding.md, design.org-system).
func Design(tree Tree, snap policy.Snapshot) ([]finding.Result, []finding.Notification, error) {
	i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == DesignRule })
	ds := snap.Capabilities["design_systems"].OrgDefault
	if i < 0 || !snap.Rules[i].On() || ds == nil {
		return nil, nil, nil
	}
	rule := snap.Rules[i]
	var notes []finding.Notification
	note := func(why string) {
		notes = append(notes, finding.Notification{Level: "warning", Message: finding.Message{Text: "not checked: " + DesignRule + ": " + why},
			AssociatedRule: &finding.RuleRef{ID: DesignRule}, Properties: &finding.NoteProperties{Canyon: finding.Covered{PolicyName: rule.Name, Why: why}}})
	}
	var manifests []manifest
	var components []shadcnConfig
	found := false
	for _, f := range tree.Files() {
		base := path.Base(f)
		if vendored(f) || base != "package.json" && base != "components.json" {
			continue
		}
		b, err := tree.Read(f)
		if err != nil {
			return nil, nil, err
		}
		var doc map[string]json.RawMessage
		valid := json.Unmarshal(b, &doc) == nil && doc != nil
		if base == "components.json" {
			if valid {
				components = append(components, shadcnConfig{f, doc})
			}
			continue
		}
		found = true
		if !valid {
			note(f + " is not valid JSON")
			continue
		}
		m := manifest{path: f, src: b, deps: map[string]bool{}}
		for _, key := range []string{"dependencies", "devDependencies"} {
			var deps map[string]json.RawMessage
			if json.Unmarshal(doc[key], &deps) == nil {
				for name := range deps {
					m.deps[name] = true
				}
			}
		}
		manifests = append(manifests, m)
	}
	if !found {
		note("no package.json in this app; Canyon reads the design system from package.json")
	}
	if len(manifests) == 0 || uses(ds.Namespace, manifests, components) {
		return nil, notes, nil
	}
	ui := func(m manifest) bool {
		return slices.ContainsFunc(uiFrameworks, func(name string) bool { return m.deps[name] })
	}
	if len(components) == 0 && !slices.ContainsFunc(manifests, ui) {
		why := "no package.json in this app lists a UI framework, so there was no UI to check"
		return nil, append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: DesignRule + ": " + why}, // never "not checked": nothing was left unread
			AssociatedRule: &finding.RuleRef{ID: DesignRule}, Properties: &finding.NoteProperties{Canyon: finding.Covered{PolicyName: rule.Name, Why: why}}}), nil
	}
	res, err := designFinding(ds, rule, manifests, components)
	if err != nil {
		return nil, nil, err
	}
	return []finding.Result{res}, notes, nil
}

// uses is whether the app declares the design system: the two Canyon ships in a components.json, as the
// shadcn CLI records them, any other in a manifest's dependencies (contracts/finding.md).
func uses(namespace string, manifests []manifest, components []shadcnConfig) bool {
	switch namespace {
	case "@shadcn/ui":
		return len(components) > 0
	case "@canyon/ui":
		return slices.ContainsFunc(components, func(c shadcnConfig) bool {
			var registries map[string]json.RawMessage
			return json.Unmarshal(c.doc["registries"], &registries) == nil && registries["@canyon"] != nil
		})
	}
	return slices.ContainsFunc(manifests, func(m manifest) bool { return m.deps[namespace] })
}

// designFinding is the one result of an app that does not use ds, on the manifest nearest the package
// root: its top-level "dependencies" key, else its opening brace. Its problem names every manifest read,
// and under Canyon's own system every components.json after them.
func designFinding(ds *policy.DesignSystem, rule policy.Rule, manifests []manifest, components []shadcnConfig) (finding.Result, error) {
	var read []string
	for _, m := range manifests {
		read = append(read, m.path)
	}
	at := slices.MinFunc(manifests, func(a, b manifest) int { return strings.Count(a.path, "/") - strings.Count(b.path, "/") }) // the first of the nearest: manifests are in path order
	problem := "The org's design system is " + ds.Name + " (" + ds.Namespace + "), and no package.json in this app lists it."
	fix := "Add " + ds.Namespace + " to the dependencies of the package.json that holds the UI (registry: " + ds.RegistryURL + "), and build the UI from its components."
	end := "The app's UI is built from " + ds.Name + "'s components, and its package.json lists " + ds.Namespace + "."
	switch ds.Namespace {
	case "@shadcn/ui":
		problem = "The org's design system is " + ds.Name + " (" + ds.Namespace + "), and this app has no components.json, which the shadcn CLI writes."
		fix = "Run npx shadcn@latest init where the UI's package.json is, then add components with npx shadcn@latest add <component>."
		end = "The app's UI is built from " + ds.Name + "'s components, and its components.json records it."
	case "@canyon/ui":
		problem = "The org's design system is " + ds.Name + " (" + ds.Namespace + "), and no components.json in this app lists the @canyon registry."
		for _, c := range components {
			read = append(read, c.path)
		}
		fix = `Add "@canyon": "` + ds.RegistryURL + `/{name}.json" to registries in the UI's components.json, then add components with npx shadcn@latest add @canyon/<component>.`
		end = "The app's UI is built from " + ds.Name + "'s components, and its components.json records it."
	}
	return finding.New(finding.Input{Rule: finding.Rule{ID: rule.ID, Name: rule.Name}, Severity: finding.High,
		Title: "The app doesn't use " + ds.Name + ", the org's design system", Problem: problem + " Canyon read " + listed(read) + ".",
		Fix: fix, RequiredEndState: end,
		DoNot:    []string{"list " + ds.Namespace + " without using its components", "copy " + ds.Name + "'s components in by hand"},
		Location: dependenciesKey(at)})
}

// dependenciesKey is the span of m's top-level "dependencies" key, quotes included, else of its opening
// brace, as the JSON decoder reads them.
func dependenciesKey(m manifest) finding.Span {
	dec := json.NewDecoder(bytes.NewReader(m.src))
	_, _ = dec.Token() // the opening brace: m parsed as an object
	start, end := int(dec.InputOffset())-1, int(dec.InputOffset())
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		if key == "dependencies" {
			end = int(dec.InputOffset())
			start = bytes.LastIndexByte(m.src[:end-1], '"') // a key that decodes to dependencies holds no quote
			break
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
	}
	lineStart := bytes.LastIndexByte(m.src[:start], '\n') + 1
	lineEnd := len(m.src)
	if n := bytes.IndexByte(m.src[start:], '\n'); n >= 0 {
		lineEnd = start + n
	}
	n := bytes.Count(m.src[:start], []byte("\n")) + 1
	col := len([]rune(string(m.src[lineStart:start]))) + 1
	return finding.Span{URI: m.path, StartLine: n, StartColumn: col, EndLine: n, EndColumn: col + len([]rune(string(m.src[start:end]))),
		Lines: []string{strings.TrimSuffix(string(m.src[lineStart:lineEnd]), "\r")}}
}

// listed is paths as a sentence names them: "a", "a and b", "a, b and c".
func listed(paths []string) string {
	if len(paths) == 1 {
		return paths[0]
	}
	return strings.Join(paths[:len(paths)-1], ", ") + " and " + paths[len(paths)-1]
}
