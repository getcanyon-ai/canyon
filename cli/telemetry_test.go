package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// telemetryHost answers GET and PUT /api/v1/telemetry from the contract's golden report, enabled and off_because as
// set, and records each request's method and body.
func telemetryHost(t *testing.T, enabled bool, off string) (*httptest.Server, *[]string, string) {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/telemetry/report.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Report json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(golden.Report) // as the server marshals it: compact
	if err != nil {
		t.Fatal(err)
	}
	asked := &[]string{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*asked = append(*asked, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		state := map[string]any{"enabled": enabled, "report": string(compact), "last_sent_at": nil} // the send's bytes, as a string
		if r.Method == http.MethodPut {
			var set struct{ Enabled bool }
			_ = json.Unmarshal(body, &set)
			if set.Enabled && off == "dashboard" { // a host with sign-in, asked from the CLI
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"dashboard_required","message":"change it in Canyon's dashboard"}`)
				return
			}
			state["enabled"] = set.Enabled && off != "env"
		}
		if state["enabled"] == false {
			state["off_because"] = map[bool]string{true: "admin", false: off}[r.Method == http.MethodPut && off != "env"]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	}))
	t.Cleanup(h.Close)
	return h, asked, string(compact)
}

// show prints the next report byte for byte as the host answered it, which is the body its next send POSTs, under
// one line saying whether it goes and, when not, why. It only reads (#2807). A reason this build doesn't know, the
// not_seen of a host from before telemetry was on by default among them, prints as such (the owner, 2026-11-12).
func TestTelemetryShowPrintsTheReportAsSent(t *testing.T) {
	for _, c := range []struct {
		enabled bool
		off     string
		line    string
	}{
		{true, "", "Telemetry is on. Canyon sends this report once a day, exactly as it reads here:"},
		{false, "env", "Telemetry is off: this server's environment turns it off (CANYON_TELEMETRY=0, DO_NOT_TRACK=1 or CI). It would send:"},
		{false, "admin", "Telemetry is off: an admin turned it off. It would send:"},
		{false, "unreachable", "Telemetry is off: it couldn't reach Canyon's collector. An admin turns it back on in Canyon's dashboard: Settings › Telemetry. It would send:"},
		{false, "not_seen", "Telemetry is off (not_seen). It would send:"},
	} {
		h, asked, report := telemetryHost(t, c.enabled, c.off)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, "telemetry", "show")
		want := c.line + "\n" + report + "\n"
		if code != exitOK || errs != "" || out != want || strings.Join(*asked, ", ") != "GET /api/v1/telemetry" {
			t.Errorf("%s: exit %d, stderr %q, asked %q\n%s\nwant\n%s\n%s", c.off, code, errs, *asked, out, c.line, report)
		}
	}
	h, _, _ := telemetryHost(t, true, "")
	if out, _, _ := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "telemetry", "show"); out != golden(t, "telemetry-show.txt") {
		t.Errorf("canyon telemetry show printed\n%s\ntestdata/telemetry-show.txt wants\n%s", out, golden(t, "telemetry-show.txt"))
	}
}

// While no report has reached the collector, show says so, when the next try comes and when it gives up, in this
// machine's time; a host from before that says neither, and prints as on.
func TestTelemetryShowSaysItIsStillTrying(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/telemetry/report.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		State map[string]any `json:"state_retrying"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	golden.State["report"] = "{}"
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(golden.State)
	}))
	defer h.Close()
	was := time.Local
	time.Local = time.UTC
	defer func() { time.Local = was }()
	out, errs, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, "telemetry", "show")
	if want := "Telemetry is on, but no report has reached Canyon's collector yet. Canyon tries again after 24 Nov 21:04 and gives up by 21 Feb. It would send:\n{}\n"; code != exitOK || errs != "" || out != want {
		t.Errorf("exit %d, stderr %q\n%s\nwant\n%s", code, errs, out, want)
	}
}

// off and on are the PUT the contract names, each saying what it did; on under the server's environment's off says
// the environment still keeps it off, and on that a host with sign-in refuses from the CLI says where an admin turns
// it on, exit 2: policy's no.
func TestTelemetryOffAndOn(t *testing.T) {
	for _, c := range []struct {
		form, off, said, put string
		exit                 int
	}{
		{"off", "", "✓ Telemetry is off: Canyon sends nothing.\n", `PUT /api/v1/telemetry {"enabled":false}`, exitOK},
		{"on", "", "✓ Telemetry is on: Canyon sends one report a day.\n", `PUT /api/v1/telemetry {"enabled":true}`, exitOK},
		{"on", "env", "! Telemetry is off: this server's environment turns it off (CANYON_TELEMETRY=0, DO_NOT_TRACK=1 or CI).\n", `PUT /api/v1/telemetry {"enabled":true}`, exitOK},
		{"on", "dashboard", "! An admin turns telemetry on in Canyon's dashboard: Settings › Telemetry.\n", `PUT /api/v1/telemetry {"enabled":true}`, exitBlocked},
		{"off", "dashboard", "✓ Telemetry is off: Canyon sends nothing.\n", `PUT /api/v1/telemetry {"enabled":false}`, exitOK},
	} {
		h, asked, _ := telemetryHost(t, c.off == "", c.off)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, "telemetry", c.form)
		if code != c.exit || errs != "" || out != c.said || len(*asked) != 1 || (*asked)[0] != c.put {
			t.Errorf("%s %s: exit %d, stderr %q, asked %q\n%q", c.form, c.off, code, errs, *asked, out)
		}
	}
}
