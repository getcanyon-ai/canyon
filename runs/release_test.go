package runs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs/runstest"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/getcanyon-ai/canyon/webhook"
	"github.com/jackc/pgx/v5/pgxpool"
)

// received is one delivery the stub platform saw: its headers and the exact raw body, so a test
// can verify the signature independently and inspect the JSON without trusting our own decoder.
type received struct {
	Delivery, Signature string
	Body                []byte
}

// stubPlatform is the org's pipeline: it answers each delivery the codes list in order, the last
// repeating once the list is exhausted, and keeps every request it saw. onDelivery, when set before
// the run starts, runs inside the handler before it answers: a pipeline that acts before its 2xx.
type stubPlatform struct {
	mu         sync.Mutex
	codes      []int
	seen       []received
	onDelivery func(body []byte)
	Server     *httptest.Server
}

func newStubPlatform(t *testing.T, codes ...int) *stubPlatform {
	t.Helper()
	s := &stubPlatform{codes: codes}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		code := 200
		if len(s.codes) > 0 {
			code = s.codes[0]
			if len(s.codes) > 1 {
				s.codes = s.codes[1:]
			}
		}
		s.seen = append(s.seen, received{Delivery: r.Header.Get("Canyon-Delivery"), Signature: r.Header.Get("Canyon-Signature-256"), Body: body})
		s.mu.Unlock()
		if s.onDelivery != nil {
			s.onDelivery(body)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *stubPlatform) requests() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received{}, s.seen...)
}

// fastReleaseTiming overrides every wait so a test's run ends in milliseconds, never real minutes.
func fastReleaseTiming(t *testing.T) {
	t.Helper()
	knobs := []*time.Duration{&releaseFastRetry, &releaseFastWindow, &releaseSlowRetry, &releaseCeiling, &reportCheckEvery, &stillWaitingEvery, &reportCeiling}
	saved := make([]time.Duration, len(knobs))
	for i, k := range knobs {
		saved[i] = *k
	}
	// sent_at has only second resolution (release.schema.json's ts): the retry interval must cross
	// a whole second so two attempts are never stamped the same, as two real ones a minute apart never are.
	releaseFastRetry, releaseFastWindow, releaseSlowRetry, releaseCeiling = 1100*time.Millisecond, 3*time.Second, 1100*time.Millisecond, 5*time.Second
	reportCheckEvery, stillWaitingEvery, reportCeiling = 5*time.Millisecond, 20*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() {
		for i, k := range knobs {
			*k = saved[i]
		}
	})
}

// releasing sets up a project under run_by: org, a fresh vault and the fake build executor: the
// deploy step under org sends a release to platform instead of calling /run (design/HAND-OFF.md §3).
func releasing(t *testing.T, secret string, platform *stubPlatform) (*pgxpool.Pool, context.Context) {
	t.Helper()
	fastReleaseTiming(t)
	p, ctx := live(t, runstest.CleanGolden)
	f := executortest.New(t)
	keys, err := vault.Load(t.TempDir() + "/kek")
	if err != nil {
		t.Fatal(err)
	}
	pipe.Executor = &executor.Client{Base: f.URL}
	pipe.PublicURL, pipe.JWKSURL, pipe.Vault = "http://localhost:7777", "http://canyon-server:7780/.well-known/jwks.json", keys
	handOrg(t, ctx, p, secret, platform)
	return p, ctx
}

// handOrg hands the host's apps to the org's platform: run_by: org, the sidecar's image, and platform's webhook
// sealed under the pipeline's vault.
func handOrg(t *testing.T, ctx context.Context, p *pgxpool.Pool, secret string, platform *stubPlatform) {
	t.Helper()
	pipe.IdentityImage = "registry.example/canyon-identity@sha256:" + strings.Repeat("b", 64)
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	version, sealedKey, sealedValue, err := pipe.Vault.Seal("platform-webhook", "webhook", "secret", []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("host-token"))
	if _, err := p.Exec(ctx, `INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display)
		VALUES (1, $1, $2, $3, $4, $5, 'person', 'dana', 'Dana')`,
		platform.Server.URL, version, sealedKey, sealedValue, tokenHash[:]); err != nil {
		t.Fatal(err)
	}
}

