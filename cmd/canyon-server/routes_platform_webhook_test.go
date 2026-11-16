package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/webhook"
	"github.com/jackc/pgx/v5"
)

const webhookPath = "/api/v1/platform-webhook"

// A member is refused before anything is read or written; an admin's PUT answers the secret and
// the host token once, and never again — not on GET, not on a later PUT that keeps the stored
// secret. The sealed value opens to the secret the admin sent.
func TestPlatformWebhookIsAdminOnlyAndAnswersItsSecretAndTokenOnce(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "dana@meridian.example", "dana@meridian.example", false)
	carol := signedIn(t, ctx, pool, base, "sub-carol", "carol@meridian.example", false)
	const url = "https://deploy.meridian.example/hooks/canyon"

	// None saved is a defined answer, never an error status a browser logs (#2560): 200 null.
	if code, _, _ := do(t, carol, http.MethodGet, base+webhookPath, ""); code != 200 || lastBody != "null" {
		t.Fatalf("a member's GET before any save: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carol, http.MethodPut, base+webhookPath, `{"url":"`+url+`"}`); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's PUT: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, carol, http.MethodPost, base+webhookPath+"/token", ""); code != 403 || lastBody != adminRequired {
		t.Fatalf("a member's rotate: %d %s", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_webhook"); n != 0 {
		t.Fatalf("a refused call wrote a row: %d", n)
	}

	// The first save carries no secret: Canyon mints one and answers it, once, alongside the
	// freshly minted host token.
	code, saved, _ := do(t, dana, http.MethodPut, base+webhookPath, `{"url":"`+url+`"}`)
	mintedSecret, _ := saved["secret"].(string)
	firstToken, _ := saved["token"].(string)
	if code != 200 || mintedSecret == "" || firstToken == "" || saved["url"] != url {
		t.Fatalf("the first PUT answers the minted secret and the minted token: %d %s", code, lastBody)
	}
	setBy, _ := saved["set_by"].(map[string]any)
	if setBy["kind"] != "person" || setBy["subject"] != "dana@meridian.example" {
		t.Fatalf("set_by is the actor: %v", setBy)
	}

	// The GET never carries a secret or a token, before or after any save.
	code, get, _ := do(t, carol, http.MethodGet, base+webhookPath, "")
	lastDelivery, hasLastDelivery := get["last_delivery"]
	if code != 200 || get["url"] != url || !hasLastDelivery || lastDelivery != nil {
		t.Fatalf("GET: %d %s", code, lastBody)
	}
	if _, has := get["secret"]; has {
		t.Fatal("GET must never carry the secret")
	}
	if _, has := get["token"]; has {
		t.Fatal("GET must never carry the token")
	}

	// A PUT of the same url with no secret keeps the stored one, and a PUT that carries a new
	// secret uses it — neither ever answers the secret again, and neither answers or changes
	// the token: only the first save ever carries it.
	code, kept, _ := do(t, dana, http.MethodPut, base+webhookPath, `{"url":"`+url+`"}`)
	if code != 200 || kept["url"] != url {
		t.Fatalf("the second PUT: %d %s", code, lastBody)
	}
	if _, has := kept["secret"]; has {
		t.Fatal("a PUT that keeps the stored secret must not answer it again")
	}
	if _, has := kept["token"]; has {
		t.Fatal("a PUT after the first save must never answer the token")
	}
	if strings.Contains(lastBody, mintedSecret) {
		t.Fatal("the response of a kept-secret PUT must not contain the secret")
	}

	const replaced = "wh_sec_9f2a8c3d4e5b6a71" //nolint:gosec // a test fixture, not a real secret
	code, resealed, _ := do(t, dana, http.MethodPut, base+webhookPath, `{"url":"`+url+`","secret":"`+replaced+`"}`)
	if code != 200 || resealed["url"] != url {
		t.Fatalf("the third PUT: %d %s", code, lastBody)
	}
	if _, has := resealed["secret"]; has {
		t.Fatal("a PUT carrying a secret must not answer it either")
	}
	if _, has := resealed["token"]; has {
		t.Fatal("a PUT carrying a secret still must not answer or rotate the token")
	}

	var keyRef int
	var sealedKey, sealedValue []byte
	if err := pool.QueryRow(ctx, "SELECT key_ref, sealed_key, sealed_value FROM platform_webhook").Scan(&keyRef, &sealedKey, &sealedValue); err != nil {
		t.Fatal(err)
	}
	opened, err := vaultOf(t, base).Open(keyRef, "platform-webhook", "webhook", "secret", sealedKey, sealedValue)
	if err != nil || string(opened) != replaced {
		t.Fatalf("the sealed secret opens to the third PUT's, the last one sent: %q (%v)", opened, err)
	}
	if ok, err := hostTokenValid(ctx, pool, firstToken); err != nil || !ok {
		t.Fatalf("the first save's token is still valid after two re-saves: %v %v", ok, err)
	}

	rows, _ := pool.Query(ctx, "SELECT detail FROM audit_events WHERE action = 'set platform webhook' ORDER BY ts")
	details, err := pgx.CollectRows(rows, pgx.RowTo[map[string]any])
	if err != nil || len(details) != 3 || details[0]["secret_replaced"] != false || details[1]["secret_replaced"] != false || details[2]["secret_replaced"] != true ||
		strings.Contains(string(mustJSON(details)), mintedSecret) || strings.Contains(string(mustJSON(details)), replaced) {
		t.Fatalf("one audit row per PUT — false on the first save (nothing was replaced), false on the kept re-save, true on the re-save that replaced it — never a secret: %v (%v)", details, err)
	}
}

// A URL re-save — with or without a secret in the body — must never rotate the host token: only
// POST .../token does. This is the rule the fix-up round pinned after the branch first shipped
// (every PUT minting a fresh token); it is exercised on its own so a regression here is unambiguous.
func TestPlatformWebhookAResaveNeverRotatesTheToken(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	_, first, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks"}`)
	firstToken, _ := first["token"].(string)
	if firstToken == "" {
		t.Fatalf("the first save mints a token: %s", lastBody)
	}
	if code, second, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks"}`); code != 200 {
		t.Fatalf("the second PUT (same url, no secret): %d %s", code, lastBody)
	} else if _, has := second["token"]; has {
		t.Fatal("a same-url re-save must not answer a token")
	}
	if code, third, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks2","secret":"wh_sec_resave_5c8a1f2e"}`); code != 200 { //nolint:gosec // a test fixture, not a real secret
		t.Fatalf("the third PUT (a new url and secret): %d %s", code, lastBody)
	} else if _, has := third["token"]; has {
		t.Fatal("a re-save with a new url and a new secret must still not answer or rotate the token")
	}
	if ok, err := hostTokenValid(ctx, pool, firstToken); err != nil || !ok {
		t.Fatalf("the first save's token must still be the current one after two re-saves: %v %v", ok, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'set platform webhook'"); n != 3 {
		t.Fatalf("three PUTs, three audit rows: %d", n)
	}
}

