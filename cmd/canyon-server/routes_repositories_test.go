package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mainSHA, releaseSHA = "8f14e45fceea167a5a36dedd4bea2543a1b2c3d4", "c9f0f895fb98ab9159f51fd0297e236d1234abcd"

// fakeGitHub is GitHub as the repository routes ask it: installation 42 sees acme/platform (81723) and
// acme/claims-portal (555), platform's main is mainSHA, claims-portal's trunk and release/2 are mainSHA and
// releaseSHA, its tarball at a commit is one file, and the GitHub account carol-gh can read both repositories,
// nobody-gh neither. It records each request's method and path.
func fakeGitHub(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	asked := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":"fake-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"repositories":[` +
			`{"id":81723,"full_name":"acme/platform","name":"platform","default_branch":"main","owner":{"login":"acme"}},` +
			`{"id":555,"full_name":"acme/claims-portal","name":"claims-portal","default_branch":"trunk","owner":{"login":"acme"}}]}`))
	})
	mux.HandleFunc("GET /repos/acme/platform/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sha":"` + mainSHA + `"}`))
	})
	mux.HandleFunc("GET /repos/acme/{repo}/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) { // carol-gh reads, nobody-gh doesn't, any other login is unknown
		permission := map[string]string{"carol-gh": "read", "nobody-gh": "none"}[r.PathValue("login")]
		if permission == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"permission":"` + permission + `"}`))
	})
	mux.HandleFunc("GET /repos/acme/claims-portal/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) { // claims-portal's trunk and release/2 (TestPackagesFromRepository)
		sha := map[string]string{"trunk": mainSHA, "release/2": releaseSHA}[r.PathValue("ref")]
		if sha == "" {
			http.Error(w, `{"message":"No commit found"}`, http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(`{"sha":"` + sha + `"}`))
	})
	mux.HandleFunc("GET /repos/acme/claims-portal/tarball/{sha}", func(w http.ResponseWriter, r *http.Request) { // one file, its text the commit's
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		root, text := "acme-claims-portal-"+r.PathValue("sha")[:7]+"/", r.PathValue("sha")+"\n"
		_ = tw.WriteHeader(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755})
		_ = tw.WriteHeader(&tar.Header{Name: root + "index.js", Mode: 0o644, Size: int64(len(text))})
		_, _ = tw.Write([]byte(text))
		_, _ = tw.Close(), gz.Close()
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	testGitHubAPI = srv.URL
	t.Cleanup(func() { testGitHubAPI = "" })
	return &asked
}

// installedApp registers acme's App, 9001, sealed under the server's vault, and installs it as 42.
func installedApp(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	version, pemKey, sealedPEM, err := keys.Seal("github", "app", "private_key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	_, hookKey, hook, err := keys.Seal("github", "app", "webhook_secret", []byte("whsec"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO github_app (id, app_id, slug, client_id, key_ref, sealed_pem_key, sealed_pem, sealed_hook_key, sealed_hook_secret, account) VALUES (NULL, 9001, 'canyon-acme', 'Iv1', $1, $2, $3, $4, $5, 'acme')`,
		`INSERT INTO github_installations (installation_id, account, app_id) VALUES (42, 'acme', 9001)`,
	} {
		args := []any{version, pemKey, sealedPEM, hookKey, hook}
		if !strings.Contains(q, "$1") {
			args = nil
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// An app links to a repository by GitHub's repository id and a folder (#1917): the admin sees what the
// Apps see, makes an app of a folder, which is reviewed at the default branch's head with no owner, and
// links an existing app; one app per repository and folder, one link per app.
func TestLinkARepositoryByIdAndFolder(t *testing.T) {
	asked := fakeGitHub(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	c := client(t)
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/github/repositories", ""); code != 200 || lastBody != `{"repositories":[`+
		`{"repo_id":555,"full_name":"acme/claims-portal","org":"acme","default_branch":"trunk","apps":[]},`+
		`{"repo_id":81723,"full_name":"acme/platform","org":"acme","default_branch":"main","apps":[]}],"unreachable":[]}` {
		t.Fatalf("the repositories: %d %s", code, lastBody)
	}
	code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/github/repositories/81723/import", `{"folder":"apps/billing"}`)
	p, _ := body["project"].(map[string]any)
	billing, _ := p["id"].(string)
	if code != 201 || p["name"] != "billing" || len(p["owners"].([]any)) != 0 || billing == "" {
		t.Fatalf("import: %d %s", code, lastBody)
	}
	if !strings.Contains(strings.Join(*asked, ","), "GET /repos/acme/platform/commits/main") {
		t.Errorf("the default branch's head was not read: %q", *asked)
	}
	if _, err := dbos.RetrieveWorkflow[string](durable, "import/"+billing+"/"+mainSHA); err != nil {
		t.Errorf("the import's review was not started: %v", err)
	}
	_ = dbos.CancelWorkflow(durable, "import/"+billing+"/"+mainSHA) // the GitHub check is github-app's to run: this server registers no courier
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'import repository' AND project_id = '"+billing+"' AND detail->>'folder' = 'apps/billing' AND detail->>'full_name' = 'acme/platform'"); n != 1 {
		t.Errorf("import audit rows: %d", n)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+"/api/v1/github/repositories/81723/import", `{"folder":"apps/billing","name":"Billing again"}`); code != 409 || body["error"] != "linked" || body["app"] != billing {
		t.Errorf("a second app on one folder: %d %s", code, lastBody)
	}
	for body, want := range map[string]string{`{"folder":"../x"}`: "invalid_folder", `{"folder":"apps//x"}`: "invalid_folder", `{"folder":"/apps"}`: "invalid_folder", `{"name":""}`: "invalid_name"} {
		if code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/github/repositories/81723/import", body); code != 400 || out["error"] != want {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if code, out, _ := do(t, c, http.MethodPost, base+"/api/v1/github/repositories/999/import", `{}`); code != 404 || out["error"] != "not_found" {
		t.Errorf("a repository no App sees: %d %s", code, lastBody)
	}

	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/repository", `{"repo_id":555}`); code != 200 || lastBody != `{"repository":{"repo_id":555,"full_name":"acme/claims-portal","folder":"","state":"linked","keeps":false,"kept_commit":null,"kept_error":""}}` {
		t.Fatalf("link: %d %s", code, lastBody)
	}
	if _, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); fmt.Sprint(read["project"].(map[string]any)["repository"]) != "map[folder: full_name:acme/claims-portal keeps:false kept_commit:<nil> kept_error: repo_id:555 state:linked]" {
		t.Errorf("the project read's repository: %s", lastBody)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/repository", `{"repo_id":81723,"folder":"apps/claims"}`); code != 409 || out["error"] != "app_linked" {
		t.Errorf("a second link for one app: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"quote-engine"}`)
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/quote-engine/repository", `{"repo_id":555,"folder":""}`); code != 409 || out["error"] != "linked" || out["app"] != "claims-portal" {
		t.Errorf("a second app on one repository's root: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/repository", ""); code != 204 {
		t.Fatalf("unlink: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/repository", ""); code != 404 || out["error"] != "not_linked" {
		t.Errorf("unlink again: %d %s", code, lastBody)
	}
	if _, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); read["project"].(map[string]any)["repository"] != nil || !strings.Contains(lastBody, `"repository":null`) {
		t.Errorf("an unlinked app's read: %s", lastBody)
	}
	// while an org keeps code, an app no repository feeds reads pending, and its upload starts the keep from main's last commit (#1926)
	if _, err := pool.Exec(ctx, "UPDATE github_app SET keeps_code = true"); err != nil {
		t.Fatal(err)
	}
	if do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); !strings.Contains(lastBody, `"repository":{"repo_id":null,"full_name":"","folder":"","state":"pending","keeps":true,"kept_commit":null,"kept_error":""}`) {
		t.Errorf("an app Canyon is to keep: %s", lastBody)
	}
	keeps := func() []dbos.WorkflowStatus {
		t.Helper()
		started, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDPrefix("keep/claims-portal/"))
		if err != nil {
			t.Fatal(err)
		}
		return started
	}
	code, up := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"index.js": "1\n"}))
	started := keeps()
	if code != 201 || len(started) != 1 || !strings.HasPrefix(started[0].ID, "keep/claims-portal/"+packageOf(up)["id"].(string)+"/") {
		t.Fatalf("the upload's keep: %d %s %v", code, lastBody, started)
	}
	_ = dbos.CancelWorkflow(durable, started[0].ID)                                   // the keep is github-app's to run: this server registers no courier
	for _, files := range []map[string]string{{"x.js": "1\n"}, {"index.js": "1\n"}} { // a new tree, and the version's own
		if code, _ := upload(t, c, base, "claims-portal", tarOf(t, files), packageOf(up)["id"].(string), "?check=true"); code != 201 || len(keeps()) != 1 {
			t.Errorf("a check's upload is never kept: %d %s", code, lastBody)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE packages SET source_kind = 'repo' WHERE id = $1", packageOf(up)["id"]); err != nil { // as if a push brought it
		t.Fatal(err)
	}
	if code, _ := uploadPulled(t, c, base, "claims-portal", tarOf(t, map[string]string{"index.js": "1\n"})); code != 201 || len(keeps()) != 1 {
		t.Errorf("a pushed version uploaded again is GitHub's already, never kept: %d %s", code, lastBody)
	}
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/nothing/repository", `{"repo_id":555}`); code != 404 || out["error"] != "not_found" {
		t.Errorf("an unknown app: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action IN ('link repository', 'unlink repository') AND project_id = 'claims-portal'"); n != 2 {
		t.Errorf("link and unlink audit rows: %d", n)
	}
	// a deleted app takes its link with it: its repository and folder are free again (#1917)
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/"+billing+"?confirm="+billing, ""); code != 204 {
		t.Fatalf("delete the imported app: %d %s", code, lastBody)
	}
	code, body, _ = do(t, c, http.MethodPost, base+"/api/v1/github/repositories/81723/import", `{"folder":"apps/billing"}`)
	if again, _ := body["project"].(map[string]any)["id"].(string); code != 201 || again == billing {
		t.Fatalf("import the deleted app's folder again: %d %s", code, lastBody)
	} else {
		_ = dbos.CancelWorkflow(durable, "import/"+again+"/"+mainSHA)
	}
}

