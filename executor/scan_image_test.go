package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	layerID   = "sha256:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a"
	appID     = "sha256:abababababababababababababababababababababababababababababababab"
	imageReq  = `{"tool":"trivy","target":"image","app":"claims-portal","image":"canyon/claims-portal@` + appID + `"}`
	sbomReq   = `{"tool":"trivy","target":"sbom"}`
	libssl3   = "pkg:deb/ubuntu/libssl3@3.0.13-0ubuntu3.4?distro=ubuntu-24.04"
	cdxOut    = `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"ubuntu","version":"24.04"},{"bom-ref":"` + libssl3 + `","type":"library","name":"libssl3","version":"3.0.13-0ubuntu3.4","purl":"` + libssl3 + `","properties":[{"name":"aquasecurity:trivy:LayerDiffID","value":"` + layerID + `"}]}]}`
	reportOut = `{"SchemaVersion":2,"ArtifactType":"cyclonedx","Results":[{"Target":"/scan/sbom.cdx.json (ubuntu 24.04)","Packages":[{"Name":"libssl3","Identifier":{"PURL":"` + libssl3 + `","BOMRef":"` + libssl3 + `"}}],"Vulnerabilities":[{"VulnerabilityID":"CVE-2024-6119","PkgName":"libssl3","PkgIdentifier":{"PURL":"` + libssl3 + `","BOMRef":"` + libssl3 + `"},"Layer":{"DiffID":"` + layerID + `"}}]}]}`
	lifecycle = `{"runImage":{"topLayer":"` + layerID + `"}}`
	osRef     = "943a80d7-07b1-4635-8fb3-080725e3f260" // trivy names a component with no purl, or a purl that repeats, by a UUID
)

// imageFake is dbFake with claims-portal's image on the daemon, size bytes, two layers and its lifecycle
// label, the tool printing out.
func imageFake(t *testing.T, meta string, size int64, out string) (string, *Executor, string) {
	srv, e, dir := dbFake(t, meta)
	must(t, os.WriteFile(filepath.Join(dir, "saved"), layoutTar(t, savedLayout()), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "images"), []byte(appID+" canyon/claims-portal:pkg_7d3e8f-1\n"), 0o600))
	inspect(t, dir, size, lifecycle, []string{layerID, appID})
	must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+out+"'\n"), 0o600))
	return srv.URL, e, dir
}

// entry is one entry of a tar the fake's docker save writes.
type entry struct {
	name string
	flag byte
	body []byte
}

// blob is the layout path of a blob named by c repeated.
func blob(c string) string { return "blobs/sha256/" + strings.Repeat(c, 64) }

// savedLayout is what Docker 29's containerd store saves of a pack-built image: the OCI layout, its two
// directory entries, a legacy manifest.json naming the blobs as layers, and layers that mix compressions,
// one uncompressed and one gzip (#2585).
func savedLayout() []entry {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(layoutTar(nil, []entry{{name: "etc/os-release", body: []byte("ID=ubuntu\n")}}))
	_ = w.Close()
	return []entry{
		{name: "blobs/", flag: tar.TypeDir},
		{name: "blobs/sha256/", flag: tar.TypeDir},
		{name: blob("1"), body: layoutTar(nil, []entry{{name: "bin/sh", body: []byte("#!")}})},
		{name: blob("2"), body: gz.Bytes()},
		{name: "index.json", body: []byte(`{"schemaVersion":2,"manifests":[]}`)},
		{name: "manifest.json", body: []byte(`[{"Layers":["` + blob("1") + `","` + blob("2") + `"]}]`)},
		{name: "oci-layout", body: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{name: "repositories", body: []byte(`{}`)},
	}
}

// layoutTar is entries as a tar, each a regular file unless its flag says otherwise.
func layoutTar(t *testing.T, entries []entry) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: cmp.Or(e.flag, tar.TypeReg), Mode: 0o444, Size: int64(len(e.body))}
		switch h.Typeflag {
		case tar.TypeDir:
			h.Size, h.Mode = 0, 0o755
		case tar.TypeSymlink, tar.TypeLink:
			h.Size, h.Linkname = 0, "/etc/passwd"
		}
		if err := tw.WriteHeader(h); err != nil && t != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write(e.body)
		}
	}
	_ = tw.Close()
	return b.Bytes()
}

// inspect is what docker image inspect answers of the app's image.
func inspect(t *testing.T, dir string, size int64, label string, layers []string) {
	l, _ := json.Marshal(label)
	ls, _ := json.Marshal(layers)
	must(t, os.WriteFile(filepath.Join(dir, "inspect"), []byte(`{"Id":"`+appID+`","Size":`+strconv.FormatInt(size, 10)+`,"Config":{"Labels":{"io.buildpacks.lifecycle.metadata":`+string(l)+`}},"RootFS":{"Type":"layers","Layers":`+string(ls)+`}}`), 0o600))
}

// postParts posts the json part, then each named part.
func postParts(t *testing.T, url, req string, parts ...[2]string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(req)))
	for _, p := range parts {
		f, _ := mw.CreateFormFile(p[0], p[0])
		must(t, write(f, []byte(p[1])))
	}
	must(t, mw.Close())
	resp, err := http.Post(url+"/scan", mw.FormDataContentType(), &body) //nolint:gosec,noctx // the test server
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// withImageMax runs the test with CANYON_SCAN_IMAGE_MAX at max.
func withImageMax(t *testing.T, max int64) {
	old := imageMax
	imageMax = max
	t.Cleanup(func() { imageMax = old })
}

var fresh = builtAgo(time.Hour)

const overCap = `"not_finished":"over_cap","reason":"image"}`

