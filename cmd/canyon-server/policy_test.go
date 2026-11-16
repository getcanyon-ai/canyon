package main

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

// withPolicyEdits is a migrated schema and an app whose boot snapshot is the fixture as the
// server boots it: the composer reads the table and the snapshot, nothing else on app.
func withPolicyEdits(t *testing.T) (*pgxpool.Pool, context.Context, *app) {
	t.Helper()
	pool, ctx := migrated(t)
	return pool, ctx, &app{pool: pool, boot: testPolicy()}
}

// edit appends one row the way PUT /policies/:rule will, with its effective_at an interval from
// now — negative for one that has passed — and a grace of that many whole days ahead, none for one
// that has passed: an edit with no grace is in effect by being written.
func edit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rule, state, effectiveIn string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject, set_by_display) VALUES ($1, $2, (SELECT COALESCE(MAX(version), 0) + 1 FROM policy_edits), GREATEST(extract(day FROM $3::interval), 0), now() + $3::interval, 'person', 'dana@meridian.example', 'Dana Whitfield')", rule, state, effectiveIn); err != nil {
		t.Fatal(err)
	}
}

// editSetAt is edit with the instant it was set named too, for the ordering cases.
func editSetAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rule, state, effectiveIn, setIn string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject, set_at) VALUES ($1, $2, (SELECT COALESCE(MAX(version), 0) + 1 FROM policy_edits), GREATEST(extract(day FROM $3::interval), 0), now() + $3::interval, 'person', 'dana@meridian.example', now() + $4::interval)", rule, state, effectiveIn, setIn); err != nil {
		t.Fatal(err)
	}
}

func stateOf(s gatepolicy.Snapshot, id string) gatepolicy.State {
	for _, r := range s.Rules {
		if r.ID == id {
			return r.State
		}
	}
	return "missing"
}

// With no edits the composed snapshot is the boot snapshot, hash and version included: the base
// carries the hash of its own content, so composing is a no-op until an edit lands.
func TestWithNoEditsTheComposerAnswersTheBootSnapshot(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a.boot) {
		t.Fatalf("composed with no edits:\n%+v\nboot:\n%+v", got, a.boot)
	}
}

// An edit applied without grace bites the next read: the rule is at the new state, and the
// snapshot's hash and version have moved, once.
func TestAppliedWithoutGraceTakesEffectAtOnce(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "auth.unauthenticated-route", "blocking", "0")
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(got, "auth.unauthenticated-route"); st != gatepolicy.Blocking {
		t.Fatalf("after the edit: %s", st)
	}
	if boot := a.boot; got.Hash == boot.Hash || got.Version != boot.Version+1 {
		t.Fatalf("edited: %s v%d; boot %s v%d", got.Hash, got.Version, boot.Hash, boot.Version)
	}
}

// An edit whose grace has not elapsed is not effective: the rule keeps its state and the snapshot
// does not move. A cache would be wrong the instant the grace elapsed, which is why there is none.
func TestAnEditStillInGraceDoesNotBiteYet(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "auth.unauthenticated-route", "blocking", "14 days")
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, a.boot) {
		t.Fatalf("a scheduled edit bit early: %s v%d, %s", got.Hash, got.Version, stateOf(got, "auth.unauthenticated-route"))
	}
}

// Of two edits that have both started, the newer one wins.
func TestTheNewerEditWins(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "owner.required", "off", "-1 hour")
	edit(t, ctx, pool, "owner.required", "blocking", "-1 minute")
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(got, "owner.required"); st != gatepolicy.Blocking {
		t.Fatalf("two edits: %s", st)
	}
}

// A row naming a rule the snapshot does not carry fails the read: the composer answers an error,
// never a snapshot with the row skipped.
// A row for a rule the snapshot does not carry — one a later release retired — is not read: it
// cannot loosen a gate, and failing every request on it would be an outage with no way out but a
// DELETE. WithStates itself still refuses an unknown id (gate/policy's test), so the write path
// cannot create one.
func TestARowForARuleTheSnapshotDoesNotCarryIsNotRead(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "nope.rule", "blocking", "0")
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil || !reflect.DeepEqual(got, a.boot) {
		t.Fatalf("with a retired rule's row: %v, %s v%d", err, got.Hash, got.Version)
	}
}

