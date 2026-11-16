package gate

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

func marshal(t *testing.T, l finding.Log) []byte {
	t.Helper()
	out, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustSchemas validates the log and runs both seam-1 schemas on its serialized bytes: the
// lexical half of the profile is the schemas' job (contracts/finding.md), and it runs here
// on the gate's own output.
func mustSchemas(t *testing.T, l finding.Log) []byte {
	t.Helper()
	if err := l.Validate(); err != nil {
		t.Fatalf("the log fails the profile: %v", err)
	}
	raw := marshal(t, l)
	if err := contracts.CheckFindingSchemas(raw); err != nil {
		t.Fatalf("the log fails the seam-1 schemas: %v", err)
	}
	return raw
}

// The scanners' run before any tool reports into it: one run, canyon/scanners at the gate's version,
// columns in code points, no rule declared and no result, and a log both seam-1 schemas pass.
func TestTheScannersRun(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	log, err := Scanners("testdata/toy/must-pass", snap, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	raw := mustSchemas(t, log)
	run := log.Runs[0]
	if d := run.Tool.Driver; len(log.Runs) != 1 || d.Name != "canyon/scanners" || d.Version != DriverVersion || len(d.Rules) != 0 || len(run.Results) != 0 {
		t.Fatalf("one run, named canyon/scanners at %q, nothing declared: %s", DriverVersion, raw)
	}
	// Every run says how its columns are counted, so GitHub and every other SARIF reader places a span
	// where Canyon does: SARIF's own default is UTF-16 code units.
	if !strings.Contains(string(raw), `"columnKind":"unicodeCodePoints"`) {
		t.Fatalf("columnKind does not serialize: %s", raw)
	}
	if run.Invocations != nil {
		t.Fatalf("with every rule checked and everything read the run carries no notification: %+v", run.Invocations)
	}
}

// A rule nothing here checks is named on the run's first notification — a warning, never a clean run
// that reads as that rule checked — before what nothing read.
func TestAnUncheckedRuleIsNamedOnTheRun(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	log, err := Scanners("testdata/toy/must-pass", snap, []string{"sql.string-concat", "code.injection"}, false)
	if err != nil {
		t.Fatal(err)
	}
	mustSchemas(t, log)
	var notes []string
	for _, inv := range log.Runs[0].Invocations {
		for _, n := range inv.ToolExecutionNotifications {
			notes = append(notes, n.Level+" "+n.Message.Text)
		}
	}
	if len(notes) < 2 || notes[0] != "warning not checked on this host: sql.string-concat, code.injection" {
		t.Fatalf("the unchecked rules come first, then what nothing read: %q", notes)
	}
}

// A Dockerfile at the tree's root is named as unread, one note-level line and never a not checked
// one, so it neither blocks nor sets the run's not_checked; one below the root, or named in another
// case, is not (contracts/finding.md, #1243).
func TestARootDockerfileIsNamedAsUnread(t *testing.T) {
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Dockerfile":        "note Canyon builds with buildpacks and does not read this Dockerfile",
		"docker/Dockerfile": "",
		"dockerfile":        "",
	} {
		dir := t.TempDir()
		for _, f := range []string{"index.html", name} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		log, err := Scanners(dir, snap, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		mustSchemas(t, log)
		var notes []string
		for _, inv := range log.Runs[0].Invocations {
			for _, n := range inv.ToolExecutionNotifications {
				notes = append(notes, n.Level+" "+n.Message.Text)
			}
		}
		if got := strings.Join(notes, "\n"); got != want {
			t.Errorf("%s: the run says %q, want %q", name, got, want)
		}
	}
}

// A tree with no file — the wrong directory after ingest.Extract — is unknown state, not a clean
// package: refused, never a valid empty log (canyonv2-3kj).
func TestAnEmptyTreeIsRefused(t *testing.T) {
	if log, err := Scanners(t.TempDir(), policy.Snapshot{}, nil, true); err == nil {
		t.Fatalf("an empty tree answered a log: %+v", log)
	}
}

// The canyon/v1 fingerprint is contracts/finding.md's recipe, derived last over the final
// log: the rule, the uri, the snippet text, the start column and the occurrence among results
// sharing all four (canyonv2-fb7). A sibling that differs in any of them never renumbers a
// finding, a different finding on another column never takes over one it approved, and two
// identical lines are told apart.
func TestFingerprintsFollowTheRecipe(t *testing.T) {
	const uri, text = "src/app.ts", "x x"
	rule := finding.Rule{ID: "sql.string-concat", Name: "No hand-built SQL from user input"}
	at := func(line, column int) finding.Span {
		return finding.Span{URI: uri, StartLine: line, StartColumn: column, EndLine: line, EndColumn: column + 1, Lines: []string{text}}
	}
	scan := func(spans ...finding.Span) ([]string, error) {
		var results []finding.Result
		for _, s := range spans {
			res, err := finding.New(finding.Input{Rule: rule, Severity: finding.High, Title: "t", Problem: "p", Fix: "f", Location: s})
			if err != nil {
				return nil, err
			}
			results = append(results, res)
		}
		fingerprintRun(results)
		var fps []string
		for _, r := range results {
			fps = append(fps, r.PartialFingerprints.CanyonV1)
		}
		return fps, nil
	}
	// The recipe restated from contracts/finding.md rather than shared with the gate, so this
	// catches the gate drifting from the contract. Each field is framed by its UTF-8 byte
	// length — len() of a Go string is already bytes, which is the unit the contract names.
	frame := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	recipe := func(column, n int) string {
		h := sha256.Sum256([]byte(frame(rule.ID) + frame(uri) + frame(text) + frame(strconv.Itoa(column)) + frame(strconv.Itoa(n))))
		return fmt.Sprintf("f_%x", h)
	}

	approved, err := scan(at(5, 3))
	if err != nil {
		t.Fatal(err)
	}
	if approved[0] != recipe(3, 0) {
		t.Fatalf("the fingerprint is not the recipe's:\n got %s\nwant %s", approved[0], recipe(3, 0))
	}
	// fb7: the approved finding is gone and another with the same rule, uri and snippet sits on
	// another column. By occurrence it was number 0 and inherited the approval.
	other, err := scan(at(9, 1))
	if err != nil {
		t.Fatal(err)
	}
	if other[0] == approved[0] {
		t.Fatal("a different finding took over the approved one's fingerprint")
	}
	// A sibling on another column, emitted first, does not move the approved finding's fingerprint.
	both, err := scan(at(1, 1), at(5, 3))
	if err != nil {
		t.Fatal(err)
	}
	if both[0] != recipe(1, 0) || both[1] != approved[0] {
		t.Fatalf("a sibling moved the fingerprint:\n got %v\nwant [%s %s]", both, recipe(1, 0), approved[0])
	}
	// Two identical lines at one column are told apart by their occurrence, in log order.
	twice, err := scan(at(5, 3), at(9, 3))
	if err != nil {
		t.Fatalf("two identical lines failed the scan: %v", err)
	}
	if twice[0] != approved[0] || twice[1] != recipe(3, 1) {
		t.Fatalf("identical lines are not numbered in log order:\n got %v\nwant [%s %s]", twice, approved[0], recipe(3, 1))
	}
}

// The tree lists every regular file by slash-separated relative path, in sorted order,
// refuses to read what it did not list, and refuses a tree with anything but regular files.
func TestTheTree(t *testing.T) {
	tr, err := Walk("testdata/toy/must-fail")
	if err != nil {
		t.Fatal(err)
	}
	got := tr.Files()
	if len(got) != 2 || got[0] != "src/app.ts" || got[1] != "src/app/b.ts" { // WalkDir visits src/app/ first
		t.Fatalf("files are not slash paths in sorted order: %v", got)
	}
	for _, rel := range []string{"src/missing.ts", "../../../scan.go"} {
		if _, err := tr.Read(rel); err == nil {
			t.Fatalf("read %s, which the walk did not list", rel)
		}
	}

	dir := t.TempDir()
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "link.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := Walk(dir); err == nil {
		t.Fatal("walked a tree with a symlink in it")
	}
}
