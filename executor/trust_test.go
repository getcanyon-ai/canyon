package executor

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixtureRoot is internal/subjecthash's PrintableString CA, whose subject hash OpenSSL itself gave: aea6fb85.
func fixtureRoot(t *testing.T) Root {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "subjecthash", "testdata", "printable.pem"))
	if err != nil {
		t.Fatal(err)
	}
	return Root{Name: "aea6fb85.0", PEM: string(raw)}
}

// systemRoots writes a system roots file of one CA to dir and points e at it.
func systemRoots(t *testing.T, e *Executor, dir string) string {
	t.Helper()
	ca := selfSigned(t, "System Root", true)
	p := filepath.Join(dir, "ca-certificates.crt")
	must(t, os.WriteFile(p, []byte(ca), 0o600))
	e.Roots = p
	return ca
}

func selfSigned(t *testing.T, cn string, isCA bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: isCA, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// trustOf is a tar's regular files by name, with their modes, and its directories' modes.
func trustOf(t *testing.T, raw []byte) (files map[string]string, modes map[string]int64) {
	t.Helper()
	files, modes = map[string]string{}, map[string]int64{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, modes
		}
		must(t, err)
		modes[h.Name] = h.Mode
		if h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			must(t, err)
			files[h.Name] = string(b)
		}
	}
}

// design/EGRESS.md §4.4; contracts/executor.md, egress.ca: on Docker a /run carrying egress.ca creates the container,
// copies /etc/canyon-egress into it, read-only, and only then starts it. Each root lies under its OpenSSL
// subject-hash name, the only name Paketo's JVM certificate loader reads in SSL_CERT_DIR (its pattern below), with
// bundle.pem the roots and system-and-canyon.pem the executor's own system roots then the roots; the app's
// environment names them in SSL_CERT_DIR, NODE_EXTRA_CA_CERTS and REQUESTS_CA_BUNDLE.
func TestJavaLoaderReadsTheHashName(t *testing.T) {
	e, dir := fakes(t)
	system := systemRoots(t, e, dir)
	root := fixtureRoot(t)
	req := egressReq
	req.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}, CA: []Root{root}}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	argv := read(t, dir, "argv")
	create, cp, start := strings.Index(argv, "create\n--name\nclaims-portal-preview\n"), strings.Index(argv, "cp\n-\nclaims-portal-preview:/\n"), strings.Index(argv, "start\nclaims-portal-preview\n")
	if create < 0 || cp < create || start < cp || strings.Contains(argv, "run\n-d\n--name\nclaims-portal-preview\n") {
		t.Fatalf("want create, then cp, then start, and no docker run -d of the app:\n%s", argv)
	}
	files, modes := trustOf(t, []byte(read(t, dir, "cp-tar")))
	paketo := regexp.MustCompile(`^[[:xdigit:]]{8}\.[[:digit:]]+$`) // libjvm's certificate loader: the files of SSL_CERT_DIR it reads
	var named []string
	for name := range files {
		if base := strings.TrimPrefix(name, "etc/canyon-egress/"); paketo.MatchString(base) {
			named = append(named, base)
		}
	}
	if !slices.Equal(named, []string{"aea6fb85.0"}) || files["etc/canyon-egress/aea6fb85.0"] != root.PEM {
		t.Errorf("the roots the JVM's loader finds: %v; want aea6fb85.0, OpenSSL's subject hash of the root, holding it", named)
	}
	if files["etc/canyon-egress/bundle.pem"] != root.PEM || files["etc/canyon-egress/system-and-canyon.pem"] != system+root.PEM {
		t.Errorf("bundle.pem %q, system-and-canyon.pem %q; want the root, then the system roots and the root", files["etc/canyon-egress/bundle.pem"], files["etc/canyon-egress/system-and-canyon.pem"])
	}
	if modes["etc/canyon-egress/"] != 0o555 || modes["etc/canyon-egress/aea6fb85.0"] != 0o444 || len(files) != 3 {
		t.Errorf("modes %v, files %v; want the directory 0555 and its three files 0444", modes, slices.Sorted(func(yield func(string) bool) {
			for k := range files {
				if !yield(k) {
					return
				}
			}
		}))
	}
	env := read(t, dir, "run-env")
	for _, want := range []string{"SSL_CERT_DIR=/etc/ssl/certs:/etc/canyon-egress\n", "NODE_EXTRA_CA_CERTS=/etc/canyon-egress/bundle.pem\n", "REQUESTS_CA_BUNDLE=/etc/canyon-egress/system-and-canyon.pem\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("the app's environment lacks %s", strings.TrimSpace(want))
		}
	}
}

