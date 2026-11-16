package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/getcanyon-ai/canyon/executor"
)

// hub is a registry holding Paketo's images as a bump finds them: each base's pinned image and its newest
// at latest, a builder ordering nodejs and procfile, and those buildpacks at the versions each builder names.
type hub struct {
	t    *testing.T
	host string
	opts []crane.Option
}

func newHub(t *testing.T) hub {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return hub{t: t, host: strings.TrimPrefix(srv.URL, "http://"), opts: []crane.Option{crane.Insecure}}
}

// push puts an image labelled labels at repo:tag and answers its digest.
func (h hub) push(repo, tag string, labels map[string]string) string {
	h.t.Helper()
	img, err := mutate.Config(empty.Image, v1.Config{Labels: labels})
	if err == nil {
		err = crane.Push(img, h.host+"/paketobuildpacks/"+repo+":"+tag, h.opts...)
	}
	d, derr := img.Digest()
	if err = errors.Join(err, derr); err != nil {
		h.t.Fatal(err)
	}
	return d.String()
}

// order is a builder's label ordering each "name version" a group of its own.
func order(buildpacks ...string) map[string]string {
	var groups []string
	for _, bp := range buildpacks {
		name, version, _ := strings.Cut(bp, " ")
		groups = append(groups, fmt.Sprintf(`{"group":[{"id":"paketo-buildpacks/%s","version":"%s"}]}`, name, version))
	}
	return map[string]string{"io.buildpacks.buildpack.order": "[" + strings.Join(groups, ",") + "]"}
}

func buildpackage(name, version string) map[string]string {
	return map[string]string{"io.buildpacks.buildpackage.metadata": fmt.Sprintf(`{"id":"paketo-buildpacks/%s","version":"%s"}`, name, version)}
}

// stale is the patch journey's older run image, pinned apart from Canyon's own.
var stale = "sha256:" + strings.Repeat("5", 64)

