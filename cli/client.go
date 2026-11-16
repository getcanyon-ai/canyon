package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Client is the CLI's side of contracts/routes.md: one http.Client with a cookie jar per
// invocation, so the session a host mints on first contact survives the calls after it. It sends no
// Origin header — the same-origin guard (contracts #140, server #142) refuses a wrong one and a CLI
// has none to claim — and follows no redirect, which Go would put a Referer on.
type Client struct {
	Base    string
	Public  string // the host's public URL off GET /auth/config, which names its session cookie; "" before login read it
	HTTP    *http.Client
	Harness string // the coding agent the CLI runs under (harnessOf), sent as Canyon-Harness; "" sends none
}

// requestTimeout is what a call waits unless it names its own: every read and every POST that only
// writes a row answers well inside it.
const requestTimeout = 30 * time.Second

// statusOpened is the want of a route that answers 201 for a thing it made and 200 for the one it attached to, both
// read into v: a database session's open (contracts/routes.md).
const statusOpened = -201

const (
	answerLimit   = 8 << 20  // past this a normal answer is a bug, not a page
	timelineLimit = 64 << 20 // a timeline page carries whole SARIF logs (contracts/events.md)
)

func newClient(base string) *Client {
	jar, _ := cookiejar.New(nil) // documented never to fail for nil options, and no path here can
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = requestTimeout // a host that accepts and never answers is "no answer in 30s" on every verb, a stream included; the body is unbounded
	return &Client{Base: base, HTTP: &http.Client{
		Jar:           jar,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// sessionCookie is the name canyon-server sets (cmd/canyon-server/session.go): plain, because
// Secure is set only under TLS and a demo host serves http on loopback.
const sessionCookie = "canyon_session"

// sessionCookieOf is the server's rule for the name (session.go's sessionCookieOf), by the scheme of
// its public URL: __Host- under https, plain under http, where the prefix is invalid. The name a
// session is read under is the name it was set under, so the jar holds it under the one this host
// reads — named from the public URL GET /auth/config answers, so an https host reached over an http
// port-forward is read as it sets it (#964), and from the typed host for a config saved before it.
func (c *Client) sessionCookie() string { return sessionCookieOf(c.page()) }

// page is the host as a person opens it: its public URL once login read it, else the address dialled.
// They differ where the CLI reaches the host by an inside name, as the demo's sample apps do (#2019).
func (c *Client) page() string { return cmp.Or(c.Public, c.Base) }

// name is the host as dialled with its scheme dropped, as the first line and the trust line say it:
// never the public URL, which is the host's word for itself and not the Canyon this CLI asked (cli.md §1).
func (c *Client) name() string {
	_, name, _ := strings.Cut(c.Base, "://")
	return name
}

func sessionCookieOf(base string) string {
	if strings.HasPrefix(strings.ToLower(base), "https://") {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

// session is what this jar holds for the base host, or "" before the host has minted one. Its value
// is never printed: it is the credential this machine keeps.
func (c *Client) session() string {
	base, err := url.Parse(c.Base)
	if err != nil {
		return ""
	}
	for _, cookie := range c.HTTP.Jar.Cookies(base) {
		if cookie.Name == c.sessionCookie() {
			return cookie.Value
		}
	}
	return ""
}

// seed puts a saved session in the jar, so a host that minted one earlier recognises this machine
// on the very first call instead of starting a fresh anonymous one.
func (c *Client) seed(value string) {
	base, err := url.Parse(c.Base)
	if value == "" || err != nil {
		return
	}
	c.HTTP.Jar.SetCookies(base, []*http.Cookie{{Name: c.sessionCookie(), Value: value, Path: "/"}}) //nolint:gosec // an entry in this process's own jar, not a Set-Cookie: the attributes govern nothing here, and Secure would withhold it from the demo's loopback http
}

// APIError is the server's `{error, message, ...context}`. Callers switch on Code, never Message.
type APIError struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]json.RawMessage // what else the envelope carried: run_in_progress names the run, stale_base the head package and who moved it
	Path    string                     // the refused request's path, query included
	Host    string                     // the host that answered, as dialled
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, e.Code) }

// Unreachable is a host nothing is listening at — DNS, dial or refused — §9's first-run failure,
// which earns its block. Any other transport failure is one line: the next move differs.
type Unreachable struct {
	Host string
	Err  error
}

func (e *Unreachable) Error() string { return "can't reach a Canyon host at " + e.Host }
func (e *Unreachable) Unwrap() error { return e.Err }

// NoAnswer is a host that took the connection and said nothing before the call's deadline: a
// blackholed host, which is not a dead port.
// Interrupted is a request the host's connection broke under before an answer came: reset, closed, or cut mid-answer,
// as it is while the host restarts (#2730). Its line is the cause as the connection said it.
type Interrupted struct {
	Host string
	Err  error
}

func (e *Interrupted) Error() string { return e.Host + ": " + e.Err.Error() }
func (e *Interrupted) Unwrap() error { return e.Err }

type NoAnswer struct {
	Host   string
	Within time.Duration
}

func (e *NoAnswer) Error() string { return fmt.Sprintf("%s: no answer in %s", e.Host, e.Within) }

// unanswered is a call no host answered at all, dead port or silence: a session it was to end may
// still work there.
func unanswered(err error) bool {
	var unreachable *Unreachable
	var silent *NoAnswer
	return errors.As(err, &unreachable) || errors.As(err, &silent)
}

// NotCanyon is an answer that is not the contract's: a login page, an unfollowed redirect, a body
// that will not decode.
type NotCanyon struct {
	Host   string
	Status int
}

func (e *NotCanyon) Error() string {
	return fmt.Sprintf("%s answered something that is not Canyon (HTTP %d)", e.Host, e.Status)
}

// SessionExpired is a 401: the session this machine offered is dead, or — Sent false — it offered
// none and was never signed in to that host. It is read once, where every answer is read, and never
// retried: the next request would carry the same session and earn the same answer. cli.md §8.2 is
// the line, and `canyon login` the way back.
type SessionExpired struct {
	Host string
	Sent bool
}

func (e *SessionExpired) Error() string {
	if !e.Sent {
		return "this machine is not signed in to " + e.Host
	}
	return "the session has expired at " + e.Host
}

// do sends one request and reads one answer. No retries: each request either changes something
// or is a poll that comes round again a second later, and a silent second send of the first is
// worse than a message. want is the single status that is an answer — any other 2xx or 3xx is a
// host that is not Canyon; a 4xx or 5xx carries the contract's envelope, which the verb reads
// for the codes it knows (exists, run_in_progress) and otherwise prints.
func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, length int64, header http.Header, want int, within time.Duration, most int64, v any) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if length > 0 { // a file is not one of the readers net/http measures, and the server reads a length, not a chunked body
		req.ContentLength = length
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "canyon/"+Version)
	if c.Harness != "" { // the host keeps it on a review it starts, for its usage report's agents (#2953)
		req.Header.Set("Canyon-Harness", c.Harness)
	}
	maps.Copy(req.Header, header) // the route's own headers, when it has any: the upload's base package
	sent := c.session() != ""
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportError(c.Base, within, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, most+1)) // one byte past the cap tells a cut answer from a whole one
	if err != nil {
		return transportError(c.Base, within, err)
	}
	if int64(len(answer)) > most {
		return fmt.Errorf("%s answered more than %d MiB, which this build will not read", c.Base, most>>20)
	}
	switch {
	case resp.StatusCode == want, want == statusOpened && (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK):
		if v == nil {
			return nil
		}
		if json.Unmarshal(answer, v) != nil {
			return &NotCanyon{Host: c.Base, Status: resp.StatusCode}
		}
		return nil
	case resp.StatusCode/100 == 2, resp.StatusCode/100 == 3:
		return &NotCanyon{Host: c.Base, Status: resp.StatusCode}
	}
	return apiError(c.Base, path, resp.StatusCode, answer, sent)
}

