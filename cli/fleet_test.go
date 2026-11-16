package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// at is the instant every golden render is taken from, so the AGE column is the same every run.
var at = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// serveFixture answers the seam's own golden page verbatim and records the queries it was asked.
func serveFixture(t *testing.T) (base string, asked *[]string) {
	t.Helper()
	page, err := os.ReadFile("../contracts/testdata/fleet/page.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.RequestURI())
		_, _ = w.Write(page)
	}))
	t.Cleanup(server.Close)
	return server.URL, seen
}

// The whole rendering, byte for byte: row order, every column and REVIEW word, the ages, the four
// summary lines and the two hints.
func TestFleetRendersTheServersPageByteForByte(t *testing.T) {
	base, _ := serveFixture(t)
	page, err := readFleet(context.Background(), newClient(base), url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	want, err := os.ReadFile("testdata/fleet-human.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("canyon fleet rendered\n%s\ntestdata/fleet-human.txt wants\n%s", out.String(), want)
	}
}

// The filters are tiles the server selects; under one, the header speaks its words and the tiles go.
func TestTheFiltersAreTheServersAndTheTilesAreNot(t *testing.T) {
	base, asked := serveFixture(t)
	for flag, want := range map[string]string{"--unowned": "tile=unowned", "--never-reviewed": "tile=never_reviewed"} {
		if _, _, code := run(t, nil, false, "fleet", "--host", base, flag); code != exitOK {
			t.Errorf("canyon fleet %s exited %d", flag, code)
		}
		if got := (*asked)[len(*asked)-1]; !strings.Contains(got, want) {
			t.Errorf("canyon fleet %s asked for %q, want %q in it", flag, got, want)
		}
	}
	if _, err, code := run(t, nil, false, "fleet", "--host", base, "--unowned", "--never-reviewed"); code != exitError || !strings.Contains(err, "two tiles") {
		t.Errorf("both filters at once: exit %d, stderr %q", code, err)
	}
	counts := map[string]int{"never_reviewed": 1, "blocked": 2}
	for _, c := range []struct {
		filter         string
		want, unwanted []string
	}{
		{"unowned", []string{"0 apps with no owner", "No apps match"}, []string{"never reviewed", "apps blocked", "canyon fleet --"}},
		{"", []string{"0 apps", "! 1 app never reviewed", "✗ 2 apps blocked", "No apps yet"}, []string{"with no owner"}},
	} {
		var out strings.Builder
		(&app{out: &out}).renderFleet(fleetPage{Tiles: counts}, at, c.filter, "http://h")
		for _, want := range c.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("filter %q: want %q in\n%s", c.filter, want, out.String())
			}
		}
		for _, unwanted := range c.unwanted {
			if strings.Contains(out.String(), unwanted) {
				t.Errorf("filter %q printed %q:\n%s", c.filter, unwanted, out.String())
			}
		}
	}
}

