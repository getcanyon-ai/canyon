package runs

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rhea holds production in these tests.
var rhea = events.Actor{Kind: "person", Subject: "rhea@meridian.example", Display: "Rhea Okafor"}

// holdProd holds the app's production as the hold route writes it, naming what production runs.
func holdProd(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	if _, err := p.Exec(ctx, `INSERT INTO prod_holds (project_id, package_id, image, gate_run, reason, held_by_kind, held_by_subject, held_by_display)
		SELECT $1, d.package_id, d.image, d.gate_run, 'Exports claimant SSNs unauthenticated (INC-2231)', $2, $3, $4 FROM (SELECT) one LEFT JOIN deploys d ON d.project_id = $1 AND d.env = 'prod'`,
		project, rhea.Kind, rhea.Subject, rhea.Display); err != nil {
		t.Fatal(err)
	}
}

// The held sentence ends a reason once (design/EXPERIENCE.md D5): one that already ends a sentence takes no full stop
// more, which read "while we look.. It stays stopped" (#2723).
func TestTheHeldLineEndsItsReasonOnce(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	for reason, want := range map[string]string{
		"INC-2231":               "Production was stopped by Rhea Okafor on 2026-11-08: INC-2231. It stays stopped until a responder or an admin lets it run again.",
		"Stop it while we look.": "Production was stopped by Rhea Okafor on 2026-11-08: Stop it while we look. It stays stopped until a responder or an admin lets it run again.",
		"Leaking PDFs?":          "Production was stopped by Rhea Okafor on 2026-11-08: Leaking PDFs? It stays stopped until a responder or an admin lets it run again.",
		"Stop it now!":           "Production was stopped by Rhea Okafor on 2026-11-08: Stop it now! It stays stopped until a responder or an admin lets it run again.",
	} {
		if got := (Hold{By: rhea, At: at, Reason: reason}).Line(); got != want {
			t.Errorf("%q:\n got %s\nwant %s", reason, got, want)
		}
	}
}

// While production is held every production writer refuses, and writes nothing (contracts/routes.md, The
// hold): a promotion's start and a rollback's, the shared production writer, and a promotion that started
// before the hold, which is stopped before its deploy as a cancel stops it. A stop pending on a run is the
// one write the writer still makes: the row says what runs until that run stops it.
func TestHeldRefusesEveryProdWriter(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	parked := make(chan struct{})
	var once sync.Once
	pipe.hold.Store(&parked)
	t.Cleanup(func() { pipe.hold.Store(nil); once.Do(func() { close(parked) }) })
	before, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	holdProd(t, ctx, p)
	once.Do(func() { close(parked) })
	if state, reason := waitEnded(t, ctx, p, before.ID); state != "failed" || why(reason) != "interrupted" || f.Runs.Load() != 1 {
		t.Fatalf("a promotion that started before the hold: %s %s, %d /run(s)", state, why(reason), f.Runs.Load())
	}
	for name, ship := range map[string]Promotion{
		"a promotion":          {Image: image, GateRun: deployRun, DeployedBy: dana},
		"a rollback's promote": {Image: image, GateRun: deployRun, DeployedBy: dana, Rollback: true, Reason: "INC-2231"},
	} {
		if _, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &ship}); !errors.Is(err, ErrHeld) {
			t.Errorf("%s while held: %v, want ErrHeld", name, err)
		}
	}
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state, kind, failure_reason, ended_at) VALUES ('gr_late', $1, $2, 'failed', 'promote', 'interrupted', now()), ('gr_stopping', $1, $2, 'running', 'promote', NULL, NULL)", project, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE runs SET deploy_started_at = now(), stop_requested_at = now() WHERE id = 'gr_stopping'"); err != nil {
		t.Fatal(err)
	}
	write := func(run string) (bool, error) {
		tx, err := p.Begin(ctx)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, live, err := WriteProdTx(ctx, tx, Deployed{Project: project, Package: pkg, Image: image, URL: "http://claims-portal.prod.localhost:7777", GateRun: deployRun, Run: run, RunBy: "canyon"})
		if err != nil {
			return false, err
		}
		return live, tx.Commit(ctx)
	}
	if _, err := write("gr_late"); !errors.Is(err, ErrHeld) {
		t.Errorf("the production writer while held: %v, want ErrHeld", err)
	}
	if n := countOf(t, ctx, p, "SELECT (SELECT count(*) FROM deploys WHERE env = 'prod') + (SELECT count(*) FROM deploy_history) + (SELECT count(*) FROM runs WHERE kind = 'promote' AND id NOT IN ('gr_late', 'gr_stopping'))"); n != 1 {
		t.Fatalf("held, production was written or a promotion started: %d rows beside the one refused before the hold", n)
	}
	if live, err := write("gr_stopping"); err != nil || live || countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = 'prod'") != 1 {
		t.Fatalf("a run with a stop pending, met by the hold mid-switch: live %v (%v); its row says what runs until it stops it", live, err)
	}
}