// stream is a GET whose body the caller reads: the caller's context with no deadline of its own,
// and the body handed back unread on a 200 with the answer's headers. `do` gives every call
// requestTimeout and reads the whole body, and either ends a stream that is meant to stay open.
// Anything but a 200 is decoded as `do` decodes it: the envelope's code and message, or NotCanyon.
// accept is the one content type the caller reads (text/plain for a log, application/x-tar for a
// package): a 200 carrying any other is a captive portal's page, not the host's answer.
func (c *Client) stream(ctx context.Context, path, accept string) (io.ReadCloser, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept+", application/json")
	req.Header.Set("Accept-Encoding", "identity") // a compressing proxy would hold lines in its own buffer, and the tail would stop being live
	req.Header.Set("User-Agent", "canyon/"+Version)
	sent := c.session() != ""
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, transportError(c.Base, requestTimeout, err)
	}
	if resp.StatusCode == http.StatusOK {
		if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != accept { // the type itself, not a prefix: x-tarball is not x-tar
			_ = resp.Body.Close()
			return nil, nil, &NotCanyon{Host: c.Base, Status: resp.StatusCode}
		}
		return resp.Body, resp.Header, nil
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit))
	if err != nil {
		return nil, nil, transportError(c.Base, 0, err)
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
		return nil, nil, &NotCanyon{Host: c.Base, Status: resp.StatusCode}
	}
	return nil, nil, apiError(c.Base, path, resp.StatusCode, answer, sent)
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	return c.getUpTo(ctx, path, answerLimit, v)
}

