package main

import (
	"strings"
	"testing"
)

// The schema's maxLength counts code points, so a non-ASCII value is as long as its characters.
func TestLengthLimitsCountCodePointsNotBytes(t *testing.T) {
	if r := strings.Repeat("失", 1024); !validReason(r) { // 3072 bytes, 1024 code points
		t.Errorf("a 1024-character CJK reason is refused")
	}
	if r := strings.Repeat("失", 1025); validReason(r) {
		t.Errorf("a 1025-character reason is accepted")
	}
	if u := "https://deploy.example/" + strings.Repeat("é", 2000); !validHTTPS(u) { // 2023 code points, 4023 bytes
		t.Errorf("a 2023-character https URL with a non-ASCII path is refused")
	}
	if u := "https://deploy.example/" + strings.Repeat("é", 2030); validHTTPS(u) {
		t.Errorf("a 2053-character URL is accepted")
	}
}
