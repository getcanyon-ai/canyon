package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ingest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func tarOf(t *testing.T, entries map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(entries[n]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entries[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// upload posts a tar; base and force are the header and the query, "" for neither.
func upload(t *testing.T, c *http.Client, base, project, tarBody, basePkg, query string) (int, map[string]any) {
	t.Helper()
	return uploadWith(t, c, base, project, tarBody, map[string]string{"Canyon-Base-Package": basePkg}, query)
}

// uploadPulled posts a tar as a copy that pulled first: based on the project's current package, and
// on none before there is one. Without a base, an upload onto a current package is stale (#1503).
func uploadPulled(t *testing.T, c *http.Client, base, project, tarBody string) (int, map[string]any) {
	t.Helper()
	return uploadWithPulled(t, c, base, project, tarBody, nil)
}

// uploadWithPulled is uploadPulled with the given headers as well.
func uploadWithPulled(t *testing.T, c *http.Client, base, project, tarBody string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	_, head, _ := pull(t, c, base, project)
	based := map[string]string{"Canyon-Base-Package": head}
	maps.Copy(based, headers)
	return uploadWith(t, c, base, project, tarBody, based, "")
}

// uploadWith posts a tar with the given headers (empty values are not sent).
func uploadWith(t *testing.T, c *http.Client, base, project, tarBody string, headers map[string]string, query string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/projects/"+project+"/packages"+query, strings.NewReader(tarBody))
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	lastBody = strings.TrimSpace(raw.String())
	_ = json.Unmarshal(raw.Bytes(), &out)
	return resp.StatusCode, out
}

func packageOf(body map[string]any) map[string]any {
	p, _ := body["package"].(map[string]any)
	return p
}

func withProject(t *testing.T) (*pgxpool.Pool, context.Context, string, *http.Client) {
	t.Helper()
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := anonymous(t, ctx, pool, base)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	return pool, ctx, base, c
}

// anonymous is a client holding a live session row for the anonymous person, as a demo host's caller
// is, so a test can make that caller someone else by updating the row (a demo host stores none itself).
func anonymous(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base string) *http.Client {
	t.Helper()
	id, err := token()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (id, actor_kind, actor_subject, actor_display, flow, expires_at) VALUES ($1, 'person', 'anonymous', '', 'browser', now() + interval '1 hour')", id); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	u, _ := neturl.Parse(base)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: id}}) //nolint:gosec // test: the row just inserted
	return c
}

func TestUploadingATarCreatesThePackageTheContractPromises(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "package.json": "{}\n"}), "", "")
	p := packageOf(body)
	tree, _ := p["tree_hash"].(string)
	commit, _ := p["commit"].(string)
	if code != 201 || len(tree) != 40 || len(commit) != 40 || p["id"] != "pkg_"+tree[:6] {
		t.Fatalf("upload: %d %s", code, lastBody)
	}
	if src := p["source"].(map[string]any); src["kind"] != "upload" || src["ref"] != "" || src["actor"] != "anonymous" || len(src) != 3 || len(p) != 4 {
		t.Fatalf("the element: %s", lastBody)
	}
	var size int64
	var kind string
	if err := pool.QueryRow(ctx, "SELECT size_bytes, source_kind FROM packages WHERE id = $1 AND project_id = 'claims-portal'", p["id"]).Scan(&size, &kind); err != nil || size != 13 || kind != "upload" {
		t.Fatalf("the row: %d %s (%v)", size, kind, err)
	}
}

func TestUploadingTheSameTreeTwiceAnswersTheSamePackageByItsLastUploaderAndKeepsOneRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	body := tarOf(t, map[string]string{"src/app.ts": "export {}\n"})
	_, first := uploadPulled(t, c, base, "claims-portal", body)
	// every v1 session is the same anonymous actor, so the first uploader is renamed on the row to tell the two apart
	if _, err := pool.Exec(ctx, "UPDATE packages SET source_actor = 'dana' WHERE id = $1 AND project_id = 'claims-portal'", packageOf(first)["id"]); err != nil {
		t.Fatal(err)
	}
	code, second := uploadPulled(t, c, base, "claims-portal", body)
	if code != 201 || packageOf(second)["id"] != packageOf(first)["id"] || packageOf(second)["commit"] != packageOf(first)["commit"] {
		t.Fatalf("the same tree again: %d %s", code, lastBody)
	}
	if src := packageOf(second)["source"].(map[string]any); src["kind"] != "upload" || src["actor"] != "anonymous" {
		t.Fatalf("the package keeps its kind and names who uploaded it last (contracts/routes.md, canyonv2-o49o): %s", lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages"); n != 1 {
		t.Fatalf("package rows: %d", n)
	}
}

