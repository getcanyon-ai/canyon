package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const preferencesAudit = "SELECT count(*) FROM audit_events WHERE action = 'PUT /api/v1/preferences'"

// withWebhookRow stores a platform webhook directly, as its own tests store the rows they read
// through: the PUT that mints its secret and token is routes_platform_webhook_test.go's.
func withWebhookRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	sum := sha256.Sum256([]byte("host-token-fixture-not-real"))
	if _, err := pool.Exec(ctx, "INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display) VALUES (1, 'https://deploy.meridian.example/hooks', 1, '', '', $1, 'person', 'dana', 'Dana')", sum[:]); err != nil {
		t.Fatal(err)
	}
}

// withDoorRow stores the front door at choice 1 (the fixture's gateway) or at choice 2, whose
// sealed client secret the refusal never opens: the PUT and its probe are routes_front_door_test.go's.
func withDoorRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, choice int) {
	t.Helper()
	door := `{` + doorOIDC + `,"domain":"canyon.meridian.example","form":"app.env.domain"}`
	q := "INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'dana', 'Dana')"
	if choice == 2 {
		door = `{"choice":2,"signin":{"issuer":"https://login.meridian.example","client_id":"canyon-apps"},"domain":"canyon.meridian.example","form":"app.env.domain"}`
		q = "INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 1, '', '', 'person', 'dana', 'Dana')"
	}
	if _, err := pool.Exec(ctx, q, door); err != nil {
		t.Fatal(err)
	}
}

// servesHandOff gives the server under test, once started, the two settings run_by: org needs of the
// server itself (#2010): the sidecar's image and an https public URL. start resets the URL, so call it after.
func servesHandOff(t *testing.T) {
	t.Helper()
	pipe.IdentityImage, pipe.PublicURL = "registry.meridian.example/canyon-identity@sha256:"+strings.Repeat("b", 64), "https://canyon.meridian.example"
	t.Cleanup(func() { pipe.IdentityImage, pipe.PublicURL = "", "http://localhost:7777" })
}

// run_by: org is refused in the contract's order, each refusal before anything is written: a docker
// host, then no sidecar image, then a public URL that is not https (#2010), then no platform webhook,
// then no front door, then a front door at choice 2 — and taken once all six hold. A member is
// refused before any of that; a value outside canyon | org is the body's bound, not a hand-off
// refusal; and going back to canyon needs nothing.
func TestRunByOrgIsRefusedInOrderAndWritesNothing(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	org := func(base string, c *http.Client) (int, map[string]any) {
		return putPreferences(t, c, base, `{"run_by":"org"}`)
	}
	refused := func(base string, c *http.Client, e, message string) {
		t.Helper()
		code, body := org(base, c)
		if code != 409 || body["error"] != e || (message != "" && body["message"] != message) {
			t.Fatalf("%s: %d %s", e, code, lastBody)
		}
		if got, rows := getPreferences(t, c, base)["run_by"], count(t, ctx, pool, preferencesAudit); got != "canyon" || rows != 0 {
			t.Fatalf("a refusal wrote something: run_by %v, %d audit row(s)", got, rows)
		}
	}
	docker, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, docker, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, docker, "sub-carol", "carol@meridian.example", false)
	if code, _ := org(docker, carol); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member: %d %s", code, lastBody)
	}
	refused(docker, dana, "not_kubernetes", onDocker)

	onKubernetes(t)
	kube, _ := start(t, pool, time.Hour)
	dana = signedIn(t, ctx, pool, kube, "dana@meridian.example", "dana@meridian.example", false)
	refused(kube, dana, "identity_image_required", identityImageRequired)
	pipe.IdentityImage = "registry.meridian.example/canyon-identity@sha256:" + strings.Repeat("b", 64)
	t.Cleanup(func() { pipe.IdentityImage = "" })
	refused(kube, dana, "https_required", httpsRequired)
	servesHandOff(t)
	refused(kube, dana, "webhook_required", "")
	withWebhookRow(t, ctx, pool)
	refused(kube, dana, "front_door_required", "")
	withDoorRow(t, ctx, pool, 2)
	refused(kube, dana, "signin_unsupported", "Your platform signs people in: set the front door to choice 1 first.")
	if _, err := pool.Exec(ctx, "DELETE FROM front_doors"); err != nil {
		t.Fatal(err)
	}
	withDoorRow(t, ctx, pool, 1)
	if code, body := org(kube, dana); code != 200 || body["run_by"] != "org" {
		t.Fatalf("org once all six hold: %d %s", code, lastBody)
	}
	if got, rows := getPreferences(t, dana, kube)["run_by"], count(t, ctx, pool, preferencesAudit); got != "org" || rows != 1 {
		t.Fatalf("the read after: run_by %v, %d audit row(s)", got, rows)
	}
	for _, bad := range []string{`"elsewhere"`, `null`, `1`} {
		if code, body := putPreferences(t, dana, kube, `{"run_by":`+bad+`}`); code != 400 || body["error"] != "invalid_body" {
			t.Fatalf("run_by %s: %d %s", bad, code, lastBody)
		}
	}
	if code, body := putPreferences(t, dana, kube, `{"run_by":"canyon"}`); code != 200 || body["run_by"] != "canyon" {
		t.Fatalf("back to canyon: %d %s", code, lastBody)
	}
}