// An app linked to a repository holds its code: an admin adopts it, and nobody claims it (#1917,
// design/PERMISSIONS.md).
func TestALinkedAppIsAdoptedNeverClaimed(t *testing.T) {
	fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal'), ('quote-engine', 'quote-engine')",
		"INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name) VALUES ('claims-portal', 555, '', 42, 'acme/claims-portal')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if code, out, _ := do(t, member, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Carol"}`); code != 403 || out["error"] != "owner_required" {
		t.Errorf("a member claiming a linked app: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, member, http.MethodPut, base+"/api/v1/projects/quote-engine/owner", `{"display":"Carol"}`); code != 200 {
		t.Errorf("a member claiming an unlinked app no grant owns: %d %s", code, lastBody)
	}
	// unlinked since, an app holding a package from GitHub is still its repository's code (#1917's security re-check)
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('was-linked', 'was-linked')",
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_aa11', 'was-linked', 't', 'c', 1, 'repo')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if code, out, _ := do(t, member, http.MethodPut, base+"/api/v1/projects/was-linked/owner", `{"display":"Carol"}`); code != 403 || out["error"] != "owner_required" {
		t.Errorf("a member claiming an unlinked app holding GitHub's code: %d %s", code, lastBody)
	}
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true)
	if code, _, _ := do(t, admin, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana"}`); code != 200 {
		t.Errorf("an admin adopting a linked app: %d %s", code, lastBody)
	}
}