func TestUploadRefusesAStaleBaseAndForceOverridesIt(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	// A base on a project with no current package replaces nothing, so it is a first upload (#1678).
	code, a := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "pkg_nothing", "")
	if _, pulled, _ := pull(t, c, base, "claims-portal"); code != 201 || pulled != packageOf(a)["id"] || strings.Contains(lastBody, "forced") {
		t.Fatalf("a base on a project with no packages: %d %s, current %q", code, lastBody, pulled)
	}
	aID, _ := packageOf(a)["id"].(string)
	code, b := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), aID, "")
	if code != 201 {
		t.Fatalf("based on the head: %d %s", code, lastBody)
	}
	bID, _ := packageOf(b)["id"].(string)
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, "")
	if code != 409 || body["error"] != "stale_base" || body["head_package"] != bID || body["moved_by"] != "anonymous" {
		t.Fatalf("based on a package that is no longer the head: %d %s", code, lastBody)
	}
	if at, _ := body["moved_at"].(string); at == "" || !strings.HasSuffix(at, "Z") {
		t.Fatalf("moved_at is a UTC timestamp: %s", lastBody)
	}
	if !strings.Contains(lastBody, `"moved_from":{"kind":"upload","ref":""}`) { // the head's source: a push's names its commit (#1938)
		t.Fatalf("moved_from: %s", lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE packages SET source_kind = 'repo', source_ref = 'a1b2c3d4e5f60718293a4b5c6d7e8f9001122334' WHERE id = $1", bID); err != nil {
		t.Fatal(err)
	}
	if upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, ""); !strings.Contains(lastBody, `"moved_from":{"kind":"repo","ref":"a1b2c3d4e5f60718293a4b5c6d7e8f9001122334"}`) {
		t.Fatalf("a pushed head's moved_from: %s", lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, "?force=yes"); code != 409 {
		t.Fatalf("only the exact force=true is force: %d %s", code, lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, "?force=true"); code != 201 {
		t.Fatalf("forced: %d %s", code, lastBody)
	}
}

// An upload with no base on an app that has a current package is stale (contracts/routes.md, #1503):
// a fresh clone never silently replaces it. The first upload of an app with only a check goes through,
// and a check's upload reads no base.
func TestAnUploadWithNoBaseIsStaleOnceTheAppHasACurrentPackage(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "0"}), "", "?check=true"); code != 201 {
		t.Fatalf("a first check: %d %s", code, lastBody)
	}
	code, a := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	aID, _ := packageOf(a)["id"].(string)
	if code != 201 {
		t.Fatalf("the first upload of an app with only a check: %d %s", code, lastBody)
	}
	if code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), "", ""); code != 409 || body["error"] != "stale_base" || body["head_package"] != aID || body["moved_by"] != "anonymous" {
		t.Fatalf("no base on an app with a current package: %d %s", code, lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), "", "?check=true"); code != 201 {
		t.Fatalf("a check with no base: %d %s", code, lastBody)
	}
	code, forced := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), "", "?force=true")
	if code != 201 {
		t.Fatalf("forced with no base: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/packages' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil || !reflect.DeepEqual(detail, map[string]any{"package": packageOf(forced)["id"], "forced": true}) {
		t.Fatalf("the forced no-base upload's audit detail: %v (%v), want it forced", detail, err)
	}
}

