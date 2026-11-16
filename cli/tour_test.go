package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tourHost is the two reads canyon demo tour makes of the tour's app: its status and its runs list.
type tourHost struct {
	auth    string   // what /auth/config says of sign-in; "" is "off", the demo's
	preview string   // the package preview runs; "" when nothing runs there
	prod    string   // the package production runs; "" when nothing runs there
	states  []string // each run's state, as the runs list answers them
}

func (h *tourHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/auth/config":
		auth := h.auth
		if auth == "" {
			auth = "off"
		}
		_, _ = io.WriteString(w, `{"canyon":true,"auth":"`+auth+`","org":"","device":false}`)
	case "/api/v1/projects/amber-gecko-w4g/status":
		var envs []string
		if h.preview != "" {
			envs = append(envs, `"preview":{"package":"`+h.preview+`"}`)
		}
		if h.prod != "" {
			envs = append(envs, `"prod":{"package":"`+h.prod+`"}`)
		}
		_, _ = io.WriteString(w, `{"envs":{`+strings.Join(envs, ",")+`},"last_run":null,"promote_run":null,"receipt":{},"earlier":[]}`)
	case "/api/v1/projects/amber-gecko-w4g/gate-runs":
		rows := make([]string, len(h.states))
		for i, s := range h.states {
			rows[i] = `{"id":"gr_` + s + `","package":"pkg_x","state":"` + s + `"}`
		}
		_, _ = io.WriteString(w, `{"runs":[`+strings.Join(rows, ",")+`],"next":null}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found","message":"no such route"}`)
	}
}

// tourIn is a machine taking the tour: a host, a config directory and a folder to plant in.
func tourIn(t *testing.T, h *tourHost) func(args ...string) (out, errs string, code int) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return tourAt(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"})
}

// tourAt is the tour with env, in a folder of its own.
func tourAt(t *testing.T, env map[string]string) func(args ...string) (out, errs string, code int) {
	t.Helper()
	t.Chdir(t.TempDir())
	return func(args ...string) (string, string, int) {
		t.Helper()
		return run(t, env, false, append([]string{"demo", "tour"}, args...)...)
	}
}

// tourDeployed is what the first canyon deploy leaves in the tour's folder.
func tourDeployed(t *testing.T) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(tourDir, projectFile), []byte("app = \"amber-gecko-w4g\"\n"), 0o600))
}

func TestTourPrintsTheChapterForAnAgentAndForAPerson(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	out, errs, code := run(t, env, false, "demo", "tour")
	if code != exitOK || errs != "" || !strings.Contains(out, "Canyon tour · chapter 1 of 3 · Standards: your agent ignores the design system") {
		t.Fatalf("exit %d, stderr %q, a person reads\n%s", code, errs, out)
	}
	env["CLAUDECODE"] = "1"
	out, errs, code = run(t, env, false, "demo", "tour")
	if code != exitOK || errs != "" || !strings.HasPrefix(out, "CANYON TOUR — CHAPTER 1 of 3: Standards: your agent ignores the design system") {
		t.Fatalf("exit %d, stderr %q, an agent reads\n%s", code, errs, out)
	}
	for _, want := range []string{"Run canyon fleet.", "    What happened:\n    How Canyon helped:\n    Without Canyon:\n    At scale:", "/#/guardrails/policies?rule=design.org-system\n    On the marked row, \"Apps use the org's design system\", choose Blocking.", `not "Save, tightening in 14 days"`, "Build me a claims tracker, and deploy it.", "Run canyon demo tour plant, then run canyon deploy in ./canyon-tour, unchanged.", "  What next?\n  1. Next chapter: Security: your agent writes a flaw", "  2. Skip to the end\n  3. Try this on my own app"} {
		if !strings.Contains(out, want) {
			t.Errorf("chapter 1's instructions lack %q", want)
		}
	}
}

