package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	buildBegan = "2026-09-02T09:31:10Z"
	buildEnded = "2026-09-02T09:32:02Z" // 52 seconds later: the Build row's own clock
	liveURL    = "http://claims-portal.preview.localhost:7777"
	fleetURL   = "http://localhost:7777/#/apps/claims-portal"
	// privateLine is D5's, the build.log row a deploy of a private app ends on (contracts/events.md).
	privateLine = "Private: only its owners and admins can open it. Share it: canyon share <email> · canyon share everyone"
)

var buildLines = []string{
	"Paketo Buildpack for Node Engine 1.2.3",
	"  Running npm ci",
	"Saving claims-portal:preview",
}

// buildPage is the one timeline page a deploy reads: the build as the executor wrote it, and when
// something shipped the deploy.completed row that closes it and the access rows the server writes
// after it — the private line, unless the case names its own.
func buildPage(t *testing.T, lines []string, live bool, access ...string) string {
	t.Helper()
	rows := []string{`{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}}`}
	logged := func(id int, ts, line string) {
		data, err := json.Marshal(map[string]string{"line": line})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, fmt.Sprintf(`{"id":%d,"ts":%q,"kind":"build.log","data":%s}`, id, ts, data))
	}
	for i, line := range lines {
		logged(50+i, buildBegan, line)
	}
	if live {
		rows = append(rows, fmt.Sprintf(`{"id":90,"ts":%q,"kind":"deploy.completed","data":{"env":"preview","url":%q,`+
			`"package":"pkg_1","image_digest":"sha256:%s"}}`, buildEnded, liveURL, strings.Repeat("ab", 32)))
		if len(access) == 0 {
			access = []string{privateLine}
		}
		for i, line := range access {
			logged(91+i, buildEnded, line)
		}
	}
	return `{"rows":[` + strings.Join(rows, ",") + `],"next":null}`
}

// deployTail renders what a deploy prints from the poll onward, with stdout and stderr in one
// buffer, so a golden is the transcript in the order a terminal shows it.
func deployTail(t *testing.T, verb string, run checkRun, page string) (string, int) {
	t.Helper()
	var b strings.Builder
	a := &app{out: &b, err: &b, verb: command{name: verb}}
	var rows timelinePage
	if err := json.Unmarshal([]byte(page), &rows); err != nil {
		t.Fatal(err)
	}
	watch, _ := a.narrate(&run)
	for _, row := range rows.Rows {
		watch(row)
	}
	code := a.verdict(run) // the buffer is read after the verdict has written to it
	return b.String(), code
}

func golden(t *testing.T, name string) string {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // a fixture this test named
	if err != nil {
		t.Fatal(err)
	}
	return string(want)
}

// cleanGate is the golden run with its blocker dropped: a gate that passed, and the two
// warnings that did not stop it.
func cleanGate(t *testing.T) checkRun {
	t.Helper()
	run := goldenRun(t)
	res := runResult{ID: run.Result.ID, Package: run.Result.Package, State: "clean"}
	for _, f := range run.Result.Findings {
		if blocks, _ := f.Blocking(); !blocks {
			res.Findings, res.Warnings = append(res.Findings, f), res.Warnings+1
		}
	}
	run.Result = res
	return run
}

func TestTheSixExitsOfCanyonDeploy(t *testing.T) {
	failed := func(reason string) checkRun {
		run := cleanGate(t)
		run.Result.State, run.Result.FailureReason = "failed", reason
		return run
	}
	for _, c := range []struct {
		name, verb string
		run        checkRun
		live       bool
		code       int
		want       string
	}{
		{"clean and deployed", "deploy", cleanGate(t), true, exitOK, "✓ Review passed"},
		{"clean and nothing deployed", "deploy", cleanGate(t), false, exitError, "deployed nothing"},
		{"blocked and deployed under approvals", "deploy", goldenRun(t), true, exitOK, "Deployed anyway"},
		{"blocked and nothing deployed", "deploy", goldenRun(t), false, exitBlocked, "Nothing was built"},
		{"the build failed", "deploy", failed("build"), false, exitError, "✗ Build failed"},
		{"the app did not start", "deploy", failed("start"), false, exitError, "✗ The app did not start"},
		{"the cluster had no room to build it", "deploy", failed("no_room"), false, exitError, "✗ The cluster had no room to build it"},
		{"the cluster had no room to scan it", "deploy", failed("scan_no_room"), false, exitError, "✗ The cluster had no room to run Canyon's checks"},
		{"a check the cluster had no room to scan", "check", failed("scan_no_room"), false, exitError, "Run it again in a few minutes: canyon check"},
		{"the run failed some other way", "deploy", failed("model"), false, exitError, "The review did not finish"},
		{"a check handed a deploy row", "check", goldenRun(t), true, exitBlocked, "Fix it, then check again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := deployTail(t, c.verb, c.run, buildPage(t, buildLines, c.live))
			if code != c.code || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want %d and %q:\n%s", code, c.code, c.want, out)
			}
			if shipped := c.live && c.verb == "deploy"; strings.Contains(out, liveURL) != shipped {
				t.Errorf("the live URL is on screen %v, and this run shipped %v:\n%s", !shipped, shipped, out)
			}
		})
	}
}

