package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// routesPlatformWebhook registers where releases go under run_by: org (contracts/routes.md, the
// platform-webhook row; design/HAND-OFF.md §3): one row, one host. The secret Canyon signs every
// delivery with (webhook/, PR 3) is sealed in the vault as the front door's client_secret is
// (routes_front_door.go); the host token the org reports with (PR 4) is stored as its SHA-256
// alone. Both are answered once, in the PUT's or the rotate's response, never on GET, and never
// cached (Cache-Control: no-store). A test delivery posts to the saved url (#2439).
func routesPlatformWebhook(api *http.ServeMux, a *app) {
	a.pings = make(chan struct{}, 1)
	api.HandleFunc("GET /platform-webhook", a.getPlatformWebhook)
	api.HandleFunc("PUT /platform-webhook", a.adminOnly(a.putPlatformWebhook))
	api.HandleFunc("POST /platform-webhook/token", a.adminOnly(a.rotatePlatformWebhookToken))
	api.HandleFunc("POST /platform-webhook/secret", a.adminOnly(a.rotatePlatformWebhookSecret))
	api.HandleFunc("POST /platform-webhook/ping", a.adminOnly(a.pingPlatformWebhook))
}

// pingInFlight is D5's line: the ping's 429, one test delivery at a time per server.
const pingInFlight = "A test delivery is being sent already. Try again once it answers."

// noStore marks an answer carrying a secret or a token: no cache along the way keeps it.
func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// errNoWebhook is a rotate with nothing to rotate: there is no url or secret to keep, so there is
// no row a token can belong to.
var errNoWebhook = errors.New("no platform webhook is set")

// storedWebhook is the row: the URL, who set it, and the sealed secret and hashed token — never
// either plaintext.
type storedWebhook struct {
	URL   string
	SetBy actor
	SetAt string

	keyRef                 int
	sealedKey, sealedValue []byte
	tokenHash              []byte
}

