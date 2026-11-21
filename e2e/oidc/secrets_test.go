//go:build oidc

package oidc

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The app page's Secrets section at each level (design/specs/mcp.md §6 item 13, §10.1; design/PERMISSIONS.md, Change
// it): an owner makes the app and shares it, a contributor sets a value in the dashboard and never sees it back, an
// admin removes it, and a read-only member and a person with no access see no section and no form. Each person signs
// in through the issuer, and e2e/ui's secrets project walks the page in Chromium with their session, a screenshot of
// each in e2e/ui/.work/walk.
func TestTheSecretsSectionAtEachLevel(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	app := "secrets-" + suffix()
	onboard(t, signIn(t, admin))
	people := map[string]string{"owner": "olive@e2e.test", "contributor": "carl@e2e.test", "viewer": "vera@e2e.test", "none": "nora@e2e.test", "admin": admin}

	// The owner makes the app, and shares it with a contributor and a read-only member.
	owner := signIn(t, people["owner"])
	send(t, owner, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated)
	for who, level := range map[string]string{"contributor": "contributor", "viewer": "viewer"} {
		send(t, owner, http.MethodPut, host+"/api/v1/projects/"+app+"/access/email/"+url.PathEscape(people[who]), map[string]any{"level": level, "roles": []string{}}, http.StatusOK)
	}
	// Each walks the page in turn: the contributor sets JOURNEY_TOKEN, the admin removes it.
	for _, who := range []string{"owner", "contributor", "viewer", "none", "admin"} {
		browser := owner
		if who != "owner" {
			browser = signIn(t, people[who])
		}
		walk(t, browser, "secrets", who, "SECRETS_APP="+app)
		if who == "contributor" {
			var read struct {
				Secrets []struct{ Name, Env, Where string } `json:"secrets"`
			}
			if code := get(t, owner, host+"/api/v1/projects/"+app+"/secrets", &read); code != http.StatusOK || len(read.Secrets) != 1 || read.Secrets[0].Name != "JOURNEY_TOKEN" || read.Secrets[0].Env != "preview" || read.Secrets[0].Where != "runtime" {
				t.Fatalf("the secrets after the contributor's set: %d %+v", code, read.Secrets)
			}
		}
	}
	var after struct {
		Secrets []struct{ Name string } `json:"secrets"`
	}
	if code := get(t, owner, host+"/api/v1/projects/"+app+"/secrets", &after); code != http.StatusOK || len(after.Secrets) != 0 {
		t.Errorf("the secrets after the admin removed JOURNEY_TOKEN: %d %+v", code, after.Secrets)
	}
}

// walkSecrets runs e2e/ui's secrets project in Chromium as who, their session cookie handed over.
// walk runs the Playwright project's test for who in that person's browser session: <PROJECT>_COOKIE_NAME and _VALUE
// carry the session, and env the project's own settings.
func walk(t *testing.T, browser *http.Client, project, who string, env ...string) {
	t.Helper()
	u, err := url.Parse(host)
	must(t, err)
	var session *http.Cookie
	for _, c := range browser.Jar.Cookies(u) {
		if strings.HasSuffix(c.Name, "canyon_session") {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("%s's browser holds no session", who)
	}
	prefix := strings.ToUpper(project)
	cmd := exec.Command("npx", "playwright", "test", "--project="+project, "--grep", "as "+who+"$") //nolint:gosec // the harness's own runner
	cmd.Dir = filepath.Join("..", "ui")
	cmd.Env = append(append(os.Environ(), "E2E_CANYON_URL="+host, prefix+"_COOKIE_NAME="+session.Name, prefix+"_COOKIE_VALUE="+session.Value), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("playwright, %s as %s:\n%s", project, who, out)
	if err != nil {
		t.Fatalf("%s as %s: %v", project, who, err)
	}
}