// The first deploy on a Docker host waits minutes for Canyon to build its own images before the build writes a
// line, and the Build row read [35.0s] after a 5-minute wait (#2711). The run's build.image row carries the wait:
// the Build row counts it and names it. A run with none, as before, is deploy-clean.txt; a wait the row cannot
// say (zero, or not a number) adds nothing.
func TestABuildThatWaitedForCanyonsBuildImageSaysSo(t *testing.T) {
	prepared := func(data string) string {
		return strings.Replace(buildPage(t, buildLines, true), `{"id":50,`, `{"id":49,"ts":"2026-09-02T09:31:09Z","kind":"build.image","data":`+data+`},{"id":50,`, 1)
	}
	if out, _ := deployTail(t, "deploy", cleanGate(t), prepared(`{"waited_ms":278000}`)); out != golden(t, "deploy-prepared.txt") {
		t.Errorf("canyon deploy rendered\n%s\ntestdata/deploy-prepared.txt wants\n%s", out, golden(t, "deploy-prepared.txt"))
	}
	for _, data := range []string{`{"waited_ms":0}`, `{"waited_ms":"5m"}`, `{}`} {
		if out, _ := deployTail(t, "deploy", cleanGate(t), prepared(data)); out != golden(t, "deploy-clean.txt") {
			t.Errorf("%s rendered\n%s", data, out)
		}
	}
}

func TestTheCleanDeployStreamsItsBuildAndRendersItsGolden(t *testing.T) {
	out, _ := deployTail(t, "deploy", cleanGate(t), buildPage(t, buildLines, true))
	if want := golden(t, "deploy-clean.txt"); out != want {
		t.Errorf("canyon deploy rendered\n%s\ntestdata/deploy-clean.txt wants\n%s", out, want)
	}
}

// The test cluster's PHP app on Postgres, its build's own log (testdata/build-php-pgsql.log, the registry's host
// replaced): Composer refused ext-pdo_pgsql because the build image lacked libpq (#1734). The headline is the
// executor's line, the step and its code, and Composer's own words stand above it whole; no culprit is named.
func TestAFailedBuildOfTheBedsPHPAppSaysTheStepAndShowsComposersWords(t *testing.T) {
	raw, err := os.ReadFile("testdata/build-php-pgsql.log")
	if err != nil {
		t.Fatal(err)
	}
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	out, _ := deployTail(t, "deploy", run, buildPage(t, strings.Split(strings.TrimRight(string(raw), "\n"), "\n"), false))
	if !strings.Contains(out, "Root composer.json requires PHP extension ext-pdo_pgsql * but it is missing from your system.") ||
		!strings.Contains(out, "\n  ✗ Build failed: the build step exited 51\n  The review was clean, so this is not a policy block: the build's own lines above say what failed.\n") ||
		strings.Contains(out, "compile error") {
		t.Errorf("the test cluster's failed PHP build rendered\n%s", out)
	}
}

// A pin the build doesn't offer is the app's to change (#2950): the screen says so under its last line.
func TestAFailedPinSaysToChangeIt(t *testing.T) {
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	out, _ := deployTail(t, "deploy", run, buildPage(t, pinnedBuild, false))
	if !strings.HasSuffix(out, "\n  ✗ Build failed: the build step exited 51\n  The review was clean, so this is not a policy block: the build's own lines above say what failed.\n"+
		"  The app pins a version this build doesn't offer: change it to one listed above.\n  Then run: canyon deploy\n") {
		t.Errorf("a failed pin rendered\n%s", out)
	}
}

// A build that wrote nothing points at no lines: the screen says it wrote none.
func TestAQuietBuildFailureSaysItWroteNoLines(t *testing.T) {
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	if out, _ := deployTail(t, "deploy", run, buildPage(t, nil, false)); !strings.HasSuffix(out, "\n  ✗ Build failed\n  The review was clean, so this is not a policy block. The build wrote no lines.\n  Then run: canyon deploy\n") {
		t.Errorf("a quiet failed build rendered\n%s", out)
	}
}

func TestABuildFailureSaysItsOwnLastLineAndNamesNoCulprit(t *testing.T) {
	failing := append(buildLines[:2:2], "\x1b[34m\x1b[1mPaketo Buildpack for CA Certificates\x1b[0m\x1b[34m 3.14.0\x1b[0m",
		"ERROR: failed to build: src/app/page.tsx:14:8 - Type error: Property 'total' does not exist on type 'Expense[]'.")
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	out, _ := deployTail(t, "deploy", run, buildPage(t, failing, false))
	// The held build log prints now, colour codes stripped rather than spelled out (cli.md §3.1).
	if !strings.Contains(out, "\n  Build\n  │ Paketo Buildpack for Node Engine 1.2.3\n  │   Running npm ci\n  │ Paketo Buildpack for CA Certificates 3.14.0\n  │ ERROR: failed to build") ||
		strings.Contains(out, `\x1b`) || strings.ContainsRune(out, 0x1b) {
		t.Errorf("a failed build's log is not its lines, colour codes stripped:\n%s", out)
	}
	// §9's three lines close the screen, never the gate's generic one: the build's own last line, and no
	// culprit — a build fails on the app, a buildpack or the host's image, which Canyon cannot tell apart (#1734).
	if !strings.HasSuffix(out, "\n  ✗ Build failed: ERROR: failed to build: src/app/page.tsx:14:8 - Type error: Property 'total' does not exist on type 'Expense[]'.\n"+
		"  The review was clean, so this is not a policy block: the build's own lines above say what failed.\n  Then run: canyon deploy\n") {
		t.Errorf("§9's three lines are not what closed the screen:\n%s", out)
	}
	// Paths that stayed home are named under it, before the fix line (#1246): Canyon took them, so
	// the builder hears it before concluding the code is wrong. Above, with none, there is no line.
	run.NotPacked = []string{".env", "dist/", "node_modules/", "link.js", ".venv/"}
	if out, _ = deployTail(t, "deploy", run, buildPage(t, failing, false)); !strings.HasSuffix(out, "say what failed.\n"+
		"  Not packed, in case the build needs one: .env, dist/, node_modules/ and 2 more\n  Then run: canyon deploy\n") {
		t.Errorf("the failed build does not name what stayed home:\n%s", out)
	}
	run.NotPacked = nil
	// A check builds nothing, so the compile error is not its screen to explain: it keeps the
	// generic line, and never tells a builder to run the verb they did not run.
	checked, code := deployTail(t, "check", run, buildPage(t, failing, false))
	if code != exitError || !strings.Contains(checked, "The review did not finish: build") ||
		strings.Contains(checked, "Build failed") {
		t.Errorf("a check exited %d and printed the deploy's build-failure copy:\n%s", code, checked)
	}
}

