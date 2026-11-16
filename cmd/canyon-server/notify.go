package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
)

// Telling the org's chat channels when production needs a person (contracts/routes.md, *Integrations*;
// design/INTEGRATIONS.md; #2725 A2). Once a minute the server's own tick computes every app's needs, as an admin reads
// them, in the fleet read's fixed statements: a need is posted once, again only when it changes in a way a person acts
// on, and one short line when it clears and stays clear for flapWindow. The sender posts the outbox, a channel at a
// time, a second apart, and retries what Slack or Teams answer 429 or 5xx for up to an hour.

const (
	notifyEvery   = time.Minute
	sendEvery     = 2 * time.Second
	flapWindow    = 30 * time.Minute
	perTick       = 20 // messages a channel is posted a minute, past which one says how many more
	retryFor      = time.Hour
	outboxKept    = 30 * 24 * time.Hour
	notifyTickKey = "SELECT pg_try_advisory_xact_lock(hashtext(current_schema() || ':notify-tick'))"
)

// canyonActor is who the tick reads the fleet as, and who audits a channel failing: Canyon, an admin.
var canyonActor = actor{Kind: "canyon", Subject: "integrations", Display: "Canyon"}

// message is what Canyon posts, names only (integrations.schema.json's message): a need opening or clearing, or
// the overflow past perTick.
type message struct {
	Kind   string         `json:"kind"`            // need | overflow
	Need   string         `json:"need,omitempty"`  // the need's kind
	State  string         `json:"state,omitempty"` // open | cleared
	App    string         `json:"app,omitempty"`
	Env    *string        `json:"env,omitempty"`
	Fields map[string]any `json:"fields,omitempty"` // destination, port, version, state, channel: names only
	Since  string         `json:"since,omitempty"`
	More   int            `json:"more,omitempty"`
	Link   string         `json:"link"`
}

// fieldsPosted are the fields a need adds that a message may carry: names, never a value, a log line or a code.
var fieldsPosted = []string{"destination", "port", "version", "state", "channel"}

// openSaid and clearedSaid are D5's lines (design/EXPERIENCE.md, *Integrations*), by the need's kind.
var openSaid = map[string]string{ //nolint:gosec // D5's lines: "secret" names a need's kind, no credential
	"held":           "<app> · production was stopped. It stays stopped until a responder or an admin lets it run again.",
	"down":           "<app> · production isn't running: it stopped.",
	"restarting":     "<app> · production keeps restarting.",
	"refused":        "<app> · production can't reach <destination>. A person on the app's team can allow it in Canyon.",
	"refused_code":   "<app> · production can't reach <destination> on port <port>, and no one can allow it: the code has to change.",
	"approval":       "<app> · <version> waits for a person to approve it for production.",
	"patch":          "<app> · a patch of <version> waits for a person to approve it for production.",
	"secret":         "<app> has a secret in its code.",
	"notify_failing": "Canyon can't post to <channel>: its webhook no longer works. An admin can fix it in Settings › Integrations.",
}

var clearedSaid = map[string]string{ //nolint:gosec // as openSaid
	"held":           "<app> · production is no longer stopped.",
	"down":           "<app> · production is running again.",
	"refused":        "<app> · production's calls to <destination> no longer need a person.",
	"refused_code":   "<app> · production's calls to <destination> on port <port> no longer need a person.",
	"approval":       "<app> · <version> no longer waits for a person to approve it.",
	"patch":          "<app> · the patch of <version> no longer waits for a person to approve it.",
	"secret":         "<app>'s latest review found no secret in its code.",
	"notify_failing": "<channel> no longer needs an admin.",
}

// line is the message in D5's words, an open need's with when it began.
func (m message) line() string {
	if m.Kind == "overflow" {
		return fmt.Sprintf("%d more messages about production: open Canyon.", m.More)
	}
	said, key := openSaid, m.Need
	if m.State == "cleared" {
		said = clearedSaid
	} else if m.Need == "down" && m.Fields["state"] == "restarting" {
		key = "restarting"
	}
	pairs := []string{"<app>", m.App}
	for _, k := range fieldsPosted {
		pairs = append(pairs, "<"+k+">", fmt.Sprint(m.Fields[k]))
	}
	out := strings.NewReplacer(pairs...).Replace(said[key])
	if at, err := time.Parse(time.RFC3339, m.Since); err == nil && m.State == "open" {
		out += " Since " + at.UTC().Format("2 Jan 15:04") + " UTC."
	}
	return out
}

