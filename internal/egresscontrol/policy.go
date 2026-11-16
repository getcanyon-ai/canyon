// Package egresscontrol is egress-control (design/EGRESS.md §3.1–3.3; contracts/routes.md, The check):
// the check Envoy asks before a call out moves, answered from the policy egress-control pulls from the
// server. It serves envs and allowlists with no expiry, but forgets callers once the policy is more than
// 30 s old, so past 30 s without the server every call is unknown_caller. It keeps nothing across a start.
package egresscontrol

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/go-jose/go-jose/v4"

	"github.com/getcanyon-ai/canyon/contracts"
)

const (
	policyBytes = 64 << 20 // one policy; a larger answer is refused, whole
	leafBytes   = 64 << 10 // one leaf: its certificate, key and chain, 48 KiB at most by the schema
	format      = 2        // the highest format this egress-control reads, 2 a Kubernetes policy's (#2142); a higher one is refused, whole
)

type appEnv struct{ app, env string }

type envPolicy struct {
	mode    string
	allowed []string
	known   map[string]bool  // the names its records hold: no new name to the rate limit
	routes  map[string]route // by destination: what interception adds, none without a signing root
	// tokenOnly is the server's mark on Kubernetes, once a /run proved by token succeeded: its pods are named by their
	// token alone, never by address again (no downgrade)
	tokenOnly bool
}

// route is one value route as the policy names it (egress.schema.json, policy_route): the header it sets, how its
// value becomes that header, and the write its value was sealed under. Never the value.
type route struct {
	header, scheme string
	rev            int64
}

// held is one policy as egress-control decides with it: its version, who holds each address, each app env's
// mode, allowlist and known names, the platform destinations and the host's public URL, and the signing root and
// the inject destinations the interception listener is built from.
type held struct {
	epoch    string
	seq      int64
	public   string
	callers  map[string]appEnv
	envs     map[appEnv]envPolicy
	platform map[string]bool
	signing  string    // the signing root's fingerprint, "" while none is live
	inject   []string  // every destination an env's routes name, each once, sorted bytewise; none without a signing root
	kube     *kubeHost // a Kubernetes host's issuer, key set and each env's ServiceAccount; nil on Docker
	newNames int       // the new names an app env may call a minute across every replica, the admin's cap
	capless  bool      // the policy named no caps, a server from before #2691's: newNames is FleetNames, the default
}

func (h *held) version() string { return h.epoch + ":" + strconv.FormatInt(h.seq, 10) }

