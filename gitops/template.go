package gitops

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	kyaml "sigs.k8s.io/yaml"
)

// The template contract (contracts/routes.md, The deploy repo, Templates).
const (
	templateMax = 64 << 10 // bytes
	objectsMax  = 10       // documents, an empty one counted
)

// coreTags are YAML's core schema: a template that spells any other tag is refused.
var coreTags = []string{"!!str", "!!int", "!!float", "!!bool", "!!null", "!!map", "!!seq"}

// clusterScopedKinds are the kinds Canyon knows are not namespaced: a template never makes one, so a deploy
// repo can never grant a release anything beyond its namespace.
var clusterScopedKinds = []string{"Namespace", "Node", "PersistentVolume", "StorageClass", "ClusterRole", "ClusterRoleBinding",
	"CustomResourceDefinition", "APIService", "PriorityClass", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration",
	"ClusterSecretStore", "ClusterIssuer", "ClusterExternalSecret"}

// variable is ${name}: what a scalar value may name, and nothing else.
var variable = regexp.MustCompile(`\$\{([^}]*)\}`)

// object is one Kubernetes object as parsed, its kind and name read off it.
type object map[string]any

func (o object) kind() string { s, _ := o["kind"].(string); return s }

func (o object) name() string {
	m, _ := o["metadata"].(map[string]any)
	s, _ := m["name"].(string)
	return s
}

func (o object) annotation(key string) (string, bool) {
	m, _ := o["metadata"].(map[string]any)
	a, _ := m["annotations"].(map[string]any)
	s, ok := a[key].(string)
	return s, ok
}

// render turns one template into its objects: parsed as nodes, every check on the nodes before any value
// is substituted, then each document a map with every ${…} replaced in scalar values alone, the result always
// a string, and metadata.namespace pinned to ns. The first reason it fails is the problem's.
func render(raw []byte, vars map[string]string, ns string) ([]object, string) {
	if len(raw) > templateMax {
		return nil, tooBig
	}
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, notYAML
		}
		docs = append(docs, &doc)
	}
	if len(docs) > objectsMax {
		return nil, tooMany
	}
	for _, check := range []struct {
		bad    func(n *yaml.Node, key bool) bool
		reason string
	}{
		{func(n *yaml.Node, _ bool) bool { return n.Anchor != "" || n.Kind == yaml.AliasNode }, anchored},
		{func(n *yaml.Node, key bool) bool { return key && (n.Value == "<<" || n.Tag == "!!merge") }, mergeKey},
		{repeats, repeatedKey},
		{func(n *yaml.Node, _ bool) bool {
			return n.Style&yaml.TaggedStyle != 0 && !slices.Contains(coreTags, n.Tag)
		}, foreignTag},
		{func(n *yaml.Node, key bool) bool { return key && strings.Contains(n.Value, "${") }, keyVariable},
		{func(n *yaml.Node, key bool) bool { return !key && n.Kind == yaml.ScalarNode && unknown(n.Value, vars) }, unknownVar},
	} {
		for _, doc := range docs {
			if walk(doc, false, check.bad) {
				return nil, check.reason
			}
		}
	}
	var out []object
	for _, doc := range docs {
		if len(doc.Content) == 0 || doc.Content[0].Kind == yaml.ScalarNode && doc.Content[0].Tag == "!!null" { // an empty document: counted, nothing made
			continue
		}
		v, ok := valueOf(doc.Content[0], vars)
		m, isMap := v.(map[string]any)
		if !ok || !isMap {
			return nil, unnamed
		}
		o := object(m)
		meta, _ := o["metadata"].(map[string]any)
		if api, _ := o["apiVersion"].(string); api == "" || o.kind() == "" || meta == nil || o.name() == "" {
			return nil, unnamed
		}
		if n, set := meta["namespace"]; set && n != ns {
			return nil, otherNS
		}
		meta["namespace"] = ns
		if slices.Contains(clusterScopedKinds, o.kind()) {
			return nil, clusterScoped
		}
		out = append(out, o)
	}
	return out, ""
}

// walk is whether bad holds for n or any node under it, key telling a mapping's keys from its values.
func walk(n *yaml.Node, key bool, bad func(*yaml.Node, bool) bool) bool {
	if bad(n, key) {
		return true
	}
	for i, c := range n.Content {
		if walk(c, n.Kind == yaml.MappingNode && i%2 == 0, bad) {
			return true
		}
	}
	return false
}

// repeats is whether a mapping names one key twice: the YAML is ambiguous, and which one wins is the parser's.
func repeats(n *yaml.Node, _ bool) bool {
	if n.Kind != yaml.MappingNode {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}

// unknown is whether s names a variable Canyon does not have, or opens ${ without closing it.
func unknown(s string, vars map[string]string) bool {
	for _, m := range variable.FindAllStringSubmatch(s, -1) {
		if _, ok := vars[m[1]]; !ok {
			return true
		}
	}
	return strings.Count(s, "${") != len(variable.FindAllString(s, -1))
}

// valueOf is a node as a value: a mapping by its scalar keys, a sequence, and a scalar by its core tag — a
// scalar holding ${…} always the string the variables make — false for a key that is not a scalar.
func valueOf(n *yaml.Node, vars map[string]string) (any, bool) {
	switch n.Kind {
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Kind != yaml.ScalarNode {
				return nil, false
			}
			v, ok := valueOf(n.Content[i+1], vars)
			if !ok {
				return nil, false
			}
			m[n.Content[i].Value] = v
		}
		return m, true
	case yaml.SequenceNode:
		s := []any{}
		for _, c := range n.Content {
			v, ok := valueOf(c, vars)
			if !ok {
				return nil, false
			}
			s = append(s, v)
		}
		return s, true
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "${") {
			return variable.ReplaceAllStringFunc(n.Value, func(m string) string { return vars[m[2:len(m)-1]] }), true
		}
		switch n.Tag {
		case "!!int":
			if i, err := strconv.ParseInt(n.Value, 0, 64); err == nil {
				return i, true
			}
		case "!!float":
			var f float64
			if n.Decode(&f) == nil {
				return f, true
			}
		case "!!bool":
			var b bool
			if n.Decode(&b) == nil {
				return b, true
			}
		case "!!null":
			return nil, true
		}
		return n.Value, true
	}
	return nil, false
}

// serialize is objects as YAML, keys sorted, each a document: what Canyon commits.
func serialize(objs []object) ([]byte, error) {
	var b bytes.Buffer
	for i, o := range objs {
		y, err := kyaml.Marshal(map[string]any(o))
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(y)
	}
	return b.Bytes(), nil
}

// reparse is the committed bytes read back as objects, the way Argo CD or Flux reads them: what an
// annotation check reads, never the value before it was written.
func reparse(raw []byte) ([]object, error) {
	var out []object
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var o map[string]any // never object: yaml decodes every mapping below into the type it is given
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
}
