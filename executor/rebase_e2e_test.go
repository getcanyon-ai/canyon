//go:build e2e

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRebaseE2EDockerLayersAboveRunImageIdentical rebases a real build onto a newer canyon-run — another
// file, one layer more, named canyon-run:noble as every build of the file is — with the real pack
// (contracts/executor.md, POST /rebase):
// the layers above the run image's top layer are the same, in order, the ones below are not, the build
// metadata, config and stack are unchanged, and the image the request named keeps its tag through the
// rebase and through the tag cleanup of a /run that keeps it. The rebased image serves the app.
func TestRebaseE2EDockerLayersAboveRunImageIdentical(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "vite-spa", Package: "pkg_e2e2"}, treeOf(t, filepath.Join("testdata", "apps", "vite-spa")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	old := strings.TrimPrefix(res.Image, "canyon/vite-spa@")
	defer exec.Command("docker", "image", "rm", "-f", old).Run()
	saved := runImage
	newer := &scanImage{dockerfile: "FROM " + saved.repo + "\nUSER root\nRUN echo patched > /etc/canyon-e2e-patch\nUSER 1002:1001\n", repo: "canyon-run:e2e-" + nonce()}
	runImage = newer
	run, err := e.RunImageID(ctx) // canyon-run:noble moves onto the newer build, as an executor with a changed file moves it
	must(t, err)
	defer func() {
		runImage = saved
		if _, err := e.RunImageID(context.Background()); err != nil { // canyon-run:noble back onto this file's build
			t.Errorf("canyon-run:noble left on %s: %v", newer.repo, err)
		}
		_ = exec.Command("docker", "image", "rm", newer.repo).Run()
	}()

	rebased, err := e.Rebase(ctx, RebaseRequest{App: "vite-spa", Package: "pkg_e2e2", Image: res.Image, RunImage: run})
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(rebased.Image, "canyon/vite-spa@")).Run()
	b, a := rebased.Before, rebased.After
	top := func(f RebaseFacts) int { return slices.Index(f.DiffIDs, f.TopLayer) }
	if top(b) < 0 || top(a) < 0 || !slices.Equal(b.DiffIDs[top(b)+1:], a.DiffIDs[top(a)+1:]) {
		t.Fatalf("the layers above the run image changed:\nbefore %v above %s\nafter  %v above %s", b.DiffIDs, b.TopLayer, a.DiffIDs, a.TopLayer)
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RootFS.Layers}}", newer.repo).Output()
	must(t, err)
	var layers []string
	must(t, json.Unmarshal(out, &layers))
	if !slices.Equal(a.DiffIDs[:top(a)+1], layers) || slices.Equal(b.DiffIDs[:top(b)+1], layers) {
		t.Fatalf("the layers below are not the newer run image's %v:\nbefore %v\nafter  %v", layers, b.DiffIDs, a.DiffIDs)
	}
	if a.BuildMetadata != b.BuildMetadata || a.Config != b.Config || a.StackID != b.StackID || a.RunImage != run || b.RunImage == run || rebased.Image == res.Image {
		t.Fatalf("before %+v\nafter %+v\nimage %s, was %s", b, a, rebased.Image, res.Image)
	}

	held := func() []string {
		tags, err := e.images(ctx, "vite-spa")
		must(t, err)
		var ids []string
		for _, tag := range tags {
			ids = append(ids, tag.id)
		}
		return ids
	}
	if !slices.Contains(held(), old) {
		t.Fatalf("the image the rebase named lost its tag: %v", held())
	}
	started, err := e.Run(ctx, RunRequest{App: "vite-spa", Target: "preview", Image: rebased.Image, Network: "canyon-app-vite-spa", Keep: Digests{old}})
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "vite-spa", "preview") }()
	e.Collect(ctx, "vite-spa", Digests{old})
	e.collecting.Wait()
	if ids := held(); !slices.Contains(ids, old) || !slices.Contains(ids, strings.TrimPrefix(rebased.Image, "canyon/vite-spa@")) {
		t.Fatalf("after a /run keeping it, the tags of %s and %s: %v", old, rebased.Image, ids)
	}
	if body := get(ctx, t, "vite-spa", started, "/"); !strings.Contains(body, `src="/assets/index-`) {
		t.Fatalf("the rebased image does not serve the app:\n%s", body)
	}
}

