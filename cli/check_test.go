package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type reply struct {
	code int
	body string
}

// gateHost answers everything check asks; a field is the answer where a test wants something
// other than the happy path, and the rest records what the CLI sent.
type gateHost struct {
	project, start reply
	upload         reply    // the answer to the package upload; a 201 naming pkg_1 when unset
	built          string   // a base the upload takes, whatever upload answers: the head a copy holds the push of
	page           string   // the timeline, whose one page ends the run
	run            string   // the body of GET /gate-runs/:id
	read           reply    // the answer to GET /projects/claims-portal; the app's row when unset
	pkg            reply    // the answer to GET /projects/claims-portal/packages/:pkg, the base check; that package's row when unset
	later          reply    // the answer to that read once the run started, when the case serves another
	status         reply    // the answer to GET /projects/claims-portal/status, when the case serves one
	manifest       reply    // the answer to GET /skill/manifest, when the case serves one
	auth           reply    // the answer to GET /auth/config, when the case serves one
	components     reply    // the answer to GET /component-types, when the case serves one
	manifestQuery  string   // the query string the manifest was asked with
	sarif          string   // the body of GET /gate-runs/:id/sarif
	reports        []reply  // the answers to the report PUTs, in order; a zero one is cut off mid-answer
	attached       []string // the bodies of the report PUTs, in order
	started        string   // the body of the run start
	harness        string   // the run start's Canyon-Harness header
	transport      string   // the run start's Canyon-Transport header
	asked          []string
	created        []string // the bodies of POST /projects, in order
	base, query    string   // the Canyon-Base-Package header and the query string of the upload
	length, packed int64
	gzipped        bool
}

func (h *gateHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.asked = append(h.asked, r.Method+" "+r.URL.Path)
	switch path := r.URL.Path; {
	case path == "/api/v1/projects":
		body, _ := io.ReadAll(r.Body)
		h.created = append(h.created, string(body))
		h.answer(w, h.project)
	case strings.HasSuffix(path, "/packages"):
		body, _ := io.ReadAll(r.Body)
		h.base, h.query, h.length, h.packed = r.Header.Get("Canyon-Base-Package"), r.URL.RawQuery, r.ContentLength, int64(len(body))
		h.gzipped = len(body) > 1 && body[0] == 0x1f && body[1] == 0x8b
		if h.upload.code == 0 || h.built != "" && h.base == h.built {
			h.answer(w, reply{http.StatusCreated, `{"package":{"id":"pkg_1"}}`})
			return
		}
		h.answer(w, h.upload)
	case strings.HasSuffix(path, "/reports"):
		body, _ := io.ReadAll(r.Body)
		h.attached = append(h.attached, string(body))
		if answer := h.reports[len(h.attached)-1]; answer.code != 0 {
			h.answer(w, answer)
		} else if conn, _, err := w.(http.Hijacker).Hijack(); err == nil { // a zero reply is the network dropping mid-answer
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 64\r\n\r\n{")
			_ = conn.Close()
		}
	case strings.HasPrefix(path, "/api/v1/projects/claims-portal/packages/") && h.pkg.code != 0:
		h.answer(w, h.pkg)
	case strings.HasPrefix(path, "/api/v1/projects/claims-portal/packages/"):
		h.answer(w, reply{http.StatusOK, `{"package":{"id":"` + strings.TrimPrefix(path, "/api/v1/projects/claims-portal/packages/") + `"}}`})
	case strings.HasSuffix(path, "/gate-runs"), strings.HasSuffix(path, "/deploys"):
		body, _ := io.ReadAll(r.Body)
		h.started, h.harness, h.transport = string(body), r.Header.Get("Canyon-Harness"), r.Header.Get("Canyon-Transport")
		h.answer(w, h.start)
	case strings.HasSuffix(path, "/timeline"):
		h.answer(w, reply{http.StatusOK, h.page})
	case strings.HasSuffix(path, "/sarif"):
		h.answer(w, reply{http.StatusOK, h.sarif})
	case h.status.code != 0 && path == "/api/v1/projects/claims-portal/status":
		h.answer(w, h.status)
	case h.manifest.code != 0 && path == "/api/v1/skill/manifest":
		h.manifestQuery = r.URL.RawQuery
		h.answer(w, h.manifest)
	case h.later.code != 0 && h.started != "" && path == "/api/v1/projects/claims-portal":
		h.answer(w, h.later)
	case path == "/api/v1/projects/claims-portal" && h.read.code != 0:
		h.answer(w, h.read)
	case strings.Count(path, "/") == 4 && strings.HasPrefix(path, "/api/v1/projects/"): // the app's row, whichever app was asked for
		h.answer(w, reply{http.StatusOK, `{"project":{"id":"` + strings.TrimPrefix(path, "/api/v1/projects/") + `"}}`})
	case h.components.code != 0 && path == "/api/v1/component-types":
		h.answer(w, h.components)
	case h.auth.code != 0 && path == "/auth/config":
		h.answer(w, h.auth)
	default:
		h.answer(w, reply{http.StatusOK, h.run})
	}
}

func (h *gateHost) answer(w http.ResponseWriter, r reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.code)
	_, _ = io.WriteString(w, r.body)
}

