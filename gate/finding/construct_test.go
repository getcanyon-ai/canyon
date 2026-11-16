package finding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/contracts"
)

const fixtureApp = "../testdata/apps/claims-portal"

// spanFor rebuilds the Span a golden location came from, reading its lines from the fixture app.
func spanFor(t *testing.T, loc Location) Span {
	t.Helper()
	p := loc.PhysicalLocation
	raw, err := os.ReadFile(filepath.Join(fixtureApp, filepath.FromSlash(p.ArtifactLocation.URI)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	return Span{
		URI: p.ArtifactLocation.URI, StartLine: p.Region.StartLine, StartColumn: p.Region.StartColumn,
		EndLine: p.Region.EndLine, EndColumn: p.Region.EndColumn, Lines: lines[p.Region.StartLine-1 : p.Region.EndLine],
	}
}

// inputFor rebuilds the Input that produces a golden result from the fixture app.
func inputFor(t *testing.T, log Log, run, i int) Input {
	t.Helper()
	want := log.Runs[run].Results[i]
	c := want.Properties.Canyon
	in := Input{Severity: c.Severity, Title: c.Title, Problem: c.Problem, Fix: c.Fix,
		RequiredEndState: c.RequiredEndState, DoNot: c.DoNot, Location: spanFor(t, want.Locations[0])}
	if c.SecretName != nil {
		in.SecretName = *c.SecretName
	}
	for _, rule := range log.Runs[run].Tool.Driver.Rules {
		if rule.ID == want.RuleID {
			in.Rule = rule
		}
	}
	for _, loc := range want.RelatedLocations {
		in.Related = append(in.Related, spanFor(t, loc))
	}
	return in
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustEqualGolden constructs in and compares it to the golden result. Two fields are compared
// separately, both because something later derives them: blocks by the server from the
// snapshot (seam 5), and until it has, New says true; canyon/v1 by the gate, last, over the
// final log, so New leaves it empty and a producer never sets it.
// Returns the serialized result so the caller can look for what must not be in it.
func mustEqualGolden(t *testing.T, in Input, want Result) []byte {
	t.Helper()
	got, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	if !*got.Properties.Canyon.Blocks {
		t.Fatal("a finding the server has not judged must block")
	}
	got.Properties.Canyon.Blocks = want.Properties.Canyon.Blocks
	if got.PartialFingerprints.CanyonV1 != "" {
		t.Fatalf("New set canyon/v1 %q; the gate derives it", got.PartialFingerprints.CanyonV1)
	}
	got.PartialFingerprints = want.PartialFingerprints
	out := mustJSON(t, got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("constructed finding differs from the golden one:\n got: %s\nwant: %s", out, mustJSON(t, want))
	}
	return out
}

// Redaction happens at construction: built from the fixture app's real line, the hero
// finding equals the golden one, and the literal appears nowhere in its serialization —
// not in the result, not in the SARIF log around it (the shape every consumer reads and the
// verdict row carries, which passes both seam-1 schemas), and not in prose that quoted it,
// whichever string it was in.
func TestNewRedactsTheCredentialAtConstruction(t *testing.T) {
	_, log := readGolden(t)
	want := log.Runs[0].Results[0]
	in := inputFor(t, log, 0, 0)
	// The literal comes from the fixture app itself, never from this file (README: not a real key).
	literal, err := in.Location.literal()
	if err != nil {
		t.Fatal(err)
	}
	out := mustEqualGolden(t, in, want)
	if strings.Contains(string(out), literal) {
		t.Fatalf("the literal survived construction:\n%s", out)
	}
	for _, s := range stringsInJSON(t, out) { // json escapes <, > and &: the byte check alone is blind to a literal carrying them
		if strings.Contains(s, literal) {
			t.Fatalf("the literal survived construction, decoded:\n%s", out)
		}
	}
	whole := Log{Version: Version, Schema: SchemaURI, Runs: []Run{{ColumnKind: ColumnKindCodePoints, Tool: log.Runs[0].Tool}}}
	if whole.Runs[0].Results, err = appendNew(nil, in); err != nil {
		t.Fatal(err)
	}
	// New leaves canyon/v1 empty, so a log straight out of construction does not satisfy the
	// profile's fingerprint pattern: the gate derives it last, over the final log. Stand one of
	// the right shape in here — deriving it by the recipe is the runner's job and is proven
	// there (gate, TestTheRunnerFingerprintsByTheRecipe) — so the schemas still see this log.
	whole.Runs[0].Results[0].PartialFingerprints.CanyonV1 = "f_" + strings.Repeat("ab", 32)
	raw := mustJSON(t, whole)
	if err := whole.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := contracts.CheckFindingSchemas(raw); err != nil {
		t.Fatalf("a constructed log fails the seam-1 schemas: %v", err)
	}
	if strings.Contains(string(raw), literal) {
		t.Fatalf("the literal survived into the log:\n%s", raw)
	}
	for _, s := range stringsInJSON(t, raw) {
		if strings.Contains(s, literal) {
			t.Fatalf("the literal survived into the log, decoded:\n%s", raw)
		}
	}
	for field, set := range map[string]func(*Input, string){
		"title":              func(in *Input, s string) { in.Title = s },
		"problem":            func(in *Input, s string) { in.Problem = s },
		"fix":                func(in *Input, s string) { in.Fix = s },
		"policy_name":        func(in *Input, s string) { in.Rule.Name = s },
		"required_end_state": func(in *Input, s string) { in.RequiredEndState = s },
		"do_not":             func(in *Input, s string) { in.DoNot = []string{s} },
	} {
		t.Run(field, func(t *testing.T) {
			in := inputFor(t, log, 0, 0)
			set(&in, "The key is "+literal+".")
			got, err := New(in)
			if err != nil {
				t.Fatal(err)
			}
			out := mustJSON(t, got)
			if strings.Contains(string(out), literal) || !strings.Contains(string(out), "The key is "+mask(literal)+".") {
				t.Fatalf("%s was not redacted to the masked form:\n%s", field, out)
			}
		})
	}
}

func appendNew(results []Result, in Input) ([]Result, error) {
	r, err := New(in)
	if err != nil {
		return nil, err
	}
	return append(results, r), nil
}

// stringsInJSON decodes the serialized document and returns every string in it: the byte
// check misses a literal json escaped (<, >, &), the decoded walk does not.
func stringsInJSON(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	return out
}

// A finding outside secrets.* is not redacted: the reviewer's finding is built from the fixture app
// and equals the golden one verbatim.
func TestNewLeavesNonCredentialFindingsVerbatim(t *testing.T) {
	_, log := readGolden(t)
	mustEqualGolden(t, inputFor(t, log, 1, 0), log.Runs[1].Results[0])
}

var clientBundle = Rule{ID: "secrets.client-bundle", Name: "No secrets in the code"}

// Every span of a credential finding is a literal, columns count characters, and a
// literal too short to keep a prefix and four characters visible is hidden whole.
func TestNewRedactsEverySpanByCharacter(t *testing.T) {
	main := Span{URI: "a.ts", StartLine: 1, StartColumn: 14, EndLine: 1, EndColumn: 46, Lines: []string{`// ünïcödé — "EXAMPLE_KEY_ABCDEFGHIJKLMNOPQR"`}}
	other := Span{URI: "b.ts", StartLine: 2, StartColumn: 5, EndLine: 3, EndColumn: 4, Lines: []string{`x = "secondsecret`, `12"`}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: main, Related: []Span{other}})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Locations[0].PhysicalLocation.Region.Snippet.Text; s != `// ünïcödé — "EXAMPLE_KEY_A••••OPQR"` {
		t.Errorf("main snippet %q", s)
	}
	if s := got.RelatedLocations[0].PhysicalLocation.Region.Snippet.Text; s != "x = \"••••\"" {
		t.Errorf("related snippet %q: a short literal is hidden whole, across lines", s)
	}
}

// Two literals where one is inside the other: the longer is replaced first, so neither the
// shorter one nor any fragment of the longer one is left in the snippet.
func TestNewRedactsTheLongerLiteralFirst(t *testing.T) {
	short := "SHORTSECRET01"
	long := "PREFIXPART_" + short + "_SUFFIXPART_EXTRA1234"
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 7 + len(short), Lines: []string{`x = "` + short + `"`}}
	related := Span{URI: "b.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 7 + len(long), Lines: []string{`y = "` + long + `"`}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc, Related: []Span{related}})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.RelatedLocations[0].PhysicalLocation.Region.Snippet.Text; s != `y = "PREFIXPART_SHOR••••1234"` {
		t.Errorf("related snippet %q", s)
	}
	out := string(mustJSON(t, got))
	if strings.Contains(out, short) || strings.Contains(out, "SUFFIXPART") {
		t.Fatalf("a literal or a fragment of one survived:\n%s", out)
	}
}

// Two keys on one line are two findings, and each one's snippet is the whole line: the key the
// other finding names is masked there too, or it survives into the snippet and New refuses a
// legal input (canyonv2-0jg case 2).
func TestTwoKeysOnOneLineAreEachMasked(t *testing.T) {
	a, b := stripeKey("sk", "A1"), stripeKey("sk", "B2")
	line := `pay("` + a + `", "` + b + `");`
	for _, start := range []int{5, 9 + len(a)} {
		loc := Span{URI: "a.ts", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + 2 + len(a), Lines: []string{line}}
		got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
		if err != nil {
			t.Fatalf("the finding at column %d was refused: %v", start, err)
		}
		if out := string(mustJSON(t, got)); strings.Contains(out, a) || strings.Contains(out, b) {
			t.Fatalf("a key survived the finding at column %d:\n%s", start, out)
		}
	}
}

// Every other key on the line is masked in the snippet as well: an rk_live_ restricted key, and a
// literal whose escapes decode to a live key, which the raw pattern never sees.
func TestOtherKeysOnTheLineAreMasked(t *testing.T) {
	a := stripeKey("sk", "A1")
	line := `pay("` + a + `", "` + stripeKey("rk", "B2") + `", "sk\x5flive_C3` + stripeKey("sk", "")[8:] + `");`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 7 + len(a), Lines: []string{line}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	snippet := got.Locations[0].PhysicalLocation.Region.Snippet.Text
	if strings.Contains(snippet, "Q2xYv7Kp3Lw9") {
		t.Fatalf("a key survived into the snippet: %s", snippet)
	}
}

// A literal whose escapes decode to a live key is masked wherever the lexer reads it whole: a string, a
// template and a template's part, after a regular expression that holds a quote; a line the lexer
// cannot read on its own is left as it is, to the keys found in its raw text.
func TestALiteralThatDecodesToALiveKeyIsMasked(t *testing.T) {
	escaped := `sk\x5flive_C3` + stripeKey("sk", "")[8:]
	for line, masked := range map[string]bool{
		`const k = "` + escaped + `";`:                                    true,
		`  apiKey: '` + escaped + `',`:                                    true,
		"const k = `" + escaped + "`;":                                    true,
		"const k = `" + escaped + "${tenant}`;":                           true,
		`const re = /"x/; const k = "` + escaped + `";`:                   true,
		`const half = total / 2, k = "` + escaped + `";`:                  true,
		`const r = count! / 2, k = "` + escaped + `";`:                    true, // TypeScript's non-null ! still ends the operand
		`  @Input() key = "` + escaped + `";`:                             true, // a decorator's @, which JavaScript has no token for
		`<Foo apiKey={"` + escaped + `"}></Foo>`:                          true, // </ closes an element
		`const r = this.#n / 2, k = "` + escaped + `";`:                   true,
		`for (const m of /["']/g.exec(s)) k = "` + escaped + `";`:         true,
		`<Foo apiKey="&#115;k_live_C3` + stripeKey("sk", "")[8:] + `" />`: true,  // a JSX attribute's entity
		`const k = "` + stripeKey("sk", "C3") + `";`:                      false, // no escape: the raw keys mask it
		`<p>Don't</p>; const k = "` + escaped + `";`:                      false, // JSX prose the lexer cannot read
	} {
		if got := maskCooked(line); strings.Contains(got, "Q2xYv7Kp3Lw9") == masked {
			t.Errorf("%s: %s", line, got)
		}
	}
}

// The mask's prefix never exceeds the hidden middle; a literal too short to hide eight
// characters behind a prefix and four is hidden whole.
func TestMaskKeepsThePrefixInsideTheHiddenMiddle(t *testing.T) {
	for literal, want := range map[string]string{
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ01234567": "ABCDEFGHIJKLMNO••••4567", // 34: the prefix and the last four
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ0":        "ABCDEFGHIJK••••XYZ0",     // 27: 15 shown, 12 hidden
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ":         "••••",                    // 26: hidden whole
	} {
		if got := mask(literal); got != want {
			t.Errorf("mask(%d runes) = %q, want %q", len([]rune(literal)), got, want)
		}
	}
}

// New refuses what it cannot redact and what the profile refuses, and its errors never
// repeat producer text: a span outside its lines or bisecting a word, one too short to be a
// credential, a line with a line break, a rule without a namespace or name, a live-shaped key
// on any rule, an unknown severity, a misplaced secret_name. The two cases that poisoned a
// producer-set fingerprint are gone with the field: the gate derives canyon/v1 itself, so
// there is no producer value left to carry a literal.
func TestNewFailsClosed(t *testing.T) {
	lit := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"
	ok := Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 6, EndLine: 1, EndColumn: 38, Lines: []string{`k := "` + lit + `"; x`}}}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Input){
		"span outside its line":       func(in *Input) { in.Location.EndColumn = 60 },
		"span on missing lines":       func(in *Input) { in.Location.EndLine = 2 },
		"span too short":              func(in *Input) { in.Location.Lines[0], in.Location.EndColumn = `k := "ABC"; x`, 11 },
		"span starts inside the word": func(in *Input) { in.Location.StartColumn = 8 },
		"span ends inside the word":   func(in *Input) { in.Location.EndColumn = 20 },
		"span with no uri":            func(in *Input) { in.Location.URI = "" },
		"line carries a line break":   func(in *Input) { in.Location.Lines[0] += "\r" },
		// Off secrets.* nothing is masked, so the span's own four lines are what serializes and
		// the cap refuses them. Under a secrets rule the same span is now ACCEPTED, because the
		// literal collapses first — TestNewCollapsesATemplateOverThreeLines is that half.
		"snippet over 4 lines off secrets": func(in *Input) {
			in.Rule = Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}
			in.Location.EndLine, in.Location.EndColumn = 4, 4
			in.Location.Lines = append(in.Location.Lines, "more", "lines", "yet")
		},
		"gate.unreadable quoting its line": func(in *Input) { in.Rule = Unreadable },
		"rule without a namespace":         func(in *Input) { in.Rule.ID = "secrets" },
		"rule without a name":              func(in *Input) { in.Rule.Name = "" },
		"live key on another rule": func(in *Input) {
			in.Rule = Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}
			in.Problem = "the key is " + stripeKey("sk", "Q1") + " here"
		},
		"unknown severity": func(in *Input) { in.Severity = Severity(lit) },
		"related span too short": func(in *Input) {
			in.Related = []Span{{URI: "b", StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2, Lines: []string{"ab"}}}
		},
		"secret_name off secrets.*": func(in *Input) {
			in.Rule = Rule{ID: "auth.unauthenticated-route", Name: "Every route needs sign-in"}
			in.SecretName = "X"
		},
		"literal where the secret_name": func(in *Input) { in.SecretName = "STRIPE_" + lit },
		"literal where the uri":         func(in *Input) { in.Location.URI = "file://" + lit },
		"literal where the ruleId":      func(in *Input) { in.Rule.ID = "secrets." + lit },
		"span splits the credential at punctuation": func(in *Input) {
			in.Location.Lines[0] = `k := "sk_live.AAAABBBBCCCCDDDDEEEE"; x`
			in.Location.StartColumn, in.Location.EndColumn = 15, 35
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := ok
			in.Location.Lines = append([]string(nil), ok.Location.Lines...)
			mutate(&in)
			_, err := New(in)
			if err == nil {
				t.Fatal("constructed a finding that should have been refused")
			}
			if strings.Contains(err.Error(), lit) {
				t.Fatalf("the error repeats producer text: %v", err)
			}
		})
	}
}

