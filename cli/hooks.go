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

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// sessionHook is the one hook `skill install` registers beside a harness's skill (cli.md §7.1):
// the harness runs `canyon manifest` at session start and puts its stdout in the model's context,
// so the org's settings reach the agent live (§7.4). Claude Code reads hooks.SessionStart in
// ~/.claude/settings.json, Codex hooks.SessionStart in ~/.codex/hooks.json, and Cursor
// hooks.sessionStart in ./.cursor/hooks.json. Each sits in the harness's own directory, which
// chosen saw, so Canyon still creates no directory.
type sessionHook struct {
	shown, path string // the file as the row shows it, and where it is
	event       string // the key under "hooks" the harness runs at session start
	entry       string // Canyon's entry under it, the JSON the harness documents
	empty       string // the document a missing file starts from
	note        string // what a person should know about the entry, printed under it; "" for none
}

// Canyon's entry is known by its command alone, never by a key of Canyon's in someone else's
// file. In exec form — command and args, with no shell between — it is Canyon's when args are
// manifest --harness <harness>, whatever the program is called: install writes the path it ran
// from, and a binary renamed, or moved and installed again, must still find its own entry. In
// shell form it is Canyon's when the command is one simple command whose first word has the base
// name canyon (canyon.exe on Windows) and whose second is manifest. The words are a shell's, quotes
// removed, so Codex's quoted path with a space in it is one word; a command that does not parse,
// or runs more than one command is theirs.
func canyonCommand(command string, args []string) bool {
	if args != nil {
		return len(args) == 3 && args[0] == "manifest" && args[1] == "--harness"
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return false
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) < 2 || file.Stmts[0].Background || len(file.Stmts[0].Redirs) > 0 {
		return false
	}
	program, perr := expand.Literal(nil, call.Args[0])
	verb, verr := expand.Literal(nil, call.Args[1])
	return perr == nil && verr == nil && strings.TrimSuffix(filepath.Base(program), ".exe") == "canyon" && verb == "manifest"
}

// canyonIn reads an entry's commands — Cursor's one, or the list under a Claude Code matcher — and
// says whether all of them are Canyon's, or only some. An entry that is all Canyon's is Canyon's to
// replace; one a person built around Canyon's command is theirs, and is left as they built it.
// Ownership is read from the command alone: a key of Canyon's in someone else's file is not a
// marker we add, so a binary that happens to be named canyon with a manifest form reads as ours.
func canyonIn(entry json.RawMessage) (all, some bool) {
	type hook struct {
		Command string
		Args    []string
	}
	var e struct {
		Command string
		Args    []string
		Hooks   []hook
	}
	if json.Unmarshal(entry, &e) != nil {
		return false, false
	}
	commands := make([]hook, 0, len(e.Hooks)+1)
	if e.Command != "" {
		commands = append(commands, hook{e.Command, e.Args})
	}
	commands = append(commands, e.Hooks...)
	all = len(commands) > 0
	for _, c := range commands {
		if canyonCommand(c.Command, c.Args) {
			some = true
		} else {
			all = false
		}
	}
	return all, some
}

// installHook writes h's file and answers written or unchanged. A file that is not JSON is
// refused with nothing written; a missing one starts from h.empty. Every other key, event and
// entry survives, since only hooks.<event> is decoded past its bytes; the file comes back
// two-space indented, its top-level keys and event names in sorted order, so a second run
// finds its own bytes and is not a write. Before a write that carries Canyon's entry, the entry
// is printed with the file and the key it goes under: a person's own settings are edited in view.
// A file that is a link to another, a dotfiles repository's, is refused where it needs a write,
// since the rename would leave a copy where the link was; the entry is printed above the refusal,
// for the person to add there.
func (a *app) installHook(h *sessionHook) (string, error) {
	old, err := os.ReadFile(h.path) //nolint:gosec // one of the two paths skillTargets builds, neither from input
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("%s cannot be read: %s", h.shown, plainly(err))
	}
	doc := []byte(h.empty)
	if err == nil {
		doc = old
	}
	next, ours, err := withHook(doc, h.event, json.RawMessage(h.entry))
	if err != nil {
		return "", fmt.Errorf("%s cannot be read as JSON: %s", h.shown, err)
	}
	if bytes.Equal(next, old) {
		return "unchanged", nil
	}
	link := linked(h.path)
	if ours || link {
		a.printf("  %s Canyon's session hook, in %s under hooks.%s:\n      %s\n", a.render.glyph(statePending), h.shown, h.event, h.entry)
		if h.note != "" {
			a.printf("    %s\n", h.note)
		}
	}
	if link {
		return "", fmt.Errorf("%s is a link to another file, which install does not rewrite: add the entry above under hooks.%s there by hand", h.shown, h.event)
	}
	if err := atomicWrite(h.path, next, false); err != nil {
		return "", refused(h.shown, "written", err)
	}
	return "written", nil
}

