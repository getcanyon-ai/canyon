package provisioners

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// Drivers is the platform's choice of driver per resource type and environment. The app never
// chooses: it declares a type, and the environment it deploys to picks the driver here.
type Drivers map[[2]string]Driver

// entry is one type × env line of the config file: {"builtin": true}, or an adapter.
type entry struct {
	Builtin   bool   `json:"builtin"`
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
	CAFile    string `json:"ca_file"`
}

// Load reads the file CANYON_PROVISIONERS names, a JSON object of type → env → entry
// (contracts/provisioner.md). An empty path (the setting unset) is builtin for postgres in both environments. A pair the file
// does not name has no driver. JSON, not TOML: the standard library decodes it strictly.
func Load(path string, builtin Driver) (Drivers, error) {
	if path == "" {
		return Drivers{{"postgres", "preview"}: builtin, {"postgres", "prod"}: builtin}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's setting names the file
	if err != nil {
		return nil, fmt.Errorf("provisioners config: %w", err)
	}
	var file map[string]map[string]entry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err = dec.Decode(&file)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		err = errors.New("data after the object")
	}
	if err != nil {
		return nil, fmt.Errorf("provisioners config %s: not one JSON object of type → env → {builtin | url, token_file, ca_file}: %v", path, err)
	}
	ds := Drivers{}
	for typ, byEnv := range file {
		if _, ok := outputs[typ]; !ok {
			return nil, fmt.Errorf("provisioners config %s: unknown resource type %q", path, typ)
		}
		for env, e := range byEnv {
			if !slices.Contains(envs, env) {
				return nil, fmt.Errorf("provisioners config %s: %s: unknown environment %q (preview or prod)", path, typ, env)
			}
			switch {
			case e.Builtin && typ != "postgres":
				return nil, fmt.Errorf("provisioners config %s: %s %s: the built-in driver serves postgres only: name an adapter url", path, typ, env)
			case e.Builtin && e == entry{Builtin: true} && builtin == nil:
				return nil, fmt.Errorf("provisioners config %s: %s %s: builtin needs CANYON_APP_DATABASE_URL", path, typ, env)
			case e.Builtin && e == entry{Builtin: true}:
				ds[[2]string{typ, env}] = builtin
			case !e.Builtin && e.URL != "":
				d, err := NewHTTP(e.URL, e.TokenFile, e.CAFile)
				if err != nil {
					return nil, fmt.Errorf("provisioners config %s: %s %s: %w", path, typ, env, err)
				}
				ds[[2]string{typ, env}] = d
			default:
				return nil, fmt.Errorf("provisioners config %s: %s %s: either builtin or an adapter url", path, typ, env)
			}
		}
	}
	return ds, nil
}

// Select is the driver for a resource of typ in env.
func (ds Drivers) Select(typ, env string) (Driver, error) {
	d, ok := ds[[2]string{typ, env}]
	if !ok {
		return nil, fmt.Errorf("no driver serves %q in %s", typ, env)
	}
	return d, nil
}

// Named is the configured driver of that name: the one a resource record says made it.
func (ds Drivers) Named(name string) (Driver, bool) {
	for _, d := range ds {
		if d.Name() == name {
			return d, true
		}
	}
	return nil, false
}