func (h *gateHost) polled() bool {
	return slices.ContainsFunc(h.asked, func(path string) bool { return strings.Contains(path, "/timeline") })
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checking runs the verb in a directory holding a canyon.toml, one file and one excluded one.
func checking(t *testing.T, h *gateHost) (out, errOut string, code int) {
	t.Helper()
	return verbing(t, h, "check")
}

func verbing(t *testing.T, h *gateHost, verb string) (out, errOut string, code int) {
	t.Helper()
	return verbingIn(t, h, projectDir(t), verb)
}

// projectDir is a directory holding a canyon.toml, one file and one excluded one.
func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	writeFile(t, filepath.Join(dir, "index.js"), "console.log(1);\n")
	writeFile(t, filepath.Join(dir, "node_modules", "left-pad.js"), "module.exports = 1;\n")
	return dir
}

func verbingIn(t *testing.T, h *gateHost, dir, verb string, args ...string) (out, errOut string, code int) {
	t.Helper()
	return verbingWith(t, h, dir, t.TempDir(), verb, args...)
}

// verbingWith is verbingIn with config as the machine's config directory, kept across runs.
func verbingWith(t *testing.T, h *gateHost, dir, config, verb string, args ...string) (out, errOut string, code int) {
	t.Helper()
	if h.project.code == 0 {
		h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
	}
	if h.start.code == 0 {
		h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	}
	if h.page == "" {
		h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(dir)
	out, errOut, code = run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": config}, false, append([]string{verb}, args...)...)
	named := strings.NewReplacer(server.URL, "http://localhost:7777", strings.TrimPrefix(server.URL, "http://"), "localhost:7777") // the host the transcripts name
	return named.Replace(out), named.Replace(errOut), code
}

// §2's fallback is about information, not decoration: a terminal that cannot draw the glyphs
// loses the separator too, and the trust statement still has to read as one.
func TestThePackageLineKeepsItsTrustStatementInASCII(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0)}
	h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
	h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(projectDir(t))
	out, _, _ := answer(t, "", map[string]string{"CANYON_HOST": server.URL,
		"XDG_CONFIG_HOME": t.TempDir(), "CANYON_ASCII": "1"}, false, "check")
	if want := "+ Package    2 files | 38 B | node_modules excluded -> " + strings.TrimPrefix(server.URL, "http://") + "\n"; !strings.Contains(out, want) {
		t.Errorf("the ASCII Package line lost the clause, its separator or its host; want %q in:\n%s", want, out)
	}
}

// D5's trust statement ends at the host the upload went to, as dialled, never the public URL the host
// reports; a tree that matched nothing still names it (cli.md §3.1).
func TestThePackageRowNamesTheHostItWentTo(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0)}
	h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
	h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
	h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	xdg := t.TempDir()
	must(t, (&config{Host: server.URL, Public: "https://canyon.meridian.example"}).write(filepath.Join(xdg, "canyon", "config.json")))
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	writeFile(t, filepath.Join(dir, "index.js"), "console.log(1);\n")
	t.Chdir(dir)
	out, _, _ := answer(t, "", map[string]string{"XDG_CONFIG_HOME": xdg, "NO_COLOR": "1"}, false, "check")
	if want := "  ✓ Package    2 files · 38 B · → " + strings.TrimPrefix(server.URL, "http://") + "\n"; !strings.Contains(out, want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
}