// detectorLog is testdata/detect-21.log of the executor, a real pack run of a Rust tree on the Paketo
// noble builder: what a failed detect streamed, and what the screen never prints.
func detectorLog(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "executor", "testdata", "detect-21.log"))
	if err != nil {
		t.Fatal(err)
	}
	return append(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), "no buildpack group passed detection")
}

// A package no buildpack on the host detects was never compiled: no "compile error", no "Fix it". The
// screen says the host can't build this kind of app and names what it builds, the run read's builds,
// and never prints the detector's lines, which stay on the run (cli.md §9; #1242). A 20 says the code
// is not at fault; a 21, where a buildpack errored while detecting, says a manifest may be malformed.
// Paths that stayed home are named either way, and the last line asks for the kind of app Canyon doesn't build.
func TestAFailedDetectSaysWhatTheHostBuilds(t *testing.T) {
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "detect"
	run.Result.Builds = []string{"Paketo Buildpack for Ruby", "Paketo Buildpack for .NET Core", "Paketo Buildpack for Go", "Paketo Buildpack for Python", "Paketo Buildpack for PHP",
		"Paketo Buildpack for Web Servers", "Paketo Buildpack for Java Native Image", "Paketo Buildpack for Java", "Paketo Buildpack for Node.js", "Paketo Buildpack for Procfile"}
	head := "\n  ✗ This host can't build this kind of app.\n" +
		"  It builds:\n" + // one name a line: a name is never broken across two
		"    Paketo Buildpack for Ruby\n    Paketo Buildpack for .NET Core\n    Paketo Buildpack for Go\n    Paketo Buildpack for Python\n    Paketo Buildpack for PHP\n" +
		"    Paketo Buildpack for Web Servers\n    Paketo Buildpack for Java Native Image\n    Paketo Buildpack for Java\n    Paketo Buildpack for Node.js\n    Paketo Buildpack for Procfile\n"
	ask := "  " + askFor(languageUnrecognised) + "\n" // what Canyon doesn't build yet, asked for (#2800)
	clean := func(out string) bool {
		return strings.Contains(out, "✓ Review passed") && !strings.Contains(out, "[detector]") && !strings.Contains(out, "\n  Build\n") &&
			!strings.Contains(out, "compile error") && !strings.Contains(out, "Fix it") && !strings.Contains(out, "--build-log")
	}
	out, code := deployTail(t, "deploy", run, buildPage(t, detectorLog(t), false))
	if code != exitError || !clean(out) || !strings.HasSuffix(out, head+
		"  No buildpack recognised the package, so nothing was compiled: not an error in the code, not a policy block.\n"+ask) {
		t.Errorf("a 20: exit %d; §9's failed detect is not what closed the screen:\n%s", code, out)
	}
	run.Result.Errored, run.NotPacked = true, []string{".env", "node_modules/"}
	if out, code = deployTail(t, "deploy", run, buildPage(t, detectorLog(t), false)); code != exitError || !clean(out) || !strings.HasSuffix(out, head+
		"  A buildpack also errored while checking. If this app is meant to be one of those stacks, check that its manifest is valid (for example package.json).\n"+
		"  Not packed, in case the build needs one: .env and node_modules/\n"+ask) || strings.Contains(out, "not an error in the code") {
		t.Errorf("a 21 with paths left home: exit %d; the screen says the code is fine, or names nothing that stayed home:\n%s", code, out)
	}
	run.Result.Builds, run.Result.Errored, run.NotPacked = []string{}, false, nil
	if out, _ = deployTail(t, "deploy", run, buildPage(t, detectorLog(t), false)); !strings.Contains(out, "\n  Its builder doesn't say what it builds.\n") || strings.Contains(out, "It builds:") {
		t.Errorf("a builder that names nothing is said so:\n%s", out)
	}
}

// Over the wire a failed detect reads what the host builds and errored off the run read; a read that
// carries no builds says the builder named none, and a terminal with no colour and no glyphs reads the
// same words.
func TestAFailedDetectOverTheWire(t *testing.T) {
	read := func(extra string) string {
		return `{"run":{"id":"gr_8f2a","package":"pkg_1","state":"failed","failure_reason":"detect"` + extra + `,"blocking":0,"warnings":0,"findings":[]}}`
	}
	for _, c := range []struct {
		extra, want string
		env         map[string]string
	}{
		{`,"builds":["Paketo Buildpack for Node.js"],"errored":true`, "\n  It builds:\n    Paketo Buildpack for Node.js\n  A buildpack also errored while checking.", nil},
		{"", "\n  Its builder doesn't say what it builds.\n  No buildpack recognised the package", nil},
		{`,"builds":["Paketo Buildpack for Node.js"]`, "\n  x This host can't build this kind of app.\n  It builds:\n    Paketo Buildpack for Node.js\n", map[string]string{"NO_COLOR": "1", "CANYON_ASCII": "1"}},
	} {
		h := &gateHost{run: read(c.extra), page: buildPage(t, []string{"[detector] err:  paketo-buildpacks/node-start@2.7.7 (1)"}, false),
			project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}, start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}}
		server := httptest.NewServer(h)
		t.Chdir(projectDir(t))
		env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
		maps.Copy(env, c.env)
		out, errOut, code := run(t, env, false, "deploy")
		server.Close()
		if code != exitError || !strings.Contains(errOut, c.want) || strings.Contains(out+errOut, "[detector]") || strings.ContainsRune(out+errOut, 0x1b) {
			t.Errorf("read %s: exit %d\n%s%s", read(c.extra), code, out, errOut)
		}
	}
}

