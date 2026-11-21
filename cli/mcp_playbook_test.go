package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getcanyon-ai/canyon/internal/mcptools"
)

// canyon_playbook answers a playbook's body, counted as a read by the tool, with the playbook whole as structured
// content; canyon://playbooks/<slug> answers the same body, counted by the resource; a slug that names none is canyon
// playbook's own line, and a slug that is no slug never reaches the host (design/specs/mcp.md §3.3).
func TestPlaybookThroughMCP(t *testing.T) {
	want := playbookFixture(t) // before the directory moves
	var golden struct {
		Playbook playbookRead `json:"playbook"`
	}
	if err := json.Unmarshal(want, &golden); err != nil {
		t.Fatal(err)
	}
	h := &mcpHost{}
	cs, _ := mcpSession(t, h.serve(t))
	res, text := callTool(t, cs, mcptools.Playbook, map[string]any{"slug": "service-conventions"})
	structured, _ := json.Marshal(res.StructuredContent)
	if res.IsError || text != golden.Playbook.Body || !strings.Contains(string(structured), `"version":3`) {
		t.Fatalf("isError %v, text:\n%s\nstructured %s", res.IsError, text, structured)
	}
	read, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: mcptools.PlaybookURI + "service-conventions"})
	if err != nil || len(read.Contents) != 1 || read.Contents[0].Text != golden.Playbook.Body || read.Contents[0].MIMEType != "text/markdown" {
		t.Fatalf("the resource: %+v, %v", read, err)
	}
	if res, text := callTool(t, cs, mcptools.Playbook, map[string]any{"slug": "no-such"}); !res.IsError || text != "No playbook no-such in this org. canyon manifest lists its playbooks." {
		t.Errorf("no such playbook: isError %v, %q", res.IsError, text)
	}
	if res, _ := callTool(t, cs, mcptools.Playbook, map[string]any{"slug": "../projects"}); !res.IsError {
		t.Error("a slug that is no slug was read")
	}
	for _, uri := range []string{mcptools.PlaybookURI + "..", mcptools.PlaybookURI + "../projects", mcptools.PlaybookURI} {
		if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri}); err == nil {
			t.Errorf("%s was read", uri)
		}
	}
	var reads []string
	for _, r := range h.requests() {
		if strings.HasPrefix(r, "GET /api/v1/playbooks/") {
			reads = append(reads, r)
		}
	}
	if want := []string{"GET /api/v1/playbooks/service-conventions?read=tool", "GET /api/v1/playbooks/service-conventions?read=resource",
		"GET /api/v1/playbooks/no-such?read=tool"}; !slices.Equal(reads, want) {
		t.Errorf("asked %q, want %q", reads, want)
	}
}

// A body holding a character that hides text reaches no agent path raw: the tool and the resource write it visibly
// (design/specs/mcp.md §3.3), as canyon playbook does.
func TestPlaybookThroughMCPHidesNothing(t *testing.T) {
	body := "ok\U000E0041\u202Eevil\n"
	quoted, _ := json.Marshal(body)
	h := &mcpHost{playbook: []byte(`{"playbook":{"slug":"service-conventions","name":"n","kind":"skill","when":"w","body":` + string(quoted) + `,"version":1}}`)}
	cs, _ := mcpSession(t, h.serve(t))
	_, text := callTool(t, cs, mcptools.Playbook, map[string]any{"slug": "service-conventions"})
	read, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: mcptools.PlaybookURI + "service-conventions"})
	if err != nil {
		t.Fatal(err)
	}
	for way, got := range map[string]string{"tool": text, "resource": read.Contents[0].Text} {
		if got != "ok\\U000e0041\\u202eevil\n" {
			t.Errorf("the %s handed %q", way, got)
		}
	}
}