// A finding the profile would accept: exactly one location with a full region (contracts/finding.md).
const where = `"locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.ts"},` +
	`"region":{"startLine":1,"startColumn":1,"endLine":1,"endColumn":6,"snippet":{"text":"x = 1"}}}}],`

const (
	blocker  = `{"ruleId":"secrets.client-bundle",` + where + `"properties":{"canyon":{"severity":"critical","blocks":true}}}`
	advisory = `{"ruleId":"deps.known-vuln",` + where + `"properties":{"canyon":{"severity":"high","blocks":false}}}`
	silent   = `{"ruleId":"secrets.client-bundle",` + where + `"properties":{"canyon":{"severity":"critical"}}}`
)

func aRun(state string, blocking, warnings int, findings ...string) string {
	return `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"` + state + `","blocking":` + strconv.Itoa(blocking) +
		`,"warnings":` + strconv.Itoa(warnings) + `,"findings":[` + strings.Join(findings, ",") + `]}}`
}

// The exit code is the product, and failed is not blocked: the gate never answered, so nothing
// about the code is known.
func TestTheFourExitsOfCanyonCheck(t *testing.T) {
	for _, c := range []struct {
		name, run, want string
		code            int
		stderr          bool
	}{
		{name: "clean", code: exitOK, want: "✓ Review passed · 0 blocking, 1 warning",
			run: aRun("clean", 0, 1, advisory)},
		{name: "clean, something not checked", code: exitOK, want: "✓ Review found nothing blocking in what was checked · 0 blocking, 1 warning",
			run: strings.Replace(aRun("clean", 0, 1, advisory), `"state":"clean"`, `"state":"clean","not_checked":true`, 1)},
		{name: "blocked", code: exitBlocked, want: "✗ Review blocked · 1 blocking finding",
			run: aRun("blocked", 1, 1, blocker, advisory)},
		{name: "failed", code: exitError, want: "model", stderr: true,
			run: `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"model","blocking":0,"warnings":0,"findings":[]}}`},
		{name: "a finding that does not say whether it blocks", code: exitError, want: "does not say whether it blocks", stderr: true,
			run: aRun("blocked", 1, 0, silent)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &gateHost{run: c.run}
			out, errOut, code := checking(t, h)
			printed := out
			if c.stderr {
				printed = errOut
			}
			if code != c.code || !strings.Contains(printed, c.want) {
				t.Errorf("exit %d, want %d and %q; stdout:\n%s\nstderr:\n%s", code, c.code, c.want, out, errOut)
			}
			if c.name != "clean" {
				return
			}
			// The Package line names what it did not upload, and the count below it keeps
			// counting: design/specs/cli.md §3.1's trust statement, on the tree projectDir built.
			if !strings.Contains(out, "canyon "+Version+" · claims-portal") || !strings.Contains(out, "· 1 path not packed: node_modules/\n") ||
				!strings.Contains(out, "Package    2 files · 38 B · node_modules excluded") {
				t.Errorf("the header and what was left out:\n%s", out)
			}
			if h.base != "" || h.query != "check=true" || h.gzipped || h.length != h.packed || h.packed == 0 { // a check's upload: review-only, no base, no force
				t.Errorf("the upload carried base %q, query %q, gzip %v, Content-Length %d for %d bytes", h.base, h.query, h.gzipped, h.length, h.packed)
			}
			if !slices.Contains(h.asked, "GET /api/v1/gate-runs/gr_8f2a") { // that run, and no other
				t.Errorf("the run read went somewhere else: %q", h.asked)
			}
			packaged := 0
			for _, asked := range h.asked {
				if asked == "POST /api/v1/projects/claims-portal/packages" {
					packaged++
				}
			}
			if packaged != 1 { // one package per invocation: the prologue runs once, whichever verb calls it
				t.Errorf("the tree was packaged %d times: %q", packaged, h.asked)
			}
		})
	}
	// The count is the run's, and D5's "Nothing was built" is a deploy's: check never deploys, so it never says it did not.
	out, _, code := checking(t, &gateHost{run: aRun("blocked", 2, 0, blocker, blocker)})
	if code != exitBlocked || !strings.Contains(out, "✗ Review blocked · 2 blocking findings") || strings.Contains(out, "Nothing was built") {
		t.Errorf("the blocked verdict exited %d and printed:\n%s", code, out)
	}
}

