package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// pack never reads the app's project.toml (contracts/executor.md, pack fetches nothing an app names): pack
// decodes it with another TOML library than Canyon's, and the two fold keys differently, so a file Canyon read
// as harmless could hand pack a buildpack's uri. Canyon reads the file instead, every key byte for byte against
// the schemas pack knows, refuses any key it does not know and any buildpack pack would fetch, and writes what it
// read as a descriptor of its own, which pack is handed with --descriptor: pack reads only bytes Canyon wrote.

// buildpack is one buildpack of a descriptor as Canyon writes it: one the builder carries, or an inline script.
type buildpack struct {
	ID      string  `toml:"id"`
	Version string  `toml:"version,omitempty"`
	Script  *script `toml:"script,omitempty"`
}

type script struct {
	API    string `toml:"api"`
	Inline string `toml:"inline"`
	Shell  string `toml:"shell,omitempty"`
}

type envVar struct {
	Name  string `toml:"name"`
	Value string `toml:"value"`
}

type group struct {
	Group []buildpack `toml:"group"`
}

// written is the descriptor Canyon hands pack: schema 0.2, whatever the app's file was.
type written struct {
	Project struct {
		SchemaVersion string `toml:"schema-version"`
	} `toml:"_"`
	IO struct {
		Buildpacks struct {
			Include []string    `toml:"include,omitempty"`
			Exclude []string    `toml:"exclude,omitempty"`
			Group   []buildpack `toml:"group,omitempty"`
			Build   *struct {
				Env []envVar `toml:"env"`
			} `toml:"build,omitempty"`
			Pre  *group `toml:"pre,omitempty"`
			Post *group `toml:"post,omitempty"`
		} `toml:"buildpacks"`
	} `toml:"io"`
}

// table is one TOML table as go-toml/v2 reads it into a map: every key exactly as written.
type table map[string]any

// keys refuses a key of t that is not one of known, byte for byte: pack's decoder would match a key that differs
// in case, or that folds to one of its names, and Canyon reads none such.
func (t table) keys(at string, known ...string) error {
	names := make([]string, 0, len(t))
	for k := range t {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !slices.Contains(known, k) {
			return fmt.Errorf("project.toml: %s%q is not a key Canyon reads (%s)", at, k, strings.Join(known, ", "))
		}
	}
	return nil
}

// roots is every table pack reads at a project.toml's root, in any schema.
var roots = []string{"_", "io", "project", "build", "metadata"}

// foreign leaves alone a key of t that is not one of known, a table of another tool's, unless it matches one of
// names only by case or Unicode folding, as pack's decoder would match it: Canyon reads none such.
func (t table) foreign(at string, known []string, names ...string) error {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the first in order, so the line names the same key every time
	for _, k := range keys {
		if slices.Contains(known, k) {
			continue
		}
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				return fmt.Errorf("project.toml: %s%q is %q in another case or spelling, which Canyon does not read", at, k, n)
			}
		}
	}
	return nil
}

func (t table) str(at, k string) (string, error) {
	v, ok := t[k]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("project.toml: %s%s is not a string", at, k)
	}
	return s, nil
}

func (t table) strs(at, k string) ([]string, error) {
	v, ok := t[k]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("project.toml: %s%s is not a list of strings", at, k)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("project.toml: %s%s is not a list of strings", at, k)
		}
		out = append(out, s)
	}
	return out, nil
}

func (t table) sub(at, k string) (table, error) {
	v, ok := t[k]
	if !ok {
		return table{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("project.toml: %s%s is not a table", at, k)
	}
	return m, nil
}

func (t table) subs(at, k string) ([]table, error) {
	v, ok := t[k]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("project.toml: %s%s is not a list of tables", at, k)
	}
	out := make([]table, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("project.toml: %s%s is not a list of tables", at, k)
		}
		out = append(out, m)
	}
	return out, nil
}

