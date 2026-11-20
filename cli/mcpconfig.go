package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// mcpConfig is the MCP server skill install registers beside a harness's skill and hook
// (design/specs/mcp.md §1.4): canyon mcp, by the path the hook runs where the file is this machine's
// alone, by name in Cursor's project file. Claude Code reads mcpServers.canyon in ~/.claude.json, Cursor
// mcpServers.canyon in ./.cursor/mcp.json, Codex [mcp_servers.canyon] in ~/.codex/config.toml.
type mcpConfig struct {
	shown, path string
	command     string // the program the entry runs
	stdio       bool   // the entry says its transport, "type": "stdio": Claude Code's does, and Cursor's docs mark it required
	toml        bool   // Codex's config.toml; the others are JSON
}

const mcpServers = "mcpServers"

// mcpByHand is a server named canyon that runs something else: a person's, left as it is.
const mcpByHand = "Left the MCP server named canyon as it is: Canyon didn't set it up."

func (m *mcpConfig) entry() string {
	if m.toml {
		body, _ := toml.Marshal(struct { // two strings, so the encoder cannot fail
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		}{m.command, []string{"mcp"}})
		return "[mcp_servers.canyon]\n" + string(body)
	}
	command, _ := encodeJSON(m.command, false) // a string, so the encoder cannot fail
	kind := ""
	if m.stdio {
		kind = `"type":"stdio",`
	}
	return `{` + kind + `"command":` + strings.TrimSuffix(string(command), "\n") + `,"args":["mcp"]}`
}

// canyonServer is whether a server entry runs canyon mcp: args are exactly mcp, and the program's base
// name is canyon (canyon.exe on Windows), whatever its path, as a hook's is read.
func canyonServer(command string, args []string) bool {
	return slices.Equal(args, []string{"mcp"}) && strings.TrimSuffix(filepath.Base(command), ".exe") == "canyon"
}

// installMCP writes m's entry and answers written, unchanged, or mcpByHand. A file that does not parse is
// refused with nothing written, and so is a link to another file that needs a write; a missing one is made.
// Every byte outside Canyon's entry stays as it was: ~/.claude.json is Claude Code's own state, which it
// rewrites while it runs, so the file is read just before the write and only the entry is spliced in.
func (a *app) installMCP(m *mcpConfig) (string, error) {
	old, err := os.ReadFile(m.path) //nolint:gosec // one of the paths skillTargets builds, none from input
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("%s cannot be read: %s", m.shown, plainly(err))
	}
	next, status, err := m.with(old)
	if err != nil {
		return "", err
	}
	if status != "" {
		return status, nil
	}
	under := " under " + mcpServers + ".canyon" // a TOML table names itself in its first line
	if m.toml {
		under = ""
	}
	a.printf("  %s Canyon's MCP server, in %s%s:\n      %s\n", a.render.glyph(statePending), m.shown, under, strings.ReplaceAll(strings.TrimSpace(m.entry()), "\n", "\n      "))
	if linked(m.path) {
		return "", fmt.Errorf("%s is a link to another file, which install does not rewrite: add the entry above there by hand", m.shown)
	}
	if err := atomicWrite(m.path, next, false); err != nil {
		return "", refused(m.shown, "written", err)
	}
	return "written", nil
}

// uninstallMCP takes Canyon's entry out of m's file, every other byte as it was, and answers "server
// removed", mcpByHand, or not installed.
func uninstallMCP(m *mcpConfig) (string, error) {
	old, err := os.ReadFile(m.path) //nolint:gosec // one of the paths skillTargets builds, none from input
	if os.IsNotExist(err) {
		return notInstalled, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %s", m.shown, plainly(err))
	}
	at, found, ours, err := m.find(old)
	switch {
	case err != nil:
		return "", err
	case !found:
		return notInstalled, nil
	case !ours:
		return mcpByHand, nil
	case linked(m.path):
		return "", fmt.Errorf("%s is a link to another file, which uninstall does not rewrite: remove Canyon's MCP server from it by hand", m.shown)
	}
	if err := atomicWrite(m.path, slices.Concat(old[:at.cut[0]], old[at.cut[1]:]), false); err != nil {
		return "", refused(m.shown, "written", err)
	}
	return "server removed", nil
}

// with is doc with Canyon's entry in it, or a status that says why nothing is written: unchanged, or mcpByHand.
func (m *mcpConfig) with(doc []byte) (_ []byte, status string, _ error) {
	at, found, ours, err := m.find(doc)
	if err != nil {
		return nil, "", err
	}
	if found && !ours {
		return nil, mcpByHand, nil
	}
	entry := m.entry()
	if m.toml {
		if found {
			if string(doc[at.value[0]:at.value[1]]) == entry {
				return nil, "unchanged", nil
			}
			return slices.Concat(doc[:at.value[0]], []byte(entry), doc[at.value[1]:]), "", nil
		}
		switch {
		case len(doc) == 0:
		case bytes.HasSuffix(doc, []byte("\n")):
			entry = "\n" + entry
		default:
			entry = "\n\n" + entry
		}
		return append(slices.Clone(doc), entry...), "", nil
	}
	if len(bytes.TrimSpace(doc)) == 0 {
		return []byte(`{"` + mcpServers + `":{"canyon":` + entry + "}}\n"), "", nil
	}
	if found {
		if string(doc[at.value[0]:at.value[1]]) == entry {
			return nil, "unchanged", nil
		}
		return slices.Concat(doc[:at.value[0]], []byte(entry), doc[at.value[1]:]), "", nil
	}
	top, err := members(doc, len(doc)-len(bytes.TrimLeft(doc, jsonSpace)))
	if err != nil {
		return nil, "", fmt.Errorf("%s cannot be read as JSON: %s", m.shown, err)
	}
	for _, s := range top.items {
		if s.key == mcpServers && s.opens('{') {
			servers, err := members(doc, s.valueAt())
			if err != nil {
				return nil, "", fmt.Errorf("%s cannot be read as JSON: %s", m.shown, err)
			}
			return servers.insert(doc, `"canyon":`+entry), "", nil
		}
	}
	return top.insert(doc, `"`+mcpServers+`":{"canyon":`+entry+`}`), "", nil
}

