package gitops

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// contract is contracts/testdata/release/repo, meridian's release as Canyon commits it.
const contract = "../contracts/testdata/release/repo"

// meridian is the contract's release: meridian.json's deploy body under its repo's settings.
func meridian(t *testing.T) (Release, Templates) {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/release/meridian.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Deploy struct {
			Release, App, Environment, Image string
			Platform                         struct{ Architecture string }
			GateRunJSON                      string `json:"gate_run"`
			Package                          struct{ ID, Label string }
			Variables                        []string
			Resources                        []struct {
				Name, Type string
				Gives      []string
			}
			AppEnv  map[string]string `json:"app_env"`
			Sidecar struct {
				Image string
				Env   map[string]string
			}
		}
		RepoGet struct{ Store string } `json:"repo_get"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	d := doc.Deploy
	rel := Release{Canyon: d.AppEnv["CANYON_IDENTITY_ISSUER"], ID: d.Release, App: d.App, Env: d.Environment, GateRun: d.GateRunJSON, Package: d.Package.ID,
		Version: d.Package.Label, Image: d.Image, Arch: d.Platform.Architecture, Identity: d.Sidecar.Image, AppEnv: d.AppEnv, SidecarEnv: d.Sidecar.Env, Variables: d.Variables, Store: doc.RepoGet.Store}
	for _, r := range d.Resources {
		rel.Resources = append(rel.Resources, Resource{Name: r.Name, Type: r.Type, Gives: r.Gives})
	}
	return rel, templatesIn(t, filepath.Join(contract, "templates", ".canyon"))
}

func templatesIn(t *testing.T, dir string) Templates {
	t.Helper()
	tmpl := Templates{}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		tmpl[filepath.ToSlash(rel)], err = os.ReadFile(path) //nolint:gosec // the contract's own fixtures
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// Meridian's release renders, byte for byte, as contracts/testdata/release/repo says Canyon commits it: the
// seam's files are what Canyon writes, never a description of them.
func TestMeridianRendersAsTheContractSays(t *testing.T) {
	rel, tmpl := meridian(t)
	got, err := Render(rel, tmpl, "apps/.canyon")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(contract, "apps", rel.App, rel.Env)
	want := map[string][]byte{}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		want[filepath.ToSlash(rel)], err = os.ReadFile(path) //nolint:gosec // the contract's own fixtures
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(got.Files)), slices.Sorted(maps.Keys(want))) {
		t.Fatalf("Canyon commits %v, the contract says %v", slices.Sorted(maps.Keys(got.Files)), slices.Sorted(maps.Keys(want)))
	}
	for name, b := range got.Files {
		if !bytes.Equal(b, want[name]) {
			t.Errorf("%s: Canyon commits\n%s\nthe contract says\n%s", name, b, want[name])
		}
	}
	if got.Address != "https://claims-portal.preview.apps.meridian.example" {
		t.Errorf("the address read back from route.yaml: %q", got.Address)
	}
	if want := []Endpoint{{Name: "db", Endpoint: "postgres://claims-portal-preview-db-rw.canyon-claims-portal-preview.svc:5432/app"}}; !slices.Equal(got.Endpoints, want) {
		t.Errorf("the endpoints read back: %v, want %v", got.Endpoints, want)
	}
}

// Rendering is the same bytes every time: a retry rebuilds the same commit, and a tree already on the branch
// is known for what it is.
func TestRenderingIsStable(t *testing.T) {
	rel, tmpl := meridian(t)
	first, err := Render(rel, tmpl, "apps/.canyon")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, _ := Render(rel, tmpl, "apps/.canyon")
		if !maps.EqualFunc(first.Files, again.Files, bytes.Equal) {
			t.Fatal("two renders of one release differ")
		}
	}
}

// Every refusal a template can earn, each the template_invalid or template_missing problem naming the file and
// the reason, never the template's contents.
func TestTemplatesAreRefused(t *testing.T) {
	rel, tmpl := meridian(t)
	route := string(tmpl["route.yaml"])
	pg := string(tmpl["resources/postgres.yaml"])
	for name, c := range map[string]struct {
		file, body, code, reason string
	}{
		"no route":                     {"route.yaml", "", TemplateMiss, ""},
		"no template for a type":       {"resources/postgres.yaml", "", TemplateMiss, ""},
		"not YAML":                     {"route.yaml", "kind: [", TemplateBad, notYAML},
		"an anchor":                    {"route.yaml", route + "---\nkind: &a ConfigMap\napiVersion: v1\nmetadata: {name: x}\n", TemplateBad, anchored},
		"an alias":                     {"route.yaml", "a: &x 1\nb: *x\n", TemplateBad, anchored},
		"a merge key":                  {"route.yaml", "<<: {kind: ConfigMap}\n", TemplateBad, mergeKey},
		"a repeated key":               {"route.yaml", route + "---\napiVersion: v1\nkind: ConfigMap\nkind: Secret\nmetadata: {name: x}\n", TemplateBad, repeatedKey},
		"a custom tag":                 {"route.yaml", "apiVersion: !secret v1\nkind: ConfigMap\nmetadata: {name: x}\n", TemplateBad, foreignTag},
		"a variable in a key":          {"route.yaml", route + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata:\n  ${app}: x\n", TemplateBad, keyVariable},
		"an unknown variable":          {"route.yaml", strings.Replace(route, "${service}", "${token}", 1), TemplateBad, unknownVar},
		"an unclosed variable":         {"route.yaml", strings.Replace(route, "${service}", "${service", 1), TemplateBad, unknownVar},
		"over 64 KiB":                  {"route.yaml", route + "#" + strings.Repeat("x", 64<<10) + "\n", TemplateBad, tooBig},
		"eleven documents":             {"route.yaml", route + strings.Repeat("---\n", 10), TemplateBad, tooMany},
		"an object with no kind":       {"route.yaml", "apiVersion: v1\nmetadata: {name: x}\n", TemplateBad, unnamed},
		"a list for an object":         {"route.yaml", "- a\n- b\n", TemplateBad, unnamed},
		"another namespace":            {"route.yaml", strings.Replace(route, "  name: ${service}\n", "  name: ${service}\n  namespace: default\n", 1), TemplateBad, otherNS},
		"a cluster-scoped kind":        {"route.yaml", route + "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: x}\n", TemplateBad, clusterScoped},
		"Canyon's own Deployment":      {"route.yaml", route + "---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: ${service}\n", TemplateBad, collides},
		"the vars Secret":              {"route.yaml", route + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: ${service}-vars\n", TemplateBad, collides},
		"an access Secret":             {"resources/postgres.yaml", pg + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: ${service}-access-1\n", TemplateBad, collides},
		"an object two templates make": {"resources/postgres.yaml", pg + "---\n" + route, TemplateBad, collides},
		"no address":                   {"route.yaml", strings.ReplaceAll(route, "canyon.dev/address", "canyon.dev/elsewhere"), TemplateBad, noAddress},
		"two addresses":                {"route.yaml", route + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, annotations: {canyon.dev/address: https://x.example}}\n", TemplateBad, noAddress},
		"an http address":              {"route.yaml", strings.Replace(route, "https://", "http://", 1), TemplateBad, badAddress},
		"no endpoint":                  {"resources/postgres.yaml", strings.ReplaceAll(pg, "canyon.dev/endpoint", "canyon.dev/else"), TemplateBad, noEndpoint},
		"an endpoint with a password":  {"resources/postgres.yaml", strings.Replace(pg, "postgres://${service}", "postgres://app:pw@${service}", 1), TemplateBad, badEndpoint("postgres")},
	} {
		t.Run(name, func(t *testing.T) {
			bad := maps.Clone(tmpl)
			if c.body == "" {
				delete(bad, c.file)
			} else {
				bad[c.file] = []byte(c.body)
			}
			_, err := Render(rel, bad, "apps/.canyon")
			p, ok := IsProblem(err)
			if !ok || p.Code != c.code {
				t.Fatalf("got %v, want %s", err, c.code)
			}
			if !strings.HasPrefix(p.Message, "apps/.canyon/"+c.file) || !strings.Contains(p.Message, c.reason) || !strings.HasSuffix(p.Message, "Ask your platform team.") {
				t.Fatalf("the sentence %q does not name the file and the reason %q and send the reader to their platform team", p.Message, c.reason)
			}
			if c.body != "" && strings.Contains(p.Message, "apiVersion") && c.reason != unnamed {
				t.Fatalf("the sentence echoes the template: %q", p.Message)
			}
		})
	}
}

// An app's name never becomes YAML: a value that would read as YAML is a string in the committed bytes, and an
// app or environment that is not a DNS label never renders at all.
func TestNoValueBecomesYAML(t *testing.T) {
	rel, tmpl := meridian(t)
	tmpl["route.yaml"] = []byte(strings.Replace(string(tmpl["route.yaml"]), "spec:\n", "data:\n  release: ${release}\n  store: ${store}\nspec:\n", 1))
	for _, value := range []string{"a\n- b: c", "no", "true", "1", "{a: b}", "[x]", "x\nkind: Secret"} { // "no": YAML 1.1's false
		rel.Store = value
		got, err := Render(rel, tmpl, "apps/.canyon")
		if err != nil {
			t.Fatal(err)
		}
		objs, err := reparse(got.Files["route.yaml"])
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := objs[0]["data"].(map[string]any); data["store"] != value || len(data) != 2 {
			t.Fatalf("%q read back as %#v, not the one string", value, data)
		}
	}
	for _, app := range []string{"a\n- b: c", "Claims", "claims_portal", "-claims"} {
		rel.App = app
		if _, err := Render(rel, tmpl, "apps/.canyon"); err == nil {
			t.Errorf("the app %q rendered", app)
		}
	}
}

// The namespace is at most 63 characters: an app whose would be longer is refused before any git call.
func TestALongNameIsRefused(t *testing.T) {
	rel, tmpl := meridian(t)
	rel.App = strings.Repeat("a", 50)
	_, err := Render(rel, tmpl, "apps/.canyon")
	if p, ok := IsProblem(err); !ok || p.Code != NameTooLong {
		t.Fatalf("got %v, want name_too_long", err)
	}
}

// Nothing secret is ever committed: the access token is read from a Secret the cluster makes, never written.
func TestNoTokenIsCommitted(t *testing.T) {
	rel, tmpl := meridian(t)
	got, err := Render(rel, tmpl, "apps/.canyon")
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range got.Files {
		if strings.Contains(string(b), "redacted") {
			t.Errorf("%s holds the fixture's token", name)
		}
	}
	if !strings.Contains(string(got.Files["deployment.yaml"]), "name: claims-portal-preview-access-8f2a") {
		t.Errorf("the Deployment does not read its token from the release's Secret:\n%s", got.Files["deployment.yaml"])
	}
}

// A release whose body was journaled before it named a platform (#2528) — one in flight across the upgrade — renders
// exactly as before: no nodeSelector at all, never an empty kubernetes.io/arch.
func TestAReleaseNamingNoPlatformPinsNothing(t *testing.T) {
	rel, tmpl := meridian(t)
	rel.Arch = ""
	got, err := Render(rel, tmpl, "apps/.canyon")
	if err != nil {
		t.Fatal(err)
	}
	if d := string(got.Files["deployment.yaml"]); strings.Contains(d, "nodeSelector") || strings.Contains(d, "kubernetes.io/arch") {
		t.Fatalf("a release naming no platform pins its pod:\n%s", d)
	}
}