// An app whose only package a pull request packaged has no current package: an upload with no base
// is its first, not a stale one, and becomes current (#1503).
func TestAnUploadWithNoBaseOnAnAppWithOnlyAPullRequestsPackageGoesThrough(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	pr, err := ingest.IngestChecked(ctx, filepath.Join(reposOf(t, base), "claims-portal.git"), strings.NewReader(tarOf(t, map[string]string{"a": "1"})), nil, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ($1, 'claims-portal', $2, $3, 1, 'repo-pr')`, pr.ID, pr.TreeHash, pr.Commit); err != nil {
		t.Fatal(err)
	}
	code, up := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), "", "")
	if _, pulled, _ := pull(t, c, base, "claims-portal"); code != 201 || pulled != packageOf(up)["id"] {
		t.Fatalf("no base on an app with only a pull request's package: %d %s, current %q", code, lastBody, pulled)
	}
}

// A tree uploaded again is current again, keeping its first source and label (contracts/routes.md,
// canyonv2-o49o): A, B, then A again, and a deploy based on that answer is not told it moved.
func TestATreeUploadedAgainIsCurrentAgain(t *testing.T) {
	_, _, base, c := withProject(t)
	_, a := uploadWith(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1"}, "")
	aID, _ := packageOf(a)["id"].(string)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), aID, "")
	code, again := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	if code != 201 || packageOf(again)["id"] != aID || packageOf(again)["label"] != "v1" {
		t.Fatalf("the same tree again: %d %s", code, lastBody)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, ""); code != 201 {
		t.Fatalf("based on the tree just uploaded again: %d %s", code, lastBody)
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", "")
	if rows := page["packages"].([]any); len(rows) != 3 || rows[1].(map[string]any)["id"] != aID {
		t.Fatalf("the list shows A where it became current again: %s", lastBody)
	}
}

// The trail tells a forced upload from a plain one (contracts/routes.md row 76): only an upload
// that overrode a stale base carries detail.forced; a plain upload's detail is just the package.
func TestAForcedUploadIsAuditedAsForced(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	newestDetail := func() map[string]any {
		t.Helper()
		var detail map[string]any
		if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/packages' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}
	_, a := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	aID, _ := packageOf(a)["id"].(string)
	if want := (map[string]any{"package": aID}); !reflect.DeepEqual(newestDetail(), want) {
		t.Fatalf("a plain upload's detail: %v, want %v", newestDetail(), want)
	}
	_, b := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), aID, "")
	bID, _ := packageOf(b)["id"].(string)
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, ""); code != 409 {
		t.Fatalf("a stale base without force: %d %s", code, lastBody)
	}
	if got := newestDetail(); !reflect.DeepEqual(got, map[string]any{"package": bID}) {
		t.Fatalf("the refused upload left a row: %v", got)
	}
	code, forced := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, "?force=true")
	if code != 201 {
		t.Fatalf("forced: %d %s", code, lastBody)
	}
	fID, _ := packageOf(forced)["id"].(string)
	if want := (map[string]any{"package": fID, "forced": true}); !reflect.DeepEqual(newestDetail(), want) {
		t.Fatalf("a forced upload's detail: %v, want %v", newestDetail(), want)
	}
	// force on a current base overrode nothing: a plain upload on the trail
	_, d := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "4"}), fID, "?force=true")
	dID, _ := packageOf(d)["id"].(string)
	if want := (map[string]any{"package": dID}); !reflect.DeepEqual(newestDetail(), want) {
		t.Fatalf("force over a current base: %v, want %v", newestDetail(), want)
	}
}

// A labelled upload's audit row names the version, not just the id (contracts/routes.md, *Naming
// a version*): the dashboard's "uploaded v1.4" sentence reads detail.label, and an upload with no
// label carries no label key at all — not a null a screen would have to test for.
func TestALabelledUploadIsAuditedWithItsLabel(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	detailOf := func(pkg string) map[string]any { // by the package, not the newest row: ts is the database's clock, which can step back
		t.Helper()
		var detail map[string]any
		if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'POST /api/v1/projects/{id}/packages' AND detail->>'package' = $1", pkg).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}
	code, labelled := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.4"})
	if code != 201 {
		t.Fatalf("a labelled upload: %d %s", code, lastBody)
	}
	lID, _ := packageOf(labelled)["id"].(string)
	if want := (map[string]any{"package": lID, "label": "v1.4"}); !reflect.DeepEqual(detailOf(lID), want) {
		t.Fatalf("a labelled upload's detail: %v, want %v", detailOf(lID), want)
	}
	code, plain := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}))
	if code != 201 {
		t.Fatalf("an unlabelled upload: %d %s", code, lastBody)
	}
	pID, _ := packageOf(plain)["id"].(string)
	if got := detailOf(pID); !reflect.DeepEqual(got, map[string]any{"package": pID}) {
		t.Fatalf("an unlabelled upload's detail carries no label key: %v", got)
	}
}

func TestUploadAppliesTheSnapshotsExclusions(t *testing.T) {
	_, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{".env": "SECRET=1\n", ".env.local": "x\n", "node_modules/left-pad/index.js": "x\n", "dist/app.js": "x\n", "src/app.ts": "export {}\n"}), "", "")
	commit, _ := packageOf(body)["commit"].(string)
	dir := t.TempDir()
	if err := ingest.Extract(ctx, filepath.Join(reposOf(t, base), "claims-portal.git"), commit, dir); err != nil {
		t.Fatal(err)
	}
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	if strings.Join(files, ",") != "src/app.ts" {
		t.Fatalf("the committed tree: %v", files)
	}
}

func TestUploadRefusesAnUnknownADeletedAndAMalformedProject(t *testing.T) {
	_, _, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	for _, id := range []string{"nothing", "gone", "claims.portal", "Claims"} {
		if code, body := upload(t, c, base, id, tarOf(t, map[string]string{"a": "1"}), "", ""); code != 404 || body["error"] != "not_found" {
			t.Fatalf("%s: %d %s", id, code, lastBody)
		}
	}
	if _, err := os.Stat(filepath.Join(reposOf(t, base), "gone.git")); !os.IsNotExist(err) {
		t.Fatalf("a refused upload created a repository: %v", err)
	}
}

func TestUploadRefusesATarThatEscapesTheTreeAndLeavesNoRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"../evil": "x"}), "", ""); code != 400 || body["error"] != "bad_request" || !strings.Contains(body["message"].(string), "../evil") {
		t.Fatalf("an escaping entry: %d %s", code, lastBody)
	}
	if code, body := upload(t, c, base, "claims-portal", "this is not a tar", "", ""); code != 400 || body["error"] != "bad_request" {
		t.Fatalf("not a tar: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages"); n != 0 {
		t.Fatalf("package rows after two refusals: %d", n)
	}
}

func TestOnlyTheUploadThatTookPlaceIsAuditedAndItNamesThePackage(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), "pkg_nothing", "") // 409: a version is current
	upload(t, c, base, "nothing", tarOf(t, map[string]string{"a": "1"}), "", "")                  // 404
	var pkg *string
	if err := pool.QueryRow(ctx, "SELECT detail->>'package' FROM audit_events WHERE action = $1", "POST /api/v1/projects/{id}/packages").Scan(&pkg); err != nil || pkg == nil || *pkg != packageOf(body)["id"] {
		t.Fatalf("the audit row names the package it produced: %v (%v)", pkg, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != 2 {
		t.Fatalf("audit rows (the project's creation, the upload): %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE project_id = 'nothing'"); n != 0 {
		t.Fatalf("the refused upload left %d audit rows", n)
	}
}

func TestUploadRefusesAnExcludedEntryDeclaredOverTheCapBeforeReadingIt(t *testing.T) {
	_, _, base, c := withProject(t)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "node_modules/huge", Mode: 0o644, Size: 600 << 20}); err != nil { // the header alone: a body that size is never sent
		t.Fatal(err)
	}
	code, body := upload(t, c, base, "claims-portal", b.String(), "", "")
	if msg, _ := body["message"].(string); code != 400 || body["error"] != "bad_request" || !strings.Contains(msg, "exceeds") {
		t.Fatalf("an excluded entry over the cap is refused at its header: %d %s", code, lastBody)
	}
}

func TestTheSameTreeInTwoProjectsIsTwoPackagesWithOneId(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"payroll"}`)
	body := tarOf(t, map[string]string{"src/app.ts": "export {}\n"})
	_, first := upload(t, c, base, "claims-portal", body, "", "")
	code, second := upload(t, c, base, "payroll", body, "", "")
	if code != 201 || packageOf(second)["id"] != packageOf(first)["id"] {
		t.Fatalf("the same tree in a second project: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM packages"); n != 2 {
		t.Fatalf("package rows: %d", n)
	}
}

// pull fetches the current package and untars it: the header, the files, the raw status.
func pull(t *testing.T, c *http.Client, base, project string) (int, string, map[string]string) {
	t.Helper()
	resp, err := c.Get(base + "/api/v1/projects/" + project + "/packages/current")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(resp.Body)
		lastBody = strings.TrimSpace(raw.String())
		return resp.StatusCode, "", nil
	}
	files := map[string]string{}
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the tar on the wire: %v", err) // a cut stream is a failure, not the end
		}
		if h.Typeflag != tar.TypeReg {
			continue // git archive's pax header and the directories
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(tr)
		files[h.Name] = b.String()
	}
	return resp.StatusCode, resp.Header.Get("Canyon-Package"), files
}

