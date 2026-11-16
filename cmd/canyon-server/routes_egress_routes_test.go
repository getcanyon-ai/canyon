package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	neturl "net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/vault"
)

// routeValue is a route's value in these tests: a synthetic shape, never a credential.
const routeValue = "canyon-fixture-not-a-credential-7d2f9e41"

func routePath(env, destination string) string {
	return outboundApp + "/" + env + "/routes/" + neturl.PathEscape(destination)
}

func routeBody(header, scheme, value string) string {
	return `{"header":"` + header + `","scheme":"` + scheme + `","value":"` + value + `"}`
}

// trusting gives the host an interception root and makes claims-portal's env trust it under via canyon, as a /run
// carrying it records once the previous instance is gone: the policy names the env's routes. It answers the root.
func (h outbound) trusting(t *testing.T, env string) string {
	t.Helper()
	seedRoot(t, h.ctx, h.pool, vaultOf(t, h.base))
	var root string
	if err := h.pool.QueryRow(h.ctx, "SELECT min(fingerprint) FROM egress_ca_roots").Scan(&root); err != nil {
		t.Fatal(err)
	}
	h.exec(t, "UPDATE egress_settings SET via = 'canyon'", "UPDATE deploys SET egress_roots = ARRAY['"+root+"'] WHERE project_id = 'claims-portal' AND env = '"+env+"'")
	return root
}

