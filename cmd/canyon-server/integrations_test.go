package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A channel's webhook URL is https on its kind's own hosts, with no userinfo and no port, or it is refused in D5's
// words (contracts/routes.md, the channels row; #2725): a typo or a pasted link never makes Canyon post elsewhere.
func TestAChannelPostsOnlyToItsKindsHosts(t *testing.T) {
	for _, c := range []struct {
		kind, url string
		ok        bool
	}{
		{"slack", "https://hooks.slack.com/services/T0/B0/XXXX", true},
		{"slack", "https://HOOKS.slack.com/services/T0/B0/XXXX", true},
		{"slack", "http://hooks.slack.com/services/T0/B0/XXXX", false},
		{"slack", "https://hooks.slack.com.evil.example/services/T0", false},
		{"slack", "https://user:pass@hooks.slack.com/services/T0", false},
		{"slack", "https://hooks.slack.com:8443/services/T0", false},
		{"slack", "https://prod-12.westeurope.logic.azure.com/workflows/x", false},
		{"teams", "https://prod-12.westeurope.logic.azure.com/workflows/x", true},
		{"teams", "https://meridian.webhook.office.com/webhookb2/x", true},
		{"teams", "https://default1.environment.api.powerplatform.com/x", true},
		{"teams", "https://logic.azure.com/workflows/x", false},
		{"teams", "https://evil.example/?h=.logic.azure.com", false},
		{"teams", "https://hooks.slack.com/services/T0", false},
		{"slack", "https://hooks.slack.com/" + strings.Repeat("a", 2048), false},
	} {
		u, said := channelURL(c.kind, c.url)
		if (u != nil) != c.ok || !c.ok && said != map[string]string{"slack": slackURLSaid, "teams": teamsURLSaid}[c.kind] {
			t.Errorf("%s %s: %v %q, want ok %v", c.kind, c.url, u, said, c.ok)
		}
	}
}

// The summary says what needs a person in production now in D5's words, the apps at most five then a count, and each
// kind renders it as its webhook takes it: Slack's text and blocks with its three characters escaped, Teams' Adaptive
// Card (contracts/routes.md, *Integrations*).
func TestTheSummaryIsD5sRenderedForEachKind(t *testing.T) {
	link := "https://canyon.meridian.example/#/fleet"
	plain := func(s string) string { return s }
	for _, c := range []struct {
		s    summary
		want string
	}{
		{summary{Needs: 0, Apps: []string{}}, "Canyon will post here when production needs a person. Right now nothing in production needs a person."},
		{summary{Needs: 1, Apps: []string{"claims-portal"}}, "Canyon will post here when production needs a person. Right now: 1 production need, across claims-portal."},
		{summary{Needs: 3, Apps: []string{"claims-portal", "endorsement-hub"}}, "Canyon will post here when production needs a person. Right now: 3 production needs, across claims-portal and endorsement-hub."},
		{summary{Needs: 9, Apps: []string{"a", "b", "c", "d", "e"}, More: 2}, "Canyon will post here when production needs a person. Right now: 9 production needs, across a, b, c, d, e and 2 more."},
	} {
		if got := c.s.line(plain); got != c.want {
			t.Errorf("%+v: %q, want %q", c.s, got, c.want)
		}
	}
	s := summary{Kind: "summary", Needs: 1, Apps: []string{"a<b>&c"}, Link: link}
	var slack struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type     string `json:"type"`
			Text     struct{ Type, Text string }
			Elements []struct{ Type, URL string }
		} `json:"blocks"`
	}
	raw, err := s.rendered("slack")
	if err != nil || json.Unmarshal(raw, &slack) != nil || !strings.Contains(slack.Text, "across a&lt;b&gt;&amp;c.") || len(slack.Blocks) != 2 ||
		slack.Blocks[0].Text.Type != "mrkdwn" || slack.Blocks[0].Text.Text != slack.Text || slack.Blocks[1].Elements[0].URL != link {
		t.Errorf("slack: %s %v", raw, err)
	}
	var teams struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Content     struct {
				Type    string `json:"type"`
				Version string `json:"version"`
				Body    []struct{ Type, Text string }
				Actions []struct{ Type, Title, URL string }
			} `json:"content"`
		} `json:"attachments"`
	}
	raw, err = s.rendered("teams")
	if err != nil || json.Unmarshal(raw, &teams) != nil || teams.Type != "message" || len(teams.Attachments) != 1 ||
		teams.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" || teams.Attachments[0].Content.Type != "AdaptiveCard" ||
		!strings.HasSuffix(teams.Attachments[0].Content.Body[0].Text, "across a<b>&c.") || teams.Attachments[0].Content.Actions[0].URL != link {
		t.Errorf("teams: %s %v", raw, err)
	}
	var golden struct {
		Summary summary `json:"summary"`
	}
	b, err := os.ReadFile("../../contracts/testdata/integrations/meridian.json")
	if err != nil || json.Unmarshal(b, &golden) != nil {
		t.Fatalf("the golden: %v", err)
	}
	if got := golden.Summary.line(plain); got != "Canyon will post here when production needs a person. Right now: 3 production needs, across claims-portal and endorsement-hub." {
		t.Errorf("the golden's summary reads %q", got)
	}
}