func TestPullReturnsTheCurrentPackageAsATarWithItsHeaderAndWritesNoAuditRow(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n", "package.json": "{}\n"}), "", "")
	before := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	code, pkg, files := pull(t, c, base, "claims-portal")
	if code != 200 || pkg != packageOf(body)["id"] {
		t.Fatalf("pull: %d %q", code, pkg)
	}
	if len(files) != 2 || files["src/app.ts"] != "export {}\n" || files["package.json"] != "{}\n" {
		t.Fatalf("the tree on the wire: %v", files)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != before {
		t.Fatalf("a pull is a read: %d audit rows written", n-before)
	}
}

func TestPullServesTheHeadAfterASecondUploadAndAReuploadMakesTheFirstCurrentAgain(t *testing.T) {
	_, _, base, c := withProject(t)
	first := tarOf(t, map[string]string{"a": "1"})
	_, one := uploadPulled(t, c, base, "claims-portal", first)
	_, two := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}))
	if _, pkg, files := pull(t, c, base, "claims-portal"); pkg != packageOf(two)["id"] || files["a"] != "2" {
		t.Fatalf("after the second upload: %q %v", pkg, files)
	}
	uploadPulled(t, c, base, "claims-portal", first) // the same tree as the first: answers it, and makes it current again (canyonv2-o49o)
	if _, pkg, files := pull(t, c, base, "claims-portal"); pkg != packageOf(one)["id"] || files["a"] != "1" {
		t.Fatalf("after re-uploading the first tree: %q %v", pkg, files)
	}
	// current is the newest package row, the one pull serves and the one a stale base is refused against
	if code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), packageOf(two)["id"].(string), ""); code != 409 || body["head_package"] != packageOf(one)["id"] {
		t.Fatalf("the 409 names the package pull serves: %d %s", code, lastBody)
	}
}

// The database's clock can step back between two uploads (a VM's clock is resynced to its host's
// every few seconds): the second upload is still the newer package, and current.
func TestAnUploadAfterTheClockStepsBackIsStillCurrent(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	if _, err := pool.Exec(ctx, "UPDATE packages SET created_at = now() + interval '1 second'"); err != nil { // as if the clock stepped back a second since
		t.Fatal(err)
	}
	code, second := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}))
	if _, pkg, _ := pull(t, c, base, "claims-portal"); code != 201 || pkg != packageOf(second)["id"] {
		t.Fatalf("current after the clock stepped back: %q, want %v (%d)", pkg, packageOf(second)["id"], code)
	}
}

func TestPullAnswersTheErrorWhenTheRepositoryIsGoneBeforeAByteIsSent(t *testing.T) {
	_, _, base, c := withProject(t)
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if err := os.RemoveAll(filepath.Join(reposOf(t, base), "claims-portal.git")); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(base + "/api/v1/projects/claims-portal/packages/current")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	if resp.StatusCode != 500 || resp.Header.Get("Canyon-Package") != "" || !strings.Contains(raw.String(), `"error":"internal"`) {
		t.Fatalf("the repository is gone: %d %q %s", resp.StatusCode, resp.Header.Get("Canyon-Package"), raw.String())
	}
}

func TestPullWithoutAPackageIsNotFound(t *testing.T) {
	_, _, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"gone"}`)
	upload(t, c, base, "gone", tarOf(t, map[string]string{"a": "1"}), "", "")
	do(t, c, http.MethodDelete, base+"/api/v1/projects/gone?confirm=gone", "")
	// the malformed id is refused by the guard before any lookup; the rest fold into "no package yet"
	for id, message := range map[string]string{"claims-portal": "no package yet", "nothing": "no package yet", "gone": "no package yet", "Claims": `\"Claims\" isn't on this Canyon yet. Deploy it first: canyon deploy`} {
		if code, _, _ := pull(t, c, base, id); code != 404 || !strings.Contains(lastBody, `"error":"not_found"`) || !strings.Contains(lastBody, message) {
			t.Fatalf("%s: %d %s, want %q", id, code, lastBody, message)
		}
	}
}

