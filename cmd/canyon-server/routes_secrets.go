package main

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/vault"
)

// contracts/routes.md, the secrets rows: 65,536 bytes per value, 1 MiB per (project, env) together.
const (
	maxSecretValue = 64 << 10
	maxSecretsEnv  = 1 << 20
)

var validSecretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`) // what the executor's env accepts (contracts/schema/executor.schema.json)

func routesSecrets(api *http.ServeMux, a *app) {
	api.HandleFunc("PUT /projects/{id}/secrets/{name}", a.teamOnly(a.putSecret))
	api.HandleFunc("GET /projects/{id}/secrets", a.teamOnly(a.listSecrets))
	api.HandleFunc("DELETE /projects/{id}/secrets/{name}", a.teamOnly(a.deleteSecret))
}

// secretOut is the list's element (contracts/routes.md row 81): the name and who set it when,
// never the value — no route reads one back.
type secretOut struct {
	Name    string `json:"name"`
	Env     string `json:"env"`
	Version int    `json:"version"`
	SetBy   actor  `json:"set_by"`
	SetAt   string `json:"set_at"`
	Origin  string `json:"origin"` // user, or provisioned: Canyon sealed a declared resource's output
	Where   string `json:"where"`
}

func (a *app) listSecrets(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if !a.projectLive(ctx, w, id) {
		return
	}
	rows, err := a.pool.Query(ctx, "SELECT name, env, version, set_by_kind, set_by_subject, set_by_display, set_at, scope, coalesce(origin, 'user') FROM secrets WHERE project_id = $1 ORDER BY env, name", id) // preview before prod, names in order: the CLI prints this list as it comes
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer rows.Close()
	out := []secretOut{}
	for rows.Next() {
		var s secretOut
		var at time.Time
		if err := rows.Scan(&s.Name, &s.Env, &s.Version, &s.SetBy.Kind, &s.SetBy.Subject, &s.SetBy.Display, &at, &s.Where, &s.Origin); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		s.SetAt = at.UTC().Format(time.RFC3339)
		out = append(out, s)
	}
	if rows.Err() != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

func (a *app) deleteSecret(w http.ResponseWriter, r *http.Request) {
	env := r.URL.Query().Get("env")
	switch env {
	case "":
		env = "preview"
	case "preview", "prod":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return
	}
	ctx, id, name, act := r.Context(), r.PathValue("id"), r.PathValue("name"), actorOf(r.Context())
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "DELETE FROM secrets WHERE project_id = $1 AND env = $2 AND name = $3 AND origin IS DISTINCT FROM 'provisioned' AND EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", id, env, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	var provisioned bool // a row the DELETE skipped is Canyon's
	if tag.RowsAffected() == 0 {
		err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM secrets s JOIN projects p ON p.id = s.project_id WHERE s.project_id = $1 AND s.env = $2 AND s.name = $3 AND p.deleted_at IS NULL)", id, env, name).Scan(&provisioned)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if provisioned {
		writeErr(w, http.StatusConflict, "provisioned", "Canyon provides this value for a declared resource; it cannot be set or removed by hand", nil)
		return
	}
	if tag.RowsAffected() == 0 { // before the audit row: a refusal is a log line, and the deferred rollback would only hide, not excuse, a row written here
		writeErr(w, http.StatusNotFound, "not_found", "no such secret", nil)
		return
	}
	if err := events.AuditTx(ctx, tx, act, action(r), id, "", map[string]any{"name": name, "env": env}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runtimeOwned are the names the image's runtime sets for itself; a secret under one would replace it.
var runtimeOwned = []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH"}

// runtimeNameOK is whether a secret may take name on any host: never PORT or CANYON_IDENTITY_*, which the executor
// sets, and never a name the runtime owns, set, the app cannot find or load its runtime (#1256).
func runtimeNameOK(name string) bool {
	return validSecretName.MatchString(name) && name != "PORT" && !strings.HasPrefix(name, "CANYON_IDENTITY_") &&
		!slices.Contains(runtimeOwned, name) && !strings.HasPrefix(name, "CNB_")
}

// dockerReserved is the Docker executor's reserved list (executor/executor.go, reserved), the names it refuses in a
// call's env (contracts/executor.md): child, what docker and pack take from the executor's own environment, refused
// in every call as written; the others pack would read on a build, refused in a build's values in any case. The
// executor keeps it unexported, so it is said twice: TestSecretNamesRefusedAreTheDockerExecutors reads the
// executor's source and holds the two equal, these prefixes among them.
var dockerReserved = []dockerName{
	{"PATH", true}, {"HOME", true}, {"TMPDIR", true}, {"DOCKER_HOST", true}, {"DOCKER_CONFIG", true}, {"DOCKER_CERT_PATH", true},
	{"DOCKER_TLS_VERIFY", true}, {"PACK_HOME", true}, {"XDG_CONFIG_HOME", true}, {"XDG_CACHE_HOME", true}, {"PACK_VOLUME_KEY", true},
	{"HTTP_PROXY", false}, {"HTTPS_PROXY", false}, {"ALL_PROXY", false}, {"NO_PROXY", false}, {"SSL_CERT_FILE", false},
	{"SSL_CERT_DIR", false}, {"GODEBUG", false}, {"REGISTRY_AUTH_FILE", false}, {"XDG_RUNTIME_DIR", false},
}

type dockerName struct {
	name  string
	child bool // docker and pack take it from the executor's environment: refused as written, in every call
}

// dockerPrefixes are the prefixes the Docker executor refuses in any case: DOCKER_ in every call, which docker's client
// reads, and PACK_ in a build's, which pack reads.
var dockerPrefixes = []struct {
	prefix string
	build  bool
}{{"DOCKER_", false}, {"PACK_", true}}

// dockerRefuses is whether the Docker executor refuses name in an app's values: a build's when build, else a /run's
// (#2490). A secret under it would fail the app's next deploy, promotion, rollback or platform patch on a Docker host.
func dockerRefuses(name string, build bool) bool {
	for _, p := range dockerPrefixes {
		if (build || !p.build) && len(name) >= len(p.prefix) && strings.EqualFold(name[:len(p.prefix)], p.prefix) {
			return true
		}
	}
	return slices.ContainsFunc(dockerReserved, func(r dockerName) bool {
		return r.child && r.name == name || !r.child && build && strings.EqualFold(r.name, name)
	})
}

func (a *app) putSecret(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value *string `json:"value"`
		Env   string  `json:"env"`
		Where string  `json:"where"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	if !runtimeNameOK(name) {
		writeErr(w, http.StatusBadRequest, "invalid_name", "a secret name is letters, digits and underscores, not starting with a digit, at most 128 characters, never PORT, PATH, LD_PRELOAD or LD_LIBRARY_PATH, and never starting CANYON_IDENTITY_ or CNB_", nil)
		return
	}
	if body.Value == nil || len(*body.Value) > maxSecretValue || strings.IndexByte(*body.Value, 0) >= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_value", "a value is a string of at most 65,536 bytes without NUL; lines are fine", nil)
		return
	}
	env := body.Env
	switch env {
	case "":
		env = "preview"
	case "preview", "prod":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_env", "env is preview or prod", nil)
		return
	}
	scope := body.Where
	switch scope {
	case "":
		scope = "runtime"
	case "runtime", "build":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_where", "where is runtime or build", nil)
		return
	}
	if env == "prod" && scope == "build" { // a build runs once, in preview, and a promotion ships its image: nothing would read it (#765)
		writeErr(w, http.StatusBadRequest, "invalid_where", "Production runs the image preview built: set build values on preview", nil)
		return
	}
	if !a.kube && dockerRefuses(name, scope == "build") { // the executor would refuse the app's next call naming it (contracts/routes.md; #2490)
		next := "deploy"
		if scope == "build" {
			next = "build"
		}
		writeErr(w, http.StatusBadRequest, "invalid_name", name+" is reserved here: this Canyon runs apps with Docker, and docker or pack reads it, so the app's next "+next+" would fail. Choose another name", nil)
		return
	}
	ctx, id, act := r.Context(), r.PathValue("id"), actorOf(r.Context())
	tx, err := a.mutation(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// the app's own trust, which would end a route's: checked and written under the app env's advisory lock, as a
	// route's write takes it, so the two never both land
	trust := scope == "runtime" && slices.Contains(trustVariables, name)
	if trust {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", egressLock(id, env)); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", id).Scan(&exists); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, "not_found", notOnCanyon(id), nil)
		return
	}
	var stored, rows int64 // the other rows of this environment, as plaintext bytes: each sealed value is its value plus the overhead. Check-then-write is safe because a.mutation holds the ledger's one lock for the whole handler.
	if err := tx.QueryRow(ctx, "SELECT coalesce(sum(octet_length(sealed_value)), 0), count(*) FROM secrets WHERE project_id = $1 AND env = $2 AND name <> $3", id, env, name).Scan(&stored, &rows); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if stored-rows*vault.Overhead+int64(len(*body.Value)) > maxSecretsEnv {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "the values of one project environment fit in 1 MiB together", nil)
		return
	}
	keyRef, sealedKey, sealedValue, err := a.vault.Seal(id, env, name, []byte(*body.Value)) // key_ref: the key version the row opens under
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	tag, err := tx.Exec(ctx, `INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (project_id, env, name) DO UPDATE SET scope = EXCLUDED.scope, version = secrets.version + 1, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key, sealed_value = EXCLUDED.sealed_value,
			set_by_kind = EXCLUDED.set_by_kind, set_by_subject = EXCLUDED.set_by_subject, set_by_display = EXCLUDED.set_by_display, set_at = now()
		WHERE secrets.origin IS DISTINCT FROM 'provisioned'`,
		id, env, name, scope, keyRef, sealedKey, sealedValue, act.Kind, act.Subject, act.Display)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if tag.RowsAffected() == 0 { // the row under this name is Canyon's
		writeErr(w, http.StatusConflict, "provisioned", "Canyon provides this value for a declared resource; it cannot be set or removed by hand", nil)
		return
	}
	if trust {
		var routed bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM egress_routes WHERE project_id = $1 AND env = $2)", id, env).Scan(&routed); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
			return
		}
		if routed {
			writeErr(w, http.StatusConflict, "route_trust", "remove the app's credentials in "+envWord(env)+" first: Canyon needs to set "+name+" itself for them", nil)
			return
		}
	}
	if err := events.AuditTx(ctx, tx, act, action(r), id, "", map[string]any{"name": name, "env": env, "where": scope}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
