package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
)

// routesTLS registers GET /tls/ask (contracts/routes.md, Open): whether the TLS proxy in front of Canyon
// may get a certificate for a name, Caddy's on-demand ask (docs: self-host/docker.md). Without it, anyone
// who points made-up app names at the proxy spends the domain's certificate limit.
func routesTLS(mux *http.ServeMux, a *app) {
	mux.HandleFunc("GET /tls/ask", a.tlsAsk)
}

// tlsAsk answers 200 for Canyon's own host or an existing app's address, 404 for any other name, and
// 403 to anyone but the proxy.
func (a *app) tlsAsk(w http.ResponseWriter, r *http.Request) {
	if !a.askedByProxy(r) {
		writeErr(w, http.StatusForbidden, "not_the_proxy", "only the proxy in front of Canyon asks this", nil)
		return
	}
	known, err := a.certifiable(r.Context(), r.URL.Query().Get("domain"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// askedByProxy is a request that forwards nobody's — the proxy's own ask carries no X-Forwarded-For or
// Forwarded, and every request it forwards from outside carries one — from loopback, a trusted proxy,
// or carrying CANYON_TLS_ASK_TOKEN in its query, compared in constant time: the token is for a proxy
// with no address to trust (the Helm chart's Caddy, #2672). Unset, no token is accepted.
func (a *app) askedByProxy(r *http.Request) bool {
	if len(r.Header.Values("X-Forwarded-For")) > 0 || len(r.Header.Values("Forwarded")) > 0 {
		return false
	}
	if token := r.URL.Query().Get("token"); a.askToken != "" && token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(a.askToken)) == 1 {
		return true
	}
	peer, err := netip.ParseAddr(peerAddr(r))
	if err != nil {
		return false
	}
	return peer.Unmap().IsLoopback() || a.proxies.trusts(peer)
}

// certifiable is Canyon's own host, the two addresses the Kubernetes front-door check dials
// (canyon-probe.<env>.<host>, contracts/executor.md, certificate), or <app>.<env>.<host> for an app that
// exists and is not deleted.
// A name is ASCII or none Canyon serves: lower-casing folds the Kelvin sign (U+212A) to k, so a name
// with any byte past ASCII is refused before it is lower-cased. The public host is lower-cased too,
// however CANYON_PUBLIC_URL spells it.
func (a *app) certifiable(ctx context.Context, domain string) (bool, error) {
	for i := range len(domain) {
		if domain[i] >= 0x80 {
			return false, nil
		}
	}
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	host := publicHost(a.public)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if domain == host || domain == "canyon-probe.preview."+host || domain == "canyon-probe.prod."+host {
		return true, nil
	}
	app, env, ok := appSurface(domain)
	if !ok || domain != app+"."+env+"."+host {
		return false, nil
	}
	var exists bool
	err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)", app).Scan(&exists)
	return exists, err
}

// askTokenRule is a CANYON_TLS_ASK_TOKEN: at least 32 characters that ride in a URL's query as they are,
// since Caddy's ask URL carries the token unencoded.
var askTokenRule = regexp.MustCompile(`^[A-Za-z0-9._~-]{32,512}$`)

// askTokenOf is CANYON_TLS_ASK_TOKEN, "" when unset; a value too short, or with a character a query would
// change, is refused, so the server never starts on a token a guess or an encoding could meet.
func askTokenOf(v string) (string, error) {
	if v == "" || askTokenRule.MatchString(v) {
		return v, nil
	}
	return "", errors.New("CANYON_TLS_ASK_TOKEN: set it to 32 to 512 characters of A-Z, a-z, 0-9, '.', '_', '~' and '-', as the Helm chart generates")
}