// TestRebaseE2EOntoCanyonImageRunByDigest builds an app onto CANYON_IMAGE_RUN named by digest — canyon-run pushed to
// a registry of the test's own — then moves CANYON_IMAGE_RUN to a newer digest of that repository, canyon-run and one
// layer more, and rebases through POST /rebase with the real pack (contracts/executor.md, POST /rebase; design/
// PATCHING.md §4.1, #2363). The lifecycle records that repository in runImage.image and the daemon's ID in
// runImage.reference; pack takes the newer digest, never forced; the layers above the run image's are the same and
// the ones below are the newer's; after.run_image is GET /builder's. The newer image under another name — another
// repository by digest, or this one by a tag — is refused 409 another_run_image before pack runs.
func TestRebaseE2EOntoCanyonImageRunByDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	registry := "canyon-e2e-registry-" + nonce() // the daemon pulls by digest only from a registry; loopback's it trusts over http
	docker("run", "-d", "--rm", "--name", registry, "-p", "127.0.0.1:5000:5000", "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373")
	defer exec.Command("docker", "rm", "-f", registry).Run()
	for i := 0; ; i++ {
		res, err := http.Get("http://127.0.0.1:5000/v2/") //nolint:noctx // a loopback probe, bounded by the loop
		if err == nil {
			_ = res.Body.Close()
			break
		}
		if i == 30 {
			t.Fatalf("the registry never answered: %v", err)
		}
		time.Sleep(time.Second)
	}
	repo := "localhost:5000/canyon-e2e-run"
	pushed := func(id, name string) string { // name, pushed, by digest
		t.Helper()
		docker("tag", id, name)
		docker("push", "-q", name)
		repository, _, _ := strings.Cut(strings.TrimPrefix(name, "localhost:5000/"), ":")
		for _, d := range strings.Fields(docker("image", "inspect", "--format", `{{join .RepoDigests " "}}`, name)) {
			if strings.HasPrefix(d, "localhost:5000/"+repository+"@sha256:") {
				return d
			}
		}
		t.Fatalf("%s pushed, with no digest of its repository", name)
		return ""
	}
	base, err := runImage.built(ctx, "docker")
	must(t, err)
	next := &scanImage{dockerfile: "FROM " + runImage.repo + "\nUSER root\nRUN echo patched > /etc/canyon-e2e-patch\nUSER 1002:1001\n", repo: "canyon-run:e2e-digest-" + nonce()}
	nextID, err := next.built(ctx, "docker")
	must(t, err)
	older, newer, other := pushed(base, repo+":older"), pushed(nextID, repo+":newer"), pushed(nextID, "localhost:5000/canyon-e2e-other:newer")
	defer exec.Command("docker", "image", "rm", repo+":older", repo+":newer", "localhost:5000/canyon-e2e-other:newer", next.repo).Run()

	e := &Executor{Join: []string{}, RunImage: older}
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	builder := func() string { // GET /builder's run_image
		t.Helper()
		res, err := srv.Client().Get(srv.URL + "/builder")
		must(t, err)
		defer res.Body.Close()
		var answer struct {
			RunImage string `json:"run_image"`
		}
		if err := json.NewDecoder(res.Body).Decode(&answer); err != nil || res.StatusCode != http.StatusOK || !imageIDRe.MatchString(answer.RunImage) {
			t.Fatalf("GET /builder under CANYON_IMAGE_RUN %s: %d %v %+v", e.RunImage, res.StatusCode, err, answer)
		}
		return answer.RunImage
	}
	was := builder()
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "vite-spa", Package: "pkg_e2e3"}, treeOf(t, filepath.Join("testdata", "apps", "vite-spa")), &log)
	if err != nil {
		t.Fatalf("build onto %s: %v\n%s", older, err, log.String())
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/vite-spa@")).Run()
	var md struct {
		RunImage struct{ Image, Reference string } `json:"runImage"`
	}
	must(t, json.Unmarshal([]byte(docker("image", "inspect", "--format", `{{index .Config.Labels "`+lifecycleLabel+`"}}`, strings.TrimPrefix(res.Image, "canyon/vite-spa@"))), &md)) // by its ID: the digest a build answers is the daemon's ID, no repository's
	t.Logf("the lifecycle recorded runImage.image %q, runImage.reference %q, for --run-image %s", md.RunImage.Image, md.RunImage.Reference, older)
	if name, err := LifecycleName(md.RunImage.Image); err != nil || name != "localhost:5000/canyon-e2e-run:latest" || "sha256:"+md.RunImage.Reference != was {
		t.Fatalf("built onto %s (GET /builder %s), the lifecycle recorded runImage.image %q (%q, %v), runImage.reference %q", older, was, md.RunImage.Image, name, err, md.RunImage.Reference)
	}

	rebase := func(runImage string) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(RebaseRequest{App: "vite-spa", Package: "pkg_e2e3", Image: res.Image, RunImage: runImage})
		must(t, err)
		answer, err := srv.Client().Post(srv.URL+"/rebase", "application/json", bytes.NewReader(body))
		must(t, err)
		defer answer.Body.Close()
		out, err := io.ReadAll(answer.Body)
		must(t, err)
		return answer.StatusCode, out
	}
	tags := func() []string {
		t.Helper()
		held, err := e.images(ctx, "vite-spa")
		must(t, err)
		var names []string
		for _, tag := range held {
			names = append(names, tag.id+" "+tag.ref)
		}
		slices.Sort(names)
		return names
	}
	before := tags()
	for _, name := range []string{other, repo + ":newer"} { // another repository by digest; this one by a tag
		e.RunImage = name
		if status, body := rebase(builder()); status != http.StatusConflict || !strings.Contains(string(body), `"error":"another_run_image"`) {
			t.Fatalf("a rebase onto %s of an image built onto %s: %d %s", name, older, status, body)
		}
		if now := tags(); !slices.Equal(now, before) {
			t.Fatalf("a refused rebase onto %s wrote a tag: %v, was %v", name, now, before)
		}
	}

	e.RunImage = newer
	run := builder()
	status, body := rebase(run)
	if status != http.StatusOK {
		t.Fatalf("a rebase onto %s, the newer digest of %s: %d %s", newer, older, status, body)
	}
	var rebased RebaseResult
	must(t, json.Unmarshal(body, &rebased))
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(rebased.Image, "canyon/vite-spa@")).Run()
	b, a := rebased.Before, rebased.After
	top := func(f RebaseFacts) int { return slices.Index(f.DiffIDs, f.TopLayer) }
	if top(b) < 0 || top(a) < 0 || !slices.Equal(b.DiffIDs[top(b)+1:], a.DiffIDs[top(a)+1:]) {
		t.Fatalf("the layers above the run image changed:\nbefore %v above %s\nafter  %v above %s", b.DiffIDs, b.TopLayer, a.DiffIDs, a.TopLayer)
	}
	var layers []string
	must(t, json.Unmarshal([]byte(docker("image", "inspect", "--format", "{{json .RootFS.Layers}}", newer)), &layers))
	if !slices.Equal(a.DiffIDs[:top(a)+1], layers) || slices.Equal(b.DiffIDs[:top(b)+1], layers) {
		t.Fatalf("the layers below are not the newer run image's %v:\nbefore %v\nafter  %v", layers, b.DiffIDs, a.DiffIDs)
	}
	if a.RunImage != run || b.RunImage != was || a.BuildMetadata != b.BuildMetadata || a.Config != b.Config || a.StackID != b.StackID || rebased.Image == res.Image {
		t.Fatalf("GET /builder %s, was %s\nbefore %+v\nafter %+v\nimage %s, was %s", run, was, b, a, rebased.Image, res.Image)
	}
}

