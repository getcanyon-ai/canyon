// Command stub stands in for everything outside Canyon in the E2E stack: the Anthropic Messages
// API, the parts of GitHub's REST API the courier calls (under /gh), a provisioner adapter
// (under /adapter), and an OIDC issuer on :8443, over TLS, that the stack with sign-in on signs
// people in through. POST /_control scripts a handler's next answers and POST /_oidc who the next
// sign-in is; every other request on :8080 is appended to the log as one JSON line, an Authorization
// value logged as "present".
package main

import (
	"bytes"
	"cmp"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const webhookSecret = "e2e-webhook-secret" //nolint:gosec // a fixed test value the journeys sign deliveries with, not a credential
const clientSecret = "e2e-client-secret"   //nolint:gosec // a fixed test value the App's registration stores, not a credential

// answer is one scripted reply. A model answer's body is the reviewer's JSON, sent as the text of a
// message when the status is 200; every other body is sent as it is.
type answer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	Times  int             `json:"times"`
}

type stub struct {
	mu       sync.Mutex
	queue    map[string][]answer // by handler; a handler with none answers as the real service would
	log      io.Writer
	pem      string
	token    string // the adapter's bearer token
	fixtures string // the tarball of repo r is fixtures/r.tar.gz
	issuer   *issuer
}

