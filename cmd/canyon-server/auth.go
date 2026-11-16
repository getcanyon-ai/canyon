package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/getcanyon-ai/canyon/identity"
)

// authMode is CANYON_AUTH (design/AUTH.md §2). The zero value is off: every test builds an app
// directly. Only main() reads the environment; authModeOf reads an unset value as oidc, so a host
// with no configuration does not start open.
type authMode int

const (
	authOff  authMode = iota // loopback at both ends, no override: go test, a laptop
	authOIDC                 // every real deployment
	authDemo                 // the shipped compose demo, and nothing else: off, bound beyond loopback
)

func (m authMode) String() string { return [...]string{"off", "oidc", "demo"}[m] }

func authModeOf(s string) (authMode, error) {
	switch s {
	case "oidc", "":
		return authOIDC, nil
	case "off":
		return authOff, nil
	case "demo":
		return authDemo, nil
	case "off-and-exposed": // the demo's name before #2416: refused, so a stale file learns the new one
		return 0, errors.New("CANYON_AUTH=off-and-exposed is now called demo: set CANYON_AUTH=demo")
	}
	return 0, fmt.Errorf("CANYON_AUTH=%s is not a mode: oidc, off, demo", s)
}

// publicURLFor is CANYON_PUBLIC_URL under mode. Unset, off and demo serve this machine at
// http://localhost:7777; oidc refuses it, since its redirect URI, cookies and every app's address
// are built from it, and a default of localhost boots a Canyon nobody can sign in to.
func publicURLFor(mode authMode, s string) (*neturl.URL, error) {
	if s == "" {
		if mode == authOIDC {
			return nil, errors.New("CANYON_AUTH=oidc needs CANYON_PUBLIC_URL: set it to the https address people open Canyon at, such as https://canyon.example.com")
		}
		s = "http://localhost:7777"
	}
	return publicURLOf(s)
}

// refuseExposed is the bind rule. off must be loopback at both ends, because a host bound to
// 127.0.0.1 behind a reverse proxy on a public address is an unauthenticated Canyon on the internet.
// demo listens on its container's network but serves a loopback address alone, for the same reason:
// every caller is an admin. oidc is served over https, but on a loopback address, so its session
// cookie is never sent in the clear.
func refuseExposed(mode authMode, addr string, public *neturl.URL) error {
	switch mode {
	case authOff:
		if !loopback(addr) {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				return fmt.Errorf("CANYON_ADDR=%s has no port", addr)
			}
			return fmt.Errorf("CANYON_AUTH=off binds loopback only, and CANYON_ADDR=%s is beyond it", addr)
		}
		if !loopbackHost(public.Hostname()) {
			return fmt.Errorf("CANYON_AUTH=off serves loopback only, and CANYON_PUBLIC_URL names %s", public.Hostname())
		}
	case authDemo:
		if !loopbackHost(public.Hostname()) {
			return fmt.Errorf("CANYON_AUTH=demo serves loopback only, and CANYON_PUBLIC_URL names %s: set CANYON_AUTH=oidc for a Canyon other people reach", public.Hostname())
		}
	case authOIDC:
		if public.Scheme != "https" && !loopbackHost(public.Hostname()) {
			return fmt.Errorf("CANYON_AUTH=oidc needs an https CANYON_PUBLIC_URL, and it is %s://%s: set the https address people open Canyon at", public.Scheme, public.Host)
		}
	}
	return nil
}

// oidcConfig is the oidc mode's environment, validated for shape at boot (design/AUTH.md §3).
type oidcConfig struct {
	issuer       string
	clientID     string
	clientSecret string   // empty: a public PKCE client, no secret sent
	domains      []string // membership: a verified email at one of these
	emails       []string // membership: exactly these addresses
	admins       []string
	adminClaim   string // with adminValue, the optional group claim that means admin
	adminValue   string
	// with responderValue, the optional claim that means responder (design/PERMISSIONS.md, Security responders)
	responderClaim, responderValue string
	groupsClaim                    string // the claim that carries a person's groups, over the row's groups_claim
}

