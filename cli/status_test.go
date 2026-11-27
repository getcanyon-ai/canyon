package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// page is the app-page contract fixture, whole: a fixture edited to suit the renderer proves nothing about the renderer.
func page(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(pkgDir, "..", "contracts", "testdata", "app-page", "claims-portal.json")) // from any directory a verb runs in
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(body, &whole); err != nil {
		t.Fatal(err)
	}
	return whole
}

// reads decodes the fixture's status read and the newest row of its gate-runs list: the two answers the verb asks for.
func reads(t *testing.T, whole map[string]json.RawMessage, key string) (statusRead, *runRow) {
	t.Helper()
	var s statusRead
	var pages []struct {
		Runs []runRow `json:"runs"`
	}
	if json.Unmarshal(whole[key], &s) != nil || json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatalf("the fixture's %s or gate_runs is not the shape the CLI decodes", key)
	}
	// The project read's access for the access golden's team (contracts/testdata/access): shared with
	// everyone, one group, and no person below owner; the reader is its owner.
	s.Access = &teamAccess{Level: "owner", Everyone: true, Groups: 1}
	return s, &pages[0].Runs[0]
}

func rendered(t *testing.T, s statusRead, latest *runRow) string {
	t.Helper()
	var out strings.Builder
	(&app{out: &out}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	return out.String()
}

func wants(t *testing.T, name, got string) {
	t.Helper()
	if want := golden(t, name); got != want {
		t.Errorf("canyon status rendered\n%s\ntestdata/%s wants\n%s", got, name, want)
	}
}

func TestStatusRendersTheServersRowsAndReceiptByteForByte(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	wants(t, "status.txt", rendered(t, s, latest))
	if spec := strings.TrimPrefix(specFence(t, "### 6.1 `canyon status`"), "$ canyon status\n"); rendered(t, s, latest) != spec {
		t.Errorf("canyon status is not §6.1's transcript:\n%s", spec)
	}
}

// §6.1's access line: how far the project read says the app is shared, then the reader's level on
// it, else admin when the fleet read's viewer says so; a read that carries no access prints none.
func TestTheAccessLineIsTheProjectReadAndTheReadersPlace(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	for _, c := range []struct {
		team  *teamAccess
		admin bool
		want  string
	}{
		{&teamAccess{Level: "contributor", People: 3, Groups: 1}, false, "  access       shared with 3 people and 1 group · you are a contributor\n"},
		{&teamAccess{Level: "owner", People: 1}, false, "  access       shared with 1 person · you are an owner\n"},
		{&teamAccess{Level: "none"}, true, "  access       private · you are an admin\n"},
		{&teamAccess{Level: "owner"}, true, "  access       private · you are an owner\n"},
	} {
		s.Access, s.Admin = c.team, c.admin
		if out := rendered(t, s, latest); !strings.Contains(out, "not promoted yet\n"+c.want+"  review ") {
			t.Errorf("%+v, admin %v: want %q after the environments in\n%s", c.team, c.admin, c.want, out)
		}
	}
	s.Access = nil
	if out := rendered(t, s, latest); strings.Contains(out, "access") {
		t.Errorf("a read with no access printed a line:\n%s", out)
	}
}

// routes.md row 117: the promotion's object IS the project read's envs.prod, so nothing is invented for this row.
func TestAPromotedProdRowIsTheFixturesPromotion(t *testing.T) {
	whole := page(t)
	s, latest := reads(t, whole, "status")
	var promoted struct {
		Promotion env `json:"promotion"`
	}
	if json.Unmarshal(whole["promotion"], &promoted) != nil {
		t.Fatal("the fixture's promotion is not an env object")
	}
	s.Envs["prod"] = &promoted.Promotion
	wants(t, "status-prod.txt", rendered(t, s, latest))
}

// Three reads, and the fleet's viewer only for a reader who is neither owner nor contributor; a
// blocked gate is still exit 0: status is a read, and check and deploy carry the verdicts.
func TestStatusReadsItsAnswersAndExitsZeroOnABlockedGate(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	for _, level := range []string{"owner", "none"} {
		var asked []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.RequestURI())
			switch {
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write(pages[0])
			case r.URL.Path == "/api/v1/projects/claims-portal":
				_, _ = fmt.Fprintf(w, `{"project":{"id":"claims-portal","access":{"level":%q,"everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`, level)
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
			case r.URL.Path == "/api/v1/fleet":
				_, _ = w.Write([]byte(`{"apps":[],"next":null,"viewer":{"display":"Ada","role":"admin"}}`))
			default:
				_, _ = w.Write(whole["status"])
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		if code != exitOK || errs != "" {
			t.Errorf("a blocked gate: exit %d, stderr %q", code, errs)
		}
		want := []string{"/api/v1/projects/claims-portal", "/api/v1/projects/claims-portal/status", "/api/v1/projects/claims-portal/gate-runs?limit=200", "/auth/config", "/api/v1/front-door"}
		you := map[string]string{"owner": "an owner", "none": "an admin"}[level]
		// the viewer's role, every reader's: only an admin is sent the scanners' data read, which audits a refusal (#2743)
		want = append(want, "/api/v1/fleet?limit=1", "/api/v1/projects/claims-portal/egress", "/api/v1/egress", "/api/v1/scanners/data")
		if !slices.Equal(asked, want) || !strings.Contains(out, "access       private · you are "+you+"\n") {
			t.Errorf("level %s: status asked for %q, want %q, and printed\n%s", level, asked, want, out)
		}
	}
}

// --format json is one object on stdout and nothing else: what runs where, the app's own newest run and
// the receipt, as the host answered them (cli.md §5.4), so CI asks "is it live, what blocks production".
func TestStatusAsJSONIsTheReadsAsTheHostAnsweredThem(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gate-runs"):
			_, _ = w.Write(pages[0])
		case r.URL.Path == "/api/v1/projects/claims-portal":
			_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`))
		case r.URL.Path == "/auth/config":
			_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
		default:
			_, _ = w.Write(whole["status"])
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	t.Chdir(dir)
	out, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status", "--format", "json")
	var got struct {
		App     string                     `json:"app"`
		Envs    map[string]json.RawMessage `json:"envs"`
		LastRun struct{ ID, State string } `json:"last_run"`
		Receipt struct {
			BlockedBy []string `json:"blocked_by"`
		} `json:"receipt"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if code != exitOK || errs != "" || dec.Decode(&got) != nil || dec.More() || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s", code, errs, out)
	}
	var want struct {
		LastRun *string `json:"last_run"`
		Receipt struct {
			BlockedBy []string `json:"blocked_by"`
		} `json:"receipt"`
	}
	_ = json.Unmarshal(whole["status"], &want)
	if got.App != "claims-portal" || want.LastRun == nil || got.LastRun.ID != *want.LastRun || got.LastRun.State == "" || !slices.Equal(got.Receipt.BlockedBy, want.Receipt.BlockedBy) || len(got.Envs) == 0 {
		t.Fatalf("status as json: %+v\n%s", got, out)
	}
}

// The server's four rules, in the server's order and words: nothing counts, reorders or filters, and what passed sits beside what didn't (D6).
func TestTheReceiptIsTheServersWordsInTheServersOrder(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	r := s.Receipt.Rules
	r[2].Unset, r[2].Why = false, "Next.js 15 is not on the approved list" // a list set, and this app off it: advisory, failed
	s.Receipt.Rules = []receiptRule{r[2], r[0], r[3], r[1]}                // an advisory first: an order no sort by `blocks` picks
	want := slices.Clone(s.Receipt.Rules)                                  // the expectation, taken before anything renders
	out, seen := rendered(t, s, latest), -1
	for _, rule := range want {
		for _, text := range append([]string{rule.Name}, wrap(cell(rule.Why), proseWidth-6)...) {
			next := strings.Index(out, text)
			if next < 0 || next < seen {
				t.Errorf("%q is missing or out of the server's order in\n%s", text, out)
			}
			seen = next
		}
	}
	if n := strings.Count(out, advisoryChip); n != 1 { // one rule failed and blocks nothing
		t.Errorf("the advisory chip is on %d rows in\n%s", n, out)
	}
	for i := range s.Receipt.Rules { // and with nothing advisory the chip's column is not there at all
		s.Receipt.Rules[i].Passed = true
	}
	if out := rendered(t, s, latest); strings.Contains(out, advisoryChip) ||
		!strings.Contains(out, "✓ Every app has an owner\n") {
		t.Errorf("a chip column with no advisory in it:\n%s", out)
	}
}

// blockedStatus is the fixture's status read with preview's own run blocked: the refusal's clean-gate
// row in place of preview's passing one.
func blockedStatus(t *testing.T) (statusRead, *runRow) {
	t.Helper()
	whole := page(t)
	s, latest := reads(t, whole, "status")
	var refusal struct {
		Receipt receipt `json:"receipt"`
	}
	if json.Unmarshal(whole["refusal"], &refusal) != nil {
		t.Fatal("the fixture's refusal is not the shape the CLI decodes")
	}
	s.Receipt.BlockedBy, s.Receipt.Rules[0] = refusal.Receipt.BlockedBy, refusal.Receipt.Rules[0] // prod.clean-gate leads, in the snapshot's order
	return s, latest
}

