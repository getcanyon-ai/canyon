package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jsonApp renders for CI with colour and the ASCII fallback both switched on, so the tests prove
// the object ignores them: in the binary resolveRender forces plain for every non-human format,
// and a test that relied on that would pass with a paint() call installed.
func jsonApp(out *strings.Builder, verb string) *app {
	return &app{out: out, render: Render{Format: FormatJSON, Color: true, ASCII: true}, verb: command{name: verb}}
}

// jsonTail is deployTail under --format json: the timeline rows are streamed, then the verdict.
func jsonTail(t *testing.T, verb string, run checkRun, page string) (string, int) {
	t.Helper()
	var b strings.Builder
	a := jsonApp(&b, verb)
	a.err = &b
	a.park() // where the verb parks it: after parse, before anything narrates
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

// oneObject is what §5.4 promises stdout holds: one object, one newline, nothing else.
func oneObject(t *testing.T, out string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout is not one object and one newline:\n%q", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout does not parse: %v\n%s", err, out)
	}
	return got
}

func TestJSONBlockedRendersItsGolden(t *testing.T) {
	var out strings.Builder
	a := jsonApp(&out, "check")
	if code := a.verdict(goldenRun(t)); code != exitBlocked {
		t.Errorf("the blocked verdict exited %d, want %d", code, exitBlocked)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(out.String()), "", "  "); err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "json-blocked.json"); pretty.String() != want {
		t.Errorf("canyon check --format json rendered\n%s\ntestdata/json-blocked.json wants\n%s", pretty.String(), want)
	}
}

func TestJSONIsTypedAndCarriesNoEscapeByte(t *testing.T) {
	var out strings.Builder
	a := jsonApp(&out, "check")
	if !a.render.Color || !a.render.ASCII { // paint() is a no-op without colour, so this proof proves nothing then
		t.Fatal("the json proof needs colour and the ASCII fallback switched on")
	}
	a.verdict(goldenRun(t))
	got := oneObject(t, out.String())
	first, ok := got["findings"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("findings[0] is %T", got["findings"].([]any)[0])
	}
	if _, isBool := first["blocks"].(bool); !isBool { // never "yes": CI reads the field, not the prose
		t.Errorf("blocks is %T (%v), want a bool", first["blocks"], first["blocks"])
	}
	if _, isNumber := first["line"].(float64); !isNumber {
		t.Errorf("line is %T (%v), want a number", first["line"], first["line"])
	}
	if _, isNumber := got["exit_code"].(float64); !isNumber {
		t.Errorf("exit_code is %T (%v), want a number", got["exit_code"], got["exit_code"])
	}
	for i := 0; i < len(out.String()); i++ {
		if b := out.String()[i]; b == 0x1b || (b < 0x20 && b != '\n') {
			t.Fatalf("byte %#x at %d: a control byte in what CI parses:\n%q", b, i, out.String())
		}
	}
	unpainted(t, "", got) // encoding/json would spell a painted value as \u001b, so the decoded strings are read too
}

func unpainted(t *testing.T, at string, v any) {
	t.Helper()
	switch v := v.(type) {
	case string:
		if strings.ContainsRune(v, 0x1b) {
			t.Errorf("%s carries an escape byte: %q", at, v)
		}
	case map[string]any:
		for k, inner := range v {
			unpainted(t, at+"."+k, inner)
		}
	case []any:
		for i, inner := range v {
			unpainted(t, at+"["+strconv.Itoa(i)+"]", inner)
		}
	}
}

func TestJSONDeploymentIsPresentOnlyWhenSomethingShipped(t *testing.T) {
	out, code := jsonTail(t, "deploy", goldenRun(t), buildPage(t, buildLines, true))
	got := oneObject(t, out)
	if code != exitOK || got["result"] != "blocked" || got["exit_code"] != float64(exitOK) {
		t.Errorf("a blocked run that deployed under approvals: exit %d, result %v, exit_code %v", code, got["result"], got["exit_code"])
	}
	if dep, ok := got["deployment"].(map[string]any); !ok || dep["url"] != liveURL || dep["env"] != "preview" || got["env"] != "preview" {
		t.Errorf("the deployment of a shipped run: %v (env %v)", got["deployment"], got["env"])
	}
	out, code = jsonTail(t, "deploy", goldenRun(t), buildPage(t, nil, false))
	got = oneObject(t, out)
	if _, present := got["deployment"]; present || code != exitBlocked || got["env"] != "preview" { // absent, never null
		t.Errorf("a blocked deploy that shipped nothing: exit %d, deployment %v, env %v", code, got["deployment"], got["env"])
	}
	out, code = jsonTail(t, "check", cleanGate(t), buildPage(t, nil, false))
	got = oneObject(t, out)
	_, deployment := got["deployment"]
	_, env := got["env"]
	if code != exitOK || got["result"] != "passed" || deployment || env || got["urls"].(map[string]any)["app"] != fleetURL {
		t.Errorf("a clean check: exit %d, result %v, deployment %v, env %v, urls %v", code, got["result"], deployment, env, got["urls"])
	}
}

