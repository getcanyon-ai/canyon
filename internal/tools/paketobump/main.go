// Command paketobump moves Canyon's Paketo pins to Paketo's newest images, pushes them to a branch and
// files the issue a person opens its pull request from, whose body is what moves in the fixture apps'
// images, built and scanned on the old pins and on the new (design/PATCHING.md §5, #2114).
// .github/workflows/paketo-bump.yml runs it. It fails closed: a digest it cannot resolve, a builder whose
// buildpacks are not the ClusterStore's, or an image it cannot build or scan ends it with an error, no
// branch and no issue.
//
//	go run ./internal/tools/paketobump -out <dir>        # from the repository's root: rewrites the pins, writes bump.json, prints the issue
//	go run ./internal/tools/paketobump -out <dir> issue  # pushes the branch from <dir>/pins.patch and opens or updates the issue
//	go run ./internal/tools/paketobump sbom              # this tree's pins: the fixture apps' image scans, as JSON on stdout
//	go run ./internal/tools/paketobump pin <name>        # this tree's one pin of paketobuildpacks/<name>, by digest
package main

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/package-url/packageurl-go"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

const (
	builder = "ubuntu-noble-builder"
	run     = "ubuntu-noble-run"
	build   = "ubuntu-noble-build"
)

// The fixture apps built on each side: Paketo's Node, Python and Java samples, whose images
// design/PATCHING.md §2's class table is checked against (executor/e2e_test.go).
var apps = []string{"node-engines", "django-sqlite", "spring-maven"}

// pinRE is a Paketo image pinned by digest, as Canyon names one in its Dockerfiles, its manifests and its tests.
var pinRE = regexp.MustCompile(`docker\.io/paketobuildpacks/([a-z0-9-]+)@(sha256:[0-9a-f]{64})`)

// apart are the files whose Paketo pins are not Canyon's and the bump never reads or moves: the patch journey's
// older run image, which must stay behind Paketo's newest for a newer run image to have an os package to fix.
var apart = []string{"e2e/oidc/patch_test.go"}

func main() {
	out := flag.String("out", "", "the bump's directory: bump.json, written only when a pin moved, and pins.patch beside it for issue")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name: the issue's repository and its compare link's")
	flag.Parse()
	ctx := context.Background()
	if flag.Arg(0) == "sbom" {
		if err := sboms(ctx, os.Stdout); err != nil {
			log.Fatalf("paketobump sbom: %v", err)
		}
		return
	}
	if flag.Arg(0) == "pin" { // what scripts/builder-node.sh builds the Node builder from (#174)
		pinned, err := bumper{root: "."}.pins()
		if err != nil {
			log.Fatalf("paketobump pin: %v", err)
		}
		p := pinned[flag.Arg(1)]
		if p == nil {
			log.Fatalf("paketobump pin: no file pins paketobuildpacks/%s", flag.Arg(1))
		}
		fmt.Printf("docker.io/paketobuildpacks/%s@%s\n", flag.Arg(1), p.digest)
		return
	}
	if *out == "" || *repo == "" {
		log.Fatal("usage: paketobump -out <dir> [-repo owner/name] [issue] | paketobump sbom | paketobump pin <name>")
	}
	if flag.Arg(0) == "issue" {
		raw, err := os.ReadFile(filepath.Join(*out, "bump.json")) //nolint:gosec // the bump job's artifact
		var r result
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		if err == nil {
			err = file(r, *repo, ghCLI{repo: *repo}, func(branch string) error { return pushBranch(*out, branch) })
		}
		if err != nil {
			log.Fatalf("paketobump issue: %v", err)
		}
		return
	}
	b := bumper{root: ".", hub: "docker.io", sbom: childSBOMs}
	r, err := b.bump(ctx)
	if err != nil {
		log.Fatalf("paketobump: %v", err)
	}
	if r == nil {
		fmt.Println("Every Paketo pin is Paketo's newest: nothing to bump.")
		return
	}
	raw, err := json.Marshal(r)
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "bump.json"), raw, 0o600)
	}
	if err != nil {
		log.Fatal(err)
	}
	title, body := r.issue(*repo)
	fmt.Printf("%s\n\n%s", title, body)
}

// bumper moves the pins under root to the newest images hub serves, scanning the fixture apps with sbom
// on each side.
type bumper struct {
	root string
	hub  string
	opts []crane.Option
	sbom func(ctx context.Context, root string) (map[string]executor.ScanResult, error)
}

// pin is one Paketo image as the repository pins it: one digest, in every file that names it.
type pin struct {
	digest string
	files  []string
}

// move is a pin that Paketo's newest image moves; a buildpack's carries its versions.
type move struct {
	Repo       string `json:"repo"`
	Old        string `json:"old"`
	New        string `json:"new"`
	OldVersion string `json:"old_version,omitempty"`
	NewVersion string `json:"new_version,omitempty"`
}

