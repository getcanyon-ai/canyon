package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// undeployHost answers the undeploy route with one status and body, and keeps every path it was
// sent, so "it sent nothing" is provable.
type undeployHost struct {
	code   int
	body   string
	sent   []string
	bodies []string // what each request carried: a hold's reason
	raw    string   // stdout whole, its first line included
}

// undeploying runs the verb in a directory holding a canyon.toml, against h, with input typed at a
// terminal or piped, as tty says.
func undeploying(t *testing.T, h *undeployHost, input string, tty bool, args ...string) (out, errs string, code int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/claims-portal" { // the app's own read, after a 404: this host has the app
			_, _ = w.Write([]byte(`{"project":{"id":"claims-portal"}}`))
			return
		}
		h.sent = append(h.sent, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // this process's own request body
		h.bodies = append(h.bodies, string(body))
		w.WriteHeader(h.code)
		_, _ = w.Write([]byte(h.body))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	t.Chdir(dir)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	out, errs, code = answer(t, input, env, tty, append([]string{"undeploy"}, args...)...)
	h.raw = strings.ReplaceAll(out, strings.TrimPrefix(server.URL, "http://"), "localhost:7777")
	return pastHeader(out), errs, code
}

const kept = "Its data and secrets are kept.\n"

func TestUndeployingPreviewAsksNothing(t *testing.T) {
	h := &undeployHost{code: http.StatusNoContent}
	out, errs, code := undeploying(t, h, "", false, "--env", "preview")
	if code != exitOK || errs != "" || len(h.sent) != 1 || h.sent[0] != "DELETE /api/v1/projects/claims-portal/envs/preview" {
		t.Errorf("preview exited %d, stderr %q, sent %q", code, errs, h.sent)
	}
	if want := "\n  ✓ Took claims-portal offline in preview. " + kept; out != want {
		t.Errorf("preview printed %q, want %q", out, want)
	}
}

func TestUndeployingProdAsksAsPromoteDoes(t *testing.T) {
	enter := &undeployHost{code: http.StatusNoContent}
	out, errs, code := undeploying(t, enter, "\n", true, "--env", "prod")
	if code != exitError || len(enter.sent) != 0 || out != "\n  Nothing was taken offline.\n" {
		t.Errorf("Enter exited %d, sent %q, printed %q", code, enter.sent, out)
	}
	if errs != "\n  Take claims-portal offline in production? [y/N] " {
		t.Errorf("the question this build asks is %q", errs)
	}
	typed := &undeployHost{code: http.StatusNoContent}
	out, _, code = undeploying(t, typed, "y\n", true, "--env", "prod")
	if want := "\n  ✓ Took claims-portal offline in production. " + kept; code != exitOK || out != want ||
		len(typed.sent) != 1 || typed.sent[0] != "DELETE /api/v1/projects/claims-portal/envs/prod" {
		t.Errorf("a yes exited %d, sent %q, printed %q", code, typed.sent, out)
	}
	piped := &undeployHost{code: http.StatusNoContent} // a pipe holding a yes nobody typed
	if _, errs, code := undeploying(t, piped, "y\n", false, "--env", "prod"); code != exitError || len(piped.sent) != 0 ||
		!strings.Contains(errs, "--yes") || strings.Contains(errs, "[y/N]") {
		t.Errorf("a machine undeployed prod, or was refused without the flag named: exit %d, stderr %q, sent %q", code, errs, piped.sent)
	}
	flagged := &undeployHost{code: http.StatusNoContent}
	if _, errs, code := undeploying(t, flagged, "", false, "--env", "prod", "--yes"); code != exitOK || errs != "" || len(flagged.sent) != 1 {
		t.Errorf("--yes on the same machine: exit %d, stderr %q, sent %q", code, errs, flagged.sent)
	}
}

func TestUndeployNamesItsEnvAndSendsNoOther(t *testing.T) {
	for _, args := range [][]string{{}, {"--env", "staging"}, {"--env", "prod", "extra"}} {
		h := &undeployHost{code: http.StatusNoContent}
		if _, errs, code := undeploying(t, h, "", false, args...); code != exitError || len(h.sent) != 0 || !strings.Contains(errs, "undeploy") {
			t.Errorf("undeploy %q exited %d, sent %q, stderr %q", args, code, h.sent, errs)
		}
	}
}

