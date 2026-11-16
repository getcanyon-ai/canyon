package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/webhook"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// pinged is one request the org's platform saw: its headers and its raw body.
type pinged struct {
	Event, Delivery, Signature string
	Body                       []byte
}

// platformAt is the org's platform for a test delivery: it answers code and keeps every request; with hold set it
// answers only once hold is closed, a platform that never answers in time.
func platformAt(t *testing.T, code int, hold chan struct{}) (*httptest.Server, func() []pinged) {
	t.Helper()
	var mu sync.Mutex
	var seen []pinged
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, pinged{r.Header.Get("Canyon-Event"), r.Header.Get("Canyon-Delivery"), r.Header.Get("Canyon-Signature-256"), body})
		mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s, func() []pinged { mu.Lock(); defer mu.Unlock(); return slices.Clone(seen) }
}

// webhookSavedAt saves the platform webhook through the PUT, as an admin does, and points it at url: the route takes
// https alone, and the platform a test stands up is http. It answers the secret Canyon minted.
func webhookSavedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, c *http.Client, base, url string) string {
	t.Helper()
	code, saved, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`)
	secret, _ := saved["secret"].(string)
	if code != 200 || secret == "" {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE platform_webhook SET url = $1", url); err != nil {
		t.Fatal(err)
	}
	return secret
}

// keysOf is a JSON object's keys, sorted.
func keysOf(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

// A test delivery is release.schema.json's ping, signed under the stored secret with Canyon-Event: ping, posted to the
// saved url alone, never one the request names; it answers the status the platform gave and writes nothing — no
// release, no audit row, never the webhook's last delivery — and one log line with the status. A member is refused
// before anything is sent, and before any webhook is saved there is nothing to ping (#2439).
func TestATestDeliveryIsTheSchemasPingSignedToTheSavedURLAlone(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if code, body, _ := do(t, dana, http.MethodPost, base+webhookPath+"/ping", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("a ping before any webhook: %d %s", code, lastBody)
	}
	platform, seen := platformAt(t, http.StatusTeapot, nil)
	elsewhere, sentElsewhere := platformAt(t, http.StatusOK, nil)
	secret := webhookSavedAt(t, ctx, pool, dana, base, platform.URL)
	if code, _, _ := do(t, carol, http.MethodPost, base+webhookPath+"/ping", ""); code != 403 || lastBody != adminRequired || len(seen()) != 0 {
		t.Fatalf("a member's ping: %d %s, %d sent", code, lastBody, len(seen()))
	}
	audits := count(t, ctx, pool, "SELECT count(*) FROM audit_events")
	code, answer, _ := do(t, dana, http.MethodPost, base+webhookPath+"/ping", `{"url":"`+elsewhere.URL+`"}`)
	if code != 200 || !reflect.DeepEqual(keysOf(answer), []string{"delivery", "sent_at", "status"}) || answer["status"] != float64(418) {
		t.Fatalf("the ping: %d %s", code, lastBody)
	}
	got := seen()
	if len(got) != 1 || len(sentElsewhere()) != 0 {
		t.Fatalf("one delivery, to the saved url alone: %d there, %d elsewhere", len(got), len(sentElsewhere()))
	}
	p := got[0]
	if p.Event != "ping" || p.Delivery != answer["delivery"] || !webhook.Verify([]byte(secret), p.Body, p.Signature) {
		t.Fatalf("Canyon-Event ping, the answer's delivery, signed under the stored secret: %+v", p)
	}
	schema, err := jsonschema.NewCompiler().Compile("../../contracts/schema/release.schema.json#/properties/ping")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(p.Body))
	if err == nil {
		err = schema.Validate(doc)
	}
	var body map[string]any
	if err != nil || json.Unmarshal(p.Body, &body) != nil || body["delivery"] != answer["delivery"] || body["sent_at"] != answer["sent_at"] {
		t.Fatalf("the body is the contract's ping, the answer's delivery and sent_at: %v\n%s", err, p.Body)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events"); n != audits {
		t.Fatalf("a ping writes no audit row: %d, was %d", n, audits)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM releases"); n != 0 {
		t.Fatalf("a ping is never a release: %d row(s)", n)
	}
	if code, get, _ := do(t, carol, http.MethodGet, base+webhookPath, ""); code != 200 || get["last_delivery"] != nil {
		t.Fatalf("a ping is never the last delivery: %d %s", code, lastBody)
	}
	if !strings.Contains(logs.String(), "a test delivery to the platform webhook answered 418") {
		t.Fatalf("one log line, the status: %s", logs.String())
	}
}

// A ping that reaches nothing answers exactly {delivery, sent_at, status: 0}, and neither the answer nor the log says
// why: a dial error names the network behind the url (#2439, the security review).
func TestATestDeliveryThatReachesNothingAnswersZeroAndNeverWhy(t *testing.T) {
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	c := client(t)
	gone, _ := platformAt(t, http.StatusOK, nil)
	webhookSavedAt(t, ctx, pool, c, base, gone.URL)
	gone.Close()
	code, answer, _ := do(t, c, http.MethodPost, base+webhookPath+"/ping", "")
	if code != 200 || !reflect.DeepEqual(keysOf(answer), []string{"delivery", "sent_at", "status"}) || answer["status"] != float64(0) {
		t.Fatalf("a ping to a closed listener: %d %s", code, lastBody)
	}
	var said []string // the server's lines about the delivery; the rest of the log names its own listener
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "test delivery") {
			said = append(said, strings.TrimSpace(line))
		}
	}
	if len(said) != 1 || !strings.HasSuffix(said[0], "canyon-server: a test delivery to the platform webhook answered 0") {
		t.Fatalf("one log line, the status alone: %q", said)
	}
	for _, why := range []string{"refused", "dial", "127.0.0.1"} {
		if strings.Contains(lastBody, why) {
			t.Fatalf("%q is in the answer: %s", why, lastBody)
		}
	}
}

// A secret the vault can't open sends nothing: the ping answers 500, and never posts unsigned (fail closed; #2439,
// the security review).
func TestATestDeliveryWhoseSecretWontOpenSendsNothing(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	platform, seen := platformAt(t, http.StatusOK, nil)
	webhookSavedAt(t, ctx, pool, c, base, platform.URL)
	if _, err := pool.Exec(ctx, "UPDATE platform_webhook SET sealed_value = 'not what the vault sealed'"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+webhookPath+"/ping", ""); code != 500 || body["error"] != "internal" || len(seen()) != 0 {
		t.Fatalf("a ping whose secret won't open: %d %s, %d sent", code, lastBody, len(seen()))
	}
}

// A ping waits on the org's platform outside any transaction: while one hangs, a write elsewhere goes through at once,
// and a second ping is refused 429, one at a time per server (#2439).
func TestATestDeliveryWaitingOnItsPlatformHoldsNoLockAndIsOneAtATime(t *testing.T) {
	withOIDC(t) // the preferences PUT is a signed-in admin's
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	hold := make(chan struct{})
	platform, seen := platformAt(t, http.StatusAccepted, hold)
	answer := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(answer) // before the platform's own Close, which waits on its handler
	webhookSavedAt(t, ctx, pool, c, base, platform.URL)
	first := make(chan string, 1)
	go func() {
		resp, err := c.Post(base+webhookPath+"/ping", "application/json", nil)
		if err != nil {
			first <- err.Error()
			return
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		first <- string(raw)
	}()
	for deadline := time.Now().Add(5 * time.Second); len(seen()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the ping never reached its platform")
		}
	}
	began := time.Now()
	code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/preferences", `{"prod_approval":true}`)
	if code != 200 || time.Since(began) > 2*time.Second {
		t.Fatalf("a write while a ping waits: %d after %s %s", code, time.Since(began), lastBody)
	}
	if code, body, _ := do(t, c, http.MethodPost, base+webhookPath+"/ping", ""); code != 429 || body["error"] != "ping_in_flight" || body["message"] != pingInFlight {
		t.Fatalf("a second ping while one is in flight: %d %s", code, lastBody)
	}
	answer()
	if got := <-first; !strings.Contains(got, `"status":202`) {
		t.Fatalf("the first ping, once answered: %s", got)
	}
	if code, _, _ := do(t, c, http.MethodPost, base+webhookPath+"/ping", ""); code != 200 {
		t.Fatalf("a ping once the first has answered: %d %s", code, lastBody)
	}
}

// The webhook's last delivery is the newest attempt to deliver a release since the webhook was last saved: an attempt
// before that save, and a release not yet attempted, are not it; a save after it leaves none (#2439).
func TestTheLastDeliveryIsTheNewestAttemptSinceTheWebhookWasSaved(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	webhookSavedAt(t, ctx, pool, c, base, "https://deploy.meridian.example/hooks/canyon")
	var setAt time.Time // an hour back, so every attempt below is in the past and a save now comes after them all
	if err := pool.QueryRow(ctx, "UPDATE platform_webhook SET set_at = set_at - interval '1 hour' RETURNING set_at").Scan(&setAt); err != nil {
		t.Fatal(err)
	}
	release := func(id, delivery string, attempted *time.Time, status int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, attempted_at, attempt_status)
			VALUES ($1, 'claims-portal', 'preview', 'run_x', 'pkg_x', 'deploy', 'registry.example/canyon/claims-portal@sha256:aa', $2, 'sent', $3, $4, $5)`,
			id, delivery, setAt.Add(-time.Hour), attempted, status); err != nil {
			t.Fatal(err)
		}
	}
	at := func(d time.Duration) *time.Time { v := setAt.Add(d); return &v }
	release("rel_before", "0b6e7c1a-2d3f-4e5a-9b8c-000000000001", at(-time.Minute), 200)
	release("rel_older", "0b6e7c1a-2d3f-4e5a-9b8c-000000000002", at(time.Minute), 503)
	release("rel_newest", "0b6e7c1a-2d3f-4e5a-9b8c-000000000003", at(2*time.Minute), 202)
	release("rel_unsent", "0b6e7c1a-2d3f-4e5a-9b8c-000000000004", nil, 0)
	_, get, _ := do(t, c, http.MethodGet, base+webhookPath, "")
	want := map[string]any{"delivery": "0b6e7c1a-2d3f-4e5a-9b8c-000000000003", "release": "rel_newest", "sent_at": at(2 * time.Minute).UTC().Format(time.RFC3339), "status": float64(202)}
	if !reflect.DeepEqual(get["last_delivery"], want) {
		t.Fatalf("the newest attempt since the save: %s", lastBody)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/v2"}`); code != 200 {
		t.Fatalf("a new address: %d %s", code, lastBody)
	}
	if _, get, _ := do(t, c, http.MethodGet, base+webhookPath, ""); get["last_delivery"] != nil {
		t.Fatalf("a delivery to the old address says nothing of the new one: %s", lastBody)
	}
}

