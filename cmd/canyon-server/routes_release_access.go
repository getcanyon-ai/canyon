package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// routesReleaseAccess registers the Open group's two access routes (contracts/routes.md,
// GET /access/:app/:env and GET /releases/:id/access-token; design/HAND-OFF.md §5): the sidecar in
// the org's pod pulls its app's access file under its release's access token, and the org's
// pipeline fetches that token under the host token. Neither is bounded and neither writes an audit
// row, as the contract's rows say.
func routesReleaseAccess(mux *http.ServeMux, a *app) {
	mux.HandleFunc("GET /access/{app}/{env}", a.pullAccess)
	mux.HandleFunc("GET /releases/{id}/access-token", a.releaseAccessToken)
}

// pullAccess answers the app's access file to a live access token of that app and environment: one
// minted for a release of it and not yet cleared, which a newer release of it reporting deployed does
// (recordDeployed). Any other token — none, a dead one, another app's or environment's — and an
// unknown app are the same 401, so nothing enumerates apps. A release Canyon stopped is answered the deny-all
// file, never a 401, which the sidecar would ride out for its grace (#2450). The file is accessFile's, the producer PUT /access carries,
// stamped now rather than through the files lock: that lock issues one stamp a second, a bound across
// the fleet, and every handed-off sidecar pulls every minute (routes.md: never bounded).
func (a *app) pullAccess(w http.ResponseWriter, r *http.Request) {
	ctx, id, env := r.Context(), r.PathValue("app"), r.PathValue("env")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(token))
	// One lookup of the token's release t answers both whether the token is live and whether t is stopped
	// (contracts/routes.md, the access row): its environment stopped — production held, its project deleting or
	// deleted — or t itself, read per token so a newer release never hands t's pods the list back: a remove of its
	// environment recorded at or after t (t itself, when t is a remove), whenever Canyon decided, whatever the
	// platform did since; or a deploy Canyon ran there since t, a switch back to run_by: canyon, which t records as
	// canyon_since (0111), so Canyon undeploying its own deploy never reopens it (#2469). The removes are read on
	// releases_env_newest (0106). A stopped release's pods admit nobody, though the platform has not taken them down.
	var live, stopped bool
	if err := a.pool.QueryRow(ctx, `WITH t AS (SELECT created_at, canyon_since FROM releases WHERE access_token_hash = $3 AND project_id = $1 AND env = $2)
		SELECT EXISTS (SELECT 1 FROM t),
			EXISTS (SELECT 1 FROM projects p WHERE p.id = $1 AND (p.deleting_since IS NOT NULL OR p.deleted_at IS NOT NULL))
			OR $2 = 'prod' AND EXISTS (SELECT 1 FROM prod_holds h WHERE h.project_id = $1)
			OR EXISTS (SELECT 1 FROM t WHERE EXISTS (SELECT 1 FROM releases n WHERE n.project_id = $1 AND n.env = $2 AND n.event = 'remove' AND n.created_at >= t.created_at)
				OR t.canyon_since IS NOT NULL)`,
		id, env, sum[:]).Scan(&live, &stopped); err != nil {
		a.log.Printf("canyon-server: access pull for %s/%s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !ok || token == "" || !live {
		writeErr(w, http.StatusUnauthorized, "bad_token", "the bearer token is missing, unknown, dead, or not this app and environment's", nil)
		return
	}
	at := time.Now().UTC().Format(time.RFC3339)
	file, err := denyAll(at)
	if !stopped {
		var o org
		if o, err = a.orgOf(ctx, a.pool); err == nil {
			file, err = a.accessFile(ctx, o, id, at)
		}
	}
	if err != nil {
		a.log.Printf("canyon-server: access pull for %s/%s: %v", id, env, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	// A stopped pull checks in too: a pod still pulling is one the platform has not taken down, which the surfaces
	// say in amber. Anything that settles a deploy by its pods' check-in must skip a stopped pull: those pods are
	// ones Canyon has asked the platform to take down, never the deploy's.
	release, settles := a.checkIn(ctx, id, env, sum[:])
	writeJSON(w, http.StatusOK, file)
	// Under delivery: repo a check-in from a pod whose app answers is what settles the release (#2470): after the
	// answer, never delaying or changing it, and never on a stopped pull.
	if settles && !stopped && r.Header.Get("Canyon-App") == "answered" {
		go a.deployByCheckIn(context.WithoutCancel(ctx), release) // the request's values, never its end: it runs after the answer
	}
}

// checkInWithin bounds the check-in's one statement; a var so a test can lower it.
var checkInWithin = time.Second

// checkIn records that a pod of the release the token was minted for pulled its access list (contracts/routes.md,
// the access row: a pod's check-in; #2452), once the 200 is certain and apart from the token check: at most once
// per 30 seconds per release, a release seen in the last 30 seconds or locked by another pull skipped, never
// waited on, and the token checked again in the update's own WHERE, so one that died since writes nothing. Best
// effort: a write that fails is logged, and the pull answers the file all the same — a check-in never gates sign-in.
// It answers the release it wrote, and whether that one is a delivery: repo deploy still waiting to go live,
// which this check-in may settle (#2470).
func (a *app) checkIn(ctx context.Context, id, env string, hash []byte) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, checkInWithin)
	defer cancel()
	var release string
	var settles bool
	err := a.pool.QueryRow(ctx, `UPDATE releases SET seen_at = now() WHERE access_token_hash = $3 AND id = (
		SELECT r.id FROM releases r WHERE r.access_token_hash = $3 AND r.project_id = $1 AND r.env = $2
			AND (r.seen_at IS NULL OR r.seen_at < now() - interval '30 seconds')
			AND EXISTS (SELECT 1 FROM projects p WHERE p.id = r.project_id AND p.deleted_at IS NULL)
		FOR UPDATE SKIP LOCKED) RETURNING id, via = 'repo' AND event = 'deploy' AND state = 'accepted'`, id, env, hash).Scan(&release, &settles)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.log.Printf("canyon-server: check-in of %s/%s: %v", id, env, err)
	}
	return release, settles
}

