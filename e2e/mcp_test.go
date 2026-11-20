//go:build stack

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// An agent checks and deploys through canyon mcp, the binary make cli builds, as an MCP client: a tiny
// Node app reviewed clean with its SARIF log, then live in preview (design/specs/mcp.md §6, item 2).
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
	for _, c := range []struct{ tool, url string }{
		{mcptools.Check, ""},
		{mcptools.Deploy, "http://" + app + ".preview.localhost:7777"},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool})
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		var v struct {
			Run, State, URL string
			Findings        struct {
				Runs []json.RawMessage `json:"runs"`
			}
		}
		structured, _ := json.Marshal(res.StructuredContent)
		if json.Unmarshal(structured, &v) != nil || res.IsError || v.State != "clean" || len(v.Findings.Runs) == 0 || v.URL != c.url {
			t.Fatalf("%s: isError %v, structured %s, content %+v", c.tool, res.IsError, structured, res.Content)
		}
	}
}
