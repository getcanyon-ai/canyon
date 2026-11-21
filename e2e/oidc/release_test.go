//go:build oidc

package oidc

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestTheReleaseScreensAtEachLevel walks v0.11.0's new screens as each person on the team setup, one screenshot per screen
// and person into the oidc-walk artifact (e2e/ui/release), each asserting what design/PERMISSIONS.md lets that person
// see:
//   - Guardrails › Playbooks: an admin's list, editor, history and import; everyone else the admins-only page;
//   - a custom rule's Playbook picker: an admin's;
//   - the app page's finding whose rule points at a playbook, with its Then step: owner, contributor and admin, never a
//     read-only viewer, and no one without access;
//   - the production approval page, asking the version question and then the put-back question: owner, contributor and
//     admin may decide, a viewer and no one else may not;
//   - Settings › People & sign-in: what each reads there today.
func TestTheReleaseScreensAtEachLevel(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	tag := suffix()
	letters := strings.Map(func(r rune) rune { return 'a' + r%26 }, tag) // a custom rule's slug is letters and hyphens alone
	app, slug, rule := "release-"+tag, "release-walk-"+tag, "release-walk-"+letters
	playbookName, ruleName := "How the release walk builds "+tag, "Every app carries RELEASE-WALK.md "+letters
	browser := signIn(t, admin)
	onboard(t, browser)
	people := map[string]string{"owner": "olive@e2e.test", "contributor": "carl@e2e.test", "viewer": "vera@e2e.test", "none": "nora@e2e.test", "admin": admin}

	// The org's playbook, and a custom rule checked by code that points at it, on in Advisory. This stack has no model: a
	// rule checked by code needs one to translate it at save, and a custom rule can't be turned on from the API without
	// one (409 no_model), though a review checks a rule checked by code with none. So its version and its state are
	// seeded as e2e/ui's dry-run journey seeds a version, the state as the policies row writes one.
	preferences, policy := host+"/api/v1/preferences", host+"/api/v1/policies/custom."+rule+"?apply=true"
	send(t, browser, http.MethodPut, host+"/api/v1/playbooks/"+slug, map[string]string{
		"name": playbookName, "kind": "skill", "when": "Read before you add a route to a release-walk app.",
		"body": "# How the release walk builds\n\nEvery app carries RELEASE-WALK.md at its root, saying who owns it.\n\n```sh\necho owner > RELEASE-WALK.md\n```\n",
	}, http.StatusOK)
	t.Cleanup(func() { send(t, browser, http.MethodDelete, host+"/api/v1/playbooks/"+slug, nil, http.StatusNoContent) })
	sql(t, `INSERT INTO custom_rule_edits (rule, version, name, instructions, check_by, check_expr, check_says, check_facts_version, set_by_kind, set_by_subject, set_by_display, playbook)
		VALUES ('custom.`+rule+`', 1, '`+ruleName+`', 'Every app has RELEASE-WALK.md at its root.', 'code', '''RELEASE-WALK.md'' in files', 'The app has RELEASE-WALK.md at its root.', 3, 'person', 'e2e', 'e2e', '`+slug+`')`)
	sql(t, `INSERT INTO policy_edits (rule, state, names_state, production, grace_days, set_at, effective_at, set_by_kind, set_by_subject, set_by_display, version)
		SELECT 'custom.`+rule+`', 'advisory', true, NULL, 0, now(), now(), 'person', 'e2e', 'e2e', (SELECT COALESCE(MAX(version), 0) + 1 FROM policy_edits)`)
	t.Cleanup(func() {
		send(t, browser, http.MethodPut, preferences, map[string]any{"prod_approval": false}, http.StatusOK)
		send(t, browser, http.MethodPut, policy, map[string]string{"state": "off"}, http.StatusOK)
	})

	// The owner's app, shared, with three versions: v1 then v2 in production, v3 in preview, each reviewed with the
	// rule's finding (no RELEASE-WALK.md), which never blocks.
	owner, home := signIn(t, people["owner"]), t.TempDir()
	send(t, owner, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated)
	for who, level := range map[string]string{"contributor": "contributor", "viewer": "viewer"} {
		send(t, owner, http.MethodPut, host+"/api/v1/projects/"+app+"/access/email/"+url.PathEscape(people[who]), map[string]any{"level": level, "roles": []string{}}, http.StatusOK)
	}
	source := appSource(t, app)
	login(t, command(t, home, source, "login", host), owner)
	deploy := func(args ...string) string {
		t.Helper()
		out, code := run(t, command(t, home, source, args...))
		if code != 0 {
			t.Fatalf("canyon %v exited %d:\n%s", args, code, out)
		}
		return out
	}
	server := filepath.Join(source, "server.js")
	for i, step := range [][]string{{"deploy"}, {"deploy", "--env", "prod", "--yes"}, {"deploy"}, {"deploy", "--env", "prod", "--yes"}, {"deploy"}} {
		if i == 2 || i == 4 { // a new version: one more line
			js, err := os.ReadFile(server) //nolint:gosec // appSource's
			must(t, err)
			must(t, os.WriteFile(server, append(js, []byte("// v"+string(rune('1'+i/2))+"\n")...), 0o600)) //nolint:gosec // appSource's, under t.TempDir
		}
		deploy(step...)
	}
	var status struct {
		Earlier []struct{ Version string } `json:"earlier"`
	}
	if code := get(t, owner, host+"/api/v1/projects/"+app+"/status", &status); code != http.StatusOK || len(status.Earlier) == 0 {
		t.Fatalf("%s's status: %d, earlier %+v; want v1 among production's earlier versions", app, code, status.Earlier)
	}
	first := status.Earlier[len(status.Earlier)-1].Version

	// The version question: the owner's CLI asks for production while a person approves it.
	send(t, browser, http.MethodPut, preferences, map[string]any{"prod_approval": true}, http.StatusOK)
	filed := regexp.MustCompile(`Code ([A-Z0-9]{4}-[A-Z0-9]{4})`)
	asked := func(args ...string) string {
		t.Helper()
		out, _ := run(t, command(t, home, source, args...))
		code := filed.FindStringSubmatch(out)
		if code == nil {
			t.Fatalf("canyon %v filed no production approval:\n%s", args, out)
		}
		return code[1]
	}
	walk := map[string]string{
		"E2E_CANYON_URL": host, "RELEASE_APP": app, "RELEASE_PLAYBOOK": playbookName, "RELEASE_SLUG": slug, "RELEASE_RULE": ruleName,
		"RELEASE_APPROVAL": asked("deploy", "--env", "prod"),
	}
	sessions := map[string]*http.Client{"owner": owner, "admin": browser}
	for _, who := range []string{"owner", "contributor", "viewer", "none", "admin"} {
		if sessions[who] == nil {
			sessions[who] = signIn(t, people[who])
		}
		walkRelease(t, sessions[who], who, "the release screens", walk)
	}

	// The put-back question: a newer request replaces the first, so it is walked apart.
	walk["RELEASE_APPROVAL"] = asked("deploy", "--env", "prod", "--version", first, "--reason", "e2e: the release walk puts v1 back")
	walk["RELEASE_PUT_BACK"] = first
	for _, who := range []string{"owner", "contributor", "viewer", "none", "admin"} {
		walkRelease(t, sessions[who], who, "the put-back question", walk)
	}
}

