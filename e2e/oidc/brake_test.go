//go:build oidc

package oidc

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"
)

// TestBrakeHoldsProductionUntilLifted is J13, the brake's server and CLI half (design/OPERATIONS.md §3.10), on the
// team setup, where a responder signs in. Production is taken offline while a preview deploy is in flight. A
// responder holds it from the CLI, and while it is held a promotion, an approval of production and a delete each
// answer 409 held, and a lift from the CLI 403. The responder lifts it in the dashboard, starting the held
// version again, which runs the same digest, and the trail has stop app and lift hold. The dashboard's lift
// that leaves production empty is e2e/ui's hold.spec.ts.
func TestBrakeHoldsProductionUntilLifted(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	app, responder := "brake-"+suffix(), "rita@e2e.test" // rita: in CANYON_OIDC_ALLOWED_DOMAINS, on no app's team
	preferences := host + "/api/v1/preferences"
	browser := signIn(t, admin)
	onboard(t, browser)
	send(t, browser, http.MethodPut, preferences, map[string]any{"responder_emails": []string{responder}}, http.StatusOK)
	t.Cleanup(func() {
		send(t, browser, http.MethodPut, preferences, map[string]any{"responder_emails": []string{}, "prod_approval": false}, http.StatusOK)
	})

	home, source := t.TempDir(), appSource(t, app)
	send(t, browser, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated)
	login(t, command(t, home, source, "login", host), browser)
	for _, args := range [][]string{{"deploy"}, {"deploy", "--env", "prod", "--yes"}} {
		if out, code := run(t, command(t, home, source, args...)); code != 0 {
			t.Fatalf("canyon %v exited %d:\n%s", args, code, out)
		}
	}

	// Production taken offline while a newer version deploys to preview.
	first := newestRun(t, browser, app)
	server := filepath.Join(source, "server.js")
	js, err := os.ReadFile(server) //nolint:gosec // appSource's
	must(t, err)
	must(t, os.WriteFile(server, append(js, "// v2\n"...), 0o600)) //nolint:gosec // appSource's, under t.TempDir
	deploying := command(t, home, source, "deploy")
	var deployed bytes.Buffer
	deploying.Stdout, deploying.Stderr = &deployed, &deployed
	must(t, deploying.Start())
	poll(t, 2*time.Minute, "the preview deploy's run to start", func() bool { return newestRun(t, browser, app) != first })
	if out, code := run(t, command(t, home, source, "undeploy", "--env", "prod", "--yes")); code != 0 {
		t.Fatalf("undeploy --env prod during a preview deploy exited %d:\n%s", code, out)
	}
	if _, running := brakeStatus(t, browser, app).Envs["prod"]; running {
		t.Error("production still runs after the undeploy")
	}
	if err := deploying.Wait(); err != nil {
		t.Fatalf("the preview deploy: %v\n%s", err, deployed.String())
	}
	if out, code := run(t, command(t, home, source, "deploy", "--env", "prod", "--yes")); code != 0 {
		t.Fatalf("promoting v2 exited %d:\n%s", code, out)
	}
	digest := brakeStatus(t, browser, app).Envs["prod"].ImageDigest
	if digest == "" {
		t.Fatal("production runs no image after v2's promotion: Start again would have nothing to compare")
	}

	// A production approval the CLI files while a person approves production, open when it is held.
	send(t, browser, http.MethodPut, preferences, map[string]any{"prod_approval": true}, http.StatusOK)
	out, _ := run(t, command(t, home, source, "deploy", "--env", "prod"))
	filed := regexp.MustCompile(`Code ([A-Z0-9]{4}-[A-Z0-9]{4})`).FindStringSubmatch(out)
	if filed == nil {
		t.Fatalf("the CLI filed no production approval:\n%s", out)
	}

	// The responder holds production from the CLI.
	rita, ritaHome := signIn(t, responder), t.TempDir()
	login(t, command(t, ritaHome, source, "login", host), rita)
	if out, code := run(t, command(t, ritaHome, source, "undeploy", "--app", app, "--env", "prod", "--hold", "--reason", "e2e: J13 holds production", "--yes")); code != 0 {
		t.Fatalf("the responder's hold exited %d:\n%s", code, out)
	}
	held := brakeStatus(t, browser, app)
	if _, running := held.Envs["prod"]; held.Hold == nil || running {
		t.Fatalf("after the hold production is %+v, hold %+v; want stopped and held", held.Envs["prod"], held.Hold)
	}
	for what, c := range map[string]struct {
		method, path string
		body         any
	}{
		"a promotion":          {http.MethodPost, "/api/v1/projects/" + app + "/promotions", map[string]string{"gate_run": held.PromoteRun}},
		"approving production": {http.MethodPost, "/api/v1/promotion-approvals/" + filed[1] + "/approve", map[string]any{}},
		"a delete":             {http.MethodDelete, "/api/v1/projects/" + app + "?confirm=" + app, nil},
	} {
		if code, refused := ask(t, browser, c.method, host+c.path, c.body); code != http.StatusConflict || refused != "held" {
			t.Errorf("%s while production is held: %d %s; want 409 held", what, code, refused)
		}
	}

	// Lifted from the CLI it is refused; in the dashboard, by the responder, it starts the held version again.
	lift := host + "/api/v1/projects/" + app + "/envs/prod/hold?held_at=" + url.QueryEscape(held.Hold.At)
	if code, refused := ask(t, cliSession(t, ritaHome), http.MethodDelete, lift+"&start=true", nil); code != http.StatusForbidden || refused != "dashboard_required" {
		t.Errorf("a lift from the CLI: %d %s; want 403 dashboard_required", code, refused)
	}
	if code, refused := ask(t, rita, http.MethodDelete, lift+"&start=true", nil); code != http.StatusAccepted {
		t.Fatalf("the responder's lift, starting it again: %d %s; want 202", code, refused)
	}
	poll(t, 5*time.Minute, "production to run the held image again", func() bool {
		return brakeStatus(t, browser, app).Envs["prod"].ImageDigest == digest
	})
	poll(t, time.Minute, "stop app and lift hold on the trail", func() bool {
		var trail struct{ Events []struct{ Action string } }
		get(t, browser, host+"/api/v1/audit?project="+app+"&limit=200", &trail)
		has := func(action string) bool {
			return slices.ContainsFunc(trail.Events, func(e struct{ Action string }) bool { return e.Action == action })
		}
		return has("stop app") && has("lift hold")
	})
}