// An image scan reads the app's image once its Java database is there: saved outside what is copied in, its
// OCI layout alone unpacked, readable by the scan's user, into /scan/image beside a tmp/ the scan's user
// writes — the legacy manifest.json and repositories dropped, a gzip blob among uncompressed ones kept as it
// is (#2585) — then trivy image lists it as CycloneDX
// on the Java database alone, offline and read-only, its TMPDIR there, the lifecycle's SBOM documents skipped
// (they name packages listed again in their own layers), and the answer is that SBOM with the
// image's lifecycle label and diff IDs in order, matched against nothing.
func TestScanImageSavesTheImageAndAnswersItsSBOM(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, cdxOut)
	status, body := postParts(t, url, imageReq)
	var res ScanResult
	if err := json.Unmarshal([]byte(body), &res); status != 200 || err != nil || string(res.SBOM) != cdxOut || res.Lifecycle != lifecycle || res.Image != trivyImage || !slices.Equal(res.DiffIDs, []string{layerID, appID}) ||
		res.DBBuiltAt != "" || res.SARIF != nil || res.NotFinished != "" {
		t.Fatalf("%d %s", status, body)
	}
	argv := readIf(dir, "argv")
	for _, want := range []string{
		"--volume\ncanyon-scandb-trivy-java:/db:ro\n--entrypoint\nsh\n" + trivyImage + "\n-c\n" + `[ "$(head -c 15 /db/java-db/trivy-java.db)" = "SQLite format 3" ] || exit 1; exec head -c 1048576 /db/java-db/metadata.json` + "\n", // the Java database's reader first
		"save\n--output\n",
		"--network\nnone\n--read-only\n--user\n65534:65534\n",
		"--volume\n/scan\n--env\nTMPDIR=/scan/tmp\n--volume\ncanyon-scandb-trivy-java:/scan/db:ro\n" + trivyImage + "\nimage\n--input\n/scan/image\n--format\ncyclonedx\n--cache-dir\n/scan/db\n--cache-backend\nmemory\n" +
			"--ignorefile\n/dev/null\n--skip-db-update\n--skip-java-db-update\n--offline-scan\n--disable-telemetry\n--skip-version-check\n--no-progress\n" +
			"--skip-files\n/layers/sbom/**/sbom.cdx.json\n--skip-files\n/layers/sbom/**/sbom.spdx.json\n--skip-files\n/layers/sbom/**/sbom.syft.json\n--skip-files\n/layers/sbom/**/sbom.legacy.json\ncp\n",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("docker argv lacks %q:\n%s", want, argv)
		}
	}
	if !strings.Contains(argv, "/image.tar\n"+appID+"\n") || strings.Contains(argv, "/scan/image.tar\n") || strings.Index(argv, "/db/java-db/metadata.json") > strings.Index(argv, "save\n") {
		t.Errorf("the image is saved by its ID, after the Java database is read:\n%s", argv)
	}
	if strings.Contains(argv, "canyon-scandb-trivy:") || strings.Contains(argv, "--list-all-pkgs") || strings.Contains(argv, "--skip-check-update") {
		t.Errorf("an image scan reads trivy's main database, or takes a flag trivy image does not:\n%s", argv)
	}
	cp, mode, tmp := readIf(dir, "cp"), readIf(dir, "mode"), readIf(dir, "tmpmode")
	if want := "./image\n./image/blobs\n./image/blobs/sha256\n./image/" + blob("1") + "\n./image/" + blob("2") + "\n./image/index.json\n./image/oci-layout\n./tmp\n"; !strings.HasSuffix(cp, want) ||
		strings.TrimSpace(mode) != "-rw-r--r--" || strings.TrimSpace(tmp) != "drwxrwxrwt" {
		t.Errorf("/scan holds %q, index.json %q, tmp %q: want the OCI layout alone, readable, and tmp writable by the scan's user", cp, mode, tmp)
	}
}

// The saved image is unpacked fail closed (contracts/executor.md, target image; #2585): only oci-layout,
// index.json and blobs/sha256/<64 hex>, regular files each once, with both of the first two. Anything else
// answers not_sarif, never an SBOM, and trivy never runs.
func TestScanImageUnpacksOnlyTheOCILayout(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, cdxOut)
	with := func(extra ...entry) []entry { return append(savedLayout(), extra...) }
	without := func(name string) []entry {
		return slices.DeleteFunc(savedLayout(), func(e entry) bool { return e.name == name })
	}
	for name, entries := range map[string][]entry{
		"another name":               with(entry{name: "etc/passwd", body: []byte("root")}),
		"a name that climbs out":     with(entry{name: "../escape", body: []byte("x")}),
		"a blob not named by sha256": with(entry{name: "blobs/sha256/" + strings.Repeat("A", 64), body: []byte("x")}),
		"a blob of another digest":   with(entry{name: "blobs/sha512/" + strings.Repeat("1", 128), body: []byte("x")}),
		"a nested blob":              with(entry{name: blob("3") + "/x", body: []byte("x")}),
		"a symlink":                  with(entry{name: blob("4"), flag: tar.TypeSymlink}),
		"a hard link":                with(entry{name: blob("5"), flag: tar.TypeLink}),
		"a device":                   with(entry{name: blob("6"), flag: tar.TypeChar}),
		"another directory":          with(entry{name: "blobs/sha256/x/", flag: tar.TypeDir}),
		"a blob twice":               with(entry{name: blob("1"), body: []byte("again")}),
		"no index.json":              without("index.json"),
		"no oci-layout":              without("oci-layout"),
		"an entry cut short":         nil,
		"no tar at all":              nil,
	} {
		raw := layoutTar(t, entries)
		switch name {
		case "an entry cut short":
			raw = layoutTar(t, savedLayout())[:1024+512+10] // a blob's header, then part of its body
		case "no tar at all":
			raw = []byte("the image")
		}
		must(t, os.WriteFile(filepath.Join(dir, "saved"), raw, 0o600))
		_ = os.Remove(filepath.Join(dir, "argv"))
		status, body := postParts(t, url, imageReq)
		if want := `{"tool":"trivy","image":"` + trivyImage + `","not_finished":"not_sarif"}`; status != 200 || strings.TrimSpace(body) != want {
			t.Errorf("%s: %d %s, want %s", name, status, body, want)
		}
		if argv := readIf(dir, "argv"); !strings.Contains(argv, "save\n") || strings.Contains(argv, "create\n") {
			t.Errorf("%s: the image was not saved, or trivy ran:\n%s", name, argv)
		}
	}
}