func TestNewRefusesInvalidUTF8(t *testing.T) {
	base := Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 6, EndLine: 1, EndColumn: 38, Lines: []string{`k := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"; x`}}}
	for name, mutate := range map[string]func(*Input){
		"a span line": func(in *Input) {
			in.Location.Lines[0] = "k := \"AAAA\xFFBBBBCCCCDDDDEEEE\"; x"
			in.Location.EndColumn = 29
		},
		"a span uri": func(in *Input) { in.Location.URI = "a\xFF.ts" },
		"the title":  func(in *Input) { in.Title = "the key is sk_live_\xFF" + strings.Repeat("Q", 24) },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := New(in); err == nil {
				t.Fatal("constructed a finding whose producer text is not valid UTF-8")
			}
		})
	}
}

func TestValidateRefusesInvalidUTF8(t *testing.T) {
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f",
		Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 6, EndLine: 1, EndColumn: 38, Lines: []string{`k := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123"; x`}}})
	if err != nil {
		t.Fatal(err)
	}
	// A poisoned producer string: invalid UTF-8 breaks every live-key match, and json
	// marshal sanitizes the byte away, so a serialized log would carry the rest of the key.
	got.Message.Text, got.Properties.Canyon.Title = "sk_live_\xFF"+strings.Repeat("Q", 24), "sk_live_\xFF"+strings.Repeat("Q", 24)
	l := Log{Version: Version, Schema: SchemaURI, Runs: []Run{{Tool: Tool{Driver: Driver{Name: "canyon/scanners", Rules: []Rule{clientBundle}}}, Results: []Result{got}}}}
	if err := l.Validate(); err == nil {
		t.Fatal("a log whose text is not valid UTF-8 must be refused")
	}
}