// A deploy that failed some other way says why in its last line — a driver, a missing front door —
// so the held lines print for it too, never only the generic reason.
func TestADeployThatFailedAnyOtherWaySaysWhyInItsLines(t *testing.T) {
	frontDoor := "No front door is set, so no app can run on this Canyon yet. An admin sets it in Settings › App hosting."
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "error"
	out, code := deployTail(t, "deploy", run, buildPage(t, append(buildLines[:1:1], frontDoor), false))
	if code != exitError || !strings.Contains(out, "│ "+frontDoor+"\n") {
		t.Errorf("exit %d; the failure's own line is not on screen:\n%s", code, out)
	}
}

// An app that built and stopped as it started is the app's error too: its own last lines and the
// executor's reason stream under Build, and §9's three lines close the screen, never "Canyon itself failed".
func TestAnAppThatDidNotStartIsTheAppsNotCanyons(t *testing.T) {
	stopped := append(buildLines[:2:2], "node: error while loading shared libraries: libatomic.so.1: cannot open shared object file",
		"the app did not stay up: its container restarted (Error, exit 127)")
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "start"
	out, code := deployTail(t, "deploy", run, buildPage(t, stopped, false))
	if code != exitError || !strings.Contains(out, "│ node: error while loading shared libraries: libatomic.so.1") || strings.Contains(out, "Canyon itself failed") ||
		!strings.HasSuffix(out, "restarted (Error, exit 127)\n\n  ✗ The app did not start\n  The build passed — the app stopped as it started, which is its own error, not a policy block.\n"+
			"  Fix it, or set a value it needs with canyon secret set, and run: canyon deploy\n") {
		t.Errorf("exit %d; the app's lines and §9's three lines are not what closed the screen:\n%s", code, out)
	}
}

// A stock starter that did not start for want of its one file is told which file, why and what goes
// in it, in place of "Fix it" (contracts/routes.md, What a starter is missing; #1654).
func TestAStarterMissingItsFileIsToldWhatToAdd(t *testing.T) {
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "start"
	if err := json.Unmarshal([]byte(procfileFix), &run.Result.Fix); err != nil {
		t.Fatal(err)
	}
	out, code := deployTail(t, "deploy", run, buildPage(t, buildLines, false))
	if want := "\n  ✗ The app did not start\n" +
		"  The build gave the app no start command, so Python started with nothing to run and exited: a Python web app needs a Procfile at its root naming the command that starts it on $PORT.\n" +
		"  Add a file named Procfile at the app's root, holding the lines for this app.\n" +
		"  For FastAPI, with uvicorn in requirements.txt (fastapi[standard] brings it):\n      web: uvicorn main:app --host 0.0.0.0 --port $PORT\n" +
		"  For Flask, with gunicorn in requirements.txt:\n      web: gunicorn app:app --bind 0.0.0.0:$PORT\n" +
		"  Then run: canyon deploy\n"; code != exitError || !strings.HasSuffix(out, want) {
		t.Errorf("exit %d; the missing Procfile did not close the screen:\n%s", code, out)
	}
	run.Result.FailureReason = "detect"
	if err := json.Unmarshal([]byte(staticFix), &run.Result.Fix); err != nil {
		t.Fatal(err)
	}
	out, _ = deployTail(t, "deploy", run, buildPage(t, buildLines, false))
	if !strings.Contains(out, "✗ The app is missing a project.toml\n") || !strings.Contains(out, "      value = \"nginx\"\n\n      [[io.buildpacks.build.env]]\n") || strings.Contains(out, "can't build this kind of app") {
		t.Errorf("a static site's missing project.toml is named, never \"can't build\":\n%s", out)
	}
}

// A blocked deploy is a blocked check in three ways: the build above it, the verdict's second
// line, and the URLs where the fix loop and the history block were. Diffing the two whole
// screens says that, and keeps one copy of the findings rather than two that drift.
func TestABlockedDeployIsChecksScreenAndSaysWhatItDid(t *testing.T) {
	nothing, code := deployTail(t, "deploy", goldenRun(t), buildPage(t, nil, false))
	verdict := "  ✗ Review blocked · 1 blocking finding\n"
	if blocked := "\n  Review\n" + strings.Replace(golden(t, "check-blocked.txt"), verdict, verdict+"\n  Nothing was built and nothing was deployed.\n", 1); nothing != blocked || code != exitBlocked {
		t.Fatalf("a blocked deploy exited %d and rendered\n%s\ncheck's own screen is\n%s", code, nothing, blocked)
	}
	shipped, code := deployTail(t, "deploy", goldenRun(t), buildPage(t, buildLines, true))
	if code != exitOK { // exiting 2 on a deploy that shipped sends an agent into a fix loop over live code
		t.Errorf("a blocked run that deployed under approvals exited %d, want %d", code, exitOK)
	}
	clean := golden(t, "deploy-clean.txt")
	head, _, _ := strings.Cut(clean, "\n  ✓ Review passed")
	_, rest, _ := strings.Cut(clean, "\n  ✓ Build")
	rows, _, _ := strings.Cut(rest, "\n\n  Live") // the verdict first, then what shipped (cli.md §3.1)
	want := strings.Replace(golden(t, "check-blocked.txt"), verdict, verdict+"\n  Deployed anyway: every blocking finding carries an approval.\n", 1)
	want = head + strings.Replace(want, "\n  Fix it, then check again without deploying:"+
		"\n    canyon check\n\n  "+approvers+"\n\n  Open in Canyon "+fleetURL+"\n",
		"\n  ✓ Build"+rows+"\n\n  Live   "+liveURL+"\n  Fleet  "+fleetURL+"\n  "+privateLine+"\n", 1) // Next: follows a clean deploy only
	if shipped != want {
		t.Errorf("the deploy that shipped rendered\n%s\nthe blocked screen plus the build and the URLs is\n%s", shipped, want)
	}
}

