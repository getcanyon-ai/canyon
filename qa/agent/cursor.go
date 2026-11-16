package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

// Cursor's headless CLI is `agent`: one `agent -p` per user turn, every turn resuming one chat the runner
// creates before turn 1. The runner installs it under the tester HOME from Cursor's versioned archive, as
// Cursor's install script lays it out, never by running that script: the archive is the one recorded here,
// checked against its sha256, and each turn passes --disable-auto-update, so a batch runs the version it names.
// The sign-in is CURSOR_API_KEY from the operator's environment, passed to Cursor's own processes alone and
// never written anywhere: a transcript that carries it has it scrubbed out (scrubCursorKey).

const agentCursor = "cursor"

// cursorVersion is the Cursor CLI the runner installs: the version Cursor's install script named on 2026-11-12.
const cursorVersion = "2026.11.11-b912975"

// cursorDownloads is where Cursor keeps its versioned archives; a var so a test can serve its own.
var cursorDownloads = "https://downloads.cursor.com/lab/"

// cursorSums are the sha256 of cursorVersion's archive for each platform, recorded when it was pinned. A
// platform with none is refused, never installed unchecked.
var cursorSums = map[string]string{
	"darwin/arm64": "2c60b58fb7d9a54c7cb9460f0cbb10d451d4af025d4451f252e5f1d956f8c66f",
	"darwin/amd64": "38578cf7f2a7c67679cffaf95818b58fa3e21ff95f8883ad0af012b5d5460489",
	"linux/arm64":  "f068af64ed91b5304605976586f690579679f740a41f388091a6b3a2e776987c",
	"linux/amd64":  "be01f42ad334f81d05a9992fcf060a52cc746f52cc451e849eb652146a6e3c91",
}

// cursorKeyName is the environment variable Cursor reads its API key from.
const cursorKeyName = "CURSOR_API_KEY"

// cursorBin is the tester's agent, the link Cursor's install script makes.
func cursorBin(home string) string { return filepath.Join(home, ".local", "bin", "agent") }

// installCursor puts cursorVersion under the tester HOME: the package in
// ~/.local/share/cursor-agent/versions/<version>, ~/.local/bin/agent a link to its cursor-agent. Every batch
// unpacks it afresh from the archive it keeps beside them, once that archive's sha256 checks again, and
// downloads the archive again when none is kept or the one kept no longer checks. A versions/<version> already
// there is never trusted: it is replaced.
func installCursor(ctx context.Context, home, platform string) error {
	sum, ok := cursorSums[platform]
	if !ok {
		return fmt.Errorf("no Cursor CLI recorded for %s: record the sha256 of %s's archive for it in cursor.go", platform, cursorVersion)
	}
	base := filepath.Join(home, ".local", "share", "cursor-agent")
	kept := filepath.Join(base, cursorVersion+"-"+strings.ReplaceAll(platform, "/", "-")+".tar.gz")
	if err := os.MkdirAll(filepath.Join(base, "versions"), 0o700); err != nil {
		return err
	}
	if checks(kept, sum) != nil {
		if err := fetchCursor(ctx, cursorURL(platform), sum, kept); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp(filepath.Join(base, "versions"), ".tmp-"+cursorVersion+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	f, err := os.Open(kept) //nolint:gosec // the archive this batch just checked
	if err != nil {
		return err
	}
	err = unpack(f, tmp)
	_ = f.Close()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "versions", cursorVersion)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	bin := cursorBin(home)
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return err
	}
	if err := os.Remove(bin); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(filepath.Join(dir, "cursor-agent"), bin)
}

// cursorURL is the archive of cursorVersion for a Go platform: Cursor names amd64 x64.
func cursorURL(platform string) string {
	return cursorDownloads + cursorVersion + "/" + strings.Replace(platform, "amd64", "x64", 1) + "/agent-cli-package.tar.gz"
}

// checks is nil when the file at path has the sha256 sum.
func checks(path, sum string) error {
	f, err := os.Open(path) //nolint:gosec // the runner's own kept archive
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%s: sha256 %s, recorded %s", path, got, sum)
	}
	return nil
}

