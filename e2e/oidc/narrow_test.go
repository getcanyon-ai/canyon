//go:build oidc

package oidc

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheAppPageFitsAPhone is the app page at 390px, as a contributor and as a responder, on the team setup (#102): a
// blocked version whose finding's line and path are far wider than a phone, and the page still never scrolls sideways,
// there or at 360px — the snippet scrolls in its own box and the path wraps. The walk is e2e/ui's narrow project, a screenshot of each kept in .work/walk.
func TestTheAppPageFitsAPhone(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	app, contributor, responder := "phone-"+suffix(), "carl@e2e.test", "rita@e2e.test" // rita: in CANYON_OIDC_ALLOWED_DOMAINS, on no app's team
	preferences := host + "/api/v1/preferences"
	browser := signIn(t, admin)
	onboard(t, browser)
	send(t, browser, http.MethodPut, preferences, map[string]any{"responder_emails": []string{responder}}, http.StatusOK)
	t.Cleanup(func() {
		send(t, browser, http.MethodPut, preferences, map[string]any{"responder_emails": []string{}}, http.StatusOK)
	})

	// A search that builds its SQL from the request, on one line far wider than a phone: the review blocks it.
	home, source := t.TempDir(), appSource(t, app)
	wide := strings.Repeat(" ", 300)
	path := filepath.Join(source, "apps", "web", "src", "app", "api", "admin", "policies", "server.js")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(filepath.Join(source, "server.js"), []byte("require(\"./apps/web/src/app/api/admin/policies/server.js\");\n"), 0o600))
	must(t, os.WriteFile(path, []byte(`const express = require("express");
const { DatabaseSync } = require("node:sqlite");
const db = new DatabaseSync(":memory:");
const app = express();
app.get("/claims", (req, res) => {`+wide+`const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all(); res.json(rows);
});
app.listen(process.env.PORT || 3000);
`), 0o600))
	send(t, browser, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated)
	login(t, command(t, home, source, "login", host), browser)
	if out, code := run(t, command(t, home, source, "deploy")); code != 2 {
		t.Fatalf("canyon deploy of the wide search exited %d, not 2 (blocked):\n%s", code, out)
	}
	send(t, browser, http.MethodPut, host+"/api/v1/projects/"+app+"/access/email/"+url.PathEscape(contributor), map[string]any{"level": "contributor", "roles": []string{}}, http.StatusOK)

	walk(t, signIn(t, contributor), "narrow", "contributor", "NARROW_APP="+app)
	walk(t, signIn(t, responder), "narrow", "responder", "NARROW_APP="+app)
}