// An image at CANYON_SCAN_IMAGE_MAX is read; one byte over it, or saved past it, more than 128 layers, or a
// lifecycle label over 1 MiB, is over_cap, reason image, before anything runs.
func TestScanImageOverCap(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 8<<30, cdxOut)
	if status, body := postParts(t, url, imageReq); status != 200 || !strings.Contains(body, `"sbom":`) {
		t.Fatalf("an image of exactly its cap: %d %s", status, body)
	}
	many := make([]string, 129)
	for i := range many {
		many[i] = "sha256:" + strings.Repeat(strconv.Itoa(i%10), 64)
	}
	for name, set := range map[string]func(){
		"one byte over":    func() { inspect(t, dir, 8<<30+1, lifecycle, []string{layerID}) },
		"129 layers":       func() { inspect(t, dir, 1<<20, lifecycle, many) },
		"a label of 1 MiB": func() { inspect(t, dir, 1<<20, strings.Repeat("x", MaxLifecycle+1), []string{layerID}) },
	} {
		_ = os.Remove(filepath.Join(dir, "argv"))
		set()
		status, body := postParts(t, url, imageReq)
		if want := `{"tool":"trivy","image":"` + trivyImage + `",` + overCap; status != 200 || strings.TrimSpace(body) != want {
			t.Errorf("%s: %d %s, want %s", name, status, body, want)
		}
		if argv := readIf(dir, "argv"); strings.Contains(argv, "save\n") || strings.Contains(argv, "create\n") || strings.Contains(argv, "run\n") {
			t.Errorf("%s: an image past its bounds was saved or ran:\n%s", name, argv)
		}
	}
	inspect(t, dir, 4, lifecycle, []string{layerID}) // what docker save writes, past 5 bytes, is what counts
	withImageMax(t, 5)
	if status, body := postParts(t, url, imageReq); status != 200 || !strings.HasSuffix(strings.TrimSpace(body), overCap) || strings.Contains(readIf(dir, "argv"), "create\n") {
		t.Errorf("an image saved past its cap: %d %s", status, body)
	}
}

// An image scan without its Java database never answers an SBOM: the first starts the database's
// download into its own volume, its download staged in the volume's tmp/, emptied first, and is busy;
// once that fails it is db_missing, and a database built 6 days ago db_stale, each naming the Java
// database. The image is never saved.
func TestScanImageWithoutItsJavaDatabaseIsNotFinished(t *testing.T) {
	url, e, dir := imageFake(t, "", 1<<30, cdxOut)
	must(t, os.WriteFile(filepath.Join(dir, "norefresh"), nil, 0o600))
	if status, body := postParts(t, url, imageReq); status != 503 || strings.TrimSpace(body) != `{"error":"busy","reason":"database"}` {
		t.Fatalf("the first image scan: %d %s", status, body)
	}
	e.collecting.Wait() // the download the scan started, which fails
	if argv := readIf(dir, "argv"); !strings.Contains(argv, "--volume\ncanyon-scandb-trivy-java:/db\n--env\nTMPDIR=/db/tmp\n--entrypoint\nsh\n"+trivyImage+"\n-c\n"+`rm -rf /db/tmp && mkdir /db/tmp && exec trivy "$@"`+"\nsh\nimage\n--cache-dir\n/db\n--download-java-db-only\n") {
		t.Errorf("the Java database's refresh:\n%s", argv)
	}
	status, body := postParts(t, url, imageReq)
	if want := `{"tool":"trivy","image":"` + trivyImage + `","not_finished":"db_missing","reason":"java"}`; status != 200 || strings.TrimSpace(body) != want {
		t.Errorf("once its download failed: %d %s, want %s", status, body, want)
	}
	must(t, os.WriteFile(filepath.Join(dir, "meta"), []byte(builtAgo(6*24*time.Hour)), 0o600))
	status, body = postParts(t, url, imageReq)
	if want := `{"tool":"trivy","image":"` + trivyImage + `","not_finished":"db_stale","reason":"java"}`; status != 200 || strings.TrimSpace(body) != want {
		t.Errorf("built 6 days ago: %d %s, want %s", status, body, want)
	}
	if argv := readIf(dir, "argv"); strings.Contains(argv, "save\n") || strings.Contains(argv, "start\n") {
		t.Errorf("an image scan without its Java database saved the image or ran:\n%s", argv)
	}
}

// Image scans run one at a time: a second while one runs answers busy with no reason, never waiting.
func TestScanImageScansRunOneAtATime(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, cdxOut)
	imageScans.Lock() // an image scan in flight
	status, body := postParts(t, url, imageReq)
	imageScans.Unlock()
	if status != 503 || strings.TrimSpace(body) != `{"error":"busy"}` || strings.Contains(readIf(dir, "argv"), "image\nls\n") {
		t.Errorf("a second image scan: %d %s", status, body)
	}
}

// An image or sbom scan whose tool wrote no document is not_sarif; one the OOM killer or a signal ended is
// killed; one past 10 MiB is over_cap, reason report.
func TestScanImageOrSBOMThatDidNotFinish(t *testing.T) {
	for _, tc := range []struct {
		name, req, out, killed, want string
		parts                        [][2]string
	}{
		{"an image, no SBOM", imageReq, "Traceback: …", "", `"not_finished":"not_sarif"}`, nil},
		{"an image, SARIF", imageReq, sarifOut, "", `"not_finished":"not_sarif"}`, nil},
		{"an image, killed", imageReq, "{", "true 137", `"not_finished":"killed"}`, nil},
		{"an sbom, killed", sbomReq, "{", "false 137", `"not_finished":"killed"}`, [][2]string{{"sbom", cdxOut}}},
		{"an image, past 10 MiB", imageReq, `{"bomFormat":"CycloneDX","x":"` + strings.Repeat("x", MaxScanOut) + `"}`, "", `"not_finished":"over_cap","reason":"report"}`, nil},
		{"an sbom, past 10 MiB", sbomReq, `{"SchemaVersion":2,"x":"` + strings.Repeat("x", MaxScanOut) + `"}`, "", `"not_finished":"over_cap","reason":"report"}`, [][2]string{{"sbom", cdxOut}}},
	} {
		url, _, dir := imageFake(t, fresh, 1<<30, "")
		must(t, os.WriteFile(filepath.Join(dir, "out"), []byte(tc.out), 0o600))
		must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("cat '"+filepath.Join(dir, "out")+"'\n"), 0o600))
		must(t, os.WriteFile(filepath.Join(dir, "killed"), []byte(tc.killed), 0o600))
		if status, body := postParts(t, url, tc.req, tc.parts...); status != 200 || !strings.HasSuffix(strings.TrimSpace(body), tc.want) {
			t.Errorf("%s: %d %.300s, want …%s", tc.name, status, body, tc.want)
		}
	}
}

