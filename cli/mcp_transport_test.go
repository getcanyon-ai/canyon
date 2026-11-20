package cli

import (
	"slices"
	"testing"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// Every request canyon mcp makes says so (design/specs/mcp.md §7): the manifest it reads for its instructions and
// each tool call's send Canyon-Transport: mcp-local, and the CLI's own verbs send none, which the host counts as the
// CLI's.
func TestMCPTransport(t *testing.T) {
	h := &mcpHost{}
	cs, env := mcpSession(t, h.serve(t))
	if res, text := callTool(t, cs, mcptools.Status, nil); res.IsError {
		t.Fatalf("canyon_status: %s", text)
	}
	h.mu.Lock()
	sent := slices.Clone(h.transports)
	h.transports = nil
	h.mu.Unlock()
	if len(sent) == 0 || slices.ContainsFunc(sent, func(s string) bool { return s != transportMCPLocal }) {
		t.Errorf("canyon mcp sent Canyon-Transport %q, want mcp-local on every request", sent)
	}

	if _, _, code := run(t, env, false, "status", "--format", "json"); code != exitOK {
		t.Fatalf("canyon status exited %d", code)
	}
	h.mu.Lock()
	sent = slices.Clone(h.transports)
	h.mu.Unlock()
	if len(sent) == 0 || slices.ContainsFunc(sent, func(s string) bool { return s != "" }) {
		t.Errorf("canyon status sent Canyon-Transport %q, want none", sent)
	}
}