func TestAnUploadMayCarryALabelAndTheLabelNamesOnePackageForGood(t *testing.T) {
	_, _, base, c := withProject(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"payroll"}`)
	label := func(l string) map[string]string { return map[string]string{"Canyon-Package-Label": l} }
	code, first := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), label("v1.0"))
	if code != 201 || packageOf(first)["label"] != "v1.0" {
		t.Fatalf("a labelled upload: %d %s", code, lastBody)
	}
	if code, body := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), label("v1.0")); code != 409 || body["error"] != "label_taken" || body["package"] != packageOf(first)["id"] {
		t.Fatalf("a label names one package: %d %s", code, lastBody)
	}
	if code, body := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), label("v1.0")); code != 201 || packageOf(body)["id"] != packageOf(first)["id"] || packageOf(body)["label"] != "v1.0" {
		t.Fatalf("the same tree with the same label again: %d %s", code, lastBody)
	}
	if code, body := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), label("v1.1")); code != 409 || body["error"] != "label_taken" || body["package"] != packageOf(first)["id"] {
		t.Fatalf("labels never move — a new label on an existing tree: %d %s", code, lastBody)
	}
	if code, _ := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"})); code != 201 || strings.Contains(lastBody, `"label"`) {
		t.Fatalf("no label is no label key at all, not a null: %d %s", code, lastBody)
	}
	for _, bad := range []string{"V1", "-x", ".hidden", strings.Repeat("a", 65), "v 1"} {
		if code, body := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "4"}), label(bad)); code != 400 || body["error"] != "invalid_label" {
			t.Fatalf("%q: %d %s", bad, code, lastBody)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/projects/claims-portal/packages", strings.NewReader(tarOf(t, map[string]string{"a": "5"})))
	req.Header["Canyon-Package-Label"] = []string{""} // present and empty is not absent
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatalf("an empty label header: %v %d", err, resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	var detail *string
	if err := poolOf(t, base).QueryRow(context.Background(), "SELECT detail->>'package' FROM audit_events WHERE action = $1 ORDER BY ts LIMIT 1", "POST /api/v1/projects/{id}/packages").Scan(&detail); err != nil || detail == nil || *detail != packageOf(first)["id"] {
		t.Fatalf("the labelled upload's audit row names the package by id: %v (%v)", detail, err)
	}
	// the database holds the line on its own (migration 0007): one label per project, whatever reaches it
	if _, err := poolOf(t, base).Exec(context.Background(), "INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, label) VALUES ('pkg_dup000', 'claims-portal', 'x', 'y', 1, 'upload', 'v1.0')"); err == nil {
		t.Fatal("the unique index accepted a second v1.0 in the project")
	}
	if code, body := uploadWithPulled(t, c, base, "payroll", tarOf(t, map[string]string{"b": "1"}), label("v1.0")); code != 201 || packageOf(body)["label"] != "v1.0" {
		t.Fatalf("a label is unique within its project only: %d %s", code, lastBody)
	}
}

func TestAGateRunNamesAPackageByLabelOrById(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, tagged := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.0"})
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"})) // the head moves on
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"v1.0"}`)
	run, _ := runOf(started)["id"].(string)
	var reviewed string
	if code != 202 || pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&reviewed) != nil || reviewed != packageOf(tagged)["id"] {
		t.Fatalf("by label: %d %s → %q", code, lastBody, reviewed)
	}
	waitVerdict(t, c, base, run)
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"v9.9"}`); code != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown label: %d %s", code, lastBody)
	}
	// a deploy names a package the same way (the gate is the golden log here: it ends blocked, deploys nothing)
	code, started, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", `{"package":"v1.0"}`)
	run, _ = runOf(started)["id"].(string)
	if code != 202 || pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&reviewed) != nil || reviewed != packageOf(tagged)["id"] {
		t.Fatalf("a deploy by label: %d %s → %q", code, lastBody, reviewed)
	}
	waitVerdict(t, c, base, run)
	// a label shaped like an id names its package unless a package has that id
	_, shaped := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), map[string]string{"Canyon-Package-Label": "pkg_zzzzzz"})
	code, started, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"pkg_zzzzzz"}`)
	run, _ = runOf(started)["id"].(string)
	if code != 202 || pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&reviewed) != nil || reviewed != packageOf(shaped)["id"] {
		t.Fatalf("a label shaped like an id: %d %s → %q", code, lastBody, reviewed)
	}
	waitVerdict(t, c, base, run)
	// and when a package does have that id, the id wins over the label
	taggedID, _ := packageOf(tagged)["id"].(string)
	_, impostor := uploadWithPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "4"}), map[string]string{"Canyon-Package-Label": taggedID})
	code, started, _ = do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+taggedID+`"}`)
	run, _ = runOf(started)["id"].(string)
	if code != 202 || pool.QueryRow(ctx, "SELECT package_id FROM runs WHERE id = $1", run).Scan(&reviewed) != nil || reviewed != taggedID || reviewed == packageOf(impostor)["id"] {
		t.Fatalf("an id beats a label of the same text: %d %s → %q", code, lastBody, reviewed)
	}
	waitVerdict(t, c, base, run)
}

// A pull request's package is reviewed and never current: not pulled, not listed, not what a run
// defaults to, not the project's latest package, and its declarations are not the project's.
func TestAPullRequestPackageNeverBecomesCurrent(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, up := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, resources, created_at)
		VALUES ('pkg_pr', 'claims-portal', 'x', 'x', 1, 'repo-pr', '{"db": "postgres"}', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	want := packageOf(up)["id"]
	_, pulled, _ := pull(t, c, base, "claims-portal")
	_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", "")
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	project, _ := read["project"].(map[string]any)
	latest, _ := project["latest"].(map[string]any)
	if code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", `{"package":"pkg_pr"}`); code != 409 || out["error"] != "review_not_passed" { // a person may once its review passes (#1947); sign-in is off here
		t.Fatalf("a deploy of a pull request's package no review passed: %d %s", code, lastBody)
	}
	packages, _ := list["packages"].([]any)
	if resources, ok := project["resources"].([]any); pulled != want || len(packages) != 1 || latest["package"] != want || !ok || len(resources) != 0 {
		t.Fatalf("current is %q, want %v; listed %d, latest %v, resources %v", pulled, want, len(packages), latest["package"], project["resources"])
	}
}

