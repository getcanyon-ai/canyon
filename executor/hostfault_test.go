package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A /build the executor's own host fails is Canyon's failure, never the package's (#2489; contracts/executor.md,
// The executor's own failure): short of the build network, on a builder that runs as root, or on a builder whose
// labels cannot be read while the package's project.toml names buildpacks, the stream ends on the executor's line
// with own, and the client answers it as HostFailed, its message whole, never BuildFailed, which the control plane
// records as failure_reason build.
func TestABuildTheHostFailsIsCanyonsOwnFailure(t *testing.T) {
	floorWait = 500 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	app := map[string]string{"package.json": "{}"}
	for name, c := range map[string]struct {
		docker string
		tree   map[string]string
		says   string
	}{
		"no build network": {`case "$1 $2" in "network inspect") exit 1;; "network create") echo 'could not find an available, non-overlapping IPv4 address pool' >&2; exit 1;; esac`,
			app, "build network: network create canyon-builds"},
		"a root builder": {`case "$*" in *"{{.Config.User}}"*) echo 'sha256:` + strings.Repeat("cd", 32) + ` root'; exit 0;; esac
[ "$1 $2" = "run --rm" ] && case "$*" in *" -u") echo 0; exit 0;; esac`, app, "runs its build as root"},
		"the builder's labels unread": {`case "$*" in *"json .Config.Labels"*) echo 'Error: No such image' >&2; exit 1;; esac`,
			map[string]string{"project.toml": "[_]\nschema-version = \"0.2\"\n[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n"},
			"the builder's labels cannot be read, and project.toml names buildpacks: "},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := fakes(t)
			withDocker(t, e, c.docker)
			srv := httptest.NewServer(Handler(e))
			t.Cleanup(srv.Close)
			_, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, c.tree)),
				func(context.Context, string) error { return nil })
			var failed *BuildFailed
			if err == nil || errors.As(err, &failed) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v (read as the package's: %t), want Canyon's own failure saying %q", err, errors.As(err, &failed), c.says)
			}
		})
	}
}
