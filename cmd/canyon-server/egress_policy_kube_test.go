package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// design/EGRESS.md §4.3: on a Kubernetes host the policy names every env's ServiceAccount, from before its first /run,
// its token_only mark as egress_envs holds it, and the cluster's issuer and key set as the server read them within
// keySetMaxAge, else null and none. A key set changed or forgotten moves the version, so no replica is told 304 for a
// version under another. On Docker the policy names none of them.
func TestPolicyNamesTheKeySetAndEachEnvsAccountOnKubernetes(t *testing.T) {
	h := outboundHost(t) // its deploy rows, under via canyon, name both envs
	h.exec(t, "UPDATE egress_envs SET token_only = true WHERE project_id = 'claims-portal' AND env = 'prod'")
	_, docker := policyAt(t, h.pool)
	if code, p := pullPolicy(t, docker, "canyon-egress-a", "have="); code != http.StatusOK || p.Kubernetes != nil || p.Format != 1 ||
		slices.ContainsFunc(p.Envs, func(e policyEnv) bool { return e.Account != "" }) {
		t.Fatalf("on Docker: %d %+v, want format 1, no key set and no account", code, p)
	}

	jwk := func(kid string) jose.JSONWebKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"}
	}
	var clock atomic.Pointer[time.Time]
	now := time.Now()
	clock.Store(&now)
	k := &kubeTokens{token: "/nonexistent/token", issuer: "https://kubernetes.default.svc.cluster.local", keys: []jose.JSONWebKey{jwk("k1")},
		read: now, fetched: now, now: func() time.Time { return *clock.Load() }, log: log.New(os.Stderr, "", 0)}
	s, _ := policyAt(t, h.pool)
	s.kube = k
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	code, p := pullPolicy(t, srv.URL, "canyon-egress-a", "have=")
	if code != http.StatusOK || p.Kubernetes == nil || deref(p.Kubernetes.Issuer) != k.issuer || len(p.Kubernetes.Keys) != 1 || p.Kubernetes.Keys[0].KeyID != "k1" {
		t.Fatalf("on Kubernetes: %d %+v, want the cluster's issuer and its one key", code, p.Kubernetes)
	}
	if p.Format != 2 { // an egress-control from before #2142 refuses it whole (#2142's review)
		t.Errorf("a Kubernetes policy of format %d, want 2", p.Format)
	}
	want := map[string]policyEnv{"preview": {Account: "canyon-app-claims-portal-preview"}, "prod": {Account: "canyon-app-claims-portal-prod", TokenOnly: true}}
	for _, e := range p.Envs {
		if w := want[e.Env]; e.App != "claims-portal" || e.Account != w.Account || e.TokenOnly != w.TokenOnly {
			t.Errorf("%s %s names %q, token_only %t; want %q, %t", e.App, e.Env, e.Account, e.TokenOnly, w.Account, w.TokenOnly)
		}
	}
	egressShape(t, "policy", policyBody(t, srv.URL, "canyon-egress-b", "have="))
	if code, _ := pullPolicy(t, srv.URL, "canyon-egress-a", "have="+versionOf(p)); code != http.StatusNotModified {
		t.Fatalf("nothing changed: %d, want 304", code)
	}

	k.mu.Lock()
	k.keys = append(k.keys, jwk("k2")) // the cluster rotates its key
	k.mu.Unlock()
	code, rotated := pullPolicy(t, srv.URL, "canyon-egress-a", "have="+versionOf(p))
	if code != http.StatusOK || rotated.Seq <= p.Seq || rotated.Kubernetes == nil || len(rotated.Kubernetes.Keys) != 2 {
		t.Fatalf("a key set changed: %d seq %d after %d, %+v; want a new version naming both keys", code, rotated.Seq, p.Seq, rotated.Kubernetes)
	}

	later := now.Add(keySetMaxAge + time.Minute) // past keySetMaxAge, and no read answers
	clock.Store(&later)
	code, forgotten := pullPolicy(t, srv.URL, "canyon-egress-a", "have="+versionOf(rotated))
	if code != http.StatusOK || forgotten.Seq <= rotated.Seq || forgotten.Kubernetes == nil || forgotten.Kubernetes.Issuer != nil || len(forgotten.Kubernetes.Keys) != 0 {
		t.Fatalf("a key set past keySetMaxAge: %d seq %d after %d, %+v; want a new version naming none", code, forgotten.Seq, rotated.Seq, forgotten.Kubernetes)
	}
}
