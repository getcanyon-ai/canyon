package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	neturl "net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/jackc/pgx/v5/pgconn"
)

// routesDesignSystems registers the design-system registry (contracts/routes.md, the
// design-systems row): the rows an org can choose from, and the one route that adds one. There is
// no edit and no delete — the two shipped rows are unchangeable because no route changes any row.
func routesDesignSystems(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /design-systems", a.listDesignSystems)
	api.HandleFunc("POST /design-systems", a.adminOnly(a.createDesignSystem))
}

type designSystem struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	SourceType  string  `json:"source_type"`
	RegistryURL string  `json:"registry_url"`
	TokensURL   *string `json:"tokens_url,omitempty"` // the contract types it a string: omitted when unset, never null
	CreatedAt   string  `json:"created_at"`
	CreatedBy   string  `json:"created_by"`
}

const urlRule = "a URL is absolute https — http only on a loopback host — names no private, link-local or loopback address, carries no credential, and is at most 2048 bytes"

// designSystemEntry is one registration as either writer sends it: the form's body with its
// tokens_url already resolved, and the setup tool's creating shape, which decodes into it.
type designSystemEntry struct {
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	RegistryURL string  `json:"registry_url"`
	TokensURL   *string `json:"tokens_url"`
}

// errDesignSystemExists is a namespace the registry already carries: the POST answers 409, and
// the setup tool answers a refusal the model reads.
var errDesignSystemExists = errors.New("a design system with that namespace exists")

// designSystemRule names the first field of a registration Canyon will not store and the rule it
// broke. The form renders it as a 400 and the setup tool hands it to the model, so a URL the
// model invented carrying a credential is refused in the same words a person's is.
func designSystemRule(d designSystemEntry) (string, string) {
	switch {
	case !plainText(d.Name, 64):
		return "invalid_name", "a name is 1 to 64 characters of plain text"
	case !plainText(d.Namespace, 128):
		return "invalid_namespace", "a namespace is the package name an app declares, 1 to 128 characters of plain text"
	case !registryURL(d.RegistryURL):
		return "invalid_registry_url", urlRule
	case d.TokensURL != nil && !registryURL(*d.TokensURL):
		return "invalid_tokens_url", urlRule
	}
	return "", ""
}

func (a *app) listDesignSystems(w http.ResponseWriter, r *http.Request) {
	out, err := designSystemRows(r.Context(), a.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"design_systems": out})
}

func (a *app) createDesignSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string          `json:"name"`
		Namespace   string          `json:"namespace"`
		RegistryURL string          `json:"registry_url"`
		TokensURL   json.RawMessage `json:"tokens_url"` // absent is unset; a null is not a URL
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	entry := designSystemEntry{Name: body.Name, Namespace: body.Namespace, RegistryURL: body.RegistryURL}
	if len(body.TokensURL) > 0 {
		var s string
		if json.Unmarshal(body.TokensURL, &s) != nil {
			writeErr(w, http.StatusBadRequest, "invalid_tokens_url", urlRule, nil)
			return
		}
		entry.TokensURL = &s
	}
	if code, rule := designSystemRule(entry); code != "" {
		writeErr(w, http.StatusBadRequest, code, rule, nil)
		return
	}
	row, err := a.newDesignSystem(r.Context(), actorOf(r.Context()), action(r), entry)
	if errors.Is(err, errDesignSystemExists) {
		writeErr(w, http.StatusConflict, "exists", errDesignSystemExists.Error(), nil)
		return
	}
	if err != nil {
		a.log.Printf("design systems: %v", err) // the 500 names nothing; the log names the cause
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"design_system": row})
}