// oidcConfigOf reads the oidc variables through get. Every refusal is fatal at boot: a typo must
// never replace a working process in a rolling restart.
func oidcConfigOf(get func(string) string) (*oidcConfig, error) {
	c := &oidcConfig{issuer: get("CANYON_OIDC_ISSUER"), clientID: get("CANYON_OIDC_CLIENT_ID"), clientSecret: get("CANYON_OIDC_CLIENT_SECRET"), adminClaim: strings.TrimSpace(get("CANYON_ADMIN_CLAIM")), adminValue: strings.TrimSpace(get("CANYON_ADMIN_VALUE")), responderClaim: strings.TrimSpace(get("CANYON_RESPONDER_CLAIM")), responderValue: strings.TrimSpace(get("CANYON_RESPONDER_VALUE")), groupsClaim: strings.TrimSpace(get("CANYON_GROUPS_CLAIM"))}
	var err error
	if c.domains, err = listOf("CANYON_OIDC_ALLOWED_DOMAINS", get("CANYON_OIDC_ALLOWED_DOMAINS"), "a domain", isDomain); err != nil {
		return nil, err
	}
	if c.emails, err = listOf("CANYON_OIDC_ALLOWED_EMAILS", get("CANYON_OIDC_ALLOWED_EMAILS"), "an email address", isEmail); err != nil {
		return nil, err
	}
	if c.admins, err = listOf("CANYON_ADMINS", get("CANYON_ADMINS"), "an email address", isEmail); err != nil {
		return nil, err
	}
	var missing []string
	for _, v := range []struct{ name, value string }{{"CANYON_OIDC_ISSUER", c.issuer}, {"CANYON_OIDC_CLIENT_ID", c.clientID}} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(c.admins) == 0 { // the bootstrap admins: the only people who may sign in until somebody has said who else may (design/ONBOARDING.md §2); the membership lists may come from the row
		missing = append(missing, "CANYON_ADMINS")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("CANYON_AUTH=oidc needs %s; set CANYON_AUTH=off for a local try", strings.Join(missing, ", "))
	}
	if u, err := neturl.Parse(c.issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("CANYON_OIDC_ISSUER=%s is not an https URL", c.issuer)
	}
	c.issuer = strings.TrimSuffix(c.issuer, "/") // the document names the issuer without it, and the match is exact
	for _, a := range c.admins {                 // admin and membership are separate questions: an admin nobody lists cannot sign in once setup is over, so say so now — when both lists are the environment's; with one from the row it is the row's to say
		if c.domains != nil && c.emails != nil && !memberOf(a, c.domains, c.emails) {
			return nil, fmt.Errorf("CANYON_ADMINS: %s is on neither CANYON_OIDC_ALLOWED_DOMAINS nor CANYON_OIDC_ALLOWED_EMAILS", a)
		}
	}
	if (c.adminClaim == "") != (c.adminValue == "") {
		return nil, errors.New("CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE are set together or not at all")
	}
	if (c.responderClaim == "") != (c.responderValue == "") {
		return nil, errors.New("CANYON_RESPONDER_CLAIM and CANYON_RESPONDER_VALUE are set together or not at all")
	}
	if c.groupsClaim != "" && !plainText(c.groupsClaim, 256) {
		return nil, errors.New("CANYON_GROUPS_CLAIM is a claim name of 1 to 256 characters of plain text")
	}
	return c, nil
}

// memberOf is membership as design/AUTH.md §3 defines it: the union of the two lists, whichever
// carried them — the environment or the preferences row.
func memberOf(email string, domains, emails []string) bool {
	email = strings.ToLower(email)      // the lists are lower-cased at boot and at PUT; whatever stored the address may not have been
	if strings.Count(email, "@") != 1 { // x@evil.example@meridian.example is nobody's member
		return false
	}
	return slices.Contains(emails, email) || slices.Contains(domains, email[strings.LastIndex(email, "@")+1:])
}

func listOf(name, raw, kind string, ok func(string) bool) ([]string, error) {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if !ok(item) {
			return nil, fmt.Errorf("%s: %q is not %s", name, item, kind)
		}
		out = append(out, item)
	}
	return out, nil
}

// isDomain also refuses anything beyond ASCII: strings.ToLower is not a case fold, and the Kelvin
// sign lower-cases to a plain k, so a domain carrying one would match a listed ASCII domain.
func isDomain(s string) bool {
	return strings.Contains(s, ".") && !strings.ContainsAny(s, "@/ \t\n\r") && !strings.ContainsFunc(s, func(r rune) bool { return r > unicode.MaxASCII || hidesText(r) })
}

