package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// accessFixture is the Access group's contract golden (contracts/testdata/access), served whole: a
// field renamed on either side turns this suite red instead of printing a blank column.
type accessFixture struct {
	Read  json.RawMessage `json:"read"`
	Share struct {
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	} `json:"share"`
	TeamRequired  json.RawMessage `json:"team_required"`
	OwnerRequired json.RawMessage `json:"owner_required"`
}

func accessGolden(t *testing.T) accessFixture {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/access/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var f accessFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// §6.7's transcript is the golden: the fixture's team, the org /auth/config names, two hours after
// the request arrived.
func TestShareListIsTheSpecsTranscript(t *testing.T) {
	var read accessRead
	if err := json.Unmarshal(accessGolden(t).Read, &read); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	(&app{out: &out}).renderAccess("claims-portal", "Meridian", read, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	if want := pastHeader(strings.TrimPrefix(specFence(t, "### 6.7 `canyon share`"), "$ canyon share\n")); out.String() != want {
		t.Errorf("canyon share rendered\n%s\n§6.7 of design/specs/cli.md is\n%s", out.String(), want)
	}
}

// A name longer than the column's 29 widens it for every row of its list, so each level lines up
// under the one above (on the test cluster, a 30-character address sat two spaces from owner while
// everyone's row was padded to 29).
func TestShareRowsLineUpUnderTheLongestName(t *testing.T) {
	var read accessRead
	must(t, json.Unmarshal([]byte(`{"entries":[{"kind":"user","grantee":"sam.nightingale@claims.example.com","level":"owner","roles":[]}],"everyone":{"roles":[]},"requests":[{"email":"kim@example.com","requested_at":"2026-09-02T08:00:00Z"}]}`), &read))
	var out strings.Builder
	(&app{out: &out}).renderAccess("claims-portal", "Example Co", read, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	want := "\n  claims-portal\n\n  sam.nightingale@claims.example.com  owner\n  everyone at Example Co              read-only\n" +
		"\n  Requests:\n  kim@example.com                2h ago        canyon share kim@example.com\n" // its own list, its own width
	if out.String() != want {
		t.Errorf("the rows read\n%q\nwant\n%q", out.String(), want)
	}
}

// A read-only member's request (#2774) is answered at contributor: the hint a plain share would give adds them
// read-only again and changes nothing. A request with no level, from before Canyon recorded it, keeps the plain hint.
func TestAReadOnlyMembersRequestIsAnsweredAtContributor(t *testing.T) {
	var read accessRead
	must(t, json.Unmarshal([]byte(`{"entries":[],"requests":[{"email":"vic@example.com","requested_at":"2026-09-02T08:00:00Z","level":"viewer"},`+
		`{"email":"nora@example.com","requested_at":"2026-09-02T09:00:00Z","level":"none"},{"email":"old@example.com","requested_at":"2026-09-02T09:30:00Z","level":null}]}`), &read))
	var out strings.Builder
	(&app{out: &out}).renderAccess("claims-portal", "Example Co", read, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	want := "\n  Requests:\n  vic@example.com                2h ago        canyon share vic@example.com --level contributor\n" +
		"  nora@example.com               1h ago        canyon share nora@example.com\n  old@example.com                30m ago       canyon share old@example.com\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("the requests read\n%q\nwant them to end\n%q", out.String(), want)
	}
}

// shareHost answers /auth/config and every access route from the golden, recording each request.
type shareHost struct {
	auth   string
	refuse json.RawMessage // answered 403 to every access route when set
	noRead bool            // the project read fails, after the grant has landed
	fix    accessFixture
	asked  []string
}

func (h *shareHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.asked = append(h.asked, strings.TrimSpace(r.Method+" "+r.URL.EscapedPath()+" "+string(body)))
	switch {
	case r.URL.Path == "/auth/config":
		_, _ = fmt.Fprintf(w, `{"canyon":true,"auth":%q,"org":"Meridian","device":true,"cli":"","public":""}`, h.auth)
	case h.refuse != nil:
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(h.refuse)
	case !strings.HasSuffix(r.URL.Path, "/access") && !strings.Contains(r.URL.Path, "/access/") && h.noRead:
		w.WriteHeader(http.StatusInternalServerError)
	case !strings.HasSuffix(r.URL.Path, "/access") && !strings.Contains(r.URL.Path, "/access/"): // the project read: where the app runs
		_, _ = w.Write([]byte(`{"project":{"envs":{"preview":{"url":"http://claims-portal.preview.localhost:7777"},"prod":{"run_by":"org","pending":{}}}}}`))
	case r.Method == http.MethodPut:
		_, _ = w.Write(h.fix.Share.Response)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		_, _ = w.Write(h.fix.Read)
	}
}

func sharing(t *testing.T, h *shareHost, args ...string) (out, errOut string, code int) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	out, errOut, code = run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, append([]string{"share"}, args...)...)
	return pastHeader(out), errOut, code
}

// Each form is one request of the contract's, the level named in the words screens use and sent as
// the API's (read-only is viewer), and the screen after it the team the host answered.
func TestEachShareFormIsTheContractsRequest(t *testing.T) {
	fix := accessGolden(t)
	var request bytes.Buffer
	must(t, json.Compact(&request, fix.Share.Request))
	const project = "/api/v1/projects/claims-portal"
	const at = project + "/access"
	for _, c := range []struct {
		args  []string
		asked []string
	}{
		{nil, []string{"GET " + at}},
		{[]string{"sam@meridian.example", "--role", "auditor"}, []string{"PUT " + at + "/email/sam@meridian.example " + request.String(), "GET " + project}},
		{[]string{"group:ops", "--level", "contributor"}, []string{"PUT " + at + `/group/ops {"level":"contributor","roles":[]}`, "GET " + project}},
		{[]string{"group:claims/eu"}, []string{"PUT " + at + `/group/claims%2Feu {"level":"viewer","roles":[]}`, "GET " + project}}, // one path segment, whatever the name holds
		{[]string{"everyone", "--role", "auditor"}, []string{"PUT " + at + `/everyone/everyone {"level":"viewer","roles":["auditor"]}`, "GET " + project}},
		{[]string{"--remove", "group:claims-engineering"}, []string{"DELETE " + at + "/group/claims-engineering", "GET " + at}},
		{[]string{"--app", "quote-engine", "--level", "owner", "ines@meridian.example"}, []string{`PUT /api/v1/projects/quote-engine/access/email/ines@meridian.example {"level":"owner","roles":[]}`, "GET /api/v1/projects/quote-engine"}},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			h := &shareHost{auth: "oidc", fix: fix}
			out, errOut, code := sharing(t, h, c.args...)
			if code != exitOK || errOut != "" || !strings.Contains(out, "everyone at Meridian") {
				t.Errorf("exit %d, stderr %q, stdout\n%s", code, errOut, out)
			}
			if want := append([]string{"GET /auth/config"}, c.asked...); !slices.Equal(h.asked, want) {
				t.Errorf("asked\n  %q\nwant\n  %q", h.asked, want)
			}
		})
	}
}

