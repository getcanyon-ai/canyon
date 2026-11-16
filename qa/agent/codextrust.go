package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"mvdan.cc/sh/v3/syntax"
)

// Codex runs a hook only once its exact definition is trusted, and records that trust against the
// hook's hash under hooks.state in the user's config.toml. The runner trusts Canyon's hook the way
// Codex's own /hooks does (codex-rs/tui/src/hooks_rpc.rs at rust-v0.164.0): it asks codex app-server
// for hooks/list, which names each hook's key and current hash, and writes
// hooks.state.<key>.trusted_hash with config/batchWrite. It trusts nothing else, and refuses a
// tester HOME whose Codex lists any other hook, so no turn runs a hook nobody reviewed.

// trustWithin bounds the whole exchange with codex app-server.
const trustWithin = time.Minute

// trustCodexHook trusts Canyon's session hook in the tester's Codex, for turns run in app.
func trustCodexHook(home, app string) error {
	return withAppServer(home, app, trustCanyonHook)
}

// forgetProject removes the trust Codex records for the scenario's app on its first turn
// ([projects."<app>"] in the tester's config.toml), through Codex's own config writer, so a batch
// leaves the tester HOME trusting no app it ran: a trusted project's own .codex layer would load.
func forgetProject(home, app string) error {
	return withAppServer(home, app, untrustProject)
}

// untrustProject is that removal: config/batchWrite replacing projects."<app>" with nothing, which
// Codex 0.164.0 writes as the table gone and every other key as it was.
func untrustProject(c *rpc, app, _, _ string) error {
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "canyon-qa-agent", "version": "1"}}, nil); err != nil {
		return err
	}
	if err := c.notify("initialized"); err != nil {
		return err
	}
	key := `projects."` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(app) + `"`
	edit := map[string]any{"keyPath": key, "value": nil, "mergeStrategy": "replace"}
	return c.call("config/batchWrite", map[string]any{"edits": []any{edit}, "reloadUserConfig": false}, nil)
}

// codexTrustsOnlyCanyon is the check after every Codex turn: the agent works in app, so a turn could
// leave a hook there, or try to trust one. Nothing under app's .codex may hold a hook or a trust,
// and Codex may list no trusted hook but Canyon's.
func codexTrustsOnlyCanyon(home, app string) error {
	if _, err := os.Stat(filepath.Join(app, ".codex", "hooks.json")); err == nil {
		return fmt.Errorf("the agent left a hooks.json in %s", filepath.Join(app, ".codex"))
	}
	if raw, err := os.ReadFile(filepath.Join(app, ".codex", "config.toml")); err == nil { //nolint:gosec // the app's own directory, read and never run
		var conf struct {
			Hooks struct {
				State map[string]any `toml:"state"`
			} `toml:"hooks"`
		}
		if err := toml.Unmarshal(raw, &conf); err != nil || len(conf.Hooks.State) > 0 {
			return fmt.Errorf("the agent left a config.toml in %s that trusts hooks, or that does not read as TOML (%v)", filepath.Join(app, ".codex"), err)
		}
	}
	return withAppServer(home, app, onlyCanyonTrusted)
}

// withAppServer runs one exchange with codex app-server under the tester's environment, in app, with
// the hooks.json path and the command Canyon's hook must have.
func withAppServer(home, app string, exchange func(c *rpc, app, hooksPath, command string) error) error {
	command, err := canyonHookCommand(home)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustWithin)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server")
	cmd.Dir, cmd.Env = app, testerEnv(home, app)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex app-server: %w", err)
	}
	err = exchange(newRPC(in, out), app, filepath.Join(home, ".codex", "hooks.json"), command)
	_ = in.Close() // the server ends when its input does
	_ = cmd.Wait()
	if err == nil && ctx.Err() != nil { // an answer that ran out of time is no answer
		err = fmt.Errorf("codex app-server: %w", ctx.Err())
	}
	return err
}