// uninstallHook takes Canyon's entry out of h's file and answers what it did: "hook removed", every
// other byte of the file as it was, and the file kept even when nothing else is in it; builtByHand
// where Canyon's command still runs inside a group a person built, which is theirs to change; or not
// installed. A file that is not JSON is refused as install refuses it, and so is a link to another
// file that holds Canyon's entry, which the rename would replace with a copy (as migrateHook leaves
// one): nothing is written.
func uninstallHook(h *sessionHook) (string, error) {
	old, err := os.ReadFile(h.path) //nolint:gosec // one of the paths skillTargets builds, none from input
	if os.IsNotExist(err) {
		return notInstalled, nil
	}
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %s", h.shown, plainly(err))
	}
	next, removed, err := withoutHook(old, h.event)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read as JSON: %s", h.shown, err)
	}
	if removed {
		if linked(h.path) {
			return "", fmt.Errorf("%s is a link to another file, which uninstall does not rewrite: remove the entry that runs canyon manifest from hooks.%s there by hand", h.shown, h.event)
		}
		if err := atomicWrite(h.path, next, false); err != nil {
			return "", refused(h.shown, "written", err)
		}
	}
	_, _, entries, _ := hookEntries(next, h.event) // next is the document withoutHook just read
	switch {
	case slices.ContainsFunc(entries, func(e json.RawMessage) bool { _, some := canyonIn(e); return some }):
		return builtByHand, nil
	case removed:
		return "hook removed", nil
	}
	return notInstalled, nil
}

// migrateHook moves Canyon's entry in Claude Code's settings from the shell form an install before
// #1898 wrote, a bare canyon that fails where canyon is not on the PATH, to exec form with this
// binary's path (cli.md §7.4), and answers the line that says so; "" when there is nothing to move.
// A file that cannot be read as JSON, a symlink (a dotfiles repo's, which the rename would replace
// unasked), a group a person built around Canyon's command, and a binary main found no path for are
// each left as they are: the hook that runs this still works.
func (a *app) migrateHook(harness string) string {
	if harness != targetClaude || !filepath.IsAbs(Self) || a.getenv("HOME") == "" {
		return ""
	}
	for _, t := range a.skillTargets() {
		if t.harness != harness {
			continue
		}
		if info, err := os.Lstat(t.hook.path); err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ""
		}
		old, err := os.ReadFile(t.hook.path) //nolint:gosec // the path skillTargets builds, not from input
		if err != nil || !shellFormIn(old, t.hook.event) {
			return ""
		}
		next, ours, err := withHook(old, t.hook.event, json.RawMessage(t.hook.entry))
		if err != nil || !ours {
			return ""
		}
		if err := atomicWrite(t.hook.path, next, false); err != nil {
			return "Canyon: your session hook was not updated: " + cell(t.hook.shown+" cannot be written: "+plainly(err)) + ". Run: canyon skill install\n"
		}
		return "Canyon updated its session hook to run " + Self + " directly.\n"
	}
	return ""
}

// shellFormIn says whether an entry under hooks.<event> that is all Canyon's runs a command in shell
// form, with no args — the entry withHook replaces.
func shellFormIn(doc []byte, event string) bool {
	var f struct{ Hooks map[string][]json.RawMessage }
	if json.Unmarshal(doc, &f) != nil {
		return false
	}
	for _, e := range f.Hooks[event] {
		var group struct{ Hooks []struct{ Args []string } }
		if all, _ := canyonIn(e); !all || json.Unmarshal(e, &group) != nil {
			continue
		}
		for _, h := range group.Hooks {
			if h.Args == nil {
				return true
			}
		}
	}
	return false
}

// withHook is doc with entry as the one Canyon entry under hooks.<event>: replaced where an
// older one stands, a duplicate dropped, or appended after the entries already there. ours says
// whether entry itself is in it, and not a group a person built around Canyon's command.
func withHook(doc []byte, event string, entry json.RawMessage) (_ []byte, ours bool, _ error) {
	top, hooks, entries, err := hookEntries(doc, event)
	if err != nil {
		return nil, false, err
	}
	// One entry of Canyon's: the first that is all Canyon's is replaced where it stands and any
	// later one dropped. An entry a person built around Canyon's command is theirs — kept as it
	// is, and Canyon's command is present in it, so none is added beside it.
	kept, placed := make([]json.RawMessage, 0, len(entries)+1), false
	for _, e := range entries {
		all, some := canyonIn(e)
		switch {
		case all && !placed:
			kept, placed, ours = append(kept, entry), true, true
		case all:
		case some:
			kept, placed = append(kept, e), true
		default:
			kept = append(kept, e)
		}
	}
	if !placed {
		kept, ours = append(kept, entry), true
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	if hooks == nil {
		hooks = map[string]json.RawMessage{}
	}
	if hooks[event], err = encodeJSON(kept, false); err != nil {
		return nil, false, err
	}
	if top["hooks"], err = encodeJSON(hooks, false); err != nil {
		return nil, false, err
	}
	out, err := encodeJSON(top, true)
	return out, ours, err
}

// hookEntries reads doc as a hook file: its top-level keys, its hooks, and the entries under
// hooks.<event>, each left as its bytes. A document that is not an object, a "hooks" that is not one,
// and a hooks.<event> that is not a list are each refused.
func hookEntries(doc []byte, event string) (top, hooks map[string]json.RawMessage, entries []json.RawMessage, err error) {
	if err := json.Unmarshal(doc, &top); err != nil {
		return nil, nil, nil, err
	}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, nil, errors.New(`"hooks" is not an object`)
		}
	}
	if raw, ok := hooks[event]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, nil, fmt.Errorf(`"hooks.%s": %w`, event, err)
		}
	}
	return top, hooks, entries, nil
}

