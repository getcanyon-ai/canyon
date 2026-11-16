package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate"
)

// scripted is the second implementation of the model seam, and what earns the interface: no
// network, no SDK. It drives the same dispatcher the SDK drives, in a fixed order, and answers a
// canned line.
type scripted struct {
	script     []step        // the tool calls to make, in order
	line       string        // the line the turn answers
	err        error         // answered instead of line, when set
	concurrent time.Duration // when set, the steps run at once, each launched this far after the one before
	results    []string      // what the dispatcher answered, one per step
	turns      int           // times the handler reached the model at all
	calls      int           // tool calls made
	deadline   time.Time     // the deadline the handler gave Turn; zero when it gave none
}

type step struct{ name, input string }

func (s *scripted) Turn(ctx context.Context, _ []turnMessage, call toolCall) (string, error) {
	s.turns++
	s.deadline, _ = ctx.Deadline()
	if s.concurrent > 0 {
		return s.siblings(ctx, call)
	}
	for _, st := range s.script {
		s.calls++
		out, err := call(ctx, st.name, json.RawMessage(st.input))
		if err != nil {
			return "", err
		}
		s.results = append(s.results, out)
	}
	if s.err != nil {
		return "", s.err
	}
	return s.line, nil
}

// siblings runs the script the way the SDK runs one assistant message's tool calls: all at once,
// with the results collected in call order rather than in the order they came back. The steps are
// launched s.concurrent apart, so which call reached the dispatcher first is the script's order
// and not the scheduler's.
func (s *scripted) siblings(ctx context.Context, call toolCall) (string, error) {
	s.results = make([]string, len(s.script))
	errs := make([]error, len(s.script))
	var wg sync.WaitGroup
	for i, st := range s.script {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * s.concurrent)
			s.results[i], errs[i] = call(ctx, st.name, json.RawMessage(st.input))
		}()
	}
	wg.Wait()
	s.calls = len(s.script)
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	if s.err != nil {
		return "", s.err
	}
	return s.line, nil
}

// blocking is a model that parks in Turn until it is let go, which is what a turn in flight is:
// this host's slot is held for as long as the model takes to answer.
type blocking struct {
	entered chan struct{} // one value per turn that reached the model
	let     chan struct{} // one value lets one held turn answer
}

func (b *blocking) Turn(ctx context.Context, _ []turnMessage, _ toolCall) (string, error) {
	b.entered <- struct{}{}
	select {
	case <-b.let:
		return "held, then answered.", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// withModel points the next test server at m until the test ends — the fake/pipe precedent.
func withModel(t *testing.T, m model) *scripted {
	t.Helper()
	testModel = m
	t.Cleanup(func() { testModel = nil })
	s, _ := m.(*scripted)
	return s
}

// newModels is boot-time configuration, so every combination this build cannot serve must fail
// here, where an operator is still reading the log, and not at the first turn or the first run.
// Every variable is set on every case: the process running the tests may already carry one.
func TestNewModelsAreConfiguredOnlyWhenACredentialResolves(t *testing.T) {
	// Synthetic credential shapes: a made-up prefix and filler, never a vendor's (AGENTS.md).
	const key, token = "zz-key-xxxxxxxxxxxxxxxxxxxxxxxx", "zz-token-yyyyyyyyyyyyyyyy"
	for _, tc := range []struct {
		what     string
		env      map[string]string
		model    bool
		id       string // checked when named: Canyon hands the provider the host's id, unmapped
		review   string // the reviewer's client, empty for none
		reviewID string
		refuses  string // a fragment of the boot error; empty means the boot succeeds
	}{
		{"a host that names nothing has no model", nil, false, "", "", "", ""},
		{"the Claude API with a key", map[string]string{"ANTHROPIC_API_KEY": key}, true, "claude-opus-5", "*gate.Anthropic", "claude-opus-5", ""},
		{"an inference-profile id the host named", map[string]string{"ANTHROPIC_API_KEY": key, "CANYON_MODEL": "eu.anthropic.claude-opus-5-v1:0"}, true, "eu.anthropic.claude-opus-5-v1:0", "*gate.Anthropic", "eu.anthropic.claude-opus-5-v1:0", ""},
		{"a gateway holding the key, reached with a bearer token", map[string]string{"CANYON_MODEL_URL": "https://gw.example", "CANYON_MODEL_TOKEN": token}, true, "", "*gate.Anthropic", "claude-opus-5", ""},
		{"a gateway that wants no credential", map[string]string{"CANYON_MODEL_URL": "https://gw.example"}, true, "", "*gate.Anthropic", "claude-opus-5", ""},
		{"a loopback gateway over http", map[string]string{"CANYON_MODEL_URL": "http://127.0.0.1:4000", "ANTHROPIC_API_KEY": key}, true, "", "*gate.Anthropic", "claude-opus-5", ""},
		{"an in-cluster gateway over http, under a path", map[string]string{"CANYON_MODEL_URL": "http://gateway.internal:4000/anthropic", "ANTHROPIC_API_KEY": key}, true, "", "*gate.Anthropic", "claude-opus-5", ""},
		{"openai reviews, and the setup chat is off", map[string]string{"CANYON_MODEL_PROVIDER": "openai", "OPENAI_API_KEY": key, "CANYON_MODEL": "gpt-test"}, false, "", "*gate.OpenAI", "gpt-test", ""},
		{"an id and no way to reach a provider", map[string]string{"CANYON_MODEL": "claude-opus-5"}, false, "", "", "", "no credential resolves"},
		{"a provider and no credential", map[string]string{"CANYON_MODEL_PROVIDER": "anthropic"}, false, "", "", "", "no credential resolves"},
		{"a key and a token: which one does a turn carry", map[string]string{"ANTHROPIC_API_KEY": key, "CANYON_MODEL_TOKEN": token}, false, "", "", "", "both set"},
		{"bedrock", map[string]string{"CANYON_MODEL_PROVIDER": "bedrock", "ANTHROPIC_API_KEY": key}, false, "", "", "", "needs cloud SDKs this build does not carry; a gateway in front of it works today"},
		{"openai without its key", map[string]string{"CANYON_MODEL_PROVIDER": "openai", "ANTHROPIC_API_KEY": key, "CANYON_MODEL": "gpt-test"}, false, "", "", "", "OPENAI_API_KEY"},
		{"openai without a model id", map[string]string{"CANYON_MODEL_PROVIDER": "openai", "OPENAI_API_KEY": key}, false, "", "", "", "CANYON_MODEL"},
		{"a provider nobody implements", map[string]string{"CANYON_MODEL_PROVIDER": "llama", "ANTHROPIC_API_KEY": key}, false, "", "", "", "not a provider Canyon knows"},
		{"a gateway address carrying a credential", map[string]string{"CANYON_MODEL_URL": "https://user:" + token + "@gw.example", "ANTHROPIC_API_KEY": key}, false, "", "", "", "not an address Canyon will call"},
		{"a gateway address that is not http", map[string]string{"CANYON_MODEL_URL": "ftp://gw.example", "ANTHROPIC_API_KEY": key}, false, "", "", "", "not an address Canyon will call"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			for _, v := range modelEnv {
				t.Setenv(v, tc.env[v])
			}
			m, review, err := newModels()
			if tc.refuses != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuses) {
					t.Fatalf("the boot must refuse this and say %q: it booted a model (%v) with err %v", tc.refuses, m != nil, err)
				}
				// The reason a host cannot boot is never the value that makes it unbootable.
				if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), token) {
					t.Fatalf("the boot error carries a credential: %v", err)
				}
				return
			}
			if err != nil || (m != nil) != tc.model {
				t.Fatalf("want a model: %v; got model %v, err %v", tc.model, m != nil, err)
			}
			if tc.id != "" && m.(*anthropicModel).id != tc.id {
				t.Fatalf("Canyon maps no id: want %q, got %q", tc.id, m.(*anthropicModel).id)
			}
			if got := fmt.Sprintf("%T", review.Model); (tc.review == "" && review.Model != nil) || (tc.review != "" && got != tc.review) || review.ModelID != tc.reviewID {
				t.Fatalf("want the reviewer %q under %q; got %s under %q", tc.review, tc.reviewID, got, review.ModelID)
			}
		})
	}
}

