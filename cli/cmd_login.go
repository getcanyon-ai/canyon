package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// suggested is the host a hint should name: the one this machine would otherwise reach, and the
// demo when nothing names one or what does cannot be used.
func (a *app) suggested() string {
	if host, err := a.host(); err == nil {
		return host
	}
	return defaultHost
}

// runLogin is §8.2: name the host, learn from GET /auth/config how it authenticates, and keep the
// session it issues — through the device grant on a host that authenticates, or the one a demo host
// mints on first contact. The next-step block names only verbs this binary dispatches, for the
// reason §1 gives about the help: a short list means a verb has not landed yet, never that it was
// cut. `canyon deploy`'s line joins it when that verb does.
// given records that a flag appeared on the line, whatever it carried: `--token "$CANYON_TOKEN"`
// with the variable unset is a token this host cannot use, not an absent flag. The value is never
// read, so the CLI never holds it.
type given bool

func (g *given) String() string   { return "" }
func (g *given) Set(string) error { *g = true; return nil }
func (g *given) IsBoolFlag() bool { return true } // a flag whose value is never read needs none, so `--token` and `--from-gate` stand alone

func runLogin(ctx context.Context, a *app, args []string) int {
	var token given
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.Var(&token, "token", "")  // refused with its own line, and never offered: --help lists no flag without a usage
		fs.Lookup("host").Usage = "" // login names its host as an argument and refuses the flag, so it is never offered
	})
	if err != nil {
		return a.usage(err)
	}
	named := ""
	switch {
	case a.hostFlag != "": // item 5's rule: one way to name login's host, and it is the argument
		return a.usage(errors.New("login names its host as an argument: canyon login <url>"))
	case bool(token):
		a.errf("\n  %s canyon login --token is not available.\n\n", a.render.glyph(stateFail))
		a.errf("  Canyon issues no tokens; a person signs in and the session is theirs.\n")
		a.errf("  Sign in instead:  canyon login %s\n", a.suggested())
		return exitError
	case len(rest) > 1:
		return a.usage(fmt.Errorf("login takes one host, and got %q as well", rest[1]))
	case len(rest) == 1:
		named = rest[0]
	default:
		asked, ok := a.askHost()
		if !ok {
			a.errf("\n  %s canyon login needs a host when nothing can answer the prompt.\n\n", a.render.glyph(stateFail))
			a.errf("  canyon login %s\n", a.suggested())
			return exitError
		}
		named = asked
	}
	host, err := validHost("the host you named", named)
	if err != nil {
		return a.usage(err)
	}
	client := newClient(host) // never a.client(), which seeds the jar from the config: login starts from nothing
	auth, err := firstContact(ctx, client)
	if err != nil {
		return a.fail(err)
	}
	client.Public = auth.Public // before the session is read off the jar: it names the cookie
	c, line := &config{Host: host, Public: auth.Public}, "Connected to "+host
	if auth.Auth == "off" {
		if c.Session, err = anonymousSession(ctx, client); err != nil {
			return a.fail(err)
		}
	} else {
		signed, code := a.deviceFlow(ctx, client, host, auth.Setup)
		if code != exitOK {
			return code
		}
		c.Session, line = signed.Session, fmt.Sprintf("Signed in to %s as %s\n    %s", host, visible(signed.Display), visible(signed.Org))
	}
	if err := a.makeDefault(ctx, c); err != nil { // nothing is written before the host has answered
		return a.fail(err)
	}
	a.printf("\n  %s %s\n\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), line)
	if a.form == "" { // canyon demo up signs in with this, and ends on a line of its own
		a.printf("  %-28s    %s\n", "canyon fleet", "see what your org has already built")
	}
	return exitOK
}

// makeDefault writes c to config.json, as login always has, so an older CLI meets the host signed in
// to last with that host's own session. The host config.json named moves to hosts/ with its session
// first, so signing in to a second host keeps the first (cli.md §8.2); a signed-out one has nothing
// to keep. Sessions are ended only once c is saved, and nothing after that ends the login: a hosts/
// file stale for the old host — read before the move writes over it — or for c's is revoked and
// removed, and what goes wrong is said (§8.4).
func (a *app) makeDefault(ctx context.Context, c *config) error {
	prior := a.config()
	stale := a.hostConfig(prior.Host)
	moved := prior.Host != c.Host
	kept := false
	if moved && prior.Session != "" && hostFile(prior.Host) != "" {
		err := prior.saveHost(a.getenv)
		if kept = err == nil; !kept {
			a.errf("\n  %s The session for %s could not be kept: %s\n", a.render.glyph(stateWarn), prior.Host, err)
		}
	}
	if err := c.save(a.getenv); err != nil {
		return err
	}
	a.conf = c
	if moved && stale != nil {
		if path, _, _ := hostPath(a.getenv, prior.Host); !kept {
			a.letGo(prior.Host, removeFile(path))
		}
		if stale.Session != "" && stale.Session != prior.Session {
			a.letGo(prior.Host, revoke(ctx, stale))
		}
	}
	a.letGo(c.Host, a.dropStale(ctx, c))
	return nil
}

