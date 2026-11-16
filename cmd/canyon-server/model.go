package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
	oaoption "github.com/openai/openai-go/v3/option"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/runs"
)

// setupSystem is the model's brief, in D8's voice (design/EXPERIENCE.md): the subject is the
// org's configuration, never the person, and nothing is guessed.
const setupSystem = `You are setting up this Canyon host's org preferences.
Read the current preferences before you change anything.
Ask about the approved stacks first, then the design system, one question at a time.
Write as soon as an answer is unambiguous, and say plainly what you wrote.
If an answer is vague, ask once more; never guess a language, a framework, or a registry.
You never change a policy state: say that the Policies screen is where a state is changed.`

// model runs one setup turn: it may call `call` up to the iteration bound, and answers the
// assistant's last line. Two implementations: anthropicModel and the tests' scripted fake, which
// is what earns the interface. Nothing above this seam ever learns which provider answered.
type model interface {
	Turn(ctx context.Context, msgs []turnMessage, call toolCall) (string, error)
}

// toolCall dispatches one tool by name over the raw JSON the model sent. A refused input is a
// result text the model reads and asks again from, never an error.
type toolCall func(ctx context.Context, name string, input json.RawMessage) (string, error)

// turnMessage is one line of the conversation the client resends every turn (routes.md: the
// server stores none of it).
type turnMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// newModels is this host's two models — the setup chat's and the AI review's — or a nil setup
// model and a Gate with no Model when the admin configured none: then GET /preferences answers
// setup_available: false and ai_review: false, and every review shows as skipped. The model is
// host configuration (design/EXPERIENCE.md D10), set once by whoever runs the host, never per laptop:
//
//	CANYON_MODEL_PROVIDER  anthropic (the default), openai (the AI review only), bedrock, vertex
//	CANYON_MODEL           the id as that provider names it, for the setup chat and the review;
//	                       Canyon never maps ids, because a mapping table goes stale every launch.
//	                       Default claude-opus-5 on anthropic; required on openai.
//	CANYON_MODEL_URL       a gateway, a proxy, or an internal endpoint speaking the provider's API
//	ANTHROPIC_API_KEY      the key, for the Claude API and for a gateway that wants one
//	CANYON_MODEL_TOKEN     a bearer token from the IdP, when the gateway holds the provider key
//	                       and this host holds none
//	OPENAI_API_KEY         the key on openai, read only there
//
// Naming any of the first five is asking for a model, and an ask this build cannot serve fails the
// boot with the reason rather than quietly serving a guardrails screen with no setup panel, or a
// review that is skipped. Silence on all five is the only way to get no model.
func newModels() (model, runs.Gate, error) {
	provider, id := os.Getenv("CANYON_MODEL_PROVIDER"), os.Getenv("CANYON_MODEL")
	url, key, token := os.Getenv("CANYON_MODEL_URL"), os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CANYON_MODEL_TOKEN")
	if provider == "" && id == "" && url == "" && key == "" && token == "" {
		return nil, runs.Gate{}, nil
	}
	// Checked at boot, so a typo is not the first turn's 502. The value is never in the error: the
	// thing that makes an address invalid may be the credential in it.
	if url != "" && !modelURL(url) {
		return nil, runs.Gate{}, errors.New("CANYON_MODEL_URL is not an address Canyon will call: it is http or https with a host, carries no credential, and is at most 2048 bytes")
	}
	switch provider {
	case "", "anthropic":
	case "openai":
		// The setup chat speaks the Claude tool-call shape (canyonv2-z9d), so on openai it is off and
		// the review alone is served. The base URL is always named: the SDK would otherwise take
		// OPENAI_BASE_URL from the environment, and the source would go somewhere the host did not name.
		openAIKey := os.Getenv("OPENAI_API_KEY")
		if openAIKey == "" {
			return nil, runs.Gate{}, errors.New("CANYON_MODEL_PROVIDER=openai and no OPENAI_API_KEY: set it, or unset the provider to have no AI review")
		}
		if id == "" {
			return nil, runs.Gate{}, errors.New("CANYON_MODEL_PROVIDER=openai names no model: set CANYON_MODEL to the id as OpenAI names it")
		}
		review := gate.NewOpenAI(oaoption.WithBaseURL(cmp.Or(url, "https://api.openai.com/v1")), oaoption.WithAPIKey(openAIKey))
		return nil, runs.Gate{Model: review, ModelID: id}, nil
	case "bedrock", "vertex":
		return nil, runs.Gate{}, fmt.Errorf("CANYON_MODEL_PROVIDER=%s needs cloud SDKs this build does not carry; a gateway in front of it works today with CANYON_MODEL_URL", provider)
	default:
		return nil, runs.Gate{}, fmt.Errorf("CANYON_MODEL_PROVIDER=%s is not a provider Canyon knows", provider)
	}
	// Configured is a credential that resolves, not a name that is spelled. A host that names an
	// id and no way to reach a provider would otherwise offer a setup panel whose every turn is a
	// 502 — the boot is where an operator is still reading the log.
	if key != "" && token != "" {
		return nil, runs.Gate{}, errors.New("ANTHROPIC_API_KEY and CANYON_MODEL_TOKEN are both set: the host must say which one a turn carries")
	}
	if url == "" && key == "" && token == "" {
		return nil, runs.Gate{}, errors.New("a model is named but no credential resolves: set ANTHROPIC_API_KEY, or CANYON_MODEL_URL with CANYON_MODEL_TOKEN for a gateway that holds the key")
	}
	// Without WithoutEnvironmentDefaults the SDK would also read ANTHROPIC_BASE_URL,
	// ANTHROPIC_AUTH_TOKEN and the profile files, and a turn or a review could go somewhere the
	// host's own configuration does not name; the base URL is named for the same reason.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithBaseURL(cmp.Or(url, "https://api.anthropic.com"))}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	} else if token != "" {
		opts = append(opts, option.WithAuthToken(token)) // Authorization: Bearer — the gateway holds the provider key
	}
	// No retry on the setup chat: the SDK's default would replay a request whose tool calls have
	// already written the org's row. The review is read-only, and keeps gate.NewAnthropic's two.
	id = cmp.Or(id, "claude-opus-5")
	setup := &anthropicModel{client: anthropic.NewClient(append(opts, option.WithMaxRetries(0))...), id: id}
	return setup, runs.Gate{Model: gate.NewAnthropic(opts...), ModelID: id}, nil
}

