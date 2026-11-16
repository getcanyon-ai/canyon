package gitops

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The reference set-up's Applications (deploy/gitops/argocd) never share a name: an app's own and its namespace's,
// over the same directories, for every app name Canyon takes — ns-payments and payments-ns among them — on both
// branches. Two ApplicationSets naming one Application fight over it, and one prunes what the other made.
func TestTheReferenceApplicationsNeverShareAName(t *testing.T) {
	type gen struct{ env, set string }
	var gens []gen
	names := map[string]string{} // the set and generator each name came from
	templates := map[string]string{}
	for _, file := range []string{"applicationsets.yaml", "namespaces.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", "argocd", file)) //nolint:gosec // this tree's reference set-up
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var doc struct {
				Kind     string
				Metadata struct{ Name string }
				Spec     struct {
					Generators []struct {
						Git struct {
							Files  []struct{ Path string }
							Values map[string]string
						}
					}
					Template struct {
						Metadata struct{ Name string }
					}
				}
			}
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if doc.Kind != "ApplicationSet" {
				continue
			}
			templates[doc.Metadata.Name] = doc.Spec.Template.Metadata.Name
			for _, g := range doc.Spec.Generators {
				env := g.Git.Values["env"]
				if env == "" && len(g.Git.Files) == 1 { // bound by its path: ${ROOT}/*/<env>/canyon.json
					parts := strings.Split(g.Git.Files[0].Path, "/")
					env = parts[len(parts)-2]
				}
				gens = append(gens, gen{env: env, set: doc.Metadata.Name})
			}
		}
	}
	if len(templates) != 3 || len(gens) != 4 {
		t.Fatalf("the reference set-up has %d ApplicationSets and %d generators, want 3 and 4", len(templates), len(gens))
	}
	for _, app := range []string{"payments", "ns-payments", "payments-ns", "ns", "a-preview", "a-prod-ns", "claims-portal"} {
		for _, g := range gens {
			name := strings.NewReplacer("{{ index .path.segments ${APP_SEGMENT} }}", app, "{{ .values.env }}", g.env).Replace(templates[g.set])
			if strings.Contains(name, "{{") {
				t.Fatalf("%s's name %q has an expression this test doesn't know: teach it, then keep the names apart", g.set, templates[g.set])
			}
			from := g.set + " for " + app + " on " + g.env
			if was, ok := names[name]; ok {
				t.Errorf("%s and %s both name %s", was, from, name)
			}
			names[name] = from
		}
	}
}

// The reference Gateway (deploy/gitops/gateway) keeps the environments apart (design/HAND-OFF.md §10, #2476): a
// listener per environment, taking only that environment's hostnames and admitting routes only from namespaces the
// namespaces chart labelled with it; the reference route names its environment's listener; and the Argo CD page
// shows the Gateway as it is. A listener both environments share would let a route pushed to preview, unreviewed,
// claim a production hostname with a more specific match.
func TestTheReferenceGatewayKeepsEachEnvironmentsRoutesToItsListener(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", "gateway", "gateway.yaml")) //nolint:gosec // this tree's reference set-up
	if err != nil {
		t.Fatal(err)
	}
	var gw struct {
		Kind string
		Spec struct {
			Listeners []struct {
				Name, Hostname string
				AllowedRoutes  struct {
					Namespaces struct {
						From     string
						Selector struct {
							MatchLabels      map[string]string `yaml:"matchLabels"`
							MatchExpressions []any             `yaml:"matchExpressions"`
						}
					}
				} `yaml:"allowedRoutes"`
			}
		}
	}
	if err := yaml.Unmarshal(raw, &gw); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, l := range gw.Spec.Listeners {
		ns := l.AllowedRoutes.Namespaces
		if l.Name != "preview" && l.Name != "prod" {
			t.Errorf("listener %q is neither environment's", l.Name)
			continue
		}
		seen[l.Name] = true
		if !strings.HasPrefix(l.Hostname, "*."+l.Name+".") {
			t.Errorf("listener %s takes hostname %q, not *.%s.<domain> alone", l.Name, l.Hostname, l.Name)
		}
		if ns.From != "Selector" || len(ns.Selector.MatchExpressions) != 0 || len(ns.Selector.MatchLabels) != 1 || ns.Selector.MatchLabels["canyon.dev/env"] != l.Name {
			t.Errorf("listener %s admits routes from %s %v, not from namespaces labelled canyon.dev/env=%s alone", l.Name, ns.From, ns.Selector, l.Name)
		}
	}
	if gw.Kind != "Gateway" || len(gw.Spec.Listeners) != 2 || !seen["preview"] || !seen["prod"] {
		t.Fatalf("the reference Gateway has listeners %v, want one for preview and one for prod", seen)
	}

	route, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", "templates", ".canyon", "route.yaml")) //nolint:gosec // this tree's reference template
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Spec struct {
			ParentRefs []struct {
				SectionName string `yaml:"sectionName"`
			} `yaml:"parentRefs"`
		}
	}
	if err := yaml.Unmarshal(route, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Spec.ParentRefs) == 0 {
		t.Error("the reference route names no parent")
	}
	for _, p := range r.Spec.ParentRefs {
		if p.SectionName != "${env}" {
			t.Errorf("the reference route attaches to listener %q, not its environment's, ${env}", p.SectionName)
		}
	}

	page, err := os.ReadFile(filepath.Join("..", "docs", "src", "content", "docs", "self-host", "hand-off-argo-cd.md")) //nolint:gosec // this tree's docs
	if err != nil {
		t.Fatal(err)
	}
	body := raw[bytes.Index(raw, []byte("apiVersion:")):]
	if !bytes.Contains(page, append(append([]byte("```yaml\n"), body...), "```"...)) {
		t.Error("the Argo CD page's Gateway is not deploy/gitops/gateway/gateway.yaml as it is")
	}
}
