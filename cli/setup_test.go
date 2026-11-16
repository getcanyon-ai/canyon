package cli

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshDir is a directory named after the app, holding one file and no canyon.toml: §3.2's first deploy.
func freshDir(t *testing.T, named string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), named)
	writeFile(t, filepath.Join(dir, "index.js"), "console.log(1);\n")
	return dir
}

// deployIn runs canyon deploy in dir with input on stdin, against a host whose gate blocks.
func deployIn(t *testing.T, h *gateHost, dir, input string, tty bool, args ...string) (out, errOut string, code int) {
	t.Helper()
	return deployEnv(t, h, dir, input, tty, nil, args...)
}

// deployEnv is deployIn with more of the environment the verb reads: a harness variable, a locale.
func deployEnv(t *testing.T, h *gateHost, dir, input string, tty bool, extra map[string]string, args ...string) (out, errOut string, code int) {
	t.Helper()
	if h.run == "" { // the demo's gate answers every run the same way, so every test's host does
		h.project = reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`}
		h.start = reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}
		h.page = `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`
		h.run = aRun("blocked", 1, 0, blocker)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(dir)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	maps.Copy(env, extra)
	return answer(t, input, env, tty, append([]string{"deploy"}, args...)...)
}

func written(t *testing.T, dir string) (string, os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, projectFile)
	info, err := os.Stat(path)
	if err != nil {
		return "", 0
	}
	body, err := os.ReadFile(path) //nolint:gosec // the file this test asked a deploy to write
	must(t, err)
	return string(body), info.Mode().Perm()
}

// picked is a host that picks funny-snake-4k2 for whatever name it is sent (contracts/routes.md, POST /projects {name}).
func picked() *gateHost {
	return &gateHost{project: reply{http.StatusCreated, `{"project":{"id":"funny-snake-4k2","name":"claims-app"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page:  `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`,
		run:   aRun("blocked", 1, 0, blocker)}
}

// A first deploy sends the directory's name as the app's name and writes the id the host made: nothing
// is asked, at a terminal or off one (cli.md §3.2; #1911).
func TestTheFileTheSetupBlockWrites(t *testing.T) {
	for _, tty := range []bool{true, false} {
		dir, h := freshDir(t, "claims-app"), picked()
		out, errOut, code := deployIn(t, h, dir, "", tty)
		body, mode := written(t, dir)
		if body != "app = \"funny-snake-4k2\"\n" || mode != 0o644 { // one line, the id the host made, as committed source is
			t.Errorf("canyon.toml holds %q at %v", body, mode)
		}
		if len(h.created) == 0 || h.created[0] != `{"name":"claims-app"}` {
			t.Errorf("the host was sent %q", h.created)
		}
		block := "\n  No canyon.toml here — setting up this app.\n" +
			"  app: funny-snake-4k2 (named \"claims-app\" after this folder)\n" +
			"\n  ✓ Wrote canyon.toml — commit it so your team deploys the same app.\n"
		if !strings.HasPrefix(out, block) || strings.Contains(errOut, "Look right?") || code != exitBlocked {
			t.Errorf("tty %v: the setup block rendered (exit %d)\n%q\nwant\n%q\nstderr %q", tty, code, out, block, errOut)
		}
		if !strings.Contains(out, "canyon "+Version+" · funny-snake-4k2 → preview") {
			t.Errorf("the run's header names another app:\n%s", out)
		}
	}
}

// A directory no app id could be named after is a name like any other: the host picks the id, so a
// first deploy never fails on a name (#1911), and a name past 100 characters is cut to them.
func TestAnyDirectoryNameMakesAnApp(t *testing.T) {
	for named, sent := range map[string]string{"My_App": "My_App", "1claims": "1claims", "claims.portal": "claims.portal",
		strings.Repeat("é", 120): strings.Repeat("é", 100)} {
		dir, h := freshDir(t, named), picked()
		_, errOut, code := deployIn(t, h, dir, "", false)
		if body, _ := written(t, dir); code != exitBlocked || body != "app = \"funny-snake-4k2\"\n" || len(h.created) == 0 || h.created[0] != `{"name":"`+sent+`"}` {
			t.Errorf("a directory named %q exited %d, sent %q and wrote %q:\n%s", named, code, h.created, body, errOut)
		}
	}
}

// The id the host answers is written into canyon.toml unquoted, so one that is no app id writes nothing.
func TestAnIdTheHostAnswersThatIsNoAppIdIsNotWritten(t *testing.T) {
	dir, h := freshDir(t, "claims-app"), picked()
	h.project = reply{http.StatusCreated, `{"project":{"id":"x\"\n[build]"}}`}
	_, errOut, code := deployIn(t, h, dir, "", false)
	if body, _ := written(t, dir); code != exitError || body != "" || !strings.Contains(errOut, "is not a Canyon app id") {
		t.Errorf("exit %d, wrote %q:\n%s", code, body, errOut)
	}
}

// A canyon.toml with no app line is an app not made yet: the line goes at its top, and the rest stays.
func TestAFileWithNoAppLineGetsOne(t *testing.T) {
	dir, h := freshDir(t, "claims-app"), picked()
	rest := "# the claims team's app\ndescription = \"Claims for the field team\"\n\n[resources]\ndb = { type = \"postgres\" }\n"
	writeFile(t, filepath.Join(dir, projectFile), rest)
	out, errOut, code := deployIn(t, h, dir, "", false)
	if body, _ := written(t, dir); code != exitBlocked || body != "app = \"funny-snake-4k2\"\n"+rest {
		t.Errorf("exit %d, the file now holds %q:\n%s", code, body, errOut)
	}
	if !strings.HasPrefix(out, "\n  No app in canyon.toml — setting up this app.\n") {
		t.Errorf("the block opened\n%s", out)
	}
}

// Someone else's app, named by a canyon.toml copied from them, says how to make your own (cli.md §9).
func TestADeployRefusedOnAnotherTeamsAppSaysHowToMakeYourOwn(t *testing.T) {
	refused := reply{http.StatusForbidden, `{"error":"team_required","message":"Ask Dana Ruiz to add you to claims-portal","owners":[]}`}
	dir := freshDir(t, "claims-app")
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	_, errOut, code := deployIn(t, &gateHost{upload: refused}, dir, "", false)
	hint := "To make your own app instead, delete the app line from canyon.toml and deploy again."
	if code != exitError || !strings.Contains(errOut, hint) {
		t.Errorf("exit %d:\n%s", code, errOut)
	}
	_, errOut, _ = deployIn(t, &gateHost{upload: refused}, freshDir(t, "claims-app"), "", false, "--app", "claims-portal")
	if strings.Contains(errOut, hint) { // the name was typed, not copied
		t.Errorf("--app was told to delete a line it did not write:\n%s", errOut)
	}
}

func TestAnExistingCanyonTomlIsNeverReplaced(t *testing.T) {
	broken := "app = [\"claims-portal\"]\n" // a file that exists and Canyon will not read
	dir, h := freshDir(t, "claims-portal"), &gateHost{}
	writeFile(t, filepath.Join(dir, projectFile), broken)
	out, errOut, code := deployIn(t, h, dir, "y\n", true)
	if code != exitError {
		t.Fatalf("a canyon.toml that will not parse exited %d:\n%s\n%s", code, out, errOut)
	}
	if body, _ := written(t, dir); body != broken {
		t.Errorf("the file that would not parse was replaced with %q", body)
	}
	if strings.Contains(out, "setting up this app") || len(h.asked) > 0 {
		t.Errorf("the setup block ran over a file that exists, and asked %q:\n%s", h.asked, out)
	}
	if !strings.Contains(errOut, "which Canyon does not read") {
		t.Errorf("the syntax error is not what the deploy said:\n%s", errOut)
	}
}

func TestTheNameTheFlagGivesIsTheNameThatIsWritten(t *testing.T) {
	dir := freshDir(t, "1-not-an-app-id") // a directory no app id can be inferred from
	out, errOut, code := deployIn(t, &gateHost{}, dir, "", false, "--app", "claims-portal")
	body, _ := written(t, dir)
	if body != "app = \"claims-portal\"\n" || !strings.Contains(out, "app: claims-portal (from --app)") || !strings.Contains(out, "✓ Wrote canyon.toml") {
		t.Errorf("--app wrote %q and printed (exit %d, stderr %s):\n%s", body, code, errOut, out)
	}
	_, errOut, code = deployIn(t, &gateHost{}, freshDir(t, "claims-portal"), "", false, "--app", "My_App")
	if code != exitError || !strings.Contains(errOut, "--app \"My_App\"") {
		t.Errorf("--app naming what no route takes exited %d:\n%s", code, errOut)
	}
}

func TestAFlagThatWouldChangeNothingIsRefused(t *testing.T) {
	dir := freshDir(t, "claims-portal")
	deployIn(t, &gateHost{}, dir, "", false) // the first deploy writes the file
	out, errOut, code := deployIn(t, &gateHost{}, dir, "", false, "--app", "another-app")
	if code != exitError || !strings.Contains(errOut, "already names the app \"claims-portal\"") {
		t.Errorf("--app beside a file that answers exited %d:\n%s\n%s", code, out, errOut)
	}
	if body, _ := written(t, dir); body != "app = \"claims-portal\"\n" {
		t.Errorf("the flag rewrote the file to %q", body)
	}
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	must(t, err)
	return info
}

// The prompt lives or dies on this one comparison, so both directions are held: a stub that answers
// false leaves every real user unasked, and one that answers true asks a machine.
func TestTheNullDeviceIsTheCharacterDeviceThatIsNoTerminal(t *testing.T) {
	discard, zero := statOf(t, os.DevNull), statOf(t, "/dev/zero") // a regular file is render_test.go's
	if isTerminal(discard, discard) {
		t.Error("the null device read as a terminal: the prompt would be asked where nothing can answer it")
	}
	if !isTerminal(zero, discard) {
		t.Error("a character device that is not the null device read as no terminal: the prompt would never be asked")
	}
	f, err := os.Open(os.DevNull) // how a harness hands a verb its stdin
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if IsTTY(f) {
		t.Error("IsTTY read the null device as a terminal")
	}
}

// The same both ways through IsTTY itself: a build whose terminal check always answers no would
// take the prompt away from every person and leave the suite green.
func TestACharacterDeviceThatIsNotTheNullDeviceReadsAsATerminal(t *testing.T) {
	f, err := os.Open("/dev/zero")
	if err != nil {
		t.Skip("no /dev/zero on this machine")
	}
	t.Cleanup(func() { _ = f.Close() })
	if !IsTTY(f) {
		t.Error("IsTTY read a character device as no terminal: nobody would ever be asked")
	}
}

// A canyon.toml that is a link is a file that exists: following a dangling one writes outside the
// directory the verb was run in, with nothing on screen saying where it went.
func TestALinkIsAnsweredAndNeverWrittenThrough(t *testing.T) {
	dir, outside := freshDir(t, "claims-portal"), filepath.Join(t.TempDir(), "elsewhere.toml")
	must(t, os.Symlink(outside, filepath.Join(dir, projectFile)))
	h := &gateHost{}
	out, errOut, code := deployIn(t, h, dir, "y\n", true)
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("the deploy wrote through the link to %s", outside)
	}
	if code != exitError || strings.Contains(out, "setting up this app") || len(h.asked) > 0 ||
		!strings.Contains(errOut, projectFile+"\" is a link to a file that is not there") {
		t.Errorf("a dangling link exited %d and asked %q:\n%s\n%s", code, h.asked, out, errOut)
	}
}

// What the filesystem refuses is Canyon's own line with the path quoted, not Go's `open …:`.
func TestWhatTheFilesystemRefusesIsACanyonLine(t *testing.T) {
	held := freshDir(t, "claims-portal")
	must(t, os.Mkdir(filepath.Join(held, projectFile), 0o700)) // a canyon.toml that is a directory
	_, errOut, code := deployIn(t, &gateHost{}, held, "y\n", true)
	if code != exitError || strings.Contains(errOut, "open ") || !strings.Contains(errOut, "cannot be read: is a directory") {
		t.Errorf("a canyon.toml that is a directory exited %d and said:\n%s", code, errOut)
	}
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a mode")
	}
	closed := freshDir(t, "claims-portal")
	must(t, os.Chmod(closed, 0o500))                  //nolint:gosec // a directory mode: one this user cannot write in
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) }) //nolint:gosec // and back, so the temp directory can be removed
	h := &gateHost{}
	_, errOut, code = deployIn(t, h, closed, "y\n", true)
	if code != exitError || strings.Contains(errOut, "open ") || strings.Contains(errOut, "CreateTemp") || !strings.Contains(errOut, projectFile) ||
		!strings.Contains(errOut, "cannot be written: permission denied") {
		t.Errorf("a directory that will not take the file exited %d and said:\n%s", code, errOut)
	}
	if len(h.created) > 0 || len(closedEntries(t, closed)) != 1 { // no app this directory could not record, and nothing left beside index.js
		t.Errorf("the host was asked to make %q, and the directory holds %q", h.created, closedEntries(t, closed))
	}
}

func closedEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	var named []string
	for _, e := range entries {
		named = append(named, e.Name())
	}
	return named
}

// An app = "" is a wrong line, not a missing one: refused, and the file left as it is.
func TestAnEmptyAppLineIsRefusedAndNeverAddedTo(t *testing.T) {
	dir, h := freshDir(t, "claims-app"), picked()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"\"\n")
	_, errOut, code := deployIn(t, h, dir, "", false)
	if body, _ := written(t, dir); code != exitError || body != "app = \"\"\n" || len(h.created) > 0 || !strings.Contains(errOut, "is not a Canyon app id") {
		t.Errorf("exit %d, sent %q, left %q:\n%s", code, h.created, body, errOut)
	}
}