// modelURL is the rule for the gateway address, and it is deliberately not the registry's rule. A
// registry URL is where an agent installs code from, so it is held to https; a gateway is operator
// configuration at the same trust tier as the key beside it, an in-cluster service name over plain
// http is the normal enterprise shape, and a passthrough lives under a path. What is refused is
// userinfo: a credential belongs in ANTHROPIC_API_KEY or CANYON_MODEL_TOKEN, not in an address
// that a log or an error could carry.
func modelURL(s string) bool {
	if len(s) > 2048 {
		return false
	}
	// The scheme is matched as written, before url.Parse lowercases it.
	if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
		return false
	}
	u, err := neturl.Parse(s)
	return err == nil && u.Hostname() != "" && u.User == nil
}

type anthropicModel struct {
	client anthropic.Client
	id     string
}

// The two tools' wire shapes: the name and the schema the model is shown, and nothing else. The
// behaviour is the toolCall its caller hands Turn, which is what lets the tests' fake drive
// exactly the closures the SDK drives.
type readPreferencesInput struct{}

type setStacksInput struct {
	Stacks []stack `json:"stacks" jsonschema:"required,description=the whole approved list; this replaces it"`
}

type listDesignSystemsInput struct{}

// setDesignSystemInput carries both of the tool's shapes, so none of its fields is required: an
// id selects a row the registry already carries, and the other four register one and select it.
type setDesignSystemInput struct {
	ID          string `json:"id,omitempty" jsonschema:"description=the id of a row list_design_systems answered; send it alone"`
	Name        string `json:"name,omitempty" jsonschema:"description=a new design system's name; with namespace and registry_url it is registered and then selected"`
	Namespace   string `json:"namespace,omitempty" jsonschema:"description=the package name an app declares for it"`
	RegistryURL string `json:"registry_url,omitempty" jsonschema:"description=where its components are installed from"`
	TokensURL   string `json:"tokens_url,omitempty" jsonschema:"description=where its design tokens are; optional"`
}

