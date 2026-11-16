package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/getcanyon-ai/canyon/vault"
)

// policyAt serves GET /egress/policy over pool as the replica the test names in each request's X-Replica,
// public URL https://canyon.meridian.example, callers answered by the test server's executor.
func policyAt(t *testing.T, pool *pgxpool.Pool, platform ...string) (*policyServer, string) {
	t.Helper()
	s := &policyServer{pool: pool, public: "https://canyon.meridian.example", log: log.New(os.Stderr, "", 0),
		platform: func(context.Context, querier) ([]string, error) { return append([]string{}, platform...), nil },
		callers:  (&executor.Client{Base: fake.URL}).Callers,
		replica:  func(r *http.Request) (string, bool) { return r.Header.Get("X-Replica"), true },
		ee:       testEE(nil), // the policy's routes, as the server's hooks name them: none without Canyon Enterprise
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// pullPolicy is one GET /egress/policy as replica: its status and, on a 200, the policy.
func pullPolicy(t *testing.T, base, replica, query string) (int, egressPolicy) {
	t.Helper()
	code, raw := pullPolicyRaw(t, base, replica, query)
	var p egressPolicy
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	} else if code == http.StatusNotModified && len(raw) != 0 {
		t.Fatalf("a 304 with a body: %s", raw)
	}
	return code, p
}

// pullPolicyRaw is pullPolicy's request, answering the body as sent.
func pullPolicyRaw(t *testing.T, base, replica, query string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/policy?"+query, nil)
	req.Header.Set("X-Replica", replica)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func versionOf(p egressPolicy) string { return p.Epoch + ":" + strconv.FormatInt(p.Seq, 10) }

// A refetch whose POST /callers fails is 503 caller_unresolved, and no policy: never the policy as if no one held
// the address, which egress-control would remember for 15 s, refusing a new app after the executor is back
// (contracts/routes.md, Egress control).
func TestARefetchTheExecutorCannotAnswerIs503(t *testing.T) {
	h := outboundHost(t)
	s, base := policyAt(t, h.pool)
	s.callers = func(context.Context, []string) ([]executor.Caller, error) {
		return nil, errors.New("executor: connection refused")
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/policy?have=&caller=172.28.3.11", nil)
	req.Header.Set("X-Replica", "canyon-egress-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var refused struct{ Error string }
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &refused); resp.StatusCode != http.StatusServiceUnavailable || err != nil || refused.Error != "caller_unresolved" {
		t.Errorf("a refetch the executor could not answer: %d %s, want 503 caller_unresolved", resp.StatusCode, raw)
	}
}

// contracts/routes.md, Egress control: 304 only on the exact version; seq moves on a change; epoch is drawn at
// each start, so a database restored to a lower seq — its old epoch with it — is answered under a new epoch,
// never the old one a replica holds a higher seq of, which it would refuse as going backwards; and a running
// server that reads a seq lower than one it answered under the same epoch — a failover to a lagging database —
// draws a new one too.
func TestVersionEpochNeverGoesBackward(t *testing.T) {
	pool, ctx := migrated(t)
	start(t, pool, time.Hour) // the executor the callers are asked of
	_, base := policyAt(t, pool)
	if err := drawEpoch(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, first := pullPolicy(t, base, "r1", "have=")
	for _, have := range []string{"", "nonsense", first.Epoch, first.Epoch + ":0", first.Epoch + ":" + strconv.FormatInt(first.Seq-1, 10), strings.Repeat("0", 32) + ":" + strconv.FormatInt(first.Seq, 10)} {
		if code, _ := pullPolicy(t, base, "r1", "have="+have); code != http.StatusOK {
			t.Errorf("have=%q: %d, want 200: only the exact version is 304", have, code)
		}
	}
	if code, _ := pullPolicy(t, base, "r1", "have="+versionOf(first)); code != http.StatusNotModified {
		t.Fatalf("have=the current version: %d, want 304", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE egress_settings SET mode = 'allowlist'"); err != nil {
		t.Fatal(err)
	}
	code, later := pullPolicy(t, base, "r1", "have="+versionOf(first))
	if code != http.StatusOK || later.Epoch != first.Epoch || later.Seq <= first.Seq {
		t.Fatalf("after a change: %d %s, want 200 under %s with a higher seq", code, versionOf(later), first.Epoch)
	}

	// A failover to a database that had not taken the change, under the running server.
	if _, err := pool.Exec(ctx, "UPDATE egress_policy SET seq = $1", first.Seq); err != nil {
		t.Fatal(err)
	}
	code, failedOver := pullPolicy(t, base, "r1", "have="+versionOf(later))
	if code != http.StatusOK || failedOver.Epoch == later.Epoch {
		t.Fatalf("after a failover: %d %s, want 200 under an epoch other than %s, which holds a higher seq", code, versionOf(failedOver), later.Epoch)
	}

	// A backup taken when first was current, restored, and a server started onto it: a process of its own, which
	// has answered nothing yet.
	if _, err := pool.Exec(ctx, "UPDATE egress_policy SET epoch = $1, seq = $2", first.Epoch, first.Seq); err != nil {
		t.Fatal(err)
	}
	if err := drawEpoch(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, restarted := policyAt(t, pool)
	code, restored := pullPolicy(t, restarted, "r1", "have="+versionOf(later))
	if code != http.StatusOK || restored.Epoch == later.Epoch {
		t.Fatalf("after a restore: %d %s, want 200 under an epoch other than %s, which holds a higher seq", code, versionOf(restored), later.Epoch)
	}
}

// design/EGRESS.md §3.2, Convergence: a /run under via canyon names its env in the policy first, then waits until
// every live egress-control replica holds that version; past the bound it fails closed naming the replica behind,
// and nothing starts. A replica unseen for longer than three polls is not live and is not waited for.
func TestRunWaitsForEveryReplica(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	pipe.EgressProxy, pipe.EgressConverge = "http://canyon-egress:3128", 2*time.Second
	t.Cleanup(func() { pipe.EgressProxy, pipe.EgressConverge = "", 0 })
	_, policy := policyAt(t, pool)
	var epoch string
	var seq int64
	if err := pool.QueryRow(ctx, "SELECT epoch, seq FROM egress_policy").Scan(&epoch, &seq); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_replicas (replica, have_epoch, have_seq, seen_at) VALUES
		('canyon-egress-a', $1, $2, now()), ('canyon-egress-b', $1, $2, now()), ('canyon-egress-gone', $1, 1, now() - interval '1 minute')`, epoch, seq); err != nil {
		t.Fatal(err)
	}
	image := "canyon/claims-portal@" + executortest.Digest
	ran := fake.Runs.Load()
	keepPolling := func(replica string, stop <-chan struct{}) { // a live replica's 5 s polls, shortened: each sends the version it holds
		held := ""
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				if code, p := pullPolicy(t, policy, replica, "have="+held); code == http.StatusOK {
					held = versionOf(p)
				}
			}
		}
	}
	stop := make(chan struct{})
	go keepPolling("canyon-egress-a", stop) // b stays live, at the version before the env
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				if _, err := pool.Exec(ctx, "UPDATE egress_replicas SET seen_at = now() WHERE replica = 'canyon-egress-b'"); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	began := time.Now()
	_, _, _, err := pipe.RunApp(ctx, "gr_8f2a", "claims-portal", "preview", image, runs.Egress{Via: "canyon"}, "")
	if err == nil || err.Error() != "the egress gateway has not taken this version (canyon-egress-b)" || fake.Runs.Load() != ran {
		close(stop)
		t.Fatalf("a replica behind past the bound: %v, %d /run(s); want the run failed closed naming canyon-egress-b, nothing started", err, fake.Runs.Load()-ran)
	}
	if waited := time.Since(began); waited < 2*time.Second {
		t.Errorf("failed after %s, before the bound", waited)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM egress_envs WHERE project_id = 'claims-portal' AND env = 'preview'"); n != 0 {
		t.Errorf("the failed run left the env it named %d time(s), want it dropped", n)
	}
	close(stop)

	both := make(chan struct{}) // a channel of its own: the goroutines above still read stop
	defer close(both)
	go keepPolling("canyon-egress-a", both)
	go keepPolling("canyon-egress-b", both)
	if _, _, _, err := pipe.RunApp(ctx, "gr_8f2a", "claims-portal", "preview", image, runs.Egress{Via: "canyon"}, ""); err != nil || fake.Runs.Load() != ran+1 {
		t.Fatalf("every live replica current: %v, %d /run(s); want it started once", err, fake.Runs.Load()-ran)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM egress_envs WHERE project_id = 'claims-portal' AND env = 'preview'"); n != 1 {
		t.Errorf("the policy names the env %d time(s), want once: named before the /run that every replica took", n)
	}
}

// The policy names each app env Canyon runs under via canyon, with its mode — its own, else the org's — and its
// allowlist without the platform destinations, the callers the executor named for those envs alone, the platform
// destinations and the public URL; never a credential. An unknown address is asked of the executor once and is a
// caller from the next version on; an address of no env Canyon runs, or no one's, stays unknown. Each replica is
// tracked: what it holds, when it asked, since when its policy is out of date.
func TestPolicyAnswersWhatEgressControlDecidesWith(t *testing.T) {
	h := outboundHost(t) // its deploy rows, under via canyon, name both envs
	h.exec(t,
		"INSERT INTO egress_modes (project_id, env, mode) VALUES ('claims-portal', 'prod', 'allowlist')",
		"INSERT INTO egress_allowed (project_id, env, entry) VALUES ('claims-portal', 'prod', 'api.stripe.com'), ('claims-portal', 'prod', '*.slack.com'), ('claims-portal', 'prod', 'login.example-idp.com')",
	)
	_, base := policyAt(t, h.pool, "login.example-idp.com")
	fake.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web"},
		executor.Caller{Address: "172.28.9.11", App: "intake-bot", Target: "prod", Kind: "web"})
	code, p := pullPolicy(t, base, "canyon-egress-a", "have=")
	if code != http.StatusOK || p.PublicURL != "https://canyon.meridian.example" || len(p.Callers) != 0 || !slices.Equal(p.Platform, []string{"login.example-idp.com"}) {
		t.Fatalf("%d %+v", code, p)
	}
	want := []policyEnv{{"claims-portal", "preview", "observe", []string{}, []string{}, false, "", []policyRoute{}}, {"claims-portal", "prod", "allowlist", []string{"*.slack.com", "api.stripe.com"}, []string{}, false, "", []policyRoute{}}}
	if !slices.EqualFunc(p.Envs, want, func(a, b policyEnv) bool {
		return a.App == b.App && a.Env == b.Env && a.Mode == b.Mode && slices.Equal(a.Allowed, b.Allowed)
	}) {
		t.Fatalf("envs %+v, want %+v", p.Envs, want)
	}
	asks := fake.CallersAsks.Load()
	code, refetched := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(p)+"&caller=172.28.3.11")
	if code != http.StatusOK || refetched.Seq <= p.Seq || len(refetched.Callers) != 1 || refetched.Callers[0] != (policyCaller{"172.28.3.11", "claims-portal", "prod"}) {
		t.Fatalf("a refetch of a caller the executor names: %d %+v", code, refetched)
	}
	for _, addr := range []string{"172.28.9.11", "172.28.3.99"} { // an app env Canyon does not run under canyon, and no one
		if code, _ := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(refetched)+"&caller="+addr); code != http.StatusNotModified {
			t.Errorf("a refetch of %s: %d, want 304: the policy it has", addr, code)
		}
	}
	if code, _ := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(refetched)+"&caller=172.28.3.11"); code != http.StatusNotModified {
		t.Errorf("a refetch of a caller the policy names: %d, want 304", code)
	}
	if n := fake.CallersAsks.Load() - asks; n != 3 {
		t.Errorf("%d POST /callers, want one for each address the policy did not name", n)
	}
	egressShape(t, "policy", policyBody(t, base, "canyon-egress-c", "have=")) // a caller, an empty allowlist, a platform destination
	if p.Format != 1 {
		t.Errorf("format %d, want 1", p.Format)
	}

	pullPolicy(t, base, "canyon-egress-b", "have="+versionOf(p)) // b holds the version before the refetch
	h.exec(t, "INSERT INTO egress_replicas (replica, seen_at) VALUES ('canyon-egress-gone', now() - interval '2 days')")
	code, body, _ := do(t, h.dana, http.MethodGet, h.base+"/api/v1/egress", "")
	egressShape(t, "setting_admin", lastBody)
	replicas, _ := body["replicas"].([]any)
	if code != 200 || len(replicas) != 3 {
		t.Fatalf("the admin's read: %d %s, want the three replicas seen in the last day", code, lastBody)
	}
	a, b := replicas[0].(map[string]any), replicas[1].(map[string]any)
	if a["replica"] != "canyon-egress-a" || a["have"] != versionOf(refetched) || a["current_at"] == nil || a["seen_at"] == nil {
		t.Errorf("a current replica: %v", a)
	}
	if b["replica"] != "canyon-egress-b" || b["have"] != versionOf(p) || b["current_at"] != nil {
		t.Errorf("a replica that never held the current version: %v", b)
	}
	if c := replicas[2].(map[string]any); c["replica"] != "canyon-egress-c" || c["have"] != nil || c["current_at"] != nil {
		t.Errorf("a replica holding no version: %v", c)
	}
	pullPolicy(t, base, "canyon-egress-a", "have=") // a, current, now says it holds none
	if n := count(t, h.ctx, h.pool, "SELECT count(*) FROM egress_replicas WHERE replica = 'canyon-egress-a' AND have_seq IS NULL AND current_at IS NULL"); n != 1 {
		t.Error("a replica that holds no version kept the time it was last current")
	}
	if code, body, _ := do(t, h.carl, http.MethodGet, h.base+"/api/v1/egress", ""); code != 200 || body["replicas"] != nil {
		t.Errorf("a member's read: %d %s, want no replicas", code, lastBody)
	}
}

// policyBody is one GET /egress/policy's body as replica.
func policyBody(t *testing.T, base, replica, query string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/egress/policy?"+query, nil)
	req.Header.Set("X-Replica", replica)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// A caller stays while the executor names its address as before: confirm drops one it names otherwise or not at
// all — an address another app's pod holds now — at once, under a new version; one it cannot confirm — the
// executor not answering — goes once callerKept has passed, never named past it.
func TestConfirmDropsACallerWhoseHolderChanged(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, id := range []string{"claims-portal", "intake-bot"} {
		if code, _, _ := do(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"`+id+`"}`); code != 201 {
			t.Fatalf("create %s: %d %s", id, code, lastBody)
		}
	}
	s, _ := policyAt(t, pool)
	for _, q := range []string{
		"INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'prod'), ('intake-bot', 'prod')",
		"INSERT INTO egress_callers (address, project_id, env) VALUES ('172.28.3.11', 'claims-portal', 'prod'), ('172.28.3.13', 'claims-portal', 'prod'), ('172.28.4.11', 'intake-bot', 'prod')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	fake.Holds(executor.Caller{Address: "172.28.3.11", App: "claims-portal", Target: "prod", Kind: "web"}, // as before
		executor.Caller{Address: "172.28.4.11", App: "claims-portal", Target: "prod", Kind: "web"}) // another app's now; .13 no one's
	var was int64
	if err := pool.QueryRow(ctx, "SELECT seq FROM egress_policy").Scan(&was); err != nil {
		t.Fatal(err)
	}
	if err := s.confirm(ctx); err != nil {
		t.Fatal(err)
	}
	callers := func() []string {
		rows, err := pool.Query(ctx, "SELECT address FROM egress_callers ORDER BY address")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			out = append(out, a)
		}
		return out
	}
	if got := callers(); !slices.Equal(got, []string{"172.28.3.11"}) {
		t.Fatalf("callers %v, want the one the executor names as before", got)
	}
	var now int64
	if err := pool.QueryRow(ctx, "SELECT seq FROM egress_policy").Scan(&now); err != nil || now <= was {
		t.Fatalf("seq %d after dropping callers, was %d", now, was)
	}
	if err := s.confirm(ctx); err != nil { // the one left, named as before: confirmed, nothing the policy says moved
		t.Fatal(err)
	}
	var again int64
	if err := pool.QueryRow(ctx, "SELECT seq FROM egress_policy").Scan(&again); err != nil || again != now || !slices.Equal(callers(), []string{"172.28.3.11"}) {
		t.Fatalf("a confirm that only confirmed: seq %d, was %d, callers %v; want it unmoved", again, now, callers())
	}
	fake.FailCallers.Store(true)
	t.Cleanup(func() { fake.FailCallers.Store(false) })
	if err := s.confirm(ctx); err != nil || !slices.Equal(callers(), []string{"172.28.3.11"}) {
		t.Fatalf("an executor that cannot tell: %v, callers %v; kept while confirmed within callerKept", err, callers())
	}
	if _, err := pool.Exec(ctx, "UPDATE egress_callers SET resolved_at = now() - make_interval(secs => $1)", callerKept.Seconds()+1); err != nil {
		t.Fatal(err)
	}
	if err := s.confirm(ctx); err != nil || len(callers()) != 0 {
		t.Fatalf("unconfirmed for callerKept: %v, callers %v; want none, so none outlives callerKept unconfirmed", err, callers())
	}
}

// The policy never names a caller unconfirmed for callerKept, however long the confirm takes: each answer first
// drops such a caller, under a new version, so a replica holding the version that named it is told 200.
func TestPolicyNamesNoCallerPastItsBound(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	_, policy := policyAt(t, pool)
	for _, q := range []string{
		"INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'prod')",
		"INSERT INTO egress_callers (address, project_id, env) VALUES ('172.28.3.11', 'claims-portal', 'prod'), ('172.28.3.13', 'claims-portal', 'prod')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, held := pullPolicy(t, policy, "r1", "have=")
	if len(held.Callers) != 2 {
		t.Fatalf("callers %+v, want both", held.Callers)
	}
	if _, err := pool.Exec(ctx, "UPDATE egress_callers SET resolved_at = now() - make_interval(secs => $1) WHERE address = '172.28.3.13'", callerKept.Seconds()+1); err != nil {
		t.Fatal(err)
	}
	code, p := pullPolicy(t, policy, "r1", "have="+versionOf(held))
	if code != http.StatusOK || p.Seq <= held.Seq || len(p.Callers) != 1 || p.Callers[0].Address != "172.28.3.11" {
		t.Fatalf("a caller unconfirmed past the bound, no confirm run: %d %+v, want 200 naming 172.28.3.11 alone under a new version", code, p)
	}
}

// The wait never fails open (design/EGRESS.md §3.2, Convergence): a live replica holding another epoch's version,
// however high its seq, or none it could say, is behind; and when the replicas or the version cannot be read —
// the database refusing, the run's context ended — the /run fails, nothing started, never skipping the wait.
func TestRunFailsClosedWhenItCannotTellTheReplicas(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("create: %d %s", code, lastBody)
	}
	pipe.EgressProxy, pipe.EgressConverge = "http://canyon-egress:3128", 300*time.Millisecond
	t.Cleanup(func() { pipe.EgressProxy, pipe.EgressConverge = "", 0 })
	image := "canyon/claims-portal@" + executortest.Digest
	ran := fake.Runs.Load()
	run := func(ctx context.Context) error {
		_, _, _, err := pipe.RunApp(ctx, "gr_8f2a", "claims-portal", "preview", image, runs.Egress{Via: "canyon"}, "")
		return err
	}
	exec := func(q string) {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO egress_replicas (replica, have_epoch, have_seq, seen_at) VALUES ('canyon-egress-other-epoch', '00000000000000000000000000000000', 9000000, now())")
	if err := run(ctx); err == nil || err.Error() != "the egress gateway has not taken this version (canyon-egress-other-epoch)" {
		t.Errorf("a live replica holding another epoch's version: %v, want it behind", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM egress_envs"); n != 0 {
		t.Errorf("a first /run that failed left its env named %d time(s), want none", n)
	}
	exec("UPDATE egress_replicas SET replica = 'canyon-egress-no-version', have_epoch = NULL, have_seq = NULL, seen_at = now()")
	if err := run(ctx); err == nil || err.Error() != "the egress gateway has not taken this version (canyon-egress-no-version)" {
		t.Errorf("a live replica holding no version it could say: %v, want it behind", err)
	}
	exec("ALTER TABLE egress_replicas RENAME TO egress_replicas_away")
	if err := run(ctx); err == nil || !strings.Contains(err.Error(), "egress_replicas") {
		t.Errorf("the replicas unreadable: %v, want the run failed", err)
	}
	exec("ALTER TABLE egress_replicas_away RENAME TO egress_replicas")
	exec("DELETE FROM egress_replicas")
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if err := run(ended); err == nil {
		t.Error("the run's context ended: the wait answered nil, want the run failed")
	}
	if n := fake.Runs.Load() - ran; n != 0 {
		t.Fatalf("%d /run(s) past a wait that could not tell, want none", n)
	}
	fake.FailRun = true
	if err := run(ctx); err == nil || count(t, ctx, pool, "SELECT count(*) FROM egress_envs") != 0 {
		t.Errorf("a first /run the executor failed: %v, %d env(s) named; want it failed, none named", err, count(t, ctx, pool, "SELECT count(*) FROM egress_envs"))
	}
	fake.FailRun = false
	if err := run(ctx); err != nil || fake.Runs.Load() != ran+2 { // no replica live, the read answered: nothing to wait for
		t.Fatalf("no live replica: %v, %d /run(s), want it started", err, fake.Runs.Load()-ran)
	}
}

// The listener's certificate is minted once and served again at every start after: the one egress-control pins.
func TestPolicyCertIsMintedOnce(t *testing.T) {
	pool, ctx := migrated(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := policyCert(ctx, pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	again, err := policyCert(ctx, pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], again.Certificate[0]) || count(t, ctx, pool, "SELECT count(*) FROM egress_policy_cert") != 1 {
		t.Fatal("a second start minted another certificate: egress-control's pin would refuse the server")
	}
	var out strings.Builder // what the install step publishes: that certificate, and no key
	if err := printPolicyCert(ctx, pool, &out); err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode([]byte(out.String()))
	if block == nil || block.Type != "CERTIFICATE" || !bytes.Equal(block.Bytes, first.Certificate[0]) || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("egress-policy-cert printed %q, want the served certificate alone", out.String())
	}
}

// canyon-server egress-policy-cert before the first start that mints the certificate says so, printing nothing.
func TestPolicyCertPrintedBeforeItIsMinted(t *testing.T) {
	pool, ctx := migrated(t)
	var out strings.Builder
	if err := printPolicyCert(ctx, pool, &out); err == nil || out.Len() != 0 || !strings.Contains(err.Error(), "no certificate yet") {
		t.Fatalf("%v, printed %q; want the error alone", err, out.String())
	}
}

// A restore or a failover always restarts the database's server: an answer whose snapshot finds another start than
// the last answer's is under a new epoch, even once writes have taken seq back to the version a replica holds,
// whose content then differs (contracts/routes.md, Egress control).
func TestARestartedDatabaseIsANewEpoch(t *testing.T) {
	pool, ctx := migrated(t)
	start(t, pool, time.Hour)
	var started atomic.Int64
	started.Store(1)
	s, _ := policyAt(t, pool)
	stub := &policyServer{pool: pool, public: s.public, log: s.log, platform: s.platform, callers: s.callers, replica: s.replica,
		started: func(context.Context, pgx.Tx) (time.Time, error) { return time.Unix(started.Load(), 0), nil }}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	if _, err := pool.Exec(ctx, "UPDATE egress_settings SET via = via"); err != nil { // a version after the first, so one before it exists
		t.Fatal(err)
	}
	_, held := pullPolicy(t, srv.URL, "r1", "have=")
	// The database restored to the version before held's and restarted; a write takes seq back to held's.
	for _, q := range []string{"UPDATE egress_policy SET seq = seq - 1", "UPDATE egress_settings SET mode = 'allowlist'"} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	started.Store(2)
	code, after := pullPolicy(t, srv.URL, "r1", "have="+versionOf(held))
	if code != http.StatusOK || after.Epoch == held.Epoch {
		t.Fatalf("the database started again: %d %s, want 200 under an epoch other than %s", code, versionOf(after), held.Epoch)
	}
	if code, _ := pullPolicy(t, srv.URL, "r1", "have="+versionOf(after)); code != http.StatusNotModified {
		t.Errorf("the same start again: %d, want 304, no epoch drawn twice", code)
	}
}

// Each env's known[] is the names a call of it went through to (admitted_at) in the last 30 days — never a name
// only refused (rate_limited, not_allowed, port), never invalid, never an IPv4 address, never another env's —
// sorted, the rate limit's no new name (contracts/routes.md, the policy row). It moves no version: a destination
// recorded leaves the version a replica holds current.
func TestPolicyKnownIsWhatTheRecordsHold(t *testing.T) {
	h := outboundHost(t)
	_, base := policyAt(t, h.pool)
	h.exec(t, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags, admitted_at) VALUES
		('claims-portal', 'prod', 'api.stripe.com', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}', now()),
		('claims-portal', 'prod', 'api.stripe.com', 80, now(), now(), 1, 'allowed', '2xx', 'http', '{}', now()),
		('claims-portal', 'prod', 'hooks.slack.com', 443, now() - interval '29 days', now(), 2, 'refused', '4xx', 'connect', '{UAEX}', now() - interval '29 days'),
		('claims-portal', 'prod', 'old.example.com', 443, now() - interval '31 days', now() - interval '31 days', 1, 'allowed', '2xx', 'connect', '{}', now() - interval '31 days'),
		('claims-portal', 'prod', 'flood.example.com', 443, now(), now(), 9, 'refused', '4xx', 'connect', '{UAEX}', NULL),
		('claims-portal', 'prod', 'api.openai.com', 8443, now(), now(), 1, 'refused', '4xx', 'connect', '{UAEX}', NULL),
		('claims-portal', 'prod', 'invalid', 443, now(), now(), 1, 'refused', '4xx', 'connect', '{UAEX}', NULL),
		('claims-portal', 'prod', '10.0.0.1', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}', now()),
		('claims-portal', 'preview', 'registry.npmjs.org', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}', now())`)
	code, p := pullPolicy(t, base, "canyon-egress-a", "have=")
	if code != http.StatusOK {
		t.Fatalf("%d", code)
	}
	known := map[string][]string{}
	for _, e := range p.Envs {
		known[e.Env] = e.Known
	}
	if !slices.Equal(known["prod"], []string{"api.stripe.com", "hooks.slack.com"}) || !slices.Equal(known["preview"], []string{"registry.npmjs.org"}) {
		t.Errorf("known %v, want each env's names a call went through to in 30 days: none only refused, no invalid, no address", known)
	}
	h.exec(t, `INSERT INTO egress_destinations (app, env, destination, port, first_seen_at, last_seen_at, calls, last_outcome, last_status_class, last_protocol, last_response_flags, admitted_at)
		VALUES ('claims-portal', 'prod', 'api.anthropic.com', 443, now(), now(), 1, 'allowed', '2xx', 'connect', '{}', now())`)
	if code, _ := pullPolicy(t, base, "canyon-egress-a", "have="+versionOf(p)); code != http.StatusNotModified {
		t.Errorf("a destination recorded: %d, want 304, the version held current", code)
	}
	egressShape(t, "policy", policyBody(t, base, "canyon-egress-b", "have="))
}