// Red is a blocker: a rule that failed and blocks nothing is never red, and the line that blocks is.
func TestOnlyABlockingRuleIsRed(t *testing.T) {
	s, latest := blockedStatus(t)
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatHuman, Color: true}}).renderStatus("claims-portal", "http://h", s, latest, at)
	for _, line := range strings.Split(out.String(), "\n") {
		red := strings.Contains(line, ansiRed)
		if strings.Contains(line, "Approved stacks only") && red {
			t.Errorf("the failed advisory is red: %q", line)
		}
		if strings.Contains(line, "Production needs a passing review") && !red {
			t.Errorf("the rule that blocks is not red: %q", line)
		}
	}
}

// The lead line is prod.clean-gate's and no other rule's (Receipt.tsx:26); the org is the server's in the header and in that sentence alike.
func TestTheLeadLineAndTheOrgAreTheServersOrAbsent(t *testing.T) {
	s, latest := blockedStatus(t)
	if !strings.Contains(rendered(t, s, latest), "Meridian requires a passing review to reach production.") {
		t.Errorf("no lead line while prod.clean-gate blocks")
	}
	s.Receipt.Org = "Northwind Mutual" // the same receipt, blocked the same way, under another org
	out := rendered(t, s, latest)
	if strings.Contains(out, "Meridian") || !strings.Contains(out, "claims-portal · Northwind Mutual") ||
		!strings.Contains(out, "Northwind Mutual requires a passing review to reach production.") {
		t.Errorf("the header and the lead line do not carry the org the server named:\n%s", out)
	}
	s.Receipt.BlockedBy = nil
	if out := rendered(t, s, latest); strings.Contains(out, "requires a passing review") {
		t.Errorf("a lead line with nothing blocked by prod.clean-gate:\n%s", out)
	}
}

// A `pkg_` id is the store's prefix, no name a person says (contracts/routes.md), and nothing builds a URL for an absent environment.
func TestAVersionIsNeverAPkgIdAndAnAbsentEnvIsNeverAURL(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	s.Envs["preview"].Label = "" // an unlabelled package: the id is all there is to say
	s.Envs["preview"].Package = "pkg_5c02"
	out := rendered(t, s, latest)
	if strings.Contains(out, "pkg_") || !strings.Contains(out, "5c02") {
		t.Errorf("an unlabelled package rendered as\n%s", out)
	}
	if !strings.Contains(out, "not promoted yet") || strings.Contains(out, ".prod.") {
		t.Errorf("an absent prod row built a URL:\n%s", out)
	}
	s.Envs = nil
	if out := rendered(t, s, nil); !strings.Contains(out, "nothing running") || !strings.Contains(out, "· never") {
		t.Errorf("an app with nothing deployed and no run rendered\n%s", out)
	}
}

// Below contributor, status reads the project and no more: a read-only member is shown what its app
// page shows them — name, owners, summary, then EXPERIENCE D5's line — and exits 0; someone with no
// access is refused as every contributor read refuses them, with §9's team_required line. The rows are
// the member fixture's reduced ones, whole.
func TestBelowContributorStatusIsTheReducedPageOrTheRefusal(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/fleet/member.json")
	if err != nil {
		t.Fatal(err)
	}
	var fleet struct{ Apps []json.RawMessage }
	if err := json.Unmarshal(raw, &fleet); err != nil || len(fleet.Apps) != 5 {
		t.Fatalf("the member fixture: %d apps (%v)", len(fleet.Apps), err)
	}
	readOnly := golden(t, "status-read-only.txt")
	if spec := strings.TrimPrefix(specFence(t, "#### 6.1.1 Below contributor"), "$ canyon status\n"); spec != readOnly {
		t.Errorf("the read-only page is not §6.1.1's transcript:\n%s", spec)
	}
	for app, c := range map[string]struct {
		row      json.RawMessage
		code     int
		out, err string
		asked    []string
	}{
		"claims-portal": {fleet.Apps[3], exitOK, readOnly, "", []string{"/api/v1/projects/claims-portal"}},
		"quote-engine": {fleet.Apps[4], exitError, "", "\n  ✗ Ask an admin to run: canyon share tomas@meridian.example --level contributor --app quote-engine\n",
			[]string{"/api/v1/projects/quote-engine", "/api/v1/projects/quote-engine/status", "/api/v1/preferences"}},
	} {
		var asked []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.RequestURI())
			switch r.URL.Path {
			case "/api/v1/projects/" + app:
				_, _ = fmt.Fprintf(w, `{"project":%s}`, c.row)
			case "/api/v1/preferences":
				_, _ = w.Write([]byte(`{"viewer":{"email":"tomas@meridian.example"}}`))
			default: // every contributor read, as the host refuses it
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"error":"team_required","message":"Ask an admin to add you to %s","owners":[]}`, app)
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \""+app+"\"\n")
		t.Chdir(dir)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		if code != c.code || out != c.out || errs != c.err || !slices.Equal(asked, c.asked) {
			t.Errorf("%s: exit %d, asked %q\n%s\nstderr %q", app, code, asked, out, errs)
		}
	}
}

// The reduced page is the org's data too: an owner's name or a summary cannot drive the terminal, and a
// summary stays one line.
func TestTheReducedPageCannotDriveTheTerminal(t *testing.T) {
	var out strings.Builder
	(&app{out: &out}).renderReduced("claims\x1b[2J", []actorRef{{Display: "Dana\x1b[2J"}, {Display: "Priya"}}, "Files\x1b[2J\nclaims")
	if got := out.String(); strings.ContainsRune(got, 0x1b) || !strings.Contains(got, "owners       Dana\\x1b[2J, Priya\n") || !strings.Contains(got, "summary      Files\\x1b[2J\\nclaims\n") {
		t.Errorf("the reduced page rendered %q", got)
	}
	out.Reset() // an app with no owner and no description, which a grant to everyone can still open
	(&app{out: &out}).renderReduced("x", nil, "")
	if got := out.String(); !strings.Contains(got, "owners       —\n") || !strings.Contains(got, "summary      No description yet\n") {
		t.Errorf("an ownerless app with no summary rendered %q", got)
	}
}

// An environment waiting on its first release (release.schema.json's env_pending alone) is said by
// the release it waits on, routes.md's lines, and never as a URL or a version it does not run yet.
func TestAnEnvironmentWaitingOnItsFirstReleaseIsSaidByItsRelease(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	sentAt := "2026-09-02T10:00:00Z"
	at, err := time.Parse(time.RFC3339, sentAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ event, state, line string }{
		{"deploy", "accepted", "sent to your platform at " + at.Local().Format("15:04") + ", not running yet"},
		{"remove", "accepted", "being removed by your platform"},
		{"deploy", "undelivered", "Your platform did not accept the release"},
		{"deploy", "refused", "Your platform refused the release"},
	} {
		state, line := c.event+" "+c.state, c.line
		var waiting env
		if err := json.Unmarshal([]byte(`{"run_by":"org","pending":{"release":"rel_8f2a","event":"`+c.event+`","sent_at":"`+sentAt+`","state":"`+c.state+`"}}`), &waiting); err != nil {
			t.Fatal(err)
		}
		s.Envs["prod"] = &waiting
		out := rendered(t, s, latest)
		i := strings.Index(out, "\n  production ")
		prod, _, _ := strings.Cut(out[i+1:], "\n")
		if i < 0 || !strings.HasSuffix(prod, "   "+line) || strings.Contains(prod, "http") || strings.Contains(prod, "ago") {
			t.Errorf("a prod waiting on its first release, %s, rendered\n%s", state, out)
		}
	}
}

// The app page's golden for an app waiting on its first release (contracts/testdata/app-page/
// broker-portal-status.json), rendered whole: preview says what it waits on, since when, and prod is
// not promoted yet. The clock is UTC so the golden is the same on every machine.
func TestStatusOfAnAppWaitingOnItsFirstReleaseIsTheGolden(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	raw, err := os.ReadFile("../contracts/testdata/app-page/broker-portal-status.json")
	if err != nil {
		t.Fatal(err)
	}
	var s statusRead
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	s.Access = &teamAccess{Level: "owner", People: 1}
	var out strings.Builder
	(&app{out: &out}).renderStatus("broker-portal", "http://localhost:7777", s, &runRow{ID: "gr_9c04", State: "running", StartedAt: "2026-09-02T09:57:41Z"}, at)
	wants(t, "status-waiting.txt", out.String())
}

// A release open over what prod runs (a promotion handed to the org, or its removal): the live row
// stays as it is, and the release it waits on is said under it — never silently gone (HAND-OFF §4).
func TestARunningEnvironmentSaysTheReleaseItWaitsOn(t *testing.T) {
	whole := page(t)
	s, latest := reads(t, whole, "status")
	var promoted struct {
		Promotion env `json:"promotion"`
	}
	if json.Unmarshal(whole["promotion"], &promoted) != nil {
		t.Fatal("the fixture's promotion is not an env object")
	}
	prod := promoted.Promotion
	prod.Pending = &pending{Event: "remove", SentAt: "2026-09-02T11:30:00Z", State: "accepted"}
	s.Envs["prod"] = &prod
	out := rendered(t, s, latest)
	if !strings.Contains(out, prod.URL) || !strings.Contains(out, "being removed by your platform") {
		t.Errorf("a running prod whose removal waits on the platform rendered\n%s", out)
	}
}

// Status says what runs now, never what the last deploy recorded (#1241): a stopped environment
// says stopped, with no URL to an app that answers nothing, and names the log that says why; one
// the host could not check keeps its URL and says it could not check.
func TestAStoppedEnvironmentSaysStoppedAndAnUncheckedOneSaysSo(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	url := s.Envs["preview"].URL
	s.Envs["preview"].State = "stopped"
	if out := rendered(t, s, latest); strings.Contains(out, url) || !strings.Contains(out, "  preview      v1.3   ✗ stopped   canyon logs --env preview says why\n") {
		t.Errorf("a stopped preview rendered\n%s", out)
	}
	s.Envs["preview"].State = "unknown"
	if out := rendered(t, s, latest); !strings.Contains(out, "  preview      v1.3   "+url+"   ! couldn't check whether it runs\n") {
		t.Errorf("a preview the host could not check rendered\n%s", out)
	}
}

// A preview started again since its deploy keeps restarting, never live, even while it is up: no URL,
// how often since its deploy, and the log that says why (contracts/routes.md, envs.<env>.state; #1722).
func TestARestartingEnvironmentSaysHowOftenAndHasNoURL(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	url, at := s.Envs["preview"].URL, s.Envs["preview"].DeployedAt
	since, _ := time.Parse(time.RFC3339, at)
	s.Envs["preview"].State = "restarting"
	for n, times := range map[int]string{7: "7 times", 1: "once"} {
		s.Envs["preview"].Restarting = &struct {
			Restarts int `json:"restarts"`
		}{n}
		want := "  preview      v1.3   ✗ keeps restarting (" + times + " since " + since.Local().Format("2 Jan 15:04") + ")   canyon logs --env preview says why\n"
		if out := rendered(t, s, latest); strings.Contains(out, url) || !strings.Contains(out, want) {
			t.Errorf("a preview restarted %d times rendered\n%s", n, out)
		}
	}
}

// A preview a deploy is still starting says so in amber, with no URL yet and no log to read: never stopped (#230).
func TestAStartingEnvironmentSaysStartingAndHasNoURL(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	url := s.Envs["preview"].URL
	s.Envs["preview"].State = "starting"
	if out := rendered(t, s, latest); strings.Contains(out, url) || !strings.Contains(out, "  preview      v1.3   ! starting\n") || strings.Contains(out, "stopped") {
		t.Errorf("a starting preview rendered\n%s", out)
	}
}

// A rule with nothing set to check against reads neutral, never a failure, and stays on the receipt
// so an admin sees it (contracts/routes.md, decision D4).
func TestAnUnsetRuleReadsNeutral(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	for i, rule := range s.Receipt.Rules {
		if rule.ID == "stack.blessed" {
			s.Receipt.Rules[i].Unset, s.Receipt.Rules[i].Why = true, "not set by your admin"
		}
	}
	out := rendered(t, s, latest)
	if !strings.Contains(out, "  · Approved stacks only") || !strings.Contains(out, "      not set by your admin\n") || strings.Contains(out, advisoryChip) {
		t.Errorf("the unset rule rendered\n%s", out)
	}
}

// D1: a status whose newest run is the approved package's blocked run reads approved, amber, never
// ✗ blocked; the project read's stage says so, for that package only.
func TestAnApprovedRunReadsApprovedOnStatus(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	_, latest := reads(t, whole, "status")
	for pkg, want := range map[string]string{latest.Package: "review       ! approved", "pkg_other": "review       ✗ blocked"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write(pages[0])
			case r.URL.Path == "/api/v1/projects/claims-portal":
				_, _ = fmt.Fprintf(w, `{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"package":%q,"stage":"approved","since":"2026-09-01T00:00:00Z"}}}`, pkg)
			default:
				_, _ = w.Write(whole["status"])
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		out, _, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("latest %s approved: exit %d, no %q in\n%s", pkg, code, want, out)
		}
	}
}

// With nothing in preview the receipt is one neutral line, never a checklist that reads as the app
// failing; an advisory rule that did not pass is an amber !, never a ✗ (canyonv2-5r11.20).
func TestAWaitingReceiptIsOneLineAndAnAdvisoryMissIsNoCross(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	rules := s.Receipt.Rules
	s.Receipt = receipt{Org: "Meridian", BlockedBy: []string{}, Rules: []receiptRule{}, Waiting: "Deploy a new version to preview to promote it."}
	if out := rendered(t, s, latest); !strings.Contains(out, "\n  · Deploy a new version to preview to promote it.\n") || strings.Contains(out, "prod.clean-gate") || strings.Contains(out, "requires a passing review") {
		t.Errorf("a waiting receipt rendered\n%s", out)
	}
	s.Receipt = receipt{Org: "Meridian", BlockedBy: []string{}, Rules: rules}
	for i := range s.Receipt.Rules {
		if s.Receipt.Rules[i].ID == "stack.blessed" {
			s.Receipt.Rules[i].Unset, s.Receipt.Rules[i].Why = false, "no stack recorded" // a list set, and nothing to check it against
		}
	}
	if out := rendered(t, s, latest); !strings.Contains(out, "  ! Approved stacks only") || strings.Contains(out, "✗ Approved stacks only") {
		t.Errorf("an advisory miss rendered\n%s", out)
	}
}

// Where nobody signs in, nobody's place on a team means anything and nothing is private: the access
// line says who can open the app, and no fleet read asks who is an admin (audit §4, item 2).
func TestStatusOnAHostWithNoSignInSaysAnyoneCanOpenIt(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/gate-runs"):
			_, _ = w.Write(pages[0])
		case r.URL.Path == "/api/v1/projects/claims-portal":
			_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","access":{"level":"none","everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`))
		case r.URL.Path == "/auth/config":
			_, _ = w.Write([]byte(`{"canyon":true,"auth":"off"}`))
		case r.URL.Path == "/api/v1/front-door": // whoever reads is the one who runs it: the host's warning is theirs
			_, _ = w.Write([]byte(`{"runtime":"kubernetes","warning":"Apps on this cluster can reach the server they run on (ports 10250, 22)."}`))
		default:
			_, _ = w.Write(whole["status"])
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	t.Chdir(dir)
	out, _, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
	if code != exitOK || !strings.Contains(out, "access       anyone who can reach this Canyon (sign-in is off)\n") || slices.Contains(asked, "/api/v1/fleet") ||
		!strings.Contains(out, "! Apps on this cluster can reach the server they run on (ports 10250, 22).") {
		t.Errorf("exit %d, asked %q:\n%s", code, asked, out)
	}
}

