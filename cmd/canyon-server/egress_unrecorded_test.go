package main

import (
	"strings"
	"testing"
)

// With no Envoy stream open, the receiver warns once a minute that no call out is being recorded — while
// egress is in use: the host's via is canyon, or an app env still runs under it. Off by design, as the laptop
// demo starts, nothing goes through Envoy and the warning is noise (#2697); an open stream silences it.
func TestTheNoStreamWarningWaitsForEgress(t *testing.T) {
	f := newEgressFixture(t)
	const warning = "egress: no Envoy stream is open, so no call out is being recorded"
	warns := func() bool {
		before := strings.Count(f.logged.String(), warning)
		f.r.unrecorded(f.ctx)
		return strings.Count(f.logged.String(), warning) > before
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if warns() {
		t.Fatalf("via off and no app env under canyon: it warned\n%s", f.logged.String())
	}
	exec("UPDATE egress_settings SET via = 'canyon'")
	if !warns() {
		t.Fatalf("via canyon: no warning\n%s", f.logged.String())
	}
	f.r.open <- struct{}{} // a stream open
	if warns() {
		t.Fatalf("via canyon, a stream open: it warned\n%s", f.logged.String())
	}
	<-f.r.open
	exec("UPDATE egress_settings SET via = 'off'")
	exec("INSERT INTO projects (id) VALUES ('claims-portal'); INSERT INTO egress_envs (project_id, env) VALUES ('claims-portal', 'prod')")
	if !warns() {
		t.Fatalf("via off, and an app env still runs under canyon: no warning\n%s", f.logged.String())
	}
	exec("DELETE FROM egress_envs; DELETE FROM egress_settings")
	if !warns() {
		t.Fatalf("no setting to read: no warning\n%s", f.logged.String())
	}
}