// walkRelease runs e2e/ui/release's test named what, as who, in who's browser session, with env.
func walkRelease(t *testing.T, browser *http.Client, who, what string, env map[string]string) {
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
	cmd := exec.Command("npx", "playwright", "test", "--project=release", "--grep", what+" as "+who+"$") //nolint:gosec // the harness's own runner
	cmd.Dir = filepath.Join("..", "ui")
	cmd.Env = append(os.Environ(), "RELEASE_COOKIE_NAME="+session.Name, "RELEASE_COOKIE_VALUE="+session.Value)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	t.Logf("playwright, %s as %s:\n%s", what, who, out)
	if err != nil {
		t.Fatalf("%s as %s: %v", what, who, err)
	}
}

// sql runs one statement on the stack's database, for what no request on this stack can make: the compose files and env
// file are make e2e-up OIDC=1's, as the workflow's log step names them.
func sql(t *testing.T, query string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "--env-file", "e2e/.work/fixtures/oidc.env", "-f", "docker-compose.yml", "-f", "e2e/compose.e2e.yml", //nolint:gosec // the stack's own compose files and this test's statement
		"-f", "deploy/docker/team.yml", "-f", "e2e/compose.oidc.yml", "exec", "-T", "postgres", "psql", "-U", "canyon", "-d", "canyon", "-AtX", "-v", "ON_ERROR_STOP=1", "-c", query)
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("psql: %v\n%s", err, out)
	}
}