// bump moves the pins to Paketo's newest and diffs the fixture apps' scans on each side; nil when every
// pin is Paketo's newest.
func (b bumper) bump(ctx context.Context) (*result, error) {
	pinned, err := b.pins()
	if err != nil {
		return nil, err
	}
	moves, err := b.newest(pinned)
	if err != nil || len(moves) == 0 {
		return nil, err
	}
	before, err := b.sbom(ctx, b.root)
	if err != nil {
		return nil, fmt.Errorf("the fixture apps on the old pins: %w", err)
	}
	for _, m := range moves {
		for _, f := range pinned[m.Repo].files {
			if err := replace(filepath.Join(b.root, f), "paketobuildpacks/"+m.Repo+"@"+m.Old, "paketobuildpacks/"+m.Repo+"@"+m.New); err != nil {
				return nil, err
			}
		}
	}
	after, err := b.sbom(ctx, b.root)
	if err != nil {
		return nil, fmt.Errorf("the fixture apps on the new pins: %w", err)
	}
	lines, err := diff(before, after)
	if err != nil {
		return nil, err
	}
	return &result{Moves: moves, Lines: lines}, nil
}

// pins reads every Paketo pin in the repository's tracked files. Two digests of one image fail: the bump
// would not know which one Canyon runs.
func (b bumper) pins() (map[string]*pin, error) {
	out, err := exec.Command("git", "-C", b.root, "ls-files", "-z").Output() //nolint:gosec // root is the checkout the workflow runs in
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	pinned := map[string]*pin{}
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if slices.Contains(apart, f) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(b.root, f)); err != nil || !info.Mode().IsRegular() {
			continue // a symbolic link or a submodule: the file it names is read as its own
		}
		text, err := os.ReadFile(filepath.Join(b.root, f)) //nolint:gosec // a file git tracks
		if err != nil {
			return nil, err
		}
		for _, m := range pinRE.FindAllStringSubmatch(string(text), -1) {
			p := pinned[m[1]]
			switch {
			case p == nil:
				pinned[m[1]] = &pin{digest: m[2], files: []string{f}}
			case p.digest != m[2]:
				return nil, fmt.Errorf("paketobuildpacks/%s is pinned at %s in %s and at %s in %s", m[1], p.digest, p.files[0], m[2], f)
			case !slices.Contains(p.files, f):
				p.files = append(p.files, f)
			}
		}
	}
	for _, base := range []string{builder, run, build} {
		if pinned[base] == nil {
			return nil, fmt.Errorf("no file pins paketobuildpacks/%s", base)
		}
	}
	return pinned, nil
}

// newest resolves Paketo's newest builder, run and build images, and each ClusterStore source at the
// version the newest builder carries, so a cluster builds with the buildpacks Docker does. The store's
// sources must be the builder's buildpacks, each pin the buildpack its repository names, and the newest
// builder's order the pinned one's, or the bump fails: a buildpack Paketo adds, drops or reorders is
// moved by hand, in the ClusterBuilder's order too.
func (b bumper) newest(pinned map[string]*pin) ([]move, error) {
	var moves []move
	for _, base := range []string{builder, run, build} {
		d, err := crane.Digest(b.hub+"/paketobuildpacks/"+base+":latest", b.opts...)
		if err != nil {
			return nil, fmt.Errorf("paketobuildpacks/%s:latest: %w", base, err)
		}
		moves = append(moves, move{Repo: base, Old: pinned[base].digest, New: d})
	}
	old, err := b.order(pinned[builder].digest)
	if err != nil {
		return nil, err
	}
	now, err := b.order(moves[0].New)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(ids(old), ids(now)) {
		return nil, fmt.Errorf("the newest builder orders %v, the pinned one %v: move the ClusterStore and the ClusterBuilder's order by hand", ids(now), ids(old))
	}
	var store []string
	for repo := range pinned {
		if repo != builder && repo != run && repo != build {
			store = append(store, repo)
		}
	}
	if slices.Sort(store); !slices.Equal(store, slices.Sorted(maps.Keys(now))) {
		return nil, fmt.Errorf("the ClusterStore's sources %v are not the builder's buildpacks %v", store, slices.Sorted(maps.Keys(now)))
	}
	for _, repo := range ids(now) {
		ref := b.hub + "/paketobuildpacks/" + repo + "@" + pinned[repo].digest
		labels, err := b.labels(ref)
		if err != nil {
			return nil, err
		}
		var was struct{ ID, Version string }
		if json.Unmarshal([]byte(labels["io.buildpacks.buildpackage.metadata"]), &was) != nil || was.ID != "paketo-buildpacks/"+repo || was.Version == "" {
			return nil, fmt.Errorf("%s is not paketo-buildpacks/%s at a version: %s", ref, repo, labels["io.buildpacks.buildpackage.metadata"])
		}
		d, err := crane.Digest(b.hub+"/paketobuildpacks/"+repo+":"+now[repo].version, b.opts...)
		if err != nil {
			return nil, fmt.Errorf("paketobuildpacks/%s:%s: %w", repo, now[repo].version, err)
		}
		moves = append(moves, move{Repo: repo, Old: pinned[repo].digest, New: d, OldVersion: was.Version, NewVersion: now[repo].version})
	}
	return slices.DeleteFunc(moves, func(m move) bool { return m.Old == m.New }), nil
}