// parse refuses, whole, a policy that fails the fields of egress.schema.json's policy it knows, whose format
// is higher than it knows, that names an address or an app env twice, or that is another runtime's than kube, this
// egress-control's own: on Kubernetes one with no kubernetes or of format 1, on Docker one carrying kubernetes (#2142's
// review). A field it does not know is ignored.
func parse(raw []byte, kube bool) (*held, error) {
	if err := contracts.CheckEgressPolicy(raw); err != nil {
		return nil, err
	}
	var p struct {
		Format    int    `json:"format"`
		Epoch     string `json:"epoch"`
		Seq       int64  `json:"seq"`
		PublicURL string `json:"public_url"`
		Callers   []struct {
			Address string `json:"address"`
			App     string `json:"app"`
			Env     string `json:"env"`
		} `json:"callers"`
		Envs []struct {
			App       string   `json:"app"`
			Env       string   `json:"env"`
			Mode      string   `json:"mode"`
			Allowed   []string `json:"allowed"`
			Known     []string `json:"known"`
			Account   string   `json:"account"`
			TokenOnly bool     `json:"token_only"`
			Routes    []struct {
				Destination string `json:"destination"`
				Header      string `json:"header"`
				Scheme      string `json:"scheme"`
				Rev         int64  `json:"rev"`
			} `json:"routes"`
		} `json:"envs"`
		Platform    []string `json:"platform"`
		SigningRoot *string  `json:"signing_root"`
		Kubernetes  *struct {
			Issuer *string           `json:"issuer"`
			Keys   []jose.JSONWebKey `json:"keys"`
		} `json:"kubernetes"`
		Caps *struct {
			NewNames int `json:"new_names"`
		} `json:"caps"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Format > format:
		return nil, fmt.Errorf("format %d, and this egress-control reads %d", p.Format, format)
	case kube && p.Kubernetes == nil: // an older server's, which would name callers by address alone (#2142's review)
		return nil, errors.New("a policy with no kubernetes, and this egress-control is on Kubernetes")
	case !kube && p.Kubernetes != nil:
		return nil, errors.New("a policy carrying kubernetes, and this egress-control is on Docker")
	case kube && p.Format != 2:
		return nil, fmt.Errorf("a Kubernetes policy of format %d, not 2", p.Format)
	}
	h := &held{epoch: p.Epoch, seq: p.Seq, public: p.PublicURL, newNames: FleetNames, capless: p.Caps == nil,
		callers: map[string]appEnv{}, envs: map[appEnv]envPolicy{}, platform: map[string]bool{}}
	if p.Caps != nil { // its range the schema's: contracts.CheckEgressPolicy refused one out of it above, the policy whole
		h.newNames = p.Caps.NewNames
	}
	if p.SigningRoot != nil {
		h.signing = *p.SigningRoot
	}
	if p.Kubernetes != nil {
		h.kube = &kubeHost{keys: p.Kubernetes.Keys, accounts: map[string]appEnv{}}
		if p.Kubernetes.Issuer != nil {
			h.kube.issuer = *p.Kubernetes.Issuer
		}
	}
	inject := map[string]bool{}
	for _, c := range p.Callers {
		if _, twice := h.callers[c.Address]; twice {
			return nil, fmt.Errorf("the address %s named twice", c.Address)
		}
		h.callers[c.Address] = appEnv{c.App, c.Env}
	}
	for _, e := range p.Envs {
		ae := appEnv{e.App, e.Env}
		if _, twice := h.envs[ae]; twice {
			return nil, fmt.Errorf("the app env %s %s named twice", e.App, e.Env)
		}
		known := map[string]bool{}
		for _, n := range e.Known {
			known[n] = true
		}
		switch {
		case e.Account != "" && h.kube == nil:
			return nil, fmt.Errorf("the app env %s %s names a ServiceAccount on a host that is not Kubernetes", e.App, e.Env)
		case e.TokenOnly && e.Account == "":
			return nil, fmt.Errorf("the app env %s %s is token_only with no ServiceAccount", e.App, e.Env)
		case e.Account != "":
			if _, twice := h.kube.accounts[e.Account]; twice {
				return nil, fmt.Errorf("the ServiceAccount %s named twice", e.Account)
			}
			h.kube.accounts[e.Account] = ae
		}
		routes := map[string]route{}
		for _, r := range e.Routes {
			if h.signing == "" { // a policy names no route while no root is live: one that does intercepts nothing
				break
			}
			if _, twice := routes[r.Destination]; twice {
				return nil, fmt.Errorf("the app env %s %s names a route to %s twice", e.App, e.Env, r.Destination)
			}
			routes[r.Destination] = route{r.Header, r.Scheme, r.Rev}
			inject[r.Destination] = true
		}
		h.envs[ae] = envPolicy{e.Mode, e.Allowed, known, routes, e.TokenOnly}
	}
	for _, n := range p.Platform {
		h.platform[n] = true
	}
	h.inject = slices.Sorted(maps.Keys(inject))
	return h, nil
}

// What NewClient refuses, each the setting it names: the URL (or the pair it takes or refuses), the token's
// file, the pinned certificate.
var (
	ErrURL   = errors.New("the policy's URL")
	ErrToken = errors.New("the policy's token")
	ErrCert  = errors.New("the policy's pinned certificate")
)

// Client asks the server's GET /egress/policy (contracts/routes.md, Egress control): never through a proxy,
// never following a redirect.
type Client struct {
	URL       string // the policy's, http:// on Docker, https:// on Kubernetes
	TokenFile string // Kubernetes: the file holding egress-control's projected token, read again at every ask; empty on Docker
	HTTP      *http.Client
}

// NewClient asks rawURL. An https:// one needs the projected token's file, which it reads once here so a start
// without it fails, and the certificate the server minted, PEM, the only one it trusts (design/EGRESS.md §3.2);
// an http:// one takes neither.
func NewClient(rawURL, tokenFile string, certPEM []byte) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Path != "/egress/policy" {
		return nil, fmt.Errorf("%w: %q is not http(s)://<host>:<port>/egress/policy", ErrURL, rawURL)
	}
	transport := &http.Transport{Proxy: nil}
	c := &Client{URL: rawURL, TokenFile: tokenFile, HTTP: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	switch {
	case u.Scheme == "http" && tokenFile == "" && certPEM == nil:
		return c, nil
	case u.Scheme == "https" && tokenFile != "" && certPEM != nil:
	default:
		return nil, fmt.Errorf("%w: an https:// one takes the token's file and the pinned certificate, an http:// one neither", ErrURL)
	}
	if _, err := c.token(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrToken, err)
	}
	pinned := x509.NewCertPool()
	if !pinned.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("%w: no PEM certificate", ErrCert)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pinned, MinVersion: tls.VersionTLS13}
	return c, nil
}

func (c *Client) token() (string, error) {
	token, err := os.ReadFile(c.TokenFile)
	if err == nil && strings.TrimSpace(string(token)) == "" {
		err = fmt.Errorf("%s is empty", c.TokenFile)
	}
	return strings.TrimSpace(string(token)), err
}

// Fetch asks for the policy, saying the version held (have) and, for a refetch, the address no caller holds:
// the body of a 200, or nil for a 304, what it holds being current.
func (c *Client) Fetch(ctx context.Context, have, caller string) ([]byte, error) {
	q := url.Values{"have": {have}}
	if caller != "" {
		q.Set("caller", caller)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, policyBytes+1))
	if err == nil && len(raw) > policyBytes {
		err = fmt.Errorf("a policy over %d bytes", policyBytes)
	}
	return raw, err
}

// Leaf asks for destination's leaf (GET /egress/leaf), at the policy's listener: the body of a 200, which holds
// the leaf's key, so no error here carries a byte of it.
func (c *Client) Leaf(ctx context.Context, destination string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.URL, "/policy")+"/leaf?"+url.Values{"destination": {destination}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, leafBytes+1))
	if err == nil && len(raw) > leafBytes {
		err = fmt.Errorf("a leaf over %d bytes", leafBytes)
	}
	return raw, err
}

// RouteValue asks for the value of app env's route to destination (GET /egress/route-value), at the policy's
// listener: the body of a 200, which holds the value, so no error here carries a byte of it.
func (c *Client) RouteValue(ctx context.Context, app, env, destination string) ([]byte, error) {
	q := url.Values{"app": {app}, "env": {env}, "destination": {destination}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.URL, "/policy")+"/route-value?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err := c.authorize(req); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, valueBytes+1))
	if err == nil && len(raw) > valueBytes {
		err = fmt.Errorf("a route value over %d bytes", valueBytes)
	}
	return raw, err
}

// authorize carries egress-control's token on Kubernetes: Docker has none, its listener admits it (§3.2).
func (c *Client) authorize(req *http.Request) error {
	if c.TokenFile == "" {
		return nil
	}
	token, err := c.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// Report tells the server what Envoy lost (POST /egress/dropped), at the policy's listener: calls, Envoy's own
// count, nil for what no one counted. Any answer but its 204 leaves it not taken.
func (c *Client) Report(ctx context.Context, calls *uint64) error {
	body, err := json.Marshal(struct {
		Calls *uint64 `json:"calls"`
	}{calls})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.URL, "/policy")+"/dropped", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := c.authorize(req); err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	return nil
}