func TestNewRedactsAThreeLineSpan(t *testing.T) {
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 3, EndColumn: 10,
		Lines: []string{`a = "AAAABBBB`, `CCCCDDDD`, `EEEEFFFF";`}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Locations[0].PhysicalLocation.Region.Snippet.Text; s != "a = \"••••\";" {
		t.Errorf("the 26-rune literal is hidden whole, quotes kept: %q", s)
	}
}

// A tagged template — gql`…`, styled.div`…`, sql`…` — is one string literal token that begins
// at the backtick; the tag is not part of it. The character before the span is therefore the
// tag's last letter, which the word guard read as a span starting inside a word and refused,
// aborting the whole scan (canyonv2-xqx). The producer declares the delimiter its parser
// delimited, and a declared span is taken at its word.
func TestNewAcceptsATaggedTemplate(t *testing.T) {
	tok := "`" + strings.Repeat("A", 20) + "`"
	line := "const q = gql" + tok + ";"
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 14, EndLine: 1, EndColumn: 36, Lines: []string{line}, Delimited: tok}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Locations[0].PhysicalLocation.Region.Snippet.Text; s != "const q = gql`••••`;" {
		t.Errorf("the tagged template's literal is hidden whole, the tag and backticks kept: %q", s)
	}
}

// A template longer than the snippet cap holds its credential all the same, and templates that
// long are ordinary. The literal is masked whole, so what is serialized is one line however many
// the template spans — the cap binds the snippet as serialized, not the span's source lines.
// The body lines here are short on purpose: with the cap moved but the mask unchanged, mask keeps
// the first fifteen and last four runes of the literal, which are the template's own newlines,
// and the snippet is still five lines. This fails unless the mask collapses a literal that spans
// lines, which is the half of the fix that is easy to leave out.
func TestNewCollapsesATemplateOverThreeLines(t *testing.T) {
	body := []string{"`", "  a", "  b", "  " + strings.Repeat("c", 23), "`"}
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 14, EndLine: 5, EndColumn: 2, Delimited: strings.Join(body, "\n"),
		Lines: []string{"const q = gql`", "  a", "  b", "  " + strings.Repeat("c", 23), "`;"}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	s := got.Locations[0].PhysicalLocation.Region.Snippet.Text
	if s != "const q = gql`••••`;" {
		t.Errorf("a template over three lines masks whole to one line: %q", s)
	}
	if n := lines(s); n != 1 {
		t.Errorf("the serialized snippet is %d lines; the cap binds what is serialized", n)
	}
	if got.Locations[0].PhysicalLocation.Region.EndLine != 5 {
		t.Error("the region still names the whole literal; only the snippet collapses")
	}
}