type proposePolicyChangeInput struct {
	Rule      string `json:"rule" jsonschema:"required,description=a rule id the policy list carries"`
	State     string `json:"state" jsonschema:"required,enum=off,enum=advisory,enum=blocking,description=the state to preview the rule at"`
	GraceDays int    `json:"grace_days,omitempty" jsonschema:"description=days before the change would bite; 0 to 365"`
}

func (m *anthropicModel) Turn(ctx context.Context, msgs []turnMessage, call toolCall) (string, error) {
	read, readErr := sdkTool[readPreferencesInput]("read_current_preferences", "The org's approved stacks and default design system, as they stand now.", call)
	set, setErr := sdkTool[setStacksInput]("set_stacks", "Replace the org's approved languages and their frameworks.", call)
	list, listErr := sdkTool[listDesignSystemsInput]("list_design_systems", "The design systems this org can choose from: the shipped rows and every one it has registered.", call)
	choose, chooseErr := sdkTool[setDesignSystemInput]("set_design_system", "Set the org's default design system, registering it first when the registry does not carry it yet.", call)
	propose, proposeErr := sdkTool[proposePolicyChangeInput]("propose_policy_change", "Preview what changing a policy's state would block today: the projects whose newest verdict carries findings the rule would hold at blocking, with a count each. This applies nothing and writes nothing; an admin applies the change on the policy list, and it is attributed to them.", call)
	if err := errors.Join(readErr, setErr, listErr, chooseErr, proposeErr); err != nil {
		return "", err
	}
	in := make([]anthropic.BetaMessageParam, len(msgs))
	for i, msg := range msgs {
		// The SDK has NewBetaUserMessage but no assistant constructor: the literal carries either role.
		in[i] = anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRole(msg.Role),
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(msg.Content)},
		}
	}
	runner := m.client.Beta.Messages.NewToolRunner([]anthropic.BetaTool{read, set, list, choose, propose}, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(m.id),
			MaxTokens: 16000,
			System:    []anthropic.BetaTextBlockParam{{Text: setupSystem}},
			Messages:  in,
			// Thinking is left unset: on Claude Opus 5 that is adaptive thinking already.
		},
		MaxIterations: 8, // past it RunToCompletion answers the last message, which is routes.md's "the model's last line"
	})
	out, err := runner.RunToCompletion(ctx)
	if err != nil || out == nil {
		return "", err // a nil message with no error is an empty line, which the handler answers as the fixed 502
	}
	var line strings.Builder
	for _, block := range out.Content {
		if text, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			line.WriteString(text.Text)
		}
	}
	return line.String(), nil
}

// sdkTool builds one SDK tool whose handler hands the model's typed input to the dispatcher as
// the raw JSON it was given, so one dispatcher validates every call whoever made it.
func sdkTool[T any](name, description string, call toolCall) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema(name, description,
		func(ctx context.Context, in T) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			raw, err := json.Marshal(in)
			if err != nil {
				return anthropic.BetaToolResultBlockParamContentUnion{}, err
			}
			text, err := call(ctx, name, raw)
			if err != nil {
				return anthropic.BetaToolResultBlockParamContentUnion{}, err
			}
			return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: text}}, nil
		})
}
