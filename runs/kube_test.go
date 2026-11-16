package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// accessFile is what the Kubernetes host's access file answers in these tests: the server builds the real one.
const accessFile = `{"updated_at":"2026-09-26T10:00:00Z","groups_claim":"","members":{"domains":["meridian.example"],"emails":[]},"admins":{"emails":["dana@meridian.example"],"claim":"","value":""},"roles":{},"entries":[]}`

// onKubernetes makes pipe a Kubernetes host's for the test.
func onKubernetes(t *testing.T) {
	t.Helper()
	pipe.Access = func(context.Context, string) (json.RawMessage, error) { return json.RawMessage(accessFile), nil }
	t.Cleanup(func() { pipe.Access = nil })
}

// carried counts the values of the run's journal, the timeline and the audit trail holding literal, as
// written or as DBOS base64-encodes them.
func carried(t *testing.T, ctx context.Context, p *pgxpool.Pool, run, literal string) int {
	t.Helper()
	rows, err := p.Query(ctx, `SELECT output FROM `+durableSchema+`.operation_outputs WHERE workflow_uuid = $1 AND output IS NOT NULL
		UNION ALL SELECT error FROM `+durableSchema+`.operation_outputs WHERE workflow_uuid = $1 AND error IS NOT NULL
		UNION ALL SELECT concat_ws(' ', inputs, output, error) FROM `+durableSchema+`.workflow_status WHERE workflow_uuid = $1
		UNION ALL SELECT data::text FROM timeline_rows UNION ALL SELECT detail::text FROM audit_events`, run)
	if err != nil {
		t.Fatal(err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	for _, v := range values {
		for _, field := range strings.Fields(v) {
			decoded, _ := base64.StdEncoding.DecodeString(field)
			if strings.Contains(field, literal) || strings.Contains(string(decoded), literal) {
				hits++
			}
		}
	}
	return hits
}

// On Kubernetes a deploy's /run carries the front door set at the call — choice 2's client secret opened
// from the vault — the app's access file and the host's database CA; the recorded address is the front door's, in its form, and
// the secret reaches no step's output, the timeline or the trail.
func TestAKubernetesDeploySendsTheFrontDoorAndTheAccessFileAndJournalsNoSecret(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	pipe.DatabaseCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n" // the server read and checked it at start
	t.Cleanup(func() { pipe.DatabaseCA = "" })
	const secret = "oauth-client-7d2f9e41" //nolint:gosec // a synthetic value, not a credential
	keyRef, sealedKey, sealedValue, err := pipe.Vault.Seal("front-door", "signin", "client_secret", []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	door := `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"},"domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
		VALUES (now(), now(), $1, $2, $3, $4, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door), keyRef, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" || reason != nil {
		t.Fatalf("a clean deploy ends clean: %s %s", state, why(reason))
	}
	var sent, want map[string]any
	ran := f.Ran()
	if err := json.Unmarshal(ran.FrontDoor, &sent); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(door), &want)
	want["signin"].(map[string]any)["client_secret"] = secret
	if !reflect.DeepEqual(sent, want) || string(ran.Access) != accessFile {
		t.Fatalf("/run's front_door and access:\n%s\n%s", ran.FrontDoor, ran.Access)
	}
	if ran.Identity.JWKSURL != "http://127.0.0.1:4182/.well-known/jwks.json" { // the sidecar's, never the Docker host's CANYON_APPS_JWKS_ADDR
		t.Fatalf("/run's identity.jwks_url: %q", ran.Identity.JWKSURL)
	}
	if ran.DatabaseCA == nil || *ran.DatabaseCA != pipe.DatabaseCA {
		t.Fatalf("/run's database_ca: %v, want the host's bundle", ran.DatabaseCA)
	}
	var url string
	if err := p.QueryRow(ctx, "SELECT url FROM deploys WHERE project_id = $1 AND env = 'preview'", project).Scan(&url); err != nil || url != "https://claims-portal-preview.apps.meridian.example" {
		t.Fatalf("the address is the front door's, in its form: %q (%v)", url, err)
	}
	if n := carried(t, ctx, p, run.ID, secret); n != 0 {
		t.Fatalf("the client secret is in %d journaled, timeline or audit value(s)", n)
	}
	if n := carried(t, ctx, p, run.ID, "claims-portal-preview.apps.meridian.example"); n == 0 {
		t.Fatal("the search finds what the run did record: its address")
	}
}

// On Kubernetes a deploy with no front door ends failed/error before the executor is called, D5's line
// its last build.log line.
func TestAKubernetesDeployWithoutAFrontDoorFailsBeforeTheExecutor(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || failedIn(t, ctx, p, run.ID) != "" {
		t.Fatalf("a deploy without a front door, which D5's line explains, names no step of Canyon's: %s %s", state, why(reason))
	}
	if n := f.Scans.Load() + f.Builds.Load() + f.Runs.Load(); n != 0 {
		t.Fatalf("the executor was called %d time(s)", n)
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1", run.ID, events.BuildLog).Scan(&line); err != nil ||
		line != "No front door is set, so no app can run on this Canyon yet. An admin sets it in Settings › App hosting." {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
}

// With no preferences row to say who runs apps, doorSet reads it as canyon's (fail closed): a deploy behind an unprobed
// door ends before the executor, D5's line its last (#2438, the security review).
func TestADeployWithNoPreferencesRowBehindAnUnprobedDoorFails(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display) VALUES ($1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM org_preferences"); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || f.Runs.Load() != 0 {
		t.Fatalf("a deploy with no preferences row behind an unprobed door: %s %s, %d /run(s)", state, why(reason), f.Runs.Load())
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1", run.ID, events.BuildLog).Scan(&line); err != nil ||
		line != "The front door hasn't passed Canyon's check of this cluster, so Canyon won't run apps behind it. An admin saves it again in Settings › App hosting." {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
}

// Under run_by: org an unprobed door on a cluster that enforces NetworkPolicy — its probe found the policy enforced and
// only the certificate missing — stops nothing: the org's platform runs the app, and the deploy sends its release
// (#2438; HAND-OFF §1). Only Canyon's own /run is refused behind one.
func TestADeployUnderOrgBehindAnUnprobedDoorSendsItsRelease(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_unprobed", platform) //nolint:gosec // a test fixture, not a real secret
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), $1, 'person', 'dana', 'Dana')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	if n := len(platform.requests()); n == 0 {
		t.Fatalf("no release sent behind an unprobed door under org: %v", buildLogLines(t, ctx, p, run.ID))
	}
}

// Canyon builds nothing on a cluster it has not seen enforce NetworkPolicy, whatever run_by is (#2507): under org, behind a
// door saved while the probe answered not_enforced — a plugin that ignores policy, or one that lets a new pod out before
// its policy is in force (the probe's lateFix) — or could not finish, the deploy ends failed/error before the executor is
// called, D5's line its last build.log line; behind one whose save found it enforced, the same deploy builds. A verdict
// belongs to the save that wrote it: the release before saves the same door again by updating warning, probed_at and
// probe_fix alone, and such a save, whatever its probe found, leaves no verdict behind (db/migrations/0110).
func TestADeployOnAClusterNotEnforcingNetworkPolicyBuildsNothing(t *testing.T) {
	for _, c := range []struct {
		name            string
		enforced, stale bool
	}{{"not enforced", false, false}, {"enforced", true, false}, {"enforced, then saved by the release before", true, true}} {
		t.Run(c.name, func(t *testing.T) { deployEnforced(t, c.enforced, c.stale) })
	}
}

// deployEnforced is one case of the test above: a deploy under org behind a door whose save found NetworkPolicy enforced
// or not, and with stale saved again after as the release before saves it, its probe finding nothing enforced.
func deployEnforced(t *testing.T, enforced, stale bool) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (CASE WHEN $1 THEN now() END, $2, 'person', 'dana', 'Dana')`, enforced, []byte(door)); err != nil {
		t.Fatal(err)
	}
	if stale {
		if _, err := p.Exec(ctx, "UPDATE front_doors SET warning = $1, probed_at = CASE WHEN $2 THEN now() END, probe_fix = $3", "", false, "This cluster does not enforce NetworkPolicy."); err != nil {
			t.Fatal(err)
		}
		enforced = false
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	if enforced {
		if f.Builds.Load() != 1 {
			t.Fatalf("under org on a cluster enforcing NetworkPolicy the deploy builds: %d build(s), %s %s %v", f.Builds.Load(), state, why(reason), buildLogLines(t, ctx, p, run.ID))
		}
		return
	}
	if state != "failed" || why(reason) != "error" || failedIn(t, ctx, p, run.ID) != "" {
		t.Fatalf("under org on a cluster not enforcing NetworkPolicy: %s %s", state, why(reason))
	}
	if n := f.Scans.Load() + f.Builds.Load() + f.Runs.Load(); n != 0 {
		t.Fatalf("the executor was called %d time(s)", n)
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1", run.ID, events.BuildLog).Scan(&line); err != nil ||
		line != `Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.` {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
}

// Under run_by: canyon a probed door whose save is not known to have found NetworkPolicy enforced — probed before the
// probe caught a new pod let out before its policy (db/migrations/0110) — builds nothing either (#2507): the deploy ends
// failed/error before the executor is called, D5's line its last build.log line.
func TestACanyonDeployBehindAProbedDoorNotSeenEnforcingBuildsNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), $1, 'person', 'dana', 'Dana')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || failedIn(t, ctx, p, run.ID) != "" {
		t.Fatalf("under canyon behind a probed door not seen enforcing: %s %s", state, why(reason))
	}
	if n := f.Scans.Load() + f.Builds.Load() + f.Runs.Load(); n != 0 {
		t.Fatalf("the executor was called %d time(s)", n)
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1", run.ID, events.BuildLog).Scan(&line); err != nil ||
		line != `Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.` {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
}

// Canyon never runs an app behind a front door that has not passed the probe (#2438): under run_by: canyon a deploy
// behind one ends failed/error before the executor is called, D5's line its last build.log line, and RunApp itself —
// a promotion's, a patch's — refuses one whatever was read before it.
func TestAKubernetesDeployBehindAnUnprobedDoorFailsBeforeTheExecutor(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	onKubernetes(t)
	door := `{"choice":1,"gateway":"alb","alb_arn":"arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef","domain":"apps.meridian.example","form":"app-env.domain"}`
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display) VALUES ($1, 'person', 'dana@meridian.example', 'Dana Whitfield')`, []byte(door)); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" {
		t.Fatalf("a deploy behind an unprobed door: %s %s", state, why(reason))
	}
	if n := f.Scans.Load() + f.Builds.Load() + f.Runs.Load(); n != 0 {
		t.Fatalf("the executor was called %d time(s)", n)
	}
	var line string
	if err := p.QueryRow(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1", run.ID, events.BuildLog).Scan(&line); err != nil ||
		line != "The front door hasn't passed Canyon's check of this cluster, so Canyon won't run apps behind it. An admin saves it again in Settings › App hosting." {
		t.Fatalf("the last build.log line: %q (%v)", line, err)
	}
	if _, _, _, err := pipe.RunApp(ctx, "run_unprobed", project, "preview", "registry.example/canyon/claims-portal@sha256:aa", Egress{Via: "off"}, ""); !errors.Is(err, errDoorUnchecked) || f.Runs.Load() != 0 {
		t.Fatalf("RunApp behind an unprobed door: %v, %d /run(s)", err, f.Runs.Load())
	}
}
