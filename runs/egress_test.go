package runs

import (
	"context"
	"reflect"
	"testing"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// design/EGRESS.md §2.5: "The egress spec a run used — via and, from layer 2, the mode — is recorded on
// its deploy row." A person's deploy of a new package runs under the host's via now: through Canyon's
// Envoy, its database reached directly, the /run naming its run (contracts/executor.md, Egress); the row
// keeps what it ran under when the host's setting moves. The next deploy reads the setting then, and under
// via off still runs through Envoy: its network is internal now, and the network wins (§2.5, the owner's
// decision, #2254) until an admin recreates it open.
func TestADeployRunsUnderTheHostsViaAndRecordsIt(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	sealed(t, ctx, p, "preview", "runtime", "DATABASE_URL", "postgres://claims_portal:value-db-5e1a@App-Postgres:5432/claims_portal")
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	via := func(v string) {
		t.Helper()
		if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = $1", v); err != nil {
			t.Fatal(err)
		}
	}
	recorded := func() string {
		t.Helper()
		var spec string
		if err := p.QueryRow(ctx, "SELECT egress::text FROM deploys WHERE env = 'preview'").Scan(&spec); err != nil {
			t.Fatal(err)
		}
		return spec
	}
	via("canyon")
	run, state, reason := deployOnce(t, ctx, p)
	if state != "clean" {
		t.Fatalf("a deploy under via canyon: %s %s", state, reasonOf(reason))
	}
	ran := f.Ran()
	if want := (&executor.Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}}); !reflect.DeepEqual(ran.Egress, want) || ran.Run != run {
		t.Fatalf("the /run carries egress %+v and run %q; want %+v and %q", ran.Egress, ran.Run, want, run)
	}
	if got := recorded(); got != `{"via": "canyon"}` {
		t.Fatalf("the deploy row records %s, want via canyon", got)
	}
	via("off")
	if got := recorded(); got != `{"via": "canyon"}` {
		t.Fatalf("the host's setting moved the recorded spec to %s", got)
	}
	if _, state, reason := deployOnce(t, ctx, p); state != "clean" {
		t.Fatalf("a deploy under via off: %s %s", state, reasonOf(reason))
	}
	if ran := f.Ran(); ran.Egress == nil || ran.Run == "" {
		t.Fatalf("a /run under via off onto the internal network carries egress %+v and run %q; want both: the network wins", ran.Egress, ran.Run)
	}
	if got := recorded(); got != `{"via": "canyon"}` {
		t.Fatalf("the next deploy records %s, want via canyon, what ran", got)
	}
}

// Fail closed: a host whose via is canyon and which names no proxy starts no container, rather than one
// that calls out past Canyon's Envoy.
func TestADeployUnderViaCanyonWithNoProxyStartsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := deployOnce(t, ctx, p); state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a deploy under via canyon with no proxy: %s %s, want failed error", state, reasonOf(reason))
	}
	if f.Runs.Load() != 0 {
		t.Fatalf("%d containers started with no proxy to call out through", f.Runs.Load())
	}
}

// design/EGRESS.md §2.5: a promotion carries the spec its package's preview deploy recorded — preview is
// the canary — never the host's now, either way the setting moved in between. Between the two, an admin
// recreates the app's network internal: a /run never changes one.
func TestAPromotionCarriesPreviewsRecordedSpec(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	for _, tc := range []struct{ deployed, promoted string }{{"off", "canyon"}, {"canyon", "off"}} {
		if tc.deployed == "canyon" {
			if err := pipe.Executor.RecreateNetwork(ctx, project, true); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = $1", tc.deployed); err != nil {
			t.Fatal(err)
		}
		deployRun, state, reason := deployOnce(t, ctx, p)
		var image string
		if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
			t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
		}
		if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = $1", tc.promoted); err != nil {
			t.Fatal(err)
		}
		if _, state, reason := promote(t, ctx, p, deployRun, image); state != "clean" {
			t.Fatalf("the promotion: %s %s", state, reasonOf(reason))
		}
		var recorded string
		if err := p.QueryRow(ctx, "SELECT egress->>'via' FROM deploys WHERE env = 'prod'").Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		if ran := f.Ran(); ran.Target != "prod" || (ran.Egress != nil) != (tc.deployed == "canyon") || recorded != tc.deployed {
			t.Fatalf("deployed under %s, promoted while the host says %s: prod's /run carries %+v and its row %s; want preview's", tc.deployed, tc.promoted, ran.Egress, recorded)
		}
	}
}

