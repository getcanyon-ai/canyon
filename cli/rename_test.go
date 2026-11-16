package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// renameHost answers the rename route as the contract does, with the name it was sent, or refuse when
// set, recording each request.
type renameHost struct {
	refuse string // a 403 body answered to the rename
	older  bool   // a host older than the route: its catch-all answers the rename 404, and the app's read 200
	asked  []string
}

func (h *renameHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.asked = append(h.asked, strings.TrimSpace(r.Method+" "+r.URL.EscapedPath()+" "+string(body)))
	switch {
	case r.URL.Path == "/auth/config":
		_, _ = w.Write([]byte(`{"canyon":true,"auth":"oidc","org":"Meridian","device":true,"cli":"","public":""}`))
	case h.older && r.URL.Path == "/api/v1/projects/claims-portal":
		_, _ = w.Write([]byte(`{"project":{"id":"claims-portal","name":"claims-portal"}}`))
	case h.older:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"no such route"}`))
	case h.refuse != "":
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(h.refuse))
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/name"):
		app := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/projects/"), "/name")
		_, _ = fmt.Fprintf(w, `{"project":{"id":%q,"name":%s}}`, app, strings.TrimSuffix(strings.TrimPrefix(string(body), `{"name":`), "}")) //nolint:gosec // test: a fake host answers its own client the name it was sent
	default:
		http.NotFound(w, r)
	}
}

func renaming(t *testing.T, h *renameHost, dir string, args ...string) (out, errOut string, code int) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Chdir(dir)
	out, errOut, code = run(t, map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}, false, append([]string{"rename"}, args...)...)
	return pastHeader(out), errOut, code
}

// §6.9's transcript is the golden: an app --app names, renamed by one PUT of its new name.
func TestRenameIsTheSpecsTranscript(t *testing.T) {
	want := pastHeader(strings.TrimPrefix(specFence(t, "### 6.9 `canyon rename`"), "$ canyon rename \"Claims portal\"\n")) // read before the run leaves the package's directory
	h := &renameHost{}
	out, errOut, code := renaming(t, h, t.TempDir(), "--app", "funny-snake-4k2", "Claims portal")
	if code != exitOK || out != want {
		t.Errorf("exit %d, stderr %q; canyon rename printed\n%s\n§6.9 of design/specs/cli.md is\n%s", code, errOut, out, want)
	}
	if want := `PUT /api/v1/projects/funny-snake-4k2/name {"name":"Claims portal"}`; !strings.Contains(strings.Join(h.asked, "\n"), want) {
		t.Errorf("asked %q, want %s", h.asked, want)
	}
}

// The app is canyon.toml's where --app names none; anyone but its owners and admins gets the server's
// owner_required line, exit 1; a name is one argument, and none or two is a usage error that asks nothing.
func TestRenameRefusalsAndUsage(t *testing.T) {
	if out, _, code := renaming(t, &renameHost{}, projectDir(t), "Motor claims"); code != exitOK || !strings.Contains(out, "claims-portal is now named Motor claims.") {
		t.Errorf("canyon.toml's app: exit %d\n%s", code, out)
	}
	owner := `{"error":"owner_required","message":"only the owners of claims-portal and admins can do this: ask olive@meridian.example"}`
	if _, errOut, code := renaming(t, &renameHost{refuse: owner}, projectDir(t), "Motor claims"); code != exitError || !strings.Contains(errOut, "only the owners of claims-portal and admins can do this: ask olive@meridian.example") {
		t.Errorf("owner_required: exit %d, stderr %q", code, errOut)
	}
	if _, errOut, code := renaming(t, &renameHost{older: true}, projectDir(t), "Motor claims"); code != exitError || strings.Contains(errOut, "isn't on") || !strings.Contains(errOut, "no such route") {
		t.Errorf("a host older than the route, which has the app: exit %d, stderr %q", code, errOut)
	}
	for _, args := range [][]string{{}, {"Motor", "claims"}} {
		h := &renameHost{}
		if _, errOut, code := renaming(t, h, projectDir(t), args...); code != exitError || !strings.Contains(errOut, `rename takes the app's new name`) || len(h.asked) != 0 {
			t.Errorf("%q: exit %d, stderr %q, asked %q", args, code, errOut, h.asked)
		}
	}
}