// seedRoot gives the host an interception root unless it holds one, as Canyon Enterprise's first start serving the
// policy mints one (design/EGRESS.md §4.5): a self-signed P-256 CA of five years, its key sealed under ("egress",
// "root", <fingerprint>), the SHA-256 of its DER. A core test reaches it with or without Canyon Enterprise.
func seedRoot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keys *vault.Vault) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{Organization: []string{"Canyon"}, CommonName: "Canyon egress root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 1, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	fingerprint := hex.EncodeToString(sum[:])
	version, sealedKey, sealedValue, err := keys.Seal("egress", "root", fingerprint, pkcs8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_ca_roots (fingerprint, cert, key_ref, sealed_key, sealed_value, not_after)
		SELECT $1, $2, $3, $4, $5, $6 WHERE NOT EXISTS (SELECT 1 FROM egress_ca_roots)`,
		fingerprint, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), version, sealedKey, sealedValue, tmpl.NotAfter); err != nil {
		t.Fatal(err)
	}
}

// rawPolicy is GET /egress/policy's 200 body as replica canyon-egress-a, checked against the contract.
func rawPolicy(t *testing.T, base string) (string, egressPolicy) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/policy?have=", nil)
	req.Header.Set("X-Replica", "canyon-egress-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the policy: %d %s", resp.StatusCode, raw)
	}
	egressShape(t, "policy", string(raw))
	var p egressPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return string(raw), p
}

// routesOf is the policy's routes of claims-portal's env.
func routesOf(p egressPolicy, env string) []policyRoute {
	for _, e := range p.Envs {
		if e.App == "claims-portal" && e.Env == env {
			return e.Routes
		}
	}
	return nil
}

// routeValueAt is one GET /egress/route-value: its status, its body and its Cache-Control.
func routeValueAt(t *testing.T, base, query string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/route-value?"+query, nil)
	req.Header.Set("X-Replica", "canyon-egress-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header.Get("Cache-Control")
}

func mapsEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// envLock is the key of claims-portal env's advisory lock, as a route's write and the receiver's flush take it.
func envLock(env string) string {
	return "hashtext('" + egressLock("claims-portal", env) + "')::bigint"
}

// ledgerLock is the key of the audit trail's lock (events.LockTx), which every audited write takes first.
const ledgerLock = "111559182283891::bigint"

// whileLocked holds the advisory lock of key in a transaction that runs q, uncommitted, until write waits on that
// lock; then it commits, and answers what write answered once it went on.
func (h outbound) whileLocked(t *testing.T, key, q string, write func() int) int {
	t.Helper()
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	if _, err := tx.Exec(h.ctx, "SELECT pg_advisory_xact_lock("+key+")"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(h.ctx, q); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- write() }()
	for waiting := false; !waiting; {
		select {
		case code := <-done:
			t.Fatalf("the write answered %d without waiting on the lock: %s", code, lastBody)
		case <-time.After(20 * time.Millisecond):
		}
		if err := h.pool.QueryRow(h.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND classid::bigint = (`+key+` >> 32) & 4294967295 AND objid::bigint = `+key+` & 4294967295 AND objsubid = 1)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func mapsEqualAny(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// AGENTS.md: a vault-sealed table is in main.go's two boot checks. A key generated over a route's value or an
// interception root's key is a key volume that did not mount, and a row of either under a key version the store
// lacks refuses the boot naming it.
func TestBootRefusesRouteValuesAndRootsUnderAnotherKey(t *testing.T) {
	pool, ctx := migrated(t)
	fresh, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil || !fresh.Generated {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ('claims-portal', 'claims-portal');
		INSERT INTO egress_routes (project_id, env, destination, header, scheme, from_secret, rev, key_ref, sealed_key, sealed_value) VALUES ('claims-portal', 'prod', 'api.stripe.com', 'authorization', 'bearer', false, nextval('egress_route_rev'), 2, '', '');
		INSERT INTO egress_ca_roots (fingerprint, cert, key_ref, sealed_key, sealed_value, not_after) VALUES (repeat('a', 64), '', 2, '', '', now() + interval '5 years')`); err != nil {
		t.Fatal(err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "2 row(s) are sealed under another key") {
		t.Fatalf("a generated key over a route's value and a root's key: %v", err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "1 egress route value(s) under key version 2") ||
		!strings.Contains(err.Error(), "1 egress interception root key(s) under key version 2") {
		t.Fatalf("a route's value and a root's key under a missing version refuse the boot by name: %v", err)
	}
}

// contracts/routes.md, the route rows and egress.schema.json's route_put and route_from_secret: a body is read field
// by field, the first out of shape named — body for one that is no such object, each key once — with a header taken
// in any case and stored lower-case, and a from-secret's placeholder_env left out the secret's own name.
func TestARouteBodyIsReadFieldByField(t *testing.T) {
	a := &app{kube: true}
	for body, want := range map[string]string{
		`{"header":"X-Api-Key","scheme":"raw","value":"v"}`:                              "",
		`{"header":"Authorization","scheme":"basic","value":"user:pass\tword"}`:          "",
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":null}`:       "",
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"HOME"}`:     "", // Kubernetes refuses no Docker name
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"OPENAI"}`:   "",
		`{"scheme":"raw","value":"v","header":"x-api-key","placeholder_env":"PORT"}`:     "placeholder_env",
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"CNB_X"}`:    "placeholder_env",
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":"no_proxy"}`: "placeholder_env",
		`{"header":"x-api-key","scheme":"raw","value":"v","placeholder_env":7}`:          "placeholder_env",
		`{"header":"X-CANYON-CALLER","scheme":"raw","value":"v"}`:                        "header",
		`{"header":"TE","scheme":"raw","value":"v"}`:                                     "header",
		`{"header":"","scheme":"raw","value":"v"}`:                                       "header",
		`{"header":"x-api-key","scheme":"Bearer","value":"v"}`:                           "scheme",
		`{"header":"x-api-key","scheme":"raw","value":"\u007f"}`:                         "value",
		`{"header":"x-api-key","scheme":"raw","value":null}`:                             "value",
		`{"header":"x-api-key","scheme":"raw","value":"v"} `:                             "",
		`{"header":"x-api-key","scheme":"raw","value":"v"},`:                             "body",
		`null`: "body",
	} {
		in, field := a.readRoute(strings.NewReader(body), false)
		if field != want || want == "" && in.header != strings.ToLower(in.header) {
			t.Errorf("%s: field %q, want %q (header %q)", body, field, want, in.header)
		}
	}
	for body, want := range map[string]string{
		`{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer"}`:                          "",
		`{"secret":"SSL_CERT_DIR","header":"Authorization","scheme":"bearer"}`:                               "placeholder_env",
		`{"secret":"SSL_CERT_DIR","header":"Authorization","scheme":"bearer","placeholder_env":null}`:        "",
		`{"secret":"1KEY","header":"Authorization","scheme":"bearer"}`:                                       "body",
		`{"header":"Authorization","scheme":"bearer"}`:                                                       "body",
		`{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer","value":"v"}`:              "body",
		`{"secret":"STRIPE_SECRET_KEY","header":"Host","scheme":"bearer","placeholder_env":"STRIPE"}`:        "header",
		`{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer","placeholder_env":"PATH"}`: "placeholder_env",
	} {
		in, field := a.readRoute(strings.NewReader(body), true)
		if field != want {
			t.Errorf("from-secret %s: field %q, want %q", body, field, want)
		}
		if body == `{"secret":"STRIPE_SECRET_KEY","header":"Authorization","scheme":"bearer"}` && (in.placeholder == nil || *in.placeholder != "STRIPE_SECRET_KEY") {
			t.Errorf("a from-secret's placeholder left out is the secret's own name: %v", in.placeholder)
		}
	}
	if !(&app{}).placeholderOK("STRIPE_SECRET_KEY") || (&app{}).placeholderOK("HOME") || (&app{}).placeholderOK("docker_host") {
		t.Error("on Docker a placeholder is never a name the Docker executor refuses in a /run")
	}
}

// credentials_exist's message agrees with how many apps it names (design/EXPERIENCE.md, D5): one app has, and its team
// needs; two or more have, and their teams need.
func TestCredentialsExistAgreesWithItsApps(t *testing.T) {
	for _, c := range []struct {
		apps []string
		want string
	}{
		{[]string{"Claims portal"}, "Claims portal has credentials that work only through Canyon. Its team needs to set those keys as secrets again and deploy, then remove the credentials."},
		{[]string{"Claims portal", "Ledger"}, "Claims portal, Ledger have credentials that work only through Canyon. Their teams need to set those keys as secrets again and deploy, then remove the credentials."},
	} {
		if got := credentialsExist(c.apps); got != c.want {
			t.Errorf("%d apps: %q, want %q", len(c.apps), got, c.want)
		}
	}
}
