// Package webhook signs and verifies a release delivery's body (design/HAND-OFF.md §3;
// contracts/routes.md, the platform-webhook row): HMAC-SHA256 of the exact raw body under the
// stored secret, GitHub's scheme byte for byte, so every receiver library already verifies it.
// PR 3 (releases: send) signs every delivery with Sign; the org's pipeline verifies with it too.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Sign is the Canyon-Signature-256 header's value for body under secret: "sha256=" then the
// lower-case hex of the HMAC-SHA256 digest.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is whether signature — a Canyon-Signature-256 header's value — is Sign(secret, body),
// compared in constant time.
func Verify(secret, body []byte, signature string) bool {
	return hmac.Equal([]byte(signature), []byte(Sign(secret, body)))
}