// A deploy run's rows after deploy.completed are the app's access (contracts/events.md): printed
// after the URLs in the server's words and order, never as build output, under both formats, and
// where §3.1's transcript prints its private line.
func TestTheAccessRowsFollowTheURLsInTheServersWords(t *testing.T) {
	access := []string{"Shared with 3 people and 1 group.",
		"Access requests: sam@meridian.example, 2h ago · canyon share sam@meridian.example",
		"No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."}
	page := buildPage(t, buildLines, true, access...)
	human, _ := deployTail(t, "deploy", cleanGate(t), page)
	if !strings.Contains(human, "\n  Fleet  "+fleetURL+"\n  "+strings.Join(access, "\n  ")+"\n") || strings.Contains(human, "│ Shared") {
		t.Errorf("the access rows are not the lines after Fleet:\n%s", human)
	}
	var rows timelinePage
	must(t, json.Unmarshal([]byte(page), &rows))
	var ai strings.Builder
	a, run := aiApp(&ai, "deploy"), cleanGate(t)
	watch, _ := a.narrate(&run)
	for _, row := range rows.Rows {
		watch(row)
	}
	if a.verdict(run); !strings.Contains(ai.String(), "\nfleet url: "+fleetURL+"\n"+strings.Join(access, "\n")+"\n\n") {
		t.Errorf("the access rows are not the lines after fleet url:\n%s", ai.String())
	}
	if !strings.Contains(specFence(t, "### 3.1 The clean run"), "\n  Fleet  http://localhost:7777/#/apps/expense-tracker\n  "+privateLine+"\n") {
		t.Errorf("§3.1 of design/specs/cli.md does not print the private line under Fleet")
	}
}

// Off a terminal a failed build reads in the order it ran: the review's heading, its stage row and
// rows, then Build and the build's own lines, never the review's rows inside the build's (cli.md §9).
func TestAFailedBuildReadsTheReviewThenTheBuild(t *testing.T) {
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	var page, checks timelinePage
	must(t, json.Unmarshal([]byte(buildPage(t, buildLines, false)), &page))
	must(t, json.Unmarshal([]byte(gatePage(checkStarted(41, "sql-injection"), checkCompleted(42, "sql-injection", "clean", 0, 800))), &checks))
	page.Rows = append(checks.Rows, page.Rows...)
	raw, err := json.Marshal(page)
	must(t, err)
	out, _ := deployTail(t, "deploy", run, string(raw))
	at := 0
	for _, want := range []string{"\n  Review\n", "\n  ✓ Review passed · 0 blocking, 3 warnings\n", "    ✓ sql-injection      clean",
		"\n  Build\n  │ Paketo Buildpack for Node Engine 1.2.3\n"} {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("%q is not where it ran, after what came before it:\n%s", want, out)
		}
		at += i + len(want)
	}
	if n := strings.Count(out, "\n  Build\n"); n != 1 { // one heading, over the log: none above the review's rows
		t.Errorf("Build is printed %d times:\n%s", n, out)
	}
}

// The run read outranks any row the poll saw: a host that deployed and then failed the build is
// contradicting itself, and the read settles it.
func TestTheRunReadOutranksTheDeployRow(t *testing.T) {
	run := goldenRun(t)
	run.Result.State, run.Result.FailureReason = "failed", "build"
	out, code := deployTail(t, "deploy", run, buildPage(t, buildLines, true))
	if code != exitError || !strings.Contains(out, "✗ Build failed") || strings.Contains(out, liveURL) {
		t.Errorf("exit %d, want %d and §9's copy with no live URL:\n%s", code, exitError, out)
	}
	if !strings.Contains(out, "The review passed under approvals, so this is not a policy block") { // this review was not clean; it was approved
		t.Errorf("a build off an approved gate claimed the gate was clean:\n%s", out)
	}
}

