package runs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/vault"
)

// nop reports nowhere: these tests read the log, not the timeline.
func nop(context.Context, gate.Check) error { return nil }

// portalReview is the AI review of the claims portal the golden records: the recorded answer
// (gate/testdata/review), the two routes a session does not guard and the table that refetches per
// keystroke — what only the reviewer reads (contracts/snapshot.md).
type portalReview struct{ t *testing.T }

func (m portalReview) Review(context.Context, gate.Request) (gate.Response, error) {
	var recorded map[string]gate.Response
	raw, err := os.ReadFile("../gate/testdata/review/claims-portal.json")
	if err := cmp.Or(err, json.Unmarshal(raw, &recorded)); err != nil || len(recorded) != 1 {
		m.t.Fatalf("the recorded review: %v", err)
	}
	for _, resp := range recorded {
		return resp, nil
	}
	return gate.Response{}, nil
}

// EXPERIENCE D4 on the fixture app: the verdict a run stores, derived and checked against the snapshot,
// is the seam-1 golden byte for byte — every finding and every word of it: the scanners' run, the
// reviewer's with the recorded review, and gitleaks' report of the Stripe key, the one tool on. The
// golden is what the product produces: regenerate it from here when the gate changes, never by hand.
func TestGateClaimsPortal(t *testing.T) {
	tree := map[string]string{}
	root := "../gate/testdata/apps/claims-portal"
	if err := fs.WalkDir(os.DirFS(root), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // the fixture app
		tree[name] = string(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, ctx := liveTree(t, Gate{Model: portalReview{t}, ModelID: "resolved-model-id"}.Review, tree)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek")) // the attach seals under the project's report key
	if err != nil {
		t.Fatal(err)
	}
	pipe.Vault = keys
	version, sealedKey, sealedValue, err := keys.Seal(project, "report", "key", make([]byte, 32)) // validate.py's TEST_PROJECT_KEY: the imported fingerprints are the golden's, every run
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE projects SET report_key_ref = $2, report_sealed_key = $3, report_sealed_value = $4 WHERE id = $1", project, version, sealedKey, sealedValue); err != nil {
		t.Fatal(err)
	}
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	// gitleaks alone of the tools, so no executor is asked, and the four policies only the others fill off
	off := map[string]policy.State{"sql.string-concat": policy.Off, "code.injection": policy.Off, "iac.misconfig": policy.Off, "deps.known-vuln": policy.Off}
	for _, tool := range []string{"opengrep", "eslint", "trivy", "checkov"} {
		off["sarif."+tool] = policy.Off
	}
	if snap, err = policy.WithStates(snap, off); err != nil {
		t.Fatal(err)
	}
	pipe.Attach = attachAs(t, snap)
	t.Cleanup(func() { pipe.Attach = nil })
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "blocked" {
		t.Fatalf("the hero app: %s %s", state, why(reason))
	}
	var got bytes.Buffer
	enc := json.NewEncoder(&got)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(verdictOf(t, ctx, p, run.ID)); err != nil {
		t.Fatal(err)
	}
	if got.String() != string(contracts.GoldenLog) {
		t.Fatalf("the verdict is not contracts/testdata/finding/claims-portal.sarif.json:\n%s", got.String())
	}
}

// A rule only a tool fills is checked while that tool runs: with opengrep and eslint off,
// sql.string-concat and code.injection are named not checked on this host, and iac.misconfig, which checkov and trivy still fill, is not.
func TestARuleOnlyAToolFillsIsNotCheckedWithoutIt(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	notes := func(snap policy.Snapshot) string {
		t.Helper()
		log, err := Gate{}.Review(context.Background(), "../gate/testdata/apps/claims-portal", "", snap, nop)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, inv := range log.Runs[0].Invocations {
			for _, n := range inv.ToolExecutionNotifications {
				texts = append(texts, n.Message.Text)
			}
		}
		return strings.Join(texts, " | ")
	}
	if got := notes(snap); strings.Contains(got, "code.injection") || strings.Contains(got, "iac.misconfig") {
		t.Fatalf("the defaults: %s", got)
	}
	off, err := policy.WithStates(snap, map[string]policy.State{"sarif.opengrep": policy.Off, "sarif.eslint": policy.Off})
	if err != nil {
		t.Fatal(err)
	}
	if got := notes(off); !strings.Contains(got, "not checked on this host: sql.string-concat, code.injection") || strings.Contains(got, "iac.misconfig") {
		t.Fatalf("opengrep and eslint off: %s", got)
	}
	// trivy reading vulnerabilities alone, with checkov off, fills nothing of iac.misconfig
	vulnOnly, err := policy.WithStates(snap, map[string]policy.State{"sarif.checkov": policy.Off})
	if err != nil {
		t.Fatal(err)
	}
	vulnOnly.Rules[slices.IndexFunc(vulnOnly.Rules, func(r policy.Rule) bool { return r.ID == "sarif.trivy" })].Settings.Scanners = []string{"vuln"}
	if got := notes(vulnOnly); !strings.Contains(got, "not checked on this host: iac.misconfig") {
		t.Fatalf("trivy vuln alone, checkov off: %s", got)
	}
}

// No module of Canyon's reads secrets, dependencies or SQL, the tools do: with the defaults the AI review
// is the one check, and with gitleaks off secrets.client-bundle is named not checked on this host, never
// read by anything else.
func TestOnlyTheToolsReadSecretsAndDependencies(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	review := func(snap policy.Snapshot) (checked, said []string) {
		t.Helper()
		report := func(_ context.Context, c gate.Check) error {
			if c.Status == "" {
				checked = append(checked, c.Name)
			}
			return nil
		}
		log, err := (Gate{}).Review(context.Background(), "../gate/testdata/apps/claims-portal", "", snap, report)
		if err != nil {
			t.Fatal(err)
		}
		return checked, notes(log.Runs[0])
	}
	if checked, _ := review(snap); !slices.Equal(checked, []string{"canyon-review"}) {
		t.Fatalf("the defaults checked %q", checked)
	}
	off, err := policy.WithStates(snap, map[string]policy.State{"sarif.gitleaks": policy.Off})
	if err != nil {
		t.Fatal(err)
	}
	if checked, said := review(off); !slices.Equal(checked, []string{"canyon-review"}) || !slices.ContainsFunc(said, func(n string) bool { return strings.HasPrefix(n, "not checked on this host: secrets.client-bundle") }) {
		t.Fatalf("gitleaks off checked %q and said %q", checked, said)
	}
}

// A tool Off before preview and placed before production is on: the rules it fills are checked, never
// named not checked on this host, so before production has findings to judge (contracts/snapshot.md,
// Two gates).
func TestAToolPlacedBeforeProductionAloneFillsItsRules(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range snap.Rules {
		if r.ID == "sarif.opengrep" || r.ID == "sarif.eslint" || r.ID == "sarif.gitleaks" {
			snap.Rules[i].State, snap.Rules[i].Production = policy.Off, policy.Blocking
		}
	}
	log, err := (Gate{}).Review(context.Background(), "../gate/testdata/apps/claims-portal", "", snap, func(context.Context, gate.Check) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if said := notes(log.Runs[0]); slices.ContainsFunc(said, func(n string) bool { return strings.HasPrefix(n, "not checked on this host") }) {
		t.Fatalf("said %q", said)
	}
}

// A tool Off before preview and placed before production is on: the run reads its reports and Canyon runs
// it; a tool Off in both gates is neither (contracts/snapshot.md, Two gates).
func TestAToolPlacedBeforeProductionAloneIsRunAndRead(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range snap.Rules {
		switch r.ID {
		case "sarif.trivy":
			snap.Rules[i].State, snap.Rules[i].Production = policy.Off, policy.Advisory
		case "sarif.checkov":
			snap.Rules[i].State = policy.Off
		}
	}
	var scanned []string
	for _, r := range scannedTools(snap) {
		scanned = append(scanned, r.ID)
	}
	if read := importedTools(snap); !slices.Contains(read, "trivy") || slices.Contains(read, "checkov") || !slices.Contains(scanned, "sarif.trivy") || slices.Contains(scanned, "sarif.checkov") {
		t.Fatalf("read %q, scanned %q", read, scanned)
	}
}

// answering is a host's model: it answers resp and keeps the id it was sent.
type answering struct {
	resp gate.Response
	id   string
}

func (a *answering) Review(_ context.Context, req gate.Request) (gate.Response, error) {
	a.id = req.Model
	return a.resp, nil
}

// With a model on the host the reviewer's run is the model's answer, sent under the host's id, and
// not the skipped notification.
func TestAHostModelReviews(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := gate.Candidate{Rule: "perf.unbounded-refetch", File: "apps/web/src/components/ClaimsTable.tsx", StartLine: 61, EndLine: 61,
		Severity: "medium", Title: "ClaimsTable refetches on each keystroke", Problem: "A full scan per character.", Fix: "Debounce it."}
	m := &answering{resp: gate.Response{Results: []gate.Candidate{c}}}
	log, err := Gate{Model: m, ModelID: "claude-opus-5"}.Review(context.Background(), "../gate/testdata/apps/claims-portal", "", snap, nop)
	if err != nil {
		t.Fatal(err)
	}
	review := log.Runs[len(log.Runs)-1]
	if m.id != "claude-opus-5" || review.Tool.Driver.Name != "canyon-review" || len(review.Invocations) != 1 || len(review.Invocations[0].ToolExecutionNotifications) != 1 ||
		review.Invocations[0].ToolExecutionNotifications[0].Message.Text != gate.SecondOpinion ||
		len(review.Results) != 1 || review.Results[0].RuleID != c.Rule || review.Results[0].Properties.Canyon.Title != c.Title {
		t.Fatalf("the reviewer's run is the host model's answer under the host's id (sent %q): %+v", m.id, review)
	}
}

// A rule the snapshot turns off is not scanned: with secrets.client-bundle off, a Vite app's key,
// assembled here so none sits on disk, is not its finding.
func TestARuleTurnedOffIsNotScanned(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	vite := t.TempDir()
	if err := cmp.Or(os.MkdirAll(filepath.Join(vite, "src"), 0o750), os.WriteFile(filepath.Join(vite, "package.json"), []byte(`{"devDependencies": {"vite": "5"}}`), 0o600),
		os.WriteFile(filepath.Join(vite, "src", "main.tsx"), []byte("export const k = \"sk_live_"+strings.Repeat("x9", 12)+"\";\n"), 0o600)); err != nil {
		t.Fatal(err)
	}
	if snap, err = policy.WithStates(snap, map[string]policy.State{"secrets.client-bundle": policy.Off}); err != nil {
		t.Fatal(err)
	}
	log, err := Gate{}.Review(context.Background(), vite, "", snap, nop)
	if err != nil || len(log.Runs[0].Results) != 0 {
		t.Fatalf("with secrets.client-bundle off the Vite app has nothing to report: %v %+v", err, log.Runs)
	}
}

// A gate step an older binary journaled as a bare log reads back as that log after an upgrade.
func TestABareLogCheckpointReadsBackAsTheGateStepsCheckpoint(t *testing.T) {
	var g gated
	if err := json.Unmarshal(contracts.GoldenLog, &g); err != nil || g.Model || len(g.Runs) != 3 || g.Runs[0].Tool.Driver.Name != "canyon/scanners" {
		t.Fatalf("the old checkpoint did not read back: %v %+v", err, g)
	}
}

// Each reason a review read nothing — no model, out of room, over 16 MiB — and each agent, the AI
// review's and the custom rules', is one gate.unreadable finding the profile admits, saying why in
// words, while a rule of it blocks before preview; an agent with no run at all fails closed too (#1555).
func TestEachReviewThatReadNothingIsOneFindingTheProfileAdmits(t *testing.T) {
	small, big := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{small: "export {};\n", big: "export const s = `" + strings.Repeat("x", 16<<20) + "`;\n"} {
		if err := os.WriteFile(filepath.Join(dir, "app.ts"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := policy.WithStates(testPolicy(), map[string]policy.State{"perf.unbounded-refetch": policy.Blocking})
	if err != nil {
		t.Fatal(err)
	}
	snap.Rules = append(snap.Rules, policy.Rule{ID: "custom.emails-encrypted", Name: "Customer emails are encrypted", State: policy.Blocking, Producer: policy.LLMReview, Instructions: "Flag an unencrypted customer email."})
	snap.Agents = append(snap.Agents, policy.Agent{ID: gate.CustomRules, Namespace: []string{"custom"}, Model: "resolved-model-id"})
	for _, c := range []struct {
		why string
		dir string
		g   Gate
	}{
		{"no model is configured on this Canyon host", small, Gate{}},
		{"the model ran out of room for its answer", small, Gate{Model: &counting{room: true}, ModelID: "claude-opus-5"}},
		{"the source is larger than it accepts (16 MiB)", big, Gate{Model: &counting{}, ModelID: "claude-opus-5"}},
		{"it did not run", small, Gate{}}, // the agents' runs dropped below
	} {
		log, err := c.g.Review(context.Background(), c.dir, "", snap, nop)
		if err != nil {
			t.Fatal(err)
		}
		if c.why == "it did not run" {
			log.Runs = log.Runs[:1]
		}
		tree, err := gate.Walk(c.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range unreadBlocking(log, snap) {
			if err := gate.CouldNotCheck(&log, tree, m.Tool, m.Why); err != nil {
				t.Fatal(err)
			}
		}
		if err := cmp.Or(finding.Derive(&log, snap), finding.Check(&log, snap)); err != nil {
			t.Fatalf("%s: the profile refuses it: %v", c.why, err)
		}
		var problems []string
		for _, r := range log.Runs[0].Results {
			if r.RuleID == finding.Unreadable.ID && r.Properties.Canyon.Blocks != nil && *r.Properties.Canyon.Blocks {
				problems = append(problems, r.Properties.Canyon.Problem)
			}
		}
		if want := []string{"could not check: the AI review: " + c.why, "could not check: custom rules: " + c.why}; !slices.Equal(problems, want) {
			t.Errorf("%s: blocking gate.unreadable %q", c.why, problems)
		}
	}
	if missed := unreadBlocking(finding.Log{}, testPolicy()); len(missed) != 0 { // advisory: the note alone
		t.Errorf("an advisory rule unread: %+v", missed)
	}
}

// A host that signs people in reviews as one: Gate's SignedIn reaches the reviewer, so an
// auth.unauthenticated-route result carries Canyon's words (contracts/finding.md).
func TestASignedInHostsGateReviewsAsOne(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := gate.Candidate{Rule: "auth.unauthenticated-route", File: "apps/web/src/components/ClaimsTable.tsx", StartLine: 61, EndLine: 61,
		Severity: "medium", Title: "GET /claims has no sign-in check", Problem: "Anyone reads it.", Fix: "Add a login page."}
	log, err := Gate{Model: &answering{resp: gate.Response{Results: []gate.Candidate{c}}}, ModelID: "claude-opus-5", SignedIn: true}.Review(context.Background(), "../gate/testdata/apps/claims-portal", "", snap, nop)
	if err != nil {
		t.Fatal(err)
	}
	if got := log.Runs[len(log.Runs)-1].Results; len(got) != 1 || !strings.HasPrefix(got[0].Properties.Canyon.Fix, "Don't build a login of your own") {
		t.Fatalf("results %+v", got)
	}
}

// A split review that left a file unread ends skipped whatever its other parts found, the findings kept
// in its run: a rule it reviews is never passed on code nothing read (contracts/finding.md, #2274).
func TestAReviewThatLeftAFileUnreadEndsSkippedWithItsFindings(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"apps/web/src/lib/payments.ts": "export const stripe = new Stripe();\n", "apps/web/src/generated.ts": strings.Repeat("x", 3<<20)} {
		if err := cmp.Or(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700), os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)); err != nil {
			t.Fatal(err)
		}
	}
	var done gate.Check
	report := func(_ context.Context, c gate.Check) error {
		if c.Status != "" {
			done = c
		}
		return nil
	}
	log, err := Gate{Model: &counting{}, ModelID: "claude-opus-5"}.agent(context.Background(), dir, "", testPolicy(), "canyon-review", finding.Log{}, report)
	if err != nil {
		t.Fatal(err)
	}
	if run := log.Runs[len(log.Runs)-1]; done.Status != "skipped" || done.Findings != 1 || len(run.Results) != 1 || gate.Unread(run) != gate.TooLargeForACall {
		t.Fatalf("the check ended %q with %d finding(s); the run %d result(s), unread %q", done.Status, done.Findings, len(run.Results), gate.Unread(run))
	}
}

// Every note of a review that read nothing has its reason on the gate.unreadable finding: one with none
// would say "could not check: the AI review: " and nothing after it (#2280).
func TestEveryUnreadNoteHasAReason(t *testing.T) {
	for _, note := range gate.UnreadNotes {
		if unreadWhy[note] == "" {
			t.Errorf("no reason for %q", note)
		}
	}
}