// onlyCanyonTrusted lists the hooks Codex loads in app and refuses any it trusts but Canyon's; one it
// does not trust never runs.
func onlyCanyonTrusted(c *rpc, app, hooksPath, command string) error {
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "canyon-qa-agent", "version": "1"}}, nil); err != nil {
		return err
	}
	if err := c.notify("initialized"); err != nil {
		return err
	}
	var list hooksList
	if err := c.call("hooks/list", map[string]any{"cwds": []string{app}}, &list); err != nil {
		return err
	}
	for _, d := range list.Data {
		for _, h := range d.Hooks {
			ours := h.EventName == "sessionStart" && h.Source == "user" && h.SourcePath == hooksPath && h.Command == command
			if !ours && (h.TrustStatus == "trusted" || h.TrustStatus == "managed") {
				return fmt.Errorf("the Codex app-server lists a %s hook as %s that is not Canyon's: %s runs %q", h.Source, h.TrustStatus, h.SourcePath, h.Command)
			}
		}
	}
	return nil
}

// canyonHookCommand is the one shell command canyon skill install writes for Codex when run as the
// tester's bin/canyon: its path, quoted for a POSIX shell, then manifest --harness codex.
func canyonHookCommand(home string) (string, error) {
	quoted, err := syntax.Quote(filepath.Join(home, "bin", "canyon"), syntax.LangPOSIX)
	if err != nil {
		return "", fmt.Errorf("the tester's bin/canyon has a path no shell can spell: %w", err)
	}
	return quoted + " manifest --harness " + agentCodex, nil
}

