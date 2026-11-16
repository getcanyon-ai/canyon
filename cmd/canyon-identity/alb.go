package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// albARN is an application load balancer's ARN in partition aws, its region the one group.
	albARN = regexp.MustCompile(`^arn:aws:elasticloadbalancing:([a-z0-9-]+):[0-9]{12}:loadbalancer/app/[A-Za-z0-9-]+/[0-9a-f]+$`)
	// albKid is a key id the key URL takes: one path segment of letters, digits and dashes.
	albKid = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// albKeysAt is where the load balancer's public keys are, by region and key id.
	albKeysAt = "https://public-keys.auth.elb.%s.amazonaws.com/%s"
)

const maxALBKey = 8 << 10 // a P-256 public key's PEM is under 200 bytes

// alb is choice 1's AWS application load balancer (contracts/executor.md, The front door): the
// x-amzn-oidc-data it signs and nothing else. Its claims are the identity provider's user-info answer.
type alb struct {
	arn, region string
	client      *http.Client
	timeout     time.Duration // one key's fetch
	keys        sync.Map      // key id → *ecdsa.PublicKey, fetched once
	fetching    sync.Mutex    // one fetch at a time; a cached key never waits on one
}

func (a *alb) verify(r *http.Request) (map[string]any, string, error) {
	values := r.Header.Values("X-Amzn-Oidc-Data")
	parts := strings.Split(strings.Join(values, ""), ".")
	if len(values) != 1 || len(parts) != 3 {
		return nil, "", errors.New("no x-amzn-oidc-data")
	}
	var header struct {
		Alg    string `json:"alg"`
		Kid    string `json:"kid"`
		Signer string `json:"signer"`
		Exp    int64  `json:"exp"`
		Iss    string `json:"iss"` // the identity provider's issuer
	}
	var claims map[string]any
	sig, err := segment(parts[2])
	if err == nil {
		err = decode(parts[0], &header)
	}
	if err == nil {
		err = decode(parts[1], &claims)
	}
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("x-amzn-oidc-data: %w", err)
	case header.Alg != "ES256" || len(sig) != 64:
		return nil, "", errors.New("x-amzn-oidc-data is not ES256")
	case header.Signer != a.arn:
		return nil, "", fmt.Errorf("x-amzn-oidc-data is signed by %q", header.Signer)
	case time.Now().Unix() >= header.Exp:
		return nil, "", errors.New("x-amzn-oidc-data has expired")
	case !albKid.MatchString(header.Kid): // before any fetch: the key id is a path segment
		return nil, "", fmt.Errorf("x-amzn-oidc-data names key %q", header.Kid)
	}
	key, err := a.key(r.Context(), header.Kid)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1])) // the segments as the load balancer signed them, padding and all
	if !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, "", errors.New("x-amzn-oidc-data's signature does not verify")
	}
	return claims, header.Iss, nil
}

// segment decodes one of the JWT's segments: the load balancer pads them with =, which is removed, and
// nothing else is relaxed.
func segment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func decode(s string, v any) error {
	b, err := segment(s)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	return err
}

// key is the load balancer's public key kid, a PEM from its region's key URL — the fetch bounded in time
// and to maxALBKey bytes — cached once it parses as P-256.
func (a *alb) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	if k, ok := a.keys.Load(kid); ok {
		return k.(*ecdsa.PublicKey), nil
	}
	a.fetching.Lock()
	defer a.fetching.Unlock()
	if k, ok := a.keys.Load(kid); ok { // fetched while this request waited
		return k.(*ecdsa.PublicKey), nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(albKeysAt, a.region, kid), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxALBKey+1))
	if err == nil && (resp.StatusCode != http.StatusOK || len(b) > maxALBKey) {
		err = fmt.Errorf("%s, over %d bytes: %t", resp.Status, maxALBKey, len(b) > maxALBKey)
	}
	var pub any
	if block, _ := pem.Decode(b); err == nil && block == nil {
		err = errors.New("no PEM")
	} else if err == nil {
		pub, err = x509.ParsePKIXPublicKey(block.Bytes)
	}
	k, ok := pub.(*ecdsa.PublicKey)
	if err == nil && (!ok || k.Curve != elliptic.P256()) {
		err = errors.New("not a P-256 key")
	}
	if err != nil {
		return nil, fmt.Errorf("the load balancer's key %s: %w", kid, err)
	}
	a.keys.Store(kid, k)
	return k, nil
}
