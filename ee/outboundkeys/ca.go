package outboundkeys

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
)

// The interception chain (design/EGRESS.md §4.5; contracts/routes.md, GET /egress/leaf and POST
// /api/v1/egress/ca/rotate): a 7-day intermediate under the signing root, held in the server's memory alone and
// name-constrained to the inject destinations; a one-SAN, 24-hour leaf for each, which egress-control serves Envoy by
// SDS; and a rotation of the root, by an admin's command or on Canyon's own schedule, onto a root Canyon mints or an
// admin's sub-CA.

const (
	leafLife         = 24 * time.Hour
	intermediateLife = 7 * 24 * time.Hour
	rotationOverlap  = 60 * 24 * time.Hour // a rotation keeps the old root, in the bundle and signing at most, this long
	backdate         = 5 * time.Minute     // a leaf or an intermediate is valid from this long before it is minted
	rotateCheck      = time.Hour           // how often the server asks whether its own rotation is due
)

// injectDestinations reads the inject destinations: every destination an app env holds a route to, each once, sorted
// bytewise, which the intermediate permits.
const injectDestinations = `SELECT d FROM (SELECT DISTINCT destination AS d FROM egress_routes) routes ORDER BY d COLLATE "C"`

// everyAddress is what the intermediate's name constraints exclude: every IPv4 and IPv6 address, so nothing under it
// is valid for an address.
var everyAddress = []*net.IPNet{{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}, {IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}}

func sooner(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func pemOf(certs ...*x509.Certificate) string {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return string(out)
}

func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil
}

