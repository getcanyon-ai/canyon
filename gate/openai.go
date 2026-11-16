package gate

import (
	"context"
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAI is the reviewer's model on the OpenAI Chat Completions API, answering in answerSchema.
type OpenAI struct {
	client openai.Client
}

// NewOpenAI is a client with at most two SDK retries; opts (key, base URL) apply after. The DBOS
// step around the review is the retry beyond that.
func NewOpenAI(opts ...option.RequestOption) *OpenAI {
	return &OpenAI{client: openai.NewClient(append([]option.RequestOption{option.WithMaxRetries(2)}, opts...)...)}
}

// Review sends req as one chat completion and reads one strict JSON answer. Anything else — an API
// error, a refusal, a cut-off or malformed answer — is an error; no body is ever put in one.
func (o *OpenAI) Review(ctx context.Context, req Request) (Response, error) {
	files, rules := reviewPrompt(req) // files first: OpenAI caches a request's leading content on its own
	text, err := o.complete(ctx, req.Model, reviewSystem, files+rules, "review", answerSchema(req.Rules))
	if err != nil {
		return Response{}, err
	}
	resp, err := parseAnswer(text)
	if err != nil {
		return Response{}, fmt.Errorf("openai: %w", err)
	}
	return resp, nil
}

// Ask is one call outside the review (gate/custom's translation of a custom rule): system and prompt
// in, the one JSON text the model answered in schema's shape out.
func (o *OpenAI) Ask(ctx context.Context, model, system, prompt string, schema map[string]any) (string, error) {
	return o.complete(ctx, model, system, prompt, "answer", schema)
}

// complete is one chat completion's one strict JSON answer, name its schema's: an API error, a refusal
// or a cut-off answer (ErrOutOfRoom) is an error.
func (o *OpenAI) complete(ctx context.Context, model, system, prompt, name string, schema map[string]any) (string, error) {
	c, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(model),
		MaxCompletionTokens: openai.Int(reviewMaxTokens),
		Messages:            []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(prompt)},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: name, Strict: openai.Bool(true), Schema: schema},
		}},
	})
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return "", fmt.Errorf("openai: status %d %s %s (request %s)", apiErr.StatusCode, apiErr.Type, apiErr.Code, apiErr.Response.Header.Get("X-Request-Id"))
	}
	if err != nil {
		return "", fmt.Errorf("openai: %w", err)
	}
	if len(c.Choices) != 1 {
		return "", fmt.Errorf("openai: %d choices, want 1", len(c.Choices))
	}
	choice := c.Choices[0]
	if choice.FinishReason == "length" {
		return "", fmt.Errorf("openai: %w at %d tokens", ErrOutOfRoom, reviewMaxTokens)
	}
	if choice.FinishReason != "stop" {
		return "", fmt.Errorf("openai: finished for %q, not stop", choice.FinishReason)
	}
	if choice.Message.Refusal != "" {
		return "", errors.New("openai: the model refused")
	}
	return choice.Message.Content, nil
}