// An env that names one of the three keeps its own values and gets none of the executor's, its trust its own; one
// without egress.ca gets no trust at all, and is started with docker run -d as before.
func TestAnEnvsOwnTrustStands(t *testing.T) {
	e, dir := fakes(t)
	systemRoots(t, e, dir)
	req := egressReq
	req.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}, CA: []Root{fixtureRoot(t)}}
	req.Env = map[string]string{"NODE_EXTRA_CA_CERTS": "/workspace/corp.pem"}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	env := read(t, dir, "run-env")
	if !strings.Contains(env, "NODE_EXTRA_CA_CERTS=/workspace/corp.pem\n") || strings.Contains(env, "SSL_CERT_DIR=") || strings.Contains(env, "REQUESTS_CA_BUNDLE=") {
		t.Errorf("env's own NODE_EXTRA_CA_CERTS: the app's environment holds\n%s\nwant env's value alone, none of the three set by the executor", env)
	}

	e, dir = fakes(t)
	if _, err := e.Run(t.Context(), egressReq); err != nil {
		t.Fatal(err)
	}
	if argv := read(t, dir, "argv"); !strings.Contains(argv, "run\n-d\n--name\nclaims-portal-preview\n") || strings.Contains(argv, "\ncp\n") || strings.Contains(read(t, dir, "run-env"), "SSL_CERT_DIR=") {
		t.Errorf("without egress.ca: want docker run -d, no cp and no trust variable:\n%s", argv)
	}
}

// egress.ca out of the contract's shapes is ErrRequest before docker is asked anything; system roots with no
// certificate fail the start, and nothing is started: never a system-and-canyon.pem of Canyon's roots alone.
func TestEgressCAOutOfShapeIsRefused(t *testing.T) {
	root := fixtureRoot(t)
	leaf := selfSigned(t, "Not A CA", false)
	nine := make([]Root, 9)
	for i := range nine {
		nine[i] = Root{Name: "aea6fb85." + string(rune('0'+i%10)), PEM: root.PEM}
	}
	for what, ca := range map[string][]Root{
		"none in a list":             {},
		"nine roots":                 nine,
		"a name that is no hash":     {{Name: "canyon-root.pem", PEM: root.PEM}},
		"another root's hash":        {{Name: "0123abcd.0", PEM: root.PEM}},
		"one name twice":             {root, root},
		"not a CA":                   {{Name: "aea6fb85.0", PEM: leaf}},
		"two certificates in one":    {{Name: "aea6fb85.0", PEM: root.PEM + root.PEM}},
		"a key, not a certificate":   {{Name: "aea6fb85.0", PEM: "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}},
		"a pem past 8 KiB":           {{Name: "aea6fb85.0", PEM: root.PEM + strings.Repeat("\n", 8<<10)}},
		"text that is no pem at all": {{Name: "aea6fb85.0", PEM: "a root"}},
	} {
		e, dir := fakes(t)
		req := egressReq
		req.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}, CA: ca}
		if _, err := e.Run(t.Context(), req); !errors.Is(err, ErrRequest) || readIf(dir, "argv") != "" {
			t.Errorf("%s: %v, want ErrRequest before docker is asked anything", what, err)
		}
	}

	e, dir := fakes(t)
	empty := filepath.Join(dir, "empty.crt")
	must(t, os.WriteFile(empty, []byte("no certificate here\n"), 0o600))
	e.Roots = empty
	req := egressReq
	req.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}, CA: []Root{root}}
	if _, err := e.Run(t.Context(), req); err == nil || errors.Is(err, ErrRequest) || strings.Contains(readIf(dir, "argv"), "create\n") {
		t.Errorf("system roots with no certificate: %v; want the start failed, nothing created", err)
	}
}
