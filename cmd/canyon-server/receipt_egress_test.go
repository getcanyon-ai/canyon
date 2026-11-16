package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

var (
	goLive     = time.Date(2026, 11, 6, 12, 0, 20, 0, time.UTC)
	allowAll   = prodPolicy{mode: "observe"}
	stripeOnly = prodPolicy{mode: "allowlist", allowed: []string{"api.stripe.com"}}
)

// healthy are the facts of a preview that first went live at goLive, through Canyon, read at 12:00:40 with the
// receiver up, nothing recorded and production allowing nothing: each case changes what it is about.
func healthy() egressFacts {
	now, via, floor := goLive.Add(20*time.Second), "canyon", "enforced"
	return egressFacts{now: now, wentLive: goLive, firstLive: &goLive, configAt: &goLive, runBy: "canyon", setting: "canyon", via: &via, floor: &floor, heartbeat: &now, prod: prodPolicy{mode: "allowlist"}}
}

// A tunnel opened at 12:00:03, before go-live at 12:00:20, is next seen around 12:01:03: its row says 12:00:03 when a
// promotion reads it at 12:00:40, and that promotion must find it (decided 2026-11-06).
func TestAnOpenTunnelFromBeforeGoLiveIsRead(t *testing.T) {
	f := healthy()
	f.rows = []egressSeen{{"api.openai.com", 443, time.Date(2026, 11, 6, 12, 0, 3, 0, time.UTC)}}
	if got := f.read(); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("a tunnel open across go-live read %+v, want its destination named", got)
	}
}

// The read starts exactly the margin before go-live: a row last seen there counts, one a moment older doesn't, and
// the margin is never negative, so nothing the check sees is looser than go-live itself.
func TestTheReadStartsTheMarginBeforeGoLive(t *testing.T) {
	for _, c := range []struct {
		name string
		seen time.Time
		held bool
	}{
		{"at go-live", goLive, true},
		{"after go-live", goLive.Add(time.Minute), true},
		{"a second inside the margin", goLive.Add(-egressSeenMargin + time.Second), true},
		{"at the margin", goLive.Add(-egressSeenMargin), true},
		{"just past the margin", goLive.Add(-egressSeenMargin - time.Nanosecond), false},
		{"an hour before", goLive.Add(-time.Hour), false},
	} {
		f := healthy()
		f.rows = []egressSeen{{"api.openai.com", 443, c.seen}}
		if got := len(f.read().denied) > 0; got != c.held {
			t.Errorf("a row last seen %s: held %v, want %v", c.name, got, c.held)
		}
	}
	if egressReadFrom(goLive) != goLive.Add(-90*time.Second) {
		t.Errorf("the read starts at %v, want 90 s before go-live", egressReadFrom(goLive))
	}
}

// Deploying the same image again moves deployed_at and clears nothing: the row is the image's, and the read starts at
// its first go-live. A new image starts its own. What a redeploy is the remedy for still reads from the running
// deployment: the records' keep clears, and the gaps' window is the loader's, from wentLive.
func TestARedeployOfTheSameImageKeepsItsRows(t *testing.T) {
	first, later, long := goLive.Add(-3*time.Hour), goLive.Add(time.Hour), goLive.Add(-40*24*time.Hour)
	f := healthy()
	f.rows = []egressSeen{{"api.openai.com", 443, goLive.Add(-2 * time.Hour)}}
	f.firstLive = &first
	if got := f.read(); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("the same image deployed again read %+v, want its row", got)
	}
	f.firstLive = &goLive
	if got := f.read(); len(got.denied) != 0 || got.unread != "" {
		t.Errorf("a new image read %+v, want nothing of the old one's", got)
	}
	f = healthy()
	f.rows, f.firstLive = []egressSeen{{"api.openai.com", 443, goLive}}, &later
	if got := f.read(); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("a first go-live after the deployment's narrowed the read: %+v", got)
	}
	f = healthy()
	f.firstLive = &long
	if got := f.read(); got.unread != "" {
		t.Errorf("a redeploy is the remedy for the records' keep, yet it read %+v", got)
	}
}