func readWebhook(ctx context.Context, q querier) (*storedWebhook, error) {
	var s storedWebhook
	var at time.Time
	err := q.QueryRow(ctx, "SELECT url, set_at, set_by_kind, set_by_subject, set_by_display, key_ref, sealed_key, sealed_value, token_hash FROM platform_webhook").
		Scan(&s.URL, &at, &s.SetBy.Kind, &s.SetBy.Subject, &s.SetBy.Display, &s.keyRef, &s.sealedKey, &s.sealedValue, &s.tokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	s.SetAt = at.UTC().Format(time.RFC3339)
	return &s, err
}

// hostTokenValid is whether presented is the platform webhook's current host token
// (POST /releases/:id/report, PR 4 — the only route a host token authenticates): its SHA-256
// against the stored hash, compared in constant time so no early byte mismatch is observable
// through timing. False, never an error, with no webhook configured.
//
// The deploy repo's row keeps a host token of its own (#2470), which counts too: each row's, never a row made for
// the other's token.
func hostTokenValid(ctx context.Context, q querier, presented string) (bool, error) {
	var hook, repo []byte
	if err := q.QueryRow(ctx, "SELECT (SELECT token_hash FROM platform_webhook), (SELECT token_hash FROM platform_repo)").Scan(&hook, &repo); err != nil {
		return false, err
	}
	return tokenIs(presented, hook) || tokenIs(presented, repo), nil
}

// fetchTokenValid is whether presented is the deploy repo's fetch-only token (#2470), the one External Secrets holds:
// GET /releases/:id/access-token takes it, and nothing else does. False, never an error, with no repo saved.
func fetchTokenValid(ctx context.Context, q querier, presented string) (bool, error) {
	var hash []byte
	err := q.QueryRow(ctx, "SELECT fetch_token_hash FROM platform_repo").Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && tokenIs(presented, hash), err
}

// tokenIs is whether presented is the token stored as hash: its SHA-256 compared in constant time, so no early
// byte mismatch is observable through timing. Never, against no hash.
func tokenIs(presented string, hash []byte) bool {
	sum := sha256.Sum256([]byte(presented))
	return len(hash) == sha256.Size && subtle.ConstantTimeCompare(sum[:], hash) == 1
}

func (a *app) getPlatformWebhook(w http.ResponseWriter, r *http.Request) {
	stored, err := readWebhook(r.Context(), a.pool)
	if err != nil {
		a.log.Printf("canyon-server: read the platform webhook: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if stored == nil { // none saved is a defined answer, never an error status a browser logs (#2560)
		writeJSON(w, http.StatusOK, nil)
		return
	}
	// last_delivery is the newest attempt to deliver a release since the webhook was last saved (#2439): one at an
	// address saved before says nothing of this one. A test delivery records nothing, so it is never this.
	var last map[string]any
	var delivery, release string
	var at time.Time
	var status int
	err = a.pool.QueryRow(r.Context(), `SELECT delivery::text, id, attempted_at, attempt_status FROM releases
		WHERE attempted_at >= (SELECT set_at FROM platform_webhook) ORDER BY attempted_at DESC, created_at DESC LIMIT 1`).Scan(&delivery, &release, &at, &status)
	switch {
	case err == nil:
		last = map[string]any{"delivery": delivery, "release": release, "sent_at": at.UTC().Format(time.RFC3339), "status": status}
	case !errors.Is(err, pgx.ErrNoRows):
		a.log.Printf("canyon-server: read the platform webhook's last delivery: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": stored.URL, "set_by": stored.SetBy, "set_at": stored.SetAt, "last_delivery": last})
}

func (a *app) putPlatformWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		URL    string          `json:"url"`
		Secret json.RawMessage `json:"secret"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validHTTPS(body.URL) {
		writeErr(w, http.StatusBadRequest, "invalid_url", "url: required, https://", nil)
		return
	}
	var secret string
	secretGiven := len(body.Secret) > 0 && string(body.Secret) != "null"
	if secretGiven {
		var ok bool
		if secret, ok = text(body.Secret); !ok || !validToken(secret) {
			writeErr(w, http.StatusBadRequest, "invalid_secret", "secret: 1 to 1024 characters, printable ASCII without spaces", nil)
			return
		}
	}
	var saved *storedWebhook
	// sealedPlaintext is whatever this call just sealed — the body's secret, or one Canyon
	// minted — kept only to seal it below; mintedToken is the plaintext only when this call
	// minted it. Neither is stored, and the response answers sealedPlaintext (as "secret")
	// only when Canyon minted it: a body-given secret is never echoed back.
	var sealedPlaintext, mintedToken string
	// a.mutation takes events.LockTx before this closure's first read, so — even though Postgres's
	// default READ COMMITTED lets two transactions both read "no row" — a concurrent PUT is
	// serialized behind it: the second call's readWebhook only runs after the first has committed
	// (or rolled back), so "the first save" and "the minted token, once" are never a race with
	// another PUT, or with anything else this server mutates.
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		before, err := readWebhook(ctx, tx)
		if err != nil {
			return err
		}
		firstSave := before == nil
		sealedNew := secretGiven || firstSave       // a fresh secret is sealed whenever the body carries one, or there is none stored yet to keep
		secretReplaced := secretGiven && !firstSave // the audit field: nothing is "replaced" on the first save, given or minted — there was nothing before it
		keyRef, sealedKey, sealedValue := 0, []byte(nil), []byte(nil)
		switch {
		case secretGiven:
			sealedPlaintext = secret
		case !firstSave: // a PUT with no secret keeps the stored one (contracts/routes.md, the platform-webhook row): reseal nothing, carry the sealed bytes forward
			keyRef, sealedKey, sealedValue = before.keyRef, before.sealedKey, before.sealedValue
		default:
			if sealedPlaintext, err = token(); err != nil { // Canyon makes one when none is given and none is stored yet, and answers this one alone
				return err
			}
		}
		if sealedNew {
			version, sk, sv, err := a.vault.Seal("platform-webhook", "webhook", "secret", []byte(sealedPlaintext))
			if err != nil {
				return err
			}
			keyRef, sealedKey, sealedValue = version, sk, sv
		}
		// The host token is minted once, on the first save, and never again by a PUT: a URL or
		// secret re-save changes neither the token nor its hash. POST .../token is the only rotate.
		tokenHash := []byte(nil)
		if before != nil {
			tokenHash = before.tokenHash
		} else {
			if mintedToken, err = token(); err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(mintedToken))
			tokenHash = sum[:]
		}
		by := actorOf(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display, set_at)
			VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, now())
			ON CONFLICT (id) DO UPDATE SET url = EXCLUDED.url, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key, sealed_value = EXCLUDED.sealed_value,
				token_hash = EXCLUDED.token_hash, set_by_kind = EXCLUDED.set_by_kind, set_by_subject = EXCLUDED.set_by_subject, set_by_display = EXCLUDED.set_by_display, set_at = EXCLUDED.set_at`,
			body.URL, keyRef, sealedKey, sealedValue, tokenHash, by.Kind, by.Subject, by.Display); err != nil {
			return err
		}
		if saved, err = readWebhook(ctx, tx); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, by, "set platform webhook", "", "", map[string]any{"url": body.URL, "secret_replaced": secretReplaced})
	})
	if err != nil {
		a.log.Printf("canyon-server: write the platform webhook: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out := map[string]any{"url": saved.URL, "set_by": saved.SetBy, "set_at": saved.SetAt}
	if sealedPlaintext != "" && !secretGiven { // answered once: only when Canyon minted it on the first save with none in the body
		out["secret"] = sealedPlaintext
	}
	if mintedToken != "" { // answered once: only on the first save; a later PUT never repeats or rotates it
		out["token"] = mintedToken
	}
	noStore(w)
	writeJSON(w, http.StatusOK, out)
}

func (a *app) rotatePlatformWebhookToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	newToken, err := token()
	if err != nil {
		a.log.Printf("canyon-server: mint a host token: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	hash := sha256.Sum256([]byte(newToken))
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE platform_webhook SET token_hash = $1 WHERE id = 1", hash[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoWebhook
		}
		return events.AuditTx(ctx, tx, actorOf(ctx), "rotate platform token", "", "", nil)
	})
	switch {
	case errors.Is(err, errNoWebhook):
		writeErr(w, http.StatusNotFound, "not_found", errNoWebhook.Error(), nil)
	case err != nil:
		a.log.Printf("canyon-server: rotate the platform webhook token: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		noStore(w)
		writeJSON(w, http.StatusOK, map[string]any{"token": newToken})
	}
}