// The not packed line names the paths, not only the count (#1246): when a build then fails, the
// path is what the builder needs. Walk order, a directory with a trailing slash, a symlink as any
// path; git's record counted and never named, and a line that is git's alone stays a bare count.
func TestThePackageRowNamesThePathsThatStayedHome(t *testing.T) {
	tree := func(t *testing.T, rels ...string) string {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
		writeFile(t, filepath.Join(dir, "index.js"), "console.log(1);\n")
		for _, rel := range rels {
			if link, ok := strings.CutPrefix(rel, "->"); ok {
				if err := os.Symlink("index.js", filepath.Join(dir, link)); err != nil {
					t.Fatal(err)
				}
				continue
			}
			writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "x\n")
		}
		return dir
	}
	for _, c := range []struct {
		name string
		dir  func(t *testing.T) string
		want string
	}{
		{"one directory", projectDir, "· 1 path not packed: node_modules/\n"},
		{"git's record alone", func(t *testing.T) string { return tree(t, ".git/config", ".gitattributes") }, "· 2 paths not packed\n"},
		// A name off someone's disk reaches a terminal as text, never as a control sequence.
		{"a symlink, sanitised", func(t *testing.T) string { return tree(t, "->\x1b[2J", "node_modules/x.js", ".git/config") },
			"· 3 paths not packed: \\x1b[2J and node_modules/\n"},
		// src/build/ is source, so it travels; the root's dist/ does not.
		{"past the cap", func(t *testing.T) string {
			return tree(t, ".env", ".env.local", ".venv/bin/activate", ".git/config", "dist/app.js", "->link.js", "node_modules/x.js", "src/build/info.go")
		}, "· 7 paths not packed: .env, .env.local, .venv/ and 3 more\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, code := verbingIn(t, &gateHost{run: aRun("clean", 0, 0)}, c.dir(t), "check")
			if code != exitOK || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want the line %q in:\n%s\nstderr:\n%s", code, c.want, out, errOut)
			}
		})
	}
}

// A Dockerfile at the package root is said to be unread, once, on deploy and check alike (cli.md §3.1,
// #1243): a dim line under the Package row, the ai block's dockerfile: line, and json's notes[] the
// review's own note, the human line with the ask line after it the first time, and the ai block never with
// one (#2800's review). The review's note in the same words is not said a second time. One below the root,
// which the review names nothing for, gets no line.
func TestARootDockerfileIsSaidToBeUnread(t *testing.T) {
	const said = "Canyon builds with buildpacks and does not read this Dockerfile"
	const ask = "Not supported yet. Ask for it: https://github.com/getcanyon-ai/canyon/issues/new?category=own_dockerfile_ignored&os=linux&template=missing-feature.yml&version=dev" // a Dockerfile Canyon builds from, asked for (#2800)
	note := `{"producer":"canyon/scanners","sarif":{"runs":[{"invocations":[{"toolExecutionNotifications":` +
		`[{"level":"note","message":{"text":"` + said + `"}}]}],"results":[]}]}}`
	for _, c := range []struct {
		name, file string
		args       []string
		want       string // the line, or "" for none
	}{
		{"check", "Dockerfile", []string{"check"}, "  · 1 path not packed: node_modules/\n  · " + said + "\n  · " + ask + "\n"},
		{"deploy", "Dockerfile", []string{"deploy"}, "  · 1 path not packed: node_modules/\n  · " + said + "\n  · " + ask + "\n"},
		{"ai", "Dockerfile", []string{"check", "--format", "ai"}, "\nnot packed: node_modules/\ndockerfile: " + said + "\n"},
		{"json", "Dockerfile", []string{"check", "--format", "json"}, `"notes":[{"producer":"canyon/scanners","text":"` + said + `"}]`},
		{"below the root", "docker/Dockerfile", []string{"check"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := projectDir(t)
			writeFile(t, filepath.Join(dir, filepath.FromSlash(c.file)), "FROM nginx\n")
			h := &gateHost{run: aRun("clean", 0, 0)}
			if c.want != "" { // the review names a root Dockerfile only (contracts/finding.md)
				h.page = gatePage(`{"id":45,"ts":"2026-09-02T09:31:05Z","kind":"producer.sarif","data":` + note + `}`)
			}
			out, errOut, _ := verbingIn(t, h, dir, c.args[0], c.args[1:]...)
			if n := strings.Count(out, said); n != min(len(c.want), 1) || strings.Count(out, ask) != strings.Count(c.want, ask) || !strings.Contains(out, c.want) {
				t.Errorf("said %d times, want the line %q once in:\n%s\nstderr:\n%s", n, c.want, out, errOut)
			}
		})
	}
}

