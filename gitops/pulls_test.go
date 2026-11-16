package gitops

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The pulls API follows no redirect: an API that sends Canyon elsewhere never hands the token to the other host,
// and the call is no answer (design/HAND-OFF.md §3b, #2471).
func TestThePullsAPIFollowsNoRedirect(t *testing.T) {
	elsewhere := make(chan string, 8)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"number": 1, "state": "closed", "merged": true}`))
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // the test's own other server: the redirect under test
	}))
	defer api.Close()
	pulls := &Pulls{API: api.URL, Owner: "meridian", Name: "deploy", Token: rand.Text()}
	if _, err := pulls.Get(t.Context(), 1); err == nil {
		t.Fatal("a redirect answered as the pull request")
	}
	select {
	case header := <-elsewhere:
		t.Fatalf("the pulls client followed the redirect, carrying %q", header)
	default:
	}
}

// Either API is https alone: an address saved in plain http never carries the token (a row edited by hand).
func TestAPullsAPIIsHTTPSAlone(t *testing.T) {
	if _, err := NewPulls("http://api.github.example", "https://github.example/meridian/deploy", "t", nil); err == nil {
		t.Error("GitHub's API over plain http was accepted")
	}
	if _, err := NewPulls("https://api.github.example", "https://github.example/meridian/deploy", "t", nil); err != nil {
		t.Errorf("GitHub's API over https: %v", err)
	}
}