// V1 connected to a destination, a person removed the call and deployed V2, and V1's last call landed 5 s before V2 went
// live: it sits in the margin, which can't tell it from a tunnel V2 holds, so it counts on V2's first deployment. It
// must not hold V2 for good: deploying again goes past it, or a person would allow what the app no longer uses. V2's own
// call, past its first go-live, holds through the same redeploy.
func TestACallInTheMarginCountsUntilADeployGoesPastIt(t *testing.T) {
	again, soon := goLive.Add(time.Hour), goLive.Add(30*time.Second)
	before := []egressSeen{{"api.openai.com", 443, goLive.Add(-5 * time.Second)}}
	own := []egressSeen{{"api.openai.com", 443, goLive.Add(time.Minute)}}
	at := func(wentLive time.Time, rows []egressSeen) egressRead {
		f := healthy()
		beat := wentLive.Add(time.Second)
		f.rows, f.wentLive, f.configAt, f.now, f.heartbeat = rows, wentLive, &wentLive, beat, &beat
		return f.read()
	}
	if got := at(goLive, before); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("on its first deployment a call in the margin read %+v, want it held", got)
	}
	if got := at(soon, before); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("a redeploy that doesn't go past it read %+v, want it held", got)
	}
	if got := at(again, before); len(got.denied) != 0 {
		t.Errorf("deployed again an hour on, the outgoing version's last call read %+v, want it gone", got)
	}
	if got := at(again, own); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("deployed again, the image's own call read %+v, want it held", got)
	}
}

// A recreation writes run_config_at and leaves deployed_at, so preview went live under another spec: what it reached
// directly then is on no row, and no row is no evidence. A deploy writes both with one now(), which reads as through.
func TestARecreatedPreviewIsNotThroughUntilItDeploysAgain(t *testing.T) {
	recreated, before := goLive.Add(40*time.Minute), goLive.Add(-time.Minute)
	for _, c := range []struct {
		name     string
		configAt *time.Time
		want     string
	}{
		{"written with the deploy", &goLive, ""},
		{"written before it", &before, ""},
		{"recreated since", &recreated, egressNoRecord},
		{"a moment after", ptrTo(goLive.Add(time.Nanosecond)), egressNoRecord},
		{"a row from before the column", nil, egressNoRecord},
	} {
		f := healthy()
		f.configAt = c.configAt
		if got := f.read(); got.unread != c.want {
			t.Errorf("%s: %+v, want %q", c.name, got, c.want)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }

// The margin covers what it is there for: the bootstrap's periodic interval, read from the file Envoy runs, plus the
// flush that writes the entry, or a tunnel across go-live is back to reading as unseen.
func TestTheMarginCoversATunnelsLag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "egress", "envoy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var intervals []string
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, e := range n {
				if s, ok := e.(string); ok && k == "access_log_flush_interval" {
					intervals = append(intervals, s)
				}
				walk(e)
			}
		case []any:
			for _, e := range n {
				walk(e)
			}
		}
	}
	walk(doc)
	if len(intervals) != 1 {
		t.Fatalf("the bootstrap names %d access_log_flush_interval, want one", len(intervals))
	}
	periodic, err := time.ParseDuration(intervals[0])
	if err != nil {
		t.Fatal(err)
	}
	if periodic != egressPeriodic {
		t.Errorf("the bootstrap logs a tunnel every %v, egressPeriodic says %v: the margin no longer covers its lag", periodic, egressPeriodic)
	}
	if egressSeenMargin < egressPeriodic+egressFlushEvery {
		t.Errorf("the margin is %v, under a periodic entry and a flush, %v", egressSeenMargin, egressPeriodic+egressFlushEvery)
	}
}

