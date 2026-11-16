package gitops

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/executor/kube"
	"github.com/getcanyon-ai/canyon/internal/names"
	"github.com/getcanyon-ai/canyon/internal/place"
	kyaml "sigs.k8s.io/yaml"
)

// Release is one deploy release as Canyon commits it: the deploy webhook's body (release.schema.json's deploy)
// and the deploy repo's settings it is rendered under.
type Release struct {
	Canyon   string // Canyon's public URL, which canyon.json names
	ID       string // rel_<hex>
	App, Env string
	GateRun  string
	Package  string
	Version  string
	Image    string // the app's, by digest
	Arch     string // its architecture, the release's platform: the Deployment's nodes; "" (a body journaled before #2528) pins nothing
	Identity string // the sidecar's, by digest

	AppEnv     map[string]string // app_env
	SidecarEnv map[string]string // sidecar.env
	Variables  []string
	Resources  []Resource

	Store string // the ClusterSecretStore Canyon's ExternalSecret reads
	CA    string // Canyon's CA, PEM, or ""
}

// Resource is one resource the release lists: canyon.toml's name, its type and what it gives.
type Resource struct {
	Name, Type string
	Gives      []string
}

// Templates are the company's, under <root>/.canyon: route.yaml, externalsecret.yaml, resources/<type>.yaml.
type Templates map[string][]byte

// Rendered is what Canyon commits under <root>/<app>/<env>/, by path below it, and what the route and the
// resources' templates say, read back from those bytes.
type Rendered struct {
	Files     map[string][]byte
	Address   string
	Endpoints []Endpoint
}

// Endpoint is where one resource is, as its template annotated it.
type Endpoint struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

// Canyon is canyon.json (release.schema.json's canyon_json): which Canyon wrote the directory and what
// release it is.
type Canyon struct {
	Canyon  string `json:"canyon"`
	App     string `json:"app"`
	Env     string `json:"env"`
	Release string `json:"release"`
	GateRun string `json:"gate_run"`
	Package string `json:"package"`
	Version string `json:"version"`
}

var (
	releaseID    = regexp.MustCompile(`^rel_[a-z0-9]+$`)
	resourceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	resourceType = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	arch         = regexp.MustCompile(`^([a-z0-9]{1,16})?$`) // release.schema.json's platform architecture, or none
)

// Namespace is a release's namespace: a fixed prefix no setting removes, so a delete never prunes a namespace
// that is not Canyon's.
func Namespace(app, env string) string { return "canyon-" + app + "-" + env }

// accessSecret is the Secret holding the release's access token: its id's hex part, a new name per release,
// so every deploy rolls the pod.
func accessSecret(rel Release) string {
	return rel.App + "-" + rel.Env + "-access-" + strings.TrimPrefix(rel.ID, "rel_")
}

// resourceSecret is the Secret a resource's template makes, holding what it gives.
func resourceSecret(app, env, name string) string { return app + "-" + env + "-res-" + name }

