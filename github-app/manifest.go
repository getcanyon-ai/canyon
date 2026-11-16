package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stateBytes is the registration's state: 32 random bytes, hex in the cookie and the query.
const stateBytes = 32

// Registration serves the routes that register Canyon's GitHub App on an organization from a manifest
// and list the orgs (contracts/routes.md); the server wraps each in its admin check, so a member is
// refused first.
type Registration struct {
	Pool   *pgxpool.Pool
	Vault  *vault.Vault
	API    string // CANYON_GITHUB_API: where the manifest's code is converted
	Public string // the host's public URL, scheme and host: every URL in the manifest, never the request's Host
	Cookie string // the state cookie's name, from the session cookie's builder, with its Secure
	Secure bool
	Actor  func(context.Context) events.Actor // the admin, for the audit row
	Log    *log.Logger
}

// form submits itself; html/template escapes the org, the state and the manifest into it.
var form = template.Must(template.New("form").Parse(`<!doctype html>
<title>Registering Canyon's GitHub App</title>
<form id="f" method="post" action="https://github.com/organizations/{{.Org}}/settings/apps/new?state={{.State}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Continue to GitHub</button></noscript>
</form>
<script>document.getElementById("f").submit()</script>
`))

// login is a GitHub login's shape: letters, digits and single dashes inside.
var login = regexp.MustCompile(`^[A-Za-z0-9](-?[A-Za-z0-9])*$`)

// validLogin is a GitHub organization's login: login's shape, at most 39 characters.
func validLogin(s string) bool { return len(s) <= 39 && login.MatchString(s) }

// New serves the form for an org, with the admin's two answers; keeps_code decides whether the App asks
// to write. One App per org, and one org keeps code.
func (g *Registration) New(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	org := q.Get("org")
	imports, keeps := q.Get("imports_new"), q.Get("keeps_code")
	switch {
	case !validLogin(org):
		writeErr(w, http.StatusBadRequest, "invalid_org", "a GitHub organization is its login: letters, digits and dashes, at most 39")
		return
	case (imports != "true" && imports != "false") || (keeps != "true" && keeps != "false"):
		writeErr(w, http.StatusBadRequest, "bad_request", "imports_new and keeps_code are each true or false")
		return
	}
	if taken, err := g.taken(ctx, g.Pool, org, keeps == "true"); err != nil {
		g.internal(w, err)
		return
	} else if taken != nil {
		taken(w)
		return
	}
	raw := make([]byte, stateBytes)
	_, _ = rand.Read(raw) // never fails: the runtime crashes rather than answer short
	state := hex.EncodeToString(raw)
	// security_events for code scanning's analyses; the org Canyon keeps code in commits there and makes each app's repository
	permissions := map[string]string{"checks": "write", "contents": "read", "metadata": "read", "security_events": "read"}
	if keeps == "true" {
		permissions["contents"], permissions["administration"] = "write", "write"
	}
	m, _ := json.Marshal(map[string]any{ // strings only, so it cannot fail; the permissions and events the courier needs
		"name": appName(org), "url": g.Public, "redirect_url": g.Public + "/api/v1/github/app/callback",
		"hook_attributes": map[string]string{"url": g.Public + "/webhooks/github"}, "public": false,
		"callback_urls":       []string{g.Public + LinkCallback}, // where a person linking their GitHub account returns (#2228)
		"default_permissions": permissions,
		"default_events":      []string{"pull_request", "push", "repository"}, // repository keeps links (#1917); installation events reach every App unsubscribed
	})
	g.setCookie(w, state+"."+imports+"."+org, 3600) // GitHub's code lives an hour; the org and its answer ride with the state
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := form.Execute(w, map[string]string{"Org": org, "State": state, "Manifest": string(m)}); err != nil {
		g.Log.Printf("canyon-server: github app form: %v", err)
	}
}

// org is one row of GET /api/v1/github.
type org struct {
	Account       string     `json:"account"`
	AppSlug       string     `json:"app_slug"`
	KeepsCode     bool       `json:"keeps_code"`
	ImportsNew    bool       `json:"imports_new"`
	Installed     bool       `json:"installed"`
	LastEventAt   *time.Time `json:"last_event_at"`
	LinksAccounts bool       `json:"links_accounts"` // Canyon holds the App's client secret, which linking a GitHub account needs (#2228)
}

// Orgs is GET /api/v1/github: the orgs Canyon's Apps are on, oldest registration first, and whether
// GitHub can reach this host to register another.
func (g *Registration) Orgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := g.orgs(r.Context())
	if err != nil {
		g.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"reachable": Reachable(g.Public), "orgs": orgs})
}