func buildLogLines(t *testing.T, ctx context.Context, p *pgxpool.Pool, run string) []string {
	t.Helper()
	// a deploy's access lines after its deploy.completed row are their own story (access_test.go)
	rows, err := p.Query(ctx, "SELECT data->>'line' FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log' AND coalesce(dedupe, '') NOT IN ('access', 'description') AND coalesce(dedupe, '') NOT LIKE 'access:%' ORDER BY id", run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	return lines
}

// A stub receiver answering 500, 500, 200 sees three identical deliveries under one delivery id,
// each its own sent_at, and one build.log row per retried attempt (design/HAND-OFF.md §3, §9 item 3).
func TestAReleaseRetriesA5xxAndAcceptsThe2xx(t *testing.T) {
	const secret = "wh_org_pipeline_secret" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 500, 500, 200)
	p, ctx := releasing(t, secret, platform)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) != 3 {
		t.Fatalf("three deliveries, got %d: %+v", len(reqs), reqs)
	}
	delivery := reqs[0].Delivery
	sentAts := map[string]bool{}
	for _, r := range reqs {
		if r.Delivery != delivery || delivery == "" {
			t.Fatalf("every retry is the same delivery id: %+v", reqs)
		}
		var body struct {
			SentAt string `json:"sent_at"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		sentAts[body.SentAt] = true
		if !webhook.Verify([]byte(secret), r.Body, r.Signature) {
			t.Fatalf("every delivery verifies under the stored secret: %+v", r)
		}
	}
	if len(sentAts) != 3 {
		t.Fatalf("each attempt re-stamps its own sent_at: %v", sentAts)
	}
	lines := buildLogLines(t, ctx, p, run.ID)
	var retrying int
	for _, l := range lines {
		if strings.Contains(l, "retrying in") {
			retrying++
		}
	}
	if retrying != 2 {
		t.Fatalf("one build.log row per retried delivery, two retried before the 2xx: %v", lines)
	}
}

// Each attempt records when it was made and the status it got, the webhook's last delivery (#2439): the platform sees
// the first attempt's 503 on the row while the second is in flight, and the row ends on the second's 202, its
// attempted_at the second delivery's own sent_at.
func TestEachAttemptRecordsItsTimeAndStatus(t *testing.T) {
	const secret = "wh_org_pipeline_secret_attempts" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 503, 202)
	p, ctx := releasing(t, secret, platform)
	var midway atomic.Int64
	midway.Store(-1)
	platform.onDelivery = func([]byte) {
		if len(platform.requests()) == 2 {
			var status *int
			_ = p.QueryRow(ctx, "SELECT attempt_status FROM releases").Scan(&status)
			if status != nil {
				midway.Store(int64(*status))
			}
		}
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	if got := midway.Load(); got != 503 {
		t.Fatalf("while the second attempt is in flight the row holds the first's status: %d", got)
	}
	reqs := platform.requests()
	var body struct {
		SentAt string `json:"sent_at"`
	}
	if len(reqs) != 2 || json.Unmarshal(reqs[1].Body, &body) != nil {
		t.Fatalf("two deliveries: %d", len(reqs))
	}
	var at time.Time
	var status int
	if err := p.QueryRow(ctx, "SELECT attempted_at, attempt_status FROM releases WHERE run_id = $1", run.ID).Scan(&at, &status); err != nil ||
		status != 202 || at.UTC().Format(time.RFC3339) != body.SentAt {
		t.Fatalf("the row ends on the last attempt: %s %d, its sent_at %s (%v)", at, status, body.SentAt, err)
	}
}

// An answer whose code is no HTTP status, 600 and up, is no answer: 0, inside webhook_ping's range and attempt_status's
// CHECK (#2439), so a receiver's odd code never fails the write or the shape. Every real status stands as it came.
func TestAnAnswerOutsideTheHTTPStatusesIsNoAnswer(t *testing.T) {
	for code, want := range map[int]int{600: 0, 799: 0, 599: 599, 202: 202} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		got, err := post(t.Context(), s.URL, []byte("a key"), []byte(`{"event":"ping"}`), "ping", "d-1")
		s.Close()
		if err != nil || got != want {
			t.Errorf("a %d answered %d (%v), want %d", code, got, err, want)
		}
	}
}

// A receiver verifying the signature refuses a tampered body: the real delivery verifies under the
// stored secret, and a single byte changed in the body it received no longer does.
func TestAReceiverVerifyingTheSignatureRefusesATamperedBody(t *testing.T) {
	const secret = "wh_org_pipeline_secret_2" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, secret, platform)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) != 1 {
		t.Fatalf("one delivery, got %d", len(reqs))
	}
	r := reqs[0]
	if !webhook.Verify([]byte(secret), r.Body, r.Signature) {
		t.Fatal("the real delivery must verify under the stored secret")
	}
	tampered := append([]byte{}, r.Body...)
	tampered[0] ^= 0xff
	if webhook.Verify([]byte(secret), tampered, r.Signature) {
		t.Fatal("a tampered body must not verify")
	}
}

// A 4xx fails the run at once with the status, refuses the release and does not retry.
func TestAReleaseRefusedWithA4xxFailsTheRunAtOnce(t *testing.T) {
	const secret = "wh_org_pipeline_secret_3" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 403)
	p, ctx := releasing(t, secret, platform)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	if state != "failed" || reason == nil || *reason != "error" {
		t.Fatalf("a 4xx fails the run at once: %s %v", state, reason)
	}
	if len(platform.requests()) != 1 {
		t.Fatalf("a 4xx is never retried: %d request(s)", len(platform.requests()))
	}
	lines := buildLogLines(t, ctx, p, run.ID)
	if last := lines[len(lines)-1]; last != "Your platform refused the release: HTTP 403" {
		t.Fatalf("the last build.log line names the status: %q", last)
	}
	var relState string
	if err := p.QueryRow(ctx, "SELECT state FROM releases WHERE run_id = $1", run.ID).Scan(&relState); err != nil || relState != "refused" {
		t.Fatalf("the release row is refused: %q (%v)", relState, err)
	}
}

// The release body carries no secret value, only the runtime variables' names.
func TestAReleaseBodyCarriesNoSecretValueOnlyNames(t *testing.T) {
	const secret = "wh_org_pipeline_secret_4"                      //nolint:gosec // a test fixture, not a real secret
	const stripeKey = "sk_live_51NotARealStripeKeyFixtureValue123" //nolint:gosec // a fixture value, never sent anywhere real // gitleaks:allow
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, secret, platform)
	keyRef, sealedKey, sealedValue, err := pipe.Vault.Seal(project, "preview", "STRIPE_SECRET_KEY", []byte(stripeKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject) VALUES ($1, 'preview', 'STRIPE_SECRET_KEY', 'runtime', $2, $3, $4, 'person', 'ana')",
		project, keyRef, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) == 0 {
		t.Fatal("no delivery received")
	}
	raw := reqs[0].Body
	if strings.Contains(string(raw), stripeKey) {
		t.Fatalf("the release body must never carry a secret value: %s", raw)
	}
	var body struct {
		Variables []string `json:"variables"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Variables) != 1 || body.Variables[0] != "STRIPE_SECRET_KEY" {
		t.Fatalf("the release names the variable: %v", body.Variables)
	}
}

