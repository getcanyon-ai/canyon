package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// cluster is a fake API server's issuer discovery: the issuer and the key set that signs egress-control's
// tokens, read with the server's own token; down answers 403 to both.
type cluster struct {
	mu     sync.Mutex
	issuer string
	key    *ecdsa.PrivateKey
	down   bool
	asked  []string // the Authorization of each read
}

// set changes what the cluster answers from the next read on.
func (c *cluster) set(key *ecdsa.PrivateKey, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.down = key, down
}

func (c *cluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, r.Header.Get("Authorization"))
	switch {
	case c.down:
		w.WriteHeader(http.StatusForbidden)
	case r.URL.Path == "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": c.issuer, "jwks_uri": c.issuer + "/openid/v1/jwks"})
	case r.URL.Path == "/openid/v1/jwks":
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &c.key.PublicKey, KeyID: "k1", Algorithm: "ES256", Use: "sig"}}})
	default:
		http.NotFound(w, r)
	}
}

// sign is a projected token of the cluster's: claims over a default of egress-control's for pod
// canyon-egress-7d9f-abcde, audience canyon-egress-policy, ten minutes left.
func (c *cluster) sign(t *testing.T, key *ecdsa.PrivateKey, over map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": c.issuer, "sub": egressControlAccount, "aud": []string{egressPolicyAudience},
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"kubernetes.io": map[string]any{"namespace": "canyon", "pod": map[string]string{"name": "canyon-egress-7d9f-abcde", "uid": "u1"}, "serviceaccount": map[string]string{"name": "canyon-egress", "uid": "u2"}},
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The policy is egress-control's alone on Kubernetes (contracts/routes.md, Egress control): a projected token of
// its ServiceAccount, for the policy's audience, signed by the cluster's key set and bound to a pod, which names
// the replica. Any other token — another account's, another audience's, expired, signed by another key — or none
// is 401, and so is every token while the key set cannot be read.
func TestPolicyTokenIsEgressControlsAlone(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{issuer: "https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE", key: key}
	api := httptest.NewServer(c)
	t.Cleanup(api.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-servers-own-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &kubeTokens{api: api.URL, token: tokenFile, client: api.Client(), log: log.New(io.Discard, "", 0)}
	ask := func(header string) (string, bool) {
		r := httptest.NewRequest(http.MethodGet, "/egress/policy", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		return k.replica(r)
	}
	if pod, ok := ask("Bearer " + c.sign(t, key, nil)); !ok || pod != "canyon-egress-7d9f-abcde" {
		t.Fatalf("egress-control's token: %q %v, want its pod", pod, ok)
	}
	if len(c.asked) == 0 || c.asked[0] != "Bearer the-servers-own-token" {
		t.Fatalf("the key set is read with the server's own token: %q", c.asked)
	}
	for name, header := range map[string]string{ //nolint:gosec // test: tokens this test signs, for a cluster it fakes
		"no token":              "",
		"not a bearer":          "Basic " + c.sign(t, key, nil),
		"another account":       "Bearer " + c.sign(t, key, map[string]any{"sub": "system:serviceaccount:canyon-apps:canyon-app-claims-portal-prod"}),
		"another audience":      "Bearer " + c.sign(t, key, map[string]any{"aud": []string{"https://kubernetes.default.svc"}}),
		"another issuer":        "Bearer " + c.sign(t, key, map[string]any{"iss": "https://issuer.example"}),
		"expired":               "Bearer " + c.sign(t, key, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
		"bound to no pod":       "Bearer " + c.sign(t, key, map[string]any{"kubernetes.io": map[string]any{"namespace": "canyon"}}),
		"signed by another key": "Bearer " + c.sign(t, other, nil),
		"not a token":           "Bearer not.a.token",
		"an empty bearer":       "Bearer ",
	} {
		if pod, ok := ask(header); ok {
			t.Errorf("%s: answered as %q, want 401", name, pod)
		}
	}

	down := &cluster{issuer: c.issuer, key: key, down: true}
	downAPI := httptest.NewServer(down)
	t.Cleanup(downAPI.Close)
	k = &kubeTokens{api: downAPI.URL, token: tokenFile, client: downAPI.Client(), log: log.New(io.Discard, "", 0)}
	if pod, ok := ask("Bearer " + c.sign(t, key, nil)); ok {
		t.Fatalf("a key set that cannot be read verified a token as %q", pod)
	}
}

// The listener's certificate is egress-control's pin: self-signed for the server's Service, a server's alone, its
// key the one sealed beside it.
func TestPolicyCertIsItsOwnPin(t *testing.T) {
	certPEM, der, err := mintPolicyCert(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatalf("the key is not the certificate's: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "canyon-server.canyon.svc", Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("pinned, it does not verify as canyon-server.canyon.svc: %v", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("the certificate's key is %T, want P-256", cert.PublicKey)
	}
	if cert.IsCA || cert.NotAfter.Before(time.Now().AddDate(9, 11, 0)) {
		t.Fatalf("a leaf for ten years: CA %v, until %s", cert.IsCA, cert.NotAfter)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "canyon.meridian.example", Roots: roots}); err == nil {
		t.Fatal("it verifies for a name other than the server's Service")
	}
}

// The listener answers an unproven caller 401 with no body, and caller= anything but one IPv4 address 400
// invalid_caller, before it reads anything.
func TestPolicyRefusesTheUnprovenAndAMalformedCaller(t *testing.T) {
	var proven bool
	s := &policyServer{replica: func(*http.Request) (string, bool) { return "r1", proven }, log: log.New(os.Stderr, "", 0)}
	get := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w
	}
	if w := get("/egress/policy?have=&caller=10.0.4.7"); w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
		t.Fatalf("unproven: %d %q, want 401 and no body", w.Code, w.Body)
	}
	proven = true
	for _, caller := range []string{"", "010.0.4.7", "10.0.4", "::ffff:10.0.4.7", "fe80::1", "10.0.4.7:80", "canyon-server", "167773191"} {
		w := get("/egress/policy?caller=" + caller)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_caller"`) {
			t.Errorf("caller=%q: %d %s, want 400 invalid_caller", caller, w.Code, w.Body)
		}
	}
	if w := get("/egress/policy?caller=10.0.4.7&caller=10.0.4.8"); w.Code != http.StatusBadRequest {
		t.Errorf("two callers: %d, want 400", w.Code)
	}
	if w := get("/egress/records"); w.Code != http.StatusNotFound {
		t.Errorf("another path: %d, want 404", w.Code)
	}
}

// The policy's address is the receiver's kind of address: on Docker the server's own on canyon-egress-ctl, never
// one an app or the host reaches; on Kubernetes 0.0.0.0 and a port, behind the NetworkPolicy.
func TestPolicyAddrIsTheGatewaysAlone(t *testing.T) {
	apps := netip.MustParsePrefix("10.213.0.0/16")
	for _, c := range []struct {
		addr string
		kube bool
		pool netip.Prefix
		ok   bool
	}{
		{"10.214.3.2:7782", false, apps, true},
		{"0.0.0.0:7782", true, netip.Prefix{}, true},
		{"0.0.0.0:7782", false, apps, false},
		{"127.0.0.1:7782", false, apps, false},
		{"10.213.4.1:7782", false, apps, false},
		{"canyon-server:7782", false, apps, false},
		{"10.214.3.2:7782", false, netip.Prefix{}, false},
		{"10.214.3.2:7782", true, netip.Prefix{}, false},
		{"[::]:7782", true, netip.Prefix{}, false},
	} {
		err := egressAddrOf("CANYON_EGRESS_POLICY_ADDR", "the policy", "7782", c.addr, c.kube, c.pool)
		if (err == nil) != c.ok {
			t.Errorf("%s (kube %v): %v, want ok %v", c.addr, c.kube, err, c.ok)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "CANYON_EGRESS_POLICY_ADDR: ") {
			t.Errorf("%s: %v does not name the setting", c.addr, err)
		}
	}
}

// On Docker nothing but the listener admits a caller (contracts/routes.md, Egress control): a peer in the apps'
// pool is closed at accept and never reaches the handler, and so is canyon-egress-ctl's gateway, the host; another
// peer on canyon-egress-ctl, the holder's, does.
func TestPolicyListenerClosesTheAppPoolAndTheHost(t *testing.T) {
	var reached atomic.Int32
	s := &policyServer{replica: func(*http.Request) (string, bool) { reached.Add(1); return "", false }, log: log.New(os.Stderr, "", 0)}
	for _, c := range []struct {
		pool, gateway string
		reaches       bool
	}{{"127.0.0.0/16", "10.214.3.1", false}, {"10.213.0.0/16", "127.0.0.1", false}, {"10.213.0.0/16", "10.214.3.1", true}} { // the test dials from 127.0.0.1: an app's address, the host's, then neither
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: s, ReadHeaderTimeout: time.Second}
		go func() {
			_ = srv.Serve(policyListener(ln, netip.MustParsePrefix(c.pool), nil, netip.MustParseAddr(c.gateway)))
		}()
		before := reached.Load()
		client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := client.Get("http://" + ln.Addr().String() + "/egress/policy?have=")
		if err == nil {
			_ = resp.Body.Close()
		}
		if got := err == nil && resp.StatusCode == http.StatusUnauthorized && reached.Load() == before+1; got != c.reaches {
			t.Errorf("a peer with the apps' pool %s and the gateway %s: reached the handler %v (%v), want %v", c.pool, c.gateway, got, err, c.reaches)
		}
		_ = srv.Close()
	}
}