// insert is doc with member added last in c.
func (c container) insert(doc []byte, member string) []byte {
	if len(c.items) == 0 {
		return slices.Concat(doc[:c.open+1], []byte(member), doc[c.close:])
	}
	end := c.items[len(c.items)-1].end
	return slices.Concat(doc[:end], []byte(","+member), doc[end:])
}

// spot is where Canyon's entry sits in a file: the range its value takes, which install replaces, and the
// range uninstall cuts. In JSON the cut takes the member out with one comma, as a hook's is cut, and
// mcpServers with it where canyon is alone there; in TOML both are the table, from its header's line to
// the end of its last key's.
type spot struct{ value, cut [2]int }

// find is where the canyon entry is in doc, whether there is one, and whether it runs canyon mcp.
func (m *mcpConfig) find(doc []byte) (at spot, found, ours bool, _ error) {
	if m.toml {
		return m.findTable(doc)
	}
	if len(bytes.TrimSpace(doc)) == 0 {
		return at, false, false, nil
	}
	top, err := members(doc, len(doc)-len(bytes.TrimLeft(doc, jsonSpace)))
	if err == nil && doc[top.open] != '{' {
		err = errors.New("it is not an object")
	}
	if err != nil {
		return at, false, false, fmt.Errorf("%s cannot be read as JSON: %s", m.shown, err)
	}
	for i, s := range top.items {
		if s.key != mcpServers || !s.opens('{') {
			continue
		}
		servers, err := members(doc, s.valueAt())
		if err != nil {
			return at, false, false, fmt.Errorf("%s cannot be read as JSON: %s", m.shown, err)
		}
		for j, e := range servers.items {
			if e.key != "canyon" {
				continue
			}
			var server struct {
				Command string
				Args    []string
			}
			at = spot{value: [2]int{e.valueAt(), e.end}, cut: top.cut(i)}
			if len(servers.items) > 1 {
				at.cut = servers.cut(j)
			}
			return at, true, json.Unmarshal(e.value, &server) == nil && canyonServer(server.Command, server.Args), nil
		}
	}
	return at, false, false, nil
}

// findTable reads config.toml with go-toml's parser: the [mcp_servers.canyon] table's range, or a canyon
// server defined any other way, which is a person's.
func (m *mcpConfig) findTable(doc []byte) (at spot, found, ours bool, _ error) {
	var all map[string]any
	if err := toml.Unmarshal(doc, &all); err != nil {
		return at, false, false, fmt.Errorf("%s cannot be read as TOML: %s", m.shown, err)
	}
	servers, _ := all["mcp_servers"].(map[string]any)
	server, defined := servers["canyon"].(map[string]any)
	if _, named := servers["canyon"]; !named {
		return at, false, false, nil
	}
	command, _ := server["command"].(string)
	var args []string
	if list, ok := server["args"].([]any); ok {
		for _, arg := range list {
			word, _ := arg.(string)
			args = append(args, word)
		}
	}
	p := unstable.Parser{KeepComments: true}
	p.Reset(doc)
	in, keys, start, end := false, 0, -1, -1
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind == unstable.Table || e.Kind == unstable.ArrayTable {
			var key []string
			var first []byte
			for it := e.Key(); it.Next(); {
				if first == nil {
					first = it.Node().Data
				}
				key = append(key, string(it.Node().Data))
			}
			in = e.Kind == unstable.Table && slices.Equal(key, []string{"mcp_servers", "canyon"})
			if in {
				at := int(p.Range(first).Offset)
				start = bytes.LastIndexByte(doc[:at], '\n') + 1
				end = lineEnd(doc, at)
			}
			continue
		}
		if in && e.Kind == unstable.KeyValue {
			keys++
			end = lineEnd(doc, int(e.Raw.Offset+e.Raw.Length))
		}
	}
	if err := p.Error(); err != nil {
		return at, false, false, fmt.Errorf("%s cannot be read as TOML: %s", m.shown, err)
	}
	if !defined || start < 0 || keys != 2 || !canyonServer(command, args) { // defined another way, or with more than install writes: theirs
		return at, true, false, nil
	}
	table, cut := [2]int{start, end}, [2]int{start, end}
	if start >= 2 && doc[start-1] == '\n' && doc[start-2] == '\n' && end == len(doc) { // the blank line install put before a table it appended goes with it
		cut[0]--
	}
	return spot{table, cut}, true, true, nil
}

// lineEnd is the offset just past the newline that ends the line holding doc[at], or the end of doc.
func lineEnd(doc []byte, at int) int {
	if i := bytes.IndexByte(doc[at:], '\n'); i >= 0 {
		return at + i + 1
	}
	return len(doc)
}
