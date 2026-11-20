package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// promoteHost serves the fixture: the promote run's timeline, the packages list by its cursor, the
// status on any other GET, the promotions route's answer on the POST. Bodies are kept, so "it sent
// nothing" is provable; tick is the verb's clock.
type promoteHost struct {
	status   json.RawMessage
	code     int
	answer   json.RawMessage
	timeline json.RawMessage            // gr_9c41's one page
	pages    map[string]json.RawMessage // the packages list by cursor; nil: preview runs the directory's own tree
	posts    []string
	tick     time.Time
	project  string   // the project read after the promotion lands; "" answers it with no access
	secrets  string   // the secrets list; "" answers it empty
	auth     string   // GET /auth/config's auth; "" is oidc
	reads    []string // the production approval's reads, answered in turn, the last one again; "" is a 404
	public   string   // the public URL login saved for this host; "" saves no config
	run      reply    // the status read's promote_run, read alone (GET /api/v1/gate-runs/:id); no code answers the status, as any other GET
	gets     []string // every read's path, so "it asked nothing there" is provable
	prefs    string   // GET /api/v1/preferences; "" answers it as the status, which names no admin
	prefsErr bool     // GET /api/v1/preferences answers 500
}

func (h *promoteHost) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			h.gets = append(h.gets, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/api/v1/gate-runs/gr_9c41/timeline") {
				_, _ = w.Write(h.timeline)
				return
			}
			if r.URL.Path == "/auth/config" {
				_, _ = io.WriteString(w, `{"canyon":true,"auth":"`+cmpOr(h.auth, "oidc")+`","org":"Meridian","device":false}`)
				return
			}
			if r.URL.Path == "/api/v1/preferences" && h.prefsErr {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"internal","message":"internal error"}`)
				return
			}
			if r.URL.Path == "/api/v1/preferences" && h.prefs != "" {
				_, _ = io.WriteString(w, h.prefs)
				return
			}
			if r.URL.Path == "/api/v1/projects/claims-portal/secrets" {
				_, _ = io.WriteString(w, cmpOr(h.secrets, `{"secrets":[]}`))
				return
			}
			if r.URL.Path == "/api/v1/projects/claims-portal" {
				_, _ = io.WriteString(w, cmpOr(h.project, `{"project":{"id":"claims-portal"}}`))
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/v1/promotion-approvals/") {
				read := h.reads[0]
				if len(h.reads) > 1 {
					h.reads = h.reads[1:]
				}
				if read == "" {
					w.WriteHeader(http.StatusNotFound)
					read = `{"error":"not_found","message":"no approval has that code"}`
				}
				_, _ = io.WriteString(w, read)
				return
			}
			if h.run.code != 0 && strings.HasPrefix(r.URL.Path, "/api/v1/gate-runs/") && !strings.HasSuffix(r.URL.Path, "/timeline") {
				w.WriteHeader(h.run.code)
				_, _ = io.WriteString(w, h.run.body)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/packages") {
				_, _ = w.Write(h.pages[r.URL.Query().Get("cursor")])
				return
			}
			_, _ = w.Write(h.status)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // this process's own request body
		h.posts = append(h.posts, string(body))
		w.WriteHeader(h.code)
		_, _ = w.Write(h.answer)
	}))
	t.Cleanup(server.Close)
	return server
}

// hesitating is the answer, typed five minutes after it was asked for: the clock moves only while
// the terminal waits, because the seconds a promotion took are the server's.
type hesitating struct {
	typed *strings.Reader
	host  *promoteHost
}

func (r hesitating) Read(p []byte) (int, error) {
	r.host.tick = r.host.tick.Add(5 * time.Minute)
	return r.typed.Read(p)
}

// promoting runs `deploy --env prod` in a directory holding a canyon.toml, against h.
func promoting(t *testing.T, h *promoteHost, input string, tty bool, args ...string) (out, errs string, code int) {
	t.Helper()
	server := h.serve(t)
	t.Chdir(appDir(t))
	if h.pages == nil {
		h.pages = listed(t, page(t), "pkg_7a1c", appTree(t))
	}
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	if h.public != "" {
		writeFile(t, filepath.Join(env["XDG_CONFIG_HOME"], "canyon", "config.json"), `{"host":"`+server.URL+`","public":"`+h.public+`"}`)
	}
	h.tick = at // the fixed instant every age and elapsed in this package is measured from
	var o, e strings.Builder
	a := &app{out: &o, err: &e, in: hesitating{strings.NewReader(input), h}, tty: tty,
		getenv: func(k string) string { return env[k] }, clock: clock{now: func() time.Time { return h.tick }, sleep: func(d time.Duration) { h.tick = h.tick.Add(d) }}}
	code = a.main(append([]string{"deploy", "--env", "prod"}, args...))
	return strings.NewReplacer(server.URL, "http://localhost:7777", strings.TrimPrefix(server.URL, "http://"), "localhost:7777").Replace(o.String()), e.String(), code
}

// pkgDir holds the testdata: the verb runs in an app directory, where a relative path finds none.
var pkgDir, _ = os.Getwd()

func wantsPromote(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join(pkgDir, "testdata", name)) //nolint:gosec // a fixture this test named
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("canyon deploy --env prod rendered\n%s\ntestdata/%s wants\n%s", got, name, want)
	}
}

// appDir is the directory the verb runs in: a canyon.toml and nothing else.
func appDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	return dir
}

// appTree is appDir's tree: the tree preview runs when the directory is what preview runs.
func appTree(t *testing.T) string {
	t.Helper()
	tree, err := treeHash(appDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// listed is the fixture's packages list keyed by the cursor that asks for each page, "" the first,
// with the tree_hash of pkg set to tree.
func listed(t *testing.T, whole map[string]json.RawMessage, pkg, tree string) map[string]json.RawMessage {
	t.Helper()
	var pages []map[string]any
	if json.Unmarshal(whole["packages"], &pages) != nil {
		t.Fatalf("the fixture's packages list is not a list of pages")
	}
	out, cursor := map[string]json.RawMessage{}, ""
	for _, p := range pages {
		for _, row := range p["packages"].([]any) {
			if row := row.(map[string]any); row["id"] == pkg {
				row["tree_hash"] = tree
			}
		}
		out[cursor], _ = json.Marshal(p) // strings, numbers and a decoded map: nothing in here can fail to marshal
		cursor, _ = p["next"].(string)
	}
	return out
}

func decode(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	var read map[string]any
	if json.Unmarshal(body, &read) != nil {
		t.Fatalf("this fixture is not an object: %s", body)
	}
	return read
}

// mutate re-serves a fixture with one field changed: a state routes.md describes that this one
// cannot hold alongside the rest of itself.
func mutate(t *testing.T, body json.RawMessage, change func(map[string]any)) json.RawMessage {
	t.Helper()
	read := decode(t, body)
	change(read)
	out, _ := json.Marshal(read) // strings and a decoded map: nothing in here can fail to marshal
	return out
}

// blocked is the fixture's status read with preview's own run blocked: the refusal's clean-gate row in
// place of preview's passing one, and promote_run the run that row names.
func blocked(t *testing.T, whole map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	refusal := decode(t, whole["refusal"])["receipt"].(map[string]any)
	return mutate(t, whole["status"], func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["blocked_by"], r["rules"].([]any)[0] = refusal["blocked_by"], refusal["rules"].([]any)[0] // prod.clean-gate leads, in the snapshot's order
		read["promote_run"] = "gr_8f2a"
	})
}

// clean is a host whose gate passes: the status with nothing left in blocked_by, which is what the
// server answers on a clean gate. Only the verdict moves; the rows and their words stay the server's.
func clean(t *testing.T, whole map[string]json.RawMessage, code int, answer json.RawMessage) *promoteHost {
	t.Helper()
	return &promoteHost{code: code, answer: answer, timeline: live, status: mutate(t, whole["status"], func(read map[string]any) {
		read["receipt"].(map[string]any)["blocked_by"] = []string{}
	})}
}

// gateRun is the body the promotions route reads, compared whole: it takes one field, and anything
// else in there is something this build invented.
const gateRun = `{"gate_run":"gr_6c12"}`

// live is the promote run's timeline to its end, as the server writes it for a promotion that went live.
var live = json.RawMessage(`{"rows": [` + logged(1, "postgres db: creating") + `,` + logged(2, "postgres db: ready") + `,` + logged(3, "deploying to production") + `,
	{"id": 4, "ts": "2026-09-24T10:00:04Z", "kind": "deploy.completed", "data": {"env": "prod", "url": "http://claims-portal.prod.localhost:7777", "package": "pkg_7d3e8f", "image_digest": "sha256:0"}},
	` + logged(5, "live at http://claims-portal.prod.localhost:7777") + `,
	{"id": 6, "ts": "2026-09-24T10:00:05Z", "kind": "run.state", "data": {"state": "clean"}}], "next": null}`)

func logged(id int, line string) string {
	return fmt.Sprintf(`{"id": %d, "ts": "2026-09-24T10:00:0%dZ", "kind": "build.log", "data": {"line": %q}}`, id, id, line)
}

// A promotion that fails prints each line to the reason, which is the last, and exits 1: production
// is as it was.
func TestAFailedPromotionEndsOnItsReason(t *testing.T) {
	h := clean(t, page(t), http.StatusAccepted, started)
	h.timeline = json.RawMessage(`{"rows": [` + logged(1, "postgres db: creating") + `,` + logged(2, "postgres db: failed: quota exceeded") + `,
		{"id": 3, "ts": "2026-09-24T10:00:03Z", "kind": "run.state", "data": {"state": "failed", "failure_reason": "error"}}], "next": null}`)
	out, errs, code := promoting(t, h, "y\n", true)
	if code != exitError || !strings.Contains(out, "│ postgres db: failed: quota exceeded\n") || !strings.Contains(out, "Nothing was promoted.") || !strings.Contains(errs, "postgres db: failed: quota exceeded") || strings.Contains(out, "Live") {
		t.Errorf("a failed promotion exited %d:\n%s\nstderr %q", code, out, errs)
	}
}

// A promotion whose progress this terminal cannot read keeps going on the host: the line says so and
// names the app's page, never the run's id (canyonv2-5r11.18).
func TestAPromotionThatOutlivesTheTerminalNamesNoRunID(t *testing.T) {
	h := clean(t, page(t), http.StatusAccepted, started)
	h.timeline = json.RawMessage(`not json`)
	_, errs, code := promoting(t, h, "y\n", true)
	if code == exitOK || !strings.Contains(errs, "\n  The promotion keeps going without this terminal:\n") || strings.Contains(errs, "gr_") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
}

// started is the promotions route's 202: the promote run, whose progress the app's page follows.
var started = json.RawMessage(`{"run":{"id":"gr_9c41","state":"pending"}}`)

func TestPromoteRendersTheReceiptAndSendsNothingWhileItIsBlocked(t *testing.T) {
	whole := page(t)
	h := &promoteHost{status: blocked(t, whole)}
	out, errs, code := promoting(t, h, "y\n", true) // a yes is typed, and never reached
	if code != exitBlocked || len(h.posts) != 0 || errs != "" {
		t.Errorf("a blocked promote exited %d, sent %q, and asked %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-blocked.txt", out)
	// Nothing blocks, but no run has looked at the app: the receipt says so better than the 404.
	none := clean(t, whole, http.StatusAccepted, started)
	none.status = mutate(t, none.status, func(read map[string]any) { read["promote_run"] = nil })
	if _, _, code := promoting(t, none, "y\n", true); code != exitBlocked || len(none.posts) != 0 {
		t.Errorf("a project with no run exited %d and sent %q", code, none.posts)
	}
}

func TestARefusalRendersItsOwnReceiptAndNotItsMessage(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusConflict, whole["refusal"])
	out, errs, code := promoting(t, h, "y\n", true)
	if code != exitBlocked || strings.Contains(errs, "the gate run is not clean") {
		t.Errorf("a refused promote exited %d with the one-line code on stderr: %q", code, errs)
	}
	if len(h.posts) != 1 || h.posts[0] != gateRun {
		t.Errorf("the promotion sent %q; the status read answered gr_6c12", h.posts)
	}
	wantsPromote(t, "deploy-prod-409.txt", out) // the refusal's own whys, not the status read's
	// A 409 that carries no receipt this build can read is an ordinary failure, not a verdict.
	broken := clean(t, whole, http.StatusConflict,
		json.RawMessage(`{"error":"gate_not_clean","message":"the gate run is not clean","receipt":"soon"}`))
	if _, errs, code := promoting(t, broken, "y\n", true); code != exitError || !strings.Contains(errs, "the gate run is not clean") {
		t.Errorf("an unreadable receipt exited %d, stderr %q", code, errs)
	}
}

func TestAPromotionShipsTheRunItNamedAndNamesTheRunItStarted(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	out, errs, code := promoting(t, h, "y\n", true)
	if code != exitOK {
		t.Errorf("a clean promote exited %d, stderr %q", code, errs)
	}
	if len(h.posts) != 1 || h.posts[0] != gateRun {
		t.Errorf("the promotion sent %q; the status read answered gr_6c12", h.posts)
	}
	wantsPromote(t, "deploy-prod-done.txt", out)
	// Every shipment after the first replaces a version, and §6.3 says which. Prod is the fixture's
	// own promotion object (routes.md), under the label the replaced version would have; one waiting
	// on its first release replaces nothing.
	promoted := decode(t, whole["promotion"])["promotion"].(map[string]any)
	promoted["label"] = "v1.2"
	for _, c := range []struct {
		prod any
		want string
	}{
		{promoted, "(passed review) from preview to production. It replaces v1.2.\n"},
		{map[string]any{"run_by": "org", "pending": map[string]any{"event": "deploy", "sent_at": "2026-09-02T10:00:00Z", "state": "accepted"}}, "(passed review) from preview to production. Nothing runs in production yet.\n"},
	} {
		again := clean(t, whole, http.StatusAccepted, started)
		again.status = mutate(t, again.status, func(read map[string]any) { read["envs"].(map[string]any)["prod"] = c.prod })
		if out, _, code := promoting(t, again, "y\n", true); code != exitOK || !strings.Contains(out, c.want) {
			t.Errorf("exit %d, and no %q in\n%s", code, c.want, out)
		}
	}
}

// An admin may make prod.clean-gate advisory: a run it failed then blocks nothing, and the line
// that ships it says what preview runs and never that a review cleared it.
func TestAnAdvisoryCleanGateShipsWithoutSayingCleared(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	h.status = mutate(t, blocked(t, whole), func(read map[string]any) {
		read["receipt"].(map[string]any)["blocked_by"] = []string{}
	})
	out, errs, code := promoting(t, h, "y\n", true)
	if code != exitOK || strings.Contains(out, "passed review") || !strings.Contains(out, "from preview to production. ") {
		t.Errorf("exit %d (stderr %q); want a shipping line that claims no clean review in\n%s", code, errs, out)
	}
}

// A directory that is not the tree preview runs ships nothing: prod would get preview, not this. A
// preview package whose row is on the list's second page is found by the host's own cursor.
func TestADirectoryAheadOfPreviewShipsNothing(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	h.pages = listed(t, whole, "pkg_7a1c", "8e2d4c6b0a1f3e5d7c9b2a4f6e8d0c1b3a5f7e9d") // the fixture's own tree, which this directory is not
	out, errs, code := promoting(t, h, "y\n", true)
	if code != exitError || len(h.posts) != 0 || errs != "" {
		t.Errorf("a directory ahead of preview exited %d, sent %q, and asked %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-changed.txt", out)
	later := clean(t, whole, http.StatusAccepted, started)
	later.status = mutate(t, later.status, func(read map[string]any) {
		read["envs"].(map[string]any)["preview"].(map[string]any)["package"] = "pkg_5c02"
	})
	later.pages = listed(t, whole, "pkg_5c02", appTree(t))
	if _, errs, code := promoting(t, later, "y\n", true); code != exitOK || len(later.posts) != 1 {
		t.Errorf("a preview package on the second page: exit %d, sent %q, stderr %q", code, later.posts, errs)
	}
	// A package the list never names is no proof of anything: nothing is sent.
	gone := clean(t, whole, http.StatusAccepted, started)
	gone.status = mutate(t, gone.status, func(read map[string]any) {
		read["envs"].(map[string]any)["preview"].(map[string]any)["package"] = "pkg_0000"
	})
	if _, errs, code := promoting(t, gone, "y\n", true); code != exitError || len(gone.posts) != 0 || !strings.Contains(errs, "the host lists no package pkg_0000") {
		t.Errorf("a preview package the list never names: exit %d, sent %q, stderr %q", code, gone.posts, errs)
	}
}

// With nothing running in preview there is no tree to compare, and the receipt says so.
func TestNothingInPreviewIsTheReceiptsToSay(t *testing.T) {
	h := &promoteHost{status: mutate(t, blocked(t, page(t)), func(read map[string]any) { delete(read["envs"].(map[string]any), "preview") })}
	h.pages = map[string]json.RawMessage{} // a list read here answers nothing a check could pass on
	if _, _, code := promoting(t, h, "y\n", true); code != exitBlocked || len(h.posts) != 0 {
		t.Errorf("nothing in preview exited %d and sent %q", code, h.posts)
	}
}

// An agent still carrying a skill from before §6.3's change is told the new form, as a usage error.
func TestPromoteIsNowDeployEnvProd(t *testing.T) {
	out, errs, code := run(t, nil, false, "promote", "--yes")
	if code != exitError || out != "" || errs != "\n  ✗ canyon promote is now: canyon deploy --env prod\n" {
		t.Errorf("canyon promote exited %d, printed %q, stderr %q", code, out, errs)
	}
	if help, _, _ := run(t, nil, false, "--help"); strings.Contains(help, "\n  promote ") { // deploy's line says "--env prod to promote"; no verb is called that
		t.Errorf("the help still lists promote:\n%s", help)
	}
}

// Every line that says what to run next names prod: the bare verb would put this directory in preview.
func TestAProdDeploysRetryLineNamesProd(t *testing.T) {
	t.Chdir(appDir(t))
	env := map[string]string{"CANYON_HOST": "http://127.0.0.1:1", "XDG_CONFIG_HOME": t.TempDir()}
	if _, errs, code := run(t, env, false, "deploy", "--env", "prod"); code != exitError || !strings.Contains(errs, "Running somewhere else?        canyon deploy --env prod --host ") {
		t.Errorf("an unreachable host: exit %d, stderr %q", code, errs)
	}
}

// Prod sends no tree and preview asks no question, so a flag that belongs to the other one is a typo.
func TestDeployEnvTakesPreviewOrProd(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--env", "staging"}, `deploy takes --env preview or --env prod, and got "staging"`},
		{[]string{"--yes"}, "--yes answers the question deploy --env prod asks, and a preview deploy asks none"},
		{[]string{"--wait"}, "--wait waits for a person to approve deploy --env prod, and a preview deploy waits on nobody"},
		{[]string{"--env", "prod", "--force"}, "so --app, --create, --force and --sarif change nothing"},
		{[]string{"--env", "prod", "--app", "claims-portal"}, "so --app, --create, --force and --sarif change nothing"},
		{[]string{"--env", "prod", "--create"}, "so --app, --create, --force and --sarif change nothing"},
		// the refusal points the way: an agent reached for json while recovering from a refused rollback (#2787)
		{[]string{"--format", "json", "--env", "prod"}, "deploy --env prod has no json rendering: use --format ai, which prints its result for an agent"},
		{[]string{"--format", "json", "--env", "prod", "--version", "v1.3"}, "deploy --env prod has no json rendering: use --format ai, which prints its result for an agent"},
	} {
		if _, errs, code := run(t, nil, false, append([]string{"deploy"}, c.args...)...); code != exitError || !strings.Contains(errs, c.want) {
			t.Errorf("deploy %q: exit %d, stderr %q; want %q", c.args, code, errs, c.want)
		}
	}
}

// The question, and who is allowed to answer it. No golden: what matters is the bytes on stderr,
// that a no sends nothing, and that D5's line stood above the question when it was asked.
func TestTheQuestionDefaultsToNoAndOnlyATerminalMayAnswerIt(t *testing.T) {
	whole := page(t)
	enter := clean(t, whole, http.StatusAccepted, started)
	out, errs, code := promoting(t, enter, "\n", true)
	if code != exitError || len(enter.posts) != 0 {
		t.Errorf("Enter exited %d and sent %q", code, enter.posts)
	}
	if errs != "\n  Promote claims-portal v1.3 to production? [y/N] " { // D5's question names what ships
		t.Errorf("the question this build asks is %q", errs)
	}
	if !strings.Contains(out, noRebuild) || !strings.HasSuffix(out, "\n  Nothing was promoted.\n") {
		t.Errorf("a decline printed\n%s", out)
	}
	piped := clean(t, whole, http.StatusAccepted, started) // a pipe holding a yes nobody typed
	if _, errs, code := promoting(t, piped, "y\n", false); code != exitError || len(piped.posts) != 0 ||
		!strings.Contains(errs, "--yes") || strings.Contains(errs, "[y/N]") {
		t.Errorf("a machine promoted, or was refused without the flag named: exit %d, stderr %q, sent %q", code, errs, piped.posts)
	}
	flagged := clean(t, whole, http.StatusAccepted, started)
	if _, errs, code := promoting(t, flagged, "", false, "--yes"); code != exitOK || errs != "" || len(flagged.posts) != 1 {
		t.Errorf("--yes on the same machine: exit %d, stderr %q, sent %q", code, errs, flagged.posts)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// A promotion says who can open what just went live, as a preview deploy does (#1679): private, or how
// far it is shared, then the reader's place; where nobody signs in, that anyone who reaches Canyon can.
func TestAPromotionSaysWhoCanOpenProduction(t *testing.T) {
	for _, c := range []struct{ project, auth, want string }{
		{`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0}}}`, "", "Access private · you are an owner"},
		{`{"project":{"id":"claims-portal","access":{"level":"contributor","everyone":false,"people":2,"groups":1}}}`, "", "Access shared with 2 people and 1 group · you are a contributor"},
		{`{"project":{"id":"claims-portal","access":{"level":"owner","everyone":true,"people":0,"groups":0}}}`, "off", "Access anyone who can reach this Canyon (sign-in is off)"},
	} {
		h := clean(t, page(t), http.StatusAccepted, started)
		h.project, h.auth = c.project, c.auth
		out, errs, code := promoting(t, h, "", false, "--yes")
		if code != exitOK || errs != "" || !strings.Contains(out, "  "+c.want+"\n") {
			t.Errorf("exit %d, stderr %q: want %q in\n%s", code, errs, c.want, out)
		}
	}
}

// A promotion links the app's page at the host's public URL, as login read it, not the address the
// CLI dials: the demo's sample apps promote through http://server:7777 (#2019).
func TestAPromotionLinksTheAppsPageAtThePublicURL(t *testing.T) {
	h := clean(t, page(t), http.StatusAccepted, started)
	h.public = "https://canyon.meridian.example"
	out, errs, code := promoting(t, h, "", false, "--yes")
	if code != exitOK || errs != "" || !strings.Contains(out, "  Fleet  https://canyon.meridian.example/#/apps/claims-portal\n") {
		t.Errorf("exit %d, stderr %q: want the page at the public URL in\n%s", code, errs, out)
	}
}

// A runtime value set for preview and not for production is named before the question, with the line
// that sets it (#1698): production runs preview's image with its own secrets. A resource's value is
// made for production by the promotion, a build value is in the image, and one set for both says nothing.
func TestAPromotionNamesWhatProductionLacks(t *testing.T) {
	row := func(name, env, where, origin string) string {
		return `{"name":"` + name + `","env":"` + env + `","where":"` + where + `","version":1,"set_by":{"kind":"person","subject":"dana","display":"Dana"},"set_at":"2026-10-01T00:00:00Z","origin":"` + origin + `"}`
	}
	h := clean(t, page(t), http.StatusAccepted, started)
	h.secrets = `{"secrets":[` + strings.Join([]string{row("STRIPE_KEY", "preview", "runtime", "user"), row("DATABASE_URL", "preview", "runtime", "provisioned"),
		row("SENTRY_RELEASE", "preview", "build", "user"), row("BOTH", "preview", "runtime", "user"), row("BOTH", "prod", "runtime", "user")}, ",") + `]}`
	out, errs, code := promoting(t, h, "", false, "--yes")
	want := "  ! STRIPE_KEY is set for preview but not production. Set it with: canyon secret set STRIPE_KEY --env prod\n"
	if code != exitOK || errs != "" || !strings.Contains(out, want) || strings.Count(out, "is set for preview but not production") != 1 {
		t.Errorf("exit %d, stderr %q: want only STRIPE_KEY named in\n%s", code, errs, out)
	}
	if strings.Index(out, want) > strings.Index(out, "Promote\n") {
		t.Errorf("the missing value is named after the promotion started:\n%s", out)
	}
}

// A promotion that goes ahead past an advisory rule says it under ai, as canyon status does (cli.md §6.3;
// #2300: platform.known-vuln's line was status's alone).
func TestAPromotionUnderAISaysItsAdvisoryLines(t *testing.T) {
	h := clean(t, page(t), http.StatusAccepted, started)
	h.status = mutate(t, h.status, func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["rules"] = append(r["rules"].([]any), map[string]any{"id": "platform.known-vuln", "name": "No known-vulnerable system libraries or build tools", "blocks": false, "passed": false,
			"why": "6 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds", "unset": false})
	})
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	line := "\nadvisory: No known-vulnerable system libraries or build tools (platform.known-vuln): 6 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds\n"
	step := ". Tell the user each advisory line in its own words: it did not stop production.\n\nexit code: 0\n"
	if code != exitOK || errs != "" || !strings.Contains(out, line) || !strings.Contains(out, step) {
		t.Errorf("exit %d, stderr %q: want %q and the step in\n%s", code, errs, line, out)
	}
}

// Under --format ai a promotion ends in one block (cli.md §6.3): its result, the run it started, what
// shipped and where, and the steps, the exit code printed as text. Nothing of the transcript is above it.
func TestAPromotionUnderAIIsOneBlock(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	h.project = `{"project":{"id":"claims-portal","access":{"level":"owner","everyone":false,"people":0,"groups":0}}}`
	h.secrets = `{"secrets":[{"name":"STRIPE_KEY","env":"preview","where":"runtime","version":1,"set_by":{"kind":"person","subject":"dana","display":"Dana"},"set_at":"2026-10-01T00:00:00Z","origin":"user"}]}`
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if code != exitOK || errs != "" {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	wantsPromote(t, "deploy-prod-ai-done.txt", out)

	failed := clean(t, whole, http.StatusAccepted, started)
	failed.timeline = json.RawMessage(`{"rows": [` + logged(1, "postgres db: creating") + `,` + logged(2, "postgres db: failed: quota exceeded") + `,
		{"id": 3, "ts": "2026-09-24T10:00:03Z", "kind": "run.state", "data": {"state": "failed", "failure_reason": "error"}}], "next": null}`)
	out, errs, code = promoting(t, failed, "", false, "--yes", "--format", "ai")
	if code != exitError || errs != "" {
		t.Errorf("a failed promotion: exit %d, stderr %q", code, errs)
	}
	wantsPromote(t, "deploy-prod-ai-failed.txt", out)

	refused := &promoteHost{status: blocked(t, whole)}
	out, errs, code = promoting(t, refused, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || errs != "" || len(refused.posts) != 0 {
		t.Errorf("a blocked promotion: exit %d, stderr %q, sent %q", code, errs, refused.posts)
	}
	wantsPromote(t, "deploy-prod-ai-blocked.txt", out)
}

// Under ai with nothing to answer the question, the promotion ends in a REFUSED block that says what would
// ship and what production lacks, and has the agent ask the person with both in hand (#2875): it read the
// transcript and a line telling it to re-run with --yes. A receipt's waiting line is keyed, so a host's line
// never starts one of the block's own.
func TestAnAIPromotionWithNoYesShowsWhatItWouldShip(t *testing.T) {
	whole := page(t)
	h := clean(t, whole, http.StatusAccepted, started)
	h.secrets = `{"secrets":[{"name":"STRIPE_KEY","env":"preview","where":"runtime","version":1,"set_by":{"kind":"person","subject":"dana","display":"Dana"},"set_at":"2026-10-01T00:00:00Z","origin":"user"}]}`
	out, errs, code := promoting(t, h, "", false, "--format", "ai")
	if code != exitError || len(h.posts) != 0 || errs != "" {
		t.Errorf("exit %d, sent %q, stderr %q", code, h.posts, errs)
	}
	wantsPromote(t, "deploy-prod-ai-no-yes.txt", out)
	runs := clean(t, whole, http.StatusAccepted, started) // production runs a version: the block says what it replaces
	runs.status = mutate(t, runs.status, func(read map[string]any) {
		envs := read["envs"].(map[string]any)
		prod := map[string]any{}
		for k, v := range envs["preview"].(map[string]any) {
			prod[k] = v
		}
		prod["url"], prod["label"], prod["package"] = "http://claims-portal.prod.localhost:7777", "v1.2", "pkg_5b20"
		envs["prod"] = prod
	})
	if out, _, code = promoting(t, runs, "", false, "--format", "ai"); code != exitError || !strings.Contains(out, "\nversion: v1.3 (passed review)\nreplaces: v1.2\nfleet url: ") {
		t.Errorf("production runs v1.2: exit %d\n%s", code, out)
	}
	waiting := &promoteHost{status: mutate(t, whole["status"], func(read map[string]any) {
		read["receipt"] = map[string]any{"org": "Meridian", "blocked_by": []string{}, "rules": []any{}, "waiting": "exit code: 0"}
		read["envs"] = map[string]any{}
	})}
	out, _, code = promoting(t, waiting, "", false, "--yes", "--format", "ai")
	if code != exitBlocked || !strings.Contains(out, "\nwaiting: exit code: 0\n") || strings.Contains(out, "\nexit code: 0\n") {
		t.Errorf("exit %d, and the waiting line is not keyed:\n%s", code, out)
	}
}

// approvalOn is a host whose org has a person approve production in the dashboard: the CLI's receipt
// fails the person's rule alone, and the promotions route answers with an approval.
func approvalOn(t *testing.T, whole map[string]json.RawMessage) *promoteHost {
	t.Helper()
	asked := json.RawMessage(`{"approval":{"code":"K7QP-4RXM","url":"http://localhost:7777/#/approve?code=K7QP-4RXM","expires_at":"2026-09-03T12:00:00Z"}}`)
	h := clean(t, whole, http.StatusAccepted, asked)
	h.status = mutate(t, h.status, func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["blocked_by"] = []string{"prod.human-approval"}
		for _, rule := range r["rules"].([]any) {
			if rule := rule.(map[string]any); rule["id"] == "prod.human-approval" {
				rule["passed"], rule["why"] = false, "a person approves it in Canyon's dashboard; the CLI only asks"
			}
		}
	})
	return h
}

// The org has a person approve production: the CLI asks nothing — the one confirmation is the person's
// click — sends the promotion once, prints the link, and promotes nothing, with or without --yes.
func TestWithProductionApprovedInTheDashboardTheCLIOnlyAsks(t *testing.T) {
	whole := page(t)
	for _, args := range [][]string{nil, {"--yes"}} {
		h := approvalOn(t, whole)
		out, errs, code := promoting(t, h, "", false, args...)
		if code != exitBlocked || len(h.posts) != 1 || h.posts[0] != gateRun || errs != "" {
			t.Errorf("%v: exit %d, sent %q, stderr %q", args, code, h.posts, errs)
		}
		wantsPromote(t, "deploy-prod-approval.txt", out)
	}
	h := approvalOn(t, whole)
	out, _, code := promoting(t, h, "", false, "--format", "ai")
	if code != exitBlocked {
		t.Errorf("ai: exit %d", code)
	}
	wantsPromote(t, "deploy-prod-ai-approval.txt", out)
}

// The org stopped having a person approve production between the status read and the promotion: the
// host promoted, and the terminal that asked no question says so rather than reading as a request.
func TestASwitchTurnedOffMidCommandSaysProductionWasPromoted(t *testing.T) {
	h := approvalOn(t, page(t))
	h.answer = started
	out, _, code := promoting(t, h, "", false)
	if code != exitOK || !strings.Contains(out, "\n  This org stopped having a person approve production while this ran, so production was promoted directly.\n") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// --wait waits for the person's answer: approved, it follows the promotion the approval started; any
// other end promotes nothing and says why.
func TestDeployProdWaitsForThePersonsAnswer(t *testing.T) {
	whole := page(t)
	const pending = `{"state":"pending"}`
	h := approvalOn(t, whole)
	h.reads = []string{pending, pending, `{"state":"approved","run":"gr_9c41","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`}
	out, errs, code := promoting(t, h, "", false, "--wait")
	if code != exitOK || !strings.Contains(out, "\n  ✓ Approved by Dana Whitfield\n") || !strings.Contains(out, "  Live   http://claims-portal.prod.localhost:7777\n") {
		t.Errorf("approved: exit %d, stderr %q\n%s", code, errs, out)
	}
	for _, c := range []struct{ read, want string }{
		{`{"state":"denied","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`, "✗ Denied by Dana Whitfield. Nothing was promoted."},
		{`{"state":"expired"}`, "The approval expired. Nothing was promoted. Run: canyon deploy --env prod"},
		{`{"state":"outdated"}`, "Preview runs another version now. Nothing was promoted. Run: canyon deploy --env prod"},
		{"", "This approval is gone: a newer request replaced it, or the app was deleted. Nothing was promoted."},
	} {
		h := approvalOn(t, whole)
		h.reads = []string{pending, c.read}
		out, _, code := promoting(t, h, "", false, "--wait")
		if code != exitBlocked || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want %q in\n%s", c.read, code, c.want, out)
		}
	}
	h = approvalOn(t, whole)
	h.reads = []string{`{"state":"denied","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`}
	out, _, _ = promoting(t, h, "", false, "--wait", "--format", "ai")
	if !strings.HasPrefix(out, "CANYON PROMOTE — RESULT: BLOCKED\n") || !strings.Contains(out, "outcome: Dana Whitfield denied the approval. Nothing was promoted.\n") {
		t.Errorf("ai, denied:\n%s", out)
	}
}