// An app's own sign-in learns of a share about a minute after the dashboard does (decision D2), so a
// share says so, with the address of each environment that runs; a removal says how long it lingers.
func TestAShareSaysWhenItReachesTheAppAndWhere(t *testing.T) {
	fix := accessGolden(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"sam@meridian.example"}, "\n  Shared with sam@meridian.example. They can open it within about a minute:\n    http://claims-portal.preview.localhost:7777\n\n  claims-portal\n"},
		{[]string{"group:ops"}, "\n  Shared with group:ops. They can open it within about a minute:\n    http://claims-portal.preview.localhost:7777\n"},
		{[]string{"everyone"}, "\n  Shared with everyone at Meridian. They can open it within about a minute:\n"},
		{[]string{"--remove", "sam@meridian.example"}, "\n  Removed sam@meridian.example. The app may still let them in for up to a minute.\n\n  claims-portal\n"},
	} {
		h := &shareHost{auth: "oidc", fix: fix}
		if out, _, code := sharing(t, h, c.args...); code != exitOK || !strings.HasPrefix(out, c.want) {
			t.Errorf("share %q: exit %d, want it to open %q:\n%s", c.args, code, c.want, out)
		}
	}
}

// A share that landed says so even when the read of where the app runs then fails: the grant is what
// was asked for, and a failure after it must not read as a share that did not happen.
func TestAShareThatLandedSaysSoWhenTheReadAfterItFails(t *testing.T) {
	h := &shareHost{auth: "oidc", noRead: true, fix: accessGolden(t)}
	out, errOut, code := sharing(t, h, "sam@meridian.example")
	if code != exitOK || errOut != "" || !strings.HasPrefix(out, "\n  Shared with sam@meridian.example.\n") {
		t.Errorf("exit %d, stderr %q, stdout\n%s", code, errOut, out)
	}
}