// The word guard is not redundant once a declared span skips it: it is the only thing refusing a
// span that runs from one string's CLOSING delimiter to another's. unquote asks only that the
// first and last characters match, so such a span yields a literal, and the real credential ahead
// of it rides into the snippet unmasked. Nothing declares Quote here, so the guard stays.
func TestAnUndeclaredSpanBetweenTwoClosingQuotesIsRefused(t *testing.T) {
	line := `x = "AAAABBBBCCCC", b = "DDDDEEEEFF";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 18, EndLine: 1, EndColumn: 37, Lines: []string{line}}
	if _, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc}); err == nil {
		t.Fatal("a span between two closing quotes was accepted; the first character cannot tell an opening delimiter from a closing one")
	}
}

// Declaring the delimiter must be a stronger statement than the word guards, not a way around
// them. Matching first and last runes is not that statement: a span from one literal's closing
// delimiter to another's has both, and the credential lying between them never becomes the literal
// anything masks — it ships in the clear. The undeclared form of this span is refused above; the
// declared form must be refused too, and for the interior, not the ends.
func TestADeclaredSpanBetweenTwoClosingQuotesIsRefused(t *testing.T) {
	line := `x = "AAAABBBBCCCC", b = "DDDDEEEEFF";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 18, EndLine: 1, EndColumn: 37, Lines: []string{line}, Delimited: `", b = "DDDDEEEEFF"`}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err == nil {
		t.Fatalf("a declared span between two closing quotes was accepted; %q rode out in the clear",
			got.Locations[0].PhysicalLocation.Region.Snippet.Text)
	}
}

