package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LinkCallback is where GitHub returns a person linking their GitHub account: the App's user authorization
// callback (the manifest's callback_urls) and the redirect_uri the link names.
const LinkCallback = "/api/v1/github/link/callback"

// Linker serves the routes that link a person's own GitHub account to them (contracts/routes.md,
// GET /api/v1/github/link; #2228): signing in to GitHub through Canyon's App proves the account is
// theirs. The server wraps each in its check that a person in a browser asks.
type Linker struct {
	Pool   *pgxpool.Pool
	Vault  *vault.Vault
	API    string // CANYON_GITHUB_API: where GET /user is asked
	Web    string // GitHub's web address: where the person signs in and the code is exchanged
	Public string // the host's public URL, scheme and host: the redirect_uri, never the request's Host
	Cookie string // the state cookie's name, from the session cookie's builder, with its Secure
	Secure bool
	Actor  func(context.Context) events.Actor // the person linking
	Log    *log.Logger
}

// Start sends the person to sign in to GitHub through the oldest App registered with a client secret,
// with a single-use state in a cookie that names that App.
func (l *Linker) Start(w http.ResponseWriter, r *http.Request) {
	var app int64
	var client string
	err := l.Pool.QueryRow(r.Context(), "SELECT app_id, client_id FROM github_app WHERE sealed_client_secret IS NOT NULL ORDER BY created_at, app_id LIMIT 1").Scan(&app, &client)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusConflict, "not_configured", "no GitHub App here can link accounts: none was registered since Canyon could link them")
		return
	}
	if err != nil {
		l.internal(w, err)
		return
	}
	raw := make([]byte, stateBytes)
	_, _ = rand.Read(raw) // never fails: the runtime crashes rather than answer short
	state := hex.EncodeToString(raw)
	l.setCookie(w, state+"."+strconv.FormatInt(app, 10), 600)
	q := url.Values{"client_id": {client}, "redirect_uri": {l.Public + LinkCallback}, "state": {state}}
	http.Redirect(w, r, l.Web+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

// Callback checks the cookie's state, exchanges the code with the App's client secret for a user token,
// asks GitHub whose it is and links that account to the person by its numeric id; the token is dropped.
// A person who cancelled on GitHub goes back to the dashboard with nothing written; every other refusal
// is one 400, its reason logged, never the code.
func (l *Linker) Callback(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	l.setCookie(w, "", -1) // single use: cleared on every answer
	if q.Get("error") == "access_denied" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	c, err := r.Cookie(l.Cookie)
	var state, app string
	if err == nil {
		state, app, _ = strings.Cut(c.Value, ".")
	}
	id, parseErr := strconv.ParseInt(app, 10, 64)
	if err != nil || parseErr != nil || len(state) != 2*stateBytes || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		l.refuse(w, "state is not the one this link issued")
		return
	}
	client, secret, err := l.client(ctx, id)
	if err != nil {
		l.refuse(w, fmt.Sprintf("app %d's client secret: %v", id, err))
		return
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"` // GitHub answers a refused exchange 200, naming the error
	}
	exchange := map[string]string{"client_id": client, "client_secret": secret, "code": q.Get("code"), "redirect_uri": l.Public + LinkCallback}
	if _, err := New(l.Web, 0, nil).do(ctx, http.MethodPost, "/login/oauth/access_token", "", "application/json", exchange, &token); err != nil || token.AccessToken == "" {
		l.refuse(w, fmt.Sprintf("the code exchange failed: %v %s", err, token.Error))
		return
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if _, err := New(l.API, 0, nil).do(ctx, http.MethodGet, "/user", "Bearer "+token.AccessToken, githubJSON, nil, &user); err != nil || user.ID <= 0 || user.Login == "" || len(user.Login) > 39 {
		l.refuse(w, fmt.Sprintf("GET /user answered no account: %v", err))
		return
	}
	if err := l.link(ctx, user.ID, user.Login); err != nil {
		l.internal(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// client is app's client id and its client secret, opened.
func (l *Linker) client(ctx context.Context, app int64) (string, string, error) {
	var client string
	var keyRef int
	var clientKey, sealed []byte
	if err := l.Pool.QueryRow(ctx, "SELECT client_id, key_ref, sealed_client_key, sealed_client_secret FROM github_app WHERE app_id = $1 AND sealed_client_secret IS NOT NULL", app).
		Scan(&client, &keyRef, &clientKey, &sealed); err != nil {
		return "", "", err
	}
	secret, err := l.Vault.Open(keyRef, "github", "app", "client_secret", clientKey, sealed)
	return client, string(secret), err
}

// link writes the person's link to the account, its login as GitHub answers it now, with one audit row
// when the link is new.
func (l *Linker) link(ctx context.Context, github int64, login string) error {
	act := l.Actor(ctx)
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return err
	}
	var added bool
	if err := tx.QueryRow(ctx, `INSERT INTO github_links (github_id, subject, login) VALUES ($1, $2, $3)
		ON CONFLICT (github_id, subject) DO UPDATE SET login = EXCLUDED.login RETURNING xmax = 0`, github, act.Subject, login).Scan(&added); err != nil {
		return err
	}
	if added {
		if err := events.AuditTx(ctx, tx, act, "link github account", "", "", map[string]any{"github_id": github, "login": login}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (l *Linker) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: l.Cookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: l.Secure}) //nolint:gosec // Secure follows the public URL's scheme, as the session cookie's does
}

func (l *Linker) refuse(w http.ResponseWriter, reason string) {
	l.Log.Printf("canyon-server: github link refused: %s", reason)
	writeErr(w, http.StatusBadRequest, "auth_failed", "your GitHub account could not be linked")
}

func (l *Linker) internal(w http.ResponseWriter, err error) {
	l.Log.Printf("canyon-server: github link: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}
