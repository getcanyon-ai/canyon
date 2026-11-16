package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// How old the host's vulnerability databases are is an admin's read (contracts/routes.md, the scanner data row;
// #2743): the executor's GET /scandb as it answered; a member is refused it, 403, and an executor from before the
// call answers none, never an error.
func TestTheScannerDataIsAnAdminsRead(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	data := base + "/api/v1/scanners/data"
	if code, body, _ := do(t, dana, http.MethodGet, data, ""); code != 200 || !reflect.DeepEqual(body["databases"], []any{}) {
		t.Fatalf("an executor from before GET /scandb: %d %s", code, lastBody)
	}
	answered := `{"databases":[{"tool":"grype"},{"tool":"trivy","built_at":"2026-11-05T01:00:58Z","stale_at":"2026-11-10T01:00:58Z"}]}`
	scandb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scandb" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(answered))
	}))
	defer scandb.Close()
	saved := pipe.Executor
	pipe.Executor = &executor.Client{Base: scandb.URL}
	defer func() { pipe.Executor = saved }()
	if code, _, _ := do(t, dana, http.MethodGet, data, ""); code != 200 || strings.TrimSpace(lastBody) != answered {
		t.Fatalf("an admin: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, member, http.MethodGet, data, ""); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member: %d %s", code, lastBody)
	}
}

// An admin sets the Opengrep rules repo's credential: sealed, never answered, one audit row that never
// names the token. A member reads that one is set and is refused setting or removing one; a body out of
// shape is 400. A policies apply naming the same repo another way keeps it; one moving the repo removes it.
func TestTheScannerCredential(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	cred, token := base+"/api/v1/scanners/opengrep/credential", "rules-reader-token-1"
	set := `{"repo":"https://github.com/Meridian/Rules.git","username":"x-access-token","token":"` + token + `"}`
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if code, _, _ := do(t, member, method, cred, set); code != 403 || lastBody != adminRequired {
			t.Fatalf("a member's %s: %d %s", method, code, lastBody)
		}
	}
	for _, bad := range []string{`{}`, `{"repo":"http://github.com/m/r","username":"u","token":"t"}`, `{"repo":"https://github.com/m/r","username":"","token":"t"}`,
		`{"repo":"https://github.com/m/r","username":"u","token":"t\n"}`, `{"repo":"https://github.com/m/r","username":"u","token":"t","scope":"all"}`,
		`{"repo":"https://github.com/m/r","username":"u","token":"t"}}`, `{"repo":"https://github.com/m/r","username":"u","token":"t"}]`} {
		if code, body, _ := do(t, dana, http.MethodPut, cred, bad); code != 400 || body["error"] != "invalid_credential" {
			t.Fatalf("%s: %d %s", bad, code, lastBody)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPut, cred, set); code != 204 {
		t.Fatalf("an admin's PUT: %d %s", code, lastBody)
	}
	code, got, _ := do(t, member, http.MethodGet, cred, "")
	c, _ := got["credential"].(map[string]any)
	if code != 200 || c["repo"] != "https://github.com/Meridian/Rules.git" || c["set_at"] == nil || !reflect.DeepEqual(c["set_by"], map[string]any{"kind": "person", "subject": "dana@meridian.example", "display": "dana@meridian.example"}) ||
		len(c) != 3 || strings.Contains(lastBody, token) || strings.Contains(lastBody, "x-access-token") {
		t.Fatalf("a member's GET: %d %s", code, lastBody)
	}
	var sealed []byte
	var detail string
	if err := pool.QueryRow(ctx, "SELECT sealed_value, (SELECT string_agg(detail::text, '') FROM audit_events WHERE action LIKE 'scanner.credential.%') FROM scanner_credentials").Scan(&sealed, &detail); err != nil ||
		strings.Contains(string(sealed), token) || strings.Contains(detail, token) || !strings.Contains(detail, `"username": "x-access-token"`) {
		t.Fatalf("stored %q, audited %s (%v)", sealed, detail, err)
	}
	apply := func(repo string) {
		t.Helper()
		body := `{"state":"advisory","run":true,"settings":{"rules":{"repo":"` + repo + `","commit":"` + strings.Repeat("a", 40) + `"}}}`
		if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/sarif.opengrep?apply=true", body); code != 200 {
			t.Fatalf("apply %s: %d %s", repo, code, lastBody)
		}
	}
	apply("https://GitHub.com:443/Meridian/Rules/")
	if n := count(t, ctx, pool, "SELECT count(*) FROM scanner_credentials"); n != 1 {
		t.Fatal("the same repo named another way removed the credential")
	}
	apply("https://github.com/Meridian/other-rules")
	if code, _, _ := do(t, member, http.MethodGet, cred, ""); code != 200 || lastBody != `{"credential":null}` ||
		count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'scanner.credential.remove' AND detail->>'repo' = 'https://github.com/Meridian/Rules.git'") != 1 {
		t.Fatalf("moving the repo keeps the credential: %s", lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodDelete, cred, ""); code != 204 {
		t.Fatalf("a DELETE with none set: %d %s", code, lastBody)
	}
}
