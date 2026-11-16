package egresscontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"slices"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/go-jose/go-jose/v4"
	"google.golang.org/grpc/codes"
)

const (
	clusterIssuer  = "https://kubernetes.default.svc.cluster.local"
	previewAccount = "canyon-app-claims-portal-preview" // claims-portal preview's, token_only
	prodAccount    = "canyon-app-claims-portal-prod"    // claims-portal prod's, not marked: its pods' address names it too
	podName        = "claims-portal-preview-6c9f7d8b4-x2k7q"
)

// cluster is a cluster's service-account signer: its key, and its public half as a key set names it.
type cluster struct {
	key *ecdsa.PrivateKey
	kid string
}

func newCluster(t *testing.T, kid string) cluster {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return cluster{key, kid}
}

func (c cluster) public(t *testing.T) map[string]any {
	t.Helper()
	var jwk map[string]any
	if err := json.Unmarshal(marshal(t, jose.JSONWebKey{Key: &c.key.PublicKey, KeyID: c.kid, Algorithm: "ES256", Use: "sig"}), &jwk); err != nil {
		t.Fatal(err)
	}
	return jwk
}

// projected is a pod's projected token as the kubelet writes one for account, live from at for 10 minutes, bound
// to its pod; change edits its claims before it is signed.
func (c cluster) projected(t *testing.T, account string, at time.Time, change func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{"iss": clusterIssuer, "sub": "system:serviceaccount:canyon-apps:" + account, "aud": []string{"canyon-egress"},
		"iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(10 * time.Minute).Unix(),
		"kubernetes.io": map[string]any{"namespace": "canyon-apps",
			"serviceaccount": map[string]string{"name": account, "uid": "8c1e4f2a-31b9-4d7e-9a0c-5b6e7f8a9b0c"},
			"pod":            map[string]string{"name": podName, "uid": "4f2a9c1e-77d3-4b8a-a1e2-0c9d8e7f6a5b"}}}
	if change != nil {
		change(claims)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: c.key, KeyID: c.kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(marshal(t, claims))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// kubePolicy is a Kubernetes host's policy under signing: claims-portal preview token_only, its pods named by their
// token of previewAccount alone, and prod not marked, named by a token of prodAccount or by its pods' address, each
// with a route to api.stripe.com, and the cluster's issuer and keys — none read when keys is nil.
func kubePolicy(t *testing.T, seq int, signing string, keys []map[string]any) []byte {
	t.Helper()
	route := []map[string]any{{"destination": "api.stripe.com", "header": "authorization", "scheme": "bearer", "rev": 7}}
	var issuer, root any = clusterIssuer, signing
	if keys == nil {
		issuer, keys = nil, []map[string]any{}
	}
	if signing == "" {
		root = nil
	}
	return marshal(t, map[string]any{"format": 2, "epoch": epoch, "seq": seq, "public_url": "https://canyon.meridian.example",
		"callers": []map[string]string{{"address": preview, "app": "claims-portal", "env": "preview"}, {"address": prod, "app": "claims-portal", "env": "prod"}},
		"envs": []map[string]any{
			{"app": "claims-portal", "env": "preview", "mode": "allowlist", "allowed": []string{"api.stripe.com"}, "known": []string{}, "account": previewAccount, "token_only": true, "routes": route},
			{"app": "claims-portal", "env": "prod", "mode": "allowlist", "allowed": []string{"api.stripe.com"}, "known": []string{}, "account": prodAccount, "routes": route}},
		"platform": []string{"login.example-idp.com"}, "signing_root": root,
		"kubernetes": map[string]any{"issuer": issuer, "keys": keys}})
}

// onKube is a Kubernetes replica holding policy.
func onKube(t *testing.T, policy []byte) replica {
	t.Helper()
	r := newReplicaOn(t, 1, &server{policy: policy}, true)
	if err := r.pull(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return r
}

// proxied is ext_authz #1's check of a CONNECT from source to authority carrying auth as Proxy-Authorization, none
// when empty.
func (r replica) proxied(t *testing.T, source, auth, authority string) verdict {
	t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["proxy-authorization"] = auth
	}
	resp, err := r.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: source, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 41234}}}}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Id: "9", Method: "CONNECT", Host: authority, Headers: headers}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return verdictOf(t, resp)
}

