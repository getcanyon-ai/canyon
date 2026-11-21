package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// playbookRead is one of the org's playbooks as GET /api/v1/playbooks/:slug answers it (contracts/routes.md, the
// playbooks row).
type playbookRead struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	When    string `json:"when"`
	Body    string `json:"body"`
	Version int    `json:"version"`
}

// noPlaybook is the line for a slug that names no playbook that stands (design/EXPERIENCE.md, D5).
const noPlaybook = "No playbook %s in this org. canyon manifest lists its playbooks."

// fetchPlaybook reads a playbook's newest version, counted as the way read names (cli: canyon playbook; sync: the
// session hook's native skill, which counts a sync and no read).
func fetchPlaybook(ctx context.Context, c *Client, slug, read string) (playbookRead, json.RawMessage, error) {
	var raw json.RawMessage
	var envelope struct {
		Playbook playbookRead `json:"playbook"`
	}
	err := c.get(ctx, "/api/v1/playbooks/"+url.PathEscape(slug)+"?read="+read, &raw)
	if err == nil {
		err = json.Unmarshal(raw, &envelope)
	}
	return envelope.Playbook, raw, err
}

// runPlaybook prints one of the org's playbooks (design/specs/mcp.md §3.3; cli.md §6.16): its body as its admin wrote
// it, for an agent whose harness has no MCP, and for a finding whose rule points at it. Every member reads it.
func runPlaybook(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) != 1 {
		return a.usage(errors.New("playbook takes one playbook's slug: canyon playbook service-conventions. canyon manifest lists them"))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	p, raw, err := fetchPlaybook(ctx, client, rest[0], "cli")
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		a.errf("\n  %s %s\n", a.render.glyph(stateFail), fmt.Sprintf(noPlaybook, cell(rest[0])))
		return exitError
	}
	if err != nil {
		return a.fail(err)
	}
	if a.render.Format == FormatJSON {
		return a.json(raw)
	}
	body := visible(p.Body) // the host refuses a control character in a body; a newline and a tab are its lines
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	_, _ = fmt.Fprint(a.out, body)
	return exitOK
}
