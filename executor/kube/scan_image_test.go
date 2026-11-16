package kube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

const (
	cdxOut    = `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`
	lifecycle = `{"runImage":{"topLayer":"sha256:1a"}}`
	report    = `{"SchemaVersion":2,"Results":[{"Packages":[{"Identifier":{"BOMRef":"pkg:deb/ubuntu/libssl3@3.0.13"}}],"Vulnerabilities":[{"VulnerabilityID":"CVE-2024-6119","PkgIdentifier":{"BOMRef":"pkg:deb/ubuntu/libssl3@3.0.13"},"Layer":{"DiffID":"sha256:` + "1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a" + `"}}]}]}`
)

// pushApp pushes claims-portal's image, layers random layers and a lifecycle label, to the registry this
// executor names, and answers the image scan's request for it, its tarball's size and its manifest's
// sizes, the compressed bytes the cap reads.
func pushApp(t *testing.T, e *Executor, layers int, label string) (executor.ScanRequest, int64, int64) {
	t.Helper()
	img, err := random.Image(256, int64(layers))
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.Config(img, v1.Config{Labels: map[string]string{executor.LifecycleLabel: label}}); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	ref := mustRef(t, e.Registry+"/canyon/claims-portal@"+digest.String())
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	size, err := tarball.CalculateSize(map[name.Reference]v1.Image{ref: img})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := img.Manifest()
	compressed := m.Config.Size
	for _, l := range m.Layers {
		compressed += l.Size
	}
	return executor.ScanRequest{Tool: "trivy", Target: "image", App: "claims-portal", Image: ref.String()}, size, compressed
}

// appInRegistry is pushApp of a two-layer image, to a registry of its own.
func appInRegistry(t *testing.T, e *Executor) (executor.ScanRequest, int64) {
	t.Helper()
	e.Registry = strings.TrimPrefix(testRegistry(t).URL, "http://")
	req, size, _ := pushApp(t, e, 2, lifecycle)
	return req, size
}

// withImageMax runs the test with CANYON_SCAN_IMAGE_MAX at max bytes.
func withImageMax(t *testing.T, max int64) {
	t.Helper()
	if err := executor.Configure(func(k string) string {
		return map[string]string{"CANYON_SCAN_IMAGE_MAX": strconv.FormatInt(max, 10)}[k]
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = executor.Configure(func(k string) string { return map[string]string{"CANYON_SCAN_IMAGE_MAX": "8Gi"}[k] })
	})
}

func noParts() *multipart.Reader { return partsOf() }

func sbomParts(sbom string) *multipart.Reader { return partsOf("sbom", sbom) }

// partsOf is a /scan body after its json: each name, then its body.
func partsOf(namesAndBodies ...string) *multipart.Reader {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for i := 0; i+1 < len(namesAndBodies); i += 2 {
		w, _ := mw.CreateFormFile(namesAndBodies[i], namesAndBodies[i])
		_, _ = io.WriteString(w, namesAndBodies[i+1])
	}
	_ = mw.Close()
	return multipart.NewReader(&b, mw.Boundary())
}

func javaDBBuilt(ago time.Duration) string {
	return `{"Version":1,"UpdatedAt":"` + time.Now().Add(-ago).UTC().Format(time.RFC3339Nano) + `"}`
}