// fetchCursor downloads the archive to dest, and refuses it, leaving nothing, unless its sha256 is sum.
func fetchCursor(ctx context.Context, url, sum, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	archive, err := os.CreateTemp(filepath.Dir(dest), ".archive-")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(archive, h), resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%s: sha256 %s, recorded %s: refused, nothing installed", url, got, sum)
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return os.Rename(archive.Name(), dest)
}

// unpack writes a gzipped tar's directories and files under dir, each path less its first element, as the install
// script's tar --strip-components=1 does. Anything
// else (a link, a device), or a path that leaves dir, refuses the archive.
func unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, rest, _ := strings.Cut(strings.TrimPrefix(hdr.Name, "./"), "/")
		if rest == "" {
			continue // the top directory itself
		}
		if !filepath.IsLocal(rest) {
			return fmt.Errorf("the archive's %q leaves its directory: refused", hdr.Name)
		}
		path := filepath.Join(dir, filepath.FromSlash(rest))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, hdr.FileInfo().Mode().Perm()&0o755) //nolint:gosec // a local path under dir, checked above
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // a checked archive of Cursor's, its size bounded by the download
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the archive's %q is neither a file nor a directory: refused", hdr.Name)
		}
	}
}

// cursorPermissions is what the tester lets Cursor's agent do without asking, the tools Claude Code is
// allowed in Cursor's words: a shell command by its first word, reads and writes in the workspace. Print mode
// asks nobody and refuses the rest (approvalMode allowlist).
func cursorPermissions() map[string]any {
	allow := []any{}
	for _, t := range tools {
		switch {
		case strings.HasPrefix(t, "Bash(") && strings.HasSuffix(t, ":*)"):
			allow = append(allow, "Shell("+strings.TrimSuffix(strings.TrimPrefix(t, "Bash("), ":*)")+")")
		case t == "Read":
			allow = append(allow, "Read(**)")
		case t == "Write":
			allow = append(allow, "Write(**)")
		}
	}
	return map[string]any{"permissions": map[string]any{"allow": allow, "deny": []any{}}, "approvalMode": "allowlist"}
}

// cursorConfig is the tester's ~/.cursor/cli-config.json.
func cursorConfig(home string) string { return filepath.Join(home, ".cursor", "cli-config.json") }

// configureCursor writes cursorPermissions into the tester's cli-config.json, keeping every other key Cursor
// keeps there.
func configureCursor(home string) error {
	conf, err := readJSONObject(cursorConfig(home))
	if err != nil {
		return err
	}
	maps.Copy(conf, cursorPermissions())
	if _, ok := conf["version"]; !ok {
		conf["version"] = 1
	}
	raw, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cursorConfig(home)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(cursorConfig(home), append(raw, '\n'), 0o600)
}

// readJSONObject is a JSON object file's keys, none when there is no file; one that is not an object is an error.
func readJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a file of the tester's or the app's, read and never run
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s does not read as a JSON object (%v): refused", path, err)
	}
	return obj, nil
}

// noHooks refuses a file that defines any hook: Cursor runs its own hooks.json and Claude Code's settings
// alike, so either would run beside Canyon's and the run would not be Canyon's setup alone.
func noHooks(paths ...string) error {
	for _, path := range paths {
		obj, err := readJSONObject(path)
		if err != nil {
			return err
		}
		hooks, ok := obj["hooks"]
		if !ok {
			continue
		}
		events, ok := hooks.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: hooks does not read as an object: refused", path)
		}
		for event, list := range events {
			if l, ok := list.([]any); !ok || len(l) > 0 {
				return fmt.Errorf("%s defines a %s hook: Cursor would run it beside Canyon's, so a Cursor run needs a tester HOME with no hooks", path, event)
			}
		}
	}
	return nil
}