// isEmail refuses, as plain text does, a character that can hide or reorder text or forge a line: an
// address reaches audit rows, canyon share, the server's log and the deploy output (#2499).
func isEmail(s string) bool {
	at := strings.LastIndex(s, "@")
	return at > 0 && !strings.ContainsAny(s[:at], "@ \t\n\r,<>") && !strings.ContainsFunc(s[:at], hidesText) && isDomain(s[at+1:]) // a space is the comma typo; a newline forges a log line
}

// clockLeeway is how far behind its identity provider a Canyon host may run.
// bounded is the display name before it becomes a column: the id token's claims are whatever the
// issuer put in them. Only the display name, which names nobody, is cut; an identifier past the limit
// refuses the sign-in.
func bounded(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}

const clockLeeway = 30 * time.Second

// oneLine is an error for the log: the first 200 bytes, no line breaks.
func oneLine(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// discoveryClient bounds the fetch: ten seconds, and no redirect is followed, so an issuer that
// answers 3xx keeps the host unready rather than sending Canyon to a plain-http or internal address.
func discoveryClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// discovery is the provider's openid-configuration, the part Canyon reads. go-oidc (PR 4a) checks
// the issuer match and the signatures, not the endpoints' scheme, so that check lives here.
// discovery is the part of the provider's document Canyon reads back: go-oidc checks the issuer,
// the endpoint schemes are checked here.
type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// provider is the identity provider once discovery lands: go-oidc's provider, and a verifier over its
// remote key set. The keys are never copied at boot: the set fetches on first use and refetches when a
// token names a key id it has not seen, because every provider rotates on its own schedule.
type provider struct {
	stamped  atomic.Bool // a sign-in this process verified under the issuer has recorded it on the subjects with none (stampIssuer)
	mu       sync.Mutex
	client   *http.Client // the bounded client discovery ran with: the code exchange and the JWKS fetches use the same one
	oidc     *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

func (p *provider) discovered() bool {
	_, ok := p.endpoint()
	return ok
}

// endpoint is the issuer's authorization and token endpoints from the discovery document, and
// false until the document has landed.
func (p *provider) endpoint() (oauth2.Endpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oidc == nil {
		return oauth2.Endpoint{}, false
	}
	return p.oidc.Endpoint(), true
}

// discover fetches the document in the background. The fetch is not fatal: it is retried with bounded
// backoff, and until it lands /ready waits on oidc, because a provider having a bad ten minutes must
// not become a crash loop that takes out the fleet page and the audit trail with it.
func discover(ctx context.Context, client *http.Client, cfg *oidcConfig, logger *log.Logger) *provider {
	p := &provider{client: client}
	go func() {
		wait := time.Second
		for {
			prov, err := fetchDiscovery(ctx, client, cfg.issuer)
			if err == nil {
				p.mu.Lock()
				// go-oidc has no leeway of its own (exp and nbf are bare comparisons), and Entra emits nbf, so a
				// host a second behind its tenant would refuse every sign-in: verify at a clock thirty seconds back.
				p.oidc, p.verifier = prov, prov.Verifier(&oidc.Config{ClientID: cfg.clientID, Now: func() time.Time { return time.Now().Add(-clockLeeway) }})
				p.mu.Unlock()
				logger.Printf("canyon-server: oidc discovery from %s complete", cfg.issuer)
				return
			}
			logger.Printf("readiness: oidc: %s", oneLine(err.Error())) // go-oidc puts the response body in its error: bounded, and never a second line
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, 30*time.Second)
		}
	}()
	return p
}

// fetchDiscovery is one attempt through the bounded client, which go-oidc keeps for the key set too.
// The library refuses a document naming another issuer; the three endpoints being https it does not
// check, so that stays here: fail closed now, not in the PR that first reads them.
func fetchDiscovery(ctx context.Context, client *http.Client, issuer string) (*oidc.Provider, error) {
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, err
	}
	var doc discovery
	if err := p.Claims(&doc); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	for name, endpoint := range map[string]string{"authorization_endpoint": doc.AuthorizationEndpoint, "token_endpoint": doc.TokenEndpoint, "jwks_uri": doc.JWKSURI} {
		if u, err := neturl.Parse(endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("discovery document's %s is not an https URL", name)
		}
	}
	return p, nil
}