func (g *Registration) orgs(ctx context.Context) ([]org, error) {
	rows, err := g.Pool.Query(ctx, `SELECT a.account, a.slug, a.keeps_code, a.imports_new,
		EXISTS (SELECT 1 FROM github_installations i WHERE i.app_id = a.app_id AND i.deleted_at IS NULL),
		(SELECT max(received_at) FROM github_deliveries d WHERE d.app_id = a.app_id), a.sealed_client_secret IS NOT NULL
		FROM github_app a ORDER BY a.created_at, a.app_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (org, error) {
		var o org
		err := row.Scan(&o.Account, &o.AppSlug, &o.KeepsCode, &o.ImportsNew, &o.Installed, &o.LastEventAt, &o.LinksAccounts)
		if o.LastEventAt != nil {
			*o.LastEventAt = o.LastEventAt.UTC() // every time the API answers is UTC
		}
		return o, err
	})
}

// SetOrg is PUT /api/v1/github/orgs/:account (#1923): whether the organization's new repositories become
// apps. A change alone is audited; the answer is the org as Orgs lists it.
func (g *Registration) SetOrg(w http.ResponseWriter, r *http.Request) {
	ctx, account := r.Context(), r.PathValue("account")
	var body struct {
		ImportsNew *bool `json:"imports_new"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	if dec.Decode(&body) != nil || dec.More() || body.ImportsNew == nil { // one object and nothing after it
		writeErr(w, http.StatusBadRequest, "bad_request", "imports_new is true or false")
		return
	}
	tx, err := g.Pool.Begin(ctx)
	if err != nil {
		g.internal(w, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		g.internal(w, err)
		return
	}
	var changed bool
	err = tx.QueryRow(ctx, `WITH one AS (SELECT app_id, imports_new, account FROM github_app WHERE account <> '' AND lower(account) = lower($1)),
		set AS (UPDATE github_app a SET imports_new = $2 FROM one WHERE a.app_id = one.app_id AND one.imports_new <> $2 RETURNING a.app_id)
		SELECT EXISTS (SELECT 1 FROM set), one.account FROM one`, account, *body.ImportsNew).Scan(&changed, &account) // the org as stored, for the audit row and the answer
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Canyon's App isn't registered on "+account)
		return
	}
	if err == nil && changed {
		err = events.AuditTx(ctx, tx, g.Actor(ctx), "set github org", "", "", map[string]any{"account": account, "imports_new": *body.ImportsNew})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	g.answerOrg(w, ctx, account)
}

// SetClientSecret is PUT /api/v1/github/orgs/:account/client-secret (#2228): the client secret of an App
// registered before Canyon kept one, which its owner generates on GitHub. It is sealed under the active key
// version, and the App's other two seals are re-wrapped to it in the same statement, so the row keeps one
// key_ref; the row is locked from its read to that write, so a rotation waits on it.
func (g *Registration) SetClientSecret(w http.ResponseWriter, r *http.Request) {
	ctx, account := r.Context(), r.PathValue("account")
	var body struct {
		ClientSecret string `json:"client_secret"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	if dec.Decode(&body) != nil || dec.More() || !printable(body.ClientSecret) {
		writeErr(w, http.StatusBadRequest, "invalid_secret", "a client secret is 1 to 255 printable ASCII characters with no spaces, as GitHub shows it")
		return
	}
	tx, err := g.Pool.Begin(ctx)
	if err != nil {
		g.internal(w, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		g.internal(w, err)
		return
	}
	var app int64
	var keyRef int
	var pemKey, hookKey []byte
	err = tx.QueryRow(ctx, `SELECT app_id, account, key_ref, sealed_pem_key, sealed_hook_key FROM github_app WHERE account <> '' AND lower(account) = lower($1) FOR UPDATE`, account).
		Scan(&app, &account, &keyRef, &pemKey, &hookKey)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Canyon's App isn't registered on "+account)
		return
	}
	if err == nil {
		err = g.storeClientSecret(ctx, tx, app, keyRef, pemKey, hookKey, body.ClientSecret)
	}
	if err == nil {
		err = events.AuditTx(ctx, tx, g.Actor(ctx), "set github client secret", "", "", map[string]any{"account": account})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		g.internal(w, err)
		return
	}
	g.answerOrg(w, ctx, account)
}

// storeClientSecret seals secret for app's row under the active key version, re-wrapping the key's and the
// webhook secret's data keys from keyRef to it, all in one write.
func (g *Registration) storeClientSecret(ctx context.Context, tx pgx.Tx, app int64, keyRef int, pemKey, hookKey []byte, secret string) error {
	version, clientKey, sealed, err := g.Vault.Seal("github", "app", "client_secret", []byte(secret))
	if err != nil {
		return err
	}
	_, newPemKey, err := g.Vault.Rewrap(keyRef, "github", "app", "private_key", pemKey)
	if err != nil {
		return err
	}
	_, newHookKey, err := g.Vault.Rewrap(keyRef, "github", "app", "webhook_secret", hookKey)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE github_app SET key_ref = $2, sealed_pem_key = $3, sealed_hook_key = $4, sealed_client_key = $5, sealed_client_secret = $6 WHERE app_id = $1`,
		app, version, newPemKey, newHookKey, clientKey, sealed)
	return err
}

// printable is a client secret's shape: 1 to 255 printable ASCII characters, no spaces.
func printable(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// answerOrg answers the org as Orgs lists it.
func (g *Registration) answerOrg(w http.ResponseWriter, ctx context.Context, account string) {
	orgs, err := g.orgs(ctx)
	if err != nil {
		g.internal(w, err)
		return
	}
	for _, o := range orgs {
		if strings.EqualFold(o.Account, account) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"org": o})
			return
		}
	}
}