// testerClean refuses a tester HOME that has a ~/.claude at all, since Cursor runs Claude Code's hooks and reads
// its settings, or a ~/.cursor/hooks.json that defines a hook.
func testerClean(home string) error {
	if _, err := os.Lstat(filepath.Join(home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the tester HOME has a ~/.claude (%v): Cursor runs Claude Code's hooks and reads its settings, so a Cursor batch takes a tester HOME without one", err)
	}
	return noHooks(filepath.Join(home, ".cursor", "hooks.json"))
}

// prepareCursor readies the tester HOME once per batch: the key there to pass, Cursor installed and allowed
// what Claude Code is, and no hook of the tester's own that Cursor would run.
func prepareCursor(ctx context.Context, home string) error {
	if os.Getenv(cursorKeyName) == "" {
		return fmt.Errorf("a Cursor scenario needs %s in the runner's environment: a Cursor user API key", cursorKeyName)
	}
	if err := testerClean(home); err != nil {
		return err
	}
	if err := installCursor(ctx, home, runtime.GOOS+"/"+runtime.GOARCH); err != nil {
		return err
	}
	return configureCursor(home)
}

// cursorEnv is a Cursor process's environment: the tester's, and the key.
func cursorEnv(home, app string) []string {
	return append(testerEnv(home, app), cursorKeyName+"="+os.Getenv(cursorKeyName))
}

// cursorApp is what a scenario's checks hold Cursor to after every turn: Canyon's hooks.json in the app as
// `canyon skill install` wrote it.
type cursorApp struct{ hooks []byte }

// setUpCursorApp sets Canyon up in the app as a Cursor user does (mkdir .cursor, canyon skill install), records
// the CLI's version for the scorecard, and creates the chat every turn resumes.
func setUpCursorApp(home, dir, app string) (cursorApp, error) {
	if err := os.Mkdir(filepath.Join(app, ".cursor"), 0o700); err != nil {
		return cursorApp{}, err
	}
	install := exec.Command(filepath.Join(home, "bin", "canyon"), "skill", "install", "--harness", agentCursor)
	install.Dir, install.Env = app, testerEnv(home, app)
	if out, err := install.CombinedOutput(); err != nil {
		return cursorApp{}, fmt.Errorf("canyon skill install --harness cursor: %w: %s", err, out)
	}
	hooks, err := os.ReadFile(filepath.Join(app, ".cursor", "hooks.json"))
	if err != nil {
		return cursorApp{}, fmt.Errorf("canyon skill install --harness cursor wrote no hooks.json: %w", err)
	}
	version, err := cursorOut(home, app, "--version")
	if err != nil {
		return cursorApp{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "cursor-version"), []byte(version+"\n"), 0o600); err != nil {
		return cursorApp{}, err
	}
	chat, err := cursorOut(home, app, "create-chat")
	if err != nil {
		return cursorApp{}, err
	}
	if len(strings.Fields(chat)) != 1 {
		return cursorApp{}, fmt.Errorf("agent create-chat printed %q, not one chat id: nothing to resume", chat)
	}
	return cursorApp{hooks: hooks}, os.WriteFile(filepath.Join(dir, "cursor-chat"), []byte(chat+"\n"), 0o600)
}

// cursorOut is one short agent command's output, run in the app.
func cursorOut(home, app string, args ...string) (string, error) {
	cmd := exec.Command(cursorBin(home), append([]string{"--disable-auto-update"}, args...)...)
	cmd.Dir, cmd.Env = app, cursorEnv(home, app)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("agent %s: %w: %s", strings.Join(args, " "), err, scrubbed(stderr.String()))
	}
	return strings.TrimSpace(scrubbed(string(out))), nil
}

// cursorArgs is one turn: print mode, Cursor's events, the app's workspace trusted so its hook runs, the chat
// setUpCursorApp created.
func cursorArgs(text, chat, app string) []string {
	return []string{"--disable-auto-update", "-p", text, "--output-format", "stream-json", "--trust", "--workspace", app, "--resume", chat}
}

