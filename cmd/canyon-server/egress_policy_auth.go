package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/vault"
)

// How egress-control proves itself on Kubernetes (design/EGRESS.md §3.2): Authorization: Bearer and a projected
// token of its ServiceAccount, audience canyon-egress-policy, verified offline against the cluster's
// service-account key set, over TLS under a certificate the server mints and egress-control pins.
const (
	egressPolicyAudience = "canyon-egress-policy"
	egressControlAccount = "system:serviceaccount:canyon:canyon-egress"
	egressPolicyHost     = "canyon-server.canyon.svc" // the certificate's one name: the server's Service
)

// keySetMaxAge is how long a key set read is trusted: a key the cluster retires stops verifying within it.
const keySetMaxAge = 10 * time.Minute

// kubeTokens verifies egress-control's tokens against the cluster's issuer and key set, read from the API
// server with the server's own projected token (its default issuer-discovery access, nothing more): read again
// once the last read is keySetMaxAge old, or when a token names a kid it does not hold, at most once a minute.
// A key set it has not read within keySetMaxAge verifies nothing: every request is 401.
type kubeTokens struct {
	api     string // https://kubernetes.default.svc
	token   string // the server's own token's file, read at each fetch: the kubelet rotates it
	client  *http.Client
	now     func() time.Time // time.Now when nil, a test's when set
	log     *log.Logger      // the policy server's
	mu      sync.Mutex
	issuer  string
	keys    []jose.JSONWebKey
	fetched time.Time // the last read tried
	read    time.Time // the last read that answered
}

// clock is k.now, else time.Now.
func (k *kubeTokens) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// fresh reads the key set again when it is due — keySetMaxAge old, or due, a kid unknown — at most once a
// minute, and forgets it once it is keySetMaxAge old; the caller holds mu.
func (k *kubeTokens) fresh(ctx context.Context, due bool) {
	if (due || k.clock().Sub(k.read) >= keySetMaxAge) && k.clock().Sub(k.fetched) >= time.Minute {
		if err := k.refresh(ctx); err != nil {
			k.log.Printf("canyon-server: egress policy: %v", err)
		}
	}
	if k.clock().Sub(k.read) >= keySetMaxAge {
		k.issuer, k.keys = "", nil
	}
}

// policy is the key set as the policy names it (§4.3): the issuer and keys read within keySetMaxAge, else null and
// none, so no pod's token verifies.
func (k *kubeTokens) policy(ctx context.Context) *policyKube {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fresh(ctx, false)
	return &policyKube{Issuer: nullable(k.issuer != "", k.issuer), Keys: append([]jose.JSONWebKey{}, k.keys...)}
}