// render is the message as kind's webhook takes it: the line and a button to act, as the summary is.
func render(kind, text, link string) ([]byte, error) {
	if kind == "slack" {
		text = slackEscape(text)
		return json.Marshal(map[string]any{"text": text, "blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
			map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": "Open in Canyon"}, "url": link}}},
		}})
	}
	return json.Marshal(map[string]any{"type": "message", "attachments": []any{map[string]any{
		"contentType": "application/vnd.microsoft.card.adaptive",
		"content": map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4",
			"body":    []any{map[string]any{"type": "TextBlock", "text": text, "wrap": true}},
			"actions": []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in Canyon", "url": link}}},
	}}})
}

// postedNeed is a need a channel is told of: its key, the change that posts it again, and its message.
type postedNeed struct {
	key, fingerprint string
	msg              message
}

// needKey names a need across ticks: the app, the env, the kind, and for a refusal its destination and port.
func needKey(app string, n need) string {
	env := ""
	if n.env != nil {
		env = *n.env
	}
	dest, _ := n.fields["destination"].(string)
	port := ""
	if p, ok := n.fields["port"]; ok {
		port = fmt.Sprint(p)
	}
	return strings.Join([]string{app, env, n.kind, dest, port}, "|")
}

// failingKey names a channel's own need, failing: the org's, no app's.
func failingKey(ch string) string { return "org|notify_failing|" + ch }

// fingerprintOf is what in a need, changed, posts it again: down's state, a version, a hold over a refusal.
func fingerprintOf(n need) string {
	switch n.kind {
	case "down":
		s, _ := n.fields["state"].(string)
		return s
	case "approval", "patch", "secret":
		v, _ := n.fields["version"].(string)
		return v
	case "refused":
		held, _ := n.fields["held"].(bool)
		return strconv.FormatBool(held)
	}
	return ""
}

// messageOf is the need's opening message, its fields the ones a message may carry.
func (a *app) messageOf(app string, n need) message {
	fields := map[string]any{}
	for _, k := range fieldsPosted {
		if v, ok := n.fields[k]; ok {
			fields[k] = v
		}
	}
	link := a.public + "/#/apps/" + app
	switch {
	case app == "":
		link = a.public + "/#/settings/integrations"
	case n.kind == "refused" || n.kind == "refused_code":
		link += "/outbound"
	}
	m := message{Kind: "need", Need: n.kind, State: "open", App: app, Env: n.env, Fields: fields, Link: link}
	if n.since != nil {
		m.Since = stamp(*n.since)
	}
	return m
}

// tickContext is the context the tick reads the fleet in: Canyon's own, an admin, so every app's needs are read
// whole and none hangs on a person having the dashboard open.
func tickContext(ctx context.Context) context.Context {
	return withActor(ctx, signedSession{act: canyonActor, role: "admin"})
}

// postedNow is every need a channel is told of now, and the apps whose production state the read could not tell,
// whose down need is neither posted nor cleared: what the read could not tell is never all clear, nor a new alarm.
func (a *app) postedNow(ctx context.Context) (map[string]postedNeed, map[string]bool, error) {
	ctx = tickContext(ctx)
	facts, err := needFactsOf(ctx, a.pool, "")
	if err != nil {
		return nil, nil, err
	}
	states := a.statesOf(ctx, facts)
	byApp, err := a.needsFrom(ctx, a.pool, facts, states)
	if err != nil {
		return nil, nil, err
	}
	org, err := a.orgNeeds(ctx, a.pool)
	if err != nil {
		return nil, nil, err
	}
	unread, out := map[string]bool{}, map[string]postedNeed{}
	for _, f := range facts {
		if f.runs["prod"] == "canyon" && states[f.id]["prod"] == "unknown" {
			unread[f.id] = true
		}
		for _, n := range byApp[f.id] {
			if posted(n.kind, n.env) {
				k := needKey(f.id, n)
				out[k] = postedNeed{key: k, fingerprint: fingerprintOf(n), msg: a.messageOf(f.id, n)}
			}
		}
	}
	for _, n := range org {
		if n.kind != "notify_failing" { // the licence's need is the dashboard's alone, never posted to a channel (design/EXPERIENCE.md, D5)
			continue
		}
		k := failingKey(n.fields["channel_id"].(string))
		out[k] = postedNeed{key: k, msg: a.messageOf("", n)}
	}
	return out, unread, nil
}