// One deploy over the wire: it packages the tree once, starts the run on the deploys route, and
// escapes every build.log line, because the executor's output is the executor's — a line that
// redraws the screen would otherwise let a build forge a verdict.
func TestOneDeployOverTheWire(t *testing.T) {
	forged := "\x1b[2J  ✓ Passed — 0 blocking findings, 0 warnings"
	h := &gateHost{run: aRun("blocked", 1, 0, blocker), page: buildPage(t, []string{forged}, true)}
	out, errOut, code := verbing(t, h, "deploy")
	if code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr:\n%s\nstdout:\n%s", code, errOut, out)
	}
	packaged := 0
	for _, asked := range h.asked {
		if asked == "POST /api/v1/projects/claims-portal/packages" {
			packaged++
		}
	}
	if packaged != 1 { // one package per invocation, whichever verb asked: the prologue is shared, not copied
		t.Errorf("the tree was packaged %d times: %q", packaged, h.asked)
	}
	if !slices.Contains(h.asked, "POST /api/v1/projects/claims-portal/deploys") {
		t.Errorf("the run did not start on the deploys route: %q", h.asked)
	}
	if strings.ContainsRune(out, 0x1b) || strings.Contains(out, "Passed — 0 blocking findings, 0 warnings") {
		t.Errorf("a passing build printed its log, so a build could forge a verdict: %q", out)
	}
	for _, want := range []string{"canyon " + Version + " · claims-portal → preview",
		// the trust clause is prologue's, so it prints on deploy exactly as it does on check
		"✓ Package    2 files · 38 B · node_modules excluded", "✓ Build      [52.0s]", "✓ Deploy     preview", "Live   " + liveURL} {
		if !strings.Contains(out, want) {
			t.Errorf("the deploy printed no %q:\n%s", want, out)
		}
	}
}

// Live says what the app answered Canyon's one GET / after the deploy, beside the URL and plain, never red:
// a stock Django's 400 with its body's first line under it, or why nothing answered (contracts/routes.md,
// envs.<env>.answered; #1740). A row from before carries none, and Live is the URL alone.
func TestLiveSaysWhatTheAppAnsweredAtItsRoot(t *testing.T) {
	for answered, want := range map[string]string{
		`{"status":400,"said":"<h1>Bad Request (400)</h1>"}`: "  Live   " + liveURL + " · answered 400 Bad Request at /\n         <h1>Bad Request (400)</h1>\n  Fleet",
		`{"status":200,"said":""}`:                           "  Live   " + liveURL + " · answered 200 OK at /\n  Fleet",
		`{"error":"no answer in 5 s"}`:                       "  Live   " + liveURL + " · no answer at /: no answer in 5 s\n  Fleet",
		`{"status":401,"said":""}`:                           "  Live   " + liveURL + " · answered 401 Unauthorized at / (Canyon's check sends no identity token, so an app that checks it answers 401 here)\n  Fleet",
	} {
		page := strings.Replace(buildPage(t, nil, true), `"image_digest":"sha256:`, `"answered":`+answered+`,"image_digest":"sha256:`, 1)
		out, _, code := verbing(t, &gateHost{run: aRun("clean", 0, 0), page: page}, "deploy")
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("answered %s: exit %d, want %q in:\n%s", answered, code, want, out)
		}
	}
}

// The one step left after a clean preview carries what the app answered when it was neither 2xx nor 3xx,
// so "Next: deploy to production" never reads clean beside a 400; the preview is not blocked, its promotion is (#1740 follow-up; cli.md §6.3).
func TestTheProdHintCarriesAnAnswerThatWasNotOK(t *testing.T) {
	for answered, want := range map[string]string{
		`{"status":400,"said":""}`:   "\n  Next: canyon deploy --env prod · answered 400 Bad Request at /\n",
		`{"error":"refused"}`:        "\n  Next: canyon deploy --env prod · no answer at /: refused\n",
		`{"status":302,"said":""}`:   "\n  Next: canyon deploy --env prod\n",
		`{"status":401,"said":""}`:   "\n  Next: canyon deploy --env prod\n", // an app's own sign-in
		`{"status":404,"said":""}`:   "\n  Next: canyon deploy --env prod\n", // an API with no page at its root
		`{"status":503,"said":""}`:   "\n  Next: canyon deploy --env prod · answered 503 Service Unavailable at /\n",
		`{"status":200,"said":"ok"}`: "\n  Next: canyon deploy --env prod\n",
	} {
		page := strings.Replace(buildPage(t, nil, true), `"image_digest":"sha256:`, `"answered":`+answered+`,"image_digest":"sha256:`, 1)
		out, _, code := verbing(t, &gateHost{run: aRun("clean", 0, 0), page: page}, "deploy")
		if code != exitOK || !strings.Contains(out, want) {
			t.Errorf("answered %s: exit %d, want %q in:\n%s", answered, code, want, out)
		}
	}
}

// A build may legitimately run for half an hour (runs.buildCeiling is 30m), so a deploy's poll
// has to outlast a review's.
func TestABuildLongerThanTheReviewCeilingIsNotAbandoned(t *testing.T) {
	building := timelineRow{ID: 41, TS: buildBegan, Kind: "build.log", Data: json.RawMessage(`{"line":"x"}`)}
	pages := []timelinePage{{Rows: []timelineRow{building}, Next: cursor(41)},
		{Rows: []timelineRow{building}, Next: cursor(42)}, {Rows: nil, Next: nil}}
	slow := func() clock { // ten minutes pass at every poll: twenty before the run ends
		at := time.Now()
		return clock{now: func() time.Time { return at }, sleep: func(time.Duration) { at = at.Add(10 * time.Minute) }}
	}
	var asked, early []string
	if _, err := poll(context.Background(), serving(t, pages, &asked), "gr_8f2a", slow(), deployCeiling, nil); err != nil {
		t.Errorf("a twenty-minute build ended with %v", err)
	}
	if _, err := poll(context.Background(), serving(t, pages, &early), "gr_8f2a", slow(), checkCeiling, nil); err == nil {
		t.Errorf("the review's ceiling let a twenty-minute build run on: %q", early)
	}
}