// An image scan is fed the app's image from the registry, as docker save lays it out, beside a tmp/ the
// scan's user writes, into a /scan sized from its manifest and requested so; trivy image lists it on the
// Java database, the volume canyon-scandb-trivy-java read-only, TMPDIR on the scan's disk, in a pod that
// reaches nothing — never a client of trivy's server — and the answer is its SBOM with the image's
// lifecycle label, its diff IDs in order, and no database date.
func TestAnImageScanIsFedTheImageAndReadsTheJavaDatabase(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	c.meta = javaDBBuilt(time.Hour)
	addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
	req, size := appInRegistry(t, e)
	res, err := e.Scan(t.Context(), req, noParts())
	if err != nil || string(res.SBOM) != cdxOut || res.Lifecycle != lifecycle || res.DBBuiltAt != "" || res.NotFinished != "" || len(res.DiffIDs) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	readers, scans := jobsNamed(c, "canyon-scan-read-trivy-java-"), jobsNamed(c, "canyon-scan-trivy-java-")
	if len(readers) != 1 || len(scans) != 1 || len(jobsNamed(c, "canyon-scan-trivy-")) != 1 {
		t.Fatalf("%d readers, %d image scans, and no other trivy scan", len(readers), len(scans))
	}
	java, _ := executor.ToolOf(executor.JavaDB)
	if cmd := readers[0].Spec.Template.Spec.Containers[0].Command; !slices.Equal(cmd, stdoutOnly(append([]string{"sh"}, java.DB.Read...)...)) || !strings.Contains(cmd[len(cmd)-1], "trivy-java.db") {
		t.Errorf("the Java database's reader runs %v", cmd)
	}
	s := scans[0].Spec.Template
	tool := s.Spec.Containers[0]
	disk := resource.NewQuantity(2*size+busyboxRoom, resource.BinarySI)
	ro := func(v corev1.Volume) bool {
		return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "canyon-scandb-trivy-java" && v.PersistentVolumeClaim.ReadOnly
	}
	scan := func(v corev1.Volume) bool { return v.Name == "scan" && v.EmptyDir.SizeLimit.Cmp(*disk) == 0 }
	if s.Labels[clientLabel] != "" || slices.Contains(tool.Args, trivyURL) || !slices.ContainsFunc(s.Spec.Volumes, ro) || !slices.ContainsFunc(s.Spec.Volumes, scan) ||
		!hasMount(tool.VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: "/scan/db", ReadOnly: true}) || tool.Resources.Requests.StorageEphemeral().Cmp(*disk) != 0 ||
		!slices.Equal(tool.Args[:3], []string{"image", "--input", "/scan/image.tar"}) || !slices.Contains(tool.Args, "--cache-backend") || !slices.Contains(tool.Args, "--ignorefile") ||
		!slices.Equal(tool.Env, []corev1.EnvVar{{Name: "TMPDIR", Value: "/scan/tmp"}}) || *tool.SecurityContext.RunAsUser != 65534 {
		t.Errorf("the image scan's Job: labels %v, args %v, env %v, volumes %+v, resources %+v", s.Labels, tool.Args, tool.Env, s.Spec.Volumes, tool.Resources)
	}
	if c.modes[0]["image.tar"] != 0o644 || c.modes[0]["tmp/"] != 0o1777 {
		t.Errorf("the feed's modes: %v", c.modes[0])
	}
	fed := c.fed[0]["image.tar"]
	img, err := tarball.Image(func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(fed)), nil }, nil)
	if err != nil || int64(len(fed)) != size {
		t.Fatalf("the feed's image.tar, %d bytes of %d: %v", len(fed), size, err)
	}
	if got, _ := img.Digest(); "/canyon/claims-portal@"+got.String() != strings.TrimPrefix(req.Image, e.Registry) {
		t.Errorf("the fed image is %s, not %s", got, req.Image)
	}
	config, _ := img.ConfigFile()
	if want := []string{config.RootFS.DiffIDs[0].String(), config.RootFS.DiffIDs[1].String()}; !slices.Equal(res.DiffIDs, want) {
		t.Errorf("diff_ids %v, want the image's in order %v", res.DiffIDs, want)
	}
}

// An image at CANYON_SCAN_IMAGE_MAX, in the compressed bytes its manifest names, is read; one byte past
// it, more than 128 layers, or a lifecycle label over 1 MiB is over_cap, reason image, with no Job.
func TestAnImageScanPastItsBoundsRunsNoJob(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	c.meta = javaDBBuilt(time.Hour)
	addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
	e.Registry = strings.TrimPrefix(testRegistry(t).URL, "http://")
	req, _, compressed := pushApp(t, e, 2, lifecycle)
	withImageMax(t, compressed)
	if res, err := e.Scan(t.Context(), req, noParts()); err != nil || res.SBOM == nil {
		t.Fatalf("an image of exactly its cap: %+v %v", res, err)
	}
	withImageMax(t, compressed-1)
	if res, err := e.Scan(t.Context(), req, noParts()); err != nil || res.NotFinished != "over_cap" || res.Reason != "image" {
		t.Errorf("an image one byte past its cap: %+v %v", res, err)
	}
	withImageMax(t, 8<<30)
	for label, layers := range map[string]int{lifecycle: executor.MaxImageLayers + 1, strings.Repeat("x", executor.MaxLifecycle+1): 1} {
		big, _, _ := pushApp(t, e, layers, label)
		if res, err := e.Scan(t.Context(), big, noParts()); err != nil || res.NotFinished != "over_cap" || res.Reason != "image" {
			t.Errorf("%d layers, a label of %d bytes: %+v %v", layers, len(label), res, err)
		}
	}
	if jobs := jobsNamed(c, "canyon-scan-trivy-java-"); len(jobs) != 1 {
		t.Errorf("%d image scans ran, want the one within its bounds", len(jobs))
	}
}

