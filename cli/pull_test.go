package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pullHost is routes.md's packages/current row: a tar under Canyon-Package, or the envelope. cut
// sends that many bytes of the tar and drops the connection.
type pullHost struct {
	status         int // 0 answers 200
	body           string
	tar            []byte
	cut            int
	pkg            string // the Canyon-Package header when not the default; "-" sends none
	asked, accepts []string
	read           string // the project read's answer, when not the tar every path gets
}

func (h *pullHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.asked = append(h.asked, r.Method+" "+r.URL.RequestURI())
	h.accepts = append(h.accepts, r.Header.Get("Accept"))
	if h.read != "" && r.URL.Path == "/api/v1/projects/claims-portal" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, h.read)
		return
	}
	if h.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		_, _ = io.WriteString(w, h.body)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	switch h.pkg {
	case "":
		w.Header().Set("Canyon-Package", "pkg_9f2a1c")
	case "-":
	default:
		w.Header().Set("Canyon-Package", h.pkg)
	}
	if h.cut > 0 {
		_, _ = w.Write(h.tar[:h.cut])
		panic(http.ErrAbortHandler) // the client's tar reader sees the cut
	}
	_, _ = w.Write(h.tar)
}

// tarOf is entries as name, body, mode; a name ending in a slash is a directory. It begins with the
// pax global header git archive writes.
func tarOf(t *testing.T, entries ...[3]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	must(t, tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "0123abcd"}}))
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e[1]))}
		if e[2] == "x" {
			h.Mode = 0o755
		}
		if strings.HasSuffix(e[0], "/") {
			h.Typeflag = tar.TypeDir
		}
		must(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(e[1]))
		must(t, err)
	}
	must(t, tw.Close())
	return b.Bytes()
}

// twoFiles is the package every test pulls: one plain file and one executable under a directory.
func twoFiles(t *testing.T) *pullHost {
	t.Helper()
	return &pullHost{tar: tarOf(t, [3]string{"canyon.toml", "app = \"claims-portal\"\n", ""}, [3]string{"bin/", "", "x"}, [3]string{"bin/start", "#!/bin/sh\n", "x"})}
}

// pullIn runs canyon pull from parent, so a relative directory argument lands under it.
func pullIn(t *testing.T, h *pullHost, parent string, args ...string) (out, errs string, code int) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(parent)
	return run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, append([]string{"pull"}, args...)...)
}

func TestPullRendersItsGoldenAndRecordsTheBase(t *testing.T) {
	want := golden(t, "pull.txt") // before the run moves into the parent directory
	parent, h := t.TempDir(), twoFiles(t)
	out, errs, code := pullIn(t, h, parent, "--app", "claims-portal", "claims-portal")
	if code != exitOK || errs != "" || out != want {
		t.Fatalf("exit %d, stderr %q, rendered\n%s\ntestdata/pull.txt wants\n%s", code, errs, out, want)
	}
	if i := slices.Index(h.asked, "GET /api/v1/projects/claims-portal/packages/current"); i < 0 || h.accepts[i] != "application/x-tar, application/json" {
		t.Errorf("asked %q with Accept %q", h.asked, h.accepts)
	}
	dir := filepath.Join(parent, "claims-portal")
	for _, c := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"canyon.toml", "app = \"claims-portal\"\n", 0o644}, // a working tree's modes, as pack sends them (#1250)
		{"bin/start", "#!/bin/sh\n", 0o755},
		{".canyon/base", "app     = \"claims-portal\"\npackage = \"pkg_9f2a1c\"\n", 0o600},
		{".canyon/.gitignore", "*\n", 0o644},
	} {
		if body, mode := readFile(t, filepath.Join(dir, c.name)); body != c.body || mode != c.mode {
			t.Errorf("%s landed as %q at %v", c.name, body, mode)
		}
	}
	if mode := statOf(t, filepath.Join(dir, "bin")).Mode().Perm(); mode != 0o755&^umask(t) {
		t.Errorf("bin/ is %v", mode)
	}
	if got, other := baseIn(t, dir, "claims-portal"), baseIn(t, dir, "hr-onboarding"); got != "pkg_9f2a1c" || other != "" { // a base is the app's it was recorded under
		t.Errorf("the base reads back as %q for its app and %q for another", got, other)
	}
}

// An entry that escapes the directory is refused by the host's own check, and nothing lands
// above the directory.
func TestPullRefusesATarThatEscapesTheDirectory(t *testing.T) {
	parent := t.TempDir()
	out, errs, code := pullIn(t, &pullHost{tar: tarOf(t, [3]string{"../escaped.txt", "out\n", ""})}, parent, "--app", "claims-portal", "claims-portal")
	if code != exitError || out != "" || !strings.Contains(errs, `"../escaped.txt" escapes the tree`) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if exists(filepath.Join(parent, "escaped.txt")) || exists(filepath.Join(parent, "claims-portal", baseDir, baseFile)) {
		t.Error("the entry was written above the directory, or a refused pull recorded a base")
	}
}

