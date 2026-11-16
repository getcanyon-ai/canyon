//go:build cluster

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const buildTime = 15 * time.Minute // one deploy: the review, a buildpack build on kpack, the rollout (README, Waits)

// sessionCookie is the name the server gives its session cookie under https, which $E2E_CANYON_URL
// always is (cmd/canyon-server/session.go sessionCookieOf).
const sessionCookie = "__Host-canyon_session"

// session is one person signed in to Canyon: the cookie every request carries and the config home
// the CLI signed in to. Each person has their own, so alice and bob can act at once (S1, S3). The
// zero value is nobody: no cookie, and a fresh config home for every CLI call.
type session struct {
	cookie string
	home   string
}

// canyon runs the CLI in dir as the session's person: its config home, and no harness variable,
// so it neither reads this machine's session nor switches to the agent format.
func (s session) canyon(t *testing.T, dir, stdin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...) //nolint:gosec // the CLI this test built, with the journey's arguments
	cmd.Dir, cmd.Stdin, cmd.Env = dir, strings.NewReader(stdin), cliEnv(t, s.home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("canyon %v: %v", args, err)
	}
	t.Logf("canyon %s → exit %d\n%s%s", strings.Join(args, " "), cmd.ProcessState.ExitCode(), stdout.String(), stderr.String())
	return stdout.String(), cmd.ProcessState.ExitCode()
}

// cliEnv is the CLI's environment with its config in home, a fresh one when home is "".
func cliEnv(t *testing.T, home string) []string {
	if home == "" {
		home = t.TempDir()
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT", "HOME", "XDG_CONFIG_HOME"}, name)
	})
	return append(env, "CANYON_HOST="+host, "HOME="+home, "XDG_CONFIG_HOME="+home, "CANYON_NO_UPDATE_NOTICE=1", "NO_COLOR=1", "CANYON_ASCII=1")
}

type cliFinding struct {
	Rule, File string
	Line       int
	Blocks     bool
	Problem    string // cli/json.go's jsonFinding.Problem: the one line naming what and where
}

// cliRun is `canyon --format json check|deploy` (cli.md §5.4), the keys the journeys read.
type cliRun struct {
	Result     string
	GateRun    string `json:"gate_run"`
	Findings   []cliFinding
	Notes      []struct{ Producer, Text string }
	Deployment *struct{ URL, Env string }
}

func (s session) cliJSON(t *testing.T, dir string, verb ...string) (cliRun, int) {
	t.Helper()
	stdout, code := s.canyon(t, dir, "", append([]string{"--format", "json"}, verb...)...)
	var out cliRun
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("canyon %s answered no JSON (exit %d): %v", strings.Join(verb, " "), code, err)
	}
	return out, code
}

// fixture copies e2e/.work/fixtures/<name> (e2e/fixtures/gen.sh) into a fresh directory, names the
// app id in its canyon.toml, keeping whatever else the file declares, and makes the app as s: check
// and deploy refuse an app the host lacks (cli.md §3.2). A 409 exists is an app an earlier case made.
func fixture(t *testing.T, s session, name, app string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("../.work/fixtures", name))); err != nil {
		t.Fatalf("fixture %s (run e2e/fixtures/gen.sh e2e/.work/fixtures, which generates it): %v", name, err)
	}
	path := filepath.Join(dir, "canyon.toml")
	body, err := os.ReadFile(path) //nolint:gosec // the fixture copy this test made
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`(?m)^app = ".*"\n`).ReplaceAllString(string(body), "")
	if err := os.WriteFile(path, []byte(`app = "`+app+"\"\n"+named), 0o600); err != nil { //nolint:gosec // the fixture copy this test made
		t.Fatal(err)
	}
	code, out := s.api(t, "POST", "/api/v1/projects", map[string]string{"id": app})
	var refused struct{ Error string }
	if code != http.StatusCreated && (code != http.StatusConflict || json.Unmarshal(out, &refused) != nil || refused.Error != "exists") {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, code, out)
	}
	return dir
}

// api is one request to the server as the session's person: body nil, raw bytes (a tar), or a
// value sent as JSON.
func (s session) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var in io.Reader
	var kind string
	switch b := body.(type) {
	case nil:
	case []byte:
		in, kind = bytes.NewReader(b), "application/x-tar"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		in, kind = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, host+path, in)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	if s.cookie != "" {
		req.Header.Set("Cookie", sessionCookie+"="+s.cookie)
	}
	res, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}

// read GETs path, which must answer 200, into v.
func (s session) read(t *testing.T, path string, v any) {
	t.Helper()
	code, body := s.api(t, "GET", path, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// forget deletes a project the journeys made, whatever it holds; one already gone is fine.
func (s session) forget(t *testing.T, id string) {
	t.Helper()
	if code, body := s.api(t, "DELETE", "/api/v1/projects/"+id+"?confirm="+id, nil); code != http.StatusNoContent && code != http.StatusNotFound {
		t.Errorf("deleting %s: %d %s", id, code, body)
	}
}

// timeline is a run's build.log lines so far.
func (s session) timeline(t *testing.T, run string) []string {
	lines, _, _ := s.timelinePage(t, run)
	return lines
}

// liveURL is the url on a run's deploy.completed row (contracts/events.md), "" before the deploy
// completes: where a preview's address is written. The `live at <url>` build.log line is prod's
// alone (runs/pipeline.go, the promote path), so a preview deploy is proved by this row.
func (s session) liveURL(t *testing.T, run string) string {
	_, url, _ := s.timelinePage(t, run)
	return url
}

func (s session) timelinePage(t *testing.T, run string) (lines []string, liveURL string, ended bool) {
	t.Helper()
	var after int64
	for {
		var page struct {
			Rows []struct {
				Kind string
				Data struct{ Line, URL string }
			}
			Next *int64
		}
		s.read(t, fmt.Sprintf("/api/v1/gate-runs/%s/timeline?after=%d&limit=200", run, after), &page)
		for _, r := range page.Rows {
			switch r.Kind {
			case "build.log":
				lines = append(lines, r.Data.Line)
			case "deploy.completed":
				liveURL = r.Data.URL
			}
		}
		if page.Next == nil || len(page.Rows) == 0 {
			return lines, liveURL, page.Next == nil
		}
		after = *page.Next
	}
}

type secretRow struct{ Name, Env, Origin string }

func (s session) secretOf(t *testing.T, project, env, name string) *secretRow {
	t.Helper()
	var list struct{ Secrets []secretRow }
	s.read(t, "/api/v1/projects/"+project+"/secrets", &list)
	for _, r := range list.Secrets {
		if r.Env == env && r.Name == name {
			return &r
		}
	}
	return nil
}
