package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// rollbackHost serves a rollback (design/OPERATIONS.md §1): the status read with production's
// earlier versions, the rollback review the gate-runs route starts and its run read, the receipt for
// that run, and the promote run. Every POST is kept as "<route> <body>", so what was sent is provable.
type rollbackHost struct {
	status  json.RawMessage // the status read
	receipt json.RawMessage // the status read for ?run=, the review's receipt; nil: status
	review  json.RawMessage // the review's run read, {run: …}
	lines   []string        // the review's build.log rows
	level   string          // "read-only": the status read and every POST answer 403 team_required
	gate    *routeAnswer    // the gate-runs route's answer; nil: 202 gr_rb01
	promo   *routeAnswer    // the promotions route's answer; nil: 202 gr_9c41
	reads   []string        // the production approval's reads, in turn, the last one again
	input   string          // what is typed at the question
	tty     bool            // whether a person is at the terminal
	posts   []string
	asked   []string // every request, as its URI
	prefs   string   // GET /api/v1/preferences; "" answers it as the status
}

// routeAnswer is a route's status and body, in place of the host's 202.
type routeAnswer struct {
	code int
	body string
}

// reviewed is a review's run read in the given state: the run gr_rb01 of the package put back.
func reviewed(state, reason string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"run":{"id":"gr_rb01","package":"pkg_7a1c","state":%q,"failure_reason":%q,"blocking":0,"warnings":0,"findings":[]}}`, state, reason))
}

func (h *rollbackHost) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.asked = append(h.asked, r.Method+" "+r.URL.RequestURI())
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // this process's own request body
			h.posts = append(h.posts, strings.TrimPrefix(r.URL.Path, "/api/v1/projects/claims-portal/")+" "+string(body))
		}
		switch path := r.URL.Path; {
		case h.level == "read-only" && (r.Method == http.MethodPost || strings.HasSuffix(path, "/status")):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"team_required","message":"Only claims-portal's team can do this. Ask an owner to add you."}`)
		case r.Method == http.MethodPost && path == "/api/v1/projects/claims-portal/gate-runs":
			say := cmp.Or(h.gate, &routeAnswer{http.StatusAccepted, `{"run":{"id":"gr_rb01","state":"pending"}}`})
			w.WriteHeader(say.code)
			_, _ = io.WriteString(w, say.body)
		case r.Method == http.MethodPost && path == "/api/v1/projects/claims-portal/promotions":
			say := cmp.Or(h.promo, &routeAnswer{http.StatusAccepted, string(started)})
			w.WriteHeader(say.code)
			_, _ = io.WriteString(w, say.body)
		case strings.HasPrefix(path, "/api/v1/promotion-approvals/"):
			read := h.reads[0]
			if len(h.reads) > 1 {
				h.reads = h.reads[1:]
			}
			_, _ = io.WriteString(w, read)
		case path == "/api/v1/projects/claims-portal/status" && r.URL.Query().Has("run") && h.receipt != nil:
			_, _ = w.Write(h.receipt)
		case strings.HasPrefix(path, "/api/v1/gate-runs/gr_rb01/timeline"):
			rows := []string{`{"id":1,"ts":"2026-10-11T09:00:00Z","kind":"run.state","data":{"state":"running"}}`}
			for i, line := range h.lines {
				rows = append(rows, fmt.Sprintf(`{"id":%d,"ts":"2026-10-11T09:00:01Z","kind":"build.log","data":{"line":%q}}`, i+2, line))
			}
			_, _ = io.WriteString(w, `{"rows":[`+strings.Join(rows, ",")+`],"next":null}`)
		case path == "/api/v1/gate-runs/gr_rb01":
			_, _ = w.Write(h.review)
		case strings.HasPrefix(path, "/api/v1/gate-runs/gr_9c41/timeline"):
			_, _ = w.Write(live)
		case path == "/api/v1/preferences" && h.prefs != "":
			_, _ = io.WriteString(w, h.prefs)
		case path == "/auth/config":
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"oidc","org":"Meridian","device":false}`)
		case path == "/api/v1/projects/claims-portal":
			_, _ = io.WriteString(w, `{"project":{"id":"claims-portal","access":{"level":"contributor","everyone":false,"people":1,"groups":0}}}`)
		default:
			_, _ = w.Write(h.status)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// rollingBack runs `deploy --env prod` and args in a directory holding a canyon.toml, against h,
// where h.tty says whether a person can answer the question, and h.input is what they type.
func rollingBack(t *testing.T, h *rollbackHost, args ...string) (out, errs string, code int) {
	t.Helper()
	server := h.serve(t)
	t.Chdir(appDir(t))
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	var o, e strings.Builder
	a := &app{out: &o, err: &e, in: strings.NewReader(h.input), tty: h.tty, getenv: func(k string) string { return env[k] },
		clock: clock{now: func() time.Time { return at }, sleep: func(time.Duration) {}}}
	code = a.main(append([]string{"deploy", "--env", "prod"}, args...))
	local := strings.NewReplacer(server.URL, "http://localhost:7777", strings.TrimPrefix(server.URL, "http://"), "localhost:7777")
	return local.Replace(o.String()), local.Replace(e.String()), code
}

// rollbackRead is contracts/testdata/app-page/claims-portal-rollback-status.json: production runs v1.4,
// and its earlier versions are v1.3 (kept), v1.4 patched and v1.2.
func rollbackRead(t *testing.T) json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(pkgDir, "..", "contracts", "testdata", "app-page", "claims-portal-rollback-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// unlabelled is the rollback read with its newest earlier version sent without a label, as the CLI
// sends every package: the version is its id without pkg_ (routes.md, Naming a version).
func unlabelled(t *testing.T) json.RawMessage {
	t.Helper()
	return mutate(t, rollbackRead(t), func(read map[string]any) {
		row := read["earlier"].([]any)[0].(map[string]any)
		row["package"], row["version"] = "pkg_3f9e1c2a", "3f9e1c2a"
	})
}

const keptDigest = "sha256:cfd86d789f71ac8e8eb75d90a61aedbcc2767236bc29255ecf3395c1886bd93d"

// The command status prints is the command that puts the version back: an unlabelled version is its
// bare id, and the review the CLI asks for names the package by its id and the image production ran
// (design/OPERATIONS.md §1.9).
func TestVersionRoundTripUnlabelled(t *testing.T) {
	var s statusRead
	if err := json.Unmarshal(unlabelled(t), &s); err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, line := range strings.Split(rendered(t, s, nil), "\n") {
		if command, ok := strings.CutPrefix(strings.TrimSpace(line), "Put one back: canyon "); ok {
			said = strings.Fields(command)
		}
	}
	if !slices.Equal(said, []string{"deploy", "--env", "prod", "--version", "3f9e1c2a"}) {
		t.Fatalf("status printed %q under Earlier in production:\n%s", said, rendered(t, s, nil))
	}
	h := &rollbackHost{status: unlabelled(t), review: reviewed("clean", "")}
	_, errs, code := rollingBack(t, h, append(said[3:], "--yes")...)
	want := []string{
		`gate-runs {"package":"pkg_3f9e1c2a","rollback_to":"` + keptDigest + `"}`,
		`promotions {"gate_run":"gr_rb01"}`,
	}
	if code != exitOK || !slices.Equal(h.posts, want) {
		t.Errorf("exit %d (stderr %q); sent\n%q\nwant\n%q", code, errs, h.posts, want)
	}
	for _, named := range []string{"pkg_3f9e1c2a", "3f9e1c2a"} { // the id as the host names it resolves the same way
		h := &rollbackHost{status: unlabelled(t), review: reviewed("clean", "")}
		if _, _, code := rollingBack(t, h, "--version", named, "--yes"); code != exitOK || len(h.posts) == 0 || h.posts[0] != want[0] {
			t.Errorf("--version %s exited %d and sent %q", named, code, h.posts)
		}
	}
}

// `canyon rollback` and `canyon revert` are no commands: each is one usage line naming the one that
// puts a version back, as promote's redirect is (design/OPERATIONS.md §1.7). The global --version still
// prints the CLI's version; deploy's needs a value and --env prod.
func TestRollbackRedirect(t *testing.T) {
	for _, verb := range []string{"rollback", "revert"} {
		out, errs, code := run(t, nil, false, verb, "v1.3")
		want := "\n  ✗ canyon " + verb + " is: canyon deploy --env prod --version <version>. canyon status lists them.\n"
		if code != exitError || out != "" || errs != want {
			t.Errorf("canyon %s exited %d, printed %q, stderr %q", verb, code, out, errs)
		}
	}
	if help, _, _ := run(t, nil, false, "--help"); strings.Contains(help, "\n  rollback ") || strings.Contains(help, "\n  revert ") {
		t.Errorf("the help lists a rollback verb:\n%s", help)
	}
	if out, _, code := run(t, nil, false, "--version"); code != exitOK || out != "canyon "+Version+"\n" {
		t.Errorf("canyon --version exited %d and printed %q", code, out)
	}
	clash := "\n  ✗ --version puts an earlier version back in production: canyon deploy --env prod --version <version>\n"
	for _, args := range [][]string{
		{"deploy", "--version", "v1.3"},
		{"deploy", "--env", "prod", "--version"},
		{"deploy", "--env", "prod", "--version="},
		{"deploy", "--version", "--env", "prod"},
		{"deploy", "--env", "prod", "--version", "--yes"},
	} {
		if out, errs, code := run(t, nil, false, args...); code != exitError || out != "" || errs != clash {
			t.Errorf("%q exited %d, printed %q, stderr %q", args, code, out, errs)
		}
	}
	// Only a --version with nothing after it is the clash: any other mistake on the line is its own.
	if _, errs, code := run(t, nil, false, "deploy", "--env", "prod", "--bogus", "--version"); code != exitError || errs != "\n  ✗ flag provided but not defined: -bogus\n" {
		t.Errorf("an unknown flag before --version exited %d, stderr %q", code, errs)
	}
}

// The transcript of design/OPERATIONS.md §1.7: the version reviewed again, what ships and what it
// replaces, then the promotion of the review's run; --reason rides on the review.
func TestRollbackPutsTheKeptImageBack(t *testing.T) {
	spec := strings.TrimPrefix(specFence(t, "#### 6.3.1 Putting an earlier version back"), "$ canyon deploy --env prod --version v1.3 --reason \"INC-2231: v1.4 corrupts exports\" --yes\n")
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--reason", "INC-2231: v1.4 corrupts exports", "--yes")
	want := []string{
		`gate-runs {"package":"pkg_7a1c","rollback_to":"` + keptDigest + `","reason":"INC-2231: v1.4 corrupts exports"}`,
		`promotions {"gate_run":"gr_rb01"}`,
	}
	if code != exitOK || errs != "" || !slices.Equal(h.posts, want) {
		t.Errorf("exit %d, stderr %q; sent\n%q\nwant\n%q", code, errs, h.posts, want)
	}
	if !slices.Contains(h.asked, "GET /api/v1/projects/claims-portal/status?run=gr_rb01") {
		t.Errorf("the receipt for the review was never read: %q", h.asked)
	}
	wantsPromote(t, "deploy-prod-rollback.txt", out)
	if strings.Replace(spec, "canyon 0.4.1 ", "canyon dev ", 1) != out {
		t.Errorf("the rollback is not cli.md §6.3.1's transcript:\n%s", spec)
	}
}

// What production refuses of what a person dismissed is said before the promotion, in the amber of an advisory line,
// and stops nothing: a rollback is never held by the rule (contracts/routes.md, The promote receipt).
func TestRollbackSaysWhatProductionRefusesOfWhatWasDismissed(t *testing.T) {
	const line = "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card."
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), receipt: mutate(t, rollbackRead(t), func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["rules"] = append(r["rules"].([]any), map[string]any{"id": egressAllowed, "name": "Production allows everything preview connects to", "blocks": false, "passed": false, "why": line})
	})}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--reason", "INC-2231", "--yes")
	if code != exitOK || errs != "" || !slices.Contains(h.posts, `promotions {"gate_run":"gr_rb01"}`) {
		t.Errorf("exit %d, stderr %q, sent %q", code, errs, h.posts)
	}
	said, promotes := strings.Index(out, line), strings.Index(out, "\n  Promote\n")
	if said < 0 || promotes < 0 || said > promotes {
		t.Errorf("the line, at %d, comes before the promotion, at %d:\n%s", said, promotes, out)
	}
}

// A version that is not one of production's last five is refused before anything is sent, whatever the
// format and however few versions production ran, naming the versions that can go back; a host that lists
// none at all is too old to say. Under ai each is a REFUSED block with its steps (#2785): an agent that read
// the line alone retried it, then said Canyon contradicted itself. A production with no earlier version says
// so, and one asked for the version it runs now says that, never that Canyon lost images it never had (#2873).
// A person reads what to do next under the line, where the line doesn't already say it.
func TestRollbackRefusesAVersionNotEarlier(t *testing.T) {
	unkept := mutate(t, rollbackRead(t), func(read map[string]any) {
		for _, row := range read["earlier"].([]any) {
			row.(map[string]any)["kept"] = false
		}
	})
	first := mutate(t, rollbackRead(t), func(read map[string]any) { read["earlier"] = []any{} })
	old := mutate(t, rollbackRead(t), func(read map[string]any) { delete(read, "earlier") })
	for _, c := range []struct {
		name   string
		status json.RawMessage
		named  string
		format string
		want   string
		golden string
	}{
		{"five listed", rollbackRead(t), "v1.1", "human", "\n  v1.1 isn't one of production's last five versions. You can put back: v1.3.\n", ""},
		{"five listed, ai", rollbackRead(t), "v1.1", "ai", "", "deploy-prod-ai-rollback-not-earlier.txt"},
		{"none kept", unkept, "v1.1", "human", "\n  v1.1 isn't one of production's last five versions, and none of those can be put back: Canyon no longer has their images.\n" +
			"  A fix goes to production as a new version: canyon deploy, then canyon deploy --env prod.\n", ""},
		{"none kept, ai", unkept, "v1.1", "ai", "", "deploy-prod-ai-rollback-none-kept.txt"},
		{"no earlier version", first, "v1.1", "human", "\n  Production has no earlier version to put back.\n" +
			"  A fix goes to production as a new version: canyon deploy, then canyon deploy --env prod.\n", ""},
		{"no earlier version, ai", first, "v1.1", "ai", "", "deploy-prod-ai-rollback-no-earlier.txt"},
		{"what production runs", first, "v1.4", "human", "\n  v1.4 is what production runs now: there's nothing to put back.\n", ""},
		{"what production runs, by its id", first, "9e40", "human", "\n  9e40 is what production runs now: there's nothing to put back.\n", ""},
		{"what production runs, ai", first, "v1.4", "ai", "", "deploy-prod-ai-rollback-current.txt"},
		{"a host from before earlier[]", old, "v1.1", "human", "\n  This Canyon doesn't list production's earlier versions; it needs updating.\n" +
			"  Whoever runs this Canyon updates it; then this command works.\n", ""},
		{"a host from before earlier[], ai", old, "v1.1", "ai", "", "deploy-prod-ai-rollback-old-host.txt"},
	} {
		h := &rollbackHost{status: c.status, review: reviewed("clean", "")}
		out, errs, code := rollingBack(t, h, "--version", c.named, "--yes", "--format", c.format)
		if code != exitError || len(h.posts) != 0 || errs != "" {
			t.Errorf("%s: exit %d, sent %q, stderr %q", c.name, code, h.posts, errs)
		}
		if c.golden != "" {
			wantsPromote(t, c.golden, out)
		} else if !strings.HasSuffix(out, c.want) {
			t.Errorf("%s: printed\n%s\nwant it to end %q", c.name, out, c.want)
		}
	}
}

// A rollback that leaves preview on the version it took out of production says so, and that the next deploy --env prod
// ships it again: fix it in preview first (#2787). Where preview runs another version it says nothing of preview.
func TestARollbackSaysPreviewStillRunsWhatItTookOut(t *testing.T) {
	same := mutate(t, rollbackRead(t), func(read map[string]any) {
		envs := read["envs"].(map[string]any)
		preview := map[string]any{}
		for k, v := range envs["prod"].(map[string]any) {
			preview[k] = v
		}
		preview["url"] = "http://claims-portal.preview.localhost:7777"
		envs["preview"] = preview
	})
	h := &rollbackHost{status: same, review: reviewed("clean", "")}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	if code != exitOK || errs != "" {
		t.Errorf("ai: exit %d, stderr %q", code, errs)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-preview-keeps.txt", out)
	h = &rollbackHost{status: same, review: reviewed("clean", "")}
	out, _, code = rollingBack(t, h, "--version", "v1.3", "--yes")
	const line = "\n  Preview still runs v1.4, the version this rollback took out of production: fix it there with canyon deploy first, or the next canyon deploy --env prod ships it again.\n"
	if code != exitOK || !strings.Contains(out, line) {
		t.Errorf("human: exit %d, want %q in\n%s", code, line, out)
	}
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("clean", "")} // preview runs v1.3, what goes back
	if out, _, _ = rollingBack(t, h, "--version", "v1.3", "--yes"); strings.Contains(out, "Preview still runs") {
		t.Errorf("a preview on another version is said:\n%s", out)
	}
	unnamed := mutate(t, same, func(read map[string]any) { // no package named on either: nothing says they are one version
		for _, e := range read["envs"].(map[string]any) {
			delete(e.(map[string]any), "package")
		}
	})
	h = &rollbackHost{status: unnamed, review: reviewed("clean", "")}
	if out, _, _ = rollingBack(t, h, "--version", "v1.3", "--yes"); strings.Contains(out, "Preview still runs") {
		t.Errorf("two environments naming no package are said to run one version:\n%s", out)
	}
}

// Today's review blocks the version: the verdict, nothing promoted, exit 2, and what a person can do.
func TestRollbackBlockedTodayShipsNothing(t *testing.T) {
	res := goldenRun(t).Result
	res.ID, res.Package = "gr_rb01", "pkg_7a1c"
	body, err := json.Marshal(map[string]any{"run": res})
	if err != nil {
		t.Fatal(err)
	}
	h := &rollbackHost{status: rollbackRead(t), review: body}
	out, _, code := rollingBack(t, h, "--version", "v1.3", "--yes")
	if code != exitBlocked || len(h.posts) != 1 {
		t.Errorf("exit %d, sent %q", code, h.posts)
	}
	wantsPromote(t, "deploy-prod-rollback-blocked.txt", out)
	h = &rollbackHost{status: rollbackRead(t), review: body}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	if code != exitBlocked || len(h.posts) != 1 || errs != "" || strings.Contains(out, "Fix every finding") {
		t.Errorf("ai: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-blocked.txt", out)
}

// The image production ran is gone: the review ends failed, the server's line says why, nothing is
// rebuilt and nothing is promoted.
func TestRollbackRefusesWhenTheImageIsGone(t *testing.T) {
	gone := "v1.3's image is no longer on this Canyon, so it can't be put back as it ran. Deploy v1.3 to preview and promote it if you want it rebuilt."
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("failed", "image_gone"), lines: []string{gone}}
	_, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
	if code != exitError || len(h.posts) != 1 || !strings.HasSuffix(errs, "\n  ✗ "+gone+"\n") {
		t.Errorf("exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	h = &rollbackHost{status: rollbackRead(t), review: reviewed("failed", "image_gone"), lines: []string{gone}}
	out, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes", "--format", "ai")
	if code != exitError || len(h.posts) != 1 || errs != "" {
		t.Errorf("ai: exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-rollback-gone.txt", out)
}

// Putting a version back is a contributor's act (design/OPERATIONS.md §1.8): a read-only member is
// refused by the status read, and nothing is sent.
func TestRollbackNeedsAContributor(t *testing.T) {
	h := &rollbackHost{status: rollbackRead(t), review: reviewed("clean", ""), level: "read-only"}
	_, errs, code := rollingBack(t, h, "--version", "v1.3", "--yes")
	if code != exitError || len(h.posts) != 0 || !strings.Contains(errs, "Only claims-portal's team can do this.") {
		t.Errorf("a read-only member exited %d, sent %q, stderr %q", code, h.posts, errs)
	}
}

// A rebuild of the version production runs is named apart from it (#2300): the label alone read as the
// version running now offered back as an earlier one.
func TestStatusNamesAnEarlierBuildOfTheVersionProductionRuns(t *testing.T) {
	var s statusRead
	if err := json.Unmarshal(mutate(t, rollbackRead(t), func(read map[string]any) {
		delete(read["earlier"].([]any)[1].(map[string]any), "patched") // pkg_9e40, the version production runs
	}), &s); err != nil {
		t.Fatal(err)
	}
	if want := "  Earlier in production  v1.3 (2 Oct) · v1.4 earlier build (30 Sep) · v1.2 (28 Sep)\n"; !strings.Contains(rendered(t, s, nil), want) {
		t.Errorf("no %q in\n%s", want, rendered(t, s, nil))
	}
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	if want := "\nearlier in production: v1.3, v1.4 earlier build, v1.2\n"; !strings.Contains(out.String(), want) {
		t.Errorf("status under ai: no %q in\n%s", want, out.String())
	}
}

// Status ends with production's earlier versions and the command that puts one back.
func TestStatusListsEarlierVersions(t *testing.T) {
	var s statusRead
	if err := json.Unmarshal(rollbackRead(t), &s); err != nil {
		t.Fatal(err)
	}
	wants(t, "status-earlier.txt", rendered(t, s, nil))
	if spec := specFence(t, "#### 6.1.3 Earlier in production"); !strings.Contains(rendered(t, s, nil), "\n"+spec+"\n") {
		t.Errorf("status's earlier lines are not cli.md §6.1.3's:\n%s", spec)
	}
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, nil, at)
	if want := "\nearlier in production: v1.3, v1.4 patched, v1.2\nput one back: canyon deploy --env prod --version v1.3\n"; !strings.Contains(out.String(), want) {
		t.Errorf("status under ai: no %q in\n%s", want, out.String())
	}
}