func TestTourPlantWritesTheAppAndSaysWhatIsPlanted(t *testing.T) {
	tour := tourIn(t, &tourHost{})
	out, errs, code := tour("plant")
	if code != exitOK || errs != "" || !strings.Contains(out, "Wrote 4 files into ./canyon-tour") || !strings.Contains(out, "PLANTED: the tour wrote this app") {
		t.Fatalf("exit %d, stderr %q, printed\n%s", code, errs, out)
	}
	if _, err := os.Stat(filepath.Join(tourDir, "components.json")); err == nil {
		t.Error("chapter 1's first plant declares a design system, so design.org-system could not block it")
	}
	// The app says what it is before its first deploy names it: a description and no app, which the deploy fills in.
	toml, err := os.ReadFile(filepath.Join(tourDir, projectFile))
	must(t, err)
	if p, err := readProject(tourDir); !strings.Contains(string(toml), "description = ") || p.App != "" || err == nil {
		t.Errorf("chapter 1's app has no canyon.toml with a description and no app yet: %q, %v", toml, err)
	}
	body, err := os.ReadFile(filepath.Join(tourDir, "server.js"))
	must(t, err)
	if strings.Contains(string(body), "req.query") {
		t.Errorf("chapter 1's app reads the request, so a second rule could block it:\n%s", body)
	}
}

// The second plant is what design.org-system passes under Canyon's own design system: a
// components.json that lists the @canyon registry (gate/design.go).
func TestTheDesignPlantListsTheCanyonRegistry(t *testing.T) {
	body, err := tourFiles.ReadFile("testdata/tour/design/components.json")
	must(t, err)
	var doc struct {
		Registries map[string]string `json:"registries"`
	}
	must(t, json.Unmarshal(body, &doc))
	if doc.Registries["@canyon"] == "" {
		t.Errorf("components.json lists no @canyon registry:\n%s", body)
	}
	for _, page := range []string{"testdata/tour/design/public/index.html", "testdata/tour/search/public/index.html"} {
		html, err := tourFiles.ReadFile(page)
		must(t, err)
		if !strings.Contains(string(html), `href="canyon.css"`) || strings.Contains(string(html), "<style>") {
			t.Errorf("%s is not styled by the design system's theme alone", page)
		}
	}
}

func TestTourPlantRefusesAFolderItDidNotWrite(t *testing.T) {
	tour := tourIn(t, &tourHost{})
	must(t, os.Mkdir(tourDir, 0o750))
	must(t, os.WriteFile(filepath.Join(tourDir, "server.js"), []byte("mine\n"), 0o600))
	_, errs, code := tour("plant")
	if code != exitError || !strings.Contains(errs, "./canyon-tour is here already, from an earlier tour or your own work") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if body, _ := os.ReadFile(filepath.Join(tourDir, "server.js")); string(body) != "mine\n" {
		t.Errorf("the refused plant changed server.js to %q", body)
	}
}

