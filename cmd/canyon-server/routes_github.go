package main

import (
	"net/http"
	"strings"

	githubapp "github.com/getcanyon-ai/canyon/github-app"
)

// githubStateCookie holds a registration's state until GitHub returns, and githubLinkCookie a link's. Named by
// the session cookie's builder, as the sign-in cookie is; Lax, so it rides the top-level navigation back from GitHub.
const (
	githubStateCookie = "canyon_github_state"
	githubLinkCookie  = "canyon_github_link"
)

// routesGitHub registers the GitHub Apps' registration and the orgs they are on (contracts/routes.md,
// Platform): every route admin-only, the callback in its own right, so a member is refused before the
// code is exchanged. Linking a GitHub account is any person's own, in a browser (#2228).
func routesGitHub(api *http.ServeMux, a *app) {
	name, secure := sessionCookieOf(a.public)
	g := &githubapp.Registration{Pool: a.pool, Vault: a.vault, API: a.github, Public: a.public,
		Cookie: strings.Replace(name, sessionCookie, githubStateCookie, 1), Secure: secure, Actor: actorOf, Log: a.log}
	api.HandleFunc("GET /github", a.adminOnly(g.Orgs))
	api.HandleFunc("PUT /github/orgs/{account}", a.adminOnly(g.SetOrg))
	api.HandleFunc("PUT /github/orgs/{account}/client-secret", a.adminOnly(g.SetClientSecret))
	api.HandleFunc("GET /github/app/new", a.adminOnly(g.New))
	api.HandleFunc("GET /github/app/callback", a.adminOnly(g.Callback))
	l := &githubapp.Linker{Pool: a.pool, Vault: a.vault, API: a.github, Web: a.ghWeb, Public: a.public,
		Cookie: strings.Replace(name, sessionCookie, githubLinkCookie, 1), Secure: secure, Actor: actorOf, Log: a.log}
	api.HandleFunc("GET /github/link", a.personInBrowser(l.Start))
	api.HandleFunc("GET /github/link/callback", a.personInBrowser(l.Callback))
}

// personInBrowser admits a person in a browser session alone: with sign-in off no caller is a person, and
// the route is not found.
func (a *app) personInBrowser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case a.auth != authOIDC:
			writeErr(w, http.StatusNotFound, "not_found", "not found", nil)
		case !fromDashboard(r.Context()):
			writeErr(w, http.StatusForbidden, "dashboard_required", "link your GitHub account in Canyon's dashboard", nil)
		default:
			next(w, r)
		}
	}
}
