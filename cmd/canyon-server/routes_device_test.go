package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	devicePath = "/auth/device"
	pollPath   = "/auth/device/token"
	grantsPath = "/api/v1/device-grants/"
)

// mint is one CLI's POST /auth/device: the device code, the shown user code, and the grant's stored hash.
func mint(t *testing.T, base string) (device, user string, hash [32]byte) {
	t.Helper()
	code, body, _ := do(t, client(t), http.MethodPost, base+devicePath, "")
	if code != 200 {
		t.Fatalf("POST %s: %d %s", devicePath, code, lastBody)
	}
	device, _ = body["device_code"].(string)
	user, _ = body["user_code"].(string)
	return device, user, sha256.Sum256([]byte(device))
}

func poll(t *testing.T, base, device string) (int, map[string]any) {
	t.Helper()
	code, body, _ := do(t, client(t), http.MethodPost, base+pollPath, `{"device_code":"`+device+`"}`)
	return code, body
}

// design/AUTH.md §4.2 step by step: the shape, the hashed row, pending then slow_down, the approval
// as a signed-in browser, the session copied from the approver's row, and the code dead after it.
func TestDeviceFlowMintsAHashedGrantAndTheApprovedCodeIsASessionOnce(t *testing.T) {
	withOIDC(t) // CANYON_ADMINS is dana
	pool, ctx := migrated(t)
	base, logs := start(t, pool, time.Hour)
	device, user, hash := mint(t, base)
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	if len(device) != 2*tokenBytes || !shape.MatchString(user) {
		t.Fatalf("device_code %q, user_code %q", device, user)
	}
	_, body, _ := do(t, client(t), http.MethodPost, base+devicePath, "")
	if body["verification_uri"] != "http://localhost:7777/#/device" || body["verification_uri_complete"] != "http://localhost:7777/#/device?code="+body["user_code"].(string) || body["expires_in"] != 600.0 || body["interval"] != 5.0 {
		t.Fatalf("shape: %s", lastBody)
	}
	var rows string // every grant's text, the hash rendered as escaped bytes: where a plaintext code would show
	if err := pool.QueryRow(ctx, "SELECT string_agg(encode(code_hash, 'escape') || ' ' || user_code || ' ' || requested_by, ' ') FROM device_grants").Scan(&rows); err != nil || strings.Contains(rows, device) {
		t.Fatalf("the device code is stored in plaintext: %v %q", err, rows)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM device_grants WHERE code_hash = '\\x"+hex.EncodeToString(hash[:])+"' AND user_code = '"+strings.ReplaceAll(user, "-", "")+"' AND requested_by = '127.0.0.1' AND session_id IS NULL"); n != 1 {
		t.Fatalf("the grant row: %d, want 1 with the code's sha256, the bare user code and the source address", n)
	}
	for i, want := range []string{"authorization_pending", "slow_down", "slow_down"} {
		if code, body := poll(t, base, device); code != 400 || body["error"] != want {
			t.Fatalf("poll %d: %d %s, want 400 %s", i, code, lastBody, want)
		}
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	code, body, _ := do(t, dana, http.MethodGet, base+grantsPath+strings.ToLower(user), "")
	requestedAt, _ := body["requested_at"].(string) // a missing field is a failure below, not a panic here
	at, _ := time.Parse(time.RFC3339, requestedAt)
	if code != 200 || body["requested_by"] != "127.0.0.1" || time.Since(at) > time.Minute || fmt.Sprint(body["approver"]) != "map[display:sub-dana email:dana@meridian.example]" { // the page names who is approving
		t.Fatalf("GET the grant: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	code, body = poll(t, base, device)
	session, _ := body["session"].(string)
	expires, _ := time.Parse(time.RFC3339, body["expires_at"].(string))
	if code != 200 || len(session) != 2*tokenBytes || body["subject"] != "sub-dana" || body["display"] != "sub-dana" || body["org"] != "Meridian" || time.Until(expires).Round(time.Minute) != deviceSession {
		t.Fatalf("the approved poll: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE id = '"+session+"' AND flow = 'device' AND email = 'dana@meridian.example' AND actor_subject = 'sub-dana'"); n != 1 {
		t.Fatal("the device session row is not the approver's identity")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'sign in' AND detail->>'flow' = 'device' AND via = 'cli' AND role = 'admin' AND session = '"+sessionDigest(session)+"' AND source = '127.0.0.1'"); n != 1 {
		t.Fatalf("%d sign in audit rows for the device flow by the CLI session it minted, from where it asked, want 1", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM device_grants WHERE code_hash = '\\x"+hex.EncodeToString(hash[:])+"'"); n != 0 {
		t.Fatal("the grant row outlived its redemption")
	}
	if code, body := poll(t, base, device); code != 400 || body["error"] != "expired_token" {
		t.Fatalf("a second redemption: %d %s, want 400 expired_token", code, lastBody)
	}
	if !strings.Contains(logs.String(), "sign in sub=sub-dana dana@meridian.example flow=device role=admin") || strings.Contains(logs.String(), device) || strings.Contains(logs.String(), session) {
		t.Fatalf("the log names the sign-in and no code:\n%s", logs.String())
	}
}

// The role is computed from the session row's email and claimed_admin (roles_test.go), so a device
// session that did not copy them would make every CLI caller a member: dana's CLI is an admin by
// the list, bob's by the claim he signed in with, carol's is a member.
func TestADeviceSessionCarriesTheApproversEmailAndClaimedFlag(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	for i, c := range []struct {
		sub, email string
		claimed    bool
		want       int
	}{{"sub-dana", "dana@meridian.example", false, 200}, {"sub-bob", "bob@meridian.example", true, 200}, {"sub-carol", "carol@meridian.example", false, 403}} {
		device, user, _ := mint(t, base)
		if code, _, _ := do(t, signedIn(t, ctx, pool, base, c.sub, c.email, c.claimed), http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
			t.Fatalf("%s approves: %d %s", c.sub, code, lastBody)
		}
		code, body := poll(t, base, device)
		if code != 200 {
			t.Fatalf("%s's CLI: %d %s", c.sub, code, lastBody)
		}
		cli := client(t)
		u, _ := neturl.Parse(base)
		cli.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: body["session"].(string)}}) //nolint:gosec // test: the session the poll answered, as cli/client.go seeds its jar
		if code, _, _ := do(t, cli, http.MethodPut, base+preferencesPath, strings.Replace(stacksBody, "React", "Vue"+string(rune('a'+i)), 1)); code != c.want {
			t.Fatalf("%s's CLI on PUT /api/v1/preferences: %d %s, want %d", c.sub, code, lastBody, c.want)
		}
	}
}

func TestDeviceGrantsExpireAreDeniedAndCleanThemselvesOnInsert(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	old, another := sha256.Sum256([]byte("an-old-device-code")), sha256.Sum256([]byte("another"))
	if _, err := pool.Exec(ctx, "INSERT INTO device_grants (code_hash, user_code, expires_at) VALUES ($1, 'OLDOLD01', now() - interval '1 second'), ($2, 'OLDOLD02', now() - interval '1 second')", old[:], another[:]); err != nil {
		t.Fatal(err)
	}
	if code, body := poll(t, base, "an-old-device-code"); code != 400 || body["error"] != "expired_token" {
		t.Fatalf("an expired code: %d %s, want 400 expired_token", code, lastBody)
	}
	mint(t, base) // deletes every expired row in the statement that inserts
	if n := count(t, ctx, pool, "SELECT count(*) FROM device_grants"); n != 1 {
		t.Fatalf("%d grant rows after an insert over two expired ones, want 1", n)
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodGet, base+grantsPath+"OLDOLD01", ""); code != 404 || lastBody != `{"error":"not_found","message":"`+codeUnknown+`"}` {
		t.Fatalf("a wrong code: %d %s", code, lastBody)
	}
	device, user, _ := mint(t, base)
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/deny", ""); code != 204 {
		t.Fatalf("deny: %d %s", code, lastBody)
	}
	for _, want := range []string{"access_denied", "expired_token"} { // dead after the denial is told once
		if code, body := poll(t, base, device); code != 400 || body["error"] != want {
			t.Fatalf("after the denial: %d %s, want 400 %s", code, lastBody, want)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 404 {
		t.Fatalf("approving a denied code: %d, want 404", code)
	}
	testAuth, testOIDC = authOff, nil // with auth off there is no device flow: the approval routes are unrouted too (main_test.go has the Open pair)
	off, _ := start(t, pool, time.Hour)
	if code, _, _ := do(t, client(t), http.MethodGet, off+grantsPath+user, ""); code != 404 {
		t.Fatalf("the grant read with auth off: %d, want 404", code)
	}
}

func TestTheFlowRoutesShareOneBoundPerSourceAddressAndItRefills(t *testing.T) { // the bucket itself is limiter_test.go's; this is the routes under it
	withOIDC(t)
	pool, _ := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := browser(t) // the callback's refusal is a redirect now, and a client that followed it would land on the dashboard's route instead
	for i := 0; i < flowBurst; i++ {
		if code, _, _ := do(t, c, http.MethodGet, base+"/auth/callback", ""); code != 302 {
			t.Fatalf("callback %d: %d, want the refusal's redirect", i, code)
		}
	}
	for _, r := range []struct{ method, path string }{{http.MethodGet, "/auth/callback"}, {http.MethodPost, devicePath}, {http.MethodGet, "/auth/start"}} { // the poll is not in this bucket: TestThePollIsBoundedPerGrant…
		if code, body, _ := do(t, c, r.method, base+r.path, "{}"); code != 429 || body["error"] != "rate_limited" || body["retry_after_s"] != 1.0 {
			t.Fatalf("%s %s past the bound: %d %s, want 429 rate_limited retry_after_s 1", r.method, r.path, code, lastBody)
		}
	}
}

// The poll is the one high-volume flow route and behind an ingress every CLI shares one address:
// it is never in the address bucket, its bound is slow_down per grant. And the approver's session
// revoked between approve and poll mints nothing: the poll answers expired_token and no row appears.
func TestThePollIsBoundedPerGrantNotPerAddressAndARevokedApproverMintsNothing(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	device, user, _ := mint(t, base)
	for i := 0; i < 130; i++ { // past the address bucket's burst: never 429, the answer is the interval's
		if code, body := poll(t, base, device); code != 400 || (body["error"] != "authorization_pending" && body["error"] != "slow_down") {
			t.Fatalf("poll %d: %d %s", i, code, lastBody)
		}
	}
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE actor_subject = 'sub-dana'"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(pollInterval + time.Second) // past the interval, so the answer is the grant's and not slow_down
	if code, body := poll(t, base, device); code != 400 || body["error"] != "expired_token" || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device'") != 0 {
		t.Fatalf("a revoked approver: %d %s, device sessions %d", code, lastBody, count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device'"))
	}
}

// Two polls of one approved code at the same instant: exactly one mints, the other finds the grant gone.
func TestTwoPollsOfOneApprovedCodeMintOneSession(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	device, user, _ := mint(t, base)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	time.Sleep(pollInterval + time.Second)
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { // a raw request each: the test helper records the last body in one place, which two goroutines must not share
			resp, err := http.Post(base+"/auth/device/token", "application/json", strings.NewReader(`{"device_code":"`+device+`"}`)) //nolint:gosec // the test server's url
			if err != nil {
				codes <- 0
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	got := []int{<-codes, <-codes}
	if (got[0] == 200) == (got[1] == 200) || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device'") != 1 {
		t.Fatalf("two polls: %v, device sessions %d; want one 200 and one row", got, count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device'"))
	}
}

// A grant approved and then expired before the poll mints nothing: the redeeming statement checks
// the grant's own expiry, not only the approver's session.
func TestAnApprovedGrantThatExpiredBeforeThePollMintsNothing(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	device, user, hash := mint(t, base)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("approve: %d %s", code, lastBody)
	}
	if _, err := pool.Exec(ctx, "UPDATE device_grants SET expires_at = now() - interval '1 second' WHERE code_hash = $1", hash[:]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(pollInterval + time.Second)
	if code, body := poll(t, base, device); code != 400 || body["error"] != "expired_token" || count(t, ctx, pool, "SELECT count(*) FROM sessions WHERE flow = 'device'") != 0 {
		t.Fatalf("an approved grant past its expiry: %d %s", code, lastBody)
	}
}

// Only a browser session reads, approves or denies a code (design/AUTH.md §4.2): a CLI session that
// approved one would be copied into a fresh CLI session, so a stolen token could renew itself
// forever. Each refusal is a denied row, and the grant stays pending for a browser to decide.
func TestACLISessionNeitherReadsNorApprovesNorDeniesADeviceCode(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	device, user, _ := mint(t, base)
	cli := cliIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example")
	want := `{"error":"dashboard_required","message":"open the link canyon login printed in your browser, signed in to Canyon, to approve or deny a CLI sign-in"}`
	for _, r := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodPost, "/approve"}, {http.MethodPost, "/deny"}} {
		if code, _, _ := do(t, cli, r.method, base+grantsPath+user+r.suffix, ""); code != 403 || lastBody != want {
			t.Fatalf("a CLI session, %s %s: %d %s", r.method, r.suffix, code, lastBody)
		}
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'denied' AND via = 'cli' AND detail->>'error' = 'dashboard_required'"); n != 3 {
		t.Fatalf("%d denied rows, want 3", n)
	}
	if code, body := poll(t, base, device); code != 400 || body["error"] != "authorization_pending" {
		t.Fatalf("the grant after the CLI's attempts: %d %s, want it pending", code, lastBody)
	}
	if code, _, _ := do(t, signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false), http.MethodPost, base+grantsPath+user+"/approve", ""); code != 204 {
		t.Fatalf("the browser approves: %d %s", code, lastBody)
	}
}