func callerIs(t *testing.T, what string, v verdict, env string) {
	t.Helper()
	if v.code != int32(codes.OK) || v.metadata["app"] != "claims-portal" || v.metadata["env"] != env {
		t.Errorf("%s: %d %q %v, want claims-portal %s's call let through", what, v.code, v.reason, v.metadata, env)
	}
}

func unknownCaller(t *testing.T, what string, v verdict) {
	t.Helper()
	if v.code != int32(codes.PermissionDenied) || v.reason != "unknown_caller" || v.metadata["app"] != nil {
		t.Errorf("%s: %d %q %v, want unknown_caller and no app", what, v.code, v.reason, v.metadata)
	}
}

// design/EGRESS.md §4.3, contracts/routes.md The check: on Kubernetes a valid projected token names its env from any
// address, token_only or not, and the OK takes the header off the call; a token_only env is its pods' token's alone,
// its own pod address with no token unknown_caller (no downgrade), while an env not marked is its pods' address's
// too. A token that fails in any way is unknown_caller, and never its address's, an unmarked env's address included.
func TestATokenOnlyEnvIsItsPodsTokenAlone(t *testing.T) {
	k := newCluster(t, "k1")
	r := onKube(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}))
	now := r.clock.now()
	good := "Bearer " + k.projected(t, previewAccount, now.Add(-time.Minute), nil)

	v := r.proxied(t, preview, good, "api.stripe.com:443")
	callerIs(t, "preview's token from its pod", v, "preview")
	if !slices.Contains(v.remove, "proxy-authorization") {
		t.Errorf("the OK removes %v, want proxy-authorization among them: the token never moves past the check", v.remove)
	}
	callerIs(t, "preview's token from an address the policy names for no one", r.proxied(t, nobody, good, "api.stripe.com:443"), "preview")
	callerIs(t, "the scheme in another case", r.proxied(t, preview, "bearer "+good[len("Bearer "):], "api.stripe.com:443"), "preview")

	unknownCaller(t, "preview's own pod address with no token", r.proxied(t, preview, "", "api.stripe.com:443"))
	other := newCluster(t, "k1")
	for what, auth := range map[string]string{ //nolint:gosec // tokens the test signs, each refused
		"another cluster's key":       "Bearer " + other.projected(t, previewAccount, now.Add(-time.Minute), nil),
		"expired":                     "Bearer " + k.projected(t, previewAccount, now.Add(-11*time.Minute), nil),
		"another audience":            "Bearer " + k.projected(t, previewAccount, now, func(c map[string]any) { c["aud"] = []string{"https://kubernetes.default.svc"} }),
		"another issuer":              "Bearer " + k.projected(t, previewAccount, now, func(c map[string]any) { c["iss"] = "https://oidc.eks.example" }),
		"a ServiceAccount none names": "Bearer " + k.projected(t, "canyon-app-intake-bot-prod", now, nil),
		"another namespace":           "Bearer " + k.projected(t, previewAccount, now, func(c map[string]any) { c["sub"] = "system:serviceaccount:canyon:" + previewAccount }),
		"bound to no pod": "Bearer " + k.projected(t, previewAccount, now, func(c map[string]any) {
			delete(c["kubernetes.io"].(map[string]any), "pod")
		}),
		"its claim another account": "Bearer " + k.projected(t, previewAccount, now, func(c map[string]any) {
			c["kubernetes.io"].(map[string]any)["serviceaccount"] = map[string]string{"name": prodAccount, "uid": "x"}
		}),
		"basic credentials":         "Basic Y2FueW9uOmNhbnlvbg==",
		"no token after the scheme": "Bearer ",
		"not a token":               "Bearer not.a.token",
	} {
		unknownCaller(t, what+", from preview's pod", r.proxied(t, preview, auth, "api.stripe.com:443"))
		unknownCaller(t, what+", from prod's pod, which its address names when it carries no token", r.proxied(t, prod, auth, "api.stripe.com:443"))
	}
	callerIs(t, "prod, not marked, by its pod's address with no token", r.proxied(t, prod, "", "api.stripe.com:443"), "prod")
	prodToken := "Bearer " + k.projected(t, prodAccount, now.Add(-time.Minute), nil)
	callerIs(t, "prod, not marked, by its new pod's token", r.proxied(t, prod, prodToken, "api.stripe.com:443"), "prod")
	callerIs(t, "prod's token from an address the policy names for no one", r.proxied(t, nobody, prodToken, "api.stripe.com:443"), "prod")
	callerIs(t, "prod's token from preview's pod address: the token, never the address", r.proxied(t, preview, prodToken, "api.stripe.com:443"), "prod")
}