// The front door's warning (contracts/routes.md, the front-door row): a pod on this cluster reaches its
// own node, which never stopped the front door being saved (#1435). Status says it once, amber, after the
// checklist, with where the host rule is, to an admin: a member can do nothing about the cluster's
// firewall. With none, status is the golden.
func TestAHostWarningIsOneAmberLineOnStatus(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	warning := "Apps on this cluster can reach the server they run on (ports 10250, 22). Your cluster admin can close this with a host firewall rule."
	s.HostWarning = warning
	if got := rendered(t, s, latest); strings.Contains(got, warning) {
		t.Errorf("a member was shown the cluster's warning:\n%s", got)
	}
	s.Admin = true
	got := rendered(t, s, latest)
	if line := "\n  ! " + warning + " The rule is in deploy/k8s/README.md, under Host firewall.\n"; strings.Count(got, line) != 1 {
		t.Errorf("status with a host warning lacks %q:\n%s", line, got)
	}
	var painted strings.Builder
	(&app{out: &painted, render: Render{Color: true}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	if !strings.Contains(painted.String(), ansiAmber+"! "+warning) {
		t.Errorf("the warning is not amber:\n%q", painted.String())
	}
}

// The front door's discovery_note (contracts/routes.md, the front-door row; #2344): Canyon has not read the sign-in
// provider's discovery yet. Status says it as it says the host warning — one amber line after the checklist to an
// admin, a discovery note: line under ai — and a member is shown none.
func TestADiscoveryNoteIsOneAmberLineOnStatus(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	note := "Canyon couldn't read your sign-in provider's discovery document yet and keeps trying every minute; until it does, apps whose outbound mode is allowlist can't sign people in."
	s.DiscoveryNote = note
	if got := rendered(t, s, latest); strings.Contains(got, note) {
		t.Errorf("a member was shown the discovery note:\n%s", got)
	}
	s.Admin = true
	if got := rendered(t, s, latest); strings.Count(got, "\n  ! "+note+"\n") != 1 {
		t.Errorf("status with a discovery note lacks its line:\n%s", got)
	}
	var painted, ai strings.Builder
	(&app{out: &painted, render: Render{Color: true}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	if !strings.Contains(painted.String(), ansiAmber+"! "+note) {
		t.Errorf("the note is not amber:\n%q", painted.String())
	}
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	if !strings.Contains(ai.String(), "\ndiscovery note: "+note+"\n") {
		t.Errorf("--format ai lacks the discovery note: line:\n%s", ai.String())
	}
}

// An owner reading status on a host with a front-door warning is asked about as an admin or a member,
// so only an admin is told about a firewall only an admin can change; and so with a discovery note (#2344).
func TestStatusAsksWhoReadsAHostWarning(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	for field, warning := range map[string]string{
		"warning":        "Apps on this cluster can reach the server they run on (ports 10250, 22). Your cluster admin can close this with a host firewall rule.",
		"discovery_note": "Canyon couldn't read your sign-in provider's discovery document yet and keeps trying every minute; until it does, apps whose outbound mode is allowlist can't sign people in.",
	} {
		for role, shown := range map[string]bool{"admin": true, "member": false} {
			var asked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.URL.RequestURI())
				switch {
				case strings.HasSuffix(r.URL.Path, "/gate-runs"):
					_, _ = w.Write(pages[0])
				case r.URL.Path == "/api/v1/projects/claims-portal":
					_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`))
				case r.URL.Path == "/auth/config":
					_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
				case r.URL.Path == "/api/v1/front-door":
					_, _ = fmt.Fprintf(w, `{"runtime":"kubernetes",%q:%q}`, field, warning)
				case r.URL.Path == "/api/v1/fleet":
					_, _ = fmt.Fprintf(w, `{"apps":[],"next":null,"viewer":{"display":"Ada","role":%q}}`, role)
				default:
					_, _ = w.Write(whole["status"])
				}
			}))
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
			t.Chdir(dir)
			out, _, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
			server.Close()
			if code != exitOK || !slices.Contains(asked, "/api/v1/fleet?limit=1") || strings.Contains(out, warning) != shown || !strings.Contains(out, "you are an owner") {
				t.Errorf("%s, %s: exit %d, asked %q, shown %v, want %v:\n%s", field, role, code, asked, strings.Contains(out, warning), shown, out)
			}
		}
	}
}

// The review row is the app's own newest run, the one the status read names (last_run), never the
// newest row of the runs list: a check's run (#1251) and a pull request's are listed there and are
// never the app's review. It is looked for a page at a time; none named is never reviewed.
func TestStatusReviewIsTheAppsOwnRunNeverACheck(t *testing.T) {
	for _, c := range []struct {
		lastRun, want string
	}{
		{`"gr_own"`, "  review       ✓ passed   0 blocking findings, 0 warnings"},
		{`null`, "  review       · never reviewed\n"},
	} {
		var asked []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.RequestURI())
			switch {
			case strings.HasSuffix(r.URL.Path, "/gate-runs") && r.URL.Query().Get("cursor") == "":
				_, _ = w.Write([]byte(`{"runs":[{"id":"gr_check","package":"pkg_checked","state":"blocked","blocking":2,"warnings":0,"started_at":"2026-09-01T00:00:00Z","ended_at":"2026-09-01T00:00:10Z"}],"next":"c2"}`))
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write([]byte(`{"runs":[{"id":"gr_own","package":"pkg_head","state":"clean","blocking":0,"warnings":0,"started_at":"2026-08-31T00:00:00Z","ended_at":"2026-08-31T00:00:10Z"}],"next":null}`))
			case r.URL.Path == "/api/v1/projects/claims-portal":
				_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"package":"pkg_head","stage":"passed","since":"2026-09-01T00:00:00Z"}}}`))
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
			case strings.HasSuffix(r.URL.Path, "/status"):
				_, _ = fmt.Fprintf(w, `{"project":"claims-portal","envs":{},"last_run":%s,"promote_run":null,"receipt":{"org":"Meridian","rules":[],"blocked_by":[]}}`, c.lastRun)
			default:
				_, _ = w.Write([]byte(`{}`))
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		if code != exitOK || errs != "" || !strings.Contains(out, c.want) || strings.Contains(out, "blocked") {
			t.Errorf("last_run %s: exit %d, stderr %q, asked %q, printed\n%s", c.lastRun, code, errs, asked, out)
		}
	}
}

// Under --format ai status is one block of key: value lines (cli.md §6.1): the same reads, nothing
// painted and no age, the receipt's rules as §6.3's checklist, the host warning an admin's last line.
func TestStatusUnderAIIsOneBlock(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	var out strings.Builder
	a := &app{out: &out, render: Render{Format: FormatAI}}
	a.renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai.txt", out.String())
	out.Reset()
	s.HostWarning, s.Admin = "Apps on this cluster can reach the server they run on (ports 10250, 22).", true
	s.Envs["preview"].State, s.Envs["preview"].Restarting = "restarting", &struct {
		Restarts int `json:"restarts"`
	}{Restarts: 1}
	a.renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	got := out.String()
	for _, want := range []string{"\npreview: v1.3 · keeps restarting (once since ", ") · canyon logs --env preview says why\n", "\nreview: never reviewed\n",
		"\nhost warning: Apps on this cluster can reach the server they run on (ports 10250, 22). The rule is in deploy/k8s/README.md, under Host firewall.\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	for state, want := range map[string]string{
		"stopped": "\npreview: v1.3 · stopped · canyon logs --env preview says why\n",
		"":        "\npreview: v1.3 · http://claims-portal.preview.localhost:7777 · deployed 2026-09-01T11:26:40Z\n",
		"unknown": "\npreview: v1.3 · http://claims-portal.preview.localhost:7777 · couldn't check whether it runs\n",
	} {
		out.Reset()
		s.Envs["preview"].State, s.Envs["preview"].Restarting = state, nil
		a.renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		if !strings.Contains(out.String(), want) {
			t.Errorf("state %q: no %q in\n%s", state, want, out.String())
		}
	}
	out.Reset()
	s.Approved, s.Envs["prod"] = true, &env{Pending: &pending{Event: "deploy", SentAt: "2026-09-02T10:00:00Z", State: "undelivered"}}
	a.renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	for _, want := range []string{"\nproduction: — · Your platform did not accept the release\n", "\nreview: approved · 1 blocking finding, 3 warnings · run gr_8f2a"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	out.Reset()
	a.renderReduced("claims-portal", []actorRef{{Display: "Dana Whitfield"}}, "")
	if want := "CANYON STATUS\n\napp: claims-portal\nowners: Dana Whitfield\nsummary: No description yet\naccess: read-only\n"; out.String() != want {
		t.Errorf("a read-only reader's block is\n%q\nwant\n%q", out.String(), want)
	}
}

// A row that shows its URL carries an answer to say beside readiness, for a person and an agent (cli.md
// §6.1): a preview answering 400 read as running here, and only deploy's step said so (QA round 3).
func TestStatusSaysAnAnswerBesideReadiness(t *testing.T) {
	const url = "http://claims-portal.preview.localhost:7777"
	for _, c := range []struct {
		state   string
		said    *answered
		human   string
		agent   string
		without string
	}{
		{"running", &answered{Status: 400}, url + "   1d ago   answered 400 Bad Request at /\n", url + " · deployed 2026-09-01T11:26:40Z · answered 400 Bad Request at /\n", ""},
		{"", &answered{Error: "refused"}, url + "   1d ago   no answer at /: refused\n", url + " · deployed 2026-09-01T11:26:40Z · gave no answer at /\n" + appSays + "\n  refused\n", ""}, // the app's words under appSays for an agent
		{"running", &answered{Status: 200}, url + "   1d ago\n", url + " · deployed 2026-09-01T11:26:40Z\n", "answered"},
		{"stopped", &answered{Status: 400}, "stopped   canyon logs --env preview says why\n", "stopped · canyon logs --env preview says why\n", "answered"},
	} {
		s, latest := reads(t, page(t), "status")
		s.Envs["preview"].State, s.Envs["preview"].Answered = c.state, c.said
		var agent strings.Builder
		(&app{out: &agent, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		for out, want := range map[string]string{rendered(t, s, latest): c.human, agent.String(): c.agent} {
			if !strings.Contains(out, want) || c.without != "" && strings.Contains(out, c.without) {
				t.Errorf("%q answered %+v: want %q (and no %q) in\n%s", c.state, *c.said, want, c.without, out)
			}
		}
	}
}

// egressRead is the egress contract fixture's app read (contracts/testdata/egress), with the host's via, and no
// credential refusal: TestStatusNamesTheLastInjectRefusal reads the fixture's own (credentialRead).
func egressRead(t *testing.T, via string) *outbound {
	t.Helper()
	o := credentialRead(t, via)
	for _, e := range o.Envs {
		for i := range e.Destinations {
			e.Destinations[i].LastCredentialRefusal = nil
		}
	}
	return o
}

// credentialRead is the egress contract fixture's app read whole, with the host's via.
func credentialRead(t *testing.T, via string) *outbound {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "contracts", "testdata", "egress", "claims-portal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var whole struct {
		ProjectRead outbound `json:"project_read"`
	}
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	whole.ProjectRead.Via = via
	return &whole.ProjectRead
}

// promotedStatus is the fixture's status read with production promoted: both envs run, Canyon running each.
func promotedStatus(t *testing.T) (statusRead, *runRow) {
	t.Helper()
	whole := page(t)
	s, latest := reads(t, whole, "status")
	var promoted struct {
		Promotion env `json:"promotion"`
	}
	if json.Unmarshal(whole["promotion"], &promoted) != nil {
		t.Fatal("the fixture's promotion is not an env object")
	}
	s.Envs["prod"] = &promoted.Promotion
	return s, latest
}

// outboundLines are the rendered status's lines between the review row and the blank line after it.
func outboundLines(out string) string {
	_, after, _ := strings.Cut(out, "\n  review ")
	_, after, _ = strings.Cut(after, "\n")
	lines, _, _ := strings.Cut("\n"+after, "\n\n")
	return strings.TrimPrefix(lines, "\n")
}

// The outbound row (design/EGRESS.md §8.1, cli.md §6.1.4): one line an env, after the review, from the
// egress contract's app read; the golden is the spec's transcript.
func TestStatusOutboundRowIsTheGolden(t *testing.T) {
	s, latest := promotedStatus(t)
	s.Outbound = egressRead(t, "canyon")
	wants(t, "status-outbound.txt", rendered(t, s, latest))
	if spec := strings.TrimPrefix(specFence(t, "#### 6.1.4 Outbound"), "$ canyon status\n"); rendered(t, s, latest) != spec {
		t.Errorf("canyon status is not §6.1.4's transcript:\n%s", spec)
	}
}

// On Kubernetes (design/EGRESS.md §8.1, #2286) the floor is per env, and there is no network to recreate: under
// not_enforced an env run under canyon is recorded, its own pod floored, and one that is not says its next deploy
// or promotion floors it; a floor this build does not know is never recorded, fail closed — both envs say so,
// though both ran under canyon.
func TestStatusOutboundOnKubernetesIsTheGolden(t *testing.T) {
	s, latest := promotedStatus(t)
	s.Outbound, s.Kube = egressRead(t, "canyon"), true
	s.Outbound.Floor, s.Outbound.Envs["prod"].Via = ptr("not_enforced"), ptr("off")
	wants(t, "status-outbound-kube-env.txt", rendered(t, s, latest))
	s.Outbound.Floor, s.Outbound.Envs["prod"].Via = ptr("unknown"), ptr("canyon")
	wants(t, "status-outbound-kube.txt", rendered(t, s, latest))
}

// §8.1's values, each env its own, the whole row matched: the org's platform runs it — before any floor;
// a network not read as enforced — not_enforced, unknown, or none at all under via canyon — is never
// recorded (fail closed); a version deployed before via canyon, or with no spec recorded; a count of the
// week's destinations, none included; nothing at all while nothing goes through Canyon.
func TestStatusOutboundSaysEachEnvsValue(t *testing.T) {
	const (
		recorded2 = "preview: recorded · 2 destinations in the last 7 days"
		floor     = "may skip Canyon until an admin recreates its network in Settings › Outbound"
		kubeFloor = "may skip Canyon until its next deploy or promotion"
	)
	row := func(preview, prod string) string {
		return "  outbound     " + preview + " (beta)\n               " + prod
	}
	for _, c := range []struct {
		name string
		edit func(o *outbound, s *statusRead)
		want string
	}{
		{"both through Canyon", func(o *outbound, s *statusRead) {}, row(recorded2, "production: recorded · 2 destinations in the last 7 days")},
		{"the org runs production", func(o *outbound, s *statusRead) { s.Envs["prod"].RunBy = "org" }, row(recorded2, "production: runs on your own platform")},
		{"the org runs production on an open network", func(o *outbound, s *statusRead) {
			s.Envs["prod"].RunBy, o.Floor = "org", ptr("not_enforced")
		}, row("preview: "+floor, "production: runs on your own platform")},
		{"an open network, production from before via canyon", func(o *outbound, s *statusRead) {
			o.Floor, o.Envs["prod"].Via = ptr("not_enforced"), ptr("off")
		}, row("preview: "+floor, "production: "+floor)},
		{"a floor this build does not know", func(o *outbound, s *statusRead) { o.Floor = ptr("unknown") }, row("preview: "+floor, "production: "+floor)},
		{"no floor under via canyon", func(o *outbound, s *statusRead) { o.Floor = nil }, row("preview: "+floor, "production: "+floor)},
		{"production from before via canyon", func(o *outbound, s *statusRead) { o.Envs["prod"].Via = ptr("off") }, row(recorded2, "production: not yet through Canyon: deploy again")},
		{"production with no spec recorded", func(o *outbound, s *statusRead) { o.Envs["prod"].Via = nil }, row(recorded2, "production: not yet through Canyon: deploy again")},
		{"nothing in production", func(o *outbound, s *statusRead) { delete(s.Envs, "prod") }, "  outbound     " + recorded2 + " (beta)"},
		{"a destination older than the week", func(o *outbound, s *statusRead) { o.Envs["prod"].Destinations[1].LastSeenAt = "2026-08-26T11:59:59Z" },
			row(recorded2, "production: recorded · 1 destination in the last 7 days")},
		{"no destination", func(o *outbound, s *statusRead) { o.Envs["prod"].Destinations = nil }, row(recorded2, "production: recorded · 0 destinations in the last 7 days")},
		{"via off on an internal network", func(o *outbound, s *statusRead) { o.Via = "off" }, row(recorded2, "production: recorded · 2 destinations in the last 7 days")},
		{"via off on an open network", func(o *outbound, s *statusRead) { o.Via, o.Floor = "off", nil }, ""},
		{"Kubernetes, production with no spec recorded", func(o *outbound, s *statusRead) {
			s.Kube, o.Floor, o.Envs["prod"].Via = true, ptr("not_enforced"), nil
		}, row(recorded2, "production: "+kubeFloor)},
		{"Kubernetes, a floor this build does not know", func(o *outbound, s *statusRead) {
			s.Kube, o.Floor, o.Envs["prod"].Via = true, ptr("unknown"), ptr("off")
		}, row("preview: "+kubeFloor, "production: "+kubeFloor)},
		{"Kubernetes, no floor under via canyon", func(o *outbound, s *statusRead) { s.Kube, o.Floor = true, nil }, row("preview: "+kubeFloor, "production: "+kubeFloor)},
		{"Kubernetes, the org runs production", func(o *outbound, s *statusRead) {
			s.Kube, o.Floor, s.Envs["prod"].RunBy = true, ptr("not_enforced"), "org"
		}, row(recorded2, "production: runs on your own platform")},
		{"Kubernetes, the org runs production, a floor this build does not know", func(o *outbound, s *statusRead) {
			s.Kube, o.Floor, s.Envs["prod"].RunBy = true, ptr("unknown"), "org"
		}, row("preview: "+kubeFloor, "production: runs on your own platform")},
		{"Kubernetes, via off, both under canyon", func(o *outbound, s *statusRead) { s.Kube, o.Via = true, "off" }, row(recorded2, "production: recorded · 2 destinations in the last 7 days")},
		{"below contributor", func(o *outbound, s *statusRead) { s.Outbound = nil }, ""},
		{"a read that failed", func(o *outbound, s *statusRead) { s.Outbound = &outbound{Unread: "internal error (500 internal)"} }, "  outbound     couldn't read: internal error (500 internal) (beta)"},
	} {
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		s.Outbound.Envs["prod"].Mode, s.Outbound.Envs["prod"].Requests = "observe", nil // the fixture's allowlist has its own test below
		c.edit(s.Outbound, &s)
		out := rendered(t, s, latest)
		if got := outboundLines(out); got != c.want {
			t.Errorf("%s: the row is\n%q\nwant\n%q\nin\n%s", c.name, got, c.want, out)
		}
	}
}

// An env through Canyon says its mode, then what it refused since its version went live, the latest first as the
// host answered, and a dismissed one says so; a request older than the version is no refusal of it, and a time that
// does not read is never older (design/EGRESS.md §8.1). Under observe and allowlist alike a refused name is listed.
func TestStatusOutboundSaysWhatEachEnvRefused(t *testing.T) {
	const prodFrom = "2026-08-30T12:00:00Z" // promotedStatus's production
	refused := []outboundRequest{
		{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-09-02T11:58:00Z", Refusals: 14},
		{Destination: "hooks.slack.com", State: "dismissed", LastRefusedAt: "2026-09-02T11:00:00Z", Refusals: 1},
	}
	head := "  outbound     preview: recorded · 2 destinations in the last 7 days (beta)\n"
	for _, c := range []struct {
		name string
		edit func(e *outboundEnv)
		want string
	}{
		{"allowlist, nothing refused", func(e *outboundEnv) { e.Requests = nil }, "               production: only allowed destinations · 2 destinations in the last 7 days"},
		{"allowlist, two refused", func(e *outboundEnv) { e.Requests = refused },
			"               production: only allowed destinations · 2 destinations refused since v1.3 went live\n" +
				"                 ✗ api.openai.com    not allowed 14 times, last 2m ago\n" +
				"                 ✗ hooks.slack.com   not allowed once, 1h ago (dismissed)"},
		{"observe, one refused", func(e *outboundEnv) { e.Mode, e.Requests = "observe", refused[:1] },
			"               production: recorded · 1 destination refused since v1.3 went live\n" +
				"                 ✗ api.openai.com   not allowed 14 times, last 2m ago"},
		{"refused before this version went live", func(e *outboundEnv) {
			e.Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-08-30T11:59:59Z", Refusals: 2}}
		}, "               production: only allowed destinations · 2 destinations in the last 7 days"},
		{"refused as it went live", func(e *outboundEnv) {
			e.Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: prodFrom, Refusals: 2}}
		}, "               production: only allowed destinations · 1 destination refused since v1.3 went live\n                 ✗ api.openai.com   not allowed 2 times, last 3d ago"},
		{"a time that does not read", func(e *outboundEnv) {
			e.Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: "soon", Refusals: 3}}
		}, "               production: only allowed destinations · 1 destination refused since v1.3 went live\n                 ✗ api.openai.com   not allowed 3 times"},
	} {
		s, latest := promotedStatus(t)
		s.Envs["prod"].DeployedAt = prodFrom
		s.Outbound = egressRead(t, "canyon")
		c.edit(s.Outbound.Envs["prod"])
		out := rendered(t, s, latest)
		if got := outboundLines(out); got != head+c.want {
			t.Errorf("%s: the row is\n%s\nwant\n%s%s", c.name, got, head, c.want)
		}
	}
}

// Under --format ai (design/EGRESS.md §8.2) a line an env and one a destination refused, then what an agent may and
// may not do, once: the golden is the block whole.
func TestStatusOutboundUnderAIIsTheGolden(t *testing.T) {
	s, latest := promotedStatus(t)
	s.Outbound = egressRead(t, "canyon")
	s.Envs["prod"].DeployedAt = "2026-08-30T12:00:00Z"
	s.Outbound.Envs["preview"].Mode = "allowlist"
	s.Outbound.Envs["prod"].Requests = []outboundRequest{
		{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-09-02T11:58:00Z", Refusals: 14},
		{Destination: "hooks.slack.com", State: "dismissed", LastRefusedAt: "2026-09-02T11:00:00Z", Refusals: 1},
	}
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai-outbound.txt", out.String())
}

// On a CONNECT most clients hide a refusal's body, so an agent sees only "Tunnel connection failed: 502" (#2692):
// canyon status names each destination's last call Canyon refused while adding its credential, since its version
// went live, what the app was told and what the agent does — fix the code, or ask a person to set the credential.
// The fixture's own refusal (hooks.slack.com, upstream_failed) is among them; one from before production went live
// is not; a reason this build does not know guesses no fix; and the HTTPS_PROXY hint, wrong for a call the gateway
// refused, is left out.
func TestStatusNamesTheLastInjectRefusal(t *testing.T) {
	s, latest := promotedStatus(t)
	s.Outbound = credentialRead(t, "canyon")
	s.Envs["prod"].DeployedAt = "2026-08-30T12:00:00Z"
	s.Outbound.Envs["prod"].Requests = nil
	refusal := func(dest string, port int, reason, at string) {
		e := s.Outbound.Envs["prod"]
		e.Destinations = append(e.Destinations, e.Destinations[0])
		d := &e.Destinations[len(e.Destinations)-1]
		d.Destination, d.Port = dest, port
		d.LastCredentialRefusal = &struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		}{reason, at}
	}
	refusal("api.stripe.com", 80, "inject_requires_tls", "2026-09-02T11:58:00Z")
	refusal("flood.example.com", 443, "misdirected", "2026-09-02T11:57:00Z")
	refusal("api.anthropic.com", 443, "inject_unavailable", "2026-09-02T11:56:00Z")
	refusal("api.openai.com", 443, "inject_unavailable", "2026-08-29T09:00:00Z") // before production went live
	refusal("api.mistral.ai", 443, "inject_later", "2026-09-02T11:55:00Z")
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai-credential.txt", out.String())
}

// With nothing refused an agent reads the lines alone and no block; an env not through Canyon its §8.1 value; a read
// that failed says so; and below contributor, or with nothing through Canyon, nothing at all.
func TestStatusOutboundUnderAISaysEachEnvsValue(t *testing.T) {
	const quiet = "outbound: preview · recorded · no destination refused since v1.3 went live\noutbound: production · only allowed destinations · no destination refused since v1.3 went live\n" + timeoutHint
	for _, c := range []struct {
		name string
		edit func(s *statusRead)
		want string
	}{
		{"nothing refused", func(*statusRead) {}, quiet},
		{"the org runs production", func(s *statusRead) { s.Envs["prod"].RunBy = "org" }, "outbound: preview · recorded · no destination refused since v1.3 went live\noutbound: production · runs on your own platform\n" + timeoutHint},
		{"a floor not enforced", func(s *statusRead) { s.Outbound.Floor = ptr("not_enforced") },
			"outbound: preview · may skip Canyon until an admin recreates its network in Settings › Outbound\noutbound: production · may skip Canyon until an admin recreates its network in Settings › Outbound\n"},
		{"a read that failed", func(s *statusRead) { s.Outbound = &outbound{Unread: "internal error (500 internal)"} }, "outbound: couldn't read: internal error (500 internal)\n"},
		{"below contributor", func(s *statusRead) { s.Outbound = nil }, ""},
		{"below contributor, the row blocking", func(s *statusRead) { s.Outbound, s.Receipt.BlockedBy = nil, []string{egressAllowed} }, ""},
		{"via off on an open network", func(s *statusRead) { s.Outbound.Via, s.Outbound.Floor = "off", nil }, ""},
	} {
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		if s.Outbound != nil {
			s.Outbound.Envs["prod"].Requests = nil
		}
		c.edit(&s)
		var out strings.Builder
		(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		got := out.String()
		_, after, _ := strings.Cut(got, "/#/apps/claims-portal\n\n")
		lines, _, _ := strings.Cut(after, "production checklist:")
		if c.want != "" {
			c.want += "\n"
		}
		if lines != c.want || strings.Contains(got, "do not:") {
			t.Errorf("%s: read\n%q\nwant\n%q", c.name, lines, c.want)
		}
	}
}

// With preview under observe nothing was refused, so nothing in the refused block is true, and an agent reading the
// blocking row still needs who allows a destination and the link. It gets them whenever the row blocks, and with nothing
// through Canyon to say of each env, those alone. A row that blocks nothing changes nothing: the env lines and the
// timeout hint, as with nothing blocking.
func TestStatusOutboundUnderAISaysWhatABlockingRowNeeds(t *testing.T) {
	const link = "  2. Give them this link: http://localhost:7777/#/apps/claims-portal/outbound\n"
	for _, c := range []struct {
		name   string
		edit   func(s *statusRead)
		blocks bool
		envs   bool
	}{
		{"production's allowlist, nothing refused", func(*statusRead) {}, true, true},
		{"nothing through Canyon", func(s *statusRead) { s.Outbound.Via, s.Outbound.Floor = "off", nil }, true, false},
		{"a read that failed", func(s *statusRead) { s.Outbound = &outbound{Unread: "internal error (500 internal)"} }, true, false},
		{"another rule blocking", func(s *statusRead) {}, false, true},
	} {
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		s.Outbound.Envs["prod"].Requests = nil
		s.Receipt.BlockedBy = []string{"prod.clean-gate"}
		if c.blocks {
			s.Receipt.BlockedBy = []string{egressAllowed}
		}
		c.edit(&s)
		var out strings.Builder
		(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		got := out.String()
		for want, on := range map[string]bool{
			link: c.blocks, "outcome: prod.egress-allowed stops this version reaching production.\n": c.blocks,
			"You cannot allow one.\n": c.blocks, "do not:\n": c.blocks, "outbound: preview · recorded": c.envs,
			"never left Canyon": false, timeoutHint: !c.blocks && c.envs,
		} {
			if strings.Contains(got, want) != on {
				t.Errorf("%s: holds %q %v, want %v:\n%s", c.name, want, !on, on, got)
			}
		}
	}
}

const timeoutHint = "If a call to an outside service times out, or can't resolve the host name, the client may be ignoring HTTPS_PROXY.\n"

// The timeout hint fits a call that never reached Canyon's gateway: an app through Canyon whose client ignores
// HTTPS_PROXY, while Canyon refused nothing. Under a refusal every call was a 403, never a timeout, so it never
// prints there (#2708), nor beside the go-live row's blocks, nor where nothing goes through Canyon. Once, right under
// the env lines.
func TestOutboundBlockSaysTheTimeoutHintOnlyWhereNothingWasRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(s *statusRead)
		hint bool
	}{
		{"nothing refused", func(*statusRead) {}, true},
		{"production refused one", func(s *statusRead) {
			s.Outbound.Envs["prod"].Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-09-02T11:58:00Z", Refusals: 14}}
		}, false},
		{"the go-live row blocks", func(s *statusRead) { s.Receipt.BlockedBy = []string{egressAllowed} }, false},
		{"the go-live row could not check", func(s *statusRead) {
			s.Receipt.BlockedBy, s.Receipt.Rules = []string{egressAllowed}, []receiptRule{{ID: egressAllowed, Blocks: true, Why: "could not check: Canyon can't read preview's connections right now."}}
		}, false},
		{"nothing through Canyon", func(s *statusRead) { s.Outbound.Floor = ptr("not_enforced") }, false},
		{"a read that failed", func(s *statusRead) { s.Outbound = &outbound{Unread: "internal error (500 internal)"} }, false},
	} {
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		s.Outbound.Envs["prod"].Requests = nil
		c.edit(&s)
		var out strings.Builder
		(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		got, want := out.String(), 0
		if c.hint {
			want = 1
			if !strings.Contains(got, "went live\n"+timeoutHint+"\n") {
				t.Errorf("%s: the hint is not right under the env lines:\n%s", c.name, got)
			}
		}
		if n := strings.Count(got, timeoutHint); n != want {
			t.Errorf("%s: the hint printed %d times, want %d:\n%s", c.name, n, want, got)
		}
	}
}

// An Allow reaches the running app with its next call, so the refused block's link says no deploy is needed (#2708:
// an agent planned a redeploy). The go-live row's block keeps its own steps, since a promotion is how its fix reaches
// production. Both say once who decides what the app reaches, after the two things an agent must not do: on the status read,
// and in a refused promotion's block too.
func TestOutboundBlockSaysAnAllowNeedsNoDeployAndWhoDecides(t *testing.T) {
	const (
		host  = "http://localhost:7777"
		allow = "  2. Give them this link: " + host + "/#/apps/claims-portal/outbound\n" +
			"     Once a person allows a destination there, the app's next call to it goes through within seconds, with no deploy.\n" +
			"  3. "
		fact = "  - unset or change HTTPS_PROXY, HTTP_PROXY or NO_PROXY\n" +
			"  A person on the app's team decides what the app reaches; any other way goes around them.\n"
	)
	row := receiptRule{ID: egressAllowed, Name: "Production allows everything preview connects to", Blocks: true, Why: "production does not allow api.openai.com"}
	refused, latest := promotedStatus(t)
	refused.Outbound = egressRead(t, "canyon")
	refused.Outbound.Envs["prod"].Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-09-02T11:58:00Z", Refusals: 14}}
	held, _ := promotedStatus(t)
	held.Outbound = egressRead(t, "canyon")
	held.Outbound.Envs["prod"].Requests = nil
	held.Receipt.BlockedBy, held.Receipt.Rules = []string{egressAllowed}, []receiptRule{row}
	var status, hold, promote strings.Builder
	(&app{out: &status, render: Render{Format: FormatAI}}).renderStatus("claims-portal", host, refused, latest, at)
	(&app{out: &hold, render: Render{Format: FormatAI}}).renderStatus("claims-portal", host, held, latest, at)
	(&app{out: &promote, render: Render{Format: FormatAI}}).aiPromoteBlocked(receipt{Org: "Meridian", BlockedBy: []string{egressAllowed}, Rules: []receiptRule{row}}, host, "claims-portal")
	for name, c := range map[string]struct {
		out   string
		allow int
	}{"refused": {status.String(), 1}, "held": {hold.String(), 0}, "a refused promotion": {promote.String(), 0}} {
		if n := strings.Count(c.out, allow); n != c.allow {
			t.Errorf("%s: says an Allow needs no deploy %d times, want %d:\n%s", name, n, c.allow, c.out)
		}
		if n := strings.Count(c.out, fact); n != 1 {
			t.Errorf("%s: says who decides %d times, want 1:\n%s", name, n, c.out)
		}
	}
}

// A row that could not check names no destination, so the steps for one say nothing true of it: an agent reads the one
// outcome line in their place, on the status read and in a refused promotion alike. A row that names one keeps them.
func TestAnEgressRowThatCouldNotCheckPrintsOnlyItsOutcome(t *testing.T) {
	const (
		outcome = "outcome: Canyon could not check outbound connections, so this version can't reach production. Do what the prod.egress-allowed line says. An approval won't clear this.\n"
		host    = "http://localhost:7777"
	)
	for _, c := range []struct {
		name, why string
		unchecked bool
	}{
		{"could not check, no record", "could not check: Canyon doesn't have a full record of preview's connections. Deploy to preview again. If this persists, ask an admin.", true},
		{"could not check, no read", "could not check: Canyon can't read preview's connections right now. Try again in a few minutes. If this persists, ask an admin.", true},
		{"could not check, over the cap", "could not check: preview connected to over 500 destinations, more than Canyon tracks. Connect to fewer and deploy to preview. If this persists, ask an admin.", true},
		{"a destination named", "production does not allow api.openai.com", false},
		{"to a responder, none named", "production does not allow everything preview connected to. The app's team can fix this.", false},
	} {
		row := receiptRule{ID: egressAllowed, Name: "Production allows everything preview connects to", Blocks: true, Why: c.why}
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		s.Outbound.Envs["prod"].Requests = nil
		s.Receipt.BlockedBy, s.Receipt.Rules = []string{egressAllowed}, []receiptRule{row}
		var status, promote strings.Builder
		(&app{out: &status, render: Render{Format: FormatAI}}).renderStatus("claims-portal", host, s, latest, at)
		(&app{out: &promote, render: Render{Format: FormatAI}}).aiPromoteBlocked(receipt{Org: "Meridian", BlockedBy: []string{egressAllowed}, Rules: []receiptRule{row}}, host, "claims-portal")
		for where, got := range map[string]string{"canyon status": status.String(), "a refused promotion": promote.String()} {
			for want, on := range map[string]bool{
				outcome: c.unchecked, "You cannot allow one.\n": !c.unchecked, "/#/apps/claims-portal/outbound": !c.unchecked, "do not:\n": !c.unchecked,
				"each destination it names": !c.unchecked,
			} {
				if strings.Contains(got, want) != on {
					t.Errorf("%s, %s: holds %q %v, want %v:\n%s", c.name, where, want, !on, on, got)
				}
			}
		}
	}
	// Advisory, the same row stops nothing, so it asks nothing of an agent: at rest, or beside another rule's refusal.
	row := receiptRule{ID: egressAllowed, Name: "Production allows everything preview connects to", Blocks: false, Why: "could not check: Canyon can't read preview's connections right now. Try again in a few minutes. If this persists, ask an admin."}
	s, latest := promotedStatus(t)
	s.Outbound = egressRead(t, "canyon")
	s.Outbound.Envs["prod"].Requests = nil
	s.Receipt.BlockedBy, s.Receipt.Rules = []string{}, []receiptRule{row}
	var status, promote strings.Builder
	(&app{out: &status, render: Render{Format: FormatAI}}).renderStatus("claims-portal", host, s, latest, at)
	(&app{out: &promote, render: Render{Format: FormatAI}}).aiPromoteBlocked(receipt{Org: "Meridian", BlockedBy: []string{"prod.clean-gate"}, Rules: []receiptRule{{ID: "prod.clean-gate", Name: "A rule", Blocks: true, Why: "a reason"}, row}}, host, "claims-portal")
	for where, got := range map[string]string{"canyon status": status.String(), "a promotion another rule refused": promote.String()} {
		if strings.Contains(got, outcome) {
			t.Errorf("advisory, %s: an agent is told the row stops production:\n%s", where, got)
		}
	}
}

// A neutral row is no failure: it stays on the checklist so a person sees it, and an agent reads no block and no advisory
// for it, whatever the rule's state (contracts/routes.md, The promote receipt).
func TestANeutralEgressRowStopsNothing(t *testing.T) {
	const why = "not checked: preview runs on your own platform"
	for _, blocks := range []bool{true, false} {
		row := receiptRule{ID: egressAllowed, Name: "Production allows everything preview connects to", Blocks: blocks, Why: why, Unset: true}
		s, latest := promotedStatus(t)
		s.Outbound = egressRead(t, "canyon")
		s.Outbound.Envs["prod"].Requests = nil
		s.Receipt.BlockedBy, s.Receipt.Rules = []string{}, []receiptRule{row}
		var out strings.Builder
		(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
		got := out.String()
		if !strings.Contains(got, "  not set: Production allows everything preview connects to (prod.egress-allowed): "+why+"\n") {
			t.Errorf("blocks %v: the checklist's line is missing:\n%s", blocks, got)
		}
		if strings.Contains(got, "outcome:") || strings.Contains(got, "do not:") {
			t.Errorf("blocks %v: a neutral row asks an agent for something:\n%s", blocks, got)
		}
		if advised := advisories(s.Receipt); len(advised) != 0 {
			t.Errorf("blocks %v: a neutral row is advised: %+v", blocks, advised)
		}
	}
}

// A promotion the row refuses names who allows a destination and the card, not the fleet url alone; one it doesn't refuse
// says nothing of outbound.
func TestPromoteBlockedUnderAISaysWhatTheEgressRowNeeds(t *testing.T) {
	const link = "  2. Give them this link: http://localhost:7777/#/apps/claims-portal/outbound\n"
	for blockedBy, on := range map[string]bool{egressAllowed: true, "prod.clean-gate": false} {
		var out strings.Builder
		r := receipt{Org: "Meridian", BlockedBy: []string{blockedBy}, Rules: []receiptRule{{ID: blockedBy, Name: "A rule", Blocks: true, Why: "production does not allow api.openai.com"}}}
		if code := (&app{out: &out, render: Render{Format: FormatAI}}).aiPromoteBlocked(r, "http://localhost:7777", "claims-portal"); code != exitBlocked {
			t.Errorf("%s: exit %d", blockedBy, code)
		}
		if got := strings.Contains(out.String(), link); got != on {
			t.Errorf("%s: says the link %v, want %v:\n%s", blockedBy, got, on, out.String())
		}
	}
}

func ptr(s string) *string { return &s }

// Status asks for the app's records and the host's via of an owner, a contributor and an admin, and of
// nobody else: a responder with no grant and a read-only member send neither (design/PERMISSIONS.md,
// Outbound). Under --format json neither is asked: it carries no row. Under ai the same readers read the same.
func TestStatusOutboundIsAContributorsAndUp(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	egress := egressFixture(t)
	var denied []string // whom the server refused the scanners' data, each a denied row on the Audit page
	const (
		reads   = "/api/v1/projects/claims-portal /api/v1/projects/claims-portal/status /api/v1/projects/claims-portal/gate-runs /auth/config /api/v1/front-door"
		records = " /api/v1/projects/claims-portal/egress /api/v1/egress"
		viewer  = " /api/v1/fleet"         // the reader's role, every status a person or an agent reads: the host's warnings are an admin's
		scanned = " /api/v1/scanners/data" // the databases' age (#2743): asked by an admin alone, since the read audits each refusal
		line    = "  outbound     preview: recorded · 2 destinations in the last 7 days (beta)\n"
	)
	for _, c := range []struct {
		who, level, role, format string
		latest                   bool
		asked                    string
	}{
		{"an owner", "owner", "member", "", true, reads + viewer + records},
		{"a contributor", "contributor", "member", "", true, reads + viewer + records},
		{"an owner who is an admin", "owner", "admin", "", true, reads + viewer + records + scanned},
		{"an admin with no grant", "none", "admin", "", true, reads + viewer + records + scanned},
		{"a responder with no grant", "none", "responder", "", true, reads + viewer},
		{"a read-only member", "viewer", "member", "", false, "/api/v1/projects/claims-portal"},
		{"an owner under json", "owner", "member", "json", true, reads},
		{"an owner under ai", "owner", "member", "ai", true, reads + viewer + records},
		{"a responder with no grant under ai", "none", "responder", "ai", true, reads + viewer},
	} {
		var asked []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.Path)
			switch {
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write(pages[0])
			case r.URL.Path == "/api/v1/projects/claims-portal":
				latest := ""
				if c.latest {
					latest = `,"latest":{"since":"2026-09-01T00:00:00Z"}`
				}
				_, _ = fmt.Fprintf(w, `{"project":{"id":"claims-portal","owners":[],"summary":"","access":{"level":%q,"everyone":false,"people":0,"groups":0}%s}}`, c.level, latest)
			case r.URL.Path == "/api/v1/projects/claims-portal/egress":
				_, _ = w.Write(egress["project_read"])
			case r.URL.Path == "/api/v1/egress":
				_, _ = w.Write(egress["setting_member"])
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
			case r.URL.Path == "/api/v1/fleet":
				_, _ = fmt.Fprintf(w, `{"apps":[],"next":null,"viewer":{"display":"Ada","role":%q}}`, c.role)
			case r.URL.Path == "/api/v1/scanners/data" && c.role != "admin": // the server refuses it, and audits the refusal as denied (#2743)
				denied = append(denied, c.who)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"admin_required","message":"only an admin can do this"}`))
			case r.URL.Path == "/api/v1/scanners/data":
				_, _ = w.Write([]byte(`{"databases":[]}`))
			default:
				_, _ = w.Write(whole["status"])
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		args := []string{"status"}
		if c.format != "" {
			args = append(args, "--format", c.format)
		}
		env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
		var o, e strings.Builder // Main with the clock at the fixture's: the week's count is under test, not today
		code := (&app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, clock: clock{now: func() time.Time { return at }, sleep: time.Sleep}}).main(args)
		server.Close()
		out, errs := o.String(), e.String()
		row := strings.Contains(out, line)
		if said := strings.Contains(out, "outbound: "); said != (c.format == "ai" && strings.Contains(c.asked, records)) {
			t.Errorf("%s: under ai it says outbound %v:\n%s", c.who, said, out)
		}
		if code != exitOK || errs != "" || strings.Join(asked, " ") != c.asked || row != (strings.Contains(c.asked, records) && c.format == "") {
			t.Errorf("%s: exit %d, stderr %q, row %v, asked\n%s\nwant\n%s\n%s", c.who, code, errs, row, strings.Join(asked, " "), c.asked, out)
		}
	}
	if len(denied) != 0 { // a member's agent looping on status would fill the Audit page with what reads as probing
		t.Errorf("the server audited a refused scanner data read for %q", denied)
	}
}

// Every person's and agent's status asks the reader's role for the scanners' warning (#2743): where that is all the
// role decides, a role read that fails is no admin, and the rest of status stands; below contributor, where the role
// decides what status shows at all, it fails status as before.
func TestAFailedRoleReadIsNoAdmin(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	egress := egressFixture(t)
	for _, c := range []struct {
		who, level string
		code       int
	}{{"an owner", "owner", exitOK}, {"an owner under ai", "owner", exitOK}, {"no grant", "none", exitError}} {
		var asked []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.Path)
			switch {
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write(pages[0])
			case r.URL.Path == "/api/v1/projects/claims-portal":
				_, _ = fmt.Fprintf(w, `{"project":{"id":"claims-portal","owners":[],"summary":"","access":{"level":%q,"everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`, c.level)
			case r.URL.Path == "/api/v1/projects/claims-portal/egress":
				_, _ = w.Write(egress["project_read"])
			case r.URL.Path == "/api/v1/egress":
				_, _ = w.Write(egress["setting_member"])
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
			case r.URL.Path == "/api/v1/fleet": // a blip of the role read
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal","message":"internal error"}`))
			default:
				_, _ = w.Write(whole["status"])
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		args := []string{"status"}
		if strings.HasSuffix(c.who, "under ai") {
			args = append(args, "--format", "ai")
		}
		env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
		var o, e strings.Builder
		code := (&app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, clock: clock{now: func() time.Time { return at }, sleep: time.Sleep}}).main(args)
		server.Close()
		out := o.String()
		if code != c.code || slices.Contains(asked, "/api/v1/scanners/data") || code == exitOK && !strings.Contains(out, "claims-portal") {
			t.Errorf("%s: exit %d, want %d, asked %q\n%s%s", c.who, code, c.code, asked, out, e.String())
		}
	}
}

func egressFixture(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "contracts", "testdata", "egress", "claims-portal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	return whole
}

// A read of the row that fails — an older host's 404, a 500 or a network error, on either GET — fails the
// row alone: it says it couldn't read, status renders everything else and exits 0, a read carrying no
// verdict.
func TestStatusOutboundThatCannotBeReadSaysSo(t *testing.T) {
	whole, pages := page(t), []json.RawMessage{}
	if json.Unmarshal(whole["gate_runs"], &pages) != nil {
		t.Fatal("the fixture's gate_runs is not a list of pages")
	}
	egress := egressFixture(t)
	for _, c := range []struct {
		name, path string
		status     int // 0: the connection is dropped
		want       string
	}{
		{"the records' 500", "/api/v1/projects/claims-portal/egress", 500, `couldn't read: internal error (500 internal)`},
		{"an older host's 404 for the records", "/api/v1/projects/claims-portal/egress", 404, `couldn't read: no such route (404 not_found)`},
		{"the records dropped", "/api/v1/projects/claims-portal/egress", 0, `couldn't read: `},
		{"via's 500", "/api/v1/egress", 500, `couldn't read: internal error (500 internal)`},
		{"an older host's 404 for via", "/api/v1/egress", 404, `couldn't read: no such route (404 not_found)`},
		{"via dropped", "/api/v1/egress", 0, `couldn't read: `},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == c.path && c.status == 0:
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
			case r.URL.Path == c.path:
				w.WriteHeader(c.status)
				_, _ = fmt.Fprintf(w, `{"error":%q,"message":%q}`, map[int]string{500: "internal", 404: "not_found"}[c.status], map[int]string{500: "internal error", 404: "no such route"}[c.status])
			case strings.HasSuffix(r.URL.Path, "/gate-runs"):
				_, _ = w.Write(pages[0])
			case r.URL.Path == "/api/v1/projects/claims-portal":
				_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0},"latest":{"since":"2026-09-01T00:00:00Z"}}}`))
			case r.URL.Path == "/api/v1/projects/claims-portal/egress":
				_, _ = w.Write(egress["project_read"])
			case r.URL.Path == "/api/v1/egress":
				_, _ = w.Write(egress["setting_member"])
			case r.URL.Path == "/auth/config":
				_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc"}`))
			default:
				_, _ = w.Write(whole["status"])
			}
		}))
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		t.Chdir(dir)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "status")
		server.Close()
		_, row, _ := strings.Cut(outboundLines(out), "  outbound     ")
		if code != exitOK || errs != "" || !strings.HasPrefix(row, c.want) || strings.Contains(row, "\n") || !strings.Contains(out, "Open in Canyon ") || strings.Contains(out, "recorded") {
			t.Errorf("%s: exit %d, stderr %q, the row %q, want it to start %q, in\n%s", c.name, code, errs, row, c.want, out)
		}
	}
}