// authConfig is GET /auth/config, the one route that proves a host is Canyon (contracts/routes.md).
type authConfig struct {
	Canyon bool   `json:"canyon"`
	Auth   string `json:"auth"`
	Org    string `json:"org"`    // the org's display name: canyon share says "everyone at <org>"
	CLI    string `json:"cli"`    // the version this host was released with; the notice compares it (notice.go)
	Public string `json:"public"` // the host's public URL, which names its session cookie (client.go)
	Setup  bool   `json:"setup"`  // the host is in the setup state, when nobody can approve a sign-in yet
}

// firstContact is the host check, on the Open group. An empty jar coming back with a session proved
// a host once and cannot now: a host that authenticates answers a stranger 401, and that proof would
// call it "not Canyon". A 401 to an Open route is not Canyon's answer either, and never "your
// session has expired" inside the verb that would mint one.
func firstContact(ctx context.Context, client *Client) (authConfig, error) {
	var auth authConfig
	err := client.get(ctx, "/auth/config", &auth)
	switch {
	case unauthenticated(err):
		return auth, &NotCanyon{Host: client.Base, Status: http.StatusUnauthorized}
	case err != nil:
		return auth, err
	case !auth.Canyon || (auth.Auth != "oidc" && auth.Auth != "off"):
		return auth, &NotCanyon{Host: client.Base, Status: http.StatusOK}
	}
	return auth, nil
}

// unauthenticated is a 401 read inside login, where it is never "your session has expired": login
// is the verb that would mint one, and the answer is a host that is not the Canyon it claimed.
func unauthenticated(err error) bool {
	var expired *SessionExpired
	return errors.As(err, &expired)
}

// anonymousSession is the proof for a host whose auth is off: the preferences — one row, registered,
// behind the session stage — read with the jar still empty. Such a host serves that caller as the
// anonymous person and sets no cookie (contracts/routes.md), so the session is "" — or, from a host
// that still mints one, the session it set, which must have a session's shape.
func anonymousSession(ctx context.Context, client *Client) (string, error) {
	var answer map[string]json.RawMessage
	err := client.get(ctx, "/api/v1/preferences", &answer)
	switch {
	case unauthenticated(err): // auth is off, said the host a request ago: nothing here is a session to expire
		return "", &NotCanyon{Host: client.Base, Status: http.StatusUnauthorized}
	case err != nil:
		return "", err
	}
	session := client.session()
	if stacks, ok := answer["stacks"]; !ok || !bytes.HasPrefix(bytes.TrimSpace(stacks), []byte("[")) || (session != "" && !validSession(session)) {
		// An object with no preferences in it, or a cookie of the wrong shape, is some other
		// service: saying so here is the difference between naming the host and quoting the
		// writer's rule about hex characters at a user who never typed one.
		return "", &NotCanyon{Host: client.Base, Status: http.StatusOK}
	}
	return session, nil
}

// deviceGrant and signedIn are POST /auth/device and POST /auth/device/token (contracts/routes.md).
type deviceGrant struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Complete        string `json:"verification_uri_complete"` // the same page with the code filled in
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type signedIn struct {
	Session string `json:"session"`
	Display string `json:"display"`
	Org     string `json:"org"`
}

// loginBackoffCap is the longest wait between two polls of a host that is not answering.
const loginBackoffCap = 30 * time.Second