// The ask under the Dockerfile note is said the first time an app's package carries one on this machine, and not
// again on the next deploy or check of it: nothing was refused, so saying it every time would only be relayed every
// time (#2800's review). The machine remembers it beside its config.
func TestTheDockerfileAskIsSaidOncePerApp(t *testing.T) {
	const ask = "Not supported yet. Ask for it: https://github.com/getcanyon-ai/canyon/issues/new?category=own_dockerfile_ignored&os=linux&template=missing-feature.yml&version=dev"
	note := `{"producer":"canyon/scanners","sarif":{"runs":[{"invocations":[{"toolExecutionNotifications":` +
		`[{"level":"note","message":{"text":"` + dockerfileNote + `"}}]}],"results":[]}]}}`
	dir, config := projectDir(t), t.TempDir()
	writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM nginx\n")
	for i, verb := range []string{"check", "deploy", "check"} {
		h := &gateHost{run: aRun("clean", 0, 0), page: gatePage(`{"id":45,"ts":"2026-09-02T09:31:05Z","kind":"producer.sarif","data":` + note + `}`)}
		out, errOut, _ := verbingWith(t, h, dir, config, verb)
		if want := map[bool]int{true: 1, false: 0}[i == 0]; strings.Count(out, ask) != want || !strings.Contains(out, dockerfileNote) {
			t.Errorf("run %d (%s): the ask said %d times, want %d, beside the note:\n%s\nstderr:\n%s", i+1, verb, strings.Count(out, ask), want, out, errOut)
		}
	}
}

// The CLI derives no verdict, and refuses one that argues with its own findings: a passing exit
// on a run carrying a blocker is the one failure this product cannot have.
func TestAVerdictThatDisagreesWithItsFindingsIsRefused(t *testing.T) {
	for _, answered := range []string{
		aRun("clean", 1, 0, blocker),             // clean beside a finding that blocks
		aRun("blocked", 0, 1, advisory),          // blocked beside nothing that blocks
		aRun("blocked", 2, 0, blocker),           // a count that is not the findings sent
		aRun("blocked", 1, 5, blocker, advisory), // a warning count that is not either
	} {
		out, errOut, code := checking(t, &gateHost{run: answered})
		if code != exitError || !strings.Contains(errOut, "the host's verdict and its findings disagree (run gr_8f2a)") {
			t.Errorf("%s: exit %d, stdout:\n%s\nstderr:\n%s", answered, code, out, errOut)
		}
	}
}

