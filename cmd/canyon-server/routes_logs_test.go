package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
)

const (
	logsRoute   = "GET /api/v1/projects/{id}/logs"
	logsAudited = "SELECT count(*) FROM audit_events WHERE action = '" + logsRoute + "'"
)

// stream opens the log read without draining it: the caller reads what it needs, then cancels.
// The cancel is also a cleanup, so a failed assertion mid-stream releases the connection instead
// of leaving the server's handler, and the fake behind it, waiting on a client that never goes.
func stream(t *testing.T, c *http.Client, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, cancel
}

func TestLogsStreamThePreviewContainerRawAndAuditTheReader(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	logs := base + "/api/v1/projects/claims-portal/logs"
	// the live tail: a line reaches the client while the stream is still open, and the
	// disclosure header travelled ahead of it
	fake.Follow.Store(true)
	resp, cancel := stream(t, c, logs)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("Canyon-Log-Redaction") != "none" {
		t.Fatalf("a served read: %d %v", resp.StatusCode, resp.Header)
	}
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != "GET /login 200 12ms\n" {
		t.Fatalf("the first line arrives before the stream ends: %q (%v)", line, err)
	}
	cancel()
	_ = resp.Body.Close()
	if got := fake.Tailed(); got.Tail != logsTail || !got.Follow { // omitted: 200 lines, following
		t.Fatalf("the executor was handed %+v", got)
	}
	// the whole tail, as the container wrote it; the executor is handed the path's project, the
	// env, the lines and follow=false, and stray query values change none of that; the fake still
	// holds a following stream open, so this body ends because it was asked not to follow
	code, _, resp := do(t, c, http.MethodGet, logs+"?env=preview&app=other-app&tail=5&lines=30&follow=false", "")
	if code != 200 || resp.Header.Get("Canyon-Log-Redaction") != "none" || lastBody != "GET /login 200 12ms\nGET /api/claims 200 31ms" {
		t.Fatalf("the tail as written: %d %q", code, lastBody)
	}
	if got := fake.Tailed(); got.App != "claims-portal" || got.Target != "preview" || got.Tail != 30 || got.Follow {
		t.Fatalf("the executor was handed %+v", got)
	}
	// one row per served read: the reader and the env, never a line
	var rows, details string
	if err := pool.QueryRow(ctx, "SELECT string_agg(project_id || ' ' || actor_subject, ','), string_agg(detail::text, ',') FROM audit_events WHERE action = $1", logsRoute).Scan(&rows, &details); err != nil ||
		rows != "claims-portal anonymous,claims-portal anonymous" || details != `{"env": "preview", "redaction": "none"},{"env": "preview", "redaction": "none"}` {
		t.Fatalf("two served reads, two rows naming the env: %q %q (%v)", rows, details, err)
	}
	// prod has nothing deployed: a 404 that reaches neither the executor nor the trail
	if code, body, _ := do(t, c, http.MethodGet, logs+"?env=prod", ""); code != 404 || body["error"] != "not_found" || fake.Logs.Load() != 2 || count(t, ctx, pool, logsAudited) != 2 {
		t.Fatalf("nothing deployed in prod: %d %s, %d executor calls", code, lastBody, fake.Logs.Load())
	}
}

func TestLogsRefuseTheEnvAndTheProjectBeforeTheExecutor(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	for _, tc := range []struct {
		path string
		code int
		err  string
	}{
		{"/claims-portal/logs?env=stage", 400, "invalid_env"},
		{"/claims-portal/logs?lines=0", 400, "invalid_lines"},
		{"/claims-portal/logs?lines=10001", 400, "invalid_lines"},
		{"/claims-portal/logs?lines=many", 400, "invalid_lines"},
		{"/claims-portal/logs?follow=yes", 400, "invalid_follow"},
		{"/nothing/logs", 404, "not_found"},
		{"/claims-portal/logs", 404, "not_found"}, // a project with nothing deployed
	} {
		if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects"+tc.path, ""); code != tc.code || body["error"] != tc.err {
			t.Fatalf("%s: %d %s", tc.path, code, lastBody)
		}
	}
	if fake.Logs.Load() != 0 || count(t, ctx, pool, logsAudited) != 0 {
		t.Fatalf("a refused read reached the executor (%d) or the trail (%d)", fake.Logs.Load(), count(t, ctx, pool, logsAudited))
	}
}