// Fail closed (design/EGRESS.md §2.5): a version whose run recorded no spec — production's history from
// before egress, its backfilled first row — carries the host's spec now, never via off by omission: a
// rollback to it on a host routed through Canyon runs through Canyon's Envoy, and so does a promotion of
// a preview deploy that recorded none.
func TestARollbackToAVersionWithNoRecordedSpecCarriesTheHosts(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	a, _ := inProduction(t, ctx, p)
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the review: %s", state)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: a, GateRun: review.ID, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || p.QueryRow(ctx, "SELECT egress->>'via' FROM deploys WHERE env = 'prod'").Scan(&recorded) != nil {
		t.Fatalf("the rollback: %s %s", state, reasonOf(reason))
	}
	if ran := f.Ran(); ran.Target != "prod" || ran.Egress == nil || ran.Egress.Proxy != "http://canyon-egress:3128" || recorded != "canyon" {
		t.Fatalf("a rollback to a version with no recorded spec on a host via canyon: /run egress %+v, the row records %q; want the host's", ran.Egress, recorded)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET egress = NULL WHERE id = 'gr_b'"); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, "gr_b", image("b")); state != "clean" || f.Ran().Egress == nil {
		t.Fatalf("a promotion of a preview deploy that recorded no spec: %s %s, /run egress %+v", state, reasonOf(reason), f.Ran().Egress)
	}
}

// design/EGRESS.md §2.5 (the owner's decision, #2254): the network wins over a spec. Onto an app whose network is
// internal, a promotion of a package whose preview deploy recorded off, and a rollback to a version production
// ran under off, each carry egress and run — never a /run the executor refuses as cut off — and the row records
// canyon, what ran.
func TestOntoAnInternalNetworkARecordedOffCarriesEgress(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	a, _ := inProduction(t, ctx, p)
	for _, q := range []string{"UPDATE deploy_history SET egress = '{\"via\": \"off\"}'", "UPDATE egress_settings SET via = 'canyon'"} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	var previewImage string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&previewImage); err != nil || state != "clean" {
		t.Fatalf("the preview deploy under via canyon: %s %s (%v)", state, reasonOf(reason), err)
	}
	if internal, made := f.Network(project); !internal || !made {
		t.Fatalf("the app's network is internal %t, made %t; want internal", internal, made)
	}
	for _, q := range []string{"UPDATE runs SET egress = '{\"via\": \"off\"}' WHERE id = '" + deployRun + "'", "UPDATE egress_settings SET via = 'off'"} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	recorded := func() string {
		t.Helper()
		var via string
		if err := p.QueryRow(ctx, "SELECT coalesce(egress->>'via', '-') FROM deploys WHERE env = 'prod'").Scan(&via); err != nil {
			t.Fatal(err)
		}
		return via
	}
	if _, state, reason := promote(t, ctx, p, deployRun, previewImage); state != "clean" || f.Ran().Target != "prod" || f.Ran().Egress == nil || recorded() != "canyon" {
		t.Fatalf("a promotion of a package recorded off onto the internal network: %s %s, /run egress %+v, the row %s; want it run through Envoy", state, reasonOf(reason), f.Ran().Egress, recorded())
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the rollback review: %s", state)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: a, GateRun: review.ID, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || f.Ran().Image != a || f.Ran().Egress == nil || recorded() != "canyon" {
		t.Fatalf("a rollback to a version production ran under off, onto the internal network: %s %s, /run %s egress %+v, the row %s; want it run through Envoy", state, reasonOf(reason), f.Ran().Image, f.Ran().Egress, recorded())
	}
}

// Only a /run a deploy makes records the app's network (codex on #2266): a promotion retried after a crash past
// deploying:prod, finding production already running its image, starts nothing, and leaves the network recorded
// open as an admin's recreation under via off left it — though the preview deploy it carries recorded canyon.
func TestARetryThatStartsNothingRecordsNoNetwork(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	if err := pipe.Executor.RecreateNetwork(ctx, project, false); err != nil { // an admin's recreation under via off, as the route records it
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE egress_settings SET via = 'off'", "UPDATE deploys SET egress = '{\"via\": \"off\"}'",
		"INSERT INTO app_networks (project_id, internal) VALUES ('" + project + "', false) ON CONFLICT (project_id) DO UPDATE SET internal = false"} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	f.Start(project, "prod") // the first attempt's /run: production runs the image
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
	var internal *bool
	if qerr := p.QueryRow(ctx, "SELECT internal FROM app_networks WHERE project_id = $1", project).Scan(&internal); qerr != nil || err != nil || f.Runs.Load() != started || internal == nil || *internal {
		t.Fatalf("the retry: %v, %d /runs, the network recorded internal %v (%v); want nothing started and the network left recorded open", err, f.Runs.Load()-started, internal, qerr)
	}
}