// tree is a repository pinning old: the bases in the executor's Dockerfiles, the buildpacks in the
// ClusterStore, the builder again in a test, as Canyon's own pins each; and stale, apart, in the patch journey.
func tree(t *testing.T, old map[string]string) string {
	root := t.TempDir()
	files := map[string]string{
		"executor/builder.Dockerfile": "FROM docker.io/paketobuildpacks/ubuntu-noble-builder@" + old[builder] + "\n",
		"executor/run.Dockerfile":     "FROM docker.io/paketobuildpacks/ubuntu-noble-run@" + old[run] + "\nUSER 1002:1001\n",
		"executor/build.Dockerfile":   "FROM docker.io/paketobuildpacks/ubuntu-noble-build@" + old[build] + "\n",
		"executor/executor_test.go":   "const want = \"FROM docker.io/paketobuildpacks/ubuntu-noble-builder@" + old[builder] + "\"\n",
		"e2e/oidc/patch_test.go":      "const oldRun = \"docker.io/paketobuildpacks/ubuntu-noble-run@" + stale + "\"\n", // apart
		"deploy/k8s/builds.yaml": "    - {image: docker.io/paketobuildpacks/nodejs@" + old["nodejs"] + "}\n" +
			"    - {image: docker.io/paketobuildpacks/procfile@" + old["procfile"] + "}\n",
	}
	for f, text := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // the test's own repository
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

// scans is each fixture app's image scan holding libssl3 in the run image's layer, npm in node-engine's,
// and in npm-install's ms, the app's own, beside packit, a module of the buildpack's own binary.
func scans(libssl3, npm, ms, packit string) map[string]executor.ScanResult {
	layer := func(n string) string { return "sha256:" + strings.Repeat(n, 64) }
	label := fmt.Sprintf(`{"runImage":{"topLayer":%q},"launcher":{"sha":%q},"buildpacks":[{"key":"paketo-buildpacks/node-engine","layers":{"node":{"sha":%q}}},{"key":"paketo-buildpacks/npm-install","layers":{"launch-modules":{"sha":%q}}}]}`,
		layer("1"), layer("4"), layer("2"), layer("3"))
	component := func(purl, name, version, pkgType, in string) string {
		return fmt.Sprintf(`{"type":"library","name":%q,"version":%q,"purl":%q,"properties":[{"name":"aquasecurity:trivy:LayerDiffID","value":%q},{"name":"aquasecurity:trivy:PkgType","value":%q}]}`,
			name, version, purl, layer(in), pkgType)
	}
	sbom := `{"bomFormat":"CycloneDX","components":[` + strings.Join([]string{
		`{"type":"operating-system","name":"ubuntu","version":"24.04"}`,
		component("pkg:deb/ubuntu/libssl3@"+libssl3+"?arch=arm64&distro=ubuntu-24.04", "libssl3", libssl3, "ubuntu", "1"),
		component("pkg:npm/npm@"+npm, "npm", npm, "node-pkg", "2"),
		component("pkg:npm/ms@"+ms, "ms", ms, "node-pkg", "3"),
		component("pkg:golang/github.com/paketo-buildpacks/packit/v2@"+packit, "github.com/paketo-buildpacks/packit/v2", packit, "gobinary", "3"),
	}, ",") + `]}`
	out := map[string]executor.ScanResult{}
	for _, app := range apps {
		out[app] = executor.ScanResult{Tool: "trivy", SBOM: json.RawMessage(sbom), Lifecycle: label, DiffIDs: []string{layer("1"), layer("2"), layer("3"), layer("4")}}
	}
	return out
}

// paketo pushes Paketo as it stands: the pins (returned) and the newest images, which move every base and
// nodejs 10.13.0 → 10.14.2, procfile staying at 5.11.0.
func paketo(h hub) (old, now map[string]string) {
	old, now = map[string]string{}, map[string]string{}
	for _, base := range []string{run, build} {
		old[base] = h.push(base, "0.0.1", map[string]string{"v": base + " 0.0.1"})
		now[base] = h.push(base, "latest", map[string]string{"v": base + " 0.0.2"})
	}
	old[builder] = h.push(builder, "0.0.1", order("nodejs 10.13.0", "procfile 5.11.0"))
	now[builder] = h.push(builder, "latest", order("nodejs 10.14.2", "procfile 5.11.0"))
	old["nodejs"] = h.push("nodejs", "10.13.0", buildpackage("nodejs", "10.13.0"))
	now["nodejs"] = h.push("nodejs", "10.14.2", buildpackage("nodejs", "10.14.2"))
	old["procfile"] = h.push("procfile", "5.11.0", buildpackage("procfile", "5.11.0"))
	now["procfile"] = old["procfile"]
	return old, now
}

// The bump against old pins, as the workflow's dry run makes it: every pin moves to Paketo's newest in
// every file naming it but the patch journey's older run image, pinned apart, and the issue's body links the branch's pull request and names what moved in the fixture apps' images — libssl3 in the run
// image, npm in node-engine — and each pin, never the app's own ms or a pin that stayed.
func TestBumpDryRunNamesTheDiff(t *testing.T) {
	h := newHub(t)
	old, now := paketo(h)
	root := tree(t, old)
	calls := 0
	b := bumper{root: root, hub: h.host, opts: h.opts, sbom: func(_ context.Context, root string) (map[string]executor.ScanResult, error) {
		calls++
		file, _ := os.ReadFile(filepath.Join(root, "executor", "run.Dockerfile")) //nolint:gosec // test dir
		if strings.Contains(string(file), now[run]) {
			return scans("3.0.13-0ubuntu3.5", "10.9.0", "2.1.3", "v2.26.0"), nil
		}
		return scans("3.0.13-0ubuntu3.4", "10.8.2", "2.1.2", "v2.25.1"), nil
	}}
	r, err := b.bump(context.Background())
	if err != nil || r == nil || calls != 2 {
		t.Fatalf("bump: %v, %d scans of the fixture apps; want both sides", err, calls)
	}
	title, body := r.issue("acme/canyon")
	if title != "chore(ci): Paketo pins are behind (3 fixes)" {
		t.Errorf("the issue's title is %q", title)
	}
	for _, want := range []string{
		"https://github.com/acme/canyon/compare/main..." + r.branch() + "?expand=1\n",
		"**os**\n\n- `libssl3` 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5\n",
		"**runtime**\n\n- `npm` 10.8.2 → 10.9.0\n\n<details><summary>Go modules of its binaries (1)</summary>\n\n- `github.com/paketo-buildpacks/packit/v2` v2.25.1 → v2.26.0\n\n</details>\n",
		fmt.Sprintf("- `paketobuildpacks/ubuntu-noble-builder` `%.19s` → `%.19s`\n", old[builder], now[builder]),
		fmt.Sprintf("- `paketobuildpacks/nodejs` 10.13.0 → 10.14.2 (`%.19s` → `%.19s`)\n", old["nodejs"], now["nodejs"]),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "`ms`") || strings.Contains(body, "procfile") {
		t.Errorf("the body names the app's own ms or the procfile pin that stayed:\n%s", body)
	}
	for f, repo := range map[string]string{"executor/builder.Dockerfile": builder, "executor/executor_test.go": builder, "executor/run.Dockerfile": run, "executor/build.Dockerfile": build, "deploy/k8s/builds.yaml": "nodejs"} {
		text, _ := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // test dir
		if !strings.Contains(string(text), "paketobuildpacks/"+repo+"@"+now[repo]) || strings.Contains(string(text), old[repo]) {
			t.Errorf("%s does not pin paketobuildpacks/%s at Paketo's newest %s:\n%s", f, repo, now[repo], text)
		}
	}
	if text, _ := os.ReadFile(filepath.Join(root, "e2e", "oidc", "patch_test.go")); !strings.Contains(string(text), "ubuntu-noble-run@"+stale) { //nolint:gosec // test dir
		t.Errorf("the bump moved the patch journey's older run image, pinned apart:\n%s", text)
	}
}

// A bump that cannot resolve a digest, finds the ClusterStore is not the builder's buildpacks, or cannot
// scan the new side fails with no body; one that resolved nothing neither built nor rewrote.
func TestBumpFailsClosed(t *testing.T) {
	for name, c := range map[string]struct {
		paketo  func(h hub, old map[string]string)
		sbomErr bool
	}{
		"no newest run image": {paketo: func(h hub, _ map[string]string) {
			if err := crane.Delete(h.host+"/paketobuildpacks/"+run+":latest", h.opts...); err != nil {
				h.t.Fatal(err)
			}
		}},
		"a store source is another buildpack": {paketo: func(h hub, old map[string]string) {
			old["nodejs"] = h.push("nodejs", "1.0.0", buildpackage("ruby", "1.0.0"))
		}},
		"the builder orders no store source": {paketo: func(h hub, old map[string]string) {
			old[builder] = h.push(builder, "0.0.1", order("nodejs 10.13.0")) // the store pins procfile besides
			h.push(builder, "latest", order("nodejs 10.14.2"))
		}},
		"the newest builder orders another buildpack": {paketo: func(h hub, _ map[string]string) {
			h.push(builder, "latest", order("nodejs 10.14.2", "procfile 5.11.0", "ruby 3.3.0"))
		}},
		"an image scan fails": {sbomErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHub(t)
			old, _ := paketo(h)
			if c.paketo != nil {
				c.paketo(h, old)
			}
			root := tree(t, old)
			scanned := false
			b := bumper{root: root, hub: h.host, opts: h.opts, sbom: func(context.Context, string) (map[string]executor.ScanResult, error) {
				if c.sbomErr && scanned { // the new side's
					return nil, errors.New("not finished: timeout")
				}
				scanned = true
				return scans("1", "1", "1", "v1"), nil
			}}
			r, err := b.bump(context.Background())
			if err == nil || r != nil {
				t.Fatalf("bump: %v, %v; want an error and no bump", err, r)
			}
			text, _ := os.ReadFile(filepath.Join(root, "executor", "run.Dockerfile")) //nolint:gosec // test dir
			if !c.sbomErr && (scanned || !strings.Contains(string(text), old[run])) {
				t.Errorf("scanned %v, run.Dockerfile %s: a bump that resolved nothing built or rewrote", scanned, text)
			}
			t.Log(err)
		})
	}
}