// An edit scheduled with a grace and then superseded by a later decision never wakes: the newest
// edit is by when it was set, not by when it would have started.
func TestASupersededScheduledEditNeverWakes(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	editSetAt(t, ctx, pool, "auth.unauthenticated-route", "blocking", "-1 minute", "-15 days") // set two weeks ago with a fortnight's grace: its start has just passed
	editSetAt(t, ctx, pool, "auth.unauthenticated-route", "advisory", "-7 days", "-7 days")    // set a week ago, at once: the later decision
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if st := stateOf(got, "auth.unauthenticated-route"); st != gatepolicy.Advisory {
		t.Fatalf("the superseded edit woke: %s", st)
	}
}

// Two edits set in the same instant: the later row wins, deterministically.
func TestTwoEditsInOneInstantPickTheLaterRow(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	// One statement, so both rows carry the same now(): two statements would be two instants.
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, version, effective_at, set_by_kind, set_by_subject, set_at) VALUES ('owner.required', 'off', 1, now() - interval '1 hour', 'person', 'x', now() - interval '1 hour'), ('owner.required', 'blocking', 2, now() - interval '1 hour', 'person', 'x', now() - interval '1 hour')"); err != nil {
		t.Fatal(err)
	}
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil || stateOf(got, "owner.required") != gatepolicy.Blocking {
		t.Fatalf("same instant: %s (%v)", stateOf(got, "owner.required"), err)
	}
}

// The table refuses what the route will refuse: a grace outside 0..365, a state outside the
// three, a version of zero.
func TestTheEditTableRefusesWhatTheContractRefuses(t *testing.T) {
	pool, ctx, _ := withPolicyEdits(t)
	for name, stmt := range map[string]string{
		"grace 366":     "INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject) VALUES ('owner.required', 'off', 1, 366, now(), 'person', 'x')",
		"state paused":  "INSERT INTO policy_edits (rule, state, version, effective_at, set_by_kind, set_by_subject) VALUES ('owner.required', 'paused', 1, now(), 'person', 'x')",
		"version zero":  "INSERT INTO policy_edits (rule, state, version, effective_at, set_by_kind, set_by_subject) VALUES ('owner.required', 'off', 0, now(), 'person', 'x')",
		"kind stranger": "INSERT INTO policy_edits (rule, state, version, effective_at, set_by_kind, set_by_subject) VALUES ('owner.required', 'off', 1, now(), 'stranger', 'x')",
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s: inserted", name)
		}
	}
}

// The version is the boot version plus the count of edits in effect: every change moves it once,
// an elapsing grace included. A is applied first with a grace and B second without one, so B is in
// effect while A is not — counting the newest in-effect edit's own number reads base+2 straight
// away and does not move when A's grace elapses, which is a change to the effective states the
// version does not show.
func TestEveryChangeMovesTheVersionOnce(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	boot := a.boot.Version
	editSetAt(t, ctx, pool, "stack.blessed", "blocking", "14 days", "-1 minute") // A: set first, still in grace
	editSetAt(t, ctx, pool, "owner.required", "blocking", "0", "0")              // B: set second, in effect at once
	version := func(want int, when string) {
		t.Helper()
		got, err := a.effectivePolicy(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != want {
			t.Fatalf("%s: v%d, want v%d", when, got.Version, want)
		}
	}
	version(boot+1, "B in effect, A still in grace")
	if _, err := pool.Exec(ctx, "UPDATE policy_edits SET effective_at = now() - interval '1 second' WHERE rule = 'stack.blessed'"); err != nil {
		t.Fatal(err)
	}
	version(boot+2, "A's grace elapsed")
}

// The newest edit per rule is by the instant it was set, whatever its effective_at: the actor
// and grace GET /policies shows beside a rule, and the scheduled change when it is still ahead.
func TestTheNewestEditPerRuleIsByWhenItWasSet(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "owner.required", "blocking", "0")
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, version, grace_days, effective_at, set_by_kind, set_by_subject, set_at) VALUES ('owner.required', 'off', 2, 14, now() + interval '14 days', 'person', 'lee@meridian.example', now() + interval '1 second')"); err != nil {
		t.Fatal(err)
	}
	got, err := newestEdits(ctx, pool, ruleIDs(a.boot))
	if err != nil {
		t.Fatal(err)
	}
	e := got["owner.required"].newest
	if len(got) != 1 || e.State != gatepolicy.Off || e.GraceDays != 14 || e.SetBy != (actor{Kind: "person", Subject: "lee@meridian.example"}) || !e.EffectiveAt.After(e.SetAt) || !e.Ahead {
		t.Fatalf("the newest edit: %+v", got)
	}
	if s, err := a.effectivePolicy(ctx, pool); err != nil || stateOf(s, "owner.required") != gatepolicy.Blocking {
		t.Fatalf("effective while the newer one is in grace: %v (%v)", stateOf(s, "owner.required"), err)
	}
}