// Every refusal the route has is one line in the person's words, never the envelope.
func TestEveryRefusalIsALineAndAnExitCode(t *testing.T) {
	for _, c := range []struct {
		env, body string
		status    int
		line      string
		exit      int
	}{
		{"preview", `{"error":"not_found","message":"not found"}`, http.StatusNotFound,
			"Nothing is running in preview.", exitError},
		{"prod", `{"error":"person_required","message":"only a person may"}`, http.StatusForbidden,
			"Only a person can take claims-portal offline in production. Sign in as yourself with canyon login, then run this again.", exitBlocked},
		{"prod", `{"error":"run_in_progress","message":"busy","run":{"id":"gr_9a1c","state":"running"}}`, http.StatusConflict,
			"A run of claims-portal is under way (gr_9a1c). Let it finish, then run this again.", exitError},
		{"prod", `{"error":"internal","message":"internal"}`, http.StatusInternalServerError,
			"The host could not stop claims-portal in production. Run this again: it picks up where it stopped.", exitError},
	} {
		h := &undeployHost{code: c.status, body: c.body}
		out, errs, code := undeploying(t, h, "", false, "--env", c.env, "--yes")
		if code != c.exit || out != "" || errs != "\n  ✗ "+c.line+"\n" {
			t.Errorf("a %d exited %d, stdout %q, stderr %q; want %q", c.status, code, out, errs, c.line)
		}
	}
}

// Undeploying an environment the org's platform runs sends a remove release and answers 202 at once:
// exit 0, and the environment is said as the screens say it, removing · waiting on your platform
// (design/HAND-OFF.md §7).
func TestUndeployingWhatTheOrgRunsWaitsOnItsPlatform(t *testing.T) {
	h := &undeployHost{code: http.StatusAccepted, body: `{"release":"rel_9b31"}`}
	out, errs, code := undeploying(t, h, "", false, "--env", "preview")
	if want := "\n  · claims-portal in preview: removing · waiting on your platform. " + kept; code != exitOK || errs != "" || out != want {
		t.Errorf("a 202 exited %d, stdout %q, stderr %q; want %q", code, out, errs, want)
	}
}

// An undeploy that meets a run already switching the environment answers 202 {stopping}: exit 0, and
// the line says the run stops it once it lands (contracts/routes.md, The brake).
func TestUndeployingWhatARunIsSwitchingSaysItIsStopping(t *testing.T) {
	for env, line := range map[string]string{
		"prod":    "Production is stopping: a promotion was already switching it, and Canyon stops it the moment it lands.",
		"preview": "Preview is stopping: a deploy was already switching it, and Canyon stops it the moment it lands.",
	} {
		h := &undeployHost{code: http.StatusAccepted, body: `{"stopping":{"run":{"id":"gr_9a1c","state":"running"}}}`}
		out, errs, code := undeploying(t, h, "", false, "--env", env, "--yes")
		if want := "\n  · " + line + "\n"; code != exitOK || errs != "" || out != want {
			t.Errorf("a 202 stopping on %s exited %d, stdout %q, stderr %q; want %q", env, code, out, errs, want)
		}
	}
}

// holding is a hold of claims-portal's production for reason, named by --app, against h.
func holding(t *testing.T, h *undeployHost, input string, tty bool, extra ...string) (out, errs string, code int) {
	t.Helper()
	return undeploying(t, h, input, tty, append([]string{"--app", "claims-portal", "--env", "prod", "--hold", "--reason", incident}, extra...)...)
}

const incident = "Exports claimant SSNs unauthenticated (INC-2231)"

// --hold needs --reason, --reason without --hold is a usage error, and only production is held
// (design/OPERATIONS.md §3.8): each refused before anything is sent.
func TestHoldNeedsReason(t *testing.T) {
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--app", "claims-portal", "--env", "prod", "--hold"}, "--hold needs --reason: " + holdUsage},
		{[]string{"--app", "claims-portal", "--env", "prod", "--hold", "--reason", ""}, "--hold needs --reason: " + holdUsage},
		{[]string{"--app", "claims-portal", "--env", "prod", "--reason", incident}, "--reason goes with --hold: " + holdUsage},
		{[]string{"--env", "prod", "--yes", "--reason", incident}, "--reason goes with --hold: " + holdUsage},
		{[]string{"--app", "claims-portal", "--env", "preview", "--hold", "--reason", incident}, "--hold holds production alone: " + holdUsage},
	} {
		h := &undeployHost{code: http.StatusNoContent}
		if _, errs, code := undeploying(t, h, "", false, c.args...); code != exitError || len(h.sent) != 0 || !strings.Contains(errs, c.says) {
			t.Errorf("undeploy %q exited %d, sent %q, stderr %q; want %q", c.args, code, h.sent, errs, c.says)
		}
	}
}

// --hold holds production alone, so it means --env prod: the form without it holds, as the help says
// (#2258: the test cluster's responder was told undeploy needs --env).
func TestHoldImpliesProd(t *testing.T) {
	h := &undeployHost{code: http.StatusNoContent}
	out, errs, code := undeploying(t, h, "", false, "--app", "claims-portal", "--hold", "--reason", incident, "--yes")
	if code != exitOK || errs != "" || len(h.sent) != 1 || h.sent[0] != "POST /api/v1/projects/claims-portal/envs/prod/hold" || !strings.Contains(out, "✓ Held    prod · stopped\n") {
		t.Errorf("--hold without --env exited %d, sent %q, stderr %q, stdout:\n%s", code, h.sent, errs, out)
	}
}