// An image the registry does not hold is ErrNoImage (404); another app's, one by tag, one whose digest is
// short, or one beside a part is 400; a second image scan while one runs answers queued, as a second
// grype scan does — image scans are serialised on the Java database's volume — and reads no image.
func TestAnImageScanOutsideItsAppOrBesideAnotherRunsNoJob(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	req, _ := appInRegistry(t, e)
	digest := req.Image[strings.LastIndex(req.Image, ":")+1:]
	for name, tc := range map[string]struct {
		image string
		parts *multipart.Reader
		want  error
	}{
		"not in the registry": {e.Registry + "/canyon/claims-portal@sha256:" + strings.Repeat("0", 64), noParts(), executor.ErrNoImage},
		"another app's":       {strings.Replace(req.Image, "/canyon/claims-portal@", "/canyon/case-intake@", 1), noParts(), executor.ErrRequest},
		"by tag":              {e.Registry + "/canyon/claims-portal:latest", noParts(), executor.ErrRequest},
		"a digest one short":  {strings.TrimSuffix(req.Image, digest) + digest[:63], noParts(), executor.ErrRequest},
		"beside a tree":       {req.Image, partsOf("tree", "x"), executor.ErrRequest},
	} {
		r := req
		r.Image = tc.image
		if _, err := e.Scan(t.Context(), r, tc.parts); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if err := scanning[executor.JavaDB].scan(); err != nil { // an image scan in flight
		t.Fatal(err)
	}
	_, err := e.Scan(t.Context(), req, noParts())
	scanning[executor.JavaDB].release(false)
	if !errors.Is(err, executor.ErrQueued) {
		t.Errorf("a second image scan: %v", err)
	}
	if jobs := jobsNamed(c, "canyon-scan-"); len(jobs) != 0 {
		t.Errorf("%d Jobs ran", len(jobs))
	}
}

// An image or sbom scan whose tool wrote no document is not_sarif, one the OOM killer ended killed, one
// past 10 MiB over_cap with reason report.
func TestAnImageOrSBOMScanThatDidNotFinish(t *testing.T) {
	oom := func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "tool", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
	}
	for _, tc := range []struct {
		name, target, out, want, reason string
		end                             func(*corev1.Pod)
	}{
		{"an image, no SBOM", "image", "Traceback: …", "not_sarif", "", nil},
		{"an image, killed", "image", "", "killed", "", oom},
		{"an image, past 10 MiB", "image", `{"bomFormat":"CycloneDX","x":"` + strings.Repeat("x", executor.MaxScanOut) + `"}`, "over_cap", "report", nil},
		{"an sbom, no report", "sbom", "Traceback: …", "not_sarif", "", nil},
		{"an sbom, killed", "sbom", "", "killed", "", oom},
		{"an sbom, past 10 MiB", "sbom", `{"SchemaVersion":2,"x":"` + strings.Repeat("x", executor.MaxScanOut) + `"}`, "over_cap", "report", nil},
	} {
		e := &Executor{}
		c := newScanCluster(t, e, tc.out)
		if tc.end != nil {
			c.end = tc.end
		}
		req, parts := executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(cdxOut)
		if tc.target == "image" {
			c.meta = javaDBBuilt(time.Hour)
			addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
			req, _ = appInRegistry(t, e)
			parts = noParts()
		} else {
			serveTrivy(t, c, e, time.Now().Add(-time.Hour))
		}
		if res, err := e.Scan(context.Background(), req, parts); err != nil || res.NotFinished != tc.want || res.Reason != tc.reason || res.SBOM != nil || res.Report != nil {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
	}
}

// An image scan without its Java database starts the database's download into its own volume, staged in
// the volume's tmp/, emptied first, and is busy; once that fails it is db_missing, and on one built 6 days
// ago db_stale, each naming the Java database, and no image scan runs.
func TestAnImageScanWithoutItsJavaDatabaseRunsNoScan(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, cdxOut)
	req, _ := appInRegistry(t, e)
	c.exit["refresh"] = 1
	if _, err := e.Scan(t.Context(), req, noParts()); !errors.Is(err, executor.ErrBusy) {
		t.Fatalf("the first image scan: %v", err)
	}
	e.collecting.Wait()
	refreshes := jobsNamed(c, "canyon-scandb-trivy-java-")
	if len(refreshes) != 1 || !slices.Equal(refreshes[0].Spec.Template.Spec.Containers[0].Command[:3], []string{"sh", "-c", `rm -rf /db/tmp && mkdir /db/tmp && exec trivy "$@"`}) ||
		!slices.Contains(refreshes[0].Spec.Template.Spec.Containers[0].Command, "--download-java-db-only") ||
		!slices.Contains(refreshes[0].Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "TMPDIR", Value: "/db/tmp"}) {
		t.Fatalf("the Java database's refresh: %+v", refreshes)
	}
	if res, err := e.Scan(t.Context(), req, noParts()); err != nil || res.NotFinished != "db_missing" || res.Reason != executor.JavaReason {
		t.Errorf("once its download failed: %+v %v", res, err)
	}
	c.meta = javaDBBuilt(6 * 24 * time.Hour)
	claims := c.CoreV1().PersistentVolumeClaims(ScanNamespace)
	claim, err := claims.Get(t.Context(), "canyon-scandb-trivy-java", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim.Status.Phase = corev1.ClaimBound // the refresh's pod bound it
	if _, err := claims.UpdateStatus(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Scan(t.Context(), req, noParts()); err != nil || res.NotFinished != "db_stale" || res.Reason != executor.JavaReason {
		t.Errorf("built 6 days ago: %+v %v", res, err)
	}
	c.exit["reader"] = 1 // trivy-java.db missing, or not SQLite's: downloaded again, and missing once that fails
	if _, err := e.Scan(t.Context(), req, noParts()); !errors.Is(err, executor.ErrBusy) {
		t.Errorf("a database the reader cannot read: %v", err)
	}
	e.collecting.Wait()
	if res, err := e.Scan(t.Context(), req, noParts()); err != nil || res.NotFinished != "db_missing" || res.Reason != executor.JavaReason {
		t.Errorf("a database the reader cannot read, its download failed: %+v %v", res, err)
	}
	if scans := jobsNamed(c, "canyon-scan-trivy-java-"); len(scans) != 0 {
		t.Errorf("%d image scans ran", len(scans))
	}
}

// A packages scan is no client of trivy's server and needs none: its pod labelled canyon.scan alone, so
// it reaches nothing, no volume, trivy's offline arguments as they are and the tree as it came, fed
// without a config; it answers trivy's JSON report as packages, with no database date.
func TestAPackagesScanReachesNothingAndAnswersTheReport(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, report)
	req := executor.ScanRequest{Tool: "trivy", Target: "packages", App: "claims-portal", Package: "pkg_7d3e8f"}
	_, tool, _ := executor.ToolFor(req)
	res, err := e.Scan(context.Background(), req, scanParts(t, scanTree(t, "yarn.lock", "docs/x.md"), nil))
	if err != nil || string(res.Packages) != report || res.DBBuiltAt != "" || res.Report != nil {
		t.Fatalf("%+v %v", res, err)
	}
	scans := jobsNamed(c, "canyon-scan-trivy-packages-")
	if len(scans) != 1 {
		t.Fatalf("%d packages scans", len(scans))
	}
	spec := scans[0].Spec.Template
	if args := spec.Spec.Containers[0].Args; spec.Labels[clientLabel] != "" || !slices.Equal(args, tool.Args) || !slices.Contains(args, "--offline-scan") || slices.Contains(args, "--server") ||
		slices.ContainsFunc(spec.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) {
		t.Errorf("the packages scan's Job: %v", spec)
	}
	if _, config := c.fed[0]["trivy.yaml"]; c.fed[0]["tree/docs/x.md"] != "xx" || config {
		t.Errorf("fed %v", c.fed[0])
	}
}

