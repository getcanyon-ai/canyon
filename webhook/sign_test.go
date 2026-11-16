package webhook

import "testing"

// The test vector is independent of Sign: computed with
// `openssl dgst -sha256 -hmac "wh_sec_9f2a8c3d4e5b6a71" -hex` over the exact body bytes below.
const (
	testSecret = "wh_sec_9f2a8c3d4e5b6a71" //nolint:gosec // a test fixture, not a real secret
	testBody   = `{"event":"deploy","delivery":"5f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b"}`
	testSig    = "sha256=684245651c6a5d2a560fe1d68b0f0a4d12eee2899f84684e135b241cdedfc442"
)

func TestSignMatchesAnIndependentlyComputedVector(t *testing.T) {
	if got := Sign([]byte(testSecret), []byte(testBody)); got != testSig {
		t.Fatalf("Sign(%q, %q) = %q, want %q (openssl dgst -sha256 -hmac)", testSecret, testBody, got, testSig)
	}
	if !Verify([]byte(testSecret), []byte(testBody), testSig) {
		t.Fatal("Verify must accept the signature Sign produces")
	}
}

func TestVerifyRefusesAWrongSecretOrATamperedBody(t *testing.T) {
	if Verify([]byte("wrong-secret"), []byte(testBody), testSig) {
		t.Fatal("a signature under another secret must not verify")
	}
	if Verify([]byte(testSecret), []byte(testBody+"x"), testSig) {
		t.Fatal("a tampered body must not verify")
	}
	if Verify([]byte(testSecret), []byte(testBody), "sha256=00") {
		t.Fatal("a malformed signature must not verify")
	}
}
