// mcpcall is an MCP client for the tour check (scripts/tour-check.sh): it starts the server a coding agent's
// config names, calls one tool as an agent would, and prints the tool's text as the agent reads it.
//
//	go run ./internal/tools/mcpcall TOOL 'ARGUMENTS-AS-JSON' -- COMMAND [ARGS...]
//
// It exits as the CLI verb the tool stands in for: 0, 2 for a verdict that blocked, 3 for a run still going,
// 1 for a tool error or anything it could not do.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	code, err := call(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpcall:", err)
	}
	os.Exit(code)
}

func call(args []string) (int, error) {
	if len(args) < 4 || args[2] != "--" {
		return 1, fmt.Errorf("usage: mcpcall TOOL 'ARGUMENTS-AS-JSON' -- COMMAND [ARGS...]")
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(args[1]), &arguments); err != nil {
		return 1, fmt.Errorf("the arguments are not a JSON object: %w", err)
	}
	ctx := context.Background()
	server := exec.Command(args[3], args[4:]...) //nolint:gosec // the server the caller's agent config names
	server.Stderr = os.Stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tour-check"}, nil).Connect(ctx, &mcp.CommandTransport{Command: server}, nil)
	if err != nil {
		return 1, err
	}
	defer func() { _ = session.Close() }()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: args[0], Arguments: arguments})
	if err != nil {
		return 1, err
	}
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			fmt.Print(text.Text)
		}
	}
	var verdict struct{ State string }
	structured, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(structured, &verdict) // a tool with no verdict has no state
	switch {
	case res.IsError:
		return 1, nil
	case verdict.State == "blocked":
		return 2, nil
	case verdict.State == "running":
		return 3, nil
	}
	return 0, nil
}
