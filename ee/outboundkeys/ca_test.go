package outboundkeys

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/db"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/pgtest"
	"github.com/getcanyon-ai/canyon/vault"
)

// mintedRoot is an interception root as Canyon mints one, parsed, with its signer.
func mintedRoot(t *testing.T, now time.Time) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	certPEM, _, der, _, err := mintInterceptionRoot(now)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := parseCertificates(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signerOf(der)
	if err != nil {
		t.Fatal(err)
	}
	return certs[0], signer
}

// leafOf parses one certificate's PEM.
func leafOf(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	certs, err := parseCertificates(certPEM)
	if err != nil || len(certs) != 1 {
		t.Fatalf("one certificate: %d, %v", len(certs), err)
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
	n, err := newSerial()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// design/EGRESS.md §4.5: "The server keeps a 7-day intermediate with nameConstraints = the union of inject
// destinations." It is a CA under the root that signs leaves alone, its name constraints critical, permitting the
// destinations and excluding every address, for 7 days or to the root's end; a leaf under it for a destination
// verifies, and one for any other name, or naming an address, does not, whoever minted it. With no destination it
// mints nothing: an intermediate permitting none would be constrained by nothing.
func TestIntermediateConstrainedToInjectDestinations(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	root, signer := mintedRoot(t, now)
	names := []string{"api.stripe.com", "hooks.slack.com"}
	inter, key, err := mintIntermediate(root, signer, names, now, root.NotAfter)
	if err != nil {
		t.Fatal(err)
	}
	if !inter.IsCA || !inter.MaxPathLenZero || inter.MaxPathLen != 0 || inter.KeyUsage != x509.KeyUsageCertSign || inter.CheckSignatureFrom(root) != nil {
		t.Fatalf("the intermediate: CA %t, path length %d (zero %t), usage %v: a CA under the root that signs leaves alone", inter.IsCA, inter.MaxPathLen, inter.MaxPathLenZero, inter.KeyUsage)
	}
	if !inter.PermittedDNSDomainsCritical || !slices.Equal(inter.PermittedDNSDomains, names) || len(inter.ExcludedIPRanges) != 2 ||
		inter.ExcludedIPRanges[0].String() != "0.0.0.0/0" || inter.ExcludedIPRanges[1].String() != "::/0" {
		t.Fatalf("the intermediate permits %v (critical %t) and excludes %v: the inject destinations, and every address", inter.PermittedDNSDomains, inter.PermittedDNSDomainsCritical, inter.ExcludedIPRanges)
	}
	if !inter.NotAfter.Equal(now.Add(7*24*time.Hour)) || !inter.NotBefore.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("the intermediate is valid %s to %s: from 5 minutes before it was minted, 7 days", inter.NotBefore, inter.NotAfter)
	}
	for _, name := range names {
		certPEM, _, _, err := mintLeaf(inter, key, name, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifies(leafOf(t, certPEM), name, root, now, inter); err != nil {
			t.Errorf("a leaf for %s, an inject destination: %v", name, err)
		}
	}
	for name, tmpl := range map[string]*x509.Certificate{
		"another name":            {DNSNames: []string{"api.openai.com"}},
		"the name above one":      {DNSNames: []string{"stripe.com"}},
		"an address beside it":    {DNSNames: []string{"api.stripe.com"}, IPAddresses: []net.IP{net.ParseIP("3.18.12.63")}},
		"an IPv6 address beside":  {DNSNames: []string{"api.stripe.com"}, IPAddresses: []net.IP{net.ParseIP("2606:4700::1")}},
		"a name and another name": {DNSNames: []string{"api.stripe.com", "api.openai.com"}},
	} {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.SerialNumber, tmpl.NotBefore, tmpl.NotAfter, tmpl.ExtKeyUsage = serialOf(t), now.Add(-time.Minute), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, inter, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(der)
		if err := verifies(leaf, tmpl.DNSNames[0], root, now, inter); err == nil {
			t.Errorf("a leaf for %s verified under the intermediate", name)
		}
	}
	if capped, _, err := mintIntermediate(root, signer, names, now, now.Add(48*time.Hour)); err != nil || !capped.NotAfter.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("an intermediate under a root ending in 2 days ends with it: %v, %v", capped.NotAfter, err)
	}
	if _, _, err := mintIntermediate(root, signer, nil, now, root.NotAfter); err == nil {
		t.Fatal("an intermediate permitting no name was minted: it would be constrained by nothing")
	}
}

// design/EGRESS.md §4.5: "mints one-SAN 24-hour leaves for SDS." A leaf names its destination alone, for a server,
// from 5 minutes before it was minted to 24 hours after, or to the intermediate's end when sooner; its key is its own,
// PKCS #8.
func TestLeafIsOneSANFor24Hours(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	root, signer := mintedRoot(t, now)
	inter, key, err := mintIntermediate(root, signer, []string{"api.stripe.com"}, now, root.NotAfter)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, notAfter, err := mintLeaf(inter, key, "api.stripe.com", now)
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf(t, certPEM)
	if !slices.Equal(leaf.DNSNames, []string{"api.stripe.com"}) || len(leaf.IPAddresses)+len(leaf.URIs)+len(leaf.EmailAddresses) != 0 || leaf.IsCA ||
		!slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Fatalf("the leaf names %v %v %v %v, CA %t, for %v: one SAN, a server's", leaf.DNSNames, leaf.IPAddresses, leaf.URIs, leaf.EmailAddresses, leaf.IsCA, leaf.ExtKeyUsage)
	}
	if !leaf.NotBefore.Equal(now.Add(-5*time.Minute)) || !leaf.NotAfter.Equal(now.Add(24*time.Hour)) || !notAfter.Equal(leaf.NotAfter) {
		t.Fatalf("the leaf is valid %s to %s, answered as ending %s: 24 hours", leaf.NotBefore, leaf.NotAfter, notAfter)
	}
	block, _ := pem.Decode([]byte(keyPEM))
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil || block.Type != "PRIVATE KEY" || !priv.(*ecdsa.PrivateKey).PublicKey.Equal(leaf.PublicKey) {
		t.Fatalf("the leaf's key is its own PKCS #8: %s, %v", block.Type, err)
	}
	late, lateKey, err := mintIntermediate(root, signer, []string{"api.stripe.com"}, now, now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, end, err := mintLeaf(late, lateKey, "api.stripe.com", now); err != nil || !end.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("a leaf under an intermediate ending in 3 hours ends %s, %v: with it", end, err)
	}
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

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// contracts/routes.md, POST /api/v1/egress/ca/rotate: the body is {} or exactly {cert, key}, and invalid_ca's reason
// the first that holds — body, cert, ca, key, expiring, verify — so an admin is told what is wrong with what they
// gave. verify needs no route: a leaf for a name the sub-CA permits must verify, so a sub-CA for clients alone, or one
// under a root that signs no CA below it, is refused before any route could find it out.
func TestBYOBodyIsReadInTheContractsOrder(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	org, other := newOrgCA(t, now, nil, "stripe.com"), newOrgCA(t, now, nil, "stripe.com")
	cert, key := pemOf(org.sub, org.root), keyPEMOf(t, org.key)
	notCA := newOrgCA(t, now, func(c *x509.Certificate) { c.IsCA, c.MaxPathLen, c.KeyUsage = false, -1, x509.KeyUsageDigitalSignature }, "stripe.com")
	zero := newOrgCA(t, now, func(c *x509.Certificate) { c.MaxPathLen, c.MaxPathLenZero = 0, true }, "stripe.com")
	short := newOrgCA(t, now, func(c *x509.Certificate) { c.NotAfter = now.AddDate(0, 0, 30) }, "stripe.com")
	expired := newOrgCA(t, now, func(c *x509.Certificate) { c.NotBefore, c.NotAfter = now.AddDate(-2, 0, 0), now.AddDate(0, 0, -1) }, "stripe.com")
	clients := newOrgCA(t, now, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, "stripe.com")
	flat := orgCAWith(t, now, func(c *x509.Certificate) { c.MaxPathLen, c.MaxPathLenZero = 0, true }, nil, "stripe.com")
	dotted := newOrgCA(t, now, nil, ".stripe.com")
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, body, reason string
	}{
		{"{}: a root Canyon mints", `{}`, ""},
		{"a sub-CA as the contract takes it", org.body(t), ""},
		{"no object", `[]`, "body"},
		{"null", `null`, "body"},
		{"a sub-CA and no key", `{"cert":` + string(mustJSON(cert)) + `}`, "body"},
		{"a field it does not take", `{"cert":` + string(mustJSON(cert)) + `,"key":` + string(mustJSON(key)) + `,"fingerprint":"x"}`, "body"},
		{"something after the object", org.body(t) + `{}`, "body"},
		{"past 64 KiB", certKey(strings.Repeat("x", 64<<10), key), "body"},
		{"no certificate", certKey("not a certificate", key), "cert"},
		{"words before the certificates", certKey("issued by the PKI team\n"+cert, key), "cert"},
		{"the sub-CA without the root above it", certKey(pemOf(org.sub), key), "cert"},
		{"the root first", certKey(pemOf(org.root, org.sub), key), "cert"},
		{"a sub-CA another root signed", certKey(pemOf(org.sub, other.root), key), "cert"},
		{"five certificates", certKey(pemOf(org.sub, org.root, org.root, org.root, org.root), key), "cert"},
		{"a key in the certificates' place", certKey(key, key), "cert"},
		{"a sub-CA past its end", certKey(pemOf(expired.sub, expired.root), keyPEMOf(t, expired.key)), "cert"},
		{"a certificate that is no CA", certKey(pemOf(notCA.sub, notCA.root), keyPEMOf(t, notCA.key)), "ca"},
		{"a CA that signs no CA below it", certKey(pemOf(zero.sub, zero.root), keyPEMOf(t, zero.key)), "ca"},
		{"another sub-CA's key", certKey(cert, keyPEMOf(t, other.key)), "key"},
		{"a key that is no PKCS #8", certKey(cert, strings.ReplaceAll(key, "PRIVATE KEY", "EC PRIVATE KEY")), "key"},
		{"a key on a curve no root takes", certKey(cert, keyPEMOf(t, p224)), "key"},
		{"a sub-CA ending within 60 days", certKey(pemOf(short.sub, short.root), keyPEMOf(t, short.key)), "expiring"},
		{"a sub-CA for clients alone", clients.body(t), "verify"},
		{"a sub-CA under a root that signs no CA below it", flat.body(t), "verify"},
		{"a sub-CA permitting the names below one, its probe a name below", dotted.body(t), ""},
	} {
		sub, reason := rotationBody(strings.NewReader(c.body), now)
		if reason != c.reason || (reason == "" && c.body != `{}`) != (sub != nil) {
			t.Errorf("%s: reason %q, a sub-CA %t; want %q", c.name, reason, sub != nil, c.reason)
		}
	}
}

