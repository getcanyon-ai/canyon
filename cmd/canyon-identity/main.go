// Command canyon-identity is the identity sidecar in every app's pod on Kubernetes
// (contracts/executor.md, The front door): it verifies who the front door says the person is, reads
// the app's access file, and hands the app Canyon's identity token. The app's port answers it alone.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/getcanyon-ai/canyon/internal/answered"
)

// frontDoor is /run's front_door as the executor hands it over in CANYON_FRONT_DOOR, without
// signin.client_secret, which oauth2-proxy alone reads: a field outside these refuses the start.
type frontDoor struct {
	Choice   int    `json:"choice"`
	Gateway  string `json:"gateway"`
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	JWKSURL  string `json:"jwks_url"`
	Cookie   string `json:"cookie"`
	ALBARN   string `json:"alb_arn"`
	Signin   struct {
		Issuer   string `json:"issuer"`
		ClientID string `json:"client_id"`
	} `json:"signin"`
	Domain string `json:"domain"`
	Form   string `json:"form"`
}

// config is the sidecar's environment, which the executor writes: the front door, and Canyon's public
// URL, the app and the target, which every token it mints carries. With CANYON_ACCESS_URL set the
// access file is pulled from there (design/HAND-OFF.md §5) under the release's token, the last one
// served through an outage for the grace, a restart included; without it the file is read.
type config struct {
	door                   frontDoor
	issuer, app, target    string
	accessURL, accessToken string
	accessGrace            time.Duration
}

// accessURLRe is the contract's https_url (contracts/schema/release.schema.json), the server's httpsRe
// (cmd/canyon-server/routes_front_door.go) character for character: https alone — the bearer never
// goes in clear — and no userinfo.
var accessURLRe = regexp.MustCompile(`^https://[^@/?#\s]+(/\S*)?$`)

func configOf(getenv func(string) string) (config, error) {
	c := config{issuer: getenv("CANYON_IDENTITY_ISSUER"), app: getenv("CANYON_IDENTITY_AUDIENCE"), target: getenv("CANYON_TARGET")}
	if !isURL(c.issuer) || c.app == "" || (c.target != "preview" && c.target != "prod") {
		return c, errors.New("CANYON_IDENTITY_ISSUER is Canyon's public URL, CANYON_IDENTITY_AUDIENCE the app and CANYON_TARGET preview or prod")
	}
	if c.accessURL = getenv("CANYON_ACCESS_URL"); c.accessURL != "" {
		c.accessToken = getenv("CANYON_ACCESS_TOKEN")
		grace, err := time.ParseDuration(getenv("CANYON_ACCESS_GRACE"))
		if !accessURLRe.MatchString(c.accessURL) || c.accessToken == "" || err != nil || grace < 0 || grace > 24*time.Hour {
			return c, errors.New("CANYON_ACCESS_URL is Canyon's access route over https, CANYON_ACCESS_TOKEN the release's token and CANYON_ACCESS_GRACE a duration from 0 to 24h")
		}
		c.accessGrace = grace
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(getenv("CANYON_FRONT_DOOR"))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c.door); err != nil {
		return c, fmt.Errorf("CANYON_FRONT_DOOR: %w", err)
	}
	switch d := c.door; {
	case d.Choice == 1 && d.Gateway == "oidc" && d.Issuer != "" && d.Audience != "" && d.JWKSURL != "" && d.Cookie != "":
	case d.Choice == 1 && d.Gateway == "alb" && albARN.MatchString(d.ALBARN):
		c.door.Cookie = cmp.Or(d.Cookie, "AWSELBAuthSessionCookie")
	case d.Choice == 2 && d.Gateway == "" && d.Signin.Issuer != "" && d.Signin.ClientID != "":
		c.door.Cookie = "_oauth2_proxy"
	default:
		return c, errors.New("CANYON_FRONT_DOOR is choice 1 with gateway oidc, issuer, audience, jwks_url and cookie, or gateway alb and an alb_arn in partition aws, or choice 2 with signin's issuer and client_id")
	}
	return c, nil
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// verifierOf is the check a request passes: choice 1 alb the load balancer's x-amzn-oidc-data; choice 1
// oidc the gateway's issuer, audience and key set; choice 2 signin's issuer and client id, and the key
// set its discovery names — which go-oidc checks names the issuer itself. An oidc key set is fetched
// now, so a sidecar that cannot reach it never starts.
func verifierOf(ctx context.Context, d frontDoor, client *http.Client) (verifier, error) {
	if d.Gateway == "alb" {
		return (&alb{arn: d.ALBARN, region: albARN.FindStringSubmatch(d.ALBARN)[1], client: client, timeout: 5 * time.Second}).verify, nil
	}
	issuer, audience, jwksURL := d.Issuer, d.Audience, d.JWKSURL
	if d.Choice == 2 {
		issuer, audience = d.Signin.Issuer, d.Signin.ClientID
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		_ = p.Claims(&doc) // the document NewProvider has parsed already
		if jwksURL, _ = doc["jwks_uri"].(string); jwksURL == "" {
			return nil, fmt.Errorf("%s names no jwks_uri", issuer)
		}
	}
	keys := &keySet{url: jwksURL, client: client}
	if err := keys.refresh(ctx); err != nil {
		return nil, err
	}
	return bearer(oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}}), issuer), nil
}