func TestLogsOfADeletedProjectAre404EvenWithItsContainerRunning(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	if code, _, _ := do(t, c, http.MethodDelete, base+"/api/v1/projects/claims-portal?confirm=claims-portal", ""); code != 204 {
		t.Fatalf("delete: %d %s", code, lastBody)
	}
	if code, body, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/logs", ""); code != 404 || body["error"] != "not_found" || fake.Logs.Load() != 0 {
		t.Fatalf("after the delete: %d %s, %d executor calls", code, lastBody, fake.Logs.Load())
	}
}

func TestLogsAnswer502WhenTheContainerIsGoneAnd500WithoutAnExecutor(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	logs := base + "/api/v1/projects/claims-portal/logs"
	fake.NoContainer.Store(true)
	if code, body, _ := do(t, c, http.MethodGet, logs, ""); code != 502 || body["error"] != "app_unreachable" || fake.Logs.Load() != 1 || count(t, ctx, pool, logsAudited) != 0 {
		t.Fatalf("the record names a container the executor no longer has: %d %s (%d rows)", code, lastBody, count(t, ctx, pool, logsAudited))
	}
	fake.NoContainer.Store(false)
	fake.FailLogs.Store(true) // any other executor failure: the fixed 500, the detail in the log only, no row
	t.Cleanup(func() { fake.FailLogs.Store(false) })
	if code, body, _ := do(t, c, http.MethodGet, logs, ""); code != 500 || body["error"] != "internal" || body["message"] != "internal error" || count(t, ctx, pool, logsAudited) != 0 {
		t.Fatalf("an executor failure that is not a missing container: %d %s (%d rows)", code, lastBody, count(t, ctx, pool, logsAudited))
	}
	fake.FailLogs.Store(false)
	executor := pipe.Executor
	pipe.Executor = nil
	defer func() { pipe.Executor = executor }()
	if code, body, _ := do(t, c, http.MethodGet, logs, ""); code != 500 || body["error"] != "internal" {
		t.Fatalf("no executor: %d %s", code, lastBody)
	}
}

// A read whose audit row cannot be written is not served: the fixed 500, no disclosure header,
// not a byte of the log. The trigger is the one TestAFailedAuditWriteFailsTheRequest uses.
func TestLogsAreNotServedWhenTheAuditRowCannotBeWritten(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit write refused'; END $$;
		CREATE TRIGGER refuse_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP TRIGGER refuse_audit ON audit_events; DROP FUNCTION refuse_audit()"); err != nil {
			t.Error(err)
		}
	})
	code, body, resp := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/logs", "")
	if code != 500 || body["error"] != "internal" || resp.Header.Get("Canyon-Log-Redaction") != "" || strings.Contains(lastBody, "GET /login") {
		t.Fatalf("a read whose audit row failed: %d %v %q", code, resp.Header, lastBody)
	}
	if fake.Logs.Load() != 1 || count(t, ctx, pool, logsAudited) != 0 {
		t.Fatalf("the executor was asked %d times, %d rows", fake.Logs.Load(), count(t, ctx, pool, logsAudited))
	}
}

// A container that is up and has written nothing still answers the status and the disclosure
// at once: the headers are flushed before the first byte, so a client with a start deadline
// does not give up on a quiet app.
func TestLogsAnswerHeadersForASilentContainer(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	fake.Silent.Store(true)
	t.Cleanup(func() { fake.Silent.Store(false) })
	got := make(chan *http.Response, 1)
	go func() {
		resp, _ := stream(t, c, base+"/api/v1/projects/claims-portal/logs")
		got <- resp
	}()
	select {
	case resp := <-got:
		if resp.StatusCode != 200 || resp.Header.Get("Canyon-Log-Redaction") != "none" {
			t.Fatalf("a silent container: %d %v", resp.StatusCode, resp.Header)
		}
	case <-time.After(5 * time.Second): // a hang guard; the fake holds the stream open, so a buffered header never arrives at all
		t.Fatal("no headers from a running container that wrote nothing")
	}
}