// The release body — and the DBOS journal that carries the "release body" step's output — must
// never carry the front door's client secret: unlike RunApp, which needs it live in the pod,
// sendRelease calls frontDoor(ctx, false), since a release travels over the wire to the org and is
// journaled as a step's return value (security review of #492).
func TestAReleaseNeverCarriesTheFrontDoorsClientSecret(t *testing.T) {
	const clientSecret = "oidc-client-secret-not-real" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_5", platform) //nolint:gosec // a test fixture, not a real secret
	onKubernetes(t)
	version, sealedKey, sealedValue, err := pipe.Vault.Seal("front-door", "signin", "client_secret", []byte(clientSecret))
	if err != nil {
		t.Fatal(err)
	}
	door := map[string]any{"choice": 2, "signin": map[string]any{"issuer": "https://login.meridian.example", "client_id": "canyon-apps"}, "domain": "canyon.meridian.example", "form": "app.env.domain"}
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
		VALUES (now(), now(), $1, $2, $3, $4, 'person', 'dana', 'Dana')`, door, version, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) == 0 {
		t.Fatal("no delivery received")
	}
	for _, r := range reqs {
		if strings.Contains(string(r.Body), clientSecret) || strings.Contains(string(r.Body), "client_secret") {
			t.Fatalf("the delivered body must never carry the front door's client secret: %s", r.Body)
		}
	}
	if n := carried(t, ctx, p, run.ID, clientSecret); n != 0 {
		t.Fatalf("the secret value must not be in the DBOS journal: %d hit(s)", n)
	}
	if n := carried(t, ctx, p, run.ID, "client_secret"); n != 0 {
		t.Fatalf("the key client_secret must not be in the DBOS journal: %d hit(s)", n)
	}
}

// A redirect is never followed: the delivery's own client refuses it (security review of #492),
// so every retry reaches only the release's own target, never wherever a 3xx points.
func TestADeliveryNeverFollowsARedirect(t *testing.T) {
	platform := newStubPlatform(t, 200)                          // never reached: its own row is repointed at the redirector below
	p, ctx := releasing(t, "wh_org_pipeline_secret_6", platform) //nolint:gosec // a test fixture, not a real secret
	var hits int32
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "http://example.invalid/elsewhere", http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	if _, err := p.Exec(ctx, "UPDATE platform_webhook SET url = $1", redirecting.URL); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	if reqs := platform.requests(); len(reqs) != 0 {
		t.Fatalf("the redirect's target must never be reached: %d request(s)", len(reqs))
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("the delivery retries against its own target, never the redirect: %d hit(s)", got)
	}
	var sawStatus302 bool
	for _, l := range buildLogLines(t, ctx, p, run.ID) {
		if strings.Contains(l, "answered 302") {
			sawStatus302 = true
		}
	}
	if !sawStatus302 {
		t.Fatal("the retrying-in row names the redirect's own status")
	}
}

// A delivery goes to the address the admin named, a private one too (design/HAND-OFF.md §3: "No address
// filtering"): where the server may reach is the operator's NetworkPolicy's to say (deploy/k8s/README.md),
// so a pipeline inside the org's network — httptest's own 127.0.0.1 here, dialed for real — is reached.
func TestADeliveryReachesAPrivateAddress(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_7", platform) //nolint:gosec // a test fixture, not a real secret
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	if reqs := platform.requests(); len(reqs) != 1 {
		t.Fatalf("127.0.0.1 is the admin's address and is reached: %d request(s)", len(reqs))
	}
}

// Every build.log row a release send writes is its own DBOS step (security review of #492): a
// crash-replay finds it in the journal instead of re-appending it.
func TestEveryReleaseBuildLogRowIsItsOwnStep(t *testing.T) {
	platform := newStubPlatform(t, 500, 500, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_8", platform) //nolint:gosec // a test fixture, not a real secret
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	var mine int
	for _, l := range buildLogLines(t, ctx, p, run.ID) {
		if strings.Contains(l, "platform") {
			mine++
		}
	}
	var steps int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM "+durableSchema+".operation_outputs WHERE workflow_uuid = $1 AND function_name = 'log'", run.ID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if steps == 0 || steps != mine {
		t.Fatalf("one log step per build.log row this send wrote: %d step(s), %d row(s)", steps, mine)
	}
}

// The hour bound is exact: an attempt whose retry would land past releaseCeiling is never
// scheduled (contract: "for one hour in all" — not one attempt over). releasing's own timing is
// overridden again, after it, so it takes effect for this test alone. The rule is checked on the
// clock the send loop decides on, each attempt's sent_at (whole seconds, release.schema.json's ts):
// on it the rule holds exactly, where the whole-second difference of the first and last sent_at
// against the ceiling failed a correct schedule at random (canyonv2-gvzv).
func TestARetryNeverOvershootsTheCeiling(t *testing.T) {
	const ceiling, interval = 2500 * time.Millisecond, time.Second
	platform := newStubPlatform(t, 500, 500, 500, 500, 500, 500, 500, 500) // never accepts
	p, ctx := releasing(t, "wh_org_pipeline_secret_9", platform)           //nolint:gosec // a test fixture, not a real secret
	wasFastRetry, wasFastWindow, wasCeiling := releaseFastRetry, releaseFastWindow, releaseCeiling
	releaseFastRetry, releaseFastWindow, releaseCeiling = interval, time.Hour, ceiling
	t.Cleanup(func() { releaseFastRetry, releaseFastWindow, releaseCeiling = wasFastRetry, wasFastWindow, wasCeiling })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) < 2 {
		t.Fatalf("need at least two attempts to compare the first and the last: %d", len(reqs))
	}
	sentAtOf := func(r received) time.Time {
		var body struct {
			SentAt string `json:"sent_at"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339, body.SentAt)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	first := sentAtOf(reqs[0])
	// "for one hour in all" (design/HAND-OFF.md §3): every attempt but the last scheduled its retry
	// within the ceiling, unlike the pre-fix ordering, which slept before checking and so retried an
	// attempt whose retry landed past it; and the last stopped because its retry would have.
	for n, r := range reqs {
		elapsed := sentAtOf(r).Sub(first)
		if last := n == len(reqs)-1; !last && elapsed+interval > ceiling {
			t.Fatalf("attempt %d, %s after the first, was retried: its retry lands past the %s ceiling", n+1, elapsed, ceiling)
		} else if last && elapsed+interval <= ceiling {
			t.Fatalf("the retries stopped at attempt %d, %s after the first, with a retry still inside the %s ceiling", n+1, elapsed, ceiling)
		}
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM releases").Scan(&state); err != nil || state != "undelivered" {
		t.Fatalf("undelivered: %q (%v)", state, err)
	}
}

