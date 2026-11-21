package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A hosted scenario's agent reaches Canyon by URL alone (design/specs/mcp.md §2), over a grant a person gave once per
// scope, before the round: `agent consent` runs the OAuth flow as a native client (DCR, PKCE, a loopback redirect)
// and keeps the access token in the hosted template HOME. Every scenario's agent sends that token as a header and
// nothing refreshes it: a rotated refresh token presented twice ends the session (oauth.go), and N copies would. So
// a round fits inside the token's hour.
const tokenDir = ".canyon-qa"

// hostedFor is the least time a grant must still have when a scenario starts: its turns' longest run.
const hostedFor = 40 * time.Minute

// consent is `agent consent <tester-home> <hosted-home> read|act`.
func consent(args []string) error {
	if len(args) != 3 || (args[2] != "read" && args[2] != "act") {
		return fmt.Errorf("usage: %s", usage)
	}
	s, err := signedIn(args[0])
	if err != nil {
		return err
	}
	hosted, scope := args[1], args[2]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := postJSON(s.host+"/oauth/register", map[string]any{"client_name": "Canyon QA runner", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code"}, "response_types": []string{"code"}, "token_endpoint_auth_method": "none"}, &reg); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	verifier, state := random(32), random(16)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect}, "scope": {scope}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {s.host + "/mcp"}}
	fmt.Printf("Open this in a browser signed in to Canyon as the person the grant is for, and allow it (%s):\n\n  %s/oauth/authorize?%s\n\n", scope, s.host, q.Encode())
	codes := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "Canyon QA runner: done, close this tab.\n")
		select {
		case codes <- r.URL.Query().Get("code"):
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	var code string
	select {
	case code = <-codes:
	case <-time.After(10 * time.Minute): // the consent request's own life (oauthRequestLife)
		return errors.New("no one allowed it within 10 minutes")
	}
	if code == "" {
		return errors.New("the person denied it")
	}
	resp, err := http.PostForm(s.host+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {reg.ClientID}, "code_verifier": {verifier}, "resource": {s.host + "/mcp"}})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		Access  string `json:"access_token"`
		Expires int    `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.Access == "" {
		return fmt.Errorf("POST /oauth/token: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Join(hosted, tokenDir), 0o700); err != nil {
		return err
	}
	until := time.Now().Add(time.Duration(tok.Expires) * time.Second).UTC().Format(time.RFC3339)
	body := strings.Join([]string{tok.Access, until, s.host}, "\n") + "\n" // ponytail: the refresh token is never kept, so nothing can rotate it
	if err := os.WriteFile(filepath.Join(hosted, tokenDir, "token-"+scope), []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Printf("%s grant kept in %s, until %s\n", tok.Scope, filepath.Join(hosted, tokenDir), until)
	return nil
}

func postJSON(at string, body, into any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, at, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, said)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// grant is a kept token: its value, until when it holds, and the host it is for.
type grant struct{ token, host string }

func readGrant(hosted, scope string) (grant, error) {
	raw, err := os.ReadFile(filepath.Join(hosted, tokenDir, "token-"+scope))
	if err != nil {
		return grant{}, fmt.Errorf("no %s grant in %s: run agent consent first: %w", scope, hosted, err)
	}
	f := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(f) != 3 {
		return grant{}, fmt.Errorf("%s's %s grant does not read: run agent consent again", hosted, scope)
	}
	until, err := time.Parse(time.RFC3339, f[1])
	if err != nil || time.Until(until) < hostedFor {
		return grant{}, fmt.Errorf("%s's %s grant ends at %s, sooner than a scenario may take: run agent consent again", hosted, scope, f[1])
	}
	return grant{f[0], f[2]}, nil
}

// hostedHome copies the hosted template HOME into the scenario's, without the kept grants, and points Claude Code's
// one MCP server at Canyon's URL with the scope's token. The agent's PATH has claude and the system's tools, never a
// canyon: noCLI checks.
func hostedHome(template, dir, scope string) (string, grant, error) {
	g, err := readGrant(template, scope)
	if err != nil {
		return "", grant{}, err
	}
	home := filepath.Join(dir, "home")
	if err := copyHome(template, home); err != nil {
		return "", grant{}, err
	}
	if err := os.RemoveAll(filepath.Join(home, tokenDir)); err != nil { // a read scenario never holds the act grant
		return "", grant{}, err
	}
	conf := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		if err := json.Unmarshal(raw, &conf); err != nil {
			return "", grant{}, fmt.Errorf("the hosted template's .claude.json does not read: %w", err)
		}
	}
	conf["mcpServers"] = map[string]any{"canyon": map[string]any{"type": "http", "url": g.host + "/mcp",
		"headers": map[string]string{"Authorization": "Bearer " + g.token}}}
	raw, _ := json.MarshalIndent(conf, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), raw, 0o600); err != nil {
		return "", grant{}, err
	}
	claude, err := exec.LookPath("claude")
	if err == nil {
		claude, err = filepath.EvalSymlinks(claude)
	}
	if err != nil {
		return "", grant{}, fmt.Errorf("claude: %w", err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return "", grant{}, err
	}
	_ = os.Remove(filepath.Join(bin, "claude"))
	return home, g, os.Symlink(claude, filepath.Join(bin, "claude"))
}

// copyHome copies a template HOME, links as links (Library/Keychains, Claude Code's own sign-in).
func copyHome(template, home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-Rp", template+"/.", home).CombinedOutput(); err != nil {
		return fmt.Errorf("copying %s: %v: %s", template, err, out)
	}
	return nil
}

// hostedEnv is a hosted agent's environment: testerEnv's, with the operator's ~/.local/bin (which may hold a canyon)
// swapped for the HOME's own, holding claude alone.
func hostedEnv(home, app string) []string {
	env := testerEnv(home, app)
	env[0] = "PATH=" + strings.Join([]string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, ":")
	return env
}

// noCLI fails a hosted scenario before turn 1 when a canyon is reachable: on the agent's PATH or anywhere in its HOME.
func noCLI(home string, env []string) error {
	for _, dir := range strings.Split(strings.TrimPrefix(env[0], "PATH="), ":") {
		if _, err := os.Stat(filepath.Join(dir, "canyon")); err == nil {
			return fmt.Errorf("via: mcp-remote: %s holds a canyon, on the agent's PATH", dir)
		}
	}
	return filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "canyon" && !d.IsDir() {
			return fmt.Errorf("via: mcp-remote: the agent's HOME holds %s", path)
		}
		return nil
	})
}

// pushSeed pushes the committed seed to a branch of its own in repo, as the operator's push identity: pushAs is a gh
// config directory whose token reaches that repository alone. The agent's own pushes use the same.
func pushSeed(app, home, repo, branch, pushAs string) error {
	helper := "!GH_CONFIG_DIR=" + pushAs + " gh auth git-credential"
	for _, args := range [][]string{
		{"remote", "add", "origin", "https://github.com/" + repo + ".git"},
		{"checkout", "-qb", branch},
		{"-c", "credential.helper=", "-c", "credential.helper=" + helper, "push", "-q", "origin", "HEAD:refs/heads/" + branch},
		{"config", "--file", filepath.Join(home, ".gitconfig"), "credential.https://github.com.helper", helper},
		{"config", "--file", filepath.Join(home, ".gitconfig"), "user.name", "tester"},
		{"config", "--file", filepath.Join(home, ".gitconfig"), "user.email", "tester@example.com"},
	} {
		if err := git(app, args...); err != nil {
			return err
		}
	}
	return nil
}

// dropBranch deletes the scenario's branch once its turns are over.
func dropBranch(app, branch, pushAs string) {
	if err := git(app, "-c", "credential.helper=", "-c", "credential.helper=!GH_CONFIG_DIR="+pushAs+" gh auth git-credential",
		"push", "-q", "origin", "--delete", branch); err != nil {
		fmt.Printf("%s: branch %s not deleted: %v\n", filepath.Base(app), branch, err)
	}
}

// link links the app the tester made to repo, as its owner (contracts/routes.md, PUT /api/v1/projects/:id/repository).
func link(home, app, repo string) error {
	s, err := signedIn(home)
	if err != nil {
		return err
	}
	a, err := s.call(http.MethodPut, "/api/v1/projects/"+app+"/repository", map[string]string{"full_name": repo})
	if err != nil {
		return err
	}
	if a.status != http.StatusOK {
		return fmt.Errorf("linked repo: PUT /api/v1/projects/%s/repository: %s", app, a)
	}
	return nil
}

// waitDisconnected waits until the grant is refused: a person disconnected the agent in Canyon.
func waitDisconnected(ctx context.Context, g grant, wait time.Duration) error {
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.host+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"canyon-qa","version":"0"}}}`))
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(patchEvery):
		}
	}
	return errors.New("no person disconnected the agent")
}

