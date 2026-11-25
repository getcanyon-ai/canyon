package main

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// Calls out not being recorded is said when Canyon has a sign of it, never for an idle host (#146): no Envoy stream is
// open until Envoy has a call to send, so a host nobody calls out from has none, and that is no fault. A loss Envoy or
// egress-control reported, or a stream the receiver refused or couldn't read, is said, once a minute at most.
func TestCallsOutNotRecordedAreSaidOnlyWhenSomethingWasLost(t *testing.T) {
	var logged syncBuffer
	r := newEgressReceiver(unreachable(t), nil, log.New(&logged, "", 0))
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	const warning = "egress: some calls out weren't recorded"
	said := func() int { return strings.Count(logged.String(), warning) }

	for range 60 { // an idle hour of flushes and ticks: no stream open, nothing lost
		_ = r.flush(ctx, nil)
		now = now.Add(time.Minute)
	}
	if said() != 0 {
		t.Fatalf("an idle host said calls out aren't recorded:\n%s", logged.String())
	}
	r.envoy.add(nil) // Envoy lost entries no one counted: a stream to the receiver that failed to open
	_ = r.flush(ctx, nil)
	if said() != 1 {
		t.Fatalf("a loss Envoy reported: said %d times, want once\n%s", said(), logged.String())
	}
	r.envoy.add(nil)
	_ = r.flush(ctx, nil)
	if said() != 1 {
		t.Fatalf("a second loss within the minute: said %d times, want once\n%s", said(), logged.String())
	}
	now = now.Add(time.Minute)
	r.refused.Add(1) // a stream the receiver refused, past its limit
	_ = r.flush(ctx, nil)
	if said() != 2 {
		t.Fatalf("a stream refused a minute later: said %d times, want twice\n%s", said(), logged.String())
	}
}
