package runs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// caFixture is internal/subjecthash's CA name: its PEM and its fingerprint, the SHA-256 of its DER.
func caFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "subjecthash", "testdata", name+".pem")) //nolint:gosec // the fixtures' own names
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(block.Bytes)
	return string(raw), hex.EncodeToString(sum[:])
}

// contracts/routes.md, Licence, A server built without Canyon Enterprise (#2922): with no Inject — a build without
// Canyon Enterprise — a /run carries no root and records none, credential routes on, live roots held, and no
// route's placeholder reaches it.
func TestWithoutEnterpriseARunCarriesNoRootAndNoPlaceholder(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	prior := pipe.Inject
	pipe.Inject = nil
	t.Cleanup(func() { pipe.Inject = prior })
	if _, err := p.Exec(ctx, `INSERT INTO egress_routes (project_id, env, destination, header, scheme, placeholder_env, from_secret, rev, key_ref, sealed_key, sealed_value)
		VALUES ($1, 'preview', 'api.stripe.com', 'authorization', 'bearer', 'STRIPE_SECRET_KEY', false, nextval('egress_route_rev'), 1, '', '')`, project); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	if _, held := f.Ran().Env["STRIPE_SECRET_KEY"]; held {
		t.Fatal("a route's placeholder reached the /run without Canyon Enterprise")
	}
	var image, run string
	if err := p.QueryRow(ctx, "SELECT image, gate_run FROM deploys WHERE env = 'preview'").Scan(&image, &run); err != nil {
		t.Fatal(err)
	}
	cert, fingerprint := caFixture(t, "printable")
	if _, err := p.Exec(ctx, `INSERT INTO egress_ca_roots (fingerprint, cert, key_ref, sealed_key, sealed_value, not_after) VALUES ($1, $2, 1, '\x00', '\x00', now() + interval '5 years')`, fingerprint, cert); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET credential_routes = true"); err != nil { // as an Enterprise server's admin left it
		t.Fatal(err)
	}
	kubeDoor(t, ctx, p)
	_, _, carried, err := pipe.RunApp(ctx, run, project, "preview", image, Egress{Via: "canyon"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ran := f.Ran(); ran.Egress.CA != nil || carried.Roots == nil || len(carried.Roots) != 0 {
		t.Fatalf("credential routes on, a live root, no Canyon Enterprise: egress.ca %v, roots %v; want none carried and none recorded", ran.Egress.CA, carried.Roots)
	}
}