// mintIntermediate mints the intermediate under parent, signed by signer: a CA that signs leaves alone, its name
// constraints critical, permitting names and excluding every address, valid from backdate before now to
// intermediateLife after it or end, the sooner. No names is an error: an intermediate permitting none would be
// constrained by nothing.
func mintIntermediate(parent *x509.Certificate, signer crypto.Signer, names []string, now, end time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if len(names) == 0 {
		return nil, nil, errors.New("an intermediate permits at least one name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{Organization: []string{"Canyon"}, CommonName: "Canyon egress intermediate"},
		NotBefore:                   now.Add(-backdate),
		NotAfter:                    sooner(now.Add(intermediateLife), end),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         slices.Clone(names),
		ExcludedIPRanges:            everyAddress,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// mintLeaf mints destination's leaf under the intermediate: one SAN, its own P-256 key, for a server, valid from
// backdate before now to leafLife after it or the intermediate's end, the sooner. It answers the certificate and the
// key, PKCS #8, as PEM, and the leaf's end.
func mintLeaf(inter *x509.Certificate, signer crypto.Signer, destination string, now time.Time) (certPEM, keyPEM string, notAfter time.Time, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", time.Time{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return "", "", time.Time{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: destination},
		DNSNames:     []string{destination},
		NotBefore:    now.Add(-backdate),
		NotAfter:     sooner(now.Add(leafLife), inter.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, inter, &key.PublicKey, signer)
	if err != nil {
		return "", "", time.Time{}, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		tmpl.NotAfter.UTC().Truncate(time.Second), nil
}

// signingCA is the signing root as a leaf needs it: its fingerprint, its certificate and those above it (an
// admin's sub-CA's, the self-signed root last), its end, and how to open its key.
type signingCA struct {
	fingerprint string
	cert        *x509.Certificate
	above       []*x509.Certificate
	ends        time.Time
	open        func() (crypto.Signer, error)
}

// chain is what Envoy sends after a leaf under inter: the intermediate, then the signing root and those above it,
// but a self-signed root, which the app trusts already.
func (ca signingCA) chain(inter *x509.Certificate) string {
	out := []*x509.Certificate{inter}
	for _, c := range append([]*x509.Certificate{ca.cert}, ca.above...) {
		if !selfSigned(c) {
			out = append(out, c)
		}
	}
	return pemOf(out...)
}

// intermediates holds the server's one intermediate, in its memory alone: its key is never written anywhere.
type intermediates struct {
	mu    sync.Mutex
	root  string   // the signing root it was minted under
	names []string // the destinations it permits, sorted
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
}

// current is the intermediate for ca and names at now, minted again when the one held was minted under another
// root or for other names, or would end before a leaf minted now would and before ca does.
func (c *intermediates) current(ca signingCA, names []string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && c.root == ca.fingerprint && slices.Equal(c.names, names) && !c.cert.NotAfter.Before(sooner(now.Add(leafLife), ca.ends)) {
		return c.cert, c.key, nil
	}
	signer, err := ca.open()
	if err != nil {
		return nil, nil, err
	}
	cert, key, err := mintIntermediate(ca.cert, signer, names, now, ca.ends)
	if err != nil {
		return nil, nil, err
	}
	c.root, c.names, c.cert, c.key = ca.fingerprint, slices.Clone(names), cert, key
	return cert, key, nil
}

// Leaf is one inject destination's leaf for egress-control (contracts/routes.md, GET /egress/leaf), read on tx, a
// read-only snapshot: 404 no_route unless an app env holds a route to destination; 503 no_signing_root while no root
// is live; else a leaf minted at now for this answer and kept nowhere. No key is in an error.
func (k *Keys) Leaf(ctx context.Context, tx pgx.Tx, destination string, now time.Time) (enterprise.Leaf, error) {
	var answer enterprise.Leaf
	ca, names, err := signingCAOf(ctx, tx, k.keys)
	switch {
	case err != nil:
		return answer, err
	case !slices.Contains(names, destination):
		return answer, &enterprise.Refusal{Status: http.StatusNotFound, Code: "no_route", Message: "no app env holds a route to that destination"}
	case ca == nil:
		return answer, &enterprise.Refusal{Status: http.StatusServiceUnavailable, Code: "no_signing_root", Message: "no interception root is live"}
	}
	inter, key, err := k.chain.current(*ca, names, now)
	if err != nil {
		return answer, err
	}
	if answer.Cert, answer.Key, answer.NotAfter, err = mintLeaf(inter, key, destination, now); err != nil {
		return answer, err
	}
	answer.Root, answer.Chain = ca.fingerprint, ca.chain(inter)
	return answer, nil
}

// signingCAOf reads on tx, one snapshot, the destinations an app env holds a route to, sorted, and the signing root,
// nil while none is live.
func signingCAOf(ctx context.Context, tx pgx.Tx, keys *vault.Vault) (*signingCA, []string, error) {
	t, err := readEgressTrust(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, injectDestinations)
	if err != nil {
		return nil, nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || t.signing == "" {
		return nil, names, err
	}
	var certPEM string
	var above *string
	var keyRef int
	var sealedKey, sealedValue []byte
	c := signingCA{fingerprint: t.signing}
	if err := tx.QueryRow(ctx, "SELECT cert, chain, least(not_after, retires_at), key_ref, sealed_key, sealed_value FROM egress_ca_roots WHERE fingerprint = $1", t.signing).
		Scan(&certPEM, &above, &c.ends, &keyRef, &sealedKey, &sealedValue); err != nil {
		return nil, nil, err
	}
	var chain string
	if above != nil {
		chain = *above
	}
	certs, err := parseCertificates(certPEM + chain)
	if err != nil {
		return nil, nil, err
	}
	c.cert, c.above = certs[0], certs[1:]
	c.open = func() (crypto.Signer, error) {
		der, err := keys.Open(keyRef, "egress", "root", c.fingerprint, sealedKey, sealedValue)
		if err != nil {
			return nil, err
		}
		return signerOf(der)
	}
	return &c, names, nil
}

// parseCertificates parses PEM that is certificates and nothing else.
func parseCertificates(s string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(s)
	for len(bytes.TrimSpace(rest)) > 0 {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("not certificates alone")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}

// signerOf is a PKCS #8 key a root may sign with: ECDSA P-256 or P-384, or RSA of 2,048 bits or more.
func signerOf(der []byte) (crypto.Signer, error) {
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() {
			return k, nil
		}
	case *rsa.PrivateKey:
		if k.N.BitLen() >= 2048 {
			return k, nil
		}
	}
	return nil, errors.New("neither ECDSA P-256 or P-384 nor RSA of 2,048 bits or more")
}

// subCA is an admin's sub-CA, as a rotation takes it: its certificates, the sub-CA first and a self-signed root
// last, its key and the key's PKCS #8, which the vault seals and nothing reads back.
type subCA struct {
	certs  []*x509.Certificate
	signer crypto.Signer
	pkcs8  []byte
}

// rotationBody reads POST /api/v1/egress/ca/rotate's body (egress.schema.json, ca_rotate) at now: nil for {}, a root
// Canyon mints; the sub-CA for {cert, key}; or the invalid_ca reason, the first that holds, in the contract's order.
func rotationBody(r io.Reader, now time.Time) (*subCA, string) {
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10+1))
	var body map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err != nil || len(raw) > 64<<10 || dec.Decode(&body) != nil || body == nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		return nil, "body"
	}
	if len(body) == 0 {
		return nil, ""
	}
	var certPEM, keyPEM string
	if len(body) != 2 || json.Unmarshal(body["cert"], &certPEM) != nil || json.Unmarshal(body["key"], &keyPEM) != nil || certPEM == "" || keyPEM == "" {
		return nil, "body"
	}
	certs, err := parseCertificates(certPEM)
	if err != nil || len(certs) > 4 || pemOf(certs...) != certPEM || !selfSigned(certs[len(certs)-1]) {
		return nil, "cert"
	}
	for i, c := range certs {
		if now.Before(c.NotBefore) || !now.Before(c.NotAfter) || i+1 < len(certs) && c.CheckSignatureFrom(certs[i+1]) != nil {
			return nil, "cert"
		}
	}
	if sub := certs[0]; !sub.IsCA || !sub.BasicConstraintsValid || sub.KeyUsage&x509.KeyUsageCertSign == 0 || sub.MaxPathLen == 0 && sub.MaxPathLenZero {
		return nil, "ca"
	}
	block, rest := pem.Decode([]byte(keyPEM))
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 || string(pem.EncodeToMemory(block)) != keyPEM {
		return nil, "key"
	}
	signer, err := signerOf(block.Bytes)
	if err != nil {
		return nil, "key"
	}
	if pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(certs[0].PublicKey) {
		return nil, "key"
	}
	for _, c := range certs {
		if c.NotAfter.Before(now.Add(rotationOverlap)) {
			return nil, "expiring"
		}
	}
	sub := &subCA{certs: certs, signer: signer, pkcs8: block.Bytes}
	// a leaf for a name it permits verifies, routes or none: a key usage that excludes serving TLS, or a path length
	// above it, refuses every leaf. With no name to try it is refused as unconstrained, after.
	if name := sub.permittedName(); name != "" {
		if bad, err := sub.excludes([]string{name}, now); err != nil || len(bad) > 0 {
			return nil, "verify"
		}
	}
	return sub, ""
}

// permittedName is a name the sub-CA permits: its certificates' first permitted DNS subtree that is not empty, or a
// name below it for one with a leading dot; "" with none.
func (s *subCA) permittedName() string {
	for _, c := range s.certs {
		if selfSigned(c) {
			continue
		}
		for _, d := range c.PermittedDNSDomains {
			if strings.TrimPrefix(d, ".") == "" {
				continue
			}
			if strings.HasPrefix(d, ".") {
				return "canyon-probe" + d
			}
			return d
		}
	}
	return ""
}

// constrained is whether one certificate of the sub-CA but a self-signed root carries critical name constraints that
// permit at least one DNS subtree, none of them empty, a leading dot aside — an empty one matches every name — and
// leave no address open: a CA key constrained less in Canyon's hands would let a compromised Canyon sign for every
// name or address the org's devices trust (design/EGRESS.md §4.5; #2717's review).
func (s *subCA) constrained() bool {
	return slices.ContainsFunc(s.certs, func(c *x509.Certificate) bool {
		return !selfSigned(c) && c.PermittedDNSDomainsCritical && len(c.PermittedDNSDomains) > 0 &&
			!slices.ContainsFunc(c.PermittedDNSDomains, func(d string) bool { return strings.TrimPrefix(d, ".") == "" }) &&
			addressesConstrained(c)
	})
}

// addressesConstrained is whether c's name constraints leave no address open: permitted ranges, none of them a /0,
// or excluded ranges covering all of IPv4 (0.0.0.0/0) and IPv6 (::/0). A mask that is no prefix counts as a /0
// permitted and as no range excluded.
func addressesConstrained(c *x509.Certificate) bool {
	all := func(bits int) func(*net.IPNet) bool {
		return func(r *net.IPNet) bool { ones, size := r.Mask.Size(); return ones == 0 && size == bits }
	}
	open := func(r *net.IPNet) bool { ones, _ := r.Mask.Size(); return ones == 0 }
	permitted := len(c.PermittedIPRanges) > 0 && !slices.ContainsFunc(c.PermittedIPRanges, open)
	return permitted || slices.ContainsFunc(c.ExcludedIPRanges, all(32)) && slices.ContainsFunc(c.ExcludedIPRanges, all(128))
}

// excludes is each of names a leaf for which, under the intermediate Canyon would mint under the sub-CA, does not
// verify against the certificates given, by Go's crypto/x509: the sub-CA's own name constraints, or its key usages,
// refuse it.
func (s *subCA) excludes(names []string, now time.Time) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	end := s.certs[0].NotAfter
	for _, c := range s.certs {
		end = sooner(end, c.NotAfter)
	}
	inter, key, err := mintIntermediate(s.certs[0], s.signer, names, now, end)
	if err != nil {
		return nil, err
	}
	roots, between := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(s.certs[len(s.certs)-1])
	between.AddCert(inter)
	for _, c := range s.certs[:len(s.certs)-1] {
		between.AddCert(c)
	}
	var out []string
	for _, name := range names {
		certPEM, _, _, err := mintLeaf(inter, key, name, now)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode([]byte(certPEM))
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: between, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// rotationUnderWay is a rotation's 409 rotating: a root other than the newest has not reached its end, the latest
// such end ends.
type rotationUnderWay struct{ ends time.Time }

func (e *rotationUnderWay) Error() string { return "a rotation is under way" }

// caExcluded is a rotation's 409 ca_excludes: the destinations a sub-CA refuses a leaf for.
type caExcluded struct{ destinations []string }

func (e *caExcluded) Error() string { return "the sub-CA excludes a route's destination" }

// rotateRoot starts a rotation in tx, under the roots' table lock (contracts/routes.md, POST
// /api/v1/egress/ca/rotate): onto a root Canyon mints when sub is nil, else onto the admin's sub-CA. One at a time:
// *rotationUnderWay while a root other than the newest has not reached its end; *caExcluded for a sub-CA that refuses
// a leaf for a destination a route names. Else the new root, its key sealed as Canyon's own root's is, every other
// root's end brought to rotationOverlap from now when later, and one audit row egress.ca.rotate by actor, never a key.
func rotateRoot(ctx context.Context, tx pgx.Tx, keys *vault.Vault, sub *subCA, actor events.Actor) error {
	if _, err := tx.Exec(ctx, "LOCK TABLE egress_ca_roots IN EXCLUSIVE MODE"); err != nil {
		return err
	}
	var now time.Time
	var under *time.Time
	if err := tx.QueryRow(ctx, `SELECT now(), (SELECT max(e) FROM (SELECT least(not_after, retires_at) e FROM egress_ca_roots
		WHERE least(not_after, retires_at) > now() ORDER BY created_at DESC OFFSET 1) older)`).Scan(&now, &under); err != nil {
		return err
	}
	if under != nil {
		return &rotationUnderWay{under.UTC()}
	}
	var certPEM, fingerprint string
	var above *string
	var key []byte
	var notAfter time.Time
	if sub == nil {
		var err error
		if certPEM, fingerprint, key, notAfter, err = mintInterceptionRoot(now); err != nil {
			return err
		}
	} else {
		rows, err := tx.Query(ctx, injectDestinations)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		bad, err := sub.excludes(names, now)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			return &caExcluded{bad}
		}
		sum := sha256.Sum256(sub.certs[0].Raw)
		certPEM, fingerprint, key, notAfter = pemOf(sub.certs[0]), hex.EncodeToString(sum[:]), sub.pkcs8, sub.certs[0].NotAfter
		chain := pemOf(sub.certs[1:]...)
		above = &chain
		for _, c := range sub.certs {
			notAfter = sooner(notAfter, c.NotAfter)
		}
	}
	version, sealedKey, sealedValue, err := keys.Seal("egress", "root", fingerprint, key)
	if err != nil {
		return err
	}
	// a root supplied again — an admin's sub-CA back after it ended — comes back as the newest
	if _, err := tx.Exec(ctx, `INSERT INTO egress_ca_roots (fingerprint, cert, chain, byo, key_ref, sealed_key, sealed_value, not_after) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (fingerprint) DO UPDATE SET cert = EXCLUDED.cert, chain = EXCLUDED.chain, byo = EXCLUDED.byo, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key,
			sealed_value = EXCLUDED.sealed_value, not_after = EXCLUDED.not_after, created_at = now(), retires_at = NULL`,
		fingerprint, certPEM, above, sub != nil, version, sealedKey, sealedValue, notAfter); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE egress_ca_roots SET retires_at = now() + make_interval(secs => $2) WHERE fingerprint <> $1 AND least(not_after, retires_at) > now() + make_interval(secs => $2)",
		fingerprint, rotationOverlap.Seconds()); err != nil {
		return err
	}
	type previous struct {
		Root   string    `json:"root"`
		EndsAt time.Time `json:"ends_at"`
	}
	rows, err := tx.Query(ctx, "SELECT fingerprint, date_trunc('second', least(not_after, retires_at)) FROM egress_ca_roots WHERE fingerprint <> $1 AND least(not_after, retires_at) > now() ORDER BY created_at DESC", fingerprint)
	if err != nil {
		return err
	}
	replaced, err := pgx.CollectRows(rows, pgx.RowToStructByPos[previous])
	if err != nil {
		return err
	}
	for i := range replaced {
		replaced[i].EndsAt = replaced[i].EndsAt.UTC()
	}
	return events.AuditTx(ctx, tx, actor, "egress.ca.rotate", "", "", map[string]any{"root": fingerprint, "byo": sub != nil, "previous": append([]previous{}, replaced...)})
}

// RotateCA starts a rotation of the interception root in tx, an admin's from a browser, every refusal of the core's
// before it (contracts/routes.md, POST /api/v1/egress/ca/rotate): the body, then a sub-CA with no name constraints,
// then a rotation under way and a sub-CA that excludes a route's destination, each a *enterprise.Refusal.
func (k *Keys) RotateCA(ctx context.Context, tx pgx.Tx, body io.Reader, actor events.Actor, now time.Time) error {
	sub, reason := rotationBody(body, now)
	if reason != "" {
		return &enterprise.Refusal{Status: http.StatusBadRequest, Code: "invalid_ca", Message: "the body is {}, or a sub-CA's {cert, key} as the contract takes them", Fields: map[string]any{"reason": reason}}
	}
	if sub != nil && !sub.constrained() {
		return &enterprise.Refusal{Status: http.StatusConflict, Code: "ca_unconstrained", Message: caUnconstrained}
	}
	err := rotateRoot(ctx, tx, k.keys, sub, actor)
	var under *rotationUnderWay
	var excluded *caExcluded
	switch {
	case errors.As(err, &under):
		return &enterprise.Refusal{Status: http.StatusConflict, Code: "rotating", Message: "a rotation is under way", Fields: map[string]any{"ends_at": under.ends.Format(time.RFC3339)}}
	case errors.As(err, &excluded):
		return &enterprise.Refusal{Status: http.StatusConflict, Code: "ca_excludes", Message: "the sub-CA refuses a leaf for a destination a route names", Fields: map[string]any{"destinations": excluded.destinations}}
	}
	return err
}

// caUnconstrained is the 409 ca_unconstrained's message, D5's (design/EXPERIENCE.md).
const caUnconstrained = "Canyon takes only a sub-CA that is name-constrained to the destinations it adds credentials for, and that allows no IP addresses."

// rotateDue starts Canyon's own rotation when it is due (contracts/routes.md, POST /api/v1/egress/ca/rotate, on a
// schedule): the newest root is Canyon's own and ends within rotationOverlap, and no rotation is under way. An
// admin's sub-CA never rotates by itself. It says whether it started one.
func rotateDue(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) (bool, error) {
	var started bool
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// the append lock, which its audit row needs held (events.AuditTx), first, then the roots', the order every
		// audit appender keeps (events.LockTx): an admin's rotation, a mutation, holds the append lock when it asks
		// for the roots
		if err := events.LockTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "LOCK TABLE egress_ca_roots IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		var due bool
		err := tx.QueryRow(ctx, "SELECT NOT byo AND least(not_after, retires_at) <= now() + make_interval(secs => $1) FROM egress_ca_roots ORDER BY created_at DESC LIMIT 1",
			rotationOverlap.Seconds()).Scan(&due)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !due {
			return nil
		}
		if err != nil {
			return err
		}
		err = rotateRoot(ctx, tx, keys, nil, egressActor)
		var under *rotationUnderWay
		if errors.As(err, &under) {
			return nil
		}
		started = err == nil
		return err
	})
	return started && err == nil, err
}