// getUpTo is get for a read whose answer is legitimately large. A timeline page carries whole
// SARIF logs, and cutting one short reads back as a host that is not Canyon.
func (c *Client) getUpTo(ctx context.Context, path string, most int64, v any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, 0, nil, http.StatusOK, requestTimeout, most, v)
}

func (c *Client) postJSON(ctx context.Context, path string, in any, want int, v any) error {
	return c.postWithin(ctx, path, in, want, requestTimeout, v)
}

// postWithin is postJSON with the deadline named, the way getUpTo is get with the answer limit
// named: for a route whose work the server does before it answers rather than on a run the CLI
// polls, where thirty seconds is a client giving up on something already under way.
func (c *Client) postWithin(ctx context.Context, path string, in any, want int, within time.Duration, v any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, "application/json", bytes.NewReader(body), 0, nil, want, within, answerLimit, v)
}

// upload posts a package tar of a known length: plain tar, no gzip — the server reads the body
// straight into tar.NewReader (ingest). base, when there is one, is the Canyon-Base-Package the
// host checks against its current package; force is the one query the route reads as force
// (routes.md row 57), and it is the exact string, not a truthy one.
func (c *Client) upload(ctx context.Context, path string, body io.Reader, length int64, base string, force bool, v any) error {
	var header http.Header
	if base != "" {
		header = http.Header{"Canyon-Base-Package": {base}}
	}
	if force {
		path += "?force=true"
	}
	return c.do(ctx, http.MethodPost, path, "application/x-tar", body, length, header, http.StatusCreated, requestTimeout, answerLimit, v)
}

// del is the shape with no body in either direction: the host answers 204 and there is nothing to
// decode. Every other status is an APIError, and the verb prints the host's own words.
func (c *Client) del(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, "", nil, 0, nil, http.StatusNoContent, requestTimeout, answerLimit, nil)
}

// postPending is delPending for a route that takes a body: true and v read from the 202's body, false on the 204.
func (c *Client) postPending(ctx context.Context, path string, in, v any) (bool, error) {
	err := c.postJSON(ctx, path, in, http.StatusAccepted, v)
	var done *NotCanyon
	if errors.As(err, &done) && done.Status == http.StatusNoContent {
		return false, nil
	}
	return err == nil, err
}

// delPending is del for a route that may answer 202 while something it asked for is still under way:
// true and v read from the 202's body, false on the 204.
func (c *Client) delPending(ctx context.Context, path string, v any) (bool, error) {
	err := c.do(ctx, http.MethodDelete, path, "", nil, 0, nil, http.StatusAccepted, requestTimeout, answerLimit, v)
	var done *NotCanyon
	if errors.As(err, &done) && done.Status == http.StatusNoContent {
		return false, nil
	}
	return err == nil, err
}

