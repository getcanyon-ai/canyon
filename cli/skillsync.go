package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The native skill sync (design/specs/mcp.md §3.3): each of the org's playbooks that is a skill becomes a real skill in
// the harness's own directory, canyon-<slug>/SKILL.md, synced by the session hook at each session start. It writes,
// updates and removes only the canyon-* directories it wrote, each marked by a .canyon file naming the host and the
// version: a directory without that marker is never touched, so no other skill is. A note is never written to disk.

// skillMarker is the file in each directory the sync wrote: what tells the sync it is its own.
const skillMarker = ".canyon"

// syncedPrefix starts each synced skill's directory name.
const syncedPrefix = "canyon-"

// marker is a synced skill's .canyon: the host it came from and the version it holds.
type marker struct {
	Host    string `json:"host"`
	Version int    `json:"version"`
}

// skillsDir is where a harness reads its skills: Claude Code's ~/.claude/skills and Codex's ~/.agents/skills (cli.md
// §7.1); "" for a harness with none, which gets no file.
func skillsDir(home, harness string) string {
	switch {
	case home == "":
		return ""
	case harness == targetClaude:
		return filepath.Join(home, ".claude", "skills")
	case harness == targetCodex:
		return filepath.Join(home, ".agents", "skills")
	}
	return ""
}

// syncedSkills is each canyon-* directory under dir whose marker names host, by slug, with the version it holds.
func syncedSkills(dir, host string) map[string]int {
	held := map[string]int{}
	for slug, m := range marked(dir) {
		if m.Host == host {
			held[slug] = m.Version
		}
	}
	return held
}

// marked is every canyon-* directory under dir that carries a marker, by slug, whichever host wrote it.
func marked(dir string) map[string]marker {
	out := map[string]marker{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		slug, ok := strings.CutPrefix(e.Name(), syncedPrefix)
		if !ok || !e.IsDir() || slug == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), skillMarker)) //nolint:gosec // a directory this sync names
		var m marker
		if err != nil || json.Unmarshal(raw, &m) != nil || m.Host == "" || m.Version < 1 {
			continue
		}
		out[slug] = m
	}
	return out
}

// heldQuery is the manifest's held: each synced skill as <slug>@<version>, sorted, so the host counts this session's
// machine (contracts/routes.md, GET /skill/manifest).
func heldQuery(held map[string]int) string {
	var parts []string
	for slug, v := range held {
		parts = append(parts, slug+"@"+strconv.Itoa(v))
	}
	slices.Sort(parts)
	return url.QueryEscape(strings.Join(parts, ","))
}

// skillFile is a playbook as a skill: its slug the name, its when the description, then its body. Both values are JSON
// strings, which YAML reads as they are, so no text of the admin's can end the frontmatter.
func skillFile(p playbookRead) []byte {
	name, _ := json.Marshal(p.Slug)
	description, _ := json.Marshal(p.When)
	return []byte("---\nname: " + string(name) + "\ndescription: " + string(description) + "\n---\n\n" + visible(p.Body))
}

// syncSkills makes dir's canyon-* skills the host's: each skill in playbooks whose version the directory doesn't hold
// is fetched (counted as a sync, not a read) and written, its marker last; each this host's sync wrote that is no
// longer a skill is removed. It gives up once left has passed, and a failure leaves the directory as it was for that
// skill: the next session tries again. A session hook never fails a session.
func syncSkills(ctx context.Context, c *Client, dir string, playbooks []handedPlaybook, held map[string]int, left time.Duration) {
	if dir == "" {
		return
	}
	until := time.Now().Add(left)
	skills := map[string]bool{}
	for _, p := range playbooks {
		if p.Kind != "skill" {
			continue
		}
		skills[p.Slug] = true
		if held[p.Slug] == p.Version || !playbookSlug(p.Slug) {
			continue
		}
		within := time.Until(until)
		if within <= 0 {
			return
		}
		var envelope struct {
			Playbook playbookRead `json:"playbook"`
		}
		if c.do(ctx, http.MethodGet, "/api/v1/playbooks/"+url.PathEscape(p.Slug)+"?read=sync", "", nil, 0, nil, http.StatusOK, within, answerLimit, &envelope) != nil ||
			envelope.Playbook.Slug != p.Slug {
			continue
		}
		at := filepath.Join(dir, syncedPrefix+p.Slug)
		if err := os.MkdirAll(at, 0o755); err != nil { //nolint:gosec // a harness's skill directory, readable as its others are
			continue
		}
		if mark, _ := json.Marshal(marker{Host: c.Base, Version: envelope.Playbook.Version}); atomicWrite(filepath.Join(at, "SKILL.md"), skillFile(envelope.Playbook), false) == nil {
			_ = atomicWrite(filepath.Join(at, skillMarker), mark, false)
		}
	}
	for slug := range held {
		if !skills[slug] { // removed, or turned into a note
			_ = os.RemoveAll(filepath.Join(dir, syncedPrefix+slug))
		}
	}
}

// removeSyncedSkills removes every canyon-* skill under dir that a sync wrote, whichever host's: canyon skill uninstall.
func removeSyncedSkills(dir string) []string {
	var removed []string
	for slug := range marked(dir) {
		if os.RemoveAll(filepath.Join(dir, syncedPrefix+slug)) == nil {
			removed = append(removed, syncedPrefix+slug)
		}
	}
	slices.Sort(removed)
	return removed
}

// playbookSlug is a playbook's slug as the host makes one (contracts/routes.md, the playbooks row): a host's answer
// never names a directory outside the harness's skills.
func playbookSlug(s string) bool {
	if s == "" || len(s) > 40 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' })
}
