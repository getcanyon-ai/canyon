package cli

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// lacks is the host's answer to the project read for an app it doesn't have (contracts/routes.md).
var lacks = reply{http.StatusNotFound, `{"error":"not_found","message":"claims-portal isn't on this Canyon yet. Deploy it first: canyon deploy"}`}

// onHost runs a verb in a checkout whose canyon.toml names claims-portal, against h, and names the host.
func onHost(t *testing.T, h *gateHost, args ...string) (out, errOut string, code int, host string) {
	t.Helper()
	server := httptest.NewServer(h.defaults())
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	out, errOut, code = run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, args...)
	return out, errOut, code, server.URL
}

// defaults is a host that makes the app, starts a run and ends it, where the case serves none.
func (h *gateHost) defaults() *gateHost {
	if h.project.code == 0 {
		h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
	}
	if h.start.code == 0 {
		h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	}
	if h.page == "" {
		h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
	}
	return h
}

func (h *gateHost) uploaded() bool {
	return slices.ContainsFunc(h.asked, func(path string) bool { return strings.HasSuffix(path, "/packages") })
}

// Guard (a), cli.md §3.2: a canyon.toml that names an app this host doesn't have is refused, exit 1,
// and nothing is made or sent — a production checkout never becomes a new app on staging.
func TestDeployRefusesAnAppTheHostLacks(t *testing.T) {
	h := &gateHost{read: lacks, run: aRun("clean", 0, 0)}
	out, errOut, code, host := onHost(t, h, "deploy")
	if code != exitError || len(h.created) > 0 || h.uploaded() {
		t.Errorf("exit %d, made %q, asked %q", code, h.created, h.asked)
	}
	if want := "\n  canyon dev · claims-portal → preview · " + strings.TrimPrefix(host, "http://") + "\n"; out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
	if want := "\n  ✗ claims-portal isn't on " + host + ". If it should be, run: canyon deploy --create\n"; errOut != want {
		t.Errorf("stderr %q, want %q", errOut, want)
	}
}

func TestCheckRefusesAnAppTheHostLacks(t *testing.T) {
	h := &gateHost{read: lacks, run: aRun("clean", 0, 0)}
	out, errOut, code, host := onHost(t, h, "check")
	if code != exitError || len(h.created) > 0 || h.uploaded() || out != "\n  canyon dev · claims-portal · "+strings.TrimPrefix(host, "http://")+"\n" ||
		errOut != "\n  ✗ claims-portal isn't on "+host+". If it should be, run: canyon deploy --create\n" {
		t.Errorf("exit %d, made %q, asked %q, stdout %q, stderr %q", code, h.created, h.asked, out, errOut)
	}
}

// Under ai the refusal is the verb's own block, and its steps leave --create to the person.
func TestTheRefusalUnderAIAsksThePerson(t *testing.T) {
	steps := "NEXT STEPS:\n" +
		"1. Tell the user the outcome line, and ask whether claims-portal is new on this Canyon.\n" +
		"2. Only if the user says it is, run: canyon deploy --create\n" +
		"3. Do not add --create unless the user says this app is new on this Canyon, and do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	for verb, env := range map[string]string{"deploy": "env: preview\n", "check": ""} {
		h := &gateHost{read: lacks, run: aRun("clean", 0, 0)}
		out, errOut, code, host := onHost(t, h, verb, "--format", "ai")
		want := "CANYON REVIEW — RESULT: REFUSED\n\napp: claims-portal\n" + env + "host: " + host + "\n" +
			"outcome: claims-portal isn't on " + host + ". Nothing was sent.\n\n" + steps
		if code != exitError || errOut != "" || out != want || len(h.created) > 0 {
			t.Errorf("%s: exit %d, made %q, stderr %q, stdout\n%s\nwant\n%s", verb, code, h.created, errOut, out, want)
		}
	}
}