// A release sent after a change of access_grace carries the new value, and one sent before does
// not: the sidecar's environment is built as the release body is, from the row as it then stands.
func TestAReleaseSentAfterAGraceChangeCarriesTheNewOne(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_6", platform) //nolint:gosec // a test fixture, not a real secret
	deploy := func() {
		t.Helper()
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
		if err != nil {
			t.Fatal(err)
		}
		waitEnded(t, ctx, p, run.ID)
	}
	deploy()
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET access_grace = '2h'"); err != nil {
		t.Fatal(err)
	}
	deploy()
	reqs := platform.requests()
	if len(reqs) != 2 {
		t.Fatalf("one delivery per release, got %d", len(reqs))
	}
	if before, after := graceOf(t, reqs[0]), graceOf(t, reqs[1]); before != "1h" || after != "2h" {
		t.Fatalf("the release sent before the change carries %q, the one after %q", before, after)
	}
}

// graceOf is the delivery's CANYON_ACCESS_GRACE, the sidecar's environment as the release carries it.
func graceOf(t *testing.T, r received) string {
	t.Helper()
	var body struct {
		Sidecar struct {
			Env map[string]string `json:"env"`
		} `json:"sidecar"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Sidecar.Env["CANYON_ACCESS_GRACE"]
}

// A retry keeps the value the release was sent with: the grace changes between a delivery the
// platform answered 500 and its retry, and both deliveries carry the old one — the body is the
// "release body" step's journaled output, built once.
func TestARetryOfAReleaseSentBeforeAGraceChangeKeepsItsValue(t *testing.T) {
	platform := newStubPlatform(t, 500, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_7", platform) //nolint:gosec // a test fixture, not a real secret
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(platform.requests()) == 0; { // the first delivery, answered 500; its retry is releaseFastRetry away
		if time.Now().After(deadline) {
			t.Fatal("no first delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := p.Exec(ctx, "UPDATE org_preferences SET access_grace = '2h'"); err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) != 2 {
		t.Fatalf("a delivery and its retry, got %d", len(reqs))
	}
	if first, retry := graceOf(t, reqs[0]), graceOf(t, reqs[1]); first != "1h" || retry != "1h" {
		t.Fatalf("the delivery carries %q, its retry %q: both are the value the release was sent with", first, retry)
	}
}

// reportOn commits what the report route commits for a report on id (cmd/canyon-server,
// reportRelease — package main, out of this package's reach): the row's new state, with failed its
// reason, and with deployed the run's deploy.completed row under the dedupe the route writes it with.
func reportOn(ctx context.Context, p *pgxpool.Pool, id, status, detail string) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var run, env, image string
	if err := tx.QueryRow(ctx, "UPDATE releases SET state = $2, reason = CASE WHEN $2 = 'failed' THEN $3 END WHERE id = $1 RETURNING run_id, env, image", id, status, detail).Scan(&run, &env, &image); err != nil {
		return err
	}
	if status == "deployed" {
		_, digest, _ := strings.Cut(image, "@")
		if _, err := events.AppendRunTx(ctx, tx, run, project, events.DeployCompleted, "deploy:"+env, map[string]any{"env": env, "url": detail, "package": pkg, "image_digest": digest}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// waitingOn starts a deploy under run_by: org, answered 200, with a bound of ceiling, and answers the
// run and its release once the release is accepted: the run is waiting on its report. The bound is set
// before the run starts, which reads it as it waits (#2261); a few seconds is long enough to report inside
// it, short enough that a run the report never ends fails the test quickly.
func waitingOn(t *testing.T, ceiling time.Duration) (p *pgxpool.Pool, ctx context.Context, run, release string) {
	t.Helper()
	p, ctx = releasing(t, "wh_org_pipeline_secret_10", newStubPlatform(t, 200)) //nolint:gosec // a test fixture, not a real secret
	wasCeiling := reportCeiling
	reportCeiling = ceiling
	t.Cleanup(func() { reportCeiling = wasCeiling })
	started, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		var state string
		err := p.QueryRow(ctx, "SELECT id, state FROM releases WHERE run_id = $1", started.ID).Scan(&release, &state)
		if err == nil && state == "accepted" {
			return p, ctx, started.ID, release
		}
		if time.Now().After(deadline) {
			t.Fatalf("the release was never accepted: %q (%v)", state, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A deployed report arriving mid-wait ends the run at once, clean, its last build.log line
// events.md's "live at <url> · reported by your platform" (canyonv2-ula5.57.1).
func TestADeployedReportMidWaitEndsTheRunLive(t *testing.T) {
	p, ctx, run, release := waitingOn(t, 5*time.Second)
	if err := reportOn(ctx, p, release, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "clean" {
		t.Fatalf("a deployed report ends the run clean: %s (%s)", state, why(reason))
	}
	lines := buildLogLines(t, ctx, p, run)
	if last := lines[len(lines)-1]; last != "live at https://claims.apps.meridian.example · reported by your platform" {
		t.Fatalf("the last build.log line: %q", last)
	}
	if got := accessRows(t, ctx, p, run); !slices.Equal(got, []string{"live at https://claims.apps.meridian.example · reported by your platform", // the report's own, written with deploy.completed
		"Private: only its owners and admins can open it. Share it: canyon share <email> · canyon share everyone",
		"No description yet — add description = \"…\" to canyon.toml so colleagues see what it does."}) { // the org's platform runs it, and the run still says who can open it (contracts/events.md, build.log)
		t.Fatalf("the access after the report's deploy.completed row: %q", got)
	}
}

// A failed report arriving mid-wait ends the run failed at once, its last build.log line
// events.md's "Your platform reported: <reason>" with the reason the platform gave.
func TestAFailedReportMidWaitEndsTheRunFailedWithItsReason(t *testing.T) {
	p, ctx, run, release := waitingOn(t, 5*time.Second)
	if err := reportOn(ctx, p, release, "failed", "ImagePullBackOff: pull access denied for canyon/claims-portal"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "error" {
		t.Fatalf("a failed report ends the run failed/error: %s (%s)", state, why(reason))
	}
	lines := buildLogLines(t, ctx, p, run)
	if last := lines[len(lines)-1]; last != "Your platform reported: ImagePullBackOff: pull access denied for canyon/claims-portal" {
		t.Fatalf("the last build.log line: %q", last)
	}
}

// No report ends the run at the bound, as before: failed/error, a "still waiting" row on the way,
// and the contract's own sentence last, the release still open.
func TestNoReportEndsTheRunAtTheBound(t *testing.T) {
	p, ctx := releasing(t, "wh_org_pipeline_secret_11", newStubPlatform(t, 200)) //nolint:gosec // a test fixture, not a real secret
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" {
		t.Fatalf("no report fails the run at the bound: %s (%s)", state, why(reason))
	}
	lines := buildLogLines(t, ctx, p, run.ID)
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "still waiting on your platform (") }) {
		t.Fatalf("a still-waiting row on the way: %v", lines)
	}
	if last := lines[len(lines)-1]; last != "Your platform has not reported on "+strings.TrimPrefix(pkg, "pkg_")+" in 30 minutes. The release stays open: when it reports, the environment goes live." {
		t.Fatalf("the last build.log line: %q", last)
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM releases WHERE run_id = $1", run.ID).Scan(&state); err != nil || state != "accepted" {
		t.Fatalf("the release stays open: %q (%v)", state, err)
	}
}

// A platform that reports inside its webhook handler, before it answers 2xx, still ends the run on
// that report: Canyon marking the release accepted after the 2xx never overwrites a report that
// landed first.
func TestAReportBeforeTheDeliveryIsAnsweredStillEndsTheRun(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_12", platform) //nolint:gosec // a test fixture, not a real secret
	wasCeiling := reportCeiling
	reportCeiling = 5 * time.Second
	t.Cleanup(func() { reportCeiling = wasCeiling })
	platform.onDelivery = func(body []byte) {
		var d struct {
			Release string `json:"release"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			t.Error(err)
			return
		}
		if err := reportOn(ctx, p, d.Release, "deployed", "https://claims.apps.meridian.example"); err != nil {
			t.Error(err)
		}
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the report that landed before the 2xx ends the run clean: %s (%s)", state, why(reason))
	}
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM releases WHERE run_id = $1", run.ID).Scan(&state); err != nil || state != "deployed" {
		t.Fatalf("the release keeps the report's state: %q (%v)", state, err)
	}
	var handedFirst bool // the timeline says it was handed over before the platform's own report
	if err := p.QueryRow(ctx, `SELECT (SELECT min(id) FROM timeline_rows WHERE run_id = $1 AND data->>'line' = 'handed to your platform · waiting for it to report')
		< (SELECT min(id) FROM timeline_rows WHERE run_id = $1 AND kind = 'deploy.completed')`, run.ID).Scan(&handedFirst); err != nil || !handedFirst {
		t.Fatalf("handed to your platform comes before the report it waits for: %v", err)
	}
}