// An sbom scan is a client of trivy's server, as a tree scan is, fed the part as /scan/sbom.cdx.json, and
// answers trivy's JSON report and when the server's database was built; a part trivy would read in part is
// refused before any Job.
func TestAnSBOMScanIsATrivyClientFedItsPart(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, report)
	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	serveTrivy(t, c, e, built)
	res, err := e.Scan(context.Background(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(cdxOut))
	if err != nil || string(res.Report) != report || res.DBBuiltAt != built.Format(time.RFC3339) {
		t.Fatalf("%+v %v", res, err)
	}
	scans := jobsNamed(c, "canyon-scan-trivy-")
	args := scans[0].Spec.Template.Spec.Containers[0].Args
	if len(scans) != 1 || scans[0].Spec.Template.Labels[clientLabel] != "true" || args[0] != "sbom" || args[len(args)-1] != "/scan/sbom.cdx.json" || !slices.Contains(args, trivyURL) ||
		slices.ContainsFunc(scans[0].Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) || len(scans[0].Spec.Template.Spec.Containers[0].Env) != 0 {
		t.Errorf("the sbom scan's Job: %v", scans[0].Spec.Template)
	}
	if c.fed[0]["sbom.cdx.json"] != cdxOut || c.modes[0]["sbom.cdx.json"] != 0o644 {
		t.Errorf("fed %v, modes %v", c.fed[0], c.modes[0])
	}
	u := "4c1a2b3d-1111-4222-8333-444455556666"
	for name, part := range map[string]string{
		"not CycloneDX":                       `{"bomFormat":"SPDX"}`,
		"a deb package and no OS":             `{"bomFormat":"CycloneDX","components":[{"bom-ref":"pkg:deb/ubuntu/libssl3@3.0.13","type":"library","name":"libssl3","version":"3.0.13","purl":"pkg:deb/ubuntu/libssl3@3.0.13"}]}`,
		"a component without its type":        `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `"}]}`,
		"a nested component":                  `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `","type":"library","components":[]}]}`,
		"metadata.component with descendants": `{"bomFormat":"CycloneDX","metadata":{"component":{"type":"container","components":[{"type":"library","name":"lodash","version":"4.17.15","purl":"pkg:npm/lodash@4.17.15"}]}},"components":[]}`,
		"a name otherwise than its purl":      `{"bomFormat":"CycloneDX","components":[{"bom-ref":"pkg:npm/lodash@4.17.15","type":"library","name":"lodash-evil","version":"4.17.15","purl":"pkg:npm/lodash@4.17.15"}]}`,
		"a purl that does not parse":          `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `","type":"library","name":"bad","purl":"pkg:npm"}]}`,
		"a bom-ref neither purl nor UUID":     `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + strings.Repeat("r", 2<<10+1) + `","type":"library","name":"a","version":"1","purl":"pkg:npm/a@1"}]}`,
		"two components sharing a bom-ref":    `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `","type":"operating-system"},{"bom-ref":"` + u + `","type":"operating-system"}]}`,
		"components twice":                    `{"bomFormat":"CycloneDX","components":[],"components":[{"bom-ref":"` + u + `","type":"operating-system"}]}`,
		"a properties array past its bound":   `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `","type":"operating-system","properties":[` + strings.Repeat(`{},`, 50_000) + `{}]}]}`,
		"3 million {}":                        `{"bomFormat":"CycloneDX","components":[` + strings.Repeat(`{},`, 3_000_000) + `{}]}`,
	} {
		if _, err := e.Scan(context.Background(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(part)); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if scans := jobsNamed(c, "canyon-scan-trivy-"); len(scans) != 1 {
		t.Errorf("%d sbom scans ran, want the one whose part trivy reads whole", len(scans))
	}
	part := `{"bomFormat":"CycloneDX","components":[{"bom-ref":"pkg:generic/foo@1","type":"library","name":"foo","version":"1","purl":"pkg:generic/foo@1"}]}` // trivy's report lists it not as read
	if res, err := e.Scan(context.Background(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(part)); err != nil || res.NotFinished != "not_sarif" || res.Reason != "unread" || res.Report != nil {
		t.Errorf("a purl of a type trivy skips: %+v %v", res, err)
	}
}

// countingRegistry serves one image by digest — its manifest's HEAD as head says, a Content-Length or none,
// its manifest and config — and counts the bytes of each body it serves.
type countingRegistry struct {
	manifest, config []byte
	headLength       string // the manifest HEAD's Content-Length; "" for none
	served           map[string]int
}

func (r *countingRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/v2/":
	case strings.Contains(req.URL.Path, "/manifests/"):
		w.Header().Set("Content-Type", string(types.OCIManifestSchema1))
		w.Header().Set("Docker-Content-Digest", digestOf(r.manifest))
		if req.Method == http.MethodHead {
			if r.headLength != "" {
				w.Header().Set("Content-Length", r.headLength)
			}
			return
		}
		n, _ := io.Copy(w, struct{ io.Reader }{bytes.NewReader(r.manifest)}) // in chunks: what a client stops reading stops being served
		r.served["manifest"] += int(n)
	case strings.Contains(req.URL.Path, "/blobs/"):
		n, _ := w.Write(r.config)
		r.served["config"] += n
	default:
		http.NotFound(w, req)
	}
}

func digestOf(b []byte) string {
	h, _, _ := v1.SHA256(bytes.NewReader(b))
	return h.String()
}

// The manifest's and the config's sizes are read from their descriptors before either body: one past 4 MiB,
// or a config of size -1 (unknown), is over_cap, reason image, and a manifest HEAD with no Content-Length
// is go-containerregistry's own error; either way that body is never read — so a registry cannot make the
// executor hold what it says nothing of.
func TestAnImagesManifestOrConfigPastItsBoundIsNeverRead(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	manifest := func(configSize int64) []byte {
		return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":` +
			strconv.FormatInt(configSize, 10) + `,"digest":"` + digestOf(config) + `"},"layers":[]}`)
	}
	for _, tc := range []struct {
		name       string
		manifest   []byte
		headLength string
		unread     string
	}{
		{"a manifest of no known size", manifest(int64(len(config))), "", "manifest"}, // an error, not over_cap
		{"a manifest past 4 MiB", manifest(int64(len(config))), strconv.Itoa(executor.MaxImageConfig + 1), "manifest"},
		{"a config of no known size", manifest(-1), "", "config"},
		{"a config past 4 MiB", manifest(executor.MaxImageConfig + 1), "", "config"},
	} {
		reg := &countingRegistry{manifest: tc.manifest, config: config, headLength: tc.headLength, served: map[string]int{}}
		if tc.unread == "config" {
			reg.headLength = strconv.Itoa(len(tc.manifest))
		}
		srv := httptest.NewServer(reg)
		e := &Executor{Registry: strings.TrimPrefix(srv.URL, "http://")}
		c := newScanCluster(t, e, cdxOut)
		c.meta = javaDBBuilt(time.Hour)
		addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
		req := executor.ScanRequest{Tool: "trivy", Target: "image", App: "claims-portal", Image: e.Registry + "/canyon/claims-portal@" + digestOf(tc.manifest)}
		res, err := e.Scan(t.Context(), req, noParts())
		if tc.unread == "manifest" && tc.headLength == "" && err != nil && !errors.Is(err, executor.ErrRequest) && reg.served["manifest"] == 0 {
			srv.Close()
			continue
		}
		if err != nil || res.NotFinished != "over_cap" || res.Reason != "image" || reg.served[tc.unread] != 0 {
			t.Errorf("%s: %+v %v, %d bytes of the %s served", tc.name, res, err, reg.served[tc.unread], tc.unread)
		}
		srv.Close()
	}
}

// A manifest's body is read to 4 MiB and one byte more, whatever its HEAD named, and must be the size its
// HEAD named: a registry naming 1 KiB and serving 64 MiB is refused with no more than the cap (and what the
// sockets buffer) served, and one serving a byte fewer than it named is refused too.
func TestAManifestServedPastItsHeadIsCut(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` + strings.Repeat("1a", 32) + `"]}}`)
	manifest := func(pad int) []byte { // one layer, so a manifest let through goes on to a Job
		return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":` +
			strconv.Itoa(len(config)) + `,"digest":"` + digestOf(config) + `"},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":10,"digest":"sha256:` +
			strings.Repeat("2b", 32) + `"}],"annotations":{"pad":"` + strings.Repeat("x", pad) + `"}}`)
	}
	small := manifest(0)
	for _, tc := range []struct {
		name       string
		manifest   []byte
		headLength string
		bound      int
	}{
		{"a HEAD of 1 KiB, 64 MiB served", manifest(64 << 20), "1024", executor.MaxImageConfig + 8<<20},
		{"a HEAD a byte longer than what is served", small, strconv.Itoa(len(small) + 1), len(small)},
	} {
		reg := &countingRegistry{manifest: tc.manifest, config: config, headLength: tc.headLength, served: map[string]int{}}
		srv := httptest.NewServer(reg)
		e := &Executor{Registry: strings.TrimPrefix(srv.URL, "http://")}
		c := newScanCluster(t, e, cdxOut)
		c.meta = javaDBBuilt(time.Hour)
		addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
		req := executor.ScanRequest{Tool: "trivy", Target: "image", App: "claims-portal", Image: e.Registry + "/canyon/claims-portal@" + digestOf(tc.manifest)}
		_, err := e.Scan(t.Context(), req, noParts())
		srv.Close()
		if err == nil || errors.Is(err, executor.ErrRequest) || reg.served["manifest"] > tc.bound || len(jobsNamed(c, "canyon-scan-")) != 0 {
			t.Errorf("%s: %v, %d bytes of the manifest served (at most %d), %d Jobs", tc.name, err, reg.served["manifest"], tc.bound, len(jobsNamed(c, "canyon-scan-")))
		}
	}
}

