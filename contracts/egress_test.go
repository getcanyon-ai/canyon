package contracts

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/destination"
)

// Every name the egress fixture carries on an allowlist, a request or the platform's list is one
// internal/destination keeps as it is (design/EGRESS.md §3.3). The schema's pattern cannot tell punycode
// that decodes to no label (xn--a) from a real A-label; Go's IDNA, which the check runs, can.
func TestEgressFixtureNamesAreCanonical(t *testing.T) {
	raw, err := os.ReadFile("testdata/egress/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	type env struct {
		Allowed  []string `json:"allowed"`
		Known    []string `json:"known"`
		Requests []struct {
			Destination string `json:"destination"`
		} `json:"requests"`
		Routes []struct {
			Destination string `json:"destination"`
		} `json:"routes"`
	}
	var doc struct {
		ProjectRead struct {
			Envs     map[string]env `json:"envs"`
			Platform []string       `json:"platform"`
		} `json:"project_read"`
		Policy struct {
			Envs     []env    `json:"envs"`
			Platform []string `json:"platform"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	names := append(doc.ProjectRead.Platform, doc.Policy.Platform...)
	for _, e := range append(doc.Policy.Envs, doc.ProjectRead.Envs["preview"], doc.ProjectRead.Envs["prod"]) {
		names = append(names, e.Allowed...)
		names = append(names, e.Known...)
		for _, r := range e.Requests {
			names = append(names, r.Destination)
		}
		for _, r := range e.Routes {
			names = append(names, r.Destination)
		}
	}
	if len(names) == 0 {
		t.Fatal("the fixture carries no name, so this proves nothing")
	}
	for _, n := range names {
		if got, err := destination.Entry(n); got != n || err != nil {
			t.Errorf("%q canonicalises to %q, %v: the fixture carries a name the check would not", n, got, err)
		}
	}
}

// CheckEgressPolicy checks the fields the policy definition names, strictly, and ignores every field it does not
// name, which a later server may add (contracts/routes.md, The check): an added field takes no egress down, and
// a named field out of shape, missing, or a name given twice is refused.
func TestEgressPolicyIgnoresFieldsItDoesNotKnow(t *testing.T) {
	raw, err := os.ReadFile("testdata/egress/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Policy map[string]any `json:"policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	with := func(f func(map[string]any)) []byte {
		var p map[string]any
		b, _ := json.Marshal(doc.Policy)
		_ = json.Unmarshal(b, &p)
		f(p)
		out, _ := json.Marshal(p)
		return out
	}
	if err := CheckEgressPolicy(with(func(p map[string]any) {
		p["later"] = map[string]any{"anything": 1}
		p["envs"].([]any)[0].(map[string]any)["later"] = []any{"x"}
		p["callers"].([]any)[0].(map[string]any)["later"] = true
	})); err != nil {
		t.Errorf("fields it does not know: %v, want ignored", err)
	}
	for what, policy := range map[string][]byte{
		"an env without known[]": with(func(p map[string]any) { delete(p["envs"].([]any)[0].(map[string]any), "known") }),
		"a caller by a name":     with(func(p map[string]any) { p["callers"].([]any)[0].(map[string]any)["address"] = "claims-portal" }),
		"a route without a rev": with(func(p map[string]any) {
			delete(p["envs"].([]any)[1].(map[string]any)["routes"].([]any)[0].(map[string]any), "rev")
		}),
		"no format":              with(func(p map[string]any) { delete(p, "format") }),
		"a name given twice":     []byte(`{"format": 1, "format": 1}`),
		"Callers beside callers": with(func(p map[string]any) { p["Callers"] = []any{} }),
		"ENVS beside envs":       with(func(p map[string]any) { p["ENVS"] = p["envs"] }),
		"a caller's App":         with(func(p map[string]any) { p["callers"].([]any)[0].(map[string]any)["App"] = "intake-bot" }),
		"callers given twice": func() []byte {
			b := with(func(map[string]any) {})
			return append(b[:len(b)-1], []byte(`,"callers":[]}`)...)
		}(),
	} {
		if err := CheckEgressPolicy(policy); err == nil {
			t.Errorf("%s: taken, want refused", what)
		}
	}
}

// A root the fixture's egress.ca carries is what the executor must take: one CA certificate, by Go's own parser,
// which the schema's pattern cannot be (contracts/executor.md, egress.ca).
func TestEgressCAFixtureIsACertificate(t *testing.T) {
	raw, err := os.ReadFile("testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Egress struct {
			CA []struct {
				PEM string `json:"pem"`
			} `json:"ca"`
		} `json:"egress_with_ca"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Egress.CA) == 0 {
		t.Fatal("the fixture carries no root, so this proves nothing")
	}
	for i, root := range doc.Egress.CA {
		block, rest := pem.Decode([]byte(root.PEM))
		if block == nil || len(rest) != 0 {
			t.Fatalf("root %d is not one PEM block", i)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("root %d: %v", i, err)
		}
		if !cert.IsCA {
			t.Errorf("root %d is no CA certificate", i)
		}
	}
}

// certificatesOf parses PEM as the contract writes it: certificates and nothing else.
func certificatesOf(t *testing.T, what, s string) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	rest := []byte(s)
	for len(rest) > 0 {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("%s is not certificates alone", what)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		out = append(out, cert)
	}
	return out
}

// The leaf the fixture answers is what Envoy serves for one inject destination (contracts/routes.md, GET
// /egress/leaf): one SAN, a destination a route of the fixture's policy names, 24 hours from its minting, signed by
// the chain's intermediate — a CA of 7 days that signs leaves alone, name-constrained to the inject destinations
// and no address. The schema's patterns cannot say any of it; Go's parser can.
func TestEgressLeafFixtureIsOneSANUnderAConstrainedIntermediate(t *testing.T) {
	raw, err := os.ReadFile("testdata/egress/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Leaf struct {
			Cert, Chain string
			NotAfter    time.Time `json:"not_after"`
		} `json:"leaf"`
		Policy struct {
			Envs []struct {
				Routes []struct {
					Destination string `json:"destination"`
				} `json:"routes"`
			} `json:"envs"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	inject := map[string]bool{}
	for _, e := range doc.Policy.Envs {
		for _, r := range e.Routes {
			inject[r.Destination] = true
		}
	}
	leaves, chain := certificatesOf(t, "the leaf", doc.Leaf.Cert), certificatesOf(t, "the chain", doc.Leaf.Chain)
	if len(leaves) != 1 || len(chain) == 0 {
		t.Fatalf("%d leaves and %d certificates of chain, want one leaf and its intermediate", len(leaves), len(chain))
	}
	leaf, inter := leaves[0], chain[0]
	if len(leaf.DNSNames) != 1 || !inject[leaf.DNSNames[0]] || len(leaf.IPAddresses)+len(leaf.URIs)+len(leaf.EmailAddresses) != 0 || leaf.IsCA {
		t.Fatalf("the leaf names %v %v %v %v, CA %t: one SAN, an inject destination, no CA", leaf.DNSNames, leaf.IPAddresses, leaf.URIs, leaf.EmailAddresses, leaf.IsCA)
	}
	if !leaf.NotAfter.Equal(doc.Leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+5*time.Minute {
		t.Errorf("the leaf is valid %s to %s, answered as ending %s: 24 hours from its minting", leaf.NotBefore, leaf.NotAfter, doc.Leaf.NotAfter)
	}
	if err := leaf.CheckSignatureFrom(inter); err != nil {
		t.Errorf("the leaf is not the intermediate's: %v", err)
	}
	if !inter.IsCA || !inter.MaxPathLenZero || inter.MaxPathLen != 0 || inter.NotAfter.Sub(inter.NotBefore) > 7*24*time.Hour+5*time.Minute {
		t.Errorf("the intermediate: CA %t, path length %d (zero %t), valid %s: a 7-day CA that signs leaves alone", inter.IsCA, inter.MaxPathLen, inter.MaxPathLenZero, inter.NotAfter.Sub(inter.NotBefore))
	}
	names := map[string]bool{}
	for _, n := range inter.PermittedDNSDomains {
		names[n] = true
	}
	if !inter.PermittedDNSDomainsCritical || len(names) != len(inject) || len(inter.ExcludedIPRanges) != 2 {
		t.Errorf("the intermediate permits %v (critical %t) and excludes %v: the inject destinations %v, and every address", inter.PermittedDNSDomains, inter.PermittedDNSDomainsCritical, inter.ExcludedIPRanges, inject)
	}
	for n := range inject {
		if !names[n] {
			t.Errorf("the intermediate does not permit %s, a route's destination", n)
		}
	}
}

// The sub-CA the rotation fixture supplies is what the route takes (contracts/routes.md, POST
// /api/v1/egress/ca/rotate): a CA that may sign a CA below it, its name constraints critical, permitting DNS
// subtrees none of them empty and every address excluded, each certificate signed by the next, the last
// self-signed. It is a fixture of its own: two keys in one file read as one to gitleaks.
func TestEgressRotateFixtureIsAConstrainedSubCA(t *testing.T) {
	raw, err := os.ReadFile("testdata/egress/byo-rotation.json")
	if err != nil {
		t.Fatal(err)
	}
	var rotate struct {
		Cert string `json:"cert"`
	}
	if err := json.Unmarshal(raw, &rotate); err != nil {
		t.Fatal(err)
	}
	certs := certificatesOf(t, "the sub-CA", rotate.Cert)
	sub, top := certs[0], certs[len(certs)-1]
	if !sub.IsCA || sub.KeyUsage&x509.KeyUsageCertSign == 0 || sub.MaxPathLenZero || !sub.PermittedDNSDomainsCritical || len(sub.PermittedDNSDomains) == 0 {
		t.Errorf("the sub-CA: CA %t, usage %v, path length zero %t, permits %v (critical %t)", sub.IsCA, sub.KeyUsage, sub.MaxPathLenZero, sub.PermittedDNSDomains, sub.PermittedDNSDomainsCritical)
	}
	for _, name := range sub.PermittedDNSDomains {
		if strings.TrimPrefix(name, ".") == "" {
			t.Errorf("the sub-CA permits %q, which matches every name", name)
		}
	}
	excluded := map[string]bool{}
	for _, r := range sub.ExcludedIPRanges {
		excluded[r.String()] = true
	}
	if len(sub.PermittedIPRanges) > 0 || !excluded["0.0.0.0/0"] || !excluded["::/0"] {
		t.Errorf("the sub-CA permits %v and excludes %v: every address excluded", sub.PermittedIPRanges, sub.ExcludedIPRanges)
	}
	for i := range certs[1:] {
		if err := certs[i].CheckSignatureFrom(certs[i+1]); err != nil {
			t.Errorf("certificate %d is not signed by the next: %v", i, err)
		}
	}
	if err := top.CheckSignatureFrom(top); err != nil || len(certs) < 2 {
		t.Errorf("%d certificates, the last self-signed: %v", len(certs), err)
	}
}
