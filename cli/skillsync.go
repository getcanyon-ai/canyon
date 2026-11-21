package cli

import (
	"context"
	"encoding/json"
	"fmt"
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

// skillsShown is each harness's skills directory as a row names it.
var skillsShown = map[string]string{targetClaude: "~/.claude/skills/", targetCodex: "~/.agents/skills/"}

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
		if m, ok := markerIn(filepath.Join(dir, e.Name())); ok {
			out[slug] = m
		}
	}
	return out
}

// markerIn is at's marker when at is a real directory, never a link to one, holding a .canyon that is a regular file
// naming a host and a version: the only directory a sync writes in or removes (os.Lstat, so a link is never followed).
func markerIn(at string) (marker, bool) {
	var m marker
	if fi, err := os.Lstat(at); err != nil || !fi.IsDir() {
		return m, false
	}
	path := filepath.Join(at, skillMarker)
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return m, false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the marker of a directory this sync names
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Host == "" || m.Version < 1 {
		return marker{}, false
	}
	return m, true
}

// notOurs is the hook's line for a canyon-<slug> path the sync didn't write, which it leaves as it is (D5).
const notOurs = "Canyon didn't sync the playbook %s: %s is not a directory Canyon wrote, so Canyon leaves it alone."

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
// strings, which YAML reads as they are, so no text of the admin's can end the frontmatter; every host string is
// visible, so no character hides text from the person who reads the file. Claude Code runs !`cmd` in a skill as a
// shell command before the model reads it (https://code.claude.com/docs/en/skills), and its docs don't say a code
// block is spared, so each !` in the body is written "! `", which runs nothing; the host refuses one outside a code
// block at save, and canyon playbook and the tool hand the body as it is.
func skillFile(p playbookRead) []byte {
	name, _ := json.Marshal(p.Slug)
	description, _ := json.Marshal(visible(p.When))
	return []byte("---\nname: " + string(name) + "\ndescription: " + string(description) + "\n---\n\n" + strings.ReplaceAll(visible(p.Body), "!`", "! `"))
}

// syncSkills makes dir's canyon-* skills the host's: each skill in playbooks whose version the directory doesn't hold
// is fetched (counted as a sync, not a read) and written, its marker last; each this host's sync wrote that is no
// longer a skill is removed. It gives up once left has passed, and a failure leaves the directory as it was for that
// skill: the next session tries again. A session hook never fails a session. A canyon-<slug> path that is not a real
// directory whose marker names this host — a person's own skill, a link, another host's — is never written in or
// removed: its line says so, shown under the harness's own path for it.
func syncSkills(ctx context.Context, c *Client, dir, shown string, playbooks []handedPlaybook, held map[string]int, left time.Duration) (said []string) {
	if dir == "" {
		return nil
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
		at := filepath.Join(dir, syncedPrefix+p.Slug)
		if _, err := os.Lstat(at); err == nil {
			if m, ours := markerIn(at); !ours || m.Host != c.Base {
				said = append(said, fmt.Sprintf(notOurs, cell(p.Slug), shown+syncedPrefix+p.Slug+"/"))
				continue
			}
		}
		within := time.Until(until)
		if within <= 0 {
			return said
		}
		var envelope struct {
			Playbook playbookRead `json:"playbook"`
		}
		if c.do(ctx, http.MethodGet, "/api/v1/playbooks/"+url.PathEscape(p.Slug)+"?read=sync", "", nil, 0, nil, http.StatusOK, within, answerLimit, &envelope) != nil ||
			envelope.Playbook.Slug != p.Slug {
			continue
		}
		if _, err := os.Lstat(at); os.IsNotExist(err) { // made here, a real directory: Mkdir never follows a link at its own path
			if os.MkdirAll(dir, 0o755) != nil || os.Mkdir(at, 0o755) != nil { //nolint:gosec // a harness's skill directory, readable as its others are
				continue
			}
		}
		if mark, _ := json.Marshal(marker{Host: c.Base, Version: envelope.Playbook.Version}); atomicWrite(filepath.Join(at, "SKILL.md"), skillFile(envelope.Playbook), false) == nil {
			_ = atomicWrite(filepath.Join(at, skillMarker), mark, false)
		}
	}
	for slug := range held {
		at := filepath.Join(dir, syncedPrefix+slug)
		if m, ours := markerIn(at); !skills[slug] && ours && m.Host == c.Base { // removed, or turned into a note: ours, checked again
			_ = os.RemoveAll(at)
		}
	}
	return said
}

// removeSyncedSkills removes every canyon-* skill under dir that a sync wrote, whichever host's: canyon skill uninstall.
func removeSyncedSkills(dir string) []string {
	var removed []string
	for slug := range marked(dir) { // a real directory holding a marker, never a link (markerIn)
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