// A member is refused linking before anything is read of GitHub, after an unknown app's 404.
func TestAMemberCannotLinkARepository(t *testing.T) {
	asked := fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal')"); err != nil {
		t.Fatal(err)
	}
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	for _, w := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/github/repositories", ""},
		{http.MethodPost, "/api/v1/github/repositories/555/import", `{}`},
		{http.MethodPut, "/api/v1/github/orgs/acme", `{"imports_new":true}`},
	} {
		if code, _, _ := do(t, member, w.method, base+w.path, w.body); code != 403 || lastBody != adminRequired {
			t.Errorf("%s %s as a member: %d %s", w.method, w.path, code, lastBody)
		}
	}
	for _, w := range []struct{ method, path, body string }{ // linking is a contributor's (TestAContributorLinksARepository)
		{http.MethodPut, "/api/v1/projects/claims-portal/repository", `{"repo_id":555}`},
		{http.MethodDelete, "/api/v1/projects/claims-portal/repository", ""},
	} {
		if code, out, _ := do(t, member, w.method, base+w.path, w.body); code != 403 || out["error"] != "team_required" {
			t.Errorf("%s %s as a member with no grant: %d %s", w.method, w.path, code, lastBody)
		}
	}
	// auto-deploy is an owner's: a contributor is refused before anything changes (#1923)
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display)
		VALUES ('claims-portal', 'email', 'carol@meridian.example', 'contributor', 'sub-carol', 'Carol', 'person', 'sub-dana', 'Dana')`); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := do(t, member, http.MethodPut, base+"/api/v1/projects/claims-portal/auto-deploy", `{"auto_deploy":true}`); code != 403 || out["error"] != "owner_required" || count(t, ctx, pool, "SELECT count(*) FROM projects WHERE auto_deploy") != 0 {
		t.Errorf("a contributor turning auto-deploy on: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, member, http.MethodPut, base+"/api/v1/projects/nothing/repository", `{"repo_id":555}`); code != 404 {
		t.Errorf("an unknown app, as a member: %d %s", code, lastBody)
	}
	if len(*asked) != 0 {
		t.Errorf("GitHub was asked %q for a member", *asked)
	}
}

// An owner hands the repository Canyon keeps to developers (#1938): the link stays, Canyon keeps no more,
// once and audited; never back, never by a contributor, and not on an app with no link.
func TestHandingAKeptRepositoryOver(t *testing.T) {
	fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('claims-app', 'Claims app'), ('quote-engine', 'quote-engine')",
		"INSERT INTO project_repositories (project_id, repo_id, folder, installation_id, full_name, keeps, kept_error) VALUES ('claims-app', 555, '', 42, 'acme/claims-app-k33p', true, 'GitHub refused v2: rule')",
		`INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('claims-app', 'email', 'carol@meridian.example', 'contributor', 'sub-carol', 'Carol', 'person', 'sub-dana', 'Dana'),
			('claims-app', 'email', 'olga@meridian.example', 'owner', 'sub-olga', 'Olga', 'person', 'sub-dana', 'Dana'),
			('quote-engine', 'email', 'olga@meridian.example', 'owner', 'sub-olga', 'Olga', 'person', 'sub-dana', 'Dana')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	at := base + "/api/v1/projects/claims-app/repository/keeps"
	contributor := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if code, out, _ := do(t, contributor, http.MethodPut, at, `{"keeps":false}`); code != 403 || out["error"] != "owner_required" {
		t.Errorf("a contributor handing it over: %d %s", code, lastBody)
	}
	owner := signedIn(t, ctx, pool, base, "sub-olga", "olga@meridian.example", false)
	for _, body := range []string{`{"keeps":true}`, `{}`, `{"keeps":false,"x":1}`} {
		if code, out, _ := do(t, owner, http.MethodPut, at, body); code != 400 || out["error"] != "bad_request" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	// a keep under way holds the app's keep lock: the hand-over waits for it, so it lands before or after, never inside
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", githubapp.KeepLock("claims-app")); err != nil {
		t.Fatal(err)
	}
	landed := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, at, strings.NewReader(`{"keeps":false}`))
		resp, err := owner.Do(req)
		if err != nil {
			landed <- 0
			return
		}
		_ = resp.Body.Close()
		landed <- resp.StatusCode
	}()
	select {
	case code := <-landed:
		t.Fatalf("the hand-over landed inside a keep: %d", code)
	case <-time.After(300 * time.Millisecond):
	}
	_, _ = held.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", githubapp.KeepLock("claims-app"))
	held.Release()
	if code := <-landed; code != 200 {
		t.Fatalf("the hand-over after the keep: %d", code)
	}
	for range 2 { // landed once already: the same answer, and no second row
		if code, _, _ := do(t, owner, http.MethodPut, at, `{"keeps":false}`); code != 200 || lastBody != `{"repository":{"repo_id":555,"full_name":"acme/claims-app-k33p","folder":"","state":"linked","keeps":false,"kept_commit":null,"kept_error":""}}` {
			t.Errorf("handed over: %d %s", code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'hand over repository' AND project_id = 'claims-app' AND actor_subject = 'sub-olga' AND detail->>'full_name' = 'acme/claims-app-k33p'"); n != 1 {
		t.Errorf("hand-over audit rows: %d", n)
	}
	if code, out, _ := do(t, owner, http.MethodPut, base+"/api/v1/projects/quote-engine/repository/keeps", `{"keeps":false}`); code != 404 || out["error"] != "not_linked" {
		t.Errorf("an app with no link: %d %s", code, lastBody)
	}
}

// Auto-deploy is the owners' and admins' switch on a linked app: off until turned on, audited when it
// changes, refused on an app with no link, and turned off by an unlink (#1923).
func TestAutoDeploySwitch(t *testing.T) {
	fakeGitHub(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	c := client(t)
	do(t, c, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`)
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/auto-deploy", `{"auto_deploy":true}`); code != 404 || out["error"] != "not_linked" {
		t.Errorf("an unlinked app: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/repository", `{"repo_id":555}`)
	if _, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); read["project"].(map[string]any)["auto_deploy"] != false {
		t.Errorf("off until turned on: %s", lastBody)
	}
	for _, body := range []string{`{}`, `{"auto_deploy":"yes"}`} {
		if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/auto-deploy", body); code != 400 || out["error"] != "bad_request" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	for range 2 { // the second changes nothing, and writes no row
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/auto-deploy", `{"auto_deploy":true}`); code != 200 || lastBody != `{"auto_deploy":true}` {
			t.Fatalf("turn it on: %d %s", code, lastBody)
		}
	}
	if _, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); read["project"].(map[string]any)["auto_deploy"] != true {
		t.Errorf("the read after: %s", lastBody)
	}
	do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal/repository", "")
	if _, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); read["project"].(map[string]any)["auto_deploy"] != false {
		t.Errorf("an unlink turns it off: %s", lastBody)
	}
	var on string
	if err := pool.QueryRow(ctx, "SELECT string_agg(detail->>'auto_deploy', ' ' ORDER BY ts, id) FROM audit_events WHERE action = 'set auto-deploy'").Scan(&on); err != nil || on != "true false" {
		t.Errorf("the audit rows: %q %v", on, err)
	}
}

// Whether an org's new repositories become apps is an admin's to set, row by row (#1923).
func TestGitHubOrgSettings(t *testing.T) {
	fakeGitHub(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	c := client(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/github/orgs/ACME", `{"imports_new":true}`); code != 200 || !strings.Contains(lastBody, `"account":"acme","app_slug":"canyon-acme","keeps_code":false,"imports_new":true`) {
		t.Fatalf("set: %d %s", code, lastBody)
	}
	do(t, c, http.MethodPut, base+"/api/v1/github/orgs/acme", `{"imports_new":true}`) // nothing changes: no row
	if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/github/orgs/nobody", `{"imports_new":true}`); code != 404 || out["error"] != "not_found" {
		t.Errorf("an org with no App: %d %s", code, lastBody)
	}
	for _, body := range []string{`{}`, `{"imports_new":false} garbage`} {
		if code, out, _ := do(t, c, http.MethodPut, base+"/api/v1/github/orgs/acme", body); code != 400 || out["error"] != "bad_request" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set github org' AND detail->>'imports_new' = 'true'"); n != 1 {
		t.Errorf("one audit row for one change: %d", n)
	}
}

// A contributor links their app to a repository by its owner/name (design/PERMISSIONS.md, linking a repository;
// #1917), only where a GitHub account they linked can read it on GitHub: none linked is github_link_required before
// GitHub is asked; one that can't read it, and a repository no App sees, answer alike, not_found; a read-only member
// is refused as below contributor. A contributor unlinks. An admin links without a GitHub account.
func TestAContributorLinksARepository(t *testing.T) {
	asked := fakeGitHub(t)
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	keys, _ := vaultByBase.Load(strings.TrimPrefix(base, "http://"))
	installedApp(t, ctx, pool, keys.(*vault.Vault))
	for _, q := range []string{
		"INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal'), ('quote-engine', 'quote-engine')",
		`INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display) VALUES
			('claims-portal', 'email', 'carol@meridian.example', 'contributor', 'sub-carol', 'Carol', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'pat@meridian.example', 'contributor', 'sub-pat', 'Pat', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'quinn@meridian.example', 'contributor', 'sub-quinn', 'Quinn', 'person', 'sub-dana', 'Dana'),
			('claims-portal', 'email', 'vic@meridian.example', 'viewer', 'sub-vic', 'Vic', 'person', 'sub-dana', 'Dana')`,
		"INSERT INTO github_links (github_id, subject, login) VALUES (7002, 'sub-carol', 'carol-gh'), (7001, 'sub-pat', 'nobody-gh'), (7003, 'sub-vic', 'carol-gh')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	at := base + "/api/v1/projects/claims-portal/repository"
	quinn := signedIn(t, ctx, pool, base, "sub-quinn", "quinn@meridian.example", false)
	if code, out, _ := do(t, quinn, http.MethodPut, at, `{"full_name":"acme/claims-portal"}`); code != 403 || out["error"] != "github_link_required" || len(*asked) != 0 {
		t.Fatalf("a contributor with no GitHub account linked: %d %s, GitHub asked %q", code, lastBody, *asked)
	}
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	if code, out, _ := do(t, vic, http.MethodPut, at, `{"full_name":"acme/claims-portal"}`); code != 403 || out["error"] != "team_required" {
		t.Errorf("a read-only member, whose GitHub account reads the repository: %d %s", code, lastBody)
	}
	pat := signedIn(t, ctx, pool, base, "sub-pat", "pat@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	notSeen := ""
	for who, w := range map[string]struct {
		c    *http.Client
		body string
	}{
		"a contributor whose GitHub account can't read it": {pat, `{"full_name":"acme/claims-portal"}`},
		"a repository no App sees":                         {carol, `{"full_name":"acme/secret-plans"}`},
	} {
		code, out, _ := do(t, w.c, http.MethodPut, at, w.body)
		if code != 404 || out["error"] != "not_found" {
			t.Errorf("%s: %d %s", who, code, lastBody)
		}
		if notSeen != "" && lastBody != notSeen {
			t.Errorf("%s answers %s, unlike the other refusal %s: a private repository's existence leaks", who, lastBody, notSeen)
		}
		notSeen = lastBody
	}
	if count(t, ctx, pool, "SELECT count(*) FROM project_repositories") != 0 {
		t.Fatal("a refused link was written")
	}
	if code, _, _ := do(t, carol, http.MethodPut, at, `{"full_name":"ACME/claims-portal"}`); code != 200 || lastBody != `{"repository":{"repo_id":555,"full_name":"acme/claims-portal","folder":"","state":"linked","keeps":false,"kept_commit":null,"kept_error":""}}` {
		t.Fatalf("a contributor whose GitHub account reads it links by owner/name: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'link repository' AND actor_subject = 'sub-carol' AND detail->>'full_name' = 'acme/claims-portal'"); n != 1 {
		t.Errorf("the link's audit row: %d", n)
	}
	for _, body := range []string{`{}`, `{"repo_id":555,"full_name":"acme/claims-portal"}`} {
		if code, out, _ := do(t, carol, http.MethodPut, base+"/api/v1/projects/claims-portal/repository", body); code != 400 || out["error"] != "bad_request" {
			t.Errorf("%s: %d %s", body, code, lastBody)
		}
	}
	if code, _, _ := do(t, carol, http.MethodDelete, at, ""); code != 204 {
		t.Errorf("a contributor unlinks: %d %s", code, lastBody)
	}
	admin := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", true)
	if code, _, _ := do(t, admin, http.MethodPut, at, `{"repo_id":555}`); code != 200 {
		t.Errorf("an admin links with no GitHub account linked: %d %s", code, lastBody)
	}
}