// A removed report arriving mid-wait ends the run at once, failed: the release never goes live (a
// deployed after it is 409 superseded), so the run says so rather than waiting out the bound
// (contracts/events.md, canyonv2-ula5.57.3).
func TestARemovedReportMidWaitEndsTheRunFailed(t *testing.T) {
	p, ctx, run, release := waitingOn(t, 5*time.Second)
	if err := reportOn(ctx, p, release, "removed", ""); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run); state != "failed" || why(reason) != "error" {
		t.Fatalf("a removed report ends the run failed/error: %s (%s)", state, why(reason))
	}
	lines := buildLogLines(t, ctx, p, run)
	if last := lines[len(lines)-1]; last != "Your platform removed the release before it went live" {
		t.Fatalf("the last build.log line: %q", last)
	}
}

// A remove release is sent as release.schema.json's remove — event, delivery, release, app,
// environment, report_url, sent_at and nothing else — retried as a deploy's is under one delivery id,
// signed, and marked accepted on the 2xx; asked again while it is open, the same release, nothing sent.
func TestARemoveReleaseIsSentRetriedAndAnsweredOnce(t *testing.T) {
	const secret = "wh_org_pipeline_secret_13" //nolint:gosec // a test fixture, not a real secret
	platform := newStubPlatform(t, 500, 200)
	p, ctx := releasing(t, secret, platform)
	for _, q := range []string{
		"INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_0rg1', '" + project + "', '" + pkg + "', 'clean')",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ('" + project + "', 'preview', '" + pkg + "', 'registry.example/canyon/app@sha256:" + strings.Repeat("c", 64) + "', 'https://claims.apps.meridian.example', 'gr_0rg1', 'org')",
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	send := func() (release string, sent bool) {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if release, sent, err = SendRemoveTx(ctx, tx, durable, project, "preview"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return release, sent
	}
	release, sent := send()
	if !sent {
		t.Fatal("the first undeploy sends a remove release")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var state string
		if err := p.QueryRow(ctx, "SELECT state FROM releases WHERE id = $1", release).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remove release was never accepted: %s", state)
		}
	}
	reqs := platform.requests()
	if len(reqs) != 2 || reqs[0].Delivery != reqs[1].Delivery || reqs[0].Delivery == "" {
		t.Fatalf("a 500 then a 200: two attempts under one delivery id, got %+v", reqs)
	}
	for _, r := range reqs {
		var body map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		keys := slices.Sorted(maps.Keys(body))
		if !slices.Equal(keys, []string{"app", "delivery", "environment", "event", "release", "report_url", "sent_at"}) || body["event"] != "remove" || body["release"] != release ||
			body["report_url"] != pipe.PublicURL+"/releases/"+release+"/report" || !webhook.Verify([]byte(secret), r.Body, r.Signature) {
			t.Fatalf("the remove body: %s", r.Body)
		}
	}
	if again, sent := send(); again != release || sent {
		t.Fatalf("asked again while it is open: %s, sent %v", again, sent)
	}
}

// A promotion under run_by: org sends a prod release in place of /run — preview's image, the gate run
// preview was deployed from, recorded with preview's deployer — and waits on the report like a deploy
// (design/HAND-OFF.md §3): a deployed report ends it clean, live at the address reported.
func TestAPromotionUnderOrgSendsAProdReleaseAndWaits(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_14", platform) //nolint:gosec // a test fixture, not a real secret
	wasCeiling := reportCeiling
	reportCeiling = 5 * time.Second
	t.Cleanup(func() { reportCeiling = wasCeiling })
	image := "registry.example/canyon/app@sha256:" + strings.Repeat("d", 64)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_prev01', $1, $2, 'clean')", project, pkg); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: "gr_prev01", DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	var release, env, gateRun, deployedBy string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var state string
		err := p.QueryRow(ctx, "SELECT id, env, state, gate_run, deployed_by_subject FROM releases WHERE run_id = $1", run.ID).Scan(&release, &env, &state, &gateRun, &deployedBy)
		if err == nil && state == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prod release was never accepted: %q (%v)", state, err)
		}
	}
	if env != "prod" || gateRun != "gr_prev01" || deployedBy != dana.Subject {
		t.Fatalf("the prod release: env %q, gate run %q, deployed by %q", env, gateRun, deployedBy)
	}
	var body struct {
		Environment string `json:"environment"`
		GateRun     string `json:"gate_run"`
		Image       string `json:"image"`
	}
	if reqs := platform.requests(); len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Environment != "prod" || body.GateRun != "gr_prev01" || body.Image != image {
		t.Fatalf("the delivered prod release: %+v", body)
	}
	if err := reportOn(ctx, p, release, "deployed", "https://claims.apps.meridian.example"); err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("a deployed report ends the promotion clean: %s (%s)", state, why(reason))
	}
	lines := buildLogLines(t, ctx, p, run.ID)
	if last := lines[len(lines)-1]; last != "live at https://claims.apps.meridian.example · reported by your platform" {
		t.Fatalf("the last build.log line: %q", last)
	}
}