func TestADeployRefusesWhatItCannotDo(t *testing.T) {
	busy := &gateHost{run: aRun("clean", 0, 0), start: reply{http.StatusConflict,
		`{"error":"run_in_progress","message":"a run of this project is running","run":{"id":"gr_9a1c","state":"running"}}`}}
	if _, errOut, code := verbing(t, busy, "deploy"); code != exitError || !strings.Contains(errOut, "run canyon deploy again") {
		t.Errorf("a run already going: exit %d, stderr:\n%s", code, errOut)
	}
}

func TestATimestampThisBuildCannotReadPrintsNoTiming(t *testing.T) {
	for _, c := range [][2]string{{"", buildEnded}, {buildBegan, "not a time"}, {buildEnded, buildBegan}} {
		if got := between(c[0], c[1]); got != 0 {
			t.Errorf("between(%q, %q) = %s, want nothing rather than a wrong timing", c[0], c[1], got)
		}
	}
}

// A deploy that reused its image says so on the Build row, in every format: its first build.log line is
// the contract's reuse sentence, a declared resource's lines may follow, and no progress row of the build
// step came — which a build whose image was gone writes, and then it is no reuse (cli.md §3.1; #1666).
func TestAReusedImageSaysSoOnTheBuildRow(t *testing.T) {
	reuse := "Reusing the image built for 22bc90 (no build value changed)"
	progress := `{"id":45,"ts":"2026-09-02T09:31:04Z","kind":"progress","data":{"step":"build","status":"running"}},`
	for name, tc := range map[string]struct {
		page   string
		reused bool
	}{
		"alone":             {buildPage(t, []string{reuse}, true), true},
		"before a resource": {buildPage(t, []string{reuse, "postgres db: ready"}, true), true},
		"then built anyway": {strings.Replace(buildPage(t, []string{reuse, "===> BUILDING"}, true), `{"id":50,`, progress+`{"id":50,`, 1), false},
	} {
		for _, format := range []Format{FormatHuman, FormatAI} {
			var b strings.Builder
			a := &app{out: &b, err: &b, verb: command{name: "deploy"}, render: Render{Format: format}}
			run := cleanGate(t)
			var rows timelinePage
			if err := json.Unmarshal([]byte(tc.page), &rows); err != nil {
				t.Fatal(err)
			}
			watch, _ := a.narrate(&run)
			for _, row := range rows.Rows {
				watch(row)
			}
			a.verdict(run)
			want := map[Format]string{FormatHuman: "Build      reused the image built for 22bc90  [52.0s]", FormatAI: "\nbuild: reused the image built for 22bc90\n"}[format]
			if out := b.String(); strings.Contains(out, want) != tc.reused || tc.reused != strings.Contains(out, "reused the image") {
				t.Errorf("%s, %v: the Build row says reused %v:\n%s", name, format, !tc.reused, out)
			}
		}
	}
}

