package main

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/vault"
)

// channelHook stands in for Slack and Teams: every post is recorded, by host, and answered status, or 404 on a path
// holding gone, a revoked webhook.
type channelHook struct {
	mu     sync.Mutex
	status int
	gone   string
	posts  []string // "<host> <body>"
}

func hookOn(t *testing.T, status int) *channelHook {
	t.Helper()
	h := &channelHook{status: status}
	saved := notifyTransport
	notifyTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.posts = append(h.posts, r.URL.Host+" "+string(body))
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("a post's content type: %q", r.Header.Get("Content-Type"))
		}
		status := h.status
		if h.gone != "" && strings.Contains(r.URL.Path, h.gone) {
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil)), Request: r}, nil
	})
	t.Cleanup(func() { notifyTransport = saved })
	return h
}

func (h *channelHook) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.posts...)
}

const slackHook = "https://hooks.slack.com/services/T000/B000/SECRETPATHSECRETPATHa1B9"

// The chat channels are an admin's alone (contracts/routes.md, the channels row; design/PERMISSIONS.md, *Integrations*;
// #2725): an admin saves one, which first posts the summary to it, reads it back by host and tail, tests it and removes
// it, each audited; a member is refused every one of the four routes. Adding or changing one is a widening, a person's
// in the dashboard: the admin's CLI session is 403 dashboard_required for both, nothing posted, and removes one as a
// browser does (#2831). No answer, list, log or audit row ever holds the webhook URL, a bearer credential.
func TestTheChatChannelsAreAnAdminsAlone(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	member := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	hook := hookOn(t, http.StatusOK)
	path := base + "/api/v1/integrations/channels/platform-alerts"
	put := `{"name":"#platform-alerts","kind":"slack","url":"` + slackHook + `"}`
	for _, c := range []struct{ method, url, body string }{
		{http.MethodGet, base + "/api/v1/integrations/channels", ""}, {http.MethodPut, path, put}, {http.MethodDelete, path, ""}, {http.MethodPost, path + "/test", ""},
	} {
		if code, _, _ := do(t, member, c.method, c.url, c.body); code != 403 || lastBody != adminRequired {
			t.Fatalf("a member's %s %s: %d %s", c.method, c.url, code, lastBody)
		}
	}
	if len(hook.sent()) != 0 {
		t.Fatalf("a member's refused PUT posted %q", hook.sent())
	}
	danaCLI := cliIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example")
	if code, _, _ := do(t, danaCLI, http.MethodPut, path, put); code != 403 || lastBody != dashboardRequired || len(hook.sent()) != 0 {
		t.Fatalf("the admin's CLI session added a channel: %d %s, %d posts", code, lastBody, len(hook.sent()))
	}
	schema := contractSchema(t, "integrations.schema.json")
	code, body, _ := do(t, dana, http.MethodPut, path, put)
	if code != 200 || strings.Contains(lastBody, "SECRETPATH") || body["channel"].(map[string]any)["tail"] != "a1B9" || body["channel"].(map[string]any)["host"] != "hooks.slack.com" {
		t.Fatalf("an admin's PUT: %d %s", code, lastBody)
	}
	if sent := hook.sent(); len(sent) != 1 || !strings.HasPrefix(sent[0], `hooks.slack.com {"blocks":`) || !strings.Contains(sent[0], "Canyon will post here when production needs a person.") {
		t.Fatalf("the save posted %q, want the summary to the channel, once", sent)
	}
	code, _, _ = do(t, dana, http.MethodGet, base+"/api/v1/integrations/channels", "")
	if code != 200 || strings.Contains(lastBody, "SECRETPATH") || !strings.Contains(lastBody, `"tail":"a1B9"`) {
		t.Fatalf("an admin's GET: %d %s", code, lastBody)
	}
	validJSON(t, schema, `{"list":`+lastBody+`,"put":`+put+`,"summary":{"kind":"summary","needs":0,"apps":[],"more":0,"link":"https://canyon.meridian.example/#/fleet"},"messages":[]}`) // A2 adds messages to the schema's root
	if code, _, _ := do(t, dana, http.MethodPut, path, `{"name":"#alerts","kind":"slack"}`); code != 200 || len(hook.sent()) != 2 {
		t.Fatalf("a change that leaves the URL out keeps it: %d %s, %d posts", code, lastBody, len(hook.sent()))
	}
	if code, _, _ := do(t, dana, http.MethodPost, path+"/test", ""); code != 204 || len(hook.sent()) != 3 {
		t.Fatalf("a test: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, danaCLI, http.MethodPut, path, `{"name":"#all-hands","kind":"slack"}`); code != 403 || lastBody != dashboardRequired {
		t.Fatalf("the admin's CLI session changed a channel: %d %s", code, lastBody)
	}
	agent := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	asAgent(t, ctx, pool, agent, base)
	if code, _, _ := do(t, agent, http.MethodPut, path, `{"name":"#all-hands","kind":"slack"}`); code != 403 ||
		lastBody != `{"error":"person_required","message":"A person adds or changes a chat channel in Canyon's dashboard: Settings › Integrations."}` {
		t.Fatalf("an agent under the admin changed a channel: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND project_id IS NULL AND detail->>'action' LIKE 'PUT /api/v1/integrations/channels/%' AND detail->>'error' IN ('person_required', 'dashboard_required')"); n != 3 {
		t.Fatalf("%d denied rows of the channel PUT, want the CLI session's two and the agent's one, naming no project", n)
	}
	if code, _, _ := do(t, danaCLI, http.MethodDelete, path, ""); code != 204 {
		t.Fatalf("the admin's CLI session removing a channel: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodDelete, path, ""); code != 404 {
		t.Fatalf("a DELETE of a channel gone: %d %s", code, lastBody)
	}
	var audited string
	if err := pool.QueryRow(ctx, "SELECT string_agg(action || ' ' || detail::text, E'\\n' ORDER BY id) FROM audit_events WHERE action LIKE 'integration.%'").Scan(&audited); err != nil ||
		strings.Contains(audited, "SECRETPATH") || strings.Count(audited, "integration.channel.set") != 2 || strings.Count(audited, "integration.channel.tested") != 1 ||
		strings.Count(audited, "integration.channel.removed") != 1 || !strings.Contains(audited, `"host": "hooks.slack.com"`) {
		t.Fatalf("audited %s (%v)", audited, err)
	}
}

// A channel is saved only once it took the summary (contracts/routes.md, the channels row): one that answers anything
// but 2xx, or nothing in time, saves nothing and says so in D5's words, never naming the URL; a URL on another host is
// refused before anything is posted; a sixth channel is refused.
func TestAChannelThatDoesNotTakeTheSummaryIsNeverSaved(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	hook := hookOn(t, http.StatusNotFound)
	put := func(id, kind, url string) (int, map[string]any) {
		t.Helper()
		code, body, _ := do(t, dana, http.MethodPut, base+"/api/v1/integrations/channels/"+id, `{"name":"#`+id+`","kind":"`+kind+`","url":"`+url+`"}`)
		return code, body
	}
	if code, body := put("gone", "slack", slackHook); code != 422 || body["error"] != "channel_unreachable" || body["status"] != float64(404) ||
		body["message"] != "Slack answered 404: that webhook URL doesn't exist or was removed. Nothing was saved." || strings.Contains(lastBody, "SECRETPATH") {
		t.Fatalf("a channel answering 404: %d %s", code, lastBody)
	}
	for status, want := range map[int]string{ // a URL Teams says is gone is said as gone; any other answer is its number
		http.StatusGone:                "Teams answered 410: that webhook URL doesn't exist or was removed. Nothing was saved.",
		http.StatusInternalServerError: "Canyon couldn't post to this channel: it answered 500. Nothing was saved.",
	} {
		hook.mu.Lock()
		hook.status = status
		hook.mu.Unlock()
		if code, body := put("gone", "teams", "https://prod-12.westeurope.logic.azure.com/workflows/gone"); code != 422 || body["message"] != want {
			t.Fatalf("a channel answering %d: %d %s", status, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_channels"); n != 0 {
		t.Fatalf("a channel that didn't take the summary was saved: %d rows", n)
	}
	posts := len(hook.sent())
	if code, body := put("elsewhere", "slack", "https://hooks.slack.com.evil.example/services/T0"); code != 400 || body["message"] != slackURLSaid || len(hook.sent()) != posts {
		t.Fatalf("a URL on another host: %d %s", code, lastBody)
	}
	if code, body := put("cross", "teams", slackHook); code != 400 || body["message"] != teamsURLSaid {
		t.Fatalf("a Slack URL for a Teams channel: %d %s", code, lastBody)
	}
	if code, _ := put("new", "slack", ""); code != 400 {
		t.Fatalf("a new channel with no URL: %d %s", code, lastBody)
	}
	hook.mu.Lock()
	hook.status = http.StatusOK
	hook.mu.Unlock()
	for i := range maxChannels {
		if code, _ := put("ch-"+string(rune('a'+i)), "teams", "https://prod-12.westeurope.logic.azure.com/workflows/"+string(rune('a'+i))); code != 200 {
			t.Fatalf("channel %d: %d %s", i, code, lastBody)
		}
	}
	if code, body := put("ch-f", "slack", slackHook); code != 409 || body["error"] != "too_many_channels" {
		t.Fatalf("a sixth channel: %d %s", code, lastBody)
	}
}

// A chat channel's webhook URL is a sealed row: a generated key over one is a key volume that did not mount, and one
// under a key version the store lacks refuses the boot naming it (AGENTS.md, a new sealed table's three places).
func TestBootRefusesChannelsUnderAnotherKey(t *testing.T) {
	pool, ctx := migrated(t)
	fresh, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil || !fresh.Generated {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notify_channels (id, name, kind, host, tail, key_ref, sealed_key, sealed_url, set_by_kind, set_by_subject, set_by_display)
		VALUES ('platform-alerts', '#platform-alerts', 'slack', 'hooks.slack.com', 'a1B9', 2, '', '', 'person', 'dana', 'Dana')`); err != nil {
		t.Fatal(err)
	}
	if err := refuseNewKeyOverSealedRows(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "1 row(s) are sealed under another key") {
		t.Fatalf("a generated key over a channel's URL: %v", err)
	}
	if err := refuseRowsUnderAMissingKey(ctx, pool, fresh); err == nil || !strings.Contains(err.Error(), "1 chat channel webhook URL(s) under key version 2") {
		t.Fatalf("a channel's URL under a missing version refuses the boot by name: %v", err)
	}
}

// A chat channel is told a need once, as it opens, again only when it changes in a way a person acts on, and once
// when it has stayed clear 30 minutes (contracts/routes.md, *Integrations*; #2725): a count going up posts nothing,
// a tick that finds nothing new queues nothing, and what the save's summary said is never posted one by one.
func TestANeedIsPostedOnceAndItsClearingOnce(t *testing.T) {
	pool, ctx, base, dana, _ := needyHost(t)
	seedNeedy(t, ctx, pool, 1, 1)
	a := appOf(t, base)
	hook := hookOn(t, http.StatusOK)
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/integrations/channels/alerts", `{"name":"#alerts","kind":"slack","url":"`+slackHook+`"}`); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	now := time.Now()
	tick := func(after time.Duration) {
		t.Helper()
		if err := a.notifyTick(ctx, now.Add(after)); err != nil {
			t.Fatal(err)
		}
	}
	queued := func(need string) int {
		t.Helper()
		return count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE message->>'need' = '"+need+"'")
	}
	tick(0)
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_outbox"); n != 0 {
		t.Fatalf("the needs the summary said were queued again: %d", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals)
		VALUES ('app-0001', 'prod', 'api.partner.example', 'pending', now(), now(), 1)`); err != nil {
		t.Fatal(err)
	}
	tick(time.Minute)
	tick(2 * time.Minute)
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE message->'fields'->>'destination' = 'api.partner.example' AND message->>'state' = 'open'"); n != 1 || queued("refused") != 1 {
		t.Fatalf("a new refusal queued %d, want once", n)
	}
	if _, err := pool.Exec(ctx, "UPDATE egress_requests SET refusals = 300 WHERE destination = 'api.partner.example'"); err != nil {
		t.Fatal(err)
	}
	tick(3 * time.Minute)
	if n := queued("refused"); n != 1 {
		t.Fatalf("a count going up queued the refusal again: %d", n)
	}
	if err := a.notifySend(ctx, now.Add(3*time.Minute), func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	sent := hook.sent()
	if len(sent) != 2 || !strings.HasPrefix(sent[1], "hooks.slack.com ") || !strings.Contains(sent[1], "app-0001 · production can't reach api.partner.example. A person on the app's team can allow it in Canyon.") ||
		count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE status = 'sent' AND answered = 200") != 1 {
		t.Fatalf("the send posted %q", sent)
	}
	if _, err := pool.Exec(ctx, "UPDATE packages SET label = 'v9.9' WHERE project_id = 'app-0001'"); err != nil {
		t.Fatal(err)
	}
	tick(4 * time.Minute)
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE message->>'need' = 'approval' AND message->'fields'->>'version' = 'v9.9'"); n != 1 || queued("approval") != 1 {
		t.Fatalf("an approval of another version queued %d, want once", n)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM egress_requests WHERE destination = 'api.partner.example'"); err != nil {
		t.Fatal(err)
	}
	tick(5 * time.Minute)
	tick(20 * time.Minute)
	if n := queued("refused"); n != 1 {
		t.Fatalf("a refusal gone under 30 minutes queued its clearing: %d", n)
	}
	tick(36 * time.Minute)
	tick(37 * time.Minute)
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE message->>'need' = 'refused' AND message->>'state' = 'cleared' AND NOT message ? 'since'"); n != 1 || queued("refused") != 2 {
		t.Fatalf("a refusal gone 30 minutes queued %d clearings, want one", n)
	}
}

// A message another server leased is its alone until the lease lapses by the database's clock: a server whose own
// clock runs 10 minutes fast takes no row still leased, so the message is not posted twice (#2725's review).
func TestALeaseIsTheDatabasesClock(t *testing.T) {
	pool, ctx, base, dana, _ := needyHost(t)
	a := appOf(t, base)
	hook := hookOn(t, http.StatusOK)
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/integrations/channels/alerts", `{"name":"#alerts","kind":"slack","url":"`+slackHook+`"}`); code != 200 {
		t.Fatalf("the save: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notify_outbox (channel_id, message, created_at, next_at, lease_until)
		VALUES ('alerts', '{"kind":"overflow","more":2,"link":"https://canyon.meridian.example/#/fleet"}', now(), now(), now() + interval '5 minutes')`); err != nil {
		t.Fatal(err)
	}
	if err := a.notifySend(ctx, time.Now().Add(10*time.Minute), func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if sent := hook.sent(); len(sent) != 1 { // the save's summary alone
		t.Fatalf("a server with a fast clock posted a message another server holds: %q", sent)
	}
}

// A channel Slack or Teams answers 404 or 410 is failing, an admin's need until an admin saves it again
// (contracts/routes.md, *Integrations* and the fleet read's org_needs; #2725): audited once, counted in the admin's
// needs_you, posted to the org's other channels and never to itself, and no one below admin reads it.
func TestAChannelThatStopsTakingMessagesIsAnAdminsNeed(t *testing.T) {
	pool, ctx, base, dana, _ := needyHost(t)
	seedNeedy(t, ctx, pool, 1, 1)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	a := appOf(t, base)
	hook := hookOn(t, http.StatusOK)
	for _, put := range []struct{ id, body string }{
		{"alerts", `{"name":"#alerts","kind":"slack","url":"` + slackHook + `"}`},
		{"risk-team", `{"name":"Risk team","kind":"teams","url":"https://prod-12.westeurope.logic.azure.com/workflows/gone"}`},
	} {
		if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/integrations/channels/"+put.id, put.body); code != 200 {
			t.Fatalf("saving %s: %d %s", put.id, code, lastBody)
		}
	}
	hook.mu.Lock()
	hook.gone = "/workflows/gone"
	hook.mu.Unlock()
	if _, err := pool.Exec(ctx, `INSERT INTO egress_requests (project_id, env, destination, state, first_refused_at, last_refused_at, refusals)
		VALUES ('app-0001', 'prod', 'api.partner.example', 'pending', now(), now(), 1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, after := range []time.Duration{0, time.Minute} {
		if err := a.notifyTick(ctx, now.Add(after)); err != nil {
			t.Fatal(err)
		}
		if err := a.notifySend(ctx, now.Add(after), func(time.Duration) {}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_channels WHERE id = 'risk-team' AND failing_since IS NOT NULL AND failing_status = 404"); n != 1 {
		t.Fatalf("a channel answering 404 isn't failing: %d", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'integration.channel.failing' AND detail->>'id' = 'risk-team' AND detail::text NOT LIKE '%workflows%'"); n != 1 {
		t.Fatalf("the failing channel audited %d times, want once, by host", n)
	}
	var told []string
	for _, p := range hook.sent() {
		if strings.Contains(p, "Canyon can't post to Risk team: its webhook no longer works.") {
			told = append(told, p)
		}
	}
	if len(told) != 1 || !strings.HasPrefix(told[0], "hooks.slack.com ") {
		t.Fatalf("the failing channel was told to %q, want the other channel once", told)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM notify_outbox WHERE channel_id = 'risk-team' AND status = 'queued'"); n != 0 {
		t.Fatalf("a failing channel still has %d messages queued to post", n)
	}
	if code, list, _ := do(t, dana, http.MethodGet, base+"/api/v1/integrations/channels", ""); code != 200 ||
		list["channels"].([]any)[1].(map[string]any)["failing"].(map[string]any)["status"] != float64(404) || list["channels"].([]any)[0].(map[string]any)["failing"] != nil {
		t.Fatalf("the channels read says nothing of the failing one: %d %s", code, lastBody)
	}
	validJSON(t, contractSchema(t, "integrations.schema.json#/definitions/list"), lastBody)
	fleet := contractSchema(t, "fleet.schema.json")
	_, body, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", "")
	validJSON(t, fleet, lastBody)
	org, _ := body["org_needs"].([]any)
	carried := len(org)
	for _, row := range body["apps"].([]any) {
		needs, _ := row.(map[string]any)["needs"].([]any)
		carried += len(needs)
	}
	if len(org) != 1 || org[0].(map[string]any)["kind"] != "notify_failing" || org[0].(map[string]any)["channel_id"] != "risk-team" ||
		org[0].(map[string]any)["channel"] != "Risk team" || body["tiles"].(map[string]any)["needs_you"] != float64(carried) {
		t.Fatalf("the admin's fleet read: org_needs %v, needs_you %v of %d", org, body["tiles"].(map[string]any)["needs_you"], carried)
	}
	_, body, _ = do(t, carol, http.MethodGet, base+"/api/v1/fleet", "")
	validJSON(t, fleet, lastBody)
	if org, _ := body["org_needs"].([]any); org == nil || len(org) != 0 {
		t.Fatalf("a member's fleet read carries org_needs %v", body["org_needs"])
	}
	hook.mu.Lock()
	hook.gone = ""
	hook.mu.Unlock()
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/integrations/channels/risk-team", `{"name":"Risk team","kind":"teams"}`); code != 200 {
		t.Fatalf("saving it again: %d %s", code, lastBody)
	}
	if _, body, _ = do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); len(body["org_needs"].([]any)) != 0 {
		t.Fatalf("a channel saved again is still failing: %v", body["org_needs"])
	}
}

// A refused channel PUT names the channel as its denied row's target, never as an app (contracts/routes.md, the audit
// read; #2821): a channel whose id is an app's, claims-portal, reads in neither that app's owner's nor an admin's
// audit of the app, and its row names {kind: channel, id} with no project.
func TestAChannelNamedLikeAnAppIsNotItsDenial(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	danaCLI := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	if code, _, _ := do(t, danaCLI, http.MethodPut, base+"/api/v1/integrations/channels/claims-portal", `{"name":"#claims","kind":"slack"}`); code != 403 || lastBody != dashboardRequired {
		t.Fatalf("the admin's CLI session adding a channel named like an app: %d %s", code, lastBody)
	}
	for who, c := range map[string]*http.Client{"its owner": olive, "an admin": dana} {
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/audit?project=claims-portal&action=denied", ""); code != 200 || strings.Contains(lastBody, "integrations/channels") {
			t.Errorf("claims-portal's audit, read by %s, holds the channel's denial: %d %s", who, code, lastBody)
		}
	}
	var app bool
	var target string
	if err := pool.QueryRow(ctx, "SELECT project_id IS NOT NULL, coalesce(detail->>'target', '') FROM audit_events WHERE action = 'denied' AND detail->>'action' LIKE 'PUT /api/v1/integrations/channels/%'").Scan(&app, &target); err != nil ||
		app || target != `{"id": "claims-portal", "kind": "channel"}` {
		t.Errorf("the channel's denied row: names an app %v, target %s (%v)", app, target, err)
	}
}