// An sbom scan re-matches the part on trivy's own database, offline and read-only, reading no image and no
// Java database, and answers trivy's JSON report — each vulnerability with its layer and its component —
// and when that database was built; a report a vulnerability of which carries neither is no answer.
func TestScanSBOMMatchesThePartOnTrivysDatabase(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, reportOut)
	status, body := postParts(t, url, sbomReq, [2]string{"sbom", cdxOut})
	var res ScanResult
	if err := json.Unmarshal([]byte(body), &res); status != 200 || err != nil || string(res.Report) != reportOut || res.DBBuiltAt == "" || res.SBOM != nil || res.Lifecycle != "" {
		t.Fatalf("%d %s", status, body)
	}
	if want := "--volume\ncanyon-scandb-trivy:/scan/db:ro\n" + trivyImage + "\nsbom\n--format\njson\n--list-all-pkgs\n--scanners\nvuln\n--cache-dir\n/scan/db\n--ignorefile\n/dev/null\n--skip-db-update\n--skip-java-db-update\n" +
		"--offline-scan\n--disable-telemetry\n--skip-version-check\n--no-progress\n/scan/sbom.cdx.json\ncp\n"; !strings.Contains(readIf(dir, "argv"), want) {
		t.Errorf("docker argv lacks %q:\n%s", want, readIf(dir, "argv"))
	}
	if argv := readIf(dir, "argv"); strings.Contains(argv, "trivy-java") || strings.Contains(argv, "save\n") || strings.Contains(argv, "TMPDIR") {
		t.Errorf("a re-match read the Java database or an image:\n%s", argv)
	}
	if readIf(dir, "sbom") != cdxOut {
		t.Errorf("/scan/sbom.cdx.json is %q, not the part", readIf(dir, "sbom"))
	}
	for name, out := range map[string]string{
		"no layer":       strings.Replace(reportOut, `,"Layer":{"DiffID":"`+layerID+`"}`, "", 1),
		"no component":   strings.Replace(reportOut, `,"BOMRef":"`+libssl3+`"},"Layer"`, `},"Layer"`, 1),
		"another schema": strings.Replace(reportOut, `"SchemaVersion":2`, `"SchemaVersion":1`, 1),
		"no results":     `{"SchemaVersion":2}`,
		"SARIF":          sarifOut,
	} {
		must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+out+"'\n"), 0o600))
		if status, body := postParts(t, url, sbomReq, [2]string{"sbom", cdxOut}); status != 200 || !strings.Contains(body, `"not_finished":"not_sarif"`) {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
}

