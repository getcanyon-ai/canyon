package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A session the database can't be read for is the server's failure, never a person signed out: with sign-in on, 503
// session_unread on every route behind the stage, saying so, the cookie left alone. A cookie the stage can't take for a
// session is 401, and a sign-out answered so clears it, as a sign-out that revoked one does; an admin's revoke of someone
// else clears nothing. With sign-in off every caller is the anonymous admin, as before (#153).
func TestAnUnreadableSessionIs503AndASignOutsUnknownSessionClearsItsCookie(t *testing.T) {
	a := &app{pool: unreachable(t), auth: authOIDC, public: "https://canyon.meridian.example", log: log.New(io.Discard, "", 0)}
	served := false
	stage := a.session(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	name, _ := sessionCookieOf(a.public)
	ask := func(method, path, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Cookie", name+"="+cookie) // what a browser sends: a name and a value
		rec := httptest.NewRecorder()
		stage.ServeHTTP(rec, req)
		return rec
	}
	ours := strings.Repeat("a", 2*tokenBytes)                                      // a cookie of a session's length, which the stage reads the database for
	off := &app{pool: unreachable(t), auth: authOff, public: a.public, log: a.log} // sign-in off: every caller is the anonymous admin
	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	req.Header.Set("Cookie", name+"="+ours)
	off.session(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })).ServeHTTP(httptest.NewRecorder(), req)
	if !served {
		t.Fatal("with sign-in off, a cookie the database can't read is still the anonymous admin's request")
	}
	served = false
	unread := `{"error":"session_unread","message":"Canyon couldn't check your sign-in just now. Try again in a moment."}`
	for _, c := range []struct {
		method, path, cookie string
		code                 int
		body                 string
		clears               bool
	}{
		{"GET", "/fleet", ours, http.StatusServiceUnavailable, unread, false},
		{"DELETE", "/sessions", ours, http.StatusServiceUnavailable, unread, false},
		{"GET", "/fleet", "not-ours", http.StatusUnauthorized, "", false},
		{"DELETE", "/sessions", "not-ours", http.StatusUnauthorized, "", true},
		{"DELETE", "/sessions?subject=bob@meridian.example", "not-ours", http.StatusUnauthorized, "", false},
	} {
		rec := ask(c.method, c.path, c.cookie)
		set := rec.Header().Get("Set-Cookie")
		cleared := strings.HasPrefix(set, name+"=;") && strings.Contains(set, "Max-Age=0") && strings.Contains(set, "HttpOnly") && strings.Contains(set, "Secure") && strings.Contains(set, "Path=/")
		body := strings.TrimSpace(rec.Body.String())
		if rec.Code != c.code || served || cleared != c.clears || (!c.clears && set != "") || (c.body != "" && body != c.body) {
			t.Errorf("%s %s with %s: %d %s, served %t, Set-Cookie %q; want %d %s, not served, cleared %t", c.method, c.path, c.cookie[:6], rec.Code, body, served, set, c.code, c.body, c.clears)
		}
	}
}