// Preview's answer, when it is one to say beside readiness, is said before anything is asked: where the
// org promotes directly it refuses the promotion (cli.md §6.3), and where a person approves production it
// is the line under the version and the first step. Production's own answer comes first in the
// promotion's steps (#1833: django-400b was promoted answering 400).
func TestAPromotionPutsANonOKAnswerFirst(t *testing.T) {
	h := clean(t, page(t), http.StatusAccepted, started)
	h.timeline = json.RawMessage(strings.Replace(string(live), `"image_digest": "sha256:0"`, `"image_digest": "sha256:0", "answered": {"status": 400, "said": ""}`, 1))
	out, _, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	if want := "\nNEXT STEPS:\n1. Tell the user first: production answered 400 Bad Request at /, so the app may not work there. Check it.\n2. Report the live url"; code != exitOK || !strings.Contains(out, want) {
		t.Errorf("exit %d, want %q in\n%s", code, want, out)
	}
	asked := func() *promoteHost {
		h := approvalOn(t, page(t))
		h.status = mutate(t, h.status, func(read map[string]any) {
			read["envs"].(map[string]any)["preview"].(map[string]any)["answered"] = map[string]any{"status": 400, "said": ""}
		})
		return h
	}
	out, _, code = promoting(t, asked(), "", false)
	if want := "  ! Preview answered 400 Bad Request at /; check the app before promoting.\n"; code != exitBlocked || !strings.Contains(out, want) {
		t.Errorf("exit %d, want %q in\n%s", code, want, out)
	}
	out, _, code = promoting(t, asked(), "", false, "--format", "ai")
	if want := "\nNEXT STEPS:\n1. Tell the user first: preview answered 400 Bad Request at /; the person who approves should check the app first.\n2. Stop."; code != exitBlocked || !strings.Contains(out, want) {
		t.Errorf("exit %d, want %q in\n%s", code, want, out)
	}
	approved := asked() // production's row carries no answer: preview's is all there is
	approved.reads = []string{`{"state":"approved","run":"gr_9c41","decided_by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"}}`}
	out, _, code = promoting(t, approved, "", false, "--wait", "--format", "ai")
	if want := "\nNEXT STEPS:\n1. Tell the user first: preview answered 400 Bad Request at / before this promotion. Check the app.\n2. Report the live url"; code != exitOK || !strings.Contains(out, want) {
		t.Errorf("exit %d, want %q in\n%s", code, want, out)
	}
}
