package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// certificate is a fresh self-signed CA as PEM, and a private key block beside it.
func certificate(t *testing.T) (cert, key []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "apps-db.meridian.example CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

// CANYON_APP_DATABASE_CA_FILE: unset is none; set, it is the file's certificates re-encoded as /run's
// database_ca takes them, and anything else fails the boot naming the setting (contracts/executor.md,
// the apps database).
func TestDatabaseCAOf(t *testing.T) {
	const appDB = "postgres://canyon_admin:x@apps-db.meridian.example:5433/postgres?sslmode=require" //nolint:gosec // a synthetic URL, not a credential
	one, key := certificate(t)
	two, _ := certificate(t)
	file := func(content string) string {
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if ca, err := databaseCAOf("", appDB, true); ca != "" || err != nil {
		t.Fatalf("unset: %q, %v; want none", ca, err)
	}
	bundle := "# Amazon RDS, two regions\r\n" + strings.ReplaceAll(string(one), "\n", "\r\n") + "\n" + string(two)
	if ca, err := databaseCAOf(file(bundle), appDB, true); ca != string(one)+string(two) || err != nil {
		t.Fatalf("a bundle with a comment and CRLF lines: %q, %v; want its two certificates alone", ca, err)
	}
	many := strings.Repeat(string(one), 256<<10/len(one)+1)
	for name, tc := range map[string]struct {
		file, appDB string
		kube        bool
	}{
		"no built-in driver":                {file(string(one)), "", true},
		"a Docker host":                     {file(string(one)), appDB, false},
		"no such file":                      {filepath.Join(t.TempDir(), "missing.pem"), appDB, true},
		"no PEM":                            {file("apps-db.meridian.example CA"), appDB, true},
		"a private key beside it":           {file(string(one) + string(key)), appDB, true},
		"a certificate that does not parse": {file(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")}))), appDB, true},
		"over 256 KiB":                      {file(many), appDB, true},
	} {
		if ca, err := databaseCAOf(tc.file, tc.appDB, tc.kube); err == nil || !strings.Contains(err.Error(), "CANYON_APP_DATABASE_CA_FILE") {
			t.Errorf("%s: %q, %v; want the boot refused naming the setting", name, ca, err)
		}
	}
}
