package gate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/getcanyon-ai/canyon/gate/finding"
)

// completion is a Chat Completions response with one choice per message, each ending for reason.
func completion(reason string, messages ...map[string]any) string {
	choices := []map[string]any{}
	for i, m := range messages {
		m["role"] = "assistant"
		choices = append(choices, map[string]any{"index": i, "finish_reason": reason, "logprobs": nil, "message": m})
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "gpt-test", "choices": choices,
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	return string(raw)
}

// content is a message whose content is text and whose refusal is null.
func content(text string) map[string]any {
	return map[string]any{"content": text, "refusal": nil}
}

// fakeOpenAI stands in for api.openai.com: it records each request body and answers status and body.
func fakeOpenAI(t *testing.T, status int, body string) (*OpenAI, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_test")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewOpenAI(option.WithAPIKey("test-key"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)), &bodies
}

var openAIReq = Request{
	Model: "gpt-test",
	Rules: []finding.Rule{{ID: "security.xss", Name: "Untrusted HTML is rendered"}},
	Files: map[string]string{"src/app.ts": "const key = \"" + masked + "\";\nel.innerHTML = q;\n"},
}

func TestOpenAIMapsTheAnswerToCandidates(t *testing.T) {
	want := []Candidate{{Rule: "security.xss", File: "src/app.ts", StartLine: 2, EndLine: 2, Severity: "high", Title: "HTML from the query", Problem: "q is rendered as HTML.", Fix: "Use textContent.", RequiredEndState: "No query text reaches innerHTML.", DoNot: []string{"escape the query by hand"}}}
	raw, _ := json.Marshal(map[string]any{"results": want})
	m, _ := fakeOpenAI(t, 200, completion("stop", content(string(raw))))
	got, err := m.Review(context.Background(), openAIReq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Results, want) || got.Recorded {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestOpenAINoFindingsIsAnEmptyAnswer(t *testing.T) {
	m, _ := fakeOpenAI(t, 200, completion("stop", content(`{"results":[]}`)))
	got, err := m.Review(context.Background(), openAIReq)
	if err != nil || len(got.Results) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestOpenAIFailsClosed(t *testing.T) {
	full := `{"rule":"security.xss","file":"src/app.ts","start_line":2,"end_line":2,"severity":"high","title":"t","problem":"p","fix":"f","required_end_state":"e","do_not":["d"]}`
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"rate limited":    {429, `{"error":{"message":"slow down","type":"requests","param":null,"code":"rate_limit_exceeded"}}`},
		"server error":    {500, `{"error":{"message":"boom","type":"server_error","param":null,"code":null}}`},
		"prose, not json": {200, completion("stop", content("I found one XSS on line 2."))},
		"content filter":  {200, completion("content_filter", content(`{"results":[]}`))},
		"refusal":         {200, completion("stop", map[string]any{"content": `{"results":[]}`, "refusal": "I can't help with that."})},
		"no choices":      {200, completion("stop")},
		"two choices":     {200, completion("stop", content(`{"results":[]}`), content(`{"results":[]}`))},
		"missing field":   {200, completion("stop", content(`{"results":[{"rule":"security.xss","file":"src/app.ts","start_line":2,"end_line":2,"severity":"high","title":"t","problem":"p"}]}`))},
		"no results key":  {200, completion("stop", content(`{}`))},
		"unknown field":   {200, completion("stop", content(`{"results":[`+full[:len(full)-1]+`,"extra":1}]}`))},
		"trailing text":   {200, completion("stop", content(`{"results":[]} and more`))},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := fakeOpenAI(t, tc.status, tc.body)
			if _, err := m.Review(context.Background(), openAIReq); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// A completion cut off at its room ("length") is ErrOutOfRoom, which the review reads as not checked.
func TestOpenAIOutOfRoomIsItsOwnError(t *testing.T) {
	m, bodies := fakeOpenAI(t, 200, completion("length", content(`{"results":[]}`)))
	if _, err := m.Review(context.Background(), openAIReq); !errors.Is(err, ErrOutOfRoom) {
		t.Fatalf("a cut-off completion: %v", err)
	}
	var sent struct {
		Max int `json:"max_completion_tokens"`
	}
	if json.Unmarshal((*bodies)[0], &sent) != nil || sent.Max != 32000 {
		t.Fatalf("the request: %s", (*bodies)[0])
	}
}

// An API error names its status, type, code and request id and never carries the body sent back.
func TestOpenAIErrorCarriesNoBody(t *testing.T) {
	m, _ := fakeOpenAI(t, 400, `{"error":{"message":"echo el.innerHTML","type":"invalid_request_error","param":"messages","code":"bad_schema"}}`)
	_, err := m.Review(context.Background(), openAIReq)
	if err == nil || strings.Contains(err.Error(), "innerHTML") {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{"400", "invalid_request_error", "bad_schema", "req_test"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err %q lacks %q", err, s)
		}
	}
}

func TestOpenAIRequestCarriesModelStrictSchemaAndMaskedSource(t *testing.T) {
	m, bodies := fakeOpenAI(t, 200, completion("stop", content(`{"results":[]}`)))
	if _, err := m.Review(context.Background(), openAIReq); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("%d requests", len(*bodies))
	}
	var body struct {
		Model               string `json:"model"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
		Stream              bool   `json:"stream"`
		ResponseFormat      struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string `json:"name"`
				Strict bool   `json:"strict"`
				Schema struct {
					Required   []string `json:"required"`
					Properties struct {
						Results struct {
							Items struct {
								Required []string `json:"required"`
							} `json:"items"`
						} `json:"results"`
					} `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	rf := body.ResponseFormat
	if body.Model != "gpt-test" || body.MaxCompletionTokens <= 0 || body.Stream || rf.Type != "json_schema" || rf.JSONSchema.Name == "" || !rf.JSONSchema.Strict {
		t.Fatalf("body = %s", (*bodies)[0])
	}
	props := rf.JSONSchema.Schema.Properties
	if !slices.Equal(rf.JSONSchema.Schema.Required, []string{"results"}) || !slices.Equal(props.Results.Items.Required, []string{"rule", "file", "start_line", "end_line", "severity", "title", "problem", "fix", "required_end_state", "do_not"}) {
		t.Fatalf("schema = %s", (*bodies)[0])
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v", body.Messages)
	}
	text := body.Messages[1].Content
	for _, s := range []string{"security.xss", "Untrusted HTML is rendered", "src/app.ts", "1\tconst key = \"" + masked + "\";", "2\tel.innerHTML = q;"} {
		if !strings.Contains(text, s) {
			t.Errorf("user text lacks %q:\n%s", s, text)
		}
	}
}

// The strict schema names the request's rule ids as the only rules a result may take (#1819).
func TestOpenAISchemaNamesOnlyTheRequestsRules(t *testing.T) {
	m, bodies := fakeOpenAI(t, 200, completion("stop", content(`{"results":[]}`)))
	if _, err := m.Review(context.Background(), openAIReq); err != nil {
		t.Fatal(err)
	}
	var body struct {
		ResponseFormat struct {
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	want := make([]any, len(openAIReq.Rules))
	for i, r := range openAIReq.Rules {
		want[i] = r.ID
	}
	if got := ruleEnum(t, body.ResponseFormat.JSONSchema.Schema); !reflect.DeepEqual(got, want) {
		t.Fatalf("rule enum %v, want %v", got, want)
	}
}

// Ask, the call outside the review, sends its system, prompt and schema, strict, and is the one text
// the model answered; a refusal is an error.
func TestOpenAIAskSendsWhatItIsGivenAndIsTheAnswer(t *testing.T) {
	m, bodies := fakeOpenAI(t, 200, completion("stop", content(`{"checker":"ai"}`)))
	schema := map[string]any{"type": "object", "properties": map[string]any{"checker": map[string]any{"type": "string"}}}
	got, err := m.Ask(context.Background(), "gpt-test", "the brief", "the rule", schema)
	if err != nil || got != `{"checker":"ai"}` {
		t.Fatalf("got %q, %v", got, err)
	}
	var body struct {
		Model          string `json:"model"`
		ResponseFormat struct {
			JSONSchema struct {
				Strict bool           `json:"strict"`
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "gpt-test" || !body.ResponseFormat.JSONSchema.Strict || !reflect.DeepEqual(body.ResponseFormat.JSONSchema.Schema, schema) ||
		len(body.Messages) != 2 || body.Messages[0].Content != "the brief" || body.Messages[1].Content != "the rule" {
		t.Fatalf("body = %s", (*bodies)[0])
	}
	refused, _ := fakeOpenAI(t, 200, completion("stop", map[string]any{"content": nil, "refusal": "no"}))
	if _, err := refused.Ask(context.Background(), "gpt-test", "the brief", "the rule", schema); err == nil {
		t.Fatal("a refusal is an answer")
	}
}