// The gateway the policy listener refuses on Docker is the first address of the network the server's address is
// on, Docker's gateway there; an address the server does not hold is refused at start.
func TestPolicyListenerFindsCanyonEgressCtlsGateway(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.214.2.4").To4(), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("10.214.3.2").To4(), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("10.214.5.0").To4(), Mask: net.CIDRMask(31, 32)},
	}
	if _, err := gatewayOf(netip.MustParseAddr("10.214.5.0"), addrs); err == nil || err.Error() != "10.214.5.0 is on 10.214.5.0/31, a /31, which has no gateway" {
		t.Errorf("an address on a /31: %v, want refused naming its network", err)
	}
	if gw, err := gatewayOf(netip.MustParseAddr("10.214.3.2"), addrs); err != nil || gw != netip.MustParseAddr("10.214.3.1") {
		t.Errorf("the gateway of 10.214.3.2/24: %v %v, want 10.214.3.1", gw, err)
	}
	if _, err := gatewayOf(netip.MustParseAddr("10.214.9.2"), addrs); err == nil || err.Error() != "10.214.9.2 is on no network of this server's" {
		t.Errorf("an address the server does not hold: %v, want refused naming it", err)
	}
}

// The cluster's key set is trusted ten minutes at most (contracts/routes.md, Egress control): read again once it
// is that old, so a key the cluster retired stops verifying; while it cannot be read, what was read stays until
// it is ten minutes old, then every request is 401; once it can be read again, at most a minute later, it verifies.
func TestPolicyKeySetIsTrustedTenMinutesAtMost(t *testing.T) {
	old, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{issuer: "https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE", key: old}
	api := httptest.NewServer(c)
	t.Cleanup(api.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-servers-own-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	k := &kubeTokens{api: api.URL, token: tokenFile, client: api.Client(), log: log.New(io.Discard, "", 0), now: func() time.Time { return at }}
	ask := func(key *ecdsa.PrivateKey) bool {
		r := httptest.NewRequest(http.MethodGet, "/egress/policy", nil)
		r.Header.Set("Authorization", "Bearer "+c.sign(t, key, map[string]any{"iat": at.Unix(), "nbf": at.Unix(), "exp": at.Add(10 * time.Minute).Unix()}))
		_, ok := k.replica(r)
		return ok
	}
	if !ask(old) {
		t.Fatal("a token of the key set read: refused")
	}
	c.set(old, true)
	at = at.Add(9 * time.Minute)
	if !ask(old) {
		t.Fatal("the key set unreadable, its read 9 minutes old: refused, want it still trusted")
	}
	at = at.Add(2 * time.Minute)
	if ask(old) {
		t.Fatal("the key set unreadable, its read 11 minutes old: verified, want 401")
	}
	c.set(rotated, false)
	at = at.Add(time.Minute)
	if ask(old) {
		t.Error("a token of the key the cluster retired, the key set read again: verified, want 401")
	}
	if !ask(rotated) {
		t.Error("a token of the cluster's key, the key set read again: refused")
	}
}

// On Docker the replica is who asks: its address on canyon-egress-ctl, never anything it says.
func TestDockerReplicaIsThePeersAddress(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/egress/policy?have=", nil)
	r.RemoteAddr = "10.214.3.3:51234"
	r.Header.Set("X-Replica", "canyon-egress-7d9f-abcde")
	if replica, ok := peerReplica(r); !ok || replica != "10.214.3.3" {
		t.Fatalf("%q %v, want the peer's address", replica, ok)
	}
	r.RemoteAddr = "not an address"
	if replica, ok := peerReplica(r); ok {
		t.Fatalf("a peer with no address: %q, want 401", replica)
	}
}