// A quiet app is not a dead stream (#1443): through every stretch of silence the server writes the
// contract's keepalive line, so a proxy's idle timeout never ends the read.
func TestLogsKeepASilentStreamAlive(t *testing.T) {
	_, _, base, c, _ := previewed(t)
	was := logsKeepalive
	logsKeepalive = 50 * time.Millisecond
	t.Cleanup(func() { logsKeepalive = was })
	fake.Follow.Store(true)
	resp, _ := stream(t, c, base+"/api/v1/projects/claims-portal/logs")
	r := bufio.NewReader(resp.Body)
	for _, want := range []string{"GET /login 200 12ms\n", "GET /api/claims 200 31ms\n", "canyon: keepalive\n", "canyon: keepalive\n"} {
		if line, err := r.ReadString('\n'); err != nil || line != want {
			t.Fatalf("read %q (%v), want %q", line, err, want)
		}
	}
}

// follow never puts the keepalive inside a line the app has not finished: it waits for the newline.
func TestTheKeepaliveWaitsForTheLineToEnd(t *testing.T) {
	pr, pw := io.Pipe()
	var got syncBuffer
	done := make(chan error, 1)
	go func() { done <- follow(&got, func() error { return nil }, pr, 20*time.Millisecond) }()
	_, _ = pw.Write([]byte("half a line"))
	time.Sleep(100 * time.Millisecond) // five quiet stretches inside the line: no keepalive may land
	_, _ = pw.Write([]byte(" and its end\n"))
	time.Sleep(60 * time.Millisecond) // quiet after the line: keepalives now
	_ = pw.Close()
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatalf("follow ended with %v, want EOF", err)
	}
	out := got.String()
	if !strings.HasPrefix(out, "half a line and its end\ncanyon: keepalive\n") || strings.Count(out, "half a line") != 1 {
		t.Fatalf("follow wrote %q", out)
	}
}