// A scan the cluster had no room for is capacity, before any review: nothing reads as a block, nothing is
// offered for approval, and the agent is told to say so and run it again later, the code untouched
// (contracts/routes.md, A failure reason in words; the 2026-10-08 QA rerun).
func TestAScanWithNoRoomIsNoBlockToApprove(t *testing.T) {
	said := "trivy: the cluster had no room to run it (0/4 nodes are available: 4 Insufficient cpu.)"
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason, run.Result.Findings, run.Result.Warnings = "failed", "scan_no_room", nil, 0
	out, code := deployTail(t, "deploy", run, buildPage(t, []string{said}, false))
	if code != exitError || !strings.Contains(out, "│ "+said) || !strings.Contains(out, "nothing to fix or approve") || strings.Contains(out, "approve it") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	var b strings.Builder
	a := &app{out: &b, render: Render{Format: FormatAI}, verb: command{name: "deploy"}}
	run.Build = []string{said}
	a.aiFailed(run)
	got := b.String()
	for _, want := range []string{"reason: scan_no_room — the cluster had no room to scan the package", "  " + said,
		"Nothing was reviewed, so there is nothing to fix or approve", "Do not change the app's code for this", "run: canyon deploy"} {
		if !strings.Contains(got, want) {
			t.Errorf("the agent's block lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "review: ") || strings.Contains(got, "approve it") {
		t.Errorf("the agent's block reads as a review or offers an approval:\n%s", got)
	}
}

// A build the cluster had no room for is capacity: the scheduler's message is the last line under Build,
// the screen names the admin's fix and never "Canyon itself failed" nor a compile error, and the agent is
// told to stop, the app's code untouched (cli.md §9; #1637).
func TestABuildWithNoRoomIsTheClustersNotTheApps(t *testing.T) {
	said := "0/4 nodes are available: 4 Insufficient cpu."
	run := cleanGate(t)
	run.Result.State, run.Result.FailureReason = "failed", "no_room"
	out, code := deployTail(t, "deploy", run, buildPage(t, append(buildLines[:1:1], said), false))
	if code != exitError || !strings.Contains(out, "│ "+said) || strings.Contains(out, "Canyon itself failed") || strings.Contains(out, "compile error") ||
		!strings.Contains(out, "an admin adds nodes, or lowers what apps request") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	var b strings.Builder
	a := &app{out: &b, render: Render{Format: FormatAI}, verb: command{name: "deploy"}}
	run.Build = []string{said}
	a.aiFailed(run)
	if got := b.String(); !strings.Contains(got, "reason: no_room — the cluster had no room to run the build") || !strings.Contains(got, "Do not change the app's code for this") {
		t.Errorf("the agent's block:\n%s", got)
	}
}

// A deploy's review is done before its build starts, and the first build on a Docker host waits many minutes
// while the executor builds canyon-builder: the review's stage row prints as the verdict row arrives, from the
// run read then, and the screen at the end is the one it always was, the review said once (cli.md §3.1; #2697).
func TestADeployPrintsItsReviewBeforeTheBuild(t *testing.T) {
	run := cleanGate(t)
	var b strings.Builder
	a := &app{out: &b, err: &b, verb: command{name: "deploy"}}
	watch := reviewedNow(t, a, &run)
	var page timelinePage
	must(t, json.Unmarshal([]byte(buildPage(t, buildLines, true)), &page))
	watch(page.Rows[0])
	watch(cleanVerdict)
	if got := b.String(); got != "\n  Review\n\n  ✓ Review passed · 0 blocking, 3 warnings\n" {
		t.Fatalf("once the verdict row is in, before any build line, the deploy printed\n%q", got)
	}
	for _, row := range page.Rows[1:] {
		watch(row)
	}
	if code := a.verdict(run); code != exitOK || b.String() != golden(t, "deploy-clean.txt") {
		t.Errorf("exit %d, and the deploy rendered\n%s\ntestdata/deploy-clean.txt wants\n%s", code, b.String(), golden(t, "deploy-clean.txt"))
	}
}

// cleanVerdict is cleanGate's verdict row, as it arrives before the build.
var cleanVerdict = timelineRow{ID: 45, TS: "2026-09-02T09:31:09Z", Kind: "verdict", Data: json.RawMessage(`{"state":"clean","blocking":0,"warnings":3,"findings":{"version":"2.1.0","runs":[]}}`)}

// reviewedNow is a deploy's narration of run with reviewNow over it, whose run read answers run's result while the
// run goes on to build: only its verdict is in.
func reviewedNow(t *testing.T, a *app, run *checkRun) func(timelineRow) {
	t.Helper()
	read := run.Result
	read.State = "running"
	body, err := json.Marshal(map[string]runResult{"run": read})
	must(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/gate-runs/gr_8f2a" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	watch, _ := a.narrate(run)
	return a.reviewNow(context.Background(), newClient(server.URL), "gr_8f2a", run, watch)
}

// A deploy that stops after its review printed — Canyon's own failure in the build, or a cancel — says the
// review once, never its rows again, and says the build did not finish: the review did (#2697).
func TestADeployThatStopsAfterItsReviewSaysItOnce(t *testing.T) {
	for reason, said := range map[string]string{
		"error":       "\n  ✗ The build did not finish: error (Canyon itself failed while building the package)\n  Run it again. If it fails again, an admin can look up run gr_8f2a in Canyon's server log.\n",
		"interrupted": "\n  ✗ The build did not finish: interrupted (it was cancelled, or another run of this app replaced it)\n",
	} {
		run := cleanGate(t)
		var b strings.Builder
		a := &app{out: &b, err: &b, verb: command{name: "deploy"}}
		watch := reviewedNow(t, a, &run)
		var checks timelinePage
		must(t, json.Unmarshal([]byte(gatePage(checkStarted(41, "sql-injection"), checkCompleted(42, "sql-injection", "clean", 0, 800))), &checks))
		for _, row := range append(checks.Rows, cleanVerdict) {
			watch(row)
		}
		run.Result.State, run.Result.FailureReason, run.Run = "failed", reason, "gr_8f2a"
		if reason == "error" {
			run.Result.FailedIn = "build"
		}
		code := a.verdict(run)
		out := b.String()
		if code != exitError || strings.Count(out, "✓ Review passed · 0 blocking, 3 warnings") != 1 || strings.Count(out, "✓ sql-injection") != 1 ||
			!strings.HasSuffix(out, said) || strings.Contains(out, "The review did not finish") {
			t.Errorf("%s: exit %d, and the deploy rendered\n%s", reason, code, out)
		}
	}
}

// On a slow line a first deploy can outlast deployCeiling while canyon-builder downloads. With its review
// printed and passed, the deploy says the app is still building and carries on, never that the review has
// not finished (#2697), and exits 3: the deploy goes on in Canyon, and 1 read as a failed one (#2721).
func TestADeployPastItsCeilingAfterItsReviewIsStillBuilding(t *testing.T) {
	h := &gateHost{run: aRun("clean", 0, 0), project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page: `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{"state":"running"}},` +
			`{"id":45,"ts":"2026-09-02T09:31:09Z","kind":"verdict","data":{"state":"clean","blocking":0,"warnings":0,"findings":{"version":"2.1.0","runs":[]}}}],"next":45}`}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/timeline") && r.URL.Query().Get("after") != "0" { // the build goes on: nothing new
			_, _ = io.WriteString(w, `{"rows":[],"next":45}`)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	want := golden(t, "deploy-still-building.txt")
	t.Chdir(projectDir(t))
	tick := time.Date(2026, 9, 2, 9, 31, 4, 0, time.UTC)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
	var o, e strings.Builder
	a := &app{out: &o, err: &e, in: strings.NewReader(""), getenv: func(k string) string { return env[k] },
		clock: clock{now: func() time.Time { return tick }, sleep: func(time.Duration) { tick = tick.Add(10 * time.Minute) }}}
	code := a.main([]string{"deploy"})
	if code != exitStillRunning || e.String() != want ||
		!strings.Contains(o.String(), "\n  ✓ Review passed · 0 blocking, 0 warnings\n") || strings.Contains(o.String()+e.String(), "has not finished") {
		t.Errorf("exit %d; stdout:\n%s\nstderr:\n%q\ntestdata/deploy-still-building.txt wants\n%q", code, o.String(), e.String(), want)
	}
}