// put sends a body and reads the answer into v: a secret's 204 carries none, a share's 200 the team.
func (c *Client) put(ctx context.Context, path string, in any, want int, v any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path, "application/json", bytes.NewReader(body), 0, nil, want, requestTimeout, answerLimit, v)
}

// transportError sorts a failed request: §9's block for a dead port, one line for anything else.
func transportError(host string, within time.Duration, err error) error {
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		err = wrapped.Err // url.Error repeats the URL the line already names
	}
	var op *net.OpError
	var waited net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &waited) && waited.Timeout(): // HTTP/2's wait for the headers among them (#2721)
		return &NoAnswer{Host: host, Within: within}
	case errors.As(err, &op) && op.Op == "dial":
		return &Unreachable{Host: host, Err: err}
	case interruption(err):
		return &Interrupted{Host: host, Err: err}
	}
	return fmt.Errorf("%s: %w", host, err)
}

// interruption is a connection the host broke before its answer ended, as it does while it restarts (#2730): reset,
// closed before the answer came or cut mid-answer, or, over HTTP/2, closed after a GOAWAY on a request it had taken.
// Go's own client sends a request the GOAWAY came before again on a new connection. Anything else the connection
// says, a certificate that doesn't verify among it, is no restart to wait out.
func interruption(err error) bool {
	if resetByPeer(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for ; err != nil; err = errors.Unwrap(err) {
		// net/http bundles its HTTP/2 client and leaves this type unexported, so its name is the one handle on it.
		// TestAnHTTP2GoAwayIsAnInterruption breaks a real HTTP/2 server's connection after a GOAWAY: a Go release that
		// renames the type fails that test, never drops these retries unseen.
		if reflect.TypeOf(err).String() == "http.http2GoAwayError" {
			return true
		}
	}
	return false
}

// apiError reads the contract's envelope, or the status alone when the body is not one. A 401 is
// its own answer before the envelope: whatever the body, the host wants a sign-in.
func apiError(host, path string, status int, body []byte, sent bool) error {
	if status == http.StatusUnauthorized {
		return &SessionExpired{Host: host, Sent: sent}
	}
	e := &APIError{Status: status, Code: "unknown", Message: http.StatusText(status), Path: path, Host: host}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return e
	}
	var code, message string
	hasCode := json.Unmarshal(raw["error"], &code) == nil && code != ""
	hasMessage := json.Unmarshal(raw["message"], &message) == nil && message != ""
	if !hasCode || !hasMessage { // both halves or it is not the contract's envelope: a proxy's {"message"} is not ours
		return e
	}
	e.Code, e.Message = code, message
	delete(raw, "error")
	delete(raw, "message")
	if len(raw) > 0 { // the rest of the envelope is the refusal's evidence, and the verb that knows the code reads it
		e.Extra = raw
	}
	return e
}

// elsewhere is how this verb is pointed at another host: login names its host as an argument, and
// every other verb takes --host. Printing the wrong one offers a line that exits 1.
func (a *app) elsewhere(host string) string {
	switch a.verb.name {
	case "login":
		return "canyon login " + host
	case "logout":
		return "" // logout refuses --host, and a <url> would name a different host than the one it could not reach
	}
	return "canyon " + a.verb.name + " --host " + host
}