// Two 409s, two meanings: the project made in between (carry on, it is what --create wanted) and a
// run is already going (stop, one line, naming it). A code without its status is neither.
func TestHowCheckReadsARefusal(t *testing.T) {
	exists := &gateHost{read: lacks, later: reply{http.StatusOK, projectRead}, project: reply{http.StatusConflict, `{"error":"exists","message":"a project with that name exists"}`}, run: aRun("clean", 0, 0), page: buildPage(t, nil, true)}
	out, errOut, code := verbingIn(t, exists, projectDir(t), "deploy", "--create")
	if code != exitOK || errOut != "" || !strings.Contains(out, "✓ Review passed") {
		t.Errorf("an app made in between: exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
	if !slices.Contains(exists.asked, "POST /api/v1/projects/claims-portal/packages") {
		t.Errorf("the upload did not happen after the 409: %q", exists.asked)
	}
	busy := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusConflict,
		`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"}}`}}
	out, errOut, code = checking(t, busy)
	if code != exitError || !strings.Contains(errOut, "already running (gr_9a1c)") || strings.Contains(strings.TrimSpace(errOut), "\n") {
		t.Errorf("a run already going: exit %d, stdout %q, stderr:\n%s", code, out, errOut)
	}
	// invalid_name is a 400 and only a 400: a 409 carrying that code is not the project existing.
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict} {
		bad := &gateHost{read: lacks, project: reply{status, `{"error":"invalid_name","message":"that name will not do"}`}, run: aRun("clean", 0, 0)}
		if _, errOut, code := verbingIn(t, bad, projectDir(t), "deploy", "--create"); code != exitError || slices.Contains(bad.asked, "POST /api/v1/projects/claims-portal/packages") {
			t.Errorf("invalid_name as a %d: exit %d, stderr %q, asked %q", status, code, errOut, bad.asked)
		}
	}
	wrong := &gateHost{read: lacks, project: reply{http.StatusBadRequest, `{"error":"exists","message":"a project with that name exists"}`}, run: aRun("clean", 0, 0)}
	if out, errOut, code := verbingIn(t, wrong, projectDir(t), "deploy", "--create"); code != exitError || wrong.polled() {
		t.Errorf("a 400 exists: exit %d, polled %v, stdout:\n%s\nstderr:\n%s", code, wrong.polled(), out, errOut)
	}
	early := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusBadRequest, `{"error":"run_in_progress","message":"a run of this project is running"}`}}
	if out, errOut, code := checking(t, early); code != exitError || strings.Contains(errOut, "already running") {
		t.Errorf("a 400 run_in_progress: exit %d, stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

// A check refused during Canyon's patch watch says so in D5's one line: whose run it is, until when in UTC, and
// that a deploy ends it, so an agent neither retries blindly nor waits on a review of its own (#2568). A watch
// that is no time is the plain refusal.
func TestCheckDuringAPatchWatchSaysUntilWhen(t *testing.T) {
	watching := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusConflict,
		`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"},"watch":{"until":"2026-10-29T14:35:12Z"}}`}}
	out, errOut, code := checking(t, watching)
	want := "Canyon is watching its patch of claims-portal's preview until 14:35 UTC, so it can't review now: run canyon check again after that, or canyon deploy, which ends the watch"
	if code != exitError || !strings.Contains(errOut, want) || strings.Contains(errOut, "already running") || strings.Contains(strings.TrimSpace(errOut), "\n") {
		t.Errorf("a check during a patch watch: exit %d, stdout %q, stderr:\n%s", code, out, errOut)
	}
	odd := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusConflict,
		`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"},"watch":{"until":"soon"}}`}}
	if _, errOut, code := checking(t, odd); code != exitError || !strings.Contains(errOut, "already running (gr_9a1c)") {
		t.Errorf("a watch that is no time: exit %d, stderr:\n%s", code, errOut)
	}
}

// A 202 naming no run, or one this CLI would have to put in a URL, is not a run to poll: the
// timeline of "" is a 404 at best and someone else's run at worst.
func TestA202ThatNamesNoUsableRunIsNotPolled(t *testing.T) {
	for _, id := range []string{"", "gr_1", "../../etc/passwd", "gr_UPPER", "gr_8f2a/../other"} {
		h := &gateHost{start: reply{http.StatusAccepted, `{"run":{"id":"` + id + `","state":"pending"}}`}, run: aRun("clean", 0, 0)}
		out, errOut, code := checking(t, h)
		if code != exitError || h.polled() || !strings.Contains(errOut, "not Canyon") {
			t.Errorf("a 202 naming %q: exit %d, polled %v, stdout %q, stderr:\n%s", id, code, h.polled(), out, errOut)
		}
	}
}