// Under json there is no object, as for every refusal (cli.md §5.4): the line goes to stderr.
func TestTheRefusalUnderJSONIsTheLineOnStderr(t *testing.T) {
	h := &gateHost{read: lacks, run: aRun("clean", 0, 0)}
	out, errOut, code, host := onHost(t, h, "deploy", "--format", "json")
	if code != exitError || out != "" || errOut != "\n  ✗ claims-portal isn't on "+host+". If it should be, run: canyon deploy --create\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// Promotion has nothing to promote, and its refusal offers no --create, in either format.
func TestPromotionRefusesAnAppTheHostLacksWithNoCreate(t *testing.T) {
	h := &gateHost{read: lacks}
	out, errOut, code, host := onHost(t, h, "deploy", "--env", "prod")
	if code != exitError || out != "\n  canyon dev · claims-portal preview → prod · "+strings.TrimPrefix(host, "http://")+"\n" ||
		errOut != "\n  ✗ claims-portal isn't on "+host+", so there is nothing here to promote.\n" ||
		slices.Contains(h.asked, "GET /api/v1/projects/claims-portal/status") {
		t.Errorf("exit %d, asked %q, stdout %q, stderr %q", code, h.asked, out, errOut)
	}
	h = &gateHost{read: lacks}
	out, errOut, code, host = onHost(t, h, "deploy", "--env", "prod", "--format", "ai")
	want := "CANYON PROMOTE — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\nhost: " + host + "\n" +
		"outcome: claims-portal isn't on " + host + ", so there is nothing here to promote.\n\n" +
		"NEXT STEPS:\n1. Tell the user the outcome line.\n2. Stop. Do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	if code != exitError || errOut != "" || out != want {
		t.Errorf("ai: exit %d, stderr %q, stdout\n%s\nwant\n%s", code, errOut, out, want)
	}
}

// --create makes the app canyon.toml names, with that id, and deploys it; on a host that has it, or
// that answers a reduced row for an app the caller isn't on, it makes nothing (cli.md §3.2).
func TestCreateMakesIt(t *testing.T) {
	h := &gateHost{read: lacks, run: aRun("clean", 0, 0), page: buildPage(t, nil, true)}
	if _, errOut, code, _ := onHost(t, h, "deploy", "--create"); code != exitOK || !slices.Equal(h.created, []string{`{"id":"claims-portal"}`}) || !h.uploaded() {
		t.Errorf("a host without it: exit %d, made %q, asked %q, stderr %q", code, h.created, h.asked, errOut)
	}
	h = &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, nil, true)} // the read answers 200
	if _, errOut, code, _ := onHost(t, h, "deploy", "--create"); code != exitOK || len(h.created) > 0 || !h.uploaded() {
		t.Errorf("a host with it: exit %d, made %q, asked %q, stderr %q", code, h.created, h.asked, errOut)
	}
	h = &gateHost{read: reduced, upload: teamRequired}
	if _, errOut, code, _ := onHost(t, h, "deploy", "--create"); code != exitError || len(h.created) > 0 || !strings.Contains(errOut, "To make your own app instead") {
		t.Errorf("an app the caller isn't on: exit %d, made %q, stderr %q", code, h.created, errOut)
	}
}

var (
	reduced      = reply{http.StatusOK, `{"project":{"id":"claims-portal","name":"claims-portal","owners":[],"summary":"","access":{"level":"none"}}}`}
	teamRequired = reply{http.StatusForbidden, `{"error":"team_required","message":"Only claims-portal's team can do that."}`}
)

// Permissions (design/PERMISSIONS.md): the read answers every signed-in caller, so the guard never
// stands in for the team check. An owner, a contributor and an admin deploy; a read-only member and
// a caller with no access read the reduced row, pass the guard, and meet the upload's team_required,
// as §9 says.
func TestTheGuardPassesEveryCallerTheHostAnswers(t *testing.T) {
	row := func(level string) reply {
		return reply{http.StatusOK, `{"project":{"id":"claims-portal","access":{"level":"` + level + `","everyone":false,"people":0,"groups":0}}}`}
	}
	for _, c := range []struct {
		who     string
		read    reply
		upload  reply
		deploys bool
	}{
		{"owner", row("owner"), reply{}, true},
		{"contributor", row("contributor"), reply{}, true},
		{"admin", row("none"), reply{}, true}, // an admin's level is whatever their grants make it, and the upload lets them in
		{"read-only", reply{http.StatusOK, `{"project":{"id":"claims-portal","name":"claims-portal","owners":[],"summary":"","access":{"level":"viewer"},"envs":{}}}`}, teamRequired, false},
		{"no access", reduced, teamRequired, false},
	} {
		h := &gateHost{read: c.read, upload: c.upload, run: aRun("clean", 0, 0), page: buildPage(t, nil, true)}
		_, errOut, code, _ := onHost(t, h, "deploy")
		refused := code == exitError && strings.Contains(errOut, "To make your own app instead, delete the app line from canyon.toml and deploy again.")
		if len(h.created) > 0 || !h.uploaded() || (code == exitOK) != c.deploys || refused == c.deploys {
			t.Errorf("%s: exit %d, made %q, asked %q, stderr %q", c.who, code, h.created, h.asked, errOut)
		}
	}
}