// An escaped delimiter inside the span is the literal's own, not a second token's, so the interior
// rule must not refuse it — otherwise every string carrying a quote fails the gate.
func TestADeclaredSpanMayCarryAnEscapedDelimiter(t *testing.T) {
	line := `k = "AAAA\"BBBBCCCCDDDDEEEE";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 29, Lines: []string{line}, Delimited: `"AAAA\"BBBBCCCCDDDDEEEE"`}
	if _, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc}); err != nil {
		t.Fatalf("a literal carrying an escaped delimiter was refused: %v", err)
	}
}

// mask hides a literal that carries a line terminator whole, and \n is not the only one. Span.check
// refuses \r and \n inside a line, so \n reaches a literal only through the multi-line join — but
// U+2028 sits inside a single line unremarked, and a consumer that honours it as a line separator
// renders a snippet of more lines than the cap ever counted. Hidden whole, there is nothing to
// render. Narrowing mask's check to \n alone leaves this green only if nothing pins it.
func TestMaskHidesALiteralCarryingAnyLineTerminator(t *testing.T) {
	lit := "AAAABBBBCCCC DDDDEEEEFFFFGGGG" // 29 runes: past the length that hides whole anyway
	line := `k = "` + lit + `";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 7 + len([]rune(lit)), Lines: []string{line}, Delimited: `"` + lit + `"`}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Locations[0].PhysicalLocation.Region.Snippet.Text; s != `k = "••••";` {
		t.Errorf("a literal carrying U+2028 is hidden whole, not head-and-tail: %q", s)
	}
}

// A span cut immediately after an escaped delimiter ends INSIDE the literal: the rune it stops on
// is the literal's own \" and not its terminator, so the rest of the credential stays in the
// snippet. Checking that the first and last runes are the delimiter cannot see that — the closing
// one's own escape parity has to be read.
func TestADeclaredTokenCutAtAnEscapedDelimiterIsRefused(t *testing.T) {
	line := `const t = "AAAABBBBCCCCDDDD\"EEEEFFFFGGGGHHHH";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 11, EndLine: 1, EndColumn: 30, Lines: []string{line}, Delimited: `"AAAABBBBCCCCDDDD\"`}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err == nil {
		t.Fatalf("a token cut at an escaped delimiter was accepted; %q kept the rest of the secret",
			got.Locations[0].PhysicalLocation.Region.Snippet.Text)
	}
}

// Declaring the DELIMITER lost what the tokenizer knew: `} TAILTAIL` opens and closes with a
// backtick and carries none in between, so it passes for a token although it begins on the closing
// backtick of a substitution. Declaring the TOKEN catches it, because the columns and the token
// have to be the same thing — a producer that names the real template here is refused for naming
// columns that cut out something else. What it cannot catch is a producer that fabricates both
// consistently; telling an opening delimiter from a closing one needs the lexer that read the
// file, which is why this is the producer's assertion and not the gate's verification.
func TestADeclaredTokenMustBeTheTextTheColumnsCutOut(t *testing.T) {
	line := "x = `SECRET!!!! ${`x`} TAILTAIL`;"
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 21, EndLine: 1, EndColumn: 33, Lines: []string{line},
		Delimited: "`SECRET!!!! ${`x`} TAILTAIL`"} // the real token the lexer would emit
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err == nil {
		t.Fatalf("a span whose columns cut out something other than the declared token was accepted; %q",
			got.Locations[0].PhysicalLocation.Region.Snippet.Text)
	}
}