// Added tools follow the boot snapshot's rules sorted bytewise by id, as Build sorts registered rules: neither the
// order they were added in nor the database's collation moves the hash. The second host's column
// sorts as glibc's en_US does, ignoring the dash a tool id may carry, so it orders sarif.a0 first.
func TestAddedToolsAreSortedByIDWhateverTheOrderOrCollation(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	compose := func(ids ...string) gatepolicy.Snapshot {
		t.Helper()
		if _, err := pool.Exec(ctx, "DELETE FROM policy_edits"); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			edit(t, ctx, pool, id, "advisory", "0")
		}
		s, err := a.effectivePolicy(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := compose("sarif.a0", "sarif.a-b")
	if _, err := pool.Exec(ctx, "CREATE COLLATION shifted (provider = icu, locale = 'en-US-u-ka-shifted'); ALTER TABLE policy_edits ALTER COLUMN rule TYPE text COLLATE shifted"); err != nil {
		t.Fatal(err)
	}
	pool.Reset() // the pooled connections' cached plans predate the column's collation
	second := compose("sarif.a-b", "sarif.a0")
	if ids := ruleIDs(second)[len(a.boot.Rules):]; first.Hash != second.Hash || !reflect.DeepEqual(ids, []string{"sarif.a-b", "sarif.a0"}) {
		t.Fatalf("added tools: %v, hash %s then %s", ids, first.Hash, second.Hash)
	}
	if want := (gatepolicy.Rule{ID: "sarif.a-b", Name: "Findings from a-b", State: gatepolicy.Advisory, Producer: gatepolicy.Sarif}); !reflect.DeepEqual(first.Rules[len(a.boot.Rules)], want) {
		t.Fatalf("the added rule: %+v, want %+v", first.Rules[len(a.boot.Rules)], want)
	}
}

// A tool added with a grace is in the snapshot at once, off, and its state bites once the grace
// elapses: each moves the hash and the version once.
func TestAToolAddedWithAGraceIsOffUntilTheGraceElapses(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "sarif.semgrep", "blocking", "14 days")
	added, err := a.effectivePolicy(ctx, pool)
	if err != nil || stateOf(added, "sarif.semgrep") != gatepolicy.Off || added.Hash == a.boot.Hash || added.Version != a.boot.Version+1 {
		t.Fatalf("added with a grace: %s %s v%d (%v); boot %s v%d", stateOf(added, "sarif.semgrep"), added.Hash, added.Version, err, a.boot.Hash, a.boot.Version)
	}
	if _, err := pool.Exec(ctx, "UPDATE policy_edits SET effective_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	bit, err := a.effectivePolicy(ctx, pool)
	if err != nil || stateOf(bit, "sarif.semgrep") != gatepolicy.Blocking || bit.Hash == added.Hash || bit.Version != a.boot.Version+2 {
		t.Fatalf("the grace elapsed: %s %s v%d (%v)", stateOf(bit, "sarif.semgrep"), bit.Hash, bit.Version, err)
	}
}

// A tool's run and settings are in the composed snapshot once written, each named only when set, and
// every scanner edit moves the version once: back at not run with no settings the rule is as it was
// added, hash and all. The table refuses a rule Canyon does not run and settings that are not an object.
func TestAToolsRunAndSettingsAreComposedIntoTheSnapshot(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	edit(t, ctx, pool, "sarif.checkov", "advisory", "0")
	compose := func(run bool, settings string) gatepolicy.Snapshot {
		t.Helper()
		if _, err := pool.Exec(ctx, "INSERT INTO scanner_edits (rule, run, settings) VALUES ('sarif.checkov', $1, $2)", run, settings); err != nil {
			t.Fatal(err)
		}
		s, err := a.effectivePolicy(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	added, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	checkov := slices.IndexFunc(added.Rules, func(r gatepolicy.Rule) bool { return r.ID == "sarif.checkov" })
	got := compose(true, `{"frameworks": ["terraform"], "skip_checks": ["CKV_AWS_20"]}`)
	if r := got.Rules[checkov]; !r.Run || !reflect.DeepEqual(r.Settings, gatepolicy.Settings{Frameworks: []string{"terraform"}, SkipChecks: []string{"CKV_AWS_20"}}) || got.Hash == added.Hash || got.Version != added.Version+1 {
		t.Fatalf("composed: %+v %s v%d; added %s v%d", r, got.Hash, got.Version, added.Hash, added.Version)
	}
	got = compose(false, `{}`)
	if !reflect.DeepEqual(got.Rules[checkov], added.Rules[checkov]) || got.Hash != added.Hash || got.Version != added.Version+2 {
		t.Fatalf("back at none: %+v %s v%d; added %s v%d", got.Rules[checkov], got.Hash, got.Version, added.Hash, added.Version)
	}
	for _, row := range []string{"('sarif.semgrep', true, '{}')", "('sarif.trivy', true, '[]')"} {
		if _, err := pool.Exec(ctx, "INSERT INTO scanner_edits (rule, run, settings) VALUES "+row); err == nil {
			t.Errorf("%s: inserted", row)
		}
	}
}

// The org's approved stacks are server state composed from its preferences row (contracts/snapshot.md),
// so setting them reaches the snapshot and moves its hash: verdicts cached against the old one go.
func TestTheOrgsApprovedStacksReachTheSnapshot(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	if _, err := pool.Exec(ctx, `UPDATE org_preferences SET stacks = '[{"language":"TypeScript","frameworks":["React"]}]'`); err != nil {
		t.Fatal(err)
	}
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if want := []gatepolicy.Stack{{Language: "TypeScript", Frameworks: []string{"React"}}}; !reflect.DeepEqual(got.Stacks, want) || got.Hash == a.boot.Hash {
		t.Fatalf("stacks %+v, hash %s (boot %s)", got.Stacks, got.Hash, a.boot.Hash)
	}
}

// The org's default design system reaches the snapshot as design.org-system reads it (contracts/snapshot.md):
// name, namespace and URLs, never the row's id, so a review checks against it and a change of default
// moves the hash; the boot's capabilities are left as they were.
func TestTheOrgsDefaultDesignSystemReachesTheSnapshot(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	if _, err := pool.Exec(ctx, `UPDATE org_preferences SET design_system_id = 'ds_0001'`); err != nil {
		t.Fatal(err)
	}
	got, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	want := &gatepolicy.DesignSystem{Name: "shadcn/ui", Namespace: "@shadcn/ui", RegistryURL: "https://ui.shadcn.com/r"}
	if ds := got.Capabilities["design_systems"]; !ds.Enabled || !reflect.DeepEqual(ds.OrgDefault, want) || got.Hash == a.boot.Hash {
		t.Fatalf("design_systems %+v, hash %s (boot %s)", ds, got.Hash, a.boot.Hash)
	}
	if a.boot.Capabilities["design_systems"].OrgDefault != nil {
		t.Fatal("composing wrote the org default into the boot snapshot")
	}
}

// editProduction appends an edit naming the gate before production alone, as editSetAt does a state; the
// state it writes beside it is not read, as it names no state.
func editProduction(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rule, production, effectiveIn, setIn string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "INSERT INTO policy_edits (rule, state, names_state, production, version, grace_days, effective_at, set_by_kind, set_by_subject, set_at) VALUES ($1, 'off', false, $2, (SELECT COALESCE(MAX(version), 0) + 1 FROM policy_edits), GREATEST(extract(day FROM $3::interval), 0), now() + $3::interval, 'person', 'dana@meridian.example', now() + $4::interval)", rule, production, effectiveIn, setIn); err != nil {
		t.Fatal(err)
	}
}

// Each gate is its newest edit naming it (contracts/snapshot.md, Two gates): an edit naming the gate
// before production places the rule there, moving the hash and the version once, and leaves its state
// before preview as it was; off takes it out, and the snapshot hashes as it did before; an edit naming
// one gate never withdraws a change scheduled in the other, which GET /policies still shows.
func TestPolicyEachGateIsItsNewestEditNamingIt(t *testing.T) {
	pool, ctx, a := withPolicyEdits(t)
	rule := func(s gatepolicy.Snapshot) gatepolicy.Rule {
		return s.Rules[slices.IndexFunc(s.Rules, func(r gatepolicy.Rule) bool { return r.ID == "deps.known-vuln" })]
	}
	editProduction(t, ctx, pool, "deps.known-vuln", "blocking", "0", "0")
	placed, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if r := rule(placed); r.State != gatepolicy.Advisory || r.Production != gatepolicy.Blocking || placed.Hash == a.boot.Hash || placed.Version != a.boot.Version+1 {
		t.Fatalf("placed: %+v %s v%d", r, placed.Hash, placed.Version)
	}
	editSetAt(t, ctx, pool, "deps.known-vuln", "blocking", "14 days", "1 second")
	editProduction(t, ctx, pool, "deps.known-vuln", "off", "0", "2 seconds")
	out, err := a.effectivePolicy(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if r := rule(out); r.State != gatepolicy.Advisory || r.Production != "" || out.Hash != a.boot.Hash || out.Version != a.boot.Version+2 {
		t.Fatalf("taken out again: %+v %s v%d, boot %s v%d", r, out.Hash, out.Version, a.boot.Hash, a.boot.Version)
	}
	edits, err := newestEdits(ctx, pool, ruleIDs(a.boot))
	if err != nil {
		t.Fatal(err)
	}
	e := edits["deps.known-vuln"]
	if e.newest.Production != gatepolicy.Off || e.newest.State != "" || !e.preview.Ahead || e.preview.State != gatepolicy.Blocking || e.production.Ahead {
		t.Fatalf("the edits: %+v", e)
	}
	if s, p := pending(e.preview, e.preview.State), pending(e.production, e.production.Production); s == nil || s.State != gatepolicy.Blocking || p != nil {
		t.Fatalf("the change still ahead before preview: %+v, before production: %+v", s, p)
	}
}

// The previous binary shares the database while this one migrates and reads every edit's state, no
// other column, newest in effect per rule (0016's composer): an apply naming the gate before production
// alone writes the rule's state now beside it, so that binary reads it as a change to nothing (0054) —
// unless an edit before preview is still in its grace, which it then supersedes there: the one trade-off.
func TestPolicyProductionEditReadsAsNoChangeToThePreviousBinary(t *testing.T) {
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/deps.known-vuln?apply=true", `{"production":"blocking"}`); code != 200 {
		t.Fatalf("the apply: %d %s", code, lastBody)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT DISTINCT ON (rule) state FROM policy_edits WHERE (grace_days = 0 OR effective_at <= now()) AND rule = 'deps.known-vuln' ORDER BY rule, set_at DESC, id DESC`).Scan(&state); err != nil || state != "advisory" {
		t.Fatalf("the previous binary reads deps.known-vuln %q (%v), want its state now, advisory", state, err)
	}
}