// A flag that would change nothing, a level the screens do not say, and everyone at any level but
// read-only are refused before the host is asked anything.
func TestAShareThatCannotMeanOneThingAsksNothing(t *testing.T) {
	fix := accessGolden(t) // before a case moves into its project directory
	for _, args := range [][]string{
		{"everyone", "--level", "read-only"},
		{"sam@meridian.example", "--level", "viewer"},
		{"--remove", "sam@meridian.example", "--level", "owner"},
		{"--remove", "sam@meridian.example", "dana@meridian.example"},
		{"--role", "auditor"},
		{"group:"},
		{"sam@meridian.example", "dana@meridian.example"},
	} {
		h := &shareHost{auth: "oidc", fix: fix}
		if out, errOut, code := sharing(t, h, args...); code != exitError || out != "" || errOut == "" || len(h.asked) != 0 {
			t.Errorf("share %q: exit %d, stdout %q, stderr %q, asked %q", args, code, out, errOut, h.asked)
		}
	}
}

// A host without sign-in has no team to show or change (contracts/routes.md, Access); a caller who
// does not own the app gets the host's own line.
func TestNoSignInAndNotAnOwnerAreOneLineAndExitOne(t *testing.T) {
	fix := accessGolden(t)
	h := &shareHost{auth: "off", fix: fix}
	out, errOut, code := sharing(t, h)
	if code != exitError || out != "" || errOut != "\n  Sign-in is off on this Canyon, so anyone who can reach it can open every app. Turn sign-in on to share with specific people.\n" || len(h.asked) != 1 {
		t.Errorf("auth off: exit %d, stdout %q, stderr %q, asked %q", code, out, errOut, h.asked)
	}
	h = &shareHost{auth: "oidc", fix: fix, refuse: fix.OwnerRequired}
	out, errOut, code = sharing(t, h, "sam@meridian.example")
	if code != exitError || out != "" || errOut != "\n  ✗ only the owners of claims-portal and admins can do this: ask Dana Whitfield\n" {
		t.Errorf("owner_required: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// §9's line: the first owner (else a member of the owning group, else an admin) and the command adding the session's address as a
// contributor on the refused app; with no address, the host's own sentence.
func TestTeamRequiredNamesTheOwnerAndTheCommandTheyRun(t *testing.T) {
	refusal := accessGolden(t).TeamRequired
	spec, err := os.ReadFile("../design/specs/cli.md") // before a case moves into its project directory
	must(t, err)
	ownerless := []byte(`{"error":"team_required","message":"Ask an admin to add you to claims-portal","owner_groups":[],"owners":[]}`)
	grouped := []byte(`{"error":"team_required","message":"Ask a member of the claims-team group to add you to claims-portal","owner_groups":["claims-team"],"owners":[]}`)
	for _, c := range []struct {
		refusal []byte
		prefs   int
		want    string
	}{
		{refusal, http.StatusOK, "Ask Dana Whitfield, an owner of claims-portal, to run: canyon share tomas@meridian.example --level contributor --app claims-portal"},
		{ownerless, http.StatusOK, "Ask an admin to run: canyon share tomas@meridian.example --level contributor --app claims-portal"},
		{grouped, http.StatusOK, "Ask a member of the claims-team group, which owns claims-portal, to run: canyon share tomas@meridian.example --level contributor --app claims-portal"},
		{refusal, http.StatusInternalServerError, "Ask Dana Whitfield to add you to claims-portal"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/preferences" {
				w.WriteHeader(c.prefs)
				_, _ = w.Write([]byte(`{"viewer":{"email":"tomas@meridian.example","role":"member"}}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(c.refusal)
		}))
		t.Chdir(projectDir(t))
		out, errOut, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		if code != exitError || out != "" || errOut != "\n  ✗ "+c.want+"\n" {
			t.Errorf("exit %d, stdout %q, stderr %q; want %q", code, out, errOut, c.want)
		}
	}
	if !strings.Contains(string(spec), "\n  ✗ Ask Dana Whitfield, an owner of claims-portal, to run: canyon share tomas@meridian.example --level contributor --app claims-portal\n") {
		t.Errorf("§9 of design/specs/cli.md does not print the line this renders")
	}
}

// A verb whose route needs owner — share, rename, delete — gets the owners' line on team_required: being added as a
// contributor would only trade it for owner_required (§9; #2357). A contributor's verb keeps the contributor line.
func TestTeamRequiredOnAnOwnersVerbNamesWhoMayDoIt(t *testing.T) {
	refusal := accessGolden(t).TeamRequired
	spec, err := os.ReadFile("../design/specs/cli.md") // before a case moves into its project directory
	must(t, err)
	ownerless := []byte(`{"error":"team_required","message":"Ask an admin to add you to claims-portal","owner_groups":[],"owners":[]}`)
	grouped := []byte(`{"error":"team_required","message":"Ask a member of the claims-team group to add you to claims-portal","owner_groups":["claims-team"],"owners":[]}`)
	for _, c := range []struct {
		refusal []byte
		args    []string
		want    string
	}{
		{refusal, []string{"rename", "Claims portal"}, "only the owners of claims-portal and admins can do this: ask Dana Whitfield"},
		{refusal, []string{"delete"}, "only the owners of claims-portal and admins can do this: ask Dana Whitfield"},
		{refusal, []string{"share", "sam@meridian.example"}, "only the owners of claims-portal and admins can do this: ask Dana Whitfield"},
		{grouped, []string{"rename", "Claims portal"}, "only the owners of claims-portal and admins can do this: ask a member of the claims-team group"},
		{ownerless, []string{"rename", "Claims portal"}, "only the owners of claims-portal and admins can do this: ask an admin"},
		{refusal, []string{"secret", "set", "PLAIN_SETTING=on"}, "Ask Dana Whitfield, an owner of claims-portal, to run: canyon share tomas@meridian.example --level contributor --app claims-portal"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
				return
			case "/api/v1/preferences":
				_, _ = w.Write([]byte(`{"viewer":{"email":"tomas@meridian.example","role":"member"}}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(c.refusal)
		}))
		t.Chdir(projectDir(t))
		out, errOut, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, c.args...)
		server.Close()
		if code != exitError || !strings.HasSuffix(errOut, "\n  ✗ "+c.want+"\n") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want %q", c.args, code, out, errOut, c.want)
		}
	}
	if !strings.Contains(string(spec), "\n  ✗ only the owners of claims-portal and admins can do this: ask Dana Whitfield\n") {
		t.Errorf("§9 of design/specs/cli.md does not print the line this renders")
	}
}

// A stopped verb asks nothing more, and a refused run route, which names no app, keeps the host's sentence.
func TestAskToJoinAsksWithinTheVerbAndNamesTheRefusedApp(t *testing.T) {
	var asked int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		_, _ = w.Write([]byte(`{"viewer":{"email":"tomas@meridian.example","role":"member"}}`))
	}))
	defer server.Close()
	var refusal APIError
	must(t, json.Unmarshal(accessGolden(t).TeamRequired, &refusal.Extra))
	refusal.Message, refusal.Path = "Ask Dana Whitfield to add you to claims-portal", "/api/v1/projects/claims-portal/status"
	a := &app{http: newClient(server.URL)}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if got := a.askToJoin(stopped, &refusal); got != refusal.Message || asked != 0 {
		t.Errorf("a stopped verb asked %d times and printed %q", asked, got)
	}
	refusal.Path = "/api/v1/gate-runs/gr_8f2a/timeline"
	if got := a.askToJoin(context.Background(), &refusal); got != refusal.Message {
		t.Errorf("a refusal naming no app printed %q", got)
	}
}