// A read that fails is the verb's failure: exit 1, nothing on stdout, and the host's own words on
// stderr. Which kind of failure it was is the client's, and cli/client_test.go owns that.
func TestAFailedReadIsTheVerbsFailure(t *testing.T) {
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden","message":"this host is not yours to read"}`)
	}))
	t.Cleanup(forbidden.Close)
	if out, err, code := run(t, nil, false, "fleet", "--host", forbidden.URL); code != exitError || out != "" || !strings.Contains(err, "this host is not yours to read") {
		t.Errorf("a 403: exit %d, stdout %q, stderr %q", code, out, err)
	}
	// A page that fails mid-read fails the read: half a fleet is not a fleet.
	reads := 0
	mid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reads++; reads == 1 {
			_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":"second"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal","message":"internal error"}`)
	}))
	t.Cleanup(mid.Close)
	if out, err, code := run(t, nil, false, "fleet", "--host", mid.URL); code != exitError || out != "" || !strings.Contains(err, "internal error") {
		t.Errorf("a 500 on the second page: exit %d, stdout %q, stderr %q", code, out, err)
	}
	// No json rendering until §5.4, so the flag is refused rather than answered with prose.
	base, _ := serveFixture(t)
	if out, err, code := run(t, nil, false, "fleet", "--host", base, "--format", "json"); code != exitError || out != "" || !strings.Contains(err, "fleet has no json rendering") {
		t.Errorf("--format json: exit %d, stdout %q, stderr %q", code, out, err)
	}
	// Under CANYON_ASCII none of Canyon's own marks survive; a stack name is the org's data.
	table, _, _ := run(t, map[string]string{"CANYON_ASCII": "1"}, false, "fleet", "--host", base)
	for _, mark := range []string{"\u2713", "\u2717", "\u2014", "\u00b7 never"} { // not a bare \u00b7: a stack name has one
		if strings.Contains(table, mark) {
			t.Errorf("the table kept Canyon's %q under CANYON_ASCII:\n%s", mark, table)
		}
	}
}

// A host answering a cursor it has already answered would otherwise be followed forever.
func TestAPagedReadIsBounded(t *testing.T) {
	repeats, fresh := 0, 0
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		repeats++ // A, B, A: a cursor seen twice is the same page, however far apart
		_, _ = io.WriteString(w, `{"tiles":{},"apps":[],"next":"`+[]string{"A", "B"}[repeats%2]+`"}`)
	}))
	t.Cleanup(same.Close)
	if _, err := readFleet(context.Background(), newClient(same.URL), url.Values{}); err == nil || !strings.Contains(err.Error(), "the same page") {
		t.Errorf("a repeated cursor answered %v after %d reads", err, repeats)
	}
	forever := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fresh++
		_, _ = fmt.Fprintf(w, `{"tiles":{},"apps":[],"next":"page-%d"}`, fresh)
	}))
	t.Cleanup(forever.Close)
	if _, err := readFleet(context.Background(), newClient(forever.URL), url.Values{}); err == nil || !strings.Contains(err.Error(), "100 pages") {
		t.Errorf("an endless fleet answered %v after %d reads", err, fresh)
	}
	if repeats != 3 || fresh != maxFleetPages {
		t.Errorf("the bounds let through %d repeated and %d advancing reads", repeats, fresh)
	}
}

// A name is the org's data. printf cannot escape centrally — Canyon's colour codes are control
// bytes — so a cell is spelled out on the way into the table.
func TestServerStringsCannotDriveTheTerminal(t *testing.T) {
	nasty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tiles":{},"apps":[{"name":"claims\u001b[2Jportal","owners":[{"subject":"dana\u001b[2J@x"}],"stack":"Next\u001b[2J","gate_state":"gone\u001b[2J","envs":{},"latest":{"since":"2026-09-01T00:00:00Z"}},`+
			`{"name":"quote\u001b[2J","owners":[{"subject":"ines\u001b[2J@x"}],"summary":"Quotes\u001b[2J\nfor brokers","access":{"level":"viewer"}}],"next":null}`)
	}))
	t.Cleanup(nasty.Close)
	out, _, code := run(t, nil, false, "fleet", "--host", nasty.URL)
	if code != exitOK || strings.ContainsRune(out, '\x1b') || strings.Count(out, `\x1b[2J`) != 7 || !strings.Contains(out, `Quotes\x1b[2J\nfor brokers`) { // name, owner, stack, state; a reduced row's name, owner and one-line summary
		t.Errorf("a name carrying an escape byte rendered %q", out)
	}
	// A re-recorded golden must not be able to hide the most damning cell in the output.
	if cell, _ := (&app{}).gateCell("never_reviewed"); cell != "\u00b7 never reviewed" {
		t.Errorf("never_reviewed renders %q", cell)
	}
}