// cursorChat is the chat setUpCursorApp created.
func cursorChat(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cursor-chat"))
	if err != nil {
		return "", fmt.Errorf("no chat to resume: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// cursorManifestRead is the check after turn 1 that Canyon's manifest reached the agent, through its hook or
// its rule. It waits on the probe's transcript to say how Cursor records either: until then it fails closed,
// and turn-1.jsonl is what the reader is written against.
func cursorManifestRead(dir string) error {
	return fmt.Errorf("no Cursor reader yet: can't tell whether Canyon's manifest reached the agent (%s is kept for the reader)",
		filepath.Join(dir, "turn-1.jsonl"))
}

// cursorUnchanged is the check after every Cursor turn: the agent works in the app, so a turn could add a hook
// or grant itself a permission. Canyon's hooks.json stays as installed, the app has no cli.json and no Claude
// Code hook, the tester still has no ~/.claude and no hook, and the tester's permissions are still the runner's.
// The agent's shell has Cursor's key, so no file in the app may hold its value either: the next deploy would
// upload it. Its message never carries the key.
func cursorUnchanged(home, app string, c cursorApp) error {
	if err := cursorChanged(home, app, c); err != nil {
		return errors.New(scrubbed(err.Error()))
	}
	return nil
}

func cursorChanged(home, app string, c cursorApp) error {
	if now, err := os.ReadFile(filepath.Join(app, ".cursor", "hooks.json")); err != nil || !bytes.Equal(now, c.hooks) {
		return fmt.Errorf("the app's .cursor/hooks.json is not as canyon skill install wrote it (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(app, ".cursor", "cli.json")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the agent left a cli.json in %s: it could grant permissions (%v)", filepath.Join(app, ".cursor"), err)
	}
	if err := testerClean(home); err != nil {
		return err
	}
	if err := noHooks(filepath.Join(app, ".claude", "settings.json"), filepath.Join(app, ".claude", "settings.local.json")); err != nil {
		return err
	}
	conf, err := readJSONObject(cursorConfig(home))
	if err != nil {
		return err
	}
	for key, want := range cursorPermissions() {
		got, _ := json.Marshal(conf[key]) // a decoded value: the encoder cannot fail
		wanted, _ := json.Marshal(want)
		var g, w any
		_ = json.Unmarshal(got, &g)
		_ = json.Unmarshal(wanted, &w)
		if !reflect.DeepEqual(g, w) {
			return fmt.Errorf("the tester's cli-config.json %s is %s, not the runner's %s", key, got, wanted)
		}
	}
	return keyIn(app)
}

// keyIn refuses an app any file of which holds Cursor's key.
func keyIn(app string) error {
	key := []byte(os.Getenv(cursorKeyName))
	if len(key) == 0 {
		return nil
	}
	return filepath.WalkDir(app, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path) //nolint:gosec // the app's own files, read and never run
		if err != nil {
			return err
		}
		if bytes.Contains(raw, key) {
			rel, _ := filepath.Rel(app, path)
			return fmt.Errorf("the app's %s holds %s's value: the next deploy would upload it", filepath.ToSlash(rel), cursorKeyName)
		}
		return nil
	})
}

// scrubbed is s with the Cursor key's value replaced by its name.
func scrubbed(s string) string {
	if key := os.Getenv(cursorKeyName); key != "" {
		return strings.ReplaceAll(s, key, "<"+cursorKeyName+">")
	}
	return s
}

// scrubCursorKey rewrites a Cursor turn's files with the key's value replaced by its name: the agent's shell
// inherits the key, so a turn that prints its environment would leave it there.
func scrubCursorKey(dir string, n int) error {
	key := os.Getenv(cursorKeyName)
	if key == "" {
		return nil
	}
	for _, ext := range []string{".jsonl", ".stderr"} {
		path := filepath.Join(dir, fmt.Sprintf("turn-%d%s", n, ext))
		raw, err := os.ReadFile(path) //nolint:gosec // the runner's own transcript
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(key)) {
			if err := os.WriteFile(path, []byte(scrubbed(string(raw))), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}
