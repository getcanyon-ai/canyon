package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/identity"
)

const albArn = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/50dc6c495c0c9188"

var dana = map[string]any{"sub": "u-dana", "email": "dana@meridian.example", "email_verified": true, "name": "Dana Whitfield"}

// albHeader is x-amzn-oidc-data's JWT header as the load balancer writes it.
func albHeader(signer, kid string, exp time.Duration) map[string]any {
	return map[string]any{"alg": "ES256", "kid": kid, "signer": signer, "iss": "https://login.meridian.example", "client": "alb-client", "exp": time.Now().Add(exp).Unix()}
}

// albSigned is x-amzn-oidc-data as the load balancer writes it: ES256 over segments padded with =.
func albSigned(key *ecdsa.PrivateKey, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	input := base64.URLEncoding.EncodeToString(h) + "." + base64.URLEncoding.EncodeToString(p)
	digest := sha256.Sum256([]byte(input))
	r, s, _ := ecdsa.Sign(rand.Reader, key, digest[:])
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + base64.URLEncoding.EncodeToString(sig)
}

// albKeys serves the load balancer's key under eu-central-1 — k-alb, k-slow after 3 s, k-huge followed
// by a MiB — points the sidecar at it, and counts the fetches.
func albKeys(t *testing.T, key *ecdsa.PrivateKey) *atomic.Int32 {
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		switch r.URL.Path {
		case "/eu-central-1/k-slow":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		case "/eu-central-1/k-huge":
			_, _ = w.Write(append(pub, bytes.Repeat([]byte("\n"), 1<<20)...))
			return
		case "/eu-central-1/k-alb":
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pub)
	}))
	t.Cleanup(srv.Close)
	was := albKeysAt
	albKeysAt = srv.URL + "/%s/%s"
	t.Cleanup(func() { albKeysAt = was })
	return &fetches
}