// Refusing only word runes beside an undeclared span was not enough: a credential ends on whatever
// its alphabet ends on, and base64 padding, a JWT's dots and a UUID's dashes are not word runes. So
// a span from one literal's closing delimiter to the next literal's passed whenever the character
// before it was punctuation, and the credential ahead of it shipped in the clear. This is the path
// every external SARIF producer takes, since none of them can declare a token.
func TestAnUndeclaredSpanBesideANonWordRuneIsRefused(t *testing.T) {
	line := `const first = "AAAA1234$$$$"; const second = ".DDDDEEEE";`
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 28, EndLine: 1, EndColumn: 47, Lines: []string{line}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err == nil {
		t.Fatalf("a span starting on a closing delimiter was accepted; %q rode out in the clear",
			got.Locations[0].PhysicalLocation.Region.Snippet.Text)
	}
}

// The allowlist has to let ordinary code through, or an undeclared producer refuses everything and
// the run dies instead. A literal in a call, an object, an array and a concatenation each abut
// punctuation an expression is built from.
// The runes a credential is built from cannot be allowed to abut an undeclared span, whatever
// else they mean in the language: `+`, `/` and `=` are base64, `-` is a UUID's, and each of these
// spans runs from one literal's closing delimiter to the next one's opening delimiter with a
// credential's own last and first characters as its neighbours. Admitting them as operators is
// what let these through. The cost is that the unspaced forms — `a+"x"+b`, `a="x"` — are refused
// when nothing declared the token; the spaced forms above still pass, prettier writes the spaced
// form, and an in-tree producer declares rather than relying on either.
// mask() hides a literal carrying U+2028 or U+2029 whole, so the ≤3-line cap has to count them
// as terminators too — otherwise a snippet a consumer renders over four lines passes a cap that
// exists to bound exactly that. contracts/validate.py's snippet_lines is the same function.
func TestTheSnippetCapCountsEveryLineTerminator(t *testing.T) {
	for text, want := range map[string]int{
		"a": 1, "a\u2028b": 2, "a\u2029b": 2, "a\u2028b\u2029c\u2028d": 4, "a\u2028": 1, "a\r\nb": 2,
	} {
		if got := lines(text); got != want {
			t.Errorf("lines(%q) = %d; want %d — gate/finding and contracts/validate.py must agree", text, got, want)
		}
	}
}

func TestAnUndeclaredSpanIsRefusedAgainstACredentialAlphabet(t *testing.T) {
	for name, line := range map[string]string{
		"base64 plus and slash": `const first = "AAAA1234PPPP+"; const second = "/DDDDEEEE";`,
		"base64 padding":        `const first = "AAAA1234PPPP="; const second = "=DDDDEEEE";`,
		"a UUID dash":           `const first = "AAAA1234BBBB-"; const second = "-DDDDEEEE";`,
		"a JWT dot":             `const first = "AAAA1234$$$$"; const second = ".DDDDEEEE";`,
	} {
		t.Run(name, func(t *testing.T) {
			closing := strings.Index(line, `";`)
			opening := closing + 1 + strings.Index(line[closing+1:], `"`)
			loc := Span{URI: "a.ts", StartLine: 1, StartColumn: closing + 1, EndLine: 1,
				EndColumn: opening + 2, Lines: []string{line}}
			res, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
			if err == nil {
				t.Fatalf("accepted; snippet %q keeps a credential",
					res.Locations[0].PhysicalLocation.Region.Snippet.Text)
			}
		})
	}
}

// An undeclared span is still a claim that this is one string literal, so it is held to the same
// token rules as a declared one. These two were accepted while those rules were gated on a
// declaration, and each leaves part of a real credential outside the mask.
func TestAnUndeclaredSpanMustAlsoBeOneToken(t *testing.T) {
	for name, c := range map[string]struct {
		line   string
		sc, ec int
		secret string
	}{
		"ending on an escaped delimiter": {
			line: `const t = "AAAABBBBCCCCDDDD\" EEEEFFFFGGGGHHHH";`, sc: 11, ec: 30, secret: "EEEEFFFFGGGGHHHH"},
		"closing on a later token": {
			line: `x = "PREFIXSECRETONE1234567890" + "TAILSECRETTWO1234567890";`, sc: 5, ec: 60, secret: "PREFIXSECRETONE"},
	} {
		t.Run(name, func(t *testing.T) {
			loc := Span{URI: "a.ts", StartLine: 1, StartColumn: c.sc, EndLine: 1, EndColumn: c.ec, Lines: []string{c.line}}
			res, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
			if err == nil {
				t.Fatalf("accepted; snippet %q leaves %q in the clear",
					res.Locations[0].PhysicalLocation.Region.Snippet.Text, c.secret)
			}
		})
	}
}

