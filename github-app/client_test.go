package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

var ctx = context.Background()

// fakeAPI is GitHub: installation 42 mints token N+1 per call, valid an hour on the test's clock, 43
// is refused; acme/claims-portal's check runs record body and authorization, acme/invalid's fail.
type fakeAPI struct {
	*http.ServeMux
	now    *time.Time
	mints  atomic.Int32
	revoke atomic.Bool // answer 401 to a check run under fake-token-1
	bodies []string
	auths  []string
}

// setup is a fake GitHub and a client of it with the given timeout, both on the test's clock.
func setup(t *testing.T, timeout time.Duration) (*fakeAPI, *Client) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	f := &fakeAPI{ServeMux: http.NewServeMux(), now: &now}
	f.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"message":"no jwt"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"fake-token-%d","expires_at":%q}`, f.mints.Add(1), f.now.Add(time.Hour).Format(time.RFC3339))
	})
	f.HandleFunc("POST /app/installations/43/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	})
	check := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.bodies, f.auths = append(f.bodies, string(body)), append(f.auths, r.Header.Get("Authorization"))
		if f.revoke.Load() && r.Header.Get("Authorization") == "token fake-token-1" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7}`)
	}
	f.HandleFunc("POST /repos/acme/claims-portal/check-runs", check)
	f.HandleFunc("PATCH /repos/acme/claims-portal/check-runs/7", check)
	f.HandleFunc("POST /repos/acme/invalid/check-runs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Validation Failed","documentation_url":"https://docs.github.com"}`, http.StatusUnprocessableEntity)
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := newWithTimeout(srv.URL, 1234, key, timeout)
	c.now = func() time.Time { return *f.now }
	return f, c
}

func TestTheAppJWTIsSignedRS256WithIssuerAndATenMinuteWindow(t *testing.T) {
	f, c := setup(t, time.Second)
	jwt, err := c.jwt()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has three parts: %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&c.key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the signature does not verify with the App's public key: %v", err)
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if want := fmt.Sprintf(`{"iss":1234,"iat":%d,"exp":%d}`, f.now.Unix()-60, f.now.Unix()+540); string(header) != `{"alg":"RS256","typ":"JWT"}` || string(claims) != want {
		t.Fatalf("header %s, claims %s, want exactly %s", header, claims, want)
	}
}

func TestTheInstallationTokenIsReusedUntilAMinuteBeforeItExpires(t *testing.T) {
	f, c := setup(t, time.Second)
	create := func() {
		t.Helper()
		if _, err := c.CreateCheckRun(ctx, 42, "acme", "claims-portal", CheckRun{Name: "Canyon gate", HeadSHA: sha, Status: "in_progress"}); err != nil {
			t.Fatal(err)
		}
	}
	create()
	create()
	*f.now = f.now.Add(time.Hour - time.Minute - time.Second) // still inside the window
	create()
	if n := f.mints.Load(); n != 1 {
		t.Fatalf("a live token is reused: %d mints", n)
	}
	*f.now = f.now.Add(2 * time.Second) // past expiry minus a minute
	create()
	if want := "token fake-token-1,token fake-token-1,token fake-token-1,token fake-token-2"; f.mints.Load() != 2 || strings.Join(f.auths, ",") != want {
		t.Fatalf("a token within a minute of expiry is replaced: %d mints, authorizations %v, want %s", f.mints.Load(), f.auths, want)
	}
}

// A 401 means GitHub no longer takes the cached token: the next call mints a fresh one.
func TestA401DropsTheCachedToken(t *testing.T) {
	f, c := setup(t, time.Second)
	f.revoke.Store(true)
	run := CheckRun{Name: "Canyon gate", HeadSHA: sha}
	if _, err := c.CreateCheckRun(ctx, 42, "acme", "claims-portal", run); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a revoked token: %v", err)
	}
	if _, err := c.CreateCheckRun(ctx, 42, "acme", "claims-portal", run); err != nil || f.mints.Load() != 2 {
		t.Fatalf("the next call mints a fresh token: %v, %d mints", err, f.mints.Load())
	}
}

// The timeout bounds a tarball's wait for headers, not its download; a JSON call it bounds in all.
func TestATarballSlowerThanTheTimeoutStillArrives(t *testing.T) {
	f, c := setup(t, 100*time.Millisecond)
	tarball := bytes.Repeat([]byte("tgz"), 40)
	trickle := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			for i := range body {
				_, _ = w.Write(body[i : i+1])
				w.(http.Flusher).Flush()
				time.Sleep(400 * time.Millisecond / time.Duration(len(body)))
			}
		}
	}
	f.HandleFunc("GET /repos/acme/claims-portal/tarball/"+sha, trickle(tarball))
	f.HandleFunc("POST /repos/acme/slow/check-runs", trickle([]byte(`{"id":8}`)))
	body, err := c.Tarball(ctx, 42, "acme", "claims-portal", sha)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || !bytes.Equal(got, tarball) {
		t.Fatalf("tarball: %d bytes, want %d: %v", len(got), len(tarball), err)
	}
	if _, err := c.CreateCheckRun(ctx, 42, "acme", "slow", CheckRun{Name: "Canyon gate", HeadSHA: sha}); err == nil {
		t.Fatal("a JSON answer slower than the timeout is an error")
	}
}

func TestCheckRunBodiesAreExactlyTheFieldsSet(t *testing.T) {
	f, c := setup(t, time.Second)
	id, err := c.CreateCheckRun(ctx, 42, "acme", "claims-portal", CheckRun{Name: "Canyon gate", HeadSHA: sha, Status: "in_progress"})
	if err != nil || id != 7 {
		t.Fatalf("create: %d %v", id, err)
	}
	if err := c.UpdateCheckRun(ctx, 42, "acme", "claims-portal", 7, CheckRun{Conclusion: "failure", Output: &Output{
		Title: "1 finding blocks", Summary: "Canyon found 1 finding.",
		Annotations: []Annotation{{Path: "src/db.ts", StartLine: 3, EndLine: 4, AnnotationLevel: "failure", Title: "SQL built from input", Message: "Use a parameter.", RawDetails: "no concatenation"}},
	}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"name":"Canyon gate","head_sha":"` + sha + `","status":"in_progress"}`,
		`{"conclusion":"failure","output":{"title":"1 finding blocks","summary":"Canyon found 1 finding.","annotations":[{"path":"src/db.ts","start_line":3,"end_line":4,"annotation_level":"failure","title":"SQL built from input","message":"Use a parameter.","raw_details":"no concatenation"}]}}`,
	}
	if strings.Join(f.bodies, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bodies:\n got %s\nwant %s", f.bodies, want)
	}
}

