package outboundkeys

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/internal/subjecthash"
)

// trustsCanyon is whether a /run with egress carries the host's roots, egress.ca (#2143; contracts/executor.md,
// egress.ca): the host's credential routes, an admin's setting, off until turned on (the owner's decision,
// 2026-11-09). While on, every env with egress trusts Canyon's interception root from its next deploy, so a route
// works with no redeploy; while off none is given it, and one given it before keeps it until its next deploy.
func trustsCanyon(ctx context.Context, q enterprise.Querier) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, "SELECT credential_routes FROM egress_settings").Scan(&on)
	return on, err
}

// ownTrust is the variables egress.ca would set; an env whose runtime values name one keeps its own and carries no
// root, so nothing of it is intercepted (contracts/executor.md, egress.ca).
var ownTrust = []string{"SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"}

// maxCA is egress.ca's bound: one to 8 certificates.
const maxCA = 8

// Roots is what app env's /run with egress carries of the host's roots: egress.ca, each live root — and above a
// BYO sub-CA, its chain — under its OpenSSL subject-hash name, the newest root first; and the roots it carries, by
// fingerprint, which the deploy row records (runs.Carried.Roots). None when the env names one of ownTrust, the host's
// credential routes are off, or no root is live. More than 8 certificates fails the /run: never a part of the host's trust.
func (k *Keys) Roots(ctx context.Context, q enterprise.Querier, app, env string, values map[string]string) ([]executor.Root, []string, error) {
	for _, k := range ownTrust {
		if _, ok := values[k]; ok {
			return nil, []string{}, nil
		}
	}
	if ok, err := trustsCanyon(ctx, q); err != nil || !ok {
		return nil, []string{}, err
	}
	rows, err := q.Query(ctx, "SELECT fingerprint, cert, coalesce(chain, '') FROM egress_ca_roots WHERE least(not_after, retires_at) > now() ORDER BY created_at DESC")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ca []executor.Root
	fingerprints := []string{}
	var seen [][]byte // each certificate once, a chain shared by two roots among them
	for rows.Next() {
		var fingerprint, cert, chain string
		if err := rows.Scan(&fingerprint, &cert, &chain); err != nil {
			return nil, nil, err
		}
		fingerprints = append(fingerprints, fingerprint)
		rest := []byte(cert + chain)
		for {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
				break
			}
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, fmt.Errorf("outbound keys: root %s: %w", fingerprint, err)
			}
			if slices.ContainsFunc(seen, func(der []byte) bool { return string(der) == string(c.Raw) }) {
				continue
			}
			seen = append(seen, c.Raw)
			hash, err := subjecthash.Of(c.RawSubject)
			if err != nil {
				return nil, nil, fmt.Errorf("outbound keys: root %s: %w", fingerprint, err)
			}
			n := 0
			for slices.ContainsFunc(ca, func(r executor.Root) bool { return r.Name == fmt.Sprintf("%s.%d", hash, n) }) {
				n++
			}
			if n > 9 {
				return nil, nil, fmt.Errorf("outbound keys: ten roots of one subject hash, %s", hash)
			}
			ca = append(ca, executor.Root{Name: fmt.Sprintf("%s.%d", hash, n), PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(ca) > maxCA {
		return nil, nil, errors.New("outbound keys: the host's roots are more than 8 certificates, egress.ca's bound")
	}
	if len(ca) == 0 {
		return nil, []string{}, nil
	}
	return ca, fingerprints, nil
}

// RoutePlaceholder is what a route's placeholder variable carries, so an SDK that needs a key starts; it carries no
// authority: the route is chosen by caller and destination, and the header overwritten whatever the app sent
// (design/EGRESS.md §4.1).
const RoutePlaceholder = "canyon-adds-the-real-key"

// Placeholders gives each route of app env that names a placeholder variable RoutePlaceholder in values, unless the
// env holds a runtime value of that name.
func (k *Keys) Placeholders(ctx context.Context, q enterprise.Querier, app, env string, values map[string]string) error {
	rows, err := q.Query(ctx, "SELECT placeholder_env FROM egress_routes WHERE project_id = $1 AND env = $2 AND placeholder_env IS NOT NULL", app, env)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, name := range names {
		if _, held := values[name]; !held {
			values[name] = RoutePlaceholder
		}
	}
	return err
}