// rotatePlatformWebhookSecret replaces the webhook secret with one Canyon makes, sealed in place of the stored one and
// answered once (#2439). The url stays, so a stale page can never put back a url another admin changed; set_by and set_at
// move, as for any save of the webhook, so the last delivery never shows an answer to a release signed with the old secret.
// Every delivery after it is signed with it, a release being retried included (deliverOnce opens the secret each attempt).
func (a *app) rotatePlatformWebhookSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	secret, err := token()
	if err != nil {
		a.log.Printf("canyon-server: mint a webhook secret: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		version, sealedKey, sealedValue, err := a.vault.Seal("platform-webhook", "webhook", "secret", []byte(secret))
		if err != nil {
			return err
		}
		by := actorOf(ctx)
		tag, err := tx.Exec(ctx, `UPDATE platform_webhook SET key_ref = $1, sealed_key = $2, sealed_value = $3,
			set_by_kind = $4, set_by_subject = $5, set_by_display = $6, set_at = now() WHERE id = 1`, version, sealedKey, sealedValue, by.Kind, by.Subject, by.Display)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoWebhook
		}
		return events.AuditTx(ctx, tx, by, "rotate platform secret", "", "", nil)
	})
	switch {
	case errors.Is(err, errNoWebhook):
		writeErr(w, http.StatusNotFound, "not_found", errNoWebhook.Error(), nil)
	case err != nil:
		a.log.Printf("canyon-server: rotate the platform webhook secret: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		noStore(w)
		writeJSON(w, http.StatusOK, map[string]any{"secret": secret})
	}
}

// pingPlatformWebhook sends a test delivery to the saved url (contracts/routes.md, POST …/ping; #2439): outside any
// transaction, one at a time per server, answering and logging the status alone — never why, which would name the
// network behind the url — and writing nothing: no release, no audit row, never the webhook's last delivery.
func (a *app) pingPlatformWebhook(w http.ResponseWriter, r *http.Request) {
	select {
	case a.pings <- struct{}{}:
		defer func() { <-a.pings }()
	default:
		writeErr(w, http.StatusTooManyRequests, "ping_in_flight", pingInFlight, nil)
		return
	}
	sent, err := a.pipe.Ping(r.Context())
	switch {
	case errors.Is(err, runs.ErrNoWebhook):
		writeErr(w, http.StatusNotFound, "not_found", errNoWebhook.Error(), nil)
	case err != nil:
		a.log.Printf("canyon-server: a test delivery to the platform webhook, not sent: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		a.log.Printf("canyon-server: a test delivery to the platform webhook answered %d", sent.Status)
		writeJSON(w, http.StatusOK, sent)
	}
}
