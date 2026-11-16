package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// licenceHost answers GET /api/v1/licence with the contract's golden read named, and records what was asked.
func licenceHost(t *testing.T, read string) (*httptest.Server, *[]string) {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/licence/licence.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	asked := &[]string{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*asked = append(*asked, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden[read])
	}))
	t.Cleanup(h.Close)
	return h, asked
}

// canyon licence prints the licence as the host answers the reader, and only reads: an admin the state's line, the
// seats and the features by name; a member what is unlocked; no licence, the free edition (#2879).
func TestLicencePrintsTheStateTheHostAnswers(t *testing.T) {
	for _, c := range []struct{ read, want string }{
		{"admin", "Canyon Enterprise, licensed to Meridian Insurance until 14 Nov 2027.\n  Seats      50\n  Features   Credentials Canyon adds, Patching, Access by sign-in group\n"},
		{"lapsed", "Canyon Enterprise, licensed to Meridian Insurance, ended on 14 Nov 2027. What's set keeps working; Enterprise features can't be changed until an admin adds a new licence.\n  Seats      50\n  Features   Credentials Canyon adds, Patching, Access by sign-in group\n"},
		{"invalid", "Canyon didn't sign this server's licence. What's set keeps working; changing an Enterprise feature needs a licence Canyon signed.\n"},
		{"member", "Only an admin can see the licence. Unlocked on this Canyon: Credentials Canyon adds, Patching, Access by sign-in group.\n"},
		{"none", "No licence: this Canyon runs the free edition. Enterprise features need a licence.\n"},
	} {
		h, asked := licenceHost(t, c.read)
		out, errs, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, "licence")
		if code != exitOK || errs != "" || out != c.want || strings.Join(*asked, ", ") != "GET /api/v1/licence" {
			t.Errorf("%s: exit %d, stderr %q, asked %q\n%s\nwant\n%s", c.read, code, errs, *asked, out, c.want)
		}
	}
	h, _ := licenceHost(t, "admin")
	if out, _, _ := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "licence"); out != golden(t, "licence.txt") {
		t.Errorf("canyon licence printed\n%s\ntestdata/licence.txt wants\n%s", out, golden(t, "licence.txt"))
	}
	if out, _, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "licence", "--format", "json"); code != exitOK || !strings.Contains(out, `"customer":"Meridian Insurance"`) {
		t.Errorf("--format json: exit %d %s", code, out)
	}
	if _, errs, code := run(t, map[string]string{"CANYON_HOST": h.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "licence", "set"); code == exitOK || !strings.Contains(errs, `licence takes no arguments`) {
		t.Errorf("an argument: exit %d %s", code, errs)
	}
}