// A chapter is done when its work shows on the host, never because the agent says so.
func TestTourNextRefusesUntilTheChapterIsDone(t *testing.T) {
	h := &tourHost{}
	tour := tourIn(t, h)
	refused := func(step, want string) {
		t.Helper()
		out, errs, code := tour("next")
		if code != exitError || out != "" || !strings.Contains(errs, want) {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q, want %q", step, code, out, errs, want)
		}
	}
	moved := func(step, want string) {
		t.Helper()
		out, errs, code := tour("next")
		if code != exitOK || errs != "" || !strings.Contains(out, want) {
			t.Fatalf("%s: exit %d, stderr %q, stdout %q, want %q", step, code, errs, out, want)
		}
	}
	refused("nothing planted", "Chapter 1 isn't done: its files are not planted. Run canyon demo tour plant.")
	if _, errs, code := tour("plant"); code != exitOK {
		t.Fatalf("plant: exit %d, %q", code, errs)
	}
	refused("planted, never deployed", "Chapter 1 isn't done: the app in ./canyon-tour has not been deployed yet.")
	tourDeployed(t)
	if _, errs, code := tour("plant"); code != exitError || !strings.Contains(errs, "Canyon has not reviewed the planted app yet") {
		t.Fatalf("the design plant before a review: exit %d, %q", code, errs)
	}
	refused("fixed before Canyon saw it", "Chapter 1 isn't done: Canyon has not blocked what the tour planted yet.")
	h.states = []string{"blocked"}
	refused("blocked, not fixed", "Chapter 1 isn't done: preview does not run the fixed app yet.")
	out, errs, code := tour("plant")
	if code != exitOK || errs != "" || !strings.Contains(out, "Wrote 3 files into ./canyon-tour") || !strings.Contains(out, "in the org's design system") {
		t.Fatalf("the design plant: exit %d, stderr %q, printed\n%s", code, errs, out)
	}
	if _, err := os.Stat(filepath.Join(tourDir, "components.json")); err != nil {
		t.Errorf("the design plant wrote no components.json: %v", err)
	}
	h.preview, h.states = "pkg_one", []string{"clean", "blocked"}
	moved("blocked, then rebuilt", "Chapter 1 is done. Run: canyon demo tour")

	refused("chapter 2, nothing planted", "Chapter 2 isn't done: its files are not planted.")
	out, errs, code = tour("plant")
	if code != exitOK || errs != "" || !strings.Contains(out, "PLANTED: server.js now has a search that builds its SQL from the request.") {
		t.Fatalf("chapter 2's plant: exit %d, stderr %q, printed\n%s", code, errs, out)
	}
	refused("chapter 1's block does not count", "Chapter 2 isn't done: Canyon has not blocked what the tour planted yet.")
	h.states = []string{"blocked", "clean", "blocked"}
	refused("blocked, not fixed", "Chapter 2 isn't done: preview does not run the fixed app yet.")
	h.preview, h.states = "pkg_two", []string{"clean", "blocked", "clean", "blocked"}
	moved("blocked, then fixed", "Chapter 2 is done. Run: canyon demo tour")

	if _, errs, code := tour("plant"); code != exitError || !strings.Contains(errs, "Chapter 3 plants nothing: preview already runs the app") {
		t.Errorf("a plant in chapter 3: exit %d, %q", code, errs)
	}
	refused("not promoted", "Chapter 3 isn't done: the app is not in production yet.")
	h.prod = "pkg_two"
	moved("in production", "Chapter 3 is done. Run: canyon demo tour")

	out, _, _ = tour()
	if !strings.Contains(out, "Canyon tour · the end") {
		t.Errorf("after chapter 2 the tour prints\n%s", out)
	}
	if _, errs, code := tour("plant"); code != exitError || !strings.Contains(errs, "the end plants nothing") {
		t.Errorf("a plant at the end: exit %d, %q", code, errs)
	}
}

func TestTourEndSkipsAheadAndResetStartsAgain(t *testing.T) {
	tour := tourIn(t, &tourHost{})
	if out, errs, code := tour("end"); code != exitOK || errs != "" || !strings.Contains(out, "Skipped to the end of the tour.") {
		t.Fatalf("end: exit %d, stderr %q, stdout %q", code, errs, out)
	}
	if out, _, _ := tour("status"); !strings.Contains(out, "The tour is finished.") {
		t.Errorf("status after end:\n%s", out)
	}
	if out, errs, code := tour("reset"); code != exitOK || errs != "" || !strings.Contains(out, "The tour starts again at chapter 1.") {
		t.Fatalf("reset: exit %d, stderr %q, stdout %q", code, errs, out)
	}
	if out, _, _ := tour("status"); !strings.Contains(out, "Chapter 1 of 3 · Standards: your agent ignores the design system") {
		t.Errorf("status after reset:\n%s", out)
	}
}

