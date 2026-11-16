package gate

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic is the reviewer's model on the Claude Messages API, answering in answerSchema.
type Anthropic struct {
	client anthropic.Client
}

// NewAnthropic is a client with at most two SDK retries; opts (key, base URL) apply after. The
// DBOS step around the review is the retry beyond that.
func NewAnthropic(opts ...option.RequestOption) *Anthropic {
	return &Anthropic{client: anthropic.NewClient(append([]option.RequestOption{option.WithMaxRetries(2)}, opts...)...)}
}

// Review sends req as one message and reads one JSON answer. Anything else — an API error, a
// refusal, a cut-off or malformed answer — is an error; no body is ever put in one. The files are
// the message's first block, marked a cache prefix the review's other call reads back.
func (a *Anthropic) Review(ctx context.Context, req Request) (Response, error) {
	files, rules := reviewPrompt(req)
	var content []anthropic.ContentBlockParamUnion
	if files != "" { // the API refuses an empty text block
		content = append(content, anthropic.ContentBlockParamUnion{OfText: &anthropic.TextBlockParam{Text: files, CacheControl: anthropic.NewCacheControlEphemeralParam()}})
	}
	content = append(content, anthropic.NewTextBlock(rules))
	text, err := a.answer(ctx, req.Model, reviewSystem, content, answerSchema(req.Rules))
	if err != nil {
		return Response{}, err
	}
	resp, err := parseAnswer(text)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: %w", err)
	}
	return resp, nil
}

// Ask is one call outside the review (gate/custom's translation of a custom rule): system and prompt
// in, the one JSON text the model answered in schema's shape out.
func (a *Anthropic) Ask(ctx context.Context, model, system, prompt string, schema map[string]any) (string, error) {
	return a.answer(ctx, model, system, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(prompt)}, schema)
}

// answer streams one message and is its one text block: an API error, a stop other than end_turn
// (ErrOutOfRoom at the room) or any other number of text blocks is an error.
func (a *Anthropic) answer(ctx context.Context, model, system string, content []anthropic.ContentBlockParamUnion, schema map[string]any) (string, error) {
	stream := a.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:        anthropic.Model(model),
		MaxTokens:    reviewMaxTokens,
		System:       []anthropic.TextBlockParam{{Text: system}},
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(content...)},
		OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: schema}},
	})
	defer func() { _ = stream.Close() }()
	var msg anthropic.Message // streamed, so the answer's room is reviewMaxTokens, not the SDK's non-streaming cap
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return "", fmt.Errorf("anthropic: %w", err)
		}
	}
	err := stream.Err()
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return "", fmt.Errorf("anthropic: status %d %s (request %s)", apiErr.StatusCode, apiErr.Type(), apiErr.RequestID)
	}
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return "", fmt.Errorf("anthropic: %w at %d tokens", ErrOutOfRoom, reviewMaxTokens)
	}
	if msg.StopReason != anthropic.StopReasonEndTurn {
		return "", fmt.Errorf("anthropic: stopped for %q, not end_turn", msg.StopReason)
	}
	var texts []string
	for _, block := range msg.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			texts = append(texts, text.Text)
		}
	}
	if len(texts) != 1 {
		return "", fmt.Errorf("anthropic: %d text blocks, want 1", len(texts))
	}
	return texts[0], nil
}
