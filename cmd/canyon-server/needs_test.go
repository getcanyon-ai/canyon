package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getcanyon-ai/canyon/executor"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/runs"
)

// contractSchema is ref, a contract schema or a definition in one ("fleet.schema.json#/definitions/need"), compiled with
// every contract schema as a resource, so nothing is fetched.
func contractSchema(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	paths, err := filepath.Glob("../../contracts/schema/*.schema.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("the contract's schemas: %v", err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // test: the contract's own schemas
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.Compile("https://docs.getcanyon.ai/contracts/schema/" + ref)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// validJSON fails the test unless body, a read's answer, is the contract's.
func validJSON(t *testing.T, s *jsonschema.Schema, body string) {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err == nil {
		err = s.Validate(doc)
	}
	if err != nil {
		t.Fatalf("not the contract's: %v\n%s", err, body)
	}
}

// asContract is v as the wire carries it, validated against fleet.schema.json's definition.
func asContract(t *testing.T, s *jsonschema.Schema, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err == nil {
		err = s.Validate(doc)
	}
	if err != nil {
		t.Fatalf("%s is not the contract's need: %v", raw, err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// Every kind the server makes is the contract's, field for field (contracts/routes.md, *What needs a person*): a
// field renamed, or one a kind needs left out, is a need no screen can draw.
func TestEveryNeedIsTheContracts(t *testing.T) {
	s := contractSchema(t, "fleet.schema.json#/definitions/need")
	at := time.Date(2026, 11, 8, 9, 40, 0, 0, time.UTC)
	admins := []askee{{Display: "Elena Vasquez", As: "admin"}}
	asked := actor{Kind: "agent", Subject: "agent:claude-code/dana", Display: "Dana's agent"}
	withAsk := func(n need) need { n.ask = admins; return n }
	viewer := "viewer"
	for _, n := range []need{
		withAsk(newNeed("held", prodNeedEnv, &at, true, nil)),
		newNeed("down", previewNeedEnv, &at, true, map[string]any{"state": "restarting"}),
		newNeed("refused", prodNeedEnv, &at, true, map[string]any{"destination": "api.stripe.com", "refusals": int64(14), "last_refused_at": stamp(at), "other_calls": int64(0), "held": true}),
		newNeed("refused_code", prodNeedEnv, &at, true, map[string]any{"destination": "1.1.1.1", "port": 443, "why": "address", "calls": int64(3), "last_seen_at": stamp(at)}),
		newNeed("secret", nil, nil, true, map[string]any{"version": "v1.4"}),
		newNeed("approval", prodNeedEnv, &at, true, map[string]any{"version": "v3.2", "asked_by": asked, "code": shown("ABCD2345"), "expires_at": stamp(at)}),
		withAsk(newNeed("blocked", nil, &at, true, map[string]any{"version": "v1.4", "count": 2})),
		withAsk(newNeed("unchecked", nil, &at, true, map[string]any{"version": "v2.2", "count": 1, "checks": []map[string]any{{"tool": "trivy", "reason": hostReasons[0], "fix": "host"}}})),
		withAsk(newNeed("blocked_prod", prodNeedEnv, nil, true, map[string]any{"version": "v1.4", "count": nil})),
		newNeed("will_refuse", prodNeedEnv, nil, true, map[string]any{"version": "v3.3", "blocks": false, "denied": []map[string]any{{"destination": "rates.meridian-partners.com", "port": 8443, "allowable": false}}}),
		newNeed("patch", prodNeedEnv, &at, true, map[string]any{"version": "v1.3", "code": shown("ABCD2345"), "expires_at": stamp(at)}),
		newNeed("access", nil, &at, true, map[string]any{"asked_by": actor{Kind: "person", Subject: "sub-priya", Display: "Priya Raman"}, "email": "priya@meridian.example", "level": &viewer}),
		newNeed("access", nil, &at, true, map[string]any{"asked_by": actor{Kind: "person", Subject: "sub-nora", Display: "Nora"}, "email": "nora@meridian.example", "level": (*string)(nil)}), // asked before the level was recorded
		withAsk(newNeed("unowned", nil, nil, false, nil)),
		newNeed("never_reviewed", nil, &at, true, nil),
		newNeed("recreate", nil, nil, true, map[string]any{"network": "unknown", "via": "off"}),
		newNeed("notify_failing", nil, &at, true, map[string]any{"channel": "Risk team", "channel_id": "risk-team"}),
	} {
		if got := asContract(t, s, n); got["kind"] != n.kind {
			t.Errorf("%s marshals as %v", n.kind, got)
		}
	}
}

// The list's order (contracts/routes.md, *What needs a person*): rank — production before preview where the two
// differ — then since, the oldest first and none last, then kind, then what it names.
func TestNeedsRunInTheListsOrder(t *testing.T) {
	early, late := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	refused := func(env *string, dest string, since time.Time) need {
		n := newNeed("refused", env, &since, true, nil)
		n.subject = dest
		return n
	}
	needs := []need{
		newNeed("unowned", nil, nil, true, nil),
		refused(previewNeedEnv, "hooks.slack.com", early),
		newNeed("never_reviewed", nil, &early, true, nil),
		refused(prodNeedEnv, "b.example", late),
		newNeed("will_refuse", prodNeedEnv, nil, true, map[string]any{"blocks": true}),
		refused(prodNeedEnv, "a.example", late),
		newNeed("held", prodNeedEnv, &late, true, nil),
		newNeed("will_refuse", prodNeedEnv, nil, true, map[string]any{"blocks": false}),
		refused(prodNeedEnv, "c.example", early),
	}
	sortNeeds(needs)
	var got []string
	for _, n := range needs {
		got = append(got, strings.TrimSpace(n.kind+" "+n.subject+" "+map[bool]string{true: "blocks"}[n.fields["blocks"] == true]))
	}
	want := []string{"held", "refused c.example", "refused a.example", "refused b.example", "will_refuse  blocks", "refused hooks.slack.com", "will_refuse",
		"never_reviewed", "unowned"}
	if !slices.Equal(got, want) {
		t.Errorf("the order:\n got %q\nwant %q", got, want)
	}
}

// A check that couldn't finish names its tool and why, and who fixes it: a reason reviewing again fails on is the
// host's, any other a review's. A finding that is not gate.unreadable, or a problem out of its shape, is no
// unchecked: the run reads as blocked, fail closed.
func TestACheckThatCouldNotFinishSaysWhoFixesIt(t *testing.T) {
	stale := openFinding{Rule: "gate.unreadable", Problem: "could not check: trivy: its vulnerability database is over 5 days old"}
	timedOut := openFinding{Rule: "gate.unreadable", Problem: "could not check: opengrep: did not finish in 10 minutes"}
	checks, ok := unchecked([]openFinding{stale, timedOut})
	if !ok || len(checks) != 2 || checks[0]["tool"] != "trivy" || checks[0]["fix"] != "host" || checks[1]["fix"] != "review" || checks[1]["reason"] != "did not finish in 10 minutes" {
		t.Errorf("the two checks: %v %t", checks, ok)
	}
	for name, open := range map[string][]openFinding{
		"a finding in the code beside it": {stale, {Rule: "secrets.client-bundle", Problem: "a live key"}},
		"a problem out of its shape":      {{Rule: "gate.unreadable", Problem: "trivy failed"}},
		"none":                            nil,
	} {
		if _, ok := unchecked(open); ok {
			t.Errorf("%s reads as unchecked", name)
		}
	}
}

// A version blocked by its secrets alone is the secret need's, never a second line (contracts/routes.md, *What needs a
// person*, blocked): a finding of another rule beside them, or none to read, still reads as blocked.
func TestASecretIsOneNeed(t *testing.T) {
	secret := openFinding{Rule: "secrets.client-bundle.stripe-access-token"}
	if !secretsAlone([]openFinding{secret, {Rule: "secrets.server-env"}}) {
		t.Error("two secrets read as more than the secret need")
	}
	for name, open := range map[string][]openFinding{"a route beside it": {secret, {Rule: "auth.unauthenticated-route"}}, "none": nil} {
		if secretsAlone(open) {
			t.Errorf("%s reads as secrets alone", name)
		}
	}
}

// A recorded call no allowlist lets through says why: a dotted IPv4 address, or a port other than 80 and 443; a
// name on 443, and invalid, which records keep for anything else, are no allowlist's to say.
func TestACallNoAllowlistLetsThroughSaysWhy(t *testing.T) {
	for _, c := range []struct {
		dest string
		port int
		want string
	}{{"1.1.1.1", 443, "address"}, {"10.0.0.1", 8443, "address"}, {"example.com", 8443, "port"}, {"example.com", 443, ""}, {"example.com", 80, ""}, {"invalid", 443, ""}} {
		if got := codeRefusal(c.dest, c.port); got != c.want {
			t.Errorf("%s:%d: %q, want %q", c.dest, c.port, got, c.want)
		}
	}
}

// The receipt's two kinds, from its own input (contracts/routes.md, *What needs a person*): blocked_prod while
// prod.clean-gate blocks a review clean before preview for findings placed before production, its count null where a
// placed row has none to read; will_refuse while prod.egress-allowed is on, ranked by its state, each destination
// marked whether an Allow lets it through. Neither while its rule is off, nor on a rollback review.
func TestTheReceiptsTwoNeeds(t *testing.T) {
	rules := func(gate, egress gatepolicy.State) []gatepolicy.Rule {
		return []gatepolicy.Rule{{ID: "prod.clean-gate", State: gate}, {ID: egressRule, State: egress}}
	}
	placed := []placedRule{{rule: gatepolicy.Rule{ID: "sast.sql", Production: gatepolicy.Blocking}, journaled: true, unapproved: 2}}
	run := &runFacts{id: "gr_8f2a", state: "clean", pkg: pkgRef{id: "pkg_9e40", label: "v1.4"}}
	egress := &egressRead{denied: []string{"1.1.1.1", "api.stripe.com", "rates.example:8443"},
		refused: []egressSeen{{destination: "1.1.1.1", port: 443}, {destination: "api.stripe.com", port: 443}, {destination: "rates.example", port: 8443}}}
	preview := &pkgRef{id: "pkg_9e40", label: "v1.4"}
	said := func(in receiptInput) string {
		var out []string
		for _, n := range receiptNeeds(in, nil) {
			raw, _ := json.Marshal(n.fields)
			out = append(out, n.kind+" "+string(raw)+" "+map[bool]string{true: "rank 6", false: "rank 7"}[n.rank == 6])
		}
		return strings.Join(out, "\n")
	}
	for _, c := range []struct {
		name string
		in   receiptInput
		want string
	}{
		{"both", receiptInput{rules: rules(gatepolicy.Blocking, gatepolicy.Advisory), run: run, placed: placed, egress: egress, preview: preview},
			`blocked_prod {"count":2,"version":"v1.4"} rank 6` + "\n" +
				`will_refuse {"blocks":false,"denied":[{"allowable":false,"destination":"1.1.1.1","port":443},{"allowable":true,"destination":"api.stripe.com","port":443},{"allowable":false,"destination":"rates.example","port":8443}],"version":"v1.4"} rank 7`},
		{"a placed row with no count", receiptInput{rules: rules(gatepolicy.Blocking, gatepolicy.Off), run: run, placed: []placedRule{{rule: placed[0].rule, unread: "skipped"}}},
			`blocked_prod {"count":null,"version":"v1.4"} rank 6`},
		{"what production will refuse, while the rule blocks", receiptInput{rules: rules(gatepolicy.Advisory, gatepolicy.Blocking), run: run, placed: placed, egress: egress, preview: preview},
			`will_refuse {"blocks":true,"denied":[{"allowable":false,"destination":"1.1.1.1","port":443},{"allowable":true,"destination":"api.stripe.com","port":443},{"allowable":false,"destination":"rates.example","port":8443}],"version":"v1.4"} rank 6`},
		{"a review blocked before preview", receiptInput{rules: rules(gatepolicy.Blocking, gatepolicy.Off), run: run, placed: placed, unapproved: 1}, ""},
		{"a rollback review", receiptInput{rules: rules(gatepolicy.Blocking, gatepolicy.Off), run: &runFacts{state: "clean", rollback: true}, placed: placed}, ""},
		{"nothing placed open", receiptInput{rules: rules(gatepolicy.Blocking, gatepolicy.Advisory), run: run, egress: &egressRead{}, preview: preview}, ""},
	} {
		if got := said(c.in); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// Past 400 environments the read asks the executor in calls of 400 at once, under its one bound (contracts/routes.md,
// envs.<env>.state; #2724): two calls that each take most of the bound both answer, where one after the other the
// second would run out of time and read its environments unknown, a down need hidden on every poll.
func TestTheExecutorIsAskedAtOnceUnderOneBound(t *testing.T) {
	fake := executortest.New(t)
	fake.SlowRunning.Store(int64(500 * time.Millisecond))
	was := runningWithin
	runningWithin = 800 * time.Millisecond
	t.Cleanup(func() { runningWithin = was })
	a := &app{pipe: &runs.Pipeline{Executor: &executor.Client{Base: fake.URL}}, log: log.New(io.Discard, "", 0)}
	envs := map[string]map[string]any{}
	for i := range 250 { // 500 environments: two calls
		envs[fmt.Sprintf("app-%04d", i)] = map[string]any{"preview": map[string]any{"run_by": "canyon"}, "prod": map[string]any{"run_by": "canyon"}}
	}
	at := time.Now()
	a.sayRunning(context.Background(), envs, false, false)
	unknown := 0
	for _, project := range envs {
		for _, e := range project {
			if e.(map[string]any)["state"] == "unknown" {
				unknown++
			}
		}
	}
	if unknown != 0 || fake.RunningOfs.Load() != 2 {
		t.Fatalf("%d of 500 environments read unknown after %d calls in %v: the calls ran one after another", unknown, fake.RunningOfs.Load(), time.Since(at))
	}
}

// What needs a person says of a hold exactly what the lift does (liftsOffTeam; design/PERMISSIONS.md, off the app's
// team): a responder holding a viewer grant lifts it; one with an owner or contributor grant now, or on the team the
// hold recorded, never does; nor one whose groups weren't read whole while a team group exists.
func TestAHoldIsTheResponderOffItsTeams(t *testing.T) {
	responder := func(subject string, groupsRead bool) context.Context {
		ctx := context.WithValue(context.Background(), actorKey{}, actor{Kind: "person", Subject: subject, Display: subject})
		ctx = context.WithValue(ctx, roleKey{}, "responder")
		ctx = context.WithValue(ctx, emailKey{}, subject+"@meridian.example")
		return context.WithValue(ctx, readKey{}, groupsRead)
	}
	record := &runs.Hold{TeamEmails: []string{"olive@meridian.example"}, TeamSubjects: []string{"sub-carl"}, TeamGroups: []string{}}
	for _, c := range []struct {
		name    string
		ctx     context.Context
		level   string
		grouped bool
		lifts   bool
	}{
		{"a responder with a viewer grant", responder("sub-rhea", true), "viewer", false, true},
		{"a responder with no grant", responder("sub-rhea", true), "none", false, true},
		{"a responder who contributes now", responder("sub-rhea", true), "contributor", false, false},
		{"a responder who contributed when it was held", responder("sub-carl", true), "none", false, false},
		{"a responder on the record by address", responder("olive", true), "viewer", false, false},
		{"a responder whose groups weren't read whole, beside a team group", responder("sub-rhea", false), "none", true, false},
	} {
		if got := liftsOffTeam(c.ctx, c.level, record, c.grouped); got != c.lifts {
			t.Errorf("%s: lifts %t, want %t", c.name, got, c.lifts)
		}
	}
}

// The review's three survivors (#2747): answered more than Canyon reads is the host's to fix; a custom rule's could-not-
// check is no unchecked, whatever its problem says, as only gate.unreadable is; and invalid, which records keep for an
// address no host name is, is never one an Allow lets through.
func TestTheReviewsThreeSurvivors(t *testing.T) {
	if checks, ok := unchecked([]openFinding{{Rule: "gate.unreadable", Problem: "could not check: opengrep: answered more than Canyon reads"}}); !ok || checks[0]["fix"] != "host" {
		t.Errorf("over_cap: %v %t, want the host's fix", checks, ok)
	}
	if _, ok := unchecked([]openFinding{{Rule: "custom.pii", Problem: "could not check: custom.pii: the model didn't answer"}}); ok {
		t.Error("a custom rule's could-not-check read as unchecked")
	}
	in := receiptInput{rules: []gatepolicy.Rule{{ID: egressRule, State: gatepolicy.Blocking}}, preview: &pkgRef{id: "pkg_9e40", label: "v1.4"},
		egress: &egressRead{denied: []string{egressInvalid}, refused: []egressSeen{{destination: "invalid", port: 443}}}}
	needs := receiptNeeds(in, nil)
	if len(needs) != 1 || needs[0].fields["denied"].([]map[string]any)[0]["allowable"] != false {
		t.Errorf("invalid in will_refuse: %v", needs)
	}
}

// A blocking prod.egress-allowed that could not check is a need of production on the status read (contracts/routes.md,
// *What needs a person*, unchecked), its fix the admin's where a setting of theirs is the cause; advisory, or reading
// what it denies, it is none of these.
func TestAGoLiveRuleThatCouldNotCheckIsANeed(t *testing.T) {
	preview := &pkgRef{id: "pkg_9e40", label: "v1.4"}
	rules := func(state gatepolicy.State) []gatepolicy.Rule {
		return []gatepolicy.Rule{{ID: egressRule, State: state}}
	}
	said := func(in receiptInput) string {
		var out []string
		for _, n := range receiptNeeds(in, nil) {
			raw, _ := json.Marshal(n.fields)
			out = append(out, fmt.Sprintf("%s %s %s", n.kind, *n.env, raw))
		}
		return strings.Join(out, "; ")
	}
	for _, c := range []struct {
		name string
		in   receiptInput
		want string
	}{
		{"its record is short", receiptInput{rules: rules(gatepolicy.Blocking), preview: preview, egress: &egressRead{unread: egressNoRecord}},
			`unchecked prod {"checks":[{"fix":"review","reason":"Canyon doesn't have a full record of preview's connections. Deploy to preview again. If this persists, ask an admin.","tool":"prod.egress-allowed"}],"count":1,"version":"v1.4"}`},
		{"Outbound is Off", receiptInput{rules: rules(gatepolicy.Blocking), preview: preview, egress: &egressRead{unread: egressDirect}},
			`unchecked prod {"checks":[{"fix":"host","reason":"Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound.","tool":"prod.egress-allowed"}],"count":1,"version":"v1.4"}`},
		{"advisory", receiptInput{rules: rules(gatepolicy.Advisory), preview: preview, egress: &egressRead{unread: egressNoRecord}}, ""},
		{"the org's platform runs preview", receiptInput{rules: rules(gatepolicy.Blocking), preview: preview, egress: &egressRead{orgRuns: true}}, ""},
	} {
		if got := said(c.in); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	n := receiptNeeds(receiptInput{rules: rules(gatepolicy.Blocking), preview: preview, egress: &egressRead{unread: egressCantRead}}, nil)
	if len(n) != 1 {
		t.Fatalf("could not read: %d needs, want the one", len(n))
	}
	asContract(t, contractSchema(t, "fleet.schema.json#/definitions/need"), n[0])
}

// The read asks the executor in a fixed order, by app then environment, so a call that fails reads the same
// environments unknown on every poll and a row never flaps between two (contracts/routes.md, envs.<env>.state).
func TestTheExecutorIsAskedInAFixedOrder(t *testing.T) {
	fake := executortest.New(t)
	a := &app{pipe: &runs.Pipeline{Executor: &executor.Client{Base: fake.URL}}, log: log.New(io.Discard, "", 0)}
	envs := map[string]map[string]any{}
	var want []executor.Container
	for i := range 250 {
		id := fmt.Sprintf("app-%04d", i)
		envs[id] = map[string]any{"preview": map[string]any{"run_by": "canyon"}, "prod": map[string]any{"run_by": "canyon"}}
		want = append(want, executor.Container{App: id, Target: "preview"}, executor.Container{App: id, Target: "prod"})
	}
	a.sayRunning(context.Background(), envs, false, false)
	asked := slices.Clone(fake.RunningAsked)
	slices.SortFunc(asked, func(x, y []executor.Container) int { return strings.Compare(x[0].App, y[0].App) }) // the calls go at once: their order is the network's
	if len(asked) != 2 || !slices.Equal(asked[0], want[:executor.MaxRunningOf]) || !slices.Equal(asked[1], want[executor.MaxRunningOf:]) {
		t.Fatalf("the calls asked %d environments in an order of their own, first %v", len(slices.Concat(asked...)), asked[0][:4])
	}
}