// A pull request's run is reported on its GitHub check, not as the project's state: a blocked run holding
// a secret changes neither the gate state, the last run, the receipt, the fleet's tiles nor a policy
// preview. (It still takes the project's one run slot and is in the runs list, on purpose.)
func TestAPullRequestRunNeverChangesWhatTheProjectShows(t *testing.T) {
	withModel(t, &scripted{}) // auth.unauthenticated-route blocking before preview needs a model on the host (#1555)
	pool, ctx, base, c := withProject(t)
	_, up := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	if _, err := pool.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, ended_at) VALUES ('gr_main', 'claims-portal', $1, 'clean', now())", packageOf(up)["id"]); err != nil {
		t.Fatal(err)
	}
	verdict := `{"state": "blocked", "blocking": 1, "findings": {"runs": [{"results": [{"ruleId": "secrets.client-bundle", "properties": {"canyon": {"blocks": true}}},
		{"ruleId": "auth.unauthenticated-route", "properties": {"canyon": {"severity": "critical", "blocks": true}}}]}]}}`
	if _, err := pool.Exec(ctx, `WITH p AS (INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, created_at) VALUES ('pkg_pr', 'claims-portal', 'x', 'x', 1, 'repo-pr', now() + interval '1 hour')),
		r AS (INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at) VALUES ('gr_pr', 'claims-portal', 'pkg_pr', 'blocked', now() + interval '1 hour', now() + interval '1 hour'))
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_pr', 'claims-portal', 'verdict', 'verdict', $1)`, verdict); err != nil {
		t.Fatal(err)
	}
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	project, _ := read["project"].(map[string]any)
	tiles, _ := fleet["tiles"].(map[string]any)
	if project["gate_state"] != "clean" || status["last_run"] != "gr_main" || tiles["secrets_exposed"] != 0.0 || tiles["blocked"] != 0.0 {
		t.Fatalf("gate_state %v, last_run %v, tiles %v: want clean, gr_main, no secret, none blocked", project["gate_state"], status["last_run"], tiles)
	}
	if do(t, c, http.MethodPut, base+"/api/v1/policies/auth.unauthenticated-route", `{"state":"blocking"}`); lastBody != `{"would_block":[]}` {
		t.Fatalf("the preview counts a pull request's verdict: %s", lastBody)
	}
}

// An upload of a tree a pull request packaged first makes that package current, as a push of it does,
// and the declarations it holds become the project's.
func TestAnUploadOfAPullRequestsTreeMakesItCurrent(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	tree := tarOf(t, map[string]string{"a": "2"})
	pr, err := ingest.IngestChecked(ctx, filepath.Join(reposOf(t, base), "claims-portal.git"), strings.NewReader(tree), nil, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, resources) VALUES ($1, 'claims-portal', $2, $3, 1, 'repo-pr', '{"db": "postgres"}')`, pr.ID, pr.TreeHash, pr.Commit); err != nil {
		t.Fatal(err)
	}
	uploadPulled(t, c, base, "claims-portal", tree)
	if _, pulled, files := pull(t, c, base, "claims-portal"); pulled != pr.ID || files["a"] != "2" || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'") != 1 {
		t.Fatalf("current after uploading the pull request's tree: %q %v, want %s with its declaration audited", pulled, files, pr.ID)
	}
}

// A pull request packaging the head's tree writes its own audit row but never makes anything current,
// so moved_by never names it: the head was made current by its last upload or push (codex on #587).
func TestMovedByNeverNamesAPullRequestsPackaging(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, a := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	aID, _ := packageOf(a)["id"].(string)
	_, b := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), aID, "")
	bID, _ := packageOf(b)["id"].(string)
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, ts, actor_kind, actor_subject, actor_display, action, project_id, detail)
		VALUES ('ev_pr0001', now() + interval '1 minute', 'agent', 'github:pr-bot', 'PR Bot', 'package from github', 'claims-portal', jsonb_build_object('package', $1::text, 'kind', 'repo-pr'))`, bID); err != nil {
		t.Fatal(err)
	}
	if code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, ""); code != 409 || body["moved_by"] != "anonymous" {
		t.Fatalf("a pull request's packaging named as the mover: %d %s", code, lastBody)
	}
}

// A check's upload is reviewed and never current (#1251), from an app's first check on: no stale base
// refuses it, the run names it, and nothing the project shows moves — not pull, the list, latest, main,
// the gate state or the last run — and it is never deployed. A teammate's directory is not made stale.
func TestACheckUploadIsReviewedAndNeverCurrent(t *testing.T) {
	_, _, base, c := withProject(t)
	shows := func() (pulled string, listed int, latest, gate, last any) {
		_, pulled, _ = pull(t, c, base, "claims-portal")
		_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", "")
		packages, _ := list["packages"].([]any)
		_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		project, _ := read["project"].(map[string]any)
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		return pulled, len(packages), project["latest"].(map[string]any)["package"], project["gate_state"], status["last_run"]
	}
	review := func(tree map[string]string, basePkg string) string {
		t.Helper()
		code, body := upload(t, c, base, "claims-portal", tarOf(t, tree), basePkg, "?check=true")
		p := packageOf(body)
		if src, _ := p["source"].(map[string]any); code != 201 || src["kind"] != "check" || src["ref"] != "" {
			t.Fatalf("a check's upload based on %q: %d %s", basePkg, code, lastBody)
		}
		id, _ := p["id"].(string)
		code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+id+`"}`)
		run, _ := runOf(started)["id"].(string)
		if code != 202 || waitVerdict(t, c, base, run)["package"] != id {
			t.Fatalf("a run of the check's package: %d %s", code, lastBody)
		}
		if _, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", ""); list["runs"].([]any)[0].(map[string]any)["check"] != true {
			t.Fatalf("the runs list does not say the check's run reviewed a check: %s", lastBody)
		}
		if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", `{"package":"`+id+`"}`); code != 404 {
			t.Fatalf("a deploy of a check's package: %d %s", code, lastBody)
		}
		return id
	}
	review(map[string]string{"a": "0"}, "pkg_nothing") // a brand-new app's first check
	if pulled, listed, latest, gate, last := shows(); pulled != "" || listed != 0 || latest != nil || gate != "never_reviewed" || last != nil {
		t.Fatalf("after a first check: pulled %q, listed %d, latest %v, gate %v, last run %v", pulled, listed, latest, gate, last)
	}
	_, head := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	headID, _ := packageOf(head)["id"].(string)
	main := func() string {
		out, _ := exec.Command("git", "--git-dir="+reposOf(t, base)+"/claims-portal.git", "rev-parse", "main").Output() //nolint:gosec // test: the server's own repos dir
		return string(out)
	}
	before := main()
	review(map[string]string{"a": "2"}, "pkg_nothing")
	if pulled, listed, latest, gate, last := shows(); pulled != headID || listed != 1 || latest != headID || gate != "never_reviewed" || last != nil || main() != before {
		t.Fatalf("after a check: pulled %q, listed %d, latest %v, gate %v, last run %v, main moved %v; want %s", pulled, listed, latest, gate, last, main() != before, headID)
	}
	if code, _ := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), headID, ""); code != 201 {
		t.Fatalf("a deploy from the head's directory after a check: %d %s", code, lastBody)
	}
	code, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", "")
	waitVerdict(t, c, base, runOf(started)["id"].(string))
	if _, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", ""); code != 202 || list["runs"].([]any)[0].(map[string]any)["check"] != nil {
		t.Fatalf("a run of the current package is listed as a check's: %d %s", code, lastBody)
	}
}

