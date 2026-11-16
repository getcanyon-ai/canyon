package finding

import (
	"errors"
	"strings"
	"testing"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// stripeKey is a Stripe-shaped key gitleaks finds — kind sk or rk, tag telling keys apart — assembled
// at run time, so no source file of the repo carries one whole (gitleaks scans the repo too).
func stripeKey(kind, tag string) string {
	return kind + "_" + "live_" + tag + "MZq8fQ2xYv7Kp3Lw9Rt4"
}

// A live key is what gitleaks' default ruleset finds, whatever the vendor: a Stripe live or test key
// and a GitHub token are; a low-entropy filler in a key's shape, which gitleaks passes, is not.
func TestALiveKeyIsWhatGitleaksFinds(t *testing.T) {
	for text, want := range map[string]bool{
		`k = "` + stripeKey("sk", "A1") + `"`:                                     true,
		`k = "` + strings.Replace(stripeKey("sk", "A1"), "live", "test", 1) + `"`: true,
		"token: ghp_" + "Xq7Lm2Vt9Rk4Pz8Ws3Nb6Hy1Jd5Fg0Ac2Ue7":                    true,
		`k = "sk_live_` + strings.Repeat("Q", 24) + `"`:                           false,
		"nothing here": false,
	} {
		if got := HasLiveKey([]byte(text)); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
	key := stripeKey("sk", "A1")
	if keys, err := keysIn(`pay("` + key + `")`); err != nil || len(keys) != 1 || keys[0] != key {
		t.Errorf("found %q, want the key itself", keys)
	}
}

// Validate refuses a serialized log carrying any secret gitleaks finds, not a Stripe key alone: a
// GitHub token in a problem, an AWS key in a fix, a Stripe test key in a title.
func TestValidateRefusesAnyGitleaksSecret(t *testing.T) {
	for name, edit := range map[string]func(c *Canyon){
		"a GitHub token in a problem":            func(c *Canyon) { c.Problem = "The token ghp_" + "Xq7Lm2Vt9Rk4Pz8Ws3Nb6Hy1Jd5Fg0Ac2Ue7 is public." },
		"an AWS key in a fix":                    func(c *Canyon) { c.Fix = "Rotate aws_access_key_id=AKIA" + "QJ5K7MZ2RT4WX6YB now." },
		"a key read by its quotes, in a problem": func(c *Canyon) { c.Problem = `It sets api_key = "Q2xYv7Kp3` + `Lw9Rt4NbHz8M" in source.` },
		"a Stripe test key in a title": func(c *Canyon) {
			c.Title = "uses " + strings.Replace(stripeKey("sk", "T1"), "live", "test", 1) + " here"
		},
	} {
		_, log := readGolden(t)
		edit(&log.Runs[0].Results[0].Properties.Canyon)
		log.Runs[0].Results[0].Message.Text = log.Runs[0].Results[0].Properties.Canyon.Title
		if err := log.Validate(); err == nil || !strings.Contains(err.Error(), "live key") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A key gitleaks reads by what surrounds it — generic-api-key wants a keyword before the value — is
// masked alike in every piece a snippet is checked in, so a finding whose region is that value, on
// any rule, is built, not refused for redaction moving its span.
func TestAKeyReadByItsContextIsMaskedAlikeInEveryPiece(t *testing.T) {
	value := "Q2xYv7Kp3" + "Lw9Rt4NbHz8M"
	line := `const apiKey = "` + value + `"; call(apiKey)`
	start := strings.Index(line, value) + 1
	got, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: Low, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + len(value), Lines: []string{line}}})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(got.Locations[0].PhysicalLocation.Region.Snippet.Text, value) {
		t.Fatalf("the value reached the snippet: %s", got.Locations[0].PhysicalLocation.Region.Snippet.Text)
	}
}

// Each string of a log is checked on its own: gitleaks pairs a keyword with a value, so read over the
// whole log one string's "sourcegraph" and another's commit sha would be a token neither carries.
func TestValidateReadsEachStringOnItsOwn(t *testing.T) {
	_, log := readGolden(t)
	c := &log.Runs[0].Results[0].Properties.Canyon
	c.Title = "Unpinned action in the sourcegraph sync workflow"
	c.Fix = "Pin it: uses: actions/checkout@b4ffde65f46336ab88eb53be808477a3936bae11"
	log.Runs[0].Results[0].Message.Text = c.Title
	if err := log.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A key on the line of a finding outside secrets.* is masked in its snippet, and the finding is built.
func TestAKeyOnAnyRulesLineIsMasked(t *testing.T) {
	key := stripeKey("sk", "A1")
	line := `refetch(); const k = "` + key + `";`
	got, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: Low, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 10, Lines: []string{line}}})
	if err != nil {
		t.Fatal(err)
	}
	if snippet := got.Locations[0].PhysicalLocation.Region.Snippet.Text; strings.Contains(snippet, key) {
		t.Fatalf("the key reached the snippet: %s", snippet)
	}
}

