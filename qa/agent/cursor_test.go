package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// archive is a gzipped tar of Cursor's shape, a top directory and what's under it, and its sha256.
func archive(t *testing.T, entries ...*tar.Header) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := []byte("#!/bin/sh\necho " + h.Name + "\n")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// serve answers cursorURL("test/arm64") with body, for one test.
func serve(t *testing.T, body []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+cursorVersion+"/test/arm64/agent-cli-package.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := cursorDownloads
	cursorDownloads = srv.URL + "/"
	t.Cleanup(func() { cursorDownloads = old })
}

func pinned(t *testing.T, sum string) {
	t.Helper()
	cursorSums["test/arm64"] = sum
	t.Cleanup(func() { delete(cursorSums, "test/arm64") })
}

var cursorPackage = []*tar.Header{
	{Name: "dist-package/", Typeflag: tar.TypeDir, Mode: 0o755},
	{Name: "dist-package/cursor-agent", Typeflag: tar.TypeReg, Mode: 0o755},
	{Name: "dist-package/lib/index.js", Typeflag: tar.TypeReg, Mode: 0o644},
}

// The runner installs Cursor only from the archive whose sha256 it recorded, unpacked as Cursor's install
// script lays it out, never a platform it has no sum for, and never an archive reaching outside its directory.
func TestCursorIsInstalledOnlyFromTheRecordedArchive(t *testing.T) {
	body, sum := archive(t, cursorPackage...)
	serve(t, body)
	home := t.TempDir()
	if err := installCursor(t.Context(), home, "plan9/arm64"); err == nil || !strings.Contains(err.Error(), "no Cursor CLI recorded for plan9/arm64") {
		t.Errorf("a platform with no recorded sum: %v", err)
	}
	pinned(t, strings.Repeat("0", 64))
	if err := installCursor(t.Context(), home, "test/arm64"); err == nil || !strings.Contains(err.Error(), "sha256 "+sum+", recorded") {
		t.Errorf("an archive that is not the recorded one: %v", err)
	}
	if _, err := os.Lstat(cursorBin(home)); err == nil {
		t.Error("an archive refused for its sum left an agent")
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".local", "share", "cursor-agent", "versions", "*")); len(left) > 0 {
		t.Errorf("an archive refused for its sum left %q", left)
	}
	pinned(t, sum)
	if err := installCursor(t.Context(), home, "test/arm64"); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(cursorBin(home))
	if want := filepath.Join(home, ".local", "share", "cursor-agent", "versions", cursorVersion, "cursor-agent"); err != nil || target != want {
		t.Errorf("agent links to %q (%v), want %q", target, err, want)
	}
	if fi, err := os.Stat(cursorBin(home)); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the agent is not executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "cursor-agent", "versions", cursorVersion, "lib", "index.js")); err != nil {
		t.Errorf("the package is not unpacked whole: %v", err)
	}
	if err := installCursor(t.Context(), home, "test/arm64"); err != nil { // again: the version is there, the link remade
		t.Errorf("a second install: %v", err)
	}
	for name, bad := range map[string][]*tar.Header{
		"a path out":     {{Name: "dist-package/../../escape", Typeflag: tar.TypeReg, Mode: 0o644}},
		"a link":         {{Name: "dist-package/cursor-agent", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}},
		"an absolute in": {{Name: "dist-package//etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}},
	} {
		body, sum := archive(t, bad...)
		serve(t, body)
		pinned(t, sum)
		fresh := t.TempDir()
		if err := installCursor(t.Context(), fresh, "test/arm64"); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Lstat(cursorBin(fresh)); err == nil {
			t.Errorf("%s: an agent was linked", name)
		}
	}
}

// Every batch unpacks the kept archive afresh once its sha256 checks again: a versions/<v> already there is never
// trusted, nor is a kept archive that no longer checks.
func TestEveryBatchUnpacksTheCheckedArchiveAfresh(t *testing.T) {
	body, sum := archive(t, cursorPackage...)
	serve(t, body)
	pinned(t, sum)
	home := t.TempDir()
	agentFile := filepath.Join(home, ".local", "share", "cursor-agent", "versions", cursorVersion, "cursor-agent")
	write(t, agentFile, "#!/bin/sh\necho tampered\n")
	if err := installCursor(t.Context(), home, "test/arm64"); err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\necho dist-package/cursor-agent\n"
	if got, _ := os.ReadFile(agentFile); string(got) != want {
		t.Errorf("a versions/%s already there was trusted: %q", cursorVersion, got)
	}
	serve(t, nil) // the next batch downloads nothing: it checks the archive it kept
	write(t, agentFile, "#!/bin/sh\necho tampered\n")
	if err := installCursor(t.Context(), home, "test/arm64"); err != nil {
		t.Fatalf("a batch with the archive kept: %v", err)
	}
	if got, _ := os.ReadFile(agentFile); string(got) != want {
		t.Errorf("the second batch kept a tampered agent: %q", got)
	}
	kept, _ := filepath.Glob(filepath.Join(home, ".local", "share", "cursor-agent", "*.tar.gz"))
	if len(kept) != 1 {
		t.Fatalf("kept archives: %q", kept)
	}
	write(t, kept[0], "not the archive")
	if err := installCursor(t.Context(), home, "test/arm64"); err == nil || !strings.Contains(err.Error(), "agent-cli-package.tar.gz: sha256") {
		t.Errorf("a kept archive that no longer checks was used, not downloaded again: %v", err) // and this server's is empty
	}
}

// A Cursor batch needs the key and a tester HOME whose Cursor and Claude Code define no hook, since Cursor
// runs both kinds; both are refused before anything is downloaded.
func TestACursorBatchNeedsItsKeyAndNoHooksOfTheTesters(t *testing.T) {
	home := t.TempDir()
	serve(t, nil) // a check that lets one through downloads nothing: this server has no archive
	t.Setenv(cursorKeyName, "")
	if err := prepareCursor(t.Context(), home); err == nil || !strings.Contains(err.Error(), "needs CURSOR_API_KEY") {
		t.Errorf("no key: %v", err)
	}
	t.Setenv(cursorKeyName, "key_test_0000")
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"canyon manifest"}]}]}}`)
	if err := prepareCursor(t.Context(), home); err == nil || !strings.Contains(err.Error(), "has a ~/.claude") {
		t.Errorf("Claude Code's hook in the tester HOME: %v", err)
	}
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"hooks":{}}`)
	if err := prepareCursor(t.Context(), home); err == nil || !strings.Contains(err.Error(), "has a ~/.claude") {
		t.Errorf("a ~/.claude with no hook in it: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".cursor", "hooks.json"), `not json`)
	if err := prepareCursor(t.Context(), home); err == nil || !strings.Contains(err.Error(), "does not read as a JSON object") {
		t.Errorf("a hooks.json that does not read: %v", err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// After every Cursor turn the runner stops the scenario when the agent changed Canyon's hook, left a cli.json
// or a Claude Code hook in the app, gave the tester a hook, or changed the tester's permissions.
func TestACursorTurnLeavesNoHookOrPermissionOfItsOwn(t *testing.T) {
	home, app := t.TempDir(), t.TempDir()
	key := "key_" + "live_cursor_a11" // synthetic, built at run time as the runner's fake keys are
	t.Setenv(cursorKeyName, key)
	write(t, cursorConfig(home), `{"model":"kept","permissions":{"allow":["Shell(rm)"],"deny":[]}}`)
	if err := configureCursor(home); err != nil {
		t.Fatal(err)
	}
	conf, err := readJSONObject(cursorConfig(home))
	if err != nil || conf["model"] != "kept" || conf["approvalMode"] != "allowlist" {
		t.Fatalf("configureCursor kept no key of Cursor's, or set no allowlist: %v %v", conf, err)
	}
	allow := conf["permissions"].(map[string]any)["allow"].([]any)
	if !slices.Contains(allow, any("Shell(canyon)")) || !slices.Contains(allow, any("Write(**)")) || slices.Contains(allow, any("Shell(rm)")) {
		t.Errorf("allowed: %v", allow)
	}
	hooks := `{"hooks":{"sessionStart":[{"command":"canyon manifest --harness cursor"}]},"version":1}`
	write(t, filepath.Join(app, ".cursor", "hooks.json"), hooks)
	c := cursorApp{hooks: []byte(hooks)}
	if err := cursorUnchanged(home, app, c); err != nil {
		t.Fatalf("as installed: %v", err)
	}
	for name, change := range map[string]struct {
		path, body, want string
	}{
		"Canyon's hook changed":   {filepath.Join(app, ".cursor", "hooks.json"), `{"hooks":{"sessionStart":[]},"version":1}`, "not as canyon skill install wrote it"},
		"a cli.json":              {filepath.Join(app, ".cursor", "cli.json"), `{"permissions":{"allow":["Shell(rm)"],"deny":[]}}`, "left a cli.json"},
		"the app's Claude hook":   {filepath.Join(app, ".claude", "settings.local.json"), `{"hooks":{"Stop":[{}]}}`, "defines a Stop hook"},
		"the tester's hook":       {filepath.Join(home, ".cursor", "hooks.json"), `{"version":1,"hooks":{"beforeShellExecution":[{"command":"true"}]}}`, "defines a beforeShellExecution hook"},
		"a tester ~/.claude":      {filepath.Join(home, ".claude", "CLAUDE.md"), "be brief", "has a ~/.claude"},
		"the key in the app":      {filepath.Join(app, "public", "config.js"), "window.K = '" + key + "';", "public/config.js holds CURSOR_API_KEY's value"},
		"the key in a permission": {cursorConfig(home), `{"permissions":{"allow":["Shell(` + key + `)"],"deny":[]},"approvalMode":"allowlist"}`, "cli-config.json permissions"},
		"a wider allowlist":       {cursorConfig(home), `{"permissions":{"allow":["Shell(rm)"],"deny":[]},"approvalMode":"allowlist"}`, "cli-config.json permissions"},
		"no allowlist":            {cursorConfig(home), "", ""},
	} {
		old, _ := os.ReadFile(change.path)
		body, want := change.body, change.want
		if name == "no allowlist" {
			conf["approvalMode"] = "unrestricted"
			raw, _ := json.Marshal(conf)
			conf["approvalMode"] = "allowlist"
			body, want = string(raw), "approvalMode"
		}
		write(t, change.path, body)
		if err := cursorUnchanged(home, app, c); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "a tester ~/.claude" {
			_ = os.RemoveAll(filepath.Dir(change.path))
		} else if old == nil {
			_ = os.Remove(change.path)
		} else {
			write(t, change.path, string(old))
		}
	}
}

// A turn whose transcript can't be scrubbed of Cursor's key fails, never passes with the key left in it.
func TestACursorTurnThatCannotBeScrubbedFails(t *testing.T) {
	t.Setenv(cursorKeyName, "key_"+"test_scrub")
	home, dir := t.TempDir(), t.TempDir()
	app := filepath.Join(dir, "qa-x")
	write(t, filepath.Join(app, "canyon.toml"), "app = \"qa-x\"\n")
	write(t, filepath.Join(dir, "cursor-chat"), "c0ffee\n")
	write(t, cursorBin(home), "#!/bin/sh\nrm ../turn-1.stderr\n") // the turn's own stderr gone: nothing to scrub, nothing checked
	if err := os.Chmod(cursorBin(home), 0o700); err != nil {      //nolint:gosec // a fake the test runs: it must be executable
		t.Fatal(err)
	}
	if err := os.MkdirAll(runTmp(filepath.Dir(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := turn(t.Context(), agentCursor, home, dir, app, 1, "Deploy it.", false); err == nil || !strings.Contains(err.Error(), "not scrubbed") {
		t.Errorf("a turn left unscrubbed: %v", err)
	}
}

// Every turn resumes the chat the runner created, in print mode, with Cursor's events, the workspace trusted
// so its hook runs, and no update mid-batch.
func TestACursorTurnResumesTheRunnersChat(t *testing.T) {
	want := []string{"--disable-auto-update", "-p", "Deploy it.", "--output-format", "stream-json", "--trust", "--workspace", "/run/app", "--resume", "c0ffee"}
	if got := cursorArgs("Deploy it.", "c0ffee", "/run/app"); !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
	if _, err := cursorChat(t.TempDir()); err == nil {
		t.Error("a scenario with no chat gave one to resume")
	}
}

// Setting the app up is what a Cursor user does, then the version for the scorecard and the chat to resume;
// a create-chat that prints anything but one id stops the scenario.
func TestSettingUpACursorAppRecordsTheVersionAndTheChat(t *testing.T) {
	home, dir, app := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "bin", "canyon"), "#!/bin/sh\nprintf '{\"hooks\":{}}' > .cursor/hooks.json\n")
	write(t, cursorBin(home), "#!/bin/sh\n[ \"$1\" = --disable-auto-update ] || exit 9\ncase \"$2\" in --version) echo "+cursorVersion+";; create-chat) cat \"$HOME/chat\";; esac\n")
	for _, f := range []string{filepath.Join(home, "bin", "canyon"), cursorBin(home)} {
		if err := os.Chmod(f, 0o700); err != nil { //nolint:gosec // a fake the test runs: it must be executable
			t.Fatal(err)
		}
	}
	t.Setenv(cursorKeyName, "key_test_0000")
	write(t, filepath.Join(home, "chat"), "one two\n")
	if _, err := setUpCursorApp(home, dir, app); err == nil || !strings.Contains(err.Error(), "not one chat id") {
		t.Errorf("two words for a chat: %v", err)
	}
	app = t.TempDir()
	write(t, filepath.Join(home, "chat"), "5f0c-chat\n")
	c, err := setUpCursorApp(home, dir, app)
	if err != nil || string(c.hooks) != `{"hooks":{}}` {
		t.Fatalf("%q, %v", c.hooks, err)
	}
	if v, _ := os.ReadFile(filepath.Join(dir, "cursor-version")); string(v) != cursorVersion+"\n" {
		t.Errorf("cursor-version: %q", v)
	}
	if chat, err := cursorChat(dir); err != nil || chat != "5f0c-chat" {
		t.Errorf("chat %q, %v", chat, err)
	}
}

// Until the probe's transcript is read, nothing of a Cursor turn is: turn 1 stops the scenario, the scorecard
// says why, and no score rests on it. Cursor's key never stays in a transcript or on the card.
func TestACursorRunFailsClosedUntilItsReaderAndKeepsNoKey(t *testing.T) {
	key := "key_" + "live_cursor_5e3" // synthetic, built at run time as the runner's fake keys are
	t.Setenv(cursorKeyName, key)
	run := t.TempDir()
	dir := filepath.Join(run, "cursor-secret-client")
	write(t, filepath.Join(dir, "scenario.md"), "agent: cursor\nstack: Node\nexpect first: BLOCKED secrets.client-bundle\n\n## Turns\n\n### 1\n```\nDeploy it.\n```\n")
	write(t, filepath.Join(dir, "turn-1.jsonl"), `{"type":"tool_call","output":"`+cursorKeyName+`=`+key+`"}`+"\n")
	write(t, filepath.Join(dir, "turn-1.stderr"), key+"\n")
	write(t, filepath.Join(dir, "turn-1.wall"), "2m5s\n")
	write(t, filepath.Join(dir, "cursor-version"), cursorVersion+"\n")
	if err := cursorManifestRead(dir); err == nil || !strings.Contains(err.Error(), "no Cursor reader yet") {
		t.Errorf("turn 1 read as the manifest reached: %v", err)
	}
	if err := scrubCursorKey(dir, 1); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"turn-1.jsonl", "turn-1.stderr"} {
		if raw, _ := os.ReadFile(filepath.Join(dir, f)); bytes.Contains(raw, []byte(key)) || !bytes.Contains(raw, []byte("<CURSOR_API_KEY>")) {
			t.Errorf("%s: %s", f, raw)
		}
	}
	write(t, filepath.Join(dir, "scorecard.md"), "## Reviewer notes\n\nit printed "+key+"\n") // a reviewer quoting it
	if err := score(run); err != nil {
		t.Fatal(err)
	}
	card, _ := os.ReadFile(filepath.Join(dir, "scorecard.md"))
	for _, want := range []string{
		"agent: cursor · stack: Node · turns sent: 1 · wall: 2m5s",
		"Cursor CLI: " + cursorVersion + " · models: not read, no Cursor reader yet",
		"| first result | BLOCKED secrets.client-bundle | none | MISS |",
		"not read: no Cursor reader yet",
		"- unreadable transcript: turn 1: no Cursor reader yet",
	} {
		if !strings.Contains(string(card), want) {
			t.Errorf("scorecard lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(string(card), key) {
		t.Errorf("the scorecard keeps Cursor's key:\n%s", card)
	}
	if last, ok := unfixed(dir, agentCursor); last != "none" || ok {
		t.Errorf("unfixed read %q from a Cursor turn", last)
	}
}