// access_grace is on every read, 1h by default; outside its shape or its range it is its own 400
// and the row is untouched; within them the PUT answers it and every later read carries it.
func TestAccessGraceIsBoundedAndOnEveryRead(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if read := getPreferences(t, c, base); read["run_by"] != "canyon" || read["access_grace"] != "1h" {
		t.Fatalf("the defaults: %s", lastBody)
	}
	for _, bad := range []string{`"25h"`, `"24h1s"`, `"-1h"`, `""`, `"1x"`, `"1.5h"`, `"30"`, `"1h0s0"`, `3600`, `null`} {
		if code, body := putPreferences(t, c, base, `{"access_grace":`+bad+`}`); code != 400 || body["error"] != "invalid_access_grace" {
			t.Fatalf("access_grace %s: %d %s", bad, code, lastBody)
		}
	}
	if got, rows := getPreferences(t, c, base)["access_grace"], count(t, ctx, pool, preferencesAudit); got != "1h" || rows != 0 {
		t.Fatalf("a refusal wrote something: access_grace %v, %d audit row(s)", got, rows)
	}
	for _, good := range []string{"0", "24h", "1h30m", "90s"} {
		if code, body := putPreferences(t, c, base, `{"access_grace":"`+good+`"}`); code != 200 || body["access_grace"] != good {
			t.Fatalf("access_grace %s: %d %s", good, code, lastBody)
		}
		if got := getPreferences(t, c, base)["access_grace"]; got != good {
			t.Fatalf("the read after %s: %v", good, got)
		}
	}
}

// The audit row carries the two keys with their before and after, and a PUT that repeats what is
// stored writes no row.
func TestTheAuditRowCarriesRunByAndAccessGraceBeforeAndAfter(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	withWebhookRow(t, ctx, pool)
	withDoorRow(t, ctx, pool, 1)
	base, _ := start(t, pool, time.Hour)
	servesHandOff(t)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	const put = `{"run_by":"org","access_grace":"2h"}`
	if code, _ := putPreferences(t, dana, base, put); code != 200 {
		t.Fatalf("the PUT: %d %s", code, lastBody)
	}
	var detail map[string]any
	if err := pool.QueryRow(ctx, "SELECT detail FROM audit_events WHERE action = 'PUT /api/v1/preferences' ORDER BY ts DESC, id DESC LIMIT 1").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if got := string(mustJSON(detail)); got != `{"access_grace":{"after":"2h","before":"1h"},"run_by":{"after":"org","before":"canyon"}}` {
		t.Fatalf("the before and the after: %s", got)
	}
	if code, _ := putPreferences(t, dana, base, put); code != 200 {
		t.Fatalf("the same PUT again: %d %s", code, lastBody)
	}
	if rows := count(t, ctx, pool, preferencesAudit); rows != 1 {
		t.Fatalf("a PUT that changed nothing wrote a row: %d", rows)
	}
}

// The front door's read names what this server lacks to serve run_by: org, in the PUT's order, so
// Settings › App hosting can say why before anyone tries (#2010); a Docker host lacks nothing it could use.
func TestTheFrontDoorReadNamesWhatHandOffNeedsOfTheServer(t *testing.T) {
	pool, _ := migrated(t)
	docker, _ := start(t, pool, time.Hour)
	needs := func(base string) any {
		t.Helper()
		_, body, _ := do(t, client(t), http.MethodGet, base+doorPath, "")
		return body["hand_off_needs"]
	}
	if got := needs(docker); fmt.Sprint(got) != "[]" {
		t.Fatalf("a Docker host: %s", lastBody)
	}
	onKubernetes(t)
	kube, _ := start(t, pool, time.Hour)
	want := []any{map[string]any{"error": "identity_image_required", "message": identityImageRequired}, map[string]any{"error": "https_required", "message": httpsRequired}}
	if got := needs(kube); !reflect.DeepEqual(got, want) {
		t.Fatalf("a Kubernetes host lacking both: %s", lastBody)
	}
	servesHandOff(t)
	if got := needs(kube); fmt.Sprint(got) != "[]" {
		t.Fatalf("a Kubernetes host lacking neither: %s", lastBody)
	}
}

// A server whose row already says run_by: org refuses to start while it lacks a setting hand-off needs,
// naming each, as every release it sent would break (#2010); under canyon, or lacking nothing, it starts.
func TestAHandedOffServerRefusesToStartWithoutWhatHandOffNeeds(t *testing.T) {
	pool, ctx := migrated(t)
	lacking := &runs.Pipeline{PublicURL: "http://canyon.meridian.example"}
	if err := refuseHandOffUnserved(ctx, pool, lacking); err != nil {
		t.Fatalf("under canyon: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET run_by = 'org'"); err != nil {
		t.Fatal(err)
	}
	if err := refuseHandOffUnserved(ctx, pool, lacking); err == nil || !strings.Contains(err.Error(), identityImageRequired) || !strings.Contains(err.Error(), httpsRequired) {
		t.Fatalf("under org, lacking both: %v", err)
	}
	served := &runs.Pipeline{PublicURL: "https://canyon.meridian.example", IdentityImage: "registry.meridian.example/canyon-identity@sha256:" + strings.Repeat("b", 64)}
	if err := refuseHandOffUnserved(ctx, pool, served); err != nil {
		t.Fatalf("under org, lacking nothing: %v", err)
	}
}