// replica is the pod a valid token names, the token egress-control's ServiceAccount's for this audience.
func (k *kubeTokens) replica(r *http.Request) (string, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return "", false
	}
	k.mu.Lock()
	k.fresh(r.Context(), false)
	issuer := k.issuer
	k.mu.Unlock()
	if issuer == "" {
		return "", false
	}
	tok, err := oidc.NewVerifier(issuer, k, &oidc.Config{ClientID: egressPolicyAudience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}, Now: k.clock}).Verify(r.Context(), raw)
	if err != nil || tok.Subject != egressControlAccount {
		return "", false
	}
	var claims struct {
		Kubernetes struct {
			Pod struct {
				Name string `json:"name"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	if tok.Claims(&claims) != nil || claims.Kubernetes.Pod.Name == "" || len(claims.Kubernetes.Pod.Name) > 253 {
		return "", false // a token bound to no pod names no replica
	}
	return claims.Kubernetes.Pod.Name, true
}

func (k *kubeTokens) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return nil, err
	}
	kid := jws.Signatures[0].Header.KeyID
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fresh(ctx, !slices.ContainsFunc(k.keys, func(key jose.JSONWebKey) bool { return key.KeyID == kid }))
	for _, key := range k.keys {
		if kid == "" || key.KeyID == kid {
			if payload, err := jws.Verify(&key); err == nil {
				return payload, nil
			}
		}
	}
	return nil, errors.New("no key of the cluster's key set verifies the token")
}

// refresh reads the issuer and the key set, 1 MiB each at most; the caller holds mu. Either failing changes
// nothing: what was read before stays until it is keySetMaxAge old (fresh).
func (k *kubeTokens) refresh(ctx context.Context) error {
	k.fetched = k.clock()
	token, err := os.ReadFile(k.token)
	if err != nil {
		return err
	}
	get := func(path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.api+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := k.client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if err = errors.New(resp.Status); resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	var set jose.JSONWebKeySet
	if err := get("/.well-known/openid-configuration", &doc); err != nil {
		return err
	}
	if err := get("/openid/v1/jwks", &set); err != nil {
		return err
	}
	if doc.Issuer == "" {
		return errors.New("/.well-known/openid-configuration names no issuer")
	}
	k.issuer, k.keys, k.read = doc.Issuer, set.Keys, k.fetched
	return nil
}

// kubeTokensOf reads the cluster's CA from the server's projected volume: a server that cannot trust the API
// server does not start.
func kubeTokensOf(dir string, logger *log.Logger) (*kubeTokens, error) {
	ca, err := os.ReadFile(dir + "/ca.crt") //nolint:gosec // the server's own projected volume, never named by a request
	if err != nil {
		return nil, fmt.Errorf("CANYON_EGRESS_POLICY_ADDR on Kubernetes reads the cluster's key set with the token and CA mounted at %s: %w", dir, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s/ca.crt holds no certificate", dir)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return &kubeTokens{api: "https://kubernetes.default.svc", token: dir + "/token", client: client, log: logger}, nil
}

// policyCert is the certificate the policy's listener serves on Kubernetes: the egress_policy_cert row (0096),
// its key opened from the vault, or on the first boot that finds none a new one, its key sealed as a secret is
// under ("egress", "policy", "tls_key"). The table lock makes two first boots write one.
func policyCert(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (tls.Certificate, error) {
	var out tls.Certificate
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "LOCK TABLE egress_policy_cert IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		var certPEM string
		var version int
		var sealedKey, sealedValue []byte
		err := tx.QueryRow(ctx, "SELECT cert, key_ref, sealed_key, sealed_value FROM egress_policy_cert").Scan(&certPEM, &version, &sealedKey, &sealedValue)
		if errors.Is(err, pgx.ErrNoRows) {
			var der []byte
			if certPEM, der, err = mintPolicyCert(time.Now()); err != nil {
				return err
			}
			if version, sealedKey, sealedValue, err = keys.Seal("egress", "policy", "tls_key", der); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "INSERT INTO egress_policy_cert (cert, key_ref, sealed_key, sealed_value) VALUES ($1, $2, $3, $4)", certPEM, version, sealedKey, sealedValue)
		}
		if err != nil {
			return err
		}
		der, err := keys.Open(version, "egress", "policy", "tls_key", sealedKey, sealedValue)
		if err != nil {
			return err
		}
		out, err = tls.X509KeyPair([]byte(certPEM), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		return err
	})
	return out, err
}

// mintPolicyCert makes the policy listener's certificate, self-signed P-256 for egressPolicyHost, ten years:
// egress-control pins it whole, so it is its own root. It answers the certificate's PEM and the key's PKCS #8.
func mintPolicyCert(now time.Time) (string, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: egressPolicyHost},
		DNSNames:     []string{egressPolicyHost},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", nil, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})), der, nil
}

// printPolicyCert is canyon-server egress-policy-cert: the certificate's PEM, which the install step publishes
// as the ConfigMap egress-control pins (deploy/k8s/README.md). Public: it reads no key.
func printPolicyCert(ctx context.Context, pool *pgxpool.Pool, out io.Writer) error {
	var cert string
	err := pool.QueryRow(ctx, "SELECT cert FROM egress_policy_cert").Scan(&cert)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("egress-policy-cert: no certificate yet: the server mints it at its first start with CANYON_EGRESS_POLICY_ADDR on Kubernetes")
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, cert)
	return err
}
