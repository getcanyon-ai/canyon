//go:build !noee

package runs

import (
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/ee/outboundkeys"
	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// withInject gives the pipeline Canyon Enterprise's outbound keys until the test ends, as the server's wiring does.
func withInject(t *testing.T) {
	t.Helper()
	prior := pipe.Inject
	pipe.Inject = outboundkeys.New(nil, nil)
	t.Cleanup(func() { pipe.Inject = prior })
}

// #2143; contracts/executor.md, egress.ca: while the host's credential routes are off, as it starts, no /run carries a
// root; once an admin turns them on every /run with egress carries the host's live roots, the newest first, each
// under OpenSSL's subject-hash name of it (hashes from internal/subjecthash's fixtures, OpenSSL's own), a BYO sub-CA's
// chain beside it, and the deploy records the roots it carried, by fingerprint; a root past its end is left out. An
// env whose own values name SSL_CERT_DIR, NODE_EXTRA_CA_CERTS or REQUESTS_CA_BUNDLE carries none and records none.
func TestARunCarriesTheHostsRoots(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	withInject(t)
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	var image, run string
	if err := p.QueryRow(ctx, "SELECT image, gate_run FROM deploys WHERE env = 'preview'").Scan(&image, &run); err != nil {
		t.Fatal(err)
	}
	canyonCA, canyonFP := caFixture(t, "printable") // Canyon's own, older
	byo, byoFP := caFixture(t, "utf8-spaces-case")  // an admin's sub-CA, newer
	above, _ := caFixture(t, "email")               // its chain
	gone, goneFP := caFixture(t, "tabs")            // retired
	if _, err := p.Exec(ctx, `INSERT INTO egress_ca_roots (fingerprint, cert, chain, byo, key_ref, sealed_key, sealed_value, created_at, not_after, retires_at) VALUES
		($1, $2, NULL, false, 1, '\x00', '\x00', now() - interval '2 days', now() + interval '5 years', NULL),
		($3, $4, $5, true, 1, '\x00', '\x00', now() - interval '1 day', now() + interval '5 years', NULL),
		($6, $7, NULL, false, 1, '\x00', '\x00', now() - interval '3 days', now() + interval '5 years', now() - interval '1 second')`,
		canyonFP, canyonCA, byoFP, byo, above, goneFP, gone); err != nil {
		t.Fatal(err)
	}
	kubeDoor(t, ctx, p)
	_, _, carried, err := pipe.RunApp(ctx, run, project, "preview", image, Egress{Via: "canyon"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ran := f.Ran(); ran.Egress.CA != nil || carried.Roots == nil || len(carried.Roots) != 0 {
		t.Fatalf("credential routes off, as a host starts: egress.ca %v, roots %v; want none carried and none recorded", ran.Egress.CA, carried.Roots)
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET credential_routes = true"); err != nil { // an admin turns them on
		t.Fatal(err)
	}
	_, _, carried, err = pipe.RunApp(ctx, run, project, "preview", image, Egress{Via: "canyon"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ran := f.Ran()
	var names []string
	for _, r := range ran.Egress.CA {
		names = append(names, r.Name)
	}
	if want := []string{"b20cba2d.0", "c1e385ae.0", "aea6fb85.0"}; !slices.Equal(names, want) {
		t.Errorf("egress.ca names %v; want %v, the newest root first, its chain beside it, each under OpenSSL's hash of it", names, want)
	}
	if len(ran.Egress.CA) == 3 && (ran.Egress.CA[0].PEM != byo || ran.Egress.CA[2].PEM != canyonCA) {
		t.Errorf("egress.ca's pems are not the roots as stored")
	}
	if !slices.Equal(carried.Roots, []string{byoFP, canyonFP}) {
		t.Errorf("the /run carried %v; want the two live roots' fingerprints, newest first", carried.Roots)
	}

	sealed(t, ctx, p, "preview", "runtime", "REQUESTS_CA_BUNDLE", "/workspace/corp.pem")
	if _, _, carried, err = pipe.RunApp(ctx, run, project, "preview", image, Egress{Via: "canyon"}, ""); err != nil {
		t.Fatal(err)
	}
	if ran := f.Ran(); ran.Egress.CA != nil || carried.Roots == nil || len(carried.Roots) != 0 {
		t.Errorf("an env naming REQUESTS_CA_BUNDLE: egress.ca %v, roots %v; want none carried and none recorded", ran.Egress.CA, carried.Roots)
	}
}

// design/EGRESS.md §4.1: a route's placeholder variable carries canyon-adds-the-real-key in the app's next /run, so an
// SDK that needs a key starts — unless the env holds a runtime value of that name, which stands.
func TestARoutesPlaceholderReachesTheNextRun(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	withInject(t)
	sealed(t, ctx, p, "preview", "runtime", "OPENAI_API_KEY", "value-openai-1f2e")
	if _, err := p.Exec(ctx, `INSERT INTO egress_routes (project_id, env, destination, header, scheme, placeholder_env, from_secret, rev, key_ref, sealed_key, sealed_value)
		VALUES ($1, 'preview', 'api.stripe.com', 'authorization', 'bearer', 'STRIPE_SECRET_KEY', false, nextval('egress_route_rev'), 1, '', ''),
			($1, 'preview', 'api.openai.com', 'authorization', 'bearer', 'OPENAI_API_KEY', false, nextval('egress_route_rev'), 1, '', ''),
			($1, 'prod', 'hooks.slack.com', 'x-slack-token', 'raw', 'SLACK_TOKEN', false, nextval('egress_route_rev'), 1, '', '')`, project); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	env := f.Ran().Env
	if env["STRIPE_SECRET_KEY"] != "canyon-adds-the-real-key" || env["OPENAI_API_KEY"] != "value-openai-1f2e" {
		t.Fatalf("the /run's placeholders: STRIPE_SECRET_KEY %q, OPENAI_API_KEY %q", env["STRIPE_SECRET_KEY"], env["OPENAI_API_KEY"])
	}
	if _, held := env["SLACK_TOKEN"]; held {
		t.Fatal("prod's route gave preview's /run a placeholder")
	}
}
