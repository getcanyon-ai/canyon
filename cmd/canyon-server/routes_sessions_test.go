package main

import (
	"context"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionID is the cookie a signedIn client carries: the row's id.
func sessionID(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	u, _ := neturl.Parse(base)
	if cookies := c.Jar.Cookies(u); len(cookies) == 1 {
		return cookies[0].Value
	}
	t.Fatal("no session cookie")
	return ""
}

func revoked(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT actor_subject FROM sessions WHERE revoked_at IS NOT NULL ORDER BY actor_subject")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestSignOutRevokesTheCallersSessionClearsTheCookieAndWritesOneRow(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := signedIn(t, ctx, pool, base, "u-dana", "dana@meridian.example", false)
	id := sessionID(t, c, base)
	code, _, resp := do(t, c, http.MethodDelete, base+"/api/v1/sessions", "")
	if code != 204 || len(resp.Cookies()) != 1 || resp.Cookies()[0].Name != sessionCookie || resp.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("sign out: %d %s, cookies %v", code, lastBody, resp.Cookies())
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE id = '"+id+"' AND revoked_at IS NOT NULL"); n != 1 {
		t.Fatal("the caller's row is revoked, not deleted")
	}
	var action, who string
	if err := pool.QueryRow(ctx, "SELECT action, actor_subject FROM audit_events").Scan(&action, &who); err != nil || action != "sign out" || who != "u-dana" {
		t.Fatalf("one audit row naming the person: %s %s %v", action, who, err)
	}
	again := signedIn(t, ctx, pool, base, "u-other", "other@meridian.example", false)
	u, _ := neturl.Parse(base)
	again.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: id}}) //nolint:gosec // test: the revoked cookie, presented again
	if code, _, _ := do(t, again, http.MethodGet, base+"/api/v1/fleet", ""); code != 401 {
		t.Fatalf("the revoked cookie is no session: %d", code)
	}
}

func TestRevokeBySubjectMatchesSubjectOrEmailIsAdminOnlyAndIsAlways204(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "u-dana", "dana@meridian.example", false) // admin by CANYON_ADMINS
	bob := signedIn(t, ctx, pool, base, "u-bob", "bob@meridian.example", false)    // a member
	signedIn(t, ctx, pool, base, "u-priya", "priya@meridian.example", false)
	signedIn(t, ctx, pool, base, "cli-priya", "Priya@Meridian.example", false) // the same person, a stored address in another case
	signedIn(t, ctx, pool, base, "u-sam", "sam@meridian.example", false)
	if code, body, _ := do(t, bob, http.MethodDelete, base+"/api/v1/sessions?subject=priya@meridian.example", ""); code != 403 || lastBody != `{"error":"admin_required","message":"only an admin can do this: ask dana@meridian.example"}` {
		t.Fatalf("a member: %d %v", code, body)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action <> 'denied'") + len(revoked(t, ctx, pool)); n != 0 || count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied'") != 1 {
		t.Fatalf("a refused revoke left %d row(s) beside its one denied row", n)
	}
	if code, _, resp := do(t, dana, http.MethodDelete, base+"/api/v1/sessions?subject=priya@meridian.example", ""); code != 204 || len(resp.Cookies()) != 0 { // the admin's own cookie is not touched
		t.Fatalf("by email: %d %s", code, lastBody)
	}
	if got := revoked(t, ctx, pool); len(got) != 2 || got[0] != "cli-priya" || got[1] != "u-priya" {
		t.Fatalf("an email revokes every session stored under it, in any case: revoked %v", got)
	}
	if code, _, _ := do(t, dana, http.MethodDelete, base+"/api/v1/sessions?subject=u-sam", ""); code != 204 {
		t.Fatalf("by subject: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodDelete, base+"/api/v1/sessions?subject=nobody@meridian.example", ""); code != 204 {
		t.Fatalf("no match: %d %s", code, lastBody)
	}
	if got := revoked(t, ctx, pool); len(got) != 3 || got[2] != "u-sam" {
		t.Fatalf("revoked %v", got)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'revoke sessions' AND actor_subject = 'u-dana' AND detail->>'subject' IN ('priya@meridian.example', 'u-sam', 'nobody@meridian.example')"); n != 3 {
		t.Fatalf("one row per act, the no-match one included: %d", n)
	}
	if code, _, _ := do(t, dana, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("the admin's own session is untouched: %d", code)
	}
}

func TestFleetViewerIsTheSessionsPersonAndRoleAndBannerIsNoneUnderOIDC(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for _, c := range []struct{ subject, email, role string }{{"u-dana", "dana@meridian.example", "admin"}, {"u-bob", "bob@meridian.example", "member"}, {"u-kim", "kim@meridian.example", "admin"}} {
		cl := signedIn(t, ctx, pool, base, c.subject, c.email, c.subject == "u-kim") // kim: the admin claim, not the list
		code, page, _ := do(t, cl, http.MethodGet, base+"/api/v1/fleet", "")
		viewer, _ := page["viewer"].(map[string]any)
		if code != 200 || page["banner"] != "none" || viewer["display"] != c.subject || viewer["role"] != c.role {
			t.Fatalf("%s: %d banner=%v viewer=%v, want none and %s", c.subject, code, page["banner"], viewer, c.role)
		}
	}
}

func TestFleetViewerOnADemoHostIsTheAnonymousAdminBehindTheBanner(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	_, page, _ := do(t, client(t), http.MethodGet, base+"/api/v1/fleet", "")
	viewer, _ := page["viewer"].(map[string]any)
	if page["banner"] != "loopback" || viewer["display"] != "" || viewer["role"] != "admin" {
		t.Fatalf("banner=%v viewer=%v", page["banner"], viewer)
	}
}

// On a demo host the very first request mints its own session on the way in and carries no
// cookie yet: signing out then has nothing to revoke and answers the same 204, never a 500.
func TestADemoHostsFirstRequestCanSignOut(t *testing.T) {
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	if code, _, resp := do(t, client(t), http.MethodDelete, base+"/api/v1/sessions", ""); code != 204 || len(resp.Cookies()) == 0 {
		t.Fatalf("a first request signing out: %d %s, cookies %v", code, lastBody, resp.Cookies())
	}
}
