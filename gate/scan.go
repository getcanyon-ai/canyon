// Package gate is the review gate's engine (Track A): it walks a package tree and assembles the
// scanners' run of the findings log (seam 1), into which the tools Canyon runs report what they could
// not check, then the reviewer's. Findings are built with finding.New — redaction happens at
// construction.
package gate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// DriverVersion is the scanner producer's version in the assembled log (contracts/finding.md);
// its name is policy.Scanner.Driver(), the one table the validator reads too.
const DriverVersion = "0.1.0"

// dockerfileNote is the scanners' run's note when the tree's root holds a Dockerfile: buildpacks build
// the package, so it is not what runs (contracts/finding.md). It says nothing went unchecked.
const dockerfileNote = "Canyon builds with buildpacks and does not read this Dockerfile"

// Tree is the package tree a scan reads: every regular file, by its slash-separated path
// relative to the root, in sorted order.
type Tree struct {
	root  string
	files []string
}

// Walk enumerates the tree at root, a package as ingest.Extract materialised it: the scanners'
// run reads it so, and so does the attach of an imported report (finding.md, In: external SARIF).
func Walk(root string) (Tree, error) {
	t := Tree{root: root}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() { // ingest materialises regular files only: anything else is not a package tree
			return fmt.Errorf("gate: %s is not a regular file", filepath.ToSlash(rel))
		}
		t.files = append(t.files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return Tree{}, err
	}
	slices.Sort(t.files) // WalkDir's order is per directory: a/b.ts comes before a.ts
	return t, nil
}

// Files returns the tree's files, sorted.
func (t Tree) Files() []string { return slices.Clone(t.files) }

// Read reads one file by its tree path; a path the walk did not list is refused.
func (t Tree) Read(rel string) ([]byte, error) {
	if _, listed := slices.BinarySearch(t.files, rel); !listed {
		return nil, fmt.Errorf("gate: %s is not in the tree", rel)
	}
	return os.ReadFile(filepath.Join(t.root, filepath.FromSlash(rel)))
}

// Check is one check's line on the run's timeline (contracts/events.md, check.*): Status empty
// when it starts, then clean, findings, warning, skipped or failed with its result count and time.
type Check struct {
	Name     string
	Status   string
	Findings int
	Duration time.Duration
}

// Report writes a Check to the run's timeline; an error fails the run.
type Report func(ctx context.Context, c Check) error

// Scanners is the scanners' run over the tree at root before any tool reports into it
// (contracts/finding.md): no rule declared until a result needs it — design.org-system's (Design) or a
// gate.unreadable one — its warnings notChecked, the enabled rules nothing on this host checks, then what
// nothing read (Coverage, then Design). A tree
// with no file — the wrong directory after ingest.Extract, say — is unknown state, never a clean log
// (canyonv2-3kj).
func Scanners(root string, snap policy.Snapshot, notChecked []string, model bool) (finding.Log, error) {
	tree, err := Walk(root)
	if err != nil {
		return finding.Log{}, err
	}
	if len(tree.files) == 0 {
		return finding.Log{}, fmt.Errorf("gate: %s holds no file", root)
	}
	notes, err := Coverage(tree, snap, model)
	if err != nil {
		return finding.Log{}, err
	}
	design, said, err := Design(tree, snap)
	if err != nil {
		return finding.Log{}, err
	}
	notes = append(notes, said...)
	if len(notChecked) > 0 {
		notes = append([]finding.Notification{{Level: "warning", Message: finding.Message{Text: "not checked on this host: " + strings.Join(notChecked, ", ")}}}, notes...)
	}
	if _, root := slices.BinarySearch(tree.files, "Dockerfile"); root { // a builder believes it is what runs (#1243)
		notes = append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: dockerfileNote}})
	}
	run := finding.Run{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: finding.Driver{Name: policy.Scanner.Driver(), Version: DriverVersion, Rules: []finding.Rule{}}}, Results: []finding.Result{}}
	if len(design) > 0 {
		run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, finding.Rule{ID: design[0].RuleID, Name: design[0].Properties.Canyon.PolicyName})
		run.Results = append(run.Results, design...)
		fingerprintRun(run.Results)
	}
	if len(notes) > 0 {
		run.Invocations = []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: notes}}
	}
	return finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{run}}, nil
}