// withoutHook is doc with every entry under hooks.<event> that is all Canyon's cut out of its bytes,
// with the one comma that joined it to a neighbour. Everything else stays byte for byte, every key's
// order and every space included, which withHook's re-encoding would not keep: uninstall touches no
// more of a person's file than the entry install put there. An entry alone under its event takes the
// event with it, and an event alone in hooks takes hooks with it, so a file install made comes back
// as the document it started from. A group a person built around Canyon's command is theirs and stays,
// as withHook leaves it. removed says whether anything was cut.
func withoutHook(doc []byte, event string) (_ []byte, removed bool, _ error) {
	if _, _, _, err := hookEntries(doc, event); err != nil {
		return nil, false, err
	}
	for {
		cut, found, err := canyonEntry(doc, event)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return doc, removed, nil
		}
		doc, removed = slices.Concat(doc[:cut[0]], doc[cut[1]:]), true
	}
}

// canyonEntry is the byte range that takes out the first entry under hooks.<event> that is all
// Canyon's: the entry and a comma, or, where the entry is alone, its event, or hooks where that event
// is alone in it. Every "hooks" key is read, a duplicate's too, since the cut is by the bytes.
func canyonEntry(doc []byte, event string) (cut [2]int, found bool, _ error) {
	top, err := members(doc, len(doc)-len(bytes.TrimLeft(doc, jsonSpace)))
	if err != nil {
		return cut, false, err
	}
	for i, h := range top.items {
		if h.key != "hooks" || !h.opens('{') {
			continue
		}
		hooks, err := members(doc, h.valueAt())
		if err != nil {
			return cut, false, err
		}
		for j, e := range hooks.items {
			if e.key != event || !e.opens('[') {
				continue
			}
			entries, err := members(doc, e.valueAt())
			if err != nil {
				return cut, false, err
			}
			for k, entry := range entries.items {
				if all, _ := canyonIn(entry.value); !all {
					continue
				}
				switch {
				case len(entries.items) > 1:
					return entries.cut(k), true, nil
				case len(hooks.items) > 1:
					return hooks.cut(j), true, nil
				}
				return top.cut(i), true, nil
			}
		}
	}
	return cut, false, nil
}

// jsonSpace is the white space JSON allows between tokens (RFC 8259 §2).
const jsonSpace = " \t\r\n"

// container is one JSON object or array as it sits in a document: the offsets of its two brackets, and
// each member's bytes.
type container struct {
	open, close int
	items       []member
}

// member is an object's key and value, or an array's element: its key ("" in an array), its value's
// bytes, and the offsets of its first byte (the key's opening quote, or the element's) and of the byte
// after its value.
type member struct {
	key        string
	value      json.RawMessage
	start, end int
}

func (m member) valueAt() int      { return m.end - len(m.value) }
func (m member) opens(b byte) bool { return len(m.value) > 0 && m.value[0] == b }

// cut is the range that takes member i out of c with one comma: from its first byte to the next
// member's, or, for the last, from the previous member's end to its own; for the only one, everything
// between the brackets, which leaves {} or [].
func (c container) cut(i int) [2]int {
	switch {
	case len(c.items) == 1:
		return [2]int{c.open + 1, c.close}
	case i < len(c.items)-1:
		return [2]int{c.items[i].start, c.items[i+1].start}
	}
	return [2]int{c.items[i-1].end, c.items[i].end}
}

// members reads the object or array whose opening bracket is doc[open] with encoding/json's decoder,
// whose InputOffset is where each token it returned ends. Between two members the grammar allows only
// white space and one comma, so each member starts at the first byte past them.
func members(doc []byte, open int) (container, error) {
	c := container{open: open}
	dec := json.NewDecoder(bytes.NewReader(doc[open:]))
	if _, err := dec.Token(); err != nil {
		return c, err
	}
	for dec.More() {
		m := member{start: open + int(dec.InputOffset())}
		for m.start < len(doc) && strings.IndexByte(jsonSpace+",", doc[m.start]) >= 0 {
			m.start++
		}
		if doc[open] == '{' {
			key, err := dec.Token()
			if err != nil {
				return c, err
			}
			m.key, _ = key.(string) // an object's key, which the decoder answers as a string
		}
		if err := dec.Decode(&m.value); err != nil {
			return c, err
		}
		m.end = open + int(dec.InputOffset())
		c.items = append(c.items, m)
	}
	if _, err := dec.Token(); err != nil {
		return c, err
	}
	c.close = open + int(dec.InputOffset()) - 1
	return c, nil
}

// encodeJSON leaves <, > and & as they are, which json.Marshal would not: the bytes of someone
// else's entries go back as they came.
func encodeJSON(v any, indent bool) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	err := enc.Encode(v)
	return b.Bytes(), err
}