// What the org's platform provisioned is listed under the environments (cli.md §6.1, #2432): the
// project read's resources[] in state provided, from the app-page fixture, each with its endpoint as
// text; Canyon's own records and the bare declarations are not. The whole rendering is the golden.
func TestStatusListsWhatThePlatformProvided(t *testing.T) {
	whole := page(t)
	s, latest := reads(t, whole, "status")
	var rows []resource
	if json.Unmarshal(whole["resources"], &rows) != nil {
		t.Fatal("the fixture's resources is not the shape the CLI decodes")
	}
	s.Provided = provided(rows)
	wants(t, "status-provided.txt", rendered(t, s, latest))
	spec, err := os.ReadFile(filepath.Join("..", "design", "specs", "cli.md"))
	if line := "`resources    s3 files · preview · s3://claims-portal-files   your platform's`"; err != nil || !strings.Contains(rendered(t, s, latest), "\n  "+strings.Trim(line, "`")+"\n") || !strings.Contains(string(spec), line) {
		t.Errorf("cli.md §6.1 and the golden do not say the same resources row %s (%v)", line, err)
	}
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	if line := "\nresource: s3 files · preview · s3://claims-portal-files · your platform's\n"; !strings.Contains(out.String(), line) || strings.Count(out.String(), "resource:") != 1 {
		t.Errorf("under --format ai, status said\n%s\nwanting exactly one line %q", out.String(), line)
	}
	s.Provided = []resource{{Name: "db", Type: "postgres", Env: "prod", State: "provided", Endpoint: "postgres://pg.example/db\x1b[2J"}}
	if got := rendered(t, s, latest); strings.ContainsRune(got, 0x1b) || !strings.Contains(got, "  resources    postgres db · production · postgres://pg.example/db\\x1b[2J   your platform's\n") {
		t.Errorf("an endpoint drove the terminal, or production was not said in words:\n%s", got)
	}
}

// A vulnerability database 5 days old makes every review "could not check", and nothing said so before (#2743):
// status warns an admin, whose read alone answers the databases' age, of each one that turns stale within a day, and
// of each one stale already, last, with the host's other warnings; one with more than a day left, or a tool this
// build doesn't know, says nothing.
func TestStatusWarnsOfScannerDataAboutToTurnStale(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	stale := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }
	s.Scanners = []scanDB{
		{Tool: "grype", BuiltAt: stale(-5*24*time.Hour + 18*time.Hour), StaleAt: stale(18 * time.Hour)},
		{Tool: "trivy", BuiltAt: stale(-2 * 24 * time.Hour), StaleAt: stale(3 * 24 * time.Hour)},
		{Tool: "trivy-java", BuiltAt: stale(-5*24*time.Hour - time.Hour), StaleAt: stale(-time.Hour)},
		{Tool: "osv", StaleAt: stale(time.Hour)},
	}
	wants(t, "status-scanners.txt", rendered(t, s, latest))
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai-scanners.txt", out.String())
}