// The dashboard's read, redaction=masked (#2744): the last lines masked with the env's runtime values by the
// status read's rule, a sealed value never in the answer, never followed, its header saying which redaction
// it applied, audited as a raw read is. A secret changed since the app started is 409 and no line; an
// unknown redaction, or one that would follow, is 400 before the executor is asked: never a raw read.
func TestTheDashboardsLogReadIsMasked(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	logs := base + "/api/v1/projects/claims-portal/logs"
	// a secret whose value the fake's log prints, set before the run that read it began
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"/api/claims"}`)
	if _, err := pool.Exec(ctx, "UPDATE deploys SET run_started_at = now(), deployed_at = now() WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	fake.Follow.Store(true) // the executor would follow: the masked read ends anyway
	code, _, resp := do(t, c, http.MethodGet, logs+"?redaction=masked", "")
	if code != 200 || resp.Header.Get("Canyon-Log-Redaction") != "masked" || lastBody != "GET /login 200 12ms\nGET <API_TOKEN> 200 31ms" { // the helper trims the body, as the raw read's test reads it
		t.Fatalf("the masked read: %d %v %q", code, resp.Header, lastBody)
	}
	if got := fake.Tailed(); got.Tail != logsTail || got.Follow {
		t.Fatalf("the executor was handed %+v, want the last %d lines and no follow", got, logsTail)
	}
	if n := count(t, ctx, pool, logsAudited); n != 1 {
		t.Fatalf("a masked read leaves the raw read's row: %d rows", n)
	}
	do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_TOKEN", `{"value":"tok-new"}`)
	if code, body, _ := do(t, c, http.MethodGet, logs+"?redaction=masked&follow=false", ""); code != 409 || body["error"] != "not_maskable" || strings.Contains(lastBody, "/api/claims") || count(t, ctx, pool, logsAudited) != 1 {
		t.Fatalf("a secret set since the app started: %d %s, want 409 and no row", code, lastBody)
	}
	asked := fake.Logs.Load()
	for query, want := range map[string]string{"?redaction=raw": "invalid_redaction", "?redaction=MASKED": "invalid_redaction", "?redaction=masked&follow=true": "invalid_follow"} {
		if code, body, _ := do(t, c, http.MethodGet, logs+query, ""); code != 400 || body["error"] != want {
			t.Errorf("%s: %d %s, want 400 %s", query, code, lastBody, want)
		}
	}
	if fake.Logs.Load() != asked || count(t, ctx, pool, logsAudited) != 1 {
		t.Fatalf("a refused redaction reached the executor or the trail")
	}
}

// The masked read bounds what it holds before it masks (#2744's review): at most 200 lines, refused before anything
// is read, and at most 1 MiB, refused whole as 422 too_long_to_mask, never cut, since a cut can split a secret's value
// and leave its tail in the clear. Neither reaches the database: no row, no mask.
func TestTheMaskedReadIsBoundedBeforeItMasks(t *testing.T) {
	a := &app{log: log.New(io.Discard, "", 0)} // no pool, no pipe: a read that got further would panic
	for _, q := range []string{"lines=201&redaction=masked", "lines=10000&redaction=masked"} {
		w := httptest.NewRecorder()
		a.projectLogs(w, httptest.NewRequest(http.MethodGet, "/api/v1/projects/claims-portal/logs?"+q, nil))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_lines"`) {
			t.Errorf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	a.maskedLogs(w, httptest.NewRequest(http.MethodGet, "/api/v1/projects/claims-portal/logs?redaction=masked", nil), "claims-portal", "preview",
		strings.NewReader(strings.Repeat("x", 1<<20+1)))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"too_long_to_mask"`) || w.Header().Get("Canyon-Log-Redaction") != "" {
		t.Errorf("a log over 1 MiB: %d %s", w.Code, w.Body)
	}
}

// A security responder reads production's logs, masked, whatever their access to the app, each read audited (design/PERMISSIONS.md,
// Security responders; the owner, 2026-11-25): never preview's, never a raw read, decided on the read as the server parses it,
// and never where a value is short enough that its name gives it away (#223's security review). A member below contributor
// who isn't a responder reads none.
func TestAResponderReadsProductionsMaskedLogsAlone(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET responder_emails = $1", []string{"rex@meridian.example"}); err != nil {
		t.Fatal(err)
	}
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	// a build value the fake's log prints (GET /api/claims): preview built the image production runs, so it is masked there too
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/CLAIMS_PATH", `{"value":"/api/claims","where":"build"}`); code != 204 {
		t.Fatalf("the build value: %d %s", code, lastBody)
	}
	// the run that built the image both environments run, after that value was set
	image := "canyon/claims-portal@" + executortest.Digest
	if _, err := pool.Exec(ctx, "UPDATE runs SET build_image = $1, created_at = now() WHERE id = 'gr_seed'", image); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_started_at) SELECT 'claims-portal', e, package_id, $1, 'http://x', 'gr_seed', now() + interval '1 second' FROM runs, unnest(ARRAY['preview', 'prod']) e WHERE id = 'gr_seed'`, image); err != nil {
		t.Fatal(err)
	}
	started := func() { // what runs started after every value set so far: the masked read's rule can mask it
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE deploys SET run_started_at = now() + interval '1 second' WHERE project_id = 'claims-portal'"); err != nil {
			t.Fatal(err)
		}
	}
	rex := signedIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example", false)
	nora := signedIn(t, ctx, pool, base, "sub-nora", "nora@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	logs := base + "/api/v1/projects/claims-portal/logs"
	reads := 0
	read := func(who string, c *http.Client, query string) {
		t.Helper()
		code, _, resp := do(t, c, http.MethodGet, logs+query, "")
		if code != 200 || resp.Header.Get("Canyon-Log-Redaction") != "masked" || strings.Contains(lastBody, "/api/claims") || !strings.Contains(lastBody, "<CLAIMS_PATH>") {
			t.Fatalf("%s, %s: %d %q, want 200 masked, the build value by its name", who, query, code, lastBody)
		}
		reads++
	}
	read("a responder", rex, "?env=prod&redaction=masked")
	var row string
	if err := pool.QueryRow(ctx, "SELECT project_id || ' ' || actor_subject || ' ' || detail::text FROM audit_events WHERE action = $1", logsRoute).Scan(&row); err != nil ||
		row != `claims-portal sub-rex {"env": "prod", "redaction": "masked"}` {
		t.Fatalf("the responder's read leaves its row: %q (%v)", row, err)
	}
	refused := func(who string, c *http.Client, query string) {
		t.Helper()
		asked := fake.Logs.Load()
		if code, body, _ := do(t, c, http.MethodGet, logs+query, ""); code != 403 || body["error"] != "team_required" {
			t.Errorf("%s, %s: %d %s, want 403 team_required", who, query, code, lastBody)
		}
		if fake.Logs.Load() != asked {
			t.Errorf("%s, %s: a refused read reached the executor", who, query)
		}
	}
	for _, query := range []string{
		"?env=preview&redaction=masked", "?env=prod", "?env=prod&redaction=none&follow=false",
		"?redaction=masked",                         // no env: preview's
		"?env=preview&env=prod&redaction=masked",    // the first env is the read's
		"?env=prod&redaction=none&redaction=masked", // the first redaction is the read's
		"?env=prod;x=1&redaction=masked",            // a pair with a semicolon is no pair: no env
		"?env=nope",                                 // a read the parse refuses is the team's refusal, never a 400
		"?env=prod&redaction=masked&follow=true",
	} {
		refused("a responder", rex, query)
	}
	refused("a member with no access", nora, "?env=prod&redaction=masked")
	refused("a read-only member", vic, "?env=prod&redaction=masked")
	// a responder who is also read-only on the app reads as a responder
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, granted_by_kind, granted_by_subject, granted_by_display) VALUES ('claims-portal', 'email', 'rex@meridian.example', 'viewer', 'person', 'sub-olive', '')`); err != nil {
		t.Fatal(err)
	}
	read("a read-only responder", rex, "?env=prod&redaction=masked")
	refused("a read-only responder", rex, "?env=prod&follow=false")
	// a value under 8 characters: the name in its place would give it away, so the responder reads none; the team reads as before
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/PIN", `{"value":"4321","env":"prod"}`); code != 204 {
		t.Fatalf("the short value: %d %s", code, lastBody)
	}
	started()
	if code, body, _ := do(t, rex, http.MethodGet, logs+"?env=prod&redaction=masked", ""); code != 409 || body["error"] != "too_short_to_mask" {
		t.Errorf("a responder's read with a short value: %d %s, want 409 too_short_to_mask", code, lastBody)
	}
	read("a contributor, with a short value", carl, "?env=prod&redaction=masked")
	// a build value rotated after the image was built, then promoted: the image holds the old one, which the vault no
	// longer has, so nobody's masked read can hide it
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/CLAIMS_PATH", `{"value":"/api/claims/v2","where":"build"}`); code != 204 {
		t.Fatalf("the rotated build value: %d %s", code, lastBody)
	}
	started()
	for who, c := range map[string]*http.Client{"a contributor": carl, "a responder": rex} {
		if code, body, _ := do(t, c, http.MethodGet, logs+"?env=prod&redaction=masked", ""); code != 409 || body["error"] != "not_maskable" {
			t.Errorf("%s, a build value rotated since the build: %d %s, want 409 not_maskable", who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, logsAudited); n != reads {
		t.Fatalf("%d rows for %d reads served", n, reads)
	}
	// a deleted app is 404 to a responder, before anything is read
	if _, err := pool.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'claims-portal'"); err != nil { // as delete leaves it
		t.Fatal(err)
	}
	if code, body, _ := do(t, rex, http.MethodGet, logs+"?env=prod&redaction=masked", ""); code != 404 || body["error"] != "not_found" || count(t, ctx, pool, logsAudited) != reads {
		t.Fatalf("a responder on a deleted app: %d %s, want 404 and no row", code, lastBody)
	}
}