func TestAnUndeclaredSpanMayAbutOrdinaryPunctuation(t *testing.T) {
	lit := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for _, c := range []struct{ name, before, after string }{
		{"a call", "f(", ")"},
		{"an object", "{k:", "}"},
		{"an array", "[", "]"},
		{"a concatenation", "a + ", " + b"},
		{"a statement", "k = ", ";"},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := c.before + `"` + lit + `"` + c.after
			start := len([]rune(c.before)) + 1
			loc := Span{URI: "a.ts", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + len(lit) + 2, Lines: []string{line}}
			if _, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc}); err != nil {
				t.Fatalf("%s was refused: %v", c.name, err)
			}
		})
	}
}

func TestMinLiteralBoundary(t *testing.T) {
	for n, want := range map[int]bool{8: true, 7: false} {
		lit := strings.Repeat("A", n)
		_, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f",
			Location: Span{URI: "a.ts", StartLine: 1, StartColumn: 6, EndLine: 1, EndColumn: 8 + n, Lines: []string{"k := \"" + lit + "\""}}})
		if want != (err == nil) {
			t.Errorf("a %d-rune literal: err %v, want refused=%v", n, err, !want)
		}
	}
}

// A masked credential on a minified line: the snippet is redacted first and then windowed, the key
// appears nowhere, the region stays on the file, and canyon.shown is the masked literal in the window.
func TestAMaskedLiteralOnAMinifiedLineIsWindowed(t *testing.T) {
	key := `"sk_live_` + strings.Repeat("Q", 24) + `"`
	line := "var a=" + strings.Repeat("x,", 750) + key + ";" + strings.Repeat("y,", 250)
	start := strings.Index(line, key) + 1
	loc := Span{URI: "a.js", StartLine: 1, StartColumn: start, EndLine: 1, EndColumn: start + len(key), Lines: []string{line}}
	got, err := New(Input{Rule: clientBundle, Severity: Critical, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	g, at := got.Locations[0].PhysicalLocation.Region, got.Properties.Canyon.Shown
	shown := []rune(g.Snippet.Text)
	if len(shown) > maxSnippet || strings.Contains(g.Snippet.Text, "QQQQQQQQ") || g.StartColumn != start || g.EndColumn != start+len(key) {
		t.Fatalf("windowed after redaction, region on the file: %d..%d %q", g.StartColumn, g.EndColumn, g.Snippet.Text)
	}
	if at == nil || string(shown[at.StartColumn-1:at.EndColumn-1]) != `"`+mask(key[1:len(key)-1])+`"` {
		t.Fatalf("canyon.shown is the masked literal in the window: %+v", at)
	}
}

// One minified line is never a refused finding (canyonv2-9ly): its snippet is a window of the line, at
// most 400 characters, cut with an ellipsis, and canyon.shown puts the caret on the span in it while
// the region stays on the file.
func TestAMinifiedLineIsWindowedNotRefused(t *testing.T) {
	const marker = "LEAK:toy"
	line := strings.Repeat("a;", 750) + marker + strings.Repeat("b;", 250)
	loc := Span{URI: "src/min.ts", StartLine: 1, StartColumn: 1501, EndLine: 1, EndColumn: 1501 + len(marker), Lines: []string{line}}
	got, err := New(Input{Rule: Rule{ID: "perf.unbounded-refetch", Name: "Bounded refetch"}, Severity: High, Title: "t", Problem: "p", Fix: "f", Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	g, at := got.Locations[0].PhysicalLocation.Region, got.Properties.Canyon.Shown
	shown := []rune(g.Snippet.Text)
	if len(shown) > 400 || !strings.HasPrefix(g.Snippet.Text, "…") || !strings.HasSuffix(g.Snippet.Text, "…") || g.StartColumn != 1501 {
		t.Fatalf("a %d-character line is windowed and the region stays on the file: column %d, snippet %q", len(line), g.StartColumn, g.Snippet.Text)
	}
	if at == nil || string(shown[at.StartColumn-1:at.EndColumn-1]) != marker {
		t.Fatalf("canyon.shown is the span's place in the window: %+v", at)
	}
}

// The golden Stripe finding is gitleaks', imported (contracts/finding.md, In: external SARIF): its
// region is the whole of payments.ts:14 and the snippet the mask alone, the caret on the mask.
func TestTheGoldenStripeIsHiddenWhole(t *testing.T) {
	var log Log
	if err := json.Unmarshal(contracts.GoldenLog, &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[slices.IndexFunc(log.Runs, func(r Run) bool { return r.Tool.Driver.Name == "sarif/gitleaks" })]
	res := run.Results[0]
	g, at := res.Locations[0].PhysicalLocation.Region, res.Properties.Canyon.Shown
	if !strings.HasPrefix(res.RuleID, clientBundle.ID+".gitleaks.") || g.StartLine != 14 || g.StartColumn != 1 || g.Snippet.Text != "••••" ||
		at == nil || at.StartColumn != 1 || at.EndColumn != 5 {
		t.Fatalf("the hero is hidden whole: %s %+v %+v", res.RuleID, g, at)
	}
}

// An imported region is hidden whole (contracts/finding.md, In: external SARIF): one line whatever it
// spans, the first line's part cut after the last terminator before the region and the last line's
// before the first after it — U+2028 and U+2029 here, which the snippet cap counts — shown the mask's
// four columns while the region stays on the file, and a live key left beside it masked as on secrets.*.
func TestNewHidesAnImportedRegionWhole(t *testing.T) {
	key := stripeKey("sk", "Q1")
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 7, EndLine: 3, EndColumn: 2,
		Lines: []string{"x\u2028q = f(", "  secret,", `) + "` + key + "\"\u2029z"}}
	got, err := New(Input{Rule: Rule{ID: "sarif.semgrep.x", Name: "Findings from semgrep"}, Severity: Low, Title: "t", Problem: "p", Fix: "f", Hidden: true, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	g, at := got.Locations[0].PhysicalLocation.Region, got.Properties.Canyon.Shown
	if want := `q = •••• + "` + mask(key) + `"`; g.Snippet.Text != want {
		t.Errorf("hidden whole on one line: %q, want %q", g.Snippet.Text, want)
	}
	if at == nil || *at != (Shown{StartColumn: 5, EndColumn: 9}) || g.StartColumn != 7 || g.EndLine != 3 || g.EndColumn != 2 {
		t.Errorf("shown is the mask, the region the file's: %+v on %+v", at, g)
	}
}

// An imported region past three lines shows its first three, a live key in them masked, the caret to
// the third line's end and the region the file's (#1834: ESLint's seven-line call printed ••••;). A key
// the third line's end cuts is refused, so the attach hides that region whole.
func TestNewShowsAnImportedRegionsFirstThreeLines(t *testing.T) {
	key := stripeKey("sk", "Q2")
	rule := Rule{ID: "sarif.eslint.security/detect-non-literal-fs-filename", Name: "Findings from eslint"}
	loc := Span{URI: "server.js", StartLine: 9, StartColumn: 3, EndLine: 13, EndColumn: 5,
		Lines: []string{"  fs.readFile(path.join(dir, file), (err, body) => {", `    const k = "` + key + `";`, "    if (err) {", "      return;", "  });"}}
	got, err := New(Input{Rule: rule, Severity: Low, Title: "t", Problem: "p", Fix: "f", Cut: true, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	g, at := got.Locations[0].PhysicalLocation.Region, got.Properties.Canyon.Shown
	if want := "  fs.readFile(path.join(dir, file), (err, body) => {\n    const k = \"" + mask(key) + "\";\n    if (err) {"; g.Snippet.Text != want {
		t.Errorf("snippet %q, want %q", g.Snippet.Text, want)
	}
	if at == nil || *at != (Shown{StartColumn: 3, EndColumn: 15}) || g.StartLine != 9 || g.EndLine != 13 || g.EndColumn != 5 {
		t.Errorf("the caret to the third line's end, the region the file's: %+v on %+v", at, g)
	}
	pem := "-----BEGIN RSA " + "PRIVATE KEY-----"
	body := "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	cut := Span{URI: "k.js", StartLine: 1, StartColumn: 1, EndLine: 6, EndColumn: 3,
		Lines: []string{"const k = `", pem, body, body, "-----END RSA " + "PRIVATE KEY-----", "`;"}}
	if _, err := New(Input{Rule: rule, Severity: Low, Title: "t", Problem: "p", Fix: "f", Cut: true, Location: cut}); err == nil {
		t.Error("a key the third line's end cuts was shown in part")
	}
	if _, err := New(Input{Rule: rule, Severity: Low, Title: "t", Problem: "p", Fix: "f", Hidden: true, Cut: true, Location: cut}); err != nil {
		t.Errorf("hidden whole, the same key is masked, never refused: %v", err) // the retry the attach makes, and a gitleaks key's first try
	}
}

// An imported region's lines are placed by SARIF's default newlines, \r\n and \n, so a lone CR stays
// inside its line; hidden, the snippet is cut at it and it never serializes. A span that is not
// hidden serializes its lines, so a CR in one is still refused.
func TestAHiddenSpanMayCarryALoneCR(t *testing.T) {
	rule := Rule{ID: "sarif.semgrep.x", Name: "Findings from semgrep"}
	loc := Span{URI: "a.ts", StartLine: 1, StartColumn: 7, EndLine: 1, EndColumn: 11, Lines: []string{"a\rb = f(x);\ry"}}
	got, err := New(Input{Rule: rule, Severity: Low, Title: "t", Problem: "p", Fix: "f", Hidden: true, Location: loc})
	if err != nil {
		t.Fatalf("a hidden span on lines carrying a lone CR: %v", err)
	}
	if text := got.Locations[0].PhysicalLocation.Region.Snippet.Text; text != "b = ••••;" {
		t.Errorf("the snippet is not cut at the CRs: %q", text)
	}
	shown := Span{URI: "a.ts", StartLine: 1, StartColumn: 5, EndLine: 1, EndColumn: 9, Lines: []string{"b = f(x);\r"}}
	if _, err := New(Input{Rule: rule, Severity: Low, Title: "t", Problem: "p", Fix: "f", Location: shown}); err == nil {
		t.Error("a span that is not hidden carried a CR into its snippet")
	}
}
