package gitops

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"text/template/parse"

	"gopkg.in/yaml.v3"
)

// Flux's reference set-up (deploy/gitops/flux, #2479), read as data: who its accounts are and what they hold, how its
// sources, generators and releases tie each environment together, and what its chart may read.

// fluxDocs is every YAML document in deploy/gitops/<rel>, as generic maps.
func fluxDocs(t *testing.T, rel string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", rel)) //nolint:gosec // this tree's reference set-up
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
}

// at is the value at path in doc, nil when any step is missing.
func at(doc any, path ...string) any {
	for _, p := range path {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		doc = m[p]
	}
	return doc
}

func str(doc any, path ...string) string {
	s, _ := at(doc, path...).(string)
	return s
}

func keys(doc any, path ...string) []string {
	m, _ := at(doc, path...).(map[string]any)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// named is the one document of kind, namespace and name in docs.
func named(t *testing.T, docs []map[string]any, kind, namespace, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, d := range docs {
		if str(d, "kind") == kind && str(d, "metadata", "namespace") == namespace && str(d, "metadata", "name") == name {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s %s/%s in the reference set-up, want one", len(found), kind, namespace, name)
	}
	return found[0]
}

// kindResources is each kind an AppProject may list, as the resource RBAC names it. A kind not here fails the test:
// teach it, so canyon-deploy and the AppProjects keep naming the same things.
var kindResources = map[[2]string]string{
	{"apps", "Deployment"}:                         "deployments",
	{"", "Service"}:                                "services",
	{"", "ConfigMap"}:                              "configmaps",
	{"networking.k8s.io", "NetworkPolicy"}:         "networkpolicies",
	{"external-secrets.io", "ExternalSecret"}:      "externalsecrets",
	{"generators.external-secrets.io", "Password"}: "passwords",
	{"gateway.networking.k8s.io", "HTTPRoute"}:     "httproutes",
	{"postgresql.cnpg.io", "Cluster"}:              "clusters",
}

// U1: what Flux's sync accounts may apply is exactly what the AppProjects admit: the same kinds, no subresource.
func TestFluxCanyonDeployGrantsExactlyTheAppProjectsKinds(t *testing.T) {
	var want []string
	for i, p := range fluxDocs(t, "argocd/appprojects.yaml") {
		var kinds []string
		for _, k := range at(p, "spec", "namespaceResourceWhitelist").([]any) {
			key := [2]string{str(k, "group"), str(k, "kind")}
			res, ok := kindResources[key]
			if !ok {
				t.Fatalf("AppProject %s lists %s/%s, which this test doesn't know: teach it the resource RBAC names it by", str(p, "metadata", "name"), key[0], key[1])
			}
			kinds = append(kinds, key[0]+"/"+res)
		}
		sort.Strings(kinds)
		if i > 0 && !slices.Equal(kinds, want) {
			t.Fatalf("the AppProjects list different kinds: %v and %v", want, kinds)
		}
		want = kinds
	}
	var got []string
	role := named(t, fluxDocs(t, "flux/rbac.yaml"), "ClusterRole", "", "canyon-deploy")
	for _, r := range at(role, "rules").([]any) {
		if at(r, "resourceNames") != nil || at(r, "nonResourceURLs") != nil {
			t.Errorf("canyon-deploy has a rule %v with resourceNames or nonResourceURLs", r)
		}
		for _, g := range at(r, "apiGroups").([]any) {
			for _, res := range at(r, "resources").([]any) {
				if strings.Contains(res.(string), "/") || res == "*" || g == "*" {
					t.Errorf("canyon-deploy grants %s/%s: a subresource or a wildcard, which no AppProject admits", g, res)
				}
				got = append(got, g.(string)+"/"+res.(string))
			}
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("canyon-deploy grants %v, the AppProjects admit %v", got, want)
	}
}

// grant is one binding's rules for an account: where they hold and what they are.
type grant struct {
	Binding, Namespace, Role string
	Rules                    []any
}

// U2: the sync accounts are bound nowhere by the platform's files (the generator binds them, namespace by namespace),
// and the generator's account holds exactly the grants the docs state, nothing in flux-system.
func TestFluxAccountsHoldExactlyTheirGrants(t *testing.T) {
	docs := fluxDocs(t, "flux/rbac.yaml")
	roles := map[string][]any{}
	var accounts []string
	for _, d := range docs {
		switch str(d, "kind") {
		case "ClusterRole", "Role":
			roles[str(d, "kind")+" "+str(d, "metadata", "namespace")+"/"+str(d, "metadata", "name")] = at(d, "rules").([]any)
		case "ServiceAccount":
			accounts = append(accounts, str(d, "metadata", "namespace")+"/"+str(d, "metadata", "name"))
		}
	}
	sort.Strings(accounts)
	if want := []string{"canyon-sync/canyon-preview", "canyon-sync/canyon-prod", "flux-system/canyon-namespaces"}; !slices.Equal(accounts, want) {
		t.Fatalf("the reference set-up makes the accounts %v, want %v", accounts, want)
	}
	grants := map[string][]grant{}
	for _, d := range docs {
		kind := str(d, "kind")
		if kind != "ClusterRoleBinding" && kind != "RoleBinding" {
			continue
		}
		roleNS := ""
		if str(d, "roleRef", "kind") == "Role" {
			roleNS = str(d, "metadata", "namespace")
		}
		role := str(d, "roleRef", "kind") + " " + roleNS + "/" + str(d, "roleRef", "name")
		rules, ok := roles[role]
		if !ok {
			t.Fatalf("%s %s binds %s, which the file does not define", kind, str(d, "metadata", "name"), role)
		}
		for _, s := range at(d, "subjects").([]any) {
			who := str(s, "namespace") + "/" + str(s, "name")
			grants[who] = append(grants[who], grant{Binding: kind, Namespace: str(d, "metadata", "namespace"), Role: role, Rules: rules})
		}
	}
	for _, sync := range []string{"canyon-sync/canyon-preview", "canyon-sync/canyon-prod"} {
		if g := grants[sync]; len(g) != 0 {
			t.Errorf("%s is bound by the platform's files (%v): only the generator binds it, in each app's namespace", sync, g)
		}
	}
	rule := func(groups, resources, names, verbs []any) map[string]any {
		r := map[string]any{"apiGroups": groups, "resources": resources, "verbs": verbs}
		if names != nil {
			r["resourceNames"] = names
		}
		return r
	}
	write := []any{"get", "create", "update", "patch", "delete"}
	want := []grant{
		{Binding: "ClusterRoleBinding", Role: "ClusterRole /canyon-namespaces", Rules: []any{
			rule([]any{""}, []any{"namespaces"}, nil, write),
			rule([]any{"rbac.authorization.k8s.io"}, []any{"rolebindings"}, nil, write),
			rule([]any{"rbac.authorization.k8s.io"}, []any{"clusterroles"}, []any{"canyon-deploy"}, []any{"bind"}),
			rule([]any{"admissionregistration.k8s.io"}, []any{"validatingadmissionpolicies", "validatingadmissionpolicybindings"}, []any{"canyon-namespaces"}, []any{"get"}),
		}},
		{Binding: "RoleBinding", Namespace: "canyon-sync", Role: "Role canyon-sync/canyon-namespaces", Rules: []any{
			rule([]any{"kustomize.toolkit.fluxcd.io"}, []any{"kustomizations"}, nil, write),
		}},
		{Binding: "RoleBinding", Namespace: "canyon-namespaces", Role: "Role canyon-namespaces/canyon-namespaces", Rules: []any{
			rule([]any{""}, []any{"secrets"}, nil, []any{"get", "list", "create", "update", "patch", "delete"}),
		}},
	}
	if got := grants["flux-system/canyon-namespaces"]; !reflect.DeepEqual(got, want) {
		t.Errorf("canyon-namespaces holds\n%v\nwant exactly\n%v", got, want)
	}
	for _, g := range grants["flux-system/canyon-namespaces"] {
		if g.Namespace == "flux-system" {
			t.Errorf("canyon-namespaces holds %s in flux-system, where Flux's own cluster-admin accounts are", g.Role)
		}
	}
}

// U3: per environment, the sync, the names, the generator and the release name the same branch and environment, the
// chart lands where Helm loads it, the names land under deploy/ by a directory copy, and the release is the one the
// docs state. A disagreement among them is what the sentinel catches on the cluster; this catches it in the files.
func TestFluxEachEnvironmentIsTiedTogether(t *testing.T) {
	sources, releases := fluxDocs(t, "flux/sources.yaml"), fluxDocs(t, "flux/generator.yaml")
	if len(sources) != 7 || len(releases) != 2 {
		t.Fatalf("sources.yaml has %d objects and generator.yaml %d, want 7 (five GitRepositories, two ArtifactGenerators) and 2", len(sources), len(releases))
	}
	branch := map[string]string{"preview": "${PREVIEW_BRANCH}", "prod": "${PROD_BRANCH}"}
	for _, env := range []string{"preview", "prod"} {
		sync := named(t, sources, "GitRepository", "canyon-sync", "canyon-"+env)
		names := named(t, sources, "GitRepository", "flux-system", "canyon-"+env+"-names")
		for _, r := range []map[string]any{sync, names} {
			if str(r, "spec", "url") != "${REPO_URL}" || str(r, "spec", "ref", "branch") != branch[env] || !slices.Equal(keys(r, "spec", "ref"), []string{"branch"}) ||
				str(r, "spec", "secretRef", "name") != "canyon-deploy-repo" {
				t.Errorf("%s reads %s at %v, want ${REPO_URL}'s %s branch through canyon-deploy-repo", str(r, "metadata", "name"), str(r, "spec", "url"), at(r, "spec", "ref"), branch[env])
			}
		}
		if got, want := str(sync, "spec", "ignore"), "/*\n!/${ROOT}/*/"+env+"/\n**/canyon.json\n**/kustomization.yaml\n**/kustomization.yml\n**/Kustomization\n"; got != want {
			t.Errorf("canyon-%s's ignore is\n%s\nwant\n%s", env, got, want)
		}
		if got, want := str(names, "spec", "ignore"), "/*\n!/${ROOT}/*/"+env+"/canyon.json\n!/${ROOT}/.canyon/route.yaml\n"; got != want {
			t.Errorf("canyon-%s-names's ignore is\n%s\nwant\n%s", env, got, want)
		}

		gen := named(t, sources, "ArtifactGenerator", "flux-system", "canyon-namespaces-"+env)
		wantSources := []any{
			map[string]any{"alias": "platform", "kind": "GitRepository", "name": "canyon-platform"},
			map[string]any{"alias": "names", "kind": "GitRepository", "name": "canyon-" + env + "-names"},
		}
		if !reflect.DeepEqual(at(gen, "spec", "sources"), wantSources) || !slices.Equal(keys(gen, "spec"), []string{"artifacts", "sources"}) {
			t.Errorf("canyon-namespaces-%s's spec is %v, want the sources %v and its artifacts alone", env, at(gen, "spec"), wantSources)
		}
		wantArtifacts := []any{map[string]any{"name": "canyon-namespaces-" + env, "copy": []any{
			map[string]any{"from": "@platform/${PLATFORM_PATH}/**", "to": "@artifact/canyon-namespace/"},
			map[string]any{"from": "@names/", "to": "@artifact/canyon-namespace/deploy/"},
		}}}
		if got := at(gen, "spec", "artifacts"); !reflect.DeepEqual(got, wantArtifacts) {
			t.Errorf("canyon-namespaces-%s makes %v, want %v: the chart in canyon-namespace/, the names copied as a directory under its deploy/, no strategy", env, got, wantArtifacts)
		}

		hr := named(t, releases, "HelmRelease", "flux-system", "canyon-namespaces-"+env)
		if got, want := keys(hr, "spec"), []string{"chartRef", "driftDetection", "install", "interval", "maxHistory", "releaseName", "serviceAccountName", "storageNamespace", "targetNamespace", "upgrade", "values"}; !slices.Equal(got, want) {
			t.Errorf("canyon-namespaces-%s's spec sets %v, want %v alone", env, got, want)
		}
		want := map[string]any{
			"releaseName":        "canyon-namespaces-" + env,
			"chartRef":           map[string]any{"kind": "ExternalArtifact", "name": "canyon-namespaces-" + env},
			"serviceAccountName": "canyon-namespaces",
			"targetNamespace":    "canyon-namespaces",
			"storageNamespace":   "canyon-namespaces",
			"interval":           "${INTERVAL}",
			"maxHistory":         3,
			"install": map[string]any{"disableWait": true, "disableTakeOwnership": true,
				"strategy": map[string]any{"name": "RetryOnFailure", "retryInterval": "1m"}},
			"upgrade": map[string]any{"disableWait": true, "disableTakeOwnership": true, "cleanupOnFail": true,
				"strategy": map[string]any{"name": "RetryOnFailure", "retryInterval": "1m"}},
			"driftDetection": map[string]any{"mode": "enabled"},
			"values":         map[string]any{"env": env, "root": "${ROOT}", "interval": "${INTERVAL}"},
		}
		if got := at(hr, "spec"); !reflect.DeepEqual(got, want) {
			t.Errorf("canyon-namespaces-%s is\n%v\nwant\n%v", env, got, want)
		}
	}
	platform := named(t, sources, "GitRepository", "flux-system", "canyon-platform")
	if str(platform, "spec", "url") != "${PLATFORM_REPO_URL}" || str(platform, "spec", "ref", "branch") != "${PLATFORM_REVISION}" || at(platform, "spec", "ignore") != nil {
		t.Errorf("canyon-platform reads %v, want ${PLATFORM_REPO_URL} at ${PLATFORM_REVISION}, whole", at(platform, "spec"))
	}
}

// helmTrees parses one of the namespaces chart's templates as Helm's engine does, its functions unchecked.
func helmTrees(t *testing.T, name string) map[string]*parse.Tree {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", "namespaces", "templates", name)) //nolint:gosec // this tree's chart
	if err != nil {
		t.Fatal(err)
	}
	tree := parse.New(name)
	tree.Mode = parse.SkipFuncCheck | parse.ParseComments
	set := map[string]*parse.Tree{}
	if _, err := tree.Parse(string(raw), "", "", set); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return set
}

// walk calls see with every node under n.
func walkTemplate(n parse.Node, see func(parse.Node)) {
	if n == nil || reflect.ValueOf(n).IsNil() {
		return
	}
	see(n)
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			walkTemplate(c, see)
		}
	case *parse.ActionNode:
		walkTemplate(n.Pipe, see)
	case *parse.PipeNode:
		for _, d := range n.Decl {
			walkTemplate(d, see)
		}
		for _, c := range n.Cmds {
			walkTemplate(c, see)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walkTemplate(a, see)
		}
	case *parse.ChainNode:
		walkTemplate(n.Node, see)
	case *parse.IfNode:
		walkTemplate(&n.BranchNode, see)
	case *parse.RangeNode:
		walkTemplate(&n.BranchNode, see)
	case *parse.WithNode:
		walkTemplate(&n.BranchNode, see)
	case *parse.BranchNode:
		walkTemplate(n.Pipe, see)
		walkTemplate(n.List, see)
		walkTemplate(n.ElseList, see)
	case *parse.TemplateNode:
		walkTemplate(n.Pipe, see)
	}
}

// U4: the chart reads the deploy repo's files by their paths alone. Under Flux the ArtifactGenerator lays the
// deploy repo's files beside it, so a template that read one's contents, or ran one as a template, would render what
// a writer committed, as an account that may make namespaces. Every template is walked. .Files.Glob maps each path
// to its contents, so it may appear only as the whole pipe of a range that names two variables, the path and one
// never used, and whose body never uses the dot, which is the contents there too: never $.Files.Glob, a Glob kept in
// a variable, or one handed to index or anything else.
func TestFluxChartNeverReadsTheDeployReposContents(t *testing.T) {
	forbidden := map[string]bool{"Get": true, "GetBytes": true, "Lines": true, "AsConfig": true, "AsSecrets": true}
	entries, err := os.ReadDir(filepath.Join("..", "deploy", "gitops", "namespaces", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the namespaces chart has no templates")
	}
	for _, e := range entries {
		name := e.Name()
		for tname, tree := range helmTrees(t, name) {
			ranged := map[parse.Node]bool{} // each .Files.Glob that is a range's whole pipe
			walkTemplate(tree.Root, func(n parse.Node) {
				r, ok := n.(*parse.RangeNode)
				if !ok || len(r.Pipe.Cmds) != 1 {
					return
				}
				f, ok := r.Pipe.Cmds[0].Args[0].(*parse.FieldNode)
				if !ok || !slices.Equal(f.Ident, []string{"Files", "Glob"}) {
					return
				}
				ranged[f] = true
				if len(r.Pipe.Decl) != 2 {
					t.Errorf("%s (%s) ranges over .Files.Glob with %d variables: name two, the path and an unused one, since one alone is the file's contents", name, tname, len(r.Pipe.Decl))
					return
				}
				contents := r.Pipe.Decl[1].Ident[0]
				for _, list := range []*parse.ListNode{r.List, r.ElseList} {
					walkTemplate(list, func(b parse.Node) {
						switch b := b.(type) {
						case *parse.VariableNode:
							if b.Ident[0] == contents {
								t.Errorf("%s (%s) uses %s, a file's contents from .Files.Glob: the chart reads the deploy repo's paths alone", name, tname, contents)
							}
						case *parse.DotNode, *parse.FieldNode:
							t.Errorf("%s (%s) uses the dot inside a range over .Files.Glob, where it is a file's contents: %s", name, tname, b)
						}
					})
				}
			})
			walkTemplate(tree.Root, func(n parse.Node) {
				var idents []string
				switch n := n.(type) {
				case *parse.FieldNode:
					idents = n.Ident
				case *parse.ChainNode:
					idents = n.Field
				case *parse.VariableNode:
					idents = n.Ident
				case *parse.IdentifierNode:
					if n.Ident == "tpl" {
						t.Errorf("%s (%s) calls tpl: the chart never renders text as a template", name, tname)
					}
				}
				for _, id := range idents {
					if forbidden[id] {
						t.Errorf("%s (%s) reads .%s: the chart reads the deploy repo's paths alone, never a file's contents", name, tname, id)
					}
					if id == "Glob" && !ranged[n] {
						t.Errorf("%s (%s) uses %s other than as the whole pipe of a range: .Files.Glob holds the files' contents", name, tname, n)
					}
				}
			})
		}
	}
}
