package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/go-jose/go-jose/v4"
)

type tokenKey struct{}

// sidecar is every request's check (contracts/executor.md, The front door): the front door's token
// verified, the access file read, the person's level, then the request on to the app with Canyon's
// token in place of every credential the front door set.
type sidecar struct {
	cfg     config
	verify  verifier
	key     *identity.Key
	access  accessSource
	proxy   *httputil.ReverseProxy
	issuers sync.Map // the issuers off the access file's already logged, each once
}

func newSidecar(c config, v verifier, key *identity.Key, access accessSource, upstream *url.URL) *sidecar {
	rewrite := func(pr *httputil.ProxyRequest) { // runs after the hop-by-hop headers are gone, so a Connection header cannot drop what it sets
		pr.SetURL(upstream)
		pr.Out.Host = pr.In.Host          // the app sees the name the router routed, and no X-Forwarded-Host a browser could have set
		for name := range pr.Out.Header { // after the sidecar has read what it verifies, before it sets its own; by the key as sent, which Header.Del would miss spelled otherwise
			if lower := strings.ToLower(name); identity.ForgedHeader(name) || lower == "authorization" || lower == "cookie" { // and another spelling of what it sets
				delete(pr.Out.Header, name)
			}
		}
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto"} {
			if v, ok := pr.In.Header[h]; ok { // the front door's, so the app builds links with the scheme people use
				pr.Out.Header[h] = v
			}
		}
		pr.Out.Header.Set("Authorization", "Bearer "+pr.In.Context().Value(tokenKey{}).(string)) // replacing the front door's
		var kept []string                                                                        // the app's own cookies; the front door's session never reaches it
		for _, line := range pr.In.Header.Values("Cookie") {
			for _, pair := range strings.Split(line, ";") {
				name, _, _ := strings.Cut(strings.TrimSpace(pair), "=")
				if pair = strings.TrimSpace(pair); pair != "" && !strings.HasPrefix(name, c.door.Cookie) {
					kept = append(kept, pair)
				}
			}
		}
		pr.Out.Header.Del("Cookie")
		if len(kept) > 0 {
			pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
		}
	}
	return &sidecar{cfg: c, verify: v, key: key, access: access, proxy: &httputil.ReverseProxy{Rewrite: rewrite}} // an app not answering is 502
}