// A retry keeps the spec its first attempt ran, and an app with no network record reads open (codex on #2266):
// an app whose preview predates recorded egress, with no record of its network, is promoted under via off; the
// first attempt starts prod and crashes before recording; an admin turns via canyon; the retry, finding prod
// running the image, records off on prod's row — never canyon, which it did not run — and the network reads
// not internal.
func TestARetryKeepsTheSpecItsFirstAttemptRan(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	pipe.EgressProxy = "http://canyon-egress:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
	deployRun, state, reason := deployOnce(t, ctx, p)
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	for _, q := range []string{"UPDATE deploys SET egress = NULL", "UPDATE runs SET egress = NULL WHERE id = '" + deployRun + "'", "DELETE FROM app_networks"} { // from before egress was recorded
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	f.Start(project, "prod") // the first attempt's /run, under off
	held := make(chan struct{})
	pipe.hold.Store(&held)
	promotion := &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: promotion})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Run: run.ID, Project: project, Package: pkg, Actor: dana, Promote: promotion}
	if err := pipe.record(ctx, in, provisioners.Key{}, "", "", "", "", "deploying:prod", "deploying to production"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE runs SET egress = '{\"via\": \"off\"}' WHERE id = '" + run.ID + "'", "UPDATE egress_settings SET via = 'canyon'"} { // the spec it started under; then an admin's switch
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	started := f.Runs.Load()
	_, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	waitEnded(t, ctx, p, run.ID)
	var via string
	var internal bool
	if qerr := p.QueryRow(ctx, "SELECT coalesce((SELECT egress->>'via' FROM deploys WHERE env = 'prod'), '-'), "+NetworkInternal("$1"), project).Scan(&via, &internal); qerr != nil || err != nil || f.Runs.Load() != started || via != "off" || internal {
		t.Fatalf("the retry: %v, %d /runs, prod's row %s, the network internal %t (%v); want nothing started, off — what it ran — and not internal", err, f.Runs.Load()-started, via, internal, qerr)
	}
}

// kubeDoor makes pipe a Kubernetes host's with a front door and Canyon's Envoy, deploy/k8s/server.yaml's.
func kubeDoor(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app.env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	pipe.EgressProxy = "http://canyon-egress.canyon.svc:3128"
	t.Cleanup(func() { pipe.EgressProxy = "" })
}

// design/EGRESS.md §2.5, A′ (#2286): on Kubernetes the host's canyon wins over a recorded off. Under via canyon a
// rollback to a version production ran under off, and a promotion of a package preview ran under off, each carry
// egress and run, and the row records canyon, what ran: nothing carried moves an app off the floor while the host
// routes through Canyon.
func TestOnKubernetesTheHostsCanyonWinsOverARecordedOff(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	kubeDoor(t, ctx, p)
	a, _ := inProduction(t, ctx, p)
	for _, q := range []string{"UPDATE deploy_history SET egress = '{\"via\": \"off\"}'", "UPDATE egress_settings SET via = 'canyon'"} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	recorded := func() string {
		t.Helper()
		var via string
		if err := p.QueryRow(ctx, "SELECT coalesce(egress->>'via', '-') FROM deploys WHERE env = 'prod'").Scan(&via); err != nil {
			t.Fatal(err)
		}
		return via
	}
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the rollback review: %s", state)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: a, GateRun: review.ID, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || f.Ran().Image != a || f.Ran().Egress == nil || recorded() != "canyon" {
		t.Fatalf("a rollback carrying a recorded off on a Kubernetes host under canyon: %s %s, /run %s egress %+v, the row %s; want it run through Envoy", state, reasonOf(reason), f.Ran().Image, f.Ran().Egress, recorded())
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'off'"); err != nil {
		t.Fatal(err)
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	var previewImage string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&previewImage); err != nil || state != "clean" || f.Ran().Egress != nil {
		t.Fatalf("the preview deploy under via off: %s %s (%v), /run egress %+v", state, reasonOf(reason), err, f.Ran().Egress)
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	if _, state, reason := promote(t, ctx, p, deployRun, previewImage); state != "clean" || f.Ran().Target != "prod" || f.Ran().Egress == nil || recorded() != "canyon" {
		t.Fatalf("a promotion of a package recorded off on a Kubernetes host under canyon: %s %s, /run egress %+v, the row %s; want it run through Envoy", state, reasonOf(reason), f.Ran().Egress, recorded())
	}
}

