package runs

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The run-config fingerprint is stable over unchanged parts, whatever order they were read in, and moves with
// each part: the spec, who runs it, the door or when it was saved, a secret's name or version, a resource
// (contracts/routes.md, Patch runs, step 5).
func TestRunConfigFingerprintStable(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	config := func() RunConfig {
		return RunConfig{Egress: Egress{Via: "canyon"}, RunBy: "canyon",
			FrontDoor: &SavedDoor{Door: json.RawMessage(`{"choice":2,"domain":"apps.meridian.example","form":"app-env.domain"}`), SetAt: at},
			Secrets:   map[string]Secret{"DATABASE_URL": {Version: 2, SetAt: at}, "STRIPE_SECRET_KEY": {Version: 1, SetAt: at}, "SENTRY_DSN": {Version: 4, SetAt: at}},
			Resources: []string{"db", "files"}}
	}
	want, err := config().Fingerprint()
	if err != nil || len(want) != 64 {
		t.Fatalf("a fingerprint is a sha256's 64 hex: %q %v", want, err)
	}
	for i := range 50 { // a map is read in another order each time: the fingerprint is not
		reread := config()
		reread.Secrets = map[string]Secret{}
		for _, name := range []string{"SENTRY_DSN", "STRIPE_SECRET_KEY", "DATABASE_URL"}[i%3:] {
			reread.Secrets[name] = config().Secrets[name]
		}
		for _, name := range []string{"SENTRY_DSN", "STRIPE_SECRET_KEY", "DATABASE_URL"}[:i%3] {
			reread.Secrets[name] = config().Secrets[name]
		}
		if got, _ := reread.Fingerprint(); got != want {
			t.Fatalf("unchanged parts, read again: %s, want %s", got, want)
		}
	}
	for name, change := range map[string]func(*RunConfig){
		"via":    func(c *RunConfig) { c.Egress.Via = "off" },
		"run_by": func(c *RunConfig) { c.RunBy = "org" },
		"the door": func(c *RunConfig) {
			c.FrontDoor.Door = json.RawMessage(`{"choice":1,"domain":"apps.meridian.example"}`)
		},
		"a re-saved door":    func(c *RunConfig) { c.FrontDoor.SetAt = at.Add(time.Second) },
		"no door":            func(c *RunConfig) { c.FrontDoor = nil },
		"a secret's version": func(c *RunConfig) { c.Secrets["SENTRY_DSN"] = Secret{Version: 5, SetAt: at} },
		"a secret set again": func(c *RunConfig) { c.Secrets["SENTRY_DSN"] = Secret{Version: 4, SetAt: at.Add(time.Microsecond)} },
		"a secret added":     func(c *RunConfig) { c.Secrets["REDIS_URL"] = Secret{Version: 1, SetAt: at} },
		"a secret removed":   func(c *RunConfig) { delete(c.Secrets, "SENTRY_DSN") },
		"a resource":         func(c *RunConfig) { c.Resources = []string{"db"} },
		"a resource renamed": func(c *RunConfig) { c.Resources = []string{"db", "uploads"} },
	} {
		c := config()
		change(&c)
		if got, _ := c.Fingerprint(); got == want {
			t.Errorf("%s changed and the fingerprint did not", name)
		}
	}
}

// setOf is when env's secret name was set, as the row reads it: UTC, to the microsecond.
func setOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, env, name string) time.Time {
	t.Helper()
	var at time.Time
	if err := p.QueryRow(ctx, "SELECT set_at FROM secrets WHERE project_id = $1 AND env = $2 AND name = $3", project, env, name).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

// runConfigOf is env's deploy row's run-config, its fingerprint and the parts' JSON as stored; nil and "" for none.
func runConfigOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, env string) (config *RunConfig, fingerprint, stored string) {
	t.Helper()
	var sha, text *string
	if err := p.QueryRow(ctx, "SELECT run_config_sha256, run_config::text FROM deploys WHERE project_id = $1 AND env = $2", project, env).Scan(&sha, &text); err != nil {
		t.Fatal(err)
	}
	if sha == nil {
		return nil, "", ""
	}
	config = &RunConfig{}
	if err := json.Unmarshal([]byte(*text), config); err != nil {
		t.Fatal(err)
	}
	return config, *sha, *text
}

