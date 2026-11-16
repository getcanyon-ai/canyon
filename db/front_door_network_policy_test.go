package db

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// The migration that records when a front door's save found NetworkPolicy enforced (#2507) starts every stored door
// not seen enforcing, probed or not: nothing records which executor probed it, so whether its probe caught a new pod
// let out before its policy is unknown, and an admin saves it once.
func TestTheEnforcedVerdictStartsUnknown(t *testing.T) {
	const enforced = "migrations/0110_front_door_network_policy.sql"
	now := "2026-10-26T00:00:00Z"
	for _, c := range []struct {
		name     string
		probedAt *string
	}{{"probed", &now}, {"not probed", nil}} {
		p, ctx := pgtest.Schema(t)
		files := upTo(t, enforced)
		if _, err := migrate(ctx, p, files); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display, set_at, probed_at)
			VALUES ('{"choice": 1}', 'person', 'dana', 'Dana', '2026-09-01T00:00:00Z', $1::timestamptz)`, c.probedAt); err != nil {
			t.Fatal(err)
		}
		data, err := migrations.ReadFile(enforced)
		if err != nil {
			t.Fatal(err)
		}
		files[enforced] = &fstest.MapFile{Data: data}
		if _, err := migrate(ctx, p, files); err != nil {
			t.Fatal(err)
		}
		var seen *time.Time
		if err := p.QueryRow(ctx, "SELECT network_policy_enforced_at FROM front_doors").Scan(&seen); err != nil || seen != nil {
			t.Fatalf("%s: network_policy_enforced_at %v, want null (%v)", c.name, seen, err)
		}
	}
}

// A verdict belongs to the save that wrote it (#2507): a save that updates warning, probed_at and probe_fix and not the
// verdict — the release before's save of the same door — clears it, whatever it found; a save that writes the verdict
// anew, enforced again, keeps it, moved; the vault rotation's rewrap of the sealed columns leaves it as it is.
func TestASaveThatDoesNotWriteTheVerdictClearsIt(t *testing.T) {
	p, ctx := pgtest.Schema(t)
	if _, err := migrate(ctx, p, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO front_doors (door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display, probed_at, network_policy_enforced_at)
		VALUES ('{"choice": 2}', 1, '\x00', '\x00', 'person', 'dana', 'Dana', now(), now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	seen := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := p.QueryRow(ctx, "SELECT network_policy_enforced_at FROM front_doors").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := seen()
	if _, err := p.Exec(ctx, "UPDATE front_doors SET sealed_key = '\\x01', sealed_value = '\\x01'"); err != nil {
		t.Fatal(err)
	}
	if at := seen(); at == nil || !at.Equal(*before) {
		t.Fatalf("the rewrap moved the verdict: %v, was %v", at, before)
	}
	if _, err := p.Exec(ctx, "UPDATE front_doors SET warning = '', probed_at = now(), probe_fix = '', network_policy_enforced_at = now()"); err != nil {
		t.Fatal(err)
	}
	if at := seen(); at == nil || !at.After(*before) {
		t.Fatalf("a save that found it enforced again: %v, was %v", at, before)
	}
	if _, err := p.Exec(ctx, "UPDATE front_doors SET warning = '', probed_at = now(), probe_fix = ''"); err != nil {
		t.Fatal(err)
	}
	if at := seen(); at != nil {
		t.Fatalf("a save that did not write the verdict left it: %v", at)
	}
}