func (s *sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if slices.Contains([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, r.Method) && !s.sameOrigin(r) { // every app shares one site: a change is this app's own pages' alone
		writeErr(w, http.StatusForbidden, "cross_origin", "this request did not come from this app")
		return
	}
	claims, issuer, err := s.verify(r)
	var expired *oidc.TokenExpiredError
	if s.cfg.door.Choice == 2 && errors.As(err, &expired) { // a fresh sign-in, not a dead end
		http.Redirect(w, r, "/oauth2/start?"+url.Values{"rd": {r.URL.RequestURI()}}.Encode(), http.StatusFound)
		return
	}
	sub, _ := claims["sub"].(string)
	var email string
	if err == nil && sub == "" {
		err = errors.New("the token names no sub")
	}
	if err == nil {
		email, err = emailOf(claims)
	}
	if err != nil {
		// Why, and what to check, is the operator's: the visitor's page says only what they can do (canyonv2-8c2q).
		log.Printf("canyon-identity: refused: %.200q; check that the gateway's sign-in rule covers this app's address", err)
		if acceptsHTML(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, signInPage)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in to open this app")
		return
	}
	f, err := s.access.read()
	if err != nil {
		log.Printf("canyon-identity: the access file: %.200q", err)
		writeErr(w, http.StatusServiceUnavailable, "access_unreadable", "this app's access list cannot be read")
		return
	}
	if f.SubjectIssuer != "" && issuer != f.SubjectIssuer {
		if _, logged := s.issuers.LoadOrStore(issuer, true); !logged {
			log.Printf("canyon-identity: a token from %.200q, not Canyon's sign-in issuer %.200q: a grant bound to a person matches nobody on it, a group or everyone grant still does", issuer, f.SubjectIssuer)
		}
	}
	level, roles := identity.Level(email, f.subjectOf(issuer, sub), f.groups(claims), f.Entries)
	if !f.member(email) || level == "none" && !f.admin(email, claims) {
		http.Redirect(w, r, s.cfg.issuer+"/#/no-access/"+s.cfg.app, http.StatusFound) //nolint:gosec // Canyon's public URL and the app id, both the executor's
		return
	}
	name, _ := claims["name"].(string)
	minted, err := s.key.Mint(identity.Claims{Issuer: s.cfg.issuer, Subject: sub, Email: email, Name: cmp.Or(strings.TrimSpace(name), email), Roles: roles, Audience: s.cfg.app, Env: s.cfg.target})
	if err != nil {
		log.Printf("canyon-identity: mint: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, minted)))
}

// verifier is the front door's check for its mode, believing one thing and nothing else: the person's
// claims and the issuer they are from, or why there are none.
type verifier func(*http.Request) (map[string]any, string, error)

// bearer is choice 1 oidc's and choice 2's: an ID token in the one Authorization: Bearer header, verified
// — the issuer, the audience, RS256 or ES256, not expired. It is from issuer, the one v checked: go-oidc
// takes Google's iss accounts.google.com for https://accounts.google.com and keeps the iss as sent.
func bearer(v *oidc.IDTokenVerifier, issuer string) verifier {
	return func(r *http.Request) (map[string]any, string, error) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			return nil, "", errors.New("no bearer token")
		}
		tok, err := v.Verify(r.Context(), strings.TrimPrefix(values[0], "Bearer "))
		var claims map[string]any
		if err == nil {
			err = tok.Claims(&claims)
		}
		if err != nil {
			return nil, "", err
		}
		return claims, issuer, nil
	}
}

// sameOrigin is the single host's fromCanyon (cmd/canyon-server/session.go) on the app's own host:
// Sec-Fetch-Site decides when present; else an Origin must be this host under Canyon's scheme.
func (s *sidecar) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	scheme, _, _ := strings.Cut(s.cfg.issuer, "://")
	origin := r.Header.Get("Origin")
	return origin == "" || originOf(origin) == originOf(scheme+"://"+r.Host)
}

func originOf(origin string) string {
	if origin = strings.ToLower(origin); strings.HasPrefix(origin, "https://") {
		return strings.TrimSuffix(origin, ":443")
	}
	return strings.TrimSuffix(origin, ":80")
}

// emailOf is the single host's (cmd/canyon-server/auth.go): the email claim, else preferred_username,
// lower-cased and address-shaped, an email_verified present and not true naming nobody — and ASCII
// alone, so no two spellings of one address compare apart.
func emailOf(c map[string]any) (string, error) {
	if v, ok := c["email_verified"]; ok && v != true {
		return "", errors.New("email_verified is not true")
	}
	for _, name := range []string{"email", "preferred_username"} {
		if s, _ := c[name].(string); strings.TrimSpace(s) != "" {
			s = strings.ToLower(strings.TrimSpace(s))
			at := strings.LastIndex(s, "@")
			if at <= 0 || strings.ContainsAny(s[:at], "@ \t\n\r,<>") || !strings.Contains(s[at+1:], ".") || strings.ContainsAny(s[at+1:], "/ \t\n\r") ||
				strings.ContainsFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) {
				return "", fmt.Errorf("the %s claim is not an email address", name)
			}
			return s, nil
		}
	}
	return "", errors.New("the token carries neither email nor preferred_username")
}

// access is the app's access file (contracts/executor.md, PUT /access), the ConfigMap mounted whole.
type access struct {
	UpdatedAt     time.Time `json:"updated_at"`
	GroupsClaim   string    `json:"groups_claim"`
	SubjectIssuer string    `json:"subject_issuer"` // Canyon's own sign-in, which recorded every entry's subject; "" in a file from before it
	Members       struct {
		Domains []string `json:"domains"`
		Emails  []string `json:"emails"`
	} `json:"members"`
	Admins struct {
		Emails []string `json:"emails"`
		Claim  string   `json:"claim"`
		Value  string   `json:"value"`
	} `json:"admins"`
	Entries []identity.Grant `json:"entries"`
}

