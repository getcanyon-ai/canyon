package egresscontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

// start is the replica's clock as newReplica sets it.
var start = time.Date(2026, 10, 19, 10, 0, 30, 0, time.UTC)

// receiverToken is newReplica's CANYON_EGRESS_RECEIVER_TOKEN, a token's shape.
var receiverToken = strings.Repeat("r7", 20)

// marshal is v as JSON.
func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// verdict is a check's answer, all of it a test reads.
type verdict struct {
	code     int32
	status   int32
	reason   string
	body     string
	set      map[string]string
	appended bool // a header set other than by overwriting
	remove   []string
	metadata map[string]any
}

func verdictOf(t *testing.T, resp *authv3.CheckResponse) verdict {
	t.Helper()
	v := verdict{code: resp.GetStatus().GetCode(), set: map[string]string{}, metadata: resp.GetDynamicMetadata().AsMap()}
	if ok := resp.GetOkResponse(); ok != nil {
		for _, h := range ok.GetHeaders() {
			v.set[h.GetHeader().GetKey()] = h.GetHeader().GetValue()
			v.appended = v.appended || h.GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD
		}
		v.remove = ok.GetHeadersToRemove()
	}
	if d := resp.GetDeniedResponse(); d != nil {
		v.status, v.body = int32(d.GetStatus().GetCode()), d.GetBody()
		for _, h := range d.GetHeaders() {
			if h.GetHeader().GetKey() == "Canyon-Egress" {
				v.reason = h.GetHeader().GetValue()
			}
		}
	}
	return v
}

// pki stands in for the server's interception chain (design/EGRESS.md §4.5): a root, the intermediate under it, and
// GET /egress/leaf's answers, each a leaf minted under the intermediate.
type pki struct {
	inter       *x509.Certificate
	interKey    *ecdsa.PrivateKey
	fingerprint string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	rootKey, interKey := key(t), key(t)
	root := sign(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Canyon egress root"}, NotBefore: start.Add(-time.Hour),
		NotAfter: start.Add(5 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil, &rootKey.PublicKey, rootKey)
	inter := sign(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Canyon egress intermediate"}, NotBefore: start.Add(-time.Hour),
		NotAfter: start.Add(7 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}, root, &interKey.PublicKey, rootKey)
	sum := sha256.Sum256(root.Raw)
	return &pki{inter: inter, interKey: interKey, fingerprint: hex.EncodeToString(sum[:])}
}

func key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// answer is GET /egress/leaf's 200 (egress.schema.json, leaf) for a leaf naming names, valid from from to to.
func (p *pki) answer(t *testing.T, from, to time.Time, names ...string) map[string]string {
	t.Helper()
	k := key(t)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	cert := sign(t, &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: from, NotAfter: to,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, p.inter, &k.PublicKey, p.interKey)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"root": p.fingerprint, "cert": pemOf(cert), "key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		"chain": pemOf(p.inter), "not_after": to.UTC().Format(time.RFC3339)}
}

// policyEdit is policy, a policy of routed's, changed by change, under the next seq.
func policyEdit(t *testing.T, policy []byte, change func(p map[string]any)) []byte {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(policy, &p); err != nil {
		t.Fatal(err)
	}
	p["seq"] = p["seq"].(float64) + 1
	change(p)
	return marshal(t, p)
}

const (
	claimsProd = "172.28.3.11" // claims-portal prod in routed's policy
	ledgerProd = "172.28.5.11" // ledger prod
)

// secret is a route value's shape, synthetic: what no log line, error or refusal may carry.
var secret = "sk_live_" + strings.Repeat("v", 24)

// outer is ext_authz #1's check of a call from source: method, scheme and authority as Envoy reads them.
func (r replica) outer(t *testing.T, source, method, scheme, authority string) verdict {
	t.Helper()
	resp, err := r.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: source, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 41234}}}}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Id: "7", Method: method, Scheme: scheme, Host: authority,
			Path: "/v1/charges?key=" + secret, Headers: map[string]string{"authorization": "Bearer canyon-adds-the-real-key"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return verdictOf(t, resp)
}