// A detector that will not build passes nothing: every text reads as carrying a key, and New refuses.
func TestABrokenDetectorPassesNothing(t *testing.T) {
	saved := liveKeys
	t.Cleanup(func() { liveKeys = saved })
	liveKeys = func() (*detect.Detector, error) { return nil, errors.New("no ruleset") }
	if !HasLiveKey([]byte("nothing here")) {
		t.Error("a clean text passed a broken detector")
	}
	_, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: Low, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 10, Lines: []string{"refetch();"}}})
	if err == nil || !strings.Contains(err.Error(), "no ruleset") {
		t.Errorf("New on a broken detector: %v", err)
	}
}

// A window cut inside a word a secrets detector skips whole would leave part of it: the window starts
// at the word's start instead, so a long line holding publicKey = "…" 100 characters before the
// region still builds (contracts/finding.md, the snippet window).
func TestAWindowNeverCutsInsideAWord(t *testing.T) {
	value := "Q2xYv7Kp3" + "Lw9Rt4NbHz8M"
	head := `const publicKey = "` + value + `"; `
	line := strings.Repeat("x", 60) + head
	line += strings.Repeat(" ", 100-len(head)+len("const pu")) + "refetch();" + strings.Repeat(";", 400)
	start := strings.Index(line, "refetch") + 1
	got, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: Low, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + len("refetch()"), Lines: []string{line}}})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if text := got.Locations[0].PhysicalLocation.Region.Snippet.Text; !strings.Contains(text, "…publicKey") {
		t.Fatalf("the window: %.60q", text)
	}
}

// A window whose cut drops a match the detector skips can leave a key name inside that match's value
// reading as a key: publicKey = "token=…" is skipped whole, token=… alone is not. The window's start
// moves back until it holds no key the whole line did not (#1374).
func TestAWindowNeverShowsAKeyTheLineDidNot(t *testing.T) {
	value := "Q2xYv7Kp3" + "Lw9Rt4NbHz8M"
	line := strings.Repeat("x", 60) + ` const publicKey = "token=` + value + `"; `
	if keys, err := keysIn(line); err != nil || len(keys) != 0 {
		t.Fatalf("the whole line reads as a key: %q %v", keys, err)
	}
	if keys, _ := keysIn(`token=` + value + `";`); len(keys) == 0 {
		t.Fatal("token=… alone does not read as a key: the case below proves nothing")
	}
	k := strings.Index(line, "token")
	line += strings.Repeat(" ", k+windowLead+2-len(line)) + "refetch();" + strings.Repeat(";", 400) // the cut lands inside token
	start := strings.Index(line, "refetch") + 1
	got, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: Low, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + len("refetch()"), Lines: []string{line}}})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	text, shown := got.Locations[0].PhysicalLocation.Region.Snippet.Text, got.Properties.Canyon.Shown
	if !strings.Contains(text, "publicKey") || shown == nil || []rune(text)[shown.StartColumn-1:][0] != 'r' {
		t.Fatalf("the window %.60q, shown %+v", text, shown)
	}
}
