package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryVerbNamesItsHost is design/OPERATIONS.md §2.3's mitigation for the hole the guards leave: a
// checkout with no base, of an app both hosts have, so every verb that acts on the app says on its
// first line which Canyon it asked — the host as dialled, never the public URL the host reports.
func TestEveryVerbNamesItsHost(t *testing.T) {
	for _, c := range []struct {
		args []string
		then string // what the header says between the app and the host
	}{
		{[]string{"check"}, ""},
		{[]string{"deploy"}, " → preview"},
		{[]string{"deploy", "--env", "prod"}, " preview → prod"},
		{[]string{"deploy", "--env", "prod", "--version", "v1.3", "--yes"}, " v1.3 → prod"},
		{[]string{"secret", "list"}, ""},
		{[]string{"secret", "set", "STRIPE_SECRET_KEY"}, ""},
		{[]string{"secret", "rm", "STRIPE_SECRET_KEY"}, ""},
		{[]string{"undeploy", "--env", "preview"}, ""},
		{[]string{"delete", "--confirm", "claims-portal"}, ""},
		{[]string{"share"}, ""},
		{[]string{"share", "everyone"}, ""},
		{[]string{"rename", "Claims portal"}, ""},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			server := httptest.NewServer(&baseHost{})
			t.Cleanup(server.Close)
			xdg := t.TempDir() // the host it saved says its public URL is another name: the line names the one dialled
			must(t, (&config{Host: server.URL, Public: "https://canyon.meridian.example", Session: heldSession}).write(filepath.Join(xdg, "canyon", "config.json")))
			t.Chdir(projectDir(t))
			out, errOut, code := answer(t, word+"\n", map[string]string{"XDG_CONFIG_HOME": xdg, "NO_COLOR": "1"}, false, c.args...)
			want := "canyon dev · claims-portal" + c.then + " · " + strings.TrimPrefix(server.URL, "http://")
			if first, _, _ := strings.Cut(strings.TrimPrefix(out, "\n  "), "\n"); first != want {
				t.Errorf("exit %d, stderr %q; the first line is %q, want %q", code, errOut, first, want)
			}
		})
	}
}

// pastHeader is a verb's stdout after its first line, for the tests that hold it to what follows —
// a run's, or a transcript's in cli.md: TestEveryVerbNamesItsHost holds the line itself. A verb that
// stopped before it knew its host printed none.
func pastHeader(out string) string {
	line, rest, _ := strings.Cut(strings.TrimPrefix(out, "\n  "), "\n")
	if version, _, ok := strings.Cut(strings.TrimPrefix(line, ansiDim), " · "); ok && strings.HasPrefix(version, "canyon ") {
		return rest
	}
	return out
}

// The line as the spec's transcripts print it, rendered by the one function every verb calls: the
// host as dialled, scheme and default port dropped, an IPv6 literal in its brackets.
func TestTheFirstLineIsTheSpecsTranscript(t *testing.T) {
	for _, c := range []struct{ heading, typed, app, host string }{
		{"### 4.3 `canyon secret set`", "$ canyon secret set STRIPE_SECRET_KEY\n", "expense-tracker", "https://canyon.meridian.example"},
		{"### 6.7 `canyon share`", "$ canyon share\n", "claims-portal", "https://canyon.meridian.example:443"},
		{"", "", "claims-portal", "http://[::1]:7777"},
	} {
		base, err := validHost("the test", c.host)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		(&app{out: &b, http: newClient(base)}).header(c.app, "")
		want := "\n  canyon dev · " + c.app + " · " + strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://") + "\n"
		if c.heading != "" {
			first, _, _ := strings.Cut(strings.TrimPrefix(specFence(t, c.heading), c.typed), "\n  canyon 0.4.1 ")
			spec, _, _ := strings.Cut(strings.TrimPrefix(specFence(t, c.heading), c.typed+first), "\n\n")
			want = strings.Replace(spec, "canyon 0.4.1 ", "canyon dev ", 1) + "\n"
		}
		if b.String() != want {
			t.Errorf("%s: the first line is %q, want %q", c.host, b.String(), want)
		}
	}
}

// Under ai a verb whose result prints no block prints the first line, and any refusal stands alone:
// the line is dropped, whichever way the refusal is written (§3.1).
func TestUnderAIARefusalDropsTheFirstLine(t *testing.T) {
	for _, c := range []struct {
		args       []string
		status     int
		body, auth string
	}{
		{[]string{"undeploy", "--env", "preview"}, http.StatusConflict, `{"error":"run_in_progress","message":"a run is under way","run":"gr_9a1c"}`, "oidc"},
		{[]string{"delete", "--confirm", "claims-portal"}, http.StatusConflict, `{"error":"run_in_progress","message":"a run is under way","run":"gr_9a1c"}`, "oidc"},
		{[]string{"share"}, http.StatusOK, `{"entries":[],"requests":[]}`, "off"},
		{[]string{"secret", "rm", "STRIPE_SECRET_KEY"}, http.StatusNotFound, `{"error":"not_found","message":"no such secret"}`, "oidc"},
		{[]string{"secret", "rm", "STRIPE_SECRET_KEY"}, http.StatusNoContent, "", "oidc"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/auth/config" {
				_, _ = io.WriteString(w, `{"canyon":true,"auth":"`+c.auth+`","org":"Meridian","device":true,"public":""}`)
				return
			}
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		t.Cleanup(server.Close)
		t.Chdir(projectDir(t))
		out, errOut, code := run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "CANYON_FORMAT": "ai"}, false, c.args...)
		want := "" // a refusal: nothing on stdout
		if code == exitOK {
			want = "\n  canyon dev · claims-portal · " + strings.TrimPrefix(server.URL, "http://") + "\n\n"
		}
		if !strings.HasPrefix(out, want) || (want == "" && out != "") {
			t.Errorf("%q: exit %d, stderr %q, stdout %q, want it to open %q", c.args, code, errOut, out, want)
		}
	}
}