// innerOf is ext_authz #2's check of a request in the tunnel of the chain for destination, the tunnel ext_authz #1
// approved for destination: caller the x-canyon-caller Envoy's header_mutation set ("" for none at all), authority
// its :authority or Host, source Envoy's internal one or an app's address, to show it is never read.
func (r replica) innerOf(t *testing.T, destination, caller, authority, source string) verdict {
	t.Helper()
	return r.innerApproved(t, destination, destination, caller, authority, source)
}

// innerApproved is innerOf of a tunnel ext_authz #1 approved for approved, the x-canyon-destination Envoy's
// header_mutation set ("" for none at all).
func (r replica) innerApproved(t *testing.T, destination, approved, caller, authority, source string) verdict {
	t.Helper()
	headers := map[string]string{"authorization": "Bearer canyon-adds-the-real-key"}
	if caller != "" {
		headers["x-canyon-caller"] = caller
	}
	if approved != "" {
		headers["x-canyon-destination"] = approved
	}
	resp, err := r.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address: source, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 0}}}}},
		ContextExtensions: map[string]string{"destination": destination},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Id: "9", Method: "POST", Scheme: "https", Host: authority,
			Path: "/v1/charges", Headers: headers}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return verdictOf(t, resp)
}

func (r replica) inner(t *testing.T, destination, caller, authority string) verdict {
	t.Helper()
	return r.innerOf(t, destination, caller, authority, "envoy://internal_client_address/")
}

func refusedAs(t *testing.T, what string, v verdict, reason string, status int32, body string) {
	t.Helper()
	if v.code == int32(codes.OK) || v.reason != reason || v.status != status || v.body != body || v.metadata["outcome"] != "refused" || v.metadata["reason"] != reason {
		t.Errorf("%s: answered %d %d %q %q %v, want %d %s %q", what, v.code, v.status, v.reason, v.body, v.metadata, status, reason, body)
	}
}

// inject is the fixture's routes: three app envs, ten inject destinations among them, several named twice.
var inject = map[string][]string{
	"claims-portal": {"api.openai.com", "api.stripe.com", "hooks.slack.com", "sqs.eu-west-1.amazonaws.com"},
	"intake-bot":    {"api.anthropic.com", "api.openai.com", "api.twilio.com", "smtp.sendgrid.net"},
	"ledger":        {"api.adyen.com", "api.plaid.com", "api.stripe.com", "graph.microsoft.com"},
}

func injected() []string {
	var all []string
	for _, ds := range inject {
		all = append(all, ds...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// routed is a policy of an env of prod for each app routes names, each env's routes to the destinations given and
// its allowlist every fixture destination, under the signing root signing, "" for null.
func routed(t *testing.T, seq int, signing string, routes map[string][]string) []byte {
	t.Helper()
	envs, callers := []map[string]any{}, []map[string]string{}
	for i, app := range slices.Sorted(maps.Keys(routes)) {
		rs := []map[string]any{}
		for j, d := range routes[app] {
			rs = append(rs, map[string]any{"destination": d, "header": "authorization", "scheme": "bearer", "rev": 100*i + j + 1})
		}
		envs = append(envs, map[string]any{"app": app, "env": "prod", "mode": "allowlist", "allowed": injected(), "known": []string{}, "routes": rs})
		callers = append(callers, map[string]string{"address": "172.28." + string(rune('3'+i)) + ".11", "app": app, "env": "prod"})
	}
	p := map[string]any{"format": 1, "epoch": epoch, "seq": seq, "public_url": "https://canyon.meridian.example",
		"callers": callers, "envs": envs, "platform": []string{"login.example-idp.com"}, "signing_root": nil, "caps": map[string]int{"new_names": FleetNames}}
	if signing != "" {
		p["signing_root"] = signing
	}
	return marshal(t, p)
}
