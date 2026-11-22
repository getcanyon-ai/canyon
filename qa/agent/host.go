package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// session is the tester's own canyon sign-in, for the runner's calls around the agent's turns.
type session struct{ host, cookie, value string }

// answer is a route's reply: its status and its error code, when it carries one.
type answer struct {
	status int
	code   string
	said   string
}

func (a answer) String() string { return strings.TrimSpace(fmt.Sprintf("%d %s", a.status, a.said)) }

func signedIn(home string) (session, error) {
	raw, err := os.ReadFile(filepath.Join(home, ".config", "canyon", "config.json"))
	if err != nil {
		return session{}, fmt.Errorf("the tester's canyon session: %w", err)
	}
	// a host with sign-in off has no session to give: canyon login records its host and an empty session there, and
	// the runner calls it with no cookie, which a host with sign-in refuses (401), so an unsigned tester stops the run
	var conf struct{ Host, Public, Session string }
	if err := json.Unmarshal(raw, &conf); err != nil || conf.Host == "" {
		return session{}, fmt.Errorf("the tester's canyon config names no host: run canyon login <host> first")
	}
	cookie := "canyon_session" // the CLI's name for it, by the public URL's scheme (cli/client.go)
	if strings.HasPrefix(strings.ToLower(conf.Public+conf.Host), "https:") {
		cookie = "__Host-" + cookie
	}
	return session{strings.TrimSuffix(conf.Host, "/"), cookie, conf.Session}, nil
}

func (s session) call(method, path string, body any) (answer, error) {
	var b io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		b = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.host+path, b)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.value != "" { // a host with sign-in off: no cookie at all
		req.Header.Set("Cookie", s.cookie+"="+s.value)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	said, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // a status read carries its receipt
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(said, &e)
	return answer{resp.StatusCode, e.Error, string(said)}, nil
}

// claim creates the scenario's app as the tester before its first turn, so the tester owns it and
// the batch never touches an app it did not make: one that exists is refused, since a fresh copy
// cannot deploy over it (the base guard) and its cleanup would take an earlier batch's app offline.
// Then each policy: line's rule gets that state for the app alone, as its owner may (contracts/
// routes.md, PUT /api/v1/projects/:id/policies/:rule). The agent never sees any of it happen.
func claim(home, app string, policies []policy) error {
	s, err := signedIn(home)
	if err != nil {
		return err
	}
	a, err := s.call(http.MethodPost, "/api/v1/projects", map[string]string{"id": app})
	switch {
	case err != nil:
		return err
	case a.status == http.StatusConflict:
		return fmt.Errorf("%s already exists on this host, from an earlier batch: run with a new --suffix", app)
	case a.status != http.StatusCreated:
		return fmt.Errorf("POST /api/v1/projects: %s", a)
	}
	for _, p := range policies {
		path := "/api/v1/projects/" + app + "/policies/" + p.Rule + "?apply=true"
		if a, err := s.call(http.MethodPut, path, map[string]string{"state": p.State}); err != nil {
			return err
		} else if a.status != http.StatusOK {
			return fmt.Errorf("PUT %s: %s", path, a)
		}
		fmt.Printf("%s: %s %s for this app\n", app, p.Rule, p.State)
	}
	return nil
}