// CANYON_RUN_APT_MIRROR builds canyon-run's packages through the archive it names (contracts/executor.md,
// Images and database sources): here Ubuntu's own, named as a mirror. The image's one apt source is it —
// sources.list.d holds ubuntu.sources alone, naming no other archive, and sources.list no deb line — so the
// build read it and nothing else.
func TestRunAptMirrorE2EBuildsCanyonRunThroughIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	saved := runImage.dockerfile
	defer func() { runImage.dockerfile = saved }()
	mirror := "http://archive.ubuntu.com/ubuntu/" // amd64's archive; every other architecture is on ports
	if runtime.GOARCH != "amd64" {
		mirror = "http://ports.ubuntu.com/ubuntu-ports/"
	}
	must(t, RunAptMirror(func(string) string { return mirror }))
	built := &scanImage{dockerfile: runImage.dockerfile, repo: "canyon-run:e2e-mirror-" + nonce()}
	_, err := built.built(ctx, "docker")
	must(t, err)
	defer exec.Command("docker", "image", "rm", built.repo).Run()
	in := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", built.repo}, args...)...).CombinedOutput()
		must(t, err)
		return string(out)
	}
	if listed := in("ls", "-A", "/etc/apt/sources.list.d"); listed != "ubuntu.sources\n" {
		t.Fatalf("sources.list.d holds %q, want ubuntu.sources alone", listed)
	}
	if lines := in("sh", "-c", `grep -Ec '^[[:space:]]*deb' /etc/apt/sources.list || true`); lines != "0\n" {
		t.Fatalf("sources.list has %q deb lines, want none", lines)
	}
	if sources := in("cat", "/etc/apt/sources.list.d/ubuntu.sources"); !strings.Contains(sources, "URIs: "+mirror+"\nSuites: noble noble-updates noble-security\n") || strings.Count(sources, "URIs:") != 1 {
		t.Fatalf("canyon-run's one source is not the mirror:\n%s", sources)
	}
}
