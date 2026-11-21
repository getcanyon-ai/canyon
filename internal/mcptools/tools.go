// Package mcptools is Canyon's MCP surface as data: each tool's name, arguments and annotations, the resources and the instructions' preamble (design/specs/mcp.md §1). The local
// server (canyon mcp) and the remote one (/mcp) both serve these, so the two can't drift apart.
package mcptools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools' names, the same on both servers.
const (
	Rules   = "canyon_rules"
	Status  = "canyon_status"
	Logs    = "canyon_logs"
	Promote = "canyon_promote"
	Check   = "canyon_check"  // the local server's alone: it reads a local directory
	Deploy  = "canyon_deploy" // the local server's alone

	CheckRef  = "canyon_check_ref"  // the remote server's alone: it reads a linked repository's ref
	DeployRef = "canyon_deploy_ref" // the remote server's alone
)

// The resources (§1.3): the manifest, and the manifest with one app's additions.
const (
	ManifestURI         = "canyon://manifest"
	ManifestAppTemplate = "canyon://manifest/{app}"
)

// Preamble opens the server's instructions, before the org's manifest (EXPERIENCE.md D5).
const Preamble = "Canyon reviews this org's apps and runs the ones that pass. Its tools: canyon_rules reads these rules again, " +
	"with app for an app's additions; canyon_status says what an app runs and what it needs to reach production; " +
	"canyon_logs reads an app's last lines, with each secret's value shown as its name; canyon_check reviews an " +
	"app's code without deploying it; canyon_deploy reviews it and deploys it to preview; canyon_promote puts what " +
	"preview runs in production, or puts an earlier version back. Set confirm on canyon_promote only when the " +
	"person asked for production in this conversation. Never ask the person for a secret's value: they set it " +
	"themselves, with canyon secret set or on the app's page. Where a tool isn't loaded, the canyon CLI does the " +
	"same."

// Handoff is how long a check or deploy is followed before its tool answers state running, the run id and its
// page, and canyon_status reads where it ended (design/specs/mcp.md §1.1).
const Handoff = 10 * time.Minute

// MaxLogLines is the most lines canyon_logs reads: the masked read's limit (contracts/routes.md, the logs row).
const MaxLogLines = 200

// RulesArgs, StatusArgs and LogsArgs are the tools' arguments on both servers. The local server adds
// dir (Local).
type RulesArgs struct {
	App string `json:"app,omitempty"`
}

type StatusArgs struct {
	App string `json:"app,omitempty"`
}

type LogsArgs struct {
	App   string `json:"app,omitempty"`
	Env   string `json:"env,omitempty"`
	Lines int    `json:"lines,omitempty"`
}