// arm64 is the executor answering every image's platform as linux/arm64, in place of releasing's: an image built for
// arm64, whatever this machine runs on.
func arm64(t *testing.T) *executortest.Fake {
	t.Helper()
	f := executortest.New(t)
	f.Platform("arm64")
	pipe.Executor = &executor.Client{Base: f.URL}
	return f
}

// releasePlatform is the platform a delivered release names, nil when it names none.
func releasePlatform(t *testing.T, raw []byte) (image string, platform *struct{ OS, Architecture string }) {
	t.Helper()
	var body struct {
		Image    string `json:"image"`
		Platform *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"platform"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Platform == nil {
		return body.Image, nil
	}
	return body.Image, &struct{ OS, Architecture string }{body.Platform.OS, body.Platform.Architecture}
}

// A hand-off release names the platform its image runs on (#2528), read by the release's image digest from the
// executor as a /run reads it: an arm64 image's deploy release says linux/arm64, once read and on every attempt,
// so the org's platform schedules the app on arm64 nodes.
func TestADeployReleaseNamesItsImagesPlatform(t *testing.T) {
	platform := newStubPlatform(t, 503, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_15", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID)
	reqs := platform.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d deliveries, want the 503's and its retry", len(reqs))
	}
	for _, r := range reqs {
		if image, got := releasePlatform(t, r.Body); got == nil || got.OS != "linux" || got.Architecture != "arm64" || !slices.Equal(f.Platformed(), []string{image}) {
			t.Fatalf("the release names platform %+v, the executor was asked of %v, want linux/arm64 read once of %s", got, f.Platformed(), image)
		}
	}
}

// A promotion's release names its image's platform too (#2528): preview's image, read again by digest.
func TestAPromotionReleaseNamesItsImagesPlatform(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_16", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	image := "registry.example/canyon/app@sha256:" + strings.Repeat("d", 64)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_prev02', $1, $2, 'clean')", project, pkg); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: "gr_prev02", DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, ctx, p, run.ID) // no report: it ends at reportCeiling, before the next test sets the knobs it reads
	if len(platform.requests()) == 0 {
		t.Fatal("no prod release was delivered")
	}
	if got, platformOf := releasePlatform(t, platform.requests()[0].Body); platformOf == nil || platformOf.OS != "linux" || platformOf.Architecture != "arm64" || got != image || !slices.Equal(f.Platformed(), []string{image}) {
		t.Fatalf("the prod release of %s names %+v, the executor asked of %v", got, platformOf, f.Platformed())
	}
}