// An sbom part is refused, naming why, unless it has the shape of a slice of trivy's own image SBOM: a
// metadata component other than trivy's bare container (its descendants trivy never reads); a component of
// a type trivy does not write, nesting others, without its own unique bom-ref — its purl, or a UUID — or
// with a purl off a library; a purl packageurl-go does not parse; a name, group or version that is not what
// trivy writes beside its purl (trivy matches by the name); or operating-system packages beside other than
// one operating-system component. What trivy itself writes — a scoped npm package, a PyPI name with an
// underscore, a Go module with capitals — passes. Each answer here follows from the part: what is refused
// never runs.
func TestScanSBOMRefusesAPartNotShapedAsTrivyWritesIt(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, reportOut)
	system := `{"bom-ref":"` + osRef + `","type":"operating-system","name":"ubuntu","version":"24.04"}`
	deb := `{"bom-ref":"` + libssl3 + `","type":"library","name":"libssl3","version":"3.0.13-0ubuntu3.4","purl":"` + libssl3 + `"}`
	jar := `{"bom-ref":"pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1","type":"library","group":"org.apache.logging.log4j","name":"log4j-core","version":"2.14.1","purl":"pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1"}`
	lib := func(ref, group, name, version, purl string) string {
		return `{"bom-ref":"` + ref + `","type":"library","group":"` + group + `","name":"` + name + `","version":"` + version + `","purl":"` + purl + `"}`
	}
	part := func(components ...string) string {
		return `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[` + strings.Join(components, ",") + `]}`
	}
	withMetadata := func(metadata string, components ...string) string {
		return `{"bomFormat":"CycloneDX","specVersion":"1.6","metadata":{"component":` + metadata + `},"components":[` + strings.Join(components, ",") + `]}`
	}
	container := `{"bom-ref":"6b5a5fb4-6f57-49e1-8fed-be93b4d845b4","type":"container","name":"/scan/image.tar"}`
	for _, tc := range []struct {
		name, part, refusal string
	}{
		{"a deb package beside its operating system", part(system, deb), ""},
		{"a jar alone, beside trivy's container", withMetadata(container, jar), ""},
		{"a scoped npm package", part(lib("pkg:npm/%40babel/core@7.0.0", "@babel", "core", "7.0.0", "pkg:npm/%40babel/core@7.0.0")), ""},
		{"a PyPI name with an underscore", part(lib("pkg:pypi/zope_interface@6.0", "", "zope_interface", "6.0", "pkg:pypi/zope_interface@6.0")), ""},
		{"a Go module with capitals", part(lib("pkg:golang/github.com/azure/go-autorest@v14.2.0", "", "github.com/Azure/go-autorest", "v14.2.0", "pkg:golang/github.com/azure/go-autorest@v14.2.0")), ""},
		{"an epoch before the version", part(system, lib("pkg:deb/ubuntu/perl@5.38.2?epoch=1&distro=ubuntu-24.04", "", "perl", "1:5.38.2", "pkg:deb/ubuntu/perl@5.38.2?epoch=1&distro=ubuntu-24.04")), ""},
		{"metadata.component with descendants", withMetadata(strings.Replace(container, `}`, `,"components":[`+jar+`]}`, 1)), "metadata.component is not trivy's container"},
		{"metadata.component a library", withMetadata(jar), "metadata.component is not trivy's container"},
		{"a component sharing the container's bom-ref", withMetadata(container, strings.Replace(system, osRef, "6b5a5fb4-6f57-49e1-8fed-be93b4d845b4", 1)), `bom-ref \"6b5a5fb4-6f57-49e1-8fed-be93b4d845b4\" names more than one component`},
		{"a deb package named otherwise than its purl", part(system, strings.Replace(deb, `"name":"libssl3"`, `"name":"harmless"`, 1)), `its name \"harmless\" is not one trivy writes`},
		{"an npm package named otherwise than its purl", part(lib("pkg:npm/lodash@4.17.15", "", "lodash-evil", "4.17.15", "pkg:npm/lodash@4.17.15")), `its group and name \"\" \"lodash-evil\" are not its purl's`},
		{"a version not its purl's", part(system, strings.Replace(deb, `"version":"3.0.13-0ubuntu3.4"`, `"version":"9.9"`, 1)), `its version \"9.9\" is not its purl's`},
		{"a group off npm and maven", part(lib("pkg:pypi/requests@2.0.0", "evil", "requests", "2.0.0", "pkg:pypi/requests@2.0.0")), `it has group \"evil\"`},
		{"a purl that does not parse", part(lib("4c1a2b3d-1111-4222-8333-444455556666", "", "bad", "", "pkg:npm")), `component \"4c1a2b3d-1111-4222-8333-444455556666\": purl is missing type or name`},
		{"a purl on an application", part(`{"bom-ref":"pkg:npm/a@1","type":"application","name":"a","purl":"pkg:npm/a@1"}`), "carries a purl, which trivy writes on a library alone"},
		{"a bom-ref an embedded SBOM gave it", part(lib("log4j-core", "org.apache.logging.log4j", "log4j-core", "2.14.1", "pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1")), ""},
		{"a bom-ref past 2 KiB", part(strings.Replace(deb, `"bom-ref":"`+libssl3+`"`, `"bom-ref":"`+strings.Repeat("r", 2<<10+1)+`"`, 1), system), "a bom-ref of more than 2048 bytes"},
		{"no bom-ref", part(strings.Replace(deb, `"bom-ref":"`+libssl3+`",`, "", 1)), "a component has no bom-ref"},
		{"two components sharing a bom-ref", part(system, deb, deb), `bom-ref \"` + libssl3 + `\" names more than one component`},
		{"a deb package beside no operating system", part(deb), "1 operating-system packages beside 0 operating-system components, not one"},
		{"a component without its type", part(strings.Replace(system, `"type":"operating-system",`, "", 1)), `is of type \"\", not one trivy writes`},
		{"a component of a type trivy does not write", part(strings.Replace(system, `"type":"operating-system"`, `"type":"file"`, 1)), `is of type \"file\", not one trivy writes`},
		{"a nested component", part(strings.Replace(jar, `}`, `,"components":[`+deb+`]}`, 1)), "nests components, which trivy does not read"},
	} {
		_ = os.Remove(filepath.Join(dir, "argv"))
		status, body := postParts(t, url, sbomReq, [2]string{"sbom", tc.part})
		ran := strings.Contains(readIf(dir, "argv"), "start\n")
		switch {
		case tc.refusal == "" && (status != 200 || !ran):
			t.Errorf("%s: %d %s, want it re-matched", tc.name, status, body)
		case tc.refusal != "" && (status != 400 || !strings.Contains(body, tc.refusal) || ran):
			t.Errorf("%s: %d %s, want 400 naming %q", tc.name, status, body, tc.refusal)
		}
	}
}

// A part is parsed once, bounded as it streams, and written again: every array, string, the depth and the
// count of tokens over the whole document are bounded before anything is built past them, and a document
// two readers could read apart — a key twice, invalid UTF-8, a lone surrogate, a byte-order mark, a second
// document — is refused. A 9 MB part of {} is refused at its 50,001st, what it allocates bounded by the
// caps — under 8 MiB — never by the part.
func TestScanSBOMRefusesAPartPastItsBoundsBeforeDecodingIt(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, reportOut)
	system := `{"bom-ref":"` + osRef + `","type":"operating-system","name":"ubuntu","version":"24.04"}`
	deb := `{"bom-ref":"` + libssl3 + `","type":"library","name":"libssl3","version":"3.0.13-0ubuntu3.4","purl":"` + libssl3 + `"}`
	empties := `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[` + strings.Repeat(`{},`, 3_000_000) + `{}]}`
	for name, tc := range map[string]struct{ part, refusal string }{
		"3 million {}":                     {empties, "an array of more than 50000 values"},
		"nested 19 deep in components":     {`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"library","properties":` + strings.Repeat(`[`, 16) + strings.Repeat(`]`, 16) + `}]}`, "nested more than 16 deep"},
		"nested 17 deep outside them":      {`{"bomFormat":"CycloneDX","metadata":{"properties":` + strings.Repeat(`[`, 15) + strings.Repeat(`]`, 15) + `},"components":[]}`, "nested more than 16 deep"},
		"50,001 properties":                {`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","properties":[` + strings.Repeat(`{"name":"a","value":"b"},`, 50_000) + `{"name":"a","value":"b"}]}]}`, "an array of more than 50000 values"},
		"a string past 64 KiB":             {`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"` + strings.Repeat("x", 64<<10+1) + `"}]}`, "of more than 65536 bytes"},
		"components twice, the second bad": {`{"bomFormat":"CycloneDX","components":[` + system + `,` + deb + `],"components":[` + strings.Replace(deb, `"name":"libssl3"`, `"name":"harmless"`, 1) + `]}`, `an object has the key \"components\" twice`},
		"a name twice in a component":      {`{"bomFormat":"CycloneDX","components":[` + system + `,` + strings.Replace(deb, `"name":"libssl3"`, `"name":"libssl3","name":"harmless"`, 1) + `]}`, `an object has the key \"name\" twice`},
		"invalid UTF-8":                    {`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"` + "\xff" + `"}]}`, "it is not valid UTF-8"},
		"a lone surrogate":                 {`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"\ud800"}]}`, "lone surrogate"},
		"a byte-order mark":                {"\xef\xbb\xbf" + cdxOut, "byte-order mark"},
		"a second document":                {cdxOut + cdxOut, "more follows its one JSON document"},
	} {
		if status, body := postParts(t, url, sbomReq, [2]string{"sbom", tc.part}); status != 400 || !strings.Contains(body, tc.refusal) {
			t.Errorf("%s: %d %.300s, want 400 naming %q", name, status, body, tc.refusal)
		}
	}
	// what trivy reads is the part written again, never its bytes: escapes and spaces gone, one key each
	spelled := strings.Replace(strings.Replace(cdxOut, `"name":"libssl3"`, `"name":"lib\u0073sl3"`, 1), `,"components"`, ` , "components"`, 1)
	if status, body := postParts(t, url, sbomReq, [2]string{"sbom", spelled}); status != 200 || readIf(dir, "sbom") != cdxOut {
		t.Errorf("a part spelled otherwise: %d %.300s; trivy read %q, want %q", status, body, readIf(dir, "sbom"), cdxOut)
	}
	b := []byte(empties)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := checkPart(b)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; err == nil || allocated > 8<<20 {
		t.Errorf("checking 3 million {} allocated %d bytes: built before it was bounded (%v)", allocated, err)
	}
}

