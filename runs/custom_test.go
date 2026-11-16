package runs

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/custom"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
)

// sentRules is a model that answers nothing and keeps the rules each call was sent.
type sentRules struct {
	mu    sync.Mutex
	rules [][]string
}

func (m *sentRules) Review(_ context.Context, req gate.Request) (gate.Response, error) {
	var ids []string
	for _, r := range req.Rules {
		ids = append(ids, r.ID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append(m.rules, ids)
	return gate.Response{Results: []gate.Candidate{}}, nil
}

// noLodash is snap with a Blocking custom rule checked by code, as the server composes one: producer
// scanner, and no custom-rules agent, since no rule checked by AI is on (contracts/snapshot.md).
func noLodash() policy.Snapshot {
	snap := testPolicy()
	snap.Rules = append(slices.Clone(snap.Rules), policy.Rule{ID: "custom.no-lodash", Name: "No lodash", State: policy.Blocking, Producer: policy.Scanner,
		Instructions: "No app may depend on lodash.",
		Check:        &policy.Check{By: custom.ByCode, Expr: `!dependencies.exists(d, d.name == 'lodash')`, Says: "No dependency is named lodash.", FactsVersion: custom.FactsVersion}})
	return snap
}

// lodashLocked is trivy's report of a package-lock.json that locks lodash 4.17.21, which the app asks for.
var lodashLocked = custom.Packages{Report: []byte(`{"SchemaVersion":2,"Results":[{"Target":"package-lock.json","Class":"lang-pkgs","Type":"npm",` +
	`"Packages":[{"Name":"lodash","Version":"4.17.21","Relationship":"direct"}]}]}`)}

// reviewedWith reviews files under snap as the gate step does — the gate, then the custom rules checked
// by code over pkgs — derived and checked against the profile, and answers its blocking results and the
// rules the model was sent.
func reviewedWith(t *testing.T, snap policy.Snapshot, files map[string]string, pkgs custom.Packages) ([]finding.Result, [][]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := &sentRules{}
	log, err := Gate{Model: m, ModelID: "claude-opus-5"}.Review(context.Background(), dir, "", snap, nop)
	if err != nil {
		t.Fatal(err)
	}
	if err := addCustom(context.Background(), &log, dir, snap, []custom.Secret{}, pkgs); err != nil {
		t.Fatal(err)
	}
	if err := cmp.Or(finding.Derive(&log, snap), finding.Check(&log, snap)); err != nil {
		t.Fatalf("the profile refuses the log: %v", err)
	}
	var blocking []finding.Result
	for _, run := range log.Runs {
		if run.Tool.Driver.Name == gate.CustomRules {
			t.Errorf("the custom-rules agent ran")
		}
		for _, r := range run.Results {
			if r.Properties.Canyon.Blocks != nil && *r.Properties.Canyon.Blocks {
				blocking = append(blocking, r)
			}
		}
	}
	return blocking, m.rules
}

// A review of an app that breaks a Blocking custom rule checked by code is blocked by that rule's
// finding, and no AI call is sent the rule (#2639).
func TestACustomRuleCheckedByCodeBlocksWithNoAICallForIt(t *testing.T) {
	blocking, sent := reviewedWith(t, noLodash(), map[string]string{"index.js": "require('lodash');\n", "package.json": `{"dependencies": {"lodash": "^4"}}`,
		"package-lock.json": `{"lockfileVersion": 3, "packages": {"": {"dependencies": {"lodash": "^4"}}, "node_modules/lodash": {"version": "4.17.21"}}}`}, lodashLocked)
	if len(blocking) != 1 || blocking[0].RuleID != "custom.no-lodash" || !strings.HasPrefix(blocking[0].Properties.Canyon.Problem, `"No lodash" (custom.no-lodash) does not hold`) {
		t.Fatalf("blocking: %+v", blocking)
	}
	for _, rules := range sent {
		if slices.Contains(rules, "custom.no-lodash") {
			t.Fatalf("a model call was sent the rule: %v", sent)
		}
	}
}

// The same rule on an app whose dependencies Canyon cannot read, or with trivy off, is could-not-check,
// which blocks too: never a pass.
func TestACustomRuleCheckedByCodeThatCouldNotBeCheckedBlocks(t *testing.T) {
	files := map[string]string{"index.js": "require('lodash');\n", "package.json": "{}", "package-lock.json": "{}"}
	for pkgs, why := range map[*custom.Packages]string{
		{Why: custom.TrivyOff}: "Canyon reads an app's packages with Trivy, which is off on this Canyon",
		{Report: []byte(`{"SchemaVersion":2,"Results":[{"Target":"requirements.txt","Class":"lang-pkgs","Type":"pip"}]}`)}: "requirements.txt names the packages it asks for, not every one pip installs: a uv.lock, poetry.lock or Pipfile.lock names them all",
	} {
		f := files
		if pkgs.Why == "" {
			f = map[string]string{"app.py": "import flask\n", "requirements.txt": "flask==3.1.0\n"}
		}
		blocking, _ := reviewedWith(t, noLodash(), f, *pkgs)
		if len(blocking) != 1 || blocking[0].RuleID != "custom.no-lodash" || blocking[0].Properties.Canyon.Problem != "could not check: custom.no-lodash: dependencies: "+why {
			t.Errorf("blocking: %+v", blocking)
		}
	}
}

// The review asks trivy for the package's packages while a rule checked by code reads the dependencies
// and sarif.trivy is on and carries run, waiting out a busy executor; with trivy off, or no rule that reads
// them, none is asked (#2644).
func TestTheReviewAsksTrivyForThePackagesWhileItRuns(t *testing.T) {
	_, ctx, f, snap := scanning(t)
	snap.Rules = append(snap.Rules, noLodash().Rules[len(noLodash().Rules)-1])
	in := Input{Project: project, Package: pkg, Policy: snap}
	f.Packages.Store(&executor.ScanResult{Tool: "trivy", Image: "ghcr.io/aquasecurity/trivy@" + executortest.Digest, Packages: lodashLocked.Report})
	f.Busy.Store(1)
	if got, err := pipe.packages(ctx, in); err != nil || string(got.Report) != string(lodashLocked.Report) || f.PackageScans.Load() != 1 {
		t.Fatalf("%+v %v, %d scans", got, err, f.PackageScans.Load())
	}
	for _, rule := range []func(*policy.Rule){func(r *policy.Rule) { r.Run = false }, func(r *policy.Rule) { r.State = policy.Off }} {
		off := snap
		off.Rules = slices.Clone(snap.Rules)
		rule(&off.Rules[slices.IndexFunc(off.Rules, func(r policy.Rule) bool { return r.ID == "sarif.trivy" })])
		before := f.PackageScans.Load()
		if got, err := pipe.packages(ctx, Input{Project: project, Package: pkg, Policy: off}); err != nil || got.Why != custom.TrivyOff || f.PackageScans.Load() != before {
			t.Fatalf("trivy off: %+v %v", got, err)
		}
	}
	readme := snap
	readme.Rules = slices.Clone(snap.Rules)
	readme.Rules[len(readme.Rules)-1].Check = &policy.Check{By: custom.ByCode, Expr: "'README.md' in files", Says: "The app has a README.md.", FactsVersion: custom.FactsVersion}
	before := f.PackageScans.Load()
	if got, err := pipe.packages(ctx, Input{Project: project, Package: pkg, Policy: readme}); err != nil || got.Report != nil || f.PackageScans.Load() != before {
		t.Fatalf("a rule that reads no dependencies asked trivy: %+v %v", got, err)
	}
}

// Nothing of the packages scan fails the run: a scan that did not finish, answered too much, an executor
// busy past busyFor or one that cannot be asked each read as could-not-check, in plain words (#2652 review).
func TestThePackagesScanNeverFailsTheReview(t *testing.T) {
	_, ctx, f, snap := scanning(t)
	snap.Rules = append(snap.Rules, noLodash().Rules[len(noLodash().Rules)-1])
	in := Input{Project: project, Package: pkg, Policy: snap}
	image := "ghcr.io/aquasecurity/trivy@" + executortest.Digest
	for answer, why := range map[string]string{"timeout": custom.TrivyUnfinished, "over_cap": custom.TrivyTooLarge, "no_image": custom.TrivyUnfinished} {
		f.Packages.Store(&executor.ScanResult{Tool: "trivy", Image: image, NotFinished: answer, Reason: map[string]string{"over_cap": "report"}[answer]})
		if got, err := pipe.packages(ctx, in); err != nil || got.Report != nil || got.Why != why {
			t.Errorf("%s: %+v %v", answer, got, err)
		}
	}
	was := busyFor
	busyFor = 1500 * time.Millisecond
	t.Cleanup(func() { busyFor = was })
	f.Busy.Store(10)
	if got, err := pipe.packages(ctx, in); err != nil || got.Why != custom.TrivyBusy {
		t.Errorf("busy past busyFor: %+v %v", got, err)
	}
	f.Busy.Store(0)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	client := pipe.Executor
	pipe.Executor = &executor.Client{Base: gone.URL}
	t.Cleanup(func() { pipe.Executor = client })
	if got, err := pipe.packages(ctx, in); err != nil || got.Why != custom.TrivyUnasked {
		t.Errorf("an executor that cannot be asked: %+v %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pipe.packages(cancelled, in); !errors.Is(err, context.Canceled) {
		t.Errorf("the run's own cancellation: %v", err)
	}
}

// A custom rule checked by code is the scanners' run's own, never a scanner rule no tool fills: the run
// never names it "not checked on this host", which would read a clean verdict as partial (#2645 review).
func TestACustomRuleCheckedByCodeIsNeverNamedNotChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := Gate{}.Review(context.Background(), dir, "", noLodash(), nop)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range log.Runs[0].Invocations {
		for _, n := range inv.ToolExecutionNotifications {
			if strings.Contains(n.Message.Text, "custom.no-lodash") {
				t.Fatalf("the scanners' run names the rule: %q", n.Message.Text)
			}
		}
	}
}