// design/EGRESS.md §2.5 (#2286): a Kubernetes /run records no app network, which would pin the app to canyon with
// no recreation to undo it: after a deploy under via canyon, the next under via off runs off, as the host says.
func TestOnKubernetesARunRecordsNoNetwork(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	kubeDoor(t, ctx, p)
	for _, via := range []string{"canyon", "off"} {
		if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = $1", via); err != nil {
			t.Fatal(err)
		}
		var recorded string
		if _, state, reason := deployOnce(t, ctx, p); state != "clean" || p.QueryRow(ctx, "SELECT egress->>'via' FROM deploys WHERE env = 'preview'").Scan(&recorded) != nil ||
			(f.Ran().Egress != nil) != (via == "canyon") || recorded != via {
			t.Fatalf("a deploy under via %s on Kubernetes: %s %s, /run egress %+v, the row %s; want %s", via, state, reasonOf(reason), f.Ran().Egress, recorded, via)
		}
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM app_networks").Scan(&n); err != nil || n != 0 {
			t.Fatalf("under via %s a Kubernetes /run recorded %d app network(s) (%v), want none", via, n, err)
		}
	}
}

// design/EGRESS.md §2.5 (#2286): under the host's off, a carried spec runs as recorded on Kubernetes too — a
// promotion of a package preview ran under canyon carries egress, and prod's row records canyon.
func TestOnKubernetesARecordedCanyonRunsUnderTheHostsOff(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	kubeDoor(t, ctx, p)
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
	}
	deployRun, state, reason := deployOnce(t, ctx, p)
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" || f.Ran().Egress == nil {
		t.Fatalf("the preview deploy under via canyon: %s %s (%v), /run egress %+v", state, reasonOf(reason), err, f.Ran().Egress)
	}
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'off'"); err != nil {
		t.Fatal(err)
	}
	var recorded string
	if _, state, reason := promote(t, ctx, p, deployRun, image); state != "clean" || f.Ran().Target != "prod" || f.Ran().Egress == nil ||
		p.QueryRow(ctx, "SELECT egress->>'via' FROM deploys WHERE env = 'prod'").Scan(&recorded) != nil || recorded != "canyon" {
		t.Fatalf("a promotion of a package recorded canyon on a Kubernetes host under off: %s %s, /run egress %+v, the row %s; want it run through Envoy", state, reasonOf(reason), f.Ran().Egress, recorded)
	}
}

// Fail closed on Kubernetes (#2286, codex on #2317): production runs the image under off and the host is now
// canyon; a promotion of it commits canyon and deploying:prod, then crashes before its /run. The retry, finding
// the image running, starts nothing, and prod's row records no spec — never canyon, which no /run of it applied,
// so the app's floor reads not enforced until prod's next /run.
func TestOnKubernetesARetryThatStartsNothingRecordsNoSpec(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	kubeDoor(t, ctx, p)
	deployRun, state, reason := deployOnce(t, ctx, p) // under the host's off
	var image string
	if err := p.QueryRow(ctx, "SELECT image FROM deploys WHERE env = 'preview'").Scan(&image); err != nil || state != "clean" {
		t.Fatalf("the preview deploy: %s %s (%v)", state, reasonOf(reason), err)
	}
	f.Start(project, "prod") // production runs the image, under off
	if _, err := p.Exec(ctx, "UPDATE egress_settings SET via = 'canyon'"); err != nil {
		t.Fatal(err)
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
	if _, err := p.Exec(ctx, "UPDATE runs SET egress = '{\"via\": \"canyon\"}' WHERE id = $1", run.ID); err != nil { // and kept the spec it would start prod under
		t.Fatal(err)
	}
	started := f.Runs.Load()
	_, err = pipe.deploy(ctx, in, "prod", image, built{})
	close(held)
	pipe.hold.Store(nil)
	waitEnded(t, ctx, p, run.ID)
	var via string
	if qerr := p.QueryRow(ctx, "SELECT coalesce(egress->>'via', 'none') FROM deploys WHERE env = 'prod'").Scan(&via); qerr != nil || err != nil || f.Runs.Load() != started || via != "none" {
		t.Fatalf("the retry on Kubernetes: %v, %d /runs, prod's row records %s (%v); want nothing started and no spec, which reads not enforced", err, f.Runs.Load()-started, via, qerr)
	}
}