// A check of a tree the project already holds answers that package and changes nothing of it: its
// kind stays, it is not made current again, and the stale base names who made the head, never the checker.
func TestACheckOfAKnownTreeChangesNothing(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, a := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	aID, _ := packageOf(a)["id"].(string)
	_, b := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "2"}), aID, "")
	bID, _ := packageOf(b)["id"].(string)
	if _, err := pool.Exec(ctx, "UPDATE audit_events SET actor_display = 'Ada' WHERE action = 'POST /api/v1/projects/{id}/packages'"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{aID, bID} {
		code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": map[string]string{aID: "1", bID: "2"}[id]}), "", "?check=true")
		if src, _ := packageOf(body)["source"].(map[string]any); code != 201 || packageOf(body)["id"] != id || src["kind"] != "upload" {
			t.Fatalf("a check of %s: %d %s", id, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE detail->>'kind' = 'check'"); n != 2 {
		t.Fatalf("%d check audit rows, want 2", n)
	}
	if code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "3"}), aID, ""); code != 409 || body["head_package"] != bID || body["moved_by"] != "Ada" {
		t.Fatalf("after checks of both trees: %d %s, want %s moved by Ada", code, lastBody, bID)
	}
}

// A check names no version: a label on its upload is refused, and nothing is made.
func TestACheckTakesNoLabel(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	code, body := uploadWith(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1"}, "?check=true")
	if code != 400 || body["error"] != "invalid_label" || count(t, ctx, pool, "SELECT count(*) FROM packages") != 0 {
		t.Fatalf("a labelled check: %d %s", code, lastBody)
	}
}

// An upload of a tree only a check packaged makes it current as that upload, its label the upload's.
func TestAnUploadOfACheckedTreeMakesItCurrent(t *testing.T) {
	_, _, base, c := withProject(t)
	uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}))
	tree := tarOf(t, map[string]string{"a": "2"})
	_, checked := upload(t, c, base, "claims-portal", tree, "", "?check=true")
	code, up := uploadWithPulled(t, c, base, "claims-portal", tree, map[string]string{"Canyon-Package-Label": "v2"})
	if src, _ := packageOf(up)["source"].(map[string]any); code != 201 || packageOf(up)["id"] != packageOf(checked)["id"] || src["kind"] != "upload" || packageOf(up)["label"] != "v2" {
		t.Fatalf("an upload of a checked tree: %d %s", code, lastBody)
	}
	if _, pulled, _ := pull(t, c, base, "claims-portal"); pulled != packageOf(checked)["id"] {
		t.Fatalf("current is %q, want the checked tree's package", pulled)
	}
}

// The fleet reads an app only checked so far as one nothing was uploaded to: never reviewed and created
// since the app was, with no package, label, summary or run taken from the check.
func TestTheFleetReadsACheckOnlyAppAsCreated(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	_, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\ndescription = \"A check's\"\n"}), "", "?check=true")
	id, _ := packageOf(body)["id"].(string)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+id+`"}`)
	waitVerdict(t, c, base, runOf(started)["id"].(string))
	var created string
	if err := pool.QueryRow(ctx, `SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM projects WHERE id = 'claims-portal'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
	row := fleet["apps"].([]any)[0].(map[string]any)
	if want := map[string]any{"package": nil, "stage": "created", "since": created}; row["gate_state"] != "never_reviewed" || !reflect.DeepEqual(row["latest"], want) || row["last_run"] != nil || row["summary"] != "" {
		t.Fatalf("a check-only app on the fleet: %s; want never_reviewed and latest %v", lastBody, want)
	}
}

// A check of a tree declaring a role and a resource declares nothing for the app, and its blocked run
// counts toward no policy: the roles the access read offers, the access file, the policy preview and
// the declare resource rows are what they were before it.
func TestACheckDeclaresNothingForTheApp(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	provisioning(t, "postgres")
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	shows := func() string {
		t.Helper()
		var roles string // the access read's and a share's roles (a demo host routes no access read)
		if err := pool.QueryRow(ctx, "SELECT COALESCE(json_agg(name ORDER BY name), '[]')::text FROM ("+declaredRolesSQL+") d", "claims-portal").Scan(&roles); err != nil {
			t.Fatal(err)
		}
		file, err := (&app{pool: pool}).accessFile(ctx, org{}, "claims-portal", "")
		if err != nil {
			t.Fatal(err)
		}
		do(t, c, http.MethodPut, base+"/api/v1/policies/auth.unauthenticated-route", `{"state":"blocking"}`)
		return fmt.Sprintf("roles %s\nfile %s\npreview %s\ndeclared %d", roles, file, lastBody, count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'"))
	}
	before := shows()
	code, body := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"canyon.toml": "app = \"claims-portal\"\n\n[resources]\ndb = { type = \"postgres\" }\n\n[roles]\napprover = \"Approves claims\"\n"}), "", "?check=true")
	verdict := `{"state": "blocked", "blocking": 1, "findings": {"runs": [{"results": [{"ruleId": "auth.unauthenticated-route", "properties": {"canyon": {"severity": "critical", "blocks": true}}}]}]}}`
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, created_at, ended_at) VALUES ('gr_check', 'claims-portal', $1, 'blocked', now(), now()))
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_check', 'claims-portal', 'verdict', 'verdict', $2)`, packageOf(body)["id"], verdict); err != nil {
		t.Fatal(err)
	}
	if after := shows(); code != 201 || after != before {
		t.Fatalf("a check declared for the app: %d\nbefore %s\nafter  %s", code, before, after)
	}
}