// claims is an ID token's payload once its signature is checked: what the readers below run over.
type claims map[string]any

// verifyIDToken checks the signature against the provider's keys, the issuer, the audience (the client
// id) and the expiry through go-oidc at a clock clockLeeway behind, then the nonce against the one the
// caller issued when the sign-in started: a token carrying any other nonce is a replay and is refused.
func (p *provider) verifyIDToken(ctx context.Context, raw, nonce string) (claims, error) {
	p.mu.Lock()
	v := p.verifier
	p.mu.Unlock()
	if v == nil {
		return nil, errors.New("oidc discovery has not completed")
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if nonce == "" || tok.Nonce != nonce {
		return nil, errors.New("id token nonce is not the one this sign-in issued")
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return nil, err
	}
	return c, nil
}

// emailOf names the person: the email claim, then preferred_username (Entra without the optional email
// claim sends only that), lower-cased, and it must have an address's shape: preferred_username is a
// UPN on an Entra guest and user-settable on some providers, and the value reaches the sign-in log.
// An email_verified that is present and not the boolean true names nobody, on either membership list;
// an absent one is accepted because Entra omits it. The second value is the claim that matched.
func emailOf(c claims) (email, matched string, err error) {
	if v, ok := c["email_verified"]; ok && v != true {
		return "", "", errors.New("email_verified is false")
	}
	for _, name := range []string{"email", "preferred_username"} {
		if s, _ := c[name].(string); strings.TrimSpace(s) != "" {
			// read before lower-casing, which folds the Kelvin sign into k: no address Canyon and the provider might fold apart
			if local, _, _ := strings.Cut(strings.TrimSpace(s), "@"); strings.ContainsFunc(local, func(r rune) bool { return r > unicode.MaxASCII }) {
				return "", "", fmt.Errorf("the %s claim's address is not ASCII before the @", name)
			}
			if s = strings.ToLower(strings.TrimSpace(s)); !isEmail(s) {
				return "", "", fmt.Errorf("the %s claim is not an email address", name)
			}
			return s, name, nil
		}
	}
	return "", "", errors.New("the id token carries neither email nor preferred_username")
}

// groupsClaimOf is the configured claim's groups (identity.Groups), with the word for the sign-in log:
// unset when no claim is configured.
func groupsClaimOf(c claims, name string) ([]string, string) {
	if name == "" {
		return []string{}, "unset"
	}
	raw, present := c[name]
	return identity.Groups(raw, present)
}

// adminClaimOf is the group test over the configured claim: membership for an array claim, equality for
// a string one. The word is for the sign-in log: hit, miss, absent (Entra sends no groups claim at all
// past its overage limit, so that person is not admin), or unset when no claim is configured.
func adminClaimOf(c claims, name, value string) string {
	if name == "" {
		return "unset"
	}
	switch v := c[name].(type) {
	case nil:
		return "absent"
	case string:
		if v == value {
			return "hit"
		}
	case []any:
		for _, item := range v {
			if item == value {
				return "hit"
			}
		}
	case map[string]any: // an object's keys are its names (Zitadel's project roles)
		if _, ok := v[value]; ok {
			return "hit"
		}
	}
	return "miss"
}

// loopbackOnly is DNS rebinding's answer under off and demo, where every caller is an admin: a page a
// browser loaded from any name can be pointed back at this loopback port, and its requests then carry
// that name as their Host. So only a loopback Host, or a name under localhost, which browsers resolve to
// loopback themselves and never ask DNS for (the apps' <app>.<env>.localhost), is served; anything else
// is 421, /ready included: a cluster's probe from the node is closed at accept anyway (localpeer.go). Under
// oidc sign-in decides, on any name.
func (a *app) loopbackOnly(next http.Handler) http.Handler {
	if a.auth == authOIDC {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackName(r.Host) {
			writeErr(w, http.StatusMisdirectedRequest, "misdirected", "sign-in is off, so this Canyon answers only at localhost", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackName is a Host header naming this machine: localhost or a name under it, or a loopback address,
// with or without a port and a final dot.
func loopbackName(host string) bool {
	if h, port, err := net.SplitHostPort(host); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil { // localhost:7777.evil.com splits, at its last colon
			return false
		}
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")), ".")
	return loopbackHost(host) || strings.HasSuffix(host, ".localhost")
}
