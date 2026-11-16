package main

import (
	"io/fs"
	"log"
	"net/http"

	githubapp "github.com/getcanyon-ai/canyon/github-app"
)

// routesOpen registers the Open group of contracts/routes.md: outside session, audit and rate
// limit, behind recovery and logging. Everything unrouted answers the contract's error shape.
func routesOpen(mux *http.ServeMux, ready *readiness, logger *log.Logger, bundle fs.FS, a *app) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if waiting := ready.waitingOn(); len(waiting) > 0 {
			writeErr(w, http.StatusServiceUnavailable, "not_ready", "not ready", map[string]any{"waiting_on": waiting})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	// /auth/config (routes.md, Open): what the CLI reads to learn whether this host authenticates.
	// The contract's union is oidc | off, so the compose demo's demo mode answers off. cli is
	// the version stamped into this binary — one release stamps bin/canyon with the same string —
	// and public is the configured URL, never the request's Host: it names the session cookie. setup
	// is the setup state, which only oidc has: a CLI whose code expired says so only when it is true.
	mux.HandleFunc("GET /auth/config", func(w http.ResponseWriter, r *http.Request) {
		mode := "off"
		if a.auth == authOIDC {
			mode = "oidc"
		}
		o := a.orgNow(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"canyon": true, "auth": mode, "org": o.name, "device": a.auth == authOIDC, "cli": version, "public": a.public, "setup": a.auth == authOIDC && o.setup})
	})
	routesAuth(mux, a)
	mux.Handle("POST /webhooks/github", &githubapp.Courier{Pool: a.pool, Vault: a.vault, Durable: a.durable, Log: logger}) // authenticated by its signature
	routeDashboard(mux, logger, bundle)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such route", nil)
	})
}

// routeDashboard serves the embedded bundle: GET /{$} — the exact root, never the mux's
// catch-all — is index.html, GET /assets/{path} its content-hashed files. Client routing is
// hash-based, so there is no fallback and every other path keeps the contract's 404. With no
// bundle embedded nothing is registered at all: the root answers 404 like any unrouted path,
// rather than a blank page that reads as a broken dashboard.
func routeDashboard(mux *http.ServeMux, logger *log.Logger, bundle fs.FS) {
	index, err := fs.ReadFile(bundle, "dist/index.html")
	if err != nil {
		logger.Printf("canyon-server: no dashboard bundle embedded, / answers 404 (build one with `make web`)")
		return
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // the one unhashed file: revalidated every load
		// No page frames the dashboard: a preview, under its registrable domain, could otherwise put the
		// device or approval page under a decoy for a signed-in person to click (contracts/routes.md).
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write(index)
	})
	mux.HandleFunc("GET /assets/{path}", func(w http.ResponseWriter, r *http.Request) {
		name := "dist/assets/" + r.PathValue("path")
		if _, err := fs.Stat(bundle, name); err != nil { // fs rejects an invalid path, so a traversal lands here too
			writeErr(w, http.StatusNotFound, "not_found", "no such route", nil) // a miss under a routed prefix is the contract's 404 too
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // the name carries the hash
		http.ServeFileFS(w, r, bundle, name)                                   //nolint:gosec // {path} is one segment and the Stat above passed only a real file in dist/assets
	})
}