// Start makes sure the host holds an interception root, then asks every rotateCheck whether Canyon's own rotation is
// due, until ctx ends: the server calls it as it starts serving the policy.
func (k *Keys) Start(ctx context.Context, pool *pgxpool.Pool, logger *log.Logger) error {
	if err := interceptionRoot(ctx, pool, k.keys); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(rotateCheck)
		defer t.Stop()
		for {
			if started, err := rotateDue(ctx, pool, k.keys); err != nil && ctx.Err() == nil {
				logger.Printf("canyon-server: egress: rotate the interception root: %v", err)
			} else if started {
				logger.Printf("canyon-server: egress: a rotation of the interception root started, 60 days before its end")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// CA reads the key dates on tx, one read-only snapshot: the signing root, each root whose end has not passed, the
// newest first, and when Canyon starts its next rotation itself — rotationOverlap before the newest root's end while
// it is Canyon's own, never an admin's sub-CA's.
func (k *Keys) CA(ctx context.Context, tx pgx.Tx) (enterprise.CARead, error) {
	out := enterprise.CARead{Roots: []enterprise.CARoot{}}
	t, err := readEgressTrust(ctx, tx)
	if err != nil {
		return out, err
	}
	if t.signing != "" {
		out.Signing = &t.signing
	}
	rows, err := tx.Query(ctx, `SELECT fingerprint, byo, date_trunc('second', created_at), date_trunc('second', not_after), date_trunc('second', least(not_after, retires_at))
		FROM egress_ca_roots WHERE least(not_after, retires_at) > now() ORDER BY created_at DESC`)
	if err != nil {
		return out, err
	}
	if out.Roots, err = pgx.AppendRows(out.Roots, rows, pgx.RowToStructByPos[enterprise.CARoot]); err != nil {
		return out, err
	}
	for i := range out.Roots {
		r := &out.Roots[i]
		r.CreatedAt, r.NotAfter, r.EndsAt = r.CreatedAt.UTC(), r.NotAfter.UTC(), r.EndsAt.UTC()
	}
	if len(out.Roots) > 0 && !out.Roots[0].BYO {
		at := out.Roots[0].EndsAt.Add(-rotationOverlap)
		out.RotatesAt = &at
	}
	return out, nil
}