// newDesignSystem is the one writer of the registry: the insert and its audit row in one
// transaction, answering the row as it now stands. The form and the setup chat's tool take the
// same path, so a row the model registered is stored exactly as a person's is — and source_type
// is the literal in the SQL, so neither of them can register a row as shipped.
func (a *app) newDesignSystem(ctx context.Context, act actor, action string, d designSystemEntry) (designSystem, error) {
	id, err := newDesignSystemID()
	if err != nil {
		return designSystem{}, err
	}
	tx, err := a.mutation(ctx)
	if err != nil {
		return designSystem{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO design_systems (id, name, namespace, source_type, registry_url, tokens_url, created_by)
		VALUES ($1, $2, $3, 'registry', $4, $5, $6)`, id, d.Name, d.Namespace, d.RegistryURL, d.TokensURL, act.Subject); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "design_systems_namespace" { // the other unique key is the generated id
			return designSystem{}, errDesignSystemExists
		}
		return designSystem{}, err
	}
	// After the insert: a refused registration leaves a log line, never an audit row. The project is
	// empty — the registry belongs to the org, so this writes no project's timeline row.
	if err := events.AuditTx(ctx, tx, act, action, "", "", map[string]any{"id": id, "name": d.Name, "namespace": d.Namespace}); err != nil {
		return designSystem{}, err
	}
	rows, err := designSystemRows(ctx, tx, id) // read inside the transaction: nothing is durable until the commit answers
	if err != nil {
		return designSystem{}, err
	}
	if len(rows) != 1 {
		return designSystem{}, errors.New("the registry does not carry the row this transaction just inserted")
	}
	return rows[0], tx.Commit(ctx)
}

// designSystemRows reads the whole registry, or the one row an id names. The order is (created_at,
// id): the shipped rows share a timestamp, so the id settles them, and a client never sees the
// order an insert happened to take.
func designSystemRows(ctx context.Context, q querier, id string) ([]designSystem, error) {
	rows, err := q.Query(ctx, `SELECT id, name, namespace, source_type, registry_url, tokens_url, created_at, created_by
		FROM design_systems WHERE $1 = '' OR id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []designSystem{}
	for rows.Next() {
		var d designSystem
		var at time.Time
		if err := rows.Scan(&d.ID, &d.Name, &d.Namespace, &d.SourceType, &d.RegistryURL, &d.TokensURL, &at, &d.CreatedBy); err != nil {
			return nil, err
		}
		d.CreatedAt = at.UTC().Format(time.RFC3339)
		out = append(out, d)
	}
	return out, rows.Err()
}

// plainText is the contract's rule for a name and a namespace: characters, not bytes, and nothing
// a terminal or a log line would act on.
// plainText is a name a person can read as written: 1 to max code points, at least one of them not
// a space, and none of them a control, a format character (a zero-width or a direction override,
// which renders one way and reads another), or a line or paragraph separator. These names are
// written into every agent's skill file as typed values, so nothing that can hide or reorder text
// is a name.
func plainText(s string, max int) bool {
	if n := utf8.RuneCountInString(s); n == 0 || n > max || strings.TrimSpace(s) == "" {
		return false
	}
	return !strings.ContainsFunc(s, hidesText)
}

// hidesText is a character that can hide or reorder text around it, or start a second line: a control
// or format character (a bidi override, a zero-width space) or a line or paragraph separator.
func hidesText(r rune) bool {
	return r == 0x7F || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// registryURL is the contract's rule for both URLs. u.User == nil is the load-bearing one: Canyon
// never stores a registry credential, and a URL is where one would arrive. An IP literal naming
// the org's own network (private, link-local — the cloud metadata service — loopback over https,
// unspecified, multicast; IPv4-mapped or not) is refused too: the URL is written into every
// builder's agent config and is where the first server-side fetch would go. No name is resolved.
func registryURL(s string) bool {
	if len(s) > 2048 {
		return false
	}
	// The scheme is matched as written: url.Parse lowercases it, and the contract's pattern does not.
	if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
		return false
	}
	u, err := neturl.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	if u.Scheme == "http" {
		return loopbackHost(u.Hostname())
	}
	return u.Scheme == "https" && !privateAddress(u.Hostname())
}

func privateAddress(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast()
}

func newDesignSystemID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ds_" + hex.EncodeToString(b[:]), nil
}