// A post's error never carries the webhook URL, a bearer credential, as net/http's own would, and a redirect is never
// followed: the answer is the channel's own.
func TestAPostNeverSaysItsURL(t *testing.T) {
	hook := "https://hooks.slack.com/services/T0/B0/SECRETSECRET" // a synthetic webhook URL
	saved := notifyTransport
	defer func() { notifyTransport = saved }()
	notifyTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &os.PathError{Op: "dial", Path: hook, Err: os.ErrDeadlineExceeded}
	})
	if status, _, err := postChannel(context.Background(), hook, []byte("{}")); status != 0 || err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("a post that failed: %d %v", status, err)
	}
	moved := httptest.NewServer(http.RedirectHandler("https://evil.example/", http.StatusFound))
	defer moved.Close()
	notifyTransport = http.DefaultTransport
	if status, _, err := postChannel(context.Background(), moved.URL, []byte("{}")); status != http.StatusFound || err != nil {
		t.Errorf("a redirect: %d %v, want its 302 as the answer", status, err)
	}
}

// Each message the golden holds reads D5's line for its kind and state (contracts/routes.md, *Integrations*), an
// open one ending with when it began, and renders for both kinds with its own link.
func TestEachMessageIsD5s(t *testing.T) {
	var golden struct {
		Messages []message `json:"messages"`
	}
	b, err := os.ReadFile("../../contracts/testdata/integrations/meridian.json")
	if err != nil || json.Unmarshal(b, &golden) != nil {
		t.Fatalf("the golden: %v", err)
	}
	want := []string{
		"claims-portal · production can't reach api.stripe.com. A person on the app's team can allow it in Canyon. Since 9 Nov 14:02 UTC.",
		"claims-portal has a secret in its code.",
		"endorsement-hub · production keeps restarting. Since 9 Nov 14:10 UTC.",
		"claims-portal · production's calls to api.stripe.com no longer need a person.",
		"Canyon can't post to Risk team: its webhook no longer works. An admin can fix it in Settings › Integrations. Since 9 Nov 15:00 UTC.",
		"7 more messages about production: open Canyon.",
	}
	if len(golden.Messages) != len(want) {
		t.Fatalf("the golden holds %d messages", len(golden.Messages))
	}
	for i, m := range golden.Messages {
		if got := m.line(); got != want[i] {
			t.Errorf("message %d reads %q, want %q", i, got, want[i])
		}
		for _, kind := range []string{"slack", "teams"} {
			if raw, err := render(kind, m.line(), m.Link); err != nil || !strings.Contains(string(raw), `"`+m.Link+`"`) {
				t.Errorf("message %d for %s: %s %v", i, kind, raw, err)
			}
		}
	}
	for kind, said := range openSaid {
		if kind != "restarting" && clearedSaid[kind] == "" {
			t.Errorf("%s opens but never says it cleared", kind)
		}
		if strings.Contains(message{Kind: "need", Need: kind, State: "open", App: "a", Fields: map[string]any{"destination": "d", "port": 1, "version": "v", "channel": "c"}}.line(), "<") {
			t.Errorf("%s leaves a placeholder: %s", kind, said)
		}
	}
}

// A message carries names only (contracts/routes.md, *Integrations*): every field a need adds that is a value, a code,
// a person or a log line is left out, whatever the need holds, and a posted need's key and its change are its own.
func TestAMessageCarriesNamesOnly(t *testing.T) {
	a := &app{public: "https://canyon.meridian.example"}
	prod := "prod"
	asked := actor{Kind: "agent", Subject: "agent:claude-code/carl", Display: "SECRET-person"}
	for _, n := range []need{
		newNeed("approval", &prod, nil, true, map[string]any{"version": "v3.2", "asked_by": asked, "code": "SECRET-CODE", "expires_at": "SECRET-time"}),
		newNeed("refused", &prod, nil, true, map[string]any{"destination": "api.stripe.com", "refusals": int64(14), "last_refused_at": "SECRET", "other_calls": int64(9), "held": true}),
		newNeed("down", &prod, nil, true, map[string]any{"state": "stopped", "said": "SECRET log line"}),
		newNeed("access", nil, nil, true, map[string]any{"asked_by": asked, "email": "SECRET@meridian.example"}),
	} {
		m := a.messageOf("claims-portal", n)
		raw, _ := json.Marshal(m)
		line := m.line()
		if strings.Contains(string(raw)+line, "SECRET") || strings.Contains(string(raw), "refusals") {
			t.Errorf("%s posts %s / %q", n.kind, raw, line)
		}
	}
	held, open := newNeed("refused", &prod, nil, true, map[string]any{"destination": "api.stripe.com", "refusals": int64(3), "held": true}),
		newNeed("refused", &prod, nil, true, map[string]any{"destination": "api.stripe.com", "refusals": int64(300), "held": false})
	if needKey("claims-portal", held) != needKey("claims-portal", open) || fingerprintOf(held) == fingerprintOf(open) {
		t.Error("a hold over a refusal is the same need, changed")
	}
	more := newNeed("refused", &prod, nil, true, map[string]any{"destination": "api.stripe.com", "refusals": int64(3000), "held": false})
	if fingerprintOf(more) != fingerprintOf(open) {
		t.Error("a count going up posts the need again")
	}
	if link := a.messageOf("claims-portal", open).Link; link != "https://canyon.meridian.example/#/apps/claims-portal/outbound" {
		t.Errorf("a refusal links to %s", link)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
