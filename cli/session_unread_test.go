package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A host that couldn't read the session (its database didn't answer) says 503 session_unread, never the 401 that means
// it ended (contracts/routes.md; #153). The CLI says the host's sentence, in both formats, and never that the person must
// sign in again: the session may well live.
func TestAnUnreadableSessionSaysTryAgainNotSignIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/config" {
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"oidc","org":"Meridian Insurance","device":true,"setup":false}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"session_unread","message":"Canyon couldn't check your sign-in just now. Try again in a moment."}`)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	writeConfig(t, dir, server.URL, aSession("ab"), 0o600)
	for _, args := range [][]string{{"fleet"}, {"fleet", "--format", "ai"}} {
		out, errOut, code := run(t, xdg(dir), false, args...)
		if want := "\n  ✗ Canyon couldn't check your sign-in just now. Try again in a moment.\n"; code != exitError || out != "" || errOut != want {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want %d and %q", args, code, out, errOut, exitError, want)
		}
	}
}
