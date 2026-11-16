package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// dana's Allow of api.openai.com, after preview's v1.3 went live (2026-09-01T11:26:40Z), six minutes before the
// status tests' clock, which closed the request Canyon filed when it refused the app's call; and one from before the
// version went live, which it already met.
var (
	danaAllows = outboundAllow{Entry: "api.openai.com", By: actorRef{Kind: "person", Subject: "sub-dana", Display: "Dana Whitfield"},
		At: "2026-09-02T11:54:00Z", LastRefusedAt: ptr("2026-09-02T11:53:41Z")}
	olderAllow = outboundAllow{Entry: "hooks.slack.com", By: actorRef{Kind: "person", Subject: "sub-olive", Display: "Olive"},
		At: "2026-08-20T09:12:00Z", LastRefusedAt: ptr("2026-08-20T09:11:43Z")}
)

// allowedStatus is the fixture's status read with preview alone, running under allowlist, nothing refused there
// since v1.3 went live, and the two Allows.
func allowedStatus(t *testing.T) (statusRead, *runRow) {
	t.Helper()
	s, latest := promotedStatus(t)
	delete(s.Envs, "prod")
	s.Outbound = egressRead(t, "canyon")
	s.Outbound.Envs["preview"].Mode, s.Outbound.Envs["preview"].Allows = "allowlist", []outboundAllow{danaAllows, olderAllow}
	return s, latest
}

