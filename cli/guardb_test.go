package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// unknownPackage is the route's answer for a package this app doesn't have (contracts/routes.md,
// GET /projects/:id/packages/:pkg): the one answer guard (b) refuses on.
var unknownPackage = reply{http.StatusNotFound, `{"error":"unknown_package","message":"no such package of this project"}`}

// staging is the host the transcripts name (cli.md §6.6): the test server's URL reads as it.
const staging = "https://canyon-staging.meridian.example"

var heldSession = strings.Repeat("5e", 32)

// signedInTo is a config directory holding a session for each host: config.json names the first,
// and each other has its own hosts/ file (cli.md §8.4).
func signedInTo(t *testing.T, hosts ...string) string {
	t.Helper()
	xdg := t.TempDir()
	must(t, (&config{Host: hosts[0], Session: heldSession}).write(filepath.Join(xdg, "canyon", "config.json")))
	for _, h := range hosts[1:] {
		must(t, (&config{Host: h, Session: heldSession}).write(filepath.Join(hostsAt(xdg), hostFile(h))))
	}
	return xdg
}

// twoOthers is a machine signed in to production and to the EU host, neither the one it dials.
func twoOthers(t *testing.T) string {
	t.Helper()
	return signedInTo(t, "https://canyon.meridian.example", "https://canyon-eu.meridian.example")
}

// fromCheckout runs a verb in a checkout of claims-portal whose base is base (none for ""), with the
// config in xdg, against h; the URL of the test server reads as staging in what it printed.
func fromCheckout(t *testing.T, h http.Handler, xdg, base string, args ...string) (out, errOut string, code int) {
	t.Helper()
	return inCheckout(t, h, xdg, base, word+"\n", false, args...)
}

