// Package mcptools is Canyon's MCP surface as data: each tool's name, arguments, result schema and
// annotations, the resources and the instructions' preamble (design/specs/mcp.md §1). The local
// server (canyon mcp) and the remote one (/mcp) both serve these, so the two can't drift apart.
package mcptools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/contracts"
)

// The tools' names, the same on both servers.
const (
	Rules  = "canyon_rules"
	Status = "canyon_status"
	Logs   = "canyon_logs"
)

// The resources (§1.3): the manifest, and the manifest with one app's additions.
const (
	ManifestURI         = "canyon://manifest"
	ManifestAppTemplate = "canyon://manifest/{app}"
)

// Preamble opens the server's instructions, before the org's manifest (EXPERIENCE.md D5).
const Preamble = "Canyon reviews this org's apps and runs the ones that pass. Its tools: canyon_rules reads these rules again, " +
	"with app for an app's additions; canyon_status says what an app runs and what it needs to reach production; " +
	"canyon_logs reads an app's last lines, with each secret's value shown as its name. Where a tool isn't loaded, the canyon CLI does the same."

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

var appArg = &jsonschema.Schema{Type: "string", Description: "The app's id. Omitted: the app canyon.toml names, where there is one."}

func object(props map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}

// Tools are the read tools, as both servers list them.
func Tools() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:         Rules,
			Title:        "Canyon's rules",
			Description:  "The org's rules for every app Canyon runs, and with app, that app's additions: what blocks a deploy and how to pass it.",
			InputSchema:  object(map[string]*jsonschema.Schema{"app": appArg}),
			OutputSchema: manifestSchema(),
			Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
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
	}
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

// manifestSchema is the skill manifest's schema, guardrails.schema.json#/properties/manifest, with the
// definitions it refers to: a tool's outputSchema must stand alone.
func manifestSchema() map[string]any {
	var root struct {
		Schema      string                     `json:"$schema"`
		Definitions map[string]json.RawMessage `json:"definitions"`
		Properties  map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(contracts.GuardrailsSchema, &root); err != nil {
		panic(err) // embedded, and checked by contracts/validate.py: a bad file is a build that can't ship
	}
	var out map[string]any
	if err := json.Unmarshal(root.Properties["manifest"], &out); err != nil || out == nil {
		panic(errors.Join(errors.New("guardrails.schema.json has no manifest"), err))
	}
	out["$schema"], out["definitions"] = root.Schema, root.Definitions
	return out
}

func ptr[T any](v T) *T { return &v }
