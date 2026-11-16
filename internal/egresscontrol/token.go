package egresscontrol

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// The caller on Kubernetes from layer 3 (design/EGRESS.md §4.3; contracts/routes.md, The check): a pod proves
// itself with its env's ServiceAccount's projected token, which its canyon-identity forwarder sends as
// Proxy-Authorization, verified here offline against the cluster's key set the policy carries (§14 decision 1).
const (
	tokenAudience = "canyon-egress"
	appsNamespace = "canyon-apps"
	// keysKept is how long the policy's key set verifies once the server last said the policy is current: the
	// server's own key set is trusted 10 minutes, and a projected token lives 10 minutes.
	keysKept = 10 * time.Minute
)

// kubeHost is a Kubernetes host's part of the policy: the cluster's issuer and key set, empty while the server
// has not read them, and the ServiceAccount each env's pods run as.
type kubeHost struct {
	issuer   string
	keys     []jose.JSONWebKey
	accounts map[string]appEnv
}

// keySet verifies a token's signature with the policy's keys alone: nothing is fetched.
type keySet []jose.JSONWebKey

func (k keySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return nil, err
	}
	kid := jws.Signatures[0].Header.KeyID
	for _, key := range k {
		if kid == "" || key.KeyID == kid {
			if payload, err := jws.Verify(&key); err == nil {
				return payload, nil
			}
		}
	}
	return nil, errors.New("no key of the cluster's key set verifies the token")
}

// caller is the token-proven env whose ServiceAccount header's token names: a bearer token the cluster's issuer
// signed for audience canyon-egress, live at now, of a ServiceAccount of canyon-apps the policy names, bound to a
// pod. Anything else names no one, and a key set the server has not read verifies nothing.
func (k *kubeHost) caller(header string, now time.Time) (appEnv, bool) {
	scheme, raw, _ := strings.Cut(strings.TrimSpace(header), " ")
	raw = strings.TrimSpace(raw)
	if !strings.EqualFold(scheme, "Bearer") || raw == "" || k.issuer == "" || len(k.keys) == 0 {
		return appEnv{}, false
	}
	verifier := oidc.NewVerifier(k.issuer, keySet(k.keys), &oidc.Config{ClientID: tokenAudience,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}, Now: func() time.Time { return now }})
	tok, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		return appEnv{}, false
	}
	account, ok := strings.CutPrefix(tok.Subject, "system:serviceaccount:"+appsNamespace+":")
	if !ok {
		return appEnv{}, false
	}
	var claims struct {
		Kubernetes struct {
			Namespace      string `json:"namespace"`
			ServiceAccount struct {
				Name string `json:"name"`
			} `json:"serviceaccount"`
			Pod struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	if tok.Claims(&claims) != nil || claims.Kubernetes.Namespace != appsNamespace || claims.Kubernetes.ServiceAccount.Name != account ||
		claims.Kubernetes.Pod.Name == "" || claims.Kubernetes.Pod.UID == "" { // a token bound to no pod proves no pod's call
		return appEnv{}, false
	}
	ae, named := k.accounts[account]
	return ae, named
}