// What production allows is what its check would answer, the check's own Canonical and Refusal: every mode refuses an
// address and a port, so observe allows neither; a platform destination passes any mode; a wildcard is any depth below
// its name and never the name; and a mode that is neither allows nothing, entries or no.
func TestProductionAllowsWhatItsCheckWouldLetThrough(t *testing.T) {
	entries := prodPolicy{mode: "allowlist", allowed: []string{"api.stripe.com", "*.slack.com"}, platform: []string{"login.meridian.example"}}
	for _, c := range []struct {
		name   string
		policy prodPolicy
		dest   string
		port   int
		want   bool
	}{
		{"observe, a name", allowAll, "api.openai.com", 443, true},
		{"observe, port 80", allowAll, "api.openai.com", 80, true},
		{"observe, an IP address", allowAll, "10.0.4.7", 443, false},
		{"observe, the invalid row", allowAll, "invalid", 443, false},
		{"observe, another port", allowAll, "api.openai.com", 8443, false},
		{"allowlist, listed", entries, "api.stripe.com", 443, true},
		{"allowlist, listed on 80", entries, "api.stripe.com", 80, true},
		{"allowlist, listed on another port", entries, "api.stripe.com", 8443, false},
		{"allowlist, not listed", entries, "api.openai.com", 443, false},
		{"allowlist, under a wildcard", entries, "a.b.slack.com", 443, true},
		{"allowlist, a wildcard's own name", entries, "slack.com", 443, false},
		{"allowlist, a platform destination", entries, "login.meridian.example", 443, true},
		{"a platform destination on another port", entries, "login.meridian.example", 8443, false},
		{"a mode that is neither", prodPolicy{mode: "strict", allowed: []string{"api.stripe.com"}}, "api.stripe.com", 443, false},
		{"no mode at all", prodPolicy{allowed: []string{"api.stripe.com"}}, "api.stripe.com", 443, false},
		{"a mode that is neither, a platform destination", prodPolicy{mode: "strict", platform: []string{"login.meridian.example"}}, "login.meridian.example", 443, true},
	} {
		if got := c.policy.allows(c.dest, c.port); got != c.want {
			t.Errorf("%s: %s:%d allowed %v, want %v", c.name, c.dest, c.port, got, c.want)
		}
	}
}

// A row a person can't allow is named as it stands: never a port-less name an allowlist entry would seem to fix.
func TestTheReadNamesEachDestinationOnce(t *testing.T) {
	f := healthy()
	f.prod = allowAll
	f.rows = []egressSeen{{"api.openai.com", 8443, goLive}, {"invalid", 443, goLive}, {"10.0.4.7", 443, goLive}, {"api.stripe.com", 443, goLive}}
	if got, want := f.read().denied, []string{"10.0.4.7", "an invalid address", "api.openai.com:8443"}; !slices.Equal(got, want) {
		t.Errorf("under observe named %q, want %q", got, want)
	}
	f.prod = stripeOnly
	f.rows = []egressSeen{{"hooks.slack.com", 443, goLive}, {"api.openai.com", 80, goLive}, {"api.openai.com", 443, goLive}, {"api.stripe.com", 443, goLive}}
	if got, want := f.read().denied, []string{"api.openai.com", "hooks.slack.com"}; !slices.Equal(got, want) {
		t.Errorf("under allowlist named %q, want %q", got, want)
	}
}