// The read check joins by bom-ref and purl together: a bom-ref trivy read for another purl is not read, and
// the same purl spelled otherwise is.
func TestAnSBOMMatchJoinsByBOMRefAndPurl(t *testing.T) {
	e := &Executor{}
	u := "4c1a2b3d-1111-4222-8333-444455556666"
	c := newScanCluster(t, e, `{"SchemaVersion":2,"Results":[{"Packages":[{"Identifier":{"BOMRef":"`+u+`","PURL":"pkg:npm/lodash@4.17.21?a=1\u0026b=2"}}]}]}`)
	serveTrivy(t, c, e, time.Now().Add(-time.Hour))
	for purl, unread := range map[string]bool{"pkg:npm/lodash@4.17.15?a=1&b=2": true, "pkg:NPM/lodash@4.17.21?b=2&a=1": false} {
		version, _, _ := strings.Cut(purl[strings.Index(purl, "@")+1:], "?")
		part := `{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + u + `","type":"library","name":"lodash","version":"` + version + `","purl":"` + purl + `"}]}`
		res, err := e.Scan(context.Background(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(part))
		if err != nil || (res.Reason == "unread") != unread || (res.Report == nil) != unread {
			t.Errorf("%s: %+v %v", purl, res, err)
		}
	}
}

// The answer names what trivy lists but never scans, as on Docker: a conda package.
func TestAnSBOMAnswerNamesWhatTrivyNeverScans(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, `{"SchemaVersion":2,"Results":[{"Packages":[{"Identifier":{"BOMRef":"pkg:conda/numpy@1.26.4","PURL":"pkg:conda/numpy@1.26.4"}}]}]}`)
	serveTrivy(t, c, e, time.Now().Add(-time.Hour))
	part := `{"bomFormat":"CycloneDX","components":[{"bom-ref":"pkg:conda/numpy@1.26.4","type":"library","name":"numpy","version":"1.26.4","purl":"pkg:conda/numpy@1.26.4"}]}`
	res, err := e.Scan(context.Background(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, sbomParts(part))
	if err != nil || res.Report == nil || !slices.Equal(res.Unscanned, []string{"pkg:conda/numpy@1.26.4"}) {
		t.Errorf("%+v %v", res, err)
	}
}