// taken answers why an App cannot be registered on org: one is already there, or keeps asks to keep code
// where another org does. nil: neither.
func (g *Registration) taken(ctx context.Context, q querier, org string, keeps bool) (func(http.ResponseWriter), error) {
	var there, keeper bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM github_app WHERE lower(account) = lower($1)), EXISTS (SELECT 1 FROM github_app WHERE keeps_code)", org).Scan(&there, &keeper); err != nil {
		return nil, err
	}
	switch {
	case there:
		return func(w http.ResponseWriter) {
			writeErr(w, http.StatusConflict, "exists", "Canyon's App is already registered on "+org)
		}, nil
	case keeps && keeper:
		return func(w http.ResponseWriter) {
			writeErr(w, http.StatusConflict, "keeping_org_exists", "another organization already keeps Canyon's code")
		}, nil
	}
	return nil, nil
}

// Reachable is whether GitHub can deliver events to the public URL: never to localhost, a name under
// .localhost, or a loopback address.
func Reachable(public string) bool {
	u, err := url.Parse(public)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	return host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback())
}

// Callback checks the cookie's state, converts the code, stores the App sealed and audited under the
// org GitHub says owns it, and sends the admin to install it. Every refusal is one 400; the reason is
// logged, never the code.
func (g *Registration) Callback(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	g.setCookie(w, "", -1) // single use: cleared on every answer
	c, err := r.Cookie(g.Cookie)
	var state, imports, org string
	if err == nil {
		state, imports, _ = strings.Cut(c.Value, ".")
		imports, org, _ = strings.Cut(imports, ".")
	}
	if err != nil || len(state) != 2*stateBytes || (imports != "true" && imports != "false") || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		g.refuse(w, "state is not the one this registration issued")
		return
	}
	conv, err := New(g.API, 0, nil).ConvertManifest(ctx, q.Get("code"))
	if err != nil {
		g.refuse(w, err.Error())
		return
	}
	if _, err := parseKey([]byte(conv.PEM)); err != nil || !validSlug(conv.Slug) || conv.ID <= 0 || conv.WebhookSecret == "" || conv.ClientSecret == "" {
		g.refuse(w, "the conversion answered no usable App")
		return
	}
	if conv.Owner.Type != "Organization" || conv.Owner.ID <= 0 || !validLogin(org) || !strings.EqualFold(conv.Owner.Login, org) { // finished on another account than the org the form named
		g.refuse(w, fmt.Sprintf("app %s was created on %s %q, not on the organization %q; delete it there", conv.Slug, conv.Owner.Type, conv.Owner.Login, org))
		return
	}
	taken, err := g.store(ctx, conv, imports == "true")
	if err != nil {
		g.internal(w, fmt.Errorf("app %s was created on GitHub but not stored; delete it there: %w", conv.Slug, err))
		return
	}
	if taken != nil {
		g.Log.Printf("canyon-server: github app %s was created on GitHub but %s is taken here; delete it there", conv.Slug, conv.Owner.Login)
		taken(w)
		return
	}
	http.Redirect(w, r, "https://github.com/apps/"+conv.Slug+"/installations/new", http.StatusFound)
}

func (g *Registration) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: g.Cookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: g.Secure}) //nolint:gosec // Secure follows the public URL's scheme, as the session cookie's does
}

func (g *Registration) refuse(w http.ResponseWriter, reason string) {
	g.Log.Printf("canyon-server: github app registration refused: %s", reason)
	writeErr(w, http.StatusBadRequest, "auth_failed", "the GitHub App registration could not be finished")
}

func (g *Registration) internal(w http.ResponseWriter, err error) {
	g.Log.Printf("canyon-server: github app registration: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}

// writeErr is the contract's error shape, {error, message}.
func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// appName is "Canyon — <org>", the GitHub org's login, cut on a rune boundary to GitHub's 34-character
// cap on App names. An App's name is unique on GitHub, and two logins alike in their first 25
// characters, or one org registering from two Canyons, ask for one name: GitHub's page then says it is
// taken, and the admin renames the App there; the callback trusts the org GitHub answers, never the name.
func appName(org string) string {
	name := []rune("Canyon — " + org)
	return string(name[:min(len(name), 34)])
}

// validSlug is a slug the installation URL can carry as one path segment.
func validSlug(s string) bool {
	return s != "" && len(s) <= 100 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") == ""
}
