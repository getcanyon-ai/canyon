package cli

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// logsHost answers GET /api/v1/projects/:id/logs the way routes.md's row does: a 200 that writes
// each line and flushes it, held open until the client goes when hold is set; else the envelope.
type logsHost struct {
	status    int // 0 answers 200
	body      string
	lines     []string
	hold      bool
	redaction string
	noHeader  bool   // no Canyon-Log-Redaction at all
	typed     string // a Content-Type other than text/plain on the 200: not a Canyon host
	asked     []string
	accepts   []string
	project   string        // the app's own read; "" one with nothing deployed
	egress    string        // the app's egress read; "" one with no egress request
	unread    int           // the egress read answers this status, a 500's envelope
	reading   chan struct{} // set, the egress read says it arrived here, then answers nothing until the client goes
	via       string        // the host's via (GET /api/v1/egress); "" canyon
	kube      bool          // the front door's runtime is kubernetes
	read      []string      // the reads beside the log's, in the order asked
	now       string        // the app's status read, asked only to name an Allow; "" one that says nothing of its envs
	asking    chan struct{} // set, the status read says it arrived here, then answers nothing until the client goes
}

func (h *logsHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch path := r.URL.Path; {
	case path == "/api/v1/egress": // the host's via, which every member reads
		h.read = append(h.read, path)
		_, _ = io.WriteString(w, `{"via":"`+cmp.Or(h.via, "canyon")+`","mode":"observe"}`)
		return
	case path == "/api/v1/front-door": // the host's runtime
		h.read = append(h.read, path)
		runtime := "docker"
		if h.kube {
			runtime = "kubernetes"
		}
		_, _ = io.WriteString(w, `{"runtime":"`+runtime+`","warning":""}`)
		return
	case strings.HasSuffix(path, "/egress"): // what Canyon refused the app's envs (contracts/routes.md, Egress control)
		h.read = append(h.read, path)
		if h.reading != nil {
			h.reading <- struct{}{}
			<-r.Context().Done()
			return
		}
		if h.unread != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(h.unread)
			_, _ = io.WriteString(w, `{"error":"internal","message":"internal error"}`)
			return
		}
		_, _ = io.WriteString(w, cmp.Or(h.egress, `{"envs":{"preview":{"requests":[]},"prod":{"requests":[]}}}`))
		return
	case strings.HasSuffix(path, "/status"): // whether each env runs, which only the status read says (#2722)
		h.read = append(h.read, path)
		if h.asking != nil {
			h.asking <- struct{}{}
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, cmp.Or(h.now, `{"envs":{}}`))
		return
	case !strings.HasSuffix(path, "/logs"): // the app's own read: after a 404, this host has the app; after the lines, what each env runs
		h.read = append(h.read, path)
		_, _ = io.WriteString(w, cmp.Or(h.project, `{"project":{"id":"claims-portal"}}`))
		return
	}
	h.asked = append(h.asked, r.Method+" "+r.URL.RequestURI())
	h.accepts = append(h.accepts, r.Header.Get("Accept"))
	if h.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		_, _ = io.WriteString(w, h.body)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if h.typed != "" {
		w.Header().Set("Content-Type", h.typed)
	}
	w.Header().Set("Canyon-Log-Redaction", "none")
	if h.redaction != "" {
		w.Header().Set("Canyon-Log-Redaction", h.redaction)
	}
	if h.noHeader {
		w.Header().Del("Canyon-Log-Redaction")
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for _, line := range h.lines {
		_, _ = io.WriteString(w, line)
		_ = rc.Flush()
	}
	if h.hold {
		<-r.Context().Done()
	}
}

func serveLogs(t *testing.T, h *logsHost) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	inProject(t, "claims-portal")
	return server
}

func logsRun(t *testing.T, h *logsHost, args ...string) (out, errs string, code int) {
	t.Helper()
	return logsRunOn(t, h, false, args...)
}