// An image whose platform cannot be read is never handed off (#2528): the run fails before any release is
// written or sent, as a /run of it would run nowhere.
func TestAReleaseOfAnImageWhosePlatformCannotBeReadIsNeverSent(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_17", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	f.FailPlatform.Store(true)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" {
		t.Fatalf("a release whose image's platform is unread: %s %s", state, why(reason))
	}
	var releases int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM releases WHERE run_id = $1", run.ID).Scan(&releases); err != nil || releases != 0 || len(platform.requests()) != 0 {
		t.Fatalf("%d release row(s) (%v), %d delivery(ies): want none", releases, err, len(platform.requests()))
	}
}

// A release row journaled before releases named a platform (#2528's architecture review, B1): its run, in flight
// across the upgrade, reaches the body with no platform read. The body reads it then, by the image's digest, and
// fails closed as the release step does: never a body this release builds without one.
func TestAReleaseBodyReadsThePlatformItsRowNeverRead(t *testing.T) {
	platform := newStubPlatform(t, 200)
	_, ctx := releasing(t, "wh_org_pipeline_secret_18", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	image := "registry.example/canyon/app@sha256:" + strings.Repeat("e", 64)
	in, row := Input{Run: "gr_body01", Project: project, Package: pkg}, releaseRow{ID: "rel_00000000000000b1", Delivery: "5b0c7a4e-4c35-4a7e-9a3c-1b2e3f405162"}
	body, err := pipe.releaseBody(ctx, in, row, "preview", image)
	if err != nil || body.Platform == nil || body.Platform.OS != "linux" || body.Platform.Architecture != "arm64" || !slices.Equal(f.Platformed(), []string{image}) {
		t.Fatalf("a body whose row read no platform: %+v (%v), the executor asked of %v", body.Platform, err, f.Platformed())
	}
	f.FailPlatform.Store(true)
	if body, err := pipe.releaseBody(ctx, in, row, "preview", image); err == nil {
		t.Fatalf("a body whose image's platform cannot be read: %+v, want an error", body.Platform)
	}
}

// A rollback under run_by: org whose kept image the registry no longer holds (GET /platform's image_gone) fails as
// on Canyon's own cluster (#2528): no release, the image no longer kept, the last line saying why, production as it
// was.
func TestARollbackUnderOrgWhoseKeptImageIsGoneSaysSo(t *testing.T) {
	fastReleaseTiming(t)
	p, ctx, f := deploying(t, runstest.CleanGolden)
	a, _ := inProduction(t, ctx, p)
	review, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, RollbackTo: digestOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := waitEnded(t, ctx, p, review.ID); state != "clean" {
		t.Fatalf("the review: %s", state)
	}
	platform := newStubPlatform(t, 200)
	handOrg(t, ctx, p, "wh_org_pipeline_secret_19", platform) //nolint:gosec // a test fixture, not a real secret
	f.GonePlatform.Store(true)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: a, GateRun: review.ID, Rollback: true}})
	if err != nil {
		t.Fatal(err)
	}
	state, reason := waitEnded(t, ctx, p, run.ID)
	var kept bool
	var releases int
	if err := p.QueryRow(ctx, "SELECT bool_or(kept), (SELECT count(*) FROM releases WHERE run_id = $2) FROM deploy_history WHERE image = $1", a, run.ID).Scan(&kept, &releases); err != nil ||
		state != "failed" || reasonOf(reason) != "error" || kept || releases != 0 || len(platform.requests()) != 0 {
		t.Fatalf("the rollback: %s %s, still kept %v, %d release(s), %d delivery(ies) (%v)", state, reasonOf(reason), kept, releases, len(platform.requests()), err)
	}
	if line := lastLine(t, ctx, p, run.ID); line != "7d3e8f's kept image is gone, so it can't be put back as it ran." {
		t.Fatalf("the last line: %q", line)
	}
}