// A hold says what it does before it asks, asks as taking production offline does, sends the reason,
// and ends on the row and the dashboard that lifts it (design/OPERATIONS.md §3.8).
func TestHeldAfterAskingWithTheReason(t *testing.T) {
	h := &undeployHost{code: http.StatusNoContent}
	out, errs, code := holding(t, h, "y\n", true)
	if code != exitOK || errs != "\n  Stop and hold claims-portal in production? [y/N] " {
		t.Errorf("a hold exited %d, asked %q", code, errs)
	}
	if !strings.HasPrefix(h.raw, "\n  canyon dev · claims-portal prod → held · localhost:7777\n") {
		t.Errorf("the first line is not the hold's:\n%s", h.raw)
	}
	want, err := os.ReadFile(filepath.Join(pkgDir, "testdata", "undeploy-hold.txt")) //nolint:gosec // a fixture this test named
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("the hold printed\n%s\ntestdata/undeploy-hold.txt wants\n%s", out, want)
	}
	if len(h.sent) != 1 || h.sent[0] != "POST /api/v1/projects/claims-portal/envs/prod/hold" || h.bodies[0] != `{"reason":"`+incident+`"}` {
		t.Errorf("the hold sent %q with %q", h.sent, h.bodies)
	}
	enter := &undeployHost{code: http.StatusNoContent}
	if out, _, code := holding(t, enter, "\n", true); code != exitError || len(enter.sent) != 0 || !strings.HasSuffix(out, "\n  Nothing was held.\n") {
		t.Errorf("Enter exited %d, sent %q, printed %q", code, enter.sent, out)
	}
	piped := &undeployHost{code: http.StatusNoContent} // nothing can answer, and --yes is the flag that does
	if _, errs, code := holding(t, piped, "y\n", false); code != exitError || len(piped.sent) != 0 || !strings.Contains(errs, "--yes") {
		t.Errorf("a pipe held production: exit %d, stderr %q, sent %q", code, errs, piped.sent)
	}
}

// A hold that meets a promotion mid-switch, or production the org's platform runs, answers 202 as the
// brake does: exit 0, and the row says what production is waiting on.
func TestHeldStillUnderWaySaysWhatItWaitsOn(t *testing.T) {
	for body, row := range map[string]string{
		`{"stopping":{"run":{"id":"gr_9a1c","state":"running"}}}`: "· Held    prod · stopping\n  Production is stopping: a promotion was already switching it, and Canyon stops it the moment it lands.\n",
		`{"release":"rel_9b31"}`:                                  "· Held    prod · removing · waiting on your platform\n",
	} {
		h := &undeployHost{code: http.StatusAccepted, body: body}
		if out, errs, code := holding(t, h, "", false, "--yes"); code != exitOK || errs != "" || !strings.Contains(out, "\n  "+row+"\n  Lift it in Canyon's dashboard when it may run again.\n") {
			t.Errorf("a 202 %s exited %d, stderr %q, stdout:\n%s", body, code, errs, out)
		}
	}
}

// Who may hold is the host's to say (design/PERMISSIONS.md, Security responders): a responder or an
// admin, a person, and never an app's own team. The CLI sends the hold whoever runs it and says each
// answer at that boundary in its own words.
func TestHeldByAResponderOrAnAdminAndAPerson(t *testing.T) {
	refused := `{"error":"responder_required","message":"only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.","admins":["ana@meridian.example"]}`
	agent := `{"error":"person_required","message":"holding production is a person's act"}`
	for _, c := range []struct {
		caller, body string
		status, exit int
		says         string
	}{
		{"owner", refused, http.StatusForbidden, exitError, "✗ only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.\n"},
		{"contributor", refused, http.StatusForbidden, exitError, "✗ only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.\n"},
		{"responder", "", http.StatusNoContent, exitOK, ""},
		{"admin", "", http.StatusNoContent, exitOK, ""},
		{"an agent's session", agent, http.StatusForbidden, exitBlocked, "✗ Only a person can take claims-portal offline in production. Sign in as yourself with canyon login, then run this again.\n"},
	} {
		h := &undeployHost{code: c.status, body: c.body}
		out, errs, code := holding(t, h, "", false, "--yes")
		held := strings.Contains(out, "✓ Held    prod · stopped\n")
		if code != c.exit || len(h.sent) != 1 || held != (c.exit == exitOK) || !strings.HasSuffix(errs, c.says) || c.says == "" && errs != "" {
			t.Errorf("%s: exit %d, sent %q, stdout %q, stderr %q", c.caller, code, h.sent, out, errs)
		}
	}
}

// A hold's 404 is guard (a)'s refusal, as the app's own read's is: under ai its block names prod.
func TestHeldOnAnAppTheHostLacksNamesProd(t *testing.T) {
	h := &undeployHost{code: http.StatusNotFound, body: `{"error":"not_found","message":"not found"}`}
	out, _, code := holding(t, h, "", false, "--yes", "--format", "ai")
	if code != exitError || !strings.HasPrefix(out, "CANYON UNDEPLOY — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\n") {
		t.Errorf("a hold's 404 exited %d:\n%s", code, out)
	}
}