func TestJSONFirstDeployPrintsOnlyTheObject(t *testing.T) {
	dir := freshDir(t, "claims-portal")
	h := &gateHost{project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page:  buildPage(t, buildLines, false), run: aRun("blocked", 1, 0, blocker)}
	out, errOut, code := deployEnv(t, h, dir, "", false, map[string]string{"CANYON_FORMAT": "json"})
	got := oneObject(t, out)
	if code != exitBlocked || errOut != "" || got["gate_run"] != "gr_8f2a" || got["started_at"] != "2026-09-02T09:31:04Z" {
		t.Errorf("exit %d, stderr %q, object %v", code, errOut, got)
	}
	if body, _ := written(t, dir); body != "app = \"claims-portal\"\n" { // the file is written, and nothing says so on stdout
		t.Errorf("the first deploy left canyon.toml as %q", body)
	}
}

func TestJSONIsStillRefusedWhereNothingRendersIt(t *testing.T) {
	if out, err, code := run(t, nil, false, "fleet", "--format", "json"); code != exitError || out != "" || !strings.Contains(err, "fleet has no json rendering") {
		t.Errorf("--format json on fleet: exit %d, stdout %q, stderr %q", code, out, err)
	}
}

// A deploy url is read by a script that never looks at it, so a host may not hand one a scheme a
// browser would run: not http(s) with a host is refused with nothing on stdout, like any bad answer.
func TestJSONRefusesADeployURLThatIsNotHTTP(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "claims-portal.preview", "https://"} {
		run := goldenRun(t)
		run.Result.State, run.Result.Blocking, run.Result.Warnings = "clean", 0, 0
		run.Result.Findings = nil
		run.Deploy = &deployed{Env: "preview", URL: bad}
		var out, errOut strings.Builder
		a := jsonApp(&out, "deploy")
		a.err = &errOut
		if code := a.verdict(run); code != exitError || out.Len() != 0 || !strings.Contains(errOut.String(), "not http(s)") {
			t.Errorf("deploy url %q: exit %d, stdout %q, stderr %q", bad, code, out.String(), errOut.String())
		}
	}
}

// canyon check under json: the check verb's own banner, the "paths not packed" line and the rows
// line are the prints only a check drives, and none of them may reach stdout beside the object.
func TestJSONCheckPrintsOnlyTheObject(t *testing.T) {
	dir := freshDir(t, "claims-portal")
	must(t, os.WriteFile(filepath.Join(dir, projectFile), []byte("app = \"claims-portal\"\n"), 0o600))
	must(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o700)) // one path the packager skips
	must(t, os.WriteFile(filepath.Join(dir, "node_modules", "x.js"), []byte("x"), 0o600))
	h := &gateHost{project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`},
		page:  `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"policy.waived","data":{}}],"next":null}`,
		run:   aRun("blocked", 1, 0, blocker)}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(dir)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_FORMAT": "json"}
	out, errOut, code := answer(t, "", env, false, "check")
	if code != exitBlocked || errOut != "" {
		t.Fatalf("exit %d, stderr:\n%s\nstdout:\n%s", code, errOut, out)
	}
	if !strings.HasPrefix(out, "{") || strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "}\n") {
		t.Errorf("stdout is not one object and one newline:\n%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["result"] != "blocked" || got["env"] != nil {
		t.Errorf("the check's object: %v, %v", err, got)
	}
}

// The three-way ordering in verdict(): a failed run still gets its object, result "failed" and
// exit_code 1, while a run the CLI refuses (an unknown state, a clean deploy that shipped
// nothing) prints no object at all, and stdout is handed back either way.
func TestJSONFailedRunsGetAnObjectAndRefusalsGetNone(t *testing.T) {
	failed := goldenRun(t)
	failed.Result.State, failed.Result.FailureReason, failed.Result.Findings, failed.Result.Blocking, failed.Result.Warnings = "failed", "model", nil, 0, 0
	var out, errOut strings.Builder
	a := jsonApp(&out, "check")
	a.err = &errOut
	if code := a.verdict(failed); code != exitError || !strings.Contains(out.String(), `"result":"failed"`) || !strings.Contains(out.String(), `"exit_code":1`) || !strings.Contains(out.String(), `"failure_reason":"model"`) {
		t.Errorf("a failed run under json: exit %d, stdout %q", code, out.String())
	}
	for name, mutate := range map[string]func(run *checkRun, a *app){
		"an unknown state": func(run *checkRun, a *app) { run.Result.State = "weird" },
		"a clean deploy that shipped nothing": func(run *checkRun, a *app) {
			run.Result.State, run.Result.Blocking, run.Result.Warnings, run.Result.Findings, a.verb.name = "clean", 0, 0, nil, "deploy"
		},
	} {
		run := goldenRun(t)
		var out, errOut strings.Builder
		a := jsonApp(&out, "check")
		a.err = &errOut
		mutate(&run, a)
		a.park()
		code := a.verdict(run)
		a.unpark(true)
		a.printf("after") // the verb's deferred unpark has handed stdout back
		if code != exitError || out.String() != "after" || errOut.Len() == 0 {
			t.Errorf("%s under json: exit %d, stdout %q, stderr %q", name, code, out.String(), errOut.String())
		}
	}
}
