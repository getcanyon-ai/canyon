//go:build !noee

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// The interception root's tests over the server's routes (ee_outbound_test.go): ee/outboundkeys's own are beside
// its code.

// rotationOverlap is how long a rotation keeps the old root, ee/outboundkeys's: 60 days.
const rotationOverlap = 60 * 24 * time.Hour

// everyAddress is every IPv4 and IPv6 address, which a name-constrained sub-CA excludes.
var everyAddress = []*net.IPNet{{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}, {IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}}

// pemOf is certs as PEM, one block each.
func pemOf(certs ...*x509.Certificate) string {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return string(out)
}

// certificatesOf parses PEM that is certificates alone.
func certificatesOf(t *testing.T, s string) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for rest := []byte(s); len(rest) > 0; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("not certificates alone: %q", s)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// leafOf parses one certificate's PEM.
func leafOf(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	certs := certificatesOf(t, certPEM)
	if len(certs) != 1 {
		t.Fatalf("one certificate: %d", len(certs))
	}
	return certs[0]
}

// verifies is whether leaf verifies for name under root, through between, at now, by Go's crypto/x509.
func verifies(leaf *x509.Certificate, name string, root *x509.Certificate, now time.Time, between ...*x509.Certificate) error {
	roots, pool := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range between {
		pool.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func serialOf(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// orgCA is an organization's PKI as a test supplies it: a self-signed root and a sub-CA under it, which tweak may
// change before it is signed, permitting names and excluding every address (no names: no name constraints), and the
// sub-CA's key.
type orgCA struct {
	root, sub *x509.Certificate
	key       *ecdsa.PrivateKey
}

func newOrgCA(t *testing.T, now time.Time, tweak func(*x509.Certificate), names ...string) orgCA {
	t.Helper()
	return orgCAWith(t, now, nil, tweak, names...)
}

// orgCAWith is newOrgCA with rootTweak changing the root before it signs itself.
func orgCAWith(t *testing.T, now time.Time, rootTweak, tweak func(*x509.Certificate), names ...string) orgCA {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTmpl := &x509.Certificate{SerialNumber: serialOf(t), Subject: pkix.Name{Organization: []string{"Meridian Insurance"}, CommonName: "Meridian Root CA"},
		NotBefore: now.AddDate(-2, 0, 0), NotAfter: now.AddDate(8, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if rootTweak != nil {
		rootTweak(rootTmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(der)
	subKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subTmpl := &x509.Certificate{SerialNumber: serialOf(t), Subject: pkix.Name{Organization: []string{"Meridian Insurance"}, CommonName: "Meridian Canyon sub-CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(2, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 1, KeyUsage: x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: len(names) > 0, PermittedDNSDomains: names}
	if len(names) > 0 {
		subTmpl.ExcludedIPRanges = everyAddress
	}
	if tweak != nil {
		tweak(subTmpl)
	}
	if der, err = x509.CreateCertificate(rand.Reader, subTmpl, root, &subKey.PublicKey, rootKey); err != nil {
		t.Fatal(err)
	}
	sub, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("the sub-CA permitting %q: %v", names, err)
	}
	return orgCA{root: root, sub: sub, key: subKey}
}

// body is the rotation's {cert, key} for the sub-CA: the sub-CA, then the root.
func (o orgCA) body(t *testing.T) string {
	t.Helper()
	return certKey(pemOf(o.sub, o.root), keyPEMOf(t, o.key))
}

// fingerprint is the sub-CA's, as the roots' table keeps it.
func (o orgCA) fingerprint() string {
	sum := sha256.Sum256(o.sub.Raw)
	return hex.EncodeToString(sum[:])
}

func certKey(cert, key string) string {
	raw, _ := json.Marshal(map[string]string{"cert": cert, "key": key})
	return string(raw)
}

func keyPEMOf(t *testing.T, key any) string {
	t.Helper()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
}

// leafAt is one GET /egress/leaf as replica: its status, its body and its Cache-Control.
func leafAt(t *testing.T, base, replica, query string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/leaf?"+query, nil)
	if replica != "" {
		req.Header.Set("X-Replica", replica)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header.Get("Cache-Control")
}

// leafFor is a 200 leaf answer for destination, checked against the contract, its leaf and its chain parsed.
func leafFor(t *testing.T, base, destination string) (enterprise.Leaf, *x509.Certificate, []*x509.Certificate) {
	t.Helper()
	code, body, cache := leafAt(t, base, "canyon-egress-a", "destination="+destination)
	if code != 200 || cache != "no-store" {
		t.Fatalf("the leaf of %s: %d %q %s", destination, code, cache, body)
	}
	egressShape(t, "leaf", body)
	var answer enterprise.Leaf
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	return answer, leafOf(t, answer.Cert), certificatesOf(t, answer.Chain)
}

// certOf is a root's certificate as the roots' table keeps it.
func (h outbound) certOf(t *testing.T, fingerprint string) *x509.Certificate {
	t.Helper()
	var certPEM string
	if err := h.pool.QueryRow(h.ctx, "SELECT cert FROM egress_ca_roots WHERE fingerprint = $1", fingerprint).Scan(&certPEM); err != nil {
		t.Fatal(err)
	}
	return leafOf(t, certPEM)
}

const rotatePath = "/api/v1/egress/ca/rotate"

// caOf is the admin's read's key dates.
func (h outbound) caOf(t *testing.T) enterprise.CARead {
	t.Helper()
	if code, _, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 {
		t.Fatalf("the admin's read: %d %s", code, lastBody)
	}
	egressShape(t, "setting_admin", lastBody)
	var out struct {
		CA enterprise.CARead `json:"ca"`
	}
	if err := json.Unmarshal([]byte(lastBody), &out); err != nil {
		t.Fatal(err)
	}
	return out.CA
}