// design/EGRESS.md §4.5: the sub-CA an admin supplies must carry name constraints of its own — critical, permitting
// at least one DNS subtree, none of them empty, and leaving no address open — as a CA key constrained less in Canyon's
// hands would let a compromised Canyon sign for every name or address the org's devices trust: a permitted subtree of
// "" matches every name (#2717's review). And they must permit every destination a route names: Canyon mints its
// intermediate under the sub-CA and a leaf for each, and Go's crypto/x509 verifies each against the certificates given.
func TestBYOMustBeNameConstrainedAndCoverTheRoutes(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	read := func(o orgCA) *subCA {
		t.Helper()
		sub, reason := rotationBody(strings.NewReader(o.body(t)), now)
		if reason != "" {
			t.Fatalf("the sub-CA was refused: %s", reason)
		}
		return sub
	}
	_, v4, _ := net.ParseCIDR("0.0.0.0/0")
	_, docs, _ := net.ParseCIDR("192.0.2.0/24")
	for name, o := range map[string]orgCA{
		"no name constraints":           newOrgCA(t, now, nil),
		"name constraints not critical": newOrgCA(t, now, func(c *x509.Certificate) { c.PermittedDNSDomainsCritical = false }, "stripe.com"),
		"names excluded alone": newOrgCA(t, now, func(c *x509.Certificate) {
			c.PermittedDNSDomainsCritical, c.ExcludedDNSDomains, c.ExcludedIPRanges = true, []string{"meridian.example"}, everyAddress
		}),
		`a permitted subtree of "", which matches every name`: newOrgCA(t, now, nil, ""),
		`a permitted subtree of "" beside stripe.com`:         newOrgCA(t, now, nil, "", "stripe.com"),
		"names permitted, addresses left open":                newOrgCA(t, now, func(c *x509.Certificate) { c.ExcludedIPRanges = nil }, "stripe.com"),
		"every IPv4 address permitted": newOrgCA(t, now, func(c *x509.Certificate) {
			c.PermittedIPRanges, c.ExcludedIPRanges = []*net.IPNet{v4}, nil
		}, "stripe.com"),
		"IPv4 excluded, IPv6 left open": newOrgCA(t, now, func(c *x509.Certificate) { c.ExcludedIPRanges = []*net.IPNet{v4} }, "stripe.com"),
	} {
		if read(o).constrained() {
			t.Errorf("a sub-CA with %s was taken as name-constrained", name)
		}
	}
	for name, o := range map[string]orgCA{
		"names permitted and every address excluded": newOrgCA(t, now, nil, "stripe.com", "slack.com"),
		"names permitted and one range of addresses": newOrgCA(t, now, func(c *x509.Certificate) {
			c.PermittedIPRanges, c.ExcludedIPRanges = []*net.IPNet{docs}, nil
		}, "stripe.com"),
		"names below one permitted and every address excluded": newOrgCA(t, now, nil, ".stripe.com"),
	} {
		if !read(o).constrained() {
			t.Errorf("a sub-CA with %s was not taken as name-constrained", name)
		}
	}
	sub := read(newOrgCA(t, now, nil, "stripe.com", "slack.com"))
	for _, c := range []struct{ names, want []string }{
		{nil, nil},
		{[]string{"api.stripe.com", "hooks.slack.com"}, nil},
		{[]string{"api.openai.com", "api.stripe.com", "hooks.slack.com"}, []string{"api.openai.com"}},
	} {
		if got, err := sub.excludes(c.names, now); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("routes to %v: the sub-CA excludes %v, %v; want %v", c.names, got, err, c.want)
		}
	}
}