// A promotion under run_by: org whose image's platform cannot be read is never handed off, and says why on Canyon's own
// cluster's line (#2534's review, C2): the run's last build.log row, so the developer and the CLI see the reason.
func TestAPromotionWhosePlatformCannotBeReadSaysWhy(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_20", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	f.FailPlatform.Store(true)
	image := "registry.example/canyon/app@sha256:" + strings.Repeat("f", 64)
	if _, err := p.Exec(ctx, "INSERT INTO runs (id, project_id, package_id, state) VALUES ('gr_prev03', $1, $2, 'clean')", project, pkg); err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Actor: dana, Promote: &Promotion{Image: image, GateRun: "gr_prev03", DeployedBy: dana}})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || len(platform.requests()) != 0 {
		t.Fatalf("the promotion: %s %s, %d delivery(ies)", state, why(reason), len(platform.requests()))
	}
	line := lastLine(t, ctx, p, run.ID)
	if !strings.HasPrefix(line, "deploying to production: failed: ") || !strings.HasSuffix(line, "; the version running there is unchanged") ||
		!strings.Contains(line, "'s architecture could not be read, so Canyon runs it nowhere: ") {
		t.Fatalf("the last build.log line: %q", line)
	}
}

// A deploy run's preview release never sent says why, as production's does, with no tail: a first preview has nothing
// running (#2539). The reason is the executor's own words, never the server's wording around them.
func TestAPreviewReleaseNeverSentSaysWhy(t *testing.T) {
	platform := newStubPlatform(t, 200)
	p, ctx := releasing(t, "wh_org_pipeline_secret_21", platform) //nolint:gosec // a test fixture, not a real secret
	f := arm64(t)
	f.FailPlatform.Store(true)
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "failed" || why(reason) != "error" || len(platform.requests()) != 0 {
		t.Fatalf("the deploy: %s %s, %d delivery(ies)", state, why(reason), len(platform.requests()))
	}
	line := lastLine(t, ctx, p, run.ID)
	if !strings.HasPrefix(line, "deploying to preview: failed: the image ") ||
		!strings.HasSuffix(line, "'s architecture could not be read, so Canyon runs it nowhere: image index") {
		t.Fatalf("the last build.log line: %q", line)
	}
}