func main() {
	f, err := os.OpenFile("/log/requests.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the harness reads it from the host
	token, tokenErr := os.ReadFile("/run/canyon/provisioners/adapter-token")
	if err := cmp.Or(err, tokenErr); err != nil {
		log.Fatal(err)
	}
	s, err := newStub(f, "/fixtures", strings.TrimSpace(string(token)))
	if err != nil {
		log.Fatal(err)
	}
	// The issuer: its own name on Canyon's network, where the server reads discovery, keys and tokens; the
	// authorization endpoint at the address the journeys' browser reaches, the port published on 127.0.0.1.
	// Its certificate, for both names, is e2e/fixtures/gen.sh's, under a CA the server trusts.
	if _, err := os.Stat("/fixtures/oidc.crt"); err == nil {
		go func() {
			srv := &http.Server{Addr: ":8443", Handler: s.issuer.routes(), ReadHeaderTimeout: 10 * time.Second}
			log.Fatal(srv.ListenAndServeTLS("/fixtures/oidc.crt", "/fixtures/oidc.key"))
		}()
	}
	log.Fatal((&http.Server{Addr: ":8080", Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

// newStub generates the fake App's private key, as GitHub does per App.
func newStub(logTo io.Writer, fixtures, token string) (*stub, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	iss, err := newIssuer("https://stub:8443", "https://localhost:8443")
	if err != nil {
		return nil, err
	}
	return &stub{queue: map[string][]answer{}, log: logTo, pem: string(p), token: token, fixtures: fixtures, issuer: iss}, nil
}

func (s *stub) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_control", s.control)
	mux.HandleFunc("POST /_oidc", s.issuer.control)
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.logged(h)) }
	handle("POST /v1/messages", s.model)
	handle("POST /gh/app-manifests/{code}/conversions", s.canned("conversions", func() any {
		return map[string]any{"id": 1, "slug": "canyon-e2e", "client_id": "Iv1.e2e", "client_secret": clientSecret, "pem": s.pem, "webhook_secret": webhookSecret,
			"owner": map[string]any{"login": "e2e", "id": 1, "type": "Organization"}, "permissions": map[string]string{"checks": "write", "contents": "read", "metadata": "read"}}
	}))
	handle("POST /gh/app/installations/{id}/access_tokens", s.canned("access_tokens", func() any {
		return map[string]any{"token": "e2e-installation-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	}))
	handle("GET /gh/repos/{owner}/{repo}/tarball/{sha}", s.tarball)
	checkRun := s.canned("check_runs", func() any { return map[string]any{"id": 1} })
	handle("POST /gh/repos/{owner}/{repo}/check-runs", checkRun)
	handle("PATCH /gh/repos/{owner}/{repo}/check-runs/{id}", checkRun)
	handle("PUT /adapter/{project}/{env}/{name}", s.adapter)
	handle("DELETE /adapter/{project}/{env}/{name}", s.adapter)
	return mux
}

// control queues {"handler", "status", "body", "times"}: the handler's next times answers.
func (s *stub) control(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Handler string `json:"handler"`
		answer
	}
	known := map[string]bool{"model": true, "conversions": true, "access_tokens": true, "tarball": true, "check_runs": true, "adapter": true}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || !known[c.Handler] || c.Status < 100 || c.Status > 599 {
		http.Error(w, "want {handler, status, body?, times?}, handler one of model, conversions, access_tokens, tarball, check_runs, adapter", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	for range max(c.Times, 1) {
		s.queue[c.Handler] = append(s.queue[c.Handler], c.answer)
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *stub) next(handler string) (answer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queue[handler]
	if len(q) == 0 {
		return answer{}, false
	}
	s.queue[handler] = q[1:]
	return q[0], true
}

// logged appends the request to the log before h answers it.
func (s *stub) logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		headers := r.Header.Clone()
		if headers.Get("Authorization") != "" {
			headers.Set("Authorization", "present")
		}
		line := mustJSON(map[string]any{"method": r.Method, "path": r.URL.Path, "headers": headers, "body": string(body)})
		s.mu.Lock()
		_, err = s.log.Write(append(line, '\n'))
		s.mu.Unlock()
		if err != nil {
			http.Error(w, "request log: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// canned answers the handler's next scripted answer, else def's value as JSON.
func (s *stub) canned(handler string, def func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		a, ok := s.next(handler)
		if !ok {
			a = answer{Status: http.StatusOK, Body: mustJSON(def())}
		}
		reply(w, a)
	}
}

// model answers the Messages API: the scripted answer's body as the one text block, streamed as
// server-sent events when the request streams, as Canyon's reviewer does.
func (s *stub) model(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // a body that does not decode is answered as one that does not stream
	a, ok := s.next("model")
	if !ok {
		a = answer{Status: http.StatusOK, Body: json.RawMessage(`{"results":[]}`)}
	}
	if a.Status == http.StatusOK && req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []map[string]any{
			{"type": "message_start", "message": map[string]any{"id": "msg_e2e", "type": "message", "role": "assistant", "model": "e2e", "content": []any{},
				"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}},
			{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": string(a.Body)}},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}},
			{"type": "message_stop"},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], mustJSON(e))
		}
		return
	}
	if a.Status == http.StatusOK {
		a.Body = mustJSON(map[string]any{
			"id": "msg_e2e", "type": "message", "role": "assistant", "model": "e2e",
			"content":     []map[string]string{{"type": "text", "text": string(a.Body)}},
			"stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}
	reply(w, a)
}

// tarball serves fixtures/<repo>.tar.gz, which e2e/fixtures/gen.sh writes with one root directory, as GitHub's has.
func (s *stub) tarball(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.next("tarball"); ok {
		reply(w, a)
		return
	}
	repo := r.PathValue("repo")
	if !filepath.IsLocal(repo) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.fixtures, repo+".tar.gz")) //nolint:gosec // repo is checked local above
}

// adapter is contracts/provisioner.md's two calls: unless scripted, a PUT is ready with a fake
// output for its type and a DELETE is gone. A wrong bearer token is 401.
func (s *stub) adapter(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		reply(w, answer{Status: http.StatusUnauthorized, Body: json.RawMessage(`{"message":"not the adapter token"}`)})
		return
	}
	a, ok := s.next("adapter")
	switch {
	case ok:
	case r.Method == http.MethodDelete:
		a = answer{Status: http.StatusNoContent}
	default:
		var req struct{ Type string }
		_ = json.NewDecoder(r.Body).Decode(&req) // no type is the 400 below
		outputs := map[string]map[string]string{
			"postgres": {"DATABASE_URL": "postgres://e2e:e2e@adapter.invalid:5432/" + r.PathValue("name")},
			"s3":       {"STORAGE_URL": "http://e2e:e2e@adapter.invalid:9000/" + r.PathValue("name") + "?region=eu-central-1"},
		}[req.Type]
		a = answer{Status: http.StatusOK, Body: mustJSON(map[string]any{"outputs": outputs})}
		if outputs == nil {
			a = answer{Status: http.StatusBadRequest, Body: json.RawMessage(`{"message":"unknown type"}`)}
		}
	}
	reply(w, a)
}

func reply(w http.ResponseWriter, a answer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.Status)
	_, _ = w.Write(a.Body)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// issuer is an OIDC provider that signs in whoever POST /_oidc last named (admin@e2e.test until then),
// with no page: GET /auth sends the browser straight back with a code. It checks nothing it is sent but
// spends each code once; its ID tokens are RS256, for the audience the authorization request named.
type issuer struct {
	url, browser string // the issuer, and the base of the authorization endpoint a browser reaches
	key          *rsa.PrivateKey
	mu           sync.Mutex
	email        string
	codes        map[string]grant
}

type grant struct{ email, nonce, audience string }

func newIssuer(url, browser string) (*issuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &issuer{url: url, browser: browser, key: key, email: "admin@e2e.test", codes: map[string]grant{}}, nil
}

func (p *issuer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, answer{Status: http.StatusOK, Body: mustJSON(map[string]any{"issuer": p.url, "authorization_endpoint": p.browser + "/auth",
			"token_endpoint": p.url + "/token", "jwks_uri": p.url + "/keys", "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		reply(w, answer{Status: http.StatusOK, Body: mustJSON(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "stub-1", "use": "sig", "alg": "RS256",
			"n": b64(p.key.N.Bytes()), "e": b64(big.NewInt(int64(p.key.E)).Bytes())}}})})
	})
	mux.HandleFunc("GET /auth", p.authorize)
	mux.HandleFunc("POST /token", p.exchange)
	return mux
}

// control is POST /_oidc {"email"}: who every sign-in from now on is.
func (p *issuer) control(w http.ResponseWriter, r *http.Request) {
	var c struct{ Email string }
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || !strings.Contains(c.Email, "@") {
		http.Error(w, `want {"email": "<address>"}`, http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.email = c.Email
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || back.Host == "" {
		http.Error(w, "no redirect_uri", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error
	code := hex.EncodeToString(raw)
	p.mu.Lock()
	p.codes[code] = grant{email: p.email, nonce: q.Get("nonce"), audience: q.Get("client_id")}
	p.mu.Unlock()
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // the client's own callback, as a real provider sends it: a stand-in that checks nothing
}

func (p *issuer) exchange(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	p.mu.Lock()
	g, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	if !ok {
		reply(w, answer{Status: http.StatusBadRequest, Body: json.RawMessage(`{"error":"invalid_grant"}`)})
		return
	}
	token, err := p.sign(g)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply(w, answer{Status: http.StatusOK, Body: mustJSON(map[string]any{"access_token": "e2e", "token_type": "Bearer", "expires_in": 3600, "id_token": token})})
}

// sign is an RS256 ID token for g.
func (p *issuer) sign(g grant) (string, error) {
	now := time.Now()
	claims := map[string]any{"iss": p.url, "sub": g.email, "aud": g.audience, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": g.email, "email_verified": true, "name": strings.SplitN(g.email, "@", 2)[0]}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	b64 := base64.RawURLEncoding.EncodeToString
	input := b64(mustJSON(map[string]string{"alg": "RS256", "kid": "stub-1", "typ": "JWT"})) + "." + b64(mustJSON(claims))
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}
