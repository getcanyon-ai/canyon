package outboundkeys

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
)

// The interception root and trust (design/EGRESS.md §4.4, §4.5; contracts/routes.md, Interception, Trust): Canyon
// intercepts an env's routes only while the root signing the current leaves — the signing root — is among the env's
// trusted roots, the roots every instance of it that may be serving trusts, which its deploy row records.

// rootYears is how long an interception root lasts (design/EGRESS.md §4.5).
const rootYears = 5

// interceptionRoot makes sure the host holds an interception root: the first start that serves the policy and finds
// none mints one, its key sealed as a secret is under ("egress", "root", <fingerprint>). The table lock makes two
// first starts write one.
func interceptionRoot(ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "LOCK TABLE egress_ca_roots IN EXCLUSIVE MODE"); err != nil {
			return err
		}
		var held bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM egress_ca_roots)").Scan(&held); err != nil || held {
			return err
		}
		certPEM, fingerprint, der, notAfter, err := mintInterceptionRoot(time.Now())
		if err != nil {
			return err
		}
		version, sealedKey, sealedValue, err := keys.Seal("egress", "root", fingerprint, der)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO egress_ca_roots (fingerprint, cert, key_ref, sealed_key, sealed_value, not_after) VALUES ($1, $2, $3, $4, $5, $6)",
			fingerprint, certPEM, version, sealedKey, sealedValue, notAfter)
		return err
	})
}

// mintInterceptionRoot makes an interception root, self-signed P-256, rootYears: a CA that signs one intermediate
// below it (design/EGRESS.md §4.5). It answers the certificate's PEM, its SHA-256 fingerprint, the key's PKCS #8 and
// the certificate's end.
func mintInterceptionRoot(now time.Time) (certPEM, fingerprint string, key []byte, notAfter time.Time, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", nil, time.Time{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return "", "", nil, time.Time{}, err
	}
	notAfter = now.AddDate(rootYears, 0, 0).UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Canyon"}, CommonName: "Canyon egress root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", nil, time.Time{}, err
	}
	if key, err = x509.MarshalPKCS8PrivateKey(priv); err != nil {
		return "", "", nil, time.Time{}, err
	}
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), hex.EncodeToString(sum[:]), key, notAfter, nil
}

// newSerial is a certificate's serial number: 128 random bits.
func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// egressRoot is one interception root as the signing root is chosen among them: ends is its end, the sooner of its
// certificate's and 60 days after the rotation that replaced it began (retires_at).
type egressRoot struct {
	fingerprint   string
	created, ends time.Time
}

// egressSigningRoot is the root signing the current leaves (design/EGRESS.md §4.5): leaves are signed under one root
// at a time, and on a rotation or a switch to a BYO sub-CA the old root signs on until no env holding a route lacks
// the new one. So among the roots in force at now, the newest that every holder's trusted roots carry signs, and with
// none such the oldest in force does: a holder lacking the new root keeps working under the old until the old one
// ends, then waits. holders are the trusted roots of each env holding a route that Canyon runs; none holding one, the
// newest signs. "" when no root is in force: no env's routes are named.
func egressSigningRoot(roots []egressRoot, holders [][]string, now time.Time) string {
	live := slices.DeleteFunc(slices.Clone(roots), func(r egressRoot) bool { return !now.Before(r.ends) })
	if len(live) == 0 {
		return ""
	}
	slices.SortStableFunc(live, func(a, b egressRoot) int { return a.created.Compare(b.created) })
	for i := len(live) - 1; i >= 0; i-- {
		if !slices.ContainsFunc(holders, func(trusted []string) bool { return !slices.Contains(trusted, live[i].fingerprint) }) {
			return live[i].fingerprint
		}
	}
	return live[0].fingerprint
}

// egressTrust is what decides whether an env's routes are named (contracts/routes.md, Interception, Trust): host,
// whether the host's via is canyon, its run_by canyon and its credential routes on (#2143); the signing root; and
// each deploy row's trusted roots, nothing recorded being none.
type egressTrust struct {
	host    bool
	signing string
	trusted map[appEnv][]string
}

type appEnv struct{ app, env string }

// live is whether the policy names app env's routes: the host routes its calls through Canyon, and the signing
// root is among the env's trusted roots.
func (t egressTrust) live(app, env string) bool {
	return t.host && t.signing != "" && slices.Contains(t.trusted[appEnv{app, env}], t.signing)
}

// readEgressTrust reads egressTrust on q, in the caller's snapshot where it has one: the host's via and run_by, every
// root, every deploy row's trusted roots and which envs hold a route, the signing root chosen at the database's now.
func readEgressTrust(ctx context.Context, q enterprise.Querier) (egressTrust, error) {
	t := egressTrust{trusted: map[appEnv][]string{}}
	var now time.Time
	if err := q.QueryRow(ctx, "SELECT (SELECT via = 'canyon' AND credential_routes FROM egress_settings) AND (SELECT run_by FROM org_preferences) = 'canyon', now()").Scan(&t.host, &now); err != nil {
		return t, err
	}
	rows, err := q.Query(ctx, "SELECT fingerprint, created_at, least(not_after, retires_at) FROM egress_ca_roots")
	if err != nil {
		return t, err
	}
	var roots []egressRoot
	var r egressRoot
	if _, err := pgx.ForEachRow(rows, []any{&r.fingerprint, &r.created, &r.ends}, func() error { roots = append(roots, r); return nil }); err != nil {
		return t, err
	}
	rows, err = q.Query(ctx, "SELECT project_id, env, coalesce(egress_roots, '{}') FROM deploys")
	if err != nil {
		return t, err
	}
	var k appEnv
	var trusted []string
	if _, err := pgx.ForEachRow(rows, []any{&k.app, &k.env, &trusted}, func() error { t.trusted[k] = trusted; trusted = nil; return nil }); err != nil {
		return t, err
	}
	// an env holding a route holds the old root back only while Canyon runs it: one with no deploy row, or one the org
	// runs, has no instance Canyon intercepts, and its routes wait, as it trusts nothing (contracts/routes.md, Trust)
	rows, err = q.Query(ctx, "SELECT DISTINCT r.project_id, r.env FROM egress_routes r JOIN deploys d USING (project_id, env) WHERE d.run_by = 'canyon'")
	if err != nil {
		return t, err
	}
	var holders [][]string
	if _, err := pgx.ForEachRow(rows, []any{&k.app, &k.env}, func() error { holders = append(holders, t.trusted[k]); return nil }); err != nil {
		return t, err
	}
	t.signing = egressSigningRoot(roots, holders, now)
	return t, nil
}