// inCheckout is fromCheckout with input typed, at a terminal when tty is set.
func inCheckout(t *testing.T, h http.Handler, xdg, base, input string, tty bool, args ...string) (out, errOut string, code int) {
	t.Helper()
	if g, ok := h.(*gateHost); ok {
		h = g.defaults()
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	dir := projectDir(t)
	if base != "" {
		writeFile(t, filepath.Join(dir, baseDir, baseFile), base)
	}
	t.Chdir(dir)
	out, errOut, code = answer(t, input, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": xdg, "NO_COLOR": "1"}, tty, args...)
	named := strings.NewReplacer(server.URL, staging, strings.TrimPrefix(server.URL, "http://"), strings.TrimPrefix(staging, "https://"))
	return named.Replace(out), named.Replace(errOut), code
}

// recorded is the base pull and deploy write: pkg_9e40, for claims-portal.
const recorded = "app     = \"claims-portal\"\npackage = \"pkg_9e40\"\n"

// baseHost has claims-portal, answers the base check with pkg (pkg_9e40's row when unset) and
// every write with success, and keeps each request.
type baseHost struct {
	pkg   reply
	asked []string
}

func (h *baseHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.asked = append(h.asked, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/api/v1/projects/claims-portal/packages/") && h.pkg.code != 0:
		w.WriteHeader(h.pkg.code)
		_, _ = io.WriteString(w, h.pkg.body)
	case strings.HasPrefix(p, "/api/v1/projects/claims-portal/packages/"):
		_, _ = io.WriteString(w, `{"package":{"id":"pkg_9e40"}}`)
	case p == "/auth/config":
		_, _ = io.WriteString(w, `{"canyon":true,"auth":"oidc","org":"Meridian","device":true,"public":""}`)
	case r.Method == http.MethodGet && p == "/api/v1/projects/claims-portal":
		_, _ = io.WriteString(w, `{"project":{"id":"claims-portal","envs":{}}}`)
	case strings.HasSuffix(p, "/access") || strings.Contains(p, "/access/"):
		_, _ = io.WriteString(w, `{"entries":[],"requests":[]}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// wrote is every request that was not a read.
func (h *baseHost) wrote() []string {
	return slices.DeleteFunc(slices.Clone(h.asked), func(r string) bool { return strings.HasPrefix(r, "GET ") })
}

func (h *baseHost) checked() bool {
	return slices.Contains(h.asked, "GET /api/v1/projects/claims-portal/packages/pkg_9e40")
}

// refusedHere is §6.6's refusal for a verb path, naming the two other hosts.
func refusedHere(verb string) string {
	return "\n  ✗ pkg_9e40 isn't on " + staging + ".\n" +
		"    This checkout was last sent or pulled with it, so it went to another Canyon, or this one no longer has it.\n\n" +
		"    Another Canyon:   canyon " + verb + " --host https://canyon-eu.meridian.example\n" +
		"                      canyon " + verb + " --host https://canyon.meridian.example\n" +
		"    This one:         rm .canyon/base, then canyon " + verb + " again\n"
}

// TestUnknownBaseRefusesNamingBothHosts is guard (b), cli.md §6.6: a checkout whose base this host
// doesn't have is refused before anything is uploaded, naming the hosts this machine is signed in
// to — in the human transcript, the ai block, and under json on stderr alone.
func TestUnknownBaseRefusesNamingBothHosts(t *testing.T) {
	human, ai := golden(t, "unknown-base-deploy.txt"), golden(t, "unknown-base-deploy-ai.txt")
	h := &gateHost{pkg: unknownPackage, run: aRun("clean", 0, 0)}
	out, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "deploy")
	if code != exitError || h.uploaded() || out != "\n  canyon dev · claims-portal → preview · canyon-staging.meridian.example\n" || errOut != human {
		t.Errorf("exit %d, asked %q, stdout %q, stderr\n%s\nwant\n%s", code, h.asked, out, errOut, human)
	}
	h = &gateHost{pkg: unknownPackage, run: aRun("clean", 0, 0)}
	out, errOut, code = fromCheckout(t, h, twoOthers(t), recorded, "deploy", "--format", "ai")
	if code != exitError || h.uploaded() || errOut != "" || out != ai {
		t.Errorf("ai: exit %d, asked %q, stderr %q, stdout\n%s\nwant\n%s", code, h.asked, errOut, out, ai)
	}
	h = &gateHost{pkg: unknownPackage, run: aRun("clean", 0, 0)}
	out, errOut, code = fromCheckout(t, h, twoOthers(t), recorded, "deploy", "--format", "json")
	if code != exitError || h.uploaded() || out != "" || errOut != human {
		t.Errorf("json: exit %d, asked %q, stdout %q, stderr %q", code, h.asked, out, errOut)
	}
}

// secret set checks the base after the app's read and before the value is asked for, so a value
// never reaches a host this checkout was not sent to; a base the host has sets it.
func TestSecretSetChecksTheBase(t *testing.T) {
	h := &baseHost{pkg: unknownPackage}
	out, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "secret", "set", "STRIPE_SECRET_KEY", "--env", "prod")
	if want := []string{"GET /api/v1/projects/claims-portal", "GET /api/v1/projects/claims-portal/packages/pkg_9e40"}; code != exitError || !slices.Equal(h.asked, want) ||
		out != "\n  canyon dev · claims-portal · canyon-staging.meridian.example\n" || errOut != refusedHere("secret set") {
		t.Errorf("exit %d, asked %q, stdout %q, stderr\n%s", code, h.asked, out, errOut)
	}
	h = &baseHost{pkg: unknownPackage}
	out, errOut, code = fromCheckout(t, h, twoOthers(t), recorded, "secret", "set", "STRIPE_SECRET_KEY", "--env", "prod", "--format", "ai")
	want := "CANYON SECRET SET — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\nhost: " + staging + "\nbase: pkg_9e40\n" +
		"outcome: pkg_9e40 isn't on " + staging + ". This checkout was last sent or pulled with it, so it went to another Canyon, or this one no longer has it. Nothing was set.\n" +
		"also signed in to:\n  https://canyon-eu.meridian.example\n  https://canyon.meridian.example\n\n" + baseSteps
	if code != exitError || len(h.wrote()) > 0 || errOut != "" || out != want {
		t.Errorf("ai: exit %d, asked %q, stderr %q, stdout\n%s\nwant\n%s", code, h.asked, errOut, out, want)
	}
	h = &baseHost{}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "secret", "set", "STRIPE_SECRET_KEY"); code != exitOK || !h.checked() ||
		!slices.Equal(h.wrote(), []string{"PUT /api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY"}) {
		t.Errorf("a base this host has: exit %d, asked %q, stderr %q", code, h.asked, errOut)
	}
}

const baseSteps = "NEXT STEPS:\n" +
	"1. Tell the user the outcome line and the hosts above, and ask which Canyon claims-portal belongs on.\n" +
	"2. Stop until they answer. Do not delete .canyon/, pull, or add --force, --host or --create, or set CANYON_HOST to get past this.\n" +
	"\nexit code: 1\n"

// Every verb that changes the app runs the check (cli.md §6.6), and each names itself by its verb
// path alone, never a name or value typed after it — undeploy with its --env, which it can't run
// without. None sends anything.
func TestEveryAppChangingVerbChecksTheBase(t *testing.T) {
	for _, c := range []struct {
		args []string
		path string
	}{
		{[]string{"check"}, "check"},
		{[]string{"deploy"}, "deploy"},
		{[]string{"deploy", "--force"}, "deploy"},
		{[]string{"deploy", "--env", "prod", "--yes"}, "deploy --env prod"},
		{[]string{"undeploy", "--env", "preview"}, "undeploy --env preview"},
		{[]string{"undeploy", "--env", "prod", "--yes"}, "undeploy --env prod"},
		{[]string{"delete", "--confirm", "claims-portal"}, "delete"},
		{[]string{"secret", "set", "STRIPE_SECRET_KEY"}, "secret set"},
		{[]string{"secret", "rm", "STRIPE_SECRET_KEY", "--env", "prod"}, "secret rm"},
		{[]string{"share", "sam@meridian.example", "--level", "contributor"}, "share"},
		{[]string{"share", "--remove", "everyone"}, "share"},
		{[]string{"rename", "Motor claims"}, "rename"},
	} {
		h := &baseHost{pkg: unknownPackage}
		_, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, c.args...)
		if code != exitError || !h.checked() || len(h.wrote()) > 0 || errOut != refusedHere(c.path) {
			t.Errorf("%q: exit %d, asked %q, stderr\n%s\nwant\n%s", c.args, code, h.asked, errOut, refusedHere(c.path))
		}
	}
}

// Under ai each verb's block carries its own header and environment, the result's where it has
// one, and no --host command: which Canyon an app belongs on is the person's call.
func TestTheBlockUnderAINamesNoHostCommand(t *testing.T) {
	for _, c := range []struct {
		args       []string
		head, tail string
	}{
		{[]string{"check"}, "CANYON REVIEW — RESULT: REFUSED\n\napp: claims-portal\n", " Nothing was sent."},
		{[]string{"deploy", "--env", "prod"}, "CANYON PROMOTE — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\n", " Nothing was sent."},
		{[]string{"undeploy", "--env", "prod"}, "CANYON UNDEPLOY — RESULT: REFUSED\n\napp: claims-portal\nenv: prod\n", " Nothing was sent."},
		{[]string{"delete"}, "CANYON DELETE — RESULT: REFUSED\n\napp: claims-portal\n", " Nothing was sent."},
		{[]string{"secret", "rm", "STRIPE_SECRET_KEY"}, "CANYON SECRET RM — RESULT: REFUSED\n\napp: claims-portal\nenv: preview\n", " Nothing was sent."},
		{[]string{"share", "everyone"}, "CANYON SHARE — RESULT: REFUSED\n\napp: claims-portal\n", " Nothing was sent."},
	} {
		h := &baseHost{pkg: unknownPackage}
		out, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, append(c.args, "--format", "ai")...)
		want := c.head + "host: " + staging + "\nbase: pkg_9e40\n" +
			"outcome: pkg_9e40 isn't on " + staging + ". This checkout was last sent or pulled with it, so it went to another Canyon, or this one no longer has it." + c.tail + "\n" +
			"also signed in to:\n  https://canyon-eu.meridian.example\n  https://canyon.meridian.example\n\n" + baseSteps
		if code != exitError || len(h.wrote()) > 0 || errOut != "" || out != want || strings.Contains(out, "--host https") {
			t.Errorf("%q: exit %d, asked %q, stderr %q, stdout\n%s\nwant\n%s", c.args, code, h.asked, errOut, out, want)
		}
	}
}

// The hosts named are those this machine holds a session for, but this one: config.json's included,
// a stale hosts/ file for its host once, a host it signed out of never, and a hosts/ file this CLI
// won't use skipped with its one line. With none, the block says nothing of another Canyon.
func TestTheHostsNamedAreTheSessionsThisMachineHolds(t *testing.T) {
	h := &baseHost{pkg: unknownPackage}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	xdg := signedInTo(t, "https://canyon.meridian.example", "https://canyon-eu.meridian.example", "https://canyon.meridian.example", server.URL)
	must(t, (&config{Host: "https://canyon-old.meridian.example"}).write(filepath.Join(hostsAt(xdg), "https_canyon-old.meridian.example.json"))) // signed out: no session
	broken := filepath.Join(hostsAt(xdg), "https_canyon-us.meridian.example.json")
	writeFile(t, broken, "not json")
	must(t, os.Chmod(broken, 0o600))
	dir := projectDir(t)
	writeFile(t, filepath.Join(dir, baseDir, baseFile), recorded)
	t.Chdir(dir)
	_, errOut, code := answer(t, "", map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": xdg}, false, "secret", "rm", "X")
	errOut = strings.ReplaceAll(errOut, server.URL, staging)
	want := "\n  ! " + broken + ": it is not the JSON Canyon writes, so it is ignored. Run: rm " + broken + "\n" + refusedHere("secret rm")
	if code != exitError || errOut != want {
		t.Errorf("exit %d, stderr\n%s\nwant\n%s", code, errOut, want)
	}

	alone := func(args ...string) (string, string) {
		h := &baseHost{pkg: unknownPackage}
		out, errOut, _ := fromCheckout(t, h, t.TempDir(), recorded, args...)
		return out, errOut
	}
	if _, errOut := alone("secret", "rm", "X"); errOut != "\n  ✗ pkg_9e40 isn't on "+staging+".\n"+
		"    This checkout was last sent or pulled with it, so it went to another Canyon, or this one no longer has it.\n\n"+
		"    This one:         rm .canyon/base, then canyon secret rm again\n" {
		t.Errorf("signed in nowhere else: stderr\n%s", errOut)
	}
	if out, _ := alone("secret", "rm", "X", "--format", "ai"); strings.Contains(out, "also signed in to") || !strings.Contains(out, "\n1. Tell the user the outcome line, and ask which Canyon claims-portal belongs on.\n") {
		t.Errorf("signed in nowhere else, under ai:\n%s", out)
	}
}

// Only the route's own 404 refuses. Any other 404 — a host older than the route, an app gone — skips
// the check, and the verb goes on as before it existed, sending the base it recorded.
func TestAnOlderHostsNotFoundSkipsTheCheck(t *testing.T) {
	for _, older := range []reply{
		{http.StatusNotFound, `{"error":"not_found","message":"no such route"}`},
		{http.StatusNotFound, `<html>Not Found</html>`},
	} {
		h := &gateHost{pkg: older, run: aRun("clean", 0, 0), page: buildPage(t, nil, true)}
		if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "deploy"); code != exitOK || !h.uploaded() || h.base != "pkg_9e40" {
			t.Errorf("%s: exit %d, base sent %q, asked %q, stderr %q", older.body, code, h.base, h.asked, errOut)
		}
		b := &baseHost{pkg: older}
		if _, errOut, code := fromCheckout(t, b, twoOthers(t), recorded, "secret", "rm", "X"); code != exitOK || !slices.Equal(b.wrote(), []string{"DELETE /api/v1/projects/claims-portal/secrets/X"}) {
			t.Errorf("%s, secret rm: exit %d, asked %q, stderr %q", older.body, code, b.asked, errOut)
		}
	}
}

// Every other answer refuses the verb, nothing sent: a 401 is §8.2's line, a caller below contributor
// §9's team_required, any other the host's words spelled out, and a 200 that is not that package's row
// is not Canyon answering.
func TestAnyOtherAnswerToTheBaseCheckRefuses(t *testing.T) {
	for _, c := range []struct {
		pkg  reply
		want string
	}{
		{reply{http.StatusUnauthorized, `{"error":"unauthenticated","message":"sign in"}`}, "You're not signed in to " + staging + ".\n\n  Run: canyon login " + staging},
		{teamRequired, "Only claims-portal's team can do that."},
		{reply{http.StatusInternalServerError, `{"error":"internal","message":"the database\u001b[2J is down"}`}, `the database\x1b[2J is down`},
		{reply{http.StatusOK, `{"package":{"id":"pkg_other"}}`}, staging + " answered something that is not Canyon (HTTP 200)"},
		{reply{http.StatusOK, `<html>a sign-in page</html>`}, staging + " answered something that is not Canyon (HTTP 200)"},
	} {
		for _, args := range [][]string{{"secret", "rm", "X"}, {"undeploy", "--env", "preview"}} {
			h := &baseHost{pkg: c.pkg}
			if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, args...); code != exitError || len(h.wrote()) > 0 || errOut != "\n  ✗ "+c.want+"\n" {
				t.Errorf("%s, %q: exit %d, asked %q, stderr %q", c.pkg.body, args, code, h.asked, errOut)
			}
		}
		g := &gateHost{pkg: c.pkg, run: aRun("clean", 0, 0)}
		if _, _, code := fromCheckout(t, g, twoOthers(t), recorded, "deploy"); code != exitError || g.uploaded() {
			t.Errorf("%s, deploy: exit %d, asked %q", c.pkg.body, code, g.asked)
		}
	}
}

// A base that names this app and can't be used refuses the same verbs, nothing asked or sent; one
// recorded for another app is no base, and no check.
func TestABaseThatCantBeUsedRefuses(t *testing.T) {
	notAnID := "app     = \"claims-portal\"\npackage = \"pkg_9e40; rm -rf\"\n"
	h := &baseHost{}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), notAnID, "secret", "rm", "X"); code != exitError || len(h.asked) > 0 ||
		errOut != "\n  ✗ .canyon/base can't be used: the package in it is not a package id. If this checkout belongs on "+staging+", run: rm .canyon/base\n" {
		t.Errorf("not an id: exit %d, asked %q, stderr %q", code, h.asked, errOut)
	}
	g := &gateHost{run: aRun("clean", 0, 0)}
	out, errOut, code := fromCheckout(t, g, twoOthers(t), notAnID, "deploy", "--format", "ai")
	want := "CANYON REVIEW — RESULT: REFUSED\n\napp: claims-portal\nenv: preview\nhost: " + staging + "\n" +
		"outcome: .canyon/base can't be used: the package in it is not a package id. Nothing was sent.\n\n" +
		"NEXT STEPS:\n1. Tell the user the outcome line, and ask whether this checkout belongs on " + staging + ".\n" +
		"2. Only if the user says it does, run: rm .canyon/base\n" +
		"3. Do not delete .canyon/base unless the user says this checkout belongs on " + staging + ", and do not add --force, --host or --create, or set CANYON_HOST to get past this.\n" +
		"\nexit code: 1\n"
	if code != exitError || g.uploaded() || errOut != "" || out != want {
		t.Errorf("ai: exit %d, asked %q, stderr %q, stdout\n%s\nwant\n%s", code, g.asked, errOut, out, want)
	}

	h = &baseHost{}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	dir := projectDir(t)
	must(t, os.MkdirAll(filepath.Join(dir, baseDir, baseFile), 0o750)) // a directory where the file goes cannot be read
	t.Chdir(dir)
	if _, errOut, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "share", "everyone"); code != exitError ||
		!strings.Contains(errOut, ".canyon/base can't be used: it cannot be read: ") || slices.Contains(h.asked, "GET /api/v1/projects/claims-portal/access") {
		t.Errorf("unreadable: exit %d, asked %q, stderr %q", code, h.asked, errOut)
	}

	for _, cut := range []string{"\n", "pkg_9e40\n", "package = \"pkg_9e40\"\n"} { // cut short, garbage, no app: fail closed
		h = &baseHost{pkg: unknownPackage}
		if _, errOut, code := fromCheckout(t, h, twoOthers(t), cut, "secret", "rm", "X"); code != exitError || len(h.asked) > 0 ||
			errOut != "\n  ✗ .canyon/base can't be used: it names no app. If this checkout belongs on "+staging+", run: rm .canyon/base\n" {
			t.Errorf("%q: exit %d, asked %q, stderr %q", cut, code, h.asked, errOut)
		}
	}
	h = &baseHost{pkg: unknownPackage}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), "app     = \"other-app\"\npackage = \"pkg_9e40\"\n", "secret", "rm", "X"); code != exitOK || h.checked() {
		t.Errorf("another app's base: exit %d, asked %q, stderr %q", code, h.asked, errOut)
	}
}

// A deploy whose --create makes the app skips the check: a new app has no package to find, and the
// upload accepts the base it doesn't know (#1678). On a host that has the app, --create changes
// nothing, and the check runs.
func TestCreateThatMakesTheAppSkipsTheCheck(t *testing.T) {
	h := &gateHost{read: lacks, pkg: unknownPackage, run: aRun("clean", 0, 0), page: buildPage(t, nil, true)}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "deploy", "--create"); code != exitOK || len(h.created) != 1 || h.base != "pkg_9e40" ||
		slices.Contains(h.asked, "GET /api/v1/projects/claims-portal/packages/pkg_9e40") {
		t.Errorf("a host without it: exit %d, made %q, base %q, asked %q, stderr %q", code, h.created, h.base, h.asked, errOut)
	}
	h = &gateHost{pkg: unknownPackage, run: aRun("clean", 0, 0)}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "deploy", "--create"); code != exitError || len(h.created) > 0 || h.uploaded() || errOut != refusedHere("deploy") {
		t.Errorf("a host with it: exit %d, made %q, asked %q, stderr %q", code, h.created, h.asked, errOut)
	}
}

// The check comes before any question: at a terminal, undeploy --env prod and delete print the
// refusal alone, never production's question or the typed name, and the answer typed is never read.
func TestTheCheckComesBeforeAnyQuestion(t *testing.T) {
	for _, c := range []struct {
		args        []string
		input, path string
	}{
		{[]string{"undeploy", "--env", "prod"}, "y\n", "undeploy --env prod"},
		{[]string{"delete"}, "claims-portal\n", "delete"},
	} {
		h := &baseHost{pkg: unknownPackage}
		out, errOut, code := inCheckout(t, h, twoOthers(t), recorded, c.input, true, c.args...)
		if code != exitError || out != "\n  canyon dev · claims-portal · canyon-staging.meridian.example\n" || errOut != refusedHere(c.path) || len(h.wrote()) > 0 ||
			!slices.Equal(h.asked, []string{"GET /api/v1/projects/claims-portal/packages/pkg_9e40"}) {
			t.Errorf("%q: exit %d, asked %q, stdout %q, stderr\n%s", c.args, code, h.asked, out, errOut)
		}
	}
}

// share with no grant only lists who can open the app: a read, so no check; a grant is checked.
func TestShareChecksTheBaseOnlyForAGrant(t *testing.T) {
	h := &baseHost{pkg: unknownPackage}
	if _, errOut, code := fromCheckout(t, h, twoOthers(t), recorded, "share"); code != exitOK || h.checked() {
		t.Errorf("the list: exit %d, asked %q, stderr %q", code, h.asked, errOut)
	}
}