// member is design/AUTH.md's membership: the address, or its domain, listed.
func (f *access) member(email string) bool {
	return slices.Contains(f.Members.Emails, email) || slices.Contains(f.Members.Domains, email[strings.LastIndex(email, "@")+1:])
}

// admin is design/AUTH.md's: the address listed, or the admin claim — membership for an array, equality
// for a string, compared exactly.
func (f *access) admin(email string, c map[string]any) bool {
	if slices.Contains(f.Admins.Emails, email) {
		return true
	}
	switch v := c[f.Admins.Claim].(type) {
	case string:
		return f.Admins.Claim != "" && v == f.Admins.Value
	case []any:
		return f.Admins.Claim != "" && slices.Contains(v, any(f.Admins.Value))
	case map[string]any: // an object's keys are its names (Zitadel's project roles)
		_, ok := v[f.Admins.Value]
		return f.Admins.Claim != "" && ok
	}
	return false
}

// subjectOf is sub where it compares with the entries' subjects — the token is from the issuer that
// recorded them — else "", which names nobody: a subject names a person only under the issuer that issued
// it (design/PERMISSIONS.md, Grants).
func (f *access) subjectOf(issuer, sub string) string {
	if f.SubjectIssuer == "" || issuer != f.SubjectIssuer {
		return ""
	}
	return sub
}

// groups are the person's groups: the file's groups_claim read as the server reads it (identity.Groups).
func (f *access) groups(c map[string]any) []string {
	if f.GroupsClaim == "" { // none set: as the server reads it, no groups
		return nil
	}
	raw, present := c[f.GroupsClaim]
	groups, _ := identity.Groups(raw, present)
	return groups
}

// accessSource is where the access file comes from: the ConfigMap's file, or Canyon under hand-off.
type accessSource interface {
	read() (*access, error)
}

// accessReader reads the file on every request, so a share reaches the app without a restart, and
// never goes back: a file older than one it has read is set aside for that newer one.
type accessReader struct {
	path string
	mu   sync.Mutex
	last *access
}

func (a *accessReader) read() (*access, error) {
	b, err := os.ReadFile(a.path)
	if err != nil {
		return nil, err
	}
	f, err := decodeAccess(b)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = newer(a.last, f)
	return a.last, nil
}

func decodeAccess(b []byte) (*access, error) {
	var f access
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.UpdatedAt.IsZero() {
		return nil, errors.New("no updated_at")
	}
	return &f, nil
}

// newer is the one to hold: f, unless the one held is newer.
func newer(held, f *access) *access {
	if held != nil && f.UpdatedAt.Before(held.UpdatedAt) {
		return held
	}
	return f
}

// accessPuller is pull mode (design/HAND-OFF.md §5): the same object fetched from Canyon at start and
// every minute under the release's token, each good fetch's taken as it is — an older updated_at too, so
// a deny-all Canyon answers is never set aside, where the file reader keeps the newer. When
// a fetch fails the last one is served for the grace, counted from that first failure, then every
// request is refused — at once for a grace of 0. The token goes into the one header and nowhere else:
// no error and no log line carries it, and a redirect is a failed fetch, so it goes to no other address.
// Each good fetch caches the list and when it was fetched (cache), so a sidecar started again while
// Canyon is unreachable serves it for what is left of the grace (startAccess).
type accessPuller struct {
	url, token string
	app        string // the app's address, dialled before each fetch: Canyon-App says whether it answered (appAnswers)
	grace      time.Duration
	client     *http.Client
	now        func() time.Time
	cache      string // the cache file, on the emptyDir the sidecar alone should mount; without it every write fails (save)
	mu         sync.Mutex
	last       *access
	failing    time.Time // the first failed fetch since the last good one; zero while fetches succeed
	unsaved    bool      // the last cache write failed, and was logged
}