// startupWait bounds the start-up fetches (#2370): a new pod's first call through Canyon's egress gateway can be
// refused while egress-control learns who the pod is, a transient the sidecar outlives rather than crash on.
const startupWait = 10 * time.Second

// startVerifier is verifierOf, its fetches — a choice 2's discovery, an oidc key set — tried again, the wait between
// doubling from 250 ms to 2 s, each failure logged once, until they answer or wait has passed: then the last error,
// which stops the sidecar as one that cannot reach its key set never starts.
func startVerifier(d frontDoor, client *http.Client, logger *log.Logger, wait time.Duration) (verifier, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for backoff := 250 * time.Millisecond; ; backoff = min(2*backoff, 2*time.Second) {
		v, err := verifierOf(ctx, d, client)
		if err == nil || ctx.Err() != nil {
			return v, err
		}
		logger.Printf("canyon-identity: %.300s; trying again in %v", strings.Join(strings.Fields(err.Error()), " "), backoff) // go-oidc's error carries the body: one bounded line
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
	}
}

func main() {
	c, err := configOf(os.Getenv)
	if err != nil {
		log.Fatalf("canyon-identity: %v", err)
	}
	key, err := identity.Generate() // this pod's own: nothing distributed, no secret in the pod
	if err != nil {
		log.Fatalf("canyon-identity: key: %v", err)
	}
	jwks, err := key.JWKS()
	if err != nil {
		log.Fatalf("canyon-identity: key set: %v", err)
	}
	if proxy, token := os.Getenv("CANYON_IDENTITY_EGRESS_PROXY"), os.Getenv("CANYON_IDENTITY_EGRESS_TOKEN"); proxy != "" || token != "" { // Kubernetes, layer 3
		f, err := newForwarder(proxy, token, log.Default())
		if err != nil {
			log.Fatalf("canyon-identity: %v", err)
		}
		ln, err := net.Listen("tcp", forwardOn) // listening before the first fetch: this sidecar's own calls go out through it
		if err != nil {
			log.Fatalf("canyon-identity: forwarder: %v", err)
		}
		go func() {
			log.Fatal((&http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}).Serve(ln))
		}()
	}
	client := &http.Client{Timeout: 10 * time.Second}
	v, err := startVerifier(c.door, client, log.Default(), startupWait)
	if err != nil {
		log.Fatalf("canyon-identity: %v", err)
	}
	var access accessSource = &accessReader{path: "/etc/canyon/access.json"}
	if c.accessURL != "" {
		p := newAccessPuller(c, client)
		if err := startAccess(p, log.Default(), startupWait); err != nil { // as a key set that cannot be fetched: never starts
			log.Fatalf("canyon-identity: %v", err)
		}
		go p.run(context.Background(), time.Minute)
		access = p
	}
	listen := ":4180" // choice 1 faces the router; choice 2 answers oauth2-proxy in the pod alone
	if c.door.Choice == 2 {
		listen = "127.0.0.1:4181"
	}
	keys := http.NewServeMux()
	keys.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	go func() { // identity.jwks_url, the app's CANYON_IDENTITY_JWKS_URL
		log.Fatal((&http.Server{Addr: "127.0.0.1:4182", Handler: keys, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
	}()
	if host := os.Getenv("CANYON_APP_HOST"); host != "" { // a pod /run started: the executor asks what the app answered
		go func() {
			log.Fatal((&http.Server{Addr: ":4183", Handler: answering(host, "http://127.0.0.1:3000/"), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
		}()
	}
	log.Printf("canyon-identity: %s-%s, choice %d, on %s", c.app, c.target, c.door.Choice, listen)
	srv := &http.Server{Addr: listen, Handler: newSidecar(c, v, key, access, &url.URL{Scheme: "http", Host: "127.0.0.1:3000"}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(srv.ListenAndServe()) // no drain on SIGTERM: kubelet stops a native sidecar after the app
}

// answering is GET /answered alone (contracts/executor.md, What the app answered): one GET of app, the
// address host as its Host, whatever the request that asked carried — it takes nothing from its caller.
func answering(host, app string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /answered", func(w http.ResponseWriter, r *http.Request) {
		a := answered.Ask(r.Context(), nil, app, host)
		a.Host = host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a)
	})
	return mux
}