// Render is the release's files, rendered in memory before any git call; templates are read from the
// branch being written, file names in a refusal spelled under dir (<root>/.canyon).
func Render(rel Release, tmpl Templates, dir string) (Rendered, error) {
	ns := Namespace(rel.App, rel.Env)
	if !names.ValidProject(rel.App) || (rel.Env != "preview" && rel.Env != "prod") || !releaseID.MatchString(rel.ID) || !arch.MatchString(rel.Arch) {
		return Rendered{}, fmt.Errorf("gitops: release %q of %q in %q is not one Canyon makes", rel.ID, rel.App, rel.Env)
	}
	if len(ns) > 63 {
		return Rendered{}, nameTooLong(ns)
	}
	labels := map[string]any{"canyon.app": rel.App, "canyon.target": rel.Env}
	pod := kube.HandOffPod{App: rel.App, Env: rel.Env, Namespace: ns, Image: rel.Image, Identity: rel.Identity, Arch: rel.Arch,
		AppEnv: rel.AppEnv, SidecarEnv: rel.SidecarEnv, AccessSecret: accessSecret(rel), VarsSecret: rel.App + "-" + rel.Env + "-vars",
		Variables: slices.Sorted(slices.Values(rel.Variables)), CA: rel.CA != "",
		Annotations: map[string]string{"canyon.dev/release": rel.ID, "canyon.dev/gate-run": rel.GateRun, "canyon.dev/package": rel.Package}}
	for _, r := range rel.Resources {
		if !resourceName.MatchString(r.Name) || !resourceType.MatchString(r.Type) {
			return Rendered{}, fmt.Errorf("gitops: resource %q of type %q is not one Canyon makes", r.Name, r.Type)
		}
		for _, g := range r.Gives {
			pod.Gives = append(pod.Gives, kube.Give{Name: g, Secret: resourceSecret(rel.App, rel.Env, r.Name)})
		}
	}
	files := map[string][]byte{}
	own := map[string]bool{} // kind/name of every object rendered so far: no template makes one twice
	add := func(path string, objs []object) error {
		for _, o := range objs {
			own[o.kind()+"/"+o.name()] = true
		}
		b, err := serialize(objs)
		files[path] = b
		return err
	}
	for path, typed := range map[string]any{"deployment.yaml": kube.HandOffDeployment(pod), "service.yaml": kube.HandOffService(rel.App, rel.Env, ns),
		"networkpolicy.yaml": kube.HandOffNetworkPolicy(rel.App, rel.Env, ns)} {
		o, err := fromTyped(typed)
		if err != nil {
			return Rendered{}, err
		}
		if err := add(path, []object{o}); err != nil {
			return Rendered{}, err
		}
	}
	if rel.CA != "" {
		if err := add("ca.yaml", []object{{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "canyon-ca", "namespace": ns, "labels": labels}, "data": map[string]any{"ca.crt": rel.CA}}}); err != nil {
			return Rendered{}, err
		}
	}
	vars := func(name, secret string) map[string]string {
		return map[string]string{"app": rel.App, "env": rel.Env, "namespace": ns, "name": name, "secret": secret, "release": rel.ID,
			"store": rel.Store, "service": rel.App + "-" + rel.Env}
	}
	reserved := func(o object, file string) bool { // Canyon's names: the vars Secret and the access Secret, whatever kind makes them
		n := o.name()
		return own[o.kind()+"/"+n] || n == rel.App+"-"+rel.Env+"-vars" || file != "externalsecret.yaml" && strings.HasPrefix(n, rel.App+"-"+rel.Env+"-access-")
	}
	from := func(file, name, secret string) ([]object, error) {
		raw, ok := tmpl[file]
		if !ok {
			return nil, templateMissing(dir + "/" + file)
		}
		objs, why := render(raw, vars(name, secret), ns)
		if why == "" && slices.ContainsFunc(objs, func(o object) bool { return reserved(o, file) }) {
			why = collides
		}
		if why != "" {
			return nil, templateInvalid(dir+"/"+file, why)
		}
		return objs, nil
	}
	external := []object{{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": accessSecret(rel), "namespace": ns, "labels": labels},
		"spec": map[string]any{"refreshInterval": "1h", "secretStoreRef": map[string]any{"kind": "ClusterSecretStore", "name": rel.Store},
			"target": map[string]any{"name": accessSecret(rel), "creationPolicy": "Owner"},
			"data":   []any{map[string]any{"secretKey": "CANYON_ACCESS_TOKEN", "remoteRef": map[string]any{"key": rel.ID}}}}}}
	if _, ok := tmpl["externalsecret.yaml"]; ok {
		var err error
		if external, err = from("externalsecret.yaml", rel.App, accessSecret(rel)); err != nil {
			return Rendered{}, err
		}
	}
	if err := add("externalsecret.yaml", external); err != nil {
		return Rendered{}, err
	}
	route, err := from("route.yaml", rel.App, "")
	if err != nil {
		return Rendered{}, err
	}
	if err := add("route.yaml", route); err != nil {
		return Rendered{}, err
	}
	out := Rendered{Files: files}
	if out.Address, err = annotated(files["route.yaml"], "canyon.dev/address", dir+"/route.yaml", noAddress, badAddress, place.ValidAddress); err != nil {
		return Rendered{}, err
	}
	for _, r := range slices.SortedFunc(slices.Values(rel.Resources), func(a, b Resource) int { return strings.Compare(a.Name, b.Name) }) {
		file := "resources/" + r.Type + ".yaml"
		objs, err := from(file, r.Name, resourceSecret(rel.App, rel.Env, r.Name))
		if err != nil {
			return Rendered{}, err
		}
		path := "resources/" + r.Name + ".yaml"
		if err := add(path, objs); err != nil {
			return Rendered{}, err
		}
		endpoint, err := annotated(files[path], "canyon.dev/endpoint", dir+"/"+file, noEndpoint, badEndpoint(r.Type), func(s string) bool { return place.ValidEndpoint(s, r.Type) })
		if err != nil {
			return Rendered{}, err
		}
		out.Endpoints = append(out.Endpoints, Endpoint{Name: r.Name, Endpoint: endpoint})
	}
	marker, err := json.MarshalIndent(Canyon{Canyon: rel.Canyon, App: rel.App, Env: rel.Env, Release: rel.ID, GateRun: rel.GateRun, Package: rel.Package, Version: rel.Version}, "", "  ")
	if err != nil {
		return Rendered{}, err
	}
	files["canyon.json"] = append(marker, '\n')
	return out, nil
}

// annotated is the one value of key among raw's objects, read back from the bytes Canyon commits, and valid.
func annotated(raw []byte, key, file, none, invalid string, valid func(string) bool) (string, error) {
	objs, err := reparse(raw)
	if err != nil {
		return "", err
	}
	var found []string
	for _, o := range objs {
		if v, ok := o.annotation(key); ok {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return "", templateInvalid(file, none)
	}
	if !valid(found[0]) {
		return "", templateInvalid(file, invalid)
	}
	return found[0], nil
}

// fromTyped is a typed object as Canyon commits it: its JSON fields, without the status and creation time the
// API server writes, so the file says what Canyon decides and nothing else.
func fromTyped(v any) (object, error) {
	raw, err := kyaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var o object
	if err := kyaml.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	delete(o, "status")
	if meta, ok := o["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	if spec, ok := o["spec"].(map[string]any); ok {
		if tmpl, ok := spec["template"].(map[string]any); ok {
			if meta, ok := tmpl["metadata"].(map[string]any); ok {
				delete(meta, "creationTimestamp")
			}
		}
	}
	return o, nil
}