// buildpacks reads one list of buildpacks: each the builder carries, by id and at its version if one is given,
// or an inline script with an id. A uri, a name of the buildpack registry, an image, or any id the builder does
// not carry, pack would fetch from its own process or through the daemon, outside the build's floor.
func buildpacks(at string, entries []table, carried map[string]bool) ([]buildpack, error) {
	var out []buildpack
	for i, e := range entries {
		at := fmt.Sprintf("%s[%d].", at, i)
		if err := e.keys(at, "id", "version", "uri", "script"); err != nil {
			return nil, err
		}
		if _, ok := e["uri"]; ok {
			uri, _ := e.str(at, "uri")
			return nil, fmt.Errorf("project.toml: buildpack %q has a uri, which pack would fetch outside the build's network: name a buildpack of the builder by its id, or an inline script", uri)
		}
		id, err := e.str(at, "id")
		if err != nil {
			return nil, err
		}
		version, err := e.str(at, "version")
		if err != nil {
			return nil, err
		}
		s, err := e.sub(at, "script")
		if err != nil {
			return nil, err
		}
		if err := s.keys(at+"script.", "api", "inline", "shell"); err != nil {
			return nil, err
		}
		inline, err := s.str(at+"script.", "inline")
		if err != nil {
			return nil, err
		}
		bp := buildpack{ID: id, Version: version}
		locator := id
		if version != "" {
			locator += "@" + version
		}
		switch {
		case id != "" && inline != "":
			api, err := s.str(at+"script.", "api")
			if err != nil {
				return nil, err
			}
			shell, err := s.str(at+"script.", "shell")
			if err != nil {
				return nil, err
			}
			bp.Script = &script{API: api, Inline: inline, Shell: shell}
		case len(s) > 0:
			return nil, fmt.Errorf("project.toml: buildpack %q has a script that is no inline script with an id", locator)
		case id == "" || !carried[locator]: // with a version, pack takes the builder's only at that version
			return nil, fmt.Errorf("project.toml: buildpack %q is not one of the builder's, and pack would fetch it outside the build's network: name a buildpack of the builder by its id, or an inline script", locator)
		}
		out = append(out, bp)
	}
	return out, nil
}

func envVars(at string, entries []table) ([]envVar, error) {
	var out []envVar
	for i, e := range entries {
		at := fmt.Sprintf("%s[%d].", at, i)
		if err := e.keys(at, "name", "value"); err != nil {
			return nil, err
		}
		name, err := e.str(at, "name")
		if err != nil {
			return nil, err
		}
		value, err := e.str(at, "value")
		if err != nil {
			return nil, err
		}
		out = append(out, envVar{Name: name, Value: value})
	}
	return out, nil
}

// descriptor writes the descriptor pack is handed, from the tree's project.toml if it has one, into a directory of
// its own, so a buildpack named by a relative path finds nothing of the tree, and answers its path and the call
// that removes it. A project.toml that does not parse, carries a key Canyon does not read, or names a buildpack
// pack would fetch refuses the build (contracts/executor.md, pack fetches nothing an app names).
func (e *Executor) descriptor(ctx context.Context, dir, builder string) (string, func(), error) {
	var w written
	w.Project.SchemaVersion = "0.2"
	b, err := os.ReadFile(filepath.Join(dir, "project.toml")) //nolint:gosec // dir is the build's own
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", nil, fmt.Errorf("project.toml: %w", err)
	default:
		if err := e.read(ctx, b, builder, &w); err != nil {
			return "", nil, err
		}
	}
	// Canyon's own descriptor, written by Canyon from what it read: a failure here is the host's (#2537).
	out, err := toml.Marshal(w)
	if err != nil {
		return "", nil, &HostFailed{Message: "the descriptor: " + err.Error()}
	}
	at, err := os.MkdirTemp("", "canyon-descriptor-")
	if err != nil {
		return "", nil, &HostFailed{Message: "the descriptor: " + err.Error()}
	}
	remove := func() { _ = os.RemoveAll(at) }
	path := filepath.Join(at, "project.toml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		remove()
		return "", nil, &HostFailed{Message: "the descriptor: " + err.Error()}
	}
	return path, remove, nil
}

