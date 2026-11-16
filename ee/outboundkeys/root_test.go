package outboundkeys

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// design/EGRESS.md §4.5: "Leaves are signed under one root at a time, the signing root: on rotation or a switch to a
// BYO sub-CA the old root signs on until no env holding a route lacks the new one, and an env still lacking it at the
// old root's end waits." Two roots, old and new: the old signs while any env holding a route lacks the new; the new
// once every one carries it, or none holds a route; the new once the old has ended, whoever lacks it; none once both
// have; and with a third, the newest every holder carries.
func TestSigningRootStaysOldUntilEveryHolderTrustsTheNew(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	old := egressRoot{fingerprint: strings.Repeat("a", 64), created: now.AddDate(-1, 0, 0), ends: now.AddDate(4, 0, 0)}
	newer := egressRoot{fingerprint: strings.Repeat("b", 64), created: now.AddDate(0, 0, -1), ends: now.AddDate(5, 0, 0)}
	newest := egressRoot{fingerprint: strings.Repeat("c", 64), created: now, ends: now.AddDate(5, 0, 0)}
	both, oldOnly, none := []string{old.fingerprint, newer.fingerprint}, []string{old.fingerprint}, []string{}
	for _, c := range []struct {
		name    string
		roots   []egressRoot
		holders [][]string
		at      time.Time
		want    string
	}{
		{"one root, an env lacking it", []egressRoot{old}, [][]string{none}, now, old.fingerprint},
		{"one root, every env trusting it", []egressRoot{old}, [][]string{oldOnly}, now, old.fingerprint},
		{"a rotation, an env holding a route lacking the new", []egressRoot{newer, old}, [][]string{both, oldOnly}, now, old.fingerprint},
		{"a rotation, an env holding a route trusting neither", []egressRoot{old, newer}, [][]string{both, none}, now, old.fingerprint},
		{"a rotation, every env holding a route trusting the new", []egressRoot{old, newer}, [][]string{both, {newer.fingerprint}}, now, newer.fingerprint},
		{"a rotation, no env holding a route", []egressRoot{old, newer}, nil, now, newer.fingerprint},
		{"the old root's end, an env still lacking the new", []egressRoot{old, newer}, [][]string{oldOnly}, old.ends, newer.fingerprint},
		{"every root ended", []egressRoot{old, newer}, [][]string{both}, newer.ends, ""},
		{"no root", nil, [][]string{both}, now, ""},
		{"three roots, every holder trusting the middle one", []egressRoot{newest, old, newer}, [][]string{both, {newer.fingerprint, newest.fingerprint}}, now, newer.fingerprint},
	} {
		if got := egressSigningRoot(c.roots, c.holders, c.at); got != c.want {
			t.Errorf("%s: the signing root is %q, want %q", c.name, got, c.want)
		}
	}
}

// design/EGRESS.md §4.5: the interception root is a CA of its own, five years, its fingerprint the SHA-256 of its DER
// in lower-case hex (egress.schema.json, root_fingerprint), and its key the PKCS #8 the vault seals.
func TestInterceptionRootIsAFiveYearCA(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	certPEM, fingerprint, key, notAfter, err := mintInterceptionRoot(now)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode([]byte(certPEM))
	if block == nil || len(rest) != 0 {
		t.Fatalf("not one PEM block: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(block.Bytes)
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil || fingerprint != hex.EncodeToString(sum[:]) ||
		!cert.NotAfter.Equal(now.AddDate(5, 0, 0)) || !notAfter.Equal(cert.NotAfter) {
		t.Fatalf("a self-signed CA of five years, fingerprinted: CA %t, usage %v, until %s (%s), fingerprint %s", cert.IsCA, cert.KeyUsage, cert.NotAfter, notAfter, fingerprint)
	}
	if _, err := x509.ParsePKCS8PrivateKey(key); err != nil {
		t.Fatalf("the key is no PKCS #8: %v", err)
	}
}