func (b bumper) labels(ref string) (map[string]string, error) {
	raw, err := crane.Config(ref, b.opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	var cfg struct {
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s's config: %w", ref, err)
	}
	return cfg.Config.Labels, nil
}

type entry struct {
	at      int
	version string
}

// order is the builder's io.buildpacks.buildpack.order label: each group's buildpack by its image's
// repository (paketo-buildpacks/nodejs is paketobuildpacks/nodejs), its place and version.
func (b bumper) order(digest string) (map[string]entry, error) {
	ref := b.hub + "/paketobuildpacks/" + builder + "@" + digest
	labels, err := b.labels(ref)
	if err != nil {
		return nil, err
	}
	var groups []struct {
		Group []struct{ ID, Version string }
	}
	if json.Unmarshal([]byte(labels["io.buildpacks.buildpack.order"]), &groups) != nil || len(groups) == 0 {
		return nil, fmt.Errorf("%s: no io.buildpacks.buildpack.order label that reads", ref)
	}
	out := map[string]entry{}
	for _, g := range groups {
		for _, bp := range g.Group {
			repo, ok := strings.CutPrefix(bp.ID, "paketo-buildpacks/")
			if !ok || bp.Version == "" {
				return nil, fmt.Errorf("%s orders %s %q, not a Paketo buildpack at a version", ref, bp.ID, bp.Version)
			}
			out[repo] = entry{at: len(out), version: bp.Version}
		}
	}
	return out, nil
}

// ids is the order's repositories, in order.
func ids(order map[string]entry) []string {
	out := slices.Collect(maps.Keys(order))
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(order[a].at, order[b].at) })
	return out
}

func replace(path, old, new string) error {
	text, err := os.ReadFile(path) //nolint:gosec // a file git tracks, that pins the image
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, bytes.ReplaceAll(text, []byte(old), []byte(new)), info.Mode()) //nolint:gosec // the same file
}

// childSBOMs scans the fixture apps on root's pins: a go run of this command, so the executor it builds
// with embeds that tree's builder.Dockerfile and run.Dockerfile.
func childSBOMs(ctx context.Context, root string) (map[string]executor.ScanResult, error) {
	cmd := exec.CommandContext(ctx, "go", "run", "./internal/tools/paketobump", "sbom")
	cmd.Dir, cmd.Stderr = root, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var scans map[string]executor.ScanResult
	if err := json.Unmarshal(out, &scans); err != nil {
		return nil, fmt.Errorf("the scans: %w", err)
	}
	return scans, nil
}

// sboms builds each fixture app with this tree's executor and scans its image as Canyon does after a
// /run (POST /scan, target image; design/PATCHING.md §3.1), writing each app's scan.
func sboms(ctx context.Context, w io.Writer) error {
	e := &executor.Executor{Join: []string{}}
	srv := httptest.NewServer(executor.Handler(e))
	defer srv.Close()
	scans := map[string]executor.ScanResult{}
	for _, app := range apps {
		tree, err := tarOf(filepath.Join("executor", "testdata", "apps", app))
		if err != nil {
			return err
		}
		res, err := e.Build(ctx, executor.BuildRequest{App: app, Package: "pkg_bump"}, tree, os.Stderr)
		if err != nil {
			return fmt.Errorf("build %s: %w", app, err)
		}
		scan, err := scanImage(ctx, srv.URL, app, res.Image)
		_ = exec.CommandContext(ctx, "docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/"+app+"@")).Run() //nolint:gosec // the image the build answered
		if err != nil {
			return fmt.Errorf("scan %s: %w", app, err)
		}
		scans[app] = scan
	}
	return json.NewEncoder(w).Encode(scans)
}