// An image or sbom target is trivy's with only its own keys and parts; an image is the app's by digest,
// and one the daemon does not hold is 404, as /run's; the sbom part is at most 10 MiB.
func TestScanRefusesAnImageOrSBOMTargetOutsideItsContract(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, cdxOut)
	other := strings.Replace(imageReq, "claims-portal\",", "case-intake\",", 1)
	padded := func(n int) string { // a CycloneDX part of exactly n bytes, its metadata's properties strings within their bound
		head, tail := `{"bomFormat":"CycloneDX","components":[],"metadata":{"properties":[`, `]}}`
		var b strings.Builder
		b.WriteString(head)
		for left := n - len(head) - len(tail); left > 0; {
			if b.Len() > len(head) {
				b.WriteByte(',')
				left--
			}
			size := min(left-2, 60_000)
			b.WriteString(`"` + strings.Repeat("x", size) + `"`)
			left -= size + 2
		}
		return b.String() + tail
	}
	for _, tc := range []struct {
		name, req string
		parts     [][2]string
		status    int
	}{
		{"grype's image", strings.Replace(imageReq, `"trivy"`, `"grype"`, 1), nil, 400},
		{"settings", strings.Replace(imageReq, `}`, `,"settings":{}}`, 1), nil, 400},
		{"a package", strings.Replace(imageReq, `}`, `,"package":"pkg_7d3e8f"}`, 1), nil, 400},
		{"a tree", imageReq, [][2]string{{"tree", string(tarOf(t, map[string]string{"a": "b"}))}}, 400},
		{"an image by tag", strings.Replace(imageReq, "@"+appID, ":latest", 1), nil, 400},
		{"a digest one short", strings.Replace(imageReq, appID, appID[:len(appID)-1], 1), nil, 400},
		{"another app's image", other, nil, 400},
		{"an image the daemon does not hold", strings.Replace(imageReq, appID, "sha256:"+strings.Repeat("cd", 32), 1), nil, 404},
		{"an sbom naming an app", `{"tool":"trivy","target":"sbom","app":"claims-portal"}`, [][2]string{{"sbom", cdxOut}}, 400},
		{"an sbom of grype", `{"tool":"grype","target":"sbom"}`, [][2]string{{"sbom", cdxOut}}, 400},
		{"no sbom part", sbomReq, nil, 400},
		{"an sbom that is not CycloneDX", sbomReq, [][2]string{{"sbom", `{"bomFormat":"SPDX"}`}}, 400},
		{"two documents", sbomReq, [][2]string{{"sbom", cdxOut + cdxOut}}, 400},
		{"an sbom one byte past 10 MiB", sbomReq, [][2]string{{"sbom", padded(MaxScanOut + 1)}}, 400},
		{"an sbom of exactly 10 MiB", sbomReq, [][2]string{{"sbom", padded(MaxScanOut)}}, 200},
		{"a part after the sbom", sbomReq, [][2]string{{"sbom", cdxOut}, {"tree", "x"}}, 400},
		{"another target", strings.Replace(sbomReq, `"sbom"`, `"repo"`, 1), nil, 400},
		{"an image on a tree scan", `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","settings":{},"image":"canyon/claims-portal@` + appID + `"}`, [][2]string{{"tree", string(tarOf(t, map[string]string{"a": "b"}))}}, 400},
	} {
		if status, body := postParts(t, url, tc.req, tc.parts...); status != tc.status {
			t.Errorf("%s: %d %.300s, want %d", tc.name, status, body, tc.status)
		}
	}
	if argv := readIf(dir, "argv"); strings.Contains(argv, "save\n") {
		t.Errorf("a refused image scan saved its image:\n%s", argv)
	}
}