// The verdict is read from the run, and every field it turns on survives the wire.
func TestTheRunReadIsWhereAVerdictComesFrom(t *testing.T) {
	body := `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"model","blocking":1,"warnings":2,"findings":[` + blocker + `]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	var read struct {
		Run runResult `json:"run"`
	}
	if err := newClient(server.URL).get(context.Background(), "/api/v1/gate-runs/gr_8f2a", &read); err != nil {
		t.Fatal(err)
	}
	got := checkRun{App: "claims-portal", Host: server.URL, Run: read.Run.ID, StartedAt: "2026-09-02T09:31:04Z",
		Files: 241, Bytes: 1 << 20, Elapsed: time.Second, Result: read.Run}
	blocks, said := got.Result.Findings[0].Blocking()
	if got.Run != "gr_8f2a" || got.Result.Package != "pkg_1" || got.Result.State != "failed" || got.Result.FailureReason != "model" ||
		got.Result.Blocking != 1 || got.Result.Warnings != 2 || !blocks || !said {
		t.Errorf("the run read: %+v", got.Result)
	}
}

// --format sarif is the host's log, as sent, and one newline on stdout — nothing else, whatever
// the run narrated — with check's exit; a failed run has no log and prints none; only check has it.
func TestCheckFormatSARIFPrintsTheHostsLogAndNothingElse(t *testing.T) {
	const log = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"canyon-scanners"}},"results":[{"ruleId":"secrets.client-bundle","suppressions":[{"kind":"external","justification":"rotated"}]}]}]}`
	for _, c := range []struct {
		run  string
		code int
		out  string
	}{
		{aRun("blocked", 1, 1, blocker, advisory), exitBlocked, log + "\n"},
		{aRun("clean", 0, 1, advisory), exitOK, log + "\n"},
		{`{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"model","blocking":0,"warnings":0,"findings":[]}}`, exitError, ""},
	} {
		h := &gateHost{run: c.run, sarif: log}
		out, errOut, code := verbingIn(t, h, projectDir(t), "check", "--format", "sarif")
		if code != c.code || out != c.out {
			t.Errorf("exit %d, want %d; stdout %q, want %q; stderr:\n%s", code, c.code, out, c.out, errOut)
		}
		if asked := slices.Contains(h.asked, "GET /api/v1/gate-runs/gr_8f2a/sarif"); asked != (c.out != "") {
			t.Errorf("the log was read %v on a %s run: %q", asked, c.run, h.asked)
		}
	}
	if out, err, code := run(t, nil, false, "deploy", "--format", "sarif"); code != exitError || out != "" || !strings.Contains(err, "deploy has no sarif rendering") {
		t.Errorf("deploy --format sarif: exit %d, stdout %q, stderr %q", code, out, err)
	}
}

// A poll that fails leaves the run going on the host; the line that says so names the app's page,
// never a run id a person does not say (canyonv2-5r11.18, as .12).
func TestARunThatOutlivesTheTerminalNamesNoRunID(t *testing.T) {
	for verb, want := range map[string]string{"check": "\n  The review keeps going without this terminal:\n", "deploy": "\n  The deploy keeps going without this terminal:\n"} {
		h := &gateHost{run: aRun("clean", 0, 0), page: "not json"}
		_, errOut, code := verbingIn(t, h, projectDir(t), verb)
		if code == exitOK || !strings.Contains(errOut, want) || strings.Contains(errOut, "gr_") {
			t.Errorf("%s: exit %d, stderr %q", verb, code, errOut)
		}
	}
}

// A fresh directory is the one dead end check had: it names the way out, canyon deploy writes the file
// and asks for the app, and asks the host nothing (#873).
func TestCheckInAFreshDirectoryNamesTheWayOut(t *testing.T) {
	h := &gateHost{}
	_, errOut, code := verbingIn(t, h, t.TempDir(), "check")
	if code != exitError || !strings.Contains(errOut, "there is no canyon.toml in") || !strings.Contains(errOut, "run canyon deploy here") || len(h.asked) != 0 {
		t.Fatalf("exit %d, asked %q, stderr:\n%s", code, h.asked, errOut)
	}
}

// A verb's help speaks the reader's words: what a report attaches to is the review, never "the
// package", Canyon's store's word (#1990).
func TestCheckHelpSaysNoInternalWord(t *testing.T) {
	for _, verb := range []string{"check", "deploy"} {
		out, _, _ := run(t, nil, false, verb, "--help")
		if strings.Contains(out, "the package") || !strings.Contains(out, "--sarif") {
			t.Errorf("%s --help:\n%s", verb, out)
		}
	}
}

// A review started under a coding agent says which (#2953): Claude Code's markers send claude_code, Codex's codex, and
// a person's terminal sends no Canyon-Harness at all.
func TestAReviewSaysTheAgentItRunsUnder(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{{map[string]string{"CLAUDECODE": "1"}, "claude_code"}, {map[string]string{"CODEX_SANDBOX": "seatbelt"}, "codex"}, {nil, ""}} {
		h := &gateHost{run: aRun("clean", 0, 0), project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
			start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}, page: `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`}
		server := httptest.NewServer(h)
		t.Chdir(projectDir(t))
		env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
		for k, v := range c.env {
			env[k] = v
		}
		run(t, env, false, "check")
		server.Close()
		if h.harness != c.want {
			t.Errorf("%v: Canyon-Harness %q, want %q", c.env, h.harness, c.want)
		}
	}
}