// scanImage asks for the image's SBOM, again while the executor answers busy: the first scan finds no
// trivy Java database and starts its download (contracts/executor.md, Busy). Any scan that does not finish
// fails, a database that never downloaded among them.
func scanImage(ctx context.Context, url, app, image string) (executor.ScanResult, error) {
	req, _ := json.Marshal(map[string]string{"tool": "trivy", "target": "image", "app": app, "image": image})
	for range 60 {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if err := mw.WriteField("json", string(req)); err != nil {
			return executor.ScanResult{}, err
		}
		if err := mw.Close(); err != nil {
			return executor.ScanResult{}, err
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/scan", &body)
		if err != nil {
			return executor.ScanResult{}, err
		}
		r.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			return executor.ScanResult{}, err
		}
		answer, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return executor.ScanResult{}, err
		}
		var scan executor.ScanResult
		switch {
		case resp.StatusCode == http.StatusServiceUnavailable:
			time.Sleep(10 * time.Second)
		case resp.StatusCode != http.StatusOK || json.Unmarshal(answer, &scan) != nil:
			return executor.ScanResult{}, fmt.Errorf("%d %.600s", resp.StatusCode, answer)
		case scan.NotFinished != "" || scan.SBOM == nil:
			return executor.ScanResult{}, fmt.Errorf("not finished: %s %s", cmp.Or(scan.NotFinished, "no SBOM"), scan.Reason)
		default:
			return scan, nil
		}
	}
	return executor.ScanResult{}, errors.New("the executor answered busy for 10 minutes")
}

func tarOf(root string) (io.Reader, error) {
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name, _ = filepath.Rel(root, path)
		if err := tw.WriteHeader(h); err != nil || info.IsDir() {
			return err
		}
		f, err := os.ReadFile(path) //nolint:gosec // a fixture app's file
		if err != nil {
			return err
		}
		_, err = tw.Write(f)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &tree, tw.Close()
}

// pkg is a package as the diff names it, by class and purl without its version.
type pkg struct {
	class, key, name string
}

var classes = []string{"os", "runtime", "lifecycle", "unknown"}

// versions is each platform package's versions across the apps' images: an app's own dependencies are
// the team's and left out (design/PATCHING.md §2).
func versions(scans map[string]executor.ScanResult) (map[pkg][]string, error) {
	out := map[pkg][]string{}
	for _, app := range apps {
		scan, ok := scans[app]
		if !ok {
			return nil, fmt.Errorf("no scan of %s", app)
		}
		owners, ok := trivy.Owners(scan.DiffIDs, scan.Lifecycle)
		if !ok {
			return nil, fmt.Errorf("%s: no layer of its image can be owned by its label", app)
		}
		var sbom struct {
			Components []struct {
				Type, Name, Version, Purl string
				Properties                []struct{ Name, Value string }
			}
		}
		if err := json.Unmarshal(scan.SBOM, &sbom); err != nil {
			return nil, fmt.Errorf("%s's SBOM: %w", app, err)
		}
		for _, c := range sbom.Components {
			if c.Type != "library" {
				continue
			}
			prop := map[string]string{}
			for _, p := range c.Properties {
				prop[p.Name] = p.Value
			}
			class := trivy.Class(owners[prop["aquasecurity:trivy:LayerDiffID"]], prop["aquasecurity:trivy:PkgType"], c.Purl)
			if class == "app" {
				continue
			}
			k := pkg{class: class, key: "?" + c.Name, name: c.Name}
			if p, err := packageurl.FromString(c.Purl); err == nil {
				k.key = p.Type + "/" + p.Namespace + "/" + p.Name
				if p.Type != packageurl.TypeDebian && p.Namespace != "" {
					k.name = p.Namespace + "/" + p.Name
				}
			}
			if !slices.Contains(out[k], c.Version) {
				out[k] = append(out[k], c.Version)
			}
		}
	}
	return out, nil
}

// goModules marks a class's Go modules in diff's lines: those of a buildpack's own binaries or the
// launcher's, which the body folds away beneath the class's other packages.
const goModules = " go"

// diff is a line per platform package whose versions move, by class: `name old → new`, "none" for a
// package one side does not hold.
func diff(before, after map[string]executor.ScanResult) (map[string][]string, error) {
	old, err := versions(before)
	if err != nil {
		return nil, err
	}
	now, err := versions(after)
	if err != nil {
		return nil, err
	}
	keys := slices.Collect(maps.Keys(old))
	for k := range now {
		if _, ok := old[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b pkg) int { return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.key, b.key)) })
	lines := map[string][]string{}
	for _, k := range keys {
		was, is := slices.Sorted(slices.Values(old[k])), slices.Sorted(slices.Values(now[k]))
		if !slices.Equal(was, is) {
			bucket := k.class
			if strings.HasPrefix(k.key, "golang/") && k.key != "golang//stdlib" {
				bucket += goModules
			}
			lines[bucket] = append(lines[bucket], fmt.Sprintf("- `%s` %s → %s", k.name, cmp.Or(strings.Join(was, ", "), "none"), cmp.Or(strings.Join(is, ", "), "none")))
		}
	}
	return lines, nil
}