// A stream that ends halfway leaves whatever landed and no base: a base names a whole tree or nothing.
func TestPullThatFailsHalfwayLeavesNoBase(t *testing.T) {
	parent, h := t.TempDir(), twoFiles(t)
	dir := filepath.Join(parent, "claims-portal")
	writeFile(t, filepath.Join(dir, baseDir, baseFile), "app     = \"claims-portal\"\npackage = \"pkg_000000\"\n") // what an earlier pull left
	h.cut = len(h.tar) - 600                                                                                       // inside the last entry
	out, errs, code := pullIn(t, h, parent, "--force", "--app", "claims-portal", "claims-portal")
	if code != exitError || out != "" || !strings.Contains(errs, "bad package") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if exists(filepath.Join(dir, baseDir, baseFile)) || baseIn(t, dir, "claims-portal") != "" {
		t.Error("a pull that failed halfway left a base file")
	}
}

func TestPullRefusesANonEmptyDirectoryUnlessForced(t *testing.T) {
	parent, h := t.TempDir(), twoFiles(t)
	dir := filepath.Join(parent, "claims-portal")
	writeFile(t, filepath.Join(dir, "notes.md"), "keep\n")
	out, errs, code := pullIn(t, h, parent, "--app", "claims-portal", "claims-portal")
	if code != exitError || out != "" || !strings.Contains(errs, "claims-portal is not empty") || !strings.Contains(errs, "--force") || !slices.Equal(h.asked, []string{"GET /api/v1/projects/claims-portal"}) || exists(filepath.Join(dir, "canyon.toml")) { // the project read alone: is the code in git?
		t.Errorf("exit %d, stdout %q, stderr %q, asked %q, and the directory was written into", code, out, errs, h.asked)
	}
	if _, errs, code := pullIn(t, twoFiles(t), parent, "--force", "--app", "claims-portal", "claims-portal"); code != exitOK {
		t.Fatalf("--force: exit %d, stderr %q", code, errs)
	}
	body, _ := readFile(t, filepath.Join(dir, "canyon.toml"))
	kept, _ := readFile(t, filepath.Join(dir, "notes.md")) // pull deletes nothing
	if body != "app = \"claims-portal\"\n" || kept != "keep\n" {
		t.Errorf("--force left canyon.toml as %q and notes.md as %q", body, kept)
	}
	h = twoFiles(t) // the canyon.toml that landed now names the app, so --app is not needed
	if _, errs, code := pullIn(t, h, parent, "--force", "claims-portal"); code != exitOK || h.asked[0] != "GET /api/v1/projects/claims-portal/packages/current" {
		t.Errorf("without --app: exit %d, stderr %q, asked %q", code, errs, h.asked)
	}
}

// An app a repository people push to feeds has its code in git: pull says git pull, before a file lands
// or the package is asked for, and --force pulls anyway (cli.md §6.6, #1938). An app Canyon keeps pulls.
func TestPullOfAnAppARepositoryFeedsSaysGitPull(t *testing.T) {
	parent := t.TempDir()
	h := twoFiles(t)
	h.read = `{"project":{"id":"claims-portal","repository":{"repo_id":555,"full_name":"acme/claims-portal","folder":"","state":"linked","keeps":false,"kept_commit":null,"kept_error":""}}}`
	out, errs, code := pullIn(t, h, parent, "--app", "claims-portal", "claims-portal")
	if code != exitError || out != "" || errs != "\n  ✗ claims-portal's code is in github.com/acme/claims-portal. Use git pull.\n" || len(h.asked) != 1 || exists(filepath.Join(parent, "claims-portal", "canyon.toml")) {
		t.Errorf("exit %d, stdout %q, stderr %q, asked %q", code, out, errs, h.asked)
	}
	if _, errs, code := pullIn(t, h, parent, "--force", "--app", "claims-portal", "claims-portal"); code != exitOK {
		t.Errorf("--force: exit %d, stderr %q", code, errs)
	}
	kept := twoFiles(t)
	kept.read = strings.Replace(h.read, `"keeps":false`, `"keeps":true`, 1)
	if _, errs, code := pullIn(t, kept, t.TempDir(), "--app", "claims-portal", "claims-portal"); code != exitOK {
		t.Errorf("an app Canyon keeps: exit %d, stderr %q", code, errs)
	}
}