// Any other answer to the read refuses in the host's words, spelled out, and is never an app to
// create: a 404 that is not the contract's not_found, a 403, a 500, or a 200 that is not the app's row.
func TestAnotherAnswerToTheReadRefusesAndMakesNothing(t *testing.T) {
	for _, c := range []struct {
		read reply
		want string // the line after the glyph; HOST is the host dialled
	}{
		{reply{http.StatusNotFound, `<html>Not Found</html>`}, "Not Found"},
		{reply{http.StatusNotFound, `{"error":"no_route","message":"no such route"}`}, "no such route"},
		{reply{http.StatusForbidden, `{"error":"forbidden","message":"members only"}`}, "members only"},
		{reply{http.StatusInternalServerError, `{"error":"internal","message":"the database\u001b[2J is down"}`}, `the database\x1b[2J is down`},
		{reply{http.StatusOK, `<html>a sign-in page</html>`}, "HOST answered something that is not Canyon (HTTP 200)"},
		{reply{http.StatusOK, `{"project":{"id":"someone-else"}}`}, "HOST answered something that is not Canyon (HTTP 200)"},
	} {
		for _, args := range [][]string{{"deploy"}, {"deploy", "--create"}, {"check"}} {
			h := &gateHost{read: c.read}
			_, errOut, code, host := onHost(t, h, args...)
			if code != exitError || len(h.created) > 0 || h.uploaded() || errOut != "\n  ✗ "+strings.ReplaceAll(c.want, "HOST", host)+"\n" {
				t.Errorf("%s, %q: exit %d, made %q, asked %q, stderr %q", c.read.body, args, code, h.created, h.asked, errOut)
			}
		}
	}
}

// --create is deploy's alone: check and secret set refuse it as an unknown flag (prod's refusal is
// TestDeployEnvTakesPreviewOrProd's).
func TestCreateIsDeploysAlone(t *testing.T) {
	for _, args := range [][]string{{"check", "--create"}, {"secret", "set", "X", "--create"}} {
		h := &gateHost{read: lacks}
		if _, errOut, code, _ := onHost(t, h, args...); code != exitError || len(h.asked) > 0 || errOut != "\n  ✗ flag provided but not defined: -create\n" {
			t.Errorf("%q: exit %d, asked %q, stderr %q", args, code, h.asked, errOut)
		}
	}
}

// secret set reads the app before it asks for the value, so a value never reaches a host that lacks
// the app; secret list learns it from its own read's 404 (cli.md §3.2, §4.3). Neither makes the app,
// and neither says there is nothing set.
func TestSecretSetAndListRefuseAnAppTheHostLacks(t *testing.T) {
	steps := "NEXT STEPS:\n" +
		"1. Tell the user the outcome line, and ask whether claims-portal is new on this Canyon.\n" +
		"2. Only if the user says it is, run: canyon deploy --create\n" +
		"3. Do not add --create unless the user says this app is new on this Canyon, and do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	for _, c := range []struct {
		args       []string
		head, tail string
	}{
		{[]string{"secret", "set", "STRIPE_SECRET_KEY", "--env", "prod"}, "CANYON SECRET SET — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\n", " Nothing was set."},
		{[]string{"secret", "list"}, "CANYON SECRET LIST — RESULT: REFUSED\n\napp: claims-portal\n", ""},
	} {
		base, asked := serveSecrets(t, http.StatusNotFound, lacks.body)
		inProject(t, "claims-portal")
		env := map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}
		out, errs, code := answer(t, word+"\n", env, false, c.args...)
		if code != exitError || out != "\n  canyon dev · claims-portal · "+strings.TrimPrefix(base, "http://")+"\n" ||
			errs != "\n  ✗ claims-portal isn't on "+base+". If it should be, run: canyon deploy --create\n" || len(*asked) != 1 ||
			(*asked)[0] != map[string]string{"set": "GET /api/v1/projects/claims-portal", "list": "GET /api/v1/projects/claims-portal/secrets"}[c.args[1]] {
			t.Errorf("%q: exit %d, asked %q, stdout %q, stderr %q", c.args, code, *asked, out, errs)
		}
		out, errs, code = answer(t, word+"\n", env, false, append(c.args, "--format", "ai")...)
		want := c.head + "host: " + base + "\noutcome: claims-portal isn't on " + base + "." + c.tail + "\n\n" + steps
		if code != exitError || errs != "" || out != want {
			t.Errorf("%q under ai: exit %d, stderr %q, stdout\n%s\nwant\n%s", c.args, code, errs, out, want)
		}
	}
}