// The state word is the host's own. Every other cell on these screens is spelled out on the way in;
// this one is the default branch's, and both canyon fleet and canyon status print through it.
func TestAStateThisBuildDoesNotKnowCannotDriveTheTerminal(t *testing.T) {
	got, _ := (&app{}).gateCell("\x1b[2Jpwned")
	if strings.ContainsRune(got, 0x1b) || !strings.Contains(got, `\x1b[2Jpwned`) {
		t.Errorf("an unknown state rendered %q", got)
	}
}

// A member's fleet: the team's rows as always, then the rows read reduced — name, owner and summary,
// never a run, a stage or an env — each summary through cell(), "No description yet" when there is none.
func TestAMembersFleetShowsTheReducedRowsAsTheyAre(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/fleet/member.json")
	if err != nil {
		t.Fatal(err)
	}
	var page fleetPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	want, err := os.ReadFile("testdata/fleet-member.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("a member's canyon fleet rendered\n%s\ntestdata/fleet-member.txt wants\n%s", out.String(), want)
	}
	if spec := strings.TrimPrefix(specFence(t, "#### 6.2.1 The rows a member reads reduced"), "$ canyon fleet\n"); spec != string(want) {
		t.Errorf("a member's canyon fleet is not §6.2.1's transcript:\n%s", spec)
	}
	out.Reset() // a member on no team: the reduced table alone, never an empty team header
	page.Apps = page.Apps[3:]
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	if got := out.String(); strings.Contains(got, "STACK") || !strings.Contains(got, "  APP            OWNER           SUMMARY\n") {
		t.Errorf("a member on no team rendered\n%s", got)
	}
}

// An environment waiting on its first release runs nothing: the ENV column names only one with a url.
func TestAnEnvironmentWaitingOnItsFirstReleaseIsNoEnv(t *testing.T) {
	a := &app{out: io.Discard}
	waiting := json.RawMessage(`{"run_by":"org","pending":{"release":"rel_8f2a","event":"deploy","sent_at":"2026-09-02T10:00:00Z","state":"accepted"}}`)
	deployed := json.RawMessage(`{"url":"http://claims-portal.preview.localhost:7777","package":"pkg_7a1c","deployed_at":"2026-09-01T11:26:40Z"}`)
	if got, _ := a.env(fleetApp{Envs: map[string]json.RawMessage{"preview": waiting}}, at); got != a.dash() {
		t.Errorf("a preview waiting on its first release is ENV %q", got)
	}
	if got, _ := a.env(fleetApp{Envs: map[string]json.RawMessage{"prod": waiting, "preview": deployed}}, at); got != "preview" {
		t.Errorf("a prod waiting and a preview deployed is ENV %q", got)
	}
}

// The ENV column says what runs now, never what the last deploy recorded (#1241): a stopped
// environment says so in red, one the host could not check in amber, and only a running one is its
// name alone.
func TestTheEnvColumnSaysAStoppedAppStopped(t *testing.T) {
	a := &app{out: io.Discard}
	for state, want := range map[string][2]string{"running": {"prod", ""}, "restarting": {"prod restarting", ansiRed}, "stopped": {"prod stopped", ansiRed}, "unknown": {"prod couldn't check", ansiAmber}, "paused": {"prod couldn't check", ansiAmber}} {
		prod := json.RawMessage(`{"url":"http://claims-archive.prod.localhost:7777","package":"pkg_8b21","deployed_at":"2026-08-24T13:55:02Z","run_by":"canyon","state":"` + state + `"}`)
		if got, colour := a.env(fleetApp{Envs: map[string]json.RawMessage{"prod": prod}}, at); got != want[0] || colour != want[1] {
			t.Errorf("a prod that is %s is ENV %q in %q, want %q in %q", state, got, colour, want[0], want[1])
		}
	}
	prod := json.RawMessage(`{"url":"http://claims-archive.prod.localhost:7777","package":"pkg_8b21","run_by":"canyon","state":"stopped"}`)
	preview := json.RawMessage(`{"url":"http://claims-archive.preview.localhost:7777","package":"pkg_9c10","run_by":"canyon","state":"running"}`)
	if got, colour := a.env(fleetApp{Envs: map[string]json.RawMessage{"prod": prod, "preview": preview}}, at); got != "prod stopped" || colour != ansiRed {
		t.Errorf("a stopped prod beside a running preview is ENV %q in %q: the column names prod, and says it stopped", got, colour)
	}
}

