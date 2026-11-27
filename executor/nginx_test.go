package executor

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// pinnedNginx is #242's project.toml: a static site served by nginx, its version pinned as Paketo documents it.
const pinnedNginx = `[_]
schema-version = "0.2"

[[io.buildpacks.build.env]]
name = "BP_WEB_SERVER"
value = "nginx"

[[io.buildpacks.build.env]]
name = "BP_WEB_SERVER_ROOT"
value = "public"

[[io.buildpacks.build.env]]
name = "BP_NGINX_VERSION"
value = "1.30.*"
`

// A detect that errored is the nginx buildpack's want of a buildpack.yml exactly when paketo-buildpacks/nginx
// 1.3.8 would have reached it: BP_NGINX_VERSION set and not empty, no buildpack.yml, and a tree nginx serves
// (BP_WEB_SERVER nginx, or something at BP_NGINX_CONF_LOCATION), the build values over project.toml's (#242).
func TestNginxWantsYmlOnlyWhereTheBuildpackReadsIt(t *testing.T) {
	static := map[string]string{"public/index.html": "<p>hi</p>\n"}
	with := func(more map[string]string) map[string]string {
		out := maps.Clone(static)
		maps.Copy(out, more)
		return out
	}
	env := func(pairs ...string) string {
		s := "[_]\nschema-version = \"0.2\"\n"
		for ; len(pairs) >= 2; pairs = pairs[2:] { // name, value, ...
			s += "\n[[io.buildpacks.build.env]]\nname = \"" + pairs[0] + "\"\nvalue = \"" + pairs[1] + "\"\n"
		}
		return s
	}
	for name, c := range map[string]struct {
		files  map[string]string
		values map[string]string
		want   bool
	}{
		"#242's app":                         {with(map[string]string{"project.toml": pinnedNginx}), nil, true},
		"with a buildpack.yml":               {with(map[string]string{"project.toml": pinnedNginx, "buildpack.yml": "nginx:\n  version: 1.30.*\n"}), nil, false},
		"no version":                         {with(map[string]string{"project.toml": env("BP_WEB_SERVER", "nginx")}), nil, false},
		"an empty version":                   {with(map[string]string{"project.toml": env("BP_WEB_SERVER", "nginx", "BP_NGINX_VERSION", "")}), nil, false},
		"not nginx's to serve":               {with(map[string]string{"project.toml": env("BP_NGINX_VERSION", "stable")}), nil, false},
		"httpd's to serve":                   {with(map[string]string{"project.toml": env("BP_WEB_SERVER", "httpd", "BP_NGINX_VERSION", "stable")}), nil, false},
		"an nginx.conf at the root":          {map[string]string{"nginx.conf": "events {}\n", "project.toml": env("BP_NGINX_VERSION", "stable")}, nil, true},
		"a conf where the location says":     {map[string]string{"conf/web.conf": "events {}\n", "project.toml": env("BP_NGINX_CONF_LOCATION", "conf/web.conf", "BP_NGINX_VERSION", "stable")}, nil, true},
		"a conf under /workspace":            {map[string]string{"conf/web.conf": "events {}\n", "project.toml": env("BP_NGINX_CONF_LOCATION", "/workspace/conf/web.conf", "BP_NGINX_VERSION", "stable")}, nil, true},
		"a conf outside the tree":            {map[string]string{"etc/nginx.conf": "events {}\n", "project.toml": env("BP_NGINX_CONF_LOCATION", "/etc/nginx.conf", "BP_NGINX_VERSION", "stable")}, nil, false},
		"a conf that climbs out":             {map[string]string{"nginx.conf": "events {}\n", "project.toml": env("BP_NGINX_CONF_LOCATION", "../nginx.conf", "BP_NGINX_VERSION", "stable")}, nil, false},
		"an empty location, the workspace":   {map[string]string{"project.toml": env("BP_NGINX_CONF_LOCATION", "", "BP_NGINX_VERSION", "stable")}, nil, true},
		"no conf where the location says":    {map[string]string{"nginx.conf": "events {}\n", "project.toml": env("BP_NGINX_CONF_LOCATION", "conf/web.conf", "BP_NGINX_VERSION", "stable")}, nil, false},
		"the version a build value":          {with(map[string]string{"project.toml": env("BP_WEB_SERVER", "nginx")}), map[string]string{"BP_NGINX_VERSION": "mainline"}, true},
		"a build value emptying the version": {with(map[string]string{"project.toml": pinnedNginx}), map[string]string{"BP_NGINX_VERSION": ""}, false},
		"no project.toml, all build values":  {static, map[string]string{"BP_WEB_SERVER": "nginx", "BP_NGINX_VERSION": "1.30.*"}, true},
		"a project.toml that does not read":  {with(map[string]string{"project.toml": pinnedNginx + "\n[io.buildpacks.nope]\n"}), nil, false},
	} {
		dir := t.TempDir()
		for rel, body := range c.files {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o750))
			must(t, os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600))
		}
		if got := nginxWantsYmlIn(dir, c.values); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// On Docker, a detect that exited 21 on #242's app ends on the contract's line, the package's failed build saying
// what to change, never a host that doesn't build it; the same exit on a tree with a buildpack.yml, and any 20,
// stays a failed detect (contracts/executor.md, But a detect the nginx buildpack errored on).
func TestADetectNginxErroredOnForWantOfABuildpackYmlSaysWhatToChange(t *testing.T) {
	app := map[string]string{"public/index.html": "<p>hi</p>\n", "project.toml": pinnedNginx}
	withYml := map[string]string{"public/index.html": "<p>hi</p>\n", "project.toml": pinnedNginx, "buildpack.yml": "nginx:\n  version: 1.30.*\n"}
	for name, c := range map[string]struct {
		tree   map[string]string
		status string
		line   bool
	}{
		"21, no buildpack.yml":   {app, "21", true},
		"21, with buildpack.yml": {withYml, "21", false},
		"20, no buildpack.yml":   {app, "20", false},
	} {
		e, dir := fakes(t)
		must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\necho '[detector] parsing version failed: open /workspace/buildpack.yml: no such file or directory'\necho '"+lifecycleFailed+c.status+"'\nexit 1\n"), 0o700)) //nolint:gosec // a test fake must be executable
		_, err := e.Build(t.Context(), BuildRequest{App: "static-site", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, c.tree)), io.Discard)
		var undetected *Undetected
		switch {
		case c.line && (err == nil || errors.As(err, &undetected) || err.Error() != NginxWithoutYml):
			t.Errorf("%s: %#v, want the line %q", name, err, NginxWithoutYml)
		case !c.line && !errors.As(err, &undetected):
			t.Errorf("%s: %#v, want a failed detect", name, err)
		}
	}
}
