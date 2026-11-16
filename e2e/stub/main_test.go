package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/getcanyon-ai/canyon/gate"
	githubapp "github.com/getcanyon-ai/canyon/github-app"
	"github.com/getcanyon-ai/canyon/provisioners"
)

// serve is a stub on a loopback test server, logging into the returned buffer, with control scripted.
func serve(t *testing.T, fixtures string, controls ...string) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	s, err := newStub(&logged, fixtures, "adapter-token")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	for _, c := range controls {
		resp, err := http.Post(srv.URL+"/_control", "application/json", strings.NewReader(c))
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("control %s: %v %v", c, resp, err)
		}
		_ = resp.Body.Close()
	}
	return srv, &logged
}

// Canyon's own reviewer reads a scripted answer, then the default empty one.
func TestTheReviewerReadsAScriptedAnswerThenNone(t *testing.T) {
	srv, _ := serve(t, t.TempDir(), `{"handler": "model", "status": 200, "body": {"results": [{"rule": "r", "file": "a.ts",
		"start_line": 1, "end_line": 1, "severity": "high", "title": "t", "problem": "p", "fix": "f", "required_end_state": "e", "do_not": ["d"]}]}}`)
	reviewer := gate.NewAnthropic(option.WithoutEnvironmentDefaults(), option.WithBaseURL(srv.URL), option.WithAPIKey("e2e"))
	for _, want := range []int{1, 0} {
		if resp, err := reviewer.Review(context.Background(), gate.Request{Model: "e2e", Files: map[string]string{"a.ts": "x"}}); err != nil || len(resp.Results) != want {
			t.Fatalf("want %d results: %+v %v", want, resp, err)
		}
	}
}

// Canyon's GitHub client converts a manifest, signs with the App key it got, and downloads the
// repository's fixture tarball.
func TestTheGitHubClientConvertsMintsAndDownloadsAFixture(t *testing.T) {
	fixtures := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixtures, "f-cp.tar.gz"), []byte("tarball"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, fixtures)
	conv, err := githubapp.New(srv.URL+"/gh", 0, nil).ConvertManifest(context.Background(), "code")
	if err != nil || conv.WebhookSecret != webhookSecret || conv.ClientSecret != clientSecret { // registration refuses an App without a client secret (#2336)
		t.Fatalf("conversion: %+v %v", conv, err)
	}
	block, _ := pem.Decode([]byte(conv.PEM))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	body, err := githubapp.New(srv.URL+"/gh", conv.ID, key).Tarball(context.Background(), 7, "acme", "f-cp", strings.Repeat("ab", 20))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	if got, err := io.ReadAll(body); err != nil || string(got) != "tarball" {
		t.Fatalf("tarball %q %v", got, err)
	}
}

// Canyon's adapter driver is refused on a wrong token, then gets the scripted pending, then ready;
// the log says the token was present and never holds it.
func TestTheAdapterAnswersInScriptOrderAndTheLogHoldsNoToken(t *testing.T) {
	srv, logged := serve(t, t.TempDir(), `{"handler": "adapter", "status": 202, "body": {"message": "waiting"}}`)
	key := provisioners.Key{Project: "claims-portal", Env: "prod", Name: "db"}
	for _, c := range []struct {
		token string
		want  provisioners.State
	}{{"wrong-token", provisioners.Failed}, {"adapter-token", provisioners.Pending}, {"adapter-token", provisioners.Ready}} {
		file := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(file, []byte(c.token), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := provisioners.NewHTTP(srv.URL+"/adapter", file, "")
		if err != nil {
			t.Fatal(err)
		}
		if r, err := d.Ensure(context.Background(), key, "postgres"); err != nil || r.State != c.want {
			t.Fatalf("want %s: %+v %v", c.want, r, err)
		}
	}
	if strings.Contains(logged.String(), "-token") || strings.Count(logged.String(), `"Authorization":["present"]`) != 3 {
		t.Fatalf("want three requests, each with its token present and not logged:\n%s", logged)
	}
}

// The issuer signs in whoever /_oidc last named, as an OIDC provider Canyon's own library accepts: its
// discovery, its key set and an ID token with the nonce the sign-in sent, its code spent once.
func TestTheIssuerSignsInWhoeverControlNamedOnce(t *testing.T) {
	s, err := newStub(io.Discard, t.TempDir(), "adapter-token")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(s.issuer.routes())
	t.Cleanup(srv.Close)
	s.issuer.url, s.issuer.browser = srv.URL, srv.URL
	ctx := oidc.ClientContext(context.Background(), srv.Client())
	provider, err := oidc.NewProvider(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	control := httptest.NewServer(s.routes())
	t.Cleanup(control.Close)
	browser := srv.Client()
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, email := range []string{"admin@e2e.test", "dana@e2e.test"} {
		if email != "admin@e2e.test" {
			resp, err := http.Post(control.URL+"/_oidc", "application/json", strings.NewReader(`{"email": "`+email+`"}`))
			if err != nil || resp.StatusCode != http.StatusNoContent {
				t.Fatalf("/_oidc: %v %v", resp, err)
			}
			_ = resp.Body.Close()
		}
		resp, err := browser.Get(srv.URL + "/auth?" + url.Values{"redirect_uri": {"https://canyon.test/auth/callback"}, "state": {"s1"}, "nonce": {"n1"}, "client_id": {"canyon"}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		back, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || resp.StatusCode != http.StatusFound || back.Host != "canyon.test" || back.Query().Get("state") != "s1" {
			t.Fatalf("/auth: %d %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		exchange := func() (*http.Response, []byte) {
			resp, err := srv.Client().PostForm(provider.Endpoint().TokenURL, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)
			return resp, raw
		}
		resp, raw := exchange()
		var tok struct {
			IDToken string `json:"id_token"`
		}
		if err := json.Unmarshal(raw, &tok); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("/token: %d %s", resp.StatusCode, raw)
		}
		id, err := provider.Verifier(&oidc.Config{ClientID: "canyon"}).Verify(ctx, tok.IDToken)
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Email    string `json:"email"`
			Verified bool   `json:"email_verified"`
		}
		if err := id.Claims(&claims); err != nil || claims.Email != email || !claims.Verified || id.Nonce != "n1" || id.Subject != email {
			t.Fatalf("the ID token: %+v nonce %q subject %q %v", claims, id.Nonce, id.Subject, err)
		}
		if resp, raw := exchange(); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("a code spent twice: %d %s", resp.StatusCode, raw)
		}
	}
}
