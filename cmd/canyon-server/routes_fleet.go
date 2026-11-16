package main

import (
	"cmp"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// routesFleet registers the fleet read (contracts/routes.md row 75): tiles over the caller's team apps (every app for an admin), one
// sorted page of app rows, the cursor after it, and the honesty banner. A read, so no audit row.
func routesFleet(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /fleet", a.fleet)
}

var (
	fleetTiles   = []string{"secrets_exposed", "blocked", "approved", "never_reviewed", "unowned", "failed", "refused"}
	fleetStages  = []string{"created", "awaiting_review", "reviewing", "blocked", "approved", "passed", "building", "preview", "production", "failed"}
	fleetRunning = []string{"production", "preview", "nothing"}
	fleetAccess  = []string{"private", "shared", "everyone"}
)

func (a *app) fleet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, p := range []struct {
		name   string
		accept []string
	}{{"tile", fleetTiles}, {"stage", fleetStages}, {"running", fleetRunning}, {"access", fleetAccess}, {"scope", []string{"team"}}, {"sort", []string{"risk", "changed", "name"}}} {
		if v := q.Get(p.name); v != "" && !slices.Contains(p.accept, v) {
			writeErr(w, http.StatusBadRequest, "invalid_"+p.name, p.name+" is one of: "+strings.Join(p.accept, ", "), nil)
			return
		}
	}
	for _, name := range []string{"stack", "q"} { // owner and needs_you refuse nothing: no match is an empty page, anything but true is off
		if utf8.RuneCountInString(q.Get(name)) > 64 {
			writeErr(w, http.StatusBadRequest, "invalid_"+name, name+" is at most 64 characters", nil)
			return
		}
	}
	limit, ok := pageLimit(w, q)
	if !ok {
		return
	}
	asked, ok := pageNumber(w, q)
	if !ok {
		return
	}
	after, ok := parseFleetCursor(q.Get("cursor"), cmp.Or(q.Get("sort"), "risk"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_cursor", "cursor is what the previous page's next carried", nil)
		return
	}
	role := roleOf(r.Context())
	held := "" // the Held filter (contracts/routes.md, The hold): only the exact true turns it on, over the rows whose hold the caller reads
	if q.Get("held") == "true" {
		held = "team"
		if role == "responder" || role == "admin" {
			held = "all"
		}
	}
	// What needs a person, of every app the caller reads (needs.go; #2724): a fixed set of statements and one executor call,
	// whatever the fleet's size. The rows' needs, needs_you, the refused tile and the risk sort all read this one list.
	facts, err := needFactsOf(r.Context(), a.pool, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	states := a.statesOf(r.Context(), facts)
	needs, err := a.needsFrom(r.Context(), a.pool, facts, states)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	org, err := a.orgNeeds(r.Context(), a.pool) // the org's own needs, an admin's alone: none for anyone else
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if org == nil {
		org = []need{}
	}
	needy, refusing, top, carried, unchecked := []string{}, []string{}, map[string]int{}, len(org), 0
	for _, f := range facts {
		for env, by := range f.runs { // what the read could not tell runs: never all clear (contracts/routes.md, the fleet read's unchecked)
			if f.team && by == "canyon" && states[f.id][env] == "unknown" {
				unchecked++
			}
		}
		list := needs[f.id]
		if len(list) == 0 {
			continue
		}
		needy, carried = append(needy, f.id), carried+len(list)
		if slices.ContainsFunc(list, func(n need) bool { return n.kind == "refused" }) {
			refusing = append(refusing, f.id)
		}
		if f.team {
			top[f.id] = list[0].rank
		}
	}
	var needsYou []string // needs_you: null off, else the apps whose rows carry a need
	if q.Get("needs_you") == "true" {
		needsYou = needy
	}
	filters := []any{q.Get("tile"), q.Get("owner"), q.Get("stage"), q.Get("running"), q.Get("stack"), q.Get("q"), needsYou, q.Get("access"), q.Get("scope") == "team", held, refusing}
	var total, offset int
	if err := a.pool.QueryRow(r.Context(), fleetCountSQL, slices.Concat(callerArgs(r.Context()), filters)...).Scan(&total); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	var page any // null on a read by cursor
	if q.Get("cursor") == "" {
		n := servedPage(asked, total, limit)
		page, offset = n, (n-1)*limit
	}
	rows, next, err := fleetPage(r.Context(), a.pool, filters, top, after, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	for _, f := range rows { // each environment's state, from the one executor call statesOf made
		for env, e := range f.Envs {
			if m, ok := e.(map[string]any); ok && m["run_by"] == "canyon" && (f.team || f.Access.Level == "viewer" || env == "prod" && role == "responder") {
				m["state"] = cmp.Or(states[f.ID][env], "unknown") // one the read did not ask of is one it could not check
			}
		}
	}
	team := map[string]map[string]any{} // the patch state is technical detail: rows read whole alone
	for _, f := range rows {
		if f.team {
			team[f.ID] = f.Envs
		}
	}
	if _, err := a.sayPatch(r.Context(), a.pool, team); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	apps := make([]any, len(rows))
	for i, f := range rows {
		apps[i] = f.read(role == "responder", needs[f.ID])
	}
	var t [14]int // the seven tiles, then the seven stats, in the order the statement counts them
	if err := a.pool.QueryRow(r.Context(), fleetTilesSQL, append(callerArgs(r.Context()), refusing)...).Scan(&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9], &t[10], &t[11], &t[12], &t[13]); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	tiles := map[string]int{"secrets_exposed": t[0], "blocked": t[1], "approved": t[2], "never_reviewed": t[3], "unowned": t[4], "failed": t[5], "refused": t[6], "needs_you": carried}
	stats := map[string]int{"apps": t[7], "production": t[8], "preview": t[9], "passed": t[10], "owners": t[11], "deploys_7d": t[12], "production_7d": t[13]}
	act := actorOf(r.Context())
	// the GitHub accounts the reader linked, and whether this host can link one: never with sign-in off (#2228)
	var logins []string
	var linkable bool
	if err := a.pool.QueryRow(r.Context(), `SELECT ARRAY(SELECT login FROM github_links WHERE subject = $1 ORDER BY login),
		$2 AND EXISTS (SELECT 1 FROM github_app WHERE sealed_client_secret IS NOT NULL)`, act.Subject, a.auth == authOIDC).Scan(&logins, &linkable); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tiles": tiles, "stats": stats, "apps": apps, "total": total, "page": page, "next": next, "banner": a.banner(r.RemoteAddr), "viewer": map[string]any{"display": act.Display, "role": role, "github_logins": logins, "github_link": linkable}, "unchecked": unchecked, "org_needs": org})
}

// none under oidc: the two sentences say the host has no auth, and the rail shows the viewer instead.
// Behind the compose demo's port mapping the container's listener and peer say nothing about who
// reaches the host's port, so a loopback published address is the banner's alone (routes.md); one beyond
// loopback fails a demo's boot (publishedGateway), and the container's own pair answers without one.
func (a *app) banner(peer string) string {
	switch {
	case a.auth == authOIDC:
		return "none"
	case a.auth == authDemo && a.publish != "" && loopback(a.publish):
		return "loopback"
	}
	return banner(a.listen, peer)
}

// publishedAddrOf reads CANYON_PUBLISHED_ADDR: host:port with a numeric port, as CANYON_ADDR is; a
// value that does not parse fails the boot (routes.md, the fleet read).
func publishedAddrOf(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if _, port, err := net.SplitHostPort(v); err != nil || !numericPort(port) {
		return "", fmt.Errorf("CANYON_PUBLISHED_ADDR=%s is not host:port", v)
	}
	return v, nil
}

func numericPort(port string) bool {
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

// banner is derived, never asserted: loopback when both the listener and the peer are on a loopback
// address, exposed otherwise. The two sentences it selects are D5's and stay in the client. The peer is r.RemoteAddr
// on purpose: a reverse proxy on the host reads loopback, X-Forwarded-For would be forgeable, and a remote bind is gated at boot.
func banner(listener, peer string) string {
	if loopback(listener) && loopback(peer) {
		return "loopback"
	}
	return "exposed"
}

func loopback(addr string) bool { // an address: a host that carries a port
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return loopbackHost(host)
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