func agentStatus(s statusRead, latest *runRow) string {
	var agent strings.Builder
	(&app{out: &agent, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	return agent.String()
}

// Run 3 of the #2162 journey: a person allowed api.openai.com, the app's next call went through, and status said only
// "no destination refused", so the agent read the old failures as current, changed the app's HTTP client and deployed
// again. Status names each destination a person allowed in an env since its version went live, by whom and when, and
// that the env's calls go through now with no deploy; an agent reads what to do, in place of the HTTPS_PROXY hint,
// which an Allow that closed a refusal makes false. An Allow from before the version went live is the version's own
// config, and is never named (cli.md §6.1.5, #2722). The goldens are the record, and §6.1.5's transcript is the agent's.
func TestStatusNamesWhatAPersonAllowed(t *testing.T) {
	s, latest := allowedStatus(t)
	human, agent := rendered(t, s, latest), agentStatus(s, latest)
	wants(t, "status-allowed.txt", human)
	wants(t, "status-ai-allowed.txt", agent)
	if spec := specFence(t, "#### 6.1.5 After an Allow"); !strings.Contains(agent, "\n\n"+spec+"\n") {
		t.Errorf("canyon status --format ai does not carry §6.1.5's transcript:\n%s", spec)
	}
	for _, out := range []string{human, agent} {
		if strings.Contains(out, "hooks.slack.com") || strings.Contains(out, "HTTPS_PROXY") {
			t.Errorf("an Allow from before the version, or the HTTPS_PROXY hint, was said:\n%s", out)
		}
	}
}

// An Allow is its env's alone: with api.openai.com allowed in preview and still refused in production, the agent
// reads production's refusal and its block, and that preview's calls go through, never that the app's do, nor not
// to deploy for a name production still refuses.
func TestAnAllowIsSaidOfItsEnvAlone(t *testing.T) {
	s, latest := allowedStatus(t)
	p, _ := promotedStatus(t)
	s.Envs["prod"] = p.Envs["prod"]
	s.Outbound.Envs["prod"].Requests = []outboundRequest{{Destination: "api.openai.com", State: "pending", LastRefusedAt: "2026-09-02T11:58:00Z", Refusals: 3}}
	agent := agentStatus(s, latest)
	for _, want := range []string{
		"outbound: production · only allowed destinations · 1 destination refused since v1.3 went live\nrefused: api.openai.com · not allowed 3 times · last 2026-09-02T11:58:00Z\n",
		"outcome: Canyon refused these calls with HTTP 403.",
		"\nA person allowed these destinations in preview: api.openai.com. Preview's calls to them go through now, with no deploy.\ndo:\n",
		"  - change the code or deploy again for a destination a person allowed in that environment\n",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("want %q in:\n%s", want, agent)
		}
	}
	if strings.Contains(agent, "The app's calls") || strings.Contains(agent, "in production: api.openai.com") {
		t.Errorf("preview's Allow was said of the app, or of production, which refuses it:\n%s", agent)
	}
	want := "  outbound     preview: only allowed destinations · 2 destinations in the last 7 days (beta)\n" +
		"                 ✓ api.openai.com   allowed by Dana Whitfield 6m ago · calls go through now, no deploy needed\n" +
		"               production: only allowed destinations · 1 destination refused since v1.3 went live\n" +
		"                 ✗ api.openai.com   not allowed 3 times, last 2m ago"
	if got := outboundLines(rendered(t, s, latest)); got != want {
		t.Errorf("the row is\n%s\nwant\n%s", got, want)
	}
}

// Only an Allow that closed a refusal, under allowlist, proves the app's client goes through the gateway: a person's
// Allow before any refusal, or one under observe, which refuses nothing, says nothing of calls going through, and the
// HTTPS_PROXY hint stays. Nor does an env that isn't running make calls: a stopped or a held one names no Allow. A host
// before #2722 sends no allows, and status says what it said before (#2722).
func TestStatusSaysCallsGoThroughOnlyWhereAnAllowProvesIt(t *testing.T) {
	for name, edit := range map[string]func(s *statusRead){
		"an Allow before any refusal": func(s *statusRead) {
			s.Outbound.Envs["preview"].Allows = []outboundAllow{{Entry: "api.openai.com", By: danaAllows.By, At: danaAllows.At}}
		},
		"an Allow under observe": func(s *statusRead) { s.Outbound.Envs["preview"].Mode = "observe" },
		"a stopped preview":      func(s *statusRead) { s.Envs["preview"].State = "stopped" },
		"a held production": func(s *statusRead) { // its Allow after v1.3 went live there, its container stopped by the hold
			p := *s.Envs["preview"]
			p.State, p.DeployedAt = "stopped", "2026-08-30T12:00:00Z"
			s.Envs["prod"], s.Receipt.Held = &p, &prodHold{Reason: "suspected leak"}
			s.Outbound.Envs["prod"].Allows, s.Outbound.Envs["prod"].Requests, s.Outbound.Envs["preview"].Allows = []outboundAllow{danaAllows}, nil, nil
		},
		"a host before #2722": func(s *statusRead) { s.Outbound.Envs["preview"].Allows = nil },
	} {
		s, latest := allowedStatus(t)
		edit(&s)
		agent, human := agentStatus(s, latest), rendered(t, s, latest)
		if said := agent + human; strings.Contains(said, "allowed:") || strings.Contains(said, "allowed by") || strings.Contains(said, "go through now") {
			t.Errorf("%s: said calls go through:\n%s\n%s", name, agent, human)
		}
		if !strings.Contains(agent, timeoutHint) { // nothing refused and no Allow that proves the gateway: the hint stands
			t.Errorf("%s: no HTTPS_PROXY hint:\n%s", name, agent)
		}
	}
}

// With sign-in off the one caller is the anonymous admin, whom D5 names someone not signed in, as the dashboard does.
func TestAnAllowWithSignInOffIsSomeoneNotSignedIn(t *testing.T) {
	s, latest := allowedStatus(t)
	s.Outbound.Envs["preview"].Allows[0].By = actorRef{Kind: "person", Subject: "anonymous"}
	agent, human := agentStatus(s, latest), rendered(t, s, latest)
	if !strings.Contains(agent, "allowed: api.openai.com · by someone not signed in at ") || !strings.Contains(human, "allowed by someone not signed in 6m ago") ||
		strings.Contains(agent+human, "anonymous") {
		t.Errorf("the anonymous admin's Allow:\n%s\n%s", agent, human)
	}
}

// allowedLogHost is run 3's preview: 001f3c went live at 11:13:09, Canyon refused api.openai.com, and dana allowed it
// at 11:37:50, closing the request; an Allow from before the version went live is its own config. Its status read
// says whether preview runs.
func allowedLogHost(state string) *logsHost {
	h := refusedHost("ready on :8080\n", cancelled, cancelled)
	h.project = `{"project":{"id":"claims-portal","envs":{"preview":{"package":"pkg_001f3c","deployed_at":"2026-11-08T11:13:09Z"}}}}`
	h.egress = `{"floor":"enforced","envs":{"preview":{"via":"canyon","mode":"allowlist","requests":[],"allows":[` +
		`{"entry":"api.openai.com","by":{"kind":"person","subject":"sub-dana","display":"Dana Whitfield"},"at":"2026-11-08T11:37:50Z","last_refused_at":"2026-11-08T11:37:41Z"},` +
		`{"entry":"hooks.slack.com","by":{"kind":"person","subject":"sub-olive","display":"Olive"},"at":"2026-11-01T09:00:00Z","last_refused_at":"2026-11-01T08:59:00Z"}]},"prod":{"requests":[]}}}`
	if state != "" {
		h.now = `{"envs":{"preview":{"package":"pkg_001f3c","deployed_at":"2026-11-08T11:13:09Z","state":"` + state + `"}}}`
	}
	return h
}

// After an Allow the log still held the app's failures from before it, and nothing in it said the refusals had
// stopped. The log names each destination a person allowed since the version went live, while the env runs: who,
// when, and when Canyon last refused it, so an agent can place the app's failures before the Allow (cli.md §6.4.2,
// #2722). Only the status read says the env runs, so the log asks it only when there is an Allow to name, and names
// none when it says otherwise or can't be read. The golden ends in §6.4.2's transcript.
func TestLogsMarkWhenAPersonAllowedADestination(t *testing.T) {
	want := golden(t, "logs-allowed.txt") // before the run moves into the project's directory
	if spec := specFence(t, "#### 6.4.2 After an Allow"); !strings.HasSuffix(want, "\n"+spec) {
		t.Errorf("testdata/logs-allowed.txt does not end in §6.4.2's transcript:\n%s", spec)
	}
	for _, format := range []string{"human", "ai"} {
		h := allowedLogHost("running")
		server := serveLogs(t, h)
		var o, e strings.Builder
		a := logsApp(t, &o, server.URL)
		a.err, a.clock = &e, clock{now: func() time.Time { return time.Date(2026, 11, 8, 11, 40, 0, 0, time.UTC) }, sleep: func(time.Duration) {}}
		if code := runLogs(context.Background(), a, []string{"--format", format}); code != exitOK || e.String() != "" || o.String() != want {
			t.Errorf("--format %s: exit %d, stderr %q, rendered\n%s\ntestdata/logs-allowed.txt wants\n%s", format, code, e.String(), o.String(), want)
		}
		reads := "/api/v1/projects/claims-portal /api/v1/projects/claims-portal/egress /api/v1/egress /api/v1/front-door /api/v1/projects/claims-portal/status"
		if strings.Join(h.read, " ") != reads {
			t.Errorf("--format %s read %q beside the log, want %q", format, h.read, reads)
		}
	}
	for name, h := range map[string]*logsHost{"a stopped preview": allowedLogHost("stopped"), "a status read that says nothing of preview": allowedLogHost("")} {
		out, errs, code := logsRun(t, h)
		if code != exitOK || errs != "" || strings.Contains(out, "allowed") || strings.Contains(out, "go through now") {
			t.Errorf("%s: exit %d, stderr %q, rendered\n%s", name, code, errs, out)
		}
	}
}

// Ctrl-C during the status read the log makes to name an Allow prints nothing more, the refusals it read before
// included (cli.md §6.4.1).
func TestLogsSayNothingMoreWhenCtrlCEndsTheStatusRead(t *testing.T) {
	h := allowedLogHost("running")
	h.egress = strings.Replace(h.egress, `"requests":[]`, `"requests":[{"destination":"api.anthropic.com","state":"pending","first_refused_at":"2026-11-08T11:20:00Z","last_refused_at":"2026-11-08T11:30:00Z","refusals":2}]`, 1)
	h.lines, h.hold, h.asking = nil, true, make(chan struct{}, 1) // no line of the app's to race the cancel
	server := serveLogs(t, h)
	out := make(writes, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := logsApp(t, out, server.URL)
	a.tty = true
	code := make(chan int, 1)
	go func() { code <- runLogs(ctx, a, nil) }()
	const heading = "\n  claims-portal · preview · live · Ctrl-C to stop\n\n"
	var seen strings.Builder
	deadline := time.After(5 * time.Second)
	for asking := h.asking; !strings.HasSuffix(seen.String(), heading) || asking != nil; {
		select {
		case w := <-out:
			seen.WriteString(w)
		case <-asking:
			asking = nil
		case <-deadline:
			t.Fatalf("the heading and the status read did not arrive within 5s; stdout so far %q", seen.String())
		}
	}
	cancel()
	if got := <-code; got != exitOK {
		t.Errorf("ctrl-c ended the read with exit %d", got)
	}
	for len(out) > 0 {
		seen.WriteString(<-out)
	}
	if seen.String() != heading {
		t.Errorf("stdout was\n%q\nwant\n%q", seen.String(), heading)
	}
}
