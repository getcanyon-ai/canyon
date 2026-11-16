package main

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// grantRows inserts project_access rows for claims-portal: grantee, subject, subject_issuer, level and roles each.
func grantRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rows ...[5]string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-portal') ON CONFLICT DO NOTHING"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		roles := []string{}
		if r[4] != "" {
			roles = []string{r[4]}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, subject, subject_issuer, level, roles, granted_by_kind, granted_by_subject, granted_by_display)
			VALUES ('claims-portal', 'email', $1, $2, $3, $4, $5, 'person', 'sub-dana', '')`, r[0], r[1], r[2], r[3], roles); err != nil {
			t.Fatal(err)
		}
	}
}

// levelAt is the caller's level on claims-portal as the API reads it.
func levelAt(t *testing.T, c *http.Client, base string) any {
	t.Helper()
	_, read, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
	project, _ := read["project"].(map[string]any)
	access, _ := project["access"].(map[string]any)
	return access["level"]
}

// A Canyon session keeps the issuer it was minted through: once the operator moves CANYON_OIDC_ISSUER, a session
// from the old one is refused, as an expired one is, and writes nothing — it never reads its subject under the
// new issuer, where it may be another person's (#2306). One recording no issuer is the current issuer's.
func TestASessionFromAnotherIssuerIsRefused(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	old := signedIn(t, ctx, pool, base, "sub-ola", "ola@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET issuer = 'https://login.contoso.example' WHERE actor_subject = 'sub-ola'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, old, http.MethodGet, base+"/api/v1/fleet", ""); code != 401 {
		t.Fatalf("a session from another issuer reads the fleet: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, old, http.MethodPost, base+"/api/v1/projects", `{"id":"made-elsewhere"}`); code != 401 || count(t, ctx, pool, "SELECT count(*) FROM projects WHERE id = 'made-elsewhere'") != 0 {
		t.Fatalf("a session from another issuer creates an app: %d %s", code, lastBody)
	}
	current := signedIn(t, ctx, pool, base, "sub-pia", "pia@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET issuer = $1 WHERE actor_subject = 'sub-pia'", goodOIDC["CANYON_OIDC_ISSUER"]); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, current, http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("a session from the current issuer: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-quinn", "quinn@meridian.example", false), http.MethodGet, base+"/api/v1/fleet", ""); code != 200 {
		t.Fatalf("a session recording no issuer: %d %s", code, lastBody)
	}
}

// An issuer is recorded on a subject with none only where it is certain: none on the anonymous admin's at the
// start; the configured issuer on every other only once a sign-in has verified an ID token under it — never with
// sign-in off, which would unbind every grant once sign-in is on again, and never on discovery alone, which a
// wrong but real issuer (another tenant, Entra's v1 for v2) passes, so fixing the variable could not undo it (#2306).
func TestTheIssuerIsStampedOnlyWhereItIsCertain(t *testing.T) {
	idp := newTestIDP(t)
	idp.key(t, "a", true)
	cfg, err := oidcConfigOf(envOf(map[string]string{"CANYON_OIDC_ISSUER": idp.URL, "CANYON_OIDC_CLIENT_ID": "canyon", "CANYON_OIDC_ALLOWED_DOMAINS": "meridian.example", "CANYON_ADMINS": "dana@meridian.example"}))
	if err != nil {
		t.Fatal(err)
	}
	_, p := idp.discovered(t)
	testAuth, testOIDC, testProvider = authOIDC, cfg, p
	t.Cleanup(func() { testAuth, testOIDC, testProvider = authOff, nil, nil })
	pool, ctx := migrated(t)
	grantRows(t, ctx, pool, [5]string{"ola@meridian.example", "sub-ola", "", "contributor", ""}, [5]string{"anonymous", "anonymous", "", "owner", ""})
	issuerOfRow := func(grantee string) string {
		var issuer string
		if err := pool.QueryRow(ctx, "SELECT subject_issuer FROM project_access WHERE grantee = $1", grantee).Scan(&issuer); err != nil {
			t.Fatal(err)
		}
		return issuer
	}
	if err := stampAnonymous(ctx, pool); err != nil { // the start, before the listener
		t.Fatal(err)
	}
	if got, anon := issuerOfRow("ola@meridian.example"), issuerOfRow("anonymous"); got != "" || anon != noIssuer {
		t.Errorf("at the start: ola's grant under %q, the anonymous admin's under %q", got, anon)
	}
	base, _ := start(t, pool, time.Hour)
	if got := issuerOfRow("ola@meridian.example"); got != "" { // the issuer discovered, nobody signed in
		t.Errorf("discovery alone recorded %q", got)
	}
	signInAs(t, idp, base, map[string]any{"email": "dana@meridian.example", "email_verified": true, "sub": "sub-dana"})
	if got := issuerOfRow("ola@meridian.example"); got != idp.URL {
		t.Errorf("after a verified sign-in, ola's grant under %q", got)
	}
}

// Sign-in off on an upgraded host, then on: a grant bound before issuers were recorded is still its person's,
// by subject, and nobody else's at its address (#2306).
func TestAGrantStaysBoundThroughABootWithSignInOff(t *testing.T) {
	pool, ctx := migrated(t)
	grantRows(t, ctx, pool, [5]string{"ola@meridian.example", "sub-ola", "", "contributor", ""})
	if err := stampAnonymous(ctx, pool); err != nil { // the boot with sign-in off
		t.Fatal(err)
	}
	withOIDC(t)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET onboarded_at = now()"); err != nil {
		t.Fatal(err)
	}
	base, _ := start(t, pool, time.Hour)
	if got := levelAt(t, signedIn(t, ctx, pool, base, "sub-ola", "ola.new@meridian.example", false), base); got != "contributor" {
		t.Fatalf("its person under a new address: %v", got)
	}
	if got := levelAt(t, signedIn(t, ctx, pool, base, "sub-other", "ola@meridian.example", false), base); got != "none" {
		t.Fatalf("another person at its address: %v", got)
	}
}

// With no issuer to read subjects under, no bound grant matches at all — not by its subject, and not by its
// address as though unbound; a grant not yet bound still matches by address (#2306).
func TestAnEmptyIssuerMatchesNoBoundGrant(t *testing.T) {
	pool, ctx := migrated(t)
	grantRows(t, ctx, pool, [5]string{"anna@meridian.example", "sub-anna", "https://login.example.com/v2.0", "contributor", ""},
		[5]string{"carl@meridian.example", "", "", "viewer", ""})
	for _, c := range []struct{ who, sub, email, want string }{
		{"the bound subject", "sub-anna", "anna.new@meridian.example", "none"},
		{"the bound grant's address", "sub-bob", "anna@meridian.example", "none"},
		{"the unbound grant's address", "sub-carl", "carl@meridian.example", "viewer"},
	} {
		caller := withActor(ctx, signedSession{act: actor{Kind: "person", Subject: c.sub}, role: "member", email: c.email})
		if got, err := levelOf(caller, pool, "claims-portal"); err != nil || got != c.want {
			t.Errorf("%s with no issuer: %q (%v), want %s", c.who, got, err, c.want)
		}
	}
}

// The roles a grant carries and the apps a caller is on the team of follow the grant's issuer as its level does:
// one bound under another issuer gives its old subject nothing; one bound with no issuer recorded is the current
// issuer's (#2306).
func TestAGrantsRolesAndTeamFollowItsIssuer(t *testing.T) {
	pool, ctx := migrated(t)
	issuer := "https://login.example.com/v2.0"
	grantRows(t, ctx, pool, [5]string{"ivo@meridian.example", "sub-ivo", "https://login.contoso.example", "contributor", "approver"},
		[5]string{"lea@meridian.example", "sub-lea", "", "contributor", "auditor"})
	for _, c := range []struct {
		sub, email string
		roles      []string
		team       []string
	}{
		{"sub-ivo", "ivo.new@meridian.example", []string{}, nil},
		{"sub-lea", "lea.new@meridian.example", []string{"auditor"}, []string{"claims-portal"}},
	} {
		caller := withActor(ctx, signedSession{act: actor{Kind: "person", Subject: c.sub}, role: "member", email: c.email, issuer: issuer})
		roles, err := rolesOf(caller, pool, "claims-portal")
		if err != nil || !slices.Equal(roles, c.roles) {
			t.Errorf("%s's roles: %v (%v), want %v", c.sub, roles, err, c.roles)
		}
		team, err := teamApps(caller, pool)
		if err != nil || !slices.Equal(team, c.team) {
			t.Errorf("%s's team apps: %v (%v), want %v", c.sub, team, err, c.team)
		}
	}
}

// A share binds the grant under the issuer its request was recorded under, so a request from another issuer
// grants its old subject nothing; an unverified person at a foreign request's address replaces it, and the same
// person asking again verified records it under the current issuer (#2306).
func TestAShareCarriesTheRequestsIssuer(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	issuer := goodOIDC["CANYON_OIDC_ISSUER"]
	vera := signedIn(t, ctx, pool, base, "sub-vera", "vera@meridian.example", false)
	if code, _, _ := do(t, vera, http.MethodPost, base+"/api/v1/projects", `{"id":"claims-portal"}`); code != 201 {
		t.Fatalf("vera creates the app: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_requests (project_id, email, subject, subject_issuer) VALUES
		('claims-portal', 'zed@meridian.example', 'sub-zed', 'https://login.contoso.example'), ('claims-portal', 'yan@meridian.example', 'sub-yan', $1),
		('claims-portal', 'wes@meridian.example', 'sub-wes', 'https://login.contoso.example'), ('claims-portal', 'vic@meridian.example', 'sub-vic', 'https://login.contoso.example')`, issuer); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"zed", "yan"} {
		if code, _, _ := do(t, vera, http.MethodPut, base+"/api/v1/projects/claims-portal/access/email/"+who+"@meridian.example", `{"level":"viewer"}`); code != 200 {
			t.Fatalf("vera shares with %s: %d %s", who, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM project_access WHERE (grantee, subject, subject_issuer) IN
		(('zed@meridian.example', 'sub-zed', 'https://login.contoso.example'), ('yan@meridian.example', 'sub-yan', '`+issuer+`'))`); n != 2 {
		t.Errorf("the shares' grants carry their requests' issuers: %d of 2", n)
	}
	if got := levelAt(t, signedIn(t, ctx, pool, base, "sub-zed", "zed.new@meridian.example", false), base); got != "none" {
		t.Errorf("a foreign request's subject reads the app at %v", got)
	}
	if got := levelAt(t, signedIn(t, ctx, pool, base, "sub-yan", "yan.new@meridian.example", false), base); got != "viewer" {
		t.Errorf("a current request's subject reads the app at %v", got)
	}
	wes := signedIn(t, ctx, pool, base, "sub-wes2", "wes@meridian.example", false)
	if _, err := pool.Exec(ctx, "UPDATE sessions SET email_verified = false WHERE actor_subject = 'sub-wes2'"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, wes, http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 201 ||
		count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'wes@meridian.example' AND subject = '' AND subject_issuer = ''") != 1 {
		t.Errorf("an unverified person over a foreign request: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false), http.MethodPost, base+"/api/v1/projects/claims-portal/access-requests", ""); code != 201 ||
		count(t, ctx, pool, "SELECT count(*) FROM access_requests WHERE email = 'vic@meridian.example' AND subject = 'sub-vic' AND subject_issuer = '"+issuer+"'") != 1 {
		t.Errorf("the same person asking again, verified, under the current issuer: %d %s", code, lastBody)
	}
}

// With sign-in off the access file carries no subject at all, whatever a grant was bound under (#2306).
func TestTheAccessFileWithSignInOffCarriesNoSubject(t *testing.T) {
	pool, ctx := migrated(t)
	grantRows(t, ctx, pool, [5]string{"anna@meridian.example", "sub-anna", "https://login.example.com/v2.0", "owner", ""},
		[5]string{"lea@meridian.example", "sub-lea", "", "viewer", ""}, [5]string{"anonymous", "anonymous", noIssuer, "owner", ""})
	raw, err := (&app{pool: pool}).accessFile(ctx, org{domains: []string{"meridian.example"}, emails: []string{}, admins: []string{}}, "claims-portal", "2026-10-17T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	file := fileOf(t, raw)
	for _, e := range file["entries"].([]any) {
		if s := e.(map[string]any)["subject"]; s != "" {
			t.Errorf("%v carries subject %v with sign-in off", e.(map[string]any)["grantee"], s)
		}
	}
	if file["subject_issuer"] != "" {
		t.Errorf("subject_issuer with sign-in off: %v", file["subject_issuer"])
	}
}
