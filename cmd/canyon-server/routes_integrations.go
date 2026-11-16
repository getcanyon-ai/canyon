package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
)

// The org's chat channels (contracts/routes.md, the channels row and *Integrations*; design/INTEGRATIONS.md; #2725):
// where Canyon posts when production needs a person. An admin's alone. The webhook URL is a bearer credential: it is
// sealed in the vault as ("integration", <id>, "webhook_url"), and no read, log, audit or timeline row holds it.
func routesIntegrations(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /integrations/channels", a.adminOnly(a.listChannels))
	api.HandleFunc("PUT /integrations/channels/{id}", a.adminOnly(a.putChannel))
	api.HandleFunc("DELETE /integrations/channels/{id}", a.adminOnly(a.deleteChannel))
	api.HandleFunc("POST /integrations/channels/{id}/test", a.adminOnly(a.testChannel))
}

const (
	maxChannels   = 5
	postTimeout   = 10 * time.Second
	slackURLSaid  = "The webhook URL must be an https address on hooks.slack.com."
	teamsURLSaid  = "The webhook URL must be an https address of a Teams workflow, on logic.azure.com, webhook.office.com or powerplatform.com."
	tooManySaid   = "Canyon posts to at most 5 channels. Remove one first."
	summaryLead   = "Canyon will post here when production needs a person."
	channelPerson = "A person adds or changes a chat channel in Canyon's dashboard: Settings › Integrations."
)

// notifyTransport carries a post to a channel; nil is http.DefaultTransport. A test points it at its own server.
var notifyTransport http.RoundTripper

var channelIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// channelHosts is where each kind of channel posts: a URL on any other host is refused, so a typo or a pasted link
// never makes Canyon post elsewhere.
var channelHosts = map[string]func(host string) bool{
	"slack": func(host string) bool { return host == "hooks.slack.com" },
	"teams": func(host string) bool {
		for _, s := range []string{".logic.azure.com", ".webhook.office.com", ".powerplatform.com"} {
			if strings.HasSuffix(host, s) && len(host) > len(s) {
				return true
			}
		}
		return false
	},
}

// channelURL is raw checked as a kind's webhook URL: https, no userinfo, its host one the kind posts to; the
// refusal is D5's words.
func channelURL(kind, raw string) (*url.URL, string) {
	said := map[string]string{"slack": slackURLSaid, "teams": teamsURLSaid}[kind]
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.User != nil || u.Port() != "" || !channelHosts[kind](strings.ToLower(u.Hostname())) {
		return nil, said
	}
	return u, ""
}

// printableName is a channel's name: 1 to 80 characters, none a control or format character.
func printableName(s string) bool {
	n := 0
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
		n++
	}
	return n >= 1 && n <= 80
}

// channelRow is a channel as the list answers it: never its URL.
type channelRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Host    string   `json:"host"`
	Tail    string   `json:"tail"`
	SetBy   actor    `json:"set_by"`
	SetAt   string   `json:"set_at"`
	Failing *failing `json:"failing"`
}

// failing is a channel Slack or Teams stopped taking messages on: since when, and what its webhook answered.
type failing struct {
	Since  string `json:"since"`
	Status int    `json:"status"`
}