func TestPullRefusals(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "hr", projectFile), "app = \"hr-onboarding\"\n")
	for _, c := range []struct {
		name string
		host *pullHost
		args []string
		want string
	}{
		{"another app's canyon.toml, even forced", twoFiles(t), []string{"--force", "--app", "claims-portal", "hr"}, `already names the app "hr-onboarding", and --app asked for "claims-portal"`},
		{"no canyon.toml and no --app", twoFiles(t), []string{"empty"}, "canyon pull --app <name>"},
		{"no package yet", &pullHost{status: http.StatusNotFound, body: `{"error":"not_found","message":"no package yet"}`}, []string{"--app", "claims-portal", "empty"}, "\n  ✗ claims-portal has no package yet, so there is nothing to pull (or there is no such app).\n"},
		{"json", twoFiles(t), []string{"--format", "json", "--app", "claims-portal", "empty"}, "pull has no json rendering"},
		{"two directories", twoFiles(t), []string{"--app", "claims-portal", "a", "b"}, `got "b" as well`},
	} {
		out, errs, code := pullIn(t, c.host, parent, c.args...)
		if code != exitError || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.name, code, out, errs)
		}
		if c.host.status == 0 && len(c.host.asked) > 0 {
			t.Errorf("%s reached the host: %q", c.name, c.host.asked)
		}
	}
}

// A base that was edited reads as no base, and an id with a space is never written; the logs verb
// still asks for text; and the base never travels in a package, where it would change the tree
// hash the host dedupes on.
func TestPullTamperedBaseLogsAcceptAndTheBaseIsNeverPacked(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{
		"app     = \"claims-portal\"\npackage = \"pkg_9f2a 1c\"\n",
		"app     = \"claims-portal\"\npackage = \"pkg_9f2a\n1c\"\n",
		"package = \"pkg_9f2a1c\"\n",
	} {
		writeFile(t, filepath.Join(dir, baseDir, baseFile), body)
		if got, _ := readBase(dir, "claims-portal"); got != "" {
			t.Errorf("%q read as the base %q", body, got)
		}
	}
	if err := writeBase(dir, "claims-portal", "pkg_9f2a 1c"); err == nil {
		t.Error("an id with a space was written")
	}
	l := &logsHost{}
	if _, errs, code := logsRun(t, l); code != exitOK || l.accepts[0] != "text/plain, application/json" {
		t.Errorf("logs: exit %d, stderr %q, Accept %q", code, errs, l.accepts)
	}
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "index.js"), "1\n")
	must(t, writeBase(dir, "claims-portal", "pkg_9f2a1c"))
	var b bytes.Buffer
	files, _, skip, err := pack(dir, &b)
	must(t, err)
	if list, _, _ := archive(t, &b); files != 1 || skip.Paths != 0 || strings.Contains(list, baseDir) {
		t.Errorf("packed %d files, skipped %d:\n%s", files, skip.Paths, list)
	}
}

// A package id this build will not record is refused before a byte lands, and a refused or absent
// host touches nothing: the base that was there is still true of the tree that is there.
func TestPullChecksTheHeaderBeforeWritingAndKeepsTheBaseWhenTheHostRefuses(t *testing.T) {
	for _, bad := range []string{"pkg_9f2a 1c", "-"} { // a control byte in a header never leaves Go's HTTP stack, so it is not a case
		parent := t.TempDir()
		h := twoFiles(t)
		h.pkg = bad
		out, errs, code := pullIn(t, h, parent, "--app", "claims-portal", "claims-portal")
		if code != exitError || out != "" || !strings.Contains(errs, "package") || exists(filepath.Join(parent, "claims-portal", "bin", "start")) || exists(filepath.Join(parent, "claims-portal", baseDir)) {
			t.Errorf("header %q: exit %d, stdout %q, stderr %q, files landed %v", bad, code, out, errs, exists(filepath.Join(parent, "claims-portal", "bin", "start")))
		}
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "claims-portal")
	must(t, os.MkdirAll(dir, 0o750), writeBase(dir, "claims-portal", "pkg_000000"))
	must(t, os.WriteFile(filepath.Join(dir, projectFile), []byte("app = \"claims-portal\"\n"), 0o600))
	_, _, code := pullIn(t, &pullHost{status: http.StatusNotFound, body: `{"error":"not_found","message":"claims-portal isn't on this Canyon yet. Deploy it first: canyon deploy"}`}, parent, "--force", "claims-portal")
	if body, _ := readFile(t, filepath.Join(dir, baseDir, baseFile)); code != exitError || !strings.Contains(body, "pkg_000000") {
		t.Errorf("a 404 must leave the base alone: exit %d, base %q", code, body)
	}
}

// umask is this process's file mode creation mask, which a directory MkdirAll makes is created under.
func umask(t *testing.T) os.FileMode {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "probe")
	must(t, os.Mkdir(dir, 0o777)) //nolint:gosec // the widest mode, so the mask alone decides what is created
	return 0o777 &^ statOf(t, dir).Mode().Perm()
}