// PromoteArgs are canyon_promote's: version and reason put an earlier version back, and confirm answers
// production's question where the client can't ask the person.
type PromoteArgs struct {
	App     string `json:"app,omitempty"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// Verdict is a check's or a deploy's structured result. Findings is the run's SARIF log, byte for byte as
// GET /gate-runs/:id/sarif serves it; URL, Answered and Access are a deploy's that went live.
type Verdict struct {
	Run           string          `json:"run"`
	State         string          `json:"state"`
	FailureReason string          `json:"failure_reason,omitempty"`
	Blocking      int             `json:"blocking"`
	Warnings      int             `json:"warnings"`
	Findings      json.RawMessage `json:"findings,omitempty"`
	URL           string          `json:"url,omitempty"`
	FleetURL      string          `json:"fleet_url,omitempty"` // a run handed off: its page, where it goes on
	Answered      json.RawMessage `json:"answered,omitempty"`
	Access        []string        `json:"access,omitempty"`
}

var appArg = &jsonschema.Schema{Type: "string", Description: "The app's id. Omitted: the app canyon.toml names, where there is one."}

func object(props map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}

// Tools are the read tools, as both servers list them. None declares an outputSchema: a client that held a
// result to one would reject a newer host's manifest in an older CLI, since the contracts are strict and grow.
func Tools() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:        Rules,
			Title:       "Canyon's rules",
			Description: "The org's rules for every app Canyon runs, and with app, that app's additions: what blocks a deploy and how to pass it.",
			InputSchema: object(map[string]*jsonschema.Schema{"app": appArg}),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		{
			Name:        Status,
			Title:       "An app's status",
			Description: "What the app runs in preview and production, its last review, and what it needs to reach production, as canyon status says.",
			InputSchema: object(map[string]*jsonschema.Schema{"app": appArg}),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		{
			Name:        Logs,
			Title:       "An app's logs",
			Description: "The app's last log lines in one environment, each secret's value shown as its name.",
			InputSchema: object(map[string]*jsonschema.Schema{
				"app":   appArg,
				"env":   {Type: "string", Enum: []any{"preview", "prod"}, Default: json.RawMessage(`"preview"`)},
				"lines": {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(float64(MaxLogLines)), Default: json.RawMessage(`200`)},
			}),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		},
		{
			Name:        Promote,
			Title:       "Promote to production",
			Description: "Puts what preview runs in production, or with version an earlier version back, as canyon deploy --env prod. It asks the person first; where it can't, it needs confirm.",
			InputSchema: object(map[string]*jsonschema.Schema{
				"app":     appArg,
				"version": {Type: "string", Description: "An earlier version to put back in production, as canyon_status lists them."},
				"reason":  {Type: "string", Description: "With version: why, an incident or change reference the audit trail records."},
				"confirm": {Type: "boolean", Description: "Set only when the person asked for production in this conversation."},
			}),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}, // it replaces what customers reach
		},
	}
}

// LocalOnly are the tools that read a local directory, which only canyon mcp serves.
func LocalOnly() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:        Check,
			Title:       "Review without deploying",
			Description: "Reviews the app's code in dir without deploying it, as canyon check: the verdict and every finding, with how to fix each.",
			InputSchema: object(map[string]*jsonschema.Schema{}),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		},
		{
			Name:        Deploy,
			Title:       "Deploy to preview",
			Description: "Reviews the app's code in dir and, when nothing blocks, builds it and deploys it to preview, as canyon deploy.",
			InputSchema: object(map[string]*jsonschema.Schema{"app": {Type: "string", Description: "The app id a first deploy writes into canyon.toml."}}),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		},
	}
}

// RemoteOnly are the tools that work from an app's linked repository, which only /mcp serves (design/specs/mcp.md §2.3).
func RemoteOnly() []*mcp.Tool {
	ref := &jsonschema.Schema{Type: "string", Description: "A branch, tag or commit of the app's linked repository."}
	return []*mcp.Tool{
		{
			Name:        CheckRef,
			Title:       "Review a ref without deploying",
			Description: "Reviews a ref of the app's linked repository without deploying it: the verdict and every finding, with how to fix each.",
			InputSchema: object(map[string]*jsonschema.Schema{"app": appArg, "ref": ref}),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		},
		{
			Name:        DeployRef,
			Title:       "Deploy a ref to preview",
			Description: "Reviews a ref of the app's linked repository and, when nothing blocks, builds it and deploys it to preview.",
			InputSchema: object(map[string]*jsonschema.Schema{"app": appArg, "ref": ref}),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		},
	}
}

// RefArgs are canyon_check_ref's and canyon_deploy_ref's.
type RefArgs struct {
	App string `json:"app"`
	Ref string `json:"ref"`
}

// Remote is t as /mcp serves it: no directory names the app there, so app is required, and ref with it where t takes one.
func Remote(t *mcp.Tool) *mcp.Tool {
	c := *t
	s := *t.InputSchema.(*jsonschema.Schema)
	s.Properties = maps.Clone(s.Properties)
	s.Properties["app"] = &jsonschema.Schema{Type: "string", Description: "The app's id."}
	s.Required = []string{"app"}
	if _, ok := s.Properties["ref"]; ok {
		s.Required = append(s.Required, "ref")
	}
	c.InputSchema = &s
	return &c
}

// Local is t with the local server's dir argument: the directory whose canyon.toml names the app.
func Local(t *mcp.Tool) *mcp.Tool {
	c := *t
	s := *t.InputSchema.(*jsonschema.Schema)
	s.Properties = maps.Clone(s.Properties)
	s.Properties["dir"] = &jsonschema.Schema{Type: "string", Description: "The app's directory. Omitted: the directory the server runs in."}
	c.InputSchema = &s
	return &c
}

// Decode reads a tool call's arguments into v, refusing any the tool's schema doesn't take.
func Decode(t *mcp.Tool, raw json.RawMessage, v any) error {
	resolved, err := t.InputSchema.(*jsonschema.Schema).Resolve(nil)
	if err != nil {
		return err
	}
	var args map[string]any
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("the arguments are not a JSON object: %w", err)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := resolved.Validate(args); err != nil {
		return err
	}
	if err := resolved.ApplyDefaults(&args); err != nil {
		return err
	}
	filled, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(filled, v)
}

func ptr[T any](v T) *T { return &v }