// The gaps are decided in one place: one that names no app holds, preview's own over_cap holds, and production's
// doesn't. Preview's own is named first: it is the app's, and a redeploy alone may not clear it.
func TestTheGapsDecideInOnePlace(t *testing.T) {
	app, preview, prod := "claims-portal", "preview", "prod"
	for _, c := range []struct {
		name string
		gaps []windowGap
		want string
	}{
		{"none", nil, ""},
		{"a gap that names no app", []windowGap{{}}, egressNoRecord},
		{"preview's over_cap", []windowGap{{&app, &preview}}, egressOverCap},
		{"production's over_cap", []windowGap{{&app, &prod}}, ""},
		{"both, the app's first", []windowGap{{}, {&app, &preview}}, egressOverCap},
		{"production's and one that names no app", []windowGap{{&app, &prod}, {}}, egressNoRecord},
	} {
		if got := egressGapHolds(c.gaps); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The row goes neutral only when both reads say org: the setting decides where a promotion goes, so preview's row alone
// can't. A switch back leaves that row org while the image starts behind production's allowlist, and a value that reads
// as neither is never neutral. Whatever preview's rows would say: an app the org runs has none of Canyon's to read.
func TestOnlyBothReadsSayingOrgIsNeutral(t *testing.T) {
	for _, c := range []struct {
		runBy, setting string
		neutral        bool
		unread         string
	}{
		{"org", "org", true, ""},
		{"org", "canyon", false, egressNoRecord},
		{"canyon", "canyon", false, ""},
		{"canyon", "org", false, ""}, // preview's records still read: stricter, never looser
		{"org", "", false, egressCantRead},
		{"", "org", false, egressCantRead},
		{"", "", false, egressCantRead},
		{"org", "platform", false, egressCantRead},
		{"platform", "org", false, egressCantRead},
		{"canyon", "", false, egressCantRead},
		{"canyon", "platform", false, egressCantRead},
	} {
		f := healthy()
		f.runBy, f.setting = c.runBy, c.setting
		if got := f.read(); got.orgRuns != c.neutral || got.unread != c.unread {
			t.Errorf("preview %q, the setting %q: %+v, want neutral %v, unread %q", c.runBy, c.setting, got, c.neutral, c.unread)
		}
	}
	f := healthy()
	f.runBy, f.setting = "org", "org"
	f.rows, f.gaps, f.via, f.heartbeat = []egressSeen{{"api.openai.com", 443, goLive}}, []windowGap{{}}, nil, nil
	if got := f.read(); !got.orgRuns || len(got.denied) != 0 {
		t.Errorf("the org's on both sides over what else reads: %+v", got)
	}
}

// Whatever can't be read never passes, each in the words that say what a person can do, and a destination production
// refuses is named over any of them but a preview Canyon can't see at all.
func TestWhatTheRuleCannotReadNeverPasses(t *testing.T) {
	off, open := "off", "not_enforced"
	old, stale := goLive.Add(-31*24*time.Hour), goLive.Add(20*time.Second-egressDownAfter-time.Second)
	fresh, ahead, far := goLive.Add(20*time.Second-egressDownAfter), goLive.Add(20*time.Second+egressDownAfter), goLive.Add(21*time.Second+egressDownAfter)
	slight := goLive.Add(20*time.Second + 50*time.Millisecond)
	for _, c := range []struct {
		name string
		edit func(f *egressFacts)
		want string
	}{
		{"nothing recorded", func(*egressFacts) {}, ""},
		{"the org runs preview, a promotion is Canyon's", func(f *egressFacts) { f.runBy = "org" }, egressNoRecord},
		{"a run_by that reads as nothing", func(f *egressFacts) { f.runBy = "" }, egressCantRead},
		{"a run_by this build does not know", func(f *egressFacts) { f.runBy = "platform" }, egressCantRead},
		{"a setting that reads as nothing", func(f *egressFacts) { f.setting = "" }, egressCantRead},
		{"no spec recorded", func(f *egressFacts) { f.via = nil }, egressNoRecord},
		{"under via off", func(f *egressFacts) { f.via = &off }, egressNoRecord},
		// The fix the Outbound card gives, never a deploy to preview, which goes around Canyon again (#2723).
		{"on an open network", func(f *egressFacts) { f.floor = &open }, "could not check: preview may skip Canyon until an admin recreates its network in Settings › Outbound."},
		{"no floor at all: the host's via is off", func(f *egressFacts) { f.floor = nil }, "could not check: Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound."},
		{"via off and no first go-live", func(f *egressFacts) { f.floor, f.firstLive = nil, nil }, "could not check: Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound."},
		{"past the records' keep", func(f *egressFacts) {
			f.wentLive, f.firstLive, f.configAt, f.now, f.heartbeat = old, &old, &old, goLive, &goLive
		}, egressNoRecord},
		{"no first go-live", func(f *egressFacts) { f.firstLive = nil }, egressNoRecord},
		{"no heartbeat", func(f *egressFacts) { f.heartbeat = nil }, egressCantRead},
		{"a stale heartbeat", func(f *egressFacts) { f.heartbeat = &stale }, egressCantRead},
		{"a server's clock slightly ahead of the database's", func(f *egressFacts) { f.heartbeat = &slight }, ""},
		{"a heartbeat ahead by the bound", func(f *egressFacts) { f.heartbeat = &ahead }, ""},
		{"a heartbeat ahead past the bound", func(f *egressFacts) { f.heartbeat = &far }, egressCantRead},
		{"a heartbeat at the bound", func(f *egressFacts) { f.heartbeat = &fresh }, ""},
		{"a gap", func(f *egressFacts) { f.gaps = []windowGap{{}} }, egressNoRecord},
	} {
		f := healthy()
		c.edit(&f)
		if got := f.read(); got.unread != c.want || len(got.denied) != 0 || got.orgRuns {
			t.Errorf("%s: %+v, want %q", c.name, got, c.want)
		}
	}
	// Under Off a null floor is an open network or one Canyon can't tell, a recreation under way or failed: the line names the
	// setting, as the Outbound card does, never the network (#2774)
	direct := healthy()
	direct.floor = nil
	if got := direct.read().unread; got == "" || strings.Contains(got, "directly") {
		t.Errorf("a null floor under Off says %q, want the setting, never that apps call out directly", got)
	}
	// On Kubernetes each env's own pod is floored by its spec: the app's floor, which production's off can set, is no read of preview's
	k := healthy()
	k.kube, k.floor = true, &open
	if got := k.read(); got.unread != "" {
		t.Errorf("on Kubernetes under canyon, floor not_enforced by production: %+v", got)
	}
	k.via = &off
	if got := k.read(); got.unread != egressNoRecord {
		t.Errorf("on Kubernetes under off: %+v", got)
	}
	k.floor = nil // the host's via is off now: a deploy goes around Canyon again
	if got := k.read(); got.unread != egressDirect {
		t.Errorf("on Kubernetes with the host's via off: %+v", got)
	}
	denied := healthy()
	denied.rows = []egressSeen{{"api.openai.com", 443, goLive}}
	denied.gaps, denied.heartbeat = []windowGap{{}}, nil
	if got := denied.read(); !slices.Equal(got.denied, []string{"api.openai.com"}) {
		t.Errorf("a refused destination over a gap: %+v", got)
	}
	denied.via = &off
	if got := denied.read(); got.unread != egressNoRecord || len(got.denied) != 0 {
		t.Errorf("rows are no evidence of a preview not through Canyon: %+v", got)
	}
}

// The row in the receipt: off it is absent, and each read says what the rule saw, advisory blocking nothing and
// blocking refusing unless it passed or is neutral. Nothing read never passes.
func TestTheEgressRowOnTheReceipt(t *testing.T) {
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	v14 := pkgRef{id: "pkg_9e40", label: "v1.4"}
	base := receiptInput{org: "Meridian", preview: &v14, owners: []actor{dana}, actor: dana, run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v14, newest: "gr_8f2a"}}
	row := func(state gatepolicy.State, read *egressRead) (receiptRule, []string) {
		t.Helper()
		snap, err := gatepolicy.WithStates(testPolicy(), map[string]gatepolicy.State{egressRule: state})
		if err != nil {
			t.Fatal(err)
		}
		in := base
		in.rules, in.egress = promoteRules(snap), read
		r := evaluate(in)
		i := slices.IndexFunc(r.Rules, func(row receiptRule) bool { return row.ID == egressRule })
		if i < 0 {
			return receiptRule{}, r.BlockedBy
		}
		if i != len(r.Rules)-1 {
			t.Errorf("the row is not last: %s", mustJSON(r))
		}
		return r.Rules[i], r.BlockedBy
	}
	if got, _ := row(gatepolicy.Off, &egressRead{denied: []string{"api.openai.com"}}); got.ID != "" {
		t.Errorf("off, yet on the receipt: %s", mustJSON(got))
	}
	for _, c := range []struct {
		name   string
		read   *egressRead
		passed bool
		why    string
		unset  bool
	}{
		{"nothing read", nil, false, "could not check: Canyon can't read preview's connections right now. Try again in a few minutes. If this persists, ask an admin.", false},
		{"a clean read", &egressRead{}, true, "production allows everything v1.4 has connected to so far", false},
		{"a destination", &egressRead{denied: []string{"api.openai.com"}}, false, "production does not allow api.openai.com", false},
		{"two", &egressRead{denied: []string{"api.openai.com", "hooks.slack.com"}}, false, "production does not allow api.openai.com, hooks.slack.com", false},
		{"no full record", &egressRead{unread: egressNoRecord}, false, "could not check: Canyon doesn't have a full record of preview's connections. Deploy to preview again. If this persists, ask an admin.", false},
		{"over the cap", &egressRead{unread: egressOverCap}, false, "could not check: preview connected to over 500 destinations, more than Canyon tracks. Connect to fewer and deploy to preview. If this persists, ask an admin.", false},
		{"the org's on both sides", &egressRead{orgRuns: true}, false, "not checked: preview runs on your own platform", true},
	} {
		for _, state := range []gatepolicy.State{gatepolicy.Advisory, gatepolicy.Blocking} {
			got, blockedBy := row(state, c.read)
			want := receiptRule{ID: egressRule, Name: "Production allows everything preview connects to", Blocks: state == gatepolicy.Blocking, Passed: c.passed, Why: c.why, Unset: c.unset}
			got.noNames = ""
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s at %s: %s, want %s", c.name, state, mustJSON(got), mustJSON(want))
			}
			if held := slices.Contains(blockedBy, egressRule); held != (state == gatepolicy.Blocking && !c.passed && !c.unset) {
				t.Errorf("%s at %s: blocked by %v", c.name, state, blockedBy)
			}
		}
	}
}