// Each deploy row records what its /run carried beside its image — the spec, run_by, the front door as saved,
// the runtime secrets' names and versions, the resources' names — and the fingerprint over it, never a value:
// no secret's, build or runtime, and never the front door's client secret. Over rows unchanged the fingerprint
// is the same; a secret set again moves it; a promotion's row records production's own (contracts/routes.md,
// Patch runs, step 5).
func TestADeployRowRecordsWhatItsRunCarriedNeverAValue(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	const secret = "oauth-client-5e8a1c" //nolint:gosec // a synthetic value, not a credential
	keyRef, sealedKey, sealedValue, err := pipe.Vault.Seal("front-door", "signin", "client_secret", []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	var door json.RawMessage
	var setAt time.Time
	if err := p.QueryRow(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
		VALUES (now(), now(), $1, $2, $3, $4, 'person', 'dana@meridian.example', 'Dana Whitfield') RETURNING door, set_at`,
		[]byte(`{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"},"domain":"apps.meridian.example","form":"app-env.domain"}`),
		keyRef, sealedKey, sealedValue).Scan(&door, &setAt); err != nil {
		t.Fatal(err)
	}
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-3f7b")
	sealed(t, ctx, p, "preview", "build", "NEXT_PUBLIC_API_URL", "value-build-8c2d")
	sealed(t, ctx, p, "prod", "runtime", "SENTRY_DSN", "value-prod-6a9e")
	if _, err := p.Exec(ctx, "INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ($1, 'preview', 'files', 'bucket', 'builtin', 'ready')", project); err != nil {
		t.Fatal(err)
	}
	deployed(t, ctx, p, pkg)
	config, fingerprint, stored := runConfigOf(t, ctx, p, "preview")
	want := RunConfig{Egress: Egress{Via: "off"}, RunBy: "canyon", FrontDoor: &SavedDoor{Door: door, SetAt: setAt.UTC()},
		Secrets: map[string]Secret{"STRIPE_SECRET_KEY": {Version: 1, SetAt: setOf(t, ctx, p, "preview", "STRIPE_SECRET_KEY")}}, Resources: []string{"files"}}
	if config == nil || !reflect.DeepEqual(*config, want) {
		t.Fatalf("preview's row records %s, want %+v", stored, want)
	}
	if again, _ := config.Fingerprint(); again != fingerprint {
		t.Fatalf("the row's fingerprint %s is not its parts' %s", fingerprint, again)
	}
	for _, value := range []string{"value-runtime-3f7b", "value-build-8c2d", "value-prod-6a9e", secret} {
		if strings.Contains(stored, value) {
			t.Fatalf("the row's run-config holds a value: %s", stored)
		}
	}
	deployed(t, ctx, p, pkg)
	if _, unchanged, _ := runConfigOf(t, ctx, p, "preview"); unchanged != fingerprint {
		t.Fatalf("a redeploy with nothing changed: %s, want %s", unchanged, fingerprint)
	}
	if _, err := p.Exec(ctx, "UPDATE secrets SET version = version + 1, set_at = now() WHERE project_id = $1 AND env = 'preview' AND name = 'STRIPE_SECRET_KEY'", project); err != nil { // set again, as the PUT does
		t.Fatal(err)
	}
	run := deployed(t, ctx, p, pkg)
	if moved, changed, stored := runConfigOf(t, ctx, p, "preview"); changed == fingerprint || moved.Secrets["STRIPE_SECRET_KEY"] != (Secret{Version: 2, SetAt: setOf(t, ctx, p, "preview", "STRIPE_SECRET_KEY")}) {
		t.Fatalf("a secret set again: %s %s, want another fingerprint naming version 2", changed, stored)
	}
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, run, image); state != "clean" {
		t.Fatalf("the promotion: %s %s", state, why(reason))
	}
	want.Secrets, want.Resources = map[string]Secret{"SENTRY_DSN": {Version: 1, SetAt: setOf(t, ctx, p, "prod", "SENTRY_DSN")}}, nil
	if prod, _, stored := runConfigOf(t, ctx, p, "prod"); prod == nil || !reflect.DeepEqual(*prod, want) {
		t.Fatalf("prod's row records %s, want %+v: production's own secrets and resources", stored, want)
	}
}

// A retry that finds its version already running records no run-config: what the crashed attempt's /run
// carried is not known, so a patch over the row fails closed — never the fingerprint of the row it replaces.
func TestARetryThatStartsNothingRecordsNoRunConfig(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, state, reason := deployOnce(t, ctx, p)
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	if _, state, reason := promote(t, ctx, p, deployRun, image); state != "clean" {
		t.Fatalf("the first promotion: %s %s", state, why(reason))
	}
	if config, _, _ := runConfigOf(t, ctx, p, "prod"); config == nil {
		t.Fatal("a promotion's /run records its run-config")
	}
	held := make(chan struct{})
	pipe.hold.Store(&held)
	promotion := &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: promotion})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Run: run.ID, Project: project, Package: pkg, Actor: dana, Promote: promotion}
	if err := pipe.record(ctx, in, provisioners.Key{}, "", "", "", "", "deploying:prod", "deploying to production"); err != nil { // the first attempt got this far
		t.Fatal(err)
	}
	started := f.Runs.Load()
	_, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	waitEnded(t, ctx, p, run.ID)
	if config, _, stored := runConfigOf(t, ctx, p, "prod"); err != nil || f.Runs.Load() != started || config != nil {
		t.Fatalf("the retry: %v, %d /runs, prod's row recording %s; want nothing started and no run-config", err, f.Runs.Load()-started, stored)
	}
}