// brakeRead is the status read's fields J13 reads (contracts/routes.md, the status row).
type brakeRead struct {
	Envs map[string]struct {
		ImageDigest string `json:"image_digest"`
	}
	PromoteRun string `json:"promote_run"`
	Hold       *struct{ At string }
}

func brakeStatus(t *testing.T, client *http.Client, app string) brakeRead {
	t.Helper()
	var s brakeRead
	if code := get(t, client, host+"/api/v1/projects/"+app+"/status", &s); code != http.StatusOK {
		t.Fatalf("%s's status read: %d", app, code)
	}
	return s
}

// newestRun is the app's newest run's id.
func newestRun(t *testing.T, client *http.Client, app string) string {
	t.Helper()
	var runs struct{ Runs []struct{ ID string } }
	if code := get(t, client, host+"/api/v1/projects/"+app+"/gate-runs?limit=1", &runs); code != http.StatusOK || len(runs.Runs) == 0 {
		t.Fatalf("%s's runs: %d, %d of them", app, code, len(runs.Runs))
	}
	return runs.Runs[0].ID
}

// ask is one request, its body JSON when in is not nil, answering the status and the error code it refused with.
func ask(t *testing.T, client *http.Client, method, u string, in any) (int, string) {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		must(t, err)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, u, body) //nolint:noctx // the client's timeout bounds it
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	var refused struct{ Error string }
	_ = json.NewDecoder(res.Body).Decode(&refused) // no body, or no error, is no code
	return res.StatusCode, refused.Error
}

// cliSession is a client carrying the session canyon login kept under home, as the CLI sends it: the CLI's, not a browser's.
func cliSession(t *testing.T, home string) *http.Client {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "canyon", "config.json")) //nolint:gosec // the CLI's config under this test's home
	must(t, err)
	var c struct{ Session string }
	must(t, json.Unmarshal(raw, &c))
	if c.Session == "" {
		t.Fatalf("%s/canyon/config.json holds no session", home)
	}
	return &http.Client{Timeout: time.Minute, Transport: cookie{"canyon_session=" + c.Session}}
}

// cookie sends one Cookie header with every request: the CLI's session on http://localhost.
type cookie struct{ value string }

func (c cookie) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Cookie", c.value)
	return http.DefaultTransport.RoundTrip(r)
}

// poll reads until done or within passes.
func poll(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	for until := time.Now().Add(within); !done(); time.Sleep(3 * time.Second) {
		if time.Now().After(until) {
			t.Fatalf("waited %s for %s", within, what)
		}
	}
}