// A non-https url is refused with the contract's error, and so is a secret out of shape; neither writes anything.
func TestPlatformWebhookRefusesANonHTTPSURLAndABadSecret(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	for _, tc := range []struct{ body, wantErr string }{
		{`{"url":"http://deploy.meridian.example/hooks"}`, "invalid_url"},
		{`{"url":""}`, "invalid_url"},
		{`{"url":"not a url"}`, "invalid_url"},
		{`{"url":"https://deploy.meridian.example/hooks","secret":""}`, "invalid_secret"},
		{`{"url":"https://deploy.meridian.example/hooks","secret":"has a space"}`, "invalid_secret"},
		{`{"url":"https://deploy.meridian.example/hooks","secret":"` + strings.Repeat("a", 1025) + `"}`, "invalid_secret"},
	} {
		code, body, _ := do(t, c, http.MethodPut, base+webhookPath, tc.body)
		if code != 400 || body["error"] != tc.wantErr {
			t.Fatalf("%s: %d %s, want %s", tc.body, code, lastBody, tc.wantErr)
		}
	}
	// the message names what is taken: tokenRe refuses a space, which "printable ASCII" admits (#2425)
	if code, body, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks","secret":"has a space"}`); code != 400 ||
		body["message"] != "secret: 1 to 1024 characters, printable ASCII without spaces" {
		t.Fatalf("a secret with a space: %d %s, want the message naming what is taken", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM platform_webhook"); n != 0 {
		t.Fatalf("every refusal wrote nothing: %d row(s)", n)
	}
}

// A delivery signed under the stored secret verifies under an independently computed HMAC, and
// the row itself holds no plaintext, of the secret or of the token.
func TestPlatformWebhookDeliverySignatureVerifiesAndNothingIsStoredInPlaintext(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	const url, secret = "https://deploy.meridian.example/hooks/canyon", "wh_sec_delivery_4a7c9e21" //nolint:gosec // a test fixture, not a real secret
	code, saved, _ := do(t, client(t), http.MethodPut, base+webhookPath, `{"url":"`+url+`","secret":"`+secret+`"}`)
	if _, has := saved["secret"]; code != 200 || has {
		t.Fatalf("PUT: %d %s (a PUT carrying a secret must not answer it)", code, lastBody)
	}
	body := []byte(`{"event":"deploy","delivery":"5f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b","release":"rel_8f2a"}`)
	signed := webhook.Sign([]byte(secret), body)

	// Computed independently of webhook.Sign, with the standard library alone.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	independent := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if signed != independent {
		t.Fatalf("Sign disagrees with an independent HMAC-SHA256: %q vs %q", signed, independent)
	}
	if !webhook.Verify([]byte(secret), body, independent) {
		t.Fatal("a delivery signed under the stored secret must verify")
	}
	if webhook.Verify([]byte(secret), append(append([]byte{}, body...), 'x'), independent) {
		t.Fatal("a tampered body must not verify")
	}

	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT to_jsonb(platform_webhook) FROM platform_webhook").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the row holds no plaintext secret: %s", raw)
	}
	token, _ := saved["token"].(string)
	if token == "" || strings.Contains(string(raw), token) {
		t.Fatalf("the row holds no plaintext host token: %s", raw)
	}
	var hashLen int
	if err := pool.QueryRow(ctx, "SELECT octet_length(token_hash) FROM platform_webhook").Scan(&hashLen); err != nil || hashLen != sha256.Size {
		t.Fatalf("token_hash is a SHA-256 digest: %d bytes (%v)", hashLen, err)
	}
}