// The held image is kept, so a lift starts its exact bytes again; and a deploy run while production is
// held says so, as its last line.
func TestTheHeldImageIsKeptAndADeploySaysProductionIsHeld(t *testing.T) {
	p, ctx, _ := deploying(t, runstest.CleanGolden)
	_, held := previewImage(t, ctx, p)
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, url, gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	holdProd(t, ctx, p)
	if _, err := p.Exec(ctx, "DELETE FROM deploys WHERE env = 'prod'"); err != nil {
		t.Fatal(err)
	}
	keep, err := pipe.keep(ctx, project, image("f"))
	if err != nil || !slices.Contains(keep, digestOf(held)) {
		t.Fatalf("the keep while held: %v (%v), want the held %s", keep, err, digestOf(held))
	}
	run, state, reason := deployOnce(t, ctx, p)
	if want := "Production was stopped by Rhea Okafor on " + heldOn(t, ctx, p) + ": Exports claimant SSNs unauthenticated (INC-2231). It stays stopped until a responder or an admin lets it run again."; state != "clean" || lastLine(t, ctx, p, run) != want {
		t.Fatalf("a deploy while held: %s %s, last line %q", state, reasonOf(reason), lastLine(t, ctx, p, run))
	}
}

// heldOn is the day the hold was placed, as the hold's line names it.
func heldOn(t *testing.T, ctx context.Context, p *pgxpool.Pool) string {
	t.Helper()
	h, err := ReadHold(ctx, p, project)
	if err != nil || h == nil {
		t.Fatalf("the hold: %+v (%v)", h, err)
	}
	return h.At.UTC().Format("2006-01-02")
}

// holdAs is the hold route's first step on its own: the hold row and the brake on the run writing
// production, which writes no request undeploy row (contracts/routes.md, The hold).
func holdAs(t *testing.T, ctx context.Context, p *pgxpool.Pool) Braked {
	t.Helper()
	holdProd(t, ctx, p)
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := BrakeTx(ctx, tx, project, "prod", rhea, true)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A hold that meets a promotion mid-switch is carried out by the run, as an undeploy's stop is, and the
// run writes the hold's row: stop app, by the one who held, naming the run it cancelled — no undeploy
// row and no request undeploy row.
func TestAHoldMidSwitchStopsAfterItAndWritesStopApp(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	run, release := midSwitch(t, ctx, p, f, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	if b := holdAs(t, ctx, p); b.Run == nil || b.Run.ID != run || !b.Stopping {
		t.Fatalf("the hold mid-switch: %+v", b)
	}
	release()
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "interrupted" || f.Stops.Load() != 1 {
		t.Fatalf("the promotion the hold met: %s %s, %d stop(s)", state, why(reason), f.Stops.Load())
	}
	var detail string
	if err := p.QueryRow(ctx, "SELECT detail::text FROM audit_events WHERE action = 'stop app' AND actor_subject = $1 AND run_id = $2", rhea.Subject, run).Scan(&detail); err != nil ||
		detail != `{"env": "prod", "reason": "Exports claimant SSNs unauthenticated (INC-2231)", "package": "`+pkg+`", "cancelled": ["`+run+`"], "image_digest": "`+digestOf(image)+`"}` {
		t.Fatalf("the stop app row: %q (%v)", detail, err)
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM audit_events WHERE action IN ('request undeploy', '"+UndeployAction+"')") + countOf(t, ctx, p, "SELECT count(*) FROM deploys WHERE env = 'prod'"); n != 0 {
		t.Fatalf("an undeploy's rows, or production's row, beside the hold's: %d", n)
	}
}

// A stop the run cannot carry out while held leaves the hold, says an admin looks the run up — never to
// take it offline again, which the hold refuses — and writes the hold's stop app row with stopped false.
func TestAHeldStopThatFailsSendsItToAnAdmin(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	deployRun, image := previewImage(t, ctx, p)
	was := stopRetries
	stopRetries = 1
	t.Cleanup(func() { stopRetries = was })
	run, release := midSwitch(t, ctx, p, f, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: deployRun, DeployedBy: dana}})
	holdAs(t, ctx, p)
	f.FailStop.Store(true)
	release()
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "error" {
		t.Fatalf("a held stop that failed: %s %s", state, why(reason))
	}
	if line, want := lastLine(t, ctx, p, run), "production was held while "+pkg[4:]+" was starting; Canyon could not stop it: an admin can look up run "+run; line != want {
		t.Fatalf("the run's last line: %q", line)
	}
	if countOf(t, ctx, p, "SELECT count(*) FROM prod_holds") != 1 {
		t.Fatal("the hold went with the failed stop")
	}
	if n := countOf(t, ctx, p, `SELECT count(*) FROM audit_events WHERE action = 'stop app' AND run_id = '`+run+`' AND actor_subject = '`+rhea.Subject+`'
		AND detail @> '{"stopped": false, "env": "prod", "package": "`+pkg+`"}' AND detail->'cancelled' = '["`+run+`"]'`); n != 1 {
		t.Fatalf("the stop app row saying the stop failed: %d", n)
	}
}