// logsRunOn is logsRun with stdout a terminal or not, which is what decides whether the verb follows.
func logsRunOn(t *testing.T, h *logsHost, tty bool, args ...string) (out, errs string, code int) {
	t.Helper()
	server := serveLogs(t, h)
	return run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, tty, append([]string{"logs"}, args...)...)
}

func TestLogsRendersAStreamThatEndsByteForByte(t *testing.T) {
	want := golden(t, "logs-preview.txt")            // before the run moves into the project's directory
	for _, format := range []string{"human", "ai"} { // no ai block exists for logs, so ai is the human rendering
		out, errs, code := logsRun(t, &logsHost{lines: []string{"ready on :8080\n", "GET / 200\n"}}, "--format", format)
		if code != exitOK || errs != "" {
			t.Errorf("--format %s: exit %d, stderr %q", format, code, errs)
		}
		if out != want {
			t.Errorf("--format %s rendered\n%s\ntestdata/logs-preview.txt wants\n%s", format, out, want)
		}
	}
}

// A read that ends with no line of the app's says so under the heading, never the heading alone (#2890): a
// keepalive is the stream's, not the app's, so a stream of keepalives alone is none either.
func TestLogsWithNoLinesSayNoneYet(t *testing.T) {
	want := golden(t, "logs-empty.txt")
	for _, lines := range [][]string{nil, {keepalive, keepalive}} {
		out, errs, code := logsRun(t, &logsHost{lines: lines})
		if code != exitOK || errs != "" || out != want {
			t.Errorf("%q: exit %d, stderr %q, rendered\n%s\ntestdata/logs-empty.txt wants\n%s", lines, code, errs, out, want)
		}
	}
}

// It follows on a person's terminal and nowhere else, unless --follow (or -f) says so (cli.md
// §6.4): an agent or a pipe that waited on a stream that never ends hung until it was killed (QA
// batch 2026-10-07). The heading says which it is.
func TestLogsFollowsOnAPersonsTerminalAndPrintsTheLastLinesElsewhere(t *testing.T) {
	for _, c := range []struct {
		tty     bool
		args    []string
		follow  string
		heading string
	}{
		{true, nil, "true", "live · Ctrl-C to stop"},
		{true, []string{"--format", "ai"}, "false", "last 200 lines"},
		{false, nil, "false", "last 200 lines"},
		{false, []string{"--follow"}, "true", "live · Ctrl-C to stop"},
		{false, []string{"-f"}, "true", "live · Ctrl-C to stop"},
		{true, []string{"--format", "ai", "--follow"}, "true", "live · Ctrl-C to stop"},
		{false, []string{"--lines", "30"}, "false", "last 30 lines"},
	} {
		h := &logsHost{lines: []string{"one\n"}}
		out, errs, code := logsRunOn(t, h, c.tty, c.args...)
		lines := "200"
		if len(c.args) == 2 && c.args[0] == "--lines" {
			lines = c.args[1]
		}
		want := "GET /api/v1/projects/claims-portal/logs?env=preview&follow=" + c.follow + "&lines=" + lines
		if code != exitOK || errs != "" || len(h.asked) != 1 || h.asked[0] != want {
			t.Errorf("tty %v %q: exit %d, stderr %q, asked %q, want %q", c.tty, c.args, code, errs, h.asked, want)
		}
		if !strings.HasPrefix(out, "\n  claims-portal · preview · "+c.heading+"\n\n") {
			t.Errorf("tty %v %q: the heading is not %q:\n%s", c.tty, c.args, c.heading, out)
		}
	}
}

func TestLinesOutsideOneToTenThousandIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, n := range []string{"0", "10001", "-3", "many"} {
		h := &logsHost{}
		out, errs, code := logsRun(t, h, "--lines", n)
		if code != exitError || out != "" || !strings.Contains(errs, "-lines") {
			t.Errorf("--lines %s: exit %d, stdout %q, stderr %q", n, code, out, errs)
		}
		if len(h.asked) > 0 {
			t.Errorf("--lines %s reached the host: %q", n, h.asked)
		}
	}
}