// D1: an app whose every blocker is approved reads approved, amber, never ✗ blocked, and the
// approved tile has its own line, so it never drops out of view (contracts/routes.md, canyonv2-5r11.10).
func TestAnApprovedAppReadsApprovedAndKeepsItsOwnLine(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/fleet/page.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["tiles"].(map[string]any)["approved"] = 1
	for _, row := range doc["apps"].([]any) {
		if row := row.(map[string]any); row["id"] == "claims-portal" {
			row["latest"].(map[string]any)["stage"] = "approved"
		}
	}
	body, _ := json.Marshal(doc) // decoded JSON: nothing in it can fail to marshal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(server.Close)
	page, err := readFleet(context.Background(), newClient(server.URL), url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	got, claims := out.String(), ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "  claims-portal ") {
			claims = line
		}
	}
	if !strings.Contains(got, "\n  ! 1 app cleared with an exception\n") || !strings.Contains(claims, "! approved") || strings.Contains(claims, "blocked") {
		t.Errorf("an approved app rendered\n%s", got)
	}
}

// An owner granted by address before they ever signed in has no subject yet but a display — the
// address — and the OWNER column names them, never "—" (codex on #587).
func TestAnOwnerNotYetSignedInIsNamed(t *testing.T) {
	row := fleetApp{Owners: []struct{ Subject, Display string }{{Subject: "", Display: "nora@meridian.example"}}}
	if got := (&app{}).owner(row); got != "nora@meridian.example" {
		t.Errorf("owner %q", got)
	}
}

// Names repeat and ids never do: two apps alike in a list say their ids too, and one alone says its
// name alone (#1911).
func TestTwoAppsAlikeInTheFleetSayTheirIds(t *testing.T) {
	page := fleetPage{Apps: []fleetApp{{ID: "tidy-finch-euk", Name: "claims-app"}, {ID: "funny-cloud-xij", Name: "claims-app"}, {ID: "calm-fox-1ab", Name: "Expense tracker"}}}
	var out strings.Builder
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	for _, want := range []string{"claims-app (tidy-finch-euk)", "claims-app (funny-cloud-xij)", "Expense tracker "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the fleet does not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "calm-fox-1ab") {
		t.Errorf("an app alone in its name was given its id:\n%s", out.String())
	}
}

// The ENV column says where the org's pods stopped checking in, or never began (#2455; cli.md §6.2):
// `<env> no check-in`, amber, by the one rule canyon status reads (contracts/routes.md, envs.<env>.seen_at) —
// ledger-sync's last check-in 20 minutes ago and claims-intake's report 30 minutes ago with none. A fresh
// check-in, a report within its first 10 minutes, a pod still pulling while its environment is removed and
// an environment Canyon runs keep the name alone; the row read reduced, a read-only member's, prints no ENV.
func TestFleetSaysWherePodsStoppedCheckingIn(t *testing.T) {
	raw, err := os.ReadFile("testdata/fleet-checkin.json")
	if err != nil {
		t.Fatal(err)
	}
	var page fleetPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	(&app{out: &out}).renderFleet(page, at, "", "http://localhost:7777")
	want, err := os.ReadFile("testdata/fleet-checkin.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("canyon fleet rendered\n%s\ntestdata/fleet-checkin.txt wants\n%s", out.String(), want)
	}
	var painted strings.Builder
	(&app{out: &painted, render: Render{Color: true}}).renderFleet(page, at, "", "http://localhost:7777")
	for _, word := range []string{"prod no check-in", "preview no check-in"} {
		if !strings.Contains(painted.String(), ansiAmber+word) {
			t.Errorf("%q is not amber:\n%q", word, painted.String())
		}
	}
}
