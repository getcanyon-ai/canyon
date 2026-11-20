package vault

import (
	"bytes"
	"testing"
	"time"
)

// A session is one holder within a 30-day period and another in the next; two sessions are never one holder, and the
// session's own bytes never appear in it (design/specs/mcp.md §3.5).
func TestAHolderIsOneSessionInOnePeriod(t *testing.T) {
	v := load(t)
	start := time.Unix(30*24*3600*700, 0) // the first second of a period
	a, err := v.Holder("ses_one", start)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := v.Holder("ses_one", start.Add(30*24*time.Hour-time.Second))
	next, _ := v.Holder("ses_one", start.Add(30*24*time.Hour))
	other, _ := v.Holder("ses_two", start)
	if len(a) != 32 || !bytes.Equal(a, same) {
		t.Fatalf("one session in one period is two holders: %x, %x", a, same)
	}
	if bytes.Equal(a, next) || bytes.Equal(a, other) {
		t.Fatal("a holder outlives its period, or two sessions share one")
	}
	if again, _ := load(t).Holder("ses_one", start); bytes.Equal(a, again) {
		t.Fatal("another key file makes the same holder: the key is not in it")
	}
}