// lackingHost answers /auth/config and 404s everything else as the host does for an app it lacks.
func lackingHost(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/config" {
			_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc","org":"Meridian","device":true,"cli":"","public":""}`))
			return
		}
		w.WriteHeader(lacks.code)
		_, _ = w.Write([]byte(lacks.body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A read that learns from its own 404 that canyon.toml's app isn't on the host says guard (a)'s
// line, never the server's "Deploy it first: canyon deploy", which names the command guard (a)
// refuses (cli.md §3.2, #2192). A share of an app named by --app has no canyon.toml behind it, and
// keeps the host's words.
func TestReadsOfAnAppTheHostLacksAgreeWithGuardA(t *testing.T) {
	steps := "NEXT STEPS:\n" +
		"1. Tell the user the outcome line, and ask whether claims-portal is new on this Canyon.\n" +
		"2. Only if the user says it is, run: canyon deploy --create\n" +
		"3. Do not add --create unless the user says this app is new on this Canyon, and do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	for _, args := range [][]string{{"status"}, {"share"}, {"share", "sam@meridian.example"}} {
		verb := args[0]
		base := lackingHost(t)
		inProject(t, "claims-portal")
		env := map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}
		_, errs, code := run(t, env, false, args...)
		if want := "\n  ✗ claims-portal isn't on " + base + ". If it should be, run: canyon deploy --create\n"; code != exitError || errs != want {
			t.Errorf("%q: exit %d, stderr %q, want %q", args, code, errs, want)
		}
		out, errs, code := run(t, env, false, append(args, "--format", "ai")...)
		want := "CANYON " + strings.ToUpper(verb) + " — RESULT: REFUSED\n\napp: claims-portal\nhost: " + base + "\noutcome: claims-portal isn't on " + base + ".\n\n" + steps
		if code != exitError || errs != "" || out != want {
			t.Errorf("%q under ai: exit %d, stderr %q, stdout\n%s\nwant\n%s", args, code, errs, out, want)
		}
	}
	inProject(t, "claims-portal")
	env := map[string]string{"CANYON_HOST": lackingHost(t), "XDG_CONFIG_HOME": t.TempDir()}
	_, errs, code := run(t, env, false, "share", "--app", "claims-portal")
	if want := "\n  ✗ claims-portal isn't on this Canyon yet. Deploy it first: canyon deploy\n"; code != exitError || errs != want {
		t.Errorf("share --app: exit %d, stderr %q, want the host's %q", code, errs, want)
	}
}

// The verbs whose route's 404 can also mean something else — nothing deployed, no package, no such
// grant — learn from the app's own read which it is, so where canyon.toml names an app the host
// lacks they say guard (a)'s line and block too, never the host's (cli.md §3.2, #2195). Each block
// says what its verb did not do; the reads end at the host.
func TestEveryVerbOfAnAppTheHostLacksSaysGuardA(t *testing.T) {
	steps := "NEXT STEPS:\n" +
		"1. Tell the user the outcome line, and ask whether claims-portal is new on this Canyon.\n" +
		"2. Only if the user says it is, run: canyon deploy --create\n" +
		"3. Do not add --create unless the user says this app is new on this Canyon, and do not edit canyon.toml, add --host or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	for _, c := range []struct {
		args       []string
		head, tail string
	}{
		{[]string{"logs"}, "CANYON LOGS — RESULT: REFUSED\n\napp: claims-portal\n", ""},
		{[]string{"pull", "--force"}, "CANYON PULL — RESULT: REFUSED\n\napp: claims-portal\n", ""},
		{[]string{"undeploy", "--env", "prod", "--yes"}, "CANYON UNDEPLOY — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\n", " Nothing was taken offline."},
		{[]string{"delete"}, "CANYON DELETE — RESULT: REFUSED\n\napp: claims-portal\n", " Nothing was deleted."},
		{[]string{"delete", "--confirm", "claims-portal"}, "CANYON DELETE — RESULT: REFUSED\n\napp: claims-portal\n", " Nothing was deleted."},
		{[]string{"share", "--remove", "sam@meridian.example"}, "CANYON SHARE — RESULT: REFUSED\n\napp: claims-portal\n", ""},
	} {
		base := lackingHost(t)
		inProject(t, "claims-portal")
		env := map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}
		_, errs, code := run(t, env, false, c.args...)
		if want := "\n  ✗ claims-portal isn't on " + base + ". If it should be, run: canyon deploy --create\n"; code != exitError || errs != want {
			t.Errorf("%q: exit %d, stderr %q, want %q", c.args, code, errs, want)
		}
		out, errs, code := run(t, env, false, append(c.args, "--format", "ai")...)
		want := c.head + "host: " + base + "\noutcome: claims-portal isn't on " + base + "." + c.tail + "\n\n" + steps
		if code != exitError || errs != "" || out != want {
			t.Errorf("%q under ai: exit %d, stderr %q, stdout\n%s\nwant\n%s", c.args, code, errs, out, want)
		}
	}
}

