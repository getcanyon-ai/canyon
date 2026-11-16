package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/webhook"
)

const (
	secret          = "pipeline-test-signing-value"                         //nolint:gosec // a test value, not a credential
	hostToken       = "pipeline-test-host-value"                            //nolint:gosec // a test value, not a credential
	fakeDatabaseURL = "postgres://role:pw@10.0.0.5:5432/db?sslmode=disable" //nolint:gosec // a test value, not a credential
	digest          = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// fakeCluster records what the pipeline asked of the cluster.
type fakeCluster struct {
	mu      sync.Mutex
	ran     []string // "<release> <access token>"
	removed []string
	values  map[string]string // the last run's: the variables the platform built
	fail    error
}

func (f *fakeCluster) run(_ context.Context, r release, token string, values map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, r.Release+" "+token)
	f.values = values
	return "https://" + r.App + "." + r.Environment + ".apps.example", f.fail
}

func (f *fakeCluster) remove(_ context.Context, r release) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, r.Release)
	return f.fail
}

// canyon is a stub Canyon: the access-token route and the report route, under the host token.
type canyon struct {
	mu      sync.Mutex
	reports []map[string]any
	*httptest.Server
}

func newCanyon(t *testing.T) *canyon {
	c := &canyon{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+hostToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/releases/rel_8f2a/access-token":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "CANYON_ACCESS_TOKEN", "value": "the-release-token"})
		case r.Method == http.MethodPost && r.URL.Path == "/releases/rel_8f2a/report":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.mu.Lock()
			c.reports = append(c.reports, body)
			c.mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.Close)
	return c
}

func newPipeline(f *fakeCluster, now time.Time) *pipeline {
	return &pipeline{secret: []byte(secret), hostToken: hostToken, cluster: f, client: http.DefaultClient, now: func() time.Time { return now }, seen: map[string]time.Time{}}
}

// fakePlatform records what the pipeline asked of the org's Postgres.
type fakePlatform struct {
	mu       sync.Mutex
	provided []string // "<app> <env> <name> <type>"
	dropped  []string // "<app> <env>"
}

func (f *fakePlatform) provide(_ context.Context, app, env string, res resource) (map[string]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provided = append(f.provided, app+" "+env+" "+res.Name+" "+res.Type)
	if res.Type != "postgres" {
		return nil, "", errors.New("this platform does not provide " + res.Type + " resources")
	}
	values := map[string]string{}
	for _, g := range res.Gives {
		values[g] = fakeDatabaseURL
	}
	return values, "postgres://10.0.0.5:5432/" + res.Name, nil
}

func (f *fakePlatform) drop(_ context.Context, app, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, app+" "+env)
	return nil
}

// deliver posts one release to the pipeline, signed with sign, and answers the status; extra adds to
// or replaces the body's fields.
func deliver(t *testing.T, p *pipeline, c *canyon, event, delivery string, sentAt time.Time, sign func([]byte) string, extra ...map[string]any) int {
	t.Helper()
	fields := map[string]any{"event": event, "delivery": delivery, "release": "rel_8f2a", "app": "claims-portal", "environment": "preview",
		"image": "registry.example/canyon/claims-portal@" + digest, "port": 3000, "sidecar": map[string]any{"image": "registry.example/canyon-identity@" + digest, "port": 4180, "env": map[string]string{}, "secret": "CANYON_ACCESS_TOKEN"},
		"report_url": c.URL + "/releases/rel_8f2a/report", "sent_at": sentAt.UTC().Format(time.RFC3339)}
	for _, e := range extra {
		maps.Copy(fields, e)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Canyon-Signature-256", sign(body))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	p.done.Wait()
	return w.Code
}

func signed(body []byte) string { return webhook.Sign([]byte(secret), body) }

// A signed deploy runs the app with the release's own access token and reports it deployed at the
// address it runs at, with the image's digest; the same delivery again is done, not run twice.
func TestADeployRunsWithTheReleasesTokenAndReportsItDeployed(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p := newPipeline(f, now)
	for range 2 {
		if code := deliver(t, p, c, "deploy", "d-1", now, signed); code != http.StatusAccepted {
			t.Fatalf("a signed deploy: %d", code)
		}
	}
	if len(f.ran) != 1 || f.ran[0] != "rel_8f2a the-release-token" {
		t.Fatalf("run once with the release's token: %v", f.ran)
	}
	if len(c.reports) != 1 || c.reports[0]["status"] != "deployed" || c.reports[0]["digest"] != digest || c.reports[0]["address"] != "https://claims-portal.preview.apps.example" {
		t.Fatalf("the report: %v", c.reports)
	}
}

// A body whose signature does not verify, or a sent_at past 30 minutes, is refused and runs nothing.
func TestATamperedOrStaleReleaseIsRefused(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p := newPipeline(f, now)
	if code := deliver(t, p, c, "deploy", "d-2", now, func([]byte) string { return signed([]byte("another body")) }); code != http.StatusUnauthorized {
		t.Fatalf("a tampered release: %d", code)
	}
	if code := deliver(t, p, c, "deploy", "d-3", now.Add(-31*time.Minute), signed); code != http.StatusBadRequest {
		t.Fatalf("a stale release: %d", code)
	}
	if code := deliver(t, p, c, "deploy", "d-5", now.Add(31*time.Minute), signed); code != http.StatusBadRequest {
		t.Fatalf("a release stamped ahead: %d", code)
	}
	if len(f.ran) != 0 || len(c.reports) != 0 {
		t.Fatalf("nothing runs and nothing is reported: %v %v", f.ran, c.reports)
	}
}

// A test delivery (#2439) is verified like any delivery — a bad signature is 401, so the ping catches a wrong secret —
// and checked for freshness; a good one is answered 2xx and runs and reports nothing.
func TestAPingIsVerifiedAnsweredAndRunsNothing(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p := newPipeline(f, now)
	ping := func(sentAt time.Time, sign func([]byte) string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"event": "ping", "delivery": "p-1", "sent_at": sentAt.UTC().Format(time.RFC3339)})
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.Header.Set("Canyon-Event", "ping")
		req.Header.Set("Canyon-Signature-256", sign(body))
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		p.done.Wait()
		return w.Code
	}
	if code := ping(now, func(body []byte) string { return webhook.Sign([]byte("another secret"), body) }); code != http.StatusUnauthorized {
		t.Fatalf("a ping signed under another secret: %d", code)
	}
	if code := ping(now.Add(-31*time.Minute), signed); code != http.StatusBadRequest {
		t.Fatalf("a stale ping: %d", code)
	}
	if code := ping(now, signed); code != http.StatusAccepted {
		t.Fatalf("a ping: %d", code)
	}
	if len(f.ran)+len(f.removed)+len(c.reports) != 0 {
		t.Fatalf("a ping runs and reports nothing: ran %v, removed %v, reports %v", f.ran, f.removed, c.reports)
	}
}

