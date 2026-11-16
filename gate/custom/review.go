package custom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Checked is a rule of the snapshot that the scanners' run checks by code: a custom rule that is on and
// checked by code (contracts/snapshot.md, Custom rules).
func Checked(r policy.Rule) bool {
	return strings.HasPrefix(r.ID, "custom.") && r.On() && r.Check != nil && r.Check.By == ByCode
}

// Review is the scanners' run's results of the snapshot's custom rules checked by code
// (contracts/finding.md, A custom rule checked by code), over the tree's facts and the app's secrets,
// and the rules they carry: none for a rule that holds, one for a rule that does not hold or could not
// be checked, each at the tree's first file, line 1, columns 1–2, fingerprinted with the facts it read
// or why it could not be checked, so an approval of the one never covers the other.
// ReadsDependencies is whether a rule of rules that is on and checked by code reads the dependencies:
// only then is trivy asked for the packages. A rule whose check cannot run reads no fact.
func ReadsDependencies(rules []policy.Rule) bool {
	return slices.ContainsFunc(rules, func(r policy.Rule) bool {
		if !Checked(r) || r.Check.FactsVersion != FactsVersion {
			return false
		}
		p, err := Compile(r.Check.Expr)
		return err == nil && slices.Contains(p.facts, "dependencies")
	})
}

func Review(ctx context.Context, tree gate.Tree, rules []policy.Rule, secrets []Secret, pkgs Packages) ([]finding.Result, []finding.Rule, error) {
	var checked []policy.Rule
	for _, r := range rules {
		if Checked(r) {
			checked = append(checked, r)
		}
	}
	if len(checked) == 0 {
		return nil, nil, nil
	}
	files := tree.Files()
	if len(files) == 0 {
		return nil, nil, errors.New("custom: a custom rule's result needs a file to sit at")
	}
	return reviewFacts(ctx, checked, ReadFacts(tree, secrets, pkgs), files[0])
}

// reviewFacts is Review on facts already read, each result at the file at.
func reviewFacts(ctx context.Context, checked []policy.Rule, facts Facts, at string) ([]finding.Result, []finding.Rule, error) {
	var results []finding.Result
	var digests []string
	var declared []finding.Rule
	for _, r := range checked {
		in, digest, ok := outcome(ctx, r, facts)
		if !ok {
			continue
		}
		in.Location = finding.Span{URI: at, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2, Lines: in.Location.Lines}
		res, err := finding.New(in)
		if err != nil {
			return nil, nil, fmt.Errorf("custom: %s: %w", r.ID, err)
		}
		results, digests = append(results, res), append(digests, digest)
		declared = append(declared, finding.Rule{ID: r.ID, Name: r.Name})
	}
	gate.FingerprintCustom(results, digests)
	return results, declared, nil
}

// outcome is rule's result on the facts, but its location's place, and the digest its fingerprint
// frames; false when the rule holds.
func outcome(ctx context.Context, r policy.Rule, f Facts) (finding.Input, string, bool) {
	p, v, why := judged(ctx, r, f)
	switch v.Result {
	case Pass:
		return finding.Input{}, "", false
	case Fail:
		return failed(r), digest(p.read(f)), true
	}
	if key, ok := f.keys[v.Missing]; ok && v.Missing != "" { // a count past a bound: the approval follows the reason, not the count
		sum := sha256.Sum256([]byte("could not check: " + v.Missing + ": " + key))
		return unchecked(r, why), hex.EncodeToString(sum[:]), true
	}
	sum := sha256.Sum256([]byte("could not check: " + why))
	return unchecked(r, why), hex.EncodeToString(sum[:]), true
}

// Judge is r's check on f as a review judges it: Pass, Fail, or Unchecked and why, in the words a
// could-not-check finding carries (contracts/finding.md, A custom rule checked by code).
func Judge(ctx context.Context, r policy.Rule, f Facts) (result, why string) {
	_, v, why := judged(ctx, r, f)
	return v.Result, why
}

// judged is r's check on f: its program when it compiled, its verdict, and why it could not be checked.
func judged(ctx context.Context, r policy.Rule, f Facts) (*Program, Verdict, string) {
	if r.Check.FactsVersion != FactsVersion {
		return nil, Verdict{Result: Unchecked}, "this rule was saved for an older Canyon: an admin saves it again"
	}
	p, err := Compile(r.Check.Expr)
	if err != nil {
		return nil, Verdict{Result: Unchecked}, "this Canyon can't run this rule's check: an admin saves it again"
	}
	switch v := p.Evaluate(ctx, f); {
	case v.Result == Pass || v.Result == Fail:
		return p, v, ""
	case v.Undirected:
		return p, v, v.Why
	case v.Stopped:
		return p, v, fmt.Sprintf("it did not finish within %d seconds", int(EvalDeadline.Seconds()))
	case v.Missing != "":
		return p, v, v.Missing + ": " + f.Missing[v.Missing]
	default:
		return p, v, "its evaluation failed" // CEL's message may quote a value the evaluation read
	}
}

// failed is the result of rule not holding.
func failed(r policy.Rule) finding.Input {
	says := r.Check.Says
	return finding.Input{Rule: finding.Rule{ID: r.ID, Name: r.Name}, Severity: finding.High,
		Title:            r.Name,
		Problem:          fmt.Sprintf("%q (%s) does not hold for this app: Canyon checked it by code: %s", r.Name, r.ID, says),
		Fix:              "Change the app so that this holds: " + says,
		RequiredEndState: fmt.Sprintf("Canyon's check of %s holds for this app when the review runs again: %s", r.ID, says),
		DoNot: []string{"Rename or move what the check reads just so it stops matching, without doing what the rule asks.",
			"Ask an admin to switch the rule to the AI review just to get past this finding."},
		Location: finding.Span{Lines: []string{r.ID + " checked by code: " + says}}}
}

// unchecked is the result of rule that could not be checked, and why.
func unchecked(r policy.Rule, why string) finding.Input {
	return finding.Input{Rule: finding.Rule{ID: r.ID, Name: r.Name}, Severity: finding.High,
		Title:            fmt.Sprintf("Canyon could not check %q", r.Name),
		Problem:          "could not check: " + r.ID + ": " + why,
		Fix:              "Change the app so Canyon can read what this rule checks, or have an admin switch the rule to the AI review.",
		RequiredEndState: "Canyon checks " + r.ID + " by code when the review runs again.",
		DoNot:            []string{"Remove the file Canyon could not read just to get past this finding."},
		Location:         finding.Span{Lines: []string{r.ID + " could not be checked by code"}}}
}

// digest is the lowercase hex sha256 of the facts an expression read as one JSON object, keys in byte
// order.
func digest(read map[string]any) string {
	b, _ := json.Marshal(read) // facts always marshal; a map's keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