// canyon demo tour skip is the visitor's way past a chapter, and each chapter it lands on stands alone:
// its plant writes what the skipped ones would have left.
func TestTourSkipLandsOnAChapterThatStandsAlone(t *testing.T) {
	h := &tourHost{}
	tour := tourIn(t, h)
	if out, errs, code := tour("skip"); code != exitOK || errs != "" || !strings.Contains(out, "Skipped chapter 1. Run: canyon demo tour") {
		t.Fatalf("skip: exit %d, stderr %q, stdout %q", code, errs, out)
	}
	out, errs, code := tour("plant")
	if code != exitOK || errs != "" || !strings.Contains(out, "Wrote 6 files into ./canyon-tour") || !strings.Contains(out, "PLANTED: server.js now has a search") {
		t.Fatalf("chapter 2's plant after a skip: exit %d, stderr %q, printed\n%s", code, errs, out)
	}
	for _, name := range []string{"package.json", "components.json", "public/canyon.css"} {
		if _, err := os.Stat(filepath.Join(tourDir, filepath.FromSlash(name))); err != nil {
			t.Errorf("the plant did not write %s: %v", name, err)
		}
	}
	tourDeployed(t)
	h.preview, h.states = "pkg_one", []string{"clean", "blocked"}
	if out, errs, code := tour("next"); code != exitOK || !strings.Contains(out, "Chapter 2 is done.") {
		t.Fatalf("next after the skipped chapter's work: exit %d, stderr %q, stdout %q", code, errs, out)
	}
	if _, errs, code := tour("plant"); code != exitError || !strings.Contains(errs, "Chapter 3 plants nothing: preview already runs the app") {
		t.Errorf("chapter 3's plant with an app in preview: exit %d, %q", code, errs)
	}
}

func TestTourSkipToProductionPlantsTheFinishedApp(t *testing.T) {
	tour := tourIn(t, &tourHost{})
	tour("skip")
	tour("skip")
	out, errs, code := tour("plant")
	if code != exitOK || errs != "" || !strings.Contains(out, "Wrote 6 files into ./canyon-tour") || !strings.Contains(out, "It has no planted flaw.") {
		t.Fatalf("chapter 3's plant after two skips: exit %d, stderr %q, printed\n%s", code, errs, out)
	}
	body, err := os.ReadFile(filepath.Join(tourDir, "server.js"))
	must(t, err)
	if strings.Contains(string(body), "req.query") {
		t.Errorf("the finished app reads the request:\n%s", body)
	}
	tour("skip")
	if out, _, _ := tour("skip"); !strings.Contains(out, "The tour is finished.") {
		t.Errorf("a skip at the end printed\n%s", out)
	}
}