// A remove removes and reports removed; a cluster that fails reports failed, its reason one line.
func TestARemoveReportsRemovedAndAFailureReportsItsReason(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p := newPipeline(f, now)
	deliver(t, p, c, "remove", "r-1", now, signed)
	f.fail = errors.New("ImagePullBackOff:\nnot found")
	deliver(t, p, c, "deploy", "d-4", now, signed)
	f.fail = errors.New(strings.Repeat("é", 2000))
	deliver(t, p, c, "deploy", "d-6", now, signed)
	if len(f.removed) != 1 || len(c.reports) != 3 || c.reports[0]["status"] != "removed" || c.reports[1]["status"] != "failed" || c.reports[1]["reason"] != "ImagePullBackOff: not found" {
		t.Fatalf("the reports: %v", c.reports)
	}
	if reason, _ := c.reports[2]["reason"].(string); utf8.RuneCountInString(reason) != 1024 {
		t.Fatalf("a long reason is cut to the 1024 code points the report takes: %d", utf8.RuneCountInString(reason))
	}
}

var postgresDB = map[string]any{"resources": []map[string]any{{"name": "db", "type": "postgres", "gives": []string{"DATABASE_URL"}}}}

// A deploy naming a postgres resource has the platform provide it, the app run with the variable it
// gives, and the report name where it is — never the credential the app was handed.
func TestADeploysResourcesAreProvidedAndReportedByEndpoint(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p, pg := newPipeline(f, now), &fakePlatform{}
	p.platform = pg
	deliver(t, p, c, "deploy", "d-7", now, signed, postgresDB)
	if len(pg.provided) != 1 || pg.provided[0] != "claims-portal preview db postgres" {
		t.Fatalf("provided: %v", pg.provided)
	}
	if f.values["DATABASE_URL"] != fakeDatabaseURL {
		t.Fatalf("the app runs with what the resource gives: %v", f.values)
	}
	raw, _ := json.Marshal(c.reports)
	if len(c.reports) != 1 || c.reports[0]["status"] != "deployed" || !strings.Contains(string(raw), `"resources":[{"endpoint":"postgres://10.0.0.5:5432/db","name":"db"}]`) || strings.Contains(string(raw), "pw@") {
		t.Fatalf("the report names the endpoint and no credential: %s", raw)
	}
}

// A resource the platform cannot provide fails the release, its reason said; a pipeline with no
// platform at all fails one that needs a resource, and runs nothing.
func TestAResourceThePlatformCannotProvideFailsTheRelease(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p := newPipeline(f, now)
	deliver(t, p, c, "deploy", "d-8", now, signed, postgresDB)
	p.platform = &fakePlatform{}
	deliver(t, p, c, "deploy", "d-9", now, signed, map[string]any{"resources": []map[string]any{{"name": "files", "type": "s3", "gives": []string{"STORAGE_URL"}}}})
	if len(f.ran) != 0 || len(c.reports) != 2 || c.reports[0]["status"] != "failed" || c.reports[1]["reason"] != "this platform does not provide s3 resources" {
		t.Fatalf("the reports: %v; ran %v", c.reports, f.ran)
	}
}

// An undeploy's remove keeps the data; only a project delete's remove (delete: true) drops it.
func TestOnlyADeletesRemoveDropsTheData(t *testing.T) {
	c, f, now := newCanyon(t), &fakeCluster{}, time.Now()
	p, pg := newPipeline(f, now), &fakePlatform{}
	p.platform = pg
	deliver(t, p, c, "remove", "r-2", now, signed)
	if len(pg.dropped) != 0 {
		t.Fatalf("an undeploy's remove dropped %v", pg.dropped)
	}
	deliver(t, p, c, "remove", "r-3", now, signed, map[string]any{"delete": true})
	if len(f.removed) != 2 || len(pg.dropped) != 1 || pg.dropped[0] != "claims-portal preview" || c.reports[1]["status"] != "removed" {
		t.Fatalf("a delete's remove: removed %v, dropped %v, reports %v", f.removed, pg.dropped, c.reports)
	}
}