// host is a migrated schema holding an interception root, as the first start serving the policy mints one.
type host struct {
	pool *pgxpool.Pool
	ctx  context.Context
	keys *vault.Vault
}

func rooted(t *testing.T) host {
	t.Helper()
	pool, ctx := pgtest.Schema(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	if err := interceptionRoot(ctx, pool, keys); err != nil {
		t.Fatal(err)
	}
	return host{pool: pool, ctx: ctx, keys: keys}
}

func (h host) exec(t *testing.T, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := h.pool.Exec(h.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// contracts/routes.md, POST /api/v1/egress/ca/rotate, on a schedule: Canyon starts a rotation itself 60 days before
// its own newest root's end — not 61 — its row's actor canyon, and not again once the new root is newest; an admin's
// sub-CA never rotates by itself, however near its end.
func TestRotationStartsItself60DaysBeforeTheEnd(t *testing.T) {
	h := rooted(t)
	keys := h.keys
	h.exec(t, "UPDATE egress_ca_roots SET not_after = now() + interval '61 days'")
	if started, err := rotateDue(h.ctx, h.pool, keys); err != nil || started || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 1 {
		t.Fatalf("61 days before the end: started %t, %v", started, err)
	}
	h.exec(t, "UPDATE egress_ca_roots SET not_after = now() + interval '59 days'")
	if started, err := rotateDue(h.ctx, h.pool, keys); err != nil || !started || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 2 {
		t.Fatalf("59 days before the end: started %t, %v", started, err)
	}
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM audit_events WHERE action = 'egress.ca.rotate' AND actor_kind = 'canyon' AND NOT (detail->>'byo')::boolean"); n != 1 {
		t.Fatalf("%d egress.ca.rotate rows by Canyon", n)
	}
	if started, err := rotateDue(h.ctx, h.pool, keys); err != nil || started {
		t.Fatalf("the new root newest: started %t, %v", started, err)
	}
	// Canyon's roots ended, so no rotation is under way: only the sub-CA's own end could start one
	h.exec(t, "UPDATE egress_ca_roots SET retires_at = now() - interval '1 second'",
		"INSERT INTO egress_ca_roots (fingerprint, cert, chain, byo, key_ref, sealed_key, sealed_value, created_at, not_after) VALUES ('"+strings.Repeat("b", 64)+"', '', '', true, 1, '', '', now() + interval '1 second', now() + interval '10 days')")
	if started, err := rotateDue(h.ctx, h.pool, keys); err != nil || started || count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_ca_roots") != 3 {
		t.Fatalf("an admin's sub-CA newest and alone live, ending in 10 days: started %t, %v", started, err)
	}
}

// events.LockTx: every audit appender holds the append lock, which keeps the trail's ts in order (events.AuditTx
// takes none itself), and takes it first, before its own, so no two wait on each other. Here a mutation — an
// admin's rotation — holds the append lock while the schedule's rotation starts: the rotation waits on it before it
// takes anything, so the mutation can still take the roots' lock and finish, and the rotation starts after it.
func TestRotationTakesTheAuditLockFirst(t *testing.T) {
	h := rooted(t)
	keys := h.keys
	h.exec(t, "UPDATE egress_ca_roots SET not_after = now() + interval '59 days'")
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	var pid int
	if err := events.LockTx(h.ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(h.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	type result struct {
		started bool
		err     error
	}
	done := make(chan result, 1)
	go func() { started, err := rotateDue(h.ctx, h.pool, keys); done <- result{started, err} }()
	for deadline := time.Now().Add(10 * time.Second); count(t, h.ctx, h.pool, fmt.Sprintf("SELECT count(*) FROM pg_stat_activity WHERE %d = ANY(pg_blocking_pids(pid))", pid)) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the rotation never waited on the mutation holding the append lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := tx.Exec(h.ctx, "LOCK TABLE egress_ca_roots IN EXCLUSIVE MODE"); err != nil {
		t.Fatalf("the mutation holding the append lock could not take the roots' lock: %v", err)
	}
	if err := tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || !r.started {
		t.Fatalf("the rotation after the mutation: started %t, %v", r.started, r.err)
	}
}