// GitHub's 302 to codeload carries its own token: the installation token must not go to that host.
func TestTheTarballFollowsOneRedirectWithoutTheToken(t *testing.T) {
	tarball := []byte("the gzipped tree")
	var codeloadAuth atomic.Value
	var again atomic.Bool
	codeload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeloadAuth.Store(r.Header.Get("Authorization"))
		if again.Load() {
			http.Redirect(w, r, "/elsewhere?token=codeload-secret", http.StatusFound)
			return
		}
		_, _ = w.Write(tarball)
	}))
	defer codeload.Close()
	f, c := setup(t, time.Second)
	f.HandleFunc("GET /repos/acme/claims-portal/tarball/"+sha, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fake-token-1" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		http.Redirect(w, r, codeload.URL+"/acme/claims-portal/legacy.tar.gz/"+sha+"?token=codeload-secret", http.StatusFound)
	})
	body, err := c.Tarball(ctx, 42, "acme", "claims-portal", sha)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if a := codeloadAuth.Load(); !bytes.Equal(got, tarball) || a != "" {
		t.Fatalf("tarball %q; the redirect's host saw Authorization %q", got, a)
	}
	again.Store(true) // a second redirect is refused, and the refusal does not carry the redirect's token
	if _, err = c.Tarball(ctx, 42, "acme", "claims-portal", sha); err == nil || strings.Contains(err.Error(), "codeload-secret") || strings.Contains(err.Error(), "fake-token") {
		t.Fatalf("a second redirect: %v", err)
	}
}

func TestAnErrorNamesTheStatusAndGitHubsMessageButNoCredential(t *testing.T) {
	_, c := setup(t, time.Second)
	jwt, _ := c.jwt()
	_, err := c.CreateCheckRun(ctx, 42, "acme", "invalid", CheckRun{Name: "Canyon gate", HeadSHA: sha})
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "Validation Failed") || strings.Contains(err.Error(), "fake-token") {
		t.Fatalf("a refused check run: %v", err)
	}
	_, err = c.CreateCheckRun(ctx, 43, "acme", "claims-portal", CheckRun{Name: "Canyon gate", HeadSHA: sha})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "could not be decoded") || strings.Contains(err.Error(), jwt[:20]) {
		t.Fatalf("a refused token: %v", err)
	}
}

func TestPathSegmentsOutsideGitHubsCharsetAreRefusedBeforeAnyRequest(t *testing.T) {
	f, c := setup(t, time.Second)
	for _, tc := range []struct{ owner, repo, sha string }{
		{"..", "claims-portal", sha},
		{"acme", "claims/portal", sha},
		{"acme", "claims-portal?x=1", sha},
		{"", "claims-portal", sha},
		{"acme", "claims-portal", sha[:39]},
		{"acme", "claims-portal", strings.ToUpper(sha)},
		{"acme", "claims-portal", "../../../app/installations/42/access_tokens"},
	} {
		if _, err := c.Tarball(ctx, 42, tc.owner, tc.repo, tc.sha); err == nil {
			t.Fatalf("tarball %+v accepted", tc)
		}
		if tc.sha != sha {
			continue // the head sha is a body field of a check run, not a path segment
		}
		if _, err := c.CreateCheckRun(ctx, 42, tc.owner, tc.repo, CheckRun{HeadSHA: tc.sha}); err == nil {
			t.Fatalf("check run %+v accepted", tc)
		}
	}
	if n := f.mints.Load(); n != 0 {
		t.Fatalf("a refused segment still reached GitHub: %d mints", n)
	}
}
