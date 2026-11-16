package gate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

var meridian = &policy.DesignSystem{Name: "Meridian UI", Namespace: "@meridian/ui", RegistryURL: "https://registry.meridian.example/ui"}

// designScan is the scanners' run over files under the default snapshot with ds as the org's default
// and design.org-system at state: its design.org-system results and the notes that name the rule.
func designScan(t *testing.T, files map[string]string, ds *policy.DesignSystem, state policy.State) ([]finding.Result, []string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	results, notes, _ := designScanDir(t, dir, ds, state)
	return results, notes
}

// designScanDir is designScan over the app in dir, with the run's whole log.
func designScanDir(t *testing.T, dir string, ds *policy.DesignSystem, state policy.State) ([]finding.Result, []string, finding.Log) {
	t.Helper()
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, err = policy.WithStates(snap, map[string]policy.State{"design.org-system": state})
	if err != nil {
		t.Fatal(err)
	}
	snap.Capabilities = map[string]policy.Capability{"design_systems": {Enabled: true, OrgDefault: ds}}
	log, err := Scanners(dir, snap, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]
	results := slices.DeleteFunc(slices.Clone(run.Results), func(r finding.Result) bool { return r.RuleID != "design.org-system" })
	var notes []string
	for _, inv := range run.Invocations {
		for _, n := range inv.ToolExecutionNotifications {
			if strings.Contains(n.Message.Text, "design.org-system") {
				notes = append(notes, n.Level+" "+n.Message.Text)
			}
		}
	}
	return results, notes, log
}

const serverManifest = "{\n  \"name\": \"api\",\n  \"dependencies\": {\n    \"express\": \"^4.21.0\"\n  }\n}\n"

// The UI's manifest in a subfolder lists the design system, beside a server-only manifest at the root:
// the app uses it, and nothing is said (contracts/finding.md, design.org-system).
func TestDesignAnyManifestListingTheSystemPasses(t *testing.T) {
	results, notes := designScan(t, map[string]string{
		"package.json":     serverManifest,
		"web/package.json": `{"devDependencies": {"vite": "^6"}, "dependencies": {"@meridian/ui": "^2.1.0", "react": "^19"}}`,
	}, meridian, policy.Advisory)
	if len(results) != 0 || len(notes) != 0 {
		t.Fatalf("results %+v, notes %q", results, notes)
	}
}

// A devDependency counts as declared, as a dependency does.
func TestDesignADevDependencyPasses(t *testing.T) {
	results, notes := designScan(t, map[string]string{"package.json": `{"devDependencies": {"@meridian/ui": "2.1.0"}}`}, meridian, policy.Advisory)
	if len(results) != 0 || len(notes) != 0 {
		t.Fatalf("results %+v, notes %q", results, notes)
	}
}

