package kube

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/subjecthash"
)

// rootAt is a CA named cn whose notBefore is at, under its OpenSSL subject-hash name, as egress.ca carries one.
func rootAt(t *testing.T, cn string, at time.Time) executor.Root {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: at, NotAfter: at.Add(5 * 365 * 24 * time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := subjecthash.Of(cert.RawSubject)
	if err != nil {
		t.Fatal(err)
	}
	return executor.Root{Name: hash + ".0", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// contracts/executor.md, On Kubernetes, canyon-egress-ca (#2143's review): a /run that read the roots before a rotation
// or a root's end never puts back the set a later /run wrote — never the older root alone, never a retired root again —
// and a newer set always replaces an older one.
func TestAnOlderEgressCASetNeverReplacesANewerOne(t *testing.T) {
	old, newer := rootAt(t, "Canyon root 2026", time.Date(2026, 1, 11, 10, 40, 0, 0, time.UTC)), rootAt(t, "Canyon root 2031", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, req, _, _ := withTrust(t)
	c := cluster(t, e, healthy)
	held := func(roots ...executor.Root) []string {
		t.Helper()
		egress := *req.Egress
		egress.CA = roots
		req.Egress = &egress
		if _, err := e.Run(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		cm, err := c.CoreV1().ConfigMaps(Namespace).Get(t.Context(), egressCAMap, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for name := range cm.Data {
			if name != "bundle.pem" && name != "system-and-canyon.pem" {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		return names
	}
	both := []string{old.Name, newer.Name}
	slices.Sort(both)
	for _, step := range []struct {
		what  string
		roots []executor.Root
		want  []string
	}{
		{"the old root alone, first", []executor.Root{old}, []string{old.Name}},
		{"a rotation's two roots replace it", []executor.Root{newer, old}, both},
		{"a /run from before the rotation leaves them", []executor.Root{old}, both},
		{"the old root's end: the new one alone", []executor.Root{newer}, []string{newer.Name}},
		{"a /run from before that end leaves it", []executor.Root{newer, old}, []string{newer.Name}},
		{"nor does one from before the rotation put the old root back", []executor.Root{old}, []string{newer.Name}},
	} {
		if got := held(step.roots...); !slices.Equal(got, step.want) {
			t.Errorf("%s: canyon-egress-ca holds %v; want %v", step.what, got, step.want)
		}
	}
}