// The key set is the policy's (§14 decision 1, offline): a policy with none read, or one past keysKept since the
// server last said it is current, verifies no token, and a token_only env is then unknown_caller, never its pods'
// address's; an env not marked is still its pods' address's.
func TestNoKeySetVerifiesNoToken(t *testing.T) {
	k := newCluster(t, "k1")
	unread := onKube(t, kubePolicy(t, 42, "", nil))
	token := "Bearer " + k.projected(t, previewAccount, unread.clock.now().Add(-time.Minute), nil)
	unknownCaller(t, "no key set read: preview's token", unread.proxied(t, preview, token, "api.stripe.com:443"))
	unknownCaller(t, "no key set read: preview's pod with no token", unread.proxied(t, preview, "", "api.stripe.com:443"))
	callerIs(t, "no key set read: prod, not marked, by its pod's address", unread.proxied(t, prod, "", "api.stripe.com:443"), "prod")

	held := onKube(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}))
	callerIs(t, "a key set the server just served", held.proxied(t, preview, token, "api.stripe.com:443"), "preview")
	held.clock.add(keysKept + time.Second)
	unknownCaller(t, "a key set past keysKept without the server", held.proxied(t, preview, token, "api.stripe.com:443"))
	unknownCaller(t, "past keysKept, preview's pod with no token", held.proxied(t, preview, "", "api.stripe.com:443"))
}

// A token caller is never trusted longer than an address caller (§3.2's 30 s): once the policy is older than
// callersKept without the server, a valid token is unknown_caller too, its env's allowlist, routes and hold left
// unread, and the moment the server answers it is named again.
func TestATokenCallerIsForgottenWithTheCallers(t *testing.T) {
	k := newCluster(t, "k1")
	r := onKube(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}))
	r.srv.set(true, nil)
	r.clock.add(callersKept)
	token := "Bearer " + k.projected(t, previewAccount, r.clock.now().Add(-time.Minute), nil)
	callerIs(t, "a policy callersKept old", r.proxied(t, preview, token, "api.stripe.com:443"), "preview")
	r.clock.add(time.Second)
	unknownCaller(t, "a valid token, the policy 31 s old", r.proxied(t, preview, token, "api.stripe.com:443"))
	unknownCaller(t, "a valid token from an address the policy names for no one, the policy 31 s old", r.proxied(t, nobody, token, "api.stripe.com:443"))
	r.srv.set(false, nil)
	r.poll(context.Background())
	callerIs(t, "the server answers again", r.proxied(t, preview, token, "api.stripe.com:443"), "preview")
}

// A ServiceAccount is named on a Kubernetes host alone, each once, and token_only marks an env that names one.
func TestAPolicyNamesAServiceAccountOnKubernetesAloneAndOnce(t *testing.T) {
	k := newCluster(t, "k1")
	twice := policyEdit(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}), func(p map[string]any) {
		p["envs"].([]any)[1].(map[string]any)["account"] = previewAccount
	})
	if _, err := parse(twice, true); err == nil {
		t.Error("a ServiceAccount named for two envs was taken")
	}
	docker := policyEdit(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}), func(p map[string]any) { delete(p, "kubernetes") })
	if _, err := parse(docker, false); err == nil {
		t.Error("a ServiceAccount on a host that is not Kubernetes was taken")
	}
	unnamed := policyEdit(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}), func(p map[string]any) {
		delete(p["envs"].([]any)[0].(map[string]any), "account")
	})
	if _, err := parse(unnamed, true); err == nil {
		t.Error("a token_only env with no ServiceAccount was taken")
	}
}