// notifyTick is one minute's diff of what needs a person against what each channel was told, its messages queued.
// One server runs it: the others find the tick's lock held and skip that minute. The needs, which ask the executor,
// are read before the lock and the transaction are taken, so neither is held across that call (#2725's review).
func (a *app) notifyTick(ctx context.Context, now time.Time) error {
	var listening bool
	if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM notify_channels WHERE failing_since IS NULL)").Scan(&listening); err != nil || !listening {
		return err
	}
	current, unread, err := a.postedNow(ctx)
	if err != nil {
		return err
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var mine bool
	if err := tx.QueryRow(ctx, notifyTickKey).Scan(&mine); err != nil || !mine {
		return err
	}
	var channels []string
	rows, err := tx.Query(ctx, "SELECT id FROM notify_channels WHERE failing_since IS NULL ORDER BY id")
	if err == nil {
		channels, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || len(channels) == 0 {
		return err
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range current {
			if !yield(k) {
				return
			}
		}
	})
	for _, ch := range channels {
		if err := a.tickChannel(ctx, tx, ch, now, current, keys, unread); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM notify_outbox WHERE created_at < $1", now.Add(-outboxKept)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tickChannel diffs one channel: what opened or changed is posted, what stayed clear flapWindow posts its clearing.
func (a *app) tickChannel(ctx context.Context, tx pgx.Tx, ch string, now time.Time, current map[string]postedNeed, keys []string, unread map[string]bool) error {
	type was struct {
		fingerprint string
		open        bool
		msg         message
		clearedSeen *time.Time
	}
	posted := map[string]was{}
	rows, err := tx.Query(ctx, "SELECT need_key, fingerprint, open, message, cleared_seen_at FROM notify_posted WHERE channel_id = $1", ch)
	if err != nil {
		return err
	}
	var key string
	var w was
	if _, err := pgx.ForEachRow(rows, []any{&key, &w.fingerprint, &w.open, &w.msg, &w.clearedSeen}, func() error {
		posted[key] = w
		return nil
	}); err != nil {
		return err
	}
	var queue []message
	save := func(k, fingerprint string, open bool, m message, clearedSeen *time.Time) error {
		_, err := tx.Exec(ctx, `INSERT INTO notify_posted (channel_id, need_key, fingerprint, open, message, posted_at, cleared_seen_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (channel_id, need_key) DO UPDATE SET fingerprint = EXCLUDED.fingerprint, open = EXCLUDED.open, message = EXCLUDED.message,
				posted_at = EXCLUDED.posted_at, cleared_seen_at = EXCLUDED.cleared_seen_at`, ch, k, fingerprint, open, m, now, clearedSeen)
		return err
	}
	for _, k := range keys {
		n := current[k]
		if k == failingKey(ch) { // a channel is never told it is failing: it can't be
			continue
		}
		p, ok := posted[k]
		switch {
		case !ok || !p.open || p.fingerprint != n.fingerprint:
			queue = append(queue, n.msg)
			if err := save(k, n.fingerprint, true, n.msg, nil); err != nil {
				return err
			}
		case p.clearedSeen != nil: // back within flapWindow: the same need, nothing to say
			if _, err := tx.Exec(ctx, "UPDATE notify_posted SET cleared_seen_at = NULL WHERE channel_id = $1 AND need_key = $2", ch, k); err != nil {
				return err
			}
		}
	}
	for k, p := range posted {
		if _, still := current[k]; still || !p.open || p.msg.Need == "down" && unread[p.msg.App] {
			continue
		}
		switch {
		case p.clearedSeen == nil:
			if _, err := tx.Exec(ctx, "UPDATE notify_posted SET cleared_seen_at = $3 WHERE channel_id = $1 AND need_key = $2", ch, k, now); err != nil {
				return err
			}
		case now.Sub(*p.clearedSeen) >= flapWindow:
			m := p.msg
			m.State, m.Since = "cleared", ""
			queue = append(queue, m)
			if err := save(k, p.fingerprint, false, p.msg, nil); err != nil {
				return err
			}
		}
	}
	if len(queue) > perTick {
		over := message{Kind: "overflow", More: len(queue) - perTick + 1, Link: a.public + "/#/fleet"}
		queue = append(queue[:perTick-1], over)
	}
	for _, m := range queue {
		if _, err := tx.Exec(ctx, "INSERT INTO notify_outbox (channel_id, message, created_at, next_at) VALUES ($1, $2, $3, $3)", ch, m, now); err != nil {
			return err
		}
	}
	return nil
}

// markPosted makes current what a channel was told, as its summary just told it: none of these needs is then posted
// one by one, and none it was told of before posts its clearing.
func markPosted(ctx context.Context, tx pgx.Tx, ch string, current map[string]postedNeed, now time.Time) error {
	if _, err := tx.Exec(ctx, "DELETE FROM notify_posted WHERE channel_id = $1", ch); err != nil {
		return err
	}
	var keys, fingerprints, messages []string
	for k, n := range current {
		if k == failingKey(ch) {
			continue
		}
		m, err := json.Marshal(n.msg)
		if err != nil {
			return err
		}
		keys, fingerprints, messages = append(keys, k), append(fingerprints, n.fingerprint), append(messages, string(m))
	}
	_, err := tx.Exec(ctx, `INSERT INTO notify_posted (channel_id, need_key, fingerprint, open, message, posted_at)
		SELECT $1, k, f, true, m::jsonb, $5 FROM unnest($2::text[], $3::text[], $4::text[]) AS u(k, f, m)`, ch, keys, fingerprints, messages, now)
	return err
}

// queued is one outbox row the sender leased.
type queued struct {
	id      int64
	channel string
	msg     message
	created time.Time
	tries   int
}

// notifySend posts what is due: leased so two servers never post one message twice, a channel's posts a second
// apart, a 429 or 5xx retried with backoff for retryFor, a 404 or 410 marking the channel failing.
func (a *app) notifySend(ctx context.Context, now time.Time, pause func(time.Duration)) error {
	// 20 a pass under a 5-minute lease, each post at most 1 s apart and 10 s long, so a lease outlives its pass. The
	// lease is the database's clock, one for every server, so a server whose own runs fast never takes a row another
	// still holds (#2725's review).
	rows, err := a.pool.Query(ctx, `UPDATE notify_outbox o SET lease_until = now() + interval '5 minutes', attempts = o.attempts + 1
		WHERE o.id IN (SELECT q.id FROM notify_outbox q JOIN notify_channels c ON c.id = q.channel_id
			WHERE q.status = 'queued' AND q.next_at <= $1 AND (q.lease_until IS NULL OR q.lease_until < now()) AND c.failing_since IS NULL
			ORDER BY q.id LIMIT 20 FOR UPDATE OF q SKIP LOCKED)
		RETURNING o.id, o.channel_id, o.message, o.created_at, o.attempts`, now)
	if err != nil {
		return err
	}
	var due []queued
	var q queued
	if _, err := pgx.ForEachRow(rows, []any{&q.id, &q.channel, &q.msg, &q.created, &q.tries}, func() error {
		due = append(due, q)
		return nil
	}); err != nil {
		return err
	}
	slices.SortStableFunc(due, func(x, y queued) int { return strings.Compare(x.channel, y.channel) })
	last := ""
	for _, d := range due {
		if d.channel == last {
			pause(time.Second) // one a second, as Slack allows an incoming webhook
		}
		last = d.channel
		if err := a.sendOne(ctx, now, d); err != nil {
			return err
		}
	}
	return nil
}

// sendOne posts one message and records what its channel answered.
func (a *app) sendOne(ctx context.Context, now time.Time, d queued) error {
	var kind string
	var failing bool
	if err := a.pool.QueryRow(ctx, "SELECT kind, failing_since IS NOT NULL FROM notify_channels WHERE id = $1", d.channel).Scan(&kind, &failing); err != nil || failing {
		return err
	}
	to, err := a.channelURLOf(ctx, d.channel)
	if err != nil {
		return err
	}
	body, err := render(kind, d.msg.line(), d.msg.Link)
	if err != nil {
		return err
	}
	status, asked, _ := postChannel(ctx, to, body)
	switch {
	case status/100 == 2:
		_, err = a.pool.Exec(ctx, "UPDATE notify_outbox SET status = 'sent', answered = $2, done_at = $3, lease_until = NULL WHERE id = $1", d.id, status, now)
	case status == 404 || status == 410: // the webhook was revoked: nothing more is posted until an admin saves the channel again
		_, err = a.pool.Exec(ctx, "UPDATE notify_outbox SET status = 'dropped', answered = $2, done_at = $3, lease_until = NULL WHERE id = $1", d.id, status, now)
		if err == nil {
			err = a.channelFailing(ctx, d.channel, status, now)
		}
	case (status == 0 || status == 429 || status >= 500) && now.Sub(d.created) < retryFor:
		wait := max(min(time.Duration(1<<min(d.tries, 6))*15*time.Second, 15*time.Minute), asked) // never sooner than the channel asked
		_, err = a.pool.Exec(ctx, "UPDATE notify_outbox SET next_at = $2, answered = nullif($3, 0), lease_until = NULL WHERE id = $1", d.id, now.Add(wait), status)
	default: // past retryFor, or refused outright
		_, err = a.pool.Exec(ctx, "UPDATE notify_outbox SET status = 'dropped', answered = nullif($2, 0), done_at = $3, lease_until = NULL WHERE id = $1", d.id, status, now)
	}
	return err
}

// channelFailing marks a channel failing, once, with its audit row: an admin's need until they save it again.
func (a *app) channelFailing(ctx context.Context, ch string, status int, now time.Time) error {
	return a.inMutation(ctx, func(tx pgx.Tx) error {
		var name, kind, host string
		err := tx.QueryRow(ctx, `UPDATE notify_channels SET failing_since = $2, failing_status = $3 WHERE id = $1 AND failing_since IS NULL RETURNING name, kind, host`,
			ch, now, status).Scan(&name, &kind, &host)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// what was still to post to it is dropped: once saved again, it is told what its summary says, nothing stale
		if _, err := tx.Exec(ctx, "UPDATE notify_outbox SET status = 'dropped', done_at = $2, lease_until = NULL WHERE channel_id = $1 AND status = 'queued'", ch, now); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, canyonActor, "integration.channel.failing", "", "", map[string]any{"id": ch, "name": name, "kind": kind, "host": host, "status": status})
	})
}

// orgNeeds are the org's own needs, an admin's alone (contracts/routes.md, the fleet read's org_needs): each chat
// channel failing, since it started to, then the licence while it is in grace, lapsed or invalid, then patching the
// server is set to do that no licence unlocks (#2879).
func (a *app) orgNeeds(ctx context.Context, q querier) ([]need, error) {
	if roleOf(ctx) != "admin" {
		return nil, nil
	}
	rows, err := q.Query(ctx, "SELECT id, name, failing_since FROM notify_channels WHERE failing_since IS NOT NULL ORDER BY failing_since, id")
	if err != nil {
		return nil, err
	}
	var out []need
	var id, name string
	var since time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &name, &since}, func() error {
		at := since
		out = append(out, newNeed("notify_failing", nil, &at, true, map[string]any{"channel": name, "channel_id": id}))
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, more := range []func(context.Context, querier) (need, bool, error){a.licenceNeed, a.patchingNeed} {
		n, ok, err := more(ctx, q)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// notifyLoop runs the tick once a minute and the sender every sendEvery, until ctx ends.
func (a *app) notifyLoop(ctx context.Context, logger *log.Logger) {
	tick, send := time.NewTicker(notifyEvery), time.NewTicker(sendEvery)
	defer tick.Stop()
	defer send.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.notifyTick(ctx, time.Now()); err != nil {
				logger.Printf("canyon-server: the chat channels' tick: %v", err)
			}
		case <-send.C:
			if err := a.notifySend(ctx, time.Now(), time.Sleep); err != nil {
				logger.Printf("canyon-server: the chat channels' sender: %v", err)
			}
		}
	}
}