func TestTourRefusesAFormItDoesNotHave(t *testing.T) {
	tour := tourIn(t, &tourHost{})
	out, errs, code := tour("jump")
	if code != exitError || out != "" || !strings.Contains(errs, `demo tour has no form "jump"`) || !strings.Contains(errs, "canyon demo tour next") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// The chapters send an agent to this binary's verbs and to the tour's own forms, and to nothing else.
func TestTheTourNamesNoCommandThisBinaryLacks(t *testing.T) {
	forms := map[string]bool{"plant": true, "next": true, "skip": true, "end": true, "reset": true}
	named := regexp.MustCompile(`\bcanyon (demo tour )?([a-z]+)`)
	for _, texts := range []map[int]string{tourAgent, tourHuman} {
		for chapter, text := range texts {
			for _, m := range named.FindAllStringSubmatch(text, -1) {
				if m[1] != "" {
					if !forms[m[2]] && !tourProse[m[2]] {
						t.Errorf("chapter %d names canyon demo tour %s, which is no form of it", chapter, m[2])
					}
					continue
				}
				if !isVerb(m[2]) && !tourProse[m[2]] {
					t.Errorf("chapter %d names canyon %s, which this binary cannot run", chapter, m[2])
				}
			}
		}
	}
}

// tourProse is the words that follow "canyon" or "canyon demo tour" in a sentence and name no command.
var tourProse = map[string]bool{"command": true, "once": true, "folder": true, "brings": true, "shows": true, "and": true, "is": true, "with": true}

func isVerb(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

// Chapter 2 is one round: the line it plants is in the file, and the fix its instructions carry is
// that line with the request bound instead of concatenated.
func TestChapterTwosFixIsOneLineFromWhatItPlants(t *testing.T) {
	planted, err := tourFiles.ReadFile("testdata/tour/search/server.js")
	must(t, err)
	const flaw = `db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all()`
	const fix = `db.prepare("SELECT * FROM claims WHERE policy_number = ?").all(req.query.policy)`
	if strings.Count(string(planted), flaw) != 1 {
		t.Errorf("the planted search does not hold the flaw once:\n%s", planted)
	}
	if !strings.Contains(tourAgent[tourSecurity], "const rows = "+fix+";") {
		t.Errorf("chapter 2's instructions do not carry the fix %q", fix)
	}
}

// The tour plants and moves on only on the laptop demo: a Canyon on this machine with sign-in off.
// Anywhere else it writes nothing, says why, and names the command that starts the demo.
func TestTourWorksOnlyOnTheDemo(t *testing.T) {
	tour := tourIn(t, &tourHost{auth: "oidc"})
	for _, form := range []string{"plant", "next"} {
		out, errs, code := tour(form)
		if code != exitError || out != "" || !strings.Contains(errs, "The tour runs only on the demo on this machine, and 127.0.0.1:") ||
			!strings.Contains(errs, "isn't the demo: it signs people in.\n\n  To start the demo: canyon demo up") {
			t.Errorf("%s on a host that signs people in: exit %d, stdout %q, stderr %q", form, code, out, errs)
		}
	}
	if _, err := os.Stat(tourDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused plant wrote ./canyon-tour (%v)", err)
	}
	tour = tourAt(t, map[string]string{"CANYON_HOST": "https://canyon.example.invalid", "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"})
	if out, errs, code := tour("plant"); code != exitError || out != "" || !strings.Contains(errs, "The tour runs only on the demo on this machine, and canyon.example.invalid isn't the demo: it's on another machine.") {
		t.Errorf("plant on a host elsewhere: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// An app Canyon passed in chapter 1, and never blocked, means the design-system rule did not hold:
// the tour says so, and where a person sets it, instead of waiting on a block that cannot come.
func TestTourSaysWhenTheDesignRuleDidNotBlock(t *testing.T) {
	h := &tourHost{}
	tour := tourIn(t, h)
	if _, errs, code := tour("plant"); code != exitOK {
		t.Fatalf("plant: exit %d, %q", code, errs)
	}
	tourDeployed(t)
	h.preview, h.states = "pkg_one", []string{"clean"}
	for _, form := range []string{"next", "plant"} {
		out, errs, code := tour(form)
		if code != exitError || out != "" || !strings.Contains(errs, "Chapter 1 isn't done: Canyon passed the planted app, so the design-system rule isn't Blocking yet. Set it at ") ||
			!strings.Contains(errs, "/#/guardrails/policies?rule=design.org-system") {
			t.Errorf("%s after a pass: exit %d, stdout %q, stderr %q", form, code, out, errs)
		}
	}
}

// A place that cannot be read is said, never taken for chapter 1 in silence; reset still starts over.
func TestTourSaysAPlaceItCannotRead(t *testing.T) {
	server := httptest.NewServer(&tourHost{})
	t.Cleanup(server.Close)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	tour := tourAt(t, env)
	path, err := tourPath(func(k string) string { return env[k] })
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("{\"chapter\": 2,"), 0o600))
	if out, errs, code := tour(); code != exitError || out != "" || !strings.Contains(errs, "The tour's saved place (") || !strings.Contains(errs, "can't be read: ") || !strings.Contains(errs, "To start over: canyon demo tour reset") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if out, errs, code := tour("reset"); code != exitOK || !strings.Contains(out, "The tour starts again at chapter 1.") {
		t.Fatalf("reset: exit %d, stderr %q, stdout %q", code, errs, out)
	}
	if out, _, code := tour("status"); code != exitOK || !strings.Contains(out, "Chapter 1 of 3") {
		t.Errorf("status after reset: exit %d\n%s", code, out)
	}
}

// packageDir is this package's folder, read before any test moves into a folder of its own.
var packageDir, _ = os.Getwd()

// goldenMatch holds got to testdata/name; CANYON_UPDATE_GOLDEN=1 rewrites the file.
func goldenMatch(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(packageDir, "testdata", name)
	if os.Getenv("CANYON_UPDATE_GOLDEN") == "1" {
		must(t, os.WriteFile(path, []byte(got), 0o644)) //nolint:gosec // a fixture, world-readable like every file in the repo
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixture this test named
	must(t, err)
	if got != string(want) {
		t.Errorf("%s is not what canyon printed; got\n%s", path, got)
	}
}

// Every chapter's text, as an agent and as a person reads it, is a file a reviewer reads whole.
func TestTheChaptersAreTheirGoldenFiles(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	path, err := tourPath(func(k string) string { return env[k] })
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	for chapter, name := range map[int]string{tourFirst: "1", tourSecurity: "2", tourProd: "3", tourEnd: "end"} {
		must(t, os.WriteFile(path, []byte(fmt.Sprintf(`{"chapter":%d}`, chapter)), 0o600))
		person, _, _ := run(t, env, false, "demo", "tour")
		goldenMatch(t, "tour-"+name+"-person.txt", person)
		status, _, _ := run(t, env, false, "demo", "tour", "status")
		goldenMatch(t, "tour-"+name+"-status.txt", status)
		agent, _, _ := run(t, map[string]string{"XDG_CONFIG_HOME": env["XDG_CONFIG_HOME"], "CLAUDECODE": "1"}, false, "demo", "tour")
		goldenMatch(t, "tour-"+name+"-agent.txt", agent)
	}
}

// What each plant prints: chapter 1's app and its page in the design system, chapter 2's search, and
// chapter 3's finished app after two skips.
func TestThePlantsAreTheirGoldenFiles(t *testing.T) {
	h := &tourHost{}
	tour := tourIn(t, h)
	out, _, _ := tour("plant")
	goldenMatch(t, "tour-plant-app.txt", out)
	tourDeployed(t)
	h.states = []string{"blocked"}
	out, _, _ = tour("plant")
	goldenMatch(t, "tour-plant-design.txt", out)
	h.preview, h.states = "pkg_one", []string{"clean", "blocked"}
	tour("next")
	out, _, _ = tour("plant")
	goldenMatch(t, "tour-plant-search.txt", out)
	dir, err := filepath.Abs(tourDir)
	must(t, err)
	out, _, _ = tour("status")
	goldenMatch(t, "tour-status-planted.txt", strings.ReplaceAll(out, dir, "/home/you/canyon-tour"))

	tour = tourIn(t, &tourHost{})
	tour("skip")
	tour("skip")
	out, _, _ = tour("plant")
	goldenMatch(t, "tour-plant-finished.txt", out)
}

// The way to one's own app says first that the design-system rule the tour set still blocks here, and
// gives the one link that sets it back: an app with a page of its own would be blocked by it.
func TestTheWayToYourOwnAppSetsTheRuleBackFirst(t *testing.T) {
	link := "/#/guardrails/policies?rule=design.org-system"
	for name, text := range map[string]string{"the end, as the agent reads it": tourAgent[tourEnd], "the end, as a person reads it": tourHuman[tourEnd], "chapter 1's third choice": tourAgent[tourFirst]} {
		way := text[strings.Index(text, "own app"):]
		if !strings.Contains(way[:min(len(way), 600)], link) || !strings.Contains(way[:min(len(way), 600)], "Advisory") {
			t.Errorf("%s does not set the rule back to Advisory, with %s, before one's own app", name, link)
		}
	}
}

// Every beat that has the agent run canyon check or canyon deploy names the folder they run in: an agent's
// shell may be in the folder above, where canyon deploy would make a second app (the final visitor run).
func TestEveryBeatThatDeploysNamesTheTourFolder(t *testing.T) {
	for chapter, text := range tourAgent {
		for _, beat := range strings.Split(text, "\nBeat ")[1:] {
			title, _, _ := strings.Cut(beat, "\n")
			if (strings.Contains(beat, "canyon check") || strings.Contains(beat, "canyon deploy")) && !strings.Contains(beat, "./"+tourDir) {
				t.Errorf("chapter %d, beat %s, runs canyon check or deploy without naming ./%s", chapter, title, tourDir)
			}
		}
	}
}
