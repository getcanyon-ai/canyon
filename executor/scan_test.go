package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// pinTool stands a tool in for opengrep and checkov for the length of a test.
func pinTool(t *testing.T, image string, args []string, deadline time.Duration) {
	old := scanTools
	tool := scanTool{image: image, args: args, deadline: deadline, config: func(s ScanSettings) []byte { b, _ := json.Marshal(s); return b }}
	opengrep := tool
	opengrep.exits = old["opengrep"].exits // the exit statuses that are a scan are the tool's own, pinned or not
	scanTools = map[string]scanTool{"opengrep": opengrep, "checkov": tool}
	t.Cleanup(func() { scanTools = old })
}

// scanDocker's docker records argv to <dir>/argv; cp lists what it is handed to <dir>/cp and keeps
// its <tool>.yaml as <dir>/config, and cp -a keeps what it reads as <dir>/owned.tar; build keeps its
// Dockerfile as <dir>/dockerfile and prints an image ID, or fails while <dir>/nobuild exists; image
// inspect fails while <dir>/pruned exists; start runs
// <dir>/start; run --rm, a database's reader, prints <dir>/meta, exiting 1 when there is none or as
// <dir>/reader says when it exists; any other run, a refresh, notes in <dir>/overlap each that starts while
// another runs, then fails while <dir>/norefresh exists, else makes <dir>/fetch the meta if it exists;
// volume ls of the canyon-scandb- names prints <dir>/volumes, which a scan made without a label; image ls
// prints <dir>/images, image inspect --format {{json .}} <dir>/inspect, and save writes <dir>/saved, the image's
// tar, its own user's alone, as docker does; cp keeps the mode of the unpacked image's index.json in <dir>/mode,
// and tmp/'s in <dir>/tmpmode; cp keeps an sbom target's part as <dir>/sbom; inspect of a container prints
// <dir>/killed, its OOMKilled and exit code.
func scanDocker(t *testing.T, start string) (*Executor, string) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
d="` + dir + `"
printf '%s\n' "$@" >> "$d/argv"
` + floorFake + `case "$*" in
cp\ -a\ -\ *) cat > "$d/owned.tar";;
cp\ *) (cd "$2" && find . | sort) >> "$d/cp"; cat "$2"/*.yaml > "$d/config" 2>/dev/null; cp "$2/sbom.cdx.json" "$d/sbom" 2>/dev/null
  [ -f "$2/image/index.json" ] && ls -l "$2/image/index.json" | cut -c1-10 > "$d/mode" && ls -ld "$2/tmp" | cut -c1-10 > "$d/tmpmode";;
image\ ls\ *) cat "$d/images" 2>/dev/null;;
"image inspect --format {{json .}} "*) cat "$d/inspect";;
inspect\ *) cat "$d/killed" 2>/dev/null;;
save\ *) cp "$d/saved" "$3"; chmod 600 "$3";;
image\ inspect\ *) { [ -f "$d/pruned" ] || [ ! -f "$d/built" ]; } && exit 1; echo sha256:` + strings.Repeat("cd", 32) + `;;
build\ *) cat > "$d/dockerfile"; [ -f "$d/nobuild" ] && exit 1; : > "$d/built"; echo sha256:` + strings.Repeat("cd", 32) + `;;
start\ *) . "$d/start";;
run\ --rm\ *) [ -f "$d/reader" ] && exit "$(cat "$d/reader")"; exec cat "$d/meta" 2>/dev/null;;
run\ *) mkdir "$d/running" 2>/dev/null || echo overlap >> "$d/overlap"; sleep 0.2; rmdir "$d/running"
  [ -f "$d/norefresh" ] && exit 1; [ -f "$d/fetch" ] && cp "$d/fetch" "$d/meta";;
volume\ ls\ --quiet\ --filter\ name=canyon-scandb-) cat "$d/volumes" 2>/dev/null;;
esac
exit 0
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	must(t, os.WriteFile(filepath.Join(dir, "start"), []byte(start), 0o600))
	return e, dir
}

// scanFake serves scanDocker with a busybox standing in for opengrep and checkov, scanning until deadline.
func scanFake(t *testing.T, start string, deadline time.Duration) (*httptest.Server, string) {
	e, dir := scanDocker(t, start)
	pinTool(t, "busybox@sha256:"+strings.Repeat("ab", 32), []string{"scan", "/scan/tree"}, deadline)
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	return srv, dir
}

// dbFake serves scanDocker with the real tools and a database whose reader prints meta, none when meta
// is empty. Once the refreshes the test's scans started have ended, no tool's last refresh has failed.
func dbFake(t *testing.T, meta string) (*httptest.Server, *Executor, string) {
	e, dir := scanDocker(t, "printf '%s' '"+sarifOut+"'\n")
	t.Cleanup(func() {
		e.collecting.Wait()
		scanTools["trivy"].db.failed.Store(false)
		scanTools["grype"].db.failed.Store(false)
		scanTools[JavaDB].db.failed.Store(false)
	})
	if meta != "" {
		must(t, os.WriteFile(filepath.Join(dir, "meta"), []byte(meta), 0o600))
	}
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	return srv, e, dir
}

func builtAgo(d time.Duration) string {
	return `{"Version":2,"UpdatedAt":"` + time.Now().Add(-d).UTC().Format(time.RFC3339Nano) + `"}`
}

// readIf is read for a file that may not exist: "" then.
func readIf(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test dir
	return string(b)
}

// postScan posts the json part, then the tree, then the rules if given.
func postScan(t *testing.T, url, req string, parts ...[]byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(req)))
	for i, p := range parts {
		f, _ := mw.CreateFormFile([]string{"tree", "rules"}[i], "part.tar")
		must(t, write(f, p))
	}
	must(t, mw.Close())
	resp, err := http.Post(url+"/scan", mw.FormDataContentType(), &body) //nolint:gosec,noctx // the test server
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// headersTar is a tar of these entries in order, each file's body Size zero bytes.
func headersTar(t *testing.T, hs ...tar.Header) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hs {
		must(t, tw.WriteHeader(&h))
		must(t, write(tw, make([]byte, h.Size)))
	}
	must(t, tw.Close())
	return b.Bytes()
}

const (
	opengrepReq = `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"opengrep","settings":{"skip_paths":["docs"]}}`
	checkovReq  = `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"checkov","settings":{"skip_paths":["docs"]}}`
	trivyReq    = `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","settings":{"scanners":["vuln","secret"],"skip_paths":["docs",".github","a: b"]}}`
	sarifOut    = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[{"ruleId":"r","suppressions":[{"kind":"inSource"}]}]}]}`
)

func TestScanRunsThePinnedImageLockedDownAndAnswersItsSARIF(t *testing.T) {
	srv, dir := scanFake(t, "printf '%s' '"+sarifOut+"'\n", 2*time.Second)
	status, body := postScan(t, srv.URL, opengrepReq, tarOf(t, map[string]string{"src/a.ts": "x"}), tarOf(t, map[string]string{"r.yaml": "rules: []"}))
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	copied, _ := os.ReadFile(filepath.Join(dir, "cp")) //nolint:gosec // test dir
	if status != 200 || !strings.Contains(body, `"image":"busybox@sha256:`) || !strings.Contains(body, `"ruleId":"r"`) || strings.Contains(body, "suppressions") {
		t.Fatalf("the tool's SARIF, suppressions removed, and the image that ran: %d %s", status, body)
	}
	for _, want := range []string{"--network\nnone\n", "--read-only\n", "--user\n65534:65534\n", "--cap-drop\nALL\n", "--security-opt\nno-new-privileges\n", "--memory\n2g\n", "--pids-limit\n512\n", "--cpus\n2\n", "--tmpfs\n/tmp:mode=1777\n", "--label\ncanyon.scan\n", "--volume\n/scan\nbusybox@sha256:", "\nstart\n-a\ncanyon-scan-", "\nrm\n-f\n-v\ncanyon-scan-"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("docker argv lacks %q", want)
		}
	}
	for _, want := range []string{"./tree/src/a.ts\n", "./opengrep.yaml\n", "./rules/r.yaml\n"} {
		if !strings.Contains(string(copied), want) {
			t.Errorf("docker cp was not handed %q: %q", want, copied)
		}
	}
}

// dropped is Opengrep's SARIF for a run that dropped one rule and ran the rest; dropped and broken also
// note an error that is no rule's (the shape of an engine the OOM killer ended).
var (
	dropped = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"invocations":[{"executionSuccessful":true,"toolExecutionNotifications":[` +
		`{"descriptor":{"id":"Rule parse error"},"level":"error","message":{"text":"Rule parse error in rule org-bad:"}},` +
		`{"descriptor":{"id":"Syntax error"},"level":"warning","message":{"text":"a.ts"}}]}],"results":[]}]}`
	droppedAndBroken = strings.Replace(dropped, `{"descriptor":{"id":"Syntax error"},"level":"warning"`, `{"descriptor":{"id":"Fatal error"},"level":"error"`, 1)
)

// Opengrep that cannot load every rules file it was handed exits other than 0 or 1 and still writes
// SARIF, with no result: rules_error, never that SARIF as a clean scan. Its 0 and 1 are a scan, a
// container docker says was killed is killed first (an engine the OOM killer ended makes Opengrep
// itself exit 2), and another tool's exit status is not read (contracts/executor.md).
func TestAnOpengrepRulesFileItCannotLoadIsNotFinished(t *testing.T) {
	tree, rules := tarOf(t, map[string]string{"src/a.ts": "x"}), tarOf(t, map[string]string{"r.yaml": "rules: []"})
	for _, c := range []struct {
		req, out, exit, inspect, want string
	}{
		{opengrepReq, dropped, "2", "false 2", `"sarif":`},               // its 2 for rules it dropped while it ran the rest is a scan
		{opengrepReq, dropped, "2", "true 2", `"not_finished":"killed"`}, // unless the OOM killer ended it
		{opengrepReq, droppedAndBroken, "2", "false 2", `"not_finished":"rules_error"`},
		{opengrepReq, dropped, "7", "false 7", `"not_finished":"rules_error"`},
		{opengrepReq, "", "7", "false 7", `"not_finished":"rules_error"`},
		{opengrepReq, "", "2", "false 2", `"not_finished":"rules_error"`},
		{opengrepReq, "", "2", "true 2", `"not_finished":"killed"`},
		{opengrepReq, "", "137", "false 137", `"not_finished":"killed"`},
		{opengrepReq, "", "1", "false 1", `"sarif":`},
		{checkovReq, "", "1", "false 1", `"sarif":`},
	} {
		srv, _ := scanFakeKilled(t, "printf '%s' '"+cmp.Or(c.out, sarifOut)+"'; exit "+c.exit, c.inspect)
		parts := [][]byte{tree}
		if c.req == opengrepReq {
			parts = append(parts, rules)
		}
		if status, body := postScan(t, srv.URL, c.req, parts...); status != 200 || !strings.Contains(body, c.want) || c.want != `"sarif":` && strings.Contains(body, `"sarif":`) {
			t.Errorf("%s exiting %s (%s): %d %s", c.req[strings.Index(c.req, `"tool"`):], c.exit, c.inspect, status, body)
		}
	}
}

// overCapSARIF prints one run of SARIF whose one result's message alone passes what Canyon keeps: a
// snippet it drops never counts (contracts/executor.md, /scan).
const overCapSARIF = `printf '{"version":"2.1.0","runs":[{"results":[{"message":{"text":"'; head -c 10485761 /dev/zero | tr '\0' x; printf '"}}]}]}'`

func TestScanAnswersNotFinishedAndAlwaysRemovesTheContainer(t *testing.T) {
	tree := tarOf(t, map[string]string{"a": "x"})
	for _, tc := range []struct {
		name, req, start, want string
		deadline               time.Duration
	}{
		{"no_image", strings.Replace(checkovReq, "checkov", "grype", 1), "", `{"tool":"grype","not_finished":"no_image"}`, 2 * time.Second},
		{"not_sarif", checkovReq, "echo hello", `"not_finished":"not_sarif"`, 2 * time.Second},
		{"not one run", checkovReq, `echo '{"version":"2.1.0","runs":[{},{}]}'`, `"not_finished":"not_sarif"`, 2 * time.Second},
		{"over_cap", checkovReq, overCapSARIF, `"not_finished":"over_cap"`, time.Minute}, // 10 MiB piped and decoded under -race: the cap trips however slow the CPU, never the deadline
		{"timeout", checkovReq, "exec sleep 10", `"not_finished":"timeout"`, 2 * time.Second},
	} {
		srv, dir := scanFake(t, tc.start, tc.deadline)
		status, body := postScan(t, srv.URL, tc.req, tree)
		argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
		if status != 200 || !strings.Contains(body, tc.want) || strings.Contains(body, `"sarif":`) {
			t.Errorf("%s: %d %s", tc.name, status, body)
		}
		if removed := strings.Contains(string(argv), "rm\n-f\n-v\ncanyon-scan-"); removed != (tc.name != "no_image") {
			t.Errorf("%s: container removed %v; argv %q", tc.name, removed, argv)
		}
	}
}

func TestScanRefusesAnythingButTheToolsAllowListAndPlainFilesAndDirectories(t *testing.T) {
	tree := tarOf(t, map[string]string{"a": "x"})
	linked := func(typ byte) []byte {
		return headersTar(t, tar.Header{Name: "src/link", Typeflag: typ, Linkname: "/etc/passwd"})
	}
	with := func(from, to string) string { return strings.Replace(opengrepReq, from, to, 1) }
	// a rules tar one entry past the cap, which falls on the block boundary between its entries
	overCap := headersTar(t, tar.Header{Name: "a.yaml", Typeflag: tar.TypeReg, Size: maxRules - 512}, tar.Header{Name: "b.yaml", Typeflag: tar.TypeReg, Size: 1})
	ids := make([]string, 257)
	for i := range ids {
		ids[i] = fmt.Sprintf("CKV_AWS_%d", i)
	}
	checks, _ := json.Marshal(ids)
	for name, tc := range map[string]struct {
		req   string
		parts [][]byte
	}{
		"a symlink":               {opengrepReq, [][]byte{linked(tar.TypeSymlink)}},
		"a hard link":             {opengrepReq, [][]byte{linked(tar.TypeLink)}},
		"a fifo":                  {opengrepReq, [][]byte{linked(tar.TypeFifo)}},
		"an escape":               {opengrepReq, [][]byte{tarOf(t, map[string]string{"../opengrep.yaml": "x"})}},
		"a symlink in the rules":  {opengrepReq, [][]byte{tree, linked(tar.TypeSymlink)}},
		"rules for another tool":  {checkovReq, [][]byte{tree, tree}},
		"opengrep without rules":  {opengrepReq, [][]byte{tree}},
		"rules past the cap":      {opengrepReq, [][]byte{tree, overCap}},
		"a file then a dir in it": {checkovReq, [][]byte{tarOf(t, map[string]string{"a": "x", "a/b": "y"})}},
		"a name the disk refuses": {checkovReq, [][]byte{tarOf(t, map[string]string{strings.Repeat("n", 300): "x"})}},
		"no tree":                 {opengrepReq, nil},
		"an unknown key":          {with(`"tool"`, `"image":"x","tool"`), [][]byte{tree}},
		"an unknown setting":      {with(`"skip_paths"`, `"rules":["x"],"skip_paths"`), [][]byte{tree}},
		"another tool's setting":  {with(`"skip_paths"`, `"scanners":["vuln"],"skip_paths"`), [][]byte{tree}},
		"no settings":             {with(`,"settings":{"skip_paths":["docs"]}`, ""), [][]byte{tree}},
		"an unknown tool":         {with(`"opengrep"`, `"gitleaks"`), [][]byte{tree}},
		"no scanners":             {trivySettings(`{"scanners":[]}`), [][]byte{tree}},
		"another scanner":         {trivySettings(`{"scanners":["license"]}`), [][]byte{tree}},
		"a scanner twice":         {trivySettings(`{"scanners":["vuln","vuln"]}`), [][]byte{tree}},
		"a skip path twice":       {trivySettings(`{"skip_paths":["docs","docs"]}`), [][]byte{tree}},
		"65 skip paths":           {trivySettings(`{"skip_paths":["p` + strings.Repeat(`","p`, 64) + `"]}`), [][]byte{tree}},
		"a skip path of 257":      {trivySettings(`{"skip_paths":["` + strings.Repeat("é", 257) + `"]}`), [][]byte{tree}},
		"no frameworks":           {checkovSettings(`{"frameworks":[]}`), [][]byte{tree}},
		"a framework not allowed": {checkovSettings(`{"frameworks":["terraform","kubernetes"]}`), [][]byte{tree}},
		"a framework twice":       {checkovSettings(`{"frameworks":["terraform","terraform"]}`), [][]byte{tree}},
		"a BC check id":           {checkovSettings(`{"skip_checks":["BC_AWS_GENERAL_1"]}`), [][]byte{tree}},
		"a severity":              {checkovSettings(`{"skip_checks":["LOW"]}`), [][]byte{tree}},
		"a wildcard":              {checkovSettings(`{"skip_checks":["CKV_AWS*"]}`), [][]byte{tree}},
		"a check twice":           {checkovSettings(`{"skip_checks":["CKV_AWS_1","CKV_AWS_1"]}`), [][]byte{tree}},
		"257 checks":              {checkovSettings(`{"skip_checks":` + string(checks) + `}`), [][]byte{tree}},
		"a check of 65":           {checkovSettings(`{"skip_checks":["CKV_` + strings.Repeat("A", 61) + `"]}`), [][]byte{tree}},
	} {
		srv, dir := scanFake(t, "printf '%s' '"+sarifOut+"'\n", 2*time.Second)
		status, body := postScan(t, srv.URL, tc.req, tc.parts...)
		argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
		if status != 400 || !strings.HasPrefix(body, `{"error":"invalid_request","message":`) || strings.Contains(string(argv), "create") {
			t.Errorf("%s: refused before any container exists: %d %s; argv %q", name, status, body, argv)
		}
	}
	for _, p := range []string{"", "/etc", "../x", "a/../b", "./a", "a/./b", "a//b", "a/", "-x", `a\\b`, `a\u0001b`} {
		srv, dir := scanFake(t, "printf '%s' '"+sarifOut+"'\n", 2*time.Second)
		if status, body := postScan(t, srv.URL, trivySettings(`{"skip_paths":["`+p+`"]}`), tree); status != 400 || strings.Contains(readIf(dir, "argv"), "create") {
			t.Errorf("skip path %q: refused before any container exists: %d %s", p, status, body)
		}
	}
}

func trivySettings(settings string) string {
	return `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","settings":` + settings + `}`
}

// A suppressed result comes back without its suppressions and without the level the tool gave it for
// the suppression (checkov: warning, the rule's default error); any other result keeps its level.
func TestSarifOfDropsASuppressedResultsLevel(t *testing.T) {
	out := `{"version":"2.1.0","runs":[{"results":[{"ruleId":"skipped","level":"warning","suppressions":[{"kind":"inSource"}]},` +
		`{"ruleId":"failed","level":"error"},{"ruleId":"none suppressed","level":"note","suppressions":[]}]}]}`
	got, ok := sarifOf([]byte(out), nil)
	if want := `{"runs":[{"results":[{"ruleId":"skipped"},{"level":"error","ruleId":"failed"},{"level":"note","ruleId":"none suppressed"}]}],"version":"2.1.0"}`; !ok || string(got) != want {
		t.Errorf("sarifOf:\n got %s\nwant %s", got, want)
	}
}

// ESLint names a file it could not parse in an ESL0999 notification of its invocation, by the file URL
// it writes every uri as: that uri comes back as the package's path, as a result's does, and nothing
// else in the notification changes (contracts/executor.md, the scan row).
func TestSarifOfAnswersANotificationsURIAsThePackagesPath(t *testing.T) {
	loc := func(uri string) string {
		return `"locations":[{"physicalLocation":{"artifactLocation":{"uri":"` + uri + `"},"region":{"startLine":2}}}]`
	}
	note := func(uri string) string {
		return `{"descriptor":{"id":"ESL0999"},"level":"error",` + loc(uri) + `,"message":{"text":"Parsing error: '}' expected."}}`
	}
	out := `{"version":"2.1.0","runs":[{"invocations":[{"executionSuccessful":false,"toolConfigurationNotifications":[` + note("file:///scan/tree/src/broken.ts") + `,` + note("file:///elsewhere/x.ts") +
		`],"toolExecutionNotifications":[` + note("file:///scan/tree/src/b.tsx") + `]}],"results":[{"ruleId":"no-eval",` + loc("file:///scan/tree/src/flagged.js") + `}]}]}`
	got, ok := sarifOf([]byte(out), scanTools["eslint"].uriRoots)
	want := `{"runs":[{"invocations":[{"executionSuccessful":false,"toolConfigurationNotifications":[` + note("src/broken.ts") + `,` + note("file:///elsewhere/x.ts") +
		`],"toolExecutionNotifications":[` + note("src/b.tsx") + `]}],"results":[{` + loc("src/flagged.js") + `,"ruleId":"no-eval"}]}],"version":"2.1.0"}`
	if !ok || string(got) != want {
		t.Errorf("sarifOf:\n got %s\nwant %s", got, want)
	}
}

func checkovSettings(settings string) string {
	return `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"checkov","settings":` + settings + `}`
}

// Invariant 9 for checkov: the executor's config by explicit path and the tree as one --directory=
// argument (a -d alone reads the tree's .checkov.yaml), the SARIF alone on stdout, suppressions
// removed, and uris under the tree — relative or absolute, prefixed once — made relative to it.
func TestScanRunsCheckovOnTheExecutorsConfigAndAnswersTreePaths(t *testing.T) {
	loc := func(uri string) string {
		return `"locations":[{"physicalLocation":{"artifactLocation":{"uri":"` + uri + `"},"region":{"startLine":2}}}]`
	}
	out := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Checkov"}},"results":[{"ruleId":"CKV_AWS_24",` + loc("scan/tree/main.tf") +
		`,"suppressions":[{"kind":"inSource"}]},{"ruleId":"CKV_DOCKER_2",` + loc("/scan/tree/scan/tree/Dockerfile") + `},{"ruleId":"CKV_AWS_18",` + loc("other/t.yaml") + `},{"ruleId":"CKV_AWS_1"}]}]}`
	e, dir := scanDocker(t, "printf '%s' '"+out+"'\n")
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	status, body := postScan(t, srv.URL, checkovSettings(`{"frameworks":["terraform","dockerfile"],"skip_checks":["CKV_AWS_20","CKV2_AWS_5"],"skip_paths":["docs","a.b/c+d"]}`),
		tarOf(t, map[string]string{"main.tf": "x"}))
	if want := `{"tool":"checkov","image":"` + checkovImage + `","sarif":`; status != 200 || !strings.HasPrefix(body, want) {
		t.Fatalf("want %s…, got %d %s", want, status, body)
	}
	for _, want := range []string{loc("main.tf"), loc("scan/tree/Dockerfile"), loc("other/t.yaml"), `{"ruleId":"CKV_AWS_1"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "suppressions") {
		t.Errorf("a suppression came back: %s", body)
	}
	argv := readIf(dir, "argv")
	if want := "--volume\n/scan\n" + checkovImage + "\n--directory=/scan/tree\n--config-file\n/scan/checkov.yaml\n--output\nsarif\n--output-file-path\n/dev/stdout,\n--skip-download\n"; !strings.Contains(argv, want) ||
		strings.Contains(argv, "\n-d\n") || strings.Contains(argv, "\n-w\n") || strings.Contains(argv, "--workdir") {
		t.Errorf("docker argv lacks %q, or names the tree's directory apart or as the working directory:\n%s", want, argv)
	}
	var config map[string][]string
	if err := yaml.Unmarshal([]byte(readIf(dir, "config")), &config); err != nil || strings.Join(config["framework"], ",") != "terraform,dockerfile" ||
		strings.Join(config["skip-check"], ",") != "CKV_AWS_20,CKV2_AWS_5" || strings.Join(config["skip-path"], " ") != `^/scan/tree/docs\Z ^/scan/tree/a\.b/c\+d\Z` {
		t.Errorf("checkov.yaml: %v %v", config, err)
	}
	var unset map[string][]string
	must(t, yaml.Unmarshal(checkovConfig(ScanSettings{}), &unset))
	if strings.Join(unset["framework"], ",") != "terraform,cloudformation,dockerfile" || len(unset) != 1 {
		t.Errorf("checkov.yaml with no settings: %v", unset)
	}
}

// A database 5 days old makes every review "could not check", and nothing said so before (#2743): GET /scandb
// answers how old each one is, from the database itself as the executor last read it — after a refresh, failed or
// not, and at a scan's check — never from when a refresh ended; none for one it has not read, and a read that
// fails keeps the one before.
func TestScannerDataAge(t *testing.T) {
	for _, name := range []string{"trivy", "grype", JavaDB} {
		scanTools[name].db.read.Store(nil)
		t.Cleanup(func() { scanTools[name].db.read.Store(nil) })
	}
	built := time.Now().Add(-4 * 24 * time.Hour).UTC().Truncate(time.Second)
	srv, e, dir := dbFake(t, `{"Version":2,"UpdatedAt":"`+built.Format(time.RFC3339Nano)+`"}`)
	ages := func() string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/scandb") //nolint:noctx // a test's own server
		must(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		must(t, err)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /scandb: %d %s", resp.StatusCode, b)
		}
		validates(t, schemaOf(t, "scandb_result"), b)
		return string(b)
	}
	none := `{"databases":[{"tool":"grype"},{"tool":"trivy"},{"tool":"trivy-java"}]}` + "\n"
	if got := ages(); got != none {
		t.Fatalf("before any read: %s", got)
	}
	read := `{"databases":[{"tool":"grype"},{"tool":"trivy","built_at":"` + built.Format(time.RFC3339) + `","stale_at":"` + built.Add(5*24*time.Hour).Format(time.RFC3339) + `"},{"tool":"trivy-java"}]}` + "\n"
	must(t, os.WriteFile(filepath.Join(dir, "norefresh"), nil, 0o600))
	if err := e.refreshDB(t.Context(), "trivy", scanTools["trivy"]); err == nil {
		t.Fatal("the refresh was meant to fail")
	}
	if got := ages(); got != read {
		t.Fatalf("after a failed refresh, the database as it is:\n%s\nwant\n%s", got, read)
	}
	must(t, os.WriteFile(filepath.Join(dir, "reader"), []byte("125"), 0o600)) // docker itself failing
	_ = e.refreshDB(t.Context(), "trivy", scanTools["trivy"])
	if got := ages(); got != read {
		t.Fatalf("a read that failed kept the one before:\n%s", got)
	}
}

// Invariants 7 and 9 for trivy: no network, the executor's config and an empty ignore file by
// explicit path, the database volume mounted read-only, and when it was built on the answer.
func TestScanRunsTrivyOfflineOnTheExecutorsConfigAndItsDatabaseReadOnly(t *testing.T) {
	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	srv, _, dir := dbFake(t, `{"Version":2,"UpdatedAt":"`+built.Format(time.RFC3339Nano)+`"}`)
	tree := tarOf(t, map[string]string{"package-lock.json": "{}", ".trivyignore": "CVE-2021-23337", "trivy.yaml": "scan: {scanners: [secret]}"})
	status, body := postScan(t, srv.URL, trivyReq, tree)
	if want := `{"tool":"trivy","image":"` + trivyImage + `","db_built_at":"` + built.Format(time.RFC3339) + `","sarif":`; status != 200 || !strings.HasPrefix(body, want) {
		t.Fatalf("want %s…, got %d %s", want, status, body)
	}
	argv := readIf(dir, "argv")
	for _, want := range []string{
		"--volume\n/scan\n--volume\ncanyon-scandb-trivy:/scan/db:ro\n" + trivyImage + "\nfs\n--config\n/scan/trivy.yaml\n--ignorefile\n/dev/null\n--misconfig-scanners\nkubernetes,helm,azure-arm\n--cache-dir\n/scan/db\n--format\nsarif\n--skip-db-update\n--skip-java-db-update\n--skip-check-update\n--offline-scan\n",
		"run\n--rm\n--network\nnone\n--read-only\n--user\n65534:65534\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n--label\ncanyon.scan\n--volume\ncanyon-scandb-trivy:/db:ro\n--entrypoint\nhead\n" + trivyImage + "\n-c\n1048576\n/db/db/metadata.json\n", // the metadata's reader, its first MiB
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("docker argv lacks %q:\n%s", want, argv)
		}
	}
	if n := strings.Count(argv, "--network\n"); n != 2 || strings.Count(argv, "--network\nnone\n") != n || strings.Contains(argv, "\n-w\n") || strings.Contains(argv, "--workdir") {
		t.Errorf("a scan's containers join no network and run outside the tree:\n%s", argv)
	}
	var config map[string]map[string][]string
	if err := yaml.Unmarshal([]byte(readIf(dir, "config")), &config); err != nil || strings.Join(config["scan"]["scanners"], ",") != "vuln,secret" ||
		strings.Join(config["scan"]["skip-dirs"], ",") != "docs,.github,a: b" || strings.Join(config["scan"]["skip-files"], ",") != "docs,.github,a: b" {
		t.Errorf("trivy.yaml: %v %v", config, err)
	}
}

// Invariants 7 and 9 for grype: no network, the executor's config by explicit path and nothing else to
// steer it, the database volume mounted read-only, /tmp the scan's user's, when the database was built on
// the answer, and each uri from the tree's root without its leading /.
func TestScanRunsGrypeOfflineOnTheExecutorsConfigAndItsDatabaseReadOnly(t *testing.T) {
	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	loc := func(uri string) string {
		return `"locations":[{"physicalLocation":{"artifactLocation":{"uri":"` + uri + `"}}}]`
	}
	e, dir := scanDocker(t, "printf '%s' '"+`{"version":"2.1.0","runs":[{"results":[{"ruleId":"a",`+loc("/package-lock.json")+`},{"ruleId":"b",`+loc("lib/x.jar")+`}]}]}`+"'\n")
	must(t, os.WriteFile(filepath.Join(dir, "meta"), []byte(`{"schemaVersion":"v6.1.9","built":"`+built.Format(time.RFC3339)+`","valid":true}`), 0o600))
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"grype","settings":{"skip_paths":["docs","a*b/[c]{d}?"]}}`, tarOf(t, map[string]string{".grype.yaml": "ignore: [{package: {name: lodash}}]"}))
	if want := `{"tool":"grype","image":"` + grypeImage + `","db_built_at":"` + built.Format(time.RFC3339) + `","sarif":`; status != 200 || !strings.HasPrefix(body, want) ||
		!strings.Contains(body, loc("package-lock.json")) || !strings.Contains(body, loc("lib/x.jar")) {
		t.Fatalf("want %s…, uris package-lock.json and lib/x.jar; got %d %s", want, status, body)
	}
	argv := readIf(dir, "argv")
	for _, want := range []string{
		"--tmpfs\n/tmp:mode=1777\n--label\ncanyon.scan\n--volume\n/scan\n--volume\ncanyon-scandb-grype:/scan/db:ro\n" + grypeImage + "\ndir:/scan/tree\n-c\n/scan/grype.yaml\n-o\nsarif\n",
		"run\n--rm\n--network\nnone\n--read-only\n--user\n65534:65534\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n--label\ncanyon.scan\n--volume\ncanyon-scandb-grype:/db:ro\n--env\nGRYPE_DB_CACHE_DIR=/db\n--env\nGRYPE_DB_VALIDATE_AGE=false\n" + grypeImage + "\ndb\nstatus\n-o\njson\n",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("docker argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Count(argv, "--env\n") != 2 || strings.Count(argv, "--network\nnone\n") != 2 || strings.Contains(argv, "\n-w\n") || strings.Contains(argv, "--workdir") {
		t.Errorf("only the reader names an env; both containers join no network; neither works in the tree:\n%s", argv)
	}
	var config struct {
		DB struct {
			CacheDir   string `yaml:"cache-dir"`
			AutoUpdate *bool  `yaml:"auto-update"`
		}
		Exclude []string
	}
	if err := yaml.Unmarshal([]byte(readIf(dir, "config")), &config); err != nil || config.DB.CacheDir != "/scan/db" || config.DB.AutoUpdate == nil || *config.DB.AutoUpdate ||
		strings.Join(config.Exclude, " ") != `./docs ./a\*b/\[c\]\{d\}\?` {
		t.Errorf("grype.yaml: %+v %v\n%s", config, err, readIf(dir, "config"))
	}
}

// A packages target lists what a tree's files name (contracts/executor.md, target packages): trivy's
// license scanner, offline, joining no network, reading no database and no config of the tree's, the tree
// as it came; its JSON report is the answer's packages, with no database date. Settings, an image, no
// package or another tool are refused before anything runs, and stdout that is no trivy report is no answer.
func TestScanPackagesListsTheTreesPackagesOfflineWithNoDatabase(t *testing.T) {
	const report = `{"SchemaVersion":2,"ArtifactType":"filesystem","Results":[{"Target":"yarn.lock","Class":"lang-pkgs","Type":"yarn",` +
		`"Packages":[{"Name":"express","Version":"4.21.2","Identifier":{"PURL":"pkg:npm/express@4.21.2"},"Relationship":"direct"}]}]}`
	e, dir := scanDocker(t, "printf '%s' '"+report+"'\n")
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	const req = `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","target":"packages"}`
	tree := tarOf(t, map[string]string{"yarn.lock": "", "docs/x.md": "", ".trivyignore": "CVE-2021-23337", "trivy.yaml": "scan: {scanners: [vuln]}"})
	status, body := postScan(t, srv.URL, req, tree)
	if want := `{"tool":"trivy","image":"` + trivyImage + `","packages":` + report + `}`; status != 200 || strings.TrimSpace(body) != want {
		t.Fatalf("want %s, got %d %s", want, status, body)
	}
	argv := readIf(dir, "argv")
	if want := "--network\nnone\n--read-only\n"; !strings.Contains(argv, want) {
		t.Errorf("docker argv lacks %q:\n%s", want, argv)
	}
	if want := trivyImage + "\nfs\n--format\njson\n--list-all-pkgs\n--scanners\nlicense\n--include-dev-deps\n--cache-dir\n/tmp/trivy\n--cache-backend\nmemory\n--ignorefile\n/dev/null\n" +
		"--skip-db-update\n--skip-java-db-update\n--offline-scan\n--disable-telemetry\n--skip-version-check\n--no-progress\n/scan/tree\n"; !strings.Contains(argv, want) {
		t.Errorf("docker argv lacks %q:\n%s", want, argv)
	}
	if strings.Contains(argv, "canyon-scandb") || strings.Contains(argv, "--config") || strings.Contains(argv, "--server") || readIf(dir, "config") != "" {
		t.Errorf("a packages scan read a database or a config:\n%s\nconfig: %q", argv, readIf(dir, "config"))
	}
	if cp := readIf(dir, "cp"); !strings.Contains(cp, "./tree/docs/x.md\n") {
		t.Errorf("the tree is not as it came: %s", cp)
	}
	for _, refused := range []string{
		`{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","target":"packages","settings":{}}`,
		`{"app":"claims-portal","package":"pkg_7d3e8f","tool":"trivy","target":"packages","image":"canyon/claims-portal@sha256:` + strings.Repeat("ab", 32) + `"}`,
		`{"app":"claims-portal","tool":"trivy","target":"packages"}`,
		`{"app":"claims-portal","package":"pkg_7d3e8f","tool":"grype","target":"packages"}`,
	} {
		before := readIf(dir, "argv")
		if status, body := postScan(t, srv.URL, refused, tree); status != 400 || !strings.HasPrefix(body, `{"error":"invalid_request"`) || readIf(dir, "argv") != before {
			t.Errorf("%s: %d %s", refused, status, body)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+sarifOut+"'\n"), 0o600))
	if status, body := postScan(t, srv.URL, req, tree); status != 200 || !strings.Contains(body, `"not_finished":"not_sarif"`) {
		t.Errorf("SARIF from a packages scan: %d %s", status, body)
	}
	// trivy leaves Results out when it lists nothing: a tree with no packages, never one it did not read
	const nothing = `{"SchemaVersion":2,"ArtifactName":"/scan/tree","ArtifactType":"filesystem"}`
	must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("printf '%s' '"+nothing+"'\n"), 0o600))
	if status, body := postScan(t, srv.URL, req, tree); status != 200 || !strings.Contains(body, `"packages":`+nothing) {
		t.Errorf("a report of nothing: %d %s", status, body)
	}
}

// Unset, trivy's scanners are vuln and misconfig, never secret: gitleaks is the one secrets detector
// (contracts/executor.md).
func TestTrivysUnsetScannersAreVulnAndMisconfig(t *testing.T) {
	var config map[string]map[string][]string
	if err := yaml.Unmarshal(trivyConfig(ScanSettings{}), &config); err != nil || strings.Join(config["scan"]["scanners"], ",") != "vuln,misconfig" {
		t.Fatalf("trivy.yaml with no settings: %v %v", config, err)
	}
}

// A skip path YAML would read as something else stays that string: the config is encoded, never formatted.
func TestTrivyConfigKeepsEverySkipPathAString(t *testing.T) {
	paths := []string{"null", "~", "true", "no", "1e3", "0x1F", "2026-09-25", ".inf", "a: b", "#c", "d #e", "[f]", "{g}", "&h", "*i", "!j", "%k", "@l", "'m", `"n`, "|o", ">p", "? q", "r, s"}
	var config map[string]map[string][]any
	must(t, yaml.Unmarshal(trivyConfig(ScanSettings{SkipPaths: paths}), &config))
	for _, key := range []string{"skip-dirs", "skip-files"} {
		got := config["scan"][key]
		for i, p := range paths {
			if i >= len(got) || got[i] != p {
				t.Errorf("%s: %q came back as %#v", key, p, got)
				break
			}
		}
	}
}

// No database the scan's user can read is busy while it is downloaded (below), one built over 5 days
// before the scan starts db_stale, and neither runs the tool.
func TestScanWithoutAUsableDatabaseRunsNoTool(t *testing.T) {
	grypeReq := strings.Replace(checkovReq, "checkov", "grype", 1)
	busy := `{"error":"busy","reason":"database"}`
	for _, tc := range []struct{ name, req, meta, want string }{
		{"no database", trivyReq, "", busy},
		{"metadata that does not parse", trivyReq, "{", busy},
		{"built over 5 days ago", trivyReq, builtAgo(dbMaxAge + time.Minute), `{"tool":"trivy","image":"` + trivyImage + `","not_finished":"db_stale"}`},
		{"built under 5 days ago", trivyReq, builtAgo(dbMaxAge - time.Minute), `","sarif":`},
		{"grype's, under another key", grypeReq, builtAgo(time.Hour), busy},
		{"grype's, built over 5 days ago", grypeReq, `{"built":"` + time.Now().Add(-dbMaxAge-time.Minute).UTC().Format(time.RFC3339) + `"}`, `{"tool":"grype","image":"` + grypeImage + `","not_finished":"db_stale"}`},
	} {
		srv, e, dir := dbFake(t, tc.meta)
		status, body := postScan(t, srv.URL, tc.req, tarOf(t, map[string]string{"a": "x"}))
		e.collecting.Wait() // the download a busy scan started
		argv := readIf(dir, "argv")
		if status != map[bool]int{true: 503, false: 200}[tc.want == busy] || !strings.Contains(body, tc.want) || strings.Contains(argv, "\nstart\n") != (tc.want == `","sarif":`) {
			t.Errorf("%s: %d %s; argv %q", tc.name, status, body, argv)
		}
		if !strings.Contains(argv, "rm\n-f\n-v\ncanyon-scan-") {
			t.Errorf("%s: the scan removed: %q", tc.name, argv)
		}
	}
}

// A reader docker itself fails (exit 125) is the executor's failure, never a database missing.
func TestScanAnswersBadGatewayWhenDockerFailsToReadTheDatabase(t *testing.T) {
	srv, _, dir := dbFake(t, builtAgo(time.Hour))
	must(t, os.WriteFile(filepath.Join(dir, "reader"), []byte("125"), 0o600))
	status, body := postScan(t, srv.URL, trivyReq, tarOf(t, map[string]string{"a": "x"}))
	if status != 502 || strings.Contains(body, "not_finished") || strings.Contains(readIf(dir, "argv"), "\nstart\n") {
		t.Errorf("%d %s", status, body)
	}
}

// A tool's first scan finds no database: it starts the tool's one refresh and answers busy, as a scan
// while that refresh runs does, and once the database exists a scan runs the tool on it.
func TestAToolsFirstScanDownloadsItsDatabaseThenScans(t *testing.T) {
	srv, e, dir := dbFake(t, "")
	must(t, os.WriteFile(filepath.Join(dir, "fetch"), []byte(builtAgo(time.Hour)), 0o600))
	tree := tarOf(t, map[string]string{"a": "x"})
	for range 2 {
		if status, body := postScan(t, srv.URL, trivyReq, tree); status != 503 || strings.TrimSpace(body) != `{"error":"busy","reason":"database"}` {
			t.Fatalf("a scan before the database exists: %d %s", status, body)
		}
	}
	e.collecting.Wait()
	if status, body := postScan(t, srv.URL, trivyReq, tree); status != 200 || !strings.Contains(body, `","sarif":`) {
		t.Fatalf("a scan once the database exists: %d %s", status, body)
	}
	if argv := readIf(dir, "argv"); strings.Count(argv, "\n--download-db-only\n") != 1 || strings.Count(argv, "\nstart\n") != 1 {
		t.Errorf("one refresh, then one scan ran the tool:\n%s", argv)
	}
}

// Scans that find no database at once — each holding the tool's lock to read — start one refresh.
func TestScansWithoutADatabaseStartOneRefresh(t *testing.T) {
	_, e, dir := dbFake(t, "")
	trivy := scanTools["trivy"]
	trivy.db.lock.RLock() // the scans in flight
	e.startRefresh(t.Context(), "trivy", trivy)
	e.startRefresh(t.Context(), "trivy", trivy)
	trivy.db.lock.RUnlock()
	if e.collecting.Wait(); strings.Count(readIf(dir, "argv"), "\n--download-db-only\n") != 1 {
		t.Errorf("%d refreshes", strings.Count(readIf(dir, "argv"), "\n--download-db-only\n"))
	}
}

// A refresh that fails is db_missing to the next scan, once; the scan after that downloads again.
func TestAFailedRefreshIsDBMissingOnceThenDownloadsAgain(t *testing.T) {
	srv, e, dir := dbFake(t, "")
	must(t, os.WriteFile(filepath.Join(dir, "norefresh"), nil, 0o600))
	var got []string
	for range 3 {
		status, body := postScan(t, srv.URL, trivyReq, tarOf(t, map[string]string{"a": "x"}))
		got = append(got, fmt.Sprint(status, " ", strings.TrimSpace(body)))
		e.collecting.Wait()
	}
	want := []string{`503 {"error":"busy","reason":"database"}`, `200 {"tool":"trivy","image":"` + trivyImage + `","not_finished":"db_missing"}`, `503 {"error":"busy","reason":"database"}`}
	if argv := readIf(dir, "argv"); !slices.Equal(got, want) || strings.Count(argv, "\n--download-db-only\n") != 2 || strings.Contains(argv, "\nstart\n") {
		t.Errorf("got %q, want %q; argv:\n%s", got, want, argv)
	}
}

// Only a tool whose volume exists — one a scan asked for — is refreshed, when the executor starts and
// every dbEvery: with none, nothing is downloaded.
func TestRefreshDBsRefreshesOnlyAToolWithAVolume(t *testing.T) {
	for _, volumes := range []string{"", "canyon-scandb-trivy\n"} {
		_, e, dir := dbFake(t, "")
		must(t, os.WriteFile(filepath.Join(dir, "volumes"), []byte(volumes), 0o600))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { e.RefreshDBs(ctx); close(done) }()
		for i := 0; i < 100 && !strings.Contains(readIf(dir, "argv"), "volume\nls\n--quiet\n--filter\nname=canyon-scandb-\n"); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(time.Second) // a refresh takes 0.2 s here
		cancel()
		<-done
		argv := readIf(dir, "argv")
		if trivy := strings.Count(argv, "\n--download-db-only\n"); trivy != strings.Count(volumes, "trivy") || strings.Contains(argv, "canyon-scandb-grype") {
			t.Errorf("volumes %q: %d trivy refreshes, grype's volume named %v", volumes, trivy, strings.Contains(argv, "canyon-scandb-grype"))
		}
	}
}

// The refresher runs at start for each tool with a volume, with network, into the volumes the sweep
// keeps, and removes its containers; grype's runs as the scan's user, the volume's root made that user's first.
func TestRefreshDBsDownloadsAtStartWithNetworkIntoTheKeptVolume(t *testing.T) {
	_, e, dir := dbFake(t, "")
	must(t, os.WriteFile(filepath.Join(dir, "volumes"), []byte("canyon-scandb-trivy\ncanyon-scandb-grype\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.RefreshDBs(ctx); close(done) }()
	for i := 0; i < 100 && strings.Count(readIf(dir, "argv"), "rm\n-f\n-v\ncanyon-scandb-refresh-") < 2; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	argv := readIf(dir, "argv")
	for _, want := range []string{"volume\ncreate\n--label\ncanyon.scandb\ncanyon-scandb-trivy\n", "--memory\n2g\n--pids-limit\n512\n--cpus\n2\n--tmpfs\n/tmp:mode=1777\n",
		"--volume\ncanyon-scandb-trivy:/db\n" + trivyImage + "\nfs\n--cache-dir\n/db\n--download-db-only\n--disable-telemetry\n--skip-version-check\n--no-progress\nrm\n", "volume\ncreate\n--label\ncanyon.scandb\ncanyon-scandb-grype\n",
		"create\n--name\ncanyon-scandb-owner-", "--label\ncanyon.scan\n--volume\ncanyon-scandb-grype:/db\n" + grypeImage + "\ncp\n-a\n-\ncanyon-scandb-owner-",
		"--volume\ncanyon-scandb-grype:/db\n--user\n65534:65534\n--env\nGRYPE_DB_CACHE_DIR=/db\n" + grypeImage + "\ndb\nupdate\n", "rm\n-f\n-v\ncanyon-scandb-owner-"} {
		if !strings.Contains(argv, want) {
			t.Errorf("docker argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Count(argv, "rm\n-f\n-v\ncanyon-scandb-refresh-") != 2 ||
		strings.Count(argv, "--network") != strings.Count(argv, "run\n--name\ncanyon-scandb-sweep-")+strings.Count(argv, "run\n--rm\n--network\nnone\n") ||
		strings.Count(argv, "run\n--rm\n--network\nnone\n") != 2 || strings.Index(argv, "cp\n-a\n") > strings.Index(argv, "db\nupdate\n") {
		t.Errorf("each refresh fetches on docker's default network (grype's sweep and each refresh's reader, after it, alone run on none: #2743) and is removed; grype's volume is its user's first:\n%s", argv)
	}
	tr := tar.NewReader(strings.NewReader(readIf(dir, "owned.tar")))
	h, err := tr.Next()
	if _, end := tr.Next(); err != nil || h.Name != "db/" || h.Typeflag != tar.TypeDir || h.Uid != 65534 || h.Gid != 65534 || h.Mode != 0o755 || !errors.Is(end, io.EOF) {
		t.Errorf("the tar docker cp -a writes onto /db: one directory db/, 65534:65534 0755: %+v %v", h, err)
	}
}

// A grype refresh cut short leaves its download, grype-db-download<N>, in the volume beside the live database
// (#2504). Each refresh first sweeps what the last one left, from the executor's busybox with no network, as the
// refresh's user within the refresh's limits; so refreshes cut short again and again leave one partial at most, the live database stays,
// and one that finishes leaves none. The sweep's docker plays the volume as <dir>/db; grype's db update adds a
// download there, cut short while <dir>/cut exists, else moved into 6/.
func TestAGrypeRefreshSweepsWhatOneCutShortLeft(t *testing.T) {
	e, dir := fakes(t)
	db := filepath.Join(dir, "db")
	must(t, os.Mkdir(db, 0o700))
	docker := `#!/bin/sh
d="` + dir + `"
printf '%s\n' "$@" >> "$d/argv"
[ "$1" = run ] || exit 0
case "$*" in
*" db update")
  part="$d/db/grype-db-download$$"; mkdir "$part" && : > "$part/vulnerability.db"
  [ -f "$d/cut" ] && exit 137
  rm -rf "$d/db/6" && mkdir "$d/db/6" && mv "$part/vulnerability.db" "$d/db/6/" && rmdir "$part";;
*) script=; prev=; for a; do [ "$prev" = -c ] && script=$a; prev=$a; done
  [ -n "$script" ] && exec sh -c "$(printf '%s' "$script" | sed "s#/db/#$d/db/#g")";;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	grype := scanTools["grype"]
	partials := func() []string {
		m, _ := filepath.Glob(filepath.Join(db, "grype-db-download*"))
		return m
	}
	must(t, e.refreshDB(context.Background(), "grype", grype))
	must(t, os.WriteFile(filepath.Join(dir, "cut"), nil, 0o600))
	for range 3 {
		if err := e.refreshDB(context.Background(), "grype", grype); err == nil {
			t.Fatal("a refresh cut short answered no error")
		}
	}
	if p := partials(); len(p) != 1 {
		t.Errorf("three refreshes cut short left %d downloads in the volume, want the last one's alone: %v", len(p), p)
	}
	if _, err := os.Stat(filepath.Join(db, "6", "vulnerability.db")); err != nil {
		t.Errorf("the live database went: %v", err)
	}
	must(t, os.Remove(filepath.Join(dir, "cut")))
	must(t, e.refreshDB(context.Background(), "grype", grype))
	if p := partials(); len(p) != 0 {
		t.Errorf("a refresh that finished left %v", p)
	}
	argv := readIf(dir, "argv")
	sweep := "--network\nnone\n--read-only\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n--memory\n2g\n--pids-limit\n512\n--cpus\n2\n--label\ncanyon.scan\n--volume\ncanyon-scandb-grype:/db\n--user\n65534:65534\n" +
		busyboxImage + "\nsh\n-c\nrm -rf /db/grype-db-download*\n"
	if i, j := strings.LastIndex(argv, sweep), strings.LastIndex(argv, "db\nupdate\n"); i < 0 || j < i {
		t.Errorf("no sweep before the last db update:\n%s", argv)
	}
}

// One lock per tool: refreshes run one at a time, a refresh waits for a scan in flight, and a scan
// during a refresh answers busy at once, never waiting on it.
func TestRefreshAndScanOfOneToolNeverOverlap(t *testing.T) {
	srv, e, dir := dbFake(t, builtAgo(time.Hour))
	trivy := scanTools["trivy"]
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := e.refreshDB(context.Background(), "trivy", trivy); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlap := readIf(dir, "overlap"); overlap != "" {
		t.Errorf("two refreshes ran at once: %q", overlap)
	}
	trivy.db.lock.RLock() // a scan in flight
	refreshed := make(chan error, 1)
	go func() { refreshed <- e.refreshDB(context.Background(), "trivy", trivy) }()
	if time.Sleep(300 * time.Millisecond); len(refreshed) > 0 {
		t.Error("a refresh ran during a scan")
	}
	trivy.db.lock.RUnlock()
	must(t, <-refreshed)
	trivy.db.lock.Lock() // a refresh in flight
	answered := make(chan string, 1)
	go func() {
		status, body := postScan(t, srv.URL, trivyReq, tarOf(t, map[string]string{"a": "x"}))
		answered <- fmt.Sprint(status, " ", strings.TrimSpace(body))
	}()
	select {
	case got := <-answered:
		if got != `503 {"error":"busy","reason":"database"}` || strings.Contains(readIf(dir, "argv"), "canyon-scan-") {
			t.Errorf("a scan during a refresh: %s, argv %q", got, readIf(dir, "argv"))
		}
	case <-time.After(2 * time.Second):
		t.Error("a scan waited on a refresh instead of answering busy")
	}
	trivy.db.lock.Unlock()
}

// scanFakeKilled is scanFake plus a docker inspect that answers inspect for the OOM/exit-code
// classification: the "start" step prints partial output (never valid SARIF) and exits, and
// "docker inspect" answers state exactly as tc.inspect gives it.
func scanFakeKilled(t *testing.T, start, inspect string) (*httptest.Server, string) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
case "$1" in
cp) (cd "$2" && find . | sort) >> "` + dir + `/cp";;
start) . "` + dir + `/start";;
inspect) printf '%s' "` + inspect + `";;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	must(t, os.WriteFile(filepath.Join(dir, "start"), []byte("#!/bin/sh\n"+start+"\n"), 0o600))
	pinTool(t, "busybox@sha256:"+strings.Repeat("ab", 32), []string{"scan", "/scan/tree"}, 2*time.Second)
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	return srv, dir
}

func TestScanAnswersNotFinishedKilledOnOOMOrASignalNeverNotSarif(t *testing.T) {
	tree := tarOf(t, map[string]string{"a": "x"})
	for _, tc := range []struct{ name, inspect string }{
		{"oom", "true 137"},              // docker's own OOM signal
		{"signal, not oom", "false 137"}, // no OOM, but a signal death (128+9)
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := scanFakeKilled(t, "printf partial", tc.inspect)
			status, body := postScan(t, srv.URL, checkovReq, tree)
			if status != 200 || !strings.Contains(body, `"not_finished":"killed"`) || strings.Contains(body, `"sarif":`) {
				t.Fatalf("%s: %d %s", tc.name, status, body)
			}
		})
	}
}

// TestScanAnswersNotSarifNotKilledOnAnOrdinaryNonzeroExit: a tool that just fails on its own —
// exit code under 128, not OOM — must not be misread as killed.
func TestScanAnswersNotSarifNotKilledOnAnOrdinaryNonzeroExit(t *testing.T) {
	srv, _ := scanFakeKilled(t, "printf partial", "false 1")
	status, body := postScan(t, srv.URL, checkovReq, tarOf(t, map[string]string{"a": "x"}))
	if status != 200 || !strings.Contains(body, `"not_finished":"not_sarif"`) {
		t.Fatalf("%d %s", status, body)
	}
}

// /build and /scan each have their own limit (contracts/executor.md, Busy; #1652): a scan past the scans'
// is busy whatever the builds hold, and one under it is not turned away by builds in flight.
func TestScanHasItsOwnLimitAndAnswersBusy(t *testing.T) {
	e, _ := fakes(t)
	was, wasScans := builds, scans
	t.Cleanup(func() { builds, scans = was, wasScans }) // as the other tests found them
	must(t, Limit(func(string) string { return "" }, 1, 2))
	b, sc := builds, scans
	b <- struct{}{} // the one build in flight
	rec := httptest.NewRecorder()
	Handler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scan", nil))
	if rec.Code == 503 {
		t.Fatalf("a scan turned away by a build: %d %q", rec.Code, rec.Body)
	}
	for range cap(sc) {
		sc <- struct{}{}
	}
	rec = httptest.NewRecorder()
	Handler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/scan", nil))
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || strings.TrimSpace(rec.Body.String()) != `{"error":"busy"}` {
		t.Fatalf("a scan past the scans' limit: %d %q retry-after %q", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	for _, v := range []string{"0", "101", "x"} {
		if err := Limit(func(n string) string { return map[string]string{"CANYON_EXECUTOR_SCANS": v}[n] }, 1, 2); err == nil || !strings.Contains(err.Error(), "CANYON_EXECUTOR_SCANS") {
			t.Errorf("CANYON_EXECUTOR_SCANS=%s: %v", v, err)
		}
	}
}

// Opengrep publishes no image: a failed build answers no_image before any container exists, the next scan
// builds it, and each scan after runs that build's ID with opengrep's argv under ThenJSON, answered as
// canyon-scan/opengrep@<id>. Opengrep reads no config file: its skip paths leave the tree before the copy,
// and one that names nothing, or a path under a file, is no error.
func TestScanBuildsOpengrepsImageOnceAndRunsItByID(t *testing.T) {
	og, old := scanTools["opengrep"], scanTools
	og.build = &scanImage{dockerfile: og.build.dockerfile, repo: og.build.repo} // unbuilt, whatever another test left
	scanTools = map[string]scanTool{"opengrep": og}
	t.Cleanup(func() { scanTools = old })
	e, dir := scanDocker(t, "printf '%s\\0%s' '"+sarifOut+"' '{\"errors\":[]}'\n")
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	scan := func() (int, string) {
		req := `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"opengrep","settings":{"skip_paths":["docs","src/a.ts/under-a-file","gone"]}}`
		return postScan(t, srv.URL, req, tarOf(t, map[string]string{"src/a.ts": "x", "docs/b.ts": "y"}), tarOf(t, map[string]string{"r.yaml": "rules: []"}))
	}
	must(t, os.WriteFile(filepath.Join(dir, "nobuild"), nil, 0o600))
	if status, body := scan(); status != 200 || strings.TrimSpace(body) != `{"tool":"opengrep","not_finished":"no_image"}` || strings.Contains(readIf(dir, "argv"), "create") {
		t.Fatalf("a failed build, no image and no container: %d %s", status, body)
	}
	must(t, os.Remove(filepath.Join(dir, "nobuild")))
	id := "sha256:" + strings.Repeat("cd", 32)
	for range 2 {
		if status, body := scan(); status != 200 || !strings.Contains(body, `"image":"canyon-scan/opengrep@`+id+`"`) || !strings.Contains(body, `"ruleId":"r"`) {
			t.Fatalf("the SARIF of the built image, answered by its ID: %d %s", status, body)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "pruned"), nil, 0o600)) // the daemon no longer holds the ID
	if status, body := scan(); status != 200 || !strings.Contains(body, `"image":"canyon-scan/opengrep@`+id+`"`) {
		t.Fatalf("a pruned image is built again, not a failed scan: %d %s", status, body)
	}
	argv, copied := readIf(dir, "argv"), readIf(dir, "cp")
	if n := strings.Count(argv, "build\n--quiet\n--tag\ncanyon-scan/opengrep\n-\n"); n != 3 || readIf(dir, "dockerfile") != opengrepDockerfile {
		t.Errorf("one failed build, one that stands and one after the prune, of opengrepDockerfile: %d builds, Dockerfile %q", n, readIf(dir, "dockerfile"))
	}
	if !strings.Contains(argv, "\n--entrypoint\n/bin/sh\n"+id+"\n-c\n"+ThenJSON+"\nsh\n/usr/local/bin/opengrep\nscan\n--config\n/scan/rules\n--no-rewrite-rule-ids\n--sarif\n--json-output=/tmp/opengrep.json\n"+
		"--disable-nosem\n--x-ignore-semgrepignore-files\n--no-git-ignore\n--disable-version-check\n--max-target-bytes\n0\n--timeout\n0\n.\n") {
		t.Errorf("docker create runs the ID with opengrep's argv under the image's sh: %q", argv)
	}
	if !strings.Contains(copied, "./tree/src/a.ts\n") || !strings.Contains(copied, "./rules/r.yaml\n") || strings.Contains(copied, "docs") || strings.Contains(copied, "./opengrep.yaml") {
		t.Errorf("the tree without docs/ and the rules, no config file: %q", copied)
	}
}

// Opengrep's stdout is its SARIF, a NUL and its JSON output: the answer carries the distinct rule_ids, in
// name order, of the JSON's Rule parse errors ("" for one without), and the distinct paths, in path order,
// of its errors that have one. Without its JSON output — no NUL, nothing after it, or no errors in it —
// the answer is not_sarif, never a clean scan.
func TestOpengrepsAnswerCarriesTheRulesAndFilesItsJSONNames(t *testing.T) {
	og, old := scanTools["opengrep"], scanTools
	scanTools = map[string]scanTool{"opengrep": og}
	t.Cleanup(func() { scanTools = old })
	errs := `{"errors":[{"code":3,"level":"warn","type":["PartialParsing",[]],"path":"src/b.ts"},{"code":2,"level":"error","type":"Rule parse error","rule_id":"r","message":"Rule parse error in rule other:"},` +
		`{"type":"Rule parse error"},{"type":"Rule parse error","rule_id":"a"},{"type":"Rule parse error","rule_id":"r"},{"type":"Other","rule_id":"o"},{"path":"src/a.tsx"},{"path":"src/b.ts"}],"results":[]}`
	for _, c := range []struct{ json, want, unloaded, unparsed string }{
		{`\0` + errs, `"sarif":`, `"unloaded":["","a","r"]`, `"unparsed":["src/a.tsx","src/b.ts"]`},
		{`\0{"errors":[]}`, `"sarif":`, "", ""},
		{`\0`, `"not_finished":"not_sarif"`, "", ""},
		{`\0{"results":[]}`, `"not_finished":"not_sarif"`, "", ""},
		{"", `"not_finished":"not_sarif"`, "", ""},
	} {
		e, _ := scanDocker(t, "printf '%s"+c.json+"' '"+sarifOut+"'\n")
		srv := httptest.NewServer(Handler(e))
		t.Cleanup(srv.Close)
		status, body := postScan(t, srv.URL, opengrepReq, tarOf(t, map[string]string{"src/a.tsx": "x", "src/b.ts": "y"}), tarOf(t, map[string]string{"r.yaml": "rules: []"}))
		if status != 200 || !strings.Contains(body, c.want) || strings.Contains(body, "unparsed") != (c.unparsed != "") || !strings.Contains(body, c.unparsed) ||
			strings.Contains(body, "unloaded") != (c.unloaded != "") || !strings.Contains(body, c.unloaded) ||
			c.want != `"sarif":` && strings.Contains(body, `"sarif":`) {
			t.Errorf("stdout SARIF%s: %d %s", c.json, status, body)
		}
	}
}

// The script Opengrep runs under exits as Opengrep did, a signal's 128+n included, so rules_error and
// killed read the same, and its stdout is the tool's, then a NUL.
func TestThenJSONExitsAsTheToolDid(t *testing.T) {
	for inner, want := range map[string]int{"echo {}; exit 0": 0, "echo {}; exit 2": 2, "echo {}; echo noise >&2; kill -9 $$": 137} {
		out, err := exec.Command("sh", "-c", ThenJSON, "sh", "sh", "-c", inner).Output() //nolint:gosec // test: the cases above
		code := 0
		if e := (*exec.ExitError)(nil); errors.As(err, &e) {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != want || !bytes.HasPrefix(out, []byte("{}\n\x00")) {
			t.Errorf("%q: exit %d, stdout %q; want exit %d and {}, then a NUL", inner, code, out, want)
		}
	}
}

// A scan whose tool quotes a 91 KB minified line in each of 120 results — a stock dotnet new webapp's
// vendored jQuery, scanned (#1684) — prints 10.9 MB, past the cap, yet answers its SARIF: the snippets
// it quoted are not what Canyon keeps.
func TestAScanQuotingMinifiedLinesAnswersItsSARIF(t *testing.T) {
	e, _ := scanDocker(t, `l=$(head -c 91000 /dev/zero | tr '\0' a)
printf '{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Checkov"}},"results":['
i=0; while [ $i -lt 120 ]; do [ $i -gt 0 ] && printf ,; printf '{"ruleId":"CKV_X","message":{"text":"m"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a"},"region":{"startLine":1,"snippet":{"text":"%s"}}}}]}' "$l"; i=$((i+1)); done
printf ']}]}'`)
	pinTool(t, "busybox@sha256:"+strings.Repeat("ab", 32), []string{"scan", "/scan/tree"}, time.Minute) // 10.9 MB through a shell loop: a slow runner's seconds
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	status, body := postScan(t, srv.URL, checkovReq, tarOf(t, map[string]string{"a": "x"}))
	if status != 200 || !strings.Contains(body, `"sarif":`) || strings.Count(body, `"ruleId":"CKV_X"`) != 120 || strings.Contains(body, "aaaa") {
		t.Fatalf("%d %.300s", status, body)
	}
}

// An image whose tag the daemon holds is reused, an earlier executor's too, and one whose tag it no longer
// holds is built again, though its ID outlived the tag: pack and a
// scan are handed the tag, never the ID (#1734).
func TestAnImageWhoseTagIsGoneIsBuiltAgain(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	must(t, os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$1 $2\" in\n\"image inspect\") [ -f '"+dir+"/untagged' ] && exit 1; echo sha256:"+strings.Repeat("ab", 32)+";;\nbuild\\ *) echo build >> '"+dir+"/builds'; echo sha256:"+strings.Repeat("ab", 32)+";;\nesac\n"), 0o700)) //nolint:gosec // a test fake must be executable
	img := &scanImage{dockerfile: "FROM scratch\n", repo: "canyon-test:abc"}
	for range 2 {
		if id, err := img.built(t.Context(), docker); err != nil || id != "sha256:"+strings.Repeat("ab", 32) {
			t.Fatalf("built: %q %v", id, err)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "untagged"), nil, 0o600))
	if _, err := img.built(t.Context(), docker); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readIf(dir, "builds"), "build"); n != 1 {
		t.Fatalf("%d builds: none while the tag an earlier executor built is there, one once it was gone", n)
	}
}