func (a *app) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := a.pool.Query(r.Context(), "SELECT id, name, kind, host, tail, set_by_kind, set_by_subject, set_by_display, set_at, failing_since, failing_status FROM notify_channels ORDER BY id")
	if err != nil {
		a.channelFailed(w, err)
		return
	}
	var at time.Time
	var since *time.Time
	var status *int
	var c channelRow
	out := []channelRow{}
	if _, err := pgx.ForEachRow(rows, []any{&c.ID, &c.Name, &c.Kind, &c.Host, &c.Tail, &c.SetBy.Kind, &c.SetBy.Subject, &c.SetBy.Display, &at, &since, &status}, func() error {
		c.SetAt, c.Failing = stamp(at), nil
		if since != nil && status != nil {
			c.Failing = &failing{Since: stamp(*since), Status: *status}
		}
		out = append(out, c)
		return nil
	}); err != nil {
		a.channelFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (a *app) putChannel(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	// It widens who reads every app's name: a person's, in the dashboard (#2725). Its denied rows name the channel as
	// their target, never as a project: a channel is no app (#2821). So refuseWidening, which names r.PathValue("id")
	// as the app, is not used here.
	switch {
	case actorOf(ctx).Kind != "person":
		writeErr(w, http.StatusForbidden, "person_required", channelPerson, nil)
		denyOn(ctx, "person_required", "channel", id)
		return
	case a.auth == authOIDC && !fromDashboard(ctx):
		writeErr(w, http.StatusForbidden, "dashboard_required", "change it in Canyon's dashboard", nil)
		denyOn(ctx, "dashboard_required", "channel", id)
		return
	}
	var body struct{ Name, Kind, URL string }
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF || !channelIDRe.MatchString(id) || !printableName(body.Name) || channelHosts[body.Kind] == nil { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "invalid_channel", "the body is {name, kind, url}: 1 to 80 printable characters, slack or teams, and the channel's https webhook URL; the id is 1 to 40 of a-z, 0-9 and -, beginning with a letter", nil)
		return
	}
	raw := body.URL
	if raw == "" { // a change keeps the URL it was saved with
		var err error
		if raw, err = a.channelURLOf(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusBadRequest, "invalid_channel", map[string]string{"slack": slackURLSaid, "teams": teamsURLSaid}[body.Kind], nil)
			return
		} else if err != nil {
			a.channelFailed(w, err)
			return
		}
	}
	u, said := channelURL(body.Kind, raw)
	if u == nil {
		writeErr(w, http.StatusBadRequest, "invalid_channel", said, nil)
		return
	}
	var held int
	var exists bool
	if err := a.pool.QueryRow(ctx, "SELECT count(*), coalesce(bool_or(id = $1), false) FROM notify_channels", id).Scan(&held, &exists); err != nil {
		a.channelFailed(w, err)
		return
	}
	if !exists && held >= maxChannels {
		writeErr(w, http.StatusConflict, "too_many_channels", tooManySaid, nil)
		return
	}
	if status, err := a.postSummary(ctx, body.Kind, u.String()); err != nil || status/100 != 2 {
		channelUnreachable(w, body.Kind, status, " Nothing was saved.")
		return
	}
	current, _, err := a.postedNow(ctx) // what the summary just said, made what the channel was told
	if err != nil {
		a.channelFailed(w, err)
		return
	}
	by := actorOf(ctx)
	tail := raw[max(0, len(raw)-4):]
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		version, sealedKey, sealedURL, err := a.vault.Seal("integration", id, "webhook_url", []byte(raw))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO notify_channels (id, name, kind, host, tail, key_ref, sealed_key, sealed_url, set_by_kind, set_by_subject, set_by_display)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind, host = EXCLUDED.host, tail = EXCLUDED.tail, key_ref = EXCLUDED.key_ref,
				sealed_key = EXCLUDED.sealed_key, sealed_url = EXCLUDED.sealed_url, set_by_kind = EXCLUDED.set_by_kind, set_by_subject = EXCLUDED.set_by_subject,
				set_by_display = EXCLUDED.set_by_display, set_at = now(), failing_since = NULL, failing_status = NULL`,
			id, body.Name, body.Kind, u.Hostname(), tail, version, sealedKey, sealedURL, by.Kind, by.Subject, by.Display); err != nil {
			return err
		}
		if err := markPosted(ctx, tx, id, current, time.Now()); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, by, "integration.channel.set", "", "", map[string]any{"id": id, "name": body.Name, "kind": body.Kind, "host": u.Hostname()})
	})
	if err != nil {
		a.channelFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": channelRow{ID: id, Name: body.Name, Kind: body.Kind, Host: u.Hostname(), Tail: tail, SetBy: by, SetAt: stamp(time.Now())}})
}

func (a *app) deleteChannel(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	gone := false
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		var name, kind, host string
		if err := tx.QueryRow(ctx, "DELETE FROM notify_channels WHERE id = $1 RETURNING name, kind, host", id).Scan(&name, &kind, &host); errors.Is(err, pgx.ErrNoRows) {
			gone = true
			return nil
		} else if err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, actorOf(ctx), "integration.channel.removed", "", "", map[string]any{"id": id, "name": name, "kind": kind, "host": host})
	})
	switch {
	case err != nil:
		a.channelFailed(w, err)
	case gone:
		writeErr(w, http.StatusNotFound, "not_found", "no channel "+id, nil)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) testChannel(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var name, kind, host string
	if err := a.pool.QueryRow(ctx, "SELECT name, kind, host FROM notify_channels WHERE id = $1", id).Scan(&name, &kind, &host); errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "no channel "+id, nil)
		return
	} else if err != nil {
		a.channelFailed(w, err)
		return
	}
	raw, err := a.channelURLOf(ctx, id)
	if err != nil {
		a.channelFailed(w, err)
		return
	}
	status, postErr := a.postSummary(ctx, kind, raw)
	if err := a.inMutation(ctx, func(tx pgx.Tx) error {
		return events.AuditTx(ctx, tx, actorOf(ctx), "integration.channel.tested", "", "", map[string]any{"id": id, "name": name, "kind": kind, "host": host, "status": status})
	}); err != nil {
		a.channelFailed(w, err)
		return
	}
	if postErr != nil || status/100 != 2 {
		channelUnreachable(w, kind, status, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// channelURLOf opens a channel's sealed webhook URL.
func (a *app) channelURLOf(ctx context.Context, id string) (string, error) {
	var version int
	var sealedKey, sealedURL []byte
	if err := a.pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_url FROM notify_channels WHERE id = $1", id).Scan(&version, &sealedKey, &sealedURL); err != nil {
		return "", err
	}
	opened, err := a.vault.Open(version, "integration", id, "webhook_url", sealedKey, sealedURL)
	return string(opened), err
}

// channelUnreachable is a post's refusal in D5's words: what the channel answered, or that it didn't in time, and a
// URL its kind says is gone, said so. Never the URL.
func channelUnreachable(w http.ResponseWriter, kind string, status int, tail string) {
	said := fmt.Sprintf("Canyon couldn't post to this channel: it answered %d.%s", status, tail)
	switch status {
	case 0:
		said = "Canyon couldn't post to this channel: it didn't answer in 10 seconds." + tail
	case http.StatusNotFound, http.StatusGone:
		said = fmt.Sprintf("%s answered %d: that webhook URL doesn't exist or was removed.%s", map[string]string{"slack": "Slack", "teams": "Teams"}[kind], status, tail)
	}
	writeErr(w, http.StatusUnprocessableEntity, "channel_unreachable", said, map[string]any{"status": status})
}

func (a *app) channelFailed(w http.ResponseWriter, err error) {
	a.log.Printf("canyon-server: the chat channels: %v", err) // a vault or a database error: never the URL, which no error here carries
	writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
}

// summary is what a channel's save and its test post (contracts/routes.md, *Integrations*, The summary): production's
// needs open now, as an admin's needs[] reads them.
type summary struct {
	Kind  string   `json:"kind"`
	Needs int      `json:"needs"`
	Apps  []string `json:"apps"`
	More  int      `json:"more"`
	Link  string   `json:"link"`
}

// posted is whether a need of kind in env is one a channel is told of (design/INTEGRATIONS.md, Which needs).
func posted(kind string, env *string) bool {
	switch kind {
	case "held", "down", "refused", "refused_code", "approval", "patch":
		return env != nil && *env == "prod"
	case "secret":
		return true
	}
	return false
}

// summaryNow counts the needs a channel is told of, across every app, as the admin asking reads them.
func (a *app) summaryNow(ctx context.Context) (summary, error) {
	s := summary{Kind: "summary", Apps: []string{}, Link: a.public + "/#/fleet"}
	facts, err := needFactsOf(ctx, a.pool, "")
	if err != nil {
		return s, err
	}
	byApp, err := a.needsFrom(ctx, a.pool, facts, a.statesOf(ctx, facts))
	if err != nil {
		return s, err
	}
	for _, f := range facts {
		n := 0
		for _, need := range byApp[f.id] {
			if posted(need.kind, need.env) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		s.Needs += n
		if len(s.Apps) < 5 {
			s.Apps = append(s.Apps, f.id)
		} else {
			s.More++
		}
	}
	return s, nil
}

// line is the summary in D5's words.
func (s summary) line(escape func(string) string) string {
	if s.Needs == 0 {
		return summaryLead + " Right now nothing in production needs a person."
	}
	names := make([]string, len(s.Apps))
	for i, app := range s.Apps {
		names[i] = escape(app)
	}
	apps := strings.Join(names, ", ")
	switch {
	case s.More > 0:
		apps += fmt.Sprintf(" and %d more", s.More)
	case len(names) > 1:
		apps = strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	needs := fmt.Sprintf("%d production needs", s.Needs)
	if s.Needs == 1 {
		needs = "1 production need"
	}
	return fmt.Sprintf("%s Right now: %s, across %s.", summaryLead, needs, apps)
}

// slackEscape is Slack's mrkdwn escaping: &, < and >.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

// rendered is the summary as kind's webhook takes it (contracts/routes.md, *Integrations*).
func (s summary) rendered(kind string) ([]byte, error) {
	return render(kind, s.line(func(s string) string { return s }), s.Link)
}

// postSummary posts the summary to a channel: the answer's status, 0 with an error when there was none in time.
func (a *app) postSummary(ctx context.Context, kind, to string) (int, error) {
	s, err := a.summaryNow(ctx)
	if err != nil {
		return 0, err
	}
	body, err := s.rendered(kind)
	if err != nil {
		return 0, err
	}
	status, _, err := postChannel(ctx, to, body)
	return status, err
}

// postChannel is one POST of body to a channel's webhook: no redirect followed, postTimeout in all. Its error never
// carries the URL, which net/http's would. The wait is the answer's Retry-After in seconds, 0 for none.
func postChannel(ctx context.Context, to string, body []byte) (int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(body)) //nolint:gosec // to is an admin's webhook URL on its kind's own hosts (channelURL)
	if err != nil {
		return 0, 0, errors.New("the channel's request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: notifyTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req) //nolint:gosec // as above
	if err != nil {
		return 0, 0, errors.New("the channel did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	secs, _ := strconv.Atoi(resp.Header.Get("Retry-After")) // seconds, as Slack and Teams send it: an HTTP date reads as none, and the backoff still waits
	return resp.StatusCode, time.Duration(max(secs, 0)) * time.Second, nil
}
