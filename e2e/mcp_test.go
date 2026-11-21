//go:build stack

package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// An agent checks and deploys through canyon mcp, the binary make cli builds, as an MCP client: a tiny
// Node app reviewed clean, then live in preview, each the CLI's --format ai text alone (design/specs/mcp.md §1.1, §6).
func TestMCPChecksAndDeploysOnTheStack(t *testing.T) {
	if !ready() {
		t.Skipf("no Canyon stack answers %s/ready: start one with make e2e-up", host)
	}
	app := "e2e-mcp-" + tag
	t.Cleanup(func() { forget(t, app) })
	dir := t.TempDir()
	for name, body := range map[string]string{
		"canyon.toml":  "app = \"" + app + "\"\n",
		"package.json": `{"name": "` + app + `", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}, "engines": {"node": "22"}}` + "\n",
		"server.js":    "require(\"node:http\").createServer((_, res) => res.end(\"ok\\n\")).listen(process.env.PORT || 3000);\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := api(t, "POST", "/api/v1/projects", map[string]string{"id": app}); code != http.StatusCreated && !madeAlready(code, out) {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, code, out)
	}
	server := exec.Command(cli, "mcp") //nolint:gosec // the CLI TestMain built
	server.Dir, server.Env = dir, cliEnv(t)
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx, &mcp.CommandTransport{Command: server}, nil)
	if err != nil {
		t.Fatalf("canyon mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	for _, c := range []struct{ tool, says string }{
		{mcptools.Check, "\nCANYON REVIEW — RESULT: PASSED\n"},
		{mcptools.Deploy, "\nlive url: http://" + app + ".preview.localhost:7777\n"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool})
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		var text string
		if len(res.Content) == 1 {
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		if res.IsError || res.StructuredContent != nil || !strings.Contains("\n"+text+"\n", c.says) {
			t.Fatalf("%s: isError %v, structured %v, text:\n%s", c.tool, res.IsError, res.StructuredContent, text)
		}
	}
}