// Deploying the tree a check reviewed makes it the app's as any upload does: its declaration is audited
// once and on the project read, and the packages list, the status and the runs list name it the app's.
func TestDeployingACheckedTreeMakesItsDeclarationsTheApps(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	provisioning(t, "postgres")
	tree := withResources(t, "db = { type = \"postgres\" }\n")
	_, checked := upload(t, c, base, "claims-portal", tree, "", "?check=true")
	id, _ := packageOf(checked)["id"].(string)
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+id+`"}`)
	run, _ := runOf(started)["id"].(string)
	waitVerdict(t, c, base, run)
	upload(t, c, base, "claims-portal", tree, "", "")
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	_, list, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/packages", "")
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	_, runs, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/gate-runs", "")
	listed, row := list["packages"].([]any)[0].(map[string]any), runs["runs"].([]any)[0].(map[string]any)
	resources := string(mustJSON(read["project"].(map[string]any)["resources"]))
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'declare resource'"); n != 1 || resources != `[{"name":"db","state":"declared","type":"postgres"}]` ||
		listed["id"] != id || status["last_run"] != run || row["id"] != run || row["check"] != nil {
		t.Fatalf("after deploying the checked tree: %d declare rows, resources %s, listed %v, last run %v, runs row %v; want %s reviewed by %s", n, resources, listed["id"], status["last_run"], row, id, run)
	}
}

// A review's audit row names its package by id alone; the audit read adds the package's label, so the
// trail names the version as the upload's row does, and a row of an unlabelled package carries none (#967).
func TestTheAuditReadNamesARunsVersionByItsLabel(t *testing.T) {
	_, _, base, c := withProject(t)
	_, up := uploadWith(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), map[string]string{"Canyon-Package-Label": "v1.4"}, "")
	id, _ := packageOf(up)["id"].(string)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/gate-runs", `{"package":"`+id+`"}`); code != 202 {
		t.Fatalf("the review: %d %s", code, lastBody)
	}
	_, page, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?project=claims-portal", "")
	var labels []string
	for _, e := range page["events"].([]any) {
		row := e.(map[string]any)
		if row["action"] == "POST /api/v1/projects/{id}/gate-runs" {
			label, _ := row["detail"].(map[string]any)["label"].(string)
			labels = append(labels, label)
		}
	}
	if len(labels) != 1 || labels[0] != "v1.4" {
		t.Fatalf("the review's row carries the label %q: %s", labels, lastBody)
	}
}

// One package by id (contracts/routes.md, GET /projects/:id/packages/:pkg) is the list's row for any
// package of the project, a check's and a pull request's too, which the list never shows: the
// golden's package_read. A package the project never had — another project's, or none at all — is its
// own 404, unknown_package, apart from the project's not_found, so the CLI's guard (b) never reads an
// older host's catch-all 404 as a checkout sent somewhere else.
func TestPackageByIDAnswersAnyPackageOfTheAppAndUnknownPackageForAnyOther(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ('other-app', 'other-app');
		INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind, source_ref, created_at) VALUES
		 ('pkg_pr01', 'claims-portal', 't', 'c', 1, 'repo-pr', 'refs/pull/7/head', '2026-09-03T08:00:00Z'),
		 ('pkg_else', 'other-app', 't', 'c', 1, 'upload', '', '2026-09-03T08:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	c, at := client(t), base+"/api/v1/projects/claims-portal/packages/"
	if code, got, _ := do(t, c, http.MethodGet, at+"pkg_2b17", ""); code != 200 || !reflect.DeepEqual(got, appPage(t)["package_read"]) {
		t.Errorf("a check's package: %d %s\nwant %s", code, lastBody, mustJSON(appPage(t)["package_read"]))
	}
	if code, got, _ := do(t, c, http.MethodGet, at+"pkg_pr01", ""); code != 200 || got["package"].(map[string]any)["source"].(map[string]any)["kind"] != "repo-pr" {
		t.Errorf("a pull request's package: %d %s", code, lastBody)
	}
	for _, pkg := range []string{"pkg_else", "pkg_nope", "v1.4"} { // another app's, none, and a label, which names no package here
		if code, _, _ := do(t, c, http.MethodGet, at+pkg, ""); code != 404 || lastBody != `{"error":"unknown_package","message":"no such package of this project"}` {
			t.Errorf("%s: %d %s, want unknown_package", pkg, code, lastBody)
		}
	}
	if code, got, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/nope/packages/pkg_2b17", ""); code != 404 || got["error"] != "not_found" {
		t.Errorf("an unknown app: %d %s, want its not_found", code, lastBody)
	}
}