// errNoRelease is releaseAccessToken's unknown or superseded release: its 404.
var errNoRelease = errors.New("no such release, or a newer one of its environment")

// releaseAccessToken answers the release's access token to the host token, the same value on every
// fetch while the release is its environment's newest: minted and sealed on the first, opened on each
// after, under the row's lock so two first fetches mint one token. The value goes into the response
// alone: never a log line, an error or an audit row.
func (a *app) releaseAccessToken(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	valid, err := hostTokenValid(ctx, a.pool, token) // a host token, or the fetch-only token External Secrets holds (#2470): this route alone takes it
	if err == nil && !valid {
		valid, err = fetchTokenValid(ctx, a.pool, token)
	}
	if err != nil {
		a.log.Printf("canyon-server: access token of release %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !ok || token == "" || !valid {
		writeErr(w, http.StatusUnauthorized, "bad_token", "the bearer token is missing, unknown or a replaced one", nil)
		return
	}
	var value []byte
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) (err error) {
		value, err = a.accessToken(ctx, tx, id)
		return err
	})
	switch {
	case errors.Is(err, errNoRelease):
		writeErr(w, http.StatusNotFound, "not_found", "no such release, or a newer one of its environment exists", nil)
	case err != nil:
		a.log.Printf("canyon-server: access token of release %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"name": "CANYON_ACCESS_TOKEN", "value": string(value)})
	}
}

// accessToken is release id's token: opened when one is sealed on the row, else minted, sealed as the
// row (project, "access-token", id) — never (project, env, name), a secret's, whose name could be the
// release id — and stored beside its SHA-256, which the access pull compares.
func (a *app) accessToken(ctx context.Context, tx pgx.Tx, id string) ([]byte, error) {
	rel := release{ID: id}
	var keyRef *int
	var sealedKey, sealedValue []byte
	err := tx.QueryRow(ctx, "SELECT project_id, env, key_ref, sealed_key, sealed_value FROM releases WHERE id = $1 FOR UPDATE", id).
		Scan(&rel.ProjectID, &rel.Env, &keyRef, &sealedKey, &sealedValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoRelease
	}
	if err != nil {
		return nil, err
	}
	if newer, err := newerRelease(ctx, tx, rel); err != nil {
		return nil, err
	} else if newer != "" {
		return nil, errNoRelease
	}
	if keyRef != nil {
		return a.vault.Open(*keyRef, rel.ProjectID, "access-token", id, sealedKey, sealedValue)
	}
	minted, err := token()
	if err != nil {
		return nil, err
	}
	version, sealedKey, sealedValue, err := a.vault.Seal(rel.ProjectID, "access-token", id, []byte(minted))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(minted))
	if _, err := tx.Exec(ctx, "UPDATE releases SET key_ref = $2, sealed_key = $3, sealed_value = $4, access_token_hash = $5 WHERE id = $1", id, version, sealedKey, sealedValue, sum[:]); err != nil {
		return nil, err
	}
	return []byte(minted), nil
}