// accessCache is where pull mode caches its list: the emptyDir the docs' reference pod spec mounts at
// /var/cache/canyon in the sidecar's container alone (Your platform runs the apps), which the org's pod
// spec may leave out.
const accessCache = "/var/cache/canyon/access.json"

// cached is the cache file: the list held after the last good fetch, and when that fetch was.
type cached struct {
	List        *access   `json:"list"`
	LastSuccess time.Time `json:"last_success"`
}

func newAccessPuller(c config, client *http.Client) *accessPuller {
	once := *client
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &accessPuller{url: c.accessURL, token: c.accessToken, app: appAddr, grace: c.accessGrace, client: &once, now: time.Now, cache: accessCache}
}

func (p *accessPuller) fetch(ctx context.Context) error {
	f, err := p.get(ctx)
	p.mu.Lock()
	if err != nil {
		if p.failing.IsZero() {
			p.failing = p.now()
		}
		p.mu.Unlock()
		return fmt.Errorf("access %s: %w", p.url, err)
	}
	p.failing = time.Time{}
	p.last = f
	c := cached{List: f, LastSuccess: p.now()}
	p.mu.Unlock()
	p.save(c)
	return nil
}

// save writes the cache whole or not at all. A failure — no emptyDir mounted, among others — is logged
// once until a write succeeds, leaves the cache as it was, and the sidecar runs on.
func (p *accessPuller) save(c cached) {
	err := writeAtomic(p.cache, c)
	if err != nil && !p.unsaved {
		log.Printf("canyon-identity: the access cache: %v; a restart while Canyon is unreachable will not start", err)
	}
	p.unsaved = err != nil
}