// undeploy --env prod reads the app before production's question, as deploy --env prod does: at a
// terminal, an app the host lacks is refused before anything is asked or sent (cli.md §3.2).
func TestUndeployProdRefusesAnAppTheHostLacksBeforeItAsks(t *testing.T) {
	base := lackingHost(t)
	inProject(t, "claims-portal")
	_, errs, code := answer(t, "y\n", map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}, true, "undeploy", "--env", "prod")
	if want := "\n  ✗ claims-portal isn't on " + base + ". If it should be, run: canyon deploy --create\n"; code != exitError || errs != want {
		t.Errorf("exit %d, stderr %q, want %q and no question", code, errs, want)
	}
}

// A 404 for any other reason keeps the verb's own line: the host has the app, and the route's 404 is
// nothing deployed there, no package yet or no such grant — or the app's own read can't say: it
// answers a 500, or the connection drops. An app named by --app has no canyon.toml behind it, and
// keeps the verb's line too (#2195).
func TestAnotherReasonsNotFoundKeepsItsOwnLine(t *testing.T) {
	const (
		has    = "has"    // the host has the app
		failed = "failed" // its read answers 500
		drops  = "drops"  // its read's connection is closed unanswered
		lacked = "lacked" // the host lacks it: every route 404s
	)
	logsLine := "Nothing is deployed in preview for claims-portal, or there is no such app."
	pullLine := "claims-portal has no package yet, so there is nothing to pull (or there is no such app)."
	for _, c := range []struct {
		args    []string
		project string
		want    string
	}{
		{[]string{"logs"}, has, logsLine},
		{[]string{"pull", "--force"}, has, pullLine},
		{[]string{"undeploy", "--env", "preview"}, has, "Nothing is running in preview."},
		{[]string{"share", "--remove", "sam@meridian.example"}, has, "no such grant"},
		{[]string{"logs"}, failed, logsLine},
		{[]string{"pull", "--force"}, failed, pullLine},
		{[]string{"share", "--remove", "sam@meridian.example"}, failed, "no such grant"},
		{[]string{"logs"}, drops, logsLine},
		{[]string{"pull", "--force"}, drops, pullLine},
		{[]string{"share", "--remove", "sam@meridian.example"}, drops, "no such grant"},
		{[]string{"logs", "--app", "claims-portal"}, lacked, logsLine},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc","org":"Meridian","device":true,"cli":"","public":""}`))
			case c.project == lacked:
				w.WriteHeader(lacks.code)
				_, _ = w.Write([]byte(lacks.body))
			case r.URL.Path != "/api/v1/projects/claims-portal":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not_found","message":"no such grant"}`))
			case c.project == failed:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal","message":"internal error"}`))
			case c.project == drops:
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			default:
				_, _ = w.Write([]byte(`{"project":{"id":"claims-portal"}}`))
			}
		}))
		t.Cleanup(server.Close)
		inProject(t, "claims-portal")
		_, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, c.args...)
		if want := "\n  ✗ " + c.want + "\n"; code != exitError || errs != want {
			t.Errorf("%q, the app read %s: exit %d, stderr %q, want %q", c.args, c.project, code, errs, want)
		}
	}
}

// status reads the app first, and its later reads, /status and the runs list, 404 only for an app the
// host no longer has: one deleted between them says guard (a)'s line too (#2195).
func TestStatusLaterReadsOfAnAppTheHostLostSayGuardA(t *testing.T) {
	for _, gone := range []string{"/api/v1/projects/claims-portal/status", "/api/v1/projects/claims-portal/gate-runs"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case gone:
				w.WriteHeader(lacks.code)
				_, _ = w.Write([]byte(lacks.body))
			case "/api/v1/projects/claims-portal":
				_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","owners":[],"summary":"","latest":{"package":"pkg_1","stage":"deployed"}}}`))
			case "/api/v1/projects/claims-portal/status":
				_, _ = w.Write([]byte(`{"envs":{},"last_run":"gr_8f2a","receipt":{"org":"Meridian","blocked_by":[],"rules":[]}}`))
			}
		}))
		t.Cleanup(server.Close)
		inProject(t, "claims-portal")
		_, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		if want := "\n  ✗ claims-portal isn't on " + server.URL + ". If it should be, run: canyon deploy --create\n"; code != exitError || errs != want {
			t.Errorf("%s: exit %d, stderr %q, want %q", gone, code, errs, want)
		}
	}
}