// x-amzn-oidc-data is believed signed by this load balancer — its ARN, its region's key — unexpired and
// intact, with the key fetched once for its key id and never for one outside a path segment; nothing
// else carries a person in alb mode, and none of the load balancer's credentials reaches the app.
func TestALBDataIsBelievedFromItsSignerUnexpiredAndIntact(t *testing.T) {
	key := newKey()
	fetches := albKeys(t, key)
	r := newRig(t, `{"choice":1,"gateway":"alb","alb_arn":"`+albArn+`","domain":"meridian.example","form":"app.env.domain"}`)
	r.from = "https://login.meridian.example" // the load balancer signs in where Canyon does: its header names the issuer
	r.access(t, "2026-09-25T11:00:00Z", identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana", Level: "owner"})
	good := albSigned(key, albHeader(albArn, "k-alb", time.Minute), dana)
	otherIssuer := albHeader(albArn, "k-alb", time.Minute)
	otherIssuer["iss"] = "https://login.partner.example"
	if p := strings.Split(good, "."); !strings.HasSuffix(p[0], "=") || !strings.HasSuffix(p[1], "=") {
		t.Fatalf("the load balancer pads the segments it signs; this fixture does not: %s", good)
	}
	sam := strings.Split(albSigned(key, albHeader(albArn, "k-alb", time.Minute), map[string]any{"sub": "u-sam", "email": "sam@meridian.example"}), ".")
	for name, c := range map[string]struct {
		h    []string
		code int
	}{
		"its own":                      {[]string{"X-Amzn-Oidc-Data", good, "X-Amzn-Oidc-Accesstoken", "a", "Cookie", "AWSELBAuthSessionCookie-0=s; app=1"}, 200},
		"another ARN":                  {[]string{"X-Amzn-Oidc-Data", albSigned(key, albHeader(strings.Replace(albArn, "canyon", "other", 1), "k-alb", time.Minute), dana)}, 401},
		"expired":                      {[]string{"X-Amzn-Oidc-Data", albSigned(key, albHeader(albArn, "k-alb", -time.Second), dana)}, 401},
		"a tampered payload":           {[]string{"X-Amzn-Oidc-Data", sam[0] + "." + strings.Split(good, ".")[1] + "." + sam[2]}, 401},
		"another key":                  {[]string{"X-Amzn-Oidc-Data", albSigned(newKey(), albHeader(albArn, "k-alb", time.Minute), dana)}, 401},
		"a key id outside one segment": {[]string{"X-Amzn-Oidc-Data", albSigned(key, albHeader(albArn, "../k-alb", time.Minute), dana)}, 401},
		"in Authorization alone":       {[]string{"Authorization", "Bearer " + good}, 401},
		"another issuer's sign-in":     {[]string{"X-Amzn-Oidc-Data", albSigned(key, otherIssuer, dana)}, 302}, // dana's grant is bound: her subject is no other issuer's (#2220)
	} {
		if rec := r.do("GET", "/", "", c.h...); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(r.seen) != 1 {
		t.Fatalf("the app was reached %d times, want once", len(r.seen))
	}
	if got := <-r.seen; got.Get("X-Amzn-Oidc-Data")+got.Get("X-Amzn-Oidc-Accesstoken") != "" || got.Get("Cookie") != "app=1" || got.Get("Authorization") == "Bearer "+good {
		t.Errorf("a load balancer credential reached the app: %v", got)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("the key was fetched %d times, want once", n)
	}
	otherClient := albHeader(albArn, "k-alb", time.Minute)
	otherClient["client"] = "another-client"
	if rec := r.do("GET", "/", "", "X-Amzn-Oidc-Data", albSigned(key, otherClient, dana)); rec.Code != http.StatusOK {
		t.Fatalf("Canyon's issuer, another client: %d, want 200", rec.Code) // dana's subject is hers under the issuer, whatever its client (#2220)
	}
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	slash := albHeader(albArn, "k-alb", time.Minute)
	slash["iss"] = "https://login.meridian.example/"
	if rec := r.do("GET", "/", "", "X-Amzn-Oidc-Data", albSigned(key, slash, dana)); rec.Code != http.StatusFound {
		t.Errorf("Canyon's issuer with a / at its end: %d, want 302", rec.Code) // another issuer, as far as a signature proves: it may name other subjects
	}
	if want := fmt.Sprintf("a token from %q, not Canyon's sign-in issuer %q", "https://login.meridian.example/", "https://login.meridian.example"); !strings.Contains(logged.String(), want) {
		t.Errorf("the log names not both issuers, %q: %q", want, logged.String())
	}
	r.from = "" // a file naming no sign-in, a sign-in-off host's: a header naming none never compares with it (#2220)
	r.access(t, "2026-09-25T12:00:00Z", identity.Grant{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana", Level: "owner"})
	bare := albHeader(albArn, "k-alb", time.Minute)
	delete(bare, "iss")
	if rec := r.do("GET", "/", "", "X-Amzn-Oidc-Data", albSigned(key, bare, dana)); rec.Code != http.StatusFound {
		t.Errorf("a header naming no sign-in, a file naming none: %d, want 302", rec.Code)
	}
	if rec := r.do("GET", "/", "", "X-Amzn-Oidc-Data", good); rec.Code != http.StatusFound {
		t.Errorf("Canyon's issuer, a file naming none: %d, want 302", rec.Code)
	}
}

// An oidc sidecar believes Authorization alone: a token its gateway signed, in x-amzn-oidc-data, is 401.
func TestAnOIDCSidecarIgnoresALBData(t *testing.T) {
	r := newRig(t, choice1)
	if rec := r.do("GET", "/", "", "X-Amzn-Oidc-Data", r.g.token("dana@meridian.example")); rec.Code != http.StatusUnauthorized || len(r.seen) != 0 {
		t.Fatalf("%d, the app reached %d times", rec.Code, len(r.seen))
	}
}

// A key that is slow to come or over the bound is no key, and costs no more than the bound.
func TestTheLoadBalancersKeyFetchIsBoundedInTimeAndSize(t *testing.T) {
	key := newKey()
	albKeys(t, key)
	a := &alb{arn: albArn, region: "eu-central-1", client: http.DefaultClient, timeout: 200 * time.Millisecond}
	for _, kid := range []string{"k-slow", "k-huge"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Amzn-Oidc-Data", albSigned(key, albHeader(albArn, kid, time.Minute), dana))
		start := time.Now()
		if _, _, err := a.verify(req); err == nil || time.Since(start) > time.Second {
			t.Errorf("%s: %v after %s", kid, err, time.Since(start))
		}
	}
}
