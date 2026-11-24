package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A session the database can't be read for is the server's failure, never a person signed out: 500 on every route behind
// the stage, the cookie left alone. A cookie the stage can't take for a session is 401, and a sign-out answered so
// clears it, as a sign-out that revoked one does; an admin's revoke of someone else clears nothing (#153).
func TestAnUnreadableSessionIs500AndASignOutsUnknownSessionClearsItsCookie(t *testing.T) {
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
	ours := strings.Repeat("a", 2*tokenBytes) // a cookie of a session's length, which the stage reads the database for
	for _, c := range []struct {
		method, path, cookie string
		code                 int
		clears               bool
	}{
		{"GET", "/fleet", ours, http.StatusInternalServerError, false},
		{"DELETE", "/sessions", ours, http.StatusInternalServerError, false},
		{"GET", "/fleet", "not-ours", http.StatusUnauthorized, false},
		{"DELETE", "/sessions", "not-ours", http.StatusUnauthorized, true},
		{"DELETE", "/sessions?subject=bob@meridian.example", "not-ours", http.StatusUnauthorized, false},
	} {
		rec := ask(c.method, c.path, c.cookie)
		set := rec.Header().Get("Set-Cookie")
		cleared := strings.HasPrefix(set, name+"=;") && strings.Contains(set, "Max-Age=0") && strings.Contains(set, "HttpOnly") && strings.Contains(set, "Secure") && strings.Contains(set, "Path=/")
		if rec.Code != c.code || served || cleared != c.clears || (!c.clears && set != "") {
			t.Errorf("%s %s with %s: %d, served %t, Set-Cookie %q; want %d, not served, cleared %t", c.method, c.path, c.cookie[:6], rec.Code, served, set, c.code, c.clears)
		}
	}
}