// read reads the app's project.toml into w, schema 0.1, 0.2 or 0.3 as pack reads it, every key exactly.
func (e *Executor) read(ctx context.Context, b []byte, builder string, w *written) error {
	var root table
	if err := toml.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("project.toml: %w", err)
	}
	project, err := root.sub("", "_")
	if err != nil {
		return err
	}
	schema, err := project.str("_.", "schema-version")
	if err != nil {
		return err
	}
	labels := []string{"id", "name", "version", "authors", "licenses", "documentation-url", "source-url", "metadata"} // read by pack for the image's labels alone
	var (
		bps                 table
		grp, pre, post, env []table
		include, exclude    []string
	)
	switch schema {
	case "", "0.1": // pack's default, schema 0.1: project, build and metadata, and _ for its version alone
		if err := root.foreign("", []string{"_", "project", "build", "metadata"}, roots...); err != nil {
			return err
		}
		if err := project.keys("_.", "schema-version"); err != nil {
			return err
		}
		p, err := root.sub("", "project")
		if err != nil {
			return err
		}
		if err := p.keys("project.", labels[:len(labels)-1]...); err != nil {
			return err
		}
		if bps, err = root.sub("", "build"); err != nil {
			return err
		}
		if err := bps.keys("build.", "include", "exclude", "buildpacks", "env", "builder", "pre", "post"); err != nil {
			return err
		}
		if grp, err = bps.subs("build.", "buildpacks"); err != nil {
			return err
		}
		if env, err = bps.subs("build.", "env"); err != nil {
			return err
		}
	case "0.2", "0.3":
		if err := root.foreign("", []string{"_", "io"}, roots...); err != nil {
			return err
		}
		if err := project.keys("_.", append([]string{"schema-version"}, labels...)...); err != nil {
			return err
		}
		io, err := root.sub("", "io")
		if err != nil {
			return err
		}
		if err := io.foreign("io.", []string{"buildpacks"}, "buildpacks"); err != nil {
			return err
		}
		if bps, err = io.sub("io.", "buildpacks"); err != nil {
			return err
		}
		known := []string{"include", "exclude", "group", "build", "builder", "pre", "post"}
		if schema == "0.2" {
			known = append(known, "env") // 0.2's old [[io.buildpacks.env.build]], read when build.env is not given
		}
		if err := bps.keys("io.buildpacks.", known...); err != nil {
			return err
		}
		if grp, err = bps.subs("io.buildpacks.", "group"); err != nil {
			return err
		}
		build, err := bps.sub("io.buildpacks.", "build")
		if err != nil {
			return err
		}
		if err := build.keys("io.buildpacks.build.", "env"); err != nil {
			return err
		}
		if env, err = build.subs("io.buildpacks.build.", "env"); err != nil {
			return err
		}
		old, err := bps.sub("io.buildpacks.", "env")
		if err != nil {
			return err
		}
		if err := old.keys("io.buildpacks.env.", "build"); err != nil {
			return err
		}
		if _, given := build["env"]; !given {
			if env, err = old.subs("io.buildpacks.env.", "build"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("project.toml: schema-version %q is not one Canyon reads (0.1, 0.2, 0.3)", schema)
	}
	at, grpAt := "io.buildpacks.", "io.buildpacks.group"
	if schema == "" || schema == "0.1" {
		at, grpAt = "build.", "build.buildpacks"
	}
	for _, side := range []struct {
		name string
		into *[]table
	}{{"pre", &pre}, {"post", &post}} {
		t, err := bps.sub(at, side.name)
		if err != nil {
			return err
		}
		if err := t.keys(at+side.name+".", "group"); err != nil {
			return err
		}
		if *side.into, err = t.subs(at+side.name+".", "group"); err != nil {
			return err
		}
	}
	if include, err = bps.strs(at, "include"); err != nil {
		return err
	}
	if exclude, err = bps.strs(at, "exclude"); err != nil {
		return err
	}
	bpsOut := &w.IO.Buildpacks
	bpsOut.Include, bpsOut.Exclude = include, exclude
	if vars, err := envVars(at+"env", env); err != nil {
		return err
	} else if vars != nil {
		bpsOut.Build = &struct {
			Env []envVar `toml:"env"`
		}{vars}
	}
	if len(grp)+len(pre)+len(post) == 0 {
		return nil
	}
	carried, err := e.carried(ctx, builder)
	if err != nil {
		return &HostFailed{Message: "the builder's labels cannot be read, and project.toml names buildpacks: " + err.Error()} // the host's (#2489)
	}
	if bpsOut.Group, err = buildpacks(grpAt, grp, carried); err != nil {
		return err
	}
	for _, side := range []struct {
		from []table
		into **group
		at   string
	}{{pre, &bpsOut.Pre, at + "pre.group"}, {post, &bpsOut.Post, at + "post.group"}} {
		list, err := buildpacks(side.at, side.from, carried)
		if err != nil {
			return err
		}
		if list != nil {
			*side.into = &group{Group: list}
		}
	}
	return nil
}