// madeApp is canyon_create_app's line for an app it made (cli/mcp_remote.go, createdApp): the id is in the parentheses.
var madeApp = regexp.MustCompile(`Made [^\n]*? \(([a-z0-9][a-z0-9-]{0,54})\), linked to `)

// agentApps are the apps the agent made with canyon_create_app, read from its turns, so they go offline with the run.
func agentApps(dir string) []string {
	var ids []string
	files, _ := filepath.Glob(filepath.Join(dir, "turn-*.jsonl"))
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		for _, m := range madeApp.FindAllSubmatch(raw, -1) {
			ids = append(ids, string(m[1]))
		}
	}
	return ids
}

// secretShape is a value setSecrets makes up: one in a transcript is a secret the agent saw.
var secretShape = regexp.MustCompile(`sk-proj-[0-9a-f]{40}`)

func secretSeen(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "turn-*.jsonl"))
	for _, f := range files {
		if raw, _ := os.ReadFile(f); secretShape.Match(raw) {
			return true
		}
	}
	return false
}

// keepToken keeps the grant's token beside the transcripts, mode 600, so the scorer can redact it.
func keepToken(dir string, g grant) error {
	return os.WriteFile(filepath.Join(dir, ".token"), []byte(g.token+"\n"), 0o600)
}