// undeploy takes the scenario's app offline in preview and production once its turns are over
// (DELETE /api/v1/projects/:id/envs/:env: data and secrets kept). The app is the one canyon.toml
// names, whatever the agent called it. A run a cut-off turn left switching the environment stops it
// once it lands (202 {stopping}), so it asks again until settleFor, when the environment is gone.
// Each answer is printed and kept in undeploy.log.
func undeploy(home, dir, app string) {
	name := appOf(app)
	say := func(env, what string) {
		line := fmt.Sprintf("%s %s: %s", name, env, what)
		fmt.Println(line)
		if f, err := os.OpenFile(filepath.Join(dir, "undeploy.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = fmt.Fprintln(f, line)
			_ = f.Close()
		}
	}
	s, err := signedIn(home)
	if err != nil {
		say("both", err.Error())
		return
	}
	for _, env := range []string{"preview", "prod"} {
		for deadline := time.Now().Add(settleFor); ; time.Sleep(settleEvery) {
			a, err := s.call(http.MethodDelete, "/api/v1/projects/"+name+"/envs/"+env, nil)
			switch {
			case err != nil:
				say(env, err.Error())
			case a.status == http.StatusNoContent:
				say(env, "taken offline, data kept")
			case a.status == http.StatusNotFound && a.code == "not_found":
				say(env, "nothing running")
			case a.status == http.StatusAccepted && stopping(a) && time.Now().Before(deadline):
				continue
			default:
				say(env, a.String())
			}
			break
		}
	}
}

// appOf is the app canyon.toml names in the app's directory, whatever the agent called it; else the
// directory's own name, the app the runner claimed.
func appOf(app string) string {
	var file struct {
		App string `toml:"app"`
	}
	if raw, err := os.ReadFile(filepath.Join(app, "canyon.toml")); err == nil && toml.Unmarshal(raw, &file) == nil && file.App != "" {
		return file.App
	}
	return filepath.Base(app)
}

// waitAsked waits until Canyon asks a person to approve its fix for the app's production: the status
// read's envs.prod.patch.state is asked (contracts/routes.md, envs.<env>.patch), read as the tester, who
// owns the app. A patch Canyon never asks for is the run's failure, never a turn sent early.
func waitAsked(ctx context.Context, home, app string, wait time.Duration) error {
	_, err := waitRead(ctx, home, app, "/api/v1/projects/"+app+"/status", "production's patch reads", "Canyon did not ask to approve its fix for production", wait,
		func(body []byte) (string, bool) {
			var st struct {
				Envs struct {
					Prod struct {
						Patch *struct {
							State string `json:"state"`
						} `json:"patch"`
					} `json:"prod"`
				} `json:"envs"`
			}
			switch err := json.Unmarshal(body, &st); {
			case err != nil:
				return "unread: " + err.Error(), false
			case st.Envs.Prod.Patch == nil:
				return "no patch state", false // patching off on the server, or production not deployed
			}
			return st.Envs.Prod.Patch.State, st.Envs.Prod.Patch.State == "asked"
		})
	return err
}

// waitRead reads path as the tester, who owns the app, every patchEvery until judge says the answer holds what the
// run waits for, and returns that answer. It prints what judge says of each answer, after reads, as it changes, and
// after wait gives up, naming the last (never, then "it reads …") and returning the last answer read: a wait that
// never ends is the run's failure, never a turn sent early.
func waitRead(ctx context.Context, home, app, path, reads, never string, wait time.Duration, judge func([]byte) (string, bool)) ([]byte, error) {
	s, err := signedIn(home)
	if err != nil {
		return nil, err
	}
	var last string
	var body []byte
	for deadline := time.Now().Add(wait); ; {
		state, done := "", false
		if a, err := s.call(http.MethodGet, path, nil); err != nil {
			state = "unread: " + err.Error()
		} else if a.status != http.StatusOK {
			state = "unread: " + a.String()
		} else {
			body = []byte(a.said)
			state, done = judge(body)
		}
		if state != last {
			fmt.Printf("%s: %s %s\n", app, reads, state)
			last = state
		}
		if done {
			return body, nil
		}
		if time.Now().After(deadline) {
			return body, fmt.Errorf("%s: %s within %s: it reads %s", app, never, wait, last)
		}
		select {
		case <-ctx.Done():
			return body, ctx.Err()
		case <-time.After(patchEvery):
		}
	}
}

// restrict sets each egress: line's mode on the app's env, as its owner may (contracts/routes.md, PUT
// /api/v1/projects/:id/egress/:env/mode): allowlist narrows, from any session. The agent never sees it happen.
func restrict(home, app string, modes []egressMode) error {
	s, err := signedIn(home)
	if err != nil {
		return err
	}
	for _, m := range modes {
		path := "/api/v1/projects/" + app + "/egress/" + m.Env + "/mode"
		if a, err := s.call(http.MethodPut, path, map[string]string{"mode": m.Mode}); err != nil {
			return err
		} else if a.status != http.StatusNoContent {
			return fmt.Errorf("PUT %s: %s", path, a)
		}
		fmt.Printf("%s: %s outbound %s\n", app, m.Env, m.Mode)
	}
	return nil
}

// hostDBProd refuses a host whose db_prod, as the preferences read answers it to the tester (contracts/routes.md,
// GET /api/v1/preferences), is not want, a db prod: line; "" wants nothing. Only an admin sets it, from a browser, so a
// batch runs the scenarios its host's setting serves, and a scenario on another stops before its app is made.
func hostDBProd(home, want, approval string) error {
	if want == "" && approval == "" {
		return nil
	}
	s, err := signedIn(home)
	if err != nil {
		return err
	}
	a, err := s.call(http.MethodGet, "/api/v1/preferences", nil)
	if err != nil {
		return err
	}
	var p struct {
		DBProd       *string `json:"db_prod"`
		ProdApproval *bool   `json:"prod_approval"`
	}
	if a.status != http.StatusOK || json.Unmarshal([]byte(a.said), &p) != nil || want != "" && p.DBProd == nil || approval != "" && p.ProdApproval == nil {
		return fmt.Errorf("GET /api/v1/preferences names no db_prod or prod_approval, whichever the scenario needs: %s", a)
	}
	if want != "" && *p.DBProd != want {
		return fmt.Errorf("db prod: %s, and the host's db_prod is %s: no screen sets it yet (#2121), so set it on the host, then run this scenario in its own batch", want, *p.DBProd)
	}
	if on := approval == "on"; approval != "" && *p.ProdApproval != on {
		return fmt.Errorf("prod approval: %s, and the host's is otherwise: an admin sets it in the dashboard, then run this scenario in its own batch", approval)
	}
	return nil
}

// stopping is whether the undeploy answered 202 {stopping}: a run switching the environment stops it.
func stopping(a answer) bool {
	var body struct {
		Stopping *struct{} `json:"stopping"`
	}
	return json.Unmarshal([]byte(a.said), &body) == nil && body.Stopping != nil
}
