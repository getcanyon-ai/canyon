package executor

import (
	"context"
	"errors"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// NginxWithoutYml is the line a detect ends on when the nginx buildpack errored on BP_NGINX_VERSION for want of a
// buildpack.yml (contracts/executor.md, But a detect the nginx buildpack errored on; #242).
const NginxWithoutYml = "this host's nginx buildpack fails on BP_NGINX_VERSION when the app has no buildpack.yml: set the version as nginx.version in a buildpack.yml at the app's root, and remove BP_NGINX_VERSION"

// NginxWantsYml says whether a detect that exited 21 is paketo-buildpacks/nginx 1.3.8's on a tree it would serve:
// its detect reads buildpack.yml whenever BP_NGINX_VERSION is set, and fails on its absence. project is the tree's
// project.toml (nil: none), values the build values, which pack's --env sets over the descriptor's, and exists
// whether a path relative to the tree's root names anything in it. A project.toml that does not read says no: the
// build was refused before it ran.
func NginxWantsYml(project []byte, values map[string]string, exists func(rel string) bool) bool {
	env := map[string]string{}
	if project != nil {
		var w written
		if err := (&Executor{}).read(context.Background(), project, "", &w); err != nil { // with no builder, the file alone
			return false
		}
		if w.IO.Buildpacks.Build != nil {
			for _, v := range w.IO.Buildpacks.Build.Env {
				env[v.Name] = v.Value
			}
		}
	}
	maps.Copy(env, values)
	if env["BP_NGINX_VERSION"] == "" || exists("buildpack.yml") {
		return false
	}
	if env["BP_WEB_SERVER"] == "nginx" {
		return true
	}
	conf, set := env["BP_NGINX_CONF_LOCATION"]
	if !set {
		conf = "nginx.conf"
	}
	if !path.IsAbs(conf) { // the buildpack joins a relative location to the workspace, the tree's root
		conf = path.Join("/workspace", conf)
	}
	conf = path.Clean(conf)
	if conf == "/workspace" {
		return true // the workspace itself
	}
	rel, under := strings.CutPrefix(conf, "/workspace/")
	return under && exists(rel) // one outside the tree is the builder's own files, which Canyon does not read
}

// nginxWantsYmlIn is NginxWantsYml of the tree in dir: a project.toml that cannot be read says no.
func nginxWantsYmlIn(dir string, values map[string]string) bool {
	project, err := os.ReadFile(filepath.Join(dir, "project.toml")) //nolint:gosec // dir is the build's own
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return NginxWantsYml(project, values, func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil
	})
}
