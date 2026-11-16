package runs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// trustedRootsOf is env's trusted roots as its deploy row records them, nothing recorded being none, and whether
// anything is recorded.
func trustedRootsOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, env string) ([]string, bool) {
	t.Helper()
	var roots []string
	if err := p.QueryRow(ctx, "SELECT egress_roots FROM deploys WHERE project_id = $1 AND env = $2", project, env).Scan(&roots); err != nil {
		t.Fatal(err)
	}
	return roots, roots != nil
}

// contracts/routes.md, Interception, Trust: before a /run an env's trusted roots are the old set intersected with the
// carried one, in the transaction that names the env, so it moves seq; the carried set alone once the executor
// answers that the new version is up and the previous instance gone; a write after a /run whose previous instance
// may still serve, or after none known, leaves the intersection; and a row the org writes records none. A Docker
// deploy, whose 200 answers once the previous container is removed, records what its /run carried: no root, as no
// /run carries egress.ca before #2143.
func TestTrustedRootsNarrowBeforeARunAndSettleOnceThePreviousIsGone(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	if roots, recorded := trustedRootsOf(t, ctx, p, "preview"); !recorded || len(roots) != 0 {
		t.Fatalf("a Docker deploy carrying no root records none: %v (recorded %t)", roots, recorded)
	}
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if _, err := p.Exec(ctx, "UPDATE deploys SET egress_roots = $1 WHERE env = 'preview'", []string{a, b}); err != nil {
		t.Fatal(err)
	}
	seq := func() int64 {
		t.Helper()
		var n int64
		if err := p.QueryRow(ctx, "SELECT seq FROM egress_policy").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := seq()
	if named, err := pipe.converge(ctx, project, "preview", []string{b, c}, false); err != nil || named {
		t.Fatalf("the narrowing: named %t, %v", named, err)
	}
	if roots, _ := trustedRootsOf(t, ctx, p, "preview"); !slices.Equal(roots, []string{b}) || seq() <= before {
		t.Fatalf("before a /run carrying %v over %v the env trusts %v, and seq moved %t; want the intersection, seq moved", []string{b, c}, []string{a, b}, roots, seq() > before)
	}
	var d Deployed
	var config []byte
	var fingerprint string
	if err := p.QueryRow(ctx, "SELECT package_id, image, url, gate_run, run_config, run_config_sha256 FROM deploys WHERE env = 'preview'").Scan(&d.Package, &d.Image, &d.URL, &d.GateRun, &config, &fingerprint); err != nil {
		t.Fatal(err)
	}
	ran := Carried{Fingerprint: fingerprint, Roots: []string{b, c}} // what the deploy's /run carried, but its roots
	if err := json.Unmarshal(config, &ran.Config); err != nil {
		t.Fatal(err)
	}
	d.Project, d.Env, d.Run, d.DeployedBy = project, "preview", d.GateRun, By(dana)
	write := func(carried *Carried, runBy string) []string {
		t.Helper()
		if _, err := p.Exec(ctx, "UPDATE deploys SET egress_roots = $1 WHERE env = 'preview'", []string{b}); err != nil { // the intersection the /run wrote
			t.Fatal(err)
		}
		d.Carried, d.RunBy = carried, runBy
		if err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error { return upsertDeployTx(ctx, tx, d) }); err != nil {
			t.Fatal(err)
		}
		roots, _ := trustedRootsOf(t, ctx, p, "preview")
		return roots
	}
	stayed, gone := ran, ran
	gone.Gone = true
	if roots := write(nil, "canyon"); !slices.Equal(roots, []string{b}) {
		t.Errorf("a retry that knows no /run: the env trusts %v, want the intersection", roots)
	}
	if roots := write(&stayed, "canyon"); !slices.Equal(roots, []string{b}) {
		t.Errorf("a /run whose previous instance may still serve (previous_gone false or absent): the env trusts %v, want the intersection", roots)
	}
	if roots := write(&gone, "canyon"); !slices.Equal(roots, []string{b, c}) {
		t.Errorf("once the previous instance is gone the env trusts %v, want the carried set", roots)
	}
	if roots := write(nil, "org"); roots != nil {
		t.Errorf("a row the org writes records %v, want none", roots)
	}
}

// contracts/executor.md, On Kubernetes: a /run's previous instance is gone only on a 200 whose previous_gone is true;
// absent or false it may still serve, and on Docker the 200 itself says so.
func TestOnKubernetesThePreviousIsGoneOnlyWhenTheExecutorSaysSo(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, reasonOf(reason))
	}
	var image, run string
	if err := p.QueryRow(ctx, "SELECT image, gate_run FROM deploys WHERE env = 'preview'").Scan(&image, &run); err != nil {
		t.Fatal(err)
	}
	gone := func(what string, want bool) {
		t.Helper()
		_, _, carried, err := pipe.RunApp(ctx, run, project, "preview", image, Egress{Via: "off"}, "")
		if err != nil || carried.Gone != want || carried.Roots == nil || len(carried.Roots) != 0 {
			t.Fatalf("%s: gone %t, roots %v (%v); want gone %t, no root carried", what, carried.Gone, carried.Roots, err, want)
		}
	}
	gone("Docker's 200", true)
	kubeDoor(t, ctx, p)
	gone("a Kubernetes 200 without previous_gone", false)
	no, yes := false, true
	f.PreviousGone.Store(&no)
	gone("previous_gone false", false)
	f.PreviousGone.Store(&yes)
	gone("previous_gone true", true)
}