// The server-start reconcile ends a promotion the hold met mid-switch whose workflow DBOS gave up on: it
// carries the stop out without the run's input — no Promote — and the stop app row names the image the run
// was switching in, off its row. Before, it panicked at boot on every restart.
func TestTheReconcileCarriesOutAHeldStopWithoutItsInput(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	_, image := previewImage(t, ctx, p)
	if _, err := p.Exec(ctx, "INSERT INTO deploys (project_id, env, package_id, image, url, gate_run) SELECT project_id, 'prod', package_id, image, url, gate_run FROM deploys WHERE env = 'preview'"); err != nil {
		t.Fatal(err)
	}
	holdProd(t, ctx, p)
	if _, err := p.Exec(ctx, `INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at, stop_requested_at, stop_requested_by_kind, stop_requested_by_subject, stop_requested_by_display, promote_image)
		VALUES ('gr_heldgone000', $1, $2, 'running', 'promote', now(), now(), $3, $4, $5, $6)`, project, pkg, rhea.Kind, rhea.Subject, rhea.Display, image); err != nil {
		t.Fatal(err)
	}
	f.Start(project, "prod")
	if _, err := pipe.Reconcile(ctx, durable); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, "gr_heldgone000"); state != "failed" || why(reason) != "interrupted" || f.Stops.Load() != 1 {
		t.Fatalf("the held run the reconcile ended: %s %s, %d stop(s)", state, why(reason), f.Stops.Load())
	}
	if n := countOf(t, ctx, p, `SELECT count(*) FROM audit_events WHERE action = 'stop app' AND actor_subject = '`+rhea.Subject+`' AND run_id = 'gr_heldgone000' AND detail->>'image_digest' = '`+digestOf(image)+`'`); n != 1 {
		t.Fatalf("the stop app row naming the image: %d", n)
	}
}

// Under the org's platform a held run that handed its release over ends with a remove of it outstanding:
// the hold's remove may have completed before the platform reported the release deployed, late, and the
// run's end finds no row and no open release — so it sends one of its own release.
func TestAHeldOrgRunEndsWithARemoveOfWhatItHandedOver(t *testing.T) {
	p, ctx := releasing(t, "wh_held_end_secret_000000001", newStubPlatform(t, 200)) //nolint:gosec // a test fixture, not a real secret
	img := image("c")
	holdProd(t, ctx, p)
	for _, q := range []string{
		`INSERT INTO runs (id, project_id, package_id, state, kind, deploy_started_at, stop_requested_at, stop_requested_by_kind, stop_requested_by_subject, stop_requested_by_display, promote_image)
			VALUES ('gr_heldorg0000', '` + project + `', '` + pkg + `', 'running', 'promote', now(), now(), 'person', '` + rhea.Subject + `', '` + rhea.Display + `', '` + img + `')`,
		`INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at)
			VALUES ('rel_handed00001', '` + project + `', 'prod', 'gr_heldorg0000', '` + pkg + `', 'deploy', '` + img + `', gen_random_uuid(), 'accepted', now(), now() - interval '2 minutes'),
			       ('rel_holdsrm0001', '` + project + `', 'prod', 'gr_heldorg0000', '` + pkg + `', 'remove', '` + img + `', gen_random_uuid(), 'removed', now(), now() - interval '1 minute')`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{Run: "gr_heldorg0000", Project: project, Package: pkg, Promote: &Promotion{Image: img}}
	if err := pipe.stopped(ctx, durable, in, "prod", stopAsk{Asked: true, By: rhea, Released: true}, true); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE event = 'remove' AND id <> 'rel_holdsrm0001' AND image = '"+img+"' AND created_at > now() - interval '1 minute'"); n != 1 {
		t.Fatalf("the held run's end sent %d remove(s) of what it handed over, want one", n)
	}
	t.Cleanup(func() { // its send, a workflow of its own, ends before the release timing is put back
		for deadline := time.Now().Add(20 * time.Second); countOf(t, ctx, p, "SELECT count(*) FROM releases WHERE event = 'remove' AND state = 'sent'") > 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the remove's send never ended")
			}
		}
	})
}