// #2142's review, B1: egress-control's runtime is its own setting, never the policy's. A Kubernetes replica refuses,
// whole, a policy with no kubernetes, an older server's (a rollback of the server alone, or a gateway upgraded first),
// and the one it holds serves on. Holding none, it names no caller, so an address-only CONNECT to a route's
// destination is unknown_caller and never intercepted. A Docker replica refuses a policy carrying kubernetes.
func TestAPolicyOfTheOtherRuntimeIsRefusedWhole(t *testing.T) {
	k := newCluster(t, "k1")
	signing := newPKI(t).fingerprint
	kube := kubePolicy(t, 42, signing, []map[string]any{k.public(t)})
	older := policyEdit(t, kube, func(p map[string]any) { // the policy a server from before #2142 serves the same host
		delete(p, "kubernetes")
		p["format"] = 1
		for _, e := range p["envs"].([]any) {
			delete(e.(map[string]any), "account")
			delete(e.(map[string]any), "token_only")
		}
	})
	r := onKube(t, kube)
	r.srv.set(false, older)
	if err := r.pull(context.Background(), ""); err == nil {
		t.Error("a Kubernetes replica took a policy with no kubernetes")
	}
	unknownCaller(t, "the held policy serves on: preview, token_only, by its pod's address", r.proxied(t, preview, "", "api.stripe.com:443"))

	fresh := newReplicaOn(t, 1, &server{policy: older}, true)
	if err := fresh.pull(context.Background(), ""); err == nil {
		t.Error("a Kubernetes replica holding nothing took a policy with no kubernetes")
	}
	v := fresh.proxied(t, prod, "", "api.stripe.com:443")
	unknownCaller(t, "a Kubernetes replica that refused an older server's policy, prod's pod address with no token", v)
	if v.set["x-canyon-intercept"] != "" {
		t.Errorf("an address-only CONNECT to a route's destination was intercepted: %v", v.set)
	}

	unkeyed := policyEdit(t, older, func(p map[string]any) { p["format"] = 2 }) // the format check aside: no kubernetes is refused on its own
	bare := newReplicaOn(t, 1, &server{policy: unkeyed}, true)
	if err := bare.pull(context.Background(), ""); err == nil {
		t.Error("a Kubernetes replica took a policy of format 2 with no kubernetes")
	}
	if v := bare.proxied(t, prod, "", "api.stripe.com:443"); v.reason != "unknown_caller" || v.set["x-canyon-intercept"] != "" {
		t.Errorf("a Kubernetes replica offered a format-2 policy with no kubernetes: %d %q %v, want unknown_caller and nothing intercepted", v.code, v.reason, v.set)
	}

	docker := newReplicaOn(t, 1, &server{policy: kube}, false)
	if err := docker.pull(context.Background(), ""); err == nil {
		t.Error("a Docker replica took a policy carrying kubernetes")
	}
}

// #2142's review, B2: a policy carrying kubernetes is format 2, so an egress-control from before #2142, which reads 1
// alone, refuses it whole (TestAPolicyOutOfShapeIsRefusedWhole's "a format it does not know", 2 on main before this).
// This one refuses a Kubernetes policy of format 1.
func TestAKubernetesPolicyIsFormat2(t *testing.T) {
	k := newCluster(t, "k1")
	one := policyEdit(t, kubePolicy(t, 42, "", []map[string]any{k.public(t)}), func(p map[string]any) { p["format"] = 1 })
	if _, err := parse(one, true); err == nil {
		t.Error("a Kubernetes policy of format 1 was taken")
	}
	if _, err := parse(kubePolicy(t, 42, "", []map[string]any{k.public(t)}), true); err != nil {
		t.Errorf("a Kubernetes policy of format 2: %v", err)
	}
}
