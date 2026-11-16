package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// The credential the server reads a private Opengrep rules repo with (contracts/routes.md, the scanner
// credential row): sealed, never answered, and removed when the repo it was set for is no longer the one
// sarif.opengrep's settings name.
func routesScanners(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /scanners/opengrep/credential", a.getScannerCredential)
	api.HandleFunc("PUT /scanners/opengrep/credential", a.adminOnly(a.putScannerCredential))
	api.HandleFunc("DELETE /scanners/opengrep/credential", a.adminOnly(a.deleteScannerCredential))
	api.HandleFunc("GET /scanners/data", a.adminOnly(a.scannerData))
}

// scannerData is how old the host's vulnerability databases are (contracts/routes.md, the scanner data row; #2743):
// an admin's read, the executor's GET /scandb as it answered, none from an executor before it or a host with none.
func (a *app) scannerData(w http.ResponseWriter, r *http.Request) {
	dbs := []executor.ScanDB{}
	if a.pipe != nil && a.pipe.Executor != nil {
		got, err := a.pipe.Executor.ScanDBs(r.Context())
		if err != nil {
			a.log.Printf("canyon-server: read the scanner databases' age: %v", err)
			writeErr(w, http.StatusBadGateway, "scanner_data_unavailable", "Canyon couldn't read how old its scanners' data is", nil)
			return
		}
		dbs = append(dbs, got...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": dbs})
}

// printable is 1 to 256 printable ASCII characters: a credential's username and token.
var printable = regexp.MustCompile(`^[\x20-\x7e]{1,256}$`)

func (a *app) getScannerCredential(w http.ResponseWriter, r *http.Request) {
	var repo string
	var at time.Time
	var by actor
	err := a.pool.QueryRow(r.Context(), "SELECT repo, set_at, set_by_kind, set_by_subject, set_by_display FROM scanner_credentials WHERE tool = 'opengrep'").
		Scan(&repo, &at, &by.Kind, &by.Subject, &by.Display)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(w, http.StatusOK, map[string]any{"credential": nil})
	case err != nil:
		a.log.Printf("canyon-server: read the scanner credential: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default: // never the username or the token
		writeJSON(w, http.StatusOK, map[string]any{"credential": map[string]any{"repo": repo, "set_at": at.UTC().Format(time.RFC3339), "set_by": by}})
	}
}

func (a *app) putScannerCredential(w http.ResponseWriter, r *http.Request) {
	var body struct{ Repo, Username, Token string }
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF || len(body.Repo) > 2048 || !repoRe.MatchString(body.Repo) || !printable.MatchString(body.Username) || !printable.MatchString(body.Token) { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "invalid_credential", "the body is {repo, username, token}: an https:// URL, and 1 to 256 printable ASCII characters each", nil)
		return
	}
	ctx, by := r.Context(), actorOf(r.Context())
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		version, sealedKey, sealedValue, err := a.vault.Seal("scanner", "opengrep", "token", []byte(body.Token))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO scanner_credentials (tool, repo, username, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
			VALUES ('opengrep', $1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tool) DO UPDATE SET repo = EXCLUDED.repo, username = EXCLUDED.username, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key,
				sealed_value = EXCLUDED.sealed_value, set_at = now(), set_by_kind = EXCLUDED.set_by_kind, set_by_subject = EXCLUDED.set_by_subject, set_by_display = EXCLUDED.set_by_display`,
			body.Repo, body.Username, version, sealedKey, sealedValue, by.Kind, by.Subject, by.Display); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, by, "scanner.credential.set", "", "", map[string]any{"tool": "opengrep", "repo": body.Repo, "username": body.Username})
	})
	a.answerCredential(w, err)
}

func (a *app) deleteScannerCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a.answerCredential(w, a.inMutation(ctx, func(tx pgx.Tx) error { return dropCredential(ctx, tx, actorOf(ctx), nil) }))
}

func (a *app) answerCredential(w http.ResponseWriter, err error) {
	if err != nil {
		a.log.Printf("canyon-server: the scanner credential: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// inMutation runs fn in a mutation and commits it.
func (a *app) inMutation(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := a.mutation(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// dropCredential removes the credential, with its audit row, unless rules names the repo it was set for:
// a DELETE passes none, and a policies apply the settings it leaves (contracts/routes.md).
func dropCredential(ctx context.Context, tx pgx.Tx, by actor, rules *gatepolicy.OpengrepRules) error {
	var repo string
	err := tx.QueryRow(ctx, "SELECT repo FROM scanner_credentials WHERE tool = 'opengrep'").Scan(&repo)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && rules != nil && runs.SameRepo(repo, rules.Repo) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM scanner_credentials WHERE tool = 'opengrep'"); err != nil {
		return err
	}
	return events.AuditTx(ctx, tx, by, "scanner.credential.remove", "", "", map[string]any{"tool": "opengrep", "repo": repo})
}