// CouldNotCheck adds to log's scanners' run the one blocking gate.unreadable finding of a tool Canyon
// runs whose scan did not finish, why its reason in the contract's words (contracts/finding.md,
// Canyon-run tools): at the tree's first file, line 1, columns 1–2, its snippet the digest of the
// tool's name, framed as a path is, then every file, framed, so an approval covers that tool on that
// tree only.
func CouldNotCheck(log *finding.Log, tree Tree, tool, why string) error {
	i := slices.IndexFunc(log.Runs, func(r finding.Run) bool { return r.Tool.Driver.Name == policy.Scanner.Driver() })
	if i < 0 || len(tree.files) == 0 {
		return errors.New("gate: a could-not-check finding needs the scanners' run and a file to sit at")
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%s", len(tool), tool) // a hash never fails a write
	for _, f := range tree.files {
		src, err := tree.Read(f)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%d:%s%x", len(f), f, sha256.Sum256(src))
	}
	res, err := finding.New(finding.Input{Rule: finding.Unreadable, Severity: finding.High,
		Title: "Canyon could not check this package with " + tool, Problem: "could not check: " + tool + ": " + why,
		Fix:              "Review the package again once " + tool + " can run, or have " + approvers + " approve it with a reason.",
		RequiredEndState: "Canyon checks the package with " + tool + ".",
		DoNot:            []string{"turn " + tool + " off just to get past this finding"},
		Location: finding.Span{URI: tree.files[0], StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2,
			Lines: []string{fmt.Sprintf("(%s could not check this tree; tree sha256:%x)", tool, h.Sum(nil))}}})
	if err != nil {
		return fmt.Errorf("%s: %w", finding.Unreadable.ID, err)
	}
	run := &log.Runs[i]
	if !slices.ContainsFunc(run.Tool.Driver.Rules, func(r finding.Rule) bool { return r.ID == finding.Unreadable.ID }) {
		run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, finding.Unreadable)
	}
	run.Results = append(run.Results, res)
	fingerprintRun(run.Results)
	return nil
}

// approvers is who may approve a gate.unreadable finding, a blocking one, as design/PERMISSIONS.md has it: an
// admin or a responder, never the person whose session started the run, nor the agent working under it.
const approvers = "an admin or a security responder who didn't start this review"

// fingerprintRun derives every result's canyon/v1 fingerprint by contracts/finding.md's
// recipe, last, over the assembled results: sha256 of the rule id, the uri, the snippet text
// exactly as it serializes — the mask included, which is why this runs after construction —
// the region's start column, and the occurrence, each framed as its UTF-8 byte length, a colon,
// then the field. len() of a Go string is already bytes, the unit the contract names; framing is
// what keeps a uri carrying a newline, or a multi-line snippet, from serializing like some other
// finding. The occurrence is the 0-based index, in log order, among the results sharing the
// other four, so only an identical line can renumber a finding (canyonv2-fb7). The whole
// digest: a short one collides and fails a good run closed.
func fingerprintRun(results []finding.Result) { fingerprint(results, nil, map[string]int{}) }

// FingerprintCustom derives canyon/v1 on a custom rule's results checked by code, in the scanners' run:
// the recipe's five fields, the uri framed empty since such a result sits at whichever file is first,
// then one more, digests' (contracts/finding.md: the sha256 of the facts the expression read, or of why
// it could not be checked), so an approval follows those facts or that reason. No other result of the
// run carries a custom.* rule, so none shares a key with these.
func FingerprintCustom(results []finding.Result, digests []string) {
	placeless := slices.Clone(results)
	for i := range placeless {
		placeless[i].Locations = slices.Clone(results[i].Locations)
		placeless[i].Locations[0].PhysicalLocation.ArtifactLocation.URI = ""
	}
	fingerprint(placeless, digests, map[string]int{})
	for i := range results {
		results[i].PartialFingerprints.CanyonV1 = placeless[i].PartialFingerprints.CanyonV1
	}
}

