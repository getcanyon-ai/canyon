package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// carryApp is the app the carry tests review: server.js with two routes the model may flag, each a line.
var carryApp = map[string]string{"server.js": "const app = express();\napp.get(\"/a\", refetchA);\napp.get(\"/b\", refetchB);\n"}

// reviewOf is a review of the app files at a temp dir, its model answering candidates: the log the gate's reviewer
// leaves, and the dir.
func reviewOf(t *testing.T, files map[string]string, candidates ...Candidate) (finding.Log, string) {
	t.Helper()
	dir := t.TempDir()
	plant(t, dir, files)
	log, err := reviewer(t, &model{resp: Response{Results: candidates}}).Review(context.Background(), dir, finding.Log{Version: finding.Version, Schema: finding.SchemaURI})
	if err != nil {
		t.Fatal(err)
	}
	return log, dir
}

// refetch is a high perf.unbounded-refetch candidate at server.js's line.
func refetch(line int) Candidate {
	return Candidate{Rule: "perf.unbounded-refetch", File: "server.js", StartLine: line, EndLine: line, Severity: "high",
		Title: "The route refetches on every request", Problem: "Every request refetches.", Fix: "Cache it."}
}

// carried is each result of log's reviewer run with carried_from, and the run's notes.
func carried(log finding.Log) (out []finding.Result, notes []string) {
	for _, run := range log.Runs {
		if run.Tool.Driver.Name != "canyon-review" {
			continue
		}
		for _, r := range run.Results {
			if r.Properties.Canyon.CarriedFrom != "" {
				out = append(out, r)
			}
		}
		for _, inv := range run.Invocations {
			for _, n := range inv.ToolExecutionNotifications {
				notes = append(notes, n.Message.Text)
			}
		}
	}
	return out, notes
}

// The cases of contracts/finding.md, A blocking finding an earlier review made stays while its code stands, over the
// reviewer's own output: what is carried, where, and what is said.
func TestCarry(t *testing.T) {
	snap := floorSnap(t)
	before, _ := reviewOf(t, carryApp, refetch(2))
	earlier := []Verdict{{Run: "gr_first", Log: before}}
	found := before.Runs[0].Results[0]
	for _, c := range []struct {
		name    string
		files   map[string]string
		model   []Candidate
		snap    policy.Snapshot
		atLine  int // 0: not carried
		comment string
	}{
		{"left out, unchanged", carryApp, nil, snap, 2, "the model left it out, the code stands"},
		{"reported again", carryApp, []Candidate{refetch(2)}, snap, 0, "not left out: nothing to carry"},
		{"its line edited", map[string]string{"server.js": "const app = express();\napp.get(\"/a\", cachedA);\napp.get(\"/b\", refetchB);\n"}, nil, snap, 0, "its code changed"},
		{"moved down", map[string]string{"server.js": "// routes\n\n" + carryApp["server.js"]}, nil, snap, 4, "its code moved: carried where it is"},
		{"file gone", map[string]string{"app.js": carryApp["server.js"]}, nil, snap, 0, "the package has no such file"},
		{"rule advisory", carryApp, nil, mustStates(t, snap, map[string]policy.State{"perf.unbounded-refetch": policy.Advisory}), 0, "its rule no longer blocks"},
		{"not UTF-8", map[string]string{"server.js": carryApp["server.js"] + "// \xff\n"}, nil, snap, 2, "the file tells nothing: carried at its own lines"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log, dir := reviewOf(t, c.files, c.model...)
			Carry(&log, earlier, c.snap, dir)
			got, notes := carried(log)
			if c.atLine == 0 {
				if len(got) != 0 || slices.Contains(notes, CarriedOne) {
					t.Fatalf("%s: carried %+v, notes %v", c.comment, got, notes)
				}
				return
			}
			if len(got) != 1 || !slices.Contains(notes, CarriedOne) {
				t.Fatalf("%s: carried %+v, notes %v", c.comment, got, notes)
			}
			r, g := got[0], got[0].Locations[0].PhysicalLocation.Region
			want := found.Locations[0].PhysicalLocation.Region
			if r.Properties.Canyon.CarriedFrom != "gr_first" || r.PartialFingerprints != found.PartialFingerprints || g.StartLine != c.atLine || g.EndLine != c.atLine ||
				g.Snippet != want.Snippet || g.StartColumn != want.StartColumn || g.EndColumn != want.EndColumn || r.Properties.Canyon.Blocks != nil {
				t.Fatalf("%s: carried %+v, want %+v at line %d, blocks for the server to derive", c.comment, r, found, c.atLine)
			}
		})
	}
}

// A finding carried again keeps the run whose review reported it; a newer verdict's words stand at the highest
// severity any gave; two carried read in the plural.
func TestCarryAgainAndMany(t *testing.T) {
	snap := floorSnap(t)
	before, _ := reviewOf(t, carryApp, refetch(2), refetch(3))
	carriedOnce, dir := reviewOf(t, carryApp)
	Carry(&carriedOnce, []Verdict{{Run: "gr_first", Log: before}}, snap, dir)
	now, dir := reviewOf(t, carryApp)
	Carry(&now, []Verdict{{Run: "gr_second", Log: carriedOnce}, {Run: "gr_first", Log: before}}, snap, dir)
	got, notes := carried(now)
	if len(got) != 2 || got[0].Properties.Canyon.CarriedFrom != "gr_first" || got[1].Properties.Canyon.CarriedFrom != "gr_first" {
		t.Fatalf("carried again from the first review: %+v", got)
	}
	if want := fmt.Sprintf(CarriedMany, 2); !slices.Contains(notes, want) || slices.Contains(notes, CarriedOne) {
		t.Fatalf("two carried read %q: %v", want, notes)
	}
	critical := strings.Replace(mustJSONString(t, before), `"severity":"high"`, `"severity":"critical"`, 1)
	var older finding.Log
	mustUnmarshal(t, critical, &older)
	now, dir = reviewOf(t, carryApp)
	Carry(&now, []Verdict{{Run: "gr_second", Log: carriedOnce}, {Run: "gr_first", Log: older}}, snap, dir)
	if got, _ := carried(now); len(got) != 2 || got[0].Properties.Canyon.Severity != finding.Critical {
		t.Fatalf("the highest severity any verdict gave: %+v", got)
	}
}

// Fail closed: a package whose tree cannot be walked tells nothing, so a result is carried at its own lines.
func TestCarryOverATreeThatCannotBeRead(t *testing.T) {
	snap := floorSnap(t)
	before, _ := reviewOf(t, carryApp, refetch(2))
	now, _ := reviewOf(t, carryApp)
	Carry(&now, []Verdict{{Run: "gr_first", Log: before}}, snap, filepath.Join(t.TempDir(), "gone"))
	if got, _ := carried(now); len(got) != 1 || got[0].Locations[0].PhysicalLocation.Region.StartLine != 2 {
		t.Fatalf("a tree that cannot be read carries it at its own lines: %+v", got)
	}
}

func mustStates(t *testing.T, snap policy.Snapshot, states map[string]policy.State) policy.Snapshot {
	t.Helper()
	out, err := policy.WithStates(snap, states)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}