// Replacing the webhook secret answers a new one once, never cached, seals it in place of the old one — every delivery
// after is signed with it — and leaves the url and who set it; it is an admin's, audited without a value, and 404 before
// any webhook. The first save's and the token rotate's answers, which carry a secret or a token, are never cached either.
func TestTheWebhookSecretIsReplacedOnceAndNeverCached(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	if code, body, _ := do(t, dana, http.MethodPost, base+webhookPath+"/secret", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("a replace before any webhook: %d %s", code, lastBody)
	}
	code, saved, resp := do(t, dana, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks/canyon"}`)
	if code != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the first save, carrying the secret and the token: %d %q", code, resp.Header.Get("Cache-Control"))
	}
	first, _ := saved["secret"].(string)
	if code, _, resp := do(t, dana, http.MethodPost, base+webhookPath+"/token", ""); code != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the token rotate: %d %q", code, resp.Header.Get("Cache-Control"))
	}
	if code, _, _ := do(t, carol, http.MethodPost, base+webhookPath+"/secret", ""); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's replace: %d %s", code, lastBody)
	}
	_, before, _ := do(t, carol, http.MethodGet, base+webhookPath, "")
	var savedAt time.Time
	if err := pool.QueryRow(ctx, "UPDATE platform_webhook SET set_at = set_at - interval '1 minute', set_by_subject = 'someone-earlier' RETURNING set_at").Scan(&savedAt); err != nil {
		t.Fatal(err)
	}
	code, replaced, resp := do(t, dana, http.MethodPost, base+webhookPath+"/secret", "")
	secret, _ := replaced["secret"].(string)
	if code != 200 || !reflect.DeepEqual(keysOf(replaced), []string{"secret"}) || secret == "" || secret == first || !validToken(secret) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the replace: %d %s %q", code, lastBody, resp.Header.Get("Cache-Control"))
	}
	// The url stays; set_by and set_at move, as for any save, so the last delivery starts again (#2439, the architect's m3).
	var movedAt time.Time
	var by string
	if err := pool.QueryRow(ctx, "SELECT set_at, set_by_subject FROM platform_webhook").Scan(&movedAt, &by); err != nil || !movedAt.After(savedAt) || by != "dana@meridian.example" {
		t.Fatalf("the replace is a save: set_at %s, was %s; set_by %q (%v)", movedAt, savedAt, by, err)
	}
	if _, after, _ := do(t, carol, http.MethodGet, base+webhookPath, ""); after["url"] != before["url"] {
		t.Fatalf("the url stays: %v, was %v", after["url"], before["url"])
	}
	platform, seen := platformAt(t, http.StatusOK, nil)
	if _, err := pool.Exec(ctx, "UPDATE platform_webhook SET url = $1", platform.URL); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+webhookPath+"/ping", ""); code != 200 || len(seen()) != 1 || !webhook.Verify([]byte(secret), seen()[0].Body, seen()[0].Signature) {
		t.Fatalf("a delivery after the replace is signed with the new secret: %d %s", code, lastBody)
	}
	var detail []byte
	if err := pool.QueryRow(ctx, "SELECT coalesce(detail::text, '') FROM audit_events WHERE action = 'rotate platform secret'").Scan(&detail); err != nil {
		t.Fatalf("one audit row: %v", err)
	}
	if bytes.Contains(detail, []byte(secret)) || strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), first) {
		t.Fatal("no secret in the audit row or the log")
	}
}