// Rotating the host token invalidates the old one at once, under the constant-time check the
// report route (PR 4) authenticates with; the new one, and only the new one, is valid straight away.
func TestPlatformWebhookRotationInvalidatesTheOldTokenImmediately(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	c := client(t)
	_, saved, _ := do(t, c, http.MethodPut, base+webhookPath, `{"url":"https://deploy.meridian.example/hooks","secret":"wh_sec_rotate_1a2b3c4d"}`) //nolint:gosec // a test fixture, not a real secret
	oldToken, _ := saved["token"].(string)
	if oldToken == "" {
		t.Fatalf("PUT: %s", lastBody)
	}
	if ok, err := hostTokenValid(ctx, pool, oldToken); err != nil || !ok {
		t.Fatalf("the freshly minted token is valid: %v %v", ok, err)
	}
	code, rotated, _ := do(t, c, http.MethodPost, base+webhookPath+"/token", "")
	newToken, _ := rotated["token"].(string)
	if code != 200 || newToken == "" || newToken == oldToken {
		t.Fatalf("rotate: %d %s", code, lastBody)
	}
	if ok, err := hostTokenValid(ctx, pool, oldToken); err != nil || ok {
		t.Fatalf("the old token must be refused immediately after rotation: %v %v", ok, err)
	}
	if ok, err := hostTokenValid(ctx, pool, newToken); err != nil || !ok {
		t.Fatalf("the rotated token must be valid: %v %v", ok, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM audit_events WHERE action = 'rotate platform token'"); n != 1 {
		t.Fatalf("one audit row for the rotation: %d", n)
	}

	// A rotate with nothing configured yet has nothing to rotate.
	pool2, _ := migrated(t)
	base2, _ := start(t, pool2, time.Hour)
	if code, body, _ := do(t, client(t), http.MethodPost, base2+webhookPath+"/token", ""); code != 404 || body["error"] != "not_found" {
		t.Fatalf("rotate before any webhook is set: %d %s", code, lastBody)
	}
}
