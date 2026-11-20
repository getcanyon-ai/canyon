package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request canyon mcp makes says so (design/specs/mcp.md §7): an invocation whose transport is canyon mcp's sends
// Canyon-Transport: mcp-local on every call, and the CLI's own verbs send none, which the host counts as the CLI's.
func TestMCPTransport(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Canyon-Transport"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}
	for _, c := range []struct {
		transport, want string
	}{{transportMCPLocal, "mcp-local"}, {"", ""}} {
		got = nil
		a := &app{getenv: func(k string) string { return env[k] }, out: io.Discard, err: io.Discard, in: strings.NewReader(""), ctx: t.Context(), transport: c.transport}
		client, err := a.client()
		if err != nil {
			t.Fatal(err)
		}
		if err := client.get(t.Context(), "/api/v1/fleet", nil); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("transport %q sent Canyon-Transport %q, want %q", c.transport, got, c.want)
		}
	}

	// canyon check, the CLI itself: no Canyon-Transport on the review it starts.
	h := &gateHost{run: aRun("clean", 0, 0), project: reply{http.StatusCreated, `{"project":{"id":"claims-portal"}}`},
		start: reply{http.StatusAccepted, `{"run":{"id":"gr_8f2a","state":"pending"}}`}, page: `{"rows":[{"id":41,"ts":"2026-09-02T09:31:04Z","kind":"run.state","data":{}}],"next":null}`}
	gate := httptest.NewServer(h)
	defer gate.Close()
	t.Chdir(projectDir(t))
	run(t, map[string]string{"CANYON_HOST": gate.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "check")
	if h.started == "" || h.transport != "" {
		t.Errorf("canyon check started %q with Canyon-Transport %q, want a review and none", h.started, h.transport)
	}
}
