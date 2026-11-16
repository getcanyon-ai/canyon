package main

import (
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/gitopstest"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const repoPath = "/api/v1/platform-repo"

// repoBody is a deploy repo's PUT, the token given or not.
func repoBody(url, token, extra string) string {
	b := `{"url":"` + url + `","preview":{"branch":"preview"},"prod":{"branch":"main"}`
	if token != "" {
		b += `,"token":"` + token + `"`
	}
	return b + extra + "}"
}

// The deploy repo is a host setting (design/PERMISSIONS.md, The deploy repo): any member reads it, an admin alone
// writes it, each write refused to a member before anything is read; the first save answers the fetch-only token
// and the repo's own host token, once, both kept on its own row; no answer ever carries the repo's token, and
// every answer is no-store.
func TestPlatformRepoIsAdminOnlyAndNeverAnswersItsToken(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	const url, secret = "https://github.com/meridian/deploy", "repo-token-fixture-9f2a" //nolint:gosec // a test fixture, not a real token

	// None saved is a defined answer, never an error status a browser logs (#2560): 200 null.
	if code, _, _ := do(t, carol, http.MethodGet, base+repoPath, ""); code != 200 || lastBody != "null" {
		t.Fatalf("a member's GET before any save: %d %s", code, lastBody)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, repoPath, repoBody(url, secret, "")}, {http.MethodPost, repoPath + "/check", ""},
		{http.MethodPost, repoPath + "/token", `{"token":"x"}`}, {http.MethodPost, repoPath + "/fetch-token", ""}, {http.MethodPost, repoPath + "/host-token", ""},
	} {
		if code, _, _ := do(t, carol, c.method, base+c.path, c.body); code != 403 || lastBody != adminRequired {
			t.Fatalf("a member's %s %s: %d %s", c.method, c.path, code, lastBody)
		}
		if code, _, _ := do(t, client(t), c.method, base+c.path, c.body); code != 401 {
			t.Fatalf("no session's %s %s: %d %s", c.method, c.path, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT (SELECT count(*) FROM platform_repo) + (SELECT count(*) FROM platform_webhook)"); n != 0 {
		t.Fatalf("a refused call wrote %d row(s)", n)
	}
	for name, c := range map[string]struct{ body, code string }{
		"a user and password in the url":   {repoBody("https://u:p@github.com/meridian/deploy", secret, ""), "invalid_repo_url"},
		"a query in the url":               {repoBody(url+"?ref=main", secret, ""), "invalid_repo_url"},
		"plain http":                       {repoBody("http://github.com/meridian/deploy", secret, ""), "invalid_repo_url"},
		"more than owner/repo on GitHub":   {repoBody(url+"/extra", secret, ""), "invalid_repo_url"},
		"a pull request with no API":       {repoBody(url, secret, `,"api":"none"`), "invalid_mode"},
		"a branch under canyon/":           {strings.Replace(repoBody(url, secret, ""), `"preview"}`, `"canyon/x"}`, 1), "invalid_branch"},
		"a root that climbs out":           {repoBody(url, secret, `,"root":"../etc"`), "invalid_root"},
		"a store that is not a label":      {repoBody(url, secret, `,"store":"Canyon Store"`), "invalid_store"},
		"a CA that is not a certificate":   {repoBody(url, secret, `,"ca":"-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"`), "invalid_ca"},
		"no token on the first save":       {repoBody(url, "", ""), "invalid_token"},
		"a token with a control character": {repoBody(url, `a\nb`, ""), "invalid_token"},
		"a token with a space":             {repoBody(url, "has a space", ""), "invalid_token"},
		"an api_url with a user":           {repoBody(url, secret, `,"api_url":"https://u@api.github.com"`), "invalid_api"},
		"an api outside the three":         {repoBody(url, secret, `,"api":"bitbucket"`), "invalid_api"},
	} {
		if code, body, _ := do(t, dana, http.MethodPut, base+repoPath, c.body); code != 400 || body["error"] != c.code {
			t.Fatalf("%s: %d %s, want 400 %s", name, code, lastBody, c.code)
		}
	}

	// the message names what is taken: tokenRe refuses a space, which "printable ASCII" admits (#2425)
	const tokenShape = "token: 1 to 1024 characters, printable ASCII without spaces"
	if code, body, _ := do(t, dana, http.MethodPut, base+repoPath, repoBody(url, "has a space", "")); code != 400 || body["message"] != tokenShape {
		t.Fatalf("a token with a space: %d %s, want %q", code, lastBody, tokenShape)
	}

	code, saved, resp := do(t, dana, http.MethodPut, base+repoPath, repoBody(url, secret, ""))
	fetch, _ := saved["fetch_token"].(string)
	host, _ := saved["token"].(string)
	if code != 200 || fetch == "" || host == "" || saved["username"] != "x-access-token" || saved["api"] != "github" || saved["store"] != "canyon" || saved["root"] != "apps" {
		t.Fatalf("the first save: %d %s", code, lastBody)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || strings.Contains(lastBody, secret) {
		t.Fatalf("the save's answer is no-store and never the repo's token: %s %s", resp.Header.Get("Cache-Control"), lastBody)
	}
	code, got, resp := do(t, carol, http.MethodGet, base+repoPath, "")
	if code != 200 || got["url"] != url || got["checked_at"] != nil || got["last_commit"] != nil || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("a member's GET: %d %s", code, lastBody)
	}
	for _, never := range []string{secret, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secret)), fetch, host, `"token"`, `"fetch_token"`} {
		if strings.Contains(lastBody, never) {
			t.Fatalf("the GET carries %q: %s", never, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodGet, base+webhookPath, ""); code != 200 || lastBody != "null" {
		t.Fatalf("the host token's row is no webhook: GET %d %s", code, lastBody)
	}
	// A rollback to a release before the deploy repo stays safe (decision 26, amended): no platform_webhook row is
	// made, the table keeps every column it had NOT NULL, and the boot's checks of sealed rows pass on this host.
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_webhook"); n != 0 {
		t.Fatalf("a repo-only host has %d platform_webhook row(s): a server before the deploy repo would read them", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'platform_webhook'
		AND column_name IN ('url', 'key_ref', 'sealed_key', 'sealed_value', 'token_hash') AND is_nullable = 'NO'`); n != 5 {
		t.Fatalf("platform_webhook keeps %d of its five NOT NULL columns", n)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, vaultOf(t, base)); err != nil {
		t.Fatalf("the boot's check of sealed rows on a repo-only host: %v", err)
	}

	// A save again keeps the token and answers neither token; a replace of it is one audit row with no value.
	code, again, _ := do(t, dana, http.MethodPut, base+repoPath, `{"url":"`+url+`","preview":{"branch":"preview"},"prod":{"branch":"main","mode":"push"}}`)
	if code != 200 || again["fetch_token"] != nil || again["token"] != nil {
		t.Fatalf("a second save answers no token: %d %s", code, lastBody)
	}
	// The stored token never follows the address to another host: such a save carries it again.
	for name, body := range map[string]string{
		"another repository host": `{"url":"https://git.other.example/meridian/deploy","preview":{"branch":"preview"},"prod":{"branch":"main"}}`,
		"another pulls API host":  `{"url":"` + url + `","api_url":"https://api.other.example","preview":{"branch":"preview"},"prod":{"branch":"main"}}`,
	} {
		if code, body, _ := do(t, dana, http.MethodPut, base+repoPath, body); code != 400 || body["error"] != "invalid_token" {
			t.Fatalf("%s with the stored token: %d %s", name, code, lastBody)
		}
	}
	if got := count(t, ctx, pool, "SELECT count(*) FROM platform_repo WHERE url = '"+url+"' AND api_url = 'https://api.github.com'"); got != 1 {
		t.Fatal("a refused move changed the repo")
	}
	if code, body, _ := do(t, dana, http.MethodPost, base+repoPath+"/token", `{"token":"has a space"}`); code != 400 || body["message"] != tokenShape {
		t.Fatalf("a replaced token with a space: %d %s, want %q", code, lastBody, tokenShape)
	}
	if code, _, resp := do(t, dana, http.MethodPost, base+repoPath+"/token", `{"token":"repo-token-fixture-new"}`); code != 200 || resp.Header.Get("Cache-Control") != "no-store" || strings.Contains(lastBody, "fixture-new") {
		t.Fatalf("replace the repo token: %d %s", code, lastBody)
	}
	var keyRef int
	var sealedKey, sealedValue []byte
	if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_value FROM platform_repo").Scan(&keyRef, &sealedKey, &sealedValue); err != nil {
		t.Fatal(err)
	}
	if opened, err := vaultOf(t, base).Open(keyRef, "platform-repo", "repo", "token", sealedKey, sealedValue); err != nil || string(opened) != "repo-token-fixture-new" {
		t.Fatalf("the sealed token opens to the replacement: %v", err)
	}
	for _, action := range []string{"set platform repo", "replace platform repo token"} {
		if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = '"+action+"' AND detail::text NOT LIKE '%fixture%' AND detail::text NOT LIKE '%x-access-token%'"); n < 1 {
			t.Fatalf("no %s row free of the token and the user name", action)
		}
	}

	// The fetch-only token opens the access-token route and nothing else; replaced, the old one is dead.
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id) VALUES ('claims-portal');
		INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_cp', 'claims-portal', 'aa', 'bb', 1, 'upload');
		INSERT INTO runs (id, project_id, package_id, state, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_cp', 'claims-portal', 'pkg_cp', 'running', 'person', 'ana', 'Ana')`); err != nil {
		t.Fatal(err)
	}
	addRelease(t, ctx, pool, "rel_cp1", "claims-portal", "gr_cp", "pkg_cp")
	c := client(t)
	if code, _ := fetchToken(t, c, base, "rel_cp1", fetch); code != 200 {
		t.Fatalf("the fetch-only token fetches an access token: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp1/report", `{"status":"removed"}`, reportHeaders(fetch)); code != 401 {
		t.Fatalf("the fetch-only token reports: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp1/report", `{"status":"failed","reason":"x"}`, reportHeaders(host)); code != 200 {
		t.Fatalf("the host token the repo's save made reports: %d %s", code, lastBody)
	}
	code, rotated, _ := do(t, dana, http.MethodPost, base+repoPath+"/fetch-token", "")
	if next, _ := rotated["fetch_token"].(string); code != 200 || next == "" || next == fetch {
		t.Fatalf("rotate the fetch-only token: %d %s", code, lastBody)
	}
	if code, _ := fetchToken(t, c, base, "rel_cp1", fetch); code != 401 {
		t.Fatalf("a replaced fetch-only token still fetches: %d", code)
	}
	// The repo's host token is replaced on its own row: the old one reports nothing from that instant.
	code, replaced, resp := do(t, dana, http.MethodPost, base+repoPath+"/host-token", "")
	next, _ := replaced["token"].(string)
	if code != 200 || next == "" || next == host || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("replace the repo's host token: %d %s", code, lastBody)
	}
	if code, _, _ := doWith(t, c, http.MethodPost, base+"/releases/rel_cp1/report", `{"status":"failed","reason":"x"}`, reportHeaders(host)); code != 401 {
		t.Fatalf("a replaced host token still reports: %d %s", code, lastBody)
	}
	if code, _ := fetchToken(t, c, base, "rel_cp1", next); code != 200 {
		t.Fatalf("the new host token fetches an access token: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'replace platform repo host token' AND detail IS NULL OR action = 'replace platform repo host token' AND detail::text NOT LIKE '%"+next+"%'"); n != 1 {
		t.Fatalf("%d replace platform repo host token row(s) free of the value, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_webhook"); n != 0 {
		t.Fatalf("a replace made %d platform_webhook row(s)", n)
	}

	// A GitLab repo is the same host setting (#2486's review): a member's save is refused before anything is read, a
	// switch of API carries the token again, and a member's read of the saved GitLab repo never carries it.
	const lab, labSecret = "https://gitlab.meridian.example/platform/deploy", "gitlab-token-fixture-4c1d" //nolint:gosec // a test fixture, not a real token
	if code, _, _ := do(t, carol, http.MethodPut, base+repoPath, repoBody(lab, labSecret, `,"api":"gitlab"`)); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's GitLab save: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, dana, http.MethodPut, base+repoPath, `{"url":"`+url+`","api":"none","preview":{"branch":"preview"},"prod":{"branch":"main","mode":"push"}}`); code != 400 || body["error"] != "invalid_token" {
		t.Fatalf("another API with the stored token: %d %s, want 400 invalid_token", code, lastBody)
	}
	if code, saved, _ := do(t, dana, http.MethodPut, base+repoPath, repoBody(lab, labSecret, `,"api":"gitlab"`)); code != 200 || saved["api"] != "gitlab" {
		t.Fatalf("the admin's GitLab save: %d %s", code, lastBody)
	}
	if code, got, _ := do(t, carol, http.MethodGet, base+repoPath, ""); code != 200 || got["api"] != "gitlab" || got["api_url"] != "https://gitlab.meridian.example/api/v4" {
		t.Fatalf("a member's read of the GitLab repo: %d %s", code, lastBody)
	}
	for _, never := range []string{labSecret, secret, `"token"`, `"fetch_token"`} {
		if strings.Contains(lastBody, never) {
			t.Fatalf("a member's read of the GitLab repo carries %q: %s", never, lastBody)
		}
	}
}

// The check of the repo proves what it can — both branches, the templates, a push, the pulls API — answers
// problem codes in D5's words, and runs one at a time; a pass writes checked_at.
func TestCheckTheRepoAnswersItsProblems(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	remote := gitopstest.New(t, filepath.Join("..", "..", "contracts", "testdata", "release", "repo", "templates", ".canyon"))
	pipe.RepoTransport = remote.Client.Transport
	t.Cleanup(func() { pipe.RepoTransport = nil })
	c := client(t) // auth off: the anonymous admin
	if code, _, _ := do(t, c, http.MethodPut, base+repoPath, repoBody(remote.URL, gitopstest.Token, `,"api_url":"`+remote.API+`"`)); code != 200 {
		t.Fatalf("save the repo: %d %s", code, lastBody)
	}
	code, checked, _ := do(t, c, http.MethodPost, base+repoPath+"/check", "")
	if code != 200 || checked["passed"] != true || len(checked["problems"].([]any)) != 0 {
		t.Fatalf("the check: %d %s", code, lastBody)
	}
	if templates := checked["templates"].([]any); len(templates) != 2 || templates[0] != ".canyon/resources/postgres.yaml" {
		t.Fatalf("the templates found: %v", templates)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_repo WHERE checked_at IS NOT NULL"); n != 1 {
		t.Fatal("a passing check wrote no checked_at")
	}
	appOf(t, base).checks <- struct{}{} // one in flight
	if code, body, _ := do(t, c, http.MethodPost, base+repoPath+"/check", ""); code != 429 || body["error"] != "check_in_flight" || body["message"] != checkInFlight {
		t.Fatalf("a second check while one runs: %d %s", code, lastBody)
	}
	<-appOf(t, base).checks
	if code, _, _ := do(t, c, http.MethodPost, base+repoPath+"/token", `{"token":"another-token"}`); code != 200 {
		t.Fatalf("replace the token: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_repo WHERE checked_at IS NULL"); n != 1 {
		t.Fatal("a replaced token kept the last check")
	}
	code, refused, _ := do(t, c, http.MethodPost, base+repoPath+"/check", "")
	problems, _ := refused["problems"].([]any)
	if code != 200 || refused["passed"] != false || len(problems) != 1 || problems[0].(map[string]any)["code"] != "auth_refused" ||
		problems[0].(map[string]any)["message"] != "The repo refused the token. Check it is current and has this repository, and replace it here." {
		t.Fatalf("a refused token: %d %s", code, lastBody)
	}
}

// A GitLab deploy repo saves with production through a merge request and its API on its own host, filled in when
// left out; the save's answer and the read are the contract's (release.schema.json's repo_saved and repo_get);
// another host for the API is refused, and the check lists the open merge requests (#2480).
func TestAGitLabRepoSavesAndIsChecked(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	remote := gitopstest.New(t, filepath.Join("..", "..", "contracts", "testdata", "release", "repo", "templates", ".canyon"))
	pipe.RepoTransport = remote.Client.Transport
	t.Cleanup(func() { pipe.RepoTransport = nil })
	c := client(t) // auth off: the anonymous admin
	elsewhere := strings.Replace(remote.GitLab, "127.0.0.1", "localhost", 1)
	if code, body, _ := do(t, c, http.MethodPut, base+repoPath, repoBody(remote.URL, gitopstest.Token, `,"api":"gitlab","api_url":"`+elsewhere+`"`)); code != 400 || body["error"] != "invalid_api" {
		t.Fatalf("GitLab's API on another host: %d %s, want 400 invalid_api", code, lastBody)
	}
	code, saved, _ := do(t, c, http.MethodPut, base+repoPath, repoBody(remote.URL, gitopstest.Token, `,"api":"gitlab"`))
	if code != 200 || saved["api"] != "gitlab" || saved["api_url"] != remote.GitLab || saved["prod"].(map[string]any)["mode"] != "pull_request" {
		t.Fatalf("a GitLab repo through a merge request: %d %s", code, lastBody)
	}
	conforms(t, "repo_saved", lastBody)
	if code, _, _ := do(t, c, http.MethodGet, base+repoPath, ""); code != 200 {
		t.Fatalf("the read: %d %s", code, lastBody)
	}
	conforms(t, "repo_get", lastBody)
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set platform repo' AND detail->>'api' = 'gitlab'"); n != 1 {
		t.Fatalf("%d set platform repo rows naming gitlab, want 1", n)
	}
	code, checked, _ := do(t, c, http.MethodPost, base+repoPath+"/check", "")
	if code != 200 || checked["passed"] != true {
		t.Fatalf("the check of a GitLab repo: %d %s", code, lastBody)
	}
}

// conforms fails t unless body is release.schema.json's definition name.
func conforms(t *testing.T, name, body string) {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile("../../contracts/schema/release.schema.json#/properties/" + name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err == nil {
		err = schema.Validate(doc)
	}
	if err != nil {
		t.Fatalf("not the contract's %s: %v\n%s", name, err, body)
	}
}