// No manifest lists it: one finding on the manifest nearest the root, at its "dependencies" key, its
// problem naming every manifest read; a vendored manifest that lists it is not the app's.
func TestDesignNoManifestListingTheSystemIsOneFinding(t *testing.T) {
	results, notes := designScan(t, map[string]string{
		"package.json":                               serverManifest,
		"web/package.json":                           `{"dependencies": {"@mui/material": "^6", "react": "^19"}}`,
		"web/node_modules/x/package.json":            `{"dependencies": {"@meridian/ui": "^2"}}`,
		"web/node_modules/@meridian/ui/package.json": `{"name": "@meridian/ui"}`,
	}, meridian, policy.Advisory)
	if len(notes) != 0 || len(results) != 1 {
		t.Fatalf("results %+v, notes %q", results, notes)
	}
	r := results[0]
	loc := r.Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "package.json" || loc.Region.StartLine != 3 || loc.Region.StartColumn != 3 || loc.Region.EndColumn != 17 ||
		loc.Region.Snippet.Text != `  "dependencies": {` {
		t.Fatalf("location %+v", loc)
	}
	c := r.Properties.Canyon
	for got, want := range map[string]string{
		c.Title:            "The app doesn't use Meridian UI, the org's design system",
		c.Problem:          "The org's design system is Meridian UI (@meridian/ui), and no package.json in this app lists it. Canyon read package.json and web/package.json.",
		c.Fix:              "Add @meridian/ui to the dependencies of the package.json that holds the UI (registry: https://registry.meridian.example/ui), and build the UI from its components.",
		c.RequiredEndState: "The app's UI is built from Meridian UI's components, and its package.json lists @meridian/ui.",
		string(c.Severity): "high",
		c.PolicyName:       "Apps use the org's design system",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if !slices.Equal(c.DoNot, []string{"list @meridian/ui without using its components", "copy Meridian UI's components in by hand"}) {
		t.Errorf("do_not %q", c.DoNot)
	}
	if r.PartialFingerprints.CanyonV1 == "" {
		t.Error("no fingerprint")
	}
}

// A manifest with no "dependencies" key: the finding sits on its opening brace.
func TestDesignAManifestWithoutDependenciesIsFoundAtItsBrace(t *testing.T) {
	results, _ := designScan(t, map[string]string{"package.json": "\r\n{\"name\": \"x\", \"devDependencies\": {\"react\": \"19\"}}\r\n"}, meridian, policy.Advisory)
	if len(results) != 1 {
		t.Fatalf("results %+v", results)
	}
	if g := results[0].Locations[0].PhysicalLocation.Region; g.StartLine != 2 || g.StartColumn != 1 || g.EndLine != 2 || g.EndColumn != 2 || g.Snippet.Text != `{"name": "x", "devDependencies": {"react": "19"}}` {
		t.Fatalf("region %+v", g)
	}
}

// No manifest at all: not checked, never a pass and never a finding.
func TestDesignNoManifestIsNotChecked(t *testing.T) {
	results, notes := designScan(t, map[string]string{"app.py": "print(1)\n", "node_modules/x/package.json": "{}"}, meridian, policy.Advisory)
	if want := []string{"warning not checked: design.org-system: no package.json in this app; Canyon reads the design system from package.json"}; len(results) != 0 || !slices.Equal(notes, want) {
		t.Fatalf("results %+v, notes %q", results, notes)
	}
}

// A manifest the parser refuses is named and not read; the others still decide.
func TestDesignAnInvalidManifestIsNamedAndSkipped(t *testing.T) {
	results, notes := designScan(t, map[string]string{"package.json": "{,", "web/package.json": `{"dependencies": {"react": "19"}}`}, meridian, policy.Advisory)
	if want := []string{"warning not checked: design.org-system: package.json is not valid JSON"}; !slices.Equal(notes, want) || len(results) != 1 ||
		results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI != "web/package.json" ||
		!strings.HasSuffix(results[0].Properties.Canyon.Problem, "Canyon read web/package.json.") {
		t.Fatalf("results %+v, notes %q", results, notes)
	}
	results, notes = designScan(t, map[string]string{"package.json": "[1"}, meridian, policy.Advisory)
	if want := []string{"warning not checked: design.org-system: package.json is not valid JSON"}; !slices.Equal(notes, want) || len(results) != 0 {
		t.Fatalf("with nothing read: results %+v, notes %q", results, notes)
	}
}

// No default set, or the rule off: nothing is read and nothing is said.
func TestDesignNoDefaultOrRuleOffSaysNothing(t *testing.T) {
	files := map[string]string{"package.json": serverManifest}
	for name, c := range map[string]struct {
		ds    *policy.DesignSystem
		state policy.State
	}{"no default": {nil, policy.Advisory}, "off": {meridian, policy.Off}} {
		if results, notes := designScan(t, files, c.ds, c.state); len(results) != 0 || len(notes) != 0 {
			t.Errorf("%s: results %+v, notes %q", name, results, notes)
		}
	}
}

// The two shipped systems are read from components.json: shadcn/ui by one existing, Canyon by its
// registries naming @canyon.
func TestDesignTheShippedSystemsAreReadFromComponentsJSON(t *testing.T) {
	shadcn := &policy.DesignSystem{Name: "shadcn/ui", Namespace: "@shadcn/ui", RegistryURL: "https://ui.shadcn.com/r"}
	canyon := &policy.DesignSystem{Name: "Canyon", Namespace: "@canyon/ui", RegistryURL: "https://canyon.example/registry/canyon"}
	ui := `{"dependencies": {"react": "^19"}}`
	for name, c := range map[string]struct {
		ds    *policy.DesignSystem
		files map[string]string
		fix   string // empty: passes
	}{
		"shadcn used":    {shadcn, map[string]string{"web/package.json": ui, "web/components.json": `{"style": "new-york"}`}, ""},
		"shadcn absent":  {shadcn, map[string]string{"web/package.json": ui}, "Run npx shadcn@latest init where the UI's package.json is, then add components with npx shadcn@latest add <component>."},
		"shadcn invalid": {shadcn, map[string]string{"web/package.json": ui, "web/components.json": `[`}, "Run npx shadcn@latest init where the UI's package.json is, then add components with npx shadcn@latest add <component>."},
		"canyon used":    {canyon, map[string]string{"web/package.json": ui, "web/components.json": `{"registries": {"@canyon": "https://canyon.example/registry/canyon/{name}.json"}}`}, ""},
		"canyon absent": {canyon, map[string]string{"web/package.json": ui, "web/components.json": `{"registries": {"@acme": "https://acme.example/{name}.json"}}`},
			`Add "@canyon": "https://canyon.example/registry/canyon/{name}.json" to registries in the UI's components.json, then add components with npx shadcn@latest add @canyon/<component>.`},
		"canyon by package": {canyon, map[string]string{"web/package.json": `{"dependencies": {"@canyon/ui": "1", "react": "^19"}}`},
			`Add "@canyon": "https://canyon.example/registry/canyon/{name}.json" to registries in the UI's components.json, then add components with npx shadcn@latest add @canyon/<component>.`},
	} {
		results, notes := designScan(t, c.files, c.ds, policy.Advisory)
		switch {
		case len(notes) != 0:
			t.Errorf("%s: notes %q", name, notes)
		case c.fix == "" && len(results) != 0:
			t.Errorf("%s: results %+v", name, results)
		case c.fix != "" && (len(results) != 1 || results[0].Properties.Canyon.Fix != c.fix):
			t.Errorf("%s: results %+v", name, results)
		case name == "canyon absent" && !strings.HasSuffix(results[0].Properties.Canyon.Problem, "lists the @canyon registry. Canyon read web/package.json and web/components.json."):
			t.Errorf("%s: problem %q", name, results[0].Properties.Canyon.Problem)
		}
	}
}

// Three manifests read are named as a sentence names them.
func TestDesignNamesThreeManifestsAsASentence(t *testing.T) {
	results, _ := designScan(t, map[string]string{"package.json": "{}", "api/package.json": "{}", "web/package.json": `{"dependencies": {"react": "^19"}}`}, meridian, policy.Advisory)
	if len(results) != 1 || !strings.HasSuffix(results[0].Properties.Canyon.Problem, "Canyon read api/package.json, package.json and web/package.json.") {
		t.Fatalf("results %+v", results)
	}
}

// A server with no UI framework in any manifest has no UI to check (contracts/finding.md, design.org-system): the
// must-pass app, express and a server.js, gets no finding, counts 0 blocking and 0 warnings, and nothing it carries
// says "not checked", which would read the run as not fully checked. The must-fail app, react without the design
// system, gets the finding.
func TestDesignAnAppWithoutAUIHasNothingToCheck(t *testing.T) {
	results, notes, log := designScanDir(t, filepath.Join("testdata", "design.org-system", "must-pass"), meridian, policy.Advisory)
	if blocking, warnings := finding.Count(log); len(results) != 0 || blocking != 0 || warnings != 0 {
		t.Errorf("a server with no UI: %d results, %d blocking, %d warnings, want none", len(results), blocking, warnings)
	}
	if want := []string{"note design.org-system: no package.json in this app lists a UI framework, so there was no UI to check"}; !slices.Equal(notes, want) {
		t.Errorf("a server with no UI says %q, want %q", notes, want)
	}
	for _, run := range log.Runs {
		for _, inv := range run.Invocations {
			for _, n := range inv.ToolExecutionNotifications {
				if strings.Contains(n.Message.Text, "not checked") {
					t.Errorf("a server with no UI reads as not fully checked: %s %q", n.Level, n.Message.Text)
				}
			}
		}
	}
	if results, _, _ := designScanDir(t, filepath.Join("testdata", "design.org-system", "must-fail"), meridian, policy.Advisory); len(results) != 1 {
		t.Errorf("a react app without the design system: %d results, want its one", len(results))
	}
}