// deviceFlow is AUTH.md §4.2 as the CLI drives it: one grant, the code and the URL on stderr where
// everything the person is asked lives, then a poll every interval — plus one interval more each
// time the host says slow_down — until a session or a terminal answer. No ceiling of its own: the
// host expires the code, and says so. A host restarting meanwhile is polled through, each wait
// twice the last, until the code has expired by the grant's own clock.
func (a *app) deviceFlow(ctx context.Context, client *Client, host string, setup bool) (signedIn, int) {
	var grant deviceGrant
	if err := client.postJSON(ctx, "/auth/device", struct{}{}, http.StatusOK, &grant); err != nil {
		return signedIn{}, a.fail(err)
	}
	if grant.DeviceCode == "" || grant.UserCode == "" || grant.VerificationURI == "" || grant.Interval < 1 || grant.ExpiresIn < 1 {
		// A 200 with none of RFC 8628's fields is some other service; a Canyon host sends all of
		// them, an interval of nothing would poll it as fast as the loop runs, and a code with no
		// expiry leaves the wait through a restart no end.
		return signedIn{}, a.fail(&NotCanyon{Host: client.Base, Status: http.StatusOK})
	}
	open := cmp.Or(grant.Complete, grant.VerificationURI)
	a.errf("\n  Your code:   %s\n\n  Open %s and approve it.\n", grant.UserCode, open)
	a.errf("  The code expires in %s.\n", plural((grant.ExpiresIn+59)/60, "minute", "minutes"))
	a.errf("\n  Waiting%s\n", a.render.punct("…", "..."))
	interval := time.Duration(grant.Interval) * time.Second
	expires := a.clock.now().Add(time.Duration(grant.ExpiresIn) * time.Second)
	for wait, down := interval, time.Duration(0); ; {
		a.clock.sleep(cmp.Or(down, wait))
		var signed signedIn
		err := client.postJSON(ctx, "/auth/device/token", map[string]string{"device_code": grant.DeviceCode}, http.StatusOK, &signed)
		if err == nil {
			return signed, exitOK
		}
		if said, restarting := hostDown(err); restarting {
			if !a.clock.now().Before(expires) {
				a.errf("\n  %s %s stopped answering (%s), and the code expired while Canyon waited.\n", a.render.glyph(stateFail), host, said)
				a.errf("\n  Run: canyon login %s\n", host)
				return signedIn{}, exitError
			}
			down = max(wait, min(max(down, wait)*2, loginBackoffCap)) // never under the interval slow_down set
			continue
		}
		down = 0
		var api *APIError
		if !errors.As(err, &api) || api.Status != http.StatusBadRequest {
			return signedIn{}, a.fail(err)
		}
		switch api.Code {
		case "authorization_pending":
		case "slow_down":
			wait += interval
		case "expired_token":
			a.errf("\n  %s The code expired before it was approved.\n", a.render.glyph(stateFail))
			if setup { // nobody can approve a code on a host still in setup, which /auth/config said (design/ONBOARDING.md §2)
				a.errf("\n  Canyon is still being set up here, so nobody can approve a sign-in yet — ask whoever installed it.\n")
			}
			a.errf("\n  Run: canyon login %s\n", host)
			return signedIn{}, exitError
		case "access_denied":
			a.errf("\n  %s That sign-in was denied in the browser.\n", a.render.glyph(stateFail))
			return signedIn{}, exitError
		default:
			return signedIn{}, a.fail(err)
		}
	}
}

// hostDown is a poll that reached no answer from Canyon itself — a proxy's 502, 503 or 504 while the
// host restarts, or a request that never got an answer — and what to call it when the wait ends:
// the status, or "no answer", since a transport error's own words repeat the host and say EOF.
func hostDown(err error) (string, bool) {
	var api *APIError
	var notCanyon *NotCanyon
	var expired *SessionExpired
	switch {
	case errors.As(err, &api):
		gateway := api.Status == http.StatusBadGateway || api.Status == http.StatusServiceUnavailable || api.Status == http.StatusGatewayTimeout
		return fmt.Sprintf("%d %s", api.Status, http.StatusText(api.Status)), gateway
	case errors.As(err, &notCanyon), errors.As(err, &expired):
		return "", false
	}
	return "no answer", true
}

// askHost is §8.2's prompt, with the host this machine would reach pre-filled. It is written to
// stderr, where everything the user is asked lives, so stdout carries only what login achieved.
// False is nothing that can answer it: no terminal, or one that closed stdin without a line.
// canPrompt gates on stdin, which the prompt reads — not on stdout, which the format ladder owns:
// `canyon login >out` from a terminal still gets its prompt.
func (a *app) canPrompt() bool {
	if a.render.Format != FormatHuman { // §5.1: a harness is reading, and a pty of its own does not make it able to answer
		return false
	}
	return a.stdinIsTerminal()
}

// stdinIsTerminal is the descriptor half of that question alone, for the one verb that answers a harness holding a terminal differently from a pipe.
func (a *app) stdinIsTerminal() bool {
	if f, ok := a.in.(*os.File); ok {
		return IsTTY(f)
	}
	return a.tty
}

func (a *app) askHost() (string, bool) {
	if !a.canPrompt() {
		return "", false
	}
	offered := a.suggested()
	a.errf("\n  Canyon host [%s]: ", offered)
	line, err := bufio.NewReader(a.in).ReadString('\n')
	switch typed := strings.TrimSpace(line); {
	case typed != "":
		return typed, true
	case err == nil: // Enter alone takes the host the prompt offered
		return offered, true
	}
	return "", false
}