// A secret removed and set again starts at version 1 again: its set time moves the fingerprint all the same,
// so a patch never carries the new value into an app that runs the old one (review of #2373).
func TestASecretRemovedAndSetAgainMovesTheFingerprint(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-1a4c")
	deployed(t, ctx, p, pkg)
	_, before, _ := runConfigOf(t, ctx, p, "preview")
	if _, err := p.Exec(ctx, "DELETE FROM secrets WHERE project_id = $1 AND name = 'STRIPE_SECRET_KEY'", project); err != nil { // canyon secret rm
		t.Fatal(err)
	}
	sealed(t, ctx, p, "preview", "runtime", "STRIPE_SECRET_KEY", "value-runtime-7e2b") // set again: version 1
	deployed(t, ctx, p, pkg)
	if _, after, stored := runConfigOf(t, ctx, p, "preview"); after == before {
		t.Fatalf("a secret removed and set again: the fingerprint stayed %s (%s)", after, stored)
	}
}

// A resource added moves the fingerprint, and removed again gives back the one before it.
func TestAResourceAddedOrRemovedMovesTheFingerprint(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	_, none, _ := runConfigOf(t, ctx, p, "preview")
	if _, err := p.Exec(ctx, "INSERT INTO resources (project_id, env, name, type, driver, state) VALUES ($1, 'preview', 'files', 'bucket', 'builtin', 'ready')", project); err != nil {
		t.Fatal(err)
	}
	deployed(t, ctx, p, pkg)
	if config, added, stored := runConfigOf(t, ctx, p, "preview"); added == none || !reflect.DeepEqual(config.Resources, []string{"files"}) {
		t.Fatalf("a resource added: %s %s", added, stored)
	}
	if _, err := p.Exec(ctx, "DELETE FROM resources WHERE project_id = $1", project); err != nil {
		t.Fatal(err)
	}
	deployed(t, ctx, p, pkg)
	if _, removed, stored := runConfigOf(t, ctx, p, "preview"); removed != none {
		t.Fatalf("the resource removed again: %s %s, want %s", removed, stored, none)
	}
}

// A server from before the fingerprint writes a row again without it — a rollback, or a rolling upgrade —
// so the database clears what it would leave stale: a deploy (a new run, image or time) or a recreation (a new
// spec) through it records none, and a patch over the row fails closed. The fingerprint this server writes
// stays, a redeploy with nothing changed included (review of #2373).
func TestAnOlderServersWriteClearsTheRunConfig(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	deployed(t, ctx, p, pkg)
	for _, older := range []string{
		"UPDATE deploys SET deployed_at = now() WHERE project_id = $1 AND env = 'preview'",          // a deploy that reuses the image
		`UPDATE deploys SET egress = '{"via": "canyon"}' WHERE project_id = $1 AND env = 'preview'`, // a recreation
	} {
		deployed(t, ctx, p, pkg)
		if config, _, _ := runConfigOf(t, ctx, p, "preview"); config == nil {
			t.Fatal("this server's deploy records its run-config")
		}
		if _, err := p.Exec(ctx, older, project); err != nil {
			t.Fatal(err)
		}
		if config, _, stored := runConfigOf(t, ctx, p, "preview"); config != nil {
			t.Fatalf("%s: the row keeps %s; want none", older, stored)
		}
	}
}