// writeAtomic writes c to path through path.tmp, truncated first and then renamed over path: a write a
// crash cuts short leaves that one file, which the next write replaces, so a crash loop piles up nothing.
func writeAtomic(path string, c cached) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the sidecar's own cache path
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// restore serves the cached list while the grace, counted from the good fetch that cached it, lasts: a
// restart never extends it. A cache that cannot be read or parsed, holds no list with an updated_at, or
// names no fetch or one after now, is no cache. The list's updated_at is Canyon's clock and last_success
// the node's, so the two are never compared; the first good fetch replaces the list whatever its date.
func (p *accessPuller) restore() (time.Time, error) {
	b, err := os.ReadFile(p.cache)
	if err != nil {
		return time.Time{}, err
	}
	var c struct {
		List        json.RawMessage `json:"list"`
		LastSuccess time.Time       `json:"last_success"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return time.Time{}, err
	}
	f, err := decodeAccess(c.List)
	if err != nil {
		return time.Time{}, fmt.Errorf("its list: %w", err)
	}
	now := p.now()
	if c.LastSuccess.After(now) || now.Sub(c.LastSuccess) >= p.grace { // none, the zero time, is past every grace
		return time.Time{}, fmt.Errorf("fetched at %s, not within the grace of %s", c.LastSuccess.UTC().Format(time.RFC3339), p.grace)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last, p.failing = f, c.LastSuccess
	return c.LastSuccess, nil
}

// startAccess is pull mode's start: the fetch tried again as startVerifier tries its own, the wait between
// doubling from 250 ms to 2 s, each failure logged once, until Canyon answers or wait has passed; then the
// cached list, for what is left of the grace. Neither is the last error, which stops the sidecar, as one
// that cannot reach its key set never starts.
func startAccess(p *accessPuller, logger *log.Logger, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var err error
	for backoff := 250 * time.Millisecond; ctx.Err() == nil; backoff = min(2*backoff, 2*time.Second) {
		if err = p.fetch(ctx); err == nil {
			return nil
		}
		logger.Printf("canyon-identity: %v; trying again in %v", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
	at, rerr := p.restore()
	if rerr != nil {
		return fmt.Errorf("%w; the access cache: %w", err, rerr)
	}
	logger.Printf("canyon-identity: Canyon unreachable at start; serving the access list cached at %s until %s, the grace", at.UTC().Format(time.RFC3339), at.Add(p.grace).UTC().Format(time.RFC3339))
	return nil
}

// appAddr is where the app listens in the pod, beside the sidecar: what the sidecar forwards to.
const appAddr = "127.0.0.1:3000"

// appAnswers is whether the app accepts a connection on its port now, within a second: the sidecar is a native
// init container that pulls before the app starts, so its pull alone proves the pod, never the app.
func (p *accessPuller) appAnswers(ctx context.Context) bool {
	dial, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dial, "tcp", p.app)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// get is one GET of the access file, 1 MiB at most; a status other than 200 — a 401 under a token a
// newer release has rotated — is a failed fetch like any other. It carries Canyon-App: answered when the app
// accepted a connection just before (contracts/executor.md, Pull mode): under delivery: repo the check-in it
// makes then settles the deploy, so a pod whose app never starts is never live (#2470).
func (p *accessPuller) get(ctx context.Context) (*access, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if p.appAnswers(ctx) {
		req.Header.Set("Canyon-App", "answered")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return decodeAccess(b)
}

// run fetches every interval until ctx ends; a failure is logged and read decides what it means.
func (p *accessPuller) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := p.fetch(ctx); err != nil {
			log.Printf("canyon-identity: %v", err)
		}
	}
}

func (p *accessPuller) read() (*access, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.failing.IsZero() && p.now().Sub(p.failing) >= p.grace {
		return nil, fmt.Errorf("not fetched since %s, past the grace of %s", p.failing.UTC().Format(time.RFC3339), p.grace)
	}
	return p.last, nil
}

// keySet is the issuer's keys, fetched again when a token names a kid it does not hold, at most once a
// minute (contracts/executor.md): a stream of made-up kids costs one fetch a minute.
type keySet struct {
	url     string
	client  *http.Client
	mu      sync.Mutex
	keys    []jose.JSONWebKey
	fetched time.Time
}

func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return nil, err
	}
	kid := jws.Signatures[0].Header.KeyID
	k.mu.Lock()
	defer k.mu.Unlock()
	if !slices.ContainsFunc(k.keys, func(key jose.JSONWebKey) bool { return key.KeyID == kid }) && time.Since(k.fetched) >= time.Minute {
		if err := k.refresh(ctx); err != nil {
			log.Printf("canyon-identity: %v", err)
		}
	}
	for _, key := range k.keys {
		if kid == "" || key.KeyID == kid {
			if payload, err := jws.Verify(&key); err == nil {
				return payload, nil
			}
		}
	}
	return nil, errors.New("no key of the issuer's key set verifies the token")
}

// refresh fetches the key set, 1 MiB at most; the caller holds mu, but for the fetch at start.
func (k *keySet) refresh(ctx context.Context) error {
	k.fetched = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var set jose.JSONWebKeySet
	if err = errors.New(resp.Status); resp.StatusCode == http.StatusOK {
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set)
	}
	if err == nil && len(set.Keys) == 0 && len(k.keys) > 0 {
		err = errors.New("no keys in it, so the keys held are kept")
	}
	if err != nil {
		return fmt.Errorf("key set %s: %w", k.url, err)
	}
	k.keys = set.Keys
	return nil
}

// signInPage is a browser's answer when no sign-in the sidecar accepts reached the app
// (contracts/executor.md, The front door): what the visitor can do, and no detail of why.
const signInPage = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>You're not signed in to this app</title>
<p style="font-family: system-ui, sans-serif; max-width: 36rem; margin: 4rem auto; padding: 0 1rem; line-height: 1.5">You're not signed in to this app. Whoever shared the link with you can help.</p>
</html>
`

// acceptsHTML reports whether the request's Accept names text/html at a quality above zero, as a
// browser's navigation does and a script's request does not.
func acceptsHTML(r *http.Request) bool {
	for _, line := range r.Header.Values("Accept") {
		for _, part := range strings.Split(line, ",") {
			media, params, err := mime.ParseMediaType(part)
			q, qerr := strconv.ParseFloat(cmp.Or(params["q"], "1"), 64)
			if err == nil && qerr == nil && media == "text/html" && q > 0 {
				return true
			}
		}
	}
	return false
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