// modelEnv is every variable newModels reads.
var modelEnv = []string{"CANYON_MODEL_PROVIDER", "CANYON_MODEL", "CANYON_MODEL_URL", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CANYON_MODEL_TOKEN"}

// recorder is the process's transport for one test: it keeps every URL asked for and answers 400,
// which neither SDK retries. No request leaves the process.
type recorder struct{ urls []string }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
}

// A base URL in the host's environment that Canyon's settings do not name never redirects a review:
// the reviewer calls the configured base, or the provider's own when none is configured.
func TestTheReviewCallsOnlyTheConfiguredBase(t *testing.T) {
	const key = "zz-key-xxxxxxxxxxxxxxxxxxxxxxxx"
	for _, tc := range []struct {
		what string
		env  map[string]string
		want string
	}{
		{"anthropic", map[string]string{"ANTHROPIC_API_KEY": key}, "https://api.anthropic.com/v1/messages"},
		{"anthropic through a gateway", map[string]string{"ANTHROPIC_API_KEY": key, "CANYON_MODEL_URL": "http://gateway.internal:4000/anthropic"}, "http://gateway.internal:4000/anthropic/v1/messages"},
		{"openai", map[string]string{"CANYON_MODEL_PROVIDER": "openai", "OPENAI_API_KEY": key, "CANYON_MODEL": "gpt-test"}, "https://api.openai.com/v1/chat/completions"},
		{"openai through a gateway", map[string]string{"CANYON_MODEL_PROVIDER": "openai", "OPENAI_API_KEY": key, "CANYON_MODEL": "gpt-test", "CANYON_MODEL_URL": "http://gateway.internal:4000/openai"}, "http://gateway.internal:4000/openai/chat/completions"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the review called the environment's base URL") }))
			defer trap.Close()
			for _, v := range modelEnv {
				t.Setenv(v, tc.env[v])
			}
			t.Setenv("ANTHROPIC_BASE_URL", trap.URL)
			t.Setenv("OPENAI_BASE_URL", trap.URL)
			rec := &recorder{}
			was := http.DefaultTransport
			http.DefaultTransport = rec // before the clients are built: each SDK takes the transport then
			t.Cleanup(func() { http.DefaultTransport = was })
			_, review, err := newModels()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := review.Model.Review(context.Background(), gate.Request{Model: review.ModelID, Files: map[string]string{}}); err == nil {
				t.Fatal("the recorder answers 400; the review must fail")
			}
			if len(rec.urls) != 1 || rec.urls[0] != tc.want {
				t.Fatalf("want one call to %s; got %v", tc.want, rec.urls)
			}
		})
	}
}