// FingerprintImported derives canyon/v1 on every imported tool's run (sarif/<tool>) of the assembled
// log: the recipe's five fields, then one more, the lowercase hex HMAC-SHA-256 under the project's
// key of the region's bytes as they stand in the package's file, none on a file that is not valid
// UTF-8 (contracts/finding.md). The digest is stored nowhere, and an edit inside the region moves the
// fingerprint, whether its snippet is shown or hidden whole.
// The occurrence counts among the log's results, across runs, so one tool's two categories carrying
// one identical result are two findings, never one fingerprint twice. Canyon's own runs are left as
// their producers derived them: none carries a sarif.<tool>.* rule, so none shares a key with an
// imported result. The key is the caller's: 32 bytes wherever a region is digested, anything else
// fails closed, since a fingerprint over a short key is a commitment a low-entropy secret could be
// guessed against.
func FingerprintImported(log *finding.Log, tree Tree, key []byte) error {
	seen := map[string]int{}
	for i := range log.Runs {
		if p, _ := policy.ByDriver(log.Runs[i].Tool.Driver.Name); p != policy.Sarif {
			continue
		}
		results := log.Runs[i].Results
		digests := make([]string, len(results))
		for j, r := range results {
			if len(key) != 32 {
				return errors.New("gate: a project key is 32 bytes")
			}
			physical := r.Locations[0].PhysicalLocation
			src, err := tree.Read(physical.ArtifactLocation.URI)
			if err != nil {
				return err
			}
			var t []byte // empty on a file that is not valid UTF-8, of which nothing is read (finding.md)
			if utf8.Valid(src) {
				if t, err = regionBytes(src, physical.Region); err != nil {
					return fmt.Errorf("gate: run %d imported result %d: %w", i, j, err)
				}
			}
			mac := hmac.New(sha256.New, key)
			_, _ = mac.Write(t) // a hash never fails a write
			digests[j] = hex.EncodeToString(mac.Sum(nil))
		}
		fingerprint(results, digests, seen)
	}
	return nil
}

// fingerprint writes canyon/v1 on every result, counting the occurrence in seen; digests, when not
// nil, is each result's framed sixth field.
func fingerprint(results []finding.Result, digests []string, seen map[string]int) {
	frame := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	for i := range results {
		physical := results[i].Locations[0].PhysicalLocation
		key := frame(results[i].RuleID) + frame(physical.ArtifactLocation.URI) +
			frame(physical.Region.Snippet.Text) + frame(strconv.Itoa(physical.Region.StartColumn))
		fields := key + frame(strconv.Itoa(seen[key]))
		if digests != nil {
			fields += frame(digests[i])
		}
		seen[key]++
		results[i].PartialFingerprints.CanyonV1 = fmt.Sprintf("f_%x", sha256.Sum256([]byte(fields)))
	}
}

// regionBytes is the region's bytes in src, line terminators as stored: lines placed by SARIF's
// default newlineSequences, \r\n and \n, which an imported region is restated under, and columns
// counted in code points.
func regionBytes(src []byte, g finding.Region) ([]byte, error) {
	start, err := offset(src, g.StartLine, g.StartColumn)
	if err != nil {
		return nil, err
	}
	end, err := offset(src, g.EndLine, g.EndColumn)
	if err != nil {
		return nil, err
	}
	if end < start {
		return nil, errors.New("the region ends before it starts")
	}
	return src[start:end], nil
}

// offset is the byte offset of line:column in src, both 1-based; the column may be one past the
// line's last character.
func offset(src []byte, line, column int) (int, error) {
	at := 0
	for range line - 1 {
		n := bytes.IndexByte(src[at:], '\n')
		if n < 0 {
			return 0, errors.New("the region is past the end of its file")
		}
		at += n + 1
	}
	end := len(src)
	if n := bytes.IndexByte(src[at:], '\n'); n >= 0 {
		end = at + n
		if n > 0 && src[end-1] == '\r' {
			end--
		}
	}
	for range column - 1 {
		if at >= end {
			return 0, errors.New("the region is past the end of its line")
		}
		_, size := utf8.DecodeRune(src[at:end])
		at += size
	}
	return at, nil
}
