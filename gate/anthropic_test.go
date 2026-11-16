package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/getcanyon-ai/canyon/gate/finding"
)

// answer is a streamed Messages API response whose one text block is text, stopped for reason: the
// review streams, so its answer's room is not the SDK's non-streaming cap.
func answer(text, reason string) string {
	event := func(kind string, v map[string]any) string {
		v["type"] = kind
		raw, _ := json.Marshal(v)
		return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
	}
	return event("message_start", map[string]any{"message": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}) +
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}}) +
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}) +
		event("content_block_stop", map[string]any{"index": 0}) +
		event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}}) +
		event("message_stop", map[string]any{})
}

// fakeAPI stands in for api.anthropic.com: it records each request body and answers status and body.
func fakeAPI(t *testing.T, status int, body string) (*Anthropic, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewAnthropic(option.WithAPIKey("test-key"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)), &bodies
}

var anthropicReq = Request{
	Model: "claude-opus-5",
	Rules: []finding.Rule{{ID: "security.xss", Name: "Untrusted HTML is rendered"}},
	Files: map[string]string{"src/app.ts": "const key = \"" + masked + "\";\nel.innerHTML = q;\n"},
}

func TestAnthropicMapsTheAnswerToCandidates(t *testing.T) {
	want := []Candidate{{Rule: "security.xss", File: "src/app.ts", StartLine: 2, EndLine: 2, Severity: "high", Title: "HTML from the query", Problem: "q is rendered as HTML.", Fix: "Use textContent.", RequiredEndState: "No query text reaches innerHTML.", DoNot: []string{"escape the query by hand"}}}
	raw, _ := json.Marshal(map[string]any{"results": want})
	m, _ := fakeAPI(t, 200, answer(string(raw), "end_turn"))
	got, err := m.Review(context.Background(), anthropicReq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Results, want) || got.Recorded {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAnthropicNoFindingsIsAnEmptyAnswer(t *testing.T) {
	m, _ := fakeAPI(t, 200, answer(`{"results":[]}`, "end_turn"))
	got, err := m.Review(context.Background(), anthropicReq)
	if err != nil || len(got.Results) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestAnthropicFailsClosed(t *testing.T) {
	full := `{"rule":"security.xss","file":"src/app.ts","start_line":2,"end_line":2,"severity":"high","title":"t","problem":"p","fix":"f","required_end_state":"e","do_not":["d"]}`
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"rate limited":    {429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		"server error":    {500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`},
		"prose, not json": {200, answer("I found one XSS on line 2.", "end_turn")},
		"refusal":         {200, answer(`{"results":[]}`, "refusal")},
		"missing field":   {200, answer(`{"results":[{"rule":"security.xss","file":"src/app.ts","start_line":2,"end_line":2,"severity":"high","title":"t","problem":"p"}]}`, "end_turn")},
		"no results key":  {200, answer(`{}`, "end_turn")},
		"unknown field":   {200, answer(`{"results":[`+full[:len(full)-1]+`,"extra":1}]}`, "end_turn")},
		"trailing text":   {200, answer(`{"results":[]} and more`, "end_turn")},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := fakeAPI(t, tc.status, tc.body)
			if _, err := m.Review(context.Background(), anthropicReq); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// An answer cut off at its room is not an answer and not a model failure: it is ErrOutOfRoom, which the
// review reads as not checked. The room is 32,000 tokens, streamed (the test cluster: a stock Next 16 app's
// review stopped at 16,000 and failed its deploy).
func TestAnthropicOutOfRoomIsItsOwnError(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"results":[`, "max_tokens"))
	_, err := m.Review(context.Background(), anthropicReq)
	if !errors.Is(err, ErrOutOfRoom) {
		t.Fatalf("a cut-off answer: %v", err)
	}
	var sent struct {
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}
	if json.Unmarshal((*bodies)[0], &sent) != nil || sent.MaxTokens != 32000 || !sent.Stream {
		t.Fatalf("the request: %s", (*bodies)[0])
	}
}

// An API error names its status and type and never carries the body the API sent back.
func TestAnthropicErrorCarriesNoBody(t *testing.T) {
	m, _ := fakeAPI(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"echo el.innerHTML"}}`)
	_, err := m.Review(context.Background(), anthropicReq)
	if err == nil || strings.Contains(err.Error(), "innerHTML") || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicRequestCarriesModelSchemaAndMaskedSource(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"results":[]}`, "end_turn"))
	if _, err := m.Review(context.Background(), anthropicReq); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("%d requests", len(*bodies))
	}
	var body struct {
		Model        string `json:"model"`
		MaxTokens    int    `json:"max_tokens"`
		OutputConfig struct {
			Format struct {
				Type   string         `json:"type"`
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-opus-5" || body.MaxTokens <= 0 || body.OutputConfig.Format.Type != "json_schema" ||
		fmt.Sprint(body.OutputConfig.Format.Schema["required"]) != "[results]" {
		t.Fatalf("body = %s", (*bodies)[0])
	}
	if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	text := body.Messages[0].Content[0].Text + body.Messages[0].Content[1].Text
	for _, s := range []string{"security.xss", "Untrusted HTML is rendered", "src/app.ts", "1\tconst key = \"" + masked + "\";", "2\tel.innerHTML = q;"} {
		if !strings.Contains(text, s) {
			t.Errorf("user text lacks %q:\n%s", s, text)
		}
	}
}

// A tree with no source shown sends the rules alone: the API refuses an empty text block.
func TestAnthropicWithNoFilesSendsTheRulesAlone(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"results":[]}`, "end_turn"))
	if _, err := m.Review(context.Background(), Request{Model: "claude-opus-5", Rules: anthropicReq.Rules}); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 1 || len(body.Messages[0].Content) != 1 || !strings.HasPrefix(body.Messages[0].Content[0].Text, "Rules:\n") {
		t.Fatalf("body = %s", (*bodies)[0])
	}
}

// ruleEnum is the values the answer's schema lets a result's rule take, from a request body's schema.
func ruleEnum(t *testing.T, schema map[string]any) []any {
	t.Helper()
	items := schema["properties"].(map[string]any)["results"].(map[string]any)["items"].(map[string]any)
	enum, _ := items["properties"].(map[string]any)["rule"].(map[string]any)["enum"].([]any)
	return enum
}

// The answer's schema names the request's rule ids as the only rules a result may take: the model
// cannot name one outside them (#1819: a first review's one candidate did, and was dropped).
func TestAnthropicSchemaNamesOnlyTheRequestsRules(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"results":[]}`, "end_turn"))
	req := anthropicReq
	req.Rules = []finding.Rule{{ID: "auth.unauthenticated-route", Name: "Every route needs sign-in"}, {ID: "perf.unbounded-refetch", Name: "No unbounded refetch"}}
	if _, err := m.Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var body struct {
		OutputConfig struct {
			Format struct {
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(ruleEnum(t, body.OutputConfig.Format.Schema)); got != "[auth.unauthenticated-route perf.unbounded-refetch]" {
		t.Fatalf("rule enum %s", got)
	}
}

// Ask, the call outside the review, sends its system, prompt and schema as given and is the one text
// the model answered; a stop at the room is ErrOutOfRoom, as the review's is.
func TestAnthropicAskSendsWhatItIsGivenAndIsTheAnswer(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"checker":"ai"}`, "end_turn"))
	schema := map[string]any{"type": "object", "properties": map[string]any{"checker": map[string]any{"type": "string"}}}
	got, err := m.Ask(context.Background(), "claude-opus-5", "the brief", "the rule", schema)
	if err != nil || got != `{"checker":"ai"}` {
		t.Fatalf("got %q, %v", got, err)
	}
	var body struct {
		Model        string                                      `json:"model"`
		System       []struct{ Text string }                     `json:"system"`
		Messages     []struct{ Content []struct{ Text string } } `json:"messages"`
		OutputConfig struct {
			Format struct {
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-opus-5" || len(body.System) != 1 || body.System[0].Text != "the brief" || len(body.Messages) != 1 ||
		len(body.Messages[0].Content) != 1 || body.Messages[0].Content[0].Text != "the rule" || !reflect.DeepEqual(body.OutputConfig.Format.Schema, schema) {
		t.Fatalf("body = %s", (*bodies)[0])
	}
	cut, _ := fakeAPI(t, 200, answer(`{"checker":`, "max_tokens"))
	if _, err := cut.Ask(context.Background(), "claude-opus-5", "the brief", "the rule", schema); !errors.Is(err, ErrOutOfRoom) {
		t.Fatalf("a cut-off answer: %v", err)
	}
}