// A rollback reads no preview and is never held by what an unrelated version connects to: blocking, its row blocks
// nothing, and passes unless it lists what a person dismissed that production refuses today, an advisory line.
func TestARollbackIsNeverHeldByTheEgressRule(t *testing.T) {
	v13 := pkgRef{id: "pkg_7c10", label: "v1.3"}
	blocking, err := gatepolicy.WithStates(testPolicy(), map[string]gatepolicy.State{egressRule: gatepolicy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	dana := actor{Kind: "person", Subject: "dana@meridian.example", Display: "Dana Whitfield"}
	in := receiptInput{org: "Meridian", rules: promoteRules(blocking), owners: []actor{dana}, actor: dana, ran: nil,
		run: &runFacts{id: "gr_8f2a", state: "clean", pkg: v13, newest: "gr_8f2a", rollback: true, ships: "claims-portal@sha256:0"}}
	for _, c := range []struct {
		name      string
		dismissed []string
		why       string
	}{
		{"nothing dismissed", nil, "not compared: a rollback puts back what production ran"},
		{"one dismissed", []string{"hooks.slack.com"}, "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card."},
		{"two dismissed", []string{"api.openai.com", "hooks.slack.com"}, "this version may need api.openai.com, hooks.slack.com, which production does not allow. A person can change that on the Outbound card."},
	} {
		in.egress = &egressRead{rollback: true, dismissed: c.dismissed}
		r := evaluate(in)
		i := slices.IndexFunc(r.Rules, func(row receiptRule) bool { return row.ID == egressRule })
		if i < 0 || r.Rules[i].Passed != (len(c.dismissed) == 0) || r.Rules[i].Blocks || r.Rules[i].Unset || r.Rules[i].Why != c.why || slices.Contains(r.BlockedBy, egressRule) || r.Waiting != "" {
			t.Errorf("%s: %s", c.name, mustJSON(r))
		}
	}
}

// A row's why holds at most 200 characters (app-page.schema.json), however many destinations an app connects to and
// however long their names, and a line past that still says how many it left out.
func TestTheRowNeverOutgrowsItsWhy(t *testing.T) {
	var many []string
	for i := range 500 {
		many = append(many, fmt.Sprintf("host-%03d.%s.example", i, strings.Repeat("a", 40)))
	}
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example"
	blocking := gatepolicy.Rule{ID: egressRule, State: gatepolicy.Blocking}
	for _, c := range []struct {
		name string
		read egressRead
		want string
	}{
		{"five hundred names", egressRead{denied: many}, " and 498 more"},
		{"one name past the limit", egressRead{denied: []string{long}}, "production does not allow 1 destination"},
		{"two names past the limit", egressRead{denied: []string{long, long + "x"}}, "production does not allow 2 destinations"},
		{"five hundred dismissed", egressRead{rollback: true, dismissed: many}, " and 499 more, which production does not allow. A person can change that on the Outbound card."},
	} {
		why := egressRow(blocking, &c.read, "v1.4").Why
		if len(why) > egressWhyLimit || !strings.HasSuffix(why, c.want) {
			t.Errorf("%s: %d characters: %s", c.name, len(why), why)
		}
	}
	version := strings.Repeat("v", 64) // the longest label (app-page.schema.json)
	if why := egressRow(blocking, &egressRead{}, version).Why; len(why) > egressWhyLimit {
		t.Errorf("%d characters: %s", len(why), why)
	}
	for _, line := range []string{egressNoRecord, egressCantRead, egressOverCap, egressDirect, egressOpenNetwork} {
		if why := egressRow(blocking, &egressRead{unread: line}, version).Why; len(why) > egressWhyLimit {
			t.Errorf("%d characters: %s", len(why), why)
		}
	}
	for _, row := range []receiptRule{egressRow(blocking, &egressRead{orgRuns: true}, version), egressRow(blocking, &egressRead{rollback: true}, version), egressRow(blocking, nil, version)} {
		if len(row.Why) > egressWhyLimit {
			t.Errorf("%d characters: %s", len(row.Why), row.Why)
		}
	}
	if len(egressNoNames) > egressWhyLimit {
		t.Errorf("%d characters: %s", len(egressNoNames), egressNoNames)
	}
}

// Outbound is technical detail: a responder with no grant reads the receipt whole but for the destinations
// (design/PERMISSIONS.md, Outbound), while a line that names none reads as it does for anyone.
func TestAResponderReadsNoDestinationOnTheRow(t *testing.T) {
	blocking := gatepolicy.Rule{ID: egressRule, Name: "Production allows everything preview connects to", State: gatepolicy.Blocking}
	for _, c := range []struct {
		name         string
		read         egressRead
		team, viewer string
	}{
		{"refused", egressRead{denied: []string{"api.openai.com"}}, "production does not allow api.openai.com", "production does not allow everything preview connected to. The app's team can fix this."},
		{"a rollback's", egressRead{rollback: true, dismissed: []string{"hooks.slack.com"}}, "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card.", egressRollback},
		{"could not check", egressRead{unread: egressNoRecord}, egressNoRecord, egressNoRecord},
		{"neutral", egressRead{orgRuns: true}, egressOrgRuns, egressOrgRuns},
		{"passed", egressRead{}, "production allows everything v1.4 has connected to so far", "production allows everything v1.4 has connected to so far"},
	} {
		rec := receipt{Rules: []receiptRule{egressRow(blocking, &c.read, "v1.4")}}
		if got := rec.Rules[0].Why; got != c.team {
			t.Errorf("%s: the team reads %q", c.name, got)
		}
		trimmed := rec.forResponder()
		if got := trimmed.Rules[0].Why; got != c.viewer || strings.Contains(string(mustJSON(trimmed)), "api.openai.com") || strings.Contains(string(mustJSON(trimmed)), "hooks.slack.com") {
			t.Errorf("%s: a responder reads %s", c.name, mustJSON(trimmed))
		}
		if rec.Rules[0].Why != c.team {
			t.Errorf("%s: trimming changed the receipt it was read from", c.name)
		}
	}
}