// fail is cli.md §9 on stderr: what happened, why, what to run next. Never a stack trace and never
// red — red is a blocker, and a host being down is not one.
func (a *app) fail(err error) int {
	a.unpark(false) // a refusal is its own block: the parked narration is dropped, never printed after it
	// a held production, on whichever verb would change it (contracts/routes.md, The hold)
	if h, admins, app, ok := heldIn(err); ok && a.http != nil {
		return a.refusedHeld(app, a.http.page(), h, admins)
	}
	glyph := a.render.glyph(stateFail)
	var unreachable *Unreachable
	var stale *staleBase
	var api *APIError
	var expired *SessionExpired
	var lacks *notOnHost
	var based *baseRefused
	switch {
	case errors.As(err, &lacks): // guard (a), its own line or block (cli.md §3.2)
		return a.lacked(lacks)
	case errors.As(err, &based): // guard (b) (cli.md §6.6)
		return a.refusedBase(based)
	case errors.As(err, &stale):
		a.movedBlock(stale)
	case errors.As(err, &expired) && !expired.Sent: // before any login, or to another host than the one it signed in to
		a.errf("\n  %s You're not signed in to %s.\n\n  Run: canyon login %s\n", glyph, expired.Host, expired.Host)
	case errors.As(err, &expired):
		a.errf("\n  %s Your session has expired.\n\n  Run: canyon login %s\n", glyph, expired.Host)
	case errors.As(err, &unreachable):
		a.errf("\n  %s Can't reach a Canyon host at %s\n\n", glyph, unreachable.Host)
		a.errf("  Is the demo running?           canyon demo up\n")
		if line := a.elsewhere("https://canyon.example.com"); line != "" {
			a.errf("  Running somewhere else?        %s\n", line)
		}
	case errors.As(err, &api) && api.Code == "team_required" && slices.Contains(ownersVerbs, a.verb.name):
		a.errf("\n  %s %s\n", glyph, ownersOnly(api))
	case errors.As(err, &api) && api.Code == "team_required":
		a.errf("\n  %s %s\n", glyph, a.askToJoin(a.ctx, api))
	case errors.As(err, &api):
		a.errf("\n  %s %s\n", glyph, api.Message)
	default:
		a.errf("\n  %s %s\n", glyph, err)
	}
	return exitError
}

// askToJoin is §9's line for a 403 team_required: the first owner, else a member of the first owning
// group, else an admin, and the command
// that adds this person as a contributor on the refused request's app, the address read within the
// verb's context. Without an app (a run's route) or an address, the host's own sentence stands.
func (a *app) askToJoin(ctx context.Context, api *APIError) string {
	var owners []actorRef
	var prefs struct {
		Viewer struct {
			Email string `json:"email"`
		} `json:"viewer"`
	}
	app, named := pathApp(api.Path)
	if !named || json.Unmarshal(api.Extra["owners"], &owners) != nil || a.http == nil ||
		a.http.get(ctx, "/api/v1/preferences", &prefs) != nil || prefs.Viewer.Email == "" {
		return api.Message
	}
	var groups []string
	_ = json.Unmarshal(api.Extra["owner_groups"], &groups) // a host before owner_groups answers none
	who := "an admin"
	switch {
	case len(owners) > 0:
		who = cell(owners[0].Display) + ", an owner of " + cell(app) + ","
	case len(groups) > 0:
		who = "a member of the " + cell(groups[0]) + " group, which owns " + cell(app) + ","
	}
	return "Ask " + who + " to run: canyon share " + cell(prefs.Viewer.Email) + " --level contributor --app " + cell(app) // theirs to run, never the reader's (#1663)
}

// ownersVerbs are the verbs whose routes need owner (contracts/routes.md, App teams): being added as a
// contributor would only trade their team_required for owner_required (§9; #2357).
var ownersVerbs = []string{"share", "rename", "delete"}

// ownersOnly is §9's line for an owner's verb refused team_required: owner_required's sentence, whom to ask
// read off the refusal as the host reads it. Without an app the host's own sentence stands.
func ownersOnly(api *APIError) string {
	var owners []actorRef
	var groups []string
	app, named := pathApp(api.Path)
	if !named || json.Unmarshal(api.Extra["owners"], &owners) != nil {
		return api.Message
	}
	_ = json.Unmarshal(api.Extra["owner_groups"], &groups) // a host before owner_groups answers none
	who := "an admin"
	switch {
	case len(owners) > 0:
		who = cell(owners[0].Display)
	case len(groups) > 0:
		who = "a member of the " + cell(groups[0]) + " group"
	}
	return "only the owners of " + cell(app) + " and admins can do this: ask " + who
}

// pathApp is the app a route under /api/v1/projects/ names; false for a route that names none (a run's).
func pathApp(path string) (string, bool) {
	rest, named := strings.CutPrefix(path, "/api/v1/projects/")
	escaped, _, _ := strings.Cut(rest, "/")
	escaped, _, _ = strings.Cut(escaped, "?")
	app, err := url.PathUnescape(escaped)
	return app, named && err == nil && app != ""
}