func TestLogsAsksForTheEnvAndNothingElse(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "GET /api/v1/projects/claims-portal/logs?env=preview&follow=false&lines=200"},
		{[]string{"--env", "prod"}, "GET /api/v1/projects/claims-portal/logs?env=prod&follow=false&lines=200"},
		{[]string{"--app", "hr-onboarding"}, "GET /api/v1/projects/hr-onboarding/logs?env=preview&follow=false&lines=200"},
	} {
		h := &logsHost{}
		if _, errs, code := logsRun(t, h, c.args...); code != exitOK {
			t.Errorf("%q: exit %d, stderr %q", c.args, code, errs)
		}
		if len(h.asked) != 1 || h.asked[0] != c.want {
			t.Errorf("%q asked %q, want %q", c.args, h.asked, c.want)
		}
	}
}

func TestAnEnvThatIsNotOneOfTheTwoIsRefusedBeforeAnyRequest(t *testing.T) {
	h := &logsHost{}
	out, errs, code := logsRun(t, h, "--env", "stage")
	if code != exitError || out != "" || !strings.Contains(errs, `--env is preview or prod, and got "stage"`) {
		t.Errorf("--env stage: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if len(h.asked) > 0 {
		t.Errorf("--env stage reached the host: %q", h.asked)
	}
}

// writes hands every write to a channel, so a test reads stdout while the verb is still running.
type writes chan string

func (w writes) Write(p []byte) (int, error) { w <- string(p); return len(p), nil }

// logsApp is the app Main builds, pointed at host, so a test can drive runLogs with its own context.
func logsApp(t *testing.T, out io.Writer, host string) *app {
	t.Helper()
	env := map[string]string{"CANYON_HOST": host, "XDG_CONFIG_HOME": t.TempDir()}
	a := &app{out: out, err: &strings.Builder{}, in: strings.NewReader(""), getenv: func(k string) string { return env[k] }, clock: wallClock}
	a.render, _ = resolveRender("", a.getenv, false)
	for _, c := range commands {
		if c.name == "logs" {
			a.verb = c
		}
	}
	return a
}

// A line reaches stdout while the stream is still open, and ctrl-c (the context ending) ends the
// verb with exit 0 and nothing more printed.
func TestALineIsPrintedAsItArrivesAndCtrlCEndsTheStream(t *testing.T) {
	server := serveLogs(t, &logsHost{lines: []string{"ready on :8080\n"}, hold: true})
	out := make(writes, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() { code <- runLogs(ctx, logsApp(t, out, server.URL), []string{"--follow"}) }()
	var seen strings.Builder
	for !strings.HasSuffix(seen.String(), "ready on :8080\n") {
		select {
		case w := <-out:
			seen.WriteString(w)
		case <-time.After(5 * time.Second):
			t.Fatalf("no line arrived within 5s while the stream was open; stdout so far %q", seen.String())
		}
	}
	cancel()
	select {
	case got := <-code:
		if got != exitOK {
			t.Errorf("ctrl-c ended the stream with exit %d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the verb did not return within 5s of the context ending")
	}
	for len(out) > 0 {
		seen.WriteString(<-out)
	}
	if want := "\n  claims-portal · preview · live · Ctrl-C to stop\n\n  ready on :8080\n"; seen.String() != want {
		t.Errorf("stdout was\n%q\nwant\n%q", seen.String(), want)
	}
}

// deadlineSpy records whether the log's request carries a deadline: a stream must not. The reads beside it are
// bounded like every other call.
type deadlineSpy struct{ deadline *bool }

func (s deadlineSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/logs") {
		_, has := r.Context().Deadline()
		*s.deadline = has
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestTheStreamCarriesNoDeadline(t *testing.T) {
	server := serveLogs(t, &logsHost{lines: []string{"one\n"}})
	has := true
	a := logsApp(t, io.Discard, server.URL)
	a.http = newClient(server.URL)
	a.http.HTTP.Transport = deadlineSpy{&has}
	if code := runLogs(context.Background(), a, nil); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if has {
		t.Errorf("the logs request carries a deadline, and a stream held open past %s would end on it", requestTimeout)
	}
}

func TestTheAppsControlBytesNeverReachTheTerminalRaw(t *testing.T) {
	out, _, code := logsRun(t, &logsHost{lines: []string{"\x1b[31mred\x1b[0m done\rback\n"}})
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.ContainsAny(out, "\x1b\r") {
		t.Errorf("a raw ESC or CR reached stdout: %q", out)
	}
	if !strings.Contains(out, `  \x1b[31mred\x1b[0m done\x0dback`) {
		t.Errorf("the line was not spelled out: %q", out)
	}
}

func TestALineLongerThanOnePieceIsPrintedWhole(t *testing.T) {
	long := strings.Repeat("x", logsPiece+7)
	out, _, code := logsRun(t, &logsHost{lines: []string{long + "\n", "after\n"}})
	if code != exitOK || !strings.Contains(out, "\n  "+long+"\n  after\n") {
		t.Errorf("exit %d, and a long line rendered as %d bytes: %q…", code, len(out), out[:min(len(out), 80)])
	}
}

func TestTheHostsRefusalsAreCanyonLines(t *testing.T) {
	for _, c := range []struct {
		status     int
		body, want string
	}{
		{http.StatusNotFound, `{"error":"not_found","message":"nothing deployed in prod"}`, "\n  ✗ Nothing is deployed in prod for claims-portal, or there is no such app.\n"},
		{http.StatusBadGateway, `{"error":"app_unreachable","message":"the app's container is gone"}`, "\n  ✗ claims-portal isn't running in production any more. Deploy it again, or take production offline on its page.\n"},
		{http.StatusInternalServerError, `{"error":"internal","message":"internal error"}`, "\n  ✗ internal error\n"},
		{http.StatusBadRequest, `{"error":"invalid_env","message":"env is preview or prod"}`, "\n  ✗ env is preview or prod\n"},
	} {
		out, errs, code := logsRun(t, &logsHost{status: c.status, body: c.body}, "--env", "prod")
		if code != exitError || out != "" || errs != c.want {
			t.Errorf("%d: exit %d, stdout %q, stderr %q, want %q", c.status, code, out, errs, c.want)
		}
	}
}

func TestLogsHasNoJSONRendering(t *testing.T) {
	h := &logsHost{}
	out, errs, code := logsRun(t, h, "--format", "json")
	if code != exitError || out != "" || !strings.Contains(errs, "logs has no json rendering") || len(h.asked) > 0 {
		t.Errorf("--format json: exit %d, stdout %q, stderr %q, asked %q", code, out, errs, h.asked)
	}
}

func TestARedactionOtherThanNoneIsSaidOnce(t *testing.T) {
	out, _, code := logsRun(t, &logsHost{redaction: "shipper", lines: []string{"one\n"}})
	if code != exitOK || !strings.Contains(out, "\n  the host reports redaction shipper\n\n  one\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if out, _, _ := logsRun(t, &logsHost{lines: []string{"one\n"}}); strings.Contains(out, "redaction") {
		t.Errorf("none was reported:\n%s", out)
	}
}

// A 200 that is not text/plain is a captive portal or a proxy, not an app's log: refused as a
// non-Canyon host, nothing streamed.
func TestATwoHundredThatIsNotTextIsNotACanyonHost(t *testing.T) {
	out, errs, code := logsRun(t, &logsHost{typed: "text/html; charset=utf-8", lines: []string{"<html>welcome to the lounge wifi</html>\n"}})
	if code != exitError || strings.Contains(out, "lounge") || !strings.Contains(errs, "is not Canyon") {
		t.Errorf("a text/html 200: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// An absent redaction header reads as none: nothing is said. A CR prints as \x0d whether it
// ends the line or stands alone, since every byte but the newline is the app's.
func TestAnAbsentRedactionHeaderSaysNothingAndACRIsSpelledOut(t *testing.T) {
	out, _, code := logsRun(t, &logsHost{noHeader: true, lines: []string{"win\r\n", "a\rb\n"}})
	if code != exitOK || strings.Contains(out, "redaction") || !strings.HasSuffix(out, "\n  win\\x0d\n  a\\x0db\n") {
		t.Errorf("exit %d:\n%q", code, out)
	}
}

// The accepted type is the type itself, not a prefix: a host answering text/plainish is not Canyon.
func TestAContentTypeThatMerelyStartsWithTheAcceptedOneIsNotCanyon(t *testing.T) {
	out, errs, code := logsRun(t, &logsHost{typed: "text/plainish", lines: []string{"one\n"}})
	if code != exitError || out != "" || !strings.Contains(errs, "is not Canyon") {
		t.Errorf("text/plainish: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// The server's keepalive through a quiet stretch (contracts/routes.md, the logs row) is not the
// app's: the CLI drops the whole line, and only the whole line — the app's own text that merely
// contains it, or a piece of a longer line, prints as written.
func TestTheServersKeepaliveIsNotPrinted(t *testing.T) {
	out, errs, code := logsRun(t, &logsHost{lines: []string{"ready on :8080\n", "canyon: keepalive\n", "canyon: keepalive\n", "app says canyon: keepalive\n", "GET / 200\n"}})
	if code != exitOK || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if strings.Contains(out, "\n  canyon: keepalive\n") || !strings.HasSuffix(out, "\n  ready on :8080\n  app says canyon: keepalive\n  GET / 200\n") {
		t.Errorf("rendered %q", out)
	}
}

// refusedHost is the first agent journey's preview (#2162, #2708): 07a88a went live at 22:43:53 and calls OpenAI,
// which Canyon refused, while Node's fetch logged only "Request was cancelled". Canyon also refused a destination a
// person dismissed, and one last refused before 07a88a went live; production refused another. Both envs run through
// Canyon's gateway on a network it floors, as status reads them.
func refusedHost(lines ...string) *logsHost {
	return &logsHost{
		lines:   lines,
		project: `{"project":{"id":"claims-portal","envs":{"preview":{"package":"pkg_07a88a","deployed_at":"2026-11-07T22:43:53Z"},"prod":{"package":"pkg_5c01de","label":"v1.3","deployed_at":"2026-11-01T09:00:00Z"}}}}`,
		egress: `{"floor":"enforced","envs":{"preview":{"via":"canyon","mode":"allowlist","requests":[` +
			`{"destination":"api.openai.com","state":"pending","first_refused_at":"2026-11-07T22:49:34Z","last_refused_at":"2026-11-07T22:50:03Z","refusals":2},` +
			`{"destination":"api.anthropic.com","state":"pending","first_refused_at":"2026-11-07T22:30:00Z","last_refused_at":"2026-11-07T22:40:00Z","refusals":5},` +
			`{"destination":"hooks.slack.com","state":"dismissed","first_refused_at":"2026-11-07T22:44:10Z","last_refused_at":"2026-11-07T22:44:10Z","refusals":1}]},` +
			`"prod":{"via":"canyon","mode":"allowlist","requests":[{"destination":"api.stripe.com","state":"pending","first_refused_at":"2026-11-02T09:30:00Z","last_refused_at":"2026-11-02T10:00:00Z","refusals":3}]}}}`,
	}
}

const cancelled = "summary refresh failed: fetch failed Request was cancelled.\n"

// asOf is when the tests below read Canyon's records, which the refusals' last line names.
var asOf = time.Date(2026, 11, 7, 22, 51, 0, 0, time.UTC)

// logsAt is canyon logs on h with its clock at asOf; tty is whether stdout is a person's terminal.
func logsAt(t *testing.T, h *logsHost, tty bool, args ...string) (out, errs string, code int) {
	t.Helper()
	server := serveLogs(t, h)
	var o, e strings.Builder
	a := logsApp(t, &o, server.URL)
	a.err, a.tty, a.clock = &e, tty, clock{now: func() time.Time { return asOf }, sleep: func(time.Duration) {}}
	code = runLogs(context.Background(), a, args)
	return o.String(), e.String(), code
}

// refusedFooter is the refusals' last line, read at asOf, of an env whose version is named version.
func refusedFooter(version string) string {
	return "  canyon: As of 2026-11-07T22:51:00Z, Canyon has refused calls to these destinations since " + version +
		" went live. The app may log them only as failed or cancelled. Canyon also refuses any call out to the internet to an IP address or a port other than 80 or 443, which isn't listed here. Run canyon status for what to do.\n"
}

// After the app's own lines, each destination Canyon refused the env since its version went live, a line each, then
// where to read what to do: the cause beside the symptom, which the app's client may never name. The golden is
// §6.4.1's transcript, and an agent reads the same lines a person does. The reads beside the log are status's.
func TestLogsShowCanyonsRefusalsAfterTheAppsLines(t *testing.T) {
	want := golden(t, "logs-refused.txt") // before the run moves into the project's directory
	if spec := strings.TrimPrefix(specFence(t, "#### 6.4.1 What Canyon refused"), "$ canyon logs --format ai\n"); spec != want {
		t.Errorf("testdata/logs-refused.txt is not §6.4.1's transcript:\n%s", spec)
	}
	for _, format := range []string{"human", "ai"} {
		h := refusedHost("ready on :8080\n", cancelled, cancelled)
		out, errs, code := logsAt(t, h, false, "--format", format)
		if code != exitOK || errs != "" {
			t.Errorf("--format %s: exit %d, stderr %q", format, code, errs)
		}
		if out != want {
			t.Errorf("--format %s rendered\n%s\ntestdata/logs-refused.txt wants\n%s", format, out, want)
		}
		reads := []string{"/api/v1/projects/claims-portal", "/api/v1/projects/claims-portal/egress", "/api/v1/egress", "/api/v1/front-door"}
		if strings.Join(h.read, " ") != strings.Join(reads, " ") {
			t.Errorf("--format %s read %q beside the log, want %q", format, h.read, reads)
		}
	}
}

// A request's count is every refusal since Canyon first refused the destination, so the version's window names the
// destinations and never the calls: one first refused before the version went live and last refused after it is
// listed, its calls counted in all.
func TestLogsShowCanyonsRefusalsOfDestinationsNotCalls(t *testing.T) {
	h := refusedHost(cancelled)
	h.egress = `{"floor":"enforced","envs":{"preview":{"via":"canyon","mode":"allowlist","requests":[` +
		`{"destination":"api.anthropic.com","state":"pending","first_refused_at":"2026-11-07T22:30:00Z","last_refused_at":"2026-11-07T22:45:00Z","refusals":5}]},"prod":{"requests":[]}}}`
	out, errs, code := logsAt(t, h, false)
	want := "\n  " + cancelled + "\n  canyon: refused api.anthropic.com · not allowed 5 times · last 2026-11-07T22:45:00Z\n" + refusedFooter("07a88a")
	if code != exitOK || errs != "" || !strings.HasSuffix(out, want) {
		t.Errorf("exit %d, stderr %q, rendered\n%s\nwant it to end\n%s", code, errs, out, want)
	}
}

// Each env's own records: production's log names what Canyon refused production since its version went live.
func TestLogsShowCanyonsRefusalsOfTheEnvItReads(t *testing.T) {
	out, errs, code := logsAt(t, refusedHost("GET / 200\n"), false, "--env", "prod")
	want := "\n  GET / 200\n\n  canyon: refused api.stripe.com · not allowed 3 times · last 2026-11-02T10:00:00Z\n" + refusedFooter("v1.3")
	if code != exitOK || errs != "" || !strings.HasSuffix(out, want) || strings.Contains(out, "openai") {
		t.Errorf("exit %d, stderr %q, rendered\n%s\nwant it to end\n%s", code, errs, out, want)
	}
}

// A followed stream never ends on its own, so the refusals come first, under the heading, and the live lines after.
func TestLogsShowCanyonsRefusalsBeforeTheLinesTheyFollow(t *testing.T) {
	out, errs, code := logsAt(t, refusedHost(cancelled), true)
	want := "\n  claims-portal · preview · live · Ctrl-C to stop\n\n" +
		"  canyon: refused api.openai.com · not allowed 2 times · last 2026-11-07T22:50:03Z\n" +
		"  canyon: refused hooks.slack.com · not allowed 1 time · last 2026-11-07T22:44:10Z · dismissed\n" +
		refusedFooter("07a88a") + "\n  " + cancelled
	if code != exitOK || errs != "" || out != want {
		t.Errorf("exit %d, stderr %q, rendered\n%s\nwant\n%s", code, errs, out, want)
	}
}

// The log names refusals only where canyon status lists them (design/EGRESS.md §8.1, §8.2), so it never sends an
// agent to a status that says nothing of them: nothing while nothing of the app goes through Canyon, the host's via
// off and its network open, nor for an env whose running version isn't through Canyon, nor for one Canyon can't say
// is floored.
func TestLogsShowCanyonsRefusalsOnlyWhereStatusDoes(t *testing.T) {
	const app = "\n  claims-portal · preview · last 200 lines\n\n  " + cancelled
	off := refusedHost(cancelled)
	off.via, off.egress = "off", strings.Replace(off.egress, `"floor":"enforced"`, `"floor":null`, 1)
	before := refusedHost(cancelled)
	before.egress = strings.Replace(before.egress, `"preview":{"via":"canyon"`, `"preview":{"via":"off"`, 1)
	open := refusedHost(cancelled)
	open.egress = strings.Replace(open.egress, `"floor":"enforced"`, `"floor":"not_enforced"`, 1)
	kube := refusedHost(cancelled)
	kube.kube, kube.egress = true, strings.Replace(kube.egress, `"floor":"enforced"`, `"floor":"unknown"`, 1)
	for name, h := range map[string]*logsHost{
		"via off on an open network":                  off,
		"the version running from before via canyon":  before,
		"a network not floored":                       open,
		"Kubernetes, a floor this build doesn't know": kube,
	} {
		out, errs, code := logsAt(t, h, false)
		if code != exitOK || errs != "" || out != app {
			t.Errorf("%s: exit %d, stderr %q, rendered\n%q\nwant\n%q", name, code, errs, out, app)
		}
	}
}

// Ctrl-C while the refusals are being read, before a followed stream's lines, prints nothing more either.
func TestLogsShowCanyonsRefusalsNotWhenCtrlCEndsTheirRead(t *testing.T) {
	h := refusedHost()
	h.hold, h.reading = true, make(chan struct{}, 1)
	server := serveLogs(t, h)
	out := make(writes, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := logsApp(t, out, server.URL)
	a.tty = true
	code := make(chan int, 1)
	go func() { code <- runLogs(ctx, a, nil) }()
	const heading = "\n  claims-portal · preview · live · Ctrl-C to stop\n\n"
	var seen strings.Builder
	deadline := time.After(5 * time.Second)
	for reading := h.reading; !strings.HasSuffix(seen.String(), heading) || reading != nil; {
		select {
		case w := <-out:
			seen.WriteString(w)
		case <-reading:
			reading = nil
		case <-deadline:
			t.Fatalf("the heading and the egress read did not arrive within 5s; stdout so far %q", seen.String())
		}
	}
	cancel()
	if got := <-code; got != exitOK {
		t.Errorf("ctrl-c ended the read with exit %d", got)
	}
	for len(out) > 0 {
		seen.WriteString(<-out)
	}
	if seen.String() != heading {
		t.Errorf("stdout was\n%q\nwant\n%q", seen.String(), heading)
	}
}

// The log and the egress read are both a contributor's and up (design/PERMISSIONS.md, Outbound): a caller the log
// refuses is never sent to the app's records, and reads its refusal alone.
func TestLogsShowCanyonsRefusalsOnlyToWhomTheLogIsShown(t *testing.T) {
	h := refusedHost()
	h.status, h.body = http.StatusForbidden, `{"error":"team_required","message":"you are not on this app's team"}`
	out, errs, code := logsRun(t, h)
	if code != exitError || out != "" || strings.Contains(errs, "refused api") || slices.ContainsFunc(h.read, func(p string) bool { return strings.HasSuffix(p, "/egress") }) {
		t.Errorf("exit %d, stdout %q, stderr %q, read %q", code, out, errs, h.read)
	}
}

// Ctrl-C prints nothing more (cli.md §6.4), the refusals included: a read sent after it would only say it failed.
func TestLogsShowCanyonsRefusalsNotAfterCtrlC(t *testing.T) {
	h := refusedHost("ready on :8080\n")
	h.hold = true
	server := serveLogs(t, h)
	out := make(writes, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() { code <- runLogs(ctx, logsApp(t, out, server.URL), nil) }()
	var seen strings.Builder
	for !strings.HasSuffix(seen.String(), "ready on :8080\n") {
		select {
		case w := <-out:
			seen.WriteString(w)
		case <-time.After(5 * time.Second):
			t.Fatalf("no line arrived within 5s; stdout so far %q", seen.String())
		}
	}
	cancel()
	if got := <-code; got != exitOK {
		t.Errorf("ctrl-c ended the read with exit %d", got)
	}
	for len(out) > 0 {
		seen.WriteString(<-out)
	}
	if want := "\n  claims-portal · preview · last 200 lines\n\n  ready on :8080\n"; seen.String() != want {
		t.Errorf("stdout was\n%q\nwant\n%q", seen.String(), want)
	}
}

// Nothing refused since the version went live, or no env in the app's read, adds nothing: the log reads as it did.
// A read that fails says so in one line and leaves the log and its exit alone.
func TestLogsShowCanyonsRefusalsOrSayTheyCouldNotBeRead(t *testing.T) {
	const app = "\n  claims-portal · preview · last 200 lines\n\n  " + cancelled
	before := refusedHost(cancelled)
	before.egress = `{"floor":"enforced","envs":{"preview":{"via":"canyon","mode":"allowlist","requests":[` +
		`{"destination":"api.anthropic.com","state":"pending","first_refused_at":"2026-11-07T22:30:00Z","last_refused_at":"2026-11-07T22:40:00Z","refusals":5}]},"prod":{"requests":[]}}}`
	undeployed := refusedHost(cancelled)
	undeployed.project = ""
	failed := refusedHost(cancelled)
	failed.unread = http.StatusInternalServerError
	for name, c := range map[string]struct {
		h    *logsHost
		want string
	}{
		"refused before the version went live": {before, app},
		"no env in the app's read":             {undeployed, app},
		"a read that fails":                    {failed, app + "\n  canyon: couldn't read what Canyon refused: internal error (500 internal)\n"},
	} {
		out, errs, code := logsAt(t, c.h, false)
		if code != exitOK || errs != "" || out != c.want {
			t.Errorf("%s: exit %d, stderr %q, rendered\n%q\nwant\n%q", name, code, errs, out, c.want)
		}
	}
}