// At start the executor removes the staging directories a crash left — an image scan's image.tar among
// them — and no build's.
func TestSweepRemovesTheScansStagingACrashLeft(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	e, _ := fakes(t)
	for _, d := range []string{"canyon-scan-1234/scan", "canyon-build-5678"} {
		must(t, os.MkdirAll(filepath.Join(tmp, d), 0o700))
	}
	must(t, os.WriteFile(filepath.Join(tmp, "canyon-scan-1234/scan/image.tar"), []byte("the image"), 0o600))
	e.Sweep(context.Background())
	if _, err := os.Stat(filepath.Join(tmp, "canyon-scan-1234")); !os.IsNotExist(err) {
		t.Errorf("a scan's staging outlived the sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "canyon-build-5678")); err != nil {
		t.Errorf("the sweep took a build's: %v", err)
	}
}

// After the run, every component of the part carrying a purl must be among the packages trivy lists as read,
// by its bom-ref and its purl as packageurl-go normalises them: one it skipped — a purl of a type it does
// not match — or one whose bom-ref trivy read for another purl makes the answer not_sarif, reason unread,
// never the clean report trivy wrote. A purl trivy reports in its canonical spelling — the npm scope's @
// percent-encoded, a PyPI underscore a dash — is the part's own: read, never refused.
func TestScanSBOMAnswersNoCleanReportOfAComponentTrivyDidNotRead(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, reportOut)
	uuid1 := "4c1a2b3d-1111-4222-8333-444455556666"
	read := func(ref, purl string) string { // trivy's report, listing one package it read
		return `{"SchemaVersion":2,"Results":[{"Packages":[{"Identifier":{"BOMRef":"` + ref + `","PURL":"` + purl + `"}}]}]}`
	}
	lib := func(ref, group, name, version, purl string) string {
		return `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"bom-ref":"` + ref + `","type":"library","group":"` + group + `","name":"` + name + `","version":"` + version + `","purl":"` + purl + `"}]}`
	}
	for _, tc := range []struct{ name, report, part, want string }{
		{"a part trivy read whole", reportOut, cdxOut, `"report":`},
		{"a purl of a type trivy skips", reportOut, strings.TrimSuffix(cdxOut, `]}`) + `,{"bom-ref":"pkg:generic/foo@1","type":"library","name":"foo","version":"1","purl":"pkg:generic/foo@1"}]}`, `"not_finished":"not_sarif","reason":"unread"}`},
		{"a bom-ref trivy read for another purl", read(uuid1, "pkg:npm/lodash@4.17.21"), lib(uuid1, "", "lodash", "4.17.15", "pkg:npm/lodash@4.17.15"), `"not_finished":"not_sarif","reason":"unread"}`},
		{"a scoped npm purl, read as trivy spells it", read("pkg:npm/@babel/core@7.0.0", "pkg:npm/%40babel/core@7.0.0"), lib("pkg:npm/@babel/core@7.0.0", "@babel", "core", "7.0.0", "pkg:npm/@babel/core@7.0.0"), `"report":`},
		{"a PyPI underscore, read as trivy spells it", read("pkg:pypi/zope_interface@6.0", "pkg:pypi/zope-interface@6.0"), lib("pkg:pypi/zope_interface@6.0", "", "zope_interface", "6.0", "pkg:pypi/zope_interface@6.0"), `"report":`},
	} {
		must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+tc.report+"'\n"), 0o600))
		if status, body := postParts(t, url, sbomReq, [2]string{"sbom", tc.part}); status != 200 || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %.300s, want %s", tc.name, status, body, tc.want)
		}
	}
}

// Listed is not scanned: a component whose purl trivy lists but never matches — a conda package, an rpm
// of a distribution trivy has no driver for or whose driver matches nothing (CoreOS), a component without a version, or a package one of trivy's
// detectors skips by name or qualifier (gpg-pubkey; an AlmaLinux modular package without its
// modularitylabel) — is in the answer's unscanned, which the server counts as not checked; one trivy
// scans is not. A bom-ref an SBOM embedded in the image gave a component
// is trivy's own, and is joined and read like any other.
func TestScanSBOMNamesWhatTrivyListsButNeverScans(t *testing.T) {
	url, _, dir := imageFake(t, fresh, 1<<30, reportOut)
	listed := func(refs ...[2]string) string { // trivy's report, listing each package as read
		var ps []string
		for _, r := range refs {
			ps = append(ps, `{"Identifier":{"BOMRef":"`+r[0]+`","PURL":"`+r[1]+`"}}`)
		}
		return `{"SchemaVersion":2,"Results":[{"Packages":[` + strings.Join(ps, ",") + `]}]}`
	}
	conda := `{"bom-ref":"pkg:conda/numpy@1.26.4","type":"library","name":"numpy","version":"1.26.4","purl":"pkg:conda/numpy@1.26.4"}`
	jar := `{"bom-ref":"log4j-core","type":"library","group":"org.apache.logging.log4j","name":"log4j-core","version":"2.14.1","purl":"pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1"}`
	fedora := `{"bom-ref":"` + osRef + `","type":"operating-system","name":"fedora","version":"40"}`
	rpm := `{"bom-ref":"pkg:rpm/fedora/bash@5.2.26?distro=fedora-40","type":"library","name":"bash","version":"5.2.26","purl":"pkg:rpm/fedora/bash@5.2.26?distro=fedora-40"}`
	rpmOf := func(purl, name, version string) string {
		return `{"bom-ref":"` + purl + `","type":"library","name":"` + name + `","version":"` + version + `","purl":"` + purl + `"}`
	}
	redhat := strings.Replace(fedora, `"name":"fedora","version":"40"`, `"name":"redhat","version":"9.4"`, 1)
	alma := strings.Replace(fedora, `"name":"fedora","version":"40"`, `"name":"alma","version":"9.3"`, 1)
	gpg, rhBash := "pkg:rpm/redhat/gpg-pubkey@fd431d51-4ae0493b?distro=redhat-9.4", "pkg:rpm/redhat/bash@5.1.8-9.el9?distro=redhat-9.4"
	modular := "pkg:rpm/alma/nodejs@18.19.0-1.module_el9.3.0%2B38%2Bcc8ed8ad?distro=alma-9.3"
	coreosRPM := "pkg:rpm/fedora/bash@5.2.15?distro=coreos-39"
	for _, tc := range []struct {
		name, report, part, want string
	}{
		{"a conda package beside a jar an embedded SBOM named", listed([2]string{"pkg:conda/numpy@1.26.4", "pkg:conda/numpy@1.26.4"}, [2]string{"log4j-core", "pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1"}),
			`{"bomFormat":"CycloneDX","components":[` + conda + `,` + jar + `]}`, `"unscanned":["pkg:conda/numpy@1.26.4"]}`},
		{"an rpm of a distribution trivy has no driver for", listed([2]string{"pkg:rpm/fedora/bash@5.2.26?distro=fedora-40", "pkg:rpm/fedora/bash@5.2.26?distro=fedora-40"}),
			`{"bomFormat":"CycloneDX","components":[` + fedora + `,` + rpm + `]}`, `"unscanned":["pkg:rpm/fedora/bash@5.2.26?distro=fedora-40"]}`},
		{"an rpm on CoreOS, whose driver matches nothing", listed([2]string{coreosRPM, coreosRPM}),
			`{"bomFormat":"CycloneDX","components":[` + strings.Replace(fedora, `"name":"fedora","version":"40"`, `"name":"coreos","version":"39"`, 1) + `,` + rpmOf(coreosRPM, "bash", "5.2.15") + `]}`, `"unscanned":["` + coreosRPM + `"]}`},
		{"a Go module without its version", listed([2]string{"pkg:golang/github.com/x/y", "pkg:golang/github.com/x/y"}),
			`{"bomFormat":"CycloneDX","components":[{"bom-ref":"pkg:golang/github.com/x/y","type":"library","name":"github.com/x/y","version":"","purl":"pkg:golang/github.com/x/y"}]}`, `"unscanned":["pkg:golang/github.com/x/y"]}`},
		{"gpg-pubkey on a distribution trivy scans, beside an rpm it scans", listed([2]string{gpg, gpg}, [2]string{rhBash, rhBash}),
			`{"bomFormat":"CycloneDX","components":[` + redhat + `,` + rpmOf(gpg, "gpg-pubkey", "fd431d51-4ae0493b") + `,` + rpmOf(rhBash, "bash", "5.1.8-9.el9") + `]}`, `"unscanned":["` + gpg + `"]}`},
		{"an AlmaLinux modular package without its modularitylabel", listed([2]string{modular, modular}),
			`{"bomFormat":"CycloneDX","components":[` + alma + `,` + rpmOf(modular, "nodejs", "18.19.0-1.module_el9.3.0+38+cc8ed8ad") + `]}`, `"unscanned":["` + modular + `"]}`},
		{"a jar an embedded SBOM named, alone", listed([2]string{"log4j-core", "pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1"}),
			`{"bomFormat":"CycloneDX","components":[` + jar + `]}`, `"report":{"SchemaVersion":2,"Results":[{"Packages":[{"Identifier":{"BOMRef":"log4j-core","PURL":"pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1"}}]}]}}`},
	} {
		must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+tc.report+"'\n"), 0o600))
		if status, body := postParts(t, url, sbomReq, [2]string{"sbom", tc.part}); status != 200 || !strings.HasSuffix(strings.TrimSpace(body), tc.want) {
			t.Errorf("%s: %d %.400s, want …%s", tc.name, status, body, tc.want)
		}
	}
}