// codexHook is one hook as hooks/list describes it; only the fields the runner reads.
type codexHook struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	HandlerType string `json:"handlerType"`
	Command     string `json:"command"`
	Source      string `json:"source"`
	SourcePath  string `json:"sourcePath"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
}

// hooksList is hooks/list's result: each working directory asked for, with the hooks that load there.
type hooksList struct {
	Data []struct {
		Cwd    string      `json:"cwd"`
		Hooks  []codexHook `json:"hooks"`
		Errors []struct {
			Message string `json:"message"`
			Path    string `json:"path"`
		} `json:"errors"`
	} `json:"data"`
}

// canyonHook is Canyon's hook among the hooks that load in app: the only one, a user hook from the
// tester's ~/.codex/hooks.json, at session start, running exactly command. Any other hook, or a load
// error, is refused: the runner trusts what it installed and nothing an agent or a stray file added.
func canyonHook(list hooksList, app, hooksPath, command string) (codexHook, error) {
	for _, d := range list.Data {
		if d.Cwd != app {
			continue
		}
		if len(d.Errors) > 0 {
			return codexHook{}, fmt.Errorf("the Codex app-server could not load a hook for %s: %s (%s)", app, d.Errors[0].Message, d.Errors[0].Path)
		}
		if len(d.Hooks) != 1 {
			var seen []string
			for _, h := range d.Hooks {
				seen = append(seen, h.SourcePath+": "+h.Command)
			}
			return codexHook{}, fmt.Errorf("the Codex app-server lists %d hooks for %s, and the runner trusts Canyon's alone: %s", len(d.Hooks), app, strings.Join(seen, "; "))
		}
		h := d.Hooks[0]
		switch {
		case h.EventName != "sessionStart", h.HandlerType != "command", h.Source != "user":
			return codexHook{}, fmt.Errorf("the hook Codex lists is a %s %s hook from its %s config, not Canyon's session hook", h.EventName, h.HandlerType, h.Source)
		case h.SourcePath != hooksPath:
			return codexHook{}, fmt.Errorf("the hook Codex lists is in %s, not %s", h.SourcePath, hooksPath)
		case h.Command != command:
			return codexHook{}, fmt.Errorf("the hook Codex lists runs %q, not %q", h.Command, command)
		case h.Key == "" || !strings.HasPrefix(h.CurrentHash, "sha256:"):
			return codexHook{}, fmt.Errorf("the Codex app-server lists Canyon's hook with no key or hash: %+v", h)
		}
		return h, nil
	}
	return codexHook{}, fmt.Errorf("the Codex app-server listed no hooks for %s", app)
}

// trustCanyonHook is the exchange itself: initialize, list, trust Canyon's hook by the hash Codex
// gave, and list again, which must say trusted.
func trustCanyonHook(c *rpc, app, hooksPath, command string) error {
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "canyon-qa-agent", "version": "1"}}, nil); err != nil {
		return err
	}
	if err := c.notify("initialized"); err != nil {
		return err
	}
	var list hooksList
	if err := c.call("hooks/list", map[string]any{"cwds": []string{app}}, &list); err != nil {
		return err
	}
	hook, err := canyonHook(list, app, hooksPath, command)
	if err != nil {
		return err
	}
	edit := map[string]any{
		"keyPath":       "hooks.state",
		"value":         map[string]any{hook.Key: map[string]string{"trusted_hash": hook.CurrentHash}},
		"mergeStrategy": "upsert",
	}
	if err := c.call("config/batchWrite", map[string]any{"edits": []any{edit}, "reloadUserConfig": false}, nil); err != nil {
		return err
	}
	if err := c.call("hooks/list", map[string]any{"cwds": []string{app}}, &list); err != nil {
		return err
	}
	if hook, err = canyonHook(list, app, hooksPath, command); err != nil {
		return err
	}
	if hook.TrustStatus != "trusted" {
		return fmt.Errorf("the Codex app-server reads Canyon's hook as %s after the runner trusted it", hook.TrustStatus)
	}
	return nil
}

// rpc is codex app-server's JSON-RPC over stdio: one JSON message a line each way.
type rpc struct {
	w    io.Writer
	r    *bufio.Scanner
	next int
}

func newRPC(w io.Writer, r io.Reader) *rpc {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 16<<20), 16<<20)
	return &rpc{w: w, r: sc}
}

func (c *rpc) send(msg map[string]any) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = c.w.Write(append(line, '\n'))
	return err
}

func (c *rpc) notify(method string) error { return c.send(map[string]any{"method": method}) }

// call sends a request and reads until its answer, past the server's notifications in between.
func (c *rpc) call(method string, params, result any) error {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("codex app-server %s: %w", method, err)
	}
	for c.r.Scan() {
		var m struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(c.r.Bytes(), &m); err != nil {
			return fmt.Errorf("codex app-server %s: %w", method, err)
		}
		if m.ID == nil || *m.ID != id {
			continue
		}
		if m.Error != nil {
			return fmt.Errorf("codex app-server %s: %s", method, m.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
	return fmt.Errorf("codex app-server %s: %w", method, cmp.Or(c.r.Err(), io.ErrUnexpectedEOF))
}

// manifestFirst is how canyon manifest's text for an agent begins (cli/testdata/manifest-agent.txt),
// the text Canyon's session hook prints.
const manifestFirst = "Canyon's settings for this org, as canyon manifest prints them."

// codexHookRan proves turn 1 began with Canyon's manifest in Codex's context, which only the trusted
// session hook puts there: codex exec --json says nothing of hooks, so the proof is Codex's own record
// of the session, its rollout under ~/.codex/sessions, which holds the hook's output as a developer
// message. A hook Codex skipped (untrusted, as after a Codex release that hashes hooks differently)
// leaves none, and the scenario stops rather than score an agent that never got the org's rules.
func codexHookRan(home, dir string) error {
	thread, err := codexThread(dir)
	if err != nil {
		return err
	}
	var rollout string
	sessions := filepath.Join(home, ".codex", "sessions")
	walked := filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), "-"+thread+".jsonl") {
			rollout = path
			return fs.SkipAll
		}
		return err
	})
	if walked != nil || rollout == "" {
		return fmt.Errorf("no Codex session record for thread %s under %s (%v)", thread, sessions, walked)
	}
	f, err := os.Open(rollout) //nolint:gosec // Codex's own record of the thread this run started
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 16<<20), 16<<20)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "response_item" || line.Payload.Type != "message" || line.Payload.Role != "developer" {
			continue
		}
		for _, c := range line.Payload.Content {
			if strings.HasPrefix(c.Text, manifestFirst) {
				return nil
			}
		}
	}
	return fmt.Errorf("the Canyon session hook did not run: %s holds no message that begins %q; Codex skips a hook it does not trust, and a Codex release may hash hooks differently", rollout, manifestFirst)
}
