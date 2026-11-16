package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/ee/licence"
)

// keyFile writes a key made at test time as openssl genpkey writes one: no key is ever committed.
func keyFile(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "canyon-licence.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, pub
}

// The tool issues a licence the server's verifier reads, expiring the day after its last day; -public prints the
// key the server lists.
func TestTheToolIssuesALicenceTheServerReads(t *testing.T) {
	path, pub := keyFile(t)
	var out bytes.Buffer
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	if err := run([]string{"-key", path, "-customer", "Meridian Insurance", "-expires", "2027-11-13", "-seats", "50", "-features", "outbound_keys,group_sync"}, nil, &out, now); err != nil {
		t.Fatal(err)
	}
	p, problem := licence.Verify(bytes.TrimSpace(out.Bytes()), []ed25519.PublicKey{pub})
	if problem != "" || p.Customer != "Meridian Insurance" || !p.Issued.Equal(now) || !p.Expires.Equal(time.Date(2027, 11, 14, 0, 0, 0, 0, time.UTC)) || p.Seats != 50 || strings.Join(p.Features, ",") != "outbound_keys,group_sync" {
		t.Fatalf("%+v %q", p, problem)
	}
	out.Reset()
	if err := run([]string{"-key", path, "-public"}, nil, &out, now); err != nil || strings.TrimSpace(out.String()) != base64.StdEncoding.EncodeToString(pub) {
		t.Fatalf("%q %v", out.String(), err)
	}
}

// What the contract refuses is never issued: no seats, a feature twice, no key.
func TestTheToolIssuesNothingTheContractRefuses(t *testing.T) {
	path, _ := keyFile(t)
	for _, args := range [][]string{
		{"-key", path, "-customer", "Meridian Insurance", "-expires", "2027-11-13", "-features", "group_sync"},
		{"-key", path, "-customer", "Meridian Insurance", "-expires", "2027-11-13", "-seats", "5", "-features", "group_sync,group_sync"},
		{"-key", path, "-customer", "", "-expires", "2027-11-13", "-seats", "5", "-features", "group_sync"},
		{"-key", path, "-customer", "Meridian Insurance", "-expires", "13/11/2027", "-seats", "5", "-features", "group_sync"},
		{"-customer", "Meridian Insurance", "-expires", "2027-11-13", "-seats", "5", "-features", "group_sync"},
	} {
		var out bytes.Buffer
		if err := run(args, nil, &out, time.Now()); err == nil || out.Len() > 0 {
			t.Errorf("%q: issued %q", args, out.String())
		}
	}
}

// A seed in base64, from stdin as the Secrets Manager pipe hands it or from a file, signs the same licence a PEM key
// does; a seed that is not 32 bytes is refused without echoing it, and -key with -seed is refused.
func TestTheToolTakesASeedFromStdinOrAFile(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	file := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(file, []byte(seed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-customer", "Meridian Insurance", "-expires", "2027-11-13", "-seats", "5", "-features", "group_sync"}
	for name, c := range map[string]struct {
		flags []string
		in    string
	}{"stdin": {[]string{"-seed", "-"}, seed + "\n"}, "a file": {[]string{"-seed", file}, ""}} {
		var out bytes.Buffer
		if err := run(append(c.flags, args...), strings.NewReader(c.in), &out, time.Now()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, problem := licence.Verify(bytes.TrimSpace(out.Bytes()), []ed25519.PublicKey{pub}); problem != "" || strings.Contains(out.String(), seed) {
			t.Errorf("%s: %q, or the seed printed", name, problem)
		}
		out.Reset()
		if err := run(append(c.flags, "-public"), strings.NewReader(c.in), &out, time.Now()); err != nil || strings.TrimSpace(out.String()) != base64.StdEncoding.EncodeToString(pub) {
			t.Errorf("%s -public: %q %v", name, out.String(), err)
		}
	}
	short := base64.StdEncoding.EncodeToString([]byte("not thirty-two bytes"))
	if err := run(append([]string{"-seed", "-"}, args...), strings.NewReader(short), &bytes.Buffer{}, time.Now()); err == nil || strings.Contains(err.Error(), short) {
		t.Errorf("a short seed: %v", err)
	}
	pem, _ := keyFile(t)
	if err := run(append([]string{"-seed", file, "-key", pem}, args...), nil, &bytes.Buffer{}, time.Now()); err == nil {
		t.Error("-key with -seed issued")
	}
}