// A token is cut off at its bound while it is read: a part holding one string of 5 MiB is refused with
// under 1 MiB allocated, the string never built; and what is written again is never more than twice the
// part — a string of < and & is written as it was read, never HTML-escaped six times over.
func TestScanSBOMBoundsATokenBeforeItIsBuilt(t *testing.T) {
	long := []byte(`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"` + strings.Repeat("x", 5<<20) + `"}]}`)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := checkPart(long)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; err == nil || !strings.Contains(err.Error(), "of more than 65536 bytes") || allocated > 1<<20 {
		t.Errorf("a string of 5 MiB: %v, %d bytes allocated", err, allocated)
	}
	escapes := []byte(`{"bomFormat":"CycloneDX","components":[{"bom-ref":"` + osRef + `","type":"operating-system","name":"ubuntu","version":"` + strings.Repeat("<&>", 20_000) + `"}]}`)
	written, err := canonical(escapes)
	if err != nil || len(written) > 2*len(escapes) || !bytes.Contains(written, []byte(strings.Repeat("<&>", 20_000))) {
		t.Errorf("a part of 60,000 escapable bytes written again as %d bytes (of %d): %v", len(written), len(escapes), err)
	}
}

// runID is the run image's ID on the fake daemon: what its image inspect --format {{.Id}} answers once built.
var runID = "sha256:" + strings.Repeat("cd", 32)

// The run image is read by the same target with no app (contracts/executor.md, The run image): exactly the
// run_image GET /builder answers, saved by its ID once the Java database is there, and answered as an app's
// image is, its diff IDs in order, matched against nothing.
func TestScanRunImageReadsTheRunImageGetBuilderNames(t *testing.T) {
	url, e, dir := imageFake(t, fresh, 1<<30, cdxOut)
	e.RunImage = "registry.example/golden/run@sha256:" + strings.Repeat("9", 64)
	must(t, os.WriteFile(filepath.Join(dir, "built"), nil, 0o600)) // the daemon holds it: its ID is runID
	inspect(t, dir, 1<<28, "", []string{layerID})
	status, body := postParts(t, url, `{"tool":"trivy","target":"image","image":"`+runID+`"}`)
	var res ScanResult
	if err := json.Unmarshal([]byte(body), &res); status != 200 || err != nil || string(res.SBOM) != cdxOut || res.Lifecycle != "" || !slices.Equal(res.DiffIDs, []string{layerID}) || res.DBBuiltAt != "" {
		t.Fatalf("%d %s", status, body)
	}
	if argv := readIf(dir, "argv"); !strings.Contains(argv, "/image.tar\n"+runID+"\n") || strings.Contains(argv, "image\nls\n") {
		t.Errorf("the run image is saved by its ID, and no app's images are listed:\n%s", argv)
	}
	validates(t, schemaOf(t, "scan_image_result"), []byte(body))
}

// Any image but the run image GET /builder answers now — an older run image the daemon still holds, or an
// app's image named without its app — is 409 run_image_moved, and nothing is saved or run.
func TestScanRunImageRefusesAnotherImage(t *testing.T) {
	url, e, dir := imageFake(t, fresh, 1<<30, cdxOut)
	e.RunImage = "registry.example/golden/run@sha256:" + strings.Repeat("9", 64)
	must(t, os.WriteFile(filepath.Join(dir, "built"), nil, 0o600))
	for _, image := range []string{"sha256:" + strings.Repeat("4d", 32), "canyon/claims-portal@" + appID} {
		status, body := postParts(t, url, `{"tool":"trivy","target":"image","image":"`+image+`"}`)
		if status != 409 || !strings.Contains(body, `"error":"run_image_moved"`) || !strings.Contains(body, "the run image is now "+runID) {
			t.Errorf("%s: %d %s", image, status, body)
			continue
		}
		validates(t, schemaOf(t, "scan_refused"), []byte(body))
	}
	if argv := readIf(dir, "argv"); strings.Contains(argv, "save\n") || strings.Contains(argv, "create\n") {
		t.Errorf("another image was saved or ran:\n%s", argv)
	}
}
